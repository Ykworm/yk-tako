package runengine

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 存储层不变量的回归网（A4-2）。curl 打不到的三类：`gone` 的 TTL 到期、Lua 的跨键原子性、
// Reset 的前缀隔离。dev 运行时用真 Redis 6390，miniredis 只服务本文件。
//
// A7 改这一层时，1/4/8 三条对 `tk:live` 闸的断言会按新语义变红——那时是**同拍改网**，不是改坏了。

const testTTL = 300 * time.Second

func newTestStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	s := NewRedisStore(mr.Addr(), testTTL)
	t.Cleanup(func() {
		s.rdb.Close()
		mr.Close()
	})
	return s, mr
}

func (s *RedisStore) mustOK(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	n, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	if n != 1 {
		t.Fatalf("EXISTS %s = %d, want 1", key, n)
	}
}

func (s *RedisStore) mustGone(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	n, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	if n != 0 {
		t.Fatalf("EXISTS %s = %d, want 0", key, n)
	}
}

func (s *RedisStore) llen(t *testing.T, ctx context.Context, key string) int {
	t.Helper()
	n, err := s.rdb.LLen(ctx, key).Result()
	if err != nil && err != redis.Nil {
		t.Fatalf("LLEN %s: %v", key, err)
	}
	return int(n)
}

// lindex 取 LIST 里的**原文**（未经 Go 侧反序列化），用来钉住落盘字节。
func (s *RedisStore) lindex(t *testing.T, ctx context.Context, key string, i int) string {
	t.Helper()
	v, err := s.rdb.LIndex(ctx, key, int64(i)).Result()
	if err != nil {
		t.Fatalf("LINDEX %s %d: %v", key, i, err)
	}
	return v
}

func (s *RedisStore) mustAcquire(t *testing.T, sid SessionID, rid RunID, msgID, text string) {
	t.Helper()
	ok, err := s.AcquireRun(context.Background(), sid, rid, Row{RunID: rid, Role: "user", MessageID: msgID, Text: text})
	if err != nil {
		t.Fatalf("AcquireRun(%s, %s): %v", sid, rid, err)
	}
	if !ok {
		t.Fatalf("AcquireRun(%s, %s) = false, want true (会话不应忙)", sid, rid)
	}
}

