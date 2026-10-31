# 47 · Plan 审核与追问澄清

> 性质：**已落地（2026-10-23）**。两条 loop：**Q（ask_user，doc/41 Loop A 换名复活）+ P（plan gate，kimi 式运行时守卫，模型自主触发）**，按 §6 顺序 Q1→Q2（∥P1）→P2→Q3（∥P3）→Q4→Q5 全部收口；同日按用户「交互要像 kimi」的要求重做前端交互层（§5.2/§5.4，decision=approve/revise 语义入列）。
> 状态：✅（Q1–Q5、P1–P3 已落地；`go test`/`go vet`/快闸 `assert_contract.py`/`check-expected.sh` 全绿；活体：问题卡瀑布、方案卡编辑后执行、`question_crash.sh` 16 条断言全过；交互重做后重跑活体：提问→选项→看板→方案→驳回→重提→编辑后批准→执行，看板 5/5）。
> 开放问题：**1–5 全部已拍**（2026-10-23，见 §8）——1 拍为 **A 纯模型自主**；6（驳回语义）当日补拍，见 §8。
> 日期：2026-10-23（方案与落地同日；P 线因开放问题 1 翻案重写为 A 形态；交互层当日按 kimi 参照重做）。
> 代码落点：`yk-tako-eino/internal/runengine/{questions.go,waittools.go,plangate.go,journal.go,worker.go,server.go}`、`web/src/{chat.ts,App.vue,PlanDoc.vue}`、`scripts/acceptance/{question_crash.sh,expected.json}`、`doc/19-v6/19v6-04-验收-curl.md`（用例 10/11）。
> 参照：doc/41（Loop A 全套设计——Q 线直接取档施工；`run_approvals`→`run_questions`，P 的批准行与 Q 的问题行共用此表，`kind` 列区分）；doc/01（「Plan 层不建 gate」的旧裁定——本篇 §3.2 是对该裁定的**翻案**，理由见 §0 病灶一）；doc/30（崩溃续跑底座：journal 重放、interrupted 派生、resume claim CAS）；doc/06（三态守卫语义遗产）；doc/33（沙箱围栏）；doc/40（响应信封与业务码表，§5.3 新增 `15` 码段）；doc/19-v6 四篇（契约与存储真源）；外部参照：eino HITL 官方文档（8 个示例）、kimi-code（plan mode 全套：EnterPlanMode 工具 + plan guard 运行时拦写 + ExitPlanMode 人审）、deepseek-harness（timed pending 变体）。
> 与 doc/16 的关系：不翻 #7（内存 checkpoint + interrupt 审批，doc/41 已判不做并维持）；本篇是**新增场景**（plan 审核 + 中途追问澄清）。落地后需回改 doc/16 §2 表 #8 行（Web 多入口「人工审批未做」的表述）与 §2c。

---

## 0. 为什么两件事一份方案

**背景**：2026-10-23 用户提出两个判断：① plan 有必要给人审——doc/01「Plan 层不建 gate」的旧裁定是在 v1 最小范围思维下拍的，时过境迁；② 追问澄清应该是**一把工具**（ask_user），而不是模型把问题当终稿文本交出来。两条判断指向同一个缺口：**「人」这个外部输入源在 v6 引擎里没有正式的进入通道**——今天人只能通过「发一条普通消息」影响 agent，既无结构化的问题/方案载体，也无「任务在等输入」的状态。

两个会真实发生的病灶故事：

**病灶一（plan 无审核）**：租户里的运营同事发「把 workspace 里的旧文件清理一下，再把报告整理成归档目录」。模型对「旧文件」的理解和小张不同——`rm` 掉一批还混着没提交配置和财务表的文件。每一道现有防线都放行：沙箱围栏只挡租户目录**之外**的读写，这些操作全在目录**之内**，合法；PII 脱敏管敏感串外泄；任务看板只是让丢的过程**可见**。破坏发生在第几秒，人没有任何动手前的检查点。而模型全部意图在动手那一刻只存在于它自己的权重里——**方案是全部意图的浓缩，审一份方案等于提前审完它接下来要发的每一次工具调用**。doc/41 当年把 plan gate 列入「不做」靠的是 doc/01 的 v1 裁定；现在产品要交付给不懂技术的人用，这个检查点从「锦上添花」变成「唯一一次介入机会」。

**病灶二（追问无载体）**：小张的消息本身就有歧义（「那个报告」是哪份？）。今天模型有两条路：猜——答错了二十分钟的活全白干；把问题当终稿交出来、等下一条消息——这条路其实**能走**（v6 的 mh 装配天然支持），但问题没有身份（答案回来时「在答哪道题」靠自然语言维系）、前端画不出问题卡、模型「决定提问」这个承诺不留痕迹。kimi 和 deepseek-harness 都把追问做成了正式工具，eino 官方 HITL 示例里它也是八个场景之一——这是被反复验证过的需求。

两个病灶共用同一副地基：**把「人」建模为外部输入源，并且现场永远在库里不在进程里**。分开做，等于两次重造同一套帧、端点、表和前端卡。所以：一份方案，两个 loop。

**端到端链路**（两条，机制在链路中的位置见 §3）：

