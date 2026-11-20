package runengine

// 历史对话混合检索 P3 的版本化重建回归（doc/84 §6.4；施工包 §7 P3 第 3/4 条）：
//
//   - 全流程：建 v2 → 幂等回放（脱敏/归属/版本号/向量维度）→ 校验 → 一笔事务切指针
//     → 重跑收敛（已是活跃版本 → 只增量校验，不建不切）；
//   - 幂等恢复窗口：回放中途失败（模拟崩溃）重跑，从 PG 正本重来，最终状态收敛；
//   - 校验门：回放失败列出 run_id 并中止切换，指针保持旧值；
//   - 维度闸：探针失败 / collection 维度不符 → 119004，不建不回放。
//
// 打桩沿用 P1+P2 惯例：进程内 fake 客户端（状态 map + 调用计数 + 故障注入）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeRebuildES 是重建用 ES 桩：docs 是「外部真实状态」（upsert 覆盖写 = 幂等语义）。
type fakeRebuildES struct {
	mu           sync.Mutex
	docs         map[string]dialogueESDoc
	boundIndex   string
	ensureCalls  int
	refreshCalls int
	deleted      []string
	ensureErr    error
	upsertErrFor map[string]error
}

func newFakeRebuildES() *fakeRebuildES {
	return &fakeRebuildES{docs: map[string]dialogueESDoc{}, upsertErrFor: map[string]error{}}
}

func (f *fakeRebuildES) EnsureIndex(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	return f.ensureErr
}

func (f *fakeRebuildES) Refresh(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCalls++
	return nil
}

func (f *fakeRebuildES) CountDocs(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.docs)), nil
}

func (f *fakeRebuildES) ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.docs))
	for id := range f.docs {
		out[id] = true
	}
	return out, false, nil
}

func (f *fakeRebuildES) Upsert(ctx context.Context, doc dialogueESDoc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.upsertErrFor[doc.RunID]; err != nil {
		return err
	}
	f.docs[doc.RunID] = doc
	return nil
}

func (f *fakeRebuildES) Delete(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, runID)
	delete(f.docs, runID)
	return nil
}

// Search 按归属过滤派生命中（模拟真实 ES 的 filter+match：命中集来自已写文档）。
func (f *fakeRebuildES) Search(ctx context.Context, query, tenantID, workspaceID, userID string, indexVersion int64, topK int) ([]dialogueLexHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []dialogueLexHit
	for _, doc := range f.docs {
		if doc.TenantID != tenantID || doc.WorkspaceID != workspaceID {
			continue
		}
		if userID != "" && doc.UserID != userID {
			continue
		}
		hits = append(hits, dialogueLexHit{RunID: doc.RunID, Score: 1})
		if len(hits) >= topK {
			break
		}
	}
	return hits, nil
}

// fakeRebuildMil 是重建用 Milvus 桩：rows 是「外部真实状态」。
type fakeRebuildMil struct {
	mu          sync.Mutex
	rows        map[string]dialogueMilvusRow
	boundColl   string
	boundDim    int
	ensureCalls int
	flushCalls  int
	deleted     [][]string
	ensureErr   error
	upsertErr   error
}

func newFakeRebuildMil() *fakeRebuildMil {
	return &fakeRebuildMil{rows: map[string]dialogueMilvusRow{}}
}

func (f *fakeRebuildMil) EnsureCollection(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	return f.ensureErr
}

func (f *fakeRebuildMil) Flush(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushCalls++
	return nil
}

func (f *fakeRebuildMil) CountDocs(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rows)), nil
}

func (f *fakeRebuildMil) ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.rows))
	for id := range f.rows {
		out[id] = true
	}
	return out, false, nil
}

func (f *fakeRebuildMil) Upsert(ctx context.Context, rows []dialogueMilvusRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	for _, r := range rows {
		f.rows[r.RunID] = r
	}
	return nil
}

