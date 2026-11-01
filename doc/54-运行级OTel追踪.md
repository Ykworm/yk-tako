# 54 · 运行级追踪：OTel GenAI 语义 + CozeLoop 上报

> 性质：**方案（已落地，2026-10-31）**。
> 状态：✅ A1–A9 全部施工并验收（2026-10-31）：`POST /api/dev/trace-check` 回 `exported:true`；真 run 产出根 `invoke_agent yktako` + `chat` / `execute_tool` / `tako.step.*` 子 span，引擎日志带 `traceId=`；`go test ./internal/runengine ./internal/observability` 全绿。台账在 `tmp/cozeloop/`（不入库）。
> 日期：2026-10-31。
> 代码落点：`yk-tako-eino/internal/observability/`（装配、内容序列化、开关）、`internal/runengine/{trace.go,trace_run.go}`（run 根 span / chat / 入口 / 探针 / 求和）、`worker.go`、`journal.go`、`recall.go`、`compact.go`、`memory.go`、`server.go`、`cmd/yktako/main.go`、`yk-tako-eino/.env`（内容开关，不入库）。
> 参照：OpenTelemetry GenAI 语义（`gen_ai.*`，semantic-conventions-genai）；CozeLoop 的 OTLP 接入点（`https://api.coze.cn/v1/loop/opentelemetry/v1/traces`，http/protobuf）。
> 与 doc/27 的关系：27 把**单次模型调用的账单**摆到 runlive 帧与 runlog 行；本篇把**整场 run 的结果**摆到外部观测面。27 是账本，54 是门牌——不改 27 的帧与行。

---

## 0. 为什么现在做这一层

### 0.1 一个具体的排查故事

今天要看一场 run 干得怎么样，人得打开页面翻帧、或者 `curl /api/runs/{id}/frames` 把几十上百帧拉下来，自己按 `eventOffset` 拼时间线：哪一枪模型慢、哪次工具等了三分钟、折叠发生在第几轮、最后为什么 error。

这在「一场 run」里勉强能用。跨 run 就断了：

- 某个租户连续十场 run 都在 2 秒内失败——没人知道，直到他投诉；
- 同一句提示词换模型后平均耗时翻倍——要靠人手动跑两次掐表；
- 线上出现一个慢 run，你想把它和昨天那批「同项目、同模型、同租户」的 run 放一起比——现在没有一个按字段可检索的集合。

CozeLoop（扣子罗盘）的 Trace 面正好是这样一个集合：一条 run 一条可检索记录，带起止时间、状态、模型、token 用量；点进去能看时间线。把它接上，上面三件事从「手工翻」变成「搜一下」。

### 0.2 为什么是 OTel GenAI，不是 CozeLoop 私有格式

CozeLoop 的 Go SDK 和 eino adapter 产出的是它自己的 span 模型：`span_type` 只有 agent / model / tool，业务属性是 `model_name`、`input_tokens`、`output_tokens`、`input`、`output` 这些自家 tag。用它接，字段名就绑死在 Loop 这一个后端；哪天换 Tempo、Jaeger、云厂商 APM，整套埋点要重写。

OpenTelemetry GenAI 语义约定（`gen_ai.*`）是这一层的公共语言：模型调用叫 `chat`、agent 调用叫 `invoke_agent`、token 叫 `gen_ai.usage.input_tokens`。按它写，Loop 只是**第一个消费方**，换后端不用改代码。

好消息是 Loop 支持 OTLP 接入（HTTP/protobuf），所以两条都要：**语义按 OTel GenAI 写，传输走 OTLP 发给 Loop**。

### 0.3 代价：记录是一棵逐步长出来的树

Loop 原生 adapter 能自动产出「每次模型调用、每次工具执行」的子 span；走 OTel 自建，这棵树要一拍一拍搭：

- 第一段（A1–A3）只有一条 run 级 `invoke_agent` span——时间线上是一个点，不是一棵树；
- A4 用模型包装层补每次模型调用的 `chat` 子 span（§2.1）；
- A5 用工具包装层补每次工具执行的 `execute_tool` 子 span；
- A6 再往上补一条 HTTP 入口 span（`POST /api/runs`），让 trace 从「服务收到请求」开始，而不是从 worker 起跑开始（§2.4）；
- A7–A9 补 run 内部的步骤 span：召回（`tako.step.recall` → `search_memory` / `retrieval`）、折叠（`tako.step.compact`）、准入/装历史/定稿（`tako.step.quota` / `workspace` / `history` / `finalize`）、后台记忆抽取（独立 trace 的 `tako.step.memory_extract`），回答「recall/compact 这些步骤各花了多久」（§2.6）。

换来的是：字段名与后端解耦；而且每一拍落下来都有独立价值——run 记录先能检索、对账、告警，子 span 再逐步把时间线补全。

---

## 1. 名词钉死

- **span（跨度）**：一段有开始时间、结束时间、一组属性的操作记录。OTel 里一次 trace 是一棵 span 树，根 span 代表整件事。**本篇要的「Run 记录」是这棵树里那条 `invoke_agent` span**——不是一行日志。第一段（A2）它就是根；A6 加上 HTTP 入口 span 后，根变成「服务收到 POST /api/runs」那条 span，run 记录挂在它下面（同一 traceId、同一次业务动作）。OTel GenAI 语义约定的主载体是 span，日志行（LogRecord）不是。
- **OTLP**：OpenTelemetry 的标准导出协议。SDK 把 span 编码成 protobuf，按 HTTP POST 发给采集端。Loop 的接入点是 `https://api.coze.cn/v1/loop/opentelemetry/v1/traces`，请求头两个：`cozeloop-workspace-id: <空间 ID>`、`authorization: Bearer <访问令牌>`。
- **`gen_ai.*` 属性**：OTel GenAI 语义约定规定的属性名。例如 `gen_ai.operation.name=invoke_agent`、`gen_ai.request.model=deepseek-flash`、`gen_ai.usage.input_tokens=1234`。
- **`cozeloop.span_type`**：Loop 用来给自定义 span 分类的属性。不写它，Loop 会把 span 归成不可分类；本篇写 `agent`，A1 实跑若显示 CUSTOM 就改回 `custom`（以观测结果为准）。
- **步骤 span**：run 内部一段非 GenAI 操作（召回、折叠、准入、定稿、后台抽取）的 INTERNAL span，用 `tako.step.*` 命名、父 span 是 run 根 span。它不是 `chat`/`execute_tool` 那种语义约定操作，只用来回答「这一段花了多久」（§2.6）。
- **内容开关**：`OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT`，规范定义的 Opt-In 开关。`SPAN_ONLY` = 内容写进 span 属性；`NO_CONTENT` = 不写。它只控制内容是否存在，不影响 span 的存在与结构（§2.2）。

---

## 2. Run 级记录的形状

