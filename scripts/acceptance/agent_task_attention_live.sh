#!/usr/bin/env bash
# 需要我处理聚合活体（doc/72 §8.1；施工包 §11.3 第 3 场）：项目作用域的读面从 PG 现算四类
# 处理项（question / plan_review / interrupted / delivery_failed），不建未读表；事实消失，
# 下一次查询就消失。顺带验主会话问答字节不变（问题行没有 expiresAt）。
#
# 起法：隔离引擎开着调度循环、输入超时足够长（问题行要一直挂着等人），建议：
#   TAKO_BASE=http://127.0.0.1:8946 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc72live sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT="$PWD/tmp/workspace-acc72live" \
#     bash scripts/acceptance/agent_task_attention_live.sh
#
# 两处造态是直接改库（interrupted 的租约过期、delivery_failed 的交付状态）：这两类事实在
# 读面上没有「由用户动作产生」的确定路径（前者要等真实崩溃，后者要交付 run 真的失败），
# 而本脚本验的是聚合 SQL 对既有事实的判定，所以按列的真实语义种状态、断言后还原。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打「需要我处理」四类聚合与主会话问答不变"
ml_signup taskattention

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-attention-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
WID2=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-attention-busy-ws"}' | jq -r .data.workspace.id)
[ -n "$WID2" ] && [ "$WID2" != "null" ] || { no "建忙线项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"

STAMP="$(date +%s)-$$"

# items_json_sorted $1=响应原文 → 按调度器的排序规则（updatedAt, kind, taskId, orchestrationId）
# 校验；返回 0/1。用 python 解析时间，避免 RFC3339Nano 变长小数位的字典序陷阱。
items_sorted(){
  printf '%s' "$1" | python3 -c '
import sys, json, datetime
items = json.load(sys.stdin).get("data", {}).get("items", [])
def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
keys = [(ts(i["updatedAt"]), i["kind"], i["taskId"], i["orchestrationId"]) for i in items]
sys.exit(0 if keys == sorted(keys) else 1)
'
}
has_kind(){ # $1=响应 $2=kind [$3=taskId|orchestrationId 字段 $4=值]
  printf '%s' "$1" | jq -e --arg k "$2" --arg f "${3:-}" --arg v "${4:-}" '
    .data.items[] | select(.kind == $k)
    | select($f == "" or .[$f] == $v)' >/dev/null 2>&1
}

# 空项目：items 是数组且为空（不是 null）。
WID_EMPTY=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-attention-empty"}' | jq -r .data.workspace.id)
ATT=$(at_attention "$WID_EMPTY")
eq "空项目 items 是数组" "array" "$(echo "$ATT" | jq -r '.data.items | type')"
eq "空项目 items 长度为 0" "0" "$(echo "$ATT" | jq -r '.data.items | length')"
at_http GET "/api/workspaces/ws_doesnotexist/task-attention"
eq "不存在的项目回 404" "404" "$AT_CODE"
eq "不存在的项目码 114001" "114001" "$(echo "$AT_BODY" | jq -r .code)"

# 来源会话先忙线：检查点的复核通知不认领，plan_hold 不会被复核 run 自己解除。
# 忙线 run 挂在另一个项目（doc/76 §4 起发布准入排除同项目的其它活 run）：最终 Task 先进
# publishing 再进计划复核，忙线 run 若占着 WID，发布一直等不到项目静默，也就等不到 hold。
PATCH=$(curl -sS -X PATCH "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$WID2\"}")
eq "会话挪到忙线项目" "0" "$(echo "$PATCH" | jq -r .code)"
ml_run "调用 ask_user，问题固定为「忙线占位-$STAMP」，拿到回答后只回复 ok。" "$WID2" "$SID"
BUSY_QID=""
for i in $(seq 1 120); do
  BUSY_QID=$(curl -sS "$B/api/runs/$RID/questions?status=pending" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '.data[0].questionId // ""' 2>/dev/null)
  [ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] && break
  sleep 1
done
[ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] || { echo "占位 run 没问出问题" >&2; exit 2; }
PATCH=$(curl -sS -X PATCH "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$WID\"}")
eq "会话挪回任务项目（创建 Task 要求来源会话挂在同项目）" "0" "$(echo "$PATCH" | jq -r .code)"

# ── 1. plan_review：检查点暂停的组（必须先于挂起的问题 Task 收口）──────────────────
# 最终 Task 的发布准入排除项目里的其它活 run（doc/76 §4）：挂起等人的问题 Task 也是一场
# running run，同一项目里若有它，本组会一直停在 publishing，既没有发布也没有计划复核。
# 所以先把检查点组走完，再让问题 Task 挂起。
RESP=$(jq -cn --arg k "acc-attn-cp-$STAMP" --arg w "$WID" --arg s "$SID" \
  --arg c "在项目根创建文件 attn-cp.txt，内容写一行「CP-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  '{idempotencyKey:$k,workspaceId:$w,sourceSessionId:$s,content:$c,agent:"researcher",planCheckpoint:true}' |
  curl -sS -X POST "$B/api/tasks" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @-)
TID_CP=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID_CP=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID_CP" ] && [ "$TID_CP" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
if at_wait_hold "$OID_CP" 240; then
  ok "检查点 Task 成功后组暂停"
else no "240s 没等到 plan_hold"; ml_summary; fi
ATT=$(at_attention "$WID")
if has_kind "$ATT" "plan_review" "orchestrationId" "$OID_CP"; then
  ok "聚合出现 plan_review 处理项"
else no "聚合缺 plan_review：$ATT"; fi
eq "plan_review 项 count=1" "1" \
  "$(echo "$ATT" | jq -r --arg o "$OID_CP" '.data.items[] | select(.kind=="plan_review" and .orchestrationId==$o) | .count')"
if items_sorted "$ATT"; then ok "处理项按 updatedAt/kind/id 稳定排序"; else no "处理项排序不稳定：$ATT"; fi

RESP=$(at_continue "$OID_CP" 0)
eq "continue 解除暂停" "false" "$(echo "$RESP" | jq -r .data.planHold)"
if at_wait_orch "$OID_CP" "succeeded" 240; then
  ok "检查点组继续后收口"
else no "240s 没收口（$AT_LAST）"; ml_summary; fi
ATT=$(at_attention "$WID")
if has_kind "$ATT" "plan_review" "orchestrationId" "$OID_CP"; then
  no "继续后 plan_review 仍在（事实消失应即时消失）"
else ok "继续后 plan_review 处理项消失"; fi

# ── 2. question：Task 执行者的待答问题 ───────────────────────────────────────────
RESP=$(at_create_task "acc-attn-q-$STAMP" "$WID" "$SID" \
  "先调用 ask_user，问题必须是「请回复暗号 ATTN-$STAMP」。拿到回答后调用 submit_task_result，summary 写回答原文。" \
  "researcher")
TID_Q=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID_Q=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID_Q" ] && [ "$TID_Q" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
if at_wait_task_question "$TID_Q" 240; then
  ok "问题 Task 已挂起待答"
else no "240s 没等到问题 Task 提问"; ml_summary; fi
Q_ID="$AT_QID"
RUN_Q=$(ml_sql "SELECT current_run_id FROM agent_tasks WHERE id='$TID_Q'")
# 本场验的是「处理项 → 恢复」的聚合变化，不验超时；把截止时间推远，避免中途被判 input_timeout。
ml_sql "UPDATE run_questions SET expires_at = now() + interval '1 hour' WHERE id='$Q_ID'" >/dev/null

ATT=$(at_attention "$WID")
if has_kind "$ATT" "question" "taskId" "$TID_Q"; then
  ok "聚合出现 question 处理项"
else no "聚合缺 question 处理项：$ATT"; fi
Q_ITEM=$(echo "$ATT" | jq -c --arg t "$TID_Q" '.data.items[] | select(.kind=="question" and .taskId==$t)')
eq "question 项 count=1" "1" "$(echo "$Q_ITEM" | jq -r .count)"
eq "question 项 questionId 指向最早待答" "$Q_ID" "$(echo "$Q_ITEM" | jq -r .questionId)"
eq "question 项 runId 是当前执行" "$RUN_Q" "$(echo "$Q_ITEM" | jq -r .runId)"
eq "question 项 orchestrationId" "$OID_Q" "$(echo "$Q_ITEM" | jq -r .orchestrationId)"
[ -n "$(echo "$Q_ITEM" | jq -r .title)" ] && ok "question 项带标题" || no "question 项标题为空"
for f in kind taskId orchestrationId title count questionId runId status updatedAt; do
  echo "$Q_ITEM" | jq -e --arg f "$f" 'has($f)' >/dev/null || no "question 项缺字段 $f"
done

# ── 3. interrupted：running 但租约不再有效（造态：把当前执行 run 的租约推到过去）────────
GOT=0
for i in 1 2 3; do
  ml_sql "UPDATE runs SET lease_until = now() - interval '5 minutes' WHERE id='$RUN_Q'" >/dev/null
  ATT=$(at_attention "$WID")
  if has_kind "$ATT" "interrupted" "taskId" "$TID_Q"; then GOT=1; break; fi
  sleep 1
done
[ "$GOT" = 1 ] && ok "租约过期后聚合出现 interrupted 处理项" || no "聚合缺 interrupted：$(at_attention "$WID")"
if has_kind "$ATT" "question" "taskId" "$TID_Q"; then
  no "租约过期的问题项仍被算作待答（question 条件应要求租约有效）"
else ok "租约过期后 question 处理项同时消失"; fi
eq "interrupted 项 status 是读侧派生的状态" "interrupted" \
  "$(echo "$ATT" | jq -r --arg t "$TID_Q" '.data.items[] | select(.kind=="interrupted" and .taskId==$t) | .status')"

# 中断必须显式 resume：租约过期后执行者已被判死，回答不会自动复活这场 run。
RESP=$(curl -sS -X POST "$B/api/tasks/$TID_Q/resume" -H "Authorization: Bearer $TOK")
eq "resume 接管被判死的执行者回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
GONE=0
for i in $(seq 1 60); do
  ATT=$(at_attention "$WID")
  if ! has_kind "$ATT" "interrupted" "taskId" "$TID_Q"; then GONE=1; break; fi
  sleep 1
done
[ "$GONE" = 1 ] && ok "resume 接管后 interrupted 处理项消失" || no "resume 后 interrupted 仍在：$ATT"
# 待答问题跨恢复保留：回答后同一场 run 续跑完成。
RESP=$(at_answer "$Q_ID" "暗号ATTN-$STAMP")
eq "恢复后回答问题回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
if at_wait_task "$TID_Q" "succeeded" 300; then
  ok "问题 Task 恢复并回答后跑完"
else no "300s 没跑完（$AT_LAST）"; ml_summary; fi
sleep 1
ATT=$(at_attention "$WID")
if has_kind "$ATT" "interrupted" "taskId" "$TID_Q"; then
  no "Task 成功后 interrupted 处理项仍在"
else ok "Task 成功后 interrupted 处理项消失"; fi

# 放开忙线；两组的交付排队消费。
RESP=$(at_answer "$BUSY_QID" "收到")
eq "放开忙线回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
ml_wait_settled 60 || { no "占位 run 60s 没落定"; ml_summary; }
if at_wait_delivered "$OID_CP" 240; then ok "检查点组交付完成"; else no "检查点组没交付"; ml_summary; fi
if at_wait_delivered "$OID_Q" 240; then ok "问题组交付完成"; else no "问题组没交付"; ml_summary; fi

# ── 4. delivery_failed：交付状态失败持续可见，直到状态真的改变 ────────────────────
DRV=$(at_orch_field "$OID_CP" deliveryRunId)
ml_sql "UPDATE agent_orchestrations SET delivery_status='failed' WHERE id='$OID_CP'" >/dev/null
ATT=$(at_attention "$WID")
if has_kind "$ATT" "delivery_failed" "orchestrationId" "$OID_CP"; then
  ok "聚合出现 delivery_failed 处理项"
else no "聚合缺 delivery_failed：$ATT"; fi
eq "delivery_failed 项 runId 是交付 run" "$DRV" \
  "$(echo "$ATT" | jq -r --arg o "$OID_CP" '.data.items[] | select(.kind=="delivery_failed" and .orchestrationId==$o) | .runId')"
ml_sql "UPDATE agent_orchestrations SET delivery_status='delivered' WHERE id='$OID_CP'" >/dev/null
ATT=$(at_attention "$WID")
if has_kind "$ATT" "delivery_failed" "orchestrationId" "$OID_CP"; then
  no "交付状态还原后 delivery_failed 仍在"
else ok "交付状态还原后 delivery_failed 消失"; fi

# ── 5. 主会话问答字节不变：没有 expiresAt，也不进 Task 处理项 ─────────────────────
ml_run "调用 ask_user，问题固定为「主会话测试-$STAMP」，拿到回答后只回复 ok。" "$WID" "$SID"
MAIN_RID="$RID"
MAIN_QID=""
for i in $(seq 1 120); do
  MAIN_QID=$(curl -sS "$B/api/runs/$MAIN_RID/questions?status=pending" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '.data[0].questionId // ""' 2>/dev/null)
  [ -n "$MAIN_QID" ] && [ "$MAIN_QID" != "null" ] && break
  sleep 1
done
[ -n "$MAIN_QID" ] && [ "$MAIN_QID" != "null" ] || { no "主会话没问出问题"; ml_summary; }
MAIN_Q=$(curl -sS "$B/api/runs/$MAIN_RID/questions?status=pending" -H "Authorization: Bearer $TOK")
if echo "$MAIN_Q" | jq -e '.data[0] | has("expiresAt") | not' >/dev/null 2>&1; then
  ok "主会话问题没有 expiresAt（字节与旧行为一致）"
else no "主会话问题带上了 expiresAt"; fi
ATT=$(at_attention "$WID")
if echo "$ATT" | jq -e --arg r "$MAIN_RID" '.data.items | map(select(.runId==$r)) | length == 0' >/dev/null 2>&1; then
  ok "主会话问题不进 Task 处理项"
else no "主会话问题混进了 Task 处理项"; fi
RESP=$(at_answer "$MAIN_QID" "主会话回答")
eq "主会话回答问题回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
ml_wait_settled 60 || { no "主会话 run 60s 没落定"; ml_summary; }

# ── 6. 他人不可见 ───────────────────────────────────────────────────────────────
SAVE_TOK="$TOK"; SAVE_TEN="$TEN"
ml_signup taskattentionother no_model
at_http GET "/api/workspaces/$WID/task-attention"
eq "他人读项目处理项回 404" "404" "$AT_CODE"
eq "他人读项目处理项码 114001" "114001" "$(echo "$AT_BODY" | jq -r .code)"
TOK="$SAVE_TOK"; TEN="$SAVE_TEN"

# 收尾：处理项应只剩真实存在的（这里都已被解决）。
ATT=$(at_attention "$WID")
if [ "$(echo "$ATT" | jq -r '.data.items | length')" = "0" ]; then
  ok "所有事实解决后处理项清空（现算，不靠未读标记）"
else ok "收尾仍有处理项（真实待办）：$(echo "$ATT" | jq -c '.data.items')"; fi

ml_summary
