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
// A4 起折叠链挂进「每次模型调用前」（midfoldHandler，midfold.go）：run 中途超线即折——本文件的
// 职责随之多一件：建 foldBox 塞 ctx、prepareInput 填 base、defer 从容器取 staged 与折后行落库。
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
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// agentMaxIterations 是单次 run 的模型调用上限（主代理与子代理共用；2026-10-16 用户裁定 50）。
// eino ADK 默认 20，B4 活体发现这个默认值同时卡着折叠链：当前 run 的折叠单位数 ≤ 模型调用数，
// 而「封顶出口」（折叠尝试封顶后仍超、候选里还剩工具行）需要单位数 ≥ 21 才存在——那是
// trimToolResults 唯一的出场位置，默认值下 trim 恒空转。上限越高 = 单 run 能跑越长的工具循环，
// 是产品取舍（50 为用户拍值），不是技术最小值。
const agentMaxIterations = 50

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
// compactWindow / counter 是长度自测（C 系列）：window > 0 时发送前数 token，超 limit = compactLimitFor(window)
// 走折叠（compact.go）；0 = 关压缩（counter 为 nil 即可，计数与摘要都不发生）。
type Worker struct {
	store      *Store
	sum        *summarizer // compact 摘要：旋钮/stall/重试分流/三层验货一条龙（C2）
	agent      adk.Agent   // 主循环：ADK ChatModelAgent（T2 起挂 get_time）
	translate  Translator
	system     string
	rootRef    string // 主模型生效 ref（T3-1b，= config.ChatModel.Ref）：root 帧的 model 值 + 身份表 root 条目
	leaseRenew time.Duration
	// workspace 是作业根（F1，绝对路径）：文件工具构造期消费它入池；doc/32 起入口媒体落盘
	// 与「第一枪还原」记账（A3/translator 后）也要它——存一份给 Workspace() 取用。
	workspace     string
	compactWindow int
	compactLimit  int // = compactLimitFor(window)（window=0 时 0）：触发线 / 准入线两处共用，不各自重算；摘要预算帽自 A7 起归 summarizeCeiling 分档
	counter       TokenCounter
	// runCancels 是手动停止（doc/17 §4）的进程内取消注册表：runID → *runCancel。
	// stop 端点赢竞态后查它，命中（本进程在跑这场 run）即 cancel、不等续约 tick（用户停止永远最大，
	// 不看 epoch）；跨副本传播走 watchdog：续约 CAS 0 行 → 查它、**epoch 相同才** cancel
	// （doc/30 §7：被接管后旧执行者的 cancel 不能误伤接管者，最坏一个续约间隔）。
	runCancels sync.Map
}

