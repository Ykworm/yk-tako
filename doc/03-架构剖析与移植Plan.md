# 03 - Octop 架构剖析 与 移植 Plan

> 日期：2026-09-23
> 依据：`/Users/yuweikang/dev/ykcode/Octop`（v0.9.33+，main 分支）
> + PyPI 拉取的 `orcakit-harness-agent 1.0.14` / `harness-memory 0.9.11` / `harness-gateway 0.9.9` 源码

## 1. 全局分层：Octop 是"壳"，harness-* 是"芯"

```
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

### 运行时对象层级（README 官方描述）

```
OctopServer（单进程，启动时从控制面 DB 重建全部状态，可重启恢复）
 └─ HarnessAgentManager（按用户）
     └─ AgentRuntime（按 Agent）
         ├─ HarnessAgent      Agent 运行时（harness-agent）
         ├─ HarnessProcessor  统一入口路由：Web UI / IM / 定时任务
         └─ ChannelManager    IM 连接（harness-gateway）
```

**关键设计**：无外部 MQ，进程内 `HarnessProcessor` 统一路由所有入口；
无外部中间件依赖，默认 `~/.octop/octop.db` SQLite 一把梭。

## 2. 组件清单与规模（移植工作量评估的基础）

### 2.1 Octop 应用壳（13.1 万行）

| 模块 | 行数 | 职责 | MVP 需要？ |
|---|---|---|---|
| `infra/agents` | 39,852 | Agent 管理器、运行时、专家库、子代理、团队、中间件装配、MBTI、记忆后端 | ✅ 核心，但可砍 MBTI/teams |
| `api` | 27,095 | 60+ REST/WS 路由 | ✅ 按需裁剪（一半路由可砍） |
| `infra/gateway` | 11,391 | IM 渠道、HITL、WS、slash 命令 | 二期（MVP 只留 Web） |
| `infra/connectors` | 7,727 | OAuth + MCP 连接器（腾讯文档等） | 二期 |
| `infra/db` | 7,645 | SQLAlchemy 模型、迁移 | ✅ |
| `cli` | 5,495 | Click CLI、REPL | 二期 |
| `infra/skills` | 3,141 | 技能包管理 | ✅（简化版） |
| `infra/setup` | 2,933 | 初始化向导、TLS | ❌ 砍 |
| `infra/backup` | 2,383 | 备份恢复 | ❌ 砍 |
| `infra/history` | 2,368 | 历史记录 | ✅（简化版） |
| `infra/auth` | 2,334 | JWT 多用户 | ✅（单用户简化） |
| `infra/knowledge` | 2,275 | RAG 知识库 | 二期 |
| `infra/cron` | 1,166 | APScheduler 定时任务 | ✅（核心卖点之一） |
| `infra/desktop/mobile/voice/proactive/backend/browser/providers` | ~9,000 | 桌面/移动/语音/主动关怀/存储后端 | ❌ MVP 全砍 |

**MVP 实际需要面对的壳代码 ≈ 3~4 万行等效功能**（重写，非翻译）。

### 2.2 harness-agent（4.4 万行）★ 移植的核心目标

- `agent.py`（1,932 行）：`HarnessAgent` 门面，内部就是 `deepagents.create_deep_agent(**kwargs)`
  产出 `CompiledStateGraph`，暴露 invoke/stream/get_state 等
- **24 个中间件**（这是 harness 的真正价值所在）：
  memory / memory_recall / model_router / model_settings / turn_model /
  tool_guard / tools_filter / tool_search / native_tool_search / client_tool_search /
  mcp_tools / skill_filter / filesystem_guard / pii / media_offload /
  compaction（上下文压缩）/ conversation_mode / runtime / session_header /
  bootstrap / checkpoint_ts / peer / turn_aware_profile / context_usage
- 其他：subagents、teams、skills、mcp、providers（多厂商模型路由）、
  security/tool_guard（命令防护规则）、acp、observability（Langfuse）

**结论：harness-agent ≈ deepagents 配置层 + 中间件矩阵。**
若走 TS 路线，JS 版 `createDeepAgent` 原生支持 middleware 机制，24 个中间件逐个对应重写即可；
若走 Eino 路线，用 Eino ADK + 自建中间件链重新组织。

### 2.3 harness-memory（3.2 万行）

ports & adapters 整洁架构：`core / domain / application / pipeline / operations / adapters`。
分层记忆 + 全文检索，记忆随工作区迁移。**接口边界清晰，适合整体替换**——
这正是 T-Mem 二期可以插入的位置。

### 2.4 harness-gateway（1.4 万行）

channels（飞书/钉钉/QQ/Telegram/企微）+ manager + push_routing + hitl + media。
统一管线：各渠道消息 → 归一化 → HarnessProcessor。二期再碰。

## 3. 移植 Plan（六阶段）

```
阶段 0 ✅ 架构侦察（本文档）——已完成
         Octop 仓库 + 三个 harness 包源码已就位
         （harness 包源码位于 /tmp/harness-pkg/，建议移入项目归档）

