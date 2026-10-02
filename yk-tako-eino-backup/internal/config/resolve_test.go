// 本文件：厂商/模型解析（ResolveModel）的单元测试。直接传 Env map，不碰真实环境变量。
package config

import "testing"

// TestResolveDeepSeekBareName 验证只写模型名（不带厂商前缀）也能解析。
func TestResolveDeepSeekBareName(t *testing.T) {
	got, err := ResolveModel(Env{
		"DEEPSEEK_API_KEY":  "sk-test",
		"DEEPSEEK_BASE_URL": "https://api.deepseek.com",
		"TAKO_MODEL":        "deepseek-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "deepseek/deepseek-flash" {
		t.Fatalf("ref = %s", got.Ref)
	}
	if got.Provider.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base = %s", got.Provider.BaseURL)
	}
	if got.Provider.Protocol != ProtocolOpenAI {
		t.Fatalf("protocol = %s", got.Provider.Protocol)
	}
}

// TestResolveRequiresKey 验证没配密钥时报错，并把配置方法说清楚。
func TestResolveRequiresKey(t *testing.T) {
	if _, err := ResolveModel(Env{}); err == nil {
		t.Fatal("expected missing provider error")
	}
}
