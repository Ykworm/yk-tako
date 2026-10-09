# 19v6-05 · tool loop（T 系列）

> 本篇是 doc/19-v6 文档集的 **tool loop 篇**：Eino ADK 底座、工具消息的两个投影、sub agent（AgentTool）、
> 工具目录与 tool_search、观测 Handler 脚手架。
> **契约字节真源仍是 19v6-01**——本篇涉及的帧新 kind 与 `agent` 字段**已随 T2（2026-10-08，七修）落 19v6-01**；
> 本篇是 tool loop 的**机制裁定真源**。
> 立项：2026-10-08（用户拍板：循环走 Eino ADK）；执行台账 = `tmp/19-v6/planning.md` §11 + todos「T 系列」。

## 0. 术语（2026-10-08 用户裁）

三个词三种形状 / 住处 / 消费者，勿混：

| 词 | 形状 | 住处 | 消费者 | 粒度 |
|---|---|---|---|---|
| **Model message** | OpenAI messages（role/content/tool_calls） | 主线程 → `run_model_messages` 表；子代理 → **只在进程内**（AgentTool runner state） | worker 组装模型输入 | 消息 |
| **runlive** | 帧（`kind` + `agent` + 负载） | Redis `tk:runlive:{runId}` LIST（TTL 300s） | frames API（客户端轮询） | 帧 |
| **runlog** | 活动行（`role`/`agent`/`text`/`payload`） | PG `runlog_entries` | log API | 消息 |

index 概念表对 runlive/runlog 的定义不变；本篇钉死的是「模型输入侧叫 Model message、观测侧叫帧/runlive」，
以及两者**永不混写**：子代理的 Model message 不进 mh、帧不进 mh。

## 1. 底座：Eino ADK

- `ChatModelAgent` 进程内 loop 嵌进 worker（旧线 `adkrun` 先例）；watchdog / endRun / 闸路径不动。
- **铁判据 = 四个 API 契约字节零变化**（同 A7/D1 内部改造拍的判据）——底座更换不出契约面。
- worker 的手写 Stream 循环与 claude/openai/eino 三 translator 由「ADK 事件流 → 归一层 → v6 帧」重写：
  ADK 的 `TypedAgentEvent` 迭代器是**引擎内部物**，v6 帧是**契约物**，归一层是两者唯一接缝。
- **观测 Handler 脚手架**（用户指令，2026-10-08）：`ChatModelAgentMiddleware` 全套钩子
  （BeforeAgent / AfterAgent / BeforeModelRewriteState / AfterModelRewriteState / WrapModel /
  WrapInvokableToolCall / WrapStreamableToolCall / WrapEnhancedInvokableToolCall / WrapEnhancedStreamableToolCall
  ——v0.9.21 实读的九方法全集，T1 勘误）
  每处打终端日志 `[adk][序号] 钩子名｜功能一句话｜进/出`。转正点 = T6（收成旗标或拆，登记 planning §4）。

## 2. 工具消息的两个投影

同一次工具调用，两个投影各自成形状（种类由 `role` 取值承载，2026-10-07 已裁）：

- **mh（Model message，模型输入）**：OpenAI messages 形状。assistant 行 `text` = 纯正文不夹带；
  新增 `tool_calls` / `tool_call_id` 两列（§6）；tool 行 text = 工具结果。
  多轮时**逐轮一行**：工具轮的前导语在自己那行（与 tool_calls 同行），不并进终稿行——终稿行只放最后一轮正文。
- **runlog（展示与历史）**：消息粒度活动行——assistant 正文行 + 扩展行
  （role=`tool_call` / `tool_result`，`payload` 列存 name/args/output 等结构化负载）。
  assistant 行 text = 全 run delta 累计（所见即所得，收口替换的锚）。
- **runlive（实时观测）**：帧粒度——`tool_call` / `tool_result` 帧 + 流式 `delta` 帧。

## 3. 帧契约扩展（已落 19v6-01 §2，2026-10-08 七修）

- **新 kind**：`tool_call{name,args}` / `tool_result{name,output}`（字段名已钉，见 19v6-01 §2；
  `args` = 模型原始 JSON 字符串，原样透传不重编码）。
- **每帧带 `agent` 字段**：空 = root，含 start/terminal（2026-10-08 用户裁「任何帧都带 agent 是必然」）。
  命名取 `agent` 不取 `role`——帧 `kind` 与 runlog 行 `role` 已是同位语义，再引 role 两义。
- runlive 帧无 seq：LIST 顺序 = 下标 → frames 响应的 `nextOffset` 游标；`agent` 与之并列携带。

