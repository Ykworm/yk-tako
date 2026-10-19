// 本文件：mid-run fold 的钩子侧（19v6-10 §4 转换层 + §7/§8 折叠链接线）。
//
// 病灶（19v6-10 §1）：run 起手前的折叠看不到起手后的增长——F 系列文件工具落地后，一个
// 「读大文件再回答」的 run 在 ReAct 中途把上下文读爆：起手时自测通过，中途 read_file 灌进
// 几十万 token，某次模型调用被 provider 以「输入太长」拒收 → run 走 error → 用户重投重演。
// 回答是把折叠链从「run 起手前一次」挪到「每次模型调用前」：ADK 的
// ChatModelAgentMiddleware.BeforeModelRewriteState 每轮模型调用前到场，在这里对当前 state
// 自测长度，超线就调 C 系列已验收的同一条折叠链（foldInput），把最老的消息折成摘要后再放行。
//
// 一次 run 的生命周期（本文件的核心不变式）：state = [system] + base + ADK 追加上来的新轮次。
//   - base（mh 形状：RunID/Seq/Role/Text）由 foldBox 持有：run 起手 prepareInput 填初值
//     （读库全序、Run fold 后），此后每次 mid-run 折叠把它换代为折算行；
//   - ADK 只往 state.Messages 追加（模型回复、工具结果），不修前缀——按索引 1:1 锁定即可把
//     state 切回 mh 形状（convertState）；新行坐标 RunID=currentRun、Seq 接在 base 里本 run
//     行的最大值之后（user=1、其后 2、3…）；
//   - 重写 state.Messages = [system 原样] + [新摘要(user)] + [保留区]（保留区恒含 user 行）。
//     ToolInfos 不为所动（它是模型声明面，归别的 handler/装配面）。
//
// 没有新折叠算法：foldInput / countInput / renderFold 算法本体不动（planning 裁定 2；B4 起
// foldInput 加 window 形参走 trim 预算，见 compact.go），
// 本文件对折叠链只做三件事——state ⇄ mh 形状的转换、容器读写、折叠日志（格式钉死在 planning 裁定 6）；
// doc/24 之后多了第四件：发出前滤思考 + 有账单时的长度估算（下面两段，同样不动折叠算法本体）。
//
// 落库路径不变（PG 一个字不动）：本文件只把折后 state 存进容器；唯一写点仍是 EndRun——
// worker 的 defer 从容器读 staged 列表与折后行（mhRowsFromFold）、complete 才落。
// AfterAgent 仅成功终态触发（error / 取消不触发）——零写入家族严丝合缝。
//
// 断言违例（system 非首 / 三件套）返回错误、不 panic（planning 裁定 7）：ADK 把 handler 错误
// 透传成 run error（eino v0.9.21 实读：graph 节点错误 → generator 的 Err 事件），run 走干净 A。
//
// doc/24 两件事（滤思考 + Usage 基线触发，位点都在 BeforeModelRewriteState）：
//   - 滤思考：模型上一枪的 ReasoningContent 会被组件原样重发进下一次请求，而 countInput 数不到它
//     （mh 形状没这个字段）→ 自测偏小被放行、厂商按真实长度拒收。每次放行前清空 state 里
//     assistant 行的 ReasoningContent，请求与自测在这一维对齐。
//   - Usage 基线触发：上一枪的回执（ResponseMeta.Usage）粘在产生它的 assistant 消息上，有它时
//     「即将发出的这一份」= PromptTokens + 可见输出 + 锚点之后新行的自测，省掉整份历史的重数。
//     账单只在原始 schema.Message 上读得到（convertState / toSchemaMessages 都丢它），
//     所以锚点必须在转换前找——折完那一次自然退回全量自测，不需要额外的清锚点开关。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// foldBox 是 run 级折叠容器（planning 裁定 4；ctx 传递比照 agentModelTable 先例）：
// worker.Run 建、塞 ctx，midfoldHandler 每次模型调用前读写，worker 的 defer 读回落库。
// 钩子跑在 ADK 内部 goroutine，RWMutex 兜底——正常时序下无竞态（AfterAgent 完成 → run 函数
// 返回 → generator.Close() → worker 的 it.Next() 返 false，channel close 给了 happens-before）。
type foldBox struct {
	mu        sync.RWMutex
	session   SessionID
	run       RunID
	base      []ModelMessage     // 「system 与当前 run 新轮次之外」的基点（mh 形状，含 RunID/Seq）
	baseCount int                // run 起手放行估算（A1：prepareInput 的 countInput 结果——detect 行消费；A2/A3 的 D4 续用）
	staged    []StagedCompaction // 折叠指令列表：Run fold 打头、mid-run 各次追加（EndRun 吃列表）
	final     []ModelMessage     // 终态 mh 投影：仅 AfterAgent（成功终态）填——error / 取消不填
	frames    int                // 已发帧计数（B1 从 worker 局部收编，D12）：A2 零帧判据 + guard 让其位的同源读数
	// recoveredOnce 是「本 run 已救过一次」的一次性闸（B2 从 worker 局部变量收编，D12）：
	// A2 零帧谓词读它（已救过不再救）、A3 强折闸花它、B2 guard 救援先花它。收编理由同 frames：
	// guard 跑在模型调用点，worker 的局部变量在它视野外——两侧必须看同一个闸，否则
	// 「guard 已救 = A3 不该再触发检②」无从判。读写只走 recovered() / tryMarkRecovered()
	// （字段名避与同名方法冲突）。
	recoveredOnce bool
	// adoptedRows / adoptedLen 是 guard 救援的交接槽（B3，D15）：guard 在模型调用点折成后写入
	// （折后行 + 被拒输入的条数），采用点在钩子里取走。与 recoveredOnce 同族：进程内交接物，
	// 只走 setAdopted / takeAdopted 读写（takeAdopted 取走即清空——一次救援只有一个采用点消费）。
	adoptedRows []ModelMessage
	adoptedLen  int
}

