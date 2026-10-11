# Handoff · 子代理设计素材（2026-10-08）

> 性质：**纯 handoff / 素材盘点**，不改任何代码。
> 触发对话（2026-10-08）：用户问「子代理在之前的项目文档里有没有说过怎么玩起来」→「写个关于 sub agent 设计的 handoff」。
> 用途：把「子代理怎么做」的全部已核实素材收成一份——接手会话拿本文可直接进 planning 对话，不必重跑本轮调研。
> 取信口径：tako 侧读 doc 原文；Octop/harness 侧读本地源码（`ref-agent/Octop` + `harness-pkg/harness-agent`）；
> Eino 侧读本地模块缓存 `eino@v0.9.21`。**全部为当日实读**；凡属推断的单独标注「推断」。

## 0. 一句话结论

- **「要什么」已写死**：doc/16 §3——问题（tool 撑爆主上下文）、Octop 答案（规格文件 + `task` 工具 + 上下文隔离 + 大文档三招）、tako 三个未答问题、下一步动作。
- **「怎么玩」从未写**：doc/16 §3 承诺的《doc/17-子代理实施计划》没有产出（doc 编号 16 → 18 跳过，尾部至今挂着「待出」）。**本交接即补这个缺口的第一层**。
- **素材三层齐备且全在本地**：
  - 功能形态 → Octop 壳（490 个规格文件的两层库 + catalog/install API + dashboard 呈现）；
  - 运行时机制 → `harness-pkg/harness-agent` 源码（deepagents 路径，含三条编译期隔离规则）；
  - Go 落地方式 → Eino v0.9.21（`adk/prebuilt/deep` 内置 `task` 工具 + `adk.AgentTool` 底层件，**两层都现成**）。
- **v6 里的位置**：子代理是 B 桶（从未做过，gap handoff §2.1）；**其前置 = v6 工具循环**（gap handoff §3 候选 1）——「tool 变子代理」要先有 tool，而 v6 现在没有任何工具面。

## 1. tako 侧已写下的

### 1.1 doc/16 §3「当前重心：子代理（#5）」（唯一实质设计）

原文要点（doc/16-能力复刻总规划.md L116-130）：

- **要解决的问题**：某些 tool 会把主 agent 上下文撑爆（疯狂刷 tool 输出、读大文档、翻一堆文件）。
- **Octop 的答案（=要什么）**：
  - 子代理 = 一个 Markdown + YAML frontmatter 规格文件（`name/id/description/tools/model/skills`，正文即 system_prompt），
    运行时被框架包成一个 **`task` 工具**挂到主 agent 上——「tool 变子代理」。
  - **上下文隔离**：调用方只能看到子代理**最终那条 assistant 消息**，中间 tool 调用不回传；子代理有自己独立的上下文窗口。
  - **大文档三招**：`read(offset, limit)` 按行分页读；正本存 workspace 文件系统（外部存储），上下文只放路径/占位；
    需要通篇理解时派子代理读完回摘要。
- **tako 怎么做（=Eino 落地，待出实施计划）**——三个当时挂起的问题：
  1. Eino 里怎么让一个子 `ChatModelAgent` 以 tool 形态挂到主 agent 上，并做到上下文隔离；
  2. 子代理规格怎么定义/注册/发现（Markdown+frontmatter 还是 Go 结构体）；
  3. 与已有能力的接缝：子代理是否继承 guard 三件套？（Octop 里子代理**不继承**父 middleware、剥离 ask_user）；子代理产出如何进 taskboard / event_log。

> 注：§1.1 第 3 问里的「已有能力」（guard 三件套、taskboard）是**旧线口径**——v6 里它们同样待重建（gap handoff §2.0），所以这两条接缝在 v6 是「未来物接未来物」，到点再合。

### 1.2 散落在各处的相关论断（均早于 doc/16，仍然成立）

