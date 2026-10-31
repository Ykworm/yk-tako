package runengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 依赖条目形状（doc/52 D1/§6.1）：name → found → path（path 只在该条 found 时出现）。
// 这里钉的是**字节**：前端与安装响应共用同一形状，改字段序就是改契约。
func TestSkillDependencyJSONShape(t *testing.T) {
	raw, err := json.Marshal(skillCheckData{
		Dependencies: []skillDependency{
			{Name: "python3", Found: true, Path: "/usr/bin/python3"},
			{Name: "no-such-tool", Found: false},
		},
		CheckedOn: "host",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"dependencies":[{"name":"python3","found":true,"path":"/usr/bin/python3"},` +
		`{"name":"no-such-tool","found":false}],"checkedOn":"host"}`
	if got := string(raw); got != want {
		t.Fatalf("check 字节 =\n%s\n期望\n%s", got, want)
	}
	// 没声明依赖：dependencies 是 []，不是 null（前端按数组画）。
	empty, err := json.Marshal(skillCheckData{Dependencies: []skillDependency{}, CheckedOn: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(empty); got != `{"dependencies":[],"checkedOn":"host"}` {
		t.Fatalf("空依赖字节 = %s", got)
	}
}

// dependenciesInPath 是纯函数核：保序、跳过空名、找到带真实路径。
func TestDependenciesInPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(dir, "present-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps := dependenciesInPath([]string{"present-tool", " ", "no-such-tool-xyz"}, dir)
	if len(deps) != 2 {
		t.Fatalf("空名应跳过，实际 %+v", deps)
	}
	if deps[0].Name != "present-tool" || !deps[0].Found || deps[0].Path != tool {
		t.Errorf("第一条 = %+v，期望 present-tool 找到于 %s", deps[0], tool)
	}
	if deps[1].Name != "no-such-tool-xyz" || deps[1].Found || deps[1].Path != "" {
		t.Errorf("第二条 = %+v，期望找不到且不带 path", deps[1])
	}
	if missing := missingDependencies(deps); len(missing) != 1 || missing[0] != "no-such-tool-xyz" {
		t.Errorf("missing = %v，期望 [no-such-tool-xyz]", missing)
	}
}

// use_skill 的标签多 requires/missing（D38），read_skill 不带；没声明 requires 时
// 两条路逐字节相同。missing 用的是与 bash 同一条过滤后的 PATH（D39）。
func TestRenderUsedSkillBodyRequiresMissing(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "present-tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{dir, "/usr/bin"}, string(os.PathListSeparator)))

	row := SkillRow{
		Name: "demo", Scope: "tenant", Bytes: 7,
		Requires: `["present-tool","no-such-tool-xyz"]`,
	}
	body := "正文"
	counter := fakeCounter{f: func(string) int { return 1 }}
	used := renderUsedSkillBody(row, body, counter, dir)
	want := `<skill name="demo" source="tenant" bytes="7" requires="present-tool no-such-tool-xyz" missing="no-such-tool-xyz">` + "\n正文\n</skill>"
	if used != want {
		t.Fatalf("use_skill 标签 =\n%s\n期望\n%s", used, want)
	}
	read := renderSkillBody(row, body, counter)
	if read != `<skill name="demo" source="tenant" bytes="7">`+"\n正文\n</skill>" {
		t.Fatalf("read_skill 不该带 requires/missing，实际 %s", read)
	}
	// 声明的都找到时 missing 写空串（不是省略这个属性）。
	row.Requires = `["present-tool"]`
	used = renderUsedSkillBody(row, body, counter, dir)
	if !strings.Contains(used, `requires="present-tool" missing=""`) {
		t.Errorf("全找到时 missing 应为空串，实际 %s", used)
	}
	// 没声明：两条路逐字节相同。
	row.Requires = ""
	used = renderUsedSkillBody(row, body, counter, dir)
	if used != renderSkillBody(row, body, counter) {
		t.Errorf("没声明 requires 时 use_skill 应与 read_skill 同字节:\n%s\n%s", used, used)
	}
}
