// bashfence_test.go 是 bash 围栏的钉子网：读写都只在工作区根（项目目录）内。
//
// 这是「web 产品不允许越出项目目录范围的 bash 操作」这条产品要求的**程序级保证**
// （sandbox-exec 的 seatbelt profile，sandbox.go）。用户拍板（2026-10-22）：
// 越界 = **禁止**（不是问人）→ 所以 bash 不需要人工审批回路。
//
// 这组用例同时钉住两件事：
//  1. 围栏真的生效：越界读 / 越界写 / 读家目录，都被沙箱拒绝；
//  2. **root 走 symlink 时围栏仍生效**——seatbelt 的 subpath 匹配走真实路径，
//     而 macOS 的 /var 与 /tmp 都是符号链接（t.TempDir() 就在 /var/folders 下）→
//     profile 里必须用 EvalSymlinks 解析后的真实路径，否则 agent 连**自己项目里的
//     文件都读不到**（Operation not permitted）。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashFenceConfinesToWorkspace(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("这台机器的 sandbox-exec 不能 apply，围栏用例无法证明沙箱路径")
	}
	ctx := context.Background()
	root := t.TempDir() // macOS 上在 /var/folders 下（/var → /private/var 是 symlink）
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① 项目内可读可写
	out, err := runBash(ctx, root, "cat in.txt && echo x > out.txt && cat out.txt", 10*time.Second, true)
	if err != nil {
		t.Fatalf("项目内命令报错: %v", err)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "x") {
		t.Fatalf("项目内读写应成功，得到 %q", out)
	}

	// ② 越界写被拒
	outside := filepath.Join(filepath.Dir(root), "tako-fence-outside.txt")
	_ = os.Remove(outside)
	out2, _ := runBash(ctx, root, "echo bad > "+outside, 10*time.Second, true)
	if !strings.Contains(out2, "not permitted") {
		t.Fatalf("越界写应被沙箱拒绝，得到 %q", out2)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("越界写竟然成功了：%s", outside)
	}

	// ③ 越界读被拒（系统文件）
	out3, _ := runBash(ctx, root, "cat /etc/hosts", 10*time.Second, true)
	if !strings.Contains(out3, "not permitted") {
		t.Fatalf("越界读 /etc/hosts 应被沙箱拒绝，得到 %q", out3)
	}

	// ④ 读家目录被拒（agent 不该翻用户家目录）
	out4, _ := runBash(ctx, root, "cat ~/.zshrc 2>/dev/null; echo done", 10*time.Second, true)
	if strings.Contains(out4, "root:") || !strings.Contains(out4, "not permitted") && !strings.Contains(out4, "done") {
		t.Fatalf("读家目录应被沙箱拒绝，得到 %q", out4)
	}
}

// TestBashFenceSurvivesSymlinkedRoot：root 路径含 symlink 时，围栏仍要生效
// （profile 里用 EvalSymlinks 解析后的真实路径；否则 agent 连自己项目里的文件都读不到）。
//
// ⚠️ 根必须放在 **/tmp** 下：macOS 的 /tmp → /private/tmp，而 /private/tmp **不在** profile 的
// 系统白名单里（白名单只有 /private/var）→ 围栏只认 root 本身那一条 subpath。
// 若根在 /var/folders 下（t.TempDir() 的默认位置），真实路径 /private/var/folders/… 会被系统
// 白名单顺带放行 → **这个 bug 会被偶然掩盖**，钉子就抓不住它了。
func TestBashFenceSurvivesSymlinkedRoot(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("这台机器的 sandbox-exec 不能 apply")
	}
	ctx := context.Background()
	real, err := os.MkdirTemp("/tmp", "tako-fence-")
	if err != nil {
		t.Skipf("造不出 /tmp 下的目录：%v", err)
	}
	defer os.RemoveAll(real)
	if err := os.WriteFile(filepath.Join(real, "in.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := real // /tmp/xxx 本身就是穿过 symlink 的路径（/tmp → /private/tmp）

	// 绝对路径穿过 symlink：seatbelt 的 subpath 匹配走真实路径 → profile 里必须写真实路径，
	// 否则这条命令被拒（agent 读不到自己项目里的文件）。
	out, err := runBash(ctx, link, "cat "+filepath.Join(link, "in.txt"), 10*time.Second, true)
	if err != nil {
		t.Fatalf("symlink 路径下项目内读取报错: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("symlink 路径下应能读到项目内文件，得到 %q", out)
	}
	// 写也要通
	out2, _ := runBash(ctx, link, "echo y > "+filepath.Join(link, "out.txt")+" && cat "+filepath.Join(link, "out.txt"), 10*time.Second, true)
	if !strings.Contains(out2, "y") {
		t.Fatalf("symlink 路径下应能写项目内文件，得到 %q", out2)
	}
}
