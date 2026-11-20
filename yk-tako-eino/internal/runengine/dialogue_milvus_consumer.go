package runengine

// 本文件是向量侧消费者（组 tako-dialogue-vector-v1，doc/84 §6.1）的处理体：
// 认领后的任务批 → 回读 chunk → PII 脱敏 → **批量** embedding（分批 + token 上限，
// 单请求 ≤BATCH_SIZE 条、累计 ≤MAX_INPUT_TOKENS）→ 一次 Milvus upsert 带全部行。
//
// embedding 的背压是结构性的：消费循环同步处理（一批做完才拉下一批），
// 批量与限流由 dialogueEmbedGroups 承担；失败的组不影响成功的组
// （组失败只翻回它自己那几条任务的退避/死信，成功的照常 upsert）。
//
// move 的纪律（doc/84 §6.3）：只更新 workspace_id 元数据、**不重新 embedding**——
// 用 GetVector 回读已存向量重写行；行不存在 = 无可移动，直接完成。

import (
	"context"

	"yk-tako-eino/internal/observability"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// dialogueMilvusIndex 是 Milvus 向量索引的消费面（真实现 *dialogueMilvusClient；单测打桩）。
type dialogueMilvusIndex interface {
	Upsert(ctx context.Context, rows []dialogueMilvusRow) error
	DeleteByRunIDs(ctx context.Context, runIDs []string) error
	GetVector(ctx context.Context, runID string) ([]float32, bool, error)
}

var _ dialogueMilvusIndex = (*dialogueMilvusClient)(nil)

// dialogueEmbedderAPI 是 embedding 的消费面（真实现 *dialogueEmbedder；单测打桩）。
type dialogueEmbedderAPI interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

var _ dialogueEmbedderAPI = (*dialogueEmbedder)(nil)

// dialogueMilvusProcessor 是向量组的批处理体。
type dialogueMilvusProcessor struct {
	store   *Store
	mil     dialogueMilvusIndex
	embed   dialogueEmbedderAPI
	counter TokenCounter
	cfg     dialogueEmbeddingConfig // 只用 BatchSize / MaxInputTokens 两个节奏值
}

func newDialogueMilvusProcessor(store *Store, mil dialogueMilvusIndex, embed dialogueEmbedderAPI, counter TokenCounter, cfg dialogueEmbeddingConfig) *dialogueMilvusProcessor {
	return &dialogueMilvusProcessor{store: store, mil: mil, embed: embed, counter: counter, cfg: cfg}
}

// Target 返回本处理体所属消费者组的目标词。
func (p *dialogueMilvusProcessor) Target() string { return DialogueTargetMilvus }

// dialogueMilvusRowFrom 组一行 upsert 数据：归属来自任务行（项目 id 真源），
// passage 来自 chunk 并做 PII 脱敏（doc/84 §4.1：外部索引只存脱敏副本）。
func dialogueMilvusRowFrom(job *DialogueIndexJob, src *dialogueIndexSource, vec []float32) dialogueMilvusRow {
	fin := int64(0)
	if src.FinishedAt != nil {
		fin = src.FinishedAt.Unix()
	}
	return dialogueMilvusRow{
		RunID:          job.RunID,
		SessionID:      job.SessionID,
		TenantID:       job.TenantID,
		UserID:         job.UserID,
		WorkspaceID:    job.WorkspaceID,
		Passage:        MaskPIIKeepUserMasked(src.Passage),
		IndexVersion:   job.IndexVersion,
		EmbeddingModel: job.EmbeddingModel,
		RunFinishedAt:  fin,
		IndexedAt:      src.IndexedAt.Unix(),
		Embedding:      vec,
	}
}

// Process 处理一批向量任务，返回与 jobs 等长同序的结局。
func (p *dialogueMilvusProcessor) Process(ctx context.Context, jobs []*DialogueIndexJob) []dialogueJobOutcome {
	outcomes := make([]dialogueJobOutcome, len(jobs))
	runIDs := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job.Kind == DialogueJobKindUpsert || job.Kind == DialogueJobKindMove {
			runIDs = append(runIDs, job.RunID)
		}
	}
	sources, err := p.store.dialogueIndexSources(ctx, runIDs)
	if err != nil {
		for i := range outcomes {
			outcomes[i] = dialogueJobOutcome{Class: dialogueErrPgRead}
		}
		return outcomes
	}

	// 第一遍：把任务分成「待 embed 的 upsert」「待删的 run」「move 专用路径」。
	type embedItem struct {
		jobIdx int
		text   string
	}
	var (
		embeds  []embedItem
		delIDs  []string
		delIdx  []int
		movRows []dialogueMilvusRow
		movIdx  []int
	)
	for i, job := range jobs {
		switch job.Kind {
		case DialogueJobKindDelete:
			delIDs = append(delIDs, job.RunID)
			delIdx = append(delIdx, i)
		case DialogueJobKindMove:
			src, ok := sources[job.RunID]
			if !ok {
				continue // 没有可移动的行：直接完成
			}
			vec, exists, gerr := p.mil.GetVector(ctx, job.RunID)
			if gerr != nil {
				outcomes[i] = dialogueOutcome(gerr)
				continue
			}
			if !exists {
				continue // 行不在：无可移动
			}
			movRows = append(movRows, dialogueMilvusRowFrom(job, src, vec))
			movIdx = append(movIdx, i)
		case DialogueJobKindUpsert:
			src, ok := sources[job.RunID]
			if !ok {
				// chunk 已没了：外部副本收敛删除。
				delIDs = append(delIDs, job.RunID)
				delIdx = append(delIdx, i)
				continue
			}
			embeds = append(embeds, embedItem{jobIdx: i, text: MaskPIIKeepUserMasked(src.Passage)})
		default:
			outcomes[i] = dialogueJobOutcome{Class: dlgErrMilvusPermanent, Permanent: true}
		}
	}

	// 第二遍：批量 embedding（分批 + 单请求 token 上限；超限块整条永久死信，不截断）。
	// span 记模型、条数与失败分类——只记 ID/计数/分类，不记正文与向量原文（doc/84 §6.5）。
	type rowItem struct {
		jobIdx int
		row    dialogueMilvusRow
	}
	var rows []rowItem
	if len(embeds) > 0 {
		embCtx, embSpan := observability.Tracer().Start(ctx, "dialogue.index.embedding", trace.WithSpanKind(trace.SpanKindInternal))
		embSpan.SetAttributes(
			attribute.Int("dialogue.texts", len(embeds)),
			attribute.String("dialogue.embedding.model", p.cfg.Model),
		)
		texts := make([]string, len(embeds))
		for i, e := range embeds {
			texts[i] = e.text
		}
		groups, over := dialogueEmbedGroups(p.counter, texts, p.cfg.BatchSize, p.cfg.MaxInputTokens)
		for _, gi := range over {
			outcomes[embeds[gi].jobIdx] = dialogueJobOutcome{Class: dlgErrEmbedOverLimit, Permanent: true}
		}
		vecs := make([][]float32, len(texts))
		done := make([]bool, len(texts))
		var embFail error
		for _, g := range groups {
			gt := make([]string, len(g))
			for j, gi := range g {
				gt[j] = texts[gi]
			}
			gv, eerr := p.embed.EmbedBatch(embCtx, gt)
			if eerr != nil {
				if embFail == nil {
					embFail = eerr
				}
				oc := dialogueOutcome(eerr)
				for _, gi := range g {
					outcomes[embeds[gi].jobIdx] = oc
				}
				continue
			}
			for j, gi := range g {
				vecs[gi] = gv[j]
				done[gi] = true
			}
		}
		for gi, e := range embeds {
			if !done[gi] {
				continue // 该组 embedding 失败：结局已写回
			}
			rows = append(rows, rowItem{
				jobIdx: e.jobIdx,
				row:    dialogueMilvusRowFrom(jobs[e.jobIdx], sources[jobs[e.jobIdx].RunID], vecs[gi]),
			})
		}
		if embFail != nil {
			embSpan.SetAttributes(attribute.String("dialogue.error.class", dialogueErrClass(embFail)))
		}
		finishSpan(embSpan, embFail)
	}
	for i := range movRows {
		rows = append(rows, rowItem{jobIdx: movIdx[i], row: movRows[i]})
	}

	// 第三遍：一次 upsert 带全部行 + 一次批量删（失败整组退避/死信；成功翻 done）。
	var milErr error
	milCtx, milSpan := observability.Tracer().Start(ctx, "dialogue.index.milvus", trace.WithSpanKind(trace.SpanKindInternal))
	defer func() {
		finishSpan(milSpan, milErr)
	}()
	if len(rows) > 0 {
		milRows := make([]dialogueMilvusRow, len(rows))
		idxs := make([]int, len(rows))
		for i, r := range rows {
			milRows[i] = r.row
			idxs[i] = r.jobIdx
		}
		if err := p.mil.Upsert(milCtx, milRows); err != nil {
			milErr = err
			oc := dialogueOutcome(err)
			for _, i := range idxs {
				outcomes[i] = oc
			}
		}
	}
	if len(delIDs) > 0 {
		if err := p.mil.DeleteByRunIDs(milCtx, delIDs); err != nil {
			if milErr == nil {
				milErr = err
			}
			oc := dialogueOutcome(err)
			for _, i := range delIdx {
				outcomes[i] = oc
			}
		}
	}
	milSpan.SetAttributes(
		attribute.Int("dialogue.jobs", len(jobs)),
		attribute.Int("dialogue.rows", len(rows)),
		attribute.Int("dialogue.deletes", len(delIDs)),
	)
	if milErr != nil {
		milSpan.SetAttributes(attribute.String("dialogue.error.class", dialogueErrClass(milErr)))
	}
	// 未写结局的任务默认空结局 = done（驱动据此翻 done）。
	return outcomes
}
