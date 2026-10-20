// 本文件是崩溃续跑的账本层（doc/30 §6）：把「模型说到哪、哪些工具可能已经跑过」写成 PG 行。
//
// 三处写入全部 root-only：assistant 行由 worker 的事件循环写，tool_call / tool_result 由
// 工具包装层写。包装点必须在 handler 链尾（journalWrapHandler）：一手池在 NewWorker 装配，
// 但 MCP 工具（mcp.go）与委派工具（agents.go）是 run 起手才 append 进 runCtx.Tools 的——
// 只包池会让整类调用漏账。链尾包壳的顺序同时定义了子代理边界：agentSpecHandler 先跑，
// 给子代理的入口快照拿的是未包壳工具；journalTool 里的 callerNameFrom 守卫是第二道闸。
//
// 执行顺序是 journalTool(wrapToolErrors(t))：错误回注在内层（工具自己的错误变成「错误：…」
// 结果、不再上抛），记账在外层——正常结果、工具报错、错误回注都会先留下一条 tool_result 行。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// errJournalGateLost = 账本写入时本执行者已不是当前令牌持有者（doc/30 §7 围栏）：
// 旧进程的 assistant/tool 行不许落在接管者的账本读取之后。写失败一律升级为 run error。
var errJournalGateLost = errors.New("账本写入时闸已不属于本 run")

// interruptedResultText 是缺 result 的调用在重放时回注给模型的合成结果（doc/30 §9.2，
// 文本逐字节固定）：账本只知道「intent 存在、result 不存在」，不知道工具到底执行没执行——
// 自动重跑会让副作用重复，如实说「结果未知」是唯一安全的默认（at-most-once）。
const interruptedResultText = "错误：工具调用在执行中被进程中断，没有拿到结果；引擎不会自动重跑这次调用。需要的话请重新发起。"

// journalTarget 是账本的写入坐标：哪一场 run、哪个接管令牌。由 Worker.Run 塞进 ctx
// （epoch 来自 Run 参数：POST 路径 1，A4a 的 resume 路径传 claim 返回值）。工具与 assistant 行都从这里取。
type journalTarget struct {
	runID RunID
	epoch int64
}

type journalTargetKey struct{}

func withJournalTarget(ctx context.Context, t journalTarget) context.Context {
	return context.WithValue(ctx, journalTargetKey{}, t)
}

func journalTargetFrom(ctx context.Context) (journalTarget, bool) {
	t, ok := ctx.Value(journalTargetKey{}).(journalTarget)
	return t, ok
}

// journalWrapHandler 注册在 handler 链最后一位（overflow guard 之后）：此时一手池、MCP 工具、
// 委派工具都已进 runCtx.Tools，一次遍历把整池换成记账壳。子代理入口快照在 agentSpecHandler
// 里已经做完，拿到的是未包壳工具——子代理内部调用天然不记账。
type journalWrapHandler struct {
	*adk.BaseChatModelAgentMiddleware
	store *Store
}

func (h *journalWrapHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	for i, t := range runCtx.Tools {
		runCtx.Tools[i] = journalTool(h.store, t)
	}
	return ctx, runCtx, nil
}

// journalTool 只包可调用工具（ToolsNode 也只执行可调用工具）；Info 透传，声明面不变。
func journalTool(store *Store, t tool.BaseTool) tool.BaseTool {
	inner, ok := t.(tool.InvokableTool)
	if !ok {
		return t
	}
	return &journalInvokableTool{inner: inner, store: store}
}

// journalInvokableTool 是记账壳。intent 写失败**不执行底层工具**（不会出现「副作用发生了但
// 账本没有」）；result 写失败时副作用已经发生，返回错误让 run 走 error——账本留下无结果的
// intent，续跑按「已中断、不自动重跑」处理（doc/30 §9.2）。
type journalInvokableTool struct {
	inner tool.InvokableTool
	store *Store
}