func newFoldBox(session SessionID, run RunID) *foldBox {
	return &foldBox{session: session, run: run}
}

func (b *foldBox) setBase(rows []ModelMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.base = rows
}

func (b *foldBox) baseRows() []ModelMessage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.base
}

func (b *foldBox) setBaseCount(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.baseCount = n
}

func (b *foldBox) countBase() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.baseCount
}

func (b *foldBox) appendStaged(sc StagedCompaction) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staged = append(b.staged, sc)
}

func (b *foldBox) stagedList() []StagedCompaction {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.staged
}

func (b *foldBox) setFinal(rows []ModelMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.final = rows
}

func (b *foldBox) finalRows() []ModelMessage {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.final
}

// addFrames / frameCount 是帧计数的唯一读写口（B1，D12）：worker 发帧后 +1、A2 谓词与
// overflow guard 同源读。收编理由 = guard 跑在模型调用点（middleware 层），worker 的局部
// 变量在它视野外，而「零帧让位」判据必须两侧看同一个数。
func (b *foldBox) addFrames(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames += n
}

func (b *foldBox) frameCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.frames
}

// recovered / tryMarkRecovered 是一次性闸的唯一读写口（B2，D12）：tryMarkRecovered 才是
// 「花掉」——首次调用花掉并返 true，之后恒 false。三个消费方共用同一实现：A2 谓词问
// 「已救过吗」（recovered）、A3 与 B2 guard 各自「我来救」时先花（tryMarkRecovered）。
// 先花后救的顺序是语义：闸在折叠/重调之前就花掉，救援链自身再失败也不回滚（B2 折败 =
// 原始错误上抛；同一 run 不二次救，doc §8）。
func (b *foldBox) recovered() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.recoveredOnce
}

func (b *foldBox) tryMarkRecovered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.recoveredOnce {
		return false
	}
	b.recoveredOnce = true
	return true
}