// runCancel 是注册表的值：取消把柄 + 它属于哪一代执行者。epoch 让 Run 的退出清理
// （CompareAndDelete 指针身份）与 watchdog 的判死 cancel 都只作用于自己那一代。
type runCancel struct {
	epoch  int64
	cancel context.CancelFunc
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
	// bash 执行工具入池（doc/17 §3）：11 个文件工具后、memory 前——批量活先写脚本再跑；
	// 作业根 workspace（相对路径在 workspace 内解析），32 KB 帽 / 环境白名单在 bash.go（无自动超时，2026-10-15 用户裁）。
	bash, err := newBashTool(workspace)
	if err != nil {
		return nil, err
	}
	pool = append(pool, wrapToolErrors(bash))
	// 笔记工具入池（M1，notes.go）：文件工具族之后 append——继承整池的子代理天然拿到它；
	// researcher 走的是 exclude_tools 排除路（doc/26），memory 不在排除项里，仍在它的有效名单。
	// 写入逻辑全在 writeNote。
	mem, err := newMemoryTool(store, workspace)
	if err != nil {
		return nil, err
	}
	pool = append(pool, wrapToolErrors(mem))
	// 历史查找工具入池（H1，doc/21）：memory 之后 append——身份从 ctx 的 noteBox 取
	// （session/run），只读 runlog_entries（工具参数里没有会话号）；H3 起吃 workspace 根
	// （截断落盘写 <workspace>/.tako/search/）。
	sh, err := newSearchHistoryTool(store, workspace)
	if err != nil {
		return nil, err
	}
	pool = append(pool, wrapToolErrors(sh))
	// 网络搜索工具入池（doc/22）：search_history 之后 append（doc/22 §3「排在 search_history
	// 后面」）；选择器在 websearch.go，按调用方 ctx 标记取生效 ref 的厂商 id。workspace 是
	// 结果落盘作业根（.tako/search/，沿 search_history 的 spill 机制），counter 与折叠链同一份
	// （描述段 token 数同口径，nil = -compact-window 0 时不报 token）。
	ws, err := newWebSearchTool(workspace, counter)
	if err != nil {
		return nil, err
	}
	pool = append(pool, wrapToolErrors(ws))
	// 摘要器在 agent 装配前构造（A4，planning 裁定 9）：midfoldHandler 要拿它的 summarize 方法值——
	// mid-run 折叠与 prepareInput 的 Run fold 共用同一个摘要器、同一顶帽（C5-A7 分档）。
	compactLimit := CompactLimitFor(compactWindow)
	ceiling := summarizeCeiling(compactWindow, modelRef, maxOutput)
	sum := newSummarizer(chat, ceiling)
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:        "yktako",
		Instruction: system,
		Model:       chat,
		// 单 run 模型调用上限：eino 默认 20，2026-10-16 用户裁定放开到 50（agentMaxIterations，理由见常量注释）。
		MaxIterations: agentMaxIterations,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: pool,
			},
			EmitInternalEvents: true,
		},
		Handlers: []adk.ChatModelAgentMiddleware{
			// 调用方身份标记（doc/22 §4）：root 挂 ""（与 agentModelTable 的 root key 一致），
			// 放链首 = 日志里它先于其余 handler 出现；其余两个 BeforeAgent handler 的先后有语义：
			// mcp 工具必须先于 agentSpecHandler append（doc/33 §4.2——子代理的入口快照 base 要含 MCP 工具）。
			&callerStampMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, name: ""},
			// MCP 工具入池（doc/33 §4.2/§5）：run 起手扫 mcp.json、逐 server 拉起、append runCtx.Tools；
			// 无配置/连不上 = 本 run 无此类工具照跑，不当致命。
			&mcpToolHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, confPath: defaultMcpConfPath},
			newAgentSpecHandler(agentsDir, store, workspace, chat, modelRef, translate, resolver,
				make(chan struct{}, subagentConcurrency), subagentReturnCap,
				counter, compactWindow, compactLimit, sum.summarize, firstPartyFileEffects),
			// 笔记快照缝（M1，notes.go）：BeforeAgent 渲染快照、每轮模型调用前把 system 重写为
			// Instruction + 快照段；排在 midfold 之前——midfold 从 state 取 system 计数，与模型实见同源。
			newNoteSnapshotHandler(store, workspace, system),
			// mid-run fold 钩子排在装载 handler 之后、观测 handler 之前（19v6-10 §4）：
			// agentLog 看到的是模型实际所见的折后输入。
			newMidfoldHandler(counter, compactWindow, compactLimit, sum.summarize, firstPartyFileEffects),
			NewAgentLogHandler(),
			// 溢出 guard（B1 观测 → B2 救援链，D13；救援依赖与 midfold 同组装配件）：
			// 链尾 = 最内 user wrapper——模型调用错误冒泡最先到它（wrappers.go:1129 逆序包裹、
			// first registered = outermost）。四条件同真就折被拒输入重调一次；零帧/闸花原样上抛。
			newOverflowGuard(compactWindow, counter, sum.summarize, firstPartyFileEffects),
			// 崩溃账本包壳（doc/30 §6.3）：必须排在所有 append runCtx.Tools 的 handler 之后——
			// 一手池、MCP（mcpToolHandler）、委派（agentSpecHandler）与以后新增的工具一处覆盖。
			// 子代理入口快照在此前已完成，拿的是未包壳工具，内部调用不记账。
			&journalWrapHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}, store: store},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Worker{store: store, sum: sum, agent: agent, translate: translate, system: system,
		rootRef: modelRef, leaseRenew: leaseRenew, workspace: workspace, compactWindow: compactWindow, compactLimit: compactLimit, counter: counter}, nil
}

// Workspace 取作业根（doc/32 A1/A4）：Server 的媒体落盘与取图端点经它拿到与文件工具
// 同一个根——同一把牢笼，不传第二份路径。
func (w *Worker) Workspace() string { return w.workspace }

// Run 是 run 的执行体。**收尾无条件化**：endRun 挂在唯一出口（defer），不散在各级 return 前——
// 超时 / 429 / 连接断（agent 事件报错 / 输出流中途断）都走同一出口放闸，worker 自己收的尾（新 A3 用例 6 在真源侧的延续）。
// status 唯一的活变量：默认 complete，任何错误出口置 error。
//
// compact（C 系列；A4 起含 mid-run）：prepareInput 在发送前把 Run fold Stage 进容器（foldBox），
// mid-run 各次折叠由钩子在模型调用前追加——一轮 run 可有多次：Run fold 打头、mid-run 各次续接；
// 落库靠 defer 的 EndRun——仅 status=complete 且闸在手上才并入同一笔事务。error / 闸失 / 进程死 = 零写入。
//
// A3（检出②）从两端各进一件：消费侧对每次 root 模型回复做截断子检（超窗行 + 强折意图），
// run 尾部对 mh 强折一次备下轮——②强折失败不杀 run（答案已在手），与①失败走 error 的不对称见 doc §7。
//
// **watchdog 生命周期严格在 EndRun 之前结束**（A7）：defer 是 LIFO，EndRun 先注册故最后跑；
// 停 watchdog 的 defer 后注册故先跑，且 `wg.Wait()` 确保续约 goroutine 完全退出——否则续约会把 runlive
// 定稿后的 300s TTL 覆盖回 lease 的 30s（D1 落库后终态行的续约被 PG 守卫挡成空转，这条顺序仍保持：
// 它同时保证「最后一次续约」早于放闸，语义最干净）。
// epoch 是 lease_epoch 真值（doc/30 §7）：POST 路径传 1，续跑路径传 claim 返回值。
// 它进 journalTarget 与所有 Redis 帧写；watchdog 的判死 cancel 也按它认代。
func (w *Worker) Run(ctx context.Context, sessionID SessionID, runID RunID, epoch int64) {
	w.run(ctx, sessionID, runID, epoch, nil)
}

