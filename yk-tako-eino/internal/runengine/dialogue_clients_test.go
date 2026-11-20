package runengine

// 历史对话混合检索的客户端与消费者回归（doc/84 §5/§6；施工包 P2 第 8 条）。
//
// 外部客户端（ES/embedding）全部走 httptest 假服务（单元测试不触网、不打真模型）；
// Milvus 不测网络路径（SDK 调用面在 P0 实测与 P5 活体验收覆盖），单测钉住：
// schema 形状、维度解析、标量过滤表达式、维度校验（不触网即拒）与 119004 映射。
// 消费者用 fake 索引/fake embedder 钉三件事：先持久化后提交、重复消息只写一次外部、
// 失败分组只回退它自己的任务。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeDialogueESIndex 记录 ES 侧外部写（单测断言「外部被写了什么/几次」）。
type fakeDialogueESIndex struct {
	mu        sync.Mutex
	upserts   []dialogueESDoc
	deletes   []string
	upsertErr error
	deleteErr error
}

func (f *fakeDialogueESIndex) Upsert(ctx context.Context, doc dialogueESDoc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, doc)
	return f.upsertErr
}

func (f *fakeDialogueESIndex) Delete(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, runID)
	return f.deleteErr
}

// fakeDialogueMilvusIndex 记录 Milvus 侧外部写并按 run_id 供给已存向量（move 用）。
type fakeDialogueMilvusIndex struct {
	mu        sync.Mutex
	vectors   map[string][]float32
	upserts   [][]dialogueMilvusRow
	deletes   [][]string
	upsertErr error
	deleteErr error
}

func (f *fakeDialogueMilvusIndex) Upsert(ctx context.Context, rows []dialogueMilvusRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, rows)
	return f.upsertErr
}

func (f *fakeDialogueMilvusIndex) DeleteByRunIDs(ctx context.Context, runIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, runIDs)
	return f.deleteErr
}

func (f *fakeDialogueMilvusIndex) GetVector(ctx context.Context, runID string) ([]float32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vectors[runID]
	return v, ok, nil
}

// fakeDialogueEmbedder 是打桩 embedder：按文本给确定性向量（同文本同向量）。
type fakeDialogueEmbedder struct {
	dim   int
	calls [][]string
	err   error
}

func (f *fakeDialogueEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, texts)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = float32(len(t)%7+1) + float32(j)
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeDialogueEmbedder) Dim() int { return f.dim }

// fakeDialogueCounter 是打桩 token 计数器（per-text 覆盖 + 默认值）。
type fakeDialogueCounter struct {
	per map[string]int
	def int
}

func (c fakeDialogueCounter) Count(text string) int {
	if v, ok := c.per[text]; ok {
		return v
	}
	return c.def
}

// dialogueTaskSpec 是给消费者测试造任务的便捷规格。
func dialogueTaskSpec(runID, kind string) DialogueIndexEventSpec {
	return DialogueIndexEventSpec{
		RunID: RunID(runID), SessionID: SessionID("s_" + strings.TrimPrefix(runID, "r_")),
		TenantID: testOwner.TenantID, UserID: testOwner.UserID,
		Kind: kind, EmbeddingModel: "test-embed-model",
	}
}

// ── ES 客户端（httptest 假服务）──────────────────────────────────────────────

// newFakeESServer 起一个按 handler 应答的 ES 假服务，返回绑定它的客户端与请求日志。
type esReqLog struct {
	mu    sync.Mutex
	calls []esReq
}

type esReq struct {
	Method string
	Path   string
	Body   string
	Auth   string
}

func (l *esReqLog) snapshot() []esReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]esReq(nil), l.calls...)
}

func newFakeESServer(t *testing.T, index string, handle func(seq int, r esReq) (int, string)) (*dialogueESClient, *esReqLog) {
	t.Helper()
	log := &esReqLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := ""
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			body = string(raw)
		}
		auth := ""
		if u, p, ok := req.BasicAuth(); ok {
			auth = u + ":" + p
		}
		r := esReq{Method: req.Method, Path: req.URL.Path, Body: body, Auth: auth}
		log.mu.Lock()
		seq := len(log.calls)
		log.calls = append(log.calls, r)
		log.mu.Unlock()
		status, resp := handle(seq, r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return newDialogueESClient(srv.URL, "elastic", "pw", index), log
}

// io_ReadAll 避免测试文件里直接依赖 io 的命名冲突。
func io_ReadAll(r interface{ Read([]byte) (int, error) }) (string, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return string(buf), nil
			}
			return string(buf), err
		}
	}
}

