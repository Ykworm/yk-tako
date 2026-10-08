// 本文件：真 LLM worker（新 A6 转正）+ Translator 归一层（planning §5 第 7 条）。
//
// Translator 是 provider chunk → 归一 delta 文本的唯一翻译点；Worker.Run 是 AppendDelta 的唯一调用方：
// runlive 的帧协议（19v6-01 §2 {kind,messageId,text,status}）不随厂商变——厂商差异全部收在这一层，
// 兼容性由它承担，不是由存储侧承担。claude / openai / 原生 eino 各一个 translator：
// 现在同读 schema.Message.Content（思考与推理在 ReasoningContent，不属于回复正文、不进 delta 帧），
// 但分家写死——某家字段一旦分叉，只改对应那一个，帧协议与存储侧不动。
package runengine

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ChatStream 是 worker 对模型的全部要求：流式生成（19v6-03「生命周期」）。
// eino 的 model.ToolCallingChatModel 满足它。
type ChatStream interface {
	Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// Translator 把一个 provider chunk 翻译成归一 delta 文本；ok=false = 这一帧不产生 delta
// （空内容帧、思考帧、用法帧等），调用方跳过。
type Translator func(chunk *schema.Message) (text string, ok bool)

// TranslatorFor 按协议给出 translator。协议值与 config 的 ProtocolName 一致
// （provider-catalog.json 的 protocol 字段）："anthropic" → claudeChunkText，"openai" → openaiChunkText；
// "eino" → einoChunkText 是给原生 eino ChatModel 预留的出口（config 目前不产出这个值）。
// 未知协议返回 nil——正常装配到不了这里：config.asProtocol 在启动期已把协议限死为 openai/anthropic。
func TranslatorFor(protocol string) Translator {
	switch protocol {
	case "anthropic":
		return claudeChunkText
	case "openai":
		return openaiChunkText
	case "eino":
		return einoChunkText
	default:
		return nil
	}
}

// claudeChunkText：claude 组件的流式 chunk。回复正文在 Content；thinking 在 ReasoningContent，不进帧。
func claudeChunkText(chunk *schema.Message) (string, bool) {
	if chunk == nil || chunk.Content == "" {
		return "", false
	}
	return chunk.Content, true
}

// openaiChunkText：openai 兼容组件的流式 chunk（含 DeepSeek 等）。reasoning_content 同理不进帧。
func openaiChunkText(chunk *schema.Message) (string, bool) {
	if chunk == nil || chunk.Content == "" {
		return "", false
	}
	return chunk.Content, true
}

// einoChunkText：原生 eino ChatModel 的 chunk，目前与上面同读 Content，单独留一个出口备分叉。
func einoChunkText(chunk *schema.Message) (string, bool) {
	if chunk == nil || chunk.Content == "" {
		return "", false
	}
	return chunk.Content, true
}

// Worker 消费 user message，流式产出 delta 帧，收完收尾（19v6-03）。
// 一个 run 一次 Run 调用；由 POST handler 在抢闸成功后以 goroutine 启动。
// leaseRenew 是 watchdog 续约间隔（A7）：生成期间每 leaseRenew 把闸的租约（runs.lease_until + 两键 TTL）续回，
// 进程死了没人续 → 活行按 lease 判死、两键 TTL 到期蒸发，会话自动解锁。
// compactWindow / counter 是长度自测（C 系列）：window > 0 时发送前数 token，超 limit = window × 4/5
// 走折叠（compact.go）；0 = 关压缩（counter 为 nil 即可，计数与摘要都不发生）。
type Worker struct {
	store         *Store
	chat          ChatStream
	translate     Translator
	system        string
	leaseRenew    time.Duration
	compactWindow int
	counter       TokenCounter
}

// NewWorker 组一个 worker。translate 传 TranslatorFor(config 的协议)；system 是系统消息（可为空）；
// leaseRenew 是 watchdog 续约间隔（dev 5s）；compactWindow > 0 时**必须**给 counter（main 用
// NewCl100kCounter 装配）——窗口开着却没有计数器是装配错误，不静默降级成「不压缩」。
func NewWorker(store *Store, chat ChatStream, translate Translator, system string, leaseRenew time.Duration,
	compactWindow int, counter TokenCounter) *Worker {
	return &Worker{store: store, chat: chat, translate: translate, system: system,
		leaseRenew: leaseRenew, compactWindow: compactWindow, counter: counter}
}

// Run 是 run 的执行体。**收尾无条件化**：endRun 挂在唯一出口（defer），不散在各级 return 前——
// 超时 / 429 / 连接断（Stream 或 Recv 报错）都走同一出口放闸，worker 自己收的尾（新 A3 用例 6 在真源侧的延续）。
// status 唯一的活变量：默认 complete，任何错误出口置 error。
//
// compact（C 系列）：prepareInput 只在**发送前**把折叠 Stage 在内存（stagedCompaction）；
// 落库靠 defer 的 EndRun——仅 status=complete 且闸在手上才并入同一笔事务。error / 闸失 / 进程死 = 零写入。
//
// **watchdog 生命周期严格在 EndRun 之前结束**（A7）：defer 是 LIFO，EndRun 先注册故最后跑；
// 停 watchdog 的 defer 后注册故先跑，且 `wg.Wait()` 确保续约 goroutine 完全退出——否则续约会把 runlive
// 定稿后的 300s TTL 覆盖回 lease 的 30s（D1 落库后终态行的续约被 PG 守卫挡成空转，这条顺序仍保持：
// 它同时保证「最后一次续约」早于放闸，语义最干净）。
func (w *Worker) Run(ctx context.Context, sessionID SessionID, runID RunID) {
	status := "complete"
	var stagedCompaction *StagedCompaction
	defer func() {
		_, _, ok, err := w.store.EndRun(ctx, sessionID, runID, status, stagedCompaction)
		switch {
		case err != nil:
			// 收尾失败仍留痕。D1 后有两个方向：DB 半败 = 闸没放（租约到点自蒸、会话自动解锁，不再永久锁死）；
			// Redis 半败 = 闸已放、帧缺 terminal（runlive 走租约/TTL 收敛成 gone，客户端从 log 拿全文）。
			log.Printf("endRun %s/%s (%s): %v", sessionID, runID, status, err)
		case !ok:
			log.Printf("endRun %s/%s (%s): 闸已不在，空转", sessionID, runID, status)
		}
	}()

	leaseCtx, cancelLease := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.watchdog(leaseCtx, sessionID, runID)
	}()
	defer func() { cancelLease(); wg.Wait() }()

	msgs, staged, err := w.prepareInput(ctx, sessionID, runID)
	if err != nil {
		// compact 的 errStillOver 也走这里：不调主模型、status=error、stagedCompaction 保持 nil（零写入）。
		log.Printf("prepareInput %s/%s: %v", sessionID, runID, err)
		status = "error"
		return
	}
	stagedCompaction = staged
	stream, err := w.chat.Stream(ctx, msgs)
	if err != nil {
		status = "error"
		return
	}
	defer stream.Close()
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			status = "error"
			return
		}
		if text, ok := w.translate(chunk); ok {
			_, live, err := w.store.AppendDelta(ctx, runID, text)
			if err != nil {
				log.Printf("appendDelta %s: %v", runID, err)
				status = "error"
				return
			}
			if !live {
				// run 已被判死（draft 随租约到期蒸发）/ 被 reset 清掉 / 闸已不在：停止推流。
				// defer 的 EndRun 拿不到闸自然空转。
				return
			}
		}
	}
}

