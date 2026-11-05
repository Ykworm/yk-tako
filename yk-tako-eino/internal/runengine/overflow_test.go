// overflow_test.go：检出①两张表的逐条 fixtures + 窗真值提取的边界（19v6-09 §4/§6、A1 四要素第 4 条）。
// 命中/排除各条一个样例；eino 组件包壳两条是**实读形状**（openai/claude 组件怎么把 provider 原文
// 带进 .Error()——表在它上面子串匹配，这两条是「匹配假设」的钉子）。
package runengine

import (
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
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
		// 活体击穿案例（A2 首次跑原件，2026-10-12，逐字节 = a2-engine-run1-extractwindow-bug.log）：
		// request_id 是带连字符的 UUID，中段 `4657` 首尾都是连字符、曾被放行当窗 → D4 落 limit' 3725
		// → 强折 errStillOver。连字符并入嵌入判定后取 1048576（uuid 数字段整体挡住）。
		{"deepseek-uuid", `error, status code: 400, status: 400 Bad Request, message: This model's maximum context length is 1048576 tokens. However, you requested 1227956 tokens (1227956 in the messages, 0 in the completion). Please reduce the length of the messages or completion. (request_id: 79123942-4657-426c-b4ce-0f56772e5888)`, 1048576, true},
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

// TestRescueWorthy：救援谓词四条件逐条翻面（A2 四要素第 5 条）——全真才救，任一为假不救。
func TestRescueWorthy(t *testing.T) {
	cases := []struct {
		name      string
		overflow  bool
		agent     string
		frames    int
		recovered bool
		want      bool
	}{
		{"four-true", true, "", 0, false, true},
		{"not-overflow", false, "", 0, false, false},
		{"subagent", true, "researcher", 0, false, false},
		{"has-frames", true, "", 3, false, false},
		{"gate-spent-zero-frames", true, "", 0, true, false},
	}
	for _, c := range cases {
		if got := rescueWorthy(c.overflow, c.agent, c.frames, c.recovered); got != c.want {
			t.Errorf("%s: rescueWorthy(%v, %q, %d, %v) = %v, want %v",
				c.name, c.overflow, c.agent, c.frames, c.recovered, got, c.want)
		}
	}
}

// TestRescueLimit：D4 目标线两路（有真值取更紧 / 提不出盲降）+ 真值更松时 C_rej 一路赢
// （两把尺子来源不同：C_rej 是 cl100k 估算、window 是 provider 真值——谁小谁是更紧者）。
func TestRescueLimit(t *testing.T) {
	cases := []struct {
		name      string
		cRej      int
		window    int
		hasWindow bool
		want      int
	}{
		// 活体场景数：C_rej≈263 万（cl100k）、真值 1048576 → min(838860, 2104000) = 838860
		{"window-tighter", 2630000, 1048576, true, 838860},
		// 真值比被拒估算还松（错估方向相反）：C_rej×4/5 是更紧者
		{"cRej-tighter", 100000, 1048576, true, 80000},
		// 提不出真值（extractWindow ok=false）：C_rej×4/5 盲降一档
		{"blind", 2630000, 0, false, 2104000},
	}
	for _, c := range cases {
		if got := rescueLimit(c.cRej, c.window, c.hasWindow); got != c.want {
			t.Errorf("%s: rescueLimit(%d, %d, %v) = %d, want %d",
				c.name, c.cRej, c.window, c.hasWindow, got, c.want)
		}
	}
}

// TestDetectTruncation：检出②两子检 (a)/(b) 的边界与负路径（A3 四要素第 4 条；doc §5/§12②）——
// 默认输出上限的 length（completion 贴默认上限、P 远小于窗）必须不命中：(c) 缺席期间它是主要误报面。
func TestDetectTruncation(t *testing.T) {
	usage := func(p, c int) *schema.TokenUsage { return &schema.TokenUsage{PromptTokens: p, CompletionTokens: c} }
	cases := []struct {
		name   string
		meta   *schema.ResponseMeta
		window int
		want   bool
		subB   bool
		prompt int
	}{
		// (a)：实数 P > 窗——provider 静默收下超窗输入、usage 里报实数（z.ai 形态）
		{"a-hit", &schema.ResponseMeta{FinishReason: "stop", Usage: usage(4123, 120)}, 4000, true, false, 4123},
		// (a) 边界：P == 窗不吃（> 是严格）；且 length/0 也不救（走 (b) 判、P 不够贴窗）
		{"a-boundary-equal", &schema.ResponseMeta{FinishReason: "stop", Usage: usage(4000, 50)}, 4000, false, false, 0},
		// (a) 先于 (b)：P 已超窗 —— (a) 命中，不附掐头形状
		{"a-before-b", &schema.ResponseMeta{FinishReason: "length", Usage: usage(4100, 0)}, 4000, true, false, 4100},
		// (b)：length + completion==0 + P ≥ 99%·窗（MiMo 形态）；边界含 P==99%·窗，差 1 不吃
		{"b-hit", &schema.ResponseMeta{FinishReason: "length", Usage: usage(3960, 0)}, 4000, true, true, 3960},
		{"b-boundary-off-by-one", &schema.ResponseMeta{FinishReason: "length", Usage: usage(3959, 0)}, 4000, false, false, 0},
		// (b) 要求 completion==0：贴窗但产出了字 = (c) 的面（缺席），不命中
		{"b-needs-zero-completion", &schema.ResponseMeta{FinishReason: "length", Usage: usage(3980, 5)}, 4000, false, false, 0},
		// 负路径①（doc §12②）：provider 默认输出上限的 length——completion 贴默认上限（DeepSeek 4096）、
		// P 远小于窗，不命中（(c) 若在场这条会被误折，正是它缺席的代价上界）
		{"default-output-cap", &schema.ResponseMeta{FinishReason: "length", Usage: usage(1000, 4096)}, 1048576, false, false, 0},
		// 负路径②：length + completion==0 但 P 不够贴窗（模型自己空输出）——不命中
		{"empty-output-far-below", &schema.ResponseMeta{FinishReason: "length", Usage: usage(100, 0)}, 4000, false, false, 0},
		// 本层关（window=0）+ usage 缺失（无从检起，沉默盲区不误报）
		{"window-off", &schema.ResponseMeta{FinishReason: "stop", Usage: usage(99999, 1)}, 0, false, false, 0},
		{"no-meta", nil, 4000, false, false, 0},
		{"no-usage", &schema.ResponseMeta{FinishReason: "length"}, 4000, false, false, 0},
	}
	for _, c := range cases {
		got := detectTruncation(c.meta, c.window)
		if (got != nil) != c.want {
			t.Errorf("%s: detectTruncation(...) = %+v, want hit=%v", c.name, got, c.want)
			continue
		}
		if got != nil && (got.subB != c.subB || got.prompt != c.prompt) {
			t.Errorf("%s: detectTruncation(...) = %+v, want subB=%v prompt=%d", c.name, got, c.subB, c.prompt)
		}
	}
}
