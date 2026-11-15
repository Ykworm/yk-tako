// 本文件是隔离结果的发布协调（方案真源 = doc/74 §9.1/§9.3）：发布准入、Provider 发布、
// 收尾对账与过窗清理。RunEnd 快路径（ReconcileRun 落 pending 后当场调）与调度慢路径
// （reconcilePublications 列 pending/publishing/blocked）调用同一个服务。
//
// 锁顺序全仓统一：workspace 行 → workspaceID advisory lock → 组行 → Task 行。
// AcquireRun（新 run 准入）与 DeleteWorkspaceCascade（项目删除）取同一把 advisory lock，
// 所以「排除活 run + 写发布标记」与「插新 run」「删项目」不可能交错。
// Provider 的文件/Git 操作一律在 PG 事务外；事务只裁决状态与标记。
//
// 发布标记（workspaces.publication_orchestration_id + publication_epoch）只表示当前短发布
// 窗口：有标记时同项目新 run 回 409 117113。清标记按 orchestration ID + epoch 条件更新——
// 旧发布者不得清除新标记（doc/74 §9.1 第 6 条）。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockWorkspacePublicationTx 取 workspaceID 的发布协调 advisory lock（事务级，提交即放）。
// 调用方必须先锁 workspace 行（或像 AcquireRun 那样先过可见性），保持全仓同序。
func lockWorkspacePublicationTx(tx *gorm.DB, workspaceID string) error {
	return tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('workspace-pub:' || ?, 0))`, workspaceID).Error
}

// checkWorkspacePublicationTx 是新 run 准入的发布标记检查（doc/74 §9.1 第 4 条）：
// 取锁 → 读标记 → 有标记拒新 run。无标记时既有行为不变。
func checkWorkspacePublicationTx(tx *gorm.DB, workspaceID string) error {
	if err := lockWorkspacePublicationTx(tx, workspaceID); err != nil {
		return err
	}
	var mark string
	if err := tx.Raw(`SELECT publication_orchestration_id FROM workspaces WHERE id = ?`, workspaceID).
		Scan(&mark).Error; err != nil {
		return err
	}
	if mark != "" {
		return errWorkspacePublishing
	}
	return nil
}

// reconcileOrchestrationPublication 是一个组的发布收尾服务（doc/74 §9.3）。
// 状态机：pending →（准入）publishing →（Provider 实际结果）published / blocked；
// blocked 不盲目自动重放，等用户显式 retry-publication；published 只补事件，不重复写项目。
// 组已停止 / 最终 Task 已不在 publishing（被停止或已收口）→ 放弃：清自己的标记、复位发布状态。
func (s *Store) reconcileOrchestrationPublication(ctx context.Context, orchID string) error {
	var o AgentOrchestration
	if err := s.db.WithContext(ctx).Where("id = ?", orchID).Take(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	switch o.PublicationStatus {
	case publicationStatusPending, publicationStatusPublishing, publicationStatusBlocked:
	default:
		return nil // ''（无发布）或 published（已成事实）：幂等 no-op
	}
	var final AgentTask
	if err := s.db.WithContext(ctx).Where("id = ?", o.FinalTaskID).Take(&final).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return s.abandonPublication(ctx, o)
		}
		return err
	}
	if o.PublicationStatus == publicationStatusBlocked {
		// blocked 且最终 Task 仍带着发布错误码 = 等用户处理，不自动重放（§9.3）。
		// Task 已被停止/复活（retry-publication 会先复位发布状态）→ 放弃残留的 blocked。
		if final.Status == agentTaskStatusBlocked && isPublicationErrorCode(final.ErrorCode) {
			return nil
		}
		return s.abandonPublication(ctx, o)
	}
	if o.Status != orchStatusActive || final.Status != agentTaskStatusPublishing {
		return s.abandonPublication(ctx, o)
	}

	epoch, err := s.admitPublication(ctx, o, final)
	if err != nil {
		return err
	}
	if epoch == 0 {
		return nil // 别的组占着项目，或还有别的活 run：下一轮再试
	}

	// Provider 半（事务外）：读不可变结果 → 有进度记录走 ReconcilePublication，否则 Prepare + Publish。
	ctx, span := startStep(ctx, "workspace_publication")
	span.SetAttributes(
		attribute.String("tako.workspace.mode", o.WorkspaceMode),
		attribute.String("tako.orchestration.id", o.ID),
		attribute.String("tako.task.id", final.ID),
		attribute.Int64("tako.workspace.publication_epoch", epoch),
		attribute.Int("tako.workspace.result_revision", final.ResultRevision),
	)
	outcome, perr := s.runProviderPublication(ctx, o, final, epoch)
	if perr != nil {
		// 操作性失败（I/O、锁竞争）：保持 publishing 与标记，下一轮重试。
		finishSpan(span, perr)
		log.Printf("publication %s epoch %d: provider 失败（保持 publishing 重试）: %v", orchID, epoch, perr)
		return nil
	}
	span.SetAttributes(attribute.String("tako.workspace.publication_outcome", outcome.Status))
	err = s.finishPublication(ctx, o, final, epoch, outcome)
	finishSpan(span, err)
	return err
}

// isPublicationErrorCode 判定通用发布三码（retry-publication 与 blocked 对账共用）。
func isPublicationErrorCode(code string) bool {
	switch code {
	case taskErrWorkspaceChanged, taskErrWorkspaceDirty, taskErrPublicationRecovery:
		return true
	}
	return false
}

// admitPublication 是 pending → publishing 的准入事务（doc/74 §9.1 第 1–3 条）：
// workspace 行 → advisory → 组行复查 → 标记检查 → 排除除最终 run 外的活 run →
// 同事务写 workspaces 标记 + 组 publishing + epoch+1。返回 0 = 本轮条件不满足（不改状态）。
// publishing 的续跑也走这里：标记仍是自己 → 沿用当前 epoch；标记丢失（理论上不该发生）→ 重新准入。
func (s *Store) admitPublication(ctx context.Context, o AgentOrchestration, final AgentTask) (int64, error) {
	var epoch int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ws Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.WorkspaceID).Take(&ws).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // 项目已删（删除保护理论上挡住，防御）：下一轮 abandon 收口
			}
			return err
		}
		if err := lockWorkspacePublicationTx(tx, o.WorkspaceID); err != nil {
			return err
		}
		var locked AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.ID).Take(&locked).Error; err != nil {
			return err
		}
		if locked.Status != orchStatusActive ||
			(locked.PublicationStatus != publicationStatusPending && locked.PublicationStatus != publicationStatusPublishing) {
			return nil // 锁后复查：已被停止或别的副本先收口
		}
		if locked.PublicationStatus == publicationStatusPublishing && ws.PublicationOrchestrationID == o.ID {
			epoch = locked.PublicationEpoch // 续跑：标记仍是自己，沿用当前 epoch
			return nil
		}
		if ws.PublicationOrchestrationID != "" && ws.PublicationOrchestrationID != o.ID {
			return nil // 别的组正在发布这个项目：稍后再试
		}
		// 排除除当前最终 run 外的其他活 run（租约有效才算活）。最终 run 已 complete，
		// 排除它是把话说死：发布窗口内不允许第二个写者。
		var live int64
		finalRunID := ""
		if final.CurrentRunID != nil {
			finalRunID = *final.CurrentRunID
		}
		if err := tx.Raw(`
