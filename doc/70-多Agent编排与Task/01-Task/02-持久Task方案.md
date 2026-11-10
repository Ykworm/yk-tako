# 持久 Task 方案：把子代理调用变成独立工作

> 性质：**方案初稿（已被取代）**；保留推导过程，不再作为方案或施工真源。
> 状态：⬜ 未施工；由 [第一版方案：持久 Task 与固定 DAG 编排](../03-第一版方案.md) 取代。
> 日期：2026-11-09。
> 代码落点：—（未施工；预计涉及 `yk-tako-eino/internal/runengine/`、`cmd/yktako/`、`web/`，点头后按契约拍细化）。
> 调研来源：[Comma 持久 Task 调研](01-Comma持久Task调研.md)。任务依赖与多 Agent 调度不在本篇，见上级 `02-多Agent编排/`。
> 现行边界：子代理委派见 doc/25、档案见 doc/39；现有看板见 doc/41；run 恢复见 doc/30。

## 1. 背景：现在的“委派”为什么还不是 Task

用户让 tako 做一份需要二十分钟调研的报告。主 Agent 调 researcher 后，当前委派仍附着在一次工具调用上：子代理有独立上下文和持久 wire 档案，但“这项工作本身”没有独立于父 run 的任务身份、状态和收件通知。

这会留下三个具体断点：

1. 父 run 正在等待时，用户只能看到一次工具调用，不能把这件工作当作独立对象继续查看和管理；
2. 子代理中途崩溃时，doc/39 的现行口径是半成品 wire 不续跑；父 Agent 只能重新委派，不能对一件持久工作做一次有边界的恢复；
3. 将来两个调研工作完成后要自动唤醒汇总工作时，没有持久通知可消费，只能让主 Agent 一直等或下次碰巧再查。

现有 `run_tasks` 不解决这件事。它是某次 run 内由模型维护的看板记录，回答“这轮计划做哪几步”；它不是“引擎应该把哪件工作交给哪个 Agent”的执行任务。**看板行不会因为存在就自动获得 worker、模型、运行环境和结果。**

## 2. 目标与非目标

### 目标

先做一个小闭环：

> 用户或主 Agent 交出一件自包含工作 → 引擎持久化 Task → 独立执行者完成任务 → 结果持久化并通知来源会话 → 用户能查看、停止和在明确边界内恢复。

### 本期不做

- 任务依赖图、自动分叉／汇合；
- Agent 之间自由群聊；
- 动态创建新的 Agent 规格；
- 把现有 `run_tasks` 改成调度队列；
- 自动重放结果未知的外部副作用；
- 常驻后台子代理的无限生命周期；Task 必须有终态与归档出口。

这些不是永远不做，而是先证明一件 Task 的生命可靠，再由多 Agent 方案增加依赖关系。

## 3. 四个对象各自负责什么

| 对象 | 真身与职责 | 不负责什么 |
|---|---|---|
| **Task** | 一行持久工作记录：目标、输入、归属、执行者规格、模型选择、状态、最终结果 | 不保存每次模型调用的完整上下文 |
| **Task 执行 run** | 复用现有 run 的执行机制：租约、执行代次、工具账本、模型输入和 trace | 不是 Task 的业务状态正本 |
| **子代理档案 wire** | 记录某次子代理执行过程，供诊断与完整档案 resume | 不等于可调度 Task |
| **`run_tasks` 看板行** | 给模型和用户展示一轮工作的步骤与结果 | 不触发执行，不承担调度 |

推荐复用现有 run 作为 Task 的一次执行尝试，而不是另写一套 Agent 循环。这样工具权限、沙箱、上下文折叠、用量与 trace 继续只有一份实现。Task 执行需要独立的内部 session／会话归属，避免多个 Task 被现有“同一聊天 session 同时只允许一个活 run”的限制串行化；具体复用形状是施工前第一项契约决策。

## 4. 端到端链路

