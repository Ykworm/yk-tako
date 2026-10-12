// overflow_test.go：检出①两张表的逐条 fixtures + 窗真值提取的边界（19v6-09 §4/§6、A1 四要素第 4 条）。
// 命中/排除各条一个样例；eino 组件包壳两条是**实读形状**（openai/claude 组件怎么把 provider 原文
// 带进 .Error()——表在它上面子串匹配，这两条是「匹配假设」的钉子）。
package runengine

import (
	"errors"
	"testing"
)

func TestIsOverflowError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		// ---- 命中表 24 条：一条一个样例（出处厂商同 overflow.go 表注）----
		{"anthropic", "prompt is too long: 209775 tokens > 200000 maximum", true},
		{"anthropic-413", `{"type":"error","error":{"type":"invalid_request_error","message":"request_too_large"}}`, true},
		{"bedrock", "ValidationException: input is too long for requested model", true},
		{"openai", "Your input exceeds the context window of this model. Please adjust your input and try again.", true},
		{"openai-compatible", "prompt exceeds the model's maximum context length", true},
		{"gemini", "input token count 1055656 exceeds the maximum 1048576", true},
		{"grok", "maximum prompt length is 131072", true},
		{"groq", "Please reduce the length of the messages.", true},
		{"openrouter", "maximum context length is 128000 tokens", true},
		{"poolside", "exceeds maximum allowed input length", true},
		{"together", "input is longer than the model's context length", true},
		{"copilot", "exceeds the limit of 200000", true},
		{"llamacpp", "exceeds the available context size", true},
		{"lmstudio", "greater than the context length", true},
		{"minimax", "context window exceeds limit", true},
		{"kimi", "exceeded model token limit", true},
		{"mistral", "too large for model with 32768 maximum context length", true},
		{"ds4", "but the configured context size is 8192", true},
		{"zai", "model_context_window_exceeded", true},
		{"ollama", "prompt too long; exceeded max context length", true},
		{"dashscope", "range of input length should be [1, 6000]", true},
		{"generic-under", "context_length_exceeded", true},
		{"generic-space", "context length exceeded", true},
		{"generic-many-tokens", "too many tokens", true},
		{"generic-limit", "token limit exceeded", true},

		// ---- eino 组件包壳形状（实读 fixtures：provider 原文必须仍可匹配）----
		{"eino-openai-wrap", `error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 65536 tokens. However, you requested 70000 tokens (70000 in the messages, 0 in the completion).`, true},
		{"eino-claude-wrap", `create new streaming message fail: POST "https://api.anthropic.com/v1/messages": 400 Bad Request {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 209775 tokens > 200000 maximum"}}`, true},

		// ---- 排除表 4 条（先排除后命中；前两条同时命中命中表词——排除必须赢）----
		{"exclude-bedrock-throttling", "ThrottlingException: Too many tokens, please wait before trying again", false},
		{"exclude-429-token-words", "429 Too Many Requests: token limit exceeded", false},
		{"exclude-service-unavailable", "service unavailable, please retry", false},
		{"exclude-rate-limit", "rate limit reached", false},

		// ---- 普通错误（不相干文本不误报）----
		{"ordinary-deadline", "context deadline exceeded", false},
		{"ordinary-conn", "broken pipe", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := IsOverflowError(errors.New(c.msg)); got != c.want {
			t.Errorf("%s: IsOverflowError(%q) = %v, want %v", c.name, c.msg, got, c.want)
		}
	}
	if IsOverflowError(nil) {
		t.Error("IsOverflowError(nil) = true, want false")
	}
}

func TestExtractWindow(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
		ok   bool
	}{
		// doc §6 原例（「窗 W + 你发了 P」两数同现取 W）
		{"two-numbers", "maximum context length is 8192 tokens, you sent 10000", 8192, true},
		// DeepSeek 系实际包壳形状（openai 组件 APIError.Error）
		{"eino-openai-wrap", `error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 65536 tokens. However, you requested 70000 tokens (70000 in the messages, 0 in the completion).`, 65536, true},
		// 活体实录（2026-10-12，api.deepseek.com deepseek-flash，逐字节；证据
		// tmp/19-v6/evidence-overflow/a1-provider-probe.md）：三个大数里窗=最小者；
		// request_id 的十六进制串（ce623e6986b8 里的 6986）必须被独立数判定挡住。
		{"deepseek-live", `error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1400065 tokens (1400049 in the messages, 16 in the completion). Please reduce the length of the messages or completion. (request_id: 4dc741ce-a82b-40ff-890f-ce623e6986b8)`, 1048576, true},
		// 十六进制串混入的独立守卫（无 request_id 全句时同样成立）
		{"hex-id-junk", "request_id: ce623e6986b8, maximum context length is 65536 tokens", 65536, true},
		// Claude 形状（eino claude 组件包壳里 provider 原文）：两数取小=窗
		{"claude-shape", "prompt is too long: 209775 tokens > 200000 maximum", 200000, true},
		// 单候选 = 它就是真值
		{"single-number", "maximum prompt length is 131072", 131072, true},
		// 全部候选低于 1024 下限（doc §6：防把「retry 3 times」的 3 当真值）
		{"below-floor", "retry 3 times", 0, false},
		// 无数字
		{"no-numbers", "exceeds the available context size", 0, false},
	}
	for _, c := range cases {
		got, ok := extractWindow(errors.New(c.msg))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: extractWindow(%q) = (%d, %v), want (%d, %v)", c.name, c.msg, got, ok, c.want, c.ok)
		}
	}
	if _, ok := extractWindow(nil); ok {
		t.Error("extractWindow(nil) ok = true, want false")
	}
}
