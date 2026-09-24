// 任务规划（L2）—— 手写版。对照 src/middleware/planning.ts。
//
// 四个零件和 TypeScript 版相同：清单、write_todos 全量替换、系统提示词、并行防守。
// 外扩的半途而废提醒也保留，触发条件和那边一致。
//
// 两处是 Eino 图结构逼出来的改法：
//
//  1. ChatModelAgent 的 state 没有自定义频道。清单放在进程内的 List 上，/todos 读它。
//     checkpoint 记的是消息和 interrupt，不记这份清单。演示进程活着的时候它都在。
//  2. 模型节点之后的路由只看最后一条消息有没有 tool call。TypeScript 版用
//     jumpTo: "model" 把提醒送回去；这里如果只追加一条 HumanMessage，最后一条没有
//     tool call，这轮会直接结束，模型要等用户下一句才看得到提醒。
//     所以提醒做成一次隐藏工具调用 planning_nudge：工具在工具节点上，
//     BeforeModelRewriteState 把它从模型可见的工具列表里拿掉。
//     图走到工具再回到模型，效果和 jumpTo 一样，而且不会跳过同轮里其它待执行的工具。
//
// 并行防守也不跳回模型。同轮出现多次 write_todos，或 write_todos 与其它干活工具
// 混排时，write_todos 不改清单、只回错误，同轮其它工具照常执行。全量替换语义下
// 清单更新必须独占一轮，混排会让清单状态和执行结果对不上时序。LangGraph 在
// 「这一轮全是 write_todos」时会直接出图，模型要到下一句用户输入才看见错误；
// Eino 执行完工具总会回到模型，所以错误在同一轮就可见。
package planning

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// List 是当前清单。读写都拷贝一份，调用方改返回值不会写穿。
type List struct {
	mu    sync.Mutex
	items []Todo
}

// Snapshot 返回当前清单的副本。
func (l *List) Snapshot() []Todo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Todo(nil), l.items...)
}

// Replace 全量替换清单。
func (l *List) Replace(todos []Todo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append([]Todo(nil), todos...)
}

// Options 是手写中间件的两个开关，和 TypeScript 版的 PlanningMiddlewareOptions 对应。
type Options struct {
	// SystemPrompt 为空时用紧凑中文提示词。
	SystemPrompt string
	// DisableNudge 关掉半途而废提醒。默认开。
	DisableNudge bool
}

// Planning 是挂到 ChatModelAgent 上的一整套：提示词、两个工具、状态钩子、清单。
type Planning struct {
	List       *List
	Middleware adk.AgentMiddleware
	Handler    *Handler
}

// Handler 负责模型前后的两道检查。工具闭包和它共享同一份清单。
type Handler struct {
	adk.BaseChatModelAgentMiddleware
	list   *List
	nudge  bool
	mu     sync.Mutex
	reject bool
	seq    int
}

type writeTodosArgs struct {
	Todos []Todo `json:"todos" jsonschema:"description=完整的任务清单（全量替换现有清单）"`
}

type emptyArgs struct{}

// New 建好手写规划。Middleware 注入提示词和工具，Handler 插进 Handlers，顺序在文件守卫之后、PII 之前。
func New(opts Options) (*Planning, error) {
	list := &List{}
	handler := &Handler{list: list, nudge: !opts.DisableNudge}
	writeTool, err := utils.InferTool(
		WriteTodosTool,
		"创建或更新任务清单（全量替换）。用于 3 步以上的复杂任务：先列计划，执行中实时更新各项状态（pending/in_progress/completed）。简单任务不要用。",
		handler.writeTodos,
	)
	if err != nil {
		return nil, fmt.Errorf("write_todos: %w", err)
	}
	nudgeTool, err := utils.InferTool(
		NudgeTool,
		"内部工具。系统用它把未完成的清单提醒送回模型。不要调用。",
		handler.remind,
	)
	if err != nil {
		return nil, fmt.Errorf("planning_nudge: %w", err)
	}
	prompt := opts.SystemPrompt
	if prompt == "" {
		prompt = defaultPrompt
	}
	return &Planning{
		List: list,
		Middleware: adk.AgentMiddleware{
			AdditionalInstruction: prompt,
			AdditionalTools:       []tool.BaseTool{writeTool, nudgeTool},
		},
		Handler: handler,
	}, nil
}

func (h *Handler) writeTodos(_ context.Context, args writeTodosArgs) (string, error) {
	if h.parallelRejected() {
		return parallelError, nil
	}
	if err := validate(args.Todos); err != nil {
		return "错误：" + err.Error(), nil
	}
	h.list.Replace(args.Todos)
	return Receipt(args.Todos), nil
}

func (h *Handler) remind(context.Context, emptyArgs) (string, error) {
	return NudgeText(h.list.Snapshot()), nil
}

// BeforeModelRewriteState 清掉上一轮的并行标记，并让模型看不见 planning_nudge。
// 工具仍留在工具节点上，图可以执行这次合成调用。
func (h *Handler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	h.setParallel(false)
	if state == nil {
		return ctx, state, nil
	}
	kept := make([]*schema.ToolInfo, 0, len(state.ToolInfos))
	for _, info := range state.ToolInfos {
		if info != nil && info.Name == NudgeTool {
			continue
		}
		kept = append(kept, info)
	}
	state.ToolInfos = kept
	return ctx, state, nil
}

// AfterModelRewriteState 看模型刚返回的那条消息。
// write_todos 同轮多次、或与其它工具混排时只打标记，不改消息，让同轮其它工具继续走。
// 该催的时候给这条助手消息补一次 planning_nudge，路由就会进工具节点再回到模型。
func (h *Handler) AfterModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	h.setParallel(false)
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	last := state.Messages[len(state.Messages)-1]
	if last == nil || last.Role != schema.Assistant {
		return ctx, state, nil
	}
	writes := 0
	for _, call := range last.ToolCalls {
		if call.Function.Name == WriteTodosTool {
			writes++
		}
	}
	if writes > 1 || (writes == 1 && len(last.ToolCalls) > 1) {
		h.setParallel(true)
		return ctx, state, nil
	}
	if ShouldNudge(state.Messages, h.list.Snapshot(), h.nudge) {
		h.attachNudge(last)
	}
	return ctx, state, nil
}

func (h *Handler) setParallel(reject bool) {
	h.mu.Lock()
	h.reject = reject
	h.mu.Unlock()
}

func (h *Handler) parallelRejected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reject
}

func (h *Handler) attachNudge(msg *schema.Message) {
	h.mu.Lock()
	h.seq++
	id := fmt.Sprintf("call_%s_%d", NudgeTool, h.seq)
	h.mu.Unlock()
	calls := make([]schema.ToolCall, len(msg.ToolCalls), len(msg.ToolCalls)+1)
	copy(calls, msg.ToolCalls)
	index := len(calls)
	msg.ToolCalls = append(calls, schema.ToolCall{
		Index: &index,
		ID:    id,
		Type:  "function",
		Function: schema.FunctionCall{
			Name:      NudgeTool,
			Arguments: "{}",
		},
	})
}