```
链路 Q（追问澄清，ask_user——run 中途的现场在库里，等待者是可重建的临时雇员）：
执行中模型遇到歧义/缺信息 → 调 ask_user(question, options?)（普通 tool_call，journalWrapHandler 先记 intent）
  → handler 三拍：INSERT run_questions 行（kind=question, pending）→ AppendFrame kind=question → 轮询该行（1–2s，尊重 ctx）
  → run 仍 running、帧流静止、watchdog 照续租约（worker.go:519-526）、不烧 token
人：页面问题卡（options 快捷按钮 + 自由文本框）→ 提交答案 → POST /api/questions/:id/answer（PG CAS 一答终答）
  → 轮询到行变 answered → 答案作为 tool_result 返回 → 模型同一轮继续
挂起中进程死：租约过期 → 派生 interrupted → 客户端 resume（既有 claim CAS）→ replayJournal 对到行：
  pending → 重新挂起（重发 question 帧，天然幂等）；answered → 答案注回 tool_result 继续跑

链路 P（plan 审核，A 形态——kimi 式运行时守卫，模型自主触发，无开关无入口 flag）：
模型判断任务多步/破坏性/不可逆 → 调 enter_plan_mode（runs 行 plan_mode 置 true，durable，崩溃恢复后仍在）
  → planGuardHandler 生效：此后一切写/执行类工具调用被硬 deny（解释文本当 tool_result 回模型，工具未执行、不留 intent）
  → 模型用读工具侦察 → 产出方案 → 调 submit_plan(plan)
    → 三拍复用 Q 机器：落行（kind=plan，question 列存方案全文，pending）→ 发帧 → 轮询
人：页面方案卡（文档态展示全文）→ 批准 / 编辑后批准 / 退回修改 → answer 端点 CAS（带 decision）
  → approve：行 answered，批准文本（含修改）注回 tool_result，runs.plan_mode 置 false → 守卫放行 → 同一轮继续执行
  → revise：行 answered（decision=revise），驳回文本注回，**plan_mode 保持 true** → 模型改方案后重新 submit_plan（新行、新卡）
崩溃/重启：plan_mode 在 runs 行（PG），resume 重建状态；submit_plan 的 intent 对到 kind=plan 行 → 同 Q 第三分支
```

## 1. 名词准入（操作定义，后文不再重复解释）

- **ask_user**：追问澄清工具，`ask_user(question, options?)`。handler 三拍：落 `run_questions` 行（kind=question，pending）→ 发 `kind=question` 帧 → 阻塞轮询该行直到 `status != pending`；答案文本作为 tool_result 返回模型。语义继承 doc/41 的 ask 三拍，把「裁决」换成「回答」。**options** 为可选快捷选项：模型在有明确候选取值时给 2–4 个（目标文件、环境二选一），前端渲染快捷按钮；不给则纯自由文本。
- **plan gate**：动手执行前的人审闸，**A 形态 = kimi 式运行时守卫**。触发是模型自主（调 enter_plan_mode）；硬保证是**进模式后**一切写/执行类工具被 planGuardHandler 当场 deny——解释文本当 tool_result 回模型，工具未执行、不留 intent。
- **plan mode**：`runs.plan_mode = true` 的一段 run 状态（durable：存 runs 行，崩溃恢复后仍在）。进：`enter_plan_mode`；出：`submit_plan` 被**批准**（decision=approve，注回时置回 false）。**退回修改（decision=revise）不退出模式**：模型按意见改完再 submit_plan。plan mode 内读工具与看板工具照用（方案阶段先把步骤建成 todo）、ask_user 放行（问澄清）、委派与 MCP deny。
- **enter_plan_mode**：进模式工具，无参。handler 置 `runs.plan_mode=true`（幂等），返回指引文本（写/执行已锁定，请侦察后调 submit_plan 提交方案人审）。
- **submit_plan**：提交方案工具，`submit_plan(plan)`。三拍复用 Q 机器，落的行 `kind=plan`、`question` 列存方案全文；批准后注回「方案已批准」+（若有）用户的修改，同时退出 plan mode；驳回后注回「方案被驳回，尚未执行，计划模式仍开」+ 人的意见，模型改完重交。
- **写/执行黑名单**（`isMutatingTool`，纯函数金样钉死）：守卫的判定——write_file/edit_file/move_file/create_directory/bash/memory（写）/委派工具/MCP 工具 → true；读类工具/get_time/`TaskList`/`TaskGet`/`TaskCreate`/`TaskUpdate`/ask_user/submit_plan/enter_plan_mode → false。看板工具只改 `run_tasks`、不动工作区，放行是为了让方案阶段就能把步骤立成 todo（2026-10-23 交互重做时从黑名单移出，见 §8-6）。
- **问题行（run_questions 表一行）**：一次「等人」的持久态，`kind` 列区分 question（追问）/ plan（方案审批）。状态机 `pending → answered`（外加 run 终态后清扫器收的 `stale`）。多副本下「人在另一个入口回答、worker 在本进程等」的共享阵地。
- **挂起**：worker goroutine 阻塞在行轮询上，run 不结束、租约照续、不烧 token。与 `interrupted`（死了）相对，挂起是**活着在等**。
- **resume 第三分支**：`replayJournal`（journal.go:141-255）重放到 ask_user / submit_plan 的 intent 时按行对账——无行 → 维持现状（`interruptedResultText` 回注，at-most-once 不重跑）；`pending` → 重新挂起；`answered` → 答案/批准注回 tool_result 继续。对齐 doc/41 Loop A 的 A5 设计。
- **checkpoint（v6 语义）**：不重建 eino gob checkpoint（doc/30 §15 裁断沿用）。挂起现场 = journal 的 intent + 行 + mh（+ P 线的 plan_mode 列）；断点定位 = journal 按 call id 归组重放。

