// 本文件：21-历史查找 H5 回归网（search_history 面）。目标是钉住后续改动最容易悄悄变形的
// 五个地方：词校验句与顺序（L8）、ILIKE 模式串的转义、片段/窗口行的有界渲染、4000 帽的
// 丢块规则（词模式末位丢起 / 窗口模式最远丢起 L5）与截断计数口径（H5 裁定：内联首行也写
// 全量命中数）、截断落盘面（内容 = 未截断渲染、路径行逐字、run 内顺编、写失败降级）。
// DB 路径的活体面已被 H1/H2/H4 验收覆盖，这里补两条 store 级小网（范围过滤与窗口负路径）。
package runengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

// seedSearchRow 往 runlog_entries 插一行（搜索测试夹具）。
func seedSearchRow(t *testing.T, store *Store, session SessionID, seq int64, run RunID, role, text string) {
	t.Helper()
	id, err := newRowID("rl")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.WithContext(context.Background()).Create(&RunlogEntry{
		ID: id, SessionID: session, Seq: seq, RunID: run, Role: role, Text: text, CreatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

// TestSearchValidationSentences 钉 L8 的校验三句 + roles 非法句的逐字文本与检查顺序
// （空 → 超 8 词 → 有词超 200 字 → roles → 查库）。store 传 nil 是故意的：这些分支若
// 碰了库会当场 nil 解引用炸出 panic——「校验不过不查库」的反证就藏在构造里。
func TestSearchValidationSentences(t *testing.T) {
	sh, err := newSearchHistoryTool(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := sh.(tool.InvokableTool)
	if !ok {
		t.Fatalf("search_history 应实现 InvokableTool，实得 %T", sh)
	}
	ctx := withNoteBox(context.Background(), newNoteBox("s_val", "r_val", 0))
	longWord := strings.Repeat("长", 201)
	cases := []struct {
		name string
		args string
		want string
	}{
		{"空句", `{"query":"   "}`, "query 为空。传入原文里会原样出现的词。"},
		{"空先于roles", `{"query":"   ","roles":"bogus"}`, "query 为空。传入原文里会原样出现的词。"},
		{"超8词先于roles", `{"query":"a a a a a a a a a","roles":"bogus"}`, "query 拆出 9 个词，最多 8 个。"},
		{"超200字先于roles", fmt.Sprintf(`{"query":%q,"roles":"bogus"}`, longWord), "有一个词超过 200 字。"},
		{"roles非法句", `{"query":"E_DECK_17","roles":"bogus"}`, "错误：roles 只认 user、assistant、tool_call、tool_result，用逗号分隔。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := inv.InvokableRun(ctx, c.args)
			if err != nil {
				t.Fatalf("校验句应走普通结果通道（err=nil），实得错误: %v", err)
			}
			if out != c.want {
				t.Fatalf("逐字文本不对\n实得: %q\n期望: %q", out, c.want)
			}
		})
	}
}

// TestParseSearchRoles 钉 roles 解析：空 = 默认三档；逗号拆 + 去空白；任一值出域 = 非法（不查库）。
func TestParseSearchRoles(t *testing.T) {
	if roles, ok := parseSearchRoles(""); !ok || len(roles) != 3 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "tool_result" {
		t.Fatalf(`空 roles 应得默认三档 [user assistant tool_result]，实得 %v %v`, roles, ok)
	}
	if roles, ok := parseSearchRoles(" user , tool_call "); !ok || len(roles) != 2 || roles[0] != "user" || roles[1] != "tool_call" {
		t.Fatalf("带空白的两档应拆对，实得 %v %v", roles, ok)
	}
	if roles, ok := parseSearchRoles("user,assistant,tool_call,tool_result"); !ok || len(roles) != 4 {
		t.Fatalf("四个 role 全给应合法，实得 %v %v", roles, ok)
	}
	for _, bad := range []string{"bogus", "user,,assistant", "user, tool_result, 其它"} {
		if _, ok := parseSearchRoles(bad); ok {
			t.Fatalf("非法 roles %q 应判 false（“不查库”）", bad)
		}
	}
}

// TestILikePattern 钉转义逐字：`\`、`%`、`_` 三词在模式串里的字节形态——不转义的 `%` 会
// 变成通配符、一条查询扫出整段历史（H3 变异抽查的靶之一）。
func TestILikePattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"E_DECK_17", `%E\_DECK\_17%`},
		{"50%", `%50\%%`},
		{`a\b`, `%a\\b%`},
		{`x_y%z\w`, `%x\_y\%z\\w%`},
		{"普通词", "%普通词%"},
	}
	for _, c := range cases {
		if got := ilikePattern(c.in); got != c.want {
			t.Fatalf("ilikePattern(%q)\n实得: %q\n期望: %q", c.in, got, c.want)
		}
	}
}