func TestDialogueESClientContract(t *testing.T) {
	ctx := context.Background()
	const idx = "tako_dialogue_v1"

	t.Run("EnsureIndex_mapping与幂等", func(t *testing.T) {
		c, log := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			if seq == 0 {
				return 200, `{"acknowledged":true}`
			}
			return 400, `{"error":{"type":"resource_already_exists_exception","reason":"x"}}`
		})
		if err := c.EnsureIndex(ctx); err != nil {
			t.Fatalf("首次建索引: %v", err)
		}
		if err := c.EnsureIndex(ctx); err != nil {
			t.Fatalf("重复建索引（already_exists）: %v", err)
		}
		reqs := log.snapshot()
		if len(reqs) != 2 || reqs[0].Method != http.MethodPut || reqs[0].Path != "/"+idx {
			t.Fatalf("请求日志 = %+v", reqs)
		}
		if reqs[0].Auth != "elastic:pw" {
			t.Fatalf("Basic auth = %q", reqs[0].Auth)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
			t.Fatalf("mapping 不是合法 JSON: %v", err)
		}
		props := body["mappings"].(map[string]any)["properties"].(map[string]any)
		passage := props["passage"].(map[string]any)
		if passage["analyzer"] != "ik_max_word" || passage["search_analyzer"] != "ik_smart" {
			t.Fatalf("passage 分析器 = %v", passage)
		}
		if props["tenant_id"].(map[string]any)["type"] != "keyword" {
			t.Fatal("归属字段不是 keyword")
		}
	})

	t.Run("EnsureIndex_其他400是永久错误", func(t *testing.T) {
		c, _ := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			return 400, `{"error":{"type":"illegal_argument_exception"}}`
		})
		err := c.EnsureIndex(ctx)
		var ce *dialogueClientError
		if !errors.As(err, &ce) || ce.Class != dlgErrESPermanent || !ce.Permanent {
			t.Fatalf("err = %v, want es_permanent 永久", err)
		}
	})

	t.Run("Upsert字节与自愈", func(t *testing.T) {
		now := time.Now()
		c, log := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			if seq == 0 {
				return 404, `{"error":{"type":"index_not_found_exception","reason":"no such index"}}`
			}
			if seq == 1 { // 自愈：EnsureIndex
				return 200, `{"acknowledged":true}`
			}
			return 200, `{"result":"created"}`
		})
		doc := dialogueESDoc{
			RunID: "r_1", SessionID: "s_1", TenantID: "t1", UserID: "u1", WorkspaceID: "w1",
			Passage: "脱敏正文", IndexVersion: 1, EmbeddingModel: "m",
			RunFinishedAt: &now, IndexedAt: now,
		}
		if err := c.Upsert(ctx, doc); err != nil {
			t.Fatalf("404 自愈 upsert: %v", err)
		}
		reqs := log.snapshot()
		if len(reqs) != 3 {
			t.Fatalf("请求数 = %d, want 3（upsert→建索引→重试）", len(reqs))
		}
		if reqs[2].Method != http.MethodPut || reqs[2].Path != "/"+idx+"/_doc/r_1" {
			t.Fatalf("upsert 请求 = %s %s", reqs[2].Method, reqs[2].Path)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(reqs[2].Body), &got); err != nil {
			t.Fatal(err)
		}
		if got["run_id"] != "r_1" || got["passage"] != "脱敏正文" || got["tenant_id"] != "t1" ||
			got["workspace_id"] != "w1" || got["index_version"] != float64(1) {
			t.Fatalf("文档体 = %v", got)
		}
	})

	t.Run("状态码分类", func(t *testing.T) {
		c, _ := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			if strings.HasPrefix(r.Path, "/"+idx+"/_doc/") && r.Method == http.MethodPut {
				if seq == 0 {
					return 429, `{"error":"too many"}`
				}
				return 400, `{"error":{"type":"x"}}`
			}
			return 500, `{}`
		})
		err := c.Upsert(ctx, dialogueESDoc{RunID: "r_c"})
		var ce *dialogueClientError
		if !errors.As(err, &ce) || ce.Class != dlgErrESUnavailable || ce.Permanent {
			t.Fatalf("429 = %v, want es_unavailable 瞬时", err)
		}
		err = c.Upsert(ctx, dialogueESDoc{RunID: "r_c"})
		if !errors.As(err, &ce) || ce.Class != dlgErrESPermanent || !ce.Permanent {
			t.Fatalf("400 = %v, want es_permanent 永久", err)
		}
	})

	t.Run("Delete三态都算gone", func(t *testing.T) {
		states := []struct {
			name   string
			status int
			body   string
		}{
			{"deleted", 200, `{"result":"deleted"}`},
			{"not_found", 200, `{"result":"not_found"}`},
			{"索引不在", 404, `{"error":{"type":"index_not_found_exception"}}`},
		}
		for _, st := range states {
			c, log := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
				return st.status, st.body
			})
			if err := c.Delete(ctx, "r_d"); err != nil {
				t.Fatalf("%s: %v", st.name, err)
			}
			if reqs := log.snapshot(); len(reqs) != 1 || reqs[0].Method != http.MethodDelete ||
				reqs[0].Path != "/"+idx+"/_doc/r_d" {
				t.Fatalf("%s: 请求 = %+v", st.name, reqs)
			}
		}
		c, _ := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			return 503, `{}`
		})
		var ce *dialogueClientError
		err := c.Delete(ctx, "r_d")
		if !errors.As(err, &ce) || ce.Class != dlgErrESUnavailable || ce.Permanent {
			t.Fatalf("503 delete = %v, want 瞬时", err)
		}
	})

	t.Run("Search与Analyze", func(t *testing.T) {
		c, log := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			if strings.HasSuffix(r.Path, "/_search") {
				// 校验查询体形状：bool filter + match。
				var body map[string]any
				if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
					t.Errorf("search body: %v", err)
				}
				return 200, `{"hits":{"hits":[
					{"_id":"r_1","_score":2.5,"_source":{"run_id":"r_1","session_id":"s_1","passage":"命中","index_version":1,"indexed_at":"2026-11-20T00:00:00Z"}},
					{"_id":"r_2","_score":1.5,"_source":{"session_id":"s_2","passage":"缺run_id回退_id","index_version":1}}]}}`
			}
			if strings.HasSuffix(r.Path, "/_analyze") {
				var body map[string]any
				if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
					t.Errorf("analyze body: %v", err)
				}
				if body["analyzer"] != "ik_max_word" {
					t.Errorf("analyzer = %v", body["analyzer"])
				}
				return 200, `{"tokens":[{"token":"数据库"},{"token":"端口"},{"token":"5433"}]}`
			}
			return 404, `{}`
		})
		hits, err := c.Search(ctx, "数据库端口", "t1", "w1", "u1", 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].RunID != "r_1" || hits[0].Score != 2.5 ||
			hits[0].Passage != "命中" || hits[0].IndexVersion != 1 || hits[0].IndexedAt == nil {
			t.Fatalf("hits = %+v", hits)
		}
		if hits[1].RunID != "r_2" {
			t.Fatalf("缺 run_id 应回退 _id: %+v", hits[1])
		}
		toks, err := c.Analyze(ctx, "ik_max_word", "数据库端口 5433")
		if err != nil || len(toks) != 3 || toks[0] != "数据库" || toks[2] != "5433" {
			t.Fatalf("analyze = %v err=%v", toks, err)
		}
		if reqs := log.snapshot(); len(reqs) != 2 {
			t.Fatalf("请求数 = %d", len(reqs))
		}
	})

	t.Run("启动预检断言ik真实生效", func(t *testing.T) {
		// 正例：P0 实测字节——ik_max_word("数据库端口 5433") 含 数据库/端口/5433，
		// ik_smart("周末") 含 周末。
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			analyzer, _ := body["analyzer"].(string)
			if analyzer == "ik_max_word" {
				_, _ = w.Write([]byte(`{"tokens":[{"token":"数据库"},{"token":"端口"},{"token":"5433"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"tokens":[{"token":"周末"}]}`))
		}))
		defer good.Close()
		if err := dialoguePrecheckAnalyzers(newDialogueESClient(good.URL, "", "", idx)); err != nil {
			t.Fatalf("P0 实测字节应通过预检: %v", err)
		}

		// 反例 1：ik 插件没了（标准分词器把中文切碎）。
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(`{"tokens":[{"token":"数"},{"token":"据"},{"token":"库"}]}`))
		}))
		defer bad.Close()
		err := dialoguePrecheckAnalyzers(newDialogueESClient(bad.URL, "", "", idx))
		if err == nil || !strings.Contains(err.Error(), "预检失败") {
			t.Fatalf("ik 失效应启动即退: %v", err)
		}

		// 反例 2：分析器名不识别（ES 400）→ 包装成客户端错误上抛。
		c, _ := newFakeESServer(t, idx, func(seq int, r esReq) (int, string) {
			return 400, `{"error":{"type":"illegal_argument_exception","reason":"analyzer [ik_max_word] not found"}}`
		})
		_, aerr := c.Analyze(ctx, "ik_max_word", "数据库端口")
		var ace *dialogueClientError
		if !errors.As(aerr, &ace) || ace.Class != dlgErrESPermanent {
			t.Fatalf("analyzer 400 = %v, want es_permanent", aerr)
		}
	})
}

// ── embedding 客户端（httptest 假服务）───────────────────────────────────────

