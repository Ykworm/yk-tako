# Handoff · 溢出修复层一期落地（A1 检出 → A2 救援链 → A3 截断检出 → A4 回归收口）（2026-10-12）

> 性质：**执行 handoff**——供接手会话直接开工。范围 = **溢出修复层一期**（19v6-09 §6/§7/§12/§13
> 的一期三件：检出① + run-start 救援 + 检出②）；**二期（mid-run 中途救援 + trimToolResults）与
> 三期（预留重裁）不在内**，停线与闸门见 §4。
> 触发：用户令「怎么告知另一个 CHAT 怎么做」→ 按 pre-T3 handoff 判例出本文件。
> 状态：**台账待用户过审**（tmp/19-v6/overflow/ 2026-10-12 拆出）——**用户把本 handoff 发给
> 接手会话即视为过审、可从 A1 开工**。
> 取信口径：§0 状态为 2026-10-12 当日实读（git/文件坐标在文内）；机制结论的权威源坐标都在 §5。
> **前置阅读（按序）**：`doc/19-v6/19v6-09-溢出修复层.md`（设计裁定真源，D1–D11 + §4 两张表照抄自它）
> → `tmp/19-v6/overflow/planning.md`（施工级裁定十条 + A1/A2 拆分明线论证）
> → `tmp/19-v6/overflow/todos.md`（**四要素权威原文——冲突以台账为准**；本文件是导览 + 操作要点）。

## 0. 一句话现状（2026-10-12 实读；动工当日重新 `git status` 复核）

- 代码 @ `a967d37`（mid-run fold A1–A5 + 19v6-09/10 doc 回填均已入库）；**树上未 commit 两批**：
  ① T3-4 的 doc 回填批次（doc/19-v6 多篇 + agents.go/compact.go/worker.go 等）；② 17-笔记 M1
  （notes.go + 若干 doc）。**开工前置 = 这两批先由用户裁归属**（commit 或等交审落定），A1 的
  diff 只许落在干净基线上——接手会话**动工第一步就是 `git status` 对照片刻状态**，若已干净则前置解除。
- mid-run fold（19v6-10）已收口：countInput 基数含 tool_calls JSON、foldInput 混合粒度、
  staged **列表**、foldBox、EndRun 吃 staged 列表 + mhRows（`8d938ee`）——**19v6-09 文内代码
  行号是立项时快照，施工一律以现役实读为准**。
- 施工顺序：**A1（检出①，只加观测行）→ A2（救援链）→ A3（检出② + run 末强折）→ A4（回归收口）**；
  逐拍停下交审、不连续推进、不自动 commit。

## 1. 硬约束与环境（动手前先过一遍）

- 硬闸门（AGENTS.md）：**逐 action 停下交审**；验收第一行必须是 curl/日志/psql 打跑着的东西；
  diff 目标 ≤80 行（A1 ≈150 / A2 ≈200 / A3 ≈120 含单测，已论证不拆，见子台账）。
- Go 不在 PATH：`export PATH="$HOME/sdk/go1.27.0/bin:$PATH"`；代码在 `yk-tako-eino/`。
- 起栈：引擎 `go run ./cmd/yktako -engine 127.0.0.1:8934`（缺省 redis 6390 / pg 5433；`.env` 在
  `yk-tako-eino/`）。**8934 起前 `lsof -i :8934` 查占**，被占换端口、不杀别人的进程；每拍结束
  杀自己起的引擎并 lsof 复核。
- **危险区**：6379 Redis 与 5432 PG 是 otel-ch-native 的活实例——全程不动。
- psql：`PGBIN=~/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin`，
  `$PGBIN/psql -h 127.0.0.1 -p 5433 -d tako`。
- 测试网 = 真实例：`go test ./internal/runengine/ -count=1`（需 6390 + 5433 已起）。
- 实测证据落 `tmp/19-v6/evidence-overflow/`。
- 话本校准纪律（塞历史三条出路：overflow 行=成活 / compact 行=减料 / complete=加料）见子台账
  planning 裁定 10；A1/A2 话本要真 provider 拒收，**不造假**。

