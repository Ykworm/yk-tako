#!/usr/bin/env bash
# 继任组创建崩溃与双副本活体（doc/75 §8／§10.1 第 5、7、9 条；施工包 §10.2）：脚本自起两台
# 共享同一 PG／Redis／workspace 的隔离引擎，真模型跑原组，只对**自己起的进程** kill -9——
#   1. 双副本并发不同键：两键同时创建，只成功一个；另一个 409 117114；PG 只有一行继任组；
#   2. 双副本同键并发：两个副本都回 202 且指向同一个继任组，不产生第二组；
#   3. 创建中崩溃（PG 插入前）：原组行被外部事务锁住让创建停在事务入口，handoff 已落盘时
#      kill -9；重启后同键重试收敛成恰好一个继任组，孤儿文件目录不被当成继任组；
#   4. 创建中崩溃（PG 插入后、收尾前）：原组仍有活 run 时创建继任组，行一出现就 kill -9；
#      重启后调度对账补齐 handoff／复用快照／继任标记，原组收口、继任组才 active，
#      执行恰好一次、交付恰好一次、发布标记清空。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，空的隔离 PG 库；脚本在里面建 pg_search 扩展）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/15（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT   副本 A 端口，默认 8954（被占就报错退出，不杀别人的进程）
#   TAKO_ENGINE_PORT_B 副本 B 端口，默认 8955（同上）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎与 psql。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT:-8954}"
PORT_B="${TAKO_ENGINE_PORT_B:-8955}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/15}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
for p in "$PORT_A" "$PORT_B"; do
  if lsof -tiTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $p 已被占用；换 TAKO_ENGINE_PORT／TAKO_ENGINE_PORT_B（不杀别人的进程）" >&2; exit 2
  fi
done

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-replan-crash.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
PID_A=""
PID_B=""
LOCK_PID=""
LOCK_APP="tako-replan-crash-lock-$$"
cleanup(){
  # 外部事务的后端要单独终止：kill psql 客户端不会中止已在跑的 pg_sleep，锁会跟着后端留着。
  if [ -n "$LOCK_PID" ]; then
    ml_sql "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='$LOCK_APP'" >/dev/null 2>&1
  fi
  for pid in "$LOCK_PID" "$PID_A" "$PID_B"; do
    [ -n "$pid" ] || continue
    if kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null
      for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
      kill -9 "$pid" 2>/dev/null
    fi
  done
  [ "${TAKO_KEEP_SCRATCH:-0}" = "1" ] || rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"
export TAKO_WS_ROOT="$WS"
export TAKO_MEMORY_MODE=markdown

start_engine(){ # $1=端口 $2=日志 → sets 对应 PID 变量由调用方接
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" -task-interval 1s
  ) >>"$2" 2>&1 &
  echo $!
}

wait_engine(){ # $1=端口 $2=日志
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  echo "引擎 127.0.0.1:$1 30s 没起来，日志：$2" >&2
  tail -30 "$2" >&2
  echo "—— PG 会话与等待事件 ——" >&2
  "$PSQL" "$DSN" -c "SELECT pid,state,wait_event_type,wait_event,left(query,70) q FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()" >&2 2>&1
  "$PSQL" "$DSN" -c "SELECT locktype,mode,granted,pid FROM pg_locks WHERE NOT granted OR locktype='advisory'" >&2 2>&1
  exit 2
}

kill_engine(){ # $1=PID（只打脚本自己的进程）
  [ -n "$1" ] || return 0
  kill -9 "$1" 2>/dev/null
  for _ in $(seq 40); do kill -0 "$1" 2>/dev/null || return 0; sleep 0.1; done
}

# release_lock：终止外部事务的 PG 后端并等到它退出。
# 病灶（2026-11-12 活体实测）：`psql -c "...; SELECT pg_sleep(45)"` 的客户端被杀后，后端仍在
# 执行 pg_sleep 并持有行锁；此时重启的副本会在启动期 CREATE INDEX 上等锁超过 30s。
release_lock(){
  ml_sql "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='$LOCK_APP'" >/dev/null 2>&1
  for _ in $(seq 100); do
    [ -z "$(ml_sql "SELECT pid FROM pg_stat_activity WHERE application_name='$LOCK_APP'")" ] && return 0
    sleep 0.2
  done
  echo "外部事务后端没退出（application_name=$LOCK_APP）" >&2
  return 1
}