| 位置 | 原文摘要 |
|---|---|
| doc/03 §5 L104 | 产品能力路线顺序：任务看板 → 上下文管理 → **子代理** → 记忆 → 断点恢复与流式 → 多入口统一路由 → 多用户/多 Agent |
| doc/05 §2.2 表 L138 | 能力表 #5 子代理：参考实现 `subagents/` + ADK，状态 ⬜ |
| doc/05 L146 | 节奏纪律：「一层没站稳不跳层」 |
| doc/08 §4 L125 | **「子代理优先用 DeepAgent 的 `TaskTool`（隔离上下文，硬隔离软调度）」**——机制级论断（要的是隔离语义）；旧线产品从未采用 `deep.New` 整包（2026-10-08 专项核查，见 §3.4） |
| doc/04 L110 | 「（子代理、断点恢复、多入口）会反复碰到」Graph 管确定性 / Agent 管自主性的分界问题 |
| doc/16 §5 L146 | T1 排序：**子代理（#5）← 当前重心。借鉴 Pi 工具即子代理挂载 + Hermes skills_hub 注册/演化** |

### 1.3 doc/17 从未产出

- `doc/` 现编号：…15、16、**18**（媒体落盘与图片读取）、19-v5/19-v6 ——17 缺号。
- doc/16 §3 尾句仍写着「下一步动作：去 harness-pkg/harness-agent + Eino ADK 摸清…出 `doc/17-子代理实施计划.md`」。
- **本轮 handoff 事实上完成了那句话的前半段（摸清）**；后半段（实施计划）属于下个 planning 对话。

### 1.4 与 v6 的关系（gap handoff 口径）

- `docs/2026-10-07-v6-gap-handoff.md` §2.1：子代理在 **B 桶（从未做过）**，T1 三项之一。
- §3 候选 1：**工具循环 + 基础工具集是 T1 三项（子代理 / 记忆 / context_usage）共同的前置**；
  第一个要拍的问题 = 循环走 Eino ADK / 手写 ReAct / 在 v6 worker 上扩展——它决定 tool 消息与 runlog 扩展行形态。
- 即：**子代理立项前必须先有「v6 的工具循环」**，这是本文与 gap handoff 的交叉点。

## 2. Octop / harness 实测（功能形态 + 运行时机制）

两个仓库的分工：`ref-agent/Octop` 是应用壳（13.1 万行，管理面 + API + dashboard），
真正的 agent 核心在 pip 包 `orcakit-harness-agent>=1.0.14`（4.4 万行）——**本地源码快照在 `harness-pkg/harness-agent/harness_agent/`**，以下机制全部实读自此。

### 2.1 两层结构：bundled library 与 workspace

| 层 | 位置 | 内容 |
|---|---|---|
| 内置库（bundled） | `Octop/src/octop/infra/agents/subagents/library/{en,zh}/<division>/*.md` | **490 个 .md 规格**，20 个 division（academic / design / engineering / finance / game-development / gis / hr / legal / marketing / paid-media / product / project-management / sales / security / spatial-computing / specialized / supply-chain / support / testing），来源 agency-agents 项目，en/zh 双语文档对 |
| agent 工作区（per-agent） | `agents/**/*.md`（harness workspace 内） | 每个 agent 实际生效的子代理定义；**删文件 = 卸载**（再 `reload_subagents()` 或重启生效） |

catalog.py 细节：**slug 取文件 stem、不取 frontmatter `name`**（保证翻译对与源文件稳定配对）；zh 未翻译的占位符（`TODO_TRANSLATE`）不进 catalog；请求 locale 无译文时**不做跨 locale 回退**（直接不列出）。

### 2.2 规格文件格式（canonical 表在 `harness-pkg/.../builtin/agents/README.md`）

| 字段 | 必填 | 语义 |
|---|---|---|
| `description` | **是** | 主 agent 何时该派它的路由依据（`task` 工具的描述里就列这个） |
| `name` | 否 | catalog 显示名 |
| `id` | 否 | **`subagent_type`**；缺省时从文件路径推导（loader 里 `meta.get("id") or slug_from_agent_path(...)`；此 slug 即运行时 `SubAgent["name"]`） |
| `emoji` | 否 | catalog 图标，缺省 🤖 |
| `model` | 否 | `provider:model` 覆盖 |
| `tools` | 否 | 四种形态：省略 / `inherit`（= 继承父池）；白名单列表；`[]`（空集） |
| `skills` | 否 | 该子代理的技能根目录 |