## 2. 四个 action（权威四要素见 `tmp/19-v6/overflow/todos.md`；这里只给导览 + 已勘察的接线）

### 2.1 A1 · 检出①：overflow.go + detect 观测行（行为零变化）

- 改：`internal/runengine/overflow.go` 新文件（24+4 两张表先排除后命中 + `IsOverflowError` +
  `extractWindow`）；worker `event.Err` 分支加 detect 行（格式 = planning 裁定 2）；
  `prepareInput` 的 `total` 恒算进 foldBox.baseCount（裁定 3）。
- **接线已勘察**（worker.go，2026-10-12 实读）：`Run` L218 起；`it := w.agent.Run(...)` L282；
  `if event.Err != nil` 分支 L288-289（现役只打 `agent %s/%s: %v`——detect 行加在这，原行不动）；
  `prepareInput` L467：`total := countInput(...)` L475 现算现扔、`box.setBase(history)` L490。
- eino 组件错误文本形状（正则能不能匹配的唯一风险）**施工期实读钉 fixtures**：openai 系
  `~/go/pkg/mod/github.com/cloudwego/eino-ext/components/model/openai*/`、anthropic 系
  `.../anthropic*/`——看 provider HTTP 错误体怎么进 `.Error()`。

### 2.2 A2 · 救援链：零帧谓词 + 一次性闸 + 强折 + 重跑一次

- 改：L282 起的「agent.Run + 事件消费」整体包外层循环（裁定 8）；帧计数（AppendDelta/
  AppendFrame 成功 +1，不分 agent——裁定 7）；谓词四条件（doc §6）命中 → `overflowRecovered=true`
  → 重读 mh（`w.store.ModelMessages`）→ `foldInput` force 入口（compact.go）+ D4 目标线
  `limit' = min(真值×4/5, C_rej×4/5)` → foldBox 同步（裁定 6：base=折后行 + appendStaged——
  **不同步则 mid-run 钩子按旧 base 重建 state，救援白救**）→ rescue 行 → 重跑。
- 负路径：重试再拒 → 闸花 → 「已救过不再救」→ 原始错误走既有 error 出口（零写入，不发明第四态）。
- 话本同 A1（撒谎窗标 + 塞历史，新会话），期望全换：rescue 行 / frames 有 delta 且 complete /
  psql 被折行 compacted=f + seq0 / 同 sid 再 POST 活（A1 里这一步必死 = 两拍对照判据）。

### 2.3 A3 · 检出②：drain 读 ResponseMeta (a)(b) 子检 + run 末强折

- 改：drain 消费侧现只读 Content/ToolCalls（19v6-09 §15-Q1）——本拍起读 `ResponseMeta` 的
  PromptTokens/FinishReason/CompletionTokens 做 (a)(b) 两子检（(c) 不做，§5 登记）；
  命中 → `pendingForceFold{C: baseCount}` + 超窗行；**Run 尾部、defer EndRun 之前**（worker.go
  defer 链 LIFO 的语义先实读）mh 强折（limit' = C×4/5），成功进 foldBox 随 complete 落库，
  **失败不杀 run**（D7 不对称：答案已在手）。
- 话本：`-compact-window 4000`，正常问答两轮——12 工具的 schema（数千 tok 不计入估算，D11）
  把 provider 实数 prompt 顶过 4000 → 超窗行 + run 照常 complete + psql 强折落库 + 续投收敛。
  window=0 负路径 = 一行 overflow 日志都没有。

### 2.4 A4 · 回归网 + 收口

- 无新功能：m_d1 四端点 baseline 逐字节对照 midrun-A5 留档形状（帧/行带 model = T3-1b 恒在
  字段）；compact 既有单测 + C 系列话本回归（foldInput 老路径非 force 逐字节等价）；
  主台账 §2 行翻 ✅ + §10.6-④ 销账；二期/三期登记移交。commit 粒度归用户。

## 3. 已裁承重裁定（防重开论战）

