// Package runengine 实现 v6 运行引擎的 API 面（doc/19-v6）。
package runengine

// SessionID / RunID 是实体标识符的命名类型：传反 = 编译错误（新 A8）。
// 命名 string 类型的 JSON 编解码与 string 逐字节相同，线上契约不受影响。
type SessionID string
type RunID string

// Row 是 runlog 里的一条定稿记录（19v6-01 §3）。
// RunlogOffset 不落存储、读时按 index+1 注入（omitempty 保证存储形态不含它）。
type Row struct {
	RunlogOffset int    `json:"runlogOffset,omitempty"`
	RunID        RunID  `json:"runId"`
	Role         string `json:"role"`
	MessageID    string `json:"messageId,omitempty"`
	Text         string `json:"text"`
}

// Frame 是 runlive 里的一帧（19v6-01 §2）。EventOffset 同理，读时注入。
type Frame struct {
	EventOffset int    `json:"eventOffset,omitempty"`
	Kind        string `json:"kind"`
	MessageID   string `json:"messageId,omitempty"`
	Text        string `json:"text,omitempty"`
	Status      string `json:"status,omitempty"`
}

// ModelMessage 是 model_history 里的一条模型侧消息（19v6-03「真源划分」：
// runlog 记「产出了什么」，model_history 记「模型现在该看到什么」）。
// RunID/Seq 是 C 系列（compact）加的行内坐标：分组按 RunID、seq=0 识别摘要行、
// 挂点唯一性靠 (RunID, Seq)。内部类型，不进任何 API 契约面。
type ModelMessage struct {
	RunID RunID  `json:"runId"`
	Seq   int    `json:"seq"`
	Role  string `json:"role"`
	Text  string `json:"text"`
}
