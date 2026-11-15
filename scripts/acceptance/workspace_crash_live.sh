#!/usr/bin/env bash
# 隔离工作区崩溃活体（doc/74 §9.2/§10；施工包 §12.2 第 4 场）：脚本自起两台隔离引擎共连同一
# PG／Redis／workspace（沿 agent_task_two_replicas_live.sh），kill -9 只打自己起的 PID。七场——
#   1. 双副本对账：两台同时调度同一隔离组，每件恰好一场执行 run、发布恰好一次、交付恰好一次；
#   2. 准备中 kill：基线/工作目录复制到一半 kill → 重启幂等准备 → resume 跑完；
#   3. 执行中 kill：拿到活租约后 kill → 租约过期 → interrupted → resume 同一场 run（epoch+1）→ 旧代交不上结果；
#   4. 验证中 kill：verifyCommand 跑着 kill → resume 重新提交 → 验证重跑通过；
#   5. 结果接受中 kill：不可变结果固化到一半 kill → resume 重新提交 → 恰好一个版本、字节一致；
#   6. 发布中 kill：400 个文件发布到一半 kill → 发布标记仍持（新 run 409 117113）→ 重启同 epoch 续发 → 全量字节；
#   7. 发布后 kill：已发布已交付后 kill → 重启不重跑、不重交付、产物仍可读。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，一个空的隔离 PG 库；脚本建 pg_search 扩展）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/13（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT_A/B  默认 8959 / 8960（被占就报错退出，不杀别人的进程）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_LEASE_WAIT    等租约过期的秒数，默认 35（引擎 lease TTL 30s）
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎 PID。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8959}"
PORT_B="${TAKO_ENGINE_PORT_B:-8960}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/13}"
LEASE_WAIT="${TAKO_LEASE_WAIT:-35}"
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

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-ws-crash.XXXXXX")
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

start_engine(){ # $1=port $2=log → echo pid
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" -task-interval 1s
  ) >>"$2" 2>&1 &
  local pid=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 30s 没起来，日志：$2" >&2; exit 2
}
start_both(){ PID_A=$(start_engine "$PORT_A" "$ELOG_A"); PID_B=$(start_engine "$PORT_B" "$ELOG_B"); }
kill_both(){
  kill -9 "$PID_A" "$PID_B" 2>/dev/null
  for _ in $(seq 40); do
    kill -0 "$PID_A" 2>/dev/null || kill -0 "$PID_B" 2>/dev/null || break
    sleep 0.25
  done
}

echo "自起引擎 A 127.0.0.1:$PORT_A · B 127.0.0.1:$PORT_B · 共库 $DSN · 共享 workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
start_both
BA="http://127.0.0.1:$PORT_A"
B="$BA"
WS_ROOT="$WS"
ml_signup wscrash

# bash 执行者（造大文件/多文件用；researcher 排除 bash）：inherit 基础池。
AGENT="wscrash_$(date +%s)_$$"
jq -cn --arg n "$AGENT" \
  '{name:$n,kind:"subagent",description:"隔离工作区执行者",spec:"你是隔离工作目录的执行者。按任务内容改动文件，完成后调用 submit_task_result 提交，artifacts 声明本次交付的文件（相对路径）。如果提交被中断，重新提交。"}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @- >/dev/null

