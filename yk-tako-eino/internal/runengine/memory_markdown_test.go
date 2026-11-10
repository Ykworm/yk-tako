// 本文件：markdown 记忆模式的单测。
//
// 钉住的东西（阶段说明 §8 的必测项）：
//
//	条目操作：add / replace / remove / batch 的结果；精确重复 add → no-op 且文件字节不变。
//	replace：整条目替换；0 命中与多命中拒绝；拒绝时文件字节不变。
//	容量：超预算拒绝且旧文件字节不变；回执里的已用 rune 是真实值。
//	batch：任一操作非法 → 一个字节都不写。
//	非法 UTF-8 / 读取失败 → 拒绝写入（不当空文件覆盖）。
//	锁：两个写者同文件排队；不同文件可并行；锁键是**逻辑键**（不是文件路径）。
//	模式门：markdown 模式下 master 有 memory 工具、subagent 没有；没有 search_memory；
//	        不登记 memory_jobs、不登记 postrun。
//	快照：场中写入不改变本场快照；下一场读到新版；文件不可读 → 快照失败。
//	PII：密钥拒收（markdown 路径）。
package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ── 测试底座 ─────────────────────────────────────────────────────────────────

// newMarkdownStore 造一个 markdown 模式的 Store。
//
// 不连库：这条路径不碰 PG（文件 + 锁而已）。真要跨副本互斥的用例另用 newTestStore。
func newMarkdownStore(t *testing.T, root string) *Store {
	t.Helper()
	s := &Store{}
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Mode: MemoryModeMarkdown})
	return s
}

// markdownCtx 造一个带身份（租户 t1 / 用户 u1）与项目引用的 ctx。
func markdownCtx(workspaceID string) context.Context {
	ctx := withScope(context.Background(), dataScope{TenantID: "t1", UserID: "u1", Scope: "user"})
	return withWorkspace(ctx, WorkspaceRef{
		TenantID: "t1", Root: "/tmp/md-test", TenantRoot: "/tmp/md-test", WorkspaceID: workspaceID,
	})
}

// markdownUserPath / markdownWorkspacePath 是两个文件的真实路径（断言用）。
func markdownUserPath(root string) string {
	return filepath.Join(root, "tenants", "t1", "users", "u1", "USER.md")
}

func markdownWorkspacePath(root, wid string) string {
	return filepath.Join(root, "tenants", "t1", "workspaces", wid, "MEMORY.md")
}

// mustAdd 走真实的写入路径（writeMemoryMarkdown），失败即测试失败。
func mustAdd(t *testing.T, s *Store, ctx context.Context, target, content string) memoryMarkdownResult {
	t.Helper()
	res, err := s.writeMemoryMarkdown(ctx, target, "add", []memoryOp{{Action: "add", Content: content}})
	if err != nil {
		t.Fatalf("写记忆失败: %v", err)
	}
	if res.Fail != "" {
		t.Fatalf("写记忆被拒: %s（%s）", res.Fail, res.Detail)
	}
	return res
}

// readFileBytes 读文件字节；文件不存在返回 nil。
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读 %s: %v", path, err)
	}
	return b
}

// ── 条目操作 ─────────────────────────────────────────────────────────────────

