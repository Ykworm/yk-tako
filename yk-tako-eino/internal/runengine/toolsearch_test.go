// 本文件钉 T4/T5 首拍的行为面：BM25 分词/排序照 pi 夹具、声明面过滤、激活后下一轮可见、
// 无命中不激活、激活集不跨 run。
package runengine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

type searchIssueParams struct {
	State string `json:"state,omitempty" jsonschema:"description=open or closed"`
}

func mustSearchToolInfo[T any](t *testing.T, name, desc string) *schema.ToolInfo {
	t.Helper()
	info, err := utils.GoStruct2ToolInfo[T](name, desc)
	if err != nil {
		t.Fatalf("造 ToolInfo %s: %v", name, err)
	}
	return info
}

func piSearchDocs(t *testing.T) []toolSearchDoc {
	t.Helper()
	return []toolSearchDoc{
		newToolSearchDoc(mustSearchToolInfo[searchIssueParams](t, "mcp__github__list_issues", "List issues in a repository.")),
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__github__create_pull_request", "Open a pull request.")),
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__linear__search_issues", "Search Linear issues by text.")),
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__docs__search", "Search the documentation.")),
	}
}

func rankNames(matches []toolSearchMatch) []string {
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.Name)
	}
	return names
}

func TestTokenize(t *testing.T) {
	got := tokenize("listIssues for the GitHub_repo")
	want := []string{"list", "issue", "git", "hub", "repo"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokenize camel/snake: got %v, want %v", got, want)
	}
	got = tokenize("searches queries HTTPServer")
	want = []string{"search", "query", "http", "server"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokenize stem/upper: got %v, want %v", got, want)
	}
}

// TestToolSearchDocIncludesSchemaText 钉文档构造：名字（原形与下划线拆词）、描述、
// schema 属性名与属性描述都进检索文本；没有 ParamsOneOf 的裸 ToolInfo 也不丢名字/描述。
func TestToolSearchDocIncludesSchemaText(t *testing.T) {
	doc := newToolSearchDoc(mustSearchToolInfo[searchIssueParams](t, "mcp__github__list_issues", "List issues."))
	set := map[string]bool{}
	for _, tok := range tokenize(doc.text) {
		set[tok] = true
	}
	for _, tok := range []string{"mcp", "github", "list", "issue", "state", "open", "closed"} {
		if !set[tok] {
			t.Fatalf("文档缺 token %q: %q", tok, doc.text)
		}
	}
	bare := newToolSearchDoc(&schema.ToolInfo{Name: "mcp__x__ping", Desc: "Ping the server."})
	if !strings.Contains(bare.text, "mcp__x__ping") || !strings.Contains(bare.text, "Ping the server.") {
		t.Fatalf("裸 ToolInfo 文档丢了名字或描述: %q", bare.text)
	}
}

// TestRankToolSearchStableTieAndLimit 钉同分保文档序（sort.SliceStable）与截断。
func TestRankToolSearchStableTieAndLimit(t *testing.T) {
	docs := []toolSearchDoc{
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__a__widget", "Widget widget.")),
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__b__widget", "Widget widget.")),
		newToolSearchDoc(mustSearchToolInfo[struct{}](t, "mcp__c__widget", "Widget widget.")),
	}
	if got := rankNames(rankToolSearch("widget", docs, 8)); strings.Join(got, ",") !=
		"mcp__a__widget,mcp__b__widget,mcp__c__widget" {
		t.Fatalf("同分应保文档序，得 %v", got)
	}
	if got := rankNames(rankToolSearch("widget", docs, 2)); strings.Join(got, ",") !=
		"mcp__a__widget,mcp__b__widget" {
		t.Fatalf("limit 2 应截前两条，得 %v", got)
	}
}

