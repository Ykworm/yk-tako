# doc/ 文档索引

> 性质：**索引**，不是方案。本文只回答「这篇文档是什么、能不能照着施工、代码在哪、被谁取代」。
> 最后核对：2026-10-23。
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
- 改响应信封／业务码表：`doc/40`（规则在根 `AGENTS.md`，码表在 doc/40 §4）。
- 改契约字节、要重跑验收：`doc/19-v6/19v6-04`（人读版）、`scripts/acceptance/`（机器真源与断言器）、`yk-tako-eino/internal/runengine/contract_test.go`（快闸）。
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
| 16 | [能力复刻总规划](16-能力复刻总规划.md) | 🔶 | 全仓；分篇见本表 | 产品能力总纲。**2026-10-22 已按 doc/35–40 六篇重基线**：新增 §0.1 现状快照、§2 表补 5b（子代理持久档案与 resume）/ 9b（团队协作前端）/ 12（项目目录）/ 13（删除与档案清理）、§2c 横切契约（响应信封 doc/40、临时键 doc/38）、§5.1 当前待做；§6 索引补齐 doc/23、24、35–40 与 v6 文档集。🔶＝总纲自身仍列着 ⬜ 未做能力（产品外围） |
| 17 | [后续计划：代码子代理与记忆](17-后续计划-代码子代理与记忆.md) | 🔶 | `internal/runengine/notes.go`、`bash.go`、`agents/`、`web/` | 笔记／执行／停止／搜索／子代理折叠已落地；§5 由 doc/22 取代，§6 由 doc/25 取代 |
| 19-v6 | [运行引擎架构 v6（index）](19-v6/19-运行引擎架构-v6.md) | ✅ | `cmd/yktako/`、`internal/runengine/`、`web/` | v6 文档集总入口；取代 v5 |
| 19v6-01 | [契约：四个 API](19-v6/19v6-01-契约-四个API.md) | ✅ | `server.go`、`types.go`、`product_api.go` | 端点字节真源；含 SSE 端点（doc/31）的翻案标注。2026-10-21 校准：帧 kind 断言改七种（补 `usage` / `resume`）、session 响应补 `lastRunStatus`/`lastRunId`/`workspaceId` |
| 19v6-02 | [reconcile 与双游标](19-v6/19v6-02-reconcile与双游标.md) | ✅ | `server.go`、`redisstore.go`、`store.go` | 客户端流程与 `eventOffset` 语义 |
| 19v6-03 | [存储形态与生命周期](19-v6/19v6-03-存储形态与生命周期.md) | ✅ | `pgstore.go`、`redisstore.go`、`store.go` | 四表 + 两键（runlive / runepoch）、真源划分、多副本 |
| 19v6-04 | [验收（curl 手打）](19-v6/19v6-04-验收-curl.md) | ✅ | —（被测端点见 19v6-01）；机器版在 `scripts/acceptance/` | 改契约字节必须同步改本篇并重跑。2026-10-21 校准：session 期望字节追平现行代码，头注附实跑实录。**2026-10-22**：机器可重跑版已入库——机器真源 `scripts/acceptance/expected.json`、断言器 `assert_contract.py`、散文↔清单一致性 `check-expected.sh`；「重跑」从此是一句命令，快闸在 `internal/runengine/contract_test.go` |
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
| 25 | [子代理进程内折叠](25-子代理折叠.md) | ✅ | `midfold.go`、`agentfold_test.go`、`agents.go`（壳化装配） | 取代 doc/17 §6；A1–A3 已验收；2026-10-23 翻案落地壳化装配（per-delegation 重装的壳化变体：装载输入/观测面 run 级不动，实例每次委派新建，#1177 委派路径消除） |
| 26 | [researcher 的工具护栏](26-researcher工具护栏.md) | ✅ | `agents.go`、`agents/researcher.md`、`agents/coder.md` | `exclude_tools` 四态；排除清单只剩 `bash` |
| 27 | [用量可见化](27-用量可见.md) | ✅ | `product_api.go`、`server.go`、`worker.go`、`web/src/` | usage 帧／页面阈值／runlog 历史行 |
| 28 | [多租户运行时](28-多租户.md) | ✅ | `auth.go`、`identity_store.go`、`members_store.go`、`tenant_api.go`、`scope.go`、`pgstore.go` | 2026-10-21 收口。A0–A6 共 30 拍已落地并 curl 验收，随 `9c21b66` 入库。团队前端不在本篇（doc/36 已落地）；A7 那一档里「旧成功体换信封」已由 [doc/40](40-响应信封统一.md) 于 2026-10-22 落地，**计费、API key、SSO、组织共享、审计导出整批已拍不做**（2026-10-22 用户裁，与运行时关系不大的商业化、合规项，§15；2026-10-23 复核维持） |
| 29 | [长期记忆与对话召回](29-长期记忆.md) | ✅ | `memory.go`、`recall.go`、`dialogue.go`、`worker.go` | A1–B4 全落地；两条加工线共用 runlog。**2026-10-22**：召回块改进「本轮输入」并写回 mh 的 seq=1（runlog 仍纯原话）→ mid-run 折叠不再抹掉它；块带 created time；计入折叠预算。折叠链本身未改 |
| 30 | [崩溃续跑](30-崩溃续跑.md) | 🔶 | `journal.go`、`worker.go`、`resume_test.go`、`web/src/api.ts`、`web/src/chat.ts`、`web/src/App.vue` | A1–A5 已落地；A6 自动巡检 2026-10-21 裁为客户端触发式恢复（server 巡检不做）；**A8 前端 reattach 已落地并验收**（interrupted 琥珀提示 +「继续这一轮」按钮，2026-10-21）；A7 回归收口已跑（`go test -count=1` 25.3s 绿、vet 干净、`19v6-04` 矩阵重放全过）。**2026-10-22**：§10.1/§10.2/§10.4 的期望字节已随 [doc/40](40-响应信封统一.md) 换信封（404 `112002`、409 三态 `112102`/`112103`/`112104`） |
| 31 | [SSE 流式端点](31-sse流式端点.md) | ✅ | `server.go`、`redisstore.go`、`stream_test.go` | 同一帧面的第二种运输方式；拉是正确性、流是优化 |
| 32 | [media_offload（对话只存 path）](32-media-offload.md) | ✅ | `media.go`、`media_send.go`、`media_tokens.go`、`filetools.go`、`pdftext.go`、`pdftool.go`、`midfold.go`、`worker.go`、`guard.go`、`profile.go`、`compact.go`、`provider-catalog.json`、`media_test.go` | 2026-10-21 收口，两个半边都落地（存储纪律 A1–A2；模型可见 A3 五拍）。A1–A7 已落地（dcf3e6b）；A3-1 至 A3-5 已落地并活体验收（2026-10-20）。A3-5 按 DeepSeek v41 计价：800×800 为 422、100×100 为 184；窗口 940 上 814>752 进入折叠。A8-1 已施工并活体验收（2026-10-20，run `r_01M5CF1DFS9ZAD72GYQ5YXRYH8` complete，assistant 含 banana，user 行仍是占位，该 run 两表零 base64）。A8-2 边界已活体验收（2026-10-20）：第 3 页 run `r_01M5CH3GN1Y9KDKPMJ90R0NPCS` 含 orange；第 99 页 run `r_01M5CH3MM7HTQ2KDXA36YWW35M` 返回 pages=3 与超出范围；扫描件 run `r_01M5CH3RM7JY9GS0CX779Y5S0Y` 写明无文本层；音视频 run `r_01M5CH3WK609YQJBGFVCAQ4BDN` 上 `read_pdf_text` 拒绝非 PDF；`TAKO_MEDIA_SEND=off` 的 run `r_01M5CH5KCY1RQA6T7Z1QY0YW7Y` 仍读到 orange 且无 media send 行。这些 run 的两表零 base64。扫描件 OCR 不做。**2026-10-22**：media 的 404 按 [doc/40](40-响应信封统一.md) 拆成三个码（`114001` 项目看不见／`114101` 文件不存在／`114102` 路径不对），成功体仍是图片字节、不进信封 |
| 33 | [MCP client](33-mcp-client.md) | ✅ | `mcp.go`、`mcpconf.go`、`mcpconf_test.go` | stdio 本地 server；工具直接进池 |
| 34 | [租户运行时装配](34-租户运行时装配.md) | ✅ | `tenantmodel.go`、`agents.go`、`mcpconf.go`、`tenant_runtime_test.go`、`web/` | R1–R5＋U1 已落地并浏览器验收；commit 9c21b66 |
| 35 | [项目目录（workspace）](35-项目目录.md) | ✅ | `product_store.go`、`session_access.go`、`media.go`、`web/` | 2026-10-21 收口。A1–A8c 共 13 拍已落地并 curl 验收，随 e30d1f9 入库；路径方案取代 doc/32 的 `sessions/<sid>/` 版。第 0 节第 3 条「删除只删空项目」已由 doc/37 改写（2026-10-21 已落地）：有会话改为连带软删，`114002` 退役，活 run 用 `114006`，单场删除用 `114007` |
| 36 | [团队前端（切换器／成员／邀请）](36-团队前端.md) | ✅ | `web/`（`App.vue`、`MembersView.vue`、`InviteLanding.vue`、`PendingInvites.vue`、`auth.ts`）；G1/G3 小改在 `product_api.go`、`product_store.go` | 2026-10-21 已落地，随 `0fd15c5` 入库：切换器／成员页／邀请落地页与待处理邀请；收尾 build/test/vet 全绿 |
| 37 | [删除与档案清理](37-删除与档案清理.md) | ✅ | `pgstore.go`、`product_api.go`、`session_access.go`、`store.go`、`server.go`、`worker.go`、`archive_sweep.go`、`auth.go`、`cmd/yktako/main.go` | 2026-10-21 已落地。D1、D2 随 0fd15c5 入库；D3、D4 在 8905 curl 验收后入库。改写 doc/35「删除只删空项目」；单场删除仅属主；清扫器持 `cleaner_state` 租约，`-archive-ttl` 默认 168h，探针 `POST /api/dev/clean-sweep`。`114002` 退役 |
| 38 | [删除 tk:draft 键](38-删除draft键.md) | ✅ | `redisstore.go`、`store.go`、`server.go`、`worker.go`、`store_test.go` | 2026-10-20 已落地；定稿正文来自 turn，写口只认 runepoch |
| 39 | [子代理持久化与 resume](39-子代理持久化与resume.md) | ✅ | `agents.go`、`subagent_wire.go`、`resume_subagent.go`、`archive_sweep.go`、`worker.go`、`server.go` | 2026-10-21 已落地。S1 随 e476749 入库；S2、S3 在 8905 curl 验收后入库。每实例追加式 wire（`.tako/subagents/`，无 PG 表）+ `resume_subagent`（只认完整档案；不做半截重接，对齐 doc/30）。第三类清扫挂在 doc/37 同一轮，只删过窗文件 |
| 40 | [响应信封统一](40-响应信封统一.md) | ✅ | `auth.go`、`server.go`、`mcp.go`、`product_api.go`、`archive_sweep.go`、`envelope_test.go`、`web/src/api.ts`、`web/src/chat.ts` | 2026-10-22 落地，随 `cd85050` 入库。A7-1…A7-8 共 8 拍：后端六拍（curl 逐字节）+ 前端一拍（Playwright 活体）+ 收口一拍。runs／sessions／mcp／media／memory 五组旧接口整包换 `{code,message,data}`；**不动存储、不动帧格式、不动 HTTP 状态码**；media 的二进制成功体与 SSE 帧面不进信封。台账 `tmp/A7-信封迁移/`（不入库） |
| 41 | [任务看板与人工审批](41-任务看板与人工审批.md) | ✅ | `pii.go`、`pii_test.go`、`pii_mount_test.go`、`tasktools.go`、`tasktools_test.go`、`tools.go`、`midfold.go`、`memory.go`、`worker.go`、`pgstore.go`、`server.go`、`contract_test.go`、`store_test.go`、`web/src/{api.ts,chat.ts,App.vue}`、`scripts/acceptance/taskboard_crash.sh` | 2026-10-23 落地。**B（PII 脱敏）+ C（任务看板）两线全施工**；**Loop A（人审回路）整条不做**（bash 与 MCP 的越界都由沙箱围栏硬挡 → 没有需要挂起问人的场景，§3.1 与 §6 的 A1–A7 留档备查）。PII 三处挂载点：工具统一出口 `wrapToolErrors`（含 MCP）＋ 模型输入唯一转换口 `toSchemaMessages`（覆盖折叠后）＋ 记忆入库前（密钥拒收 / 证件打码）。看板：PG 表 `run_tasks` + 四把工具 + `kind=task` 帧（词表 7→8）+ 前端看板块；**活得过崩溃**（kill -9 → 重启 → resume，崩前建的行一条没丢且续跑里继续更新）。task 帧字节在 doc/19-v6-04「用例 8」登记为暂不断言（模型行为不稳）。对应 doc/16 §5.1 第 1、2 项与 §2 表 #2/#3 |
| 42 | [产品形态收窄与共享项目退役](42-产品形态收窄与共享项目退役.md) | ✅ | `pgstore.go`、`store.go`、`product_store.go`、`product_api.go`、`server.go`、`auth.go`、`filetools.go` | 2026-10-22 落地，随 `44cf7ca` 入库。形态定稿：tako = 多租户的 AI 工作台，**一个员工 = 一个项目**，**项目文件由多 session 共享**，团队共享账号/额度/审计、**不共享文件**（不做协同编辑）。退役两样：跨成员共享项目（可见性判定只剩属主；`PATCH /api/workspaces/:id/visibility` 端点留着路由恒回 404 `114001`；`114004` 退役）、项目闸（`one_live_run_per_workspace` 索引 DROP，同项目并发不再被 409 挡）。另：不再有游离会话——每场 run 自动挂默认项目 |
| 43 | [租户并发闸入 DB](43-租户并发闸入DB.md) | ✅ | `store.go`、`scope.go`、`server.go`、`product_api.go`、`store_test.go`、`scripts/acceptance/tenant_gate.sh` | 2026-10-23 落地。**租户并发上限**（`tenants.max_concurrent_runs`，默认 8）那道闸原来在 API 层「数一遍 → 比上限 → 放行」，数与放行是两次独立的事 → 并发下超发（活体实测：上限 2，并发四打进来 4 场）。**现在数与放行收进同一笔 PG 事务**，中间持一把所有副本共享的顾问锁（`pg_advisory_xact_lock`）→ 并发下不可能超发。**无 DDL、无存量迁移**、计数口径逐字不变（僵尸行不算活，不误拒）、**契约字节不变**（满租户仍回 429 `113001`）。回归网：存储层 `TestTenantGateConcurrent`（摘掉闸即红）+ 活体 `tenant_gate.sh`。对应 doc/16 §2c「闸在数据库」 |
| 44 | [前端三处欠账](44-前端三处欠账.md) | ✅ | `web/src/{api.ts,chat.ts,App.vue,ProjectsView.vue,ConfirmDialog.vue}` | 2026-10-23 落地。三处前端入口：**个人项目页**（列表／新建／改名／删除／项目下会话／开新对话）、**项目与单场会话删除 + 确认弹窗**（服务端 409 文案原样显示）、**当前会话的子代理列表与「继续」**（把带 `sa_id` 的指令追加进输入框，由父 agent 调 `resume_subagent`，不新增 HTTP 端点）。**后端零改动**。当前项目按租户存 `localStorage['tako.workspaceId.<tid>']`，`workspaceId` 只随新会话的 `POST /api/runs` 发送。对应 doc/16 §5.1 第 2 项与 §2 表 #5b/#12/#13 |
| 45 | [场景化 Eval 框架（方案）](45-场景化Eval框架.md) | ⬜（方案待点头） | —（未施工） | 2026-10-23 立项。给「**活有没有干完**」建一层可机器重跑的回归——前两层（快闸 `contract_test.go` + 契约 `scripts/acceptance/`）只断"接口有没有按约定回"，断不了"活干完了没有"（2026-10-23 PII 挂载点选错时两层全绿）。场景 = 准备/动作/判定/证据四件套；判定分 **A 硬断言 / B 模型判分（带抖动容忍）/ C 只登记不断言** 三档；语料 = 这一周手工验过的活体固化。**不改 `internal/runengine/` 生产代码**——Eval 站在引擎外面看它。预计落点 `scripts/eval/`（复用 `scripts/acceptance/harness.py`，不另起 HTTP 底座）。对应 doc/16 §2b 第 6 行与 §2c「验收可机器重跑」第三层 |
| 46 | [变更同步与事件通道（方案）](46-变更同步与事件通道.md) | ⬜（方案待点头） | —（未施工） | 2026-10-23 立项。多 tab 症状（落点失效 nit 开着）的根治 = 变更事件通道。**Loop A**：BroadcastChannel 同机广播 + 统一失效函数 `handleMutation`（半天，埋掉 nit 4）；**Loop B**：账号级 SSE 事件流 `GET /api/events`（Redis Stream `tk:events:<tid>` 第三个临时键；变更端点横切契约：删改成功必 `XADD`），失效函数同一份、广播与 SSE 两个源都喂它——天然是 doc/16 #8b（IM/cron 入口）的前置。纪律：事件只通知不带状态、真源永远重拉（doc/31「拉是正确性、流是优化」） |

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
   **动了产品能力的增减或状态翻转，同批还要更新 [doc/16](16-能力复刻总规划.md)**（§2 能力表 + §2c 横切契约 + 头注基线日期）——本索引是目录页，doc/16 是结论页，两个都得更，只更一边总纲的「现状」就是假的。收尾跑 `scripts/check-master-plan.sh`（比对本表 §1 的现行真源编号与 doc/16 的引用，漏引即失败）。
7. **`doc/` 与 `docs/` 的边界**：`doc/` 是现行方案与实现真源；`docs/` 是历史交接与文章；`tmp/` 是执行台账。三者不互相冒充。
