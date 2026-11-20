package runengine

// P4-4 单测（doc/84 §7.1/§7.3；施工包 §5.6/§6.2）：RRF 合并、融合、引用块渲染与
// dev query 的 limit 解析——全部纯函数，不打模型、不依赖 PG/Redis。hybrid 召回链
// 与 dev 端点的行为测试在 dialogue_recall_hybrid_test.go。

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// RRF 合并：名次从 1 起（1/(60+rank)）、两臂累加、rrfScore 降序、runID 升序打破平分、
// sources 按词表顺序只含出现过的臂。
func TestDialogueRRFMerge(t *testing.T) {
	// 单臂单条：rank 1 → 1/61（不是 1/60——名次从 1 起不是从 0 起）。
	out := dialogueRRFMerge([]string{"a"}, nil)
	if len(out) != 1 || out[0].RunID != "a" {
		t.Fatalf("merge = %+v, want 单条 a", out)
	}
	if math.Abs(out[0].RRF-1.0/61.0) > 1e-12 {
		t.Fatalf("RRF = %v, want 1/61（rank 从 1 起）", out[0].RRF)
	}
	if strings.Join(out[0].Sources, ",") != "lexical" {
		t.Fatalf("sources = %v, want [lexical]", out[0].Sources)
	}

	// 两臂累加：同名 run 两臂都是第 1 名 → 2/61，sources 两个臂都在、词法在前。
	out = dialogueRRFMerge([]string{"a"}, []string{"a"})
	if len(out) != 1 {
		t.Fatalf("merge = %+v, want 单条 a", out)
	}
	if math.Abs(out[0].RRF-2.0/61.0) > 1e-12 {
		t.Fatalf("RRF = %v, want 2/61（两臂累加）", out[0].RRF)
	}
	if strings.Join(out[0].Sources, ",") != "lexical,vector" {
		t.Fatalf("sources = %v, want [lexical vector]", out[0].Sources)
	}

	// 名次不同：x 第一名 1/61，y 第二名 1/62，z 第三名 1/63 → 降序 x,y,z。
	out = dialogueRRFMerge([]string{"x", "y", "z"}, nil)
	if len(out) != 3 || out[0].RunID != "x" || out[1].RunID != "y" || out[2].RunID != "z" {
		t.Fatalf("merge = %+v, want x,y,z 按分数降序", out)
	}
	if math.Abs(out[2].RRF-1.0/63.0) > 1e-12 {
		t.Fatalf("第三名 RRF = %v, want 1/63", out[2].RRF)
	}

	// 平分打破：两臂名次交叉（a=1/61+1/62，b=1/62+1/61），分数相等 → runID 升序 a 在前。
	// 同一份输入永远得到同一份输出（可测、可对质）。
	out = dialogueRRFMerge([]string{"b", "a"}, []string{"a", "b"})
	if len(out) != 2 {
		t.Fatalf("merge = %+v, want 两条", out)
	}
	if math.Abs(out[0].RRF-out[1].RRF) > 1e-12 {
		t.Fatalf("两臂交叉应平分：%v vs %v", out[0].RRF, out[1].RRF)
	}
	if out[0].RunID != "a" || out[1].RunID != "b" {
		t.Fatalf("平分应 runID 升序：%+v", out)
	}

	// 只在向量臂出现：sources 只有 vector；不跨臂比较分数。
	out = dialogueRRFMerge(nil, []string{"v1"})
	if len(out) != 1 || out[0].RunID != "v1" || strings.Join(out[0].Sources, ",") != "vector" {
		t.Fatalf("merge = %+v, want v1 只带 vector", out)
	}
}

