# 19v6-10 · mid-run fold（run 中途压缩 · 立项方案）

> **立项：2026-10-11（用户裁「现在就要立项…我是让你出方案的」）**。本篇取代原需求登记，
> 把立项要裁的三件事 + 三件套不变量闸门全部裁掉；19v6-07 §5 的同名条目以本篇为准。
> 施工前的事实依据 = runengine 压缩链全量实读（worker/compact/pgstore + eino v0.9.21
> 钩子面），行号级证据在施工台账。
> 2026-10-11 自查修订：潜伏边由不可达变为正常态（初修 = 挂点退让；同日用户裁改为
> **同槽换代 UPSERT**——mh 本有 id 主键，撞槽即换代、无退让无兜底）；user 行钉死与
> 后缀预算含 user 行（恒有解推导）；三处事实措辞修正。
> 2026-10-11 复审修订（证据 = eino v0.9.21 实源码 adk/handler.go、wrappers.go、
> chatmodel.go、react.go 逐行核）：**system 行进折叠公式**——state.Messages[0] 恒是
> ADK 种入的 system 消息，§8 补它的切分/重写/回写三处位置；**传递机制点名**——staged /
> 标签 / 折后 state 走 run 级容器（比照 agentModelTable 的 ctx 携带先例），折后 state
> 的取货口 = AfterAgent 钩子；**新裁定**——keep 区活摘要 run 的翻转粒度（§10，复审
> 发现的静毒洞）；§4 After 钩子机制纠错；§0 活摘要不变量改述；§9 errStillOver 可达
> 路径收窄；连续 user 行、挂载顺序、重试不重复触发登记入册。

## §0 地基：五个词的真身

**Run** = 用户发一条消息到引擎给出终稿的一次执行。run 起手时引擎往 PG 抢闸
（`runs` 表部分唯一索引，`running` 行 = 会话同时只允许一个 run），并把这条 user 消息
写进 `run_model_messages` 表（简称 **mh**），恒 seq=1。run 进行中 mh 里这个 run **只有
这一行**——后面每轮模型产出在引擎内存累积，要等 run 结束（EndRun）才落库。

**mh 行长什么样**：一次带工具调用的 run 落库后共 4 行——

| seq | role | text | tool_calls 列 | tool_call_id 列 |
|---|---|---|---|---|
| 1 | user | 用户消息 | NULL | NULL |
| 2 | assistant | 本轮正文（可空） | 模型原生 JSON：`[{"id":"call_abc",…,"function":{…}}]` | NULL |
| 3 | tool | 工具结果全文 | NULL | `call_abc`（字面对上上一行某条 id） |
| 4 | assistant | 终稿正文 | NULL | NULL |

多次工具调用 = 第 2、3 行重复几轮再接终稿行。provider 铁律：**tool 行必须紧跟发起它的
assistant(tool_calls) 行**，中间插任何消息都拒收——本篇 §7 的一切都从这一行字展开。

**compacted 标记**：每行一个布尔。`false` = 模型看得见；`true` = 已被压缩、只留档不进
输入。热查询只读 `compacted=false` 的行，按 `(run_id, seq)` 排（run_id 是 ULID，字典序
即时序）。

**seq=0 摘要行**：压缩的产物。把被压段的原文行翻成 `compacted=true`，调一次 summarize
产出**一份**摘要，作为 `seq=0`、`role=user` 的新行挂到「被压段里时间最晚的那个 run」
上；那个 run 的 0 号槽若已被占（mid-run 时代的正常态，见 §5），写入走**同槽换代**
（UPSERT）而非撞车。摘要数量的准确不变量 = **每次折叠事件把当时可见的旧摘要全部
并入新摘要、旧行翻死留档**（换代点除外，见 §5 代价登记）；两个折叠事件之间，库里
可以并存分属不同 run 的多份活摘要——mid-run 时代每个被折过的 run 都带一份摘要 +
保留轮次（§5），它们概括不同时段、内容不重复，下一次折叠经 oldSummaryRuns 机制
一并收编（「此前摘要，请合并进新的摘要」）。

**countInput**：压缩的长度自测——`system 提示词 + 每行文本` 过 cl100k 分词器求和，
跟 `limit = window×4/5` 比。触发线、POST 准入线、摘要预算帽三处共用这一个 limit。
**现口径的洞**：只数 `text` 列，`tool_calls` 的 JSON 一个字不计——而工具参数才是工具环
时代的真实体积大头（`write_file` 一次调用可以把整份文件内容塞进参数）。本篇 §6 补。

**Run fold（现役）**：下刀点只有一处——**run 起手前**（prepareInput 发送前自测）。
那一刻当前 run 在 mh 里只有 user 一行，所以折叠对象 = 全部已完成 run；keep = 最近 n 个
完整 run（n 从 20 降档），drop = 更老的；foldInput 是纯函数不落库，产出
`stagedCompaction{DropRunIDs, Summary, AttachRun}` 暂存内存，run 结束（EndRun）时与
放闸、定稿**同一笔 PG 事务**落库：被折行 `UPDATE compacted=true` + `INSERT seq=0 摘要行`。
error / 闸失 / 进程死 = 零写入，下轮从原文重压。

