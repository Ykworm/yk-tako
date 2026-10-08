# Handoff · v6 现状与能力差距（2026-10-07）

> 性质：**纯 handoff / 快照文档**，不改动任何代码。
> 触发对话（2026-10-07）：用户问「现在的系统还差什么？可以对比 pi、weknora」。
> 本文记录：① 当日核实的 v6 现状快照；② 对比 pi / WeKnora 的能力差距（分「重建 / 从未做」两叠）；
> ③ 下一步候选与挂账清单。留给后续会话对照：D1 验收、下个 loop 立项、doc/16 状态重标。
> 文中「现状」均逐条对过代码或台账；pi / WeKnora 结论来自当日对其仓库的实读盘点（结论级）。

## 1. 现状快照（2026-10-07）

### 1.1 构成

**v6 新仓 `yk-tako-eino/` 的全部 = 运行引擎 + 单会话聊天 UI + 模型配置层。**

| 位置 | 内容 |
|---|---|
| `cmd/yktako/` | 单入口；旗标 `-engine -batch-window -dev -redis -pg -lease-ttl -lease-renew`；启动双 fail fast（Redis / PG / 模型配置任一不可达不起进程） |
| `internal/config/` | `provider-catalog.json`（39 家厂商、openai/anthropic 两协议）+ `.env` 解析 + Eino ChatModel 装配；只支持 chat 一种模态 |
| `internal/runengine/` | 四个 API + dev reset + Store（跨 PG/Redis）+ Worker（真 LLM 流 + translator 归一层 + watchdog） |
| `web/` | Vue3 + TS 单会话聊天（~950 行）；U1–U6 + B1–B3 已验收 |

**不存在**：工具、护栏、看板、压缩、子代理、记忆、RAG、评测——旧线（v1–v5）已随 backup 整体出仓。

### 1.2 引擎已验收能力（A 系列全部销账）

- **四 API**：`POST /api/runs` · `GET /api/sessions/{sessionId}` · `GET /api/runs/{runId}/frames` ·
  `GET /api/sessions/{sessionId}/log`（+ dev `DELETE /api/dev/reset`）。
  纯拉模型：无 SSE / 长连接 / 推送，实时性 = 客户端轮询节奏。
- **存储（D1 后）**：PG 四表 `sessions` / `runs` / `runlog_entries` / `run_model_messages` +
  Redis 两键 `tk:runlive` / `tk:draft`（runlive 终态 TTL 300s，回收参数非正确性参数）。
- **闸 = `runs` 活行部分唯一索引**（`one_live_run_per_session`），插入冲突即 409；
  租约 `lease_until`（出生即带）+ worker watchdog 续约（CAS：UPDATE 影响 0 行 = 判死停续）。
- **双游标**：`eventOffset`（run 内、临时，挂 frames）与 `runlogOffset`（session 内、永久，挂 log），
  各自只在自己端点前进；单批上限 W（`-batch-window`，dev 200）。
- **kill -9 自愈链**（已实测）：lease 判死 → 两键蒸发 → frames `gone` → 会话自动解锁 → 再 POST 202；
  死 run 不进历史（error 形状）。
- **provider 归一层**：claude / openai / eino 三 translator——加厂商 = 加 translator，帧协议与存储侧不动。
- 验收网：curl baseline（①–⑫ + ⓐ–ⓓ，字节级）+ `go test ./internal/runengine/ -count=1` 16 条真实例网
  （真 6390 独立 db + 5433 临时库）+ 变异检查 M1–M10。

### 1.3 未 commit / 挂账

**未 commit 三笔**（commit 粒度归用户）：

1. **doc/19 拆篇**：index + `19v6-01` 契约 / `02` reconcile 与双游标 / `03` 存储与生命周期 / `04` 验收 /
   `90` 演进记录，及全仓注释引用同步（含 `web/src/{api,chat}.ts`）。
2. **D1 全套**：`store.go` / `pgstore.go` / `store_test.go` 新增、`redisstore_test.go` 删除、
   `redisstore.go` 收窄、`main/server/worker/types.go`、`go.mod`/`go.sum`（+gorm/postgres、−miniredis）、
   `scripts/dev.sh`（PG 自举半边）。
3. **`chat.ts` 后加两笔**（晚于台账落盘、此前无记录；核准未提交清单时补记）：
   ① **B4#1 实修**——`fetchAssistantRow` → `drainLog`：收口拉 log 时把游标经过的**别人的**定稿行
   补渲染（`renderForeign`），本 tab 投递过的 user 行按 `messageId` 跳过；② 两处 D1 后过时注释
   （「闸释放与 terminal 压入是同一笔原子写」）回填为「放闸与定稿同笔 PG 事务」的说法。
   **未 commit、未验收**；`chat.ts` 因此不能按文件粒度折进 1 / 2 任一单批。

**挂账**（登记在案）：

- **D1 待用户验收**；两处待过目：① planning §8.4「事务漏口方向」的修正（已改，可被否决）；
  ② `web/src/chat.ts` 两处 D1 后过时注释——已回填，见批次 3（回填在验收链之后，当时「前端零改动」
  的证据不受影响）。
