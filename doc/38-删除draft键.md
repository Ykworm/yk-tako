# 38 · 删除 tk:draft 键

> 性质：**已落地**（2026-10-20 验收并提交）。
> 施工台账：`tmp/38-删除draft键/`（不入库）。复查结束前先留着。
> 一句话：draft 的两个职责——定稿正文的缓存、run 活着的哨兵——分别已经有 `turn` 和 `runepoch`
> 两个更准确的承载者；删掉它，Redis 临时态从三键收敛为 `tk:runlive` + `tk:runepoch` 两键，
> 定稿路径不再读 Redis。
> 关联：doc/30（崩溃续跑，draft 的出生地）、19v6-03（存储形态）、19v6-01（帧契约）、
> doc/31（SSE，读 runlive）。doc/30 第 8.3 节和第 19 节里的旧 draft 设计已标明「现行见 doc/38」，原文留作历史诊断。
> 验收（隔离库 `tako_38`、Redis `6390/9`、端口 8938）：
> - `go test ./internal/runengine` 通过（28.855s），`go vet ./internal/runengine` 通过。`yk-tako-eino` 与 `scripts/` 里 `grep draft` 无命中。
> - 多枪 `r_01M5CYQ2CRTJAK3QTHS58V6EQJ`：`done`，log 的 root assistant 与 Σ root delta 相同（长度 434），`tool_call` 1 帧。POST 后只有 `tk:runlive` 与 `tk:runepoch`，结束后只剩 `tk:runlive`。`tk:draft:*` 为空。
> - kill -9 后续跑 `r_01M5CYQ7JW1JA4HQNTHCKV3Y7J`：resume HTTP 202，首帧 `kind=resume`，`done`，assistant 文本长度 53，无 draft 键。
> - 短 run `r_01M5CYTQSDHYB6R3Y8CHC09XHG`：本 run 出生键是 `runlive` + `runepoch`，结束后只剩 `runlive`。
> - 最终回归多枪 `r_01M5CYTVW8CT2GFRDRER22NCGF`：`done`，全文相同，`tool_call` 2 帧，无 draft 键。
> - 最终回归 kill -9 `r_01M5CYV1RM6JGQZJ0XDQCDC60B`：重启后 `status=interrupted`、`resumable=true`，resume 202，首帧 `resume`，`done`，assistant 文本长度 4，无 draft 键。
> 第 1 节到第 4 节写的是删除前的链路，用来说明为什么删。现行写路径见第 5 节。

## 1. 先看一场 run 的完整链路

用户发一句话，服务端做这些事（箭头 = 数据流向）：

1. 用户 → `POST /api/runs`。
2. 引擎在 **PG 事务**里抢闸（`runs` 插入 running 行 + 部分唯一索引）、写 user 行
   （`run_model_messages` seq=1、`runlog_entries`），同时在 Redis 出生三个键：
   `tk:runlive:<run>`（帧流，压入 start 帧）、`tk:draft:<run>`（空字符串）、
   `tk:runepoch:<run>`（接管令牌，出生值 1），三者同一租约 TTL。
3. 模型流 → `worker.drain`：每个 chunk 先过 translator 取 `chunk.Content`，然后对 root 调
   `AppendDelta`。这条 Lua 做两件事：`RPUSH` 一帧 delta 到 `tk:runlive`，
   `APPEND` 同一段文本到 `tk:draft`。
4. 一枪排干后，worker 把合并后的 `merged.Content` 追加到内存里的 `turn`；如果模型要调工具，
   工具结果也进 `turn`，然后发下一枪。同一枪结束时还把 `merged.Content` 写进 PG 的
   `run_journal`（续跑账本）。
5. 用户 → `GET /frames` 轮询 `tk:runlive`，看到 start / delta / tool_call / tool_result / terminal。
6. run 结束 → `EndRun`。今天它在开 PG 事务**之前**先 `GET tk:draft` 拿全文；然后在同一笔
   PG 事务里放闸、把 `turn` 投影成 `run_model_messages`、把 `draft` 写成 `runlog_entries`
   的 root 终稿行；commit 后再压 terminal、删 `tk:draft` / `tk:runepoch`。
7. 用户 → `GET /log` 看到定稿的 assistant 全文。

同一段回复正文，在这条链路里出现了三次：

