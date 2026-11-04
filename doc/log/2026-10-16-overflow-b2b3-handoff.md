# Handoff · 溢出修复层二期 B2/B3 落地（guard 救援链 + 采用/槽位落库）（2026-10-16）

> 性质：**执行 handoff**——供接手会话直接开工。范围 = **二期 B2–B3 两拍**（guard 救援链 =
> 进程内自愈；采用 + 槽位落库 = 状态层）；B4（trimToolResults）/ B5（回归收口）不在内。
> 触发：用户令「给我 handoff 我让另一个 agent 去做」——B2/B3 必要性已于今日对话确认
> （中途拒收 = 一期零帧谓词救不了的面，B1 真栈实测撞活过），语义澄清（×4/5 = 验收线不是
> 砍量；B2/B3 是一个整体、只落一半 = 死缓）已吸收进本文，**无需重开讨论**。
> 状态：**台账待用户过审**（tmp/19-v6/overflow/ 2026-10-12 拆出）——**用户把本 handoff 发给
> 接手会话即视为过审、可从 B2 开工**。
> 取信口径：§0 状态为 2026-10-16 当日实读（git/文件坐标在文内）；机制结论的权威源坐标都在 §5。
> **前置阅读（按序）**：`doc/19-v6/19v6-09-溢出修复层.md` §8（机制设计原文）→
> `tmp/19-v6/overflow/planning.md` 二期节（D12–D16 + 缝勘察五条）→
> `tmp/19-v6/overflow/todos.md` B2/B3 两节（**四要素权威原文——冲突以台账为准**）。

## 0. 一句话现状（2026-10-16 实读；动工当日重新 `git status` 复核）

- 代码 @ `cf2bde3`（另一会话的批已 commit；树上未 commit = **B2 本拍** 5 文件 411+/85-，
  归属已明）。`midfold.go` 的 BeforeModelRewriteState 内部结构（现役 = box nil 直通 → 休眠门
  （stripReasoning+return）→ convertState → 账单/全量估算 → 折叠或放行；注释钉死顺序
  「先估→再折→最后滤」，midfold.go:375-424）——**B3 施工期实读定夺：planning D15 的
  「头部（休眠门后）」= 休眠门块（midfold.go:381-384）之后、`base := box.baseRows()`（:385）之前**。
- **计数口径已升级**（`8d938ee` 起）：countInput 基数 = system + 每行 text + 每行
  tool_calls JSON（compact.go:249 一带）——B2 的 C_rej 用现役口径，勿用立项时快照印象。
- **B2 已完工验收**（2026-10-16，口径 = 裁定 A 四条「救回一次 / 重试成功 / 工具不重跑 /
  数据没动」；回填 = todos B2 节末、证据 = `tmp/19-v6/evidence-overflow/b2/`）——guard.go
  手已装、worker.go 闸已收编（box.recovered() / tryMarkRecovered()，三点同一闸）。终态基线：
  r1 救援后 call4 照鼓 state 再拒 → error（B2/B3 成对差分的一半——B3 后应翻成完整轮 + 落库）。
- **B3 已完工验收**（2026-10-16，三场景活体：run1 采用点 1 中间轮 / run2 采用点 2 收尾轮 +
  下一 POST 活；回填 = todos B3 节末「B3 验收结果」、证据 = `tmp/19-v6/evidence-overflow/b3/`）
  ——foldBox adopted 槽 + setAdopted/takeAdopted、采用点 1（休眠门后）+ 采用点 2（AfterAgent）
  两点已装；B2 终态基线的「下一 POST 仍拒」已翻「下一 POST 活」（差分另一半闭环）。