- **lease 参数**：renew 默认 5s→10s、`-lease-ttl`/`-lease-renew` 默认值改从 `.env` 读
  （按 `TAKO_*` 约定，如 `TAKO_LEASE_TTL`/`TAKO_LEASE_RENEW`）——已裁，待发话单开 action。
- **B4 前端遗留三条**：① 多客户端下 `fetchAssistantRow` 推进对齐游标却不渲染经过的行——已在工作树
  实修（批次 3），**未 commit、未验收**；② 一次性 DOM 失踪（未复现，观察项）；③ 自愈成新会话时
  旧气泡滞留。
- **两个待裁议点**：error 结局服务端要不要补 terminal 帧；
  endRun 时 DB 败要不要加进程内重试 / draft 打捞窗（现状 = 单次尝试、无重试、不压 terminal、无打捞窗）。

## 2. 差距分析（对比 pi / WeKnora）

### 2.0 先点名一个对齐问题

**doc/16 §2 能力总表的 ✅ 列是旧线口径**：它标的落点（`internal/adkrun`、`internal/tools`、`internal/guard`、
`internal/taskboard`、`internal/compact`、`internal/mediaoffload`、`internal/webui`）在当前仓均不存在。
下次维护 doc/16 时需重标——建议拆「旧线达成（历史）/ v6 现状」两列，避免把「做过的」继续当「现在有的」。

### 2.1 两叠清单

**A. 需在 v6 重建**（旧线达成过、代码已出仓、设计仍在 doc/）：

| 能力 | 设计文档 | v6 接缝（已预留处） |
|---|---|---|
| 主循环 + 工具集（read/bash/edit/write 等） | doc/05、08、09 | `run_model_messages` 的 role 锚 OpenAI 标准（tool 行随 loop 进）；runlog 扩展行（tool_call/tool_result）已裁「进 runlog、种类由 role 取值承载」 |
| 工具安全三件套 + 审批（放行/硬拒/暂停问人） | doc/06 | 「暂停问人」吃 `hitl_prompt`/`hitl_reply` 行；payload 列形状待立项时定（planning §8.6 ⑤） |
| 任务看板（Plan/Tasks/Actions） | doc/01、02、07 | 扩展行进 runlog 的裁定已覆盖；**重建时用已裁名 `tasktracker`**（见 `doc/XX-taskboard-重命名-handoff.md`，改名当时未执行） |
| 上下文压缩 | doc/12、13 | 落点已裁：`run_model_messages` 打 `compacted` 标记（原文保留）+ 每被压 run 一行 seq=0 摘要；触发者/摘要 role 未定 |
| media_offload（图片落盘换占位符） | doc/18 | —（旧线末期遗留五问记录在 `docs/2026-09-30-uncommitted-review-handoff.md`） |
| Web 端旧能力（审批 / 停止 / 事件回放等） | doc/10、11、15 | 现 web 只有聊天 + 连接自愈（reconcile / 双游标 / 409 排队 / 僵尸 sid 自愈） |

**B. 从未做过**（doc/16 的 ⬜）：

- **T1**：子代理（#5，旧排的「当前重心」；v6 里其前置 = 工具循环）、记忆（#6）、context_usage（#4c）。
- **T2**：树状会话（pi）、skill 使用追踪（Hermes）、场景化 eval（pi/Hermes）、§2b 创意池其余。
- **T3**：崩溃续跑、IM/cron、多用户多 Agent、对话→RAG（#10，有护栏）、
  多会话列表 UI（会话列表端点当时被裁：没有用例逼出的端点不进契约）。

### 2.2 对照要点（每源最值得偷什么）

**pi**（TS 极简 agent）：

- 工具面：默认工具集只 read/bash/edit/write 四项（doc/16 说的「4 原语」实指它）；
  codemode——模型写 JS 在 QuickJS 沙箱一次并列调多个工具、结果过滤后才进上下文；
  MCP 工具默认不注入，BM25 搜索按需加载。
- 上下文面：compaction 写 CompactionEntry——`firstKeptEntryId` 划保留区、**原条目永不删除**；
  `transformContext → convertToLlm` 两段转换（自定义消息可只给 UI 不进模型）。
- 会话面：树状 JSONL（entry 带 `id/parentId`，只 active branch 进上下文），`/tree` 原地分支、`/fork`、`/clone`。
- 分发布局：Skills 走 agentskills.io 标准（SKILL.md；启动只注入 name+description，渐进披露）；
  Pi Packages（npm/git/本地）一包同带 skills/extensions。
- 评测：vitest-evals；docs-lift（Docker 双镜像 with_docs/without_docs 配对算「文档提升」）。
- **注意**：pi 的多客户端/崩溃恢复在实验包（durable/chord），且明确不做 CRDT 离线合并、
  storage 无跨进程锁——tako 的「多副本闸 + 租约 + 双游标」在这条线上已更彻底；树状会话才是它领先的形态。

**WeKnora**（Go，Eino 类库参考）：

- RAG 工程全套：docreader（独立 Python gRPC 解析）/ chunker（父子块 + 标题面包屑）/
  向量库抽象 ~10 后端（pgvector、ES、Qdrant、Milvus…）/ 混合检索 RRF 融合 / rerank / GraphRAG。
