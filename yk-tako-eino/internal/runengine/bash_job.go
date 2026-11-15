// 后台命令（doc/82 §6；施工包 §6）的存储、登记路径、生命周期与产品读/停端点。
//
// 病灶（doc/82 §1）：bash 的 30 秒上限同时是前台等待与进程总寿命——npm run build 这类
// 长命令到点被杀，只回一句「超时，死前输出」。改后：
//   - job 表（背景命令的持久身份）+ 一次工具调用一行（幂等键 (run, toolCallID)）；
//   - 输出从首字节落盘到 job 固定目录（stdout.out/stderr.out），供 /output 分页读；
//   - **前台等待窗口与进程总寿命解耦**：工具最多前台等 bashForegroundWait，到点未完返回
//     `bj_…` 命令号，命令继续在后台跑（进程用 context.WithoutCancel，不随工具返回或 run 结束而死）；
//   - 单 owner + 租约：owner 起 watcher 接住进程退出、续租、轮询停止请求、杀进程组，
//     终态与 bash_job_finished 通知同一事务写；其他副本可读、可写停止请求，对账循环收 interrupted。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"yk-tako-eino/internal/observability"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// errJobNotFound：命令不存在、不可见或跨租户——统一 404 117117，不给探测通道。
	errJobNotFound = errors.New("background job not found")
	// errJobNotStoppable：仍处 stopping 时重复停止（409 117118）。已终态重复停止不是错误，回当前态。
	errJobNotStoppable = errors.New("background job not stoppable")
)

// 后台命令的工程参数（施工包 §4 拍定值）：由 main 用旗标注入，都有生产默认值，单测直接依赖默认值。
var (
	// bashForegroundWait 是 bash 工具的前台等待窗口（旗标 -bash-foreground-wait，默认 30s）：
	// 窗口内完成沿用旧结果字节；到点未完返回命令号，命令继续在后台跑。
	bashForegroundWait = 30 * time.Second
	// waitWindow 是 wait_task 与 bash_output(wait=true) 共用的等待窗口（旗标 -wait-window，默认 30s）。
	waitWindow = 30 * time.Second
	// jobLeaseTTL 是后台命令的租约时长（施工包 §4 拍定 30s）。owner 存活期间持续续租；
	// 对账循环把「租约已过期仍在跑」的行收成 interrupted。多副本共存时它是「谁是 owner」的时间判据。
	jobLeaseTTL = 30 * time.Second
	// jobLeaseRenew 是 owner 的续租周期（旗标 -job-lease-renew，默认 10s，须 < jobLeaseTTL）。
	jobLeaseRenew = 10 * time.Second
	// jobReconcileInterval 是对账循环周期（旗标 -job-reconcile-interval，默认 15s）。
	jobReconcileInterval = 15 * time.Second
)

// jobStopPollInterval 是 owner 检查停止请求的周期（代码常量，不设旗标）。
const jobStopPollInterval = 2 * time.Second

// ConfigureBashJobs 由 main 注入五个时长旗标（施工包 §5）。只接受正数，缺省用上面的默认值。
func ConfigureBashJobs(foregroundWait, wait, leaseTTL, leaseRenew, reconcile time.Duration) {
	if foregroundWait > 0 {
		bashForegroundWait = foregroundWait
	}
	if wait > 0 {
		waitWindow = wait
	}
	if leaseTTL > 0 {
		jobLeaseTTL = leaseTTL
	}
	if leaseRenew > 0 {
		jobLeaseRenew = leaseRenew
	}
	if reconcile > 0 {
		jobReconcileInterval = reconcile
	}
}

// jobView 是列表行与详情共用的形状（施工包 §6.3，键序固定；字段恒在，不适用用 null/空串）。
// 路径是相对运行资源根的逻辑路径，前端不拼宿主绝对路径。
type jobView struct {
	JobID                 string     `json:"jobId"`
	WorkspaceID           string     `json:"workspaceId"`
	SourceSessionID       string     `json:"sourceSessionId"`
	SourceRunID           string     `json:"sourceRunId"`
	SourceTaskID          string     `json:"sourceTaskId"`
	SourceOrchestrationID string     `json:"sourceOrchestrationId"`
	Status                string     `json:"status"`
	ExitCode              *int       `json:"exitCode"`
	StopRequested         bool       `json:"stopRequested"`
	Backgrounded          bool       `json:"backgrounded"`
	Command               string     `json:"command"`
	StdoutPath            string     `json:"stdoutPath"`
	StderrPath            string     `json:"stderrPath"`
	CreatedAt             time.Time  `json:"createdAt"`
	StartedAt             *time.Time `json:"startedAt"`
	EndedAt               *time.Time `json:"endedAt"`
}

