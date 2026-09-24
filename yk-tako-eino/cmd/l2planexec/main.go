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
// 工具只留时间和计算（同 l2builtin），不接 L1 守卫——Gate 不执行工具，
// Executor 的 ExecutorConfig 不暴露 middleware 入口（见 planexec/gate.go 头注）。
// 试两句对比：
//
//	1+1 等于几                                        # Gate 直接答复，不进循环
//	帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结  # Gate 出计划，逐步执行
package main

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/planexecute"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"yk-tako-eino/internal/config"
	"yk-tako-eino/internal/planexec"
	"yk-tako-eino/internal/repl"
	"yk-tako-eino/internal/tools"
)

// main 建 Gate + Executor/Replanner 的 plan-execute 流程并进 REPL 循环。
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
		repl.Exitf("创建 executor: %v", err)
	}
	replanner, err := planexecute.NewReplanner(ctx, &planexecute.ReplannerConfig{
		ChatModel: planModel,
	})
	if err != nil {
		repl.Exitf("创建 replanner: %v", err)
	}
	loop, err := adk.NewLoopAgent(ctx, &adk.LoopAgentConfig{
		SubAgents:     []adk.Agent{executor, replanner},
		MaxIterations: 10, // 同 planexecute.New 的默认值
	})
	if err != nil {
		repl.Exitf("创建 loop: %v", err)
	}
	agent, err := adk.NewSequentialAgent(ctx, &adk.SequentialAgentConfig{
		SubAgents: []adk.Agent{gate, loop},
	})
	if err != nil {
		repl.Exitf("创建 plan-execute agent: %v", err)
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
		answer, err := planexec.Run(ctx, runner, turn)
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
