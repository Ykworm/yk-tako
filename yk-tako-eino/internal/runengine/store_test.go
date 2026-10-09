package runengine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 存储层不变量的回归网（A4-2 / A7 / A7-2 / D1）。curl 打不到的三类原样在：`gone` 的 TTL 到期、
// 跨键/跨存储写的事务性、Reset 的前缀隔离；A7 的闸租约与 A7-2 的出生窗口改按 DB 形态断言
// （闸互斥 = INSERT 冲突、死行清扫、lease 更新影响行数、过期不可复活）。
//
// 跑在真实例上（2026-10-07 立项裁定：miniredis 退役）：Redis 用真 6390 的**独立 DB index**（db1，
// 不碰 dev 的 db0）、PG 用真 5433 的**临时库**（每测试建/拆一个 tako_test_<ULID>）。
// 前置：PG 5433 与 Redis 6390 都已起（dev.sh 或手动）；环境不在时本文件大声失败，不静默跳过。
// 拨不了钟就把钟调小：TTL 断言从 FastForward 改为「短租约 + 轮询等待」。
const (
	testRedisURL   = "redis://127.0.0.1:6390/1"
	testPGAdminDSN = "host=127.0.0.1 port=5433 dbname=postgres sslmode=disable"
	testLease      = 30 * time.Second
	testTTL        = 300 * time.Second
)

func testPGDSN(db string) string {
	return fmt.Sprintf("host=127.0.0.1 port=5433 dbname=%s sslmode=disable", db)
}

// newTestStore 起一套隔离存储：临时 PG 库（AutoMigrate 四表）+ Redis db1（清掉上一轮 tk:* 残留）。
// cleanup（LIFO 无关，本函数内显式排序）：先关 Store 连接、再 DROP 库。
func newTestStore(t *testing.T, leaseTTL, ttl time.Duration) *Store {
	t.Helper()
	ctx := context.Background()
	rowID, err := newRowID("t")
	if err != nil {
		t.Fatal(err)
	}
	dbName := "tako_test_" + strings.ToLower(rowID)
	admin, err := gorm.Open(postgres.Open(testPGAdminDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf("连 5433 失败（PG 需先起着）: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE " + dbName).Error; err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}
	store, err := OpenStore(ctx, testRedisURL, testPGDSN(dbName), ttl, leaseTTL)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		if err := admin.Exec("DROP DATABASE " + dbName + " WITH (FORCE)").Error; err != nil {
			t.Errorf("DROP DATABASE %s: %v", dbName, err)
		}
		if sqlDB, err := admin.DB(); err == nil {
			sqlDB.Close()
		}
	})
	store.resetRedisKeys(t)
	return store
}

// resetRedisKeys 清 db1 的 tk:* 键（不 FLUSHDB——db1 理论上只有我们，但按前缀纪律行事）。
func (s *Store) resetRedisKeys(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, keyPrefix+"*", 500).Result()
		if err != nil {
			t.Fatalf("SCAN tk:*: %v", err)
		}
		if len(keys) > 0 {
			if err := s.rdb.Unlink(ctx, keys...).Err(); err != nil {
				t.Fatalf("UNLINK: %v", err)
			}
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

func (s *Store) mustOK(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	n, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	if n != 1 {
		t.Fatalf("EXISTS %s = %d, want 1", key, n)
	}
}

func (s *Store) mustGone(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	n, err := s.rdb.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	if n != 0 {
		t.Fatalf("EXISTS %s = %d, want 0", key, n)
	}
}

func (s *Store) llen(t *testing.T, ctx context.Context, key string) int {
	t.Helper()
	n, err := s.rdb.LLen(ctx, key).Result()
	if err != nil {
		t.Fatalf("LLEN %s: %v", key, err)
	}
	return int(n)
}

// lindex 取 LIST 里的**原文**（未经 Go 侧反序列化），用来钉住落盘字节。
func (s *Store) lindex(t *testing.T, ctx context.Context, key string, i int) string {
	t.Helper()
	v, err := s.rdb.LIndex(ctx, key, int64(i)).Result()
	if err != nil {
		t.Fatalf("LINDEX %s %d: %v", key, i, err)
	}
	return v
}

func (s *Store) ttlOf(t *testing.T, ctx context.Context, key string) time.Duration {
	t.Helper()
	d, err := s.rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL %s: %v", key, err)
	}
	return d
}

