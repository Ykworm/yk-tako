# 83 配套 · 企业级 Agent 课程对 Tako 的借鉴分析

> 性质：**课程研读与方案分析（待点头）**。按用户提供的完整课程页面，逐篇逐课判断哪些方法可用于 Tako、哪些已有对应实现、哪些需要改造、哪些属于其他 Agent 场景。
> 状态：⬜ 未施工。本文不取代 [doc/83-场景化Eval方案](README.md)、[doc/56](../56-Agent测试与基准选型.md) 或 [Anthropic 思路稿](Anthropic思路稿.md)，不改变施工真源口径。
> 课程材料：`tmp/geekbang-agent-42/课程内容.md` 及其中四张技术长图（三阶能力、六大架构、DataAgent 项目、六类交付物）。营销、师资、价格和退款信息不属于技术分析范围。
> 证据边界：课程覆盖范围可以确认，实际讲授深度仍需看参考实现、Fixture、Grader、故障案例和发布实验，不能只凭大纲断言内容质量。Tako 功能状态以现行代码、配置和已验收文档为准。
> 日期：2026-11-16。

## 0. 总结论

这套课程以多租户零售经营分析 DataAgent 为项目，但核心不是 GMV、SQL 或经典 RAG，而是三条通用工程线：

1. **Runtime**：把开放式任务收进可控制、可停止、可审批、可恢复的执行过程；
2. **Memory**：规定长期记忆怎么写、怎么召回、怎么改口、怎么删除，以及如何防止跨用户污染；
3. **EvalOps**：把“定义质量 → 准备可复现环境 → 自动评分 → 诊断失败 → 发布判断 → 失败回流”变成长期运行的工程流程。

读完十篇后的修正判断是：

- Tako 已经覆盖课程中相当多的 Runtime、长任务、上下文、工具和 Memory 机制，不能按课程目录重新施工一遍；
- **最直接、最缺的仍是第九篇的场景 Eval 与失败诊断闭环**；
- 第六篇的 Context Decision Contract、候选信息过滤证据和 Context Eval，是 Eval 之外最值得继续对照的部分；
- 第四篇的自动模型路由、Fallback 和统一可靠执行器有价值，但应由真实失败与成本数据驱动，不应为对齐课程而新建模型网关；
- 第七篇的通用 Action Runtime 适合真正写外部业务系统的 Agent；Tako 当前不是预算调整／工单／支付 Agent，不能把通用工具审批重新引入；
- 第五篇的“统一执行模型”不能抹掉 Tako 普通 run、同步子代理、持久 Task、后台 bash 和记忆作业的真实差异；
- 第五篇之后的 Shadow、Canary、Kill Switch 和资产恢复属于生产发布体系，不是 Eval 第一版的完成条件；
- 这些原则可迁移到 3D／多模态 Agent，但课程不提供空间智能、多模态感知或其专用评分方法。

一句话概括：**值得学的是“可控 Runtime＋可信证据＋EvalOps”的工程骨架，不是 DataAgent 的业务题，也不是把课程名词变成 Tako 的新模块清单。**

## 1. 从完整页面看到的课程骨架

### 1.1 三阶能力

课程长图把能力进阶分为：

| 阶段 | 课程含义 | 对 Tako 的对应动作 |
|---|---|---|
| 构建可运行 Agent 系统 | 理解模型、状态、上下文、工具、记忆与评测如何组成长期运行系统 | Tako 已跨过“只有模型调用”的阶段，应对账现有系统而非重写 |
| 设计架构与边界 | 拆分任务中的确定性、不确定性和风险，分配代码、模型与人的控制权 | 用现有权限、沙箱、计划审批、Task 和结果发布验证边界 |
| 用评测持续改进 | 把失败变成数据，把 Trace 变成证据，把优化变成可重复实验与受控发布 | 这是当前最应补齐的一层 |

### 1.2 六大核心架构

| 架构域 | 课程关注点 | Tako 当前判断 |
|---|---|---|
| 模型运行时 | Provider 抽象、结构化输出、Streaming、Retry、Fallback、成本与回放 | 有 Provider／模型解析、流式与用量；自动路由和完整降级链不是现有能力 |
| 智能体运行时 | Loop、Workflow、Run／Step／Event、Checkpoint、Scheduler、Worker、Sandbox | 已有较多对应机制，需保持不同执行原语的边界 |
| 上下文运行时 | Context Schema、动态装配、压缩、Token Budget、证据与 ContextOps | 有折叠、溢出、media、记录块；候选过滤证据与按决策契约仍可加强 |
| 工具与能力 | Tool、CLI、Skill、MCP、发现、权限、审批、凭据隔离与 Tool Eval | 工具／技能／MCP 已有；通用业务 Action Runtime 不是当前产品主线 |
| 记忆治理 | 分层、写入校验、合并、召回、修正、删除、审计与 Memory Eval | 双模式记忆已落地；评测和使用过程证据仍需补齐 |
| 评测运营 | Outcome／Trajectory／System、Grader、Trace、Replay、回归与灰度 | 契约验收和公开基准已有，场景 Eval 与生产发布闭环未建 |

### 1.3 六类交付物对 Tako 的启发

