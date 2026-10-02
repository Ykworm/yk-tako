// 本文件：L1 三件套守卫的 ADK middleware 实现、interrupt 载荷与恢复判定。
// 命令规则在 toolguard.go，路径规则在 filesystem.go，打码算法在 pii.go；
// 包的整体说明见 filesystem.go 的文件头。
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

// init 把 interrupt 载荷注册进 schema，checkpoint 序列化靠它。
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

// toolGuardHandler 是命令守卫：扫规则命中，按模式拦、问或放行。
//
// 实现接口：adk.ChatModelAgentMiddleware —— github.com/cloudwego/eino/adk 包
// handler.go:139 定义的 TypedChatModelAgentMiddleware[*schema.Message]（eino v0.9.21；
// 第 259 行起别名 ChatModelAgentMiddleware）。内嵌 adk.BaseChatModelAgentMiddleware
// （同文件 :277）提供其余钩子的空实现，本类型覆盖 WrapInvokableToolCall 与
// WrapEnhancedInvokableToolCall（:193 与 :217）——两种工具接口都要拦住。
type toolGuardHandler struct {
	adk.BaseChatModelAgentMiddleware
	engine *Engine
}

// 编译期校验 toolGuardHandler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*toolGuardHandler)(nil)

// filesystemHandler 是路径守卫：工作区内外、敏感文件规则都在这一层判。
//
// 实现接口：adk.ChatModelAgentMiddleware（同上，eino/adk/handler.go:139；基类 :277）。
// 覆盖 WrapInvokableToolCall 与 WrapEnhancedInvokableToolCall（:193 与 :217）：
// read_file 是 enhanced 工具，只覆盖前者的话它就绕过了路径规则。
type filesystemHandler struct {
	adk.BaseChatModelAgentMiddleware
	options Options
}

// 编译期校验 filesystemHandler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*filesystemHandler)(nil)

// piiHandler 是出口脱敏：模型输入和工具输出各过一遍打码。
//
// 实现接口：adk.ChatModelAgentMiddleware（同上，eino/adk/handler.go:139；基类 :277）。
// 覆盖 BeforeModelRewriteState 与两个工具包裹钩子（:193 与 :217）。
type piiHandler struct {
	adk.BaseChatModelAgentMiddleware
}

// 编译期校验 piiHandler 实现了 adk.ChatModelAgentMiddleware。
var _ adk.ChatModelAgentMiddleware = (*piiHandler)(nil)

// Handlers 是 L1 的三件套，顺序就是包裹顺序：命令守卫在外，文件守卫居中，PII 最贴工具。
func Handlers(engine *Engine, fs Options) []adk.ChatModelAgentMiddleware {
	return []adk.ChatModelAgentMiddleware{
		&toolGuardHandler{engine: engine},
		&filesystemHandler{options: fs},
		&piiHandler{},
	}
}

// verdict 是守卫对一次工具调用的判定：放行（args 是真正要执行的参数，续跑时是
// 当初保存的那份）、不执行工具只把 deny 这句话回给模型、或者用 err 暂停/报错。
// 字符串工具和 enhanced 工具共用这一份判定，两条包裹路径才不会走出两种行为。
type verdict struct {
	args string
	deny string
	err  error
}

// textResult 把一句话包成 enhanced 工具的回执。
func textResult(text string) *schema.ToolResult {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: text}}}
}

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall
// 钩子（github.com/cloudwego/eino/adk/handler.go:193）：框架在每次工具即将执行时调用它，
// 传入真执行的 endpoint、接收包过一层的新 endpoint——守卫就是靠这个挂进工具执行链。
// 本实现借这层拦截：先认自己那笔中断的续跑，再扫规则决定放行、打断还是拒绝；
// 批准后的续跑执行保存下来的原始参数，拒绝则不执行工具、只回一句拒绝说明。
func (h *toolGuardHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		switch v := h.decide(ctx, tCtx.Name, args); {
		case v.err != nil:
			return "", v.err
		case v.deny != "":
			return v.deny, nil
		default:
			return endpoint(ctx, v.args, opts...)
		}
	}, nil
}

// WrapEnhancedInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的同名钩子
// （github.com/cloudwego/eino/adk/handler.go:217）：只有实现 tool.EnhancedInvokableTool
// 的工具会走到这里，而 compose 的 ToolsNode 对同时实现两种接口的工具**优先挑 enhanced**
// （compose/tool_node.go:638）。read_file 要把图片以图像片段回给模型，用的正是这个接口；
// 不覆盖这个钩子，命令守卫和人工审批会被整个绕过。
func (h *toolGuardHandler) WrapEnhancedInvokableToolCall(_ context.Context, endpoint adk.EnhancedInvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, arg *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		args := argumentText(arg)
		switch v := h.decide(ctx, tCtx.Name, args); {
		case v.err != nil:
			return nil, v.err
		case v.deny != "":
			return textResult(v.deny), nil
		default:
			return endpoint(ctx, &schema.ToolArgument{Text: v.args}, opts...)
		}
	}, nil
}

