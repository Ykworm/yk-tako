package runengine

// P4-4 单测（doc/84 §7；施工包 §5.6/§6.2）：hybrid 召回链行为（零命中不注入、
// 有命中引用注入、臂失败 fail closed 不退回 pg_search、active 记忆路径无变化）
// 与两个 dev 诊断端点的守卫序与响应形状。打桩两臂检索器与 embedder，不打外部服务。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeDialogueLexSearcher 打桩词法臂：记下查询参数供断言，按 preset 回命中或错误。
type fakeDialogueLexSearcher struct {
	hits    []dialogueLexHit
	err     error
	healthy error
	calls   int

	gotQuery    string
	gotTenant   string
	gotWS       string
	gotUser     string
	gotVersion  int64
	gotTopK     int
}

func (f *fakeDialogueLexSearcher) Search(ctx context.Context, query, tenantID, workspaceID, userID string, indexVersion int64, topK int) ([]dialogueLexHit, error) {
	f.calls++
	f.gotQuery, f.gotTenant, f.gotWS, f.gotUser, f.gotVersion, f.gotTopK =
		query, tenantID, workspaceID, userID, indexVersion, topK
	return f.hits, f.err
}

func (f *fakeDialogueLexSearcher) Healthy(ctx context.Context) error { return f.healthy }

// fakeDialogueVecSearcher 打桩向量臂。
type fakeDialogueVecSearcher struct {
	hits    []dialogueVecHit
	err     error
	healthy error
	dim     int
	calls   int

	gotExpr string
	gotTopK int
}

func (f *fakeDialogueVecSearcher) Search(ctx context.Context, vec []float32, expr string, topK int) ([]dialogueVecHit, error) {
	f.calls++
	f.gotExpr, f.gotTopK = expr, topK
	return f.hits, f.err
}

func (f *fakeDialogueVecSearcher) Healthy(ctx context.Context) error { return f.healthy }
func (f *fakeDialogueVecSearcher) Dim() int                          { return f.dim }

// stubEmbedder 覆盖错误与形状异常路径（happy path 用 dialogue_clients_test.go 的
// fakeDialogueEmbedder——同包直接复用）。
type stubEmbedder struct {
	vecs [][]float32
	err  error
}

func (f *stubEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	return f.vecs, f.err
}
func (f *stubEmbedder) Dim() int { return 4 }

