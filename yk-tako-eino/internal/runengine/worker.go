// 本文件：真 LLM worker（新 A6 转正）+ Translator 归一层（planning §5 第 7 条）。
//
// Translator 是 provider chunk → 归一 delta 文本的唯一翻译点；Worker.Run 是 AppendDelta 的唯一调用方：
// runlive 的帧协议（19v6-01 §2 {kind,messageId,text,status}）不随厂商变——厂商差异全部收在这一层，
// 兼容性由它承担，不是由存储侧承担。claude / openai / 原生 eino 各一个 translator：
// 现在同读 schema.Message.Content（思考与推理在 ReasoningContent，不属于回复正文、不进 delta 帧），
// 但分家写死——某家字段一旦分叉，只改对应那一个，帧协议与存储侧不动。
//
// T1 起主循环跑在 Eino ADK 上（adk.NewChatModelAgent；T2 起挂 get_time 工具）：Run 消费 agent 事件流、
// 把模型输出 chunk 交给同一个 Translator 翻成 delta 帧——归一层语义不变，换的只是载体
// （手写 Stream→Recv 循环 → agent.Run 的事件迭代）。compact 摘要仍直连模型、不另开 agent（doc/12 §4）。
//
// T2 的工具环（帧契约见 19v6-01 §2）：流式模型事件排干后拿到合并全文（ConcatMessages），
// 其中 ToolCalls 逐条压 tool_call 帧；非流式 Role=Tool 事件 = 工具结果（ADK 的 typedToolInvokeEvent），
// 压一条 tool_result 帧。两类事件同时进 turn（EndRun 落 mh 行 + runlog 扩展行）。
//
// T3 的帧路由：事件按 event.AgentName 归属——"" = root（主线程直走 agent.Run、不过 flowAgent 指名），
// 非空 = 子代理（AgentTool 内转发事件，经内层 flowAgent 着名、EmitInternalEvents 打开才到场）。
// 帧上带 Agent；子代理的 delta 走 AppendFrame（不写 draft——draft 只装 root），turn 收 root 与
// 子代理条目（T3-2 起带 Agent 归属，EndRun 按投影分拣：mh 只装 root、runlog 全量）。
// T3-1b：帧上再带 Model（生效 ref）——Run 建「agent 名 → 模型身份」表（agentModelTable）塞 ctx，
// 装载 handler 在同一 ctx 下填表，这里按 event.AgentName 查它：写帧取 model 值、drain 取 translator。
package runengine

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// ChatStream 是 worker 直连模型的口子：现在只剩 compact 摘要这一条路用（主循环已走 agent）；
// 摘要不另开 agent（doc/12 §4，一次 Generate 级的动作，不该有 run/闸/帧的语义）。
// eino 的 model.BaseChatModel（ToolCallingChatModel 的底）天然满足它。
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
	sum           *summarizer // compact 摘要：旋钮/stall/重试分流/三层验货一条龙（C2）
	agent         adk.Agent   // 主循环：ADK ChatModelAgent（T2 起挂 get_time）
	translate     Translator
	system        string
	rootRef       string // 主模型生效 ref（T3-1b，= config.ChatModel.Ref）：root 帧的 model 值 + 身份表 root 条目
	leaseRenew    time.Duration
	compactWindow int
	compactLimit  int // window×4/5（window=0 时 0）：触发线 / 准入线两处共用，不各自重算；摘要预算帽自 A7 起归 summarizeCeiling 分档
	counter       TokenCounter
}