正文（frontmatter 之下）= `system_prompt`；正文为空时**回退用 description 充当**。

> 对比 doc/16 §3 记的字段表（`name/id/description/tools/model/skills`）：**少了 emoji，其余准确**。
> 权威以 harness README 为准。

### 2.3 注册 / 发现 / 安装链路（Octop 壳）

`Octop/src/octop/api/routers/subagents.py`（router docstring 自述：per-agent 定义在 `agents/<slug>.md`，本 router 给 dashboard 供摘要）：

- `GET /subagent-catalog` · `/subagent-catalog/divisions` · `/subagent-catalog/{slug}` —— 浏览内置库；
- `GET /agents/{agent_id}/subagents` —— 某 agent 已装入的子代理；
- `POST /agents/{agent_id}/subagents/install` —— **把内置库里的定义「复制」进该 agent 的 workspace `agents/` 目录**（handler docstring 明说 "Copy a bundled subagent definition into the agent workspace"），带 locale 解析。

**安装 = 复制粘贴为文件；卸载 = 删文件**。运行时无注册表——每次 compile/reload 时扫盘。（推断：这条「文件即注册表」是该系统的简洁性来源，也是 tako 抄规格格式时的主要候选。）

### 2.4 运行时机制（harness_agent，deepagents 路径）

装载链：`subagents/loader.py` 扫描 workspace → `parse_agent_markdown` 产出 deepagents `SubAgent` dict
`{name, description, system_prompt, [model], [skills], [tools]}` → 传给 deepagents 的 `create_deep_agent`（harness 是其门面封装）。
合并策略：多个扫描根 + config 合并（**同名时 config 胜**）；`subagents_auto_load` 开关控制自动装载。

`tools` 字段四态解析（`_parse_tools_meta`）：`None`/空串 / `inherit` / `default` → 返回 None（**= 继承父工具池**）；
列表 → `resolve_subagent_tools` 按名字从**父池**映射（不认识的工具名记 warning 后跳过）；`[]` → 空列表（真的什么都不给）。

**编译期三条隔离规则**（`agent.py _resolve_subagents`，L1596 起）：

1. **子代理不继承父的 middleware 链**（源注释：Subagents do not inherit the parent's middleware chain）；
2. **MCP 工具在 compile 时剥掉**；
3. **`ask_user_question` 剥掉**（源注释：子代理是没有父 HITL 配置的嵌套图，永远到不了人）；
   省略的 tools 继承父集后重新 pin 住。

配套：`middleware/tool_order.py` 的 `TaskToolLastMiddleware`（docstring「Stable model-facing tool ordering for parent/subagent cache reuse」）——
把 `task` 工具排到父/子共享工具**之后**，让父与子前缀相同、命中 prompt cache；`cfg.task_tool_last` 默认 True。

呈现语义：**只回最终 assistant 消息**（中间 tool 调用不回传调用方）；`task` 工具本身由 deepagents 提供。

### 2.5 壳与库的分工（一句话）

Octop 壳管「**规格从哪来、装没装、长什么样**」（catalog/install/dashboard）；
harness 库管「**规格怎么变成运行时行为**」（扫描/解析/合并/隔离/排序）。
tako 若要抄，这两层可以合并为一层（tako 没有 dashboard 浏览内置库的需求场景），但**「规格文件」与「运行时装载」的解耦**值得保留。

## 3. Eino 侧实测：官方 Go 支持（v0.9.21，本地模块缓存）

### 3.1 `adk/prebuilt/deep` = deepagents 的 Go 同位物（`~/go/pkg/mod/github.com/cloudwego/eino@v0.9.21/adk/prebuilt/deep/`）

`TypedConfig` 关键项（deep.go）：

