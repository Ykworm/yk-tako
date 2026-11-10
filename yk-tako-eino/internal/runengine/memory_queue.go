package runengine

// 本文件：记忆任务队列的**抽象**与进程内实现。
//
// 队列在整套机制里只做一件事：**叫醒**。
//
// 任务的真源永远是 PG 的 memory_jobs 表——消息正文只带一个 job_id。
// 「收到消息」**不等于**「这个任务该由我处理」：处理权由 DB 的认领条件
// （状态 + 租约 + claim_epoch）决定。这样分工是为了故障语义：
//
//	DB 提交后还没发出消息   → 对账器扫到超龄 pending，补投
//	消息重复投递           → 认领条件挡住（一个 running 只有一个持有者）
//	消费者崩了             → 租约过期，另一个副本捡回；epoch 递增，旧的写不进去
//	消息丢了               → 对账器补投
//
// 所以消息丢了、重复了、来得晚了，都不会丢任务、也不会写重复——**因为状态与写批次都在库里**。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// 队列的固定口径（doc/65 §4.6；施工包 §3.3）：
const (
	memoryQueueTopic   = "tako.memory.extract"
	memoryQueueGroup   = "tako-memory-extractor-v1"
	memoryQueueEnvKey  = "TAKO_REDPANDA_BROKERS"
	memoryQueueUserEnv = "TAKO_REDPANDA_USER"
	memoryQueuePassEnv = "TAKO_REDPANDA_PASSWORD"
	memoryQueueMechEnv = "TAKO_REDPANDA_MECH"
)

// memoryMessage 是队列里的一条消息。
//
// JobID 是唯一的真信息；Key 是来源 session_id（决定分区 → 同 session 的消息进同一分区，
// 正常路径下按会话顺序到达）；TraceParent 是 trace 透传串。
type memoryMessage struct {
	JobID       string
	Key         string
	TraceParent string
}

// memoryQueue 是队列接口：真 broker 一份、进程内 fake 一份。
type memoryQueue interface {
	// Publish 发一条叫醒消息。key 决定分区。
	Publish(ctx context.Context, msg memoryMessage) error
	// Consume 拉消息并交给 handler。
	//
	// handler 返回 nil → **提交 offset**（这条消息处理完了）；
	// handler 返回 error → **不提交**（消息会重投）。
	//
	// 「什么时候能提交」是正确性的关键：只有 DB 已经进入可安全结束的状态
	// （done / failed / 已回 pending 且写清了 next_attempt_at）才能提交。
	// 提交早了 = 任务丢失；提交晚了 = 任务重复处理（但认领条件挡住重复写）。
	Consume(ctx context.Context, handler func(ctx context.Context, msg memoryMessage) error) error
	Close()
}

// memoryQueueConfig 是队列的连接口径（全部来自环境变量，**不写死主机名与凭据**）。
type memoryQueueConfig struct {
	Brokers  []string
	Topic    string
	Group    string
	User     string
	Password string
	Mech     string // scram-sha-256 | plain
}

// memoryQueueConfigFromEnv 从环境变量读队列配置。
//
// 凭据只从环境变量读：仓库里不落密码、不进日志、不进错误文案。
// broker 地址为空 = 没配 broker（调用方决定是「不启动消费者」还是「启动失败」）。
func memoryQueueConfigFromEnv() memoryQueueConfig {
	brokers := []string{}
	for _, b := range strings.Split(os.Getenv(memoryQueueEnvKey), ",") {
		b = strings.TrimSpace(b)
		if b != "" {
			brokers = append(brokers, b)
		}
	}
	mech := strings.ToLower(strings.TrimSpace(os.Getenv(memoryQueueMechEnv)))
	if mech == "" {
		mech = "scram-sha-256"
	}
	return memoryQueueConfig{
		Brokers:  brokers,
		Topic:    memoryQueueTopic,
		Group:    memoryQueueGroup,
		User:     os.Getenv(memoryQueueUserEnv),
		Password: os.Getenv(memoryQueuePassEnv),
		Mech:     mech,
	}
}

