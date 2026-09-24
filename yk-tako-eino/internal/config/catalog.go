package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed provider-catalog.json
var catalogJSON []byte

// ProtocolName 只有两种。openai 覆盖所有 OpenAI 兼容接口；anthropic 走 Anthropic 自己的接口。
type ProtocolName string

const (
	ProtocolOpenAI    ProtocolName = "openai"
	ProtocolAnthropic ProtocolName = "anthropic"
)

// ModelPreset 是目录里的一条模型条目。
type ModelPreset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProviderPreset 是目录里的一家厂商：协议、默认 baseURL、密钥环境变量名、可选模型。
type ProviderPreset struct {
	ID        string
	Name      string
	Protocol  ProtocolName
	BaseURL   string
	APIKeyEnv string
	Models    []ModelPreset
}

// catalogItem 是 provider-catalog.json 的原始条目，Protocol 这时还是字符串。
type catalogItem struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Protocol  string        `json:"protocol"`
	BaseURL   string        `json:"baseURL"`
	APIKeyEnv string        `json:"apiKeyEnv"`
	Models    []ModelPreset `json:"models"`
}

// loadPresets 把内嵌的 JSON 目录解成预设，协议名在这里校验。
func loadPresets() ([]ProviderPreset, error) {
	var items []catalogItem
	if err := json.Unmarshal(catalogJSON, &items); err != nil {
		return nil, fmt.Errorf("解析厂商目录: %w", err)
	}
	presets := make([]ProviderPreset, 0, len(items))
	for _, item := range items {
		protocol, err := asProtocol(item.Protocol, item.ID)
		if err != nil {
			return nil, err
		}
		presets = append(presets, ProviderPreset{
			ID:        item.ID,
			Name:      item.Name,
			Protocol:  protocol,
			BaseURL:   item.BaseURL,
			APIKeyEnv: item.APIKeyEnv,
			Models:    item.Models,
		})
	}
	return presets, nil
}

// asProtocol 校验协议名，只认 openai 和 anthropic。
func asProtocol(value, id string) (ProtocolName, error) {
	switch ProtocolName(value) {
	case ProtocolOpenAI, ProtocolAnthropic:
		return ProtocolName(value), nil
	default:
		return "", fmt.Errorf("厂商 %s 的协议 %s 不支持", id, value)
	}
}

// providerPresets 是进程启动时加载一次的厂商目录，之后只读。
var providerPresets = mustPresets()

// mustPresets 启动时加载目录；目录坏了直接 panic，不留到运行期。
func mustPresets() []ProviderPreset {
	presets, err := loadPresets()
	if err != nil {
		panic(err)
	}
	return presets
}

// findPreset 按 id 找厂商预设。
func findPreset(id string) (ProviderPreset, bool) {
	for _, preset := range providerPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return ProviderPreset{}, false
}