// newHybridStore 装配一台 hybrid 模式的 Store：pg_search 扩展 + 记忆索引（线一
// active 路径要保持可用）+ 指针播种 + 打桩运行时挂载（newTestStore 的清理收尾）。
func newHybridStore(t *testing.T) (*Store, *fakeDialogueLexSearcher, *fakeDialogueVecSearcher, *fakeDialogueEmbedder) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	if err := s.db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_search CASCADE`).Error; err != nil {
		t.Fatalf("CREATE EXTENSION pg_search: %v", err)
	}
	s.ConfigureMemory(MemoryConfig{
		TenantID: "default", Workspace: "/tmp/ws-test", Extract: true, Recall: true, RAG: true,
	})
	if err := s.EnsureMemoryIndexes(ctx); err != nil {
		t.Fatalf("EnsureMemoryIndexes: %v", err)
	}
	s.ConfigureDialogueRetrieval(true, "unit-embed")
	if _, err := s.EnsureDialogueRetrievalPointer(ctx, "tako_dialogue_unit", "tako_dialogue_unit"); err != nil {
		t.Fatalf("EnsureDialogueRetrievalPointer: %v", err)
	}
	lex := &fakeDialogueLexSearcher{}
	vec := &fakeDialogueVecSearcher{dim: 4}
	emb := &fakeDialogueEmbedder{dim: 4}
	s.attachDialogueRuntime(&dialogueHybridRuntime{
		cfg: DialogueRetrievalConfig{
			Mode:             DialogueModeHybrid,
			ESIndex:          "tako_dialogue_unit",
			MilvusCollection: "tako_dialogue_unit",
			Embedding:        dialogueEmbeddingConfig{Model: "unit-embed"},
		},
		es: lex, mil: vec, embed: emb,
		esQ: newFakeDialogueQueue(), vecQ: newFakeDialogueQueue(),
		esIndex: "tako_dialogue_unit", milColl: "tako_dialogue_unit",
	})
	return s, lex, vec, emb
}

// doc/84 §7.3：Milvus 零命中 → 即使 ES 有候选也不生成 <dialogue-context>，连空壳和
// 提示语都不给 LLM；并且不静默退回 pg_search（库里种了 pg_search 能命中的 chunk 也一样）。
func TestRecallHybridZeroVectorNoInject(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	lex.hits = []dialogueLexHit{{RunID: "r_lex", SessionID: "s1", Passage: "词法臂命中", Score: 2.0}}
	// vec.hits 为空：Milvus 零命中。
	s.addChunk(t, "r_pg", "s_pg", "/tmp/ws-test", "5433 端口 pg_search 会命中这条")

	res, err := s.Recall(ctx, "5433")
	if err != nil {
		t.Fatalf("零命中不是失败: %v", err)
	}
	if res.Block != "" || res.DialogueHits != 0 || res.InjectedRunes != 0 {
		t.Fatalf("零命中应整块不注入: %+v", res)
	}
	if lex.calls != 1 || vec.calls != 1 {
		t.Fatalf("两臂各应查一次: lex=%d vec=%d", lex.calls, vec.calls)
	}
}

// 有命中：引用块按模板拼接、注入条数与 rune 数正确、两臂查询参数与召回链同口径
// （脱敏文本、tenant/workspace/user 过滤、活跃代次、Top-10 候选）。
func TestRecallHybridInjectCitations(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	fin := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	lex.hits = []dialogueLexHit{
		{RunID: "r1", SessionID: "s1", Passage: "[会话 s1 · 场 r1]\n用户：数据库端口在哪", Score: 3.2, FinishedAt: &fin},
		{RunID: "r2", SessionID: "s2", Passage: "[会话 s2 · 场 r2]\n助手：5433", Score: 2.1},
	}
	vec.hits = []dialogueVecHit{{RunID: "r1", Score: 0.9}}

	res, err := s.Recall(ctx, "5433 端口")
	if err != nil {
		t.Fatal(err)
	}
	// r1 两臂都命中、r2 只在词法臂 → 注入 2 条。
	if res.DialogueHits != 2 {
		t.Fatalf("DialogueHits = %d, want 2", res.DialogueHits)
	}
	if res.InjectedRunes != len([]rune(res.Block)) {
		t.Fatalf("InjectedRunes 与块 rune 数不符: %d vs %d", res.InjectedRunes, len([]rune(res.Block)))
	}
	for _, frag := range []string{
		"<dialogue-context>",
		dialogueContextNoteHybrid,
		"- (2026-11-20, 会话 s1, run r1) 用户：数据库端口在哪\n",
		"- (, 会话 s2, run r2) 助手：5433\n",
		"</dialogue-context>",
	} {
		if !strings.Contains(res.Block, frag) {
			t.Fatalf("引用块缺片段 %q:\n%s", frag, res.Block)
		}
	}
	// 坐标头只出现一次（引用前缀），passage 首行被剥掉。
	if strings.Count(res.Block, "会话 s1") != 1 {
		t.Fatalf("坐标头应剥离只留引用前缀:\n%s", res.Block)
	}
	// 与召回链同口径：Top-10 候选、活跃代次、无身份（default 分区）→ tenant=default 不限人。
	if lex.gotTopK != dialogueRecallCandidates || lex.gotVersion != 1 {
		t.Fatalf("词法臂参数: topK=%d version=%d", lex.gotTopK, lex.gotVersion)
	}
	if lex.gotTenant != "default" || lex.gotUser != "" || lex.gotWS != "" {
		t.Fatalf("词法臂 scope: tenant=%q user=%q ws=%q", lex.gotTenant, lex.gotUser, lex.gotWS)
	}
	if !strings.Contains(vec.gotExpr, "tenant_id") || !strings.Contains(vec.gotExpr, "index_version == 1") {
		t.Fatalf("向量臂过滤表达式: %s", vec.gotExpr)
	}
}

// 查询文本先过 MaskPIIKeepUserMasked：两臂拿到的都是脱敏后的文本（doc/84 §7）。
func TestRecallHybridQueryMasked(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	vec.hits = []dialogueVecHit{{RunID: "r1", Score: 0.9}}
	if _, _, err := s.recallDialogueHybrid(ctx, "联系 13800138000 的记录"); err != nil {
		t.Fatal(err)
	}
	masked := MaskPIIKeepUserMasked("联系 13800138000 的记录")
	if masked == "联系 13800138000 的记录" {
		t.Fatal("测试前置：手机号应命中脱敏规则")
	}
	if lex.gotQuery != masked || !strings.Contains(lex.gotQuery, "*") {
		t.Fatalf("词法臂应拿脱敏文本: got %q want %q", lex.gotQuery, masked)
	}
	if !strings.Contains(masked, "*") {
		t.Fatalf("脱敏结果应带掩码星号: %q", masked)
	}
}

// 任一臂失败：本次召回不注入、错误上抛（run 侧记日志继续）——绝不静默退回 pg_search。
func TestRecallHybridArmFailureNoFallback(t *testing.T) {
	ctx := context.Background()
	s, lex, _, _ := newHybridStore(t)
	lex.err = &dialogueClientError{Class: dlgErrESUnavailable}
	s.addChunk(t, "r_pg", "s_pg", "/tmp/ws-test", "5433 端口 pg_search 会命中这条")

	res, err := s.Recall(ctx, "5433")
	if err == nil {
		t.Fatal("词法臂失败应上抛错误")
	}
	if res.Block != "" || res.DialogueHits != 0 {
		t.Fatalf("臂失败应不注入（也不退回 pg_search）: %+v", res)
	}
}

// 向量臂失败同样上抛不退回；embedding 服务不可用走同一纪律。
func TestRecallHybridVectorArmFailure(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	lex.hits = []dialogueLexHit{{RunID: "r1", Passage: "p", Score: 1.0}}
	vec.err = &dialogueClientError{Class: dlgErrMilvusUnavailable}

	res, err := s.Recall(ctx, "5433")
	if err == nil || res.Block != "" {
		t.Fatalf("向量臂失败应上抛且不注入: %+v err=%v", res, err)
	}
}

// 查询 embedding 形状异常（0 条或空向量）→ embedding_bad_response，不注入。
func TestRecallHybridEmbeddingShapeError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	s.ConfigureMemory(MemoryConfig{TenantID: "default", Workspace: "/tmp/ws-test", Recall: true, RAG: true})
	s.ConfigureDialogueRetrieval(true, "unit-embed")
	if _, err := s.EnsureDialogueRetrievalPointer(ctx, "tako_dialogue_unit", "tako_dialogue_unit"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		emb  *stubEmbedder
	}{
		{"零条", &stubEmbedder{vecs: [][]float32{}}},
		{"空向量", &stubEmbedder{vecs: [][]float32{{}}}},
	} {
		s.attachDialogueRuntime(&dialogueHybridRuntime{
			cfg: DialogueRetrievalConfig{Mode: DialogueModeHybrid, Embedding: dialogueEmbeddingConfig{Model: "unit-embed"}},
			es:  &fakeDialogueLexSearcher{}, mil: &fakeDialogueVecSearcher{dim: 4}, embed: tc.emb,
			esQ: newFakeDialogueQueue(), vecQ: newFakeDialogueQueue(),
			esIndex: "tako_dialogue_unit", milColl: "tako_dialogue_unit",
		})
		_, _, err := s.recallDialogueHybrid(ctx, "查询")
		if err == nil || !strings.Contains(err.Error(), dlgErrEmbedBadResponse) {
			t.Fatalf("%s: err = %v, want %s", tc.name, err, dlgErrEmbedBadResponse)
		}
	}
}

// 关停窗口：Hybrid=true 但查询面已摘（attachDialogueRuntime(nil)）→ fail closed 报错，
// 不注入、不退回 pg_search（分支判定看登记开关，不看运行时存在性）。
func TestRecallHybridShutdownFailClosed(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newHybridStore(t)
	s.addChunk(t, "r_pg", "s_pg", "/tmp/ws-test", "5433 端口 pg_search 会命中这条")
	s.attachDialogueRuntime(nil)

	res, err := s.Recall(ctx, "5433")
	if err == nil || !strings.Contains(err.Error(), "不注入") {
		t.Fatalf("关停窗口应 fail closed: err=%v", err)
	}
	if res.Block != "" || res.DialogueHits != 0 {
		t.Fatalf("关停窗口应不注入: %+v", res)
	}
}

// 指针切换（版本化重建切了指针）而本进程未重启：客户端仍绑旧索引 → 零候选不注入，
// 也不是失败（与对账器「等重启重绑」同语义）；两臂不再真查。
func TestRecallHybridPointerSwitchNoInject(t *testing.T) {
	ctx := context.Background()
	s, lex, _, _ := newHybridStore(t)
	lex.hits = []dialogueLexHit{{RunID: "r1", Passage: "p", Score: 1.0}}
	if err := s.db.Exec(`UPDATE dialogue_retrieval_pointer
		SET es_index = 'tako_dialogue_new', milvus_collection = 'tako_dialogue_new' WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}

	res, err := s.Recall(ctx, "5433")
	if err != nil {
		t.Fatalf("绑定错位不是失败: %v", err)
	}
	if res.Block != "" || res.DialogueHits != 0 {
		t.Fatalf("绑定错位应零候选不注入: %+v", res)
	}
	if lex.calls != 0 {
		t.Fatalf("绑定错位不应真查词法臂: %d 次", lex.calls)
	}
}