一条名为 `invoke_agent yktako` 的 span。A6 之前它是 trace 的根；A6 之后它是 HTTP 入口 span 的子 span，trace 的根变成入口 span（§2.4）。

| 项 | 值 |
|---|---|
| span 名 | `invoke_agent yktako`（语义约定：`{gen_ai.operation.name} {gen_ai.agent.name}`） |
| span kind | `CLIENT`（语义约定对 invoke_agent 的规定） |
| resource | `service.name=yktako`、`service.instance.id=<hostname>:<pid>`（多副本下区分执行者） |
| 状态 | 正常收尾 `OK`；`error` 终态 `ERROR`；用户停止 `Unset`（不是失败也不是成功） |

`gen_ai.operation.name` 先单独解释，因为它是整条记录的「类型标签」：语义约定给 GenAI 操作定义了一张固定词表——`chat` 是一次模型对话补全、`execute_tool` 是一次工具执行、`invoke_agent` 是一次 agent 调用（agent 内部可能跑很多轮模型和工具）、`create_agent` 是装配一个 agent。接收端（Loop、Tempo 等都一样）靠这个属性决定把记录画成哪一类。我们这条 span 代表**整场 run 的一次 agent 调用**，所以取 `invoke_agent`；它和 span 名里的前半段同源（`{gen_ai.operation.name} {gen_ai.agent.name}`）。

属性分两批写：起手能确定的，收尾才能确定的。属性名分三类，别混：

1. **语义约定原名**（`gen_ai.*`、`error.type`）：不是我们自造，逐字取自 OpenTelemetry 的 GenAI 语义约定 registry（2026-10-31 拉取；这些 `gen_ai.*` 目前都标 Development，`error.type` 是 Stable）。`gen_ai.operation.name`、`gen_ai.agent.name`、`gen_ai.provider.name`、`gen_ai.request.model`、`gen_ai.conversation.id`、`gen_ai.usage.input_tokens`、`gen_ai.usage.output_tokens`、`gen_ai.conversation.compacted` 都在 registry 里有原文定义，不是我们起的名字。
2. **tako 自建**（`tako.*`）：语义约定没有对应字段，按 OTel 的命名规则用自有命名空间补。接收端不认识它们，只作为附加属性可见；哪天语义约定出现了同义字段，优先改用规范名。
3. **CozeLoop 私有**（`cozeloop.span_type`）：不属于 OTel 规范，只服务 Loop 的分类显示。

`service.name` / `service.instance.id` 来自 OTel 的资源语义约定（不是 GenAI 命名空间）。

| 属性 | 取值 | 含义（接收端拿它干什么） | 来源 | 何时写 |
|---|---|---|---|---|
| `gen_ai.operation.name` | `invoke_agent` | 这条记录代表哪种 GenAI 操作；`invoke_agent` = 一次 agent 调用（区别于模型调用的 `chat`、工具执行的 `execute_tool`） | 常量 | 起手 |
| `gen_ai.agent.name` | `yktako` | 被调用的 agent 名；run 级只标主代理，子代理的模型调用在 A4 的 `chat` 子 span 里出现 | 常量（与 `newRunAgent` 的 `Name` 同值） | 起手 |
| `gen_ai.provider.name` | 模型 ref 的前缀，如 `deepseek` | 模型厂商，接收端按它分组统计 | 生效 ref（`rootRef`）按 `/` 切开 | 起手 |
| `gen_ai.request.model` | 模型 ref 的后缀，如 `deepseek-flash` | 请求的模型名；语义约定要求厂商放 `provider.name`，这里只放模型 | 同上 | 起手 |
| `gen_ai.conversation.id` | `sessionId` | 会话 ID，Loop 用它把同一会话的多场 run 串在一起 | run 参数 | 起手 |
| `tako.run.id` | `runId` | tako 的 run 主键，从 Loop 跳回库里查帧与日志 | run 参数 | 起手 |
| `tako.run.epoch` | 租约代数（1 = 首发，>1 = 续跑） | 区分同一 run 的第几次执行；续跑是另一条 trace | run 参数 | 起手 |
| `tako.model.ref` | 完整生效 ref，如 `deepseek/deepseek-flash` | tako 内部原样的模型标识；拆成 provider/model 两列会丢「这就是 tako 配置里那一串」的对应关系 | 同上 | 起手 |
| `tako.tenant.id` | 租户 ID | 多租户下按租户聚合 run（失败率、耗时、用量） | `principalFromCtx` | 起手 |
| `tako.workspace.id` | 项目 ID | 项目维度的业务归组：用户换了会话继续同一件事，仍能按项目找回（§2.5）；为空不写 | `store.SessionWorkspaceID(ctx, sessionId)` | 起手 |
| `gen_ai.usage.input_tokens` | Σ `promptTokens` | 本 run 输入 token 总数（主代理 + 子代理），用于成本与用量看板 | `turn` 里全部带账单的 assistant 条目（含子代理） | 收尾 |
| `gen_ai.usage.output_tokens` | Σ `completionTokens` | 本 run 输出 token 总数 | 同上 | 收尾 |
| `tako.usage.scope` | `conversation` | 钉死这两个 token 的口径 = 对话账本（= usage 帧/runlog 的 `turn` 求和），不含折叠摘要与记忆抽取；接收端对账时按这个口径（§2.3） | 常量 | 起手 |
| `gen_ai.input.messages` | JSON 字符串（semconv 消息数组） | 本轮用户原文（召回注入前，业务输入）；开关关时不写 | run 级 trace 容器里的本轮 user 行 | 内容开关开时，召回前 |
| `gen_ai.output.messages` | JSON 字符串（semconv 消息数组） | 本轮 root agent 的 assistant 产出（含 tool_call parts）；子代理条目不进根输出 | `turn` 里 `Agent==""` 的 assistant 条目 | 内容开关开时，收尾 |
| `gen_ai.system_instructions` | JSON 字符串（`[{"type":"text",...}]`） | 本轮系统提示词；开关关时不写 | `w.instruction(ctx)` | 内容开关开时，解析后 |
| `tako.content.truncated` | `true` | 内容属性被体积帽截断过；接收端据此知道内容不完整 | 内容序列化 helper | 写内容时 |
| `gen_ai.conversation.compacted` | `true` | 本 run 是否折过历史；语义约定要求只在确定折过时写 `true`，**不写 `false`** | 本 run 产生过折叠产物（`foldBox.stagedList()` 非空） | 收尾 |
| `error.type` | `run_error` | 失败分类，接收端按它做错误分组；本篇只有这一个粗分类，拿得到更细的错误码再细分 | `status=error` 时写 | 收尾 |
| `tako.run.status` | `complete` / `error` / `stopped` | 最终态；用户停止由停止端点翻库，所以要从库里回读，不能只信本地变量 | 收尾时读 `runs.status` | 收尾 |
| `tako.run.turns` | assistant 条目数 | 这场 run 模型回了几轮，用于识别「话多/空转」 | `turn` | 收尾 |
| `tako.run.tool_calls` | 模型请求的工具调用数 | 这场 run 调了几次工具 | `turn` 里 assistant 条目的 `ToolCalls` 长度求和 | 收尾 |
| `tako.run.frames` | 本 run 帧数 | 本 run 往 runlive 写了多少帧 | `foldBox.frameCount()` | 收尾 |
| `tako.run.start_delay_ms` | 毫秒数（≥0） | 「服务收到请求 → run 起跑」的延迟，把入口 span 与 run span 之间那段空隙变成一个可检索的数（§2.4） | A6 随 ctx 带入的入口起手时刻 | 起手（A6 起） |
| `cozeloop.span_type` | `agent` | Loop 私有的归类标签，决定这条 span 在时间线上显示成 AGENT 类型 | 常量 | 起手 |

