// 本文件：webui 的端到端测试——假模型走真实 ChatModelAgent 图，httptest 起服务，
// 按 SSE 帧断言事件序列与冲突检查；审批用例在 Task 2 追加。
package webui

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// Stream 把 Generate 的一条回复拆成流。Web 打开了逐字流出，测试模型必须能 Stream。
func (s *scriptModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := s.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
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
	// onGenerate 在关闭 started 之前调用，此时 start 信封应已落盘、模型还没返回。
	onGenerate func()
}

// 编译期校验 blockingModel 实现了 model.ToolCallingChatModel。
var _ model.ToolCallingChatModel = (*blockingModel)(nil)

// Generate 实现 model.ToolCallingChatModel 的 Generate：先跑 onGenerate，再关 started，然后等 release 或 ctx 取消。
func (m *blockingModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if m.onGenerate != nil {
		m.onGenerate()
	}
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return schema.AssistantMessage("完成", nil), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Stream 与 Generate 一样先卡住，再把整句作为一块流出。
func (m *blockingModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
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
type sseReader struct {
	sc  *bufio.Scanner
	raw bool // 为真时保留还在变长的正文帧
}

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
		if ev.Partial && !r.raw {
			continue
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

// newTestServer 用临时库文件起服务，测试结束关库。
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(t.TempDir(), "webui.db")
	}
	srv, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// TestChatSSESequence 验证一轮的事件序列（《10》§6 关口 3）：
// start → 假模型建表 → 看板回执 → 收尾 → complete。tool_call 带上剧本里的原始参数。
func TestChatSSESequence(t *testing.T) {
	replyModel := &scriptModel{replies: []*schema.Message{
		toolCallMsg("c1", taskboard.ToolCreate, `{"subject":"查时间"}`),
		schema.AssistantMessage("收尾", nil),
	}}
	boardMW, err := taskboard.New(taskboard.NewStore())
	if err != nil {
		t.Fatalf("taskboard.New: %v", err)
	}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
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
	wantKinds := []string{"start", "tool_call", "board", "message", "complete"}
	if len(got) != len(wantKinds) {
		t.Fatalf("事件序列 = %+v，want %v", got, wantKinds)
	}
	for i, kind := range wantKinds {
		if got[i].Kind != kind || got[i].Seq != int64(i+1) {
			t.Fatalf("帧 %d = %+v，want kind=%s seq=%d", i, got[i], kind, i+1)
		}
	}
	if got[0].Text != "先建个任务再收尾" {
		t.Fatalf("start 文本 = %q", got[0].Text)
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Name != taskboard.ToolCreate || got[1].ToolCalls[0].Args != `{"subject":"查时间"}` {
		t.Fatalf("tool_call 载荷 = %+v", got[1])
	}
	if !strings.Contains(got[2].Text, "查时间") {
		t.Fatalf("board 原文应含任务主题，实际 %q", got[2].Text)
	}
	if got[3].Text != "收尾" {
		t.Fatalf("收尾事件 = %+v", got[3])
	}
}

// TestStartPersistedBeforeModel 验证调用模型之前 start 已经提交：
// Generate 一进来就能从另一条连接读到 seq=1 的 start，文本是本轮用户消息。
func TestStartPersistedBeforeModel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seen.db")
	release := make(chan struct{})
	bm := &blockingModel{started: make(chan struct{}), release: release}
	bm.onGenerate = func() {
		dsn, err := sqliteDSN(dbPath)
		if err != nil {
			t.Errorf("dsn: %v", err)
			return
		}
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Errorf("打开库: %v", err)
			return
		}
		defer db.Close()
		var payload string
		if err := db.QueryRow(`SELECT payload FROM event_log WHERE seq = 1`).Scan(&payload); err != nil {
			t.Errorf("start 应已落盘: %v", err)
			return
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Errorf("解析 start: %v", err)
			return
		}
		if ev.Kind != "start" || ev.Text != "等一下" || ev.Seq != 1 {
			t.Errorf("start = %+v", ev)
		}
	}
	srv := newTestServer(t, Config{DBPath: dbPath, NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	errCh := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"sessionId":"s1","message":"等一下"}`))
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		errCh <- err
	}()
	<-bm.started
	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

// TestChatConflict409 验证同一会话并发两轮：第二轮在写 SSE header 之前拿到 409。
func TestChatConflict409(t *testing.T) {
	bm := &blockingModel{started: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
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
	<-bm.started // 模型已进入 Generate：liveRun 已占上。start 可能已经写出响应头，409 看的是第二轮。

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
			danger, err := utils.InferTool("danger", "测试：会被 interruptMW 暂停的工具",
				func(context.Context, struct{}) (string, error) { return "执行完毕", nil })
			if err != nil {
				t.Fatalf("建 danger: %v", err)
			}
			m := &scriptModel{replies: []*schema.Message{
				toolCallMsg("c1", "danger", `{}`),
				schema.AssistantMessage("收尾", nil),
			}}
			srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
				return newTestAgent(t, m, []tool.BaseTool{danger}, &interruptMW{}), nil
			}})
			ts := httptest.NewServer(srv)
			defer ts.Close()

			resp := postChat(t, ts.URL, "s1", "跑个危险工具")
			defer resp.Body.Close()
			r := newSSEReader(resp.Body)

			start, ok := r.next(t)
			if !ok || start.Kind != "start" {
				t.Fatalf("第一帧应是 start，实际 %+v ok=%v", start, ok)
			}
			ev, ok := r.next(t)
			if !ok || ev.Kind != "tool_call" {
				t.Fatalf("第二帧应是 tool_call，实际 %+v ok=%v", ev, ok)
			}
			approval, ok := r.next(t)
			if !ok || approval.Kind != "approval" || approval.Approval == nil {
				t.Fatalf("第三帧应是 approval，实际 %+v ok=%v", approval, ok)
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

			receipt, ok := r.next(t)
			if !ok || receipt.Kind != "approval_result" || receipt.Approval == nil || receipt.Approval.Decision != tc.decision {
				t.Fatalf("审批后应是回执 %s，实际 %+v ok=%v", tc.decision, receipt, ok)
			}
			result, ok := r.next(t)
			if !ok || result.Kind != "tool_result" || result.Text != tc.wantResult {
				t.Fatalf("回执后应是 %q 的 tool_result，实际 %+v ok=%v", tc.wantResult, result, ok)
			}
			final, ok := r.next(t)
			if !ok || final.Kind != "message" || final.Text != "收尾" {
				t.Fatalf("应收尾 message，实际 %+v ok=%v", final, ok)
			}
			done, ok := r.next(t)
			if !ok || done.Kind != "complete" {
				t.Fatalf("最后应是 complete，实际 %+v ok=%v", done, ok)
			}
			if _, ok := r.next(t); ok {
				t.Fatal("complete 后流应结束")
			}
		})
	}
}

// TestApprovalBadRequest 验证回执校验：非法 decision 回 400，未知请求回 404。
func TestApprovalBadRequest(t *testing.T) {
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
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

// TestIndexPage 验证单页可出：200 + HTML，且带聊天/审批/看板的关键挂点。
func TestIndexPage(t *testing.T) {
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, &scriptModel{}, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q，want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读页面: %v", err)
	}
	for _, marker := range []string{`id="log"`, `id="board-body"`, `id="approval"`, "api/chat", "api/approval", "api/stop"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("页面缺挂点 %q", marker)
		}
	}
}

// collectSSE 读完整条流。流应在终局后由服务端结束。
func collectSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	r := newSSEReader(body)
	var out []sseEvent
	for {
		ev, ok := r.next(t)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func getJSON(t *testing.T, url string, dest any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if dest != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
			t.Fatalf("解析 JSON: %v", err)
		}
	}
	return resp
}

// TestDisconnectDoesNotCancelRun 断掉第一个读者后 run 仍在，重连 from=最后一帧能接上且不重复。
func TestDisconnectDoesNotCancelRun(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	bm := &blockingModel{started: make(chan struct{}), release: release}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"sessionId":"s1","message":"继续"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := newSSEReader(resp.Body).next(t)
	if !ok || first.Kind != "start" || first.Seq != 1 || first.Run == "" {
		t.Fatalf("第一帧 = %+v ok=%v", first, ok)
	}
	cancel()
	resp.Body.Close()

	var view struct {
		LiveRun string `json:"liveRun"`
	}
	sessResp := getJSON(t, ts.URL+"/api/session?id=s1", &view)
	if sessResp.StatusCode != http.StatusOK || view.LiveRun != first.Run {
		t.Fatalf("断线后 liveRun = %q status=%d，want %s", view.LiveRun, sessResp.StatusCode, first.Run)
	}

	releaseOnce.Do(func() { close(release) })
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer streamCancel()
	sreq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, fmt.Sprintf("%s/api/chat/stream?run=%s&from=%d", ts.URL, first.Run, first.Seq), nil)
	if err != nil {
		t.Fatal(err)
	}
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("续传状态码 = %d", sresp.StatusCode)
	}
	rest := collectSSE(t, sresp.Body)
	if len(rest) == 0 || rest[len(rest)-1].Kind != "complete" {
		t.Fatalf("续传 = %+v，want 以 complete 结束", rest)
	}
	for i, ev := range rest {
		want := first.Seq + int64(i) + 1
		if ev.Seq != want {
			t.Fatalf("续传 seq[%d] = %d，want %d（重复或空洞）", i, ev.Seq, want)
		}
	}
}

// TestStopOneTerminal 停止只产生一条 stop，对不上的 run 是 404，停止后可以再开一轮。
func TestStopOneTerminal(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	bm := &blockingModel{started: make(chan struct{}), release: release}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postChat(t, ts.URL, "s1", "停一下")
	defer resp.Body.Close()
	r := newSSEReader(resp.Body)
	first, ok := r.next(t)
	if !ok || first.Kind != "start" {
		t.Fatalf("第一帧 = %+v", first)
	}
	bad, err := http.Post(ts.URL+"/api/stop", "application/json", strings.NewReader(`{"sessionId":"s1","run":"r_nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusNotFound {
		t.Fatalf("错误 run 状态码 = %d，want 404", bad.StatusCode)
	}
	good, err := http.Post(ts.URL+"/api/stop", "application/json", strings.NewReader(fmt.Sprintf(`{"sessionId":"s1","run":%q}`, first.Run)))
	if err != nil {
		t.Fatal(err)
	}
	good.Body.Close()
	if good.StatusCode != http.StatusOK {
		t.Fatalf("停止状态码 = %d，want 200", good.StatusCode)
	}
	var terminals []sseEvent
	for {
		ev, ok := r.next(t)
		if !ok {
			break
		}
		if isTerminal(ev.Kind) {
			terminals = append(terminals, ev)
		}
	}
	if len(terminals) != 1 || terminals[0].Kind != "stop" {
		t.Fatalf("终局 = %+v，want 唯一一条 stop", terminals)
	}
	releaseOnce.Do(func() { close(release) })

	again := postChat(t, ts.URL, "s1", "下一轮")
	defer again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("停止后下一轮状态码 = %d", again.StatusCode)
	}
	next := collectSSE(t, again.Body)
	if len(next) == 0 || next[len(next)-1].Kind != "complete" {
		t.Fatalf("下一轮 = %+v", next)
	}
}

// TestTwoReaders 后来的连接通过 GET /api/session 拿到 run，两边的 seq 一致。
func TestTwoReaders(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	bm := &blockingModel{started: make(chan struct{}), release: release}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	firstCh := make(chan []sseEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"sessionId":"s1","message":"一起看"}`))
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		firstCh <- collectSSE(t, resp.Body)
	}()

	var view struct {
		LiveRun string `json:"liveRun"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for view.LiveRun == "" && time.Now().Before(deadline) {
		getJSON(t, ts.URL+"/api/session?id=s1", &view)
		time.Sleep(10 * time.Millisecond)
	}
	if view.LiveRun == "" {
		t.Fatal("会话没有报出 liveRun")
	}
	streamCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sreq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, ts.URL+"/api/chat/stream?run="+view.LiveRun+"&from=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	releaseOnce.Do(func() { close(release) })
	second := collectSSE(t, sresp.Body)
	var first []sseEvent
	select {
	case err := <-errCh:
		t.Fatal(err)
	case first = <-firstCh:
	}
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("读者帧数 %d 与 %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Seq != second[i].Seq || first[i].Kind != second[i].Kind {
			t.Fatalf("帧 %d：%+v vs %+v", i, first[i], second[i])
		}
	}
}