// TestMemoryMarkdownAddReplaceRemoveBatch：四个动作各自的结果 + 文件的真实字节形态。
func TestMemoryMarkdownAddReplaceRemoveBatch(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	// add：文件还不存在 = 空记忆（正常起点，允许写）。
	res := mustAdd(t, s, ctx, memoryTargetUser, "deck 放在 output/deck.html")
	if res.FileName != "USER.md" {
		t.Errorf("回执文件名 = %q, want USER.md", res.FileName)
	}
	if res.Action != "add" {
		t.Errorf("回执动作 = %q, want add", res.Action)
	}
	if res.Used != len([]rune("deck 放在 output/deck.html")) {
		t.Errorf("已用 rune = %d, want %d", res.Used, len([]rune("deck 放在 output/deck.html")))
	}
	if res.Cap != memoryUserCap {
		t.Errorf("上限 = %d, want %d", res.Cap, memoryUserCap)
	}
	if got := string(readFileBytes(t, userPath)); got != "deck 放在 output/deck.html" {
		t.Errorf("文件内容 = %q", got)
	}

	// 第二条：两条之间严格用 \n§\n。
	mustAdd(t, s, ctx, memoryTargetUser, "回复别用 emoji")
	want := "deck 放在 output/deck.html" + memoryEntrySep + "回复别用 emoji"
	if got := string(readFileBytes(t, userPath)); got != want {
		t.Errorf("两条文件内容 = %q, want %q", got, want)
	}

	// replace：整条目替换（不是只换命中的那一片）。
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "replace", []memoryOp{
		{Action: "replace", OldText: "deck 放在 output/deck.html", NewContent: "deck 放在 output/v2.html"},
	})
	if err != nil || res.Fail != "" {
		t.Fatalf("replace 被拒: %v %s %s", err, res.Fail, res.Detail)
	}
	want = "deck 放在 output/v2.html" + memoryEntrySep + "回复别用 emoji"
	if got := string(readFileBytes(t, userPath)); got != want {
		t.Errorf("replace 后文件 = %q, want %q（整条目被换掉）", got, want)
	}

	// remove：按条目原文删掉整条。
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "remove", []memoryOp{
		{Action: "remove", OldText: "回复别用 emoji"},
	})
	if err != nil || res.Fail != "" {
		t.Fatalf("remove 被拒: %v %s %s", err, res.Fail, res.Detail)
	}
	if got := string(readFileBytes(t, userPath)); got != "deck 放在 output/v2.html" {
		t.Errorf("remove 后文件 = %q", got)
	}
	if res.Used != len([]rune("deck 放在 output/v2.html")) {
		t.Errorf("remove 后已用 = %d", res.Used)
	}

	// batch：同一份文件里按顺序做多个操作。
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "batch", []memoryOp{
		{Action: "add", Content: "先给结论"},
		{Action: "replace", OldText: "deck 放在 output/v2.html", NewContent: "deck 放在 output/v3.html"},
	})
	if err != nil || res.Fail != "" {
		t.Fatalf("batch 被拒: %v %s %s", err, res.Fail, res.Detail)
	}
	if res.Action != "batch" {
		t.Errorf("batch 回执动作 = %q", res.Action)
	}
	// add 追加到尾部，replace 就地替换整条 —— 顺序按操作发生的次序。
	want = "deck 放在 output/v3.html" + memoryEntrySep + "先给结论"
	if got := string(readFileBytes(t, userPath)); got != want {
		t.Errorf("batch 后文件 = %q, want %q", got, want)
	}
}

// TestMemoryMarkdownExactDuplicateAddIsNoOp：精确重复的 add 是 no-op——不写盘、字节不变。
func TestMemoryMarkdownExactDuplicateAddIsNoOp(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	mustAdd(t, s, ctx, memoryTargetUser, "deck 放在 output/deck.html")
	before := readFileBytes(t, userPath)

	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "deck 放在 output/deck.html"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != "" {
		t.Fatalf("精确重复不该被拒: %s %s", res.Fail, res.Detail)
	}
	if !res.NoOp {
		t.Errorf("精确重复的 add 必须是 no-op（回执 NoOp=false）")
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("no-op 之后文件字节变了：%q → %q", before, after)
	}
	if res.Used != memoryEntriesRunes([]string{"deck 放在 output/deck.html"}) {
		t.Errorf("no-op 回执的已用 = %d, want 当前用量", res.Used)
	}
	if !strings.Contains(memoryMarkdownResultText(res), "no-op") {
		t.Errorf("no-op 回执没说明是 no-op: %q", memoryMarkdownResultText(res))
	}
}