// 融合：词法臂的坐标与原文优先（两臂内容一致），向量臂只补名次与分数；
// 只在向量臂出现时用向量臂的。
func TestDialogueHybridFuse(t *testing.T) {
	tA := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	lex := []dialogueLexHit{
		{RunID: "r1", SessionID: "s1", Passage: "p1", Score: 3.2, FinishedAt: &tA},
		{RunID: "r2", SessionID: "s2", Passage: "p2", Score: 2.1},
	}
	vec := []dialogueVecHit{
		{RunID: "r2", Score: 0.91},
		{RunID: "r1", Score: 0.88},
		{RunID: "r3", SessionID: "s3", Passage: "p3", Score: 0.7, FinishedAt: &tB},
	}
	fused := dialogueHybridFuse(lex, vec)
	if len(fused) != 3 {
		t.Fatalf("fuse = %d 条, want 3", len(fused))
	}
	// r1 与 r2 的 RRF 相等（1/61+1/62 vs 1/62+1/61）→ runID 升序 r1 在前；r3 只有 1/63。
	if fused[0].RunID != "r1" || fused[1].RunID != "r2" || fused[2].RunID != "r3" {
		t.Fatalf("fuse 顺序 = %v,%v,%v, want r1,r2,r3", fused[0].RunID, fused[1].RunID, fused[2].RunID)
	}
	h := fused[0]
	if h.LexRank != 1 || h.LexScore != 3.2 || h.VecRank != 2 || h.VecScore != 0.88 {
		t.Fatalf("r1 名次/分数 = %+v", h)
	}
	if math.Abs(h.RRFScore-(1.0/61.0+1.0/62.0)) > 1e-12 {
		t.Fatalf("r1 RRF = %v", h.RRFScore)
	}
	if strings.Join(h.Sources, ",") != "lexical,vector" {
		t.Fatalf("r1 sources = %v", h.Sources)
	}
	if h.Passage != "p1" || h.SessionID != "s1" || h.FinishedAt != &tA {
		t.Fatalf("r1 应取词法臂细节: %+v", h)
	}
	// r2：词法 rank 2、向量 rank 1——细节仍以词法臂为准。
	h = fused[1]
	if h.LexRank != 2 || h.LexScore != 2.1 || h.VecRank != 1 || h.VecScore != 0.91 || h.Passage != "p2" {
		t.Fatalf("r2 = %+v", h)
	}
	// r3：只在向量臂 → LexRank 0、细节取向量臂。
	h = fused[2]
	if h.LexRank != 0 || h.LexScore != 0 || h.VecRank != 3 || h.VecScore != 0.7 ||
		h.Passage != "p3" || h.SessionID != "s3" || h.FinishedAt != &tB {
		t.Fatalf("r3 = %+v", h)
	}
	if strings.Join(h.Sources, ",") != "vector" {
		t.Fatalf("r3 sources = %v", h.Sources)
	}
}