// decide 是命令守卫的判定本体：先认自己那笔中断的续跑，再扫规则决定放行、暂停还是拒绝。
func (h *toolGuardHandler) decide(ctx context.Context, name, args string) verdict {
	if stored, ok := ownInterrupt(ctx, kindToolGuard); ok {
		decision, decided := resumeDecision(ctx)
		if !decided {
			return verdict{err: interruptCall(ctx, kindToolGuard, stored)}
		}
		if decision != ResumeApprove {
			return verdict{deny: fmt.Sprintf("用户拒绝了这次 %s 调用。不要重试相同命令；如需类似操作请先与用户商量替代方案。", name)}
		}
		return verdict{args: stored.Args}
	}
	findings := h.engine.Guard(name, parseArgs(args))
	if len(findings) == 0 {
		return verdict{args: args}
	}
	if h.engine.Mode() == ModeWarn {
		fmt.Printf("\n⚠️  [tool_guard] %s\n\n", formatFindings(findings, "检测到风险参数（warn 模式，放行）"))
		return verdict{args: args}
	}
	if h.engine.Mode() == ModeApprove && h.engine.ShouldBlock(findings) {
		info := approvalInfo(h.engine, name, args)
		return verdict{err: compose.StatefulInterrupt(ctx, info, savedCall{
			Kind: kindToolGuard, Tool: name, Args: args, Message: info.Message,
		})}
	}
	if h.engine.ShouldBlock(findings) {
		return verdict{deny: formatFindings(findings, "工具调用被安全策略拦截（command guard）")}
	}
	return verdict{args: args}
}

// argumentText 取出 enhanced 工具的参数 JSON。框架保证非 nil（adk/wrappers.go:216），
// 但这里不靠这个保证：参数为空时守卫按空参数判，总比 panic 好。
func argumentText(arg *schema.ToolArgument) string {
	if arg == nil {
		return ""
	}
	return arg.Text
}

const (
	kindToolGuard = "tool_guard"
	kindFSGuard   = "fs_guard"
)

// ownInterrupt 判断这次恢复是不是自己之前那次中断（Kind 用来分辨嵌套守卫里的归属）。
func ownInterrupt(ctx context.Context, kind string) (savedCall, bool) {
	was, has, stored := compose.GetInterruptState[savedCall](ctx)
	if !was || !has || stored.Kind != kind {
		return savedCall{}, false
	}
	return stored, true
}

// interruptCall 用保存的载荷重新暂停，把人的决定留给下一次 Resume。
func interruptCall(ctx context.Context, kind string, stored savedCall) error {
	return compose.StatefulInterrupt(ctx, ApprovalInfo{
		Kind: kind, Tool: stored.Tool, Args: stored.Args, Message: stored.Message,
	}, stored)
}

// approvalInfo 拼一份给 REPL 看的审批载荷。
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

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall
// 钩子（github.com/cloudwego/eino/adk/handler.go:193，与命令守卫同一钩子）：框架在每次
// 工具即将执行时调用它并传入真执行 endpoint。本实现借它按路径规则放行、问人或硬拒；
// 问人时用 StatefulInterrupt 暂停整轮，用户拒绝就返回拒绝文案而不执行工具。
func (h *filesystemHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		switch v := h.decide(ctx, tCtx.Name, args); {
		case v.err != nil:
			return "", v.err
		case v.deny != "":
			return v.deny, nil
		default:
			return endpoint(ctx, v.args, opts...)
		}
	}, nil
}

// WrapEnhancedInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的同名钩子
// （github.com/cloudwego/eino/adk/handler.go:217）：read_file 是 enhanced 工具
// （要回图片），只会走这个钩子。少了它，read_file 就能读工作区外和 .env 之类的敏感文件。
func (h *filesystemHandler) WrapEnhancedInvokableToolCall(_ context.Context, endpoint adk.EnhancedInvokableToolCallEndpoint, tCtx *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, arg *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		args := argumentText(arg)
		switch v := h.decide(ctx, tCtx.Name, args); {
		case v.err != nil:
			return nil, v.err
		case v.deny != "":
			return textResult(v.deny), nil
		default:
			return endpoint(ctx, &schema.ToolArgument{Text: v.args}, opts...)
		}
	}, nil
}