func (t *journalInvokableTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *journalInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	target, ok := journalTargetFrom(ctx)
	if !ok {
		return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	}
	// 第二道子代理闸：即使将来有人把包装点挪到入口快照之前，子代理调用也不落账。
	if name, ok := callerNameFrom(ctx); ok && name != "" {
		return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	}
	info, err := t.inner.Info(ctx)
	if err != nil {
		return "", err
	}
	callID := compose.GetToolCallID(ctx)
	if err := t.store.InsertJournal(ctx, RunJournal{
		RunID: target.runID, Epoch: target.epoch, Role: "tool_call",
		ToolCallID: callID, ToolName: info.Name, Args: argumentsInJSON,
	}); err != nil {
		return "", fmt.Errorf("账本 tool_call 写入失败，工具未执行: %w", err)
	}
	out, err := t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	if err != nil {
		// 工具错误已被内层 wrapToolErrors 转成结果，能走到这里的是取消/中断一类的真错误：
		// 不写 result，run 照常走 error 出口；账本留下 intent = 「可能执行过、结果不可知」。
		return "", err
	}
	if err := t.store.InsertJournal(ctx, RunJournal{
		RunID: target.runID, Epoch: target.epoch, Role: "tool_result",
		Text: out, ToolCallID: callID, ToolName: info.Name,
	}); err != nil {
		return "", fmt.Errorf("账本 tool_result 写入失败（工具已执行）: %w", err)
	}
	return out, nil
}

// journalReplay 是账本重放的产物（doc/30 §9.1）：模型输入行、EndRun 的两个投影所用 turn、
// resume 帧正文（所有 root assistant 正文拼接）、以及「最后一条 assistant 没有 tool_calls」的终点判定。
type journalReplay struct {
	Rows  []ModelMessage
	Turn  []TurnEntry
	Seed  string
	Final bool
}

// replayJournal 把账本规范重放成续跑所需的四样产物。纯函数（单测直测）。
//
// 排序按 call id、不按 seq（doc/30 §9.1）：工具在 ToolsNode 里并行执行，result 的真实落账
// 顺序可能早于/晚于其他调用；模型输入必须是「assistant(tool_calls) → 它名下各 result」
// 的规范形状。输出 Seq 从 2 连续排（user 占 1），RunID 固定当前 run。
//
// 缺结果的调用**不报错、不重跑**：回注 interruptedResultText（doc/30 §9.2，at-most-once）；
// 孤儿 intent/result（不属于任何 assistant 行的调用）按相邻出现归成一组，输出一条合成的
// assistant 行让 tool 行在协议里有合法发起者——参数取 intent 的 args。
func replayJournal(runID RunID, rows []RunJournal) (journalReplay, error) {
	var out journalReplay
	results := make(map[string]RunJournal, len(rows))
	owned := make(map[string]bool)
	for _, r := range rows {
		switch r.Role {
		case "assistant":
			calls, err := parseJournalCalls(r)
			if err != nil {
				return out, err
			}
			for _, c := range calls {
				owned[c.ID] = true
			}
		case "tool_result":
			if r.ToolCallID != "" {
				if _, dup := results[r.ToolCallID]; !dup {
					results[r.ToolCallID] = r
				}
			}
		}
	}

	seq := 2
	appendRow := func(m ModelMessage, t TurnEntry) {
		m.RunID, m.Seq = runID, seq
		seq++
		out.Rows = append(out.Rows, m)
		out.Turn = append(out.Turn, t)
	}
	appendTool := func(callID, toolName, text, model string) {
		appendRow(ModelMessage{Role: "tool", Text: text, ToolCallID: callID},
			TurnEntry{Role: "tool", Text: text, ToolCallID: callID, ToolName: toolName, Model: model})
	}
	emitCalls := func(calls []schema.ToolCall, model string) {
		for _, c := range calls {
			name := c.Function.Name
			text := interruptedResultText
			if res, ok := results[c.ID]; ok {
				if name == "" {
					name = res.ToolName
				}
				text = res.Text
			}
			appendTool(c.ID, name, text, model)
		}
	}

	var orphans []RunJournal
	flushOrphans := func() {
		if len(orphans) == 0 {
			return
		}
		group := orphans
		orphans = nil
		// 相邻孤儿按 call id 归组（保留首次出现顺序）；intent 给参数与工具名，result 给正文。
		intents := map[string]RunJournal{}
		order := make([]string, 0, len(group))
		for _, r := range group {
			if r.ToolCallID == "" {
				continue
			}
			if _, seen := intents[r.ToolCallID]; !seen {
				order = append(order, r.ToolCallID)
			}
			if r.Role == "tool_call" || intents[r.ToolCallID].Role == "" {
				intents[r.ToolCallID] = r
			}
		}
		calls := make([]schema.ToolCall, 0, len(order))
		for _, id := range order {
			in := intents[id]
			args := in.Args
			if args == "" {
				args = "{}"
			}
			calls = append(calls, schema.ToolCall{ID: id, Type: "function",
				Function: schema.FunctionCall{Name: in.ToolName, Arguments: args}})
		}
		appendRow(ModelMessage{Role: "assistant", ToolCalls: calls},
			TurnEntry{Role: "assistant", ToolCalls: calls})
		emitCalls(calls, "")
	}

	for _, r := range rows {
		switch r.Role {
		case "assistant":
			flushOrphans()
			calls, err := parseJournalCalls(r)
			if err != nil {
				return out, err
			}
			bill, err := parseJournalUsage(r)
			if err != nil {
				return out, err
			}
			appendRow(ModelMessage{Role: "assistant", Text: r.Text, ToolCalls: calls},
				TurnEntry{Role: "assistant", Text: r.Text, ToolCalls: calls, Model: r.Model, Usage: bill})
			out.Seed += r.Text
			if len(calls) == 0 {
				// 这一轮是终点：输出它、停（之后的行是异常写入，不参与模型输入）。
				out.Final = true
				return out, nil
			}
			emitCalls(calls, r.Model)
		case "tool_call", "tool_result":
			if r.ToolCallID != "" && !owned[r.ToolCallID] {
				orphans = append(orphans, r)
			}
			// 属于某条 assistant 的 result 已在 assistant 那一拍按计划顺序输出，这里跳过。
		}
	}
	flushOrphans()
	return out, nil
}

