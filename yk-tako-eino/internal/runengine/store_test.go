package runengine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestStore(t *testing.T) (*Store, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewStore(rdb, 300*time.Second), rdb, mr
}

const (
	txnSession = "s_test"
	txnRun     = "r_test"
)

func acquire(t *testing.T, s *Store) {
	t.Helper()
	ok, err := s.AcquireRun(context.Background(), txnSession, txnRun, "m_1", "hi")
	if err != nil || !ok {
		t.Fatalf("AcquireRun = (%v, %v), want (true, nil)", ok, err)
	}
}

// 断言 1：闸抢两次，第二次 false 且什么都不写。
func TestStoreGateWritesNothingOnSecondAcquire(t *testing.T) {
	s, rdb, _ := newTestStore(t)
	ctx := context.Background()

	ok, _ := s.AcquireRun(ctx, txnSession, "r_1", "m_1", "hi")
	if !ok {
		t.Fatal("first AcquireRun = false, want true")
	}
	ok, _ = s.AcquireRun(ctx, txnSession, "r_2", "m_2", "急")
	if ok {
		t.Fatal("second AcquireRun = true, want false (gate held)")
	}
	if n := rdb.LLen(ctx, kRunlog(txnSession)).Val(); n != 1 {
		t.Fatalf("LLEN runlog = %d, want 1", n)
	}
	if n := rdb.LLen(ctx, kRunlive("r_1")).Val(); n != 1 {
		t.Fatalf("LLEN runlive:r_1 = %d, want 1", n)
	}
	if n := rdb.LLen(ctx, kRunlive("r_2")).Val(); n != 0 {
		t.Fatalf("LLEN runlive:r_2 = %d, want 0", n)
	}
	if v := rdb.Get(ctx, kLive(txnSession)).Val(); v != "r_1" {
		t.Fatalf("gate holder = %q, want r_1", v)
	}
}

