// filefp_test.go 是「同文件并发写」底座的钉子网（方案真源 = tmp/文件写入并发/planning.md）：
// 指纹判据、基准表的 agent 隔离、flock 的互斥与崩溃释放、write_file 的三条写前拒绝、
// 以及最关键的那条性质——「edit_file 会把别的写者落盘的改动原样带过去」。
package runengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

// ── 指纹 ──────────────────────────────────────────────────────────────────

func TestFingerprintEqualAndDiffer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp1, err := fingerprintOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if fp1.SHA256 == "" {
		t.Fatalf("小文件应当有内容哈希，实际降级了")
	}
	fp2, _ := fingerprintOf(p)
	if !fp1.equal(fp2) {
		t.Fatalf("同一文件两次指纹应相等")
	}
	// 内容变了 → 不等
	if err := os.WriteFile(p, []byte("hell0"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp3, _ := fingerprintOf(p)
	if fp1.equal(fp3) {
		t.Fatalf("内容变了指纹仍判相等")
	}
	// 内容没变、只 touch（mtime 变）→ 仍判相等（判据是内容，不是 mtime）
	if err := os.WriteFile(p, []byte("hell0"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp4, _ := fingerprintOf(p)
	if !fp3.equal(fp4) {
		t.Fatalf("内容相同应判相等（mtime 变不算冲突）")
	}
}

// 大文件降级：>64MB 不算 sha256，weak=true；判据退回 (mtime, size)。
func TestFingerprintWeakFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// 稀疏文件：不真写 64MB，stat 的 size 却是 64MB+1
	if err := f.Truncate(fingerprintHashMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	fp, err := fingerprintOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fp.weak() {
		t.Fatalf("超过 %d 字节应当降级（weak=true）", fingerprintHashMaxBytes)
	}
	if fp.SHA256 != "" {
		t.Fatalf("降级时不应带内容哈希")
	}
	if fp.Size != fingerprintHashMaxBytes+1 {
		t.Fatalf("size %d, want %d", fp.Size, fingerprintHashMaxBytes+1)
	}
}

// ── 基准表 ────────────────────────────────────────────────────────────────

func TestFileBaselineAgentIsolation(t *testing.T) {
	b := newFileBaseline()
	fp := Fingerprint{Mtime: time.Unix(1, 0), Size: 3, SHA256: "aa"}
	b.set("root", "/x/a.md", fp)
	if _, ok := b.get("root", "/x/a.md"); !ok {
		t.Fatalf("root 应能读到自己的基准")
	}
	if _, ok := b.get("coder", "/x/a.md"); ok {
		t.Fatalf("子代理不应借用主代理的「读过」")
	}
	b.set("coder", "/x/a.md", Fingerprint{SHA256: "bb"})
	got, _ := b.get("coder", "/x/a.md")
	if got.SHA256 != "bb" {
		t.Fatalf("子代理基准应独立，got %q", got.SHA256)
	}
	if got, _ := b.get("root", "/x/a.md"); got.SHA256 != "aa" {
		t.Fatalf("主代理基准被子代理覆盖，got %q", got.SHA256)
	}
}

// ── 写锁 ──────────────────────────────────────────────────────────────────

// 同路径两个写者必须串行（不交叠）：flock 挂在 open file description 上，同进程两次 open
// 同一文件是两个不同的描述 → 互斥。
func TestFileLockSerializesSamePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	var mu sync.Mutex
	var seq []string
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			unlock, err := lockPaths(dir, []string{target})
			if err != nil {
				t.Errorf("取锁失败: %v", err)
				return
			}
			mu.Lock()
			seq = append(seq, fmt.Sprintf("enter%d", id))
			mu.Unlock()
			time.Sleep(120 * time.Millisecond)
			mu.Lock()
			seq = append(seq, fmt.Sprintf("exit%d", id))
			mu.Unlock()
			unlock()
		}(i)
	}
	wg.Wait()
	if len(seq) != 4 {
		t.Fatalf("事件数 %d, want 4（%v）", len(seq), seq)
	}
	// 必须形如 enter,exit,enter,exit —— 不能 enter,enter
	if seq[1][0] != 'e' || seq[1][0] == 'e' && seq[1][:4] == "ente" {
		t.Fatalf("两个写者交叠了（没串行）：%v", seq)
	}
	if !strings.HasPrefix(seq[0], "enter") || !strings.HasPrefix(seq[1], "exit") {
		t.Fatalf("事件顺序不对（应 enter→exit→enter→exit）：%v", seq)
	}
}

