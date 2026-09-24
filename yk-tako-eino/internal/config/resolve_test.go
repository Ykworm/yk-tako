package config

import "testing"

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

func TestResolveRequiresKey(t *testing.T) {
	if _, err := ResolveModel(Env{}); err == nil {
		t.Fatal("expected missing provider error")
	}
}
