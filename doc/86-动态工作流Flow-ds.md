# 86 · Dynamic Workflow（Flow）与 Flow Agent · 纯 Go 内嵌 JS 引擎路线

> 性质：**方案候选（已参与最终综合，不作施工真源）**。
> 状态：⬜ 未施工；最终唯一综合方案以 [doc/85](85-动态工作流Flow.md) 为准。本文保留纯 Go 内嵌 JS、revision 与自动资源终止等候选设计，只作评审记录，不得直接照此施工。
> 日期：2026-11-20。
> 代码落点：无；预计施工面见原 §9，均非已落地事实。
> 最终综合：仅吸收“固定运行时前置能力验证”和保留小宿主面的思想；未采用未验证的纯 Go async 引擎路线。见 [最终综合记录](log/2026-11-20-Flow-Arena最终综合.md)。
> 参照：Claude Code [Dynamic workflows](https://code.claude.com/docs/zh-CN/workflows)；最终 base [85](85-动态工作流Flow.md)；持久执行底座 [70](70-多Agent编排与Task/README.md)、[72](72-多Agent编排下一阶段.md)、[73](73-协调通知与交付恢复.md)、[74](74-并行任务工作区隔离.md)、[75](75-目标重规划与继任编排组.md)、[82](82-可组合执行原语与后台任务.md)。
> 原合成说明保留在 §16；其中“选定 base”指该次候选生成的历史结论，不是最终施工真源选择。

---

## 0. 一句话与本质

用户或主 Agent 给出目标后，模型写一段**受限 JavaScript**；脚本用循环、分支、`await` 与动态扇出决定「接下来派谁做什么」，每次 `await agent(...)` 由 Go 接上一件普通子代理并取回结果，PostgreSQL 保存脚本版本、调用事实与结果；进程崩溃后，新的一次驱动**从脚本第一行重放**，已经成功且输入未变的调用**直接返回保存结果**，不重做已完成的工作。

本路线的技术主张只有一条：**JS 执行放进 tako 自己的纯 Go 运行时，不新增 Node.js**。落点是「同一份 Go 二进制以一个极小、无网络的子命令进程（`yktako flow-runner`）运行脚本，进程内嵌入一个纯 Go 的 ECMAScript 引擎」；脚本只能调用 Go 暴露的宿主函数，没有任何文件、网络、进程能力。

它**不是**：把 Flow 翻译成一张静态 DAG（nodes/edges 不存在）；让脚本直接执行 bash／文件／MCP／网络（脚本里根本没有这些能力，它们只在 Go 侧、在既有权限与沙箱之内）；一个「能打印 hello 的 JS 输入框」（进程边界、语言求值、持久化、恢复与防重、权限与页面同属一条完成线）。

---

## 1. 背景与病灶

### 1.1 一个会真实发生的失败故事

用户对一个项目说：

> 「持续审查并修复，直到检查通过或连续两轮没有进展。」

这句话里有三件没法在动手前写死的事：

1. **事先不知道检查会报几类错。** 第一次跑检查可能报 3 类，也可能报 17 类。派几个修复 Agent，取决于**结果**。
2. **第二轮的错误集合可能完全不同。** 第一轮修完重跑检查，报出来的可能是另一批。要不要第二轮、派给谁，取决于**中间结果**。
3. **停止条件是运行中才知道的。** 「连续两轮没有进展」只有在第二轮结果出来后才算得出来。

现有三种工作方式各自在链条的哪一步断掉（下表是对 [82](82-可组合执行原语与后台任务.md)、[70](70-多Agent编排与Task/README.md) 与代码现状的归纳，不是新设计）：

| 方式 | 谁决定下一步 | 适合什么 | 在这里缺什么 |
|---|---|---|---|
| 主 Agent + Todo | 主模型逐轮决定 | 步骤不多、要频繁用主上下文的工作 | 每轮检查/修复的中间输出持续堆进主上下文；跑 30 个子代理时主上下文被中间结果淹掉 |
| 普通子代理 | 一次独立执行 | 一个独立上下文能装下的一件工作 | 父 Agent 等最终字符串才继续，外层没有程序能做「先看结果再决定派几个」的大规模扇出 |
| JSON DAG + 持久 Task | PG 里预先声明的节点和边 | 已知「A、B 都成功后才做 C」 | 创建时穷举不出未知数量的工作，也表达不了「结果驱动的循环」 |

把这句目标硬塞进初始 DAG 是什么样：模型只能**先编造未来节点**——假设有 5 类错误就建 5 个修复节点；检查跑完发现是 3 类，于是取消 2 个；第二轮冒出 4 类新的，再补建……**创建时写下的计划，在真实结果面前每一轮都要取消和补建。** 这不是模型的错，是这张图承载不了它。

**缺的那一层，是让「运行中根据中间结果决定后续计划」成为一段可复用、可恢复、防重、受权限约束的程序。**

### 1.2 三个必须一并解决的失败故事

上面那个目标一旦落到一段程序上，立刻带出三个问题，缺一个方案就不完整：

- **崩溃重做。** 脚本派到第 30 个子代理时，引擎进程被 `kill -9`（部署、OOM、机器重启都行）。如果「哪些调用已经做过、结果是什么」只活在进程内存与 JS 堆里，那重启后唯一能做的就是从第一行**重跑**——重读 30 次、重跑 30 个子代理、把带外部副作用的工具再执行一遍。烧钱，而且可能重复写文件、重复发请求。
- **双份驱动。** 部署了两台副本（AGENTS.md 的目标形态就是多副本）。如果两台同时跑同一段脚本，会派出**双份子代理**、产生**双份交付**。谁在处理眼前这一轮？不能靠「一般只有一台在跑」。
- **越权。** 脚本如果是模型生成的、能拿到租户身份、项目路径、任意模型或宿主文件系统，那它就是一个绕过权限的后门。读者要的是「审查并修复」，不是「模型写一段能读任何文件的代码然后我们假装没看见」。

### 1.3 用户的真实工作习惯（价值锚点）

[2026-11-14 讨论记录](log/2026-11-14-异步委派与DAG讨论.md) 里的原话很关键：用户平时**不用 goal 模式**，而是**让模型写带验收的文档**，自己也不常看验收文档。

所以这条路线的价值锚点是「**把带验收的目标变成一段可执行的程序**」，不是「给用户一个天天盯的看板」。这决定了两件事：页面只做可核对的调用轨迹，不做重型可视化；脚本本身才是被持久化、被复用、被重放的产物（见 §2.1）。

---

## 2. 设计方向与关键取舍

本节逐条给出选型与**「没它会怎样」**。

### 2.1 为什么是「一门语言」，而不是声明式原语

**先把要的产物说清楚。** 需要的是一段**可重复执行、且决策由中间结果驱动**的程序。语言恰好是唯一同时满足「表达动态控制流」与「可被原样重跑」的产物。

操作定义先说清——「可重复执行」在这里的**操作**是：把同一段脚本文本 + 同一组参数交给运行时，它会**按同一顺序**发生同一串外部调用（§2.4 给了这条为什么成立的机制）。因此崩溃后只要从第一行再跑一次、把已保存的调用结果直接返回，就既不重做、又得到和崩溃前一致的轨迹。

逐项对比另两种做法：

- **声明式原语（加几个工具，如 `spawn_many(n)` / `await`）。** 这类原语能把「扇出」做成一次调用，但**判断「派几个、派给谁、要不要第二轮」必须由某个东西在每轮结果出来后决定**。若那个东西是模型（主 Agent 逐轮看结果再调工具），就退回「主 Agent + Todo」——中间结果持续占主上下文，而且它**不是一段可复用程序，是模型临场发挥**；若那个东西是调度器读一张图，那又回到「图必须事先存在」，图上没有的节点表达不了。
- **JSON DAG。** 它能表达「A、B 成功后做 C」，也能持久化与恢复；但表达的是**创建时已知**的节点与边。未知数量的修复节点、按结果分叉的第二轮，都不在图里。硬塞就是 §1.1 的取消与补建。
- **一门语言。** 循环、分支、`await`、动态扇出都是一等表达；脚本文本本身**就是那件可持久化、可原样重跑的产物**——崩溃恢复不需要「把运行时做的决定序列化回一张图」，因为决定本来就写在文本里，重跑文本自然重建同一串调用。

**没它会怎样**：没有这一层，回到二选一——要么模型在循环里临场决定（上下文爆掉、不是可复用程序），要么预先编造 DAG（每轮取消补建）。两者都是 §1.1 的病灶。这正面回答了 doc/82 §12 的问题「现有调用与修订为什么表达不了」。

选 **JavaScript** 而不是自造领域语言，理由是可被社区模型写对：模型对 JS 的循环／`async/await`／`Promise`／数组方法的先验，远强于对一个自定义图语言的先验；自造语言还要自己写解析器、错误定位与文档，收益为负。

### 2.2 运行时边界：为什么不引 Node，也不放进主引擎进程内

**本方案的选择：脚本跑在一个独立进程里，但那个进程是 tako 自己的 Go 二进制（子命令 `flow-runner`），进程内嵌入纯 Go 的 ECMAScript 引擎。**

三个位置逐个对比：

| 维度 | 主引擎进程内 | 独立 Node 子进程（doc/85） | 独立 Go 子进程内嵌引擎（本方案） |
|---|---|---|---|
| 崩溃影响面 | 坏脚本（无界循环、疯狂分配）炸的是**服务所有租户**的引擎进程 | 炸隔离进程，不波及引擎 | 炸隔离进程，不波及引擎 |
| 内存上限 | **无法硬限**：社区资料表明引擎级内存 hook 约束不了全部分配 | 靠 Node 侧终止 + 进程边界 | 靠操作系统：`RLIMIT_AS`／cgroup + 进程组（内核级，脚本越不过） |
| 沙箱围栏 | seatbelt／bwrap 是**进程级**设施，进程内没有可围栏的边界 | 需把 Node 与 runner 只读挂入，Linux 侧另禁网络 | **直接复用现有 bash 沙箱**（`sandbox.go` 的 `detectSandboxBackend`／`sandboxProfile`／`sandboxWrap`／`bwrapArgs`／`killProcessGroup`，已读），runner 本就无网络 API |
| 新增运行时依赖 | 无（但把风险引进了主进程） | **新增 Node.js**：发布镜像、版本、CVE 面、CI 都要多一条线 | **无新运行时**：一个纯 Go 模块依赖，`go mod vendor` 即锁死 |

**没选进程内会怎样**：见上表第一行——一次坏脚本会波及同进程内所有租户；且进程内没有内核级资源上限，也没有 seatbelt／bwrap 可用。**没选 Node 会怎样**：多一份运行期外部依赖要发布、要版本化、要打补丁，而它换来的只是「语法更全」这一项（见 §2.3 的代价）。

### 2.3 目标引擎与 async 驱动机制（本方案最需要先验证的一处）

**选型。** 纯 Go 的 ECMAScript 引擎（[goja](https://github.com/dop251/goja) 一类），随引擎二进制一起编译，`go.mod` 只多一个纯 Go 依赖，不引入 cgo、不引入外部运行时。

**代价（正面说）。** 纯 Go 引擎的语言覆盖面小于 V8／Node：社区资料显示这类引擎完整实现 ECMAScript 5.1、**ES6 大部分仍在推进**。**没接受这个代价会怎样**：若为「随便写现代 JS」改用 Node，就重新引入了 §2.2 里要避开的整套运行时依赖。第一版把可写语法**收窄到「选定引擎版本能跑通的能力测试集」**，写进系统提示与工具说明；引擎拒收一段语法时**响亮报编译错误让模型改写**，不静默降级。

**支持子集必须在 PoC 钉死，不能靠印象。** 施工前先跑一个能力测试集，逐项记录：箭头函数、`let/const`、模板字符串、解构、展开、`for...of`、`try/catch`、`JSON`、数组/字符串/对象方法、**Promise**、**async 函数 + `await`**、生成器。测试集结果就是「第一版允许的语法白名单」，进 `contract_test.go` 与 `expected.json`。**这是本方案最需要先验证的一处**（§12.0 是施工前置门槛）。

**顶层 `await` 的机制级处理。** 「脚本体 `await` 后 `return`」不依赖引擎是否原生支持**顶层** await（那是 ES2022 的模块特性，多数嵌入式引擎没有）。做法是：

1. runner 把脚本体**整体包进一个 async 函数**再运行：`(async () => { <脚本> })()`。脚本永远不作为顶层程序跑，因此不需要顶层 await。
2. 这个 async 函数被调用后立即返回一个 Promise P，并在第一个尚未落定的 `await` 处挂起，把控制权交回 runner。
3. 宿主函数 `agent()` **不阻塞**：它用引擎的 Promise 构造接口造一个挂起的 Promise，登记 `requestId → {resolve, reject}`，把这次调用落库，然后把这个挂起 Promise 交回 JS。`await` 于是在这里挂起。
4. 子代理完成是**异步**的；Go 在**持有该 VM 的那条 goroutine（owner 协程）**上调用该 Promise 的 `resolve(value)`／`reject(err)`，把续体排进引擎的微任务（job）队列。
5. runner 在 owner 协程上**泵动引擎的微任务队列**，async 函数在 `await` 处恢复执行。
6. 脚本执行到 `return v`，包壳 Promise P 落定，runner 发出 `done {result}`。

**没这套机制会怎样**：若改用「宿主 `agent()` 同步阻塞、等子代理跑完再返回」，`parallel`／`pipeline` 就无法真正并发——每个 thunk 里的 `agent()` 会串行阻塞；脚本也无法在等待期间交回控制权，等于把「动态扇出」退化回同步调用。所以必须用「Promise 落定 + owner 泵队列」这一形态。

**引擎能力前提（PoC 要证，不是已证）。** 这套机制要求引擎**同时**具备：async 函数挂起/恢复、Promise、以及**可被宿主泵动的微任务队列**。社区资料显示 goja 已实现 async 函数对象与 Promise，但「宿主驱动挂起/恢复的确切接口」必须 PoC 实测。**若选定版本只实现了 Promise 而没有完整的 async 挂起/恢复**，降级路径是把首版脚本约束成「只对宿主 Promise 用 `.then` 或 `await`」，并在 runner 装载时做一次语法层包裹——**不引入转译器**（§3.2）。这条降级也进 PoC 决策。

### 2.4 确定性：为什么重放能得到相同调用顺序

**先演一场戏。** 脚本第 12 行 `await agent(...)` 拿到「有 5 类错误」，据此进一个 5 次循环，第 18 行又各派一个修复 Agent。跑到第 3 个修复 Agent 时引擎被 `kill -9`。恢复时从第一行重放：如果重放到第 12 行拿到的不是原来那个「5 类错误」，而是子代理这次给出的「4 类错误」，循环次数就变了，后面所有调用的**位置**都错位——缓存会张冠李戴（把第 4 次调用的缓存喂给一个身份已经不同的调用）。

**机制。** 调用身份定义为三元组 `(script_revision, call_seq, input_hash)`：

- `script_revision`：脚本版本号，每次编辑 +1（§6.2）。
- `call_seq`：**沿脚本求值顺序**给每次 `agent()` 分配的自增序号，从 1 起。
- `input_hash`：对该次调用规范化输入的哈希，至少覆盖 agent 名、prompt、schema/options 与**完整规范化 Flow args**（§6.3）。

要让同一 revision 重放得到**相同的 `call_seq` 分配**，靠三条：

1. **脚本内没有任何环境不确定性**：不暴露 `Date`（含 `Date.now`、无参 `new Date`）、`Math.random`、随机 UUID、`setTimeout`／`setInterval` 与任何定时器、`fetch`／`WebSocket`／`net`、`fs`／`process`／`require`／`import`、`eval`／`Function`。需要时间或随机性时，只能由创建参数显式传入（§5.4）。
2. **完成先后不改变序号**：`parallel(thunks)` 按**数组下标**分配序号，`pipeline(items, mapper)` 按 **item 下标**分配序号。做法是这两个宿主辅助在 JS 里**先同步调完所有 thunk**（每个 thunk 里的 `agent()` 同步登记并返回挂起 Promise），再等全部落定——所以登记顺序 = 声明顺序，与谁先跑完无关。
3. **重放返回的是**保存的**值**：恢复时每个 `agent()` 先按 `(revision, call_seq, input_hash)` 查库；命中成功的调用**直接返回保存结果**，不重跑子代理。于是脚本看到的前缀值与原次完全相同 → 控制流走同一条路 → 后续 `call_seq` 分配也相同。

**没它会怎样**：若允许完成顺序决定序号，`parallel` 的结果会随网络/模型延迟漂移，缓存错配，恢复时把 A 的结果喂给 B；若重放用「重跑子代理取新值」而不是「返回保存值」，模型不确定性会让第 12 行的错误数变化，整条后缀张冠李戴。**所以「无环境不确定性 + 序号按声明顺序 + 重放返回保存值」三件缺一不可**，它们共同保证的正是「重放得到相同调用顺序」。

### 2.5 多副本下「同一件事只有一个驱动者」，以及状态为什么必须在 PG

**演一场戏。** 两个引擎副本同时从「有 Flow 待跑」的查询里读到同一个 Flow，各自启动 runner 驱动它。两个 runner 都把第 1 行 `agent()` 落到「创建子代理」这一步——产生两件子代理、双份交付；两边都以为自己是唯一驱动者，页面上出现两条互相矛盾却都「成功」的轨迹。

**机制（全部用数据库约束，不用进程内锁）。** 完全沿用现有持久 Task 已经验证的形态（出处见 §9）：

1. **驱动权 = 租约 + 代次。** `flow_runs` 有 `driver_owner / driver_epoch / driver_lease_until`（对应现行 `runs` 的 `lease_epoch` / `lease_until`，以及 `ClaimResume` 的 `epoch+1` 续租约 CAS：`store.go:955`）。领取驱动是一笔事务里的**条件更新**；只有拿到**当前 epoch** 的副本能写事实，续租用 `WHERE driver_epoch = <自己的代次>`，代次不符即空转。
2. **一次调用只有一个执行者。** `flow_agent_calls` 对 `(flow_id, script_revision, call_seq)` 建**唯一索引**。两个副本同时想为同一 `call_seq` 创建调用时，第二个 `INSERT` 撞唯一索引失败——**插入失败本身就是防重**（与 `one_live_run_per_session`（`pgstore.go:48`）、`agent_orchestrations_idem`（`agent_task_types.go:194`）、以及 `agent_tasks_exec_session_once` / `agent_tasks_current_run_once`（`pgstore.go:993`／`998`）同一手法）。撞了之后回读已存在的行：已成功且 `input_hash` 相同 → 返回保存结果；仍在执行 → 接回同一执行 run；都没有 → 创建。
3. **旧驱动者与旧 runner 作废。** 一切写操作带 `driver_epoch` 条件；代次不符的迟到写被拒绝。旧 runner 进程即使还活着、还会往协议流里写消息，Go 也只认**当前代次**发出的请求（§5.3）。

**为什么状态必须在 PG、不能在引擎内存。** 三点，逐条对应 §1.2：

- **多副本**：同一个 Flow 可能被任意副本接管；内存只在某一个进程里，换副本就看不见。
- **崩溃恢复**：进程内存随进程死。恢复依据必须是「脚本 revision + args + 已发生的调用事实」这些持久行，重放才能算出相同 `call_seq`（§2.4）。
- **单驱动裁决**：租约续期、代次比较、唯一索引冲突，全是数据库能给出的原子判定；进程内锁在单进程内有效，多副本上线即失效（AGENTS.md 明令）。

### 2.6 宿主面收窄：只留一个真·宿主动词

**Go 对 JS 只暴露一个真·宿主原语：`agent(prompt, options) -> Promise`。** `parallel`／`pipeline`／`phase`／`log` 都是**随 runner 发布的纯 JS 薄封装**，不是新的宿主能力——它们内部只用 `agent()` 与 Promise 组合。Go 不需要理解 JS 的数组、闭包或 Promise 内部结构。

**没它会怎样**：如果把 `parallel`／`pipeline` 也做成宿主动词，Go↔runner 协议要承载「一组 thunk 的并发上限、顺序保证、部分失败语义」，契约面成倍放大；而这三件事在 JS 侧用几行 Promise 组合就能表达（并发上限用分批，顺序用下标索引回填）。宿主面越小，要审计的垫片越少。

### 2.7 对 doc/82 的新决定

doc/82 §2.2 拍定「不引入 JS 执行器」，§12 结尾留了口子：JS 若有真实消费者，必须另立方案证明「现有调用与修订为什么表达不了、JS 调用哪些原语、状态如何恢复、沙箱权限是什么」。

本文就是那份方案，四项一一对应：**为什么表达不了** = §2.1；**调用哪些原语** = §2.6（一个宿主动词 + JS 侧薄封装）；**状态如何恢复** = §2.4、§7；**沙箱权限** = §2.2、§8。其余边界不动：后台 bash、`wait_task`、JSON DAG 创建与修订、通知恢复全部继续有效。落地方案时同批在 doc/82 头注补记这条新决定。

---

## 3. 范围与非目标

### 3.1 第一版完整范围

- 对话输入框提供 `/agent`（默认）、`/dag`、`/flow` 三个显式入口；`executionMode` 随 run 持久化，resume 读同一字段。
- `/flow` 规划 run 中提供 `start_flow` 工具：模型提交目标、脚本、args 与**允许调用的普通 Agent 名单**。
- 命名 Flow Agent：一种 Agent 规格（`runtime: flow` + `allow_agents`），正文即 JS，主 Agent 像调普通子代理一样调用它。
- runner = tako 的 Go 二进制以子命令形态启动，嵌入纯 Go JS 引擎，套现有沙箱，内存受 OS 限制；脚本只作数据传入。
- JS 宿主面：`agent / parallel / pipeline / phase / log / args`（其中只有 `agent` 是宿主动词，见 §2.6）。
- PG 保存 Flow、脚本 revision、Agent 调用、结果版本、驱动代次与来源归属。
- 同 revision 崩溃恢复、暂停/继续、停止、失败可见；显式编辑后新 revision 重放。
- 双副本下同一 Flow 只有一个有效驱动者，同一 `call_seq` 只有一场有效执行，最终通知与交付不重复。
- Flow 卡与右侧面板展示脚本、阶段、调用轨迹、结果、缓存命中、暂停/停止/恢复与 revision。
- OTel 关联 Flow、脚本求值、宿主调用与缓存命中。
- 单测、契约、真实模型、真实 runner、双副本与 `kill -9` 活体验收。

### 3.2 明确不做

- 不支持任意 JS 程序、npm 包、`import/require`、直接文件系统、shell、网络或数据库访问。
- 不支持 TypeScript / JSX / Babel / 任何转译或擦除步骤——脚本必须落在 §2.3 的语法子集内。
- 不支持浏览器 DOM、A2UI，不以 Mermaid 为执行真源。
- 不允许运行中的脚本修改自身；修改须暂停/停止后创建新 revision。
- 不允许 `Flow → Flow` 嵌套，也不允许普通子代理再创建 Flow，或任意深度递归委派。
- 不做自动复杂度路由，不在 `/agent` 中未经用户选择临时生成 Flow。
- 不做脚本内中途提问；需要人决定时结束当前 Flow，由主 Agent 追问后以新输入重启或修订。
- 不做任意历史节点单点重跑、自动数据依赖推断或完整 JS 数据流分析。
- 不承诺外部副作用 exactly-once；已执行但结果未知的工具仍按现有中断语义处理（§7.4）。
- 不复制一套简化 Agent 执行器，不改变现有 JSON DAG、同步子代理和后台 bash 的产品语义。

---

## 4. 端到端业务链路

每环都点名；标出本方案所讲机制在链路的哪一环。

### 4.1 触发链（`/flow` 一次性动态工作流）

```text
用户输入 /flow + 目标
 → POST /api/runs 建一场 executionMode=flow 的普通规划 run            【沿用现有 run 创建】
 → 主模型生成受限 JS、结构化 args 与 allowAgents
 → 模型调 start_flow 工具：服务端校验脚本语法子集、来源归属、配额、Agent 名单
 → 同一笔 PG 事务写 flow_runs + flow_script_revisions(revision=1) + 来源关系，返回 flowId
 → 规划 run 结束，页面出现 Flow 卡；来源会话可继续别的对话
 →【本方案机制起点】某个副本以条件更新取得驱动租约（driver_epoch=1）     §2.5
 → Go 启动沙箱里的 flow-runner 子进程，把 start{script,args,...} 从 stdio 注入   §2.2
 → runner 装入脚本、包成 async IIFE，开始求值                          §2.3
 → 脚本执行到 agent()：runner 发 agent_request{requestId, callSeq, ...}  §5.3
 → Go 查 (flow, revision, callSeq)：命中缓存直接回 host_result；
   未命中则创建/接上一件独立子代理 session/run，把结果落 PG             §7.1
 → Go 把 host_result 回 runner，owner 协程 resolve 对应 Promise → await 恢复  §2.3
 → 脚本据结果继续循环/分支/扇出，重复上面两步
 → 脚本 return：runner 发 done{result}；Go 在 PG 写最终结果与终态        §7.3
 → 写出 Flow 完成通知；来源会话空闲时创建一次交付 run（复用 doc/73 链）    §5.5
 → 页面重拉 PG 权威状态
```

**机制在链路哪一环**：驱动租约在「取得驱动权」一环；`call_seq`／缓存判定在「每次 `agent()`」一环；终态落库在「`done` 之后由 Go 定稿」一环——**runner 自己说完成不算完成**（§7.3）。

### 4.2 触发链（命名 Flow Agent）

```text
用户普通对话
 → 主 Agent 按 description 调用某命名 Flow Agent（走现有委派工具形状）
 → Go 读到规格 runtime=flow，以 (来源 run, toolCallId) 为创建幂等坐标建立或接回一个 Flow
 → 同一个 FlowRunner 执行规格正文里的 JS
 → 脚本只能调用 allow_agents 内的普通 Agent
 → Flow 最终文本作为父 Agent 的委派工具结果返回，父 Agent 继续自己的循环
```

父 run 崩溃后的「Flow 已创建、工具结果还没写回」窗口按 `(source_run_id, source_tool_call_id)` 查 Flow：已成功则注入原结果，仍在跑则接回等待，失败/中断则把 flowId 与真实状态交给模型——**不得盲建第二个 Flow**。

### 4.3 恢复链（同 revision 崩溃重放）

```text
Flow 在执行中崩溃（runner 进程死 / 引擎副本死 / 租约过期）
 → 租约到期后，读侧把该 Flow 派生为 interrupted（不写回存储，与 runs 读侧派生同规则）
 → 用户或来源会话显式触发继续
 → 某副本 CAS 取得新驱动代次（driver_epoch+1），旧代次的一切写被拒绝     §2.5
 → Go 重新启动 runner，从 revision 第 1 行重放脚本                      §2.4
 → 每个 agent() 按 (revision, call_seq, input_hash) 查库：
     已成功且 input_hash 不变 → 返回保存结果（cache hit，不重跑子代理）  §2.4
     已存在且仍在执行、租约有效 → 接回原执行 run
     子代理也已中断 → 按现有 run 代次规则显式续跑，不盲目重放未知副作用   §7.4
     不存在 → 按脚本自然创建
 → 前缀缓存命中使控制流走回原路，call_seq 分配与原次一致（§2.4 的核心保证）
 → 最终 return 落库、通知与交付恰好一次
```

**这一环回答 §1.2 的失败故事 1**：30 件已完成的工作通过 cache hit 直接复用，不重跑。

---

## 5. 契约

> 以下全部是**拟议形状**，不是已实跑字节。

### 5.1 示例脚本

```js
const found = await agent(
  "列出需要审查的 API 文件，返回 files 数组",
  { agent: "researcher", key: "discover", schema: {
    type: "object", required: ["files"],
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

const valid = reviews.filter(Boolean)
if (valid.length === 0) {
  return { status: "no_result", message: "没有取得有效审查结果" }
}

return agent("汇总这些审查结果并去重：" + JSON.stringify(valid), {
  agent: "researcher",
  key: "summarize"
})
```

脚本体由可信 runner 包进受控 async 函数，支持 `await` 与 `return`。脚本不是 ES module，没有导入导出。

### 5.2 宿主 API

```text
agent(prompt, options) -> Promise<text | JSON value>      // 唯一的真·宿主动词
parallel(thunks) -> Promise<Array<result | null>>         // runner 发布的 JS 薄封装
pipeline(items, mapper, options) -> Promise<Array<result | null>>  // 同上
phase(name) -> void                                       // 同上
log(text) -> void                                         // 同上
args -> 创建时传入、深冻结的 JSON 值
```

`agent.options` 第一版只认 `agent`（必填，普通 Agent 名，必须在 allowAgents 快照内且 `runtime=agent`）、`key`（可选，人可读稳定标识，同一 revision 内不得重复）、`label`（可选，页面短标题，不改变执行身份）、`schema`（可选，子代理最终结构化输出约束）。**未声明字段一律拒绝，不静默忽略。** 模型不能从 JS 传任意 modelRef、工具白名单、项目路径、租户身份或宿主文件路径。

`parallel` 接收零参函数数组，按输入下标分配 `call_seq`，返回顺序与输入一致；`pipeline` 按 item 下标分配序号并限制同时等待的 Agent 数。完成先后顺序**不改变**调用序号。

失败语义：

- 直接 `await agent()` 的执行失败使脚本失败，除非作者显式 `try/catch`；
- `parallel`／`pipeline` 等待本组全部结束，单个子代理业务失败对应位置返回 `null`；
- PG 写失败、越权、协议错误、驱动租约丢失属于**宿主故障**，不能转成普通 `null` 继续推进；
- `mapper`／`thunk` 自己抛错使脚本失败；已启动的子代理仍按真实状态落库，不能假装从未发生。

### 5.3 Go ↔ runner 协议（拟议形状）

Go 永远启动仓库内**固定的** `flow-runner` 子命令，不拼接 `yktako flow-runner -e <模型代码>`。脚本、args 与配置通过首条 `start` 消息传入。传输用 stdio 上**一行一个 JSON**（NDJSON）：stdout 只准协议，诊断只能去 stderr；每行有字节上限；未知消息、非法 ID、重复终态或协议版本不符**立即失败**。

```text
Go → runner
  start       {protocolVersion, flowId, revision, script, args, limits}
  host_result {requestId, ok, result | error}
  cancel      {reason}

runner → Go
  agent_request {requestId, callSeq, agent, key, label, prompt, schema}
  phase         {name}
  log           {text}
  done          {result}
  script_error  {kind, message}
```

- `requestId` 由 runner 从 `revision + callSeq` 派生，**不使用随机数**（否则重放对不上）。
- `callSeq` 由 runner 沿求值顺序自增（§2.4）。
- Go 只认**当前驱动代次**发出的请求；旧 runner 迟到写消息不能创建调用或推进状态（§2.5）。
- `phase` 由 Go 条件更新 `current_phase`，并作为标签写进后续调用记录；`log` 进引擎日志与 trace，不承载业务结果。
- `agent()` 在 JS 侧造 Promise 并保存 `requestId → resolve/reject`；收到匹配 `host_result` 后落定（§2.3）。`parallel`／`pipeline` 在 runner 内基于 `agent()` 组合实现。

### 5.4 脚本环境不暴露这些（确定性边界）

```text
import / require / dynamic import
process / Buffer / fs / child_process / worker_threads / vm
net / http / https / fetch / WebSocket
直接 PG / Redis / MCP / bash / 文件工具
setTimeout / setInterval / 其他墙钟等待
Date.now / 无参 new Date / Math.random / 随机 UUID
eval / Function / 动态代码再执行
```

普通数学、字符串、数组、对象与 JSON 操作可用。需要时间与随机性时必须由创建参数显式传入，保证相同 revision 的重放得到相同调用顺序。

**引擎自身能力不等于安全边界。** 即使不暴露上述全局，runner 仍须：置于专用沙箱档（无网络、无可写根、脚本从 stdio 传入）；内存受 OS `RLIMIT_AS`/cgroup 限制；CPU 受引擎 `Interrupt` 截止；没有可用沙箱时 **Flow 与 bash 一样拒绝执行**（fail-closed，判定依据是 `sandbox.go` 的 `detectSandboxBackend` 探测结果）。

### 5.5 通知与交付

`/flow` 后台完成复用现有持久通知与交付恢复链（doc/73），新增 Flow 来源与 `flow_finished` 类型，不另造只在内存里的通知。命名 Flow Agent 由父工具调用等待，**不额外**给来源会话发一条重复完成通知。

---

## 6. 持久模型

Flow 不存强制 nodes/edges。JS 的变量、条件与 `await` 已表达控制关系，PG 保存的是**脚本版本与实际发生的调用事实**。页面默认是时间线/阶段轨迹，**不宣称**能从任意 JS 自动推出完整 DAG。

### 6.1 `flow_runs`（拟议形状）

```text
id / tenant_id / user_id / workspace_id
source_kind = slash | agent_tool
source_session_id / source_run_id / source_tool_call_id
goal / status / current_revision / current_phase
args_json / allow_agents / agent_snapshot_json
driver_owner / driver_epoch / driver_lease_until
final_result / error_code / error_message
created_at / started_at / finished_at / updated_at
```

- `agent_tool` 来源对 `(source_run_id, source_tool_call_id)` 建**唯一索引**（创建幂等，与 `agent_orchestrations_idem` 同手法）；`slash` 来源用创建幂等键。
- `agent_snapshot_json` 保存 Flow 启动时解析后的允许 Agent 规格与策略快照，**不保存解密后的模型密钥**；正常恢复不因子代理配置在别处被编辑而悄悄换执行者。
- `driver_owner / driver_epoch / driver_lease_until` 三列就是 §2.5 的单驱动裁决依据。

### 6.2 `flow_script_revisions`（拟议形状）

```text
flow_id / revision
script / script_hash / args_hash
reason / created_by
created_at
```

`(flow_id, revision)` 唯一；当前行只指向完整写入的 revision；正在运行的 revision **不可原地覆盖**。

### 6.3 `flow_agent_calls`（拟议形状）

```text
id / flow_id / script_revision / call_seq / node_key / phase
agent_name / prompt / options_json / input_hash
status / execution_session_id / current_run_id
result / result_revision / reused_from_call_id
error_code / error_message
created_at / started_at / finished_at / updated_at
```

核心唯一性：

- `(flow_id, script_revision, call_seq)` **唯一**（§2.5 的防重复执行落点）；
- 非空 `node_key` 在同一 revision 内唯一；
- 执行 session / run 各自只能绑定一个调用（与 `agent_tasks` 的 `agent_tasks_exec_session_once` / `agent_tasks_current_run_once` 同一手法）。

`call_seq` 是恢复的**身份**；`input_hash` **至少**覆盖规范化后的 agent 名、prompt、schema/options 与完整规范化 Flow args——**不能**只哈希显示 label，也**不能**猜测任意 JS 中哪些 args「相关」。

### 6.4 迁移与清理落点

- 三张新表进 `pgstore.go` 的迁移列表（`AutoMigrate`）与其后的显式 DDL（唯一索引与 CHECK 词表，风格见 `agent_tasks_exec_session_once` 系列）。
- `Store.Reset`（`store.go:1027`，全表 TRUNCATE）同批加入这三张表。
- 项目/会话删除要走级联清理：仍有非终态 Flow 时按「忙」拒绝删除，与活 run 同一条规则。

---

## 7. 执行、重放与恢复

### 7.1 每次 `agent_request` 的处理

```text
1. 校验请求属于当前 Flow、revision、driver_epoch（旧代次 → 拒绝）
2. 计算 call_seq 与 input_hash，校验 allowAgents 与预算
3. 查 (flow, revision, call_seq)
4. 已成功且 input_hash 相同 → 记录 cache hit，原结果回 runner
5. 已存在且仍在执行、租约有效 → 接回原执行 run，不创建第二件
6. 不存在 → 同一 PG 事务创建调用与独立执行 session/run，再启动子代理
7. 已失败 → 按 §5.2 的失败语义回 runner
8. 子代理终态与调用结果在同一持久链收口，再把 host_result 回 runner
9. runner 丢失或连接中断时，PG 中已发生的工作不因它回滚或重复创建
```

**调用与执行 run 的创建不持有模型调用期间的 PG 事务**：模型与工具运行很久，事务只负责执行权、状态与结果落库。

### 7.2 崩溃窗口怎么收敛

| 窗口 | 崩溃点在 | 恢复行为 |
|---|---|---|
| 调用行写之前 | `agent_request` 刚发出、还没落库 | 重跑脚本自然重建；无半成品 |
| 调用行写之后、执行 run 未启动 | 事务提交后又崩 | 调用行存在、无执行 run → 恢复时按规则启动执行（`execution_run_id` 唯一索引挡重复） |
| 子代理运行中 | 执行 run 活着、租约有效 | 接回原 run 等结果 |
| 子代理完成、结果未回 runner | 结果已落库 | 重跑脚本到同 `call_seq` 直接返回保存结果（cache hit） |
| Flow 完成未定稿 | `done` 已写出、PG 未确认 | **不算完成**；恢复时同 revision 重跑，全 cache hit，最终一次定稿 |

### 7.3 什么算成功、什么算中断

- **成功**：Go 在 PG 写最终结果与终态，且驱动代次匹配。runner 说 `done` 而 PG 未确认**不算完成**。
- **中断**：驱动租约过期、runner 被杀、子代理执行 run 租约过期——读侧派生 `interrupted`，进入处理提醒；对账只补状态与通知，**不自动重放未知副作用**。
- **不确定的副作用**：只有 intent 没有 result 的工具仍走既有中断纪律（doc/30、doc/82 §8.2），不自动盲重放。

### 7.4 暂停、停止、新 revision

- **暂停**：持久化 pausing；停止接受新 `agent_request`，在途子代理可完成并保存结果，随后退出 runner，状态为 paused。继续时重放同 revision。
- **停止**：停止 runner 并请求停止当前子代理；已保存结果保留，Flow 为 stopped。重新处理必须显式继续或创建新 revision，**不能后台自行复活**。
- **新 revision**：只有 paused／failed／stopped／已完成的 Flow 能提交新 revision（running 必须先暂停）；请求带 `baseRevision` 防两个编辑者互相覆盖。新 revision 从头重放，按调用顺序比较：同一 `call_seq` 且 `input_hash` 不变的成功结果可复用；遇到**第一处**调用身份或 `input_hash` 变化后，该位置及之后不再自动复用；复用行通过 `reused_from_call_id` 指向旧结果，**不改写旧 revision 历史**。这是**保守的后缀失效**，不声称理解任意 JS 数据依赖——独立分支也可能被后缀失效带上重跑，要减少重跑只能把改动放在更晚的位置。

---

## 8. 权限与隔离

- **触发资格**：只有 member 能创建 Flow（写面走既有 `RequireTenantRole("member")`，viewer 403，形状同 doc/82 §6.3）。
- **可见性**：Flow 读取按项目与来源会话可见性；跨租户／跨用户／不可见统一 404。
- **脚本无身份**：脚本里没有租户、用户、项目、模型、工具白名单或宿主路径；这些一律由 Go 从来源 run 或 `flow_runs` 行**服务端取**，脚本传入一律忽略并拒绝。
- **子代理名单**：`allow_agents` 缺省**不表示全员开放**——命名 Flow Agent 必须显式列出，ad hoc `start_flow` 必须提交并校验名单；只能引用 `runtime=agent`，改名不能绕过嵌套限制。
- **执行隔离**：每个调用的子代理有独立 execution session/run 与上下文，使用被调用普通 Agent 的模型、工具、沙箱与记忆快照政策，可以有自己的 `run_tasks` Todo；父 JS 只拿最终文本或结构化结果，不接收全部中间上下文；完整 runlog 留在子代理执行记录里。
- **进程隔离**：runner 套专用 seatbelt／bwrap profile（只读安装、无网络、stdio 唯一宿主通道）；无可用沙箱时拒绝执行。**JS 沙箱不是唯一防线**——就算脚本逃出 JS 约束，进程里也没有 bash／文件／MCP 能力，profile 也不放行它们。
- **额度**：Flow 驱动与子代理**合计**不能绕过租户容量（沿用现有租户并发与额度规则）。

---

## 9. 现有代码接缝与施工面

以下只是施工面地图，**不表示已实现**。所引函数/结构均已打开代码核对（行号为阅读时所见，施工以当时代码为准）。

| 领域 | 现有接缝（核对过） | 预计新增/调整 |
|---|---|---|
| Agent 规格 | `agents.go:48` `AgentSpec`、`agents.go:136` `parseAgentSpec`（`KnownFields(true)`，未知字段拒载）、普通委派装配 | 增加 `runtime` 与 Flow 的 `allow_agents`/policy；`runtime` 零值仍为 `agent`（**必须零回归**）；Flow 正文按 JS 语法子集校验 |
| 子代理执行 | `agents.go`（`subagentShell.InvokableRun` 同步）、`agent_task_runner.go`、`agent_task_scheduler.go`（`ClaimReadyInGroup`，`:545`） | 抽出「按 AgentSpec 创建独立执行 session/run、装配模型与工具、提交结果」的**共享持久执行服务**，由现有 Task 与 Flow 共同消费；**不新建第三套模型循环、权限判定与结果格式** |
| 运行模式 | `server.go` 的 run 创建、`store.go:163` `AcquireRun`、`worker.go` 工具装配 | 持久 `executionMode`，按 agent/dag/flow 收窄工具与提示；`resume` 读同一字段 |
| 驱动租约 | `store.go:955` `ClaimResume`（`epoch+1`+续租约 CAS）、`store.go:1012` `AbandonClaim`；`pgstore.go:48` `one_live_run_per_session` | Flow 驱动租约与代次（同形状），落在 `flow_runs` 三列 |
| 持久 Task | `agent_task_types.go:156` `agentTaskMaxCount=32`、`:194` `agent_orchestrations_idem`；`pgstore.go:993`/`998` `agent_tasks_exec_session_once`/`agent_tasks_current_run_once` | Flow 三表与唯一约束、revision、项目/会话删除与归档清理 |
| Flow 运行时 | 无 | `flow_{types,store,service,runner,protocol,recovery,api,tools}.go`；`cmd/yktako` 增 `flow-runner` 子命令与协议测试 |
| 沙箱 | `sandbox.go`（`detectSandboxBackend`，`:47`；`sandboxProfile`，`:149`；`sandboxWrap`，`:248`；`bwrapArgs`，`:284`；`killProcessGroup`，`:442`） | 专用无网络 flow-runner profile；macOS/Linux 分流与拒绝路径；OS 内存上限设置 |
| 通知与事件 | `agent_task_delivery.go`（`planReviewToolNames`，`:421`；认领／交付）、`agent_task_scheduler.go`（`applyPlanCheckpointTx`，`:208`）、`events.go`（`PublishEvent`） | Flow 来源、完成通知、处理提醒与 `flows_changed`；不复制内存通知 |
| 存储 | `pgstore.go`（迁移与部分唯一索引）、`store.go:1027` `Reset` | Flow 三表、唯一索引、DDL 词表；同批加进 `Reset` 的 TRUNCATE 列表 |
| 观测 | `trace*.go`、`internal/observability/` | Flow 根、JS 求值、宿主调用、cache hit、恢复与 revision 属性 |
| 前端 | `web/src/{App.vue,ChatDock.vue,api.ts,chat.ts,mutation.ts}` | 斜杠命令、Flow 卡、`FlowView.vue`、阶段/调用轨迹、脚本与操作按钮 |
| 验收 | `contract_test.go`、`scripts/acceptance/{expected.json,assert_contract.py,check-expected.sh,spotcheck.sh}` | Flow 契约用例与 `flow_{single,dynamic,crash,replica,revision,determinism}_live.sh` |

**没有临时实现或过渡开关可以替代完成线。** 特别是「runner 能打印 hello」「Go 直接调内存里的子代理」「只有假调用数据画页面」**都不算** Flow 落地。§12.0 的引擎能力 PoC 是施工的**前置门槛**，不是可选步骤。

---

## 10. API、页面与观测

### 10.1 API 面（拟议形状）

施工包先用 curl 钉死真实字节，至少覆盖：

```text
POST /api/runs                          接收并持久化 executionMode
模型工具 start_flow                     创建后台 Flow，立即返回 flowId/status
GET  /api/workspaces/:id/flows          项目列表，可选 sessionId 过滤
GET  /api/flows/:id                     当前 revision、阶段、调用、结果与错误
GET  /api/flows/:id/revisions           revision 时间线
POST /api/flows/:id/pause
POST /api/flows/:id/resume
POST /api/flows/:id/stop
POST /api/flows/:id/revisions           baseRevision + 新脚本 + reason + 幂等键
```

所有新 JSON 接口使用统一信封 `{"code":…,"message":…,"data":…}`，键序固定 `code → message → data`；HTTP 状态照旧用 400/401/403/404/409/429/500，不为了信封统一改成 200。业务码**具体值在施工包核对 [doc/40](40-响应信封统一.md) §4 后钉死**，并与 `expected.json`、人读验收 [19v6-04](19-v6/19v6-04-验收-curl.md)、`contract_test.go` 同批改。本文不把未实跑示例字节冒充已验收契约。

### 10.2 页面

聊天输入框的斜杠菜单只展示三个执行方式及简短解释；`/agent` 是默认，不需要用户重复输入。

Flow 在对话中显示一张普通卡：目标、状态、当前 phase、Agent 成功/运行/失败数、缓存复用数与打开入口。点击在右侧工作区打开 Flow 标签，包含：

- **运行**：按 phase 与 `call_seq` 排列的调用轨迹（完成顺序可不同，序号稳定）；
- **脚本**：当前与历史 revision 的 JS、args、修改原因；
- **结果**：最终返回值与各 Agent 结果，标明「新执行」或「缓存复用」；
- **操作**：暂停、继续、停止、编辑 revision；权限不足时不显示写操作。

不根据模型自由文本拼 UI，不要求 Mermaid，也不在前端重建执行状态；所有状态来自 Flow API。

### 10.3 观测

OTel 根 span 以 Flow 为生命周期，子 span 至少包括脚本求值、每次宿主 Agent 调用、结果等待与恢复；属性包括 `flowId / revision / callSeq / nodeKey / cacheHit / driverEpoch / 子 runId / 终态`。观测导出**不参与业务裁决**，导出失败不影响 Flow。

---

## 11. 可靠性与安全不变量

1. **PG 是执行状态正本**：runner 内存、stdio 消息、SSE 与页面副本都不能独立证明成功。
2. **一脚本 revision 一有效驱动者**：驱动租约 + 代次使旧副本与旧 runner 无法继续写。
3. **一调用身份一件执行工作**：`(flow_id, script_revision, call_seq)` 唯一索引与条件更新挡重复 `agent_request`、重放与双副本竞争——插入失败本身即防重。
4. **成功结果不可被旧执行者覆盖**：`result_revision` 与 run 代次共同裁决。
5. **正常恢复不重复成功工作**：相同 revision 与 `input_hash` 的成功调用只返回保存结果。
6. **中断不等于无副作用**：只有 intent 没有 result 的工具仍走现有未知结果纪律，不自动盲重放。
7. **脚本无直接宿主能力**：所有实际工作经普通 Agent 与现有权限、沙箱与审计。
8. **确定性是恢复前提**：时间、随机、外部网络与完成顺序不能改变 `call_seq`。
9. **执行侧硬上限**：脚本字节、args 字节、消息大小、Agent 数、并发数、revision 数、日志数与最终返回值**均有上限**；**上限由 Go 在每次 `agent_request` 侧强制**（不是靠脚本自觉），超限响亮失败，不截断后假装完整。
10. **进程边界提供硬上限**：CPU 由引擎 `Interrupt` 截止、内存由 OS `RLIMIT_AS`/cgroup 限制；沙箱不可用时 fail-closed。
11. **交付最多一次业务收尾**：通知去重、来源会话忙时等待、失败可恢复，但不承诺外部世界 exactly-once。
12. **不新增语言运行时**：脚本执行不引入 Node/npm 或任何非 Go 运行时；`go.mod` 只多一个纯 Go 依赖。

---

## 12. 验收与完成线

### 12.0 前置门槛：引擎能力 PoC（施工前必做）

在写任何 Flow 代码前，先用一个小程序跑能力测试集并留证：async 函数挂起/恢复、Promise、宿主泵动微任务队列、`Interrupt` 对死循环的截止、调用栈上限、以及 §2.3 列的语法子集逐项。**结果决定第一版语法白名单与是否需要 §2.3 的降级路径。** 这一步不通过，方案范围要重新裁——不能把它当成「先写代码、跑不通再说」。

### 12.1 确定性单测与协议测试（`go test ./internal/runengine`，不打模型、不依赖 PG/Redis 的部分）

- 普通 Agent 规格不写 `runtime` 时解析与行为**完全不变**；Flow 规格缺 `allow_agents`、引用 Flow、未知 frontmatter 字段、重复 `key` 均拒绝。
- JS 可用 API、Promise 顺序、`parallel`/`pipeline` 顺序与并发、`null`/异常分流、返回值序列化。
- 禁止 import/fs/process/network/timer/random/eval；沙箱不可用时拒绝，不能无围栏执行；死循环被 `Interrupt` 截止；内存超限被 OS 杀死后对账收 interrupted。
- NDJSON 分片、超大行、非法消息、重复 `requestId`、旧代次、runner 提前退出与 stderr 诊断。
- PG 唯一约束、租约接管、旧代写拒绝、缓存命中、`input_hash` 变化、revision 冲突与后缀失效。
- 项目删除、会话删除、归档清扫、租户/用户可见性与容量。
- 新增用例进入 `contract_test.go`，字节从活引擎实跑抄入（不手写）。

### 12.2 真实活体场景（`scripts/acceptance/flow_*.sh`，参数化 `TAKO_BASE`／`TAKO_ENV`／`TAKO_PG_DSN`）

必须起真实引擎、真实 runner 与真实模型，不允许全程 mock：

1. `/flow` 创建动态脚本：上游返回列表后才确定 Agent 数；循环至少两轮，证明不是预建 DAG。
2. 命名 Flow Agent 被主 Agent 按 description 调用，内部再调普通 researcher/coder，最终结果回父 Agent。
3. 30 个调用完成一部分后 `kill -9` driver/引擎；显式继续后成功调用**全部 cache hit**、其余只执行一次、最终交付一次。
4. 在 §7.2 五个窗口各崩溃一次，均按 PG 状态收敛。
5. 两副本共库同时驱动同一 Flow：同一 `call_seq` 只有一场有效执行，旧 driver 不能写，新 driver 可接回。
6. 子代理本身崩溃，Flow 不重建成功兄弟节点；中断子代理按 run 恢复边界继续。
7. 暂停时不再创建新调用、在途结果保留；停止后无后台复活；显式恢复才继续。
8. 新 revision 的未变前缀复用、第一处变化后的后缀按规则重跑；旧 revision 历史仍可读。
9. 恶意脚本尝试读文件、开网络、起进程、无限循环、超 Agent 配额与跨租户 Agent，**全部被拒绝或有界终止**。
10. 浏览器完成 `/agent /dag /flow` 斜杠选择、Flow 卡、阶段轨迹、脚本、暂停/停止/恢复与通知交付；移动端无横向溢出。

### 12.3 收尾命令

```bash
cd yk-tako-eino
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go test ./internal/runengine ./internal/observability
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go vet ./internal/runengine
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go build ./cmd/yktako
cd web
npm run typecheck
npm run build
```

改契约字节后运行 `assert_contract.py`、`check-expected.sh`；改存储/可见性后运行 `spotcheck.sh`。runner 自身的确定性测试（含 §12.0 的能力测试集）必须是一条可重复命令并**入库**（放 `scripts/acceptance/` 或 `internal/runengine`，不留 `tmp/`）。

### 12.4 完成线（不许半成品）

「Flow 单条链完整可用」= API + 存储 + 执行 + 恢复 + 通知 + 页面 + 测试，不是「`start_flow` 能回 202」。§9 表格里的每个接缝都要在**同一条 `/flow` 与 Flow Agent 业务链**里被验证，不能在存储或 runner 半成品处停下等验收。

---

## 13. 这条路线做不到什么、边界在哪

一份方案只说「能做什么」是不诚实的。这一节逐条说明本路线**不保证**什么，以及代价落在谁身上。

1. **恢复的正确性押在「脚本是确定性的」上。** §2.4 保证了「同一 revision 的同一 `call_seq` 对应同一输入」，前提是脚本不引入拒绝清单之外的隐式顺序（例如自己写 `Promise.all` 依赖完成先后、而不是用按下标分配的 `pipeline`）。这套只能靠 runner 的确定性执行 + 拒绝清单 + 有界上限兜，**不能靠模型自觉**——模型写错脚本时，症状是「缓存错配」，不是「脚本报错」。第一版用「宿主辅助按下标分配序号 + 白名单语法」把它压到最小，但它不是零。
2. **语法子集是真实的功能成本。** 模型可能写出引擎拒收的语法；代价是「编译错误回注 → 模型改写 → 再跑」这一轮往返。写进系统提示只能降低概率，不能消除。
3. **不承诺外部副作用 exactly-once。** 只保证「已成功的调用不重跑」；只有 intent 没有 result 的工具仍可能已在外部世界生效。这与全仓既有纪律一致，不是本路线独有，但读者不能把它读成「Flow 保证不重复发请求」。
4. **多一个需要跟踪 CVE 的 Go 依赖。** 虽然不新增运行时，但嵌入的引擎是一个第三方模块；它自身的缺陷、版本升级与安全公告进入供应链。这是相对「什么都不引」路线的净成本。
5. **Flow 不是默认路径。** 短任务、一件独立工作、已知依赖的编排分别走主循环、单件 Task、JSON DAG；本路线只服务「长、动态、要可恢复」的那一类。**不做自动复杂度路由**——不因为一个请求「看起来复杂」就暗中生成 Flow，那是另一套失败空间。

反过来说，本路线**能**做而「模型在环」路线做不到的，正是：机械的数据变换（filter/map/aggregate 不花模型调用）、嵌套的动态结构、以及每轮零模型调用的紧循环。这些能力是本路线存在的理由，每一处以「模型要写对一段程序」为代价。

---

## 14. 与 doc/85 的对比

先逐项对比，再重述需求。

### 14.1 相同点（明确声明，不假装独立发明）

[doc/85](85-动态工作流Flow.md) 与本方案是**同一主题的两个执行运行时选择**，下列部分**实质相同**：

- 产品形态相同：`/flow` + 命名 Flow Agent，共用同一个 FlowRunner；两者只是来源与等待方式不同。
- **固定、可信 runner + 脚本只作数据传入**；不拼接 `-e <模型代码>`。
- 宿主面相同：`agent / parallel / pipeline / phase / log / args`；`parallel`/`pipeline` 在 runner 内基于 `agent()` 组合。
- PG 保存 revision + 调用事实（不存 JS 堆）；`(flow, revision, call_seq)` 唯一；`input_hash` 覆盖规范化 args。
- 确定性要求相同：不暴露时间/随机/网络/文件/定时器；完成顺序不改变序号。
- 恢复语义相同：同 revision 重放、按 `input_hash` 干净前缀复用、显式继续、不确定副作用不盲重放。
- 单驱动用租约 + 代次；通知交付复用 doc/73；子代理执行复用同一个持久执行服务。
- 都形成对 doc/82「不引入 JS 执行器」的新决定；都保留现有 JSON DAG；都「纯 JavaScript，不引 TypeScript 转译」。

（这些相同点大多来自**现有代码接缝的约束**——委派工具形状、`agent_tasks` 唯一索引、`ClaimResume` 的 CAS、通知去重——不是互相抄。）

### 14.2 关键差异

| 维度 | [doc/85](85-动态工作流Flow.md) | 本文 |
|---|---|---|
| 执行运行时 | Node 子进程 + stdio NDJSON | 纯 Go 引擎 + **本仓库 Go 二进制**的 `flow-runner` 子命令 |
| 顶层 `await` | 靠 Node 原生支持，脚本自由用现代 JS | 不依赖顶层 await；脚本体包进 async IIFE，靠「Promise 落定 + owner 泵微任务队列」（§2.3） |
| 语法覆盖面 | 全量现代 JS | ES5.1 + 选定子集（PoC 钉死），**这是主要功能成本** |
| 新运行时依赖 | 新增 Node.js（部署件 + 版本 + 安全面） | **无**；一个纯 Go 模块依赖 |
| 内存上限 | 进程边界 + Node 侧终止 | 进程边界 + OS `RLIMIT_AS`/cgroup |
| 沙箱 | 为 Linux Flow runner 单独禁网络 namespace | 复用现有 bash 沙箱路径，runner 本无网络 API |
| 安全面 | Node 内置模块需要额外锁死 | 引擎默认无 fs/net/process，暴露面天生更小 |

### 14.3 各自的代价

- **doc/85 的代价**：多一个 Node 运行时（安装、版本、CVE、供应链），每副本多一份要维护的外部件；换来全量现代 JS 与更贴近 Claude 参照形态。
- **本文的代价**：语法子集收窄，模型可能写出引擎拒收的语法（靠系统提示 + 编译错误回注改写）；且 §2.3 的 async/await 驱动机制**必须 PoC 证实**。付出这些，换来不新增运行时、供应链更短、确定性更强、沙箱复用现有代码。

### 14.4 需求原文重述（对比服务需求，不是替代需求）

需求是：**让「运行中根据中间结果决定后续计划」成为一段可复用、可恢复、防重、受权限约束的程序**，同时（1）崩溃不重做已完成工作、（2）双副本不双驱动、（3）程序不能绕过权限。两条路线都服务这同一需求，只在「用哪种 JS 运行时」上分叉。

---

## 15. 所以这解决了什么

它补的不是「Go 会不会跑 JS」，而是现有系统缺的那一层能力：**让模型把结果驱动的循环、分支和动态扇出写成可复用程序，同时不让程序绕过权限，也不让进程崩溃和双副本把已完成的工作重做一遍。**

把 §1.1 的需求原文重述一遍：用户要的是「持续审查并修复，直到检查通过或连续两轮没有进展」——**事先不知道会报几类错、第二轮集合可能完全不同、停止条件运行中才知道**。这段程序：

- 用一门语言的循环／分支／`await`／动态扇出表达「结果驱动」，不用把未来节点预先编进图；
- 用进程外的隔离运行时把模型写的代码关在有内核级上限、无网络、无 bash/文件/MCP 的盒子里；
- 用 PG 里的调用事实 + 「重放脚本返回保存结果」把崩溃变成「几乎免费地接着跑」，而不是重做；
- 用数据库的条件更新与唯一索引让多副本下同一件事**只有一个驱动者、一个调用一件工作**；
- 用服务端裁决 + 冻结名单 + 既有权限路径让程序**拿不到租户、项目或任意工具**。

本条路线在满足这一需求之外，**不新增任何语言运行时**：JS 执行落在 tako 自己的纯 Go 二进制里——一个套着现有 bash 沙箱、内存受 OS 限制、不连数据库也不读密钥的极小进程。它放弃的是「随便写现代 JS」的便利，换的是更短的供应链、更强的确定性与更小的安全面。

---

## 16. 附录：代码出处与合成说明

### 16.1 本文事实断言的代码出处（供对质）

- run 领取与「INSERT runs 即准入」：`store.go:163` `AcquireRun`。
- 驱动/恢复 CAS（`epoch+1`+续租约）：`store.go:955` `ClaimResume`、`store.go:1012` `AbandonClaim`。
- 会话忙的部分唯一索引：`pgstore.go:48`／`:52` `one_live_run_per_session`。
- 一执行一 Task 的部分唯一索引：`pgstore.go:993` `agent_tasks_exec_session_once`、`pgstore.go:998` `agent_tasks_current_run_once`。
- 编排组节点上限与创建幂等唯一索引：`agent_task_types.go:156` `agentTaskMaxCount=32`、`:194` `agent_orchestrations_idem`。
- 领取事务与组内一致性：`agent_task_scheduler.go:545` `ClaimReadyInGroup`。
- 检查点与复核工具面：`agent_task_scheduler.go:208` `applyPlanCheckpointTx`、`agent_task_delivery.go:421` `planReviewToolNames`。
- Agent 规格严格解析：`agents.go:48` `AgentSpec`、`agents.go:136` `parseAgentSpec`。
- 沙箱：`sandbox.go:47` `detectSandboxBackend`、`:149` `sandboxProfile`、`:248` `sandboxWrap`、`:284` `bwrapArgs`、`:442` `killProcessGroup`。
- 迁移与全局清：`pgstore.go` 的 `AutoMigrate` 与显式 DDL、`store.go:1027` `Reset`。
- 契约与验收设施：`contract_test.go`、`scripts/acceptance/{expected.json,assert_contract.py,check-expected.sh,spotcheck.sh}`。
- 引擎能力（**仓库外，社区资料，待 §12.0 PoC 证实**）：选定纯 Go 引擎的 `Interrupt`、调用栈上限、Promise、async 函数对象与 ES5.1 + 部分 ES6 支持。

### 16.2 合成说明（arena）

本文由 arena 方法产出：三份并行候选（① 受限 JS + Node 子进程；② 纯 Go 内嵌 JS 引擎；③ 不引脚本语言的持久动态原语 + 模型在环）各自独立成文，再由只读裁判按六条 rubric（病灶覆盖、多副本防重、恢复签名、代码接缝接地、契约具体度、解释与范围纪律）逐条评分。

- **选定的 base = 候选②**：它保住「机器自己执行结果驱动控制流」这个主题（语言作控制逻辑，不把每轮判断交回模型），去掉了 doc/85 的结构性代价（新增 Node 运行时），并在「恢复确定性」「契约具体度」两条上强于候选③；它唯一的硬风险（嵌入引擎的 async 驱动能力）被自己立成**施工前置 PoC 门槛**，不冒充已证。
- **从候选①嫁接**：§2.6「宿主面只留一个真·宿主动词」——`parallel`/`pipeline` 降为随 runner 发布的 JS 薄封装；§2.1 的「为什么必须是语言而不是声明式原语」论证（正是 doc/82 §12 要求的那份证明）。
- **从候选③嫁接**：§13「这条路线做不到什么、边界在哪」的诚实清单，与「Flow 不是默认路径、不做自动复杂度路由」的定位。
- **未采纳**：候选①（新增 Node 运行时，且其核心机制与 doc/85 实质相同，作为「重写」的新意最少）；候选③（不引语言、复用现有计划复核机制，是强备选，但把每轮判断交回模型，恢复签名押在「决策 run 无副作用」这条纪律上，且契约具体度较弱）。候选稿留在 `tmp/86-flow-arena/`（不入库），本方案点头与否均不改变它们的存在。
