# Comma 持久 Task 调研：可靠委派底座，不是任务图

> 性质：**非方案内容，调研记录**；不作施工真源。
> 状态：✅ 调研完成；借鉴条目由同目录《持久 Task 方案》承接。
> 日期：2026-11-09。
> 代码落点：—（调研记录）。
> 调研对象：本地 `/Users/yuweikang/dev/ykcode/ref-agent/Comma/`，快照 `66ab3dc92475005aea9afd123df36a28e4ef86ab`。
> 证据范围：阅读现行工程契约与关键 Elixir 代码；没有启动 Comma、没有运行其测试。下文区分代码事实与 tako 的借鉴判断。

## 1. 先回答什么问题

tako 目前已有主 Agent 调用 coder／researcher 子代理的能力，但这种委派仍是一次工具调用：父 Agent 发起调用，子代理跑完，最终结果回到父 Agent。要走向多 Agent 编排，先缺的不是一张复杂的任务依赖图，而是一个更基础的对象：**一件工作能不能脱离这次工具调用，独立存在、独立执行、独立留下结果？**

Comma 值得看，是因为它把这个对象做成了持久 Task。它回答的是“可靠委派怎么做”；它没有回答完整的“多个任务怎样按依赖自动推进”。

## 2. 名词先对齐

- **Router**：负责接待用户、拆解意图、创建 Task、接收结果和决定 Task 状态的协调 Agent。
- **Worker**：真正执行 Task 的 Agent。一个 Worker 可以服务多个互相独立的 Task。
- **Task**：一件持久工作。Comma 把它表示为 `kind='agent_task'` 的 Conversation；其中有工作指令、结果消息、参与者和状态。
- **Runtime Session**：某个 Agent 的一次模型与工具执行上下文。Task 是业务工作对象，Session 是运行上下文，两者不是一个东西。
- **Loop**：定时或事件驱动的后台程序。它不是 Task 依赖图，也不是一次 Task 执行。

这套区分直接修正一种常见误读：看到“Conversation”就以为 Comma 只保存聊天记录。实际上 Conversation 在这里是 Task 的持久边界；Worker 的 Session 才负责模型执行。

## 3. 一条真实委派链

```text
用户向 Router 提出目标
  → Router 调 im_api.internal.task.create
  → 服务端确定 Task 身份、Worker、模型、运行位置和权限
  → Conversation owner 一笔提交 Task 与第一条工作指令
  → Worker 获得自己的 Runtime Session 并执行任务
  → Worker 把进度与结果写成 Task Message
  → 结果消息唤醒 Router 的等待
  → Router 读取结果、决定 Task 状态、向用户交付
```

代码落点：

- 创建入口与角色校验：`/Users/yuweikang/dev/ykcode/ref-agent/Comma/systems/apps/salix_im/lib/salix_im/provider/internal.ex:74`。
- 创建参数会固定 Worker、模型、位置、来源和请求身份：`.../provider/internal.ex:630`。
- Task、参与者与第一条指令交给同一个 Conversation owner：`.../task_conversation_input.ex:26`，实际创建调用在该文件 `:121`。
- 新 Task 与首条指令必须一起提交；失败不能留下一张没有工作指令的任务卡：`.../conversation_actor.ex:2132`。
- Worker 获得新 Session，委派者复用自己的 Session：`.../agent_delivery_payload.ex:386`。
- Router 的 `wait_for` 由后续输入、Worker 结果或故障通知唤醒，不靠页面轮询维持任务生命：`.../tools/async_ops.ex:93`。

## 4. 值得摄取的六个机制

### 4.1 Task 与首条指令原子创建

**失败故事**：如果先创建 Task、再写工作指令，进程可能死在两次写入之间。页面上出现“正在运行”的任务，但 Worker 永远不知道要做什么。

**Comma 的做法**：Task 记录和第一条指令在同一owner的同一笔元数据写入里提交；同请求重试不会把未提交消息变成另一份事实。

**对 tako 的启示**：持久 Task 的创建不是先插一行再慢慢补内容，而是“工作对象＋首份自包含输入”一起成立。

### 4.2 创建请求有稳定身份

**失败故事**：工具调用已经创建 Task，但回执在网络中丢失。模型或客户端再试一次，如果生成一个新 ID，同一个目标就有两个执行者。

**Comma 的做法**：创建请求身份绑定目标 Worker、模型、运行位置、正文等命令内容；重试沿同一个身份寻找既有结果，不因结果不确定就另建替代 Task。相关构造在 `.../provider/internal.ex:705`。

**对 tako 的启示**：异步委派必须从第一拍就有幂等键，不能等出现重复任务后补去重。

### 4.3 Task、执行 Session 与结果消息分开