// TestMemoryMarkdownReplaceZeroAndMultiHitRejected：0 命中与多命中都拒绝，且文件字节不变。
func TestMemoryMarkdownReplaceZeroAndMultiHitRejected(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	mustAdd(t, s, ctx, memoryTargetUser, "deck 放在 output/deck.html")
	mustAdd(t, s, ctx, memoryTargetUser, "回复别用 emoji")
	mustAdd(t, s, ctx, memoryTargetUser, "构建产物放 output/dist")
	before := readFileBytes(t, userPath)

	// 0 命中：old_text 不是任何一条的原文，也没唯一命中某一条。
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "replace", []memoryOp{
		{Action: "replace", OldText: "完全不存在的原文", NewContent: "x"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailLocate {
		t.Fatalf("0 命中的失败分类 = %q, want %q", res.Fail, mdFailLocate)
	}
	if res.LocateHits != 0 {
		t.Errorf("0 命中的命中数 = %d, want 0", res.LocateHits)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("0 命中拒绝后文件字节变了")
	}

	// 多命中：old_text 同时命中两条不同的条目 → 不确定是哪条 → 拒绝。
	// （"output" 不是任何一条的原文，但出现在两条里 → 走「唯一子串」这条路且命中多条。）
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "replace", []memoryOp{
		{Action: "replace", OldText: "output", NewContent: "x"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailLocate {
		t.Fatalf("多命中的失败分类 = %q, want %q", res.Fail, mdFailLocate)
	}
	if res.LocateHits < 2 {
		t.Errorf("多命中的命中数 = %d, want >=2", res.LocateHits)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("多命中拒绝后文件字节变了")
	}

	// remove 的多命中同样拒绝。
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "remove", []memoryOp{
		{Action: "remove", OldText: "output"},
	})
	if err != nil || res.Fail != mdFailLocate {
		t.Fatalf("remove 多命中应被拒: %v %s", err, res.Fail)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("remove 多命中拒绝后文件字节变了")
	}
}

// TestMemoryMarkdownCapacityRejected：超预算拒绝，旧文件字节不变，回执的已用是真实值。
func TestMemoryMarkdownCapacityRejected(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	// 先写满到接近上限（用户文件上限 1375 rune）。
	filler := strings.Repeat("甲", memoryUserCap-10)
	res := mustAdd(t, s, ctx, memoryTargetUser, filler)
	if res.Used != memoryUserCap-10 {
		t.Fatalf("已用 = %d, want %d", res.Used, memoryUserCap-10)
	}
	before := readFileBytes(t, userPath)

	// 再加一条就超了 → 拒绝，旧字节不变。
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: strings.Repeat("乙", 100)},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailOverCap {
		t.Fatalf("超预算的失败分类 = %q, want %q", res.Fail, mdFailOverCap)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("超预算拒绝后文件字节变了")
	}
	// 回执里的已用是**当前真实用量**（不是 0，也不是猜的）。
	if res.Used != memoryUserCap-10 {
		t.Errorf("超预算回执的已用 = %d, want %d（真实当前用量）", res.Used, memoryUserCap-10)
	}
	if !strings.Contains(memoryMarkdownResultText(res), "已用：") {
		t.Errorf("回执缺已用行: %q", memoryMarkdownResultText(res))
	}

	// replace 也要算容量：换成更长的句子会超 → 拒绝。
	long := strings.Repeat("丙", memoryUserCap+10)
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "replace", []memoryOp{
		{Action: "replace", OldText: filler, NewContent: long},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailOverCap {
		t.Fatalf("replace 超预算的失败分类 = %q, want %q", res.Fail, mdFailOverCap)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("replace 超预算拒绝后文件字节变了")
	}
}

// TestMemoryMarkdownBatchAllOrNothing：任一操作非法 → 一个字节都不写。
func TestMemoryMarkdownBatchAllOrNothing(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	mustAdd(t, s, ctx, memoryTargetUser, "已有的一条")
	before := readFileBytes(t, userPath)

	// 第二条操作非法（old_text 命中不到）→ 整批不写。
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "batch", []memoryOp{
		{Action: "add", Content: "这一条本来是好的"},
		{Action: "remove", OldText: "命中的原文"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailLocate {
		t.Fatalf("batch 失败的分类 = %q, want %q", res.Fail, mdFailLocate)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("batch 失败后文件字节变了：%q → %q", before, after)
	}

	// 动作名非法 → 同样整批不写。
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "batch", []memoryOp{
		{Action: "add", Content: "这一条本来是好的"},
		{Action: "explode", Content: "炸"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailAction {
		t.Fatalf("非法动作的分类 = %q, want %q", res.Fail, mdFailAction)
	}
	if after := readFileBytes(t, userPath); string(after) != string(before) {
		t.Errorf("batch 非法动作后文件字节变了")
	}
}

// TestMemoryMarkdownRejectsUnreadableFile：非法 UTF-8 / 非规范格式 → 拒绝写入，不当空文件覆盖。
func TestMemoryMarkdownRejectsUnreadableFile(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// 非法 UTF-8 字节。
	bad := []byte{0xff, 0xfe, 'x', 'y'}
	if err := os.WriteFile(userPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "新的一条"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailFile {
		t.Fatalf("非法 UTF-8 的失败分类 = %q, want %q", res.Fail, mdFailFile)
	}
	if after := readFileBytes(t, userPath); string(after) != string(bad) {
		t.Errorf("读不出的文件被覆盖了：%q → %q", bad, after)
	}
	if res.UsageKnown {
		t.Errorf("读不出文件时不该报已用数（会假装知道）")
	}

	// 非规范形态（CRLF 行尾 / 条目首尾带空格）→ 同样拒绝。
	nonCanonical := "第一条 \r\n" + memoryEntrySep + "第二条"
	if err := os.WriteFile(userPath, []byte(nonCanonical), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "新的一条"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailFile {
		t.Fatalf("非规范格式的失败分类 = %q, want %q", res.Fail, mdFailFile)
	}
	if after := readFileBytes(t, userPath); string(after) != nonCanonical {
		t.Errorf("非规范文件被覆盖了")
	}
}

// TestMemoryMarkdownEmptyFileIsEmptyMemory：空文件 = 空记忆（不是「格式坏」）。
func TestMemoryMarkdownEmptyFileIsEmptyMemory(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, ctx, memoryTargetUser, "第一条")
	if got := string(readFileBytes(t, userPath)); got != "第一条" {
		t.Errorf("空文件之后写入 = %q", got)
	}
}

// ── 锁 ───────────────────────────────────────────────────────────────────────

// mdKeyLocker 记下它拿到的逻辑键。
type mdKeyLocker struct {
	mu   *sync.Mutex
	keys *[]string
}

func (l mdKeyLocker) Lock(_ context.Context, keys []string) (func(), error) {
	l.mu.Lock()
	*l.keys = append(*l.keys, keys...)
	l.mu.Unlock()
	return func() {}, nil
}

// TestMemoryMarkdownLockKeyIsLogical：锁键是 user:<userId> / workspace:<workspaceId>，不是文件路径。
func TestMemoryMarkdownLockKeyIsLogical(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	var mu sync.Mutex
	var keys []string
	base := markdownCtx("w1")
	ctx := WithFileLocker(base, mdKeyLocker{mu: &mu, keys: &keys})

	mustAdd(t, s, ctx, memoryTargetUser, "用户偏好")
	mustAdd(t, s, ctx, memoryTargetWorkspace, "项目事实")

	mu.Lock()
	got := append([]string(nil), keys...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("取了 %d 把锁, want 2: %v", len(got), got)
	}
	if got[0] != "user:u1" {
		t.Errorf("用户文件的锁键 = %q, want user:u1", got[0])
	}
	if got[1] != "workspace:w1" {
		t.Errorf("项目文件的锁键 = %q, want workspace:w1", got[1])
	}
	for _, k := range got {
		if strings.Contains(k, "/") || strings.HasSuffix(k, ".md") {
			t.Errorf("锁键 %q 不该是文件路径（多副本要按逻辑键互斥）", k)
		}
	}
}

// mdHoldLocker 是「取到锁后停一会儿」的锁：用来确定性地验临界区不交叠。
type mdHoldLocker struct {
	inner FileLocker
	name  string
	mu    *sync.Mutex
	seq   *[]string
	hold  time.Duration
}

func (l mdHoldLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	rel, err := l.inner.Lock(ctx, keys)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	*l.seq = append(*l.seq, "enter-"+l.name)
	l.mu.Unlock()
	time.Sleep(l.hold)
	return func() {
		rel()
		l.mu.Lock()
		*l.seq = append(*l.seq, "exit-"+l.name)
		l.mu.Unlock()
	}, nil
}

// TestMemoryMarkdownSameFileWritersQueue：两个写者写同一份文件 → 临界区不交叠、不丢更新。
//
// 用**真 PG advisory lock**（跨副本的那把）：两个 Store 指向同一个库 = 两个副本。
func TestMemoryMarkdownSameFileWritersQueue(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	root := t.TempDir()
	for _, s := range []*Store{storeA, storeB} {
		s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Mode: MemoryModeMarkdown})
	}
	ctxA := markdownCtx("w1")
	ctxB := markdownCtx("w1")

	var mu sync.Mutex
	var seq []string
	lockerA := mdHoldLocker{inner: newPGFileLocker(storeA.db, 0), name: "A", mu: &mu, seq: &seq, hold: 120 * time.Millisecond}
	lockerB := mdHoldLocker{inner: newPGFileLocker(storeB.db, 0), name: "B", mu: &mu, seq: &seq, hold: 120 * time.Millisecond}
	ctxA = WithFileLocker(ctxA, lockerA)
	ctxB = WithFileLocker(ctxB, lockerB)

	var wg sync.WaitGroup
	for i, c := range []struct {
		store *Store
		ctx   context.Context
		text  string
	}{{storeA, ctxA, "A 记的一条"}, {storeB, ctxB, "B 记的一条"}} {
		wg.Add(1)
		go func(i int, st *Store, cx context.Context, text string) {
			defer wg.Done()
			res, err := st.writeMemoryMarkdown(cx, memoryTargetUser, "add", []memoryOp{{Action: "add", Content: text}})
			if err != nil {
				t.Errorf("写失败: %v", err)
				return
			}
			if res.Fail != "" {
				t.Errorf("写被拒: %s %s", res.Fail, res.Detail)
			}
		}(i, c.store, c.ctx, c.text)
	}
	wg.Wait()

	mu.Lock()
	got := strings.Join(seq, ",")
	mu.Unlock()
	if got != "enter-A,exit-A,enter-B,exit-B" && got != "enter-B,exit-B,enter-A,exit-A" {
		t.Fatalf("两个写者的临界区交叠了：%q", got)
	}
	// 锁内重读 → 两笔都在（不丢更新）。
	entries, err := readMemoryEntries(markdownUserPath(root))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("文件里 %d 条, want 2（丢更新了）: %v", len(entries), entries)
	}
}

// TestMemoryMarkdownDifferentFilesRunInParallel：不同项目的文件 → 两把锁不互相阻塞。
func TestMemoryMarkdownDifferentFilesRunInParallel(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	root := t.TempDir()
	store.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Mode: MemoryModeMarkdown})

	var mu sync.Mutex
	var seq []string
	locker := mdHoldLocker{inner: newPGFileLocker(store.db, 0), name: "A", mu: &mu, seq: &seq, hold: 150 * time.Millisecond}
	ctx1 := WithFileLocker(markdownCtx("w1"), locker)
	ctx2 := WithFileLocker(markdownCtx("w2"), locker)

	var wg sync.WaitGroup
	for _, c := range []struct {
		ctx    context.Context
		target string
	}{{ctx1, memoryTargetWorkspace}, {ctx2, memoryTargetWorkspace}} {
		wg.Add(1)
		go func(cx context.Context, target string) {
			defer wg.Done()
			res, err := store.writeMemoryMarkdown(cx, target, "add", []memoryOp{{Action: "add", Content: "各写各的"}})
			if err != nil {
				t.Errorf("写失败: %v", err)
				return
			}
			if res.Fail != "" {
				t.Errorf("写被拒: %s %s", res.Fail, res.Detail)
			}
		}(c.ctx, c.target)
	}
	wg.Wait()

	mu.Lock()
	got := strings.Join(seq, ",")
	mu.Unlock()
	// 两个 enter 都在第一个 exit 之前 = 两把锁没互相等。
	if !strings.HasPrefix(got, "enter-A,enter-A") {
		t.Fatalf("不同项目的写入被串行化了：%q", got)
	}
	for _, wid := range []string{"w1", "w2"} {
		entries, err := readMemoryEntries(markdownWorkspacePath(root, wid))
		if err != nil {
			t.Fatalf("读 %s: %v", wid, err)
		}
		if len(entries) != 1 {
			t.Errorf("%s 的文件里 %d 条, want 1", wid, len(entries))
		}
	}
}

// ── 模式门 ───────────────────────────────────────────────────────────────────

// TestMemoryMarkdownModeGate：markdown 模式下 master 有 memory 工具、subagent 没有；
// 没有 search_memory；不登记 memory_jobs、不登记 postrun。
func TestMemoryMarkdownModeGate(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	root := t.TempDir()
	store.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Mode: MemoryModeMarkdown})
	agentsDir := t.TempDir()

	w, err := NewWorker(store, &fakeModel{}, claudeChunkText, "系统提示", root, agentsDir, time.Minute,
		0, "main/ref", 0, 2, 4096, nil, fakeCounter{f: func(string) int { return 0 }}, false, 0)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	// master 的池里有 memory（同步条目工具）。
	if findInvokable(t, w.tools, memoryToolName) == nil {
		t.Fatalf("markdown 模式下 master 的工具池里没有 memory")
	}
	// 没有 search_memory（markdown 模式不提供主动历史查询）。
	for _, tt := range w.tools {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "search_memory" {
			t.Fatalf("markdown 模式下不该有 search_memory")
		}
	}
	// subagent 继承的池里**没有** memory（dropMemoryAndSearchTools 一并剔 search_memory）。
	memTool := findInvokable(t, w.tools, memoryToolName)
	other, err := newGetTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	pool := dropMemoryAndSearchTools(context.Background(), []tool.BaseTool{memTool, other})
	if len(pool) != 1 {
		t.Fatalf("剔掉 memory 后池里 %d 把, want 1", len(pool))
	}
	info, err := pool[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name == memoryToolName {
		t.Fatalf("subagent 的池里还有 memory")
	}

	// 不登记 memory_jobs / postrun：定稿一场 run 之后 memory_jobs 仍是空的。
	store.completeRun(t, "s_md", "r_md", "hi")
	if n := store.count(t, &MemoryJob{}, ""); n != 0 {
		t.Fatalf("markdown 模式登记了 %d 条 memory_jobs, want 0", n)
	}
	// 不注入每回合 BM25 召回：markdown 模式的记忆召回开关是关的。
	res, err := store.Recall(context.Background(), "deck 放在 output/deck.html")
	if err != nil {
		t.Fatalf("召回: %v", err)
	}
	if res.MemoryHits != 0 || res.Block != "" {
		t.Fatalf("markdown 模式不该注入记忆召回块: hits=%d block=%q", res.MemoryHits, res.Block)
	}
}

// hasTool 只判在不在（不 Fatal）：模式门测试要断言「不在池」。
func hasTool(ts []tool.BaseTool, name string) bool {
	for _, tt := range ts {
		info, err := tt.Info(context.Background())
		if err == nil && info.Name == name {
			return true
		}
	}
	return false
}

// TestMemoryPipelineModeGate：pipeline 模式的工具形状由两个开关决定（doc/65 §6.2/§6.4）——
// Extract=on 挂派工 memory；Recall=on 挂 search_memory；Extract=off 不挂 memory
// （没有消费者，登记的任务只会永远 pending）。两把都只给 master，subagent 一律剔掉。
func TestMemoryPipelineModeGate(t *testing.T) {
	cases := []struct {
		name            string
		extract, recall bool
		wantMemory      bool
		wantSearch      bool
	}{
		{"两开关全开", true, true, true, true},
		{"只抽取", true, false, true, false},
		{"只召回", false, true, false, true},
		{"全关", false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newTestStore(t, testLease, testTTL)
			root := t.TempDir()
			store.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: root, Extract: c.extract, Recall: c.recall})
			w, err := NewWorker(store, &fakeModel{}, claudeChunkText, "系统提示", root, t.TempDir(), time.Minute,
				0, "main/ref", 0, 2, 4096, nil, fakeCounter{f: func(string) int { return 0 }}, false, 0)
			if err != nil {
				t.Fatalf("NewWorker: %v", err)
			}
			mem := hasTool(w.tools, memoryToolName)
			if mem != c.wantMemory {
				t.Fatalf("memory 在池 = %v, want %v", mem, c.wantMemory)
			}
			search := hasTool(w.tools, searchMemoryToolName)
			if search != c.wantSearch {
				t.Fatalf("search_memory 在池 = %v, want %v", search, c.wantSearch)
			}
			// subagent 的 base 快照：memory 与 search_memory 都被剔掉。
			other, err := newGetTimeTool()
			if err != nil {
				t.Fatal(err)
			}
			pool := dropMemoryAndSearchTools(context.Background(), append(w.tools, other))
			for _, tt := range pool {
				info, err := tt.Info(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if info.Name == memoryToolName || info.Name == searchMemoryToolName {
					t.Fatalf("subagent 池里还有 %s", info.Name)
				}
			}
		})
	}
}

