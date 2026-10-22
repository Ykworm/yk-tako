package runengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bash 的时长帽。工作目录只决定默认路径，拦不住 cat /etc/passwd。
// 有 sandbox-exec 时用它把读写收进会话根；没有时不能假装已经隔离。
const (
	bashTimeout    = 30 * time.Second
	bashDevTimeout = 4 * time.Second
)

// sandboxAvailable 不只看 binary 在不在。LookPath 成功而 seatbelt 拒绝 apply 时，
// 命令会以普通非零退出结束，调用方还会以为已经隔离。这里真正跑一次 apply。
func sandboxAvailable() bool {
	sandboxProbe.once.Do(func() {
		path, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return
		}
		cmd := exec.Command(path, "-p", "(version 1)(allow default)", "/bin/echo", "ok")
		out, err := cmd.CombinedOutput()
		sandboxProbe.ok = err == nil && strings.Contains(string(out), "ok")
	})
	return sandboxProbe.ok
}

var sandboxProbe struct {
	once sync.Once
	ok   bool
}

// sandboxProfile 是 bash 的围栏：**写只在工作区根（项目目录）内**，读是「工作区根 + 系统路径白名单」。
//
// 2026-10-22 核过一遍围栏的真实行为（`sandbox-exec` 实测，不是推测）：
//   - 读 /etc/hosts → Operation not permitted ✓（`literal "/"` 只匹配根目录本身，不是全盘）
//   - 读 ~/.ssh、~/.zshrc → Operation not permitted ✓
//   - 写工作区外 → Operation not permitted ✓
//
// → **越界读与越界写本来就被硬挡**（不是问人）→ bash 不需要人工审批回路。
//
// ⚠️ root 必须用**解析后的真实路径**：seatbelt 的 subpath 匹配走真实路径，而 macOS 的
// `/tmp` 是指向 `/private/tmp` 的符号链接 → 若把 root 写成 `/tmp/xxx`，(subpath root)
// 匹配不上，agent 会连**自己项目里的文件都读不到**（Operation not permitted）。
func sandboxProfile(root string) string {
	// 真实路径（symlink 解析）：root 可能带 symlink（macOS 的 /tmp、挂载点软链等）。
	real := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		real = r
	}
	root = real
	return fmt.Sprintf(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read)
(allow mach-lookup)
(allow file-ioctl)
(allow file-read-metadata)
(allow file-read* (literal "/"))
(allow file-read* (subpath "/usr") (subpath "/bin") (subpath "/sbin") (subpath "/opt") (subpath "/System") (subpath "/Library") (subpath "/dev") (subpath "/private/var"))
(allow file-read* file-write* (subpath %q))
(deny file-read-data (literal "/etc/passwd") (literal "/private/etc/passwd"))
`, root)
}

// sandboxWrap 把一条命令包进 seatbelt 围栏（root = 围栏根；root 为空 = 不包）。
//
// 用途：MCP server 是宿主上的子进程（mcp.go 的 exec.Command），继承宿主权限 → 不围栏它，
// 它能读所有租户的目录。租户自己配的 MCP 一律包围栏（根 = 该租户的存储根）。
// 平台的系统 MCP（引擎自己的 mcp.json）不包——那是平台自己配的。
func sandboxWrap(cmd *exec.Cmd, root string) *exec.Cmd {
	if root == "" {
		return cmd
	}
	args := append([]string{"-p", sandboxProfile(root)}, cmd.Args...)
	return exec.Command("sandbox-exec", args...)
}

// commandInsideRoot 判定一条待执行的命令是否落在围栏根内（绝对路径判定）。
// 用途：围栏只允许写围栏根 → `npx` 这类要把依赖下到 ~/.npm 的启动方式在围栏里必然失败。
// 与其让它在启动时莫名失败，不如在启动前说清「依赖必须预装在租户目录内」。
func commandInsideRoot(command, root string) bool {
	if command == "" {
		return false
	}
	if !filepath.IsAbs(command) {
		return false // 相对路径按 cwd 解析，cwd 不保证在围栏根内 → 不认
	}
	real, err := filepath.EvalSymlinks(command)
	if err != nil {
		real = command
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// startBash 准备一条命令。requireSandbox 为真且本机没有能 apply 的 sandbox-exec 时直接失败，不退回裸 bash。
// 返回的 runCtx 是套了时长帽的那个 context。沙箱里的进程被杀掉时，cmd.Run 往往给出退出码 -1，
// 不再带 DeadlineExceeded；调用方要看 runCtx 才知道是超时。
func startBash(ctx context.Context, workspace, command string, timeout time.Duration, requireSandbox bool) (*exec.Cmd, func(), bool, context.Context, error) {
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		var c context.CancelFunc
		runCtx, c = context.WithTimeout(ctx, timeout)
		cancel = c
	}
	path, lookErr := exec.LookPath("sandbox-exec")
	if lookErr != nil || !sandboxAvailable() {
		if requireSandbox {
			cancel()
			return nil, func() {}, false, nil, fmt.Errorf("这台机器的 sandbox-exec 不能 apply，不能把工作目录当成隔离")
		}
		cmd := exec.CommandContext(runCtx, "bash", "-c", command)
		cmd.Dir = workspace
		return cmd, cancel, false, runCtx, nil
	}
	f, err := os.CreateTemp("", "tako-sb-*.sb")
	if err != nil {
		cancel()
		return nil, func() {}, false, nil, err
	}
	if _, err := f.WriteString(sandboxProfile(workspace)); err != nil {
		f.Close()
		os.Remove(f.Name())
		cancel()
		return nil, func() {}, false, nil, err
	}
	f.Close()
	name := f.Name()
	cmd := exec.CommandContext(runCtx, path, "-f", name, "/bin/bash", "-c", command)
	cmd.Dir = workspace
	return cmd, func() {
		cancel()
		os.Remove(name)
	}, true, runCtx, nil
}
