package runengine

// 统一抽取器三段流的确定性测试（doc/65 §4.7）。
//
// **模型行为不进单测**：用脚本化假模型（按调用次序吐固定 JSON）驱动，验的全是
// 程序侧规则——条目准入、分批全覆盖、相关集合只进最终裁决、预算超了明确失败、
// 重放不重新分析。真模型的行为归活体验收（memory_pipeline_live.sh）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// memoryScriptModel 按调用次序吐预设响应；记录每次调用的 user 输入（断言
// 「最终裁决只看相关候选」就靠它——检查第③次调用里出现了谁、没出现谁）。
type memoryScriptModel struct {
	mu        sync.Mutex
	responses []string
	calls     []string
}

func (f *memoryScriptModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responses) == 0 {
		return nil, errors.New("脚本用尽（测试收到了没安排的模型调用）")
	}
	out := f.responses[0]
	f.responses = f.responses[1:]
	if len(msgs) > 1 {
		f.calls = append(f.calls, msgs[1].Content)
	}
	return &schema.Message{Content: out}, nil
}

func (f *memoryScriptModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("抽取器不该走流式")
}

// newScriptExtractor 造一个带脚本模型的抽取器（走真 PG 测试库）。
func newScriptExtractor(t *testing.T, s *Store, responses ...string) (*memoryExtractor, *memoryScriptModel) {
	t.Helper()
	m := &memoryScriptModel{responses: responses}
	return newMemoryExtractor(s, func(context.Context) (model.BaseChatModel, error) { return m, nil }, nil), m
}

// claimJobFor 把一条已登记的任务领到手（消费者认领后的形状：running + epoch + 租约）。
func claimJobFor(t *testing.T, s *Store, id string) *MemoryJob {
	t.Helper()
	job, err := s.claimMemoryJobByID(context.Background(), id, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("认领 %s: job=%+v err=%v", id, job, err)
	}
	return job
}

// TestMemoryExtractEntriesDropsInvalid：单个条目字段非法 → 丢该条目，其他继续
// （doc/65 §4.8 失败语义表）；quote 超长**截**不拒（它是原话引用，截断不丢事实）。
func TestMemoryExtractEntriesDropsInvalid(t *testing.T) {
	e, m := newScriptExtractor(t, nil, `{"entries":[
		{"scope":"team","content":"scope 非法，丢"},
		{"scope":"user","content":"  ", "quote":"空正文，丢"},
		{"scope":"user","content":"正文超过两百字……`+strings.Repeat("长", 300)+`","quote":"超长，丢"},
		{"scope":"user","content":"好的这条留下","quote":"`+strings.Repeat("话", 300)+`"},
		{"scope":"workspace","content":"这条也留下","quote":""}
	]}`)
	entries, err := e.extractEntries(context.Background(), m, &MemoryJob{Kind: MemoryJobKindInrun}, "现场")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("留下 %d 条, want 2（scope 非法/空正文/超长正文三条丢弃）：%+v", len(entries), entries)
	}
	if entries[0].Content != "好的这条留下" || entries[1].Content != "这条也留下" {
		t.Fatalf("留下的内容不对：%+v", entries)
	}
	if got := utf8Count(entries[0].Quote); got > memoryQuoteCap {
		t.Fatalf("quote %d 字, want 截到 %d", got, memoryQuoteCap)
	}
	if m.callCount() != 1 {
		t.Fatalf("抽条目只该调一次模型，实际 %d 次", m.callCount())
	}
}

// utf8Count 数 rune（测试里到处用得到）。
func utf8Count(s string) int { return len([]rune(s)) }

