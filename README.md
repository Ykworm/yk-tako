# yk-tako

从 pi 式 agent 循环长出来的 AI 工作伙伴。目标是生长式复刻 [Octop](https://github.com/TencentCloud/Octop) 的核心能力：每层能力自己先写，卡住再对照参考实现。复刻是**功能翻译，不是设计移植**——参考实现只回答「要什么」，「怎么设计、怎么写」遵循 Eino 的架构思想和最佳实践。

技术栈：**Go 1.27 + [Eino](https://github.com/cloudwego/eino)**（字节 CloudWeGo）+ Gin + PostgreSQL/pgvector + Redis，前端 Vue 3 + Vite。主线代码在 `yk-tako-eino/`，唯一入口 `cmd/yktako`。

> 当前是 v6（2026-10-03 起从零重写）。旧 v1/v2 的实现与协议均已废弃，只作历史留档，**不作施工面**；文档怎么分层见 [doc/README.md](doc/README.md)。

## 能力

- **运行引擎**：一场 run 从投递到定稿的全生命周期。四个拉取 API（投递、会话状态、frames、定稿 log）是契约面；`GET /api/runs/{runId}/stream` 用 SSE 把同一帧面再运一遍——拉是正确性，流是优化。租约与互斥闸在 Postgres，临时态在 Redis，按多副本目标栈设计。
- **崩溃续跑**：轮次内的 assistant/tool 行先进 `run_journal`；进程被 `kill -9` 后 `POST /api/runs/{runId}/resume` 由新副本按账本接管，已完成的工具调用不重跑，没拿到结果的调用如实告知模型（doc/30）。
- **多租户与登录**：注册即建个人租户，支持邀请、租户与工作区切换；请求按租户隔离数据，JWT 访问令牌 + 刷新令牌（doc/28、doc/34）。
- **工具循环**：文件读写/检索、`bash`、`web_search`、`search_history`、`memory`、`read_media_file` 等一批第一方工具。文件工具限制在 `-workspace` 内；`bash` 在 macOS 上用 seatbelt 把读写收进工作区，子代理要求沙箱可用。
- **子代理**：`agents/*.md` 用 YAML frontmatter + 正文定义规格，run 起手扫盘加载；`tools:` 白名单与 `exclude_tools:` 排除二选一；进程级并发闸与回传长度帽。出厂规格是 `coder` 与 `researcher`（doc/17、doc/26）。
- **MCP**：stdio 方式接入外部 server，工具以 `mcp__<server>__<工具名>` 进池；配置在 `yk-tako-eino/mcp.json`（示例 `mcp.example.json`），管理页可维护（doc/33）。
- **上下文预算**：cl100k 计数、超窗折叠（staged compaction）、任务中途折叠与模型溢出救援，保证长任务不因窗口被拒（doc/21、19v6-09）。
- **长期记忆与对话召回**：run 结束后异步抽取事实/偏好/决定进 `memory_items`，新会话机械召回注入；完整 run 同步索引进 `dialogue_chunks`，用 pg_search 的 BM25 跨会话召回（doc/29）。
- **媒体**：消息可带图片，字节按内容寻址落 `workspace/.media/`，对话与落库面只留占位行，模型按需 `read_media_file`；前端按 `GET /api/media` 渲染（doc/32）。
- **Web UI**：Vue 3 + Vite，登录/多租户、聊天、子代理过程折叠、配置页；开发时由 Vite 代理 `/api` 到引擎。

## 快速开始

### 一键开发环境

```bash
./scripts/dev.sh
```

前置条件：

- Go 1.27（脚本会把 `$HOME/sdk/go1.27.0/bin` 加进 PATH）
- Node.js >= 20.19（Vite 7 要求）
- Redis 与 Postgres：端口上已有实例就复用；空闲时脚本用本机 `redis-server` 和 `initdb/pg_ctl` 起本地实例（数据在 `tmp/pg-5433/`）
- `yk-tako-eino/.env` 里配好模型：`DEEPSEEK_API_KEY`、`DEEPSEEK_BASE_URL`、`TAKO_MODEL`

默认前端 <http://127.0.0.1:5173>、引擎 `127.0.0.1:8901`、Redis `127.0.0.1:6390`、Postgres `127.0.0.1:5433/tako`。页面只有登录表单；第一次用先建账号（注册即建个人租户，密码至少 8 位）：

```bash
curl -s -X POST http://127.0.0.1:8901/api/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"changeme123"}'
```

`Ctrl-C` 一起停。

```bash
./scripts/dev.sh status      # 有没有在跑（读 pidfile）
./scripts/dev.sh stop        # 发 TERM，走和 Ctrl-C 同一条收尾
./scripts/dev.sh restart     # 停掉再前台起

ENGINE_PORT=8905 WEB_PORT=5180 REDIS_ADDR=127.0.0.1:6392 PG_PORT=5434 ./scripts/dev.sh
```

脚本只在调用方没给够长的值时，为本机开发填 `TAKO_JWT_SECRET` / `TAKO_DATA_KEY` 的默认值；这两个密钥至少 32 字节，手动起引擎时必须自己 export。

### 手动起引擎

```bash
cd yk-tako-eino
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go run ./cmd/yktako \
  -engine 127.0.0.1:8901 \
  -redis 127.0.0.1:6390 \
  -pg "host=127.0.0.1 port=5433 dbname=tako sslmode=disable" \
  -workspace ../tmp/workspace
```

启动即校验模型配置、Redis、Postgres、`TAKO_JWT_SECRET` / `TAKO_DATA_KEY` 与分词器缓存，任一不可用直接退出，不留半死进程。其余参数（租约、压缩窗口、子代理并发、续跑次数、记忆开关等）见 `go run ./cmd/yktako -h`。

### 测试

```bash
cd yk-tako-eino
PATH="$HOME/sdk/go1.27.0/bin:$PATH" go test ./internal/runengine

cd web
npm run typecheck
```

单元测试不打模型，不需要 API Key。

## 目录

```text
yk-tako-eino/          Go 引擎 + Vue 前端
  cmd/yktako/          唯一入口
  internal/runengine/  运行引擎：API、worker、存储、工具、子代理、记忆、MCP
  internal/config/     模型厂商目录、模型客户端与系统提示词
  agents/              出厂子代理规格（coder.md / researcher.md）
  web/                 Vue 3 + Vite 前端
doc/                   当前方案与实现真源；索引见 doc/README.md
docs/                  历史交接与文章，不是施工真源
scripts/dev.sh         一键开发环境
tmp/                   本地执行台账（已忽略，不入库）
```

## 文档

- [doc/README.md](doc/README.md)：文档索引——哪些是现行真源、哪些是归档、`docs/` 里的历史文章怎么读。
- [doc/19-v6/19-运行引擎架构-v6.md](doc/19-v6/19-运行引擎架构-v6.md)：运行引擎架构入口。
- [AGENTS.md](AGENTS.md)：协作与施工纪律——用例驱动的 loop、todos 交审后才 coding、验收写成 curl 与期望字节。

## 仓库

两个远端同步同一份 `main`：

- `origin`（AtomGit）是主仓，**始终是最新代码**，部署从这里拉。
- `github`（GitHub）是镜像，可以落后，不能超前。

推送顺序永远是 **AtomGit → GitHub**：AtomGit 成功之后才推 GitHub。

日常推送用一条命令：

```bash
./scripts/sync-remotes.sh        # 等价于先 git push origin main，再 git push github main
```

每个 clone 执行一次 `git config core.hooksPath .githooks` 安装推送闸：
直接 `git push github` 时，若本地分支 SHA 与 AtomGit 同分支不一致，推送会被拒绝。
不要用 `git push --mirror`。
