// 本文件是 doc/32 A3 的发送侧包壳：模型调用前把「当前 run 的占位行」换成带图 part 的
// 输入副本。存储面（mh / runlog / 帧）仍只有占位——包壳只改发给模型的那一份。
//
// 当前拍（A3-5）：还原最后一条 user 行，以及它之后的 tool 回执；历史 user 占位改成只留元数据
// 的说明行，历史 tool 不还原。user 图与工具图都在 root 成功回复后才算发过（失败的枪下一枪可重试）。
// 同一 foldBox 里 user 图只还原第一枪。两把闸都真才还原：RunProfile.Vision（目录标记，缺省 false）
// 与 worker 是否挂上本包壳（TAKO_MEDIA_SEND）。Vision 为 false 时输入原样返回，不读盘。
// 原 input 切片与原消息对象零修改；没有可还原内容时原样返回 input（不是等价副本）。
// media send 行尾的 img= 是本枪实际还原的图按 v41 合计的 token。纯文本不打这行。
package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// mediaLinePattern 解析两种占位（入口图 / 工具回执）：seq、name 都可缺省；path 允许含空格
// （非贪婪吃到 " mime="）；sha 允许存量截断值。
var mediaLinePattern = regexp.MustCompile(
	`^\[media (?:seq=(\d+) )?(?:name="([^"]*)" )?path=(.+?) mime=([^ ]+) size=(\d+)B sha=([0-9a-fA-F]+)；[^\]]*\]$`,
)

// restorableImageMIME 是本拍允许送进视觉管道的栅格格式。入口白名单里的 BMP/SVG/PDF/音视频
// 仍保留占位——它们不是所有视觉提供商共同支持的格式，送错格式会让整枪调用报错。
var restorableImageMIME = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

const (
	// 活性验证（2026-10-20）：只说「已附上」时，模型仍会照占位句去调 bash/read_media_file，
	// 而回执写着「媒体字节不会写入对话记录」——模型随即否认自己刚看到的像素。
	// 所以这里要写明像素就在本条消息里，并点名禁止重复读文件。
	mediaAttachedUser = "图片像素已随本条消息直接发送，直接看即可，不要调用 read_media_file 或 bash 读取该文件"
	mediaAttachedTool = "图片像素已随本条工具结果直接发送，直接看即可，不要调用 read_media_file 或 bash 读取该文件"
	// 历史 run 的占位不能原样留着：原句「要看就调 read_media_file」会把「刚才那张图什么颜色」
	// 引去核对，而旧回执说不回传字节，模型就会否认自己上一轮已经答过的内容。
	// 图片（以及音视频）点名 read_media_file：重看是模型自己调工具，引擎不主动重发。
	mediaHistorical = "该附件只在当时那条消息里发送过，本轮不再附带像素；用户问当时的分析时，直接引用随后 assistant 的回复，不要为核对再读文件；用户明确要求重看时再调 read_media_file"
	// PDF 没有像素可附。历史行改口，避免对 PDF 说「不再附带像素」，并点名 read_pdf_text。
	mediaHistoricalPDF = "该 PDF 只在当时那条消息里出现；需要重读时调 read_pdf_text，不要为核对重复抽取"
)

// mediaRef 是一行占位解析出的字段。receipt 区分工具回执（无 seq/name、句尾「已按路径读取」）。
type mediaRef struct {
	seq     int
	name    string
	path    string
	mime    string
	size    int
	sha     string
	receipt bool
}

func parseMediaLine(line string) (mediaRef, bool) {
	line = strings.TrimSuffix(line, "\r")
	m := mediaLinePattern.FindStringSubmatch(line)
	if m == nil {
		return mediaRef{}, false
	}
	seq := 0
	if m[1] != "" {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return mediaRef{}, false
		}
		seq = n
	}
	size, err := strconv.Atoi(m[5])
	if err != nil {
		return mediaRef{}, false
	}
	return mediaRef{
		seq:     seq,
		name:    m[2],
		path:    m[3],
		mime:    m[4],
		size:    size,
		sha:     strings.ToLower(m[6]),
		receipt: strings.Contains(line, "已按路径读取"),
	}, true
}

// mediaSkip 是一次降级读盘的原因（只进日志，不改消息）。
type mediaSkip struct {
	path   string
	reason string
}

// mediaRestoreStats 是「本枪还原集」的观测：seen > 0 才打 media send 行。
// tokens 只加实际还原成功的图，跳过的占位不加。
type mediaRestoreStats struct {
	seen   int
	user   int
	tool   int
	tokens int
	skips  []mediaSkip
}

