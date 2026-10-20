// 本文件是上下文压缩（compact）的算法与装配：C 系列的第一步（planning §10，C1）。
//
// 设计来源 doc/12-上下文压缩.md §9（算法伪代码）+ doc/13（T1–T9）——**旧线口径**：钩子
// BeforeModelRewriteState、run_archive、event_log、compact_rag 在 v6 都没有对应物；
// 取其算法与取舍逻辑，落点按 v6 前提重推（planning §10.4）。v6 的落点 D1 已就位
// （run_model_messages.compacted 列 + 部分索引 + seq=0 语义，pgstore.go），本文件补上
// **触发者与写入者**：触发在 worker 组装输入时自测长度（prepareInput），写入在 EndRun(complete)
// 同笔事务（store.go）。
//
// doc/12 → v6 的逐条映射（§10.4，偏离全部登记）：
//   - 单位：旧「轮」= 新 run；「已完成」= 非当前 run 的行（error run 只留 user 半，照压）。
//   - 快速路（日常零变化）：CountMessages(系统 + 全量非 compacted 行) ≤ limit → 原样送模型。
//   - 计数基数（D3，19v6-10 §6）：text + tool_calls JSON 序列化（工具 schema 声明不计）。
//   - 超限进循环（phase 1）：n 从 20 起降（keep = 最近 n 个已完成 run，drop = 其余；drop 空 → n--）；
//     每档 Summarize(旧摘要 + 渲染 drop) → 拼装 [摘要行] + [keep 行] + [当前 run 行] →
//     CountMessages < limit 则用它作本轮输入、stagedCompaction 挂收尾；否则 n--。
//   - phase 2（mid-run 泛化，19v6-10 §7/§8）：phase 1 折尽（n=0 档判过）仍超 → 收缩当前 run 的
//     交换单位后缀（user 行钉死、一次吐一个最老单位），真止条件同「组装计数 < limit」；当前 run
//     无单位（Run fold 形态）时 phase 2 不进场，出口与现状逐字节相同。
//   - n=0：drop = 全部已完成原文 run（旧摘要行天然并入）；仍超限 → errStillOver：
//     **不调主模型、status=error**（页面上这一轮是错误，无助手回复——现役 error 路径原样）。
//   - 落库时机（承 D1 事务边界）：发送前只 Stage 在内存；EndRun 仅 complete 分支把
//     stagedCompaction 并入同一笔 PG 事务（放闸 + 定稿 + compacted 标记 + seq=0 行）；error / 闸失 / 进程死 →
//     零写入，下一轮从原文重压——「原文从未被动过」，连旧线的发送前快照都不必有。
//   - 偏离登记：无 RAG 两档舞（检索块不存在）；无 Notify(正在压缩)（帧面零变化，§10.3 第 8 条）；
//     无 Usage 基数（v6 无 Usage 列，全量 BPE）；窗口查找链不搬（旗标即源，§10.3 第 9 条）；
//     旧线把摘要压进本轮 user_message——v6 不复刻（摘要独立成 seq=0 行）。
//
// 摘要行挂点的不变量（§10.3 第 6 条读法 A 的机械半边，mid-run 泛化后）：`UNIQUE(run_id, seq)`
// 逼着摘要行只能挂在**本事件最后折入原料所在的 run**——旧摘要所在 run 的 seq=0 槽被旧行占着
// （行在库就在，compacted=true 不释放唯一约束）。折过当前 run 单位时挂点 = 当前 run，槽已被
// 本 run 上一次折叠的摘要占着 → 同槽换代（UPSERT，19v6-10 §5）；只折老 run 时挂点槽恒空，
// UPSERT ≡ INSERT（Run fold 逐字节不变）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// compactKeepRuns 是循环起点 n（doc/12 §5「从 20 轮开始留」）。
// 「20 轮只数它前面已经结束的对话」；当前 run 的行不进 n 的 keep/drop 划分——user 行钉死、
// 其余按交换单位走 phase 2 后缀收缩（19v6-10 §7/§8）。
const compactKeepRuns = 20

// maxUnitFoldAttempts 是 phase 2「当前 run 单位后缀收缩」的尝试封顶（19v6-10 §8「封顶约 20 次
// 尝试」）：一次吐一个最老单位进 drop、每档一次 summarize + 组装计数；耗尽 = errStillOver。
// 真止条件是「组装计数 < limit」，本常量只兜底病态输入（单位数远超任何真实轮次）。
const maxUnitFoldAttempts = 20

// compactPrompt 是摘要提示词模板（**只有一份**，C5 起为六槽 markdown 骨架；19v6-07 §5「结构化摘要」）。
// 独立性：不含任何会话内容，调用方用同一个 ChatStream 另起一次流式生成（不新开 agent）。
// 骨架三重动机：卡死教训（体积失控要有槽级配给）、散文漂移（无骨架跨合并栏目互吞）、
// 机械锚点无家（fileOps 挂靠 ## 重要事实）。存量散文摘要的迁移语义 = 首事件整体进
// ## 其他上下文（prompt 第 3 条与 renderFold 的「此前摘要，请合并进新的摘要」对应，改一处要同步）。
// 规则 ② 的总预算为 %d 占位符——attempt() 按硬帽（maxTokens×4/5 字/tok，dev 校准）每次调用渲染：
// 软目标必须与硬帽同源，静态值在硬帽落地后会诱导超帽截断（A3 实测教训）。
const compactPrompt = `你在为一段较早的对话维护一份长期摘要，供后续轮次继续使用。` +
	`按这段内容实际在谈的事做摘要，只保留对后续仍然有用的信息。` +
	"摘要必须用下面六个栏目的 markdown 骨架，栏目标题一字不差、顺序不变：\n" +
	"## 会话意图\n用户当前任务的核心目标（一段话）。\n" +
	"## 关键决策\n已做出的决策及理由——理由必须写，后续轮次要靠它判断沿用还是推翻。\n" +
	"## 重要事实\n具体数字、约束、文件路径、错误信息、暗号类锚点。\n" +
	"## 待办与下一步\n还没做完的事、下一步动作。\n" +
	"## 用户偏好\n语言、风格、格式要求。\n" +
	"## 其他上下文\n装不进上面栏目但对后续有用的信息。\n" +
	"规则：\n" +
	"1. 没有内容的栏目直接省略整个标题，不要编造、不要留空标题。\n" +
	"2. 全文总量控制在 %d 字以内（预算随 compact 窗口缩放，各槽占比约：会话意图一成、关键决策二成半、" +
	"重要事实二成半、待办与下一步一成半、用户偏好半成、其他上下文吃余额），栏目内自行取舍。\n" +
	"3. 输入开头若有一份旧摘要（此前摘要，请合并进新的摘要）：它有槽结构就按栏目并入；" +
	"没有槽结构（纯散文）就整体并进「## 其他上下文」。\n" +
	"4. 输出必须总是上述骨架中有内容的槽——无论输入里有没有旧摘要、旧摘要有无槽结构，" +
	"都不允许输出没有槽标题的正文（A2 e2e 实录：模型把无槽旧摘要原样 echo，骨架从此不再愈合）。\n" +
	"5. 只输出摘要正文（栏目标题 + 内容），不要开场白、不要代码块围栏。\n" +
	"6. 用户明确说过的规矩（「以后都…」「不要再…」这类长期要求）必须按原句保留——" +
	"带引号照抄原话，不要改写、不要只写归纳；后续轮次要靠原句判断约束的边界。"

