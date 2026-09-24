// ① 裸写版。对照 TypeScript 的 src/l0/01-raw-loop.ts。
//
// 循环还是 for：把历史发给模型，没有 tool_calls 就结束，否则执行工具再问一次。
// Eino 在这里只提供 ChatModel 和工具 schema，不接管循环。
package main

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

// main 建模型和工具后进 REPL 循环，每次输入交给 chat 跑完整轮。
func main() {
	ctx := context.Background()
	chatModel, ref := config.MustOpen(ctx)
	baseTools := tools.MustBasic(ctx)
	infos := make([]*schema.ToolInfo, 0, len(baseTools))
	byName := map[string]tool.InvokableTool{}
	for _, item := range baseTools {
		inv, ok := item.(tool.InvokableTool)
		if !ok {
			repl.Exitf("工具不能被同步调用")
		}
		info, err := inv.Info(ctx)
		if err != nil {
			repl.Exitf("%v", err)
		}
		infos = append(infos, info)
		byName[info.Name] = inv
	}
	llm, err := chatModel.WithTools(infos)
	if err != nil {
		repl.Exitf("%v", err)
	}

	console := repl.New()
	history := []*schema.Message{schema.SystemMessage(config.SystemPrompt())}
	fmt.Printf("① 裸写版 agent（手写 for 循环）。模型 %s。输入 quit 退出。\n\n", ref)
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		next, err := chat(ctx, llm, byName, history, input)
		history = next
		if err != nil {
			fmt.Println("出错了:", err)
		}
	}
}

// chat 就是手写的那层循环：发历史给模型，有 tool_calls 就执行工具再问一次。
func chat(ctx context.Context, llm model.ToolCallingChatModel, byName map[string]tool.InvokableTool, history []*schema.Message, input string) ([]*schema.Message, error) {
	history = append(history, schema.UserMessage(input))
	for {
		ai, err := llm.Generate(ctx, history)
		if err != nil {
			return history, err
		}
		history = append(history, ai)
		if len(ai.ToolCalls) == 0 {
			fmt.Printf("\n🐙 %s\n\n", ai.Content)
			return history, nil
		}
		for _, call := range ai.ToolCalls {
			fmt.Printf("  → 🔧 %s(%s)\n", call.Function.Name, call.Function.Arguments)
			result := runTool(ctx, byName[call.Function.Name], call.Function.Name, call.Function.Arguments)
			fmt.Printf("  ← %s\n", repl.Clip(result, 100))
			history = append(history, schema.ToolMessage(result, call.ID, schema.WithToolName(call.Function.Name)))
		}
	}
}

// runTool 同步执行一次工具调用；失败也当文本回执交给模型，不中断循环。
func runTool(ctx context.Context, inv tool.InvokableTool, name, args string) string {
	if inv == nil {
		return "未知工具: " + name
	}
	result, err := inv.InvokableRun(ctx, args)
	if err != nil {
		return "工具执行失败: " + err.Error()
	}
	return result
}
