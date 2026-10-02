// 本文件：钉住第 9 节的进入条件、N 的下降、检索只发生一次，以及失败时不改原消息。
package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestPlanUnderLimitDoesNotSummarize 钉住：计数不大于 limit 时不摘要，并返回新的消息对象。
func TestPlanUnderLimitDoesNotSummarize(t *testing.T) {
	original := schema.UserMessage("短")
	deps := newRecordingDeps(sumContent, sumContent)
	out, err := Plan(context.Background(), Input{
		Messages: []*schema.Message{original},
		Window:   100,
		Query:    "短",
	}, deps.deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Compacted || deps.summaries != 0 || deps.notifies != 0 {
		t.Fatalf("不该压缩：compacted=%v summaries=%d notifies=%d", out.Compacted, deps.summaries, deps.notifies)
	}
	if len(deps.logs) != 0 {
		t.Fatalf("没超过阈值时不应打日志：%v", deps.logs)
	}
	if len(out.Messages) != 1 || out.Messages[0] == original {
		t.Fatal("没超过阈值时也应返回新的消息对象")
	}
	out.Messages[0].Content = "改副本"
	if original.Content != "短" {
		t.Fatalf("原消息被写成 %q", original.Content)
	}
}

// TestPlanEqualityDoesNotStart 钉住：计数等于 limit 时不开始压缩。
func TestPlanEqualityDoesNotStart(t *testing.T) {
	limit := Limit(100)
	deps := newRecordingDeps(func([]*schema.Message) int { return limit }, sumContent)
	out, err := Plan(context.Background(), Input{
		Messages: []*schema.Message{schema.UserMessage("刚好")},
		Window:   100,
	}, deps.deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Compacted || deps.summaries != 0 {
		t.Fatalf("等于 %d 时不应开始，summaries=%d", limit, deps.summaries)
	}
}

// TestPlanEqualityDoesNotExit 钉住：循环里计数等于 limit 时继续压，不会停下来发送。
func TestPlanEqualityDoesNotExit(t *testing.T) {
	limit := Limit(100)
	deps := newRecordingDeps(
		func([]*schema.Message) int { return limit + 1 },
		func(msgs []*schema.Message) int {
			for _, msg := range msgs {
				if isSynthetic(msg) {
					return limit
				}
			}
			return limit + 1
		},
	)
	_, err := Plan(context.Background(), Input{
		Messages: []*schema.Message{schema.UserMessage(strings.Repeat("长", 30))},
		Window:   100,
		Query:    "问",
	}, deps.deps)
	if !errors.Is(err, ErrStillOver) {
		t.Fatalf("err = %v，want ErrStillOver", err)
	}
	if deps.summaries != 0 {
		t.Fatalf("没有可换掉的已结束轮，不应摘要，实际 %d 次", deps.summaries)
	}
	text := strings.Join(deps.logs, "\n")
	if !strings.Contains(text, "n 从 20 降到 0") || !strings.Contains(text, "这一轮不发给模型") {
		t.Fatalf("日志 = %s", text)
	}
	if strings.Contains(text, "摘要正文") {
		t.Fatalf("没有摘要却打出了正文：%s", text)
	}
}

// TestPlanReducesNAndSearchesOnce 钉住：N 从 20 降到能放下为止；同一轮原文只写一次检索；检索只查一次。
func TestPlanReducesNAndSearchesOnce(t *testing.T) {
	window := 63 // 63*4/5 = 50
	msgs := []*schema.Message{
		schema.UserMessage(strings.Repeat("a", 30)),
		schema.UserMessage(strings.Repeat("b", 30)),
		schema.UserMessage(strings.Repeat("c", 30)),
		schema.UserMessage(strings.Repeat("d", 10)),
	}
	deps := newRecordingDeps(testTokens, testTokens)
	deps.deps.SearchRAG = func(string) (string, error) {
		deps.searches++
		return "旧对话里有压缩", nil
	}
	deps.deps.Summarize = func(context.Context, string) (string, error) {
		deps.summaries++
		return "S", nil
	}
	query := msgs[3].Content
	out, err := Plan(context.Background(), Input{
		Messages: msgs,
		RunIDs:   []string{"a", "b", "c"},
		Window:   window,
		Query:    query,
	}, deps.deps)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Compacted {
		t.Fatal("应压缩")
	}
	if deps.searches != 1 {
		t.Fatalf("检索 %d 次，want 1", deps.searches)
	}
	if deps.writes["a"] != 1 || deps.writes["b"] != 1 || deps.writes["c"] != 0 {
		t.Fatalf("写入次数 = %+v，want a、b 各一次", deps.writes)
	}
	if len(out.KeptRunIDs) != 1 || out.KeptRunIDs[0] != "c" {
		t.Fatalf("留下 = %+v", out.KeptRunIDs)
	}
	if len(out.DroppedRunIDs) != 2 || out.DroppedRunIDs[0] != "a" || out.DroppedRunIDs[1] != "b" {
		t.Fatalf("换掉 = %+v", out.DroppedRunIDs)
	}
	if !strings.Contains(out.CombinedUser, "S") || !strings.HasSuffix(out.CombinedUser, query) {
		t.Fatalf("拼接 = %q", out.CombinedUser)
	}
	for _, msg := range out.Messages {
		if isSynthetic(msg) && msg.Extra[extraCompact] == "rag" {
			t.Fatal("检索结果已经写进新摘要，不应再单独占一条消息")
		}
	}
	if !strings.Contains(out.Messages[0].Content, SummaryHeading) {
		t.Fatalf("首条 = %q", out.Messages[0].Content)
	}
	for _, msg := range out.Messages {
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			t.Fatal("拼装后的消息不应保留 Usage")
		}
	}
	for _, msg := range msgs {
		if strings.Contains(msg.Content, SummaryHeading) {
			t.Fatal("原消息被写成了摘要")
		}
	}
	text := strings.Join(deps.logs, "\n")
	for _, want := range []string{
		"窗口大小 63",
		"阈值 50",
		"CountPrompt 100",
		"n 从 20 降到 2",
		"摘要正文：\nS",
		"CountMessages 41 < 阈值 50",
		"留下 c，换掉 a,b",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志缺少 %q\n%s", want, text)
		}
	}
}

