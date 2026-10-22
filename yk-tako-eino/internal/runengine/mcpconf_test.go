package runengine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testMcpRequest(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/mcp/servers", nil)
	s.getMcpServers(c)
	return w
}

type fakeSession struct{ onClose func() }

func (f *fakeSession) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	return nil
}

func TestLoadMcpConfigMissingFile(t *testing.T) {
	cfg, err := loadMcpConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("文件不存在应视同无配置，得 err: %v", err)
	}
	if len(cfg.McpServers) != 0 {
		t.Fatalf("应得空名单，得 %v", cfg.McpServers)
	}
}

func TestParseMcpConfigOk(t *testing.T) {
	cfg, err := parseMcpConfig([]byte(`{"mcpServers":{"fs":{"command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","./workspace"],"env":{}}}}`))
	if err != nil {
		t.Fatalf("合法配置应载入，得 err: %v", err)
	}
	sc, ok := cfg.McpServers["fs"]
	if !ok {
		t.Fatalf("缺 server fs")
	}
	if sc.Command != "npx" || len(sc.Args) != 3 {
		t.Fatalf("command/args 未按原样载入: %+v", sc)
	}
}

func TestParseMcpConfigEmptyServers(t *testing.T) {
	cfg, err := parseMcpConfig([]byte(`{"mcpServers":{}}`))
	if err != nil {
		t.Fatalf("空名单不是错误，得 err: %v", err)
	}
	if len(cfg.McpServers) != 0 {
		t.Fatalf("应得空名单")
	}
}

func TestParseMcpConfigRejectsUnknownKey(t *testing.T) {
	_, err := parseMcpConfig([]byte(`{"mcpServers":{"fs":{"command":"npx","transport":"stdio"}}}`))
	if err == nil {
		t.Fatal("未知键须拒载")
	}
	if !strings.Contains(err.Error(), "command/args/env") {
		t.Fatalf("报错应列出已知键，得: %v", err)
	}
}

func TestParseMcpConfigRejectsBadShortName(t *testing.T) {
	_, err := parseMcpConfig([]byte(`{"mcpServers":{"FS":{"command":"npx"}}}`))
	if err == nil {
		t.Fatal("坏短名须拒载")
	}
	if !strings.Contains(err.Error(), `短名 "FS" 不合法`) {
		t.Fatalf("报错应带短名与规则，得: %v", err)
	}
}

func TestParseMcpConfigRejectsMissingCommand(t *testing.T) {
	_, err := parseMcpConfig([]byte(`{"mcpServers":{"fs":{}}}`))
	if err == nil {
		t.Fatal("缺 command 须拒载")
	}
	if !strings.Contains(err.Error(), "缺必填字段 command") {
		t.Fatalf("报错应点名 command，得: %v", err)
	}
}

