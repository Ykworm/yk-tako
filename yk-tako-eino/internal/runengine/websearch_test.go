// 本文件：web_search 的单测网（doc/22 §3/§4/§6 + 台账 D8）。活体覆盖不到的分支在这里钉死：
// 换厂商兜底（引擎缺第二家密钥起不来）、双失败串联、空查询不发 HTTP（doc/22 §10）。
// 外部端点全部用 httptest 替身，绝不真打 api.deepseek.com / searchfree.site。
package runengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// stampCtx 造带调用方标记与身份表的 ctx（callerStampMiddleware 在活体里干的事，这里手工等价）。
func stampCtx(ctx context.Context, name, ref string) context.Context {
	table := newAgentModelTable("deepseek/deepseek-v4-flash", nil)
	table.set(name, agentModelEntry{ref: ref})
	ctx = context.WithValue(ctx, callerNameKey{}, name)
	return withAgentModelTable(ctx, table)
}

// invokeWebSearch 走真实工具面（InferTool 反射 schema + InvokableTool.Invoke），不直调函数。
func invokeWebSearch(t *testing.T, ctx context.Context, args string) (string, error) {
	t.Helper()
	raw, err := newWebSearchTool()
	if err != nil {
		t.Fatalf("newWebSearchTool: %v", err)
	}
	it, ok := raw.(tool.InvokableTool)
	if !ok {
		t.Fatalf("web_search 应实现 InvokableTool，实得 %T", raw)
	}
	return it.InvokableRun(ctx, args)
}

// wrapErr 模拟装配点 wrapToolErrors 的错误文本出口（toolErrorText 是它的内核）。
func wrapErr(err error) string { return toolErrorText(context.Background(), err) }

// countingServer 返回一个计数 httptest：每次请求 +1，body 由调用方给。
func countingServer(t *testing.T, hits *atomic.Int64, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
}

// 空查询：包完后的文本是「错误：查询是空的」，且两个 endpoint 都没被碰（计数 server 证明
// 没发 HTTP——doc/22 §10 的单测断言）。
func TestWebSearchEmptyQueryNoHTTP(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := countingServer(t, &dsHits, "{}")
	defer ds.Close()
	sf := countingServer(t, &sfHits, "{}")
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	_, err := invokeWebSearch(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), `{"query":"  \n\t "}`)
	if err == nil {
		t.Fatal("空查询应返回 error")
	}
	if got := wrapErr(err); got != "错误：查询是空的" {
		t.Fatalf("包完后 = %q, want 错误：查询是空的", got)
	}
	if n := dsHits.Load(); n != 0 {
		t.Fatalf("deepseek 端点被打了 %d 次，空查询不该发 HTTP", n)
	}
	if n := sfHits.Load(); n != 0 {
		t.Fatalf("searchfree 端点被打了 %d 次，空查询不该发 HTTP", n)
	}
}

// 裸 ctx 无标记：不猜主 ref，报「web_search 不知道当前模型」。
func TestWebSearchNoCallerStamp(t *testing.T) {
	_, err := invokeWebSearch(t, context.Background(), `{"query":"北京天气"}`)
	if got := wrapErr(err); err == nil || got != "错误：web_search 不知道当前模型" {
		t.Fatalf("包完后 = %q, want 错误：web_search 不知道当前模型", got)
	}
}

// 严格读：标记在但表里没有该调用方条目，同样报错（不走 entryFor 的回落 root——回落会把
// 子代理的搜索静默打去主模型厂商，doc/22 §4）。
func TestWebSearchStrictLookup(t *testing.T) {
	ctx := context.WithValue(context.Background(), callerNameKey{}, "ghost")
	ctx = withAgentModelTable(ctx, newAgentModelTable("deepseek/deepseek-v4-flash", nil))
	_, err := invokeWebSearch(t, ctx, `{"query":"北京天气"}`)
	if got := wrapErr(err); err == nil || got != "错误：web_search 不知道当前模型" {
		t.Fatalf("包完后 = %q, want 错误：web_search 不知道当前模型", got)
	}
}

