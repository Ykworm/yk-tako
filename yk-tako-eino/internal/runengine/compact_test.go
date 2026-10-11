// compact 的 worker 层回归网（C2 边界 + C3 的 foldInput 假件走位）：不触 PG/Redis（那是
// store_test.go 的面），全靠假 ChatStream / 假计数器 / 假 summarize 直测保险丝边界。
// 验收口径照 todos「C 系列」：渲染精确字节、截断 rune 计、三层验货逐层、重试分流、stall 快速失败。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ---- 假件 ----

// fakeChatStream 按调用序号线程不安全地脚本化（单测顺序调用，不用并发）。
type fakeChatStream struct {
	calls   int
	handler func(call int) (*schema.StreamReader[*schema.Message], error)
	lastIn  []*schema.Message // 最近一次 Stream 收到的完整输入（C5-A2 起用于钉 system prompt）
}

func (f *fakeChatStream) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.calls++
	f.lastIn = in
	return f.handler(f.calls)
}

// streamOf 造一条纯成功的假流：content chunk 序列 + 末帧（usage/finish_reason 挂这里，同真流形状）。
func streamOf(chunks []string, completion int, finish string) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](len(chunks) + 2)
	for _, c := range chunks {
		sw.Send(&schema.Message{Role: schema.Assistant, Content: c}, nil)
	}
	sw.Send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{
		FinishReason: finish,
		Usage:        &schema.TokenUsage{CompletionTokens: completion},
	}}, nil)
	sw.Close()
	return sr
}

// streamErr 造一条建流/排干即败的假流。
func streamErr(err error) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(nil, err)
	sw.Close()
	return sr
}

// neverStream 造一条永不吐帧的假流（stall 用）；writer 由调用方持有、测试收尾关。
func neverStream() (*schema.StreamReader[*schema.Message], *schema.StreamWriter[*schema.Message]) {
	return schema.Pipe[*schema.Message](1)
}

// testSummarizer 小旋钮的 summarizer（stall 计时缩到毫秒级，不然单测要等 30s）。
// 帽取中窗常量 4096：验货断言（completion ≥ 帽）在各测试自带的 completion 量级下行为同 A3。
func testSummarizer(chat ChatStream) *summarizer {
	s := newSummarizer(chat, summarizeCeilingMid)
	s.stallFirstToken = 80 * time.Millisecond
	s.stallChunkGap = 80 * time.Millisecond
	s.stallTotal = 2 * time.Second
	return s
}

// fakeCounter 脚本化 token 计数（foldInput 走位用）。
type fakeCounter struct{ f func(string) int }

func (c fakeCounter) Count(text string) int { return c.f(text) }

// ---- renderFold：标注转录的精确字节（验收 ② / C3 第 6 条）----

