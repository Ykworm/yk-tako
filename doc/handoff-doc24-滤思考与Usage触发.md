# 交接：doc/24 两件事实现（滤思考 + Usage mid-run 触发）

> 写给实现方（人或 agent）。项目：`/Users/yuweikang/dev/ykcode/learn/yk-tako`，代码主仓在 `yk-tako-eino/`。
>
> **性质说明**：`doc/24-用量计数.md` 已改为**实现真源**，与本交接对齐。实现以两文公式与裁决为准，着手本篇对齐的 **2 个需求**（滤思考 + Usage mid-run 触发）。仍**不写** `tmp/` 台账；存储面（思考进 runlog / 改 `19v6-03`）仍**不要做**。
>
> 指针已更新：`doc/19-v6/19v6-07-compact.md` §1、`doc/19-v6/19v6-10-mid-run-fold.md` §6。`19v6-03` / 架构总览 / API **未动**（runlog-for-reasoning 未定）。

---

## 1. 目标 / 非目标

### 目标（只做这两件）

1. **同一场 run 内、下一 Turn 发给模型之前**：清空历史 assistant 消息上的 `ReasoningContent`，使真实请求不再重发上一枪思考；从而 `countInput` 与真实请求在「思考」这一维对齐。
2. **可选的 Usage 基线 mid-run 触发**：有上一笔账单时，用  
   `PromptTokens + (CompletionTokens − reasoning_est) + 锚点后新行`  
   代替全量 BPE；无账单时仍走今天的全量 `countInput`。工具 schema **仍不进**触发。

### 非目标（明确不做）

- 不写 `tmp/` 台账（**仍不造临时台账**；两文已对齐为可实现，本拍仍不写台账）。
- **不改** `countInput` 的三块基数（system / text / tool_calls）。
- **不改** limit 公式（`window − min(window/5, 16384)`）。
- **不把** Usage 或思考默认写入四个 API / 帧 / `run_model_messages` / `runlog_entries`。
- **不做**「思考进 runlog」展示列（另裁存储面时再开；见 §6）。
- **不改** `19v6-03`、架构总览、API 契约。
- 不做挂在引擎上的全局 `lastUsage`；账单粘在产生它的 assistant 消息上。
- 不给子代理新挂折叠；不替换溢出层。
- 滤思考后若厂商 400：**不补假思考、不单独重试**，走现有 `error`。

---

## 2. 已对齐裁决（实现时当不变量）

| 项 | 裁决 |
|---|---|
| 滤思考 | 场内下一 Turn 发出前清空 `ReasoningContent`（默认要做） |
| schema | **不计入**触发（WeKnora 战例仍有效；不翻案） |
| Usage | **可选**：有则用基线+delta；无则全量 `countInput` |
| 上下文长度看哪 | 看 **`PromptTokens`（输入侧）**，**不要**把 `CompletionTokens` 当「当前上下文」 |
| 思考落库 | 默认 **不进** `run_model_messages`；展示若将来要做，只进 `runlog_entries`，另开存储面 |
| 账单落库 | 本拍 **不写 Usage 列**；跨场从 mh 读回没有 `ResponseMeta` |

---

## 3. 病灶（为何要做）

- DeepSeek 等思考模型把长思考放在 `schema.Message.ReasoningContent`，正文在 `Content`。
- eino openai 兼容组件发下一轮时会把 `ReasoningContent` **原样**放进 assistant → 思考进入真实请求。
- mid-run 计数走 `convertState` → `ModelMessage` 只有 `Text` + `ToolCalls`，**丢掉** `ReasoningContent` → `countInput` 把思考数成 0 → 自测偏小被放行，厂商按真实长度拒收或吃窗。
- 今天思考本来就不落库：`drain` 合并后只把 `merged.Content` + `ToolCalls` 写入 `TurnEntry`；EndRun 进 mh/runlog 也只有正文侧。思考只活在本场 ADK `state.Messages`。跨场读回没有它。

---

## 4. 精确公式与回退

### 4.1 锚点（必须在原始 `state.Messages` 上找）

从队尾往前，找最后一条 `role == assistant` 且 `ResponseMeta.Usage` 非空（三个整数不全为 0）的消息。

**关键**：`convertState` / `toSchemaMessages` 之后账单已丢，**禁止**在转换后再找锚点。

### 4.2 有账单时

```text
reasoning_est =
  若 Usage.CompletionTokensDetails.ReasoningTokens > 0 → 用它
  否则 → cl100k(锚点.ReasoningContent)   // 滤之前估；无则 0

visible_completion = CompletionTokens − reasoning_est
若 visible_completion < 0 → 置 0

next_input = PromptTokens
           + visible_completion
           + countInput("" /* system 空 */, 锚点之后的新行)
```