// setAdopted / takeAdopted 是 guard 救援交接槽的读写口（B3，D15）。inputLen = 被拒输入的条数
// （guard 侧 len(input)），采用点用它把「救援之后 ADK 追加的新行」切出来接在槽后面（尾随行）：
// 字面方案（只写 system+槽）会把这批新行丢掉——场景 a 丢「重试回复 + 它触发的工具结果」，
// 下一轮模型看不到自己已调过工具 → 重发同一批调用（帧双份、工具做两遍），与 B2 已验收的
// 「工具不重跑、帧不重发」当场冲突；场景 b 丢最终答复行本身（mh 少一行）。尾随行接回后两条
// 不变量都保住；重试回复的账单（ResponseMeta.Usage，其 PromptTokens 恰对应槽）也留在 state
// 上，下一轮 usageBasedInput 锚点不脱钩（doc/24 §5）。takeAdopted 读走即清空。
func (b *foldBox) setAdopted(rows []ModelMessage, inputLen int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adoptedRows = rows
	b.adoptedLen = inputLen
}

func (b *foldBox) takeAdopted() ([]ModelMessage, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows, n := b.adoptedRows, b.adoptedLen
	b.adoptedRows, b.adoptedLen = nil, 0
	return rows, n
}

// foldBoxKey 是 ctx 携带折叠容器的私有 key 类型（防撞车惯例同 agentModelTableKey）。
type foldBoxKey struct{}

func withFoldBox(ctx context.Context, b *foldBox) context.Context {
	return context.WithValue(ctx, foldBoxKey{}, b)
}

// foldBoxFrom 取回容器；缺失返回 nil（旁路调用没有容器，调用方各自显式处理）。
func foldBoxFrom(ctx context.Context) *foldBox {
	b, _ := ctx.Value(foldBoxKey{}).(*foldBox)
	return b
}

// convertState 把 ADK state 的消息按索引锁定切回 mh 形状——state = [system] + base + 新轮次。
// system 恒在队首（react Init 节点先拼 SystemMessage(instruction)、worker 的输入随后整段进队），
// 违例 = ADK 输入形状变了，返回错误（裁定 7，不 panic）。base 之外的多余行 = 本 run 新追加的
// 轮次：RunID=currentRun，Seq 接 base 里本 run 行的最大值连续排；重写后 base 自更新为折算行，
// 该坐标在重写前后稳定（折叠不动 user 行之后的相对位置，折叠只换前缀）。
func convertState(base []ModelMessage, currentRun RunID, msgs []*schema.Message) (system string, history []ModelMessage, err error) {
	if len(msgs) == 0 || msgs[0].Role != schema.System {
		return "", nil, errors.New("midfold: state 首条不是 system（ADK 输入形状违例）")
	}
	system = msgs[0].Content
	rest := msgs[1:]
	if len(rest) < len(base) {
		return "", nil, fmt.Errorf("midfold: state 行数 %d 少于基点 %d（重写链违例）", len(rest), len(base))
	}
	history = make([]ModelMessage, 0, len(rest))
	history = append(history, base...)
	next := nextRunSeq(base, currentRun)
	for _, m := range rest[len(base):] {
		history = append(history, newRowFromMessage(currentRun, next, m))
		next++
	}
	return system, history, nil
}

// nextRunSeq 是本 run 新行起算的 seq：base 里本 run 行（user=1、其后单位行 2、3…；历史上折叠
// 留下的空洞不填）的最大值 +1。base 恒含本 run 的 user 行（prepareInput 读的 mh 表里本轮这条
// 在 AcquireRun 已入）——找不到时兜底 1。
func nextRunSeq(base []ModelMessage, currentRun RunID) int {
	next := 1
	for _, m := range base {
		if m.RunID == currentRun && m.Seq > next {
			next = m.Seq
		}
	}
	return next + 1
}