// active 记忆路径无变化：hybrid 挂载下线一（BM25 记忆）照常召回，响应字段不变
// （GET /api/memory/recall 的 {block,dialogueHits,injectedRunes,memoryHits} 语义不动）。
func TestRecallHybridMemoryLineUnchanged(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newHybridStore(t)
	s.addMemory(t, "dev 库是 PostgreSQL 5433 端口", time.Now())

	res, err := s.Recall(ctx, "5433 端口")
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoryHits != 1 {
		t.Fatalf("MemoryHits = %d, want 1（hybrid 不影响线一）", res.MemoryHits)
	}
	if res.DialogueHits != 0 || res.Block == "" || !strings.Contains(res.Block, "<memory-context>") {
		t.Fatalf("线一单独出块: %+v\n%s", res, res.Block)
	}
	if strings.Contains(res.Block, "<dialogue-context>") {
		t.Fatalf("两臂零命中不应有对话块:\n%s", res.Block)
	}
}

// hybrid 召回的项目过滤真源 = ctx 的 WorkspaceRef.WorkspaceID（run 起跑注入）；
// 预览端点没有 run 上下文 = 空串，只匹配未挂项目的块。
func TestRecallHybridWorkspaceFromCtx(t *testing.T) {
	ctx := withWorkspace(context.Background(), WorkspaceRef{
		TenantID: "default", Root: t.TempDir(), WorkspaceID: "w_proj",
	})
	s, lex, vec, _ := newHybridStore(t)
	vec.hits = []dialogueVecHit{{RunID: "r1", Score: 0.9}}

	if _, _, err := s.recallDialogueHybrid(ctx, "查询"); err != nil {
		t.Fatal(err)
	}
	if lex.gotWS != "w_proj" {
		t.Fatalf("词法臂 workspace = %q, want w_proj（取 ctx WorkspaceRef）", lex.gotWS)
	}
	if lex.gotTenant != "default" {
		t.Fatalf("词法臂 tenant = %q, want default（cfg.TenantID）", lex.gotTenant)
	}
}

