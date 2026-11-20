package runengine

// 本文件是历史对话向量索引的 Milvus 客户端（doc/84 §5.2）：collection 创建（维度由
// 真实 embedding 探针确定）、幂等 upsert/delete、标量过滤向量检索。用官方 SDK
// github.com/milvus-io/milvus-sdk-go/v2（施工包 §2.3 唯一例外；P0 实测 v2.4.2 +
// client.Config.APIKey token 认证）。
//
// Zilliz Cloud serverless 的冷启动（P0 实测）会让首批调用偶发超时——重试 2 次内嵌在
// doTransient 里；连续失败按 milvus_unavailable 瞬时分类走退避。
//
// 维度纪律（doc/84 §5.2）：EnsureCollection 用 DescribeCollection 的真实 schema 比对
// 维度，不符 = 永久错误（milvus_dimension_mismatch → 119004，必须显式重建，不混写）。
// 本期固定 embedding 模型，不做模型迁移。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// dialogueVecHit 是向量检索的一条命中（doc/84 §5.5：run_id、距离分数、index_version、
// 脱敏 passage、来源会话与时间元数据；不再逐条回 PG）。
type dialogueVecHit struct {
	RunID        string
	Score        float64
	IndexVersion int64
	SessionID    string
	Passage      string
	FinishedAt   *time.Time
	IndexedAt    *time.Time
}

// dialogueMilvusRow 是一次 upsert 的一行（doc/84 §5.2 字段词表）。
// 时间列 INT64 存 unix 秒（0 = 无值）；embedding 由调用方填（Milvus 消费者批量生成）。
type dialogueMilvusRow struct {
	RunID          string
	SessionID      string
	TenantID       string
	UserID         string
	WorkspaceID    string
	Passage        string
	IndexVersion   int64
	EmbeddingModel string
	RunFinishedAt  int64
	IndexedAt      int64
	Embedding      []float32
}

// dialogueMilvusClient 是 Milvus 客户端（单进程一份，绑定一个 collection 与锁定维度）。
type dialogueMilvusClient struct {
	coll string
	dim  int
	cli  client.Client
}

// newDialogueMilvusClient 建 gRPC 客户端（懒连接：首轮真实调用才连上；serverless
// 冷启动的偶发超时由各方法的 doTransient 兜）。
func newDialogueMilvusClient(uri, token, db, collection string, useTLS bool, dim int) (*dialogueMilvusClient, error) {
	if uri == "" || collection == "" {
		return nil, fmt.Errorf("milvus 配置不全：uri 或 collection 为空")
	}
	cli, err := client.NewClient(context.Background(), client.Config{
		Address:       uri,
		APIKey:        token,
		DBName:        db,
		EnableTLSAuth: useTLS,
	})
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrMilvusUnavailable, Cause: err}
	}
	return &dialogueMilvusClient{coll: collection, dim: dim, cli: cli}, nil
}