- **B4 已完工验收**（2026-10-16）：代码 + 单测已落（foldInput trim 兜底 + 循环守卫修 +
  window 入签名）；**活体验收带一项重大发现**——差分半边在活体上**不可达**（ADK 默认
  MaxIterations=20 → 本 run 折行排队列 ≤ 20 → 唯一带工具行的 trim 出口「phase-2 封顶」需
  units ≥ 21 → 活体不存在；两代二进制同话本同死 `exceeds max iterations` 已钉），trim 今日
  = 单测钉形态（六件全过）+ 零变化半边活体对照两代同形（无 trim 行、complete、库行同构）。
  **已裁（2026-10-16 用户令「放开到50」）：MaxIterations 放开 = 50**（`agentMaxIterations`，
  worker.go 常量，主 / 子代理两装配点同值）——同话本① 即刻重放两跑、差分成立：封顶出口
  `compact trim:` 行（预算 8192 tok，还原 0 条，夹 1 条）+ terminal{complete}（帧原文 +
  两跑引擎日志 + psql，证据 `tmp/19-v6/evidence-overflow/b4-max50/`）。详 = todos B4
  节末「验收结果」+ 证据 `tmp/19-v6/evidence-overflow/b4/`（README 含三重论证与重跑指引）。
- **B5 已完工**（2026-10-16，回归网 + 收口）：五场全量重放逐项同形——① m_d1 四端点
  （202+102+字段序/session/frames n=51/log 字段集）对 A4 基线；② A3 截断链（两超窗行格式
  逐字同、末折两态、m_3/m_4 零命中、psql 同构）；③ window=0 负路径（零折/零翻转/无 seq0）；
  ④ A2 拒收链（rescue 行估算 2625116 逐值同、psql 11 行逐位同构、重跑 complete）；⑤ 强折
  失败负路径（rescue 行逐字同、terminal 帧逐字节同 A1、零写入）；**全程 `compact trim` 行零
  出现**（B5 时默认栈下——同日午后放 50 后差分已重放成立，见 B4 条）。单测 13.708s 绿 + vet 干净。doc 回填同拍：19v6-09
  head/§8 勘误/§13-4 ✅+D15 旁注/§14 核对、子台账 planning head+演进、主台账 86 行+§10.6-④。
  证据 = `tmp/19-v6/evidence-overflow/b5-regression/`（README 五场对照表）。**二期 B1–B5
  至此全部落地、19v6-09 §13 五项收官**；唯一开口项 MaxIterations 已裁 50、重放通过（见上）。
- commit 安排（2026-10-16 用户令「放开到50，然后commit」）：本批 = B2–B5 + max50 单 commit
  入库（本文件随批）；不替别的会话 commit——另一会话的批（doc/22、doc/23 等）已由其 commit
  为 `cf2bde3`。
- **话本校准（B1 实测回填，B2/B3 沿用）**：deepseek-flash 真窗 = **1048576**（不是
  ~128k）；实配 = 新会话塞 **3×580,011 字符**填充 + 指令「读 a.txt、b.txt、c.txt 后总结」
  （F 工具环连读；模型行为 = 先 list_directory 再 read_text_file，首读后下一调用即过窗）+
  `-compact-window 4000000`（limit 3983616）。B1 实测拒收：`requested 1057854 > 1048576`，
  报文逐字节在 `tmp/19-v6/evidence-overflow/b1/b1-detect-rescue-lines.txt`（B2 实测用
  3×575,007 字符脚本，逐条判读见 b2 报告）。
- 施工顺序（2026-10-16 用户修订）：**B2 ✅ → B3 ✅ → B4 ✅ → B5 ✅（全部完工交付）**
  （用户令「你改完以后就请另外完成后 继续 b4 b5」）；每拍仍出验收记录、B2/B3 差分成对不合并
  （「下一 POST 仍拒」已由 B2 终态观察钉住、B3 已翻「下一 POST 活」——判例同 A1→A2）；
  不自动 commit、每拍收尾杀自己起的引擎并 lsof 复核。

## 1. 硬约束与环境（动手前先过一遍）

