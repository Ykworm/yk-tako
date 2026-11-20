package runengine

// 本文件是 ES 侧消费者（组 tako-dialogue-es-v1，doc/84 §6.1）的处理体：
// 认领后的任务批 → 回读 chunk → PII 脱敏 → ES 文档 upsert/delete。
//
// 归属一律以任务行为准（workspace_id = 项目 id）；chunk 行只供 passage 与 run 定稿时间
// （chunk.workspace 是引擎作业根路径，语义不同，绝不当项目 id 用）。
//
// 收敛语义（doc/84 §6.2 幂等）：
//   - upsert 但 chunk 已没了  → 删外部副本（防 stale 写）；
//   - move 但 chunk 已没了    → 直接完成（无可移动；级联删除的 delete 任务负责清外部）；
//   - delete                  → ES 文档删除，deleted/not_found 都算确认不存在。
//
// 全部走 doc/84 §6.1 的可靠性契约：持久化结果在前、提交 offset 在后（驱动在
// dialogue_outbox_relay.go）；失败分类由 dialogueClientError 带出（瞬时退避 / 永久死信）。

import (
	"context"
	"errors"
	"strings"

	"yk-tako-eino/internal/observability"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// dialogueESIndex 是 ES 词法索引的消费面（真实现 *dialogueESClient；单测打桩）。
type dialogueESIndex interface {
	Upsert(ctx context.Context, doc dialogueESDoc) error
	Delete(ctx context.Context, runID string) error
}

var _ dialogueESIndex = (*dialogueESClient)(nil)

// dialogueESProcessor 是 ES 组的批处理体。
type dialogueESProcessor struct {
	store *Store
	es    dialogueESIndex
}

func newDialogueESProcessor(store *Store, es dialogueESIndex) *dialogueESProcessor {
	return &dialogueESProcessor{store: store, es: es}
}

// Target 返回本处理体所属消费者组的目标词。
func (p *dialogueESProcessor) Target() string { return DialogueTargetES }

// dialogueOutcome 把一次外部调用的错误翻成写回结局：带分类的客户端错误按其
// 瞬时/永久语义写回；未知错误按瞬时退避（下一轮自愈，不轻易死信）。
func dialogueOutcome(err error) dialogueJobOutcome {
	if err == nil {
		return dialogueJobOutcome{}
	}
	var ce *dialogueClientError
	if errors.As(err, &ce) {
		return dialogueJobOutcome{Class: ce.Class, Permanent: ce.Permanent}
	}
	return dialogueJobOutcome{Class: dialogueErrInternalTransient}
}

// Process 处理一批 ES 任务，返回与 jobs 等长同序的结局（驱动据此写回）。
// span 记任务数、run id 清单与失败分类（doc/84 §6.5：只记 ID 与分类，不记正文/密钥）。
func (p *dialogueESProcessor) Process(ctx context.Context, jobs []*DialogueIndexJob) (outcomes []dialogueJobOutcome) {
	ctx, span := observability.Tracer().Start(ctx, "dialogue.index.es", trace.WithSpanKind(trace.SpanKindInternal))
	runIDs := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job.Kind == DialogueJobKindUpsert || job.Kind == DialogueJobKindMove {
			runIDs = append(runIDs, job.RunID)
		}
	}
	defer func() {
		classes := make([]string, 0, len(outcomes))
		for _, oc := range outcomes {
			if oc.Class != "" {
				classes = append(classes, oc.Class)
			}
		}
		span.SetAttributes(
			attribute.Int("dialogue.jobs", len(jobs)),
			attribute.String("dialogue.run_ids", clipMemoryText(strings.Join(runIDs, ","), 200)),
		)
		if len(classes) > 0 {
			span.SetAttributes(attribute.String("dialogue.error.classes", strings.Join(classes, ",")))
		}
		finishSpan(span, nil)
	}()
	outcomes = make([]dialogueJobOutcome, len(jobs))
	sources, err := p.store.dialogueIndexSources(ctx, runIDs)
	if err != nil {
		// chunk 都读不出来：整批按瞬时失败退回（attempts 不白耗，下轮重试）。
		for i := range outcomes {
			outcomes[i] = dialogueJobOutcome{Class: dialogueErrPgRead}
		}
		return outcomes
	}
	for i, job := range jobs {
		outcomes[i] = p.processOne(ctx, job, sources)
	}
	return outcomes
}

// processOne 处理单条任务（批内的回读结果共享）。
func (p *dialogueESProcessor) processOne(ctx context.Context, job *DialogueIndexJob, sources map[string]*dialogueIndexSource) dialogueJobOutcome {
	switch job.Kind {
	case DialogueJobKindUpsert, DialogueJobKindMove:
		src, ok := sources[job.RunID]
		if !ok {
			if job.Kind == DialogueJobKindMove {
				return dialogueJobOutcome{} // 没有可移动的行：直接完成
			}
			// upsert 但 chunk 已没了：外部副本收敛删除（绝不拿旧内存写 ES）。
			return dialogueOutcome(p.es.Delete(ctx, job.RunID))
		}
		doc := dialogueESDoc{
			RunID:          job.RunID,
			SessionID:      job.SessionID,
			TenantID:       job.TenantID,
			UserID:         job.UserID,
			WorkspaceID:    job.WorkspaceID,
			Passage:        MaskPIIKeepUserMasked(src.Passage),
			IndexVersion:   job.IndexVersion,
			EmbeddingModel: job.EmbeddingModel,
			RunFinishedAt:  src.FinishedAt,
			IndexedAt:      src.IndexedAt,
		}
		return dialogueOutcome(p.es.Upsert(ctx, doc))
	case DialogueJobKindDelete:
		return dialogueOutcome(p.es.Delete(ctx, job.RunID))
	default:
		// kind 不在词表（CHECK 约束本应挡住）：死信，不重试。
		return dialogueJobOutcome{Class: dlgErrESPermanent, Permanent: true}
	}
}