### 2.2 内容属性：默认开、可关（PII 开关）

**决定**：内容属性写，但由标准开关控制；本仓库 `.env` 默认打开。

开关不自己起名，用 OTel GenAI 规范定义的 `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT`（Content capture configuration）：

| 值 | 行为 |
|---|---|
| `NO_CONTENT`（规范默认） | 不写任何内容属性 |
| `SPAN_ONLY` | 只在 span 上写内容；本篇用它（`.env` 已写 `SPAN_ONLY`，等价于你说的「默认 true」） |
| `SPAN_AND_EVENT` | 规范允许；tako 不产出内容事件，退化为 `SPAN_ONLY` |
| `EVENT_ONLY` | 规范允许；tako 不产出内容事件，等于关闭 |

未配置或非法值按规范回退 `NO_CONTENT`。开关只在 `internal/observability` 解析一次，业务代码只问 `ContentCaptureEnabled()`，不在各处重复读 env。

**写哪些、写在哪：**

| span | 属性 | 内容 |
|---|---|---|
| run 根 span | `gen_ai.input.messages` | 本轮用户原文（召回注入前，业务输入） |
| run 根 span | `gen_ai.output.messages` | 本轮 root agent 的 assistant 产出（含 tool_call parts） |
| run 根 span | `gen_ai.system_instructions` | 本轮系统提示词 |
| `chat` 子 span（A4） | 同上三个 | 该次调用真正的 wire 输入/输出；system 单独拆出 |
| `execute_tool`（A5） | `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result` | 工具入参与结果 |
| `search_memory`（A7） | `gen_ai.memory.query.text` / `gen_ai.memory.records` | 召回查询与命中的记忆条目 |
| `retrieval`（A7） | `gen_ai.retrieval.query.text` / `gen_ai.retrieval.documents` | 召回查询与命中的历史对话块 |
| `tako.step.compact`（A7） | 不额外写 | 摘要正文在它的 `chat` 子 span 的 output 里 |
| `tako.step.memory_extract`（A9） | 不额外写 | 抽取输入/输出在它的 `chat` 子 span 里 |

`gen_ai.tool.definitions` 不写：它装的是 schema 而不是用户/模型内容，规范也把它排除在内容开关之外、体积另算。

**格式**：按 semconv 的 JSON schema 序列化成 **JSON 字符串**（Go 的 OTel span 还不支持结构化属性；规范允许在 span 上退化为字符串）。消息对象是 `{"role":"user","parts":[...]}`；文本 part `{"type":"text","content":"..."}`；工具调用 `{"type":"tool_call","id":...,"name":...,"arguments":...}`；工具结果 `{"type":"tool_call_response","id":...,"response":...}`；思考内容 `{"type":"reasoning","content":...}`。图片不内联 base64，用 `{"type":"uri","uri":"tako://media/<id>","modality":"image"}` 引用，字节仍走 tako 的 media 出口。

**体积帽**（常量，不做配置）：每个文本 part 最多 8 KiB；每个属性序列化后最多 64 KiB，超了保留最近的 part/message 并写 `tako.content.truncated=true`。不设帽会撞上传体上限，也会把 trace 存储成本放大。

**PII 说明**：默认开意味着用户原文、工具结果、系统提示词会随 span 发到 Loop。这是本仓库 `.env` 的选择——排障需要内容。关掉只需把值改成 `NO_CONTENT`；本篇不做掩码、不做租户级开关、不做外部存储引用（§6 登记）。将来给多租户用，先补掩码再谈默认值。

#### 大厂是怎么处理的（2026-10-31 查官方文档）

| 产品 | 默认行为 | 关掉/打开的开关 | 开关作用在哪一层 |
|---|---|---|---|
| OTel GenAI 语义约定 | **默认不记录**：规范原文的建议就是「不要记录指令、输入或输出」；内容字段标为 Opt-In，并写明可能含敏感数据 | 标准 env `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT`，值 `NO_CONTENT` / `SPAN_ONLY` / `EVENT_ONLY` / `SPAN_AND_EVENT` | SDK/厂商实现；规范还建议生产环境把内容存外部、span 里只放引用 |
| LangSmith | **默认记录**：trace 里带模型的 inputs/outputs | `LANGSMITH_HIDE_INPUTS` / `LANGSMITH_HIDE_OUTPUTS`，或 Client 的 `hide_inputs` / `hide_outputs` | SDK 发送之前，按 trace 类型过滤 |
| Langfuse | 记录；但提供**数据离开应用前的掩码**机制 | SDK hook / OTel processor 做 masking 与截断 | 应用进程内、导出之前；官方明说不内置 PII 识别，规则要自己写 |
| OpenAI Agents SDK | **默认记录**：generation/function span 带输入输出，`trace_include_sensitive_data` 默认 `True` | `RunConfig.trace_include_sensitive_data=False` 或同名环境变量 | SDK 导出之前，一次性关掉全部敏感数据 |
| Azure AI Foundry | 内容捕获由**显式环境变量**控制 | `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`（如开发期用 `SPAN_AND_EVENT`） | 导出器/进程配置 |

三条可复用的结论：

1. **默认值行业里没有统一答案。** OTel 规范与 Azure 是「默认不采」，LangSmith 与 OpenAI Agents SDK 是「默认采、给关掉的开关」。默认值取决于产品把 trace 当调试工具（需要内容）还是当运营台账（只需要元数据）。
2. **只有「全采/全不采」不够，真正要的是出口前的掩码。** Langfuse 的做法值得抄：内容不是不能记，而是必须在离开应用之前把手机号、身份证、token 这类字段替换掉。这件事必须在进程内做（SDK hook 或 OTel processor），不能指望后端。本篇先不做，登记在 §6。
3. **内容体积是独立于 PII 的问题。** 几 MB 的 messages 会撞上传体上限、拖慢导出、把 trace 存储成本放大几十倍；所以即使开关打开，也要有体积帽（上文 8 KiB / 64 KiB）。