// NewWorker 组一个 worker。chat 是两处模型的共同底座（model.BaseChatModel，
// 即 config 装配的 ToolCallingChatModel 的上界）：compact 摘要直连流 + 主 agent 的 Model。
// 工具经 ToolsConfig 交 ADK 内建工具环：模型给出 tool_calls → ToolsNode 执行 → 结果回注 → 模型续答；
// get_time 是 T2 的首个工具（无副作用、可 curl 造），构造失败 = 装配错误（fail fast 同款）。
// translate 传 TranslatorFor(config 的协议)；system 是系统消息（进 agent 的 Instruction——
// 模型输入 = 它打头的 system 消息 + 历史）；leaseRenew 是 watchdog 续约间隔（dev 10s）；
// compactWindow > 0 时**必须**给 counter（main 用 NewCl100kCounter 装配）——窗口开着却没有计数器是装配错误，
// 不静默降级成「不压缩」。agent 装配失败 = 启动错误，调用方 fail fast（同 config/Redis/PG 一贯式）。
//
// T3：agentsDir 是子代理规格目录——装载 handler 在每 run 起手扫盘（挂 Handlers 首位），
// 装配出的委派工具经 runCtx.Tools 进模型声明面；EmitInternalEvents 打开 AgentTool 的内转发
// （子代理事件由此汇进本 worker 消费的事件流）。
//
// F1：workspace 是文件工具的作业根（绝对路径，main 启动作 Abs + MkdirAll 后传入）——
// F 系列文件工具入池即受它约束；池内全部工具经 wrapToolErrors 包成「错误→结果」回注模型
// （tools.go 的机制说明）。
//
// C5-A7：modelRef 是主模型生效 ref（config.ChatModel.Ref，Claude override 判定用）、
// maxOutput 是 TAKO_MODEL_MAX_OUTPUT（正整数 tok，0 = 不顶）——摘要预算帽 =
// summarizeCeiling(compactWindow, modelRef, maxOutput)，装配期一次算好注给 summarizer。
//
// T3-1b：resolver 是子代理 `model:` 写值档的解析口（进程内 ModelRegistry，main 侧适配）——
// 装载 handler 拿它换出对应 ChatModel + 生效 ref + 协议。
//
// T3-3：subagentConcurrency 是委派并发闸容量（-subagent-concurrency，main 已保证 >= 1）——
// 池级信号量、全部规格共享；subagentReturnCap 是子代理回传上限字节数（-subagent-return-cap，
// <=0 = 不裁剪）——超帽截断的语义在 delegationTool（agents.go）。
func NewWorker(store *Store, chat model.BaseChatModel, translate Translator, system, workspace, agentsDir string, leaseRenew time.Duration,
	compactWindow int, modelRef string, maxOutput int, subagentConcurrency, subagentReturnCap int, resolver ModelResolver, counter TokenCounter) (*Worker, error) {
	// file_effect 启动 lint（C5-A4，fileops.go）：F1 起注册表非空（write_file 首条），
	// lint 从空转变成真检查——此后任何文件工具漏声明 = 启动失败。
	if err := lintFileEffects(firstPartyFileEffects); err != nil {
		return nil, err
	}
	getTime, err := newGetTimeTool()
	if err != nil {
		return nil, err
	}
	// 文件工具族入池：构造函数同签名（吃 workspace 根），循环装配——数量与拍号不在注释里
	// 复述（真清单即下方构造器列表，写数字会腐化）；每把经 wrapToolErrors 包成「错误→结果」
	// 回注模型（tools.go 的机制说明）。
	pool := []tool.BaseTool{wrapToolErrors(getTime)}
	for _, mk := range []func(string) (tool.BaseTool, error){
		newWriteFileTool, newReadTextFileTool, newReadFileTool, newEditFileTool,
		newMoveFileTool, newCreateDirectoryTool, newListDirectoryTool,
		newDirectoryTreeTool, newGetFileInfoTool, newSearchFilesTool, newReadMediaFileTool,
	} {
		t, err := mk(workspace)
		if err != nil {
			return nil, err
		}
		pool = append(pool, wrapToolErrors(t))
	}
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:        "yktako",
		Instruction: system,
		Model:       chat,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: pool,
			},
			EmitInternalEvents: true,
		},
		Handlers: []adk.ChatModelAgentMiddleware{newAgentSpecHandler(agentsDir, chat, modelRef, translate, resolver,
			make(chan struct{}, subagentConcurrency), subagentReturnCap), NewAgentLogHandler()},
	})
	if err != nil {
		return nil, err
	}
	compactLimit := compactWindow * 4 / 5
	ceiling := summarizeCeiling(compactWindow, modelRef, maxOutput)
	return &Worker{store: store, sum: newSummarizer(chat, ceiling), agent: agent, translate: translate, system: system,
		rootRef: modelRef, leaseRenew: leaseRenew, compactWindow: compactWindow, compactLimit: compactLimit, counter: counter}, nil
}

