# todos · 看板结果列（B 档）

> **性质：执行记录（冻结）**。状态以 `doc/41` 与代码为准；本文件只回答「当时怎么走的」。
> **结果（2026-11-06）**：A1–A4 全部施工并验收（单测 + 隔离引擎 8943 活体），未 commit（粒度由用户定）。
> **未做/登记**：真模型「拒写后当场补写 result」的行为断言未入 `expected.json`（模型行为不稳，不进判红集合）；活体中另观察到 `TaskCreate` 并发同名任务号的唯一索引冲突（既有缺陷，不在本方案范围）。
> 以下正文为原 `tmp/64-看板结果列/todos.md` 原样搬入。

> 交审稿。点头后按 A1 → A2 → A3 → A4 逐拍施工，每拍做完停下等验收，不自动 commit。
> 待拍项见 `planning.md §6`（P0–P6），默认按推荐写；要改直接在这份上划。

---

## A1 · 闸的契约与纯函数（不碰库）

**改什么**
- `tasktools.go`：新增 `taskResultGate(status, result string) (allowed bool, reason string)`；
  `taskUpdateInput` 加 `Result` 字段（schema 文案见 planning §2.2）；`TaskUpdate` 工具描述加一句。
- `server.go`：dev 块加 `POST /api/dev/task-check`（`authn`），只调上函数，
  回 `{"code":0,"message":"","data":{"allowed":bool,"text":"…"}}`。
- 单测 `TestTaskResultGate` 三条金样。

**为什么**
- 模型唯一能看见的信号就是「这次调用被拒了 + 那句错误文本」，这句话必须在写库之前定死；
  存储层与工具层共用同一个判定函数，避免两边各写一份。

**怎么验收**
1. 起隔离引擎，登录拿 `TOKEN`（起法与账号见 `scripts/acceptance/README.md`、`yk-tako-eino/.env`）：
   ```bash
   curl -s -X POST "$TAKO_BASE/api/dev/task-check" \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"status":"completed","result":""}'
   ```
   期望：`{"code":0,"message":"","data":{"allowed":false,"text":"错误：标 completed 必须带 result——写这一步的产出（关键数字、结论、产物路径、跑过的命令），不是「已完成」「已修复」。"}}`
2. `-d '{"status":"completed","result":"p95=1.8s，scripts/bench.py"}'` → `allowed:true`、`text:""`。
3. `-d '{"status":"in_progress","result":""}'` → `allowed:true`。
4. `cd yk-tako-eino && go test ./internal/runengine`。

**这一拍回答什么**：闸的判定边界与给模型看的措辞钉死了吗？

---

## A2 · 存储列与真写入路径（探针扩写）

**改什么**
- `RunTask` 加 `Result string`（AutoMigrate 自动加列）。
- `UpdateTask` 加 `result` 参数：事务内先查行 → `effective = in.Result || row.Result` →
  调 A1 的闸；拒绝返回哨兵 `ErrTaskResultRequired`（状态与 result 都不写）；
  通过则 `truncateRunes(result, 300)` 后落库。
- `TaskUpdate` handler 把哨兵转成错误文本（`toolErrorText` 不会重复加「错误：」）。
- 探针扩成 `action=create / update / list`（真写库，租户取登录身份，`run_id` 由调用方给）。
- 单测 `TestUpdateTaskRequiresResultOnComplete`：拒绝后行仍 `in_progress` 且 result 空；
  通过后两列一起写；350 字截断标记。

**为什么**
- 闸必须落在唯一写路径上（工具与探针共用），否则模型可以绕过；
  列不落库，下一场 `TaskList` 读不到，整个方案无意义。

**怎么验收**
1. ```bash
   curl -s -X POST "$TAKO_BASE/api/dev/task-check" -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"action":"create","run_id":"r_probe_a","subject":"探针任务"}'
   ```
   期望：`data = {"taskId":1,"status":"pending"}`。
2. 拒绝路径：
   ```bash
   curl -s … -d '{"action":"update","run_id":"r_probe_a","task_id":1,"status":"completed"}'
   ```
   期望 `allowed:false` + planning §2.2 的错误文本；随后 `action=list` 期望 `status:"pending"`、`result:""`。
3. 通过路径：`… "status":"completed","result":"p95=1.8s"` → `allowed:true`；再 `list` 期望
   `{"taskNo":1,"status":"completed","subject":"探针任务","result":"p95=1.8s"}`。
