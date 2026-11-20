package runengine

// 历史对话混合检索 P3 的对账器与指针切换回归（doc/84 §6.4；施工包 §7 P3 第 3/4 条）：
//
//   - 覆盖水位：pending/retry/running/failed 的聚合口径与最老未完成任务时间；
//   - 对账缺失重投：done 行外部副本回退 → 重拨 pending 清 published_at（relay 重投）；
//     无行 chunk → 登记配对任务走既有 pending→领取路径；
//   - 孤儿清理：外部有、PG 无 → 登记 kind=delete 任务（走既有 delete 链），
//     登记前复查 chunk 是否刚写入（防误删真数据）；
//   - 指针切换事务性：一笔事务一条 UPDATE；事务回滚（kill -9 模拟）指针保持旧值；
//   - 幂等恢复窗口：done 的重复写回不报错、不覆盖新状态。
//
// 打桩沿用 P1+P2 惯例：进程内 fake，不打外部服务。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

// fakeDialogueLister 是对账用的外部 run_id 枚举桩（可注入计数差异与故障）。
type fakeDialogueLister struct {
	ids      map[string]bool
	count    int64 // CountDocs 返回值；0 且 countSet=false 时用 len(ids)
	countSet bool
	listErr  error
	countErr error
}

func (f *fakeDialogueLister) ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	return f.ids, false, nil
}

func (f *fakeDialogueLister) CountDocs(ctx context.Context) (int64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	if f.countSet {
		return f.count, nil
	}
	return int64(len(f.ids)), nil
}

// seedDialogueChunk 给对账/重建测试造一条 chunk（归属走 testOwner，正文带 PII 供脱敏断言）。
func seedDialogueChunk(t *testing.T, s *Store, runID, passage string) {
	t.Helper()
	if err := s.db.Exec(
		`INSERT INTO dialogue_chunks (run_id, session_id, tenant_id, user_id, workspace, passage, runes, truncated, indexed_at)
		 VALUES (?, 's_p', ?, ?, '/tmp/ws-test', ?, ?, false, now())
		 ON CONFLICT (run_id) DO UPDATE SET passage = EXCLUDED.passage`,
		runID, testOwner.TenantID, testOwner.UserID, passage, len([]rune(passage))).Error; err != nil {
		t.Fatal(err)
	}
}

// TestDialogueCoverageCounts：水位聚合口径——pending（没消费过）/ retry（退避中）/
// running（认领未归还）/ failed（死信）四个桶互斥，最老未完成取 pending∪running。
func TestDialogueCoverageCounts(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	for i := 1; i <= 5; i++ {
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec(fmt.Sprintf("r_cov%d", i), DialogueJobKindUpsert)); err != nil {
			t.Fatal(err)
		}
	}
	esCov := func(n int) *DialogueIndexJob {
		return dialogueJobRow(t, s, fmt.Sprintf("r_cov%d", n), DialogueJobKindUpsert, DialogueTargetES)
	}
	claimOne := func(n int) *DialogueIndexJob {
		// 认领并返回 RETURNING 的任务行（claim_epoch 以认领返回为准，不是领取前的读）。
		id := esCov(n).ID
		claimed, err := s.claimDialogueJobsByIDs(ctx, []string{id}, dialogueJobLease)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("认领 r_cov%d: %d %v", n, len(claimed), err)
		}
		return claimed[0]
	}
	// r_cov1：做完 → done。
	done1 := claimOne(1)
	if err := s.completeDialogueJob(ctx, done1.ID, done1.ClaimEpoch); err != nil {
		t.Fatal(err)
	}
	// r_cov2：失败一次（瞬时）→ pending + attempts=1 → retry 桶。
	j := claimOne(2)
	if err := s.failDialogueJob(ctx, j.ID, j.ClaimEpoch, dlgErrESUnavailable, false); err != nil {
		t.Fatal(err)
	}
	// r_cov3：不动 → pending 桶（attempts=0）。
	// r_cov4：认领不归还 → running 桶。
	_ = claimOne(4)
	// r_cov5：连败三次（瞬时退避）→ failed 桶（死信；永久失败当场死信，走不到三次）。
	for i := 0; i < 3; i++ {
		fresh := claimOne(5)
		if err := s.failDialogueJob(ctx, fresh.ID, fresh.ClaimEpoch, dlgErrESUnavailable, false); err != nil {
			t.Fatal(err)
		}
		forceDialogueJobDue(t, s, fresh.ID) // 退避拨到期，让下一次认领拿得到
	}

	cov, err := s.DialogueCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 10 行里：done 1（r_cov1/es）、retry 1（r_cov2/es）、running 1（r_cov4/es）、
	// failed 1（r_cov5/es），其余 6 行 pending 且 attempts=0（r_cov3/es + 5 条 milvus）。
	if cov.Pending != 6 || cov.Retry != 1 || cov.Running != 1 || cov.Failed != 1 {
		t.Fatalf("覆盖水位 = pending=%d retry=%d running=%d failed=%d, want 6/1/1/1",
			cov.Pending, cov.Retry, cov.Running, cov.Failed)
	}
	if cov.OldestPendingAt == nil {
		t.Fatal("最老未完成任务时间 = nil，want 非空（存在 pending/running 行）")
	}
	if cov.OldestPendingAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("最老未完成任务时间 %s 在未来", cov.OldestPendingAt)
	}
}

