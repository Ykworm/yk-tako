#!/usr/bin/env bash
# 编排并发档位与路径账本活体（doc/82/74；施工包 §12.2 场景 8/12）：
#   8a shared 档串行执行、8b isolated 档并发执行（用 runs 的创建/结束时间证）；
#   12 journey 组装（初始修订行 + 真实修订行 + Task 行 + summary 计数）、字节稳定、
#      跨租户 404、跨副本同字节。
#
# 场景 12 不重复造失败节点：Task 行失败形状（status=failed + reason）由
# orchestration_journey_test.go 的 TestOrchestrationJourneyAssembly 覆盖，活体只验真实链路。
#
# 起法：主引擎带固定 TAKO_JWT_SECRET（跨副本对照靠它让 token 互通），例：
#   TAKO_JWT_SECRET=tako-acc82-jwt ./tmp/yktako-acc82 -engine 127.0.0.1:8945 \
#     -pg "$DSN" -redis 127.0.0.1:6390 -workspace ../tmp/workspace-acc82 \
#     -bash-foreground-wait 3s -wait-window 8s -task-interval 1s
# 然后：
#   TAKO_BASE=http://127.0.0.1:8945 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc82 sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT=<仓库>/tmp/workspace-acc82 \
#   TAKO_ENGINE_BIN=<仓库>/tmp/yktako-acc82 \
#     bash scripts/acceptance/orchestration_live.sh
# 未提供 TAKO_ENGINE_BIN 时跨副本对照显式 SKIP（不静默）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
echo "引擎 $B · 场景 8（并发档位）+ 场景 12（路径账本）"
ml_signup orchlive

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"orch-live-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID"
STAMP="$(date +%s)-$$"

# task_body $1=key $2=sleep秒 → 单件 Task 内容（后台命令→等终态→提交，时长可控）
task_body(){
  printf '调用 bash 工具，command 参数原样传：sleep %s; echo done-%s-%s（前台 3 秒会转后台返回命令号，不用等它）。然后调用 bash_output（jobId 用命令号，wait=true）等它完成，再调用 submit_task_result，summary 写 done-%s。' "$2" "$1" "$STAMP" "$1"
}
# wait_all $1=$2=$3=taskId → 逐个等 succeeded
wait_all(){
  for t in "$@"; do
    if at_wait_task "$t" "succeeded" 300; then ok "Task $t succeeded"
    else no "Task $t 300s 没到 succeeded（$AT_LAST）"; ml_summary; fi
  done
}
# run_pairs $1=orchId → "key start end" 三行（UTC ISO，end 空串=未结束）
run_pairs(){
  "$PSQL" "$DSN" -At -c "SELECT t.task_key||' '||to_char(r.created_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US')||' '||COALESCE(to_char(r.finished_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US'),'') FROM agent_tasks t JOIN runs r ON r.id=t.current_run_id WHERE t.orchestration_id='$1' ORDER BY t.task_key"
}
# no_overlap / has_overlap 读 run_pairs 输出判重叠
no_overlap(){
  printf '%s\n' "$1" | python3 -c '
import sys
from datetime import datetime
iv=[]
for ln in sys.stdin:
    p=ln.split()
    if len(p)==3 and p[2]:
        s=datetime.fromisoformat(p[1]); e=datetime.fromisoformat(p[2]); iv.append((p[0],s,e))
bad=[(a[0],b[0]) for i,a in enumerate(iv) for b in iv[i+1:] if a[1]<b[2] and b[1]<a[2]]
print("OVERLAP:"+",".join(f"{x[0]}~{x[1]}" for x in bad) if bad else "SERIAL")'
}
has_overlap(){
  printf '%s\n' "$1" | python3 -c '
import sys
from datetime import datetime
iv=[]
for ln in sys.stdin:
    p=ln.split()
    if len(p)==3 and p[2]:
        s=datetime.fromisoformat(p[1]); e=datetime.fromisoformat(p[2]); iv.append((p[0],s,e))
hit=[(a[0],b[0]) for i,a in enumerate(iv) for b in iv[i+1:] if a[1]<b[2] and b[1]<a[2]]
print("OVERLAP:"+",".join(f"{x[0]}~{x[1]}" for x in hit) if hit else "SERIAL")'
}

# ══════════════════════════════════════════════════════════════════════════
# 8a：shared 档——预算 1，三个 Task 的执行 run 两两不重叠
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 8a：shared 档串行 ═══"
# 图形：s1、s2 两路并行 → s3 收尾。恰一个最终节点（117101 规则）；预算 1 下 s1/s2 必须串行。
TASKS=$(jq -cn --arg a "$(task_body s1 8)" --arg b "$(task_body s2 8)" --arg c "$(task_body s3 2)" \
  '[{key:"s1",content:$a,agent:"researcher"},{key:"s2",content:$b,agent:"researcher"},{key:"s3",content:$c,agent:"researcher"}]')
