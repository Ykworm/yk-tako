// 本文件是上下文压缩（compact）的算法与装配：C 系列的第一步（planning §10，C1）。
//
// 设计来源 doc/12-上下文压缩.md §9（算法伪代码）+ doc/13（T1–T9）——**旧线口径**：钩子
// BeforeModelRewriteState、run_archive、event_log、compact_rag 在 v6 都没有对应物；
// 取其算法与取舍逻辑，落点按 v6 前提重推（planning §10.4）。v6 的落点 D1 已就位
// （run_model_messages.compacted 列 + 部分索引 + seq=0 语义，pgstore.go），本文件补上
// **触发者与写入者**：触发在 worker 组装输入时自测长度（prepareInput），写入在 EndRun(complete)
// 同笔事务（store.go）。
//
// doc/12 → v6 的逐条映射（§10.4，偏离全部登记）：
//   - 单位：旧「轮」= 新 run；「已完成」= 非当前 run 的行（error run 只留 user 半，照压）。
//   - 快速路（日常零变化）：CountMessages(系统 + 全量非 compacted 行) ≤ limit → 原样送模型。
//   - 超限进循环：n 从 20 起降（keep = 最近 n 个已完成 run，drop = 其余；drop 空 → n--）；
//     每档 Summarize(旧摘要 + 渲染 drop) → 拼装 [摘要行] + [keep 行] + [当前 run 行] →
//     CountMessages < limit 则用它作本轮输入、stagedCompaction 挂收尾；否则 n--。
//   - n=0：drop = 全部已完成原文 run（旧摘要行天然并入）；仍超限 → errStillOver：
//     **不调主模型、status=error**（页面上这一轮是错误，无助手回复——现役 error 路径原样）。
//   - 落库时机（承 D1 事务边界）：发送前只 Stage 在内存；EndRun 仅 complete 分支把
//     stagedCompaction 并入同一笔 PG 事务（放闸 + 定稿 + compacted 标记 + seq=0 行）；error / 闸失 / 进程死 →
//     零写入，下一轮从原文重压——「原文从未被动过」，连旧线的发送前快照都不必有。
//   - 偏离登记：无 RAG 两档舞（检索块不存在）；无 Notify(正在压缩)（帧面零变化，§10.3 第 8 条）；
//     无 Usage 基数（v6 无 Usage 列，全量 BPE）；窗口查找链不搬（旗标即源，§10.3 第 9 条）；
//     旧线把摘要压进本轮 user_message——v6 不复刻（摘要独立成 seq=0 行）。
//
// 摘要行挂点的不变量（§10.3 第 6 条读法 A 的机械半边）：`UNIQUE(run_id, seq)` 逼着摘要行
// 只能挂在**本事件最后折入的原文 run**——旧摘要所在 run 的 seq=0 槽被旧行永久占着（行在库就在，
// compacted=true 不释放唯一约束），挂回去必撞。所以折叠集合永远含 ≥1 个新原文 run；
// 无可折原文（rawRuns 空）时连「只再压摘要本身」都做不了（没有落点），直接 errStillOver。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// compactKeepRuns 是循环起点 n（doc/12 §5「从 20 轮开始留」）。
// 「20 轮只数它前面已经结束的对话」：当前 run 的行不进 keep/drop 划分（v6 里它随 tail 原样带在末尾）。
const compactKeepRuns = 20

// compactPrompt 是摘要提示词（**只有一份**，doc/12 §6/§9 同款措辞）。
// 独立性：不含任何会话内容，调用方用同一个 ChatStream 另起一次流式生成（不新开 agent）。
const compactPrompt = `你在为一段较早的对话维护一份长期摘要，供后续轮次继续使用。` +
	`按这段内容实际在谈的事做摘要，只保留对后续仍然有用的信息。` +
	`能抽到的才写成这些栏目：已知事实、关键决策、未解决问题、用户偏好；没有的栏目不要编造，直接省略。` +
	`只输出摘要正文，不要开场白。`

// errStillOver 是「压到头仍不低于 limit」的哨兵（doc/12 §9 的 ErrStillOver）：
// 调用方（worker）把它当普通错误处理——不调主模型、status=error、零写入。
var errStillOver = errors.New("compact: 摘要后仍不低于 limit（不调主模型）")