// ── dev 诊断端点 ─────────────────────────────────────────────────────────────

// dialogueAPICall 直接调 handler（同 memory_observe_test.go 的 observeCall 模式）。
func dialogueAPICall(t *testing.T, s *Server, method, url, body string, p principal) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if body != "" {
		c.Request = httptest.NewRequest(method, url, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
	} else {
		c.Request = httptest.NewRequest(method, url, nil)
	}
	c.Set("tako.principal", p)
	switch {
	case method == http.MethodPost && strings.HasPrefix(url, "/api/dev/dialogue-retrieval/query"):
		s.devDialogueRetrievalQuery(c)
	case method == http.MethodGet && strings.HasPrefix(url, "/api/dev/dialogue-retrieval/status"):
		s.devDialogueRetrievalStatus(c)
	default:
		t.Fatalf("未登记的测试路由: %s %s", method, url)
	}
	return w
}

// 非 hybrid（查询面未挂载）→ 409 119005，字节数固定。
func TestDevDialogueRetrievalQueryOff(t *testing.T) {
	s := newTestStore(t, testLease, testTTL)
	srv := &Server{store: s}
	w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
		`{"query":"5433"}`, testOwner)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	want := `{"code":119005,"message":"当前未启用历史对话混合检索","data":null}`
	if w.Body.String() != want {
		t.Fatalf("119005 字节 = %s\nwant %s", w.Body.String(), want)
	}
}