// TestDialogueReconcileMissingRescheduled：缺失收敛的两条路——
// done 行（外部副本回退）重拨 pending 清 published_at，relay 会重新投递；
// 无行 chunk（hybrid 开启前落库）登记配对任务走既有领取路径。
func TestDialogueReconcileMissingRescheduled(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	// r_a：登记 → 两条都做完（外部副本一度存在）。
	seedDialogueChunk(t, s, "r_a", "密钥是 key=sk-proj-AbCdEfGhIjKlMnOpQrSt 请保管好")
	if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_a", DialogueJobKindUpsert)); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{DialogueTargetES, DialogueTargetMilvus} {
		id := dialogueJobRow(t, s, "r_a", DialogueJobKindUpsert, target).ID
		claimed, err := s.claimDialogueJobsByIDs(ctx, []string{id}, dialogueJobLease)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("认领 r_a/%s: %d %v", target, len(claimed), err)
		}
		if err := s.completeDialogueJob(ctx, claimed[0].ID, claimed[0].ClaimEpoch); err != nil {
			t.Fatal(err)
		}
	}

	// r_b：有 chunk 但从未登记（模拟 hybrid 开启前的存量块）。
	seedDialogueChunk(t, s, "r_b", "会话记录：端口 5433 用 go1.27 编译")

	// 外部状态：ES 有 r_a（副本还在），Milvus 的 r_a 文档被删（副本回退）。
	esList := &fakeDialogueLister{ids: map[string]bool{"r_a": true}}
	milList := &fakeDialogueLister{ids: map[string]bool{}}
	rec := &dialogueReconciler{
		store:  s,
		esName: "tako_dialogue_test", esList: esList,
		milName: "tako_dialogue_test", milList: milList,
		iter: dialogueCoverageInterval,
	}
	report := rec.runOnce(ctx)

	// ES 臂：PG={r_a,r_b} ext={r_a} → 缺 r_b；r_b 无任务行 → 登记配对（两条）。
	if report.ESMissing != 1 || report.ESOrphan != 0 {
		t.Fatalf("ES 臂差异 missing=%d orphan=%d, want 1/0", report.ESMissing, report.ESOrphan)
	}
	// Milvus 臂：ext={} → 缺 r_a+r_b；r_a done → 重拨；r_b 无行 → 登记。
	if report.MilvusMissing != 2 || report.MilvusOrphan != 0 {
		t.Fatalf("Milvus 臂差异 missing=%d orphan=%d, want 2/0", report.MilvusMissing, report.MilvusOrphan)
	}
	if report.Rescheduled != 1 {
		t.Fatalf("重拨数 = %d, want 1（r_a/milvus done→pending）", report.Rescheduled)
	}
	// 登记原语一次产出两条目标任务（es + milvus 配对），计数按动作算一次；
	// Milvus 臂轮到 r_b 时行已在（pending）→ 记在途。
	if report.Registered != 1 {
		t.Fatalf("新登记数 = %d, want 1（r_b 一组配对登记）", report.Registered)
	}
	if report.Inflight != 1 {
		t.Fatalf("在途数 = %d, want 1（r_b/milvus 在登记后已 pending）", report.Inflight)
	}

	// 重拨后的 r_a/milvus：pending、published_at 空（relay 会重投）、attempts 归零。
	milJob := dialogueJobRow(t, s, "r_a", DialogueJobKindUpsert, DialogueTargetMilvus)
	if milJob.Status != DialogueJobStatusPending || milJob.PublishedAt != nil || milJob.Attempts != 0 {
		t.Fatalf("r_a/milvus 重拨态 = status=%s published=%v attempts=%d, want pending/nil/0",
			milJob.Status, milJob.PublishedAt, milJob.Attempts)
	}
	// r_a/es 的 done 行没被误动（ES 副本还在，不算缺失）。
	esJob := dialogueJobRow(t, s, "r_a", DialogueJobKindUpsert, DialogueTargetES)
	if esJob.Status != DialogueJobStatusDone {
		t.Fatalf("r_a/es 被误动：status=%s, want done", esJob.Status)
	}

	// 新登记的 r_b：两条目标任务、共用 event_id、归属从 chunk+runs 来、状态 pending。
	xb := dialogueJobRow(t, s, "r_b", DialogueJobKindUpsert, DialogueTargetES)
	xm := dialogueJobRow(t, s, "r_b", DialogueJobKindUpsert, DialogueTargetMilvus)
	if xb.EventID != xm.EventID || xb.Status != DialogueJobStatusPending || xm.Status != DialogueJobStatusPending {
		t.Fatalf("r_b 新登记 = es(%s,%s) mil(%s,%s), want 同 event_id 且都 pending", xb.EventID, xb.Status, xm.EventID, xm.Status)
	}
	if xb.TenantID != testOwner.TenantID || xb.UserID != testOwner.UserID {
		t.Fatalf("r_b 归属没落行: tenant=%s user=%s", xb.TenantID, xb.UserID)
	}
	if xb.EmbeddingModel != "test-embed-model" {
		t.Fatalf("r_b embedding model = %q, want test-embed-model", xb.EmbeddingModel)
	}

	// relay 视角：重拨的 r_a/milvus 与新登记的 r_b 都在未投递清单里（重投是 relay 的活）。
	unpub, err := s.unpublishedDialogueJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range unpub {
		got[j.RunID+"/"+j.Target] = true
	}
	if !got["r_a/"+DialogueTargetMilvus] {
		t.Fatal("重拨的 r_a/milvus 不在 relay 未投递清单里")
	}
	if !got["r_b/"+DialogueTargetES] || !got["r_b/"+DialogueTargetMilvus] {
		t.Fatal("新登记的 r_b 两条不在 relay 未投递清单里")
	}
}

