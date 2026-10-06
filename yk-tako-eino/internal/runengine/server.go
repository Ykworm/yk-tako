package runengine

import (
	"context"
	"crypto/rand"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// Server 把存储层接到 v6 的 API 面上（doc/19 §0 端点表）。
// w 是 frames / log 两个拉取端点共用的单批上限（doc/19 §0「拉取一律有界」，-batch-window 传入）。
type Server struct {
	store  *RedisStore
	w      int
	worker *Worker
}

func NewRouter(store *RedisStore, batchWindow int, dev bool, worker *Worker) *gin.Engine {
	s := &Server{store: store, w: batchWindow, worker: worker}
	r := gin.Default()
	r.POST("/api/runs", s.postRuns)
	r.GET("/api/sessions/:id", s.getSession)
	r.GET("/api/runs/:runId/frames", s.getFrames)
	r.GET("/api/sessions/:id/log", s.getSessionLog)
	if dev {
		r.DELETE("/api/dev/reset", s.devReset)
	}
	return r
}

type postRunsRequest struct {
	SessionID SessionID `json:"sessionId"`
	MessageID string    `json:"messageId"`
	Text      string    `json:"text"`
}

// 字段序 = doc/19 §1 期望字节序：sessionId → runId → messageId。
type postRunsResponse struct {
	SessionID SessionID `json:"sessionId"`
	RunID     RunID     `json:"runId"`
	MessageID string    `json:"messageId"`
}

type sessionResponse struct {
	LiveRunID    RunID `json:"liveRunId"`
	RunlogOffset int   `json:"runlogOffset"`
}

// 字段序 = doc/19 §2/§3 期望字节序。
type framesResponse struct {
	Frames     []Frame `json:"frames"`
	NextOffset int     `json:"nextOffset"`
	Status     string  `json:"status"`
}

type logResponse struct {
	Rows       []Row `json:"rows"`
	NextOffset int   `json:"nextOffset"`
}

func (s *Server) postRuns(c *gin.Context) {
	ctx := c.Request.Context()
	var req postRunsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return
	}
	sessionID := req.SessionID
	if sessionID != "" {
		exists, err := s.store.Exists(ctx, sessionID)
		if err != nil {
			internal(c, err)
			return
		}
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
			return
		}
	}
	runID, err := newRunID()
	if err != nil {
		internal(c, err)
		return
	}
	if sessionID == "" {
		if sessionID, err = newSessionID(); err != nil {
			internal(c, err)
			return
		}
	}
	user := Row{RunID: runID, Role: "user", MessageID: req.MessageID, Text: req.Text}
	acquired, err := s.store.AcquireRun(ctx, sessionID, runID, user)
	if err != nil {
		internal(c, err)
		return
	}
	if !acquired {
		c.JSON(http.StatusConflict, gin.H{"error": "session_busy"})
		return
	}
	// 抢闸成功才开 worker：POST 返回 202 时 run 已在生成，frames/log 立刻可拉（doc/19 §1）。
	// Background 而非请求 ctx：run 的生命周期不属于这个请求，响应写完它还在跑。
	go s.worker.Run(context.Background(), sessionID, runID)
	c.JSON(http.StatusAccepted, postRunsResponse{SessionID: sessionID, RunID: runID, MessageID: req.MessageID})
}

func (s *Server) getSession(c *gin.Context) {
	liveRunID, runlogOffset, exists, err := s.store.SessionStatus(c.Request.Context(), SessionID(c.Param("id")))
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
		return
	}
	c.JSON(http.StatusOK, sessionResponse{LiveRunID: liveRunID, RunlogOffset: runlogOffset})
}

// getFrames（doc/19 §2）：status 三态只靠「键存在性 + 本批是否含 terminal」推出，无额外状态字段。
// run 不存在 → gone（200，不 404——「从未存在」与「已过期删除」故意不区分，客户端都转拉 log）。
func (s *Server) getFrames(c *gin.Context) {
	from := queryOffset(c, "eventOffset")
	frames, next, exists, err := s.store.ReadFrames(c.Request.Context(), RunID(c.Param("runId")), from, s.w)
	if err != nil {
		internal(c, err)
		return
	}
	status := "generating"
	if !exists {
		status = "gone"
	} else {
		for _, f := range frames {
			if f.Kind == "terminal" {
				status = "done"
				break
			}
		}
	}
	c.JSON(http.StatusOK, framesResponse{Frames: frames, NextOffset: next, Status: status})
}

// getSessionLog（doc/19 §3）：session 未登记 → 404。
func (s *Server) getSessionLog(c *gin.Context) {
	from := queryOffset(c, "runlogOffset")
	rows, next, exists, err := s.store.ReadLog(c.Request.Context(), SessionID(c.Param("id")), from, s.w)
	if err != nil {
		internal(c, err)
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "session_not_found"})
		return
	}
	c.JSON(http.StatusOK, logResponse{Rows: rows, NextOffset: next})
}

// internal 是存储 / ID 生成故障的统一出口：客户端拿 doc/19 §1 的 500 字节，原因只进服务端日志。
func internal(c *gin.Context, err error) {
	log.Printf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
}

// queryOffset 解析拉取游标：缺省 / 非法 / 负数一律视为 0（契约只规定缺省=0，不发明新错误码）。
func queryOffset(c *gin.Context, name string) int {
	n, err := strconv.Atoi(c.Query(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// devReset 是 dev-only 全局归零（planning §4 脚手架表：长期 dev 工具，只在 dev 装配注册）。
func (s *Server) devReset(c *gin.Context) {
	deleted, err := s.store.Reset(c.Request.Context())
	if err != nil {
		internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// newSessionID / newRunID 生成 s_/r_ 前缀 + ULID。rand 失败返回 error（调用方回 500），不 panic。
func newSessionID() (SessionID, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return "s_" + SessionID(id.String()), nil
}

func newRunID() (RunID, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return "r_" + RunID(id.String()), nil
}
