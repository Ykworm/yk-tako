package runengine

// 本文件是历史对话混合检索的投递机械（doc/84 §6.1）：outbox relay（写 published_at）、
// Redpanda 队列的真实现与 fake、两个消费者共用的「认领 → 续租 → 批处理 → 写回」驱动。
//
// 消息只做一件事：叫醒。任务的真源永远是 PG 的 dialogue_index_jobs——消息正文只带
// {eventId,runId,target,kind,indexVersion}（doc/84 §4.2，不带 passage/密钥/向量），
// 消费者按 target 回读任务行，归属与正文都从库里的行来。
//
// 三条硬要求与 memory_queue 同源（doc/84 §6.1）：
//
//  1. 关闭自动提交：消费者先持久化处理结果（PG 写回完成/退避/死信），再提交 offset。
//  2. key = run_id：同一 run 的 upsert/delete 进同一分区，保序；不同 run 可并行。
//  3. 至少一次投递：relay 投递成功才写 published_at；写失败下轮重投，允许重复消息。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"yk-tako-eino/internal/observability"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// 队列与节奏的固定口径（doc/84 §6.1）。
const (
	dialogueIndexTopic  = "tako.dialogue.index"
	dialogueESGroup     = "tako-dialogue-es-v1"
	dialogueVectorGroup = "tako-dialogue-vector-v1"

	dialogueQueueEnvKey  = "TAKO_REDPANDA_BROKERS"
	dialogueQueueUserEnv = "TAKO_REDPANDA_USER"
	dialogueQueuePassEnv = "TAKO_REDPANDA_PASSWORD"
	dialogueQueueMechEnv = "TAKO_REDPANDA_MECH"

	dialogueRelayInterval     = 5 * time.Second  // relay 空转间隔
	dialogueRelayBatch        = 200              // relay 单轮最多补投多少行
	dialogueReconcileInterval = 30 * time.Second // 消费者对账周期
	dialogueReconcileBatch    = 100              // 对账单轮最多补投多少条
)

// dialogueMessage 是队列里的一条消息。只有五个真信息字段 + 分区键 + trace 透传；
// 正文/密钥/向量一律不进消息（消费者回 PG 读任务行与 chunk）。
type dialogueMessage struct {
	EventID      string `json:"eventId"`
	RunID        string `json:"runId"`
	Target       string `json:"target"`
	Kind         string `json:"kind"`
	IndexVersion int64  `json:"indexVersion"`
	Key          string `json:"-"` // 分区键 = run_id
	TraceParent  string `json:"-"` // 走消息头 traceparent
}

// dialogueQueue 是队列接口：真 broker 一份（每个消费者组一个实例）、进程内 fake 一份。
type dialogueQueue interface {
	// Publish 发一条消息。key（=run_id）决定分区。
	Publish(ctx context.Context, msg dialogueMessage) error
	// ConsumeBatch 拉一批消息交给 handler。
	//
	// handler 返回 nil → **提交本批全部 offset**——契约是「先持久化后提交」：
	// 消费者驱动已在 handler 里把每条任务的结局写进 PG（done / 回 pending 退避 / failed）；
	// handler 返回 error → **整批不提交**（重投），只用于「连任务状态都读不了」的 DB 故障。
	ConsumeBatch(ctx context.Context, handler func(ctx context.Context, msgs []dialogueMessage) error) error
	Close()
}

// dialogueQueueConfig 是队列的连接口径（全部来自环境变量，与 memory 共用同一组键）。
type dialogueQueueConfig struct {
	Brokers  []string
	Topic    string
	Group    string
	User     string
	Password string
	Mech     string // scram-sha-256（默认）| plain
}

