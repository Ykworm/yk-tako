// 本文件：四个记账工具（TaskCreate/TaskUpdate/TaskList/TaskGet）与输出格式
// （操作回执 + 当前看板 + 催更块）。工具由 utils.InferTool 生成，最终实现
// tool.InvokableTool（github.com/cloudwego/eino/components/tool 包 interface.go:42，
// eino v0.9.21），对外用更宽的 tool.BaseTool（同文件 :32）承载。
//
// 约束写在 tool description（不是状态机），学 Claude Tasks；
// 催更块学 WeKnora 的 todo_write（internal/agent/tools/todo_write.go 的
// Important Reminder 注入）。
package taskboard

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// 四个工具名常量：middleware 注入、IsBoardTool 投影、测试共用一份事实来源。
const (
	ToolCreate = "TaskCreate"
	ToolUpdate = "TaskUpdate"
	ToolList   = "TaskList"
	ToolGet    = "TaskGet"
)

// IsBoardTool 判断工具名是不是四个记账工具之一。adkrun 的 printMessage 用它做看板投影。
func IsBoardTool(name string) bool {
	switch name {
	case ToolCreate, ToolUpdate, ToolList, ToolGet:
		return true
	}
	return false
}

// createArgs 是 TaskCreate 的参数。
type createArgs struct {
	Subject     string `json:"subject" jsonschema:"description=任务标题，一句话说清楚要做什么"`
	Description string `json:"description,omitempty" jsonschema:"description=补充细节（可选）：为什么做、约束、验收线索"`
	ActiveForm  string `json:"activeForm,omitempty" jsonschema:"description=「正在做」的现在时短句（可选），看板 in_progress 时展示"`
}

// updateArgs 是 TaskUpdate 的参数；只传要改的字段，空串表示不动。
type updateArgs struct {
	TaskID      string `json:"taskId" jsonschema:"description=要更新的任务 id（TaskCreate 返回的 #N）"`
	Status      string `json:"status,omitempty" jsonschema:"description=新状态：pending / in_progress / completed / deleted"`
	Subject     string `json:"subject,omitempty" jsonschema:"description=新标题（可选）"`
	Description string `json:"description,omitempty" jsonschema:"description=新描述（可选）"`
	ActiveForm  string `json:"activeForm,omitempty" jsonschema:"description=新的「正在做」短句（可选）"`
}

// getArgs 是 TaskGet 的参数。
type getArgs struct {
	TaskID string `json:"taskId" jsonschema:"description=要读取的任务 id（#N）"`
}

// listArgs 是 TaskList 的占位参数（无参）。
type listArgs struct{}

// 工具描述即记账协议（约束写在这里，不靠状态机强挡）。
const (
	createDesc = `创建一条任务写进看板，返回 taskId。什么时候建表：请求需要 ≥3 步、或用户一次列了多件事时才建；一步能完成的请求不要建表。`
	updateDesc = `按 taskId 更新任务（只传要改的字段）。纪律：动手前先把对应任务标 in_progress（同时只保持一条）；做完一项立刻标 completed，不要攒到最后一起勾——测试未通过、只做了一半、有未解决报错时不准标 completed；不要的任务标 deleted。卡住时改用 TaskCreate 建一条 blocker 任务说明卡点。`
	listDesc   = `列出看板上全部未删除任务（含状态），按创建顺序。`
	getDesc    = `按 taskId 读一条任务的完整信息（标题、描述、状态）。`
)

// makeTools 生成四个记账工具，操作同一份 store 正本。
// 每次变更后把全量快照写进 session（adk.AddSessionValue）：
// interrupt 时镜像随 runContext gob 进 checkpoint（机制见 taskboard.go 的 SessionKey 注释）；
// 无 session 的 ctx（单测直调）下镜像是静默空操作。
func makeTools(store *Store) ([]tool.BaseTool, error) {
	// mirror 把当前全量快照写进 session。
	mirror := func(ctx context.Context) {
		adk.AddSessionValue(ctx, SessionKey, store.Snapshot())
	}
	// withBoard 拼标准输出：回执 + 看板 + 催更块。
	withBoard := func(receipt string) string {
		list := store.List()
		return receipt + "\n\n" + board(list) + nudge(list)
	}

	create, err := utils.InferTool(ToolCreate, createDesc,
		func(ctx context.Context, args createArgs) (string, error) {
			t := store.Create(args.Subject, args.Description, args.ActiveForm)
			mirror(ctx)
			return withBoard(fmt.Sprintf("已创建任务 #%s「%s」", t.ID, t.Subject)), nil
		})
	if err != nil {
		return nil, err
	}
	update, err := utils.InferTool(ToolUpdate, updateDesc,
		func(ctx context.Context, args updateArgs) (string, error) {
			t, err := store.Update(args.TaskID, Patch{
				Subject: args.Subject, Description: args.Description,
				ActiveForm: args.ActiveForm, Status: args.Status,
			})
			if err != nil {
				return "", err
			}
			mirror(ctx)
			return withBoard(fmt.Sprintf("已更新任务 #%s（当前状态：%s）", t.ID, t.Status)), nil
		})
	if err != nil {
		return nil, err
	}
	list, err := utils.InferTool(ToolList, listDesc,
		func(ctx context.Context, _ listArgs) (string, error) {
			l := store.List()
			return board(l) + nudge(l), nil
		})
	if err != nil {
		return nil, err
	}
	get, err := utils.InferTool(ToolGet, getDesc,
		func(ctx context.Context, args getArgs) (string, error) {
			t, err := store.Get(args.TaskID)
			if err != nil {
				return "", err
			}
			receipt := fmt.Sprintf("#%s「%s」状态 %s", t.ID, t.Subject, t.Status)
			if t.Description != "" {
				receipt += "\n描述：" + t.Description
			}
			return withBoard(receipt), nil
		})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{create, update, list, get}, nil
}

// statusIcon 给看板行的状态图标。
func statusIcon(s Status) string {
	switch s {
	case StatusCompleted:
		return "✅"
	case StatusInProgress:
		return "🔄"
	default:
		return "⏳"
	}
}

// board 渲染紧凑看板；空板返回占位行。
func board(tasks []Task) string {
	if len(tasks) == 0 {
		return "📋 任务看板（空）"
	}
	done := 0
	for _, t := range tasks {
		if t.Status == StatusCompleted {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📋 任务看板（%d/%d 已完成）", done, len(tasks))
	for _, t := range tasks {
		subject := t.Subject
		if t.Status == StatusInProgress && t.ActiveForm != "" {
			subject = t.ActiveForm
		}
		fmt.Fprintf(&b, "\n  %s #%s %s", statusIcon(t.Status), t.ID, subject)
	}
	return b.String()
}

// nudge 是催更块：还有未完成项时追加在工具输出尾部；全部完成返回空串。
func nudge(tasks []Task) string {
	remaining := 0
	for _, t := range tasks {
		if t.Status == StatusPending || t.Status == StatusInProgress {
			remaining++
		}
	}
	if remaining == 0 {
		return ""
	}
	return fmt.Sprintf(`
⚠️ 还有 %d 项未完成。要求：
- 动手前先把要做的任务标成 in_progress（同时只保持一条）；
- 做完一项立刻标 completed，不要攒到最后一起勾；
- 全部完成前不要输出最终总结。`, remaining)
}