ws_dir(){ echo "$WS_ROOT/tenants/$TEN/workspaces/$1/runtime/orchestrations/$2/workspace"; }
task_field(){ at_task "$1"; echo "$AT_TASK" | jq -r "$2 // empty"; }
mk_ws(){ # $1=name → echo wid（并初始化项目容器布局，doc/76）
  local w; w=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"name":"'"$1"'"}' | jq -r .data.workspace.id)
  # 先打一次文件清单建好 layout.json 与 content/runtime；反序写文件会造出未迁移容器（409 117116）。
  curl -sS -o /dev/null "$B/api/workspaces/$w/files" -H "Authorization: Bearer $TOK"
  echo "$w"
}
# 崩溃恢复：等读侧派生 interrupted（租约过期）→ resume 同一场 run → succeeded；
# 若 kill 落在领取前（直接 succeeded）也认。$1=taskId $2=超时秒。
recover_task(){
  local tid="$1" t="${2:-300}" st="" deadline=$((SECONDS + ${2:-300}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_task_status "$tid")
    [ "$st" = "succeeded" ] && return 0
    [ "$st" = "interrupted" ] && break
    sleep 1
  done
  if [ "$st" = "interrupted" ]; then
    local res; res=$(curl -sS -X POST "$B/api/tasks/$tid/resume" -H "Authorization: Bearer $TOK")
    [ "$(echo "$res" | jq -r .code)" = "0" ] || { echo "resume 失败：$res" >&2; return 1; }
  fi
  at_wait_task "$tid" succeeded "$t"
}
artifact_eq(){ # $1=taskId $2=path $3=期望内容 $4=标签
  curl -sS "$B/api/tasks/$1/artifact?path=$2" -H "Authorization: Bearer $TOK" -o "$SCRATCH/got.bin"
  printf '%s' "$3" > "$SCRATCH/want.bin"
  if cmp -s "$SCRATCH/got.bin" "$SCRATCH/want.bin"; then ok "$4"; else no "$4 —— 字节不符"; fi
}

# ── 1. 双副本对账：两台同时调度同一隔离组 ────────────────────────────────────────
W1=$(mk_ws ws-crash-replica)
mkdir -p "$WS_ROOT/tenants/$TEN/workspaces/$W1/content"
printf '基线\n' > "$WS_ROOT/tenants/$TEN/workspaces/$W1/content/report.md"
at_probe_run "$W1"
STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "在工作目录创建 alpha.txt，内容恰好一行「甲-$STAMP」。然后调用 submit_task_result，artifacts 声明 alpha.txt（kind=file）。" \
  --arg b "在工作目录创建 beta.txt，内容恰好一行「乙-$STAMP」。然后调用 submit_task_result，artifacts 声明 beta.txt（kind=file）。" \
  --arg c "工作目录里已有上游的 alpha.txt 与 beta.txt。读齐两份，创建 final.txt：第一行 alpha 原文、第二行 beta 原文。然后调用 submit_task_result，artifacts 声明 final.txt（kind=file）。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"},{key:"c",content:$c,agent:"researcher"}]')
RESP=$(at_create_orch "acc-wscrash-rep-$STAMP" "$W1" "$SID" "双副本抢同一隔离组" "$TASKS" \
  '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]' "isolated")
OID1=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID1" ] && [ "$OID1" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  场1 orchId=$OID1（两台引擎都在看）"
if at_wait_orch "$OID1" "succeeded" 300; then ok "场1 整组收口 succeeded"; else no "场1 没收口（$AT_LAST）"; ml_summary; fi
at_orch "$OID1"
for k in a b c; do
  T=$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"$k\") | .taskId")
  eq "$k 恰好一个执行会话" "1" "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$T' AND execution_session_id IS NOT NULL")"
  eq "$k 恰好一场执行 run（双副本不重影）" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$T')")"
done
TID_C1=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
eq "发布恰好一次（epoch=1）" "1" "$(task_field "$TID_C1" '.workspace.publication.epoch')"
if at_wait_delivered "$OID1" 180; then ok "场1 交付完成"; else no "场1 没交付"; ml_summary; fi
eq "场1 来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"
echo "  ℹ️ 引擎 A 领取：$(grep -c 'task scheduler: claimed task' "$ELOG_A" 2>/dev/null) 次；引擎 B：$(grep -c 'task scheduler: claimed task' "$ELOG_B" 2>/dev/null) 次"

# ── 2. 准备中 kill：基线/工作目录复制到一半 ──────────────────────────────────────
W2=$(mk_ws ws-crash-prepare)
mkdir -p "$WS_ROOT/tenants/$TEN/workspaces/$W2/content"
head -c 33554432 /dev/zero | tr '\0' 'S' > "$WS_ROOT/tenants/$TEN/workspaces/$W2/content/seed.bin"  # 32MB 撑宽准备窗口
at_probe_run "$W2"
STAMP2="$(date +%s)-$$"
RESP=$(at_create_task "acc-wscrash-prep-$STAMP2" "$W2" "$SID" \
  "在工作目录创建 prep.txt，内容恰好一行「准备-$STAMP2」。然后调用 submit_task_result，artifacts 声明 prep.txt（kind=file）。" \
  "researcher" "isolated")
TID2=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID2=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID2" ] && [ "$TID2" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD2=$(ws_dir "$W2" "$OID2")
echo "  场2 taskId=$TID2 orchId=$OID2"
# 等领取后准备真的开始（staging 目录出现）再 kill。
SAW=""
for i in $(seq 1 600); do
  CUR=$(task_field "$TID2" '.currentRunId')
  if [ -n "$CUR" ]; then
    if ls "$GD2"/provider/.staging-baseline-* >/dev/null 2>&1 || ls "$GD2"/work/.staging-* >/dev/null 2>&1; then SAW=1; break; fi
  fi
  sleep 0.02
