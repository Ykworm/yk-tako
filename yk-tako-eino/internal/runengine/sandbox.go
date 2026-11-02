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
// 围栏后端：macOS 用 sandbox-exec（seatbelt），Linux 用 bubblewrap（bwrap）；
// 两者都探测不到时不能假装已经隔离。
const (
	bashTimeout    = 30 * time.Second
	bashDevTimeout = 4 * time.Second
)

// bashSandboxBackend 是这台机器上真正能 apply 的围栏后端。LookPath 成功不代表能用：
// seatbelt 可能被策略拒，Linux 可能禁了非特权 user namespace，所以每个候选都要真跑一次。
type bashSandboxBackend struct {
	kind   string // "seatbelt" / "bwrap" / "none"
	path   string
	reason string // kind == "none" 时给人看的原因（启动日志与拒绝文案共用）
}

func (b bashSandboxBackend) usable() bool { return b.kind != "none" }

func currentSandboxBackend() bashSandboxBackend {
	sandboxProbe.once.Do(func() {
		sandboxProbe.b = detectSandboxBackend()
	})
	return sandboxProbe.b
}

var sandboxProbe struct {
	once sync.Once
	b    bashSandboxBackend
}

// detectSandboxBackend 逐个候选真跑探针：macOS 的 seatbelt 在前，Linux 的 bwrap 在后。
// 第一个能 apply 的后端胜出；全失败时把所有原因拼进 reason，启动日志一次看全。
func detectSandboxBackend() bashSandboxBackend {
	var reasons []string
	if path, err := exec.LookPath("sandbox-exec"); err == nil {
		if ok, reason := probeSeatbelt(path); ok {
			return bashSandboxBackend{kind: "seatbelt", path: path}
		} else {
			reasons = append(reasons, "sandbox-exec："+reason)
		}
	}
	if path, err := exec.LookPath("bwrap"); err == nil {
		if ok, reason := probeBwrap(path); ok {
			return bashSandboxBackend{kind: "bwrap", path: path}
		} else {
			reasons = append(reasons, "bwrap："+reason)
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "本机没有 sandbox-exec 或 bwrap")
	}
	return bashSandboxBackend{
		kind:   "none",
		reason: strings.Join(reasons, "；") + "（Debian/Ubuntu 请安装 bubblewrap 并确认非特权 user namespace 可用）",
	}
}

// probeSeatbelt 是原来的探测：apply 一个全放行的最小 profile，看 echo 能不能跑出来。
func probeSeatbelt(path string) (bool, string) {
	cmd := exec.Command(path, "-p", "(version 1)(allow default)", "/bin/echo", "ok")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(err.Error() + " " + string(out))
	}
	if !strings.Contains(string(out), "ok") {
		return false, "探针输出里没有 ok：" + strings.TrimSpace(string(out))
	}
	return true, ""
}

// probeBwrap 真跑一次最小挂载表。只看 LookPath 分不清三种失败：
// binary 不在 / 非特权 userns 被禁 / seccomp 挡住 unshare——探针输出能区分。
func probeBwrap(path string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", "--new-session",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--", "/bin/echo", "tako-bash-probe",
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(err.Error() + " " + string(out))
	}
	if !strings.Contains(string(out), "tako-bash-probe") {
		return false, "探针输出里没有 tako-bash-probe：" + strings.TrimSpace(string(out))
	}
	return true, ""
}

// sandboxAvailable 是产品路径的闸：任意一个后端能 apply 就算可用。
func sandboxAvailable() bool { return currentSandboxBackend().usable() }