## §1 病灶：run 中途为什么会顶穿窗口

今天 F 系列文件工具已落地，一个真实场景：用户说「把这三个日志文件读完，写一份汇总
报告」。

1. run 起手前自测通过（历史不长，远低于 limit）；
2. 模型进 ReAct 工具环：第一轮 `read_file` 读 a.log（200KB ≈ 5 万 token），结果**灌进
   上下文**；再读 b.log、c.log，上下文又涨十几万 token；
3. 中途某次模型调用前，输入顶穿窗口 → provider 以「输入太长」打回 → run 走 error
   结局 → 用户重投 → 自测还是通过 → 模型做出一模一样的调用、读一模一样的文件、死在
   同一轮。**会话事实上卡死在这条消息上。**

注意病灶的准确形状：**run 起手前确实压过一次（Run fold），问题不在那一次没压，而在
ReAct 中途读文件涨出来的部分根本没被那一次覆盖**——起手前压，压不到起手后。Run fold
够不着它有两个结构性原因：

- **下刀点在前**：fold 的时机是 run 起手前，超窗的增长发生在 run 中间，下刀时那部分
  还不存在；
- **更根本的：run 中途的行不在 mh 里**。assistant/工具结果轮次在引擎内存累积，EndRun
  才落库。Run fold 的原料是 mh 表，它对 run 中途的增长**连看都看不见**，更别说压。

同病灶还有两个兄弟方案（19v6-07 §5 登记）：**溢出修复层**（19v6-09：provider 报错后
强制压缩、只重试一次）与 **trimToolResults**（压缩后仍超则砍工具结果体积）。三者关系
在 §9 一并裁。

**所以这解决了什么**：把下刀点从「run 起手前」扩到「run 进行中每轮模型调用前」，
让「读大文件的长 run」不再死在中途——这是 mid-run fold 存在的全部理由。

## §2 端到端链路：方案落在哪一环

今天的链路（`▼` = 本篇方案插入处）：

```
用户 POST → 准入（单条消息 ≤ limit/3）→ 抢闸（PG 唯一索引）
  → goroutine Run：
      prepareInput：读 mh 可见行 → countInput → 超线？→ Run fold（staged 暂存）
      ▼
      ADK 主循环（ReAct 工具环）：
          ┌─ 每轮模型调用前：BeforeModelRewriteState 钩子        ◄── 方案落点
          │    （本篇：自写 middleware 在此数长度、超线就折 state）
          ├─ 模型调用（流式 delta 帧）
          └─ 工具执行（tool_call/tool_result 帧；结果回注 state）
      → EndRun（唯一 PG 写点）：放闸 + 定稿 + staged 落库 + mh 行 + runlog 行
          （commit 后 Redis 半：terminal 帧 + 删 draft）
```

钩子是 eino ADK 的 `BeforeModelRewriteState`：**run 内每一次模型调用前**触发，入参是
本轮的 state（含全部消息列表），改动会持久进后续迭代。方案的机械结构一句话：**middleware
在钩子里对 state 数长度，超线就调 v6 自己的折叠链把 state 里的旧消息折成一份摘要，
模型下一轮看到的就是「摘要 + 最近若干轮原文」**；PG 一个字不动，run 结束后由 EndRun
把折后的世界照常住库。

链路每一环的四问：触发者 = middleware（每轮模型调用前）；输入 = state 消息列表 +
「哪段是历史、哪段是当前 run 轮次」标签（run 级容器携带，机制见 §8）；产出 = 改后
的 state（消费者 = 本轮模型调用）；不动的 = mh 表、runlog、闸、帧（四端点字节零变化）。

## §3 裁定一览

| # | 决策点 | 裁定 | 一句话理由 |
|---|---|---|---|
| D1 | 触发面 | 自写 middleware 挂 `BeforeModelRewriteState`；**每轮全量重数**，超线才折 | 成本被窗口钉死（≤1s vs 模型调用数秒起）；零漂、单代码路径 |
| D2 | 落库 | **没有第二写点**：只改内存 state；PG 写点仍是 EndRun 一处（staged 列表：只写最后一份摘要 + DropRunIDs 并集翻转）；mh 写入源改为折后 state；runlog 照写全量 turn | 闸串行化 + 无人中途读 mh ⇒ 中途落库无消费者；零写入家族原样保住 |
| D3 | 计数 | 基数 = text + tool_calls JSON（Run fold 与 mid-run **统一**）；工具 schema 不计（登记） | 现口径对工具参数全盲，工具环时代先瞎在这 |
| 三件套① | 下刀粒度 | 老 run 整 run 下刀（路线 A）；当前 run 内 user 行钉死 + 按「交换单位」后缀保留 | 最小引入消息粒度——B 是被本篇用例逼出的最小形 |
| 三件套② | role 回扩 | 保留的第一个交换单位不允许以 tool 行开头；候选切点回退到发起它的 assistant(tc) 行 | provider 铁律，违反 = 下一次调用 400 |
| 三件套③ | 在途豁免 | 后缀保留天然罩住最新在途交换；保留一条断言防将来改保留策略 | drop 只发生在前缀，最新交换永远在被保留的后缀里 |
| 挂点 | 摘要挂哪 | 规则不变 = 被折段时序最后的 run；槽被占则**同槽换代**（UPSERT） | 「同 run 活摘要 + 可见原文」在 mid-run 后是正常态；撞槽的物理含义是换代、不是换 run（§5/§10） |
| 失败出口 | mid-run 压不进 | errStillOver = 干净 A：run error、零写入 | 与 C2 已定出口同形；卡死逃生归兄弟层 |

