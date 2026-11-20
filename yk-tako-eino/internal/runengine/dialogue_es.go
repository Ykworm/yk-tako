package runengine

// 本文件是历史对话词法索引的 ES 客户端（doc/84 §5.1）：mapping 创建、幂等 upsert/delete、
// BM25 查询与启动预检用的 _analyze。走标准库 HTTP + Basic auth（施工包 §2.3：ES 不引第三方库）。
//
// 实例是共享 demo（P0 实测 9.5.0 + analysis-ik）：**只创建 tako_dialogue_ 前缀的索引**，
// 绝不 delete/close/update 任何既有索引——索引名前缀在配置解析处闸（dialogue_retrieval.go）。
//
// 错误分类（doc/84 §6.2）：网络/429/5xx = 瞬时（es_unavailable，走退避）；
// 其余 4xx = 永久（es_permanent，死信）。index_not_found 自愈一次（EnsureIndex 后重试）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// dialogueESTimeout 是单次 ES REST 调用的超时（P0 实测共享实例冷查询 <1s，15s 留足余量）。
const dialogueESTimeout = 15 * time.Second

// dialogueLexHit 是词法检索的一条命中（doc/84 §5.4：run_id/score/index_version +
// 脱敏 passage + 来源会话与时间元数据）。
type dialogueLexHit struct {
	RunID        string
	Score        float64
	IndexVersion int64
	SessionID    string
	Passage      string
	FinishedAt   *time.Time
	IndexedAt    *time.Time
}

// dialogueESDoc 是 ES 文档体（doc/84 §5.1 字段词表）。_id = run_id。
type dialogueESDoc struct {
	RunID          string     `json:"run_id"`
	SessionID      string     `json:"session_id"`
	TenantID       string     `json:"tenant_id"`
	UserID         string     `json:"user_id"`
	WorkspaceID    string     `json:"workspace_id"`
	Passage        string     `json:"passage"`
	IndexVersion   int64      `json:"index_version"`
	EmbeddingModel string     `json:"embedding_model"`
	RunFinishedAt  *time.Time `json:"run_finished_at"`
	IndexedAt      time.Time  `json:"indexed_at"`
}

// dialogueESClient 是 ES REST 客户端（单实例单索引；每个引擎进程一份）。
type dialogueESClient struct {
	base    string
	user    string
	pass    string
	index   string
	hc      *http.Client
	ensured bool // EnsureIndex 成功后置位（进程内缓存；外部删索引的窗口自愈靠 404 重试路径）
}

// newDialogueESClient 建客户端。URL/索引名已在配置解析处校验（前缀闸在 dialogue_retrieval.go）。
func newDialogueESClient(url, username, password, index string) *dialogueESClient {
	return &dialogueESClient{
		base:  strings.TrimRight(url, "/"),
		user:  username,
		pass:  password,
		index: index,
		hc:    &http.Client{Timeout: dialogueESTimeout},
	}
}

// dialogueESIndexMapping 是 tako_dialogue_* 的 mapping（doc/84 §5.1）：归属为 keyword，
// passage 用 ik_max_word 建索引、ik_smart 查询（IK 社区惯例搭配）。
func dialogueESIndexMapping() map[string]any {
	return map[string]any{
		"settings": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
		},
		"mappings": map[string]any{
			"properties": map[string]any{
				"run_id":          map[string]any{"type": "keyword"},
				"session_id":      map[string]any{"type": "keyword"},
				"tenant_id":       map[string]any{"type": "keyword"},
				"user_id":         map[string]any{"type": "keyword"},
				"workspace_id":    map[string]any{"type": "keyword"},
				"passage":         map[string]any{"type": "text", "analyzer": "ik_max_word", "search_analyzer": "ik_smart"},
				"index_version":   map[string]any{"type": "keyword"},
				"embedding_model": map[string]any{"type": "keyword"},
				"run_finished_at": map[string]any{"type": "date"},
				"indexed_at":      map[string]any{"type": "date"},
			},
		},
	}
}

// do 执行一次 ES REST 调用，返回状态码与响应体。网络层失败一律按瞬时分类
// （es_unavailable）；调用方对状态码再做分类。
func (c *dialogueESClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, &dialogueClientError{Class: dlgErrESUnavailable, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, &dialogueClientError{Class: dlgErrESUnavailable, Cause: err}
	}
	return resp.StatusCode, data, nil
}

// dialogueClassifyHTTPStatus 把一次非 2xx 的 HTTP 状态分类成客户端错误：
// 429/5xx = 瞬时（退避重试），其余 4xx = 永久（死信）。2xx 返回 nil。
func dialogueClassifyHTTPStatus(status int, unavailableClass, permanentClass string) *dialogueClientError {
	switch {
	case status == 429 || status >= 500:
		return &dialogueClientError{Class: unavailableClass, Cause: fmt.Errorf("http %d", status)}
	case status >= 400:
		return &dialogueClientError{Class: permanentClass, Permanent: true, Cause: fmt.Errorf("http %d", status)}
	}
	return nil
}