- `tk:runlive` 的 delta 帧（按帧保存，客户端消费）；
- `tk:draft`（按字符串拼接，EndRun 消费）；
- `turn`（内存，按枪保存，EndRun 消费）与 `run_journal`（PG，按枪保存，续跑时重放成 turn）。

§4 会证明 `draft` 与 `turn` 逐字节同源；§2 说明为什么保留 `turn`、删掉 `draft`。

## 2. 病灶：把缓存当成了真源

### 2.1 故事一：模型写完了，Redis 恰好不可用

一场 3000 字的回答，最后一段 delta 已经推给客户端。`EndRun` 开始收尾，第一件事是
`GET tk:draft`。这时 Redis 正在主从切换 / 重启 / 网络抖动，GET 返回连接错误。`EndRun`
在开 PG 事务之前就返回错误；worker 的 defer 只能记日志。PG 里的 run 仍是 `running`，租约
30 秒后过期。读侧把它判成 `interrupted`，下一次 POST 清扫成 `error`。

用户刚才明明在屏幕上看到了完整回答；刷新页面后，`/log` 里没有这条 assistant 行，气泡变成失败。
**模型已经写完，PG 也健康，但这场回答永远不会进 runlog。**

### 2.2 故事二：draft 被逐出，活 run 被缓存判死

Redis 配了 `maxmemory`，在内存压力下逐出了 `tk:draft`。下一次 `AppendDelta` 的 Lua 第一句是
`EXISTS draft`，返回 0，脚本直接返回 0；worker 把 `!live` 当成「run 已被判死」，停止推流。
PG 里的租约还在，watchdog 还在续约，但客户端再也收不到 delta。
**一个缓存键的消失，被当成了执行者的死亡。**

还有更安静的一种：如果 draft 在最后一枪之后、`EndRun` 之前消失（TTL 被误设、人工删除、
故障转移），`EndRun` 会读到空串，然后在 `complete` 状态下写一条 `text=''` 的 assistant 行——
**complete 但空回复**，且没有任何错误日志。

### 2.3 不做会怎样

- 定稿路径永远依赖一次 Redis 读；Redis 的可用性被抬到与 PG 同级，哪怕它只该影响
  「正在看的帧」。
- 每个 root delta 两次 Redis 写（RPUSH + APPEND），其中 APPEND 的结果在定稿后没有任何读者。
- 三份正文三种生命周期：draft 与 runlive 同 TTL、turn 在内存、journal 在 PG；任何一处不一致
  都要写一个新的对账测试。
- 讨论「谁是锁」时，draft 的 `EXISTS` 看起来像锁，实际锁在 PG。这个错觉已经导致过一次错误
  设计（把 draft 当互斥闸）；留着它，下一个人还会再错一次。

## 3. draft 到底是什么（操作定义）

不是比喻，逐条操作：

- **键**：`tk:draft:<runID>`，Redis STRING。
- **出生**：`AcquireRun` 的 Lua `SET key ''`，与 runlive/runepoch 同一毫秒 TTL。
- **增长**：每个 root delta 的 Lua `APPEND key <delta text>`。子代理的 delta 走 `AppendFrame`，
  不碰它。
- **守护**：`AppendDelta` / `AppendFrame` 的 Lua 第一句 `EXISTS key`；不存在 = 返回 0 = worker 停流。
- **读取**：`EndRun` 在 PG 事务前 `GET key`，把结果写进 runlog root 终稿行。
- **续跑**：`seedResume` 的 Lua `SET key <replay.Seed>`，然后新 delta 继续 APPEND；EndRun 时就是
  「旧正文 + 新正文」。
- **收尾**：`endRunRedisScript` 的 Lua `DEL key`。
- **过期**：watchdog 每 `lease-renew` 用 `PEXPIRE` 把它与 runlive/runepoch 一起刷回 lease；
  没人续约时三键同 TTL 蒸发。

所以它有两个职责：

| 职责 | 谁在用 | 今天怎么用 |
|---|---|---|
| 定稿正文 | `EndRun` | GET 全文 → runlog root assistant 行 |
| 活判哨兵 | `AppendDelta` / `AppendFrame` | `EXISTS` = 0 → 停流 |

它**不是**锁：锁是 PG `runs` 的活行 + `one_live_run_per_session` 部分唯一索引；活判的权威是
PG `lease_until`；防旧执行者的是 PG `lease_epoch` 与 Redis `runepoch` 的比对。这些在 doc/30
里已经是现行设计。

## 4. 为什么可以删：逐字节等价证明

### 4.1 记号