// Run 是 run 的执行体。**收尾无条件化**：endRun 挂在唯一出口（defer），不散在各级 return 前——
// 超时 / 429 / 连接断（agent 事件报错 / 输出流中途断）都走同一出口放闸，worker 自己收的尾（新 A3 用例 6 在真源侧的延续）。
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
	// turn 是本 run 模型侧消息的全序（T2）：流式事件合并全文（assistant 轮，带 tool_calls）与
	// 工具结果事件（tool 轮）按事件序累积，defer 交给 EndRun——complete 才落库，error/闸失零写入。
	// T3-2 起子代理条目也在列（带 Agent 归属）：分拣归 EndRun 的两个投影，worker 只负责记全。
	var turn []TurnEntry
	defer func() {
		_, _, ok, err := w.store.EndRun(ctx, sessionID, runID, status, stagedCompaction, turn)
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
	// T1：主循环 = ADK agent 事件流。正文 chunk 经同一 Translator 翻成 delta 帧（归一层语义不变，换的只是载体）。
	// T2：工具环两态事件——流式（模型输出，含 tool_calls）与非流式（工具结果，Role=Tool）。
	// T3-1b：身份表先落 root 条目（主 ref + 主 translator），塞 ctx 给装载 handler 填子代理条目
	// （handler 先于事件消费到场：agent.Run 内部同步跑完 BeforeAgent 才开事件 goroutine）。
	table := newAgentModelTable(w.rootRef, w.translate)
	ctx = withAgentModelTable(ctx, table)
	it := w.agent.Run(ctx, &adk.AgentInput{Messages: msgs, EnableStreaming: true})
	for {
		event, ok := it.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			log.Printf("agent %s/%s: %v", sessionID, runID, event.Err)
			status = "error"
			return
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mo := event.Output.MessageOutput
		agent := event.AgentName       // T3 路由："" = root / 非空 = 子代理（转发事件）
		entry := table.entryFor(agent) // T3-1b：该事件的模型身份（写帧的 model 值 + drain 的 translator）
		if !mo.IsStreaming {
			// 非流式事件 = 工具结果（ADK typedToolInvokeEvent：Message 已成形、Role=Tool）。一次调用一条
			// tool_result 帧（非流式，19v6-01 §2）；同时进 turn（mh tool 行 + runlog 扩展行）。
			if mo.Role == schema.Tool {
				out, callID, name := "", "", mo.ToolName
				if mo.Message != nil {
					out = mo.Message.Content
					callID = mo.Message.ToolCallID
					if name == "" {
						name = mo.Message.ToolName
					}
				}
				// T3-2：子代理条目也收（root 的委派回传事件 agent 为空——它属于 root 的循环）。
				turn = append(turn, TurnEntry{Role: "tool", Agent: agent, Text: out, ToolCallID: callID, ToolName: name, Model: entry.ref})
				live, err := w.store.AppendFrame(ctx, runID, Frame{Kind: "tool_result", Agent: agent, Model: entry.ref, Name: name, Output: out})
				if err != nil {
					log.Printf("frame %s/%s: %v", sessionID, runID, err)
					status = "error"
					return
				}
				if !live {
					return // 同 drain 的判死出口：run 已没了，该收摊
				}
			}
			continue
		}
		merged, live, err := w.drain(ctx, runID, agent, entry, mo.MessageStream)
		if err != nil {
			log.Printf("stream %s/%s: %v", sessionID, runID, err)
			status = "error"
			return
		}
		if !live {
			// run 已被判死（draft 随租约到期蒸发）/ 被 reset 清掉 / 闸已不在：停止推流。
			// defer 的 EndRun 拿不到闸自然空转。
			return
		}
		// 本轮合并全文进 turn（assistant 轮，root 与子代理都收——T3-2 起按 Agent 分拣归 EndRun）；
		// ToolCalls 逐条压 tool_call 帧，args 取模型原始 JSON 原文。
		turn = append(turn, TurnEntry{Role: "assistant", Agent: agent, Text: merged.Content, ToolCalls: merged.ToolCalls, Model: entry.ref})
		for _, tc := range merged.ToolCalls {
			live, err := w.store.AppendFrame(ctx, runID, Frame{Kind: "tool_call", Agent: agent, Model: entry.ref, Name: tc.Function.Name, Args: tc.Function.Arguments})
			if err != nil {
				log.Printf("frame %s/%s: %v", sessionID, runID, err)
				status = "error"
				return
			}
			if !live {
				return
			}
		}
	}
}

