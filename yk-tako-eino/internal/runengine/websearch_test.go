// 本文件：web_search 的单测网（doc/22 §3/§4/§5/§6）。活体覆盖不到的分支在这里钉死：
// 换厂商兜底（引擎缺第二家密钥起不来）、双失败串联、空查询不发 HTTP（doc/22 §10）、
// 始终落盘/写失败退化/无 noteBox 退化、token 数同口径。外部端点全部用 httptest 替身，
// 绝不真打 api.deepseek.com / searchfree.site。
package runengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
)

// stampCtx 造带调用方标记与身份表的 ctx（callerStampMiddleware 在活体里干的事，这里手工等价）。
func stampCtx(ctx context.Context, name, ref string) context.Context {
	table := newAgentModelTable("deepseek/deepseek-v4-flash", nil)
	table.set(name, agentModelEntry{ref: ref})
	ctx = context.WithValue(ctx, callerNameKey{}, name)
	return withAgentModelTable(ctx, table)
}

// withRunBox 给 ctx 挂 noteBox（活体里 worker.Run 挂的那份的等价物）；落盘文件名用它顺编。
func withRunBox(ctx context.Context, run RunID) context.Context {
	return withNoteBox(ctx, newNoteBox("s_q", run, 1))
}

// runeCounter 是假计数器：数 Unicode 字数。测试只要「同一段文本两处数得同一个数」，
// 不需要真 BPE（真 cl100k 在 compact 测试里已有替身惯例）。
type runeCounter struct{}

func (runeCounter) Count(s string) int { return utf8.RuneCountInString(s) }

// invokeWebSearchWS 走真实工具面（InferTool 反射 schema + InvokableTool.Invoke），不直调函数。
func invokeWebSearchWS(t *testing.T, ctx context.Context, workspace string, counter TokenCounter, args string) (string, error) {
	t.Helper()
	raw, err := newWebSearchTool(workspace, counter)
	if err != nil {
		t.Fatalf("newWebSearchTool: %v", err)
	}
	it, ok := raw.(tool.InvokableTool)
	if !ok {
		t.Fatalf("web_search 应实现 InvokableTool，实得 %T", raw)
	}
	return it.InvokableRun(ctx, args)
}

// invokeWebSearch 是不关心落盘的测试用的便捷入口（TempDir workspace、无计数器）。
func invokeWebSearch(t *testing.T, ctx context.Context, args string) (string, error) {
	return invokeWebSearchWS(t, ctx, t.TempDir(), nil, args)
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

// captureServer 在计数之外把最近一次请求的 Authorization 头与 body 存下来（断言 Bearer 认证
// 与请求形状用）。
func captureServer(t *testing.T, hits *atomic.Int64, body string, auth *string, reqBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request, ) {
		hits.Add(1)
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		if reqBody != nil {
			b, _ := io.ReadAll(r.Body)
			*reqBody = string(b)
		}
		_, _ = w.Write([]byte(body))
	}))
}

// readSpillFile 读落盘文件全文；文件不存在 = 测试失败（始终落盘语义下，成功路径必有文件）。
func readSpillFile(t *testing.T, workspace, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("落盘文件 %s 应存在: %v", rel, err)
	}
	return string(b)
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