// TestDialogueReconcileOrphanDeleted：孤儿（外部有、PG 无）登记 kind=delete 任务，
// 登记前复查 chunk 是否刚写入；delete 沿既有投递链（relay → 消费者 → 客户端删除）。
func TestDialogueReconcileOrphanDeleted(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	seedDialogueChunk(t, s, "r_live", "活着的会话记录")
	if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_live", DialogueJobKindUpsert)); err != nil {
		t.Fatal(err)
	}
	// 复查窗口的材料：r_fake 在 PG 有 chunk——模拟「枚举 PG 之后 chunk 刚写入」，
	// 直接打登记原语验证复查把它拦下（runOnce 的 diff 里它根本不是孤儿）。
	seedDialogueChunk(t, s, "r_fake", "复查窗口内刚写入的会话记录")

	esList := &fakeDialogueLister{ids: map[string]bool{"r_live": true, "r_orphan_a": true}}
	milList := &fakeDialogueLister{ids: map[string]bool{"r_live": true, "r_orphan_a": true}}
	rec := &dialogueReconciler{
		store:  s,
		esName: "tako_dialogue_test", esList: esList,
		milName: "tako_dialogue_test", milList: milList,
		iter: dialogueCoverageInterval,
	}
	report := rec.runOnce(ctx)
	if report.OrphanDeleted != 1 || report.SkippedOrphans != 0 {
		t.Fatalf("孤儿处理 deleted=%d skipped=%d, want 1/0", report.OrphanDeleted, report.SkippedOrphans)
	}

	// 登记原语的复查窗口：r_orphan_a（无 chunk）幂等回读，r_fake（有 chunk）被拦下。
	registered, skipped, err := s.registerDialogueOrphanDeletes(ctx, []string{"r_orphan_a", "r_fake"})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("复查应拦下 r_fake: skipped=%d", skipped)
	}
	if got := s.count(t, &DialogueIndexJob{}, "run_id = ? AND kind = ?", "r_fake", DialogueJobKindDelete); got != 0 {
		t.Fatalf("有 chunk 的假孤儿不该被登记 delete（%d 行）", got)
	}
	_ = registered // r_orphan_a 已在 runOnce 登记过，这里回读幂等

	// 孤儿 delete 任务存在（两条 target 配对）且走 relay 投递。
	delES := dialogueJobRow(t, s, "r_orphan_a", DialogueJobKindDelete, DialogueTargetES)
	delMil := dialogueJobRow(t, s, "r_orphan_a", DialogueJobKindDelete, DialogueTargetMilvus)
	if delES.EventID != delMil.EventID || delES.Status != DialogueJobStatusPending {
		t.Fatalf("孤儿 delete 任务 = es(%s,%s) mil(%s,%s)", delES.EventID, delES.Status, delMil.EventID, delMil.Status)
	}
	if delES.TenantID != "" || delES.UserID != "" {
		t.Fatalf("孤儿 delete 归属应留空（PG 正本已无）: %s/%s", delES.TenantID, delES.UserID)
	}

	// 消费 delete 任务：ES 消费者按 run_id 调既有 delete 路径（Process 出结局，
	// 写回由消费者驱动做——本断言只看结局与外部调用）。
	fake := &fakeDialogueESIndex{}
	p := newDialogueESProcessor(s, fake)
	outcomes := p.Process(ctx, []*DialogueIndexJob{delES})
	if outcomes[0] != (dialogueJobOutcome{}) {
		t.Fatalf("孤儿 delete 结局 = %+v, want 空结局（成功）", outcomes[0])
	}
	if len(fake.upserts) != 0 {
		t.Fatalf("孤儿清理不应产生 upsert: %v", fake.upserts)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "r_orphan_a" {
		t.Fatalf("delete 调用 = %v, want [r_orphan_a]", fake.deletes)
	}
}