## 2. 现状锚点（方案就建立在这些事实上）

结合现行代码（2026-10-23，main @ 随 doc/41 `fd6a489` 之后）逐条锚定：

1. **守卫挂点现成**：`journalWrapHandler` 就是 `adk.ChatModelAgentMiddleware` 覆写 `WrapInvokableToolCall`（journal.go:55-65），在 handler 链最内位；doc/41 §4 已把 gate 的位置定好——**gate 在 journal 外**（deny 的调用从未发生，不该有 intent）。planGuardHandler 用同一个钩子类挂在 journal 外。
2. **ask_user 作为池内普通工具自动获得两层既有包装**：`wrapToolErrors`（PII 打码，tools.go:49——答案/方案文本出向打码免费）+ journal 记账（intent 先落账再执行，journal.go:79-121——挂起现场的一半免费得到）。
3. **帧 8 种、写帧唯一口**：词表 `start/delta/tool_call/tool_result/usage/terminal/resume/task`（contract_test.go 钉死，doc/41 已从 7 扩到 8）；`AppendFrame` 是写帧唯一口（RPUSH `tk:runlive`）。P/Q 共用一个新 kind `question`（语义=「run 在等这人这一行」，`name` 区分 ask_user/submit_plan）——词表 8→9，测试会立刻红，**显式同步点，不是隐患**。
4. **journal 意图先行**：intent 写失败不执行工具；result 写失败 run 走 error。崩溃恢复 `replayJournal` 按 call id 归组，缺结果回注 `interruptedResultText`（at-most-once）。P/Q 的挂起/崩溃兼容**天然借这层**——挂起时 intent 已在账本里，重放对得上。
5. **interrupted 是派生视图不是存储状态**（doc/30 §2.3）：runs 行保持 `status='running'` + 租约过期，读侧派生。P/Q 挂起**沿用同一哲学**：不加存储状态，挂起 = running + 存在 pending 行。
6. **恢复通路现成且已验收**：`POST /api/runs/:id/resume`（server.go:579-645）PG `FOR UPDATE` CAS、epoch+1、三个 409（112102/112103/112104）；`seedResume` 清空 runlive 重建；`RunResume` 与 Run 共用主体。第三分支是它的一个分支，不新造通路。
7. **存储形态可照搬先例**：`run_tasks` 表经 autoMigrate 从 struct 落库（pgstore.go:488-498 模式）；runs 加列有现成迁移路径；清扫器挂 doc/37 同一轮（`cleaner_state` 租约 + `archive_sweep.go`）；dev 探针转正有 doc/37 clean-sweep 先例。
8. **业务码段有空位**：`11` 身份 / `12` 会话与运行 / `13` 配额 / `14` 项目与工作区（doc/40 §4）。doc/41 曾提议 `15` 审批段随 Loop A 取消未入库——本篇复活该段并定名**人审与问答段**。
9. **契约零变更（A 形态的红利）**：模型自主触发意味着不需要入口 flag——POST /api/runs 请求体不变、GET /api/runs/{id} 响应字节不动（plan_mode 列**不出 API**，前端靠 list 端点感知等待）。本篇对既有契约的唯一变更 = 帧词表 +9（新端点全是新增）。
10. **doc/41 Loop A 档案全在**：三拍、CAS 裁决、resume 第三分支、留档验收写法全部在案（doc/41 §6）。Q 线 = `run_approvals` 改名 `run_questions`、`approved/rejected` → `answered`；P 线的批准行**与 Q 共用一张表**（`kind` 列）——设计债已付，施工取档即可。

## 3. 总设计

### 3.1 共享机器（Q 先施工，P 复用）

P 和 Q 共用同一套「等人」机器，Q 先施工成底座：

```
等人机器 = run_questions 表（kind 列：question/plan）
         + kind=question 帧（name 区分发起工具，args 携原始参数 JSON，messageId=行 id）
         + 工具 handler 三拍（落行 → 发帧 → 轮询至 answered → 注回 tool_result）
         + answer 端点（PG CAS 一答终答）
         + resume 第三分支（无行 / pending / answered）
```

写/执行工具的黑名单判定、plan_mode 守卫、enter/submit 两个工具是 P 线叠加在这套机器上的增量。

### 3.2 plan gate（Loop P）：kimi 式运行时守卫，模型自主

**进模式**（enter_plan_mode）：模型判断任务多步/破坏性/不可逆（系统提示词约定边界），调 `enter_plan_mode` → `runs.plan_mode=true`（幂等；存 runs 行 = PG 共享阵地，崩溃恢复后 resume 重建）。返回指引文本：写/执行已锁定，请用读工具侦察，产出方案后调 submit_plan 提交人审。

**守卫**（planGuardHandler，journal 外）：每次工具调用先过 `isMutatingTool(name)`——plan_mode=true 且命中黑名单 → **deny**：解释文本（「plan mode 下不允许修改，请先完成方案并调 submit_plan 提交人审」）当 tool_result 回模型，工具未执行、不落 intent。放行：读类工具（侦察照做）、ask_user（plan mode 内问澄清）、enter_plan_mode（幂等重进）、submit_plan（出口）。

