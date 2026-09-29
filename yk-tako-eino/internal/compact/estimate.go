// 本文件：按 WeKnora internal/agent/token/estimator.go 的口径数 token。
// 编码固定 cl100k_base。有上一轮 Usage 时只数那条助手消息之后的增量；
// 没有 Usage 时只数消息，不把工具说明加进去。
package compact

import (
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/tiktoken-go/tokenizer"
)

const (
	// perMessageOverhead 是一条消息除正文外的固定开销，和 WeKnora 的同名常量一致。
	perMessageOverhead = 3
	// perConversationTail 是整段消息末尾的固定开销。
	perConversationTail = 3
	// perToolCallOverhead 是一次工具调用除名字和参数外的固定开销。
	perToolCallOverhead = 4
	// estimatedImageTokens 是一张图按块计费时的估算。不按 URL 或 base64 的长度算。
	estimatedImageTokens = 1200
)

// Estimator 用 cl100k_base 数消息的 token。它是触发压缩前的估算，下一次 Usage 会把它纠正过来。
type Estimator struct {
	codec tokenizer.Codec
}

// NewEstimator 建一个 cl100k_base 计数器。编码数据在库里，不按模型再下载 tokenizer。
func NewEstimator() (*Estimator, error) {
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		return nil, fmt.Errorf("compact: 初始化 cl100k_base: %w", err)
	}
	return &Estimator{codec: codec}, nil
}

// CountPrompt 数将要发送的消息。
// 最后一条带 Usage 且 token 数大于 0 的助手消息是基数，这个数已经包含那次请求的系统指令、工具说明和它自己的回复。
// 只对它后面的消息做 BPE。没有这样的 Usage 时，只数消息本身。
func (e *Estimator) CountPrompt(msgs []*schema.Message) int {
	idx := -1
	baseline := 0
	for i, msg := range msgs {
		n := usageTokens(msg)
		if n > 0 {
			idx = i
			baseline = n
		}
	}
	if idx >= 0 {
		return baseline + e.estimateMessages(msgs[idx+1:])
	}
	return e.estimateMessages(msgs)
}

// CountMessages 忽略 Usage，只对这份消息做 BPE。
// 压缩刚拼出来的新消息还没发给主模型，消息上残留的 Usage 描述的是压缩前那一份，不能当基数。
func (e *Estimator) CountMessages(msgs []*schema.Message) int {
	return e.estimateMessages(msgs)
}

// estimateMessages 把每条消息的估算加起来，并加上整段末尾的固定开销。
func (e *Estimator) estimateMessages(msgs []*schema.Message) int {
	total := 0
	for _, msg := range msgs {
		total += e.estimateMessage(msg)
	}
	total += perConversationTail
	return total
}

// estimateMessage 数一条消息里会进请求的字段。推理正文也算，否则思考模型的上下文会被少算很多。
func (e *Estimator) estimateMessage(msg *schema.Message) int {
	if msg == nil {
		return 0
	}
	tokens := perMessageOverhead
	tokens += e.estimateString(string(msg.Role))
	tokens += e.estimateString(msg.Content)
	tokens += e.estimateString(msg.Name)
	tokens += e.estimateString(msg.ToolCallID)
	tokens += e.estimateString(msg.ReasoningContent)
	extraText, images := multiParts(msg)
	tokens += e.estimateString(extraText)
	tokens += images * estimatedImageTokens
	for _, call := range msg.ToolCalls {
		tokens += e.estimateString(call.Function.Name)
		tokens += e.estimateString(call.Function.Arguments)
		tokens += perToolCallOverhead
	}
	return tokens
}

// estimateString 用 BPE 数一段文字。编码失败时退回按字节除以 4，只作为这一段的兜底。
func (e *Estimator) estimateString(s string) int {
	if s == "" || e == nil || e.codec == nil {
		return 0
	}
	n, err := e.codec.Count(s)
	if err != nil {
		return (len(s) + 3) / 4
	}
	return n
}

// usageTokens 取出一条助手消息上的 Usage。TotalTokens 优先，否则用 PromptTokens 与 CompletionTokens 之和。
// 缓存读写的计数不另加，它们已经包含在 TotalTokens 里。
func usageTokens(msg *schema.Message) int {
	if msg == nil || msg.Role != schema.Assistant || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return 0
	}
	usage := msg.ResponseMeta.Usage
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.PromptTokens + usage.CompletionTokens
}

// multiParts 取出多模态片段里的文字和图片张数。
// Content 已经有正文时不再把文本片段加一次，避免同一段话数两遍。
func multiParts(msg *schema.Message) (text string, images int) {
	if len(msg.UserInputMultiContent) > 0 {
		return partsText(msg.Content, func(yield func(kind, text string)) {
			for _, part := range msg.UserInputMultiContent {
				kind := "text"
				if part.Type == schema.ChatMessagePartTypeImageURL || part.Image != nil {
					kind = "image"
				}
				yield(kind, part.Text)
			}
		})
	}
	if len(msg.MultiContent) > 0 {
		return partsText(msg.Content, func(yield func(kind, text string)) {
			for _, part := range msg.MultiContent {
				kind := "text"
				if part.Type == schema.ChatMessagePartTypeImageURL || part.ImageURL != nil {
					kind = "image"
				}
				yield(kind, part.Text)
			}
		})
	}
	if len(msg.AssistantGenMultiContent) > 0 {
		return partsText(msg.Content, func(yield func(kind, text string)) {
			for _, part := range msg.AssistantGenMultiContent {
				kind := "text"
				if part.Type == schema.ChatMessagePartTypeImageURL || part.Image != nil {
					kind = "image"
				}
				yield(kind, part.Text)
			}
		})
	}
	return "", 0
}

// partsText 把片段收成额外文字和图片张数。
func partsText(content string, walk func(yield func(kind, text string))) (string, int) {
	var text string
	images := 0
	walk(func(kind, partText string) {
		if kind == "image" {
			images++
			return
		}
		if content == "" {
			text += partText
		}
	})
	return text, images
}