// errStillOver 是「压到头仍不低于 limit」的哨兵（doc/12 §9 的 ErrStillOver）：
// 调用方（worker）把它当普通错误处理——不调主模型、status=error、零写入。
var errStillOver = errors.New("compact: 摘要后仍不低于 limit（不调主模型）")

// errStall 是流式 stall 哨兵（首 token 限时 / chunk 间隔限时 / 宽松总时限到点）：
// 非重试类失败，直接走失败出口 A（19v6-07 §3「失败出口 = A」）。
var errStall = errors.New("compact: 摘要流 stall 超时")

// 摘要调用的旋钮三件套 + 失败分类（19v6-07 §3；初值全部标注来源、dev 校准，拒抄常数双重标准）。
// 这些是 summarize 一条链路的保险丝组：输入无界时重试同一份必然原样失败（截断与重试是绑定关系）。
const (
	// summarizeTemp 低温档 0.3（借 WeKnora 经验值）——摘要要稳不要花活。
	summarizeTemp = 0.3
	// summarizeMaxAttempts = 1 + 重试 ≤2（19v6-07 §3「重试 ≤2 次」）。
	summarizeMaxAttempts = 3
)

// 摘要预算帽分档的档常量（C5-A7；19v6-07 §5 分档表 = 2026-10-11 用户表落码）。
// maxTokens（= 分档 ceiling）是两层身份：硬预算帽 + 三层验货算术主检的比对基线
// （completion ≥ 它 ⇒ 按截断拒收，误杀代价仅多一次重试）。
const (
	summarizeCeilingSmallCap   = 1024    // 小窗式 min(limit/2, ·) 的反向校准封顶（防 verbose 超产，dev 位）
	summarizeCeilingMid        = 4096    // 中窗固定档（= A3 现行常量，区间下限）
	summarizeCeilingLarge      = 8192    // 大窗固定档（区间下限；不随被压段线性涨）
	summarizeCeilingSmallBound = 32768   // 小窗档界：window ≤ 此值走小窗式
	summarizeCeilingLargeBound = 1048576 // 大窗档界：window ≥ 此值走 8192
	claudeCeilingFactorNum     = 13      // Claude override 系数 13/10 = 1.3（区间中值；整数算、截断）
	claudeCeilingFactorDen     = 10
)

// compactAdmissionDivisor 是 POST 单条消息准入的除数（C5-A1，tail 支卡死的入口治法；
// 19v6-07 §5 结构化摘要条的推导：恒有解要求 S ≤ limit − tail_max）——单条 user 消息
// cl100k 计数 > limit/divisor ⇒ 抢闸前拒收。
const compactAdmissionDivisor = 3

// summarizeBudgetDivisor 是小窗式 min(limit/2, 1024) 的除数（C5-A3 校准值；A7 起由
// summarizeCeiling 的小窗分支使用）：除数 2 的守恒 S + tail_max = limit/2 + limit/3 = 5/6·limit < limit
// （n=0 恒有解，余 limit/6 给 keep 与估算误差）；除数 3 在 window 4000 下帽 = 1066，低于
// A2 实录 S = 1221（模型自然产出），已弃。
const summarizeBudgetDivisor = 2

// CompactLimitFor 是触发线 / 准入线 / 摘要预算的共同底数**唯一算式**（三期重裁，19v6-09 §9：
// 文中作 compactLimitFor）：limit = window − reserve，reserve = min(window/5, 16384)。预留从
// 「×4/5（响应空间 + 估算误差垫）」重裁为固定档——误差垫职能随溢出修复层一期落地移交兜底层，
// 16384 = WeKnora settings.go 固定 16k（回复大小不随窗口缩放）。window = 81920 处两算法相交
// （81920/5 = 16384），之下退化为 ×4/5：dev 小窗测试零变化（600 窗 → limit 480 两式同值），
// 不留新魔法边界。256k 窗：limit 204800 → 239616，吃回 34816 ≈ 35k。window ≤ 0（compact 关）= 0。
// 导出供 main 的启动日志显示生效 limit（worker 装配与 summarizeCeiling 两处内部调用）。
func CompactLimitFor(window int) int {
	if window <= 0 {
		return 0
	}
	return window - min(window/5, 16384)
}

// summarizeCeiling 按窗口分档算摘要预算帽（C5-A7；19v6-07 §5 分档表，纯函数，装配期一次）。
//
//	window ≤ 0（compact 关）        → 4096（A3 前常量语义；小窗式禁算——min(0,1024)=0 会把帽掐死）
//	window ≤ 32768（小窗/dev）      → min(limit/2, 1024)——恒有解守恒体（limit/2）＋反向校准封顶；
//	                                  limit 经 compactLimitFor（三期起；32768 ≤ 81920 恒走 ×4/5 支，逐值不变）
//	32768 < window < 1048576（中窗）→ 4096；window ≥ 1048576（大窗）→ 8192（均区间下限，不插值）
//
// Claude override：ref 厂商段 = anthropic 或 ref 含 claude（大小写不敏感）→ 固定档（中/大）×13/10
// ——只乘固定档，不乘小窗式（小窗式是恒有解算术本体，乘了破守恒），window=0 的兜底也不乘
// （compact 关时 summarizer 永不跑，值不生效）。
// maxOutput > 0（TAKO_MODEL_MAX_OUTPUT）最后硬顶：ceiling = min(ceiling, maxOutput)。
func summarizeCeiling(window int, modelRef string, maxOutput int) int {
	var ceiling int
	switch {
	case window <= 0:
		ceiling = summarizeCeilingMid
	case window <= summarizeCeilingSmallBound:
		ceiling = min(CompactLimitFor(window)/summarizeBudgetDivisor, summarizeCeilingSmallCap)
	case window < summarizeCeilingLargeBound:
		ceiling = summarizeCeilingMid
	default:
		ceiling = summarizeCeilingLarge
	}
	if window > summarizeCeilingSmallBound && claudeRef(modelRef) {
		ceiling = ceiling * claudeCeilingFactorNum / claudeCeilingFactorDen
	}
	if maxOutput > 0 {
		ceiling = min(ceiling, maxOutput)
	}
	return ceiling
}

// claudeRef 认 Claude 系模型 ref：厂商段 = anthropic，或 ref 含 claude 子串（大小写不敏感）——
// 厂商 id 与模型名由用户配置、大小写不可控，先小写化再判。
func claudeRef(ref string) bool {
	lower := strings.ToLower(ref)
	provider, _, _ := strings.Cut(lower, "/")
	return provider == "anthropic" || strings.Contains(lower, "claude")
}

// 分级截断的上限（19v6-07 §3「分级截断」，rune 计 + 截断标记）：
// 工具结果动辄上万 token，不封顶则摘要输入无界；工具参数每 value 封顶保 k=v 行有界。
const (
	foldToolResultMaxRunes = 2000
	foldArgValueMaxRunes   = 400
	foldTruncMarkerFormat  = "…〔截断：原 %d 字符〕"
)

