// toolroots_test.go 钉住 F1 的两层纯函数：名字白名单与扫根结果。
// 名字是路径的一段——`..` 进了 filepath.Join 就是租户根；扫根跳过 `.` 开头
// 是 PATH 前置的前提（F2），半成品不能被列出来。
package runengine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestToolNameProblem(t *testing.T) {
	ok := []string{"hello", "ffmpeg", "py3", "a", "a1-b2"}
	for _, name := range ok {
		if msg := toolNameProblem(name); msg != "" {
			t.Errorf("%q 应合法，实得 %q", name, msg)
		}
	}
	bad := []string{"", "Bash", "../evil", "a/b", "-foo", ".hidden", "bash", "sh", "env",
		"has space", "x_y", string(make([]byte, 65))}
	for _, name := range bad {
		if msg := toolNameProblem(name); msg == "" {
			t.Errorf("%q 应被拒", name)
		}
	}
}

func TestListToolsSkipsDotAndReportsExe(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, body string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("hello/bin/hello", "#!/bin/sh\necho hi\n", 0o755)
	// 前缀在、可执行文件不在：exe=false，但列表仍看得见（管理页要能删它）。
	if err := os.MkdirAll(filepath.Join(root, "broken", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 半成品与回收站都不能被列出来。
	write(".staging/abc/bin/hello", "half", 0o755)
	write(".trash/hello-1/bin/hello", "old", 0o755)

	got := listTools(root)
	if len(got) != 2 || got[0].Name != "broken" || got[1].Name != "hello" {
		t.Fatalf("应只列 broken/hello，实得 %+v", got)
	}
	if got[0].Exe {
		t.Fatalf("broken 没有可执行文件，exe 应为 false: %+v", got[0])
	}
	if !got[1].Exe || got[1].Bytes != int64(len("#!/bin/sh\necho hi\n")) {
		t.Fatalf("hello 应 exe=true 且 bytes 为文件大小: %+v", got[1])
	}
	if got[1].Path != filepath.Join(root, "hello", "bin", "hello") {
		t.Fatalf("path 应指向可执行文件: %q", got[1].Path)
	}
}
