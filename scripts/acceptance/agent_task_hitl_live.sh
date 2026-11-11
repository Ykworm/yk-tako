#!/usr/bin/env bash
# Task HITL 活体（doc/72 §7；施工包 §11.3 第 2 场）：Task 执行者能问、问题只在当前执行上可见、
# 问答落库带截止时间；覆盖「两件任务各自提问」「后到的问题不覆盖先到的」「重复回答」「他人不可见」
# 「崩溃后回答 + resume 接回同一场 run」「超时收口 + 迟到回答 115102」。
#
# 起法（**必须**用短租约 + 短输入超时，否则崩溃等待/超时收口会超出直觉时长）：
#   -lease-ttl 8s -task-input-timeout 45s，调度循环开着（默认 -task-interval 即开）。
# 崩溃段要杀/重启隔离引擎，沿用 agent_task_crash_live.sh 的契约：
#   TAKO_ENGINE_PIDS     要 kill -9 的隔离引擎 PID（**必填**）
#   TAKO_ENGINE_RESTART  重启命令（**必填**，后台起，参数与被打的实例一致）
#   TAKO_LEASE_WAIT      等租约过期的秒数，默认 12（引擎 -lease-ttl 8s）
#
#   TAKO_BASE=http://127.0.0.1:8947 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc72hitl sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT="$PWD/tmp/workspace-acc72hitl" \
#   TAKO_ENGINE_PIDS=… TAKO_ENGINE_RESTART='…' \
#     bash scripts/acceptance/agent_task_hitl_live.sh
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn
PIDS="${TAKO_ENGINE_PIDS:-}"
RESTART="${TAKO_ENGINE_RESTART:-}"
LEASE_WAIT="${TAKO_LEASE_WAIT:-12}"
for v in PIDS RESTART; do
  eval "val=\$$v"
  [ -n "$val" ] || { echo "必填参数缺了：TAKO_${v^^}（见本文件头）" >&2; exit 2; }
done

echo "引擎 $B · 打 Task 提问 → 回答 → 崩溃恢复 → 超时的链"
ml_signup taskhitl

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-hitl-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"

STAMP="$(date +%s)-$$"

# 建一件会提问的 Task（单节点组）；sets OID / TID。
mk_asking_task(){ # $1=key 后缀 $2=暗号 $3=选项 JSON 或空
  local opts=""
  if [ -n "${3:-}" ]; then
    opts="，options 给 $3"
  fi
  local prompt="先调用 ask_user，问题必须是「请回复暗号 $2」$opts。拿到回答后调用 submit_task_result，summary 写你收到的回答原文。"
  local resp
  resp=$(at_create_task "acc-hitl-$1-$STAMP" "$WID" "$SID" "$prompt" "researcher")
  OID=$(echo "$resp" | jq -r '.data.orchestrationId // ""')
  TID=$(echo "$resp" | jq -r '.data.taskId // ""')
  if [ -z "$TID" ] || [ "$TID" = "null" ]; then echo "创建失败：$resp" >&2; exit 2; fi
}

# ── 1. 第一件 Task 提问：问题行契约、回答、重复回答 ───────────────────────────────
mk_asking_task q1 "Q1-$STAMP" "[\"甲-$STAMP\",\"乙-$STAMP\"]"
echo "  q1 taskId=$TID orchId=$OID"
if at_wait_task_question "$TID" 240; then
  ok "q1 执行者问出了问题（${AT_QUESTION:0:40}…）"
else no "240s 没等到 q1 提问（task=$(at_task_status "$TID")）"; ml_summary; fi
Q1_ID="$AT_QID"
RUN1=$(ml_sql "SELECT current_run_id FROM agent_tasks WHERE id='$TID'")
Q1_BODY=$(at_task_questions "$TID" pending)
eq "pending 问题恰好 1 条" "1" "$(echo "$Q1_BODY" | jq '.data | length')"
eq "问题 kind=question" "question" "$(echo "$Q1_BODY" | jq -r '.data[0].kind')"
eq "问题 status=pending" "pending" "$(echo "$Q1_BODY" | jq -r '.data[0].status')"
eq "Task 问题带 options（args 里 2 个候选）" "2" "$(echo "$Q1_BODY" | jq -r '.data[0].args | fromjson | .options | length')"
if echo "$Q1_BODY" | jq -e '.data[0] | has("expiresAt")' >/dev/null 2>&1; then
  ok "Task 问题带 expiresAt（有限等待）"
