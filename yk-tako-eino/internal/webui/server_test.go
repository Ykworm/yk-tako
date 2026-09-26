// 本文件：webui 的端到端测试——假模型走真实 ChatModelAgent 图，httptest 起服务，
// 按 SSE 帧断言事件序列与冲突检查；审批用例在 Task 2 追加。
package webui

import (
	"bufio"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/taskboard"
)

// scriptModel 是按剧本逐条吐消息的假模型（手法同 taskboard 集成测试，包内自带一份）。
//
// 实现接口：model.ToolCallingChatModel —— github.com/cloudwego/eino/components/model
// 包 interface.go:99（eino v0.9.21），覆盖 Generate / Stream / WithTools。
type scriptModel struct {
	mu      sync.Mutex
	replies []*schema.Message
}

// 编译期校验 scriptModel 实现了 model.ToolCallingChatModel。
var _ model.ToolCallingChatModel = (*scriptModel)(nil)

// Generate 实现 model.ToolCallingChatModel 的 Generate：每次调用弹出一条剧本消息。
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

// Stream 实现 model.ToolCallingChatModel 的 Stream：测试只走 Generate（panic 兜底）。
func (s *scriptModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not implemented")
}

// WithTools 实现 model.ToolCallingChatModel 的 WithTools：测试不关心工具绑定，原样返回。
func (s *scriptModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return s, nil
}

// blockingModel 首轮 Generate 卡住不返回，用来制造「同会话已有 run 在跑」的 409 场景。
//
// 实现接口：model.ToolCallingChatModel（同上）。
type blockingModel struct {
	started chan struct{} // 第一次进入 Generate 即关闭，测试据此确认 run 已开跑
	release chan struct{} // 关闭后 Generate 才返回
	once    sync.Once
}

// 编译期校验 blockingModel 实现了 model.ToolCallingChatModel。
var _ model.ToolCallingChatModel = (*blockingModel)(nil)

// Generate 实现 model.ToolCallingChatModel 的 Generate：首次调用关 started，再等 release。
func (m *blockingModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.once.Do(func() { close(m.started) })
	<-m.release
	return schema.AssistantMessage("完成", nil), nil
}

// Stream 实现 model.ToolCallingChatModel 的 Stream：测试只走 Generate（panic 兜底）。
func (m *blockingModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not implemented")
}