// count 数一张表的行（where 为空则全表）。
func (s *Store) count(t *testing.T, model any, where string, args ...any) int64 {
	t.Helper()
	q := s.db.Model(model)
	if where != "" {
		q = q.Where(where, args...)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("COUNT %T (%s): %v", model, where, err)
	}
	return n
}

// sqlBool 跑一条返回单布尔的探针 SQL（行形状断言用），失败即 Fatal。
func (s *Store) sqlBool(t *testing.T, query string, args ...any) bool {
	t.Helper()
	var b bool
	if err := s.db.Raw(query, args...).Row().Scan(&b); err != nil {
		t.Fatalf("SQL %s: %v", query, err)
	}
	return b
}

// waitCond 轮询条件直到成立或超时——真钟下代替 miniredis 的 FastForward。
func waitCond(t *testing.T, desc string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时（%v）: %s", timeout, desc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Store) mustAcquire(t *testing.T, sid SessionID, rid RunID, msgID, text string) {
	t.Helper()
	ok, err := s.AcquireRun(context.Background(), sid, rid, Row{RunID: rid, Role: "user", MessageID: msgID, Text: text})
	if err != nil {
		t.Fatalf("AcquireRun(%s, %s): %v", sid, rid, err)
	}
	if !ok {
		t.Fatalf("AcquireRun(%s, %s) = false, want true (会话不应忙)", sid, rid)
	}
}

func (s *Store) mustDelta(t *testing.T, ctx context.Context, rid RunID, text string) {
	t.Helper()
	if _, ok, err := s.AppendDelta(ctx, rid, text); err != nil || !ok {
		t.Fatalf("AppendDelta(%s): ok=%v err=%v", rid, ok, err)
	}
}

