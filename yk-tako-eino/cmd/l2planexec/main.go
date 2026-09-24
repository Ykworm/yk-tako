// L2 对照 —— planexecute + Gate：规划与执行的时序由图保证，要不要规划由闸门判断。
//
// 对照 l2planning（手写 write_todos 中间件）和 l2builtin（deep.New 自带 write_todos）：
// 那两个把"先列计划再干活、干完再更新清单"交给模型自觉（软约束）；
// 这一个是 Sequential(Gate, Loop(Executor, Replanner)) 的节点图——
// Gate 是入口分流闸门（治实测痛点：官方 Planner 看不见 Executor 的工具清单，
// "现在几点"会被注水成 5 步计划）：强制 tool_choice 二选一，
// 纯知识问答直接 respond + Exit 终止全流程（Loop 不执行），
// 需要工具/多步才 plan，且 prompt 注入了 Executor 的工具清单（治幻觉）。
// Executor 每轮只执行计划的第一步，跑完才进 Replanner 改计划/收工。
// 对应 Eino 分层思想：要不要规划 = 判断题交给模型（结构化收束），执行时序 = 编排题交给图。
//
// 工具只留时间和计算（同 l2builtin），不接 L1 守卫。试两句对比：
//
//	1+1 等于几                                        # Gate 直接答复，不进循环
//	帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结  # Gate 出计划，逐步执行
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/planexecute"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/planexec"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

func main() {
	ctx := context.Background()
	// executor 用默认模型（干活的，保留完整能力）；planner/replanner 用强制
	// tool_choice 专用模型（DeepSeek 的 thinking 模式会拒强制 tool_choice，见 config 注释）。
	chatModel, ref := config.MustOpen(ctx)
	planModel, _ := config.MustOpenForForcedToolChoice(ctx)

	ts := tools.MustBasic(ctx)

	// Gate 顶替官方 Planner：同样的强制 tool_choice，但多一个 respond 出口
	// （+Exit 动作终止 Sequential），prompt 里注入了 executor 的工具清单。
	gate := planexec.NewGate(planModel, planexec.ToolsSummary(ctx, ts))
	executor, err := planexecute.NewExecutor(ctx, &planexecute.ExecutorConfig{
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: ts}},
	})
	if err != nil {
		exitf("创建 executor: %v", err)
	}
	replanner, err := planexecute.NewReplanner(ctx, &planexecute.ReplannerConfig{
		ChatModel: planModel,
	})
	if err != nil {
		exitf("创建 replanner: %v", err)
	}
	loop, err := adk.NewLoopAgent(ctx, &adk.LoopAgentConfig{
		SubAgents:     []adk.Agent{executor, replanner},
		MaxIterations: 10, // 同 planexecute.New 的默认值
	})
	if err != nil {
		exitf("创建 loop: %v", err)
	}
	agent, err := adk.NewSequentialAgent(ctx, &adk.SequentialAgentConfig{
		SubAgents: []adk.Agent{gate, loop},
	})
	if err != nil {
		exitf("创建 plan-execute agent: %v", err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: false,
	})

	console := repl.New()
	var history []*schema.Message
	fmt.Printf("L2 任务规划演示（planexecute 硬编排）。模型 %s。输入 quit 退出。\n\n", ref)
	for {
		input, ok := console.ReadLine("你: ")
		if !ok || input == "quit" || input == "exit" {
			break
		}
		if input == "" {
			continue
		}
		turn := append(append([]*schema.Message{}, history...), schema.UserMessage(input))
		answer, err := run(ctx, runner, turn)
		if err != nil {
			fmt.Println("出错了:", err)
			continue
		}
		history = append(history, schema.UserMessage(input))
		if answer != "" {
			history = append(history, schema.AssistantMessage(answer, nil))
		}
	}
}

// run 跑一轮 plan-execute，逐个打印阶段事件，返回 replanner 的最终答复。
// 各阶段的中间消息（planner 的计划 JSON、executor 的步骤输入）不进入对话历史：
// 它们是各 agent 用 GenModelInput 从 session 现拼的工作记忆，不是共享对话。
// 历史只保留"用户说了什么 + 最终答复"，供下一轮规划参考。
func run(ctx context.Context, runner *adk.Runner, input []*schema.Message) (string, error) {
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
		fmt.Printf("  ← 🔧 %s: %s\n", name, clip(msg.Content, 120))
		return "", false
	}
	if msg.Content == "" {
		return "", false
	}
	kind, steps, response := planexec.Classify(msg.Content)
	switch kind {
	case planexec.KindPlan:
		title := "📋 计划"
		if agent == "replanner" {
			title = "📋 更新计划（只含剩余步骤）"
		}
		fmt.Printf("  %s:\n", title)
		for i, step := range steps {
			fmt.Printf("     %d. %s\n", i+1, step)
		}
		fmt.Println()
	case planexec.KindRespond:
		fmt.Printf("\n🐙 %s\n\n", response)
		return response, true
	default:
		// Gate 直接答复（respond）已是纯文本而非 JSON，按最终答复处理并进历史。
		if agent == "gate" {
			fmt.Printf("\n🐙 %s\n\n", msg.Content)
			return msg.Content, true
		}
		fmt.Printf("  ✅ [%s] %s\n", agent, clip(msg.Content, 200))
	}
	return "", false
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