- 硬闸门（AGENTS.md）：**逐 action 停下交审**；验收第一行必须是 curl/日志/psql 打跑着的东西；
  diff 目标 ≤80 行（B2 ≈220 / B3 ≈200 含单测，已论证不拆，判例同 A2，见 todos 原文）。
- Go 不在 PATH：`export PATH="$HOME/sdk/go1.27.0/bin:$PATH"`；代码在 `yk-tako-eino/`。
- 起栈：引擎 `go run ./cmd/yktako -engine 127.0.0.1:8934`（缺省 redis 6390 / pg 5433；`.env` 在
  `yk-tako-eino/`）。**8934 起前 `lsof -i :8934` 查占**，被占换端口、不杀别人的进程；每拍结束
  杀自己起的引擎并 lsof 复核。
- **危险区**：6379 Redis 与 5432 PG 是 otel-ch-native 的活实例——全程不动。
- psql：`PGBIN=~/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin`，
  `$PGBIN/psql -h 127.0.0.1 -p 5433 -d tako`。
- 测试网 = 真实例：`go test ./internal/runengine/ -count=1`（需 6390 + 5433 已起）。
- 实测证据落 `tmp/19-v6/evidence-overflow/b2/`、`b3/`（本 loop 新建）。
- 话本校准纪律（塞历史三条出路：overflow 行=成活 / compact 行=减料 / complete=加料）见
  planning 裁定 10；B2/B3 话本要真 provider 拒收，**不造假**。

## 2. 两个 action（权威四要素见 `tmp/19-v6/overflow/todos.md` B2/B3 节；这里给导览 + 已勘察的接线 + 施工期实读钉）

### 2.1 B2 · guard 救援链：折叠被拒输入 + 重调一次（进程内自愈）· **已完工验收 2026-10-16**

- **升级点已勘察**：`guard.go:63-71`（guardModel.Stream 错误分支，B1 的观测行处升级）。
  ctx 里拿得到 foldBox 已由 B1 活体证实（`foldBoxFrom(ctx)` 非 nil + frameCount()>0 即
  「中途」；零帧静默让位 A2，行为原样）。
- **判据四条件**（D14）：`IsOverflowError(err) ∧ windowOn ∧ box.frameCount()>0 ∧
  box.tryMarkRecovered()`（**闸先花**——收编对象 = worker.go:337 的 overflowRecovered
  局部变量，D12：A2 谓词两处 :349、A3 闸读写 :434-435、日志分叉 :368 随之改读 box，
  **行为零变化，同一闸换载体**）。
- **折叠链全复用**（D14）：`convertState(box.baseRows(), box.run, input)`（midfold 现役）
  → `countInput`（**C_rej = 这份被拒输入的估算，现役口径**：system 从 input[0] 取，
  与 convertState 同款）→ `rescueLimit(C_rej, extractWindow(err) 真值, hasWindow)`
  （overflow.go:167 现役）→ `foldInput(ctx, sys, history, box.run, limit', ...)`（compact.go:388，
  **签名无 force——「force」的施工形态 = 传 rescueLimit 现算的 limit'，A2 的 rescueOverflow
  块同款，worker.go :349-368 区域实读**）→ 摘要行打戳（AttachRun/seq0，worker.go:486-487
  的 A2 先例同款）→ `inner.Stream(ctx, toSchemaMessages(folded), opts...)` **重调一次**：
  成功返新流 + **guard rescue 行**（格式 D14 钉死：`overflow guard rescue: provider 拒收
  （估算 C tok；真值 W' 或盲降）；强折 k 个 run，摘要 S tok；重试`）。
- **负路径**：折败 / 重调再拒 → **原样上抛（上抛第二次的错误——首次拒已进 guard 日志；
  闸已花不再救）**，救援链自身失败 = 原始错误走既有 error 出口（零写入家族不变）。
- **B2 的边界（验收列③，越界 = 本拍白做）**：**不动 box.base、不 appendStaged、不改
  foldBox.baseCount**——救援产物只在进程内，库未动是 B2/B3 差分的对照基线。
