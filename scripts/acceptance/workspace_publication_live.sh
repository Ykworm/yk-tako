#!/usr/bin/env bash
# 发布协调活体（doc/74 §9；施工包 §12.2 第 3 场）：预起引擎（调度循环开着），真模型跑四场——
#   1. 忙：同项目另有一场 ask_user 挂起的活 run → 发布准入按住（pending，不写标记）→ 答题收尾后自动发布；
#   2. 脏：Git 原项目有未提交跟踪修改 → 发布受阻 workspace_dirty → 人工移除 → retry → 发布；
#   3. 受阻 + 人工重新发布 + 权限：Directory 项目变化 → workspace_changed → 恢复基线 → retry 202 →
#      双击 200 幂等 → 发布；viewer retry 403；succeeded 后 retry 409 117112；
#   4. 多标签页事件：SSE 流收到 agent_tasks_changed / agent_orchestrations_changed，重拉与真实状态一致。
#
# 参数（全部环境变量，不写死本机路径端口）：
#   TAKO_BASE / TAKO_ENV / TAKO_PG_DSN（**必填**）/ TAKO_PSQL / TAKO_WS_ROOT / TAKO_MODEL
#   含义与默认同 memory_live_lib.sh 头注；引擎必须开着调度循环。
# 纪律：git init/commit/checkout 只发生在脚本自建的隔离项目夹具里；不杀任何进程；不调 /api/dev/reset。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-ws-pub.XXXXXX")
SSE_PID=""
cleanup(){ [ -n "$SSE_PID" ] && kill "$SSE_PID" 2>/dev/null; rm -rf "$SCRATCH"; }
trap cleanup EXIT

echo "引擎 $B · 发布协调四场（忙/脏/受阻+人工重新发布+权限/多标签页事件）"
ml_signup wspub

AGENT="wspub_$(date +%s)_$$"
jq -cn --arg n "$AGENT" \
  '{name:$n,kind:"subagent",description:"Git 隔离工作区执行者",spec:"你是隔离 Git worktree 的执行者。按任务内容改动文件，git add 并 git commit 后调用 submit_task_result 提交，artifacts 声明本次交付的文件（相对路径，必须已提交进 HEAD）。"}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @- >/dev/null

# 两个项目：Git（脏场）与 Directory（忙场/受阻场）。各起一场探针 run 造来源会话。
WG=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"name":"ws-pub-git"}' | jq -r .data.workspace.id)
WD=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"name":"ws-pub-dir"}' | jq -r .data.workspace.id)
{ [ -n "$WG" ] && [ "$WG" != "null" ] && [ -n "$WD" ] && [ "$WD" != "null" ]; } || { no "建项目失败"; ml_summary; }
PROJ_G="$WS_ROOT/tenants/$TEN/workspaces/$WG"
PROJ_D="$WS_ROOT/tenants/$TEN/workspaces/$WD"
mkdir -p "$PROJ_G" "$PROJ_D"
git -C "$PROJ_G" init -q -b main
printf '基线内容\n' > "$PROJ_G/report.md"
git -C "$PROJ_G" add report.md
git -C "$PROJ_G" -c user.name=probe -c user.email=probe@tako.test commit -qm seed
printf '基线内容\n' > "$PROJ_D/report.md"
at_probe_run "$WG"; SID_G="$SID"
at_probe_run "$WD"; SID_D="$SID"
echo "  Git 项目 $WG（会话 $SID_G）· Directory 项目 $WD（会话 $SID_D）"

ws_dir(){ echo "$WS_ROOT/tenants/$TEN/.tako/orchestrations/$1/workspace"; }
task_field(){ at_task "$1"; echo "$AT_TASK" | jq -r "$2 // empty"; }

