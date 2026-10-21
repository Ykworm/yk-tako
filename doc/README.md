# doc/ 文档索引

> 性质：**索引**，不是方案。本文只回答「这篇文档是什么、能不能照着施工、代码在哪、被谁取代」。
> 最后核对：2026-10-21。
> 状态口径：✅ 已落地 · 🔶 部分落地／待收口 · ⬜ 未做 · 存档 = 历史材料，不作施工面。
> 代码路径均相对 `yk-tako-eino/`。

## 0. 先读哪篇

**第一次进仓库，按这个顺序：**

1. 根 `README.md`：怎么把引擎和前端跑起来。
2. 本索引：分清哪些文档能施工、哪些只是历史。
3. [`19-v6/19-运行引擎架构-v6.md`](19-v6/19-运行引擎架构-v6.md)：v6 文档集总入口。
4. 契约与存储四篇，按这个顺序读：
   - [`19v6-01-契约-四个API.md`](19-v6/19v6-01-契约-四个API.md)：四个端点的请求／响应／错误字节。
   - [`19v6-03-存储形态与生命周期.md`](19-v6/19v6-03-存储形态与生命周期.md)：四表 + 两键（runlive / runepoch）、真源划分、多副本。
   - [`19v6-02-reconcile与双游标.md`](19-v6/19v6-02-reconcile与双游标.md)：客户端轮询与游标。
   - [`19v6-04-验收-curl.md`](19-v6/19v6-04-验收-curl.md)：用例 → curl → 期望字节。

**按任务查：**

- 改运行引擎、API、存储：先读 19-v6 四篇，再查本表第 1 节。
- 加工具、改子代理：`19v6-05`、`19v6-08`、`doc/23`、`doc/25`、`doc/26`、`doc/39`。
- 改上下文压缩：`19v6-07`、`19v6-09`、`19v6-10`。
- 改登录、租户、会话归属：`doc/28`、`doc/34`、`doc/35`。
- 改团队前端（切换器／成员管理／邀请落地页）：`doc/36`。
- 改删除与档案清理（连带删会话、单场删除、保留窗口）：`doc/37`。
- 改记忆、召回：`doc/29`。
- 改崩溃续跑：`doc/30`。
- 改 Redis 临时键（移除 draft）：`doc/38`。
- 改流式端点：`doc/31`。
- 改媒体／附件：`doc/32`、`doc/35`。
- 接 MCP：`doc/33`。
- 看产品路线与优先级：`doc/16`。

**不要先读：** `doc/01`–`15`、`doc/19-v3`／`v4`／`v5`、`docs/`。这些是历史材料，不是现行施工面。

## 1. 现行真源（施工按这些）

