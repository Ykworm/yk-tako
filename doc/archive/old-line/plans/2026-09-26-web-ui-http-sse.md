# 最小 Web-UI T2：HTTP + SSE 最小 UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给唯一入口加最小 Web UI——浏览器里聊天（SSE 逐事件推流）、审批卡片（POST 回执唤醒）、看板面板（事件投影）；新包 `internal/webui` + `cmd/yktako -web` 开关，REPL 一条不改（《10》§2.4 步 2）。

**Architecture:** `internal/webui` 持有会话表（session → 专属 agent/runner/history/审批挂起表）。`POST /api/chat` 先做冲突检查（同会话已有 run → 409，**在写 SSE header 之前**，照学 WeKnora `qa.go` 的顺序教训）再开流；adkrun 的事件经 sink 转 SSE 载荷；`approval` 由本层 `ask` 产出、`POST /api/approval` 送决定唤醒挂起的 run（resume 值仍是 `"approve"`/`"reject"`，`ApprovalInfo` 不动）；单页用 `//go:embed` 焊进二进制。会话之间看板不串账：agent（含 taskboard 正本）按会话新建，生产由 `cmd/yktako` 的工厂提供。

**Tech Stack:** Go 1.27 标准库（`net/http` 方法路由与 `{$}`、`httptest`）、eino v0.9.21、复用 `adkrun`/`guard`/`taskboard`。上游依据：《10》§2.4/§3.1/§4/§5/§6、doc/11 §4（T2.1–T2.5）。

**执行说明（与默认流程的有意偏离，原因可检验）：**
1. 不新建 worktree/分支：直接在 `master` 上逐任务提交，与仓库既有习惯一致（见 `git log`）；起点 HEAD 为 T1 收尾 `4c65f0e`，工作区干净。
2. 提交消息中文短句、不加 `feat:` 前缀；每个 Task 末尾提交一次。
3. 范围纪律以《10》§5 为界：不做鉴权/RBAC、不做 token 级 delta、不做断线回放、不做前端构建链、不做 Wails、不顺手做 #4；服务默认只该绑 `127.0.0.1`（README 写明）。
4. 所有新增/修改代码按 AGENTS.md：中文注释、文件头与函数头齐全；实现 Eino 接口的测试假件同样标注。
5. 已知的第一期边界（写进代码注释，不在本计划扩）：会话表只在进程内、不清理；单页无重连，POST 流断线即这一轮作废（《10》§4.4）。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `yk-tako-eino/internal/webui/sse.go` | 新建 | SSE 线格式：`sseEvent`/`sseApproval` 载荷、`sseEventFromAdkrun` 映射、`writeSSE` 写帧 |
| `yk-tako-eino/internal/webui/session.go` | 新建 | 会话：专属 agent/runner、history、单轮并发闸、审批挂起表、`runTurn`（sink/ask 接法） |
| `yk-tako-eino/internal/webui/server.go` | 新建 | `Server`/`Config`/路由、会话表、`handleChat`（409 先于 header）、`handleApproval` |
| `yk-tako-eino/internal/webui/page.go` | 新建 | `//go:embed static/index.html` + `handleIndex` |
| `yk-tako-eino/internal/webui/static/index.html` | 新建 | 单页：聊天流 + 审批卡片 + 看板面板（原生 JS，无构建链） |
| `yk-tako-eino/internal/webui/server_test.go` | 新建 | 假模型/假 interrupt middleware（走真实 ChatModelAgent 图）+ SSE 解析 + 4 个端到端测试 |
| `yk-tako-eino/cmd/yktako/main.go` | 修改 | 抽 `newAgent` 工厂（每会话一套看板）；加 `-web` 开关进 Web 模式 |
| `yk-tako-eino/README.md`、根 `README.md` | 修改 | 目录与用法补 Web 入口（Task 6） |
| `doc/01`、`doc/05`、`doc/06`、`doc/10`、`doc/11` | 修改 | 落点/缺口/路线表/台账同步（Task 6） |

不改：`internal/adkrun`（事件契约不动）、`internal/repl`（保持「与 Eino 无关」）、`internal/taskboard`、`internal/guard` 的引擎与规则。

任务与 doc/11 §4 的映射：Task 1 = T2.1 + T2.2、Task 2 = T2.3、Task 3 = T2.4、Task 5 = T2.5；Task 4（入口接线）与 Task 6（文档）是台账未细列的必要收尾，完成后回填台账。

---

## Task 0: 计划入档

**Files:**
- Create（已存在，未跟踪）: `docs/superpowers/plans/2026-09-26-web-ui-http-sse.md`

- [ ] **Step 1: 提交本计划，此后每笔提交才只含当步改动**

