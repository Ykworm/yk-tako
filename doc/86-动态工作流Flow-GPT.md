# 86 · 动态工作流 Flow-GPT

> 性质：**前次 arena 综合候选（已参与最终综合，不作施工真源）**。
> 状态：未施工；最终唯一综合方案以 [doc/85](85-动态工作流Flow.md) 为准。本文保留当时拟议的 revision、QuickJS-WASM、整轮求值和文件发布设计，只作评审记录，不得直接照此施工。
> 日期：2026-11-20。
> 代码落点：无；本文所有接口、表、执行器和数值均为候选设计，不是已落地事实。
> 最终综合：以 85 为 base，移植本文的未知副作用恢复前处理、严格结果契约与父/子容量保护；未采用 QuickJS-WASM、revision 复用和文件发布替换。见 [最终综合记录](log/2026-11-20-Flow-Arena最终综合.md)。
> 验证边界：候选形成时未启动服务、运行模型、测试 JS 执行器或沙箱；原 arena 记录保留在 §18。

## 0. 方案结论

保留 `/agent`、`/dag`、`/flow` 三个显式入口。Flow 用受限 JavaScript 执行循环、分支、await 和动态扇出，不转换为静态 DAG。

建议采用固定 Node launcher 加 QuickJS-WASM 客体解释器，由 Go 持久保存每轮 JS 求值的输入、调用命令和结果。恢复时从原脚本开头重新求值，按历史顺序向 JS 交付已保存结果，逐轮核对输出后才允许产生新工作。

后台 `/flow` 与命名 Flow Agent 共用一个引擎。普通 Agent 在独立 session/run 中执行，文件只写隔离现场，最终发布才修改项目 `content/`。未知工具结果先由程序阻止恢复模型，脚本修订不能自动重做未知外部副作用。

## 1. 背景与失败故事

用户要求“检查所有 API，按实际问题修复，重复检查，直到通过或连续两轮没有进展”。第一轮找到三个文件，第二轮可能只剩一个，也可能出现新的问题。初始 DAG 难以提前声明这些未知工作；JS 可以根据检查结果决定下一轮。

恢复不能只有调用编号和缓存。假设 A、B 两个审查并行，B 先返回，脚本的闭包把 B 写进数组首项，再派 `repairB`。进程崩溃后 A、B 都已完成，若缓存先返回 A，脚本就会派 `repairA`。脚本、参数和结果都没变，后续工作却变了。

因此还要记录“JS 先收到 B，再收到 A”。这份记录是实际发给脚本的结果及其顺序，不是模型摘要，也不是推测出的依赖图。

另一个失败发生在写入之后：子 Agent 已修改文件或发送外部请求，工具结果尚未落库就崩溃。直接重跑会重复动作。用户改脚本后缀也有同样问题，所以调用恢复、文件现场、修订授权和发布必须分别定义。

## 2. 需求与执行方式

三种方式分别承担以下工作：

| 方式 | 下一步由谁决定 | 中间结果在哪 | 适用任务 |
|---|---|---|---|
| `/agent` | 主模型逐轮决定 | 主上下文及普通子代理上下文 | 少量步骤、频繁追问与直接工作 |
| `/dag` | 已声明的 JSON 节点与依赖 | 现有持久 Task 结果 | 已知依赖，保留同组修订与继任组 |
| `/flow` | JS 根据结果执行控制流 | JS 变量，持久结果和求值历史 | 未知数量的工作、动态循环与扇出 |

需求仍是 doc/85 所描述的三个显式方式、动态 JS、单 Markdown Flow Agent、Go 执行普通 Agent、JS 无直接工具 I/O，以及持久恢复、双副本、revision、通知和真实页面。本文改变的是实现设计，不缩成静态 DAG 或内存脚本。

无前缀默认 `/agent`。`executionMode=agent|dag|flow` 写入 run，resume 读回原值。服务端按 mode 装配创建工具，不能只靠提示词收窄行为。

`/agent` 不提供临时 `start_flow` 或 `create_orchestration`，但可委派已安装且可见的命名 Flow Agent。`/dag` 使用现有编排创建工具，不临时生成 Flow。`/flow` 的主模型负责规划并提交脚本，不在规划阶段直接实施修改。

这会收窄当前普通对话的临时建组能力，是待批准的行为变化。方案获批后须同步 doc/82；既有 Task、同步普通子代理与后台 bash 的其他行为不借本任务重写。

