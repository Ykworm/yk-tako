package runengine

// 本文件是 hybrid 检索的两个 dev 诊断端点（doc/84 §8.3；施工包 §6.2/§6.3）：
//
//	POST /api/dev/dialogue-retrieval/query   一次完整查询的诊断视图（词法/向量/合并三段）
//	GET  /api/dev/dialogue-retrieval/status  指针、两臂健康、覆盖水位与消费 lag
//
// 仅 -dev 且登录（路由在 server.go 的 dev 块 + authn）。全部 {code,message,data} 信封，
// 失败 data:null；不写底层堆栈、正文、凭据。响应 data 用结构体保键序（gin.H 是 map，
// 字节序会按字典序排，钉不住契约）。
//
// query 的守卫次序：119005（未启用 hybrid）→ 111001（JSON 坏）→ 111002（query 空 /
// limit 非法）→ 114001（项目不可见）→ 执行（臂失败按 119001/119002/119003/119004 映射）。

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// dialogueQueryLimitDefault / Max 是 dev query 的 limit 口径（施工包 §6.2：缺省 10、最大 20）。
const (
	dialogueQueryLimitDefault = 10
	dialogueQueryLimitMax     = 20
)

// dialogueQueryRequest 是 POST query 的请求体。limit 用 RawMessage 自己解析：
// 「limit 非法」按施工包沿用 111002，而不是 gin 绑定失败的 111001。
type dialogueQueryRequest struct {
	Query       string          `json:"query"`
	WorkspaceID string          `json:"workspaceId"`
	Limit       json.RawMessage `json:"limit"`
}

// 响应 data 结构（键序即字节序，契约机器真源按它钉）。
type dialogueQueryData struct {
	Mode    string                    `json:"mode"`
	Lexical []dialogueQueryArmItem    `json:"lexical"`
	Vector  []dialogueQueryArmItem    `json:"vector"`
	Merged  []dialogueQueryMergedItem `json:"merged"`
}

type dialogueQueryArmItem struct {
	RunID string  `json:"runId"`
	Score float64 `json:"score"`
}

type dialogueQueryMergedItem struct {
	RunID    string   `json:"runId"`
	RRFScore float64  `json:"rrfScore"`
	Sources  []string `json:"sources"`
}

