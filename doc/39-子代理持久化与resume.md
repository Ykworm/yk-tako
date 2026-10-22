# 39 · 子代理持久化与 resume

> 性质：**已落地**。2026-10-21。v2：持久化形态改为「每实例一份追加式 wire 文件」（借鉴 Kimi wire.jsonl 的形态，消息粒度）；v3：砍 PG 目录表，约定定位（目录布局 + metadata + terminal 行）；**v4：砍半截重接（S-i 反转）**——崩溃在半途的实例 = 失败，对齐 doc/30「放弃半截、client 重发」的 run 级语义，resume 只认完整档案。S1 随 `e476749` 入库；S2、S3 于 2026-10-21 在 `127.0.0.1:8905` 用 curl 验收后随本篇入库。不表示版本号跳了一档。
> 验收结论（2026-10-21，工作区 `tmp/ws-38`）：会话 `s_01M5D7XEPP9ZC4QMG1V58X0SS6` 的实例 `sa_01M5D7XFPTZYDEBTCYHTCRYE1G`，wire 的 SHA-256 在其后的重启和清扫里仍是 `71a4d705d46ef01d212704b8d29f37cc036d06d1982bbd54b403055d79856455`。续跑 run `r_01M5D94WR1NK5R96CKRN1T5B6B` 完成，新实例 `sa_01M5D94XYAGRZX720Q1T4Z74C0`，父工具结果以「（本次委派实例：sa_01M5D94XYAGRZX720Q1T4Z74C0）」收尾。半截副本回「该实例未完成或已中断，请重新委派」；过窗副本回「档案已过期」；不存在的 id 回「找不到该子代理档案，请重新委派」。S3 挂在 doc/37 同一轮清扫上，只删文件：探针删掉过窗且有 terminal 的副本，以及 `created_at` 过窗的半截副本；窗口内的完整档案和窗口内的半截都留下。`sa_01M5D7PAST000000000000000AA.jsonl` 的 `finished_at` 早于默认 168h，第一轮 ticker 收走了它；`sa_01M5D7PART00000000000000AA.jsonl` 还在盘上。单测 `TestLocate` 用同一份 `archiveTTL` 证明：窗口收成 12h 时，过窗档案拒绝续跑。活体没有把 S1 原件拨老。
> 代码落点：`yk-tako-eino/internal/runengine/agents.go`（wire 写入 + resume 工具）、`worker.go`（注入与装配）、doc/37 的清扫器（第三类清理，只删文件）。
> 真源关系：委派与折叠机制是 [doc/25](25-子代理折叠.md)、运行时装配是 [doc/34](34-租户运行时装配.md)、崩溃语义对齐 [doc/30](30-崩溃续跑.md)（放弃半截、不自动重跑）；档案的保留窗口与清扫器继承 [doc/37](37-删除与档案清理.md) §5.5。**定位：本篇从清理架构看是 doc/37 的一员**——wire 是会话生命周期内的系统档案（`.tako/`），resume 只是它在保留窗口内的价值形态；窗口与清扫的真源唯一在 doc/37，本篇不另立（同一份文件，ttl 读数前叫劳动力、读数后叫垃圾，同一个数）。Kimi wire 参考：[ref-agent/kimi-code wire-manifest.d.ts](../ref-agent/kimi-code/packages/agent-core-v2/docs/wire-manifest.d.ts)（事件溯源协议，**只取形态不取协议**，见 §3.1）。
> 不在本篇：用户页面入口（子代理列表/「继续」按钮 **2026-10-23 已由 [doc/44](44-前端三处欠账.md) 落地**）、跨会话 resume、事件溯源级别的 replay 引擎（§3.1）、跨会话子代理用量统计（S-a 的「要认的损失」）、**崩溃半截重接**（v4 明确不做，见 §9）、常驻后台子代理。

## 0. 本篇做什么

给每次委派一个**持久实例档案**，跑的过程中**每轮追加**记录；**完整跑完**的实例在窗口内可以「接着继续做」（resume）。两件事：

1. **wire 文件**：每次委派一个 `sa_` 实例一份 JSONL（`<会话目录>/.tako/subagents/<sa_id>.jsonl`），每轮模型往返追加一条消息，终态追加 terminal 行。**没有 PG 表**——实例身份、定位、过期判断全由约定（§5）从文件推导；
2. **`resume_subagent` 工具**：父 agent 在对话里发起「让刚才那个调研子代理补充一下 xx」，引擎按约定找到完整档案、注入历史、重建子代理继续跑，产出**新实例**（`resumed_from` 成链）；档案过期由 doc/37 D4 的清扫器回收（第三类，只删文件）。