| 编号 | 文档 | 状态 | 代码落点 | 说明／替代关系 |
|---|---|---|---|---|
| 16 | [能力复刻总规划](16-能力复刻总规划.md) | 🔶 | 全仓；分篇见本表 | 产品能力总纲。2026-10-20 已按 v6 重基线（状态、落点、旧线标注） |
| 17 | [后续计划：代码子代理与记忆](17-后续计划-代码子代理与记忆.md) | 🔶 | `internal/runengine/notes.go`、`bash.go`、`agents/`、`web/` | 笔记／执行／停止／搜索／子代理折叠已落地；§5 由 doc/22 取代，§6 由 doc/25 取代 |
| 19-v6 | [运行引擎架构 v6（index）](19-v6/19-运行引擎架构-v6.md) | ✅ | `cmd/yktako/`、`internal/runengine/`、`web/` | v6 文档集总入口；取代 v5 |
| 19v6-01 | [契约：四个 API](19-v6/19v6-01-契约-四个API.md) | ✅ | `server.go`、`types.go`、`product_api.go` | 端点字节真源；含 SSE 端点（doc/31）的翻案标注。2026-10-21 校准：帧 kind 断言改七种（补 `usage` / `resume`）、session 响应补 `lastRunStatus`/`lastRunId`/`workspaceId` |
| 19v6-02 | [reconcile 与双游标](19-v6/19v6-02-reconcile与双游标.md) | ✅ | `server.go`、`redisstore.go`、`store.go` | 客户端流程与 `eventOffset` 语义 |
| 19v6-03 | [存储形态与生命周期](19-v6/19v6-03-存储形态与生命周期.md) | ✅ | `pgstore.go`、`redisstore.go`、`store.go` | 四表 + 两键（runlive / runepoch）、真源划分、多副本 |
| 19v6-04 | [验收（curl 手打）](19-v6/19v6-04-验收-curl.md) | ✅ | —（被测端点见 19v6-01） | 改契约字节必须同步改本篇并重跑。2026-10-21 校准：session 期望字节追平现行代码，头注附实跑实录 |
| 19v6-05 | [tool loop（T 系列）](19-v6/19v6-05-tool-loop.md) | ✅ | `tools.go`、`agents.go`、`agentlog.go`、`midfold.go`、`toolsearch.go`、`profile.go` | T1–T3 已落地；T4/T5（分级 + BM25 `tool_search`）2026-10-20 落地（A1 `ad1ab6b`、A2 `a190f4d`；A3 寿命验收、A4 回归收口） |
| 19v6-06 | [工具分级与 deferred tools](19-v6/19v6-06-deferred-tools与缓存机制.md) | ✅ | —（纯机制背景篇，无代码落点） | 非契约篇；client mode 已是终态底座，hosted mode 2026-10-21 裁定不接；厂商矩阵与链路留作背景 |
| 19v6-07 | [compact（v6 形态）](19-v6/19v6-07-compact.md) | ✅ | `compact.go`、`compact_test.go` | C1–C4 已落地 |
| 19v6-08 | [文件工具族（F 系列）](19-v6/19v6-08-file-tools.md) | ✅ | `fileops.go`、`filetools.go`、`media.go` | 文件工具机制与契约真源 |
| 19v6-09 | [溢出修复层](19-v6/19v6-09-溢出修复层.md) | ✅ | `overflow.go`、`overflow_test.go` | 一期／二期／三期均已落地 |
| 19v6-10 | [mid-run fold](19-v6/19v6-10-mid-run-fold.md) | ✅ | `midfold.go`、`midfold_test.go` | run 中途压缩；A5 已验收 |
| 19v6-90 | [演进记录](19-v6/19v6-90-演进记录.md) | ✅ | —（修订记录） | 只追加不篡改；契约本身看 19v6-01 |
| 21 | [同会话历史查找](21-同会话历史查找.md) | ✅ | `search.go`、`search_test.go` | H1–H5 落地；`search_history` 工具 |
| 22 | [网络搜索](22-网络搜索.md) | ✅ | `websearch.go`、`websearch_test.go` | 已切 DeepSeek `/v1/tools/search`；结果始终落盘再给描述段 |
| 23 | [coder 规格](23-coder规格.md) | ✅ | `agents/coder.md`、`agents.go` | §2 已逐字落盘；`tools:` 白名单挡 `search_history`／`web_search` |
| 24 | [用量账单、滤思考与折叠触发](24-用量计数.md) | ✅ | `compact.go`、`agentlog.go`、`worker.go` | 计数、滤思考、Usage mid-run 触发 |
| 25 | [子代理进程内折叠](25-子代理折叠.md) | ✅ | `midfold.go`、`agentfold_test.go` | 取代 doc/17 §6；A1–A3 已验收 |
| 26 | [researcher 的工具护栏](26-researcher工具护栏.md) | ✅ | `agents.go`、`agents/researcher.md`、`agents/coder.md` | `exclude_tools` 四态；排除清单只剩 `bash` |
| 27 | [用量可见化](27-用量可见.md) | ✅ | `product_api.go`、`server.go`、`worker.go`、`web/src/` | usage 帧／页面阈值／runlog 历史行 |
| 28 | [多租户运行时](28-多租户.md) | ✅ | `auth.go`、`identity_store.go`、`members_store.go`、`tenant_api.go`、`scope.go`、`pgstore.go` | 2026-10-21 收口。A0–A6 共 30 拍已落地并 curl 验收，随 `9c21b66` 入库。团队前端不在本篇（doc/36 已落地）；A7 那一档（旧成功体换信封、计费、API key、SSO）未开工 |
| 29 | [长期记忆与对话召回](29-长期记忆.md) | ✅ | `memory.go`、`recall.go`、`dialogue.go` | A1–B4 全落地；两条加工线共用 runlog |
| 30 | [崩溃续跑](30-崩溃续跑.md) | 🔶 | `journal.go`、`worker.go`、`resume_test.go`、`web/src/api.ts`、`web/src/chat.ts`、`web/src/App.vue` | A1–A5 已落地；A6 自动巡检 2026-10-21 裁为客户端触发式恢复（server 巡检不做）；**A8 前端 reattach 已落地并验收**（interrupted 琥珀提示 +「继续这一轮」按钮，2026-10-21）；A7 回归收口已跑（`go test -count=1` 25.3s 绿、vet 干净、`19v6-04` 矩阵重放全过）。残余：`19v6-04` 的期望字节停在 doc/28 之前，待补 |
| 31 | [SSE 流式端点](31-sse流式端点.md) | ✅ | `server.go`、`redisstore.go`、`stream_test.go` | 同一帧面的第二种运输方式；拉是正确性、流是优化 |
| 32 | [media_offload（对话只存 path）](32-media-offload.md) | ✅ | `media.go`、`media_send.go`、`media_tokens.go`、`filetools.go`、`pdftext.go`、`pdftool.go`、`midfold.go`、`worker.go`、`guard.go`、`profile.go`、`compact.go`、`provider-catalog.json`、`media_test.go` | 2026-10-21 收口，两个半边都落地（存储纪律 A1–A2；模型可见 A3 五拍）。A1–A7 已落地（dcf3e6b）；A3-1 至 A3-5 已落地并活体验收（2026-10-20）。A3-5 按 DeepSeek v41 计价：800×800 为 422、100×100 为 184；窗口 940 上 814>752 进入折叠。A8-1 已施工并活体验收（2026-10-20，run `r_01M5CF1DFS9ZAD72GYQ5YXRYH8` complete，assistant 含 banana，user 行仍是占位，该 run 两表零 base64）。A8-2 边界已活体验收（2026-10-20）：第 3 页 run `r_01M5CH3GN1Y9KDKPMJ90R0NPCS` 含 orange；第 99 页 run `r_01M5CH3MM7HTQ2KDXA36YWW35M` 返回 pages=3 与超出范围；扫描件 run `r_01M5CH3RM7JY9GS0CX779Y5S0Y` 写明无文本层；音视频 run `r_01M5CH3WK609YQJBGFVCAQ4BDN` 上 `read_pdf_text` 拒绝非 PDF；`TAKO_MEDIA_SEND=off` 的 run `r_01M5CH5KCY1RQA6T7Z1QY0YW7Y` 仍读到 orange 且无 media send 行。这些 run 的两表零 base64。扫描件 OCR 不做 |
| 33 | [MCP client](33-mcp-client.md) | ✅ | `mcp.go`、`mcpconf.go`、`mcpconf_test.go` | stdio 本地 server；工具直接进池 |
| 34 | [租户运行时装配](34-租户运行时装配.md) | ✅ | `tenantmodel.go`、`agents.go`、`mcpconf.go`、`tenant_runtime_test.go`、`web/` | R1–R5＋U1 已落地并浏览器验收；commit 9c21b66 |
| 35 | [项目目录（workspace）](35-项目目录.md) | ✅ | `product_store.go`、`session_access.go`、`media.go`、`web/` | 2026-10-21 收口。A1–A8c 共 13 拍已落地并 curl 验收，随 e30d1f9 入库；路径方案取代 doc/32 的 `sessions/<sid>/` 版。第 0 节第 3 条「删除只删空项目」已由 doc/37 改写（2026-10-21 已落地）：有会话改为连带软删，`114002` 退役，活 run 用 `114006`，单场删除用 `114007` |
| 36 | [团队前端（切换器／成员／邀请）](36-团队前端.md) | ✅ | `web/`（`App.vue`、`MembersView.vue`、`InviteLanding.vue`、`PendingInvites.vue`、`auth.ts`）；G1/G3 小改在 `product_api.go`、`product_store.go` | 2026-10-21 已落地，随 `0fd15c5` 入库：切换器／成员页／邀请落地页与待处理邀请；收尾 build/test/vet 全绿 |
| 37 | [删除与档案清理](37-删除与档案清理.md) | ✅ | `pgstore.go`、`product_api.go`、`session_access.go`、`store.go`、`server.go`、`worker.go`、`archive_sweep.go`、`auth.go`、`cmd/yktako/main.go` | 2026-10-21 已落地。D1、D2 随 0fd15c5 入库；D3、D4 在 8905 curl 验收后入库。改写 doc/35「删除只删空项目」；单场删除仅属主；清扫器持 `cleaner_state` 租约，`-archive-ttl` 默认 168h，探针 `POST /api/dev/clean-sweep`。`114002` 退役 |
| 38 | [删除 tk:draft 键](38-删除draft键.md) | ✅ | `redisstore.go`、`store.go`、`server.go`、`worker.go`、`store_test.go` | 2026-10-20 已落地；定稿正文来自 turn，写口只认 runepoch |
| 39 | [子代理持久化与 resume](39-子代理持久化与resume.md) | ✅ | `agents.go`、`subagent_wire.go`、`resume_subagent.go`、`archive_sweep.go`、`worker.go`、`server.go` | 2026-10-21 已落地。S1 随 e476749 入库；S2、S3 在 8905 curl 验收后入库。每实例追加式 wire（`.tako/subagents/`，无 PG 表）+ `resume_subagent`（只认完整档案；不做半截重接，对齐 doc/30）。第三类清扫挂在 doc/37 同一轮，只删过窗文件 |