else no "Task 问题缺 expiresAt"; fi
eq "问题挂在当前执行 run 上" "$RUN1" "$(echo "$Q1_BODY" | jq -r '.data[0].runId')"

RESP=$(at_answer "$Q1_ID" "暗号Q1-$STAMP")
eq "回答回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
eq "回答回 status=answered" "answered" "$(echo "$RESP" | jq -r .data.status)"
at_http POST "/api/questions/$Q1_ID/answer" "$(jq -cn --arg a '再答一次' '{answer:$a}')"
eq "重复回答回 409" "409" "$AT_CODE"
eq "重复回答码 115101" "115101" "$(echo "$AT_BODY" | jq -r .code)"
at_http POST "/api/questions/qs_doesnotexist/answer" '{"answer":"x"}'
eq "不存在的提问回 404" "404" "$AT_CODE"
eq "不存在的提问码 115002" "115002" "$(echo "$AT_BODY" | jq -r .code)"

if at_wait_task "$TID" "succeeded" 300; then
  ok "q1 拿到答案后跑到 succeeded"
else no "300s 没到 succeeded（最后 $AT_LAST）"; ml_summary; fi
at_task "$TID"
eq "回答后 currentRunId 没换（同一场 run 继续）" "$RUN1" "$(echo "$AT_TASK" | jq -r .currentRunId)"
eq "问题行 answered" "answered" "$(at_task_questions "$TID" answered | jq -r '.data[0].status')"
eq "答案原文落库" "暗号Q1-$STAMP" "$(at_task_questions "$TID" answered | jq -r '.data[0].answer')"
SUMMARY=$(echo "$AT_TASK" | jq -r '.result.summary // ""')
if echo "$SUMMARY" | grep -q "暗号Q1-$STAMP"; then
  ok "结果 summary 带着收到的答案（模型拿到了问答结果）"
else no "结果 summary 没带答案：$SUMMARY"; fi
if at_wait_delivered "$OID" 240; then ok "q1 组交付完成"; else no "q1 组没交付"; ml_summary; fi

# ── 2. 后到的第二件 Task 提问：互不影响 + 他人不可见 ─────────────────────────────
TID1="$TID"
mk_asking_task q2 "Q2-$STAMP" ""
OID2="$OID"; TID2="$TID"
if at_wait_task_question "$TID2" 240; then
  ok "q2 后来也问出了问题"
else no "240s 没等到 q2 提问（task=$(at_task_status "$TID2")）"; ml_summary; fi
Q2_ID="$AT_QID"
[ "$Q1_ID" != "$Q2_ID" ] && ok "两件 Task 的问题行互不相同" || no "两件 Task 拿到同一个问题 id"
eq "q1 的问题仍是 answered（没被后到的问题覆盖）" "answered" \
  "$(at_task_questions "$TID1" answered | jq -r '.data[0].status')"

SAVE_TOK="$TOK"; SAVE_TEN="$TEN"
ml_signup taskhitlother no_model
at_http GET "/api/tasks/$TID2/questions"
eq "他人读 Task 问题回 404" "404" "$AT_CODE"
eq "他人读 Task 问题码 117001" "117001" "$(echo "$AT_BODY" | jq -r .code)"
at_http POST "/api/questions/$Q2_ID/answer" "$(jq -cn --arg a '抢答' '{answer:$a}')"
[ "$AT_CODE" != "200" ] && ok "他人回答被拒（$AT_CODE）" || no "他人居然答上了"
TOK="$SAVE_TOK"; TEN="$SAVE_TEN"
eq "抢答没有改状态（仍 pending）" "pending" \
  "$(at_task_questions "$TID2" pending | jq -r '.data[0].status')"

RESP=$(at_answer "$Q2_ID" "自定义Q2-$STAMP")
eq "自由文本回答回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
if at_wait_task "$TID2" "succeeded" 300; then
  ok "q2 跑到 succeeded"