// 失败原因分类（19v6-07 §3 失败出口 A：分类进引擎日志）。瞬时/验货拒收可重试，
// 超长拒收/stall 不重试（原样重试必然原样失败 / 快速失败不挂死 run）。
const (
	summarizeClassTransient = "瞬时"
	summarizeClassRejected  = "验货拒收"
	summarizeClassLength    = "超长拒收"
	summarizeClassStall     = "stall"
	summarizeClassOther     = "其他"
)

// TokenCounter 是长度自测的全部要求：一段文本的 token 数。
// 装配期给 cl100k（NewCl100kCounter），单测注入假计数器（不触网）。
type TokenCounter interface {
	Count(text string) int
}

// cl100kCounter 是 TokenCounter 的正式实现（doc/12 §3：编码固定 cl100k_base，不按模型再配一份）。
type cl100kCounter struct{ enc *tiktoken.Tiktoken }

// NewCl100kCounter 装配期构造（main 在 -compact-window > 0 时调用）。
// 词表首次从 openaipublic 拉取并缓存到磁盘（TIKTOKEN_CACHE_DIR / os.TempDir），之后启动即命中；
// 拉取失败 = 返回错误，由 main fail fast——「压不动」不能在超窗后才发现（§10.3 第 10 条）。
func NewCl100kCounter() (TokenCounter, error) {
	enc, err := tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
	if err != nil {
		return nil, err
	}
	return cl100kCounter{enc: enc}, nil
}

// Count 数一段文本的 BPE token 数。空串 = 0。
// 计数口径（§10.4）：只数消息文本本身，不发明 per-message 开销——doc/12 §3 只说「用 BPE 数消息」。
func (c cl100kCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	return len(c.enc.EncodeOrdinary(text))
}

// StagedCompaction 是一次折叠（发送前 Stage 的产物）的落库指令；EndRun(complete) 吃**列表**
// （一轮 run 可有多次折叠：Run fold 打头、mid-run 各次追加，19v6-10 §5）——
// 所有 entry 的 DropRunIDs 并集整翻 + MergedSummaryRunIDs 并集只翻 seq=0 + 只落最后一份摘要。
// 不变式：AttachRun 是本事件最后折入原料所在的 run（挂点唯一性，见文件头）——只折过老 run 时
// ∈ DropRunIDs；折过当前 run 的单位时 = 当前 run（同槽换代的落点，19v6-10 §5）。
type StagedCompaction struct {
	DropRunIDs          []RunID // 整 run 翻死：drop 区原文 run + 死摘要 run（无存活原文行）
	MergedSummaryRunIDs []RunID // 只翻 seq=0：keep 区里仍有存活原文行的旧摘要 run（19v6-10 §10）
	Summary             string  // 新摘要全文（含 file-ops 块，若有；已过含块收口检查，非空）
	AttachRun           RunID   // 摘要行挂点（= 本事件最后折入原料所在的 run）
}

// countInput 数「将要发送的这一份」：系统消息（若有）+ 每行 text + 每行 tool_calls JSON
// （D3 基数口径，19v6-10 §6）。工具参数是工具环时代的真实体积大头——write_file 一次调用
// 可以把整份文件塞进 arguments，旧口径（只数 text）对这类行全盲。tool_calls 的序列化
// 与 mh 的 tool_calls 列同源（json.Marshal([]schema.ToolCall)）——量级口径，不要求与
// 落库字节逐字节相等。工具 schema 声明不计（登记项，19v6-10 §6）。
// MediaTokens 是行上已经算好的图片 token，直接相加，不再交给计数器。
func countInput(counter TokenCounter, system string, msgs []ModelMessage) int {
	n := 0
	if system != "" {
		n += counter.Count(system)
	}
	for _, m := range msgs {
		n += counter.Count(m.Text)
		n += m.MediaTokens
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				n += counter.Count(string(b))
			}
			// marshal 失败不回退、不计——ToolCall 全字段是字符串，实际到不了这个分支；
			// 真到了也只是低估（与旧口径同形），不炸折叠链。
		}
	}
	return n
}

// runRows 是分组后的一个 run（或当前 run 的一个交换单位）：id + 行。
// 老 run = 整 run 一组；当前 run 的单位分组复用本类型（id = 当前 run）。
type runRows struct {
	id   RunID
	rows []ModelMessage
}

// currentRunUnits 是当前 run 分组的结果：user 行（钉死）+ 交换单位序列。
type currentRunUnitsResult struct {
	userRow *ModelMessage
	units   []runRows
}

// currentRunUnits 把当前 run 的可见行切成「user 行 + 交换单位」（19v6-10 §7 三件套①）：
// assistant(tool_calls) 行 + 它名下的全部 tool 行 = 一次工具往返；无 tool_calls 的 assistant
// 行（终稿）独立成一个单位；user 行不是单位（钉死）；当前 run 的 seq=0 行（本 run 上一次
// mid-run fold 的活摘要）不属于任何单位——由调用方并入 oldSummaryTexts。
// role 回扩（三件套②）的构造性版本：单位只能以 assistant 行开头，tool 行只作为续行被收进
// 发起它的单位；孤儿 tool（无发起行 / 发起行被折走）没有合法落点 = 错误。
func currentRunUnits(rows []ModelMessage) (currentRunUnitsResult, error) {
	var res currentRunUnitsResult
	for _, m := range rows {
		if m.Seq == 0 {
			continue // 本 run 上次折叠的摘要行：归调用方并入 oldSummaryTexts
		}
		if m.Role == "user" && res.userRow == nil {
			row := m
			res.userRow = &row
			continue
		}
		if m.Role == "tool" {
			// tool 行只作为续行被收进上一个单位（发起行必比它早）。
			if len(res.units) == 0 {
				return res, errors.New("compact: 当前 run 出现孤儿 tool 行（无发起单位）")
			}
			res.units[len(res.units)-1].rows = append(res.units[len(res.units)-1].rows, m)
			continue
		}
		res.units = append(res.units, runRows{id: m.RunID, rows: []ModelMessage{m}})
	}
	return res, nil
}

// assembleInput 拼一个折叠候选：新摘要行 + keep 老 run 行 + user 行（钉死，恒在）+ 剩余单位行。
// 顺序即发送序：keep 在全量可见区维持原相对位置，user 恒在保留区首位（19v6-10 §8 state 重写形状）。
func assembleInput(summary string, keep []runRows, userRow *ModelMessage, units []runRows) []ModelMessage {
	assembled := make([]ModelMessage, 0, 1+len(keep)+1)
	assembled = append(assembled, ModelMessage{Role: "user", Text: summary})
	for _, g := range keep {
		assembled = append(assembled, g.rows...)
	}
	assembled = append(assembled, *userRow)
	for _, u := range units {
		assembled = append(assembled, u.rows...)
	}
	return assembled
}