Claude [官方 workflows 说明](https://code.claude.com/docs/zh-CN/workflows)已核对。官方描述 JS 动态编排、后台进度和同一 Claude Code session 内缓存恢复，退出后下一 session 从头运行。本文的 PG、跨进程、双副本恢复是 Tako 自行设计的能力，不声称知道 Claude 内部实现。

## 3. 完整业务链与责任

```text
用户选择 /flow 并发目标
  → 创建 executionMode=flow 的规划 run
  → 主模型提交 JS、args、允许 Agent 与预算
  → start_flow 校验并在 PG 创建 Flow 和 revision
  → 规划 run 返回 flowId 并结束，来源会话可继续聊天
  → Go 领取驱动租约，固定 runner 求值 JS
  → Go 持久保存求值轮次及调用，事务后调度普通 Agent
  → Worker 在独立 session/run 和隔离现场执行
  → Go 验证、固化结果，按持久顺序交给 JS
  → JS 根据结果继续循环或分支，最后 return
  → Go 接受最终结果，完成或阻止文件发布
  → PG 创建完成通知，空闲来源会话只读交付
  → 用户从 Vue 打开报告和实际文件
```

命名 Flow Agent 从父模型的一次委派工具调用开始。Go 按 `(parentRunId, toolCallId)` 创建或接回 Flow，等待同一引擎的最终结果，再写父工具结果，父模型继续回答。该来源不另发后台完成通知。

JS 只决定业务调用和结果组合。Go 决定身份、执行权、预算、校验、取消和结果交付顺序。Worker 执行普通 Agent。WorkspaceProvider 处理文件现场、不可变结果和发布。

PG 保存执行与恢复事实，共享持久卷保存文件。Redis 只唤醒与通知页面重拉，丢事件由 PG 对账补偿。Vue 读接口，不从模型文字、SSE 或本地状态推断成功。

## 4. 执行器与规格

### 4.1 执行器选择

建议固定 Node launcher 只处理 stdio 和 QuickJS-WASM 客体作业队列。模型脚本在客体中求值，不进入 Node 主执行环境，不使用 `node:vm` 当安全边界。

客体解释器只注册 JSON 宿主 API，不安装文件、网络、进程和模块能力。所有出入值都序列化，不把 Node 对象或函数引用交给脚本。Node、WASM、runner、helper 和 JSON 规范的构建摘要写入 revision。

选择这个组合是为了主动排空 Promise 作业，得到可核对的求值轮次，同时避免直接维护 C 可执行程序。代价是 Node 与 WASM 部署依赖、兼容和安全维护；仓库当前没有这项能力。

Promise 支持、动态代码禁用、资源终止和两个平台的沙箱必须实测。若该组合不能满足契约，应修改待审方案，不静默换执行器、删场景或无沙箱执行。

### 4.2 单 Markdown Flow Agent

```markdown
---
name: api-review-flow
description: 根据检查结果组织多轮 API 修复
runtime: flow
allow_agents: [researcher, coder]
---
const result = await agent(args.request, {
  agent: "researcher", key: "discover"
});
return {value: result.value, publish: []};
```

普通规格缺 runtime 时仍是 `agent`。Flow 正文是 JS，不是 prompt 或任意 `entry` 路径。Flow 拒绝 `model`、工具及技能执行字段；模型和工具从允许的普通 Agent 解析。

`allow_agents` 必须显式非空，只能引用当前用户可见、`runtime=agent` 的普通子代理。Flow 不作聊天根模型或 DAG Task 执行者，不支持 Flow 嵌套。seed、保存、更新、导入导出和有效规格转换都要携带新字段。

命名委派保留 `{"request":"..."}`，宿主生成 `args={"request":该字符串}`。脚本需要额外参数时，由规格写默认值或另行采用明确结构化调用契约，不让父模型传入权限策略。

创建时冻结 Agent 正文、有效模型 ref、工具策略、团队指令、笔记输入、能力策略和 runner 版本，不保存明文密钥、连接或模型对象。恢复使用原快照，但重新检查当前权限与能力，只能收窄；撤权或能力不兼容进入 `needs_review`。

既有 Task 创建后重读 AgentSpec 的政策不因此改成快照。

## 5. JS 数据与错误契约

脚本是受控 async 函数体，支持局部变量、闭包、循环、if、try/catch、await 和 Promise 组合。宿主 API 为：

```text
agent(prompt, {agent,key,label?,schema?,inputs?})
  → Promise<{value: JSON值或文本, resultRef: 宿主结果引用}>
parallel(thunks)
pipeline(items, mapper, {concurrency})
  → Promise<Array<{ok:true,result:{value,resultRef}}|{ok:false,error}>>
phase(name) / log(text) → 本轮观察命令
args → 创建时冻结的 JSON
```

`key` 必填、revision 内唯一。`resultRef` 由 Go 产生，模型不能在 JSON 中伪造。`inputs` 接受本 Flow 已成功结果或批准的复用结果，缺省为空，表示只读创建基线。JS 数据依赖不会自动变成文件依赖。

直接 await 的业务执行、JSON 解析或 schema 校验失败抛 `FlowAgentError`，作者可 catch。helper 按输入下标返回成功或失败对象，只转换业务失败，不用 `null` 兼任失败标记，所以成功的 `value:null` 仍可区分。

mapper 或 thunk 可多次 await、产生多个调用。helper 控制活跃 mapper 数，Go 另限制真实子 run 并发。返回数组按输入顺序，调用编号按实际命令生成顺序。

越权、协议错误、PG 失败、驱动失权、预算超限属于宿主终止原因，Go 停止求值，不能被 JS catch 后继续。中断结果未知也不默认为普通 helper 失败跳过。

`return` 固定为 `{value,publish:ResultRef[]}`。Go 验证引用和 JSON；有未完成调用、未处理 rejection 或运行中后台命令时，进入 `failed/unjoined_calls`，取消只做失败清理，不能让漏写 await 的脚本成功。Promise.race 的输家须显式等待收口再 return。

参数与宿主结果深冻结。边界拒绝重复 JSON 键、非有限数、函数、访问器、循环引用、BigInt 和 undefined；整数限制在 JS 安全范围，超范围数用字符串。对象键规范排序，数组顺序保留，字符串不 trim。

## 6. 动态循环示例与文件输入

下面的检查结果出来后才知道要修几个文件，没有预建后续 DAG。每个检查结果的文件引用包含检查所见的完整稳定现场；修复从它建立独立副本。

```js
let inputs = [];
let previousCount = null;
let noProgress = 0;
for (let round = 0; round < args.maxRounds; round++) {
  phase(`检查第 ${round + 1} 轮`);
  const checked = await agent("检查 API，返回错误文件与错误数量", {
    agent: "researcher", key: `check:${round}`, inputs,
    schema: {
      type: "object", required: ["files", "count"],
      additionalProperties: false,
      properties: {
        files: {type: "array", items: {type: "string"}, maxItems: 20},
        count: {type: "integer", minimum: 0}
      }
    }
  });
  if (checked.value.count === 0) {
    return {value: checked.value, publish: [checked.resultRef]};
  }
  noProgress = previousCount !== null &&
    checked.value.count >= previousCount ? noProgress + 1 : 0;
  if (noProgress >= 2) {
    return {value: {reason: "no_progress", report: checked.value}, publish: []};
  }
  previousCount = checked.value.count;
  const fixes = await pipeline(checked.value.files, (file, i) =>
    agent(`修复 ${file}，不要修改其他文件`, {
      agent: "coder", key: `fix:${round}:${i}`,
      inputs: [checked.resultRef]
    }), {concurrency: 2});
  if (fixes.some(x => !x.ok)) throw new Error("修复未全部成功");
  inputs = fixes.map(x => x.result.resultRef);
}
return {value: {reason: "round_limit"}, publish: []};
```

`20` 和并发 `2` 是建议约束；`maxRounds` 在创建边界验证为有界正整数。无进展、达到轮数或部分修复失败都返回明确结果，不假称检查通过。

下一轮检查从上一轮修复引用组合现场。公共继承变更只应用一次；不同修复对同一路径产生冲突则准备失败。要解决冲突，脚本必须显式派合并调用，Provider 不偷偷选择最后写者或调用模型。

## 7. 结构化结果由程序校验

schema 在调用创建前由 Go 编译，在结果进入成功状态前由 Go 验证。放进 prompt 或厂商参数只是指导，不代替验证。

首期支持有界 JSON Schema 子集：type、properties、required、additionalProperties、items、enum、const、长度和数值范围。拒绝 `$ref`、format、自定义函数、pattern 和未列出的关键词，不静默忽略，也不联网解析。

schema 输出必须为单个严格 JSON 值，不从自然语言猜 JSON，不剥代码围栏后宣布成功。限制字节、深度和验证工作量。schema 摘要纳入调用身份。

新增 `submit_flow_call_result` 保存终稿、结构化值与文件声明。验证失败可回字段错误，但不自动重新执行模型或工具；run 结束无有效提交为 `failed/result_not_submitted`。成功还须 Worker 收口、后台 job 结束和 Provider 固化，不是提交工具返回成功即可。

现有 TaskResult 的摘要、路径、metadata 校验不具备这项 schema 能力。Task 保留自己的提交工具和政策。

## 8. 求值轮次与重放

求值轮次指 runner 接收启动参数或一个宿主结果后，执行同步代码及所有可运行 Promise 作业，直到没有可继续作业。期间只缓冲调用、phase、log 和 return，不执行 Agent。

```text
Go → runner: init {v,flowId,revision,driverEpoch,buildId,script,args,limits}
Go → runner: evaluate {turnSeq,settlement:null|{callSeq,ok,result|error}}
runner → Go: turn {turnSeq,commands:[...],state:waiting|returned|thrown,
                   value?,error?,pendingCount}
Go → runner: committed {turnSeq}
Go → runner: cancel {reason}
```

stdout 只准有界 NDJSON，stderr 只作脱敏诊断。transport 身份由可信 launcher 提供，Go 仍检查当前租约、版本、顺序、消息长度和未知字段。

每轮按以下顺序推进：

1. Go 选择一个已有持久终态的调用结果，交给 runner。第一轮输入为 null。
2. runner 排空客体作业，把整轮命令按生成次序返回；未收到 committed 不接受下一结果。
3. Go 在短事务中校验整轮、驱动 epoch、状态和预算，保存输入与输出、调用行及待调度事实；本轮任何一项非法则整批拒绝。
4. 事务提交后才可领取其中的调用，Go 回 committed，随后才选择下一个结果。

调用可以并行执行，交给 JS 的结果一次一个。若求值后未提交就崩溃，这一结果尚未形成已接受的脚本行为，新请求不能已执行；恢复允许重新选择未提交历史尾部的输入。

历史为 `turn(A,B) → settle(B)/turn(repairB) → settle(A)/turn(...)` 时，恢复必须重演 B 先到，即使 A 已在缓存。闭包、数组顺序、race 胜者和 pipeline 补位才能一致。

调用身份为 `(flowId,revision,callSeq)`；`key` 给作者和文件交接使用，driverEpoch 只管执行权。inputHash 覆盖完整 args、prompt、schema、inputs 及结果版本、基线、Agent 快照、能力和 API 版本，label 不改变业务执行。

同 revision 从第一行重放，每轮比对输入与 commands、phase/log、控制输出和最终值的规范 JSON。运输 epoch 不参与业务比较。所有历史核对完才允许新工作；偏离进入 `needs_review/replay_mismatch`，不提前执行差异批次。

自建纯 Promise 与闭包按固定解释器作业顺序重建，不保存 JS 堆。构建不一致或旧构建不可用时拒绝恢复。无限同步循环或微任务在排空到等待点前就触发资源终止。

## 9. 持久普通 Agent 与父工具恢复

新增 Flow 执行适配，按允许的普通 Agent 快照生成独立 `kind=flow_call` session/run，作为 Worker 的根执行者。它共用模型解析、工具过滤、笔记输入、Worker.Run 和 RunResume，不共用 Task 的领取或状态语义，不造假 Task/DAG 行。

调用、session/run 绑定和排队在短事务完成，事务外准备现场和启动。领取后未启动的崩溃由对账识别；driver 死亡不判健康子 run 失败，子结果接受检查自己的 runEpoch 与调用绑定。

Flow 子执行者剔除递归委派、建 Task/DAG/Flow、技能、写记忆及 ask_user。需要人决定时由 Flow 状态和来源对话处理，不引入脚本内等人 API。

子调用中断恢复前，程序扫描 journal。普通工具有 intent 没有 result 时，先登记 incident 并进入 `needs_review/outcome_unknown`，不能先恢复模型再靠 prompt 要求它不要重发动作。

首期处理项支持“补交可验证的结果证据”或“放弃调用”，不提供忽略未知副作用的一键重跑。证据校验与人工判断必须分别标注；只有完整文件清单、实际字节或可核验外部回执才能作为相应结果，人工摘要不冒充程序验证。

处理后沿原 session/run 的 ClaimResume、epoch+1 与 RunResume，不改变普通 journal 的缺结果回注政策。缺结果事实未解决时不调用子模型；既有 Task 与普通 run 不增加这个 Flow 专属检查。

父工具也新增明确的持久对象恢复分支。`start_flow` 和命名 Flow 委派缺 tool_result 时按来源 run/toolCallId 查 Flow：成功补原结果，活执行接回持久等待，中断或失败补真实状态；没有对象沿用普通未知结果，不盲目补建。

父工具结果写回必须匹配父当前 epoch、toolCallId 和 Flow 结果身份，已有结果则核对后空操作。等待记录在 PG，本机 channel 只能优化唤醒。

## 10. 状态、控制并发与预算

Flow 执行状态为 `preparing → queued → running → finalizing → succeeded`，另有 `pausing/paused/stopping/stopped/interrupted/failed/needs_review`。调用状态为 `queued/preparing/running/succeeded/failed/interrupted/cancelled`。

发布状态独立为 `none/pending/publishing/blocked/published`，交付独立为 `none/pending/claimed/consumed/failed`。`succeeded` 表示 JS 与结果已接受；用户完成线还要求文件已发布或明确无发布、通知已消费。发布受阻不能显示“文件已改好”。

所有控制操作带 `expectedRevision + controlVersion + idempotencyKey`。行锁下检查版本再转换，递增 controlVersion。迟到 pause/stop 不能作用于已经恢复的新执行；同键同体回原结果，同键异体冲突。

暂停先写 pausing，拒绝新轮次和新领取，已提交未领取调用留 queued，在途执行允许完成。无在途执行后丢弃 runner、进入 paused。继续重放历史再处理排队调用。

停止先写 stopping，拒绝新工作，取消子 run、关联后台 job 并杀 runner 进程组。收口后 stopped，不自行复活。租约过期只拒绝旧 PG 写，不能证明远端进程或外部请求已停止；无法确认的工作保留 incident。

paused 与已解决 incident 的 interrupted 可同 revision resume。failed/stopped 不能原地重试失败工作，要创建新 revision。finalizing 崩溃只恢复发布与结果收尾，不重新求值已接受 return。

driver 领取和续租用 PG 时间、owner、epoch、状态和 lease 条件。子 runEpoch、publicationEpoch、交付 attempt 各管自己的写权，不拿 revision 代替它们。

同步父仍占现有租户 run 容量。创建时通过同一数据库容量协议预留至少一个子位，不足立即拒绝，不能父等子、子永远无槽。预留按同步 Flow 持久身份计数，子 run 占用后不重复计数，终态、取消和失租对账释放。

该预留要与 AcquireRun 和 Task 领取的容量计数同批适配并回归，但不改变“等待父 run 是否算运行位”的现有定义。更多子调用可排队，后台 Flow 不占父位，驱动另设并发上限。

父停止或 deadline 到期持久传播取消；父进程崩溃只把 waiter 标为 detached，健康 Flow 在原 deadline 内可继续。父恢复接回同一 Flow，不重置预算，不自动给同步来源另发通知。

父、Flow、子调用 deadline 取最早者。usage 计所有真实执行，失败取消也记账；缓存不重复计模型消费。revision、resume 和新 session 不能重置累计预算或租户日额度。

## 11. 工作现场与发布替换

资源放 `runtime/flows/<flowId>/revisions/<r>/`，区分基线、每调用现场、不可变结果和发布记录。默认每调用 isolated；JS runner 不挂项目目录，子 Agent 只写调用现场，不能直接写 content。

inputs 以显式顺序交接已成功文件结果，顺序纳入输入摘要。Provider 将公共祖先差异去重再合并，冲突拒绝。结果引用保存 parent refs 和相对原基线的完整稳定结果，不让下一轮检查又回到旧基线。

复用旧结果不复用旧可写目录。旧 worker 或 bash 无法证实停止时，不在其现场启动新写者，也不复制仍会变动的半截目录；保持 needs_review，或从已接受不可变结果另建新调用。

最终 publish 只选成功 resultRef。多个引用用同样的确定合并规则组合；冲突进入 blocked，不隐式运行合并模型。通过后固化一个最终目标，Provider 保存 Git/文件内部状态，PG 只记通用引用与发布状态。

初次发布与新 revision 替换都保存三份事实：

- 原始基线 B：创建 Flow 时的文件状态。
- 预期当前 E：初次为 B，替换已发布 revision 时为该旧版实际发布后的状态。
- 新目标 N：新 revision 相对 B 计算出的完整选中结果。

发布计划覆盖 E 与 N 不同的路径，保存每条路径的旧状态、新状态和“文件不存在”标记。修改、新增、删除均按同一比较：当前等于 E 才可写 N；当前已等于 N 可核对为幂等完成；两者都不等则 blocked。

例子是旧版 A→B，新版目标 C。当前确为 B 时替换为 C，不再把 A→C 补丁叠到 B。旧版新增、新版取消时删除该文件；旧版删除、新版保留时恢复。用户或其他 run 对受影响路径有改动则不覆盖，不受影响路径保持现状。

写入持项目共享 PG 锁，锁内再次校验，Provider 用原子文件操作与持久进度记录。发布中崩溃按 E/N/实际字节收敛，遇第三种字节阻止继续；不重新跑 Agent“修复发布”。

这是 Flow 发布适配必须新增的替换契约，不能宣称现有 Provider 已经支持。一次发布可能涉及多个文件，不承诺跨文件系统瞬时原子可见；未全部收口前页面显示 publishing 或 blocked。

## 12. revision 与副作用

旧版必须稳定，无活子执行、发布或交付尝试，才能建新 revision。请求带 baseRevision 和 controlVersion，不覆盖旧脚本、结果或通知历史。

新 revision 先 paused，在不派新工作的比较模式中从头提供旧 settlement。完整轮次相同且结果成功的前缀可复用；第一处差异所在整轮及之后不复用，不按相同 key 偷跳独立分支。args、基线、快照或 runtime 改变默认不复用。

差异产生持久 reviewHash，显示复用前缀、首个变化、旧后缀、已发布状态和可能的副作用。批准须匹配该摘要及当前控制版本，才能在新隔离现场派新工作。

批准范围是“此 revision 在剩余累计预算内生成后续 workspace-only 工作”，后续数量尚未知，不能假称已列全。只在新现场重做文件，复用前缀的不可变结果作为输入，最终发布按 §11 替换。

默认 Flow 子 bash 禁出站网络、限制写根，MCP 不装配；不改普通 Task 的 bash 出站政策。未分类能力保守视作外部副作用，不因工具名是 bash 或 researcher 就认定只读。

外部写能力首期默认不开放。需要开放时必须另有可执行的能力策略、明确授权及对应验收；这项授权不能顺便通过 allow_agents 获得。外部结果未知不自动重发，远端无幂等协议时不承诺 exactly-once。

revision 不恢复累计预算，不清除 incident。批准文件重执行不意味着批准重复发送邮件或覆盖外部系统。

## 13. PG 数据与约束

下表是新增设计，归属由宿主解析，FK 与查询包含 tenant/user/workspace 关系：

| 表 | 主要内容 |
|---|---|
| flow_runs | 来源、当前 revision、执行/发布/交付状态、controlVersion、deadline、预算和驱动租约 |
| flow_revisions | 脚本原字节、args、快照、构建摘要、基线、原因、reviewHash 和批准事实 |
| flow_turns | 每轮 settlement、commands、控制输出、规范摘要和复用来源 |
| flow_calls | callSeq/key/inputHash、执行 session/run 绑定、状态、结果及 incident |
| flow_results | 调用或最终结果的不可变版本、原始终稿、校验值和文件引用 |
| flow_waiters | 父 run/toolCallId、父 epoch、Flow、deadline、等待及容量预留状态 |

incident 可作为 flow_calls 的结构化持久字段，至少保存 journal 身份、未知动作、处理决定及证据引用；不另建通用事故平台。

唯一约束为 `(flow,revision)`、`(flow,revision,turnSeq)`、`(flow,revision,callSeq)`、同 revision 非空 key、非空执行 session/run 绑定及结果身份版本。复用行只指旧不可变结果，不重新绑定旧执行 run。

创建防重同时有 `(source_run_id,source_tool_call_id)` 和后台 `(tenant_id,source_session_id,idempotency_key)`，start_flow 也写前者，父 journal 才能查回。重复键须比较请求 hash。

整轮提交同时写调用及待调度事实，结果完整后才能成为 settlement。结果固化先写共享卷再 PG 条件接受，孤儿文件由对账与清理处理；不把 PG 与文件系统说成同一原子事务。

普通子结果接受匹配当前 runEpoch，Flow 命令与收尾匹配 driverEpoch，发布和交付匹配各自身份。Redis 不能替代这些约束或求值历史。

## 14. API 与页面契约

以下是待审形状，不是 curl 实测字节。新 JSON 固定 `code → message → data`，成功 message=""，失败 data=null 或明确详情，保留正确 HTTP 状态，文件成功体为原字节。

```text
POST /api/runs                       增 executionMode，缺省 agent
start_flow / POST /api/flows          共用创建服务
GET /api/workspaces/:id/flows         sessionId、cursor、limit
GET /api/flows/:id                    摘要、actions、预算、结果/发布/交付
GET /api/flows/:id/calls              revision、afterCallSeq、limit
GET /api/flows/:id/turns              revision、afterTurnSeq、limit
GET /api/flows/:id/revisions          版本摘要，详情另 GET /:revision
GET /api/flows/:id/results/:resultId  value、artifact 清单及来源
GET /api/flows/:id/artifacts/:resultId/*path   清单授权后的文件字节
POST /api/flows/:id/pause|resume|stop
POST /api/flows/:id/revisions
POST /api/flows/:id/revisions/:revision/approve
POST /api/flows/:id/calls/:callId/resolve
POST /api/flows/:id/retry-publication|retry-delivery
DELETE /api/flows/:id                 稳定终态软归档
```

后台创建体为 `{sourceSessionId,goal,script,args,allowAgents,limits,idempotencyKey}`；来源必须属于本人项目，agent_tool 来源和 toolCallId 只能由宿主提供。首次 202、同键同体 200 回原对象，异体 409。

创建 data 固定为 `{flowId,revision,status,controlVersion}`。详情 data 为 `{flowId,goal,source,revision,status,controlVersion,phase,counts,budget,result,publication,delivery,error,actions,updatedAt}`，无结果为 null。

pause/resume/stop/retry 带 `{expectedRevision,controlVersion,idempotencyKey}`；revisions 带 `{baseRevision,controlVersion,script,args,reason,idempotencyKey}`，201 回 paused 新版；approve 另带 reviewHash，202 进入 queued。resolve 带 callId、incidentVersion、decision、证据及控制版本。

建议申请 120xxx，实际分配须核码表：120001 不可见 404，120101 输入/schema 400，120102 幂等冲突 409，120103 版本冲突 409，120104 状态禁止 409，120105 需处理 409，120106 runner/沙箱不可用 503，120107 Flow 预算耗尽 429。认证、角色及现有租户容量沿用既有码。

列表游标为 `(createdAt,id)`，绑定过滤条件，limit 建议默认 20、上限 100；调用与轮次按序号分页，超界或跨 revision 游标拒绝。未知 sessionId 过滤回空列表，不冒充可见性认证。

只读也检查项目属主，写面再查角色、用途、当前版本与项目未删。未知和越权对象同 404。artifact 先规范化和解析软链，再逐项核清单与归属，不能扩现有 media 读根。

Vue 的 Flow 卡展示目标、phase、状态、计数和打开入口；ChatDock 加 Flow 标签，FlowView 显示调用、脚本版本、结果和处理项。分别显示创建 callSeq、结果进入 JS 的 turnSeq、完成时间和复用来源。

按钮按 actions；操作成功重拉，失败弹业务 message 并保留脚本草稿。移动端从卡进入整页详情，不能只依赖手机不渲染的右栏。文件复用现有预览组件，所有数据来自上述 API。

## 15. 安全、观测、通知与清理

runner 环境白名单，去除 NODE_OPTIONS、预加载、HOME 凭据、API Key、租户目录和工具 PATH。Linux 专用 bwrap 隔离网络，仅只读挂运行依赖；macOS 专用 seatbelt 禁网络、项目访问和子进程创建，不套 bash 的出站许可。

客体禁止模块、Date、随机、计时器、共享内存、eval 和动态代码。动态构造器包含 Function、AsyncFunction、GeneratorFunction 的间接入口；必须由解释器初始化与编译入口策略封闭，不只是字符串匹配或删全局名字。无法封闭则验收失败。

建议默认累计 64 调用、16 revision、子并发 2、Flow 总 deadline 30 分钟；脚本/args 各 64KiB、协议行 1MiB、单结果 256KiB、JSON 深度 32、日志 256 条、历史 16MiB、客体内存 64MiB、launcher RSS 256MiB、单轮 JS CPU 2 秒、累计 JS CPU 30 秒。均为设计初值，须真实运行校准，不是性能测量。

资源限制包括执行 watchdog、结果大小、PG 历史与文件存储，超限明确终止，不截断后假装完整。秘密不进快照、runner 或 trace。

OTel 每 driver attempt 建 span，关联 flowId/revision/driverEpoch/turnSeq/callSeq/runId 和复用/incident/发布/交付。跨崩溃用稳定 ID 与 link，不假造活过进程的 span。导出失败不参与业务裁决。

后台最终结果接受与 `flow_finished:<flowId>:<revision>:<resultRevision>` 通知同事务。新增 agent_notifications 的 Flow 来源，复用 attempts、来源忙时 pending、只读交付、显式失败重处理。发布 blocked 时交付明确说明未发布，不说文件已修改。

claim、输入构造与收尾都要实现 Flow 来源，不能只加 kind 常量。旧尝试不能收口新 attempt。同步来源只有父工具结果，没有重复完成通知。`flows_changed` 只发失效标识，页面重拉 PG。

来源会话或项目删除事务检查活 Flow、waiter、incident、发布和交付租约，未收口拒绝删除。终态软归档隐藏，过现有 archiveTTL 且无复用引用再清理；文件失败记 cleanup=pending，不先删恢复证据。

项目删除覆盖 runtime/flows，但不删租户技能、工具和用户记忆。旧 revision 结果仍被新 revision 引用时不能按旧日期单独清掉，引用解除后统一清理。

## 16. 真实代码边界与施工方法

本轮已核代码路径均相对 `yk-tako-eino/internal/runengine/`：

| 现有代码 | 当前事实与新增工作 |
|---|---|
| agents.go 的 AgentSpec、subagentShell | 严格规格与内存委派已有；新增 runtime/名单及持久 Flow 壳 |
| product_store.go 的默认/租户/有效规格 | 显式字段转换；新字段必须贯穿保存、更新、seed、导入导出 |
| agent_task_runner.go 的 assembleAgentTaskRun | 依赖 Task/组、重读规格、附 submit_task_result；只抽无 Task 状态含义的装配 |
| agent_task_store.go 与 types.go | Task 专用结果、固化和绑定；新增 Flow 结果验证和执行身份 |
| journal.go 的 replayJournal、worker.go | 普通缺结果不重跑；新增父 FlowWait 和子模型恢复前的 incident 检查 |
| agent_task_delivery.go | 非 bash 分支仍读取编排组；新增 Flow 来源解析、认领、输入和收尾 |
| workspace_engine.go、workspace_provider.go | 接口依赖 AgentTask 和 links；提取文件操作核，保留 Task 适配，新增 Flow 及旧发布替换 |
| sandbox.go、project_layout.go | 当前 bash 允许出站、content/runtime 已分离；新增专用沙箱、Flow 容器和清理 |

预计新增 `flow_{types,store,service,scheduler,turns,protocol,recovery,revision,api,tools,execution,workspace}.go`、固定 launcher 与 WASM 资源、`web/src/FlowView.vue` 和 Flow 验收脚本。不复制第三套模型循环或改造全仓成通用工作流平台。

施工包获批后按真实 API 契约、页面数据、存储、runner、持久子执行、恢复、文件发布、通知和 Vue 连续完成一个大任务。先实跑危险的求值重放和沙箱假设，失败如实报告。

没有无沙箱降级、内存调用替代、假页面或临时兼容开关。范围和施工包分别按仓库流程批准，不自动 commit、push 或 deploy。

## 17. 验收与完成线

确定性夹具负责安排反序完成与崩溃窗口，真实模型负责证明动态业务和产物。两者报告分开，不能用模型偶然按序返回证明重放正确。

| 用例 | 机器必须观测的结果 |
|---|---|
| 动态修复及命名委派 | 首轮列表后才创建 N 个调用，至少两轮；后台可继续聊天，命名结果回原父工具 |
| Promise 与闭包 | 强制 B 先到、触发 repairB；恢复时 A 已缓存仍重演 B，多 await mapper 请求与最终值一致 |
| 整轮崩溃 | 输入已选、求值未返、事务提交前、提交后未 ack；未提交新工作不执行，已提交不重复 |
| 子执行崩溃 | 调用绑定未启动、准备现场、intent、文件已写未记结果、固化未入 PG、结果未交付，各有状态和证据 |
| 未知副作用 | 程序在模型恢复前阻止，计数器不自动再增；补证据或放弃后才推进 |
| 父恢复及容量 | Flow 已建未写父结果时接回同 flowId；父 stop/deadline 传播，子位不足立即拒绝 |
| 双副本与旧代 | 同调用一执行绑定；旧 driver 发命令、旧 run 提结果拒绝，健康子结果不因 driver 死亡丢失 |
| 失去数据库的旧进程 | 旧 bash 仍活时不在同现场恢复新写者，不拿失租当停止证明 |
| 生命周期 | 暂停不新领取、在途保存；迟到控制版本冲突；提前 return 失败清理；停止不自行复活 |
| revision 与文件 | 完整前缀复用，差异整轮停止待确认，新现场后缀；已发布 A→B 再改 C 不叠补丁 |
| 发布替换及崩溃 | 新增/修改/删除/恢复均按 E/N 比较；第三种实际字节 blocked，逐路径 kill 后可收口 |
| schema | 错类型、重复键、未知关键词、缺字段判失败；合法 value:null 与 helper 失败可区分 |
| 安全及资源 | 构造器、动态求值、网络/文件/进程、协议伪造、无限循环/微任务、超内存、无沙箱均拒绝或有界终止 |
| 通知清理及 Vue | 来源忙不堵其他来源，attempt 防旧写，同步不双通知；引用不误清，桌面手机可操作并拿到真实文件 |

额外覆盖 return 已接受未发布、发布已执行未写终态、通知已认领未启动、交付 run 中断或失败窗口；每个窗口核对 PG 身份、事件、文件 hash、预算和通知去重，而非只查返回 200。

脚本入 `scripts/acceptance/flow_*.sh`，参数化 TAKO_BASE/TAKO_ENV/TAKO_PG_DSN，隔离 PG/Redis、测试账号与两副本，不用开发库 reset。runner 确定性测试必须一条命令重跑，两个 OS 的安全验收缺一不能宣布跨平台完成。

未来快测期望从 curl 实际字节录入 `contract_test.go`；expected.json 与 v6 人读验收同批更新，跑 assert_contract.py、check-expected.sh、spotcheck.sh。保留尚未断言原因，但关键恢复与安全不能借登记跳过完成线。

收尾在 yk-tako-eino 以 Go 专用 PATH 跑 test/vet/build，web 跑 typecheck/build 和浏览器验收；重跑 Task/DAG、修订、HITL、工作区、交付恢复、后台 bash、租户容量与普通 journal 回归。

交付证据为真实 curl、PG 轮次与身份、子 run、Provider 清单、文件字节、各 kill 窗口、双副本旧代拒绝、浏览器和机器退出码。API 返回 flowId、Node hello、内存 AgentTool 或只有流水页面都不满足完成线。

## 18. arena 综合记录

本轮共用同一需求和代码指针，并行得到 A、B、C 三份独立候选，无掉队。评选为需求覆盖、重放正确性、安全边界、实现复用、契约可施工、验收完整六项，每项 0–5；这些分数是审查判断，不是运行成绩。

| 维度 | 主审 A/B/C | 交叉评审 A/B/C |
|---|---|---|
| 需求覆盖 | 5 / 5 / 5 | 5 / 5 / 4 |
| 重放正确性 | 4 / 4 / 4 | 4 / 4 / 4 |
| 安全边界 | 4 / 4 / 4 | 4 / 3 / 4 |
| 实现复用 | 4 / 4 / 5 | 4 / 4 / 4 |
| 契约可施工 | 4 / 3 / 4 | 3 / 2 / 3 |
| 验收完整 | 5 / 5 / 5 | 5 / 5 / 4 |
| 合计 | 26 / 25 / 27 | 25 / 23 / 23 |

双方都选 C 作底稿。交叉评审虽给 A 总分较高，仍认为 C 的整批求值提交和独立执行适配更易维护；其低分项明确指出原稿不能直接施工。最终稿没有按总分机械选稿。

从 A 整合宿主恢复前处理未知结果、父工具持久接回和发布/交付细分 kill 窗口。从 A、B 的成功失败对象设计统一 JS 结果，消除 null 歧义。保留 C 的整批协议和独立 Flow 适配。

终审另外补写 controlVersion、防遗留调用假成功、容量预留、参数契约及 B/E/N 文件替换，避免把候选未解决处一起保留下来。

未采用 A 的半轮逐条提交与直接 C runner，前者恢复核对复杂，后者增加原生构建维护；未采用 B 的“父等待释放运行槽”，它会改变全仓容量定义。也不采用仅 callSeq 缓存、JS 堆快照、任意 key 跨版本复用或未知副作用自动重试。

三份都收敛到保存结果可见顺序，不把 JS 转为 DAG，且将文件现场与发布分开。模型配置文件不存在，本轮按 arena 默认标签发起调用，但执行模型路由未获核验，不能把本次独立交叉评审当作已经证明的跨厂商评审。

验证结果：完成静态契约、示例、引用及索引检查；运行验证全部仍待施工。Laziness Protocol 用于只抽真正共用的装配和文件操作；Separate Before Serializing Shared State 用于每调用独立现场；Prove It Works 用于区分本轮静态核对与未来真实验收，不把设计承诺写成通过事实。

## 19. 非目标与最终结果

不做 TypeScript/npm、任意 Node、JS 堆快照、静态 DAG 转译、Flow 嵌套、自动方式切换、脚本内提问、自修改、任意节点重跑或外部 exactly-once。外部写能力首期默认关闭。

这份方案让未知数量的工作按真实中间结果继续执行，崩溃后按原结果交付顺序恢复，并把“重新执行”“替换项目文件”“交付给用户”分别做成可检查的步骤。它解决的是动态编排的完整交付与恢复，不以多一个 JS 输入框作为完成。