// EnsureIndex 幂等创建索引（PUT settings+mappings）。索引已存在时 ES 回
// resource_already_exists_exception（400）——视为成功。
func (c *dialogueESClient) EnsureIndex(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodPut, "/"+c.index, dialogueESIndexMapping())
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		c.ensured = true
		return nil
	}
	if strings.Contains(string(body), "resource_already_exists_exception") {
		c.ensured = true
		return nil
	}
	if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
		return ce
	}
	return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
		Cause: fmt.Errorf("ensure index http %d: %s", status, clipMemoryText(string(body), 200))}
}

// ensureOnce 统一处理「索引不在」的自愈：404 且 body 指向 index_not_found →
// EnsureIndex 一次并返回 true（调用方重试原操作一次）。
func (c *dialogueESClient) ensureOnce(ctx context.Context, status int, body []byte) (bool, error) {
	if status != http.StatusNotFound || !strings.Contains(string(body), "index_not_found_exception") {
		return false, nil
	}
	if err := c.EnsureIndex(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Upsert 幂等写一个文档（PUT _doc/{run_id} 整文档覆盖）；索引缺失自愈一次。
func (c *dialogueESClient) Upsert(ctx context.Context, doc dialogueESDoc) error {
	status, body, err := c.do(ctx, http.MethodPut, "/"+c.index+"/_doc/"+doc.RunID, doc)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if retried, rerr := c.ensureOnce(ctx, status, body); rerr != nil {
		return rerr
	} else if retried {
		status, body, err = c.do(ctx, http.MethodPut, "/"+c.index+"/_doc/"+doc.RunID, doc)
		if err != nil {
			return err
		}
		if status >= 200 && status < 300 {
			return nil
		}
	}
	if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
		return ce
	}
	return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
		Cause: fmt.Errorf("upsert http %d: %s", status, clipMemoryText(string(body), 200))}
}

// Delete 删除一个文档；result=deleted 与 not_found 都算「确认不存在」
// （doc/84 §5.4：delete 后确认不存在）。索引本身不在（404 index_not_found）也算 gone。
func (c *dialogueESClient) Delete(ctx context.Context, runID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, "/"+c.index+"/_doc/"+runID, nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil // deleted 或 not_found：都已确认不存在
	}
	if status == http.StatusNotFound && strings.Contains(string(body), "index_not_found_exception") {
		return nil // 索引都不在，文档必然不在
	}
	if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
		return ce
	}
	return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
		Cause: fmt.Errorf("delete http %d: %s", status, clipMemoryText(string(body), 200))}
}

// dialogueESSearchBody 组查询体：bool filter（归属等值 + 活跃代次）+ match（BM25），
// size=topK。index_version 在 mapping 里是 keyword，term 过滤用十进制串。
// indexVersion<=0 不过代次（仅限诊断用例；召回链永远带指针的真源值）。
func dialogueESSearchBody(query, tenantID, workspaceID, userID string, indexVersion int64, topK int) map[string]any {
	filter := []any{
		map[string]any{"term": map[string]any{"tenant_id": tenantID}},
		map[string]any{"term": map[string]any{"workspace_id": workspaceID}},
	}
	if userID != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"user_id": userID}})
	}
	if indexVersion > 0 {
		filter = append(filter, map[string]any{"term": map[string]any{
			"index_version": strconv.FormatInt(indexVersion, 10)}})
	}
	return map[string]any{
		"size":             topK,
		"track_total_hits": false,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": filter,
				"must":   []any{map[string]any{"match": map[string]any{"passage": query}}},
			},
		},
	}
}