done
[ -n "$SAW" ] || { no "12s 没等到准备中的 staging 窗口"; ml_summary; }
ok "准备复制到一半 → kill -9 两台"
kill_both
sleep "$LEASE_WAIT"
start_both
if recover_task "$TID2" 300; then ok "场2 重启幂等准备 + resume 跑完"; else no "场2 没恢复到 succeeded（$AT_LAST）"; tail -30 "$ELOG_A"; ml_summary; fi
eq "场2 基线清单完整（重准备）" "1" "$(jq '.files | length' "$GD2/provider/baseline.json" | awk '{print ($1>=1)?1:0}')"
artifact_eq "$TID2" "prep.txt" "准备-$STAMP2
" "场2 产物字节一致"

# ── 3. 执行中 kill：活租约 → interrupted → resume 同一场 run（epoch+1）──────────────
W3=$(mk_ws ws-crash-exec)
at_probe_run "$W3"
STAMP3="$(date +%s)-$$"
RESP=$(at_create_task "acc-wscrash-exec-$STAMP3" "$W3" "$SID" \
  "在工作目录创建 exec.txt，内容恰好一行「执行-$STAMP3」。然后调用 submit_task_result，artifacts 声明 exec.txt（kind=file）。如果提交被中断，重新提交。" \
  "researcher" "isolated")
TID3=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID3=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID3" ] && [ "$TID3" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD3=$(ws_dir "$W3" "$OID3")
echo "  场3 taskId=$TID3 orchId=$OID3"
KILLED=0
for i in $(seq 1 90); do
  CUR=$(task_field "$TID3" '.currentRunId')
  if [ -n "$CUR" ]; then
    LIVE=$(ml_sql "SELECT count(*) FROM runs WHERE id='$CUR' AND status='running' AND lease_until > now()")
    if [ "$LIVE" = "1" ] && [ -d "$GD3/work/$TID3" ]; then KILLED=1; break; fi
  fi
  sleep 1
done
[ "$KILLED" = 1 ] || { no "90s 没等到执行中（活租约 + 工作目录已备）"; ml_summary; }
RUN3="$CUR"
ok "场3 执行中（run $RUN3，工作目录已备）→ kill -9 两台"
kill_both
sleep "$LEASE_WAIT"
start_both
ST=""
for i in $(seq 1 30); do ST=$(at_task_status "$TID3"); [ "$ST" = "interrupted" ] && break; sleep 1; done
eq "重启后读侧派生 interrupted（存储仍 running）" "interrupted" "$ST"
eq "run 行没被翻态（resume 前提）" "running" "$(ml_sql "SELECT status FROM runs WHERE id='$RUN3'")"
RES=$(curl -sS -X POST "$B/api/tasks/$TID3/resume" -H "Authorization: Bearer $TOK")
eq "resume 成功" "0" "$(echo "$RES" | jq -r .code)"
eq "接的是同一场 run" "$RUN3" "$(echo "$RES" | jq -r '.data.runId // ""')"
eq "run 行 lease_epoch 涨到 2" "2" "$(ml_sql "SELECT lease_epoch FROM runs WHERE id='$RUN3'")"
if at_wait_task "$TID3" "succeeded" 300; then ok "场3 续跑 succeeded"; else no "场3 没到 succeeded（$AT_LAST）"; ml_summary; fi
eq "旧代交不上结果（result_epoch=2）" "2" "$(ml_sql "SELECT result_epoch FROM agent_tasks WHERE id='$TID3'")"
eq "result_run_id 指回同一场 run" "$RUN3" "$(ml_sql "SELECT result_run_id FROM agent_tasks WHERE id='$TID3'")"
artifact_eq "$TID3" "exec.txt" "执行-$STAMP3
" "场3 产物字节一致"
if at_wait_delivered "$OID3" 180; then ok "场3 交付完成"; else no "场3 没交付"; ml_summary; fi

# ── 4. 验证中 kill：verifyCommand 跑着 kill → resume 重提交 → 验证重跑 ─────────────
W4=$(mk_ws ws-crash-verify)
at_probe_run "$W4"
STAMP4="$(date +%s)-$$"
RESP=$(at_create_task "acc-wscrash-verify-$STAMP4" "$W4" "$SID" \
  "在工作目录创建 v.txt，内容恰好一行「验证-$STAMP4」。然后调用 submit_task_result，artifacts 声明 v.txt（kind=file）。如果提交被中断，重新提交。" \
  "researcher" "isolated" 'sleep 8; test -f v.txt')
