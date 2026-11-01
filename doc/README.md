# doc/ 文档索引

> 性质：**索引**，不是方案。本文只回答「这篇文档是什么、能不能照着施工、代码在哪、被谁取代」。
> 最后核对：2026-11-01。
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
- 改界面可用性（markdown 渲染／会话历史入口／停止按钮／文件可达／破坏性操作与文案）：`doc/48`。
- 改侧栏项目树（项目 → 对话二级、10 条上限、展示全部）：`doc/50`。
- 改视觉（配色／字阶／间距／圆角／侧栏三段／手机抽屉／气泡）：`doc/51`。
- 加技能、改技能目录／加载／依赖／管理面：`doc/52`。
- 改删除与档案清理（连带删会话、单场删除、保留窗口）：`doc/37`。
- 改记忆、召回：`doc/29`。
- 改崩溃续跑：`doc/30`。
- 改 Redis 临时键（移除 draft）：`doc/38`。
- 改流式端点：`doc/31`。
- 改媒体／附件：`doc/32`、`doc/35`。
- 接 MCP：`doc/33`。
- 看产品路线与优先级：`doc/16`。
- 不知道某块代码在哪、归哪个模块：`doc/53`（模块地图）。
- 想知道「agent 怎么测、公开基准选哪个、哪些不该跑」：`doc/56`。
- 改网络抓取（`web_fetch`）：`doc/57`。
- 改读文件（`read_file` 怎么分页）或 bash 大输出（留尾／全文落盘）：`doc/58`。
- 想知道某个设计当时为什么这么裁、代价是什么：`doc/problems/`（问题档案，按模块分组）。

**不要先读：** `doc/01`–`15`、`doc/19-v3`／`v4`／`v5`、`docs/`。这些是历史材料，不是现行施工面。

## 1. 现行真源（施工按这些）

