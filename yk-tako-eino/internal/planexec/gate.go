// Gate：Plan-Execute 的入口分流代理（Eino 风格治"过度规划"）。
//
// 问题（doc/06 §6 实测）：planexecute 的 Planner 看不见 Executor 的工具清单，
// 且 Sequential 无条件执行每个子代理——"现在几点"这种一步问题也会被
// 规划成 5 步注水计划。Eino 的思想是"Graph 管确定性，Agent 管自主性"：
// "要不要规划"是判断题，交给模型做，但用强制 tool_choice 收束成结构化决策
// （复用 replanner 的 respond/plan 工具对）；执行时序仍由图保证。
//
// 组成：Sequential(Gate, Loop(Executor, Replanner))
//   - 纯知识问答 → Gate 调 respond 直接答复 + Exit 动作终止 Sequential，Loop 不执行；
//   - 需要工具/多步 → Gate 调 plan（prompt 里注入了 Executor 的工具清单，治幻觉），
//     把计划写入 session，Loop 照常执行。
package planexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/planexecute"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// Plan 是 planexecute.Plan 接口的最小实现——官方的 defaultPlan 未导出，
// 但只要满足接口（FirstStep + JSON 序列化），Executor/Replanner 的类型断言即通过。
type Plan struct {
	Steps []string `json:"steps"`
}

func (p *Plan) FirstStep() string {
	if len(p.Steps) == 0 {
		return ""
	}
	return p.Steps[0]
}

func (p *Plan) MarshalJSON() ([]byte, error) {
	type alias Plan // 防止方法递归
	return json.Marshal((*alias)(p))
}

func (p *Plan) UnmarshalJSON(v []byte) error {
	type alias Plan
	return json.Unmarshal(v, (*alias)(p))
}

// ToolsSummary 生成工具清单的文本描述，注入 Gate 提示词，
// 让计划只依赖执行代理真实具备的能力（治"没有时钟工具"式幻觉）。
func ToolsSummary(ctx context.Context, tools []tool.BaseTool) string {
	var b strings.Builder
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", info.Name, info.Desc)
	}
	return b.String()
}

const gateInstruction = `你是一个任务分流闸门。对用户的请求做二选一判断，必须调用且只调用一个工具：

- respond：仅凭你已有的知识就能可靠回答（不需要任何工具、不需要实时信息、不需要精确计算）时，直接给出答复。
- plan：请求需要调用工具、查询实时信息（当前时间/日期等）、精确计算或分多步完成时，给出有序的执行计划，交给下游执行代理。

判断规则：
- 涉及"现在、当前、今天"等实时信息 → 必须 plan，你没有时钟，只有下游执行代理有工具；
- 涉及较大数字的精确计算 → 必须 plan，心算结果不可靠；
- 出现"再、又、这个结果、乘积、它"等指代词，说明请求引用了之前的对话 → 必须 plan，
  你看不到对话历史，但下游执行代理能看到，会结合历史正确执行；
- 拿不准 → plan。

下游执行代理可用的工具如下，计划只能依赖这些能力：
%s
计划要求：每步具体、可执行、自包含，按逻辑顺序排列；只包含达成目标所必需的步骤，
不要注水——只需要一次工具调用的请求，计划就只有一步。`

// Gate 的工具描述比官方默认更强调决策边界（ToolInfo 是值类型，复制后改 Desc）。
const (
	gatePlanToolDesc = "生成执行计划交给下游执行代理。当请求需要调用工具、查询实时信息（当前时间/日期等）、精确计算或分多步完成时，必须选择本工具。"
	gateRespondDesc  = "直接答复用户。仅当凭已有知识就能可靠回答（不需要任何工具、实时信息或精确计算）时才可选择本工具。"
)

// Gate 是 Plan-Execute 的入口分流代理。respond 时发 Exit 动作终止整个
// Sequential（ADK 官方机制，见 adk.NewExitAction）；plan 时顶替 planexecute
// planner 的职责，把用户输入与计划写入 session 供 Executor/Replanner 读取。
type Gate struct {
	model        model.ToolCallingChatModel // 需支持强制 tool_choice（config.OpenForForcedToolChoice）
	toolsSummary string
}

