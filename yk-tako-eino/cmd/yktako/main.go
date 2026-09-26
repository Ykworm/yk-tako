// yk-tako 唯一入口：裸 ReAct + 任务看板（taskboard）+ L1 三件套守卫。
//
// 组装（doc/01 §5）：ChatModelAgent 的 Handlers 链 = [taskboard] + guard 三件套；
// 看板正本在 taskboard middleware 里跨轮存活，镜像随 interrupt 落 checkpoint。
// 两个入口：默认 REPL；-web 起最小 Web UI（internal/webui），共用同一套审批语义。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"yk-tako-eino/internal/adkrun"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/guard"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/taskboard"
	"yk-tako-eino/internal/tools"
	"yk-tako-eino/internal/webui"
)

// main 建模型、工具、看板与守卫，进 REPL 循环；-web 时改成起 Web 服务。
// 审批两个入口共用 interrupt/resume：REPL 读 stdin，Web 挂起等 POST（《10》§4.3）。
func main() {
	ctx := context.Background()
	webAddr := flag.String("web", "", "启动最小 Web UI 的监听地址（如 127.0.0.1:8765），留空进 REPL")
	flag.Parse()

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

	engine := guard.NewEngine(guard.ModeApprove, nil)

	// newAgent 造一套会话 agent。看板正本挂在 taskboard middleware 实例上，
	// 所以每个会话要造一份——共用会把不同会话的账本串在一起；模型与守卫引擎共享。
	newAgent := func(ctx context.Context) (adk.Agent, error) {
		boardMW, err := taskboard.New(taskboard.NewStore())
		if err != nil {
			return nil, fmt.Errorf("taskboard: %w", err)
		}
		handlers := append([]adk.ChatModelAgentMiddleware{boardMW}, guard.Handlers(engine, guard.Options{WorkspaceRoot: cwd})...)
		return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Name:        config.AgentName(),
			Description: "带任务看板与工具安全层的工作伙伴",
			Instruction: config.SystemPrompt() + "\n\n" + taskboard.Instruction,
			Model:       chatModel,
			ToolsConfig: adk.ToolsConfig{
				// all 是静态组装的 5 个基础工具；taskboard 的 4 个记账工具由
				// middleware 的 BeforeAgent 每轮动态注入 runCtx.Tools（运行时合计 9 个）。
				// ExecuteSequentially：同批工具调用串行（秩序偏好，不是正确性保证，见 doc/01 §4）。
				ToolsNodeConfig: compose.ToolsNodeConfig{Tools: all, ExecuteSequentially: true},
			},
			Handlers:      handlers,
			MaxIterations: 20,
		})
	}

	// Web 模式：单用户、无鉴权，只该绑本机地址（《10》§5 范围冻结）。
	if *webAddr != "" {
		srv := webui.New(ctx, webui.Config{NewAgent: newAgent})
		fmt.Printf("yk-tako Web UI: http://%s（Ctrl-C 退出）\n", *webAddr)
		if err := http.ListenAndServe(*webAddr, srv); err != nil {
			repl.Exitf("web: %v", err)
		}
		return
	}

	agent, err := newAgent(ctx)
	if err != nil {
		repl.Exitf("创建 agent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
		CheckPointStore: adkrun.NewStore(),
	})

	// 呈现归消费方：REPL sink 写 stdout；root 从 agent 自己取，与 adkrun.Run 同源。
	sink := newREPLSink(os.Stdout, agent.Name(ctx))

	console := repl.New()
	var history []*schema.Message
	turn := 0
	fmt.Printf("yk-tako。模型 %s。\n", ref)
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
		produced, err := adkrun.Run(ctx, runner, agent, fmt.Sprintf("turn-%d", turn), messages, sink, func(info guard.ApprovalInfo) (string, error) {
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