下面每节给完整推理和「没裁会怎样」。

## §4 D1 触发面：挂哪、什么信号、数多少

**挂哪**：自写 middleware，挂主 agent 的 `BeforeModelRewriteState`，排在 agentlog
之前（钩子按 Handlers 注册顺序逐个调用——wrappers.go `for _, handler := range
w.handlers`；agentlog 只观测不改，顺序不动行为，动的是观测口径：compact 在前，日志
看到的就是模型实际所见的折后输入）。不抄 eino 内置 summarization middleware——§12
的四条否决理由在本篇语境全部仍然成立，且它只钉 system 前缀、对话尾部无保留区，会把
「刚拿到、模型还没看」的工具结果一起压掉，与本方案的后缀保留根本对立。

**什么信号**：每轮模型调用前，对当前 state 全量 countInput（§6 基数），`> limit`
才进折叠；折完必 `< limit`（折叠定案条件本身），所以**不存在「压后仍超、每轮重复重压」
的活锁**——这是 n 降档 + errStillOver 已覆盖的家族语义，只是触发点挪进 run 内。

**数多少（全量 vs 增量）**：裁**每轮全量重数**。账：窗口把历史钉死在 ≤256k token
（约 1MB 文本），cl100k 全量编码一遍 ≤1s；而钩子的服务对象是每次**数秒起**的模型
调用——占比可忽略。增量扣减要维护「累计值 + 每次追加的记账点 + 折叠后的重基」三套
记账，多一条计数代码路径就多一种和全量数对不上账的漂法；而折叠链内部本来就在反复做
组装后计数（n 降档每档一次）。**没裁会怎样的对称面**：若裁增量，省下的是每轮一次
亚秒级纯 CPU，换来的是漂账风险——不值得。

**没裁触发面的具体事故**：如果只挂 prepareInput（= 现状），§1 的故事就是终局；
如果挂别的钩子（AfterModelRewriteState）——它的返回 state 同样持久进后续迭代
（wrappers.go：After 循环后 `ProcessState` 写回），所以「白折」不是它的死因；真正的
死因是**模型调用报错时 After 钩子根本不执行**（wrappers.go：调用 err 直接 return，
After 循环被跳过）——超窗场景恰恰是这次调用会被 provider 拒收，After 下刀永远晚于
死期。Before 是唯一能在「这次调用发出前」动 state 的位点。顺带登记：**模型重试不
重复触发钩子**（retry_chatmodel.go：retry 不再跑 state 管理钩子），不存在「重试叠
一次折叠」。

## §5 D2 落库：为什么没有第二写点 + 挂点同槽换代

原登记担心「mid-run 下刀要在 run 中途落 compacted 态，第二个写点的事务边界与回滚
语义全部重推」。实读代码后这个前提**不成立**：run 中途落库没有任何消费者——

- 闸在 PG 唯一索引上：run 活着期间，别的副本对这个会话的一切写入 409；没有任何 API
  在 run 中途读 mh（frames 读 Redis，log 读 runlog，session 读 runs 表）；
- 进程死（kill -9）：turn 本来就蒸发，折叠跟着一起蒸发——mh 原样，下轮 Run fold 从
  原文重压，零写入家族原样成立；
- error 结局：staged 被 complete 分支条件丢弃（现状如此），mid-run 折过的痕迹全在
  内存，随 run 死光。「error 只留 user 半」的可见集语义不动。

所以裁定：**mid-run fold 只改内存 state，PG 仍只有 EndRun 一个写点**。但这带来三个
连带改动，全是本篇施工面：

1. **mh 写入源从 turn 换成折后 state**：今天 EndRun 按内存 `turn` 逐轮落 mh；mid-run
   fold 之后 state（模型视图）比 turn（全量审计视图）短——被折掉的轮次**从未落库**，
   没有洞、没有补删。若仍按 turn 写，等于把折掉的又写回去，折叠白做。写入时跳过
   当前 run 的 user 行（seq=1 已由 AcquireRun 落库，重写会撞 UNIQUE）。
2. **runlog 照写全量 turn**：展示历史一条不缺（用户回看时中间几轮工具调用都在），
   被折的只是模型的输入视图。两表投影分离（runlog 记产出、mh 记模型该看什么）的
   既有原则原样延伸。
3. **staged 从单个变列表**：一轮 run 可能先经历 Run fold（起手前）再经历若干次
   mid-run fold。每次折叠的 renderFold 都拿 state 里的上一份摘要当原料并掉，所以
   **只落最后一份摘要**（其内容已包含前序各份）+ 各次 DropRunIDs 的并集翻转
   （`UPDATE … WHERE compacted=false` 幂等，天然防半写；keep 区旧摘要 run 只翻
   seq=0 行的粒度细化见 §10）。挂点 = 最后那次的 AttachRun。