## 2. 历史存档（`doc/`，不作施工面）

| 编号 | 文档 | 状态 | 代码落点 | 说明 |
|---|---|---|---|---|
| 01 | [任务看板与单入口](01-任务看板与单入口.md) | 存档 | —（旧线 `internal/taskboard` 已删） | 2026-09-26 单入口定案；v6 未重建 Task 工具 |
| 02 | [任务看板实现计划](02-任务看板实现计划.md) | 存档 | —（旧线代码已删） | 旧执行台账 |
| 03 | [立项与命名](03-立项与命名.md) | 存档 | —（记录） | 项目立项／命名 |
| 04 | [技术路线](04-技术路线.md) | 存档 | —（记录） | Go＋Eino 路线决策；LangGraph→Eino 映射表 |
| 05 | [Octop 架构剖析与生长式复刻](05-Octop架构剖析与生长式复刻.md) | 存档 | —（参考） | “要什么”的背景地图，仍可读 |
| 06 | [工具安全层](06-工具安全层.md) | 存档 | `guard.go`（v6 继承三态守卫） | 旧线能力记录 |
| 07 | [任务规划与看板](07-任务规划与看板.md) | 存档 | —（v6 未重建 Task 工具） | 旧线能力记录 |
| 08 | [Eino 规划机制调研](08-Eino规划机制调研.md) | 存档 | —（调研） | `write_todos`／`PlanTask`／Workflow 对照 |
| 09 | [设计了解（Eino 机制笔记）](09-设计了解.md) | 存档 | —（笔记） | 面试材料与设计笔记；修改需用户同意 |
| 10 | [最小 Web UI（方案 A 定案）](10-最小Web-UI.md) | 存档 | —（旧线 `internal/webui` 已删） | 旧线定案 |
| 11 | [最小 Web-UI 实施计划](11-最小Web-UI实施计划.md) | 存档 | —（旧线代码已删） | 旧线执行台账 |
| 12 | [上下文压缩](12-上下文压缩.md) | 存档 | `compact.go`（v6 重写） | 旧线算法；v6 真源见 19v6-07 |
| 13 | [上下文压缩实施计划](13-上下文压缩实施计划.md) | 存档 | —（旧线代码已删） | 旧线执行台账 |
| 15 | [页面从 event_log 读对话的实施计划](15-页面从event_log读对话的实施计划.md) | 存档 | —（旧线代码已删） | 旧线 Web UI 实施计划 |
| 18 | [媒体落盘与图片读取](18-媒体落盘与图片读取.md) | 存档 | —（旧线 `internal/mediaoffload` 已删） | 旧线实现存档；v6 真源见 doc/32、doc/35 |
| 19-v3 | [运行引擎架构 v3](19-运行引擎架构-v3.md) | 存档 | —（历史架构） | 投递与执行解耦的早期方案 |
| 19-v4 | [运行引擎架构 v4](19-运行引擎架构-v4.md) | 存档 | —（历史架构） | 已被 v5 取代 |
| 19-v5 | [运行引擎架构 v5](19-v5/19-运行引擎架构-v5.md) | 存档 | —（历史架构） | 已被 v6 取代 |
| 19-v5/7.9 | [业务用例链路](19-v5/7.9-业务用例链路.md) | 存档 | —（历史架构） | v5 附篇 |
| XX | [taskboard 重命名 handoff](XX-taskboard-重命名-handoff.md) | 存档 | —（命名 handoff） | 现行统称 tasktracker；本文是当时的决策记录 |
| handoff-doc24 | [滤思考与 Usage 触发交接](handoff-doc24-滤思考与Usage触发.md) | 存档 | —（交接） | doc/24 的交接稿；真源已并入 doc/24 |