| 课程交付物 | 内容 | Tako 已有／可复用 | 真正欠项 |
|---|---|---|---|
| 可运行系统 | 执行任务、业务分析、证据链、人工审批、多租户配置 | 已有可运行 Agent Runtime | 不需要另做 DataAgent 演示系统 |
| 架构设计包 | 架构图、任务状态机、关键链路时序 | `doc/19-v6`、架构说明页、70～82 系列 | 持续同步而非重新画一套 |
| 决策证据包 | ADR、Decision Matrix、Badcase、Benchmark | `doc/problems/`、`scripts/bench/` | 将场景失败与对照实验关联起来 |
| 治理资产包 | Policy、Schema、Contract、Trace、权限和行动策略 | 已有大量权限、工具、记忆和 Trace 契约 | 资产版本元组与评测覆盖 |
| 质量保障包 | 故障注入、红队、Regression 与发布判断 | 单元、契约、kill -9 活体、公开基准 | 场景 Eval、Grader 校准与统一报告 |
| 上线方案包 | 监控、Canary、Kill Switch、Rollback | 有运行观测与局部停止／恢复 | 等真实生产流量后建设 |

## 2. 为什么 Eval 必须与 Runtime 证据连接

假设某次修改了上下文折叠代码：单元测试全绿，API 契约验收全绿，GAIA 分数甚至提高，但折叠之后用户身份证明文重新进入模型输入。

再假设另一场长任务中，助手已经告诉用户“结果交付完成”，项目里却只有半成品，恢复过程中还发出了第二次通知。

接口形状正确、公开题分数较高、模型自述完成，都不能证明产品可靠。需要接通：

```text
用户发任务
   ↓
Tako Runtime：模型、工具、记忆、技能、Task、审批、恢复
   ↓
同时留下两类产出
├─ 最终结果：文件、代码、数据库状态、任务产物
└─ 执行证据：frames、runlog、Trace、journey、审批与通知记录
   ↓
Eval：检查最终结果、必要轨迹和系统代价
   ↓
结果报告 + 失败归因 + 回归结论
   ↓
发布系统消费结论：允许、拒绝或标记“本次未验证”
   ↓
真实失败缩成可重跑 Case，回到评测集
```

责任分工是：Runtime 执行并留下证据；Eval 站在引擎外读取结果，不替 Agent 完成任务；发布系统消费结论，Eval Runner 不自己灰度或回滚。没有这条链，修复一个问题也可能悄悄破坏另一项已有能力。

## 3. 逐篇逐课分析

### 第一篇　AI 核心认知：建立模型与系统边界

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 从训推机理理解 LLM | Token、训练、SFT／RLHF、prefill／decode／KV cache、采样、幻觉、ICL、窗口与注意力 | **值得学原理，不是功能施工清单。**它能解释成本、时延、结构化输出与不稳定性的来源；Tako 不应自行实现 Tokenizer 或训练框架，而应把模型不确定性交给契约、工具和验证兜底 |
| 2. Autonomous 与 Workflow 范式 | ReAct、Plan-and-Execute、Prompt Chaining、Routing、Parallelization、Orchestrator-Workers、Evaluator-Optimizer | **直接有用。**Tako 已是模型工具循环＋计划＋持久 DAG＋子代理的混合系统；应按任务选择最简单形态，不为覆盖范式而新增固定 Prompt Chain |
| 3. Agent 工程师能力模型 | 技术、架构、职业责任；L1 功能到 L4 平台演进 | **适合作为学习自评，不是 Tako 功能。**可转成“能否解释、能否写 Case、能否做对照、能否证明发布安全”的能力检查 |
| 4. AI Native 团队与个人 | 从完成任务到对结果负责；个人／团队规则、SPEC、Skill、验证与资产隔离 | **部分有用。**Tako 的租户、Agent 规格、技能和验证资产可复用；组织方法和“超级个体”叙事不应变成产品需求 |
| 5. PE／CE／HE／LE／GE | Prompt、Context、Harness、Loop、Graph 五种调整系统行为的层次 | **可当作课程自己的分析框架。**实际操作是：先改任务表达，再改信息，再改工具执行壳，再改循环，最后才改图；这些缩写不应冒充行业统一标准 |

本篇最重要的落点是“模型与确定性系统的职责边界”：模型适合理解、规划和生成；权限、状态、预算、幂等、验证与恢复必须由可检查机制承担。

### 第二篇　企业级 AI Coding：把概率生成变成可验证交付

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 道：生产观、角色观、责任观 | 工程系统决定质量、总交付成本、风险适配自动化、人的审查责任 | **直接认同。**与 Tako“不靠模型记性、不靠自述完成、危险动作受控”的方向一致；不等于所有动作都要人审 |
| 2. 法：SPEC 到 Deliver | SDD、最小充分 Context、Contract-First、Invariant、Task Graph、Test／Eval-Driven、Loop、Independent Verification、Progressive Delivery | **高度可用。**与仓库“契约→实现→验收”和场景 Eval 相合；可补的是每个 Case 的不变量、独立 Grader 和参考解法 |
| 3. 术：Agent 驾驭与编排 | Prompt、Context、任务拆解、Loop、Hook、Skill、SubAgent、验证恢复与成本 | **可用于调优实验。**每个技巧都应通过 A/B 证明净收益，不能把“更多技巧”当成质量提升 |
| 4. 器：可治理 Coding Harness | Agent-Friendly Repository、Context Manifest、Worktree、密钥治理、Trace、资产平台、Badcase 回流 | **Tako 已有大半底座。**仓库规则与文档真源、沙箱、worktree、技能、Trace 可复用；Context Manifest 与资产版本元组值得补，密钥不能进报告 |

