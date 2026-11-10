package runengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// D1.2 观测口改造（doc/65 §6.5）的确定性回归：
// items 默认只回 active 未过期、历史要显式 state；markdown 模式 items/jobs/recall 回 409/118001；
// dev 种子口的新字段校验；dev memory-file 只读口的原文/使用量/格式失败如实上报。
// 契约字节（逐字）由 D1.5 的真实 curl 钉进 contract_test.go 与 expected.json，这里断语义。

func observeCall(t *testing.T, s *Server, method, url, body string, p principal) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
		c.Request = httptest.NewRequest(method, url, rdr)
		c.Request.Header.Set("Content-Type", "application/json")
	} else {
		c.Request = httptest.NewRequest(method, url, nil)
	}
	c.Set("tako.principal", p)
	switch {
	case method == http.MethodPost && strings.HasPrefix(url, "/api/dev/memory"):
		s.postDevMemory(c)
	case strings.HasPrefix(url, "/api/dev/memory-file"):
		s.getDevMemoryFile(c)
	case strings.HasPrefix(url, "/api/memory/items"):
		s.getMemoryItems(c)
	case strings.HasPrefix(url, "/api/memory/jobs"):
		s.getMemoryJobs(c)
	case strings.HasPrefix(url, "/api/memory/recall"):
		s.getMemoryRecall(c)
	case strings.HasPrefix(url, "/api/memory/dialogue"):
		s.getMemoryDialogue(c)
	default:
		t.Fatalf("未登记的测试路由: %s", url)
	}
	return w
}

func observeEnvelope(t *testing.T, w *httptest.ResponseRecorder) (int, string, json.RawMessage) {
	t.Helper()
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是信封 JSON: %s", w.Body.String())
	}
	return env.Code, env.Message, env.Data
}

// items 默认只回 active 且未过期；历史必须显式 state；显式 active 与默认同口径。
func TestMemoryItemsStateFilter(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	seedResident(t, s, "m_act", "t1", "u1", "", MemoryScopeUser, "当前态", "active", base)
	seedResident(t, s, "m_sup", "t1", "u1", "", MemoryScopeUser, "被替代", "superseded", base)
	seedResident(t, s, "m_rm", "t1", "u1", "", MemoryScopeUser, "被删除", "removed", base)
	seedResident(t, s, "m_ar", "t1", "u1", "", MemoryScopeUser, "已归档", "archived", base)
	seedResident(t, s, "m_exp", "t1", "u1", "", MemoryScopeUser, "已过期", "active", base)
	if err := s.db.Model(&MemoryItem{}).Where("id = ?", "m_exp").Update("expires_at", base).Error; err != nil {
		t.Fatal(err)
	}

	ids := func(layer, state string) []string {
		t.Helper()
		items, err := s.ListMemoryItems(ctx, layer, state, 50)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids("", ""); len(got) != 1 || got[0] != "m_act" {
		t.Fatalf("默认 = %v, want [m_act]（历史与过期不进默认列表）", got)
	}
	if got := ids("", "active"); len(got) != 1 || got[0] != "m_act" {
		t.Fatalf("显式 active = %v, want [m_act]", got)
	}
	if got := ids("", "superseded"); len(got) != 1 || got[0] != "m_sup" {
		t.Fatalf("superseded = %v, want [m_sup]", got)
	}
	if got := ids("resident", "removed"); len(got) != 1 || got[0] != "m_rm" {
		t.Fatalf("removed = %v, want [m_rm]", got)
	}
	if got := ids("recall", ""); len(got) != 0 {
		t.Fatalf("recall 层默认 = %v, want 空（种子全在 resident）", got)
	}
}

// markdown 模式：items/jobs/recall → 409/118001/固定文案/data:null；dialogue 两模式都保留。
func TestMemoryObserveMarkdownMode(t *testing.T) {
	s := newMemoryStore(t)
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: t.TempDir(), Mode: MemoryModeMarkdown})
	srv := &Server{store: s}
	p := principal{TenantID: "t1", UserID: "u1", Role: "owner"}

	for _, url := range []string{"/api/memory/items", "/api/memory/jobs", "/api/memory/recall?q=茶"} {
		w := observeCall(t, srv, http.MethodGet, url, "", p)
		if w.Code != http.StatusConflict {
			t.Fatalf("%s 状态 = %d, want 409", url, w.Code)
		}
		code, msg, data := observeEnvelope(t, w)
		if code != 118001 || msg != "当前记忆模式不提供这个观测口" || string(data) != "null" {
			t.Fatalf("%s 信封 = %d/%q/%s, want 118001/固定文案/null", url, code, msg, string(data))
		}
	}
	w := observeCall(t, srv, http.MethodGet, "/api/memory/dialogue", "", p)
	if w.Code != http.StatusOK {
		t.Fatalf("dialogue 在 markdown 模式应保留: %d %s", w.Code, w.Body.String())
	}
	// pipeline 模式不挡。
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: t.TempDir(), Mode: MemoryModePipeline})
	if w := observeCall(t, srv, http.MethodGet, "/api/memory/items", "", p); w.Code != http.StatusOK {
		t.Fatalf("pipeline items = %d, want 200", w.Code)
	}
	// 非法 state → 400。
	if w := observeCall(t, srv, http.MethodGet, "/api/memory/items?state=bogus", "", p); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 state = %d, want 400", w.Code)
	}
}