// dialogueQueueConfigFromEnv 读队列配置。凭据只从环境变量读：不落仓库、不进日志、不进错误文案。
func dialogueQueueConfigFromEnv(group string) dialogueQueueConfig {
	brokers := []string{}
	for _, b := range strings.Split(os.Getenv(dialogueQueueEnvKey), ",") {
		b = strings.TrimSpace(b)
		if b != "" {
			brokers = append(brokers, b)
		}
	}
	mech := strings.ToLower(strings.TrimSpace(os.Getenv(dialogueQueueMechEnv)))
	if mech == "" {
		mech = "scram-sha-256"
	}
	return dialogueQueueConfig{
		Brokers:  brokers,
		Topic:    dialogueIndexTopic,
		Group:    group,
		User:     os.Getenv(dialogueQueueUserEnv),
		Password: os.Getenv(dialogueQueuePassEnv),
		Mech:     mech,
	}
}

// dialogueSASLMech 认证机制（与 memory_redpanda.go 同一裁决：默认 SCRAM-SHA-256，
// 本项目 broker 实测只支持它；MECH=plain 才走 plain）。
func dialogueSASLMech(cfg dialogueQueueConfig) sasl.Mechanism {
	if cfg.Mech == "plain" {
		return plain.Auth{User: cfg.User, Pass: cfg.Password}.AsMechanism()
	}
	return scram.Auth{User: cfg.User, Pass: cfg.Password}.AsSha256Mechanism()
}

// ErrDialogueQueueUnavailable 表示队列不可用（没配 broker / 连不上 / 认证失败）。
var ErrDialogueQueueUnavailable = errors.New("历史对话检索队列不可用")

// ── 真 broker（franz-go）─────────────────────────────────────────────────────

type redpandaDialogueQueue struct {
	producer *kgo.Client
	consumer *kgo.Client
	cfg      dialogueQueueConfig
}

// newDialogueRedpandaQueue 建一个真 broker 的队列（producer + 该组 consumer）。
// kgo.NewClient 是懒连接，主动探一次 topic 元数据：broker 不可达 / 认证失败 /
// topic 不存在都在启动期返回错误（错误文案不含密码）。
func newDialogueRedpandaQueue(cfg dialogueQueueConfig) (*redpandaDialogueQueue, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("%w：没配 %s", ErrDialogueQueueUnavailable, dialogueQueueEnvKey)
	}
	mech := dialogueSASLMech(cfg)
	producer, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.SASL(mech),
		kgo.DefaultProduceTopic(cfg.Topic),
		kgo.ProducerBatchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("%w：建 producer：%v", ErrDialogueQueueUnavailable, err)
	}
	if err := probeMemoryTopic(producer, cfg.Topic); err != nil {
		producer.Close()
		return nil, err
	}
	// relay 队列（group 为空）只投不消：franz-go 禁止「无组却带 autocommit 语义」的
	// consumer（invalid autocommit options when a group was not specified，2026-11-20
	// 隔离引擎实测），producer-only 构造，ConsumeBatch/ConsumerLag 对 nil consumer 自有防。
	var consumer *kgo.Client
	if cfg.Group != "" {
		consumer, err = kgo.NewClient(
			kgo.SeedBrokers(cfg.Brokers...),
			kgo.SASL(mech),
			kgo.ConsumeTopics(cfg.Topic),
			kgo.ConsumerGroup(cfg.Group),
			// 手动提交：PG 没落定就不提交 offset（doc/84 §6.1）。
			kgo.DisableAutoCommit(),
			kgo.FetchMaxBytes(4<<20),
		)
		if err != nil {
			producer.Close()
			return nil, fmt.Errorf("%w：建 consumer（group=%s）：%v", ErrDialogueQueueUnavailable, cfg.Group, err)
		}
	}
	return &redpandaDialogueQueue{producer: producer, consumer: consumer, cfg: cfg}, nil
}

// dialogueMessageWire 是消息正文（doc/84 §4.2：只有事件与归属元数据）。
type dialogueMessageWire struct {
	EventID      string `json:"eventId"`
	RunID        string `json:"runId"`
	Target       string `json:"target"`
	Kind         string `json:"kind"`
	IndexVersion int64  `json:"indexVersion"`
}