“独立验证”尤其重要：写代码的 Agent 说测试通过不算证据，Eval 要重新读取产物并运行检查；但也不能把发布验证和评分混成同一个会修改运行结果的组件。

### 第三篇　DataAgent 项目立项：学方法，不搬业务

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 多租户零售经营分析 DataAgent | 查询订单／库存／广告，SQL＋Python 分析，生成建议，高危写入审批，周期报告，租户配置；M0 质量基线与 ADR | **业务本身不搬。**可借鉴模块化单体起步、M0 基线、Decision Matrix、Badcase 与 ADR；Tako 的 M0 应是文件、研究、记忆、长任务等真实任务的结果／成本／安全基线 |

DataAgent 的四步业务链是“查询定位异常 → 分析原因形成证据 → 建议经审批执行 → 持续监测追踪效果”。Tako 对应的是“接收任务 → 用工具形成产物与证据 → 必要审批／验证后发布 → 后台跟踪与恢复”。两者共享控制结构，不共享指标语义。

### 第四篇　LLM Runtime：先区分现有适配与未建的治理

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 统一 LLM Runtime | Provider Adapter、统一调用契约、能力差异、Registry、Routing、Execution、Observability | **部分已有。**Tako 有厂商目录、OpenAI／Anthropic 协议适配、租户模型解析、客户端缓存和 Trace；不应把缓存 Registry 误称为自动路由器 |
| 2. 多模型路由 | 逻辑／物理模型、Capability、Task Profile、Fallback、成本与租户策略 | **未来有用，当前非最优先。**Tako 已能按租户、Agent、Task 指定 modelRef，但不是按任务能力自动选模型；先做配对 Eval，再决定路由策略 |
| 3. 可靠模型执行 | Logical Call／Attempt、Retry、Fallback、Timeout、Deadline、Schema Validation、流式与最终提交 | **值得逐项对账。**Tako 有 run／epoch、流式帧、部分超时与错误处理；不能用 Task attempt 代替模型调用 attempt，也不能假定所有模型错误已有统一重试与降级 |
| 4. 模型治理 | Trace、成本、多租户、版本、路由策略与 Replay | **观测已有，治理未齐。**用量与 OTel 已落地；模型／策略版本应进 Eval 版本元组，Replay 不等于重跑得到相同输出 |

本篇可借鉴的是“模型调用也是可观察、可验证的执行行为”，不是立即增加自动 Fallback。错误重试可能放大成本或重复副作用，必须由错误类型与幂等性决定。

### 第五篇　Agent Harness 与 Runtime：Tako 已有基础最多的一篇

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 混合架构选型 | Task Contract、范式选择、控制权矩阵、Graph、Verifier、Human Gate、Autonomy Budget | **直接用于评测现有系统。**Tako 已有计划、Task 图、审批、验证和沙箱；run 级原生自主预算仍是独立缺口 |
| 2. Autonomous Harness | Plan／Replan／Verifier、Stop Condition、Analysis State | **已有对应机制。**计划审批、同组修订、继任组与结果验证可评测；Analysis State 不能简单等同于聊天记录 |
| 3. 统一执行模型 | Run／Step／Attempt、Event／Checkpoint、状态机、Run Timeline | **映射而非强行统一。**普通 run、同步子代理、Task、后台 bash、记忆作业语义不同；可统一证据字段，不必统一为一种存储和状态机 |
| 4. Durable Workflow | Checkpoint／Resume、Human Approval、副作用不重复 | **Tako 已有长任务持久化与恢复。**已有结果的工具可回放，intent 无结果的调用不能盲目重做；README 明确不承诺所有外部副作用 exactly-once |
| 5. 分布式 Runtime | Scheduler、Queue、Worker、Lease、Heartbeat、多租户资源治理 | **已有较多实现与双副本验收。**应继续测接管、过期执行者防写和容量上限；容量限制不等于完整公平调度 |
| 6. 故障注入 | Failure Taxonomy、Retry／Resume／Replay／Fork、Runtime Invariant、Run Timeline | **直接可用。**把已有 kill -9 活体扩成统一失败分类；恢复、重跑、证据回看和中间分叉必须分开命名 |

本篇的关键不是“再做一个 Runtime”，而是证明以下不变量：旧执行者不能写、等待现场不丢、通知不重复消费、发布结果完整、恢复不自动重放未知副作用。

