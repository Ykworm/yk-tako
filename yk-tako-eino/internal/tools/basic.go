package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type emptyArgs struct{}

type calcArgs struct {
	Expression string `json:"expression" jsonschema:"description=要计算的数学表达式，例如 1234*5678"`
}

// Basic 是 L0 的两个无害工具：时间和四则运算。
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