// Publish 发一条消息（key=run_id 决定分区；traceparent 走消息头）。
func (q *redpandaDialogueQueue) Publish(ctx context.Context, msg dialogueMessage) error {
	if q == nil || q.producer == nil {
		return fmt.Errorf("%w：producer 未就绪", ErrDialogueQueueUnavailable)
	}
	payload, err := json.Marshal(dialogueMessageWire{
		EventID: msg.EventID, RunID: msg.RunID, Target: msg.Target,
		Kind: msg.Kind, IndexVersion: msg.IndexVersion,
	})
	if err != nil {
		return err
	}
	key := msg.Key
	if key == "" {
		key = msg.RunID
	}
	rec := &kgo.Record{Key: []byte(key), Value: payload}
	if msg.TraceParent != "" {
		rec.Headers = []kgo.RecordHeader{{Key: "traceparent", Value: []byte(msg.TraceParent)}}
	}
	res := q.producer.ProduceSync(ctx, rec)
	if err := res.FirstErr(); err != nil {
		return fmt.Errorf("投递索引消息：%w", err)
	}
	return nil
}

// ConsumeBatch 拉一批消息交给 handler；handler 返回 error → 整批不提交（消息重投）。
func (q *redpandaDialogueQueue) ConsumeBatch(ctx context.Context, handler func(ctx context.Context, msgs []dialogueMessage) error) error {
	if q == nil || q.consumer == nil {
		return fmt.Errorf("%w：consumer 未就绪", ErrDialogueQueueUnavailable)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fetches := q.consumer.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			// 拉取错误（分区不可用等）：记日志继续，不静默当成功。
			for _, e := range errs {
				log.Printf("dialogue queue(group=%s): 拉取错误：%v", q.cfg.Group, e.Err)
			}
			continue
		}
		msgs := make([]dialogueMessage, 0, fetches.NumRecords())
		fetches.EachRecord(func(r *kgo.Record) {
			var w dialogueMessageWire
			if err := json.Unmarshal(r.Value, &w); err != nil || w.EventID == "" || w.Target == "" {
				// 消息体不认识：跳过（这批提交后它不会再出现；坏消息不能无限重投）。
				log.Printf("dialogue queue(group=%s): 消息体不认识，跳过（partition=%d offset=%d）",
					q.cfg.Group, r.Partition, r.Offset)
				return
			}
			m := dialogueMessage{
				EventID: w.EventID, RunID: w.RunID, Target: w.Target,
				Kind: w.Kind, IndexVersion: w.IndexVersion, Key: string(r.Key),
			}
			for _, h := range r.Headers {
				if h.Key == "traceparent" {
					m.TraceParent = string(h.Value)
				}
			}
			msgs = append(msgs, m)
		})
		if len(msgs) > 0 {
			if err := handler(ctx, msgs); err != nil {
				log.Printf("dialogue queue(group=%s): 批处理失败（不提交 offset，等重投）：%v", q.cfg.Group, err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				continue
			}
		}
		// handler 返回 nil = 本批任务的结局都已写进 PG → 提交全部 offset。
		var recs []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) { recs = append(recs, r) })
		if len(recs) > 0 {
			if err := q.consumer.CommitRecords(ctx, recs...); err != nil {
				log.Printf("dialogue queue(group=%s): 提交 offset 失败：%v", q.cfg.Group, err)
			}
		}
	}
}

// Close 关掉 producer 与 consumer。
func (q *redpandaDialogueQueue) Close() {
	if q == nil {
		return
	}
	if q.producer != nil {
		q.producer.Close()
	}
	if q.consumer != nil {
		q.consumer.Close()
	}
}

var _ dialogueQueue = (*redpandaDialogueQueue)(nil)

// dialogueQueueLagger 是消费 lag 的可选查询面（doc/84 §6.4 对账水位）：队列实现它则
// 对账器报告消费组积压；没实现或查询失败记 0 并在日志里说明（不伪造水位）。
type dialogueQueueLagger interface {
	// ConsumerLag 返回本队列消费组（cfg.Group）的总积压条数（未消费消息数）。
	ConsumerLag(ctx context.Context) (int64, error)
}

