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
	// EventToolCall 是模型请求调用工具，一条事件带本批全部工具名。
	EventToolCall EventKind = "tool_call"
	// EventToolResult 是工具回执（Role=Tool 的消息），Text 为原始内容（截断归 sink）。
	EventToolResult EventKind = "tool_result"
	// EventBoard 是账本工具回执的看板投影，Text 含看板与催更原文（sink 不得截断）。
	EventBoard EventKind = "board"
)

// Event 是 adkrun 产出的一条结构化呈现事件。字段只带「发生了什么」：
// 前缀、emoji、截断等格式全部归 sink 决定（REPL sink 复刻旧 printMessage 的终端格式）。
type Event struct {
	Kind      EventKind       // 事件种类，决定 Text / ToolName / ToolNames 哪个有意义
	Agent     string          // 事件所属 agent 名（adk 盖的 AgentName），子代理前缀由 sink 决定
	Role      schema.RoleType // 消息角色：正文/调用为 assistant，回执/看板为 tool
	Text      string          // 正文 / 工具回执原文 / 看板原文
	ToolName  string          // 工具名（tool_result / board 用）
	ToolNames []string        // 工具名列表（tool_call 用，一条助手消息可带多个）
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
		names := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			names = append(names, call.Function.Name)
		}
		return Event{Kind: EventToolCall, Agent: agentName, Role: msg.Role, ToolNames: names}, true
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