- **为什么守卫而不是装配剥工具**：装配剥工具要求入口 flag（显式开关——用户已否），运行时守卫把触发权还给模型（kimi 全套同款：EnterPlanMode 工具 + plan guard 逐调用拦）。
- **硬保证的准确表述**：「**进模式后**想写也没有通道」——deny 是 middleware 硬拦，不是提示词；「**进不进模式**」是提示词（软）。这是 A 形态与开关形态的取舍，已拍（§8 问题 1）。残余风险：模型该进不进直接干——由提示词约定 + deny 反馈矫正 + 用户 stopRun 兜底；将来若出现真损失，可加「首次写前必审」升级（弃用方案 B 留档，见 §7）。
- **plan mode 内读工具全开**：方案质量靠真侦察；kimi 的 plan 模式也只拦写、留读。

**出人审**（submit_plan）：模型侦察完调 `submit_plan(plan)` → 三拍复用等人机器（行 kind=plan、question 列存方案全文）→ 人审期间 run 挂起（running + pending 行，同 Q）。

**批准与放行**：人在方案卡（对话流里的文档态方案 + 输入区上方的审核面板）点「批准执行」或「编辑后批准」→ answer 端点 CAS（decision=approve）→ 行 answered（`answer` 列存**编辑后的方案全文**，未编辑存空串）→ 轮询注回：批准文本（「方案已批准，按方案执行」+ 有修改则附「用户的修改：\n<全文>」）作 tool_result 返回，**同时 `runs.plan_mode` 置回 false** → 守卫放行，模型同一轮继续执行。想撤回同 Q 走 stopRun。

**退回修改（不是拒绝）**：点「退回修改」并写意见 → answer 端点 CAS（decision=revise，answer=意见）→ 注回「方案被驳回，尚未执行；计划模式仍然开启。请根据用户意见修改方案，再调用 submit_plan 重新提交…」→ **`runs.plan_mode` 保持 true**（守卫不放行）→ 模型改完重新 submit_plan，落新行、发新帧、页面另起一张新方案卡，旧卡标「已驳回」并留住当时的意见。

**系统提示词追加段（何时进模式）**：任务涉及多步修改、删除/覆盖文件、批量改动前，先调 enter_plan_mode 出方案过人审；不要绕过守卫（绕不过）。

**没 plan gate 会怎样**：病灶一原样发生——围栏挡不了目录内的破坏，人在破坏发生前没有介入权。

### 3.3 ask_user（Loop Q）：Loop A 换名复活

**工具三拍**（每步四问齐全）：

1. **落行**：`INSERT run_questions`（kind=question，tenant_id/workspace/run_id/session_id/question/status=pending）。不做会怎样：进程死了对不清在等什么。
2. **发帧**：`AppendFrame` 一条 `kind=question`（字段映射见 §5.2）。不做会怎样：前端不知道在等人，页面死寂。
3. **阻塞等**：goroutine 内 1–2s 轮询该行直到 `status != pending`；全程尊重 ctx（stopRun 能打断，run 走 stopped，行留 pending，清扫器收 stale）。答案文本作为 tool_result 返回 → 模型**同一轮继续**。

**挂起现场 = 账本 intent + 行，不新造 checkpoint**。挂起期间 worker 活着：watchdog 照续租约、帧流静止、agent loop 不消耗迭代数。模型输入的下一次装配本来就从 mh 全序 + 账本重放出发（`prepareInput`），中断 run 的行本就未进 mh（EndRun 才落）——journal 记 intent、questions 行记内容与答案、mh 记对话流，三样分工互不重叠。

**options（2026-10-23 拍）**：`options: string[]` 可选。question 帧 `args` 放模型给出的**原始参数 JSON**（与 tool_call 帧同惯例），text 仍是问题全文；前端从 args 解析 options 渲染**快捷按钮 + 自由文本框**（点选项 = 选项文本作为 answer 提交，answer 端点与表结构不变）。有明确候选取值时给 2–4 个，没有就留自由文本。

**系统提示词追加段**：遇到歧义／缺关键信息／不可逆动作意图不明时调 ask_user，不要替用户猜；一次只问一个问题；**有明确候选取值时通过 options 给快捷选项**。

**崩溃兼容 = journal 重放的第三分支**（取 doc/41 A5 档案）：

| 账本 intent 对到的行 | 重放行为 |
|---|---|
| 无行 | 维持现状：`interruptedResultText` 回注，不重跑（at-most-once） |
| `pending` | **重新挂起**：重发 question 帧（seedResume 已清空 runlive，天然幂等），继续轮询 |
| `answered` | **注回答案/批准**：内容作为该次调用的 tool_result（submit_plan 行附带退出 plan mode），模型继续跑 |

**等待者的多副本定性**：阻塞中的 goroutine 只是**可重建的临时雇员**——进程死了 → 租约过期 → interrupted → 客户端 resume 在任何副本重建等待者，现场全部从 PG（行 + journal + runs 租约 + plan_mode 列）恢复。家规的准确表述是「**现场在库不在进程**」，P/Q 满足它。

**授权语义**：没有拒绝分支——answer 只可能是回答/批准（人答什么是什么）；想撤回用 stopRun。挂起时长**无上限**（已拍），lease 照续；人几天不答，会话闸就占几天——与 doc/41 同款取舍，stopRun 是手动出口。