func (f *fakeRebuildMil) DeleteByRunIDs(ctx context.Context, runIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, runIDs)
	for _, id := range runIDs {
		delete(f.rows, id)
	}
	return nil
}

// Search 按归属过滤派生命中（模拟真实 Milvus 的 expr+语义检索）。
func (f *fakeRebuildMil) Search(ctx context.Context, vec []float32, expr string, topK int) ([]dialogueVecHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []dialogueVecHit
	for _, row := range f.rows {
		if !strings.Contains(expr, fmt.Sprintf("tenant_id == %q", row.TenantID)) {
			continue
		}
		hits = append(hits, dialogueVecHit{RunID: row.RunID, Score: 0.9})
		if len(hits) >= topK {
			break
		}
	}
	return hits, nil
}

// newRebuildDeps 组装重建依赖（fakes 注入；记录绑定名供断言）。
func newRebuildDeps(s *Store, es *fakeRebuildES, mil *fakeRebuildMil, probeDim func(context.Context) (int, error)) dialogueRebuildDeps {
	return dialogueRebuildDeps{
		store:   s,
		esBase:  "tako_dialogue_test",
		milBase: "tako_dialogue_test",
		newES: func(index string) (dialogueESRebuildIndex, error) {
			es.mu.Lock()
			es.boundIndex = index
			es.mu.Unlock()
			return es, nil
		},
		newMil: func(collection string, dim int) (dialogueMilvusRebuildIndex, error) {
			mil.mu.Lock()
			mil.boundColl = collection
			mil.boundDim = dim
			mil.mu.Unlock()
			return mil, nil
		},
		probeDim: probeDim,
		embed:    &fakeDialogueEmbedder{dim: 4},
		counter:  fakeDialogueCounter{def: 1},
		embCfg:   dialogueEmbeddingConfig{Model: "test-embed-model", BatchSize: 2, MaxInputTokens: 100},
		batch:    2,
	}
}

// TestDialogueRebuildFullFlowThenConvergedRerun：全流程建 v2 切指针；重跑收敛为
// 增量校验（不再建 ES 索引、不再切指针、退出码 0）。
func TestDialogueRebuildFullFlowThenConvergedRerun(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_1", "密钥是 key=sk-proj-AbCdEfGhIjKlMnOpQrSt 请保管好")
	seedDialogueChunk(t, s, "r_2", "会话记录：端口 5433 用 go1.27 编译")
	seedDialogueChunk(t, s, "r_3", "发布窗口改到周六凌晨")
	es := newFakeRebuildES()
	mil := newFakeRebuildMil()

	if err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil })); err != nil {
		t.Fatalf("重建: %v", err)
	}

	// 指针已切 v2，绑定名带版本后缀。
	ptr, err := s.dialoguePointer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ptr.IndexVersion != 2 || ptr.EsIndex != "tako_dialogue_test_v2" || ptr.MilvusCollection != "tako_dialogue_test_v2" {
		t.Fatalf("指针 = v%d es=%s mil=%s, want v2/版本化名", ptr.IndexVersion, ptr.EsIndex, ptr.MilvusCollection)
	}
	if es.boundIndex != "tako_dialogue_test_v2" || mil.boundColl != "tako_dialogue_test_v2" {
		t.Fatalf("客户端绑定 = es=%s mil=%s, want _v2 后缀", es.boundIndex, mil.boundColl)
	}

	// ES 状态：3 份文档、版本号 2、正文已脱敏（密钥不落外部）。
	if len(es.docs) != 3 {
		t.Fatalf("ES 文档数 = %d, want 3", len(es.docs))
	}
	doc := es.docs["r_1"]
	if doc.IndexVersion != 2 || doc.EmbeddingModel != "test-embed-model" {
		t.Fatalf("r_1 文档 = version=%d model=%s", doc.IndexVersion, doc.EmbeddingModel)
	}
	if strings.Contains(doc.Passage, "sk-proj-AbCdEfGhIjKlMnOpQrSt") {
		t.Fatal("r_1 passage 未脱敏（原始密钥落了外部索引）")
	}
	if doc.TenantID != testOwner.TenantID || doc.UserID != testOwner.UserID {
		t.Fatalf("r_1 归属 = %s/%s", doc.TenantID, doc.UserID)
	}
	// Milvus 状态：3 行、向量维度 4、版本号 2。
	if len(mil.rows) != 3 {
		t.Fatalf("Milvus 行数 = %d, want 3", len(mil.rows))
	}
	row := mil.rows["r_1"]
	if row.IndexVersion != 2 || len(row.Embedding) != 4 {
		t.Fatalf("r_1 行 = version=%d dim=%d", row.IndexVersion, len(row.Embedding))
	}

	// 重跑：已是活跃版本 → 增量校验（不建 ES 索引、不切指针）。
	ensureBefore := es.ensureCalls
	milEnsureBefore := mil.ensureCalls
	if err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil })); err != nil {
		t.Fatalf("重跑（收敛路径）: %v", err)
	}
	ptr2, _ := s.dialoguePointer(ctx)
	if ptr2.IndexVersion != 2 || ptr2.EsIndex != "tako_dialogue_test_v2" {
		t.Fatalf("重跑动了指针: v%d es=%s", ptr2.IndexVersion, ptr2.EsIndex)
	}
	if es.ensureCalls != ensureBefore {
		t.Fatalf("重跑又建了 ES 索引（ensure %d → %d）", ensureBefore, es.ensureCalls)
	}
	if mil.ensureCalls <= milEnsureBefore {
		t.Fatalf("收敛校验应至少 describe 一次 collection（%d → %d）", milEnsureBefore, mil.ensureCalls)
	}
}

