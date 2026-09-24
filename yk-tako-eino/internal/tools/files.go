package tools

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
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
func Files(ctx context.Context) ([]tool.BaseTool, error) {
	readFile, err := utils.InferTool("read_file", "读取指定路径的文件内容（绝对路径或相对当前目录）。",
		func(_ context.Context, args readArgs) (string, error) {
			content, err := os.ReadFile(args.Path)
			if err != nil {
				return "读取失败: " + err.Error(), nil
			}
			text := string(content)
			if len(text) > 4000 {
				return fmt.Sprintf("%s\n...(截断，共 %d 字符)", text[:4000], len(text)), nil
			}
			return text, nil
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