// RunResume 是续跑入口（doc/30 §9）：与 Run 共用同一条 run 主体，差异只有两处——
// turn 从账本重放产物起手（EndRun 的 runlog/mh 投影要把中断前已发生的行一并定稿），
// 模型输入 = mh 全序 + replay.Rows（已发生的轮次不进 mh，只在本轮输入里补上）。
// replay.Final = 账本最后一条 assistant 无 tool_calls（答案已说完、只差放闸）：不调模型，直接定稿。
func (w *Worker) RunResume(ctx context.Context, sessionID SessionID, runID RunID, epoch int64, replay journalReplay) {
	w.run(ctx, sessionID, runID, epoch, &replay)
}

func (w *Worker) run(ctx context.Context, sessionID SessionID, runID RunID, epoch int64, resume *journalReplay) {
	status := "complete"
	// 手动停止（doc/17 §4）：run ctx 包一层可取消并注册——stop 端点 / watchdog 判死都经
	// 注册表 cancel 这一个把柄，模型流与工具（子代理调用在 ctx.Done 上返回）随取消 unwind。
	// 退出清理按指针身份删自己那一条：续跑接管后注册表里已是新一代的值，旧 Run 不许误删。
	ctx, cancelRun := context.WithCancel(ctx)
	rc := &runCancel{epoch: epoch, cancel: cancelRun}
	w.runCancels.Store(runID, rc)
	defer w.runCancels.CompareAndDelete(runID, rc)
	defer cancelRun()
	// run 级折叠容器（A4，planning 裁定 4）：塞进 ctx 供 midfoldHandler 读写；prepareInput 填
	// base 与 Run fold 的 staged，defer 从这里取货落库。
	box := newFoldBox(sessionID, runID)
	ctx = withFoldBox(ctx, box)
	// 崩溃账本坐标（doc/30 A2）：本 run 的 id 与接管令牌（POST 路径 1，续跑路径 = claim 返回值，
	// A3 起所有写点都带它）。这里塞进的是 agent.Run 与全部工具调用共用的 ctx——工具包装层的
	// intent/result 都读它。
	ctx = withJournalTarget(ctx, journalTarget{runID: runID, epoch: epoch})
	// turn 是本 run 模型侧消息的全序（T2）：流式事件合并全文（assistant 轮，带 tool_calls）与
	// 工具结果事件（tool 轮）按事件序累积，defer 交给 EndRun——complete 才落库，error/闸失零写入。
	// T3-2 起子代理条目也在列（带 Agent 归属）：分拣归 EndRun 的两个投影，worker 只负责记全。
	// 续跑（doc/30 §9）：起手先把账本重放出的已发生轮次装进 turn——EndRun 定稿时它们与
	// 本次新产出的条目同笔落 runlog/mh，不丢中断前的半场。
	var turn []TurnEntry
	if resume != nil {
		turn = resume.Turn
	}
	defer func() {
		// mh 写入源 = 折后 state 派生（A4）：RunID==currentRun 且 Seq≥2 的行、按位置从 2 连续
		// 重编（mid-fold 折走单位留下的 seq 空洞在此闭合）。final 为空 = 不可达态（成功终态
		// AfterAgent 恒已填），一行守卫兜回 turn 派生——error/取消本就不落库，兜底值不上账。
		var mhRows []ModelMessage
		if final := box.finalRows(); final != nil {
			mhRows = mhRowsFromFold(runID, final)
		} else {
			mhRows = mhRowsFromTurn(runID, turn)
		}
		// WithoutCancel（doc/17 §4 D4）：stop 判死后本 ctx 已取消，但 stopped 收尾还要写
		// 已配对工具行——收尾的 PG/Redis 调用不能吃取消；foldBox 等 ctx 值仍要带上。
		_, _, ok, err := w.store.EndRun(context.WithoutCancel(ctx), sessionID, runID, epoch, status, box.stagedList(), mhRows, turn)
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
		w.watchdog(leaseCtx, sessionID, runID, epoch)
	}()
	defer func() { cancelLease(); wg.Wait() }()

	// 笔记的 run 身份（M1，notes.go）：本 run user 行的 runlog seq——AcquireRun 已入该行；
	// 工具写 notes 时以 (run id, seq) 填 source 两列（对质入口：从笔记行找回原话现场）。
	// 查询失败走 run error（与 prepareInput 的失败形态同款，零写入家族）。
	userSeq, err := w.store.runlogUserSeq(ctx, sessionID, runID)
	if err != nil {
		log.Printf("note source %s/%s: %v", sessionID, runID, err)
		status = "error"
		return
	}
	ctx = withNoteBox(ctx, newNoteBox(sessionID, runID, userSeq))

	// 续跑终稿态（doc/30 §9.1）：账本最后一条 assistant 无 tool_calls = 模型已说完、只差放闸。
	// 不调模型、不组输入——turn 里已是完整半场，直接走 defer 的 EndRun 定稿。
	if resume != nil && resume.Final {
		log.Printf("resume %s/%s: 账本终稿已在手（epoch %d，turn %d 条），直接定稿", sessionID, runID, epoch, len(turn))
		return
	}
	var msgs []*schema.Message
	if resume != nil {
		// 续跑输入 = mh 全序 + 账本重放行（doc/30 §9.1）：中断前的轮次从未进 mh（EndRun 没跑），
		// 只能由账本恢复；折叠自测仍走同一条 prepareInputFrom。
		history, herr := w.store.ModelMessages(ctx, sessionID)
		if herr != nil {
			log.Printf("prepareInput %s/%s: %v", sessionID, runID, herr)
			status = "error"
			return
		}
		history = append(history, resume.Rows...)
		msgs, err = w.prepareInputFrom(ctx, sessionID, runID, history)
	} else {
		msgs, err = w.prepareInput(ctx, sessionID, runID)
	}
	if err != nil {
		// compact 的 errStillOver 也走这里：不调主模型、status=error、staged 零写入（闸放、无库写）。
		log.Printf("prepareInput %s/%s: %v", sessionID, runID, err)
		status = "error"
		return
	}
	// 记忆召回注入（doc/29 §6.1/§6.4）：prepareInput 之后、agent.Run 之前；只注入新 run
	// （续跑路径的输入尾不一定是 user，且它本就不是新一场开头）。块只加在消息副本上，
	// mh 投影走 foldBox 的 base，没经过这里——永不落库。
	if resume == nil {
		w.injectRecall(ctx, sessionID, runID, msgs)
	}
	// T1：主循环 = ADK agent 事件流。正文 chunk 经同一 Translator 翻成 delta 帧（归一层语义不变，换的只是载体）。
	// T2：工具环两态事件——流式（模型输出，含 tool_calls）与非流式（工具结果，Role=Tool）。
	// T3-1b：身份表先落 root 条目（主 ref + 主 translator），塞 ctx 给装载 handler 填子代理条目
	// （handler 先于事件消费到场：agent.Run 内部同步跑完 BeforeAgent 才开事件 goroutine）。
	// A2（doc §6 / planning 裁定 8）：agent.Run + 事件消费整体包进外层循环——零帧拒收时强折一次、
	// 同 ctx 再调一次 agent.Run（watchdog 同闸同租约，无新生命周期）；谓词未命中 / 重试再拒
	// 照走同一段 error 出口（出口不复制）。
	table := newAgentModelTable(w.rootRef, w.translate)
	ctx = withAgentModelTable(ctx, table)
	// 救援状态（planning 裁定 7/8）：帧计数与一次性闸都是 per-run，重跑不重置——
	// 重试再拒时谓词读同一份（此时计数或闸已花，救援路径必然出不去）。
	// 帧计数自 B1 起收编 foldBox（D12：guard 在模型调用点，worker 局部变量在它视野外）——
	// 计数唯一真源 = box.frameCount()，worker 谓词与 guard 让位判据同源读。
	// 一次性闸自 B2 起同样收编（D12，同一理由）：box.recovered() / box.tryMarkRecovered()，
	// 三个消费方——A2 谓词、A3 检出②、B2 guard 救援——看同一个闸，guard 救过 A3 就不再触发检②。
	// pendingForceFold 是检出②（A3）的强折意图：>0 = 本 run 检测到截断、run 末强折的目标线
	// 基数（run 起手放行估算 C，doc §7）；写入发生在事件流排完后（Run 尾部）。
	pendingForceFold := 0
	for {
		it := w.agent.Run(ctx, &adk.AgentInput{Messages: msgs, EnableStreaming: true})
		retry := false
		for {
			event, ok := it.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				overflow := w.compactWindow > 0 && IsOverflowError(event.Err)
				if rescueWorthy(overflow, event.AgentName, box.frameCount(), box.recovered()) {
					// 救援命中（doc §6）：闸先花（D5——无论强折成败，本 run 不再有第二次救援），
					// 强折 mh 原文 + foldBox 同步，折成即换消息重跑；折不成记分类、原始错误照走。
					box.tryMarkRecovered()
					next, rerr := w.rescueOverflow(ctx, box, event.Err)
					if rerr == nil {
						msgs = next
						retry = true
						break
					}
					log.Printf("overflow rescue %s/%s: %v", sessionID, runID, rerr)
				} else if overflow {
					// 谓词未命中而 ① 命中 → detect 行原样（A1：中途/子代理/闸花都打，不救但留观测）；
					// 闸花附一句（planning 裁定 2——「重试再拒」在日志里的可读证据）。
					window := "未知"
					if n, ok := extractWindow(event.Err); ok {
						window = strconv.Itoa(n)
					}
					log.Printf("overflow: provider 拒收（估算 %d tok；真值 %s）", box.countBase(), window)
					if box.recovered() {
						log.Printf("overflow: 已救过不再救")
					}
				}
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
					live, err := w.store.AppendFrame(ctx, runID, epoch, Frame{Kind: "tool_result", Agent: agent, Model: entry.ref, Name: name, Output: out})
					if err != nil {
						log.Printf("frame %s/%s: %v", sessionID, runID, err)
						status = "error"
						return
					}
					if !live {
						return // 同 drain 的判死出口：run 已没了，该收摊
					}
					box.addFrames(1) // A2 帧计数（裁定 7：不分 root/子代理——有帧后重跑同样造成帧重复；B1 收编进 box）
				}
				continue
			}
			merged, live, err := w.drain(ctx, runID, epoch, agent, entry, mo.MessageStream, box)
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
			// A3 检出②（doc §5/§7；只检主线程——折的是 mh/foldBox 这些 root 的量，子代理不救，§11）：
			// 成功但被窗口截断。merged 的 ResponseMeta 带着 FinishReason 与 Usage（openai 组件恒开
			// IncludeUsage、claude messageDelta 同面带，C2 plumbing 实读）。超窗行恒打（观测面：
			// 实数 P 与估算 C 的偏差供运营商修窗标）；强折意图只记一次（闸与①/B2 guard 共用，D5——
			// 命中即花掉，一次 run 一个强折动作）。本答不救（D7）：帧面 append-only 撤不回，半截答案原样
			// 留给用户，run 正常 complete——救的是会话（run 尾部强折落库，见函数末）。
			if agent == "" {
				if hit := detectTruncation(merged.ResponseMeta, w.compactWindow); hit != nil {
					suffix := ""
					if hit.subB {
						suffix = "；finish_reason=length, completion=0"
					}
					log.Printf("overflow: 实数超窗：prompt_tokens %d > window %d（估算 %d tok；偏差 %d）%s",
						hit.prompt, w.compactWindow, box.countBase(), hit.prompt-box.countBase(), suffix)
					if box.tryMarkRecovered() {
						pendingForceFold = box.countBase()
					}
				}
			}
			// usage 帧（doc/27 §4a）：每枪回执后压一帧 provider 账单进 runlive。root 与子代理同点
			// （帧 agent 字段区分，前端第一版只渲染 root）。账单缺失或三数全 0 = 厂商没报 → 不发帧
			// （不发 0，与 doc/24 usageBasedInput 的锚点判据同一句）。window 是 -compact-window
			// 配置值非模型真值（真值校准归溢出层），0 = 未配窗、字段省略。同一账单随本轮 assistant
			// 条目进 turn（A3：EndRun 把**最后一枪**的账单落终稿行 payload 列）。
			bill, billOK := usageBill(merged, w.compactWindow)
			// 崩溃账本 assistant 行（doc/30 §6.3）：drain 已排干 → 先落账，再发 usage/tool_call
			// 帧、再进 turn。只记 root（子代理对 root 只是一次委派结果）。写失败 = run error，
			// 不静默降级——账本丢一段，续跑重放出的输入就会与真实步骤不一致。
			if agent == "" {
				var billed *UsageBill
				if billOK {
					billed = bill
				}
				if err := writeJournalAssistant(ctx, w.store, merged, entry.ref, billed); err != nil {
					log.Printf("journal assistant %s/%s: %v", sessionID, runID, err)
					status = "error"
					return
				}
			}
			if billOK {
				live, err := w.store.AppendFrame(ctx, runID, epoch, usageFrame(bill, agent, entry.ref))
				if err != nil {
					log.Printf("frame %s/%s: %v", sessionID, runID, err)
					status = "error"
					return
				}
				if !live {
					return
				}
				box.addFrames(1)
			}
			// 本轮合并全文进 turn（assistant 轮，root 与子代理都收——T3-2 起按 Agent 分拣归 EndRun）；
			// ToolCalls 逐条压 tool_call 帧，args 取模型原始 JSON 原文。
			turn = append(turn, TurnEntry{Role: "assistant", Agent: agent, Text: merged.Content, ToolCalls: merged.ToolCalls, Model: entry.ref, Usage: bill})
			for _, tc := range merged.ToolCalls {
				live, err := w.store.AppendFrame(ctx, runID, epoch, Frame{Kind: "tool_call", Agent: agent, Model: entry.ref, Name: tc.Function.Name, Args: tc.Function.Arguments})
				if err != nil {
					log.Printf("frame %s/%s: %v", sessionID, runID, err)
					status = "error"
					return
				}
				if !live {
					return
				}
				box.addFrames(1)
			}
		}
		if !retry {
			break
		}
	}
	// A3 run 末强折（doc §7）：②命中过 → 对 mh 强折一次备下轮。失败**不杀 run**——答案已在手
	// （doc §7 的不对称分界：与①强折失败走 error 相对），日志登记了事。折产物进容器，随 defer
	// 的 EndRun 同笔事务落库——少了这一步，省下的只是本 run 的内存、下条消息重演（D6 另一半）。
	if pendingForceFold > 0 && status == "complete" {
		if err := w.runEndFold(ctx, box, pendingForceFold); err != nil {
			log.Printf("overflow 末折 %s/%s: %v", sessionID, runID, err)
		}
	}
}