**子代理继承**：ask_user 静态注册进一手池（tasktools 之后 append，worker.go 定位）。coder 白名单不含它（不变）；researcher 走 exclude_tools 继承路会多拿到它——中途问人符合 researcher 定位，**金样测试 `TestShippedAgentSpecToolGuards` 的 effective 名单随 Q2 同步一行**。enter_plan_mode / submit_plan 同理入池（plan mode 通常主代理触发；子代理在 plan mode 内委派被守卫 deny，天然成立）。

### 3.4 共享哲学与外部参照

两个 loop 一条根：**人是外部输入源，进入系统的唯一形态是消息/行**；等待的现场永远在库里，进程里的等待者只是可重建的临时雇员。外部参照的分族：

| 参照 | 形态 | 我们取什么 |
|---|---|---|
| eino HITL（官方 8 示例） | 图挂起 + gob checkpoint + 地址重入 | 场景清单（plan 审核=示例 6 族、追问=示例 4 族）；**不取** checkpoint/寻址系统（v6 无可重入执行图，doc/30 §15） |
| kimi-code plan mode 全套 | EnterPlanMode 工具（模型自主）+ plan guard 运行时拦写 + ExitPlanMode waitUntil 人审 | **全套机制对标**（P 线 = 它在 v6 的移植）；**不取** waitUntil 进程内等待（批准等待复用 Q 的轮询 + resume） |
| kimi-code / dsh 的 ask 工具 | 进程内 promise park | 工具形态与提示词惯例；**不取** park 载体 |
| dsh timed ask_user_question | 调用以 `{pending:true}` 完成、答案作为后续 turn 的 user message | 「工具不等人，等人落成消息」的同路证据；我们选了更强的同轮注回（理由 = 工具契约完整性 + run 连续性） |
| doc/41 Loop A | 三拍 + CAS + resume 第三分支 | **直接取档施工**（Q 线 = 换名复活；P 线 = 共用其表） |

## 4. 顺序即语义：handler 链与装配点

```
一手池工具（worker.go:193-258 既有，Q2 起 +ask_user，P1 起 +enter_plan_mode/submit_plan）
  └─ planGuardHandler（新增 P1，journal 外，仅 plan_mode run：黑名单命中 → deny 解释文本）
       └─ wrapToolErrors（既有：错误→结果 + PII 打码，tools.go:49）
            └─ journalWrapHandler（既有：intent 记账——deny 的调用 never 到这层，天然无 intent）
                 └─ 真工具（ask_user / submit_plan 的三拍在这层：落行→发帧→轮询）
```

- planGuardHandler 在 journal **外**：deny 的调用从未发生，不该有 intent（doc/41 §4 的既定分工）。
- 两个「等人」工具在 journal **内**、贴真工具：intent 先于轮询落账，崩溃重放才对得上；答案/方案文本出向经 `wrapToolErrors` 打码。
- submit_plan 的**注回动作含写 runs.plan_mode**——它发生在 handler 内部（真工具层），不经守卫（守卫只管模型发起的工具调用；handler 内部直写 store，对齐 tasktools 直写 PG 的先例）。
- 帧进当前 run 的 runlive；子代理若在执行轮调 ask_user，帧出现在父 run 帧面，属预期（登记为已知形态，同 doc/41 §4 的 MCP 约定）。

## 5. 契约

### 5.1 新表与改列（autoMigrate 从 struct，沿用 pgstore.go:488-498 模式）

**`runs` 加列**：`plan_mode bool NOT NULL DEFAULT false`。**不出 API**（守卫内部读 + 崩溃恢复用；前端靠 list 端点感知等待）——既有端点字节零变更。

**`run_questions`**（P/Q 共用的多副本阵地）：

| 列 | 类型 | 说明 |
|---|---|---|
| id | string ULID `qs_` | 主键；同时进 question 帧的 `messageId` 字段 |
| tenant_id / workspace | string | 可见性过滤键（对齐全部 PG 表的 scope 口径） |
| run_id / session_id | string | 回属；resume 重放按 run_id 对账 |
| tool_call_id | string | 发起这次等待的工具调用 id；resume 重放按它把 journal 里缺结果的 intent 对到行上（空串 = 对不上，按无行处理） |
| kind | string | `question`（ask_user 追问）/ `plan`（submit_plan 方案审批） |
| question | string | 等人内容全文：question 行=问题；plan 行=方案全文 |
| args | string | 工具原始参数 JSON（question 帧的 args 同源；前端刷新后从 list 端点重建问题时靠它解析 options） |
| status | string | `pending → answered`；run 终态后清扫器收 `stale`（挂 doc/37 同一轮） |
| answer | string | 答案全文（点快捷选项=选项文本）；plan 行 approve 时=编辑后的方案全文（未编辑空串），revise 时=人的修改意见（可空） |
| decision | string | plan 行专用：`approve` / `revise`；question 行恒空串。approve 退出 plan mode，revise 保持 |
| created_at / answered_at | — | 审计面 |

**2026-10-27 补记（`stale` 没有生产者）**：`stale` 今天只存在于词表、查询过滤与读侧分支（`waittools.go:262`、`questions.go` 的 status 校验、journal 重放注释）；全仓 `UPDATE run_questions` 只有 `questions.go:114` 那一处 CAS（`pending → answered`），**没有任何代码把 `pending` 改写成 `stale`**。所以上表 `status` 一行写的「run 终态后清扫器收 stale」是设计口径、未实现：进程收摊或被 stop 的 run 会留一行永远 `pending`，`GET /api/runs/:id/questions?status=pending` 照常把它列出来。补法与 doc/37 §5.5 的会话级清理同一拍——run 终态时把该 run 的 pending 行收 `stale`，过窗随会话删行。