// dialogueMilvusSchema 建 collection schema（doc/84 §5.2 字段词表；P0 探针逐字验证过
// 的建法）。passage 16384 同时覆盖「4000 rune ≤ 16384 字节/字符」两种计量口径。
func dialogueMilvusSchema(collection string, dim int) *entity.Schema {
	return entity.NewSchema().WithName(collection).
		WithField(entity.NewField().WithName("run_id").WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(64)).
		WithField(entity.NewField().WithName("session_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("tenant_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("user_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("workspace_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("passage").WithDataType(entity.FieldTypeVarChar).WithMaxLength(16384)).
		WithField(entity.NewField().WithName("index_version").WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName("embedding_model").WithDataType(entity.FieldTypeVarChar).WithMaxLength(256)).
		WithField(entity.NewField().WithName("run_finished_at").WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName("indexed_at").WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
}

// doTransient 给 SDK 调用加两次重试（serverless 冷启动偶发超时，P0 实测 2 次内稳定）。
func doTransient(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		err = fn()
		if err == nil {
			return nil
		}
		if strings.Contains(strings.ToLower(err.Error()), "dimension") {
			return &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true, Cause: err}
		}
	}
	return &dialogueClientError{Class: dlgErrMilvusUnavailable, Cause: err}
}

// dialogueSchemaDim 从 DescribeCollection 的真实 schema 里读 embedding 字段维度
// （SDK 把维度放在 TypeParams["dim"]，DescribeCollection 回的是服务端真实值）。
func dialogueSchemaDim(coll *entity.Collection) (int, bool) {
	if coll == nil || coll.Schema == nil {
		return 0, false
	}
	for _, f := range coll.Schema.Fields {
		if f.Name == "embedding" && f.DataType == entity.FieldTypeFloatVector {
			d, perr := strconv.ParseInt(f.TypeParams[entity.TypeParamDim], 10, 64)
			if perr != nil || d <= 0 {
				return 0, false
			}
			return int(d), true
		}
	}
	return 0, false
}

// EnsureCollection 幂等建 collection（doc/84 §5.2）：已存在则用真实 schema 比对维度
// （不符 = 永久错 119004 类）；不存在则按锁定维度建（HNSW COSINE M=16 efConstruction=200）
// 并 load。启动预检调用；进程内后续调用直接返回。
func (c *dialogueMilvusClient) EnsureCollection(ctx context.Context) error {
	if c.dim <= 0 {
		return &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true,
			Cause: fmt.Errorf("milvus 客户端未携带锁定维度（embedding 预检未跑？）")}
	}
	coll, err := c.cli.DescribeCollection(ctx, c.coll)
	if err == nil {
		d, ok := dialogueSchemaDim(coll)
		if !ok {
			return &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true,
				Cause: fmt.Errorf("collection %s 缺 embedding FLOAT_VECTOR 字段", c.coll)}
		}
		if d != c.dim {
			return &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true,
				Cause: fmt.Errorf("collection %s 维度 %d ≠ embedding 指纹 %d（模型或维度已变，请重建索引）", c.coll, d, c.dim)}
		}
		return nil
	}
	// DescribeCollection 失败：区分「不存在」与「连不上」——不存在才走建路径。
	// 「不存在」的报文实测过两种形态（2026-11-20 隔离引擎实测）：Milvus 开源形态
	// 「can not find collection」与 Zilliz Cloud serverless 形态
	// 「can't find collection[database=…][collection=…]」（多副本/重建与启动预检共用本判定）。
	if !strings.Contains(err.Error(), "can not find collection") &&
		!strings.Contains(err.Error(), "can't find collection") &&
		!strings.Contains(err.Error(), "not found") &&
		!strings.Contains(err.Error(), "describe collection failed") {
		return &dialogueClientError{Class: dlgErrMilvusUnavailable, Cause: err}
	}
	if err := doTransient(ctx, func() error {
		return c.cli.CreateCollection(ctx, dialogueMilvusSchema(c.coll, c.dim), 1)
	}); err != nil {
		return err
	}
	idx, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
	if err != nil {
		return &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true, Cause: err}
	}
	if err := doTransient(ctx, func() error {
		return c.cli.CreateIndex(ctx, c.coll, "embedding", idx, false)
	}); err != nil {
		return err
	}
	if err := doTransient(ctx, func() error {
		return c.cli.LoadCollection(ctx, c.coll, false)
	}); err != nil {
		return err
	}
	return nil
}

// milvusTimePtr 把 unix 秒转回 *time.Time（0 = 无值）。
func milvusTimePtr(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

// Upsert 幂等批量写行（单次调用带全部行；serverless 对批量大小另有服务端上限，
// 调用方批次受 BATCH_SIZE 与 token 上限约束，规模有限）。
func (c *dialogueMilvusClient) Upsert(ctx context.Context, rows []dialogueMilvusRow) error {
	if len(rows) == 0 {
		return nil
	}
	runIDs := make([]string, 0, len(rows))
	sessionIDs := make([]string, 0, len(rows))
	tenantIDs := make([]string, 0, len(rows))
	userIDs := make([]string, 0, len(rows))
	workspaceIDs := make([]string, 0, len(rows))
	passages := make([]string, 0, len(rows))
	indexVersions := make([]int64, 0, len(rows))
	models := make([]string, 0, len(rows))
	finishedAts := make([]int64, 0, len(rows))
	indexedAts := make([]int64, 0, len(rows))
	vecs := make([][]float32, 0, len(rows))
	for _, r := range rows {
		if len(r.Embedding) != c.dim {
			return &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true,
				Cause: fmt.Errorf("run %s 向量维度 %d ≠ 锁定维度 %d", r.RunID, len(r.Embedding), c.dim)}
		}
		runIDs = append(runIDs, r.RunID)
		sessionIDs = append(sessionIDs, r.SessionID)
		tenantIDs = append(tenantIDs, r.TenantID)
		userIDs = append(userIDs, r.UserID)
		workspaceIDs = append(workspaceIDs, r.WorkspaceID)
		passages = append(passages, r.Passage)
		indexVersions = append(indexVersions, r.IndexVersion)
		models = append(models, r.EmbeddingModel)
		finishedAts = append(finishedAts, r.RunFinishedAt)
		indexedAts = append(indexedAts, r.IndexedAt)
		vecs = append(vecs, r.Embedding)
	}
	cols := []entity.Column{
		entity.NewColumnVarChar("run_id", runIDs),
		entity.NewColumnVarChar("session_id", sessionIDs),
		entity.NewColumnVarChar("tenant_id", tenantIDs),
		entity.NewColumnVarChar("user_id", userIDs),
		entity.NewColumnVarChar("workspace_id", workspaceIDs),
		entity.NewColumnVarChar("passage", passages),
		entity.NewColumnInt64("index_version", indexVersions),
		entity.NewColumnVarChar("embedding_model", models),
		entity.NewColumnInt64("run_finished_at", finishedAts),
		entity.NewColumnInt64("indexed_at", indexedAts),
		entity.NewColumnFloatVector("embedding", c.dim, vecs),
	}
	return doTransient(ctx, func() error {
		_, err := c.cli.Upsert(ctx, c.coll, "", cols...)
		return err
	})
}