// buildStaged 从本事件折入的原料构造落库指令（翻转三分类，planning 裁定 1/5）：
//   - DropRunIDs（整 run 翻死）= drop 里折入的原文 run ＋ 死摘要 run——旧摘要所在 run 在本事件后
//     无存活原文行：Run fold 时代摘要 run 恒是此形态（旧行为逐字节接住）；mid-run 时代 =
//     该 run 的原文行更早在别的折叠里被折走、本次只继承了它的摘要；
//   - MergedSummaryRunIDs（只翻 seq=0）= 活摘要 run 里**保留区仍有原文行**的（19v6-10 §10）：
//     摘要被合并进新摘要，原文行还在 keep 区活着，整翻会连保留行一起丢；
//   - 当前 run：永不入两个翻转集（mid-run 的摘要换代由 A3 的 UPSERT 接住，19v6-10 §5）。
//
// AttachRun = 本事件最后折入原料所在的 run：折过当前 run 单位 = 当前 run（同槽换代落点）；
// 只折老 run = 最后一个被折老 run（与 Run fold 同）。
func buildStaged(oldSummaryRuns []RunID, rawRuns, drop []runRows, currentRun RunID, summary string) *StagedCompaction {
	rawAlive := make(map[RunID]bool, len(rawRuns))
	for _, g := range rawRuns {
		rawAlive[g.id] = true
	}
	dropSet := make(map[RunID]bool, len(drop))
	for _, g := range drop {
		dropSet[g.id] = true
	}
	var dropIDs, merged []RunID
	seen := map[RunID]bool{}
	for _, id := range oldSummaryRuns {
		if seen[id] || dropSet[id] {
			continue // 已归类 / 原文行已折入（整翻由下面的 drop 循环覆盖，不重复加）
		}
		seen[id] = true
		if rawAlive[id] {
			merged = append(merged, id)
			continue
		}
		dropIDs = append(dropIDs, id) // 死摘要 run：整 run 翻死
	}
	for _, g := range drop {
		if g.id == currentRun || seen[g.id] {
			continue // 当前 run 不进翻转集（§5 由 UPSERT 接住）；已归类的去重
		}
		seen[g.id] = true
		dropIDs = append(dropIDs, g.id)
	}
	return &StagedCompaction{
		DropRunIDs:          dropIDs,
		MergedSummaryRunIDs: merged,
		Summary:             summary,
		AttachRun:           drop[len(drop)-1].id,
	}
}

