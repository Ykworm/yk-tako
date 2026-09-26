// 本文件：SSE 线格式——adkrun 事件与 approval/error 的统一载荷，以及写帧。
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"yk-tako-eino/internal/adkrun"
)

// sseApproval 是 approval 事件的载荷：审批请求 id + guard 的原始信息。
// id 用于回执定位：前端 POST /api/approval 带上它，服务端凭它唤醒挂起的 ask。
type sseApproval struct {
	ID      string `json:"id"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	Message string `json:"message,omitempty"`
}

// sseEvent 是发给前端的一条事件，字段名对齐《10》§4.2 的事件契约：
// 前四类来自 adkrun.Event（message/tool_call/tool_result/board），
// approval 由本层的 ask 产出，error 来自 Run 的返回。
type sseEvent struct {
	Kind      string       `json:"kind"`
	Agent     string       `json:"agent,omitempty"`
	Role      string       `json:"role,omitempty"`
	Text      string       `json:"text,omitempty"`
	ToolName  string       `json:"toolName,omitempty"`
	ToolNames []string     `json:"toolNames,omitempty"`
	Approval  *sseApproval `json:"approval,omitempty"`
}

// sseEventFromAdkrun 把驱动层事件映射成 SSE 载荷：字段一一对应，不做格式加工
// （前缀、截断这些呈现决定归前端，同 T1 的「呈现归消费方」）。
func sseEventFromAdkrun(ev adkrun.Event) sseEvent {
	return sseEvent{
		Kind:      string(ev.Kind),
		Agent:     ev.Agent,
		Role:      string(ev.Role),
		Text:      ev.Text,
		ToolName:  ev.ToolName,
		ToolNames: ev.ToolNames,
	}
}

// writeSSE 写一帧并立即刷出：data 行装 JSON，帧以空行结束（SSE 规范）。
// 返回 false 表示写不出去（客户端断了）：调用方停止写并排干事件，
// 这一轮作废——sink 是易失的，断线不回放（《10》§4.4）。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev sseEvent) bool {
	data, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
