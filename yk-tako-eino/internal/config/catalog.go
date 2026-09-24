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

type ModelPreset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ProviderPreset struct {
	ID        string
	Name      string
	Protocol  ProtocolName
	BaseURL   string
	APIKeyEnv string
	Models    []ModelPreset
}

type catalogItem struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Protocol  string        `json:"protocol"`
	BaseURL   string        `json:"baseURL"`
	APIKeyEnv string        `json:"apiKeyEnv"`
	Models    []ModelPreset `json:"models"`
}

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

func asProtocol(value, id string) (ProtocolName, error) {
	switch ProtocolName(value) {
	case ProtocolOpenAI, ProtocolAnthropic:
		return ProtocolName(value), nil
	default:
		return "", fmt.Errorf("厂商 %s 的协议 %s 不支持", id, value)
	}
}

var providerPresets = mustPresets()

func mustPresets() []ProviderPreset {
	presets, err := loadPresets()
	if err != nil {
		panic(err)
	}
	return presets
}

func findPreset(id string) (ProviderPreset, bool) {
	for _, preset := range providerPresets {
		if preset.ID == id {
			return preset, true
		}
	}
	return ProviderPreset{}, false
}