**不做什么（v4 拍死）：** 子代理跑到一半被 close / 崩了 / 出 error —— **放弃**，这就是一次失败的委派，语义对齐 doc/30 的 run 级兜底（「引擎不会自动重跑这次调用，需要的话请重新发起」）。想继续 = client 重发、父 agent 重新委派（新实例），或 resume 一个**之前完整跑完**的档案。不做从断点重接：半截现场的工具副作用已部分发生，续跑要处理幂等，比干净重跑更不可靠，收益配不上复杂度。

## 1. 背景与动机

**主故事（想追问，没了）：** researcher 子代理跑两小时、十几个工具调用，折叠成三行摘要回主对话。用户想追问「那个对比表的数据源是哪几个？」——父 agent 没有：子代理的现场在跑完那一刻**即弃**（doc/25 §4 原话：「无 AfterAgent：子代理 state 跑完即弃，没有取货口」）。只能从头交代背景重跑。

**副故事（过程是黑箱）：** 子代理在跑的时候，人只能等 Redis 帧流推给页面的片段；想看完整过程记录，库里没有按实例组织的持久化档案（`runlog_entries` 按 run 混排，无父子配对结构）。崩死的委派连"死在哪"都要翻混排日志。

两个故事同根：**完整跑完的子代理价值在过程里，而过程只活在内存**——追问接不上、过程查不了。持久化的正确姿势是**跑的过程中按实例追加**（文件里永远有跑到哪算哪的记录，过程可查、`tail -f` 可见），并让**完整档案**在窗口内可续。

**半截（v4 的明确边界）：** 崩在半途的子代理不救。追加写留下的半成品 wire 有两样残余价值：人可以用它**诊断死在哪**（读到文件末尾即可），清扫器会在 orphans TTL 后收走它。它是事故现场的笔录，不是复活仓。

## 2. 名词

- **委派实例**：一次工具调用把某个规格的子代理跑完的全过程。**sa_id** 是这次实例的 id（`sa_<ULID>`，文件名主键；规格 coder/researcher 可反复委派，每次一个新 sa_id）。
- **wire 文件**：`<会话目录>/.tako/subagents/<sa_id>.jsonl`。第一行 `metadata`；随后每轮模型往返**追加**一条消息行；**最后一行 `terminal`（status + 完成时间 + 终稿）——完整档案的标志，resume 和清扫都只认它**。粒度是消息，不是事件（§3.1）。
- **完整档案 / 半成品**：有 terminal 行 / 无 terminal 行。resume 只认完整档案；半成品是诊断材料 + 孤儿垃圾（§5 清扫分支二）。
- **约定**：定位实例的三件事全在文件上——目录布局、metadata 字段、terminal 行。没有数据库索引，文件系统本身就是索引。
- **resume**：以完整档案的消息序列作为历史、追加一条新指令继续跑。产出是**新实例**（新 wire，`resumed_from` 指回原 sa_id）——档案只追加成链，原现场不可变。
- **保留窗口**：terminal 行的 `finished_at + archive_ttl`（doc/37 的 `-archive-ttl`，默认 7 天）。窗口内可 resume，窗口外文件被清扫。
- **orphans TTL**：无 terminal 行的半成品文件按 `created_at + archive_ttl` 清扫——正在跑的（created_at 新）自然豁免，死透的（created_at 旧）被收走，无需 lease 区分死活。

## 3. 现状机制（代码事实，2026-10-21 实读）