**tako 选规范里的第 2 种用法**：内容记录在 span 属性上，加标准开关。第 1 种是 OTel 默认（不采），第 3 种是生产态（外部存储 + 引用）。现在处于开发/自用阶段，内容直接放 span 最省事；开关和体积帽先立好，将来切第 3 种时只换出口，不换调用点。

出处（官方文档，2026-10-31 核对）：[OTel GenAI spans 语义约定](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-spans/)、[LangSmith 文档](https://docs.smith.langchain.com/)、[Langfuse 文档](https://langfuse.com/docs)、[OpenAI Agents SDK tracing](https://openai.github.io/openai-agents-python/tracing/)、[Azure AI Foundry 文档](https://learn.microsoft.com/en-us/azure/ai-foundry/)。

### 2.1 模型调用子 span 的时钟与算法（A4）

run 级 span 算不出 TTFT/TPOT，因为一场 run 里混着多次模型调用和工具执行；把它们摊平再除，工具耗时会被算进「每个 token」。要算这两项，必须在模型客户端包一层，每次调用单独记时。

一次模型调用，客户端能可靠拿到的只有三个时间戳：

```text
t0 ── 请求发出（调用 Stream 之前）
t1 ── 第一个 chunk 到达（第一次 Recv 拿到非空消息）
tn ── 流结束（Recv 返回 io.EOF）
```

- **TTFT** = `t1 − t0`。包含网络往返、厂商排队、prefill 三段；语义约定的名字是 `time_to_first_chunk`（首块），不是「首 token」——第一块里可能装了好几个 token。
- **解码段** = `tn − t1`。第一块之后，剩余 token 在这段时间里陆续到达。
- **TPOT** = `(tn − t1) / (output_tokens − 1)`。分母减一：第一个 token 的耗时已经算进 TTFT，从第一个到最后一个之间只剩 N−1 个间隔。
- **output_tokens** 取厂商回执里的 `CompletionTokens`（tako 已有：`usageBill`），不本地数。**口径就是全部输出 token，含 reasoning**：分子里思考的时间也在跑，分母把它们排除会把 TPOT 算高。`tako.chat.reasoning_tokens` 另写一份只为诊断，不参与分母选择。

算例：`t0=0`、`t1=0.80s`、`tn=3.30s`、`output_tokens=100` → TTFT = 0.80s，解码段 = 2.50s，TPOT = 2.50 / 99 ≈ 25.3ms。

三个边界：

1. **要的是 token 口径，不是 chunk 口径**：`output_tokens` 是 token 数，所以算出来的 TPOT 是「平均每 token」。chunk 只用来取 `t1` 的到达时刻，不参与分母；协议里没有逐 token 的到达时间戳，所以 TPOT 只能是平均值，给不出 p99 或分布。规范客户端那个 `time_per_output_chunk` 是每块间隔，和这个不是一回事，不要拿它冒充。
2. **厂商不报 usage**（Prompt/Completion/Total 三数全 0）：优先用账单，没有账单就用本地计数器对合并后的正文（含 `ReasoningContent`）估算，并在属性上标 `tako.chat.output_tokens_estimated=true`；TPOT 跟着标估算。本地计数器是 cl100k，和厂商分词器不一定一致，只能当近似值，不能当账单。
3. **非流式调用没有 t1/tn 序列**：不写 TTFT/TPOT。run 主链（主代理、子代理、折叠摘要）都是流式；记忆抽取走 `Generate` 非流式（`memory.go`），它只有总时长与 usage。

**内容**：A4 的 `chat` span 同时是内容属性的主载体（§2.2）——`gen_ai.system_instructions`（从 system 消息拆出）、`gen_ai.input.messages`（其余 wire 输入）、`gen_ai.output.messages`（本次返回的 assistant 消息，含 tool_call / reasoning parts）。流式与非流式都写；非流式没有 TTFT，但有内容、usage 与总时长。内容开关关时这些属性全不写。

### 2.3 usage 求和口径：为什么折叠摘要与记忆抽取不算在内

**先把三个名字都带「记忆」的动作分开**——它们不在同一条路上，混淆了就会问出「记忆抽取是不是从 BM25 取相似度高的记忆」：

| 动作 | 方向 | 何时跑 | 代码 | 结果去哪 |
|---|---|---|---|---|
| 折叠摘要（compact） | 写 | run 中途，历史超窗时 | `compact.go` 的 `summarizer.attempt` | 一条摘要行替换掉一批历史消息，之后进后续 prompt |
| 记忆抽取（extract） | 写 | run **结束后**，`memory_jobs` 异步任务 | `store.go:737` 登记 → `memory.go:187` 轮询 → `memory.go:336` `Generate` | 写 `memory_items` 表 |
| 记忆召回（recall） | 读 | run **起手**时 | `recall.go:35`：线一词法查 `memory_items`，线二 pg_search BM25 查 `dialogue_chunks`（`dialogue.go:187`） | 拼成一段文本追加到本轮 user 消息 |

「从 BM25 取一批相似度高的记忆」是第三行（召回，读侧），不是第二行（抽取，写侧）。本篇争的 usage 归属只涉及第一行和第二行——它们各自触发了一次模型调用；第三行只往 prompt 里拼文本，自身不调模型、不花 token（它拼进去的内容会算进那次主调用的 `prompt_tokens`）。另外 `toolsearch.go` 里也有一份 BM25，那是给延迟加载的工具挑声明，与记忆无关。

**再澄清：这不是「测不到」。** A4 之后每次模型调用都按黑盒记 provider 报的 usage 和耗时——不拆它内部读了哪些记忆、折了哪些历史，只看这次调用自己报的数。摘要调用有自己的一条 `chat` span（A7 后挂在 `tako.step.compact` 下），记忆抽取有自己的一条（A9 后根是独立 trace 的 `tako.step.memory_extract`），成本都看得到。下面争的只是**根 span 那一个 `gen_ai.usage.*` 字段的归属**：它要和 usage 帧逐值对账。

先演一场会真实发生的对账：某场 run 在页面上有三条带 usage 的 assistant 条目，求和输入 2,400 / 输出 800；同一天账单显示这个时段多扣了 750（折叠一次）和一笔记忆抽取。三个数各自都是真的，混在一个字段里就没人能说清差额从哪来。

tako 里同时存在两个账本，口径不同：

| 账本 | 内容 | 今天在哪能看到 |
|---|---|---|
| A 对话账本 | 每条进入对话历史的 assistant 消息的 provider usage（主代理 + 子代理） | `turn` → runlog 行 + runlive 的 usage 帧 |
| B 本进程为这场 run 触发的全部模型调用 | A + 折叠摘要 +（run 结束后异步跑的）记忆抽取 | 没有单一字段；A4 后本 run trace 里的 `chat` 求和 = A + 折叠；记忆抽取是另一条独立 trace（根是 A9 的 `tako.step.memory_extract`），不在本 run 的 trace 里 |

**折叠摘要为什么不进 A。** 看代码路径：`foldInput` 调 `summarizer.attempt`，用 `chatFrom(ctx, s.chat).Stream(...)` 发一次摘要请求（`compact.go:957`）；拿回的 `ResponseMeta.Usage.CompletionTokens` 只被 `rejectReason(...)` 和一行日志使用（`compact.go:979-987`），既不写成 assistant 条目，也不发 usage 帧。这是有意的：摘要是一条内部产物行（替换掉一批历史消息），不是对话里说过的话。如果把它算进本 run 的对话账本，run span 的 token 就和页面帧对不上；而且同一段历史每折一次，就会在新 run 上再记一笔——它不是这场对话新花的钱。

**记忆抽取为什么更不可能进。** 只有 `complete` 终态会在 `EndRun` 事务里登记一条 `memory_jobs`（`store.go:737`），进程里的常驻循环每 2 秒 claim 一条（`memory.go:187`），用非流式 `Generate` 调模型（`memory.go:336`）；它连 provider usage 都没落库，统计量是本地 cl100k 计数器算的（`memory.go:354-357`）。它跑在 run 定稿之后、可能重试、可能几分钟后才执行——不在 run 的 ctx 里，也不可能是 run span 的子 span。A4 的包装器会在它自己的执行里开一条 `chat` span，A9 再给它套一条 `tako.step.memory_extract` 作为这条独立 trace 的根；那条 trace 与 run 无关（run 早已结束），run 根 span 的 token 和永远不含它。

**「摘要会进后续消息」不改变这个结论。** 摘要文本之后会作为后续请求的输入，但那只体现在后续那次调用的 `prompt_tokens` 里（provider 报的输入 token），不是摘要调用自己的 `completion_tokens`；两次调用各记一次，是两笔真实计费。A4 的每次 `chat` span 已经天然包含这两笔，不需要在根 span 里再把内容拆开算一遍。

所以定两条规则：

1. **根 span 的 `gen_ai.usage.*` 只取 A 口径**，并用 `tako.usage.scope=conversation` 显式钉住。A2 的验收就是拿它和 usage 帧求和逐值对账；口径不一致这个验收永远过不了，也就没人能拿它做假报表。
2. **要看 B 就求和子 span**：A4 之后每个模型调用一条 `chat` span，折叠摘要也在其中（它走 `Stream`，所以有 TTFT）；A5 的工具 span 不带 token；A7/A8/A9 的步骤 span 只有计数，token 只在它们嵌套的 `chat` 子 span 上。记忆抽取的那条 `chat` span 在另一条独立 trace 里，不属于本 run 的 trace；要把 B 算全，得把两边的 span 相加，并且它是否发生取决于后台调度（§6 登记）。

**为什么必须在根 span 这一层选口径。** 只有 `turn` 是权威且可核对的来源：provider 报的 usage 原样落在那里，帧和 runlog 都从它投影。根 span 若自己发明一个「全都算上」的数，它既对不上帧，也对不上 A4 的子 span，接收端拿它做成本看板就会和 tako 自己的账本打架。**选一个能对账的口径并写明，比选一个看起来更全的口径有用。**

数字示例：根 span 写 2,400 / 800（`tako.usage.scope=conversation`）；折叠摘要 600 / 150 单独出现在它的 `chat` 子 span 上；本 run 的 trace 里所有 `chat` span 求和 = 3,000 / 950；记忆抽取约 400 出现在它自己那条独立 trace 里。谁想回答哪个问题，就用对应的数。

### 2.4 入口 span、排队与父子关系（A6）

现在的 202 路径是：`postRuns` 抢闸成功 → `go worker.Run(context.Background()+principal, …)` → 写 202。run span 在 worker 里起手，所以「服务收到请求」到「run 起跑」这段（鉴权、准入事务、项目预检、goroutine 调度）没有任何记录；trace 从起跑才开始。

A6 补一条 HTTP 入口 span：

| 项 | 值 |
|---|---|
| span 名 | `POST /api/runs` / `POST /api/runs/:id/resume`（`{method} {route}`） |
| span kind | `SERVER`（OTel HTTP 语义：入站请求） |
| 起止 | 请求进入 handler 链时起手；响应写完（202/401/404/409/…) 时收尾 |
| 属性 | `http.request.method`、`http.route`、`url.path`、`http.response.status_code` |