阶段 1 ⬜ Agent 核心规格化（与选型无关，纯阅读）
         产出《04-Agent核心规格.md》：
         - HarnessAgent 的 graph 形状（节点/边/状态 schema）
         - 24 个中间件的执行顺序与各自职责（逐个一句话）
         - checkpoint / interrupt / 流式的用法
         - HarnessProcessor 的消息生命周期（一条消息从进入到回答的完整旅程）

阶段 2 ⬜ MVP 范围冻结
         单用户、Web 控制台 only、chat + 工具 + 简版记忆 + cron
         明确砍单：IM 渠道、connectors、移动/语音/备份/setup/桌面、teams、ACP
         产出《05-MVP范围.md》

阶段 3 ⬜ 技术选型终审（用阶段 1 的真实数据拍板）
         产出组件级映射表：每个组件 → TS 方案 vs Go/Eino 方案 → 工作量估算
         拍板：全 TS（LangGraph.js + deepagents JS）还是 全 Go（Eino）
         确定项目名 ✅（2026-09-23 定案 `yk-tako`，目录已重命名）

阶段 4 ⬜ 骨架 + Agent 主循环打通
         最小可跑：单进程服务 + 一个 agent + 一个工具 + Web 流式对话
         （TS: Hono/Nest + createDeepAgent；Go: Gin/net-http + Eino ADK）

阶段 5 ⬜ 垂直切片
         一条消息完整旅程：Web → 会话 → Agent → 工具 → 记忆 → 流式回包
         落库（SQLite/Postgres）、checkpoint、重启恢复

阶段 6 ⬜ 增量移植（按优先级）
         cron 定时任务 → 技能体系 → 记忆增强（harness-memory → 后续 T-Mem）
         → IM 渠道（gateway）→ 知识库 RAG → connectors/MCP 市场
```

### 阶段门禁（每阶段完成标准）

| 阶段 | 完成标准 |
|---|---|
| 1 | 能脱离源码向别人讲清一条消息的完整旅程 |
| 2 | MVP 砍单有书面记录，不再变动 |
| 3 | 映射表覆盖阶段 2 的全部组件，工作量有估算 |
| 4 | `curl`/浏览器 能与 agent 流式对话 |
| 5 | 重启进程后会话可恢复 |
| 6 | 每个子项独立可验收 |

## 4. 当前决策点

~~唯一阻塞事项：阶段 3 的技术选型~~ → ✅ 已定案：**Go + Eino**（2026-09-24 终版，
依据见《06-Eino规划机制调研与路线定案》；此前 2026-09-23 的"全栈 TypeScript"定案已被推翻，
TS 实现已删除）。生长式路线从 L1 开工（L0 已由 pi 复刻版覆盖），L1/L2 已在 `yk-tako-eino/` 完成。

## 5. 计划调整（2026-09-23）：从"移植"改为"生长式复刻"

> 背景：项目 owner 曾从零复刻过 pi（极简 coding agent）的第一个版本（亲手写透过
> tool-call 主循环），并玩过 Google ADK（熟悉 Agent/Runner/Session/多智能体概念）。

### 理念转变

```
原计划：读懂 22 万行存量 → 翻译到新语言（"精读-规格化-翻译"）
新计划：从已有的 pi 复刻版出发 → 逐层生长能力 → harness 源码当"参考答案"
        最终功能对齐 Octop MVP，但每一行代码都知道为什么存在