# ── 1. 忙：同项目另有活 run → 发布准入按住 → 空闲后自动发布 ───────────────────────
STAMP="$(date +%s)-$$"
# 忙源是确定性的：ask_user 挂起的 run 保持 running + 活租约（另起会话，不挡来源会话交付）。
ml_run '请调用 ask_user 工具问我「选 A 还是 B？」，问完就停在那里等我的答案，不要自己替我选。' "$WD"
BUSY_RUN="$RID"; BUSY_SID="$SID"
QID=""
for i in $(seq 1 60); do
  QID=$(ml_sql "SELECT id FROM run_questions WHERE session_id='$BUSY_SID' AND status='pending' ORDER BY created_at DESC LIMIT 1")
  [ -n "$QID" ] && break
  sleep 1
done
[ -n "$QID" ] || { no "60s 没等到 ask_user 挂起"; ml_summary; }
ok "同项目另有一场挂起的活 run（$BUSY_RUN，question $QID）"
RESP=$(at_create_task "acc-wspub-busy-$STAMP" "$WD" "$SID_D" \
  "在工作目录创建文件 busy.txt，内容恰好一行「忙-$STAMP」。然后调用 submit_task_result，summary 写文件路径，artifacts 声明 busy.txt（kind=file）。" \
  "researcher" "isolated")
TID1=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID1=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID1" ] && [ "$TID1" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  场1 taskId=$TID1 orchId=$OID1"
if at_wait_task "$TID1" "publishing" 300; then ok "场1 执行完进入 publishing（发布被忙源按住）"; else no "场1 没到 publishing（$AT_LAST）"; at_task "$TID1"; echo "$AT_TASK" | jq -c '{status,workspace}'; ml_summary; fi
sleep 6   # 给慢路径几轮：正忙就必须按住不动
eq "忙时发布状态保持 pending" "pending" "$(task_field "$TID1" '.workspace.publication.status')"
eq "忙时不写发布标记" "" "$(ml_sql "SELECT publication_orchestration_id FROM workspaces WHERE id='$WD'")"
[ ! -f "$PROJ_D/busy.txt" ] && ok "忙时项目里还没有结果文件" || no "忙时结果文件已落项目（准入没按住）"
eq "忙源 run 仍 running + 活租约" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE id='$BUSY_RUN' AND status='running' AND lease_until > now()")"
ANS=$(curl -sS -X POST "$B/api/questions/$QID/answer" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"answer":"B"}')
eq "答题收尾忙源" "0" "$(echo "$ANS" | jq -r .code)"
if at_wait_task "$TID1" "succeeded" 180; then ok "场1 忙源收尾后自动发布 succeeded"; else no "场1 没到 succeeded（$AT_LAST）"; ml_summary; fi
eq "发布后项目 busy.txt 字节" "忙-$STAMP" "$(cat "$PROJ_D/busy.txt")"
eq "发布标记已清" "" "$(ml_sql "SELECT publication_orchestration_id FROM workspaces WHERE id='$WD'")"
if at_wait_delivered "$OID1" 180; then ok "场1 交付完成"; else no "场1 没交付"; ml_summary; fi

# ── 2. 脏：Git 原项目未提交跟踪修改 → workspace_dirty → 人工移除 → retry → 发布 ────
STAMP2="$(date +%s)-$$"
RESP=$(at_create_task "acc-wspub-dirty-$STAMP2" "$WG" "$SID_G" \
  "把 report.md 的内容改成恰好一行「脏场结果-$STAMP2」。git add 并 git commit，然后调用 submit_task_result，summary 写「完成」，artifacts 声明 report.md（kind=file）。" \
  "$AGENT" "isolated")
