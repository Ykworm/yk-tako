// 本文件：钉住 CountPrompt 与 CountMessages 的口径。计数用真的 cl100k_base，不打模型。
package compact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestCountPromptUsesUsageBaseline 钉住：最后一条 TotalTokens 大于 0 的助手消息是基数，
// 只把它后面的消息交给 BPE。基数已经含那次请求的系统指令和工具说明，这里不再加。
func TestCountPromptUsesUsageBaseline(t *testing.T) {
	est := testEstimator(t)
	msgs := []*schema.Message{
		schema.UserMessage("你好"),
		{
			Role:    schema.Assistant,
			Content: "早",
			ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
				TotalTokens: 1000,
				PromptTokenDetails: schema.PromptTokenDetails{
					CachedTokens: 5000,
				},
			}},
		},
		schema.UserMessage("再问一句"),
	}
	got := est.CountPrompt(msgs)
	want := 1000 + est.CountMessages(msgs[2:])
	if got != want {
		t.Fatalf("CountPrompt = %d，want %d", got, want)
	}
	if got == est.CountMessages(msgs) {
		t.Fatal("有 Usage 时不应把整段消息再数一遍")
	}
}

// TestCountPromptFallsBackToPromptPlusCompletion 钉住：TotalTokens 为 0 时用
// PromptTokens 与 CompletionTokens 之和。缓存计数不另加。
func TestCountPromptFallsBackToPromptPlusCompletion(t *testing.T) {
	est := testEstimator(t)
	msgs := []*schema.Message{
		{
			Role:    schema.Assistant,
			Content: "早",
			ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
				PromptTokens:     40,
				CompletionTokens: 2,
				PromptTokenDetails: schema.PromptTokenDetails{
					CacheWriteTokens: 999,
				},
			}},
		},
		schema.UserMessage("下一句"),
	}
	got := est.CountPrompt(msgs)
	want := 42 + est.CountMessages(msgs[1:])
	if got != want {
		t.Fatalf("CountPrompt = %d，want %d", got, want)
	}
}

// TestCountPromptWithoutUsageCountsMessagesOnly 钉住：没有 Usage 时 CountPrompt 等于 CountMessages。
// 接口上没有工具说明参数，所以整份工具说明不会被加进去。
func TestCountPromptWithoutUsageCountsMessagesOnly(t *testing.T) {
	est := testEstimator(t)
	msgs := []*schema.Message{
		schema.UserMessage("只有消息"),
		schema.AssistantMessage("没有 Usage", nil),
	}
	if got, want := est.CountPrompt(msgs), est.CountMessages(msgs); got != want {
		t.Fatalf("CountPrompt = %d，CountMessages = %d", got, want)
	}
}

// TestCountMessagesIgnoresStaleUsage 钉住：拼出来的新消息即使带着旧 Usage，CountMessages 也不拿它当基数。
func TestCountMessagesIgnoresStaleUsage(t *testing.T) {
	est := testEstimator(t)
	with := []*schema.Message{{
		Role:         schema.Assistant,
		Content:      "正文",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 50000}},
	}}
	without := []*schema.Message{schema.AssistantMessage("正文", nil)}
	if est.CountMessages(with) != est.CountMessages(without) {
		t.Fatalf("带 Usage = %d，不带 = %d", est.CountMessages(with), est.CountMessages(without))
	}
	if est.CountPrompt(with) == est.CountMessages(with) {
		t.Fatal("CountPrompt 应使用 50000 这个基数")
	}
}

// TestReasoningAndImageAreCounted 钉住：推理正文要算进去；图片按张加固定值，不按 URL 长度算。
func TestReasoningAndImageAreCounted(t *testing.T) {
	est := testEstimator(t)
	plain := est.CountMessages([]*schema.Message{schema.AssistantMessage("答案", nil)})
	reasoning := est.CountMessages([]*schema.Message{{
		Role:             schema.Assistant,
		Content:          "答案",
		ReasoningContent: strings.Repeat("想", 80),
	}})
	if reasoning <= plain {
		t.Fatalf("推理正文 %d 应大于纯正文 %d", reasoning, plain)
	}
	image := est.CountMessages([]*schema.Message{{
		Role: schema.User,
		MultiContent: []schema.ChatMessagePart{{
			Type:     schema.ChatMessagePartTypeImageURL,
			ImageURL: &schema.ChatMessageImageURL{URL: "https://example.test/" + strings.Repeat("a", 2000)},
		}},
	}})
	empty := est.CountMessages([]*schema.Message{schema.UserMessage("")})
	if image != empty+estimatedImageTokens {
		t.Fatalf("图片 %d，空消息 %d，应正好多 %d", image, empty, estimatedImageTokens)
	}
}

// TestContentIsNotDoubleCounted 钉住：Content 已有正文时，多模态文本片段不再加一次。
func TestContentIsNotDoubleCounted(t *testing.T) {
	est := testEstimator(t)
	only := est.CountMessages([]*schema.Message{schema.UserMessage("同一段")})
	both := est.CountMessages([]*schema.Message{{
		Role:    schema.User,
		Content: "同一段",
		UserInputMultiContent: []schema.MessageInputPart{{
			Type: schema.ChatMessagePartTypeText,
			Text: "同一段",
		}},
	}})
	if only != both {
		t.Fatalf("只写 Content = %d，再带文本片段 = %d", only, both)
	}
}

// testEstimator 建真的 cl100k_base。初始化失败就停，不改用字节除以 4。
func testEstimator(t *testing.T) *Estimator {
	t.Helper()
	est, err := NewEstimator()
	if err != nil {
		t.Fatalf("NewEstimator: %v", err)
	}
	return est
}