```text
用户发出目标
  → 主 Agent 判断需要独立委派
  → 调 delegate_task，给出自包含 content、执行者规格和可选模型
  → 服务端校验租户／项目／执行者权限
  → 一笔事务写 Task＋首条 command，并生成执行 run
  → 工具立即返回 {taskId,status}，不把长时间执行塞在这次调用里
  → Task worker 领取既有执行 run，按现有 Agent 循环干活
  → 每次执行结果与产物路径按现有账本落库
  → 成功时写 Task.result＋终态，并写一条持久通知到来源 session inbox
  → 来源 session 空闲时，引擎用通知起一场恢复／汇总 run
  → 主 Agent 读取 Task 结果并向用户交付
```

这条链里上一步的输出分别是下一步的输入：

- `delegate_task` 的自包含 `content` 是 Task 的首条 command；没有它，独立执行者看不到父会话，无法猜任务背景；
- Task 的 `current_run_id` 把业务工作连接到执行账本；没有它，崩溃后无法判断该恢复哪次执行；
- 最终结果与产物路径进入 session inbox；没有它，父 Agent 不知道何时该汇总；
- inbox 的消费位置决定通知是否还需要再投递；没有它，重启只能靠内存记忆。

## 5. 最小契约草案

以下是讨论用形状，**不是已验收字节**。点头后的第一拍必须先跑 API 契约，再反推表与前端。

### 5.1 模型工具

新增 `delegate_task`，与看板工具 `TaskCreate` 明确分开：

```json
{
  "content": "调研 A 市场，给出三个可核实的来源，并把报告写入 research-a.md",
  "agent": "researcher",
  "modelRef": "可选；缺省继承该执行者配置",
  "workspaceId": "可选；缺省使用当前项目"
}
```

成功立即返回：

```json
{"taskId":"at_…","status":"pending","runId":"r_…"}
```

`content` 必须自包含；引擎不把父会话全部聊天记录隐式塞给执行者。执行者规格只能来自当前租户可用清单，工具权限仍受父池与规格共同约束。

### 5.2 产品 API

- `POST /api/tasks`：用同一套规则手工创建 Task，供契约拍与页面使用；
- `GET /api/tasks/:id`：读取目标、状态、当前 run、结果与错误；
- `GET /api/workspaces/:id/tasks`：按项目列出 Task；
- `POST /api/tasks/:id/stop`：停止当前执行，Task 记为 cancelled；不撤销已经发生的副作用；
- `POST /api/tasks/:id/resume`：只对租约过期且可恢复的执行开放，复用 doc/30 的接管语义。

成功与失败使用现行 `{code,message,data}` 信封；模型工具错误仍是可供模型修正的文字结果，不把 HTTP 栈暴露给它。真实业务码与响应字段顺序在契约拍实跑后登记，不在初稿手写期望字节。

## 6. 状态与存储

### 6.1 Task 状态

```text
pending → running → succeeded
                  → failed
                  → cancelled
running + 执行租约过期 → 读侧 interrupted → 显式 resume → running
```

- `pending`：工作已持久化，执行尚未开始；
- `running`：存在当前执行 run；
- `succeeded`：结果已写入；不代表人类已审核内容质量；
- `failed`：执行有确定失败结果；
- `interrupted`：与现有 run 一样，是“仍在 running，但执行租约已过期”的读侧派生，不是假装执行正常；
- `cancelled`：用户或协调者明确停止，不回滚外部动作。

### 6.2 最小持久事实

建议新增 `agent_tasks`，字段至少覆盖：

- 身份与归属：`id`、`tenant_id`、`workspace_id`、`source_session_id`；
- 工作命令：`content`、`agent_name`、`model_ref`；
- 执行指针：`current_run_id`；
- 状态与结果：`status`、`result`、`error`；
- 创建去重：`idempotency_key` 与命令摘要；
- 生命周期：`created_at`、`started_at`、`finished_at`、`updated_at`。

