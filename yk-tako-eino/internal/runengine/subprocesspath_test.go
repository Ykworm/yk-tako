package runengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PATH 过滤（doc/52 D39）：围栏读不到的段丢掉、逐段 EvalSymlinks、保序去重。
//
// 用临时目录当「项目根 / 系统目录」，不依赖本机装了什么——判据是前缀与真实路径，
// 与机器状态无关。t.TempDir() 在 macOS 上落在 /private/var/...，两侧都取 EvalSymlinks
// 后的坐标，断的才是规则本身。
func TestFilterSubprocessPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "bin")
	otherDir := filepath.Join(base, "other")
	for _, d := range []string{binDir, otherDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 链接段：解析后与 binDir 相同，应当被去重掉（不是当第二段）。
	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(binDir, linkDir); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	missing := filepath.Join(base, "does-not-exist")
	outside := filepath.Join(os.TempDir(), "tako-d39-outside")
	_ = os.MkdirAll(outside, 0o755)

	raw := strings.Join([]string{
		binDir, otherDir, binDir, missing, linkDir, outside, "", base,
	}, sep)
	got := filterSubprocessPath(raw, []string{base})
	want := strings.Join([]string{binDir, otherDir, base}, sep)
	if got != want {
		t.Fatalf("过滤结果 = %q，期望 %q", got, want)
	}
}

// 顺序与真实路径：先出现的保序；`/bin` 与 `/usr/bin` 解析成同一段只留一个。
// 这两条在 macOS 上是默认事实（/bin -> /usr/bin），断不了也不该把测试弄红——
// 只在两个目录都真实存在时才断。
func TestFilterSubprocessPathStableOrderAndRealPaths(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := filterSubprocessPath(strings.Join([]string{"/bin", "/usr/bin", "/usr/bin"}, sep), []string{"/usr"})
	if got != "/usr/bin" {
		t.Fatalf("/bin 与 /usr/bin 应收敛成一段 /usr/bin，实际 %q", got)
	}
	// 围栏外的段整段丢掉（HOME 下的工具链是 D39 的典型病灶）。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("取不到 HOME")
	}
	out := filterSubprocessPath(strings.Join([]string{home + "/.local/bin", "/usr/bin"}, sep), []string{"/usr"})
	if out != "/usr/bin" {
		t.Fatalf("HOME 下的段应被丢掉，实际 %q", out)
	}
}

// subprocessPath 是 filter 的宿主环境入口：workspace 作为一个额外根参与保留判定。
func TestSubprocessPathKeepsWorkspace(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{base, "/usr/bin", filepath.Join(os.TempDir(), "tako-d39-nope")}, string(os.PathListSeparator)))
	got := subprocessPath(base, "")
	want := strings.Join([]string{base, "/usr/bin"}, string(os.PathListSeparator))
	if got != want {
		t.Fatalf("subprocessPath = %q，期望 %q", got, want)
	}
}

// lookPathIn 与 bash 同一把尺：只认可执行名、只按路径找、不试跑；
// 找到回真实路径（顺带证明符号链接按目标解析）。
func TestLookPathIn(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(dir, "present-tool")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias-tool")
	if err := os.Symlink(toolPath, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-directory"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got, ok := lookPathIn(dir, "present-tool"); !ok || got != toolPath {
		t.Errorf("present-tool = %q,%v，期望 %q,true", got, ok, toolPath)
	}
	if got, ok := lookPathIn(dir, "alias-tool"); !ok || got != toolPath {
		t.Errorf("alias-tool 应解析到目标 %q，实际 %q,%v", toolPath, got, ok)
	}
	for _, name := range []string{"plain.txt", "a-directory", "missing", "", "sub/present-tool", dir + "/present-tool"} {
		if got, ok := lookPathIn(dir, name); ok {
			t.Errorf("%q 不该被当成依赖找到，实际 %q", name, got)
		}
	}
	// 搜索顺序：靠前的目录先赢。
	second := filepath.Join(dir, "second")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(second, "present-tool")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := strings.Join([]string{dir, second}, string(os.PathListSeparator))
	if got, ok := lookPathIn(env, "present-tool"); !ok || got != toolPath {
		t.Errorf("PATH 靠前的目录应赢，实际 %q,%v", got, ok)
	}
}

// 工具根（D36 F2）：`<tenant>/tools/*/bin` 排在宿主 PATH 前面，且跳过 `.` 开头的目录。
// 半成品（.staging）与已删除的旧版本（.trash）拼进 PATH，就是它们抢先被 exec。
func TestSubprocessPathPrependsToolBins(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(base, "tools")
	for _, rel := range []string{"hello/bin", "py3/bin", ".staging/abc/bin", ".trash/hello-1/bin"} {
		if err := os.MkdirAll(filepath.Join(tools, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "/usr/bin")
	got := subprocessPath(base, tools)
	want := strings.Join([]string{
		filepath.Join(tools, "hello", "bin"),
		filepath.Join(tools, "py3", "bin"),
		"/usr/bin",
	}, string(os.PathListSeparator))
	if got != want {
		t.Fatalf("subprocessPath = %q，期望 %q", got, want)
	}
	// 宿主里与工具根撞名的段被去重（工具根在前，赢的是它）。
	got = subprocessPath(base, tools)
	if strings.Count(got, filepath.Join(tools, "hello", "bin")) != 1 {
		t.Fatalf("工具根段应只出现一次：%q", got)
	}
}

// 目录形状推导租户根：项目/会话根都是 `<tenant>/{workspaces,sessions}/<id>`。
func TestTenantRootFromDir(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws", "tenants", "t_1")
	cases := map[string]string{
		filepath.Join(root, "workspaces", "w1"): root,
		filepath.Join(root, "sessions", "s1"):   root,
		filepath.Join(root, "skills"):           "",
		"":                                      "",
	}
	for dir, want := range cases {
		if got := tenantRootFromDir(dir); got != want {
			t.Errorf("tenantRootFromDir(%q) = %q，期望 %q", dir, got, want)
		}
	}
}