### 第六篇　Context 治理：Eval 之外最值得继续深入的一篇

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. Context Runtime 与决策信息契约 | 每个 Step 必须／可以／禁止看到什么；Context Schema 携带来源、版本、权限、可信度和生命周期；缺关键输入则停止 | **很值得对照。**Tako 有统一模型输入转换、历史／记忆／技能装配，但尚未把每类决策的信息契约与缺失拒绝统一表达 |
| 2. 经营语义上下文 | Metric Registry、术语别名、粒度、时区、币种、血缘、权限、新鲜度 | **DataAgent 专用。**Tako 可借鉴“先确认定义再使用数据”和“权限先于相关性”，不需要 GMV 指标注册中心 |
| 3. 动态装配与预算 | 硬过滤／软排序、必需信息优先、分区 Token Budget、去重冲突、Artifact 引用、充分性检查 | **高度相关。**Tako 已有 compact、溢出修复、media、记录块；可补“为什么这些信息被选中／被挤掉”的证据与关键信息保留评测 |
| 4. Agentic Evidence | 按信息缺口搜索、搜索预算、假设／证据／结论分离、Claim-Evidence 关系 | **适合 researcher 与报告任务。**现有 web_search／web_fetch 可支撑；Eval 应检查结论是否有来源，不要求固定搜索路径 |
| 5. 长任务上下文 | History／State／Checkpoint／Summary 分工、生命周期、压缩不变量、Artifact、Handoff、Context Drift | **与 Tako 核心高度重合。**已有 runlog／模型输入／journal 分离、file／skill 记录块、子代理与 handoff；应测目标、未完成事项、权限与证据是否漂移 |
| 6. Context Security 与 ContextOps | 信任边界、外部内容不当指令、租户隔离、Context Decision Trace、故障分类、Context Eval、策略版本 | **直接有价值。**Tako 有权限、PII 和部分 Trace；候选集、过滤原因、排序、预算与最终输入的完整决策证据仍不能假定已经齐全 |

本篇给 Tako 的新增启发是：模型最终输入存在，不等于能解释“为什么其他候选没进来”。如果没有候选与过滤证据，记忆没召回、权限过滤、排序丢失和预算挤压很容易被误归因成模型不会答。

### 第七篇　Tool／Skill 治理：按能力形态和风险吸收

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. Capability Architecture | API、Tool、Skill、Workflow、SubAgent、MCP 的形态选择；最小模型可见面 | **Tako 已有对应抽象。**继续用工具分级、tool_search、技能与子代理权限控制上下文和误选，不增加插件体系作为装饰 |
| 2. Tool Contract 与动态供给 | 模型语义、Runtime 执行、治理三层契约；参数、结果、错误和动态最小工具集 | **直接有用。**Tako 有工具 schema、业务错误、白名单与分级；Eval 应同时测选对、传对、读懂错误和不该看到时不可见 |
| 3. Action Runtime | 提议、校验、授权、审批、执行、验真、补偿；短期凭据、幂等与部分成功 | **仅在外部高风险业务行动成为产品目标时优先。**Tako 已有局部审批与发布，不等于通用业务动作平台；不能借此恢复已否决的通用工具审批 |
| 4. Skill Engineering | Skill 边界、完成标准、Tool／Workflow／SubAgent 组合、渐进加载、Validator、生命周期 | **高度相关。**Tako 已有技能安装、读取、作用域、依赖、折叠和任务记录；可补技能参考任务、版本配对和效果／干扰评测 |
| 5. MCP 与 CapabilityOps | 生产 MCP、能力注册、动态 Tool Set、Credential Broker、Tool Trace、红队 | **部分适用。**Tako 当前是 stdio MCP＋围栏；远程服务认证、能力所有者、短期凭据经纪不是现有能力，应由真实 MCP 场景驱动 |

工具课程最容易造成的误建是“为了企业级而增加一层”。内部函数不需要强行 MCP 化；上传可执行文件进入 PATH 也不等于自动新增模型工具 schema。

### 第八篇　Memory 治理：评测现有双模式，不改成向量记忆项目

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 记忆契约 | Memory 与 History／Context／State 边界；权威数据不复制成记忆；权限不继承；当前任务优先 | **直接有用。**Tako 已有记忆边界与 scope；应增加“旧记忆不得覆盖当前指令／权限／实时文件”的反例 |
| 2. Memory Write Pipeline | 触发、提取、标准化、校验、策略、提交、Scope、幂等、冲突与版本 | **大部分已落地。**`pipeline` 有抽取、Redpanda、PG 状态、证据时间和写事务，`markdown` 走同步写；两者分别评测 |
| 3. Recall Contract | 按 Step 召回、权限与时效过滤、综合排序、召回与行动权限分离、最小注入、使用追踪 | **核心机制已有，决策证据可补。**Tako 使用 BM25，不需为课程改向量 Top-K；候选、过滤原因和最终注入应能用于失败归因 |
| 4. MemoryOps 与 Memory Eval | 查看解释、修正、删除传播、Poisoning、多租户权限、写入／召回／删除 Eval、Memory Off 对照 | **评测价值很高。**管理 UI 已按设计取消，不应恢复；可用 Memory Off／无治理召回／受治理记忆做配对实验，证明收益与风险 |

