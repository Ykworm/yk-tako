# 19v6-06 · 工具分级与 deferred tools（机制篇）

> 本篇是 doc/19-v6 文档集的 **deferred tools 机制篇**：工具分级为什么存在、前缀缓存的失效力学、
> client mode 与 hosted mode 的完整链路、各厂商支持矩阵（2026-10-08 实证）、v6 的升级路。
> 分级的 v6 落点（全注册 + `BeforeModelRewriteState` 声明过滤、三档定义）见 `19v6-05` §5——本篇不重复裁定，
> 只给机制背景与 provider 原生通道的对照。**本篇不做契约承诺**，所有 provider 原生内容均为「升级路素材」。

## 0. 一句话

工具分级解决「工具一多，声明成本（token / 注意力 / 缓存）爆炸」；**deferred tools 是各厂商对同一问题的
协议级回答——目前无标准名、无标准协议，形态碎片化**，这正是 v6 把客户端过滤定为默认底座的原因。

## 1. 为什么需要分级

- 模型在协议上只能调用**当前请求 `tools` 数组里声明了**的工具；声明 = 占用前缀 token。
- 上百个工具的全量 schema 既烧 token 又稀释注意力 → 只常声明一小撮（direct），
  其余进目录（deferred：模型经检索按需激活）或彻底藏起（hidden：代码内部/子代理用）。
- v6 三档 = pi `ToolExposure` 五档（direct / model-only / codemode / deferred / hidden）的收敛。

## 2. 缓存力学（一切的根源）

- **无状态协议**：服务端不记会话，每轮客户端全量重发 history → provider 按「前缀哈希」做 prompt caching
  （Anthropic 实证：写缓存 = 断点处前缀的哈希；命中要求逐字节相同）。
- **前缀层级固定**：`tools → system → messages`。改 tools 段 = 三层缓存全灭
  （Anthropic 失效表原话：*"Modifying tool definitions invalidates the entire cache"*）。
- KV 按位置缓存：前缀任何位置变更，**其后所有位置**的 K/V 作废；tools 排最前，所以一改全灭。
- **追加不破坏缓存**：tool result 作为新消息追加在尾部，是缓存友好动作——多轮对话本身就是
  「恒定前缀 + 尾部追加」结构，agent loop 天然适配。**破坏缓存的是「改写已存在的前缀内容」，
  不是「出现 tool result」**（ tool_use / tool result 块在 Anthropic 均可被缓存）。
- 缓存价值：读 ≈ 0.1x 价、TTFT 大降；一 run 多轮的 tool loop 是最依赖缓存的负载——声明集稳不稳是真金白银。

## 3. client mode = v6 现在（手写声明过滤）

- 全工具注册进 tools node（永远可执行，注册 ≠ 声明）；
  `BeforeModelRewriteState` 每轮筛 `state.ToolInfos` 决定本轮发给模型的 `tools` 数组（eino 钩子，provider 零感知）。
- `tool_search` 是**普通工具**：模型调用 → 客户端执行检索 → 结果落消息历史 →
  钩子扫历史把命中工具的 ToolInfo 补回声明集 → 下一轮模型才「看见」。
- **代价**：激活那轮 tools 数组在中段变更 → 前缀哈希作废 → 缓存失效（单调收敛：搜出即常驻，
  之后稳定，仅「发现期」几次 miss）。
- **优点**：对 provider 零要求——任何模型、任何兼容端点都能跑。T4（目录直出）/ T5（BM25 排序器）复刻的就是检索层。

## 4. hosted mode = provider 原生 deferred tools

### 4.1 名字（无标准）

| 出处 | 叫法 |
|---|---|
| Anthropic | deferred tool loading + Tool Search（`tool_search_tool_bm25_20251119` / regex 变体） |
| OpenAI | tool search（Responses API，`{"type":"tool_search"}` + `defer_loading`） |
| eino 抽象 | `WithDeferredTools` / `WithToolSearchTool` |
| pi | ToolExposure 的 deferred 档 + tool_search 扩展 |

