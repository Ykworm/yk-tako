// 本文件：ADK Handler 观测架（T1 脚手架；转正点 T6——planning §4/§11.1-3，到时收成旗标或拆除）。
//
// 一件事：agent 的每个 Handler 钩子被调时，终端按序打 `[adk][序号] 钩子名｜功能一句话｜进/出`。
// 序号进程内单调（多 run 交错时也单调），每对 进/出 标记一次调用。
// 钩子名单照 v0.9.21 的 adk.ChatModelAgentMiddleware 接口（adk/handler.go）：九个方法全实现、
// **不嵌 Base 兜底**——接口将来加方法时这里编译不过（下面的编译期锚），提示补观测，而不是静默漏钩子。
// 只观测不改行为：所有方法原样返回入参。
//
// T1 零工具，跑一场 curl 实际到五个：BeforeAgent → BeforeModelRewriteState → WrapModel →
// AfterModelRewriteState → AfterAgent；四个工具包钩子（Wrap*ToolCall）要等 T2 有工具才到场。
package runengine

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
)

// 编译期锚：接口九方法缺一不可。
var _ adk.ChatModelAgentMiddleware = (*AgentLogHandler)(nil)

// AgentLogHandler 是观测 Handler：每个钩子进/出各打一行终端。
type AgentLogHandler struct {
	seq atomic.Int64
}

// NewAgentLogHandler 组一个观测 Handler，挂进 agent 装配的 Handlers。
func NewAgentLogHandler() *AgentLogHandler { return &AgentLogHandler{} }

// step 打一对 进/出：一次调用吃掉两个序号（跨 goroutine 也只增不重复）。
func (h *AgentLogHandler) step(hook, role string) {
	n := h.seq.Add(2)
	log.Printf("[adk][%d] %s｜%s｜进", n-1, hook, role)
	log.Printf("[adk][%d] %s｜%s｜出", n, hook, role)
}

// BeforeAgent：run 级入口——改 instruction / 增删 tools 的位点。
func (h *AgentLogHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	h.step("BeforeAgent", "run 级入口：改 instruction / tools 的位点")
	return ctx, runCtx, nil
}

// AfterAgent：run 级出口——拿完整会话状态；仅成功终态到达（error / 取消不来这里）。
func (h *AgentLogHandler) AfterAgent(ctx context.Context, state *adk.ChatModelAgentState) (context.Context, error) {
	h.step("AfterAgent", "run 级出口：拿完整会话状态（仅成功终态）")
	return ctx, nil
}

// BeforeModelRewriteState：每次调模型前——改 messages / ToolInfos 的位点（T4 声明过滤在这）。
func (h *AgentLogHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	h.step("BeforeModelRewriteState", "调模型前：改 messages / ToolInfos（声明过滤位点）")
	return ctx, state, nil
}

// AfterModelRewriteState：每次调模型后——模型回复已并入 state.Messages 末条。
func (h *AgentLogHandler) AfterModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	h.step("AfterModelRewriteState", "调模型后：回复已进 state 末条")
	return ctx, state, nil
}

// WrapModel：包模型调用——重试 / 换模型 / 改造输出流的位点。
func (h *AgentLogHandler) WrapModel(ctx context.Context, m model.BaseChatModel, mc *adk.ModelContext) (model.BaseChatModel, error) {
	h.step("WrapModel", "包模型调用：重试 / 换模型 / 改造流的位点")
	return m, nil
}

// WrapInvokableToolCall：包同步工具调用；tCtx.Name 是工具名（T2 起会到场）。
func (h *AgentLogHandler) WrapInvokableToolCall(ctx context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	h.step("WrapInvokableToolCall("+tCtx.Name+")", "包同步工具调用：拦参数 / 改结果")
	return endpoint, nil
}

// WrapStreamableToolCall：包流式工具调用。
func (h *AgentLogHandler) WrapStreamableToolCall(ctx context.Context, endpoint adk.StreamableToolCallEndpoint, tCtx *adk.ToolContext) (adk.StreamableToolCallEndpoint, error) {
	h.step("WrapStreamableToolCall("+tCtx.Name+")", "包流式工具调用")
	return endpoint, nil
}

// WrapEnhancedInvokableToolCall：包增强协议（ToolResult）的同步工具调用。
func (h *AgentLogHandler) WrapEnhancedInvokableToolCall(ctx context.Context, endpoint adk.EnhancedInvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	h.step("WrapEnhancedInvokableToolCall("+tCtx.Name+")", "包增强同步工具调用")
	return endpoint, nil
}

// WrapEnhancedStreamableToolCall：包增强协议的流式工具调用。
func (h *AgentLogHandler) WrapEnhancedStreamableToolCall(ctx context.Context, endpoint adk.EnhancedStreamableToolCallEndpoint, tCtx *adk.ToolContext) (adk.EnhancedStreamableToolCallEndpoint, error) {
	h.step("WrapEnhancedStreamableToolCall("+tCtx.Name+")", "包增强流式工具调用")
	return endpoint, nil
}