func TestRenderFoldAnnotatedForms(t *testing.T) {
	drop := []runRows{
		{id: "r1", rows: []ModelMessage{
			{RunID: "r1", Seq: 1, Role: "user", Text: "查一下时间，顺便写个文件"},
			{RunID: "r1", Seq: 2, Role: "assistant", Text: "我先拿时间再写文件。",
				ToolCalls: []schema.ToolCall{
					{ID: "call_1", Function: schema.FunctionCall{Name: "get_time", Arguments: `{"tz":"Asia/Shanghai","verbose":true}`}},
					{ID: "call_2", Function: schema.FunctionCall{Name: "write_file", Arguments: `{"path":"/tmp/a.txt"}`}},
				}},
			{RunID: "r1", Seq: 3, Role: "tool", Text: "2026-10-11T10:00:00+08:00", ToolCallID: "call_1"},
			{RunID: "r1", Seq: 4, Role: "tool", Text: "ok", ToolCallID: "call_2"},
			{RunID: "r1", Seq: 5, Role: "assistant", Text: "已写入，现在是 10 点。"},
		}},
		{id: "r2", rows: []ModelMessage{
			{RunID: "r2", Seq: 1, Role: "user", Text: "谢谢"},
			{RunID: "r2", Seq: 2, Role: "assistant", Text: "不客气"},
		}},
	}
	want := `[User]: 查一下时间，顺便写个文件
[Assistant]: 我先拿时间再写文件。
[Assistant tool calls]: get_time(tz=Asia/Shanghai, verbose=true)
[Assistant tool calls]: write_file(path=/tmp/a.txt)
[Tool result get_time]: 2026-10-11T10:00:00+08:00
[Tool result write_file]: ok
[Assistant]: 已写入，现在是 10 点。

[User]: 谢谢
[Assistant]: 不客气
`
	if got := renderFold(nil, drop); got != want {
		t.Fatalf("renderFold 标注转录字节漂移：\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// 参数 key 排序（保跨次渲染一致）+ 无正文纯调用行 + 未知 tool_call_id 兜底名。
func TestRenderFoldKeySortAndFallbacks(t *testing.T) {
	drop := []runRows{{id: "r1", rows: []ModelMessage{
		{RunID: "r1", Seq: 1, Role: "assistant", ToolCalls: []schema.ToolCall{
			{ID: "call_9", Function: schema.FunctionCall{Name: "search", Arguments: `{"q":"xkcd","limit":3}`}},
		}},
		{RunID: "r1", Seq: 2, Role: "tool", Text: "裸结果", ToolCallID: "call_9"},
		{RunID: "r1", Seq: 3, Role: "tool", Text: "孤儿行", ToolCallID: "call_x"},
		{RunID: "r1", Seq: 4, Role: "assistant", Text: ""},
	}}}
	want := `[Assistant tool calls]: search(limit=3, q=xkcd)
[Tool result search]: 裸结果
[Tool result ?]: 孤儿行
[Assistant]: 
`
	if got := renderFold(nil, drop); got != want {
		t.Fatalf("renderFold 排序/兜底字节漂移：\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// 分级截断：rune 计（中文 3 bytes/字，按 byte 截会穿帮）+ 截断标记带原长度（验收 ② / C3 第 7 条）。
func TestRenderFoldTruncationRunes(t *testing.T) {
	// 阈值钉绝对值（2000/400，19v6-07 §3）：变异检查「阈值改大」要靠它红——跟着 const 走会一起漂。
	const toolResultCap, argValueCap = 2000, 400
	longResult := strings.Repeat("结", toolResultCap+1)
	longValue := strings.Repeat("参", argValueCap+1)
	drop := []runRows{{id: "r1", rows: []ModelMessage{
		{RunID: "r1", Seq: 1, Role: "assistant", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "write_file", Arguments: fmt.Sprintf(`{"content":%q}`, longValue)}},
		}},
		{RunID: "r1", Seq: 2, Role: "tool", Text: longResult, ToolCallID: "call_1"},
	}}}
	got := renderFold(nil, drop)
	head := "[Tool result write_file]: " + strings.Repeat("结", toolResultCap) +
		fmt.Sprintf(foldTruncMarkerFormat, toolResultCap+1) + "\n"
	if !strings.HasSuffix(got, head) {
		t.Fatalf("工具结果截断字节漂移（应保留前 %d runes + 标记）：\n%q", toolResultCap, got)
	}
	marker := fmt.Sprintf(foldTruncMarkerFormat, argValueCap+1)
	if !strings.Contains(got, marker) {
		t.Fatalf("参数 value 截断标记缺失（原 %d runes）：\n%q", argValueCap+1, got)
	}
	// 恰好封顶不截（边界 = 不截）。
	short := []runRows{{id: "r1", rows: []ModelMessage{{RunID: "r1", Seq: 2, Role: "tool", Text: strings.Repeat("结", toolResultCap), ToolCallID: "x"}}}}
	if got := renderFold(nil, short); strings.Contains(got, "截断") {
		t.Fatalf("恰好封顶被误截：%q", got)
	}
}

// 旧摘要 prepend 形状不回归（C1 现形状，重压合并的输入半边）。
func TestRenderFoldOldSummaryPrepend(t *testing.T) {
	drop := []runRows{{id: "r2", rows: []ModelMessage{{RunID: "r2", Seq: 1, Role: "user", Text: "新内容"}}}}
	want := "（此前摘要，请合并进新的摘要）\n旧摘要A\n[User]: 新内容\n"
	if got := renderFold([]string{"旧摘要A"}, drop); got != want {
		t.Fatalf("旧摘要 prepend 漂移：got %q want %q", got, want)
	}
}

// ---- foldInput：n 降序走位 + errStillOver（C3 第 3 条）----

func historyOf(runs ...[]ModelMessage) []ModelMessage {
	var out []ModelMessage
	for _, rows := range runs {
		out = append(out, rows...)
	}
	return out
}

func userRow(run string, seq int, text string) ModelMessage {
	return ModelMessage{RunID: RunID(run), Seq: seq, Role: "user", Text: text}
}

// n 从 20 降档：n=3 塞不下、n=2 定案——summarize 被调两次，staged 挂点/集合正确。
func TestFoldInputDescendsN(t *testing.T) {
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	calls := 0
	summarize := func(_ context.Context, in string) (string, error) {
		calls++
		return "SUMMARY", nil
	}
	// 4 个已完成 run 各 4 runes + 当前 run 4 runes。SUMMARY=7 runes。
	history := historyOf(
		[]ModelMessage{userRow("r1", 1, "aaaa")},
		[]ModelMessage{userRow("r2", 1, "bbbb")},
		[]ModelMessage{userRow("r3", 1, "cccc")},
		[]ModelMessage{userRow("r4", 1, "dddd")},
		[]ModelMessage{userRow("r5", 1, "eeee")},
	)
	// n=3: 7+4*3+4=23 ≥ 20 败；n=2: 7+4*2+4=19 < 20 定案。
	assembled, staged, err := foldInput(context.Background(), "", history, "r5", 20, counter, summarize, nil)
	if err != nil {
		t.Fatalf("foldInput: %v", err)
	}
	if calls != 2 {
		t.Fatalf("summarize 调用 %d 次，want 2（n=3 败 → n=2 成）", calls)
	}
	if staged.AttachRun != "r2" || len(staged.DropRunIDs) != 2 || staged.DropRunIDs[0] != "r1" || staged.DropRunIDs[1] != "r2" {
		t.Fatalf("staged 形状漂移: %+v", staged)
	}
	// 组装行的摘要 RunID 为空串——它是发送前的临时输入行，落库 RunID 由 EndRun 写 AttachRun。
	want := historyOf(
		[]ModelMessage{{RunID: "", Seq: 0, Role: "user", Text: "SUMMARY"}},
		[]ModelMessage{userRow("r3", 1, "cccc")},
		[]ModelMessage{userRow("r4", 1, "dddd")},
		[]ModelMessage{userRow("r5", 1, "eeee")},
	)
	if fmt.Sprint(assembled) != fmt.Sprint(want) {
		t.Fatalf("assembled 布局漂移:\n got %v\nwant %v", assembled, want)
	}
}

// n=0 仍超 limit → errStillOver（哨兵可判），且 summarize 被调用（n=0 档真压过）。
func TestFoldInputErrStillOver(t *testing.T) {
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	calls := 0
	summarize := func(_ context.Context, in string) (string, error) {
		calls++
		return "SUMMARY", nil
	}
	history := historyOf(
		[]ModelMessage{userRow("r1", 1, "aaaa")},
		[]ModelMessage{userRow("r2", 1, "eeee")},
	)
	// n=0: 摘要 7 + tail 4 = 11 ≥ 5 → errStillOver。
	_, _, err := foldInput(context.Background(), "", history, "r2", 5, counter, summarize, nil)
	if !errors.Is(err, errStillOver) {
		t.Fatalf("err=%v，want errStillOver", err)
	}
	if calls != 1 {
		t.Fatalf("summarize 调用 %d 次，want 1（n=0 档真压过再判死）", calls)
	}
}

// summarize 失败透传：foldInput 不吞错（不 Stage、不发）。
func TestFoldInputSummarizeErrorPropagates(t *testing.T) {
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	boom := errors.New("模型炸了")
	_, _, err := foldInput(context.Background(), "",
		historyOf([]ModelMessage{userRow("r1", 1, "aaaa")}, []ModelMessage{userRow("r2", 1, "eeee")}),
		"r2", 5, counter, func(context.Context, string) (string, error) { return "", boom }, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v，want 原错误透传", err)
	}
}

// ---- C5-A5：钉块与三档收口检查 ----

// pinOnce 跑一次定案必成的 foldInput（limit 足够大、摘要固定 SUMMARY），返回组装输入与 staged。
func pinOnce(t *testing.T, history []ModelMessage, fileEffects map[string]string) ([]ModelMessage, *StagedCompaction) {
	t.Helper()
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	assembled, staged, err := foldInput(context.Background(), "", history, "r_cur", 100000, counter,
		func(context.Context, string) (string, error) { return "SUMMARY", nil }, fileEffects)
	if err != nil {
		t.Fatalf("foldInput: %v", err)
	}
	return assembled, staged
}

// 钉块全链：drop 里的 write_file 提取 → 块渲染 → 钉进 assembled[0] 与 staged.Summary（同源）。
func TestFoldInputPinsFileOpsBlock(t *testing.T) {
	history := historyOf(
		[]ModelMessage{fileCallMsg("r1", fileCall("write_file", `{"path":"/tmp/a.txt"}`))},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	assembled, staged := pinOnce(t, history, firstPartyFileEffects)
	want := "SUMMARY\n\n<!-- tako:file-ops v1 -->\n改过:\n- /tmp/a.txt（write_file，r1）\n<!-- /tako:file-ops -->"
	if staged.Summary != want {
		t.Fatalf("staged.Summary 漂移：\n got %q\nwant %q", staged.Summary, want)
	}
	if assembled[0].Text != want {
		t.Fatalf("发送候选的摘要行应同源钉块：got %q", assembled[0].Text)
	}
}

// 无 file 调用 → 无块 → staged.Summary 与无 fileOps 时代逐字节相同（零行为变化）。
func TestFoldInputNoOpsNoBlock(t *testing.T) {
	history := historyOf(
		[]ModelMessage{fileCallMsg("r1", fileCall("get_time", `{"tz":"Asia/Shanghai"}`))},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	_, staged := pinOnce(t, history, firstPartyFileEffects)
	if staged.Summary != "SUMMARY" {
		t.Fatalf("无 file 调用不应有块：%q", staged.Summary)
	}
}

// 收口第 1→2 档：全量 60 条钉入后合计 ≥ limit，砍到 50 条后 < limit 定案（砍最老 /f00–/f09）。
func TestFoldInputBlockOverLimitCapsTo50(t *testing.T) {
	calls := make([]schema.ToolCall, 0, 60)
	for i := 0; i < 60; i++ {
		calls = append(calls, fileCall("write_file", fmt.Sprintf(`{"path":"/f%02d"}`, i)))
	}
	history := historyOf(
		[]ModelMessage{fileCallMsg("r1", calls...)},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	// 行 22 runes ×60 = 1373 块；SUMMARY 7 + sep 2 + tail 4 = 13；limit 1200：
	// 全量 1386 ≥ 1200 败第 1 档；砍 50 后 1166 < 1200 过第 2 档。
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	assembled, staged, err := foldInput(context.Background(), "", history, "r_cur", 1200, counter,
		func(context.Context, string) (string, error) { return "SUMMARY", nil }, firstPartyFileEffects)
	if err != nil {
		t.Fatalf("砍 50 后应收口成功: %v", err)
	}
	if got := strings.Count(staged.Summary, "（write_file，r1）"); got != fileOpsTableCap {
		t.Fatalf("cap 后条目 %d 条，want %d", got, fileOpsTableCap)
	}
	if strings.Contains(staged.Summary, "/f00（") || strings.Contains(staged.Summary, "/f09（") {
		t.Fatalf("应砍最老 /f00–/f09：%q", staged.Summary)
	}
	if !strings.Contains(staged.Summary, "/f10（") || !strings.Contains(staged.Summary, "/f59（") {
		t.Fatalf("应保留 /f10–/f59：%q", staged.Summary)
	}
	if assembled[0].Text != staged.Summary {
		t.Fatalf("发送候选与 staged 应同源（cap 版）")
	}
}

// 收口第 3 档：砍 50 后仍 ≥ limit → errStillOver、staged 不产出（干净 A）。
func TestFoldInputBlockStillOver(t *testing.T) {
	calls := make([]schema.ToolCall, 0, 60)
	for i := 0; i < 60; i++ {
		calls = append(calls, fileCall("write_file", fmt.Sprintf(`{"path":"/f%02d"}`, i)))
	}
	history := historyOf(
		[]ModelMessage{fileCallMsg("r1", calls...)},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	_, staged, err := foldInput(context.Background(), "", history, "r_cur", 1100, counter,
		func(context.Context, string) (string, error) { return "SUMMARY", nil }, firstPartyFileEffects)
	if !errors.Is(err, errStillOver) {
		t.Fatalf("err=%v，want errStillOver（砍 50 后 1166 ≥ 1100）", err)
	}
	if staged != nil {
		t.Fatalf("第 3 档不应 Stage：%+v", staged)
	}
	if !strings.Contains(err.Error(), "fileOps 块") {
		t.Fatalf("错误应点名 fileOps 块档位：%v", err)
	}
}

// 继承：旧块条目先进池、新提取排后；同 path 跨批去重保留旧注记；DropRunIDs 含旧摘要 run。
func TestFoldInputInheritsOldBlock(t *testing.T) {
	oldSummary := "旧摘要正文\n\n<!-- tako:file-ops v1 -->\n改过:\n- /w/old.md（write_file，r_old）\n<!-- /tako:file-ops -->"
	history := historyOf(
		[]ModelMessage{{RunID: "r1", Seq: 0, Role: "user", Text: oldSummary}},
		[]ModelMessage{
			userRow("r2", 1, "再改一版"),
			fileCallMsg("r2",
				fileCall("write_file", `{"path":"/w/new.txt"}`),
				fileCall("edit_file", `{"path":"/w/old.md"}`)),
		},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	_, staged := pinOnce(t, history, firstPartyFileEffects)
	if strings.Count(staged.Summary, "/w/old.md") != 1 {
		t.Fatalf("同 path 跨批应去重且保留旧注记：%q", staged.Summary)
	}
	if !strings.Contains(staged.Summary, "- /w/old.md（write_file，r_old）") {
		t.Fatalf("旧条目注记应原样继承（旧在先）：%q", staged.Summary)
	}
	if !strings.Contains(staged.Summary, "- /w/new.txt（write_file，r2）") {
		t.Fatalf("新提取应在池中：%q", staged.Summary)
	}
	// 旧条目在前、新条目在后：淘汰顺序的池序。
	if strings.Index(staged.Summary, "/w/old.md") > strings.Index(staged.Summary, "/w/new.txt") {
		t.Fatalf("旧条目应先于新提取入池：%q", staged.Summary)
	}
	if len(staged.DropRunIDs) != 2 || staged.DropRunIDs[0] != "r1" || staged.DropRunIDs[1] != "r2" {
		t.Fatalf("DropRunIDs 应含旧摘要 run：%+v", staged.DropRunIDs)
	}
}

// 旧块缺闭标记（解析失败）→ 按空处理 + log，不拒收；正常摘要照出、不带块。
func TestFoldInputBrokenOldBlockTreatedEmpty(t *testing.T) {
	oldSummary := "旧摘要\n<!-- tako:file-ops v1 -->\n改过:\n- /w/a.md（write_file，r_1）" // 无闭标记
	history := historyOf(
		[]ModelMessage{{RunID: "r1", Seq: 0, Role: "user", Text: oldSummary}},
		[]ModelMessage{userRow("r2", 1, "普通消息")},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	_, staged := pinOnce(t, history, firstPartyFileEffects)
	if staged.Summary != "SUMMARY" {
		t.Fatalf("坏块应整块按空、且无新提取 → 无块：%q", staged.Summary)
	}
}

// echo 冒充不了继承源：旧文本里出现两个块时取最后一个（程序钉的块在摘要末尾）。
func TestFoldInputOldBlockTakesLastCopy(t *testing.T) {
	oldSummary := "正文开头 echo 了见过的块：\n" +
		"<!-- tako:file-ops v1 -->\n改过:\n- /w/fake.md（write_file，r_fake）\n<!-- /tako:file-ops -->\n" +
		"正文继续。\n\n" +
		"<!-- tako:file-ops v1 -->\n读过:\n- /w/real.md（read_file，r_9）\n<!-- /tako:file-ops -->"
	history := historyOf(
		[]ModelMessage{{RunID: "r1", Seq: 0, Role: "user", Text: oldSummary}},
		[]ModelMessage{userRow("r2", 1, "普通消息")},
		[]ModelMessage{userRow("r_cur", 1, "eeee")},
	)
	_, staged := pinOnce(t, history, firstPartyFileEffects)
	if strings.Contains(staged.Summary, "/w/fake.md") {
		t.Fatalf("伪块（非末尾）不该被继承：%q", staged.Summary)
	}
	if !strings.Contains(staged.Summary, "- /w/real.md（read_file，r_9）") {
		t.Fatalf("末尾真块应被继承：%q", staged.Summary)
	}
}

// ---- summarizer：三层验货 / 重试分流 / stall（验收 ⑤⑥⑦ / C3 第 8–11 条）----

// 算术主检：completion ≥ 自设上限 ⇒ 拒收 + 重试耗尽（验货拒收可重试，代价仅多两次调用）。
func TestSummarizeRejectsCompletionAtCap(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return streamOf([]string{"一份摘要"}, summarizeCeilingMid, "stop"), nil
	}}
	_, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassRejected) || !strings.Contains(err.Error(), "重试") {
		t.Fatalf("err=%v，want 验货拒收 + 重试耗尽", err)
	}
	if chat.calls != summarizeMaxAttempts {
		t.Fatalf("调用 %d 次，want %d（验货拒收重试到耗尽）", chat.calls, summarizeMaxAttempts)
	}
}

// 字符串辅检：finish_reason 黑名单（大小写不敏感——Gemini 原生 MAX_TOKENS 同拦）。
func TestSummarizeRejectsTruncFinishReason(t *testing.T) {
	for _, finish := range []string{"length", "max_tokens", "max_output_tokens", "MAX_TOKENS"} {
		chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
			return streamOf([]string{"摘"}, 10, finish), nil
		}}
		s := testSummarizer(chat)
		s.maxAttempts = 1
		_, err := s.summarize(context.Background(), "输入")
		if err == nil || !strings.Contains(err.Error(), summarizeClassRejected) {
			t.Fatalf("finish=%q err=%v，want 验货拒收", finish, err)
		}
	}
}

// 缺失处理：finish_reason 缺失 + completion < 上限 ⇒ 放行（不误杀）。
func TestSummarizeAllowsMissingFinishReason(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return streamOf([]string{"可用摘要"}, 100, ""), nil
	}}
	out, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err != nil || out != "可用摘要" {
		t.Fatalf("out=%q err=%v，want 放行", out, err)
	}
}

// 重试分流：瞬时错误原样重试到成功；成功全文正确。
func TestSummarizeRetriesTransientThenSucceeds(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		if call == 1 {
			return nil, fmt.Errorf("request failed with status 429: rate limit")
		}
		return streamOf([]string{"重试后", "的全文"}, 100, "stop"), nil
	}}
	out, err := testSummarizer(chat).summarize(context.Background(), "同一份输入")
	if err != nil || out != "重试后的全文" {
		t.Fatalf("out=%q err=%v，want 重试成功全文", out, err)
	}
	if chat.calls != 2 {
		t.Fatalf("调用 %d 次，want 2（首次 429 原样重试）", chat.calls)
	}
}

