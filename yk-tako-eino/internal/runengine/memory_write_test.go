package runengine

// 写库事务的确定性测试（doc/65 §4.8）：准入、删除抑制（fail-closed）、merge 词集、
// 指针动作、时间校验、常驻预算、claim_epoch 守卫——全部走真 PG；
// merge 词集测试用带 pg_search 的库（分词器真跑，不 mock）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ptrTime / ptrString 造指针（时间与字符串字段的断言用）。
func ptrTime(t time.Time) *time.Time { return &t }
func ptrString(s string) *string     { return &s }

// writeJobFor 登记并认领一条 inrun 任务（写事务测试的公共前置）。
func writeJobFor(t *testing.T, s *Store, rid, ws string) *MemoryJob {
	t.Helper()
	id, err := s.RegisterMemoryJob(context.Background(), MemoryJobSpec{
		RunID: RunID(rid), SessionID: SessionID("s_" + rid), TenantID: "t1", UserID: "u1",
		Workspace: ws, Kind: MemoryJobKindInrun, TargetLayer: MemoryLayerRecall,
		Quote: "用户原话", SourceSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claimJobFor(t, s, id)
}

func insertActionStr(scope, content string) string {
	return `{"op":"insert","targets":[],"scope":"` + scope + `","content":"` + content + `","quote":"用户原话"}`
}

// TestMemoryWriteInsertHappyPath：insert 落行 + 批次与 done **同笔**提交。
func TestMemoryWriteInsertHappyPath(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w1", "p1")
	e := newMemoryExtractor(s, nil, nil)

	n, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "deck 放在 output/v2.html", Quote: "以后 deck 放 output/v2.html"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("written = %d, want 1", n)
	}
	row := s.jobOf(t, "r_w1")
	if row.Status != MemoryJobStatusDone || row.BatchCommittedAt == nil || row.BatchID == "" {
		t.Fatalf("job = %+v, want done + batch_id + batch_committed_at（同笔提交）", row)
	}
	if !strings.Contains(row.BatchPayload, "deck 放在 output/v2.html") {
		t.Fatalf("batch_payload = %q, want 含动作 JSON", row.BatchPayload)
	}
	var item MemoryItem
	if err := s.db.Where("content = ?", "deck 放在 output/v2.html").First(&item).Error; err != nil {
		t.Fatalf("新行没落库: %v", err)
	}
	if item.Layer != MemoryLayerRecall || item.Scope != MemoryScopeUser || item.Source != MemorySourceExtracted {
		t.Fatalf("新行 = %+v, want recall/user/extracted（这些由程序定，模型不给）", item)
	}
	// 来源指针由程序从 job 补（doc/65 §4.7）——空指针会让 BM25 的证据时间排序全落 NULL。
	if item.SourceRunID != job.RunID || item.SourceSessionID != job.SessionID || item.SourceSeq != job.SourceSeq {
		t.Fatalf("来源指针 = run=%q session=%q seq=%d, want job 的 %q/%q/%d",
			item.SourceRunID, item.SourceSessionID, item.SourceSeq, job.RunID, job.SessionID, job.SourceSeq)
	}
}

// TestMemoryWriteInsertRejectedRollsBack：scope 非法 → 整批不写、job 不动（仍 running）。
func TestMemoryWriteInsertRejectedRollsBack(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w2", "p1")
	e := newMemoryExtractor(s, nil, nil)

	if _, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: "tenant", Content: "域不对"},
	}); err == nil {
		t.Fatal("scope 非法应拒绝")
	}
	if n := s.count(t, &MemoryItem{}, "1 = 1"); n != 0 {
		t.Fatalf("拒绝了还写了 %d 行", n)
	}
	if row := s.jobOf(t, "r_w2"); row.Status != MemoryJobStatusRunning || row.BatchCommittedAt != nil {
		t.Fatalf("job = %+v, want 仍 running、批次未提交", row)
	}
}