// ErrMemoryQueueUnavailable 表示队列不可用（没配 broker / 连不上 / 认证失败）。
var ErrMemoryQueueUnavailable = errors.New("记忆队列不可用")

// ── 进程内 fake（单测用）───────────────────────────────────────────────────────

// fakeMemoryQueue 是进程内队列：单测用它跑「登记 → 叫醒 → 处理」的全流程，
// **不需要真 broker**（单测不该绑在网络上）。
//
// 它只保证两件事：消息按 key 分组（模拟分区）、handler 失败时**不丢消息**（重投）。
type fakeMemoryQueue struct {
	mu        sync.Mutex
	pending   []memoryMessage
	closed    bool
	published []memoryMessage
	// failHandler 让下一个 handler 调用返回错误（测「处理失败 → 消息重投」）。
	failNext bool
}

// newFakeMemoryQueue 造一个空的进程内队列。
func newFakeMemoryQueue() *fakeMemoryQueue { return &fakeMemoryQueue{} }

// Publish 塞一条消息。
func (q *fakeMemoryQueue) Publish(ctx context.Context, msg memoryMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("队列已关闭")
	}
	q.pending = append(q.pending, msg)
	q.published = append(q.published, msg)
	return nil
}

// Consume 把待办消息交给 handler；handler 出错的消息**放回队首**（重投）。
func (q *fakeMemoryQueue) Consume(ctx context.Context, handler func(ctx context.Context, msg memoryMessage) error) error {
	for {
		q.mu.Lock()
		if q.closed || len(q.pending) == 0 {
			q.mu.Unlock()
			return nil
		}
		msg := q.pending[0]
		q.pending = q.pending[1:]
		fail := q.failNext
		q.failNext = false
		q.mu.Unlock()

		if fail {
			return fmt.Errorf("fake queue: 注入的失败")
		}
		if err := handler(ctx, msg); err != nil {
			// 不提交 → 放回队首，等下一轮重投。
			q.mu.Lock()
			q.pending = append([]memoryMessage{msg}, q.pending...)
			q.mu.Unlock()
			return err
		}
	}
}

// Close 关掉队列。
func (q *fakeMemoryQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

// Published 返回已发出的消息（测试断言用）。
func (q *fakeMemoryQueue) Published() []memoryMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]memoryMessage(nil), q.published...)
}

// SetFailNext 让下一次 handler 调用失败（测重投语义）。
func (q *fakeMemoryQueue) SetFailNext(v bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failNext = v
}

// ── 消费者：把「叫醒」变成「认领 → 处理 → 写回」─────────────────────────────
//
// 收到消息**不等于**该由我处理：处理权由 DB 的认领条件裁决（状态 + 租约 + claim_epoch）。
// 消费者的每一步都围绕 doc/65 §4.6 的四个故障语义：
//
//	DB 提交后还没发出消息   → 对账器扫到，补投
//	消息重复投递           → done/failed 直接 ack；running 有活租约直接 ack（不重复执行）
//	消费者崩了             → 租约过期，另一个副本按认领条件接管；epoch 递增，旧的写不进去
//	处理失败               → 同事务回 pending + 退避（1/5/30 分钟，第三次 failed），随后 ack
//	                         —— **禁止 MQ 与 DB 双头重试**：重试只由 DB（对账器补投）驱动。

// memoryConsumer 把队列的叫醒变成「认领 → 处理 → 写回」。
type memoryConsumer struct {
	store     *Store
	queue     memoryQueue
	extractor *memoryExtractor
	// lease 是 running 租约（默认 memoryJobLease）。租约不承诺「一定够抽完」——
	// 处理期间持续续约（renewLoop），续约失败取消本次处理。
	lease time.Duration
	// deferDelay 是 postrun 前置未满足时的重新叫醒延迟（保持 pending、不消耗 attempts）。
	deferDelay time.Duration
	// reconcileEvery 是对账周期（doc/65 §4.6：每 30 秒）。
	reconcileEvery time.Duration
}