- **装配**：`agentSpecHandler` 每 Run 把规格装配成 `adk.NewAgentTool` 包进 `delegationTool`（并发闸 + 回传裁剪），追加进父 run 工具池（agents.go:430-509）。模型按「显式 ref > 租户 profile > 继承主实例」三级定；工具池「名单独立、实例共享」（agents.go:590）。
- **消息落点**：子代理的完整 ReAct 消息序列**不进 `run_model_messages`**（root-only，midfold.go:394）。持久化的是 `runlog_entries`（按 run 混排的文本流水）和 Redis 帧流——展示有余、按实例重建不足。
- **折叠**：`agentFoldHandler` 挂在子代理 middleware 链，每轮模型调用前对**子代理全量 `state.Messages`** 估/折/滤（midfold.go:673-721）——折前原文拿得到，但跑完即弃、不落库。
- **工作目录**：子代理跑在父 run 的同一个 ctx 里（delegationTool 原样透传），作业根 = 父 run 的会话或项目目录（worker.go:468-480）。子代理写的文件就是父容器里的文件，resume 后天然可见——文件副作用不需要恢复，这半边成本为零。
- **注入路径**：eino v0.9.21 `ChatModelAgentConfig` 无「初始历史」字段，但 `GenModelInput` 回调可在装配时构造喂给模型的消息序列（已查证签名）。
- **run 级 resume 的语义（v4 对齐它）**：`POST /api/runs/:runId/resume` 的 journal 是 root-only（journal.go:281、:93-96），重放时对中断的工具调用回固定文本「**引擎不会自动重跑这次调用，需要的话请重新发起**」——放弃半截、重发重跑。子代理沿用同一产品语义。

### 3.1 与 Kimi wire 的关系：取形态，不取协议

Kimi 的 `wire.jsonl` 是**事件溯源 journal**：每行一条事件、64 种 record type、protocol 迁到 1.5，状态靠重放事件折出，支撑它的是 DI 引擎 + replay + vis 一整套。**本篇不取这套协议**：tako 的 resume 单元是**消息序列**（喂给模型接着跑），不需要事件级粒度；再造一个事件 journal 会与 Redis 帧流 / runlog 形成第二个真源；事件溯源 = 引擎级改造，超出本篇。取它的**形态**：每实例一份、追加写、metadata 首行 + terminal 尾行、版本号防格式漂移、按会话目录组织随删会话回收。

## 4. 待拍决策与推荐

| # | 决策点 | 推荐 | 理由 |
|---|---|---|---|
| S-a | 档案存哪 | **纯 wire 文件，无 PG 表** | 约定即索引：目录布局定位文件、metadata 定身份、terminal 行定过期——查询/权限/清扫都从文件推导（§5）。少一张表、少一类清扫、少一层一致性缝。要认的损失：跨会话 SQL 聚合审计没了，真有需求时从 metadata 反向同步出表 |
| S-b | resume 的入口 | **模型侧工具 `resume_subagent`**，两种寻址：`{id}` 或 `{agent}`（默认该规格最近一次完整档案） | 模型不必记 ULID——回传文本带实例 id，模型的记忆就是对话内容；不带 id 就按规格名 glob 取 created_at 最新。**只列/只认有 terminal 行的档案**（v4） |
| S-c | resume 的可见范围 | **先限同会话** | 文件挂在会话目录下，SessionAccess 拦会话即拦文件；跨会话等有真实需求再开 |
| S-d | wire 的行格式 | **消息粒度 JSONL**：metadata 首行、schema.Message JSON 消息行、terminal 尾行 | 消息是 resume 的最小充分单元；eino 原生序列化形态，反序列化直接可用 |
| S-e | resume 产出放哪 | **新实例**：新 wire，`resumed_from` 写进新 metadata | 档案只追加成链：原现场不可变，新跑的可追溯 |
| S-f | 运行环境跟谁 | **跟随发起 resume 的当前 run**：作业根用当前 run 的（文件天然可见），模型/工具池按当前租户 profile 重新装配 | 档案只带「知识和上下文」，不带「目录/模型绑定」——租户换过模型时 resume 用新配置，不绕过 doc/34 装配预检 |
| S-g | 窗口与清扫 | **继承 doc/37：`-archive-ttl` 同一个数；清扫器第三类 = 只删文件**，两个分支：① 有 terminal 且 `finished_at + ttl < now` → 删（正常过期）；② **无 terminal 且 `created_at + ttl < now` → 删（死透的半成品孤儿）** | terminal 行自带 finished_at，过期现场算；分支二让崩死的半成品也有出口——正在跑的（created_at 新）自然豁免，无需 lease 区分死活（v4 的关键简化） |
| S-h | 档案过期/缺失/损坏/不完整 | **拒绝，错误文本回给模型**（「已过期 / 找不到 / 损坏」）；**无 terminal 行 = 「该实例未完成/已中断，请重新委派」** | 对齐 doc/25 错误形状（工具失败不打断父 run）与 doc/30 语义（不自动重跑中断的调用） |
| S-i | 崩溃在半途的实例（**v4 反转**） | **不重接**。半成品 wire 保留到清扫（可诊断死在哪），resume 一律拒绝；想继续 = 重新委派或 resume 别的完整档案 | 半截现场的工具副作用已部分发生，续跑要处理幂等，比干净重跑更不可靠；与 doc/30「放弃半截、client 重发」语义对齐；省掉 running lease / 尾轮重试一整层复杂度 |
| S-j | 写入时机与性能 | **每轮模型往返后追加**（子代理 middleware 链，与折叠同链相邻）；写失败仅记日志、不影响跑 | 追加 O(1)、摊销成本；档案是增强不是正确性依赖。并发同名委派各写各的文件，无写冲突 |
| S-k | 实例 id 的生成 | **sa_ + ULID**，创建 wire 时铸 | ULID 免并发撞号；id 在 metadata、回传文本、resume 参数三处一致 |