// 1. 连抢两次闸：第二次 false，且**两半都不写**（19v6-01 §1 POST 事务的原子性：PG 事务持有跨 Redis 写）。
// 闸互斥的证据 = runs INSERT 撞 one_live_run_per_session（D1 形态）。
func TestAcquireRunGateBlocksSecondRun(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, r1, r2 := SessionID("s_1"), RunID("r_1"), RunID("r_2")

	s.mustAcquire(t, sid, r1, "m_1", "hi")
	ok, err := s.AcquireRun(ctx, sid, r2, Row{RunID: r2, Role: "user", MessageID: "m_2", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("第二次抢闸应失败（唯一索引冲突 = 调用方回 409）")
	}

	// PG 半零写入：会话 1 行、run 只 1 行、mh 只 1 行、runlog 只 1 行。
	if got := s.count(t, &Session{}, "id = ?", sid); got != 1 {
		t.Errorf("sessions 行数 = %d, want 1", got)
	}
	if got := s.count(t, &Run{}, "session_id = ?", sid); got != 1 {
		t.Errorf("runs 行数 = %d, want 1（忙时不得为 r2 建行）", got)
	}
	if got := s.count(t, &RunModelMessage{}, "session_id = ?", sid); got != 1 {
		t.Errorf("mh 行数 = %d, want 1", got)
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 1 {
		t.Errorf("runlog 行数 = %d, want 1（忙时不应写入第二条 user 记录）", got)
	}
	// Redis 半零写入：r1 的 start 帧在，r2 两键都不存在。
	if got := s.llen(t, ctx, keyRunlive(r1)); got != 1 {
		t.Errorf("runlive(r1) LLEN = %d, want 1", got)
	}
	s.mustGone(t, ctx, keyRunlive(r2))
	s.mustGone(t, ctx, keyDraft(r2))
	// 闸落点 = runs 活行。
	live, _, _, err := s.SessionStatus(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if live != r1 {
		t.Errorf("liveRunId = %q, want %q", live, r1)
	}
}

// 2. start 帧的 offset 由 LIST index 注入；from 越界得空批且 next 不前进。
func TestReadFramesStartFrameAndBeyondEnd(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
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
	s := newTestStore(t, testLease, testTTL)
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

// 4. EndRun(complete) 的一笔 PG 事务：放闸 + 定稿（mh seq2 / runlog assistant 同事务）；
// Redis 半在 commit 后：末帧 terminal + runlive 设 TTL + 删 draft。
func TestEndRunCompleteFinalizes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "Hello, ")
	s.mustDelta(t, ctx, rid, "world")

	// T2 起 mh 的 assistant 半来自 worker 累积的 turn（不再由 EndRun 从 draft 合成）：测试给合成 turn 走同一路。
	turn := []TurnEntry{{Role: "assistant", Text: "Hello, world"}}
	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, turn)
	if err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if eventOffset != 4 {
		t.Errorf("eventOffset = %d, want 4（start+2 delta+terminal）", eventOffset)
	}
	if runlogOffset != 2 {
		t.Errorf("runlogOffset = %d, want 2", runlogOffset)
	}

	// PG 半：放闸 = 活行翻终态（status/ finished_at / lease_until 三处同笔）。
	if !s.sqlBool(t, `SELECT (status = 'complete' AND finished_at IS NOT NULL AND lease_until IS NULL) FROM runs WHERE id = ?`, rid) {
		t.Error("runs 行应为 complete + finished_at + lease_until NULL")
	}
	// 定稿两行与现役契约同序：mh seq2 = assistant 全文（两次 delta 拼接）。
	var mhText string
	if err := s.db.Raw(`SELECT text FROM run_model_messages WHERE run_id = ? AND seq = 2`, rid).Row().Scan(&mhText); err != nil {
		t.Fatal(err)
	}
	if mhText != "Hello, world" {
		t.Errorf("mh seq2 text = %q, want %q", mhText, "Hello, world")
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 2 {
		t.Errorf("runlog 行数 = %d, want 2", got)
	}
	// Redis 半：draft 已删；runlive 键还在（done ≠ gone）且带 TTL。
	s.mustGone(t, ctx, keyDraft(rid))
	last, _, exists, err := s.ReadFrames(ctx, rid, 3, 200)
	if err != nil || !exists {
		t.Fatalf("定稿后 runlive 键应还在（done ≠ gone）: exists=%v err=%v", exists, err)
	}
	if len(last) != 1 || last[0].Kind != "terminal" || last[0].Status != "complete" {
		t.Errorf("末帧 = %+v, want {kind:terminal status:complete}", last)
	}
	if ttl := s.ttlOf(t, ctx, keyRunlive(rid)); ttl <= 0 {
		t.Errorf("TTL(runlive) = %v, want > 0（19v6-03「生命周期」）", ttl)
	}
}

// 5. TTL 到期才是 gone：ReadFrames 返 exists=false（handler 据此回 status:"gone"，不是 404）；
// 同一时刻 PG 的 runlog 完好（历史真源不受临时态蒸发影响）。
func TestReadFramesGoneAfterTTL(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, 2*time.Second)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	if _, _, exists, err := s.ReadFrames(ctx, rid, 0, 200); err != nil || !exists {
		t.Fatalf("TTL 未到期前应可读: exists=%v err=%v", exists, err)
	}

	waitCond(t, "runlive 随 TTL 蒸发", 8*time.Second, func() bool {
		n, err := s.rdb.Exists(ctx, keyRunlive(rid)).Result()
		if err != nil {
			t.Fatalf("EXISTS: %v", err)
		}
		return n == 0
	})
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
	rows, _, ok, err := s.ReadLog(ctx, sid, 0, 200)
	if err != nil || !ok || len(rows) != 2 {
		t.Errorf("runlive 蒸发后 runlog 应完好: ok=%v rows=%d err=%v", ok, len(rows), err)
	}
}

// 6. 落盘原文：runlive 的帧 JSON 不含 offset（读时按 index+1 注入，写侧无计数器、多副本并发不重号），
// 且恒带 agent（T2，空 = root）；PG 侧钉行形状——assistant 行 message_id 为 NULL（契约：assistant 无此字段）、
// user 行带 m_1、agent/payload 缺省为 NULL。
// 逐字节钉存储形态顺带钉字段序（读出来的字节要被客户端按名取）；重头判据在 curl baseline（A4-1 重跑）。
func TestStoredShapeCarriesNoOffset(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	wantFrames := []string{
		`{"kind":"start","agent":"","messageId":"m_1"}`,
		`{"kind":"delta","agent":"","text":"world"}`,
		`{"kind":"terminal","agent":"","status":"complete"}`,
	}
	for i, want := range wantFrames {
		if got := s.lindex(t, ctx, keyRunlive(rid), i); got != want {
			t.Errorf("runlive[%d] = %s, want %s", i, got, want)
		}
	}

	if !s.sqlBool(t, `SELECT message_id IS NULL FROM runlog_entries WHERE run_id = ? AND role = 'assistant'`, rid) {
		t.Error("runlog assistant 行的 message_id 应为 NULL")
	}
	if !s.sqlBool(t, `SELECT message_id = 'm_1' FROM runlog_entries WHERE run_id = ? AND role = 'user'`, rid) {
		t.Error("runlog user 行的 message_id 应为 m_1")
	}
	// T2 两列同理走「缺省即 NULL」约定（读侧归一到 "" 对外）：本拍 agent 恒空、payload 只有扩展行有。
	if !s.sqlBool(t, `SELECT bool_and(agent IS NULL AND payload IS NULL) FROM runlog_entries WHERE run_id = ?`, rid) {
		t.Error("user/assistant 行 agent/payload 应为 NULL（缺省不写字面空串）")
	}
}

// 7. 批上限 W 是一条规则、两个拉取端点共用（19v6-01 §0「拉取一律有界」）——log 侧换成 PG 的
// `seq > from ORDER BY seq LIMIT w`，语义与旧 LRANGE 逐字相同（含往回拨）。
func TestBatchWindowCapsBothEndpoints(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid := SessionID("s_1")

	// 造 5 条 runlog：两轮完整 run（每轮 user+assistant）+ 第三轮的 user。
	for _, rid := range []RunID{"r_1", "r_2"} {
		s.mustAcquire(t, sid, rid, "m_u", "q")
		s.mustDelta(t, ctx, rid, "a")
		if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil); err != nil || !ok {
			t.Fatalf("EndRun %s: ok=%v err=%v", rid, ok, err)
		}
	}
	rid3 := RunID("r_open")
	s.mustAcquire(t, sid, rid3, "m_u3", "q3")
	if got := s.count(t, &RunlogEntry{}, "session_id = ?", sid); got != 5 {
		t.Fatalf("runlog 行数 = %d, want 5", got)
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
	if _, _, ok, err := s.EndRun(ctx, sid, rid3, "complete", nil, nil); err != nil || !ok {
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

// 8. 闸已放之后再调 EndRun：返 false 且**不双写** runlog（A7 那条论断的存储级证据；D1 形态的
// 守卫 = 统一谓词「闸在手上」的 UPDATE 影响 0 行）。
func TestEndRunAfterGateLostIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil); err != nil || !ok {
		t.Fatalf("首次 EndRun: ok=%v err=%v", ok, err)
	}
	before := s.count(t, &RunlogEntry{}, "session_id = ?", sid)

	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("闸已不属于本 run 时 EndRun 应空转（返 false）")
	}
	if eventOffset != 0 || runlogOffset != 0 {
		t.Errorf("空转时返回值 = (%d, %d), want (0, 0)", eventOffset, runlogOffset)
	}
	if after := s.count(t, &RunlogEntry{}, "session_id = ?", sid); after != before {
		t.Errorf("runlog 行数 %d → %d，want 不变（否则两行 assistant）", before, after)
	}
}