// TestDialogueRebuildCrashResume：回放中途失败（模拟崩溃在半途）——指针不动，
// 修好原因后重跑从 PG 正本重来，最终状态与一次完整重建一致（回放幂等）。
func TestDialogueRebuildCrashResume(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_1", "密钥是 key=sk-proj-AbCdEfGhIjKlMnOpQrSt 请保管好")
	seedDialogueChunk(t, s, "r_2", "会话记录：端口 5433 用 go1.27 编译")
	seedDialogueChunk(t, s, "r_3", "发布窗口改到周六凌晨")
	es := newFakeRebuildES()
	mil := newFakeRebuildMil()
	mil.upsertErr = errors.New("注入：Milvus 写入失败")

	if err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil })); err == nil {
		t.Fatal("注入失败应报错并中止切换")
	}
	ptr, err := s.dialoguePointer(ctx)
	if err != nil || ptr.IndexVersion != 1 {
		t.Fatalf("失败后指针应保持 v1: v%d err=%v", ptr.IndexVersion, err)
	}
	if len(mil.rows) != 0 {
		t.Fatalf("Milvus upsert 整批失败不应留行: %d", len(mil.rows))
	}
	if len(es.docs) != 3 {
		t.Fatalf("ES 臂独立推进（崩溃点在 Milvus 臂）: %d, want 3", len(es.docs))
	}

	// 重跑：同版本号 v2，幂等回放覆盖半成品，最终收敛。
	mil.upsertErr = nil
	if err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil })); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	ptr2, _ := s.dialoguePointer(ctx)
	if ptr2.IndexVersion != 2 || ptr2.EsIndex != "tako_dialogue_test_v2" {
		t.Fatalf("重跑后指针 = v%d es=%s", ptr2.IndexVersion, ptr2.EsIndex)
	}
	if len(es.docs) != 3 || len(mil.rows) != 3 {
		t.Fatalf("重跑后外部状态 = es%d mil%d, want 3/3（幂等覆盖，不重复不缺失）", len(es.docs), len(mil.rows))
	}
}

