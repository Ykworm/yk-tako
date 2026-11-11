#!/usr/bin/env bash
# 调度唤醒活体（doc/72 §9；施工包 §11.3 第 4 场）：
#   1. 快速发现：两台调度器都配 1h 对账周期，新建 ready Task 仍被立即领取——走的是事件唤醒，
#      不是周期轮询；两副本同时被唤醒，领取 CAS 保证恰好一场执行。
#   2. 流丢失：直接种一行 ready Task（不经过 API，因此没有唤醒消息），2s 对账周期把它补上；
#      领取后唤醒流仍为空，证明这不是事件路径。
#   3. 启动对账：调度器重启后的第一轮 RunOnce 就能补齐停机期间积压的 ready Task（周期设 1h）。
#
# 本脚本自己起停四台隔离引擎（kill 只打自己起的 PID）：
#   TAKO_PG_DSN      空的隔离 PG 库（**必填**；库可共用，脚本只建扩展）
#   TAKO_REDIS_URL   默认 redis://127.0.0.1:6390/13（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT_A/C/D/E  默认 8971/8972/8973/8974（被占就报错退出，不杀别人的进程）
# 需要本机有 python3（用来对 Redis 发 DEL/EXISTS 原生命令，环境里没有 redis-cli）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8971}"
PORT_C="${TAKO_ENGINE_PORT_C:-8972}"
PORT_D="${TAKO_ENGINE_PORT_D:-8973}"
PORT_E="${TAKO_ENGINE_PORT_E:-8974}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/13}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
for p in "$PORT_A" "$PORT_C" "$PORT_D" "$PORT_E"; do
  if lsof -tiTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $p 已被占用；换 TAKO_ENGINE_PORT_A/C/D/E（不杀别人的进程）" >&2; exit 2
  fi
done

# redis_int $1=url 其余=命令 → 打印整数回复（DEL/EXISTS 用）。
redis_int(){
  python3 - "$@" <<'PY'
import sys, socket, re
url, args = sys.argv[1], sys.argv[2:]
m = re.match(r'redis://([^:/]+):(\d+)(?:/(\d+))?', url)
if not m:
    print("bad redis url: " + url, file=sys.stderr); sys.exit(2)
host, port, db = m.group(1), int(m.group(2)), int(m.group(3) or 0)
def enc(*words):
    out = b'*%d\r\n' % len(words)
    for w in words:
        b = w.encode()
        out += b'$%d\r\n%s\r\n' % (len(b), b)
    return out
s = socket.create_connection((host, port), 5)
s.sendall(enc('SELECT', str(db))); s.recv(1024)
s.sendall(enc(*args))
buf = s.recv(4096)
s.close()
if not buf.startswith(b':'):
    print(buf.decode(errors='replace').strip(), file=sys.stderr); sys.exit(1)
print(buf[1:].split(b'\r\n')[0].decode())
PY
}

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-task-wake.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
declare -a PIDS=()
cleanup(){
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    kill -0 "$pid" 2>/dev/null || continue
    kill -TERM "$pid" 2>/dev/null
    for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$pid" 2>/dev/null
  done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"
# 所有副本同一 JWT 密钥：一台签发的 token 在另一台上同样有效。

start_engine(){ # $1=port $2=log $3=额外参数（可省）
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    # shellcheck disable=SC2086
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" $3
  ) >>"$2" 2>&1 &
  local pid=$!
  PIDS+=("$pid")
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 30s 没起来，日志：$2" >&2; exit 2
}

stop_pid(){ # $1=pid：TERM 并等退出（XREAD 最长 5s 才响应 ctx，留足时间）。
  local pid="$1"
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null
  for _ in $(seq 80); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.25; done
  kill -9 "$pid" 2>/dev/null
}

"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }

echo "A=$PORT_A C=$PORT_C（调度 1h 对账） D=$PORT_D（只提供 API）"
PID_A=$(start_engine "$PORT_A" "$SCRATCH/a.log" "-task-interval=1h")
PID_C=$(start_engine "$PORT_C" "$SCRATCH/c.log" "-task-interval=1h")
PID_D=$(start_engine "$PORT_D" "$SCRATCH/d.log" "-task-interval=-1s")
BA="http://127.0.0.1:$PORT_A"
BC="http://127.0.0.1:$PORT_C"
BD="http://127.0.0.1:$PORT_D"
B="$BD"
ml_signup taskwake

WID=$(curl -sS -X POST "$BD/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-wake-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"

STAMP="$(date +%s)-$$"

# 来源会话先忙线（挂一个等人 run）：整组完成通知不认领，就没有交付 run 的 run_end 唤醒，
# 下面「没有事件、只靠周期」的断言才不会被无关唤醒污染。
ml_run "调用 ask_user，问题固定为「忙线占位-$STAMP」，拿到回答后只回复 ok。" "$WID" "$SID"
BUSY_OK=0
for i in $(seq 1 120); do
  if [ "$(curl -sS "$BD/api/runs/$RID/questions?status=pending" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '.data | length' 2>/dev/null)" != "0" ]; then BUSY_OK=1; break; fi
  sleep 1
done
[ "$BUSY_OK" = 1 ] || { echo "占位 run 没问出问题" >&2; exit 2; }

# wait_claimed $1=taskId $2=超时秒 → 等到 status 离开 ready（running/succeeded 都算被领取）。
wait_claimed(){
  local deadline=$((SECONDS + ${2:-15})) st
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_task_status "$1")
    case "$st" in running|succeeded) return 0;; failed|blocked) echo "$st"; return 0;; esac
    sleep 0.5
  done
  return 1
}