// 断言 2：ReadLive from=0 得 start（offset 1）；from=999 得空批 + next=999。
func TestStoreReadLiveStartAndOutOfBounds(t *testing.T) {
	s, _, _ := newTestStore(t)
	acquire(t, s)

	frames, next, status, err := s.ReadLive(context.Background(), txnRun, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].EventOffset != 1 || frames[0].Kind != "start" || frames[0].MessageID != "m_1" {
		t.Fatalf("frames = %+v, want [{eventOffset:1 kind:start messageId:m_1}]", frames)
	}
	if next != 1 || status != "generating" {
		t.Fatalf("next/status = %d/%q, want 1/generating", next, status)
	}

	frames, next, status, err = s.ReadLive(context.Background(), txnRun, 999, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 || next != 999 || status != "generating" {
		t.Fatalf("OOB read = len %d next %d status %q, want 0/999/generating", len(frames), next, status)
	}
}

// 断言 3：两次 AppendDelta 后 from=1 得 offset 2、3，next=3。
func TestStoreAppendDeltaIncrement(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	acquire(t, s)
	if err := s.AppendDelta(ctx, txnRun, "好的"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDelta(ctx, txnRun, "，我来读"); err != nil {
		t.Fatal(err)
	}

	frames, next, status, err := s.ReadLive(ctx, txnRun, 1, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || frames[0].EventOffset != 2 || frames[0].Text != "好的" ||
		frames[1].EventOffset != 3 || frames[1].Text != "，我来读" {
		t.Fatalf("frames = %+v", frames)
	}
	if next != 3 || status != "generating" {
		t.Fatalf("next/status = %d/%q, want 3/generating", next, status)
	}
}

// 断言 4：定稿五件事——闸消失、assistant 行入 runlog 且逐字节等于 delta 拼接、
// runlive 末帧 terminal、TTL>0、draft 删除。
func TestStoreFinalize(t *testing.T) {
	s, rdb, _ := newTestStore(t)
	ctx := context.Background()
	acquire(t, s)
	_ = s.AppendDelta(ctx, txnRun, "好的")
	_ = s.AppendDelta(ctx, txnRun, "，我来读")

	if err := s.Finalize(ctx, txnSession, txnRun); err != nil {
		t.Fatal(err)
	}
	if rdb.Exists(ctx, kLive(txnSession)).Val() != 0 {
		t.Fatal("gate still exists after Finalize")
	}
	if rdb.Exists(ctx, kDraft(txnRun)).Val() != 0 {
		t.Fatal("draft still exists after Finalize")
	}
	if n := rdb.LLen(ctx, kRunlog(txnSession)).Val(); n != 2 {
		t.Fatalf("LLEN runlog = %d, want 2", n)
	}
	var assistant storedRow
	if err := json.Unmarshal([]byte(rdb.LIndex(ctx, kRunlog(txnSession), 1).Val()), &assistant); err != nil {
		t.Fatal(err)
	}
	if assistant.Role != "assistant" || assistant.Run != txnRun || assistant.Text != "好的，我来读" {
		t.Fatalf("assistant row = %+v, want role=assistant run=%s text=好的，我来读", assistant, txnRun)
	}
	frames, _, status, err := s.ReadLive(ctx, txnRun, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if status != "done" || frames[len(frames)-1].Kind != "terminal" || frames[len(frames)-1].Status != "complete" {
		t.Fatalf("after finalize status/last = %q/%+v, want done/terminal complete", status, frames[len(frames)-1])
	}
	if ttl := rdb.TTL(ctx, kRunlive(txnRun)).Val(); ttl <= 0 {
		t.Fatalf("TTL runlive = %v, want > 0", ttl)
	}
	// 定稿不删 runlive：键还在、帧序列还可读（done ≠ gone）。
	if rdb.Exists(ctx, kRunlive(txnRun)).Val() != 1 {
		t.Fatal("runlive deleted at finalize, want kept with TTL")
	}
}

// 断言 5：TTL 到期后 ReadLive 得 gone。
func TestStoreGoneAfterTTL(t *testing.T) {
	s, _, mr := newTestStore(t)
	ctx := context.Background()
	acquire(t, s)
	if err := s.Finalize(ctx, txnSession, txnRun); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(301 * time.Second)

	frames, next, status, err := s.ReadLive(ctx, txnRun, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 || next != 0 || status != "gone" {
		t.Fatalf("after TTL = len %d next %d status %q, want 0/0/gone", len(frames), next, status)
	}
}

// 断言 6：落盘原始 JSON 不含 eventOffset —— 游标只由 index 推出。
func TestStorePersistedFramesCarryNoOffset(t *testing.T) {
	s, rdb, _ := newTestStore(t)
	ctx := context.Background()
	acquire(t, s)
	_ = s.AppendDelta(ctx, txnRun, "好的")

	for _, idx := range []int64{0, 1} {
		raw := rdb.LIndex(ctx, kRunlive(txnRun), idx).Val()
		if strings.Contains(raw, "eventOffset") {
			t.Fatalf("persisted frame %d contains eventOffset: %s", idx, raw)
		}
	}
}

// 断言 7：单批上限 W 的截断 + nextOffset 续拉 + 往回拨无损。
func TestStoreBatchWindow(t *testing.T) {
	s, rdb, _ := newTestStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		rdb.RPush(ctx, kRunlog(txnSession), mustJSON(storedRow{
			Run: txnRun, Role: "user", MessageID: "m_" + strconv.Itoa(i), Text: "t",
		}))
	}

	tests := []struct {
		from, w    int
		wantLen    int
		wantFirst  int // 0 = 不检查首条
		wantOffset int // 期望 offset 区间末
		wantNext   int
	}{
		{from: 0, w: 3, wantLen: 3, wantFirst: 1, wantOffset: 3, wantNext: 3},
		{from: 3, w: 3, wantLen: 2, wantFirst: 4, wantOffset: 5, wantNext: 5},
		{from: 5, w: 3, wantLen: 0, wantNext: 5},
		{from: 1, w: 3, wantLen: 3, wantFirst: 2, wantOffset: 4, wantNext: 4}, // 往回拨
	}
	for _, tc := range tests {
		rows, next, err := s.ReadLog(ctx, txnSession, tc.from, tc.w)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != tc.wantLen || next != tc.wantNext {
			t.Fatalf("ReadLog(from=%d,w=%d) = len %d next %d, want len %d next %d",
				tc.from, tc.w, len(rows), next, tc.wantLen, tc.wantNext)
		}
		if tc.wantLen > 0 {
			if rows[0].RunlogOffset != tc.wantFirst || rows[len(rows)-1].RunlogOffset != tc.wantOffset {
				t.Fatalf("ReadLog(from=%d) offsets = %d..%d, want %d..%d",
					tc.from, rows[0].RunlogOffset, rows[len(rows)-1].RunlogOffset, tc.wantFirst, tc.wantOffset)
			}
		}
	}
}

func TestStoreSessionAndHighWater(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()

	if ok, _ := s.SessionExists(ctx, txnSession); ok {
		t.Fatal("unregistered session exists")
	}
	if err := s.RegisterSession(ctx, txnSession); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SessionExists(ctx, txnSession); !ok {
		t.Fatal("registered session missing")
	}

	acquire(t, s)
	if h, _ := s.RunlogHighWater(ctx, txnSession); h != 1 {
		t.Fatalf("high water = %d, want 1", h)
	}
	if r, _ := s.LiveRun(ctx, txnSession); r != txnRun {
		t.Fatalf("live run = %q, want %q", r, txnRun)
	}
	if err := s.Finalize(ctx, txnSession, txnRun); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.LiveRun(ctx, txnSession); r != "" {
		t.Fatalf("live run after finalize = %q, want empty", r)
	}
}