// TestMemoryWriteSuppressRemovedContent：被明确删除过的内容不许写回；
// 抑制命中不是失败——批次照常提交（动作被跳过）。
func TestMemoryWriteSuppressRemovedContent(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w3", "p1")
	removed := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: "不要的旧事实", State: MemoryStateRemoved, RemovedAt: ptrTime(time.Now())})
	e := newMemoryExtractor(s, nil, nil)

	n, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "不要的旧事实"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("删除抑制后 written = %d, want 0", n)
	}
	if row := s.jobOf(t, "r_w3"); row.Status != MemoryJobStatusDone {
		t.Fatalf("抑制命中是正常收尾, job = %+v", row)
	}
	if n := s.count(t, &MemoryItem{}, "content = ? AND state = 'active'", "不要的旧事实"); n != 0 {
		t.Fatal("被删的内容复活了")
	}
	if n := s.count(t, &MemoryItem{}, "id = ?", removed.ID); n != 1 {
		t.Fatal("被删的历史行不该消失")
	}
}

// TestMemoryWriteSuppressQueryFailFailsJob：删除抑制查询失败 → **整 job 失败**，
// 不 fail-open（fail-open = 把用户删掉的事实写回去——记忆系统最不可接受的失败）。
func TestMemoryWriteSuppressQueryFailFailsJob(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w4", "p1")
	e := newMemoryExtractor(s, nil, nil)

	// 拆掉基表：删除抑制查询必炸（测试库用完即删，不影响别的用例）。
	if err := s.db.Exec(`DROP TABLE memory_items CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	_, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "想写回被删事实"},
	})
	if err == nil || !strings.Contains(err.Error(), "删除抑制检查失败") {
		t.Fatalf("err = %v, want 删除抑制检查失败（不 fail-open）", err)
	}
}

// TestMemoryWriteMergeWordSet：merge 不能引入原文没有的新事实（词集校验，真分词）；
// 词集校验不可用 → 拒绝 merge，不降级 coexist。
func TestMemoryWriteMergeWordSet(t *testing.T) {
	t.Run("新词被拒", func(t *testing.T) {
		s := newMemoryStoreWithSearch(t)
		job := writeJobFor(t, s, "r_w5", "p1")
		a := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户喜欢咖啡"})
		b := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户在北京工作"})
		cands := []memoryExtractCandidate{
			{Index: 1, Item: a}, {Index: 2, Item: b},
		}
		e := newMemoryExtractor(s, nil, nil)
		_, err := e.applyActions(context.Background(), job, cands, []memoryAction{
			{Op: memoryOpMerge, Targets: []int{1, 2}, Scope: MemoryScopeUser, Content: "用户喜欢咖啡"},
		})
		if err == nil || !strings.Contains(err.Error(), "新事实") {
			t.Fatalf("err = %v, want merge 引入原文没有的新事实被拒（丢了北京工作的内容）", err)
		}
		if n := s.count(t, &MemoryItem{}, "state = 'active'"); n != 2 {
			t.Fatalf("被拒的 merge 写了行：%d 行 active, want 2", n)
		}
	})
	t.Run("并集合成通过且旧行挂指针", func(t *testing.T) {
		s := newMemoryStoreWithSearch(t)
		job := writeJobFor(t, s, "r_w6", "p1")
		a := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户喜欢咖啡"})
		b := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户在北京工作"})
		cands := []memoryExtractCandidate{
			{Index: 1, Item: a}, {Index: 2, Item: b},
		}
		e := newMemoryExtractor(s, nil, nil)
		n, err := e.applyActions(context.Background(), job, cands, []memoryAction{
			{Op: memoryOpMerge, Targets: []int{1, 2}, Scope: MemoryScopeUser, Content: "用户喜欢咖啡，用户在北京工作", Quote: "用户原话"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("written = %d, want 1", n)
		}
		var merged MemoryItem
		if err := s.db.Where("content = ?", "用户喜欢咖啡，用户在北京工作").First(&merged).Error; err != nil {
			t.Fatalf("合并行没落库: %v", err)
		}
		for _, old := range []MemoryItem{a, b} {
			var row MemoryItem
			if err := s.db.Where("id = ?", old.ID).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.State != MemoryStateSuperseded || row.SupersededBy == nil || *row.SupersededBy != merged.ID {
				t.Fatalf("旧行 %s = %+v, want superseded 指向合并行", old.ID, row)
			}
		}
	})
	t.Run("分词不可用拒绝", func(t *testing.T) {
		s := newMemoryStoreWithSearch(t)
		job := writeJobFor(t, s, "r_w7", "p1")
		a := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户喜欢咖啡"})
		b := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户在北京工作"})
		cands := []memoryExtractCandidate{{Index: 1, Item: a}, {Index: 2, Item: b}}
		e := newMemoryExtractor(s, nil, nil)
		// 拆掉扩展（级联删索引）：分词器没了 → merge 必须被拒，不降级 coexist。
		if err := s.db.Exec(`DROP EXTENSION pg_search CASCADE`).Error; err != nil {
			t.Fatal(err)
		}
		_, err := e.applyActions(context.Background(), job, cands, []memoryAction{
			{Op: memoryOpMerge, Targets: []int{1, 2}, Scope: MemoryScopeUser, Content: "用户喜欢咖啡，用户在北京工作"},
		})
		if err == nil || !strings.Contains(err.Error(), "词集校验不可用") {
			t.Fatalf("err = %v, want 词集校验不可用（拒绝 merge，不降级）", err)
		}
	})
}

// TestMemoryWriteConflictAndRemove：conflict 互写指针；remove 写 removed_at；
// source=user 的行不被动（用户自己的原话，程序不替他删）。
func TestMemoryWriteConflictAndRemove(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w8", "p1")
	a := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "说法甲"})
	b := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "说法乙"})
	usr := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "用户亲口说的", Source: MemorySourceUser})
	cands := []memoryExtractCandidate{{Index: 1, Item: a}, {Index: 2, Item: b}, {Index: 3, Item: usr}}
	e := newMemoryExtractor(s, nil, nil)

	if _, err := e.applyActions(context.Background(), job, cands, []memoryAction{
		{Op: memoryOpConflict, Targets: []int{1, 2}},
		{Op: memoryOpRemove, Targets: []int{1, 3}},
	}); err != nil {
		t.Fatal(err)
	}
	var ra, rb MemoryItem
	if err := s.db.Where("id = ?", a.ID).First(&ra).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Where("id = ?", b.ID).First(&rb).Error; err != nil {
		t.Fatal(err)
	}
	if ra.ConflictWith == nil || *ra.ConflictWith != rb.ID || rb.ConflictWith == nil || *rb.ConflictWith != ra.ID {
		t.Fatalf("conflict 没互写：a=%+v b=%+v", ra.ConflictWith, rb.ConflictWith)
	}
	if ra.State != MemoryStateRemoved || ra.RemovedAt == nil {
		t.Fatalf("remove 生效行 = %+v, want removed + removed_at", ra)
	}
	var ru MemoryItem
	if err := s.db.Where("id = ?", usr.ID).First(&ru).Error; err != nil {
		t.Fatal(err)
	}
	if ru.State != MemoryStateActive {
		t.Fatalf("source=user 的行被程序删了 = %+v, want 不动", ru)
	}
}

// TestMemoryWriteTimesValidated：expires_at 必须在未来；valid_from 解析不了就清空；
// 起点不早于终点。
func TestMemoryWriteTimesValidated(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_w9", "p1")
	e := newMemoryExtractor(s, nil, nil)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)

	if _, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "过期时间在过去", ExpiresAt: &past},
	}); err == nil {
		t.Fatal("expires_at 在过去应被拒")
	}
	if _, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "起点晚于终点", ValidFrom: &future, ExpiresAt: &past},
	}); err == nil {
		t.Fatal("valid_from 晚于 expires_at 应被拒")
	}
	n, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "解析不了的时间被清空", ValidFrom: ptrString("昨天下午")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("written = %d, want 1（valid_from 清空不算拒收）", n)
	}
	var item MemoryItem
	if err := s.db.Where("content = ?", "解析不了的时间被清空").First(&item).Error; err != nil {
		t.Fatal(err)
	}
	if item.ValidFrom != nil {
		t.Fatalf("valid_from = %v, want nil（解析不了就清空，不猜时间）", item.ValidFrom)
	}
}

// TestMemoryWriteTargetStateChanged：目标行在写事务内已不是 active → 整批不写
// （不降级成矛盾双写）。
func TestMemoryWriteTargetStateChanged(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_wa", "p1")
	target := seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "要被改口的原行"})
	// 目标行在抽取器读取之后、写事务之前被别人改了状态（模拟并发）。
	if err := s.RemoveMemoryItem(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	e := newMemoryExtractor(s, nil, nil)
	cands := []memoryExtractCandidate{{Index: 1, Item: target}}
	if _, err := e.applyActions(context.Background(), job, cands, []memoryAction{
		{Op: memoryOpReplace, Targets: []int{1}, Scope: MemoryScopeUser, Content: "改口后的新说法"},
	}); err == nil {
		t.Fatal("目标状态已变应整批拒绝")
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "改口后的新说法"); n != 0 {
		t.Fatalf("整批拒绝还写了 %d 行", n)
	}
}

// TestMemoryWriteResidentBudget：常驻超预算 → 不写（返回使用量错误），整笔回滚。
func TestMemoryWriteResidentBudget(t *testing.T) {
	s := newMemoryStore(t)
	job := writeJobFor(t, s, "r_wb", "p1")
	// 默认预算 2200 rune；先塞满，再写一条就该超。
	seedMem(t, s, MemoryItem{Layer: MemoryLayerResident, Scope: MemoryScopeUser,
		Content: strings.Repeat("预", 2300)})
	e := newMemoryExtractor(s, nil, nil)
	if _, err := e.applyActions(context.Background(), job, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "常驻新规矩"},
	}); err == nil || !strings.Contains(err.Error(), "超预算") {
		t.Fatalf("err = %v, want 常驻记忆超预算", err)
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "常驻新规矩"); n != 0 {
		t.Fatalf("超预算还写了 %d 行", n)
	}
}

// TestMemoryWriteStaleEpochRejected：旧持有者（claim_epoch 过期）写不进去——
// 行不写、批次不记、状态不动。
func TestMemoryWriteStaleEpochRejected(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_wc", SessionID: "s_wc", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindInrun, TargetLayer: MemoryLayerRecall, Quote: "记", SourceSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := claimJobFor(t, s, id)
	// 租约拨过期 → 另一个副本接管（epoch 递增）。
	if err := s.db.Exec(`UPDATE memory_jobs SET lease_until = now() - interval '1 second' WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	fresh := claimJobFor(t, s, id)
	if fresh.ClaimEpoch != stale.ClaimEpoch+1 {
		t.Fatalf("接管后 epoch = %d, want %d", fresh.ClaimEpoch, stale.ClaimEpoch+1)
	}
	e := newMemoryExtractor(s, nil, nil)
	if _, err := e.applyActions(ctx, stale, nil, []memoryAction{
		{Op: memoryOpInsert, Scope: MemoryScopeUser, Content: "旧持有者的写入"},
	}); err == nil {
		t.Fatal("旧 epoch 的写入应被拒")
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "旧持有者的写入"); n != 0 {
		t.Fatalf("旧 epoch 写进了 %d 行", n)
	}
	if row := s.jobOf(t, "r_wc"); row.Status != MemoryJobStatusRunning || row.ClaimEpoch != fresh.ClaimEpoch {
		t.Fatalf("job = %+v, want 仍归新持有者", row)
	}
}