// runEndFold 是 A3 检出②的强折段（doc §7）：重读 mh 原文（裁定 5 同款：不沿用本地变量）→
// foldInput 强制进入（limit' = C×4/5，D4 一族——被掐只证明「该折了」，没有真值可校准；
// C 低估 = 折更深 = 安全方向）→ 摘要行打戳 + foldBox 同步（裁定 6 同款路径）→ 末折行。
// 折的都是老 run（当前 run 的 assistant/tool 行还没进 mh，phase 2 天然空转）——产物靠 EndRun
// 的并集翻转 + 同槽 UPSERT 落库；失败（errStillOver / summarize 败）由调用方记日志、不杀 run。
func (w *Worker) runEndFold(ctx context.Context, box *foldBox, c int) error {
	history, err := w.store.ModelMessages(ctx, box.session)
	if err != nil {
		return fmt.Errorf("重读 mh: %w", err)
	}
	folded, staged, err := foldInput(ctx, w.system, history, box.run, c*4/5, w.compactWindow, w.counter, w.sum.summarize, firstPartyFileEffects)
	if err != nil {
		return fmt.Errorf("强折: %w", err)
	}
	folded[0].RunID = staged.AttachRun
	folded[0].Seq = 0
	box.setBase(folded)
	box.appendStaged(*staged)
	log.Printf("overflow 末折: 强折 %d 个 run，摘要 %d tok；备下轮",
		len(staged.DropRunIDs)+len(staged.MergedSummaryRunIDs), w.counter.Count(staged.Summary))
	return nil
}

