// 本文件：read_file / write_file 两个文件工具，供 L1 的文件守卫包住做路径检查。
package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

const (
	// maxTextBytes 是文本读回上下文的字节上限，超了就截断。
	maxTextBytes = 4000
	// maxImageBytes 是图片读回上下文的字节上限。base64 还要再涨三分之一，
	// 比这大的图不是「读文件」而是塞爆上下文；mediaoffload 也只在该管历史，
	// 不该让一张巨图先进来一次。
	maxImageBytes = 5 << 20
)

// readArgs 是 read_file 的参数。
type readArgs struct {
	Path string `json:"path" jsonschema:"description=文件路径，绝对路径或相对当前目录"`
}

// writeArgs 是 write_file 的参数。
type writeArgs struct {
	Path    string `json:"path" jsonschema:"description=文件路径"`
	Content string `json:"content" jsonschema:"description=要写入的完整内容"`
}

// Files 是 filesystem guard 的被保护对象。路径校验不在工具里。
// read_file 走 eino 的 enhanced 工具（components/tool/interface.go:67），因为图片要
// 以图像片段回给模型，字符串回执装不下；write_file 仍是普通字符串工具。
// 两者都实现工具接口（见包注释）。
func Files(ctx context.Context) ([]tool.BaseTool, error) {
	readFile, err := utils.InferEnhancedTool("read_file",
		"读取指定路径的文件内容（绝对路径或相对当前目录）。图片会把图本身给模型看，文本超过 4000 字节会截断。",
		func(_ context.Context, args readArgs) (*schema.ToolResult, error) {
			data, err := os.ReadFile(args.Path)
			if err != nil {
				return textResult("读取失败: " + err.Error()), nil
			}
			if mime, ok := imageMIME(data); ok {
				if len(data) > maxImageBytes {
					return textResult(fmt.Sprintf("%s 是 %s 图片，%d 字节，超过 %d 字节的上限，没有读进来。",
						args.Path, mime, len(data), maxImageBytes)), nil
				}
				return imageResult(args.Path, mime, data), nil
			}
			return textResult(clipText(string(data))), nil
		})
	if err != nil {
		return nil, err
	}
	writeFile, err := utils.InferTool("write_file", "把内容写入指定路径的文件（会覆盖已有文件）。",
		func(_ context.Context, args writeArgs) (string, error) {
			if err := os.WriteFile(args.Path, []byte(args.Content), 0o644); err != nil {
				return "写入失败: " + err.Error(), nil
			}
			return fmt.Sprintf("已写入 %s（%d 字符）", args.Path, len(args.Content)), nil
		})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{readFile, writeFile}, nil
}

// clipText 按字节上限截断，并把切口退到字符边界上——从中间劈开一个汉字会得到
// 非法 UTF-8，模型那边看到的是乱码。
func clipText(text string) string {
	if len(text) <= maxTextBytes {
		return text
	}
	cut := maxTextBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n...(截断，共 %d 字符)", text[:cut], len(text))
}

// imageMIME 按内容认图片，不看扩展名：扩展名会说谎，字节头不会。
// 只认 image/*，音频和视频不走 read_file。
func imageMIME(data []byte) (string, bool) {
	mime := http.DetectContentType(data)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return mime, strings.HasPrefix(mime, "image/")
}

// textResult 是只有文字的回执。
func textResult(text string) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: text}}}
}

// imageResult 是「一句说明 + 图本身」的回执。说明里带路径和大小，
// 这样这一轮之后 mediaoffload 把图换成占位符时，模型还记得自己读过什么。
func imageResult(path, mime string, data []byte) *schema.ToolResult {
	b64 := base64.StdEncoding.EncodeToString(data)
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{
		{Type: schema.ToolPartTypeText, Text: fmt.Sprintf("图片 %s（%s，%d 字节）", path, mime, len(data))},
		{
			Type: schema.ToolPartTypeImage,
			Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &b64,
				MIMEType:   mime,
			}},
		},
	}}
}