### 5.2 帧词表：8 → 9

`contract_test.go` 词表测试同步改（快闸显式同步点）。新帧复用现有 Frame 字段不加列——P/Q 共用一个 kind，语义=「run 在等这人这一行」：

| kind | name | args | text | messageId | 语义 |
|---|---|---|---|---|---|
| `question` | 发起工具名（`ask_user` / `submit_plan`） | 原始参数 JSON（含可选 `options`） | 等人内容全文（问题 / 方案） | 行 id | run 在等这行的答案/批准 |

SSE 与轮询同真源透传（server.go:978 原样透传，零改动）；前端 `chat.ts` 分派加一个分支、按 `name` 渲染问题卡或方案卡。交互层对齐 kimi（2026-10-23 重做）：**对话流里留档案**（问题记录 = 编号选项 + 你的回答；方案记录 = `PlanDoc` 按标题/列表/表格/代码块渲染的文档，带状态 chip），**待答现场挂在输入区上方**（问题面板编号选项 `→ [n]` + 自由文本；方案面板 = 批准 / 编辑后批准 / 退回修改；多条待答可翻页）。刷新后靠 `questions?status=pending` 从 PG 重建面板与档案，不依赖帧的 TTL。

### 5.3 端点（信封走 doc/40；全部新增，既有端点字节零变更）

| 端点 | 契约（成功体在 `data`） | 失败码（新增 **`15` 人审与问答段**，doc/40 §4.2 补行） |
|---|---|---|
| `GET /api/runs/:id/questions?status=pending` | `[{questionId,runId,kind,question,args?,status,decision?,createdAt}]`（plan 行的 `question` 字段存方案全文；`args` 是工具原始参数 JSON，前端刷新后靠它重建 options；`decision` 给已答方案行标明批准/驳回；前端据 `kind` 渲染） | 400 `111002`（status 不在 pending/answered/stale）；404 `112002`（run 不在） |
| `POST /api/questions/:id/answer` body `{answer,decision?}` | `{questionId,status}`（plan 行：approve 时 answer=编辑后方案全文、未编辑空串；revise 时 answer=修改意见、可空。question 行不接受 decision；plan 行 decision 缺省=approve） | 400 `111002`（decision 不在 approve/revise，或 question 行带了 decision）；404 `115002`（行不存在）；409 `115101`（已答过/失效——CAS 影响 0 行） |
| `POST /api/dev/question-check`（仅 -dev）body `{runId,status?}` | 只读诊断：`{runId,total,rows:[{questionId,kind,status,toolCallId}]}` | — |
| `POST /api/dev/plan-gate-check`（仅 -dev）body `{tool,planMode?}` | `{planMode,deny}`——走真 `isMutatingTool` 与真守卫判定路径（不依赖模型）。`write_file` → `{planMode:true,deny:true}`；`read_file` → `{planMode:true,deny:false}`；显式 `planMode:false` → `{planMode:false,deny:false}` | — |

挂起期间「在等什么」的可见性**不改 `GET /api/runs/:id` 的存储语义**：前端 reconcile 时查 `questions?status=pending`（PG 是真源，刷新/换窗口/换副本都不丢；长挂起不依赖 runlive 的 TTL）。刻意对齐 doc/30「读侧派生、不动存储状态」。

### 5.4 批准/答案的提交形态

不新增消息类端点。**Loop Q**：答案提交走 answer 端点——快捷选项按钮与自由文本框提交的都是 answer 字符串。**Loop P**：方案卡在对话流里以文档态展示，输入区上方的审核面板给三条路（2026-10-23 按 kimi 交互重做）：

| 人的动作 | body | 行状态 | 模型读到 |
|---|---|---|---|
| 批准执行 | `{answer:"",decision:"approve"}` | answered / approve | 「方案已批准，按方案执行。」→ plan mode 退出，写工具放行 |
| 编辑后批准 | `{answer:"<改后全文>",decision:"approve"}` | answered / approve | 批准文本 + 「用户的修改：\n<改后全文>」 |
| 退回修改 | `{answer:"<意见，可空>",decision:"revise"}` | answered / revise | 「方案被驳回，尚未执行；计划模式仍然开启。请根据用户意见修改方案，再调用 submit_plan 重新提交…」+ 意见 |

驳回后模型重新 submit_plan 会落**新行、新帧、新卡**；旧卡显示「已驳回」和当时的意见，档案不丢。resume 对账读同一 `decision` 列：approve 补退出 plan mode，revise 只把驳回文本换回 tool_result（plan mode 保持）。

## 6. Loop 拆分（已按此顺序收口；每拍先交 todos.md，粒度 ≤80 行 diff）

顺序 **Q1→Q2（∥P1）→P2→Q3（∥P3）→Q4→Q5 收口**——Q 先（等人机器是 P 的底座），P 守卫与 P1 可并行；Q1 是一切的前提。每拍「怎么验收」第一行都是打在一个跑着的东西上（硬闸门）。

### Loop Q · ask_user（等人机器底座）