// ConsumerLag 用 kadm 查本消费组的总积压。kadm 只按次包裹现有 consumer 客户端——
// kadm.Close() 会关掉底层 kgo.Client，这里绝不调它（共享连接被误关是事故）。
// 组刚建还没提交过 offset 时 Lag 为空集，按 0 处理（新部署确实没积压）。
func (q *redpandaDialogueQueue) ConsumerLag(ctx context.Context) (int64, error) {
	if q == nil || q.consumer == nil {
		return 0, errors.New("redpanda 队列未初始化，查不了 lag")
	}
	adm := kadm.NewClient(q.consumer)
	lags, err := adm.Lag(ctx, q.cfg.Group)
	if err != nil {
		return 0, fmt.Errorf("查询消费组 %s lag: %w", q.cfg.Group, err)
	}
	dl, ok := lags[q.cfg.Group]
	if !ok {
		return 0, nil
	}
	if gerr := dl.Error(); gerr != nil {
		return 0, fmt.Errorf("消费组 %s lag 描述失败: %w", q.cfg.Group, gerr)
	}
	return dl.Lag.Total(), nil
}

// ── 进程内 fake（单测用）───────────────────────────────────────────────────────

// fakeDialogueQueue 是进程内队列：单测跑「登记 → 投递 → 消费」全流程，不绑网络。
// ops 记录 publish / commit 的先后（消费者「先持久化后提交」的顺序断言用）。
type fakeDialogueQueue struct {
	mu              sync.Mutex
	pending         []dialogueMessage
	published       []dialogueMessage
	closed          bool
	ops             []string
	failNextPublish bool
	lagOverride     int64
	lagErr          error
}

func newFakeDialogueQueue() *fakeDialogueQueue { return &fakeDialogueQueue{} }

func (q *fakeDialogueQueue) Publish(ctx context.Context, msg dialogueMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("队列已关闭")
	}
	q.ops = append(q.ops, "publish:"+msg.EventID+":"+msg.Target)
	if q.failNextPublish {
		q.failNextPublish = false
		return errors.New("fake queue: 注入的投递失败")
	}
	q.pending = append(q.pending, msg)
	q.published = append(q.published, msg)
	return nil
}

func (q *fakeDialogueQueue) ConsumeBatch(ctx context.Context, handler func(ctx context.Context, msgs []dialogueMessage) error) error {
	for {
		q.mu.Lock()
		if q.closed || len(q.pending) == 0 || ctx.Err() != nil {
			q.mu.Unlock()
			return ctx.Err()
		}
		n := len(q.pending)
		if n > 16 {
			n = 16
		}
		batch := append([]dialogueMessage(nil), q.pending[:n]...)
		q.pending = q.pending[n:]
		q.mu.Unlock()

		if err := handler(ctx, batch); err != nil {
			// 不提交 → 整批放回队首，等下一轮重投。
			q.mu.Lock()
			q.ops = append(q.ops, "requeue")
			q.pending = append(batch, q.pending...)
			q.mu.Unlock()
			return err
		}
		q.mu.Lock()
		q.ops = append(q.ops, fmt.Sprintf("commit:%d", len(batch)))
		q.mu.Unlock()
	}
}

func (q *fakeDialogueQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

// Published 返回已发出的消息（测试断言用）。
func (q *fakeDialogueQueue) Published() []dialogueMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]dialogueMessage(nil), q.published...)
}

// Ops 返回 publish/commit/requeue 的先后记录（测试断言用）。
func (q *fakeDialogueQueue) Ops() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.ops...)
}

// SetFailNextPublish 让下一次 Publish 失败（测「投递失败 → published_at 不写 → relay 重投」）。
func (q *fakeDialogueQueue) SetFailNextPublish(v bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failNextPublish = v
}

// lagOverride / lagErr 让单测注入对账器看到的水位与故障（缺省 0 = 没积压）。
func (q *fakeDialogueQueue) ConsumerLag(ctx context.Context) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lagOverride, q.lagErr
}

var _ dialogueQueue = (*fakeDialogueQueue)(nil)

// ── relay：已提交事件的至少一次投递 ─────────────────────────────────────────