// TestDialogueRebuildVerifyGateAbortsSwitch：回放有失败 → 列出 run_id 并中止切换，
// 指针保持旧值（§6.4 校验门）。
func TestDialogueRebuildVerifyGateAbortsSwitch(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_1", "会话记录一")
	seedDialogueChunk(t, s, "r_2", "会话记录二")
	seedDialogueChunk(t, s, "r_3", "会话记录三")
	es := newFakeRebuildES()
	mil := newFakeRebuildMil()
	es.upsertErrFor["r_2"] = &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: errors.New("注入：4xx")}

	err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil }))
	if err == nil {
		t.Fatal("回放失败应报错")
	}
	if !strings.Contains(err.Error(), "r_2") {
		t.Fatalf("错误应列出失败 run_id r_2: %v", err)
	}
	ptr, perr := s.dialoguePointer(ctx)
	if perr != nil || ptr.IndexVersion != 1 {
		t.Fatalf("校验门应保住旧指针: v%d err=%v", ptr.IndexVersion, perr)
	}
}

// TestDialogueRebuildDimensionMismatchNoBuild：已存在 collection 维度不符 →
// 119004 永久失败，本轮不建任何东西（ES 索引也不建）、不回放、不切指针。
func TestDialogueRebuildDimensionMismatchNoBuild(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_1", "会话记录")
	es := newFakeRebuildES()
	mil := newFakeRebuildMil()
	mil.ensureErr = &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true, Cause: errors.New("schema dim 8 != probe dim 4")}

	err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 4, nil }))
	if err == nil {
		t.Fatal("维度不符应报错")
	}
	status, code, _, ok := dialogueRetrievalHTTPError(err)
	if !ok || status != 409 || code != codeDialogueDimensionMismatch {
		t.Fatalf("维度不符应映射 119004（409）: status=%d code=%d ok=%v err=%v", status, code, ok, err)
	}
	if es.ensureCalls != 0 {
		t.Fatalf("维度死因下 ES 索引也不应建（ensure %d 次）", es.ensureCalls)
	}
	if len(es.docs) != 0 || len(mil.rows) != 0 {
		t.Fatal("维度不符不应进入回放")
	}
	ptr, _ := s.dialoguePointer(ctx)
	if ptr.IndexVersion != 1 {
		t.Fatalf("指针被动了: v%d", ptr.IndexVersion)
	}
}

// TestDialogueRebuildProbeFailNoBuild：embedding 探针失败（真实调用失败/维度不符）
// → 119004/119003，不建任何外部对象。
func TestDialogueRebuildProbeFailNoBuild(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	seedDialogueChunk(t, s, "r_1", "会话记录")
	es := newFakeRebuildES()
	mil := newFakeRebuildMil()
	probeErr := &dialogueClientError{Class: dlgErrEmbedDimMismatch, Permanent: true, Cause: errors.New("探针维度异常")}

	err := runDialogueRebuild(ctx, newRebuildDeps(s, es, mil, func(context.Context) (int, error) { return 0, probeErr }))
	if err == nil {
		t.Fatal("探针失败应报错")
	}
	if _, code, _, ok := dialogueRetrievalHTTPError(err); !ok || code != codeDialogueDimensionMismatch {
		t.Fatalf("探针维度失败应映射 119004: code=%d ok=%v err=%v", code, ok, err)
	}
	if es.ensureCalls != 0 || mil.ensureCalls != 0 {
		t.Fatalf("探针失败不应建任何对象: es=%d mil=%d", es.ensureCalls, mil.ensureCalls)
	}
}

// TestDialogueRebuildVersionedName：版本化命名与收敛判定的口径（基名_vN）。
func TestDialogueRebuildVersionedName(t *testing.T) {
	got := dialogueVersionedName("tako_dialogue_v1", 3)
	if got != "tako_dialogue_v1_v3" {
		t.Fatalf("dialogueVersionedName = %q", got)
	}
	// Milvus collection 名只允许字母/数字/下划线——版本名不得含连字符。
	if strings.ContainsAny(got, "-") {
		t.Fatalf("版本名含连字符（Milvus 拒收）: %q", got)
	}
}