```bash
git add docs/superpowers/plans/2026-09-26-web-ui-http-sse.md
git commit -m "plan：T2 HTTP + SSE 最小 UI 执行计划入档"
```

- [ ] **Step 2: 确认工作区干净**

Run: `git status --short`
Expected: 无输出。

---

---

## Task 1 (T2.1 + T2.2 骨架): webui 包与 SSE 聊天流

**Files:**
- Create: `yk-tako-eino/internal/webui/sse.go`
- Create: `yk-tako-eino/internal/webui/session.go`
- Create: `yk-tako-eino/internal/webui/server.go`
- Create: `yk-tako-eino/internal/webui/server_test.go`

- [ ] **Step 1: 写端到端测试（先失败）**

`yk-tako-eino/internal/webui/server_test.go`：

```go
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/webui/ -run 'TestChat'`
Expected: 编译失败（`undefined: New`、`undefined: Config`、`undefined: sseEvent`）。

- [ ] **Step 3: 实现 `sse.go`**

`yk-tako-eino/internal/webui/sse.go`：

```go
// 本文件：SSE 线格式——adkrun 事件与 approval/error 的统一载荷，以及写帧。
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"yk-tako-eino/internal/adkrun"
)

// sseApproval 是 approval 事件的载荷：审批请求 id + guard 的原始信息。
// id 用于回执定位：前端 POST /api/approval 带上它，服务端凭它唤醒挂起的 ask。
type sseApproval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	Message string `json:"message,omitempty"`
}

// sseEvent 是发给前端的一条事件，字段名对齐《10》§4.2 的事件契约：
// 前四类来自 adkrun.Event（message/tool_call/tool_result/board），
// approval 由本层的 ask 产出，error 来自 Run 的返回。
type sseEvent struct {
	Kind      string       `json:"kind"`
	Agent     string       `json:"agent,omitempty"`
	Role      string       `json:"role,omitempty"`
	Text      string       `json:"text,omitempty"`
	ToolName  string       `json:"toolName,omitempty"`
	ToolNames []string     `json:"toolNames,omitempty"`
	Approval  *sseApproval `json:"approval,omitempty"`
}

// sseEventFromAdkrun 把驱动层事件映射成 SSE 载荷：字段一一对应，不做格式加工
//（前缀、截断这些呈现决定归前端，同 T1 的「呈现归消费方」）。
func sseEventFromAdkrun(ev adkrun.Event) sseEvent {
	return sseEvent{
		Kind:      string(ev.Kind),
		Agent:     ev.Agent,
		Role:      string(ev.Role),
		Text:      ev.Text,
		ToolName:  ev.ToolName,
		ToolNames: ev.ToolNames,
	}
}

// writeSSE 写一帧并立即刷出：data 行装 JSON，帧以空行结束（SSE 规范）。
// 返回 false 表示写不出去（客户端断了）：调用方停止写并排干事件，
// 这一轮作废——sink 是易失的，断线不回放（《10》§4.4）。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev sseEvent) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
```

- [ ] **Step 4: 实现 `session.go`**

`yk-tako-eino/internal/webui/session.go`：

```go
// 本文件：会话——每个会话一套专属 agent/runner（看板正本与 checkpoint 不跨会话串账）、
// 历史与单轮并发闸。
package webui

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
)

// session 是一个聊天会话的全部状态。除 agent/runner 不可变外，其余字段由 mu 保护：
// HTTP handler 与 run goroutine 会并发碰它们（-race 是合入门槛，《10》§6）。
type session struct {
	id     string
	agent  adk.Agent
	runner *adk.Runner

	mu      sync.Mutex
	history []*schema.Message // 本会话累积的历史，语义同 REPL 的 history
	turn    int               // 轮次计数：checkpointID 用它拼，且每会话独立
	active  bool              // 是否有 run 在跑（POST /api/chat 的 409 判定）
}

// newSession 建会话：agent 由 Server 的工厂造（每会话一套），runner 的
// checkpoint store 也是每会话一份——进程内存，重启即丢（《10》§4.4）。
func newSession(ctx context.Context, id string, agent adk.Agent) *session {
	return &session{
		id:     id,
		agent:  agent,
		runner: adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: false, CheckPointStore: adkrun.NewStore()}),
	}
}

// beginTurn 开一轮：占并发闸、取消息快照。已有 run 在跑返回 ok=false（调用方回 409）。
func (s *session) beginTurn(message string) (turn int, messages []*schema.Message, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return 0, nil, false
	}
	s.active = true
	s.turn++
	turn = s.turn
	messages = append(append([]*schema.Message{}, s.history...), schema.UserMessage(message))
	return turn, messages, true
}

// finishTurn 收轮：成功才把用户消息与产出写进历史（与 REPL 的「err != nil 不落历史」一致）。
func (s *session) finishTurn(message string, produced []*schema.Message, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.history = append(s.history, schema.UserMessage(message))
		s.history = append(s.history, produced...)
	}
	s.active = false
}

// runTurn 跑一轮：把 adkrun 事件经 sink 转成 SSE 载荷发进 events。
// 发送带 ctx 守卫：客户端断线时 run 侧不卡死在发送上（handler 已停止读）。
func (s *session) runTurn(ctx context.Context, turn int, messages []*schema.Message, events chan<- sseEvent) ([]*schema.Message, error) {
	send := func(ev sseEvent) {
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	sink := func(ev adkrun.Event) { send(sseEventFromAdkrun(ev)) }
	// 审批回调先传 nil（T2.3 换成挂起等 POST 的实现）；nil 是 adkrun 定义的
	// 「不接审批」语义，本步测试的剧本不触发 interrupt。
	return adkrun.Run(ctx, s.runner, s.agent, fmt.Sprintf("turn-%d", turn), messages, sink, nil)
}
```

