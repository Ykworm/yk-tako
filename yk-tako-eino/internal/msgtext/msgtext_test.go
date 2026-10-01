// 本文件：Of 的取值口径——Content 优先，为空才落到片段上。
package msgtext

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestOfPrefersContent 是 Content 有值时不碰片段，避免同一段话出现两遍。
func TestOfPrefersContent(t *testing.T) {
	msg := &schema.Message{
		Role:                  schema.User,
		Content:               "看这张图",
		UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: "看这张图"}},
	}
	if got := Of(msg); got != "看这张图" {
		t.Fatalf("Of=%q", got)
	}
}

// TestOfFallsBackToParts 是 enhanced 工具的回执形状：Content 为空，正文和图都在片段里
// （compose/tool_node.go:1234）。
func TestOfFallsBackToParts(t *testing.T) {
	uri := "data:image/png;base64,AAAA"
	msg := &schema.Message{
		Role:       schema.Tool,
		ToolCallID: "call-1",
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "图片 a.png（image/png，4 字节）"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &uri, MIMEType: "image/png"},
			}},
		},
	}
	got := Of(msg)
	if !strings.Contains(got, "图片 a.png") || !strings.Contains(got, "[图片]") {
		t.Fatalf("Of=%q，想要说明文字加图片标记", got)
	}
}

// TestOfEmpty 是空消息与纯文字片段之外的形状都不该炸。
func TestOfEmpty(t *testing.T) {
	if got := Of(nil); got != "" {
		t.Fatalf("nil 消息应返回空串，实到 %q", got)
	}
	if got := Of(&schema.Message{Role: schema.User}); got != "" {
		t.Fatalf("空消息应返回空串，实到 %q", got)
	}
	audio := &schema.Message{UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeAudioURL, Audio: &schema.MessageInputAudio{}},
	}}
	if got := Of(audio); got != "[音频]" {
		t.Fatalf("音频片段应留 [音频]，实到 %q", got)
	}
	file := &schema.Message{UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeFileURL, File: &schema.MessageInputFile{Name: "a.pdf"}},
	}}
	if got := Of(file); got != "[文件 a.pdf]" {
		t.Fatalf("文件片段应带文件名，实到 %q", got)
	}
}