**挂点与同槽换代**（消息粒度的挂点重推，三件套①的推理核；自查发现、用户裁机制）：

挂点规则**不变**——仍挂「被折段时序最后的 run」（19v6-07 §2 原规则）。变的是对撞槽的
处理。Run fold 时代挂得上摘要的 run 必然没有 seq=0 行（它若被挂过，原文行当时就全翻
死、永不复见），所以 INSERT 从不撞车；mid-run fold 把摘要挂在当前 run 而当前 run
**保留可见轮次**——「同一 run 同时有活摘要与可见原文行」从潜伏边（19v6-07 §5：手工
INSERT 才造得出）**变成每个被 mid-run 折过的 run 的正常形态**，这种 run 再被整体折入
时就是挂点，旧 INSERT 会撞 `UNIQUE(run_id, seq)`（死行不释放槽位）→ EndRun 整笔回滚、
run 白跑。

处理：**摘要写入从 INSERT 改为 UPSERT**：

```sql
INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
VALUES (?, ?, ?, 0, 'user', ?, false, now())
ON CONFLICT (run_id, seq) DO UPDATE SET text = EXCLUDED.text, compacted = false
```

撞槽的物理含义不是「换个 run 挂」，是**「这个 run 的摘要槽要换代」**——旧摘要的内容
已并入新摘要（renderFold 重压合并保证），槽位原样、内容焕新；`id` 不动（行还是那行，
§13 验收断言 id 不变即据此）。mh 行本来就有 id 主键（`msg_<ULID>`）：不需要加列、
不需要占用查询、不需要退让或兜底。`UNIQUE(run_id, seq)` 继续做机器守卫——**绝不能撤**：
撤了撞槽就从响亮错误变成「同 run 两份 seq=0」的静默腐败；保留它 + UPSERT = 约束当
守卫、冲突即换代。

代价登记：换代点的旧摘要**逐字稿**被覆盖（死摘要留档原则在换代点让位）；其内容不丢
（已并入新摘要），丢的只是上一代摘要的原文。纯 Run fold 路径下挂点槽恒空，
UPSERT ≡ INSERT——既有行为逐字节不变。

**没裁这条的具体事故**：若坚持 run 中途落库，就得回答「中途这笔事务和闸/租约什么
关系、error 后回滚不回滚、kill -9 留下半截压缩态怎么办」——三个问题今天的答案全部
是「不存在」，因为唯一写点设计本来就避开了它们；为不存在的消费者建写点，纯负债。
若不做同槽换代，第二个被折季就是 UNIQUE 回滚季（上面的事故）。

## §6 D3 计数：基数、频率、schema

**基数**：`system + Σ(每行/每条消息 text) + Σ(tool_calls JSON 序列化文本)`。
tool_calls 进基数是**补洞**：今天 `write_file` 把整份文件内容写进参数，计数器全盲——
工具环时代先瞎在这。Run fold 与 mid-run fold **共用同一基数同一函数**，不存在两套
长度。**代价登记**：工具密集会话的触发线会比现在灵敏（以前少算了参数），dev 旗标
时代校准，属修正而非回归。

**schema 不计**：厂商每次调用重发的工具声明清单不进触发。v6 当前目录小（11 个文件
工具 + 少量基础工具），计入只是把 limit 平移；WeKnora 战例（232 个 MCP schema ≈ 10 万
token）那是目录大潮时的重裁项，随 MCP 工具 loop 立项一并复议（19v6-07 §1 复审项原文
挂着，本条不提前消费）。

**频率**：每轮全量（§4 的账）。实现可带「state 未变则跳过」短路，属优化不改裁定。

**没裁基数的事故**：`write_file` 这类把整份内容写进参数的调用在计数器里隐形
（`read_file` 的大头在结果、走 text 列，今天照数；参数侧才是真盲区），直到 provider
以「太长」拒收才现身——正是 19v6-09 溢出修复层要接的报错，本篇把它从触发面就堵住一半。

## §7 三件套：「不拆配对」今天为什么免费、mid-run 怎么把它守住

**今天的免费午餐**：「assistant(tool_calls) 行与其 tool 结果行相邻、同 run_id」
不靠任何检查——折叠单位是整个 run，run 边界天然就是工具往返的完整单位，切整个 run
永远切不坏配对；落库靠 EndRun 按轮序同一笔事务写，读回按同序重建。mid-run 在 run
中间下刀，亲手把这条免费保证撕掉，三个检查补上：

**① 交换单位 = 下刀的最小单位**。把当前 run 的轮次切成不可拆的单位：
`assistant(tool_calls) + 它名下的全部 tool 行`（一次工具往返）/ `assistant 终稿行`。
**user 行不是单位、永不被折**——它是本轮问题的本体，折了模型不知道在答什么；user 行
恒在保留区首位（落库形态 = seq=0 摘要、seq=1 user、seq≥2 保留轮次，与 Run fold 的
老 run 形态同构）。老 run 仍整 run 切（路线 A 精神）；当前 run 内只允许切在交换单位
边界上，keep = user 行 + 最新若干交换单位的后缀，drop = 更老的前缀。这样 pairing 的
半边（drop 区里的 tool 行，其发起 assistant 比它更早，必同在被折前缀里）天然闭合，
剩另一半由 ② 守。

