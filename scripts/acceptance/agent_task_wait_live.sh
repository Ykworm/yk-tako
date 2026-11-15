#!/usr/bin/env bash
# Task 主动等待活体（doc/82；施工包 §12.2 场景 6/7/9/10）：wait_task 五态、bash_output 就地认领
# 不重复交付、submit_task_result 的后台命令拒绝、执行者工具面。
#
# 起法：按 scripts/acceptance/README.md 起一个开着调度循环的隔离引擎
# （建议 -bash-foreground-wait 3s -wait-window 8s -task-interval 1s），然后：
#   TAKO_BASE=http://127.0.0.1:8945 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc82 sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT=<仓库>/tmp/workspace-acc82 \
#     bash scripts/acceptance/agent_task_wait_live.sh
#
# 说明：场景 6 的「命中终态立即返回」由 6b/6c 的返回时序证明（wait=false 读终态立即返回）；
# 场景 10 的「声明面无 wait_task」由单测 TestTaskToolSurfaces 覆盖，这里留执行面的真实证据。
# 结构性说明（2026-11-15 活体三轮实测确认）：成功链的就地认领在发起 run 内**不可达**——
# 发布准入按项目静默（doc/76 §10.5.1，排除项目内所有活 run），而 delegate_task 强制发起 run
# 挂项目（RunWorkspace），所以发起 run 活着时发布进不了准入、Task 停在 publishing；
# 调度器一轮 tick 顺势做完「发布→收组→写通知→铸交付 run」（RunOnce 顺序），通知落库与
# 交付铸 run 之间没有可插run 的窗口。因此 wait_task 在发起 run 内的终态 reading 只有失败链
# （task_failed 不走发布，6b 断言就地认领）与派生/blocked（不认领，6c/6d）；成功链交付归
# 交付 run（attempt=1、consumed，6a 断言）。wait_task 机制本身（advanceOrchestration +
# ClaimNotificationInline）由 §12.1 单测钉住。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打 wait_task 五态 + 就地认领 + 提交拒绝链"
ml_signup waitlive

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"wait-live-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"
MAIN_SID=$SID
STAMP="$(date +%s)-$$"

wt_toolout(){ # $1=工具名 → 最后一条 tool_result 的 output 原文（当前 RID 的 frames）
  printf '%s' "$FRAMES" | jq -s -r --arg n "$1" \
    '[.[].data.frames[]? | select(.kind=="tool_result" and .name==$n) | .output] | last // ""' 2>/dev/null
}
wt_run(){ # $1=指令 → 投 run 落定，frames 存 FRAMES（交付 run 占闸时自动重试）
  at_run_retry "$1" "$WID" "$MAIN_SID"
  ml_wait_settled 150 || no "run 150s 没落定"
  ml_frames
}
wt_keyorder(){ # $1=JSON 原文 → 顶层键序（python3 解析保留插入序）
  python3 -c 'import json,sys; print(",".join(json.loads(sys.argv[1]).keys()))' "$1" 2>/dev/null
}
wt_session_runs(){ ml_sql "SELECT count(*) FROM runs WHERE session_id='$MAIN_SID'"; }

# ══════════════════════════════════════════════════════════════════════════
# 6a：成功链——发起 run 内 wait_task 停在 publishing（项目静默门槛，见头部说明）；
#     发起 run 结束后调度器发布→收组→交付 run 收口（attempt=1、consumed、无第二场交付）
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 6a：成功 + publishing + 交付 run 收口 ═══"
RUNS0=$(wt_session_runs)
wt_run "做两件事：
1. 调用 delegate_task 创建单件任务，content 为：在项目根创建文件 waited-ok-$STAMP.txt，内容写一行「ok-$STAMP」，然后调用 submit_task_result，summary 里写文件路径。agent 用 researcher。
2. 拿到创建回执里的 taskId 后，调用一次 wait_task（taskId 用回执原值，wait=true）。无论它返回什么状态，把返回的 status 和 result 原样写进回复，然后结束回复——不要再调用 wait_task。"
DLG=$(wt_toolout delegate_task)
TIDA=$(printf '%s' "$DLG" | jq -r '.taskId // ""' 2>/dev/null)
[ -n "$TIDA" ] && ok "delegate_task 返回 taskId $TIDA" || { no "delegate_task 没回 taskId：${DLG:0:200}"; ml_summary; }
[ -n "$(printf '%s' "$DLG" | jq -r '.orchestrationId // ""' 2>/dev/null)" ] \
  && ok "delegate_task 回执带编排组号" || no "delegate_task 没回 orchestrationId"
