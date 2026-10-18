package runengine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newStreamRouter(t *testing.T, s *Store) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := &Server{store: s, w: 200}
	r := gin.New()
	r.GET("/api/runs/:runId/stream", srv.getRunStream)
	return r
}

// doStream 打一次流。lastEventID 空 = 不带头；ctx 非 nil 时挂到请求上（心跳/取消测用）。
func doStream(t *testing.T, r *gin.Engine, runID, lastEventID string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/runs/"+runID+"/stream", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// 1. 已定稿 run 的存量重放：start/delta/terminal 各一个 event 块，data = 帧 JSON 原文，
// terminal 是最后一块，其后零字节（服务端关连接）。
func TestRunStreamReplaysTerminalRun(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "你好")
	if _, _, _, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	body := doStream(t, newStreamRouter(t, s), string(rid), "", nil).Body.String()
	wantStart := "id: 1\nevent: frame\ndata: {\"eventOffset\":1,\"kind\":\"start\",\"agent\":\"\",\"model\":\"\",\"messageId\":\"m_1\"}\n\n"
	wantDelta := "id: 2\nevent: frame\ndata: {\"eventOffset\":2,\"kind\":\"delta\",\"agent\":\"\",\"model\":\"\",\"text\":\"你好\"}\n\n"
	wantTerminal := "id: 3\nevent: frame\ndata: {\"eventOffset\":3,\"kind\":\"terminal\",\"agent\":\"\",\"model\":\"\",\"status\":\"complete\"}\n\n"
	if !strings.Contains(body, wantStart) {
		t.Errorf("缺 start 块，body =\n%s", body)
	}
	if !strings.Contains(body, wantDelta) {
		t.Errorf("缺 delta 块，body =\n%s", body)
	}
	if !strings.HasSuffix(body, wantTerminal) {
		t.Errorf("terminal 不是最后一块（其后应零字节），body =\n%s", body)
	}
}

// 2. runlive 键不存在（从未存在与 TTL 过期同语义）→ 一条 gone 事件后关闭。
func TestRunStreamGone(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	rec := doStream(t, newStreamRouter(t, s), "r_never", "", nil)
	if got := rec.Body.String(); got != "event: gone\n\n" {
		t.Errorf("body = %q, want %q", got, "event: gone\n\n")
	}
}

// 3. Last-Event-ID 续传：从 offset > N 继续（首块 id=N+1），N 之前的帧不重放；
// 非法值与缺省同宽容 = 0（全量重放）。
func TestRunStreamLastEventID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "一")
	s.mustDelta(t, ctx, rid, "二")
	if _, _, _, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	r := newStreamRouter(t, s)

	body := doStream(t, r, string(rid), "2", nil).Body.String()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Errorf("Last-Event-ID=2 不应重放前两帧，body =\n%s", body)
	}
	if !strings.HasPrefix(body, "id: 3\nevent: frame\n") {
		t.Errorf("首块应是 id: 3，body =\n%s", body)
	}
	if !strings.Contains(body, "\"kind\":\"terminal\"") {
		t.Errorf("应推到 terminal 收尾，body =\n%s", body)
	}

	if got := doStream(t, r, string(rid), "abc", nil).Body.String(); !strings.Contains(got, "id: 1\n") {
		t.Errorf("非法 Last-Event-ID 应按 0 全量重放，body =\n%s", got)
	}
}

// 4. 心跳：静默期（无 terminal、无新帧）按间隔写出 `: ping` 注释帧；
// 请求 ctx 取消（客户端断开）时循环退出。
func TestRunStreamHeartbeat(t *testing.T) {
	oldTail, oldBeat := sseTailInterval, sseHeartbeatInterval
	sseTailInterval, sseHeartbeatInterval = 20*time.Millisecond, 40*time.Millisecond
	defer func() { sseTailInterval, sseHeartbeatInterval = oldTail, oldBeat }()

	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	body := doStream(t, newStreamRouter(t, s), string(rid), "", ctx).Body.String()
	if n := strings.Count(body, ": ping\n\n"); n < 2 {
		t.Errorf("200ms 静默期 : ping 出现 %d 次, want >= 2；body =\n%s", n, body)
	}
}
