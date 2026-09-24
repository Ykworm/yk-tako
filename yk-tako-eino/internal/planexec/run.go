// plan-execute 的事件消费与打印。与 internal/adkrun 对 ChatModelAgent 的职责相同，
// 只是 plan-execute 各阶段的事件语义不同（gate/replanner 的"消息"是强制工具调用的
// 参数 JSON），所以单独一份。
package planexec

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/repl"
)

// Run 跑一轮 plan-execute，逐个打印阶段事件，返回最终答复。
// 各阶段的中间消息（gate 的计划 JSON、executor 的步骤输入）不进入对话历史：
// 它们是各 agent 用 GenModelInput 从 session 现拼的工作记忆，不是共享对话。
// 历史只保留"用户说了什么 + 最终答复"，供下一轮规划参考。
func Run(ctx context.Context, runner *adk.Runner, input []*schema.Message) (string, error) {
	iter := runner.Run(ctx, input)
	var answer string
	for {
		event, ok := iter.Next()
		if !ok {
			return answer, nil
		}
		if event.Err != nil {
			return answer, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		msg, err := event.Output.MessageOutput.GetMessage()
		if err != nil {
			return answer, err
		}
		if msg == nil {
			continue
		}
		if final, ok := printEvent(event.AgentName, event.Output.MessageOutput, msg); ok {
			answer = final
		}
	}
}

// printEvent 按来源打印一条事件。gate / replanner 的"消息"其实是工具参数 JSON
// （强制工具调用的产物），按形状分类后展示；executor 的是正常的工具调用循环。
// 返回最终答复（gate respond 或 replanner respond）。
func printEvent(agent string, mv *adk.MessageVariant, msg *schema.Message) (string, bool) {
	if len(msg.ToolCalls) > 0 {
		names := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			names = append(names, call.Function.Name)
		}
		fmt.Printf("  → 🔧 [%s] 调用: %s\n", agent, strings.Join(names, ", "))
		return "", false
	}
	if mv.Role == schema.Tool || msg.Role == schema.Tool {
		name := mv.ToolName
		if name == "" {
			name = msg.ToolName
		}
		fmt.Printf("  ← 🔧 %s: %s\n", name, repl.Clip(msg.Content, 120))
		return "", false
	}
	if msg.Content == "" {
		return "", false
	}
	kind, steps, response := Classify(msg.Content)
	switch kind {
	case KindPlan:
		title := "📋 计划"
		if agent == "replanner" {
			title = "📋 更新计划（只含剩余步骤）"
		}
		fmt.Printf("  %s:\n", title)
		for i, step := range steps {
			fmt.Printf("     %d. %s\n", i+1, step)
		}
		fmt.Println()
	case KindRespond:
		fmt.Printf("\n🐙 %s\n\n", response)
		return response, true
	default:
		// Gate 直接答复（respond）已是纯文本而非 JSON，按最终答复处理并进历史。
		if agent == "gate" {
			fmt.Printf("\n🐙 %s\n\n", msg.Content)
			return msg.Content, true
		}
		fmt.Printf("  ✅ [%s] %s\n", agent, repl.Clip(msg.Content, 200))
	}
	return "", false
}
