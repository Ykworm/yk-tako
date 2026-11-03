# 05 · Octop 架构剖析与生长式复刻

> 性质：**参考实现剖析 + 复刻方针**。新系列第五篇，改写自旧《03-架构剖析与移植Plan》：
> Octop 的壳/芯分层、组件清单、规模数据原样保留（这些是"要什么"的地图），
> 把原来的"六阶段移植 Plan"和"L0–L8 学习阶梯"合并改写为**产品能力路线**（§4），
> 因为 2026-09-26 定案后仓库不再是分层学习对照，而是单入口产品形态。
> 依据：`ref-agent/Octop`（v0.9.33+，main 分支）+ `harness-pkg/` 里的
> `orcakit_harness_agent 1.0.14` / `harness_memory 0.9.11` / `harness_gateway 0.9.9` 源码。

## 1. 全局分层：Octop 是"壳"，harness-* 是"芯"

```text
┌─────────────────────────────────────────────────────────┐
│  入口                                                    │
│  Web Dashboard(React)  CLI  IM(飞书/钉钉/QQ/TG/企微)  Cron │
└──────────────┬──────────────────────────────────────────┘
               ▼
┌─────────────────────────────────────────────────────────┐
│  Octop 应用壳（src/octop，Python 单进程，约 13.1 万行）     │
│  ├── api/        FastAPI 路由层（60+ router，2.7 万行）    │
│  ├── infra/agents 每用户 Agent 管理（4.0 万行，最大模块）   │
│  ├── infra/gateway IM 渠道接入（1.1 万行）                │
│  ├── infra/{db,cron,auth,knowledge,skills,connectors...} │
│  └── launch.py   组合根：OctopServer + FastAPI + uvicorn │
└──────────────┬──────────────────────────────────────────┘
               ▼
┌─────────────────────────────────────────────────────────┐
│  harness-agent（4.4 万行，pip 外部包）★ Agent 核心         │
│  HarnessAgent = deepagents.create_deep_agent 的门面封装   │
│  + 24 个中间件 + 子代理/团队/技能/MCP/安全/模型路由         │
├─────────────────────────────────────────────────────────┤
│  harness-memory（3.2 万行）分层记忆，ports & adapters      │
│  harness-gateway（1.4 万行）IM 通道统一管线                │
├─────────────────────────────────────────────────────────┤
│  deepagents → langgraph → langchain（Python 生态）        │
└─────────────────────────────────────────────────────────┘
        存储：SQLite(WAL)/PostgreSQL 控制面 + checkpoint
        前端：dashboard/ React18+Vite+AntD（选型无关，可复用）
        桌面：desktop/ Go 托盘外壳（2.6k 行）
```

### 1.1 运行时对象层级（README 官方描述）

```text
OctopServer（单进程，启动时从控制面 DB 重建全部状态，可重启恢复）
 └─ HarnessAgentManager（按用户）
     └─ AgentRuntime（按 Agent）
         ├─ HarnessAgent      Agent 运行时（harness-agent）
         ├─ HarnessProcessor  统一入口路由：Web UI / IM / 定时任务
         └─ ChannelManager    IM 连接（harness-gateway）
```

**关键设计**：无外部 MQ，进程内 `HarnessProcessor` 统一路由所有入口；
无外部中间件依赖，默认 `~/.octop/octop.db` SQLite 一把梭。
对 yk-tako 的启示：**入口多不等于架构复杂**——多入口收敛到一个处理器里，是后面"多入口统一路由"那一层的参考形状。

## 2. 组件清单与规模（工作量评估的地图）

### 2.1 Octop 应用壳（13.1 万行）