- `SubAgents []adk.TypedAgent[M]` —— 子代理出口；**内置 `task` 工具**（`taskToolName = "task"`，types.go L29）：
  参数 `subagent_type` + `description`，按名字路由到子代理（task_tool.go InvokableRun）。
- **`general-purpose` 子代理自动创建**（`generalAgentName`，types.go L28）：不设 `WithoutGeneralSubAgent` 时，
  用主 agent 同款 model/instruction/toolsConfig/middlewares 造一个通用子代理并入 task 表（deep.go L130-136；task_tool.go L85-112）。
- `TaskToolDescriptionGenerator` —— 定制 task 工具描述；缺省实现 = 把每个子代理 `- name: description` 列进描述（task_tool.go L177-191）。
- 自带 `write_todos`（`WithoutWriteTodos` 可关）、文件系统 Backend（`read_file/write_file/edit_file/glob/grep`）、
  Shell / StreamingShell（`execute` 工具）、MaxIteration。
- **task 工具的 prompt 追加**（prompt.go `taskPrompt`）：内建一段「何时用 task、何时不用（要看中间步骤就不要派、
  琐碎任务不要派）、鼓励并行」的指令，作为 middleware 挂上去（task_tool.go L53-58 `typedBuildAppendPromptTool`）。

### 3.2 `adk.AgentTool` = 把任意 agent 包成 tool 的底层件（`adk/agent_tool.go`）

- `NewTypedAgentTool`（L107）：任意 `TypedAgent` → `InvokableTool`。
- **默认就是上下文隔离**：调用只把单个 `request` 参数作为 user message 发给子 agent
  （`defaultAgentToolParam`，L33）；要反向（全量历史喂进去）得显式 `WithFullChatHistoryAsInput()`（L49-50）。
- **只回最终文本**：内部跑到结束，取最后一帧 `ret = extractTextContent(msg)`（L275），中间过程不出。
- **interrupt 穿透**：子 agent 挂起 → `tool.CompositeInterrupt`（L260）冒泡到父层，跨 agent 边界 resume——
  即 **HITL 接缝已存在**（docstring L83-87）。

### 3.3 两条路的关系与选型空间

- **实质关系**：`deep` 的 task 工具**内部就是用 `NewTypedAgentTool` 把每个子代理包成 InvokableTool**
  （task_tool.go L106/L116），再包一层「按 subagent_type 路由 + 描述生成 + prompt 追加 + general-purpose 兜底」。
- 所以选型本质是**要多少这层壳**：
  - **A. 用 `deep.New` 整包**：白拿 task 工具、prompt 追加、general-purpose、write_todos、文件系统工具——
    代价是接受它的默认假定（deepagents 世界观：文件系统 Backend、todo 工具形态、prompt 措辞）。
    粒度止步于此：deep 的导出面只有 `New`/`NewTyped` + `Config`（**task 工具本身 unexported**），
    `WithoutWriteTodos` / `WithoutGeneralSubAgent` 只能各关一件默认物，「只拿 task 工具」的中间路不存在。
  - **B. 手拼 `AgentTool`**：自己把子 `ChatModelAgent` 包成 tool、自己写工具描述与路由——全控，但 prompt 追加、
    路由表、general-purpose 兜底都要自产。
  - **C. 自研**（不推荐起点）：以上两层的存在使自研的增量收益只可能来自非常特殊的拓扑需求。
- 与 tako 现状的耦合点：v6 主循环还没定（gap handoff §3 候选 1：Eino ADK / 手写 ReAct / worker 扩展），
  **主循环选型直接决定这里选 A 还是 B**——若主循环走 Eino ADK，A 几乎免费；若手写 ReAct，可能只剩 B 可用。

### 3.4 旧线实践与 `deep.New` 的关系（2026-10-08 专项核查）

> 触发：本 handoff 初稿曾把 doc/08 §4 的机制注引作 `deep.New` 整包的「选型倾向」。
> 专项核查（现行 doc、git 历史里的已删旧文档、全仓代码含全历史、会话记录）结论如下。