// dialogueOutboxRelay 把 dialogue_index_jobs 里已提交但 published_at 缺失的行投给
// Redpanda，成功后写 published_at（doc/84 §6.1）。投递与写标记之间崩溃 = 下轮重投，
// 允许重复消息——重复由消费侧的认领条件挡住，不丢事件。
type dialogueOutboxRelay struct {
	store    *Store
	queue    dialogueQueue
	interval time.Duration
	batch    int
}

func newDialogueOutboxRelay(store *Store, q dialogueQueue) *dialogueOutboxRelay {
	return &dialogueOutboxRelay{store: store, queue: q, interval: dialogueRelayInterval, batch: dialogueRelayBatch}
}

// relayOnce 投一轮；返回成功补投的行数。span 记本轮事件数与失败分类
// （doc/84 §6.5：只记 ID 计数与分类，不记正文/密钥）。
func (r *dialogueOutboxRelay) relayOnce(ctx context.Context) (n int, err error) {
	ctx, span := observability.Tracer().Start(ctx, "dialogue.outbox.relay", trace.WithSpanKind(trace.SpanKindInternal))
	defer func() {
		span.SetAttributes(
			attribute.Int("dialogue.published", n),
			attribute.String("dialogue.topic", dialogueIndexTopic),
		)
		finishSpan(span, err)
	}()
	jobs, err := r.store.unpublishedDialogueJobs(ctx, r.batch)
	if err != nil {
		return 0, err
	}
	n = 0
	pubFail := 0
	for i := range jobs {
		job := &jobs[i]
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		msg := dialogueMessage{
			EventID: job.EventID, RunID: job.RunID, Target: job.Target,
			Kind: job.Kind, IndexVersion: job.IndexVersion,
			Key: job.RunID, TraceParent: job.TraceParent,
		}
		if perr := r.queue.Publish(ctx, msg); perr != nil {
			pubFail++
			log.Printf("dialogue relay: 投递 event=%s target=%s 失败（published_at 不写，下轮重投）：%v",
				job.EventID, job.Target, perr)
			continue
		}
		if merr := r.store.markDialogueJobPublished(ctx, job.EventID, job.Target); merr != nil {
			pubFail++
			log.Printf("dialogue relay: 写 published_at event=%s target=%s 失败（下轮重投，允许重复消息）：%v",
				job.EventID, job.Target, merr)
			continue
		}
		n++
	}
	span.SetAttributes(attribute.Int("dialogue.publish_failures", pubFail))
	return n, nil
}

// Run relay 主循环：满批不睡继续追赶（启动时历史未投递多），空轮按 interval 睡。
func (r *dialogueOutboxRelay) Run(ctx context.Context) {
	log.Printf("dialogue relay: started (topic=%s)", dialogueIndexTopic)
	for {
		n, err := r.relayOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("dialogue relay: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		if n < r.batch {
			if !sleepCtx(ctx, r.interval) {
				return
			}
		}
	}
}

// ── 消费者驱动：把叫醒变成「认领 → 续租 → 批处理 → 写回」────────────────────

// dialogueJobOutcome 是 processor 对单条任务的结局判定，驱动写回：
// Class 为空 = 成功（翻 done）；否则写失败——Permanent=true 直接死信，false 走退避。
type dialogueJobOutcome struct {
	Class     string
	Permanent bool
}

// dialogueBatchProcessor 是单目标的批处理体：ES 与 Milvus 消费者各实现一份。
// Process 返回的 []dialogueJobOutcome 必须与 jobs 等长同序。
type dialogueBatchProcessor interface {
	Target() string
	Process(ctx context.Context, jobs []*DialogueIndexJob) []dialogueJobOutcome
}

// dialogueConsumer 把队列的叫醒变成「认领 → 处理 → 写回」（两个消费者组共用）。
//
// 故障语义（doc/84 §6.1/§6.2，与 memoryConsumer 同源）：
//
//	消息重复投递     → 认领条件挡住（running 只有一个持有者，done/failed 直接 ack）
//	消费者崩了       → 租约过期，另一副本接管；claim_epoch 递增，旧的写不进去
//	处理失败         → 同事务写回退避（1/5/30 分钟，第三次 failed 死信），随后 ack
//	                   ——重试只由对账器（DB 状态）驱动，禁止 MQ 与 DB 双头重试
type dialogueConsumer struct {
	store          *Store
	queue          dialogueQueue
	proc           dialogueBatchProcessor
	lease          time.Duration
	reconcileEvery time.Duration
	reconcileBatch int
}