// jobOutputView 是 /output 与 bash_output 的共用形状（施工包 §6.3；键序与 §6.2 同形同序）。
type jobOutputView struct {
	JobID      string `json:"jobId"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	StdoutPath string `json:"stdoutPath"`
	StderrPath string `json:"stderrPath"`
}

// jobStopView 是停止端点/工具的响应体（§6.3）。
type jobStopView struct {
	JobID         string `json:"jobId"`
	Status        string `json:"status"`
	StopRequested bool   `json:"stopRequested"`
}

func jobViewOf(j BackgroundJob) jobView {
	return jobView{
		JobID:                 j.ID,
		WorkspaceID:           j.WorkspaceID,
		SourceSessionID:       j.SourceSessionID,
		SourceRunID:           j.SourceRunID,
		SourceTaskID:          j.SourceTaskID,
		SourceOrchestrationID: j.SourceOrchestrationID,
		Status:                j.Status,
		ExitCode:              j.ExitCode,
		StopRequested:         j.StopRequested,
		Backgrounded:          j.Backgrounded,
		Command:               j.Command,
		StdoutPath:            j.StdoutRelPath,
		StderrPath:            j.StderrRelPath,
		CreatedAt:             j.CreatedAt,
		StartedAt:             j.StartedAt,
		EndedAt:               j.EndedAt,
	}
}

// jobOwnerQuery 是 job 可见性链的公共 SQL：background_jobs → 来源会话 → 本人。
// 非本人与不存在同形（404 117117）。项目管理员（owner/admin）另走一条：同租户全可读。
const jobOwnerQuery = `
SELECT j.* FROM background_jobs j
  JOIN sessions s ON s.id = j.source_session_id
 WHERE j.tenant_id = ? AND s.user_id = ? AND s.deleted_at IS NULL`

// jobRow 读一行 job 并按可见性判定；不可见/不存在都归 errJobNotFound。
func (s *Store) jobRow(ctx context.Context, p principal, jobID string) (BackgroundJob, error) {
	var rows []BackgroundJob
	if roleRank(p.Role) >= roleRank("admin") {
		if err := s.db.WithContext(ctx).
			Where("id = ? AND tenant_id = ?", jobID, p.TenantID).Limit(1).Find(&rows).Error; err != nil {
			return BackgroundJob{}, err
		}
	} else {
		if err := s.db.WithContext(ctx).Raw(jobOwnerQuery+` AND j.id = ?`, p.TenantID, p.UserID, jobID).
			Scan(&rows).Error; err != nil {
			return BackgroundJob{}, err
		}
	}
	if len(rows) == 0 {
		return BackgroundJob{}, errJobNotFound
	}
	return rows[0], nil
}

// GetJob 读一条命令（施工包 §6.3）。
func (s *Store) GetJob(ctx context.Context, p principal, jobID string) (jobView, error) {
	j, err := s.jobRow(ctx, p, jobID)
	if err != nil {
		return jobView{}, err
	}
	return jobViewOf(j), nil
}

// ListWorkspaceJobs 列本人项目下的命令（施工包 §6.3）：默认列表 = backgrounded=true 或状态
// ∈ {running, stopping}；sessionId 可选过滤。没有项目的 job 不进任何项目列表。
func (s *Store) ListWorkspaceJobs(ctx context.Context, p principal, workspaceID, sessionID string) ([]jobView, error) {
	var one int
	if err := s.db.WithContext(ctx).Raw(
		`SELECT 1 FROM workspaces WHERE id = ? AND tenant_id = ? AND user_id = ?`,
		workspaceID, p.TenantID, p.UserID).Scan(&one).Error; err != nil {
		return nil, err
	}
	if one == 0 {
		return nil, ErrWorkspaceNotFound
	}
	q := s.db.WithContext(ctx).
		Where("tenant_id = ? AND workspace_id = ?", p.TenantID, workspaceID).
		Where("backgrounded = ? OR status IN ?", true, []string{jobStatusRunning, jobStatusStopping})
	if sessionID != "" {
		q = q.Where("source_session_id = ?", sessionID)
	}
	var rows []BackgroundJob
	if err := q.Order("created_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]jobView, 0, len(rows))
	for _, r := range rows {
		views = append(views, jobViewOf(r))
	}
	return views, nil
}

// StopJob 请求停止一条命令（施工包 §4-2/§6.3）。
//
//   - running：条件 UPDATE 写成 stopping + stop_requested=true，回 (stopping, true)；
//   - 已终态：幂等，回当前终态 + stopRequested=false；
//   - 仍处 stopping：409 117118（errJobNotStoppable）。
//
// 本步只写库：进程组的杀由 owner（下一步）看到 stop_requested 后执行。
func (s *Store) StopJob(ctx context.Context, p principal, jobID string) (string, bool, error) {
	j, err := s.jobRow(ctx, p, jobID)
	if err != nil {
		return "", false, err
	}
	return s.stopJobStatus(ctx, j)
}

// stopJobStatus 是停止状态机的落库半（§6.2/§6.3 同源）：HTTP 端点（StopJob）与 stop_bash 工具
// 共用同一份——可见性各自判定（端点走项目链，工具走 run 归属），状态迁移只有这里一处。
func (s *Store) stopJobStatus(ctx context.Context, j BackgroundJob) (string, bool, error) {
	switch j.Status {
	case jobStatusStopping:
		return "", false, errJobNotStoppable
	case jobStatusRunning:
		res := s.db.WithContext(ctx).Model(&BackgroundJob{}).
			Where("id = ? AND status = ?", j.ID, jobStatusRunning).
			Updates(map[string]any{"stop_requested": true, "status": jobStatusStopping})
		if res.Error != nil {
			return "", false, res.Error
		}
		if res.RowsAffected > 0 {
			return jobStatusStopping, true, nil
		}
		// 并发下已被 owner 收成终态：回读当前态（幂等）。
		cur, err := s.jobByID(ctx, j.ID)
		if err != nil {
			return "", false, err
		}
		return cur.Status, false, nil
	default: // 终态（succeeded/failed/stopped/interrupted）
		return j.Status, false, nil
	}
}

// jobByID 按主键读一行 job（不做可见性判定）：工具路径与并发回读用，可见性由调用方判。
func (s *Store) jobByID(ctx context.Context, jobID string) (BackgroundJob, error) {
	var rows []BackgroundJob
	if err := s.db.WithContext(ctx).Where("id = ?", jobID).Limit(1).Find(&rows).Error; err != nil {
		return BackgroundJob{}, err
	}
	if len(rows) == 0 {
		return BackgroundJob{}, errJobNotFound
	}
	return rows[0], nil
}

// finishBashJob 写一条命令的终态（status + exitCode + endedAt），并（当这条命令以后台身份回过
// 调用方、且是聊天来源时）**在同一事务里**落一条 bash_job_finished 通知（§6.5）。
//
// 条件写：epoch 必须与本代一致（旧代 owner 的写不再命中）、状态必须是未收口（running/stopping）。
// 期间收到过停止请求的，终态一律记 stopped、exitCode 置空——用户要求停，不把结果当成功（§4-2）。
// 返回 wrote=false 表示没写到（旧代 / 已被对账收成终态），调用方据此不发事件。
func (s *Store) finishBashJob(ctx context.Context, jobID string, epoch int, status string, exitCode *int) (bool, error) {
	wrote := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job BackgroundJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", jobID).Take(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // 行被清理了：不写、不发
			}
			return err
		}
		if job.Epoch != epoch || jobTerminal(job.Status) {
			return nil // 旧代或已终态：不覆盖
		}
		if job.StopRequested {
			status, exitCode = jobStatusStopped, nil
		}
		now := nowUTC()
		res := tx.Model(&BackgroundJob{}).
			Where("id = ? AND epoch = ?", jobID, epoch).
			Updates(map[string]any{"status": status, "exit_code": exitCode, "ended_at": &now, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		wrote = true
		if job.Backgrounded && job.SourceTaskID == "" {
			return s.insertJobNotificationTx(tx, job)
		}
		return nil
	})
	return wrote, err
}

// insertJobNotificationTx 在终态那笔事务里落 bash_job_finished 通知（§6.5）：去重键
// bash:<jobId>:<epoch>，接收会话 = 来源会话，background_job_id 指向本 job（认领按它找）。
func (s *Store) insertJobNotificationTx(tx *gorm.DB, job BackgroundJob) error {
	bid := job.ID
	return s.insertNotificationTx(tx, AgentNotification{
		TenantID:          job.TenantID,
		BackgroundJobID:   &bid,
		ReceiverSessionID: SessionID(job.SourceSessionID),
		Kind:              notifKindBashJobFinished,
		DedupKey:          fmt.Sprintf("%s:%s:%d", notifKindBashJobFinished, job.ID, job.Epoch),
		Status:            notifStatusPending,
	})
}

// registerBashJob 为一次真实 bash 工具调用登记一行（施工包 §6.4）。
//
// 返回 hasRow=false 表示 ctx 没有 run 身份（旁路调用，如 dev 端点之外的探针）——照跑不登记。
// 有 run 身份但取不到工具调用坐标时返回错误：幂等键拼不出来，不登记也不执行（§6.4）。
// 幂等键 (source_run_id, tool_call_id) 的唯一索引就是「一次工具调用一行」的闸：重复插入回读既有行。
//
// backgrounded 是调用方起手就知道的「以后台身份返回」意图（显式 runInBackground）；前台超时
// 转后台由 markJobBackgrounded 事后翻。StartedAt 留空，等 cmd.Start() 成功、拿到进程组号再写。
func (s *Store) registerBashJob(ctx context.Context, workspace, command string, backgrounded bool) (BackgroundJob, bool, error) {
	box := taskBoxFrom(ctx)
	p, ok := principalFromCtx(ctx)
	if box == nil || !ok {
		return BackgroundJob{}, false, nil
	}
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return BackgroundJob{}, false, errors.New("bash 不可用（没有工具调用坐标）")
	}
	existing := func() (BackgroundJob, bool, error) {
		var rows []BackgroundJob
		if err := s.db.WithContext(ctx).
			Where("source_run_id = ? AND tool_call_id = ?", string(box.run), callID).
			Limit(1).Find(&rows).Error; err != nil {
			return BackgroundJob{}, false, err
		}
		if len(rows) > 0 {
			return rows[0], true, nil
		}
		return BackgroundJob{}, false, nil
	}
	if j, found, err := existing(); err != nil {
		return BackgroundJob{}, false, err
	} else if found {
		return j, true, nil
	}
	id, err := newRowID("bj_")
	if err != nil {
		return BackgroundJob{}, false, err
	}
	wid := ""
	if ref, ok := workspaceFrom(ctx); ok {
		wid = ref.WorkspaceID
	}
	if wid == "" {
		if wid, err = s.RunWorkspace(ctx, box.run); err != nil {
			return BackgroundJob{}, false, err
		}
	}
	sourceTaskID, sourceOrchID := "", ""
	if tb := agentTaskRunBoxFrom(ctx); tb != nil {
		sourceTaskID, sourceOrchID = tb.TaskID, tb.OrchestrationID
	}
	exec, err := sweepOwner()
	if err != nil {
		return BackgroundJob{}, false, err
	}
	stdoutRel, stderrRel := jobOutputRelPaths(ctx, workspace, id)
	now := nowUTC()
	job := BackgroundJob{
		ID:                    id,
		TenantID:              p.TenantID,
		WorkspaceID:           wid,
		SourceSessionID:       string(box.session),
		SourceRunID:           string(box.run),
		ToolCallID:            callID,
		SourceTaskID:          sourceTaskID,
		SourceOrchestrationID: sourceOrchID,
		Command:               command,
		Status:                jobStatusRunning,
		Backgrounded:          backgrounded,
		ExecutorID:            exec,
		Epoch:                 1,
		LeaseUntil:            now.Add(jobLeaseTTL),
		StdoutRelPath:         stdoutRel,
		StderrRelPath:         stderrRel,
		CreatedAt:             now,
	}
	if err := s.db.WithContext(ctx).Create(&job).Error; err != nil {
		// 并发撞唯一索引 → 回读赢家（同一工具调用只应有一行）。
		if j, found, e2 := existing(); e2 == nil && found {
			return j, true, nil
		}
		return BackgroundJob{}, false, err
	}
	return job, true, nil
}

// markJobStarted 记下启动确认：进程组号（= 子进程 pid）与启动时刻。带 epoch 条件——只认本代 owner。
func (s *Store) markJobStarted(ctx context.Context, jobID string, epoch int, pgid int64) error {
	now := nowUTC()
	return s.db.WithContext(ctx).Model(&BackgroundJob{}).
		Where("id = ? AND epoch = ?", jobID, epoch).
		Updates(map[string]any{"process_group_id": pgid, "started_at": &now, "updated_at": now}).Error
}

// markJobBackgrounded 把一条已登记的命令翻成「以后台身份返回过」（前台超时转后台或显式后台）。
// 只有它才写 bash_job_finished 通知、才进默认命令列表（§6.5）。
func (s *Store) markJobBackgrounded(ctx context.Context, jobID string, epoch int) error {
	now := nowUTC()
	return s.db.WithContext(ctx).Model(&BackgroundJob{}).
		Where("id = ? AND epoch = ?", jobID, epoch).
		Updates(map[string]any{"backgrounded": true, "updated_at": now}).Error
}

// jobStopRequested 读一条命令是否已被请求停止（owner 轮询用）。读不到行 = false（行已被清理）。
func (s *Store) jobStopRequested(ctx context.Context, jobID string) bool {
	var row struct {
		StopRequested bool `gorm:"column:stop_requested"`
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT stop_requested FROM background_jobs WHERE id = ?`, jobID).Scan(&row).Error; err != nil {
		return false
	}
	return row.StopRequested
}

