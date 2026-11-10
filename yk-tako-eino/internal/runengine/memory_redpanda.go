package runengine

// 本文件：Redpanda（Kafka 协议）的队列实现（franz-go）。
//
// 为什么用 Kafka 协议的客户端而不是 Redpanda 专用 SDK：协议层是通用的，
// 同一份客户端代码对 Redpanda 与 Apache Kafka 都成立；换 broker 只改环境变量。
//
// 三条硬要求（doc/65 §4.6）：
//
//  1. **关闭自动提交**：只有 DB 已经进入可安全结束的状态才提交 offset。
//     自动提交会在「DB 还没写完就提交了」时丢任务——而丢任务在记忆系统里
//     表现为「用户明明说了，模型下一场却不知道」，最难查。
//  2. **key = 来源 session_id**：同 session 的消息进同一分区，正常路径下按会话顺序到达。
//     tidy 没有来源 session 时用 `tidy:<tenant>:<workspace>`，**不伪造用户会话**。
//  3. **value 只含 job_id**：任务正文真源在库里；消息只是叫醒。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// redpandaQueue 是 franz-go 实现的队列。
type redpandaQueue struct {
	producer *kgo.Client
	consumer *kgo.Client
	cfg      memoryQueueConfig
}

// newRedpandaQueue 建一个真 broker 的队列（producer + consumer）。
//
// 认证：默认 SCRAM-SHA-256（本项目的 broker 实测只支持它；PLAIN 被拒）。
// kgo.NewClient 是懒连接，所以这里主动探一次 topic 元数据：
// broker 不可达 / 认证失败 / topic 不存在都在启动期返回错误（错误文案不含密码）。
func newRedpandaQueue(cfg memoryQueueConfig) (*redpandaQueue, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("%w：没配 %s", ErrMemoryQueueUnavailable, memoryQueueEnvKey)
	}
	var mech sasl.Mechanism
	switch cfg.Mech {
	case "plain":
		mech = plain.Auth{User: cfg.User, Pass: cfg.Password}.AsMechanism()
	default:
		mech = scram.Auth{User: cfg.User, Pass: cfg.Password}.AsSha256Mechanism()
	}
	producer, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.SASL(mech),
		kgo.DefaultProduceTopic(cfg.Topic),
		kgo.ProducerBatchMaxBytes(1<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("%w：建 producer：%v", ErrMemoryQueueUnavailable, err)
	}
	if err := probeMemoryTopic(producer, cfg.Topic); err != nil {
		producer.Close()
		return nil, err
	}
	consumer, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.SASL(mech),
		kgo.ConsumeTopics(cfg.Topic),
		kgo.ConsumerGroup(cfg.Group),
		// 手动提交：DB 没落定就不提交 offset。
		kgo.DisableAutoCommit(),
		kgo.FetchMaxBytes(4<<20),
	)
	if err != nil {
		producer.Close()
		return nil, fmt.Errorf("%w：建 consumer：%v", ErrMemoryQueueUnavailable, err)
	}
	return &redpandaQueue{producer: producer, consumer: consumer, cfg: cfg}, nil
}

// probeMemoryTopic 启动期探一次 topic 元数据：broker 不可达 / 认证失败在这里报错，
// topic 不存在报 ErrorCode=3（AllowAutoTopicCreation 零值=false，不会顺手建 topic）。
func probeMemoryTopic(cl *kgo.Client, topic string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := cl.RequestCachedMetadata(ctx, &kmsg.MetadataRequest{
		Topics: []kmsg.MetadataRequestTopic{{Topic: &topic}},
	}, 0)
	if err != nil {
		return fmt.Errorf("%w：连 broker：%v", ErrMemoryQueueUnavailable, err)
	}
	for _, t := range meta.Topics {
		if t.Topic != nil && *t.Topic == topic {
			if t.ErrorCode != 0 {
				return fmt.Errorf("%w：topic %q 不可用（ErrorCode=%d，3=不存在）", ErrMemoryQueueUnavailable, topic, t.ErrorCode)
			}
			return nil
		}
	}
	return fmt.Errorf("%w：topic %q 不在元数据响应里", ErrMemoryQueueUnavailable, topic)
}