// 9. Reset 两半都清、且只删自己的东西：PG 四表空、db1 的 tk:* 键清空，而邻居键（非 tk: 前缀）完好——
// 本机 6390 上真跑着别的键空间，这条不是假想需求。
func TestResetClearsBothHalvesOnlyOwnPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
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
	if deleted != 2 {
		t.Errorf("Reset deleted = %d, want 2（runlive + draft 两键）", deleted)
	}
	for _, m := range []any{&Session{}, &Run{}, &RunModelMessage{}, &RunlogEntry{}} {
		if got := s.count(t, m, ""); got != 0 {
			t.Errorf("reset 后 %T 行数 = %d, want 0", m, got)
		}
	}
	keys, _, err := s.rdb.Scan(ctx, 0, keyPrefix+"*", 500).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("reset 后仍有 tk: 键: %v", keys)
	}
	s.mustOK(t, ctx, "other:x")
	if err := s.rdb.Del(ctx, "other:x").Err(); err != nil {
		t.Fatal(err)
	}

	if _, _, exists, err := s.SessionStatus(ctx, sid); err != nil || exists {
		t.Errorf("reset 后 session 应不存在: exists=%v err=%v", exists, err)
	}
}

// 10. 闸租约全链（A7 / A7-2 的 DB 形态）：出生即带（Redis 两键 TTL + runs.lease_until 同笔）；
// 续约两半同动（lease_until 前进 + 两键 TTL 刷回）；零续约越过租约 → 两键蒸发、读侧即回收
// （liveRunId 空、不等清扫）、再抢闸 202（清扫翻死行）；同 session 两次抢闸 = 真复活。
func TestGateLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	lease := 1 * time.Second
	s := newTestStore(t, lease, 2*time.Second)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	// 出生即带租约：Redis 两键 TTL > 0；PG 侧 lease_until 在 (now(), now()+2s) 窗口内（DB 时钟，now() 事务语义）。
	for _, k := range []string{keyDraft(rid), keyRunlive(rid)} {
		if ttl := s.ttlOf(t, ctx, k); ttl <= 0 {
			t.Fatalf("抢闸后 %s 应带租约 TTL, got %v", k, ttl)
		}
	}
	if !s.sqlBool(t, `SELECT (lease_until > now() AND lease_until <= now() + interval '2 seconds') FROM runs WHERE id = ?`, rid) {
		t.Fatal("抢闸后 lease_until 应落在 (now(), now()+2s]——出生即带且走 DB 时钟")
	}

	// 续约一拍：PG 半 lease_until 前进；Redis 半两键 TTL 仍在。
	var before, after time.Time
	if err := s.db.Raw(`SELECT lease_until FROM runs WHERE id = ?`, rid).Row().Scan(&before); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RenewLease(ctx, sid, rid); err != nil || !ok {
		t.Fatalf("RenewLease: ok=%v err=%v", ok, err)
	}
	if err := s.db.Raw(`SELECT lease_until FROM runs WHERE id = ?`, rid).Row().Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.After(before) {
		t.Errorf("续约后 lease_until 应前进: %v → %v", before, after)
	}
	for _, k := range []string{keyDraft(rid), keyRunlive(rid)} {
		if ttl := s.ttlOf(t, ctx, k); ttl <= 0 {
			t.Errorf("续约后 %s 应带租约 TTL, got %v", k, ttl)
		}
	}

	// 零续约越过租约：两键蒸发（等真钟）。
	waitCond(t, "两键随租约蒸发", 8*time.Second, func() bool {
		n1, err1 := s.rdb.Exists(ctx, keyDraft(rid)).Result()
		n2, err2 := s.rdb.Exists(ctx, keyRunlive(rid)).Result()
		if err1 != nil || err2 != nil {
			t.Fatalf("EXISTS: %v %v", err1, err2)
		}
		return n1 == 0 && n2 == 0
	})
	// 读侧即回收：活行还没被清扫，但 lease 已过 → liveRunId 空。
	waitCond(t, "lease 过期（读侧判死）", 8*time.Second, func() bool {
		return s.sqlBool(t, `SELECT lease_until < now() FROM runs WHERE id = ?`, rid)
	})
	live, runlogOffset, exists, err := s.SessionStatus(ctx, sid)
	if err != nil || !exists {
		t.Fatalf("SessionStatus: exists=%v err=%v", exists, err)
	}
	if live != "" {
		t.Errorf("lease 过期后 liveRunId = %q, want \"\"（读侧即回收）", live)
	}
	if runlogOffset != 1 {
		t.Errorf("runlogOffset = %d, want 1（死 run 只留 user 行）", runlogOffset)
	}
	if !s.sqlBool(t, `SELECT (status = 'running' AND lease_until < now()) FROM runs WHERE id = ?`, rid) {
		t.Error("清扫前死行应仍挂 running（清扫只为放行新 INSERT，不保显示正确性）")
	}

	// 再抢闸：不是 409——清扫把死行翻 error，唯一索引放行。
	r2 := RunID("r_2")
	ok, err := s.AcquireRun(ctx, sid, r2, Row{RunID: r2, Role: "user", MessageID: "m_2", Text: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("闸随租约到期自开后，同 session 应能重新抢闸")
	}
	if !s.sqlBool(t, `SELECT (status = 'error' AND finished_at IS NOT NULL AND lease_until IS NULL) FROM runs WHERE id = ?`, rid) {
		t.Error("死行应被清扫成 error（finished_at 非空、lease_until 清空）")
	}
	if live, _, _, err := s.SessionStatus(ctx, sid); err != nil || live != r2 {
		t.Errorf("重抢后 liveRunId = %q, want %q (err=%v)", live, r2, err)
	}
}