// TestRuneIndexFold 钉大小写不敏感定位（L2 的底座）：rune 下标、空词 = 0、没找到 = -1。
func TestRuneIndexFold(t *testing.T) {
	hay := []rune("你好ABC世界")
	if got := runeIndexFold(hay, "abc"); got != 2 {
		t.Fatalf(`runeIndexFold("你好ABC世界", "abc") = %d, want 2`, got)
	}
	if got := runeIndexFold(hay, ""); got != 0 {
		t.Fatalf("空词应得 0，实得 %d", got)
	}
	if got := runeIndexFold(hay, "zzz"); got != -1 {
		t.Fatalf("没找到应得 -1，实得 %d", got)
	}
}

// TestSearchSnippet 钉片段切取：以第一个词首次出现处为中心前后各 200 字、行短整行、
// 切断处独占一行的前略/后略标记、定位大小写不敏感（L2）、定位失配按行首窗兜底。
func TestSearchSnippet(t *testing.T) {
	word := "E_DECK_17"

	t.Run("行短整行", func(t *testing.T) {
		text := "短行里有个 E_DECK_17 标记。"
		if got := searchSnippet(text, word); got != text {
			t.Fatalf("行比窗口短应整行返回（无省略标记），实得 %q", got)
		}
	})

	t.Run("恰长不切", func(t *testing.T) {
		text := strings.Repeat("甲", 200) + word + strings.Repeat("乙", 191) // 总 400 字，词在 200 处
		if got := searchSnippet(text, word); got != text {
			t.Fatalf("恰 400 字应整行返回（无省略标记），实得 %q", got)
		}
	})

	t.Run("前后各200", func(t *testing.T) {
		text := strings.Repeat("甲", 300) + word + strings.Repeat("乙", 300)
		want := "……（前略）\n" + string([]rune(text)[100:500]) + "\n……（后略）"
		if got := searchSnippet(text, word); got != want {
			t.Fatalf("居中切片不对\n实得: %q\n期望: %q", got, want)
		}
	})

	t.Run("定位大小写不敏感", func(t *testing.T) {
		text := strings.Repeat("甲", 300) + "E_DECK_17" + strings.Repeat("乙", 300)
		upper := searchSnippet(text, "E_DECK_17")
		lower := searchSnippet(text, "e_deck_17")
		if lower != upper {
			t.Fatalf("小写词应定位到同一中心\n小写: %q\n大写: %q", lower, upper)
		}
		if upper == text {
			t.Fatalf("长行应切出省略标记，实得整行")
		}
	})

	t.Run("定位失配退行首", func(t *testing.T) {
		// pos=0 兜底：中心窗 [0-200, 0+200] 收成行首起 200 字（无前略、有后略）。
		text := strings.Repeat("甲", 600)
		want := string([]rune(text)[:200]) + "\n……（后略）"
		if got := searchSnippet(text, "没有的词"); got != want {
			t.Fatalf("定不到中心应按行首窗兜底（无前略，有后略）\n实得: %q\n期望: %q", got, want)
		}
	})
}

// TestWindowLine 钉窗口模式每行从行首起最多 800 字（恰 800 不切、801 切出后略独占行）。
func TestWindowLine(t *testing.T) {
	if got := windowLine(strings.Repeat("字", 800)); len([]rune(got)) != 800 || strings.Contains(got, "后略") {
		t.Fatalf("恰 800 字应原样，实得 %d 字", len([]rune(got)))
	}
	long := strings.Repeat("字", 801)
	want := strings.Repeat("字", 800) + "\n……（后略）"
	if got := windowLine(long); got != want {
		t.Fatalf("801 字应切为前 800 字 + 后略标记行\n实得: %q", got)
	}
	if got := windowLine("短行"); got != "短行" {
		t.Fatalf("短行应原样，实得 %q", got)
	}
}