## 3. 非方案内容

| 位置 | 文档 | 状态 | 说明 |
|---|---|---|---|
| `doc/blog/` | [叶子才是根：被 Eino 中断契约坑掉的一天](blog/被Eino坑的一天.md) | 非方案 | 工程故事；不作施工面 |
| `doc/interview/` | [会话级并发：从 409 到收件箱](interview/20-会话级并发-从409到收件箱.md) | 非方案 | 访谈稿＋当时的设计草稿；其中 409 诊断是旧现状，v6 已改为收件箱模型 |
| `doc/interview/` | [面试准备：资深后端与 AI 应用后端](interview/面试准备-资深后端与AI应用后端.md) | 非方案，准备初稿 | 2026-10-21 核对相关实现；介绍练习稿、三段工程故事、追问与七天准备安排；个人职责和实验结果待补，不作施工真源 |
| 仓库根 | [进度误会.md](../进度误会.md) | 非方案 | 工程纪律与协作复盘；不属 doc/ 方案体系 |

## 4. `docs/` 历史交接与文章

`docs/` 只放**某一时刻的交接、执行计划与文章**，不作为施工真源；需要了解“当时为什么这么做”时再读。

| 日期 | 文档 | 类型 | 说明 |
|---|---|---|---|
| 2026-09-30 | [未提交改动审查交接](../docs/2026-09-30-uncommitted-review-handoff.md) | 交接 | 旧线未提交改动审查 |
| 2026-10-07 | [v6 现状与能力差距](../docs/2026-10-07-v6-gap-handoff.md) | 交接 | v6 现状快照与 pi／WeKnora 差距 |
| 2026-10-08 | [pre-T3 落地](../docs/2026-10-08-pre-t3-handoff.md) | 执行交接 | T3 之前的 lease／T1／T2 |
| 2026-10-08 | [子代理设计素材](../docs/2026-10-08-subagent-design-handoff.md) | 素材盘点 | 子代理设计的已核实素材 |
| 2026-10-09 | [Deferred loading：把工具 schema 移出前缀](../docs/2026-10-09-deferred-loading.md) | 文章 | 前缀缓存与工具 schema |
| 2026-10-12 | [溢出修复层一期落地](../docs/2026-10-12-overflow-handoff.md) | 执行交接 | 检出／救援链／截断检出 |
| 2026-10-16 | [溢出修复层二期 B2/B3](../docs/2026-10-16-overflow-b2b3-handoff.md) | 执行交接 | guard 救援链＋槽位落库 |
| 2026-10-19 | [多租户运行时现状交接](../docs/2026-10-19-多租户-handoff.md) | 现状交接 | 多租户全改后代码停在哪 |
| 2026-09-26 | [MAF 最小审批回路 Implementation Plan](../docs/superpowers/plans/2026-09-26-maf-tool-approval.md) | 旧线计划 | MAF 对照，冻结存档 |
| 2026-09-26 | [最小 Web-UI T2：HTTP + SSE](../docs/superpowers/plans/2026-09-26-web-ui-http-sse.md) | 旧线计划 | 旧线 Web UI |
| 2026-09-26 | [最小 Web-UI T1：事件 sink 重构](../docs/superpowers/plans/2026-09-26-web-ui-sink.md) | 旧线计划 | 旧线 Web UI |
| 2026-09-27 | [T2 人工验收用例](../docs/superpowers/plans/2026-09-27-t2-人工验收用例.md) | 旧线验收 | 旧线 Web UI |
| 2026-09-29 | [Web UI：页面读内存列表](../docs/superpowers/plans/2026-09-29-webui-memory-read.md) | 旧线计划 | 旧线 Web UI |
| 2026-09-29 | [流式枢纽人工验收](../docs/superpowers/plans/2026-09-29-webui-stream-hub-manual.md) | 旧线验收 | 旧线 Web UI |
| 2026-09-29 | [Web UI 流式枢纽 Implementation Plan](../docs/superpowers/plans/2026-09-29-webui-stream-hub.md) | 旧线计划 | 旧线 Web UI |
| 2026-09-30 | [WebUI net/http → Gin 迁移](../docs/superpowers/plans/2026-09-30-webui-gin-migration.md) | 旧线计划 | 旧线 Web UI |
| 2026-09-26 | [MAF 最小审批回路设计](../docs/superpowers/specs/2026-09-26-maf-tool-approval-design.md) | 旧线设计 | MAF 对照，冻结存档 |
| 2026-09-28 | [Web UI 流式枢纽设计 v3](../docs/superpowers/specs/2026-09-28-webui-stream-hub-design.md) | 旧线设计 | 落盘＋seq 续传 |