// searchfree 行（非 deepseek 厂商）：始终落盘 + 描述段形状。文件里标题截 200、url 去重、
// 无 url 项丢掉、摘要空写无摘要、searchfree 的 500 字帽生效；描述段 token 数 = 文件字数
//（runeCounter 口径自洽）。
func TestWebSearchSearchfreeSpillsFile(t *testing.T) {
	long := strings.Repeat("标", 250)  // 250 字，超 200 截
	huge := strings.Repeat("摘", 500) // 500 字 = searchfree parse 帽，文件里应全文在场
	results := map[string]any{"results": []map[string]any{
		{"title": long, "url": "https://a.example/1", "snippet": "s1"},
		{"title": "无 url 项", "snippet": "该丢"},
		{"title": "重复 url", "url": "https://a.example/1"},
		{"title": "第二条", "url": "https://a.example/2"},
		{"title": "巨摘要一", "url": "https://a.example/3", "snippet": huge},
		{"title": "摘要空", "url": "https://a.example/6"},
	}}
	raw, _ := json.Marshal(results)
	var hits atomic.Int64
	sf := countingServer(t, &hits, string(raw))
	defer sf.Close()
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)

	ws := t.TempDir()
	ctx := withRunBox(stampCtx(context.Background(), "", "tencent-token-plan/deepseek-v4-flash-202605"), "r_ws1")
	out, err := invokeWebSearchWS(t, ctx, ws, runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("searchfree 被打 %d 次, want 1", hits.Load())
	}
	rel := ".tako/search/r_ws1_1.md"
	file := readSpillFile(t, ws, rel)
	lines := strings.Split(out, "\n")
	if lines[0] != "来源：searchfree" {
		t.Fatalf("首行 = %q", lines[0])
	}
	if lines[1] != "说明：厂商 tencent-token-plan 没有搜索实现" {
		t.Fatalf("说明行 = %q", lines[1])
	}
	wantSearch := fmt.Sprintf("搜索「q」：%d 条结果，共 %d token，完整结果已写入 %s。",
		4, utf8.RuneCountInString(file), rel)
	if lines[2] != wantSearch {
		t.Fatalf("搜索行 = %q, want %q", lines[2], wantSearch)
	}
	// 文件面：标题截 200、去重、无 url 项丢、巨摘要全文（不吃任何帽）、摘要空写无摘要。
	if !strings.Contains(file, "1. "+strings.Repeat("标", 200)+"…") {
		t.Fatal("标题没按 200 字截断")
	}
	if strings.Count(file, "https://a.example/1") != 1 {
		t.Fatal("url 1 应去重只留一次")
	}
	if strings.Contains(file, "无 url 项") || strings.Contains(file, "该丢") {
		t.Fatal("无 url 项应被丢掉")
	}
	if !strings.Contains(file, huge) {
		t.Fatal("500 字巨摘要应在文件里全文在场")
	}
	if !strings.Contains(file, "\n   无摘要") {
		t.Fatal("摘要空应写无摘要")
	}
	// 同一 run 第二次搜索：计数器顺编 _2，第一次文件原样在。
	if _, err := invokeWebSearchWS(t, ctx, ws, runeCounter{}, `{"query":"q2"}`); err != nil {
		t.Fatalf("第二次 invoke: %v", err)
	}
	if got := readSpillFile(t, ws, ".tako/search/r_ws1_2.md"); !strings.Contains(got, "查询：q2") {
		t.Fatalf("第二份文件内容不对: %q", got[:60])
	}
}

// deepseek 行（新 /v1/tools/search）：Bearer 认证、请求形状（query + need_content:false）、
// url 去重、summary 全文进文件、searchfree 零调用。
func TestWebSearchDeepseekHit(t *testing.T) {
	resp := map[string]any{"results": []map[string]any{
		{"title": "甲", "url": "https://x.example/a", "summary": strings.Repeat("甲页原文", 400)},
		{"title": "乙", "url": "https://x.example/b", "summary": "乙的摘要"},
		{"title": "重复", "url": "https://x.example/a", "summary": "重复条目"},
		{"title": "丙", "url": "https://x.example/c", "summary": ""},
	}}
	raw, _ := json.Marshal(resp)
	var dsHits, sfHits atomic.Int64
	var auth, dsReq string
	ds := captureServer(t, &dsHits, string(raw), &auth, &dsReq)
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	ws := t.TempDir()
	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_ds1"), ws, runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 1 {
		t.Fatalf("deepseek 被打 %d 次, want 1", dsHits.Load())
	}
	if sfHits.Load() != 0 {
		t.Fatalf("deepseek 成功时 searchfree 不该被调用，实打 %d 次", sfHits.Load())
	}
	if auth != "Bearer k" {
		t.Fatalf("Authorization = %q, want Bearer k", auth)
	}
	var sent struct {
		Query       string `json:"query"`
		NeedContent bool   `json:"need_content"`
		Count       int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(dsReq), &sent); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if sent.Query != "q" || sent.NeedContent || sent.Count != 0 {
		t.Fatalf("请求形状不对: %+v", sent)
	}
	rel := ".tako/search/r_ds1_1.md"
	file := readSpillFile(t, ws, rel)
	if !strings.Contains(file, "来源：deepseek\n查询：q\n") {
		t.Fatalf("文件头不对: %q", file[:40])
	}
	if !strings.Contains(file, strings.Repeat("甲页原文", 400)) {
		t.Fatal("长 summary 应全文进文件")
	}
	if strings.Contains(file, "重复条目") {
		t.Fatal("重复 url 应去重")
	}
	if !strings.Contains(file, "\n   无摘要") {
		t.Fatal("summary 空应写无摘要")
	}
	want := fmt.Sprintf("来源：deepseek\n搜索「q」：%d 条结果，共 %d token，完整结果已写入 %s。",
		3, utf8.RuneCountInString(file), rel)
	if out != want {
		t.Fatalf("描述段形状不对:\n%s\nwant:\n%s", out, want)
	}
}

