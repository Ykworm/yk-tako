// 本文件：记忆快照缝（每场 run 起手读一次、场��冻结）。
//
// 它承载的是**读侧**的机制，不是某一种记忆的存储：
//
//	run 起手 → BeforeAgent 渲染快照段 → 冻进 noteBox
//	→ 每轮模型调用前 BeforeModelRewriteState 把 state.Messages[0] 重写为
//	  Instruction + "\n\n" + 快照段 → 模型在系统提示里读到上一场留下的当前事实
//
// 两种模式的快照内容来自不同地方（doc/65 §3.2 / §4.1 / §5.1）：
//
//   - markdown 模式：两份纯文本文件（USER.md / MEMORY.md）→ memory_markdown.go
//   - pipeline 模式：memory_items 的 resident 层（每场全量）→ memory_store.go
//
// **旧的 notes 表那条线已退役**（doc/65 §6.1）：notes 表的数据在存量迁移里并进
// memory_items 的 resident 层（memory_migrate.go），旧的写入工具与 notes 端点随之退役。
// 旧线的容量上限、写入校验、条目清单渲染等代码也一并退役——它们描述的是一张不再存在的表。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// noteFailAdviseAfter 是劝停句的触发线（doc/17 §7 四件之 2）：同一场 run 里写入失败记到 3 次，
// 第 4 次的结果写明「停止再调、先回复用户」——否则上限满/定位失败时模型一直重试，用户等不到回复。
const noteFailAdviseAfter = 3

// ── 不可见字符检查（doc/17 §7 四件之 3；2026-10-15 十七修：句式词表裁除，只留本档）───────
//
// 病灶：记忆会进以后每一场的系统提示——一行夹带零宽 / bidi 控制字符的话，人审稿面看不见，
// 却随记忆永久排在系统提示里、每场模型原样读到（人审以为记的是句人话）。所以写入时拒、
// 存量毒行渲染占位。
//
// 为什么只剩这一档（19v6-90 十七次修订，用户 2026-10-15 裁定）：原句式神词表
// （Hermes threat_patterns.py strict 档 11 条）经真码探针逐条实测后裁除——纯中文写法
// （「禁用 system prompt」）一条不命中，锚词换中文、写全角都能绕，命中面只剩「句子里恰好
// 用了英文锚词」的窄缝，类别的清单列不尽；隐形字符清单有限、能枚举尽、与语言无关——拦的是
// 「能不能藏」不是「说了什么」；正经用途少（组合 emoji 的 ZWJ、波斯语 ZWNJ），误伤面小、
// 拒写时当场报码点（调用方可见、可改）。
// 隐形字符清单在 memory_text_guard.go 的 memoryInvisibleRunes（两条记忆线共用同一份）。

// scanNoteInvisible 扫一段文字里的隐形字符，返回命中说明
// （"invisible_unicode_U+XXXX"——拒写文案与占位文案照报，调用方/用户当场看见是哪个码点；
// 空串 = 无命中）。
//
// 实现放在 memory_text_guard.go：pipeline 的写入口与 markdown 的文件条目共用同一份
// 隐形字符扫描与 PII 校验——两份实现漂移就会让两条线的拒写口径不一样。
func scanNoteInvisible(text string) string {
	return scanMemoryInvisible(text)
}

// noteBlockPlaceholder 是快照里毒行的占位行（doc/17 §7 四件之 3 的字面：不展示、写明用 remove 删）。
// 快照打占位、工具结果清单留原文（Hermes 同款两端策略，实读确认：清单里删掉原文 = 静默藏起攻击，
// 谁也删不掉它；原文只在模型主动调 memory 的那一轮经工具结果进对话，不进跨 run 的系统提示）。
func noteBlockPlaceholder(id string) string {
	return fmt.Sprintf("[BLOCKED: 此条记忆含人眼不可见字符（%s），不进入系统提示；请用 memory 的 remove 删掉它。]", id)
}

// memoryStopAdvice 是第 4 次写入失败起追加的劝停句（M3 钉死的固定句，doc/17 §7 四件之 2 的字面：
// 停止再调 memory，先回复用户）。带次数，模型能看出这是累积而非偶发。
func memoryStopAdvice(n int) string {
	return fmt.Sprintf("警告：本场 run 内已第 %d 次写入失败——停止再调 memory，先回复用户。", n)
}