| 拍 | 改什么 | 怎么验收（curl 第一） | 回答什么 |
|---|---|---|---|
| Q1 | `run_questions` 表（含 kind 列）+ list/answer 端点 + `seed-question` dev 探针 + doc/40 码段补行（`15` 段：115002/115101） | `seed-question` 造行 → `answer` 答 → 行 answered；再 `answer` 拿 409 `115101`；`answer` 不存在的 id 拿 404 `115002` | 一答终答的 CAS 成立，码段进表 |
| Q2 | ask_user 工具入池（三拍）+ `kind=question` 帧（词表 8→9，args 带原始参数 JSON 含 options）+ 系统提示词段（含 options 约定）+ 金样 `TestShippedAgentSpecToolGuards` 同步 | 活体：起 run 引导模型中途调 ask_user → 帧见 `kind=question`（name=ask_user、text=问题、args 含 options）、run 仍 running、`questions?status=pending` 见行 → curl `answer` → run complete 且 tool_result 帧含答案；「模型是否会调 ask_user / 给 options」登记 `declared_not_asserted`（同 doc/41 用例 8 先例） | 三拍全回路：落行-发帧-等答-注回 |
| Q3 | 前端问题交互（2026-10-23 按 kimi 重做）：对话流里留「问题档案」（编号选项 + 你的回答）；待答面板挂在输入区上方——编号选项 `→ [n]`、自由文本「其他」、数字键快选、Enter 提交、多条待答可翻页；刷新后从 `questions?status=pending` 重建 | 浏览器：有 options 场景面板渲染编号按钮、数字键选中 / Enter 提交即续跑；无 options 场景文本框提交续跑；刷新页面面板与档案都还在 | 人能答，两种答案形态闭环，且交互对齐 kimi |
| Q4 | resume 重放第三分支（无行/pending/answered 三分支，覆盖两种 kind）+ `scripts/acceptance/question_crash.sh`（对齐 taskboard_crash.sh 先例） | 活体：ask_user 挂起中 `kill -9` → interrupted → resume（202）→ 帧重发 → `answer` → run complete；脚本可重跑判红 | 挂起现场活得过进程死亡 |
| Q5 | 契约收口：词表测试、expected.json 登记、`check-expected.sh`、`go test`+`go vet`+活体全套；`seed-question` 转只读 `question-check` | 两台机器全绿 | 契约钉死 |

### Loop P · plan gate（守卫 + 两工具，叠在等人机器上）

| 拍 | 改什么 | 怎么验收（curl 第一） | 回答什么 |
|---|---|---|---|
| P1 | `runs.plan_mode` 列 + `isMutatingTool` 黑名单纯函数 + planGuardHandler（journal 外 deny 写/执行）+ enter_plan_mode 工具（置位+指引）+ `plan-gate-check` dev 探针 + 提示词「何时进模式」段 + 金样同步（researcher 名单 +enter/submit 两把） | 第一行硬断言（不依赖模型）：`curl -X POST "$TAKO/api/dev/plan-gate-check" -d '{"tool":"write_file"}'` → `deny:true`；`{"tool":"read_file"}` → `deny:false`。活体 smoke：进模式后尝试写 → 帧见拒绝文本 tool_result、runlog 无写入；「模型何时进模式」登记 `declared_not_asserted` | 进模式后写的硬保证成立且不依赖模型行为 |
| P2 | submit_plan 工具（三拍复用等人机器：kind=plan 行 → 帧 → 轮询；decision=approve 注回 + plan_mode 置 false；decision=revise 注回驳回文、模式保持）+ 提示词出方案/改方案段 + 看板工具在计划模式内放行（TaskCreate/TaskUpdate） | 活体：进模式 → 侦察 → 用 TaskCreate 建看板 → submit_plan → 帧见 kind=question(name=submit_plan)、run 挂起、list 见 kind=plan pending → curl `answer {decision:"revise"}` → 模型收到驳回文、模式仍开、改完重新 submit_plan（新行）；再 `answer {decision:"approve"}` → run complete，后续帧见写工具**真执行**（守卫已放行） | 方案-批准/驳回-执行全链路 |
| P3 | 前端方案交互（2026-10-23 按 kimi 重做）：对话流里方案卡用 `PlanDoc` 按文档渲染（标题/列表/表格/代码块/状态 chip）；输入区上方审核面板给**批准 / 编辑后批准 / 退回修改（带意见）**；驳回后旧卡留「已驳回 + 意见」，新方案另起一卡 | 浏览器：submit_plan 挂起出卡；不编辑批准 → 模型按原方案执行；编辑后批准 → 模型按修改版执行；退回修改 → 模型改完重新提交，新卡出现、旧卡标已驳回 | 人能审能批能改能退 |

**脚手架与转正**：`seed-question` 与 `plan-gate-check` 探针曾是脚手架——**已按登记的转正点收口**：`seed-question`（写行）在 Q4 验收后换成只读 `POST /api/dev/question-check`（按 run 列行的 id/kind/status/toolCallId，不再造行）；`plan-gate-check` 本就是只读纯判定，Q5 收口后保留为长期诊断口。黑名单、kind 词表、两段提示词 v1 硬编码，tenant 级可配登记为后续拍，不做进 v1。