## 4. sub agent：AgentTool 自组装

- **裁定**：`adk.NewAgentTool` 每规格一包挂主 agent（tool as agent，Octop「tool 变子代理」同构）；
  **不采用 `adk/prebuilt/deep` 整包**——捆绑 write_todos/文件系统/Shell 与自带 prompt 哲学，
  工具集按「没有用例逼出的不进契约」由用例逐个逼出。
- **规格形态 = 文件式（2026-10-09 裁）**：`yk-tako-eino/agents/` 单目录，**md + YAML frontmatter**、
  正文 = system prompt；**装载 = run 起手扫盘**（改文件 → 下一条消息生效、run 中途不变；run 内工具集固定、
  子代理 runner 不重扫盘）。理由链：每规格一包 AgentTool ⇒ 工具清单 = 规格名单 ⇒ Eino 工具 schema 定稿于装配点
  （`once.Do`、首次 Run 前冻结）⇒ 要新鲜只能重装配 ⇒ 装载点 = 重装配点 = run 起手。
  实现两路（每 run 重建主 agent / `BeforeAgent` 换本次工具列表）实施时验证取哪条；随仓 seed 一条 tako 自写规格。
- **字段表（2026-10-09 裁）**：`name`（单字段身份 = AgentTool 工具名 = runlog `agent` 值；文件名不参与语义；
  撞名 → 拒该条 + 显式报错）/ `description`（必填，缺 → 拒该条报错）/ `tools`（三态，见下）/
  `model`（**缺席 → 继承主 worker 实例**；**写值 → 装载即拒报「未实现」**——「ref → 实例」机械件留给模型注册表 loop）；
  **不加展示字段**（emoji/color 等）。**不设信任门**（T3：规格 = 部署态只读产物；运行时写 = 挂账，真做前先裁真源）；
  管理面（dashboard/API = 文件操作可视壳：列/读/写/删，装 = 复制、卸 = 删）后置独立小拍。
- **Model message = 进程内**：AgentTool 每次调用起独立 runner（独立 compose state），跑完即弃，
  只有最终文本作为 tool result 回主 run。崩溃即丢，符合「run 死、会话活」语义。
- **runlog 落消息粒度行**：委派 tool_call / 子代理动作行 / 子代理终稿，`agent` 列非空；
  历史组装按 root 过滤（`event.AgentName == root`——不过滤会把子代理内部 tool 消息混进主历史）。
- **runlive 帧流**：子代理帧与主 agent 帧进同一归一层、同一 LIST，每帧带 `agent`。
- **粒度裁定（2026-10-08）**：runlog 记消息粒度、不记帧——主 agent 的 delta 本就不落历史表，
  子代理同构：**delta 帧只进 runlive，不落 runlog**。
- **并行**：同一 run 内多 goroutine 各跑独立 state，帧事件全经主 run 的单一 writer（归一层）落库，
  seq 串行分配——**并行不改表**，无共享可变状态。
- **嵌套**：默认禁止（子代理规格不挂委派工具，pi 同款）；开放时 max=1 且深度持久化防 resume 绕过
  （dsh 默认）——随用例再立。
- **规格 `tools` 字段（2026-10-09 更新为三态）**：**缺席 → 继承父池**（父池已摘掉委派工具 = 禁嵌套；pi/harness 同款）/
  **白名单**（按名从父池映射）/ **`[]` 空集**（「不写 vs 写空」用 nil / 空 slice 分开表达）。
  「+search」= 保留名 **`tool_search`** 进列表（用户 2026-10-08 裁「子代理挂 search，当然要」；**实现随 T4 触发拍**）——
  搜索范围 = **自己的池**（统一规则见 §5）。T3 期池内工具**全声明**（无 deferred 机械件，分级随 T4 触发）。
- **预算面（2026-10-09 三裁）**——**取消是控制面的事（abort 级联），不是时间面的事**（用户裁，并指 Google ADK 同立场）：
  - **并发**：上限 = **配置项（默认 8）**（选择归 human 写配置、代码只做机械件）；pi 为硬编码常量（单批 8 / 同时 4），tako 无「单批」概念，只设「同时」一个旋钮。
  - **超时**：**不设总时限**、不另立计时器；要掐走将来的「取消」面（abort 级联；与 T1 遗留「abandon 不断流」是同一个待做面）。pi 实读（2026-10-09）：两套子代理实现均无计时器，唯一 kill 路径都是 abort 传播；时间上限只加在有界操作上（per-request / 单条命令）。
  - **回传裁剪**：终稿回传超 **50 KB 字节**即截（UTF-8 安全 + omitted 提示，照 pi）；全量在子代理自己的 runlog 行（tako 无 pi 的 tool details 通道）。
