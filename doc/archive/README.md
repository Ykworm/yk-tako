# doc/archive/ 归档区

> 性质：**归档**。这里只放被取代的整代材料与旧线方案，**不作施工面**。
> 判据：改代码不需要照着它做；只在回答「当时为什么这么做 / 旧版长什么样」时读。
> 正文里的路径与文件名是**当时的状态**，未随归档改写（如 `docs/superpowers/...`、`doc/07...`）；需要现行位置时以本目录清单为准。
> 现行真源见 [doc/README.md](../README.md)；带日期的记录见 `doc/log/`。

## old-line/（v6 之前的旧线）

| 文件 | 一句话 |
|---|---|
| [01-任务看板与单入口](old-line/01-任务看板与单入口.md) | 单入口定案；旧线 Task 工具，v6 未继承 |
| [02-任务看板实现计划](old-line/02-任务看板实现计划.md) | 旧线执行台账 |
| [03-立项与命名](old-line/03-立项与命名.md) | 项目立项与命名记录 |
| [04-技术路线](old-line/04-技术路线.md) | Go＋Eino 路线决策；LangGraph→Eino 映射表 |
| [05-Octop架构剖析与生长式复刻](old-line/05-Octop架构剖析与生长式复刻.md) | Octop 架构剖析；「要什么」的背景地图 |
| [06-工具安全层](old-line/06-工具安全层.md) | 旧线三态守卫与 PII 打码 |
| [07-任务规划与看板](old-line/07-任务规划与看板.md) | 旧线任务规划与看板 |
| [08-Eino规划机制调研](old-line/08-Eino规划机制调研.md) | `write_todos`／`PlanTask`／Workflow 对照 |
| [09-设计了解](old-line/09-设计了解.md) | Eino 机制笔记与面试材料 |
| [10-最小Web-UI](old-line/10-最小Web-UI.md) | 旧线最小 Web UI 定案 |
| [11-最小Web-UI实施计划](old-line/11-最小Web-UI实施计划.md) | 旧线 Web UI 执行台账 |
| [12-上下文压缩](old-line/12-上下文压缩.md) | 旧线压缩算法；v6 真源见 19v6-07 |
| [13-上下文压缩实施计划](old-line/13-上下文压缩实施计划.md) | 旧线压缩执行台账 |
| [15-页面从event_log读对话的实施计划](old-line/15-页面从event_log读对话的实施计划.md) | 旧线 Web UI 实施计划 |
| [18-媒体落盘与图片读取](old-line/18-媒体落盘与图片读取.md) | 旧线 mediaoffload；v6 真源见 doc/32、doc/35 |
| [XX-taskboard-重命名-handoff](old-line/XX-taskboard-重命名-handoff.md) | taskboard 重命名的决策记录 |
| [handoff-doc24-滤思考与Usage触发](old-line/handoff-doc24-滤思考与Usage触发.md) | doc/24 的交接稿；真源已并入 doc/24 |

## engine/（运行引擎旧代）

| 文件 | 一句话 |
|---|---|
| [19-运行引擎架构-v3](engine/19-运行引擎架构-v3.md) | 投递与执行解耦的早期方案 |
| [19-运行引擎架构-v4](engine/19-运行引擎架构-v4.md) | 已被 v5 取代 |
| [19-v5/19-运行引擎架构-v5](engine/19-v5/19-运行引擎架构-v5.md) | 已被 v6 取代 |
| [19-v5/7.9-业务用例链路](engine/19-v5/7.9-业务用例链路.md) | v5 附篇 |

### old-line/plans/（旧线执行计划）

| 文件 | 一句话 |
|---|---|
| [MAF 最小审批回路 Implementation Plan](old-line/plans/2026-09-26-maf-tool-approval.md) | 旧线计划；MAF 对照，冻结存档 |
| [最小 Web-UI T2：HTTP + SSE](old-line/plans/2026-09-26-web-ui-http-sse.md) | 旧线 Web UI |
| [最小 Web-UI T1：事件 sink 重构](old-line/plans/2026-09-26-web-ui-sink.md) | 旧线 Web UI |
| [T2 人工验收用例](old-line/plans/2026-09-27-t2-人工验收用例.md) | 旧线 Web UI 验收 |
| [Web UI：页面读内存列表](old-line/plans/2026-09-29-webui-memory-read.md) | 旧线 Web UI |
| [流式枢纽人工验收](old-line/plans/2026-09-29-webui-stream-hub-manual.md) | 旧线 Web UI 验收 |
| [Web UI 流式枢纽 Implementation Plan](old-line/plans/2026-09-29-webui-stream-hub.md) | 旧线 Web UI |
| [WebUI net/http → Gin 迁移](old-line/plans/2026-09-30-webui-gin-migration.md) | 旧线 Web UI |

### old-line/specs/（旧线设计）

| 文件 | 一句话 |
|---|---|
| [MAF 最小审批回路设计](old-line/specs/2026-09-26-maf-tool-approval-design.md) | 旧线设计；冻结存档 |
| [Web UI 流式枢纽设计 v3](old-line/specs/2026-09-28-webui-stream-hub-design.md) | 旧线设计；落盘＋seq 续传 |