// TestRenderSearchHits 钉词模式渲染：空/无截断/截断三态。截断态的计数口径 = H5 裁定——
// 内联首行也写全量命中数（与落盘文件首行一致），丢块不改写；丢块从末位（最旧）整块丢起。
func TestRenderSearchHits(t *testing.T) {
	t.Run("空", func(t *testing.T) {
		inline, full, truncated := renderSearchHits(nil, "x")
		if inline != "没有命中。" || full != "" || truncated {
			t.Fatalf("0 条应得「没有命中。」三值，实得 %q %q %v", inline, full, truncated)
		}
	})

	t.Run("无截断", func(t *testing.T) {
		hits := []RunlogEntry{
			{Seq: 2, Role: "user", Text: "上一行有 E_DECK_17"},
			{Seq: 1, Role: "assistant", Text: "E_DECK_17 也在"},
		}
		inline, full, truncated := renderSearchHits(hits, "E_DECK_17")
		if truncated || full != "" {
			t.Fatalf("小结果不应触发截断，实得 truncated=%v full=%q", truncated, full)
		}
		want := "命中 2 条。范围：本会话里本场 run 之前已提交的行。\n---\nseq=2 role=user agent=\n上一行有 E_DECK_17\n---\nseq=1 role=assistant agent=\nE_DECK_17 也在"
		if inline != want {
			t.Fatalf("无截断渲染逐字节不对\n实得: %q\n期望: %q", inline, want)
		}
	})

	t.Run("截断", func(t *testing.T) {
		// 10 块 × ≈438 字（片段 414 + 头行 24/23），拼接 4416 > 4000 → 恰丢 1 块（seq=1，最旧）。
		hits := make([]RunlogEntry, 10)
		for i := range hits {
			hits[i] = RunlogEntry{
				Seq:  int64(10 - i), // seq 10..1，DESC 序
				Role: "user",
				Text: strings.Repeat("甲", 300) + "E_DECK_17" + strings.Repeat("乙", 300),
			}
		}
		inline, full, truncated := renderSearchHits(hits, "E_DECK_17")
		if !truncated {
			t.Fatalf("10 块应超帽触发截断")
		}
		if !strings.HasPrefix(inline, "命中 10 条。范围：本会话里本场 run 之前已提交的行。") {
			t.Fatalf("截断后内联首行应写全量数「命中 10 条」（H5 裁定）\n实得首行: %q", strings.SplitN(inline, "\n", 2)[0])
		}
		if !strings.HasSuffix(inline, "\n其余命中已截断。") {
			t.Fatalf("截断后内联应以「其余命中已截断。」收尾，实得尾: %q", inline[len(inline)-30:])
		}
		if strings.Contains(inline, "seq=1 role=") {
			t.Fatalf("最旧一块（seq=1）应被丢掉，内联里不该有")
		}
		if got := strings.Count(inline, "role=user"); got != 9 {
			t.Fatalf("内联应恰留 9 块，实得 %d 块", got)
		}
		if !strings.HasPrefix(full, "命中 10 条。") {
			t.Fatalf("落盘全文首行应写全量数，实得首行: %q", strings.SplitN(full, "\n", 2)[0])
		}
		if strings.Contains(full, "其余命中已截断") {
			t.Fatalf("落盘全文不该含截断标记行")
		}
		if got := strings.Count(full, "role=user"); got != 10 {
			t.Fatalf("落盘全文应含全部 10 块，实得 %d 块", got)
		}
	})
}

