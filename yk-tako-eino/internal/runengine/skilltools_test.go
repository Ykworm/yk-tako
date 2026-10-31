// skilltools_test.go 钉住 B1/B2（doc/52 §2.2/§2.8/§2.10/§6.1）的几处字节与结构性口径：
// 目录块的渲染形状与转义、子代理剔除名单、GET /api/runs/:id/skills 的空实例字节、
// 正文的 frontmatter 切分与转义往返、正文上限的两条边界。
//
// 这些错了不会崩，会安静地漏：目录多一行就是给模型看了不该看的技能，
// 剔除少一个名字就是子代理多出一条读技能（C3 后还有写盘）的路，
// 上限判错就是半篇正文进上下文——而 trim 换掉它时模型看不出自己只拿到半截。
package runengine

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// 目录块：形状逐字固定（§2.2），描述原样全拼不截断。
func TestAvailableSkillsPromptFormat(t *testing.T) {
	rows := []SkillRow{{
		Name: "demo", Description: "演示技能", Path: "/ws/tenants/t1/skills/demo/SKILL.md", Valid: true,
	}}
	got := availableSkillsPrompt(rows, map[string]bool{})
	want := "<available_skills>\n" +
		"  <skill>\n" +
		"    <name>demo</name>\n" +
		"    <description>演示技能</description>\n" +
		"    <location>/ws/tenants/t1/skills/demo/SKILL.md</location>\n" +
		"  </skill>\n" +
		"</available_skills>"
	if got != want {
		t.Errorf("目录块 =\n%s\n期望\n%s", got, want)
	}
}

// 空集 → 空串（Instruction 一个字节都不多）；invalid 行与被禁用的行都不进目录。
func TestAvailableSkillsPromptSkips(t *testing.T) {
	if got := availableSkillsPrompt(nil, nil); got != "" {
		t.Errorf("空集应当回空串，实际 %q", got)
	}
	rows := []SkillRow{
		{Name: "bad", Description: "坏行", Path: "/p/bad", Valid: false},
		{Name: "off", Description: "被禁用", Path: "/p/off", Valid: true},
		{Name: "ok", Description: "生效", Path: "/p/ok", Valid: true},
	}
	got := availableSkillsPrompt(rows, map[string]bool{"off": true})
	if strings.Contains(got, "bad") || strings.Contains(got, "off") {
		t.Errorf("invalid 或被禁用的行不该进目录:\n%s", got)
	}
	if !strings.Contains(got, "<name>ok</name>") {
		t.Errorf("生效行应当进目录:\n%s", got)
	}
}

// 子代理剔除：四个保留名都不在，其余工具原样保留（顺序不变）。
func TestDropSkillTools(t *testing.T) {
	ctx := context.Background()
	base := []tool.BaseTool{
		&fakeNamedTool{name: "bash"},
		&fakeNamedTool{name: "read_skill"},
		&fakeNamedTool{name: "use_skill"},
		&fakeNamedTool{name: "web_search"},
		&fakeNamedTool{name: "read_skill_file"},
		&fakeNamedTool{name: "skill_install"},
	}
	out := dropSkillTools(ctx, base)
	var names []string
	for _, tb := range out {
		info, err := tb.Info(ctx)
		if err != nil {
			t.Fatalf("Info 失败: %v", err)
		}
		names = append(names, info.Name)
	}
	if len(out) != 2 || names[0] != "bash" || names[1] != "web_search" {
		t.Errorf("剔除后应只剩 bash/web_search，实际 %v", names)
	}
}