// TestDialogueReconcileLagAndPointerSkip：lag 拿不到记 0 并带说明（不伪造水位）；
// 指针绑定名与启动期不一致时暂停外部对账，只报水位。
func TestDialogueReconcileLagAndPointerSkip(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_l", "会话记录")

	// lag 注入：ES 组 3 条、向量组查询失败（记 0 + 说明）。
	esQ := newFakeDialogueQueue()
	esQ.lagOverride = 3
	vecQ := newFakeDialogueQueue()
	vecQ.lagErr = fmt.Errorf("broker 拒绝")
	rec := &dialogueReconciler{
		store:  s,
		esName: "tako_dialogue_test", esList: &fakeDialogueLister{ids: map[string]bool{"r_l": true}},
		milName: "tako_dialogue_test", milList: &fakeDialogueLister{ids: map[string]bool{"r_l": true}},
		esQ: esQ, vecQ: vecQ,
		iter: dialogueCoverageInterval,
	}
	report := rec.runOnce(ctx)
	if report.LagES != 3 || report.LagMilvus != 0 || report.LagNote == "" {
		t.Fatalf("lag = es%d vec%d note=%q, want es3 vec0 非空说明", report.LagES, report.LagMilvus, report.LagNote)
	}

	// 指针切换后的旧副本：绑定名不匹配 → PointerSkipped，不做外部差异。
	if err := s.SwitchDialogueRetrievalPointer(ctx, 2, "tako_dialogue_test-v2", "tako_dialogue_test-v2"); err != nil {
		t.Fatal(err)
	}
	moved := rec.runOnce(ctx)
	if !moved.PointerSkipped {
		t.Fatal("指针已切换后旧副本应跳过外部对账")
	}
	if moved.ESMissing != 0 || moved.MilvusMissing != 0 || moved.OrphanDeleted != 0 {
		t.Fatalf("跳过轮不应产生任何收敛动作: %+v", moved)
	}
}