// rescueOverflow 是 A2 救援的强折段（doc §6；谓词与一次性闸在调用方）：重读 mh 可见行（裁定 5：
// 原料要原文，不沿用 prepareInput 本地变量）→ D4 目标线（rescueLimit）→ foldInput 强制进入 →
// 摘要行打戳（AttachRun, seq0——同 midfold 的同步路径，裁定 6）→ foldBox 同步（base=折算行 +
// staged 追加）→ rescue 行（格式钉死，裁定 2）→ 返回重跑用的消息。
// 返回错误 = 重读失败 / 强折失败（含 errStillOver 与 summarize 失败）：调用方记分类、原始错误
// 走既有 error 出口——强折产物不落库（零写入家族，doc §10「不发明第四态」）。
func (w *Worker) rescueOverflow(ctx context.Context, box *foldBox, cause error) ([]*schema.Message, error) {
	history, err := w.store.ModelMessages(ctx, box.session)
	if err != nil {
		return nil, fmt.Errorf("重读 mh: %w", err)
	}
	window, hasWindow := extractWindow(cause)
	limit := rescueLimit(box.countBase(), window, hasWindow)
	folded, staged, err := foldInput(ctx, w.system, history, box.run, limit, window, w.counter, w.sum.summarize, firstPartyFileEffects)
	if err != nil {
		return nil, fmt.Errorf("强折: %w", err)
	}
	folded[0].RunID = staged.AttachRun
	folded[0].Seq = 0
	box.setBase(folded)
	box.appendStaged(*staged)
	truth := "盲降"
	if hasWindow {
		truth = "真值 " + strconv.Itoa(window)
	}
	log.Printf("overflow rescue: provider 拒收（估算 %d tok；%s）；强折 %d 个 run，摘要 %d tok；重试",
		box.countBase(), truth, len(staged.DropRunIDs)+len(staged.MergedSummaryRunIDs), w.counter.Count(staged.Summary))
	return toSchemaMessages(folded), nil
}