**② role 回扩**：保留的第一个交换单位不允许以 tool 行开头。若按 token 预算找切点时
落在了「assistant(tc) 已折、它的 tool 行要留」的位置，必须把保留区**回退**到发起它的
assistant(tc) 行（连带其 tool 行一起留）。没它 = 下一次模型调用 provider 直接 400，
run 以 error 收场——而且这个错是折叠自造的，原文毫无问题。

**③ 在途交换豁免**：最新一轮 assistant(tool_calls) 与其还没回结果的工具调用，压不得
——压掉发起方，结果回来即成孤儿。在后缀保留设计下**天然成立**：drop 只发生在前缀，
最新单位永远在被保留的后缀里；钩子的时机（每次模型调用前）又保证 state 尾部只可能是
两种完整形态：起手第一钩 = user 行，其后每钩 = 一组已回齐的工具结果。保留一条断言
（「保留后缀不得切断任何未完成交换」）防将来有人把保留策略改成非后缀（如「保留关键
轮」）。今天同步工具下它是摆设，异步工具 / HITL 行插入立项那天它变成真闸门——这正是
登记时把它列进三件套的前瞻价值。

**三件套同拍的理由**：三者守的是同一条 provider 铁律的三个破口，缺任何一个，另两个
的防护面就出现一个能穿进 400 的角。一次施工一起落，不留「下轮再补」的窗口。

## §8 折叠算法本身：两个 fold 的对照、共享与差异

**共享的（同一函数同一语义，一行不改）**——两个 fold 共用 C 系列已验收的整条链：

- **计数**：countInput（cl100k）与同一个 limit（window×4/5，触发线/准入/摘要帽三处
  共用）；D3 的基数扩展（text + tool_calls JSON）写进这一个函数，两个 fold 同步生效。
  mid-run 侧的原料是 `[]*schema.Message`：**转换层先行**（切出 system 首条、其余行转
  mh 行形状，见下「原料转换层」），再过同一个 countInput——函数签名不动，system 走
  它已有的 system 形参。
- **转录**：renderFold 四形态（`[User]` / `[Assistant]` / `[Assistant tool calls]` /
  `[Tool result <name>]`）+ 分级截断（工具结果 2000 / 参数每 value 400 字符）。
- **重压合并**：旧摘要并入（「此前摘要，请合并进新的摘要」）、**每次折叠把可见旧摘要
  全并全翻死**（准确不变量，§0；事件之间多份并存是正常态）、二级压缩不单做。
- **摘要调用**：同一 ChatStream 直连（不经 ADK ⇒ 钩子里调它不递归触发钩子）、低温 0.3、
  stall 超时、按失败原因分流的重试、三层验货（算术主检 + finish_reason 辅检 + 缺失处理）、
  结构化六槽 + 预算帽 `S ≤ min(4096, limit/2)`。
- **fileOps 钉块**：提取 / 继承 / 钉块 / 三档收口——mid-run 里被折的是当前 run 的
  早段轮次，提取原料是同一批 tool_calls（write_file 路径不能随被折轮次消失）。
- **失败出口**：errStillOver 哨兵 + 诊断参数格式 + 干净 A（error、零写入）。
- **落库**：foldInput 纯函数出 staged → EndRun 唯一写点；放闸+定稿+压缩写同一笔事务；
  幂等翻转（`UPDATE … WHERE compacted=false`）；`UNIQUE(run_id, seq)` 做机器守卫。
- **展示面**：不动 runlog，四端点字节零变化（折叠不出帧）。

**差异（逐项对照）**：

| 维度 | Run fold（run 起手前） | mid-run fold（run 中途） |
|---|---|---|
| 触发点 | prepareInput，每 run 一次 | `BeforeModelRewriteState`，每轮模型调用前一次 |
| 原料 | mh 可见行（全在库里） | ADK 内存 state（当前 run 轮次未落库；「历史/当前 run」标签由 run 级容器携带，见下「worker 侧记账与传递机制」） |
| 可压对象 | 已完成的老 run（下刀时当前 run 只有 user 一行，本来就压不到它） | 老 run + **当前 run 的早段轮次**——这才是本篇要的那部分 |
| 下刀粒度 | 整 run（run 边界 = 工具往返完整单位，「不拆配对」免费） | 老 run 整 run + 当前 run 内「交换单位」（消息粒度，三件套守门） |
| keep 形态 | 最近 n 个完整 run（n 从 20 降档） | 先 n 个老 run；仍超则当前 run「user 行钉死 + 交换单位后缀」（后缀预算 limit/3，user 行算进这份预算） |
| 当前 run 行 | 原样跟尾、永不进 keep/drop | user 行钉死；早段轮次可进 drop；被折轮次**永不落库** |
| 挂点 | 被折段时序最后的老 run（槽恒空，UPSERT ≡ INSERT） | 规则不变 = 时序最后的 run；槽被占 = 同槽换代（§5） |
| staged 形态 | 单个（prepareInput 最多一次） | 列表（Run fold + 若干次 mid-run fold；只落最后一份摘要 + DropRunIDs 并集翻转） |
| EndRun 的 mh 写入源 | turn（折老 run 不动当前 run，turn ≡ state） | 折后 state（折了当前 run 轮次，turn ≠ state；runlog 仍写全量 turn，展示不塌） |
| 卡死形态 | 起手前塞不下 → 干净 A，重投继续卡直到人工/换窗 | 中途塞不下 → 干净 A，重投可能重演；逃生归 19v6-09 / trimToolResults |