// 重试分流：超长错误不重试同一份输入——一次即败，分类 = 超长拒收。
func TestSummarizeLengthErrorNoRetry(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return nil, fmt.Errorf("openai error: request entity too large")
	}}
	_, err := testSummarizer(chat).summarize(context.Background(), "同一份输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassLength) || strings.Contains(err.Error(), "重试") {
		t.Fatalf("err=%v，want 超长拒收且不重试", err)
	}
	if chat.calls != 1 {
		t.Fatalf("调用 %d 次，want 1（超长不重试）", chat.calls)
	}
}

// 重试分流：瞬时错误耗尽 ⇒ 分类含 瞬时 + 重试 + 耗尽，调用 = 上限。
func TestSummarizeTransientExhausted(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return nil, fmt.Errorf("internal server error")
	}}
	_, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassTransient) ||
		!strings.Contains(err.Error(), "重试") || !strings.Contains(err.Error(), "耗尽") {
		t.Fatalf("err=%v，want 瞬时重试耗尽", err)
	}
	if chat.calls != summarizeMaxAttempts {
		t.Fatalf("调用 %d 次，want %d", chat.calls, summarizeMaxAttempts)
	}
}

// stall：首 token 永不至 ⇒ 快速失败（不挂死 run），分类 = stall，不重试。
func TestSummarizeStallFirstToken(t *testing.T) {
	sr, sw := neverStream()
	defer sw.Close() // 收尾解放阻塞在 Recv 的假流
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return sr, nil
	}}
	_, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassStall) {
		t.Fatalf("err=%v，want stall", err)
	}
	if chat.calls != 1 {
		t.Fatalf("调用 %d 次，want 1（stall 不重试）", chat.calls)
	}
}