// 11. 易主 no-op（A7 CAS 的 DB 等价物）：行被翻离 running 后，老 run 的 watchdog 续约
// UPDATE 影响 0 行 → 不续、也不改写两键的 TTL（PERSIST 掉 TTL 作探针：被误刷会重新带上）。
func TestRenewLeaseNoopWhenGateLost(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")
	if ok, err := s.RenewLease(ctx, sid, rid); err != nil || !ok {
		t.Fatalf("首次续约（闸属本 run）应成功: ok=%v err=%v", ok, err)
	}

	// 易主/放闸的等价物：行翻离 running（status 与 lease_until 双双失效）。
	if err := s.db.Exec(`UPDATE runs SET status = 'error', lease_until = NULL WHERE id = ?`, rid).Error; err != nil {
		t.Fatal(err)
	}
	// 摘掉两键 TTL——续约若被误执行（PEXPIRE）会把 TTL 刷回来，no-op 则保持 -1。
	for _, k := range []string{keyDraft(rid), keyRunlive(rid)} {
		if err := s.rdb.Persist(ctx, k).Err(); err != nil {
			t.Fatal(err)
		}
	}

	ok, err := s.RenewLease(ctx, sid, rid)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("闸已不在本 run，续约应 no-op（返 false）")
	}
	for _, k := range []string{keyDraft(rid), keyRunlive(rid)} {
		s.mustOK(t, ctx, k) // 没被删
		if ttl := s.ttlOf(t, ctx, k); ttl >= 0 {
			t.Errorf("续约 no-op 不应改写 %s 的 TTL: %v", k, ttl)
		}
	}
}

