package planning

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// TestFormatTodos 验证清单和回执的排版，以及空清单的提示。
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

// TestShouldNudge 验证半途而废提醒的各种触发和不该触发的边界。
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

// scriptModel 是按脚本回消息的假模型：每次 Generate 弹出下一条，弹空报错。
// 并行防守的标记存在 run-local 状态里，脱离真实运行造不出带运行状态的 ctx，
// 所以这几个测试都走完整 runner，不再直接调钩子。
type scriptModel struct {
	msgs []*schema.Message
}

// Generate 弹出脚本里的下一条消息。
func (m *scriptModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	if len(m.msgs) == 0 {
		return nil, errors.New("脚本用完了，模型不该再被调用")
	}
	msg := m.msgs[0]
	m.msgs = m.msgs[1:]
	return msg, nil
}

// Stream 不支持：测试都跑非流式。
func (m *scriptModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("测试不开流式")
}

// WithTools 直接返回自己：假模型不真的绑定工具。
func (m *scriptModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// runScript 用假模型跑一轮完整 agent，返回这轮产生的全部消息。
// extra 是挂工具节点的额外工具；planning 的两个工具由 BeforeAgent 注入。
func runScript(t *testing.T, kit *Planning, extra []tool.BaseTool, script ...*schema.Message) []*schema.Message {
	t.Helper()
	ctx := context.Background()
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:  "test",
		Model: &scriptModel{msgs: script},
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: extra},
		},
		Handlers:      []adk.ChatModelAgentMiddleware{kit.Handler},
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("开始")})
	var msgs []*schema.Message
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, err := event.Output.MessageOutput.GetMessage()
		if err != nil {
			t.Fatal(err)
		}
		if msg != nil && msg.ToolName == "" {
			msg.ToolName = event.Output.MessageOutput.ToolName
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// toolReplies 收集某个工具在这轮里的全部回执正文。
func toolReplies(t *testing.T, msgs []*schema.Message, toolName string) []string {
	t.Helper()
	var out []string
	for _, msg := range msgs {
		if msg != nil && msg.Role == schema.Tool && msg.ToolName == toolName {
			out = append(out, msg.Content)
		}
	}
	if len(out) == 0 {
		t.Fatalf("没有 %s 的工具回执", toolName)
	}
	return out
}

// writeCall 造一次 write_todos 调用，参数是一条 in_progress 的合法清单。
func writeCall(id string) schema.ToolCall {
	return schema.ToolCall{
		ID:   id,
		Type: "function",
		Function: schema.FunctionCall{
			Name:      WriteTodosTool,
			Arguments: `{"todos":[{"content":"查时间","status":"in_progress"}]}`,
		},
	}
}

// 同轮两次 write_todos：两次都回并行错误，清单不动。
func TestParallelWriteDoesNotReplace(t *testing.T) {
	kit, err := New(Options{DisableNudge: true})
	if err != nil {
		t.Fatal(err)
	}
	msgs := runScript(t, kit, nil,
		schema.AssistantMessage("", []schema.ToolCall{writeCall("1"), writeCall("2")}),
		schema.AssistantMessage("收工", nil),
	)
	for _, got := range toolReplies(t, msgs, WriteTodosTool) {
		if !strings.HasPrefix(got, "错误：write_todos 必须独占一轮") {
			t.Fatalf("parallel: %q", got)
		}
	}
	if len(kit.List.Snapshot()) != 0 {
		t.Fatal("并行调用不该改清单")
	}
}

// 拒绝标记只管当轮：并行轮被拒后，下一轮独占的 write_todos 照常生效。
func TestParallelRejectResetsNextRound(t *testing.T) {
	kit, err := New(Options{DisableNudge: true})
	if err != nil {
		t.Fatal(err)
	}
	msgs := runScript(t, kit, nil,
		schema.AssistantMessage("", []schema.ToolCall{writeCall("1"), writeCall("2")}),
		schema.AssistantMessage("", []schema.ToolCall{writeCall("3")}),
		schema.AssistantMessage("收工", nil),
	)
	replies := toolReplies(t, msgs, WriteTodosTool)
	if len(replies) != 3 {
		t.Fatalf("回执数: %d", len(replies))
	}
	for _, got := range replies[:2] {
		if !strings.HasPrefix(got, "错误：write_todos 必须独占一轮") {
			t.Fatalf("第一轮该被拒: %q", got)
		}
	}
	if strings.HasPrefix(replies[2], "错误") {
		t.Fatalf("下一轮独占调用应成功: %q", replies[2])
	}
	got := kit.List.Snapshot()
	if len(got) != 1 || got[0].Status != StatusInProgress {
		t.Fatalf("list: %+v", got)
	}
}

// write_todos 与干活工具同轮混排：write_todos 回错误、清单不动，同轮其它工具不受影响。
func TestMixedTurnWriteRejected(t *testing.T) {
	kit, err := New(Options{DisableNudge: true})
	if err != nil {
		t.Fatal(err)
	}
	clock, err := utils.InferTool("get_current_time", "返回固定时间，测试混排用",
		func(context.Context, emptyArgs) (string, error) { return "12:00", nil })
	if err != nil {
		t.Fatal(err)
	}
	msgs := runScript(t, kit, []tool.BaseTool{clock},
		schema.AssistantMessage("", []schema.ToolCall{
			writeCall("1"),
			{ID: "2", Type: "function", Function: schema.FunctionCall{Name: "get_current_time", Arguments: "{}"}},
		}),
		schema.AssistantMessage("收工", nil),
	)
	if got := toolReplies(t, msgs, WriteTodosTool)[0]; !strings.HasPrefix(got, "错误：write_todos 必须独占一轮") {
		t.Fatalf("mixed turn: %q", got)
	}
	if got := toolReplies(t, msgs, "get_current_time")[0]; got != "12:00" {
		t.Fatalf("同轮其它工具照常执行: %q", got)
	}
	if len(kit.List.Snapshot()) != 0 {
		t.Fatal("混排轮不该改清单")
	}
}

// write_todos 独占一轮时不触发防守，清单正常更新；非法 status 不替换清单。
// 直接调工具时 ctx 里没有运行状态，标记按未设置读，正好覆盖这条路径。
func TestSoloWriteAndValidation(t *testing.T) {
	kit, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeTool := kit.Tools[0].(tool.InvokableTool)
	out, err := writeTool.InvokableRun(context.Background(), `{"todos":[{"content":"查时间","status":"in_progress"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "错误") {
		t.Fatalf("独占一轮应成功: %q", out)
	}
	if got := kit.List.Snapshot(); len(got) != 1 || got[0].Status != StatusInProgress {
		t.Fatalf("独占一轮应更新清单: %+v", got)
	}

	bad, err := writeTool.InvokableRun(context.Background(), `{"todos":[{"content":"x","status":"doing"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bad, "错误：") || len(kit.List.Snapshot()) != 1 {
		t.Fatalf("非法 status 不该替换清单: %q %+v", bad, kit.List.Snapshot())
	}
}

// TestNudgeAttachesHiddenToolCall 验证提醒会补一次隐藏工具调用，且模型看不见这个工具、同轮不重复催。
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

// TestListParsing 验证从回执里反解清单（图标行和状态行两种形状）。
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