SELECT COUNT(*) FROM runs
 WHERE workspace_id = ? AND status = 'running' AND lease_until > now() AND id <> ?`,
			o.WorkspaceID, finalRunID).Scan(&live).Error; err != nil {
			return err
		}
		if live > 0 {
			return nil
		}
		epoch = locked.PublicationEpoch + 1
		if err := tx.Model(&Workspace{}).Where("id = ?", o.WorkspaceID).
			Updates(map[string]any{
				"publication_orchestration_id": o.ID,
				"publication_epoch":            epoch,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&AgentOrchestration{}).Where("id = ?", o.ID).
			Updates(map[string]any{
				"publication_status":     publicationStatusPublishing,
				"publication_epoch":      epoch,
				"publication_error_code": "",
				"updated_at":             nowUTC(),
			}).Error
	})
	if err != nil {
		return 0, err
	}
	if epoch > 0 {
		s.PublishEvent(ctx, o.TenantID, eventAgentOrchestrationsChanged, o.ID)
	}
	return epoch, nil
}

// runProviderPublication 是事务外的 Provider 半：不可变结果必须完整（缺失 = 受阻，需人工检查）；
// publication/<epoch>.json 已存在 = 崩溃前已开始发布 → ReconcilePublication 按现场与记录判定；
// 不存在 → PreparePublication 落计划再 Publish。
func (s *Store) runProviderPublication(ctx context.Context, o AgentOrchestration, final AgentTask, epoch int64) (PublishOutcome, error) {
	g, err := s.workspaceGroupHandle(o)
	if err != nil {
		return PublishOutcome{}, err
	}
	provider, err := s.workspaceProvider(g)
	if err != nil {
		return PublishOutcome{}, err
	}
	result, err := readImmutableResult(g, ResultRef{TaskID: final.ID, ResultRevision: final.ResultRevision})
	if err != nil {
		if errors.Is(err, errImmutableResultMissing) {
			return PublishOutcome{
				Status:    publishOutcomeBlocked,
				ErrorCode: taskErrPublicationRecovery,
				Detail:    "不可变结果缺失，需人工检查",
			}, nil
		}
		return PublishOutcome{}, err
	}
	if _, statErr := os.Stat(g.publicationPath(epoch)); statErr == nil {
		return provider.ReconcilePublication(ctx, g, epoch)
	}
	if _, err := provider.PreparePublication(ctx, g, result); err != nil {
		return PublishOutcome{}, err
	}
	return provider.Publish(ctx, g, result, epoch)
}

// finishPublication 是收尾事务：workspace 行 → advisory → 组行复查（epoch 与状态仍是本场）→
// 按 Provider 实际结果落 published / blocked；retry 保持 publishing 与标记，下一轮继续。
// 清标记一律带 orchestration ID + epoch 条件——旧发布者不得清除新标记。
func (s *Store) finishPublication(ctx context.Context, o AgentOrchestration, final AgentTask, epoch int64, outcome PublishOutcome) error {
	if outcome.Status == publishOutcomeRetry {
		return nil
	}
	checkpointed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ws Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.WorkspaceID).Take(&ws).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := lockWorkspacePublicationTx(tx, o.WorkspaceID); err != nil {
			return err
		}
		var locked AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.ID).Take(&locked).Error; err != nil {
			return err
		}
		if locked.PublicationStatus != publicationStatusPublishing || locked.PublicationEpoch != epoch {
			// 已被新一代接管或已收口：只清可能残留的自己的标记，不动任何状态。
			return clearPublicationMarkTx(tx, o.WorkspaceID, o.ID, epoch)
		}
		var lockedFinal AgentTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", final.ID).Take(&lockedFinal).Error; err != nil {
			return err
		}
		if err := clearPublicationMarkTx(tx, o.WorkspaceID, o.ID, epoch); err != nil {
			return err
		}
		if lockedFinal.Status != agentTaskStatusPublishing {
			// Task 在发布窗口内被停止：不复活它。内容若已写成仍记 published 事实
			//（停止不撤销已发布内容）；否则按放弃复位，组状态由取消链收口。
			next := ""
			if outcome.Status == publishOutcomePublished {
				next = publicationStatusPublished
			}
			updates := map[string]any{
				"publication_status":     next,
				"publication_error_code": "",
				"updated_at":             nowUTC(),
			}
			if next == publicationStatusPublished {
				updates["published_at"] = nowUTC()
			}
			return tx.Model(&AgentOrchestration{}).Where("id = ?", o.ID).Updates(updates).Error
		}
		switch outcome.Status {
		case publishOutcomePublished:
			if err := tx.Model(&AgentOrchestration{}).Where("id = ?", o.ID).
				Updates(map[string]any{
					"publication_status":     publicationStatusPublished,
					"publication_error_code": "",
					"published_at":           nowUTC(),
					"updated_at":             nowUTC(),
				}).Error; err != nil {
				return err
			}
			r := tx.Model(&AgentTask{}).Where("id = ?", final.ID).
				Updates(map[string]any{
					"status":        agentTaskStatusSucceeded,
					"error_code":    "",
					"error_message": "",
					"finished_at":   nowUTC(),
				})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected > 0 && final.PlanCheckpoint {
				cp, err := s.applyPlanCheckpointTx(tx, o.TenantID, o.ID, final.ID, string(o.SourceSessionID), final.ResultRevision)
				if err != nil {
					return err
				}
				checkpointed = cp
			}
			return nil
		case publishOutcomeBlocked:
			if err := tx.Model(&AgentOrchestration{}).Where("id = ?", o.ID).
				Updates(map[string]any{
					"publication_status":     publicationStatusBlocked,
					"publication_error_code": outcome.ErrorCode,
					"updated_at":             nowUTC(),
				}).Error; err != nil {
				return err
			}
			return tx.Model(&AgentTask{}).Where("id = ?", final.ID).
				Updates(map[string]any{
					"status":        agentTaskStatusBlocked,
					"error_code":    outcome.ErrorCode,
					"error_message": outcome.Detail,
					"finished_at":   nowUTC(),
				}).Error
		default:
			return fmt.Errorf("未知的发布结果状态：%q", outcome.Status)
		}
	})
	if err != nil {
		return err
	}
	s.PublishEvent(ctx, o.TenantID, eventAgentTasksChanged, final.ID)
	s.PublishEvent(ctx, o.TenantID, eventAgentOrchestrationsChanged, o.ID)
	s.PublishTaskWake(ctx, o.TenantID, "publication", o.ID)
	if checkpointed {
		s.PublishTaskWake(ctx, o.TenantID, "checkpoint", final.ID)
	}
	return nil
}

// clearPublicationMarkTx 按 orchestration ID + epoch 清发布标记：条件不命中 = 标记已不是
// 本场（新发布者接管或已清），空转即幂等。
func clearPublicationMarkTx(tx *gorm.DB, workspaceID, orchID string, epoch int64) error {
	return tx.Model(&Workspace{}).
		Where("id = ? AND publication_orchestration_id = ? AND publication_epoch = ?", workspaceID, orchID, epoch).
		Updates(map[string]any{"publication_orchestration_id": "", "publication_epoch": 0}).Error
}

// abandonPublication 收口一场不再有效的发布（组被停止 / 最终 Task 被停止或已收口）：
// 清自己的标记、复位发布状态。published 是既成事实，永不复位（停止不撤销已发布内容）。
func (s *Store) abandonPublication(ctx context.Context, o AgentOrchestration) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ws Workspace
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.WorkspaceID).Take(&ws).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if err := lockWorkspacePublicationTx(tx, o.WorkspaceID); err != nil {
			return err
		}
		var locked AgentOrchestration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", o.ID).Take(&locked).Error; err != nil {
			return err
		}
		if locked.PublicationStatus == publicationStatusPublished || locked.PublicationStatus == "" {
			return nil
		}
		if err := clearPublicationMarkTx(tx, o.WorkspaceID, o.ID, locked.PublicationEpoch); err != nil {
			return err
		}
		return tx.Model(&AgentOrchestration{}).Where("id = ?", o.ID).
			Updates(map[string]any{
				"publication_status":     "",
				"publication_error_code": "",
				"updated_at":             nowUTC(),
			}).Error
	})
	if err != nil {
		return err
	}
	s.PublishEvent(ctx, o.TenantID, eventAgentOrchestrationsChanged, o.ID)
	return nil
}

// reconcilePublications 是调度慢路径：列出所有带发布状态的组逐个走同一收尾服务。
// blocked 也在列——不是为了重放，而是收口「Task 已被停止但发布状态残留」的情形。
func (ts *TaskScheduler) reconcilePublications(ctx context.Context) {
	var ids []string
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT id FROM agent_orchestrations
 WHERE publication_status IN (?, ?, ?)
 ORDER BY created_at, id`,
		publicationStatusPending, publicationStatusPublishing, publicationStatusBlocked).Scan(&ids).Error; err != nil {
		log.Printf("task scheduler publication list: %v", err)
		return
	}
	for _, id := range ids {
		if err := ts.store.reconcileOrchestrationPublication(ctx, id); err != nil {
			log.Printf("task scheduler publication %s: %v", id, err)
		}
	}
}