// foldInput 是超限后的折叠主循环（doc/12 §9 的逐条 v6 映射，见文件头）：先折老 run（phase 1，
// n 从 20 降到 0），仍超再收缩当前 run 的交换单位后缀（phase 2，一次吐一个最老单位）；每档
// 调一次 summarize（旧摘要 + drop 渲染）、拼装候选输入，组装计数 < limit 即定案。
// 纯函数——summarize 由调用方注入（worker 给「同 ChatStream 另起一次流式调用」），
// 单测注入假件即可直测循环边界（C3）。
//
// C5-A5 钉块：定案处把 file-ops 块（旧块继承 ∪ 本次 drop 提取）钉进摘要末尾——
// assembled[0] 与 staged.Summary 同源（本轮模型也看得到路径）。三档检查 = **重跑本循环自己的
// 收口**（含块全量 < limit；2026-10-11 用户拍死口径，弃静态「S+块+tail ≤ 帽」算式：它不含 keep、
// 且帽与摘要自身预算同值使块无预算、二档无成立空间）：全量钉 → 超则两表各砍 50 重渲染再验 →
// 仍超 = 折叠失败（errStillOver，C2 已有链）。无操作 = 无块 = 检查不发生（无 file 工具会话零行为变化）。
//
// B4 trim 兜底（19v6-09 §8「压后仍超的最后手段」）：定案循环折尽仍超（n=0 / 单位折尽或封顶）
// 时先 trim 候选——候选里的工具结果总量超预算（window/5，clamp [8192, 32768]）就压到预算内再
// 验一次；过关即定案（trim 只动本次候选的拷贝，留档原文一个字节不动）。折叠恒优先于瘦身，
// trim 只在下不来台时出场。window 由调用方给（midfold/worker 给声明窗；救援链给 extractWindow
// 真值、盲降给 0）；window ≤ 0 时预算取下界 8192——裁得深是安全方向，裁后仍超走既有 errStillOver。
//
// 返回值：新输入（摘要行 + keep + 当前 run 行）与 stagedCompaction；errStillOver = 不调主模型、零写入。
// 前提（A2 扩写）：超限（countInput(history) > limit）**或本层强制**（溢出救援：被拒输入 +
// D4 现算的 limit'，只见于 rescueOverflow）才该进来；counter/summarize 非 nil。
// fileEffects = 第一方工具 file_effect 声明表（fileops.go），钉块提取的判定入口。
func foldInput(ctx context.Context, system string, history []ModelMessage, currentRun RunID,
	limit, window int, counter TokenCounter, summarize func(context.Context, string) (string, error),
	fileEffects map[string]string) ([]ModelMessage, *StagedCompaction, error) {

	// ---- 分组：history 已按 (run_id, seq) 有序，run_id 是 ULID（字典序 = 时序）。----
	// 旧不变量「seq=0 必是该 run 唯一可见行」在 mid-run（A4 起）已死：被折过的 run 可以同时有
	// 活摘要行（seq=0）与保留的原文行（seq≥2）。分组按此语义收口：
	var rawRuns []runRows      // 老 run：有可见原文行的已完成 run（按 run 时序，整 run 粒度）
	var oldSummaryRuns []RunID // 旧摘要行（可见 seq=0）所在 run —— 重压时并入、翻死去重靠它们
	var oldSummaryTexts []string
	var userRow *ModelMessage // 当前 run 的 user 行（seq=1，钉死：永不进 drop）
	var units []runRows       // 当前 run 的交换单位（assistant(tc)+其 tool 行 / assistant 终稿行）
	for i := 0; i < len(history); {
		id := history[i].RunID
		j := i
		for j < len(history) && history[j].RunID == id {
			j++
		}
		rows := history[i:j]
		i = j
		if id == currentRun {
			for _, m := range rows {
				if m.Seq == 0 {
					// 本 run 上一次 mid-run fold 的活摘要：只并入 oldSummaryTexts（被合并进新摘要）、
					// 永不进任何翻转集——换代由 A3 的 UPSERT 接住它的 seq0 槽（19v6-10 §5）。
					oldSummaryTexts = append(oldSummaryTexts, m.Text)
				}
			}
			u, err := currentRunUnits(rows)
			if err != nil {
				return nil, nil, err
			}
			userRow, units = u.userRow, u.units
			continue
		}
		var raws []ModelMessage
		for _, m := range rows {
			if m.Seq == 0 {
				oldSummaryRuns = append(oldSummaryRuns, id)
				oldSummaryTexts = append(oldSummaryTexts, m.Text)
				continue
			}
			raws = append(raws, m)
		}
		if len(raws) > 0 {
			rawRuns = append(rawRuns, runRows{id: id, rows: raws})
		}
	}
	if userRow == nil {
		// 当前 run 没有 user 行 = 调用方没把本轮这条带上（history 的真源是 mh 表，seq=1 恒在）。
		return nil, nil, errors.New("compact: 当前 run 的 user 行不在输入里（history 分组异常）")
	}
	// 三件套断言（planning 裁定 7）：每个单位恒以非 tool 行开头（role 回扩的构造性版本）——
	// 以 tool 行开头的单位 = 孤儿 tool（发起行不在或切错），违例 = 内部编程错误，返回错误不 panic。
	for _, u := range units {
		if u.rows[0].Role == "tool" {
			return nil, nil, errors.New("compact: 交换单位以 tool 行开头（配对回扩违例）")
		}
	}

	// ---- 一档折叠 = summarize（旧摘要 + 本档 drop 渲染）→ 组装 → 含块收口（C5-A5 三档）。 ----
	type foldTrial struct {
		assembled []ModelMessage    // 本档最终候选（收敛 = 含钉块；未收敛 = 组装原形——B4 trim 兜底的原料）
		staged    *StagedCompaction // 收敛（组装 < limit 且块收口过）时非 nil
		foldText  string
		summary   string // 本档摘要；未收敛 = 原始产出（带数字死因用）；收敛 = 含钉块全文
		total     int    // 本档组装计数
	}
	// converge 是「已 < limit 的候选」的公共收口（B4 起 tryFold 与 trim 兜底共用同一条尾巴）：
	// C5-A5 钉块（收口口径，2026-10-11 用户拍：块 = 旧块继承 ∪ 本次 drop 提取，钉进发送候选的
	// 摘要末尾；三档检查见函数头）→ 定案 t（summary/assembled/staged，组装行与落库摘要同源）。
	// 返回 errStillOver = 块收口失败（第 3 档），调用方按既有折叠失败链出口。
	converge := func(t *foldTrial, drop []runRows, assembled []ModelMessage, summary string) error {
		ops := mergeFileOps(inheritFileOps(oldSummaryTexts), extractFileOps(dropMessages(drop), fileEffects))
		res := resolveOps(ops)
		if block := renderFileOpsBlock(res); block != "" {
			assembled[0].Text = summary + fileOpsBlockSep + block
			if countInput(counter, system, assembled) >= limit { // 第 1 档：全量钉不过收口
				// 第 2 档减灾：两张表各砍 50（公平对待、无优先级），重渲染再验
				cappedRes := resolvedOps{changed: capOps(res.changed), read: capOps(res.read)}
				capped := renderFileOpsBlock(cappedRes)
				assembled[0].Text = summary + fileOpsBlockSep + capped
				if over := countInput(counter, system, assembled); over >= limit {
					// 第 3 档：仍超 = fold 失败走干净 A（errStillOver 哨兵，C2 已有链）
					return fmt.Errorf("%w（fileOps 块：全量 %d tok、砍 50 后 %d tok；发送合计 %d tok ≥ limit %d）",
						errStillOver, counter.Count(block), counter.Count(capped), over, limit)
				}
			}
			summary = assembled[0].Text
		}
		t.summary = summary
		t.assembled = assembled
		t.staged = buildStaged(oldSummaryRuns, rawRuns, drop, currentRun, summary)
		return nil
	}
	tryFold := func(drop, keep, rest []runRows) (foldTrial, error) {
		var t foldTrial
		t.foldText = renderFold(oldSummaryTexts, drop)
		summary, err := summarize(ctx, t.foldText)
		if err != nil {
			return t, err // Summarize 失败：不 Stage、不发（doc/12 §9）
		}
		t.summary = summary
		assembled := assembleInput(summary, keep, userRow, rest)
		// 保留区首行非 tool（裁定 7 第 2 条；结构上恒真，违例 = 切错/分组异常，返回错误不 panic）。
		if len(assembled) > 1 && assembled[1].Role == "tool" {
			return t, errors.New("compact: 保留区首行是 tool（配对回扩违例）")
		}
		t.total = countInput(counter, system, assembled)
		if t.total >= limit {
			t.assembled = assembled // B4：未收敛也留本档候选——trim 兜底（循环出口处）的原料
			return t, nil           // 未收敛：调用方按阶段推进下一档
		}
		if err := converge(&t, drop, assembled, summary); err != nil {
			return t, err
		}
		return t, nil
	}

	// ---- phase 1：老 run 整折，n 从 20 降序（doc/12 §9 loop 原样：drop 空档 n--；n=0 档 =
	// 全部老 run 一折）。当前 run 无单位（Run fold 形态）时出口与现状逐字节同；有单位且
	// 老 run 折尽仍超 → 交 phase 2。 ----
	n := compactKeepRuns
	for {
		dropCount := len(rawRuns) - min(n, len(rawRuns))
		if dropCount == 0 {
			if n == 0 {
				break // 老 run 折尽（rawRuns 空）：进 phase 2
			}
			n--
			continue
		}
		drop := rawRuns[:dropCount]
		t, err := tryFold(drop, rawRuns[dropCount:], units)
		if err != nil {
			return nil, nil, err
		}
		if t.staged != nil {
			return t.assembled, t.staged, nil
		}
		if n == 0 {
			if len(units) == 0 {
				// 全部老 run 折尽仍超（doc/12 §9）。带数字死因：限值是量级、摘要长度看模型，
				// 没有这几个数就没法判断「差多少才能过」。
				// B4（19v6-09 §8）：折叠已到极限，最后一手 = trim 候选（工具结果瘦身）——过关即定案。
				t.assembled, t.total = trimTrial(counter, window, t.assembled, system)
				if t.total < limit {
					if err := converge(&t, drop, t.assembled, t.summary); err != nil {
						return nil, nil, err
					}
					return t.assembled, t.staged, nil
				}
				return nil, nil, fmt.Errorf("%w（折叠 %d 个 run 共 %d tok → 摘要 %d tok；发送合计 %d tok ≥ limit %d）",
					errStillOver, len(drop), counter.Count(t.foldText), counter.Count(t.summary), t.total, limit)
			}
			break // 有单位：进 phase 2
		}
		n--
	}

	// ---- phase 2：当前 run 的交换单位后缀收缩（19v6-10 §8）——一次吐一个最老单位进 drop，
	// 每档 summarize + 组装计数；真止条件 = 组装计数 < limit；尝试封顶 maxUnitFoldAttempts
	// 只兜底病态单位数（后缀预算 limit/3 是终止性论证，裁定 8）。
	// B4 守卫修正：条件带 dropUnits < len(units)——单位数 < 封顶时「折尽」也收口（旧条件
	// len(units) > 0 在恒超 + 少单位下会让 dropUnits 越界一步、units[dropUnits:] panic）。
	dropUnits := 0
	var last foldTrial
	for dropUnits < len(units) && dropUnits < maxUnitFoldAttempts {
		dropUnits++
		drop := append(append([]runRows{}, rawRuns...), units[:dropUnits]...)
		t, err := tryFold(drop, nil, units[dropUnits:])
		if err != nil {
			return nil, nil, err
		}
		if t.staged != nil {
			return t.assembled, t.staged, nil
		}
		last = t
	}
	if len(units) == 0 {
		// 无原料可折（老 run 空、当前 run 只剩 user 行）：原出口逐字节。
		return nil, nil, errStillOver
	}
	// 单位折尽（≤ 封顶）或尝试封顶（> 封顶）仍超：B4 先 trim 候选（最后一次尝试的组装），
	// 过关即定案；仍超 = 诊断带数字（原出口形态，数字 = trim 后的最终计数）。
	last.assembled, last.total = trimTrial(counter, window, last.assembled, system)
	if last.total < limit {
		drop := append(append([]runRows{}, rawRuns...), units[:dropUnits]...)
		if err := converge(&last, drop, last.assembled, last.summary); err != nil {
			return nil, nil, err
		}
		return last.assembled, last.staged, nil
	}
	return nil, nil, fmt.Errorf("%w（当前 run %d 个单位折 %d 个仍超（老 run %d 个）；发送合计 %d tok ≥ limit %d）",
		errStillOver, len(units), dropUnits, len(rawRuns), last.total, limit)
}