func TestDialogueEmbeddingClient(t *testing.T) {
	ctx := context.Background()

	// fakeEmbeddingsServer：按 handler 应答 /embeddings；记录收到的请求体。
	newEmbServer := func(t *testing.T, handle func(input []string) (int, string)) (*dialogueEmbedder, *[]string) {
		t.Helper()
		var inputs []string
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			var r dialogueEmbeddingRequest
			if err := json.NewDecoder(req.Body).Decode(&r); err != nil {
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			inputs = append(inputs, fmt.Sprintf("%d", len(r.Input)))
			mu.Unlock()
			status, body := handle(r.Input)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		e := newDialogueEmbedder(dialogueEmbeddingConfig{
			BaseURL: srv.URL, APIKey: "k", Model: "test-embedding",
			Timeout: 5 * time.Second, BatchSize: 8, MaxInputTokens: 6000,
		})
		return e, &inputs
	}
	vecsJSON := func(dim, n int) string {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(fmt.Sprintf(`{"index":%d,"embedding":[`, i))
			for j := 0; j < dim; j++ {
				if j > 0 {
					b.WriteString(",")
				}
				b.WriteString("0.5")
			}
			b.WriteString(`]}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}

	t.Run("正常与维度指纹锁定", func(t *testing.T) {
		e, inputs := newEmbServer(t, func(input []string) (int, string) {
			return 200, vecsJSON(1024, len(input))
		})
		out, err := e.EmbedBatch(ctx, []string{"一", "二"})
		if err != nil || len(out) != 2 || len(out[0]) != 1024 || len(out[1]) != 1024 {
			t.Fatalf("out=%d err=%v", len(out), err)
		}
		if e.Dim() != 1024 {
			t.Fatalf("指纹 = %d, want 1024", e.Dim())
		}
		if d, err := e.ProbeDim(ctx); err != nil || d != 1024 {
			t.Fatalf("ProbeDim = %d err=%v", d, err)
		}
		if got := *inputs; len(got) != 2 || got[0] != "2" || got[1] != "1" {
			t.Fatalf("请求批次 = %v", got)
		}
		// 维度变了（模型换了）→ 永久错误。
		srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = w.Write([]byte(vecsJSON(512, 1)))
		}))
		defer srv2.Close()
		e.cfg.BaseURL = srv2.URL
		_, err = e.EmbedBatch(ctx, []string{"三"})
		var ce *dialogueClientError
		if !errors.As(err, &ce) || ce.Class != dlgErrEmbedDimMismatch || !ce.Permanent {
			t.Fatalf("维度漂移 = %v, want embedding_dimension_mismatch 永久", err)
		}
	})

	t.Run("非2xx分类", func(t *testing.T) {
		cases := []struct {
			status    int
			wantClass string
			perm      bool
		}{
			{429, dlgErrEmbedUnavailable, false},
			{500, dlgErrEmbedUnavailable, false},
			{503, dlgErrEmbedUnavailable, false},
			{401, dlgErrEmbedBadResponse, true},
			{404, dlgErrEmbedBadResponse, true},
		}
		for _, c := range cases {
			e, _ := newEmbServer(t, func(input []string) (int, string) {
				return c.status, `{"error":"x"}`
			})
			_, err := e.EmbedBatch(ctx, []string{"t"})
			var ce *dialogueClientError
			if !errors.As(err, &ce) || ce.Class != c.wantClass || ce.Permanent != c.perm {
				t.Fatalf("%d: err = %v, want %s perm=%v", c.status, err, c.wantClass, c.perm)
			}
		}
	})

	t.Run("2xx非法响应", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"坏JSON", `{"data": [`},
			{"条数不符", vecsJSON(4, 1)},
			{"空向量", `{"data":[{"index":0,"embedding":[]}]}`},
			{"顺序错位", `{"data":[{"index":1,"embedding":[0.5]},{"index":0,"embedding":[0.5]}]}`},
		}
		for _, c := range cases {
			e, _ := newEmbServer(t, func(input []string) (int, string) {
				return 200, c.body
			})
			_, err := e.EmbedBatch(ctx, []string{"一", "二"})
			var ce *dialogueClientError
			if !errors.As(err, &ce) || ce.Class != dlgErrEmbedBadResponse || !ce.Permanent {
				t.Fatalf("%s: err = %v, want embedding_bad_response 永久", c.name, err)
			}
		}
	})

	t.Run("超批次上限本地拒绝", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			hits++
			_, _ = w.Write([]byte(vecsJSON(4, 9)))
		}))
		defer srv.Close()
		e := newDialogueEmbedder(dialogueEmbeddingConfig{
			BaseURL: srv.URL, APIKey: "k", Model: "m",
			Timeout: time.Second, BatchSize: 8, MaxInputTokens: 6000,
		})
		texts := make([]string, 9)
		_, err := e.EmbedBatch(ctx, texts)
		var ce *dialogueClientError
		if !errors.As(err, &ce) || !ce.Permanent {
			t.Fatalf("超批次 = %v, want 本地永久拒绝", err)
		}
		if hits != 0 {
			t.Fatalf("不该发请求（hits=%d）", hits)
		}
	})
}

// TestDialogueEmbedGroups：分批是「分批不是截断」——单请求 ≤BatchSize 条、
// 累计 token ≤MaxInputTokens；超单块上限的文本整条进 overLimit（应用层闸，
// provider 不自查错）。
func TestDialogueEmbedGroups(t *testing.T) {
	counter := fakeDialogueCounter{per: map[string]int{
		"a": 3, "b": 4, "c": 4, // a+b=7 ≤10；+c=11 超 → c 进下一组
		"d": 99, // 单块超限
		"e": 2,
	}, def: 1}
	texts := []string{"a", "b", "c", "d", "e", "f", "g"}
	groups, over := dialogueEmbedGroups(counter, texts, 2, 10)
	if len(over) != 1 || over[0] != 3 {
		t.Fatalf("overLimit = %v, want [3]（d 整条超限）", over)
	}
	// 期望：g0=[a,b](3+4=7)、g1=[c,e](4+2=6)、g2=[f,g](1+1=2)；批次上限 2 同时起作用。
	if len(groups) != 3 ||
		fmt.Sprint(groups[0]) != "[0 1]" || fmt.Sprint(groups[1]) != "[2 4]" || fmt.Sprint(groups[2]) != "[5 6]" {
		t.Fatalf("groups = %v", groups)
	}

	// 批次上限优先：单块都很小，但一组最多 batchSize 条。
	counter2 := fakeDialogueCounter{def: 1}
	g2, over2 := dialogueEmbedGroups(counter2, []string{"a", "b", "c", "d", "e"}, 2, 100)
	if len(over2) != 0 || len(g2) != 3 {
		t.Fatalf("batchSize=2: groups=%d over=%v, want 3 组 0 超限", len(g2), over2)
	}

	// nil counter（计数器装配失败的兜底形态）：跳过 token 上限，只按条数分组。
	g3, over3 := dialogueEmbedGroups(nil, []string{"a", "b", "c"}, 2, 10)
	if len(over3) != 0 || len(g3) != 2 || fmt.Sprint(g3[0]) != "[0 1]" {
		t.Fatalf("nil counter: groups=%v over=%v", g3, over3)
	}
}

// ── 错误映射（119001–119005）─────────────────────────────────────────────────

func TestDialogueRetrievalHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int
	}{
		{"检索未启用", ErrDialogueRetrievalOff, http.StatusConflict, codeDialogueRetrievalOff},
		{"指纹哨兵", ErrDialogueDimensionMismatch, http.StatusConflict, codeDialogueDimensionMismatch},
		{"ES不可用", &dialogueClientError{Class: dlgErrESUnavailable}, http.StatusServiceUnavailable, codeDialogueLexicalUnavailable},
		{"ES永久", &dialogueClientError{Class: dlgErrESPermanent, Permanent: true}, http.StatusServiceUnavailable, codeDialogueLexicalUnavailable},
		{"Milvus不可用", &dialogueClientError{Class: dlgErrMilvusUnavailable}, http.StatusServiceUnavailable, codeDialogueVectorUnavailable},
		{"Milvus维度", &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true}, http.StatusConflict, codeDialogueDimensionMismatch},
		{"embedding不可用", &dialogueClientError{Class: dlgErrEmbedUnavailable}, http.StatusServiceUnavailable, codeDialogueEmbeddingUnavailable},
		{"embedding超限", &dialogueClientError{Class: dlgErrEmbedOverLimit, Permanent: true}, http.StatusServiceUnavailable, codeDialogueEmbeddingUnavailable},
		{"embedding维度", &dialogueClientError{Class: dlgErrEmbedDimMismatch, Permanent: true}, http.StatusConflict, codeDialogueDimensionMismatch},
	}
	for _, c := range cases {
		status, code, msg, ok := dialogueRetrievalHTTPError(c.err)
		if !ok || status != c.wantStatus || code != c.wantCode || msg == "" {
			t.Fatalf("%s: (%d, %d, %q, %v)", c.name, status, code, msg, ok)
		}
	}
	// 包一层也算（errors.As 穿透）。
	wrapped := fmt.Errorf("查询失败: %w", &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true})
	if status, code, _, ok := dialogueRetrievalHTTPError(wrapped); !ok || status != http.StatusConflict || code != codeDialogueDimensionMismatch {
		t.Fatalf("包装错误: (%d, %d, %v)", status, code, ok)
	}
	// 不相干的错误不接管。
	if _, _, _, ok := dialogueRetrievalHTTPError(errors.New("别的事")); ok {
		t.Fatal("不相干错误不该被接管")
	}
}

// ── Milvus 形状（不触网）─────────────────────────────────────────────────────

func TestDialogueMilvusShape(t *testing.T) {
	schema := dialogueMilvusSchema("tako_dialogue_v1", 1024)
	if schema.CollectionName != "tako_dialogue_v1" || len(schema.Fields) != 11 {
		t.Fatalf("schema = %s, %d 字段", schema.CollectionName, len(schema.Fields))
	}
	pk := schema.Fields[0]
	if pk.Name != "run_id" || pk.DataType != entity.FieldTypeVarChar || !pk.PrimaryKey {
		t.Fatalf("主键 = %s %v %v", pk.Name, pk.DataType, pk.PrimaryKey)
	}
	for _, f := range schema.Fields {
		if f.Name == "passage" && (f.DataType != entity.FieldTypeVarChar ||
			f.TypeParams[entity.TypeParamMaxLength] != "16384") {
			t.Fatalf("passage = %v maxlen=%q", f.DataType, f.TypeParams[entity.TypeParamMaxLength])
		}
		if f.Name == "embedding" {
			if f.DataType != entity.FieldTypeFloatVector || f.TypeParams[entity.TypeParamDim] != "1024" {
				t.Fatalf("embedding 字段 = %v dim=%q", f.DataType, f.TypeParams[entity.TypeParamDim])
			}
		}
	}
	coll := &entity.Collection{Schema: schema}
	if d, ok := dialogueSchemaDim(coll); !ok || d != 1024 {
		t.Fatalf("dialogueSchemaDim = %d %v", d, ok)
	}
	if _, ok := dialogueSchemaDim(nil); ok {
		t.Fatal("nil collection 应解析失败")
	}

	if got := dialogueMilvusFilter("t1", "w1", "", 7); got != `tenant_id == "t1" && workspace_id == "w1" && index_version == 7` {
		t.Fatalf("filter = %q", got)
	}
	if got := dialogueMilvusFilter("t1", "w1", "u1", 0); !strings.Contains(got, `user_id == "u1"`) {
		t.Fatalf("filter = %q（缺 user 等值）", got)
	}
	if got := milvusQuoteExpr(`a"b`); got != `a\"b` {
		t.Fatalf("quote = %q", got)
	}

	// 维度校验在任何网络调用之前（cli 为 nil 也不许 panic）。
	c := &dialogueMilvusClient{coll: "c", dim: 4}
	err := c.Upsert(context.Background(), []dialogueMilvusRow{{RunID: "r", Embedding: make([]float32, 3)}})
	var ce *dialogueClientError
	if !errors.As(err, &ce) || ce.Class != dlgErrMilvusDimMismatch || !ce.Permanent {
		t.Fatalf("upsert 维度错 = %v, want milvus_dimension_mismatch 永久", err)
	}
	_, serr := c.Search(context.Background(), make([]float32, 3), "", 10)
	if !errors.As(serr, &ce) || ce.Class != dlgErrMilvusDimMismatch {
		t.Fatalf("search 维度错 = %v", serr)
	}
}

// TestDialogueMilvusErrorClassify：doTransient 的错误分类（doc/84 §6.2 机器词表的
// milvus 侧）——报文含 dimension 的错误是永久维度不符（119004 类，死信不重试）；
// 其余一律 milvus_unavailable 瞬时（退避）；前两次的瞬时抖动内被重试吞掉（serverless
// 冷启动，P0 实测 2 次内稳定）；ctx 取消后不再重试。
func TestDialogueMilvusErrorClassify(t *testing.T) {
	ctx := context.Background()
	var ce *dialogueClientError

	err := doTransient(ctx, func() error {
		return errors.New("dimension mismatch: expect 4 but got 8")
	})
	if !errors.As(err, &ce) || ce.Class != dlgErrMilvusDimMismatch || !ce.Permanent {
		t.Fatalf("dimension 错误 = %v, want milvus_dimension_mismatch 永久", err)
	}

	err = doTransient(ctx, func() error {
		return errors.New("server unavailable")
	})
	if !errors.As(err, &ce) || ce.Class != dlgErrMilvusUnavailable || ce.Permanent {
		t.Fatalf("其他错误 = %v, want milvus_unavailable 瞬时", err)
	}

	calls := 0
	err = doTransient(ctx, func() error {
		calls++
		if calls < 2 {
			return errors.New("timeout")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("瞬时抖动后成功 = %v calls=%d, want 重试到成功", err, calls)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	calls = 0
	err = doTransient(cctx, func() error {
		calls++
		return errors.New("timeout")
	})
	if calls != 1 || err == nil {
		t.Fatalf("ctx 取消后 = %v calls=%d, want 首败即返回不重试", err, calls)
	}
}

// ── ES 消费者（fake 索引）────────────────────────────────────────────────────

func TestDialogueESProcessor(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	seedChunk := func(t *testing.T, runID, passage string) {
		t.Helper()
		if err := s.db.Exec(
			`INSERT INTO dialogue_chunks (run_id, session_id, tenant_id, user_id, workspace, passage, runes, truncated, indexed_at)
			 VALUES (?, 's_p', ?, ?, '/tmp/ws-test', ?, ?, false, now())
			 ON CONFLICT (run_id) DO UPDATE SET passage = EXCLUDED.passage`,
			runID, testOwner.TenantID, testOwner.UserID, passage, len([]rune(passage))).Error; err != nil {
			t.Fatal(err)
		}
	}

	t.Run("upsert脱敏与归属", func(t *testing.T) {
		seedChunk(t, "r_es1", "密钥是 key=sk-proj-AbCdEfGhIjKlMnOpQrSt 请保管好")
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_es1", DialogueJobKindUpsert)); err != nil {
			t.Fatal(err)
		}
		es := dialogueJobRow(t, s, "r_es1", DialogueJobKindUpsert, DialogueTargetES)
		es.WorkspaceID = "ws_proj" // 归属以任务行为准（不读 chunk 行的 workspace 列）

		fake := &fakeDialogueESIndex{}
		p := newDialogueESProcessor(s, fake)
		out := p.Process(ctx, []*DialogueIndexJob{es})
		if len(out) != 1 || out[0].Class != "" {
			t.Fatalf("outcome = %+v, want 成功", out)
		}
		if len(fake.upserts) != 1 {
			t.Fatalf("upserts = %d", len(fake.upserts))
		}
		doc := fake.upserts[0]
		if strings.Contains(doc.Passage, "sk-proj-AbCdEfGhIjKlMnOpQrSt") {
			t.Fatalf("外部索引存了未脱敏正文: %q", doc.Passage)
		}
		if !strings.Contains(doc.Passage, "sk-p") {
			t.Fatalf("脱敏后的正文不在: %q", doc.Passage)
		}
		if doc.WorkspaceID != "ws_proj" || doc.TenantID != testOwner.TenantID || doc.RunID != "r_es1" {
			t.Fatalf("归属没从任务行来: %+v", doc)
		}
		if doc.IndexVersion != 1 || doc.EmbeddingModel != "test-embed-model" {
			t.Fatalf("文档代次/模型 = %d/%s", doc.IndexVersion, doc.EmbeddingModel)
		}
	})

	t.Run("upsert缺块收敛删除", func(t *testing.T) {
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_es_gone", DialogueJobKindUpsert)); err != nil {
			t.Fatal(err)
		}
		es := dialogueJobRow(t, s, "r_es_gone", DialogueJobKindUpsert, DialogueTargetES)
		fake := &fakeDialogueESIndex{}
		out := newDialogueESProcessor(s, fake).Process(ctx, []*DialogueIndexJob{es})
		if out[0].Class != "" {
			t.Fatalf("outcome = %+v, want 成功", out[0])
		}
		if len(fake.upserts) != 0 || len(fake.deletes) != 1 || fake.deletes[0] != "r_es_gone" {
			t.Fatalf("外部调用 = upserts:%d deletes:%v", len(fake.upserts), fake.deletes)
		}
	})

	t.Run("move缺块直接完成", func(t *testing.T) {
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_es_mv", DialogueJobKindMove)); err != nil {
			t.Fatal(err)
		}
		es := dialogueJobRow(t, s, "r_es_mv", DialogueJobKindMove, DialogueTargetES)
		fake := &fakeDialogueESIndex{}
		out := newDialogueESProcessor(s, fake).Process(ctx, []*DialogueIndexJob{es})
		if out[0].Class != "" || len(fake.upserts) != 0 || len(fake.deletes) != 0 {
			t.Fatalf("move 缺块 = %+v 外部:%d/%d", out[0], len(fake.upserts), len(fake.deletes))
		}
	})

	t.Run("delete删除外部文档", func(t *testing.T) {
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_es_del", DialogueJobKindDelete)); err != nil {
			t.Fatal(err)
		}
		es := dialogueJobRow(t, s, "r_es_del", DialogueJobKindDelete, DialogueTargetES)
		fake := &fakeDialogueESIndex{}
		out := newDialogueESProcessor(s, fake).Process(ctx, []*DialogueIndexJob{es})
		if out[0].Class != "" || len(fake.deletes) != 1 || fake.deletes[0] != "r_es_del" {
			t.Fatalf("delete = %+v deletes=%v", out[0], fake.deletes)
		}
	})

	t.Run("外部瞬时失败带出分类", func(t *testing.T) {
		seedChunk(t, "r_es_fail", "正文")
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_es_fail", DialogueJobKindUpsert)); err != nil {
			t.Fatal(err)
		}
		es := dialogueJobRow(t, s, "r_es_fail", DialogueJobKindUpsert, DialogueTargetES)
		fake := &fakeDialogueESIndex{upsertErr: &dialogueClientError{Class: dlgErrESUnavailable}}
		out := newDialogueESProcessor(s, fake).Process(ctx, []*DialogueIndexJob{es})
		if out[0].Class != dlgErrESUnavailable || out[0].Permanent {
			t.Fatalf("outcome = %+v, want es_unavailable 瞬时", out[0])
		}
	})
}

// ── Milvus 消费者（fake 索引 + fake embedder）────────────────────────────────

func TestDialogueMilvusProcessor(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	seedChunk := func(t *testing.T, runID, passage string) {
		t.Helper()
		if err := s.db.Exec(
			`INSERT INTO dialogue_chunks (run_id, session_id, tenant_id, user_id, workspace, passage, runes, truncated, indexed_at)
			 VALUES (?, 's_p', ?, ?, '/tmp/ws-test', ?, ?, false, now())
			 ON CONFLICT (run_id) DO UPDATE SET passage = EXCLUDED.passage`,
			runID, testOwner.TenantID, testOwner.UserID, passage, len([]rune(passage))).Error; err != nil {
			t.Fatal(err)
		}
	}
	milCfg := dialogueEmbeddingConfig{BatchSize: 2, MaxInputTokens: 100}
	claim := func(t *testing.T, runID, kind, target string) *DialogueIndexJob {
		t.Helper()
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec(runID, kind)); err != nil {
			t.Fatal(err)
		}
		row := dialogueJobRow(t, s, runID, kind, target)
		jobs, err := s.claimDialogueJobsByIDs(ctx, []string{row.ID}, dialogueJobLease)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("认领 %s = %d err=%v", row.ID, len(jobs), err)
		}
		return jobs[0]
	}

	t.Run("批量upsert一次外部调用", func(t *testing.T) {
		seedChunk(t, "r_mv1", "向量一 key=sk-proj-AbCdEfGhIjKlMnOpQrSt")
		seedChunk(t, "r_mv2", "向量二")
		jobs := []*DialogueIndexJob{
			claim(t, "r_mv1", DialogueJobKindUpsert, DialogueTargetMilvus),
			claim(t, "r_mv2", DialogueJobKindUpsert, DialogueTargetMilvus),
		}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		embed := &fakeDialogueEmbedder{dim: 4}
		out := newDialogueMilvusProcessor(s, fakeMil, embed, fakeDialogueCounter{def: 2}, milCfg).Process(ctx, jobs)
		for i, oc := range out {
			if oc.Class != "" {
				t.Fatalf("out[%d] = %+v, want 成功", i, oc)
			}
		}
		if len(fakeMil.upserts) != 1 || len(fakeMil.upserts[0]) != 2 {
			t.Fatalf("upsert 批 = %d 次共 %d 行, want 1 次 2 行", len(fakeMil.upserts), len(fakeMil.upserts))
		}
		if len(embed.calls) != 1 || len(embed.calls[0]) != 2 {
			t.Fatalf("embedding 批 = %v, want 1 次 2 条（批次内合并）", embed.calls)
		}
		row0 := fakeMil.upserts[0][0]
		if strings.Contains(row0.Passage, "sk-proj-AbCdEfGhIjKlMnOpQrSt") {
			t.Fatalf("向量库存了未脱敏正文: %q", row0.Passage)
		}
		if row0.WorkspaceID != jobs[0].WorkspaceID || row0.TenantID != testOwner.TenantID ||
			row0.RunID != "r_mv1" || row0.IndexVersion != 1 || row0.EmbeddingModel != "test-embed-model" {
			t.Fatalf("行归属没从任务行来: %+v", row0)
		}
		if len(row0.Embedding) != 4 {
			t.Fatalf("向量维度 = %d, want 4", len(row0.Embedding))
		}
	})

	t.Run("批次上限拆分", func(t *testing.T) {
		for _, r := range []string{"r_mv3", "r_mv4", "r_mv5"} {
			seedChunk(t, r, "拆批 "+r)
		}
		jobs := []*DialogueIndexJob{
			claim(t, "r_mv3", DialogueJobKindUpsert, DialogueTargetMilvus),
			claim(t, "r_mv4", DialogueJobKindUpsert, DialogueTargetMilvus),
			claim(t, "r_mv5", DialogueJobKindUpsert, DialogueTargetMilvus),
		}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		embed := &fakeDialogueEmbedder{dim: 4}
		out := newDialogueMilvusProcessor(s, fakeMil, embed, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		for i, oc := range out {
			if oc.Class != "" {
				t.Fatalf("out[%d] = %+v", i, oc)
			}
		}
		// BatchSize=2：两次 embed 调用（2+1），一次 upsert 带 3 行。
		if len(embed.calls) != 2 || len(embed.calls[0]) != 2 || len(embed.calls[1]) != 1 {
			t.Fatalf("embedding 批 = %v, want [2,1]", embed.calls)
		}
		if len(fakeMil.upserts) != 1 || len(fakeMil.upserts[0]) != 3 {
			t.Fatalf("upsert = %d 次 %d 行", len(fakeMil.upserts), len(fakeMil.upserts[0]))
		}
	})

	t.Run("超限整条永久死信不截断", func(t *testing.T) {
		seedChunk(t, "r_big", "这条很长")
		jobs := []*DialogueIndexJob{claim(t, "r_big", DialogueJobKindUpsert, DialogueTargetMilvus)}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		embed := &fakeDialogueEmbedder{dim: 4}
		counter := fakeDialogueCounter{per: map[string]int{"这条很长": 9999}, def: 1}
		out := newDialogueMilvusProcessor(s, fakeMil, embed, counter, milCfg).Process(ctx, jobs)
		if out[0].Class != dlgErrEmbedOverLimit || !out[0].Permanent {
			t.Fatalf("outcome = %+v, want embedding_over_limit 永久", out[0])
		}
		if len(embed.calls) != 0 || len(fakeMil.upserts) != 0 {
			t.Fatalf("超限文本仍然被送出: embed=%d upsert=%d", len(embed.calls), len(fakeMil.upserts))
		}
	})

	t.Run("embed失败只回退自己的组", func(t *testing.T) {
		seedChunk(t, "r_ef1", "失败组")
		seedChunk(t, "r_ef2", "成功组")
		jobs := []*DialogueIndexJob{
			claim(t, "r_ef1", DialogueJobKindUpsert, DialogueTargetMilvus),
			claim(t, "r_ef2", DialogueJobKindUpsert, DialogueTargetMilvus),
		}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		embed := &fakeDialogueEmbedder{dim: 4, err: &dialogueClientError{Class: dlgErrEmbedUnavailable}}
		out := newDialogueMilvusProcessor(s, fakeMil, embed, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		if out[0].Class != dlgErrEmbedUnavailable || out[0].Permanent {
			t.Fatalf("out[0] = %+v, want embedding_unavailable 瞬时", out[0])
		}
		if out[1].Class != dlgErrEmbedUnavailable {
			t.Fatalf("out[1] = %+v（同组一起回退）", out[1])
		}
		if len(fakeMil.upserts) != 0 {
			t.Fatal("embed 全挂还写了外部")
		}
	})

	t.Run("move不重新embedding", func(t *testing.T) {
		seedChunk(t, "r_mvm", "要搬的正文")
		oldVec := []float32{1, 2, 3, 4}
		jobs := []*DialogueIndexJob{claim(t, "r_mvm", DialogueJobKindMove, DialogueTargetMilvus)}
		jobs[0].WorkspaceID = "ws_new" // 移到新项目
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{"r_mvm": oldVec}}
		embed := &fakeDialogueEmbedder{dim: 4}
		out := newDialogueMilvusProcessor(s, fakeMil, embed, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		if out[0].Class != "" {
			t.Fatalf("outcome = %+v", out[0])
		}
		if len(embed.calls) != 0 {
			t.Fatalf("move 重新 embedding 了: %v", embed.calls)
		}
		if len(fakeMil.upserts) != 1 || len(fakeMil.upserts[0]) != 1 {
			t.Fatalf("move upsert = %v", fakeMil.upserts)
		}
		row := fakeMil.upserts[0][0]
		if fmt.Sprint(row.Embedding) != fmt.Sprint(oldVec) {
			t.Fatalf("move 没复用原向量: %v", row.Embedding)
		}
		if row.WorkspaceID != "ws_new" {
			t.Fatalf("move 行归属 = %s, want ws_new", row.WorkspaceID)
		}
	})

	t.Run("move缺向量直接完成", func(t *testing.T) {
		jobs := []*DialogueIndexJob{claim(t, "r_mvg", DialogueJobKindMove, DialogueTargetMilvus)}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		out := newDialogueMilvusProcessor(s, fakeMil, &fakeDialogueEmbedder{dim: 4}, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		if out[0].Class != "" || len(fakeMil.upserts) != 0 {
			t.Fatalf("move 缺向量 = %+v upserts=%d", out[0], len(fakeMil.upserts))
		}
	})

	t.Run("upsert缺块收敛删除", func(t *testing.T) {
		jobs := []*DialogueIndexJob{claim(t, "r_mvgone", DialogueJobKindUpsert, DialogueTargetMilvus)}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		out := newDialogueMilvusProcessor(s, fakeMil, &fakeDialogueEmbedder{dim: 4}, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		if out[0].Class != "" {
			t.Fatalf("outcome = %+v", out[0])
		}
		if len(fakeMil.upserts) != 0 || len(fakeMil.deletes) != 1 || fakeMil.deletes[0][0] != "r_mvgone" {
			t.Fatalf("收敛删除 = upserts:%d deletes:%v", len(fakeMil.upserts), fakeMil.deletes)
		}
	})

	t.Run("delete批量删除", func(t *testing.T) {
		jobs := []*DialogueIndexJob{
			claim(t, "r_dl1", DialogueJobKindDelete, DialogueTargetMilvus),
			claim(t, "r_dl2", DialogueJobKindDelete, DialogueTargetMilvus),
		}
		fakeMil := &fakeDialogueMilvusIndex{vectors: map[string][]float32{}}
		out := newDialogueMilvusProcessor(s, fakeMil, &fakeDialogueEmbedder{dim: 4}, fakeDialogueCounter{def: 1}, milCfg).Process(ctx, jobs)
		if out[0].Class != "" || out[1].Class != "" {
			t.Fatalf("outcomes = %+v", out)
		}
		if len(fakeMil.deletes) != 1 || len(fakeMil.deletes[0]) != 2 {
			t.Fatalf("批量删 = %v", fakeMil.deletes)
		}
	})
}

// ── 消费者驱动（先持久化后提交、重复消息、跨组消息）─────────────────────────

// statusProbeProcessor 采样 Process 时刻任务行的状态（先持久化后提交的断言探针）。
type statusProbeProcessor struct {
	inner dialogueBatchProcessor
	store *Store
	mu    sync.Mutex
	seen  map[string]string
}

func (p *statusProbeProcessor) Target() string { return p.inner.Target() }

func (p *statusProbeProcessor) Process(ctx context.Context, jobs []*DialogueIndexJob) []dialogueJobOutcome {
	p.mu.Lock()
	p.seen = map[string]string{}
	p.mu.Unlock()
	for _, j := range jobs {
		row, err := p.store.getDialogueJob(context.Background(), j.ID)
		if err == nil && row != nil {
			p.mu.Lock()
			p.seen[j.ID] = row.Status
			p.mu.Unlock()
		}
	}
	return p.inner.Process(ctx, jobs)
}

func (p *statusProbeProcessor) snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.seen))
	for k, v := range p.seen {
		out[k] = v
	}
	return out
}

func TestDialogueConsumerDrive(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	// 造一场真 run：chunk + 两条任务（es/milvus）。
	sid, rid := SessionID("s_e2e"), RunID("r_e2e")
	s.mustAcquire(t, sid, rid, "m_e2e", "端到端的一场 5433")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun: ok=%v err=%v", ok, err)
	}
	if n, err := s.indexDialogueBatch(ctx); err != nil || n != 1 {
		t.Fatalf("indexDialogueBatch = %d err=%v", n, err)
	}
	es := dialogueJobRow(t, s, string(rid), DialogueJobKindUpsert, DialogueTargetES)

	// relay 投递（2 条消息：es+milvus）。
	q := newFakeDialogueQueue()
	relay := newDialogueOutboxRelay(s, q)
	if n, err := relay.relayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("relay = %d err=%v", n, err)
	}

	t.Run("先持久化后提交与重复消息", func(t *testing.T) {
		fakeES := &fakeDialogueESIndex{}
		probe := &statusProbeProcessor{inner: newDialogueESProcessor(s, fakeES), store: s}
		cons := newDialogueConsumer(s, q, probe)

		// 手动驱动一轮 ConsumeBatch（fake 队列）：handler 里断言「返回时任务已终态」，
		// commit 记录在 handler 返回之后——先持久化、后提交的顺序就有了机械证据。
		var persistedBeforeCommit bool
		handler := func(hctx context.Context, msgs []dialogueMessage) error {
			herr := cons.handleBatch(hctx, msgs)
			row, _ := s.getDialogueJobByEvent(hctx, es.EventID, DialogueTargetES)
			persistedBeforeCommit = row != nil && row.Status == DialogueJobStatusDone
			return herr
		}
		if err := q.ConsumeBatch(ctx, handler); err != nil {
			t.Fatalf("ConsumeBatch: %v", err)
		}
		if !persistedBeforeCommit {
			t.Fatal("handler 返回时任务还不是 done（先提交后持久化？）")
		}
		seen := probe.snapshot()
		if seen[es.ID] != DialogueJobStatusRunning {
			t.Fatalf("Process 时刻任务状态 = %q, want running（结局写回发生在处理之后）", seen[es.ID])
		}
		// 同批两份消息只有 ES 一份生效（milvus 是另一组的）：外部写 1 次。
		if len(fakeES.upserts) != 1 {
			t.Fatalf("外部 upsert = %d 次, want 1", len(fakeES.upserts))
		}
		// commit 在 handler 之后（fake 的 ops 顺序）。
		ops := q.Ops()
		found := false
		for _, op := range ops {
			if op == "commit:2" {
				found = true
			}
		}
		if !found {
			t.Fatalf("ops = %v（缺 commit:2）", ops)
		}

		// 重复投递同一事件：认领条件挡住，外部不再写。
		if perr := q.Publish(ctx, dialogueMessage{
			EventID: es.EventID, RunID: string(rid), Target: DialogueTargetES,
			Kind: DialogueJobKindUpsert, IndexVersion: 1, Key: string(rid),
		}); perr != nil {
			t.Fatal(perr)
		}
		if err := q.ConsumeBatch(ctx, cons.handleBatch); err != nil {
			t.Fatalf("重投 ConsumeBatch: %v", err)
		}
		if len(fakeES.upserts) != 1 {
			t.Fatalf("重复消息多写了外部: %d 次", len(fakeES.upserts))
		}
	})

	t.Run("任务行不存在跳过不重建", func(t *testing.T) {
		fakeES := &fakeDialogueESIndex{}
		cons := newDialogueConsumer(s, q, newDialogueESProcessor(s, fakeES))
		if err := cons.handleBatch(ctx, []dialogueMessage{{
			EventID: "evt_不存在", RunID: "r_缺", Target: DialogueTargetES,
			Kind: DialogueJobKindUpsert, IndexVersion: 1,
		}}); err != nil {
			t.Fatalf("缺失任务的消息应跳过不报错: %v", err)
		}
		if len(fakeES.upserts) != 0 {
			t.Fatal("缺失任务被写了外部")
		}
	})

	t.Run("跨组消息不处理", func(t *testing.T) {
		fakeES := &fakeDialogueESIndex{}
		cons := newDialogueConsumer(s, q, newDialogueESProcessor(s, fakeES))
		if err := cons.handleBatch(ctx, []dialogueMessage{{
			EventID: es.EventID, RunID: string(rid), Target: DialogueTargetMilvus,
			Kind: DialogueJobKindUpsert, IndexVersion: 1,
		}}); err != nil {
			t.Fatalf("跨组消息: %v", err)
		}
		if len(fakeES.upserts) != 0 {
			t.Fatal("ES 组处理了 milvus 的消息")
		}
	})
}