// query 守卫序：JSON 坏 → 111001；query 空 → 111002；limit 非法 → 111002；
// 项目不可见 → 404 114001。全部 data:null。
func TestDevDialogueRetrievalQueryGuards(t *testing.T) {
	s, _, _, _ := newHybridStore(t)
	srv := &Server{store: s}

	t.Run("空query_111002", func(t *testing.T) {
		w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
			`{"query":"   "}`, testOwner)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		want := `{"code":111002,"message":"query 不能为空","data":null}`
		if w.Body.String() != want {
			t.Fatalf("111002 字节 = %s\nwant %s", w.Body.String(), want)
		}
	})

	t.Run("limit非法_111002", func(t *testing.T) {
		for _, raw := range []string{`{"query":"x","limit":0}`, `{"query":"x","limit":21}`, `{"query":"x","limit":"9"}`} {
			w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query", raw, testOwner)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400", raw, w.Code)
			}
			want := `{"code":111002,"message":"limit 必须是 1–20 的整数","data":null}`
			if w.Body.String() != want {
				t.Fatalf("%s 字节 = %s\nwant %s", raw, w.Body.String(), want)
			}
		}
	})

	t.Run("项目不可见_114001", func(t *testing.T) {
		w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
			`{"query":"x","workspaceId":"ws_missing"}`, testOwner)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		want := `{"code":114001,"message":"找不到这个项目","data":null}`
		if w.Body.String() != want {
			t.Fatalf("114001 字节 = %s\nwant %s", w.Body.String(), want)
		}
	})

	t.Run("坏JSON_111001", func(t *testing.T) {
		w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
			`{query`, testOwner)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":111001`) {
			t.Fatalf("坏 JSON = %d %s", w.Code, w.Body.String())
		}
	})
}

// hybrid 成功：data 键序 mode → lexical → vector → merged（结构体字段序即字节序）；
// merged 的 rrfScore 与 sources 按 RRF 结果；limit 缺省 10、显式 20 贯通到两臂 topK。
func TestDevDialogueRetrievalQueryShape(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	if err := s.db.WithContext(ctx).Create(&Workspace{
		ID: "ws_probe", TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Name: "探测", CreatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	fin := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	lex.hits = []dialogueLexHit{
		{RunID: "r1", SessionID: "s1", Passage: "p1", Score: 2.5, FinishedAt: &fin},
		{RunID: "r2", SessionID: "s2", Passage: "p2", Score: 1.5},
	}
	vec.hits = []dialogueVecHit{{RunID: "r1", Score: 0.9}}
	srv := &Server{store: s}

	w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
		`{"query":"5433","workspaceId":"ws_probe"}`, testOwner)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	// r1 两臂第 1（2/61）、r2 词法第 2（1/62）。rrfScore 是浮点，期望字节用同样的
	// FormatFloat 规则拼（json 对该量级的 float64 用 'f',-1 格式）。
	want := `{"code":0,"message":"","data":{"mode":"hybrid","lexical":[{"runId":"r1","score":2.5},{"runId":"r2","score":1.5}],"vector":[{"runId":"r1","score":0.9}],"merged":[{"runId":"r1","rrfScore":` +
		strconv.FormatFloat(2.0/61.0, 'f', -1, 64) +
		`,"sources":["lexical","vector"]},{"runId":"r2","rrfScore":` +
		strconv.FormatFloat(1.0/62.0, 'f', -1, 64) +
		`,"sources":["lexical"]}]}}`
	if w.Body.String() != want {
		t.Fatalf("query 形状 = %s\nwant %s", w.Body.String(), want)
	}
	// 键序自检：mode → lexical → vector → merged。
	body := w.Body.String()
	idx := []int{
		strings.Index(body, `"mode"`), strings.Index(body, `"lexical"`),
		strings.Index(body, `"vector"`), strings.Index(body, `"merged"`),
	}
	if idx[0] < 0 || idx[0] > idx[1] || idx[1] > idx[2] || idx[2] > idx[3] {
		t.Fatalf("data 键序错: %v in %s", idx, body)
	}
	// limit 缺省 10 贯通到两臂。
	if lex.gotTopK != 10 || vec.gotTopK != 10 {
		t.Fatalf("缺省 limit: lex=%d vec=%d", lex.gotTopK, vec.gotTopK)
	}

	// 显式 limit 20：合法并贯通；lexical/vector 各按臂内名次排列。
	w = dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
		`{"query":"5433","workspaceId":"ws_probe","limit":20}`, testOwner)
	if w.Code != http.StatusOK {
		t.Fatalf("limit 20 status = %d %s", w.Code, w.Body.String())
	}
	if lex.gotTopK != 20 || vec.gotTopK != 20 {
		t.Fatalf("limit 20 贯通: lex=%d vec=%d", lex.gotTopK, vec.gotTopK)
	}
	// 词法臂序列按名次：r1(1) 在 r2(2) 前；向量臂只有 r1。
	if !strings.Contains(w.Body.String(), `"lexical":[{"runId":"r1","score":2.5},{"runId":"r2","score":1.5}]`) ||
		!strings.Contains(w.Body.String(), `"vector":[{"runId":"r1","score":0.9}]`) {
		t.Fatalf("臂内名次序列:\n%s", w.Body.String())
	}
}