中间件挂在这两条路由的 `authn` **之前**，所以 401 也能留下一条只有入口 span 的 trace（失败请求没有 run span，正好靠状态码分类）。

关系用**父子**，不用 link：入口 span 的 span context 和起手时刻随 ctx 带进 worker，run span 以它为 parent。这样一次请求触发的入口、排队、run、chat、tool 全在同一个 traceId 下，Loop 按 trace 打开就是完整时间线。

两个容易踩的点：

1. **parent 结束得比 child 早是合法的**：入口 span 在写完 202 时就 End 了，run span 之后才结束。OTel 不要求父子时间重叠；Loop 的时间线会显示成「入口一段 → 空隙 → run 一段」。如果把入口 span 一直开到 run 结束，HTTP 耗时就会被 run 时长污染，等于没有入口指标。
2. **不能把请求 ctx 直接交给 worker**：`postRuns` 现在就有意用 `context.Background()`（请求生命周期不属于 run，响应写完请求 ctx 就取消了）。所以带过去的是 span context + 入口起手时刻，不是可取消的 ctx；`tako.run.start_delay_ms` = run 起跑 − 入口起手。

「202 到起跑」的等待在这个进程里几乎为零：goroutine 在写响应之前就 launch 了。真正值得看的是「收到请求 → 起跑」的准入延迟，所以属性用入口起手时刻做基准，而不是用 202 的写出时刻——后者甚至可能晚于 run 起跑，算出负数。

### 2.5 业务关联：同一件事的多场 run 怎么串

一条 trace 对应一次 HTTP 请求；「同一件事」往往不止一次请求：run 1 失败，用户补一句话再发一轮；或者崩溃后续跑；或者用户换了个会话继续。

根因是两个维度本来就多对多：一个 session 里可以聊很多件事，一件业务也可能跨多个 session（甚至多个项目）。**加一列或加一张表本身不难**——难的是先把这个 ID 代表什么定义清楚，而这一步今天没有答案：

- **谁创建、什么时候创建。** 客户端在第一次提问时自己铸一个 UUID 并一直带着？还是服务端看到一句「补充」时猜它属于上一场 run？前者要求产品里有「任务」这个对象，后者需要用户显式标注或模型判断，两者都还没有。
- **一场 run 属于几个任务。** 一次提问可能同时推进两件事；一个任务也可能一场 run 只做其中一步。一对一、一对多、多对多选哪种，决定了这个字段放在 `runs` 行上还是一张关联表——现在连选哪种都没定。
- **生命周期归谁。** 任务什么时候算完成、失败、被放弃？谁来翻状态？今天 `runs.status` 是 run 的权威、session 由用户显式结束、workspace 没有终态；再加一个对象就要再定一套状态机。
- **UI 怎么承载。** 用户能重命名、合并、拆分任务吗？如果没有入口，这个 ID 只是客户端悄悄带上的一个不透明字符串，那它就不是可运营的业务对象。

