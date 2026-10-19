package runengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

func sandboxProfile(root string) string {
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