// newRowFromMessage 把 ADK 追加的新消息转成 mh 行（坐标 = 本 run 第 seq 行）。
func newRowFromMessage(run RunID, seq int, m *schema.Message) ModelMessage {
	row := ModelMessage{RunID: run, Seq: seq, Text: m.Content}
	switch m.Role {
	case schema.Assistant:
		row.Role = "assistant"
		row.ToolCalls = m.ToolCalls
	case schema.Tool:
		row.Role = "tool"
		row.ToolCallID = m.ToolCallID
	default:
		row.Role = "user"
	}
	return row
}

// toSchemaMessages 把 mh 形状的行翻回 schema.Message（模型输入形）——prepareInput 的发送转换与
// 钩子的 state 重写共用同一条映射（转换层规则只此一处）。
func toSchemaMessages(history []ModelMessage) []*schema.Message {
	msgs := make([]*schema.Message, 0, len(history))
	for _, h := range history {
		switch h.Role {
		case "assistant":
			// ToolCalls 回放：带 tool_calls 的 assistant 行原样还给模型（OpenAI 协议要求
			// 其后紧跟对应的 tool 行）；老行无 tool_calls，nil 即原样。
			msgs = append(msgs, schema.AssistantMessage(h.Text, h.ToolCalls))
		case "tool":
			msgs = append(msgs, schema.ToolMessage(h.Text, h.ToolCallID))
		default:
			msgs = append(msgs, schema.UserMessage(h.Text))
		}
	}
	return msgs
}

// mhRowsFromFold 把折后 state 的 mh 投影切成落库行（A4，planning 裁定 3）：取
// RunID==currentRun 且 Seq≥2 的行（seq0 = mid-run 摘要行，由 staged 的 UPSERT 写进它的槽；
// seq1 = user 行，AcquireRun 已入），按出现位置从 2 连续重编——折叠折走单位后留下的 seq 空洞
// 在此闭合（§8「seq 从 2 连续排」）。子代理消息不在 root state 里，天然不出现（root 单实例语义不变）。
func mhRowsFromFold(runID RunID, final []ModelMessage) []ModelMessage {
	rows := make([]ModelMessage, 0, len(final))
	seq := 2
	for _, m := range final {
		if m.RunID != runID || m.Seq < 2 {
			continue
		}
		row := m
		row.Seq = seq
		rows = append(rows, row)
		seq++
	}
	return rows
}

// droppedRunUnits 数本次折叠折走的当前 run 单位数：单位恒以一条 assistant 行开头
// （currentRunUnits 的构造），故 = before/after 两侧本 run assistant 行数之差；折叠产物里
// 的摘要行是 role=user，不混入。
func droppedRunUnits(before, after []ModelMessage, currentRun RunID) int {
	n := 0
	for _, m := range before {
		if m.RunID == currentRun && m.Role == "assistant" {
			n++
		}
	}
	for _, m := range after {
		if m.RunID == currentRun && m.Role == "assistant" {
			n--
		}
	}
	return n
}

// 编译期锚：接口一缺，编译不过（其余七方法由 Base 兜底）。
var _ adk.ChatModelAgentMiddleware = (*midfoldHandler)(nil)

// stripReasoning 清空 state 里所有 assistant 行的 ReasoningContent（doc/24 §3 主裁决）。
// 原地改：state 装的是指针，改完 ADK 拼请求时读到的就是空字段——上一枪的思考不再重发。
// 必须在用量估算之后调用：估算的减项读的就是这个字段，滤完再从空字段估只能得 0。
// 折过的那一路不需要它（toSchemaMessages 重建的行本来就不带思考）；没折的直通路径必须它。
func stripReasoning(msgs []*schema.Message) {
	for _, m := range msgs {
		if m.Role == schema.Assistant && m.ReasoningContent != "" {
			m.ReasoningContent = ""
		}
	}
}

