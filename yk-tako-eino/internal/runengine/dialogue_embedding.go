package runengine

// 本文件是历史对话向量检索的 embedding 客户端（doc/84 §5.2）：OpenAI 兼容 /embeddings
// 的批量调用、响应校验与维度指纹。走标准库 HTTP（施工包 §2.3：不引第三方库）。
//
// 维度指纹（doc/84 §6.2「以真实响应为准并写入 collection 指纹」）：
//   - 启动预检 ProbeDim 打一次最小真实调用，把返回维度**锁进进程**；
//   - 之后每次 EmbedBatch 校验每条向量长度 == 锁定维度，不符 = 永久错误
//     （119004 类，模型或维度变了，重试没有意义）。
//
// 超限（P0 实测：provider 硬限 131072 tokens **不自查错**）：
// 单块 token 数由调用方用 TokenCounter 预检（dialogueEmbedGroups），超限块整条报
// 永久错误——**只拒绝，不截断**（静默截断会让检索语义悄悄变化）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// embedding 的节奏（doc/84 §6.2：批量、限流、背压）。批次与单次请求 token 上限可被
// 环境变量覆盖（TAKO_DIALOGUE_EMBEDDING_BATCH_SIZE / _MAX_INPUT_TOKENS），
// 超时同理（_TIMEOUT）；P0 实测单块上限 6000 tokens 是安全值（上限 131072 不自查错）。
const (
	dialogueEmbeddingBatchSize      = 8
	dialogueEmbeddingMaxInputTokens = 6000
)

// dialogueEmbeddingConfig 是 embedding 的连接口径（凭据只从环境读）。
type dialogueEmbeddingConfig struct {
	BaseURL        string
	APIKey         string
	Model          string
	Timeout        time.Duration
	BatchSize      int
	MaxInputTokens int
}

// dialogueEmbedder 是 OpenAI 兼容 embedding 客户端。
type dialogueEmbedder struct {
	cfg dialogueEmbeddingConfig
	hc  *http.Client

	mu  sync.Mutex
	dim int // 维度指纹：ProbeDim 首次真实调用锁定；0 = 未锁定
}

func newDialogueEmbedder(cfg dialogueEmbeddingConfig) *dialogueEmbedder {
	return &dialogueEmbedder{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.Timeout},
	}
}

// Dim 返回已锁定的维度（0 = 未锁定）。
func (e *dialogueEmbedder) Dim() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dim
}

// lockDim 锁定维度指纹；与已锁值不符报永久错（模型/维度变了必须显式重建索引，
// 不能让同一 collection 里混写两种维度）。
func (e *dialogueEmbedder) lockDim(d int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dim == 0 {
		e.dim = d
		return nil
	}
	if e.dim != d {
		return &dialogueClientError{Class: dlgErrEmbedDimMismatch, Permanent: true,
			Cause: fmt.Errorf("embedding 维度指纹 %d ≠ 本次响应维度 %d", e.dim, d)}
	}
	return nil
}

// dialogueEmbeddingRequest/Response 是 OpenAI 兼容 /embeddings 的请求响应形状
// （P0 实测：qwen3.7-text-embedding，dim=1024，data[i].index 与输入顺序对应）。
type dialogueEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type dialogueEmbeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// EmbedBatch 批量取向量：返回与 texts 等长同序的向量切片。
//
// 校验链（任一不过 = 永久错误，死信不重试）：
//   - data 长度 == 输入长度且 index 顺序对应（乱序/缺条 = 非法响应）；
//   - 每条向量维度 == 锁定指纹（首次调用锁定）。
//
// 非法响应分两类：网络/429/5xx = 瞬时（embedding_unavailable，退避）；
// 2xx 但形状不对 / 其它 4xx = 永久。
func (e *dialogueEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > e.cfg.BatchSize {
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
			Cause: fmt.Errorf("一次请求 %d 条超过批次上限 %d（调用方必须先分批）", len(texts), e.cfg.BatchSize)}
	}
	payload, err := json.Marshal(dialogueEmbeddingRequest{Model: e.cfg.Model, Input: texts})
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true, Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(e.cfg.BaseURL, "/")+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true, Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrEmbedUnavailable, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrEmbedUnavailable, Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if ce := dialogueClassifyHTTPStatus(resp.StatusCode, dlgErrEmbedUnavailable, dlgErrEmbedBadResponse); ce != nil {
			return nil, ce
		}
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
			Cause: fmt.Errorf("embeddings http %d", resp.StatusCode)}
	}
	var parsed dialogueEmbeddingResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true, Cause: err}
	}
	if len(parsed.Data) != len(texts) {
		return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
			Cause: fmt.Errorf("embeddings 返回 %d 条，输入 %d 条", len(parsed.Data), len(texts))}
	}
	out := make([][]float32, len(texts))
	for i, d := range parsed.Data {
		if d.Index != i {
			return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
				Cause: fmt.Errorf("embeddings data[%d].index=%d 与顺序不符", i, d.Index)}
		}
		if len(d.Embedding) == 0 {
			return nil, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
				Cause: fmt.Errorf("embeddings data[%d] 空向量", i)}
		}
		if err := e.lockDim(len(d.Embedding)); err != nil {
			return nil, err
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// ProbeDim 启动预检：打一次最小真实调用，锁定维度指纹并返回（doc/84 §6.2）。
// **每次都是真实请求**，不用已锁指纹短路——预检的价值就在「服务真的在、维度真的对」，
// 缓存值回答不了这两问（施工包 §4：启动预检必须真实完成最小 embedding）。
// 调用失败原样上抛——hybrid 装配失败即启动失败（119003/119004 类），不静默降级。
func (e *dialogueEmbedder) ProbeDim(ctx context.Context) (int, error) {
	vecs, err := e.EmbedBatch(ctx, []string{"维度探针"})
	if err != nil {
		return 0, err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return 0, &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
			Cause: fmt.Errorf("维度探针返回 %d 条向量", len(vecs))}
	}
	return len(vecs[0]), nil
}

// dialogueEmbedGroups 把一批文本按「单请求 ≤BatchSize 条、累计 token ≤MaxInputTokens」
// 切成若干组（doc/84 §6.2：批量+限流+背压；拆分是分批不是截断）。
// 超过单块上限的文本出现在 overLimit 里——调用方对它们整条报永久错误（embedding_over_limit），
// **不截断**（P0 实测：provider 对超限不自查错，必须应用层闸）。
func dialogueEmbedGroups(counter TokenCounter, texts []string, batchSize, maxTokens int) (groups [][]int, overLimit []int) {
	type acc struct {
		items []int
		toks  int
	}
	cur := &acc{}
	flush := func() {
		if len(cur.items) > 0 {
			groups = append(groups, cur.items)
			cur = &acc{}
		}
	}
	for i, t := range texts {
		n := 0
		if counter != nil {
			n = counter.Count(t)
		}
		if n > maxTokens {
			overLimit = append(overLimit, i)
			continue
		}
		if len(cur.items) >= batchSize || cur.toks+n > maxTokens {
			flush()
		}
		cur.items = append(cur.items, i)
		cur.toks += n
	}
	flush()
	return groups, overLimit
}