else no "300s 没到 succeeded（最后 $AT_LAST）"; ml_summary; fi
if at_wait_delivered "$OID2" 240; then ok "q2 组交付完成"; else no "q2 组没交付"; ml_summary; fi

# ── 3. 崩溃恢复：提问后 kill -9 → 重启 → 回答 → resume 接回同一场 run ─────────────
mk_asking_task q3 "Q3-$STAMP" ""
OID3="$OID"; TID3="$TID"
if at_wait_task_question "$TID3" 240; then
  ok "q3 提问已挂起（准备 kill -9）"
else no "240s 没等到 q3 提问"; ml_summary; fi
Q3_ID="$AT_QID"
RUN3=$(ml_sql "SELECT current_run_id FROM agent_tasks WHERE id='$TID3'")
kill -9 $PIDS 2>/dev/null
sleep 1
echo "  等租约过期 ${LEASE_WAIT}s…"
sleep "$LEASE_WAIT"
bash -c "$RESTART" >/dev/null 2>&1 </dev/null
for i in $(seq 1 40); do
  sleep 1
  curl -sS -m 2 -o /dev/null "$B/api/auth/login" 2>/dev/null && break
done
ST=""
for i in $(seq 1 20); do
  ST=$(at_task_status "$TID3")
  [ "$ST" = "interrupted" ] && break
  sleep 1
done
eq "重启后 q3 读侧派生 interrupted" "interrupted" "$ST"
eq "q3 的 run 行没被翻态" "running" "$(ml_sql "SELECT status FROM runs WHERE id='$RUN3'")"
RESP=$(at_answer "$Q3_ID" "崩溃后回答-$STAMP")
eq "崩溃后回答问题回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
eq "问题行 answered" "answered" "$(at_task_questions "$TID3" answered | jq -r '.data[0].status')"
RES=$(curl -sS -X POST "$B/api/tasks/$TID3/resume" -H "Authorization: Bearer $TOK")
eq "resume 回 code 0" "0" "$(echo "$RES" | jq -r .code)"
eq "resume 接的是同一场 run" "$RUN3" "$(echo "$RES" | jq -r '.data.runId // ""')"
if at_wait_task "$TID3" "succeeded" 300; then
  ok "q3 恢复后跑到 succeeded（答案由续跑的工具调用读回）"
else no "300s 没到 succeeded（最后 $AT_LAST）"; ml_summary; fi
eq "q3 的 run 代涨到 2" "2" "$(ml_sql "SELECT lease_epoch FROM runs WHERE id='$RUN3'")"
eq "q3 结果来自当前代" "2" "$(ml_sql "SELECT result_epoch FROM agent_tasks WHERE id='$TID3'")"
if at_wait_delivered "$OID3" 300; then ok "q3 组交付完成"; else no "q3 组没交付"; ml_summary; fi

# ── 4. 超时收口：不答 → Task failed(input_timeout) → 迟到回答 115102 ─────────────
mk_asking_task q4 "Q4-$STAMP" ""
OID4="$OID"; TID4="$TID"
if at_wait_task_question "$TID4" 240; then
  ok "q4 提问已挂起（本次不回答，等超时）"
else no "240s 没等到 q4 提问"; ml_summary; fi
Q4_ID="$AT_QID"
if at_wait_task "$TID4" "failed" 240; then
  ok "q4 超时后被收口为 failed"
else no "240s 没等到超时失败（最后 $AT_LAST）"; ml_summary; fi
at_task "$TID4"
eq "失败机器原因是 input_timeout" "input_timeout" "$(echo "$AT_TASK" | jq -r '.error.code // ""')"
eq "超时后问题行翻 stale" "stale" "$(at_task_questions "$TID4" stale | jq -r '.data[0].status')"
RUN4=$(ml_sql "SELECT current_run_id FROM agent_tasks WHERE id='$TID4'")
eq "q4 的 run 已被停止" "stopped" "$(ml_sql "SELECT status FROM runs WHERE id='$RUN4'")"
RESP=$(at_answer "$Q4_ID" "迟到的回答")
eq "迟到回答码 115102" "115102" "$(echo "$RESP" | jq -r .code)"
eq "task_failed 通知恰好 1 条" "1" "$(at_notif_count "$OID4" "task_failed")"

ml_summary
