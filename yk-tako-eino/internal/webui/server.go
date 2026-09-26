// 本文件：HTTP 装配——路由、会话表、/api/chat 的 SSE 响应与冲突检查。
package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/cloudwego/eino/adk"

	"yk-tako-eino/internal/guard"
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
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("POST /api/approval", s.handleApproval)
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
