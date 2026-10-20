// 本文件钉 T4/T5 首拍的行为面：BM25 分词/排序照 pi 夹具、声明面过滤、激活后下一轮可见、
// 无命中不激活、激活集不跨 run。
package runengine

import (
	"context"
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
