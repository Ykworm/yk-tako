# 叶子才是根：被 Eino 中断契约坑掉的一天

## 1. 三个偷懒的问题

背景一句话：这个助手跑在终端里。你说一句话，它自己决定调哪些工具——跑命令、读写文件——
把事情做完；危险操作会先弹出来问你要不要批准。我最近想给它补两块东西：

- **任务看板**：多步骤的请求，先拆成任务清单，边做边更新状态——也就是"先计划，再动手"；
- **工具安全层**：命令、文件、密钥各过一道规则，该拦的拦、该问的问。

真正卡住我的是三个问题：

1. **agent 能不能自己决定进 planning 模式？** 一个请求来了，它自己判断"值不值得先立个计划"，而不是让用户去拨一个开关。
2. **planning 衍生出来的任务，状态改在哪？** 模型自己调工具改，还是代码里替它改？
3. **这些规则要不要写进 agent 的 instruction（系统提示词）？**

三个问题都不大，但答案决定设计形状。第一个问题决定要不要做"模式开关"这个子系统——如果 agent 自己能判断**一个请求要不要先立计划**，开关就不用做了：省掉一套状态、一套提示词变体、一套入口逻辑；后两个问题决定规则住在哪：提示词里，还是代码里——它决定以后改行为的人该翻哪个文件。

但猜不算数，代码才算。这三问的答案，全取决于一个更底层的事实：**框架在一轮对话里到底给了 agent 多少自由**。于是我开始翻 Eino 的 agent 实现——这一翻，一天没了。

## 2. 一个 turn，不止一次调用

先说我原来的心智模型：一个 turn = 一次 LLM 调用。用户说一句，模型答一句。

读代码发现不是。一个 turn 是一个循环：

```
LLM 决策 → 工具调用 → 结果回填 → LLM 再决策 → …… → 直到模型不再要工具
```

配置里 `MaxIterations` 决定这个循环的上限，我设的是 20——意味着一次用户输入，
模型最多能在这个循环里转 20 次。它不是在"回答"，是在循环里"边做边想"。

第二个发现：这个循环里，框架在好几个位置留了插手点（钩子），由 middleware 实现，
接口叫 `adk.ChatModelAgentMiddleware`。以工具执行为例——每次工具即将执行时，
框架会调用 `WrapInvokableToolCall`：

```go
// github.com/cloudwego/eino/adk/handler.go:193
WrapInvokableToolCall(ctx context.Context, endpoint InvokableToolCallEndpoint, tCtx *ToolContext) (InvokableToolCallEndpoint, error)
```

翻译成人话：框架把"真正干活的工具函数"（`endpoint`）交给你，让你返回一个新函数，
这个新函数变成实际入口。工具执行链就像洋葱，middleware 一层层包上去。

还有一个钩子叫 `BeforeAgent`，每次 agent 启动一轮执行时调用，给 middleware 一次
改"本轮指令和工具表"的机会。我的两个 middleware 正好一动一静：
一个用 `BeforeAgent` 往工具表里**加工具**；一个用 `WrapInvokableToolCall` 把工具调用
**拦下来**。两者都内嵌 `adk.BaseChatModelAgentMiddleware`，拿到其余钩子的空实现，
只覆盖自己关心的那个。

第一个问题开始有眉目了：模型这一轮能看见哪些工具，不是固定的，是可以每轮变的。

## 3. 工具的两条注册路径

想清楚上面那件事之后，我回头看自己的代码，发现工具居然有两条注册路径。

**路径一：写在 agent 配置里（静态）。** 基础工具——取时间、算数、跑命令、读文件、写文件——
组好之后直接进 `ToolsConfig`，agent 一造好就在工具表里。

**路径二：middleware 在钩子里追加（动态）。** 看板的四个记账工具
（`TaskCreate` / `TaskUpdate` / `TaskList` / `TaskGet`）不在配置里，
而是在每轮开始的时候被塞进 `runCtx.Tools`：

```go
// 一个往本轮工具表里动态追加工具的 middleware（eino 自带的 plantask 同款写法）
func (h *handler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	nRunCtx := *runCtx
	nRunCtx.Tools = append(nRunCtx.Tools, h.tools...)
	return ctx, &nRunCtx, nil
}
```

