package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func init() {
	schema.RegisterName[ApprovalInfo]("tako_approval_info")
	schema.RegisterName[savedCall]("tako_saved_call")
}

// savedCall 记在 interrupt 状态里。嵌套的守卫会看到同一次中断，用 Kind 区分是谁暂停的。
type savedCall struct {
	Kind    string
	Tool    string
	Args    string
	Message string
}

// ApprovalInfo 是 interrupt 带给 REPL 的载荷。resume 用 "approve" / "reject"，不用布尔。
type ApprovalInfo struct {
	Kind    string
	Tool    string
	Args    string
	Message string
}

const (
	ResumeApprove = "approve"
	ResumeReject  = "reject"
)

type toolGuardHandler struct {
	adk.BaseChatModelAgentMiddleware
	engine *Engine
}

type filesystemHandler struct {
	adk.BaseChatModelAgentMiddleware
	options Options
}

type piiHandler struct {
	adk.BaseChatModelAgentMiddleware
}

// Handlers 是 L1 的三件套，顺序就是包裹顺序：命令守卫在外，文件守卫居中，PII 最贴工具。
func Handlers(engine *Engine, fs Options) []adk.ChatModelAgentMiddleware {
	return []adk.ChatModelAgentMiddleware{
		&toolGuardHandler{engine: engine},
		&filesystemHandler{options: fs},
		&piiHandler{},
	}
}

func (h *toolGuardHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if stored, ok := ownInterrupt(ctx, kindToolGuard); ok {
			decision, decided := resumeDecision(ctx)
			if !decided {
				return "", interruptCall(ctx, kindToolGuard, stored)
			}
			if decision != ResumeApprove {
				return fmt.Sprintf("用户拒绝了这次 %s 调用。不要重试相同命令；如需类似操作请先与用户商量替代方案。", tCtx.Name), nil
			}
			return endpoint(ctx, stored.Args, opts...)
		}
		params := parseArgs(args)
		findings := h.engine.Guard(tCtx.Name, params)
		if len(findings) == 0 {
			return endpoint(ctx, args, opts...)
		}
		if h.engine.Mode() == ModeWarn {
			fmt.Printf("\n⚠️  [tool_guard] %s\n\n", formatFindings(findings, "检测到风险参数（warn 模式，放行）"))
			return endpoint(ctx, args, opts...)
		}
		if h.engine.Mode() == ModeApprove && h.engine.ShouldBlock(findings) {
			info := approvalInfo(h.engine, tCtx.Name, args)
			return "", compose.StatefulInterrupt(ctx, info, savedCall{Kind: kindToolGuard, Tool: tCtx.Name, Args: args, Message: info.Message})
		}
		if h.engine.ShouldBlock(findings) {
			return formatFindings(findings, "工具调用被安全策略拦截（command guard）"), nil
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

const (
	kindToolGuard = "tool_guard"
	kindFSGuard   = "fs_guard"
)

func ownInterrupt(ctx context.Context, kind string) (savedCall, bool) {
	was, has, stored := compose.GetInterruptState[savedCall](ctx)
	if !was || !has || stored.Kind != kind {
		return savedCall{}, false
	}
	return stored, true
}

func interruptCall(ctx context.Context, kind string, stored savedCall) error {
	return compose.StatefulInterrupt(ctx, ApprovalInfo{
		Kind: kind, Tool: stored.Tool, Args: stored.Args, Message: stored.Message,
	}, stored)
}

func approvalInfo(engine *Engine, toolName, args string) ApprovalInfo {
	params := parseArgs(args)
	findings := engine.Guard(toolName, params)
	header := "工具调用需要人工批准（命中风险规则）"
	if engine.Mode() == ModeBlock {
		header = "工具调用被安全策略拦截（command guard）"
	}
	return ApprovalInfo{
		Kind:    "tool_guard_approval",
		Tool:    toolName,
		Args:    args,
		Message: formatFindings(findings, header),
	}
}

func (h *filesystemHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if stored, ok := ownInterrupt(ctx, kindFSGuard); ok {
			decision, decided := resumeDecision(ctx)
			if !decided {
				return "", interruptCall(ctx, kindFSGuard, stored)
			}
			if decision != ResumeApprove {
				return fmt.Sprintf("用户拒绝了这次 %s 调用。不要重试相同路径。", tCtx.Name), nil
			}
			return endpoint(ctx, stored.Args, opts...)
		}
		decision := Decide(h.options, tCtx.Name, parseArgs(args))
		switch decision.Action {
		case Allow:
			return endpoint(ctx, args, opts...)
		case Ask:
			info := ApprovalInfo{
				Kind:    "fs_guard_approval",
				Tool:    tCtx.Name,
				Args:    args,
				Message: "文件访问需要人工批准：" + decision.Reason,
			}
			return "", compose.StatefulInterrupt(ctx, info, savedCall{Kind: kindFSGuard, Tool: tCtx.Name, Args: args, Message: info.Message})
		default:
			return "Error: permission denied —— " + decision.Reason, nil
		}
	}, nil
}

// BeforeModelRewriteState 把历史里的密钥打码后再给模型。RewriteState 钩子官方
// 允许就地改 state（chatmodel.go 的生命周期注释："can modify state"）；注意
// state.Messages 与事件流、调用方 history 共享消息指针，这里的改写会写穿回
// REPL 历史——当前语义下正是想要的（历史同样脱敏）。如果哪天需要"对模型脱敏、
// 历史保留原文"，这里要先浅拷贝消息再改。
func (h *piiHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	for _, msg := range state.Messages {
		if msg == nil || msg.Content == "" {
			continue
		}
		msg.Content = MaskSecrets(msg.Content)
	}
	return ctx, state, nil
}

func (h *piiHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, _ *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		out, err := endpoint(ctx, args, opts...)
		if err != nil {
			return "", err
		}
		return MaskSecrets(out), nil
	}, nil
}

func resumeDecision(ctx context.Context) (string, bool) {
	isTarget, hasData, data := compose.GetResumeContext[string](ctx)
	if !isTarget || !hasData {
		return "", false
	}
	return data, true
}

func parseArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(raw), &params); err != nil || params == nil {
		return map[string]any{"_raw": raw}
	}
	return params
}

func formatFindings(findings []Finding, header string) string {
	toolName := "?"
	if len(findings) > 0 {
		toolName = findings[0].ToolName
	}
	lines := []string{header, "Tool: " + toolName, ""}
	limit := len(findings)
	if limit > 5 {
		limit = 5
	}
	for _, finding := range findings[:limit] {
		lines = append(lines,
			fmt.Sprintf("- [%s] %s", finding.Severity, finding.Description),
			"  snippet: "+fmt.Sprintf("%q", finding.Snippet),
			"  remediation: "+finding.Remediation,
		)
	}
	if len(findings) > 5 {
		lines = append(lines, fmt.Sprintf("... 以及另外 %d 条告警", len(findings)-5))
	}
	return strings.Join(lines, "\n")
}