// ---- B4 trim 兜底（19v6-09 §8「压后仍超的最后手段」）----

// 规则四件照抄 WeKnora observe.go 的 trimToolResultsToBudget（2026-10-16 实读对照）：预算 =
// window/5 clamp [8192, 32768]（tok）；全部工具结果先换 marker（原字节数记账）；预算内从最新
// 往回还原文本；放不下的给 1/4 头 + 3/4 尾预览夹。
const (
	trimBudgetDivisor = 5
	trimBudgetMin     = 8192
	trimBudgetMax     = 32768
	// marker 与预览夹的两种文本件（〔〕是房内截断标记惯例，同 foldTruncMarkerFormat；
	// 原字节数记账 = len(string) 字节，照 WeKnora original_bytes 口径）：
	trimMarkerFormat   = "〔工具结果已瘦身：原 %d 字节；要细节请缩小范围重跑该工具〕"
	trimPreviewMidMark = "…〔中段省略〕…"
)

// toolTrimBudget 是工具结果总预算（19v6-09 §8）：window/5，clamp [8192, 32768] tok。
// window ≤ 0（盲降救援的未知窗）落 8192 下界：裁得深是安全方向，裁后仍超由调用方原样报错。
func toolTrimBudget(window int) int {
	budget := window / trimBudgetDivisor
	if budget < trimBudgetMin {
		budget = trimBudgetMin
	}
	if budget > trimBudgetMax {
		budget = trimBudgetMax
	}
	return budget
}

// trimTrial 是折叠循环出口的 trim 一步（B4）：候选过 trimCandidate；动手了就打钉死的日志行
// （格式见 19v6-09 §8，含预算与还原/夹条数），返回裁剪后的候选与重算的计数；未动手 = 原候选
// 与原计数逐值不变。callers 用返回计数判「trim 后过关即定案」。
func trimTrial(counter TokenCounter, window int, candidate []ModelMessage, system string) ([]ModelMessage, int) {
	trimmed, restored, previewed, fired := trimCandidate(counter, window, candidate)
	if fired {
		log.Printf("compact trim: 候选仍超线，trim 工具结果（预算 %d tok，还原 %d 条，夹 %d 条）",
			toolTrimBudget(window), restored, previewed)
	}
	return trimmed, countInput(counter, system, trimmed)
}

// trimCandidate 是 trim 的算法本体（B4；规则四件见上）：候选里的工具结果总和超预算才动手——
// 全部工具结果先换 marker，预算内从最新往回还原原文；第一条放不下的给 1/4 头 + 3/4 尾预览夹，
// 更老的只剩 marker。copy-on-write：返回的是候选的拷贝，入参一字节不动——留档原文（frames /
// mh 原文行 / 容器 base）永不被 trim 改写，「库=模型所见」的差额只体现为候选与折后 state。
// 返回（裁剪后候选，还原 r 条，夹 p 条，是否动手）：未动手（无工具行 / 未超预算）= 原候选原样。
func trimCandidate(counter TokenCounter, window int, candidate []ModelMessage) ([]ModelMessage, int, int, bool) {
	var idx []int
	total := 0
	for i := range candidate {
		if candidate[i].Role == "tool" {
			idx = append(idx, i)
			total += countInput(counter, "", candidate[i:i+1])
		}
	}
	if len(idx) == 0 || total <= toolTrimBudget(window) {
		return candidate, 0, 0, false
	}
	out := append([]ModelMessage(nil), candidate...)
	baseCost := make(map[int]int, len(idx))
	remaining := toolTrimBudget(window)
	for _, i := range idx {
		out[i].Text = fmt.Sprintf(trimMarkerFormat, len(candidate[i].Text))
		baseCost[i] = countInput(counter, "", out[i:i+1])
		remaining -= baseCost[i]
	}
	if remaining < 0 {
		remaining = 0
	}
	// 预算内从最新往回还原：整条塞得下 = 原文全还原；第一条塞不下的 = 头尾预览夹；更老的 marker。
	restored, previewed := 0, 0
	for k := len(idx) - 1; k >= 0; k-- {
		i := idx[k]
		extra := countInput(counter, "", candidate[i:i+1]) - baseCost[i]
		if extra <= 0 {
			out[i].Text = candidate[i].Text // 空/极短结果 marker 不省反涨：原样留，不动预算账
			continue
		}
		if extra <= remaining {
			out[i].Text = candidate[i].Text
			remaining -= extra
			restored++
			continue
		}
		if remaining > 0 && trimPreview(counter, &out[i], candidate[i].Text, baseCost[i]+remaining) {
			previewed++
		}
		remaining = 0
	}
	return out, restored, previewed, true
}

// trimPreview 给放不下的工具结果装预览夹（1/4 头 + 3/4 尾；WeKnora compactToolMessage 的二分
// 同款）：在 maxCost 内找最大的保留量（runes 二分），装得下才改行；一条都塞不下 = 留 marker、
// 返回 false（预算账由调用方清零）。
func trimPreview(counter TokenCounter, row *ModelMessage, full string, maxCost int) bool {
	marker := fmt.Sprintf(trimMarkerFormat, len(full))
	runes := []rune(full)
	best := ""
	low, high := 1, len(runes)
	for low <= high {
		keep := low + (high-low)/2
		head := keep / 4
		tail := keep - head
		probe := *row
		probe.Text = marker + "\n\n" + string(runes[:head]) + "\n" + trimPreviewMidMark + "\n" + string(runes[len(runes)-tail:])
		if countInput(counter, "", []ModelMessage{probe}) <= maxCost {
			best = probe.Text
			low = keep + 1
		} else {
			high = keep - 1
		}
	}
	if best == "" {
		return false
	}
	row.Text = best
	return true
}