// memoryReconcileBatch 是对账器单轮最多补投多少条（防一次扫出风暴打满分区）。
const memoryReconcileBatch = 100

// newMemoryConsumer 造消费者（单测可直接调，把 lease / deferDelay / reconcileEvery 调小）。
func newMemoryConsumer(store *Store, q memoryQueue, ext *memoryExtractor) *memoryConsumer {
	return &memoryConsumer{
		store:          store,
		queue:          q,
		extractor:      ext,
		lease:          memoryJobLease,
		deferDelay:     30 * time.Second,
		reconcileEvery: 30 * time.Second,
	}
}

// Run 消费直到队列退出或 ctx 取消。
func (c *memoryConsumer) Run(ctx context.Context) error {
	return c.queue.Consume(ctx, c.handleWake)
}

// handleWake 处理一条叫醒消息。返回 nil = 这条消息可以提交 offset（ack）；
// 返回 error = 不提交（消息重投——只用于**读 DB 失败**这类「连任务状态都看不了」的故障）。
func (c *memoryConsumer) handleWake(ctx context.Context, msg memoryMessage) error {
	job, err := c.store.GetMemoryJob(ctx, msg.JobID)
	if err != nil {
		return fmt.Errorf("读任务 %s：%w", msg.JobID, err)
	}
	if job == nil {
		// 项目删除后旧消息找不到任务：直接 ack，**不重建项目记忆**（施工包 §3.4）。
		log.Printf("memory queue: 任务不存在（项目已删除？），跳过 job=%s", msg.JobID)
		return nil
	}
	now := time.Now()
	switch {
	case job.Status == MemoryJobStatusDone || job.Status == MemoryJobStatusFailed:
		return nil // 已终结：重复消息 / 迟到消息，ack
	case job.Status == MemoryJobStatusRunning:
		if job.LeaseUntil != nil && job.LeaseUntil.After(now) {
			return nil // 别的持有者活着：重复消息，ack（不重复执行）
		}
		// 租约过期 → 走认领（接管）
	case job.Status == MemoryJobStatusPending:
		if job.NextAttemptAt.After(now) {
			return nil // 还没到点（退避中 / 等待中）：ack，对账器到点会再叫
		}
	}

	// postrun 前置：在认领**前**查——等待不消耗 attempts（doc/65 §4.6 ②）。
	if job.Kind == MemoryJobKindPostrun {
		waiting, err := c.store.hasUnfinishedInrun(ctx, job.SessionID)
		if err != nil {
			return err // DB 故障：不 ack，消息重投
		}
		if waiting {
			if err := c.store.deferMemoryJob(ctx, job.ID, nil, c.deferDelay); err != nil {
				return err
			}
			return nil // ack；对账器到点重新叫醒
		}
		// failed 的 inrun **不算**未完成（不卡 postrun），但要记警告——
		// 不能假装失败 inrun 的证据已写入；失败细节留在它自己的 job 行里。
		if failedIDs, ferr := c.store.failedInrunJobIDs(ctx, job.SessionID); ferr == nil && len(failedIDs) > 0 {
			warn := fmt.Sprintf("警告(inrun_failed)：同 session 的 inrun 任务已失败（其证据未写入）：%s",
				strings.Join(failedIDs, ","))
			log.Printf("memory postrun job=%s: %s", job.ID, warn)
			if werr := c.store.noteMemoryJobWarning(ctx, job.ID, warn); werr != nil {
				log.Printf("memory postrun job=%s: 记 inrun_failed 警告失败：%v", job.ID, werr)
			}
		}
	}

	claimed, err := c.store.claimMemoryJobByID(ctx, job.ID, c.lease)
	if err != nil {
		return fmt.Errorf("认领任务 %s：%w", job.ID, err)
	}
	if claimed == nil {
		return nil // 已被别的副本认领 / 状态已变：ack
	}
	c.processClaimed(ctx, claimed)
	return nil // 处理结果已写进 DB（done / 回 pending 退避 / failed）——无论成败都 ack
}

