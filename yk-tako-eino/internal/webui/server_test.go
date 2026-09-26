// 本文件：webui 的端到端测试——假模型走真实 ChatModelAgent 图，httptest 起服务，
// 按 SSE 帧断言事件序列与冲突检查；审批用例在 Task 2 追加。
package webui

import (
	"bufio"
	"context"
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
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
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
