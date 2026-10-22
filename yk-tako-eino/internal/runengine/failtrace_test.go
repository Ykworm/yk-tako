// failtrace_test.go 是「失败留痕」的钉子网（方案真源 = tmp/失败留痕/planning.md）：
// 失败终态（error / stopped）不再把这一轮抹掉——已说出的半截回答与已执行的工具调用照常落库，
// 页面刷新后仍在，模型的下一枪也接得上；而「失败且无产出」的那一档仍然一行不写。
package runengine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestEndRunErrorLeavesTrace：error 终态 + 有产出 → runlog 有 assistant 行与工具行、mh 有行。
func TestEndRunErrorLeavesTrace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "帮我看下 X")
	s.mustDelta(t, ctx, rid, "半截回答")

	turn := []TurnEntry{
		{Role: "assistant", Text: "半截回答", Model: "deepseek/flash",
			ToolCalls: []schema.ToolCall{{ID: "call_1", Type: "function",
				Function: schema.FunctionCall{Name: "bash", Arguments: "{}"}}}},
		{Role: "tool", Text: "命令输出", ToolCallID: "call_1", ToolName: "bash"},
	}
	mhRows := balanceToolResults(mhRowsFromTurn(rid, turn))
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "error", nil, mhRows, turn); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	// runlog：user 行 + tool_call 行 + tool_result 行 + assistant 半截行 = 4
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 4 {
		t.Fatalf("runlog 行数 = %d, want 4（user/tool_call/tool_result/assistant）", got)
	}
	var half string
	if err := s.db.Raw(`SELECT text FROM runlog_entries WHERE run_id = ? AND role = 'assistant'`, rid).Row().Scan(&half); err != nil {
		t.Fatal(err)
	}
	if half != "半截回答" {
		t.Fatalf("runlog 的 assistant 行 = %q, want %q", half, "半截回答")
	}
	// mh：user 行(seq=1) + assistant(seq=2) + tool(seq=3)
	if got := s.count(t, &RunModelMessage{}, "run_id = ?", rid); got != 3 {
		t.Fatalf("mh 行数 = %d, want 3（user/assistant/tool）", got)
	}
	if !s.sqlBool(t, `SELECT (text = '半截回答') FROM run_model_messages WHERE run_id = ? AND seq = 2`, rid) {
		t.Error("mh seq2 应是半截回答")
	}
	// 记忆不放开：失败轮不登记抽取任务
	if got := s.count(t, &MemoryJob{}, "run_id = ?", rid); got != 0 {
		t.Fatalf("失败轮不该登记记忆抽取任务，却有 %d 条", got)
	}
}

// TestEndRunErrorWithoutOutputWritesNothing：失败且无产出 → 一行不写（与改造前同形）。
func TestEndRunErrorWithoutOutputWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")

	// turn 空 = 还没说出话就失败
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "error", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 1 {
		t.Fatalf("runlog 行数 = %d, want 1（只有 user 行）", got)
	}
	if got := s.count(t, &RunModelMessage{}, "run_id = ?", rid); got != 1 {
		t.Fatalf("mh 行数 = %d, want 1（只有 user 行）", got)
	}
}

// TestEndRunStoppedLeavesTrace：stopped 终态与 error 同待遇（都是「这一轮做了一半」）。
func TestEndRunStoppedLeavesTrace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "说到一半")

	turn := []TurnEntry{{Role: "assistant", Text: "说到一半", Model: "deepseek/flash"}}
	mhRows := balanceToolResults(mhRowsFromTurn(rid, turn))
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "stopped", nil, mhRows, turn); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 2 {
		t.Fatalf("runlog 行数 = %d, want 2（user + assistant 半截）", got)
	}
	if !s.sqlBool(t, `SELECT (text = '说到一半') FROM run_model_messages WHERE run_id = ? AND seq = 2`, rid) {
		t.Error("stopped 轮已说出的半截应落 mh")
	}
}

// TestBalanceToolResultsFillsMissingToolRow：assistant 带 tool_calls 但没有对应 tool 行
// → 补一条「没拿到结果」的合成行（否则下一枪的 provider 会因 tool_calls 缺配对结果而 400）。
func TestBalanceToolResultsFillsMissingToolRow(t *testing.T) {
	rows := []ModelMessage{
		{RunID: "r_1", Seq: 2, Role: "assistant", Text: "我去跑一下",
			ToolCalls: []schema.ToolCall{{ID: "call_x", Type: "function",
				Function: schema.FunctionCall{Name: "bash", Arguments: "{}"}}}},
	}
	out := balanceToolResults(rows)
	if len(out) != 2 {
		t.Fatalf("配平后行数 = %d, want 2（assistant + 合成 tool 行）", len(out))
	}
	if out[1].Role != "tool" || out[1].ToolCallID != "call_x" {
		t.Fatalf("第二行 = %+v, want tool 行（call_x）", out[1])
	}
	if !strings.Contains(out[1].Text, "没有拿到结果") {
		t.Fatalf("合成行文案 = %q, want 含「没有拿到结果」", out[1].Text)
	}
	// seq 要连续（mh 是 (run_id, seq) 唯一）
	if out[0].Seq != 2 || out[1].Seq != 3 {
		t.Fatalf("seq = %d,%d，want 2,3", out[0].Seq, out[1].Seq)
	}
	// 已有配对结果时不得补
	rows2 := []ModelMessage{
		{RunID: "r_1", Seq: 2, Role: "assistant", Text: "x",
			ToolCalls: []schema.ToolCall{{ID: "call_x", Type: "function",
				Function: schema.FunctionCall{Name: "bash", Arguments: "{}"}}}},
		{RunID: "r_1", Seq: 3, Role: "tool", Text: "结果", ToolCallID: "call_x"},
	}
	if out2 := balanceToolResults(rows2); len(out2) != 2 {
		t.Fatalf("已有配对结果时不该补行，却有 %d 行", len(out2))
	}
}
