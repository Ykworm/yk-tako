// doc/58 §4 的快闸：bash 的结果字节（留尾 + 落盘提示行）、落盘文件是全文、
// 装不下全文就删文件、30 天清扫。
//
// 期望字节全部来自实跑（不是手写）：留尾的形状与提示行取自
// `POST /api/dev/workspace/bash` 的真实输出，落盘文件的大小取自真实落盘结果。
package runengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bashTestCtx 造一个带 noteBox 的 ctx：落盘目录名与序号都由它决定，测试才有确定字节。
func bashTestCtx() context.Context {
	return withNoteBox(context.Background(), newNoteBox("s_bash", "r_bash", 0))
}

// 小输出：字节与今天逐字不变（不落盘、不追加提示行）。
func TestBashSmallOutputUnchanged(t *testing.T) {
	root := t.TempDir()
	out, err := runBash(bashTestCtx(), root, "echo hello", 10*time.Second, true)
	if err != nil {
		t.Fatalf("跑命令失败: %v", err)
	}
	const want = "退出码：0\nstdout:\nhello\n"
	if out != want {
		t.Fatalf("小输出字节漂移: got %q want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".tako", "bash")); !os.IsNotExist(err) {
		t.Fatalf("小输出不该落盘（.tako/bash 不该存在）: %v", err)
	}
}

// 大输出：内存里留的是**尾**，头丢掉；全文落盘并回路径。
func TestBashKeepsTailAndSpillsWhole(t *testing.T) {
	root := t.TempDir()
	const size = 100000
	if err := os.WriteFile(filepath.Join(root, "big.bin"), []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runBash(bashTestCtx(), root, "cat big.bin", 10*time.Second, true)
	if err != nil {
		t.Fatalf("跑命令失败: %v", err)
	}
	// 留的是尾 64,000 字节：结果里全是 x，且总长 = 头部 + 64000 + 提示行。
	want := "退出码：0\nstdout:\n" + strings.Repeat("x", bashOutputCap) +
		fmt.Sprintf("\n（stdout 完整输出已存到 .tako/bash/r_bash/1.out，共 %d 字节；用 read_file 分页读它）", size)
	if out != want {
		t.Fatalf("留尾/落盘提示行不对:\n got %q\nwant %q", out, want)
	}
	// 落盘文件是**全文**（不是半截）：100000 字节，一个字节不少。
	disk, err := os.ReadFile(filepath.Join(root, ".tako", "bash", "r_bash", "1.out"))
	if err != nil {
		t.Fatalf("读落盘文件失败: %v", err)
	}
	if len(disk) != size || string(disk) != strings.Repeat("x", size) {
		t.Fatalf("落盘文件不是全文: %d 字节", len(disk))
	}
}

// 装不下全文：把落盘帽调到很小，超了就**删文件、不报路径**（dsh 的 discardSpill 纪律）。
func TestBashSpillDiscardedWhenWholeDoesNotFit(t *testing.T) {
	root := t.TempDir()
	c := &tailCollector{
		cap:      100,
		spillCap: 1024,
		spill:    &bashSpill{dirRel: ".tako/bash/r_x", dirAbs: filepath.Join(root, ".tako", "bash", "r_x"), seq: 1},
		ext:      "out",
	}
	c.Write([]byte(strings.Repeat("y", 2000)))
	c.closeSpill()
	if c.relPath != "" {
		t.Fatalf("装不下全文不该报路径: %q", c.relPath)
	}
	if _, err := os.Stat(filepath.Join(root, ".tako", "bash", "r_x", "1.out")); !os.IsNotExist(err) {
		t.Fatalf("装不下全文就该把文件删掉: %v", err)
	}
	if len(c.tail) != 100 {
		t.Fatalf("尾窗应留 100 字节: %d", len(c.tail))
	}
	if !strings.Contains(c.note("stdout"), "已只保留尾部") {
		t.Fatalf("提示行该说清只剩尾部: %q", c.note("stdout"))
	}
}

// 30 天清扫：过期的删、没到期的留（doc/58 §4.4）。
func TestSweepBashSpills(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, ".tako", "bash")
	old := filepath.Join(base, "r_old")
	fresh := filepath.Join(base, "r_fresh")
	for _, d := range []string{old, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	sweepBashSpills(root, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("31 天前的目录该被删: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("没到期的目录不该被删: %v", err)
	}
}