### 4.2 完整链路（请求携带式 hosted，OpenAI 文档 + eino-ext 源码实证）

| # | 步骤 | 执行者 | 输入 | 输出 | 作用 |
|---|---|---|---|---|---|
| 0 | 目录组装 | 客户端（eino wrapper 每轮附加 `WithDeferredTools`） | 应用的工具注册表 | 请求 tools 数组：全量 schema + `defer_loading` 标记 + `{type:"tool_search"}` | provider 本轮知道库存 |
| 1 | 扣留暂存 | provider 网关 | tools 数组 | 暂存区（deferred schema 隔离，**不进 KV key**） | 目录保管在上下文外 |
| 2 | 前缀预填充 | provider | 摘要层 + 常声明工具 + system + messages | 前缀 KV；模型开局视野 | 建立恒定可缓存前缀 |
| 3 | 检索词生成 | 模型 | 任务上下文 + 摘要层 | query / paths（hosted 下为内部信号） | 模型唯一发言点：搜什么 |
| 4 | 索引检索 | provider 检索索引 | query + 暂存区 | 命中的工具 schema | BM25/regex 机械匹配（无任何智能成分） |
| 5 | 末尾注入 | provider 基础设施 | 命中 schema | 追加在**上下文末尾**（只增区） | 模型看见 schema 且前缀不动 |
| 6 | 工具调用生成 | 模型 | 注入的 schema | 普通 function_call / tool_use | 与常声明工具同构 |
| 7 | 工具执行 | 客户端 | function_call | tool result（下轮 messages 追加） | 客户端常规职责，零特殊逻辑 |
| 8 | 事件记录 | provider → 客户端 | 步骤 3–5 过程 | 响应里的 `tool_search_call` / `tool_search_output` item | 可观测；多轮回放保持状态 |

循环规则：同轮可 3→6 多遍（需要第二个工具就再搜）；跨轮重复 0–2 时前缀字节相同 → 缓存命中，注入区累积在尾部。

### 4.3 关键机制点

- **`defer_loading` 的实质**：全量 schema 仍随每轮请求上行（占带宽、不占上下文），但被剥出模型工作
  上下文、不进 KV key——「provider 本轮替你保管目录」。
- **摘要/细节二分**：模型开局只见摘要层（名+描述，常驻前缀、供生成检索词）；被扣的是参数 schema。
  加载粒度 = 检索路径命中的子树（如搜 `paths:["crm"]` → 整个 namespace 进来），模型不能逐工具勾选，
  不满意只能再搜。
- **注入位置是护城河**：*"All tools are loaded at the end of the model's context window"*（OpenAI 原文）——
  变更永远落在只增不改的尾部，所以缓存保活。client mode 永远做不到这点（只能改 tools 段中段）。
- **`tool_search` 用 `type` 不用 `name`**：tools 数组是联合类型（function / namespace / tool_search /
  web_search…），`type` 是判别字段；`name` 只属于模型按名调用的用户函数。
- **变体**：
  - *client-executed*（OpenAI）：模型发 `tool_search_call`（execution:"client"），客户端执行检索、
    下轮把 `tool_search_output` 塞进 input——检索位置灵活，但加载仍走尾部注入（协议化的 client mode）。
  - *MCP 自动发现*（OpenAI Agents API）：provider 自己连远程 MCP server 做 tools/list，目录不进请求。
- **铁律不变**：工具定义的控制权在应用/MCP server 侧，模型不能给自己造工具（所有模式一致）。

### 4.4 缓存友好的三条路线（同一原则的实现谱系）

原则：**变更只能落在「只增不改」的尾部，或干脆不落在模型上下文里。**

| 路线 | 工具变更落在哪 | 前缀旧段 | 代表 |
|---|---|---|---|
| 前缀外通道 | provider 暂存区（不进 KV key） | 原样 | `defer_loading`（Anthropic / OpenAI / DeepSeek） |
| 尾部追加 | messages 尾部 | 原样 | Anthropic `tool_addition`（inline-tools beta）；OpenAI `additional_tools`；Kimi `messages[].tools` |
| 认损 | tools 段中段 | **被破坏**（发现期） | client 过滤 = v6 现在 |

