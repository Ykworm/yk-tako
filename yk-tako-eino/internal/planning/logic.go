package planning

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// 清单状态。和 TypeScript 版的 zod enum 同一组取值。
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"

	WriteTodosTool = "write_todos"
	NudgeTool      = "planning_nudge"
	NudgePrefix    = "[系统提醒·任务清单]"
)

// Todo 是清单里的一项。write_todos 每次提交整份列表，全量替换。
type Todo struct {
	Content string `json:"content" jsonschema:"description=任务内容，一句话，具体可执行"`
	Status  string `json:"status" jsonschema:"enum=pending,enum=in_progress,enum=completed,description=pending 待办 / in_progress 进行中 / completed 已完成"`
}

const defaultPrompt = `## ` + "`write_todos`" + ` 任务清单

你可以用 ` + "`write_todos`" + ` 工具维护一份任务清单，把复杂目标拆成可跟踪的步骤，让用户看到你的进度。

什么时候用：
- 需要 3 步以上的复杂任务、用户一次给了多件事、或用户明确要求列计划
- 简单任务（一两步能完成）直接做，不要用——写清单本身也花 token

怎么用：
- 每次调用提交完整清单（全量替换语义，不是追加）
- 开始某项前先把它标成 in_progress；完成一项立刻标 completed，不要攒着批量改
- 计划可以随进展修订：过时的删掉，新发现的加上
- 每轮最多调用一次，且写清单的那轮不要同时调其它工具（全量替换下混排会让清单状态和执行结果对不上时序）`

const parallelError = "错误：write_todos 必须独占一轮：每轮只能调用一次，且不能与其它工具同轮调用。它是全量替换语义，混排会让清单状态和执行结果对不上。请把清单更新单独放一轮，工具执行放到下一轮。"

var statusIcon = map[string]string{
	StatusPending:    "⬜",
	StatusInProgress: "🔨",
	StatusCompleted:  "✅",
}

// FormatTodos 是紧凑摘要。工具回执和 /todos 共用。
func FormatTodos(todos []Todo) string {
	if len(todos) == 0 {
		return "（清单为空）"
	}
	lines := make([]string, len(todos))
	for i, todo := range todos {
		icon := statusIcon[todo.Status]
		if icon == "" {
			icon = "❓"
		}
		lines[i] = fmt.Sprintf("%s %d. %s", icon, i+1, todo.Content)
	}
	return strings.Join(lines, "\n")
}

// Receipt 是 write_todos 成功后的回执。不回显 JSON。
func Receipt(todos []Todo) string {
	done := 0
	for _, todo := range todos {
		if todo.Status == StatusCompleted {
			done++
		}
	}
	return fmt.Sprintf("任务清单已更新（%d/%d 已完成）：\n%s", done, len(todos), FormatTodos(todos))
}

// NudgeText 是半途而废提醒的正文。只列出未完成项。
func NudgeText(todos []Todo) string {
	left := incomplete(todos)
	return fmt.Sprintf("%s 还有 %d 项任务未完成（含进行中的）：\n%s\n如果确实无法继续，请向用户说明卡在哪里；否则请继续执行，并在完成后更新清单。",
		NudgePrefix, len(left), FormatTodos(left))
}

// IsPlanningNudge 认工具名，也认正文前缀。事件拷贝有时只剩正文。
func IsPlanningNudge(msg *schema.Message) bool {
	if msg == nil {
		return false
	}
	if msg.Name == NudgeTool || msg.ToolName == NudgeTool {
		return true
	}
	return strings.HasPrefix(msg.Content, NudgePrefix)
}

// AlreadyNudgedThisTurn 从后往前扫。先撞上提醒就停；先撞上用户发言说明这轮还没催过。
func AlreadyNudgedThisTurn(messages []*schema.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil {
			continue
		}
		if IsPlanningNudge(msg) {
			return true
		}
		if msg.Role == schema.User {
			return false
		}
	}
	return false
}

// WroteTodosThisTurn 只认最近一条真实用户消息之后的成功回执。
// 上一轮留下的 in_progress 不算，避免下一句无关问答被拽回去。
func WroteTodosThisTurn(messages []*schema.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil || IsPlanningNudge(msg) {
			continue
		}
		if msg.Role == schema.User {
			return false
		}
		if !isWriteTodosResult(msg) || isErrorReceipt(msg.Content) {
			continue
		}
		return true
	}
	return false
}

// ShouldNudge 是外扩的收工检查。四条都要满足：开关开着、模型想收工、
// 清单里有 in_progress、本轮成功写过清单、这轮还没催过。
func ShouldNudge(messages []*schema.Message, todos []Todo, enabled bool) bool {
	if !enabled || len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last == nil || last.Role != schema.Assistant || len(last.ToolCalls) > 0 {
		return false
	}
	if !hasInProgress(todos) {
		return false
	}
	if !WroteTodosThisTurn(messages) {
		return false
	}
	return !AlreadyNudgedThisTurn(messages)
}

// IconList 从手写回执里取出图标列表。
func IconList(content string) (string, bool) {
	if !strings.Contains(content, "任务清单已更新") {
		return "", false
	}
	_, list, ok := strings.Cut(content, "\n")
	if !ok || list == "" {
		return "", false
	}
	return list, true
}

// StatusList 从官方 write_todos 回执的 JSON 里拼出 [status] content。
func StatusList(content string) (string, bool) {
	start := strings.Index(content, "[")
	if start < 0 {
		return "", false
	}
	var todos []Todo
	if err := json.NewDecoder(strings.NewReader(content[start:])).Decode(&todos); err != nil || len(todos) == 0 {
		return "", false
	}
	parts := make([]string, len(todos))
	for i, todo := range todos {
		parts[i] = fmt.Sprintf("[%s] %s", todo.Status, todo.Content)
	}
	return strings.Join(parts, " | "), true
}

// incomplete 过滤出未完成项（pending 和 in_progress）。
func incomplete(todos []Todo) []Todo {
	out := make([]Todo, 0, len(todos))
	for _, todo := range todos {
		if todo.Status != StatusCompleted {
			out = append(out, todo)
		}
	}
	return out
}

// hasInProgress 判断清单里是否有进行中的项。
func hasInProgress(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status == StatusInProgress {
			return true
		}
	}
	return false
}

// isWriteTodosResult 判断消息是否是 write_todos 的工具结果。
func isWriteTodosResult(msg *schema.Message) bool {
	if msg.Role != schema.Tool && msg.ToolCallID == "" {
		return false
	}
	return msg.ToolName == WriteTodosTool || msg.Name == WriteTodosTool
}

// isErrorReceipt 判断回执是否为错误提示。
func isErrorReceipt(content string) bool {
	return strings.HasPrefix(content, "错误")
}

// validate 校验每项 status 取值是否合法。
func validate(todos []Todo) error {
	for i, todo := range todos {
		switch todo.Status {
		case StatusPending, StatusInProgress, StatusCompleted:
		default:
			return fmt.Errorf("第 %d 项 status 必须是 pending、in_progress 或 completed", i+1)
		}
	}
	return nil
}