| 模块 | 行数 | 职责 | MVP 需要？ |
|---|---|---|---|
| `infra/agents` | 39,852 | Agent 管理器、运行时、专家库、子代理、团队、中间件装配、MBTI、记忆后端 | ✅ 核心，但可砍 MBTI/teams |
| `api` | 27,095 | 60+ REST/WS 路由 | ✅ 按需裁剪（一半路由可砍） |
| `infra/gateway` | 11,391 | IM 渠道、HITL、WS、slash 命令 | 二期（MVP 只留 Web/CLI） |
| `infra/connectors` | 7,727 | OAuth + MCP 连接器（腾讯文档等） | 二期 |
| `infra/db` | 7,645 | SQLAlchemy 模型、迁移 | ✅ |
| `cli` | 5,495 | Click CLI、REPL | 二期（我们现在只有 REPL） |
| `infra/skills` | 3,141 | 技能包管理 | ✅（简化版） |
| `infra/setup` | 2,933 | 初始化向导、TLS | ❌ 砍 |
| `infra/backup` | 2,383 | 备份恢复 | ❌ 砍 |
| `infra/history` | 2,368 | 历史记录 | ✅（简化版） |
| `infra/auth` | 2,334 | JWT 多用户 | ✅（单用户简化） |
| `infra/knowledge` | 2,275 | RAG 知识库 | 二期 |
| `infra/cron` | 1,166 | APScheduler 定时任务 | ✅（核心卖点之一） |
| `infra/desktop/mobile/voice/proactive/backend/browser/providers` | ~9,000 | 桌面/移动/语音/主动关怀/存储后端 | ❌ MVP 全砍 |

**MVP 实际需要面对的壳代码 ≈ 3~4 万行等效功能**（重写，非翻译）。这些活都排在能力路线靠后（§4 第 8、9 项）。

### 2.2 harness-agent（4.4 万行）★ 复刻的核心目标

- `agent.py`（1,932 行）：`HarnessAgent` 门面，内部就是 `deepagents.create_deep_agent(**kwargs)`
  产出 `CompiledStateGraph`，暴露 invoke/stream/get_state 等。
- **24 个中间件**（harness 的真正价值所在）：

  ```text
  memory / memory_recall / model_router / model_settings / turn_model /
  tool_guard / tools_filter / tool_search / native_tool_search / client_tool_search /
  mcp_tools / skill_filter / filesystem_guard / pii / media_offload /
  compaction（上下文压缩）/ conversation_mode / runtime / session_header /
  bootstrap / checkpoint_ts / peer / turn_aware_profile / context_usage
  ```

- 其他：`subagents`、`teams`、`skills`、`mcp`、`providers`（多厂商模型路由）、
  `security/tool_guard`（命令防护规则）、`acp`、`observability`（Langfuse）

**结论：harness-agent ≈ deepagents 配置层加中间件矩阵。** 它的每一条中间件都是一层能力的「要什么」清单，
但**实现方式不直接照抄**——同一件事在 Eino 里可能落在完全不同的一层（例如"规划与执行的时序"，
harness 只能用中间件的软钩子，"Eino 的正式答案"是编图，见《08》）。

### 2.3 harness-memory（3.2 万行）

采用 ports & adapters 整洁架构：`core / domain / application / pipeline / operations / adapters`。
分层记忆 + 全文检索，记忆随工作区迁移。**接口边界清晰，适合整体替换**——这正是 T-Mem 二期可以插入的位置。

### 2.4 harness-gateway（1.4 万行）

channels（飞书/钉钉/QQ/Telegram/企微）+ manager + push_routing + hitl + media。
统一管线：各渠道的消息 → 归一化 → HarnessProcessor。二期再碰。

## 3. 为什么是"生长式复刻"而不是"移植"

```text
原计划：读懂 22 万行存量 → 翻译到新语言（"精读-规格化-翻译"）
现计划：从一个能跑的主循环出发 → 逐层生长能力 → 参考实现只当"参考答案"
        最终功能对齐 Octop MVP，但每一行代码都知道为什么存在
```

- 起点不是零：owner 亲手写过一遍 pi 的 tool-call 主循环，也玩过 Google ADK
  （Agent/Runner/Session/多智能体概念，与 Eino 的 ADK 大量同源）。
- 每层节奏固定：**先自己尝试 → 卡住时读参考实现的对应代码 → 实现 → 对比反思**（反思写成本系列文档）。
- 原六阶段 Plan（架构侦察 → Agent 核心规格化 → MVP 范围冻结 → 选型终审 → 骨架 + 主循环 →
  垂直切片 → 增量移植）只保留前两步的成果：架构侦察就是本文 §1、§2；
  "Agent 核心规格化"改成**伴随式阅读**——不再先整块精读，而是每长一层、带着问题精读对应模块。
