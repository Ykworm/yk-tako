# yk-tako

从 pi 式 agent 循环长出来的 AI 工作伙伴。目标是生长式复刻 [Octop](https://github.com/TencentCloud/Octop) 的核心能力：每一层自己先写，卡住再对照 harness 源码。复刻是**功能翻译，不是设计移植**——Octop 源码只回答"要什么"，"怎么设计、怎么写"一律遵循 Eino 的架构思想和最佳实践，不被 Octop（Python/LangGraph 系）的设计带偏；Eino 没有的类库（记忆、RAG、MCP、IM、沙箱等）参考 [WeKnora](https://github.com/Tencent/WeKnora) 的 Go 实现。

技术栈：**Go + [Eino](https://github.com/cloudwego/eino)**（字节 CloudWeGo）。主线代码在 `yk-tako-eino/`。

> 路线定案（2026-09-24）：原 TypeScript（LangGraph.js + deepagents）路线已废弃并删除。
> 决定性依据是一轮 Eino 调研：Eino 把"规划与执行的时序"拆成三套东西——
> DeepAgent 的 `write_todos`（软约束，prompt + 工具）、`PlanTask`（带依赖的工单，仍是软约束）、
> 以及真正硬的 **Workflow Agents / Plan-Execute 节点图**（阶段顺序由图保证，不咨询 LLM）。
> 要工程保证时不需要在 middleware 上加防守，直接编图即可（已落地为 `cmd/l2planexec`）。
> 调研全文见 `doc/06`。
> `maf/`（Microsoft Agent Framework 对照）保留为冻结存档，不再推进。

## 当前进度

进行到 **L2**：任务规划层上线——手写 `planning`（清单状态 + `write_todos` 全量替换 + 提示词注入 + 并行防守），并外扩了官方版没有的"半途而废提醒"。L1 工具安全层三件套（命令守卫 / 文件守卫 / PII 脱敏）已在栈内，两个守卫都是三态（放行 / 硬拒 / 暂停问人），审批用 Eino 的 interrupt/resume 实现；工作区外访问默认弹审批，对齐 Codex 的 workspace-write 提权模型。

| 层 | 能力 | 状态 |
| --- | --- | --- |
| L0 | tool-call 主循环 | ✅ 三个对照脚本 |
| L1 | 工具权限与安全 | ✅ 三件套守卫 + 人工审批 |
| L2 | 任务规划 | ✅ 手写 planning + 官方对照 + planexecute 硬编排 |
| L3 | 上下文管理 | 下一步 |
| L4 | 子代理 | |
| L5 | 记忆 | |
| L6 | 断点恢复与流式 | |
| L7 | 多入口统一路由 | |
| L8 | 多用户、多 Agent | |

决策过程和 Octop 架构笔记在 `doc/`。

## 快速开始

需要 Go 1.25+。`.env` 已放在 `yk-tako-eino/`（配厂商 Key 和 `TAKO_MODEL`，厂商目录在 `yk-tako-eino/internal/config/provider-catalog.json`）。

```bash
cd yk-tako-eino
go run ./cmd/l0raw        # ① 手写 for 循环
go run ./cmd/l0react      # ② react.NewAgent
go run ./cmd/l0deep       # ③ deep.New（DeepAgent）
go run ./cmd/l1guard      # L1 工具安全层（含人工审批）
go run ./cmd/l2planning   # L2 任务规划（手写 planning，/todos 查看清单）
go run ./cmd/l2builtin    # L2 对照：deep.New 自带 write_todos
go run ./cmd/l2planexec   # L2 对照：planexecute 硬编排（时序由图保证）
go test ./...             # 单元测试（不打模型）
```

详见 `yk-tako-eino/README.md`。

## 目录

```text
yk-tako-eino/   主线：L0–L2 的 Go/Eino 实现，后续层级都在这里生长
maf/            L2 的 Microsoft Agent Framework（.NET）对照，冻结存档，见 maf/README.md
doc/            立项、技术路线、Octop 架构剖析、各层小结、Eino 调研定案
```

## 参考

- Octop 源码（功能参照物，只取"要什么"）：`/Users/yuweikang/dev/ykcode/Octop`
- harness 包源码（卡住时的参考答案）：`/Users/yuweikang/dev/ykcode/harness-pkg/`
- WeKnora（Eino 缺失类库的 Go 参考实现）：`/Users/yuweikang/dev/ykcode/WeKnora`