// TestStreamEdges 未知 run 是 404，from 超过最大 seq 是空的 200，会话 id 为空是 400。
func TestStreamEdges(t *testing.T) {
	reply := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("好", nil)}}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, reply, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	missing, err := http.Get(ts.URL + "/api/chat/stream?run=r_missing")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 run = %d，want 404", missing.StatusCode)
	}
	noID, err := http.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	noID.Body.Close()
	if noID.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 id = %d，want 400", noID.StatusCode)
	}

	resp := postChat(t, ts.URL, "s1", "你好")
	defer resp.Body.Close()
	evs := collectSSE(t, resp.Body)
	if len(evs) == 0 || evs[len(evs)-1].Kind != "complete" {
		t.Fatalf("首轮 = %+v", evs)
	}
	pastCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	preq, err := http.NewRequestWithContext(pastCtx, http.MethodGet, fmt.Sprintf("%s/api/chat/stream?run=%s&from=99", ts.URL, evs[0].Run), nil)
	if err != nil {
		t.Fatal(err)
	}
	past, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	defer past.Body.Close()
	body, err := io.ReadAll(past.Body)
	if err != nil {
		t.Fatal(err)
	}
	if past.StatusCode != http.StatusOK || strings.Contains(string(body), "data:") {
		t.Fatalf("越界续传 status=%d body=%q", past.StatusCode, body)
	}
}