func (f *memoryScriptModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestMemoryZeroEntriesFinishesJob：没有要记的 → 空批次收尾（job done），
// **不打候选**（分批与最终裁决都不该发生）。
func TestMemoryZeroEntriesFinishesJob(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_z", SessionID: "s_z", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindPostrun,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, m := newScriptExtractor(t, s, `{"entries":[]}`)
	res, err := e.process(ctx, claimJobFor(t, s, id))
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 0 || res.Replay {
		t.Fatalf("res = %+v, want Entries=0", res)
	}
	if m.callCount() != 1 {
		t.Fatalf("模型调用 %d 次, want 1（只有抽条目；没有条目就不该再打候选）", m.callCount())
	}
	row := s.jobOf(t, "r_z")
	if row.Status != MemoryJobStatusDone || row.BatchCommittedAt == nil {
		t.Fatalf("job = %+v, want done + 批次已提交", row)
	}
	if n := s.count(t, &MemoryItem{}, "1 = 1"); n != 0 {
		t.Fatalf("记忆多出 %d 行, want 0", n)
	}
}

// TestMemoryCandidatesBatchFullCoverage：候选全量分批，**每条恰好进一批**
// ——第 151 条及之后必须出现在某一批里（doc/65 §4.7：不设 150 行上限）。
func TestMemoryCandidatesBatchFullCoverage(t *testing.T) {
	e := newMemoryExtractor(nil, nil, nil)
	var candidates []memoryExtractCandidate
	for i := 1; i <= 160; i++ {
		candidates = append(candidates, memoryExtractCandidate{
			Index: i,
			Item:  MemoryItem{ID: fmt.Sprintf("mem_%04d", i), Content: fmt.Sprintf("记忆正文第%d条", i)},
		})
	}
	batches := e.batchCandidates(candidates)
	if len(batches) < 2 {
		t.Fatalf("160 条只分成 %d 批, want 多批", len(batches))
	}
	seen := map[int]int{}
	for bi, batch := range batches {
		for _, c := range batch {
			seen[c.Index] = bi
		}
	}
	for i := 1; i <= 160; i++ {
		if _, ok := seen[i]; !ok {
			t.Fatalf("候选 #%d 没进任何一批（第 151 条之后的候选也必须参与）", i)
		}
	}
	if len(seen) != 160 {
		t.Fatalf("有候选进了多批：%d 个去重序号, want 160", len(seen))
	}
}

// TestMemoryFinalSeesRelevantOnly：最终裁决的输入 = 新条目 + **相关候选**——
// 不相关的候选不许出现在第③次调用的输入里（分批筛的意义就在这）。
func TestMemoryFinalSeesRelevantOnly(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "无关记忆甲"})
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "相关记忆乙"})
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser, Content: "无关记忆丙"})
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_rel", SessionID: "s_rel", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindInrun, TargetLayer: MemoryLayerRecall, Quote: "记住乙", SourceSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 候选序号按 **id 排序**（同毫秒 ULID 不保证播种顺序），序号要动态算，不能写死。
	yi := candidateIndexOf(t, s, id, "相关记忆乙")
	e, m := newScriptExtractor(t, s,
		`{"entries":[{"scope":"user","content":"相关记忆乙的新说法","quote":"记住乙"}]}`,
		fmt.Sprintf(`{"relevant":[%d]}`, yi),
		fmt.Sprintf(`{"actions":[{"op":"replace","targets":[%d],"scope":"user","content":"相关记忆乙的新说法","quote":"记住乙"}]}`, yi),
	)
	res, err := e.process(ctx, claimJobFor(t, s, id))
	if err != nil {
		t.Fatal(err)
	}
	if m.callCount() != 3 {
		t.Fatalf("模型调用 %d 次, want 3（抽条目/筛相关/最终裁决）", m.callCount())
	}
	if res.Relevant != 1 || res.Actions != 1 {
		t.Fatalf("res = %+v, want relevant=1 actions=1", res)
	}
	final := m.calls[2]
	if !strings.Contains(final, "相关记忆乙的新说法") || !strings.Contains(final, "相关记忆乙") {
		t.Errorf("最终裁决输入缺新条目/相关候选：\n%s", final)
	}
	if strings.Contains(final, "无关记忆甲") || strings.Contains(final, "无关记忆丙") {
		t.Errorf("最终裁决的输入混进了不相关候选（甲/丙不该出现）：\n%s", final)
	}
	// replace 生效：乙被新行替代，指针指向新行。
	var newItem MemoryItem
	if err := s.db.Where("content = ?", "相关记忆乙的新说法").First(&newItem).Error; err != nil {
		t.Fatalf("新行没写进去: %v", err)
	}
	var oldRow MemoryItem
	if err := s.db.Where("content = ?", "相关记忆乙").First(&oldRow).Error; err != nil {
		t.Fatal(err)
	}
	if oldRow.State != MemoryStateSuperseded || oldRow.SupersededBy == nil || *oldRow.SupersededBy != newItem.ID {
		t.Fatalf("旧行 = %+v, want superseded 且指向新行", oldRow)
	}
	if newItem.Layer != MemoryLayerRecall {
		t.Fatalf("新行层 = %s, want recall（inrun 用 job.target_layer，提取器不能换层）", newItem.Layer)
	}
}