- 第 i 枪模型输出一串 chunk：`c_{i,1}, c_{i,2}, …`。
- translator 对每个 chunk 返回 `text_{i,j}`。当前三个 translator（`claudeChunkText` /
  `openaiChunkText` / `einoChunkText`）都只返回 `chunk.Content`；空内容返回 `ok=false`。
- `drain` 把同一串 chunk 交给 `schema.ConcatMessages`，得到 `merged_i`；因此
  `merged_i.Content = Σ_j text_{i,j}`。
- `drain` 对 root 的每个 `text_{i,j}` 调一次 `AppendDelta`；一枪结束后，worker 往 `turn`
  追加一条 `TurnEntry{Role:"assistant", Agent:"", Text: merged_i.Content}`。

### 4.2 归纳

设 `D_i` 为第 i 枪结束后的 draft，`T_i` 为此时 `turn` 中所有 root assistant 条目的 Text 拼接。

- 出生：`D_0 = ""`，`T_0 = ""`。
- 第 i 枪：`D_i = D_{i-1} + Σ_j text_{i,j} = D_{i-1} + merged_i.Content`（每个 delta 一次 APPEND）。
  `T_i = T_{i-1} + merged_i.Content`（每枪一条 turn）。
- 所以 `D_i = T_i` 对所有 i 成立。

`EndRun` 里的 `pending[""]` 正是 `Σ root assistant turn Text`（`store.go:521-529` 的循环），
也就是 `T_n`。因此把 `EndRun` 的正文来源从 `GET draft` 换成 `pending[""]`，写入 runlog 的
字节完全相同。

### 4.3 续跑也成立

- `replayJournal` 把账本里每条 root assistant 行同时放进 `Turn` 和 `Seed`
  （`journal.go:239`）；`Seed = Σ assistant 行 Text`。
- `seedResume` 今天把 draft 设成 `Seed`；续跑后 draft = `Seed + Σ 新 delta`。
- 续跑后 `turn` 从 `replay.Turn` 起手，
  `T = Σ 账本 assistant Text + Σ 新 merged.Content = Seed + Σ 新 delta`。
- 起点相等，之后每枪按 4.2 归纳。等价。

### 4.4 边界

- 空内容 chunk / 只有 tool_calls 的枪：translator 的 `ok=false` 或 text 为空，draft 与 turn
  都不增长。
- 子代理：`AppendFrame` 只写 runlive，不写 draft；`pending[agent]` 按 agent 分组，
  `pending[""]` 不含子代理文本。两边都不含。
- 思考内容：translator 不返回 `ReasoningContent`；draft 与 turn 都没有它。
- 半截 delta（没等到整枪排干就崩溃）：draft 有、journal/turn 没有。这是 doc/30 的
  at-most-once 语义——崩溃前那半句本来就会被 resume 丢弃。删除 draft 不改变这个语义（§8）。

### 4.5 哨兵等价

`EXISTS draft` 与 `runepoch == epoch` 的同生同灭关系：

- 出生：`acquireRedisScript` 在同一 Lua 里建两个键，TTL 相同。
- 续约：`renewRedisScript` 先比对 epoch，再 `PEXPIRE` 两个键。
- 收尾：`endRunRedisScript` 在同一 Lua 里 `DEL` 两个键。
- 接管：`seedResumeScript` 在同一 Lua 里 `SET` 新 epoch、`SET` 新 draft。
- 过期：两键 TTL 相同，同时蒸发。
- 外部删除：只有 draft 被删而 epoch 还在——这正是 §2.2 的病灶。删除 `EXISTS` 后，这种情况
  不再被误判为死亡。

反过来，epoch 还在而 draft 不在的唯一正常场景不存在；epoch 不在而 draft 在也不可能（同一 Lua）。
所以 `runepoch == epoch` 是比 `EXISTS draft` 更准确的判据：它回答的是「我还是不是这一代的
执行者」，而不是「一个缓存键还在不在」。

## 5. 目标机制

### 5.1 键空间

| 键 | 类型 | 谁写 | 谁读 | 生命周期 |
|---|---|---|---|---|
| `tk:runlive:<run>` | LIST | acquire / append / end / seed | frames / stream | 出生带 lease TTL；terminal 后 300s TTL；TTL 到 = gone |
| `tk:runepoch:<run>` | STRING | acquire / seed | append / renew | 出生带 lease TTL；续约刷新；end 删除；TTL 到 = 写口拒收 |