// TestDialogueConsumerWriteBackDrivesJobState：processor 的结局判定驱动写回——
// 瞬时失败回 pending（下轮对账重试）、永久失败进死信、成功翻 done。
func TestDialogueConsumerWriteBackDrivesJobState(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)

	mk := func(runID string) (*Store, *DialogueIndexJob, *fakeDialogueESIndex, *dialogueConsumer) {
		t.Helper()
		if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec(runID, DialogueJobKindUpsert)); err != nil {
			t.Fatal(err)
		}
		// 播种 chunk：upsert 任务要有块可写——没块时处理器按契约走「收敛删除」，
		// 这组子测要驱动的是 upsert 路径的结局判定（成功/瞬时/永久/未知）。
		if err := s.db.Exec(
			`INSERT INTO dialogue_chunks (run_id, session_id, tenant_id, user_id, workspace, passage, runes, truncated, indexed_at)
			 VALUES (?, ?, ?, ?, '/tmp/ws-test', '写回测试正文', 6, false, now())
			 ON CONFLICT (run_id) DO UPDATE SET passage = EXCLUDED.passage`,
			runID, "s_"+strings.TrimPrefix(runID, "r_"), testOwner.TenantID, testOwner.UserID).Error; err != nil {
			t.Fatal(err)
		}
		row := dialogueJobRow(t, s, runID, DialogueJobKindUpsert, DialogueTargetES)
		fake := &fakeDialogueESIndex{}
		cons := newDialogueConsumer(s, newFakeDialogueQueue(), newDialogueESProcessor(s, fake))
		return s, row, fake, cons
	}

	// 成功 → done。
	_, row, fake, cons := mk("r_wb1")
	if err := cons.handleBatch(ctx, []dialogueMessage{{
		EventID: row.EventID, RunID: row.RunID, Target: DialogueTargetES,
		Kind: DialogueJobKindUpsert, IndexVersion: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if got := dialogueJobRow(t, s, "r_wb1", DialogueJobKindUpsert, DialogueTargetES); got.Status != DialogueJobStatusDone {
		t.Fatalf("成功后 status = %s", got.Status)
	}
	if len(fake.upserts) != 1 {
		t.Fatalf("外部写 = %d", len(fake.upserts))
	}

	// 瞬时失败 → pending 退避，last_error 只存机器分类。
	_, row2, _, cons2 := mk("r_wb2")
	fake2 := &fakeDialogueESIndex{upsertErr: &dialogueClientError{Class: dlgErrESUnavailable, Cause: errors.New("boom")}}
	cons2.proc = newDialogueESProcessor(s, fake2)
	if err := cons2.handleBatch(ctx, []dialogueMessage{{
		EventID: row2.EventID, RunID: row2.RunID, Target: DialogueTargetES,
		Kind: DialogueJobKindUpsert, IndexVersion: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	got := dialogueJobRow(t, s, "r_wb2", DialogueJobKindUpsert, DialogueTargetES)
	if got.Status != DialogueJobStatusPending || got.LastError != dlgErrESUnavailable {
		t.Fatalf("瞬时失败 = status=%s last_error=%q", got.Status, got.LastError)
	}
	if strings.Contains(got.LastError, "boom") {
		t.Fatalf("last_error 混入了底层错误细节: %q（只许机器分类）", got.LastError)
	}

	// 永久失败 → failed 死信。
	_, row3, _, cons3 := mk("r_wb3")
	fake3 := &fakeDialogueESIndex{upsertErr: &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: errors.New("400")}}
	cons3.proc = newDialogueESProcessor(s, fake3)
	if err := cons3.handleBatch(ctx, []dialogueMessage{{
		EventID: row3.EventID, RunID: row3.RunID, Target: DialogueTargetES,
		Kind: DialogueJobKindUpsert, IndexVersion: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	got3 := dialogueJobRow(t, s, "r_wb3", DialogueJobKindUpsert, DialogueTargetES)
	if got3.Status != DialogueJobStatusFailed || got3.LastError != dlgErrESPermanent {
		t.Fatalf("永久失败 = status=%s last_error=%q", got3.Status, got3.LastError)
	}

	// 未知错误 → 按瞬时退避（internal_transient），不轻易死信。
	_, row4, _, cons4 := mk("r_wb4")
	fake4 := &fakeDialogueESIndex{upsertErr: errors.New("意外错误")}
	cons4.proc = newDialogueESProcessor(s, fake4)
	if err := cons4.handleBatch(ctx, []dialogueMessage{{
		EventID: row4.EventID, RunID: row4.RunID, Target: DialogueTargetES,
		Kind: DialogueJobKindUpsert, IndexVersion: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	got4 := dialogueJobRow(t, s, "r_wb4", DialogueJobKindUpsert, DialogueTargetES)
	if got4.Status != DialogueJobStatusPending || got4.LastError != dialogueErrInternalTransient {
		t.Fatalf("未知错误 = status=%s last_error=%q", got4.Status, got4.LastError)
	}
}

// TestDialogueReconcilerRedeliversDue：对账器把到期任务补投成叫醒消息（只叫醒，
// 能不能处理由认领条件裁决）。
func TestDialogueReconcilerRedeliversDue(t *testing.T) {
	ctx := context.Background()
	s := newDialogueHybridStore(t)
	if _, err := s.RegisterDialogueIndexJobs(ctx, dialogueTaskSpec("r_rc", DialogueJobKindUpsert)); err != nil {
		t.Fatal(err)
	}
	// relay 已投过一轮（published_at 写上），任务还没人认领。
	q := newFakeDialogueQueue()
	if n, err := newDialogueOutboxRelay(s, q).relayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("relay = %d err=%v", n, err)
	}
	// 认领后崩了（租约过期）→ 对账器捞起补投。
	es := dialogueJobRow(t, s, "r_rc", DialogueJobKindUpsert, DialogueTargetES)
	if _, err := s.claimDialogueJobsByIDs(ctx, []string{es.ID}, dialogueJobLease); err != nil {
		t.Fatal(err)
	}
	forceDialogueJobDue(t, s, es.ID)

	cons := newDialogueConsumer(s, q, newDialogueESProcessor(s, &fakeDialogueESIndex{}))
	n, err := cons.reconcileOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("对账补投 = %d err=%v, want 1（只补本 target）", n, err)
	}
	pub := q.Published()
	if len(pub) != 3 { // relay 的 2 条 + 对账补投的 1 条（es）
		t.Fatalf("队列 = %d 条, want 3", len(pub))
	}
	last := pub[len(pub)-1]
	if last.EventID != es.EventID || last.Target != DialogueTargetES || last.Key != es.RunID {
		t.Fatalf("补投消息 = %+v", last)
	}
}