### 第九篇　EvalOps：当前最直接的施工依据

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. 质量契约 | Eval Contract、Outcome／Trajectory／System、安全不变量、软评分与硬门槛 | **直接吸收。**把“完成”写成文件／库／Task 状态与必要轨迹，不读模型自述 |
| 2. Dataset 与 Fixture | Task Taxonomy、开放 Case、环境快照、版本元组、数据集分层 | **重点补强。**每次试验都要能从同一初始世界开始，并说明哪些资产发生版本变化 |
| 3. Multi-Grader 与 Runner | Code／Environment／Trace／LLM、校准、Multi-Trial、Scorecard、批量执行 | **直接吸收。**复用 acceptance HTTP 底座与 bench 重复／配对能力，不另建通用平台 |
| 4. Trace／Replay／Ablation | 统一 Trace、Failure Taxonomy、症状／直接原因／根因、Fork、回归沉淀 | **直接吸收诊断方法。**Replay 和 Fork 按真实能力边界解释，根因用单变量对照证明 |
| 5. 受控发布 | 资产版本、Baseline／Candidate、CI Gate、Shadow／Canary、Kill Switch、Rollback、自动优化边界 | **分阶段。**离线回归先做；真实流量和外部副作用治理到位后再做发布层 |

### 第十篇　完结篇：怎样利用，而不是把预测当需求

| 课节 | 主要内容 | 对 Tako 的判断与可借鉴点 |
|---|---|---|
| 1. DataAgent 最终展示与总结 | 正常、故障、演进三链；M0 到最终版；全景图与治理主线 | **可借鉴验收叙事。**Tako 交付也应展示正常任务、kill -9／工具失败和版本对照，不只演成功 Demo |
| 2. 人与 AI 的四种关系 | 工具、依附、导学、内观；目标、判断、接管和责任 | **作为人的学习提醒。**Agent 多做执行后，人仍需能解释、验证和接管；不是 Tako 功能 |
| 3. 未来一年趋势 | 长时执行、真实行动、多 Agent、Harness 重构、EvalOps 与行动安全 | **用于保持架构可演进。**不能把当前模型短板固化为永久复杂流程，也不能把趋势预测当成立即施工清单 |
| 4. DeepSeek Harness 拆解 | DSH 与 Claude Code／DataAgent 的产品目标、架构取舍与边界 | **适合参照阅读。**仓库已有 deepseek-harness 参考与 dsh 公开基准臂；应读代码核事实，不把课程对比当源码证据 |

## 4. Eval 质量契约与可复现基准的细化

### 4.1 Outcome／Trajectory／System 三层

| 层 | 操作定义 | Tako 示例 |
|---|---|---|
| **Outcome：结果** | 任务结束后读取文件、数据库、代码和 Task 状态 | 文件确实修改，测试通过，最终产物已发布，回答包含要求事实 |
| **Trajectory：执行轨迹** | 读取模型、工具、人和恢复机制实际发生的事件 | 是否经过计划审批，是否读取技能，是否折叠，是否重复交付 |
| **System：系统代价** | 汇总完成这次任务消耗的资源与时间 | token、模型与工具调用、耗时、重试、后台等待、人工提问 |

Trajectory 不是强制唯一工具顺序。例如不应规定必须先 `read_file`、再 `TaskCreate`、再 `edit_file`；只约束审批前不写、交付不重复、需要验证折叠时确实发生折叠等必要条件。否则会惩罚合法的替代路径。

软评分用于报告完整性、代码质量和沟通；跨租户访问、PII 泄漏、审批前写入和重复外部副作用是硬性否决，不能被其他高分抵消。

DataAgent 的“华东区 GMV 下降诊断”可翻译为 Tako 的“并行完成项目修改并交付”：

| 契约项 | DataAgent | Tako |
|---|---|---|
| 结果 | GMV 方向、贡献因素和数值正确 | 文件与代码正确、测试通过、产物完整 |
| 证据 | SQL、来源表、指标定义、时间窗口 | 文件路径、diff／测试、来源 URL、runlog、journey |
| 轨迹 | 数据权限与高危审批 | 审批前不写、隔离执行、发布前验证、交付一次 |
| 系统代价 | 查询耗时、扫描量、仓库费用 | token、工具调用、耗时、重试、人工成本 |
| 硬性风险 | 跨租户查数、未审批 SQL | 跨租户文件／记忆、越权写、泄漏、重复副作用 |

### 4.2 Task Taxonomy 与 Case 覆盖

Tako 的语料应覆盖编码与文件、网络研究、长上下文、记忆、技能、子代理、持久 Task、后台命令、计划问答、多租户、重规划与结果发布。每类再覆盖：

1. 正常任务；
2. 空文件、长文件、无文本 PDF、权限不足等边界；
3. 工具失败、崩溃、通知中断等故障；
4. 越权诱导、提示注入、过期覆写、记忆污染等对抗。

还必须有正反向配对：需要最新信息时搜索，常识问题不搜索；信息不足时追问，清楚任务不反复追问；需要长期复用时记忆，闲聊不乱记；真正并行时隔离，普通任务不创建复杂编排。

### 4.3 Fixture 与逐试验隔离

Fixture 是运行前预置好的文件、Git、用户权限、记忆、技能、MCP、时间和外部响应。隔离单位应是 `case × trial`，否则上一次留下的文件、记忆或 Git 历史会污染下一次。

网络任务可通过同一工具接口提供固定响应，另跑少量真实联网烟雾。Harness 不能替模型读页面、修改答案或调用它本没有的工具。