# 注：finalTaskId 是 create_orchestration（建组）的回执键，delegate_task 的回执只有
# taskId/orchestrationId/status 三个——创建回执键序由 orchestration_live.sh 对 HTTP 端点断言。
WTA=$(wt_toolout wait_task)
eq "发起 run 内 wait_task 停在 publishing（项目静默门槛）" "publishing" "$(printf '%s' "$WTA" | jq -r '.status // ""' 2>/dev/null)"
[ "$(printf '%s' "$WTA" | jq -r '.result.summary // ""' 2>/dev/null)" != "" ] \
  && ok "publishing 已带执行者提交的 result（结果 submit 时已落库，发布只管进 content/）" \
  || no "publishing 没带 result"
eq "wait_task 只调了一次（描述里的 publishing 指引，没有死循环轮询）" "1" \
  "$(printf '%s' "$FRAMES" | jq -s -r '[.[].data.frames[]? | select(.kind=="tool_result" and .name=="wait_task")] | length' 2>/dev/null)"
eq "返回体键序 taskId→orchestrationId→status→result→error" \
  "taskId,orchestrationId,status,result,error" "$(wt_keyorder "$WTA")"
OIDA=$(printf '%s' "$WTA" | jq -r '.orchestrationId // ""')
# 发起 run 已结束 → 项目静默 → 调度器发布、收组、铸交付 run；逐项轮询到位。
GRP=""; deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$deadline" ]; do
  GRP=$(ml_sql "SELECT status || '/' || publication_status || '/' || delivery_status FROM agent_orchestrations WHERE id='$OIDA'")
  [ "$GRP" = "succeeded/published/delivered" ] && break
  sleep 3
done
eq "组被收口成 succeeded/published/delivered" "succeeded/published/delivered" "$GRP"
eq "orchestration_finished 通知恰好一条（交付 run 认领，非就地）" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE orchestration_id='$OIDA' AND kind='orchestration_finished'")"
eq "交付 run 认领（claimed_in_place=false）" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE orchestration_id='$OIDA' AND kind='orchestration_finished' AND claimed_in_place")"
eq "交付 attempt 恰好一条" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.orchestration_id='$OIDA'")"
NF6A=""; deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  NF6A=$(ml_sql "SELECT status FROM agent_notifications WHERE orchestration_id='$OIDA' AND kind='orchestration_finished' LIMIT 1")
  [ "$NF6A" = "consumed" ] && break
  sleep 2
done
eq "通知最终 consumed" "consumed" "$NF6A"
PUBF="$WS_ROOT/tenants/$TEN/workspaces/$WID/content/waited-ok-$STAMP.txt"
[ -s "$PUBF" ] && ok "发布把执行者产物收进项目 content/：$PUBF" || no "产物没进 content/：$PUBF"
RUNS1=$(wt_session_runs)
eq "会话 run 数 =探针+发起+交付" "$((RUNS0 + 2))" "$RUNS1"
sleep 12
eq "会话空闲后没有第二场交付 run" "$RUNS1" "$(wt_session_runs)"

