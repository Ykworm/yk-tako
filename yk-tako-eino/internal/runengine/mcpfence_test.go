// mcpfence_test.go 是 MCP 围栏的钉子网（方案真源 = doc/33 头注 + doc/41 §8 开放问题 6/7）。
//
// 2026-10-22 复核出的缺口：MCP server 是**宿主上的子进程**（mcp.go 的 exec.Command），
// 继承宿主权限 → 不围栏它，它能读所有租户的目录（tenants/<别人的tid>/… 都在同一个
// workspace 根下），而它的工具直接进模型工具池 → 跨租户泄漏面。
//
// 处置（用户拍板「选 b」）：**租户自己配的 MCP 进围栏（macOS seatbelt / Linux bwrap），围栏根 = 该租户的存储根**；
// 平台的系统 MCP（引擎自己的 mcp.json）不围栏；没有沙箱 → fail closed（不假装已隔离）。
package runengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandInsideRoot：围栏根的路径判定（绝对路径 + 解析 symlink + 不许越出）。
func TestCommandInsideRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "server.py")
	if err := os.WriteFile(inside, []byte("# server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "tako-outside-mcp")
	if err := os.WriteFile(outside, []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	if !commandInsideRoot(inside, root) {
		t.Errorf("根内的绝对路径应判为在内：%s", inside)
	}
	if commandInsideRoot(outside, root) {
		t.Errorf("根外的绝对路径应判为在外：%s", outside)
	}
	if commandInsideRoot("npx", root) {
		t.Errorf("相对路径应判为在外（cwd 不保证在围栏根内）")
	}
	if commandInsideRoot("/usr/bin/node", root) {
		t.Errorf("系统解释器不在租户目录内，应判为在外")
	}
	if commandInsideRoot("", root) {
		t.Errorf("空命令应判为在外")
	}
	// 相对根（`-workspace ../tmp/xxx` 起引擎时的真实现场）：要能判出根内。
	if relRoot, err := filepath.Rel(".", root); err == nil {
		if !commandInsideRoot(inside, relRoot) {
			t.Errorf("相对根 %q 下的根内命令应判为在内：%s", relRoot, inside)
		}
	}
}

// TestMcpFenceRootByCustom：租户配的进围栏（根=租户根），平台系统配置不进围栏。
func TestMcpFenceRootByCustom(t *testing.T) {
	tenantRoot := "/tmp/tenants/t_1"
	if got := mcpFenceRoot(mergedServer{name: "x", custom: true}, tenantRoot); got != tenantRoot {
		t.Errorf("租户配的应围栏且根=租户根，得到 %q", got)
	}
	if got := mcpFenceRoot(mergedServer{name: "x", custom: false}, tenantRoot); got != "" {
		t.Errorf("平台系统配置不应围栏，得到 %q", got)
	}
}

// TestMcpConnCacheKeyIncludesFenceRoot：同一份配置在两个租户下 → **不能复用同一个连接**
// （复用 = 围栏串了：租户 B 会拿到租户 A 的那个无边界进程）。
func TestMcpConnCacheKeyIncludesFenceRoot(t *testing.T) {
	s := &Server{mcpConfPath: filepath.Join(t.TempDir(), "none.json")}
	calls := 0
	s.mcpConnect = func(_ context.Context, name string, sc McpServerConfig, root string) (*mcpServerConn, error) {
		calls++
		return &mcpServerConn{name: name, conf: sc, root: root, session: &flagCloser{}}, nil
	}
	sc := McpServerConfig{Command: "/bin/true"}
	if _, err := s.ensureMcpConn("fs", sc, "/tmp/tenants/t_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureMcpConn("fs", sc, "/tmp/tenants/t_1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("同租户同配置应复用连接，connect 调了 %d 次", calls)
	}
	if _, err := s.ensureMcpConn("fs", sc, "/tmp/tenants/t_2"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("换租户（围栏根不同）必须重连，connect 调了 %d 次", calls)
	}
}

// TestTenantMcpCommandOutsideRootRejected：租户 MCP 的 command 不在租户目录内 → 启动前就拒，
// 并说清「依赖要预装到租户目录里」（围栏不允许在租户目录之外启动程序）。
func TestTenantMcpCommandOutsideRootRejected(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("这台机器没有可用的沙箱（seatbelt/bwrap 都探测失败）")
	}
	root := t.TempDir()
	_, err := connectMcpServer(context.Background(), "x", McpServerConfig{Command: "/bin/echo", Args: []string{"hi"}}, root)
	if err == nil {
		t.Fatal("租户 MCP 的 command 在租户目录外应被拒")
	}
	if !strings.Contains(err.Error(), "租户的项目目录内") {
		t.Fatalf("错误文案要说清「依赖要预装在租户目录内」，得到 %q", err.Error())
	}
}