## 5. 约定（本方案的"真身"，全部在这）

**目录布局**：`<workspace>/tenants/<tid>/sessions/<sid>/.tako/subagents/<sa_id>.jsonl`。挂载项目的会话也统一放**会话目录**（不在项目目录）——删会话（doc/37 D3 的 `.tako/` 级联清）天然回收。

**wire 文件（JSONL）**：

```jsonl
{"type":"metadata","sa_id":"sa_…","agent_name":"researcher","model_ref":"deepseek/…","run_id":"r_…","resumed_from":"sa_…或空","protocol":1,"created_at":"…"}
{"type":"message","msg":{ …assistant，含 tool_calls… }}
{"type":"message","msg":{ …tool 结果… }}
{"type":"terminal","status":"complete","result":"…回传终稿…","finished_at":"…"}
```

**四个消费方各怎么动作**：

| 消费方 | 动作 |
|---|---|
| 委派工具 | 铸 `sa_id` → MkdirAll + 写 metadata 行；每轮追加消息行；**终态（含失败）追加 terminal 行（status=complete/error 均可被 resume——error 档案 resume 时模型自己会看到上轮怎么死的）**；回传文本带「本次委派实例：sa_…」。**中断（无终态）不留 terminal**——文件保持半成品，交清扫分支二 |
| resume 工具 | `{id}` 拼路径 / `{agent}` glob 取该规格最新 **terminal 行**的实例 → 校验：terminal 存在？`finished_at + ttl` 未过期？消息行逐条可解析（丢最后一条半截解析失败的行——读完整性处置，不为重接）→ 反序列化 → `GenModelInput` 注入 |
| 清扫器 | glob 各租户 `sessions/*/.tako/subagents/*.jsonl` → 读末行：有 terminal 且过窗 → 删；无 terminal 且 `created_at` 过窗 → 删（孤儿）；其余跳过 |
| 查询端点 | `GET /api/sessions/:id/subagents` → glob + 读 metadata/末行组装 JSON（标注 `complete` / `interrupted`），权限同 SessionAccess |

## 6. 端到端链路

**正常委派（改造后）：** 用户发任务 → 父 agent 调委派工具 → 铸 `sa_id`、开 wire 写 metadata → 子代理跑（折叠照常）→ 每轮追加消息行（S-j）→ 终态：追加 terminal 行，回传终稿（带实例 id）→ 父对话。**原链路每一步不变，只多一个文件。**

**resume：** 用户说「让那个调研子代理再看看 Y」→ 父 agent 调 `resume_subagent({agent:"researcher"})` → 按 §5 定位**完整档案** → 校验（S-h）→ 装配新子代理：`GenModelInput` 返回 `[wire 消息…, 新 user(instruction)]`，配置按当前租户 profile → 跑（每轮追加到**新 wire**，`resumed_from` 指回原实例）→ 新终稿回传。

**中断（v4 语义）：** 进程死在子代理半途 → 无 terminal 行 → 父对话由 doc/30 journal 兜底（「引擎不会自动重跑这次调用，需要的话请重新发起」）→ 用户重发，父 agent **重新委派**（新实例）；半成品 wire 留着给人诊断（`tail` 看死在哪），清扫分支二到期收走。拿半成品 id 来 resume → 「该实例未完成/已中断，请重新委派」（S-h）。

**过期：** terminal.finished_at 过窗 → 清扫删文件 → 再 resume 回「已过期」。