// mediaRestoreHandler 是挂在中介链里的模型包壳。store/workspace 是 worker 装配件，
// 与 run 无关；每 run 的发送标志（A3-3 起）放 foldBox，不放 handler 字段（handler 被多 run 复用）。
type mediaRestoreHandler struct {
	*adk.BaseChatModelAgentMiddleware
	store     *Store
	workspace string
}

func newMediaRestoreHandler(store *Store, workspace string) *mediaRestoreHandler {
	return &mediaRestoreHandler{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		store:                        store,
		workspace:                    workspace,
	}
}

func (h *mediaRestoreHandler) WrapModel(_ context.Context, m model.BaseChatModel, _ *adk.ModelContext) (model.BaseChatModel, error) {
	return &mediaSendModel{inner: m, store: h.store, workspace: h.workspace}, nil
}

// mediaSendModel 只在调用前改输入副本；Generate/Stream 的错误原样上抛，交给外层 guard 处理。
type mediaSendModel struct {
	inner     model.BaseChatModel
	store     *Store
	workspace string
}

func (m *mediaSendModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := m.inner.Generate(ctx, m.restore(ctx, input), opts...)
	if err != nil {
		return nil, err
	}
	// Generate 一次调用就拿到完整回复，成功即可确认本枪已发出。
	// Stream 要等 worker 把流排干才知道成功，提交点在 drain 分支，不在包装层。
	if box := foldBoxFrom(ctx); box != nil {
		box.commitMediaSend()
	}
	return msg, nil
}

func (m *mediaSendModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, m.restore(ctx, input), opts...)
}

func (m *mediaSendModel) restore(ctx context.Context, input []*schema.Message) []*schema.Message {
	if !profileVision(ctx) {
		if inputHasMediaPlaceholder(input) {
			logModelNoVision(ctx)
		}
		return input
	}
	resolve := func(ctx context.Context, path string) (string, error) {
		return resolveReadMediaPath(ctx, m.store, m.workspace, path)
	}
	box := foldBoxFrom(ctx)
	out, stats := restoreUserMedia(ctx, input, resolve, box)
	out, toolStats := restoreToolMedia(ctx, out, resolve, box)
	stats.seen += toolStats.seen
	stats.tool = toolStats.tool
	stats.tokens += toolStats.tokens
	stats.skips = append(stats.skips, toolStats.skips...)
	logMediaRestore(ctx, stats)
	return rewriteHistoricalMedia(out)
}

// restoreToolMedia 扫描最后一条 user 之后的 tool 回执，把可还原的回执换成 image part。
// 同一个 ToolCallID 只在成功后记一次（foldBox）；失败的枪不 commit，下一枪还能重试。
// 没有 foldBox（旁路调用/单测）时不做去重，仍然还原。
//
// 双闸（doc/32 A8-1）：先从最后一条 user 之后的 assistant 消息收集 ToolCallID→工具名。
// 只有映射到 read_media_file 的 tool 消息才进入还原；映射缺失（历史被裁、或这行根本不是
// 这次工具调用）按不还原处理，连盘都不读。行本身还要是「已按路径读取」回执，见 restoreMediaText。
// 只查回执句不够：PDF 正文可以把这句话原样印出来，放进别的工具结果里。
func restoreToolMedia(
	ctx context.Context,
	input []*schema.Message,
	resolve func(context.Context, string) (string, error),
	box *foldBox,
) ([]*schema.Message, mediaRestoreStats) {
	var stats mediaRestoreStats
	last := lastUserIndex(input)
	if last < 0 {
		return input, stats
	}
	names := toolNamesAfterLastUser(input, last)
	var out []*schema.Message
	for i := last + 1; i < len(input); i++ {
		msg := input[i]
		if msg == nil || msg.Role != schema.Tool || msg.Content == "" || len(msg.UserInputMultiContent) > 0 {
			continue
		}
		if names[msg.ToolCallID] != "read_media_file" {
			continue
		}
		if box != nil && box.toolMediaSent(msg.ToolCallID) {
			continue
		}
		text, images, st := restoreMediaText(ctx, msg.Content, resolve, true)
		stats.seen += st.seen
		stats.tokens += st.tokens
		stats.skips = append(stats.skips, st.skips...)
		if st.restored == 0 {
			continue
		}
		if out == nil {
			out = make([]*schema.Message, len(input))
			copy(out, input)
		}
		out[i] = mediaMessageWithParts(msg, text, images)
		stats.tool += st.restored
		if box != nil {
			box.noteToolMediaPending(msg.ToolCallID)
		}
	}
	if out == nil {
		return input, stats
	}
	return out, stats
}