// 额外命中解析：同 run 两臂都出现时 sources 两个词都在（词法在前）——这里覆盖
// merged 列表对 sources 的透传（RRF 本体已在纯函数测试钉死）。
func TestDevDialogueRetrievalQuerySources(t *testing.T) {
	ctx := context.Background()
	s, lex, vec, _ := newHybridStore(t)
	if err := s.db.WithContext(ctx).Create(&Workspace{
		ID: "ws_probe", TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Name: "探测", CreatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	lex.hits = []dialogueLexHit{{RunID: "r9", Passage: "p", Score: 1.0}}
	vec.hits = []dialogueVecHit{{RunID: "r9", Score: 0.5}}
	srv := &Server{store: s}

	w := dialogueAPICall(t, srv, http.MethodPost, "/api/dev/dialogue-retrieval/query",
		`{"query":"x","workspaceId":"ws_probe"}`, testOwner)
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Code != 0 {
		t.Fatalf("响应 = %s err=%v", w.Body.String(), err)
	}
	if !strings.Contains(string(env.Data), `"sources":["lexical","vector"]`) {
		t.Fatalf("两臂同 run 的 sources:\n%s", env.Data)
	}
}

// status 形状：非 hybrid 只回 mode 与 coverage（pointer/lexical/vector 缺席）；
// hybrid 回五段全形状。两台都是全新库（覆盖水位全零）。
func TestDevDialogueRetrievalStatusShape(t *testing.T) {
	t.Run("非hybrid只回mode与coverage", func(t *testing.T) {
		s := newTestStore(t, testLease, testTTL)
		srv := &Server{store: s}
		w := dialogueAPICall(t, srv, http.MethodGet, "/api/dev/dialogue-retrieval/status", "", testOwner)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d %s", w.Code, w.Body.String())
		}
		want := `{"code":0,"message":"","data":{"mode":"pg_search","coverage":{"pending":0,"running":0,"retry":0,"failed":0,"oldestPendingAt":null,"consumerLag":0}}}`
		if w.Body.String() != want {
			t.Fatalf("pg_search status = %s\nwant %s", w.Body.String(), want)
		}
	})

	t.Run("hybrid五段全形状", func(t *testing.T) {
		s, _, _, _ := newHybridStore(t)
		srv := &Server{store: s}
		w := dialogueAPICall(t, srv, http.MethodGet, "/api/dev/dialogue-retrieval/status", "", testOwner)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d %s", w.Code, w.Body.String())
		}
		want := `{"code":0,"message":"","data":{"mode":"hybrid",` +
			`"pointer":{"indexVersion":1,"esIndex":"tako_dialogue_unit","milvusCollection":"tako_dialogue_unit"},` +
			`"lexical":{"kind":"elasticsearch","index":"tako_dialogue_unit","healthy":true},` +
			`"vector":{"kind":"milvus","collection":"tako_dialogue_unit","healthy":true,"embeddingModel":"unit-embed","dimensions":4},` +
			`"coverage":{"pending":0,"running":0,"retry":0,"failed":0,"oldestPendingAt":null,"consumerLag":0}}}`
		if w.Body.String() != want {
			t.Fatalf("hybrid status = %s\nwant %s", w.Body.String(), want)
		}
	})
}

// status 的 consumerLag = 两臂之和；任一臂查不动 → -1（「未知」），不虚构 0。
func TestDialogueStatusLag(t *testing.T) {
	ctx := context.Background()
	esQ, vecQ := newFakeDialogueQueue(), newFakeDialogueQueue()
	rt := &dialogueHybridRuntime{esQ: esQ, vecQ: vecQ}
	if got := dialogueStatusLag(ctx, rt); got != 0 {
		t.Fatalf("零积压 lag = %d, want 0", got)
	}
	esQ.lagOverride = 3
	vecQ.lagOverride = 4
	if got := dialogueStatusLag(ctx, rt); got != 7 {
		t.Fatalf("两臂之和 = %d, want 7", got)
	}
	vecQ.lagErr = errors.New("lag 查询失败")
	if got := dialogueStatusLag(ctx, rt); got != -1 {
		t.Fatalf("任一臂失败 lag = %d, want -1", got)
	}
	if got := dialogueStatusLag(ctx, nil); got != 0 {
		t.Fatalf("非 hybrid（nil 运行时）lag = %d, want 0", got)
	}
}