- **不存在成文的「DeepAgent 不推荐」裁定**——上述四处都没有这句话。
- **但产品线确未采用 `deep.New` 整包**（这是事实，不是裁定）：
  - `cmd/yktako` 在其**全历史**（旧线 + v6）里对 `deep` 包零引用（`git log --all -S "deep"` 该路径无命中）；
  - `deep.New` 只出现在两个对照/演示入口：`cmd/l0deep`（概念演示）与 `cmd/l2builtin`（官方 `write_todos` 软对照），
    二者随 2026-09-26 单入口收敛删除（`1d0d1eb`；退场记录见 doc/07 §3.2）；
  - 产品形态自述：**裸 ReAct `ChatModelAgent` + 自研 taskboard + guard 三件套**（旧线 backup README，已随仓体清障出仓）；
  - 两处成文的**机制级替代**：doc/01 L9「taskboard 替代 DeepAgent 的整表 `write_todos`」；
    doc/08 §4 L122「用 DeepAgent 就接受和 Octop 一样的"自觉"——清单是软约束，别试图把它改硬」（代价告诫）。
- **doc/08 §4 L125 的正面提法未落地**：它推荐的是**隔离语义（机制）**，不是「采用 `deep.New` 整包」；
  且与它同段列出的硬编排（`planexecute`）后来已被移出产品形态（doc/08 §4 更新段）。初稿把机制注读成整包倾向，属过度解读。
- **对 v6 的含义**：以上均为**旧线事实陈述，既非推荐也非禁令**。
  §3.3 的 A 选项只是「Eino 有一条现成路径」的事实；采不采用由 v6 的主循环选型（§5 问题 1）重新拍。

## 4. 旧线痕迹（历史诊断专用，**不作施工面**）

旧线（v1–v5，代码已随 backup 出仓）实际跑过子代理——DeepAgent 的 `TaskTool` + `AgentTool` 都在用，
留下三条「跑起来后才知道」的记录：

1. **runner 会 emit 整个 agent 层级的事件**（子代理、`AgentTool` 包住的内部 agent 都有自己的 ReAct 循环）——
   所以历史必须按 root 过滤：只有 root agent 的消息能进返回给调用方的 history
   （`event.AgentName == root`，doc/09 §5），**否则子代理内部的 tool 消息混进来 = 「没有对应 tool call 的 ToolMessage」= 接口直接 400**。
2. **root 参数承载两件事**：筛历史 + 给非 root 事件加 `[子代理名]` 前缀（REPL sink，doc/09 §5）。
   它后来从「脚本手写」改为从 agent 自己身上取（对不上时静默出错，属于最难查的错）。
3. **呈现侧**：`message` 事件带 agent 名，子代理带前缀（doc/10 §4.2）；前缀被明确归为「修饰，不是独立分支」（doc/11 §4.2 第 5 条）。

**对 v6 的含义（推断）**：v6 是纯拉模型 + frames/runlog 两平面，第 1 条对应的新问题是
「子代理中间过程进不进 frames / runlog」——旧线的答案（全 emit、消费端过滤/加前缀）不能直接搬，
因为 v6 的 frames 是 run 级终态可 gone 的临时平面、runlog 是要长期正确对齐的定稿面。这正是 §5 问题 4。

## 5. 未答问题清单（给 planning 对话，不预拍板）

1. **Eino 选型从属于主循环选型**：`deep.New`（选型 A）还是手拼 `AgentTool`（选型 B）——先拍 v6 工具循环走哪条线（gap handoff §3.1），再回来拍这个。
   事实背景（§3.4）：旧线从未采用整包，**A 没有「惯性」可沿**——选 A 要当新决策来做；B 同样不是默认。
2. **规格格式与发现**：抄 Octop 的 Markdown+frontmatter（文件即注册表、删除即卸载），还是 Go 结构体（编译期注册）？
   若抄文件式：放哪（仓库内嵌 / workspace）、何时扫描（启动 / 每 run）、无效规格怎么办。
