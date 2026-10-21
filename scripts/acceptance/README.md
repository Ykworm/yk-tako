# scripts/acceptance —— 可机器重跑的验收

这里的脚本是**资产**，不是台账：它们进版本库，跟着仓库走，换机器也能跑。
（`tmp/` 下的东西是本地执行台账，不入库——验收脚本放那里等于没有，见 AGENTS.md「文档状态与真源」。）

## 里面有什么

| 文件 | 干什么 | 要不要模型 |
|---|---|---|
| `expected.json` | 契约字节的**机器真源**：每条 = 用例号 + 请求 + 期望 HTTP 码／业务码／文案／data 字段序 | — |
| `assert_contract.py` | 读 `expected.json` 逐条打引擎比对，红打 ❌、失败非零退出 | 核心用例不要；model 层要 |
| `matrix.py` | 19v6-04 用例矩阵**实跑**：逐字节打印真实响应，供人肉对齐期望字节 | 要 |
| `spotcheck.sh` | doc/28 多租户 / doc/35 项目目录 / doc/32 media_offload 的**活体抽验**（shell + jq） | 要 |
| `check-expected.sh` | 守「19v6-04 散文」与「`expected.json`」的用例号集合一致 | — |
| `harness.py` | 三个脚本共用的底座：HTTP 调用、.env 取密钥、psql 补丁、断言计数 | — |

## 怎么起一个隔离引擎（靶子）

**别拿开发库当靶子**——脚本会注册账号、建项目、删默认工作区、留会话。
按下面这套起一个隔离实例：独立 PG 库 + 独立 Redis db + 独立 workspace。

```bash
# 1) 隔离 PG 库（dev.sh 的 PG 实例在 5433）
#    createdb / psql 不在 PATH 时用绝对路径，例如
#    PGBIN="$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin"
"$PGBIN/createdb" -h 127.0.0.1 -p 5433 tako_acc 2>/dev/null || true

# 2) 装 pg_search 扩展——**少了它引擎起不来**（对话召回要它）
"$PGBIN/psql" -h 127.0.0.1 -p 5433 -d tako_acc -c "CREATE EXTENSION pg_search CASCADE;"

# 3) 隔离引擎（Redis db 7、workspace 落在 tmp/ 下）
cd yk-tako-eino
PATH="$HOME/sdk/go1.27.0/bin:$PATH" \
TAKO_JWT_SECRET='dev-only-jwt-secret-change-me-32bytes' \
TAKO_DATA_KEY='data-key-aaaaaaaaaaaaaaaaaaaaaaaa32' \
go run ./cmd/yktako \
  -engine 127.0.0.1:8943 \
  -pg "host=127.0.0.1 port=5433 dbname=tako_acc sslmode=disable" \
  -redis "redis://127.0.0.1:6390/7" \
  -workspace ../tmp/workspace-acc
```

两个容易踩的点：

- **Redis 的 db 索引要用 URL 形式**（`redis://host:port/db`）。写 `127.0.0.1:6390/7` 会被当成主机名，引擎报 `lookup tcp/6390/7: unknown port`。
- **`pg_search` 扩展没装，引擎直接退出**（不是降级，是 fail fast）。隔离库要单独装一次。

## 怎么跑

```bash
# 契约断言（机器真源这一侧）：红了退出码非零
TAKO_BASE=http://127.0.0.1:8943 TAKO_ENV=yk-tako-eino/.env \
  python3 scripts/acceptance/assert_contract.py

# 只跑不需要模型的核心用例（快，几秒）
... python3 scripts/acceptance/assert_contract.py --tier core

# 矩阵实跑（打印真实字节，人肉对齐用）
TAKO_BASE=... TAKO_ENV=... python3 scripts/acceptance/matrix.py

# 活体抽验（doc/28 / doc/35 / doc/32）
TAKO_BASE=... TAKO_ENV=... bash scripts/acceptance/spotcheck.sh

# 散文 ↔ 机器真源的用例号一致性
bash scripts/acceptance/check-expected.sh
```

参数：

| 变量 | 默认 | 说明 |
|---|---|---|
| `TAKO_BASE` | `http://127.0.0.1:8943` | 引擎地址 |
| `TAKO_ENV` | `<仓库根>/yk-tako-eino/.env` | 取模型密钥的 .env |
| `TAKO_MODEL` | `deepseek/deepseek-flash` | 租户里配的模型 ref（只有 `matrix.py` 用） |
| `TAKO_PG_DSN` | 空 | 直连 PG 的 DSN。**`4-busy-rejected` 那条要它**（要造「running + 活租约」的忙闸）；不给就跳过并明说，不假装跑过 |
| `TAKO_PSQL` | `psql` | psql 可执行文件路径 |

依赖：`python3`、`curl`、`jq`；`psql` 只在要造忙闸时用。

## 期望字节在哪

- **机器真源**：`expected.json`。
- **人读版**：`doc/19-v6/19v6-04-验收-curl.md`（散文，用例 → curl → 期望字节）。

两份**同批改**。改了散文就要改清单，`check-expected.sh` 守着两边的用例号集合一致；
清单里 `declared_not_asserted` 显式登记「声明了但暂不断言」的用例与原因（不假装覆盖）。

### 清单怎么写

```jsonc
{
  "id": "3-unknown-session",
  "case": "用例 3",          // 必须与 19v6-04 的「用例 N」对得上
  "tier": "core",            // core=不需要模型，必须全过；model=要真跑一轮
  "desc": "未知 sessionId：三个端点一律 404 112001",
  "steps": [
    {
      "method": "GET", "path": "/api/sessions/s_nope",
      "auth": true,                    // 默认带 token；false = 不带（测 401）
      "capture": {"sid": "data.sessionId"},   // 存成变量，后面的 {sid} 引用
      "expect": {
        "http": 404,
        "code": 112001,
        "message": "找不到这场会话",
        "data": null,                   // 子集比对：只断点名的键
        "data_keys": ["a", "b"],        // 键集与键序必须逐字相同（要精确就写这个）
        "frame_kinds_contains": ["start", "terminal"],
        "rows_len_equals": "{rows_before}"
      }
    }
  ]
}
```

步骤类型：`op=signup`（建账号+租户+配模型）、`op=wait_terminal`（等这场跑完出 terminal 帧）、
`op=wait_settled`（等**会话落定**——terminal 帧比 DB 翻终态早一拍，只等帧会撞出假失败）、
带 `sql` 的步骤（请求前先跑一句 SQL）。

## 什么时候必须跑

- 改了契约字节（端点的请求/响应/帧格式/业务码）→ 跑 `assert_contract.py`，
  并同步 `doc/19-v6/19v6-04-验收-curl.md` 与 `expected.json`。
- 改了存储形态、游标语义、多租户可见性 → 跑 `spotcheck.sh`。
- 只改了模型侧的散文期望 → 也要跑 `check-expected.sh`，确认清单与散文没脱节。
