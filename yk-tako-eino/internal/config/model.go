// 本文件：按厂商目录建 Eino ChatModel。
// 返回值实现 model.ToolCallingChatModel 接口——github.com/cloudwego/eino/components/model
// 包 interface.go（eino v0.9.21）；具体实现来自 eino-ext 的 openai / claude 组件。
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

// SystemPrompt 是 run worker 组模型侧消息时用的系统消息。
func SystemPrompt() string { return systemPrompt }

// LoadDotEnv 读取 .env。在 yk-tako-eino/ 里运行时读当前目录，再试仓库根的 ../.env。
func LoadDotEnv() {
	for _, path := range []string{".env", "../.env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}

// ChatModel 是 Open 的成品：模型 + 协议 + 可打印的 厂商/模型。协议决定 worker 用哪个 Translator（runengine.TranslatorFor 分派）。
type ChatModel struct {
	Chat     model.ToolCallingChatModel
	Protocol ProtocolName
	Ref      string
}

// Open 按 TAKO_MODEL 建成 Eino 的 ToolCallingChatModel。
// openai 协议（含 DeepSeek 等兼容接口）走 openai.ChatModel，只换 BaseURL；anthropic 走 claude.ChatModel。
// 调用方负责失败即退出（main 里 fail fast，不留半个起不来的引擎）。
func Open(ctx context.Context) (*ChatModel, error) {
	LoadDotEnv()
	resolved, err := ResolveModel(Environ())
	if err != nil {
		return nil, err
	}
	chatModel, err := build(ctx, resolved)
	if err != nil {
		return nil, err
	}
	return &ChatModel{Chat: chatModel, Protocol: resolved.Provider.Protocol, Ref: resolved.Ref}, nil
}

// build 按协议建客户端：anthropic 走 claude，openai 兼容走 openai，只换 baseURL。
func build(ctx context.Context, resolved ResolvedModel) (model.ToolCallingChatModel, error) {
	temp := float32(0)
	switch resolved.Provider.Protocol {
	case ProtocolAnthropic:
		base := strings.TrimRight(resolved.Provider.BaseURL, "/")
		base = strings.TrimSuffix(base, "/v1")
		cfg := &claude.Config{
			APIKey:      resolved.Provider.APIKey,
			Model:       resolved.ModelID,
			Temperature: &temp,
		}
		if base != "" {
			cfg.BaseURL = &base
		}
		return claude.NewChatModel(ctx, cfg)
	case ProtocolOpenAI:
		cfg := &openai.ChatModelConfig{
			APIKey:      resolved.Provider.APIKey,
			BaseURL:     resolved.Provider.BaseURL,
			Model:       resolved.ModelID,
			Temperature: &temp,
		}
		return openai.NewChatModel(ctx, cfg)
	default:
		return nil, fmt.Errorf("不支持的协议 %s", resolved.Provider.Protocol)
	}
}

// must 把「建模型失败」压成进程级退出，省得每个入口重复写。
func MustOpen(ctx context.Context) *ChatModel {
	opened, err := Open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return opened
}