// drain 排干一条模型输出流：chunk → Translator → delta 帧；EOF 时把全部 chunk 合并成完整消息返回
// （T2：merged.ToolCalls 是 tool_call 帧的来源、merged.Content 是本轮 assistant 行）。
// T3：agent = "" 走 AppendDelta（root——draft 的唯一写入方）；子代理文本走 AppendFrame
// （delta 帧带 agent，**不 APPEND draft**——收口锚「Σ root delta」不被子代理文本污染）。
// T3-1b：drain 的 translator 与 model 值都取自 entry（该 agent 的模型身份）——继承档 = 主 translator/主 ref，
// 写值档 = 按该模型协议选的 translator / 其生效 ref；root 与子代理共用同一条排干逻辑。
// 返回 live=false = run 已判死（该收摊，不是错误）；err != nil = 错误结局。
// close 责任也在本函数：事件流的另一副本由 agent 内部排干，早退不关会把两边一起吊住。
func (w *Worker) drain(ctx context.Context, runID RunID, agent string, entry agentModelEntry, stream *schema.StreamReader[*schema.Message]) (*schema.Message, bool, error) {
	defer stream.Close()
	var chunks []*schema.Message
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			// ConcatMessages 空输入返回零值 Message（无错）：空流不该出现，真出现也只是空轮、不炸。
			merged, cerr := schema.ConcatMessages(chunks)
			if cerr != nil {
				return nil, false, cerr
			}
			return merged, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		chunks = append(chunks, chunk)
		if text, ok := entry.translate(chunk); ok {
			var live bool
			if agent == "" {
				_, live, err = w.store.AppendDelta(ctx, runID, entry.ref, text)
			} else {
				live, err = w.store.AppendFrame(ctx, runID, Frame{Kind: "delta", Agent: agent, Model: entry.ref, Text: text})
			}
			if err != nil {
				return nil, false, err
			}
			if !live {
				return nil, false, nil
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

// AdmitText 是 POST 单条消息的准入检查（C5-A1，tail 支）：compact 关闭（window=0）时一切放行；
// 开启时单条 user 消息计数 > limit/compactAdmissionDivisor ⇒ 不放行，由调用方回 413。
// 返回 (limit, tokens, ok)：limit = window×4/5（与触发折叠同一 limit，不发明第二个数）；
// ok=false 时 limit/tokens 供错误体带回去（报清原因，客户端可裁剪重发）。策略本身属 compact
// 域（19v6-07 §5 的三等分推导），但用的是 Worker 的 window/counter——所以是 Worker 方法、
// 住在 prepareInput 旁边：同一道「发送前自测」的门，长度检查的两个刻度。
func (w *Worker) AdmitText(text string) (limit, tokens int, ok bool) {
	if w.compactWindow == 0 || w.counter == nil {
		return 0, 0, true
	}
	limit = w.compactLimit
	tokens = w.counter.Count(text)
	return limit, tokens, tokens <= limit/compactAdmissionDivisor
}

// prepareInput 组模型侧消息：model_history 全序（user 在 AcquireRun 时已入，含本轮这条）。
// **不拼 system**（T1 起）：agent 的 Instruction 就是系统提示词，ADK defaultGenModelInput 会在
// 消息头拼 SystemMessage(instruction)——这里再拼一条，模型输入里 system 会出现两遍。
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
		limit := w.compactLimit
		if total := countInput(w.counter, w.system, history); total > limit {
			log.Printf("compact %s/%s: 输入 %d tok > limit %d，进入折叠", sessionID, runID, total, limit)
			history, stagedCompaction, err = foldInput(ctx, w.system, history, runID, limit, w.counter, w.sum.summarize, firstPartyFileEffects)
			if err != nil {
				return nil, nil, err
			}
			log.Printf("compact %s/%s: 压 %d 个 run，摘要 %d tok，挂 %s；发送输入 %d tok < limit %d",
				sessionID, runID, len(stagedCompaction.DropRunIDs), w.counter.Count(stagedCompaction.Summary),
				stagedCompaction.AttachRun, countInput(w.counter, w.system, history), limit)
		}
	}
	msgs := make([]*schema.Message, 0, len(history))
	for _, h := range history {
		switch h.Role {
		case "assistant":
			// ToolCalls 回放（T2）：带 tool_calls 的 assistant 行原样还给模型（OpenAI 协议要求
			// 其后紧跟对应的 tool 行）；老行无 tool_calls，nil 即原样。
			msgs = append(msgs, schema.AssistantMessage(h.Text, h.ToolCalls))
		case "tool":
			// tool 行回放（T2）：结果文本 + 回指的 tool_call_id——带上会话再投时，
			// 模型凭这里看到上一轮的工具结果（19v6-01 §3 的 mh 真源划分）。
			msgs = append(msgs, schema.ToolMessage(h.Text, h.ToolCallID))
		default:
			msgs = append(msgs, schema.UserMessage(h.Text))
		}
	}
	return msgs, stagedCompaction, nil
}
