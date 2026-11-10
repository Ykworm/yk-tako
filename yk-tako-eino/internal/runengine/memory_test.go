package runengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 记忆任务与记忆行的存储链路回归（doc/65 §4.2/§4.3/§4.5）。
//
// 这一版对应**新行结构**：没有 kind/topic、没有正文去重索引，任务状态机按 claim_epoch
// 守卫。旧的「抽取循环 + 正文去重 + episode」那一套随旧表结构退役（doc/65 §4.2 取消
// kind/topic/精确内容去重索引），所以这里的断言换成新结构的等价不变量：
//   - 终态只登记一次 job（outbox 同事务语义不变）；
//   - claim 一次只有一条拿到手；
//   - **旧 claim_epoch 写不进去**（被别的副本重新 claim 过的旧 attempt 迟到不改状态）；
//   - **同内容不被去重吞掉**（A→B→A 改口能产生新历史行）；
//   - 失败退避到 failed；
//   - 并发 claim 各捡各的（SKIP LOCKED）。
//
// 视图、可见性两分支、状态机、证据时间、BM25 索引的断言在 memory_store_test.go。
func newMemoryStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t, testLease, testTTL)
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test", Extract: true, Recall: true})
	return s
}

// completeRun 造一场定稿 run（user 行 + EndRun complete）。
func (s *Store) completeRun(t *testing.T, sid SessionID, rid RunID, text string) {
	t.Helper()
	s.mustAcquire(t, sid, rid, "m_"+string(rid), text)
	if _, _, ok, err := s.EndRun(context.Background(), sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun(%s): ok=%v err=%v", rid, ok, err)
	}
}

func (s *Store) jobOf(t *testing.T, rid RunID) MemoryJob {
	t.Helper()
	var job MemoryJob
	if err := s.db.Where("run_id = ?", rid).First(&job).Error; err != nil {
		t.Fatalf("memory_jobs(run=%s): %v", rid, err)
	}
	return job
}

// TestMemoryOutboxOnCompleteOnly：终态只登记一次 job。
// error 结局不登记（没有可信结果，EndRun 本就不写完整轨迹）。
func TestMemoryOutboxOnCompleteOnly(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	s.completeRun(t, "s_1", "r_1", "hi")
	job := s.jobOf(t, "r_1")
	if job.Status != "pending" || job.Attempts != 0 || job.LastError != "" {
		t.Fatalf("complete 后 job = %+v, want pending/attempts=0", job)
	}
	// workspace 是**项目 id**（新结构的语义），不是引擎工作区根路径（doc/65 §4.2）。
	// 这场会话没挂项目 → 空串。
	if job.Workspace != "" || job.TenantID != testOwner.TenantID || job.UserID != testOwner.UserID {
		t.Fatalf("job scope = %s/%s/%s, want %s/%s/（无项目）", job.TenantID, job.UserID, job.Workspace, testOwner.TenantID, testOwner.UserID)
	}
	// 挂了项目的会话 → job 的 workspace 是那个项目 id。
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "记忆项目")
	if err != nil {
		t.Fatal(err)
	}
	s.mustAcquireWithWorkspace(t, "s_3", "r_3", "m_3", "hi", ws.ID)
	if _, _, ok, err := s.EndRun(ctx, "s_3", "r_3", 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun(r_3): ok=%v err=%v", ok, err)
	}
	if got := s.jobOf(t, "r_3").Workspace; got != ws.ID {
		t.Errorf("job.workspace = %q, want 项目 id %q", got, ws.ID)
	}

	// error 结局不登记（没有可信结果，EndRun 本就不写完整轨迹）。
	s.mustAcquire(t, "s_2", "r_2", "m_2", "hi")
	if _, _, ok, err := s.EndRun(ctx, "s_2", "r_2", 1, "error", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun(error): ok=%v err=%v", ok, err)
	}
	if got := s.count(t, &MemoryJob{}, "run_id = ?", "r_2"); got != 0 {
		t.Fatalf("error 结局登记了 %d 条 job, want 0", got)
	}
}