// 12. 出生窗口（A7-2 的 DB 形态）：进程死在「抢闸 → 首次续约」之间（一次续约都没做成）时，
// 两键也随租约一起蒸发、闸自开——修前 draft/runlive 出生不带 TTL，这个窗口里死掉会永久泄漏。
func TestGateLeaseBirthWindowExpiresWithoutRenew(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, 1*time.Second, 2*time.Second)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	// 零续约（= 进程死在首次续约前），直接等过租约。
	waitCond(t, "两键随租约蒸发（零续约）", 8*time.Second, func() bool {
		n1, err1 := s.rdb.Exists(ctx, keyDraft(rid)).Result()
		n2, err2 := s.rdb.Exists(ctx, keyRunlive(rid)).Result()
		if err1 != nil || err2 != nil {
			t.Fatalf("EXISTS: %v %v", err1, err2)
		}
		return n1 == 0 && n2 == 0
	})

	// 闸随租约自开：同 session 再抢成功。
	r2 := RunID("r_2")
	ok, err := s.AcquireRun(ctx, sid, r2, Row{RunID: r2, Role: "user", MessageID: "m_2", Text: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("出生窗口死亡后闸应随租约自开，同 session 应能重新抢闸")
	}
}

// 13. 过期不可复活：lease 已过但行还挂着 running（没人清扫过）时，续约也必须拒——
// 「lease_until > now()」是续约守卫的另一半；少了它，迟到续约会复活死 run，而 draft 已蒸发，
// 收尾会拿空文本定稿出「complete 但空回复」的形状。
func TestRenewLeaseRefusesExpiredGate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, 1*time.Second, 2*time.Second)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	waitCond(t, "lease 过期（行仍 running、未清扫）", 8*time.Second, func() bool {
		return s.sqlBool(t, `SELECT (status = 'running' AND lease_until < now()) FROM runs WHERE id = ?`, rid)
	})

	ok, err := s.RenewLease(ctx, sid, rid)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("lease 已过，续约应拒（过期不可复活）")
	}
	if live, _, _, err := s.SessionStatus(ctx, sid); err != nil || live != "" {
		t.Errorf("过期后 liveRunId = %q, want \"\" (err=%v)", live, err)
	}
}

// 14. POST 跨存储事务的失败支（§8.4 第一行）：Redis 半失败 → 整笔 ROLLBACK → 500 零痕迹，
// 与 409 同形「什么都没写」。用「共享同库 db、连到死端口的 rdb」复刻 Redis 掉线（runliveStart 必败）。
func TestAcquireRunRollsBackWhenRedisFails(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = deadRdb.Close() })
	bad := &Store{db: s.db, rdb: deadRdb, ttl: s.ttl, leaseTTL: s.leaseTTL}

	sid, rid := SessionID("s_1"), RunID("r_1")
	ok, err := bad.AcquireRun(ctx, sid, rid, Row{RunID: rid, Role: "user", MessageID: "m_1", Text: "hi"})
	if err == nil {
		t.Fatal("Redis 半不可达时 AcquireRun 应报错（调用方回 500）")
	}
	if ok {
		t.Fatal("Redis 半失败时不可能是成功抢闸")
	}
	for _, m := range []any{&Session{}, &Run{}, &RunModelMessage{}, &RunlogEntry{}} {
		if got := s.count(t, m, ""); got != 0 {
			t.Errorf("回滚后 %T 行数 = %d, want 0", m, got)
		}
	}
}