// rewriteHistoricalMedia 把最后一条 user 之前的历史占位换成只留元数据的说明行。历史图本轮
// 不读盘、不发送，但路径/名字继续留在上下文里；模型问当时的分析时依据紧随其后的 assistant
// 回复，不再被旧占位句引去重读。没有历史占位时原样返回原切片。
func rewriteHistoricalMedia(input []*schema.Message) []*schema.Message {
	last := lastUserIndex(input)
	if last <= 0 {
		return input
	}
	var out []*schema.Message
	for i := 0; i < last; i++ {
		msg := input[i]
		if msg == nil || msg.Role != schema.User || msg.Content == "" {
			continue
		}
		text, changed := rewriteHistoricalText(msg.Content)
		if changed == 0 {
			continue
		}
		if out == nil {
			out = make([]*schema.Message, len(input))
			copy(out, input)
		}
		clone := *msg
		clone.Content = text
		out[i] = &clone
	}
	if out == nil {
		return input
	}
	return out
}

// rewriteHistoricalText 只改第一个召回标签之前的正文；召回块里的伪造占位不动。
func rewriteHistoricalText(text string) (string, int) {
	cut := recallCut(text)
	body, tail := text[:cut], text[cut:]
	lines := strings.Split(body, "\n")
	changed := 0
	for i, line := range lines {
		ref, ok := parseMediaLine(line)
		if !ok {
			continue
		}
		lines[i] = mediaHistoricalLine(ref)
		changed++
	}
	if changed == 0 {
		return text, 0
	}
	return strings.Join(lines, "\n") + tail, changed
}

// restoreUserMedia 只处理输入里最后一条 role=user 的消息：解析第一个召回标签之前的正文，
// 把可还原的占位行替换成 attached marker，并造 Content="" + UserInputMultiContent 的副本。
// box 非空且本 run 已经成功发过 user 图时不读盘，占位原样返回。resolve 由调用方注入
// （生产走 resolveReadMediaPath，单测走临时目录闭包），读盘校验失败只跳过。
// 没有 box（旁路调用/单测）时不去重，每次都还原。
func restoreUserMedia(
	ctx context.Context,
	input []*schema.Message,
	resolve func(context.Context, string) (string, error),
	box *foldBox,
) ([]*schema.Message, mediaRestoreStats) {
	var stats mediaRestoreStats
	if box != nil && box.userMediaSent() {
		return input, stats
	}
	idx := lastUserIndex(input)
	if idx < 0 {
		return input, stats
	}
	msg := input[idx]
	if msg == nil || msg.Content == "" || len(msg.UserInputMultiContent) > 0 {
		return input, stats
	}
	text, images, st := restoreMediaText(ctx, msg.Content, resolve, false)
	stats.seen, stats.user, stats.skips = st.seen, st.restored, st.skips
	stats.tokens = st.tokens
	if st.restored == 0 {
		return input, stats
	}
	if box != nil {
		box.noteUserMediaPending()
	}
	out := make([]*schema.Message, len(input))
	copy(out, input)
	out[idx] = mediaMessageWithParts(msg, text, images)
	return out, stats
}

// mediaTextStats 是单条文本的还原结果。tokens 是本条实际还原的图的 v41 合计。
type mediaTextStats struct {
	seen     int
	restored int
	tokens   int
	skips    []mediaSkip
}

// restoreMediaText 在第一个召回标签之前按行解析占位；召回块里的伪造占位不读盘、不还原。
// 没有任何成功还原时返回原文（调用方据此保持原切片）。
func restoreMediaText(
	ctx context.Context,
	text string,
	resolve func(context.Context, string) (string, error),
	tool bool,
) (string, []schema.MessageInputPart, mediaTextStats) {
	var stats mediaTextStats
	cut := recallCut(text)
	body, tail := text[:cut], text[cut:]
	lines := strings.Split(body, "\n")
	images := make([]schema.MessageInputPart, 0)
	for i, line := range lines {
		ref, ok := parseMediaLine(line)
		if !ok {
			continue
		}
		// 工具路径只认自家回执。入口占位句尾没有「已按路径读取」，不在这里还原；
		// 那种行留在 user 消息里，由 restoreUserMedia 处理。非回执行不计入 seen，避免空打 media send。
		if tool && !ref.receipt {
			continue
		}
		stats.seen++
		raw, reason := loadMediaBytes(ctx, resolve, ref)
		if reason != "" {
			stats.skips = append(stats.skips, mediaSkip{path: ref.path, reason: reason})
			continue
		}
		if n, err := imageTokensFromBytes(raw); err == nil {
			stats.tokens += n
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		images = append(images, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{
					Base64Data: &b64,
					MIMEType:   ref.mime,
				},
			},
		})
		lines[i] = mediaAttachedLine(ref, tool)
		stats.restored++
	}
	if stats.restored == 0 {
		return text, nil, stats
	}
	return strings.Join(lines, "\n") + tail, images, stats
}

