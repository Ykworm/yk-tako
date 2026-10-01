# Handoff · 包重命名：`taskboard` → `tasktracker`

> 性质：**纯 handoff 文档**。不改动任何代码；记录本次命名对话的决策背景、
> 被否决的候选、影响面清单、以及逐文件的操作步骤，留给后续执行重命名时对照。
>
> 触发对话（2026-09-30）：用户先问 `taskboard.go:L1` 有什么用 → 引申到「包名不合理」
> → 排查与 eino `plantask`/`Claude Tasks` 的概念冲突 → 否决 `tasks`（概念歧义）、
> `planning`（反三层分离、且与已删除旧包同名）、`ledger`（联想财务）、
> `board`（联想前端 dashboard）→ **终定 `tasktracker`**。

---

## 1. 决策：为什么是 `tasktracker`

### 1.1 三层分离约束（硬边界）

三层分离写死在 [internal/taskboard/taskboard.go#L5-L6](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/taskboard.go#L5-L6)
及 [doc/07 §4.1](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/doc/07-%E4%BB%BB%E5%8A%A1%E8%A7%84%E5%88%92%E4%B8%8E%E7%9C%8B%E6%9D%BF.md#L83-L96)：

| 层 | 含义 | 命名禁区 |
|---|---|---|
| Plan | 给人审的方案（动手前拆步骤） | ❌ 不能沾 **plan/planning** |
| Tasks | 施工期勾选看板（开干后跟踪状态） | ✅ 包名必须落在这一层 |
| Actions | `bash`/文件读写等干活工具 | ❌ 不能沾 action/exec |

### 1.2 被否决的候选（每次否决都是心智雷区）

| 候选 | 否决原因 | 谁的直觉 |
|---|---|---|
| `taskboard` | `board` 会联想到「前端 dashboard / UI 面板」，掩盖「跟踪施工状态」的核心职责 | 用户（核心否决） |
| `planning` | 指 Plan 层，直接违背三层分离；且与 2026-09-26 删除的旧 `internal/planning` 同名，历史混淆 | 用户+文档 |
| `tasks` | 与 eino `plantask`、Claude Tasks 概念撞脸，每次读代码要做「哦是第三种 task」的消歧 | 用户+我们在 §2 一起确认 |
| `ledger` / `account` | 强联想「财务、钱」，虽然工程界有「事件流水账=ledger」的泛义，但压不过用户的先验语义 | 用户（核心否决） |
| `dashboard` / `panel` | 纯 UI 概念，比 `board` 更坏 | 直接排除 |
| `kanban` | 比 `board` 好（是方法学不是 UI），但中文心智里仍是「看板=墙上那块板」，且用户没提这个词 | 保留但不推荐 |
| `worktrack` | 弱化了 `task` 前缀，但对外工具名是 `TaskCreate/TaskUpdate`（给模型看的），包名 vs 工具前缀拧巴 | 保留但不推荐 |

### 1.3 `tasktracker` 终定理由

- **无语义冲突**：软件工程界 `bugtracker` / `issuetracker` / `tasktracker` 用法现成，
  **没有人会把 tracker 联想到前端 UI 或财务账**。
- **过程语义精准**：`track` 直接对应 `pending → in_progress → completed` 的**状态跟踪**动作；
  既不是「规划前」（planning）、也不是「展示」（board）、更不是「账」（ledger）。
- **与工具名自洽**：对外工具名 `TaskCreate/TaskUpdate/...` 前缀是 `Task`，包名 `tasktracker` 前缀一致，
  Go 代码里看到 `tasktracker.NewStore()` 和模型看到 `TaskCreate` 不会产生两套心智。
- **不撞历史**：仓库里从未出现过 `tasktracker`，git 考古不串台。

---

## 2. 影响面（37 处引用 × 9 文件 × 1 README）

2026-09-30 全仓 `grep "taskboard\."` 结果（已排除包内注释中「见 taskboard.go」之类的文字性提及，
这些需跟着内部重命名再改）。数字只是参考，**以改名时的实际 grep 为准**。

### 2.1 需改 import 路径 + 包名引用

| 文件 | 引用形态（示意） | 处数 |
|---|---|---|
| [cmd/yktako/main.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/cmd/yktako/main.go) | `import "yk-tako-eino/internal/taskboard"`、`taskboard.New/NewStore`、`taskboard.Instruction` | 4 |
| [cmd/yktako/sink_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/cmd/yktako/sink_test.go) | `taskboard.ToolList` | 2 |
| [internal/adkrun/event.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/event.go) | `import "…/taskboard"`、`taskboard.IsBoardTool` | 1+1=2 |
| [internal/adkrun/event_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/event_test.go) | `taskboard.ToolList`（断言 Kind=EventBoard 的用例） | 9 |
| [internal/adkrun/turn_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/turn_test.go) | 注释：「taskboard.IsBoardTool 判定」 | 1（文字） |
| [internal/webui/server_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/webui/server_test.go) | `import`、`taskboard.New/NewStore/ToolCreate` | 4 |
| [README.md](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/README.md) | `internal/taskboard` 路径 + 文字说明 | 1+（多处文字） |

### 2.2 包内部（目录移动后，import 路径变了，**但包内自引用是 `包名.符号`**，
因此把 `package taskboard` 改成 `package tasktracker` 后，所有 `taskboard.` 自引用都要同步改成 `tasktracker.`）

| 文件 | 需改 |
|---|---|
| [taskboard.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/taskboard.go) | `package taskboard` → `package tasktracker`；文件本身也建议改名成 `store.go`（见 §3.1） |
| [tools.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/tools.go) | `package taskboard`；第 72 行注释「机制见 taskboard.go 的 SessionKey 注释」→「机制见 store.go」（如果文件也改名了） |
| [middleware.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/middleware.go) | `package taskboard`；第 2、44 行注释提「eino plantask 同款」不必改，但提 `taskboard.go` 文件名的要同步 |
| [taskboard_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/taskboard_test.go) | `package taskboard` → `package tasktracker` |
| [tools_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/tools_test.go) | `package taskboard` → `package tasktracker` |
| [middleware_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/middleware_test.go) | `package taskboard` → `package tasktracker` |
| [integration_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/integration_test.go) | 外部测试包 `package taskboard_test` → `package tasktracker_test`；所有 `taskboard.` 导出符号引用全部改成 `tasktracker.`；import 路径改成 `…/internal/tasktracker` |

### 2.3 可能的连锁：常量 `IsBoardTool` / `EventBoard` / `SessionKey` 值

这些**不在本次必须改名范围内**，但逻辑上已轻微错位，留待后续决策：

| 东西 | 错位点 | 是否本次改 |
|---|---|---|
| `IsBoardTool(name) bool`（[tools.go#L29-L36](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/tools.go#L29-L36)） | 判断的是不是「账本/跟踪工具」，但名字还叫 Board | ⚠️ 可选，建议改 `IsTrackerTool`，但要同步 adkrun 的引用 |
| `EventBoard EventKind`（[event.go#L28](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/event.go#L28-L28)） | 呈现事件四类之一叫 board，其实是「账本投影」 | ❌ 本次不改（是呈现层的标签，不是包概念） |
| `SessionKey = "taskboard_session_key_tasks"`（[taskboard.go#L25](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/taskboard.go#L25-L25)） | 字符串里还嵌着 `taskboard` | ⚠️ **强建议一起改**，否则 grep 代码时永远能搜到 `taskboard`，且这个 key 是 checkpoint 内部用的（目前 checkpoint 也是内存的，无向后兼容负担）。现在不改以后就成死债 |
| 工具名 `TaskCreate/TaskUpdate/...` | 给模型看的外部协议，和 Claude Tasks 对齐 | ❌ **绝对不能改** — 这是模型协议层 |
| `Instruction` 常量（[middleware.go#L57-L61](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/middleware.go#L57-L61)） | 首行「任务看板约定」文字还是看板说法 | ⚠️ 可选，改成「任务跟踪约定」比较顺手 |

---

## 3. 操作步骤清单（留给下次执行时逐条 tick）

### 3.1 改目录 + 内部文件（最小变更顺序）

- [ ] 1. `mv yk-tako-eino/internal/taskboard yk-tako-eino/internal/tasktracker`
- [ ] 2. 目录内 7 个文件的 `package taskboard` → `package tasktracker`；
        外部测试包 `package taskboard_test` → `package tasktracker_test`
- [ ] 3. （顺手）把 `taskboard.go` 重命名为 `store.go` —— 因为包名不再叫 taskboard，
        文件名还用旧包名会误导人；它的内容本来就是「数据模型 + Store 正本」，叫 `store.go`
        刚好和 adkrun、mediaoffload、compact 等包的命名风格一致（它们都有 `store.go`）。
- [ ] 4. （如果改了文件名）同步所有注释里的「见 taskboard.go」→「见 store.go」：
        目前至少 [tools.go#L72](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/tools.go#L72-L72)
- [ ] 5. 包内自引用 `taskboard.Task` / `taskboard.StatusCompleted` / `taskboard.NewStore` 等
        全部改成 `tasktracker.`；集成测试（外部包）里的 `taskboard.ToolCreate` 等量替换
- [ ] 6. （建议）`SessionKey` 常量字符串值改为 `tasktracker_session_key_tasks`
        （或更短 `tasktracker_tasks`，唯一即可）
- [ ] 7. （可选）`IsBoardTool` → `IsTrackerTool`
- [ ] 8. （可选）`Instruction` 首行「任务看板约定」→「任务跟踪约定」

### 3.2 改外部引用（按 §2.1 列表逐文件扫）

- [ ] 9. [cmd/yktako/main.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/cmd/yktako/main.go)：
        import 路径 + `taskboard.X` 全部替换
- [ ] 10. [cmd/yktako/sink_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/cmd/yktako/sink_test.go)：同上
- [ ] 11. [internal/adkrun/event.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/event.go)：
         import 路径 + `taskboard.IsBoardTool`（如果改了函数名就改新名）
- [ ] 12. [internal/adkrun/event_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/event_test.go)：
         import + 全部 `taskboard.ToolList`
- [ ] 13. [internal/adkrun/turn_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/adkrun/turn_test.go)：
         文字注释 `taskboard.IsBoardTool` → `tasktracker.IsTrackerTool`（如果函数改名了）
- [ ] 14. [internal/webui/server_test.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/webui/server_test.go)：
         import + 全部 `taskboard.X`

### 3.3 验证（必须全绿才能收尾）

- [ ] 15. `cd yk-tako-eino && go build ./...`
- [ ] 16. `go vet ./...`
- [ ] 17. `go test -race ./...`（确保集成测试的 gob checkpoint 也过——`SessionKey` 字符串值改了后要重跑
         [integration_test.go 的 TestInterruptCheckpointCarriesBoard](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/taskboard/integration_test.go#L1094-L1140)，
         它目前断言 checkpoint 字节里含 `"查时间"`，和 SessionKey 无关，应该不受影响）
- [ ] 18. README：[yk-tako-eino/README.md](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/README.md)
         里所有 `internal/taskboard` 路径 + 「任务看板」文字说明按需更新（看板是 UX 呈现名词，
         不是包名，所以文字层保留「看板」没问题；只是代码路径要改对）
- [ ] 19. （顺手）确认 doc/ 目录里其他 md 是否引用 `internal/taskboard` 路径——
         目前 [doc/02](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/doc/02-%E4%BB%BB%E5%8A%A1%E7%9C%8B%E6%9D%BF%E5%AE%9E%E7%8E%B0%E8%AE%A1%E5%88%92.md)
         和 [doc/07](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/doc/07-%E4%BB%BB%E5%8A%A1%E8%A7%84%E5%88%92%E4%B8%8E%E7%9C%8B%E6%9D%BF.md)
         有大量引用。**策略：这些是历史文档（「实现计划」「决策记录」），
         引用的是当时写就时的旧路径，不必改**，保持记录原样更真实；
         **只改「当前生效」的 README**——除非你想要所有历史文档都一致，那另计。

### 3.4 本 handoff 文档的处理

- [ ] 20. 改名做完后，把本文件的编号 `XX-` 改成正式编号（按 doc/ 目录现有最大编号 +1）。
         目前最大编号是 `18-`，新的就是 `19-`。
         文件从 `XX-taskboard-重命名-handoff.md` 改成
         `19-taskboard-重命名-handoff.md`（保留原名里的 `taskboard` 关键词，
         方便未来考古——这是 handoff 不是新文档）。
- [ ] 21. 在 §0 顶部追加一行「执行记录」：日期、执行人、对应 commit hash（以后 git 有 commit 之后）。

---

## 4. 风险点清单

| 风险 | 概率 | 应对 |
|---|---|---|
| `SessionKey` 改字符串后，**如果**有任何外部 checkpoint 持久化到磁盘（目前 adkrun.Store 是内存的，但将来接磁盘就有），会出现续跑找不到 key | 目前极低 | 现在不改，将来接入磁盘 checkpoint 时就必须带迁移脚本；现在改是最佳时机，0 兼容成本 |
| 集成测试 gob 反序列化受影响 | 极低 | gob key 是 `SessionKey` 字符串，改了后 **新 run 用新 key**，老 run（内存的进程都结束了）不存在 |
| `IsBoardTool` 改 `IsTrackerTool` 时漏改 adkrun 引用 | 中 | 用 `grep -rn "IsBoardTool"` 全仓搜，改完 `go build` 会报错兜底，不会沉默 |
| 注释里「taskboard」文字性残留 | 高 | 残留不影响构建，纯美观；允许分批清理，但 `SessionKey` / `import` / `package` 声明必须一次改完 |
| IDE / 其他构建缓存把旧路径留在 `.vscode/`、`.idea/` 等 | 低 | 「手动移目录后重新跑 `go build ./...`」会重建 Go 索引 |

---

## 5. 本次对话的决策摘要（防止将来又绕一圈）

> **核心分歧**：`board` → 你联想「前端 dashboard」；`ledger` → 你联想「财务账」；
> `planning` → 违背三层分离且踩历史包名；`tasks` → 与 eino/Claude Task 概念消歧成本高。
>
> **收敛标准**：命名必须是「软件工程有现成先例」「不占用你的已有语义雷区」「落在 Tasks 层（施工跟踪）」。
> 满足全部三条的只有 `tasktracker`（参考 `bugtracker/issuetracker` 用法）。
>
> **对外协议不动**：`TaskCreate/TaskUpdate/TaskList/TaskGet` 四个工具名（给模型的）、
> `pending/in_progress/completed/deleted` 状态值、`EventBoard` 呈现类别标签——
> 这些都不是 Go 包名层面的事，本次不碰。