// renderRunNotes 是一场 run 起手真正塞进系统提示的记忆段（快照的唯一入口：
// 主代理的 noteSnapshotHandler 与子代理的装载处都调它）。
//
// **模式分流**（doc/65 §3.2）：
//   - markdown 模式：记忆在两份纯文本文件里（USER.md / MEMORY.md）→ memory_markdown.go；
//   - pipeline 模式：记忆在 memory_items 的 resident 层（每场全量进系统提示）→ memory_store.go。
//
// 两条路径共用同一层「每场读一次、场内冻结」的缝——冻结语义（doc/65 §3.2）与存储无关。
func renderRunNotes(ctx context.Context, store *Store, workspace string) (string, error) {
	if store == nil {
		return "", nil
	}
	if store.markdownMode() {
		return renderMarkdownMemorySnapshot(ctx, store)
	}
	return renderResidentMemorySnapshot(ctx, store)
}

// renderResidentMemorySnapshot 渲染 pipeline 模式的常驻记忆段（doc/65 §4.1 的 resident 读契约）：
// 每场全量、当场冻结。
//
// 读口走统一 Store 方法（ListVisibleMemoryItems → resident 视图 + 统一可见性谓词 +
// 只认 active 且未过期），**不自己写 workspace 条件**——可见性只有一处定义（memory_store.go）。
//
// 渲染格式：**常驻记忆：** 标题 + 每行一条（scope 标注 + 正文）。这一段的字节是**新的**
// （旧的 notes 快照是「团队记忆：/ 个人记忆：」两段，notes 表退役后不再存在），
// 按 doc/65 §4.1 的读契约定稿。
func renderResidentMemorySnapshot(ctx context.Context, store *Store) (string, error) {
	items, err := store.ListVisibleMemoryItems(ctx, MemoryLayerResident, 0)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "常驻记忆：（空）", nil
	}
	var b strings.Builder
	b.WriteString("常驻记忆（每场 run 都要遵守的规矩；本场中途的写入要到下一场才进入这里）：")
	for _, it := range items {
		b.WriteString("\n")
		if it.Scope == MemoryScopeWorkspace {
			b.WriteString("[项目] ")
		} else {
			b.WriteString("[个人] ")
		}
		if scanNoteInvisible(it.Content) != "" {
			// 毒行不进系统提示：打占位，写明怎么删（doc/17 §7 四件之 3 的两端策略）。
			b.WriteString(noteBlockPlaceholder(it.ID))
			continue
		}
		b.WriteString(it.Content)
	}
	return b.String(), nil
}

// noteBox 是 run 级的记忆容器（ctx 携带，比照 foldBox / agentModelTable 先例）：
// worker.Run 建（带 run id 与 user 行 runlog seq），快照 handler 的 BeforeAgent 填渲染快照、
// BMR 钩子读快照重写 system，累计写入失败次数（M3）。
type noteBox struct {
	mu      sync.RWMutex
	session SessionID
	run     RunID
	userSeq int64  // 本 run user 行的 runlog seq（source_seq）
	snap    string // 渲染后的快照段：run 起手填一次，run 内冻结
	fails   int    // 本 run 写入失败次数（只由 tickFailure 增；随 run 死）
	spills  int    // 本 run 截断落盘计数器（只由 tickSpill 增；文件名顺编用，随 run 死）
	bashes  int    // 本 run bash 落盘计数器（只由 tickBash 增；stdout/stderr 各占一个顺号，随 run 死）
}

func newNoteBox(session SessionID, run RunID, userSeq int64) *noteBox {
	return &noteBox{session: session, run: run, userSeq: userSeq}
}

// tickFailure 记一次写入失败并返回劝停句（空串 = 还没到第 4 次）。只失败调用 tick：
// 成功的调用不加也不清（计数语义 = 本 run 累计失败数），第 4 次起每次失败都带劝停句。
func (b *noteBox) tickFailure() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails <= noteFailAdviseAfter {
		return ""
	}
	return memoryStopAdvice(b.fails)
}

// tickSpill 记一次截断落盘、返回本 run 内的顺编号（从 1 起）。主代理与子代理共用同一份
// noteBox（ctx 流到整棵执行树）= 同序列顺编、天然不撞（doc/21 §3.2「截断落盘」）。
func (b *noteBox) tickSpill() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spills++
	return b.spills
}