# ══════════════════════════════════════════════════════════════════════════
# 6b：失败态 + wait_task 就地认领失败通知——失败不走发布链（task_failed 与 Task 终态
#     同事务落库），发起 run 活着挡住交付铸 run，wait_task 轮询到 failed 即就地认领。
#     这是 wait_task「先收口/认领」写副作用在活体里的确定性证明（成功链归交付 run，见头部）。
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 6b：失败态 + 认领失败通知 ═══"
# verifyCommand 仅 isolated 档可用（shared 建 117111）；verify 对最终结果跑，不过即 failed。
# 执行者时延有方差（可能超过一个等待窗），指令里允许有界的第二次等待；就地认领要求发起
# run 在 Task 失败时仍活着（挡住交付铸 run），两次 wait=true ≈ 90s 覆盖。
RUNS6B=$(wt_session_runs)
wt_run "做两件事：
1. 调用 delegate_task 创建单件任务：workspaceMode 传 isolated，verifyCommand 传 test -f nonexistent-$STAMP.txt，content 为：直接调用 submit_task_result，summary 写「直接提交，不创建文件」。不要创建任何文件。agent 用 researcher。
2. 拿到创建回执里的 taskId 后，调用 wait_task（taskId 用回执原值，wait=true）：如果返回 running，隔几秒再调用一次 wait_task（wait=true）；拿到 failed 后（或已返回 failed）把最后一次返回的 status 和 error 里的 code 原样写进回复并结束——最多调用两次，不要再多。"
DLGB=$(wt_toolout delegate_task)
TIDB=$(printf '%s' "$DLGB" | jq -r '.taskId // ""' 2>/dev/null)
[ -n "$TIDB" ] && ok "delegate_task 返回失败任务的 taskId $TIDB" || { no "delegate_task 没回 taskId：${DLGB:0:200}"; ml_summary; }
WTB=$(wt_toolout wait_task)
eq "wait_task 返回 failed" "failed" "$(printf '%s' "$WTB" | jq -r '.status // ""' 2>/dev/null)"
[ "$(printf '%s' "$WTB" | jq -r '.error.code // ""' 2>/dev/null)" != "" ] \
  && ok "error 带着失败原因（code=$(printf '%s' "$WTB" | jq -r '.error.code // ""')）" || no "error 是空的"
eq "task_failed 通知被就地认领" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE task_id='$TIDB' AND kind='task_failed' AND claimed_in_place")"
eq "就地认领不写 attempt 行" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.task_id='$TIDB'")"
NF6B=""; deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  NF6B=$(ml_sql "SELECT status FROM agent_notifications WHERE task_id='$TIDB' AND kind='task_failed' LIMIT 1")
  [ "$NF6B" = "consumed" ] && break
  sleep 2
done
eq "run 结束即 consumed" "consumed" "$NF6B"
eq "失败也不重复交付：会话 run 数 =上一场+本场" "$((RUNS6B + 1))" "$(wt_session_runs)"

# ══════════════════════════════════════════════════════════════════════════
# 6c：blocked——上游失败把下游封成 blocked；wait_task 读到 blocked 不认领任何东西
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 6c：blocked 不认领 ═══"
# 上游失败靠「执行者 run 正常结束但没提交结果」（taskErrResultNotSubmit → failed）：
# 组级 verifyCommand 对每件任务都跑，造不出只 fail 上游的链，不用它。
TASKS=$(jq -cn --arg a "在项目根创建文件 blocked-a-$STAMP.txt，内容写一行 x。不要调用 submit_task_result，也不要再调用任何其他工具，直接结束回复。" \
  --arg b "在项目根创建文件 blocked-b-$STAMP.txt，内容一行 x，然后调用 submit_task_result。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"}]')
LINKS='[{"parent":"a","child":"b"}]'
RESP=$(at_create_orch "acc-wt-block-$STAMP" "$WID" "$MAIN_SID" "两步链，第一步注定失败" "$TASKS" "$LINKS")
OIDC=$(printf '%s' "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OIDC" ] || { echo "创建失败：$RESP" >&2; exit 2; }
TIDA_C=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OIDC' AND task_key='a'")
TIDB_C=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OIDC' AND task_key='b'")
if at_wait_task "$TIDA_C" "failed" 240; then
  ok "上游 a 失败"
else no "a 240s 没到 failed（$AT_LAST）"; ml_summary; fi
if at_wait_task "$TIDB_C" "blocked" 60; then
  ok "下游 b 被封成 blocked"