另设持久 `task_inbox`（或等价的 session inbox 表）：一行包含来源 session、Task、通知类型、投递状态与去重键。Task 完成与通知写入必须在同一笔 PG 事务里；否则“任务已成功但父会话永远收不到”的窗口会一直存在。Redis 只能作叫醒优化，真相仍在 PG。

具体表名和字段是否复用现有表，施工前先按实际 run／session 存储契约复核，不在初稿假定可无改造复用。

## 7. 崩溃与重复执行边界

1. 执行前先按现有 journal 记录工具意图，执行后记录结果；Task 不另造第二份工具账本。
2. worker 死亡后租约过期，Task 读侧显示 `interrupted`；不会因为进程消失就自动写成 succeeded。
3. 显式 resume 时先取得新执行代次，再恢复已有结果；缺结果的工具仍按“结果未知、引擎不自动重跑”处理。
4. Task 创建重试只认同一幂等键与同一命令摘要；同键不同内容必须冲突，不能覆盖原工作。
5. 完成写入与 inbox 通知一笔提交；通知重复投递由消费去重处理，不承诺外部副作用 exactly-once。

这解决的是“执行状态可判定、结果有去处、旧执行者不能覆盖新执行者”，不是“任何任务都能无损自动重做”。

## 8. 监控与页面最小入口

- 每次 Task 创建、状态翻转和停止按 doc/46 发变更事件；页面收事件后重拉，PG 仍是真相；
- Task 详情展示目标、执行者、模型、当前状态、结果和产物链接；
- trace 以 Task ID 关联来源 run 与执行 run，能够从用户请求跳到执行者，再跳回汇总；
- watcher 第一阶段只处理“执行中断未通知”和“结果已落库但来源通知未消费”，不做复杂 stall 评分。

页面不是任务生命周期的持有者；关掉页面不会停止 Task。

## 9. 验收场景

第一用例是“一件工作独立跑完”，不是直接演示多 Agent：

```bash
curl -i -X POST "$BASE/api/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"workspaceId":"ws_…","agent":"researcher","content":"只回复 task-ready","idempotencyKey":"task-demo-001"}'
```

期望在契约拍中实跑钉死：HTTP 202，业务信封成功，`data` 中有同一个 `taskId`、`runId` 和 `status`。随后：

1. 用同一请求体重打，仍返回原 Task，不产生第二个执行 run；
2. 同幂等键换 `content`，返回冲突；
3. `GET /api/tasks/:id` 最终读到 succeeded 与真实结果；
4. 执行中终止自己启动的隔离引擎，重启后读到 interrupted，显式 resume 后完成；
5. 来源会话只消费一次完成通知；重复通知不产生重复汇总；
6. 另一租户用同一 Task ID 读取，仍按现行不可见规则拒绝；
7. 停止后工具副作用不被宣称已回滚。

详细 curl、期望字节和机器断言必须在对应契约拍同步进入 `scripts/acceptance/expected.json` 与 doc/19-v6-04，不能只留在本篇。

## 10. 施工前必须点头的决策

1. Task 执行 run 使用什么内部 session 形状，怎样绕开“同一聊天 session 串行”而不破坏现有限流；
2. 是否采用 `agent_tasks＋task_inbox` 两张新表，以及既有 run／session 表的复用边界；
3. 中断后第一期是否坚持显式 resume；本初稿推荐坚持，不做自动重放；
4. Task 完成后是否允许主 Agent 自动起汇总 run，以及该动作消耗的租户额度如何展示；
5. 对 doc/39“常驻后台子代理不做”的翻案范围：本方案只新增有终态的 Task 执行，不让既有子代理无限常驻。

## 11. 所以这解决什么

它把“子代理正在被调用”变成“一件工作有独立身份、输入、执行、结果和通知”。只有这层成立后，多 Agent 编排里的 A、B、C 才有可调度的对象；否则任务依赖图只是另一张不会执行的看板。
