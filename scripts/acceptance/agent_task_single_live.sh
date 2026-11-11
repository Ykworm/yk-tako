#!/usr/bin/env bash
# 单件 Task 活体（doc/70；施工包 §12.3 第 1 场）：真模型领一件持久任务 → 真实 run →
# submit_task_result 显式交结果 → 组收口 → 来源会话交付**恰好一次**；同一创建请求重打不加行。
#
# 起法：按 scripts/acceptance/README.md 起一个**开着调度循环**的隔离引擎
# （默认 -task-interval 即开；建议配 TAKO_MEMORY_MODE=markdown 免 broker），然后：
#   TAKO_BASE=http://127.0.0.1:8943 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" \
#     bash scripts/acceptance/agent_task_single_live.sh
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打单件 Task 的创建→执行→交结果→交付链"
ml_signup tasksingle

# 来源会话必须挂着项目（创建 Task 要求两者一致），所以先建项目、探针 run 带上它。
WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-single-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"

STAMP="$(date +%s)-$$"
KEY="acc-single-$STAMP"
RESP=$(at_create_task "$KEY" "$WID" "$SID" \
  "在项目根创建文件 single.txt，内容写一行「tako 单件任务活体 $STAMP」。然后调用 submit_task_result，summary 里写明你创建的文件路径。" \
  "researcher")
TID=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
CST=$(echo "$RESP" | jq -r '.data.status // ""')
if [ -z "$TID" ] || [ "$TID" = "null" ]; then echo "创建失败：$RESP" >&2; exit 2; fi
echo "  taskId=$TID orchId=$OID"
eq "创建回 202 且状态 ready" "ready" "$CST"
eq "未领取时 currentRunId 是空串" "" "$(echo "$RESP" | jq -r '.data.currentRunId')"

# ── 1. 同一请求重打：幂等回原 Task，不加行 ────────────────────────────────────────
RESP2=$(at_create_task "$KEY" "$WID" "$SID" \
  "在项目根创建文件 single.txt，内容写一行「tako 单件任务活体 $STAMP」。然后调用 submit_task_result，summary 里写明你创建的文件路径。" \
  "researcher")
eq "重打回同一个 taskId" "$TID" "$(echo "$RESP2" | jq -r '.data.taskId')"
eq "重打回同一个 orchestrationId" "$OID" "$(echo "$RESP2" | jq -r '.data.orchestrationId')"
eq "重打后 agent_tasks 还是 1 行" "1" "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE orchestration_id='$OID'")"

# ── 2. 调度领取 → 真模型执行 → submit_task_result → 对账 succeeded ───────────────
# 不等中间态 running（小任务几秒就跑完，中间态可能错过）：直接等终态，领取由 currentRunId 证明。
if at_wait_task "$TID" "succeeded" 240; then
  ok "Task 跑到 succeeded"
else no "240s 没跑到 succeeded（最后状态 $AT_LAST）"; ml_summary; fi
at_task "$TID"
SUMMARY=$(echo "$AT_TASK" | jq -r '.result.summary // ""')
REV=$(echo "$AT_TASK" | jq -r '.resultRevision')
RUN1=$(echo "$AT_TASK" | jq -r '.currentRunId')
[ -n "$RUN1" ] && [ "$RUN1" != "null" ] && ok "领取铸了执行 run（$RUN1）" || no "currentRunId 没写上"
[ -n "$SUMMARY" ] && ok "结果 summary 非空（${SUMMARY:0:60}…）" || no "submit_task_result 的 summary 是空"
eq "resultRevision=1" "1" "$REV"

# 执行 run 的账：恰好一场、终态 complete、结果指回这场 run
EXEC_SESS=$(ml_sql "SELECT execution_session_id FROM agent_tasks WHERE id='$TID'")
eq "执行 run 恰好一场" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE id='$RUN1' AND session_id='$EXEC_SESS'")"
eq "执行 run 终态 complete" "complete" "$(ml_sql "SELECT status FROM runs WHERE id='$RUN1'")"
eq "result_run_id 指回这场 run" "$RUN1" "$(ml_sql "SELECT result_run_id FROM agent_tasks WHERE id='$TID'")"

# ── 3. 组收口 → 来源会话交付恰好一次 ─────────────────────────────────────────────
if at_wait_orch "$OID" "succeeded" 120; then
  ok "编排组收口 succeeded"
else no "组没收口（最后状态 $AT_LAST）"; ml_summary; fi
if at_wait_delivered "$OID" 180; then
  ok "来源会话交付完成（deliveryStatus=delivered）"
else no "180s 没等到交付（deliveryStatus=$(at_orch_field "$OID" deliveryStatus)）"; ml_summary; fi
DRV=$(at_orch_field "$OID" "deliveryRunId")
eq "finalTaskId 指向这件 Task" "$TID" "$(at_orch_field "$OID" "finalTaskId")"
[ -n "$DRV" ] && ok "交付 run 铸了（$DRV）" || no "deliveryRunId 是空"
eq "交付 run 终态 complete" "complete" "$(ml_sql "SELECT status FROM runs WHERE id='$DRV'")"
eq "orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID" "orchestration_finished")"
eq "且已消费（consumed）" "1" "$(at_notif_count "$OID" "orchestration_finished" "consumed")"
eq "来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"

# ── 4. 收尾再重打一次创建：终态组幂等原样回，仍不加行 ─────────────────────────────
RESP3=$(at_create_task "$KEY" "$WID" "$SID" \
  "在项目根创建文件 single.txt，内容写一行「tako 单件任务活体 $STAMP」。然后调用 submit_task_result，summary 里写明你创建的文件路径。" \
  "researcher")
eq "终态后重打仍回同一个 taskId" "$TID" "$(echo "$RESP3" | jq -r '.data.taskId')"
eq "agent_tasks 仍是 1 行" "1" "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE orchestration_id='$OID'")"
eq "agent_notifications 仍是 1 条" "1" "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE orchestration_id='$OID'")"

ml_summary