// SandboxStatus 供启动日志与诊断：seatbelt / bwrap / none（原因）。
// 配错在这行就暴露，不等客户第一次调工具。
func SandboxStatus() string {
	b := currentSandboxBackend()
	if !b.usable() {
		return "none（" + b.reason + "；bash 工具将拒绝执行）"
	}
	return b.kind
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

// sandboxWrap 把一条命令包进当前后端（root = 围栏根；root 为空 = 不包）。
//
// 用途：MCP server 是宿主上的子进程（mcp.go 的 exec.Command），继承宿主权限 → 不围栏它，
// 它能读所有租户的目录。租户自己配的 MCP 一律包围栏（根 = 该租户的存储根）。
// 平台的系统 MCP（引擎自己的 mcp.json）不包——那是平台自己配的。
func sandboxWrap(cmd *exec.Cmd, root string) (*exec.Cmd, error) {
	if root == "" {
		return cmd, nil
	}
	b := currentSandboxBackend()
	switch b.kind {
	case "bwrap":
		return exec.Command(b.path, bwrapArgs(root, nil, cmd.Args, pathExists)...), nil
	case "seatbelt":
		args := append([]string{"-p", sandboxProfile(root, nil)}, cmd.Args...)
		return exec.Command(b.path, args...), nil
	default:
		return nil, fmt.Errorf("这台机器没有可用的沙箱（%s），不能把租户 MCP server 当成已隔离", b.reason)
	}
}

// bwrapEtcFiles 是沙箱里放行的 /etc 单文件白名单（只读）。
// 纪律：只列工具真正要用的配置/证书，不 bind 整个 /etc——/etc/shadow 不在里面。
// /etc/hosts 故意不放：macOS 的 profile 也拒绝它（越界读同一个口径），DNS 走 resolv.conf。
var bwrapEtcFiles = []string{
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/alternatives",
	"/etc/nsswitch.conf", "/etc/localtime",
	"/etc/resolv.conf", "/etc/gai.conf", "/etc/host.conf", "/etc/services",
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/ca-certificates.conf",
}

// bwrapArgs 生成一个 bwrap 命令：新根里只有系统目录（只读）、白名单 /etc（只读）、
// 可写的工作区根，以及本场技能/工具根的只读挂载。
//
// 网络**不**卸 namespace：2026-10-28 D37 把 macOS 的 bash 出站网络打开了，Linux 必须对齐；
// --unshare-all 会连 net 一起卸掉，技能脚本里的 curl/pip 会断。代价是沙箱进程能在宿主网卡上
// bind 端口（macOS 侧是「出站开、入站禁」），这是与 seatbelt 的唯一已知差异，记在
// doc/problems/没有沙箱的系统怎么办/问题与方案.md。
//
// exists 注入是为了单测金样；生产传 pathExists。
func bwrapArgs(root string, extraReadRoots []string, command []string, exists func(string) bool) []string {
	root = sandboxRealPath(root)
	args := []string{
		// 逐个卸命名空间（不用 --unshare-all：它含 net）
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", // 引擎被 kill -9 时沙箱进程不孤儿化
		"--new-session",     // 断开控制终端，防 TIOCSTI
		"--proc", "/proc",   // 私有 /proc（ps 只看得到沙箱内进程）
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		// Debian 12 是 merged-usr：/bin 等都是指向 /usr 的符号链接
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
	}
	for _, p := range bwrapEtcFiles {
		if exists(p) {
			args = append(args, "--ro-bind", p, p)
		}
	}
	if exists("/opt") {
		args = append(args, "--ro-bind", "/opt", "/opt")
	}
	seen := map[string]bool{}
	for _, p := range extraReadRoots {
		if p == "" {
			continue
		}
		p = sandboxRealPath(p)
		// 已经在可写根里的目录不再 ro-bind：macOS 的 profile 只加读权限、不撤写权限，
		// 这里挂 ro 会让同一个目录在两套后端下语义不同。
		if seen[p] || pathWithin(root, p) || !exists(p) {
			continue
		}
		seen[p] = true
		args = append(args, "--ro-bind", p, p)
	}
	args = append(args, "--bind", root, root, "--chdir", root, "--")
	return append(args, command...)
}

// pathExists 是 bwrapArgs 的生产版存在性判断。挂载表只列确实存在的路径：
// bwrap 对不存在的 bind 源是硬失败（不是跳过），一个缺失的可选文件会让整条 bash 起不来。
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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

// startBash 准备一条命令。requireSandbox 为真且本机没有能 apply 的围栏后端时直接失败，不退回裸 bash。
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
	backend := currentSandboxBackend()
	if !backend.usable() {
		if requireSandbox {
			cancel()
			return nil, func() {}, false, nil, fmt.Errorf("bash 沙箱不可用（%s），不能把工作目录当成隔离", backend.reason)
		}
		cmd := exec.CommandContext(runCtx, "bash", "-c", command)
		cmd.Dir = workspace
		return cmd, cancel, false, runCtx, nil
	}
	extraReadRoots := append(skillReadRoots(ctx), toolsRootFor(ctx, workspace))
	if backend.kind == "bwrap" {
		cmd := exec.CommandContext(runCtx, backend.path, bwrapArgs(workspace, extraReadRoots, []string{"/bin/bash", "-c", command}, pathExists)...)
		cmd.Dir = workspace
		return cmd, cancel, true, runCtx, nil
	}
	f, err := os.CreateTemp("", "tako-sb-*.sb")
	if err != nil {
		cancel()
		return nil, func() {}, false, nil, err
	}
	if _, err := f.WriteString(sandboxProfile(workspace, extraReadRoots)); err != nil {
		f.Close()
		os.Remove(f.Name())
		cancel()
		return nil, func() {}, false, nil, err
	}
	f.Close()
	name := f.Name()
	cmd := exec.CommandContext(runCtx, backend.path, "-f", name, "/bin/bash", "-c", command)
	cmd.Dir = workspace
	return cmd, func() {
		cancel()
		os.Remove(name)
	}, true, runCtx, nil
}