`runCtx.Tools` 就是这一轮的工具表。注意它是"先拷贝、再追加、返回副本"——
改的是这一轮的副本，不动调用方的原值。

一个框架为什么要留两条路？算一笔账：

- 静态注册的成本是"读的时候简单"：所有工具在一个地方，一眼看全；
  代价是工具必须在构造时就存在，没法跟着每轮的状态变。
- 动态注入的成本是"找的时候费劲"：工具来源散在若干 middleware 里，
  读代码要顺着钩子找"谁在什么时候往里加了什么"；换来的是按状态变化的能力。

两边买的东西不一样：一个买好读，一个买灵活。框架把两条都留着，不是设计不收敛，
是把选择权交给使用方。我的取舍也各有理由：基础工具永远都在，静态；
看板工具只在"有看板"的语义下才有意义，动态。

顺带还有一个发现，藏在这行 `append` 里：**注册顺序不是清单顺序，是执行顺序**。它同时管着两件事：

```go
// boardMW = 看板 middleware；guardMWs = 命令 / 文件 / 脱敏三个守卫
handlers := append([]adk.ChatModelAgentMiddleware{boardMW}, guardMWs...)
```

第一件事：**钩子按这个顺序串行执行**。`BeforeAgent` 从第一个走到最后一个，每个 middleware 把改完的 runCtx 交给下一个——所以看板 middleware 先注入工具，后面的守卫原样透传。

第二件事：**工具执行的包裹层次也按这个顺序，从外到内排**（eino 源码注释原话：`handlers[0]` ends up outermost，`adk/wrappers.go:114`）。一次工具调用先过命令守卫：它要是拒绝或暂停，链条到此为止——文件守卫和脱敏 middleware 根本不会跑；批准之后，请求继续往里穿，最后脱敏贴着真工具把输出打码，往回传的时候已经处理过。（看板 middleware 也在这条链上，但它的工具钩子是基类空实现，直接透传。）

顺序一换，行为就换。比如把脱敏挪到最外层：一次注定被拒绝的调用也得先穿过它一圈。注册顺序是要当语义来对待的——`append` 写完的那一刻，执行序就定死了。

到这里，第三个问题也有答案了：instruction 里放的是"什么时候用看板"的约定
（我把它拼在系统提示词后面：`系统提示词 + "\n\n" + 看板约定`），
而真正决定"模型能不能用一件工具"的，是每轮动态拼出来的工具表。**提示词和工具，一个都不能少。**

## 4. 有的工具会喊暂停

看板工具很温和，守卫工具不是。审批模式下，撞上风险规则的工具
既不会被执行，也不会被拒绝——整个 run 会暂停：

```
工具即将执行 → 守卫命中规则 → 暂停整个 run → 状态存进 checkpoint → 问人 → 按人的决定继续
```

在 Eino 里这个暂停叫 interrupt（`compose.StatefulInterrupt`）。它能成立，
靠的正是 `WrapInvokableToolCall` 给的那个插手位置：**模型决策之后、工具真正执行之前**。
这个位置很讲究——再早一点，你只知道用户说了什么，不知道模型要调哪个工具；
再晚一点，工具已经跑完了。人要插手，只有这一个窗口。

`endpoint` 的定义只有一行：

```go
// github.com/cloudwego/eino/adk/handler.go:34
type InvokableToolCallEndpoint func(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error)
```

它就是真工具的执行函数。包裹它的新函数里，你有三种玩法：直接调（放行）；
换参数再调（审批通过后恢复原调用）；不调它、直接返回一段字符串（拒绝）。
审批 middleware 正好把这三种都用上了：

```go
// 审批 middleware 的核心（简化）
return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	if stored, ok := ownInterrupt(ctx); ok { // 认出：我被打断过，这是一次续跑
		decision, decided := resumeDecision(ctx) // 取回：人的决定
		if !decided {
			return "", interruptCall(ctx, stored) // 还没有决定，继续暂停
		}
		if decision != approve {
			return "用户拒绝了这次调用。不要重试相同命令；……", nil // 拒绝：不执行，回一句话
		}
		return endpoint(ctx, stored.Args, opts...) // 批准：用存下来的原始参数，真执行
	}
	// ……第一次执行：扫规则；命中且需要人工批准时
	// return "", compose.StatefulInterrupt(ctx, info, savedCall{...})
}
```