// 1. 连抢两次闸：第二次 false，且**一个键都不写**（doc/19 §1 POST 事务的原子性）。
func TestAcquireRunGateBlocksSecondRun(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, r1, r2 := SessionID("s_1"), RunID("r_1"), RunID("r_2")

	s.mustAcquire(t, sid, r1, "m_1", "hi")
	ok, err := s.AcquireRun(ctx, sid, r2, Row{RunID: r2, Role: "user", MessageID: "m_2", Text:"second"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("第二次抢闸应失败（会话忙 = 调用方回 409）")
	}

	if got := s.llen(t, ctx, keyRunlog(sid)); got != 1 {
		t.Errorf("runlog LLEN = %d, want 1（忙时不应写入第二条 user 记录）", got)
	}
	if got := s.llen(t, ctx, keyRunlive(r1)); got != 1 {
		t.Errorf("runlive(r1) LLEN = %d, want 1", got)
	}
	s.mustGone(t, ctx, keyRunlive(r2))
	s.mustGone(t, ctx, keyDraft(r2))
	if got := s.llen(t, ctx, keyModelHistory(sid)); got != 1 {
		t.Errorf("mh LLEN = %d, want 1", got)
	}
	live, _, _, err := s.SessionStatus(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if live != r1 {
		t.Errorf("tk:live = %q, want %q", live, r1)
	}
}

// 2. start 帧的 offset 由 LIST index 注入；from 越界得空批且 next 不前进。
func TestReadFramesStartFrameAndBeyondEnd(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")

	frames, next, exists, err := s.ReadFrames(ctx, rid, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("runlive 刚建，exists 应为 true")
	}
	if len(frames) != 1 || frames[0].EventOffset != 1 || frames[0].Kind != "start" {
		t.Fatalf("ReadFrames(0) = %+v, want 1 帧 {eventOffset:1 kind:start}", frames)
	}
	if next != 1 {
		t.Errorf("nextOffset = %d, want 1", next)
	}

	frames, next, exists, err = s.ReadFrames(ctx, rid, 999, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || len(frames) != 0 || next != 999 {
		t.Fatalf("ReadFrames(999) = (%v, %+v, next=%d), want (exists:true, 空批, next:999)", exists, frames, next)
	}
}

// 3. delta 追加后从上一游标续读，offset 连续。
func TestAppendDeltaThenRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")

	for i, want := range []int{2, 3} {
		n, ok, err := s.AppendDelta(ctx, rid, "你好")
		if err != nil || !ok {
			t.Fatalf("AppendDelta #%d: ok=%v err=%v", i, ok, err)
		}
		if n != want {
			t.Errorf("AppendDelta #%d eventOffset = %d, want %d", i, n, want)
		}
	}

	frames, next, _, err := s.ReadFrames(ctx, rid, 1, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].EventOffset != 2 || frames[1].EventOffset != 3 || next != 3 {
		t.Fatalf("ReadFrames(1) = %+v next=%d, want offset 2,3 next=3", frames, next)
	}
}

// 4. EndRun(complete) 的一笔原子写：放闸、删 draft、定稿进 runlog + mh、末帧 terminal、runlive 设 TTL。
func TestEndRunCompleteFinalizes(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "Hello, ")
	s.mustDelta(t, ctx, rid, "world")

	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "complete")
	if err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if eventOffset != 4 {
		t.Errorf("eventOffset = %d, want 4（start+2 delta+terminal）", eventOffset)
	}
	if runlogOffset != 2 {
		t.Errorf("runlogOffset = %d, want 2", runlogOffset)
	}

	s.mustGone(t, ctx, keyLive(sid))
	s.mustGone(t, ctx, keyDraft(rid))
	if got := s.llen(t, ctx, keyRunlog(sid)); got != 2 {
		t.Fatalf("runlog LLEN = %d, want 2", got)
	}
	if got := s.llen(t, ctx, keyModelHistory(sid)); got != 2 {
		t.Errorf("mh LLEN = %d, want 2", got)
	}
	mh, err := s.ModelMessages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(mh) != 2 || mh[1].Role != "assistant" || mh[1].Text != "Hello, world" {
		t.Errorf("ModelMessages = %+v, want 第二条 assistant 且全文为两次 delta 拼接", mh)
	}

	last, _, exists, err := s.ReadFrames(ctx, rid, 3, 200)
	if err != nil || !exists {
		t.Fatalf("定稿后 runlive 键应还在（done ≠ gone）: exists=%v err=%v", exists, err)
	}
	if len(last) != 1 || last[0].Kind != "terminal" || last[0].Status != "complete" {
		t.Errorf("末帧 = %+v, want {kind:terminal status:complete}", last)
	}
	ttl, err := s.rdb.TTL(ctx, keyRunlive(rid)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Errorf("TTL(runlive) = %v, want > 0（doc/19 §7 终态设 TTL）", ttl)
	}
}

func (s *RedisStore) mustDelta(t *testing.T, ctx context.Context, rid RunID, text string) {
	t.Helper()
	if _, ok, err := s.AppendDelta(ctx, rid, text); err != nil || !ok {
		t.Fatalf("AppendDelta(%s): ok=%v err=%v", rid, ok, err)
	}
}

// 5. TTL 到期才是 gone：ReadFrames 返 exists=false（handler 据此回 status:"gone"，不是 404）。
func TestReadFramesGoneAfterTTL(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete"); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	if _, _, exists, err := s.ReadFrames(ctx, rid, 0, 200); err != nil || !exists {
		t.Fatalf("TTL 未到期前应可读: exists=%v err=%v", exists, err)
	}

	mr.FastForward(testTTL + time.Second)
	frames, next, exists, err := s.ReadFrames(ctx, rid, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("TTL 到期后 exists 应为 false")
	}
	if len(frames) != 0 || next != 0 {
		t.Errorf("gone 时 = (%+v, next=%d), want (空批, next=0)", frames, next)
	}
}

// 6. 落盘原文不含 offset：offset 只在读时由 index+1 注入（写侧无计数器，多副本并发追加不错号）。
// 逐字节钉住存储形态 = 顺带钉住字段序，字段序是契约（读出来的字节要被客户端按名取）。
func TestStoredJSONCarriesNoOffset(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete"); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	wantFrames := []string{
		`{"kind":"start","messageId":"m_1"}`,
		`{"kind":"delta","text":"world"}`,
		`{"kind":"terminal","status":"complete"}`,
	}
	for i, want := range wantFrames {
		if got := s.lindex(t, ctx, keyRunlive(rid), i); got != want {
			t.Errorf("runlive[%d] = %s, want %s", i, got, want)
		}
	}
	wantLog := []string{
		`{"runId":"r_1","role":"user","messageId":"m_1","text":"hi"}`,
		`{"runId":"r_1","role":"assistant","text":"world"}`,
	}
	for i, want := range wantLog {
		if got := s.lindex(t, ctx, keyRunlog(sid), i); got != want {
			t.Errorf("runlog[%d] = %s, want %s", i, got, want)
		}
	}
	if got := s.lindex(t, ctx, keyModelHistory(sid), 1); got != `{"role":"assistant","text":"world"}` {
		t.Errorf("mh[1] = %s", got)
	}
}