- **施工期实读钉**：
  a) rescueOverflow（worker.go A2 救援块）逐行实读——B2 复用它的 rescueLimit/foldInput
     调用形态，但**不**重跑整条 agent.Run、**不**重读 mh（原料 = 当次 input 转录，A2 重读
     mh 的理由「prepareInput 本地变量可能折过」在 B2 不成立：装饰器看到的是实发的那份）；
  b) 重调输入 = `toSchemaMessages(folded)`——折后行由 renderFold/mh 形状派生、**本不带
     思考**（doc/24 的 stripReasoning 管的是「没折的直通路径」），无需重复滤；单测钉
     「重调输入无 ReasoningContent 残留」；
  c) eino 错误 fixtures 已随 A1 钉（overflow_test），IsOverflowError/extractWindow 直接用；
  d) 闸收编后 A3 检出②不重复触发（单测必做项，todos 原文）。
- **验收导览（已完工，口径 = 裁定 A 四条；记录 = todos B2 节末「B2 验收结果」，证据 =
  `tmp/19-v6/evidence-overflow/b2/`）**：① 救回一次——r1 rescue 行（:445，估算 2241697 /
  摘要 623 tok）；② 重试成功——重试输入过关、其后 get_file_info×4 是重试活的硬证据；
  ③ 工具不重跑——tool_result 条数 = 读取次数（r1 26 帧形状见 b2 报告）；④ 数据没动——
  psql 7 行 0 翻 / 0 seq0；A2 零帧回归照旧（r3 8 翻死 + 1 seq0）。**归 B3 验收记录的项**：
  「下一 POST 仍拒」= B2 终态基线（r1 call4 照鼓 state 再拒 requested 1049976 → error，
  即死缓活证——B3 后应翻成完整轮 + 落库）；「整轮跑完 done/complete」在 B3 拍验收。

### 2.2 B3 · 采用 + 槽位落库（状态层，D15）· **已完工验收 2026-10-16**

> 验收记录 = todos B3 节末「B3 验收结果」；证据 = `tmp/19-v6/evidence-overflow/b3/`
> （run1/run2：rescue 行、psql 行形状、frames、g2「下一 POST 活」两连完）。以下导览保留作
> 施工对照（机制坐标仍在文内）；施工期修订（采用 = 槽 + 尾随行）已被活体证成。

> 施工期修订（动工前已落账，todos B3 节末）：采用 = 「system + 槽 + **尾随行**」，不是
> §2.2 原文的字面 `[system]+槽`——字面版会丢救援点之后的「重试回复 + 工具结果」（重生调用 =
> 帧双份、工具两遍，与 B2 验收③冲突）或丢最终答复行（场景 b）。尾随行 = 救援点之后 ADK
> 追加的新行，guard 写槽时记行数（len(input)）供切分。全文与理由在 todos。

- **槽位新件**：foldBox（midfold.go:70 一带）加 adopted 槽 + `setAdopted/takeAdopted`；
  guard 折成后 `box.setAdopted(folded)` + `box.appendStaged(*staged)`（appendStaged 现役
  :94，先例 :391——**落库走 EndRun 吃 staged 列表的现役路径，不改 EndRun/存储层**）。
- **采用点 1（主路径）**：`BeforeModelRewriteState` 头部——`takeAdopted` 非空 →
  `state.Messages = [state.Messages[0] 原样] + toSchemaMessages(槽)` + `box.setBase(槽)` +
  返回（**跳过本轮估算折叠**：形状刚折成、目标线已按被拒输入校准，再折一遍 = 同段历史
  两份漂移摘要）。
- **采用点 2（兜底）**：`AfterAgent`——槽非空 → `box.setFinal(槽)` 直接返回（**跳过
  convertState**——state 鼓着、base 未动，不能从它算）。缺这点 = 「被救的是最后一轮」
  落坏账。现役 AfterAgent（midfold.go:402-413）先 convertState 后 setFinal，加头部早退分支。
