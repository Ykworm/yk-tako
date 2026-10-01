// 本文件：HTTP 装配——路由、会话表、/api/chat 的 SSE 响应与冲突检查。
package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"

	"yk-tako-eino/internal/compact"
	"yk-tako-eino/internal/guard"
)

// Config 是装配参数。
type Config struct {
	// NewAgent 必须是「每调用一套新 agent」：taskboard 正本（看板）挂在 middleware
	// 实例上，共用会让不同会话的看板串账。生产由 cmd/yktako 提供，测试注入假模型。
	NewAgent func(ctx context.Context) (adk.Agent, error)
	// DBPath 是事件库文件。空字符串用内存库，只该出现在测试里；生产传 -web-db。
	DBPath string
}

// Server 是 webui 的 http.Handler：持有会话表、事件库与 agent 工厂。
type Server struct {
	ctx      context.Context // 基础设施生命周期：建 runner/agent 用，绝不能用请求 ctx（请求一断 runner 内部状态跟着没）
	newAgent func(ctx context.Context) (adk.Agent, error)
	db       *LogDB
	events   *eventMem // 页面读的事件列表；不因完成而删除
	engine   *gin.Engine

	mu       sync.Mutex
	sessions map[string]*session
}

// New 组装 Server 与路由。ctx 决定所有会话 runner 的生命周期（生产传进程 ctx）。
// 打开库之后先对账：上次没写完终局的 run 标 failed，已落盘的事件留着。
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.NewAgent == nil {
		return nil, fmt.Errorf("NewAgent 必填")
	}
	db, err := OpenLogDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if err := db.Reconcile(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Server{ctx: ctx, newAgent: cfg.NewAgent, db: db, events: newEventMem(), sessions: map[string]*session{}}
	engine := gin.New()
	engine.GET("/", s.handleIndex)
	engine.GET("/api/session", s.handleSession)
	engine.GET("/api/session/history", s.handleHistory)
	engine.POST("/api/chat", s.handleChat)
	engine.GET("/api/chat/stream", s.handleStream)
	engine.POST("/api/approval", s.handleApproval)
	engine.POST("/api/stop", s.handleStop)
	engine.POST("/api/chat/restore", s.handleRestore)
	s.engine = engine
	return s, nil
}

// Close 关闭事件库。监听退出时调用。
func (s *Server) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ServeHTTP 实现 net/http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.engine.ServeHTTP(w, r) }