// mhRowsFromTurn 把 turn 派生为 mh 写入行（A3，planning 裁定 3）：root 条目（Agent 空）逐条映射、
// Seq 从 2 连续排（user 占 1，被跳过的子代理条目不占号）——与旧 EndRun 内联循环逐字节等价。
// 转换知识归 worker：存储层只做机械搬运（m.Seq / m.Role / m.Text / 两列组装）。
// A4 起 mhRows 主源是折后 state 派生（mhRowsFromFold）；本函数留作 final 缺失（不可达态）的兜底。
func mhRowsFromTurn(runID RunID, turn []TurnEntry) []ModelMessage {
	rows := make([]ModelMessage, 0, len(turn))
	seq := 2
	for _, e := range turn {
		if e.Agent != "" {
			continue
		}
		rows = append(rows, ModelMessage{RunID: runID, Seq: seq, Role: e.Role, Text: e.Text,
			ToolCalls: e.ToolCalls, ToolCallID: e.ToolCallID})
		seq++
	}
	return rows
}

// usageBill 从排干合并后的回执消息取 provider 账单（doc/27 §4a）。ok=false = 账单缺失
// （ResponseMeta/Usage nil，或 Prompt/Completion/Total 三数全 0 = 厂商没报）——不发帧、不发 0、
// turn 条目不带账单。promptTokens 即本枪发出的完整输入大小（含 system/schema/全部历史）＝
// 上下文占用真值；window 是引擎配置值非模型真值。
func usageBill(merged *schema.Message, window int) (*UsageBill, bool) {
	if merged == nil || merged.ResponseMeta == nil || merged.ResponseMeta.Usage == nil {
		return nil, false
	}
	u := merged.ResponseMeta.Usage
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil, false
	}
	return &UsageBill{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
		TotalTokens:      u.TotalTokens,
		Window:           window,
	}, true
}