// 同一路径传两次只取一把锁（取两次 = 自己等自己 = 死锁）。
func TestFileLockDedupSamePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	done := make(chan error, 1)
	go func() {
		unlock, err := lockPaths(dir, []string{target, target})
		if err == nil {
			unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("重复路径取锁失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("重复路径取锁死锁了")
	}
}

// 多路径按字典序取（防 AB-BA 死锁）：两个 goroutine 反序取两把锁也不能死锁。
func TestFileLockNoABBA(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	done := make(chan error, 2)
	go func() {
		unlock, err := lockPaths(dir, []string{a, b})
		if err == nil {
			unlock()
		}
		done <- err
	}()
	go func() {
		unlock, err := lockPaths(dir, []string{b, a})
		if err == nil {
			unlock()
		}
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("取锁失败: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("两把锁 AB-BA 死锁了")
		}
	}
}

// 持锁进程死了 → 内核自动释放锁（flock 挂在 fd 上，进程退出 fd 全关）。
func TestFileLockReleasedWhenHolderDies(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	ready := filepath.Join(dir, "ready")

	cmd := exec.Command(os.Args[0], "-test.run=TestFileLockHolderHelper")
	cmd.Env = append(os.Environ(),
		"TAKO_TEST_LOCK_DIR="+dir,
		"TAKO_TEST_LOCK_TARGET="+target,
		"TAKO_TEST_LOCK_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// 等子进程拿到锁
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("持锁子进程没起来")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 子进程还活着 → 拿不到锁（非阻塞试一次）
	lockFile := lockPathFor(dir, target)
	fd, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatalf("子进程持锁期间不该拿到锁")
	}
	_ = fd.Close()

	// 杀掉子进程（模拟崩溃：它不释放锁，交给内核回收）→ 锁应当自动消失
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("杀持锁进程失败: %v", err)
	}
	_, _ = cmd.Process.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for {
		fd, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN)
			_ = fd.Close()
			break
		}
		_ = fd.Close()
		if time.Now().After(deadline) {
			t.Fatalf("持锁进程死了但锁没释放")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFileLockHolderHelper 是上面那个测试的子进程侧：拿到锁 → 落 ready → 直接退出（不释放）。
func TestFileLockHolderHelper(t *testing.T) {
	dir := os.Getenv("TAKO_TEST_LOCK_DIR")
	target := os.Getenv("TAKO_TEST_LOCK_TARGET")
	ready := os.Getenv("TAKO_TEST_LOCK_READY")
	if dir == "" || target == "" || ready == "" {
		t.Skip("不是 helper 进程")
	}
	fd, err := os.OpenFile(lockPathFor(dir, target), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		os.Exit(2)
	}
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(ready, []byte("ok"), 0o644); err != nil {
		os.Exit(4)
	}
	// 模拟崩溃：等父进程验完「锁确实被占着」，再被强杀 → 锁由内核回收（我们不释放、不关 fd）
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// ── write_file 的三条写前拒绝 ────────────────────────────────────────────

func writeToolFor(t *testing.T, root string) tool.InvokableTool {
	t.Helper()
	raw, err := newWriteFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw.(tool.InvokableTool)
}

func readToolFor(t *testing.T, root string) tool.InvokableTool {
	t.Helper()
	raw, err := newReadFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw.(tool.InvokableTool)
}

// 没读过就写已存在的文件 → 拒（Kimi "Read before overwriting" 的程序级版）。
func TestWriteFileRequiresPriorRead(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := writeToolFor(t, root)
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	_, err := it.InvokableRun(ctx, `{"path":"a.txt","content":"new"}`)
	if err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("没读过就写应被拒（要指引用 read_file），实际 err=%v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "old" {
		t.Fatalf("磁盘被改了：%q", got)
	}
}

// 读过、但内容被别人改过 → 拒，且文案引导改用 edit_file。
func TestWriteFileRejectsChangedContent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	readToolFor(t, root).InvokableRun(ctx, `{"path":"a.txt"}`) // 建基准
	// 外部写者（bash / 另一个 agent）改了内容
	if err := os.WriteFile(p, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := writeToolFor(t, root)
	_, err := it.InvokableRun(ctx, `{"path":"a.txt","content":"new"}`)
	if err == nil || !strings.Contains(err.Error(), "跟你读到的不一样") {
		t.Fatalf("内容变了应被拒，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "edit_file") {
		t.Fatalf("拒的文案应引导改用 edit_file，实际 err=%v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "other" {
		t.Fatalf("磁盘被旧版覆盖了：%q", got)
	}
}

// 读过、但文件被删/被移走 → 拒，不重建。
func TestWriteFileRejectsDeletedFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	readToolFor(t, root).InvokableRun(ctx, `{"path":"a.txt"}`)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	it := writeToolFor(t, root)
	_, err := it.InvokableRun(ctx, `{"path":"a.txt","content":"new"}`)
	if err == nil || !strings.Contains(err.Error(), "刚被删") {
		t.Fatalf("文件没了应被拒，实际 err=%v", err)
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Fatalf("被拒后不该重建出文件")
	}
}

// ── edit_file：现读带过别人的改动 ────────────────────────────────────────

// TestEditCarriesForeignChange 是本方案最关键的一条性质：
// edit_file 在**编辑时刻现读**磁盘、写回时用的也是它 → 别的写者在你读之后落盘的改动会被原样带过去。
// 丢更新的窗口因此只是「程序读→程序写」那几微秒，而不是「模型读→模型写」那几秒到几分钟。
func TestEditCarriesForeignChange(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%02d", i)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	readToolFor(t, root).InvokableRun(ctx, `{"path":"f.txt"}`) // A 读 v0（建基准）

	// B（外部写者 / bash）在 A 读之后改了第 10 行
	foreign := append([]string(nil), lines...)
	foreign[10] = "line10-CHANGED-BY-B"
	if err := os.WriteFile(p, []byte(strings.Join(foreign, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	// A 用 edit_file 改第 50 行 → 应当**同时**保留 B 的第 10 行改动
	rawEdit, err := newEditFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := rawEdit.(tool.InvokableTool)
	out, err := it.InvokableRun(ctx, `{"path":"f.txt","old_text":"line50","new_text":"line50-BY-A"}`)
	if err != nil {
		t.Fatalf("edit_file 报错: %v", err)
	}
	if want := "已编辑 f.txt（替换 1 处）"; out != want {
		t.Fatalf("edit_file 返回 %q, want %q", out, want)
	}
	got, _ := os.ReadFile(p)
	gotLines := strings.Split(string(got), "\n")
	if gotLines[10] != "line10-CHANGED-BY-B" {
		t.Fatalf("B 的第 10 行改动被抹掉了：%q", gotLines[10])
	}
	if gotLines[50] != "line50-BY-A" {
		t.Fatalf("A 的第 50 行改动没落上：%q", gotLines[50])
	}

	// 反面：B 改的正是 A 的锚点那段 → A 的 edit_file 报「没有这段内容」。
	// （锚点是**子串**匹配，所以 B 得把那段整个换掉、不能让锚点还留在里面。）
	rawEdit2, _ := newEditFileTool(root)
	it2 := rawEdit2.(tool.InvokableTool)
	anchorGone := append([]string(nil), gotLines...)
	anchorGone[20] = "B-TOOK-THIS-LINE-OVER"
	if err := os.WriteFile(p, []byte(strings.Join(anchorGone, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := it2.InvokableRun(ctx, `{"path":"f.txt","old_text":"line20","new_text":"x"}`); err == nil ||
		!strings.Contains(err.Error(), "没有这段内容") {
		t.Fatalf("锚点段被改后应报「没有这段内容」，实际 err=%v", err)
	}

	// 写后基准刷新 → A 再改一次不误报
	if _, err := it2.InvokableRun(ctx, `{"path":"f.txt","old_text":"line51","new_text":"line51-BY-A"}`); err != nil {
		t.Fatalf("连续编辑不该误报：%v", err)
	}
}
