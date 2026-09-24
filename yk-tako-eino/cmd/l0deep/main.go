// ③ DeepAgent。对照 TypeScript 的 src/l0/03-deepagents.ts。
//
// 代码和「一个会调工具的 agent」差不多，但 deep.New 在 ChatModelAgent 上又叠了三样：
// write_todos（任务规划）、文件工具（下面的 Backend）、以及一个通用子代理。
// 可以试：帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/fsbackend"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

// main 建 DeepAgent（自带 write_todos、文件工具和通用子代理）并进 REPL 循环。
func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		repl.Exitf("%v", err)
	}
	backend, err := fsbackend.New(cwd)
	if err != nil {
		repl.Exitf("文件后端: %v", err)
	}
	agent, err := deep.New(ctx, &deep.Config{
		Name:        "yk-tako",
		Description: "正在成长中的 AI 工作伙伴",
		ChatModel:   chatModel,
		Instruction: config.SystemPrompt() + "\n多步任务先用 write_todos 列出计划再执行。写文件时使用绝对路径。",
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools.MustBasic(ctx)},
		},
		Backend:      backend,
		MaxIteration: 30,
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
	fmt.Printf("③ Eino DeepAgent。模型 %s。输入 quit 退出。\n\n", ref)
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
