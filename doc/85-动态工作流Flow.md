# 85 · Dynamic Workflow（Flow）

> 性质：**方案（待点头；Flow arena 最终综合稿）**。base 为本文，移植 86-GPT 的 QuickJS-WASM 执行、未知副作用处理与严格结果契约，吸收 86-ds 的运行时前置能力验证；综合记录见 [Flow arena 最终综合记录](log/2026-11-20-Flow-Arena最终综合.md)。后续讨论已删去命名 Flow Agent 与同步子执行容量预留，第一版只保留临时 `/flow`。同日拍定本期采用 QuickJS-WASM 执行 JS，Node 只负责宿主通信与进程边界；动态计划与多 Agent 并非 Flow 独有，跨任务编排复用留到下一期根据真实需求另立方案。
> 状态：⬜ 未施工；本文只定义方案，不授权 coding。用户确认范围后，主 Agent 另写 `tmp/85-动态工作流/施工包.md` 交审，施工包点头后才能开始实现。
> 日期：2026-11-20。
> 代码落点：—（未施工；预计落点见 §11）。
> 参照：Claude Code [Dynamic workflows](https://code.claude.com/docs/zh-CN/workflows)；本地概念澄清见 [2026-11-14 讨论记录](log/2026-11-14-异步委派与DAG讨论.md)。参照外部行为，不把 Claude 未公开的内部存储与实现当成事实。
> 与 doc/82 的关系：doc/82 当时拍定「不增加用户可见 DAG 模式、不引入 JS 执行器」。本方案若获点头，将只对这两条形成新决定；bash、wait_task、现有 JSON DAG 与其余可靠性边界继续有效，施工时同步补记 doc/82。

## 0. 一句话与本质

增加第三种执行方式 **Flow**：用户通过 `/flow` 给出目标后，模型编写一段**受限 JavaScript**；脚本用循环、分支、`await` 和动态扇出决定接下来做什么，每次 `agent()` 由 Go 启动或接回一个普通子代理。一次 Flow 只保存一份不可变脚本，PostgreSQL 保存求值 step、调用历史与结果；崩溃后逐步重放已保存输入并核对整批输出，已执行工作不重复。

Flow 的本质是**运行中根据中间结果决定后续计划**，不是生成一张 DAG，也不是算法课里的 Dynamic Programming。为避免歧义，产品名统一使用 **Dynamic Workflow / Flow**。

第一版只交付 `/flow`：针对当前目标临时生成并后台运行脚本，来源会话可继续做别的事。不提供命名 Flow Agent，不把 Flow 脚本变成需要长期维护的 Agent 规格。

## 1. 背景与病灶

先澄清一个前提：**Todo 只记录主 Agent 的计划，本身不执行工作。**真正执行工作的是主 Agent、普通子代理或持久 Task。主 Agent 可以根据 Todo 和中间结果创建多个执行单元、用 `wait_task` 读取结果、修改 Todo，或通过 `revise_orchestration` 追加工作。因此，「动态计划 + 多 Agent」不是 Flow 独有能力。

| 方式 | 谁决定下一步 | 适合什么 | 代价或边界 |
|---|---|---|---|
| 主 Agent + Todo | 主模型逐轮决定 | 路线需要不断根据语义判断调整，例如复杂修复与方案探索 | 中间结果和协调过程持续占用主上下文，重复协调需要反复调用模型 |
| 普通子代理 | 被调用的一次 ReAct 执行 | 独立上下文内能完成的一件工作 | 父 Agent 等最终结果，不能由外层程序做大规模动态扇出 |
| JSON DAG + 持久 Task | 引擎按 PG 中当前已知的节点和边调度，主模型可按真实结果有界修订 | 真实依赖需要程序保证，例如测试、Lint 都完成后才能发布 | 结果驱动的重复循环需要主模型逐次创建或修订，协调结果会回到主上下文 |

DAG 并不要求一开始穷举全部未来工作。现有系统已经支持追加节点和边、取消尚未开始的节点，以及交付后的继任组；不能把「静态 DAG」与「动态 Flow」写成非此即彼的对立。

一个适合比较这些方式的真实场景是：

```text
用户要求「持续审查并修复，直到检查通过或连续两轮没有进展」
  → 先跑检查，预先不知道会报几类错误
  → 每类错误派一个修复 Agent
  → 审查修复结果，决定是否需要第二轮
  → 第二轮的错误集合可能完全不同
  → 达到通过、无进展或预算上限后停止
```

现有 ReAct + Todo + 持久 Task 已能完成这件事：主模型根据检查结果创建多个 Task，用 `wait_task` 读取结果，再通过同组修订追加第二轮。Flow 的区别是把「每类错误派一个 Agent、修复后重查、连续两轮无改善就停止」这些协调规则写成程序，而不是每次结果回来后都由主模型重新决定。

所以 Flow 不是动态计划或多 Agent 能力上的从无到有。它的增量是：

1. **减少主模型逐轮协调**：明确、重复的规则由程序持续执行，不靠模型每一轮都记住并重复选择。
2. **降低主上下文压力**：中间结果留在脚本变量与持久调用记录中，由脚本过滤、合并和循环，主模型不必逐批装下全部协调过程。
3. **让协调过程可逐步重放核对**：PG 保存每次结果输入及其产生的整批行为，恢复时核对程序实际走过的路径。

这些属于执行方式、成本与可核对性的改进，不等于现有系统不能完成同类目标。每一步都需要语义判断时，ReAct + Todo 更合适；只需表达真实依赖与增量修订时，现有 Task / DAG 已经足够；只有协调规则稳定、重复且适合程序化时，Flow 才有明确增量。没有这种真实需求，不应为了对标 Claude 而新增 Flow。

这里的「动态」是脚本运行中根据结果选择分支、循环和追加调用，不是主模型持续改写脚本；第一版脚本创建后不可变。

但这不等于「让 Node 跑一段代码」就完成了。若脚本在 30 个 Agent 完成后崩溃，直接从第一行普通重跑会重做全部工作；若两个副本同时执行同一脚本，还会产生双份 Agent 和重复交付。因此本方案的完成线同时包含**动态表达、持久结果、恢复、防重、权限与真实页面**，不是多一个 JS 语法输入框。

## 2. 已拍定的设计方向与待审默认选择

### 2.1 本 step讨论已经明确的方向

1. JS 是执行控制逻辑，不把 Flow 强行转换为静态 DAG，也不要求保存 nodes/edges。
2. Go 向 JS 暴露内部「执行普通 subagent」宿主 API；JS 不直接执行 bash、文件、MCP 或网络工具。
3. 普通 subagent 继续复用现有 Agent 规格与 Go 执行能力；Flow 只新增外层控制与持久调用。
4. `/agent`、`/dag`、`/flow` 是三个可见入口；Flow 与现有 JSON DAG 是不同执行契约。
5. 页面以阶段、调用轨迹和结果为主，不做 A2UI，不把 Mermaid 当运行协议。
6. 第一版采用纯 JavaScript，不引入 TypeScript 擦除或转译步骤。
7. Flow 只能调用普通 Agent；第一版不允许 Flow 嵌套 Flow。
8. 一次 Flow 一份不可变 JS；历史调用与 prompt 只增不改，常规重规划在同一 Flow 内跳过未开始工作、追加新调用，不通过修改历史脚本实现。

### 2.2 本文给出、仍需随方案一起确认的默认选择

- **Node 子进程 + stdio NDJSON + QuickJS-WASM**，而不是 Node 直接执行模型脚本或 Go 进程内嵌入 JS 引擎。Node 只负责宿主通信、协议与进程边界，模型脚本在固定的 QuickJS-WASM 中执行；通过 QuickJS 的待处理作业接口主动驱动并排空本轮 Promise 作业，不能用 `setImmediate` 猜测求值边界。代价是生产运行同时新增 Node.js 与 QuickJS-WASM 依赖，并须验证 JS 兼容性、宿主 Promise 桥接和资源限制。
- 斜杠是显式选择：`/agent` 不暗中把当前请求切成 `/dag` 或 `/flow`，主 Agent 也不通过委派工具创建 Flow。主 Agent 可以建议用户换模式。
- 普通崩溃恢复沿用产品的**显式继续**语义，不做服务端无限制自动重放；调度对账负责发现中断、保留状态和防止重复。
- 一次 Flow 的 JS 与 args 在创建后不可变，不开放运行中人工编辑、脚本 revision、跨 revision 缓存复用或历史 prompt 修改。
- 不设置 JS 执行时间或步数自动判定；提供持久执行历史、心跳和用户停止。进程 CPU/内存、Agent 数、并发与费用仍须有资源上限，不能让观测代替保护。

### 2.3 Anthropic 的参考价值与编排复用延期决定

Claude Code 的 Dynamic workflows 除了大规模脚本协调，还强调把脚本保存为命令、传入不同 `args`、通过插件分发，让同一套协调规则可以跨任务重复执行。这个价值属于「编排复用」，不能自动变成本文第一版的价值：本文只有临时 `/flow`，而 Tako 已有 ReAct + Todo + 持久 Task + 可修订 DAG。是否施工仍须由真实的大规模、重复、可程序化协调需求证明，不能只以「Claude 有」为依据。

本文区分两件容易混名的事：

- **恢复重放**：同一 Flow 中断后，使用同一不可变脚本和已保存结果继续执行。它解决「这次工作崩溃后怎样不重复已成功工作」，属于本期完成线。
- **编排复用**：把一套协调规则保存为可命名的 Workflow，传入不同参数，多次创建相互独立的 Flow。它解决「下一批文件、下一个项目怎样再次使用同一套协调规则」，**第一版不做，下一期根据真实复用需求另立方案评估**；这不是无条件承诺下一期一定实施。

下一期若启动，建议把「可复用编排定义」与「一次 Flow 执行」分开：定义保存名字、脚本、参数约束和版本；每次执行固定一个定义版本与参数快照。修改定义产生新版本，不改写历史 Flow；跨 Flow 默认不沿用旧 Agent 结果，避免把旧任务的副作用当成新任务已经执行。具体 API、存储、权限、共享和版本迁移由下一期方案拍定，本期不提前实现命名模板、命令、插件分发、脚本 revision、`inputHash` 或跨执行结果复用。

## 3. 范围与非目标

### 3.1 第一版完整范围

- 对话输入框提供 `/agent`、`/dag`、`/flow`；不输入时默认 `/agent`。
- `/flow` 规划 run 中提供 `start_flow`，模型提交目标、不可变脚本、参数和允许调用的普通 Agent 名单。
- Go 启动固定、可信的 Node 通信 runner，通过 stdio 交换有界消息；模型脚本只作为数据交给 runner，由其中的 QuickJS-WASM 执行。
- JS 提供 `agent / parallel / pipeline / phase / log / args`，支持动态循环、分支和结构化结果。
- Go 对有 outputSchema 的结果做程序校验；helper 用显式成功/失败对象，合法 `null` 不与失败混淆；脚本 return 前必须收拢所有已发起调用。
- 子代理恢复前程序检查未知工具结果，存在未知副作用时先进入 `needs_review/outcome_unknown`，不先恢复模型再靠提示词克制。
- PG 保存 Flow、不可变脚本、求值 step、append-only 调用历史、结果版本、执行代次与来源归属。
- 同一 Flow 崩溃恢复、暂停/继续、停止、失败可见；逐步重放并比较整批行为，成功子代理不重复执行，不做脚本编辑或跨版本缓存。
- 双副本下同一 Flow 只有一个有效驱动者，同一调用只有一件执行工作，最终通知与交付不重复；每次子调用按现有租户容量规则执行。
- 对话 Flow 卡与右侧 Flow 面板展示脚本、执行历史、求值 step、阶段、调用轨迹、结果、重放核对、心跳和暂停/停止/恢复。
- OTel/Langfuse 关联 Flow、求值 step、Agent 调用、执行历史与重放核对。
- 单测、契约、真实模型、真实 Node + QuickJS-WASM、双副本和 `kill -9` 活体验收。

### 3.2 明确不做

- 不支持任意 Node.js 程序、npm 包、`import/require`、直接文件系统、shell、网络或数据库访问。
- 不支持 TypeScript、JSX、浏览器 DOM、A2UI 或以 Mermaid 为执行真源。
- 不提供运行中人工修改脚本、在线编辑器、脚本 revision、inputHash、跨 revision 结果复用或后缀失效；需要独立目标或原样完整重跑时另建 Flow。
- 不提供命名 Flow Agent、Flow 专用 Agent 规格或主 Agent 同步委派 Flow；不为这种入口设计父子容量预留。
- 不做跨任务**编排复用**：不能把一次 Flow 保存为命名 Workflow／命令后带不同 `args` 反复实例化，也不做共享或插件分发；下一期按真实复用需求另立方案，边界见 §2.3。同一 Flow 的崩溃重放不属于此项，仍是本期完成线。
- 不允许 `Flow → Flow`、普通子代理再创建 Flow，或任意深度递归委派。
- 不做自动复杂度路由器，不在 `/agent` 中未经用户选择临时生成 ad hoc Flow。
- 不做脚本内通用提问或自由问答；子 Agent 需要业务澄清时结束当前 Flow，由主 Agent 追问后按用户选择继续。`outcome_unknown` 是安全处理项，不受这条限制：Flow 进入 `needs_review`，等人提交程序可验证的证据或放弃调用后在原 Flow 继续。
- 不做任意历史节点单点重跑、自动数据依赖推断或完整 JS 静态数据流分析；也不承诺 100% 静态识别死循环。
- 不用执行历史代替资源保护，不默认按 JS 执行时间或步数自动终止。
- 不承诺外部副作用 exactly-once；已执行但结果未知的工具仍按现有中断语义处理。
- 不复制一套简化 Agent 执行器，不改变现有 JSON DAG、同步子代理和后台 bash 的产品语义。
- 不在第一版新增 Flow 专属 worktree、跨结果文件合并或已发布版本替换；文件读写继续走普通 Agent 现有权限与写入保护，未来确有并行修改需求另立方案。

## 4. 三个用户入口

| 命令 | 展示名称 | 控制逻辑 | 权威执行状态 | 第一版行为 |
|---|---|---|---|---|
| `/agent` | 主 Agent | 主模型逐轮决定 | 现有 run、run_tasks、子代理 wire | 直接工作、Todo、同步委派普通子代理；不能创建或调用 Flow |
| `/dag` | 声明式 DAG | 模型提交 JSON 节点和边 | 现有 agent_orchestrations / agent_tasks / links | 使用 create_orchestration，保留有界修订与继任组 |
| `/flow` | Dynamic Workflow | 一份不可变脚本与脚本变量 | 新 flow_runs / agent_calls / events | 模型用 start_flow 创建后台 Flow，页面随后观察 |

斜杠不是只改前端颜色或提示词标签。创建 run 时把 `executionMode=agent|dag|flow` 持久化，worker 据此装配允许的工具；resume 读取同一字段，不能刷新后变成另一种模式。

对现有默认行为的变化必须如实记录：`/agent` 不再临时创建 JSON DAG，用户明确 `/dag` 才进入该路径；只有用户明确 `/flow` 才创建 Flow。

## 5. 端到端业务链

### 5.1 `/flow`：一次性动态工作流

```text
用户输入 /flow + 目标
  → 创建 executionMode=flow 的普通规划 run
  → 主模型生成受限 JS、结构化 args 与 allowAgents
  → start_flow 校验脚本、归属、配额与 Agent 名单
  → PG 同事务写 Flow、不可变脚本、args 与来源关系，返回 flowId
  → 当前规划 run 结束，页面出现 Flow 卡，来源会话可继续其他对话
  → 有效副本取得 Flow 驱动租约，启动固定 Node 通信 runner + QuickJS-WASM
  → JS 执行到 agent()，向 Go 发宿主调用
  → Go 读取本 Flow 已保存结果或创建独立子代理 session/run，结果落 PG
  → Go 把结果回给 JS，JS 根据结果继续循环/分支/扇出
  → 最终 return 写入 Flow 结果并发出来源会话通知
  → 来源会话空闲时交付一次，页面重拉 PG 权威状态
```







规划 run 崩溃后的恢复必须处理「Flow 已创建、`start_flow` 工具结果还没写回」的窗口：journal 重放遇到该 toolCall 时，按 `(source_run_id, create_idempotency_key)` 查 Flow；已成功则注入原结果，仍在跑则接回等待，失败/中断则把 flowId 和真实状态交给模型，不得盲目再建第二个 Flow。

## 6. JavaScript 契约

### 6.1 示例

```js
const found = await agent(
  "列出需要审查的 API 文件，返回 files 数组",
  { agent: "researcher", key: "discover", outputSchema: {
    type: "object",
    required: ["files"],
    properties: { files: { type: "array", items: { type: "string" } } }
  } }
)

phase("逐文件审查")

const reviews = await pipeline(found.files, (file, index) =>
  agent(`审查 ${file} 的鉴权问题，只报告有依据的问题`, {
    agent: "researcher",
    key: `review:${file}`,
    label: `审查 ${index + 1}/${found.files.length}`
  }),
  { concurrency: 4 }
)

const valid = reviews.filter(x => x.ok).map(x => x.value)
if (valid.length === 0) {
  return { status: "no_result", message: "没有取得有效审查结果" }
}

return agent("汇总这些审查结果并去重：" + JSON.stringify(valid), {
  agent: "researcher",
  key: "summarize"
})
```

脚本主体由可信 runner 包进受控异步函数，支持 `await` 与 `return`。脚本不是 ES module，没有导入和导出。

### 6.2 宿主 API

```text
agent(prompt, options) -> Promise<text | JSON value>
parallel(thunks) -> Promise<Array<FlowCallOutcome>>
pipeline(items, mapper, options) -> Promise<Array<FlowCallOutcome>>
phase(name) -> void
log(text) -> void
args -> 创建时传入、深冻结的 JSON 值

FlowCallOutcome =
  {ok:true, value:text | JSON value} |
  {ok:false, error:{code:string, message:string}}
```

`agent.options` 第一版只认：

| 字段 | 必填 | 含义 |
|---|---|---|
| `agent` | 是 | 普通 Agent 名，必须在 Flow 的 allowAgents 快照内，且 runtime=agent |
| `key` | 否 | 人可读稳定标识，只用于页面、日志与诊断；同一 Flow 内不得重复，不参与跨版本比较 |
| `label` | 否 | 页面短标题，不改变执行身份 |
| `outputSchema` | 否 | 子代理最终结构化输出约束；缺省返回文本 |

未声明字段一律拒绝，不静默忽略。模型不能从 JS 传任意 modelRef、工具白名单、项目路径、租户身份或宿主文件路径。

`parallel` 接收零参数函数数组，先按输入下标预留调用序号再同步调用 thunk，返回顺序与输入一致；`pipeline` 同样按 item 下标预留序号，并限制同时等待的 Agent 数。thunk/mapper 内的额外 await 与 Agent 完成先后都不改变预留序号；未使用的预留号不复用。

第一版失败语义：

- 直接 `await agent()` 的业务执行、JSON 解析或 outputSchema 校验失败抛出可识别的 `FlowAgentError`，作者可显式 `try/catch`；
- `parallel/pipeline` 等待本组全部结束，逐项返回显式 `{ok,value|error}`；成功的 `value:null` 仍是 `{ok:true,value:null}`，不与失败混淆；
- PG 写失败、身份越权、协议错误、驱动租约丢失属于宿主故障，不能转换成普通业务失败继续推进；
- `mapper/thunk` 自己抛错使脚本失败；已启动的 Agent 仍按真实状态落库，不能假装从未发生；
- 未知副作用与 `outcome_unknown` 不是可跳过的一般 helper 失败，必须走 §9.3 的处理项。

### 6.3 确定性、资源与安全边界

脚本及参数按 UTF-8 字节设上限，Agent 数、并发数、日志数、消息大小、结构化结果大小和最终返回值均设上限；具体数值由施工包用真实 Node、QuickJS-WASM 与模型校准，不能无界。

第一版**不设置 JS 执行时间或执行步数自动终止**。死循环无法 100% 静态识别，明显模式可以提示但不据此宣称安全。runner 必须作为独立进程设置 CPU、内存与输出资源约束，并受 Agent 数、租户 token、并发和费用预算约束；页面通过执行历史与心跳暴露失控状态，由用户显式停止，不能只看历史而没有任何资源保护。

脚本环境不暴露：

```text
import / require / dynamic import
process / Buffer / fs / child_process / worker_threads / vm
net / http / https / fetch / WebSocket
直接 PG / Redis / MCP / bash / 文件工具
setTimeout / setInterval / 其他墙钟等待
Date.now / 无参 new Date / Math.random / 随机 UUID
eval / Function / 动态代码再执行
```

普通数学、字符串、数组、对象和 JSON 操作可用。需要时间与随机性时必须由创建参数显式传入，保证同一不可变脚本的重放得到相同调用顺序。

QuickJS-WASM 或删除全局变量**不能单独当安全边界**。runner 还须置于专用 seatbelt/bwrap 配置：Node 安装、QuickJS-WASM 与固定 runner 只读，无项目写权限，无网络；stdio 是唯一宿主调用通道。Linux Flow runner 与当前 bash 不同，必须禁用网络 namespace；没有可用沙箱时 Flow 与 bash 一样拒绝执行。

部署固定 Node、QuickJS-WASM 与 runner 协议版本，升级走完整回归，不在不同副本间各自选择运行时版本。

### 6.4 结构化结果与脚本完成校验

`outputSchema` 只约束子代理最终结构化输出，不声明 `agent()` 的调用参数，也不是只写进 prompt 的希望。Go 在调用创建前编译一个有界 JSON Schema 子集，并在结果进入成功状态前程序验证；第一版只支持 type、properties、required、additionalProperties、items、enum、const、长度与数值范围，拒绝 `$ref`、format、自定义函数、pattern 和未知关键词，不静默忽略。

结构化输出必须是单个严格 JSON 值，不从自然语言猜 JSON，不剥代码围栏后宣布成功。重复 JSON 键、非有限数、超深度或超字节一律失败。校验失败回 `FlowAgentError`，不自动重新执行模型或工具；成功值可以是 `null`，由显式 ok 标记区分。

脚本 `return` 时，Go 与 runner 必须核对没有未等待的 Agent 调用、未处理 rejection 或仍在运行的后台命令；存在则回 `failed/unjoined_calls` 并做失败清理，不能让漏写 `await` 的脚本假装成功。`Promise.race` 的输家也须明确收拢后才能 return。

## 7. Go ↔ Node 求值 step协议

Go 永远启动仓库内固定 Node runner，由 runner 把脚本装入 QuickJS-WASM，而不是拼接 `node -e <模型代码>`。脚本、args 和配置通过首条 `start` 消息传入。传输采用 stdio NDJSON，stdout 只准协议，诊断只能去 stderr；每行有字节上限，未知消息、非法 ID、重复终态或协议版本不符立即失败。

仅按 B→A 的顺序 resolve 两个 Promise 不够：同一检查点与分检查点处理可能产生不同的后续调用。因此协议的基本单位不是单个 `host_result`，而是**求值 step**：

```text
Go → Node
  start {protocolVersion, flowId, script, args, limits}
  evaluate {stepSeq, agent_output:null | {callSeq, ok, result | error}}
  committed {stepSeq}
  cancel {reason}

Node → Go
  step {stepSeq, agent_input:[agent | phase | log],
        state:waiting | returned | thrown, value?, error?, pendingCount}
  heartbeat {}
  protocol_error {message}
```

每个 step严格按下面顺序推进：

1. Go 选择启动输入 `null`，或一个已有终态的 Agent 结果；先在 `flow_steps` 写入 `evaluating` 行，固定 stepSeq 与 agent_output，再把输入发给 runner。
2. runner 在 QuickJS-WASM 内执行同步代码，并通过待处理作业接口主动驱动、排空调用该输入后可运行的全部 Promise 作业；期间只缓冲 agent/phase/log/return，不执行任何新 Agent。
3. runner 将**整个 step 的 agent_input 批次**、pendingCount 和 waiting/returned/thrown 一起返回。QuickJS-WASM 的作业队列接口、宿主 Promise 桥接与“队列空 = 当前无可运行 JS”的语义仍须在 §14.0 实测，不能用“setImmediate 大概会晚一点”冒充证明。
4. Go 校验整个 step、驱动 epoch、预算和 agent_input 顺序，在一个短事务内把 step 从 evaluating 改为 accepted，同时写调用行与历史；任一非法则整批拒绝，新 agent_input 不得执行。
5. 事务提交后 Go 才领取其中的 Agent 调用，并回 `committed`；runner 收到确认前不接受下一结果。
6. Go 随后选择下一项结果进入下一个 step。首次执行一次只向 JS 交付一个结果，step历史自然记录实际结果可见顺序。

恢复时从 step 1 开始逐步喂入相同 agent_output，排空作业并比较整批 agent_input、控制输出、pendingCount 与最终值；历史 step 未全部核对前不得执行新 agent_input。差异进入 `failed/replay_mismatch`。evaluating 行的输入已固定，崩溃后可重求该 step；accepted 行已提交的调用只接回，不重复执行。

callSeq 仍在一个 step 的 agent_input 生成时按顺序分配，管“哪次调用”；stepSeq 与 agent_output 管“哪次结果输入后产生了哪一批行为”，结果可见顺序由 step 输入直接表达。`parallel/pipeline` 仍在 runner 内组合，Agent 可以并行执行，但结果一次一个 step 交给 JS。

心跳只更新 `last_heartbeat_at` 和页面健康状态，不参与业务裁决。无限同步循环或无限微任务若使本 step无法返回，表现为历史停最后活动与心跳失联，由 §9 的停止和资源边界处理，不另设时间/步数自动判死。

## 8. 持久模型

Flow 不存强制 nodes/edges。JS 的变量、条件与 `await` 已表达控制关系，PG 保存的是**一份不可变脚本、求值 step、实际调用事实与 append-only 执行历史**。页面默认是执行历史/阶段轨迹，不宣称能从任意 JS 自动推出完整 DAG。

### 8.1 `flow_runs`

至少保存：

```text
id / tenant_id / user_id / workspace_id
source_kind = flow_slash
source_session_id / source_run_id / create_idempotency_key
goal / status / script / args_json / current_phase
allow_agents / agent_snapshot_json
driver_lease_epoch / driver_lease_until / driver_owner / last_heartbeat_at
final_result / error_code / error_message
created_at / started_at / finished_at / updated_at / deleted_at
```

`flow_slash` 来源对 `(source_run_id, create_idempotency_key)` 建唯一约束，防止规划 run 恢复时重复创建 Flow。`agent_snapshot_json` 保存 Flow 启动时解析后的允许 Agent 规格与策略，不保存解密后的模型密钥；正常恢复不因子代理配置在别处被维护而悄悄换执行者。

`script` 与 `args_json` 创建后不可变。没有运行中脚本编辑器、脚本版本表或跨版本结果复用。

### 8.2 `flow_agent_calls`

```text
id / flow_id / call_seq / node_key / phase
agent_name / prompt / options_json
status / execution_session_id / current_run_id
created_step_seq / settled_step_seq
result / result_revision
error_code / error_message
incident_json / incident_version
created_at / started_at / finished_at / updated_at
```

核心唯一性：

- `(flow_id, call_seq)` 唯一；
- 非空 `node_key` 在同一 Flow 内唯一；
- 执行 session/run 各自只能绑定一个调用；
- 结果写回必须匹配当前 run 与执行代次，旧执行者不能覆盖。

callSeq 在一个 step 的 agent_input 生成时按顺序分配，不能在 Agent 完成时编号。已接受 step 中的同序号 agent_input 若与已存 agent/prompt/options 不同，属于 runner 或协议错误，必须失败并暴露；通过比较已存调用内容完成，不需要 inputHash。

`incident_json` 保存未知工具结果的 journal 身份、动作摘要、处理决定和证据引用，`incident_version` 防两个处理者互相覆盖；它不是通用事故平台，也不允许人工摘要把未验证副作用写成已核实。

### 8.3 `flow_steps`

```text
flow_id / step_seq / status = evaluating | accepted
agent_output_json / agent_input_json
state / value / error / pending_count
created_at / accepted_at
```

`(flow_id, step_seq)` 唯一。evaluating 行固定本 step输入；accepted 行还固定完整输出批次。首个 step agent_output=null，此后每个 step至多一个 Agent 结果。`pendingCount` 统计未收拢的受跟踪 Promise：未落定，或已落定但没有被 await/then/helper 收拢都计入；parallel/pipeline 的组合 Promise 也纳入跟踪，被 helper 接管的成员不重复计数，因此 Promise.race 输家不能漏掉。accepted step不可原地改写，恢复只重算并比较；崩溃留下的 evaluating 尾行用同一输入重求，不另选另一个就绪结果。

### 8.4 `flow_events`

```text
id / flow_id / event_seq / kind / call_seq / step_seq
text / payload_json / created_at
```

事件种类至少包括 `phase / log / step_accepted / agent_requested / agent_running / agent_result / replayed / resumed / incident_opened / incident_resolved / failed / stopped`。`event_seq` 只表示真实展示顺序，不参与重放身份。

执行历史必须持久可见：step/call 状态变化与对应历史尽量同事务写入。Flow/call/step 行是当前状态与恢复依据，events 是 append-only 审计与页面历史，不通过重放 events 重新裁决业务状态。心跳只更新 Flow 行，不逐拍写 events。

### 8.5 通知与变更事件

`/flow` 后台完成复用现有持久通知与交付恢复链，增加 Flow 来源和 `flow_finished` 类型，不另造一套只存在于内存的通知。

Flow 创建、状态翻转、暂停/停止/恢复成功后发现有租户级 `flows_changed`；事件只让前端失效重拉，不带状态当真源。临时唤醒不是审计存储，PG 对账必须兜住丢事件。

## 9. 执行、重放、容量、动态计划与超时

### 9.1 accepted step中的 Agent 调用

step 未 accepted 前不得产生新工作。提交后，Go 才逐项处理其中的 agent 调用：

```text
1. 校验 agent_input 属于当前 Flow、accepted step 与 driver lease epoch
2. 按 callSeq 读取同事务写入的调用行，校验 allowAgents 与预算
3. 已有执行 run 且仍健康 → 接回，不创建第二件
4. 已落调用但尚未启动 → 在事务内绑定独立执行 session/run，提交后启动
5. 已成功或失败 → 只读取真实结果，不重复执行
6. 子代理终态、调用结果和历史在持久链收口
7. Go 选择一个结果写入下一个 step evaluating 的 agent_output，再交给 runner
```

调用与执行 run 的创建不持有模型调用期间的 PG 事务。模型与工具运行长，事务只负责执行权、状态、step、历史和结果落库。Node 丢失不撤销已接受工作，也不允许未接受批次偷偷执行。

### 9.2 同一不可变脚本的崩溃恢复

Node 与 QuickJS-WASM 的内存都不是恢复真源。显式继续后：

```text
新 driver epoch +1
  → 从同一不可变脚本与 step 1 开始重放
  → evaluating 尾行使用已固定 agent_output 重求；accepted 行逐步喂入原 agent_output
  → 比较整批 agent_input、控制输出、pendingCount 与最终值，全程禁止执行重放 agent_input
  → 任一偏离进入 failed/replay_mismatch，不提前执行差异批次
  → 历史 step全部一致后，接回 queued/running 子代理并选择下一项结果
  → 子代理也已中断时先扫描 journal；有 intent 无 result 则进入 needs_review，解决后才按 run epoch+1 续跑
  → 旧 driver 与旧 runner 的所有写操作因代次不符失效
```

机器时钟、随机数和缓存就绪顺序不能改变历史 step。这里不做 inputHash、跨脚本版本复用或后缀失效。

### 9.3 暂停、停止、失联与完成

- **暂停**：持久化 pausing；停止接受新 step和新领取，在途子代理可完成并保存结果，随后退出 Node，状态为 paused。继续时逐步重放。
- **停止**：停止 Node 并请求停止当前子代理；已保存结果与历史保留，Flow 为 stopped。重新处理必须显式继续，不能后台自行复活。纯 JS 死循环无法读取 cancel 时，停止必须直接杀 runner 进程组，不能等它优雅响应。
- **中断**：driver 或子代理租约失效时读侧派生 interrupted 并进入处理提醒；对账只补状态和通知，不把 interrupted 当成可以无条件重新执行外部动作。
- **失联**：runner 心跳停更但 Go 驱动仍存活时，页面派生 unresponsive；不据此自动创建第二个 driver，也不自动按时间/步数杀掉。用户可以从历史看到最后活动并显式停止。
- **重放不一致**：进入 `failed/replay_mismatch`，保留历史用于诊断；不在原 Flow 内忽略差异或继续执行，需要新目标时另建 Flow。
- **未知结果处理**：子模型恢复前程序扫描 journal；普通工具有 intent 无 result 时，Flow 进入 `needs_review/outcome_unknown`，不得先恢复模型再靠提示词要求它不要重发。处理项只允许补交程序可验证的结果证据或放弃整个 Agent 调用；本期证据只认与未知动作对应的实际文件路径 + SHA-256 或已经持久化的真实工具结果，自由文本和外部 URL 不算程序验证。接受证据后沿原 session/run、epoch+1 续跑；放弃则不再恢复子模型，把整个 Agent 调用以 `failed/outcome_abandoned` 回给 JS。不提供“忽略未知副作用并一键重跑”。
- **完成**：runner 只在 returned step 被 Go 接受、所有调用收拢后才算脚本完成；JS 自己 return 而 PG 未确认不算完成。

### 9.4 同一 Flow 内 append-only 动态计划

正常重规划不修改历史脚本、旧 prompt 或旧结果：

- 已执行调用是历史，不能原地改掉；
- 尚未发起的未来工作可以不发起，已经在等待但尚未开始的调用可以标 cancelled；
- 新工作以新的 callSeq 追加；
- 例如旧 summary 尚未执行时可以跳过它，再发起一个新的 summary；旧 summary 已执行则保留其结果，新版总结是另一次调用；
- 常规重规划仍属于同一 Flow。只有用户放弃整个旧目标、旧脚本，或明确要求独立完整重跑时才新建 Flow；
- Agent 数、并发与费用预算始终有效，不能借 append-only 无限追加。

因此第一版没有在线脚本编辑、revision API、baseRevision、inputHash、复用旧脚本前缀或后缀失效。

### 9.5 超时不是统一失败或重试信号

- **JS 死循环或无限微任务**：不设执行时间或步数自动判定，step不返回时靠历史、心跳、资源限制与用户停止；
- **`agent()` 等待很久**：属于正常长任务，JS 持久等待，不因等待时间长就创建第二个 Agent；
- **子代理/模型无响应**：沿用现有 stall、租约和恢复机制，先分清仍在工作、明确失败与结果未知；
- **bash 前台等待结束**：沿用现有后台 job 机制，不等于命令失败；
- **租约过期**：首先表示中断和执行权需要重新裁决，不表示可以安全重做；
- **结果未知**：可能已有副作用，不自动重试；只有明确失败才交给 JS `try/catch` 或追加新的计划调用。

核心原则是：超时不等于失败，更不等于可以安全重做。

## 10. 子代理执行复用与权限

现有同步 `AgentTool` 的直接内存等待不能单独承担 Flow 的持久调用。施工应抽出 Task runner 与普通子代理装配中可复用的「按 AgentSpec 创建独立执行 session/run、装配模型与工具、提交结果」服务，由现有 Task 与 Flow 共同消费；不得复制第三套模型循环、权限判定和结果格式。

Flow 中每个普通 Agent 调用：

- 有独立 execution session/run 与上下文；
- 使用被调用普通 Agent 的模型、工具、沙箱与记忆快照政策；创建 Flow 时冻结有效 model ref、完整系统指令与工具策略，不保存密钥，恢复不重新读取已修改的 Agent 正文；
- 可以拥有自己的 run_tasks Todo，但剔除 `ask_user`、计划审批、Task/DAG/Flow 创建与等待工具；需要业务澄清时以明确失败结束，不在子 run 里偷偷挂起等人；
- 外层 JS 只拿最终文本或结构化结果，不接收全部中间模型上下文；
- 完整 runlog 留在子代理执行记录，Flow 保存结果引用与必要副本。

Flow 本身不直接获得 bash/文件/MCP 权限。`allow_agents` 缺省不表示全员开放；`start_flow` 必须提交并校验名单。只能引用普通 Agent，不能引用 Flow 或通过改名绕过嵌套限制。

可见性、项目归属、角色、日 token、租户并发与存储额度沿用现有 Task/run 规则；Flow 驱动与子代理合计不能绕过租户容量。跨租户或跨用户 ID 一律由服务端从上下文裁决，不接受脚本传入。

## 11. 现有代码接缝与预计施工面

以下只是施工面地图，不表示已经实现：

| 领域 | 现有接缝 | 预计新增/调整 |
|---|---|---|
| 子代理执行 | `agents.go`、`agent_task_runner.go`、`worker.go` | 抽共享持久 Agent 执行服务；Flow 不直接复用无持久身份的内存等待 |
| 运行模式 | `server.go` run 创建、`worker.go` 工具装配、`agent_task_tools.go` 执行方式提示 | 持久 executionMode，按 agent/dag/flow 收窄工具与提示 |
| Flow 运行时 | 无 | `flow_{types,store,service,runner,protocol,recovery,api,tools}.go`；固定 Node 通信 runner + QuickJS-WASM 与协议测试 |
| 沙箱 | `sandbox.go`、`subprocesspath.go`、`bash.go` | 专用无网络 Flow runner profile；macOS/Linux 分流与拒绝路径 |
| 通知与事件 | `agent_task_delivery.go`、`events.go`、`questions.go` 等现有持久机制 | Flow 来源、完成通知、处理提醒和 flows_changed；不复制内存通知 |
| 存储 | `pgstore.go`、`store.go` 的迁移、租约、容量与删除级联 | Flow 四表（run/call/step/event）、不可变脚本、求值 step、唯一约束、执行历史、项目/会话删除与归档清理 |
| 观测 | `trace.go`、`trace_run.go`、`internal/observability/` | Flow 根、求值 step、宿主调用、重放核对、执行历史与心跳属性 |
| 前端 | `App.vue`、`ChatDock.vue`、`api.ts`、`chat.ts`、`mutation.ts` | 斜杠命令、Flow 卡、`FlowView.vue`、执行历史、阶段/调用轨迹、心跳、脚本与停止操作 |
| 验收 | `contract_test.go`、`scripts/acceptance/expected.json` 与现有 Task/crash 脚本 | Flow 契约用例和 `flow_{single,dynamic,crash,replica,security,history}_live.sh` |

没有临时实现或过渡开关可以替代完成线。特别是「Node/QuickJS-WASM 能打印 hello」「Go 直接调内存 AgentTool」「只有假调用数据画页面」都不算 Flow 落地。

## 12. API、页面与观测契约

### 12.1 API 面

施工包先用 curl 钉死真实字节，至少覆盖：

```text
POST /api/runs                         接收并持久化 executionMode
模型工具 start_flow                    创建后台 Flow，立即返回 flowId/status
GET  /api/workspaces/:id/flows         项目列表，可选 sessionId 过滤
GET  /api/flows/:id                    不可变脚本、当前阶段、调用、结果与错误
GET  /api/flows/:id/steps              求值 step，按 stepSeq 有界分页
GET  /api/flows/:id/history            持久执行历史，按 eventSeq 有界分页
POST /api/flows/:id/pause
POST /api/flows/:id/resume
POST /api/flows/:id/stop
POST /api/flows/:id/calls/:callId/resolve  处理 outcome_unknown，带 incidentVersion、decision 与证据
```

所有新 JSON 接口使用统一 `{code,message,data}`；HTTP 状态、业务码、data 键序、错误文案、权限与幂等冲突在施工包定义并进入 `expected.json` 和 `contract_test.go`。本文不把未实跑示例字节冒充已验收契约。

### 12.2 页面

聊天输入框的斜杠菜单只展示三个执行方式及简短解释；`/agent` 是默认，不需要用户重复输入。

Flow 在对话中显示一张普通卡：目标、状态、当前 phase、Agent 成功/运行/失败数、缓存复用数与打开入口。点击在右侧工作区打开 Flow 标签，包含：

- **历史**：按 eventSeq 展示 phase、log、step accepted、Agent 请求/等待/结果、重放、恢复、失败和停止；可按 stepSeq 打开每个 step 的 agent_output 与完整 agent_input 批次；
- **运行**：按 phase 与 callSeq 排列当前调用，完成顺序可以不同但调用序号稳定；
- **脚本**：本次 Flow 唯一且不可变的 JS 与 args，只读，不提供在线编辑或历史版本；
- **结果**：最终返回值和各 Agent 结果，标明新执行或缓存复用；结构化失败与合法 null 分开显示；
- **操作**：暂停、继续、停止，显示心跳与 unresponsive；`outcome_unknown` 显示处理卡，提交证据或放弃调用；权限不足时不显示写操作。

不根据模型自由文本拼 UI，不要求 Mermaid，也不在前端重建执行状态；所有状态来自 Flow API。

### 12.3 观测

OTel 根 span 以 Flow 为生命周期，子 span 至少包括每个求值 step、宿主 Agent 调用、结果等待与重放核对；属性包括 flowId、stepSeq、callSeq、nodeKey、pendingCount、driverEpoch、子 runId、心跳状态与终态。持久执行历史由 Flow API 提供，不能只靠 Langfuse 重建。

观测导出不参与业务裁决，导出失败不影响 Flow。Langfuse/CozeLoop 继续复用现有出口。

## 13. 可靠性与安全不变量

1. **PG 是执行状态正本**：Node 与 QuickJS-WASM 内存、stdio 消息、SSE 和页面副本都不能独立证明成功。
2. **一 Flow 一有效驱动者**：driver lease + epoch 使旧副本和旧 runner 无法继续写。
3. **一调用身份一件执行工作**：`(flow_id, callSeq)` 唯一约束与条件更新挡重复 agent_input、重放和双副本竞争。
4. **成功结果不可被旧执行者覆盖**：resultRevision 与 run epoch 共同裁决。
5. **正常恢复不重复成功工作**：重放已接受求值 step并比较整批行为，成功调用只读保存结果，不依赖 inputHash。
6. **历史只增不改**：旧 prompt、调用和结果不原地修改，未来工作只跳过/取消或追加；执行事件 append-only。
7. **未知结果先处理再恢复模型**：有 intent 无 result 时先进入 needs_review，不自动盲重放，也不先让子模型看到现场后继续行动。
8. **脚本无直接宿主能力**：所有实际工作经普通 Agent 与现有权限、沙箱和审计。
9. **确定性是恢复前提**：时间、随机和外部网络不能改变 callSeq；一次结果输入后排空 Promise 作业并整批提交，stepSeq 与 agent_output 固定 JS 实际经历的求值路径。
10. **有界但不按 JS 时间/步数判死**：脚本、消息、Agent 数、并发、日志、结果及进程 CPU/内存均有上限；心跳和停止处理失控。
11. **交付最多一次业务收尾**：通知去重、来源会话忙时等待、失败可恢复，但不承诺外部世界 exactly-once。
12. **结果由程序验收**：outputSchema 不代替执行成功，合法 null 不与失败混淆，未收拢调用不能 return 成功。

## 14. 验收与完成线

### 14.0 运行时前置能力验证

施工包获批后的第一道门槛，是在固定 Node、QuickJS-WASM 与 runner 版本上验证 async/Promise 兼容性、待处理作业接口与宿主 Promise 桥接、未处理 rejection 跟踪、逐步排空 Promise 作业的可判定性、stdio 整批提交/确认协议、step 重放、心跳、进程组停止、CPU/内存约束及两个 OS 的沙箱。结果形成可重跑的入库测试；任一关键假设不成立就回到方案修订，不静默删掉故障场景、改用未经拍定的执行器或无沙箱降级。

### 14.1 确定性单测与协议测试

- 普通 Agent 规格解析与行为完全不变；`start_flow` 缺 allowAgents、引用非普通 Agent 或重复 key 均拒绝。
- JS 可用 API、Promise 顺序、parallel/pipeline 顺序与并发、显式成功/失败对象、合法 null、返回值序列化。
- outputSchema 严格 JSON、未知关键词、重复键、错类型与超深度均失败；未等待调用、未处理 rejection 和运行中后台命令使 return 回 unjoined_calls。
- 最小 QuickJS-WASM 反例必须先判红旧假设：同样 B→A，若同检查点产生 repairA→repairB、分检查点产生 repairB→repairA，则证明不能仅记投递顺序；新协议首次执行即一个 step一个结果，并由 QuickJS-WASM 主动排空 Promise 作业，恢复逐步比较 agent_output 与整批 agent_input，重放输出必须逐字段一致。
- 禁止 import/fs/process/network/timer/random/eval；沙箱不可用时拒绝，不能无围栏执行。
- NDJSON 分片、超大行、非法消息、重复 stepSeq、旧 driver epoch、Node 提前退出和 stderr 诊断。
- PG 唯一约束、租约接管、旧代写拒绝、step evaluating/accepted、重放整批比较、同 callSeq 不同 agent_input 拒绝、脚本与 args 不可变。
- 执行历史 append-only、step/eventSeq 分页、phase/log/step accepted/replayed/resume/stop 留痕；心跳停更派生 unresponsive，但不自动换 driver。
- 项目删除、会话删除、归档清扫、租户/用户可见性和容量。

### 14.2 真实活体场景

必须起真实引擎、真实 Node 通信 runner、真实 QuickJS-WASM 和真实模型，不允许全程 mock：

1. `/flow` 创建动态脚本：上游返回列表后才确定 Agent 数；循环至少两轮，证明不是预建 DAG。
2. 30 个调用完成一部分后 `kill -9` driver/引擎；显式继续后历史 step全部重放匹配、成功子代理零重跑，其余只执行一次，最终交付一次。
3. 在「step 输入已固定未求值 / 求值未返回 / 整批事务未提交 / 提交后未 ack / 调用绑定未启动 / 子代理运行中 / 结果已接受未入下一个 step / returned step 未接受」各窗口崩溃，均按 PG 状态收敛。
4. 两副本共库同时驱动同一 Flow，同一 callSeq 只有一场有效执行，旧 driver 不能写，新 driver 可接回。
5. 子代理本身崩溃，Flow 不重建成功兄弟节点；中断子代理按 run 恢复边界继续。
6. 暂停时不再创建新调用，在途结果保留；停止后无后台复活；显式恢复才继续。
7. 常规重规划在同一 Flow 内追加新 callSeq，旧 prompt 和结果不变；独立完整重跑使用新 Flow，不复用旧缓存。
8. 恶意脚本尝试读文件、开网络、起进程、超 Agent 配额与跨租户 Agent时被拒绝；纯 JS 死循环时历史停在最后活动、心跳失联，用户停止能杀进程组且资源约束生效，不依赖时间/步数自动判死。
9. 浏览器完成 `/agent /dag /flow` 斜杠选择、Flow 卡、持久执行历史、阶段轨迹、只读脚本、心跳、暂停/停止/恢复和通知交付；移动端无横向溢出。
10. 子代理有工具 intent 无 result 时，恢复模型前程序即进入 outcome_unknown，外部计数器不自动增加；补交可验证证据后沿原 run 继续，放弃则不再恢复子模型，把 outcome_abandoned 作为明确失败回给 JS。

### 14.3 收尾命令

```bash
cd yk-tako-eino
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go test ./internal/runengine ./internal/observability
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go vet ./internal/runengine
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go build ./cmd/yktako
cd web
npm run typecheck
npm run build
```

改契约字节后运行 `assert_contract.py`、`check-expected.sh`；改存储/可见性后运行 `spotcheck.sh`。Node 通信 runner 与 QuickJS-WASM 的确定性测试也必须是一条可重复命令并入库。

## 15. 施工组织与交付证据

用户确认本方案后，主 Agent 先写施工包，明确最终 API 字节、表 DDL、状态机、Node 协议、具体预算、错误码、沙箱参数与测试夹具；施工包点头后才允许 coding。

施工内部顺序：

```text
真实 API/run mode 契约
  → Flow 表、不可变脚本与最小 start/detail
  → 固定 Node 通信 runner + QuickJS-WASM + 安全宿主协议
  → 持久普通 Agent 执行服务
  → 求值 step整批提交、逐步重放与完整崩溃窗口
  → 双副本、通知和交付恢复
  → 未知结果处理、严格 outputSchema 与 return 收拢校验
  → 持久执行历史、心跳、斜杠与页面
  → 全量回归和活体验收
```

内部可连续推进，但交付必须回到同一条 `/flow` 业务链，不能在存储或 runner 半成品处停下等验收。

开发交付必须提供：真实 curl、PG 行与结果版本、Node 协议实录、浏览器走查、双副本与 kill 窗口证据、所有测试命令结果；主 Agent 另审 diff 并重跑关键场景。验收后再更新 doc/README、doc/16、doc/40、doc/46、doc/53、doc/82 与相关 v6 契约，删除临时施工材料并按规定留档。

## 16. 所以这解决了什么

它补的不是动态计划、多 Agent，也不是「Go 会不会启动 JS」；现有 ReAct + Todo + 持久 Task + 可修订 DAG 已能完成同类目标。它新增的是一种协调执行方式：**当规则明确、重复且规模较大时，让模型把结果驱动的循环、分支和动态扇出写成一次目标对应的持久程序，由程序处理重复协调，把中间结果留在脚本与执行记录中，同时不让程序绕过权限，也不让进程崩溃和双副本把已经完成的工作重做一遍。**

路线需要持续语义判断时继续由主 Agent 自主工作，真实依赖继续由 JSON DAG 表达，稳定、重复、可程序化的协调才进入 Flow；三种方式各有明确名字和完成边界。第一版只交付临时 `/flow`，跨任务编排复用下一期按真实需求另立方案；如果拿不出这种场景，本方案不应开工。