// usageEstimate 是「账单基线 + delta」四个读数（活体日志与单测都看分项，不看合计一个数）。
type usageEstimate struct {
	baseline int // 锚点那一次请求的输入侧账单（system、历史、schema 都在里面）
	visible  int // 那一次的输出账单减掉思考；<0 置 0
	delta    int // 锚点之后的新行（尚未发出去的 tool 结果等）自测长度
	total    int // 三者之和，即「即将发出的这一份」的估算长度
}

// usageBasedInput 在原始 state.Messages 上按 doc/24 §5 的公式估算下一次请求的输入长度。
// 锚点 = 从队尾往前最后一条 role 为 assistant 且 Usage 三个整数不全为 0 的消息；找不到（本场
// 第一枪、刚折完、厂商没给账单）返回 false，调用方退回全量 countInput（doc/24 §6）。
// 锚点自己的正文 / tool_calls 由 visible 代表，不进 delta——算两遍会偏大、早折。
// system 与工具 schema 声明都不另加：前者已在账单里，后者有意不进触发（doc/24 §7）。
func usageBasedInput(counter TokenCounter, currentRun RunID, msgs []*schema.Message) (usageEstimate, bool) {
	idx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != schema.Assistant || m.ResponseMeta == nil || m.ResponseMeta.Usage == nil {
			continue
		}
		u := m.ResponseMeta.Usage
		if u.PromptTokens != 0 || u.CompletionTokens != 0 || u.TotalTokens != 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		return usageEstimate{}, false
	}
	u := msgs[idx].ResponseMeta.Usage
	// 思考的减项：厂商给了明细就用它，否则在滤之前对原文做一次同词表的估算。
	reasoning := u.CompletionTokensDetails.ReasoningTokens
	if reasoning <= 0 {
		reasoning = counter.Count(msgs[idx].ReasoningContent)
	}
	visible := u.CompletionTokens - reasoning
	if visible < 0 {
		visible = 0
	}
	rows := make([]ModelMessage, 0, len(msgs)-idx-1)
	for _, m := range msgs[idx+1:] {
		// seq 传占位 0：这些行只进 countInput 数长度，不落库、不参与坐标推导。
		rows = append(rows, newRowFromMessage(currentRun, 0, m))
	}
	delta := countInput(counter, "", rows)
	return usageEstimate{baseline: u.PromptTokens, visible: visible, delta: delta,
		total: u.PromptTokens + visible + delta}, true
}

// midfoldHandler 是 mid-run fold 的钩子：每次模型调用前对当前 state 自测长度、超线走同一条
// 折叠链；成功终态时把折后 state 存进容器供 worker 落库。挂载次序 = agentSpecHandler /
// noteSnapshotHandler 之后、agentLog 之前（19v6-10 §4：日志看到的是模型实际所见的折后输入）。
// 子代理侧由 agentFoldHandler 另挂（doc/17 §6）——不共用本类型，差异在容器，见彼处注释。
// 计数/折叠用的 system 从 state 首条取（M1 起）——快照 handler 已把
// 快照段拼进它，数到的就是模型实见的（不再用构造期形参，「单一真源全从 state 读」）。
type midfoldHandler struct {
	*adk.BaseChatModelAgentMiddleware
	window      int // = compactWindow：只喂 foldInput 的 trim 预算（B4，19v6-09 §8）；与 limit 同源、各用各的
	limit       int // = compactLimit（compactLimitFor(window)）；<= 0 = 折叠休眠（只滤思考、不自测不折）
	counter     TokenCounter
	summarize   func(context.Context, string) (string, error) // = summarizer.summarize（同一条链同一个摘要器）
	fileEffects map[string]string                             // = firstPartyFileEffects（钉块提取的判定入口）
}

func newMidfoldHandler(counter TokenCounter, window, limit int,
	summarize func(context.Context, string) (string, error), fileEffects map[string]string) *midfoldHandler {
	return &midfoldHandler{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		window:                       window,
		limit:                        limit,
		counter:                      counter,
		summarize:                    summarize,
		fileEffects:                  fileEffects,
	}
}