// decide 是路径守卫的判定本体：先认自己那笔中断的续跑，再按路径规则放行、问人或硬拒。
func (h *filesystemHandler) decide(ctx context.Context, name, args string) verdict {
	if stored, ok := ownInterrupt(ctx, kindFSGuard); ok {
		decision, decided := resumeDecision(ctx)
		if !decided {
			return verdict{err: interruptCall(ctx, kindFSGuard, stored)}
		}
		if decision != ResumeApprove {
			return verdict{deny: fmt.Sprintf("用户拒绝了这次 %s 调用。不要重试相同路径。", name)}
		}
		return verdict{args: stored.Args}
	}
	decision := Decide(h.options, name, parseArgs(args))
	switch decision.Action {
	case Allow:
		return verdict{args: args}
	case Ask:
		info := ApprovalInfo{
			Kind:    "fs_guard_approval",
			Tool:    name,
			Args:    args,
			Message: "文件访问需要人工批准：" + decision.Reason,
		}
		return verdict{err: compose.StatefulInterrupt(ctx, info, savedCall{
			Kind: kindFSGuard, Tool: name, Args: args, Message: info.Message,
		})}
	default:
		return verdict{deny: "Error: permission denied —— " + decision.Reason}
	}
}

// BeforeModelRewriteState 实现 adk.ChatModelAgentMiddleware 的 BeforeModelRewriteState
// 钩子（github.com/cloudwego/eino/adk/handler.go:171）：框架在每次模型调用前调用，
// 返回的 state 会被持久化并作为模型输入，是官方推荐改消息/工具表的位置。
// 本实现借它把历史里的密钥打码后再给模型。RewriteState 钩子官方
// 允许就地改 state（chatmodel.go 的生命周期注释："can modify state"）；注意
// 压缩 Handler 注册在本钩子前面，返回的是新的消息对象。这里改的是那些副本的 Content，
// modelMsgs 和 event_log 里已经写下的原文不是这一批。没有装压缩 Handler 时，
// state.Messages 仍可能和调用方历史共享指针，改写会写到那些对象上。
func (h *piiHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	for _, msg := range state.Messages {
		if msg == nil || msg.Content == "" {
			continue
		}
		msg.Content = MaskSecrets(msg.Content)
	}
	return ctx, state, nil
}

// WrapInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的 WrapInvokableToolCall
// 钩子（github.com/cloudwego/eino/adk/handler.go:193，与命令守卫同一钩子）：框架在每次
// 工具即将执行时调用它。本实现借它在 endpoint 返回之后把工具输出里的密钥打码，
// 再交回给模型——只改模型看到的内容，不碰工具真实执行结果。
func (h *piiHandler) WrapInvokableToolCall(_ context.Context, endpoint adk.InvokableToolCallEndpoint, _ *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		out, err := endpoint(ctx, args, opts...)
		if err != nil {
			return "", err
		}
		return MaskSecrets(out), nil
	}, nil
}

// WrapEnhancedInvokableToolCall 实现 adk.ChatModelAgentMiddleware 的同名钩子
// （github.com/cloudwego/eino/adk/handler.go:217）：enhanced 工具的回执不走上面那条，
// read_file 的文本正好在回执片段里，漏了这个钩子它就不打码了。
func (h *piiHandler) WrapEnhancedInvokableToolCall(_ context.Context, endpoint adk.EnhancedInvokableToolCallEndpoint, _ *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, arg *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		out, err := endpoint(ctx, arg, opts...)
		if err != nil {
			return nil, err
		}
		return maskResult(out), nil
	}, nil
}

// maskResult 打码回执里的文本片段，返回新的 ToolResult，不改工具真实执行结果。
// 图片之类的二进制片段原样留着：MaskSecrets 是文本规则，拿去扫 base64 会误伤
// （base64 里迟早出现形似密钥的串），而且改一个字节整张图就废了。
func maskResult(result *schema.ToolResult) *schema.ToolResult {
	if result == nil || len(result.Parts) == 0 {
		return result
	}
	out := *result
	out.Parts = append([]schema.ToolOutputPart(nil), result.Parts...)
	for i := range out.Parts {
		if out.Parts[i].Type == schema.ToolPartTypeText {
			out.Parts[i].Text = MaskSecrets(out.Parts[i].Text)
		}
	}
	return &out
}

// resumeDecision 读这次 Resume 带回来的人的决定。
func resumeDecision(ctx context.Context) (string, bool) {
	isTarget, hasData, data := compose.GetResumeContext[string](ctx)
	if !isTarget || !hasData {
		return "", false
	}
	return data, true
}

// parseArgs 把工具参数 JSON 解成 map；解不开就原样放进 _raw，规则照样能扫到。
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

// formatFindings 把命中清单排成人读的几行，最多列 5 条。
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
