// 本文件：事件库单测。seq 单调、from 边界、终局事务、启动对账。
package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestDB(t *testing.T) *LogDB {
	t.Helper()
	d, err := OpenLogDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenLogDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestLogDBSeqMatchesPayload 钉住：start 的 seq 是 1，其后无洞，列与 payload 是同一个数。
func TestLogDBSeqMatchesPayload(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	start, err := d.BeginRun(ctx, "r_1", "s", "你好", "yk-tako")
	if err != nil {
		t.Fatal(err)
	}
	if start.Seq != 1 || start.Kind != "start" || start.Text != "你好" {
		t.Fatalf("start = %+v", start)
	}
	msg, err := d.Append(ctx, sseEvent{Run: "r_1", Kind: "message", Text: "在"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Seq != 2 {
		t.Fatalf("seq = %d，want 2", msg.Seq)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT seq, payload FROM event_log WHERE run_id = ? ORDER BY seq`, "r_1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var seq int64
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			t.Fatal(err)
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Seq != seq {
			t.Fatalf("payload.seq = %d，列 seq = %d", ev.Seq, seq)
		}
		n++
		if seq != int64(n) {
			t.Fatalf("seq = %d，want %d（中间有洞）", seq, n)
		}
	}
	if n != 2 {
		t.Fatalf("行数 = %d，want 2", n)
	}
}

// TestFinishWithPatchRewritesCurrentUserAndDropsOldRows 钉住：补丁和 produced_json 同一笔提交。
// 被换掉的行不再进入 CompletedTurns。更早还留着的行，user_message 保持原话。
func TestFinishWithPatchRewritesCurrentUserAndDropsOldRows(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, item := range []struct{ id, user string }{
		{"r_old", "旧问题"},
		{"r_keep", "留下的问题"},
		{"r_now", "这一轮"},
	} {
		if _, err := d.BeginRun(ctx, item.id, "s", item.user, "yk-tako"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Finish(ctx, "r_old", "complete", "", []byte(`[{"role":"assistant","content":"旧答"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(ctx, "r_keep", "complete", "", []byte(`[{"role":"assistant","content":"留答"}]`)); err != nil {
		t.Fatal(err)
	}
	combined := "【上下文摘要】\n事实\n\n这一轮"
	if _, err := d.FinishWithPatch(ctx, "r_now", "complete", "", []byte(`[{"role":"assistant","content":"新答"}]`), ArchivePatch{
		UserMessage: combined,
		DropRunIDs:  []string{"r_old", "r_now", "", "idx:0"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.CompletedTurns(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != "r_keep" || got[0].UserMessage != "留下的问题" || got[1].RunID != "r_now" || got[1].UserMessage != combined {
		t.Fatalf("已完成轮次 = %+v", got)
	}
	old, err := d.lookupArchive(ctx, "r_old")
	if err != nil {
		t.Fatal(err)
	}
	if old.ProducedJSON.Valid {
		t.Fatalf("被换掉的行仍有产出：%+v", old)
	}
	now, err := d.lookupArchive(ctx, "r_now")
	if err != nil {
		t.Fatal(err)
	}
	if !now.ProducedJSON.Valid || !strings.Contains(now.ProducedJSON.String, "新答") {
		t.Fatalf("当前行产出 = %+v", now.ProducedJSON)
	}
}

// TestWriteRAGRoundTrip 钉住：原文按会话写进 compact_rag，另一会话读不到。
func TestWriteRAGRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.WriteRAG(ctx, "s", "r1", "这里有压缩算法"); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteRAG(ctx, "other", "r2", "别的会话"); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteRAG(ctx, "s", "idx:0", "占位不写"); err != nil {
		t.Fatal(err)
	}
	got, err := d.RAGBodies(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "这里有压缩算法" {
		t.Fatalf("bodies = %+v", got)
	}
}

// TestCompletedTurnsSkipsRunsWithoutOutput 钉住：只有写了产出的完成轮次进入下一轮。
func TestCompletedTurnsSkipsRunsWithoutOutput(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.BeginRun(ctx, "r_ok", "s", "你好", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(ctx, "r_ok", "complete", "", []byte(`[{"role":"assistant","content":"早"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BeginRun(ctx, "r_bad", "s", "失败的提问", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(ctx, "r_bad", "error", "boom", nil); err != nil {
		t.Fatal(err)
	}
	got, err := d.CompletedTurns(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UserMessage != "你好" || got[0].ProducedJSON == "" {
		t.Fatalf("已完成轮次 = %+v", got)
	}
}

// TestLogDBReadAfterBounds 覆盖空、从头、等于最后一帧、超过最大 seq。
func TestLogDBReadAfterBounds(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	got, err := d.ReadAfter(ctx, "missing", 0, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("未知 run = %+v err=%v，want 空", got, err)
	}
	if _, err := d.BeginRun(ctx, "r_1", "s", "hi", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Append(ctx, sseEvent{Run: "r_1", Kind: "message", Text: "a"}); err != nil {
		t.Fatal(err)
	}
	all, err := d.ReadAfter(ctx, "r_1", 0, 50)
	if err != nil || len(all) != 2 || all[0].Seq != 1 || all[1].Seq != 2 {
		t.Fatalf("from=0 = %+v err=%v", all, err)
	}
	tail, err := d.ReadAfter(ctx, "r_1", 2, 50)
	if err != nil || len(tail) != 0 {
		t.Fatalf("from=最后一帧 = %+v err=%v，want 空", tail, err)
	}
	beyond, err := d.ReadAfter(ctx, "r_1", 99, 50)
	if err != nil || len(beyond) != 0 {
		t.Fatalf("from 超过最大 = %+v err=%v，want 空", beyond, err)
	}
}

// TestLogDBFinishOnce 成功收尾写产出且只有一条终局；第二次收尾失败且不增行。
// 出错与停止也是 completed / is_completed=1，但不写产出。
func TestLogDBFinishOnce(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.BeginRun(ctx, "r_ok", "s", "问", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	term, err := d.Finish(ctx, "r_ok", "complete", "", []byte(`[{"role":"assistant"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if term.Kind != "complete" || term.Seq != 2 {
		t.Fatalf("终局 = %+v", term)
	}
	view, err := d.lookupArchive(ctx, "r_ok")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "completed" || view.IsCompleted != 1 || !view.ProducedJSON.Valid {
		t.Fatalf("档案 = %+v", view)
	}
	if _, err := d.Finish(ctx, "r_ok", "complete", "", nil); err == nil {
		t.Fatal("第二次收尾应失败")
	}
	evs, err := d.ReadAfter(ctx, "r_ok", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, ev := range evs {
		if ev.Kind == "complete" || ev.Kind == "error" || ev.Kind == "stop" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("终局帧数 = %d，want 1", terminals)
	}

	for _, kind := range []string{"error", "stop"} {
		id := "r_" + kind
		if _, err := d.BeginRun(ctx, id, "s", "问", "yk-tako"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Finish(ctx, id, kind, "原因", []byte(`should-drop`)); err != nil {
			t.Fatal(err)
		}
		view, err := d.lookupArchive(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != "completed" || view.IsCompleted != 1 || view.ProducedJSON.Valid {
			t.Fatalf("%s 档案 = %+v，want completed/1 且无产出", kind, view)
		}
	}
}

// TestLogDBReconcileFailed 没写终局的行对账后是 failed / 0，事件还在。
func TestLogDBReconcileFailed(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.BeginRun(ctx, "r_die", "s", "问", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Append(ctx, sseEvent{Run: "r_die", Kind: "message", Text: "说到一半"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := d.lookupArchive(ctx, "r_die")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "failed" || view.IsCompleted != 0 {
		t.Fatalf("对账后 = %+v，want failed/0", view)
	}
	evs, err := d.ReadAfter(ctx, "r_die", 0, 50)
	if err != nil || len(evs) != 2 {
		t.Fatalf("对账后事件 = %+v err=%v，want 2 帧仍可读", evs, err)
	}
	if _, err := d.BeginRun(ctx, "r_ok", "s", "问", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(ctx, "r_ok", "complete", "", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err := d.lookupArchive(ctx, "r_ok")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Status != "completed" || ok.IsCompleted != 1 {
		t.Fatalf("已完成的行被对账改掉了: %+v", ok)
	}
}

// TestLogDBConcurrentRuns 不同 run 同时写同一库，各自的 seq 从 1 连续。
func TestLogDBConcurrentRuns(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	const runs = 8
	const extra = 20
	ids := make([]string, runs)
	for i := 0; i < runs; i++ {
		ids[i] = "r_c" + string(rune('a'+i))
		if _, err := d.BeginRun(ctx, ids[i], "s", "hi", "yk-tako"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(runs)
	for i := 0; i < runs; i++ {
		go func(id string) {
			defer wg.Done()
			for n := 0; n < extra; n++ {
				if _, err := d.Append(ctx, sseEvent{Run: id, Kind: "message", Text: "x"}); err != nil {
					t.Errorf("append %s: %v", id, err)
					return
				}
			}
		}(ids[i])
	}
	wg.Wait()
	for _, id := range ids {
		evs, err := d.ReadAfter(ctx, id, 0, extra+5)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != extra+1 {
			t.Fatalf("%s 帧数 = %d，want %d", id, len(evs), extra+1)
		}
		for i, ev := range evs {
			if ev.Seq != int64(i+1) || ev.Run != id {
				t.Fatalf("%s[%d] = %+v", id, i, ev)
			}
		}
	}
}

// TestSQLiteDSNRelativeNotHost 锁住默认 -web-db：相对路径不能被编成 file://名字。
func TestSQLiteDSNRelativeNotHost(t *testing.T) {
	dsn, err := sqliteDSN("yktako-webui.db")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dsn, "file://") || !strings.HasPrefix(dsn, "file:yktako-webui.db?") {
		t.Fatalf("DSN = %s，相对路径被当成了 host", dsn)
	}
	abs, err := sqliteDSN(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(abs, "file:/") {
		t.Fatalf("绝对路径 DSN = %s", abs)
	}
}

// TestLogDBRelativePath 用相对路径真正建表。这是 -web-db 的默认值走的那条路。
func TestLogDBRelativePath(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	d, err := OpenLogDB("t.db")
	if err != nil {
		t.Fatalf("OpenLogDB: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.BeginRun(context.Background(), "r_rel", "s", "hi", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "t.db")); err != nil {
		t.Fatalf("相对路径没有落在工作目录: %v", err)
	}
}

// TestLogDBMemory 空路径能开，确认测试可以不落文件。
func TestLogDBMemory(t *testing.T) {
	d, err := OpenLogDB("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.BeginRun(context.Background(), "r_m", "s", "hi", "yk-tako"); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryPage_FirstPage 无 cursor 时返回最新 N 条已结束的 run。
func TestHistoryPage_FirstPage(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	// 建 3 条已完成的 run
	for _, id := range []string{"r_1", "r_2", "r_3"} {
		if _, err := d.BeginRun(ctx, id, "s", "问"+id, "yk-tako"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Finish(ctx, id, "complete", "", []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := d.HistoryPage(ctx, "s", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("runs = %d，want 2", len(page.Runs))
	}
	// 按 created_at 降序，最新的在前
	if page.Runs[0].RunID != "r_3" || page.Runs[1].RunID != "r_2" {
		t.Fatalf("runs = %+v，want r_3, r_2", page.Runs)
	}
	if page.NextCursor == "" {
		t.Fatal("nextCursor 不应为空")
	}
}

// TestHistoryPage_NextPage 有 cursor 时返回更早的 run。
func TestHistoryPage_NextPage(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, id := range []string{"r_1", "r_2", "r_3"} {
		if _, err := d.BeginRun(ctx, id, "s", "问"+id, "yk-tako"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Finish(ctx, id, "complete", "", []byte(`[]`)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := d.HistoryPage(ctx, "s", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.HistoryPage(ctx, "s", first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Runs) != 1 {
		t.Fatalf("runs = %d，want 1", len(second.Runs))
	}
	if second.Runs[0].RunID != "r_1" {
		t.Fatalf("runs = %+v，want r_1", second.Runs)
	}
	if second.NextCursor != "" {
		t.Fatalf("nextCursor = %s，want 空", second.NextCursor)
	}
}

// TestHistoryPage_ExcludesRunning status='running' 的 run 不在结果里。
func TestHistoryPage_ExcludesRunning(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.BeginRun(ctx, "r_done", "s", "完成", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Finish(ctx, "r_done", "complete", "", []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BeginRun(ctx, "r_live", "s", "正在跑", "yk-tako"); err != nil {
		t.Fatal(err)
	}
	// r_live 还是 running
	page, err := d.HistoryPage(ctx, "s", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("runs = %d，want 1", len(page.Runs))
	}
	if page.Runs[0].RunID != "r_done" {
		t.Fatalf("runs = %+v，want r_done", page.Runs)
	}
}

// TestHistoryPage_SameCreatedAt 同一秒多条 run，用 run_id 区分，不重复不漏。
func TestHistoryPage_SameCreatedAt(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	// 手动插入同一 created_at 的多条 run
	now := nowRFC3339()
	for _, id := range []string{"r_a", "r_b", "r_c"} {
		if _, err := d.db.ExecContext(ctx, `
INSERT INTO run_archive(run_id, session_id, status, is_completed, user_message, created_at)
VALUES (?, 's', 'completed', 1, '问', ?)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := d.HistoryPage(ctx, "s", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Runs) != 2 {
		t.Fatalf("first runs = %d，want 2", len(first.Runs))
	}
	// 按 run_id 降序：r_c, r_b
	if first.Runs[0].RunID != "r_c" || first.Runs[1].RunID != "r_b" {
		t.Fatalf("first runs = %+v，want r_c, r_b", first.Runs)
	}
	second, err := d.HistoryPage(ctx, "s", first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Runs) != 1 {
		t.Fatalf("second runs = %d，want 1", len(second.Runs))
	}
	if second.Runs[0].RunID != "r_a" {
		t.Fatalf("second runs = %+v，want r_a", second.Runs)
	}
}

// TestHistoryPage_InvalidCursor cursor 格式错误时返回错误。
func TestHistoryPage_InvalidCursor(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	_, err := d.HistoryPage(ctx, "s", "bad-cursor", 10)
	if err == nil {
		t.Fatal("无效 cursor 应返回错误")
	}
}

// 编译期提醒：sql.NullString 的 Valid 语义在本测试里表示「产出列有值」。
var _ = sql.ErrNoRows