// BeforeModelRewriteState：每次模型调用前到场。休眠门（limit<=0 或 counter 缺）= 直通；
// 否则转换 → 长度自测（有账单走 usageBasedInput，无账单走 countInput，doc/24 §5/§6）→ 超线折
// → 重写 state（ToolInfos 不动）→ 容器同步 → 日志。放行前恒清空 state 上的思考（doc/24 §3）：
// 顺序是「先估（还要读 ReasoningContent 做减项）→ 再折 → 最后滤」，滤早了减项只能估成 0。
// 折叠失败（errStillOver / 摘要失败）与断言违例都返回错误：run 走 status=error、零写入（裁定 7）。
func (h *midfoldHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	box := foldBoxFrom(ctx)
	if box == nil {
		return ctx, state, nil
	}
	// 滤思考与折叠休眠无关：limit<=0 / counter 缺时不再重发上一枪思考，仍然是对请求形状的正确修法。
	if h.limit <= 0 || h.counter == nil {
		stripReasoning(state.Messages)
		return ctx, state, nil
	}
	// 采用点 1（B3，D15）：guard 刚救过（槽非空）→ 本轮跳过估算与折叠，直接把 state 换成
	// 「system + 槽 + 尾随行」。尾随行 = 救援点之后 ADK 追加的新行（重试回复、其工具结果），
	// 切分依据 = 槽里的救援点行数（见 setAdopted 注释）。base 换代 = 槽（坐标链随动）；
	// 放行前照 doc/24 规则滤思考（尾随行里的重试回复带上一枪思考）。断言违例（state 比救援点
	// 还短 = 重写链坏了）返回错误（裁定 7，出口与转换层同族）。
	if slot, rescuedLen := box.takeAdopted(); slot != nil {
		if len(state.Messages) < rescuedLen {
			return ctx, state, fmt.Errorf("midfold: state 行数 %d 少于救援点 %d（采用链违例）", len(state.Messages), rescuedLen)
		}
		rewritten := append([]*schema.Message{state.Messages[0]}, toSchemaMessages(slot)...)
		state.Messages = append(rewritten, state.Messages[rescuedLen:]...)
		box.setBase(slot)
		stripReasoning(state.Messages)
		return ctx, state, nil
	}
	base := box.baseRows()
	sys, history, err := convertState(base, box.run, state.Messages)
	if err != nil {
		return ctx, state, err
	}
	var total int
	if est, ok := usageBasedInput(h.counter, box.run, state.Messages); ok {
		// 账单在读的地方（原始 state）只数锚点之后的新行，整份历史不再重数一次。
		// 对照用的全量自测不在这里算：每轮都算一遍等于没省，doc/24 §10 的收益正是跳过整份重数；
		// 两条口径同量级由单测钉（midfold_test TestUsageBasedInput）。
		total = est.total
		log.Printf("compact usage %s/%s: 基线 %d + 可见输出 %d + 新增 %d = %d tok，limit %d",
			box.session, box.run, est.baseline, est.visible, est.delta, total, h.limit)
	} else {
		total = countInput(h.counter, sys, history)
	}
	if total <= h.limit {
		stripReasoning(state.Messages)
		return ctx, state, nil
	}
	log.Printf("compact mid-run %s/%s: 输入 %d tok > limit %d，进入折叠", box.session, box.run, total, h.limit)
	folded, staged, err := foldInput(ctx, sys, history, box.run, h.limit, h.window, h.counter, h.summarize, h.fileEffects)
	if err != nil {
		return ctx, state, err
	}
	// 摘要行打戳（foldInput 的产物无坐标）：挂点 = AttachRun、seq=0——落库时占该 run 的 seq0 槽，
	// 撞槽（同 run 上一次 mid-run fold 的摘要）由 EndRun 的 UPSERT 接住换代（A3，§5）。
	folded[0].RunID = staged.AttachRun
	folded[0].Seq = 0
	// 重写 state：system 原样留队首、其余 = 折算行；ToolInfos / DeferredToolInfos 不为所动
	// （wrapper 拿返回 state 的 ToolInfos 走 model.WithTools——动它即毁声明面）。
	state.Messages = append([]*schema.Message{state.Messages[0]}, toSchemaMessages(folded)...)
	box.setBase(folded)
	box.appendStaged(*staged)
	curUnits := droppedRunUnits(history, folded, box.run)
	log.Printf("compact mid-run %s/%s: 压 %d 单位（含当前 run %d 轮），摘要 %d tok",
		box.session, box.run, len(staged.DropRunIDs)+len(staged.MergedSummaryRunIDs)+curUnits, curUnits,
		h.counter.Count(staged.Summary))
	return ctx, state, nil
}