- **施工期实读钉（6c0d525 后的新接缝，本文出前刚实读）**：
  a) **采用点与「先估→再折→最后滤」的共存位置**：planning D15 写于 doc/24 之前，「头部
     （休眠门后）」落在现役代码哪一行施工期实读定夺。已知交互两条，登记不裁：① 采用重写
     后的 state 由 toSchemaMessages 派生、**无 ResponseMeta** → 下一轮 usageBasedInput
     找不到锚点 → 自动回退全量 countInput = doc/24 §6「刚折完」分支的**既有行为，不是
     缺陷**，单测钉即可；② 采用路径跳过 stripReasoning 的安全性——折后行本无思考，
     大概率无操作，单测钉「采用后 state 无思考残留」；
  b) 采用不重新摘要 = 写两处同一份（state 重写 + staged），单测钉 staged.Summary 与
     state 里的摘要行同源；
  c) guard 折 + 采用与 midfold 自身折叠的共存序列（todos 原文：不重复摘要）。
- **验收导览**（话本同 B2，期望再换）：① rescue 行照旧 + frames done/complete；
  psql = 被折行 compacted=t + 新 seq0 摘要行 compacted=f + **本 run 行 = 折后保留单位**
  （被折走的本 run 单位不在库中——mhRowsFromFold 从折后 final 派生）；② **下一 POST 活**
  （墙消失——B2/B3 的成对判据反转）；③ 采用两点各一场景：a) 中间轮被救（读后模型继续
  调工具/再答）→ 下一轮钩子采用生效；b) 最后轮被救（读后直接收尾）→ AfterAgent 兜底
  （① 的 psql 形状即其证）。单测全绿收尾。

## 3. 已裁承重裁定（防重开论战）

| 裁定 | 内容 | 出处 |
|---|---|---|
| B2/B3 拆拍 | 官方定界：WrapModel 改输入不持久进 state（单次调用层 = B2），state 重写归 BeforeModelRewriteState（状态层 = B3）；合拍 ≈350 行超 A2 判例体量线 | planning 拆分明线第 2 条；handler.go:246-250 实读 |
| B2 边界 | 不动 base / 不 appendStaged / 不动 baseCount——库未动是差分对照基线 | todos B2 验收列③ |
| 闸先花 | tryMarkRecovered 成功才进折叠；折败/再拒上抛第二次错误；一 run 一救（D5） | planning D14 |
| 日志行 | B2 rescue 行 / B1 观测行全文钉死（只多 guard 一词，与 A2 rescue 行同族便于对照） | planning D14 |
| C_rej 口径 | C_rej = 被拒输入的 countInput（现役：system+text+tool_calls JSON），不是 box.baseCount | planning D14；compact.go:249 |
| 采用不重新摘要 | 同一份折后形状写 state + staged 两处，防两份漂移摘要 | 19v6-09 §8；planning D15 |
| 落库不改存储层 | staged 进现役列表 → EndRun 既有单写点天然落库（「EndRun 槽位落库」的施工等价） | planning D15 |
| 采用双点 | 主路径 BeforeModelRewriteState + 兜底 AfterAgent（缺兜底 = 最后一轮被救落坏账） | planning D15 |
| 话本校准 | 真窗 1048576（非 ~128k）；3×580,011 字符 + 工具连读 + window 4000000 | B1 验收回填（todos B1 节末） |
| 子代理不挂 | guard 装配面天然排除（agents.go 无 Handlers 字段），零排除代码 | guard.go 头注 |
| D12–D16 全部 | 帧计数收编 foldBox / guard 链尾 / 折叠全复用 / 采用双点 / trim 挂点（B4 用） | planning 二期节，无留待拍 |
| B2 验收口径（裁定 A） | B2 = 「救回一次 / 重试成功 / 工具不重跑 / 数据没动」四条；「整轮跑完」与「下一 POST 活」归 B3 验收记录 | 2026-10-16 用户「本来就是选A啊」 |
| 连续推进令 | 自 B3 起连续推进 B3 → B4 → B5，不再逐拍停审；每拍仍出验收记录、差分成对、不自动 commit | 2026-10-16 用户「你改完以后就请另外完成后 继续 b4 b5」 |
| 采用 = 槽 + 尾随行 | B3 施工期修订：字面 `[system]+槽` 会砸 B2 已验收不变量（场景 a/b 见 todos B3 节末）；尾随行 = 救援点之后 ADK 追加的新行 | todos B3 节末施工期修订（交审重点项） |

