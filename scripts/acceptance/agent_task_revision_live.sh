#!/usr/bin/env bash
# 计划修订活体（doc/72 §5/§6；施工包 §11.3 第 1 场）：真模型把 Task 跑到检查点 → 组暂停
# （plan_hold）→ 两条恢复路径都验：
#   A. continue：版本过期的继续回 117106，正确的继续解除暂停并让后续节点跑完；
#   B. revision：加节点 D（a→D→b）→ 过期冲突 117106 → 幂等重打 → 同键异命令 117108 →
#      预算超限与非法最终节点各回 117101 → 组收口、交付恰好一次、修订历史逐字段核对。
#
# 起法：按 scripts/acceptance/README.md 起一个**开着调度循环**的隔离引擎
# （默认 -task-interval 即开；建议 TAKO_MEMORY_MODE=markdown 免 broker），然后：
#   TAKO_BASE=http://127.0.0.1:8946 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc72live sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT="$PWD/tmp/workspace-acc72live" \
#     bash scripts/acceptance/agent_task_revision_live.sh
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打检查点暂停 → continue / revision 两条恢复路径"
ml_signup taskrevision

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-revision-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"

STAMP="$(date +%s)-$$"

# 来源会话先挂一个等人 run：检查点的 plan_review_required 通知只在「来源会话空闲」时才被认领，
# 会话忙 → 通知留在 pending、plan_hold 不会被计划复核 run 自己解除。这样本脚本（人）先做
# continue／revision 决策的路径才是确定性的；不忙时复核 run 会自行调用决策工具（设计如此）。
ml_run "调用 ask_user，问题固定为「忙线占位-$STAMP」，拿到回答后只回复 ok。" "$WID" "$SID"
BUSY_RID="$RID"
BUSY_QID=""
for i in $(seq 1 120); do
  BUSY_QID=$(curl -sS "$B/api/runs/$BUSY_RID/questions?status=pending" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '.data[0].questionId // ""' 2>/dev/null)
  [ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] && break
  sleep 1
done
[ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] || { echo "占位 run 没问出问题，无法制造忙线" >&2; exit 2; }
ok "来源会话已忙线（占位问题 $BUSY_QID），检查点通知会留在 pending"

# ── A. continue：检查点暂停 → 过期版本拒绝 → 解除暂停 ────────────────────────────
TASKS_A=$(jq -cn \
  --arg a "在项目根创建文件 rev-a.txt，内容写一行「A-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  --arg b "读项目根下的 rev-a.txt，把原文写进 rev-b.txt，然后调用 submit_task_result，summary 写 rev-b.txt 的路径。" \
  '[{key:"a",content:$a,agent:"researcher",planCheckpoint:true},{key:"b",content:$b,agent:"researcher"}]')
RESP=$(at_create_orch "acc-rev-continue-$STAMP" "$WID" "$SID" "检查点后继续" "$TASKS_A" '[{"parent":"a","child":"b"}]')
OID_A=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_A" ] && [ "$OID_A" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID_A"
TID_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TID_B=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
echo "  orchId=$OID_A a=$TID_A b=$TID_B"

if at_wait_hold "$OID_A" 240; then
  ok "a 成功后组进入 plan_hold（检查点生效）"
else no "240s 没等到 plan_hold（a=$(at_task_status "$TID_A")，b=$(at_task_status "$TID_B")）"; ml_summary; fi
eq "初始 planRevision=0" "0" "$(at_orch_field "$OID_A" planRevision)"
eq "a 的 planCheckpoint=true" "true" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"a\") | .planCheckpoint")"
eq "a 的 createdRevision=0" "0" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"a\") | .createdRevision")"
eq "plan_review_required 通知恰好 1 条" "1" "$(at_notif_count "$OID_A" "plan_review_required")"
if at_attention "$WID" | jq -e --arg o "$OID_A" '.data.items[] | select(.kind=="plan_review" and .orchestrationId==$o)' >/dev/null 2>&1; then
  ok "聚合读面出现 plan_review 处理项"
else no "聚合读面没有 plan_review 处理项"; fi