// AfterAgent：成功终态的取货口——把最终 state 转回 mh 形状存进容器，worker 的 defer 读它落库。
// 恒做、不受休眠门限制：window=0 时 mh 写入源同样切到折后 state 派生（跑的是同一套坐标）。
// error / 取消不触发（ADK 只在成功终态调 afterAgentFunc）——零写入家族严丝合缝。
func (h *midfoldHandler) AfterAgent(ctx context.Context, state *adk.ChatModelAgentState) (context.Context, error) {
	box := foldBoxFrom(ctx)
	if box == nil {
		return ctx, nil
	}
	// 采用点 2（B3，D15）：救援发生在最后一次模型调用（收尾轮）——槽没被采用点 1 取走，
	// 在这里兜底入 final：槽 + 尾随行（救援点之后 ADK 追加的新行 = 最终答复），尾随行按槽坐标
	// 续接（newRowFromMessage）。不能走 convertState：state 鼓着（旧行还是救援前的原样、
	// 与槽对不上）、base 未换代。缺这点 = 被救的最后一轮落坏账（最终答复行不进 mh）。
	if slot, rescuedLen := box.takeAdopted(); slot != nil {
		if len(state.Messages) < rescuedLen {
			return ctx, fmt.Errorf("midfold: state 行数 %d 少于救援点 %d（采用链违例）", len(state.Messages), rescuedLen)
		}
		final := append([]ModelMessage{}, slot...)
		next := nextRunSeq(slot, box.run)
		for _, m := range state.Messages[rescuedLen:] {
			final = append(final, newRowFromMessage(box.run, next, m))
			next++
		}
		box.setFinal(final)
		return ctx, nil
	}
	_, history, err := convertState(box.baseRows(), box.run, state.Messages)
	if err != nil {
		return ctx, err // 断言违例：run 走 error（干净 A、零写入），出口与 BeforeModelRewriteState 同族
	}
	box.setFinal(history)
	return ctx, nil
}

// agentFoldHandler 是子代理侧的 mid-run fold 钩子（doc/17 §6，台账 17-子代理折叠 A1）：挂子代理
// 装配点，复用主 run 的同一条「估→折→滤」链（同 counter / compactLimit / 摘要器 / fileEffects）。
// 与 midfoldHandler 的两条差异（planning D1 施工禁令）：
//   - 容器只读不写：ctx 里的 foldBox 是 root 的——只读 box.run / box.session（file-ops 块的 r_
//     指针要指 runlog，而 runlog 工具行本来就挂 root runId）；不调 setBase/appendStaged、摘要行
//     不打 seq=0、产物永不落库。
//   - 无 AfterAgent：子代理 state 跑完即弃，没有取货口。
//
// 字段全是每规格常量，不含任何 per-delegation 状态：同一场 run 里主代理可以在一条消息里并行发
// 两次同名委派（eino ToolsNode 默认并行，tako 未设 ExecuteSequentially），两个 goroutine 会同时
// 进同一个实例——一次调用的东西只走参数与 ADK 给的 state。
type agentFoldHandler struct {
	*adk.BaseChatModelAgentMiddleware
	name        string
	window      int // 只喂 foldInput 的 trim 预算（B4），同 midfoldHandler.window
	limit       int // = compactLimit：<= 0 休眠（只滤思考、不自测不折）
	counter     TokenCounter
	summarize   func(context.Context, string) (string, error)
	fileEffects map[string]string
}

