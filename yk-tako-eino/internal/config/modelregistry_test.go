package config

import (
	"context"
	"strings"
	"testing"
)

// ModelRegistry 的三条不变式（T3-1b 验收③）：同 ref 二次 Get 命中同一实例（缓存不重建）、
// 不同 ref 各建各的、解析失败不入缓存。走真解析链（内嵌目录 + 测试 Key），不触网——
// build 只装配 HTTP 客户端，第一次真请求发生在模型调用时。
func TestModelRegistryGetCaches(t *testing.T) {
	env := Env{"DEEPSEEK_API_KEY": "sk-test"}
	r := NewModelRegistry(env)
	ctx := context.Background()

	a1, err := r.Get(ctx, "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if a1.Ref != "deepseek/deepseek-v4-flash" || a1.Protocol != ProtocolOpenAI {
		t.Fatalf("成品 = {ref:%s proto:%s}, want 规范 ref + openai", a1.Ref, a1.Protocol)
	}
	a2, err := r.Get(ctx, "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Fatal("同 ref 两次 Get 应复用同一实例（缓存命中不重建）")
	}

	// trim 后同 ref 命中同一条目（缓存 key = trim 后原文）。
	a3, err := r.Get(ctx, "  deepseek/deepseek-v4-flash  ")
	if err != nil {
		t.Fatal(err)
	}
	if a3 != a1 {
		t.Fatal("trim 后同 ref 应命中同一实例")
	}

	// 不同 model id 各建各的（同一张表容多家/多模型共存）。
	b, err := r.Get(ctx, "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if b == a1 {
		t.Fatal("不同 ref 不应共享实例")
	}
	if len(r.cache) != 2 {
		t.Fatalf("cache 条目 = %d, want 2", len(r.cache))
	}
}

func TestModelRegistryFailureNotCached(t *testing.T) {
	r := NewModelRegistry(Env{"DEEPSEEK_API_KEY": "sk-test"})
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		if _, err := r.Get(ctx, "nope/x"); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("第 %d 次 Get 应失败并报出厂商名，实得: %v", i, err)
		}
	}
	if len(r.cache) != 0 {
		t.Fatalf("失败不得入缓存，cache = %d 条", len(r.cache))
	}
}
