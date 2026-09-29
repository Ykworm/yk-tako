// 本文件：压缩用的 ChatModelAgent middleware。摘要是一次模型 Generate，不是第二个 ChatModelAgent。
package compact

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// Generator 是摘要要用的那一次生成。只要求 Generate，测试不必实现 Stream。
// 生产里的 model.ToolCallingChatModel 满足这个方法集。
type Generator interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// HandlerConfig 是构造一个会话的压缩 Handler 要的材料。
type HandlerConfig struct {
	// Model 是和主对话同一个聊天模型。摘要直接调它的 Generate，不经过 agent，避免再次进入本钩子。
	Model Generator
	// WindowKey 是 厂商 ID/模型 ID，用来读写窗口大小。
	WindowKey string
	// Windows 是进程内那一份窗口大小记录。几场会话可以共用。
	Windows *WindowStore
	// Lookup 是缓存都没有时向厂商查的函数。为空则直接用 256000。
	Lookup Lookup
	// FixedWindow 大于 0 时跳过内存、文件和网络。测试用它钉住窗口大小。
	FixedWindow int
	// Estimator 为空且没有下面的计数函数时，NewHandler 自己建 cl100k_base。
	Estimator *Estimator
	// CountPrompt 与 CountMessages 非空时替代 Estimator。测试用它们控制 token 数。
	CountPrompt   func([]*schema.Message) int
	CountMessages func([]*schema.Message) int
}

// Handler 在每次主模型 Generate 之前决定要不要压缩。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139 定义的 TypedChatModelAgentMiddleware[*schema.Message]（eino v0.9.21；
// 第 259 行起别名 ChatModelAgentMiddleware）。内嵌 adk.BaseChatModelAgentMiddleware
// （同文件 :277）提供其余钩子的空实现，本类型覆盖 BeforeAgent 和 BeforeModelRewriteState。
type Handler struct {
	adk.BaseChatModelAgentMiddleware
	model         Generator
	windowKey     string
	windows       *WindowStore
	lookup        Lookup
	fixedWindow   int
	est           *Estimator
	countPrompt   func([]*schema.Message) int
	countMessages func([]*schema.Message) int
	search        tool.BaseTool
	mem           Memory
}

// 编译期校验 Handler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*Handler)(nil)

// NewHandler 绑定一场会话的 Memory。mem 为空会让几场会话没有地方可写，直接拒绝。
func NewHandler(cfg HandlerConfig, mem Memory) (*Handler, error) {
	if mem == nil {
		return nil, fmt.Errorf("compact: 缺少会话内存")
	}
	est := cfg.Estimator
	if est == nil && cfg.CountPrompt == nil {
		var err error
		est, err = NewEstimator()
		if err != nil {
			return nil, err
		}
	}
	search, err := newSearchTool(mem)
	if err != nil {
		return nil, err
	}
	return &Handler{
		model:         cfg.Model,
		windowKey:     cfg.WindowKey,
		windows:       cfg.Windows,
		lookup:        cfg.Lookup,
		fixedWindow:   cfg.FixedWindow,
		est:           est,
		countPrompt:   cfg.CountPrompt,
		countMessages: cfg.CountMessages,
		search:        search,
		mem:           mem,
	}, nil
}

// BeforeAgent 实现 adk.ChatModelAgentMiddleware 的 BeforeAgent
// （github.com/cloudwego/eino/adk/handler.go:142）：框架在每次 agent run 启动时调用，
// 给 middleware 一次改本轮指令和工具表的机会。
// 本实现借它把 search_archive 追加进本轮工具表。压缩循环里的那一次检索不走这个工具。
// 先拷贝 runCtx 再改，不动调用方原来的工具表。
func (h *Handler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	if h == nil || runCtx == nil || h.search == nil {
		return ctx, runCtx, nil
	}
	copied := *runCtx
	copied.Tools = append(copied.Tools, h.search)
	return ctx, &copied, nil
}

// BeforeModelRewriteState 实现 adk.ChatModelAgentMiddleware 的 BeforeModelRewriteState
// （github.com/cloudwego/eino/adk/handler.go:171）：框架在每次主模型调用前调用，工具结果写回之后也会再进一次。
// 返回的 state.Messages 会被持久化，并成为这一次主模型 Generate 的输入。
// 本实现按第 9 节压缩。没超过 80% 也换成新的消息对象，后面的 piiHandler 就地改的是副本。
// 返回错误时不改 state.Messages，框架不会去调主模型。
func (h *Handler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if h == nil || state == nil || h.mem == nil {
		return ctx, state, nil
	}
	out, err := Plan(ctx, Input{
		Messages: state.Messages,
		RunIDs:   h.mem.CompletedRunIDs(),
		Window:   h.window(ctx),
		Query:    h.mem.OriginalUser(),
	}, h.deps())
	if err != nil {
		return ctx, state, err
	}
	if out.Compacted {
		if err := h.mem.Stage(Commit{
			KeptRunIDs:    out.KeptRunIDs,
			DroppedRunIDs: out.DroppedRunIDs,
			CombinedUser:  out.CombinedUser,
		}); err != nil {
			return ctx, state, err
		}
	}
	state.Messages = out.Messages
	return ctx, state, nil
}

// window 取这一次比较用的窗口大小。FixedWindow 优先，避免测试去碰文件和网络。
func (h *Handler) window(ctx context.Context) int {
	if h.fixedWindow > 0 {
		return h.fixedWindow
	}
	if h.windows == nil {
		return FallbackWindow
	}
	return h.windows.Get(ctx, h.windowKey, h.lookup)
}

// deps 把 Handler 上的模型和计数收成 Plan 要的函数。
func (h *Handler) deps() Deps {
	countPrompt := h.countPrompt
	countMessages := h.countMessages
	if countPrompt == nil && h.est != nil {
		countPrompt = h.est.CountPrompt
	}
	if countMessages == nil && h.est != nil {
		countMessages = h.est.CountMessages
	}
	return Deps{
		CountPrompt:   countPrompt,
		CountMessages: countMessages,
		Summarize:     h.summarize,
		WriteRAG:      h.mem.WriteRAG,
		SearchRAG:     h.mem.SearchRAG,
		Notify:        h.mem.Notify,
		Logf:          log.Printf,
	}
}

// summarize 直接调用聊天模型的 Generate。不带工具，也不会再进入 BeforeModelRewriteState。
// 空正文当成失败，不能拿空摘要去换掉已经结束的轮。
func (h *Handler) summarize(ctx context.Context, transcript string) (string, error) {
	if h.model == nil {
		return "", fmt.Errorf("compact: 没有摘要模型")
	}
	msg, err := h.model.Generate(ctx, []*schema.Message{
		schema.SystemMessage(summaryPrompt),
		schema.UserMessage(transcript),
	})
	if err != nil {
		return "", err
	}
	if msg == nil || strings.TrimSpace(msg.Content) == "" {
		return "", fmt.Errorf("compact: 摘要为空")
	}
	return strings.TrimSpace(msg.Content), nil
}