func newDialogueConsumer(store *Store, q dialogueQueue, proc dialogueBatchProcessor) *dialogueConsumer {
	return &dialogueConsumer{
		store:          store,
		queue:          q,
		proc:           proc,
		lease:          dialogueJobLease,
		reconcileEvery: dialogueReconcileInterval,
		reconcileBatch: dialogueReconcileBatch,
	}
}

// Run 消费直到队列退出或 ctx 取消。
func (c *dialogueConsumer) Run(ctx context.Context) error {
	return c.queue.ConsumeBatch(ctx, c.handleBatch)
}

// handleBatch 处理一批消息。返回 nil = 整批可以提交 offset；
// 返回 error = 整批不提交（只用于「连任务状态都读不了」的 DB 故障）。
func (c *dialogueConsumer) handleBatch(ctx context.Context, msgs []dialogueMessage) error {
	target := c.proc.Target()
	// 1. 只处理本组 target 的消息；另一组的消息直接算空投（两个组各消费各的）。
	mine := make([]dialogueMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Target == target {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	// 2. 回读任务行（消息只是叫醒）。读行失败 = 连状态都看不了：整批不提交。
	ids := make([]string, 0, len(mine))
	seen := make(map[string]bool, len(mine))
	for _, m := range mine {
		job, err := c.store.getDialogueJobByEvent(ctx, m.EventID, target)
		if err != nil {
			return fmt.Errorf("读任务 %s/%s：%w", m.EventID, target, err)
		}
		if job == nil {
			// 任务行不存在（登记事务已回滚的迟到消息）：跳过，不重建。
			continue
		}
		if !seen[job.ID] {
			seen[job.ID] = true
			ids = append(ids, job.ID)
		}
	}
	claimed, err := c.store.claimDialogueJobsByIDs(ctx, ids, c.lease)
	if err != nil {
		return fmt.Errorf("认领任务：%w", err)
	}
	if len(claimed) == 0 {
		return nil // 都不满足认领条件（别的持有者活着 / 退避中 / 已终结）：ack
	}
	// 3. 按消息顺序处理：同分区（同 run）的 upsert/delete 消息顺序 = 业务顺序。
	order := make(map[string]int, len(mine))
	for i, m := range mine {
		if _, ok := order[m.EventID]; !ok {
			order[m.EventID] = i
		}
	}
	sort.SliceStable(claimed, func(i, j int) bool {
		return order[claimed[i].EventID] < order[claimed[j].EventID]
	})
	// 4. 版本过滤：index_version 与当前指针不符的任务直接翻 done、不写外部
	// （doc/84 §6.2：旧消息和旧任务不得覆盖新代次）。
	pointer, err := c.store.dialoguePointer(ctx)
	if err != nil {
		// 指针读不到：认领了却干不了活——按可重试失败退回，不让这批吞掉 attempts。
		for _, job := range claimed {
			if ferr := c.store.failDialogueJob(ctx, job.ID, job.ClaimEpoch, "pointer_missing", false); ferr != nil {
				log.Printf("dialogue consumer(%s): 退回任务 %s 失败：%v", target, job.ID, ferr)
			}
		}
		return nil // 状态已写库：ack（对账器到点再叫）
	}
	var toProcess []*DialogueIndexJob
	for _, job := range claimed {
		if job.IndexVersion != pointer.IndexVersion {
			log.Printf("dialogue consumer(%s): 忽略非当前版本任务 event=%s target=%s（job v%d ≠ pointer v%d）",
				target, job.EventID, job.Target, job.IndexVersion, pointer.IndexVersion)
			if derr := c.store.completeDialogueJob(ctx, job.ID, job.ClaimEpoch); derr != nil {
				log.Printf("dialogue consumer(%s): 完成被忽略任务 %s 失败：%v", target, job.ID, derr)
			}
			continue
		}
		toProcess = append(toProcess, job)
	}
	if len(toProcess) == 0 {
		return nil
	}
	// 5. 续租 + 批处理 + 写回。
	pctx, cancel := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go c.renewLoop(ctx, pctx, cancel, toProcess, renewDone)

	outcomes := c.proc.Process(pctx, toProcess)
	cancel()
	<-renewDone

	if len(outcomes) != len(toProcess) {
		// processor 契约被破坏（实现 bug）：全部按可重试失败退回，不假装成功。
		log.Printf("dialogue consumer(%s): processor 返回 %d 个结局但处理了 %d 条任务，全部退回重试",
			target, len(outcomes), len(toProcess))
		outcomes = make([]dialogueJobOutcome, len(toProcess))
		for i := range outcomes {
			outcomes[i] = dialogueJobOutcome{Class: "processor_contract"}
		}
	}
	for i, job := range toProcess {
		oc := outcomes[i]
		if oc.Class == "" {
			if derr := c.store.completeDialogueJob(ctx, job.ID, job.ClaimEpoch); derr != nil {
				log.Printf("dialogue consumer(%s): 完成任务 %s 写回失败：%v", target, job.ID, derr)
			}
			continue
		}
		if ferr := c.store.failDialogueJob(ctx, job.ID, job.ClaimEpoch, oc.Class, oc.Permanent); ferr != nil {
			log.Printf("dialogue consumer(%s): 记录任务 %s 失败状态出错：%v", target, job.ID, ferr)
		}
	}
	// 无论成败都 ack：结局已在 PG（done / 退避 / 死信），重试由对账器驱动。
	return nil
}

// renewLoop 在处理期间持续续租本批全部任务。
//
// 续租的 DB 调用走外层 ctx（要取消的是处理，不是续租本身）；确认丢租约（任一任务
// 0 行更新 = 已被接管或已终结）→ 取消本次处理——后续的外部写随 ctx 取消，就算
// 跑完了，写回也被 claim_epoch 守卫拒绝。
func (c *dialogueConsumer) renewLoop(ctx, pctx context.Context, cancel context.CancelFunc, jobs []*DialogueIndexJob, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(c.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-pctx.Done():
			return
		case <-t.C:
		}
		for _, job := range jobs {
			ok, err := c.store.renewDialogueJobLease(ctx, job.ID, job.ClaimEpoch, c.lease)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("dialogue consumer(%s): 续租任务 %s 出错（下个 tick 再试）：%v", c.proc.Target(), job.ID, err)
				continue
			}
			if !ok {
				log.Printf("dialogue consumer(%s): 任务 %s 租约已丢失（被接管或已终结），取消本次处理", c.proc.Target(), job.ID)
				cancel()
				return
			}
		}
	}
}

