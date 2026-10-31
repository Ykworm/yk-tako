package runengine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepTempDirsByMtime 钉住清扫规则：只按 mtime、只删超过 24 小时的条目，
// `.staging` 与 `.trash` 同一套规则，新条目与未来 mtime（时钟偏斜）都留着。
func TestSweepTempDirsByMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	mk := func(dir, name string, age time.Duration) string {
		p := filepath.Join(root, dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		// 目录里放一个文件：RemoveAll 要真的把内容一起带走。
		if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		ts := now.Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
		return p
	}

	oldTrash := mk(".trash", "old-1", 25*time.Hour)
	freshTrash := mk(".trash", "fresh-1", time.Hour)
	oldStage := mk(".staging", "old-2", 48*time.Hour)
	freshStage := mk(".staging", "fresh-2", 23*time.Hour)
	future := mk(".trash", "skewed-1", -time.Hour)

	sweepTempDirs(root, now)

	for _, gone := range []string{oldTrash, oldStage} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s 应该被清掉，仍然存在（err=%v）", gone, err)
		}
	}
	for _, kept := range []string{freshTrash, freshStage, future} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s 应该保留：%v", kept, err)
		}
	}
}

// TestSweepTempDirsNoDirs 根下没有点目录时是空操作（从没装过的租户每次安装都会走到这里）。
func TestSweepTempDirsNoDirs(t *testing.T) {
	root := t.TempDir()
	sweepTempDirs(root, time.Now()) // 不 panic、不建目录
	if _, err := os.Stat(filepath.Join(root, ".trash")); !os.IsNotExist(err) {
		t.Errorf("清扫不该顺手建出点目录")
	}
}

// TestSweepTempDirsKeepsInFlightStaging 钉住唯一的豁免：本次要 rename 走的那份 staging
// 就算已经过期也不删。没它会怎样——对话内安装的提议挂起超过 24 小时、用户点头，
// 同一段临界区先把源目录当垃圾清掉，接着 rename ENOENT，用户读到一句裸 I/O 错误。
func TestSweepTempDirsKeepsInFlightStaging(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	mkAged := func(dir, name string) string {
		p := filepath.Join(root, dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		ts := now.Add(-48 * time.Hour)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
		return p
	}

	inFlight := mkAged(".staging", "in-flight")
	otherStage := mkAged(".staging", "abandoned")
	oldTrash := mkAged(".trash", "old-1")

	sweepTempDirsKeeping(root, now, "in-flight")

	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("豁免的 staging 应该留着：%v", err)
	}
	for _, gone := range []string{otherStage, oldTrash} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s 应该被清掉（err=%v）", gone, err)
		}
	}
}

// TestStagingKeep 豁免名只从「本根的 .staging 直下」取：别处的路径、空串一律不豁免，
// 免得配置写错时凭一个同名 Base 保住别的根下的垃圾。符号链接下的根也要认得出来
// （提议里的 staging 是 EvalSymlinks 过的绝对路径，root 是拼出来的字面路径）。
func TestStagingKeep(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".staging", "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := stagingKeep(root, filepath.Join(root, ".staging", "abc")); got != "abc" {
		t.Errorf("本根 staging 的豁免名 = %q，期望 abc", got)
	}
	if got := stagingKeep(root, ""); got != "" {
		t.Errorf("空路径不该有豁免名，得 %q", got)
	}
	if got := stagingKeep(root, filepath.Join(root, "abc")); got != "" {
		t.Errorf("`.staging` 之外的路径不该有豁免名，得 %q", got)
	}
	if got := stagingKeep(root, filepath.Join(t.TempDir(), ".staging", "abc")); got != "" {
		t.Errorf("别的根下的 staging 不该有豁免名，得 %q", got)
	}

	link := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("建符号链接失败，跳过： %v", err)
	}
	resolved := filepath.Join(mustEvalSymlinks(t, root), ".staging", "abc")
	if got := stagingKeep(filepath.Join(link, ""), resolved); got != "abc" {
		t.Errorf("符号链接下的根认不出来：豁免名 = %q，期望 abc", got)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks %s: %v", p, err)
	}
	return out
}