| 编号 | 文档 | 状态 | 代码落点 | 说明／替代关系 |
|---|---|---|---|---|
| 16 | [能力复刻总规划](16-能力复刻总规划.md) | 🔶 | 全仓；分篇见本表 | 产品能力总纲。**2026-10-23 已按 doc/35–49 重基线**：§0.1 现状快照、§2 表补 5b（子代理持久档案与 resume）/ 9b（团队协作前端）/ 12（项目目录）/ 13（删除与档案清理）、§2c 横切契约（响应信封 doc/40、临时键 doc/38）、§5.1 当前待做、§6 索引补齐 doc/23、24、35–49 与 v6 文档集；doc/48 与 doc/49 已落地并从欠账里划掉；**2026-10-27 增补 doc/52 技能方案（已点头；A1–A4、B1–B8、C1–C5、D1、E1、E2、F1–F3、G1 已落地 2026-10-28，24 拍收口）**。🔶＝总纲自身仍列着 ⬜ 未做能力（IM/cron、多 agent 协作、场景化 Eval） |
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
| 45 | [场景化 Eval 框架（方案）](45-场景化Eval框架.md) | ⬜（方案待点头） | —（未施工） | 2026-10-23 立项。给「**活有没有干完**」建一层可机器重跑的回归——前两层（快闸 `contract_test.go` + 契约 `scripts/acceptance/`）只断"接口有没有按约定回"，断不了"活干完了没有"（2026-10-23 PII 挂载点选错时两层全绿）。场景 = 准备/动作/判定/证据四件套；判定分 **K 系统字节 / S 引擎裁决 / B 模型行为 / C 只登记** 四组（按「判定锁在被测系统上」分，不按「模型会不会抖」分）；判定 = **M=M**（跑 M 次全过才算过，M=5），不通过也逐次记录；红的成因分**归因**（`engine_regression` / `model_below_baseline` / `model_baseline` / `unasserted` / `incomplete`），**只有前两者拦门禁**；语料 = **17 条四组目录**（K 7 / S 4 / B 4 / C 1 + 示例 2）。**不改 `internal/runengine/` 生产代码**——Eval 站在引擎外面看它。预计落点 `scripts/eval/`（复用 `scripts/acceptance/harness.py`，不另起 HTTP 底座）。对应 doc/16 §2b 第 6 行与 §2c「验收可机器重跑」第三层 |
| 46 | [变更同步与事件通道](46-变更同步与事件通道.md) | ✅ | `yk-tako-eino/internal/runengine/{events.go,server.go,auth.go,product_api.go,contract_test.go}`、`web/src/{mutation.ts,api.ts,chat.ts,App.vue,ProjectsView.vue}`、`scripts/acceptance/events_live.sh` | 2026-10-23 落地。多 tab 失效通知的根治：**Loop A** BroadcastChannel 同机广播 + 统一失效函数（清落点/重拉列表/会话已删横幅）+ 投递 114001 自愈；**Loop B** 账号级 SSE `GET /api/events`（Redis Stream `tk:events:<tid>` 第三临时键，`XADD` 写口=变更端点横切契约；`Last-Event-ID` 续传、新连接从 `$` 起；`?access_token=` 兜底鉴权）——doc/16 #8b 前置。前端 EventSource 与广播并存、同喂一个 dispatch。活体：B2 七条 + B3 三条全过；用例 9 进 `expected.json` declared_not_asserted（`events_live.sh` 判红）+ `TestContractStreamEventBytes` 快闸。doc/16 §2c 扩两行 |
| 47 | [Plan 审核与追问澄清](47-Plan审核与追问澄清.md) | ✅ | `runengine/{questions.go,waittools.go,plangate.go,journal.go,worker.go,server.go}`、`web/src/chat.ts`、`scripts/acceptance/question_crash.sh` | 2026-10-23 立项并落地。**Q（ask_user）**：`run_questions` 表 + `kind=question` 帧（词表 8→9）+ 三拍（落行/发帧/轮询）+ CAS 一答终答 + resume 重放第三分支 + options 快捷选项；**P（plan gate，A 形态=kimi 式运行时守卫）**：模型自主调 `enter_plan_mode`，`planGuardHandler` 黑名单硬 deny 写/执行，`submit_plan` 复用等人机器过人审（可编辑方案卡），批准注回并放行。P/Q 共用一张表（kind 列）一种帧一套 resume 分支；新增 `15` 人审与问答段（115002/115101）。`question_crash.sh` 16 条断言验「挂起中 kill -9 → resume → 答 → complete」；`seed-question` 脚手架已转只读 `question-check`。翻 doc/41 §7「不做 Plan 层人审 gate」之案（用户 2026-10-23 拍） |
| 48 | [前端可用性欠账（方案）](48-前端可用性欠账.md) | ✅（2026-10-23 全部 17 拍落地） | `yk-tako-eino/web/{playwright.config.ts,e2e/,test/,src/{App.vue,ProjectsView.vue,Configure.vue,MembersView.vue,api.ts,chat.ts,markdown.ts}}`、`yk-tako-eino/internal/runengine/{product_api.go,server.go,contract_test.go}`、`scripts/acceptance/{expected.json,harness.py,assert_contract.py}`（**新增**：文件清单端点 `GET /api/workspaces/:id/files`、项目对象的 `isDefault` 字段；markdown 渲染走 `marked` + `dompurify`） | 2026-10-23 立项。2026-10-23 浏览器走查（四个页面 × 两种视口宽度 + 只读 API 交叉核对）逐条验出 **17 条界面现象（归并为 14 项修复）**：**markdown 渲染是 v6 契约违背**（19v6-03:118 写明「客户端按 markdown 渲染」，代码是纯文本插值）；**后端早有的能力前端没入口**（停止端点 `POST /api/runs/:id/stop` 一次没调 / 跨项目会话列表 `GET /api/sessions` 前端从不裸调 / 文件字节出口 `GET /api/media` 只有图片在用）；**agent 产出的文件用户拿不走**（项目页不列文件、文件片不可点）。分 4 个 loop 17 个 action，**只有 2 条动后端**（B1 新增 `GET /api/workspaces/:id/files`、C1 响应体加 `isDefault` 字段）。台账 `tmp/48-前端可用性欠账/`（不入库） |
| 49 | [走查缺陷修复](49-走查缺陷修复.md) | ✅（已落地，2026-10-23） | `internal/runengine/{agents.go,product_api.go,server.go,accesslog_test.go,agents_test.go}`、`web/src/{chat.ts,App.vue,Configure.vue,ProjectsView.vue,InviteLanding.vue,markdown.ts,auth.ts}`、`web/test/markdown.test.ts` | 2026-10-23 立项并于同日落地。doc/48 落地后的**第二次**全站走查：Playwright 逐页点击 + 真停止 + 跨标签页 + 窄屏 + 引擎日志对读，逐条验出 **12 项**。**1 项 P0**：子代理名字含空格/中文 → 厂商 `tools[i].function.name` 400 → **整个租户所有 run 失败**（保存/导入/落盘三个入口都没校验）；**5 项 P1**：停止/失败刷新后整轮消失、等人面板被别端停掉后卡死输入框且等待期间无停止入口、删除会话事件不剪侧栏（死链点进空对话）、新建同名内置助手静默清空说明/规格/工具、导入默认可见性（团队）与手工新建（仅自己）相反；**6 项 P2/P3**：报错带 HTTP 状态码、refresh 失效静默登出、邀请页印错邮箱、复制失败无反馈、密钥空名行谎报成功、灯箱不能关/不放大、名字无长度上限、自动链接吞中文标点、项目页两句话与行为不符、访问日志含完整 `access_token`。四问按推荐值落地（§14），落地与验收记录见 §15。台账 `tmp/49-走查缺陷修复/`（不入库） |
| 50 | [侧栏项目树（方案）](50-侧栏项目树.md) | 🔶（A1–A4 已落地 2026-10-24；A5 待做） | `web/src/App.vue`（只此一处） | 2026-10-24 立项。侧栏从「最近 10 条扁平会话 + 内部滚动条」改为**全部项目 → 各自对话**的二级树（手风琴、默认全展开、空项目显示「还没有对话」）：整栏合计 10 条、超出点「展示更多」就地 +10、全展开后变「收起」；**每个项目再限 10 条**，项目内超出时该项目下出「展示全部」，点开只放开这一个项目；项目名前是文件夹图标（开口=展开、合口=折叠）。列表内滚动条已删。**后端零改动**（`GET /api/workspaces` + `GET /api/sessions` 现有字段够拼）。A1 数据层／A2 树与空态／A3 展示更多／A4 双层上限与图标均已浏览器验收；**A5（`workspace_renamed`/`workspace_deleted` 事件重载、窄屏、文档回填）未开工**。台账 `tmp/50-侧栏项目树/`（不入库） |
| 51 | [UI 视觉重做](51-UI视觉重做.md) | ✅（已落地，2026-10-24） | `web/src/base.css` + `web/src/App.vue` + `web/src/{ProjectsView,Configure,MembersView,ConfirmDialog,InviteLanding,PendingInvites,PlanDoc}.vue`（样式段；后端零改动） | 2026-10-24 立项并于同日 U1–U7 落地。**token 层先行**（8 个组件 150 处硬编码颜色 → `base.css` 变量），中性现代风（浅灰白 + 单一蓝强调、白卡细边、无暖阴影）；侧栏三段式修掉 900px 高时底栏压住树；三页骨架/弹层统一 860px 列、22px 标题、12px 卡片、28/32 控件；对话列 720px、气泡 ≤86% 且用量/复制移到气泡下方；≤860px 顶栏 52px + 280px 抽屉（遮罩 / `Esc` / 开会话三法关闭，焦点往返、`aria-modal`）；四档 1440/1024/700/390 收口：无横向溢出、正文 16.88:1、次要 4.51:1、控件高 28/32、行高 30/34。**不改后端契约、不改交互流程、不改 doc/50 的树逻辑**。台账 `tmp/51-UI重做/`（不入库） |
| 52 | [技能（Agent Skills）](52-技能.md) | ✅（已落地 2026-10-28；**A1–A4、B1–B8、C1–C5、D1、E1、E2、F1、F2、F3、G1 全部施工并验收，24 拍收口**） | **A1 已落**：`internal/runengine/skills.go`（`skill_rows` / `skill_disables` 两张表 + `GET /api/skills`）、`internal/runengine/pgstore.go`（建表与部分唯一索引 `(tenant_id, name) WHERE valid`）、`internal/runengine/server.go`（路由）；契约字节 `scripts/acceptance/expected.json` 用例 14 + `doc/19-v6/19v6-04-验收-curl.md` 同号散文。<br>**A2 已落**：`internal/runengine/skills_scan.go`（`POST /api/skills/rescan`、frontmatter 解析、扫盘）、`internal/runengine/skills_test.go`（解析与扫盘单测）；库级两级锁（`pg_advisory_lock` 会话级，全局共享 + 租户独占）；`-system-skills-dir` 旗标（`cmd/yktako/main.go`）；契约字节用例 15。<br>**A3 已落**：系统管理员全量重扫（`-bootstrap-admin-email`；全局独占锁；系统根 + 每个租户根；系统行落空串桶）；系统根读失败 → 整次中止不删行、读到 0 条 → 只报数不删行；租户级重扫只涉本租户根与本租户的行；跨桶同名按「先占的生效」显式比；平铺形态也进扫描；契约字节用例 16。<br>**A4 已落**：`GET /api/skills/:name` 详情（租户视界解析、`body` 逐字原文、`missing` 恒空数组）；契约字节用例 17。<br>**B1 已落**：`internal/runengine/skilltools.go`（`<available_skills>` 目录、`read_skill`/`use_skill`/`read_skill_file` 占位、子代理剔除四名）、`profile.go`（目录只拼 manager Instruction）、`agents.go`、`worker.go`、`types.go`（`Frame.toolCallId`）、`store.go`（payload `toolCallId`）、`server.go`（`GET /api/runs/:id/skills` 空实现）；契约字节用例 18 + 用例 1 末步。<br>**B2 已落**：`read_skill` 真身（租户视界查行 → 禁用闸 `116107` → 读盘剥 frontmatter → `&`/`<` 转义成 `<skill name source bytes>正文</skill>`，不带 `path`；超 16,384 token／64KB 回 `116116` 不截断；计数器由工具自己懒构造，`-compact-window=0` 照判）、目录转义补齐、`auth.go`（`116107`／`116116`）、`skilltools_test.go`（切分/转义往返/两条上限/信封逐字）；契约进 `declared_not_asserted` 用例 19 + 同号散文。<br>**B3 已落**：`use_skill` 真身（与 `read_skill` 同一条读路、成功回执逐字相同；成功时把 `{name, root}` 记进 run 内技能盒子 `skillbox.go`）、`sandboxProfile(root, extraReadRoots)`（盒子里每个技能根只加只读 `file-read*`；`sandboxWrap` 传 `nil`；子代理在 `runSubagentGuarded` 换空盒）、`(allow network-outbound)` + `/private/etc/ssl` 实测补丁（D37；入站不开）、`commandInsideRoot`／`sandboxRealPath` 两侧 `Abs()`（修 `-workspace` 传相对路径时租户 MCP 全被判围栏外）、`skillbox_test.go`／`skillfence_test.go`／`mcpfence_test.go`（相对根）、验收夹具 `scripts/acceptance/mcp_fetch_server.py`；契约进 `declared_not_asserted` 用例 20 + 同号散文。<br>未施工：拟落 `skills_install.go`、`skillops.go`、`tools.go`、`web/src/SkillsView.vue`、`web/src/ToolsView.vue`；另涉 `internal/runengine/{compact.go,fileops.go,sandbox.go,bash.go,agents.go,tasktools.go}` | 2026-10-27 立项。本地 Markdown 技能包：安装时校验 `name`/`description`（≤1024 字符），run 装配时读 `skill_rows`（PG）生成 `<available_skills>` 目录（派生视图，同一场 run 只算一次，装配不扫盘）；盘上的偏离靠写入口与 `rescan` 收；**目录不截断**，成本靠作用域／禁用两把闸控（**不做技能搜索**——目录体积是已知天花板）；**2026-10-28 执行模型翻案**：取消内置 `skill agent`，**manager（根助手／主代理）自己 `read_skill`／`use_skill` 读正文、自己执行**，正文里的步骤展开成**看板任务**（`run_tasks` 加 `skill_name` 列）——正文是说明书、任务是替身，压缩时正文可折、任务不折；普通子代理仍技能盲（池里无技能工具、bash 看不到技能根）；正文以 `<skill name=…>` 包裹进上下文（trim 整块替换不夹切），折叠时程序侧钉「技能记录块」进摘要末尾；不做技能市场，但**认来源串与 `npx skills add …` 命令串**（严格解析、原生抓取、不执行 npx），对话里也能装（`skill_install`，只建不覆盖），删除只有管理面；系统技能与团队技能都在共享 PVC 上（系统技能部署时从仓库同步、应用只读）、项目删除不伤真身；staging→rename 原子更新；`requires` 安装时 `LookPath` 检查；**2026-10-28 第十四轮**：新增工具上传写面（`GET`／`POST`／`DELETE /api/tools`，单文件、文件级校验、不执行不探测），工具根 `<tenant>/tools` 按租户常开（所有角色含普通子代理可用、`bin` 前置进 `PATH`），技能根仍证据收窄——缺 `ffmpeg` 团队自己补，不必等运维；**2026-10-28 第十五轮**：bash 围栏放开出站网络（D37，租户 MCP 一并生效），`use_skill` 返回时按 `requires` 现查并在 `<skill>` 标签给 `missing`（D38，不阻断、只提示上传）；子进程 `PATH` 只留围栏读得到的目录，围栏外解释器报 `command not found` 而不是 EPERM（D39）；帧与 runlog 补 `toolCallId`。Linux 沙箱单独 loop。待拍项见该篇 §9。<br>**B4 已落**：`read_skill_file` 换真身——资源根内解析 `path`（拒绝对路径与任何一段 `..`，`EvalSymlinks` 后判包含，软链越界同 `116114`），入口 `SKILL.md` 用 `os.SameFile` 认出并指向 `read_skill`，目录回不递归条目列表，文件与正文同一条上限（超 64KB／16,384 token 报 `116115`、不带部分内容）；`GET /api/runs/:id/skills` 改从 runlog 真派生（`toolCallId` 配对、`seq` 排序、`path` 只在 `read_skill_file` 行）；落点 `skilltools.go`、`skills.go`、`server.go`、`auth.go`、`skillresource_test.go` <br>**B5 已落**：`run_tasks` 加 `skill_name`（`TaskCreate` 按本 run 最近一次 `use_skill` 盖戳；`read_skill` 不盖）；`taskBoardView` 每行带「（技能：名字）」（`TaskList`/写回执/`kind=task` 帧同源）；task 帧加顶层 `skillName`；落点 `tasktools.go`、`skillbox.go`、`types.go`；契约字节用例 22。<br>**B6 已落**：`TaskList` 读侧拼「上一场」（未完成行 + 说明；全完成一行提示），写回执与 task 帧不带；run 装配按看板 `skill_name` 把技能根补回 bash 盒子（续跑查本 run、新 run 查上一场，逐项重判）；落点 `tasktools.go`、`skillrestore.go`、`worker.go`；契约字节用例 23。<br>**B7 已落**：正文用完让位（D33）——`skillslim.go` 从行序算「更晚的地方建过 `TaskCreate`」的 `read_skill`/`use_skill` 结果，`slimSkillBlocks` 每枪在 `BeforeModelRewriteState` 最上面把正文换成 `<skill name="…" folded="true" reload="read_skill" tasks="见看板"/>`（state 与 base 一起改，`-compact-window=0` 也执行）；fold 转录写 marker、trim 不进预算与还原队列；runlog 仍是全文。<br>**B8 已落**：`skillops.go` 技能记录块（`<!-- tako:skill-ops v1 -->`，用过/看过两组，旧摘要继承 ∪ 本次提取，与 file-ops 块并排钉摘要末尾、同一套三档收口）；trim 对 `<skill>` 块整块替换；摘要里不写正文、不写看板任务号。契约字节用例 24。<br>**C1 已落**：`skillinstall.go` 安装/更新/删除——multipart zip/tar.gz（按魔数判格式），staging 内解包校验（zip slip/软链/层级/文件数/总字节/正文上限），锁内只搬目录与写行（旧目录进 `.trash/<name>-<ts>`）；同名冲突 install 用 `skillNameTaken`（同桶也算）、update 用 `nameTakenInView`；`DELETE` 同事务删 `skill_rows` 与 `skill_disables`；`POST /api/skills/install`（201）、`POST /api/skills/:name/update`（200）、`DELETE /api/skills/:name`（`{"status":"deleted"}`），写面只给 owner/admin。契约字节用例 25。<br>**C2 已落**：`skillsource.go`（来源串／`npx skills add` 命令串的严格解析：只认 `owner/repo`、`@ref`／`#ref`、`tree/<ref>/<subpath>`、GitHub/GitLab/归档直链；命令用自写分词器，不执行 npx，拒绝 `;|<>`$()` 与换行）＋ `skillfetch.go`（原生抓取：拨号前解析并校验 IP、直接拨已验证 IP、禁代理、重定向 ≤5 跳每跳重验、拒回环/私网/`100.64.0.0/10`/元数据地址；GitHub 走 `codeload.github.com/<o>/<r>/tar.gz/<ref|HEAD>`，GitLab 先查 `default_branch`）＋ `skillresolve.go`（`POST /api/skills/preview`、来源安装 `POST /api/skills/install` 的 `{source}`／`{command}` 分支；歧义 116108 带 `data.skills`）；仓库归档容忍 PAX 头与非普通文件（上传仍拒）、frontmatter 名优先于目录名（只警告）、多行／折叠式 frontmatter 与 `requires:` 块序列；契约字节用例 26。<br>**C3 已落**：对话内安装 HITL（D20/D24）——`skillask.go` 新建，`skill_install` 只提议（解析 → `.staging` → `run_questions(kind=install)` 行 → `kind=question` 帧 → 挂起），approve 由答案端点 CAS 赢下后由程序装盘（重新校验 staging 路径、重扫候选、同一把锁、删 staging、发 `skills_changed`），cancel/过期/未知三种收尾文本；角色闸在 `answerQuestion` 里按「可读 run + `roleRank>=admin`」判（viewer 403 `111203`，不走只放 owner 的 `denyRunWrite`）；续跑不重装；`Store.PublishEvent` + 事件词表新增 `skills_changed`（doc/46 §7.6）；契约字节用例 27（`declared_not_asserted`，活体字节在 19v6-04 用例 27）。<br>**C4 已落**：`web/src/SkillsView.vue`（列表／描述折叠／详情抽屉／上传／删除确认／invalid 区／重扫；按 doc/51 浅色皮肤）；导航新增「技能」（`Page` + `App.vue` 模板分支）；`api.ts` 技能类型与调用、`mutation.ts` 事件类型加 `skills_changed`、列表收到事件自动重拉。同拍补 `PATCH /api/skills/:name` 的服务端半（`SetSkillDisabled` 幂等写/删 `skill_disables`、回 `data.skill`、发事件、admin 闸），E2 收剩余口径与契约用例。`npm run typecheck`／`build` 绿，活体浏览器验收（列/详情/开关/上传/删除/重扫）。<br>**C5 已落**：`web/src/SkillsView.vue` 安装入口收成一个输入框＋「预览」（来源串／整条 `npx skills add …`／本地包 multipart），预览卡列全部候选（名字、描述、`字节 · 文件数`、依赖、已选✓／未选○）与 warnings；`web/src/SkillInstallCard.vue` 新建，只按 `kind=install` 的 `args` 渲染来源/ref/候选全表/选中集/「本会话未出现」提示，`owner`/`admin` 才出「安装」「取消」，其他角色只看到「已提交，等管理员确认」；`web/src/App.vue` 输入区与历史区共用这张卡（历史区无按钮），对话页工具卡 `use_skill`/`read_skill`/`read_skill_file`（按 `toolCallId` 配对、失败显示结构化 message）；`web/src/chat.ts` 的 install 行带 `answer` 复述；服务端 preview 补 `selected`（与执行同一套默认）与候选 `files`，`questions` 列表只对 install 行回 `answer`。<br>**D1 已落**：`requires` 从「存着」变成「三处同源地用」——`use_skill` 成功标签多两个属性 `requires="…" missing="…"`（`read_skill` 逐字节不带；`missing` 调用那一刻现查、不阻断），列表/详情的 `missing[]` 同源现算，新端点 `GET /api/skills/:name/check` 回 `{dependencies:[{name,found,path?}],checkedOn:"host"}`（安装/更新响应用同一形状）；子进程 `PATH` 与沙箱 profile 共用 `sandboxReadRoots`（`subprocesspath.go` 逐段 `EvalSymlinks`＋保序去重），围栏外解释器报 `command not found` 而不是 `Operation not permitted`，检查说找到的路径与 bash 解析到的是同一个；落点 `sandbox.go`、`subprocesspath.go`、`skilldeps.go`、`bash.go`、`skilltools.go`、`skills.go`、`skillinstall.go`、`server.go`、`web/src/{api.ts,SkillsView.vue}`；契约字节用例 28（+ 用例 25 形状改动），单测 `subprocesspath_test.go`／`skilldeps_test.go`。<br>**E1 已落**：manager 规格的 `skills:`／`exclude_skills:`（parse 层同写拒载；`skills: []` 靠指针非 nil = 一个都不要）落成 `tenant_agents`／`agent_defaults` 的 `skill_policy` 列，JSON 面 `{"mode":"allow|exclude|inherit","names":[…]}`（agent 读写与导入认 `skillPolicy`）；作用域在四处解析时点生效——装配期目录过滤、`read_skill`／`use_skill` 共用的 `loadByName`、`read_skill_file`、D35 看板恢复；范围外名字与「表里没有」同回 `116001` 同字节；普通子代理写 allow／exclude 在入口 400 `111002`（JSON 与 md 导入同判），出厂规格 seed 时日志拒载，显式 `inherit` 放行；落点 `agents.go`、`product_store.go`、`product_api.go`、`skills.go`、`profile.go`、`worker.go`、`skilltools.go`、`skillrestore.go`、新文件 `skillscope.go`，测试 `skillscope_test.go`／`agents_test.go`；活体抓到 `UpdateAgent` 漏写 `skill_policy` 并补列＋回归测试；契约字节用例 29。<br>**E2 已落**：禁用收口成可撤销、按租户、不被吞的开关——`PATCH /api/skills/:name` 只写 `skill_disables`（false = FirstOrCreate、true = Delete，两方向幂等），不碰 `skill_rows`、不碰盘；目录装配与 `read_skill`／`use_skill`／`read_skill_file` 读同一张禁用表（禁用后目录无它、调用回 `116107`），正在跑的 run 盒子不回缩；写面 `owner`／`admin`（viewer 403 `111203`、未登录 401 `111102`）；禁用/恢复各发一条 `skills_changed`，字节按 doc/46 §7.6 `{seq,type,id,at}`（**台账原写的 `{action,name}` 作废**——action 是状态，事件带状态会诱导客户端就地改副本而不重拉）；`DELETE` 同事务清禁用行（重装同名回 `enabled:true`，行为可证）；落点 `skills.go`、`events.go`、`skilltools.go`、`web/src/SkillsView.vue`；验收原语新增 `invite_viewer` 与 `tokenVar`（「有身份但角色不够」不能用 401 代替）；契约字节用例 30（15 步）与 `19v6-04` 同号散文，core 活体 93/93。<br>**F1 已落**：工具上传写面 `GET`／`POST`／`DELETE /api/tools`——工具根 `<tenant>/tools/<name>/bin/<name>` 不进表（列表即扫盘，跳过 `.` 开头）；128MB 帽 413 `116118`、名字白名单＋预留名＋空文件 400 `116117`、`ReserveStorage` 额度 429 `113001`、staging→`.trash`→rename 与 C1 同一把锁；写入只给 owner/admin、列表全员可读；删除先过白名单（`..` 拼成租户根）、不存在 404 `116119`；成功发 `tools_changed`；落点 `toolroots.go`；契约字节用例 31（19 步），core 活体 138/138<br>**F2 已落**：工具根对所有角色常开（含普通子代理）——`<tenant>/tools` 整棵进 profile 只读白名单（写仍只在项目根）、每个 `<name>/bin` 前置进子进程 `PATH`（跳过 `.` 开头、与宿主 `PATH` 同一套 `sandboxReadRoots` 过滤），依赖检查同走这条；`which hello` → 工具根、`which python3` → 宿主；子代理跑 `hello` 成功而 `cat <技能根>/SKILL.md` 被拒；run 中覆盖后下一枪即新版、`DELETE` 后 `command not found` 而 run 仍 complete；落点 `sandbox.go`、`subprocesspath.go`、`bash.go`、`skilldeps.go`<br>**F3 已落**：工具区前端 `web/src/ToolsView.vue`（列表带「被技能引用」、上传、同名覆盖确认、删除确认、无检测按钮），导航与路由接进 `App.vue`；技能详情每个缺失依赖旁「上传 <名字>」跳到工具区预填；`api.ts` 加三个端点封装，`mutation.ts` 补 `tools_changed` 事件类型，`ConfirmDialog` 加 `tone`／`busyText`；浏览器全流程验收 + `npm run typecheck`／`build` 绿<br>**G1 已落**：`.staging`／`.trash` 的 24h 清扫（`internal/runengine/tempsweep.go`，`tempDirTTL` + `sweepTempDirs(root, now)`）只在已有的锁内跑——技能安装／删除／租户重扫／全量重扫（每个根各清一次）、工具上传／删除；技能根与工具根同一规则，启动时不扫盘、失败只记日志（`TestSweepTempDirsByMtime`／`NoDirs`）。活体：两个根各造 25 小时前与刚建的条目 → 一次 `rescan`（技能根）与一次工具删除（工具根）之后旧条目消失、新条目留下；`rescan` 对账：造目录 → `added` 有它、删目录 → `removed` 一次、再扫 `removed:[]`、表里无重复行；并发装+扫三轮技能都不丢 |
| 54 | [运行级 OTel 追踪](54-运行级OTel追踪.md) | ✅（已落地，2026-10-31） | `yk-tako-eino/internal/observability/`（装配／内容序列化／开关）、`internal/runengine/{trace.go,trace_run.go,worker.go,journal.go,recall.go,compact.go,memory.go,server.go}`、`cmd/yktako/main.go`、`.env`（内容开关） | 2026-10-31 立项并同日 A1–A9 落地。按 **OTel GenAI 语义**写 span（`gen_ai.*` 均为 Development），走 **OTLP/http protobuf** 上报 CozeLoop：run 根 `invoke_agent yktako`（CLIENT；usage 口径＝对话账本 `tako.usage.scope=conversation`，与 usage 帧求和逐值对账）、HTTP 入口 `POST /api/runs`（SERVER，`tako.run.start_delay_ms`）、每次模型调用 `chat <model>`（TTFT/TPOT/usage/内容）、每次根级工具 `execute_tool`，以及 `tako.step.{quota,workspace,history,recall,compact,finalize,memory_extract}` 内部步骤 span（召回下挂 `search_memory`/`retrieval`）；内容属性由 `OTEL_INSTRUMENTATION_GENAI_CAPTURE_CONTENT` 控制（`.env` 默认 `SPAN_ONLY`，8KiB/文本段、64KiB/属性截断）；缺令牌/空间 ID 即 noop，导出失败只记日志、不影响 run。**实跑证据（2026-10-31）**：探针回 `exported:true`；真 run 日志打出 `traceId=`，含 `get_time` 调用的一轮 usage 帧求和 13717/112 与根 span 同源；`go test ./internal/runengine ./internal/observability` 全绿。**注**：`COZELOOP_WORKSPACE_ID` 必须是令牌所属工作区，不匹配时 OTLP 回 400。台账 `tmp/cozeloop/`（不入库） |
| 56 | [Agent 测试与基准选型（方案·部分落地）](56-Agent测试与基准选型.md) | 🔶（G1 已跑完出数，两臂配对已跑完，11-01） | `scripts/bench/`（`row.schema.json` / `arms/tako.py` / `arms/dsh.py` / `judge.py` / `cases/gaia_l1_25.json` / `slice.py` / `run_bench.py` / `pricing.json` / `check-schema.sh`）；回填 `scripts/eval/`（doc/45）仍未施工；不改 `internal/runengine/` | 2026-10-31 立项。11-01 第一批落地：把「测谁」钉死（**公开基准测的是模型 + 它自带的 harness，不是 tako**），并落纸**可比性协议**（doc/56 §4.7：受试者=runtime、钉死的变量、答案抽取规则、官方 scorer 三支、报告必带口径字段、三条禁语）。GAIA 验证集 L1 的 25 题纯文本切片是**唯一现在能直接跑 tako** 的公开基准——切片**只入配方 + `slice_hash`**（题面/答案/task_id 不入库，依据 GAIA 条款），判定器内置官方 scorer 三支并有 `--selftest` 对照官方源码；**GAIA 25 题已实跑两轮单臂并出报告**（题面本身 2/25；题面 + 尾句 21/25——差的全在「答案长什么样」，口径钉死见 doc/56 §4.8）；**同日装上 dsh 臂并跑出两臂配对**（tako 22/25 vs dsh 20/25，都过 18、只有 tako 过 4、只有 dsh 过 2——但同臂隔一小时重跑的逐题分歧率 28% 比两臂之间的 24% 还大，即换 harness 的影响不如重跑一次，**所以这轮不给两臂排名**，见 doc/56 §4.9）；pi 臂未装，三臂并排报告、pass^k 与基线归因留到下一轮；SWE-bench 先在 Linux 服务器用 mini-swe-agent 跑 25 题作**模型基线**（C2，明确不叫 tako 分数），tako 级要补引擎镜像与 bash 容器后端（G4，容器当围栏，见 §5.5）；HumanEval/MBPP/LiveCodeBench/AgentBench 不作 tako 测试；τ-bench/Claw-Eval 不移植环境，只借 pass^k、终态断言、安全一票否决回填 doc/45；另借 pi `packages/evals` 的 A/B 对照协议（case × 两臂 × 重复 → 配对 lift，只借协议不移植 vitest 栈，§6.5/G5），Eino 开源仓无 eval 包可接。**CI 门禁仍只认确定性断言**（L0 快闸 / L1 契约 / L2 doc/45 K/S 组） |

| 57 | [网络抓取（`web_fetch`）](57-网络抓取.md) | ✅（2026-11-01，A1–A3） | `internal/runengine/{webfetch.go,webfetch_test.go,worker.go,fileops.go,search.go}`、`agents/researcher.md` | 父池加 `web_fetch`：按调用方厂商选择器走 DeepSeek `/v1/tools/fetch`（只用 `deepseek` 一行，其他厂商直接报错）；整页 Markdown 落盘 `.tako/fetch/`，工具只回描述段；researcher 走继承自动拿到，coder 白名单不动 |
| 58 | [读分页与输出留存](58-读分页与输出留存.md) | ✅（2026-11-01，A0–A8） | `internal/runengine/{readwindow.go,filetools.go,bash.go,bashspill.go,notes.go,product_api.go}`、`internal/runengine/{filetools_test.go,bashspill_test.go}` | 两处「拿不到就永远拿不到」的收口：`read_file` 改**分页读**（入参 `path`/`offset`/`limit`，`head`/`tail` 删掉；每页 2000 行 / 50KB / 单行 2000 字符；页脚恒带总行数与下一个 `offset`；「超 200KB 直接报错」撤掉，长文件读得到中间）；`bash` 改**留尾 + 全文落盘 + 回路径**（每流 64,000 字节尾窗；全文落 `.tako/bash/<runId>/<seq>.out`，64MB/流，**装不下全文就删文件、不报路径**；回执行给路径；30 天清扫，每次 bash 调用前触发）。四个 API 的请求／响应字节不动；`POST /api/dev/workspace/read`（A0）让 read 的模型侧字节可 curl 逐字断言 |

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
| `doc/` | [53-模块地图](53-模块地图.md) | 非方案 | 代码结构索引：顶层模块与小模块 → 落点文件与职责；讲「代码在哪一块」，不讲「该怎么施工」 |
| `doc/` | [55-Go 少见玩法辑录](55-Go-少见玩法辑录.md) | 非方案 | 代码阅读笔记：flock / SSE / Lua CAS / seatbelt 等 20 处不常见写法的落点、动机与代价；不作施工面 |
| `doc/problems/` | [问题与方案库](problems/README.md)（43 条） | 非方案 | 决策档案：每条一个设计选择题——当时遇到什么、桌上有哪些候选、为什么这么裁、代价是什么、今天还成不成立；按模块分组，并在 `doc/53` 各小模块后反链 |
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
