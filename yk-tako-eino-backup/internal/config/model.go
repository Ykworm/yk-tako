// 本文件：身份常量（人设、主 agent 名字）与按厂商目录建 Eino ChatModel
// （Open / OpenForForcedToolChoice）。
// 返回值实现 model.ToolCallingChatModel 接口——github.com/cloudwego/eino/components/model
// 包 interface.go:99（eino v0.9.21）；具体实现来自 eino-ext 的 openai / claude 组件。
package config

import (
	"context"
	"fmt"
	"os"
	"strings"

	claude "github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/joho/godotenv"
)

const systemPrompt = "你是 yk-tako，一个正在成长中的 AI 工作伙伴。回答简洁、说中文。"

// SystemPrompt 是三个 L0 脚本和 L1 共用的人设。
func SystemPrompt() string { return systemPrompt }

// agentName 是主 agent 的标识。adk 把它写进每个事件（TypedChatModelAgent.Name，
// eino/adk/chatmodel.go:680），adkrun 靠它分辨「哪条消息是自己说的、谁算子代理」。
// 四个入口共用这一份：以前各脚本手写字符串，一旦某处改了名字，
// 另一处不会报错，只是历史变空——所以收在这里，由 AgentName 统一暴露。
const agentName = "yk-tako"

// AgentName 是主 agent 的名字，给 ChatModelAgentConfig.Name / deep.Config.Name 用。
func AgentName() string { return agentName }

// LoadDotEnv 读取 .env。在 yk-tako-eino 目录里运行时读当前目录；在仓库根运行时再试 yk-tako-eino/.env。
func LoadDotEnv() {
	if err := godotenv.Load(); err == nil {
		return
	}
	_ = godotenv.Load("yk-tako-eino/.env")
}

// Open 按 TAKO_MODEL 建成 Eino 的 ToolCallingChatModel。
// openai 协议（含 DeepSeek）走 openai.ChatModel，只换 BaseURL；anthropic 走 claude.ChatModel。
func Open(ctx context.Context) (model.ToolCallingChatModel, string, error) {
	return open(ctx, false)
}

// OpenForForcedToolChoice 给"强制 tool_choice"的场景建模：planexecute 的 planner/replanner
// 靠 ToolChoiceForced 让模型只输出结构化的计划/答复。DeepSeek 系在 thinking 模式下
// 拒绝强制 tool_choice（400: Thinking mode does not support this tool_choice），
// 所以要关掉 thinking——输出反正只是工具参数 JSON，思考对它是纯开销。
// anthropic 原生支持强制 tool_choice，无需处理，行为与 Open 一致。
func OpenForForcedToolChoice(ctx context.Context) (model.ToolCallingChatModel, string, error) {
	return open(ctx, true)
}

// open 是 Open 和 OpenForForcedToolChoice 的共同入口：读环境、解析厂商模型、建客户端。
func open(ctx context.Context, forForcedToolChoice bool) (model.ToolCallingChatModel, string, error) {
	LoadDotEnv()
	resolved, err := ResolveModel(Environ())
	if err != nil {
		return nil, "", err
	}
	chatModel, err := build(ctx, resolved, forForcedToolChoice)
	if err != nil {
		return nil, "", err
	}
	return chatModel, resolved.Ref, nil
}

// build 按协议建客户端：anthropic 走 claude，openai 兼容走 openai，只换 baseURL。
func build(ctx context.Context, resolved ResolvedModel, forForcedToolChoice bool) (model.ToolCallingChatModel, error) {
	temp := float32(0)
	provider := resolved.Provider
	switch provider.Protocol {
	case ProtocolAnthropic:
		base := strings.TrimRight(provider.BaseURL, "/")
		base = strings.TrimSuffix(base, "/v1")
		cfg := &claude.Config{
			APIKey:      provider.APIKey,
			Model:       resolved.ModelID,
			Temperature: &temp,
		}
		if base != "" {
			cfg.BaseURL = &base
		}
		return claude.NewChatModel(ctx, cfg)
	case ProtocolOpenAI:
		cfg := &openai.ChatModelConfig{
			APIKey:      provider.APIKey,
			BaseURL:     provider.BaseURL,
			Model:       resolved.ModelID,
			Temperature: &temp,
		}
		// DeepSeek 的 thinking 模式不支持强制 tool_choice，关掉。
		// thinking 字段是 DeepSeek 私有扩展，OpenAI 兼容服务对未知字段可能直接 400，
		// 所以只对它注入；以后碰到别家"思考模式拒强制 tool_choice"的，在这里加自己的开关。
		if forForcedToolChoice && provider.ID == "deepseek" {
			cfg.ExtraFields = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		}
		return openai.NewChatModel(ctx, cfg)
	default:
		return nil, fmt.Errorf("不支持的协议 %s", provider.Protocol)
	}
}

// MustOpen 在 main 里用。配置错误直接退出，避免每个脚本再写一遍。
func MustOpen(ctx context.Context) (model.ToolCallingChatModel, string) {
	return must(Open(ctx))
}

// MustOpenForForcedToolChoice 是 MustOpen 的强制 tool_choice 版本，见 OpenForForcedToolChoice。
func MustOpenForForcedToolChoice(ctx context.Context) (model.ToolCallingChatModel, string) {
	return must(OpenForForcedToolChoice(ctx))
}

// must 把「建模型失败」压成脚本级退出，省得每个 main 重复写。
func must(chatModel model.ToolCallingChatModel, ref string, err error) (model.ToolCallingChatModel, string) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return chatModel, ref
}