# 暂停期间 b 不能开跑（hold 只挡领取，不挡 pending→ready 的晋升）；先确认，再继续。
eq "暂停期间 b 没有执行会话（hold 挡住领取）" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$TID_B' AND execution_session_id IS NOT NULL")"
at_http POST "/api/orchestrations/$OID_A/continue" '{"baseRevision":3}'
eq "过期版本的 continue 回 409" "409" "$AT_CODE"
eq "过期版本的 continue 码 117106" "117106" "$(echo "$AT_BODY" | jq -r .code)"
RESP=$(at_continue "$OID_A" 0)
eq "正确版本的 continue 回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
eq "continue 回 planHold=false" "false" "$(echo "$RESP" | jq -r .data.planHold)"
eq "continue 回 revision=0" "0" "$(echo "$RESP" | jq -r .data.revision)"
RESP=$(at_continue "$OID_A" 0)
eq "重复 continue 自然幂等（仍 200/code 0）" "0" "$(echo "$RESP" | jq -r .code)"
eq "没有修订行（continue 不进修订历史）" "0" "$(at_revisions "$OID_A" | jq '.data.revisions | length')"

if at_wait_orch "$OID_A" "succeeded" 300; then
  ok "继续后组收口 succeeded（b 在解除暂停后被领取）"
else no "300s 没收口（组 $AT_LAST）"; ml_summary; fi
eq "组 A orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID_A" "orchestration_finished")"
eq "忙线期间交付通知留在 pending（不打断用户）" "1" \
  "$(at_notif_count "$OID_A" "orchestration_finished" "pending")"
eq "组 A plan_review_required 通知恰好 1 条" "1" "$(at_notif_count "$OID_A" "plan_review_required")"
eq "组 A 复核通知在暂停解除后被清账（consumed）" "1" \
  "$(at_notif_count "$OID_A" "plan_review_required" "consumed")"

