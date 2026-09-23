# yk-tako

从 pi 式 agent 循环长出来的 AI 工作伙伴。目标是生长式复刻 [Octop](https://github.com/TencentCloud/Octop) 的核心能力：每一层自己先写，卡住再对照 harness 源码，而不是把存量代码翻译过来。

技术栈：TypeScript、LangGraph.js、deepagents。模型按 Octop 的厂商目录来接，OpenAI 兼容接口和 Anthropic 各走各的客户端。

## 当前进度

进行到 **L1**：工具安全层三件套（命令守卫 / 文件守卫 / PII 脱敏）已上线，两个守卫都是三态（放行 / 硬拒 / 暂停问人），审批用 LangGraph 原生 `interrupt` 实现；工作区外访问默认弹审批，对齐 Codex 的 workspace-write 提权模型。

| 层 | 能力 | 状态 |
| --- | --- | --- |
| L0 | tool-call 主循环 | ✅ 三个对照脚本 |
| L1 | 工具权限与安全 | ✅ 三件套中间件 + 人工审批 |
| L2 | 任务规划 | 下一步 |
| L3 | 上下文管理 | |
| L4 | 子代理 | |
| L5 | 记忆 | |
| L6 | 断点恢复与流式 | |
| L7 | 多入口统一路由 | |
| L8 | 多用户、多 Agent | |

决策过程和 Octop 架构笔记在 `doc/`。

## 快速开始

需要 Node.js 与 [pnpm](https://pnpm.io/)。

```bash
pnpm install
cp .env.example .env
```

在 `.env` 里填入要用的厂商 Key，并用 `TAKO_MODEL` 指定 `厂商/模型`。只配了一个厂商时，模型名可以不带前缀，也不必出现在目录里。

```bash
DEEPSEEK_API_KEY=sk-你的key
DEEPSEEK_BASE_URL=https://api.deepseek.com/v1
TAKO_MODEL=deepseek/deepseek-v4-flash
```

厂商目录在 `src/config/provider-catalog.json`，和 Octop 用的 harness 模板是同一份（DeepSeek、OpenAI、Anthropic、Gemini、Kimi、智谱、通义、Groq、OpenRouter、Ollama 等）。协议是 `openai` 的都走 `ChatOpenAI`，只换 base URL；`anthropic` 走 `ChatAnthropic`。三个脚本不用改。

一个 Key 对应多个地域时，`TAKO_MODEL` 必须带厂商前缀，例如 `kimi-cn/kimi-k2.5`、`zhipu-cn/glm-5`。目录里没有的兼容接口用 `TAKO_PROVIDER_<NAME>_API_KEY`、`TAKO_PROVIDER_<NAME>_BASE_URL`，可选 `TAKO_PROVIDER_<NAME>_PROTOCOL`（`openai` 或 `anthropic`）。

三个脚本都是命令行对话，输入 `quit` 退出。可以试「现在几点」或「算一下 1234*5678」。

```bash
pnpm l0:raw          # ① 手写 while 循环
pnpm l0:langgraph    # ② createAgent
pnpm l0:deepagents   # ③ createDeepAgent
pnpm l1:guard        # L1 工具安全层（含人工审批，危险命令试试 rm -rf /tmp/xx）
```

类型检查：

```bash
pnpm typecheck
```

## 三个 L0 脚本

共用 `src/config/model.ts` 和 `src/tools/index.ts`（`get_current_time`、`calculate`）。差别只在循环由谁负责。启动时会打印当前的 `厂商/模型`。

**① 裸写版**（`src/l0/01-raw-loop.ts`）

`while (true)`：把历史发给模型；没有 `tool_calls` 就结束本轮，否则执行工具、把 `ToolMessage` 塞回历史，再问一次。循环本身很短，状态持久化、中断、流式都不在这里。

**② LangGraph.js**（`src/l0/02-langgraph.ts`）

`createAgent` 把循环收成一张图：`model_request` 节点调模型，有工具调用就走到 `tools`，否则结束。`MemorySaver` 按 `thread_id` 记住会话；`stream()` 能看到每个节点的增量。换 `PostgresSaver` 才是可重启的持久化，当前还是内存。

**③ deepagents**（`src/l0/03-deepagents.ts`）

`createDeepAgent` 在上面再叠中间件。复杂任务会先列 todo，也可以写文件、派子代理。Octop 的 harness-agent 就是 `createDeepAgent` 加上自研中间件；后面每爬一层，就是往这里插一个。

可以拿这句感受和前两版的差别：

```text
帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结
```

## 目录

```text
src/
  config/model.ts              按 TAKO_MODEL 建成 LangChain 模型
  config/resolve.ts            从环境变量选出厂商和模型
  config/providers.ts          厂商目录的类型
  config/provider-catalog.json 厂商目录（来自 harness provider_template.json）
  tools/index.ts               工具定义
  l0/                          三个对照脚本
doc/                           立项、技术路线、Octop 架构与移植计划
  ts-syntax.md                 读代码时记下的 TypeScript 语法
```

## 参考

- Octop 源码：`/Users/yuweikang/dev/ykcode/Octop`
- harness 包源码：`/Users/yuweikang/dev/ykcode/harness-pkg/`
- WeKnora：`/Users/yuweikang/dev/ykcode/WeKnora`
