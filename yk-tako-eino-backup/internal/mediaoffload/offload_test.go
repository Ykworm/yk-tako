// 本文件：Rewrite 的规则测试——上一轮换占位符、本轮第一次留原图、换轮重新可见。
package mediaoffload

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// png 是一段够大的假图片字节。内容不必是真 PNG，落盘只管字节。
var png = bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}, 1024)

// imageMsg 造一条带一张内联图的用户消息。
func imageMsg(text string, raw []byte, mime string) *schema.Message {
	b64 := base64.StdEncoding.EncodeToString(raw)
	return &schema.Message{
		Role:    schema.User,
		Content: text,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: text},
			{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: mime},
				},
			},
		},
	}
}

// toolImageMsg 造一条工具结果消息，形状和 eino 把 ToolResult 转成的消息一致
// （compose/tool_node.go:1235：Content 为空，片段在 UserInputMultiContent）。
func toolImageMsg(raw []byte) *schema.Message {
	b64 := base64.StdEncoding.EncodeToString(raw)
	return &schema.Message{
		Role:       schema.Tool,
		ToolCallID: "call-1",
		UserInputMultiContent: []schema.MessageInputPart{
			{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: "image/png"},
				},
			},
		},
	}
}

// newTestSession 在 t.TempDir() 下建一个 Session。
func newTestSession(t *testing.T, minBytes int) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	return NewSession(Config{Store: NewStore(filepath.Join(dir, DefaultDir), 0), MinBytes: minBytes}), dir
}

// firstPart 取出消息的第一个片段，断言它是文本并返回正文。
func firstText(t *testing.T, msg *schema.Message) string {
	t.Helper()
	if len(msg.UserInputMultiContent) == 0 {
		t.Fatal("消息里没有片段")
	}
	part := msg.UserInputMultiContent[0]
	if part.Type != schema.ChatMessagePartTypeText {
		t.Fatalf("第一个片段是 %s，应该是 text", part.Type)
	}
	return part.Text
}

// hasImage 报告消息里还有没有图像片段。
func hasImage(msg *schema.Message) bool {
	for _, part := range msg.UserInputMultiContent {
		if part.Image != nil {
			return true
		}
	}
	return false
}