// dev 种子口：新字段落库；词表外值拒绝；scope=workspace 缺 workspaceId 拒绝。
func TestDevMemorySeedNewFields(t *testing.T) {
	s := newMemoryStore(t)
	srv := &Server{store: s}
	p := principal{TenantID: "t1", UserID: "u1", Role: "owner"}

	w := observeCall(t, srv, http.MethodPost, "/api/dev/memory",
		`{"layer":"resident","scope":"user","state":"superseded","content":"旧说法","quote":"原话"}`, p)
	if w.Code != http.StatusCreated {
		t.Fatalf("种子 = %d %s, want 201", w.Code, w.Body.String())
	}
	var row MemoryItem
	if err := s.db.Where("content = ?", "旧说法").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Layer != "resident" || row.Scope != "user" || row.State != "superseded" || row.Quote != "原话" ||
		row.TenantID != "t1" || row.UserID != "u1" || row.LeftActiveAt == nil {
		t.Fatalf("落库行 = %+v", row)
	}
	for _, body := range []string{
		`{"content":""}`,
		`{"content":"x","layer":"bogus"}`,
		`{"content":"x","scope":"bogus"}`,
		`{"content":"x","state":"bogus"}`,
		`{"content":"x","scope":"workspace"}`,
	} {
		if w := observeCall(t, srv, http.MethodPost, "/api/dev/memory", body, p); w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", body, w.Code)
		}
	}
	// 缺省字段：layer=recall scope=user state=active。
	if w := observeCall(t, srv, http.MethodPost, "/api/dev/memory", `{"content":"默认值"}`, p); w.Code != http.StatusCreated {
		t.Fatalf("缺省种子 = %d", w.Code)
	}
	var row2 MemoryItem
	if err := s.db.Where("content = ?", "默认值").First(&row2).Error; err != nil {
		t.Fatal(err)
	}
	if row2.Layer != "recall" || row2.Scope != "user" || row2.State != "active" {
		t.Fatalf("缺省落库 = %+v, want recall/user/active", row2)
	}
}

// dev memory-file 只读口：原文与使用量；文件不存在 = 空记忆；格式坏如实报 parseError。
func TestDevMemoryFileReadonly(t *testing.T) {
	root := t.TempDir()
	s := newMemoryStore(t)
	srv := &Server{store: s, mediaRoot: root}
	p := principal{TenantID: "t1", UserID: "u1", Role: "owner"}

	type fileData struct {
		Target     string `json:"target"`
		FileName   string `json:"fileName"`
		Content    string `json:"content"`
		UsedRunes  int    `json:"usedRunes"`
		Cap        int    `json:"cap"`
		EntryCount int    `json:"entryCount"`
		UsageKnown bool   `json:"usageKnown"`
		ParseError string `json:"parseError"`
	}
	call := func(url string) (int, fileData) {
		t.Helper()
		w := observeCall(t, srv, http.MethodGet, url, "", p)
		code, _, data := observeEnvelope(t, w)
		var d fileData
		if len(data) > 0 && string(data) != "null" {
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatal(err)
			}
		}
		return code, d
	}

	// 文件不存在 = 空记忆（正常起点），不是错误。
	code, d := call("/api/dev/memory-file?target=user")
	if code != 0 || d.Content != "" || !d.UsageKnown || d.UsedRunes != 0 || d.Cap != memoryUserCap || d.FileName != "USER.md" {
		t.Fatalf("空起点 = %d %+v", code, d)
	}
	dir := filepath.Join(root, "tenants/t1/users/u1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "USER.md")
	if err := os.WriteFile(path, []byte("喜欢茶\n§\n在杭州"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, d = call("/api/dev/memory-file?target=user")
	if code != 0 || d.Content != "喜欢茶\n§\n在杭州" || !d.UsageKnown || d.UsedRunes != 6 || d.EntryCount != 2 {
		t.Fatalf("正常文件 = %d %+v", code, d)
	}
	// 只读：调用后字节不变。
	raw, _ := os.ReadFile(path)
	if string(raw) != "喜欢茶\n§\n在杭州" {
		t.Fatalf("只读口动了文件: %q", raw)
	}
	// 格式坏：原文照回、usageKnown=false、parseError 非空。
	if err := os.WriteFile(path, []byte(" 首尾带空格"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, d = call("/api/dev/memory-file?target=user")
	if code != 0 || d.Content != " 首尾带空格" || d.UsageKnown || d.ParseError == "" {
		t.Fatalf("格式坏 = %d %+v, want usageKnown=false + parseError", code, d)
	}
	// target 校验与项目可见性。
	if code, _ := call("/api/dev/memory-file?target=bogus"); code == 0 {
		t.Fatal("bogus target 应 4xx")
	}
	if w := observeCall(t, srv, http.MethodGet, "/api/dev/memory-file?target=workspace", "", p); w.Code != http.StatusBadRequest {
		t.Fatalf("workspace 缺 workspaceId = %d, want 400", w.Code)
	}
	if w := observeCall(t, srv, http.MethodGet, "/api/dev/memory-file?target=workspace&workspaceId=w_nope", "", p); w.Code != http.StatusNotFound {
		t.Fatalf("看不见的项目 = %d, want 404", w.Code)
	}
	ws, err := s.CreateWorkspace(context.Background(), "t1", "u1", "记忆项目")
	if err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(root, "tenants/t1/workspaces", ws.ID)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "MEMORY.md"), []byte("项目用 Go"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, d = call("/api/dev/memory-file?target=workspace&workspaceId=" + ws.ID)
	if code != 0 || d.Content != "项目用 Go" || d.Cap != memoryWorkspaceCap || d.FileName != "MEMORY.md" || d.UsedRunes != 6 {
		t.Fatalf("workspace 文件 = %d %+v", code, d)
	}
}