// getMcpServers 的字节面（connect 注入假替身，不真拉子进程）：字段序 name → command → state →
// tools → reason、servers 按短名字典序、空 tools 序列化成 [] 不是 null。解析负路径上面六件已盖。
func TestGetMcpServersShape(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mcp.json")
	body := `{"mcpServers":{"fs":{"command":"npx"},"bb":{"command":"node"}}}`
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{mcpConfPath: conf, mcpConnect: func(_ context.Context, name string, _ McpServerConfig, _ string) (*mcpServerConn, error) {
		names := []string{mcpToolName(name, "zebra"), mcpToolName(name, "apple")}
		sort.Strings(names)
		return &mcpServerConn{name: name, toolNames: names}, nil
	}}
	w := testMcpRequest(t, s)
	if w.Code != 200 {
		t.Fatalf("得 %d: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	wantOrder := `"servers":[` +
		`{"name":"bb","command":"node","state":"connected","tools":["mcp__bb__apple","mcp__bb__zebra"]},` +
		`{"name":"fs","command":"npx","state":"connected","tools":["mcp__fs__apple","mcp__fs__zebra"]}` +
		`],"count":2`
	if !strings.Contains(got, wantOrder) {
		t.Fatalf("字节面不符。\n得: %s\n要含: %s", got, wantOrder)
	}
}

// failed 形状：reason 透出、tools 仍为 []（不是 null）。
func TestGetMcpServersFailedShape(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(conf, []byte(`{"mcpServers":{"fs":{"command":"nope"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{mcpConfPath: conf, mcpConnect: func(_ context.Context, _ string, _ McpServerConfig, _ string) (*mcpServerConn, error) {
		return nil, errors.New("exec: not found")
	}}
	w := testMcpRequest(t, s)
	if w.Code != 200 {
		t.Fatalf("连接失败不该变 5xx，得 %d", w.Code)
	}
	got := w.Body.String()
	want := `{"name":"fs","command":"nope","state":"failed","tools":[],"reason":"exec: not found"}`
	if !strings.Contains(got, want) {
		t.Fatalf("字节面不符。\n得: %s\n要含: %s", got, want)
	}
}

// 配置没变复用连接（connect 只调一次）；变了就换（老会话关闭、按新配置重拉）。
func TestEnsureMcpConnReuseAndReplace(t *testing.T) {
	s := &Server{}
	calls := 0
	closed := 0
	s.mcpConnect = func(_ context.Context, _ string, sc McpServerConfig, _ string) (*mcpServerConn, error) {
		calls++
		return &mcpServerConn{name: "fs", conf: sc, session: &fakeSession{onClose: func() { closed++ }}, toolNames: []string{}}, nil
	}
	sc1 := McpServerConfig{Command: "npx"}
	if _, err := s.ensureMcpConn("fs", sc1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureMcpConn("fs", sc1, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("配置没变应复用，connect 调了 %d 次", calls)
	}
	sc2 := McpServerConfig{Command: "node"}
	if _, err := s.ensureMcpConn("fs", sc2, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || closed != 1 {
		t.Fatalf("配置变了应换连接：calls=%d closed=%d", calls, closed)
	}
}

// 坏配置 400 且错误原文透出——诊断面不吞原因。
func TestGetMcpServersBadConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(conf, []byte(`{"mcpServers":{"fs":{"command":"npx","oops":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{mcpConfPath: conf}
	w := testMcpRequest(t, s)
	if w.Code != 400 {
		t.Fatalf("得 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "command/args/env") {
		t.Fatalf("错误应列已知键，得: %s", w.Body.String())
	}
}

// 命名唯一性：mcp__ 前缀保证与一手工具不撞（filesystem server 的 read_file 与一手 read_file 同名，
// 靠前缀区分）、短名段保证 server 之间不撞。
func TestMcpToolNameNoCollision(t *testing.T) {
	if mcpToolName("fs", "read_file") == "read_file" {
		t.Fatal("mcp 名不得与一手工具名同形")
	}
	if mcpToolName("fs", "x") == mcpToolName("bar", "x") {
		t.Fatal("不同 server 的同名工具必须不同名")
	}
	if !strings.HasPrefix(mcpToolName("fs", "x"), "mcp__fs__") {
		t.Fatal("拼装点格式漂移")
	}
}

type fakeMcpTool struct{ name string }

func (f *fakeMcpTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name}, nil
}

// handler 正路：append 进 runCtx.Tools（含 wrapToolErrors 壳），ctx cancel 后会话被 watcher 关闭。
func TestMcpToolHandlerAppendsAndClosesOnCtxDone(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(conf, []byte(`{"mcpServers":{"fs":{"command":"npx"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	closed := 0
	h := &mcpToolHandler{confPath: conf, connect: func(_ context.Context, name string, sc McpServerConfig, _ string) (*mcpServerConn, error) {
		return &mcpServerConn{
			name: name, conf: sc, session: &fakeSession{onClose: func() { closed++ }},
			tools:     []tool.BaseTool{&fakeMcpTool{name: mcpToolName(name, "read_file")}},
			toolNames: []string{mcpToolName(name, "read_file")},
		}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	runCtx := &adk.ChatModelAgentContext{}
	_, out, err := h.BeforeAgent(ctx, runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("应 append 1 个工具，得 %d", len(out.Tools))
	}
	info, err := out.Tools[0].Info(context.Background())
	if err != nil || info.Name != "mcp__fs__read_file" {
		t.Fatalf("append 的工具名不符: %v %v", info, err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for closed == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closed != 1 {
		t.Fatalf("ctx cancel 后应关会话，closed=%d", closed)
	}
}

// 无配置 = runCtx.Tools 原样。
func TestMcpToolHandlerNoConfigNoop(t *testing.T) {
	h := &mcpToolHandler{confPath: filepath.Join(t.TempDir(), "absent.json")}
	runCtx := &adk.ChatModelAgentContext{}
	_, out, err := h.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 0 {
		t.Fatalf("无配置不得动池，得 %d", len(out.Tools))
	}
}

// server 起不来 = 降级不致命：工具不进池、BeforeAgent 不报错。
func TestMcpToolHandlerConnectFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(conf, []byte(`{"mcpServers":{"fs":{"command":"nope"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &mcpToolHandler{confPath: conf, connect: func(_ context.Context, _ string, _ McpServerConfig, _ string) (*mcpServerConn, error) {
		return nil, errors.New("exec: not found")
	}}
	runCtx := &adk.ChatModelAgentContext{}
	_, out, err := h.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("起不来不得变 run 错误: %v", err)
	}
	if len(out.Tools) != 0 {
		t.Fatalf("失败 server 的工具不得进池，得 %d", len(out.Tools))
	}
}

// untrusted notice：内容块头部插 notice、原块原样保留、空内容/nil 不炸。
func TestMcpNoticeHandler(t *testing.T) {
	h := mcpNoticeHandler("fs")
	// 正路：notice 打头、原块在后
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}
	out, err := h(context.Background(), "read_file", res)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("应 2 个内容块，得 %d", len(out.Content))
	}
	first, ok := out.Content[0].(*mcp.TextContent)
	if !ok || first.Text != mcpUntrustedNotice("fs") {
		t.Fatalf("首块应是 notice，得 %#v", out.Content[0])
	}
	second, ok := out.Content[1].(*mcp.TextContent)
	if !ok || second.Text != "hello" {
		t.Fatalf("原块须原样保留，得 %#v", out.Content[1])
	}
	// 空内容：只有 notice，不炸
	out2, err := h(context.Background(), "read_file", &mcp.CallToolResult{})
	if err != nil || len(out2.Content) != 1 {
		t.Fatalf("空内容应得单 notice 块: %v %d", err, len(out2.Content))
	}
	// nil 不炸
	if _, err := h(context.Background(), "read_file", nil); err != nil {
		t.Fatalf("nil 应透传: %v", err)
	}
}