// recallCut 找第一个召回标签的位置：媒体解析只处理它之前的用户原文，避免记忆内容里恰好
// 出现占位文本时被当成附件去读盘（doc/29 §6.4 的注入格式）。
func recallCut(text string) int {
	cut := len(text)
	for _, tag := range []string{"<memory-context>", "<dialogue-context>"} {
		if i := strings.Index(text, tag); i >= 0 && i < cut {
			cut = i
		}
	}
	return cut
}

func lastUserIndex(msgs []*schema.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i] != nil && msgs[i].Role == schema.User {
			return i
		}
	}
	return -1
}

// mediaMessageWithParts 造发送副本：Content 必须清空，否则 openai 适配器会因
// Content 与 MultiContent 同时非空报 ErrContentFieldsMisused。
func mediaMessageWithParts(msg *schema.Message, text string, images []schema.MessageInputPart) *schema.Message {
	clone := *msg
	clone.Content = ""
	parts := make([]schema.MessageInputPart, 0, len(images)+1)
	parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	parts = append(parts, images...)
	clone.UserInputMultiContent = parts
	return &clone
}

// loadMediaBytes 走与 read_media_file 相同的路径闸，并校验普通文件、5MB 上限、大小与 sha 前缀。
// 返回的 reason 只用于日志；空串 = 可以还原。
func loadMediaBytes(
	ctx context.Context,
	resolve func(context.Context, string) (string, error),
	ref mediaRef,
) ([]byte, string) {
	if !restorableImageMIME[ref.mime] {
		return nil, "mime_not_restorable"
	}
	full, err := resolve(ctx, ref.path)
	if err != nil {
		return nil, "path_unreadable"
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "file_missing"
		}
		return nil, "stat_failed"
	}
	if !fi.Mode().IsRegular() {
		return nil, "not_regular"
	}
	if fi.Size() > mediaMaxBytes {
		return nil, "too_large"
	}
	if fi.Size() != int64(ref.size) {
		return nil, "size_mismatch"
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return nil, "read_failed"
	}
	if len(raw) != ref.size {
		return nil, "size_mismatch"
	}
	sum := sha256.Sum256(raw)
	if !strings.HasPrefix(hex.EncodeToString(sum[:]), ref.sha) {
		return nil, "sha_mismatch"
	}
	return raw, ""
}

// mediaAttachedLine 是只存在于发送份的替换行：句尾告诉模型图已经附上，别再为同一张图调工具。
func mediaAttachedLine(ref mediaRef, tool bool) string {
	suffix := mediaAttachedUser
	if tool {
		suffix = mediaAttachedTool
	}
	return mediaLine(ref, suffix)
}

func mediaHistoricalLine(ref mediaRef) string {
	suffix := mediaHistorical
	if ref.mime == "application/pdf" {
		suffix = mediaHistoricalPDF
	}
	return mediaLine(ref, suffix)
}

// toolNamesAfterLastUser 收集最后一条 user 之后 assistant 消息里的 ToolCallID→工具名。
// 更早的 tool_call 不在这张表里：那些调用的结果属于历史，本枪不还原。
func toolNamesAfterLastUser(input []*schema.Message, last int) map[string]string {
	names := make(map[string]string)
	for i := last + 1; i < len(input); i++ {
		msg := input[i]
		if msg == nil || msg.Role != schema.Assistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			if call.ID == "" {
				continue
			}
			if _, exists := names[call.ID]; exists {
				continue
			}
			names[call.ID] = call.Function.Name
		}
	}
	return names
}