// watchdog 在生成期间每 leaseRenew 续约一次闸的租约（A7）。续约 no-op（PG 守卫 UPDATE 影响 0 行：
// 闸已易主 / 已放 / 已到期）= 本 run 被判死 → **记日志、退出 watchdog，但不主动杀 LLM 流**（dev 默认）：
// 主循环的 AppendDelta 会因 draft 蒸发自然停写（返 !live）、EndRun 被守卫挡住空转，
// 都是现役既有语义，不用新代码去中断流。
// ctx 由 Run 在退出前 cancel，watchdog 随即返回；wg.Wait() 保证它完全退出后 EndRun 才跑。
func (w *Worker) watchdog(ctx context.Context, sessionID SessionID, runID RunID) {
	t := time.NewTicker(w.leaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := w.store.RenewLease(ctx, sessionID, runID)
			if err != nil {
				if ctx.Err() != nil {
					return // Run 已收工、ctx 被 cancel 导致的在途续约失败，安静退出
				}
				log.Printf("lease renew %s/%s: %v", sessionID, runID, err)
				continue
			}
			if !ok {
				log.Printf("lease renew %s/%s: 闸已不属于本 run，停止续约（本 run 判死）", sessionID, runID)
				return
			}
		}
	}
}

// prepareInput 组模型侧消息：系统消息 + model_history 全序（user 在 AcquireRun 时已入，含本轮这条）。
// 读 model_history 而非 runlog——前者记「模型现在该看到什么」，后者记「产出了什么」（19v6-03「真源划分」）。
//
// compact 挂钩（C 系列）：只在本函数里做长度自测——快速路（≤ limit）原样送、行为与压缩前零差异；
// 超限走 foldInput（compact.go）折出候选输入与 stagedCompaction，由 Run 透传给 defer 的 EndRun 落库。
// 本函数不写任何存储：折叠失败（Summarize 失败 / errStillOver）直接返回错误，Run 置 status=error。
func (w *Worker) prepareInput(ctx context.Context, sessionID SessionID, runID RunID) ([]*schema.Message, *StagedCompaction, error) {
	history, err := w.store.ModelMessages(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	var stagedCompaction *StagedCompaction
	if w.compactWindow > 0 {
		limit := w.compactWindow * 4 / 5
		if total := countInput(w.counter, w.system, history); total > limit {
			log.Printf("compact %s/%s: 输入 %d tok > limit %d，进入折叠", sessionID, runID, total, limit)
			history, stagedCompaction, err = foldInput(ctx, w.system, history, runID, limit, w.counter, w.summarize)
			if err != nil {
				return nil, nil, err
			}
			log.Printf("compact %s/%s: 压 %d 个 run，摘要 %d tok，挂 %s；发送输入 %d tok < limit %d",
				sessionID, runID, len(stagedCompaction.DropRunIDs), w.counter.Count(stagedCompaction.Summary),
				stagedCompaction.AttachRun, countInput(w.counter, w.system, history), limit)
		}
	}
	msgs := make([]*schema.Message, 0, len(history)+1)
	if w.system != "" {
		msgs = append(msgs, schema.SystemMessage(w.system))
	}
	for _, h := range history {
		switch h.Role {
		case "assistant":
			msgs = append(msgs, schema.AssistantMessage(h.Text, nil))
		default:
			msgs = append(msgs, schema.UserMessage(h.Text))
		}
	}
	return msgs, stagedCompaction, nil
}
