// Package adkrun 是 ADK Runner 的驱动层：消费 Runner 的事件流并分类产出（呈现归消费方
// sink）、攒历史、跑 interrupt 审批回路，外加一个内存版 checkpoint store。唯一入口
// cmd/yktako 使用；驱动循环在 turn.go，事件契约在本文件。
package adkrun

import (
	"yk-tako-eino/internal/taskboard"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// EventKind 是事件的种类标签，取值对应《10》§4.2 事件契约的呈现四类。
// 只有这四类经过 Sink：approval 由消费方的 ask 实现产出（HTTP 入口在 T2.3 推 SSE）、
// error 由 Run 返回调用方——两者都不属于「呈现」，T1 保持原有行为不变。
type EventKind string

// 呈现事件的四种 Kind（《10》§4.2 表的 message / tool_call / tool_result / board）。
const (
	// EventMessage 是代理正文（助手消息 Content）；子代理的由 sink 决定加前缀。
	EventMessage EventKind = "message"
	// EventToolCall 是模型请求调用工具，一条事件带本批全部调用（名字与原始参数）。
	EventToolCall EventKind = "tool_call"
	// EventToolResult 是工具回执（Role=Tool 的消息），Text 为原始内容（截断归 sink）。
	EventToolResult EventKind = "tool_result"
	// EventBoard 是账本工具回执的看板投影，Text 含看板与催更原文（sink 不得截断）。
	EventBoard EventKind = "board"
)

// ToolCall 是一次工具调用的呈现载荷。Args 是模型给出的原始参数字符串
// （schema.FunctionCall.Arguments），产生侧不截断、不解析。
// 截断与折叠归消费方：REPL 打印前 Clip，Web 在页面里折叠。
// 调用 id 不在这里：tool_result 今天也只按工具名配对，档案不另开一条关联。
type ToolCall struct {
	Name string
	Args string
}

// Event 是 adkrun 产出的一条结构化呈现事件。字段只带「发生了什么」：
// 前缀、emoji、截断等格式全部归 sink 决定（REPL sink 复刻旧 printMessage 的终端格式）。
// 序号不在本结构上。Web 落盘时才分配 seq，REPL 不写库。
type Event struct {
	Kind      EventKind       // 事件种类，决定 Text / ToolName / ToolCalls 哪个有意义
	Agent     string          // 事件所属 agent 名（adk 盖的 AgentName），子代理前缀由 sink 决定
	Role      schema.RoleType // 消息角色，沿用 msg.Role（可能为空，如流式回执）；分流请以 Kind 为准
	Text      string          // 正文 / 工具回执原文 / 看板原文
	ToolName  string          // 工具名（tool_result / board 用）
	ToolCalls []ToolCall      // 工具调用（tool_call 用，一条助手消息可带多次，参数原样保留）
	Partial   bool            // 正文还在逐字流出。消费方只更新内存，不落库
	Drop      bool            // 模型流出失败，半截作废
	Discard   bool            // 半截正文被最终结果取代（例如变成工具调用），静默丢掉
}

// Sink 是事件消费函数：adkrun 只负责产出，打印、推流等呈现由消费方实现。
// 用函数类型而非 interface：单方法、无状态，与同包的 Ask（turn.go）同一风格。
// nil 合法：Run 收到 nil 表示不呈现（只攒历史、收 interrupt），测试与无头调用可用。
type Sink func(Event)

// eventFrom 把一条消息输出分类成呈现事件。ok=false 表示没有可呈现内容（空消息），
// 调用方直接忽略。分类口径与重构前 printMessage 的分支一一对应：
// ToolCalls→tool_call、Role==Tool→tool_result/board、Content→message。
// mv 与 msg 的关系：tool 判定取「mv.Role == Tool || msg.Role == Tool」；
// ToolName 取 mv 优先、msg 兜底；Event.Role 一律沿用 msg.Role（mv 只做判定）。
// mv 由调用方保证非 nil（consume 里 MessageOutput 非空才调）。
func eventFrom(agentName string, mv *adk.MessageVariant, msg *schema.Message) (Event, bool) {
	if msg == nil {
		return Event{}, false
	}
	if len(msg.ToolCalls) > 0 {
		calls := make([]ToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			// Arguments 原样留下。日志是档案，这里一裁，回放就补不回来。
			calls = append(calls, ToolCall{Name: call.Function.Name, Args: call.Function.Arguments})
		}
		return Event{Kind: EventToolCall, Agent: agentName, Role: msg.Role, ToolCalls: calls}, true
	}
	if mv.Role == schema.Tool || msg.Role == schema.Tool {
		name := mv.ToolName
		if name == "" {
			name = msg.ToolName
		}
		kind := EventToolResult
		if taskboard.IsBoardTool(name) {
			kind = EventBoard
		}
		return Event{Kind: kind, Agent: agentName, Role: msg.Role, Text: msg.Content, ToolName: name}, true
	}
	if msg.Content != "" {
		return Event{Kind: EventMessage, Agent: agentName, Role: msg.Role, Text: msg.Content}, true
	}
	return Event{}, false
}
