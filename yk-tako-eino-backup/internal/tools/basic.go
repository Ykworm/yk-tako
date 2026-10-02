// Package tools 是给 agent 用的基础工具集：时间、四则运算、bash、文件读写。
//
// 工具都由 utils.InferTool（github.com/cloudwego/eino/components/tool/utils）生成，
// 产物实现 tool.InvokableTool 接口（github.com/cloudwego/eino/components/tool 包
// interface.go:42，eino v0.9.21），对外用更宽的 tool.BaseTool（同文件 :32）承载。
// 路径/命令的安全检查不在工具里，在 internal/guard 层。
package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// emptyArgs 是无参工具的占位参数。
type emptyArgs struct{}

// calcArgs 是 calculate 的参数。
type calcArgs struct {
	Expression string `json:"expression" jsonschema:"description=要计算的数学表达式，例如 1234*5678"`
}

// Basic 是 L0 的两个无害工具：时间和四则运算。
// 两个工具经 utils.InferTool 生成，实现工具接口（见包注释）。
func Basic(ctx context.Context) ([]tool.BaseTool, error) {
	clock, err := utils.InferTool("get_current_time", "获取当前的日期和时间（东八区）",
		func(context.Context, emptyArgs) (string, error) {
			loc, err := time.LoadLocation("Asia/Shanghai")
			if err != nil {
				loc = time.FixedZone("CST", 8*3600)
			}
			return time.Now().In(loc).Format("2006-01-02 15:04:05"), nil
		})
	if err != nil {
		return nil, err
	}
	calc, err := utils.InferTool("calculate", "计算数学表达式的值，例如 '1234*5678' 或 '(100+200)/3'",
		func(_ context.Context, args calcArgs) (string, error) {
			value, err := Eval(args.Expression)
			if err != nil {
				return "错误：" + err.Error(), nil
			}
			return value, nil
		})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{clock, calc}, nil
}

// MustBasic 在 main 里用。
func MustBasic(ctx context.Context) []tool.BaseTool {
	tools, err := Basic(ctx)
	if err != nil {
		panic(fmt.Errorf("创建基础工具: %w", err))
	}
	return tools
}