`tk:draft:<run>` 不再存在。

### 5.2 各动作

- **出生**：Lua 两件事——`RPUSH runlive start` + `PEXPIRE`，`SET runepoch 1` + `PEXPIRE`。
- **delta**：Lua 先 `GET runepoch == 本执行者 epoch`，不等返回 0；相等则 `RPUSH runlive delta`，
  返回 `LLEN`。
- **工具帧**：同一道 epoch 守卫，只 `RPUSH`。
- **续约**：PG CAS 成功 → Redis `GET runepoch == epoch` → `PEXPIRE` 两键。
- **EndRun**：PG 事务前**不碰 Redis**；事务内 `pending[""]` → runlog root 终稿；commit 后压
  terminal + `EXPIRE` + `DEL runepoch`。Redis 半失败仍按既有 B1 收口（run 已定稿在 PG，
  runlive 收敛成 gone）。
- **resume**：claim（PG epoch+1）→ replay（Seed 仍用于 resume 帧）→ `seedResume`：
  `SET runepoch 新代` + `DEL runlive` + `RPUSH resume 帧` + `PEXPIRE` 两键。不再写 draft。
  （`SET` 会清掉 runepoch 原有 TTL，脚本顺手 `PEXPIRE` 补回，保持两键同 TTL。）
- **Reset**：`SCAN tk:*` 少删一个键；dev 返回的 `deleted` 从 3/run 变 2/run。

### 5.3 端到端链路（删除后）

用户 → POST → PG 抢闸 + Redis 两键出生 → 模型流 → drain → `RPUSH runlive`（每 delta）→
客户端 frames → 每枪 `turn` 追加 → EndRun → PG 事务（`pending[""]` → runlog；turn → mh）→
commit → Redis terminal + DEL epoch → 客户端 log。

箭头每一环都能点名；没有一环需要 draft。

### 5.4 删除后的故障行为

- **生成中 Redis 写失败**：`AppendDelta` 返回错误，worker 照旧走 error 出口。这是既有行为，
  不因删除 draft 改变。删除 draft 不承诺「Redis 挂了 run 还能继续生成」。
- **定稿时 Redis 不可用**：EndRun 的 PG 事务照常提交，runlog 有完整正文；commit 后的
  terminal/删 epoch 失败，`runlive` 靠 lease/300s 收敛成 gone，客户端从 log 拿全文。这是删除
  draft 带来的真实改善（今天会在 GET draft 处直接失败，PG 事务根本不开始）。
- **`runepoch` 被逐出**：append 返回 0，worker 停流。它不再把正文带进判死：EndRun 仍能从
  `turn` 拿到已收到的部分。需要说明的是，这个出口今天不把 status 改成 error，所以若 PG
  租约仍有效，EndRun 会把已收到的部分按 complete 落库——这是既有行为（今天更糟：draft 被逐出
  时是 complete + 空文本），本方案不扩大也不修复它；彻底修需要单独裁决结局语义。
  2026-10-20 用户拍：本批只改这条路径的命名（§5.5），`status` 语义不动。

### 5.5 命名：拒写不等于 run 死了

删掉 draft 后，append 返回 0 的唯一原因是 `runepoch` 缺失或与本执行者 epoch 不等。它回答的是
「本执行者还是不是当前一代」，不是「run 还在不在」：run 可能已被接管、已收尾，也可能只是令牌键
到期/被逐出而 PG 租约仍有效。旧名字把这层意思说错了：

| 位置 | 今天 | 改为 | 为什么 |
|---|---|---|---|
| `AppendDelta` / `AppendFrame` 的命名返回值 | `ok bool` | `accepted bool` | 值 = 本代令牌接受这一帧写进 runlive |
| `drain` 的局部与返回 | `live bool` / `!live` | `accepted bool` / `!accepted` | 同上；不再是「run 活不活」 |
| worker 四处帧写出口的局部 | `live` | `accepted` | tool_result / drain / usage / tool_call |
| 相关注释 | 「run 判死」「liveness 由 draft 承载」 | 「本执行者已不是当前一代」「令牌缺失/不等」 | 删掉 draft 时代语言 |

只改名字，不改行为：四个 `!accepted` 出口仍然直接 `return`，不把 status 改成 error（见 §5.4）。
`search.go` 的结果计数、`product_api.go` 的活 run 数、`SessionStatus` 的 `liveRunId` 是别的
「live」，不在本批范围。