// WithTools 实现 model.ToolCallingChatModel 的 WithTools：原样返回自己。
func (m *blockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
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

// newTestAgent 建测试 agent：假模型 + 给定工具 + 给定 middleware 链，走真实图。
func newTestAgent(t *testing.T, m model.ToolCallingChatModel, tools []tool.BaseTool, mws ...adk.ChatModelAgentMiddleware) adk.Agent {
	t.Helper()
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:          "yk-tako",
		Description:   "webui 测试",
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

// sseReader 把响应体按帧解析成 sseEvent：只认 data: 行，帧以空行分隔。
type sseReader struct{ sc *bufio.Scanner }

// newSSEReader 包一个流读取器。
func newSSEReader(body io.Reader) *sseReader { return &sseReader{sc: bufio.NewScanner(body)} }

// next 读下一帧；流结束返回 ok=false。
func (r *sseReader) next(t *testing.T) (sseEvent, bool) {
	t.Helper()
	for r.sc.Scan() {
		data, ok := strings.CutPrefix(r.sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("SSE 载荷不是 JSON: %q err=%v", data, err)
		}
		return ev, true
	}
	if err := r.sc.Err(); err != nil {
		t.Fatalf("读 SSE: %v", err)
	}
	return sseEvent{}, false
}

// postChat 发一条聊天请求，返回响应（调用方负责 Close）。
func postChat(t *testing.T, base, sessionID, message string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"sessionId":%q,"message":%q}`, sessionID, message)
	resp, err := http.Post(base+"/api/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	return resp
}

// TestChatSSESequence 验证一轮的事件序列（《10》§6 关口 3）：
// 假模型建表 → 看板回执 → 收尾，SSE 帧依此为 tool_call / board / message。
func TestChatSSESequence(t *testing.T) {
	ctx := context.Background()
	replyModel := &scriptModel{replies: []*schema.Message{
		toolCallMsg("c1", taskboard.ToolCreate, `{"subject":"查时间"}`),
		schema.AssistantMessage("收尾", nil),
	}}
	boardMW, err := taskboard.New(taskboard.NewStore())
	if err != nil {
		t.Fatalf("taskboard.New: %v", err)
	}
	srv := New(ctx, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, replyModel, nil, boardMW), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postChat(t, ts.URL, "s1", "先建个任务再收尾")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，want text/event-stream", ct)
	}

	r := newSSEReader(resp.Body)
	var got []sseEvent
	for {
		ev, ok := r.next(t)
		if !ok {
			break
		}
		got = append(got, ev)
	}
	if len(got) != 3 || got[0].Kind != "tool_call" || got[1].Kind != "board" || got[2].Kind != "message" {
		t.Fatalf("事件序列 = %+v，want [tool_call board message]", got)
	}
	if len(got[0].ToolNames) != 1 || got[0].ToolNames[0] != taskboard.ToolCreate {
		t.Fatalf("tool_call 载荷 = %+v", got[0])
	}
	if !strings.Contains(got[1].Text, "查时间") {
		t.Fatalf("board 原文应含任务主题，实际 %q", got[1].Text)
	}
	if got[2].Text != "收尾" {
		t.Fatalf("收尾事件 = %+v", got[2])
	}
}

// TestChatConflict409 验证同一会话并发两轮：第二轮在写 SSE header 之前拿到 409。
func TestChatConflict409(t *testing.T) {
	ctx := context.Background()
	bm := &blockingModel{started: make(chan struct{}), release: make(chan struct{})}
	srv := New(ctx, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	firstCh := make(chan *http.Response, 1)
	firstErr := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/api/chat", "application/json",
			strings.NewReader(`{"sessionId":"s1","message":"跑着"}`))
		if err != nil {
			firstErr <- err
			return
		}
		firstCh <- resp
	}()
	<-bm.started // run 已开跑：active 已置位、响应 header 尚未写（模型没返回）

	resp2 := postChat(t, ts.URL, "s1", "插队")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("第二轮状态码 = %d，want 409", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("409 不应是 SSE 响应，Content-Type = %q", ct)
	}

	close(bm.release)
	var resp1 *http.Response
	select {
	case err := <-firstErr:
		t.Fatalf("第一轮请求失败: %v", err)
	case resp1 = <-firstCh:
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("第一轮状态码 = %d，want 200", resp1.StatusCode)
	}
	r := newSSEReader(resp1.Body)
	for {
		if _, ok := r.next(t); !ok {
			break
		}
	}
}

// init 注册测试 interrupt 的状态类型：checkpoint 是 gob 编码，接口字段背后的
// 具体类型必须注册（手法同 taskboard 集成测试的 gob.Register）。
func init() { gob.Register("webui-test-interrupt-state") }

// interruptMW 是测试专用 middleware：把 danger 工具调用暂停成 interrupt，
// 恢复时按决策执行或回绝。生产上的 interrupt 来自 guard 引擎，这里不引引擎保测试自包含，
// 但暂停载荷用 guard.ApprovalInfo、resume 值用 guard.ResumeApprove/ResumeReject——
// 审批链路两端（ask 产出、POST 回执）的类型与常量与生产完全一致。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139（eino v0.9.21；:259 起别名）。内嵌基类（同文件 :277），覆盖 WrapInvokableToolCall。
type interruptMW struct {
	adk.BaseChatModelAgentMiddleware
}

// 编译期校验 interruptMW 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*interruptMW)(nil)

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall 钩子
// （github.com/cloudwego/eino/adk/handler.go:193）：danger 一律先暂停；恢复时只认
// 冲自己来的决定（approve 才执行，reject 回拒绝文案），不是冲自己来的恢复信号重新暂停传递。
func (interruptMW) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if tCtx.Name != "danger" {
			return endpoint(ctx, args, opts...)
		}
		pause := func() error {
			return compose.StatefulInterrupt(ctx, guard.ApprovalInfo{
				Kind: "test_approval", Tool: tCtx.Name, Args: args, Message: "测试审批：确认执行 danger",
			}, "webui-test-interrupt-state")
		}
		if was, has, _ := compose.GetInterruptState[string](ctx); was && has {
			if isTarget, hasData, decision := compose.GetResumeContext[string](ctx); isTarget && hasData {
				if decision == guard.ResumeApprove {
					return endpoint(ctx, args, opts...)
				}
				return "用户拒绝了这次 danger 调用。", nil
			}
			return "", pause() // 不是冲自己来的恢复：重新暂停，把信号传给目标
		}
		return "", pause()
	}, nil
}

// TestApprovalRoundTrip 验证审批回路端到端（《10》§6 关口 4）：
// tool_call → approval 事件 → POST /api/approval → 按决定继续；批准/拒绝各验一遍。
func TestApprovalRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		decision   string
		wantResult string
	}{
		{"批准后执行工具", guard.ResumeApprove, "执行完毕"},
		{"拒绝后不执行", guard.ResumeReject, "用户拒绝了这次 danger 调用。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			danger, err := utils.InferTool("danger", "测试：会被 interruptMW 暂停的工具",
				func(context.Context, struct{}) (string, error) { return "执行完毕", nil })
			if err != nil {
				t.Fatalf("建 danger: %v", err)
			}
			m := &scriptModel{replies: []*schema.Message{
				toolCallMsg("c1", "danger", `{}`),
				schema.AssistantMessage("收尾", nil),
			}}
			srv := New(ctx, Config{NewAgent: func(context.Context) (adk.Agent, error) {
				return newTestAgent(t, m, []tool.BaseTool{danger}, &interruptMW{}), nil
			}})
			ts := httptest.NewServer(srv)
			defer ts.Close()

			resp := postChat(t, ts.URL, "s1", "跑个危险工具")
			defer resp.Body.Close()
			r := newSSEReader(resp.Body)

			ev, ok := r.next(t)
			if !ok || ev.Kind != "tool_call" {
				t.Fatalf("第一帧应是 tool_call，实际 %+v ok=%v", ev, ok)
			}
			approval, ok := r.next(t)
			if !ok || approval.Kind != "approval" || approval.Approval == nil {
				t.Fatalf("第二帧应是 approval，实际 %+v ok=%v", approval, ok)
			}
			if approval.Approval.Tool != "danger" || approval.Approval.ID == "" {
				t.Fatalf("approval 载荷不全: %+v", approval.Approval)
			}

			body := fmt.Sprintf(`{"sessionId":"s1","requestId":%q,"decision":%q}`, approval.Approval.ID, tc.decision)
			post, err := http.Post(ts.URL+"/api/approval", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("POST /api/approval: %v", err)
			}
			post.Body.Close()
			if post.StatusCode != http.StatusOK {
				t.Fatalf("回执状态码 = %d，want 200", post.StatusCode)
			}

			result, ok := r.next(t)
			if !ok || result.Kind != "tool_result" || result.Text != tc.wantResult {
				t.Fatalf("审批后应是 %q 的 tool_result，实际 %+v ok=%v", tc.wantResult, result, ok)
			}
			final, ok := r.next(t)
			if !ok || final.Kind != "message" || final.Text != "收尾" {
				t.Fatalf("最后应是收尾 message，实际 %+v ok=%v", final, ok)
			}
			if _, ok := r.next(t); ok {
				t.Fatal("收尾后流应结束")
			}
		})
	}
}

// TestApprovalBadRequest 验证回执校验：非法 decision 回 400，未知请求回 404。
func TestApprovalBadRequest(t *testing.T) {
	srv := New(context.Background(), Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, &scriptModel{}, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"decision 非法", `{"sessionId":"s1","requestId":"x","decision":"yes"}`, http.StatusBadRequest},
		{"请求不存在", `{"sessionId":"s1","requestId":"nope","decision":"approve"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/api/approval", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST /api/approval: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("状态码 = %d，want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
