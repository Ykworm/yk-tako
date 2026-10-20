package config

import "testing"

func TestCatalogVision(t *testing.T) {
	preset, ok := findPreset("deepseek")
	if !ok {
		t.Fatal("目录应有 deepseek 厂商")
	}
	var flash, pro bool
	var flashVision, proVision bool
	for _, model := range preset.Models {
		switch model.ID {
		case "deepseek-flash":
			flash, flashVision = true, model.Vision
		case "deepseek-v4-pro":
			pro, proVision = true, model.Vision
		case "deepseek-v4-flash":
			t.Fatal("官方 deepseek 条目的模型 id 应是 deepseek-flash")
		}
	}
	if !flash || !flashVision {
		t.Fatal("deepseek-flash 应在目录里且 vision=true")
	}
	if !pro || proVision {
		t.Fatal("deepseek-v4-pro 应在目录里且 vision 缺省 false")
	}

	env := Env{"DEEPSEEK_API_KEY": "sk-test"}
	got, err := ResolveModelWithRef(env, "deepseek/deepseek-flash")
	if err != nil || !got.Vision || got.Ref != "deepseek/deepseek-flash" {
		t.Fatalf("deepseek/deepseek-flash: vision=%v ref=%s err=%v", got.Vision, got.Ref, err)
	}
	got, err = ResolveModelWithRef(env, "deepseek/deepseek-v4-pro")
	if err != nil || got.Vision {
		t.Fatalf("deepseek/deepseek-v4-pro 不应标 vision: vision=%v err=%v", got.Vision, err)
	}
	got, err = ResolveModelWithRef(env, "deepseek/not-in-catalog")
	if err != nil || got.Vision {
		t.Fatalf("目录里没有的模型 id 应为 vision=false: vision=%v err=%v", got.Vision, err)
	}
	other, ok := findPreset("mimo")
	if !ok || len(other.Models) == 0 || other.Models[0].Vision {
		t.Fatal("没写 vision 字段的模型应为 false")
	}
}
