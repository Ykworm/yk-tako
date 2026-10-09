// Package runengine 实现 v6 运行引擎的 API 面（doc/19-v6）。
package runengine

import "github.com/cloudwego/eino/schema"

// SessionID / RunID 是实体标识符的命名类型：传反 = 编译错误（新 A8）。
// 命名 string 类型的 JSON 编解码与 string 逐字节相同，线上契约不受影响。
type SessionID string
type RunID string

// Row 是 runlog 里的一条定稿记录（19v6-01 §3）。
// RunlogOffset 不落存储、读时按 index+1 注入（omitempty 保证存储形态不含它）。
// Agent 恒在（空 = root，19v6-01 §3）；Payload 只有工具扩展行（role=tool_call/tool_result）有。
type Row struct {
	RunlogOffset int    `json:"runlogOffset,omitempty"`
	RunID        RunID  `json:"runId"`
	Role         string `json:"role"`
	Agent        string `json:"agent"`
	MessageID    string `json:"messageId,omitempty"`
	Text         string `json:"text"`
	Payload      string `json:"payload,omitempty"`
}

// Frame 是 runlive 里的一帧（19v6-01 §2）。EventOffset 同理，读时注入。
// Agent 恒在（空 = root，含 start/terminal）；Name/Args/Output 是工具帧（kind=tool_call/tool_result）的负载。
type Frame struct {
	EventOffset int    `json:"eventOffset,omitempty"`
	Kind        string `json:"kind"`
	Agent       string `json:"agent"`
	MessageID   string `json:"messageId,omitempty"`
	Name        string `json:"name,omitempty"`
	Args        string `json:"args,omitempty"`
	Output      string `json:"output,omitempty"`
	Text        string `json:"text,omitempty"`
	Status      string `json:"status,omitempty"`
}

// ModelMessage 是 model_history 里的一条模型侧消息（19v6-03「真源划分」：
// runlog 记「产出了什么」，model_history 记「模型现在该看到什么」）。
// RunID/Seq 是 C 系列（compact）加的行内坐标：分组按 RunID、seq=0 识别摘要行、
// 挂点唯一性靠 (RunID, Seq)。内部类型，不进任何 API 契约面。
// ToolCalls / ToolCallID 是 T2 工具消息的两个挂件（OpenAI assistant/tool 形状）：
// assistant 行带 ToolCalls、tool 行带 ToolCallID，读回时重建模型输入（prepareInput）。
type ModelMessage struct {
	RunID      RunID             `json:"runId"`
	Seq        int               `json:"seq"`
	Role       string            `json:"role"`
	Text       string            `json:"text"`
	ToolCalls  []schema.ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
}

// TurnEntry 是 worker 在 run 内累积的一条模型侧消息（T2 归一层产物，随 defer 交 EndRun 落库），
// 与 ModelMessage 同形状、方向相反（产出侧）：assistant 轮带 ToolCalls，tool 轮带 ToolCallID/ToolName。
// 全部 PG 写收在 EndRun 的一笔事务里：要么整轮消息齐进 mh/runlog，要么一字不写（error / 闸失 = 零写入）。
type TurnEntry struct {
	Role       string            // "assistant" / "tool"
	Text       string            // assistant：本轮正文（前导语就在自己这轮）；tool：结果全文
	ToolCalls  []schema.ToolCall // assistant 轮：模型给出的工具调用（原样落 JSON）
	ToolCallID string            // tool 轮：回指 assistant 轮 tool_calls 里的 id
	ToolName   string            // tool 轮：工具名（tool_result 帧 / runlog 扩展行用）
}