// processClaimed 处理一条已认领的任务并写回。
//
// 处理失败也 ack：失败已写进 DB（回 pending + 退避 / 第三次 failed），重试由对账器
// 到点补投，不是 MQ 重投。写回动作全部用 claim_epoch 守卫——旧持有者写不进去。
func (c *memoryConsumer) processClaimed(ctx context.Context, job *MemoryJob) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewDone := make(chan struct{})
	go c.renewLoop(ctx, pctx, cancel, job.ID, job.ClaimEpoch, renewDone)

	res, err := c.extractor.process(pctx, job)
	cancel()
	<-renewDone

	switch {
	case err == nil:
		logExtract(job, res, nil)
	case errors.Is(err, errPostrunWaiting):
		// 认领后才发现要等（检查与认领之间登记了 inrun 的窄窗口）：退回 pending，
		// 归还 claim 吃掉的 attempts（等待不消耗次数），到点由对账器再叫。
		if derr := c.store.deferMemoryJob(ctx, job.ID, &job.ClaimEpoch, c.deferDelay); derr != nil {
			log.Printf("memory queue: 退回等待任务 %s 失败：%v", job.ID, derr)
		}
	default:
		// 失败：同事务回 pending + last_error + 退避（1/5/30 分钟，第三次 failed），
		// 随后 ack——对账器到点补投。
		if ferr := c.store.FailMemoryJob(ctx, job.ID, job.ClaimEpoch, err); ferr != nil {
			log.Printf("memory queue: 记录任务 %s 的失败状态出错：%v", job.ID, ferr)
		}
		logExtract(job, res, err)
	}
}

// renewLoop 在处理期间持续续约。
//
// 续约的 DB 调用走**外层** ctx（不是处理 ctx）：要取消的是处理，不是续约本身。
// DB 出错 → 记日志、下个 tick 再试（瞬时故障不该放弃处理）；
// 确认丢租约（0 行更新 = 已被接管或已终结）→ 取消本次处理——
// 后续的模型调用随 ctx 取消，就算跑完了，写回也被 claim_epoch 守卫拒绝。
func (c *memoryConsumer) renewLoop(ctx, pctx context.Context, cancel context.CancelFunc, id string, epoch int64, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(c.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-pctx.Done():
			return
		case <-t.C:
		}
		ok, err := c.store.renewMemoryJobLease(ctx, id, epoch, c.lease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("memory queue: 续约任务 %s 出错（下个 tick 再试）：%v", id, err)
			continue
		}
		if !ok {
			log.Printf("memory queue: 任务 %s 租约已丢失（被接管或已终结），取消本次处理", id)
			cancel()
			return
		}
	}
}

// reconcileOnce 对账器扫一轮并补投（doc/65 §4.6 ③）。
//
// 扫描**不改状态**：补投只发 job_id；同一任务被多副本重复叫醒没有关系——
// 能不能处理由认领条件与 claim_epoch 裁决，不会重复写。
func (c *memoryConsumer) reconcileOnce(ctx context.Context) (int, error) {
	jobs, err := c.store.dueMemoryJobs(ctx, memoryReconcileBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range jobs {
		job := &jobs[i]
		msg := memoryMessage{JobID: job.ID, Key: memoryQueueKeyFor(job), TraceParent: job.TraceParent}
		if err := c.queue.Publish(ctx, msg); err != nil {
			log.Printf("memory reconcile: 补投任务 %s 失败（下轮再试）：%v", job.ID, err)
			continue
		}
		n++
	}
	return n, nil
}

// reconcileLoop 每 reconcileEvery 扫一次，直到 ctx 取消。
func (c *memoryConsumer) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(c.reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := c.reconcileOnce(ctx); err != nil {
			log.Printf("memory reconcile: 扫描失败：%v", err)
		}
	}
}

// ── 消费者与对账器用的存储原语 ───────────────────────────────────────────────