// usageFrame 把账单装成 usage 帧（帧 agent/model 标识产出方；数值列同 bill 逐字段拷贝）。
func usageFrame(bill *UsageBill, agent, model string) Frame {
	return Frame{Kind: "usage", Agent: agent, Model: model,
		PromptTokens:     bill.PromptTokens,
		CompletionTokens: bill.CompletionTokens,
		ReasoningTokens:  bill.ReasoningTokens,
		TotalTokens:      bill.TotalTokens,
		Window:           bill.Window}
}

// drain 排干一条模型输出流：chunk → Translator → delta 帧；EOF 时把全部 chunk 合并成完整消息返回
// （T2：merged.ToolCalls 是 tool_call 帧的来源、merged.Content 是本轮 assistant 行）。
// T3：agent = "" 走 AppendDelta（root——draft 的唯一写入方）；子代理文本走 AppendFrame
// （delta 帧带 agent，**不 APPEND draft**——收口锚「Σ root delta」不被子代理文本污染）。
// T3-1b：drain 的 translator 与 model 值都取自 entry（该 agent 的模型身份）——继承档 = 主 translator/主 ref，
// 写值档 = 按该模型协议选的 translator / 其生效 ref；root 与子代理共用同一条排干逻辑。
// A2：帧计数（裁定 7：AppendDelta/AppendFrame 成功次数总和，不分 root/子代理）——救援谓词的
// 零帧判据；只有 err==nil && live 才 +1（判死/失败没写进 runlive，不算发过帧）。B1 起计数
// 收编 foldBox（D12）——guard 与 worker 读同一个数。
// 返回 live=false = run 已判死（该收摊，不是错误）；err != nil = 错误结局。
// close 责任也在本函数：事件流的另一副本由 agent 内部排干，早退不关会把两边一起吊住。
func (w *Worker) drain(ctx context.Context, runID RunID, epoch int64, agent string, entry agentModelEntry, stream *schema.StreamReader[*schema.Message], box *foldBox) (*schema.Message, bool, error) {
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
				_, live, err = w.store.AppendDelta(ctx, runID, epoch, entry.ref, text)
			} else {
				live, err = w.store.AppendFrame(ctx, runID, epoch, Frame{Kind: "delta", Agent: agent, Model: entry.ref, Text: text})
			}
			if err != nil {
				return nil, false, err
			}
			if !live {
				return nil, false, nil
			}
			box.addFrames(1)
		}
	}
}

// watchdog 在生成期间每 leaseRenew 续约一次闸的租约（A7）。续约 no-op（PG 守卫 UPDATE 影响 0 行：
// 闸已易主 / 已放 / 已到期 / 已被接管）= 本 run 被判死 → 记日志、cancel 本场 run（doc/17 §4
// 手动停止：stop 端点翻 stopped 后由此传播取消；自然过期同样掐流——2026-10-15 用户拍，
// 原「不杀流」语义废）。
// 主循环的 AppendDelta 还会因 draft 蒸发 / 令牌不符自然停写（返 !live）、EndRun 被守卫挡住
// 走「闸已不在，空转」。
// epoch 匹配才 cancel（doc/30 §7）：接管者已注册新一代的 cancel，旧 watchdog 的判死
// 不许误伤它。ctx 由 Run 在退出前 cancel，watchdog 随即返回；wg.Wait() 保证它完全退出后
// EndRun 才跑。
func (w *Worker) watchdog(ctx context.Context, sessionID SessionID, runID RunID, epoch int64) {
	t := time.NewTicker(w.leaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := w.store.RenewLease(ctx, sessionID, runID, epoch)
			if err != nil {
				if ctx.Err() != nil {
					return // Run 已收工、ctx 被 cancel 导致的在途续约失败，安静退出
				}
				log.Printf("lease renew %s/%s: %v", sessionID, runID, err)
				continue
			}
			if !ok {
				// 续约 CAS 0 行（doc/17 §4，2026-10-15 用户拍「採」）：闸已不属于本 run——
				// 被 stop 端点翻了 stopped，或租约自然死亡，或已被接管。都 cancel 本场 run：
				// stop 的契约是真断流；自然过期不掐只会让模型流白挂着烧 token（原「不杀流」语义废）。
				log.Printf("lease renew %s/%s: 闸已不属于本 run，cancel 本场 run（本 run 判死）", sessionID, runID)
				if c, hit := w.runCancels.Load(runID); hit {
					if rc := c.(*runCancel); rc.epoch == epoch {
						rc.cancel()
					}
				}
				return
			}
		}
	}
}

