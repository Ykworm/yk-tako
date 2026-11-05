# 19v6-05 · tool loop（T 系列）

> 本篇是 doc/19-v6 文档集的 **tool loop 篇**：Eino ADK 底座、工具消息的两个投影、sub agent（AgentTool）、
> 工具目录与 tool_search、观测 Handler 脚手架。
> **契约字节真源仍是 19v6-01**——本篇涉及的帧新 kind 与 `agent` 字段**已随 T2（2026-10-08，七修）落 19v6-01**；
> `model` 字段（2026-10-10 纠偏，帧/行同带）**已补 19v6-01，随 T3-1b/T3-2 落地**；
> 本篇是 tool loop 的**机制裁定真源**。
> 立项：2026-10-08（用户拍板：循环走 Eino ADK）；执行台账 = `tmp/19-v6/planning.md` §11 + todos「T 系列」。
> **T4/T5 已落地（2026-10-20）**：工具分级（direct/deferred）+ `tool_search`（BM25 首版）A1–A4 四拍完成——
> A1 `ad1ab6b`（`toolsearch.go`：分词/BM25/检索/run-local box + 单测）、A2 `a190f4d`（子代理保留名与
> 显式挂载）、A3 纯验收（激活集只活一场 run）、A4 回归与收口（单测补网、A1/A2 话本重跑、四个 API
> 字节零变化）。落地细节与验收实录见 §5 尾条与 `tmp/19-v6/todos.md`。

## 0. 术语（2026-10-08 用户裁；2026-10-10 补 3 条模型侧术语）

先看「观测 / 存储」侧三个词，三种形状 / 住处 / 消费者，勿混：

| 词 | 形状 | 住处 | 消费者 | 粒度 |
|---|---|---|---|---|
| **Model message** | OpenAI messages（role/content/tool_calls） | 主线程 → `run_model_messages` 表；子代理 → **只在进程内**（AgentTool runner state） | worker 组装模型输入 | 消息 |
| **runlive** | 帧（`kind` + `agent` + `model` + 负载） | Redis `tk:runlive:{runId}` LIST（TTL 300s） | frames API（客户端轮询） | 帧 |
| **runlog** | 活动行（`role`/`agent`/`model`/`text`/`payload`） | PG `runlog_entries` | log API | 消息 |

再看「模型选型 / 客户端」侧三个词（§4 开始大量用，先钉死定义）：

| 词 | 形状（正向定义） | 住处 | 消费者 | 例子 |
|---|---|---|---|---|
| **ref** | 字符串，格式 = `厂商ID / 模型ID`。唯一身份：带厂商命名空间，避免同名模型撞车。 | 不单独存：从环境变量 `TAKO_MODEL` 读、或从子代理 md 的 `model:` 行读 | `ResolveModel` 解析它、ModelRegistry 拿它当缓存 key、帧/行里的 `model` 字段存它 | `deepseek/deepseek-v4-flash`、`anthropic/claude-sonnet-4`、`ollama/qwen2.5-coder:14b` |
| **ChatModel 实例** | 一个具体的 HTTP 客户端：里面装好了 baseURL、API Key、协议（openai/anthropic）、模型 ID。**无状态，不记任何对话**。 | 进程内存（Go struct） | ChatModelAgent / AgentTool 的 loop 用它发 LLM 请求 | 主 worker 启动时 `config.Open()` 返回的那一个；子代理按 ref 新建的那些 |
| **ModelRegistry** | 一张进程内的**懒加载缓存表**：`map[ref]ChatModel实例`。懒加载 = 进程启动时是空的，**第一次用到某个 ref 才解析并建客户端**，之后同样的 ref 直接复用。 | 进程内存（Go struct，带读写锁防并发重复建） | 装配层（BeforeAgent）把子代理规格转成 ChatModelAgent 时用它取实例 | 一条规格写了 `model: anthropic/claude-sonnet-4`，第一次遇到时 ModelRegistry 去解析环境变量、建 claude 客户端、存进 map；下一条同样写了这个 ref 的规格直接拿 map 里的，不再重复解析 |

index 概念表对 runlive/runlog 的定义不变；本篇钉死的是「模型输入侧叫 Model message、观测侧叫帧/runlive」，
以及两者**永不混写**：子代理的 Model message 不进 mh、帧不进 mh。

## 1. 底座：Eino ADK

