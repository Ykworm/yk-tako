// L1 工具安全层。对照 TypeScript 的 src/l1/01-tool-guard.ts。
//
// 挂载点换成了 ADK 的 ChatModelAgentMiddleware：WrapInvokableToolCall 包住工具执行。
// 审批用 compose.StatefulInterrupt 把图暂停，Runner 配上内存 CheckPointStore，
// 人回答后再 ResumeWithParams。resume 值用 "approve" / "reject"，不用布尔。
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
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	cwd, err := os.Getwd()
	if err != nil {
		exitf("%v", err)
	}
	bash, err := tools.Bash(ctx)
	if err != nil {
		exitf("bash: %v", err)
	}
	files, err := tools.Files(ctx)
	if err != nil {
		exitf("files: %v", err)
	}
	all := append(tools.MustBasic(ctx), bash)
	all = append(all, files...)

	engine := guard.NewEngine(guard.ModeApprove, nil)
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "yk-tako",
		Description: "带工具安全层的工作伙伴",
		Instruction: config.SystemPrompt(),
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: all},
		},
		Handlers:      guard.Handlers(engine, guard.Options{WorkspaceRoot: cwd}),
		MaxIterations: 20,
	})
	if err != nil {
		exitf("创建 agent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
		CheckPointStore: adkrun.NewStore(),
	})

	console := repl.New()
	var history []*schema.Message
	turn := 0
	fmt.Printf("L1 工具安全层演示。模型 %s。\n", ref)
	fmt.Printf("ToolGuard 模式: %s（/mode 可切换）。输入 quit 退出。\n\n", engine.Mode())
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
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
		produced, err := adkrun.Run(ctx, runner, fmt.Sprintf("l1-%d", turn), "yk-tako", messages, func(info guard.ApprovalInfo) (string, error) {
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

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