### 4.4 版本元组与数据集治理

报告尽量绑定：

```text
引擎 commit
模型 provider + model ref
系统提示词版本／摘要
Context 装配与折叠策略版本
工具 schema 版本
Agent 规格版本
Skill 内容摘要
Memory 模式 + 初始数据版本
权限／沙箱策略版本
Dataset／Fixture 版本
Grader 版本
运行预算
```

Baseline 与 Candidate 只允许研究变量不同。无法记录版本的资产标 `unversioned`；密钥不能进入版本报告。

数据集按开发、回归、能力、保留和安全分用途；K/S/B/C 仍按判定对象分类，两者是正交维度。能力集稳定高通过后可转入回归集，饱和后补更困难任务。

## 5. Multi-Grader、稳定性与证据

### 5.1 四类 Grader

| Grader | 读取什么 | Tako 示例 |
|---|---|---|
| **Code Grader** | 可精确计算的数据 | JSON、哈希、测试退出码、答案精确匹配 |
| **Environment Grader** | 任务结束后的真实环境 | PG、项目文件、Git、Task 发布结果 |
| **Trace Grader** | run 中实际发生的事件 | 审批、折叠、禁止调用、重复通知 |
| **LLM Grader** | 难写成固定规则的质量 | 报告完整性、过度设计、回答切题 |

能确定性判断就不用 LLM；能读最终状态就不听自述；语义质量才用裁判模型并保留人工抽查。

### 5.2 Grader 校准

每个 Case 至少有一个参考解法，证明题可做且 Grader 接受正确结果；每个 Grader 用正确与错误样例自测。LLM 裁判每次只评一个维度，输出结构化分数、证据和 `unknown`；用人工金样校准，裁判模型／提示／采样变化后重新校准。裁判采样 3 次取多数只能降低波动，不能代替校准。

### 5.3 Multi-Trial 与 Scorecard

保留 M=5 的回归口径，同时区分：

- **pass@1**：单次成功率；
- **Pass@5**：5 次至少成功一次；
- **pass^5**：5 次全部成功。

5 次只成功 1 次是“可能做得到”，不是“可稳定交付”。5 次适合回归烟雾，不足以证明 2%～3% 的小改进。

Scorecard 分开报告结果、可靠性、安全、质量和成本，不合成总分。证据包关联 runId、sessionId、workspaceId、traceId、断言明细、轨迹、产物、版本与预算，但避免把 PII 和密钥复制进报告。

### 5.4 Batch Runner 的责任

```text
读取 Case → 建立干净 Fixture → 执行真实任务 → 收集证据
→ 调用 Grader → 保存逐次结果 → 汇总报告 → 按归因决定退出码
```

Runner 不替 Agent 操作、不修改被测引擎、不承担发布。复用 `scripts/acceptance/harness.py`；acceptance 已覆盖的单次接口字节不重复搬入场景 Eval。

## 6. 失败诊断、Ablation 与 Replay 的边界

### 6.1 症状、直接原因、根因

Trace 提供证据，不自动证明根因：

```text
症状：新会话回答了已经作废的地址。
直接原因：召回到了旧记忆，没有召回后续改口。
根因：记忆写入时，证据时间或 supersede 链处理错误。
```

```text
症状：折叠后身份证明文重新出现。
直接原因：折叠后的模型输入包含原文。
根因：PII 处理只覆盖首轮输入，没有覆盖统一模型输入转换口。
```

重要失败记录 `symptom / direct_cause / root_cause / evidence / reproduction_case / fix / verification`，避免全部归成“模型不行”。

### 6.2 Ablation 与 A/B 是同一条实验线

Ablation 是固定其他条件，只去掉或替换一个组件。Tako 可做：去掉记忆、去掉技能、去掉 `web_fetch`、替换 Context 召回、比较 shared／isolated、固定模型输出只改引擎逻辑。

统一使用 `case × {base, variant} × trial`：同题、同模型、同初始环境、同预算，两臂交替，按 `(case, trial)` 配对；缺一臂不进配对分母，成本和 token 分开报告。第一批实验可含 `without_skill / with_skill`、`without_memory / with_memory`、`prompt_a / prompt_b`、`single_agent / orchestration`、`shared / isolated`、`base / plus_browser`。

### 6.3 Retry／Resume／Replay／Fork 不能混用

| 名词 | 实际操作 | Tako 边界 |
|---|---|---|
| Retry | 同一逻辑操作失败后再尝试 | 是否安全取决于错误类型、预算与副作用 |
| Resume | 从持久状态接管未完成任务 | 已有；旧 epoch 不得继续写，未知副作用不盲目重做 |
| 证据回看 | 不重跑，重建当时消息、工具和状态 | 最适合排障 |
| 受控重跑 | 相同输入与 Fixture 再跑，允许新模型轨迹 | 用于修复验证和多次试验 |
| 局部确定性重放 | 固定模型或工具结果，重放引擎状态转换 | 用于恢复、折叠、交付机制测试 |
| Fork | 从中间状态复制现场，让候选版本分别继续 | 需要完整 Context、文件、记忆、Task 与租约状态，不适合 Eval 第一版 |