// GetMemoryJob 读一条任务（无行返回 nil，不是错误）。
func (s *Store) GetMemoryJob(ctx context.Context, id string) (*MemoryJob, error) {
	var job MemoryJob
	err := s.db.WithContext(ctx).Raw(`SELECT * FROM memory_jobs WHERE id = ?`, id).Scan(&job).Error
	if err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// claimMemoryJobByID 认领**指定**的一条任务（消费者收到叫醒后调）。
//
// 认领条件（doc/65 §4.6 ①）：pending 已到期，或 running 租约已过期——
// 不满足就 0 行更新（返回 nil），重复消息与并行副本在这里被挡住。
// 认领动作：status=running、attempts+1、claim_epoch+1、写 lease_until。
// 返回带新 claim_epoch 的任务快照，**后续写回必须带同一个 epoch**。
func (s *Store) claimMemoryJobByID(ctx context.Context, id string, lease time.Duration) (*MemoryJob, error) {
	var job MemoryJob
	err := s.db.WithContext(ctx).Raw(
		`UPDATE memory_jobs SET status = 'running', attempts = attempts + 1,
		        claim_epoch = claim_epoch + 1,
		        lease_until = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id = ? AND (
		        (status = 'pending' AND next_attempt_at <= now()) OR
		        (status = 'running' AND lease_until IS NOT NULL AND lease_until < now()))
		 RETURNING id, run_id, session_id, tenant_id, user_id, workspace, kind, target_layer,
		           quote, source_seq, trace_parent, status, attempts, claim_epoch, lease_until,
		           next_attempt_at, dedup_key, created_at, updated_at`,
		lease.Seconds(), id).Scan(&job).Error
	if err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, nil
	}
	return &job, nil
}

// renewMemoryJobLease 续租：租约顺延。返回 false = 已不归本持有者
// （被接管 / 已终结）——调用方应取消本次处理。
func (s *Store) renewMemoryJobLease(ctx context.Context, id string, claimEpoch int64, lease time.Duration) (bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET lease_until = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
		lease.Seconds(), id, claimEpoch)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// deferMemoryJob 让任务**等一拍**：保持 pending、不动 attempts、设短 next_attempt_at。
//
// 用于 postrun 前置未满足（同 session 还有未完成的 inrun）——等待不是失败，
// 不消耗 attempts（doc/65 §4.6 ②）。
//
// claimEpoch 非 nil = 从 running 退回（认领后才发现要等的窄窗口）：
// 退回时归还这次 claim 吃掉的 attempts，并要求 epoch 仍是自己的（被接管就不动）。
func (s *Store) deferMemoryJob(ctx context.Context, id string, claimEpoch *int64, delay time.Duration) error {
	if claimEpoch != nil {
		return s.db.WithContext(ctx).Exec(
			`UPDATE memory_jobs SET status = 'pending', attempts = GREATEST(attempts - 1, 0),
			        lease_until = NULL,
			        next_attempt_at = now() + make_interval(secs => ?), updated_at = now()
			  WHERE id = ? AND status = 'running' AND claim_epoch = ?`,
			delay.Seconds(), id, *claimEpoch).Error
	}
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET next_attempt_at = now() + make_interval(secs => ?), updated_at = now()
		  WHERE id = ? AND status = 'pending'`,
		delay.Seconds(), id).Error
}

// failedInrunJobIDs 同 session 已 failed 的 inrun 任务 id（postrun 记 inrun_failed 警告用）。
func (s *Store) failedInrunJobIDs(ctx context.Context, sessionID SessionID) ([]string, error) {
	if sessionID == "" {
		return nil, nil
	}
	var ids []string
	err := s.db.WithContext(ctx).Raw(
		`SELECT id FROM memory_jobs WHERE session_id = ? AND kind = 'inrun' AND status = 'failed' ORDER BY created_at`,
		sessionID).Scan(&ids).Error
	return ids, err
}

// noteMemoryJobWarning 把一条**警告**写进任务行的 last_error。
//
// 任务本身没失败——last_error 是行上唯一的自由文本格，警告借用它并带「警告」前缀，
// 避免被读成「这个 postrun 失败了」。
func (s *Store) noteMemoryJobWarning(ctx context.Context, id, warning string) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET last_error = ?, updated_at = now() WHERE id = ?`, warning, id).Error
}

