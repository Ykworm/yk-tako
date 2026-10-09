# Handoff · pre-T3 落地（lease 小拍 → T1 → T2）（2026-10-08）

> 性质：**执行 handoff**——供接手会话直接开工。范围 = **T3 之前的三个任务**（lease 小拍 → T1 → T2）；
> **T3 本体不在内**，停线 = T3 动工前必拍三项（§4）。
> 触发：用户 2026-10-08「我在另一个会话去落地 t3 之前的任务」；随本文件同批落了两处台账补登——
> todos「T 系列」节（lease 小拍四要素 + T3 动工前必拍三项）与 planning §2/§11 镜像。
> 取信口径：全部为 2026-10-08 当日实读（代码坐标在文内）；机制结论均带源坐标、可直接复查。
> **前置阅读（按序）**：`doc/19-v6/19v6-05-tool-loop.md`（机制裁定真源）→ `tmp/19-v6/planning.md` §11（施工规格）
> → `tmp/19-v6/todos.md`「T 系列」节（四要素权威原文——**冲突以台账为准**；本文件是导览 + 操作要点）。

## 0. 一句话现状

- master @ `4904d39`；D1/C1 已验收、已 commit（`f09deb6`）；A 系列全销账。
- **未 commit = T 立项 doc 批次**：`doc/19-v6/19v6-05-tool-loop.md` + `19v6-06-deferred-tools与缓存机制.md`（新增）
  + index 修订；2026-10-08 晚另新增本 handoff（`docs/`）。**commit 粒度归用户——不自动 commit。**
- 2026-10-08 用户裁：T 系列立项无修改意见；**pre-T3 三项转本会话落地**，逐拍停下交审。
- 施工顺序：**lease 小拍（前置）→ T1（ADK 接入 + 观测架）→ T2（get_time）→ 停**（T3 动工前必拍三项见 §4）。

## 1. 硬约束与环境（动手前先过一遍）

- 硬闸门（AGENTS.md）：四要素已登记、**逐 action 停下交审**、不自动 commit；验收第一行必须是 curl/psql 打跑着的东西。
- Go 不在 PATH：`export PATH="$HOME/sdk/go1.27.0/bin:$PATH"`；代码在 `yk-tako-eino/`。
- 起栈：`./scripts/dev.sh`（前台；自举 Redis 6390 + PG 5433 + 前端 vite；`restart`/`stop`/`status` 只动自己的 pidfile）。
  单起引擎：`go run ./cmd/yktako -engine 127.0.0.1:<E>`（缺省 redis 6390 / pg 5433；`.env` 在 `yk-tako-eino/`，查找链 `.env` → `../.env`）。
- **危险区**：6379 Redis 与 5432 PG 是 otel-ch-native 的活实例——全程不动；端口被占先 `lsof` 查是谁，换端口，不杀别人的进程。
- 测试网 = 真实例：`go test ./internal/runengine/ -count=1`（需 PG 5433 + Redis 6390 已起；独立 db1 + 临时库，不碰 dev 数据；环境不在会大声失败）。
- 历史实测值（租约 30s / 续约 5s）在 A4-1/A7 记录里是当时事实，**不改**——新默认落 main.go 后以代码为准。

## 2. 三个 action（权威四要素见 todos「T 系列」节；这里只给导览 + 已勘察的接线）

### 2.1 前置小拍 · lease 参数（5s→10s + `.env` 化）

- 改：`defaultLeaseRenew` 5s→10s；`-lease-ttl`/`-lease-renew` 默认值读 `.env` 的
  `TAKO_LEASE_TTL`/`TAKO_LEASE_RENEW`（未设置回落 30s/10s；显式旗标压 .env）。
- **接线已勘察**：`LoadDotEnv()` 现在在 `config.Open()` 里调，而 `flag.Parse()` 在 main L42、`config.MustOpen` 在 L47——
  要把 `config.LoadDotEnv()` 提到 `flag.Parse()` **之前**先调一次；解析函数放 config 包还是 main 随动工定（引擎参数入 config 首例）。
- **连带**：main.go L20 注释「30s/5s = 连续 6 次续约…」算数改 3 次；「renew ≥ ttl 是否 fail fast」动工时裁（.env 手误是新引入的面）。
- 验收六步（启动日志 / psql `lease_until` / 续约现场 / kill -9 自愈 / 旗标压 .env / 单测）见 todos；
  续约现场用「写数百字」类请求造十余秒生成窗口，`-lease-ttl 60s -lease-renew 5s` 显式压时间轴。

### 2.2 T1 · ADK 接入 + Handler 观测架

- 改：worker 手写 Stream 循环 → `adk.NewChatModelAgent`（零工具，复用 config 的 ChatModel）；
  ADK 事件流 → v6 帧的翻译进归一层；挂全套中间件钩子终端日志 `[adk][序号] 钩子名｜功能一句话｜进/出`。
  watchdog/endRun/闸路径不动；**铁判据 = 四 API 契约字节零变化**。