else no "b 没变 blocked（$AT_LAST）"; ml_summary; fi
CLAIMED0=$(ml_sql "SELECT count(*) FROM agent_notifications WHERE claimed_in_place")
wt_run "调用 wait_task（taskId=$TIDB_C，wait=false），把返回的 status 原样写进回复。"
WTC=$(wt_toolout wait_task)
eq "wait_task 返回 blocked" "blocked" "$(printf '%s' "$WTC" | jq -r '.status // ""' 2>/dev/null)"
CLAIMED1=$(ml_sql "SELECT count(*) FROM agent_notifications WHERE claimed_in_place")
eq "blocked 不认领任何通知" "$CLAIMED0" "$CLAIMED1"

# ══════════════════════════════════════════════════════════════════════════
# 6d：派生 interrupted——播种 running + 假 run 号；读侧派生不写回、不认领
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 6d：派生 interrupted（种子行） ═══"
"$PSQL" "$DSN" -q <<SQL
INSERT INTO agent_orchestrations (id, tenant_id, workspace_id, source_session_id, goal, status, final_task_id, idempotency_key, created_at, updated_at)
VALUES ('or_seed_$STAMP', '$TEN', '$WID', '$MAIN_SID', '种子：派生中断读面', 'active', 'at_seed_$STAMP', 'seed-orch-$STAMP', now(), now());
INSERT INTO agent_tasks (id, orchestration_id, task_key, tenant_id, workspace_id, content, agent_name, status, current_run_id, created_at, updated_at)
VALUES ('at_seed_$STAMP', 'or_seed_$STAMP', 'seed', '$TEN', '$WID', '种子任务（只为读面派生）', 'researcher', 'running', 'r_seed_missing_$STAMP', now(), now());
SQL
wt_run "调用 wait_task（taskId=at_seed_$STAMP，wait=false），把返回的 status 原样写进回复。"
WTD=$(wt_toolout wait_task)
eq "running + 假 run 号 → 读侧派生 interrupted" "interrupted" "$(printf '%s' "$WTD" | jq -r '.status // ""' 2>/dev/null)"
eq "存储态仍是 running（派生不写回）" "running" "$(ml_sql "SELECT status FROM agent_tasks WHERE id='at_seed_$STAMP'")"
eq "没有为种子任务写中断通知" "0" "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE task_id='at_seed_$STAMP'")"
"$PSQL" "$DSN" -q -c "DELETE FROM agent_tasks WHERE id='at_seed_$STAMP'; DELETE FROM agent_orchestrations WHERE id='or_seed_$STAMP';"
ok "种子行清理"

# ══════════════════════════════════════════════════════════════════════════
# 6e：窗口结束返回 running——执行者（sleep 60 + bash_output 等完）比等待窗（45s）长，
#     发起 run 内 wait=true 到点回 running 并结束回合；此后 Task 经发布链到 succeeded
#     （脚本侧 API 轮询——成功链终态不在发起 run 内出现，见头部结构性说明）
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 6e：窗口结束返回 running ═══"
# 执行者要跑 bash：researcher 规格排除 bash/bash_output/stop_bash（doc/82 §301），用 coder。
wt_run "做两件事：
1. 调用 delegate_task 创建单件任务，agent 用 coder，content 为：第一步调用 bash 工具，command 参数原样传：sleep 60（前台 3 秒会转后台返回命令号，不用等它）；第二步调用 bash_output（jobId 用命令号，wait=true）等命令完成；第三步调用 submit_task_result，summary 写后台命令已完成。
2. 拿到 taskId 后立刻调用一次 wait_task（wait=true）。无论它返回什么状态，把返回的 status 原样写进回复，然后结束回复——不要再调用 wait_task。"
WTES=$(printf '%s' "$FRAMES" | jq -s -r '[.[].data.frames[]? | select(.kind=="tool_result" and .name=="wait_task") | .output]' 2>/dev/null)
FIRST=$(printf '%s' "$WTES" | jq -r '.[0] | fromjson? | .status // ""' 2>/dev/null)
eq "第一次 wait=true 到点回 running（执行者还在等后台命令）" "running" "$FIRST"
TIDE=$(printf '%s' "$WTES" | jq -r '.[0] | fromjson? | .taskId // ""' 2>/dev/null)
[ -n "$TIDE" ] && [ "$TIDE" != "null" ] && ok "wait_task 回执带任务号 $TIDE" || { no "wait_task 回执没带 taskId：$(printf '%s' "$WTES" | head -c 200)"; ml_summary; }
if at_wait_task "$TIDE" "succeeded" 240; then
  ok "发起 run 结束后 Task 经发布链到 succeeded"
