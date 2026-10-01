// 本文件：read_file / write_file 的行为测试。read_file 是 enhanced 工具，
// 回执在 schema.ToolResult 的片段里，所以这里直接调 InvokableRun 验片段形状。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// pngSignature 是 PNG 的字节头。DetectContentType 只看头几个字节，不必是真图。
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// reader 取出 read_file 那个工具，按 enhanced 接口调它。
func reader(t *testing.T) tool.EnhancedInvokableTool {
	t.Helper()
	all, err := Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	for _, item := range all {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.Name != "read_file" {
			continue
		}
		enhanced, ok := item.(tool.EnhancedInvokableTool)
		if !ok {
			t.Fatal("read_file 不是 enhanced 工具，图片就回不到模型")
		}
		return enhanced
	}
	t.Fatal("没有找到 read_file")
	return nil
}

// read 调一次 read_file。
func read(t *testing.T, path string) *schema.ToolResult {
	t.Helper()
	result, err := reader(t).InvokableRun(context.Background(), &schema.ToolArgument{Text: `{"path":` + strconv.Quote(path) + `}`})
	if err != nil {
		t.Fatalf("read_file 出错: %v", err)
	}
	return result
}

// textOf 拼出回执里的文本片段。
func textOf(result *schema.ToolResult) string {
	var text string
	for _, part := range result.Parts {
		if part.Type == schema.ToolPartTypeText {
			text += part.Text
		}
	}
	return text
}

// imageOf 取出回执里的图像片段，没有就返回 nil。
func imageOf(result *schema.ToolResult) *schema.ToolOutputImage {
	for _, part := range result.Parts {
		if part.Type == schema.ToolPartTypeImage {
			return part.Image
		}
	}
	return nil
}

// TestReadFileImage 是图片这条路：一句说明加图本身，字节原样、MIME 认对。
func TestReadFileImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.png")
	raw := append(append([]byte{}, pngSignature...), bytes.Repeat([]byte{0x42}, 2048)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	result := read(t, path)
	image := imageOf(result)
	if image == nil || image.Base64Data == nil {
		t.Fatalf("回执里没有图像片段: %v", result.Parts)
	}
	got, err := base64.StdEncoding.DecodeString(*image.Base64Data)
	if err != nil {
		t.Fatalf("图像片段的 base64 解不开: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("回给模型的字节和文件里的不一致")
	}
	if image.MIMEType != "image/png" {
		t.Fatalf("MIME 是 %q，想要 image/png", image.MIMEType)
	}
	for _, want := range []string{path, "image/png", "2056 字节"} {
		if !strings.Contains(textOf(result), want) {
			t.Fatalf("说明文字 %q 里没有 %q", textOf(result), want)
		}
	}
}

// TestReadFileText 是文本这条路：行为和改成 enhanced 之前一致。
func TestReadFileText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("第一行\n第二行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := read(t, path)
	if imageOf(result) != nil {
		t.Fatal("文本文件不该回图像片段")
	}
	if got := textOf(result); got != "第一行\n第二行\n" {
		t.Fatalf("正文是 %q", got)
	}
}

// TestReadFileClipsOnRuneBoundary 是截断不能劈开一个汉字：劈开了就是非法 UTF-8，
// 模型那边看到乱码。
func TestReadFileClipsOnRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("中文", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	text := textOf(read(t, path))
	if !strings.Contains(text, "截断") {
		t.Fatalf("超长文本该截断: %q", text[len(text)-40:])
	}
	body := text[:strings.Index(text, "\n...(截断")]
	if !utf8.ValidString(body) {
		t.Fatal("截断切口落在汉字中间，得到非法 UTF-8")
	}
	if len(body) > maxTextBytes {
		t.Fatalf("截断后还有 %d 字节，上限是 %d", len(body), maxTextBytes)
	}
}

// TestReadFileMissing 是读不到文件：错误当正文回，不让整轮失败。
func TestReadFileMissing(t *testing.T) {
	text := textOf(read(t, filepath.Join(t.TempDir(), "nope.txt")))
	if !strings.HasPrefix(text, "读取失败: ") {
		t.Fatalf("回执是 %q", text)
	}
}

// TestReadFileRejectsHugeImage 是超大图片不读进来：base64 还要再涨三分之一。
func TestReadFileRejectsHugeImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.png")
	raw := append(append([]byte{}, pngSignature...), make([]byte, maxImageBytes)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	result := read(t, path)
	if imageOf(result) != nil {
		t.Fatal("超大图片不该回图像片段")
	}
	if !strings.Contains(textOf(result), "上限") {
		t.Fatalf("该说清为什么没读进来: %q", textOf(result))
	}
}

// TestWriteFileUnchanged 是 write_file 仍是普通字符串工具。
func TestWriteFileUnchanged(t *testing.T) {
	all, err := Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var writer tool.InvokableTool
	for _, item := range all {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.Name == "write_file" {
			writer, _ = item.(tool.InvokableTool)
		}
	}
	if writer == nil {
		t.Fatal("没有找到 write_file，或者它不再是字符串工具")
	}
	path := filepath.Join(t.TempDir(), "out.txt")
	out, err := writer.InvokableRun(context.Background(), `{"path":"`+path+`","content":"你好"}`)
	if err != nil || !strings.HasPrefix(out, "已写入 ") {
		t.Fatalf("write_file 回执 %q, err=%v", out, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "你好" {
		t.Fatalf("文件内容不对: %q, err=%v", raw, err)
	}
}