- [ ] **Step 5: 实现 `server.go`**

`yk-tako-eino/internal/webui/server.go`：

```go
// 本文件：HTTP 装配——路由、会话表、/api/chat 的 SSE 响应与冲突检查。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/cloudwego/eino/adk"
)

// Config 是装配参数。
type Config struct {
	// NewAgent 必须是「每调用一套新 agent」：taskboard 正本（看板）挂在 middleware
	// 实例上，共用会让不同会话的看板串账。生产由 cmd/yktako 提供，测试注入假模型。
	NewAgent func(ctx context.Context) (adk.Agent, error)
}

// Server 是 webui 的 http.Handler：持有会话表与 agent 工厂。
type Server struct {
	ctx      context.Context // 基础设施生命周期：建 runner/agent 用，绝不能用请求 ctx（请求一断 runner 内部状态跟着没）
	newAgent func(ctx context.Context) (adk.Agent, error)
	mux      *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
}

// New 组装 Server 与路由。ctx 决定所有会话 runner 的生命周期（生产传进程 ctx）。
func New(ctx context.Context, cfg Config) *Server {
	s := &Server{ctx: ctx, newAgent: cfg.NewAgent, sessions: map[string]*session{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", s.handleChat)
	s.mux = mux
	return s
}

// ServeHTTP 实现 net/http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// session 取或建会话；同一 id 的并发首访由 s.mu 串行。
func (s *Server) session(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	agent, err := s.newAgent(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("建会话 agent: %w", err)
	}
	sess := newSession(s.ctx, id, agent)
	s.sessions[id] = sess
	return sess, nil
}

// chatRequest 是 POST /api/chat 的请求体。
type chatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

// handleChat 跑一轮并把事件以 SSE 推给前端。冲突检查（同会话已有 run）在写
// SSE header 之前——流一旦开了就没法回头报错，这个顺序照学 WeKnora（《10》§3.1）。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.Message == "" {
		http.Error(w, "sessionId 与 message 必填", http.StatusBadRequest)
		return
	}
	sess, err := s.session(req.SessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	turn, messages, ok := sess.beginTurn(req.Message)
	if !ok {
		http.Error(w, "该会话已有一轮在跑", http.StatusConflict)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	// events 是 run goroutine 与本次响应之间的唯一通道；缓冲只为解耦，不承诺顺序外语义。
	events := make(chan sseEvent, 16)
	go func() {
		defer close(events)
		produced, err := sess.runTurn(r.Context(), turn, messages, events)
		sess.finishTurn(req.Message, produced, err)
		if err != nil {
			// error 事件的文案走 Run 的返回（T1 口径：error 不经 sink）。
			events <- sseEvent{Kind: "error", Text: err.Error()}
		}
	}()
	for ev := range events {
		if !writeSSE(w, flusher, ev) {
			// 客户端断了：继续排干 events，别让 run goroutine 卡在发送上；
			// run 会因 r.Context() 取消收尾，这一轮作废、不回放（《10》§4.4）。
			for range events {
			}
			return
		}
	}
}
```