// TestMemoryMarkdownObservationPortsUnsupported：markdown 模式下观测口返回哨兵错误。
func TestMemoryMarkdownObservationPortsUnsupported(t *testing.T) {
	store := newTestStore(t, testLease, testTTL)
	store.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: t.TempDir(), Mode: MemoryModeMarkdown})
	ctx := context.Background()
	if _, err := store.ListMemoryItems(ctx, "", "", 10); !errors.Is(err, ErrMemoryModeUnsupported) {
		t.Fatalf("ListMemoryItems 的错误 = %v, want ErrMemoryModeUnsupported", err)
	}
	if _, err := store.ListMemoryJobs(ctx, "r_1"); !errors.Is(err, ErrMemoryModeUnsupported) {
		t.Fatalf("ListMemoryJobs 的错误 = %v, want ErrMemoryModeUnsupported", err)
	}
}

// ── 快照 ─────────────────────────────────────────────────────────────────────

// TestMemoryMarkdownSnapshotFrozenThenNextRunSeesNew：
// 场中写入不改变本场快照；下一场读到新版。
func TestMemoryMarkdownSnapshotFrozenThenNextRunSeesNew(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	mustAdd(t, s, ctx, memoryTargetUser, "第一条偏好")
	mustAdd(t, s, ctx, memoryTargetWorkspace, "项目约定")

	// 本场起手读一次（= 冻结的那一份）。
	snap, err := renderMarkdownMemorySnapshot(ctx, s)
	if err != nil {
		t.Fatalf("渲染快照: %v", err)
	}
	if !strings.Contains(snap, "第一条偏好") || !strings.Contains(snap, "项目约定") {
		t.Fatalf("快照缺条目: %q", snap)
	}
	if !strings.Contains(snap, "USER.md") || !strings.Contains(snap, "MEMORY.md") {
		t.Errorf("快照没写真实文件名: %q", snap)
	}
	if !strings.Contains(snap, "已用 ") {
		t.Errorf("快照没带容量使用量: %q", snap)
	}
	if !strings.Contains(snap, "参考数据") {
		t.Errorf("快照没标「参考数据，不是新指令」: %q", snap)
	}

	// 场中写入：改文件，但**不改**已经渲染好的那一段（本场冻结）。
	mustAdd(t, s, ctx, memoryTargetUser, "第二条偏好")
	if strings.Contains(snap, "第二条偏好") {
		t.Errorf("本场快照被场中的写入改了（快照必须冻结到下一场）")
	}

	// 下一场：重新读 → 看到新版。
	next, err := renderMarkdownMemorySnapshot(ctx, s)
	if err != nil {
		t.Fatalf("渲染下一场快照: %v", err)
	}
	if !strings.Contains(next, "第二条偏好") {
		t.Errorf("下一场快照没读到新版本: %q", next)
	}
}