// DeleteByRunIDs 按主键删行（run_id VarChar PK）。空列表直接返回。
func (c *dialogueMilvusClient) DeleteByRunIDs(ctx context.Context, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	return doTransient(ctx, func() error {
		return c.cli.DeleteByPks(ctx, c.coll, "", entity.NewColumnVarChar("run_id", runIDs))
	})
}

// GetVector 回读一行已存的向量（move 语义专用：doc/84 §6.3 只更新 workspace_id 元数据、
// 不重新 embedding——用原向量重写行）。行不存在返回 (nil, false, nil)。
func (c *dialogueMilvusClient) GetVector(ctx context.Context, runID string) ([]float32, bool, error) {
	expr := fmt.Sprintf(`run_id == "%s"`, milvusQuoteExpr(runID))
	var rs client.ResultSet
	if err := doTransient(ctx, func() error {
		var qerr error
		rs, qerr = c.cli.Query(ctx, c.coll, []string{}, expr, []string{"embedding"})
		return qerr
	}); err != nil {
		return nil, false, err
	}
	if len(rs) == 0 || rs[0].Len() == 0 {
		return nil, false, nil
	}
	v, err := rs[0].Get(0)
	if err != nil {
		return nil, false, &dialogueClientError{Class: dlgErrMilvusUnavailable, Cause: err}
	}
	switch vec := v.(type) {
	case entity.FloatVector:
		return []float32(vec), true, nil
	case []float32:
		return vec, true, nil
	default:
		return nil, false, &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true,
			Cause: fmt.Errorf("embedding 列返回类型 %T 不是向量", v)}
	}
}

// milvusQuoteExpr 转义标量表达式里的字符串字面量（id 来自内部 ULID，防御性处理）。
func milvusQuoteExpr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// dialogueMilvusFilter 组标量过滤表达式（doc/84 §5.5：tenant/user/workspace 等值 +
// 活跃代次）。userID 为空 = 不限用户（tenant 级可见）；indexVersion<=0 不过代次
// （仅限诊断用例；召回链永远带指针的真源值）。
func dialogueMilvusFilter(tenantID, workspaceID, userID string, indexVersion int64) string {
	parts := []string{
		fmt.Sprintf(`tenant_id == "%s"`, milvusQuoteExpr(tenantID)),
		fmt.Sprintf(`workspace_id == "%s"`, milvusQuoteExpr(workspaceID)),
	}
	if userID != "" {
		parts = append(parts, fmt.Sprintf(`user_id == "%s"`, milvusQuoteExpr(userID)))
	}
	if indexVersion > 0 {
		parts = append(parts, fmt.Sprintf(`index_version == %d`, indexVersion))
	}
	return strings.Join(parts, " && ")
}

// Search 向量检索（doc/84 §5.5：COSINE + HNSW 查询 ef=64，Top-10；expr 为标量过滤）。
func (c *dialogueMilvusClient) Search(ctx context.Context, vec []float32, expr string, topK int) ([]dialogueVecHit, error) {
	if len(vec) != c.dim {
		return nil, &dialogueClientError{Class: dlgErrMilvusDimMismatch, Permanent: true,
			Cause: fmt.Errorf("查询向量维度 %d ≠ 锁定维度 %d", len(vec), c.dim)}
	}
	sp, err := entity.NewIndexHNSWSearchParam(64)
	if err != nil {
		return nil, &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true, Cause: err}
	}
	outFields := []string{"run_id", "session_id", "passage", "index_version", "embedding_model", "run_finished_at", "indexed_at"}
	var rs []client.SearchResult
	if err := doTransient(ctx, func() error {
		var serr error
		rs, serr = c.cli.Search(ctx, c.coll, []string{}, expr, outFields,
			[]entity.Vector{entity.FloatVector(vec)}, "embedding", entity.COSINE, topK, sp)
		return serr
	}); err != nil {
		return nil, err
	}
	hits := make([]dialogueVecHit, 0, topK)
	for _, r := range rs {
		for i := 0; i < r.ResultCount; i++ {
			hit := dialogueVecHit{}
			if r.IDs != nil {
				if idv, gerr := r.IDs.Get(i); gerr == nil {
					if s, ok := idv.(string); ok {
						hit.RunID = s
					}
				}
			}
			if i < len(r.Scores) {
				hit.Score = float64(r.Scores[i])
			}
			if col := r.Fields.GetColumn("session_id"); col != nil {
				if v, gerr := col.Get(i); gerr == nil {
					if s, ok := v.(string); ok {
						hit.SessionID = s
					}
				}
			}
			if col := r.Fields.GetColumn("passage"); col != nil {
				if v, gerr := col.Get(i); gerr == nil {
					if s, ok := v.(string); ok {
						hit.Passage = s
					}
				}
			}
			if col := r.Fields.GetColumn("index_version"); col != nil {
				if iv, gerr := col.GetAsInt64(i); gerr == nil {
					hit.IndexVersion = iv
				}
			}
			if col := r.Fields.GetColumn("run_finished_at"); col != nil {
				if iv, gerr := col.GetAsInt64(i); gerr == nil {
					hit.FinishedAt = milvusTimePtr(iv)
				}
			}
			if col := r.Fields.GetColumn("indexed_at"); col != nil {
				if iv, gerr := col.GetAsInt64(i); gerr == nil {
					hit.IndexedAt = milvusTimePtr(iv)
				}
			}
			hits = append(hits, hit)
		}
	}
	return hits, nil
}