所以今天缺的不是「一个能加字段的地方」，而是**任务的产品语义**。能先做的最小形态是**不透明的客户端归组键**：客户端保证同一件事带同一个值，服务端只存、只写进 trace 属性，不解释、不校验、不做生命周期。它能解决「换会话/换项目后怎么连起来」，但要求客户端改造，也解决不了「服务端怎么判断两次提问属于同一件事」。完整的任务实体（建表、状态机、UI 入口）是产品级决定，本篇不发明（§6）。

在那之前，用现有三种键做近似：

按范围从窄到宽有三个键：

| 要回答的问题 | 用哪个键 | 怎么用 |
|---|---|---|
| 同一场对话里的多次提问 | `gen_ai.conversation.id = sessionId` | 用户在同一个会话里补发消息时，`POST /api/runs` 带同一个 `sessionId`，但会铸一个新 `runId` → 新 trace、同一个 conversation.id。Loop 里按它搜，两场 run 按时间排在一起 |
| 同一场 run 的多次执行（续跑） | `tako.run.id` + `tako.run.epoch` | 崩溃/停止后续跑沿用同一个 runId，epoch 递增；每次执行是一条独立 trace，按 run.id 搜得到全部代数 |
| 同一个项目里的多场会话 | `tako.workspace.id` | 用户换了新会话（新 sessionId）继续同一件事时，会话仍挂在同一个项目下；按项目搜能收拢（同一项目里也可能有别的任务，这是粗归组） |

今天**没有**一个显式的业务/任务 ID：API 里没有「这是一次重试」或「这条补充属于哪个失败」的字段，客户端也不传 traceparent。所以：

- 「同一会话里补发」能精确归组（conversation.id）；
- 「换会话继续」只能靠项目（workspace.id），项目里混着别的任务时无法再细分；
- 「换项目继续」没有任何键能连起来——要连就必须加一个归组键（例如客户端生成的不透明 taskId），这要客户端配合且属于产品决定，本篇不做，登记在 §6。

### 2.6 内部步骤 span：把 run 拆成可读的时间线（A7–A9）

run 根 span 只回答「整场多久」，A4/A5 只回答「模型/工具各自多久」。中间还有几段既不调模型也不是工具的动作：准入、装历史、召回、折叠、定稿、后台记忆抽取。它们可能各自吃掉几百毫秒，出问题时却只能在一行日志里猜。A7–A9 给它们各开一条 INTERNAL 子 span，父 span 是 run 根 span；不改任何业务行为。

| 步骤 | span 名 | 语义 | 关键属性 | 代码点 | 拍 |
|---|---|---|---|---|---|
| 配额 | `tako.step.quota` | 自有步骤 | `tako.quota.daily`、`tako.quota.used`、`tako.quota.exceeded` | `worker.run` 的 quota 检查 | A8 |
| 项目目录 | `tako.step.workspace` | 自有步骤 | `tako.workspace.id`、`tako.workspace.max_bytes`、`error.type` | `runWorkspace` + 配额 reserve | A8 |
| 装历史 | `tako.step.history` | 自有步骤 | `tako.history.messages`、`tako.history.resume` | `ModelMessages` 调用处（两条 prepareInput 路径） | A8 |
| 召回总 | `tako.step.recall` | 自有步骤 | `tako.recall.terms`、`memory_hits`、`dialogue_hits`、`injected_runes` | `recall.go` `Store.Recall` | A7 |
| 线一记忆 | `search_memory` | `gen_ai.operation.name=search_memory`、`gen_ai.memory.store.id=tako-memory`、`gen_ai.memory.record.count` | 内容开关开时 `gen_ai.memory.query.text`、`gen_ai.memory.records` | `recallMemoryItems` | A7 |
| 线二对话 | `retrieval` | `gen_ai.operation.name=retrieval`、`gen_ai.data_source.id=pg_search` | `gen_ai.retrieval.top_k`；内容开关开时 query/documents | `recallDialogueItems` | A7 |
| 折叠 | `tako.step.compact` | 自有步骤 | `tako.compact.trigger`（initial/midfold/rescue）、输入/摘要 token、drop runs、结果 | `foldInput` | A7 |
| 定稿 | `tako.step.finalize` | 自有步骤 | `tako.finalize.status`、staged/mh 行数 | `EndRun` defer | A8 |
| 记忆抽取 | `tako.step.memory_extract` | 自有步骤 | job/run/session/attempt、item 数 | `processMemoryJob` | A9 |

四条关系：

1. **父子**：除记忆抽取外，所有步骤 span 的 parent 是 run 根 span。`tako.step.compact` 里的摘要调用由 A4 包出一条 `chat` 子 span，所以「折叠总耗时 − chat 耗时 = 本地折叠耗时」。`tako.step.memory_extract` 跑在 run 之外，是**独立 trace 的根**，它的 `chat` 子 span 也在这条 trace 里。
2. **召回拆两段**：`tako.step.recall` 是总时长，两个子 span 分别是线一词法查询与线二 pg_search。哪条慢直接看子 span；总时长与两子段之差是切词/拼装开销。
3. **不携带 token**：步骤 span 只有记录数、token 数（折叠的输入/摘要统计）等；唯一的模型 token 在嵌套的 `chat` 子 span 上。根 span 的 usage 口径不变（§2.3）。
4. **内容不重复抄**：步骤 span 只在 §2.2 列出的位置写内容（召回 query/records/documents；折叠与抽取靠嵌套 `chat`），不在每个步骤里再抄一份正文。

**为什么放在这一层**：这些步骤不是 GenAI 操作，不能编 `gen_ai.operation.name`（规范词表里没有 compact/finalize）；但它们又确实发生在 run 内部、需要一个共同父 span 才能和模型/工具时间对齐。所以用 `tako.step.*` 自有命名空间、kind `INTERNAL`；`cozeloop.span_type` 先不写，Loop 归不了类就按 CUSTOM 显示，A7 实跑后再决定是否补标签。

**不做**：子代理内部工具调用仍无 span（A5 的包装链限制）；工具检索（`toolsearch.go` 的 BM25）不单独开 span，它发生在工具声明装配期、耗时通常可忽略，真要查再说。

---

## 3. 数据从哪来（端到端链路）