## 5. 厂商支持矩阵（2026-10-08 实证）

| 厂商 | 支持 | 形态 | eino-ext 组件 |
|---|---|---|---|
| Anthropic | ✅ | `defer_loading` + 内置 BM25/Regex tool search + inline-tools beta | ✅ 已接（claude.go 翻译 defer_loading 与 tool search 工具） |
| OpenAI | ✅（Responses API，gpt-5.4+） | hosted + client-executed 两式 + namespaces + `additional_tools` | ❌ **组件未翻译**（API 有、组件没有） |
| DeepSeek | ✅ | Messages API 认 `defer_loading`（服务端 400 校验："at least one tool must have defer_loading=false"） | — |
| Kimi | ✅（私有变体） | 消息级工具声明 `messages[].tools`：已加载工具从顶层 tools[] 剥离、schema 走消息通道 | — |
| Gemini | ❌ | 仅标准 function calling（feature request 仍开） | — |
| Qwen / 豆包 / GLM | ❌ 未见 | 仅标准 function calling（托管 web_search 是厂商自带工具，与用户工具 deferred 无关） | — |

要点：
- 「支持」本身也分裂：DeepSeek 认 Anthropic 式标记、Kimi 用自家消息通道、Anthropic 带服务端 BM25——
  **没有两家同协议**，升级路只能按家适配。
- eino 核心只定义 option，**翻译靠组件、可用性靠 provider 运行时校验**（eino 不做任何能力探测；
  eino-ext openai 组件静默忽略 option 即是例证）。
- 顺手的推论：deferred schema 不进 KV key → 原生模式下**改目录本身可能缓存中性**
  （与 client mode 的中段变更形成对照），待接入时以厂商文档最终确认。

## 6. v6 的裁定与升级路

- **默认底座 = client 过滤**（`19v6-05` §5）：碎片化格局下的最大公约数，不挑 provider。
- 升级路按家接：claude 经 eino-ext 已具备（切 `WithDeferredTools` 即可）；
  DeepSeek 认标记但服务端检索能力未证实；Kimi 需私有协议适配；OpenAI 要等 eino-ext 组件补齐。
- T4/T5（目录直出 + BM25）复刻的是检索层——即使将来切 hosted mode，这套客户端目录与匹配逻辑仍是
  client-executed 变体的执行体，不白做。
- **第一个真实负载已经到场**（2026-10-18 补）：MCP client（doc/33）落地后，MCP 工具以
  `mcp__短名__工具名` 直接进池走声明过滤——数量少时照旧；工具总数涨到声明成本可见的量级时，
  T4/T5 的目录代理以 MCP 工具为第一个真实负载开工（doc/33 §9 后置拍表）。

## 附 · 证据清单（全部 2026-10-08 实读）

- Anthropic prompt caching 文档：缓存层级（tools→system→messages）、失效表、tool result 可缓存、逐字节命中。
- OpenAI Tool search 文档：hosted / client-executed 全流程、`additional_tools`、尾注入缓存语义、gpt-5.4+ 限制。
- eino v0.9.21 源码：`components/model/option.go`（两 option）、`adk/handler.go`（`BeforeModelRewriteState` /
  `state.ToolInfos` 为 source of truth）、`adk/middlewares/dynamictool/toolsearch/toolsearch.go`（client 过滤
  参考实现 + `UseModelToolSearch` 开关）、`adk/wrappers.go`（每轮附加 option）。
- eino-ext 主干：`components/model/claude/claude.go`（defer_loading / BM25 工具翻译）、
  `components/model/openai`（无对应实现）、`examples/hosted_tool_search`（官方用法）。
- 厂商生态佐证：deepseek-ai/deepseek-harness#8683（defer_loading 400 校验原文）、
  MoonshotAI/kimi-code#3295（messages[].tools 私有通道）、googleapis/python-genai#2185（Gemini 未支持）。
