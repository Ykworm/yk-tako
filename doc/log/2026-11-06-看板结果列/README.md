# 2026-11-06 看板结果列（B 档）

> 性质：**执行记录**。结论已并入 `doc/41` §5.1 的「`run_tasks` 加 `result` 列与『标完成必须带 result』」段；
> 与代码或 doc/41 冲突时以它们为准。执行过程见同目录 `tasks.md`（冻结）。

## 病灶

一场 run 里第 2 步跑统计脚本，输出三万 token，中间有 `p95 = 1.8s`。模型用它写了结论、把任务标 completed，
接着做后面几步。上下文超窗后 mid-run 折叠把那次工具往返折掉，摘要器看到的原料已被截到 2000 字，
关键数字没进「重要事实」。模型对第 2 步失忆：要么编一个数，要么重新跑一遍脚本。

跨 run 的版本更常见：`TaskList` 只回 `#2 [completed] 统计延迟分布`，没有任何产出，要看数只能重跑。
数据本身没丢（mh 原文行、runlog、落盘文件都在），丢的是模型眼前那一份。

## 拍的轴

**标 completed 的那次调用必须带 result，否则拒绝写入。**

四档对照过：A = 只加栏位与提示（软，模型可以不写）；**B = A + 缺结果拒写（本方案）**；
C = 程序按时间窗抓工具结果、另起小模型代写；D = 程序直接截最后一条工具结果。

选 B 的理由：它让「completed 且结果为空」这个状态对**新写入**不可能出现，所以「跑完 5 发现 4 是空、
回头重跑 4」的诱发路径从结构上消失，不是靠措辞避免。闸只管「有没有字」，不管「有没有用」；
旧行不回填、不打标、不生成任何「回去补」的措辞。

## 动了什么

- `run_tasks.result` 列（`not null default ''`），入库前截到 300 字并标原长，不拒写。
- `TaskUpdate` 事务内判定：标 `completed` 且 `effectiveResult`（本次传入 || 行上已有）为空 → 哨兵
  `ErrTaskResultRequired`，整笔回滚；已有结果的行再改别的字段不会被二次拦。
- 放行口：同 run 第 3 次因缺结果被拒起，标 completed 并落固定串 `（未记录：连续 3 次没写结果）`。
- 读者：`TaskList` 给 completed 且有结果的行加 `  结果：<前 100 字>`；`TaskGet` 给全文；
  「上一场」段加「已完成的结果」子段。写回执与 `kind=task` 帧不带（D34 取舍）。
- dev 探针 `POST /api/dev/task-check`：A1 纯函数 → A2 真写入 → **A4 转正为只读 `action=list`**。

## 验收

- 单测：闸的三条金样、拒绝后状态与结果都没动、通过后两列一起写、350 字截断、两个视图金样、
  上一场段两种形态（`tasktools_test.go`）。
- 活体（隔离引擎 8943 / 隔离库 tako_acc）：探针 create → 拒绝 → list（行仍 pending、result 空）→
  带结果 update → list（视图含 `  结果：p95=1.8s`）；350 字结果落库为 300 字 + `…〔截断：原 350 字符〕`。
- 真跑一场：模型收到 `错误：标 completed 必须带 result…` 后没有当场补写，转而问
  「要不要带上 result 重新标一次」——这条行为断言**不判红**，不进 `expected.json`。
- `go test ./internal/runengine` + `go vet` + `go build` 全过；契约字节未变（`expected.json`、
  `contract_test.go` 未动）。

## 顺带观察（未修）

同一场活体里，模型并发发出两次 `TaskCreate`，第二条撞上 `run_tasks_run_no` 唯一索引
（`duplicate key value violates unique constraint`）。这是既有缺陷：任务号由事务内
`MAX(task_no)+1` 生成，并发插入没有锁。与本方案无关，登记待单独一拍。

## 关联真源

- 方案增补：`doc/41-任务看板与人工审批.md` §5.1（字段、闸、放行、渲染口径）与头注。
- 前序：doc/52 D34/D35（技能归属、会话内跨 run 续做）。
- 代码：`yk-tako-eino/internal/runengine/tasktools.go`、`tasktools_test.go`、`server.go`。
