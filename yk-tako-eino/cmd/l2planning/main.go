// L2 任务规划。对照 TypeScript 的 src/l2/01-planning.ts。
//
// L1 三件套不动，planning 插在文件守卫之后、PII 之前。
// 清单不在图 state 里，/todos 读进程内的那份。
//
// 试试：
//
//	现在几点
//	帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
//	/todos
//	/mode block|warn|require_approval
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/planning"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		repl.Exitf("%v", err)
	}
	bash, err := tools.Bash(ctx)
	if err != nil {
		repl.Exitf("bash: %v", err)
	}
	files, err := tools.Files(ctx)
	if err != nil {
		repl.Exitf("files: %v", err)
	}
	all := append(tools.MustBasic(ctx), bash)
	all = append(all, files...)

	plan, err := planning.New(planning.Options{})
	if err != nil {
		repl.Exitf("planning: %v", err)
	}
	engine := guard.NewEngine(guard.ModeApprove, nil)
	guards := guard.Handlers(engine, guard.Options{WorkspaceRoot: cwd})
	// cap 截断，避免 append 写穿原来的 PII handler。
	handlers := append(guards[:2:2], plan.Handler, guards[2])

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "yk-tako",
		Description: "会列计划的工作伙伴",
		Instruction: config.SystemPrompt(),
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: all},
		},
		Middlewares:   []adk.AgentMiddleware{plan.Middleware},
		Handlers:      handlers,
		MaxIterations: 30,
	})
	if err != nil {
		repl.Exitf("创建 agent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
		CheckPointStore: adkrun.NewStore(),
	})

	console := repl.New()
	var history []*schema.Message
	turn := 0
	fmt.Printf("L2 任务规划演示（手写 planning 中间件）。模型 %s。\n", ref)
	fmt.Printf("ToolGuard 模式: %s。/todos 看清单，quit 退出。\n\n", engine.Mode())
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		if input == "/todos" {
			fmt.Printf("📋 当前任务清单:\n%s\n\n", repl.Indent(planning.FormatTodos(plan.List.Snapshot())))
			continue
		}
		if mode, ok := strings.CutPrefix(input, "/mode "); ok {
			switch guard.Mode(strings.TrimSpace(mode)) {
			case guard.ModeBlock, guard.ModeWarn, guard.ModeApprove:
				engine.Configure(guard.Mode(strings.TrimSpace(mode)))
				fmt.Printf("ToolGuard 模式已切换为: %s\n\n", engine.Mode())
			default:
				fmt.Println("用法: /mode block|warn|require_approval")
			}
			continue
		}
		turn++
		messages := append(append([]*schema.Message{}, history...), schema.UserMessage(input))
		produced, err := adkrun.Run(ctx, runner, fmt.Sprintf("l2-%d", turn), "yk-tako", messages, func(info guard.ApprovalInfo) (string, error) {
			fmt.Println("\n⏸️  ═══════════════ 人工审批（interrupt）═══════════════")
			fmt.Printf("工具: %s   参数: %s\n", info.Tool, info.Args)
			if info.Message != "" {
				fmt.Println(info.Message)
			}
			answer, ok := console.ReadLine("批准执行？[y/N] ")
			if !ok {
				return guard.ResumeReject, nil
			}
			answer = strings.ToLower(answer)
			if answer == "y" || answer == "yes" {
				fmt.Println("✅ 已批准，继续执行")
				fmt.Println()
				return guard.ResumeApprove, nil
			}
			fmt.Println("🚫 已拒绝")
			fmt.Println()
			return guard.ResumeReject, nil
		})
		if err != nil {
			fmt.Println("出错了:", err)
			continue
		}
		history = append(history, schema.UserMessage(input))
		history = append(history, produced...)
	}
}
