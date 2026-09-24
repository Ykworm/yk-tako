// ② ReAct 图。对照 TypeScript 的 src/l0/02-langgraph.ts。
//
// while 不见了。react.NewAgent 把循环收成一张图：ChatModel 节点，有工具调用就走到 Tools，否则结束。
// 这张图的 state 活在单次 Generate 里。跨轮记忆仍由 REPL 把历史再传进去。
// checkpoint 和 interrupt 在 Eino 里是 ADK Runner 的能力，L1 才用上。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	logged := logTools(tools.MustBasic(ctx))
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chatModel,
		ToolsConfig:      compose.ToolsNodeConfig{Tools: logged},
		MessageModifier: func(_ context.Context, input []*schema.Message) []*schema.Message {
			out := make([]*schema.Message, 0, len(input)+1)
			out = append(out, schema.SystemMessage(config.SystemPrompt()))
			return append(out, input...)
		},
		MaxStep: 40,
	})
	if err != nil {
		exitf("创建 ReAct agent: %v", err)
	}

	console := repl.New()
	var history []*schema.Message
	fmt.Printf("② Eino ReAct agent。模型 %s。输入 quit 退出。\n\n", ref)
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		history = append(history, schema.UserMessage(input))
		out, err := agent.Generate(ctx, history)
		if err != nil {
			fmt.Println("出错了:", err)
			history = history[:len(history)-1]
			continue
		}
		history = append(history, out)
		if out.Content != "" {
			fmt.Printf("\n🐙 %s\n\n", out.Content)
		}
	}
}

type loggingTool struct {
	tool.InvokableTool
}

func (t loggingTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	name := "tool"
	if info, err := t.Info(ctx); err == nil && info != nil {
		name = info.Name
	}
	fmt.Printf("  → 🔧 %s(%s)\n", name, args)
	out, err := t.InvokableTool.InvokableRun(ctx, args, opts...)
	if err != nil {
		fmt.Printf("  ← %s\n", err)
		return "", err
	}
	fmt.Printf("  ← %s\n", clip(out, 100))
	return out, nil
}

func logTools(items []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(items))
	for _, item := range items {
		inv, ok := item.(tool.InvokableTool)
		if !ok {
			exitf("工具不能被同步调用")
		}
		out = append(out, loggingTool{InvokableTool: inv})
	}
	return out
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
