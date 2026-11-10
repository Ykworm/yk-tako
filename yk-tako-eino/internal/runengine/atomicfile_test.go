// atomicfile_test.go 是「同目录临时文件 + 原子 rename」的钉子网（方案真源 = doc/71 §4.3）。
//
// 钉的四条性质：
//  1. 写成功后磁盘是**新内容**，旧内容不残留；
//  2. 权限：已有文件保留原权限，新文件 0644；
//  3. 失败路径：目标字节不变、目标目录不留临时文件（不留「看起来像目标」的垃圾）；
//  4. 写期间持续读：读者只看到完整旧版或完整新版——没有空文件、半截、混合版本。
//
// 另外钉住「临时文件建在目标的同目录」——跨文件系统的 rename 会退化成复制+删除，
// 原子性就没了（doc/71 §4.3）。
package runengine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// tempFilesIn 列出目录里的临时文件（.tako-tmp- 前缀）。
func tempFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tako-tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 写成功后磁盘是新内容；旧内容不残留。
func TestAtomicWriteFileReplacesContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(target, []byte("new content"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Fatalf("磁盘内容 %q, want %q", got, "new content")
	}
	if temps := tempFilesIn(t, dir); len(temps) != 0 {
		t.Fatalf("写完不该留临时文件：%v", temps)
	}
}

// 权限：已有文件保留原权限（覆盖写不该顺手改权限），新文件 0644。
func TestAtomicWriteFilePermissions(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(existing, []byte("y"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	fi, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("已有文件权限 %o, want 600（保留原权限）", got)
	}

	fresh := filepath.Join(dir, "new.txt")
	if err := atomicWriteFile(fresh, []byte("z"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	fi2, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi2.Mode().Perm(); got != 0o644 {
		t.Fatalf("新文件权限 %o, want 644", got)
	}
}

// 临时文件必须建在**目标的同目录**（同一文件系统 → rename 才是原子替换）。
// 用观察点断言：建临时文件时它落在哪个目录。
func TestAtomicTempFileLivesInTargetDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "b.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	var seenDir, seenName string
	prev := atomicTempHook
	atomicTempHook = func(d, n string) { seenDir, seenName = d, n }
	defer func() { atomicTempHook = prev }()

	if err := atomicWriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	if seenDir != filepath.Dir(target) {
		t.Fatalf("临时文件建在 %s, want %s（必须同目录 → 同文件系统）", seenDir, filepath.Dir(target))
	}
	if seenName == target || filepath.Base(seenName) == filepath.Base(target) {
		t.Fatalf("临时文件 %s 与目标同名（会冒充目标）", seenName)
	}
	if !strings.HasPrefix(filepath.Base(seenName), ".tako-tmp-") {
		t.Fatalf("临时文件名 %s 应带 .tako-tmp- 前缀", filepath.Base(seenName))
	}
}

// 失败路径：目标字节不变 + 目标目录不留临时文件。
// 造失败的方式：把目标做成一个**目录** → rename 覆盖目录必然失败（ENOTEMPTY/EISDIR）。
func TestAtomicWriteFailureKeepsTargetAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// 目录里先放一个标记文件，确认失败后它还在（目标没被动过）。
	marker := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := atomicWriteFile(target, []byte("boom"), 0o644)
	if err == nil {
		t.Fatalf("往目录上 rename 应失败，实际成功了")
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("目标目录不该消失：%v", statErr)
	}
	if got, _ := os.ReadFile(marker); string(got) != "keep" {
		t.Fatalf("目标内容被改了：%q", got)
	}
	if temps := tempFilesIn(t, dir); len(temps) != 0 {
		t.Fatalf("失败路径不该留临时文件：%v", temps)
	}
}

// 写期间持续读：只看到完整旧版或完整新版——没有空文件、半截、混合版本。
func TestAtomicWriteNeverExposesPartialContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "big.txt")
	oldContent := strings.Repeat("A", 256*1024)
	newContent := strings.Repeat("B", 256*1024)
	if err := os.WriteFile(target, []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var partial, readErrs, reads atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(target)
			if err != nil {
				readErrs.Add(1)
				continue
			}
			reads.Add(1)
			s := string(raw)
			if s != oldContent && s != newContent {
				partial.Add(1)
			}
		}
	}()
	for i := 0; i < 60; i++ {
		content := oldContent
		if i%2 == 0 {
			content = newContent
		}
		if err := atomicWriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatalf("第 %d 次写失败：%v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if partial.Load() != 0 {
		t.Fatalf("读到 %d 次半截/混合内容（共读 %d 次）", partial.Load(), reads.Load())
	}
	if readErrs.Load() != 0 {
		t.Fatalf("原子替换期间读失败 %d 次（目标不该消失）", readErrs.Load())
	}
	if reads.Load() == 0 {
		t.Fatalf("读者一次都没读到东西，测试没意义")
	}
}

// 崩溃（kill -9）残留的临时文件**不冒充目标**：目标仍是崩溃前的完整内容。
func TestAtomicStaleTempDoesNotImpersonateTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	oldContent := "the only real content"
	if err := os.WriteFile(target, []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃残留：目标同目录里躺着一个没被 rename 的临时文件。
	stale := filepath.Join(dir, ".tako-tmp-crashed123")
	if err := os.WriteFile(stale, []byte("half written garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != oldContent {
		t.Fatalf("目标内容 %q, want %q", got, oldContent)
	}
	if filepath.Base(stale) == filepath.Base(target) {
		t.Fatalf("临时文件与目标同名 → 会冒充目标")
	}
	// 后续一次正常写也不能被残留临时文件干扰。
	if err := atomicWriteFile(target, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got2, _ := os.ReadFile(target)
	if string(got2) != "fresh" {
		t.Fatalf("写后内容 %q, want %q", got2, "fresh")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("残留临时文件不该被这次写动到（它不是目标）：%v", err)
	}
}

// 并发写者不会撞临时文件名（撞名 = 一个写者把别人的临时文件 rename 成目标）。
func TestAtomicTempNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	var mu sync.Mutex
	prev := atomicTempHook
	atomicTempHook = func(d, n string) {
		mu.Lock()
		seen[n] = true
		mu.Unlock()
	}
	defer func() { atomicTempHook = prev }()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
			if err := atomicWriteFile(target, []byte("x"), 0o644); err != nil {
				t.Errorf("写失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 20 {
		t.Fatalf("临时文件名 %d 个, want 20（必须唯一）", n)
	}
}