- [ ] **Step 6: 跑测试与静态检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./internal/webui/ -race -v`
Expected: `gofmt` 无输出；`TestChatSSESequence`、`TestChatConflict409` 全过。

- [ ] **Step 7: 提交**

```bash
git add yk-tako-eino/internal/webui/sse.go yk-tako-eino/internal/webui/session.go yk-tako-eino/internal/webui/server.go yk-tako-eino/internal/webui/server_test.go
git commit -m "webui：SSE 会话流与冲突检查（chat handler + 假模型测试）"
```

---

## Task 2 (T2.3): 审批挂起与 POST 回执

**Files:**
- Modify: `yk-tako-eino/internal/webui/session.go`（挂起表 + ask）
- Modify: `yk-tako-eino/internal/webui/server.go`（`POST /api/approval`）
- Modify: `yk-tako-eino/internal/webui/server_test.go`（interruptMW + 审批测试）

- [ ] **Step 1: 先追加审批测试（失败）**

`server_test.go` 的 import 块补：`"encoding/gob"`、`"github.com/cloudwego/eino/components/tool/utils"`、`"yk-tako-eino/internal/guard"`。

文件里追加：

```go
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
type interruptMW struct{ adk.BaseChatModelAgentMiddleware }

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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/webui/ -run 'TestApproval'`
Expected: 编译通过但断言失败——`/api/approval` 未注册：往返用例在回执处拿到 404，校验用例拿到 405。

- [ ] **Step 3: `session.go` 加挂起表与 ask**

imports 补 `"sync/atomic"` 与 `"yk-tako-eino/internal/guard"`；在包级加：

```go
// approvalSeq 给审批请求发全局递增 id：前端回执只用它定位，不解析内容。
var approvalSeq atomic.Uint64
```

`session` 结构体加字段：

```go
	pending map[string]chan string // 审批请求 id → 决策通道（ask 挂在这上面）
```

`newSession` 初始化加 `pending: map[string]chan string{}`。

追加三个方法（放在 `finishTurn` 之后）：

```go
// waitApproval 登记一个审批请求，返回等决策的通道。
func (s *session) waitApproval(id string) chan string {
	ch := make(chan string, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	return ch
}

// resolveApproval 把决策送进对应通道；id 不存在或已处理返回 false（HTTP 404）。
func (s *session) resolveApproval(id, decision string) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if !ok {
		return false
	}
	ch <- decision
	return true
}

// dropApproval 取消挂起的审批（run 的 ctx 取消时调用），避免 pending 泄漏。
func (s *session) dropApproval(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}
```

`runTurn` 里把 `nil` 换成真实 ask（替换 `return adkrun.Run(...)` 所在段，注释一并更新）：

```go
	sink := func(ev adkrun.Event) { send(sseEventFromAdkrun(ev)) }
	// ask 是交给 adkrun.Run 的审批回调（《10》§4.3）：不阻塞任何输入，
	// ① 推一条 approval 事件给 SSE，② 挂起在这笔请求的通道上，等 POST /api/approval。
	// resume 值不变：仍返回 guard.ResumeApprove / guard.ResumeReject 字符串。
	ask := func(info guard.ApprovalInfo) (string, error) {
		id := fmt.Sprintf("%s-a%d", s.id, approvalSeq.Add(1))
		ch := s.waitApproval(id)
		send(sseEvent{Kind: "approval", Approval: &sseApproval{ID: id, Tool: info.Tool, Args: info.Args, Message: info.Message}})
		select {
		case decision := <-ch:
			return decision, nil
		case <-ctx.Done():
			s.dropApproval(id)
			return "", ctx.Err()
		}
	}
	return adkrun.Run(ctx, s.runner, s.agent, fmt.Sprintf("turn-%d", turn), messages, sink, ask)
```

- [ ] **Step 4: `server.go` 加回执接口**

imports 补 `"yk-tako-eino/internal/guard"`；`New` 里补路由 `mux.HandleFunc("POST /api/approval", s.handleApproval)`（在 chat 路由之后）。

追加：

```go
// approvalRequest 是 POST /api/approval 的请求体。
type approvalRequest struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"`
}