// stall：首 chunk 到了、第二个间隔超时 ⇒ 同样快速失败。
func TestSummarizeStallChunkGap(t *testing.T) {
	sr, sw := neverStream()
	sw.Send(&schema.Message{Role: schema.Assistant, Content: "半句话"}, nil)
	defer sw.Close()
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return sr, nil
	}}
	_, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassStall) || !strings.Contains(err.Error(), "间隔") {
		t.Fatalf("err=%v，want stall（chunk 间隔）", err)
	}
}

// 排干中途断连（EOF 之前的流错误）按错误内容分类：断连 = 瞬时，可重试成功。
func TestSummarizeMidStreamDisconnectRetries(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		if call == 1 {
			return streamErr(fmt.Errorf("failed to receive stream chunk: %w", errors.New("connection reset by peer"))), nil
		}
		return streamOf([]string{"续上了"}, 10, "stop"), nil
	}}
	out, err := testSummarizer(chat).summarize(context.Background(), "输入")
	if err != nil || out != "续上了" {
		t.Fatalf("out=%q err=%v，want 断连重试成功", out, err)
	}
	if chat.calls != 2 {
		t.Fatalf("调用 %d 次，want 2", chat.calls)
	}
}

// 空摘要拒收（C1 现状语义保留，现归验货拒收类、可重试）。
func TestSummarizeEmptyRejected(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return streamOf(nil, 10, "stop"), nil
	}}
	s := testSummarizer(chat)
	s.maxAttempts = 1
	_, err := s.summarize(context.Background(), "输入")
	if err == nil || !strings.Contains(err.Error(), summarizeClassRejected) {
		t.Fatalf("err=%v，want 验货拒收（空摘要）", err)
	}
}