// Search BM25 词法检索（doc/84 §5.4：最多 topK 条候选；返回 run_id/score/index_version/
// 脱敏 passage 与来源元数据）。索引缺失自愈一次后仍失败才报错。
func (c *dialogueESClient) Search(ctx context.Context, query, tenantID, workspaceID, userID string, indexVersion int64, topK int) ([]dialogueLexHit, error) {
	body := dialogueESSearchBody(query, tenantID, workspaceID, userID, indexVersion, topK)
	status, respBody, err := c.do(ctx, http.MethodPost, "/"+c.index+"/_search", body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		if retried, rerr := c.ensureOnce(ctx, status, respBody); rerr != nil {
			return nil, rerr
		} else if retried {
			status, respBody, err = c.do(ctx, http.MethodPost, "/"+c.index+"/_search", body)
			if err != nil {
				return nil, err
			}
		}
		if status < 200 || status >= 300 {
			if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
				return nil, ce
			}
			return nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
				Cause: fmt.Errorf("search http %d: %s", status, clipMemoryText(string(respBody), 200))}
		}
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				ID     string        `json:"_id"`
				Score  float64       `json:"_score"`
				Source dialogueESDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// 2xx 但响应解析不动：非法响应 = 永久错误（死信，不退避重试同一形状）。
		return nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
	}
	hits := make([]dialogueLexHit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		runID := h.Source.RunID
		if runID == "" {
			runID = h.ID
		}
		hit := dialogueLexHit{
			RunID:        runID,
			Score:        h.Score,
			IndexVersion: h.Source.IndexVersion,
			SessionID:    h.Source.SessionID,
			Passage:      h.Source.Passage,
			FinishedAt:   h.Source.RunFinishedAt,
		}
		if !h.Source.IndexedAt.IsZero() {
			t := h.Source.IndexedAt
			hit.IndexedAt = &t
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// Analyze 跑一次 _analyze 并返回 token 列表（启动预检用，doc/84 §5.1）：ik 插件缺失
// 或分析器名不识别都会在这里失败（启动即退，不静默换语义）。
func (c *dialogueESClient) Analyze(ctx context.Context, analyzer, text string) ([]string, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/"+c.index+"/_analyze",
		map[string]any{"analyzer": analyzer, "text": text})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
			return nil, ce
		}
		return nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
			Cause: fmt.Errorf("analyze %s http %d: %s", analyzer, status, clipMemoryText(string(body), 200))}
	}
	var parsed struct {
		Tokens []struct {
			Token string `json:"token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
	}
	tokens := make([]string, 0, len(parsed.Tokens))
	for _, t := range parsed.Tokens {
		tokens = append(tokens, t.Token)
	}
	return tokens, nil
}

// Healthy 词法臂健康探测（status 诊断端点用，doc/84 §6.3）：ES 可达**且索引在**才算
// 健康——索引缺失（_count 404 index_not_found）与网络不可达都报不健康，不虚构状态。
func (c *dialogueESClient) Healthy(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, "/"+c.index+"/_count", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound && strings.Contains(string(body), "index_not_found_exception") {
		return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
			Cause: fmt.Errorf("index %s 不存在", c.index)}
	}
	if status < 200 || status >= 300 {
		if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
			return ce
		}
		return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
			Cause: fmt.Errorf("count http %d", status)}
	}
	return nil
}

// Refresh 强制刷新索引（对账计数与重建校验前用，保证刚写入的文档可见）。
func (c *dialogueESClient) Refresh(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodPost, "/"+c.index+"/_refresh", nil)
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
		return ce
	}
	return &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
		Cause: fmt.Errorf("refresh http %d: %s", status, clipMemoryText(string(body), 200))}
}

// CountDocs 计数当前索引文档数（_count）。索引不存在（404 index_not_found）返回 0：
// 对账语义下「索引不在」=「文档都不在」（缺失会被补投自愈；EnsureIndex 在消费者写入路径自愈建索引）。
func (c *dialogueESClient) CountDocs(ctx context.Context) (int64, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/"+c.index+"/_count", nil)
	if err != nil {
		return 0, err
	}
	if status == http.StatusNotFound && strings.Contains(string(body), "index_not_found_exception") {
		return 0, nil
	}
	if status < 200 || status >= 300 {
		if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
			return 0, ce
		}
		return 0, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
			Cause: fmt.Errorf("count http %d: %s", status, clipMemoryText(string(body), 200))}
	}
	var parsed struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
	}
	return parsed.Count, nil
}

// ListAllRunIDs 枚举当前索引全部 run_id（对账用：按 run_id keyword 排序 + search_after
// keyset 分页；不能用 _id 排序——ES 默认不给 _id 开 fielddata）。索引不存在返回空集。
// 超过 maxRuns 截断（truncated=true），防外部实例被别的写入灌爆时对账失控。
func (c *dialogueESClient) ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	const page = dialogueReconcileListPage
	out := make(map[string]bool)
	after := ""
	for {
		body := map[string]any{
			"size":             page,
			"query":            map[string]any{"match_all": map[string]any{}},
			"_source":          false,
			"track_total_hits": false,
			"sort":             []any{map[string]any{"run_id": "asc"}},
		}
		if after != "" {
			body["search_after"] = []any{after}
		}
		status, respBody, err := c.do(ctx, http.MethodPost, "/"+c.index+"/_search", body)
		if err != nil {
			return nil, false, err
		}
		if status == http.StatusNotFound && strings.Contains(string(respBody), "index_not_found_exception") {
			return out, false, nil
		}
		if status < 200 || status >= 300 {
			if ce := dialogueClassifyHTTPStatus(status, dlgErrESUnavailable, dlgErrESPermanent); ce != nil {
				return nil, false, ce
			}
			return nil, false, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true,
				Cause: fmt.Errorf("list http %d: %s", status, clipMemoryText(string(respBody), 200))}
		}
		var parsed struct {
			Hits struct {
				Hits []struct {
					ID   string `json:"_id"`
					Sort []any  `json:"sort"`
				} `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, false, &dialogueClientError{Class: dlgErrESPermanent, Permanent: true, Cause: err}
		}
		n := 0
		for _, h := range parsed.Hits.Hits {
			if h.ID != "" {
				out[h.ID] = true
				n++
			}
			if len(h.Sort) > 0 {
				if s, ok := h.Sort[0].(string); ok {
					after = s
				}
			}
		}
		if n < page || len(out) >= maxRuns {
			break
		}
	}
	return out, len(out) >= maxRuns, nil
}
