// 本文件：media_offload 的 ADK middleware。改写规则在 offload.go。
package mediaoffload

import (
	"context"

	"github.com/cloudwego/eino/adk"
)

// Handler 在每次主模型 Generate 之前把历史里的内联媒体换成占位符。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139 定义的 TypedChatModelAgentMiddleware[*schema.Message]（eino v0.9.21；
// 第 259 行起别名 ChatModelAgentMiddleware）。内嵌 adk.BaseChatModelAgentMiddleware
// （同文件 :277）提供其余钩子的空实现，本类型只覆盖 BeforeModelRewriteState（:171）。
//
// 每场会话一份：Session 记着本轮看过哪些媒体，共用会串台。
type Handler struct {
	adk.BaseChatModelAgentMiddleware
	session *Session
}

// 编译期校验 Handler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*Handler)(nil)

// NewHandler 建一场会话的落盘 middleware。cfg.Store 为空返回 nil，调用方就别往 Handlers 里塞。
func NewHandler(cfg Config) *Handler {
	session := NewSession(cfg)
	if session == nil {
		return nil
	}
	return &Handler{session: session}
}

// BeforeModelRewriteState 实现 adk.ChatModelAgentMiddleware 的 BeforeModelRewriteState
// （github.com/cloudwego/eino/adk/handler.go:171）：框架在每次主模型调用前调用，
// 工具结果写回之后也会再进一次，返回的 state.Messages 会被持久化并成为这次的模型输入。
// 本实现换掉的是新的消息对象，会话内存里的正本仍是原始字节，事件库回放不受影响。
// 落盘失败在 Session 里记日志，这里不返回错误——媒体省不下来是降级，不该让整轮失败。
func (h *Handler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if h == nil || state == nil {
		return ctx, state, nil
	}
	if msgs, changed := h.session.Rewrite(state.Messages); changed {
		state.Messages = msgs
	}
	return ctx, state, nil
}