- 记忆：asynq 异步抽取 → consolidate 合并 → topic 归类；向量+词法混合召回；
  常驻块（profile/偏好）+ 按需块（fact/task/interest）；agent 有 search_memory 工具。
- agent：手写 ReAct 引擎（非 Eino）+ 工具审批 + MCP client/manager/OAuth。
- 评测：指标族（Precision/Recall/NDCG/MRR/MAP/BLEU/ROUGE）+ parquet 数据集跑真实管线。
- 产品面：~30 家 provider 分型（chat/embedding/rerank/vlm/asr）、多租户 RBAC、IM 十渠道、
  数据源连接器、Langfuse 追踪。
- **护栏**（doc/16 §4）：只借工程不学定位——正本永远结构化存储，向量库只是可重建派生副本；
  只在「agent 回忆历史」场景服务 agent。

**steer 附注**：运行中插话——pi 有 steering/follow-up 双队列，WeKnora 有生产版收件箱
（inject/after 双模式、队深上限 10、SteerID 幂等键）。tako 现裁 = 409 + 客户端本地排队（B1 已落），
属「明确不做」，不是遗漏。

### 2.3 归纳

引擎层（多副本正确性、崩溃自愈、纯拉取收敛）是刚打稳的地基，比两个参考源的同位物更彻底；
**agent 能力层几乎全空**（重建 + 从未做两叠）；产品层（多会话管理、多入口、多模态、多用户）按 T3 排队。

## 3. 下一步候选（不预拍板，供立项对话）

1. **工具循环 + 基础工具集**——T1 三项（子代理 / 记忆 / context_usage）共同的前置，引擎 schema 已预留写入口，
   也是第一个「重建」项。立项时第一个要拍的问题：循环走 **Eino ADK**（旧线路径，middleware/checkpoint 现成）
   还是**手写 ReAct**（WeKnora 路径）还是**在 v6 worker 上扩展**——它决定 tool 消息写入
   `run_model_messages`、以及 runlog 扩展行 payload 的形态（planning §8.6 ⑤）。
2. **doc/16 状态重标到 v6 口径**——小活，可与 1 同拍或先行。
3. **D1 验收**（用户点头后才谈 commit 粒度）。
4. 挂账零星项（§1.3）按用户发话逐个开 action。

## 4. 环境与纪律（给接手会话）

- **协作模式**（AGENTS.md）：planning + todos 交审 → 点头才 coding；每个 action 停下交审；
  **不自动 commit**；契约先行、先 API 后存储；验收第一行必须是「打一个跑着的东西」
  （curl + 期望字节 / `kill -9` / 两个窗口），不写「页面上看到 X」。
- **端口纪律**：6379（otel-ch-native 的 Redis，37 键）/ 5432（其 PG）/ 8090、8901（用户自己的实例）
  **全程不动**；dev 自建 = Redis 6390 + PG 5433；端口被占先 `lsof`，换端口不杀别人。
- **承重论断先验前提**：doc 注释 / 记忆 / 印象 / 已废代码都不是证据；
  **`doc/19-v6/` 是引擎真源**（v1–v5 不作论据）；`tmp/19-v6/` 是执行台账（不进版本库）。
- **命令**：

```bash
# 起全栈（前台，Ctrl-C 一起停；Redis/PG 缺啥自举啥，已有监听者复用不杀）
./scripts/dev.sh            # 引擎 8901 + vite 5173 + Redis 6390 + PG 5433
./scripts/dev.sh status     # 同理 stop；多实例按 ENGINE_PORT 区分

# 单跑引擎 / 单测
cd yk-tako-eino             # .env 只查 CWD，必须从本目录起
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go run ./cmd/yktako -engine 127.0.0.1:8901 \
  -redis 127.0.0.1:6390 -pg "host=127.0.0.1 port=5433 dbname=tako sslmode=disable"
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go test ./internal/runengine/ -count=1   # 16 条真实例网
```

## 5. 关键文档索引

| 文档 | 内容 |
|---|---|
| `doc/19-v6/19-运行引擎架构-v6.md` + `19v6-01..04/90` | **引擎真源**：契约四 API / reconcile 与双游标 / 存储形态与生命周期 / 验收 / 演进记录 |
| `tmp/19-v6/planning.md` + `todos.md` | 执行台账（planning §8 = 落库施工规格；D1 实测记录在 todos 节末）；不进版本库 |
| `doc/16-能力复刻总规划.md` | 能力总规划与梯队（状态列待按 v6 重标） |
| `doc/01–18` | 旧线设计文档——重建时的设计来源；注意端点/协议措辞多为旧线口径，与 v6 冲突时以 v6 为准 |
| `docs/2026-09-30-uncommitted-review-handoff.md` | 旧线末期未清问题（mediaoffload 五问等） |
| `doc/XX-taskboard-重命名-handoff.md` | taskboard→tasktracker 改名决策与影响面（改名未执行） |
| 本文件 | 现状快照 + 差距分析 + 下一步候选 |