// reconcileOnce 对账一轮：扫本 target 到期 pending 与租约过期的 running，补投叫醒消息。
// 扫描不改状态；同一任务被多副本重复叫醒没有关系——能不能处理由认领条件裁决。
func (c *dialogueConsumer) reconcileOnce(ctx context.Context) (int, error) {
	jobs, err := c.store.dueDialogueJobs(ctx, c.proc.Target(), c.reconcileBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range jobs {
		job := &jobs[i]
		msg := dialogueMessage{
			EventID: job.EventID, RunID: job.RunID, Target: job.Target,
			Kind: job.Kind, IndexVersion: job.IndexVersion,
			Key: job.RunID, TraceParent: job.TraceParent,
		}
		if err := c.queue.Publish(ctx, msg); err != nil {
			log.Printf("dialogue consumer(%s): 补投任务 %s 失败（下轮再试）：%v", c.proc.Target(), job.ID, err)
			continue
		}
		n++
	}
	return n, nil
}

// reconcileLoop 每 reconcileEvery 扫一次，直到 ctx 取消。
func (c *dialogueConsumer) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(c.reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := c.reconcileOnce(ctx); err != nil {
			log.Printf("dialogue consumer(%s): 对账扫描失败：%v", c.proc.Target(), err)
		}
	}
}