else no "240s 没到 succeeded（$AT_LAST）"; ml_summary; fi
eq "执行者提交的结果版本已登记（resultRevision≥1）" "1" \
  "$(ml_sql "SELECT CASE WHEN result_revision >= 1 THEN 1 ELSE 0 END FROM agent_tasks WHERE id='$TIDE'")"

# ══════════════════════════════════════════════════════════════════════════
# 场景 7：bash_output 读到终态就地认领——runs 与 attempts 都不变，run 结束即 consumed
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 7：bash_output 就地认领 ═══"
RUNS7=$(wt_session_runs)
wt_run "做两件事：
1. 调用 bash 工具，command 参数原样传：echo inline-claim-$STAMP; sleep 9（3 秒转后台返回命令号）。
2. 调用 bash_output（jobId 用命令号，wait=true）等它完成，把返回的 stdout 原样写进回复。"
BO=$(wt_toolout bash_output)
eq "bash_output 读到 succeeded" "succeeded" "$(printf '%s' "$BO" | jq -r '.status // ""' 2>/dev/null)"
case "$(printf '%s' "$BO" | jq -r '.stdout // ""')" in
  *"inline-claim-$STAMP"*) ok "stdout 尾部带着命令输出";; *) no "stdout 没有命令输出：$(printf '%s' "$BO" | jq -r '.stdout // ""' | head -c 120)";; esac
BJ7=$(printf '%s' "$BO" | jq -r '.jobId // ""')
eq "bash_job_finished 通知被就地认领" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id='$BJ7' AND claimed_in_place")"
eq "就地认领不写 attempt 行" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.background_job_id='$BJ7'")"
sleep 10
eq "run 结束即 consumed" "consumed" \
  "$(ml_sql "SELECT status FROM agent_notifications WHERE background_job_id='$BJ7' LIMIT 1")"
eq "没有第二场交付 run（本场 run 自身 +1）" "$((RUNS7 + 1))" "$(wt_session_runs)"

# ══════════════════════════════════════════════════════════════════════════
# 场景 9 + 10：submit 被后台命令拒绝 → 收口后再提交；执行者工具面有 bash_output、无 wait_task
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 9/10：提交拒绝 + 执行者工具面 ═══"
# 执行者要跑 bash：researcher 排除 bash/bash_output（doc/82 §301），用 coder。
RESP=$(at_create_task "acc-wt-submit-$STAMP" "$WID" "$MAIN_SID" \
  "按顺序做四步：
1. 调用 bash 工具，command 参数原样传：sleep 12（3 秒转后台返回命令号）。
2. 立刻调用 submit_task_result，summary 写「第一次提交」。这时会被告知还有后台命令未收口——这是预期现象，照着提示做下一步。
3. 调用 bash_output（jobId 用命令号，wait=true）等命令到终态。
4. 再调用 submit_task_result，summary 写「后台命令已收口」。" \
  "coder")
TID9=$(printf '%s' "$RESP" | jq -r '.data.taskId // ""')
[ -n "$TID9" ] || { echo "创建失败：$RESP" >&2; exit 2; }
if at_wait_task "$TID9" "succeeded" 240; then
  ok "拒绝后收口再提交 → Task succeeded"
else no "240s 没到 succeeded（$AT_LAST）"; ml_summary; fi
EXEC_RUN=$(ml_sql "SELECT current_run_id FROM agent_tasks WHERE id='$TID9'")
RID_SAVE=$RID; RID=$EXEC_RUN; ml_frames; RID=$RID_SAVE
NAMES=$(printf '%s' "$FRAMES" | jq -s -r '[.[].data.frames[]? | select(.kind=="tool_call") | .name] | unique | join(",")' 2>/dev/null)
case ",$NAMES," in *,bash_output,*) ok "执行者实际用上了 bash_output（$NAMES）";; *) no "执行者没调 bash_output：$NAMES";; esac
case ",$NAMES," in *,wait_task,*) no "执行者面不该有 wait_task：$NAMES";; *) ok "执行者没调 wait_task";; esac
REJECT=$(printf '%s' "$FRAMES" | jq -s -r '[.[].data.frames[]? | select(.kind=="tool_result" and .name=="submit_task_result") | .output] | .[0] // ""' 2>/dev/null)
case "$REJECT" in *"后台命令未收口"*) ok "第一次提交被拒：${REJECT:0:80}…";; *) no "第一次提交没被拒：${REJECT:0:120}";; esac