// TestMemoryJobClaimEpochGuards：claim_epoch 是状态机的守卫。
//
// claim 时 claim_epoch+1、attempts+1；**旧 epoch 的写一律写不进去**：
// 别人重新 claim 过的旧 attempt 迟到，既翻不了 done，也覆盖不了 last_error。
func TestMemoryJobClaimEpochGuards(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	s.completeRun(t, "s_1", "r_1", "记一下 5433")

	first, err := s.ClaimMemoryJob(ctx, "")
	if err != nil || first == nil {
		t.Fatalf("claim: job=%+v err=%v", first, err)
	}
	if first.Attempts != 1 || first.ClaimEpoch != 1 || first.Status != MemoryJobStatusRunning {
		t.Fatalf("claim 后 job = %+v, want attempts=1/claim_epoch=1/running", first)
	}
	if first.LeaseUntil == nil {
		t.Fatal("claim 后没有写 lease_until（崩溃后别的副本按它捡回）")
	}

	// 再 claim 应空（唯一任务已被本尝试持住，租约未过）。
	if again, err := s.ClaimMemoryJob(ctx, ""); err != nil || again != nil {
		t.Fatalf("第二次 claim = %+v, err=%v, want nil", again, err)
	}

	// 旧 epoch 翻不了 done。
	if err := s.CompleteMemoryJob(ctx, first.ID, first.ClaimEpoch-1); err != nil {
		t.Fatal(err)
	}
	row := s.jobOf(t, "r_1")
	if row.Status != MemoryJobStatusRunning {
		t.Fatalf("旧 epoch 把 job 翻成了 %s, want 仍 running", row.Status)
	}

	// 旧 epoch 覆盖不了错误。
	cause := errors.New("model down")
	if err := s.FailMemoryJob(ctx, first.ID, first.ClaimEpoch-1, cause); err != nil {
		t.Fatal(err)
	}
	row = s.jobOf(t, "r_1")
	if row.LastError != "" || row.Status != MemoryJobStatusRunning {
		t.Fatalf("旧 epoch 改了 job：status=%s lastError=%q, want running/空", row.Status, row.LastError)
	}

	// 本次 epoch 能翻 done。
	if err := s.CompleteMemoryJob(ctx, first.ID, first.ClaimEpoch); err != nil {
		t.Fatal(err)
	}
	row = s.jobOf(t, "r_1")
	if row.Status != MemoryJobStatusDone || row.LeaseUntil != nil {
		t.Fatalf("本次 epoch 后 job = %+v, want done/lease NULL", row)
	}
}

// TestMemoryJobDedupKeyReplay：同 job 重放不重新分析、不新增行。
//
// dedup_key 是幂等键（可空、唯一）：同一 dedup_key 重复登记返回**同一行**——
// 重放的派工不会多出一条任务，消费者据此判定「这单已经干过了」。
func TestMemoryJobDedupKeyReplay(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	key := "postrun:r_1"
	id1, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_1", SessionID: "s_1", TenantID: "t1", UserID: "u1",
		Workspace: "/tmp/ws-test", Kind: MemoryJobKindPostrun, Quote: "记住 5433",
		SourceSeq: 7, DedupKey: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 重放：同一 dedup_key 再登记一次 → 同一个 id，不新增行。
	id2, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_1", SessionID: "s_1", TenantID: "t1", UserID: "u1",
		Workspace: "/tmp/ws-test", Kind: MemoryJobKindPostrun, Quote: "记住 5433",
		SourceSeq: 7, DedupKey: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("重放登记拿到不同 id：%s vs %s", id1, id2)
	}
	if got := s.count(t, &MemoryJob{}, "dedup_key = ?", key); got != 1 {
		t.Fatalf("重放后 job 行数 = %d, want 1（不新增行）", got)
	}

	// 不同 dedup_key 各登记各的（幂等键只挡同一单）。
	other := "postrun:r_2"
	id3, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_2", TenantID: "t1", UserID: "u1", Workspace: "/tmp/ws-test",
		Kind: MemoryJobKindPostrun, DedupKey: &other,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id1 {
		t.Fatalf("不同 dedup_key 拿到同一个 id：%s", id3)
	}
	if got := s.count(t, &MemoryJob{}, ""); got != 2 {
		t.Fatalf("job 行数 = %d, want 2", got)
	}
}

// TestMemoryJobFailBackoffAndFailed：失败退避重试，次数用尽翻 failed。
func TestMemoryJobFailBackoffAndFailed(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	s.completeRun(t, "s_1", "r_1", "hi")

	cause := errors.New("model down")
	for attempt := 1; attempt <= memoryMaxAttempts; attempt++ {
		job, err := s.ClaimMemoryJob(ctx, "")
		if err != nil || job == nil {
			t.Fatalf("第 %d 次 claim: job=%+v err=%v", attempt, job, err)
		}
		if job.Attempts != attempt {
			t.Fatalf("第 %d 次 claim attempts=%d", attempt, job.Attempts)
		}
		if err := s.FailMemoryJob(ctx, job.ID, job.ClaimEpoch, cause); err != nil {
			t.Fatal(err)
		}
		row := s.jobOf(t, "r_1")
		if attempt < memoryMaxAttempts {
			if row.Status != MemoryJobStatusPending || !row.NextAttemptAt.After(time.Now()) {
				t.Fatalf("第 %d 次失败后 = %+v, want pending + 未来 next_attempt_at", attempt, row)
			}
			if row.LastError != "model down" {
				t.Fatalf("第 %d 次失败后 lastError=%q, want %q", attempt, row.LastError, "model down")
			}
			// 把退避拨到过去，模拟到点重试。
			if err := s.db.Exec(`UPDATE memory_jobs SET next_attempt_at = now() - interval '1 second' WHERE id = ?`, row.ID).Error; err != nil {
				t.Fatal(err)
			}
		} else if row.Status != MemoryJobStatusFailed || row.Attempts != memoryMaxAttempts || row.LastError != "model down" {
			t.Fatalf("第三次失败后 = %+v, want failed/attempts=3/lastError", row)
		}
	}
	// failed 不再被 claim。
	if job, err := s.ClaimMemoryJob(ctx, ""); err != nil || job != nil {
		t.Fatalf("failed 后 claim = %+v err=%v, want nil", job, err)
	}
}