// 15. 过期不可复活（EndRun 侧，test 13 的孪生）：lease 已过、draft 已蒸发后，迟到的
// EndRun(complete) 必须空转——少了守卫会拿空 draft 定稿出「complete 但空回复」的形状。
func TestEndRunRefusesExpiredGate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, 1*time.Second, 2*time.Second)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "world")

	waitCond(t, "lease 过期且 draft 蒸发", 8*time.Second, func() bool {
		n, err := s.rdb.Exists(ctx, keyDraft(rid)).Result()
		if err != nil {
			t.Fatalf("EXISTS: %v", err)
		}
		return n == 0 && s.sqlBool(t, `SELECT lease_until < now() FROM runs WHERE id = ?`, rid)
	})

	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("lease 已过，EndRun 应空转（过期不可复活）")
	}
	if eventOffset != 0 || runlogOffset != 0 {
		t.Errorf("空转时返回值 = (%d, %d), want (0, 0)", eventOffset, runlogOffset)
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ? AND role = 'assistant'", sid); got != 0 {
		t.Errorf("assistant 行数 = %d, want 0（空 draft 不得定稿）", got)
	}
	if !s.sqlBool(t, `SELECT (status = 'running' AND lease_until IS NOT NULL) FROM runs WHERE id = ?`, rid) {
		t.Error("空转的 EndRun 不应动行（清扫归下次 AcquireRun）")
	}
}

// 16. EndRun(error) 的形状（kill -9 冒烟 ⓒ 的活体孪生）：放闸 + terminal 帧，但**不定稿**——
// runlog 无 assistant 行、mh 只留 user 半、runlogOffset = 当前高水位（旧 Lua 的 LLEN 语义）。
func TestEndRunErrorKeepsLogUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "hi")
	s.mustDelta(t, ctx, rid, "partial...")

	eventOffset, runlogOffset, ok, err := s.EndRun(ctx, sid, rid, "error", nil, nil)
	if err != nil || !ok {
		t.Fatalf("EndRun(error): ok=%v err=%v", ok, err)
	}
	if eventOffset != 3 {
		t.Errorf("eventOffset = %d, want 3（start+delta+terminal）", eventOffset)
	}
	if runlogOffset != 1 {
		t.Errorf("runlogOffset = %d, want 1（高水位只含 user 行）", runlogOffset)
	}
	if !s.sqlBool(t, `SELECT (status = 'error' AND finished_at IS NOT NULL AND lease_until IS NULL) FROM runs WHERE id = ?`, rid) {
		t.Error("runs 行应为 error + finished_at + lease_until NULL（放闸）")
	}
	if got := s.count(t, &RunlogEntry{}, "session_id = ? AND role = 'assistant'", sid); got != 0 {
		t.Errorf("assistant 行数 = %d, want 0（error 不定稿）", got)
	}
	if got := s.count(t, &RunModelMessage{}, "session_id = ?", sid); got != 1 {
		t.Errorf("mh 行数 = %d, want 1（只留 user 半）", got)
	}
	s.mustGone(t, ctx, keyDraft(rid))
	last, _, exists, err := s.ReadFrames(ctx, rid, 2, 200)
	if err != nil || !exists {
		t.Fatalf("收尾后 runlive 应还在: exists=%v err=%v", exists, err)
	}
	if len(last) != 1 || last[0].Kind != "terminal" || last[0].Status != "error" {
		t.Errorf("末帧 = %+v, want {kind:terminal status:error}", last)
	}
}