3. **隔离边界到底切在哪**：子代理继承什么、不继承什么（model？工具池？guard？）——harness 的答案（不继承 middleware、
   剥 MCP 与 ask_user、tools 默认继承父池）是 deepagents 语境的答案；tako 的 guard/approval 还没在 v6 重建，接缝到点再合。
4. **呈现**：子代理中间过程进不进 frames / runlog？进则怎么标（agent 名？嵌套结构？）；
   不进的话「过程不可见」是否接受（隔离本就是目的，Octop 也不回传中间步）。
   ——旧线 400 事故（§4 第 1 条）是 v6 设计这条时必须内化的约束。
5. **存储**：子代理的模型消息落哪？现 `run_model_messages` 是「单 run 内、单 agent 序列」的假设
   （role 枚举已锚 OpenAI 标准、`msg_<ULID>` 预留 tool_call/HITL 引用；见 `pgstore.go` 注释）——
   子代理独立上下文窗口与这张表的关系待定义；崩溃恢复时子代理算什么（gap handoff：崩溃续跑本身在 T3）。
6. **命名冲突**：tasktracker 的四个模型可见工具（TaskCreate/TaskUpdate/TaskList/TaskGet，见 `doc/XX-taskboard-重命名-handoff.md`）
   与子代理 `task` 工具——模型可见空间里两个「Task」语义并存，需裁定（改名？接受并存？）。
7. **并发与预算**：Eino 的 `taskPrompt` 明确鼓励并行派发（一条 assistant 消息多个 tool_call）——
   tako 要不要跟？子代理并发上限、超时、token 预算、递归深度（子代理能否再派子代理）怎么定。
8. **顺序**：doc/16 §5 排的 T1 顺序（子代理 → 记忆 → context_usage）与 gap handoff §3 的「工具循环是三者共同前置」如何编排进同一条施工线。

## 6. 素材位置索引

| 素材 | 位置 |
|---|---|
| tako 设计原点 | `doc/16-能力复刻总规划.md` §3（+ §5 排序）；doc/08 §4；doc/03 §5；doc/05 §2.2 |
| v6 现状与差距口径 | `docs/2026-10-07-v6-gap-handoff.md` §2.1（B 桶）/ §3（前置=工具循环） |
| 旧线子代理痕迹 | `doc/09-设计了解.md` §5（root 过滤/前缀）；`doc/10` §4.2；`doc/11` §4.2 |
| Octop 壳（库+API） | `ref-agent/Octop/src/octop/infra/agents/subagents/`（catalog.py + library/ 490 md）；`ref-agent/Octop/src/octop/api/routers/subagents.py` |
| harness 运行时 | `harness-pkg/harness-agent/harness_agent/subagents/loader.py`；`.../agent.py` `_resolve_subagents`(~L1596)；`.../middleware/tool_order.py`；`.../builtin/agents/README.md`（权威字段表）+ `en/general-purpose.md`（默认种子） |
| Eino 深挖入口 | `~/go/pkg/mod/github.com/cloudwego/eino@v0.9.21/adk/prebuilt/deep/`（deep.go / task_tool.go / prompt.go / types.go）；`.../adk/agent_tool.go` |
| v6 存储接缝 | `yk-tako-eino/internal/runengine/pgstore.go`（`RunModelMessage.Role/ID`、`RunlogEntry.Role` 注释） |

## 7. 纪律提醒（给接手会话）

- **本文是素材不是计划**：进 planning 对话后按 AGENTS.md 硬闸门走（planning.md + todos.md 交审 → 点头 → 第一 action 必须是 curl 能打到的端点）。
- **承重论断先验前提**：引用本文时，机制级事实（§2/§3）都有源码坐标，可直接复查；
  标「推断」的两处（§2.3 尾、§4 尾）不可当裁定用。
- **doc/16 §3 的三问里，第 1 问已被本轮调研回答**（Eino 两层都现成，§3.1/§3.2）；第 2、3 问与 §5 清单一起留给 planning。
- 端口/环境/命令纪律见 `docs/2026-10-07-v6-gap-handoff.md` §4。