// NewGate 创建分流闸门；toolsSummary 由 ToolsSummary 生成。
func NewGate(m model.ToolCallingChatModel, toolsSummary string) *Gate {
	return &Gate{model: m, toolsSummary: toolsSummary}
}

func (g *Gate) Name(context.Context) string { return "gate" }

func (g *Gate) Description(context.Context) string {
	return "任务分流闸门：简单请求直接答复，复杂请求生成执行计划"
}

func (g *Gate) Run(ctx context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		fail := func(err error) { gen.Send(&adk.AgentEvent{Err: err}) }

		planInfo := planexecute.PlanToolInfo
		planInfo.Desc = gatePlanToolDesc
		respondInfo := planexecute.RespondToolInfo
		respondInfo.Desc = gateRespondDesc
		genModel, err := g.model.WithTools([]*schema.ToolInfo{&planInfo, &respondInfo})
		if err != nil {
			fail(fmt.Errorf("gate 绑定工具失败: %w", err))
			return
		}

		msg, err := genModel.Generate(ctx, g.buildMessages(input.Messages),
			model.WithToolChoice(schema.ToolChoiceForced))
		if err != nil {
			fail(fmt.Errorf("gate 生成失败: %w", err))
			return
		}
		if len(msg.ToolCalls) == 0 {
			fail(fmt.Errorf("gate 未产出工具调用（强制 tool_choice 失效？）: %s", msg.Content))
			return
		}

		tc := msg.ToolCalls[0]
		if os.Getenv("GATE_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[gate debug] tool=%s args=%s\n", tc.Function.Name, clipRunes(tc.Function.Arguments, 200))
		}
		switch tc.Function.Name {
		case planexecute.RespondToolInfo.Name:
			var args struct {
				Response string `json:"response"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				fail(fmt.Errorf("解析 respond 参数失败: %w", err))
				return
			}
			gen.Send(adk.EventFromMessage(schema.AssistantMessage(args.Response, nil), nil, schema.Assistant, ""))
			// Exit 动作：Sequential 见到子代理最后的动作是 Exit 即终止，
			// Loop(Executor, Replanner) 不会执行。
			gen.Send(&adk.AgentEvent{Action: adk.NewExitAction()})

		case planexecute.PlanToolInfo.Name:
			p := &Plan{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), p); err != nil {
				fail(fmt.Errorf("解析 plan 参数失败: %w", err))
				return
			}
			if p.FirstStep() == "" {
				fail(fmt.Errorf("计划为空"))
				return
			}
			// 顶替 planexecute planner 的职责：Executor 从 session 读这两样。
			adk.AddSessionValue(ctx, planexecute.UserInputSessionKey, input.Messages)
			adk.AddSessionValue(ctx, planexecute.PlanSessionKey, p)
			// 与官方 planner 一致：工具参数 JSON 转 content 发出（argToContent）。
			gen.Send(adk.EventFromMessage(schema.AssistantMessage(tc.Function.Arguments, nil), nil, schema.Assistant, ""))

		default:
			fail(fmt.Errorf("gate 产出了未知工具调用: %s", tc.Function.Name))
		}
	}()
	return iter
}

func clipRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// buildMessages 只把"当前请求"给闸门做决策，不给对话历史：
// 实测 deepseek 系模型见到历史里"user→assistant 直接答复"的样本会模式锁定，
// 后续所有请求都模仿历史走 respond 并复读旧答复（见 doc/06 §6）。
// 这也贴合 planexecute 的设计哲学——每个阶段用 GenModelInput 从 session
// 现拼自己需要的工作记忆，而不是共享一整段对话。多轮上下文并未丢失：
// 完整 input.Messages 已写入 UserInputSessionKey，Executor/Replanner 读得到。
func (g *Gate) buildMessages(userMsgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, 0, 2)
	out = append(out, schema.SystemMessage(fmt.Sprintf(gateInstruction, g.toolsSummary)))
	if len(userMsgs) > 0 {
		out = append(out, userMsgs[len(userMsgs)-1])
	}
	return out
}