// devDialogueRetrievalQuery：一次完整 hybrid 查询的诊断视图。两臂与召回链同口径
// （同脱敏、同归属过滤、同代次过滤、同 RRF），只是把三段候选都摆出来给操作员对质。
func (s *Server) devDialogueRetrievalQuery(c *gin.Context) {
	rt := s.store.dialogueHybrid()
	if rt == nil {
		// 未启用 hybrid：端点没有可诊断的对象。409 119005（关停窗口同样按此答——
		// 这个端点在 hybrid 关闭时不存在「部分可用」形态）。
		writeEnvelope(c, http.StatusConflict, codeDialogueRetrievalOff, ErrDialogueRetrievalOff.Error(), nil)
		return
	}
	if ok, _ := s.memoryContext(c); !ok {
		return
	}
	var req dialogueQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "query 不能为空", nil)
		return
	}
	limit, ok := dialogueQueryLimit(req.Limit)
	if !ok {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "limit 必须是 1–20 的整数", nil)
		return
	}
	sc := scopeFrom(c.Request.Context())
	ws, err := s.store.VisibleWorkspace(c.Request.Context(), strings.TrimSpace(req.WorkspaceID), sc.TenantID, sc.UserID)
	if errors.Is(err, ErrWorkspaceNotFound) {
		writeEnvelope(c, http.StatusNotFound, codeWorkspaceMissing, "找不到这个项目", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}

	ptr, err := s.store.dialoguePointer(c.Request.Context())
	if err != nil {
		internal(c, err)
		return
	}
	// scope=tenant（owner/admin 预览）不按单个 user 过滤，与召回链同口径。
	userID := sc.UserID
	if sc.Scope == "tenant" {
		userID = ""
	}
	fused, err := dialogueHybridSearch(c.Request.Context(), rt, ptr, query,
		sc.TenantID, userID, ws.ID, limit)
	if err != nil {
		if status, code, msg, ok := dialogueRetrievalHTTPError(err); ok {
			writeEnvelope(c, status, code, msg, nil)
			return
		}
		internal(c, err)
		return
	}

	data := dialogueQueryData{
		Mode:    DialogueModeHybrid,
		Lexical: dialogueQueryArmList(fused, func(h dialogueHybridHit) int { return h.LexRank }, func(h dialogueHybridHit) float64 { return h.LexScore }),
		Vector:  dialogueQueryArmList(fused, func(h dialogueHybridHit) int { return h.VecRank }, func(h dialogueHybridHit) float64 { return h.VecScore }),
		Merged:  make([]dialogueQueryMergedItem, 0, len(fused)),
	}
	for _, h := range fused {
		data.Merged = append(data.Merged, dialogueQueryMergedItem{
			RunID:    h.RunID,
			RRFScore: h.RRFScore,
			Sources:  h.Sources,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", data)
}

// dialogueQueryLimit 解析 limit：缺省 10；非法（非整数 / ≤0 / >20）回 false（111002）。
func dialogueQueryLimit(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return dialogueQueryLimitDefault, true
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if n < 1 || n > dialogueQueryLimitMax {
		return 0, false
	}
	return n, true
}

// dialogueQueryArmList 从融合结果还原一个臂的名次序列（rank 升序；rank 0 = 不在该臂）。
func dialogueQueryArmList(fused []dialogueHybridHit, rankFn func(dialogueHybridHit) int, scoreFn func(dialogueHybridHit) float64) []dialogueQueryArmItem {
	ordered := make([]dialogueHybridHit, 0, len(fused))
	for _, h := range fused {
		if rankFn(h) > 0 {
			ordered = append(ordered, h)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return rankFn(ordered[i]) < rankFn(ordered[j]) })
	out := make([]dialogueQueryArmItem, 0, len(ordered))
	for _, h := range ordered {
		out = append(out, dialogueQueryArmItem{RunID: h.RunID, Score: scoreFn(h)})
	}
	return out
}

// status 响应 data 结构（键序即字节序）。非 hybrid 只回 mode 与 coverage——
// pointer/lexical/vector 三段缺席（omitempty），不虚构健康状态（施工包 §6.3）。
type dialogueStatusData struct {
	Mode     string                 `json:"mode"`
	Pointer  *dialogueStatusPointer `json:"pointer,omitempty"`
	Lexical  *dialogueStatusLexical `json:"lexical,omitempty"`
	Vector   *dialogueStatusVector  `json:"vector,omitempty"`
	Coverage dialogueStatusCoverage `json:"coverage"`
}

type dialogueStatusPointer struct {
	IndexVersion     int64  `json:"indexVersion"`
	EsIndex          string `json:"esIndex"`
	MilvusCollection string `json:"milvusCollection"`
}

type dialogueStatusLexical struct {
	Kind    string `json:"kind"`
	Index   string `json:"index"`
	Healthy bool   `json:"healthy"`
}

type dialogueStatusVector struct {
	Kind           string `json:"kind"`
	Collection     string `json:"collection"`
	Healthy        bool   `json:"healthy"`
	EmbeddingModel string `json:"embeddingModel"`
	Dimensions     int    `json:"dimensions"`
}

type dialogueStatusCoverage struct {
	Pending         int64      `json:"pending"`
	Running         int64      `json:"running"`
	Retry           int64      `json:"retry"`
	Failed          int64      `json:"failed"`
	OldestPendingAt *time.Time `json:"oldestPendingAt"`
	ConsumerLag     int64      `json:"consumerLag"`
}

// devDialogueRetrievalStatus：指针 + 两臂健康 + 覆盖水位与消费 lag。
func (s *Server) devDialogueRetrievalStatus(c *gin.Context) {
	if _, ok := s.requirePrincipal(c); !ok {
		return
	}
	ctx := c.Request.Context()
	coverage, err := s.store.DialogueCoverage(ctx)
	if err != nil {
		internal(c, err)
		return
	}
	data := dialogueStatusData{
		Mode: DialogueModePGSearch,
		Coverage: dialogueStatusCoverage{
			Pending:         coverage.Pending,
			Running:         coverage.Running,
			Retry:           coverage.Retry,
			Failed:          coverage.Failed,
			OldestPendingAt: coverage.OldestPendingAt,
			ConsumerLag:     dialogueStatusLag(ctx, s.store.dialogueHybrid()),
		},
	}
	if rt := s.store.dialogueHybrid(); rt != nil {
		data.Mode = DialogueModeHybrid
		ptr, err := s.store.dialoguePointer(ctx)
		if err != nil {
			internal(c, err)
			return
		}
		data.Pointer = &dialogueStatusPointer{
			IndexVersion:     ptr.IndexVersion,
			EsIndex:          ptr.EsIndex,
			MilvusCollection: ptr.MilvusCollection,
		}
		// 健康探测限 8s：诊断端点不该被半死的外部依赖拖满整个客户端超时。
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		lexErr, vecErr := rt.es.Healthy(probeCtx), rt.mil.Healthy(probeCtx)
		data.Lexical = &dialogueStatusLexical{Kind: "elasticsearch", Index: rt.esIndex, Healthy: lexErr == nil}
		data.Vector = &dialogueStatusVector{
			Kind:           "milvus",
			Collection:     rt.milColl,
			Healthy:        vecErr == nil,
			EmbeddingModel: rt.cfg.Embedding.Model,
			Dimensions:     rt.mil.Dim(),
		}
		if lexErr != nil || vecErr != nil {
			// 探测失败只压低 healthy 布尔值；分类与原因进日志（不含凭据与正文）。
			log.Printf("dialogue status: 健康探测 lexical=%v vector=%v", lexErr, vecErr)
		}
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", data)
}

// dialogueStatusLag 汇总两个消费者组的积压（词法 + 向量）。任一臂查不动 → -1
// （「未知」），操作员从日志看原因，端点不虚构一个 0。
func dialogueStatusLag(ctx context.Context, rt *dialogueHybridRuntime) int64 {
	if rt == nil {
		return 0
	}
	esLag, esErr := dialogueQueueLag(ctx, rt.esQ)
	vecLag, vecErr := dialogueQueueLag(ctx, rt.vecQ)
	if esErr != nil || vecErr != nil {
		log.Printf("dialogue status: 消费 lag 查询失败 es=%v vector=%v", esErr, vecErr)
		return -1
	}
	return esLag + vecLag
}