# ── 1. 事件唤醒：1h 周期下新建 Task 仍被立即领取；两副本不重影 ──────────────────
RESP=$(at_create_task "acc-wake-fast1-$STAMP" "$WID" "$SID" \
  "在项目根创建文件 wake-1.txt，内容写一行「W1-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  "researcher")
T1=$(echo "$RESP" | jq -r '.data.taskId // ""')
O1=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
T0=$SECONDS
if wait_claimed "$T1" 10; then
  ok "1h 对账周期下新建 ready Task 仍在 $((SECONDS-T0))s 内被领取（事件唤醒生效）"
else no "10s 内没被领取（最后 $(at_task_status "$T1")）"; ml_summary; fi
eq "第一件 Task 恰好一个执行会话" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$T1' AND execution_session_id IS NOT NULL")"
eq "第一件 Task 恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$T1')")"

RESP=$(at_create_task "acc-wake-fast2-$STAMP" "$WID" "$SID" \
  "在项目根创建文件 wake-2.txt，内容写一行「W2-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  "researcher")
T2=$(echo "$RESP" | jq -r '.data.taskId // ""')
if wait_claimed "$T2" 10; then
  ok "两副本同时被唤醒，第二件 Task 也在 10s 内被领取"
else no "第二件 10s 内没被领取"; ml_summary; fi
eq "两副本下第二件 Task 恰好一个执行会话" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$T2' AND execution_session_id IS NOT NULL")"
eq "两副本下第二件 Task 恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$T2')")"

# ── 2. 流丢失：种一行 ready Task（没有唤醒消息），2s 对账补上 ────────────────────
stop_pid "$PID_A"; stop_pid "$PID_C"
PID_E=$(start_engine "$PORT_E" "$SCRATCH/e.log" "-task-interval=2s")
BE="http://127.0.0.1:$PORT_E"
B="$BE"
redis_int "$REDIS_URL" DEL tk:agent-task-wake >/dev/null || { no "Redis DEL 失败"; ml_summary; }
eq "清空后唤醒流为空" "0" "$(redis_int "$REDIS_URL" EXISTS tk:agent-task-wake)"

SEED_O="or_wake_seed1_$STAMP"
SEED_T="at_wake_seed1_$STAMP"
"$PSQL" "$DSN" -v ON_ERROR_STOP=1 >/dev/null <<SQL || { no "种 Task 失败"; ml_summary; }
INSERT INTO agent_orchestrations
  (id, tenant_id, workspace_id, source_session_id, goal, status, final_task_id, idempotency_key, plan_hash, created_at, updated_at)
VALUES
  ('$SEED_O','$TEN','$WID','$SID','周期性对账种子','active','$SEED_T','seed-1-$STAMP','',now(),now());
INSERT INTO agent_tasks
  (id, orchestration_id, task_key, tenant_id, workspace_id, content, agent_name, status, created_at, updated_at)
VALUES
  ('$SEED_T','$SEED_O','seed','$TEN','$WID',
   '在项目根创建文件 seed-1.txt，内容写一行「SEED1-$STAMP」。然后调用 submit_task_result，summary 写文件路径。',
   'researcher','ready',now(),now());
SQL
T0=$SECONDS
if wait_claimed "$SEED_T" 20; then
  ok "没有唤醒消息的 ready Task 被 2s 周期对账补上（$((SECONDS-T0))s）"
else no "20s 内周期对账没补上（最后 $(at_task_status "$SEED_T")）"; ml_summary; fi
eq "补上后唤醒流仍为空（不是事件路径）" "0" "$(redis_int "$REDIS_URL" EXISTS tk:agent-task-wake)"
eq "种子 Task 恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$SEED_T')")"

# ── 3. 启动对账：停机期间积压的 ready Task，重启后的第一轮就补上（周期 1h）──────────
stop_pid "$PID_E"
redis_int "$REDIS_URL" DEL tk:agent-task-wake >/dev/null 2>&1
SEED2_O="or_wake_seed2_$STAMP"
SEED2_T="at_wake_seed2_$STAMP"
"$PSQL" "$DSN" -v ON_ERROR_STOP=1 >/dev/null <<SQL || { no "种第二件 Task 失败"; ml_summary; }
INSERT INTO agent_orchestrations
  (id, tenant_id, workspace_id, source_session_id, goal, status, final_task_id, idempotency_key, plan_hash, created_at, updated_at)
VALUES
  ('$SEED2_O','$TEN','$WID','$SID','启动对账种子','active','$SEED2_T','seed-2-$STAMP','',now(),now());
INSERT INTO agent_tasks
  (id, orchestration_id, task_key, tenant_id, workspace_id, content, agent_name, status, created_at, updated_at)
VALUES
  ('$SEED2_T','$SEED2_O','seed','$TEN','$WID',
   '在项目根创建文件 seed-2.txt，内容写一行「SEED2-$STAMP」。然后调用 submit_task_result，summary 写文件路径。',
   'researcher','ready',now(),now());
SQL
PID_E2=$(start_engine "$PORT_E" "$SCRATCH/e2.log" "-task-interval=1h")
BE="http://127.0.0.1:$PORT_E"
B="$BE"
T0=$SECONDS
if wait_claimed "$SEED2_T" 15; then
  ok "重启后的启动对账在 $((SECONDS-T0))s 内领取了积压 Task（周期 1h）"
else no "15s 内启动对账没领取（最后 $(at_task_status "$SEED2_T")）"; ml_summary; fi
eq "启动对账领取的 Task 恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$SEED2_T')")"
eq "启动对账路径也没有唤醒消息" "0" "$(redis_int "$REDIS_URL" EXISTS tk:agent-task-wake)"

echo "  ℹ️ 唤醒流消息总数（领取不依赖消费者组）：$(redis_int "$REDIS_URL" XLEN tk:agent-task-wake 2>/dev/null || echo '?')"
ml_summary
