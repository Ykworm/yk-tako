package planning

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func TestFormatTodos(t *testing.T) {
	if got := FormatTodos(nil); got != "（清单为空）" {
		t.Fatalf("empty: %q", got)
	}
	got := FormatTodos([]Todo{
		{Content: "查时间", Status: StatusInProgress},
		{Content: "做计算", Status: StatusPending},
		{Content: "写总结", Status: StatusCompleted},
	})
	want := "🔨 1. 查时间\n⬜ 2. 做计算\n✅ 3. 写总结"
	if got != want {
		t.Fatalf("got %q", got)
	}
	receipt := Receipt([]Todo{
		{Content: "查时间", Status: StatusCompleted},
		{Content: "做计算", Status: StatusInProgress},
	})
	if !strings.Contains(receipt, "1/2 已完成") {
		t.Fatalf("receipt: %q", receipt)
	}
}

func TestShouldNudge(t *testing.T) {
	wrote := []*schema.Message{
		schema.UserMessage("帮我规划一下"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "w", Function: schema.FunctionCall{Name: WriteTodosTool}}}),
		schema.ToolMessage(Receipt([]Todo{{Content: "查时间", Status: StatusInProgress}}), "w", schema.WithToolName(WriteTodosTool)),
		schema.AssistantMessage("先这样吧", nil),
	}
	inProgress := []Todo{{Content: "查时间", Status: StatusInProgress}, {Content: "总结", Status: StatusPending}}
	if !ShouldNudge(wrote, inProgress, true) {
		t.Fatal("本轮写过清单且有 in_progress，应该催")
	}

	pendingOnly := []Todo{{Content: "查时间", Status: StatusPending}}
	if ShouldNudge(wrote, pendingOnly, true) {
		t.Fatal("纯 pending 不催")
	}
	done := []Todo{{Content: "查时间", Status: StatusCompleted}}
	if ShouldNudge(wrote, done, true) {
		t.Fatal("全部完成不催")
	}
	if ShouldNudge(wrote, inProgress, false) {
		t.Fatal("关掉提醒时不催")
	}

	withCall := append(append([]*schema.Message{}, wrote[:len(wrote)-1]...),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "t", Function: schema.FunctionCall{Name: "get_current_time"}}}))
	if ShouldNudge(withCall, inProgress, true) {
		t.Fatal("还有待执行工具时不催")
	}

	leftover := []*schema.Message{
		schema.UserMessage("上一轮"),
		schema.ToolMessage(Receipt(inProgress), "old", schema.WithToolName(WriteTodosTool)),
		schema.AssistantMessage("上次做到这", nil),
		schema.UserMessage("现在几点"),
		schema.AssistantMessage("我去查", nil),
	}
	if ShouldNudge(leftover, inProgress, true) {
		t.Fatal("上一轮留下的 in_progress 不劫持新话题")
	}

	nudged := append(append([]*schema.Message{}, wrote...),
		schema.ToolMessage(NudgeText(inProgress), "n", schema.WithToolName(NudgeTool)),
		schema.AssistantMessage("还是想收工", nil),
	)
	if ShouldNudge(nudged, inProgress, true) {
		t.Fatal("同一轮已经催过，不再催")
	}

	prefixOnly := schema.UserMessage(NudgePrefix + " 还有 1 项")
	if !IsPlanningNudge(prefixOnly) {
		t.Fatal("没有 name 时，正文前缀也要认得出提醒")
	}
	if !AlreadyNudgedThisTurn([]*schema.Message{schema.UserMessage("hi"), prefixOnly}) {
		t.Fatal("前缀提醒算本轮已催")
	}

	failed := []*schema.Message{
		schema.UserMessage("再试一次"),
		schema.ToolMessage(parallelError, "e", schema.WithToolName(WriteTodosTool)),
		schema.AssistantMessage("收工", nil),
	}
	if WroteTodosThisTurn(failed) || ShouldNudge(failed, inProgress, true) {
		t.Fatal("错误回执不算成功写过清单")
	}
}