// renderFold 渲染一次摘要调用的输入：旧摘要（重压时，天然并入）在前，被压段原文按 run 时序在后。
// 旧线还有一份检索块（RAG）——v6 无 RAG（§10.3 第 7 条），登记为偏离。
//
// C2 标注转录（19v6-07 §3「工具环渲染缺口」）：T2 之前的 renderFold 只拍平 role: text，
// assistant 的 tool_calls 与 tool 行的归属整体失明——变更多次调用归属错乱、变更类工具
// 「这次写入」等于没发生过。现按 WeKnora serialize.go 全形态转录：
//
//	[User]: <text>                                user 行
//	[Assistant]: <text>                           纯文本 assistant 行（带 tool_calls 的行先吐正文再吐调用）
//	[Assistant tool calls]: name(k=v,…)           每次调用一行；参数 key 排序（保跨次渲染一致）、每 value 封顶
//	[Tool result <name>]: <text>                  tool 行；<name> 经前序 assistant 行的 ToolCalls 按 id 解析
//
// 分级截断：工具结果 2000 runes、工具参数每 value 400 runes 封顶（截断标记带原长度）。
// mh 的 tool 行无 ToolName 列（T2 形状）——不为此加列，靠 OpenAI 邻接要求保证按 id 解析得到。
func renderFold(oldTexts []string, drop []runRows) string {
	// 第一遍：tool_call_id → 工具名。tool 行的发起 assistant 行必在同 run 内（run 粒度折叠，
	// OpenAI 邻接要求 assistant 调用行紧跟其 tool 行），先全量建表再渲染，顺序天然满足「前序」。
	names := make(map[string]string)
	for _, g := range drop {
		for _, m := range g.rows {
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
			}
		}
	}
	var b strings.Builder
	if len(oldTexts) > 0 {
		b.WriteString("（此前摘要，请合并进新的摘要）\n")
		for _, t := range oldTexts {
			b.WriteString(t)
			b.WriteString("\n")
		}
	}
	for i, g := range drop {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, m := range g.rows {
			renderFoldRow(&b, m, names)
		}
	}
	return b.String()
}

// renderFoldRow 转录一行（renderFold 的四形态，精确字节被 compact_test 钉住）。
func renderFoldRow(b *strings.Builder, m ModelMessage, names map[string]string) {
	switch m.Role {
	case "assistant":
		if m.Text != "" {
			b.WriteString("[Assistant]: ")
			b.WriteString(m.Text)
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			b.WriteString("[Assistant tool calls]: ")
			b.WriteString(tc.Function.Name)
			b.WriteString("(")
			b.WriteString(renderToolArgs(tc.Function.Arguments))
			b.WriteString(")\n")
		}
		if m.Text == "" && len(m.ToolCalls) == 0 {
			b.WriteString("[Assistant]: \n")
		}
	case "tool":
		name := names[m.ToolCallID]
		if name == "" {
			name = "?"
		}
		b.WriteString("[Tool result ")
		b.WriteString(name)
		b.WriteString("]: ")
		b.WriteString(truncateRunes(m.Text, foldToolResultMaxRunes))
		b.WriteString("\n")
	default: // user（system 不进 mh 表；未知角色按 User 形态兜底不丢内容）
		b.WriteString("[User]: ")
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
}

// renderToolArgs 把模型原始 arguments JSON 渲染成 k=v,…：key 排序（保跨次渲染一致）、
// 字符串 value 原样、非字符串压成紧凑 JSON、每 value 400 runes 封顶。
// 坏 JSON（模型给了 garbage）：原样截断兜底——不丢「调用发生过」这个事实。
func renderToolArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return truncateRunes(args, foldArgValueMaxRunes)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		sv, ok := v.(string)
		if !ok {
			b, err := json.Marshal(v)
			if err != nil {
				sv = fmt.Sprintf("%v", v)
			} else {
				sv = string(b)
			}
		}
		parts = append(parts, k+"="+truncateRunes(sv, foldArgValueMaxRunes))
	}
	return strings.Join(parts, ", ")
}

// truncateRunes 按 rune 截断（不是 byte——中文一字 3 bytes，按 byte 截会把摘要输入切出半个字）。
// 超封顶：前 max runes + 截断标记（带原长度，摘要器知道丢了多少）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf(foldTruncMarkerFormat, len(r))
}

// dropMessages 拍平被压 run 组 / 单位的行（renderFold 之外的两种摊平口径之一：
// extractFileOps 的输入单位，C5-A5）。
func dropMessages(drop []runRows) []ModelMessage {
	var out []ModelMessage
	for _, g := range drop {
		out = append(out, g.rows...)
	}
	return out
}

// summarizer 是摘要调用的一条龙（19v6-07 §3「调用/旋钮/验货/失败出口」+ §5「结构化摘要」预算）：
// 同一 ChatStream 另起一次流式生成（doc/12 §4：一次 Generate 级的动作，不另开 agent），排干取全文，
// 三层验货过了才算数。worker 只持有它、prepareInput 把 summarize 方法注给 foldInput。
// 字段全是可拨的旋钮（初值见 newSummarizer 的来源标注），单测缩小时钟直接改字段。
type summarizer struct {
	chat            ChatStream
	temp            float32       // 低温档（0.3，借 WeKnora 经验值）
	maxTokens       int           // = summarizeCeiling(window, ref, maxOutput)（A7 分档）——算术主检基线 + 硬预算帽
	stallFirstToken time.Duration // 首 token 限时（初值 30s 待校准）
	stallChunkGap   time.Duration // chunk 间隔限时（初值 30s 待校准）
	stallTotal      time.Duration // 宽松总上限：本质仍是墙钟，仅防无限拖长
	maxAttempts     int           // 1 + 重试 ≤2
}

// newSummarizer 给装配期的默认值（19v6-07 §3 旋钮三件套 + §5 分档预算；dev 校准后回改这里）。
// ceiling = summarizeCeiling(window, modelRef, maxOutput)（C5-A7，装配期一次算好）——
// 小窗下让 n=0 时 S+tail ≤ 5/6·limit < limit 恒有解（§5 推导 + A3 校准的守恒体）；
// 中/大窗固定档与两个 override 的取值理由见 summarizeCeiling。
func newSummarizer(chat ChatStream, ceiling int) *summarizer {
	return &summarizer{
		chat:            chat,
		temp:            summarizeTemp,
		maxTokens:       ceiling,
		stallFirstToken: 30 * time.Second,
		stallChunkGap:   30 * time.Second,
		stallTotal:      10 * time.Minute,
		maxAttempts:     summarizeMaxAttempts,
	}
}