// TestRewriteKeepsFirstSightThenOffloads 是核心那一条：本轮第一次留原图，第二次换占位符。
func TestRewriteKeepsFirstSightThenOffloads(t *testing.T) {
	session, dir := newTestSession(t, 0)
	msgs := []*schema.Message{schema.UserMessage("看这张图"), toolImageMsg(png)}

	out, changed := session.Rewrite(msgs)
	if changed {
		t.Fatal("本轮第一次看到图，不该改消息")
	}
	if !hasImage(out[1]) {
		t.Fatal("本轮第一次看到图，字节应该还在")
	}

	// 同一轮里再进一次 BeforeModel：模型已经看过了，换成占位符。
	out, changed = session.Rewrite(out)
	if !changed {
		t.Fatal("本轮第二次应该换占位符")
	}
	if hasImage(out[1]) {
		t.Fatal("占位符换上去之后不该还留着图")
	}
	text := firstText(t, out[1])
	path := filepath.Join(dir, DefaultDir, SHA256(png)+".png")
	for _, want := range []string{"image 已落盘", "sha=" + SHA256(png)[:12], "path=" + path, fmt.Sprintf("size=%dB", len(png)), "mime=image/png", "read_file"} {
		if !strings.Contains(text, want) {
			t.Fatalf("占位符 %q 里没有 %q", text, want)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	if !bytes.Equal(raw, png) {
		t.Fatal("落盘的字节和原图不一致")
	}
}

// TestRewriteOffloadsEarlierTurn 是上一轮的媒体：一进来就是占位符，不再给模型看。
func TestRewriteOffloadsEarlierTurn(t *testing.T) {
	session, _ := newTestSession(t, 0)
	// 第一轮：图留在消息里。
	first := []*schema.Message{schema.UserMessage("看这张图"), toolImageMsg(png)}
	if _, changed := session.Rewrite(first); changed {
		t.Fatal("本轮第一次看到图，不该改消息")
	}
	// 第二轮：上一轮那条工具结果只该剩占位符。
	second := append(append([]*schema.Message{}, first...), schema.UserMessage("再看一眼"), toolImageMsg(png))
	out, changed := session.Rewrite(second)
	if !changed {
		t.Fatal("上一轮的图应该换占位符")
	}
	if hasImage(out[1]) {
		t.Fatal("上一轮的图还留着")
	}
	if !hasImage(out[3]) {
		t.Fatal("本轮刚读回来的图应该还留着")
	}
}

// TestRewriteSameImageTwiceInTurn 是同一轮里两次读到同一张图：只有第一处留字节，
// 后面那处立刻换占位符——同一次请求里重复的字节模型看不出区别，白占上下文。
func TestRewriteSameImageTwiceInTurn(t *testing.T) {
	session, _ := newTestSession(t, 0)
	msgs := []*schema.Message{schema.UserMessage("看这张图"), toolImageMsg(png), toolImageMsg(png)}
	out, changed := session.Rewrite(msgs)
	if !changed {
		t.Fatal("同一轮里重复的那张图该换占位符")
	}
	if !hasImage(out[1]) {
		t.Fatal("第一处该留着字节")
	}
	if hasImage(out[2]) {
		t.Fatal("第二处该是占位符")
	}
	out, changed = session.Rewrite(out)
	if !changed {
		t.Fatal("同一轮第二遍第一处也该换占位符")
	}
	if hasImage(out[1]) {
		t.Fatal("同一轮第二遍不该还留着图")
	}
}

// TestRewriteLeavesOriginalsUntouched 保证改写不写穿调用方那批消息：
// 会话内存里的正本是原始字节，事件库回放的画面不能变成占位符。
func TestRewriteLeavesOriginalsUntouched(t *testing.T) {
	session, _ := newTestSession(t, 0)
	original := toolImageMsg(png)
	msgs := []*schema.Message{schema.UserMessage("看这张图"), original}
	if _, changed := session.Rewrite(msgs); changed {
		t.Fatal("本轮第一次看到图，不该改消息")
	}
	out, changed := session.Rewrite(msgs)
	if !changed {
		t.Fatal("本轮第二次应该换占位符")
	}
	if !hasImage(original) {
		t.Fatal("正本消息被就地改了")
	}
	if hasImage(out[1]) {
		t.Fatal("返回的消息里还留着图")
	}
	if out[0] != msgs[0] {
		t.Fatal("没改动的消息应该还是同一个对象")
	}
}

// TestRewriteSkipsSmallMedia 是门槛：小图换占位符不划算，原样留着。
func TestRewriteSkipsSmallMedia(t *testing.T) {
	session, _ := newTestSession(t, DefaultMinBytes)
	small := bytes.Repeat([]byte{1}, DefaultMinBytes-1)
	msgs := []*schema.Message{schema.UserMessage("看图"), toolImageMsg(small)}
	for i := 0; i < 2; i++ {
		out, changed := session.Rewrite(msgs)
		if changed {
			t.Fatalf("第 %d 遍：小于门槛的图不该动", i+1)
		}
		if !hasImage(out[1]) {
			t.Fatalf("第 %d 遍：小于门槛的图被换掉了", i+1)
		}
	}
}

// TestRewriteHandlesDataURI 是另一种内联形状：base64 塞在 data: URI 里。
func TestRewriteHandlesDataURI(t *testing.T) {
	session, dir := newTestSession(t, 0)
	uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(png)
	msg := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &uri},
			},
		}},
	}
	if _, changed := session.Rewrite([]*schema.Message{msg}); changed {
		t.Fatal("本轮第一次看到图，不该改消息")
	}
	out, changed := session.Rewrite([]*schema.Message{msg})
	if !changed {
		t.Fatal("data: URI 也该换占位符")
	}
	text := firstText(t, out[0])
	if !strings.Contains(text, "path="+filepath.Join(dir, DefaultDir, SHA256(png)+".jpg")) {
		t.Fatalf("占位符里的路径不对: %q", text)
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultDir, SHA256(png)+".jpg")); err != nil {
		t.Fatalf("MIME 推出的扩展名没落盘: %v", err)
	}
}