// TestDialoguePointerSwitchTransactional：切换是「一笔事务一条 UPDATE」——
// 成功路径三字段原子换新；事务回滚（kill -9 模拟：语句已执行、事务未提交）指针
// 保持旧值；指针行不存在时报错且不动任何东西。
func TestDialoguePointerSwitchTransactional(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t) // 指针 v1：tako_dialogue_test / tako_dialogue_test

	if err := s.SwitchDialogueRetrievalPointer(ctx, 7, "tako_dialogue_test-v7", "tako_dialogue_test-v7"); err != nil {
		t.Fatal(err)
	}
	ptr, err := s.dialoguePointer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ptr.IndexVersion != 7 || ptr.EsIndex != "tako_dialogue_test-v7" || ptr.MilvusCollection != "tako_dialogue_test-v7" {
		t.Fatalf("切换后指针 = v%d es=%s mil=%s", ptr.IndexVersion, ptr.EsIndex, ptr.MilvusCollection)
	}

	// kill -9 模拟：切换语句在自己的事务里执行成功，但事务随后回滚——指针保持 v7。
	// switchDialoguePointerRow 拆成独立函数就是为了能在测试事务里跑这一条语句。
	var rolled string
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := switchDialoguePointerRow(tx, 8, "tako_dialogue_test-v8", "tako_dialogue_test-v8"); err != nil {
			return err
		}
		return fmt.Errorf("注入失败：模拟 kill -9（事务未提交）")
	}); err == nil {
		t.Fatal("注入失败应返回错误")
	}
	if err := s.db.Raw(`SELECT es_index FROM dialogue_retrieval_pointer WHERE id = 1`).Scan(&rolled).Error; err != nil {
		t.Fatal(err)
	}
	if rolled != "tako_dialogue_test-v7" {
		t.Fatalf("回滚后指针 es_index = %q, want tako_dialogue_test-v7（要么旧要么新）", rolled)
	}

	// 指针行不存在的存储：切换报错（装配错误不被静默吞掉）。
	bare := newTestStore(t, testLease, testTTL)
	if err := bare.SwitchDialogueRetrievalPointer(ctx, 1, "x", "y"); err == nil {
		t.Fatal("指针未初始化时切换应报错")
	}
}

// TestDialogueDuplicateWriteBackIdempotent：done 的重复写回不报错、不改状态——
// 消费者与对账重拨竞争时的幂等恢复窗口（P3 第 3 条）。
func TestDialogueDuplicateWriteBackIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_dup", DialogueJobKindUpsert)); err != nil {
		t.Fatal(err)
	}
	j := dialogueJobRow(t, s, "r_dup", DialogueJobKindUpsert, DialogueTargetES)
	claimed, err := s.claimDialogueJobsByIDs(ctx, []string{j.ID}, dialogueJobLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领: %d %v", len(claimed), err)
	}
	epoch := claimed[0].ClaimEpoch
	if err := s.completeDialogueJob(ctx, claimed[0].ID, epoch); err != nil {
		t.Fatal(err)
	}
	first := dialogueJobRow(t, s, "r_dup", DialogueJobKindUpsert, DialogueTargetES)

	// 同一代次的第二次完成写回：不报错（幂等），行状态不变。
	if err := s.completeDialogueJob(ctx, claimed[0].ID, epoch); err != nil {
		t.Fatalf("done 重复写回报错: %v", err)
	}
	second := dialogueJobRow(t, s, "r_dup", DialogueJobKindUpsert, DialogueTargetES)
	if second.Status != DialogueJobStatusDone || second.Attempts != first.Attempts {
		t.Fatalf("重复写回改了状态: %+v vs %+v", second, first)
	}

	// 旧代次写回仍然被拒（既有守卫不动，重复写回语义没有放开它）。
	if err := s.completeDialogueJob(ctx, claimed[0].ID, epoch-1); err != nil {
		t.Fatalf("旧代次写回应静默不动而非报错: %v", err)
	}
}