# ── B. revision：加 D → 过期/幂等/预算/最终节点逐条验 ────────────────────────────
TASKS_B=$(jq -cn \
  --arg a "在项目根创建文件 rev-a.txt，内容写一行「A-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  --arg b "读项目根下的 rev-a.txt 与 rev-d.txt（两份都应存在），把两份原文都写进 rev-b.txt，然后调用 submit_task_result，summary 写 rev-b.txt 的路径。" \
  '[{key:"a",content:$a,agent:"researcher",planCheckpoint:true},{key:"b",content:$b,agent:"researcher"}]')
RESP=$(at_create_orch "acc-rev-revise-$STAMP" "$WID" "$SID" "检查点后加节点" "$TASKS_B" '[{"parent":"a","child":"b"}]')
OID_B=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_B" ] && [ "$OID_B" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID_B"
TB_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TB_B=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
echo "  orchId=$OID_B a=$TB_A b=$TB_B"
if at_wait_hold "$OID_B" 240; then
  ok "组 B 进入 plan_hold"
else no "组 B 240s 没等到 plan_hold（a=$(at_task_status "$TB_A")）"; ml_summary; fi

KEY="acc-rev-$STAMP"
ADD_D=$(jq -cn --arg c "上游任务 a 留下 rev-a.txt（路径在它结果的 summary 里）。读它，把原文摘抄进 rev-d.txt，再另起一行写「D-$STAMP」，然后调用 submit_task_result，summary 写 rev-d.txt 的路径。" \
  '[{key:"d",content:$c,agent:"researcher"}]')
LINKS='[{"parent":"a","child":"d"},{"parent":"d","child":"b"}]'

# 空命令：纯校验，避免「什么都没改也算一次修订」。
at_http POST "/api/orchestrations/$OID_B/revisions" \
  "$(jq -cn --arg k "$KEY-empty" '{idempotencyKey:$k,baseRevision:0,reason:"空命令"}')"
eq "空修订命令回 400" "400" "$AT_CODE"
eq "空修订命令码 117101" "117101" "$(echo "$AT_BODY" | jq -r .code)"
eq "原因说明是「修订命令为空」" "修订命令为空" "$(echo "$AT_BODY" | jq -r .data.reason)"

# 过期版本：新键 + 旧版本号。
at_http POST "/api/orchestrations/$OID_B/revisions" \
  "$(jq -cn --arg k "$KEY-stale" --argjson t "$ADD_D" --argjson l "$LINKS" \
    '{idempotencyKey:$k,baseRevision:9,reason:"过期",addTasks:$t,addLinks:$l}')"
eq "过期版本的修订回 409" "409" "$AT_CODE"
eq "过期版本的修订码 117106" "117106" "$(echo "$AT_BODY" | jq -r .code)"

# 预算：现有 2 件 + 31 件 = 33 > 32（历史节点计入，上限不因为还没建行而放宽）。
BUDGET=$(jq -cn '[range(1;32) | {key:("x"+(tostring)),content:"占位",agent:"researcher"}]')
at_http POST "/api/orchestrations/$OID_B/revisions" \
  "$(jq -cn --arg k "$KEY-budget" --argjson t "$BUDGET" \
    '{idempotencyKey:$k,baseRevision:0,reason:"预算",addTasks:$t}')"
eq "超预算回 400" "400" "$AT_CODE"
eq "超预算码 117101" "117101" "$(echo "$AT_BODY" | jq -r .code)"
if echo "$AT_BODY" | jq -r .data.reason | grep -q '最多 32 件'; then
  ok "原因写明节点预算（含历史）"
else no "预算原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"; fi

# 非法最终节点：不在本组。
at_http POST "/api/orchestrations/$OID_B/revisions" \
  "$(jq -cn --arg k "$KEY-final" '{idempotencyKey:$k,baseRevision:0,reason:"换最终",finalTaskKey:"nope"}')"
eq "非法最终节点回 400" "400" "$AT_CODE"
eq "非法最终节点码 117101" "117101" "$(echo "$AT_BODY" | jq -r .code)"

# 合法修订：加 D，重连 a→D→b，最终节点仍是 b。
RESP=$(at_revision "$OID_B" "$(jq -cn --arg k "$KEY" --argjson t "$ADD_D" --argjson l "$LINKS" \
  '{idempotencyKey:$k,baseRevision:0,reason:"在 a 与 b 之间补一件 D",addTasks:$t,addLinks:$l}')")
eq "合法修订回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
eq "修订版本是 1" "1" "$(echo "$RESP" | jq -r .data.revision)"
eq "修订解除 planHold" "false" "$(echo "$RESP" | jq -r .data.planHold)"
eq "修订后最终节点仍是 b" "$TB_B" "$(echo "$RESP" | jq -r .data.finalTaskId)"
eq "新增 1 件 Task" "1" "$(echo "$RESP" | jq -r '.data.addedTaskIds | length')"
TID_D=$(echo "$RESP" | jq -r '.data.addedTaskIds[0]')
REV_ID=$(echo "$RESP" | jq -r .data.revisionId)
at_orch "$OID_B"
eq "组 planRevision 推进到 1" "1" "$(echo "$AT_ORCH" | jq -r .orchestration.planRevision)"
eq "D 的 createdRevision=1" "1" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"d\") | .createdRevision")"
eq "D 的 planCheckpoint=false" "false" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"d\") | .planCheckpoint")"
eq "a 的 createdRevision 不变（0）" "0" "$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"a\") | .createdRevision")"

# 幂等重打：同键同命令回原修订；同键异命令回 117108。
RESP2=$(at_revision "$OID_B" "$(jq -cn --arg k "$KEY" --argjson t "$ADD_D" --argjson l "$LINKS" \
  '{idempotencyKey:$k,baseRevision:0,reason:"在 a 与 b 之间补一件 D",addTasks:$t,addLinks:$l}')")
eq "重打回同一个 revisionId" "$REV_ID" "$(echo "$RESP2" | jq -r .data.revisionId)"
eq "重打回同一版（revision=1）" "1" "$(echo "$RESP2" | jq -r .data.revision)"
eq "重打回同一批 addedTaskIds" "$TID_D" "$(echo "$RESP2" | jq -r '.data.addedTaskIds[0]')"
at_http POST "/api/orchestrations/$OID_B/revisions" \
  "$(jq -cn --arg k "$KEY" --argjson t "$ADD_D" '{idempotencyKey:$k,baseRevision:0,reason:"换了原因",addTasks:$t}')"
