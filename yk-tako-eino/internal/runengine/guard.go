// 本文件：溢出修复层二期——WrapModel 装饰层：B1 观测 → B2 救援链 → B3 交接（槽 + staged，已落地）。
//
// 病灶（19v6-09 §8）：一期救援的零帧谓词救不了「读大文件读到一半被拒」——第一轮
// tool_call/tool_result 帧已发、工具副作用已在场，整条 agent.Run 重跑 = 帧双份 + 副作用两遍；
// worker 级没有别的出路。二期把救援点下移到**模型调用点**：拦得住当次流式调用的错误、
// 折掉被拒输入、重调一次——工具不重执行、帧不重发（被拒调用一帧未出）。
//
// B1 装的眼睛在 B2 接上手：命中「中途拒收」不再只打观测行——四条件同真（IsOverflowError ∧
// windowOn ∧ 已有帧 ∧ 闸新，D14）就地把**当次被拒输入**折一遍重调（折叠链全复用，见下），
// 折不动或重调再拒才把错误放走。闸花（本 run 已救过一次）仍打 B1 观测行原样上抛。
//
// 缝实读（eino v0.9.21，2026-10-12）：WrapModel 在每次模型调用点逐层包裹（wrappers.go:1129
// 逆序包裹、first registered = outermost）——guard 挂在媒体包壳之外，模型调用错误冒泡最先到它；
// 媒体包壳（media_send.go）只换输入副本、错误原样上抛，guard 的折叠/计数/救援看到的仍是占位文本。
// guard 的 inner 不再是裸模型端点，而是 mediaSendModel；被拒调用在事件面上无声，重调成功后事件
// 恰好一次，不会双发。子代理装配（agents.go）不含本 guard：装配面没有 Handlers 字段，
// 天然排除（§10 的边界，零排除代码）。
//
// 救援链（D14）逐段与来源：
//   - 原料 = **当次 input 转录**（convertState(box.baseRows(), box.run, input)，midfold 现役）
//     ——不重读 mh：装饰器看到的就是实发的那份，A2 重读 mh 的理由（prepareInput 本地变量可能
//     折过）在这里不成立；
//   - C_rej = 这份被拒输入的 countInput（现役口径：system 从 input[0] 取，与 convertState 同款）
//     ——比 box.baseCount 的 run 起手值更贴近被拒事实；
//   - limit' = rescueLimit(C_rej, extractWindow(真值), hasWindow)（overflow.go 现役）；
//   - foldInput(ctx, sys, history, box.run, limit', 真窗, ...)（compact.go 现役，「force」的施工形态
//     = 传现算的 limit' + trim 预算用真窗——B4 起；盲降（无真值）给 0，trim 预算取下界 8192）；
//   - 摘要行打戳（AttachRun/seq0，同 midfold/A2）→ **交接槽 + staged 进容器（B3，D15）**：
//     槽给 state 层的采用点（midfold.go 采用点 1/2——B2 的「救一轮不救 state = 死缓一轮」
//     由它解开），staged 由 EndRun 吃列表的现役路径落库（complete-only）；本处仍不动 base
//     （换代在采用点 1，折了没采用时基点不被动）→ 重调 = inner.Stream(ctx, [input[0]] +
//     toSchemaMessages(folded), opts...) 一次。折后行由 mh 形状重建、本不带思考（doc/24 的
//     stripReasoning 管的是没折的直通路径），无需重复滤。
//
// 负路径（D14；格式面只钉两行日志，不发明）：折败（convertState 违例 / errStillOver /
// summarize 败）→ 原始错误对象原样上抛，闸已花不再救（容器零写：交接在折成之后）；重调再拒 →
// 上抛第二次的错误（首次拒已随 rescue 行进 guard 日志）。库仍一个字不动——落库只在 EndRun
// 且 status=complete（B3 的 staged 列表进的是这条现役通道；error / 取消 = 零写入）。
package runengine