// ---- classifyStreamErr 判定面 ----

func TestClassifyStreamErr(t *testing.T) {
	cases := []struct {
		err   error
		class string
	}{
		{fmt.Errorf("status 429"), summarizeClassTransient},
		{fmt.Errorf("status 503 service unavailable"), summarizeClassTransient},
		{fmt.Errorf("connection reset by peer"), summarizeClassTransient},
		{fmt.Errorf("request entity too large"), summarizeClassLength},
		{fmt.Errorf("This model's maximum context length is 131072 tokens"), summarizeClassLength},
		{fmt.Errorf("invalid api key"), summarizeClassOther},
		{context.DeadlineExceeded, summarizeClassStall},
		{context.Canceled, summarizeClassOther},
	}
	for _, c := range cases {
		if got := classifyStreamErr(c.err); got != c.class {
			t.Fatalf("classify(%v) = %q, want %q", c.err, got, c.class)
		}
	}
}

// ---- C5-A1：POST 单条消息准入（tail 支）----

// 准入边界：窗口 600 → limit 480 → 线 = 160；恰好在线放行、越线拒收，错误体带回 limit/tokens。
func TestAdmitTextBoundary(t *testing.T) {
	w := &Worker{compactWindow: 600, compactLimit: 480, counter: fakeCounter{f: func(s string) int { return len([]rune(s)) }}}
	for _, tc := range []struct {
		tokens int
		ok     bool
	}{
		{0, true}, {1, true}, {159, true}, {160, true}, // 恰好在线 = 放行（≤）
		{161, false}, {480, false}, {5000, false},
	} {
		limit, tokens, ok := w.AdmitText(strings.Repeat("x", tc.tokens))
		if limit != 480 || tokens != tc.tokens || ok != tc.ok {
			t.Fatalf("tokens=%d: got (limit=%d tokens=%d ok=%v), want (480 %d %v)", tc.tokens, limit, tokens, ok, tc.tokens, tc.ok)
		}
	}
}

