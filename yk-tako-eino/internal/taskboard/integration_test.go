// 本文件：taskboard 的集成测试——假模型走真实 ChatModelAgent + Runner 图：
// 建表→更新→完成全链路、session 镜像（probe 工具）、interrupt 时看板随 checkpoint 落盘。
// 外部测试包（taskboard_test）：只用导出 API，假模型与探针都在本文件。
package taskboard_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/taskboard"
)

// init 注册测试 interrupt 的状态类型：checkpoint 是 gob 编码，
// 接口字段背后的具体类型必须注册（eino 注册 []byte 同款，adk/interrupt.go init）。
func init() {
	gob.Register("test-interrupt-state")
}

// scriptModel 是按剧本逐条吐消息的假模型：每次 Generate 弹出下一条预设消息。
//
// 实现接口：model.ToolCallingChatModel —— github.com/cloudwego/eino/components/model
// 包 interface.go:99（eino v0.9.21），覆盖 Generate / Stream / WithTools。
type scriptModel struct {
	mu      sync.Mutex
	replies []*schema.Message // 剧本：依次吐出的助手消息
}

// 编译期校验 scriptModel 实现了 model.ToolCallingChatModel。
var _ model.ToolCallingChatModel = (*scriptModel)(nil)

// Generate 实现 model.ToolCallingChatModel 的 Generate 方法：ChatModelAgent 每轮把
// 当前消息交给模型时回调它；本替身弹出剧本下一条当模型回复。
func (s *scriptModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.replies) == 0 {
		return nil, fmt.Errorf("剧本已用完")
	}
	msg := s.replies[0]
	s.replies = s.replies[1:]
	return msg, nil
}

// Stream 实现 model.ToolCallingChatModel 的 Stream 方法：接口要求的流式入口；
// 测试只走 Generate，这里不实现（panic 兜底）。
func (s *scriptModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not implemented")
}

// WithTools 实现 model.ToolCallingChatModel 的 WithTools 方法：框架调模型前用它把
// 工具绑定到模型上；本替身不关心绑定，返回自己——绑什么工具都按剧本吐。
func (s *scriptModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return s, nil
}

// toolCallMsg 造一条带单个工具调用的助手消息；id 保证工具回执可配对。
func toolCallMsg(id, name, args string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       id,
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// interruptMW 是测试专用 middleware：把 danger 工具调用暂停成 interrupt，
// 只为制造一次 checkpoint 落盘（生产上的 interrupt 来自 guard，这里不引 guard 保测试自包含）。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139（eino v0.9.21；:259 起别名）。内嵌基类（同文件 :277），覆盖 WrapInvokableToolCall。
type interruptMW struct {
	adk.BaseChatModelAgentMiddleware
}

// 编译期校验 interruptMW 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*interruptMW)(nil)

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall 钩子
// （github.com/cloudwego/eino/adk/handler.go:193）：框架在每次工具即将执行时调用它；
// 本替身借此让 danger 工具一律暂停成 interrupt，其余透传。
func (interruptMW) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if tCtx.Name == "danger" {
			return "", compose.StatefulInterrupt(ctx, "等审批", "test-interrupt-state")
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

// cpStore 是内存 checkpoint，只为验证 interrupt 落盘字节。
//
// 实现接口：adk.CheckPointStore —— github.com/cloudwego/eino/compose 包 checkpoint.go:53
// 的同名别名（eino v0.9.21），覆盖 Get / Set。
type cpStore struct {
	m map[string][]byte
}

// 编译期校验 cpStore 实现了 adk.CheckPointStore。
var _ adk.CheckPointStore = (*cpStore)(nil)

// Get 实现 compose.CheckPointStore 的 Get 方法：Runner 续跑时按 checkpointID 读图状态；
// 本替身直存直取，不拷贝（测试不关心字节隔离）。
func (s *cpStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := s.m[key]
	return v, ok, nil
}

// Set 实现 compose.CheckPointStore 的 Set 方法：图暂停落盘时按 checkpointID 写图状态；
// 测试只验证落盘字节里能找到看板镜像。
func (s *cpStore) Set(_ context.Context, key string, value []byte) error {
	s.m[key] = value
	return nil
}

// emptyArgs 是无参测试工具的占位参数。
type emptyArgs struct{}

// newBoardAgent 建测试 agent：假模型 + 给定工具 + 给定 middleware 链。
func newBoardAgent(t *testing.T, m model.ToolCallingChatModel, tools []tool.BaseTool, mws ...adk.ChatModelAgentMiddleware) adk.Agent {
	t.Helper()
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:          "yk-tako",
		Description:   "集成测试",
		Model:         m,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
		Handlers:      mws,
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatalf("建 agent: %v", err)
	}
	return agent
}

// drain 收完事件流，有错误事件立即失败。
func drain(t *testing.T, iter *adk.AsyncIterator[*adk.AgentEvent]) []*adk.AgentEvent {
	t.Helper()
	var events []*adk.AgentEvent
	for {
		ev, ok := iter.Next()
		if !ok {
			return events
		}
		if ev.Err != nil {
			t.Fatalf("事件流出错: %v", ev.Err)
		}
		events = append(events, ev)
	}
}

// TestBoardFlowEndToEnd 验证全链路：模型建表 → 标完成 → 收工，看板正本状态正确。
func TestBoardFlowEndToEnd(t *testing.T) {
	store := taskboard.NewStore()
	mw, err := taskboard.New(store)
	if err != nil {
		t.Fatalf("taskboard.New: %v", err)
	}
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("c1", taskboard.ToolCreate, `{"subject":"查时间","activeForm":"查时间"}`),
		toolCallMsg("c2", taskboard.ToolUpdate, `{"taskId":"1","status":"completed"}`),
		schema.AssistantMessage("全部完成", nil),
	}}
	agent := newBoardAgent(t, m, nil, mw)
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent})

	events := drain(t, runner.Run(context.Background(), []*schema.Message{schema.UserMessage("先查时间再总结")}))

	task, err := store.Get("1")
	if err != nil || task.Status != taskboard.StatusCompleted {
		t.Fatalf("看板 #1 应 completed: %+v err=%v", task, err)
	}
	last := events[len(events)-1]
	msg, err := last.Output.MessageOutput.GetMessage()
	if err != nil || msg.Content != "全部完成" {
		t.Fatalf("最后事件应是最终答复: %+v err=%v", last, err)
	}
}