// cleanupWorkspaces 过窗清理组的工作现场（doc/74 §10.8；doc/76 §4）：只清终态且过 archiveTTL
// 的组；shared 与 isolated 都有组工作现场（work/shared 或 work/<taskId>），同一判据。
// Provider 自己判定哪些现场能证明安全（未提交内容、进行中 Git 操作、部分发布未收口的保留）。
// 清理失败只记日志，不改业务状态。archiveTTL <= 0（未配置）= 不清理。
func (ts *TaskScheduler) cleanupWorkspaces(ctx context.Context) {
	if ts.worker == nil {
		return // 测试模式调度器没有 worker：清理依赖 archiveTTL 配置，跳过
	}
	ttl := ts.worker.archiveTTL
	if ttl <= 0 {
		return
	}
	var orchs []AgentOrchestration
	if err := ts.store.db.WithContext(ctx).Raw(`
SELECT * FROM agent_orchestrations
 WHERE status IN (?, ?, ?) AND finished_at IS NOT NULL AND finished_at < ?
 ORDER BY finished_at LIMIT 32`,
		orchStatusSucceeded, orchStatusFailed, orchStatusCancelled,
		time.Now().UTC().Add(-ttl)).Scan(&orchs).Error; err != nil {
		log.Printf("task scheduler workspace cleanup list: %v", err)
		return
	}
	for _, o := range orchs {
		g, err := ts.store.workspaceGroupHandle(o)
		if err != nil {
			log.Printf("workspace cleanup %s: %v", o.ID, err)
			continue
		}
		provider, err := ts.store.workspaceProvider(g)
		if err != nil {
			log.Printf("workspace cleanup %s: %v", o.ID, err)
			continue
		}
		report, err := provider.Cleanup(ctx, g, time.Now().UTC(), ttl)
		if err != nil {
			log.Printf("workspace cleanup %s: %v", o.ID, err)
			continue
		}
		if len(report.Removed) > 0 || len(report.Kept) > 0 {
			log.Printf("workspace cleanup %s: removed=%v kept=%v", o.ID, report.Removed, report.Kept)
		}
	}
}