// Publish 发一条叫醒消息。
func (q *redpandaQueue) Publish(ctx context.Context, msg memoryMessage) error {
	if q == nil || q.producer == nil {
		return fmt.Errorf("%w：producer 未就绪", ErrMemoryQueueUnavailable)
	}
	payload, err := json.Marshal(map[string]string{"job_id": msg.JobID})
	if err != nil {
		return err
	}
	rec := &kgo.Record{Key: []byte(msg.Key), Value: payload}
	if msg.TraceParent != "" {
		rec.Headers = []kgo.RecordHeader{{Key: "traceparent", Value: []byte(msg.TraceParent)}}
	}
	res := q.producer.ProduceSync(ctx, rec)
	if err := res.FirstErr(); err != nil {
		return fmt.Errorf("投递叫醒消息：%w", err)
	}
	return nil
}

// Consume 拉消息并交给 handler；handler 返回错误 → **不提交 offset**（消息会重投）。
func (q *redpandaQueue) Consume(ctx context.Context, handler func(ctx context.Context, msg memoryMessage) error) error {
	if q == nil || q.consumer == nil {
		return fmt.Errorf("%w：consumer 未就绪", ErrMemoryQueueUnavailable)
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
				log.Printf("memory queue: 拉取错误：%v", e.Err)
			}
			continue
		}
		var failed []kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			msg := memoryMessage{Key: string(r.Key)}
			var body struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(r.Value, &body); err != nil || body.JobID == "" {
				// 消息体不认识：**直接提交跳过**（否则一条坏消息会无限重投）。
				log.Printf("memory queue: 消息体不认识，跳过（partition=%d offset=%d）", r.Partition, r.Offset)
				return
			}
			msg.JobID = body.JobID
			for _, h := range r.Headers {
				if h.Key == "traceparent" {
					msg.TraceParent = string(h.Value)
				}
			}
			if err := handler(ctx, msg); err != nil {
				log.Printf("memory queue: 处理失败（不提交 offset，等重投）：job=%s err=%v", msg.JobID, err)
				failed = append(failed, *r)
			}
		})
		// 提交**处理成功**的那些（处理失败的留在分区里重投）。
		if n := fetches.NumRecords() - len(failed); n > 0 {
			var ok []*kgo.Record
			fetches.EachRecord(func(r *kgo.Record) {
				for _, f := range failed {
					if f.Partition == r.Partition && f.Offset == r.Offset {
						return
					}
				}
				ok = append(ok, r)
			})
			if err := q.consumer.CommitRecords(ctx, ok...); err != nil {
				log.Printf("memory queue: 提交 offset 失败：%v", err)
			}
		}
		if len(failed) > 0 {
			// 有失败的消息：不提交它们，并短暂退避，避免热循环。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
}

// Close 关掉 producer 与 consumer。
func (q *redpandaQueue) Close() {
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

// memoryQueueKeyFor 取消息的分区键。
//
// inrun / postrun 用来源 session_id（同会话的消息进同一分区）；
// tidy 没有来源 session → `tidy:<tenant>:<workspace>`，**不伪造用户会话**。
func memoryQueueKeyFor(job *MemoryJob) string {
	if job == nil {
		return ""
	}
	if job.SessionID != "" {
		return string(job.SessionID)
	}
	return fmt.Sprintf("tidy:%s:%s", job.TenantID, job.Workspace)
}

// ensureTraceParent 从 ctx 里取 trace 透传串（没有就空）。
func ensureTraceParent(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s := traceParentFrom(ctx); s != "" {
		return s
	}
	return ""
}

// traceParentFrom 取 ctx 里的 traceparent（没有该能力时返回空串，不报错）。
func traceParentFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(traceParentCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// traceParentCtxKey 是 ctx 携带 trace 透传串的私有 key。
type traceParentCtxKey struct{}

// withTraceParent 把 trace 透传串放进 ctx（消费者收到消息后用）。
func withTraceParent(ctx context.Context, s string) context.Context {
	if strings.TrimSpace(s) == "" {
		return ctx
	}
	return context.WithValue(ctx, traceParentCtxKey{}, s)
}

var _ memoryQueue = (*redpandaQueue)(nil)
var _ memoryQueue = (*fakeMemoryQueue)(nil)