func newAgentFoldHandler(name string, counter TokenCounter, window, limit int,
	summarize func(context.Context, string) (string, error), fileEffects map[string]string) *agentFoldHandler {
	return &agentFoldHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		name: name, window: window, limit: limit, counter: counter, summarize: summarize, fileEffects: fileEffects}
}

// 编译期锚：接口一缺，编译不过（同 midfoldHandler）。
var _ adk.ChatModelAgentMiddleware = (*agentFoldHandler)(nil)

// foldChainErrorMark 是子代理折叠 handler 给自己返回的每个错误加的前缀（A2，planning D2）。
// 判定走文本不走 errors.Is：handler 错误到 delegationTool 之间隔着 ADK graph 的 [NodeRunError]
// 包裹层（v0.9.21 活体实证），Go 错误链不保证端到端保留——与 IsOverflowError 用报文正则同一理由。
const foldChainErrorMark = "子代理折叠链:"

// BeforeModelRewriteState：估长度（有账单 usageBasedInput、无账单 convertState+countInput，
// doc/24 §5/§6）→ 超 limit 走同一条 foldInput → 重写子代理自己的 state.Messages（ToolInfos 不动）
// → 滤思考（滤必须排在估之后，减项读 ReasoningContent）。子代理没有基点：convertState 传 nil，
// 全部历史按本 run 新行处理（坐标只进折叠分组，永不落库）。
// 折叠失败带 foldChainErrorMark 前缀上抛——delegationTool 据此把它转成委派的错误文本结果，
// 父 run 保持 complete（A2，planning D2）；错误原文随文本一并交回，不吞信息。
func (h *agentFoldHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if h.limit <= 0 || h.counter == nil {
		stripReasoning(state.Messages)
		return ctx, state, nil
	}
	box := foldBoxFrom(ctx)
	if box == nil {
		return ctx, state, nil
	}
	sys, history, err := convertState(nil, box.run, state.Messages)
	if err != nil {
		return ctx, state, fmt.Errorf("%s %w", foldChainErrorMark, err)
	}
	var total int
	if est, ok := usageBasedInput(h.counter, box.run, state.Messages); ok {
		total = est.total
		log.Printf("agent usage %s: 基线 %d + 可见输出 %d + 新增 %d = %d tok，limit %d",
			h.name, est.baseline, est.visible, est.delta, total, h.limit)
	} else {
		total = countInput(h.counter, sys, history)
	}
	if total <= h.limit {
		stripReasoning(state.Messages)
		return ctx, state, nil
	}
	folded, _, err := foldInput(ctx, sys, history, box.run, h.limit, h.window, h.counter, h.summarize, h.fileEffects)
	if err != nil {
		return ctx, state, fmt.Errorf("%s %w", foldChainErrorMark, err)
	}
	state.Messages = append([]*schema.Message{state.Messages[0]}, toSchemaMessages(folded)...)
	// 观测面（planning D3，格式钉死）：前 = 折前估算，后 = 折后这份的计数。路径从钉块解析回来
	// （foldInput 已把块钉进摘要末尾，parseFileOpsBlock 是它的解析半边）。
	ops := parseFileOpsBlock(folded[0].Text)
	var paths []string
	for _, e := range ops.written {
		paths = append(paths, e.path)
	}
	for _, e := range ops.edited {
		paths = append(paths, e.path)
	}
	for _, e := range ops.read {
		paths = append(paths, e.path)
	}
	log.Printf("agent fold %s: %d→%d tok; 钉 %d 路径: %s",
		h.name, total, countInput(h.counter, sys, folded), len(paths), strings.Join(paths, ","))
	return ctx, state, nil
}