// handleApproval 把前端决定塞回挂起的 ask（《10》§4.3）。
// resume 值不变：只接受 guard.ResumeApprove / guard.ResumeReject。
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体必须是 JSON", http.StatusBadRequest)
		return
	}
	if req.Decision != guard.ResumeApprove && req.Decision != guard.ResumeReject {
		http.Error(w, "decision 只能是 approve / reject", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sess, ok := s.sessions[req.SessionID]
	s.mu.Unlock()
	if !ok || !sess.resolveApproval(req.RequestID, req.Decision) {
		http.Error(w, "审批请求不存在或已处理", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 5: 跑测试与静态检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./internal/webui/ -race -v`
Expected: 全部通过（含 Task 1 的两个测试）。

- [ ] **Step 6: 提交**

```bash
git add yk-tako-eino/internal/webui/session.go yk-tako-eino/internal/webui/server.go yk-tako-eino/internal/webui/server_test.go
git commit -m "webui：审批挂起与 POST 回执（ask 通道化，resume 值不变）"
```

---

## Task 3 (T2.4): go:embed 单页

**Files:**
- Create: `yk-tako-eino/internal/webui/page.go`
- Create: `yk-tako-eino/internal/webui/static/index.html`
- Modify: `yk-tako-eino/internal/webui/server.go`（根路由）
- Modify: `yk-tako-eino/internal/webui/server_test.go`（页面测试）

- [ ] **Step 1: 写页面测试（失败）**

`server_test.go` 追加：

```go
// TestIndexPage 验证单页可出：200 + HTML，且带聊天/审批/看板的关键挂点。
func TestIndexPage(t *testing.T) {
	srv := New(context.Background(), Config{NewAgent: func(context.Context) (adk.Agent, error) {
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
	for _, marker := range []string{`id="log"`, `id="board-body"`, `id="approval"`, "api/chat", "api/approval"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("页面缺挂点 %q", marker)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/webui/ -run 'TestIndexPage'`
Expected: 404（`GET /` 未注册），状态码断言失败。

- [ ] **Step 3: 实现 `page.go` 与 `static/index.html`**

`yk-tako-eino/internal/webui/page.go`：

```go
// 本文件：内嵌单页——//go:embed 把 static/index.html 焊进二进制（机制见《09》§10），
// 零构建链、零路径依赖（《10》§3.2）。
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

// handleIndex 出单页。注意 `//` 与 `go:embed` 之间不能有空格，否则静默退化（《09》§10.1）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
```

`yk-tako-eino/internal/webui/static/index.html`：

```html
<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>yk-tako</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { margin: 0; font: 14px/1.5 system-ui, sans-serif; display: flex; height: 100vh; }
  #left { flex: 1; display: flex; flex-direction: column; min-width: 0; }
  #log { flex: 1; overflow-y: auto; padding: 12px; }
  .msg { margin: 6px 0; white-space: pre-wrap; }
  .assistant { white-space: pre-wrap; }
  .user { color: #0a7d4f; font-weight: 600; }
  .meta { color: #8a8a8a; font-size: 12px; }
  .error { color: #c0392b; }
  #approval { display: none; padding: 10px 12px; border-top: 1px solid #9994; }
  #approval h3 { margin: 0 0 6px; font-size: 14px; }
  #approval-info { white-space: pre-wrap; margin-bottom: 8px; }
  #approval button { margin-right: 8px; padding: 6px 16px; }
  #inputbar { display: flex; gap: 8px; padding: 10px 12px; border-top: 1px solid #9994; }
  #input { flex: 1; padding: 8px; }
  #board { width: 320px; border-left: 1px solid #9994; padding: 12px; overflow-y: auto; }
  #board h3 { margin: 0 0 8px; font-size: 14px; }
  #board-body { white-space: pre-wrap; font: 13px/1.5 ui-monospace, monospace; color: inherit; }
</style>
</head>
<body>
  <div id="left">
    <div id="log"></div>
    <div id="approval">
      <h3>⏸️ 需要审批</h3>
      <div id="approval-info"></div>
      <button id="btn-approve">批准</button>
      <button id="btn-reject">拒绝</button>
    </div>
    <div id="inputbar">
      <input id="input" placeholder="说点什么（回车发送）" autocomplete="off">
      <button id="send">发送</button>
    </div>
  </div>
  <div id="board">
    <h3>📋 任务看板</h3>
    <pre id="board-body">（暂无）</pre>
  </div>
<script>
// 会话 id 存 localStorage：刷新页面不丢上下文（服务端进程内存，重启即丢，《10》§4.4）。
const sessionId = localStorage.yktakoSession || (localStorage.yktakoSession = crypto.randomUUID());
const log = document.getElementById('log');
const boardBody = document.getElementById('board-body');
const approvalBox = document.getElementById('approval');
const approvalInfo = document.getElementById('approval-info');
const input = document.getElementById('input');
let busy = false;
let pendingApproval = null;

function add(cls, text) {
  const div = document.createElement('div');
  div.className = 'msg ' + cls;
  div.textContent = text;
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
}

function showApproval(a) {
  pendingApproval = a.id;
  approvalInfo.textContent = '工具：' + (a.tool || '（未标注）') + '\n参数：' + (a.args || '') + (a.message ? '\n' + a.message : '');
  approvalBox.style.display = 'block';
}

function render(ev) {
  switch (ev.kind) {
    case 'message': add('assistant', '🐙 ' + ev.text); break;
    case 'tool_call': add('meta', '  → 🔧 ' + (ev.toolNames || []).join(', ')); break;
    case 'tool_result': add('meta', '  ← 🔧 ' + ev.toolName + ': ' + ev.text); break;
    case 'board': boardBody.textContent = ev.text; break;
    case 'approval': showApproval(ev.approval); break;
    case 'error': add('error', '出错了：' + ev.text); break;
  }
}

async function decide(decision) {
  if (!pendingApproval) return;
  const requestId = pendingApproval;
  pendingApproval = null;
  approvalBox.style.display = 'none';
  await fetch('/api/approval', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId, requestId, decision }),
  });
}

document.getElementById('btn-approve').onclick = () => decide('approve');
document.getElementById('btn-reject').onclick = () => decide('reject');

async function send() {
  const message = input.value.trim();
  if (!message || busy) return;
  input.value = '';
  busy = true;
  add('user', '你：' + message);
  try {
    const resp = await fetch('/api/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessionId, message }),
    });
    if (!resp.ok) {
      add('error', '请求失败：' + resp.status + ' ' + await resp.text());
      return;
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        const line = frame.split('\n').find(l => l.startsWith('data: '));
        if (line) {
          try { render(JSON.parse(line.slice(6))); } catch (e) { console.error(e); }
        }
      }
    }
  } catch (e) {
    add('error', '连接断了：' + e);
  } finally {
    busy = false;
    pendingApproval = null;
    approvalBox.style.display = 'none';
  }
}

document.getElementById('send').onclick = send;
input.addEventListener('keydown', e => { if (e.key === 'Enter') send(); });
</script>
</body>
</html>
```

- [ ] **Step 4: `server.go` 注册根路由**

`New` 里在 chat 路由之前加：

```go
	mux.HandleFunc("GET /{$}", s.handleIndex)
```

（`{$}` 是 Go 1.22+ 的精确匹配：只匹配 `/` 本身，不吞掉其它路径。）

- [ ] **Step 5: 跑测试与静态检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./internal/webui/ -race -v`
Expected: 全部通过。

- [ ] **Step 6: 提交**

```bash
git add yk-tako-eino/internal/webui/page.go yk-tako-eino/internal/webui/static/index.html yk-tako-eino/internal/webui/server.go yk-tako-eino/internal/webui/server_test.go
git commit -m "webui：go:embed 单页（聊天流 + 审批卡片 + 看板面板）"
```

---

## Task 4: 入口接线（`-web` 开关 + agent 工厂）

**Files:**
- Modify: `yk-tako-eino/cmd/yktako/main.go`（全文替换，见下）

- [ ] **Step 1: 替换 `main.go`**

`yk-tako-eino/cmd/yktako/main.go` 新全文：

```go
// yk-tako 唯一入口：裸 ReAct + 任务看板（taskboard）+ L1 三件套守卫。
//
// 组装（doc/01 §5）：ChatModelAgent 的 Handlers 链 = [taskboard] + guard 三件套；
// 看板正本在 taskboard middleware 里跨轮存活，镜像随 interrupt 落 checkpoint。
// 两个入口：默认 REPL；-web 起最小 Web UI（internal/webui），共用同一套审批语义。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/taskboard"
	"yk-tako-eino/internal/tools"
	"yk-tako-eino/internal/webui"
)

// main 建模型、工具、看板与守卫，进 REPL 循环；-web 时改成起 Web 服务。
// 审批两个入口共用 interrupt/resume：REPL 读 stdin，Web 挂起等 POST（《10》§4.3）。
func main() {
	ctx := context.Background()
	webAddr := flag.String("web", "", "启动最小 Web UI 的监听地址（如 127.0.0.1:8765），留空进 REPL")
	flag.Parse()

	chatModel, ref := config.MustOpen(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		repl.Exitf("%v", err)
	}
	bash, err := tools.Bash(ctx)
	if err != nil {
		repl.Exitf("bash: %v", err)
	}
	files, err := tools.Files(ctx)
	if err != nil {
		repl.Exitf("files: %v", err)
	}
	all := append(tools.MustBasic(ctx), bash)
	all = append(all, files...)

	engine := guard.NewEngine(guard.ModeApprove, nil)

	// newAgent 造一套会话 agent。看板正本挂在 taskboard middleware 实例上，
	// 所以每个会话要造一份——共用会把不同会话的账本串在一起；模型与守卫引擎共享。
	newAgent := func(ctx context.Context) (adk.Agent, error) {
		boardMW, err := taskboard.New(taskboard.NewStore())
		if err != nil {
			return nil, fmt.Errorf("taskboard: %w", err)
		}
		handlers := append([]adk.ChatModelAgentMiddleware{boardMW}, guard.Handlers(engine, guard.Options{WorkspaceRoot: cwd})...)
		return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Name:        config.AgentName(),
			Description: "带任务看板与工具安全层的工作伙伴",
			Instruction: config.SystemPrompt() + "\n\n" + taskboard.Instruction,
			Model:       chatModel,
			ToolsConfig: adk.ToolsConfig{
				// all 是静态组装的 5 个基础工具；taskboard 的 4 个记账工具由
				// middleware 的 BeforeAgent 每轮动态注入 runCtx.Tools（运行时合计 9 个）。
				// ExecuteSequentially：同批工具调用串行（秩序偏好，不是正确性保证，见 doc/01 §4）。
				ToolsNodeConfig: compose.ToolsNodeConfig{Tools: all, ExecuteSequentially: true},
			},
			Handlers:      handlers,
			MaxIterations: 20,
		})
	}

	// Web 模式：单用户、无鉴权，只该绑本机地址（《10》§5 范围冻结）。
	if *webAddr != "" {
		srv := webui.New(ctx, webui.Config{NewAgent: newAgent})
		fmt.Printf("yk-tako Web UI: http://%s（Ctrl-C 退出）\n", *webAddr)
		if err := http.ListenAndServe(*webAddr, srv); err != nil {
			repl.Exitf("web: %v", err)
		}
		return
	}

	agent, err := newAgent(ctx)
	if err != nil {
		repl.Exitf("创建 agent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
		CheckPointStore: adkrun.NewStore(),
	})

	// 呈现归消费方：REPL sink 写 stdout；root 从 agent 自己取，与 adkrun.Run 同源。
	sink := newREPLSink(os.Stdout, agent.Name(ctx))

	console := repl.New()
	var history []*schema.Message
	turn := 0
	fmt.Printf("yk-tako。模型 %s。\n", ref)
	fmt.Printf("ToolGuard 模式: %s（/mode 可切换）。输入 quit 退出。\n\n", engine.Mode())
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		if mode, ok := strings.CutPrefix(input, "/mode "); ok {
			switch guard.Mode(strings.TrimSpace(mode)) {
			case guard.ModeBlock, guard.ModeWarn, guard.ModeApprove:
				engine.Configure(guard.Mode(strings.TrimSpace(mode)))
				fmt.Printf("ToolGuard 模式已切换为: %s\n\n", engine.Mode())
			default:
				fmt.Println("用法: /mode block|warn|require_approval")
			}
			continue
		}
		turn++
		messages := append(append([]*schema.Message{}, history...), schema.UserMessage(input))
		produced, err := adkrun.Run(ctx, runner, agent, fmt.Sprintf("turn-%d", turn), messages, sink, func(info guard.ApprovalInfo) (string, error) {
			fmt.Println("\n⏸️  ═══════════════ 人工审批（interrupt）═══════════════")
			fmt.Printf("工具: %s   参数: %s\n", info.Tool, info.Args)
			if info.Message != "" {
				fmt.Println(info.Message)
			}
			answer, ok := console.ReadLine("批准执行？[y/N] ")
			if !ok {
				return guard.ResumeReject, nil
			}
			answer = strings.ToLower(answer)
			if answer == "y" || answer == "yes" {
				fmt.Println("✅ 已批准，继续执行")
				fmt.Println()
				return guard.ResumeApprove, nil
			}
			fmt.Println("🚫 已拒绝")
			fmt.Println()
			return guard.ResumeReject, nil
		})
		if err != nil {
			fmt.Println("出错了:", err)
			continue
		}
		history = append(history, schema.UserMessage(input))
		history = append(history, produced...)
	}
}
```

- [ ] **Step 2: 全量检查**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./... -race`
Expected: 全绿（REPL 行为不变；`sink.go`/`main.go` 同包编译通过）。

- [ ] **Step 3: 提交**

```bash
git add yk-tako-eino/cmd/yktako/main.go
git commit -m "yktako：-web 启动最小 Web UI；agent 按会话隔离看板"
```

---

## Task 5 (T2.5): 验收关口

**Files:** 无（发现问题回到对应 Task 修）

- [ ] **Step 1: 机器验收（《10》§6 关口 3/4）**

```bash
cd yk-tako-eino
gofmt -l .
go build ./...
go vet ./...
go test ./... -race -count=1
```

Expected: `gofmt` 无输出；全部测试通过（`internal/webui` 的 4 个测试覆盖 SSE 序列、409 先于 header、审批批准/拒绝、页面挂点）。

- [ ] **Step 2: 人工判定（浏览器，需要 `.env` 模型 key）**

```bash
cd yk-tako-eino && go run ./cmd/yktako -web 127.0.0.1:8765
```

浏览器开 `http://127.0.0.1:8765`，确认四项：

1. 聊天流：发一句多步任务（例：`先查现在时间，再算 1234*5678，最后写个总结`），工具行（`→ 🔧` / `← 🔧`）与正文（🐙）逐事件出现，不是跑完一次性显示；
2. 看板面板：右侧出现 `📋 任务看板` 投影（与终端 REPL 同款文本）；
3. 审批卡片：发一个会命中命令规则的请求（例：`用 bash 执行 rm -rf /tmp/yktako-ui-test`，guard 默认 require_approval），卡片弹出工具与参数；点「拒绝」→ 模型收到拒绝说明继续收尾；再来一次点「批准」→ 命令真的执行；
4. REPL 回归：另开终端 `go run ./cmd/yktako`，多步任务输出与 T1 验收一致（y/n 审批仍在）。

- [ ] **Step 3: 记录验收结论**（写进 Task 6 的台账日志；跳过人工项要写原因）

---

## Task 6: 文档同步与台账

**Files:**
- Modify: `README.md`（根：能力段 + 快速开始）
- Modify: `yk-tako-eino/README.md`（目录 + 用法）
- Modify: `doc/01-任务看板与单入口.md`（§5 单入口结构补 Web 分支）
- Modify: `doc/05-Octop架构剖析与生长式复刻.md`（§4 路线表第 8 行）
- Modify: `doc/06-工具安全层.md`（§8 审批缺口补更新行）
- Modify: `doc/10-最小Web-UI.md`（§7 加更新行）
- Modify: `doc/11-最小Web-UI实施计划.md`（T2 状态、§4 子步骤、§6 日志）

- [ ] **Step 1: 根 `README.md`**

- 能力段加一条：`- **本地 Web UI**：\`cmd/yktako -web\` 起 HTTP + SSE 单页（聊天流 + 审批卡片 + 看板面板），审批走 interrupt/resume 的 POST 回执（定案见 \`doc/10\`，台账见 \`doc/11\`）。`
- 快速开始代码块加一行：`go run ./cmd/yktako -web 127.0.0.1:8765   # 最小 Web UI（只绑本机）`

- [ ] **Step 2: `yk-tako-eino/README.md`**

- 目录块补一行：`internal/webui    HTTP + SSE 最小 Web UI：会话表、审批挂起、embed 单页`
- 如有用法段，补 `-web` 说明（只绑 `127.0.0.1`，单用户无鉴权）。

- [ ] **Step 3: `doc/01` §5 单入口结构**

在结构图/文字里补 Web 分支一句，例如：`另有 \`-web\` 模式：同一套 agent 工厂按会话新建（会话表在 internal/webui，审批从 stdin 改为挂起等 POST，见《11》T2）。`

- [ ] **Step 4: `doc/05` §4 路线表第 8 行**

现状 `🔶 进行中——Web 入口主体（HTTP + SSE + HITL 审批结构）由《10》提前拉动；IM / cron 未动`
改为 `🔶 Web 入口主体已落地（HTTP + SSE + 审批 POST 回执，《11》T2）；IM / cron 未动`。

- [ ] **Step 5: `doc/06` §8 审批缺口**

在「审批是 REPL 的 y/n……等多入口统一路由那一层再对齐」处**保留原文**，追加更新行：`> 更新（2026-09-26）：Web 入口落地后审批已通道化——ask 挂起等 POST /api/approval，resume 值仍是 "approve"/"reject"；REPL 的 y/n 保留为另一入口（见《11》T2.3）。`

- [ ] **Step 6: `doc/10` §7 更新行**

在「逐任务实现计划仿《02》另起一篇」相关更新后，追加：`- **更新（2026-09-26）**：T2（HTTP + SSE 最小 UI）已落地，见《11》§1/§6 与 docs/superpowers/plans/2026-09-26-web-ui-http-sse.md。`

- [ ] **Step 7: `doc/11` 台账**

- §1：T2 行改 ✅（带 commits 与日期）；
- §4：按执行的子步骤勾选并补「落点」列（`internal/webui`）+ 指计划文件；
- §6：倒序追加 T2 日志（日期 | T2 | 各 commit | 摘要，含 Task 5 验收结论）。

- [ ] **Step 8: 提交**

```bash
git add README.md yk-tako-eino/README.md doc/01-任务看板与单入口.md doc/05-Octop架构剖析与生长式复刻.md doc/06-工具安全层.md doc/10-最小Web-UI.md doc/11-最小Web-UI实施计划.md
git commit -m "doc：T2 完成——Web 入口 README、单入口结构与台账同步"
```

---

## 不在本计划范围（防范围膨胀，依据《10》§5）

- 鉴权/RBAC/多用户；token 级 delta（第二期跟 #7）；断线重连与事件回放；跨实例（无 redis）；
- steer 运行中插话；前端构建链（Vue/Vite 等）；Wails 桌面壳；#4 上下文管理；
- 会话表清理/过期限流（第一期进程内不清理）；SSE keepalive 心跳（第一期内网本机够用）；
- 503 类服务级拒绝（WeKnora「409/503 顺序」的另一半）：第一期只有会话级 409，服务级拒绝不做；
- 「正文 + 工具调用同帧」的前言不显示：T1 口径把这种消息归为 `tool_call` 且不带 Text，单页与 REPL 行为一致；要显示前言属于事件契约扩展，留后续。