4. 超长：`result` 塞 350 字 → `list` 里 `result` 尾部是 `…〔截断：原 350 字符〕`。
5. `go test ./internal/runengine`（既有 5 处 `UpdateTask` 调用同步改签名）。

**这一拍回答什么**：闸在真写入路径生效吗？拒绝会不会留下半截状态？下一场读得到吗？

> diff 预估 ~110 行（超 80 目标，报备）：探针扩写占大头，可拆「create/list」与「update」两小拍。

---

## A3 · 读者：TaskList / TaskGet / 提示词

**改什么**
- `TaskList` 用新的 `taskListView(rows)`：`taskBoardView` 原样保留给写回执与 `kind=task` 帧；
  新视图在 completed 且有 result 的行尾加 `\n  结果：<前 100 字>`。
- `TaskGet` 多一行 `结果：<全文>`（仅非空时）。
- `taskBoardPrompt` 加 planning §2.2 那一条。
- 上一场段按 P1 拍板：推荐加「已完成结果」子段（一条一行前 100 字）。
- 探针 `action=list` 的返回加 `view` 字段（与 TaskList 同源），让视图字节可 curl。
- 单测：视图金样（含截断、空结果不渲染、上一场段形态）。

**为什么**
- 写了没人读等于没写；写回执不带是为了不让每张回执重付所有历史结果（doc/52 D34 同款取舍）。

**怎么验收**
1. ```bash
   curl -s -X POST "$TAKO_BASE/api/dev/task-check" -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"action":"list","run_id":"r_probe_a"}'
   ```
   期望 `data.rows[0].view` 包含两行：
   `#1 [completed] 探针任务` / `  结果：p95=1.8s`。
2. 活体：按 `doc/19-v6/19v6-04-验收-curl.md` 用例 1 起一场 run，然后
   `curl -s "$TAKO_BASE/api/runs/$RUN/frames?after=0" -H "Authorization: Bearer $TOKEN"`
   → `TaskList` 的 `tool_result` 帧文本里能看到 `结果：`。
3. `go test ./internal/runengine`。

**这一拍回答什么**：本场折叠后、以及下一场的看板读口，都能把结果拿回模型眼前吗？写回执有没有被撑胖？

---

## A4 · 放行口 + 活体 + 收口

**改什么**
- `taskBox` 加拒写计数（同 noteBox 先例，随 run 死）：同 run 第 3 次拒写起放行，
  status 落 `completed`，result 落固定串 `（未记录：连续 3 次没写结果）`；回执不追加任何催促。
- 探针转正：删 `create` / `update` 两个写动作，只留 `action=list`（转正点，见 planning §4）。
- `doc/41` 追加段（字段、闸、放行、渲染口径、日期）；`doc/README.md` 状态不变（仍 ✅）。
- 台账留档：`todos.md` 原样进 `doc/log/<落地日>-看板结果列/tasks.md`，`planning.md` 提炼进同目录 `README.md`，删 `tmp/64-看板结果列/`。

**为什么**
- 没有放行口，模型如果固执会在这个调用上无限撞墙（memory 工具踩过，最后加了劝停）；
  脚手架不登记转正点会永久留在代码里。

**怎么验收**
1. 活体（隔离实例）：起 run，提示「建两条任务，把第一条标 completed，不要写 result」→
   `curl "$TAKO_BASE/api/runs/$RUN/frames?after=0"` 期望先看到
   `tool_result` 文本以 `错误：标 completed 必须带 result` 开头，同一 run 后续出现带 `"result"` 的
   `TaskUpdate` 调用。模型没补写时记入 `declared_not_asserted`（行为不稳），不判红。
2. 转正生效：旧的 `action":"create"` 命令回 HTTP 400（拒绝信封，字节以实跑为准）。
3. `go test ./internal/runengine` + `go vet ./internal/runengine`；前端未动。
4. `bash scripts/check-master-plan.sh` + `bash scripts/check-doc-layout.sh`。

**这一拍回答什么**：闸不会把 run 卡死吗？下一场真能靠这条结果续上吗？

---

## 收尾清单（A4 内）

- [ ] 探针写动作已删；`r_probe_` 行已清
- [ ] doc/41 追加段与代码一致；台账已留档
- [ ] 未 commit（等用户定粒度）