（`ownInterrupt` / `resumeDecision` 是自己写的薄封装，里面就是 `compose.GetInterruptState` 和 `compose.GetResumeContext`，后面会再遇到。）

这里有个反直觉的设计值得停一下：拒绝返回的是**字符串，不是 error**。为什么？
如果抛 error，模型只看到"工具执行失败"，不知道是人拒绝的，可能傻乎乎地重试；
返回一句"用户拒绝了这次调用，不要重试，需要的话先跟用户商量替代方案"，
模型就懂了——这是人的决定，不是系统故障。拒绝也要把话说清楚，机制上做不到的部分，
用沟通补上。

继续跑起来的路径也值得记一笔：

```go
// 驱动循环的关键几步（简化）
targets := map[string]any{}
for _, point := range interrupts {
	decision := ask(point) // 问人：批准或拒绝
	targets[point.ID] = decision // 决定挂在这次中断的 ID 上
}
iter, err = runner.ResumeWithParams(ctx, checkpointID, &adk.ResumeParams{Targets: targets})
```

`checkpointID` 是"被暂停的那次执行"在 store 里的钥匙——我传的是 `turn-1`、`turn-2`
这样的轮次编号；store 用的是内存实现（换了 Redis 之类也只是换一个实现）。
暂停时整个 run 的状态序列化存进这把钥匙；恢复时用同一把钥匙取回来，接着跑。

问题也来了：`point.ID` 是个什么东西？决定挂在它上面，凭什么就能找回去？
以及——`interrupts` 这个列表是怎么筛出来的？

我收集中断的代码是这样写的，然后被它坑了半天：

```go
for _, point := range event.Action.Interrupted.InterruptContexts {
	if point != nil && point.IsRootCause {
		interrupts = append(interrupts, point)
	}
}
if len(interrupts) == 0 {
	interrupts = append(interrupts, event.Action.Interrupted.InterruptContexts...)
}
```

## 5. 卡住我的 IsRootCause

### 5.1 我先推错了一遍

看名字，`IsRootCause` 就是"根因"。再想一下"interrupt 是棵树"这个信息
（为什么是棵树，当时我还不知道，但这个名字暗示了树），很自然地推出：

1. `IsRootCause` 标记的是根因，那它应该在树根上；
2. 树根在最外层，包着全部子中断；
3. 所以这行过滤是在挑"树根"，也就是挑祖先；下面那句 `len == 0` 的兜底，
   是防"一个都没挑到"的退化情况。

每一步都"看着对"。如果你也在点头——正常，这个坑专坑会推理的人。

### 5.2 两份文档说了相反的话

为了确认，我去翻 Eino 的注释。字段文档是这么写的：

```go
// github.com/cloudwego/eino/adk/interrupt.go:52
// InterruptContexts provides a structured, user-facing view of the interrupt chain.
// Each context represents a step in the agent hierarchy that was interrupted.
```

"每一步"——照这个读，列表里应该有整条链，祖先和叶子都在，那过滤当然必要。

但同一个包里的函数文档（`ToInterruptContexts`，`internal/core/interrupt.go:258`）
说的正好相反：返回的是 "a list of ... **for the root causes** of the interruption"，
祖先不在列表里，只通过 `Parent` 链可达。

两份文档，一句说"混合"，一句说"只有真因"。信谁？都不能信——文档是别人写的，
代码是它实际做的。这种事只能去读赋值。

### 5.3 去读赋值的那一行

创建中断信号的地方，只做了一件事：

```go
// github.com/cloudwego/eino/internal/core/interrupt.go:102
if len(subContexts) == 0 {
	myPoint.IsRootCause = true
}
```

"没有子节点"才为 true。也就是说，`true` 的那一个是**自己按下暂停键的组件**，
是最内层的叶子；所有替它传话的外层（树根在内），全是 `false`。真实的信号树长这样：

```
信号树（Runner 手里那个信号往下看）          IsRootCause
└─ ChatModelAgent 复合信号                    false
   └─ ToolsNode 复合信号                       false
      └─ 审批 middleware 的 StatefulInterrupt  true   ← 列表里只有它
```