// AdmitText 是 POST 单条消息的准入检查（C5-A1，tail 支）：compact 关闭（window=0）时一切放行；
// 开启时单条 user 消息计数 > limit/compactAdmissionDivisor ⇒ 不放行，由调用方回 413。
// 媒体不在预算里（doc/32 A2 翻案 2026-10-18）：文本管道下请求里只有占位行，base64 字节
// 根本不进模型窗口——对它跑 cl100k 既拒错人（3MB 图实际只发一行占位）又跑不动
// （regexp2 对 MB 级 base64 分钟级）；图的门槛是 ingestImages 的硬帽（≤5MB/张、≤4 张）。
// 半边二（字节真进请求）动工时窗口计数按 provider 口径（deepseek 实测按张平价 ~384 token）
// 另拍，不回这套字节折算。
// 返回 (limit, tokens, ok)：limit = compactLimitFor(window)（与触发折叠同一 limit，不发明第二个数）；
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
// compact 挂钩（C 系列）：只在本函数里做 Run fold 的长度自测——快速路（≤ limit）原样送、行为与
// 压缩前零差异；超限走 foldInput（compact.go）折出候选输入与 stagedCompaction。A4 起两样产出都
// 进 run 级容器（foldBox）：staged 打头进 staged 列表、折算行成为钩子的坐标基点（base）——
// 钩子此后在每次模型调用前对同一个 base 续折（midfold.go）。本函数不写任何存储：折叠失败
// （Summarize 失败 / errStillOver）直接返回错误，Run 置 status=error、零写入。
// injectRecall 把两线召回块追加到本轮 user 消息末尾（doc/29 §6.4）：与预览端点同一个
// Store.Recall（开关也同一份）。失败只记一行日志、run 继续——记忆不是主流程的硬依赖。
func (w *Worker) injectRecall(ctx context.Context, sessionID SessionID, runID RunID, msgs []*schema.Message) {
	cfg := w.store.MemoryConfig()
	if !cfg.Recall && !cfg.RAG {
		return
	}
	text := lastUserText(msgs)
	if text == "" {
		return
	}
	res, err := w.store.Recall(ctx, text)
	if err != nil {
		log.Printf("memory recall %s/%s: %v", sessionID, runID, err)
		return
	}
	if res.Block == "" {
		log.Printf("memory recall %s/%s: 无命中", sessionID, runID)
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			msgs[i].Content += "\n\n" + res.Block
			break
		}
	}
	log.Printf("memory recall %s/%s: terms=%d memoryHits=%d dialogueHits=%d injected=%d runes",
		sessionID, runID, res.Terms, res.MemoryHits, res.DialogueHits, res.InjectedRunes)
}

// lastUserText 取本轮 user 原文（召回查询的词源）。
func lastUserText(msgs []*schema.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			return msgs[i].Content
		}
	}
	return ""
}

func (w *Worker) prepareInput(ctx context.Context, sessionID SessionID, runID RunID) ([]*schema.Message, error) {
	history, err := w.store.ModelMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return w.prepareInputFrom(ctx, sessionID, runID, history)
}

// prepareInputFrom 是 prepareInput 的折叠段（doc/30 §9.1 拆出）：输入行由调用方给——
// 正常路径 = mh 全序，续跑路径 = mh 全序 + 账本重放行。其余（长度自测、Run fold、容器填装）
// 两条路径完全一致。
func (w *Worker) prepareInputFrom(ctx context.Context, sessionID SessionID, runID RunID, history []ModelMessage) ([]*schema.Message, error) {
	var staged *StagedCompaction
	if w.compactWindow > 0 {
		limit := w.compactLimit
		// 放行估算 C 恒算并入容器（A1，planning 裁定 3）：detect 行 / rescue 行 / D4 的 limit'
		// 三处消费同一个数——C 跟「产出全进容器」同一条路（box.setBase 的先例），不恢复返回值传递。
		total := countInput(w.counter, w.system, history)
		if box := foldBoxFrom(ctx); box != nil {
			box.setBaseCount(total)
		}
		if total > limit {
			log.Printf("compact %s/%s: 输入 %d tok > limit %d，进入折叠", sessionID, runID, total, limit)
			var err error
			history, staged, err = foldInput(ctx, w.system, history, runID, limit, w.compactWindow, w.counter, w.sum.summarize, firstPartyFileEffects)
			if err != nil {
				return nil, err
			}
			log.Printf("compact %s/%s: 压 %d 个 run，摘要 %d tok，挂 %s；发送输入 %d tok < limit %d",
				sessionID, runID, len(staged.DropRunIDs), w.counter.Count(staged.Summary),
				staged.AttachRun, countInput(w.counter, w.system, history), limit)
		}
	}
	if box := foldBoxFrom(ctx); box != nil {
		if staged != nil {
			box.appendStaged(*staged)
		}
		box.setBase(history)
	}
	return toSchemaMessages(history), nil
}