## 6. 移除方式：一次性删除（2026-10-20 用户拍）

不做「先换正文源、再换哨兵」的过渡态，一次把 draft 从代码里删干净。交付分三拍，
但后两拍不改变运行时行为：

| 拍 | 改什么 | 活体证明什么 |
|---|---|---|
| A1 | 一次性删除：EndRun 正文源改 `pending[""]`；append 守卫只认 epoch；删 `keyDraft`、各 Lua 的 draft 读写、`seedResume` 的 seed 参数、注释与测试机械修复；`live`/`ok` 改名为 `accepted` | 多枪 run 全程无 `tk:draft:*`；log 全文 == Σ root delta；kill -9 后 resume 仍 complete |
| A2 | 只加结构不变量测试（无 draft 键、seed 只留两键、租约过期仍挡住晚到 EndRun、Reset=2） | 复用 A1 的活体；把「不能把 draft 加回来」写成测试 |
| A3 | 文档收口 + 最终回归（正常多枪 + kill -9 resume） | 代码、文档、索引同一套真源 |

为什么可以一把梭：§4 已证明 `turn` 与 draft 逐字节同源，§4.5 已证明 `runepoch` 与 draft
同生同灭。过渡态的唯一价值是「draft 还在，可逐字节对照」，代价是两个不可发布的中间状态
（A1 后 draft 只写不读；A2 后 APPEND 可能重建无 TTL 的孤儿键）。用户选择一次性，用单测
（EndRun 不读 Redis、错 epoch 仍拒绝）与活体对账替代过渡态对照。

回滚：A1 是单批代码回退，键布局回到三键；draft 是临时态，没有数据迁移。整个方案不动
PG 表结构。

## 7. 文档清单（A3）

- `doc/38-删除draft键.md`（本篇）：头注从「方案（待点头）」改为「已落地」，写验收 run id /
  测试结论。
- `doc/30-崩溃续跑.md`：§4 的真源表、§7.3 的 Redis 围栏、§8.3 的 seed Lua、§11.4 的旧进程表、
  §19 的字节推导，以及正文所有「draft 是定稿真源」的句子；§7.3/§11.4 的 `live=false` / `!live` /
  「drain 判死退出」改为 `accepted=false` / `!accepted` / 「写口拒收退出」；在 §8.3 / §19 开头加
  「2026-10-20 起 draft 已移除，现行见 doc/38」，保留原文作历史诊断。注意该文件有另一会话的
  未提交 §19 附录，只做定点修改，不重写、不回滚。
- `19v6-03`：「三搬两留一消失」表中 `draft` 行改为「已移除（doc/38）」；「留 Redis 的理由」
  「生命周期」「真源划分」中 draft 的句子改为 runlive + runepoch；「四表两键」的表述改为
  「四表 + 两键（runlive / runepoch）」。
- `19-v6/19-运行引擎架构-v6.md:53-54`：结束/定稿两行的「删 draft」「残缺 draft」改为
  「删 runepoch」「残缺输出不进历史」。
- `19v6-05:260`：root 终稿「仍是 draft」改为「= Σ root turn 正文」。
- `19v6-10:108/357`：commit 后 Redis 半与终轮正文来源改词。
- `19v6-90`：追加一条修订记录（只追加，不改历史条目），说明 draft 移除与 doc/38。
- `doc/README.md`：新增 doc/38 行；状态从待点头改已落地。
- `doc/37-删除项目连带删会话.md`：不归本批改；其表中「runlive/draft」字面由该文作者随其
  落地更新。doc/38 记一条指针。

## 8. 风险、边界、不做

### 8.1 风险

- **translator 未来不再返回 `chunk.Content`**：等价证明会失效。防线 = A1 的活体对账
  （log 文本 vs Σ root delta）与 A3 的结构测试；任何新 translator 必须遵守「返回 Content
  子串」的约定，写在 doc/38。
- **有外部监控/脚本读 `tk:draft`**：仓库内没有；若部署侧有，需要改为读 `runepoch` 或 PG。
  发布说明里写一句。
- **`EXISTS` 守卫被别的代码路径依赖**：grep gate（Go / Lua / scripts）在 A3 验收里必须为
  0 命中；文档中的历史引用除外。
- **`runlive` 被误当哨兵**：见 §9 的否决项；runlive 在 terminal 后还活 300s，不能区分
  「已收尾」和「还在跑」。