一句话：**共享的是「怎么压」（转录/摘要/验货/钉块/落库纪律），差异的是「压谁的哪一段、
什么时候看、以及折后怎么回写」**。mid-run fold 没引入第二种压缩，是把同一条链的原料口
从「库里的旧 run」扩到「内存里的当前 run 轮次」。

---

**新写**（共享清单之外的全部增量）：

- **system 行钉首**（复审补的承重事实）：`state.Messages[0]` 恒是 ADK 种入的 system
  消息（chatmodel.go `defaultGenModelInput` 把它 prepend 进输入、react.go init 节点
  原样种进 state），整个 run 期间都在。它对折叠链三处可见：**切分**时它不进交换单位、
  不进 drop；**重写**时它原样留队首；**回写**时它不落 mh（mh 没有 system 行的位置，
  instruction 每 run 由 ADK 重建）。**没这条的具体事故**：按旧公式
  `state.Messages = [摘要] + [保留区]` 逐字实现，system 行被静默丢掉——从折叠那一轮
  起，这个 run 的所有后续模型调用都不带 instruction；调用照常 200、run 照常
  complete，只是 agent 被摘了脑。不炸的 bug，正是最要防的那种。
- **原料转换层**：state 切出 system 首条（其文本走 countInput 的 system 形参）后，其余
  `[]*schema.Message` 机械转成 mh 行形状（RunID 按「历史/当前 run」标签填，tool 行的
  名字经 tool_call_id 对回 tc 条目），然后喂进同一条 renderFold / foldInput 链——
  转换是唯一的新代码，链本身不动。
- **切点选择**：下刀顺序固定——先老 run（整 run，最老先折，n 从 20 降档，同 Run fold
  语义；**首档即「只折老 run、当前 run 全留」**，多数触发场景折几个老 run 就下去，
  够不着当前 run）；老 run 全折仍超，再进当前 run：keep = user 行 + 交换单位后缀，
  **后缀预算 = C5 的 tail 准入 `limit/3`，且 user 行算进这份预算**（A1 已保证单条
  user ≤ limit/3，所以预算恒非负；`S ≤ min(4096, limit/2)` + 后缀 ≤ limit/3 与 C5
  同式恒有解 5/6·limit），收缩步长 = 一次吐一个最老单位，每档一次 summarize + 组装
  计数，封顶约 20 次尝试，耗尽即 errStillOver（可达路径见 §9）。
- **state 重写**：定案后 `state.Messages = [system 行原样] + [摘要(user 角色)] +
  [保留区]`，保留区恒含当前 run 的 user 行。
- **worker 侧记账与传递机制**：run 级容器（比照 agentModelTable 的在职先例——
  worker.Run 建、塞 ctx、middleware 读写、worker 读回，RWMutex 兜底并发）装三样：
  staged 列表（替代今天单个 stagedCompaction）、「历史/当前 run」标签（prepareInput
  时填入，middleware 每次重写 state 后同步维护）、折后 state 快照。**折后 state 的
  取货口 = AfterAgent 钩子**：它在成功终态触发、入参即含全部消息的最终 state
  （handler.go："the final conversation state"），middleware 在此把它存进容器；
  error / 取消 / 超迭代时 AfterAgent 不触发、容器为空、EndRun 自然零写入——干净 A
  与钩子语义严丝合缝。eino 的 SetRunLocalValue 不可用：它只能在 middleware 执行
  上下文内读写，EndRun 在 agent.Run 之外够不到。EndRun 的 mh 写入源切到容器里的折后
  state：过滤 system 行、跳过已在库的 user 行（seq=1），保留轮次 seq 从 2 连续排；
  终轮 assistant 取 state 末条（runlog 终稿仍取 draft——两投影分离不动）。

## §9 失败出口与兄弟层关系

**mid-run errStillOver**：先收窄它的可达路径——按 §8 的恒有解算术（S ≤ limit/2 +
后缀含 user 行 ≤ limit/3 ⇒ 合计 ≤ 5/6·limit < limit），「老 run 全折 + 只剩 user 行
仍超」在既有守恒体内**算术不可达**；实际可达路径 = fileOps 钉块三档收口全失败
（C5-A7 既有分支原样复用）。出口形态不变：走 C2 已定的干净
A——run `status=error`、零写入、frames 只有 start+terminal{error}。理由：此刻即使硬发
模型调用也是 provider 拒收的同义词，早死干净；零写入让重投从原文重来，Run fold 至少能
折掉老 run 腾地方。诚实登记：重投可能重演同一中途超窗（§1 的卡死形态不变完全体），
**真正的卡死逃生靠兄弟层**——本篇不越俎：