// candidateIndexOf 按抽取器同款口径算候选序号（读全部 active，按 id 排序，返回目标正文的序号）。
func candidateIndexOf(t *testing.T, s *Store, jobID, content string) int {
	t.Helper()
	job, err := s.GetMemoryJob(context.Background(), jobID)
	if err != nil || job == nil {
		t.Fatalf("读任务 %s: %v", jobID, err)
	}
	items, err := s.listActiveMemoryForExtraction(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	for i, it := range items {
		if it.Content == content {
			return i + 1
		}
	}
	t.Fatalf("候选里没有 %q", content)
	return 0
}

// TestMemoryFinalOverBudgetFails：最终相关集合超预算 → **明确失败**（不截断、
// 不随机选、不先写一半），行不写、批次不记（doc/65 §4.7）。
func TestMemoryFinalOverBudgetFails(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	seedMem(t, s, MemoryItem{Layer: MemoryLayerRecall, Scope: MemoryScopeUser,
		Content: strings.Repeat("超", memoryFinalTokens*2)})
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_bud", SessionID: "s_bud", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindInrun, TargetLayer: MemoryLayerRecall, Quote: "改记忆", SourceSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := newScriptExtractor(t, s,
		`{"entries":[{"scope":"user","content":"新说法","quote":"改记忆"}]}`,
		`{"relevant":[1]}`,
		// 第③次调用不该发生——预算闸在调用前就把 job 打回重试。
		`{"actions":[]}`,
	)
	res, err := e.process(ctx, claimJobFor(t, s, id))
	if err == nil {
		t.Fatalf("相关集合超预算必须明确失败，res=%+v", res)
	}
	if !strings.Contains(err.Error(), "超预算") {
		t.Fatalf("错误 = %v, want 明确说超预算", err)
	}
	row := s.jobOf(t, "r_bud")
	if row.BatchCommittedAt != nil {
		t.Fatalf("失败任务不许记批次：batch_committed_at=%v", row.BatchCommittedAt)
	}
	if n := s.count(t, &MemoryItem{}, "content = ?", "新说法"); n != 0 {
		t.Fatalf("失败任务写进了 %d 行, want 0（不先写一半）", n)
	}
}

// TestMemoryFinalTooManyActionsRejected：动作 > 20 被拒（抽取器侧与事务内侧都查）。
func TestMemoryFinalTooManyActionsRejected(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_many", SessionID: "s_many", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindInrun, TargetLayer: MemoryLayerRecall, Quote: "记", SourceSeq: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var many []string
	for i := 0; i < memoryMaxActions+1; i++ {
		many = append(many, fmt.Sprintf(`{"op":"insert","scope":"user","content":"第%d条","quote":"记"}`, i))
	}
	e, _ := newScriptExtractor(t, s,
		`{"entries":[{"scope":"user","content":"第0条","quote":"记"}]}`,
		// 场上没有已有记忆 → 筛选步零批、不打模型，脚本直接接最终裁决。
		`{"actions":[`+strings.Join(many, ",")+`]}`,
	)
	job := claimJobFor(t, s, id)
	if _, err := e.process(ctx, job); err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("动作 %d 条应被拒, err=%v", memoryMaxActions+1, err)
	}
	// 事务内的第二道：绕过抽取器，直接把超量动作喂给写库。
	if _, err := e.applyActions(ctx, job, nil, make([]memoryAction, memoryMaxActions+1)); err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("事务内也应拒: err=%v", err)
	}
}

// TestMemoryProcessReplaySkipsAnalysis：批次已提交的任务重放 → 不打模型、不新增行
// （doc/65 §4.8：看到已提交标记直接收尾）。
func TestMemoryProcessReplaySkipsAnalysis(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	id, err := s.RegisterMemoryJob(ctx, MemoryJobSpec{
		RunID: "r_re", SessionID: "s_re", TenantID: "t1", UserID: "u1", Workspace: "p1",
		Kind: MemoryJobKindPostrun,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := claimJobFor(t, s, id)
	now := time.Now()
	job.BatchCommittedAt = &now
	job.Status = MemoryJobStatusDone
	e, m := newScriptExtractor(t, s) // 没有脚本：任何模型调用都会报「脚本用尽」
	res, err := e.process(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replay {
		t.Fatalf("res = %+v, want Replay=true", res)
	}
	if m.callCount() != 0 {
		t.Fatalf("重放不该打模型，实际 %d 次", m.callCount())
	}
}
