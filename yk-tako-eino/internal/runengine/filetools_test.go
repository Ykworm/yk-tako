// filetools_test.go 是 F1 的钉子网：resolve 边界表（含「前缀像但非子路径」那条）、
// write_file 行为（写/覆盖/父目录）、错误回注语义（裸工具抛 error vs 包装后变结果）、
// 以及两个登记点的配对 lint。活体验收（curl + 磁盘对字节）在台账 F1 拍。
package runengine

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

// resolve 边界表：正常相对路径 / 点段归一 / 上跳越界 / 绝对路径 / 根本身 / 前缀同形目录。
func TestResolveInWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{"正常相对路径", "notes/todo.txt", filepath.Join(root, "notes", "todo.txt"), false},
		{"点段与冗余分隔符归一", "./a//b/../c.txt", filepath.Join(root, "a", "c.txt"), false},
		{"上跳越界", "../evil.txt", "", true},
		{"绝对路径直接拒", "/etc/passwd", "", true},
		{"根本身允许", ".", root, false},
		{"前缀同形目录不算子路径", "../ws-other/f.txt", "", true},
	}
	for _, c := range cases {
		got, err := resolveInWorkspace(root, c.path)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: resolve(%q) = %q，want error", c.name, c.path, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: resolve(%q) 意外报错: %v", c.name, c.path, err)
		}
		if got != c.want {
			t.Fatalf("%s: resolve(%q) = %q, want %q", c.name, c.path, got, c.want)
		}
	}
}

