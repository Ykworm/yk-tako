// Package planexec 是 l2planexec 演示的事件解析。
//
// planexecute 的 planner / replanner 用"强制工具调用"产出结构化结果，
// 但发到事件流里时是把工具参数 JSON 当普通文本（AssistantMessage 的 Content）。
// 这里按 JSON 形状把它分回三类：计划、最终答复、都不是（executor 的步骤小结）。
package planexec

import "encoding/json"

// Kind 是事件文本的分类。
type Kind int

const (
	// KindText 不是计划也不是答复（例如 executor 的步骤小结），原样展示即可。
	KindText Kind = iota
	// KindPlan 是计划：planner 的首版或 replanner 的修订版，只含剩余步骤。
	KindPlan
	// KindRespond 是 replanner 决定收工时的最终答复。
	KindRespond
)

// envelope 与 planexecute.PlanToolInfo / RespondToolInfo 的参数形状一一对应。
type envelope struct {
	Steps    []string `json:"steps"`
	Response string   `json:"response"`
}

// Classify 解析 planner / replanner 发出的文本。
// 返回类别；KindPlan 时 steps 为剩余步骤，KindRespond 时 response 为最终答复。
func Classify(content string) (kind Kind, steps []string, response string) {
	var env envelope
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return KindText, nil, ""
	}
	if env.Response != "" {
		return KindRespond, nil, env.Response
	}
	if len(env.Steps) > 0 {
		return KindPlan, env.Steps, ""
	}
	return KindText, nil, ""
}