// recordingModel 记下每次 Generate 收到的消息，用来确认下一轮读的是档案而不是内存历史。
type recordingModel struct {
	mu      sync.Mutex
	replies []*schema.Message
	seen    [][]*schema.Message
}

var _ model.ToolCallingChatModel = (*recordingModel)(nil)

func (m *recordingModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*schema.Message, len(in))
	copy(cp, in)
	m.seen = append(m.seen, cp)
	if len(m.replies) == 0 {
		return nil, fmt.Errorf("剧本已用完")
	}
	msg := m.replies[0]
	m.replies = m.replies[1:]
	return msg, nil
}

func (m *recordingModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *recordingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// TestRestartReloadsEventsAndCompletedTurns 空内存的新进程从同一库装回事件，
// 下一轮模型输入含上一轮已完成的用户消息和助手回复。
func TestRestartReloadsEventsAndCompletedTurns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "webui.db")
	model := &recordingModel{replies: []*schema.Message{
		schema.AssistantMessage("早", nil),
		schema.AssistantMessage("好", nil),
	}}
	newAgent := func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, model, nil), nil
	}
	first := newTestServer(t, Config{DBPath: dbPath, NewAgent: newAgent})
	ts1 := httptest.NewServer(first)
	resp := postChat(t, ts1.URL, "s1", "你好")
	evs := collectSSE(t, resp.Body)
	resp.Body.Close()
	ts1.Close()
	if len(evs) < 2 || evs[len(evs)-1].Kind != "complete" {
		t.Fatalf("首轮 = %+v", evs)
	}

	second, err := New(context.Background(), Config{DBPath: dbPath, NewAgent: newAgent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	ts2 := httptest.NewServer(second)
	defer ts2.Close()

	pastCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	from := evs[0].Seq
	sreq, err := http.NewRequestWithContext(pastCtx, http.MethodGet, fmt.Sprintf("%s/api/chat/stream?run=%s&from=%d", ts2.URL, evs[0].Run, from), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	got := collectSSE(t, stream.Body)
	stream.Body.Close()
	if len(got) != len(evs)-1 {
		t.Fatalf("重启后续传 = %+v，want %d 帧", got, len(evs)-1)
	}
	for i, ev := range got {
		if ev.Seq != evs[i+1].Seq || ev.Kind != evs[i+1].Kind {
			t.Fatalf("续传帧 %d = %+v，want %+v", i, ev, evs[i+1])
		}
	}

	resp2 := postChat(t, ts2.URL, "s1", "再说")
	defer resp2.Body.Close()
	secondRound := collectSSE(t, resp2.Body)
	if len(secondRound) == 0 || secondRound[len(secondRound)-1].Kind != "complete" {
		t.Fatalf("第二轮 = %+v", secondRound)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.seen) != 2 {
		t.Fatalf("模型被调用 %d 次，want 2", len(model.seen))
	}
	var texts []string
	for _, msg := range model.seen[1] {
		texts = append(texts, string(msg.Role)+":"+msg.Content)
	}
	want := []string{"user:你好", "assistant:早", "user:再说"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("第二轮模型输入 = %v，want %v", texts, want)
	}
}

// gatedModel 把正文拆成多块，并在最后一块之前停住，用来观察半截尚未落库。
type gatedModel struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	chunks  []string
}

var _ model.ToolCallingChatModel = (*gatedModel)(nil)

func (m *gatedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(strings.Join(m.chunks, ""), nil), nil
}

func (m *gatedModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		m.once.Do(func() { close(m.started) })
		for i, chunk := range m.chunks {
			if i == len(m.chunks)-1 {
				select {
				case <-m.release:
				case <-ctx.Done():
					writer.Send(nil, ctx.Err())
					return
				}
			}
			if writer.Send(schema.AssistantMessage(chunk, nil), nil) {
				return
			}
		}
	}()
	return reader, nil
}