**失败故事**：父 Agent 的上下文被压缩或当轮已经结束，子工作仍要继续。如果工作的唯一载体是父 Agent 的工具调用参数和内存消息列表，就无处查它现在做什么、上次结果是什么。

**Comma 的做法**：Task 保存业务事实；Worker Session 保存模型执行上下文；结果以持久 Message 回到 Task。压缩 Session 不会删掉 Task，结束一次模型调用也不等于完成 Task。

**对 tako 的启示**：未来的 Task 不能只是现有子代理 wire 文件的另一个名字；wire 是执行档案，Task 还需要自己的身份、指令、状态和结果。

### 4.4 结果到达与状态裁决分开

Worker 只发布结果，Router 决定 Task 是否完成。`TaskCompletion` 在 Conversation owner 内重新检查当前 Router 与状态，而不是相信模型随结果附带的一句“完成了”：`.../task_completion.ex:6`。

这解决的是“执行者说自己成功”和“协调者确认工作已收尾”不是同一件事。它仍不能证明结果语义正确；任务效果需要独立评测。

### 4.5 两类沉默分别有人兜底

`TaskWorkerWatch` 处理两种真实断点：

1. Worker 没发布结果就停了：先提醒 Worker一次，再向 Router 报告；
2. Worker 已发布结果，但 Router 结束后忘了决定 Task 状态：再通知 Router。

每种通知都按委派消息或结果消息的序号做幂等，重启后从既有消息重建判断，不靠 watcher 内存记住“我提醒过没有”。代码与生产故障动机见 `.../task_worker_watch.ex:1`。

这对 tako 的价值不是“加一个定时器”，而是：**任务创建、执行者回复、协调者收尾是三个不同环节，每个环节都可能停住。**

### 4.6 每个 Task 独立选择模型与运行位置

Router 表达任务目标，服务端根据权限与默认值解析模型和位置。Task 可以在内部运行时、云 VM、连接设备或编码沙箱执行。这里分配的是模型与计算资源，不等于在多个角色 Agent 之间做任务路由。

对 tako 可借的是“每个工作项可以有自己的模型与执行环境”；不应把它误写成“已经实现多 Agent 动态分工”。

## 5. Comma 的明确边界

### 5.1 一个 Task 只有一个负责 Worker

现行契约原文是 “Each Task has one Worker”，见 `/Users/yuweikang/dev/ykcode/ref-agent/Comma/docs/salix/tasks-background-execution.md:16`。多个 Task 可以独立存在，但不是一个 Task 内多个角色自动分叉和汇合。

### 5.2 旧 Task Graph 已退役

当前创建入口遇到 `workflow` 会直接返回 “Task graph creation is retired”，见 `.../provider/internal.ex:642`。迁移代码把旧图转换为普通单 Worker Task，并停用额外图参与者：`.../migrations/retire_task_graph.ex:1`。

代码能证明旧图已退役，**不能证明当时为什么退役**；本调研不补编决策动机。

### 5.3 当前 Comma 产品不让 Router 管理 Worker

Router 不获得 `agent.list`、`agent.create_worker`、`agent.update` 等管理工具，默认 Task 跑在 Group 的 Task agent 上：`.../tool_policy.ex:512`。底层创建接口仍保留合法 `agent_id` 的解析路径，因此这里要区分“底层接口能指定”与“当前产品策略让模型自由选择”。

### 5.4 没有 Hermes 式依赖调度器

Comma 当前没有“所有父 Task 成功后，调度器自动把子 Task 改为 ready 并领取执行”的这条现行链路。结果汇合由 Router 根据消息继续判断。故它不是 tako 学习 DAG／依赖调度的主要参照。

## 6. 对 tako 的借鉴清单

| 摄取 | 不摄取 |
|---|---|
| 持久 Task 作为委派工作对象 | Elixir/OTP 技术栈 |
| Task 与首条指令原子创建 | 把 Task 强行做成现有聊天 Conversation |
| 创建幂等键绑定完整命令 | Comma 的整套设备、云 VM 与 Loop 体系 |
| Task 状态与执行 Session 分离 | 把 watcher 当成任务依赖调度器 |
| Worker 报告、协调者裁决的分工 | “README 写了 Swarm，所以现行就是完整多 Agent”的结论 |
| 两种沉默的持久化监控 | Lean／TLA+ 作为本方案前置条件 |

## 7. 调研结论

Comma 对 tako 最有价值的一句话是：

> **先把一次子代理调用变成一件可持久、可追踪、可恢复、可通知的工作，再谈多个 Agent 的依赖编排。**

它不能替代 Hermes Kanban 作为任务依赖参照；两条参照的分工由同目录方案与上级《多 Agent 编排》线承接。