- 选型已终审（Go + Eino，见《04》§4.3），不再反复。

## 4. 能力路线（产品视角）

每层都是"能给用户用的东西"，不是"为学某个框架概念而写的 demo"。
参考实现一列只回答"要什么"，具体设计按 Eino 的正式做法来。

| # | 能力 | 参考实现（harness-agent / Octop） | 状态 |
|---|---|---|---|
| 1 | 主循环 + 工具（时间/计算/bash/文件） | deepagents 核心 | ✅ |
| 2 | 工具安全：命令守卫 / 文件守卫 / PII + 人工审批 | `security/tool_guard`、`middleware/{tool_guard,filesystem_guard,pii}.py` | ✅（实现于 `internal/guard`，见《06》） |
| 3 | 任务看板：Plan / Tasks / Actions 三层分离 | 不照 `write_todos` 整表覆盖，自研记账工具 | 🔶 定案完成、实现进行中（见《01》《02》《07》） |
| 4 | 上下文管理 | `compaction` / `media_offload` / `context_usage` | ✅ 上下文压缩已实现（算法见《12》第 9 节，任务见《13》）。`media_offload` 与 `context_usage` 仍未做 |
| 5 | 子代理 | `subagents/` + ADK | ⬜ |
| 6 | 记忆 | `memory` / `memory_recall` → 二期 T-Mem（见《04》§2） | ⬜ |
| 7 | 断点恢复与流式 | checkpoint / interrupt | 🔶 内存 checkpoint + interrupt 审批已有。Web 事件已落 SQLite，断线可按 seq 回放；在途轮次崩溃后续跑仍未做（见《10》第 4.4 节） |
| 8 | 多入口统一路由 | `HarnessProcessor`（Web / IM / cron） | 🔶 Web 入口有 SSE、审批、续传与显式停止；IM / cron 未动 |
| 9 | 多用户、多 Agent | `HarnessAgentManager` + `AgentRuntime` | ⬜ |

节奏与纪律：

- 一层没站稳不跳层（例如任务看板没跑通就先不上上下文压缩——后者是"任务变长之后"才暴露的问题）；
- 每层结束写一篇小结进 doc/（正在写的这个系列就是）；
- 只有"要不要做这一层"听参考实现的，**"怎么做"不听**。

## 5. 翻译原则（2026-09-24 定案）

复刻 Octop 是**功能翻译，不是设计移植**。每爬一层：

- Octop / harness 源码只回答"**要什么**"——功能规格、行为对照、卡住时的参考答案；
- "**怎么设计、怎么写**"一律遵循 Eino 的架构思想与工程最佳实践：
  - **compose**（Chain / Graph / Workflow）管确定性编排；
  - **ADK**（`ChatModelAgent` / middleware / prebuilt）管自主性；
  - 官方 prebuilt 优先于手搓；
  - middleware 只做横切关注点（守卫、脱敏、压缩），不塞业务状态机；
  - 不被 Octop 的 Python/LangGraph 设计（24 个中间件、state 频道那套）带偏；
- Eino **没有的类库**（长时记忆、RAG、MCP、IM 接入、沙箱、任务队列等），参考
  WeKnora（`ref-agent/WeKnora`）的 Go 工程实现，而不是找 Octop 的 Python 对应物。

已经验证过一次：L2（任务规划）阶段曾依照 Octop 的 `afterModel` 设想手搓软约束线，
后来按 Eino 的方式收口（先编图），2026-09-26 定案又收敛为看板 + 单入口（见《07》《08》）。
反面教训是：**硬翻 LangGraph 的防守思路在 Eino 里没有落脚点**，那条路只会碰壁。

## 6. 资产归档位置

| 资产 | 位置 |
|---|---|
| Octop 源码（功能参照物，只取"要什么"） | `/Users/yuweikang/dev/ykcode/ref-agent/Octop` |
| WeKnora 源码（Eino 缺失类库的 Go 参考） | `/Users/yuweikang/dev/ykcode/ref-agent/WeKnora` |
| harness-* 包源码（含 wheel 归档） | `/Users/yuweikang/dev/ykcode/harness-pkg/` |
| 本系列文档 | `/Users/yuweikang/dev/ykcode/learn/yk-tako/doc/` |