真因在叶子；结构上的树根本来就只是信封。而 `ToInterruptContexts` 只把 `true` 的装进列表，
祖先只留在 `Parent` 链上——resume 时靠这条链把整棵树重建起来（`FromInterruptContexts`
干这个，`internal/core/interrupt.go:202`）。

所以我那段过滤，在标准路径上其实是个无害的 no-op；
`len == 0` 的兜底防的是极罕见的退化形态（子树丢失那种），不是日常。
它没错，但它也从来不干活。

### 5.4 一个错为什么能活这么久

到这里还有个问题没解开：一个语义反直觉的字段名、两份互相打架的文档，
为什么会一起活到现在？官方测试里甚至也在抄那行防御性过滤
（`adk/agentic_react_test.go:1158`、`:1311`），就像在给你发一张"大家都这么写"的许可证。

算一笔账就明白了。修文档需要：读完实现、想清楚正确表述、承担"改错了"的责任——
成本是维护者一个人的。而修好以后省的弯路，是给所有使用者的，一分也回不到修的人头上。
一个人出成本、所有人受益的事，供给不足不奇怪。

更糟的是官方测试的那行过滤：它把"冗余"包装成了"惯例"。每个读到它的人都会想
"官方都这么写，那肯定是必须的"，于是接着抄。坏惯例的成本不是一次性的，
它会被权威示范放大。

所以结论不是"Eino 有 bug"——机制上树和叶子都必需：树撑着跨层地址，
列表给出要回答的问题。缺陷在 API 的"表达"：一个冗余的 flag、两段互斥的文档、
一份抄了防御写法的官方测试，凑成了一个专坑认真读文档的人的组合。

### 5.5 复盘

`IsRootCause` 回答的问题是"这次中断是不是你亲手发起的"——它在描述**角色**，
不是在描述**位置**。我错在读名字，而不是读行为。

名字是人起的，行为是代码写的。两者冲突时，以行为为准。这不是什么了不起的道理，
但那天它值四个小时。

我给自己留了个可检验的尾巴：把那行 `if` 删掉跑全量测试——如果标准路径全绿，
就证明它真是 no-op；如果绿不了，那说明列表里真的混了东西，问题就不是文档，是实现。
一个判断能被证伪，才值得写进笔记。

## 6. 深潜之后，我忘了主程序怎么拼

这一天最诡异的部分来了。读了一天的中断、信号、地址之后，我合上电脑，
想跟人讲讲这个 agent 是怎么搭的——然后发现说不清了。

是先建模型还是先建 agent？工具是在哪注册的？`runner.Run` 的入参到底是什么？

局部我全懂，整体丢了。于是我把一个 Eino agent 的组装重新拼了一遍：

1. **模型**：选一个 chat model；
2. **工具**：把要给模型用的工具装进配置（静态那批）；
3. **middleware 链**：谁先谁后想清楚——顺序即执行序；
4. **造 agent**：`adk.NewChatModelAgent`，把模型、工具配置、handlers、instruction 一起装进去；
5. **造 runner**：`adk.NewRunner`，配上 checkpoint store；
6. **驱动**：跑一轮、收事件、遇到中断问人、`ResumeWithParams` 接着跑——外层套一个 REPL 循环。

六步摆出来，其实很直白。写成代码，骨架大概是这样（省略错误处理）：

```go
// 1. 模型
chatModel := mustModel(ctx)

// 2. 静态工具（动态那批由 middleware 每轮注入）
baseTools := []tool.BaseTool{bash, readFile, writeFile, /* …… */}

// 3. middleware 链：顺序即执行序
boardMW := newBoardMW()   // 看板 middleware：BeforeAgent 里每轮把看板工具注入工具表
guardMWs := newGuardMWs() // 三个守卫 middleware：WrapInvokableToolCall 里拦命令 / 文件 / 脱敏
handlers := append([]adk.ChatModelAgentMiddleware{boardMW}, guardMWs...)

// 4. agent：模型、工具、handlers、instruction 一起装进去
agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
	Name:          "my-agent",
	Instruction:   systemPrompt + "\n\n" + boardInstruction,
	Model:         chatModel,
	ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: baseTools}},
	Handlers:      handlers,
	MaxIterations: 20,
})

// 5. runner：checkpoint store 是 interrupt/resume 的落点
runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, CheckPointStore: store})

// 6. 驱动：跑一轮；遇到中断就问人，再按同一个 checkpointID 恢复
iter := runner.Run(ctx, messages, adk.WithCheckPointID(checkpointID))
// ……消费事件、收集中断（就是前面那段过滤）……
iter, err = runner.ResumeWithParams(ctx, checkpointID, &adk.ResumeParams{Targets: targets})
```

