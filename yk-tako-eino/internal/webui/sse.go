// 本文件：SSE 线格式——adkrun 事件与 approval/error 的统一载荷，以及写帧。
package webui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"yk-tako-eino/internal/adkrun"
)

// sseApproval 是审批帧的载荷。请求帧带 id / tool / args / message；
// approval_result 帧只带 id 与 decision。id 用于回执定位：
// 前端 POST /api/approval 带上它，服务端凭它唤醒挂起的 ask。
type sseApproval struct {
	ID       string `json:"id,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Args     string `json:"args,omitempty"`
	Message  string `json:"message,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// sseToolCall 是 tool_call 帧里的一次调用。Args 为模型给出的原始参数字符串。
type sseToolCall struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// sseEvent 是落盘也是发给前端的一帧。run + seq 是续传地址，在写入事件库时填上；
// adkrun 的事件没有这两个字段。kind 区分 start / 呈现四类 / approval /
// approval_result / complete / error / stop / compact。
// compact 只表示正在压缩，text 是短句，不是摘要正文。
type sseEvent struct {
	Run       string        `json:"run,omitempty"`
	Seq       int64         `json:"seq,omitempty"`
	TS        string        `json:"ts,omitempty"`
	Kind      string        `json:"kind"`
	Agent     string        `json:"agent,omitempty"`
	Role      string        `json:"role,omitempty"`
	Text      string        `json:"text,omitempty"`
	ToolName  string        `json:"toolName,omitempty"`
	ToolCalls []sseToolCall `json:"toolCalls,omitempty"`
	Approval  *sseApproval  `json:"approval,omitempty"`
	Partial   bool          `json:"partial,omitempty"` // 正文还在变长，同一 seq 会再推一次
	Failed    bool          `json:"failed,omitempty"`  // 半截作废，不落库
}

// sseEventFromAdkrun 把驱动层事件映射成 SSE 载荷：字段一一对应，不做格式加工
// （前缀、截断这些呈现决定归前端）。seq 与 run 不在这里填，落盘时才有地址。
func sseEventFromAdkrun(ev adkrun.Event) sseEvent {
	out := sseEvent{
		Kind:     string(ev.Kind),
		Agent:    ev.Agent,
		Role:     string(ev.Role),
		Text:     ev.Text,
		ToolName: ev.ToolName,
	}
	if len(ev.ToolCalls) > 0 {
		out.ToolCalls = make([]sseToolCall, len(ev.ToolCalls))
		for i, c := range ev.ToolCalls {
			out.ToolCalls[i] = sseToolCall{Name: c.Name, Args: c.Args}
		}
	}
	return out
}

// writeSSE 写一帧并立即刷出：data 行装 JSON，帧以空行结束（SSE 规范）。
// 返回 false 表示写不出去（客户端断了）。事件已经先落了库，写失败只说明这个读者离开，run 继续。
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