eq "同键异命令回 409" "409" "$AT_CODE"
eq "同键异命令码 117108" "117108" "$(echo "$AT_BODY" | jq -r .code)"
eq "agent_plan_revisions 仍只有 1 行" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_plan_revisions WHERE orchestration_id='$OID_B'")"

if at_wait_orch "$OID_B" "succeeded" 360; then
  ok "修订后的图跑完、组收口 succeeded"
else no "360s 没收口（组 $AT_LAST）"; at_orch "$OID_B"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
eq "D 的 Task 恰好一场执行 run" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID_D')")"
FINAL_B="$WS_ROOT/tenants/$TEN/workspaces/$WID/rev-b.txt"
if [ -f "$FINAL_B" ] && grep -q "A-$STAMP" "$FINAL_B" && grep -q "D-$STAMP" "$FINAL_B"; then
  ok "rev-b.txt 由 a 与 D 的证据合成（D 真的跑过）"
else no "rev-b.txt 缺证据字节（$FINAL_B）"; fi

# 两组的任务都已跑完、暂停都已由本脚本解除；此时放开忙线，让排队的交付开始消费。
RESP=$(at_answer "$BUSY_QID" "收到")
eq "放开忙线问题回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
ml_wait_settled 60 || { no "占位 run 60s 没落定"; ml_summary; }

if at_wait_delivered "$OID_A" 240; then
  ok "组 A 交付完成"
else no "组 A 240s 没等到交付（$(at_orch_field "$OID_A" deliveryStatus)）"; ml_summary; fi
if at_wait_delivered "$OID_B" 240; then
  ok "组 B 交付完成"
else no "组 B 240s 没等到交付（$(at_orch_field "$OID_B" deliveryStatus)）"; ml_summary; fi
eq "组 B orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID_B" "orchestration_finished")"
eq "组 B plan_review_required 通知恰好 1 条" "1" "$(at_notif_count "$OID_B" "plan_review_required")"

# 修订历史逐字段：版本、基版、原因、新增节点、边、来源 run（HTTP 手工修订为空）、时间。
HIST=$(at_revisions "$OID_B")
eq "修订历史 1 行" "1" "$(echo "$HIST" | jq '.data.revisions | length')"
R0=$(echo "$HIST" | jq -c '.data.revisions[0]')
eq "历史 revision=1" "1" "$(echo "$R0" | jq -r .revision)"
eq "历史 baseRevision=0" "0" "$(echo "$R0" | jq -r .baseRevision)"
eq "历史 reason 原样" "在 a 与 b 之间补一件 D" "$(echo "$R0" | jq -r .reason)"
eq "历史新增节点 key=d" "d" "$(echo "$R0" | jq -r '.addTasks[0].key')"
eq "历史新增边两条" "2" "$(echo "$R0" | jq -r '.addLinks | length')"
eq "历史无取消节点" "0" "$(echo "$R0" | jq -r '.cancelTasks | length')"
eq "HTTP 修订的 sourceRunId 为空" "" "$(echo "$R0" | jq -r .sourceRunId)"
[ "$(echo "$R0" | jq -r .createdAt)" != "null" ] && ok "历史带 createdAt" || no "历史缺 createdAt"

# 收口后再打一次同键：仍回原修订（幂等先于状态检查）。
RESP3=$(at_revision "$OID_B" "$(jq -cn --arg k "$KEY" --argjson t "$ADD_D" --argjson l "$LINKS" \
  '{idempotencyKey:$k,baseRevision:0,reason:"在 a 与 b 之间补一件 D",addTasks:$t,addLinks:$l}')")
eq "组终态后同键重打仍回原修订" "$REV_ID" "$(echo "$RESP3" | jq -r .data.revisionId)"
eq "组终态后 agent_plan_revisions 仍 1 行" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_plan_revisions WHERE orchestration_id='$OID_B'")"

eq "组 B 复核通知在暂停解除后被清账（consumed）" "1" \
  "$(at_notif_count "$OID_B" "plan_review_required" "consumed")"
eq "来源会话恰好四场 run（探针 + 忙线占位 + 两次交付）" "4" "$(at_session_runs "$SID")"
ml_summary