- 现循环坐标：`internal/runengine/worker.go`（TranslatorFor L35、NewWorker L88）。
- Eino 坐标：`~/go/pkg/mod/github.com/cloudwego/eino@v0.9.21/adk/`（`chatmodel.go` 的 ChatModelAgent、
  `middlewares/` 钩子、`agent_tool.go` 留 T3）。
- 验收：钩子日志按序七类全覆盖 / curl 主链与 D1 baseline 同形（202 字段序、frames 序列、收口、log 两行）/ `go test` 全绿。
- D1 baseline 证据：todos「D 系列」节末 + `tmp/19-v6/evidence-d1/`。

### 2.3 T2 · get_time（帧契约 + mh 两列 + runlog 两列）

- 改（**先改 doc 再动码**）：19v6-01 增 tool 帧 kind（建议 `tool_call`/`tool_result`，字段名 T2 前排定）；
  mh 加 `tool_calls`/`tool_call_id`（AutoMigrate）；runlog 加 `agent`（本拍恒空）+ `payload`；
  worker 注册 `get_time`（无副作用、可 curl 造）；归一层补 tool 帧翻译。
- doc 同批落点四处（已补登 T2 节）：19v6-01 / 19v6-04（加 tool 帧用例与期望字节）/ 19v6-90 / index 引用处如有。
- 验收：curl「现在几点」→ frames 含 tool 帧序；psql mh 两行（assistant.tool_calls + tool.tool_call_id 回指）；
  log 扩展行（payload 非空、agent 空）；**同会话追投「我刚才问的几点」→ 模型凭 tool 结果答出**。

## 3. 已裁承重裁定（防重开论战；出处 = 19v6-05 / planning §11.1 / todos 2026-10-08 已裁块）

| 裁定 | 内容 |
|---|---|
| 底座 | 循环走 **Eino ADK**（用户拍板） |
| sub agent 落地 | **`adk.NewAgentTool` 自组装**，不用 `prebuilt/deep` 整包（捆绑 write_todos/FS/Shell + prompt 哲学） |
| 观测 | Handler 全套钩子终端日志（脚手架，转正点 T6） |
| 工具分级 | node 全注册 + `BeforeModelRewriteState` 声明过滤（direct/deferred/hidden） |
| tool_search | T4 直出 / **T5 BM25 = 独立 task**；任何 agent 只搜自己池的 deferred 项 |
| 子代理 | Model message 进程内、跑完即弃；帧带 `agent`；runlog 消息粒度；并行不改表 |
| 规格 tools | 两态：白名单 / 白名单+search（用户裁「当然要」） |
| fork | 后面再做（copy-on-write 策略已备，§11.4） |
| 挂账（用户裁「先别搞」） | 子代理目录运行时安装、运行中途改工具集 |

## 4. 停线：T3 动工前必拍三项（已写进 todos T3 节）

① **规格字段表**（name/description/tools/model 最小集；**model 继承 or 覆盖未落笔**）；
② **预算面明账**（并发上限 / 超时 / 回传裁剪；pi 参考 8 任务 / 4 并发 / 并行回传裁 50KB）；
③ **规格库 loop（候选）**（文件式规格 / 发现时机 / 信任门）——已登记 planning §2「前方未开工登记项」。

到 T3 动工前先把三项拍掉；**T3 本体不在本 handoff 范围**。

## 5. 素材坐标索引

| 素材 | 位置 |
|---|---|
| 机制裁定真源 | `doc/19-v6/19v6-05-tool-loop.md`；`19v6-06-deferred-tools与缓存机制.md`（缓存 / 延迟工具） |
| 契约真源 | `doc/19-v6/19v6-01-契约-四个API.md`（帧 / 端点字节）、`19v6-04-验收-curl.md`（期望字节） |
| 施工规格 | `tmp/19-v6/planning.md` §11 |
| 四要素权威 | `tmp/19-v6/todos.md`「T 系列」节（lease 小拍 + T1–T6） |
| Eino | `~/go/pkg/mod/github.com/cloudwego/eino@v0.9.21/adk/`（chatmodel.go / middlewares/ / agent_tool.go） |
| 现引擎代码 | `yk-tako-eino/internal/runengine/worker.go`（循环 / translator）；`yk-tako-eino/cmd/yktako/main.go`（旗标 / 租约注释） |
| 环境 | `scripts/dev.sh`；`yk-tako-eino/.env`；`internal/runengine/store_test.go`（真实例网） |
| D1 证据 | `tmp/19-v6/evidence-d1/`；todos「D 系列」节末 |
| 参考实现 | `ref-agent/pi`（subagent 扩展示例 + `src/experimental/durable/`）、`ref-agent/deepseek-harness` |

## 6. 纪律提醒（给接手会话）

- 本文件是导览不是裁定：机制级事实都有源坐标；**冲突以 doc / 台账原文为准**。
- 承重论断先验前提（AGENTS.md）：文档注释、记忆、印象、已废代码都不是证据，证据只有代码、配置和用户原话。
- 每个 action 停下汇报等验收；diff 目标 ≤80 行；不自动 commit。
- `tmp/` 台账不入 git；`doc/19-v6/` 与 `docs/` 的未 commit 批次，commit 粒度归用户。