| 裁定 | 内容 | 出处 |
|---|---|---|
| 日志三件套格式 | detect / rescue / 超窗三行全文已钉死 | 子台账 planning 裁定 2 |
| doc 一拍 → A1/A2 两拍 | 体量 + eino 错误形状风险先验活 | 子台账 planning 拆分明线论证 |
| window=0 本层全关 | ①②同生死（doc 只写了②，①补裁） | 子台账 planning 裁定 1 |
| C 经 foldBox.baseCount | 不恢复 prepareInput 返回值传递 | 子台账 planning 裁定 3 |
| staged 按列表语义登记 | 19v6-09「staged 变量替换」的现役读法 | 子台账 planning 裁定 4 |
| 重跑前同步 foldBox | 救一轮不救 state = 白救 | 子台账 planning 裁定 6 |
| D1–D11（检测①②/救援面/目标线/闸/落库/不救本答/二期三期/失败哲学/计数口径） | 全部已裁，无留待拍 | 19v6-09 §3 |
| 排除表先跑 | 限流误判成溢出 = 超窗请求重试到耗尽 | 19v6-09 §4 |
| (c) 子检暂缺 | 无自请 max_tokens 可比对，宁漏不误 | 19v6-09 §5 |

## 4. 停线（不在本 handoff 范围）

- **二期**：mid-run 中途拒收救援（WrapModel 装饰器 + ctx 救援句柄/run 槽位 + 钩子采用 +
  EndRun 槽位落库）+ trimToolResults 规则四件——依赖（19v6-10）已落地，**动工当日另拆四要素**。
- **三期**：预留重裁 `compactLimitFor`——**闸门 = 一期在真 provider 上真兜住过一次**（A2 话本
  实测记录就是动工凭证），禁只重裁不落地。
- 子代理溢出不救、(c) 子检、Usage 基线 + delta 计数（19v6-07 §1 复审项挂着）——均 doc §11 已裁。
- 四端点字节 / runlog / 闸 / 计数口径（D11）/ 帧 kind 全不动。

## 5. 素材坐标索引

| 素材 | 位置 |
|---|---|
| 设计裁定真源 | `doc/19-v6/19v6-09-溢出修复层.md`（§3 裁定一览 D1–D11；§4 两张表；§6/§7 两链；§12 话本） |
| 施工级裁定 | `tmp/19-v6/overflow/planning.md`（十条 + 拆分明线论证） |
| 四要素权威 | `tmp/19-v6/overflow/todos.md`（A1–A4 全文 + 通则） |
| 现引擎代码 | `yk-tako-eino/internal/runengine/worker.go`（Run L218 / agent.Run L282 / event.Err L288 / prepareInput L467）；`compact.go`（foldInput——A2 开 force 入口）；`midfold.go`（foldBox 容器——A2/A3 同步复用它的路径） |
| eino 错误包壳 | `~/go/pkg/mod/github.com/cloudwego/eino-ext/components/model/openai*/`、`.../anthropic*/`（A1 实读钉 fixtures） |
| 借鉴真源 | `ref-agent/WeKnora`（overflow.go 两张表 / think.go / engine.go / observe.go——19v6-09 实读版本） |
| 证据落点 | `tmp/19-v6/evidence-overflow/`（本 loop 新建）；midrun-A5 baseline 留档 = `tmp/19-v6/evidence-midrun/` + `tmp/19-v6/midrun-fold/` |
| 环境 | `scripts/dev.sh`；`yk-tako-eino/.env`；PG 5433 / Redis 6390 |

## 6. 纪律提醒（给接手会话）

- 本文件是导览不是裁定：机制级事实都有源坐标；**冲突以 doc/19v6-09 与子台账原文为准**。
- 承重论断先验前提（AGENTS.md）：文档注释、记忆、印象、已废代码都不是证据，证据只有代码、
  配置和用户原话。
- 每个 action 停下汇报等验收；不自动 commit；`tmp/` 台账不入 git。
- **动工第一步 = `git status`**：§0 的未 commit 两批（T3-4 / 17-笔记 M1）若仍在树上，先找用户
  裁归属，不要替别的会话 commit、也不要在其上叠 diff。