// jobLeaseLost 报告「租约是否已确定不属于本次执行」：owner 存活期间每隔 jobLeaseRenew
// 把 lease_until 推后。带 epoch 与状态条件——被对账收成 interrupted（epoch+1）或已终态后
// 写不中（RowsAffected==0），返回 true，owner 据此停止续租并杀本地进程组。
// DB 侧错误（瞬时抖动）返回 false：租约归属未知，不杀健康长命令，下一 tick 重试续租；
// 若租约真的在此期间被对账收走，最迟 TTL 过后对账会再收一次，不会永久漏杀。
func (s *Store) jobLeaseLost(ctx context.Context, jobID string, epoch int) bool {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE background_jobs SET lease_until = ?, updated_at = ?
		  WHERE id = ? AND epoch = ? AND status IN ?`,
		nowUTC().Add(jobLeaseTTL), nowUTC(), jobID, epoch,
		[]string{jobStatusRunning, jobStatusStopping})
	if res.Error != nil {
		log.Printf("bash job %s: lease renew write failed (retry next tick): %v", jobID, res.Error)
		return false
	}
	return res.RowsAffected == 0
}

// runBashJob 是 bash 工具的新 handler（施工包 §6.1）：前台等待窗口 + 登记 + 转后台返回。
//
// 与 runBash 的关键差别是**进程寿命不再等于工具调用寿命**：
//   - 进程跑在 context.WithoutCancel(ctx) 上，不随工具返回或 run 结束而死（§7 前台窗口语义）；
//   - 工具最多前台等 bashForegroundWait：窗口内完成 → 沿用旧结果字节（§13 字节不变）；
//     到点未完 → 返回 {"jobId","status":"running","reason":"foreground_timeout"}，命令继续在后台跑；
//     显式 runInBackground → 启动确认后立即返回 {"jobId","status":"running","reason":"requested"}。
//   - 终态、通知、事件由 watcher goroutine 收尾，不挂在工具调用上。
func runBashJob(ctx context.Context, store *Store, fallbackWorkspace string, in bashIn) (string, error) {
	workspace := activeRoot(ctx, fallbackWorkspace)
	job, hasRow, err := store.registerBashJob(ctx, workspace, in.Command, in.RunInBackground)
	if err != nil {
		return "", err
	}
	// 清扫过期落盘（doc/58 §4.4）：每次 bash 调用前扫一次。
	sweepBashSpills(ctx, workspace, time.Now())

	// 进程上下文：保留 ctx 的值（沙箱根、工具根、Task 身份盒），但丢掉取消——命令不随工具返回死。
	procCtx := context.WithoutCancel(ctx)
	// 有登记行 → job 固定目录（jobs/<bj>/，固定文件名），从首字节落盘，供 /output 直接读；
	// 无登记行（旁路调用）→ 沿用 run 级落盘，字节与位置都不变。
	var spill *bashSpill
	if hasRow {
		spill = newJobSpill(procCtx, workspace, job.ID)
	} else {
		spill = newBashSpill(procCtx, workspace)
	}
	stdout := &tailCollector{cap: bashOutputCap, spill: spill, ext: "out"}
	stderr := &tailCollector{cap: bashOutputCap, spill: spill, ext: "err"}
	if hasRow {
		stdout.fileName, stderr.fileName = jobStdoutFileName, jobStderrFileName
		stdout.force, stderr.force = true, true
	}
	// timeout=0：命令不设进程寿命上限；前台窗口由下面的 select 计时器管。
	cmd, cleanup, _, _, err := startBash(procCtx, workspace, in.Command, 0, true)
	if err != nil {
		if hasRow {
			_, _ = store.finishBashJob(ctx, job.ID, job.Epoch, jobStatusFailed, nil)
		}
		return "", err
	}
	cmd.Env = append(cmd.Env, "PATH="+subprocessPath(workspace, toolsRootFor(procCtx, workspace)))
	for _, k := range bashEnvWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if tb := agentTaskRunBoxFrom(procCtx); tb != nil && tb.WorkspaceProvider == workspaceProviderGit {
		cmd.Env = append(cmd.Env, gitSandboxEnv()...)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		cleanup()
		if hasRow {
			_, _ = store.finishBashJob(ctx, job.ID, job.Epoch, jobStatusFailed, nil)
		}
		return "", fmt.Errorf("bash 启动失败: %w", err)
	}
	pgid := int64(cmd.Process.Pid) // Setpgid=true → 子进程自成进程组，pgid = pid
	if hasRow {
		if err := store.markJobStarted(ctx, job.ID, job.Epoch, pgid); err != nil {
			log.Printf("bash job %s: mark started: %v", job.ID, err)
		}
	}

	// watcher：接住进程退出、写终态（+通知），存活期间续租并轮询停止请求。
	// 它的寿命不挂在工具调用或 run 上——这正是「命令活过 30 秒」的实现（§7）。
	done := make(chan error, 1)
	go func() {
		werr := watchBashProcess(procCtx, store, hasRow, job, pgid, cmd)
		cleanup()
		stdout.closeSpill()
		stderr.closeSpill()
		done <- werr
	}()

	if !hasRow {
		// 旁路调用（无 run 身份）：没有命令号可回，等它跑完给旧结果字节。
		return bashForegroundResult(<-done, stdout, stderr, workspace)
	}
	if in.RunInBackground {
		// 显式后台：启动确认即返回（登记时 Backgrounded 已为 true，watcher 归位落终态）。
		store.PublishEvent(ctx, job.TenantID, eventBackgroundJobsChanged, job.ID)
		return backgroundAck(job.ID, "requested"), nil
	}
	select {
	case werr := <-done:
		return bashForegroundResult(werr, stdout, stderr, workspace)
	case <-time.After(bashForegroundWait):
		// 到点再看一眼：正好这一瞬退出 → 当窗口内完成（字节不变）；仍活着 → 转后台。
		select {
		case werr := <-done:
			return bashForegroundResult(werr, stdout, stderr, workspace)
		default:
		}
		if err := store.markJobBackgrounded(ctx, job.ID, job.Epoch); err != nil {
			log.Printf("bash job %s: mark backgrounded: %v", job.ID, err)
		}
		store.PublishEvent(ctx, job.TenantID, eventBackgroundJobsChanged, job.ID)
		return backgroundAck(job.ID, "foreground_timeout"), nil
	}
}

// backgroundAck 是「命令转入后台」的工具回执（§6.1）：键序 jobId → status → reason。
func backgroundAck(jobID, reason string) string {
	b, err := json.Marshal(struct {
		JobID  string `json:"jobId"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}{JobID: jobID, Status: jobStatusRunning, Reason: reason})
	if err != nil {
		return fmt.Sprintf(`{"jobId":%q,"status":"running","reason":%q}`, jobID, reason)
	}
	return string(b)
}