LINKS='[{"parent":"s1","child":"s3"},{"parent":"s2","child":"s3"}]'
RESP=$(at_create_orch "acc-orc-shared-$STAMP" "$WID" "$SID" "shared 档两路并行汇入收尾" "$TASKS" "$LINKS" "shared")
OIDA=$(printf '%s' "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OIDA" ] || { echo "创建失败：$RESP" >&2; exit 2; }
eq "shared 档落库 workspaceMode" "shared" "$(ml_sql "SELECT workspace_mode FROM agent_orchestrations WHERE id='$OIDA'")"
TIDS_A=$(ml_sql "SELECT string_agg(id, chr(44) ORDER BY task_key) FROM agent_tasks WHERE orchestration_id='$OIDA'")
IFS=',' read -r TA1 TA2 TA3 <<<"$TIDS_A"
wait_all "$TA1" "$TA2" "$TA3"
PAIRS=$(run_pairs "$OIDA")
eq "shared 档三场执行 run 串行" "SERIAL" "$(no_overlap "$PAIRS")"

# ══════════════════════════════════════════════════════════════════════════
# 8b：isolated 档——预算 2，两路并行汇入 i3，i1/i2 的执行 run 时间重叠
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 8b：isolated 档并发 ═══"
TASKS=$(jq -cn --arg a "$(task_body i1 8)" --arg b "$(task_body i2 8)" --arg c "$(task_body i3 2)" \
  '[{key:"i1",content:$a,agent:"researcher"},{key:"i2",content:$b,agent:"researcher"},{key:"i3",content:$c,agent:"researcher"}]')
LINKS='[{"parent":"i1","child":"i3"},{"parent":"i2","child":"i3"}]'
RESP=$(at_create_orch "acc-orc-isolated-$STAMP" "$WID" "$SID" "isolated 档两路并行汇入收尾" "$TASKS" "$LINKS" "isolated")
OIDB=$(printf '%s' "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OIDB" ] || { echo "创建失败：$RESP" >&2; exit 2; }
eq "isolated 档落库 workspaceMode" "isolated" "$(ml_sql "SELECT workspace_mode FROM agent_orchestrations WHERE id='$OIDB'")"
TIDS_B=$(ml_sql "SELECT string_agg(id, chr(44) ORDER BY task_key) FROM agent_tasks WHERE orchestration_id='$OIDB'")
IFS=',' read -r TB1 TB2 TB3 <<<"$TIDS_B"
wait_all "$TB1" "$TB2" "$TB3"
PAIRS=$(run_pairs "$OIDB")
OV=$(has_overlap "$PAIRS")
case "$OV" in
  OVERLAP:*) ok "isolated 档出现并发执行：$OV";;
  *) no "isolated 档三场仍串行，没吃到并发预算 2";;
esac

# ══════════════════════════════════════════════════════════════════════════
# 12：journey——初始修订行 + 真实修订行 + Task 行 + summary 计数 + 字节稳定
#     + 跨租户 404 + 跨副本同字节
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 12：路径账本 ═══"
TASKS=$(jq -cn --arg a "$(task_body j1 12)" --arg b "$(task_body j2 20)" \
  '[{key:"j1",content:$a,agent:"researcher"},{key:"j2",content:$b,agent:"researcher"}]')
RESP=$(at_create_orch "acc-orc-journey-$STAMP" "$WID" "$SID" "两件串行，中途补计划" "$TASKS" '[{"parent":"j1","child":"j2"}]')
OIDJ=$(printf '%s' "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OIDJ" ] || { echo "创建失败：$RESP" >&2; exit 2; }
TIDJ1=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OIDJ' AND task_key='j1'")
TIDJ2=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OIDJ' AND task_key='j2'")
# 修订准入（117107）：最终 Task 一旦开始执行就不能改图。j1 是 model 驱动的长命令，
# 此处它还在跑、最终节点 j2 未开始，正是可修订的窗口。
eq "修订前最终 Task j2 还没开始（准入前提）" "true" \
  "$(ml_sql "SELECT (started_at IS NULL)::text FROM agent_tasks WHERE id='$TIDJ2'")"
# 修订形状：j2 -> j3 并把最终节点改指 j3（appendTask 不能是孤岛，每个活跃节点都要走得到最终节点）
RESP=$(jq -cn --arg k "acc-rev-$STAMP" --arg c "$(task_body j3 2)" \
  '{idempotencyKey:$k,baseRevision:0,reason:"补一个收尾任务",addTasks:[{key:"j3",content:$c,agent:"researcher"}],addLinks:[{parent:"j2",child:"j3"}],finalTaskKey:"j3"}' |
  curl -sS -X POST "$B/api/orchestrations/$OIDJ/revisions" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @-)
