// 本文件是后台命令（doc/82 §6/§7）的对账与清理循环。
//
// 病灶：一条后台命令由「启动它的那个副本」用租约监管（bash_job.go 的 watcher goroutine）。
// 那个副本进程崩了、被 kill -9、或机器断电，watcher 就不再运行，行会永远停在 running——
// 命令列表里那条命令永远「运行中」，来源会话再也收不到它的终态通知，Task 也永远提交不了
// （submit 会一直撞 errSubmitJobsRunning）。
//
// 两件事分开、周期不同：
//   - 对账（每 jobReconcileInterval）：把「租约已过期却仍在 running/stopping」的行收成
//     interrupted 并 epoch+1。仅当 ExecutorID 指向本机时，按 ProcessGroupID 尽力杀残余进程组
//     （别人的进程不归本副本管，也不能被当成本地进程 kill）。不自动重放。
//   - 清理（每 jobCleanupInterval）：updated_at 超过保留期的终态行连输出目录一起删。
//     按主键条件 DELETE、幂等、多副本可同跑；被清理后查询即 404。
//
// 每副本一个循环，正确性由 PG 条件更新与主键 DELETE 承担（不看进程内状态）。
package runengine

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// bashJobRetentionTTL 是后台命令行与输出目录的保留期（与 doc/58 的喷溅落盘同一 30 天）。
const bashJobRetentionTTL = 30 * 24 * time.Hour

// jobCleanupInterval 是清理循环周期。对账 15 秒一轮；清理在一小时一轮即可——保留期以天计，
// 更密只是白扫。
const jobCleanupInterval = time.Hour

// jobSweepBatch 是一轮清理最多删的行数，避免一次大 DELETE 长时间占锁。
const jobSweepBatch = 2000

// StartJobSupervisor 显式启动后台命令的对账与清理循环（施工包 §5 main.go 行 / §7），返回停止函数。
// 与 TaskScheduler 同形：每副本一个、显式 Start、不藏在路由注册里。interval <= 0 只关循环，
// 停止/查询端点照常可用（由 main 决定是否调用本函数）。
func (s *Store) StartJobSupervisor(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		s.reconcileJobs(ctx)
		s.sweepJobs(ctx)
		reconcile := time.NewTicker(jobReconcileInterval)
		cleanup := time.NewTicker(jobCleanupInterval)
		defer reconcile.Stop()
		defer cleanup.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcile.C:
				s.reconcileJobs(ctx)
			case <-cleanup.C:
				s.sweepJobs(ctx)
			}
		}
	}()
	return cancel
}

// reconcileJobs 列出所有「租约已过期却仍在 running/stopping」的行，逐行收成 interrupted。
func (s *Store) reconcileJobs(ctx context.Context) {
	local, _ := sweepOwner()
	var rows []BackgroundJob
	if err := s.db.WithContext(ctx).
		Where("status IN ? AND lease_until < ?",
			[]string{jobStatusRunning, jobStatusStopping}, nowUTC()).Find(&rows).Error; err != nil {
		log.Printf("job reconcile list: %v", err)
		return
	}
	for _, row := range rows {
		wrote, err := s.interruptStaleJob(ctx, row)
		if err != nil {
			log.Printf("job reconcile %s: %v", row.ID, err)
			continue
		}
		if !wrote {
			continue
		}
		s.PublishEvent(ctx, row.TenantID, eventBackgroundJobsChanged, row.ID)
		// 只有本机起的进程才归本副本杀（别人的进程不能当本地进程处理，§7）。best-effort：
		// 进程若已不存在，kill 静默失败；本机若真有残余 watcher，续租写不中时会自行杀。
		if row.ExecutorID == local && row.ProcessGroupID != nil {
			killProcessGroup(*row.ProcessGroupID)
		}
	}
}

// interruptStaleJob 收一行过期 job：锁行 → 锁后复查（本代一致、仍非终态、租约确已过期）→
// 条件 UPDATE 成 interrupted 且 epoch+1；符合条件的（后台身份 + 聊天来源）在同一事务落
// bash_job_finished 通知（与 owner 侧 finishBashJob 同一条规矩）。返回 wrote=false 表示没轮到本副本收。
func (s *Store) interruptStaleJob(ctx context.Context, stale BackgroundJob) (bool, error) {
	wrote := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job BackgroundJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", stale.ID).Take(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // 行已被清理
			}
			return err
		}
		// 锁后复查：可能刚被 owner 续租、刚被收成终态，或已被别的副本对账收走（epoch 变了）。
		if job.Epoch != stale.Epoch || jobTerminal(job.Status) || job.LeaseUntil.After(nowUTC()) {
			return nil
		}
		now := nowUTC()
		res := tx.Model(&BackgroundJob{}).
			Where("id = ? AND epoch = ? AND status IN ?", job.ID, job.Epoch,
				[]string{jobStatusRunning, jobStatusStopping}).
			Updates(map[string]any{
				"status": jobStatusInterrupted, "epoch": job.Epoch + 1,
				"ended_at": &now, "updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		wrote = true
		// 通知去重键用**本代**（出错的那一代）epoch：与 owner 侧 finishBashJob 同一口径，
		// 一条命令只会有一条终态通知。
		if job.Backgrounded && job.SourceTaskID == "" {
			return s.insertJobNotificationTx(tx, job)
		}
		return nil
	})
	return wrote, err
}

// sweepJobs 删掉超过保留期的终态行，并删它们的输出目录（行与文件同周期，§7）。
// 条件只认终态：running/stopping 的行即便 updated_at 很旧，也先由对账收口，不由清理直接删除。
func (s *Store) sweepJobs(ctx context.Context) {
	cutoff := nowUTC().Add(-bashJobRetentionTTL)
	var rows []BackgroundJob
	if err := s.db.WithContext(ctx).
		Where("updated_at < ? AND status IN ?", cutoff,
			[]string{jobStatusSucceeded, jobStatusFailed, jobStatusStopped, jobStatusInterrupted}).
		Limit(jobSweepBatch).Find(&rows).Error; err != nil {
		log.Printf("job cleanup list: %v", err)
		return
	}
	for _, row := range rows {
		// 先删目录再删行：目录删不掉（权限等）不该挡住行清理——行删了查询就是 404。
		if phys, err := s.resolveJobOutputPath(jobViewOf(row), row.TenantID, row.StdoutRelPath); err == nil && phys != "" {
			if err := os.RemoveAll(filepath.Dir(phys)); err != nil {
				log.Printf("job cleanup %s: remove dir: %v", row.ID, err)
			}
		}
		if err := s.db.WithContext(ctx).Where("id = ?", row.ID).Delete(&BackgroundJob{}).Error; err != nil {
			log.Printf("job cleanup %s: delete: %v", row.ID, err)
		}
	}
}