- **溢出修复层（19v6-09）**：provider 报「太长」后的强制压缩 + 一次重试，是撞墙后的
  兜底；
- **trimToolResults**（19v6-07 §5）：压缩已发生仍超时的工具结果瘦身（总预算 window/5，
  还原文本从最新往回），是「既不 error 也能发出去」的最后手段。

三者一并裁定的关系：**mid-run fold 是撞墙前的主动下刀（本篇），溢出修复层是撞墙后的
兜底下刀，trimToolResults 是两者都失败后的瘦身出口**。本篇施工期间若实测出现卡死会话，
按此顺序找责任人，不在本篇里塞后两层的料。

**摘要调用失败**（重试耗尽 / 验货拒收 / stall）：同样干净 A，失败分类进引擎日志
（C2 既有分流，不重裁）。

## §10 捎带裁定

- **foldInput 潜伏边 → 本篇转正为正常态并已裁**（19v6-07 §5 登记；2026-10-11 自查发现、
  用户裁机制 = 同槽换代）：「同一 run 同时有活 seq=0 摘要行与可见原文行」在 Run fold
  时代生产不可达（摘要只挂在被整体折死的 run 上）；mid-run fold 把摘要挂在当前 run 而
  当前 run 保留可见轮次，该状态成为**每个被 mid-run 折过的 run 的正常形态**。它的事故
  形态 = 后续折叠事件把这个 run 选作挂点 → `INSERT seq=0` 撞 `UNIQUE(run_id, seq)` →
  EndRun 整笔回滚、run 白跑。本篇用**同槽换代**裁掉（§5：摘要写入改 UPSERT，撞槽 =
  换代非换 run）。登记从「待裁」改为「已裁」。
- **路线 A/B 兑现**（19v6-07 §5「倾向先 A 后 B，B 留待用例逼出」）：B 已被本篇用例
  逼出，裁其**最小形**——老 run 保持整 run 粒度（A），消息粒度只引进当前 run 内部。
  将来若用例逼出「跨 run 消息级 keep」（如保留某轮关键工具往返多年），再扩。
- **子代理内部不压**：compact middleware 只挂主 agent。子代理状态进程内即弃、回传
  有 50KB 裁剪（19v6-05 §4），压它没有消费者；也避免子代理循环里多一条 summarize
  链。主 run 视角里一次子代理调用 = 一次普通工具往返，随主 run 的折叠一起走。
- **POST 准入（A1）不动**：单条消息 ≤ limit/3 的 413 在抢闸前，与本篇正交——且它是
  「user 行 ≤ limit/3」的机器保证，后缀预算含 user 行的恒有解推导（§8）站在它上面。
- **keep 区活摘要 run 的翻转粒度**（复审新裁）：「seq=0 必是该 run 唯一可见行」一死，
  既有链的一个隐藏前提就暴露了——foldInput 把 oldSummaryRuns **无条件**并进
  DropRunIDs，EndRun 按 run_id 把可见行整 run 翻死（`UPDATE … WHERE run_id IN …`）。
  Run fold 时代这无害（这种 run 除摘要外没有别的可见行）；mid-run 时代，一个被折过的
  run 若落在下一次折叠的 keep 区，它的**保留轮次**会被一并翻死——本轮模型刚看过这些
  行、下一轮库里没了，静默丢失、无报错。裁定：**翻转粒度细化**——DropRunIDs 拆两半：
  drop 区的 run 照旧整 run 翻死；keep 区的旧摘要 run 只翻其 seq=0 行（其内容已并入
  新摘要，保留行留活、与本 run 模型所见一致）。被否的替代：带活摘要的 run 强制进
  drop——翻转不用改，但「keep = 最近 n 个 run」被摘要史污染（近期 run 只因被折过就
  永远留不住原文尾段），与最小下刀相悖。
- **连续 user 行登记**：折后 state 形状 = `[system, 摘要(user), user 行, …]`，第 2、3
  位相邻同角色。OpenAI / Claude 均接受连续 user 消息（合并语义），现役两家扩展照走；
  §13 话本在真实 provider 下跑即覆盖。将来若接入拒绝相邻同角色的 provider，备选 =
  摘要行 role 换 system 或并入 user 行——登记为已知依赖，不占本篇施工面。

## §11 反向登记：WeKnora 三重防反复闸为什么不需要（维持原裁）

WeKnora 的 ErrNothingToCompact / compactionExhaustedAt / minFreedFraction 三重闸，
前提是「摘要活在 turn 内存、被压历史跨轮还在」——会反复触发同段压缩。v6 里被压行
落库即不可见、每次重压的原文集合自然收缩（新 run + 一份活摘要），不存在同段反复触发；
n 降档 + errStillOver 已覆盖。mid-run fold 落地不改变这个判断：fold 后的 state 同样
落库即不可见。登记维持，防将来拿 WeKnora 对拍时把不需要的抄回来。

## §12 明确不做