// TestToolSearchLimitDefaultAndExplicit 钉默认 limit=8、显式 limit、超量不补、非法 limit。
func TestToolSearchLimitDefaultAndExplicit(t *testing.T) {
	ctx := context.Background()
	newBox := func() *toolSearchBox {
		b := newToolSearchBox()
		for i := 0; i < 10; i++ {
			info := mustSearchToolInfo[struct{}](t, fmt.Sprintf("mcp__s__widget%d", i), "Widget tool.")
			if err := b.add(info); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		return b
	}
	run := func(args string) string {
		ts, err := newToolSearchTool(newBox())
		if err != nil {
			t.Fatalf("newToolSearchTool: %v", err)
		}
		out, err := ts.InvokableRun(ctx, args)
		if err != nil {
			t.Fatalf("InvokableRun %s: %v", args, err)
		}
		return out
	}
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"query":"widget"}`, "Loaded 8 tools for this run:"},
		{`{"query":"widget","limit":3}`, "Loaded 3 tools for this run:"},
		{`{"query":"widget","limit":50}`, "Loaded 10 tools for this run:"},
	} {
		if out := run(tc.args); !strings.HasPrefix(out, tc.want) {
			t.Fatalf("%s 应回 %q，得 %q", tc.args, tc.want, out)
		}
	}
	ts, err := newToolSearchTool(newBox())
	if err != nil {
		t.Fatalf("newToolSearchTool: %v", err)
	}
	if _, err := ts.InvokableRun(ctx, `{"query":"widget","limit":-1}`); err == nil {
		t.Fatal("负 limit 应报错")
	}
}

func TestRankToolSearchPiFixtures(t *testing.T) {
	docs := piSearchDocs(t)

	cases := []struct {
		query string
		want  []string
	}{
		{"issue", []string{"mcp__linear__search_issues", "mcp__github__list_issues"}},
		{"pull requests", []string{"mcp__github__create_pull_request"}},
		{"closed", []string{"mcp__github__list_issues"}},
	}
	for _, tc := range cases {
		got := rankNames(rankToolSearch(tc.query, docs, 8))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("rank %q: got %v, want %v", tc.query, got, tc.want)
		}
	}
	if got := rankToolSearch("search", docs, 1); len(got) != 1 {
		t.Fatalf("limit 1 应只回一条，得 %v", rankNames(got))
	}
	for _, query := range []string{"kubernetes", "the", "tickets"} {
		if got := rankToolSearch(query, docs, 8); len(got) != 0 {
			t.Fatalf("query %q 应无命中，得 %v", query, rankNames(got))
		}
	}
	if got := rankToolSearch("", docs, 8); len(got) != 0 {
		t.Fatalf("空 query 应无命中，得 %v", rankNames(got))
	}
}

func fakeSearchTool(t *testing.T, name, desc string) tool.BaseTool {
	t.Helper()
	info := mustSearchToolInfo[struct{}](t, name, desc)
	return utils.NewTool(info, func(context.Context, struct{}) (string, error) { return "ok", nil })
}

func toolNames(t *testing.T, ts []tool.BaseTool) []string {
	t.Helper()
	names := make([]string, 0, len(ts))
	for _, tt := range ts {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatalf("读工具信息: %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

func declaredNames(state *adk.ChatModelAgentState) []string {
	names := make([]string, 0, len(state.ToolInfos))
	for _, info := range state.ToolInfos {
		names = append(names, info.Name)
	}
	return names
}

func fullInfos(t *testing.T, ts []tool.BaseTool) []*schema.ToolInfo {
	t.Helper()
	infos := make([]*schema.ToolInfo, 0, len(ts))
	for _, tt := range ts {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatalf("读工具信息: %v", err)
		}
		infos = append(infos, info)
	}
	return infos
}

func findInvokable(t *testing.T, ts []tool.BaseTool, name string) tool.InvokableTool {
	t.Helper()
	for _, tt := range ts {
		info, err := tt.Info(context.Background())
		if err != nil {
			t.Fatalf("读工具信息: %v", err)
		}
		if info.Name != name {
			continue
		}
		inv, ok := tt.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s 不是 InvokableTool", name)
		}
		return inv
	}
	t.Fatalf("池里没有 %s：%v", name, toolNames(t, ts))
	return nil
}

func TestToolSearchHandlerDeclaresAndActivates(t *testing.T) {
	h := newToolSearchHandler()
	direct := fakeSearchTool(t, "get_time", "查当前时间")
	deferred := fakeSearchTool(t, "mcp__fs__list_directory", "List the contents of a directory.")
	runCtx := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{direct, deferred}}

	ctx, runCtx, err := h.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent: %v", err)
	}
	if got := toolNames(t, runCtx.Tools); got[len(got)-1] != toolSearchToolName {
		t.Fatalf("tool_search 应排在池尾，得 %v", got)
	}

	// 首轮 state.ToolInfos 是 runCtx.Tools 的全量（Eino genToolInfos 的形状）。
	state := &adk.ChatModelAgentState{ToolInfos: fullInfos(t, runCtx.Tools)}
	_, state, err = h.BeforeModelRewriteState(ctx, state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("首轮 BeforeModelRewriteState: %v", err)
	}
	first := strings.Join(declaredNames(state), ",")
	if !strings.Contains(first, "get_time") || !strings.Contains(first, toolSearchToolName) {
		t.Fatalf("direct 与 tool_search 应声明，得 %v", declaredNames(state))
	}
	if strings.Contains(first, "mcp__fs__") {
		t.Fatalf("首轮不应声明 deferred，得 %v", declaredNames(state))
	}

	search := findInvokable(t, runCtx.Tools, toolSearchToolName)
	out, err := search.InvokableRun(ctx, `{"query":"list directory"}`)
	if err != nil {
		t.Fatalf("tool_search 调用: %v", err)
	}
	if !strings.HasPrefix(out, "Loaded 1 tool for this run:") || !strings.Contains(out, "mcp__fs__list_directory") {
		t.Fatalf("命中回执不对: %q", out)
	}

	// 第二轮仍拿上一轮的 filtered ToolInfos 起手（模拟 Eino 持久化后的状态），重建才看得到新激活项。
	state = &adk.ChatModelAgentState{ToolInfos: state.ToolInfos}
	_, state, err = h.BeforeModelRewriteState(ctx, state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("二轮 BeforeModelRewriteState: %v", err)
	}
	if !strings.Contains(strings.Join(declaredNames(state), ","), "mcp__fs__list_directory") {
		t.Fatalf("激活项第二轮应声明，得 %v", declaredNames(state))
	}
	if !strings.Contains(strings.Join(declaredNames(state), ","), "get_time") {
		t.Fatalf("direct 工具激活后应恒留声明面，得 %v", declaredNames(state))
	}

	// 已加载的工具不重复返回（pi 同语义）。
	out, err = search.InvokableRun(ctx, `{"query":"list directory"}`)
	if err != nil || out != "No matching tools found." {
		t.Fatalf("已激活工具不应重复返回: %q / %v", out, err)
	}

	// 无命中不激活；错误经 wrapToolErrors 变成「错误：」结果。
	out, err = search.InvokableRun(ctx, `{"query":"kubernetes"}`)
	if err != nil || out != "No matching tools found." {
		t.Fatalf("无命中回执不对: %q / %v", out, err)
	}
	out, err = search.InvokableRun(ctx, `{"query":"  "}`)
	if err != nil || !strings.HasPrefix(out, "错误：") {
		t.Fatalf("空 query 应是错误结果，得 %q / %v", out, err)
	}
	out, err = search.InvokableRun(ctx, `{"query":"list","limit":0}`)
	if err != nil || !strings.HasPrefix(out, "错误：") {
		t.Fatalf("limit=0 应是错误结果，得 %q / %v", out, err)
	}
}

// TestResolveToolPoolReservedToolSearch 钉 A2 的池解析面：tool_search 是保留名——
// 写进 tools: 不算未知名、也不进池（真工具由子代理自己的 handler 装入）；
// 继承档与 exclude_tools 档不自动获得它。
func TestResolveToolPoolReservedToolSearch(t *testing.T) {
	ctx := context.Background()
	base := []tool.BaseTool{
		fakeSearchTool(t, "get_time", "查当前时间"),
		fakeSearchTool(t, "mcp__fs__list_directory", "List the contents of a directory."),
	}

	list := []string{toolSearchToolName, "mcp__fs__list_directory"}
	pool, err := resolveToolPool(ctx, AgentSpec{Tools: &list}, base)
	if err != nil {
		t.Fatalf("保留名不应被当未知名: %v", err)
	}
	if got := toolNames(t, pool); len(got) != 1 || got[0] != "mcp__fs__list_directory" {
		t.Fatalf("池里应只有声明的 MCP 工具（tool_search 由 handler 装入），得 %v", got)
	}
	if !specWantsToolSearch(AgentSpec{Tools: &list}) {
		t.Fatal("显式写 tool_search 应要求挂子代理 handler")
	}

	only := []string{toolSearchToolName}
	if pool, err := resolveToolPool(ctx, AgentSpec{Tools: &only}, base); err != nil || len(pool) != 0 {
		t.Fatalf("只写 tool_search 应得空池、不报错: %v %v", toolNames(t, pool), err)
	}
	if specWantsToolSearch(AgentSpec{}) {
		t.Fatal("继承档不应自动获得 tool_search")
	}
	drop := []string{"get_time"}
	if specWantsToolSearch(AgentSpec{ExcludeTools: &drop}) {
		t.Fatal("exclude_tools 档不应自动获得 tool_search")
	}

	typo := []string{"tool_serch"}
	_, err = resolveToolPool(ctx, AgentSpec{Tools: &typo}, base)
	if err == nil || !strings.Contains(err.Error(), "tool_serch") || !strings.Contains(err.Error(), toolSearchToolName) {
		t.Fatalf("未知名报错的可选名单应含保留名，实得: %v", err)
	}
}

func TestToolSearchHandlerRunLocalBox(t *testing.T) {
	h := newToolSearchHandler()
	direct := fakeSearchTool(t, "get_time", "查当前时间")
	deferred := fakeSearchTool(t, "mcp__fs__list_directory", "List the contents of a directory.")

	first := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{direct, deferred}}
	ctx1, first, err := h.BeforeAgent(context.Background(), first)
	if err != nil {
		t.Fatalf("第一场 BeforeAgent: %v", err)
	}
	search := findInvokable(t, first.Tools, toolSearchToolName)
	if _, err := search.InvokableRun(ctx1, `{"query":"directory"}`); err != nil {
		t.Fatalf("第一场激活: %v", err)
	}

	// 第二场 run 用同一个 handler 实例，激活集必须重新从零开始。
	second := &adk.ChatModelAgentContext{Tools: []tool.BaseTool{direct, deferred}}
	ctx2, second, err := h.BeforeAgent(context.Background(), second)
	if err != nil {
		t.Fatalf("第二场 BeforeAgent: %v", err)
	}
	state := &adk.ChatModelAgentState{ToolInfos: fullInfos(t, second.Tools)}
	_, state, err = h.BeforeModelRewriteState(ctx2, state, &adk.ModelContext{})
	if err != nil {
		t.Fatalf("第二场 BeforeModelRewriteState: %v", err)
	}
	if strings.Contains(strings.Join(declaredNames(state), ","), "mcp__fs__") {
		t.Fatalf("新 run 应从零激活，得 %v", declaredNames(state))
	}
}