TID4=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID4=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID4" ] && [ "$TID4" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD4=$(ws_dir "$W4" "$OID4")
echo "  场4 taskId=$TID4 orchId=$OID4"
VSAW=""
for i in $(seq 1 900); do
  if [ -f "$GD4/verification/$TID4.json" ]; then
    VS=$(jq -r .status "$GD4/verification/$TID4.json" 2>/dev/null)
    [ "$VS" = "running" ] && { VSAW=1; break; }
  fi
  sleep 0.05
done
[ -n "$VSAW" ] || { no "45s 没等到验证 running 窗口"; at_task "$TID4"; echo "$AT_TASK" | jq -c '{status,workspace}'; ml_summary; }
ok "验证命令跑着（sleep 8）→ kill -9 两台"
kill_both
sleep "$LEASE_WAIT"
start_both
if recover_task "$TID4" 300; then ok "场4 resume 重提交后 succeeded"; else no "场4 没到 succeeded（$AT_LAST）"; tail -30 "$ELOG_A"; ml_summary; fi
eq "验证重跑通过（passed）" "passed" "$(task_field "$TID4" '.workspace.verification.status')"
eq "验证退出码 0" "0" "$(task_field "$TID4" '.workspace.verification.exitCode')"
[ -f "$GD4/verification/$TID4.out" ] && ok "验证输出留存（.out）" || no "验证输出没留存"
if at_wait_delivered "$OID4" 180; then ok "场4 交付完成"; else no "场4 没交付"; ml_summary; fi

# ── 5. 结果接受中 kill：不可变结果固化到一半 ─────────────────────────────────────
W5=$(mk_ws ws-crash-accept)
at_probe_run "$W5"
STAMP5="$(date +%s)-$$"
RESP=$(at_create_task "acc-wscrash-accept-$STAMP5" "$W5" "$SID" \
  "用 bash 在工作目录执行：head -c 67108864 /dev/zero | tr '\\0' 'B' > big.bin（造一个 64MB 文件）。然后调用 submit_task_result，summary 写「大文件」，artifacts 声明 big.bin（kind=file）。如果提交被中断，文件已存在就不要重新造，直接重新提交。" \
  "$AGENT" "isolated")
TID5=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID5=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID5" ] && [ "$TID5" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD5=$(ws_dir "$W5" "$OID5")
echo "  场5 taskId=$TID5 orchId=$OID5"
ASAW=""
for i in $(seq 1 1800); do
  if ls "$GD5/results/$TID5"/.staging-* >/dev/null 2>&1; then ASAW=1; break; fi
  sleep 0.02
done
[ -n "$ASAW" ] || { no "36s 没等到结果固化的 staging 窗口"; at_task "$TID5"; echo "$AT_TASK" | jq -c '{status,workspace}'; ml_summary; }
ok "不可变结果固化到一半 → kill -9 两台"
kill_both
sleep "$LEASE_WAIT"
start_both
if recover_task "$TID5" 300; then ok "场5 resume 重提交后 succeeded"; else no "场5 没到 succeeded（$AT_LAST）"; tail -30 "$ELOG_A"; ml_summary; fi
eq "恰好一个结果版本（resultRevision=1）" "1" "$(task_field "$TID5" '.resultRevision')"
[ -f "$GD5/results/$TID5/1/result.json" ] && ok "结果清单完整（result.json）" || no "结果清单缺失"
SZ=$(wc -c < "$GD5/results/$TID5/1/artifacts/big.bin" | tr -d ' ')
eq "固化产物字节数 64MB" "67108864" "$SZ"
if at_wait_delivered "$OID5" 180; then ok "场5 交付完成"; else no "场5 没交付"; ml_summary; fi

# ── 6. 发布中 kill：400 个文件发布到一半 → 标记仍持 → 重启同 epoch 续发 ─────────────
W6=$(mk_ws ws-crash-publish)
at_probe_run "$W6"
PROJ6="$WS_ROOT/tenants/$TEN/workspaces/$W6/content"
STAMP6="$(date +%s)-$$"
RESP=$(at_create_task "acc-wscrash-pub-$STAMP6" "$W6" "$SID" \
  "用 bash 在工作目录执行：mkdir -p data && for i in \$(seq -w 1 400); do head -c 262144 /dev/zero | tr '\\0' 'A' > \"data/f\$i.bin\"; done（造 400 个 256KB 文件）。然后调用 submit_task_result，summary 写「400 个文件」，artifacts 只声明 data/f001.bin（kind=file）即可（发布按改动清单全量应用）。" \
  "$AGENT" "isolated")