// bashForegroundResult 渲染「前台窗口内完成」的结果文本，与旧 runBashJob 逐字同形（§13 字节不变）。
// runErr 非退出错误（启动失败之外）罕见；退出错误按退出码渲染。
func bashForegroundResult(runErr error, stdout, stderr *tailCollector, workspace string) (string, error) {
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			return "", fmt.Errorf("bash 启动失败: %w", runErr)
		}
		code = ee.ExitCode()
	}
	out := fmt.Sprintf("退出码：%d\n%s", code, outputSections(stdout, stderr))
	if strings.Contains(out, "Operation not permitted") || strings.Contains(out, "not permitted") {
		out += fmt.Sprintf("\n（沙箱围栏：只能在项目目录 %s 内读写；越出这个范围的路径已被拒绝——请改用项目内的路径。）", workspace)
	}
	return out, nil
}

// watchBashProcess 是 owner 侧 watcher：等进程退出（存活期间续租、轮询停止请求并杀组），
// 退出后写终态（+通知，同一事务）。返回 cmd.Wait 的错误供前台路径渲染结果文本。
// hasRow=false（旁路调用）时不做任何 DB 操作，只等退出。
func watchBashProcess(ctx context.Context, store *Store, hasRow bool, job BackgroundJob, pgid int64, cmd *exec.Cmd) error {
	if !hasRow {
		return cmd.Wait()
	}
	// job 生命周期独立 span（§6.6）：创建点在工具调用（procCtx 带着工具 span），但寿命不挂在
	// 工具返回或 run 结束上——命令前台超时转后台后，这条 span 仍在，直到进程退出写终态才 End。
	// 只记 job id／来源 run·Task／执行者实例／终态与退出码；stdout 不进 trace。
	ctx, span := observability.Tracer().Start(ctx, "bash_job", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.String("tako.bash_job.id", job.ID),
		attribute.String("tako.bash_job.source_run_id", job.SourceRunID),
		attribute.String("tako.bash_job.source_task_id", job.SourceTaskID),
		attribute.String("tako.bash_job.workspace_id", job.WorkspaceID),
		attribute.String("tako.bash_job.executor_id", job.ExecutorID),
	)
	defer span.End()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	renew := time.NewTicker(jobLeaseRenew)
	stopPoll := time.NewTicker(jobStopPollInterval)
	defer renew.Stop()
	defer stopPoll.Stop()
	for {
		select {
		case werr := <-exited:
			status, exitCode := classifyBashJobExit(werr)
			span.SetAttributes(attribute.String("tako.bash_job.status", status))
			if exitCode != nil {
				span.SetAttributes(attribute.Int("tako.bash_job.exit_code", *exitCode))
			}
			wrote, err := store.finishBashJob(ctx, job.ID, job.Epoch, status, exitCode)
			if err != nil {
				log.Printf("bash job %s: finish: %v", job.ID, err)
			}
			if wrote {
				store.PublishEvent(ctx, job.TenantID, eventBackgroundJobsChanged, job.ID)
			}
			return werr
		case <-renew.C:
			if store.jobLeaseLost(ctx, job.ID, job.Epoch) {
				// 租约确定已换手（被对账接管成 interrupted / 行已终态）：杀本地进程组停更，
				// 终态由对账收口；这里不再写（epoch 条件也会挡住）。
				// DB 写失败不算——归属未知不杀，下一 tick 重试续租。
				killProcessGroup(pgid)
			}
		case <-stopPoll.C:
			if store.jobStopRequested(ctx, job.ID) {
				killProcessGroup(pgid)
			}
		}
	}
}