## 4. 停线（不在本 handoff 范围）

- **B4 trimToolResults**（n=0 兜底瘦身，规则四件）与 **B5 回归收口**——另拆四要素后动工。
- 一期 A1–A4、三期（limit 重裁）已落地验收——只回归、不改。
- 子代理溢出不救、(c) 子检、Usage 基线复审项——均已裁，见 19v6-09 §11。
- 四端点字节 / runlog / 闸 / 计数口径（D11）/ 帧 kind 全不动。

## 5. 素材坐标索引（2026-10-16 实读）

| 素材 | 位置 |
|---|---|
| 机制设计真源 | `doc/19-v6/19v6-09-溢出修复层.md` §8（装饰器形态 / 采用没它会死 / D6 落库） |
| 施工级裁定 | `tmp/19-v6/overflow/planning.md` 二期节（D12–D16 + 缝勘察五条 + 拆分明线四条） |
| 四要素权威 | `tmp/19-v6/overflow/todos.md` B2/B3 节（改什么/为什么/怎么验收/回答什么问题） |
| B2 升级点 | `guard.go:63-71`（B1 观测分支）；闸收编 `worker.go:337`（A2 块 :349-368、box 同步先例 :486-487） |
| B3 采用点 | `midfold.go`：foldBox :70（appendStaged :94、frameCount :127）、BeforeModelRewriteState :348（休眠门 :354-357、重写+同步先例 :389-391）、AfterAgent :402 |
| 折叠链 | `compact.go:388` foldInput（注释 :385-386 明写救援形态）、`overflow.go:76` IsOverflowError / :118 extractWindow / :167 rescueLimit |
| doc/24 新接缝 | `midfold.go:262` stripReasoning、:283 usageBasedInput、:343-397 先估→再折→后滤（B3 实读钉 a 的原文） |
| B1 话本与证据 | `tmp/19-v6/evidence-overflow/b1/`（报文逐字节、psql、帧序）；拒收行 `requested 1057854 > 1048576` |
| 新证据落点 | `tmp/19-v6/evidence-overflow/b2/`、`b3/`（本 loop 新建） |
| 环境 | `scripts/dev.sh`；`yk-tako-eino/.env`；PG 5433 / Redis 6390；同判例 handoff = `doc/log/2026-10-12-overflow-handoff.md`（A1–A4 已完工，仅供查环境纪律） |

## 6. 纪律提醒（给接手会话）

- 本文件是导览不是裁定：机制级事实都有源坐标；**冲突以 doc/19v6-09 与子台账原文为准**。
- 承重论断先验前提（AGENTS.md）：文档注释、记忆、印象、已废代码都不是证据，证据只有代码、
  配置和用户原话。凡本文写「实读」处，先实读再动手。
- 每拍仍出验收记录；不自动 commit；`tmp/` 台账不入 git。
- 施工顺序（2026-10-16 修订）：B2 ✅ → B3 ✅ → **B4（进行中：代码+单测已落，活体差分待跑）
  → B5** 连续推进、不再逐拍停审（用户令）；差分成对不合并验收；B4 在 B2/B3 未 commit diff 上
  续叠（同一拍分拍）。
