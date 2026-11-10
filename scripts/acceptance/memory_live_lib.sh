#!/usr/bin/env bash
# 记忆活体脚本（doc/65 §13.3）的共享底座：**source 用，不直接执行**。
# 六个 memory_*_live.sh / memory_file_race.sh 共用这一份，免得「六处各写一遍注册与拉帧」。
#
# 参数全部走环境变量（不写死本机端口、账号或路径）：
#   TAKO_BASE     引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV      取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN   直连 PG 的 DSN（行数/状态断言靠它；需要它的脚本自己检查必填）
#   TAKO_PSQL     psql 可执行文件，默认 psql
#   TAKO_WS_ROOT  引擎的 workspace 根（文件字节断言要用），默认 <仓库根>/tmp/workspace-acc
#   TAKO_MODEL    租户里配的模型 ref，默认 deepseek/deepseek-flash
#   TAKO_REDPANDA_BROKERS/USER/PASSWORD/MECH  broker 连接与凭据（只从环境变量读，不落任何文件）
#
# 纪律：kill 只打脚本自己起或调用方明确指定的隔离 PID；不调 /api/dev/reset；
# 注册密码每次随机生成（脚本里不出现密码字面量）；模型密钥只从 .env 读。

ML_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
B="${TAKO_BASE:-http://127.0.0.1:8943}"
ENV="${TAKO_ENV:-$ML_REPO_ROOT/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-psql}"
DSN="${TAKO_PG_DSN:-}"
WS_ROOT="${TAKO_WS_ROOT:-$ML_REPO_ROOT/tmp/workspace-acc}"
MODEL="${TAKO_MODEL:-deepseek/deepseek-flash}"

P=0; F=0
ok(){ echo "  ✅ $1"; P=$((P+1)); }
no(){ echo "  ❌ $1"; F=$((F+1)); }
eq(){ if [ "$2" = "$3" ]; then ok "$1"; else no "$1 —— 期望 [$2] 实际 [$3]"; fi; }
ml_summary(){ echo; echo "═══ 通过 $P 条，失败 $F 条 ═══"; [ "$F" -eq 0 ] || exit 1; }

# 计数/id/状态用（去空白）；读正文用 ml_sql_raw（保留空格）。
ml_sql(){ "$PSQL" "$DSN" -tAc "$1" 2>/dev/null | tr -d ' \n'; }
ml_sql_raw(){ "$PSQL" "$DSN" -tAc "$1" 2>/dev/null; }
ml_need_dsn(){ if [ -z "$DSN" ]; then echo "必填参数缺了：TAKO_PG_DSN（见各脚本头注）" >&2; exit 2; fi; }

ml_key(){ grep '^DEEPSEEK_API_KEY=' "$ENV" 2>/dev/null | head -1 | cut -d= -f2- | tr -d '"'; }

# 注册账号 + 配租户模型。 sets TOK/TEN/ML_UID。第二参 no_model 时只注册（迁移/锁类脚本不打模型）。
ml_signup(){
  local em="mlive_${1}_$(date +%s)_${RANDOM}@tako.test" pw="pw_${RANDOM}_$(date +%s)_$$"
  local resp
  resp=$(curl -sS -X POST "$B/api/auth/signup" -H 'Content-Type: application/json' \
    -d '{"email":"'"$em"'","password":"'"$pw"'"}')
  TOK=$(echo "$resp" | jq -r .data.accessToken)
  TEN=$(echo "$resp" | jq -r .data.tenant.id)
  ML_UID=$(echo "$resp" | jq -r .data.user.id)
  if [ -z "$TOK" ] || [ "$TOK" = "null" ] || [ -z "$TEN" ] || [ "$TEN" = "null" ]; then
    echo "注册失败：$resp" >&2; exit 2
  fi
  if [ "${2:-}" != "no_model" ]; then
    local key; key=$(ml_key)
    if [ -z "$key" ]; then echo "读不到模型密钥：${ENV}（用 TAKO_ENV 指定）" >&2; exit 2; fi
    local prc
    prc=$(curl -sS -X PUT "$B/api/tenants/$TEN/runtime-config" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
      -d '{"modelRef":"'"$MODEL"'","mcpServers":{},"secrets":{"DEEPSEEK_API_KEY":"'"$key"'"},"sandbox":{"network":false}}')
    if [ "$(echo "$prc" | jq -r .code 2>/dev/null)" != "0" ]; then
      echo "租户模型配置失败：$prc" >&2; exit 2
    fi
  fi
}