```
用户 POST /api/runs
   → HTTP 入口 span 起手（t0 = 服务收到请求；A6）
   → server 鉴权、铸会话/run 行、抢闸、写 user 行、返回 202
        （准入耗时 = 入口 span 自己的时长；写完 202 时入口 span 收尾）
   → worker.run 起手：principal → 开 invoke_agent 子 span（t1 = 起跑；A6 起写 tako.run.start_delay_ms = t1 − t0；quota 早退也留记录）
        → tako.step.quota → tako.step.workspace
        → tako.step.history（读 model_history）
        → tako.step.recall → search_memory / retrieval（内容开关开时带 query/records）
        → tako.step.compact（超限时）→ 它的 chat 子 span（摘要调用）
        → 主循环：chat 子 span（A4）+ execute_tool 子 span（A5）；turn 累积 assistant/tool 条目
   → tako.step.finalize（EndRun：写 runlog/mh、登记记忆任务、放闸）
   → run span 收尾 defer：读 runs.status → 求和 usage → 写收尾属性/内容 → span.End()
   → SDK 批量导出（约 5 秒一批）→ OTLP POST → CozeLoop

后台（run 结束之后，独立 trace）：
   memory_jobs claim → tako.step.memory_extract → chat 子 span → memory_items
```

- 求和口径是 **turn 全量**，含子代理的模型调用；不含折叠摘要与记忆抽取（§2.3 讲了两本账的口径）。
- run span 收尾的 defer 注册在 EndRun 的 defer 之前——LIFO 决定它最后跑，所以能读到定稿后的 `runs.status`，并在 `tako.step.finalize` 关闭之后才关闭根 span。
- 入口 span 与 run span 同一条 trace；A6 之前没有入口 span，run span 自己就是根（§2.4）。
- 内容属性受 `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT` 控制（`.env` 默认 `SPAN_ONLY`）：根 span 写本轮业务输入/输出/系统提示词，`chat`/`execute_tool`/召回 span 写各自的内容（§2.2）。
- 日志里打一行 `trace <sessionId>/<runId> traceId=<32 hex>`，客服/排查从 runId 能跳到 Loop。

---

## 4. 四条硬约束

1. **观测失败不许碰业务**：令牌/空间 ID 任一为空 → 全程 noop（本地开发与单测零联网）；配了但网络失败 → 导出报错只记日志，run 照常跑、启动照常成功。trace 不是模型配置，不走 fail fast。
2. **内容受开关控制**（§2.2）：`.env` 默认 `SPAN_ONLY`，会把用户原文、工具结果、系统提示词写进 span；改成 `NO_CONTENT` 则回到只发元数据。掩码与租户级开关没做，多租户上线前必须补（§6）。这不是性能取舍，是 PII 边界。
3. **不改契约字节**：帧、runlog、任何 API 响应都不加 traceId；traceId 只进进程日志。前端要看 trace，去 Loop。
4. **多副本无共享态**：每个进程自建 TracerProvider，没有跨进程锁或注册表；`service.instance.id` 区分副本。崩溃时最多丢批量缓冲里那几秒的 span——可接受，run 的权威结果仍在 tako 库里。

---

## 5. 落地步骤

| 步 | 做什么 | 解决了什么 | 验收（第一行是打一个跑着的东西） |
|---|---|---|---|
| A1 | `internal/observability` 装配（env → OTLP exporter → TracerProvider，缺配置即 noop；解析 `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT` 并提供内容序列化 helper）+ `POST /api/dev/trace-check` 发一条样例根 span（开关开时带样例 input/output/system，验证 JSON 与体积帽）并 ForceFlush | 先证明「令牌 + 空间 ID + 端点 + 属性名 + 内容开关」这条管道通；不通时能隔离在观测层 | `curl -X POST /api/dev/trace-check` → `{"code":0,...,"data":{"traceId":"<32 hex>","exported":true}}`；Loop 按 traceId 搜到 `invoke_agent yktako`，内容开关开时能看到 `gen_ai.input.messages`/`output.messages`；把 env 改成 `NO_CONTENT` 重起 → 同一条探针没有内容属性；清空 COZELOOP env → `exported:false` |
| A2 | `worker.run` 起手开 span、收尾写属性；`runs.status` 回读；`tako.workspace.id` 回读；根 span 内容属性（本轮用户原文 / root assistant 输出 / 系统提示词）；日志打 traceId | 把真 run 的会话/租户/项目/用量/状态/业务输入输出装进记录 | curl 起一场真 run → 完成后 Loop 搜 `conversation.id=<sessionId>`；`gen_ai.usage.*` 与 `/api/runs/{id}/frames` 的 usage 帧求和逐值相等；`tako.workspace.id` 与 `sessions.workspace_id` 一致；内容开时三个内容属性可 JSON 解析，`NO_CONTENT` 时一个都没有 |
| A3 | doc/54 收口、doc/README 索引、doc/16 基线、`.env` 注释与启动日志说明（含内容开关与 A7–A9 已登记） | 状态与真源一致，下一场会话知道第一段已落地、后续拍已登记 | `bash scripts/check-master-plan.sh` 通过；A1/A2 两条 curl 复打仍绿 |
| A4（第二段，A3 绿后接） | 模型调用子 span：包装 `model.BaseChatModel`，每次调用一条 `chat {model}` CLIENT span，写 `gen_ai.response.time_to_first_chunk`、usage、`gen_ai.response.finish_reasons`；内容开关开时写 `gen_ai.system_instructions` / `input.messages` / `output.messages`；TTFT/TPOT 从这里出 | run 级只有总时长，TTFT/TPOT 是推理调用层的度量；内容属性落到每次真实调用上 | curl 起一场流式 run → Loop 里 run span 下出现 `chat <model>` 子 span；`time_to_first_chunk` 非空且 < 总时长；`(总时长 − TTFT) / (output_tokens − 1)` 与手算值一致；内容开时 input/output 可解析，output 含 tool_call parts |
| A5 | 工具执行子 span：在 `journalInvokableTool.InvokableRun` 最外层（早于所有 early return）开 `execute_tool {name}` INTERNAL span，写 `gen_ai.operation.name=execute_tool`、`gen_ai.tool.name`、`gen_ai.tool.call.id`；内容开时写 `gen_ai.tool.call.arguments` / `result`；工具报错被 `wrapToolErrors` 转成文本 → 仍算 OK，取消/中断 → Unset | run 总时长能拆成模型时间与工具时间，直接看出「哪次工具等了三分钟」 | curl 起一场会调工具（如 `get_time`）的 run → Loop 里 run span 下出现 `execute_tool <name>`；`gen_ai.tool.call.id` 与 frames 里该次 tool_call 的 id 相同；子 span 时长 > 0 且 < run span 总时长；内容开时 arguments/result 非空 |
| A6 | HTTP 入口 span：两条铸 run 路由（`POST /api/runs`、`POST /api/runs/:runId/resume`）在 `authn` 之前挂中间件，开 SERVER span；把 span context + 起手时刻带给 worker，run span 挂成子 span；写 `tako.run.start_delay_ms` | trace 从「服务收到请求」开始，准入延迟与排队空隙可见；失败请求也有记录 | curl 起一场 run → Loop 里一条 trace 的根是 `POST /api/runs`、子 span 是 `invoke_agent yktako`；`tako.run.start_delay_ms` 非负；用无 token 打 `POST /api/runs` → 只有入口 span、`http.response.status_code=401` |
| A7（步骤 span 第一批） | 召回与折叠：`Store.Recall` 开 `tako.step.recall`，两线各开 `search_memory` / `retrieval` 子 span（带记录数；内容开时带 query/records/documents）；`foldInput` 开 `tako.step.compact`（加 trigger 参数 initial/midfold/rescue，写输入/摘要 token、drop runs、结果），摘要 `chat` span 挂成它的子 span | 回答「召回花了多久、折叠花了多久」，并把召回两线拆开；折叠总时长能减去模型时长得到本地耗时 | curl 起一场记忆打开的 run → Loop 里 `tako.step.recall` 下有 `search_memory` + `retrieval`，三者时长均 ≥0；用 `-compact-window 1000` 起隔离引擎跑长会话 → 出现 `tako.step.compact`，其下出现摘要 `chat`；根 span 的 usage 仍与 usage 帧相等；内容开时 records/documents 非空 |
| A8（步骤 span 第二批） | 准入/装历史/定稿：`tako.step.quota`、`tako.step.workspace`、`tako.step.history`（两条 prepareInput 路径）、`tako.step.finalize`（EndRun defer，包住整笔事务） | 把 run 开始前和结束时的本地耗时与水落可见，解释「总时长里还有哪几段」 | curl 起一场 run → Loop 里 run span 下出现 quota/workspace/history/finalize 四条子 span；finalize 时长 > 0；quota 早退时 run span 下仍能看到 `tako.step.quota` 与失败原因 |
| A9（步骤 span 第三批） | 后台记忆抽取：`processMemoryJob` 开 `tako.step.memory_extract`（独立 trace 的根），把 ctx 传给 `Generate`，让 A4 的 `chat` span 挂成子 span；写 job/run/session/attempt/item 数 | 记忆抽取的模型耗时与本地解析/写库耗时可见，且明确它不属于 run trace | 完成一场 run 并等到 memory job 跑完 → Loop 搜 `tako.step.memory_extract`，看到独立 trace 与 `chat` 子 span；该 trace 里没有 run span；run trace 里没有这条 span |

