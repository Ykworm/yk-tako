// 本文件：消息的深拷贝、轮次切分，以及送给摘要模型的文本。
// 拷贝是为了让后面的 piiHandler 改 Content 时碰不到 modelMsgs 里的对象。
package compact

import (
	"fmt"
	"maps"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// extraCompact 标出压缩自己放进提示里的消息。键只留在进程内，OpenAI 请求不读 Extra。
const extraCompact = "yk_compact"

// turn 是一轮已经结束的对话：从一条用户消息到下一条用户消息之前。
type turn struct {
	id   string
	msgs []*schema.Message
}

// CloneMessages 复制一批消息。Usage 保留，给下一轮 CountPrompt 当基数。
// 切片和 Extra 是新的，Content 是字符串，改副本不会写回原来的对象。
func CloneMessages(msgs []*schema.Message) []*schema.Message {
	return deepCopy(msgs, false)
}

// deepCopy 复制消息。stripUsage 为真时清掉 Usage，避免旧基数把已经拿掉的轮次继续算进去。
func deepCopy(msgs []*schema.Message, stripUsage bool) []*schema.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]*schema.Message, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, cloneMessage(msg, stripUsage))
	}
	return out
}

// cloneMessage 复制一条消息。nil 保持 nil，调用方的洞不要被填成空消息。
func cloneMessage(src *schema.Message, stripUsage bool) *schema.Message {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Extra != nil {
		dst.Extra = maps.Clone(src.Extra)
	}
	if len(src.ToolCalls) > 0 {
		dst.ToolCalls = append([]schema.ToolCall(nil), src.ToolCalls...)
		for i := range dst.ToolCalls {
			if src.ToolCalls[i].Extra != nil {
				dst.ToolCalls[i].Extra = maps.Clone(src.ToolCalls[i].Extra)
			}
			if src.ToolCalls[i].Index != nil {
				idx := *src.ToolCalls[i].Index
				dst.ToolCalls[i].Index = &idx
			}
		}
	}
	if src.ResponseMeta != nil {
		meta := *src.ResponseMeta
		if stripUsage {
			meta.Usage = nil
		} else if src.ResponseMeta.Usage != nil {
			usage := *src.ResponseMeta.Usage
			meta.Usage = &usage
		}
		dst.ResponseMeta = &meta
	}
	if src.MultiContent != nil {
		dst.MultiContent = append([]schema.ChatMessagePart(nil), src.MultiContent...)
	}
	if src.UserInputMultiContent != nil {
		dst.UserInputMultiContent = append([]schema.MessageInputPart(nil), src.UserInputMultiContent...)
	}
	if src.AssistantGenMultiContent != nil {
		dst.AssistantGenMultiContent = append([]schema.MessageOutputPart(nil), src.AssistantGenMultiContent...)
	}
	return &dst
}

// mark 给压缩自己造的用户消息打上 summary 或 rag。这条消息不作为一轮的起点。
func mark(msg *schema.Message, kind string) *schema.Message {
	if msg == nil {
		return nil
	}
	if msg.Extra == nil {
		msg.Extra = map[string]any{}
	}
	msg.Extra[extraCompact] = kind
	return msg
}

// isSynthetic 判断这条用户消息是不是压缩放进去的摘要或检索结果。
func isSynthetic(msg *schema.Message) bool {
	if msg == nil || msg.Extra == nil {
		return false
	}
	kind, ok := msg.Extra[extraCompact].(string)
	return ok && (kind == "summary" || kind == "rag")
}

// split 把消息切成已经结束的轮，和还没结束的这一轮。
// 一条没有压缩标记的用户消息开始一轮。最后一轮是未完成的。
// runIDs 与已结束轮对齐；缺 id 时用 idx:序号，序号是已结束轮里的下标，后面 N 减小也不会变。
func split(msgs []*schema.Message, runIDs []string) (completed []turn, unfinished []*schema.Message) {
	var groups [][]*schema.Message
	var cur []*schema.Message
	started := false
	for _, msg := range msgs {
		if msg != nil && msg.Role == schema.User && !isSynthetic(msg) {
			if started {
				groups = append(groups, cur)
				cur = []*schema.Message{msg}
				continue
			}
			cur = append(cur, msg)
			started = true
			continue
		}
		cur = append(cur, msg)
	}
	if !started {
		return nil, msgs
	}
	groups = append(groups, cur)
	unfinished = groups[len(groups)-1]
	for i, group := range groups[:len(groups)-1] {
		id := ""
		if i < len(runIDs) {
			id = runIDs[i]
		}
		if id == "" {
			id = fmt.Sprintf("idx:%d", i)
		}
		completed = append(completed, turn{id: id, msgs: group})
	}
	return completed, unfinished
}

// renderTurns 把若干轮写成摘要模型能读的文本。不送未完成的那一轮。
func renderTurns(turns []turn) string {
	var b strings.Builder
	for i, item := range turns {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderTurn(item))
	}
	return b.String()
}

// renderTurn 写出一轮里的正文和工具调用。检索存的就是这段文本。
func renderTurn(item turn) string {
	var b strings.Builder
	for _, msg := range item.msgs {
		writeMsg(&b, msg)
	}
	return b.String()
}

// writeMsg 把一条消息收成一行角色加正文。工具调用单独成行，参数留在原文里。
func writeMsg(b *strings.Builder, msg *schema.Message) {
	if msg == nil {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", msg.Role, msg.Content)
	if msg.ReasoningContent != "" {
		fmt.Fprintf(b, "reasoning: %s\n", msg.ReasoningContent)
	}
	for _, call := range msg.ToolCalls {
		fmt.Fprintf(b, "tool_call %s: %s\n", call.Function.Name, call.Function.Arguments)
	}
	if msg.Role == schema.Tool && msg.ToolName != "" {
		fmt.Fprintf(b, "tool_result %s\n", msg.ToolName)
	}
}