// TestSessionMirrorViaProbe 验证 TaskCreate 执行后 session 里就有看板镜像
// （probe 工具在执行时把 session 值抄进闭包）。
func TestSessionMirrorViaProbe(t *testing.T) {
	var captured map[string]any
	probe, err := utils.InferTool("probe_session", "测试探针：把当前 session 值抄到闭包",
		func(ctx context.Context, _ emptyArgs) (string, error) {
			captured = adk.GetSessionValues(ctx)
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("建 probe: %v", err)
	}
	store := taskboard.NewStore()
	mw, err := taskboard.New(store)
	if err != nil {
		t.Fatalf("taskboard.New: %v", err)
	}
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("c1", taskboard.ToolCreate, `{"subject":"查时间"}`),
		toolCallMsg("c2", "probe_session", `{}`),
		schema.AssistantMessage("完成", nil),
	}}
	agent := newBoardAgent(t, m, []tool.BaseTool{probe}, mw)
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent})
	drain(t, runner.Run(context.Background(), []*schema.Message{schema.UserMessage("干活")}))

	snap, ok := captured[taskboard.SessionKey].([]taskboard.Task)
	if !ok || len(snap) != 1 || snap[0].Subject != "查时间" {
		t.Fatalf("session 镜像应是 1 条「查时间」: %+v", captured[taskboard.SessionKey])
	}
}

// TestInterruptCheckpointCarriesBoard 验证 interrupt 时看板镜像随图状态落盘：
// checkpoint 字节里能直接找到任务主题（gob 线格式里字符串原样存放）。
func TestInterruptCheckpointCarriesBoard(t *testing.T) {
	danger, err := utils.InferTool("danger", "测试用：会被测试 middleware 暂停",
		func(context.Context, emptyArgs) (string, error) { return "不应执行到这里", nil })
	if err != nil {
		t.Fatalf("建 danger: %v", err)
	}
	store := taskboard.NewStore()
	mw, err := taskboard.New(store)
	if err != nil {
		t.Fatalf("taskboard.New: %v", err)
	}
	m := &scriptModel{replies: []*schema.Message{
		toolCallMsg("c1", taskboard.ToolCreate, `{"subject":"查时间"}`),
		toolCallMsg("c2", "danger", `{}`),
	}}
	agent := newBoardAgent(t, m, []tool.BaseTool{danger}, mw, &interruptMW{})
	cp := &cpStore{m: map[string][]byte{}}
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent, CheckPointStore: cp})

	events := drain(t, runner.Run(context.Background(), []*schema.Message{schema.UserMessage("干活")}, adk.WithCheckPointID("cp-1")))

	last := events[len(events)-1]
	if last.Action == nil || last.Action.Interrupted == nil {
		t.Fatalf("应以 interrupt 收尾: %+v", last)
	}
	data, ok, err := cp.Get(context.Background(), "cp-1")
	if err != nil || !ok {
		t.Fatalf("checkpoint 应已落盘: ok=%v err=%v", ok, err)
	}
	if !bytes.Contains(data, []byte("查时间")) {
		t.Fatalf("checkpoint 字节里应能找到任务主题（session 镜像随图落盘），字节长 %d", len(data))
	}
	// 顺带确认 drain 过程中事件流里出现过看板回执（投影的数据源）
	found := false
	for _, ev := range events {
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			if msg, err := ev.Output.MessageOutput.GetMessage(); err == nil && msg != nil && strings.Contains(msg.Content, "📋 任务看板") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("事件流里应出现过看板回执")
	}
}