// 引用块：程序按模板拼接（doc/84 §7.3 逐字），坐标头剥离、HTML 转义、300 rune 截取、
// 上限 3 条、只剩坐标头的条目跳过、零候选整块不生成。
func TestRenderDialogueHybridBlock(t *testing.T) {
	// 零候选：块与注入数都为 0。
	if b, n := renderDialogueHybridBlock(nil); b != "" || n != 0 {
		t.Fatalf("零候选渲染 = %q,%d", b, n)
	}

	fin := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	hits := []dialogueHybridHit{{
		RunID: "r1", SessionID: "s1",
		Passage:    "[会话 s1 · 场 r1]\n用户：数据 <b>5433</b> 端口",
		FinishedAt: &fin,
	}}
	block, n := renderDialogueHybridBlock(hits)
	want := "<dialogue-context>\n" + dialogueContextNoteHybrid + "\n" +
		"- (2026-11-20, 会话 s1, run r1) 用户：数据 &lt;b&gt;5433&lt;/b&gt; 端口\n" +
		"</dialogue-context>"
	if n != 1 || block != want {
		t.Fatalf("渲染 = %d 条\n%s\nwant\n%s", n, block, want)
	}

	// 坐标头剥离：引用前缀已带 session/run，passage 首行的「[会话 x · 场 y]」不重复贴。
	// 没有坐标头的 passage 原样进引用行。
	hits = []dialogueHybridHit{{RunID: "r2", SessionID: "s2", Passage: "无坐标头原文", FinishedAt: &fin}}
	block, n = renderDialogueHybridBlock(hits)
	if n != 1 || !strings.Contains(block, "- (2026-11-20, 会话 s2, run r2) 无坐标头原文\n") {
		t.Fatalf("无坐标头渲染:\n%s", block)
	}

	// 长度截取：300 rune 上限（与 pg_search 渲染同款，保留头部），转义在截取之后。
	long := "汉" + strings.Repeat("字", dialogueRecallRuneCap+50)
	hits = []dialogueHybridHit{{RunID: "r3", SessionID: "s3", Passage: long, FinishedAt: &fin}}
	block, _ = renderDialogueHybridBlock(hits)
	line := strings.TrimPrefix(strings.TrimPrefix(block, "<dialogue-context>\n"), dialogueContextNoteHybrid+"\n")
	line = strings.TrimSuffix(line, "</dialogue-context>")
	// 行 = "- (date, 会话 s3, run r3) <片段>\n"，数片段里的汉字。
	body := strings.TrimPrefix(line, "- (2026-11-20, 会话 s3, run r3) ")
	body = strings.TrimSuffix(body, "\n")
	if got := utf8.RuneCountInString(body); got != dialogueRecallRuneCap {
		t.Fatalf("截取后 %d rune, want %d", got, dialogueRecallRuneCap)
	}
	if got := strings.Count(body, "字"); got != dialogueRecallRuneCap-1 {
		t.Fatalf("截取后 %d 个「字」, want %d", got, dialogueRecallRuneCap-1)
	}
	if !strings.HasPrefix(body, "汉") {
		t.Fatalf("截取保留头部，首字符应是「汉」:\n%s", body)
	}

	// 上限 3 条：5 条命中最多注入 dialogueRecallItemsMax 条。
	hits = make([]dialogueHybridHit, 5)
	for i := range hits {
		hits[i] = dialogueHybridHit{RunID: string(rune('a' + i)), SessionID: "s", Passage: "内容", FinishedAt: &fin}
	}
	if _, n := renderDialogueHybridBlock(hits); n != dialogueRecallItemsMax {
		t.Fatalf("注入 %d 条, want %d", n, dialogueRecallItemsMax)
	}

	// 整条只剩坐标头（无换行）= 没有可引用的原文 → 跳过，全跳则整块不生成。
	hits = []dialogueHybridHit{
		{RunID: "r9", SessionID: "s9", Passage: "[会话 s9 · 场 r9]", FinishedAt: &fin},
		{RunID: "ra", SessionID: "sa", Passage: "正常内容", FinishedAt: &fin},
	}
	block, n = renderDialogueHybridBlock(hits)
	if n != 1 || !strings.Contains(block, "run ra") {
		t.Fatalf("坐标头条目应跳过:\n%s (%d)", block, n)
	}
	if b, n := renderDialogueHybridBlock([]dialogueHybridHit{
		{RunID: "r9", SessionID: "s9", Passage: "[会话 s9 · 场 r9]"},
	}); b != "" || n != 0 {
		t.Fatalf("全坐标头渲染 = %q,%d, want 空", b, n)
	}
}

// dev query 的 limit 解析：缺省 10、null 视同缺省、1–20 合法、其余（0/21/负数/非整数）非法。
func TestDialogueQueryLimit(t *testing.T) {
	if v, ok := dialogueQueryLimit(nil); !ok || v != dialogueQueryLimitDefault {
		t.Fatalf("缺省 limit = %d,%v", v, ok)
	}
	if v, ok := dialogueQueryLimit(json.RawMessage("null")); !ok || v != dialogueQueryLimitDefault {
		t.Fatalf("null limit = %d,%v", v, ok)
	}
	for _, raw := range []string{"1", "10", "20"} {
		if v, ok := dialogueQueryLimit(json.RawMessage(raw)); !ok || v == 0 {
			t.Fatalf("limit %s 应合法，得 %d,%v", raw, v, ok)
		}
	}
	if v, _ := dialogueQueryLimit(json.RawMessage("20")); v != dialogueQueryLimitMax {
		t.Fatalf("limit 20 = %d, want %d", v, dialogueQueryLimitMax)
	}
	for _, raw := range []string{"0", "21", "-1", "1.5", `"5"`, "abc", "[]"} {
		if v, ok := dialogueQueryLimit(json.RawMessage(raw)); ok {
			t.Fatalf("limit %s 应非法，得 %d", raw, v)
		}
	}
}