import (
	"context"
	"log"
	"strconv"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// overflowGuard 是挂在中介链尾的救援中间件（D13）：只覆写 WrapModel，把内层模型包成
// guardModel。windowOn 取自操作者窗标（compactWindow > 0；窗未声明 = 本层关）。
// 救援依赖（B2）随构造注入：counter 计数、summarize 摘要、fileEffects 钉块判定表——
// 与 midfoldHandler 同一组装配件（同一顶帽、同一张表）。
type overflowGuard struct {
	*adk.BaseChatModelAgentMiddleware
	windowOn    bool
	counter     TokenCounter
	summarize   func(context.Context, string) (string, error)
	fileEffects map[string]string
}

func newOverflowGuard(compactWindow int, counter TokenCounter,
	summarize func(context.Context, string) (string, error), fileEffects map[string]string) *overflowGuard {
	return &overflowGuard{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		windowOn:                     compactWindow > 0,
		counter:                      counter,
		summarize:                    summarize,
		fileEffects:                  fileEffects,
	}
}

func (g *overflowGuard) WrapModel(_ context.Context, m model.BaseChatModel, _ *adk.ModelContext) (model.BaseChatModel, error) {
	return &guardModel{inner: m, windowOn: g.windowOn, counter: g.counter, summarize: g.summarize, fileEffects: g.fileEffects}, nil
}

// guardModel 是装饰后的模型：Generate 直通（主循环走 Stream，直通只为接口完备——
// BaseModel 只有这两个方法）；Stream 拦错误分支。
type guardModel struct {
	inner       model.BaseChatModel
	windowOn    bool
	counter     TokenCounter
	summarize   func(context.Context, string) (string, error)
	fileEffects map[string]string
}

func (m *guardModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.inner.Generate(ctx, input, opts...)
}

// Stream：成功 = 原流原样返回。错误路径——四条件（IsOverflowError ∧ windowOn ∧ 容器在 ∧
// 已有帧）同真才进救援；任一不真照旧上抛：
//   - 零帧静默让位 A2（run-start 拒收走整条重跑链，一期形状原样；闸也不花——A2 谓词要它）；
//   - 无容器直通（旁路调用没有 foldBox）；
//   - 闸已花 = 本 run 已救过一次（判断在 tryMarkRecovered 内）——保留 B1 观测行原样上抛，
//     同一闸换载体后 guard 与 worker 的 A2/A3 三点看的是同一个数。
//
// 闸先花（D14）：条件全真即花掉再进折叠——折败/再拒都不回滚，本 run 不再有第二次救援。
func (m *guardModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err == nil || !m.windowOn || !IsOverflowError(err) {
		return stream, err
	}
	box := foldBoxFrom(ctx)
	if box == nil || box.frameCount() == 0 {
		return stream, err
	}
	if !box.tryMarkRecovered() {
		log.Printf("overflow guard: 中途拒收（观测；本拍不救）")
		return stream, err
	}
	retryInput, ok := m.rescueFold(ctx, box, input, err)
	if !ok {
		return nil, err // 折败：原始错误对象上抛（闸已花）
	}
	retry, retryErr := m.inner.Stream(ctx, retryInput, opts...)
	if retryErr != nil {
		return nil, retryErr // 重调再拒：上抛第二次的错误（首次拒已进 rescue 行）
	}
	return retry, nil
}

// rescueFold 折被拒输入并造重调消息（D14 全复用链）：convertState 切回 mh 形状（system 恒
// 队首断言同款）→ C_rej = 这份输入的估算 → D4 目标线 → foldInput 强制进入 → 摘要行打戳
// （AttachRun/seq0，同 midfold/A2 先例）→ 交接（B3：槽 + staged 进容器）→ rescue 行（格式
// 钉死，与 A2 同族只多 guard 一词）→ [input[0] 原样] + 折后行。ok=false = 救援链自身失败——
// 调用方上抛原始错误、容器零写（交接在折成之后，折败走不到那两行）；注：本处不落库、不动
// box 的 base（换代在采用点 1）与 baseCount。
func (m *guardModel) rescueFold(ctx context.Context, box *foldBox, input []*schema.Message, cause error) ([]*schema.Message, bool) {
	sys, history, err := convertState(box.baseRows(), box.run, input)
	if err != nil {
		return nil, false
	}
	annotateMediaTokens(ctx, history, box, box.mediaResolver())
	cRej := countInput(m.counter, sys, history)
	window, hasWindow := extractWindow(cause)
	folded, staged, err := foldInput(withCompactTrigger(ctx, "rescue"), sys, history, box.run, rescueLimit(cRej, window, hasWindow), window,
		m.counter, m.summarize, m.fileEffects)
	if err != nil {
		return nil, false
	}
	folded[0].RunID = staged.AttachRun
	folded[0].Seq = 0
	// B3（D15）：救援产物进交接槽 + 现役 staged 列表——采用点负责 state 换代、EndRun 负责落库
	// （complete-only）。行数 = 被拒输入条数，采用点据此切尾随行（见 foldBox.setAdopted 注释）。
	// 重调若再拒：run 走 error → EndRun 零写入，这里的容器写自然作废（不影响既有错误出口）。
	box.setAdopted(folded, len(input))
	box.appendStaged(*staged)
	truth := "盲降"
	if hasWindow {
		truth = "真值 " + strconv.Itoa(window)
	}
	log.Printf("overflow guard rescue: provider 拒收（估算 %d tok；%s）；强折 %d 个 run，摘要 %d tok；重试",
		cRej, truth, len(staged.DropRunIDs)+len(staged.MergedSummaryRunIDs), m.counter.Count(staged.Summary))
	retryInput := make([]*schema.Message, 0, len(folded)+1)
	retryInput = append(retryInput, input[0])
	retryInput = append(retryInput, toSchemaMessages(folded)...)
	return retryInput, true
}

// 编译期锚：接口一缺，编译不过（中间件其余方法由 Base 兜底；guardModel 两方法 = BaseModel 全部）。
var _ adk.ChatModelAgentMiddleware = (*overflowGuard)(nil)
var _ model.BaseChatModel = (*guardModel)(nil)
