# yk-tako

从 pi 式 agent 循环长出来的 AI 工作伙伴。目标是生长式复刻 [Octop](https://github.com/TencentCloud/Octop) 的核心能力：每层能力自己先写，卡住再对照参考实现。复刻是**功能翻译，不是设计移植**——参考实现只回答"要什么"，"怎么设计、怎么写"一律遵循 Eino 的架构思想和最佳实践；Eino 没有的类库（记忆、RAG、MCP、IM、沙箱等）参考 [WeKnora](https://github.com/Tencent/WeKnora) 的 Go 实现。

技术栈：**Go + [Eino](https://github.com/cloudwego/eino)**（字节 CloudWeGo）。主线代码在 `yk-tako-eino/`，**唯一入口 `cmd/yktako`**。

> 定案（2026-09-26）：仓库从「分层学习对照」收敛为单入口产品形态。原 L0–L2 的五个对照入口（l0raw / l0react / l0deep / l1guard / l2planexec）与 planexec 硬编排已删除，能力折进唯一入口：裸 ReAct + 任务看板 + L1 三件套守卫。doc/ 旧文档（01–06 与《设计了解》）同时删除，新系列从《01-任务看板与单入口》重新开始。`maf/`（Microsoft Agent Framework 对照）保留为冻结存档，不再推进。

## 能力

- **任务看板**：`TaskCreate` / `TaskUpdate` / `TaskList` / `TaskGet` 四个记账工具，Plan / Tasks / Actions 三层分离；看板正本跨轮存活，interrupt 时随 checkpoint 落盘（定案见 `doc/01`，实现记录见 `doc/07`）。
- **工具安全**：命令守卫 / 文件守卫 / PII 脱敏三件套，三态判定（放行 / 硬拒 / 暂停问人），审批用 Eino 的 interrupt/resume；工作区外访问默认弹审批，对齐 Codex 的 workspace-write 提权模型（见 `doc/06`）。
- **厂商目录接模型**：OpenAI 兼容接口走 `eino-ext/openai`，Anthropic 走 `eino-ext/claude`，目录在 `yk-tako-eino/internal/config/provider-catalog.json`。

## 快速开始

需要 Go 1.25+。`.env` 已放在 `yk-tako-eino/`（配厂商 Key 和 `TAKO_MODEL`）。

```bash
cd yk-tako-eino
go run ./cmd/yktako                              # REPL
go run ./cmd/yktako -web 127.0.0.1:8765          # 本机 Web UI
go test ./...                                    # 单元测试不打模型，不需要 API Key
```

Web UI 单用户、无鉴权。事件库是 `-web-db`（默认 `yktako-webui.db`）：断线后续传，停止按钮取消这一轮，另一个标签页可以看同一场 run。REPL 不写这个库。

详见 `yk-tako-eino/README.md`。

## 目录

```text
yk-tako-eino/   主线实现（Go + Eino），唯一入口 cmd/yktako
maf/            Microsoft Agent Framework（.NET）对照，冻结存档，见 maf/README.md
doc/            定案与实现记录，2026-09-26 起新系列（01 定案 → 02 实现计划 → 03-09 各篇重写）
```

## 参考

- Octop 源码（功能参照物，只取"要什么"）：`/Users/yuweikang/dev/ykcode/ref-agent/Octop`
- harness 包源码（卡住时的参考答案）：`/Users/yuweikang/dev/ykcode/harness-pkg/`
- WeKnora（Eino 缺失类库的 Go 参考实现）：`/Users/yuweikang/dev/ykcode/ref-agent/WeKnora`