// TestRenderSearchWindow 钉窗口模式渲染：空兜底、升序、每行 800 字帽、截断从离 around_seq
// 最远的行丢起（L5：距离相同先丢 seq 较大者）、截断句与全文。
func TestRenderSearchWindow(t *testing.T) {
	t.Run("空兜底", func(t *testing.T) {
		inline, full, truncated := renderSearchWindow(nil, 7, 2)
		if inline != "本会话没有 seq=7。" || full != "" || truncated {
			t.Fatalf("空窗口应得负路径句三值，实得 %q %q %v", inline, full, truncated)
		}
	})

	t.Run("升序无截断", func(t *testing.T) {
		rows := []RunlogEntry{
			{Seq: 10, Role: "user", Text: "行十"},
			{Seq: 11, Role: "assistant", Text: "行十一"},
			{Seq: 12, Role: "tool_result", Text: "行十二"},
		}
		inline, full, truncated := renderSearchWindow(rows, 11, 2)
		if truncated || full != "" {
			t.Fatalf("小窗口不应触发截断，实得 truncated=%v full=%q", truncated, full)
		}
		want := "seq=11 的窗口，前后各 2 行。\n---\nseq=10 role=user agent=\n行十\n---\nseq=11 role=assistant agent=\n行十一\n---\nseq=12 role=tool_result agent=\n行十二"
		if inline != want {
			t.Fatalf("窗口渲染逐字节不对\n实得: %q\n期望: %q", inline, want)
		}
	})

	t.Run("行帽800", func(t *testing.T) {
		rows := []RunlogEntry{{Seq: 5, Role: "user", Text: strings.Repeat("字", 900)}}
		inline, _, _ := renderSearchWindow(rows, 5, 1)
		want := "seq=5 的窗口，前后各 1 行。\n---\nseq=5 role=user agent=\n" + strings.Repeat("字", 800) + "\n……（后略）"
		if inline != want {
			t.Fatalf("900 字行应切为 800 字 + 后略标记行\n实得: %q\n期望: %q", inline, want)
		}
	})

	t.Run("截断丢最远", func(t *testing.T) {
		// 10 行 seq 6..15、每行 700 字：块 723/724，拼接 7281 > 4000。丢序 = 15,14,6,13,7
		// （距离 5,4,4,3,3——平局丢 seq 较大者），留 {8,9,10,11,12}，剩 3638 ≤ 4000。
		rows := make([]RunlogEntry, 10)
		for i := range rows {
			rows[i] = RunlogEntry{Seq: int64(6 + i), Role: "user", Text: strings.Repeat("甲", 700)}
		}
		inline, full, truncated := renderSearchWindow(rows, 10, 5)
		if !truncated {
			t.Fatalf("10 行 × 700 字应超帽触发截断")
		}
		if !strings.HasPrefix(inline, "seq=10 的窗口，前后各 5 行。") {
			t.Fatalf("窗口首行不对，实得: %q", strings.SplitN(inline, "\n", 2)[0])
		}
		if !strings.HasSuffix(inline, "\n其余命中已截断。") {
			t.Fatalf("截断后应以「其余命中已截断。」收尾")
		}
		for _, dropped := range []int64{6, 7, 13, 14, 15} {
			if strings.Contains(inline, fmt.Sprintf("seq=%d role=", dropped)) {
				t.Fatalf("离 around_seq 最远的行 seq=%d 应被先丢，内联里不该有", dropped)
			}
		}
		for _, kept := range []int64{8, 9, 10, 11, 12} {
			if !strings.Contains(inline, fmt.Sprintf("seq=%d role=", kept)) {
				t.Fatalf("近 around_seq 的行 seq=%d 应保留", kept)
			}
		}
		if strings.Contains(full, "其余命中已截断") {
			t.Fatalf("落盘全文不该含截断标记行")
		}
		for seq := int64(6); seq <= 15; seq++ {
			if !strings.Contains(full, fmt.Sprintf("seq=%d role=", seq)) {
				t.Fatalf("落盘全文应含全部行，缺 seq=%d", seq)
			}
		}
	})
}