// classifyBashJobExit 把进程退出结果落成 job 终态与退出码（§6.3）：0 → succeeded(0)；
// 非零 → failed(code)；信号杀死（退出码 -1）→ failed 且 exitCode 为 null（不把 -1 伪装成业务结果）。
func classifyBashJobExit(runErr error) (string, *int) {
	if runErr == nil {
		zero := 0
		return jobStatusSucceeded, &zero
	}
	if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() != 0 {
		code := ee.ExitCode()
		return jobStatusFailed, &code
	}
	return jobStatusFailed, nil
}

// jobOutput 组装一条命令的输出视图（施工包 §6.3）：stdout/stderr 取两路文件的当前尾部
// 64,000 字节；路径恒是登记时算好的逻辑相对路径（文件还没落成时输出为空串，路径照给）。
// 是 Store 方法（用 workspaceRoot = 作业根）——HTTP 端点与 bash_output 工具共用同一份读取。
func (s *Store) jobOutput(j jobView, tenantID string) (jobOutputView, error) {
	stdout, err := s.readJobOutputFile(j, tenantID, j.StdoutPath)
	if err != nil {
		return jobOutputView{}, err
	}
	stderr, err := s.readJobOutputFile(j, tenantID, j.StderrPath)
	if err != nil {
		return jobOutputView{}, err
	}
	return jobOutputView{
		JobID:      j.JobID,
		Status:     j.Status,
		ExitCode:   j.ExitCode,
		Stdout:     stdout,
		Stderr:     stderr,
		StdoutPath: j.StdoutPath,
		StderrPath: j.StderrPath,
	}, nil
}

