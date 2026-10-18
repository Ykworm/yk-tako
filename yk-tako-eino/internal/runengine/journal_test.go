package runengine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// TestJournalWrapHandlerCoversRunPool 钉 A2 的包装边界（doc/30 §6.3）：
// 链尾 handler 把整池都换成记账壳——一手池（NewWorker）、MCP（mcpToolHandler append）、
// 委派（agentSpecHandler append）三类在同一遍遍历里都覆盖；Info 透传（声明面不变）；
// root 调用落 tool_call→tool_result 两行，子代理 ctx 与无 target ctx 都只执行、不落账。
func TestJournalWrapHandlerCoversRunPool(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	// A3 起 InsertJournal 带围栏：runs 行必须真在跑、令牌相等，账本才接受写入。
	sid, rid := SessionID("s_a2"), RunID("r_a2")
	s.mustAcquire(t, sid, rid, "m_a2", "hi")

	firstParty := &fakeInvokableTool{info: &schema.ToolInfo{Name: "get_time"}, out: "2026-10-18"}
	mcpTool := &fakeInvokableTool{info: &schema.ToolInfo{Name: "mcp__x__y"}, out: "mcp-out"}
	subInner := &fakeInvokableTool{info: &schema.ToolInfo{Name: "researcher"}, out: "报告"}
	delegation := &delegationTool{inner: subInner, sem: make(chan struct{}, 1), capBytes: 0, name: "researcher"}

	runCtx := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{firstParty, mcpTool, delegation}}
	h := &journalWrapHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, store: s}
	_, got, err := h.BeforeAgent(ctx, runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent: %v", err)
	}
	if len(got.Tools) != 3 {
		t.Fatalf("Tools len = %d, want 3", len(got.Tools))
	}
	wantNames := []string{"get_time", "mcp__x__y", "researcher"}
	for i, bt := range got.Tools {
		jt, ok := bt.(*journalInvokableTool)
		if !ok {
			t.Fatalf("工具 %d 未被包成 journalInvokableTool: %T", i, bt)
		}
		info, err := jt.Info(ctx)
		if err != nil || info.Name != wantNames[i] {
			t.Fatalf("Info 透传 = (%v, %v), want name %q", info, err, wantNames[i])
		}
	}

	// root 调用（journalTarget 在 ctx、无调用方标记）：先 intent 后 result。
	rootCtx := withJournalTarget(ctx, journalTarget{runID: rid, epoch: 1})
	out, err := got.Tools[0].(tool.InvokableTool).InvokableRun(rootCtx, `{"tz":"UTC"}`)
	if err != nil || out != "2026-10-18" {
		t.Fatalf("root InvokableRun = (%q, %v)", out, err)
	}
	rows, err := s.JournalRows(ctx, rid)
	if err != nil {
		t.Fatalf("JournalRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("root 调用后账本行数 = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Role != "tool_call" || rows[0].ToolName != "get_time" || rows[0].Args != `{"tz":"UTC"}` {
		t.Fatalf("第一行 = %+v, want tool_call get_time 原文 args", rows[0])
	}
	if rows[1].Role != "tool_result" || rows[1].Text != "2026-10-18" || rows[1].ToolName != "get_time" {
		t.Fatalf("第二行 = %+v, want tool_result 结果全文", rows[1])
	}
	if rows[0].Epoch != 1 || rows[1].Epoch != 1 {
		t.Fatalf("epoch = (%d,%d), want (1,1)", rows[0].Epoch, rows[1].Epoch)
	}

	// 子代理 ctx：工具照跑，账本不增行（第二道闸；第一道是链尾包壳晚于子代理入口快照）。
	subCtx := context.WithValue(rootCtx, callerNameKey{}, "researcher")
	out, err = got.Tools[1].(tool.InvokableTool).InvokableRun(subCtx, `{}`)
	if err != nil || out != "mcp-out" || mcpTool.calls != 1 {
		t.Fatalf("子代理调用 = (%q, %v, calls=%d), want (mcp-out, nil, 1)", out, err, mcpTool.calls)
	}
	if rows, err = s.JournalRows(ctx, rid); err != nil || len(rows) != 2 {
		t.Fatalf("子代理调用后账本 = %d 行 (err=%v), want 仍 2 行", len(rows), err)
	}

	// 无 journalTarget：纯透传（未进入 run 装配的单测/工具直调路径不落账）。
	out, err = got.Tools[2].(tool.InvokableTool).InvokableRun(ctx, `{"request":"q"}`)
	if err != nil || out != "报告" || subInner.calls != 1 {
		t.Fatalf("无 target 调用 = (%q, %v, calls=%d), want (报告, nil, 1)", out, err, subInner.calls)
	}
	if rows, err = s.JournalRows(ctx, rid); err != nil || len(rows) != 2 {
		t.Fatalf("无 target 调用后账本 = %d 行 (err=%v), want 仍 2 行", len(rows), err)
	}
}

// TestJournalDeletedOnTerminalPaths 钉 A2 的清理三路（doc/30 §6.2「终态即删」）：
// EndRun 成功事务、StopRun 事务、AcquireRun 清扫死行——三条路径各自把账本删干净。
func TestJournalDeletedOnTerminalPaths(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	mustJournal := func(rid RunID) {
		t.Helper()
		if err := s.InsertJournal(ctx, RunJournal{RunID: rid, Epoch: 1, Role: "assistant", Text: "x"}); err != nil {
			t.Fatalf("InsertJournal(%s): %v", rid, err)
		}
	}
	journalCount := func(rid RunID) int64 {
		t.Helper()
		return s.count(t, &RunJournal{}, "run_id = ?", rid)
	}

	// 1. EndRun(complete)：与放闸/定稿同一笔事务。
	sid, rid := SessionID("s_a2_end"), RunID("r_a2_end")
	s.mustAcquire(t, sid, rid, "m_a2_end", "hi")
	mustJournal(rid)
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun = (ok=%v, err=%v), want ok", ok, err)
	}
	if n := journalCount(rid); n != 0 {
		t.Fatalf("EndRun 后账本 %d 行, want 0", n)
	}

	// 2. StopRun：状态翻转与删账本同事务。
	sid2, rid2 := SessionID("s_a2_stop"), RunID("r_a2_stop")
	s.mustAcquire(t, sid2, rid2, "m_a2_stop", "hi")
	mustJournal(rid2)
	if status, found, err := s.StopRun(ctx, rid2); err != nil || !found || status != "stopped" {
		t.Fatalf("StopRun = (%q, %v, %v), want stopped/true", status, found, err)
	}
	if n := journalCount(rid2); n != 0 {
		t.Fatalf("StopRun 后账本 %d 行, want 0", n)
	}

	// 3. AcquireRun 清扫：租约过期的死行被翻 error 时，同一 CTE 删账本。
	sid3, oldRID := SessionID("s_a2_sweep"), RunID("r_a2_old")
	s.mustAcquire(t, sid3, oldRID, "m_a2_old", "hi")
	mustJournal(oldRID)
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = ?`, oldRID).Error; err != nil {
		t.Fatalf("造死行: %v", err)
	}
	s.mustAcquire(t, sid3, RunID("r_a2_new"), "m_a2_new", "hi again")
	if n := journalCount(oldRID); n != 0 {
		t.Fatalf("清扫后旧 run 账本 %d 行, want 0", n)
	}
}

// TestReplayJournal 钉 A5 的重放四形状（doc/30 §9.1/§9.2）：正常配对、结果乱序（按 call id
// 归组、不按 seq 猜）、孤儿 intent 归组成合成 assistant、缺 result 回注逐字节固定的中断文本。
// 四形状都验行序/Seq 连续/RunID 归属，Turn 投影与 Rows 同步。
func TestReplayJournal(t *testing.T) {
	callsJSON := func(calls ...schema.ToolCall) string {
		t.Helper()
		b, err := json.Marshal(calls)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	c1 := schema.ToolCall{ID: "call_1", Type: "function",
		Function: schema.FunctionCall{Name: "bash", Arguments: `{"command":"echo one"}`}}
	c2 := schema.ToolCall{ID: "call_2", Type: "function",
		Function: schema.FunctionCall{Name: "get_time", Arguments: `{}`}}

	t.Run("正常配对", func(t *testing.T) {
		rows := []RunJournal{
			{Role: "assistant", Text: "先查一下", ToolCalls: callsJSON(c1), Model: "m1"},
			{Role: "tool_call", ToolCallID: "call_1", ToolName: "bash", Args: c1.Function.Arguments},
			{Role: "tool_result", ToolCallID: "call_1", ToolName: "bash", Text: "one"},
			{Role: "assistant", Text: "完成", Model: "m2"},
		}
		got, err := replayJournal("r_replay", rows)
		if err != nil {
			t.Fatalf("replayJournal: %v", err)
		}
		if !got.Final || got.Seed != "先查一下完成" {
			t.Fatalf("Final=%v Seed=%q, want true / 先查一下完成", got.Final, got.Seed)
		}
		wantRoles := []string{"assistant", "tool", "assistant"}
		if len(got.Rows) != len(wantRoles) || len(got.Turn) != len(wantRoles) {
			t.Fatalf("rows=%d turn=%d, want %d", len(got.Rows), len(got.Turn), len(wantRoles))
		}
		for i, role := range wantRoles {
			if got.Rows[i].Role != role || got.Rows[i].RunID != "r_replay" || got.Rows[i].Seq != i+2 {
				t.Fatalf("行 %d = %+v, want role=%s seq=%d runId=r_replay", i, got.Rows[i], role, i+2)
			}
		}
		if got.Turn[1].ToolCallID != "call_1" || got.Turn[1].Text != "one" || got.Turn[1].ToolName != "bash" || got.Turn[1].Model != "m1" {
			t.Fatalf("tool turn = %+v", got.Turn[1])
		}
		if got.Turn[2].Model != "m2" || got.Turn[2].Text != "完成" {
			t.Fatalf("终稿 turn = %+v", got.Turn[2])
		}
	})

	t.Run("结果乱序按 call id 归组", func(t *testing.T) {
		// 工具在 ToolsNode 里并行：result 落账顺序可以与 call 顺序相反；重放必须按
		// assistant 的 calls 顺序产出 tool 行（协议要求每个 tool_call 后跟它自己的 result）。
		rows := []RunJournal{
			{Role: "assistant", ToolCalls: callsJSON(c1, c2), Model: "m"},
			{Role: "tool_result", ToolCallID: "call_2", ToolName: "get_time", Text: "two"},
			{Role: "tool_call", ToolCallID: "call_2", ToolName: "get_time", Args: "{}"},
			{Role: "tool_call", ToolCallID: "call_1", ToolName: "bash", Args: c1.Function.Arguments},
			{Role: "tool_result", ToolCallID: "call_1", ToolName: "bash", Text: "one"},
		}
		got, err := replayJournal("r_replay", rows)
		if err != nil {
			t.Fatalf("replayJournal: %v", err)
		}
		if got.Final {
			t.Fatal("末行是 assistant with tool_calls，Final 应为 false")
		}
		if len(got.Turn) != 3 {
			t.Fatalf("turn = %d 条, want 3: %+v", len(got.Turn), got.Turn)
		}
		if got.Turn[1].ToolCallID != "call_1" || got.Turn[1].Text != "one" ||
			got.Turn[2].ToolCallID != "call_2" || got.Turn[2].Text != "two" {
			t.Fatalf("tool 顺序 = [%s=%q, %s=%q], want call_1=one, call_2=two",
				got.Turn[1].ToolCallID, got.Turn[1].Text, got.Turn[2].ToolCallID, got.Turn[2].Text)
		}
	})

	t.Run("孤儿 intent 归组合成 assistant", func(t *testing.T) {
		rows := []RunJournal{
			{Role: "tool_call", ToolCallID: "call_o", ToolName: "bash", Args: `{"command":"x"}`},
			{Role: "tool_result", ToolCallID: "call_o", ToolName: "bash", Text: "out"},
			{Role: "assistant", Text: "完", Model: "m"},
		}
		got, err := replayJournal("r_replay", rows)
		if err != nil {
			t.Fatalf("replayJournal: %v", err)
		}
		if !got.Final || got.Seed != "完" || len(got.Rows) != 3 {
			t.Fatalf("Final=%v Seed=%q rows=%d, want true / 完 / 3", got.Final, got.Seed, len(got.Rows))
		}
		syn := got.Rows[0]
		if syn.Role != "assistant" || len(syn.ToolCalls) != 1 ||
			syn.ToolCalls[0].ID != "call_o" || syn.ToolCalls[0].Function.Name != "bash" ||
			syn.ToolCalls[0].Function.Arguments != `{"command":"x"}` {
			t.Fatalf("合成 assistant = %+v, want 带 call_o 的 intent 原文", syn)
		}
		if got.Rows[1].Role != "tool" || got.Rows[1].Text != "out" || got.Rows[1].ToolCallID != "call_o" {
			t.Fatalf("孤儿 result = %+v", got.Rows[1])
		}
	})

	t.Run("缺 result 合成中断文本", func(t *testing.T) {
		rows := []RunJournal{
			{Role: "assistant", ToolCalls: callsJSON(c1), Model: "m"},
			{Role: "tool_call", ToolCallID: "call_1", ToolName: "bash", Args: c1.Function.Arguments},
		}
		got, err := replayJournal("r_replay", rows)
		if err != nil {
			t.Fatalf("缺 result 不许报错（不重跑，回注中断文本）: %v", err)
		}
		if got.Final || len(got.Rows) != 2 {
			t.Fatalf("Final=%v rows=%d, want false / 2（模型还要接着答）", got.Final, len(got.Rows))
		}
		res := got.Rows[1]
		if res.Role != "tool" || res.ToolCallID != "call_1" {
			t.Fatalf("合成 tool 行 = %+v", res)
		}
		if res.Text != interruptedResultText {
			t.Fatalf("合成文本 = %q, want 逐字节 %q", res.Text, interruptedResultText)
		}
		if got.Turn[1].ToolName != "bash" || got.Turn[1].Model != "m" {
			t.Fatalf("合成 tool turn = %+v, want 工具名与模型身份随行", got.Turn[1])
		}
	})
}