// query 超 128 字：截断不报错，端点收到的是截后的 128 字（描述段同口径展示截后 query）。
func TestWebSearchQueryClipped(t *testing.T) {
	var hits atomic.Int64
	var dsReq string
	ds := captureServer(t, &hits, `{"results":[{"title":"t","url":"https://x.example/a","summary":"s"}]}`, nil, &dsReq)
	defer ds.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	long := strings.Repeat("长", 130)
	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_clip"), t.TempDir(), runeCounter{}, fmt.Sprintf(`{"query":%q}`, long))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var sent struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(dsReq), &sent); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if utf8.RuneCountInString(sent.Query) != 128 || !strings.HasPrefix(sent.Query, strings.Repeat("长", 128)) {
		t.Fatalf("端点应收到截后的 128 字, 实得 %d 字", utf8.RuneCountInString(sent.Query))
	}
	if !strings.Contains(out, "搜索「"+strings.Repeat("长", 128)+"」") {
		t.Fatal("描述段应展示截后的 query")
	}
}

// deepseek 行失败（results 为空）→ searchfree 兜底，说明行带原因。
func TestWebSearchDeepseekEmptyResultsFallsBack(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := countingServer(t, &dsHits, `{"results":[]}`)
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_fb1"), t.TempDir(), runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if dsHits.Load() != 1 || sfHits.Load() != 1 {
		t.Fatalf("ds=%d sf=%d, want 各 1（先试 deepseek 再兜底）", dsHits.Load(), sfHits.Load())
	}
	if !strings.HasPrefix(out, "来源：searchfree\n说明：deepseek 搜索失败：results 为空\n搜索「q」：1 条结果") {
		t.Fatalf("兜底形状不对:\n%s", out)
	}
}

// deepseek 行失败（HTTP 500）→ searchfree 兜底，原因只报状态码不贴正文。
func TestWebSearchDeepseekHTTPFailFallsBack(t *testing.T) {
	var dsHits, sfHits atomic.Int64
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dsHits.Add(1)
		http.Error(w, "server on fire", http.StatusInternalServerError)
	}))
	defer ds.Close()
	sf := countingServer(t, &sfHits, `{"results":[{"title":"t","url":"https://s.example/1"}]}`)
	defer sf.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("SEARCHFREE_ENDPOINT", sf.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_fb2"), t.TempDir(), runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.HasPrefix(out, "来源：searchfree\n说明：deepseek 搜索失败：HTTP 500\n搜索「q」：1 条结果") {
		t.Fatalf("兜底形状不对:\n%s", out)
	}
}

// 无 noteBox（旁路调用）：搜索成功但无处落盘 → 描述段报「仅本次可见」，workspace 无文件。
func TestWebSearchNoNoteBoxOnlyVisible(t *testing.T) {
	var hits atomic.Int64
	ds := countingServer(t, &hits, `{"results":[{"title":"t","url":"https://x.example/a","summary":"s"}]}`)
	defer ds.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	ws := t.TempDir()
	out, err := invokeWebSearchWS(t, stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), ws, runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "结果仅本次可见（未能写入文件）。") {
		t.Fatalf("缺仅本次可见句: %q", out)
	}
	if strings.Contains(out, "token") || strings.Contains(out, "已写入") {
		t.Fatalf("无落盘不该报 token 或路径: %q", out)
	}
	entries, err := os.ReadDir(ws)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace 应保持为空: %v %d", err, len(entries))
	}
}

// 落盘写失败（workspace 根是一个文件，MkdirAll 必炸）→ 静默退化为「仅本次可见」，不报错。
func TestWebSearchSpillFailureDegrades(t *testing.T) {
	var hits atomic.Int64
	ds := countingServer(t, &hits, `{"results":[{"title":"t","url":"https://x.example/a","summary":"s"}]}`)
	defer ds.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_sf1"), blocked, runeCounter{}, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("写失败不应上抛: %v", err)
	}
	if !strings.Contains(out, "结果仅本次可见（未能写入文件）。") {
		t.Fatalf("缺仅本次可见句: %q", out)
	}
}

// nil 计数器（-compact-window 0 的装配形状）：描述段报条数与路径、不报 token——宁缺不发明
// 第二套估算。
func TestWebSearchNilCounterNoToken(t *testing.T) {
	var hits atomic.Int64
	ds := countingServer(t, &hits, `{"results":[{"title":"t","url":"https://x.example/a","summary":"s"}]}`)
	defer ds.Close()
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", ds.URL)
	t.Setenv("DEEPSEEK_API_KEY", "k")

	ws := t.TempDir()
	out, err := invokeWebSearchWS(t, withRunBox(stampCtx(context.Background(), "", "deepseek/deepseek-v4-flash"), "r_nc1"), ws, nil, `{"query":"q"}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	want := "来源：deepseek\n搜索「q」：1 条结果，完整结果已写入 .tako/search/r_nc1_1.md。"
	if out != want {
		t.Fatalf("描述段 = %q, want %q", out, want)
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