// TestPlanFirstSummaryAtN0RetriesWithoutRAG 钉住：只剩一轮已结束对话时，第一次摘要落在 n 为 0，
// 检索块还单独放着。这一档不少于 limit 时再压一次摘要本身，不再带检索块。
func TestPlanFirstSummaryAtN0RetriesWithoutRAG(t *testing.T) {
	window := 63 // limit = 50
	msgs := []*schema.Message{
		schema.UserMessage("old"),
		schema.UserMessage("now"),
	}
	deps := newRecordingDeps(func([]*schema.Message) int { return 100 }, countSummaryAsOne)
	deps.deps.SearchRAG = func(string) (string, error) {
		deps.searches++
		return strings.Repeat("r", 80), nil
	}
	out, err := Plan(context.Background(), Input{
		Messages: msgs,
		RunIDs:   []string{"old"},
		Window:   window,
		Query:    "now",
	}, deps.deps)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Compacted {
		t.Fatal("去掉检索块后再压一次摘要，应能放下")
	}
	if deps.summaries != 2 || deps.searches != 1 {
		t.Fatalf("摘要 %d 次，检索 %d 次，want 2 和 1", deps.summaries, deps.searches)
	}
	for _, msg := range out.Messages {
		if isSynthetic(msg) && msg.Extra[extraCompact] == "rag" {
			t.Fatal("第二次拼装不应再带检索块")
		}
	}
	text := strings.Join(deps.logs, "\n")
	if !strings.Contains(text, "再压一次摘要本身") {
		t.Fatalf("日志 = %s", text)
	}
}

// countSummaryAsOne 把摘要消息算成 1，其余消息按正文字节数。用来把检索块单独撑过阈值。
func countSummaryAsOne(msgs []*schema.Message) int {
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if isSynthetic(msg) && msg.Extra[extraCompact] == "summary" {
			n++
			continue
		}
		n += len(msg.Content)
	}
	return n
}