// GET /api/runs/:id/skills 的空实例字节：loads 是空数组不是 null（null 会让前端多判一次）。
func TestRunSkillsDataEmptyShape(t *testing.T) {
	raw, err := json.Marshal(runSkillsData{RunID: "run_x", Loads: []runSkillLoad{}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"runId":"run_x","loads":[]}`; got != want {
		t.Errorf("空实例字节 = %s，期望 %s", got, want)
	}
}

// 目录里的描述要转义 + 折叠空白（§2.2/§2.10-3）：不转义时一段 `<skill>` 示例就能把
// 目录块的边界顶开；不折叠时描述里的换行会把一条技能撑成几行，模型按行读就错位。
func TestAvailableSkillsPromptEscapes(t *testing.T) {
	rows := []SkillRow{{
		Name: "demo", Valid: true, Path: "/ws/skills/demo/SKILL.md",
		Description: "多行\n  描述 <b>&</b>",
	}}
	got := availableSkillsPrompt(rows, map[string]bool{})
	if !strings.Contains(got, "<description>多行 描述 &lt;b>&amp;&lt;/b></description>") {
		t.Errorf("描述应当折叠空白并转义 & 与 <:\n%s", got)
	}
}

// frontmatter 切分：与扫盘认同一套边界（BOM、`---` 开头、`\n---` 收口），输出是正文。
func TestStripSkillFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{"常规", "---\nname: demo\ndescription: 演示\n---\n\n# 演示\n\n正文。\n", "# 演示\n\n正文。", false},
		{"BOM", "\ufeff---\nname: demo\n---\n# 演示\n", "# 演示", false},
		{"CRLF", "---\r\nname: demo\r\n---\r\n# 演示\r\n", "# 演示", false},
		{"空正文", "---\nname: demo\n---\n", "", false},
		{"缺 frontmatter", "# 演示\n正文", "", true},
		{"未闭合", "---\nname: demo\n正文", "", true},
	}
	for _, c := range cases {
		got, err := stripSkillFrontmatter(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%s：应当报错，实际得到 %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：意外错误 %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s：正文 = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 转义往返（§2.10-3/§11.2）：`<`/`&` 转义后，还原回去必须与盘上的正文逐字相同。
// 比的是「看起来一致」，不是原始字节——转义本来就是展示形态。
func TestRenderSkillBodyEscapeRoundTrip(t *testing.T) {
	body := "比较 a < b && c\n示例 <skill>…</skill>\n已经写过 &amp; 的字样"
	row := SkillRow{Name: "demo", Scope: "tenant", Path: "/ws/tenants/t1/skills/demo/SKILL.md", Bytes: 4213}
	got := renderSkillBody(row, body, fakeCounter{f: func(string) int { return 10 }})
	want := "<skill name=\"demo\" source=\"tenant\" bytes=\"4213\">\n" + body + "\n</skill>"
	if got == want {
		t.Fatal("正文里的 < 与 & 必须转义，实际原样输出")
	}
	if !strings.Contains(got, "a &lt; b &amp;&amp; c") {
		t.Errorf("`<`/`&` 未按契约转义:\n%s", got)
	}
	if strings.Contains(got, row.Path) || strings.Contains(got, `"path"`) {
		t.Errorf("read_skill 的返回体不带 path:\n%s", got)
	}
	if inner := skillBlockInner(t, got); html.UnescapeString(inner) != body {
		t.Errorf("反转义后与原文不一致:\n得到 %q\n期望 %q", html.UnescapeString(inner), body)
	}
}

// 上限两条边界（D26）：≤16,384 token 且 ≤64KB，取更严的；超了是 JSON 信封、不截断。
func TestRenderSkillBodyLimits(t *testing.T) {
	row := SkillRow{Name: "demo", Scope: "tenant", Bytes: 1}
	tooLarge := `{"code":116116,"message":"技能正文超过上下文预算（16,384 token 或 64KB），没有读取","data":null}`

	if got := renderSkillBody(row, "正文", fakeCounter{f: func(string) int { return skillBodyMaxTokens }}); strings.HasPrefix(got, "{") {
		t.Errorf("恰好 16,384 token 应当放行，实际 %s", got)
	}
	if got := renderSkillBody(row, "正文", fakeCounter{f: func(string) int { return skillBodyMaxTokens + 1 }}); got != tooLarge {
		t.Errorf("16,385 token = %s，期望 %s", got, tooLarge)
	}
	if got := renderSkillBody(row, strings.Repeat("a", skillBodyMaxBytes), fakeCounter{f: func(string) int { return 0 }}); strings.HasPrefix(got, "{") {
		t.Errorf("恰好 64KB 应当放行，实际 %s", got[:40])
	}
	if got := renderSkillBody(row, strings.Repeat("a", skillBodyMaxBytes+1), fakeCounter{f: func(string) int { return 0 }}); got != tooLarge {
		t.Errorf("64KB+1 字节 = %s，期望 %s", got, tooLarge)
	}
}

// 失败信封逐字（§8.2-2）：116001／116107 带 name（模型能改道），116116 的 data 是 null。
func TestSkillFailEnvelope(t *testing.T) {
	cases := []struct {
		code    int
		message string
		name    string
		want    string
	}{
		{codeSkillMissing, skillMissingMsg, "demo",
			`{"code":116001,"message":"找不到这个技能，可能已被删除或改名","data":{"name":"demo"}}`},
		{codeSkillDisabled, skillDisabledMsg, "demo",
			`{"code":116107,"message":"这个技能已被禁用，请先在管理页恢复","data":{"name":"demo"}}`},
		{codeSkillBodyTooLarge, "技能正文超过上下文预算（16,384 token 或 64KB），没有读取", "",
			`{"code":116116,"message":"技能正文超过上下文预算（16,384 token 或 64KB），没有读取","data":null}`},
	}
	for _, c := range cases {
		if got := skillFailEnvelope(c.code, c.message, c.name); got != c.want {
			t.Errorf("%d 信封 = %s\n期望 %s", c.code, got, c.want)
		}
	}
}

// skillBlockInner 取出 `<skill …>\n` 与 `\n</skill>` 之间的正文。
func skillBlockInner(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, ">\n")
	end := strings.LastIndex(out, "\n</skill>")
	if start < 0 || end <= start {
		t.Fatalf("不是合法的 <skill> 块:\n%s", out)
	}
	return out[start+len(">\n") : end]
}

// fakeNamedTool 是只带名字的 tool.BaseTool 替身（剔除只看 Info().Name）。
type fakeNamedTool struct{ name string }

func (f *fakeNamedTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name}, nil
}