TID6=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID6=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID6" ] && [ "$TID6" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD6=$(ws_dir "$W6" "$OID6")
echo "  场6 taskId=$TID6 orchId=$OID6"
PUBFILE=""
for i in $(seq 1 1200); do
  for f in "$GD6"/publication/[0-9]*.json; do
    [ -f "$f" ] || continue
    DONE=$(jq '[.entries[] | select(.status=="done")] | length' "$f" 2>/dev/null)
    TOTAL=$(jq '.entries | length' "$f" 2>/dev/null)
    if [ "${DONE:-0}" -gt 0 ] 2>/dev/null && [ "${DONE:-0}" -lt "${TOTAL:-0}" ] 2>/dev/null; then PUBFILE="$f"; break; fi
  done
  [ -n "$PUBFILE" ] && break
  sleep 0.05
done
[ -n "$PUBFILE" ] || { no "60s 没等到部分发布窗口"; at_task "$TID6"; echo "$AT_TASK" | jq -c '{status,workspace}'; ml_summary; }
EPOCH6=$(jq -r .epoch "$PUBFILE")
ok "发布到一半（done=$(jq '[.entries[] | select(.status=="done")] | length' "$PUBFILE")/400，epoch=$EPOCH6）→ kill -9 两台"
kill_both
sleep 1
eq "崩后发布标记仍持（旧发布者占着项目）" "$OID6" "$(ml_sql "SELECT publication_orchestration_id FROM workspaces WHERE id='$W6'")"
start_both
# 重启对账窗口：标记还没清时，同项目新 run 必须 409 117113。
GOT117113=0
for i in $(seq 1 150); do
  R=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"messageId":"m_pubprobe_'"$RANDOM"'","text":"只回复 ok","workspaceId":"'"$W6"'"}')
  C=$(echo "$R" | jq -r .code)
  [ "$C" = "117113" ] && { GOT117113=1; break; }
  [ "$(at_task_status "$TID6")" = "succeeded" ] && break
  sleep 0.1
done
eq "重启对账期间同项目新 run 被 117113 挡住" "1" "$GOT117113"
if at_wait_task "$TID6" "succeeded" 300; then ok "场6 重启同 epoch 续发 succeeded"; else no "场6 没到 succeeded（$AT_LAST）"; tail -30 "$ELOG_A"; ml_summary; fi
eq "续发沿用同一 epoch" "$EPOCH6" "$(task_field "$TID6" '.workspace.publication.epoch')"
eq "发布记录全量 done" "400" "$(jq '[.entries[] | select(.status=="done")] | length' "$GD6/publication/$EPOCH6.json")"
eq "项目里 400 个文件齐了" "400" "$(ls "$PROJ6/data" | wc -l | tr -d ' ')"
WANT_SHA=$(head -c 262144 /dev/zero | tr '\0' 'A' | shasum -a 256 | cut -d' ' -f1)
for f in f001.bin f200.bin f400.bin; do
  SHA=$(shasum -a 256 "$PROJ6/data/$f" | cut -d' ' -f1)
  eq "data/$f 字节一致" "$WANT_SHA" "$SHA"
done
eq "发布标记已清" "" "$(ml_sql "SELECT publication_orchestration_id FROM workspaces WHERE id='$W6'")"
if at_wait_delivered "$OID6" 180; then ok "场6 交付完成"; else no "场6 没交付"; ml_summary; fi
eq "场6 恰好一场执行 run" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID6')")"

# ── 7. 发布后 kill：已发布已交付 → 重启不重跑、不重交付 ───────────────────────────
RUNS_BEFORE=$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID6')")
SESS_RUNS_BEFORE=$(at_session_runs "$SID")
ok "场7 已发布已交付 → kill -9 两台"
kill_both
sleep 3
start_both
sleep 8   # 给对账几轮：published 不该被重置、不该重跑
eq "发布后崩溃重启，任务仍 succeeded" "succeeded" "$(at_task_status "$TID6")"
eq "发布状态仍 published（不重置）" "published" "$(task_field "$TID6" '.workspace.publication.epoch' >/dev/null; task_field "$TID6" '.workspace.publication.status')"
eq "执行 run 不重影" "$RUNS_BEFORE" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID6')")"
eq "来源会话不重交付" "$SESS_RUNS_BEFORE" "$(at_session_runs "$SID")"
eq "orchestration_finished 通知仍恰好 1 条" "1" "$(at_notif_count "$OID6" "orchestration_finished")"
artifact_eq "$TID6" "data/f001.bin" "$(head -c 262144 /dev/zero | tr '\0' 'A')" "发布后崩溃重启，产物仍可读且字节一致"

ml_summary