那为什么读了深水区反而说不清？因为**深度是拿广度换的**：
读 interrupt 的时候，脑子里全是信号、地址、checkpoint，组装那层"谁先谁后"的图被挤出去了。
理解一个系统，不是把每个零件都读过，是随时能把零件重新拼回去——那天我丢了拼图的能力，
等于暂时丢了理解。

也是在这个"重新拼"的过程里，我意识到自己漏了什么。这套组装里没有一个文件叫"树"：
工具在各自的位置注册，守卫在钩子里拦，中断在 store 里存，决定在 map 里挂。
每块都对得上，但把它们串起来的那根线，不在组装的任何一步里。

我漏掉的是那棵树。

## 7. 我漏掉的，是那棵树

### 7.1 第一份工作：把中断从最深处抬到最外层

中断发生在最深的地方：审批 middleware 包着工具函数，按下暂停的是叶子。

但"暂停"这个决定要传到最外层给 Runner，中间隔着 ToolsNode、ChatModelAgent 好几层。
每过一层，外层就把内层的中断包进自己的中断里（`CompositeInterrupt`），一层层往上收，
最后 Runner 手里是一个完整的信号树。

没有这棵树行不行？每层各抛各的，到最外层就变成一句"内部出了点问题"——
最里面那次"要调用哪个工具、参数是什么、是谁在问"全丢了。
树是让外层"知道里面发生了什么"的唯一方式。

### 7.2 第二份工作：resume 的地址簿

这是树真正让我服气的地方。

先说清 `id` 是什么，因为前面一直在回避它。

每个中断点在生成那一刻会拿到一个 id，实现是 `uuid.NewString()`——一个现生成的随机
UUID。它只是一张"身份证"，不回答"这个中断发生在哪"。（这里还有个小插曲：
`InterruptCtx.ID` 的字段注释说它本身就是"由地址段拼成的字符串，如
`agent:A;node:graph_a;tool:tool_call_123`"；v0.9.21 的实现给的是 UUID。
又是一处文档和实现不一致——和 `IsRootCause` 一个毛病。）

那为什么还需要知道"发生在哪"？因为 resume 不是跳回某个内存地址继续跑，
而是**重入整条执行链**：从 checkpoint 恢复现场后，执行一层层重新走下来，
走到刚才暂停的那个地方。走到时必须认得出来"就是这儿"——靠什么认？
靠**地址**：从执行链最外层到该中断点的段链（哪一层 agent、哪一个节点、
哪一次工具调用……）；重入时每下钻一层，这段链就往前长一节，
长到和存档一致，位置就对上了。

于是中断发生时，整棵树被 `SignalToPersistenceMaps` 遍历一遍，每个节点登记进两张表。
这两张表是后面反复要用的索引，值得看清楚：

- **`InterruptID2Address map[string]Address`**：key 是中断 id，value 是结构化地址——
  一串 `AddressSegment`，每段三样东西：类型（agent / 节点 / 工具调用）、id（agent 名、
  节点 key、工具名）、必要时再补一个子 id（比如并发调用的两次同名工具，靠 tool call id
  区分）。`agent:A → node:graph_a → tool:call_123` 这样串起来，就是中断点的门牌号。
- **`InterruptID2State map[string]InterruptState`**：key 是同一个中断 id，value 是
  中断者随中断交出来的东西——就是你调 `StatefulInterrupt(ctx, info, state)` 时传的
  那份 `state`（业务数据，比如被暂停那次调用的原始参数），结构体上再加一个中间层
  可选的附加字段。

保存时，`runnerSaveCheckPointImpl` 把这两张表连同整个执行现场（消息历史、循环进度……）
一起 gob 编码进 checkpoint 结构体，`CheckPointStore` 按 `checkpointID` 存下这坨字节——
`checkpointID` 就是取这份东西的把手。