// Dim 返回锁定维度（启动预检探针写入；status 诊断端点报给操作员）。
func (c *dialogueMilvusClient) Dim() int { return c.dim }

// Healthy 向量臂健康探测（status 诊断端点用，doc/84 §6.3）：collection 可查即健康。
// CountDocs 对缺失 collection / 连接失败都报错——这里不虚构健康状态。
func (c *dialogueMilvusClient) Healthy(ctx context.Context) error {
	_, err := c.CountDocs(ctx)
	return err
}

// Flush 触发一次同步 flush（重建校验计数前用，让 row_count 尽量收敛）。
// 共享实例上它是幂等维护操作，只影响本 collection。
func (c *dialogueMilvusClient) Flush(ctx context.Context) error {
	return doTransient(ctx, func() error {
		return c.cli.Flush(ctx, c.coll, false)
	})
}

// CountDocs 计数当前 collection 行数（GetCollectionStatistics 的 row_count）。
// collection 不在时报错（与 ES 不同：Milvus 消费者写入路径没有建 collection 的自愈，
// 缺 collection 必须人工/重启预检介入，对账跳过该臂而不是当 0 处理）。
func (c *dialogueMilvusClient) CountDocs(ctx context.Context) (int64, error) {
	var stats map[string]string
	if err := doTransient(ctx, func() error {
		var err error
		stats, err = c.cli.GetCollectionStatistics(ctx, c.coll)
		return err
	}); err != nil {
		return 0, err
	}
	raw, ok := stats["row_count"]
	if !ok {
		return 0, &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true,
			Cause: fmt.Errorf("collection %s 统计缺 row_count", c.coll)}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &dialogueClientError{Class: dlgErrMilvusPermanent, Permanent: true,
			Cause: fmt.Errorf("collection %s row_count=%q 解析失败: %w", c.coll, raw, err)}
	}
	return n, nil
}

// ListAllRunIDs 枚举当前 collection 全部 run_id（对账用：Query 全量 + offset/limit
// 分页；不按 index_version 过滤——旧版本残留行按缺失/孤儿自然收敛）。
// collection 不在时 Query 报错（milvus_unavailable），由调用方跳过本臂。
func (c *dialogueMilvusClient) ListAllRunIDs(ctx context.Context, maxRuns int) (map[string]bool, bool, error) {
	const page = dialogueReconcileListPage
	out := make(map[string]bool)
	for offset := 0; ; offset += page {
		var rs client.ResultSet
		opts := []client.SearchQueryOptionFunc{client.WithLimit(int64(page)), client.WithOffset(int64(offset))}
		if err := doTransient(ctx, func() error {
			var qerr error
			rs, qerr = c.cli.Query(ctx, c.coll, []string{}, "index_version >= 0", []string{"run_id"}, opts...)
			return qerr
		}); err != nil {
			return nil, false, err
		}
		n := 0
		if len(rs) > 0 {
			var col entity.Column
			for _, candidate := range rs {
				if candidate.Name() == "run_id" {
					col = candidate
					break
				}
			}
			if col == nil {
				col = rs[0]
			}
			for i := 0; i < col.Len(); i++ {
				if v, gerr := col.Get(i); gerr == nil {
					if s, ok := v.(string); ok && s != "" {
						out[s] = true
						n++
					}
				}
			}
		}
		if n < page || len(out) >= maxRuns {
			break
		}
	}
	return out, len(out) >= maxRuns, nil
}

// Close 收连接。
func (c *dialogueMilvusClient) Close() {
	if c != nil && c.cli != nil {
		c.cli.Close()
	}
}