**删会话：** D3 把 `.tako/` 整个 RemoveAll → wire 随系统档案清；清扫器 glob 不到即一致（没有 PG 行要补删）。

## 7. 分拍（点头后拆进 tmp 台账）

| 拍 | 做什么 | 验收形态 |
|---|---|---|
| S1 | wire 写入（铸实例、metadata、每轮 append、terminal 行、回传带 id）+ `GET /api/sessions/:id/subagents`（glob 组装，标 complete/interrupted，权限同 SessionAccess） | curl：固定行为规格委派一场（先例 doc/34 SPEC-OK 手法）→ 查端点字段齐；盘上 wire 三行齐；`tail -f` 可见追加；**原链路不变**（折叠、runlog 照常） |
| S2 | `resume_subagent` 工具（§5 两种寻址、只认 terminal 行、过期/缺失/损坏/未完成四拒绝分支、`GenModelInput` 注入、新实例 `resumed_from` 成链） | curl：会话内按 `{agent}` resume（固定行为规格）→ 新实例行为可预期、metadata 链正确；**手工截掉 terminal 行造半成品 → resume 回「未完成/已中断」**；跨会话/过期/缺失各打一遍 |
| S3 | 清扫挂接（第三类：glob + 末行判断两分支——过窗 terminal / 过窗无 terminal 孤儿，只删文件）+ 单测全量 | curl：dev 探针（doc/37 D4 的 `clean-sweep`）→ terminal.finished_at 改 8 天前 → 文件没了；**无 terminal 的文件 created_at 改 8 天前 → 也没了**；新跑的（两种都新）不动；`go test` + `go vet` 绿 |

S1 是第一个 action（curl 可打）。S2 依赖 S1；S3 依赖 doc/37 D4（清扫器先存在）——**施工顺序本篇排在 doc/37 D4 之后**。

## 8. 与 doc/37 的接口（双向）

- 继承 doc/37 的 `-archive-ttl`、`cleaner_state` 租约/水印、`POST /api/dev/clean-sweep` 探针；
- wire 在会话目录 `.tako/subagents/` 下 = **系统档案**：doc/37 D3 删会话随 `.tako/` 级联清；清扫器第三类删文件，没有 PG 行要补删；
- doc/37 的清扫器在 D4 施工时**不用预留第三类**——本篇 S3 自己加；
- 一致性缝只剩一条：会话目录被 D3 清掉后，模型拿旧 id 来 resume → 「找不到」（S-h）。

## 9. 明确不做的

- **崩溃半截重接（v4 拍死）：** 中断的委派 = 失败，client 重发、重新委派；半成品只作诊断 + 孤儿清扫。理由见 §0 / S-i：与 doc/30 语义一致、避开副作用幂等坑、省 running lease 一层。
- **事件溯源协议**：不引入 Kimi 式事件类型体系与 replay 引擎（§3.1）。
- **跨会话 resume**：S-c 已拍先限同会话。
- **前台入口**：子代理列表与「继续」按钮 **2026-10-23 已由 [doc/44](44-前端三处欠账.md) 落地**；wire 查看页仍不做（`tail -f` 已给本机调试一条活路）。
- **档案编辑/删除的用户入口**：档案由窗口管理，用户不直接碰。
- **跨会话用量统计的 SQL 聚合**：S-a 已认损失；需要时从 metadata 反向同步出表。
- **runlog_entries 按实例重组**：流水投影不动，实例档案由 wire 承担。

## 10. 闭环

**所以这解决了什么。** 完整跑完的子代理从「一次性用品」变成「窗口内可召回的劳动力」：追问能续（wire 注入历史，接着做不从头做）、过程可查（`tail -f` 一个 JSONL）；崩溃的委派有明确而不暧昧的语义——**放弃、重发、重跑**（对齐 doc/30），半成品 wire 是事故笔录（人可诊断死在哪）而不是复活仓，且孤儿文件由清扫分支二按同一窗口收走，无泄漏。档案生命周期完全复用 doc/37 的窗口与清扫器——恢复承诺和空间回收同一个数、同一套机制；约定（目录布局 + metadata + terminal 行）取代数据库索引，文件系统本身就是索引。

**仍不解决的：** 跨会话召回、wire 查看页、事件级 replay、跨会话用量统计、半截复活——§9 明说。
