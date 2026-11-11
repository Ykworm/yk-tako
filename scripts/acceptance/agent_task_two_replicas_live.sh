#!/usr/bin/env bash
# 多副本领取活体（doc/70 §6.2；施工包 §12.3 第 5 场）：两台隔离引擎共连同一 PG／Redis、
# 调度循环都开着，同时盯着同一批 ready 的 Task——领取 CAS + 执行会话唯一约束必须保证
# **恰好一台**铸了恰好一场执行 run；执行完交付也恰好一次。多副本下不能出现重影执行。
#
# 本脚本自己起停两台引擎（kill 只打自己起的 PID），起法沿 memory_file_race.sh：
#   靶子库：TAKO_PG_DSN 指向一个空的隔离 PG 库（两台共用；脚本在里面建 pg_search 扩展）。
#   Redis：TAKO_REDIS_URL（默认 redis://127.0.0.1:6390/11，避开其它脚本占用的 db）。
#   端口：TAKO_ENGINE_PORT_A/B（默认 8956 / 8957，被占就报错退出，不杀别人的进程）。
#   memory 模式固定 markdown（免 broker；本场景不验记忆行为）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8956}"
PORT_B="${TAKO_ENGINE_PORT_B:-8957}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/11}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
for p in "$PORT_A" "$PORT_B"; do
  if lsof -tiTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $p 已被占用；换 TAKO_ENGINE_PORT_A/B（不杀别人的进程）" >&2; exit 2
  fi
done

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-task-replica.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG_A="$SCRATCH/engine-a.log"
ELOG_B="$SCRATCH/engine-b.log"
PID_A=""; PID_B=""
cleanup(){
  for pid in "$PID_A" "$PID_B"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null
      for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
      kill -9 "$pid" 2>/dev/null
    fi
  done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"
# 两台引擎同一 JWT 密钥：A 签发的 token 在 B 上同样有效（多副本同身份）。

start_engine(){ # $1=port $2=log
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS"
  ) >>"$2" 2>&1 &
  local pid=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 30s 没起来，日志：$2" >&2; exit 2
}

echo "引擎 A 127.0.0.1:$PORT_A · 引擎 B 127.0.0.1:$PORT_B · 共库 $DSN · 共享 workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
PID_A=$(start_engine "$PORT_A" "$ELOG_A")
PID_B=$(start_engine "$PORT_B" "$ELOG_B")
BA="http://127.0.0.1:$PORT_A"
BB="http://127.0.0.1:$PORT_B"
B="$BA"
ml_signup taskreplica

# 来源会话必须挂着项目（创建 Task 要求两者一致），所以先建项目、探针 run 带上它。
WID=$(curl -sS -X POST "$BA/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-replica-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
# ── 1. 经引擎 A 投一张菱形图：a、b 无父先领（两台引擎同时抢），c 汇合作唯一终态 ────────
# 注：一张图必须恰好一个无子节点的最终 Task（117101 校验），所以并行件不能裸投。
STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "在项目根创建文件 replica-a.txt，内容写一行「A-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  --arg b "在项目根创建文件 replica-b.txt，内容写一行「B-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  --arg c "上游任务 a 与 b 各留下一份证据文件（路径在他们结果的 summary 里）。读齐两份证据，把两份原文都写进项目根的 replica-final.txt（一段不少），然后调用 submit_task_result，summary 写 replica-final.txt 的路径。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"},{key:"c",content:$c,agent:"researcher"}]')
RESP=$(at_create_orch "acc-replica-$STAMP" "$WID" "$SID" "两件并行件两台引擎抢着领，c 汇合" "$TASKS" '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]')
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID" ] && [ "$OID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  orchId=$OID（两件 ready 同时挂着，A/B 引擎都在看）"

# ── 2. 等整组收口（谁领到都行，但每件只能有一场执行）──────────────────────────────
if at_wait_orch "$OID" "succeeded" 300; then
  ok "两件都执行完、组收口 succeeded"
else no "300s 没收口（组状态 $AT_LAST）"; at_orch "$OID"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi

at_orch "$OID"
for k in a b c; do
  eq "$k succeeded" "succeeded" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"$k\") | .status")"
done
# 领取唯一性的硬断言：每件 Task 恰好一个执行会话、恰好一场执行 run——
# 两台引擎同时调度，重影执行会在这里现形。
for k in a b c; do
  T=$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"$k\") | .taskId")
  eq "$k 恰好一个执行会话" "1" \
    "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$T' AND execution_session_id IS NOT NULL")"
  eq "$k 恰好一场执行 run" "1" \
    "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$T')")"
done
eq "agent_notifications 全组恰好 1 条（整组完成）" "1" "$(at_notif_count "$OID" "orchestration_finished")"

# ── 3. 交付恰好一次（交付 run 由某一台引擎铸，来源会话不重影）────────────────────
if at_wait_delivered "$OID" 180; then
  ok "来源会话交付完成"
else no "180s 没等到交付（deliveryStatus=$(at_orch_field "$OID" deliveryStatus)）"; ml_summary; fi
eq "来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"

# 观察（不作断言）：两台引擎的领取分布——谁抢到归谁，日志里各留一行 launching。
echo "  ℹ️ 引擎 A 领取记录：$(grep -c 'task scheduler: claimed task' "$ELOG_A" 2>/dev/null) 次；引擎 B：$(grep -c 'task scheduler: claimed task' "$ELOG_B" 2>/dev/null) 次"

ml_summary