- `ChatModelAgent` 进程内 loop 嵌进 worker（旧线 `adkrun` 先例）；watchdog / endRun / 闸路径不动。
- **铁判据 = 四个 API 契约字节零变化**（同 A7/D1 内部改造拍的判据）——底座更换不出契约面。
- worker 的手写 Stream 循环与 claude/openai/eino 三 translator 由「ADK 事件流 → 归一层 → v6 帧」重写：
  ADK 的 `TypedAgentEvent` 迭代器是**引擎内部物**，v6 帧是**契约物**，归一层是两者唯一接缝。
- **观测 Handler 脚手架**（用户指令，2026-10-08）：`ChatModelAgentMiddleware` 全套钩子
  （BeforeAgent / AfterAgent / BeforeModelRewriteState / AfterModelRewriteState / WrapModel /
  WrapInvokableToolCall / WrapStreamableToolCall / WrapEnhancedInvokableToolCall / WrapEnhancedStreamableToolCall
  ——v0.9.21 实读的九方法全集，T1 勘误）
  每处打终端日志 `[adk][序号] 钩子名｜功能一句话｜进/出`。转正点 = T6（收成旗标或拆，登记 planning §4）。

## 2. 工具消息的两个投影

同一次工具调用，两个投影各自成形状（种类由 `role` 取值承载，2026-10-07 已裁）：

- **mh（Model message，模型输入）**：OpenAI messages 形状。assistant 行 `text` = 纯正文不夹带；
  新增 `tool_calls` / `tool_call_id` 两列（§6）；tool 行 text = 工具结果。
  多轮时**逐轮一行**：工具轮的前导语在自己那行（与 tool_calls 同行），不并进终稿行——终稿行只放最后一轮正文。
- **runlog（展示与历史）**：消息粒度活动行——assistant 正文行 + 扩展行
  （role=`tool_call` / `tool_result`，`payload` 列存 name/args/output 等结构化负载）。
  assistant 行 text = 全 run delta 累计（所见即所得，收口替换的锚）。
- **runlive（实时观测）**：帧粒度——`tool_call` / `tool_result` 帧 + 流式 `delta` 帧。

## 3. 帧契约扩展（已落 19v6-01 §2，2026-10-08 七修）

- **新 kind**：`tool_call{name,args}` / `tool_result{name,output}`（字段名已钉，见 19v6-01 §2；
  `args` = 模型原始 JSON 字符串，原样透传不重编码）。
- **每帧带 `agent` 字段**：空 = root，含 start/terminal（2026-10-08 用户裁「任何帧都带 agent 是必然」）。
  命名取 `agent` 不取 `role`——帧 `kind` 与 runlog 行 `role` 已是同位语义，再引 role 两义。
- **每帧带 `model` 字段**（2026-10-10 纠偏）：生效 ref（`厂商/模型`）——契约已补 19v6-01 §2，随 T3-1b 落地；
  行侧 `model` 列随 T3-2（§6 已登记，PG 落库）。
- runlive 帧无 seq：LIST 顺序 = 下标 → frames 响应的 `nextOffset` 游标；`agent` 与之并列携带。

## 4. sub agent：AgentTool 自组装

- **裁定**：`adk.NewAgentTool` 每规格一包挂主 agent（tool as agent，Octop「tool 变子代理」同构）；
  **不采用 `adk/prebuilt/deep` 整包**——捆绑 write_todos/文件系统/Shell 与自带 prompt 哲学，
  工具集按「没有用例逼出的不进契约」由用例逐个逼出。
- **规格形态 = 文件式（2026-10-09 裁）**：`yk-tako-eino/agents/` 单目录，**md + YAML frontmatter**、
  正文 = system prompt；**装载 = run 起手扫盘**（改文件 → 下一条消息生效、run 中途不变；run 内工具集固定、
  子代理 runner 不重扫盘）。理由链：每规格一包 AgentTool ⇒ 工具清单 = 规格名单 ⇒ Eino 工具 schema 定稿于装配点
  （`once.Do`、首次 Run 前冻结）⇒ 要新鲜只能重装配 ⇒ 装载点 = 重装配点 = run 起手。
  实现取 `BeforeAgent` 换本次工具列表（T3-1 源码实读 + 真栈落地）；随仓 seed 一条 tako 自写规格。
