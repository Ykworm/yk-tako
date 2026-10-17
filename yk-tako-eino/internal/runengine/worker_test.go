package runengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// doc/27 §4a：账单提取纯函数的四件负路径 + 一件正形（帧与 payload 列共用 UsageBill 形状）。
func TestUsageFrame(t *testing.T) {
	// 负路径：无 ResponseMeta / 无 Usage / 三数全 0 —— 三种都 ok=false，不发帧不发 0。
	if _, ok := usageBill(&schema.Message{}, 256000); ok {
		t.Error("无 ResponseMeta 应不发帧")
	}
	if _, ok := usageBill(&schema.Message{ResponseMeta: &schema.ResponseMeta{}}, 256000); ok {
		t.Error("无 Usage 应不发帧")
	}
	if _, ok := usageBill(&schema.Message{ResponseMeta: &schema.ResponseMeta{
		Usage: &schema.TokenUsage{}}},
		256000); ok {
		t.Error("三数全 0 应不发帧")
	}

	// 正形：数值列对齐 TokenUsage；装帧后 kind/agent/model 恒在、window 原样带配置值。
	bill, ok := usageBill(&schema.Message{ResponseMeta: &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 2910, CompletionTokens: 63,
			TotalTokens: 3034, CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 61}},
		FinishReason: "stop"}}, 256000)
	if !ok {
		t.Fatal("有账单应发帧")
	}
	if bill.PromptTokens != 2910 || bill.CompletionTokens != 63 || bill.ReasoningTokens != 61 ||
		bill.TotalTokens != 3034 || bill.Window != 256000 {
		t.Errorf("账单字段不对: %+v", bill)
	}
	f := usageFrame(bill, "", "deepseek/deepseek-flash")
	if f.Kind != "usage" || f.Agent != "" || f.Model != "deepseek/deepseek-flash" ||
		f.PromptTokens != 2910 || f.CompletionTokens != 63 || f.ReasoningTokens != 61 ||
		f.TotalTokens != 3034 || f.Window != 256000 {
		t.Errorf("usage 帧字段不对: %+v", f)
	}

	// 字节面：camelCase 键 + reasoningTokens/window 缺省时 omitempty 省略。
	b := mustJSON(t, f)
	for _, key := range []string{`"promptTokens":2910`, `"completionTokens":63`, `"totalTokens":3034`, `"kind":"usage"`} {
		if !strings.Contains(b, key) {
			t.Errorf("JSON 缺 %s: %s", key, b)
		}
	}
	// 子代理同形状：agent 非空原样透传（前端第一版只渲染 root 是消费侧的事）；
	// 无 ReasoningTokens、window=0 时 omitempty 省略——字节面不出现缺省列。
	bill, _ = usageBill(&schema.Message{ResponseMeta: &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}}, 0)
	f = usageFrame(bill, "researcher", "deepseek/deepseek-v4-pro")
	if f.Agent != "researcher" || f.Window != 0 {
		t.Errorf("子代理帧应透传 agent、window=0 原样: %+v", f)
	}
	b = mustJSON(t, f)
	if strings.Contains(b, "reasoningTokens") || strings.Contains(b, `"window"`) {
		t.Errorf("缺省列应省略: %s", b)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