// TestSpillSurface 钉截断落盘面：文件名 run 内顺编、文件内容 = 未截断渲染、内联路径行逐字、
// 撞名视为写失败、写失败（目录建不出）静默降级、无截断不落盘。
func TestSpillSurface(t *testing.T) {
	t.Run("顺编与内容", func(t *testing.T) {
		ws := t.TempDir()
		box := newNoteBox("s_sp", "r_sp", 0)
		full := "命中 2 条。范围：本会话里本场 run 之前已提交的行。\n---\n假块"
		out1 := spillIfTruncated("INLINE", full, true, ws, box)
		if out1 != "INLINE\n完整结果已写入 .tako/search/r_sp_1.md。" {
			t.Fatalf("路径行应逐字拼在内联末尾，实得 %q", out1)
		}
		got, err := os.ReadFile(filepath.Join(ws, ".tako", "search", "r_sp_1.md"))
		if err != nil {
			t.Fatalf("落盘文件应存在: %v", err)
		}
		if string(got) != full {
			t.Fatalf("文件内容应 = 未截断渲染\n实得: %q\n期望: %q", string(got), full)
		}
		out2 := spillIfTruncated("INLINE", full, true, ws, box)
		if out2 != "INLINE\n完整结果已写入 .tako/search/r_sp_2.md。" {
			t.Fatalf("同 run 第二次应顺编 _2，实得 %q", out2)
		}
		if box.spills != 2 {
			t.Fatalf("run 内计数器应为 2，实得 %d", box.spills)
		}
	})

	t.Run("撞名视为写失败", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, ".tako", "search")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "r_clash_1.md"), []byte("旧文件"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := spillIfTruncated("INLINE", "FULL", true, ws, newNoteBox("s_sp", "r_clash", 0))
		if out != "INLINE" {
			t.Fatalf("撞名应视为写失败、静默降级为纯截断，实得 %q", out)
		}
	})

	t.Run("写失败降级", func(t *testing.T) {
		ws := t.TempDir()
		// .tako 位置被普通文件占着 → MkdirAll(.tako/search) 必失败。
		if err := os.WriteFile(filepath.Join(ws, ".tako"), []byte("占位"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := spillIfTruncated("INLINE", "FULL", true, ws, newNoteBox("s_sp", "r_fail", 0))
		if out != "INLINE" {
			t.Fatalf("写失败应不加路径行（其余字节不变），实得 %q", out)
		}
	})

	t.Run("无截断不落盘", func(t *testing.T) {
		ws := t.TempDir()
		out := spillIfTruncated("INLINE", "", false, ws, newNoteBox("s_sp", "r_none", 0))
		if out != "INLINE" {
			t.Fatalf("无截断应原样返回，实得 %q", out)
		}
		if _, err := os.Stat(filepath.Join(ws, ".tako")); !os.IsNotExist(err) {
			t.Fatalf("无截断不该碰文件系统（.tako 不应存在），stat err=%v", err)
		}
	})
}

// TestSearchRunlogScope 钉读路径范围：session 等值、排除本场 run、roles 过滤、
// ILIKE 大小写不敏感与转义生效（% 不作通配符）、seq DESC。
func TestSearchRunlogScope(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t, time.Minute, time.Minute)
	seedSearchRow(t, store, "s_q", 1, "r_prev", "user", "E_DECK_17 第一行，50% 的进度。")
	seedSearchRow(t, store, "s_q", 2, "r_prev", "assistant", "E_DECK_17 第二行 5071 进度。")
	seedSearchRow(t, store, "s_q", 3, "r_prev", "tool_result", "E_DECK_17 tool 行")
	seedSearchRow(t, store, "s_q", 4, "r_prev", "tool_call", `search_history({"query":"E_DECK_17"})`)
	seedSearchRow(t, store, "s_q", 5, "r_cur", "user", "本场 user 行 E_DECK_17 不该入选")
	seedSearchRow(t, store, "s_other", 1, "r_prev", "user", "别的会话也有 E_DECK_17")

	rows, err := store.searchRunlog(ctx, "s_q", "r_cur", searchDefaultRoles, []string{ilikePattern("E_DECK_17")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("应命中 3 行（user/assistant/tool_result，排 tool_call、排本场、排他会话），实得 %d", len(rows))
	}
	for i, want := range []int64{3, 2, 1} {
		if rows[i].Seq != want {
			t.Fatalf("应 seq DESC（3,2,1），第 %d 位实得 %d", i, rows[i].Seq)
		}
	}

	rows, err = store.searchRunlog(ctx, "s_q", "r_cur", searchDefaultRoles, []string{ilikePattern("e_deck_17")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("小写词应同样命中 3 行（ILIKE 面），实得 %d", len(rows))
	}

	rows, err = store.searchRunlog(ctx, "s_q", "r_cur", searchDefaultRoles, []string{ilikePattern("50%")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Seq != 1 {
		t.Fatalf(`「50%%」应只命中字面含 50%% 的 seq=1（转义生效，百分号不作通配符），实得 %d 行`, len(rows))
	}
}

// TestSearchAroundSeqStore 钉窗口模式两条 DB 路径：本会话有该 seq（窗口升序渲染）与
// 没有该 seq（负路径句逐字）。
func TestSearchAroundSeqStore(t *testing.T) {
	store := newTestStore(t, time.Minute, time.Minute)
	seedSearchRow(t, store, "s_w", 1, "r_prev", "user", "内容一")
	seedSearchRow(t, store, "s_w", 2, "r_prev", "assistant", "内容二")
	seedSearchRow(t, store, "s_w", 3, "r_prev", "tool_result", "内容三")

	sh, err := newSearchHistoryTool(store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := sh.(tool.InvokableTool)
	if !ok {
		t.Fatalf("search_history 应实现 InvokableTool，实得 %T", sh)
	}
	ctx := withNoteBox(context.Background(), newNoteBox("s_w", "r_cur", 0))

	out, err := inv.InvokableRun(ctx, `{"around_seq":2}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "seq=2 的窗口，前后各 2 行。\n---\nseq=1 role=user agent=\n内容一\n---\nseq=2 role=assistant agent=\n内容二\n---\nseq=3 role=tool_result agent=\n内容三"
	if out != want {
		t.Fatalf("窗口输出不对\n实得: %q\n期望: %q", out, want)
	}

	out, err = inv.InvokableRun(ctx, `{"around_seq":99}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "本会话没有 seq=99。" {
		t.Fatalf("负路径句应逐字「本会话没有 seq=99。」，实得 %q", out)
	}
}