- 不抄 eino 内置 summarization middleware（四条否决：只钉 system 前缀、**对话尾部无
  保留区**——刚回的工具结果随被压段一起进摘要 / 无终止防线 / chars/4 对中文低估
  3–4 倍 / 纯内存态与 mh 落库冲突——19v6-07 §5 原文在案；v0.9.21 实源码复核四条全部
  成立：其 finalizer 确实是 `append(systemMsgs, 摘要)` 一把换掉全部对话，默认计数就是
  chars/4 估算）。
- 不动 19v6-07 的既有裁定（触发线 ×4/5、结构化摘要、fileOps 块、干净 A 出口）。
- 不在本篇塞溢出修复层 / trimToolResults 的料（§9 的关系裁定即可）。
- 子代理内部压缩、异步工具 / HITL 的折叠语义：未立项，出现时另裁（三件套③的断言
  已给那时的施工留了闸门位置）。

## §13 验收口径（施工期 curl 话本，供拆 todos 用）

- **压缩发生**：dev 小窗旗标（`-compact-window` 逼出）+ 文件工具长 run；引擎日志出
  「mid-run 输入 N tok > limit M，进入折叠」→「压 k 单位（含当前 run m 轮），摘要 S tok」
  → 模型继续、run 正常 complete。
- **暗号存活**：被折段里先告诉模型暗号（如「蓝莓七号」），run 终稿答出（来源只能是
  摘要）。
- **库形态（psql）**：被折老 run 原文行 `compacted=t`；当前 run `seq=0` 摘要行 +
  `seq=1` user 行 + 保留轮次行齐全、被折轮次**无行**（从未落库）；runlog 完整含被折
  轮次的 tool_call/tool_result 行。
- **配对不破**：折叠后至 run 结束，模型每次调用不遭 provider 400（role 回扩生效的
  活证据）；fixtures 单测钉「保留的第一个交换单位不以 tool 行开头」「切点全在单位边界」
  「user 行恒在保留区首位」。
- **system 钉首**：fixtures 断言重写后 `state.Messages[0]` 仍是 system 行（角色与原文
  逐字节不变）；psql 断言 mh 无 system 行（折后落库的行全是 user/assistant/tool）。
- **同槽换代**：同一 run 被 mid-run 折过之后，再被后续 run 的折叠整体折入（此时它已有
  seq=0 行）→ EndRun UPSERT 正常提交、无 UNIQUE 回滚；psql 断言该 seq=0 行 `id` 不变、
  `text` 已换代为新摘要、`compacted=f`（同 run 活摘要恒 ≤ 1）。
- **keep 区保留行存活**：被 mid-run 折过的 run 在下一次折叠落 keep 区 → psql 断言其
  保留轮次行仍 `compacted=f`、其 seq=0 行已翻死、旧摘要内容在新摘要里（§10 翻转粒度
  裁定的活证据）。
- **零写入家族**：mid-run fold 后 kill -9 ⇒ 库里与未压过同形；error 结局 ⇒ 零
  compacted 写入；staged 列表只落最后一份摘要（psql：本次被并的旧摘要行全部
  compacted=t，可见 seq=0 只剩新落的那份）。
- **四端点字节零变化**：POST / session / frames / log 响应形状与压缩前逐字节同形
  （折叠不出帧，承 19v6-90 三修原则）。
- **触发线灵敏**：带巨大 `write_file` 参数的会话在旧口径下不触发、新口径下触发
  （基数补洞的定向用例）。

## §14 施工面清单（供拆 todos，顺序即依赖序）

1. **countInput 基数扩展**（text + tool_calls JSON）——独立可验，先落。
2. **compact.go 泛化**：foldInput 支持「老 run 整 run + 当前 run 单位后缀」的混合
   drop、切点选择、keep 区构造（user 行钉死 + 后缀预算含 user 行）、**摘要写入 UPSERT**
   （同槽换代，SQL 见 §5）、**翻转粒度细化**（DropRunIDs 拆两半：drop 区整 run 翻死、
   keep 区旧摘要 run 只翻其 seq=0 行，§10）、staged 列表产出；renderFold 吃
   schema.Message 转换层。文件头与分组注释里「seq=0 必是该 run 唯一可见行」的已死
   不变量同步改写。
3. **middleware 新文件**：`BeforeModelRewriteState` 里数长度（转换层切出 system 首条后
   过同一 countInput）、调折叠链、重写 state（**system 行钉首**）；三件套断言（单位
   边界 / 首个交换单位非 tool 开头 / 后缀不切在途）+ system 钉首断言；挂载排 agentlog
   之前（§4）；`AfterAgent` 里把最终 state 存进 run 级容器。
4. **worker/EndRun 改造**：run 级容器建/塞 ctx（比照 agentModelTable 先例，§8）；
   staged 列表应用（并集翻转 + 只插最后摘要、挂点 = 最后那次的 AttachRun）；mh 写入源
   切折后 state（过滤 system 行、跳过已在库的 user 行，保留轮次 seq 从 2 连续排）；
   runlog 保持全量 turn；prepareInput 往容器填历史/当前标签。
5. **验收网**：§13 话本 + fixtures + 四端点 baseline 重跑。

每拍验收 = 对应话本的 curl + 期望字节/日志行；第一拍能 curl 到的是「带大参数 write_file
的会话触发线变化」（日志断言）。