Tako 的崩溃续跑不等于“重放后答案相同”；journal 也不能未经证明被称为完整世界快照。

### 6.4 Regression Case 沉淀

```text
真实失败 → 保存必要输入与现场 → 删除无关步骤
→ 写明最终状态与轨迹断言 → 证明旧版失败
→ 修复后通过 → 纳入回归集
```

没有这一步，同一问题只能依靠下一位开发者记得曾经踩过。

## 7. Runtime 与 Memory 的关键边界

### 7.1 Autonomy Budget：Harness 预算不等于引擎预算

预算可包括墙钟、token、模型调用、工具调用、后台任务和外部副作用。Eval 第一版可以由 Harness 超时后调用停止接口，只能证明“Tako 没在外部预算内完成”，不能宣称“Tako 自己有 run 级预算控制”。原生预算还需持久化额度、扣减、恢复继承与超限停止，应单独立项。

### 7.2 Verifier 与 Eval Grader 不同

运行时 Verifier：

```text
Task 产出 → 验证文件、测试、项目变化 → 失败可阻止发布
```

外部 Eval Grader：

```text
run 结束 → 读取文件、数据库、轨迹 → 给出评分 → 不修改被测系统
```

两者可复用检查规则，但 Grader 不能替 Agent 修结果，否则失去独立观察意义。

### 7.3 Human Gate 不等于所有工具都先问人

Tako 应测试等待时禁止什么、崩溃能否恢复、答案是否只消费一次、拒绝后是否没执行。现有计划审批、技能安装确认、ask_user 和 Task 提问保留；越界命令仍由沙箱和权限拒绝，不新增通用工具审批回路。

### 7.4 统一证据不等于统一所有执行原语

普通 run、同步子代理、持久 Task、后台 bash 和记忆作业可以共享 runId、tenantId、traceId、状态和证据关联，但它们等待、恢复、交付和存储语义不同。若强行统一为一个状态机，会丢掉已有边界并制造兼容成本。

### 7.5 Memory Eval 的具体清单

| 行为 | 要证明什么 | 不做会怎样 |
|---|---|---|
| 写入 | 该记的记住，闲聊、密钥、临时推测不乱记 | 噪声淹没有用事实 |
| 改口 | 新事实生效，旧事实按模式契约处理 | 同时看到两个答案，继续用旧事实 |
| 召回 | 当前问题拿到相关记忆，不被无关内容淹没 | 记忆很多，需要时却拿不到 |
| 删除 | 不因召回、折叠、恢复或后台抽取复活 | 用户说“别记了”，系统又写回来 |
| 过期 | 失效信息不冒充当前事实 | 旧地址、旧配置影响新任务 |
| 隔离 | 用户、项目、租户之间不串记忆 | A 的路径、偏好或密钥进入 B 的输入 |
| 崩溃与并发 | 重试不重复写，乱序不覆盖更新证据 | 晚到旧任务覆盖刚说的新事实 |
| 权威与权限 | 记忆不替代当前数据库／文件，不继承旧权限，不覆盖当前任务 | 历史记忆变成越权或错误行动依据 |
| 模式差异 | `pipeline` 与 `markdown` 分开报告 | 把一种模式的限制误算到另一种模式 |

`pipeline` 有历史链、删除留痕和自动抽取；`markdown` 维护当前文件。不能要求 Markdown 提供数据库历史查询，也不能因当前文件正确就宣称运行档案全部抹除。

## 8. 受控发布与不适配内容

### 8.1 发布部分怎样分阶段

现在可用：资产版本元组、Baseline／Candidate 公平比较、稳定后的 CI Regression Gate。确定性 K/S 必须通过，关键能力不能低于冻结基线，安全问题不能被平均分抵消。

`incomplete` 不等于 Agent 失败，但正式发布的必测安全用例没跑成意味着“本次未验证”，不能宣布安全通过。[doc/83-场景化Eval方案](README.md) 的本地默认放行与正式发布视图是否阻断，需在口径裁定时明确。

### 8.2 Shadow、Canary、Kill Switch 与恢复

- **Shadow**：候选版本接收真实请求副本但不对用户生效；适合有稳定流量的 Agent SaaS、客服和 DataAgent。Agent 会写文件、发请求、运行命令，不能像推荐模型一样复制真实流量，否则可能重复产生副作用；只能做只读或严格隔离的 Shadow。
- **Canary**：少量用户或租户先用候选版本。Tako 的多租户结构未来适合按租户灰度，但不是当前 Eval 的完成条件。
- **Kill Switch**：严重异常时停止新 run、工具或后台任务，适合云运维、退款、邮件工单、数据库写入和交易 Agent；Tako 当前单 run／Task 停止不等于全局紧急开关。
- **完整版本恢复**：恢复代码、Prompt、Skill、模型和策略不能撤销已发邮件、已删数据或已发布文件；这些需要补偿操作，不能承诺靠回滚全部回退。
- **AI 自动优化**：适合自优化 Prompt／Skill／Harness 的系统。候选不能自行修改权限、安全和发布策略，也不能自行推向真实用户；不是 Tako Eval 第一版目标。

### 8.3 DataAgent 业务内容适合谁

