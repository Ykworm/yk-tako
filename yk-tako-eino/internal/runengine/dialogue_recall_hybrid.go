package runengine

// 本文件是 hybrid 查询链的装配与执行（doc/84 §7；施工包 §5.6/§6.5）：
//
//   - dialogueHybridRuntime：StartDialogueRetrieval 预检全过后挂在 Store 上的查询面
//     （词法臂、向量臂、embedding、两个队列的 lag 口）。pg_search 模式 / 关停后为 nil。
//   - dialogueHybridSearch：一次完整查询 = 脱敏 → ES 词法臂（归属等值 + index_version
//     过滤 + match，Top-K）→ 查询 embedding（真实调用）→ Milvus 向量臂（同口径标量
//     过滤，Top-K）→ RRF 合并。权限过滤在两臂的检索请求里完成（doc/84 §7.2：外部
//     过滤是必要条件）；Milvus 零命中 → 零候选，即使 ES 有命中也不注入（doc/84 §7.3）；
//     任一臂失败 → 错误上抛（调用方记日志、run 继续，不静默退回 pg_search）。
//   - 观测（doc/84 §6.5）：词法/embedding/向量/RRF 四个 span，只记模型、候选数、
//     版本、失败分类——不记正文、向量原文或密钥。

import (
	"context"
	"errors"
	"fmt"
	"log"

	"yk-tako-eino/internal/observability"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// dialogueRecallCandidates 是每臂的候选上限（doc/84 §5.4/§5.5 Top-10；写死不是配置）。
const dialogueRecallCandidates = 10

// dialogueLexSearcher 是词法臂的查询面（真实现 *dialogueESClient；单测打桩）。
// indexVersion 过滤活跃代次（dialogue_retrieval_pointer 的真源值）。
type dialogueLexSearcher interface {
	Search(ctx context.Context, query, tenantID, workspaceID, userID string, indexVersion int64, topK int) ([]dialogueLexHit, error)
	Healthy(ctx context.Context) error
}

// dialogueVecSearcher 是向量臂的查询面（真实现 *dialogueMilvusClient；单测打桩）。
type dialogueVecSearcher interface {
	Search(ctx context.Context, vec []float32, expr string, topK int) ([]dialogueVecHit, error)
	Healthy(ctx context.Context) error
	Dim() int
}

// dialogueHybridRuntime 是 hybrid 模式挂在 Store 上的查询面运行时：持有启动期装配的
// 两个客户端与 embedding（客户端绑定启动时的索引/collection 名，指针切换后由绑定
// 检查 fail closed），另存两个队列供 status 端点查消费 lag。cfg 只读连接口径
// （status 端点报模型名；凭据不出运行时）。
type dialogueHybridRuntime struct {
	cfg     DialogueRetrievalConfig
	es      dialogueLexSearcher
	mil     dialogueVecSearcher
	embed   dialogueEmbedderAPI
	esQ     dialogueQueue
	vecQ    dialogueQueue
	esIndex string // 启动绑定的 ES 索引名（指针切换检测用）
	milColl string // 启动绑定的 Milvus collection 名
}

// attachDialogueRuntime / dialogueHybrid：运行时的挂载面。start 装配完成后挂上，
// stop 第一时间摘除（原子指针，读侧无锁）。
func (s *Store) attachDialogueRuntime(rt *dialogueHybridRuntime) {
	if s != nil {
		s.dialogueRuntime.Store(rt)
	}
}

// dialogueHybrid 返回当前运行时；nil = pg_search 生效或 hybrid 尚未装配完。
func (s *Store) dialogueHybrid() *dialogueHybridRuntime {
	if s == nil {
		return nil
	}
	return s.dialogueRuntime.Load()
}

// dialogueRecallScope 决定 hybrid 两臂的租户与人（与 memoryRecallFilter 同口径，
// 只是返回普通值给外部过滤器）：scope=user 按人过滤；scope=tenant（owner/admin
// 预览）只按租户，能看到本工作区全员；无身份（单测 default 分区）= 不限人。
func dialogueRecallScope(ctx context.Context, cfg MemoryConfig) (tenantID, userID string) {
	tenantID = cfg.TenantID
	sc := scopeFrom(ctx)
	if scopeActive(sc) {
		tenantID = sc.TenantID
		if sc.Scope != "tenant" {
			userID = sc.UserID
		}
	}
	return tenantID, userID
}

// dialogueRecallWorkspaceID 取当次查询的项目 id：真源是 ctx 里的 WorkspaceRef
// （Worker 起跑时按会话挂的项目注入）。预览端点没有 run 上下文 = 空串，
// 只匹配未挂项目的对话块——预览显示的就是模型实际会看到的东西。
func dialogueRecallWorkspaceID(ctx context.Context) string {
	if ref, ok := workspaceFrom(ctx); ok {
		return ref.WorkspaceID
	}
	return ""
}

// recallDialogueHybridItems 是 Recall 线二的 hybrid 入口：读指针（代次真源）、
// 解析查询范围，走完整查询链。
func (s *Store) recallDialogueHybridItems(ctx context.Context, rt *dialogueHybridRuntime, raw string) ([]dialogueHybridHit, error) {
	ptr, err := s.dialoguePointer(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, userID := dialogueRecallScope(ctx, s.memoryCfg)
	return dialogueHybridSearch(ctx, rt, ptr, raw, tenantID, userID,
		dialogueRecallWorkspaceID(ctx), dialogueRecallCandidates)
}

// recallDialogueHybrid 是 Recall 线二 hybrid 分支的一站式调用：查询链 + 前
// dialogueRecallItemsMax 条渲染成引用块。返回（注入条数, 块, 错误）：
//   - Milvus 零命中 → ("", 0, nil)：不是失败，是没有可注入的内容（doc/84 §7.3）；
//   - 任一臂失败 → err 上抛：调用方记日志、run 继续，**不静默退回 pg_search**；
//   - 关停窗口（查询面已摘）→ fail closed 报错，同样不退回。
func (s *Store) recallDialogueHybrid(ctx context.Context, raw string) (int, string, error) {
	rt := s.dialogueHybrid()
	if rt == nil {
		return 0, "", errors.New("dialogue hybrid 运行时不可用（关停中），本次不注入")
	}
	lineCtx, lineSpan := observability.Tracer().Start(ctx, "retrieval", trace.WithSpanKind(trace.SpanKindInternal))
	lineSpan.SetAttributes(
		attribute.String(attrOperation, "retrieval"),
		attribute.String("gen_ai.data_source.id", "dialogue_hybrid"),
		attribute.Int("gen_ai.retrieval.top_k", dialogueRecallItemsMax),
	)
	observability.SetTextJSON(lineSpan, "gen_ai.retrieval.query.text", raw)
	hits, err := s.recallDialogueHybridItems(lineCtx, rt, raw)
	if err != nil {
		finishSpan(lineSpan, err)
		return 0, "", err
	}
	lineSpan.SetAttributes(attribute.Int("gen_ai.retrieval.record.count", len(hits)))
	finishSpan(lineSpan, nil)
	if len(hits) > dialogueRecallItemsMax {
		hits = hits[:dialogueRecallItemsMax]
	}
	block, n := renderDialogueHybridBlock(hits)
	return n, block, nil
}

// dialogueHybridSearch 是 hybrid 查询链本体（doc/84 §7）。返回融合后的全部候选
// （调用方取前 3 注入，或全量给诊断端点）。零命中与绑定错位都返回 (nil, nil)：
// 不是失败，是没有可注入的内容。
func dialogueHybridSearch(ctx context.Context, rt *dialogueHybridRuntime, ptr *DialogueRetrievalPointer,
	raw, tenantID, userID, workspaceID string, topK int) ([]dialogueHybridHit, error) {
	if ptr == nil {
		return nil, errors.New("dialogue_retrieval_pointer 未初始化（hybrid 装配未执行？）")
	}
	// 指针已切换（版本化重建换了索引名）而本进程未重启：客户端还绑着旧索引，
	// 按旧代次查只会拿旧数据——fail closed，零候选不注入，等重启重绑。
	if ptr.EsIndex != rt.esIndex || ptr.MilvusCollection != rt.milColl {
		log.Printf("dialogue recall: 指针已切到 es=%s mil=%s，检索客户端仍绑 %s/%s（重启后重绑）——本次不注入",
			ptr.EsIndex, ptr.MilvusCollection, rt.esIndex, rt.milColl)
		return nil, nil
	}
	masked := MaskPIIKeepUserMasked(raw)

	// 词法臂：归属等值 + 活跃代次过滤 + match（BM25 分数只做臂内排序）。
	lexCtx, lexSpan := observability.Tracer().Start(ctx, "dialogue.recall.es", trace.WithSpanKind(trace.SpanKindInternal))
	lexHits, lerr := rt.es.Search(lexCtx, masked, tenantID, workspaceID, userID, ptr.IndexVersion, topK)
	if lerr != nil {
		lexSpan.SetAttributes(attribute.String("dialogue.error.class", dialogueErrClass(lerr)))
		finishSpan(lexSpan, lerr)
		return nil, lerr
	}
	lexSpan.SetAttributes(
		attribute.Int("dialogue.candidates", len(lexHits)),
		attribute.Int64("dialogue.index_version", ptr.IndexVersion),
	)
	finishSpan(lexSpan, nil)

	// 向量臂第一步：查询 embedding（真实调用；索引与查询同一脱敏口径）。
	embCtx, embSpan := observability.Tracer().Start(ctx, "dialogue.recall.embedding", trace.WithSpanKind(trace.SpanKindInternal))
	embSpan.SetAttributes(attribute.String("dialogue.embedding.model", rt.cfg.Embedding.Model))
	vecs, eerr := rt.embed.EmbedBatch(embCtx, []string{masked})
	if eerr != nil {
		embSpan.SetAttributes(attribute.String("dialogue.error.class", dialogueErrClass(eerr)))
		finishSpan(embSpan, eerr)
		return nil, eerr
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		bad := &dialogueClientError{Class: dlgErrEmbedBadResponse, Permanent: true,
			Cause: fmt.Errorf("查询向量返回 %d 条（要 1 条）", len(vecs))}
		embSpan.SetAttributes(attribute.String("dialogue.error.class", bad.Class))
		finishSpan(embSpan, bad)
		return nil, bad
	}
	finishSpan(embSpan, nil)

	// 向量臂第二步：Milvus 检索（同口径标量过滤 + 活跃代次）。
	milCtx, milSpan := observability.Tracer().Start(ctx, "dialogue.recall.milvus", trace.WithSpanKind(trace.SpanKindInternal))
	vecExpr := dialogueMilvusFilter(tenantID, workspaceID, userID, ptr.IndexVersion)
	vecHits, verr := rt.mil.Search(milCtx, vecs[0], vecExpr, topK)
	if verr != nil {
		milSpan.SetAttributes(attribute.String("dialogue.error.class", dialogueErrClass(verr)))
		finishSpan(milSpan, verr)
		return nil, verr
	}
	milSpan.SetAttributes(
		attribute.Int("dialogue.candidates", len(vecHits)),
		attribute.Int64("dialogue.index_version", ptr.IndexVersion),
	)
	finishSpan(milSpan, nil)

	// Milvus 零命中：即使 ES 有候选，也不生成 <dialogue-context>，连空壳和提示语
	// 都不给 LLM（doc/84 §7.3）；只记日志与诊断。
	if len(vecHits) == 0 {
		log.Printf("dialogue recall: milvus 零命中（es 候选 %d）——不注入任何检索块（doc/84 §7.3）", len(lexHits))
		return nil, nil
	}

	// RRF 合并（rank 从 1 起，1/(60+rank) 累加；降序 + runID 升序）。
	_, rrfSpan := observability.Tracer().Start(ctx, "dialogue.recall.rrf", trace.WithSpanKind(trace.SpanKindInternal))
	fused := dialogueHybridFuse(lexHits, vecHits)
	rrfSpan.SetAttributes(
		attribute.Int("dialogue.candidates", len(lexHits)+len(vecHits)),
		attribute.Int("dialogue.merged", len(fused)),
	)
	finishSpan(rrfSpan, nil)
	return fused, nil
}

// dialogueErrClass 提取机器可读错误分类（span 与日志用；无分类按瞬时内部错误报）。
func dialogueErrClass(err error) string {
	var ce *dialogueClientError
	if errors.As(err, &ce) {
		return ce.Class
	}
	return dialogueErrInternalTransient
}

// dialogueQueueLag 查一个消费者组的积压（status 端点用；与对账器同一 lag 面）。
func dialogueQueueLag(ctx context.Context, q dialogueQueue) (int64, error) {
	if q == nil {
		return 0, errors.New("队列未装配")
	}
	lagger, ok := q.(dialogueQueueLagger)
	if !ok {
		return 0, errors.New("队列未实现 lag 查询")
	}
	return lagger.ConsumerLag(ctx)
}