// tickBash 记一次 bash 调用、返回本 run 内的顺编号（从 1 起）。一次 bash 调用占一个号：
// stdout 与 stderr 各写一个文件（`.out` / `.err`），同号不同名，不会互相盖。
func (b *noteBox) tickBash() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bashes++
	return b.bashes
}

func (b *noteBox) setSnapshot(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snap = s
}

func (b *noteBox) snapshot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snap
}

// noteBoxKey 是 ctx 携带记忆容器的私有 key 类型（防撞车惯例同 foldBoxKey）。
type noteBoxKey struct{}

func withNoteBox(ctx context.Context, b *noteBox) context.Context {
	return context.WithValue(ctx, noteBoxKey{}, b)
}

// noteBoxFrom 取回容器；缺失返回 nil（旁路调用没有容器，调用方各自显式处理）。
func noteBoxFrom(ctx context.Context) *noteBox {
	b, _ := ctx.Value(noteBoxKey{}).(*noteBox)
	return b
}

// 编译期锚：接口一缺，编译不过（其余方法由 Base 兜底）。
var _ adk.ChatModelAgentMiddleware = (*noteSnapshotHandler)(nil)

// noteSnapshotHandler 是记忆快照缝（主代理侧，planning §5）：BeforeAgent 在 run 起手读当前记忆、
// 渲染快照段冻进 noteBox（+ dev 日志行——观测面，转正点登记在 planning §7）；
// BeforeModelRewriteState 每轮把 state.Messages[0] 重写为 Instruction + "\n\n" + 快照段（幂等）。
// 挂载序 = 装载 handler 后、midfold 前：midfold 的 convertState 从 state 取 system 计数/折叠，
// 数到的就是模型实见的（含快照）。子代理不走本 handler（其快照缝在 agents.go 装载处）。
type noteSnapshotHandler struct {
	*adk.BaseChatModelAgentMiddleware
	store     *Store
	workspace string
	system    string // Instruction 原文：重写 system 时拼它 + "\n\n" + 快照段
}

func newNoteSnapshotHandler(store *Store, workspace, system string) *noteSnapshotHandler {
	return &noteSnapshotHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		store: store, workspace: workspace, system: system}
}

// BeforeAgent：run 起手渲染一次快照（每 Run 调一次，先于任何模型调用）。读库失败 = run 走 error：
// 快照读不到，记忆层不可信，静默空快照会让模型按无规矩行事。无 noteBox = 旁路（直测 handler）。
func (h *noteSnapshotHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	box := noteBoxFrom(ctx)
	if box == nil {
		return ctx, runCtx, nil
	}
	snap, err := renderRunNotes(ctx, h.store, h.workspace)
	if err != nil {
		return ctx, runCtx, err
	}
	box.setSnapshot(snap)
	log.Printf("memory snapshot %s/%s:\n%s", box.session, box.run, snap)
	return ctx, runCtx, nil
}

// BeforeModelRewriteState：每次模型调用前重写 system（Messages[0]）为 Instruction + 快照段。
// 幂等（重写 = 赋值，不是追加）；midfold 折叠后 system 原样留队首（midfold.go），
// 本 handler 在链上先跑、重写后再交 midfold 计数。断言违例（首条非 system）返回错误、不 panic。
func (h *noteSnapshotHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	box := noteBoxFrom(ctx)
	if box == nil {
		return ctx, state, nil
	}
	snap := box.snapshot()
	if snap == "" || len(state.Messages) == 0 {
		return ctx, state, nil // 未渲染（旁路直测）或缺消息：保持原样
	}
	if state.Messages[0].Role != schema.System {
		return ctx, state, errors.New("memory snapshot: state 首条不是 system（ADK 输入形状违例）")
	}
	// 这场 run 绑了助手时，系统提示是那一行的 spec，不是进程启动时冻进 handler 的那份。
	sys := h.system
	if s, ok := instructionFrom(ctx); ok {
		sys = s
	}
	// 租户规范（teamInstructions）：租户系统提示材料，与记忆轴无关，但确实进系统提示
	// （doc/65 §6.1）。放在 Instruction 之后、快照段之前——它是「规矩」，快照是「当前事实」。
	if team := teamInstructionsSection(h.store.teamInstructionsFor(ctx)); team != "" {
		sys = sys + "\n\n" + team
	}
	state.Messages[0].Content = sys + "\n\n" + snap
	return ctx, state, nil
}
