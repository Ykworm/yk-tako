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
// 没有新折叠算法：foldInput / countInput / renderFold 三个签名不动（planning 裁定 2），
// 本文件只做三件事——state ⇄ mh 形状的转换、容器读写、两行日志（格式钉死在 planning 裁定 6）。
//
// 落库路径不变（PG 一个字不动）：本文件只把折后 state 存进容器；唯一写点仍是 EndRun——
// worker 的 defer 从容器读 staged 列表与折后行（mhRowsFromFold）、complete 才落。
// AfterAgent 仅成功终态触发（error / 取消不触发）——零写入家族严丝合缝。
//
// 断言违例（system 非首 / 三件套）返回错误、不 panic（planning 裁定 7）：ADK 把 handler 错误
// 透传成 run error（eino v0.9.21 实读：graph 节点错误 → generator 的 Err 事件），run 走干净 A。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// foldBox 是 run 级折叠容器（planning 裁定 4；ctx 传递比照 agentModelTable 先例）：
// worker.Run 建、塞 ctx，midfoldHandler 每次模型调用前读写，worker 的 defer 读回落库。
// 钩子跑在 ADK 内部 goroutine，RWMutex 兜底——正常时序下无竞态（AfterAgent 完成 → run 函数
// 返回 → generator.Close() → worker 的 it.Next() 返 false，channel close 给了 happens-before）。
type foldBox struct {
	mu      sync.RWMutex
	session SessionID
	run     RunID
	base    []ModelMessage     // 「system 与当前 run 新轮次之外」的基点（mh 形状，含 RunID/Seq）
	staged  []StagedCompaction // 折叠指令列表：Run fold 打头、mid-run 各次追加（EndRun 吃列表）
	final   []ModelMessage     // 终态 mh 投影：仅 AfterAgent（成功终态）填——error / 取消不填
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

// midfoldHandler 是 mid-run fold 的钩子：每次模型调用前对当前 state 自测长度、超线走同一条
// 折叠链；成功终态时把折后 state 存进容器供 worker 落库。挂载次序 = agentSpecHandler /
// noteSnapshotHandler 之后、agentLog 之前（19v6-10 §4：日志看到的是模型实际所见的折后输入）。
// 子代理内部不挂（§10）。计数/折叠用的 system 从 state 首条取（M1 起）——快照 handler 已把
// 快照段拼进它，数到的就是模型实见的（不再用构造期形参，「单一真源全从 state 读」）。
type midfoldHandler struct {
	*adk.BaseChatModelAgentMiddleware
	limit       int // = compactLimit（window×4/5）；<= 0 = 折叠休眠（BeforeModelRewriteState 直通）
	counter     TokenCounter
	summarize   func(context.Context, string) (string, error) // = summarizer.summarize（同一条链同一个摘要器）
	fileEffects map[string]string                             // = firstPartyFileEffects（钉块提取的判定入口）
}

func newMidfoldHandler(counter TokenCounter, limit int,
	summarize func(context.Context, string) (string, error), fileEffects map[string]string) *midfoldHandler {
	return &midfoldHandler{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		limit:                        limit,
		counter:                      counter,
		summarize:                    summarize,
		fileEffects:                  fileEffects,
	}
}

// BeforeModelRewriteState：每次模型调用前到场。休眠门（limit<=0 或 counter 缺）= 直通；
// 否则转换 → countInput（A1 基数）→ 超线折 → 重写 state（ToolInfos 不动）→ 容器同步 → 两行日志。
// 折叠失败（errStillOver / 摘要失败）与断言违例都返回错误：run 走 status=error、零写入（裁定 7）。
func (h *midfoldHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	box := foldBoxFrom(ctx)
	if box == nil || h.limit <= 0 || h.counter == nil {
		return ctx, state, nil
	}
	base := box.baseRows()
	sys, history, err := convertState(base, box.run, state.Messages)
	if err != nil {
		return ctx, state, err
	}
	total := countInput(h.counter, sys, history)
	if total <= h.limit {
		return ctx, state, nil
	}
	log.Printf("compact mid-run %s/%s: 输入 %d tok > limit %d，进入折叠", box.session, box.run, total, h.limit)
	folded, staged, err := foldInput(ctx, sys, history, box.run, h.limit, h.counter, h.summarize, h.fileEffects)
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
	_, history, err := convertState(box.baseRows(), box.run, state.Messages)
	if err != nil {
		return ctx, err // 断言违例：run 走 error（干净 A、零写入），出口与 BeforeModelRewriteState 同族
	}
	box.setFinal(history)
	return ctx, nil
}