// TestRewriteLeavesRemoteAndBroken 是三种不该动的片段：远端 URL、坏 base64、纯文本。
func TestRewriteLeavesRemoteAndBroken(t *testing.T) {
	session, _ := newTestSession(t, 0)
	remote := "https://example.test/a.png"
	broken := "not base64 at all!!"
	msgs := []*schema.Message{schema.UserMessage("看图"), {
		Role: schema.Tool,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "这是一段说明"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &remote},
			}},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &broken, MIMEType: "image/png"},
			}},
		},
	}}
	for i := 0; i < 2; i++ {
		if _, changed := session.Rewrite(msgs); changed {
			t.Fatalf("第 %d 遍：远端 URL 与坏 base64 都不该动", i+1)
		}
	}
}

// TestRewriteStoreFailureIsSoft 是落盘失败要软降级：字节留着，不报错。
func TestRewriteStoreFailureIsSoft(t *testing.T) {
	dir := t.TempDir()
	session := NewSession(Config{Store: NewStore(filepath.Join(dir, DefaultDir), len(png)-1)})
	msgs := []*schema.Message{schema.UserMessage("看图"), toolImageMsg(png)}
	for i := 0; i < 2; i++ {
		out, changed := session.Rewrite(msgs)
		if changed {
			t.Fatalf("第 %d 遍：落盘失败时不该改消息", i+1)
		}
		if !hasImage(out[1]) {
			t.Fatalf("第 %d 遍：落盘失败时字节该留着", i+1)
		}
	}
}

// TestRewriteNoMedia 是没有媒体时的快路径：原切片原样返回。
func TestRewriteNoMedia(t *testing.T) {
	session, _ := newTestSession(t, 0)
	msgs := []*schema.Message{schema.UserMessage("你好"), schema.AssistantMessage("在", nil)}
	out, changed := session.Rewrite(msgs)
	if changed {
		t.Fatal("没有媒体不该说改了")
	}
	if len(out) != len(msgs) || out[0] != msgs[0] || out[1] != msgs[1] {
		t.Fatal("应该原样返回原来那批消息")
	}
}

// TestParseDataURI 是 data: URI 的几种写法。
func TestParseDataURI(t *testing.T) {
	cases := []struct {
		uri      string
		wantB64  string
		wantMIME string
		wantOK   bool
	}{
		{uri: "data:image/png;base64,QUJD", wantB64: "QUJD", wantMIME: "image/png", wantOK: true},
		{uri: "data:image/png;charset=utf-8;base64,QUJD", wantB64: "QUJD", wantMIME: "image/png", wantOK: true},
		{uri: "data:;base64,QUJD", wantB64: "QUJD", wantMIME: "", wantOK: true},
		{uri: "data:image/png,QUJD", wantOK: false},
		{uri: "data:image/png;base64,", wantOK: false},
		{uri: "https://example.test/a.png", wantOK: false},
	}
	for _, item := range cases {
		b64, mime, ok := parseDataURI(item.uri)
		if ok != item.wantOK {
			t.Fatalf("%q 的 ok=%v，想要 %v", item.uri, ok, item.wantOK)
		}
		if !ok {
			continue
		}
		if b64 != item.wantB64 || mime != item.wantMIME {
			t.Fatalf("%q 解出 (%q,%q)，想要 (%q,%q)", item.uri, b64, mime, item.wantB64, item.wantMIME)
		}
	}
}

// TestExtForMIME 是扩展名映射与兜底。
func TestExtForMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":                ".png",
		"IMAGE/JPEG":               ".jpg",
		"audio/mpeg":               ".mp3",
		"application/pdf":          ".pdf",
		"application/octet-stream": ".bin",
		"":                         ".bin",
		"image/x-unknown":          ".bin",
	}
	for mime, want := range cases {
		if got := extForMIME(mime); got != want {
			t.Fatalf("extForMIME(%q)=%q，想要 %q", mime, got, want)
		}
	}
}

// TestNewSessionNeedsStore 是没有缓存目录时不装这一层。
func TestNewSessionNeedsStore(t *testing.T) {
	if session := NewSession(Config{}); session != nil {
		t.Fatal("没有 Store 应该返回 nil")
	}
	if handler := NewHandler(Config{}); handler != nil {
		t.Fatal("没有 Store 应该返回 nil Handler")
	}
}