// compact 关闭（window=0）= 准入整体消失：多大都放行（返回零值，调用方不渲染 413）。
func TestAdmitTextDisabledWhenWindowOff(t *testing.T) {
	w := &Worker{compactWindow: 0, counter: fakeCounter{f: func(s string) int { return len([]rune(s)) }}}
	limit, tokens, ok := w.AdmitText(strings.Repeat("x", 99999))
	if limit != 0 || tokens != 0 || !ok {
		t.Fatalf("window=0: got (%d %d %v), want (0 0 true)", limit, tokens, ok)
	}
}

// window×4/5 的整数关系照另一条线重验（防除数与乘数改了一边）。
func TestAdmitTextLimitFollowsWindow(t *testing.T) {
	w := &Worker{compactWindow: 100, compactLimit: 80, counter: fakeCounter{f: func(s string) int { return len([]rune(s)) }}}
	// limit = 80，线 = 80/3 = 26。
	if _, _, ok := w.AdmitText(strings.Repeat("x", 26)); !ok {
		t.Fatal("26 tok 应放行（恰好在线）")
	}
	if limit, tokens, ok := w.AdmitText(strings.Repeat("x", 27)); ok || limit != 80 || tokens != 27 {
		t.Fatalf("27 tok: got (%d %d %v), want (80 27 false)", limit, tokens, ok)
	}
}