// TokenCounter 是长度自测的全部要求：一段文本的 token 数。
// 装配期给 cl100k（NewCl100kCounter），单测注入假计数器（不触网）。
type TokenCounter interface {
	Count(text string) int
}

// cl100kCounter 是 TokenCounter 的正式实现（doc/12 §3：编码固定 cl100k_base，不按模型再配一份）。
type cl100kCounter struct{ enc *tiktoken.Tiktoken }

// NewCl100kCounter 装配期构造（main 在 -compact-window > 0 时调用）。
// 词表首次从 openaipublic 拉取并缓存到磁盘（TIKTOKEN_CACHE_DIR / os.TempDir），之后启动即命中；
// 拉取失败 = 返回错误，由 main fail fast——「压不动」不能在超窗后才发现（§10.3 第 10 条）。
func NewCl100kCounter() (TokenCounter, error) {
	enc, err := tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
	if err != nil {
		return nil, err
	}
	return cl100kCounter{enc: enc}, nil
}

// Count 数一段文本的 BPE token 数。空串 = 0。
// 计数口径（§10.4）：只数消息文本本身，不发明 per-message 开销——doc/12 §3 只说「用 BPE 数消息」。
func (c cl100kCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	return len(c.enc.EncodeOrdinary(text))
}

// StagedCompaction 是一次折叠（发送前 Stage 的产物）的落库指令，EndRun(complete) 依它写两笔：
// 被压 run 的原文行 compacted=true（保留不删）+ 新的 seq=0 摘要行。
// 不变式：AttachRun 必 ∈ DropRunIDs、且是本事件最后折入的原文 run（挂点唯一性，见文件头）。
type StagedCompaction struct {
	DropRunIDs []RunID // 被压掉的原文 run + 旧摘要行的 run
	Summary    string  // 新摘要全文（已过 limit 检查，非空）
	AttachRun  RunID   // 摘要行挂点（= DropRunIDs 中时序最后的一个）
}

// countInput 数「将要发送的这一份」：系统消息（若有）+ 所有行文本（doc/12 §3：数消息本身）。
func countInput(counter TokenCounter, system string, msgs []ModelMessage) int {
	n := 0
	if system != "" {
		n += counter.Count(system)
	}
	for _, m := range msgs {
		n += counter.Count(m.Text)
	}
	return n
}

// runRows 是分组后的一个已完成 run：id + 其可见原文行（seq≥1）。
type runRows struct {
	id   RunID
	rows []ModelMessage
}