func mediaLine(ref mediaRef, suffix string) string {
	var b strings.Builder
	b.WriteString("[media ")
	if ref.seq > 0 {
		fmt.Fprintf(&b, "seq=%d ", ref.seq)
	}
	if ref.name != "" {
		fmt.Fprintf(&b, "name=%q ", ref.name)
	}
	fmt.Fprintf(&b, "path=%s mime=%s size=%dB sha=%s；", ref.path, ref.mime, ref.size, ref.sha)
	b.WriteString(suffix)
	b.WriteString("]")
	return b.String()
}

// profileVision 读这场 run 装配进 ctx 的模型能力。
// 没有 profile（启动自检、单测旁路）时用装配默认值 false：没标 vision 就不发图。
func profileVision(ctx context.Context) bool {
	p := profileFrom(ctx)
	if p == nil {
		return false
	}
	return p.Vision
}

func inputHasMediaPlaceholder(input []*schema.Message) bool {
	for _, msg := range input {
		if msg == nil || msg.Content == "" {
			continue
		}
		for _, line := range strings.Split(msg.Content, "\n") {
			if _, ok := parseMediaLine(line); ok {
				return true
			}
		}
	}
	return false
}

func logModelNoVision(ctx context.Context) {
	session, run := SessionID("?"), RunID("?")
	if box := foldBoxFrom(ctx); box != nil {
		session, run = box.session, box.run
	}
	log.Printf("media skip %s/%s: reason=model_no_vision", session, run)
}

func logMediaRestore(ctx context.Context, stats mediaRestoreStats) {
	if stats.seen == 0 {
		return
	}
	session, run := SessionID("?"), RunID("?")
	if box := foldBoxFrom(ctx); box != nil {
		session, run = box.session, box.run
	}
	for _, skip := range stats.skips {
		log.Printf("media skip %s/%s: path=%s reason=%s", session, run, skip.path, skip.reason)
	}
	log.Printf("media send %s/%s: user=%d tool=%d skipped=%d img=%dtok", session, run, stats.user, stats.tool, len(stats.skips), stats.tokens)
}

// annotateMediaTokens 先把每行的图片 token 清零，再只给这一枪还会发出去的图标价。
// 没有 profile、Vision 为 false、厂商不是 deepseek，或者没有路径解析器：全部保持 0。
// 最后一条 user 只在本 run 还没成功发过 user 图时计价。它之后的 tool 行，ToolCallID 已提交的不算。
// 没有 user 行的切片（usage 锚点之后往往只剩 tool 回执）里，每条未提交的 tool 都算在锚点之后。
func annotateMediaTokens(ctx context.Context, rows []ModelMessage, box *foldBox, resolve func(context.Context, string) (string, error)) {
	for i := range rows {
		rows[i].MediaTokens = 0
	}
	p := profileFrom(ctx)
	if p == nil || !p.Vision || p.Provider != "deepseek" || resolve == nil {
		return
	}
	last := lastModelUser(rows)
	start := 0
	if last >= 0 {
		if box == nil || !box.userMediaSent() {
			rows[last].MediaTokens = priceMediaText(ctx, rows[last].Text, box, resolve)
		}
		start = last + 1
	}
	for i := start; i < len(rows); i++ {
		if rows[i].Role != "tool" {
			continue
		}
		if box != nil && box.toolMediaSent(rows[i].ToolCallID) {
			continue
		}
		rows[i].MediaTokens = priceMediaText(ctx, rows[i].Text, box, resolve)
	}
}

func lastModelUser(rows []ModelMessage) int {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == "user" {
			return i
		}
	}
	return -1
}

func priceMediaText(ctx context.Context, text string, box *foldBox, resolve func(context.Context, string) (string, error)) int {
	cut := recallCut(text)
	total := 0
	for _, line := range strings.Split(text[:cut], "\n") {
		ref, ok := parseMediaLine(line)
		if !ok {
			continue
		}
		total += priceMediaRef(ctx, box, resolve, ref)
	}
	return total
}

func priceMediaRef(ctx context.Context, box *foldBox, resolve func(context.Context, string) (string, error), ref mediaRef) int {
	if box != nil {
		if n, ok := box.cachedMediaTokens(ref.path, ref.sha); ok {
			return n
		}
	}
	n, err := mediaRefTokens(ctx, resolve, ref)
	if err != nil || n <= 0 {
		return 0
	}
	if box != nil {
		box.rememberMediaTokens(ref.path, ref.sha, n)
	}
	return n
}

var _ adk.ChatModelAgentMiddleware = (*mediaRestoreHandler)(nil)
var _ model.BaseChatModel = (*mediaSendModel)(nil)
