// 本文件：真 LLM worker（新 A6 转正）+ Translator 归一层（planning §5 第 7 条）。
//
// Translator 是 provider chunk → 归一 delta 文本的唯一翻译点；Worker.Run 是 AppendDelta 的唯一调用方：
// runlive 的帧协议（doc/19 §2 {kind,messageId,text,status}）不随厂商变——厂商差异全部收在这一层，
// 兼容性由它承担，不是由存储侧承担。claude / openai / 原生 eino 各一个 translator：
// 现在同读 schema.Message.Content（思考与推理在 ReasoningContent，不属于回复正文、不进 delta 帧），
// 但分家写死——某家字段一旦分叉，只改对应那一个，帧协议与存储侧不动。
package runengine

import (
	"context"
	"io"
	"log"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ChatStream 是 worker 对模型的全部要求：流式生成（doc/19 §7 worker 行为）。
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

// Worker 消费 user message，流式产出 delta 帧，收完收尾（doc/19 §7）。
// 一个 run 一次 Run 调用；由 POST handler 在抢闸成功后以 goroutine 启动。
type Worker struct {
	store     *RedisStore
	chat      ChatStream
	translate Translator
	system    string
}

// NewWorker 组一个 worker。translate 传 TranslatorFor(config 的协议)；system 是系统消息（可为空）。
func NewWorker(store *RedisStore, chat ChatStream, translate Translator, system string) *Worker {
	return &Worker{store: store, chat: chat, translate: translate, system: system}
}

// Run 是 run 的执行体。**收尾无条件化**：endRun 挂在唯一出口（defer），不散在各级 return 前——
// 超时 / 429 / 连接断（Stream 或 Recv 报错）都走同一出口放闸，worker 自己收的尾（新 A3 用例 6 在真源侧的延续）。
// status 唯一的活变量：默认 complete，任何错误出口置 error。
func (w *Worker) Run(ctx context.Context, sessionID SessionID, runID RunID) {
	status := "complete"
	defer func() {
		_, _, ok, err := w.store.EndRun(ctx, sessionID, runID, status)
		switch {
		case err != nil:
			// 收尾失败 = 闸没放：那个 session 之后每次 POST 都 409，而 A7 的 reaper 之前没有别的解法。
			log.Printf("endRun %s/%s (%s): %v", sessionID, runID, status, err)
		case !ok:
			log.Printf("endRun %s/%s (%s): 闸已不在，空转", sessionID, runID, status)
		}
	}()

	msgs, err := w.modelInput(ctx, sessionID)
	if err != nil {
		log.Printf("modelHistory %s: %v", sessionID, err)
		status = "error"
		return
	}
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
				// run 已被 reset 清掉 / 闸已不在：停止推流。defer 的 EndRun 拿不到闸自然空转。
				return
			}
		}
	}
}

// modelInput 组模型侧消息：系统消息 + model_history 全序（user 在 AcquireRun 时已入，含本轮这条）。
// 读 model_history 而非 runlog——前者记「模型现在该看到什么」，后者记「产出了什么」（doc/19 §7 真源划分）。
func (w *Worker) modelInput(ctx context.Context, sessionID SessionID) ([]*schema.Message, error) {
	history, err := w.store.ModelMessages(ctx, sessionID)
	if err != nil {
		return nil, err
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
	return msgs, nil
}
