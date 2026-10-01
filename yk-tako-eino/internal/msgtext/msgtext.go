// Package msgtext 取一条消息的正文。
//
// 为什么要这一层：eino 的 enhanced 工具（components/tool/interface.go:67）把结果放进
// UserInputMultiContent，消息的 Content 是空串（compose/tool_node.go:1234）。呈现
// （internal/adkrun）和压缩（internal/compact）都按 Content 取正文，遇到多模态工具
// 结果就会取到空——界面上工具回执变空白，摘要里也少掉这段。
package msgtext

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Of 返回消息的正文：Content 有就用它，没有就把片段里的文字拼起来。
// 非文字片段按类别留一个方括号标记，界面上看得出「这里有一张图」，
// 摘要里也不会把图片当成什么都没有。
func Of(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Content != "" {
		return msg.Content
	}
	if len(msg.UserInputMultiContent) == 0 {
		return ""
	}
	var b strings.Builder
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText || part.Text != "" {
			b.WriteString(part.Text)
			b.WriteByte('\n')
			continue
		}
		if label := mediaLabel(part); label != "" {
			b.WriteString(label)
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// mediaLabel 给非文字片段一个短标记。认不出来的片段不留字。
func mediaLabel(part schema.MessageInputPart) string {
	switch {
	case part.Image != nil:
		return "[图片]"
	case part.Audio != nil:
		return "[音频]"
	case part.Video != nil:
		return "[视频]"
	case part.File != nil:
		if part.File.Name == "" {
			return "[文件]"
		}
		return "[文件 " + part.File.Name + "]"
	default:
		return ""
	}
}