- **字段表（2026-10-09 裁）**：`name`（单字段身份 = AgentTool 工具名 = runlog `agent` 值；文件名不参与语义；
  撞名 → 拒该条 + 显式报错）/ `description`（必填，缺 → 拒该条报错）/ `tools`（2026-10-09 三态、
  2026-10-16 加 `exclude_tools` 成四态，见下与 `doc/26` §3）/
  `model`（写值分**三种写法穷举**，不漏不重，实现落 T3-1b，链路与例子见下）：

  | 写法编号 | 在 agents/*.md 里怎么写 `model:` | 含义 | 行为 | YAML 例子 |
  |---|---|---|---|---|
  | 写法一 | **根本不写** `model:` 这一行 | 隐式继承主 worker 的模型 | 直接拿主 worker 启动时建好的那个 ChatModel 实例用，不查 ModelRegistry、不新建 | （不写这行） |
  | 写法二 | 写了且去前后空格后（大小写不敏感）**正好等于**三者之一：`default` / `inherit` / `继承` | 显式声明「用主 worker 的模型」 | 与写法一**完全同义**，拿同一个主实例，不进 ModelRegistry、不新建 | `model: inherit` |
  | 写法三 | 除了写法一、写法二以外的**任何字符串**（排中律：不是前两种就一定是这种） | 明确指定要用哪个模型 | 字符串必须是 `厂商/模型` 格式（与 `TAKO_MODEL` 完全相同的写法，例：`deepseek/deepseek-v4-flash`）。调用 `ModelRegistry.Get(ctx, ref)`（进程内懒加载缓存表，见下）拿到该 ref 对应的 ChatModel 实例：<br>• 缓存里已有 → 直接复用<br>• 缓存里没有 → 走与 `TAKO_MODEL` 相同的 `ResolveModel` 链（扫环境变量找 Key → 查厂商目录 → 定 baseURL 和协议 → `build` 建客户端），建好后塞进缓存，下次再用<br>• 解析失败（没配这家厂商的 Key、厂商不在目录、格式不对缺模型名等）→ **这条规格不装载**，进程启动日志打一行明确报错，其余规格照常装载 | `model: anthropic/claude-sonnet-4` |

  **不加展示字段**（emoji/color 等）。**不设信任门**（T3：规格 = 部署态只读产物；运行时写 = 挂账，真做前先裁真源）；
  管理面（dashboard/API = 文件操作可视壳：列/读/写/删，装 = 复制、卸 = 删）后置独立小拍。
- **`model` 解析链路（T3-1b 落地；用具体规格走一遍，比抽象箭头易懂）**：

  前置说明（分阶段职责，别混）：
  - **写规格的人**：在 `agents/*.md` 的 YAML frontmatter 里写 `model:`
  - **parse 层（`parseAgentSpec`）**：只管拆 YAML 语法，把 `model:` 那一行的字符串**原样拿出来**，**不在这里判断是写法一/二/三**——判断留到装配层做，理由是 parse 只该认语法、不该懂语义
  - **装配层（BeforeAgent）**：同一个钩子函数里拿刚才原样带出的字符串，按上面三种写法的穷举表**分流**——写法一/二合并走「继承」路，写法三走「查 ModelRegistry」路

  用三条具体规格跑一遍全链路（主 worker 启动时 `TAKO_MODEL=deepseek/deepseek-v4-flash`，主实例已经建好。下图里 ① 的「什么都没写」**只指 `model:` 那一行**——`researcher.md` 自 2026-10-16 起写了 `exclude_tools` 工具排除名单，`model:` 仍不写，见 `doc/26`）：

```text
┌─ 三条具体规格（agents/*.md） ───────────────────────────────────────────────┐
│                                                                           │
│  ① researcher.md  ：什么都没写（写法一·隐式继承）                            │
│  ② writer.md      ：model: inherit（写法二·显式继承）                       │
│  ③ code-review.md ：model: anthropic/claude-sonnet-4（写法三·指定 ref）    │
│                                                                           │
└───────────────────────────────┬───────────────────────────────────────────┘
                                │
                                ▼
             run 起手扫盘（BeforeAgent → LoadAgentSpecs → parseAgentSpec）
                                │
                                ▼
                   parse 只拆语法：model 字符串原样带出
                   （不判断是继承还是 ref，判断留到下一步）
                                │
                                ▼
              ┌──────── 装配层分流（同一次 BeforeAgent） ────────┐
              │                                                 │
              │  ① researcher.md  → 没写 model 这行            │
              │  ② writer.md      → "inherit"                  │
              │        │                                         │
              │        └──► 走「继承分支」                      │
              │              直接用主 worker 已有的那个         │
              │              deepseek/deepseek-v4-flash 实例    │
              │              （不查 ModelRegistry、不新建）           │
              │                                                 │
              │  ③ code-review.md → "anthropic/claude-sonnet-4"│
              │        │                                         │
              │        └──► 走「指定模型分支」                  │
              │              调 ModelRegistry.Get(ctx, ref)     │
              │              ref = "anthropic/claude-sonnet-4" │
              │                    │                            │
              │                    ├─ 缓存命中（之前有别的      │
              │                    │    规格也写了这个 ref）    │
              │                    │    └─► 直接拿 map 里的     │
              │                    │         ChatModel 实例用   │
              │                    │                            │
              │                    └─ 缓存未命中（第一次用）   │
              │                         │                        │
              │                         └─► 与 TAKO_MODEL       │
              │                              走完全相同的链：    │
              │                              ResolveModel +     │
              │                              build 建客户端     │
              │                              → 成功：塞进缓存    │
              │                                         返回    │
              │                              → 失败：没配 Key / │
              │                                         厂商不  │
              │                                         存在等  │
              │                                         └─► 这  │
              │                                             条  │
              │                                             规  │
              │                                             格  │
              │                                             不  │
              │                                             装  │
              │                                             载  │
              │                                             +   │
              │                                             log │
              │                                             一  │
              │                                             行  │
              └─────────────────────────────────────────────────┘
                                │
                                ▼
              每条成功装载的规格 → ChatModelAgent{Model: 它拿到的实例}
                                │
                                ▼
                         包成 AgentTool → 挂进 runCtx.Tools
                         （本 run 的工具列表就齐了）
                                │
                                ▼
              同时产出一张「agent 名 → 生效 ref」的小表
              （例：researcher→deepseek/x，code-review→anthropic/s4）
              写帧、写 runlog 时，按 agent 名查出 ref，填进 `model` 字段
              （契约见 19v6-01 §2/§3）
```

  **ModelRegistry.Get 到底在做什么？（最小伪代码，30 行以内讲明白）**：

  ```go
  // ModelRegistry 就是进程内一张带懒加载的缓存表。
  // 注意：这是解释性伪代码，不是产品代码。产品代码细节（double-check、锁粒度等）落地时再抠。
  type ModelRegistry struct {
      mu    sync.RWMutex
      cache map[string]*ChatModel // key = ref，value = 已经建好的 HTTP 客户端
      env   Env                   // 进程启动时的环境变量快照（Key/BaseURL 都从这里读）
  }

  func (r *ModelRegistry) Get(ctx context.Context, ref string) (*ChatModel, error) {
      // 1. 先查缓存：之前有人用过这个 ref → 直接返回
      r.mu.RLock()
      if m, ok := r.cache[ref]; ok {
          r.mu.RUnlock()
          return m, nil
      }
      r.mu.RUnlock()

      // 2. 缓存空 = 第一次用到这个 ref。走与 TAKO_MODEL 完全相同的解析链：
      //    ResolveModelWithRef(r.env, ref) → 查出厂商、Key、baseURL、协议
      //    build(ctx, resolved)            → 按协议建对应的 HTTP 客户端（openai / claude）
      resolved, err := ResolveModelWithRef(r.env, ref)
      if err != nil {
          return nil, err // 没配这家的 Key / 厂商不在目录 / ref 格式错 → 上层拒载该规格
      }
      chatModel, err := build(ctx, resolved)
      if err != nil {
          return nil, err
      }

      // 3. 塞进缓存。下次同样的 ref 就不用再走解析 + 建客户端了。
      r.mu.Lock()
      defer r.mu.Unlock()
      if m, ok := r.cache[ref]; ok { // double-check：并发下可能别人刚好塞了
          return m, nil
      }
      r.cache[ref] = &ChatModel{Chat: chatModel, Protocol: resolved.Provider.Protocol, Ref: resolved.Ref}
      return r.cache[ref], nil
  }
  ```

  几个**有意这样设计**、不按直觉改的点（别看到进程内就想搬去共享存储）：

  - 「多实例」= 同一张缓存表里可以同时有 deepseek 客户端、claude 客户端、ollama 客户端——只要你在规格里写了不同 ref，第一次用到时就各建各的，互不干扰。
  - 「晚解析」= 进程启动时 ModelRegistry.cache 是空的。你写了 20 条子代理规格，但只有 3 个被真正装载用到 → 只建 3 个客户端。剩下 17 个永远不会浪费内存去建。
  - **解析链同源**：写法三里那个「ResolveModel + build」和主 worker 启动时 `config.Open()` 调的是**同两个函数**（只是 `ResolveModel` 会抽一个通用版 `ResolveModelWithRef`，主 worker 的 `TAKO_MODEL` 也是拿这个通用版喂环境变量），保证 `TAKO_MODEL=deepseek/x` 和 md 里 `model: deepseek/x` 行为 100% 一致——不会出现「主 worker 能调通、子代理调不通」这种同一个 ref 两种行为的诡异 bug。

- **为什么 ModelRegistry 放「进程内存」，不放 PG/Redis 共享存储？（一张表说清楚判断依据）**

  判断原则来自仓库级硬规则 [AGENTS.md](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/AGENTS.md)：「凡是要长期存在的状态，先问『多副本下它在哪』」——但不是所有东西都叫「长期状态」。四种东西对比一下：

  | 东西 | 本质是什么 | 需要跨副本一致吗？ | 放哪里（v6 现状） | 为什么这么放 |
  |---|---|---|---|---|
  | 会话「单 run 跑着」的闸 | 互斥锁：两副本同时抢同一会话会出两份 run、写乱帧序 | ✅ 必须全集群唯一 | PG `runs` 表部分唯一索引（插入失败本身就是 409） | 共享可变状态 = 必须放共享存储。进程内 `chan` 或 mutex 一上多副本就废 |
  | runlive 帧流 | 生成中帧的实时队列：客户端不管打哪个副本的 frames API 都得看到同一批帧、同一顺序 | ✅ 必须全集群同序 | Redis `tk:runlive:{runId}` LIST（TTL 300s） | 时序队列 = 必须放共享。各副本各写各的，客户端轮询就丢帧乱序了 |
  | runlog 历史行 | 会话的定稿历史：用户关了浏览器再开，历史必须还在 | ✅ 必须持久、不丢 | PG `runlog_entries` 表 | 持久化数据 = 必须放共享。进程内存里一重启就没了 |
  | **ChatModel 客户端实例** | 一个无状态的 HTTP 请求发件器：装着 URL + Key + 协议 + 模型 ID，**不记任何对话**，同样的 ref 建 100 个和建 1 个行为字节级一样 | ❌ **不需要一致** | **进程内存（本进程的 ModelRegistry）** | 它是「连接句柄」，不是「数据」：<br>1. 跨进程传不了 HTTP client（里面有连接池、goroutine 私域），物理上就没法塞 Redis<br>2. 多副本各建一份成本可以忽略：扫几十条环境变量（μs 级）+ 分配几个 struct，真金白银的成本是后面几百 ms 的 LLM 请求本身<br>3. 同 ref 解析结果必然一致：所有副本读的同一套环境变量 + 同一套厂商目录，不存在「A 副本解析成功、B 副本解析失败」的可能（除非滚动部署时半更新，那是部署纪律的锅）<br>4. 缓存丢了也没关系：进程一重启、ModelRegistry 空了，下次 `Get` 再建就行，不丢任何业务数据——缓存只是「省一次解析」，不是真源 |

  总结成一句话：**数据（要持久、要跨副本一致的）放共享存储；连接句柄（用了就扔、重建成本极低的）放进程内。** ChatModel 客户端是连接句柄，不是数据。

- **Model message = 进程内**：AgentTool 每次调用起独立 runner（独立 compose state），跑完即弃，
  只有最终文本作为 tool result 回主 run。崩溃即丢，符合「run 死、会话活」语义。
- **runlog 落消息粒度行**：委派 tool_call / 子代理动作行 / 子代理终稿，`agent` 列非空、`model` 列 = 该 agent 生效 ref（PG 落库随 T3-2）；
  历史组装按 root 过滤（`event.AgentName == root`——不过滤会把子代理内部 tool 消息混进主历史）。
- **runlive 帧流**：子代理帧与主 agent 帧进同一归一层、同一 LIST，每帧带 `agent` 与 `model`（生效 ref）。
- **粒度裁定（2026-10-08）**：runlog 记消息粒度、不记帧——主 agent 的 delta 本就不落历史表，
  子代理同构：**delta 帧只进 runlive，不落 runlog**。
- **并行**：同一 run 内多 goroutine 各跑独立 state，帧事件全经主 run 的单一 writer（归一层）落库，
  seq 串行分配——**并行不改表**，无共享可变状态。
- **嵌套**：默认禁止（子代理规格不挂委派工具，pi 同款）；开放时 max=1 且深度持久化防 resume 绕过
  （dsh 默认）——随用例再立。
- **规格的工具面字段（`tools` 2026-10-09 定为三态；2026-10-16 加 `exclude_tools` 成四态，`doc/26`）**：
  **不写 → 继承父池**（父池已摘掉委派工具 = 禁嵌套；pi/harness 同款）/
  **白名单**（按名从父池映射）/ **`[]` 空集**（「不写 vs 写空」用 nil / 空 slice 分开表达）/
  **`exclude_tools: [名字…]` → 继承父池减去列出的名字**（新字段，与 `tools:` **互斥**：同写整条拒载；
  排除一个父池没有的名字也拒载；`exclude_tools: []` 视同不写。结果序是池序、元素是父池同一批对象。
  四态表与操作定义只在 `doc/26` §3 一份，这里不复述）。
  「+search」= 保留名 **`tool_search`** 显式进列表才挂（用户 2026-10-08 裁「子代理挂 search，当然要」；
  2026-10-20 已拍：继承档——不写 / `exclude_tools`——不自动加，搜索面是能力面，不悄悄多一把工具）——
  搜索范围 = **自己的池**（统一规则见 §5）。分级落地后子代理池同样分 direct/deferred，池内工具不再全声明。
- **预算面（2026-10-09 三裁）**——**取消是控制面的事（abort 级联），不是时间面的事**（用户裁，并指 Google ADK 同立场）：
  - **并发**：上限 = **配置项（默认 8）**（选择归 human 写配置、代码只做机械件）；pi 为硬编码常量（单批 8 / 同时 4），tako 无「单批」概念，只设「同时」一个旋钮。
  - **超时**：**不设总时限**、不另立计时器；要掐走将来的「取消」面（abort 级联；与 T1 遗留「abandon 不断流」是同一个待做面）。pi 实读（2026-10-09）：两套子代理实现均无计时器，唯一 kill 路径都是 abort 传播；时间上限只加在有界操作上（per-request / 单条命令）。
  - **回传裁剪**：终稿回传超 **50 KB 字节**即截（UTF-8 安全 + omitted 提示，照 pi）；全量在子代理自己的 runlog 行（tako 无 pi 的 tool details 通道）。
- **T3 落地实施口径（2026-10-11 落地；验收脚本 = 19v6-04 用例 7）**：
  - **子代理终稿行（runlog assistant 行）**= 该 agent **全部轮次正文累计**（工具轮前导语 + 末轮正文）——遇它无 `tool_calls` 的末轮落一行（agent=名、model=其生效 ref）并清空暂存（同 agent 二次委派不串轮）；root 终稿 = Σ root turn 正文（收口锚不变）。
  - **回传给父 run 的文本** = AgentTool 的返回值 = 子代理**末轮消息正文**（多轮时为其末段：不含工具轮前导语）；**先裁后落**——mh tool 行 / runlog 委派 `tool_result` 行 / 帧侧 `tool_result.output` 三处是同一份**已裁剪文本**（逐字节相等）；全量只在子代理终稿行。
  - **截断尾缀原文**（delegationTool，代码 `agents.go`；照 pi）：
    `[Output truncated: %d bytes omitted. Full output preserved in subagent runlog.]`，N = 总字节 − 保留字节；
    截点回退到 UTF-8 rune 边界（绝不劈开多字节字符，保留段最多比上限少 3 字节）；`cap ≤ 0` = 不裁剪。
    尾缀里的「subagent runlog」= 同一 log 端点上该 agent 的行（无新端点；客户端按行内 `agent` 字段跳转）。
  - **两个旋钮（main 旗标；选择归 human 写配置）**：`-subagent-concurrency`（默认 8；进程级信号量、全部规格共享；<1 = 启动即拒——0 = 所有委派永久等闸）+ `-subagent-return-cap`（默认 51200 B）；启动日志 `subagents: concurrency N, return cap M B`。
  - **随仓 seed 规格** `yk-tako-eino/agents/researcher.md`（开箱可跑 + 格式参照）：不写 `model` = 继承主实例那一档的活样例。`tools` 那一档自 2026-10-16 起改写成 `exclude_tools` 排除名单（`doc/26`：排除 `bash` 与写类四把，有效名单 11 项），不再是「什么都不写」的样例——**「两行都不写 = 整份继承父池」这一档今天没有出厂规格在用**，它仍是合法写法（`resolveToolPool` 第一态），但 `agents_test.go` 的 `TestShippedAgentSpecToolGuards` 要求出厂规格写 `tools:` / `exclude_tools:` 之一，有意继承要在测试的 `inheritOK` 里显式表态。
- 挂账：continuable/后台子代理（dsh send_message/jobs）、出进程 provider（acp/codex/claude-code）、
  **子代理目录的运行时安装**（Octop catalog/install）、**子代理运行中途改自己的工具集**
  （Eino `BeforeAgent` 可改、用例未逼出）——后两条 2026-10-08 用户裁「先别搞，记下来」。

## 5. 工具目录与 tool_search

- **施工状态（2026-10-09 裁触发式；2026-10-20 开工并当日落地）**：原裁「等 MCP 工具 loop 进来（工具多到
  撑上下文）或子代理库长到几十条再动工」；MCP client（doc/33）已落地、工具规模信号成立，用户
  2026-10-20 裁「**一步到位把 T5 也做了**」——T4（分级 + tool_search）与 T5（BM25 排序）**同批施工**，
  **BM25 是首版排序器，不先做名称/描述直出再替换**。分离原则不变：
  「**可执行集合 ⊇ 声明集合 ⊇ 可检索集合**」；T3 期「一个源、一个装载点、一个声明状态（全声明）」到此让位给分级。
  施工与验收结论见本节尾条。
- **分级 = 启用工具全注册 + 声明过滤**：装配时决定放不放进池——**不放 = 禁用**，不进 tools node，
  既不可执行也不可检索；放进池的工具全部注册进 tools node。每轮模型调用前，
  `BeforeModelRewriteState` 用本 run 保存的**完整目录**重建 `state.ToolInfos` 决定模型看见什么——
  **direct** = 常声明 / **deferred** = 目录层，整条 ToolInfo（名字、描述、参数 schema）都不进声明面，
  `tool_search` 激活后才声明。不能对当前 `state.ToolInfos` 做减法：
  首轮过滤后 deferred 项已不在其中，减法的源会丢；完整目录必须另存（run-local）。
  **不设 hidden 档（2026-10-20 裁）**：tako 的池每 run 重建，禁用就是装配时不 append，不存在 pi
  「注册表撤不掉、只能重注册成 hidden」的问题。把 hidden 定义成「永不声明」也不成立——工具仍在
  node 里，而 Eino tools node 按全量工具表按名分发（名字不在节点才报 `tool X not found`，v0.9.21
  `compose/tool_node.go` 实读），执行面并没有关，模型猜中名字仍可能调起。今日没有「只给代码调、
  不给模型调」的消费者（三条来源都在装配时不放；子代理池从入口快照按名收窄、只减不增，§4），
  所以不留这一档；将来真出现该需求，按新需求重新立档并同时设执行闸。
- **tool_search（T4/T5 合批，2026-10-20 用户裁）**：模型检索目录、BM25 排序、命中即激活（**同一 run** 的
  下一次模型调用即声明）。首版就是 pi 的 Go 移植——camelCase 分词 + 朴素词干 + 停用词；Okapi BM25
  （k1=1.2、b=0.75）；文档 = 名字 + 下划线拆词 + 描述 + 参数 schema 的描述与属性名（MCP 工具再加
  server 短名）；`query` 必填、`limit` 缺省 8、空 query / 非正整数 limit 当场报错；无命中回
  `No matching tools found.`。**直出版不落地**。
- **mh 与激活集分家（2026-10-20 定）**：mh 只记 tool_search 的 assistant 调用行与 tool 结果文本
  （`Loaded N tools...`），**不记工具 schema、不记「已激活」**。激活集是 run 状态；每轮模型看见哪些工具
  由 `BeforeModelRewriteState` 用完整目录按「direct + 激活集」**重建** `state.ToolInfos` 决定，
  同一 run 内由 run-local 状态延续。新 run 读回
  的历史里只有那句结果文本，没有声明——**「记录了」不等于「模型这一轮看得见」**。
- **激活集寿命（2026-10-20 已拍：只活一场 run）**：新 run 从零激活（tool_search 本身是 direct，模型可重检）；
  崩溃续跑同一场 run 时同样从零激活（doc/30 账本不存激活集），模型凭历史里上次的回执与调用参数重检。
  回执固定写 `Loaded N tools for this run...`——不照抄 pi 的 `available from your next call`（跨 run 读回
  历史时会变成半句假话）。若产品将来要「加载过就一直可用」，必须把激活集结构化持久化（多副本不能靠
  进程内状态），从 mh 文本重放会被折叠折走、不可靠——那是另立方案。
- **首版分级来源（2026-10-20 已拍：按来源的机械规则）**：`mcp__` 前缀 = deferred（工具规模增长源）；
  一手工具 + 子代理 = direct。不新增配置面；单个 MCP 工具若将来要固定 direct，再加 server 级覆盖。
- **落地与验收（2026-10-20）**：A1 `ad1ab6b`（`toolsearch.go`：分词/BM25/`tool_search`/run-local box，
  worker 每 run 挂载）；A2 `a190f4d`（子代理保留名不进池、只有显式 `tools:` 才挂自己的 handler，
  租户 `checkSubagentBounds` 预检给保留名开例外）。A3 纯验收：同会话第二场 run `declared=21 active=[]`、
  崩溃续跑同样从零激活（账本无 schema），回执「for this run」与真实寿命一致。A4 回归：
  单测补 schema 文本入文档、同分稳定序与 limit 截断、缺省 8 与越界报错、已激活不重复返回；
  活体重跑 A1（`catalog=35 deferred=14`，`declared` 21→29）、A2 正/负路径（子代理 `catalog=2`、
  继承档名单无 `tool_search`）、无 mcp.json（`catalog=19 deferred=0`，`tool_search` 回
  `No matching tools found.`）；`server.go`/`types.go`/`redisstore.go` 自 `ad60e49` 到 `a190f4d`
  零 diff，四个 API 字节不变。
- **红线**（pi/dsh 同款收敛）：模型只能**装配目录内工具**，不能改 harness 本身。
- **统一搜索规则（2026-10-08）**：任何 agent（主或子）的 tool_search 只搜**自己 tools node 里注册的
  deferred 项**——搜索范围 = 可执行范围的子集，越界激活物理上不存在；主 agent 的 node 不含子代理
  专属工具，互相不可见。一条规则覆盖主从（子代理挂 search 见 §4 规格字段）。
- Eino 原生 `WithDeferredTools` + `WithToolSearchTool`（provider 托管的 hosted mode）——
  **支持度已查证（2026-10-08，矩阵与机制见 19v6-06 §4–5）**：eino-ext claude 组件 ✅ / openai 组件 ❌；
  Anthropic·OpenAI·DeepSeek·Kimi 各形态 ✅，Gemini/Qwen/豆包/GLM ❌。
  **2026-10-21 用户裁定：client 过滤（本条）为终态底座，hosted mode 不接**（理由三条见 `19v6-06` §6：
  覆盖面对不上 / 缓存损失有界 / 对 provider 零要求）。`19v6-06` 降为纯背景篇，不作施工面、不进待办。

## 6. 表新增汇总

| 表 | 新增 | 落地拍 | 说明 |
|---|---|---|---|
| `run_model_messages` | `tool_calls` TEXT、`tool_call_id` TEXT | T2 ✅ | OpenAI assistant/tool 形状；§8.2 已预告 |
| `runlog_entries` | `agent` TEXT（空=root）、`payload` TEXT（JSON） | T2 ✅（两列，2026-10-08）/ T3（agent 非空） | payload 单列 TEXT：结构化字段种类差异大、稀疏列展不开 tool_calls 数组；「零 JSONB」原裁针对消息表（§8.6 收口） |
| `runlog_entries` | `model` TEXT（空 = 非模型产物） | T3-2（2026-10-10 登记） | 与帧同义的生效 ref（`厂商/模型`）：`user` 行空 / root 行 = 主 ref / 子代理行 = 该规格生效 ref——md 改 `model` 的持久验证面（帧是 TTL 的实时视图，本列是永久真源） |
| `sessions` / `runs` | 不动 | — | fork 的 `forked_from` 列随 T2 树状会话 loop 再加（§7） |

## 7. 挂账（本 loop 不做）

- **fork（pi 会话树）**：用户裁「后面再做」（2026-10-08）。策略已备 = **copy-on-write**：新 session 物化源分支投影视图（只带已完成 run 边界，runs 表的 complete 闸天然切点）；
  真树（parent 链共享历史）撞「worker 热读单表无 JOIN」已裁约束，不取。
- **guard middleware**（allow/deny per agent、拒绝归一为 isError 结果、HITL ask 走 interrupt）：
  本 loop 只在 Handler 链留口，独立小拍。
- **「压 run 中间」**（C 系列 10.3-3 遗留）：run 内多段调用落地后回 C 系列议。
- ~~Eino 原生 deferred tools 的 provider 支持度查证~~ **已查证（2026-10-08）**，
  矩阵与机制见 `19v6-06` §4–5；默认底座维持 client 过滤。

## 8. 与既有篇的关系

- **19v6-01**：四个端点字节真源。本篇 §3 的帧扩展在 T2 同拍修订它（先改 doc 再动码）。
- **19v6-03**：存储形态真源。本篇 §6 的表新增是它的增量。
- **index 注意事项全部继续适用**：纯拉模型、拉取有界、闸在共享存储、runlog 只增不删——
  tool loop 不引入任何例外。