// 17. T2 的 turn 落库（tool loop 的存储级证据）：assistant 轮（带 tool_calls）→ tool 轮 → assistant 终轮——
// mh 逐轮落（seq 2/3/4；tool_calls/tool_call_id 两列按 OpenAI 工具协议回填，ModelMessages 原样读回 =
// prepareInput 回放链的实证），runlog 落 tool_call/tool_result 扩展行（text 可展示、payload 结构化）+ 终稿行。
func TestEndRunLandsTurnRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	sid, rid := SessionID("s_1"), RunID("r_1")
	s.mustAcquire(t, sid, rid, "m_1", "现在几点")
	s.mustDelta(t, ctx, rid, "我查一下")
	s.mustDelta(t, ctx, rid, "，现在是 15:04")

	call := schema.ToolCall{ID: "call_1", Type: "function",
		Function: schema.FunctionCall{Name: "get_time", Arguments: `{"timezone":"Asia/Shanghai"}`}}
	turn := []TurnEntry{
		{Role: "assistant", Text: "我查一下", ToolCalls: []schema.ToolCall{call}},
		{Role: "tool", Text: "2026-10-08 15:04:05 +0800 CST", ToolCallID: "call_1", ToolName: "get_time"},
		{Role: "assistant", Text: "，现在是 15:04"},
	}
	if _, _, ok, err := s.EndRun(ctx, sid, rid, "complete", nil, turn); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}

	// mh：4 行（user + 3 轮）；tool_calls 的 args 原样（模型原始 JSON 字串，不重编码）。
	mh, err := s.ModelMessages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(mh) != 4 {
		t.Fatalf("mh 行数 = %d, want 4", len(mh))
	}
	if mh[1].Role != "assistant" || len(mh[1].ToolCalls) != 1 ||
		mh[1].ToolCalls[0].ID != "call_1" || mh[1].ToolCalls[0].Function.Name != "get_time" ||
		mh[1].ToolCalls[0].Function.Arguments != `{"timezone":"Asia/Shanghai"}` {
		t.Errorf("mh[1] = %+v, want assistant + get_time 调用（args 原样）", mh[1])
	}
	if mh[2].Role != "tool" || mh[2].ToolCallID != "call_1" || mh[2].Text != "2026-10-08 15:04:05 +0800 CST" {
		t.Errorf("mh[2] = %+v, want tool + 回指 call_1", mh[2])
	}
	if mh[3].Role != "assistant" || mh[3].Text != "，现在是 15:04" {
		t.Errorf("mh[3] = %+v, want 终轮 assistant 本体", mh[3])
	}
	// 两列「缺席即 NULL」（存储形态级）：assistant 行无 tool_call_id、tool 行无 tool_calls、终轮两列皆空。
	if !s.sqlBool(t, `SELECT (tool_calls IS NOT NULL AND tool_call_id IS NULL) FROM run_model_messages WHERE run_id = ? AND seq = 2`, rid) {
		t.Error("seq2 assistant 行应为 tool_calls 非空 + tool_call_id NULL")
	}
	if !s.sqlBool(t, `SELECT (tool_calls IS NULL AND tool_call_id = 'call_1') FROM run_model_messages WHERE run_id = ? AND seq = 3`, rid) {
		t.Error("seq3 tool 行应为 tool_calls NULL + tool_call_id 回指")
	}
	if !s.sqlBool(t, `SELECT (tool_calls IS NULL AND tool_call_id IS NULL) FROM run_model_messages WHERE run_id = ? AND seq = 4`, rid) {
		t.Error("seq4 终轮 assistant 行两列皆 NULL（缺省不写字面空串）")
	}

	// runlog：user + tool_call + tool_result + assistant 四行；扩展行 payload 与帧字段一一对应。
	rows, _, exists, err := s.ReadLog(ctx, sid, 0, 200)
	if err != nil || !exists {
		t.Fatalf("ReadLog: exists=%v err=%v", exists, err)
	}
	if len(rows) != 4 {
		t.Fatalf("runlog 行数 = %d, want 4", len(rows))
	}
	if rows[1].Role != "tool_call" || rows[1].Text != `get_time({"timezone":"Asia/Shanghai"})` ||
		rows[1].Payload != `{"name":"get_time","args":"{\"timezone\":\"Asia/Shanghai\"}"}` {
		t.Errorf("rows[1] = %+v, want tool_call 扩展行（text 可展示 + payload 结构化）", rows[1])
	}
	if rows[2].Role != "tool_result" || rows[2].Text != "2026-10-08 15:04:05 +0800 CST" ||
		rows[2].Payload != `{"name":"get_time","output":"2026-10-08 15:04:05 +0800 CST"}` {
		t.Errorf("rows[2] = %+v, want tool_result 扩展行", rows[2])
	}
	if rows[3].Role != "assistant" || rows[3].Text != "我查一下，现在是 15:04" {
		t.Errorf("rows[3] = %+v, want 终稿 = Σ delta", rows[3])
	}
	for i, r := range rows {
		if r.Agent != "" {
			t.Errorf("rows[%d].agent = %q, want \"\"（本拍恒空，读侧从 NULL 归一）", i, r.Agent)
		}
	}
	if !s.sqlBool(t, `SELECT bool_and(agent IS NULL) FROM runlog_entries WHERE run_id = ? AND role IN ('tool_call','tool_result')`, rid) {
		t.Error("扩展行 agent 应为 NULL（缺省不写字面空串）")
	}
}