// foldInput 是超限后的折叠主循环（doc/12 §9 的逐条 v6 映射，见文件头）：
// 对每个 n 档调一次 summarize（旧摘要 + drop 渲染），拼装候选输入；count < limit 即定案。
// 纯函数——summarize 由调用方注入（worker 给「同 ChatStream 另起一次流式调用」），
// 单测注入假件即可直测循环边界（C3）。
//
// 返回值：新输入（摘要行 + keep + 当前 run 行）与 stagedCompaction；errStillOver = 不调主模型、零写入。
// 前提：只有超限（countInput(history) > limit）才该进来；counter/summarize 非 nil。
func foldInput(ctx context.Context, system string, history []ModelMessage, currentRun RunID,
	limit int, counter TokenCounter, summarize func(context.Context, string) (string, error)) ([]ModelMessage, *StagedCompaction, error) {

	// ---- 分组：history 已按 (run_id, seq) 有序，run_id 是 ULID（字典序 = 时序）。----
	// 不变量（D1 形态的自然结果）：可见行按 run 统一——被压 run 的行全 compacted（读不出来），
	// 未压 run 的行全可见；seq=0（若有）必是该 run 唯一可见行。
	var rawRuns []runRows      // 有可见原文行的已完成 run（按 run 时序）
	var tail []ModelMessage    // 当前 run 的行（发送时原样带在末尾，不进折叠）
	var oldSummaryRuns []RunID // 旧摘要行（seq=0 可见行）所在 run —— 重压时并入
	var oldSummaryTexts []string
	for i := 0; i < len(history); {
		id := history[i].RunID
		j := i
		for j < len(history) && history[j].RunID == id {
			j++
		}
		rows := history[i:j]
		i = j
		if id == currentRun {
			tail = append(tail, rows...)
			continue
		}
		var raws []ModelMessage
		for _, m := range rows {
			if m.Seq == 0 {
				oldSummaryRuns = append(oldSummaryRuns, id)
				oldSummaryTexts = append(oldSummaryTexts, m.Text)
				continue
			}
			raws = append(raws, m)
		}
		if len(raws) > 0 {
			rawRuns = append(rawRuns, runRows{id: id, rows: raws})
		}
	}

	// ---- n 从 20 降序（doc/12 §9 loop；drop 空 → n--）。 ----
	n := compactKeepRuns
	for {
		dropCount := len(rawRuns) - min(n, len(rawRuns))
		if dropCount == 0 {
			if n == 0 {
				return nil, nil, errStillOver // 无原文可折（rawRuns 空）且 n 已到 0
			}
			n--
			continue
		}
		drop := rawRuns[:dropCount]
		keep := rawRuns[dropCount:]

		foldText := renderFold(oldSummaryTexts, drop)
		summary, err := summarize(ctx, foldText)
		if err != nil {
			return nil, nil, err // Summarize 失败：不 Stage、不发（doc/12 §9）
		}

		assembled := make([]ModelMessage, 0, len(history)+1)
		assembled = append(assembled, ModelMessage{Role: "user", Text: summary})
		for _, g := range keep {
			assembled = append(assembled, g.rows...)
		}
		assembled = append(assembled, tail...)

		total := countInput(counter, system, assembled)
		if total < limit {
			stagedCompaction := &StagedCompaction{
				DropRunIDs: append(append([]RunID{}, oldSummaryRuns...), ids(drop)...),
				Summary:    summary,
				AttachRun:  drop[len(drop)-1].id, // 挂点 = 本事件最后折入的原文 run（唯一不撞 UNIQUE 的选择）
			}
			return assembled, stagedCompaction, nil
		}
		if n == 0 {
			// 摘要本身仍不低于 limit（doc/12 §9）。带数字死因：限值是量级、摘要长度看模型，
			// 没有这几个数就没法判断「差多少才能过」。
			return nil, nil, fmt.Errorf("%w（折叠 %d 个 run 共 %d tok → 摘要 %d tok；发送合计 %d tok ≥ limit %d）",
				errStillOver, dropCount, counter.Count(foldText), counter.Count(summary), total, limit)
		}
		n--
	}
}

// renderFold 渲染一次摘要调用的输入：旧摘要（重压时，天然并入）在前，被压段原文按 run 时序在后。
// 旧线还有一份检索块（RAG）——v6 无 RAG（§10.3 第 7 条），登记为偏离。
func renderFold(oldTexts []string, drop []runRows) string {
	var b strings.Builder
	if len(oldTexts) > 0 {
		b.WriteString("（此前摘要，请合并进新的摘要）\n")
		for _, t := range oldTexts {
			b.WriteString(t)
			b.WriteString("\n")
		}
	}
	for i, g := range drop {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, m := range g.rows {
			b.WriteString(m.Role)
			b.WriteString(": ")
			b.WriteString(m.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ids 取 run 组的 id 序列。
func ids(groups []runRows) []RunID {
	out := make([]RunID, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.id)
	}
	return out
}

// summarize 用同一个 ChatStream 另起一次流式生成（system = 摘要提示词、user = 渲染文本），
// 排干取全文（doc/12 §4：压缩是钩子里调用一次摘要模型的 Generate，不另开 agent）。
// 空摘要 = 错误（拒收，与失败同路；行表形态下空摘要会变成一条空洞的 seq=0 行）。
func (w *Worker) summarize(ctx context.Context, input string) (string, error) {
	stream, err := w.chat.Stream(ctx, []*schema.Message{
		schema.SystemMessage(compactPrompt),
		schema.UserMessage(input),
	})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var b strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if text, ok := w.translate(chunk); ok {
			b.WriteString(text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("compact: 摘要为空，拒收")
	}
	return out, nil
}