// TestMemoryJobClaimSkipLocked：并发 claim 各捡各的（SKIP LOCKED），同一 job 不被捡两次。
func TestMemoryJobClaimSkipLocked(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	s.completeRun(t, "s_1", "r_1", "one")
	s.completeRun(t, "s_2", "r_2", "two")

	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := s.ClaimMemoryJob(ctx, "")
			if err != nil {
				t.Errorf("并发 claim: %v", err)
				return
			}
			if job == nil {
				t.Error("并发 claim 拿到 nil（两条任务应各被捡一次）")
				return
			}
			mu.Lock()
			if seen[job.ID] {
				t.Errorf("同一 job 被捡两次: %s", job.ID)
			}
			seen[job.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 2 {
		t.Fatalf("并发 claim 共捡到 %d 条, want 2", len(seen))
	}
}

// TestMemoryWriteNoContentDedup：同内容不被去重吞掉。
//
// 旧结构靠 `memory_items_dedup` 唯一索引（tenant,user,workspace,kind,content）做
// 「同内容只留一行」，结果**真实的 A→B→A 改口会被吞掉**——用户说了 A、改口 B、
// 又改回 A，库里只剩一行 A。新结构取消正文去重（doc/65 §4.2），所以 A→B→A 必须是三行。
func TestMemoryWriteNoContentDedup(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	a := MemoryItem{TenantID: "t1", UserID: "u1", Workspace: "p1", Layer: MemoryLayerRecall,
		Scope: MemoryScopeUser, Content: "端口是 5433", Quote: "端口是 5433", Source: MemorySourceUser}
	b := MemoryItem{TenantID: "t1", UserID: "u1", Workspace: "p1", Layer: MemoryLayerRecall,
		Scope: MemoryScopeUser, Content: "端口改成 5434", Quote: "端口改成 5434", Source: MemorySourceUser}
	a2 := MemoryItem{TenantID: "t1", UserID: "u1", Workspace: "p1", Layer: MemoryLayerRecall,
		Scope: MemoryScopeUser, Content: "端口是 5433", Quote: "端口是 5433", Source: MemorySourceUser}

	res, err := s.WriteMemoryItems(ctx, []MemoryItem{a, b, a2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 3 || res.Rejected != 0 {
		t.Fatalf("写入结果 = %+v, want written=3/rejected=0", res)
	}
	if got := s.count(t, &MemoryItem{}, "content = ?", "端口是 5433"); got != 2 {
		t.Fatalf("同内容行数 = %d, want 2（A→B→A 三行都在，不被去重吞掉）", got)
	}
	if got := s.count(t, &MemoryItem{}, ""); got != 3 {
		t.Fatalf("总行数 = %d, want 3", got)
	}
}

// TestMemoryWriteRejectsSecrets：密钥类整条拒收、证件类打码后入库（doc/41 §3.2 挂载点③）。
// 记忆正文会进模型提示，密钥不该进库；证件类可复用但不可直接利用。
func TestMemoryWriteRejectsSecrets(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)

	res, err := s.WriteMemoryItems(ctx, []MemoryItem{
		{TenantID: "t1", UserID: "u1", Workspace: "p1", Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
			Content: "我的密钥是 sk-proj-AbCdEfGhIjKlMnOpQrSt"},
		{TenantID: "t1", UserID: "u1", Workspace: "p1", Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
			Content: "我的身份证是 110101199003077777"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 1 || res.Written != 1 {
		t.Fatalf("写入结果 = %+v, want rejected=1（密钥类整条拒收）/written=1", res)
	}
	if got := s.count(t, &MemoryItem{}, "content LIKE ?", "%EfGhIjKlMnOpQrSt%"); got != 0 {
		t.Fatalf("密钥进了记忆库（%d 条）", got)
	}
	var content string
	if err := s.db.Raw(`SELECT content FROM memory_items WHERE content LIKE ?`, "%110101%").Row().Scan(&content); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "110101199003077777") {
		t.Fatalf("证件号未打码: %q", content)
	}
	if !strings.Contains(content, "110101") || !strings.Contains(content, "7777") {
		t.Fatalf("打码形状不对: %q", content)
	}
}