func (m *gatedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// TestSameProcessTurnKeepsRunArchiveCache 同一进程的下一轮用内存里的档案消息。
// 第一轮收尾后改掉库里的 user_message，第二轮模型仍看到原来的「你好」。
func TestSameProcessTurnKeepsRunArchiveCache(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "webui.db")
	model := &recordingModel{replies: []*schema.Message{
		schema.AssistantMessage("早", nil),
		schema.AssistantMessage("好", nil),
	}}
	srv := newTestServer(t, Config{DBPath: dbPath, NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, model, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := postChat(t, ts.URL, "s1", "你好")
	first := collectSSE(t, resp.Body)
	resp.Body.Close()
	if len(first) == 0 || first[len(first)-1].Kind != "complete" {
		t.Fatalf("首轮 = %+v", first)
	}
	d, err := OpenLogDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.db.ExecContext(context.Background(), `UPDATE run_archive SET user_message = ? WHERE session_id = ?`, "被改过", "s1"); err != nil {
		t.Fatal(err)
	}

	resp2 := postChat(t, ts.URL, "s1", "再说")
	defer resp2.Body.Close()
	second := collectSSE(t, resp2.Body)
	if len(second) == 0 || second[len(second)-1].Kind != "complete" {
		t.Fatalf("第二轮 = %+v", second)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.seen) != 2 {
		t.Fatalf("模型被调用 %d 次，want 2", len(model.seen))
	}
	var texts []string
	for _, msg := range model.seen[1] {
		texts = append(texts, string(msg.Role)+":"+msg.Content)
	}
	want := []string{"user:你好", "assistant:早", "user:再说"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("第二轮模型输入 = %v，want %v", texts, want)
	}
}

// TestPartialTextIsNotDurable 半截正文只在内存里。放开最后一块之后，库里才有完整的一条。
func TestPartialTextIsNotDurable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "webui.db")
	release := make(chan struct{})
	model := &gatedModel{release: release, started: make(chan struct{}), chunks: []string{"你", "好"}}
	srv := newTestServer(t, Config{DBPath: dbPath, NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, model, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"sessionId":"s1","message":"问"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := &sseReader{sc: bufio.NewScanner(resp.Body), raw: true}
	var partial sseEvent
	for {
		ev, ok := raw.next(t)
		if !ok {
			t.Fatal("流在半截之前结束")
		}
		if ev.Partial && ev.Text == "你" {
			partial = ev
			break
		}
	}
	<-model.started
	d, err := OpenLogDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mid, err := d.ReadAfter(context.Background(), partial.Run, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range mid {
		if ev.Kind == "message" || strings.Contains(ev.Text, "你") {
			t.Fatalf("半截不该落库，实际 %+v", mid)
		}
	}
	close(release)
	var sealed bool
	for {
		ev, ok := raw.next(t)
		if !ok {
			break
		}
		if ev.Kind == "message" && !ev.Partial && ev.Text == "你好" && ev.Seq == partial.Seq {
			sealed = true
		}
	}
	if !sealed {
		t.Fatal("写完后应有一条序号不变的正文")
	}
	done, err := d.ReadAfter(context.Background(), partial.Run, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var messages int
	for _, ev := range done {
		if ev.Kind == "message" {
			messages++
			if ev.Text != "你好" || ev.Partial {
				t.Fatalf("落库正文 = %+v", ev)
			}
		}
	}
	if messages != 1 {
		t.Fatalf("正文应只有一条，实际 %+v", done)
	}
}

// TestRestoreCompletedMessage 进程没把写完的正文落库时，补发按原序号写回，下一轮模型读得到。
func TestRestoreCompletedMessage(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.BeginRun(ctx, "r_1", "s", "问", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := d.RestoreMessage(ctx, "s", sseEvent{Run: "r_1", Seq: 2, Kind: "message", Text: "早", Role: "assistant"})
	if err != nil || !stored {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
	again, err := d.RestoreMessage(ctx, "s", sseEvent{Run: "r_1", Seq: 2, Kind: "message", Text: "早", Role: "assistant"})
	if err != nil || again {
		t.Fatalf("重复补发 stored=%v err=%v", again, err)
	}
	turns, err := d.CompletedTurns(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserMessage != "问" || !strings.Contains(turns[0].ProducedJSON, "早") {
		t.Fatalf("补发后的轮次 = %+v", turns)
	}
	evs, err := d.ReadAfter(ctx, "r_1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Seq != 2 || evs[1].Text != "早" {
		t.Fatalf("事件 = %+v", evs)
	}
}

// TestHandleHistory_OK 验证历史接口正常返回已结束的 run。
func TestHandleHistory_OK(t *testing.T) {
	reply := &scriptModel{replies: []*schema.Message{schema.AssistantMessage("好", nil)}}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, reply, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 跑一轮
	resp := postChat(t, ts.URL, "s1", "你好")
	evs := collectSSE(t, resp.Body)
	resp.Body.Close()
	if len(evs) == 0 || evs[len(evs)-1].Kind != "complete" {
		t.Fatalf("首轮 = %+v", evs)
	}

	// 读历史
	var page HistoryPage
	histResp := getJSON(t, ts.URL+"/api/session/history?session=s1", &page)
	if histResp.StatusCode != http.StatusOK {
		t.Fatalf("历史状态码 = %d", histResp.StatusCode)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %d，want 1", len(page.Runs))
	}
	if page.Runs[0].RunID != evs[0].Run {
		t.Fatalf("runId = %s，want %s", page.Runs[0].RunID, evs[0].Run)
	}
	if len(page.Runs[0].Events) != len(evs) {
		t.Fatalf("events = %d，want %d", len(page.Runs[0].Events), len(evs))
	}
}

// TestHandleHistory_MissingSession 验证缺少 session 参数时返回 400。
func TestHandleHistory_MissingSession(t *testing.T) {
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, &scriptModel{}, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/session/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，want 400", resp.StatusCode)
	}
}

// TestHandleHistory_InvalidCursor 验证无效 cursor 返回 400。
func TestHandleHistory_InvalidCursor(t *testing.T) {
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, &scriptModel{}, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/session/history?session=s1&before=bad-cursor")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，want 400", resp.StatusCode)
	}
}

// TestHandleHistory_ExcludesRunning 验证正在跑的 run 不在历史里。
func TestHandleHistory_ExcludesRunning(t *testing.T) {
	release := make(chan struct{})
	bm := &blockingModel{started: make(chan struct{}), release: release}
	srv := newTestServer(t, Config{NewAgent: func(context.Context) (adk.Agent, error) {
		return newTestAgent(t, bm, nil), nil
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 开一轮但不让它结束
	go func() {
		resp := postChat(t, ts.URL, "s1", "跑着")
		resp.Body.Close()
	}()
	<-bm.started

	// 读历史，应该是空的
	var page HistoryPage
	histResp := getJSON(t, ts.URL+"/api/session/history?session=s1", &page)
	if histResp.StatusCode != http.StatusOK {
		t.Fatalf("历史状态码 = %d", histResp.StatusCode)
	}
	if len(page.Runs) != 0 {
		t.Fatalf("runs = %d，want 0（running 不应出现）", len(page.Runs))
	}

	close(release)
}