说明：

- 锚点之后的新行主要是尚未发给厂商的 tool result（以及若有的新 user）。
- 锚点自己的正文 / tool_calls 已由 `visible_completion` 代表，**不要**再进 delta。
- system 与上一枪 schema 已在 `PromptTokens` 里，delta **不再加**。
- **等价验收写法**（不用 Completion 减思考）：  
  `PromptTokens + countInput(上一枪可见输出行) + countInput(新行)`  
  两条应对齐到同一量级，允许分词器偏差。

与 `limit` 比：超了走现有 `foldInput`；没超则放行。**无论超不超，放行前都要执行滤思考**。

**不要用**：`TotalTokens + 新行`（含思考，滤后会偏保守早折）；也不要用「只有 Completion − 思考 + 新行、没有 Prompt」（缺整份输入历史）。

### 4.3 无账单时（回退）

下列情况用今天的全量自测：

- 每场第一次模型调用前（`prepareInput` / Run fold）——本来就没有本场 Usage
- 刚折完、厂商还没对折后那一份回执（`toSchemaMessages` 重写 state **不拷贝** `ResponseMeta`，锚点自然消失）
- Usage 缺失或不完整

```text
total = countInput(system, 全部可见历史)
```

**禁止**拿上一场末尾账单当本场第一呼长度。

### 4.4 滤思考时机与 `reasoning_est` 顺序

1. （可选）在原始 state 上找锚点、读 Usage、在**清空前**算 `reasoning_est`
2. 用公式或全量 `countInput` 做触发判断；超限则 `foldInput`
3. **发出前**：清空 state 里相关 assistant 的 `ReasoningContent`（至少清空会被重发的那些；实现上可对整份 `state.Messages` 里所有 assistant 清空）

滤完字段空了，减项必须事先算好，不能事后从空字段估。

---

## 5. 改哪里（已在 Mac 代码核实）

根目录：`yk-tako-eino/internal/runengine/`。eino = `v0.9.21`。

### 5.1 计数与转换（现状核实）

| 符号 | 文件:约行 | 现状 |
|---|---|---|
| `countInput` | `compact.go:249` | `system + Text + json.Marshal(ToolCalls)`；**不数** ReasoningContent / schema |
| `foldInput` | `compact.go:388` | 超限折叠主循环；本拍签名可不动 |
| `convertState` | `midfold.go:141` | state → mh；首条必须 system |
| `newRowFromMessage` | `midfold.go:174` | 只取 `m.Content` + Role/ToolCalls；**丢** ReasoningContent / ResponseMeta |
| `toSchemaMessages` | `midfold.go:191` | `AssistantMessage(Text, ToolCalls)` 等；**不带回** ReasoningContent / Usage |
| `midfoldHandler.BeforeModelRewriteState` | `midfold.go:275` | 现：`convertState` → **全量** `countInput` → 超限 `foldInput` → 重写 state |
| `midfoldHandler.AfterAgent` | `midfold.go:313` | 折后 state → mh 投影进 foldBox |
| `prepareInput` | `worker.go:612` | Run fold：全量 `countInput`；产物 `toSchemaMessages` |
| `drain` | `worker.go:523` | 流排干 `ConcatMessages`；思考在 chunk 的 ReasoningContent，**不进 delta 帧** |
| turn 写入 | `worker.go:413` | `TurnEntry{Text: merged.Content, ToolCalls: ...}`——**无** ReasoningContent |
| EndRun | `worker.go` defer → `store.EndRun` | mh/runlog 只有正文侧 |
| Handlers 顺序 | `worker.go:197-211` | agentSpec → noteSnapshot → **midfold** → agentLog → overflowGuard |

### 5.2 eino 字段（依赖）

- `schema.Message.ReasoningContent`（`string`，json `reasoning_content`）
- `schema.ResponseMeta.Usage` → `TokenUsage`：`PromptTokens` / `CompletionTokens` / `TotalTokens`
- `Usage.CompletionTokensDetails.ReasoningTokens`（有则优先于 cl100k 估思考）

### 5.3 建议落点（实现面）

**主战场 = `midfold.go` 的 `BeforeModelRewriteState`**（每轮模型调用前唯一能在「发出前」动 state 的位点）：

1. **在 `convertState` 之前**（原始 `state.Messages`）：
   - 找 Usage 锚点
   - 若有锚点：算 `reasoning_est`（优先 `ReasoningTokens`，否则对 `ReasoningContent` 做与 `countInput` 同词表的 `counter.Count`）
   - 算 `next_input`；无锚点则走现有 `countInput(sys, history)`（仍可先 convert 再全量数，与今天同形）
