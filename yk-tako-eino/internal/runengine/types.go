// Package runengine 实现 v6 运行引擎的 API 面（doc/19-v6）。
package runengine

import "github.com/cloudwego/eino/schema"

// SessionID / RunID 是实体标识符的命名类型：传反 = 编译错误（新 A8）。
// 命名 string 类型的 JSON 编解码与 string 逐字节相同，线上契约不受影响。
type SessionID string
type RunID string

// Row 是 runlog 里的一条定稿记录（19v6-01 §3）。
// RunlogOffset 不落存储、读时按 index+1 注入（omitempty 保证存储形态不含它）。
// Agent / Model 恒在（19v6-01 §3）：Agent 空 = root；Model = 产出该行的模型生效 ref，
// 空 = 非模型产物（user 行——两列都空）。
// Payload 只有工具扩展行（role=tool_call/tool_result）有。
// UsageBill 是一次模型调用的 provider 账单（doc/27 §4a/§4b）：usage 帧五个数值列与
// assistant 行 payload 列共用同形状。ReasoningTokens/Window 缺席即省略（0 = 没报/未配窗）。
type UsageBill struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	ReasoningTokens  int `json:"reasoningTokens,omitempty"`
	TotalTokens      int `json:"totalTokens"`
	Window           int `json:"window,omitempty"`
}

// Row 是 runlog 里的一条定稿记录（19v6-01 §3）。RunlogOffset 读时注入（seq 列形态）。
// Usage 是 assistant 行的账单（读侧从 payload 解出注入；user/tool 行不写该字段）；
// assistant 行的 payload 原文不再透出，tool 扩展行 payload 照旧原样。
type Row struct {
	RunlogOffset int        `json:"runlogOffset,omitempty"`
	RunID        RunID      `json:"runId"`
	Role         string     `json:"role"`
	Agent        string     `json:"agent"`
	Model        string     `json:"model"`
	MessageID    string     `json:"messageId,omitempty"`
	Text         string     `json:"text"`
	Payload      string     `json:"payload,omitempty"`
	Usage        *UsageBill `json:"usage,omitempty"`
}

// Frame 是 runlive 里的一帧（19v6-01 §2）。EventOffset 同理，读时注入。
// Agent / Model 恒在（无 omitempty）：Agent 空 = root（含 start/terminal）；Model = 产出该帧的模型
// 生效 ref（T3-1b，与 TAKO_MODEL 同词表），空 = 非模型产物（start/terminal）。
// Name/Args/Output 是工具帧（kind=tool_call/tool_result）的负载。
// usage 帧（doc/27 §4a）五个数值列：每枪 provider 账单 + 窗配置值；账单缺失不发帧（不发 0）。
type Frame struct {
	EventOffset int    `json:"eventOffset,omitempty"`
	Kind        string `json:"kind"`
	Agent       string `json:"agent"`
	Model       string `json:"model"`
	MessageID   string `json:"messageId,omitempty"`
	Name        string `json:"name,omitempty"`
	// SkillName 只出现在 kind=task 帧（doc/52 B5）：触发这次写入的那条任务所属技能。
	// 逐行归属看 text（每行带「（技能：名字）」）；这个字段给页面标「这次写的是哪个技能的活」。
	SkillName string `json:"skillName,omitempty"`
	Args      string `json:"args,omitempty"`
	// ToolCallID 是 tool_call/tool_result 两帧的配对键（doc/52 B1）：模型给的调用 id，
	// 同一个工具调用的两帧此值相同。前端据此把结果挂回调用，不再靠相邻性猜。
	ToolCallID       string `json:"toolCallId,omitempty"`
	Output           string `json:"output,omitempty"`
	Text             string `json:"text,omitempty"`
	Status           string `json:"status,omitempty"`
	PromptTokens     int    `json:"promptTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens,omitempty"`
	ReasoningTokens  int    `json:"reasoningTokens,omitempty"`
	TotalTokens      int    `json:"totalTokens,omitempty"`
	Window           int    `json:"window,omitempty"`
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
	// MediaTokens 是这一行里、这一枪还会发给视觉模型的图片 token。
	// 已经算好，countInput 直接相加。不进 JSON，所以不进 mh、不进帧。
	MediaTokens int `json:"-"`
}

// TurnEntry 是 worker 在 run 内累积的一条模型侧消息（T2 归一层产物，随 defer 交 EndRun 落库），
// 与 ModelMessage 同形状、方向相反（产出侧）：assistant 轮带 ToolCalls，tool 轮带 ToolCallID/ToolName。
// 全部 PG 写收在 EndRun 的一笔事务里：要么整轮消息齐进 mh/runlog，要么一字不写（error / 闸失 = 零写入）。
// T3-2 加 Agent（空 = root、非空 = 子代理名）：EndRun 按它分拣两个投影——mh 只装 root 条目，
// runlog 全量装（子代理条目连 agent/model 两列一起落，供历史溯源）。
type TurnEntry struct {
	Role       string            // "assistant" / "tool"
	Text       string            // assistant：本轮正文（前导语就在自己这轮）；tool：结果全文
	ToolCalls  []schema.ToolCall // assistant 轮：模型给出的工具调用（原样落 JSON）
	ToolCallID string            // tool 轮：回指 assistant 轮 tool_calls 里的 id
	ToolName   string            // tool 轮：工具名（tool_result 帧 / runlog 扩展行用）
	Agent      string            // T3-2：产出该条目的 agent 名；空 = root
	Model      string            // 产出该条目的模型生效 ref（T3-1b 填）
	Usage      *UsageBill        // assistant 轮：本枪 provider 账单（doc/27 A3 落终稿行 payload 列用）
}