## 5. 新增／修改文档的规则

1. **编号与位置**
   - 主系列：`doc/NN-短标题.md`，编号递增，不复用已删编号。
   - v6 子系列：`doc/19-v6/19v6-NN-短标题.md`；index 与演进记录分别是 `19-运行引擎架构-v6.md`、`19v6-90-演进记录.md`。
   - 交接／文章：`docs/YYYY-MM-DD-短标题.md`；执行台账：`tmp/<方案名>/`（本地，不入库）。
2. **头注必须写四件事**：性质（方案／实现真源／讨论稿／存档）、状态（✅／🔶／⬜）、日期、代码落点。写了“未定案”的文档不能被当成施工真源引用。
3. **替代关系写在头注**：新文档取代旧文档时，旧文档头注加“已被 X 取代”，新文档写“取代 Y”；不删除旧文。
4. **同一机制只留一份真源**：其他文档只写指针，不复制条款；发现两处冲突，先改真源，再改指针。
5. **代码落点必须是真实路径**：没有代码就写 `—（文档／验收／记录）`，不要写已删除的包。
6. **新增或改状态时同步本索引**：同一提交里更新对应行；纯错别字修正除外。
7. **`doc/` 与 `docs/` 的边界**：`doc/` 是现行方案与实现真源；`docs/` 是历史交接与文章；`tmp/` 是执行台账。三者不互相冒充。