// TestPlanStillOverDoesNotMutate 钉住：未完成的这一轮自己已经不小于 limit 时返回 ErrStillOver，原消息不动。
func TestPlanStillOverDoesNotMutate(t *testing.T) {
	original := schema.UserMessage(strings.Repeat("长", 100))
	usage := &schema.Message{
		Role:         schema.Assistant,
		Content:      strings.Repeat("答", 10),
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 9}},
	}
	deps := newRecordingDeps(sumContent, sumContent)
	_, err := Plan(context.Background(), Input{
		Messages: []*schema.Message{
			schema.UserMessage(strings.Repeat("旧", 10)),
			usage,
			original,
		},
		RunIDs: []string{"old"},
		Window: 63,
		Query:  original.Content,
	}, deps.deps)
	if !errors.Is(err, ErrStillOver) {
		t.Fatalf("err = %v，want ErrStillOver", err)
	}
	if !strings.HasPrefix(original.Content, "长") {
		t.Fatalf("原话被改成 %q", original.Content)
	}
	if usage.ResponseMeta.Usage.TotalTokens != 9 {
		t.Fatalf("Usage = %+v", usage.ResponseMeta.Usage)
	}
	if deps.writes["old"] != 1 {
		t.Fatalf("离开保留集合时应写过检索，实际 %+v", deps.writes)
	}
}

// TestPlanNoCompactKeepsUsage 钉住：没压缩的副本仍带着 Usage，下一轮还能拿它当基数。
func TestPlanNoCompactKeepsUsage(t *testing.T) {
	msg := &schema.Message{
		Role:         schema.Assistant,
		Content:      "短",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 9}},
	}
	deps := newRecordingDeps(func([]*schema.Message) int { return 1 }, sumContent)
	out, err := Plan(context.Background(), Input{Messages: []*schema.Message{msg}, Window: 100}, deps.deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0] == msg || out.Messages[0].ResponseMeta == nil || out.Messages[0].ResponseMeta.Usage == nil {
		t.Fatal("副本应单独带着 Usage")
	}
	if out.Messages[0].ResponseMeta.Usage.TotalTokens != 9 {
		t.Fatalf("Usage = %+v", out.Messages[0].ResponseMeta.Usage)
	}
	out.Messages[0].ResponseMeta.Usage.TotalTokens = 1
	if msg.ResponseMeta.Usage.TotalTokens != 9 {
		t.Fatal("改副本写穿了原 Usage")
	}
}

// testTokens 把压缩自己放进去的摘要和检索结果算成 1。
// 标题的字节数不参与这个测试，N 降到哪一轮才只由原消息的长度决定。
func testTokens(msgs []*schema.Message) int {
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if isSynthetic(msg) {
			n++
			continue
		}
		n += len(msg.Content)
	}
	return n
}

// sumContent 用正文字节数当 token 数，让测试不依赖 BPE 的具体切分。
func sumContent(msgs []*schema.Message) int {
	n := 0
	for _, msg := range msgs {
		if msg != nil {
			n += len(msg.Content)
		}
	}
	return n
}

// recordingDeps 记下摘要、检索、写入和打出的日志。
type recordingDeps struct {
	deps      Deps
	summaries int
	searches  int
	notifies  int
	writes    map[string]int
	logs      []string
}

func newRecordingDeps(countPrompt, countMessages func([]*schema.Message) int) *recordingDeps {
	rec := &recordingDeps{writes: map[string]int{}}
	rec.deps = Deps{
		CountPrompt:   countPrompt,
		CountMessages: countMessages,
		Summarize: func(context.Context, string) (string, error) {
			rec.summaries++
			return "S", nil
		},
		WriteRAG: func(runID, _ string) error {
			rec.writes[runID]++
			return nil
		},
		SearchRAG: func(string) (string, error) {
			rec.searches++
			return "", nil
		},
		Notify: func(string) { rec.notifies++ },
		Logf: func(format string, args ...any) {
			rec.logs = append(rec.logs, fmt.Sprintf(format, args...))
		},
	}
	return rec
}