// readJobOutputFile 把逻辑相对路径解析成物理路径并读其尾部 ≤cap 字节；文件不存在 = 空串。
func (s *Store) readJobOutputFile(j jobView, tenantID, rel string) (string, error) {
	phys, err := s.resolveJobOutputPath(j, tenantID, rel)
	if err != nil {
		return "", err
	}
	if phys == "" {
		return "", nil
	}
	return readTailFile(phys, bashOutputCap)
}

// resolveJobOutputPath 把 job 输出的逻辑相对路径解析成物理路径（施工包 §6.3：API 不接受
// 调用方传任意路径，一切从登记时的相对路径出发）。
//
//	有项目 → runtime/... 落到项目容器的 runtime/ 下；
//	无项目 → .tako/... 落到会话目录下的 .tako/。
func (s *Store) resolveJobOutputPath(j jobView, tenantID, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if s.workspaceRoot == "" {
		return "", os.ErrInvalid
	}
	slash := filepath.ToSlash(rel)
	if j.WorkspaceID != "" {
		runtimeDir, err := projectRuntimeDir(s.workspaceRoot, tenantID, j.WorkspaceID)
		if err != nil {
			return "", err
		}
		rest := strings.TrimPrefix(slash, "runtime/")
		if rest == slash {
			return "", os.ErrInvalid
		}
		return resolveInWorkspace(runtimeDir, rest)
	}
	base := filepath.Join(s.workspaceRoot, "tenants", tenantID, "sessions", j.SourceSessionID, ".tako")
	rest := strings.TrimPrefix(slash, ".tako/")
	if rest == slash {
		return "", os.ErrInvalid
	}
	return resolveInWorkspace(base, rest)
}