**交互层重做（2026-10-23，Q3/P3 的追加拍）**：用户对照 kimi 判「原交互差得远」，按以下清单重做并逐条活体验收——对话流档案（问题卡/方案卡）与输入区上方待答面板分离；问题面板编号选项 + 自由文本 + 数字键/Enter；方案卡文档态渲染（`PlanDoc.vue` 支持 Markdown 表格）；方案面板批准/编辑后批准/退回修改；看板 Todo 面板（标题、完成数、进度条、折叠）挂在输入区上方；驳回→重提的多卡档案。重做中发现并修掉一个真 bug：`reconcile()` 把原始对象塞进 `ref([])`，刷新建出来的 bubble 不经代理，`upsertQuestion`/流式追加都不触发重渲染——`push` 改为返回数组里的响应式元素后，刷新可从 PG 重建待答面板。

## 7. 明确不做（本期）

- **不做 ask_user 的 B 形态**（挂号立即返回、答案走下一条 user 消息）：已拍同轮注回，理由 = 工具契约完整性 + run 连续性（runlog/看板/账目不切碎）+ B 省的钱比看起来少（表/帧/端点/前端卡两家全要）。
- **不做首次写前必审**（方案 B：模型发起第一把写工具时强制问人）：A 形态（模型自主进模式）已拍；B 是将来「模型该进不进造成真损失」时的升级路径，留档不强做。
- **不做用户侧强制进模式入口**（v1 纯模型自主触发；将来可在前端加「要求先出方案」按钮 = 注入一条系统消息引导模型调 enter_plan_mode，零新机制）。
- **不做 exit_plan_mode 无审退出**：进了模式要么 submit_plan 过人审、要么 stopRun；防止「进了模式悄悄退出绕过审核」。
- **不做框架拦截式的工具审批 gate**（模型没问人、框架替它拦）：沙箱围栏已挡越界（doc/33），目录内动作靠 plan gate 的方案审核兜住。ask_user 是模型主动问，不是框架拦。
- **plan mode 内不放行**：写文件族、bash、memory 写、委派、MCP；放行只读类、看板工具（`TaskList`/`TaskGet`/`TaskCreate`/`TaskUpdate`，只改 `run_tasks`）、ask_user、enter/submit（§1 黑名单）。
- **不重建 eino checkpoint / gob 快照**（doc/30 §15 沿用）；interrupt 类错误通道维持预留不启用（tools.go:47 注释不动）。
- **不做方案卡的行级 diff 对照视图**：方案卡在对话流里按文档渲染（已拍），修改轨迹由 mh 上下文承载；不做「原文 vs 修改」双栏对照。
- **不做超时自动拒答清扫**（挂起无上限拍板后此项关闭；stale 清扫仍随 doc/37 轮做 run 终态后的）。
- **不做运行中插话 steer**（doc/16 §5 纪律沿用）；挂起期间会话闸占用是语义正确的（任务半途挂着承重问题，不该同会话并发 competing run）。
- **不做子代理半截重接的变化**（doc/39 语义不动）。
- **tenant 级可配**（黑名单/提示词/kind 词表默认）登记后续拍。

## 8. 开放问题（2026-10-23：1–6 全部已拍）

1. ~~plan gate 适用范围~~ → **A 纯模型自主**（kimi 式：enter_plan_mode 工具 + 运行时守卫 + submit_plan 人审）。显式开关（用户前置判断、UI 负担）与首次写前必审（B，每个干活 run 多一次打断）弃用；「模型该进不进」的残余风险由提示词 + deny 反馈 + stopRun 兜底，真出现损失再加 B（留档 §7）。
2. ~~挂起时长上限~~ → **无上限**、lease 照续、stopRun 可打断。
3. ~~问题卡答案形态~~ → **做 options**：ask_user 加 `options?: string[]`，前端快捷按钮 + 自由文本框，question 帧 args 承载（§3.3）。
4. ~~批准消息~~ → **方案审批三态**：批准 / 编辑后批准 / 退回修改；`run_questions.decision` 承载 approve/revise，编辑后全文走 answer（§5.4）。
5. ~~Loop 顺序~~ → **Q 先 P 后**（P 复用 Q 的等人机器）；Q2 ∥ P1、Q3 ∥ P3 可并行。
6. ~~驳回语义~~ → **revise 保持 plan mode**：驳回只把意见注回、不清守卫，模型改完重新 submit_plan；看板工具（TaskCreate/TaskUpdate）在计划模式内放行，让方案阶段就能把步骤立成 todo（2026-10-23 交互重做时拍）。

## 9. 所以这解决了什么

v6 第一次把「人」建模为系统内的**外部输入源**：动手前有方案审核（plan gate，模型自主触发、运行时守卫硬保证、方案可批准/修改后批准/退回重做），干到一半有结构化追问（ask_user，现场在库、等待者可重建、options 快捷答法 + 自由文本双形态）。病灶一里小张能在 `rm` 发生前改掉「旧文件」的定义；病灶二里模型承重式的歧义有正式的提问通道，答案按工具契约注回同一轮。机制上没引入任何新哲学：P 是 kimi plan mode 在 v6 机器上的移植（等待复用 Q 的轮询而非进程内 waitUntil），Q 把 doc/41 已付完设计债的 Loop A 换名施工——P/Q 共用一张表、一种帧、一套 resume 分支。落地已回改 doc/16 #8 行与 §2c（新增「等人现场落库」横切契约）；施工按 §6 拆 todos.md 逐拍收口。