### 8.2 边界

- 不改 `resume` 帧契约；`Seed` 继续作为帧文本。
- 不改 at-most-once（半截 delta 仍然丢）。
- 不改 frames/log/stream 的字节、状态码、游标语义。
- 不改 PG 表结构、不做数据迁移。
- 不改 `!accepted` 出口的结局语义（见 §5.4 最后一条）；本批只改名（§5.5）。
- 不保留「draft 只写不读」的配置开关；不提供 `TAKO_DRAFT=off`。
- 不碰 doc/37 的措辞；不碰 doc/30 另一会话的未提交附录内容。

### 8.3 不做

- 不把 epoch 塞进 runlive（见 §9）。
- 不引入 Redis Stream / 新键类型。
- 不把 runlive 的 300s 收尾 TTL 改短或改长。
- 不为「半截 delta」做持久化。

## 9. 被否决的替代方案

1. **用 `EXISTS runlive` 当哨兵**：runlive 在 terminal 后还有 300s TTL，这段时间 EXISTS=1，
   旧执行者可以在收尾后继续追加帧；而且 runlive 是客户端消费的帧流，把它的存在性当判活依据
   会把两个契约绑死。否决。
2. **EndRun 读 `run_journal` 拼正文**：journal 在 PG，文本确实在，但多一次读、多一个失败面；
   `turn` 已经在 EndRun 的参数里（worker 的 defer 直接传），没有任何理由再读一次。否决。
3. **draft 只留作缓存（写但不读/不当哨兵）**：每个 delta 仍多一次 APPEND，多一个生命周期；
   没有读者 = 死代码。否决。
4. **配置开关（`TAKO_DRAFT=on/off`）**：两条代码路径都要测，且「要不要一个缓存」不是运维
   选择，是结构决定。用户要的是安全、结构性的移除，不是可回退的影子模式。否决。
5. **把 epoch 塞进 runlive 头/尾**：`eventOffset = LIST index + 1` 是帧契约；头部加元素会让
   所有游标偏移，客户端全部返工。否决。
6. **把半截 delta 持久化到 journal 再 resume**：那是另一个需求（at-least-once），需要每 delta
   一次 PG 写或新的持久化层；doc/30 已裁 at-most-once。本方案不改变它。否决。
7. **本批顺手把 append 拒写出口的 status 改成 error**：能消除「令牌被逐出 + 租约仍有效 →
   部分正文按 complete 落库」的边角，但「被逐出」到底是失败还是可接受的部分定稿，是产品判断，
   牵扯前端对 complete 的信任。2026-10-20 用户拍：本批只改命名（§5.5），结局语义单独裁决。
   否决（本批）。

## 10. 验收总览

| 拍 | 活体验收（第一行是跑着的命令） | 单测 |
|---|---|---|
| A1 | 多枪 run（tool_call 至少 1 帧）→ 全程 `--scan --pattern 'tk:draft:*'` 为空；log 的 root assistant 文本 == Σ `kind=delta, agent=""` 的 text；kill -9 后 resume → 首帧 kind=resume、terminal complete，仍无 draft 键 | EndRun 正文来自 turn（不读 Redis）；Redis 不可达时 PG 事务仍提交（ok=true，历史正确）；错 epoch 仍拒绝；出生/追加/收尾/seed 后无 draft 键 |
| A2 | 短 run 扫键（复用 A1 活体）；`SCAN tk:*` 只有 runlive + runepoch | 无第三类键；seed 只留两键；租约过期后带非空 turn 的 EndRun 仍 ok=false、无 assistant 行（Reset deleted=2 由 A1 修现有测试时覆盖） |
| A3 | 最终回归：正常多枪 + kill -9 resume 各一场；`go test ./internal/runengine` + `go vet` 绿；代码/脚本 grep `draft` = 0 | —（回归） |

活体验收使用隔离环境：PG 库 `tako_38`、Redis db9（`redis://127.0.0.1:6390/9`）、
独立 workspace、独立端口；不碰 dev 的 db0/`tako` 库，不调用 `/api/dev/reset`。
原因：Reset 的 `SCAN tk:*` 是**库内全局**的，共享 db0 会误删别的会话的活 run 键。

## 11. 一句话

**draft 是 `turn` 的缓存、`runepoch` 的孪生哨兵；两个真身都已经在链路里。删掉缓存，
定稿只信 PG 里的 `turn`，帧流只认 `runepoch`。**