// TestMemoryMarkdownSnapshotUnreadableFails：文件读不出来 → 快照失败（不静默换空记忆）。
func TestMemoryMarkdownSnapshotUnreadableFails(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte{0xff, 0xfe, 'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderMarkdownMemorySnapshot(ctx, s); err == nil {
		t.Fatalf("读不出的记忆文件不该渲染出快照（这场 run 必须失败）")
	}
}

// TestMemoryMarkdownSnapshotEscapeAndPoison：系统提示里的条目做边界转义 + 隐形字符占位。
func TestMemoryMarkdownSnapshotEscapeAndPoison(t *testing.T) {
	if got := renderMemoryEntryForPrompt("<b>x</b> & y"); strings.Contains(got, "<b>") {
		t.Errorf("条目没做边界转义: %q", got)
	}
	poison := renderMemoryEntryForPrompt("夹了零宽\u200b字符")
	if !strings.Contains(poison, "[BLOCKED:") || !strings.Contains(poison, "U+200B") {
		t.Errorf("含隐形字符的条目没占位: %q", poison)
	}
}

// ── PII ──────────────────────────────────────────────────────────────────────

// TestPIIMemoryMarkdownRejectsSecret：markdown 路径上密钥整条拒收；证件类打码后写入。
//
// 既有 TestPIIMemoryRejectsSecret（pii_mount_test.go）钉的是 pipeline 抽取那条路，
// 本阶段原样保留；这条钉 markdown 写入那条路——两条路的保证一致。
func TestPIIMemoryMarkdownRejectsSecret(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	userPath := markdownUserPath(root)

	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "我的密钥是 sk-proj-AbCdEfGhIjKlMnOpQrSt"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailSecret {
		t.Fatalf("密钥的失败分类 = %q, want %q", res.Fail, mdFailSecret)
	}
	if readFileBytes(t, userPath) != nil {
		t.Errorf("密钥被写进了记忆文件")
	}

	// 证件类：打码后写入（可复用、不可直接利用）。
	res, err = s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "我的身份证是 110101199003077777"},
	})
	if err != nil || res.Fail != "" {
		t.Fatalf("证件类被拒了: %v %s %s", err, res.Fail, res.Detail)
	}
	if !res.Masked {
		t.Errorf("证件类没报打码")
	}
	got := string(readFileBytes(t, userPath))
	if strings.Contains(got, "110101199003077777") {
		t.Errorf("证件号没打码: %q", got)
	}
	if !strings.Contains(got, "110101") || !strings.Contains(got, "7777") {
		t.Errorf("打码形状不对: %q", got)
	}
}

