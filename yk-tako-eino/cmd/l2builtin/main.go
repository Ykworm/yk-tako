// L2 对照 —— Eino DeepAgent 自带的 write_todos。对照 TypeScript 的 src/l2/02-builtin.ts。
//
// TypeScript 那边是 langchain 的 todoListMiddleware()。Eino 没有同名中间件，
// 官方实现在 deep.New 里：默认挂上 write_todos，Instruction 留空就用框架那篇长提示词
// （默认英文）。回执是 JSON，没有半途而废提醒，也没有 L1 守卫。
//
// 关掉通用子代理，不接文件后端，工具只留时间和计算。试同一句：
//
//	帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
package main

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

// main 建官方 DeepAgent 的 write_todos 对照版并进 REPL 循环。
func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	agent, err := deep.New(ctx, &deep.Config{
		Name:                   "yk-tako",
		Description:            "官方 write_todos 对照",
		ChatModel:              chatModel,
		ToolsConfig:            adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools.MustBasic(ctx)}},
		WithoutGeneralSubAgent: true,
		MaxIteration:           30,
	})
	if err != nil {
		repl.Exitf("创建 DeepAgent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
	})

	console := repl.New()
	var history []*schema.Message
	fmt.Printf("L2 任务规划演示（官方 write_todos）。模型 %s。输入 quit 退出。\n\n", ref)
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		turn := append(append([]*schema.Message{}, history...), schema.UserMessage(input))
		produced, err := adkrun.Run(ctx, runner, "", "yk-tako", turn, nil)
		if err != nil {
			fmt.Println("出错了:", err)
			continue
		}
		history = append(history, schema.UserMessage(input))
		history = append(history, produced...)
	}
}