实跑发现的两条运维事实：① `COZELOOP_WORKSPACE_ID` 必须是令牌所属工作区（用 `GET https://api.coze.cn/v1/workspaces` 核对）；不一致时 OTLP 回 400，SDK 只把错误交给全局 error handler，业务不受影响但看不到任何 trace。② 传输固定 HTTP/protobuf（同一份 span 用 JSON 上报回 500），exporter 保持默认 gzip。

A4–A9 都只依赖 A2 的 run span 与 A3 的文档收口；A7 的折叠 `chat` 子 span、A9 的抽取 `chat` 子 span需要 A4 已落地，A7/A8/A9 其余部分可与 A6 换序。A5 只覆盖根级工具调用；子代理内部工具调用不在 `journalWrapHandler` 的包装链上，没有 span（§6 登记）。

---

## 6. 本篇明确不做（登记，不假装覆盖）

- **子代理内部工具调用没有 span**：A5 的包装点 `journalWrapHandler` 只把根级工具池换成记账壳；子代理的入口快照在包壳之前拍下，它内部再调工具不进包装链。要补得先有子代理的 agent span，本计划不做（子代理调用本身是一个根级工具，A5 会给它一条 `execute_tool <subagent>` span）。
- **折叠摘要与记忆抽取不进 run span 的 usage 求和**：A4 的包装器会给折叠摘要一条 `chat` 子 span（流式、有 TTFT），给记忆抽取一条 `chat` span 但落在独立 trace 里（非流式、只有总时长）；run span 的 token 和仍只算 `turn`（主代理 + 子代理的对话调用），并用 `tako.usage.scope=conversation` 钉住口径。根 span 回答的是「这场对话花了多少」，不是「本 run 触发的全部模型花费」——对账时按这个口径（§2.3）。
- **内容属性的掩码、租户级开关、外部存储引用**：内容本身按 §2.2 的开关记录（`.env` 默认开）；本篇不做出口前掩码、不做按租户开关、不把内容搬到外部存储再放引用（OTel 规范的生产态用法）。多租户上线前必须先补掩码，否则默认开就是把用户原文推给 Loop SaaS。
- **内容事件（events）与 `gen_ai.tool.definitions`**：只写 span 属性；`SPAN_AND_EVENT` 退化为 `SPAN_ONLY`，`EVENT_ONLY` 等于关闭（tako 不产出内容事件）。工具 schema 不是用户/模型内容，不写。
- **工具检索的 BM25 span**：`toolsearch.go` 的 BM25 给延迟加载的工具挑声明，发生在工具装配/首轮，耗时通常可忽略；本篇不单独开 span。
- **CozeLoop 的 Eval / 实验管理**：doc/45 的场景化 Eval 仍是我们自己的回归层；Loop 的 experiment 不替代它。trace 只解决「看」。
- **入站 trace context 与跨请求链路传播**：客户端不传 traceparent，每次 HTTP 请求一条独立 trace；同一 run 的续跑是另一条 trace，靠 `tako.run.id` + `tako.run.epoch` 归组，跨会话靠 `gen_ai.conversation.id` / `tako.workspace.id`（§2.5）。
- **产品级业务/任务 ID**：加字段本身可行，缺的是任务语义——谁创建、一场 run 归属几个任务、生命周期归谁、UI 怎么承载都未定义（§2.5）。最小形态是客户端自带的不透明归组键，那要求客户端改造，属于产品决定；本篇不发明。

---

## 7. 收尾：所以这解决了什么

今天「一场 run 干得怎么样」只能靠人翻帧；跨 run 的比较、租户维度的失败聚集、换模型前后的耗时对比都没有一个可检索的集合。这一层把每场 run 变成一条按 OTel GenAI 语义命名的记录，经 OTLP 报到 CozeLoop：**字段名是公共语言（换后端不改代码），内容由标准开关控制（`.env` 默认开、改 `NO_CONTENT` 一键关），失败隔离（观测坏了业务照跑）**。完整形态下，一条 trace 从服务收到请求开始，包住 run、每次模型调用、每次工具执行，以及召回/折叠/准入/定稿这些内部步骤（§2.6）；同一场对话的后续提问按 `gen_ai.conversation.id` 归组，换了会话还能按 `tako.workspace.id` 找回（§2.5）；token 口径显式钉为对话账本（§2.3），与 usage 帧逐值对得上。

它不替代 doc/27 的账本，也不替代 doc/45 的回归——它补的是「门牌」：出了事，先能按租户、会话、项目、状态、模型把相关 run 找出来。
