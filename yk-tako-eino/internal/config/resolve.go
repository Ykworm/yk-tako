package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Env 是进程环境的快照。测试直接传 map，不碰真正的环境变量。
type Env map[string]string

// ResolvedProvider 是解析后的一家厂商：id、协议、地址、密钥、可选模型都齐了。
type ResolvedProvider struct {
	ID       string
	Name     string
	Protocol ProtocolName
	BaseURL  string
	APIKey   string
	Models   []ModelPreset
}

// ResolvedModel 是这次要用的模型：厂商加模型 id，Ref 是能打印的 厂商/模型。
type ResolvedModel struct {
	// Ref 带厂商前缀。模型 id 本身也可以含 /。
	Ref      string
	ModelID  string
	Provider ResolvedProvider
}

// customKey 认出 TAKO_PROVIDER_<名字>_API_KEY 这类自定义厂商密钥变量。
var customKey = regexp.MustCompile(`^TAKO_PROVIDER_([A-Z0-9]+)_API_KEY$`)

// envToken 把厂商 id 转成环境变量里的大写下划线形式。
func envToken(providerID string) string {
	return strings.ToUpper(strings.ReplaceAll(providerID, "-", "_"))
}

// presetForEnvName 拿变量里的名字去目录里找厂商，下划线和短横线都认。
func presetForEnvName(raw string) (ProviderPreset, bool) {
	lower := strings.ToLower(raw)
	if preset, ok := findPreset(lower); ok {
		return preset, true
	}
	return findPreset(strings.ReplaceAll(lower, "_", "-"))
}

// baseURLFor 定 baseURL：专用变量优先，其次厂商级变量，最后目录默认值。
func baseURLFor(env Env, preset *ProviderPreset, rawEnvName string) string {
	if specific := strings.TrimSpace(env["TAKO_PROVIDER_"+rawEnvName+"_BASE_URL"]); specific != "" {
		return specific
	}
	if preset == nil {
		return ""
	}
	// 一个 Key 只对应一个厂商时，才认 DEEPSEEK_BASE_URL 这种变量。
	// 一个 Key 挂多个地域时，共用变量会把各自的地址冲掉。
	if preset.APIKeyEnv != "" {
		var siblings int
		for _, item := range providerPresets {
			if item.APIKeyEnv == preset.APIKeyEnv {
				siblings++
			}
		}
		if siblings == 1 {
			stem := preset.APIKeyEnv[:len(preset.APIKeyEnv)-len("API_KEY")] + "BASE_URL"
			if vendor := strings.TrimSpace(env[stem]); vendor != "" {
				return vendor
			}
		}
	}
	return preset.BaseURL
}

// protocolFor 定协议：专用变量优先，其次目录预设，最后按 openai 兼容处理。
func protocolFor(env Env, preset *ProviderPreset, rawEnvName string) (ProtocolName, error) {
	raw := strings.ToLower(strings.TrimSpace(env["TAKO_PROVIDER_"+rawEnvName+"_PROTOCOL"]))
	switch ProtocolName(raw) {
	case ProtocolOpenAI, ProtocolAnthropic:
		return ProtocolName(raw), nil
	case "":
		if preset != nil {
			return preset.Protocol, nil
		}
		return ProtocolOpenAI, nil
	default:
		return "", fmt.Errorf("不支持的协议 %s。当前只接 openai（含 OpenAI 兼容接口）和 anthropic", raw)
	}
}