// dueMemoryJobs 对账器的扫描：到期 pending（从未投递的超龄 / 到点重试 / postrun 等待到期）
// + 租约过期的 running。
//
// 只读不改：补投只发 job_id；把 DB 状态恢复成「可认领形态」靠认领条件本身
// （pending 到期 / running 租约过期都可直接认领），不预改状态——
// 改了反而会在「原持有者还活着只是慢」时制造两个处理者。
func (s *Store) dueMemoryJobs(ctx context.Context, limit int) ([]MemoryJob, error) {
	var jobs []MemoryJob
	err := s.db.WithContext(ctx).Raw(
		`SELECT id, session_id, tenant_id, workspace, kind, trace_parent FROM memory_jobs
		  WHERE (status = 'pending' AND next_attempt_at <= now())
		     OR (status = 'running' AND lease_until IS NOT NULL AND lease_until < now())
		  ORDER BY next_attempt_at
		  LIMIT ?`, limit).Scan(&jobs).Error
	return jobs, err
}

// ── 装配（cmd/yktako/main.go 接线的公开入口）─────────────────────

// MemoryWakeFunc 在任务**登记提交后**发叫醒（doc/65 §4.5：提交后才发）。
// 发送失败只记日志、不上抛——消息丢了有对账器兜底，叫醒失败不该让已登记的任务回滚。
type MemoryWakeFunc func(ctx context.Context, job *MemoryJob)

// newMemoryWakeFunc 把队列包成登记后的叫醒函数。
func newMemoryWakeFunc(q memoryQueue) MemoryWakeFunc {
	return func(ctx context.Context, job *MemoryJob) {
		if job == nil || job.ID == "" {
			return
		}
		msg := memoryMessage{JobID: job.ID, Key: memoryQueueKeyFor(job), TraceParent: job.TraceParent}
		if err := q.Publish(ctx, msg); err != nil {
			log.Printf("memory queue: 叫醒消息发送失败（对账器会补投）job=%s：%v", job.ID, err)
		}
	}
}

// StartMemoryConsumer 起 pipeline 的 MQ 消费者与对账器。
//
// 接线点在 cmd/yktako/main.go：pipeline 且 Extract=on 才调用；
// 返回错误一律启动即退（broker 没配 / 不可达 / 认证失败 / topic 不存在）。
//
// 返回：
//   - stop：停消费者与对账器（等两者退出；幂等）；
//   - wake：给派工工具在登记提交后发叫醒用（newMemoryDispatchTool 的装配处传入；
//     不接也行——对账器会补投，只是场中抽取多等最多一个对账周期）；
//   - err：包装 ErrMemoryQueueUnavailable，错误文案不含密码。
func StartMemoryConsumer(parent context.Context, store *Store, getModel MemoryModelFunc, counter TokenCounter) (stop func(), wake MemoryWakeFunc, err error) {
	cfg := memoryQueueConfigFromEnv()
	if len(cfg.Brokers) == 0 {
		return nil, nil, fmt.Errorf("%w：没配 %s", ErrMemoryQueueUnavailable, memoryQueueEnvKey)
	}
	q, err := newRedpandaQueue(cfg)
	if err != nil {
		return nil, nil, err
	}
	cons := newMemoryConsumer(store, q, newMemoryExtractor(store, getModel, counter))
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if cerr := cons.Run(ctx); cerr != nil && ctx.Err() == nil {
			log.Printf("memory queue: 消费者退出：%v", cerr)
		}
	}()
	go func() {
		defer wg.Done()
		cons.reconcileLoop(ctx)
	}()
	stop = func() {
		cancel()
		q.Close()
		wg.Wait()
	}
	return stop, newMemoryWakeFunc(q), nil
}