// ---- C5-A2：结构化摘要 prompt（六槽骨架）----

// 槽位合同：六槽标题在 prompt 中按定序出现（槽序恒定是跨合并漂移的防线），四条规则各有着落。
func TestCompactPromptSixSlotsInOrder(t *testing.T) {
	headers := []string{
		"## 会话意图", "## 关键决策", "## 重要事实",
		"## 待办与下一步", "## 用户偏好", "## 其他上下文",
	}
	last := -1
	for _, h := range headers {
		i := strings.Index(compactPrompt, h)
		if i < 0 {
			t.Fatalf("prompt 缺槽位 %q", h)
		}
		if i < last {
			t.Fatalf("槽位 %q 顺序漂移（应顺序出现）", h)
		}
		last = i
	}
	for _, rule := range []string{
		"直接省略整个标题",       // 规则①空槽省略不编造
		"%d 字以内",         // 规则②总量预算为占位符——渲染值随硬帽（A3 动态化）
		"整体并进「## 其他上下文」", // 规则③散文迁移语义
		"不允许输出没有槽标题的正文",  // 规则④：迁移/echo 时骨架仍必须存在（A2 e2e 防回归）
		"不要开场白、不要代码块围栏",  // 规则⑤输出纯净
		"理由必须写",          // 关键决策带理由
	} {
		if !strings.Contains(compactPrompt, rule) {
			t.Fatalf("prompt 缺规则条款 %q", rule)
		}
	}
}

// 接线钉：summarize 真把结构化 prompt 放在 system 位发出（防改 prompt 名/位时悄悄失联）。
func TestSummarizeSendsStructuredPrompt(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return streamOf([]string{"## 会话意图\n验证"}, 10, "stop"), nil
	}}
	out, err := testSummarizer(chat).summarize(context.Background(), "任意输入")
	if err != nil || !strings.HasPrefix(out, "## 会话意图") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(chat.lastIn) != 2 || chat.lastIn[0].Role != schema.System {
		t.Fatalf("输入应 = [system, user] 两条: %+v", chat.lastIn)
	}
	for _, h := range []string{"## 会话意图", "## 关键决策", "## 重要事实", "## 待办与下一步", "## 用户偏好", "## 其他上下文"} {
		if !strings.Contains(chat.lastIn[0].Content, h) {
			t.Fatalf("system 消息缺槽位 %q", h)
		}
	}
}

