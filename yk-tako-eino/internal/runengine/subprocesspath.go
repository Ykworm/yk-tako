// subprocesspath.go 是子进程 PATH 的唯一来源（doc/52 D39）。
//
// 病灶：引擎原样把宿主 PATH 透传给 bash，于是 `~/.pyenv/shims/python3` 能被找到、
// 执行时被围栏拒成 `Operation not permitted`——模型会当成路径写错，换着路径重试。
// 这里把围栏读不到的段先丢掉：同一条命令直接报 `command not found`，模型的正确反应
// 是「这台机器没装，请管理员上传」（D36）。
//
// 判断只看路径前缀、**不试跑**（D22 同一条线）：试跑等于执行一个别人声明的可执行文件。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// subprocessPath 给出子进程该用的 PATH：**租户工具根的前缀在最前面**（D36 F2），
// 再跟按 sandboxReadRoots 过滤后的宿主 PATH。
//
// projectRoot 是这场 run 的围栏根（项目目录）；HTTP 侧（依赖检查、安装）传的是
// 租户根。它只影响「PATH 里落在它下面的段是否保留」，不影响系统目录。
// toolsRoot 是 `<tenant>/tools`：它既是 PATH 的前缀（租户工具抢过宿主同名），
// 也必须在 sandboxReadRoots 里（拼进 PATH 却执行被围栏拒，就是 D39 的老病灶）。
func subprocessPath(projectRoot, toolsRoot string) string {
	sep := string(os.PathListSeparator)
	parts := toolBinDirs(toolsRoot)
	if host := filterSubprocessPath(os.Getenv("PATH"), sandboxReadRoots(projectRoot, toolsRoot)); host != "" {
		parts = append(parts, host)
	}
	return strings.Join(dedupePathParts(parts), sep)
}

// toolBinDirs 列出 `<tenant>/tools/*/bin`（跳过 `.` 开头的目录）。
//
// 跳过点目录不是洁癖：`.staging/<rand>/bin` 里是在途上传的半成品，`.trash` 里是
// 已删除的旧版本——任何一个被拼进 PATH，都是「半成品/已删除的东西抢先被 exec」。
func toolBinDirs(toolsRoot string) []string {
	if toolsRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(toolsRoot)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		bin := filepath.Join(toolsRoot, e.Name(), "bin")
		if info, err := os.Stat(bin); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, sandboxRealPath(bin))
	}
	return out
}

// dedupePathParts 把 `a:b/c` 这种已拼好的 PATH 段表按**真实路径**去重后保序返回。
// 只在跨「工具根 + 宿主段」时用得着：`filterSubprocessPath` 内部已经去过重一遍。
func dedupePathParts(parts []string) []string {
	sep := string(os.PathListSeparator)
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, chunk := range parts {
		for _, seg := range strings.Split(chunk, sep) {
			if seg == "" || seen[seg] {
				continue
			}
			seen[seg] = true
			out = append(out, seg)
		}
	}
	return out
}

// filterSubprocessPath 是 subprocessPath 的纯函数核（单测直接喂串）。
//
// 逐段 EvalSymlinks（`/bin` → `/usr/bin`），只保留落在某个根之下的段：
//   - 解析不了的段（目录不存在、悬空链接）丢掉——它指向的可执行文件本来也跑不了；
//   - 保序去重：`/bin` 与 `/usr/bin` 解析成同一段，只留先出现的那一个；
//   - 返回的每一段都是真实路径，与围栏的 subpath 匹配用同一套坐标。
func filterSubprocessPath(raw string, roots []string) string {
	realRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		realRoots = append(realRoots, sandboxRealPath(r))
	}
	sep := string(os.PathListSeparator)
	seen := make(map[string]bool, 8)
	out := make([]string, 0, 8)
	for _, seg := range strings.Split(raw, sep) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if _, err := filepath.EvalSymlinks(seg); err != nil {
			continue
		}
		real := sandboxRealPath(seg)
		allowed := false
		for _, root := range realRoots {
			if pathWithin(root, real) {
				allowed = true
				break
			}
		}
		if !allowed || seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	return strings.Join(out, sep)
}

// lookPathIn 在一条 PATH 串里找一个可执行文件（doc/52 §7 第 2 条 / D39）：
// 与 bash 用同一把尺，不做「宿主找得到、跑起来没有」的错位判断。
//
// 只认可执行名，不认路径：`requires` 的元素是名字；带 `/` 的输入不是依赖声明，
// 直接当找不到（否则第一次检查就变成「用绝对路径探文件存不存在」）。
// 找到回 EvalSymlinks 后的真实路径，找不到回空。
func lookPathIn(pathEnv, name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) || strings.Contains(name, "/") {
		return "", false
	}
	for _, dir := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		info, err := os.Stat(cand) // 跟随符号链接：一条指向可执行脚本的链接也是可执行的
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if real, err := filepath.EvalSymlinks(cand); err == nil {
			return real, true
		}
		return cand, true
	}
	return "", false
}

// tenantRootFor 取本场 run 的租户根（`<workspace>/tenants/<tid>`）。
//
// 优先用 ctx 里的 WorkspaceRef.TenantRoot（worker 装配时写好的同一个值）；
// 没有引用时（dev bash、单测）从目录形状推：项目/会话根都是
// `<tenant>/workspaces/<wid>` 或 `<tenant>/sessions/<sid>`，上两级就是租户根。
// 形状不符就回空串——宁可没有工具根，也不猜一个目录当租户根。
func tenantRootFor(ctx context.Context, workspaceDir string) string {
	if ref, ok := workspaceFrom(ctx); ok && ref.TenantRoot != "" {
		return ref.TenantRoot
	}
	return tenantRootFromDir(workspaceDir)
}

// tenantRootFromDir 是形状推导的纯函数核。
func tenantRootFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	parent := filepath.Dir(filepath.Clean(dir))
	switch filepath.Base(parent) {
	case "workspaces", "sessions":
		return filepath.Dir(parent)
	}
	return ""
}

// toolsRootFor 给出本场 run 的工具根；取不到租户根就回空串（不前置、不加白名单）。
func toolsRootFor(ctx context.Context, workspaceDir string) string {
	tenantRoot := tenantRootFor(ctx, workspaceDir)
	if tenantRoot == "" {
		return ""
	}
	return filepath.Join(tenantRoot, "tools")
}

// toolsDirFor 是「租户根 → 工具根」的直达形状（HTTP 侧只有租户根，没有 ctx）。
func toolsDirFor(tenantRoot string) string {
	if tenantRoot == "" {
		return ""
	}
	return filepath.Join(tenantRoot, "tools")
}