| 内容 | Tako 怎么处理 | 最适合的 Agent 场景 |
|---|---|---|
| 华东区 GMV 下降诊断 | 换成代码修复、研究报告、长任务交付 | BI、经营分析、财务分析 Agent |
| 指标口径与 Metric Registry | 只借鉴先确认定义、权限与新鲜度 | 依赖企业指标语义层的数据 Agent |
| SQL／来源表／数据血缘证据 | 换成文件、URL、测试、runlog、journey | 数仓查询、报表生成 Agent |
| 扫描行数与查询费用 | 换成 token、模型调用、工具耗时 | 查询成本高的数据平台 Agent |
| 高危 SQL 审批 | 保留危险行动受控的思想，不搬 SQL 规则 | 有数据库写权限的企业 Agent |
| 跨租户表／行权限 | 改成项目、会话、Task、Memory 权限 | 企业多租户数据 Agent |
| 通用 Action Runtime | 等真实外部高风险写入场景出现再评 | 工单、预算、支付、云运维 Agent |
| 真实流量 Shadow | 推迟，并禁止复制副作用 | 有稳定线上流量的客服／数据／SaaS Agent |
| 自动优化与晋级 | 第一版不做 | 自改进、自演化 Agent |
| 3D／多模态感知 | 只迁移工程方法 | 空间智能、机器人、视觉与仿真 Agent |

### 8.4 对 3D／多模态 Agent 的可迁移边界

可迁移 Task Contract、控制权分配、Outcome／Trajectory／System、固定场景 Fixture、多次试验、Ablation、安全硬性条件和失败回归。

不能直接获得几何尺寸、空间关系、碰撞约束、多视角一致性、物理仿真、渲染视觉质量、传感器噪声／遮挡／标定误差和机器人动作安全等专用评分器。这些需要领域环境、参考结果和人工校准。

## 9. 读完全课程后，Tako 最值得补的内容

按“先证明现有系统，再新增机制”的顺序：

1. **场景 Eval 主链**：质量契约、Fixture、逐试验隔离、版本元组、Multi-Grader、Multi-Trial、证据报告；
2. **失败诊断**：Failure Taxonomy、症状／直接原因／根因、受控重跑、A/B＋Ablation、Regression Case；
3. **Context 可解释性**：按决策的信息契约、候选／过滤／排序／预算证据、关键上下文召回与噪声指标；
4. **Memory 价值与风险实验**：Memory Off、无治理召回、受治理记忆的配对对照，以及删除／污染／权限反例；
5. **技能与工具效果实验**：是否正确加载、何时不该加载、完成质量与成本是否改善；
6. **模型可靠执行与路由**：只有失败和成本证据支持时，再考虑统一 Retry／Fallback／自动路由；
7. **外部 Action 与生产发布**：只有真实业务写入和线上流量出现后，再建设通用 Action Runtime、Shadow、Canary、Kill Switch 和资产恢复。

不建议新增的内容：DataAgent 业务指标平台、为对齐课程而设的向量记忆替换、通用工具审批、统一所有执行原语的大状态机、第一版自动优化晋级。

## 10. 建议的学习与落地顺序

### 第一阶段：把 Eval 本身做可信

输入是现有真实事故与手工验收；动作是建立 Task／Eval Contract、Fixture、版本元组、四类 Grader、多次试验和证据报告；输出是任何人都能重跑并复查的逐次结果。没有这一步，后续根因和发布结论都不可信。

### 第二阶段：让失败能指导修复

输入是第一阶段红行与证据；动作是建立失败分类，区分症状／直接原因／根因，提供证据回看与受控重跑，用 A/B 完成 Ablation；输出是被实验证明的修复。

### 第三阶段：只补证据暴露的 Runtime／Context／Memory 缺口

输入是已归因的真实失败；可能的动作包括 run 级预算、Context Decision Trace、更细的恢复语义或模型降级。每项单独定义产品契约与验收，不与 Eval Runner 捆绑施工。

### 第四阶段：生产发布与反馈回流

输入是稳定可信的离线报告与真实流量；动作是 CI Regression Gate、必测安全用例的未验证处理、按租户 Canary、无副作用 Shadow、Kill Switch 和资产恢复；真实失败再缩成 Regression Case 回到第一阶段。

## 11. 最终判断与自学边界

完整大纲比最初 Eval 五章更有价值的地方，是它把模型、Runtime、Context、Tool、Memory 和 Eval 连成了一张图；对 Tako 最危险的使用方式，则是把这张图当成“缺什么就建什么”的采购清单。

正确吸收方式是：**复用已有执行、恢复、审批、记忆、工具和 Trace 机制，把 Eval 接在真实产出上；用失败证据证明缺口，再决定是否修改 Runtime。**

仅凭大纲可以开始自学和实践，但不能判断课程实操资产深度。后续真正需要验证的是：Fixture 是否稳定、Grader 是否校准、故障案例能否区分根因、Action 是否处理部分成功与副作用、发布实验是否避免重复行动。

所以这解决的是：完整课程里哪些思想能转成 Tako 的质量保障能力，哪些已有功能只需评测，哪些概念必须重新解释，以及哪些业务与生产内容应留给其他 Agent 场景，同时避免把 Eval 项目膨胀成没有边界的 Runtime 重建。