# ══════════════════════════════════════════════════════════════════════════
# 场景 11 半场（doc/82 §12.2 场景 11）：执行者来源 job 的可见性——
# 创建者读得到（200）；同租户 member 读/停都 404（不可见同形；member 过写面中间件，
# viewer 的 403 档由 bash_job_live.sh 场景 3 覆盖）。素材 = 场景 9 执行者的后台命令。
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 11 半场：执行者 job 可见性 ═══"
BJ9=$(ml_sql "SELECT id FROM background_jobs WHERE source_task_id='$TID9' AND backgrounded ORDER BY created_at DESC LIMIT 1")
[ -n "$BJ9" ] && ok "执行者的后台命令行在（$BJ9）" || { no "场景 9 执行者没留下后台命令行"; ml_summary; }
AT_CODE=""; AT_BODY=""
at_http GET "/api/jobs/$BJ9"
eq "创建者读执行者的 job → 200" "200" "$AT_CODE"
INV=""; deadline=$((SECONDS + 30))
while [ "$SECONDS" -lt "$deadline" ]; do
  INV=$(curl -sS -X POST "$B/api/tenants/$TEN/invitations" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"email":"wtmember-'"$STAMP"'@tako.test","role":"member"}')
  [ "$(printf '%s' "$INV" | jq -r '.code // ""')" = "0" ] && break
  sleep 2
done
ITOK=$(printf '%s' "$INV" | jq -r '.data.inviteToken // ""')
[ -n "$ITOK" ] && ok "owner 邀 member 成功" || { no "邀请失败：$INV"; ml_summary; }
REG=$(curl -sS -X POST "$B/api/auth/register-by-invite" -H 'Content-Type: application/json' \
  -d '{"token":"'"$ITOK"'","email":"wtmember-'"$STAMP"'@tako.test","password":"changeme123"}')
[ "$(printf '%s' "$REG" | jq -r .code)" = "0" ] || { no "member 注册失败：$REG"; ml_summary; }
MLOGIN=$(curl -sS -X POST "$B/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"identifier":"wtmember-'"$STAMP"'@tako.test","password":"changeme123"}')
MSW=$(curl -sS -X POST "$B/api/auth/switch-tenant" -H "Authorization: Bearer $(printf '%s' "$MLOGIN" | jq -r .data.accessToken)" -H 'Content-Type: application/json' \
  -d '{"tenantId":"'"$TEN"'","refreshToken":"'"$(printf '%s' "$MLOGIN" | jq -r '.data.refreshToken')"'"}')
MTOK=$(printf '%s' "$MSW" | jq -r '.data.accessToken')
M404=$(curl -sS "$B/api/jobs/$BJ9" -H "Authorization: Bearer $MTOK")
eq "同租户 member 读执行者 job → 404 117117（不可见同形）" "117117" "$(printf '%s' "$M404" | jq -r '.code')"
MSTOP=$(curl -sS "$B/api/jobs/$BJ9/stop" -X POST -H "Authorization: Bearer $MTOK")
eq "同租户 member 停执行者 job → 404（过写面中间件，撞可见性）" "117117" "$(printf '%s' "$MSTOP" | jq -r '.code')"

ml_summary