- 挂账：continuable/后台子代理（dsh send_message/jobs）、出进程 provider（acp/codex/claude-code）、
  **子代理目录的运行时安装**（Octop catalog/install）、**子代理运行中途改自己的工具集**
  （Eino `BeforeAgent` 可改、用例未逼出）——后两条 2026-10-08 用户裁「先别搞，记下来」。

## 5. 工具目录与 tool_search

- **施工触发（2026-10-09 裁）**：本节的分级与 tool_search **施工推迟到触发信号**——**MCP 工具 loop 进来**
  （工具多到撑上下文）或**子代理库长到几十条**，二者之一出现才动工；纸面保留分离原则
  「**可执行集合 ⊇ 声明集合 ⊇ 可检索集合**」。T3 期 = **一个源（文件）、一个装载点（run 起手）、一个声明状态（全声明）**。
- **分级 = node 全注册 + 声明过滤**：全部工具注册进 tools node（永远可执行），
  `BeforeModelRewriteState` 每轮筛 `state.ToolInfos` 决定模型看见什么——
  **direct** = 常声明 / **deferred** = 目录层、激活才声明 / **hidden** = 永不声明。
- **tool_search**：模型检索目录、命中即激活（下一次模型调用即声明）。T4 = 目录直出（名称/描述匹配）；
  **T5 = BM25 排序器，独立 task**（用户指令）——pi 版 40 行 TS（camelCase 分词+词干+停用词）的 Go 移植。
- **红线**（pi/dsh 同款收敛）：模型只能**装配目录内工具**，不能改 harness 本身。
- **统一搜索规则（2026-10-08）**：任何 agent（主或子）的 tool_search 只搜**自己 tools node 里注册的
  deferred 项**——搜索范围 = 可执行范围的子集，越界激活物理上不存在；主 agent 的 node 不含子代理
  专属工具，互相不可见。一条规则覆盖主从（子代理挂 search 见 §4 规格字段）。
- Eino 原生 `WithDeferredTools` + `WithToolSearchTool` 留作 provider 支持时的升级路——
  **支持度已查证（2026-10-08，矩阵与机制见 19v6-06 §4–5）**：eino-ext claude 组件 ✅ / openai 组件 ❌；
  Anthropic·OpenAI·DeepSeek·Kimi 各形态 ✅，Gemini/Qwen/豆包/GLM ❌。
  client 过滤（本条）为默认底座不变，原生通道按家适配时再接。

## 6. 表新增汇总

| 表 | 新增 | 落地拍 | 说明 |
|---|---|---|---|
| `run_model_messages` | `tool_calls` TEXT、`tool_call_id` TEXT | T2 ✅ | OpenAI assistant/tool 形状；§8.2 已预告 |
| `runlog_entries` | `agent` TEXT（空=root）、`payload` TEXT（JSON） | T2 ✅（两列，2026-10-08）/ T3（agent 非空） | payload 单列 TEXT：结构化字段种类差异大、稀疏列展不开 tool_calls 数组；「零 JSONB」原裁针对消息表（§8.6 收口） |
| `sessions` / `runs` | 不动 | — | fork 的 `forked_from` 列随 T2 树状会话 loop 再加（§7） |

## 7. 挂账（本 loop 不做）

- **fork（pi 会话树）**：用户裁「后面再做」（2026-10-08）。策略已备 = **copy-on-write**：新 session 物化源分支投影视图（只带已完成 run 边界，runs 表的 complete 闸天然切点）；
  真树（parent 链共享历史）撞「worker 热读单表无 JOIN」已裁约束，不取。
- **guard middleware**（allow/deny per agent、拒绝归一为 isError 结果、HITL ask 走 interrupt）：
  本 loop 只在 Handler 链留口，独立小拍。
- **「压 run 中间」**（C 系列 10.3-3 遗留）：run 内多段调用落地后回 C 系列议。
- ~~Eino 原生 deferred tools 的 provider 支持度查证~~ **已查证（2026-10-08）**，
  矩阵与机制见 `19v6-06` §4–5；默认底座维持 client 过滤。

## 8. 与既有篇的关系

- **19v6-01**：四个端点字节真源。本篇 §3 的帧扩展在 T2 同拍修订它（先改 doc 再动码）。
- **19v6-03**：存储形态真源。本篇 §6 的表新增是它的增量。
- **index 注意事项全部继续适用**：纯拉模型、拉取有界、闸在共享存储、runlog 只增不删——
  tool loop 不引入任何例外。