eq "修订被受理（组在 active 且最终 Task 未开始）" "0" "$(printf '%s' "$RESP" | jq -r '.code')" || { echo "$RESP" >&2; ml_summary; }
TIDJ3=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OIDJ' AND task_key='j3'")
if [ -n "$TIDJ3" ]; then
  wait_all "$TIDJ1" "$TIDJ2" "$TIDJ3"
else
  no "修订没落库 j3，跳过等待"
fi

J1=$(curl -sS "$B/api/orchestrations/$OIDJ/journey" -H "Authorization: Bearer $TOK")
J2=$(curl -sS "$B/api/orchestrations/$OIDJ/journey" -H "Authorization: Bearer $TOK")
eq "两次读取字节完全一致（只读 PG 派生，无进程内状态）" "$J1" "$J2"
eq "journey.planRevision = 1" "1" "$(printf '%s' "$J1" | jq -r '.data.planRevision')"
eq "summary.planRevisions = planRevision+1" "2" "$(printf '%s' "$J1" | jq -r '.data.summary.planRevisions')"
FIRST=$(printf '%s' "$J1" | jq -c '.data.steps[0]')
eq "首行是合成的初始修订" "plan_revision" "$(printf '%s' "$FIRST" | jq -r '.type')"
eq "初始修订 revision=0" "0" "$(printf '%s' "$FIRST" | jq -r '.revision')"
eq "初始修订 reason=initial" "initial" "$(printf '%s' "$FIRST" | jq -r '.reason')"
eq "真实修订行 revision=1、原因入库" "补一个收尾任务" \
  "$(printf '%s' "$J1" | jq -r '[.data.steps[] | select(.type=="plan_revision")][1].reason')"
ADDED=$(printf '%s' "$J1" | jq -c '[.data.steps[] | select(.type=="plan_revision")][1].added')
eq "修订行记着新增的 j3" '["j3"]' "$ADDED"
for k in j1 j2 j3; do
  ST=$(printf '%s' "$J1" | jq -r --arg k "$k" '[.data.steps[] | select(.type=="task" and .taskKey==$k)][0].status')
  eq "Task 行 $k 已收口且进时间线" "succeeded" "$ST"
done
SUM=$(printf '%s' "$J1" | jq -c '.data.summary')
eq "summary 计数自洽：succeeded=3" "3" "$(printf '%s' "$SUM" | jq -r '.succeeded')"
eq "summary 计数自洽：tasksCreated=3" "3" "$(printf '%s' "$SUM" | jq -r '.tasksCreated')"

# 跨租户不可见：第二个用户（自己的租户）读同一组 → 404 117002；先存主用户令牌，读完换回
TOK_MAIN=$TOK
ml_signup orchlive2
RESP=$(curl -sS -w '\n%{http_code}' "$B/api/orchestrations/$OIDJ/journey" -H "Authorization: Bearer $TOK")
CODE=$(printf '%s' "$RESP" | head -1 | jq -r '.code'); HTTP=$(printf '%s' "$RESP" | tail -1)
eq "跨租户读 journey 404" "404" "$HTTP"
eq "跨租户业务码 117002" "117002" "$CODE"
TOK=$TOK_MAIN

# 跨副本同字节：同 DSN 起第二副本读同一组（token 互通靠两边同 TAKO_JWT_SECRET）
BIN="${TAKO_ENGINE_BIN:-$REPO/tmp/yktako-acc82}"
if [ ! -x "$BIN" ]; then
  echo "SKIP 跨副本字节对照：未找到引擎二进制（设 TAKO_ENGINE_BIN 后重跑可补验）"
else
  P2="${TAKO_REPLICA_PORT:-8962}"
  LOG2="$(mktemp -t orchreplica)"
  ( cd "$REPO/yk-tako-eino" && exec "$BIN" -engine "127.0.0.1:$P2" -pg "$DSN" \
      -redis 127.0.0.1:6390 -workspace "$WS_ROOT" -dev \
      -task-interval 0 -job-reconcile-interval 0 ) >"$LOG2" 2>&1 &
  P2PID=$!
  trap 'kill "$P2PID" 2>/dev/null' EXIT
  for _ in $(seq 60); do
    curl -sS -m 1 -o /dev/null "http://127.0.0.1:$P2/api/auth/login" 2>/dev/null && break
    kill -0 "$P2PID" 2>/dev/null || { echo "第二副本启动即退，日志：$LOG2" >&2; tail -20 "$LOG2" >&2; ml_summary; }
    sleep 0.5
  done
  J3=$(curl -sS "http://127.0.0.1:$P2/api/orchestrations/$OIDJ/journey" -H "Authorization: Bearer $TOK")
  eq "第二副本读同一组字节一致" "$J1" "$J3"
  kill "$P2PID" 2>/dev/null; wait "$P2PID" 2>/dev/null; trap - EXIT
  rm -f "$LOG2"
fi

ml_summary