恢复的时候，`ResumeWithParams` 只做两件事：把 checkpoint 里的现场读回来，
再把驱动层传的 `Targets`（`targets[point.ID] = 决定`）放进 ctx——
**贴公告，不是投递**：决定挂在 ctx 上，等重入时被认领，而不是由框架亲手交给谁。

然后重入开始：每下钻一层，框架拿当前地址去查 `InterruptID2Address`——对上的瞬间，
把旧状态和人的决定挂到这一层的 ctx 上。这两张表在运行时的入口叫 `rInfo`
（checkpoint 解码后，`PopulateInterruptState` 把它们原样装进 resume info）：

```go
// github.com/cloudwego/eino/internal/core/address.go:154，简化
// rInfo.id2Addr / rInfo.id2State：就是 checkpoint 里那两张表在运行时的样子
var id string
for id_, addr := range rInfo.id2Addr {
	if addr.Equals(currentAddress) {
		runCtx.interruptState = generic.PtrOf(rInfo.id2State[id_]) // 旧状态挂上
		id = id_
		break
	}
}
if rData, existed := rInfo.id2ResumeData[id]; existed {
	runCtx.resumeData = rData // 人的决定挂上
	runCtx.isResumeTarget = true
}
```

然后被中断的那个 wrapper 重新跑起来——注意，**没有专门的"恢复分支"**，
就是把第一次执行的路径原样再走一遍。审批 middleware 在自己的包装函数里读 ctx：

```go
if stored, ok := ownInterrupt(ctx); ok { // 认出：这就是我那次中断的续跑
	decision, decided := resumeDecision(ctx) // 读走：挂在 ctx 上的决定
```

妙处在这里：驱动层写 `targets[point.ID]`，审批 middleware 读
`GetResumeContext`——两段代码互不引用，中间没有任何函数调用把它们连起来。
它们靠"地址"在运行时相遇：写的人在公告板上贴 ID，重入的每一层拿自己的地址来认领。
是数据耦合，不是调用耦合。

所以 `Targets` 的 key 只能照抄 `point.ID`：你不能自己编一个 id（它是随机的），
也没法从它字面推出地址——两者之间唯一的桥，就是 checkpoint 里的 `InterruptID2Address`。

为什么要设计成这样？再算一笔账。如果每层自己管理自己的恢复状态：
N 层就有 N 套恢复逻辑，跨 agent 边界时对不上号，还容易漏。
现在把"谁在哪、谁被问了什么"集中登记成一棵树、两张表，
恢复逻辑只实现一次、所有层复用。集中登记比各自记账便宜——前提是地址链得统一。
树就是这个统一。

### 7.3 回到那行过滤

现在再看我那段过滤，就顺眼了：保留没问题（意图自明，防御性），
但真正承担正确性的不是它，是两件事——

- `targets[point.ID] = decision`：把决定挂在正确的中断 ID 上；
- 问人的时候只回答 `point.Info`：只处理真因列表里给出的问题。

那行 `if` 干不干活，都不影响这套机制。这就是 no-op 的底气。

## 8. 带走的三条

1. **读框架**：文档是别人说的，代码是它实际做的。名字和行为冲突时，以行为为准；
   怀疑的时候，设计一个能被证伪的实验。

2. **写 API**：一个词最好只有一个意思。`root` 同时指"树根"和"根因"的那天，
   坑的不是我这种路人，是每一个认真读文档的人。冗余的 flag 也别留——
   它不会闲着，它会招来防御性的抄写，然后变成惯例。

3. **看系统**：深潜之后，一定回来重新拼图。理解 = 能重建。只读局部，会把整体弄丢。

最后，回到开头那三个偷懒的问题。

绕了一天，答案其实中途就出现了，只是当时没认出来：所谓"进入 planning 模式"，
不是 agent 内部的某个开关。它是两件外部的事——**每一轮把工具摆在它手边**
（用 `BeforeAgent` 动态注入），**提示词里教会它什么时候用**（静态拼进 instruction）。
它不是在两个模式之间切换，它是在每一轮里选择要不要调那些工具。

要不要把规则写进 instruction？要。但另一半"权力"在工具表里：
模型能不能用一件工具，不只由它的意愿决定，还由我们每轮放不放进去决定。

问题没白问，只是答案比问题大。