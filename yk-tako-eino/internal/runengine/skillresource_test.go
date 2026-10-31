// skillresource_test.go 钉住 B4（doc/52 §2.3/§8.2-2/§8.2-3）的两处字节：
//
//   - read_skill_file 的路径边界与上限：越界/缺失/入口文件都回 116114 且文案一致，
//     超限回 116115 且一个字节内容都不带；目录列表不递归、目录条目不带 bytes。
//   - GET /api/runs/:id/skills 的派生：key 是 toolCallId、顺序是 seq、path 只出现在
//     read_skill_file 行、成功/失败由 result 的形态判定。
//
// 这两处错了不会崩：越界读是安全边界，上限判错是半篇资源进上下文——
// 都属于「安静地漏」，必须由逐字断言兜住。
package runengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSkillFixture 造一个目录形态的技能：references/ok.md、references/big.md、references/nested/inner.md。
func newSkillFixture(t *testing.T) SkillRow {
	t.Helper()
	dir := t.TempDir()
	entry := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(entry, []byte("---\nname: demo\ndescription: 演示\n---\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs := filepath.Join(dir, "references")
	if err := os.MkdirAll(filepath.Join(refs, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "ok.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "big.md"), []byte(strings.Repeat("a", skillBodyMaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "nested", "inner.md"), []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	return SkillRow{Name: "demo", Path: entry, Valid: true}
}

// 路径字面规则：空、绝对、任何一段 `..` 都拒；`./x` 可折叠、放行。
func TestCleanSkillRel(t *testing.T) {
	good := map[string]string{
		"references/ok.md":   "references/ok.md",
		"./references/ok.md": "references/ok.md",
		"a/b c.md":           "a/b c.md",
	}
	for in, want := range good {
		got, ok := cleanSkillRel(in)
		if !ok || got != want {
			t.Errorf("cleanSkillRel(%q) = (%q,%v)，期望 (%q,true)", in, got, ok, want)
		}
	}
	bad := []string{"", "  ", "/etc/passwd", "../x", "references/../x", "..", ".", "a/../../b"}
	for _, in := range bad {
		if got, ok := cleanSkillRel(in); ok {
			t.Errorf("cleanSkillRel(%q) 应当拒绝，实际回 %q", in, got)
		}
	}
}

// 文件成功：JSON 信封逐字（§8.2-2），bytes 与盘上一致，path 回显调用方传的相对路径。
func TestReadSkillResourceFileSuccess(t *testing.T) {
	row := newSkillFixture(t)
	got := readSkillResource(row, "references/ok.md", fakeCounter{f: func(string) int { return 10 }})
	want := `{"code":0,"message":"","data":{"name":"demo","path":"references/ok.md","type":"file","content":"hello\nworld\n","bytes":12}}`
	if got != want {
		t.Errorf("资源成功回执 =\n%s\n期望\n%s", got, want)
	}
}

// 目录：不递归的直接条目列表，按名字排序；目录条目不带 bytes，文件条目带。
func TestReadSkillResourceDirListing(t *testing.T) {
	row := newSkillFixture(t)
	got := readSkillResource(row, "references", fakeCounter{f: func(string) int { return 1 }})
	var env struct {
		Code int `json:"code"`
		Data struct {
			Name    string `json:"name"`
			Path    string `json:"path"`
			Type    string `json:"type"`
			Entries []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Bytes *int64 `json:"bytes"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(got), &env); err != nil {
		t.Fatalf("目录回执不是 JSON: %v\n%s", err, got)
	}
	if env.Code != 0 || env.Data.Type != "dir" || env.Data.Path != "references" {
		t.Fatalf("目录回执形状不对: %s", got)
	}
	names := make([]string, 0, len(env.Data.Entries))
	for _, e := range env.Data.Entries {
		names = append(names, e.Name)
		switch e.Name {
		case "nested":
			if e.Type != "dir" || e.Bytes != nil {
				t.Errorf("目录条目不该带 bytes: %+v", e)
			}
		case "big.md", "ok.md":
			if e.Type != "file" || e.Bytes == nil {
				t.Errorf("文件条目应当带 bytes: %+v", e)
			}
		default:
			t.Errorf("不该出现的条目（递归了？）: %s", e.Name)
		}
	}
	want := []string{"big.md", "nested", "ok.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("条目 = %v，期望 %v（按名字排序、不递归）", names, want)
	}
}

// 越界、缺失、入口文件、平铺形态：全部 116114 且文案一致（不透露根外有没有东西）。
func TestReadSkillResourcePathBad(t *testing.T) {
	row := newSkillFixture(t)
	bad := []string{
		"../x",
		"../../../etc/passwd",
		"/etc/passwd",
		"references/../SKILL.md",
		"SKILL.md", // 入口文件：一个文件只留 read_skill 这一条读法
		"references/missing.md",
	}
	want := `{"code":116114,"message":"` + skillPathBadMsg + `","data":{"name":"demo"}}`
	for _, p := range bad {
		if got := readSkillResource(row, p, fakeCounter{f: func(string) int { return 1 }}); got != want {
			t.Errorf("path %q =\n%s\n期望\n%s", p, got, want)
		}
	}

	// 软链跳根外：EvalSymlinks 之后不在根内，同样是 116114。
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(row.Path)
	if err := os.Symlink(outside, filepath.Join(root, "references", "leak.md")); err != nil {
		t.Fatal(err)
	}
	if got := readSkillResource(row, "references/leak.md", fakeCounter{f: func(string) int { return 1 }}); got != want {
		t.Errorf("软链越界 =\n%s\n期望\n%s", got, want)
	}

	// 平铺形态（<name>.md 自己就是根）没有资源目录。
	flat := SkillRow{Name: "demo", Path: filepath.Join(t.TempDir(), "demo.md"), Valid: true}
	if got := readSkillResource(flat, "references/ok.md", fakeCounter{f: func(string) int { return 1 }}); got != want {
		t.Errorf("平铺形态 =\n%s\n期望\n%s", got, want)
	}
}

// 上限与正文同一条判据：64KB 或 16,384 token，超了 116115，不带任何部分内容。
func TestReadSkillResourceTooLarge(t *testing.T) {
	row := newSkillFixture(t)
	want := `{"code":116115,"message":"` + skillFileTooLargeMsg + `","data":{"name":"demo"}}`
	if got := readSkillResource(row, "references/big.md", fakeCounter{f: func(string) int { return 0 }}); got != want {
		t.Errorf("64KB+1 =\n%s\n期望\n%s", got, want)
	}
	if got := readSkillResource(row, "references/ok.md", fakeCounter{f: func(string) int { return skillBodyMaxTokens + 1 }}); got != want {
		t.Errorf("16,385 token =\n%s\n期望\n%s", got, want)
	}
	if got := readSkillResource(row, "references/ok.md", fakeCounter{f: func(string) int { return skillBodyMaxTokens }}); strings.HasPrefix(got, `{"code":116`) {
		t.Errorf("恰好 16,384 token 应当放行，实际 %s", got)
	}
}

// runlog 派生：toolCallId 配对、seq 顺序、path 只在 read_skill_file 行、结局按 result 形态判。
func TestLoadsFromEntries(t *testing.T) {
	entries := []RunlogEntry{
		{Role: "assistant", Seq: 2, Text: "我看看"},
		{Role: "tool_call", Seq: 3, Agent: "", Payload: `{"name":"read_skill","args":"{\"name\":\"demo\"}","toolCallId":"c1"}`},
		{Role: "tool_result", Seq: 4, Payload: `{"name":"read_skill","output":"<skill name=\"demo\" bytes=\"12\">\n正文\n</skill>","toolCallId":"c1"}`},
		{Role: "tool_call", Seq: 5, Payload: `{"name":"use_skill","args":"{\"name\":\"off\"}","toolCallId":"c2"}`},
		{Role: "tool_result", Seq: 6, Payload: `{"name":"use_skill","output":"{\"code\":116107,\"message\":\"禁用\",\"data\":{\"name\":\"off\"}}","toolCallId":"c2"}`},
		{Role: "tool_call", Seq: 7, Payload: `{"name":"read_skill_file","args":"{\"name\":\"demo\",\"path\":\"references/ok.md\"}","toolCallId":"c3"}`},
		{Role: "tool_result", Seq: 8, Payload: `{"name":"read_skill_file","output":"{\"code\":0,\"message\":\"\",\"data\":{\"name\":\"demo\",\"path\":\"references/ok.md\",\"type\":\"file\",\"content\":\"x\",\"bytes\":1}}","toolCallId":"c3"}`},
		{Role: "tool_call", Seq: 9, Payload: `{"name":"read_skill_file","args":"{\"name\":\"demo\",\"path\":\"references/big.md\"}","toolCallId":"c4"}`},
		{Role: "tool_call", Seq: 10, Payload: `{"name":"bash","args":"{\"command\":\"ls\"}","toolCallId":"c5"}`},
		{Role: "tool_result", Seq: 11, Payload: `{"name":"bash","output":"ok","toolCallId":"c5"}`},
	}
	loads := loadsFromEntries(entries)
	if len(loads) != 4 {
		t.Fatalf("应当只留四次技能调用，实际 %d: %+v", len(loads), loads)
	}
	checks := []struct {
		tool, name, path, agent string
		seq                     int64
		ok                      bool
		code                    int
	}{
		{"read_skill", "demo", "", "", 3, true, 0},
		{"use_skill", "off", "", "", 5, false, 116107},
		{"read_skill_file", "demo", "references/ok.md", "", 7, true, 0},
		{"read_skill_file", "demo", "references/big.md", "", 9, false, 0}, // result 没落：调了，没见到结果
	}
	for i, want := range checks {
		got := loads[i]
		if got.Tool != want.tool || got.Name != want.name || got.Path != want.path ||
			got.Agent != want.agent || got.Seq != want.seq || got.OK != want.ok || got.Code != want.code {
			t.Errorf("loads[%d] = %+v，期望 %+v", i, got, want)
		}
		if got.ToolCallID == "" {
			t.Errorf("loads[%d] 缺 toolCallId", i)
		}
	}
	raw, err := json.Marshal(loads[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"toolCallId":"c1","name":"demo","tool":"read_skill","agent":"","seq":3,"ok":true}`
	if string(raw) != want {
		t.Errorf("一行字节 =\n%s\n期望\n%s", raw, want)
	}
}