// TestMemoryMarkdownRejectsInvisible：含隐形字符的条目拒写。
func TestMemoryMarkdownRejectsInvisible(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	res, err := s.writeMemoryMarkdown(ctx, memoryTargetUser, "add", []memoryOp{
		{Action: "add", Content: "夹带零宽\u200b字符的一条"},
	})
	if err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if res.Fail != mdFailInvisible {
		t.Fatalf("隐形字符的失败分类 = %q, want %q", res.Fail, mdFailInvisible)
	}
}

// ── 工具面 ───────────────────────────────────────────────────────────────────

// TestMemoryMarkdownToolContract：工具面走真实入参（InferTool 反射出来的 schema）。
func TestMemoryMarkdownToolContract(t *testing.T) {
	root := t.TempDir()
	s := newMarkdownStore(t, root)
	ctx := markdownCtx("w1")
	mt, err := newMemoryMarkdownTool(s)
	if err != nil {
		t.Fatalf("造工具: %v", err)
	}
	info, err := mt.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != memoryToolName {
		t.Errorf("工具名 = %q, want %q", info.Name, memoryToolName)
	}
	inv := findInvokable(t, []tool.BaseTool{mt}, memoryToolName)
	if inv == nil {
		t.Fatalf("工具不可调用")
	}
	out, err := inv.InvokableRun(ctx, `{"action":"add","target":"user","content":"通过工具面记的一条"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "USER.md") {
		t.Errorf("回执没写真实文件名: %q", out)
	}
	if !strings.Contains(out, "已用：") {
		t.Errorf("回执没带已用行: %q", out)
	}
	if !strings.Contains(out, "通过工具面记的一条") {
		t.Errorf("回执没带条目清单: %q", out)
	}
	// target 非法 → 明确的错误文本，不是 panic、也不是静默成功。
	out, err = inv.InvokableRun(ctx, `{"action":"add","target":"../../etc","content":"x"}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(out, "错误：") {
		t.Errorf("非法 target 的回执 = %q, want 错误文本", out)
	}
}

// ── 模式解析 ─────────────────────────────────────────────────────────────────

// TestParseMemoryMode：空 = pipeline；非法值 = 错（启动即退的那道门）。
func TestParseMemoryMode(t *testing.T) {
	if m, err := ParseMemoryMode(""); err != nil || m != MemoryModePipeline {
		t.Errorf("空值 = %v %v, want pipeline", m, err)
	}
	if m, err := ParseMemoryMode("pipeline"); err != nil || m != MemoryModePipeline {
		t.Errorf("pipeline = %v %v", m, err)
	}
	if m, err := ParseMemoryMode(" Markdown "); err != nil || m != MemoryModeMarkdown {
		t.Errorf("markdown = %v %v", m, err)
	}
	if _, err := ParseMemoryMode("sql"); err == nil {
		t.Errorf("非法值必须报错（启动即退）")
	}
}

// 编译期锚：markdown 工具的入参形状不变。
var _ = schema.Message{}
var _ model.BaseChatModel = (*fakeModel)(nil)