// session 取或建会话；同一 id 的并发首访由 s.mu 串行。
func (s *Server) session(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	sess := newSession(s.ctx, id, s.db)
	agent, err := s.newAgent(compact.WithMemory(s.ctx, sess))
	if err != nil {
		return nil, fmt.Errorf("建会话 agent: %w", err)
	}
	sess.bindAgent(s.ctx, agent)
	sess.events = s.events
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
func (s *Server) handleChat(c *gin.Context) {
	w := c.Writer
	r := c.Request
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
	runID, err := newRunID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	turn, messages, runCtx, ok, err := sess.beginTurn(runID, req.Message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "该会话已有一轮在跑", http.StatusConflict)
		return
	}
	// start 在模型调用之前落盘。失败则放开闸，此时还没有 SSE 响应头。
	// 落库成功后放进内存列表，读者只读这份列表。
	start, err := sess.db.BeginRun(s.ctx, runID, req.SessionID, req.Message, sess.agent.Name(s.ctx))
	if err != nil {
		sess.abortTurn()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.events.publish(start)
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, _ = sess.db.Finish(s.ctx, runID, "error", "不支持流式响应", nil)
		sess.abortTurn()
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	// run 用会话 ctx。读者用请求 ctx 轮询；请求结束只是这个读者离开。
	go s.driveRun(sess, runCtx, turn, runID, messages)
	s.writeEvents(r.Context(), w, flusher, runID, 0)
}

// driveRun 把一轮跑完并写下唯一终局。不持有任何 HTTP 连接。
func (s *Server) driveRun(sess *session, runCtx context.Context, turn int, runID string, messages []*schema.Message) {
	produced, err := sess.runTurn(runCtx, turn, runID, messages)
	kind, text, producedJSON, _ := terminalOf(err, produced, sess.wasStopped())
	patch, patched := sess.archivePatch()
	var ev sseEvent
	var ferr error
	if kind == "complete" && patched {
		ev, ferr = sess.db.FinishWithPatch(s.ctx, runID, kind, text, producedJSON, patch)
	} else {
		if kind != "complete" {
			sess.revertStage()
		}
		ev, ferr = sess.db.Finish(s.ctx, runID, kind, text, producedJSON)
	}
	if ferr != nil {
		if kind == "complete" {
			sess.revertStage()
		}
	} else {
		if kind == "complete" {
			sess.noteCompleted(produced)
		}
		s.events.publish(ev)
	}
	sess.finishTurn()
}

// terminalOf 把这一轮的结局收成终局 kind、文案、成功时才有的产出 JSON，以及要不要写入模型历史。
// histErr 非 nil 时 finishTurn 不追加历史。用户停止是 stop；其它错误是 error；成功才是 complete。
func terminalOf(err error, produced []*schema.Message, stopped bool) (kind, text string, producedJSON []byte, histErr error) {
	// Run 已经成功返回时按完成记。停止若和正常结束撞在一起，这一轮的产出仍然算数。
	if err == nil {
		if produced == nil {
			produced = []*schema.Message{}
		}
		raw, merr := json.Marshal(produced)
		if merr != nil {
			return "error", merr.Error(), nil, merr
		}
		return "complete", "", raw, nil
	}
	if stopped {
		return "stop", "", nil, err
	}
	return "error", err.Error(), nil, err
}

// handleStream 按 run 与 from 续传。from 缺省或 0 表示从头（seq > 0）。
// 不创建会话，也不取消 run。run 不在档案里则 404。
func (s *Server) handleStream(c *gin.Context) {
	w := c.Writer
	r := c.Request
	runID := r.URL.Query().Get("run")
	if runID == "" {
		http.Error(w, "run 必填", http.StatusBadRequest)
		return
	}
	from, err := parseFrom(r.URL.Query().Get("from"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.db.lookupArchive(r.Context(), runID); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "run 不存在", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	s.writeEvents(r.Context(), w, flusher, runID, from)
}

// handleSession 告诉后来的页面这场会话有没有在跑的 run。未知会话不创建，liveRun 为空字符串。
func (s *Server) handleSession(c *gin.Context) {
	w := c.Writer
	r := c.Request
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id 必填", http.StatusBadRequest)
		return
	}
	live := ""
	s.mu.Lock()
	sess := s.sessions[id]
	s.mu.Unlock()
	if sess != nil {
		live = sess.liveID()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		SessionID string `json:"sessionId"`
		LiveRun   string `json:"liveRun"`
	}{SessionID: id, LiveRun: live})
}

// handleHistory 返回会话里已结束的 run，按 created_at 降序分页。
// 正在跑的 run 不在结果里——由 /api/session 报告，页面用 from=0 接上。
func (s *Server) handleHistory(c *gin.Context) {
	w := c.Writer
	r := c.Request
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "session 必填", http.StatusBadRequest)
		return
	}
	cursor := r.URL.Query().Get("before")
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 50 {
			http.Error(w, "limit 必须是 1-50 的整数", http.StatusBadRequest)
			return
		}
		limit = n
	}
	page, err := s.db.HistoryPage(r.Context(), sessionID, cursor, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// stopRequest 是 POST /api/stop 的请求体。sessionId 与 /api/chat 同一字段名。
type stopRequest struct {
	SessionID string `json:"sessionId"`
	Run       string `json:"run"`
}

// handleStop 核对 liveRun 后取消这场 run。不写日志。对不上则 404。
func (s *Server) handleStop(c *gin.Context) {
	w := c.Writer
	r := c.Request
	var req stopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.Run == "" {
		http.Error(w, "sessionId 与 run 必填", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sess := s.sessions[req.SessionID]
	s.mu.Unlock()
	if sess == nil || !sess.stop(req.Run) {
		http.Error(w, "没有这场 run", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// parseFrom 解析续传游标。空字符串与 0 都是从头。负数与非整数是 400。
func parseFrom(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("from 必须是非负整数")
	}
	return n, nil
}

// writeEvents 把 seq > from 的帧按顺序写给这个读者。读的是进程内列表；
// 列表还没有这场 run 时先从库装入。同一条还在变长的正文会随代数再推一次，
// 序号不变，游标也不前移。见到终局，或 run 已经不在 running 且没有新帧，就收流。
// 写失败只结束这个读者。
//
// 库调用用进程 ctx。请求一断就取消查询的话，modernc 会把文件锁挂满
// busy_timeout，正在落盘的那一帧跟着 SQLITE_BUSY 失败，这一轮被写成错误。
func (s *Server) writeEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, runID string, from int64) {
	cursor := from
	var seen uint64
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		batch, gen, notify, err := s.eventsAfter(s.ctx, runID, cursor, pollBatch)
		if err != nil {
			return
		}
		if gen != seen {
			seen = gen
			for {
				if len(batch) == 0 {
					break
				}
				advanced := false
				for _, ev := range batch {
					if !writeSSE(w, flusher, ev) {
						return
					}
					if ev.Failed {
						s.events.discardSeq(runID, ev.Seq)
					}
					if !ev.Partial && !ev.Failed {
						cursor = ev.Seq
						advanced = true
					}
					if isTerminal(ev.Kind) {
						return
					}
				}
				if !advanced {
					break
				}
				batch, gen, notify, err = s.eventsAfter(s.ctx, runID, cursor, pollBatch)
				if err != nil {
					return
				}
				seen = gen
			}
		}
		view, err := s.db.lookupArchive(s.ctx, runID)
		if err != nil {
			return
		}
		if view.Status != "running" {
			tail, _, _, err := s.eventsAfter(s.ctx, runID, cursor, pollBatch)
			if err != nil {
				return
			}
			for _, ev := range tail {
				if ev.Partial {
					continue
				}
				if !writeSSE(w, flusher, ev) {
					return
				}
				if ev.Failed {
					s.events.discardSeq(runID, ev.Seq)
				}
			}
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(pollInterval)
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-timer.C:
		}
	}
}

const (
	pollBatch    = 50
	pollInterval = 100 * time.Millisecond
)

// eventsAfter 从内存列表取 seq > from 的帧，并带上当前代数。
// 列表还不可读时，先把库里这场 run 全部装入。notify 在下一次变化时关闭。
func (s *Server) eventsAfter(ctx context.Context, runID string, from int64, limit int) ([]sseEvent, uint64, <-chan struct{}, error) {
	batch, gen, notify, ready := s.events.snapshot(runID, from, limit)
	if ready {
		return batch, gen, notify, nil
	}
	all, err := s.db.readAll(ctx, runID)
	if err != nil {
		return nil, 0, nil, err
	}
	s.events.install(runID, all)
	batch, gen, notify, _ = s.events.snapshot(runID, from, limit)
	return batch, gen, notify, nil
}

func isTerminal(kind string) bool {
	return kind == "complete" || kind == "error" || kind == "stop"
}

// restoreRequest 是浏览器补回一条已写完、但库里没有的正文。
type restoreRequest struct {
	SessionID string `json:"sessionId"`
	Run       string `json:"run"`
	Seq       int64  `json:"seq"`
	Text      string `json:"text"`
	Role      string `json:"role"`
	Agent     string `json:"agent"`
}

// handleRestore 在服务重启后把浏览器记下的正文补进库。
// 内存里已经有、库里已经有、或这轮还在跑，都不写。
func (s *Server) handleRestore(c *gin.Context) {
	w := c.Writer
	r := c.Request
	var req restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" || req.Run == "" || req.Seq <= 0 || req.Text == "" {
		http.Error(w, "sessionId、run、seq 与 text 必填", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.events.has(req.Run, req.Seq) {
		writeStored(w, false)
		return
	}
	ev := sseEvent{Run: req.Run, Seq: req.Seq, Kind: "message", Text: req.Text, Role: req.Role, Agent: req.Agent}
	stored, err := s.db.RestoreMessage(r.Context(), req.SessionID, ev)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if stored {
		s.events.publish(ev)
		s.mu.Lock()
		sess := s.sessions[req.SessionID]
		s.mu.Unlock()
		if sess != nil {
			sess.dropModelCache()
		}
	}
	writeStored(w, stored)
}

func writeStored(w http.ResponseWriter, stored bool) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Stored bool `json:"stored"`
	}{stored})
}

// approvalRequest 是 POST /api/approval 的请求体。
type approvalRequest struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"`
}

// handleApproval 把前端决定塞回挂起的 ask（《10》§4.3）。
// resume 值不变：只接受 guard.ResumeApprove / guard.ResumeReject。
func (s *Server) handleApproval(c *gin.Context) {
	w := c.Writer
	r := c.Request
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