// ---- C5-A7：summarizer 预算帽分档（小窗式 / 中窗 / 大窗 + 档界 + 两个 override）----

// 五档定点与档界两侧：小窗式 min(limit/2, 1024)、中窗 4096、大窗 8192、window=0 回落中窗常量。
func TestSummarizeCeilingTiers(t *testing.T) {
	for _, tc := range []struct {
		window int
		want   int
	}{
		{0, 4096},       // compact 关：中窗常量（现行 limit>0 不生效时的语义）
		{100, 40},       // 极限 dev 窗：limit/2 = 40 未被 1024 封顶
		{600, 240},      // window 600（C2/A3 同窗）：limit/2 = 240
		{4000, 1024},    // window 4000：limit/2 = 1600 → 1024 封顶（A7 新档）
		{32768, 1024},   // 小窗档界内侧
		{32769, 4096},   // 档界外侧 → 中窗
		{256000, 4096},  // 256k 生产窗
		{1048575, 4096}, // 大窗档界内侧
		{1048576, 8192}, // 档界 → 大窗
	} {
		if got := summarizeCeiling(tc.window, "", 0); got != tc.want {
			t.Fatalf("window=%d: ceiling=%d, want %d", tc.window, got, tc.want)
		}
	}
}

// Claude override：ref 的 provider 段 = anthropic 或整体含 claude（小写化后）→ 固定档 ×1.3
// （13/10 整数截断）；只乘固定档——小窗式（含 window=0 回落）是恒有解算术本体，乘了破守恒。
func TestSummarizeCeilingClaudeOverride(t *testing.T) {
	for _, tc := range []struct {
		window int
		ref    string
		want   int
	}{
		{256000, "anthropic/claude-sonnet-4", 5324}, // provider 段 anthropic（4096×1.3=5324.8→5324）
		{256000, "MyVendor/CLAUDE-Opus", 5324},      // claude 子串，大小写不敏感
		{256000, "deepseek/deepseek-chat", 4096},    // 非 claude 不触发
		{1048576, "anthropic/claude-opus-4", 10649}, // 大窗档 ×1.3（8192×1.3=10649.6→10649）
		{32769, "anthropic/claude-3", 5324},         // 档界外侧即享乘
		{32768, "anthropic/claude-3", 1024},         // 小窗档界内侧：小窗式不被乘
		{4000, "anthropic/claude-3", 1024},          // 小窗式不被乘
		{0, "anthropic/claude-3", 4096},             // window=0 回落不乘（compact 关，惰性）
	} {
		if got := summarizeCeiling(tc.window, tc.ref, 0); got != tc.want {
			t.Fatalf("window=%d ref=%q: ceiling=%d, want %d", tc.window, tc.ref, got, tc.want)
		}
	}
}

// max_output 硬顶：min(ceiling, 值) 最后生效（乘后也顶）；缺省/0 = 不顶。
func TestSummarizeCeilingMaxOutput(t *testing.T) {
	for _, tc := range []struct {
		window    int
		ref       string
		maxOutput int
		want      int
	}{
		{4000, "", 500, 500},                              // 小于帽 → 顶到 500
		{4000, "", 2048, 1024},                            // 大于帽 → 帽不变
		{4000, "", 1024, 1024},                            // 等于帽
		{256000, "", 0, 4096},                             // 0 = 不顶
		{256000, "anthropic/claude-sonnet-4", 5000, 5000}, // 乘后 5324 → 顶到 5000（硬顶最后生效）
		{256000, "anthropic/claude-sonnet-4", 5324, 5324}, // 等于乘后帽
	} {
		if got := summarizeCeiling(tc.window, tc.ref, tc.maxOutput); got != tc.want {
			t.Fatalf("window=%d ref=%q maxOutput=%d: ceiling=%d, want %d",
				tc.window, tc.ref, tc.maxOutput, got, tc.want)
		}
	}
}

// prompt 的 %d 预算位真被渲染成数（防模板与 Sprintf 脱节——占位符忘渲染会直接以 "%d" 上线）。
func TestAttemptRendersBudgetIntoPrompt(t *testing.T) {
	chat := &fakeChatStream{handler: func(call int) (*schema.StreamReader[*schema.Message], error) {
		return streamOf([]string{"## 会话意图\n验证"}, 10, "stop"), nil
	}}
	s := newSummarizer(chat, summarizeCeiling(600, "", 0)) // 帽 = 240 tok → 字预算 = 192
	if _, _, err := s.attempt(context.Background(), "任意输入", 1); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	sys := chat.lastIn[0].Content
	if strings.Contains(sys, "%d") {
		t.Fatalf("prompt 预算位未渲染：%q", sys)
	}
	if !strings.Contains(sys, "192 字以内") {
		t.Fatalf("prompt 字预算应为 192（240×4/5）：%q", sys)
	}
}
