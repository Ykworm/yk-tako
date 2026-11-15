// runviews_test.go 覆盖 doc/78 §4.1 的两个纯函数：调用记录 → 改动文件（桶判定 + 归一 + 去重），
// 以及磁盘过滤（只留当前能打开的普通文件）。处理器本身靠 contract_test 与 expected.json 用例 65。
package runengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func toolCallEntry(t *testing.T, name, args string) RunlogEntry {
	t.Helper()
	payload, err := json.Marshal(toolCallPayload{Name: name, Args: args, ToolCallID: "tc"})
	if err != nil {
		t.Fatalf("构造 payload 失败：%v", err)
	}
	return RunlogEntry{Role: "tool_call", Payload: string(payload)}
}

func changedPaths(files []runChangedFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path+":"+f.Op)
	}
	return out
}

func TestChangedFilesFromEntriesBuckets(t *testing.T) {
	dir := t.TempDir()
	entries := []RunlogEntry{
		toolCallEntry(t, "write_file", `{"path":"a.py"}`),
		toolCallEntry(t, "edit_file", `{"path":"b.py"}`),
		toolCallEntry(t, "read_file", `{"path":"c.py"}`),
		toolCallEntry(t, "read_media_file", `{"path":"d.png"}`),
		toolCallEntry(t, "list_directory", `{"path":"."}`),
	}
	got := changedPaths(changedFilesFromEntries(entries, firstPartyFileEffects, dir))
	want := []string{"a.py:write", "b.py:edit"}
	if len(got) != len(want) {
		t.Fatalf("改动清单 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("改动清单 = %v，期望 %v", got, want)
		}
	}
}

func TestChangedFilesFromEntriesDedupeKeepsFirstPositionLastOp(t *testing.T) {
	dir := t.TempDir()
	entries := []RunlogEntry{
		toolCallEntry(t, "write_file", `{"path":"a.py"}`),
		toolCallEntry(t, "write_file", `{"path":"b.py"}`),
		toolCallEntry(t, "edit_file", `{"path":"a.py"}`),
		toolCallEntry(t, "write_file", `{"path":"b.py"}`),
	}
	got := changedPaths(changedFilesFromEntries(entries, firstPartyFileEffects, dir))
	want := []string{"a.py:edit", "b.py:write"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("去重结果 = %v，期望 %v（位置取首次、op 取最后一次）", got, want)
	}
}

func TestChangedFilesFromEntriesBashWrites(t *testing.T) {
	dir := t.TempDir()
	entries := []RunlogEntry{
		toolCallEntry(t, "bash", `{"command":"echo hi > out.txt"}`),
		toolCallEntry(t, "bash", `{"command":"cp a b"}`),
		toolCallEntry(t, "bash", `{"command":"echo x | tee c"}`),
		toolCallEntry(t, "bash", `{"command":"sed -i s/x/y/ d"}`),
		toolCallEntry(t, "bash", `{"command":"dd if=/dev/zero of=e count=1"}`),
		toolCallEntry(t, "bash", `{"command":"cat x"}`),
		toolCallEntry(t, "bash", `{"command":"ls -la"}`),
	}
	got := changedPaths(changedFilesFromEntries(entries, firstPartyFileEffects, dir))
	want := []string{"out.txt:write", "b:write", "c:write", "d:write", "e:write"}
	if len(got) != len(want) {
		t.Fatalf("bash 写入 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bash 写入 = %v，期望 %v", got, want)
		}
	}
}

func TestChangedFilesFromEntriesBadPayload(t *testing.T) {
	dir := t.TempDir()
	entries := []RunlogEntry{
		{Role: "tool_call", Payload: "not json"},
		toolCallEntry(t, "write_file", `{"path":"ok.txt"}`),
	}
	got := changedPaths(changedFilesFromEntries(entries, firstPartyFileEffects, dir))
	if len(got) != 1 || got[0] != "ok.txt:write" {
		t.Fatalf("坏 payload 应跳过、不影响后续行，得到 %v", got)
	}
}

func TestChangedFilesFromEntriesEmptyIsArray(t *testing.T) {
	files := changedFilesFromEntries(nil, firstPartyFileEffects, t.TempDir())
	if files == nil {
		t.Fatal("空清单必须是非 nil 空切片（JSON 要是 []）")
	}
	b, err := json.Marshal(runChangedFilesData{RunID: "r_1", Files: files})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"runId":"r_1","files":[]}`
	if string(b) != want {
		t.Fatalf("空清单字节 = %s，期望 %s", b, want)
	}
}

func TestNormalizeContentRelPath(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a.py", "a.py", true},
		{"./a.py", "a.py", true},
		{"sub/../a.py", "a.py", true},
		{"..", "", false},
		{"../a", "", false},
		{"sub/../../a", "", false},
		{".tako/x", "", false},
		{".tako", "", false},
		{".media/x", "", false},
		{"", "", false},
		{"   ", "", false},
		{" . ", "", false},
		{filepath.Join(dir, "sub", "a.py"), "sub/a.py", true},
		{filepath.Join(dir, "..", "outside.txt"), "", false},
		{"/etc/passwd", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeContentRelPath(c.in, dir)
		if ok != c.ok || got != c.want {
			t.Fatalf("normalize(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestPruneMissingChangedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []runChangedFile{
		{Path: "keep.txt", Op: "write"},
		{Path: "adir", Op: "write"},
		{Path: "missing.txt", Op: "edit"},
	}
	got := pruneMissingChangedFiles(dir, files)
	if len(got) != 1 || got[0].Path != "keep.txt" {
		t.Fatalf("磁盘过滤后 = %v，期望只剩 keep.txt", got)
	}
}