// searchfree 响应解析 / url 去重 / 无 url 项丢掉 / 摘要缺写无摘要 / 标题截断 / 字节帽丢整条加丢弃行。
func TestWebSearchSearchfreeAssemble(t *testing.T) {
	long := strings.Repeat("标", 250) // 250 字，超 200 截
	huge := strings.Repeat("摘", 500) // 500 字 ≈ 1500 字节，用来把 8192 顶爆
	results := map[string]any{"results": []map[string]any{
		{"title": long, "url": "https://a.example/1", "snippet": "s1"},
		{"title": "无 url 项", "snippet": "该丢"},
		{"title": "重复 url", "url": "https://a.example/1"},
		{"title": "第二条", "url": "https://a.example/2"},
		{"title": "巨摘要一", "url": "https://a.example/3", "snippet": huge},
		{"title": "巨摘要二", "url": "https://a.example/4", "snippet": huge},
		{"title": "巨摘要三", "url": "https://a.example/5", "snippet": huge},
		{"title": "巨摘要四", "url": "https://a.example/7", "snippet": huge},
		{"title": "巨摘要五", "url": "https://a.example/8", "snippet": huge},
		{"title": "巨摘要六", "url": "https://a.example/9", "snippet": huge},
		{"title": "巨摘要七", "url": "https://a.example/10", "snippet": huge},
		{"title": "摘要空", "url": "https://a.example/6"},
	}}
	raw, _ := json.Marshal(results)
	var hits atomic.Int64
	sf := countingServer(t, &hits, string(raw))
	defer sf.Close()
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)

	out, err := invokeWebSearch(t, stampCtx(context.Background(), "", "tencent-token-plan/deepseek-v4-flash-202605"), `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("searchfree 被打 %d 次, want 1", hits.Load())
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "来源：searchfree" {
		t.Fatalf("首行 = %q", lines[0])
	}
	if lines[1] != "说明：厂商 tencent-token-plan 没有搜索实现" {
		t.Fatalf("说明行 = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "1. "+strings.Repeat("标", 200)+"…") {
		t.Fatalf("标题没按 200 字截断: %q", lines[2][:40])
	}
	joined := out
	for _, want := range []string{"https://a.example/1", "https://a.example/2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺 url %s", want)
		}
	}
	// 去重：url 1 只出现一次；无 url 项被丢
	if strings.Count(joined, "https://a.example/1") != 1 {
		t.Fatal("url 1 应去重只留一次")
	}
	if strings.Contains(joined, "无 url 项") {
		t.Fatal("无 url 项应被丢掉")
	}
	// 巨摘要条目顶爆 8192：3、4、5 至少一条被整条丢（不从中间切），末尾有丢弃行
	if !strings.Contains(joined, "（已丢弃") || !strings.Contains(joined, "单次上限 8192 字节）") {
		t.Fatalf("缺丢弃行: %q", joined[:200])
	}
	droppedIdx := strings.Index(joined, "（已丢弃")
	if strings.Contains(joined[droppedIdx:], "\n   https://") {
		t.Fatal("丢弃行之后不该再有半截条目")
	}
	// 最后一条摘要空 → 无摘要
	if !strings.Contains(joined, "\n   无摘要") {
		t.Fatal("摘要空应写无摘要")
	}
}

// deepseek 行活体替身：web_search_tool_result 块 + citations 的 cited_text 按 url 对上（手打
// 实录形状：结果块是 content 顶层兄弟块，citations 可缺席）。
func TestWebSearchDeepseekHit(t *testing.T) {
	resp := map[string]any{"content": []map[string]any{
		{"type": "thinking", "thinking": "..."},
		{"type": "server_tool_use", "id": "c1", "name": "web_search"},
		{"type": "web_search_tool_result", "tool_use_id": "t1", "content": []map[string]any{
			{"type": "web_search_result", "url": "https://x.example/a", "title": "甲"},
			{"type": "web_search_result", "url": "https://x.example/b", "title": "乙"},
		}},
		{"type": "text", "text": "总结", "citations": []map[string]any{
			{"url": "https://x.example/b", "cited_text": "乙的摘要"},
		}},
	}}
	raw, _ := json.Marshal(resp)
	var dsHits atomic.Int64
	ds := countingServer(t, &dsHits, string(raw))
	defer ds.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	out, err := invokeWebSearch(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 1 {
		t.Fatalf("deepseek 被打 %d 次, want 1", dsHits.Load())
	}
	if !strings.HasPrefix(out, "来源：deepseek\n1. 甲\n   https://x.example/a\n   无摘要\n2. 乙\n   https://x.example/b\n   乙的摘要") {
		t.Fatalf("装配形状不对:\n%s", out)
	}
}

// deepseek 行失败（无结果块）→ searchfree 兜底，说明行形状（doc/22 §5/§6）。
func TestWebSearchDeepseekFailFallback(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := countingServer(t, &dsHits, `{"content":[{"type":"text","text":"没搜"}]}`)
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	out, err := invokeWebSearch(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 1 || sfHits.Load() != 1 {
		t.Fatalf("ds=%d sf=%d, want 各 1（先试 deepseek 再兜底）", dsHits.Load(), sfHits.Load())
	}
	if !strings.HasPrefix(out, "来源：searchfree\n说明：deepseek 搜索失败：响应里没有 web_search_tool_result 或去重后没有 url\n1. t\n   https://s.example/1") {
		t.Fatalf("兜底形状不对:\n%s", out)
	}
}

// 双失败：两句原因串联（deepseek 挂 + searchfree 挂），由 wrapToolErrors 包成「错误：」。
func TestWebSearchBothFail(t *testing.T) {
	dead := "http://127.0.0.1:1"
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", dead)
	t.Setenv("SEARCHFREE_ENDPOINT", dead)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	_, err := invokeWebSearch(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), `{"query":"q"}`)
	if err == nil {
		t.Fatal("双失败应返回 error")
	}
	msg := wrapErr(err)
	if !strings.HasPrefix(msg, "错误：") {
		t.Fatalf("应带错误：前缀: %q", msg)
	}
	if !strings.Contains(msg, "deepseek 搜索失败：") || !strings.Contains(msg, "searchfree 失败：") {
		t.Fatalf("两句原因都应在场: %q", msg)
	}
}

// 选择器分支一：非 deepseek 厂商（模型名里带 deepseek 也不行）→ 只走 searchfree，
// deepseek 端点零请求——「不许按模型名里的字打 DeepSeek 官方」的单测钉死。
func TestWebSearchNonDeepseekVendor(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := countingServer(t, &dsHits, "{}")
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	out, err := invokeWebSearch(t, stampCtx(context.Background(), "researcher", "tencent-token-plan/deepseek-v4-flash-202605"), `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 0 {
		t.Fatalf("deepseek 端点被打了 %d 次，非 deepseek 厂商不该走那一行", dsHits.Load())
	}
	if sfHits.Load() != 1 {
		t.Fatalf("searchfree 被打 %d 次, want 1", sfHits.Load())
	}
	if !strings.HasPrefix(out, "来源：searchfree\n说明：厂商 tencent-token-plan 没有搜索实现") {
		t.Fatalf("说明行不对:\n%s", out)
	}
}

// 选择器分支二：deepseek 厂商但无密钥 → 直接 searchfree，说明行「未配置 DEEPSEEK_API_KEY」，
// deepseek 端点零请求。
func TestWebSearchDeepseekNoKey(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := countingServer(t, &dsHits, "{}")
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "")

	out, err := invokeWebSearch(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 0 {
		t.Fatalf("无密钥还打了 deepseek 端点 %d 次", dsHits.Load())
	}
	if !strings.HasPrefix(out, "来源：searchfree\n说明：未配置 DEEPSEEK_API_KEY") {
		t.Fatalf("说明行不对:\n%s", out)
	}
}