# 投一场 run。$1=正文 $2=workspaceId（可省）$3=sessionId（可省，接回同一场）。sets RID/SID。
ml_run(){
  local body resp
  body=$(jq -cn --arg t "$1" --arg m "m_live_$(date +%s)_$RANDOM" --arg w "${2:-}" --arg s "${3:-}" \
    '{messageId:$m,text:$t} + (if $w != "" then {workspaceId:$w} else {} end) + (if $s != "" then {sessionId:$s} else {} end)')
  resp=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$body")
  RID=$(echo "$resp" | jq -r .data.runId)
  SID=$(echo "$resp" | jq -r .data.sessionId)
  if [ -z "$RID" ] || [ "$RID" = "null" ]; then echo "投递失败：$resp" >&2; exit 2; fi
}

# 拉全部帧：必须跟着 nextOffset 走（单批上限 200，terminal 可能排在后面）。sets FRAMES（多段 JSON）。
ml_frames(){
  local off=0 body nxt
  FRAMES=""
  while :; do
    body=$(curl -sS "$B/api/runs/$RID/frames?eventOffset=$off" -H "Authorization: Bearer $TOK" 2>/dev/null)
    FRAMES="$FRAMES$body
"
    nxt=$(printf '%s' "$body" | jq -r '.data.nextOffset // 0' 2>/dev/null)
    [ -z "$nxt" ] && nxt=0
    [ "$nxt" = "$off" ] && break
    off="$nxt"
  done
}

ml_answer(){ printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="delta") | .text] | join("")' 2>/dev/null; }

ml_wait_terminal(){ # $1=超时秒，默认 120
  local deadline=$((SECONDS + ${1:-120})) n
  while [ "$SECONDS" -lt "$deadline" ]; do
    ml_frames
    n=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="terminal")] | length' 2>/dev/null)
    if [ "${n:-0}" -ge 1 ] 2>/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

# 等会话落定：terminal 帧比 DB 翻终态早一拍，只等帧会撞假失败。
ml_wait_settled(){ # $1=超时秒，默认 30
  local deadline=$((SECONDS + ${1:-30})) body st live
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(curl -sS "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK" 2>/dev/null)
    st=$(echo "$body" | jq -r '.data.lastRunStatus // ""')
    live=$(echo "$body" | jq -r '.data.liveRunId // ""')
    case "$st" in complete|error|stopped) if [ -z "$live" ] || [ "$live" = "null" ]; then return 0; fi;; esac
    sleep 0.5
  done
  return 1
}

# 等本租户的记忆任务收口（无 pending/running）。failed 不在等待集——调用方自己断言。
ml_wait_jobs(){ # $1=超时秒，默认 90
  local deadline=$((SECONDS + ${1:-90})) n
  while [ "$SECONDS" -lt "$deadline" ]; do
    n=$(ml_sql "SELECT count(*) FROM memory_jobs WHERE tenant_id='$TEN' AND status IN ('pending','running')")
    if [ "${n:-1}" = "0" ]; then return 0; fi
    sleep 1
  done
  return 1
}

# broker 前置：没给地址或 TCP 连不上 → 明确报「缺 broker，未跑」，退出非零（不静默跳过当通过）。
ml_broker_check(){
  local brokers="${TAKO_REDPANDA_BROKERS:-}"
  if [ -z "$brokers" ]; then echo "缺 broker，未跑：没给 TAKO_REDPANDA_BROKERS" >&2; exit 2; fi
  # 拆两条语句：一条 local 里后项引用前项，展开发生在赋值前，会拿到空值。
  local first="${brokers%%,*}"
  local host="${first%:*}" port="${first##*:}"
  local sock_err
  if ! sock_err=$(python3 -c 'import socket,sys; socket.create_connection((sys.argv[1],int(sys.argv[2])),5).close()' "$host" "$port" 2>&1); then
    echo "缺 broker，未跑：$first 连不上（$sock_err）" >&2; exit 2
  fi
}