func parseJournalCalls(r RunJournal) ([]schema.ToolCall, error) {
	if r.ToolCalls == "" {
		return nil, nil
	}
	var calls []schema.ToolCall
	if err := json.Unmarshal([]byte(r.ToolCalls), &calls); err != nil {
		return nil, fmt.Errorf("账本 assistant 行 seq=%d tool_calls 解析失败: %w", r.Seq, err)
	}
	return calls, nil
}

func parseJournalUsage(r RunJournal) (*UsageBill, error) {
	if r.Usage == "" {
		return nil, nil
	}
	var bill UsageBill
	if err := json.Unmarshal([]byte(r.Usage), &bill); err != nil {
		return nil, fmt.Errorf("账本 assistant 行 seq=%d usage 解析失败: %w", r.Seq, err)
	}
	return &bill, nil
}

// writeJournalAssistant 写 root 一枪的 assistant 行：drain 排干、拿到 merged 之后立即写，
// 先于 usage / tool_call 帧与 turn（doc/30 §6.3）。思考不进账本（与 stripReasoning 的输入侧同源）。
// 只由 agent == "" 的事件调用；子代理的 assistant 对 root 只是委派工具的一次结果。
func writeJournalAssistant(ctx context.Context, store *Store, merged *schema.Message, model string, bill *UsageBill) error {
	target, ok := journalTargetFrom(ctx)
	if !ok {
		return nil
	}
	calls := ""
	if len(merged.ToolCalls) > 0 {
		b, err := json.Marshal(merged.ToolCalls)
		if err != nil {
			return err
		}
		calls = string(b)
	}
	usage := ""
	if bill != nil {
		b, err := json.Marshal(bill)
		if err != nil {
			return err
		}
		usage = string(b)
	}
	return store.InsertJournal(ctx, RunJournal{
		RunID: target.runID, Epoch: target.epoch, Role: "assistant",
		Text: merged.Content, ToolCalls: calls, Model: model, Usage: usage,
	})
}
