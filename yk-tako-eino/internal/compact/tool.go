// 本文件：主模型事后再查旧对话的工具。压缩循环里的那一次检索不走这里。
package compact

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// searchArgs 是 search_archive 的参数。
type searchArgs struct {
	Query string `json:"query" jsonschema:"description=要在已经压缩掉的旧对话里查找的词，用这一次问题里的关键词"`
}

// newSearchTool 生成 search_archive。
// 工具由 utils.InferTool 生成，最终实现 tool.InvokableTool
// （github.com/cloudwego/eino/components/tool 包 interface.go:42，eino v0.9.21）。
// 查的是构造时绑上的那一份 Memory，不从 ctx 再取，避免一场会话读到另一场的归档。
func newSearchTool(mem Memory) (tool.BaseTool, error) {
	return utils.InferTool("search_archive",
		"在已经压缩掉的旧对话里按词查找原文。查询用当前问题里的关键词，不要把整段工具结果塞进来。",
		func(_ context.Context, args searchArgs) (string, error) {
			if mem == nil {
				return "没有可检索的归档。", nil
			}
			hits, err := mem.SearchRAG(args.Query)
			if err != nil {
				return "检索失败。", nil
			}
			if strings.TrimSpace(hits) == "" {
				return "没有命中。", nil
			}
			return hits, nil
		})
}