echo "自起双副本 127.0.0.1:$PORT_A + 127.0.0.1:$PORT_B · 库 $DSN · workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
PID_A=$(start_engine "$PORT_A" "$SCRATCH/engine-a.log")
PID_B=$(start_engine "$PORT_B" "$SCRATCH/engine-b.log")
wait_engine "$PORT_A" "$SCRATCH/engine-a.log"
wait_engine "$PORT_B" "$SCRATCH/engine-b.log"
B="http://127.0.0.1:$PORT_A"
BB="http://127.0.0.1:$PORT_B"
restart_all(){
  kill_engine "$PID_A"; kill_engine "$PID_B"
  PID_A=$(start_engine "$PORT_A" "$SCRATCH/engine-a.log")
  PID_B=$(start_engine "$PORT_B" "$SCRATCH/engine-b.log")
  wait_engine "$PORT_A" "$SCRATCH/engine-a.log"
  wait_engine "$PORT_B" "$SCRATCH/engine-b.log"
}

ml_signup replan_crash

new_project(){
  local r
  r=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"name":"'"$1"'"}' | jq -r .data.workspace.id)
  [ -n "$r" ] && [ "$r" != "null" ] || { echo "建项目失败" >&2; exit 2; }
  curl -sS "$B/api/workspaces/$r/files" -H "Authorization: Bearer $TOK" >/dev/null
  echo "$r"
}
cont_of(){ echo "$WS/tenants/$TEN/workspaces/$1"; }
gdir(){ echo "$(cont_of "$1")/runtime/orchestrations/$2"; }
res_of(){ echo "$(gdir "$1" "$2")/workspace/results/$3/$4"; }
handoff_dirs(){ ls -d "$(cont_of "$1")/runtime/orchestrations"/*/handoff.md 2>/dev/null | wc -l | tr -d ' '; }
succ_rows(){ ml_sql "SELECT count(*) FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$1:%%'"; }
wait_orch_any(){ # $1=orchId $2=正则 $3=超时秒
  local deadline=$((SECONDS + ${3:-300})) st
  AT_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_orch_status "$1"); AT_LAST="$st"
    printf '%s' "$st" | grep -Eq "$2" && return 0
    sleep 1
  done
  return 1
}
reuse_entry(){ jq -cn --arg t "$1" --argjson r "${2:-1}" '[{taskId:$t,resultRevision:$r}]'; }
succ_body(){ # $1=key $2=goal $3=taskContent $4=reuseJSON [$5=workspaceId]
  jq -cn --arg k "$1" --arg g "$2" --arg c "$3" \
    --argjson reuse "${4:-[]}" --arg ws "${5:-}" \
    '{idempotencyKey:$k,goalAssessment:{outcome:"feasible",evidence:"崩溃后按新路径仍能达成同一目标"},
      reason:"创建继任组时引擎崩溃",successorReason:"原组已不可继续，必须换路径重做",
      noReplay:["不要再次发送已经发出的外部通知"],reuse:$reuse,goal:$g,
      tasks:[{key:"s",content:$c,agent:"researcher"}],links:[]}
     + (if $ws != "" then {workspaceId:$ws} else {} end)'
}
succ_post(){ # $1=base $2=orchId $3=bodyfile $4=输出前缀（code/body 两个文件）
  curl -sS -o "$4.body" -w '%{http_code}' -X POST "$1/api/orchestrations/$2/successors" \
    -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @"$3" > "$4.code" 2>/dev/null
}
make_original(){ # $1=项目 $2=前缀 $3=任务内容 JSON [$4=links JSON] → 输出原组 id
  local w="$1" pfx="$2" tasks="$3" links="${4:-[]}" resp oid
  resp=$(at_create_orch "$pfx" "$w" "$SID" "崩溃活体原组" "$tasks" "$links")
  oid=$(echo "$resp" | jq -r '.data.orchestrationId // ""')
  [ -n "$oid" ] && [ "$oid" != "null" ] || { echo "建组失败：$resp" >&2; exit 2; }
  echo "$oid"
}

STAMP="$(date +%s)-$$"

# ── 1. 双副本并发不同键：只成功一个 ─────────────────────────────────────────────
W1=$(new_project crash-w1)
at_probe_run "$W1"
SID1="$SID"
O1=$(make_original "$W1" "crash-dual-$STAMP" \
  "$(jq -cn --arg c "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" '[{key:"a",content:$c,agent:"researcher"}]')")
[ -n "$O1" ] && [ "$O1" != "null" ] || { echo "场1 原组建不出来" >&2; exit 2; }
if at_wait_orch "$O1" "succeeded" 300; then ok "场1 原组已成功"; else no "场1 原组没成功（$AT_LAST）"; ml_summary; fi

succ_body "dual-a-$STAMP" "并发重规划 A" "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" > "$SCRATCH/dual-a.json"
succ_body "dual-b-$STAMP" "并发重规划 B" "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" > "$SCRATCH/dual-b.json"
succ_post "$BB" "$O1" "$SCRATCH/dual-a.json" "$SCRATCH/dual-a" &
succ_post "$B"  "$O1" "$SCRATCH/dual-b.json" "$SCRATCH/dual-b" &
wait
CODE_A=$(cat "$SCRATCH/dual-a.code"); CODE_B=$(cat "$SCRATCH/dual-b.code")
case "$CODE_A$CODE_B" in
  202409|409202) ok "场1 双副本并发不同键：一个 202、一个 409（A=$CODE_A B=$CODE_B）";;
  *) no "场1 并发返回不对（A=$CODE_A B=$CODE_B）";;
esac
LOSER_BODY=""
if [ "$CODE_A" = "202" ]; then WINNER=$(jq -r .data.orchestrationId "$SCRATCH/dual-a.body"); LOSER_BODY="$SCRATCH/dual-b.body"; else WINNER=$(jq -r .data.orchestrationId "$SCRATCH/dual-b.body"); LOSER_BODY="$SCRATCH/dual-a.body"; fi
eq "场1 落败方码 117114" "117114" "$(jq -r .code "$LOSER_BODY" 2>/dev/null)"
eq "场1 PG 只有一行继任组" "1" "$(succ_rows "$O1")"
eq "场1 successor.json 指向胜者" "$WINNER" "$(jq -r .successorOrchestrationId "$(gdir "$W1" "$O1")/successor.json" 2>/dev/null)"
eq "场1 只有胜者一个 handoff 目录" "1" "$(handoff_dirs "$W1")"
if at_wait_orch "$WINNER" "succeeded" 600; then ok "场1 胜者继任组执行并成功"; else no "场1 胜者没成功（$AT_LAST）"; ml_summary; fi
if at_wait_delivered "$WINNER" 300; then ok "场1 胜者交付完成"; else no "场1 胜者没交付"; ml_summary; fi

# ── 2. 双副本同键并发：两个副本回同一个继任组 ───────────────────────────────────
W2=$(new_project crash-w2)
at_probe_run "$W2"
SID2="$SID"
O2=$(make_original "$W2" "crash-same-$STAMP" \
  "$(jq -cn --arg c "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" '[{key:"a",content:$c,agent:"researcher"}]')")
[ -n "$O2" ] && [ "$O2" != "null" ] || { echo "场2 原组建不出来" >&2; exit 2; }
if at_wait_orch "$O2" "succeeded" 300; then ok "场2 原组已成功"; else no "场2 原组没成功（$AT_LAST）"; ml_summary; fi
succ_body "same-key-$STAMP" "同键并发重规划" "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" > "$SCRATCH/same.json"
succ_post "$BB" "$O2" "$SCRATCH/same.json" "$SCRATCH/same-a" &
succ_post "$B"  "$O2" "$SCRATCH/same.json" "$SCRATCH/same-b" &
wait
S2A=$(jq -r '.data.orchestrationId // ""' "$SCRATCH/same-a.body" 2>/dev/null)
S2B=$(jq -r '.data.orchestrationId // ""' "$SCRATCH/same-b.body" 2>/dev/null)
eq "场2 两副本同键都回 202" "202 202" "$(cat "$SCRATCH/same-a.code") $(cat "$SCRATCH/same-b.code")"
eq "场2 两副本指向同一个继任组" "$S2A" "$S2B"
eq "场2 PG 仍只有一行继任组" "1" "$(succ_rows "$O2")"
if at_wait_orch "$S2A" "succeeded" 600; then ok "场2 同键胜者执行并成功"; else no "场2 同键组没成功（$AT_LAST）"; ml_summary; fi
if at_wait_delivered "$S2A" 300; then ok "场2 同键组交付完成"; else no "场2 同键组没交付"; ml_summary; fi

# ── 3. 创建中崩溃（PG 插入前）：handoff 落后 kill -9 → 重启重试收敛 ──────────────
W3=$(new_project crash-w3)
at_probe_run "$W3"
SID3="$SID"
O3=$(make_original "$W3" "crash-prepg-$STAMP" \
  "$(jq -cn --arg c "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" '[{key:"a",content:$c,agent:"researcher"}]')")
[ -n "$O3" ] && [ "$O3" != "null" ] || { echo "场3 原组建不出来" >&2; exit 2; }
if at_wait_orch "$O3" "succeeded" 300; then ok "场3 原组已成功"; else no "场3 原组没成功（$AT_LAST）"; ml_summary; fi

succ_body "prepg-$STAMP" "插入前崩溃重规划" "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" > "$SCRATCH/prepg.json"
# 外部事务锁住原组行 45s：创建会在真正插入前停在 FOR UPDATE；此时 handoff 已落盘。
# PGAPPNAME 是给 release_lock 认后端用的（等一下要按它 terminate）。
PGAPPNAME="$LOCK_APP" "$PSQL" "$DSN" \
  -c "BEGIN; SELECT id FROM agent_orchestrations WHERE id='$O3' FOR UPDATE; SELECT pg_sleep(45);" \
  >/dev/null 2>&1 &
LOCK_PID=$!
sleep 1
succ_post "$B" "$O3" "$SCRATCH/prepg.json" "$SCRATCH/prepg" &
REQ_PID=$!
GOT=0
for _ in $(seq 1 100); do
  if [ "$(handoff_dirs "$W3")" -ge 1 ]; then GOT=1; break; fi
  sleep 0.2
done
eq "场3 创建已写到 handoff（事务尚未提交）" "1" "$GOT"
eq "场3 崩溃前 PG 还没有继任组行" "0" "$(succ_rows "$O3")"
kill_engine "$PID_A"
wait "$REQ_PID" 2>/dev/null
kill "$LOCK_PID" 2>/dev/null
LOCK_PID=""
if release_lock; then ok "场3 外部事务后端已终止"; else no "场3 外部事务后端没退出"; fi
# 锁真释放后才重启，否则重启副本的启动期 CREATE INDEX 会等在这里（30s 超时误报「起不来」）。
READY=0
for _ in $(seq 1 100); do
  if [ "$(ml_sql 'SELECT count(*) FROM pg_locks WHERE NOT granted')" = "0" ]; then READY=1; break; fi
  sleep 0.2
done
eq "场3 重启前 PG 没有等锁的会话" "1" "$READY"
eq "场3 崩溃后没有继任组行" "0" "$(succ_rows "$O3")"
eq "场3 崩溃后没有 successor.json" "0" "$([ -e "$(gdir "$W3" "$O3")/successor.json" ] && echo 1 || echo 0)"
eq "场3 崩溃留下孤儿 handoff 目录" "1" "$(handoff_dirs "$W3")"

PID_A=$(start_engine "$PORT_A" "$SCRATCH/engine-a.log"); wait_engine "$PORT_A" "$SCRATCH/engine-a.log"
succ_post "$B" "$O3" "$SCRATCH/prepg.json" "$SCRATCH/prepg-retry"
eq "场3 重启后同键重试回 202" "202" "$(cat "$SCRATCH/prepg-retry.code")"
S3=$(jq -r '.data.orchestrationId // ""' "$SCRATCH/prepg-retry.body")
eq "场3 重试后 PG 恰好一行继任组" "1" "$(succ_rows "$O3")"
eq "场3 successor.json 指向重试创建的组" "$S3" "$(jq -r .successorOrchestrationId "$(gdir "$W3" "$O3")/successor.json" 2>/dev/null)"
eq "场3 孤儿目录不被当成继任组（handoff 目录=孤儿+正式）" "2" "$(handoff_dirs "$W3")"
if at_wait_orch "$S3" "succeeded" 600; then ok "场3 重试创建的组执行并成功"; else no "场3 组没成功（$AT_LAST）"; ml_summary; fi
if at_wait_delivered "$S3" 300; then ok "场3 组交付完成"; else no "场3 组没交付"; ml_summary; fi

# ── 4. 创建中崩溃（PG 插入后、收尾前）：原组有活 run，行一出现就 kill -9 ─────────
W4=$(new_project crash-w4)
at_probe_run "$W4"
SID4="$SID"
TASKS4=$(jq -cn \
  --arg a "在项目根创建 c4-input.txt，内容写一行「C4-$STAMP」。然后调用 submit_task_result，summary 写路径，artifacts 声明 c4-input.txt（kind=file）。" \
  --arg b "先调用 ask_user 工具提问「崩溃等待-场4」，拿到回答后只回复 ok。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"}]')
O4=$(make_original "$W4" "crash-midpg-$STAMP" "$TASKS4" '[{"parent":"a","child":"b"}]')
[ -n "$O4" ] && [ "$O4" != "null" ] || { echo "场4 原组建不出来" >&2; exit 2; }
at_orch "$O4"
TA4=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TB4=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
if at_wait_task "$TB4" "running" 300; then ok "场4 原组最后一件任务在等人（活 run）"; else no "场4 b 没跑起来（$AT_LAST）"; ml_summary; fi
if at_wait_task_question "$TB4" 240; then ok "场4 原组问出了问题"; else no "场4 没等到提问"; ml_summary; fi
at_task "$TA4"
eq "场4 上游 a 已成功且版本=1" "1" "$(echo "$AT_TASK" | jq -r '.resultRevision // 0')"

succ_body "midpg-$STAMP" "插入后崩溃重规划" \
  "当前工作目录下有继承来的 c4-input.txt（上一代任务的有效结果）。读它，把原文抄进 c4-final.txt，再另起一行写「S4-$STAMP」，然后调用 submit_task_result，summary 写路径，artifacts 声明 c4-final.txt（kind=file）。" \
  "$(reuse_entry "$TA4" 1)" > "$SCRATCH/midpg.json"
succ_post "$B" "$O4" "$SCRATCH/midpg.json" "$SCRATCH/midpg" &
REQ4_PID=$!
SEEN=""
for _ in $(seq 1 400); do
  ROW=$(ml_sql "SELECT status FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$O4:%%' LIMIT 1")
  if [ -n "$ROW" ]; then SEEN="$ROW"; break; fi
  sleep 0.05
done
if [ -n "$SEEN" ]; then ok "场4 继任组行已落库（状态 $SEEN）→ 立即 kill -9 双副本"; else no "场4 20s 内没看到继任组行"; fi
kill_engine "$PID_A"; kill_engine "$PID_B"
wait "$REQ4_PID" 2>/dev/null
case "$SEEN" in
  preparing|waiting_previous) ok "场4 崩溃时继任组处于可对账状态（$SEEN）";;
  *) no "场4 崩溃时状态不是 preparing／waiting_previous（$SEEN）";;
esac
eq "场4 崩溃后 PG 恰好一行继任组" "1" "$(succ_rows "$O4")"

restart_all
# 对账：preparing／waiting_previous → 补齐 handoff／复用快照／标记 → 原组收口 → active。
if at_wait_orch "$(ml_sql "SELECT id FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$O4:%%' LIMIT 1")" "succeeded" 600; then
  ok "场4 重启对账后继任组真执行并成功"
else no "场4 继任组没收敛到 succeeded（$AT_LAST）"; ml_summary; fi
S4=$(ml_sql "SELECT id FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$O4:%%' LIMIT 1")
eq "场4 仍只有一行继任组（没有重复建组）" "1" "$(succ_rows "$O4")"
eq "场4 successor.json 指向对账后的组" "$S4" "$(jq -r .successorOrchestrationId "$(gdir "$W4" "$O4")/successor.json" 2>/dev/null)"
eq "场4 原组收口到 cancelled" "cancelled" "$(at_orch_field "$O4" status)"
eq "场4 原组活任务落到 cancelled" "cancelled" "$(ml_sql "SELECT status FROM agent_tasks WHERE id='$TB4'")"
eq "场4 原组活 run 已停（旧代交不上结果）" "stopped" "$(ml_sql "SELECT status FROM runs WHERE id=(SELECT current_run_id FROM agent_tasks WHERE id='$TB4')")"
eq "场4 继任组任务恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE orchestration_id='$S4')")"
eq "场4 复用副本在对账后的组里" "1" "$([ -f "$(res_of "$W4" "$S4" "$TA4" 1)/result.json" ] && echo 1 || echo 0)"
eq "场4 交付把复用证据与本次结果一起发布" "2" \
  "$(grep -c -e "C4-$STAMP" -e "S4-$STAMP" "$(cont_of "$W4")/content/c4-final.txt" 2>/dev/null)"
if at_wait_delivered "$S4" 300; then ok "场4 交付完成"; else no "场4 没交付"; ml_summary; fi
eq "场4 orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$S4" "orchestration_finished")"
eq "场4 发布标记已清空" "" "$(ml_sql "SELECT publication_orchestration_id FROM workspaces WHERE id='$W4'")"

ml_summary
