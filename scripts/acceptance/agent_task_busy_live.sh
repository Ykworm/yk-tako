#!/usr/bin/env bash
# 忙会话交付活体（doc/70 §9.2；施工包 §12.3 第 4 场）：来源会话正忙（一场 ask_user 挂起中、
# watchdog 照续租约的 run）时整组收口 → orchestration_finished 通知保持 pending、不铸交付 run →
# 会话空闲（答题收尾）后消费，来源会话**恰好多出一场交付 run**。
#
# 忙源是确定性的：ask_user 挂起的 run 保持 running + 活租约（waittools 的约定），
# 挂多久由脚本控制——不赌模型输出时长。
# 忙源不挂项目（doc/76 §4）：组结果现在要发布进 content/，发布准入排除同项目其它活 run；
# 若忙源挂在该项目上，组要等它收尾才可能发布，就测不到「组已收口而会话仍忙」的门。
# 起法同 agent_task_single_live.sh 头注（开着调度循环的隔离引擎 + TAKO_PG_DSN）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打「来源会话正忙 → 通知 pending → 空闲后恰好一场交付」的链"
ml_signup taskbusy

# ── 1. 建项目，并让来源会话挂上它（创建 Task 要求两者一致）─────────────────────
WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-busy-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"

# ── 2. 建单件 Task（来源会话此刻挂着项目，创建校验通过）────────────────────────
STAMP="$(date +%s)-$$"
RESP=$(at_create_task "acc-busy-$STAMP" "$WID" "$SID" \
  "在项目根创建文件 busy.txt，内容写一行「busy-probe-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  "researcher")
TID=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID" ] && [ "$TID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  taskId=$TID orchId=$OID"

# ── 3. 立刻在来源会话起 ask_user 挂起 run——它就是「会话正忙」本身 ────────────────
# 必须长在来源会话里（不另起会话，否则交付判据看不到它），但不能占着任务所在的项目：
# doc/76 §4 起 shared 组的结果也要发布进 content/，发布准入会排除同项目的其它活 run
# （发布窗口内不允许第二个写者）——忙源留在原项目会让组一直等它，根本闭不上。
# 已有会话的 run 沿用会话列上的项目（请求里的 workspaceId 不改列），所以先 PATCH 把会话
# 挪到另一个项目，再起挂起 run；任务已经建好，不再依赖会话挂哪个项目。
WID2=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-busy-other-ws"}' | jq -r .data.workspace.id)
[ -n "$WID2" ] && [ "$WID2" != "null" ] || { no "建第二个项目失败"; ml_summary; }
PATCH=$(curl -sS -X PATCH "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$WID2\"}")
eq "会话挪到第二个项目" "0" "$(echo "$PATCH" | jq -r .code)"
ml_run '请调用 ask_user 工具问我「选 A 还是 B？」，问完就停在那里等我的答案，不要自己替我选。' "$WID2" "$SID"
BUSY_RUN="$RID"
# 等挂起真的发生：run_questions 出 pending 行（挂起中 watchdog 照续租约）。
QID=""
for i in $(seq 1 60); do
  QID=$(ml_sql "SELECT id FROM run_questions WHERE session_id='$SID' AND status='pending' ORDER BY created_at DESC LIMIT 1")
  [ -n "$QID" ] && break
  sleep 1
done
[ -n "$QID" ] || { no "60s 没等到 ask_user 挂起（run_questions 无 pending 行）"; ml_summary; }
ok "来源会话已挂起在 ask_user（question $QID，run $BUSY_RUN）"
eq "挂起中 run 仍是 running + 活租约" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE id='$BUSY_RUN' AND status='running' AND lease_until > now()")"

if at_wait_orch "$OID" "succeeded" 300; then
  ok "Task 跑完、整组收口 succeeded（来源会话仍挂着）"
else no "组没收口（最后状态 $AT_LAST）"; ml_summary; fi

# ── 3. 收口时刻会话正忙：通知必须保持 pending，交付 run 一个都不许铸 ──────────────
NOTIF=$(ml_sql "SELECT id FROM agent_notifications WHERE orchestration_id='$OID' AND kind='orchestration_finished' LIMIT 1")
[ -n "$NOTIF" ] || { no "组收口了却没有 orchestration_finished 通知"; ml_summary; }
# 再给消费循环几轮时间：它每轮都看一眼这条通知，正忙就必须按住不动。
sleep 6
eq "通知保持 pending（消费循环看过多轮仍不认领）" "pending" \
  "$(ml_sql "SELECT status FROM agent_notifications WHERE id='$NOTIF'")"
eq "组交付状态还是 pending" "pending" "$(at_orch_field "$OID" "deliveryStatus")"
eq "deliveryRunId 还是空" "" "$(at_orch_field "$OID" "deliveryRunId")"
eq "来源会话仍只有探针 + 挂起两场 run" "2" "$(at_session_runs "$SID")"

# ── 4. 答题收尾 → 会话空闲 → 消费 → 恰好一场交付 run ─────────────────────────────
ANS=$(curl -sS -X POST "$B/api/questions/$QID/answer" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"answer":"B"}')
eq "answer 成功，挂起 run 收尾" "0" "$(echo "$ANS" | jq -r .code)"
ml_wait_settled 90 || no "答题后 90s 挂起 run 没落定"
if at_wait_delivered "$OID" 180; then
  ok "会话空闲后交付完成"
else no "180s 没等到交付（deliveryStatus=$(at_orch_field "$OID" deliveryStatus)）"; ml_summary; fi
DRV=$(at_orch_field "$OID" "deliveryRunId")
[ -n "$DRV" ] && ok "交付 run 铸了（$DRV）" || no "deliveryRunId 是空"
eq "来源会话恰好三场 run（探针 + 挂起 + 交付）" "3" "$(at_session_runs "$SID")"
eq "交付 run 终态 complete" "complete" "$(ml_sql "SELECT status FROM runs WHERE id='$DRV'")"
eq "orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID" "orchestration_finished")"
eq "且已消费" "1" "$(at_notif_count "$OID" "orchestration_finished" "consumed")"

ml_summary