```

pi 复刻经验 = 已掌握系统中最难理解但代码量最小的部分（tool-call 主循环）；
Google ADK 经验 = Agent/Runner/Session/子代理概念已见过
（注：Eino 的多智能体框架同样名为 ADK，概念大量同源）。

### 能力阶梯（替代原"阶段 4"）

每层节奏：先自己尝试 → 卡住时读 harness-agent 对应源码 → 实现 → 对比反思。

| 层 | 能力 | harness-agent 参考答案 | 已有基础 |
|---|---|---|---|
| L0 | tool-call 主循环 | deepagents 核心 | ✅ pi 复刻 |
| L1 | 工具权限/安全 | `tool_guard` / `filesystem_guard` / `pii` | |
| L2 | 任务规划 | `todoListMiddleware` | |
| L3 | 上下文管理 | `compaction` / `media_offload` / `context_usage` | |
| L4 | 子代理 | `subagents/` + ADK | ✅ Google ADK |
| L5 | 记忆 | `memory` / `memory_recall` → 二期 T-Mem | |
| L6 | 断点恢复/流式 | checkpoint / interrupt | |
| L7 | 多入口统一路由 | cron / Web / IM（HarnessProcessor） | |
| L8 | 多用户/多 Agent | manager + runtime 层级 | |

### 翻译原则（2026-09-24）：按 Eino 的思想复刻，WeKnora 补位

复刻 Octop 是**功能翻译，不是设计移植**。每爬一层：

- Octop / harness 源码只回答"**要什么**"——功能规格、行为对照、卡住时的参考答案；
- "**怎么设计、怎么写**"一律遵循 Eino 的架构思想与工程最佳实践：compose
  （Chain/Graph/Workflow）管确定性编排，ADK（ChatModelAgent / middleware / prebuilt）
  管自主性，官方 prebuilt 优先于手搓，middleware 只做横切关注点——不被 Octop 的
  Python/LangGraph 设计（middleware 矩阵、state 频道那套）带偏；
- Eino **没有的类库**（长时记忆、RAG、MCP、IM 接入、沙箱、任务队列等），参考
  WeKnora（`/Users/yuweikang/dev/ykcode/WeKnora`）的 Go 工程实现，
  而不是找 Octop 的 Python 版对应物。

L2 已经验证过一次：硬编排对照（`cmd/l2planexec`）直接用 Eino 官方 `planexecute` 编图，
而不是翻译 LangGraph 的 afterModel 防守思路——那条路在 Eino 里根本不存在，硬翻只会碰壁
（见《06》）。

### 对原六阶段 Plan 的修订

- 原阶段 1（Agent 核心规格化）改为：**伴随式阅读**——不再先整块精读，
  而是每生长一层、精读对应模块（带着自己的问题读，效率数倍）
- 原阶段 4（骨架+主循环）大幅缩短：L0 已会，直接从 L1 开始
- 阶段 2（MVP 范围冻结）、阶段 3（选型终审）不变
- 新增收益：每层结束产出一篇小结（可作为系列技术博客素材）

### 选型影响

> 2026-09-24 更新：选型已终版定为 Go/Eino（见《06》），本小节权衡仅作历史参考。

生长式路线下，**语言选型 ≈ "pi 复刻版用什么语言写的 + 想在哪门语言里继续深入"**。
阶梯在任何一门语言里都能爬，TS 与 Go 的权衡不变（见《02-技术路线讨论》第 7 节），
但"学习体验连续性"成为新的重要权重。

- 倾向全 TS 的信号：想最快复刻 harness-agent 的中间件矩阵；跟随 LangChain 生态
- 倾向全 Go 的信号：单二进制分发；复用 WeKnora 的 IM/RAG/MCP 实现；desktop/ Go 外壳融合

## 6. 资产归档位置

| 资产 | 位置 |
|---|---|
| Octop 源码 | `/Users/yuweikang/dev/ykcode/Octop` |
| WeKnora 源码 | `/Users/yuweikang/dev/ykcode/WeKnora` |
| harness-* 包源码 | `/Users/yuweikang/dev/ykcode/harness-pkg/`（已从 /tmp 归档） |
| 讨论文档 | `/Users/yuweikang/dev/ykcode/learn/yk-tako/doc/` |