TID2=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID2=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID2" ] && [ "$TID2" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD2=$(ws_dir "$OID2")
echo "  场2 taskId=$TID2 orchId=$OID2"
for i in $(seq 1 60); do [ -f "$GD2/state.json" ] && break; sleep 1; done
[ -f "$GD2/state.json" ] || { no "60s 没等到组准备"; ml_summary; }
sleep 2
printf '未保存的脏修改\n' >> "$PROJ_G/report.md"   # 跟踪文件未提交修改（不 commit）
if at_wait_task "$TID2" "blocked" 300; then ok "场2 发布受阻 blocked"; else no "场2 没到 blocked（$AT_LAST）"; at_task "$TID2"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "受阻原因 workspace_dirty" "workspace_dirty" "$(task_field "$TID2" '.workspace.publication.errorCode')"
case "$(cat "$PROJ_G/report.md")" in *"未保存的脏修改"*) ok "受阻时脏修改没被自动清理/覆盖";; *) no "脏修改被引擎动了";; esac
# 人工移除脏修改（恢复到 HEAD），再 retry-publication。
git -C "$PROJ_G" checkout -- report.md
RES=$(curl -sS -X POST "$B/api/tasks/$TID2/retry-publication" -H "Authorization: Bearer $TOK")
eq "retry-publication 202" "0" "$(echo "$RES" | jq -r .code)"
if at_wait_task "$TID2" "succeeded" 300; then ok "场2 移除脏修改后重新发布 succeeded"; else no "场2 没到 succeeded（$AT_LAST）"; at_task "$TID2"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "发布后项目 report.md 是结果内容" "脏场结果-$STAMP2" "$(cat "$PROJ_G/report.md")"
if at_wait_delivered "$OID2" 180; then ok "场2 交付完成"; else no "场2 没交付"; ml_summary; fi

# ── 3. 受阻 + 人工重新发布 + 权限（SSE 从这场开始前挂读，场4 用）────────────────────
curl -sS -N "$B/api/events" -H "Authorization: Bearer $TOK" > "$SCRATCH/events.sse" 2>/dev/null &
SSE_PID=$!
sleep 1
STAMP3="$(date +%s)-$$"
RESP=$(at_create_task "acc-wspub-block-$STAMP3" "$WD" "$SID_D" \
  "把 report.md 的内容改成恰好一行「受阻结果-$STAMP3」。然后调用 submit_task_result，summary 写「完成」，artifacts 声明 report.md（kind=file）。" \
  "researcher" "isolated")
