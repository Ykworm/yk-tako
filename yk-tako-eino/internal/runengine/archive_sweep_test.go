package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepArchived(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	if !s.sqlBool(t, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'run_model_messages_compacted_run')`) {
		t.Fatal("run_model_messages_compacted_run 没有建出来")
	}
	if !s.sqlBool(t, `SELECT COUNT(*) = 2 FROM cleaner_state`) {
		t.Fatal("cleaner_state 应有 lease 与 archive 两行")
	}

	oldSid, oldRid := SessionID("s_old"), RunID("r_old")
	s.mustAcquire(t, oldSid, oldRid, "m_old", "old")
	if _, _, ok, err := s.EndRun(ctx, oldSid, oldRid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun old = (ok %v, err %v)", ok, err)
	}
	newerSid, newerRid := SessionID("s_newer"), RunID("r_newer")
	s.mustAcquire(t, newerSid, newerRid, "m_newer", "newer")
	if _, _, ok, err := s.EndRun(ctx, newerSid, newerRid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun newer = (ok %v, err %v)", ok, err)
	}
	freshSid, freshRid := SessionID("s_fresh"), RunID("r_fresh")
	s.mustAcquire(t, freshSid, freshRid, "m_fresh", "fresh")
	if _, _, ok, err := s.EndRun(ctx, freshSid, freshRid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun fresh = (ok %v, err %v)", ok, err)
	}
	goneSid, goneRid := SessionID("s_gone"), RunID("r_gone")
	s.mustAcquire(t, goneSid, goneRid, "m_gone", "gone")
	if _, _, ok, err := s.EndRun(ctx, goneSid, goneRid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun gone = (ok %v, err %v)", ok, err)
	}

	insertMsg := func(id string, sid SessionID, rid RunID, seq int, compacted bool) {
		t.Helper()
		if err := s.db.WithContext(ctx).Exec(
			`INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
			 VALUES (?, ?, ?, ?, 'user', ?, ?, now())`,
			id, sid, rid, seq, id, compacted).Error; err != nil {
			t.Fatal(err)
		}
	}
	insertMsg("msg_old_sum", oldSid, oldRid, 0, false)
	insertMsg("msg_old_verb", oldSid, oldRid, 2, true)
	insertMsg("msg_old_live", oldSid, oldRid, 3, false)
	insertMsg("msg_newer_verb", newerSid, newerRid, 2, true)
	insertMsg("msg_fresh_verb", freshSid, freshRid, 2, true)
	insertMsg("msg_fresh_sum", freshSid, freshRid, 0, false)
	insertMsg("msg_gone_live", goneSid, goneRid, 2, false)
	insertMsg("msg_gone_sum", goneSid, goneRid, 0, false)

	if err := s.db.WithContext(ctx).Exec(
		`UPDATE runs SET finished_at = now() - interval '10 days' WHERE id = ?`, oldRid).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE runs SET finished_at = now() - interval '9 days' WHERE id = ?`, newerRid).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE sessions SET deleted_at = now() - interval '8 days' WHERE id = ?`, goneSid).Error; err != nil {
		t.Fatal(err)
	}
	logsBefore := s.count(t, &RunlogEntry{}, "session_id = ?", goneSid)
	if logsBefore == 0 {
		t.Fatal("软删会话应还有 runlog")
	}

	if err := s.db.WithContext(ctx).Exec(
		`UPDATE cleaner_state SET owner = 'hold', expires_at = now() + interval '5 min' WHERE key = 'lease'`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SweepArchived(ctx, 168*time.Hour, 10, ""); !errors.Is(err, ErrSweepNotClaimed) {
		t.Fatalf("租约被占时 = %v, want not claimed", err)
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_old_verb'") != 1 {
		t.Fatal("没抢到租约却删了行")
	}
	if err := s.db.WithContext(ctx).Exec(`UPDATE cleaner_state SET expires_at = 'epoch' WHERE key = 'lease'`).Error; err != nil {
		t.Fatal(err)
	}

	var cutoff, wm time.Time
	if err := s.db.WithContext(ctx).Raw(`SELECT now() - interval '7 days'`).Scan(&cutoff).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.WithContext(ctx).Raw(`SELECT watermark FROM cleaner_state WHERE key = 'archive'`).Scan(&wm).Error; err != nil {
		t.Fatal(err)
	}
	cur, err := s.sweepCompactedBatch(ctx, cutoff, wm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.full || cur.n == 0 {
		t.Fatalf("第一批 = %+v, 应删到最旧那一刻并报满批", cur)
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_old_verb'") != 0 {
		t.Fatal("最旧 run 的 compacted 原文还在")
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_newer_verb'") != 1 {
		t.Fatal("游标跳过了次旧 run")
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_old_sum'") != 1 || s.count(t, &RunModelMessage{}, "id = 'msg_old_live'") != 1 {
		t.Fatal("摘要行或未压行被第一类删了")
	}

	root := t.TempDir()
	wireDir := filepath.Join(root, "tenants", "t1", "sessions", "s1", ".tako", "subagents")
	if err := os.MkdirAll(wireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldWire := filepath.Join(wireDir, "sa_old.jsonl")
	halfWire := filepath.Join(wireDir, "sa_half.jsonl")
	freshWire := filepath.Join(wireDir, "sa_fresh.jsonl")
	liveWire := filepath.Join(wireDir, "sa_live.jsonl")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	freshAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	write(oldWire, `{"type":"metadata","sa_id":"sa_old","agent_name":"researcher","protocol":1,"created_at":"`+past+`"}`+"\n"+
		`{"type":"terminal","status":"complete","result":"x","finished_at":"`+past+`"}`+"\n")
	write(halfWire, `{"type":"metadata","sa_id":"sa_half","agent_name":"researcher","protocol":1,"created_at":"`+past+`"}`+"\n"+
		`{"type":"message","msg":{"role":"assistant","content":"半"}}`+"\n")
	write(freshWire, `{"type":"metadata","sa_id":"sa_fresh","agent_name":"researcher","protocol":1,"created_at":"`+freshAt+`"}`+"\n"+
		`{"type":"terminal","status":"complete","result":"y","finished_at":"`+freshAt+`"}`+"\n")
	write(liveWire, `{"type":"metadata","sa_id":"sa_live","agent_name":"researcher","protocol":1,"created_at":"`+freshAt+`"}`+"\n"+
		`{"type":"message","msg":{"role":"assistant","content":"在跑"}}`+"\n")

	swept, watermark, err := s.SweepArchived(ctx, 168*time.Hour, 10, root)
	if err != nil {
		t.Fatal(err)
	}
	if swept < 2 {
		t.Fatalf("swept = %d, 应含次旧 compacted 与软删会话的消息，再加上过期文件", swept)
	}
	if !watermark.After(time.Unix(0, 0)) {
		t.Fatalf("watermark = %s, 应离开 epoch", watermark)
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_newer_verb'") != 0 {
		t.Fatal("次旧 compacted 原文还在")
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_old_sum'") != 1 || s.count(t, &RunModelMessage{}, "id = 'msg_old_live'") != 1 {
		t.Fatal("过窗 run 的摘要或未压行没了")
	}
	if s.count(t, &RunModelMessage{}, "id = 'msg_fresh_verb'") != 1 || s.count(t, &RunModelMessage{}, "id = 'msg_fresh_sum'") != 1 {
		t.Fatal("窗口内的 run 被清了")
	}
	if s.count(t, &RunModelMessage{}, "session_id = ?", goneSid) != 0 {
		t.Fatal("过窗软删会话的模型消息还在")
	}
	if s.count(t, &RunlogEntry{}, "session_id = ?", goneSid) != logsBefore {
		t.Fatal("runlog 被清了")
	}
	if s.count(t, &Run{}, "id = ?", goneRid) != 1 || s.count(t, &Session{}, "id = ?", goneSid) != 1 {
		t.Fatal("清扫删了 run 或会话行")
	}
	for _, path := range []string{oldWire, halfWire} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s 应被删: %v", path, err)
		}
	}
	for _, path := range []string{freshWire, liveWire} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s 不该动: %v", path, err)
		}
	}

	again, watermark2, err := s.SweepArchived(ctx, 168*time.Hour, 10, root)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("第二轮 swept = %d, want 0", again)
	}
	if !watermark2.Equal(watermark) {
		t.Fatalf("watermark 从 %s 走到 %s", watermark, watermark2)
	}
}
