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

// sandboxProfile 是 bash 的围栏：**写只在工作区根（项目目录）内**，读是「工作区根 + 系统路径白名单
// + 本场 use_skill 过的技能根」。网络出站开、入站不开（doc/52 D37）。
//
// 2026-10-22 核过一遍围栏的真实行为（`sandbox-exec` 实测，不是推测）：
//   - 读 /etc/hosts → Operation not permitted ✓（`literal "/"` 只匹配根目录本身，不是全盘）
//   - 读 ~/.ssh、~/.zshrc → Operation not permitted ✓
//   - 写工作区外 → Operation not permitted ✓
//
// → **越界读与越界写本来就被硬挡**（不是问人）→ bash 不需要人工审批回路。
//
// extraReadRoots 是 run 盒子里登记过的技能根（skillbox.go）：只加 file-read*，**不加写**——
// 技能脚本能跑、能读自己的 references，但改不了自己那份正文；隔壁技能不在盒子里，仍然看不见。
//
// /private/etc/ssl 单独放行是 D37 的实测补丁（2026-10-28）：出站网络开了之后 curl 仍报
// 「Auto configuration failed」——LibreSSL 起手就要读 /private/etc/ssl/openssl.cnf 与 CA 包，
// 而 /private/etc 不在系统读白名单里。这一条只放证书与配置（公开材料），不放 /private/etc 全树，
// /etc/passwd 的显式拒绝照旧。
//
// ⚠️ root 与每条 extra 都必须用**解析后的真实路径**：seatbelt 的 subpath 匹配走真实路径，
// 而 macOS 的 `/tmp` 是指向 `/private/tmp` 的符号链接 → 若把 root 写成 `/tmp/xxx`，
// (subpath root) 匹配不上，agent 会连**自己项目里的文件都读不到**（Operation not permitted）。
func sandboxProfile(root string, extraReadRoots []string) string {
	root = sandboxRealPath(root)
	var system, extra strings.Builder
	for _, r := range sandboxReadRoots("", "") {
		fmt.Fprintf(&system, " (subpath %q)", r)
	}
	for _, r := range extraReadRoots {
		if r == "" {
			continue
		}
		fmt.Fprintf(&extra, "(allow file-read* (subpath %q))\n", sandboxRealPath(r))
	}
	return fmt.Sprintf(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read)
(allow mach-lookup)
(allow file-ioctl)
(allow file-read-metadata)
(allow network-outbound)
(allow file-read* (literal "/"))
(allow file-read*%s)
(allow file-read* (subpath "/private/etc/ssl"))
%s(allow file-read* file-write* (subpath %q))
(deny file-read-data (literal "/etc/passwd") (literal "/private/etc/passwd"))
`, system.String(), extra.String(), root)
}

// sandboxReadRoots 是围栏的只读根清单（doc/52 D39）：系统可执行目录 + 项目根 + 租户工具根。
// profile 与子进程 PATH 过滤**共用这一份**——抄两份就会漂，漂回去又是「PATH 里有、
// 围栏读不到」的 EPERM。工具根（`<tenant>/tools`）自 F2 起按租户常开（D36）：
// 所有角色（含普通子代理）的 bash 都读得到，写仍然只在项目根——run 改不了自己的解释器。
//
// 系统目录保持原样（`/bin` 在 macOS 上是指向 `/usr/bin` 的符号链接）：seatbelt 的
// subpath 匹配走真实路径，两者都放行不会放宽任何东西。需要真实路径的比对（PATH 过滤）
// 由调用方自己 EvalSymlinks。
func sandboxReadRoots(projectRoot, toolsRoot string) []string {
	roots := []string{"/usr", "/bin", "/sbin", "/opt", "/System", "/Library", "/dev", "/private/var"}
	if projectRoot != "" {
		roots = append(roots, projectRoot)
	}
	if toolsRoot != "" {
		roots = append(roots, toolsRoot)
	}
	return roots
}

// sandboxRealPath 解析符号链接并转绝对；解析不了（路径不存在等）就原样返回——
// 让围栏匹配不上好过让这条命令完全起不来（错误会以 Operation not permitted 显现）。
// 转绝对是因为 `-workspace` 可以写相对路径：seatbelt 的 subpath 只认绝对真实路径。
func sandboxRealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
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
	args := append([]string{"-p", sandboxProfile(root, nil)}, cmd.Args...)
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
	// root 可能是相对路径（`-workspace ../tmp/xxx` 时的真实现场）：EvalSymlinks 对相对输入
	// 返回相对结果，直接与绝对 command 做 Rel 会报错并判成「在外」→ 租户 MCP 起不来。
	// 两侧都先转绝对（按进程 cwd 解析，与 exec 的默认行为一致）。
	if abs, err := filepath.Abs(real); err == nil {
		real = abs
	}
	if abs, err := filepath.Abs(realRoot); err == nil {
		realRoot = abs
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
	if _, err := f.WriteString(sandboxProfile(workspace, append(skillReadRoots(ctx), toolsRootFor(ctx, workspace)))); err != nil {
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