2. 超限 → 现有 `foldInput` 路径不变
3. **放行前滤思考**：清空 `state.Messages` 里 assistant 的 `ReasoningContent`（折后重写路径：`toSchemaMessages` 本就不带思考，折后自然干净；未折的直通路径必须显式清空）

可选辅助：

- 抽纯函数如 `usageBasedInput(counter, msgs) (n int, ok bool)` / `stripReasoning(msgs)`，便于单测钉公式，不必起整条 ADK。
- `prepareInput` / Run fold：**不必**接 Usage 公式（首呼无本场账单；跨场也无 ResponseMeta）。滤思考对首呼基本无操作对象（库回放本来无 ReasoningContent）。

**不要改**：`countInput` 基数、`foldInput` 算法本体、`19v6-03`、EndRun 落库列、四个 API。

---

## 6. 尚未裁、本拍不要做

- **思考进 `runlog_entries` 展示**（新列或 payload）：点头另开存储面，并改 `19v6-03` / 架构总览 / 必要时 `19v6-01`。未裁前思考只存在于本枪响应内存，滤掉即丢。
- Usage 落库 / 全局 lastUsage
- 改 limit、改 schema 是否计入
- 子代理折叠

---

## 7. 风险

1. **厂商要求下一轮带回 `reasoning_content`**（部分思考模型）：滤掉后可能 **400**。裁定：走现有 `error`，不补假思考、不单独重试。验收时用目录常用思考模型（如 DeepSeek）打一枪确认行为。
2. **`reasoning_est` 与厂商 `ReasoningTokens` 不一致**：优先厂商字段；无则 cl100k 估；`visible_completion < 0` 置 0。
3. **PromptTokens 含 schema，自测不含**：有意为之；溢出层继续兜底（doc/24 §8）。
4. **折后 state**：`toSchemaMessages` 不拷贝 Usage → 自然退回全量 `countInput`，不要另做「清锚点」开关。
5. **锚点找错位置**：若在 `convertState` 之后找，Usage 已丢，会永远回退全量——功能「看起来能跑」但优化永不生效。单测必须钉「在原始 schema.Message 上找」。

---

## 8. 建议实现顺序

1. **探活（可先做、改动小）**  
   同一场先 `get_time`（或任意工具）再回答，迫使第二次模型调用。第二次调用前日志打印上一条 assistant 的 `PromptTokens` / `CompletionTokens`（至少一个 > 0）。对目录常用模型记一笔「Usage 是否稳定 / 是否有 ReasoningTokens」。确认字段再接线。

2. **滤思考（需求 1，独立可验）**  
   在 `BeforeModelRewriteState` 放行前清空 ReasoningContent。单测/断言：发出前锚点 assistant 字段为空。活体：思考模型二次调用不带上一枪思考（组件或抓包）。

3. **Usage 公式（需求 2）**  
   锚点查找 + `next_input` + 无账单回退。超限仍进现有 `foldInput`。日志建议：`compact usage` 打 baseline / delta / total，旁注全量自测；`window = 0` 时不出现。

4. **单测钉死（见下节）**

5. **活体 + 四端点同形**（未裁思考进 runlog 之前，字段面与改前同形）

---

## 9. 验收（摘自 doc/24 §9，实现完成标准）

### 探活

- 第二次模型调用前能读到上一条 assistant 的 Usage（至少一个 token 字段 > 0）。

### 单测

1. 有 Usage + 其后一条 tool 文本：`next_input` 等于公式；把思考再加进 delta 的变体更大（钉「可见输出只算一次」）。
2. 无 Usage：结果等于全量 `countInput`；函数无「传入 schema」参数。
3. 经 `toSchemaMessages` 重写后：退回全量 `countInput`。
4. 滤思考：发出前 state 上锚点 assistant 的 `ReasoningContent` 为空；请求侧不带该字段。

### 活体

- 第二次调用前可打 `compact usage`（baseline / delta / total / 旁注全量自测）。
- `window = 0` 时不出现折叠/usage 触发日志。
- 四端点路径与字段面与改前同形。

---

## 10. 参考文档

- 实现真源：`doc/24-用量计数.md`（全文；与本交接对齐）
- 指针：`doc/19-v6/19v6-07-compact.md` §1（复审项段落）
- 指针：`doc/19-v6/19v6-10-mid-run-fold.md` §6（schema 不计 + 指向 doc/24）
- 溢出层 usage 缺失 = 沉默盲区：`overflow.go` `detectTruncation`（已有先例，可对照）

---

## 11. 一句话收束

场内滤掉思考重发，治自测漏数；有账单时 mid-run 用 `PromptTokens + 可见 completion + 新行`，无账单仍全量 `countInput`；schema 不进触发；思考/Usage 默认不落库。主改 `midfold.BeforeModelRewriteState`，不动 `countInput` 基数与存储面。