// write_file 行为：写 → 磁盘对字节；覆盖 → 返回带「（覆盖）」且磁盘是新内容。
func TestWriteFileToolBasicAndOverwrite(t *testing.T) {
	root := t.TempDir()
	raw, err := newWriteFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"sub/hello.txt","content":"tako f1"}`)
	if err != nil {
		t.Fatalf("首次写入报错: %v", err)
	}
	if want := "已写入 sub/hello.txt（7 字节）"; out != want {
		t.Fatalf("首次写入返回 %q, want %q", out, want)
	}
	disk, err := os.ReadFile(filepath.Join(root, "sub", "hello.txt"))
	if err != nil {
		t.Fatalf("磁盘读回: %v", err)
	}
	if string(disk) != "tako f1" {
		t.Fatalf("磁盘内容 %q, want %q", disk, "tako f1")
	}

	out, err = it.InvokableRun(ctx, `{"path":"sub/hello.txt","content":"tako f1 again"}`)
	if err != nil {
		t.Fatalf("覆盖写入报错: %v", err)
	}
	if want := "已写入 sub/hello.txt（13 字节）（覆盖）"; out != want {
		t.Fatalf("覆盖写入返回 %q, want %q", out, want)
	}
	disk, _ = os.ReadFile(filepath.Join(root, "sub", "hello.txt"))
	if string(disk) != "tako f1 again" {
		t.Fatalf("覆盖后磁盘内容 %q, want %q", disk, "tako f1 again")
	}
}

// 错误回注语义：裸工具越界 = error 上抛（ToolsNode 遇此让 run 失败——这就是必须包的理由）；
// wrapToolErrors 之后 = 「错误：…」文本 + error 为 nil（回注模型、run 照跑）；两次都不落盘。
func TestWriteFileToolErrorAsResult(t *testing.T) {
	root := t.TempDir()
	raw, err := newWriteFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	if _, err := it.InvokableRun(ctx, `{"path":"../evil.txt","content":"x"}`); err == nil {
		t.Fatal("裸工具越界应当返回 error")
	}

	wrapped := wrapToolErrors(raw).(tool.InvokableTool)
	out, err := wrapped.InvokableRun(ctx, `{"path":"../evil.txt","content":"x"}`)
	if err != nil {
		t.Fatalf("包装后不该再抛 error: %v", err)
	}
	// 逐字节钉死：带「错误：」锚 + eino 的 [LocalFunc] 外壳已剥（活体验收所见噪音）。
	if want := "错误：路径越出 workspace（../evil.txt）"; out != want {
		t.Fatalf("包装后结果 %q, want %q", out, want)
	}

	wrapped2 := wrapToolErrors(raw).(tool.InvokableTool)
	out2, err := wrapped2.InvokableRun(ctx, `{"path":"/etc/passwd","content":"x"}`)
	if err != nil || !strings.HasPrefix(out2, "错误：") {
		t.Fatalf("绝对路径包装后应得错误文本: out=%q err=%v", out2, err)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("越界文件不该落盘（stat err = %v）", err)
	}
}

// 两个登记点配对（F1–F11 全量 + memory 表态 none）：11 把文件工具的声明与 bucket 逐一钉死、
// memory 显式声明 none，lint 必须放行。
// 漏一行 = 池里有工具而注册表没有（lint 报启动失败），声明错 = fileOps 分桶错（这里先钉住）。
func TestFirstPartyFileEffectsPassLint(t *testing.T) {
	if err := lintFileEffects(firstPartyFileEffects); err != nil {
		t.Fatalf("lint 拒绝了自己的注册表: %v", err)
	}
	want := map[string]string{
		"write_file":       fileEffectWrite,
		"read_text_file":   fileEffectRead,
		"read_file":        fileEffectRead,
		"edit_file":        fileEffectEdit,
		"move_file":        fileEffectEdit,
		"create_directory": fileEffectWrite,
		"list_directory":   fileEffectRead,
		"directory_tree":   fileEffectRead,
		"get_file_info":    fileEffectRead,
		"search_files":     fileEffectRead,
		"read_media_file":  fileEffectRead,
		"memory":           fileEffectNone,
	}
	if len(firstPartyFileEffects) != len(want) {
		t.Fatalf("注册表条目数 %d, want %d（11 文件工具 + memory）", len(firstPartyFileEffects), len(want))
	}
	for name, effect := range want {
		if firstPartyFileEffects[name] != effect {
			t.Fatalf("%s 声明 = %q, want %q", name, firstPartyFileEffects[name], effect)
		}
	}
}

// read_text_file：全文直出 / head / tail（尾换行不折出空行）/ 同给报错 / 非正数报错 / 不存在 / 目录。
func TestReadTextFileBasicHeadTail(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newReadTextFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"lines.txt"}`)
	if err != nil {
		t.Fatalf("全文读取报错: %v", err)
	}
	if out != sb.String() {
		t.Fatalf("全文直出漂移: got %q", out)
	}

	out, err = it.InvokableRun(ctx, `{"path":"lines.txt","head":3}`)
	if err != nil || out != "line 1\nline 2\nline 3" {
		t.Fatalf("head=3: out=%q err=%v", out, err)
	}

	out, err = it.InvokableRun(ctx, `{"path":"lines.txt","tail":2}`)
	if err != nil || out != "line 29\nline 30" {
		t.Fatalf("tail=2: out=%q err=%v", out, err)
	}

	if _, err = it.InvokableRun(ctx, `{"path":"lines.txt","head":1,"tail":1}`); err == nil || !strings.Contains(err.Error(), "不能同时") {
		t.Fatalf("head+tail 同给应报错: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"lines.txt","head":0}`); err == nil || !strings.Contains(err.Error(), "正整数") {
		t.Fatalf("head=0 应报非正数: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"nope.txt"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失文件应报不存在: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"."}`); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("读目录应报目录: %v", err)
	}
}

// 200KB 减灾档：无截读参数被挡（错误带上限提示），head 放行、恰 5 行。
func TestReadTextFileSizeGuard(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for sb.Len() <= 200*1024 {
		sb.WriteString("row 0123456789\n") // 15 字节/行
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newReadTextFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	if _, err = it.InvokableRun(ctx, `{"path":"big.txt"}`); err == nil || !strings.Contains(err.Error(), "200KB") {
		t.Fatalf(">200KB 无截读应报上限提示: %v", err)
	}
	out, err := it.InvokableRun(ctx, `{"path":"big.txt","head":5}`)
	want := strings.Repeat("row 0123456789\n", 4) + "row 0123456789"
	if err != nil || out != want {
		t.Fatalf("head=5: out=%q err=%v", out, err)
	}
}

// edit_file：恰一处才动 / 0 处报错 / 多命中默认拒 / replace_all 全替 / 空 old_text / 缺失 / 目录。
func TestEditFileTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "edit.txt"), []byte("alpha beta gamma"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dup.txt"), []byte("aaa\naaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newEditFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"edit.txt","old_text":"beta","new_text":"BETA"}`)
	if err != nil || out != "已编辑 edit.txt（替换 1 处）" {
		t.Fatalf("单处替换: out=%q err=%v", out, err)
	}
	disk, _ := os.ReadFile(filepath.Join(root, "edit.txt"))
	if string(disk) != "alpha BETA gamma" {
		t.Fatalf("磁盘内容 %q, want %q", disk, "alpha BETA gamma")
	}

	if _, err = it.InvokableRun(ctx, `{"path":"edit.txt","old_text":"notexist","new_text":"x"}`); err == nil || !strings.Contains(err.Error(), "没有这段") {
		t.Fatalf("0 处命中应报没有这段: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"dup.txt","old_text":"aaa","new_text":"bbb"}`); err == nil || !strings.Contains(err.Error(), "出现 2 次") {
		t.Fatalf("多命中默认应报次数: %v", err)
	}
	disk, _ = os.ReadFile(filepath.Join(root, "dup.txt"))
	if string(disk) != "aaa\naaa\n" {
		t.Fatalf("多命中被拒不落盘，磁盘 %q", disk)
	}

	out, err = it.InvokableRun(ctx, `{"path":"dup.txt","old_text":"aaa","new_text":"bbb","replace_all":true}`)
	if err != nil || out != "已编辑 dup.txt（替换 2 处）" {
		t.Fatalf("replace_all: out=%q err=%v", out, err)
	}
	disk, _ = os.ReadFile(filepath.Join(root, "dup.txt"))
	if string(disk) != "bbb\nbbb\n" {
		t.Fatalf("replace_all 后磁盘 %q", disk)
	}

	if _, err = it.InvokableRun(ctx, `{"path":"edit.txt","old_text":"","new_text":"x"}`); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空 old_text 应报错: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"nope.txt","old_text":"a","new_text":"b"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失文件应报不存在: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":".","old_text":"a","new_text":"b"}`); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("编辑目录应报目录: %v", err)
	}
}