// summarize 对一份渲染输入产一份过验货的摘要。失败出口 = A 的 family（19v6-07 §3）：
// 重试按失败原因分流——瞬时错误 / 验货拒收可重试（同一份输入），超长错误 / stall 不重试；
// 重试耗尽或非重试类失败，返回带分类的错误，调用方走 C1 现状路径（status=error、零写入）。
// 每次尝试（成败都算）打 agent_context_compaction 日志行——摘要流量与主推理分离的观测标记。
func (s *summarizer) summarize(ctx context.Context, input string) (string, error) {
	var lastClass string
	var lastErr error
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		out, class, err := s.attempt(ctx, input, attempt)
		if err == nil {
			return out, nil
		}
		lastClass, lastErr = class, err
		log.Printf("compact summarize: agent_context_compaction attempt=%d/%d 失败[%s] budget=%d: %v",
			attempt, s.maxAttempts, class, s.maxTokens, err)
		// 非重试类（超长/stall/其他）与 run 已收尾（ctx 取消）：快速出口，不重试。
		if class != summarizeClassTransient && class != summarizeClassRejected {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	switch lastClass {
	case summarizeClassTransient, summarizeClassRejected:
		return "", fmt.Errorf("compact: 摘要失败（%s，重试 %d 次耗尽）: %w", lastClass, s.maxAttempts-1, lastErr)
	default:
		return "", fmt.Errorf("compact: 摘要失败（%s）: %w", lastClass, lastErr)
	}
}

// attempt 单次摘要调用：建流 → stall 排干 → ConcatMessages 归并 → 三层验货。
// 返回 (摘要, 失败分类, 错误)：成功时分类为空串。
func (s *summarizer) attempt(ctx context.Context, input string, attempt int) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.stallTotal)
	defer cancel()
	// 字预算随硬帽动态渲染（A3）：静态值（A2 的 1200 字）在硬帽落地后会诱导超帽截断 →
	// 验货拒收 → 重试耗尽 → 每次折叠都走 A——prompt 软目标必须与硬帽同源。
	// 换算系数 4/5 字/tok：来源 C2 实测中文摘要 ≈1.18 tok/字，保守取 0.8（dev 校准）。
	budgetRunes := s.maxTokens * 4 / 5
	stream, err := chatFrom(ctx, s.chat).Stream(ctx, []*schema.Message{
		schema.SystemMessage(fmt.Sprintf(compactPrompt, budgetRunes)),
		schema.UserMessage(input),
	}, model.WithTemperature(s.temp), model.WithMaxTokens(s.maxTokens))
	if err != nil {
		return "", classifyStreamErr(err), fmt.Errorf("建流: %w", err)
	}
	defer stream.Close()
	chunks, err := drainStalled(ctx, stream, s.stallFirstToken, s.stallChunkGap)
	if err != nil {
		// ctx 总时限到点 = stall 家族（本质墙钟）；其余按错误内容分类。
		if errors.Is(err, errStall) || errors.Is(err, context.DeadlineExceeded) {
			return "", summarizeClassStall, err
		}
		return "", classifyStreamErr(err), fmt.Errorf("排干: %w", err)
	}
	// ConcatMessages 归并：正文拼接、ResponseMeta 取末帧（Usage/FinishReason 在流末帧，
	// openai 组件恒开 IncludeUsage，claude 组件 messageDelta 同面带——plumbing 实读结论）。
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		return "", summarizeClassOther, fmt.Errorf("归并: %w", err)
	}
	completion, finish := -1, ""
	if merged.ResponseMeta != nil {
		finish = merged.ResponseMeta.FinishReason
		if merged.ResponseMeta.Usage != nil {
			completion = merged.ResponseMeta.Usage.CompletionTokens
		}
	}
	if reason := rejectReason(completion, finish, s.maxTokens); reason != "" {
		return "", summarizeClassRejected, errors.New(reason)
	}
	out := strings.TrimSpace(merged.Content)
	if out == "" {
		// 行表形态下空摘要会变成一条空洞的 seq=0 行——拒收（C1 现状语义保留）。
		return "", summarizeClassRejected, errors.New("摘要为空，拒收")
	}
	log.Printf("compact summarize: agent_context_compaction attempt=%d/%d 通过 finish=%q completion=%d budget=%d chars=%d",
		attempt, s.maxAttempts, finish, completion, s.maxTokens, len(out))
	return out, "", nil
}

// drainStalled 排干一条摘要流：首 token 限时、之后 chunk 间隔限时（19v6-07 §3「流式 stall 检测为主」）。
// Recv 在 goroutine 里跑、经 channel 交付——stall 是「等不到下一帧」，不是靠 Recv 自己报。
// 返回 errStall = 计时器先到（快速失败，不挂死 run）；其余原样透传 Recv 错误。
func drainStalled(ctx context.Context, stream *schema.StreamReader[*schema.Message],
	firstToken, chunkGap time.Duration) ([]*schema.Message, error) {
	type recvResult struct {
		chunk *schema.Message
		err   error
	}
	ch := make(chan recvResult, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			chunk, err := stream.Recv()
			select {
			case ch <- recvResult{chunk: chunk, err: err}:
			case <-done:
				return // 调用方已超时退出：带走 goroutine，不泄漏
			}
			if err != nil {
				return
			}
		}
	}()
	var chunks []*schema.Message
	timer := time.NewTimer(firstToken)
	defer timer.Stop()
	for {
		select {
		case r := <-ch:
			if r.err == io.EOF {
				return chunks, nil
			}
			if r.err != nil {
				return nil, r.err
			}
			chunks = append(chunks, r.chunk)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(chunkGap)
		case <-timer.C:
			if len(chunks) == 0 {
				return nil, fmt.Errorf("%w（首 token 逾 %s 未至）", errStall, firstToken)
			}
			return nil, fmt.Errorf("%w（chunk 间隔逾 %s）", errStall, chunkGap)
		}
	}
}

// rejectReason 三层验货（19v6-07 §3）：返回非空 = 拒收原因。
//
//	① 算术主检：completion ≥ 自设上限 ⇒ 截断拒收（厂商/分词器无关，误杀代价仅多一次重试）；
//	② 字符串辅检：finish_reason（小写后）∈ 截断黑名单拒收——白名单有缺失/变体盲区，只作辅检；
//	③ 缺失处理：finish_reason 缺失时不误杀——算术检过（或无从检起）即放行。
func rejectReason(completion int, finish string, maxTokens int) string {
	if completion >= maxTokens {
		return fmt.Sprintf("算术主检拒收：completion_tokens=%d ≥ 自设上限 %d，按截断处理", completion, maxTokens)
	}
	switch strings.ToLower(finish) {
	case "length", "max_tokens", "max_output_tokens":
		return fmt.Sprintf("字符串辅检拒收：finish_reason=%q ∈ 截断黑名单", finish)
	}
	return ""
}

// classifyStreamErr 按错误内容分失败类（19v6-07 §3 重试分流的判定面）：
// 超长错误（provider 报「太长」）不重试——同一份输入原样重试必然原样失败；
// 瞬时错误（429/5xx/断连/超时）原样重试。串匹配是 dev 初值（校准点：接溢出修复层时换正则集）。
func classifyStreamErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return summarizeClassStall // 宽松总时限到点，归 stall 家族
	}
	if errors.Is(err, context.Canceled) {
		return summarizeClassOther // run 收尾导致的取消：不是模型失败，重试无意义
	}
	msg := strings.ToLower(err.Error())
	for _, p := range []string{"too long", "context length", "maximum context", "reduce the length",
		"request entity too large", "prompt is too long", "exceeds the context", "max_tokens"} {
		if strings.Contains(msg, p) {
			return summarizeClassLength
		}
	}
	for _, p := range []string{"429", "too many requests", "rate limit", "status 5", " 500", " 502",
		" 503", " 504", "internal server error", "bad gateway", "service unavailable",
		"gateway timeout", "connection reset", "broken pipe", "unexpected eof", "timeout",
		"temporarily unavailable", "try again later"} {
		if strings.Contains(msg, p) {
			return summarizeClassTransient
		}
	}
	return summarizeClassOther
}