// 7. 批上限 W 是一条规则、两个拉取端点共用（doc/19 §7：LRANGE from..from+W-1）。
func TestBatchWindowCapsBothEndpoints(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid := SessionID("s_1")

	// 造 5 条 runlog：两轮完整 run（每轮 user+assistant）+ 第三轮的 user。
	for _, rid := range []RunID{"r_1", "r_2"} {
		s.mustAcquire(t, sid, rid, "m_u", "q")
		s.mustDelta(t, ctx, rid, "a")
		if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete"); err != nil || !ok {
			t.Fatalf("EndRun %s: ok=%v err=%v", rid, ok, err)
		}
	}
	rid3 := RunID("r_open")
	s.mustAcquire(t, sid, rid3, "m_u3", "q3")
	if got := s.llen(t, ctx, keyRunlog(sid)); got != 5 {
		t.Fatalf("runlog LLEN = %d, want 5", got)
	}

	for _, c := range []struct {
		from, wantLen, wantNext int
	}{
		{0, 3, 3}, // 截断：续拉即补齐
		{3, 2, 5},
		{5, 0, 5}, // 空批 next 不动
		{1, 3, 4}, // 往回拨（翻页游标）
	} {
		rows, next, exists, err := s.ReadLog(ctx, sid, c.from, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("ReadLog(%d): exists = false", c.from)
		}
		if len(rows) != c.wantLen || next != c.wantNext {
			t.Errorf("ReadLog(from=%d, w=3) = %d 条 next=%d, want %d 条 next=%d",
				c.from, len(rows), next, c.wantLen, c.wantNext)
			continue
		}
		for i, r := range rows {
			if want := c.from + i + 1; r.RunlogOffset != want {
				t.Errorf("ReadLog(from=%d) 第 %d 行 offset = %d, want %d", c.from, i, r.RunlogOffset, want)
			}
		}
	}

	// 同一条规则也管 runlive：start+2 delta+terminal = 4 帧，W=1 时每批一帧。
	s.mustDelta(t, ctx, rid3, "d1")
	s.mustDelta(t, ctx, rid3, "d2")
	if _, _, ok, err := s.EndRun(ctx, sid, rid3, "complete"); err != nil || !ok {
		t.Fatalf("EndRun %s: ok=%v err=%v", rid3, ok, err)
	}
	for _, c := range []struct {
		from, wantLen, wantNext int
	}{
		{0, 1, 1},
		{1, 1, 2},
		{3, 1, 4}, // 末帧 terminal：offset 4 落在 index 3
	} {
		frames, next, _, err := s.ReadFrames(ctx, rid3, c.from, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != c.wantLen || next != c.wantNext {
			t.Errorf("ReadFrames(from=%d, w=1) = %d 帧 next=%d, want %d 帧 next=%d",
				c.from, len(frames), next, c.wantLen, c.wantNext)
		}
	}
}

// 8. 闸已放之后再调 EndRun：返 false 且**不双写** runlog（A7 那条论断的存储级证据）。
func TestEndRunAfterGateLostIsNoop(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete"); err != nil || !ok {
		t.Fatalf("首次 EndRun: ok=%v err=%v", ok, err)
	}
	before := s.llen(t, ctx, keyRunlog(sid))

	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("闸已不属于本 run 时 EndRun 应空转（返 false）")
	}
	if eventOffset != 0 || runlogOffset != 0 {
		t.Errorf("空转时返回值 = (%d, %d), want (0, 0)", eventOffset, runlogOffset)
	}
	if after := s.llen(t, ctx, keyRunlog(sid)); after != before {
		t.Errorf("runlog LLEN %d → %d，want 不变（否则两行 assistant）", before, after)
	}
}

// 9. Reset 只删 tk:* —— 本机 6379 上真跑着别的项目的活 Redis，这条不是假想需求。
func TestResetOnlyDeletesOwnPrefix(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")
	if err := s.rdb.Set(ctx, "other:x", "not mine", 0).Err(); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.Reset(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 6 {
		t.Errorf("Reset deleted = %d, want 6（session/live/runlog/runlive/draft/mh）", deleted)
	}
	keys, _, err := s.rdb.Scan(ctx, 0, keyPrefix+"*", 500).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("reset 后仍有 tk: 键: %v", keys)
	}
	s.mustOK(t, ctx, "other:x")

	if _, _, exists, err := s.SessionStatus(ctx, sid); err != nil || exists {
		t.Errorf("reset 后 session 应不存在: exists=%v err=%v", exists, err)
	}
}