// readTailFile 读一个文件末尾 ≤cap 字节；不存在 = 空串（命令还没产出已落盘的内容）。
func readTailFile(path string, capBytes int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > int64(capBytes) {
		start = info.Size() - int64(capBytes)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// listWorkspaceJobs = GET /api/workspaces/:id/jobs?sessionId=…（施工包 §6.3）。
func (s *Server) listWorkspaceJobs(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	views, err := s.store.ListWorkspaceJobs(c.Request.Context(), p, c.Param("id"), c.Query("sessionId"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"jobs": views})
}

// getJob = GET /api/jobs/:id（施工包 §6.3）。
func (s *Server) getJob(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetJob(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", view)
}

// getJobOutput = GET /api/jobs/:id/output（施工包 §6.3，与 bash_output 同形同序）。
func (s *Server) getJobOutput(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	view, err := s.store.GetJob(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	out, err := s.store.jobOutput(view, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", out)
}

// stopJob = POST /api/jobs/:id/stop（施工包 §6.3；写面挂 RequireTenantRole("member")）。
func (s *Server) stopJob(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	status, stopRequested, err := s.store.StopJob(c.Request.Context(), p, c.Param("id"))
	if err != nil {
		if taskStoreErr(c, err) {
			return
		}
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", jobStopView{
		JobID: c.Param("id"), Status: status, StopRequested: stopRequested,
	})
}

// jobVisibleToRun 报告一条命令是否归属当前 run（施工包 §6.2/§8）：模型不能自报租户或会话，
// 归属只看 ctx 的来源会话或来源 Task。HTTP 端点另走项目可见性链（jobRow）。
func jobVisibleToRun(ctx context.Context, j BackgroundJob) bool {
	if box := taskBoxFrom(ctx); box != nil && string(box.session) == j.SourceSessionID {
		return true
	}
	if tb := agentTaskRunBoxFrom(ctx); tb != nil && tb.TaskID != "" && j.SourceTaskID == tb.TaskID {
		return true
	}
	return false
}

// pollJobUntilTerminal 在 waitWindow 内轮询一条命令到终态（§4-3：主键读、不加锁、起始 200ms
// 指数退避到 1s）；到点仍非终态返回当前行。命中终态（含租约过期的 interrupted 派生）立即返回。
func (s *Store) pollJobUntilTerminal(ctx context.Context, j BackgroundJob) (BackgroundJob, error) {
	deadline := time.Now().Add(waitWindow)
	interval := 200 * time.Millisecond
	for {
		if jobTerminal(j.Status) || !time.Now().Before(deadline) {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(interval):
		}
		next, err := s.jobByID(ctx, j.ID)
		if err != nil {
			return j, err
		}
		j = next
		if interval < time.Second {
			interval *= 2
			if interval > time.Second {
				interval = time.Second
			}
		}
	}
}

// claimPendingJobNotification 在来源 run 里读到 job 终态后就地认领该 job 的 pending 通知
// （§6.5）：按 background_job_id 找本 job 的 pending 行、接收会话 == 当前 run 会话才认领；
// ClaimNotificationInline 写 claimed_in_place、不铸新 run。没有通知（前台短命令、执行者来源）
// 或已被别人认领 = 什么都不做——收口留给本 run 的 RunEnd 对账。
func (s *Store) claimPendingJobNotification(ctx context.Context, box *taskBox, jobID string) {
	var notifID string
	if err := s.db.WithContext(ctx).Raw(`
SELECT id FROM agent_notifications
 WHERE background_job_id = ? AND status = ? AND receiver_session_id = ?
 LIMIT 1`, jobID, notifStatusPending, string(box.session)).Scan(&notifID).Error; err != nil {
		return
	}
	if notifID == "" {
		return
	}
	if _, err := s.ClaimNotificationInline(ctx, notifID, box.run, box.session); err != nil {
		log.Printf("bash job %s: inline claim: %v", jobID, err)
	}
}

// bashOutputIn 是 bash_output 的入参（施工包 §6.2）：缺省 wait=false，只读当前尾部。
type bashOutputIn struct {
	JobID string `json:"jobId" jsonschema:"description=命令号（bash 工具超时转后台时返回的 jobId）"`
	Wait  bool   `json:"wait,omitempty" jsonschema:"description=可选；true=最多等待一个前台等待窗口，命令到终态或到点后返回"`
}

// newBashOutputTool 造 bash_output：读一条后台命令的当前输出与状态。读到终态时就地认领该命令的
// 交付——本会话不会再收到一次重复结果（§6.5）。
func newBashOutputTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("bash_output",
		"读取一条后台命令的当前输出与状态（stdout/stderr 为当前尾部最多 64000 字节，全文路径一并给出）。"+
			"wait=true 时最多等一个前台等待窗口，命令到终态或到点后返回。",
		func(ctx context.Context, in bashOutputIn) (string, error) {
			return handleBashOutput(ctx, store, in)
		})
}

func handleBashOutput(ctx context.Context, store *Store, in bashOutputIn) (string, error) {
	box := taskBoxFrom(ctx)
	if box == nil {
		return "", fmt.Errorf("bash_output 不可用（没有 run 身份）")
	}
	job, err := store.jobByID(ctx, in.JobID)
	if err != nil || !jobVisibleToRun(ctx, job) {
		return "", fmt.Errorf("找不到这条命令：%s", in.JobID)
	}
	if in.Wait && !jobTerminal(job.Status) {
		job, err = store.pollJobUntilTerminal(ctx, job)
		if err != nil {
			return "", err
		}
	}
	if jobTerminal(job.Status) {
		store.claimPendingJobNotification(ctx, box, job.ID)
	}
	// 等待窗口与读到的状态记在当前 span 上（§6.6）：轮询本身不打面向用户的进度帧。
	waitSpan := trace.SpanFromContext(ctx)
	waitSpan.SetAttributes(
		attribute.Bool("tako.bash_job.wait", in.Wait),
		attribute.String("tako.bash_job.observed_status", job.Status),
	)
	if in.Wait {
		waitSpan.SetAttributes(attribute.Int64("tako.bash_job.wait_window_ms", waitWindow.Milliseconds()))
	}
	out, err := store.jobOutput(jobViewOf(job), job.TenantID)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// stopBashIn 是 stop_bash 的入参（施工包 §6.2）。
type stopBashIn struct {
	JobID string `json:"jobId" jsonschema:"description=要停止的命令号"`
}

// newStopBashTool 造 stop_bash：请求停止一条还在运行的后台命令（连同它的子进程）。状态机与
// HTTP 停止端点同源（stopJobStatus）——running→stopping；已终态回当前态；stopping 时拒重复停止。
func newStopBashTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("stop_bash",
		"请求停止一条还在运行的后台命令（连同它的子进程）。返回 stopping 表示停止请求已受理、进程正在退出；"+
			"到终态之前不要重复请求停止。已结束的命令返回当前终态。",
		func(ctx context.Context, in stopBashIn) (string, error) {
			return handleStopBash(ctx, store, in)
		})
}

func handleStopBash(ctx context.Context, store *Store, in stopBashIn) (string, error) {
	job, err := store.jobByID(ctx, in.JobID)
	if err != nil || !jobVisibleToRun(ctx, job) {
		return "", fmt.Errorf("找不到这条命令：%s", in.JobID)
	}
	status, stopRequested, err := store.stopJobStatus(ctx, job)
	if err != nil {
		if errors.Is(err, errJobNotStoppable) {
			return "", errors.New("这条命令正在停止中，等它到终态再查询，不要重复请求停止")
		}
		return "", err
	}
	b, _ := json.Marshal(jobStopView{JobID: in.JobID, Status: status, StopRequested: stopRequested})
	return string(b), nil
}