func TestParallelWriteDoesNotReplace(t *testing.T) {
	kit, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "1", Type: "function", Function: schema.FunctionCall{Name: WriteTodosTool, Arguments: "{}"}},
			{ID: "2", Type: "function", Function: schema.FunctionCall{Name: WriteTodosTool, Arguments: "{}"}},
		}),
	}}
	if _, _, err := kit.Handler.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if len(state.Messages[0].ToolCalls) != 2 {
		t.Fatal("并行防守不该改掉原有 tool call")
	}
	writeTool := kit.Middleware.AdditionalTools[0].(tool.InvokableTool)
	out, err := writeTool.InvokableRun(context.Background(), `{"todos":[{"content":"查时间","status":"in_progress"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "错误：write_todos 每轮只能调用一次") {
		t.Fatalf("parallel: %q", out)
	}
	if len(kit.List.Snapshot()) != 0 {
		t.Fatal("并行调用不该改清单")
	}

	if _, _, err := kit.Handler.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{}, nil); err != nil {
		t.Fatal(err)
	}
	out, err = writeTool.InvokableRun(context.Background(), `{"todos":[{"content":"查时间","status":"in_progress"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "错误") {
		t.Fatalf("下一轮单次调用应成功: %q", out)
	}
	got := kit.List.Snapshot()
	if len(got) != 1 || got[0].Status != StatusInProgress {
		t.Fatalf("list: %+v", got)
	}

	bad, err := writeTool.InvokableRun(context.Background(), `{"todos":[{"content":"x","status":"doing"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bad, "错误：") || len(kit.List.Snapshot()) != 1 {
		t.Fatalf("非法 status 不该替换清单: %q %+v", bad, kit.List.Snapshot())
	}
}

func TestNudgeAttachesHiddenToolCall(t *testing.T) {
	kit, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	kit.List.Replace([]Todo{{Content: "查时间", Status: StatusInProgress}})
	finish := schema.AssistantMessage("我先总结到这", nil)
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("帮我规划一下"),
		schema.ToolMessage("任务清单已更新（0/1 已完成）", "w", schema.WithToolName(WriteTodosTool)),
		finish,
	}}
	if _, _, err := kit.Handler.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if len(finish.ToolCalls) != 1 || finish.ToolCalls[0].Function.Name != NudgeTool {
		t.Fatalf("calls: %+v", finish.ToolCalls)
	}
	if finish.Content != "我先总结到这" {
		t.Fatal("催的时候保留模型原文")
	}

	again := schema.AssistantMessage("还想停", nil)
	state.Messages = append(state.Messages,
		schema.ToolMessage(NudgePrefix+" 还有 1 项", finish.ToolCalls[0].ID, schema.WithToolName(NudgeTool)),
		again,
	)
	if _, _, err := kit.Handler.AfterModelRewriteState(context.Background(), state, nil); err != nil {
		t.Fatal(err)
	}
	if len(again.ToolCalls) != 0 {
		t.Fatal("同一轮第二次不该再催")
	}

	visible := &adk.ChatModelAgentState{ToolInfos: []*schema.ToolInfo{
		{Name: "calculate"},
		{Name: NudgeTool},
		{Name: WriteTodosTool},
	}}
	if _, visible, err = kit.Handler.BeforeModelRewriteState(context.Background(), visible, nil); err != nil {
		t.Fatal(err)
	}
	for _, info := range visible.ToolInfos {
		if info.Name == NudgeTool {
			t.Fatal("模型不该看见 planning_nudge")
		}
	}
	if len(visible.ToolInfos) != 2 {
		t.Fatalf("tools: %+v", visible.ToolInfos)
	}

	off, err := New(Options{DisableNudge: true})
	if err != nil {
		t.Fatal(err)
	}
	off.List.Replace([]Todo{{Content: "查时间", Status: StatusInProgress}})
	quiet := schema.AssistantMessage("收工", nil)
	quietState := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("规划"),
		schema.ToolMessage("ok", "w", schema.WithToolName(WriteTodosTool)),
		quiet,
	}}
	if _, _, err := off.Handler.AfterModelRewriteState(context.Background(), quietState, nil); err != nil {
		t.Fatal(err)
	}
	if len(quiet.ToolCalls) != 0 {
		t.Fatal("关掉提醒时不该补工具调用")
	}
}

func TestListParsing(t *testing.T) {
	icons, ok := IconList(Receipt([]Todo{{Content: "查时间", Status: StatusPending}}))
	if !ok || icons != "⬜ 1. 查时间" {
		t.Fatalf("icons: %v %q", ok, icons)
	}
	raw := `Updated todo list to [{"content":"查时间","status":"in_progress"},{"content":"计算","status":"pending"}]`
	line, ok := StatusList(raw)
	if !ok || line != "[in_progress] 查时间 | [pending] 计算" {
		t.Fatalf("status: %v %q", ok, line)
	}
}