// discoverProviders 扫环境变量里的各家 API Key，拼出已配置的厂商（按变量名排序，结果稳定）。
func discoverProviders(env Env) ([]ResolvedProvider, error) {
	byID := map[string]ResolvedProvider{}
	var order []string
	add := func(item ResolvedProvider) {
		if _, ok := byID[item.ID]; !ok {
			order = append(order, item.ID)
		}
		byID[item.ID] = item
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		matched := customKey.FindStringSubmatch(key)
		apiKey := strings.TrimSpace(env[key])
		if matched == nil || apiKey == "" {
			continue
		}
		raw := matched[1]
		preset, ok := presetForEnvName(raw)
		var presetPtr *ProviderPreset
		id := strings.ToLower(raw)
		name := id
		if ok {
			presetPtr = &preset
			id = preset.ID
			name = preset.Name
		}
		baseURL := baseURLFor(env, presetPtr, raw)
		if baseURL == "" {
			return nil, fmt.Errorf("设置了 %s，但没有 base URL。补上 TAKO_PROVIDER_%s_BASE_URL，或改用目录里已有厂商的 Key 变量", key, raw)
		}
		protocol, err := protocolFor(env, presetPtr, raw)
		if err != nil {
			return nil, err
		}
		models := []ModelPreset{}
		if ok {
			models = preset.Models
		}
		add(ResolvedProvider{ID: id, Name: name, Protocol: protocol, BaseURL: baseURL, APIKey: apiKey, Models: models})
	}

	for _, preset := range providerPresets {
		if preset.APIKeyEnv == "" {
			continue
		}
		if _, ok := byID[preset.ID]; ok {
			continue
		}
		apiKey := strings.TrimSpace(env[preset.APIKeyEnv])
		if apiKey == "" {
			continue
		}
		baseURL := baseURLFor(env, &preset, envToken(preset.ID))
		if baseURL == "" {
			continue
		}
		protocol, err := protocolFor(env, &preset, envToken(preset.ID))
		if err != nil {
			return nil, err
		}
		add(ResolvedProvider{
			ID: preset.ID, Name: preset.Name, Protocol: protocol,
			BaseURL: baseURL, APIKey: apiKey, Models: preset.Models,
		})
	}

	out := make([]ResolvedProvider, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// withOllama 处理本地 Ollama：只有 TAKO_MODEL 指到 ollama/ 时才补这条厂商。
func withOllama(providers []ResolvedProvider, env Env, spec string) []ResolvedProvider {
	for _, provider := range providers {
		if provider.ID == "ollama" {
			return providers
		}
	}
	preset, ok := findPreset("ollama")
	if !ok {
		return providers
	}
	explicit := strings.TrimSpace(env["TAKO_PROVIDER_OLLAMA_BASE_URL"])
	if explicit == "" {
		explicit = strings.TrimSpace(env["OLLAMA_BASE_URL"])
	}
	host := strings.TrimSpace(env["OLLAMA_HOST"])
	wants := explicit != "" || host != "" || strings.HasPrefix(strings.ToLower(spec), "ollama/")
	if !wants {
		return providers
	}
	baseURL := preset.BaseURL
	switch {
	case explicit != "":
		baseURL = explicit
	case host != "":
		host = strings.TrimRight(host, "/")
		if strings.HasSuffix(host, "/v1") {
			baseURL = host
		} else {
			baseURL = host + "/v1"
		}
	}
	apiKey := strings.TrimSpace(env["OLLAMA_API_KEY"])
	if apiKey == "" {
		apiKey = "ollama"
	}
	return append(providers, ResolvedProvider{
		ID: "ollama", Name: preset.Name, Protocol: ProtocolOpenAI,
		BaseURL: baseURL, APIKey: apiKey, Models: preset.Models,
	})
}

// pack 把厂商和模型 id 打包成解析结果。
func pack(provider ResolvedProvider, modelID string) ResolvedModel {
	return ResolvedModel{Provider: provider, ModelID: modelID, Ref: provider.ID + "/" + modelID}
}

// exampleRef 给报错信息造一个当前就能用的 TAKO_MODEL 例子。
func exampleRef(provider ResolvedProvider) string {
	modelID := "模型名"
	if len(provider.Models) > 0 {
		modelID = provider.Models[0].ID
	}
	return provider.ID + "/" + modelID
}

// requireProvider 取指定厂商，取不到时按「没配密钥」和「没这家厂商」分别给提示。
func requireProvider(providers []ResolvedProvider, id string) (ResolvedProvider, error) {
	for _, provider := range providers {
		if provider.ID == id {
			return provider, nil
		}
	}
	configured := joinIDs(providers)
	if preset, ok := findPreset(id); ok {
		if preset.APIKeyEnv != "" {
			return ResolvedProvider{}, fmt.Errorf("厂商 %s 还没有凭据。设置 %s。已配置：%s", id, preset.APIKeyEnv, configured)
		}
		return ResolvedProvider{}, fmt.Errorf("厂商 %s 还没有凭据。已配置：%s", id, configured)
	}
	return ResolvedProvider{}, fmt.Errorf("未知厂商 %s。已配置：%s", id, configured)
}

// joinIDs 把厂商 id 连成一句人读的列表。
func joinIDs(providers []ResolvedProvider) string {
	ids := make([]string, len(providers))
	for i, provider := range providers {
		ids[i] = provider.ID
	}
	return strings.Join(ids, "、")
}

// ResolveModel 从环境变量选出当前要用的厂商和模型。
func ResolveModel(env Env) (ResolvedModel, error) {
	spec := strings.TrimSpace(env["TAKO_MODEL"])
	providers, err := discoverProviders(env)
	if err != nil {
		return ResolvedModel{}, err
	}
	providers = withOllama(providers, env, spec)
	if len(providers) == 0 {
		return ResolvedModel{}, fmt.Errorf("没有可用的模型厂商。在 .env 里设置 API Key，例如 DEEPSEEK_API_KEY，再用 TAKO_MODEL=deepseek/deepseek-v4-flash 选择模型")
	}
	if spec == "" {
		if len(providers) != 1 {
			return ResolvedModel{}, fmt.Errorf("配置了多个厂商（%s）。请设置 TAKO_MODEL=厂商/模型，例如 %s", joinIDs(providers), exampleRef(providers[0]))
		}
		only := providers[0]
		if len(only.Models) == 0 {
			return ResolvedModel{}, fmt.Errorf("厂商 %s 没有默认模型。请设置 TAKO_MODEL=%s/模型名", only.ID, only.ID)
		}
		return pack(only, only.Models[0].ID), nil
	}
	if slash := strings.Index(spec, "/"); slash > 0 {
		providerID := strings.ToLower(spec[:slash])
		modelID := strings.TrimSpace(spec[slash+1:])
		if modelID == "" {
			return ResolvedModel{}, fmt.Errorf("TAKO_MODEL=%s 缺少模型名。格式是 厂商/模型", spec)
		}
		provider, err := requireProvider(providers, providerID)
		if err != nil {
			return ResolvedModel{}, err
		}
		return pack(provider, modelID), nil
	}
	var matches []ResolvedProvider
	for _, provider := range providers {
		for _, model := range provider.Models {
			if model.ID == spec {
				matches = append(matches, provider)
				break
			}
		}
	}
	switch len(matches) {
	case 1:
		return pack(matches[0], spec), nil
	case 0:
		if len(providers) == 1 {
			return pack(providers[0], spec), nil
		}
		return ResolvedModel{}, fmt.Errorf("模型 %s 不在已配置厂商的目录里。请写成 厂商/模型。当前厂商：%s", spec, joinIDs(providers))
	default:
		refs := make([]string, len(matches))
		for i, provider := range matches {
			refs[i] = provider.ID + "/" + spec
		}
		return ResolvedModel{}, fmt.Errorf("模型 %s 在多个厂商里都有：%s。请把 TAKO_MODEL 写成带厂商前缀的那一个", spec, strings.Join(refs, "、"))
	}
}

// Environ 读取当前进程的环境变量。
func Environ() Env {
	env := Env{}
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}
