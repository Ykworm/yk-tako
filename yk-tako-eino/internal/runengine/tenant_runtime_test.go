package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTenantEnvStripsPlatformKeys(t *testing.T) {
	platform := []string{
		"DEEPSEEK_API_KEY=platform-secret",
		"TAKO_PROVIDER_FOO_API_KEY=platform-custom",
		"TAKO_PROVIDER_FOO_BASE_URL=https://example.test/v1",
		"TAKO_MODEL=deepseek/deepseek-v4-flash",
		"PATH=/usr/bin",
	}
	got := tenantEnvPairs(platform, map[string]string{
		"DEEPSEEK_API_KEY": "tenant-secret",
		"EMPTY_API_KEY":    "",
	})
	seen := map[string]string{}
	for _, pair := range got {
		k, v, _ := cutPair(pair)
		seen[k] = v
	}
	if seen["DEEPSEEK_API_KEY"] != "tenant-secret" {
		t.Fatalf("租户密钥应盖住平台密钥，得到 %q", seen["DEEPSEEK_API_KEY"])
	}
	if _, ok := seen["TAKO_PROVIDER_FOO_API_KEY"]; ok {
		t.Fatal("平台自定义厂商密钥不应留在租户环境里")
	}
	if seen["TAKO_PROVIDER_FOO_BASE_URL"] != "https://example.test/v1" || seen["TAKO_MODEL"] == "" || seen["PATH"] == "" {
		t.Fatalf("非密钥配置应留下，得到 %#v", seen)
	}
}

func cutPair(pair string) (string, string, bool) {
	for i := 0; i < len(pair); i++ {
		if pair[i] == '=' {
			return pair[:i], pair[i+1:], true
		}
	}
	return "", "", false
}

func TestReserveStorageOneWinner(t *testing.T) {
	st := newTestStore(t, time.Minute, time.Minute)
	_, tenant, _, err := st.Signup(context.Background(), "quota@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "have"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	used := func() int64 { return directoryBytes(dir) }
	var okN atomic.Int32
	var rels []func()
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel, err := st.ReserveStorage(context.Background(), tenant.ID, 80, 100, used)
			if errors.Is(err, errStorageQuota) {
				return
			}
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			okN.Add(1)
			mu.Lock()
			rels = append(rels, rel)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if okN.Load() != 1 {
		t.Fatalf("100 字节帽、目录已有 10、两笔各占 80，只应有一笔成功，得到 %d", okN.Load())
	}
	for _, rel := range rels {
		rel()
	}
	rel, err := st.ReserveStorage(context.Background(), tenant.ID, 80, 100, used)
	if err != nil {
		t.Fatalf("放开预留后应能再占，%v", err)
	}
	rel()
}

func TestRegisterByInviteExpiredLeavesNoUser(t *testing.T) {
	st := newTestStore(t, time.Minute, time.Minute)
	ctx := context.Background()
	owner, _, _, err := st.Signup(ctx, "owner@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	team, _, err := st.CreateTeamTenant(ctx, owner.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := st.CreateInvitation(ctx, team.ID, owner.ID, "new@example.com", "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.db.Model(&TenantInvitation{}).Where("id = ?", inv.ID).Update("expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterByInvite(ctx, "new@example.com", "hash", inv.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("过期邀请应失败，得到 %v", err)
	}
	var n int64
	if err := st.db.Model(&User{}).Where("email = ?", "new@example.com").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("过期邀请不应留下用户，得到 %d", n)
	}
}

type flagCloser struct{ closed atomic.Bool }

func (f *flagCloser) Close() error {
	f.closed.Store(true)
	return nil
}

func TestCustomMCPFailureClosesEarlierConn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	body := []byte(`{"mcpServers":{"aaa":{"command":"/bin/true","args":[],"env":{}}}}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := &flagCloser{}
	s := &Server{
		mcpConfPath: path,
		mcpConnect: func(_ context.Context, name string, _ McpServerConfig) (*mcpServerConn, error) {
			if name == "aaa" {
				return &mcpServerConn{name: name, session: sys}, nil
			}
			return nil, errors.New("down")
		},
	}
	cfg := runtimeConfig{McpServers: map[string]McpServerConfig{
		"zzz": {Command: "/bin/false"},
	}}
	conns, _, aerr := s.connectRunMCP(context.Background(), cfg, nil)
	if aerr == nil || aerr.Code != codeMcpDown {
		t.Fatalf("自定义连不上应 503，得到 %#v", aerr)
	}
	closeMCP(conns)
	if !sys.closed.Load() {
		t.Fatal("自定义失败时，前面已经连上的系统 MCP 应关掉")
	}
}

func TestSystemMCPFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	body := []byte(`{"mcpServers":{"sysdown":{"command":"/bin/false","args":[],"env":{}}}}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		mcpConfPath: path,
		mcpConnect: func(context.Context, string, McpServerConfig) (*mcpServerConn, error) {
			return nil, errors.New("down")
		},
	}
	conns, tools, aerr := s.connectRunMCP(context.Background(), runtimeConfig{}, nil)
	if aerr != nil {
		t.Fatalf("系统 MCP 失败应降级，得到 %v", aerr)
	}
	if len(conns) != 0 || len(tools) != 0 {
		t.Fatalf("降级后不应留下连接或工具，conns=%d tools=%d", len(conns), len(tools))
	}
}

func TestProfileCloseDropsConn(t *testing.T) {
	c := &flagCloser{}
	p := &RunProfile{conns: []*mcpServerConn{{session: c}}}
	p.Close()
	p.Close()
	if !c.closed.Load() {
		t.Fatal("关闭 profile 应关掉 MCP，并且可以再调一次")
	}
}