// read_file 别名（F4）：声明面真名 = read_file，行为与 read_text_file 同体（含 head 截读）。
func TestReadFileAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("alias body\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	rawText, err := newReadTextFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	rawAlias, err := newReadFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	infoText, err := rawText.Info(ctx)
	if err != nil || infoText.Name != "read_text_file" {
		t.Fatalf("read_text_file 声明名 = %q err=%v", infoText.Name, err)
	}
	infoAlias, err := rawAlias.Info(ctx)
	if err != nil || infoAlias.Name != "read_file" {
		t.Fatalf("read_file 声明名 = %q err=%v", infoAlias.Name, err)
	}

	it := rawAlias.(tool.InvokableTool)
	out, err := it.InvokableRun(ctx, `{"path":"hello.txt"}`)
	if err != nil || out != "alias body\nsecond\n" {
		t.Fatalf("别名全文读取: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"hello.txt","head":1}`)
	if err != nil || out != "alias body" {
		t.Fatalf("别名 head=1: out=%q err=%v", out, err)
	}
}

// move_file：移动（父目录自动建）/ 目标已存在不覆盖 / 源不存在。
func TestMoveFileTool(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.txt", "c.txt", "d.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("content of "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := newMoveFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"a.txt","destination":"sub/b.txt"}`)
	if err != nil || out != "已移动 a.txt → sub/b.txt" {
		t.Fatalf("移动: out=%q err=%v", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Fatalf("目标未落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("源应已挪走，stat err=%v", err)
	}

	if _, err = it.InvokableRun(ctx, `{"path":"c.txt","destination":"d.txt"}`); err == nil || !strings.Contains(err.Error(), "目标已存在") {
		t.Fatalf("覆盖目标应被拒: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "c.txt")); err != nil {
		t.Fatalf("反例后源文件应原样: %v", err)
	}

	if _, err = it.InvokableRun(ctx, `{"path":"nope.txt","destination":"x.txt"}`); err == nil || !strings.Contains(err.Error(), "源不存在") {
		t.Fatalf("缺失源应报源不存在: %v", err)
	}
}

// create_directory：递归建 / 幂等成功 / 同名文件拒 / 根本身幂等。
func TestCreateDirectoryTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newCreateDirectoryTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	if out, err := it.InvokableRun(ctx, `{"path":"notes/2026"}`); err != nil || out != "已建目录 notes/2026" {
		t.Fatalf("递归建目录: out=%q err=%v", out, err)
	}
	fi, err := os.Stat(filepath.Join(root, "notes", "2026"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("目录未落盘: %v", err)
	}

	if out, err := it.InvokableRun(ctx, `{"path":"notes/2026"}`); err != nil || out != "已建目录 notes/2026（已存在）" {
		t.Fatalf("重复建应幂等: out=%q err=%v", out, err)
	}
	if out, err := it.InvokableRun(ctx, `{"path":"."}`); err != nil || out != "已建目录 .（已存在）" {
		t.Fatalf("根已存在应幂等: out=%q err=%v", out, err)
	}
	if _, err := it.InvokableRun(ctx, `{"path":"f.txt"}`); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("同名文件应被拒: %v", err)
	}
}

// list_directory：排序与 / 后缀形状 / 空目录 / 不是目录 / 不存在。
func TestListDirectoryTool(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mix", "d1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mix", "f1.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newListDirectoryTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"mix"}`)
	if err != nil || out != "d1/\nf1.txt" {
		t.Fatalf("mix 列举: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"."}`)
	if err != nil || out != "empty/\nmix/" {
		t.Fatalf("根列举: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"empty"}`)
	if err != nil || out != "（空目录）" {
		t.Fatalf("空目录: out=%q err=%v", out, err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"mix/f1.txt"}`); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("文件应报不是目录: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"nope"}`); err == nil || !strings.Contains(err.Error(), "目录不存在") {
		t.Fatalf("缺失目录应报不存在: %v", err)
	}
}

// directory_tree：默认 depth 3 在 d 层处标 … / depth=5 展开到 d / 超上限钳到 5 / depth≤0 拒 /
// 节点 500 截断 / 根行为 path 原文 + /。
func TestDirectoryTreeTool(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "c", "d", "x.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f0.txt"), []byte("f0"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newDirectoryTreeTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{}`)
	wantDefault := "./\n  a/\n    b/\n      c/\n        …\n  f0.txt"
	if err != nil || out != wantDefault {
		t.Fatalf("默认树: out=%q err=%v", out, err)
	}

	out, err = it.InvokableRun(ctx, `{"depth":5}`)
	wantDeep := "./\n  a/\n    b/\n      c/\n        d/\n          x.txt\n  f0.txt"
	if err != nil || out != wantDeep {
		t.Fatalf("depth=5: out=%q err=%v", out, err)
	}

	out, err = it.InvokableRun(ctx, `{"depth":10}`)
	if err != nil || out != wantDeep {
		t.Fatalf("depth=10 应钳到 5 与 depth=5 同形: out=%q err=%v", out, err)
	}
	if _, err = it.InvokableRun(ctx, `{"depth":0}`); err == nil || !strings.Contains(err.Error(), "正整数") {
		t.Fatalf("depth=0 应拒: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"f0.txt"}`); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("文件应报不是目录: %v", err)
	}

	root2 := t.TempDir()
	for i := 0; i < 600; i++ {
		if err := os.WriteFile(filepath.Join(root2, fmt.Sprintf("f%03d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw2, err := newDirectoryTreeTool(root2)
	if err != nil {
		t.Fatal(err)
	}
	out, err = raw2.(tool.InvokableTool).InvokableRun(ctx, `{}`)
	if err != nil || strings.Count(out, "\n") != 501 || !strings.HasSuffix(out, "（已截断）") {
		t.Fatalf("600 文件应 500 节点截断: lines=%d suffix=%v err=%v", strings.Count(out, "\n"), strings.HasSuffix(out, "（已截断）"), err)
	}
}

// get_file_info：四行契约（size 带「字节」后缀 / modified 可解析 RFC3339 / mode 符号串（-rw-r--r--）/ type 文件与目录）。
func TestGetFileInfoTool(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(p, []byte("tako f1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newGetFileInfoTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatalf("info 报错: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("应四行，得 %d 行: %q", len(lines), out)
	}
	if lines[0] != "size: 7 字节" {
		t.Fatalf("size 行 %q, want size: 7 字节", lines[0])
	}
	if rest, ok := strings.CutPrefix(lines[1], "modified: "); !ok {
		t.Fatalf("modified 行 %q", lines[1])
	} else if _, err := time.Parse(time.RFC3339, rest); err != nil {
		t.Fatalf("modified 非 RFC3339: %v", err)
	}
	if lines[2] != "mode: -rw-r--r--" {
		t.Fatalf("mode 行 %q, want mode: -rw-r--r--", lines[2])
	}
	if lines[3] != "type: 文件" {
		t.Fatalf("type 行 %q, want type: 文件", lines[3])
	}

	out, err = it.InvokableRun(ctx, `{"path":"."}`)
	if err != nil || !strings.HasSuffix(out, "type: 目录") {
		t.Fatalf("目录 type: out=%q err=%v", out, err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"nope"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失应报不存在: %v", err)
	}
}

// search_files：命中格式与行序 / 目录参数（relpath 相对根）/ glob / 无命中 / 不是目录 /
// 50 条封顶标记 / >1MB 跳过。
func TestSearchFilesTool(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"README.md": "docs\nneedleX here\n",
		"src/a.go":  "package a\n// needleX\n",
		"src/b.go":  "needleX\nneedleX\n",
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := newSearchFilesTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":".","query":"needleX"}`)
	want := "README.md:2:needleX here\nsrc/a.go:2:// needleX\nsrc/b.go:1:needleX\nsrc/b.go:2:needleX"
	if err != nil || out != want {
		t.Fatalf("全量搜索: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"src","query":"needleX"}`)
	if err != nil || out != "src/a.go:2:// needleX\nsrc/b.go:1:needleX\nsrc/b.go:2:needleX" {
		t.Fatalf("src 搜索: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":".","query":"needleX","file_glob":"*.go"}`)
	if err != nil || out != "src/a.go:2:// needleX\nsrc/b.go:1:needleX\nsrc/b.go:2:needleX" {
		t.Fatalf("glob 搜索: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":".","query":"zzz"}`)
	if err != nil || out != "（无命中）" {
		t.Fatalf("无命中: out=%q err=%v", out, err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"README.md","query":"x"}`); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("文件应报不是目录: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":".","query":""}`); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空 query 应拒: %v", err)
	}

	root2 := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&sb, "needleX %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root2, "cap.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", 1<<20) + "needleX"
	if err := os.WriteFile(filepath.Join(root2, "big.bin"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	raw2, err := newSearchFilesTool(root2)
	if err != nil {
		t.Fatal(err)
	}
	it2 := raw2.(tool.InvokableTool)
	out, err = it2.InvokableRun(ctx, `{"path":".","query":"needleX","file_glob":"cap.txt"}`)
	if err != nil || strings.Count(out, "\n") != 50 || !strings.HasSuffix(out, "（更多命中已省略）") {
		t.Fatalf("60 命中应 50 封顶: lines=%d err=%v", strings.Count(out, "\n"), err)
	}
	out, err = it2.InvokableRun(ctx, `{"path":".","query":"needleX","file_glob":"big.bin"}`)
	if err != nil || out != "（无命中）" {
		t.Fatalf(">1MB 应跳过: out=%q err=%v", out, err)
	}
}

// read_media_file：data URI 逐字节（base64 可解回原文）/ 扩展名映射 / 未知扩展名兜底 /
// 缺失与目录 / >5MB 拒。
func TestReadMediaFileTool(t *testing.T) {
	root := t.TempDir()
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x01, 0x02, 0x03}
	if err := os.WriteFile(filepath.Join(root, "pic.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.dat"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := newReadMediaFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	ctx := context.Background()

	out, err := it.InvokableRun(ctx, `{"path":"pic.png"}`)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if err != nil || out != want {
		t.Fatalf("png data URI: out=%q err=%v", out, err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"blob.dat"}`)
	if err != nil || !strings.HasPrefix(out, "data:application/octet-stream;base64,") {
		t.Fatalf("未知扩展名兜底: out=%q err=%v", out, err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"nope.png"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失应报不存在: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"."}`); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("目录应拒: %v", err)
	}

	huge := make([]byte, 5*1024*1024+1)
	if err := os.WriteFile(filepath.Join(root, "big.png"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"big.png"}`); err == nil || !strings.Contains(err.Error(), "5MB") {
		t.Fatalf(">5MB 应拒: %v", err)
	}
}