TID3=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID3=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID3" ] && [ "$TID3" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD3=$(ws_dir "$OID3")
echo "  场3 taskId=$TID3 orchId=$OID3"
for i in $(seq 1 60); do [ -f "$GD3/state.json" ] && break; sleep 1; done
[ -f "$GD3/state.json" ] || { no "60s 没等到组准备"; ml_summary; }
sleep 2
printf '人改\n' > "$PROJ_D/report.md"
if at_wait_task "$TID3" "blocked" 300; then ok "场3 发布受阻 blocked"; else no "场3 没到 blocked（$AT_LAST）"; at_task "$TID3"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "受阻原因 workspace_changed" "workspace_changed" "$(task_field "$TID3" '.workspace.publication.errorCode')"
# 权限：viewer 不能 retry（403 111203）。邀请 → 注册 → 登录 → 切租户。
VEM="viewer_$(date +%s)_$$@tako.test"; VPW="viewer_pw_$$"
INV=$(curl -sS -X POST "$B/api/tenants/$TEN/invitations" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"email":"'"$VEM"'","role":"viewer"}')
ITOK=$(echo "$INV" | jq -r '.data.inviteToken // ""')
[ -n "$ITOK" ] && [ "$ITOK" != "null" ] || { no "发 viewer 邀请失败：$INV"; ml_summary; }
curl -sS -X POST "$B/api/auth/register-by-invite" -H 'Content-Type: application/json' -d '{"token":"'"$ITOK"'","email":"'"$VEM"'","password":"'"$VPW"'"}' >/dev/null
VLOGIN=$(curl -sS -X POST "$B/api/auth/login" -H 'Content-Type: application/json' -d '{"email":"'"$VEM"'","password":"'"$VPW"'"}')
VACCESS=$(echo "$VLOGIN" | jq -r .data.accessToken); VREFRESH=$(echo "$VLOGIN" | jq -r .data.refreshToken)
VSW=$(curl -sS -X POST "$B/api/auth/switch-tenant" -H "Authorization: Bearer $VACCESS" -H 'Content-Type: application/json' -d '{"tenantId":"'"$TEN"'","refreshToken":"'"$VREFRESH"'"}')
VTOK=$(echo "$VSW" | jq -r .data.accessToken)
[ -n "$VTOK" ] && [ "$VTOK" != "null" ] || { no "viewer 就位失败：$VSW"; ml_summary; }
VRES=$(curl -sS -w '\n%{http_code}' -X POST "$B/api/tasks/$TID3/retry-publication" -H "Authorization: Bearer $VTOK")
VCODE=${VRES##*$'\n'}; VBODY=${VRES%$'\n'*}
eq "viewer retry 403" "403" "$VCODE"
eq "viewer retry 业务码 111203" "111203" "$(echo "$VBODY" | jq -r .code)"
# 人工恢复基线 → retry 202 → 双击 200 幂等 → 发布。
cp "$GD3/provider/baseline/report.md" "$PROJ_D/report.md"
RES=$(curl -sS -w '\n%{http_code}' -X POST "$B/api/tasks/$TID3/retry-publication" -H "Authorization: Bearer $TOK")
RCODE=${RES##*$'\n'}; RBODY=${RES%$'\n'*}
eq "owner retry 202" "202" "$RCODE"
RETRY_RUN=$(echo "$RBODY" | jq -r '.data.runId // ""')
RES2=$(curl -sS -w '\n%{http_code}' -X POST "$B/api/tasks/$TID3/retry-publication" -H "Authorization: Bearer $TOK")
R2CODE=${RES2##*$'\n'}; R2BODY=${RES2%$'\n'*}
eq "处理 run 活着时双击 200 幂等" "200" "$R2CODE"
eq "双击回的 runId 与首次一致" "$RETRY_RUN" "$(echo "$R2BODY" | jq -r '.data.runId // ""')"
if at_wait_task "$TID3" "succeeded" 300; then ok "场3 重新发布 succeeded"; else no "场3 没到 succeeded（$AT_LAST）"; at_task "$TID3"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "发布后项目 report.md 是结果内容" "受阻结果-$STAMP3" "$(cat "$PROJ_D/report.md")"
# succeeded 之后不能再 retry（409 117112）。
RES3=$(curl -sS -w '\n%{http_code}' -X POST "$B/api/tasks/$TID3/retry-publication" -H "Authorization: Bearer $TOK")
R3CODE=${RES3##*$'\n'}; R3BODY=${RES3%$'\n'*}
eq "succeeded 后 retry 409" "409" "$R3CODE"
eq "succeeded 后 retry 业务码 117112" "117112" "$(echo "$R3BODY" | jq -r .code)"
if at_wait_delivered "$OID3" 180; then ok "场3 交付完成"; else no "场3 没交付"; ml_summary; fi

# ── 4. 多标签页事件：SSE 收到变更事件，重拉与真实状态一致 ─────────────────────────
sleep 2   # 让 SSE 把场3 的事件都吐出来
kill "$SSE_PID" 2>/dev/null; SSE_PID=""
eq "SSE 收到本件 Task 的 agent_tasks_changed" "1" "$(grep -c '"type":"agent_tasks_changed","id":"'"$TID3"'"' "$SCRATCH/events.sse" | tr -d ' ' | awk '{print ($1>0)?1:0}')"
eq "SSE 收到本组的 agent_orchestrations_changed" "1" "$(grep -c '"type":"agent_orchestrations_changed","id":"'"$OID3"'"' "$SCRATCH/events.sse" | tr -d ' ' | awk '{print ($1>0)?1:0}')"
# 第二个标签页收到事件后重拉：读到的就是最新真实状态（拉是正确性）。
eq "重拉 Task 详情是 succeeded（与事件一致）" "succeeded" "$(task_field "$TID3" '.status')"
eq "重拉组详情交付状态 delivered" "delivered" "$(at_orch_field "$OID3" deliveryStatus)"

ml_summary