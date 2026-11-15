#!/usr/bin/env bash
# 目标重规划与继任编排组活体（doc/75 §4/§5/§6/§8；施工包 §10.2）：脚本自起一台隔离引擎，
# 真模型跑组、同一服务真写 handoff／reuse／successor 标记——
#   1. 同组修订优先：检查点暂停 → 走 doc/72 在 a 与 b 之间补一件中间任务 → 组成功；
#      没有 handoff、没有继任组，上游 a 没有被重跑，中间任务的产物真进了工作现场；
#   2. 继任创建：原组成功 → 新项目里建继任组，handoff 五段与机器字段、reuse.json 与结果副本逐字节，
#      复用的证据真的进了继任组工作根；幂等重打、同键异方案 117102、二次继任 117114；
#   3. 原组未终态：执行者在等人 → 创建继任组先停原组 → waiting_previous → 调度器激活后才执行；
#   4. 拒绝面：infeasible／uncertain／缺继任原因／被取消任务／错版本／链外结果各回 117115；跨租户 404；
#   5. 预算：沿继任链造到第 8 层，第 9 次 117115，不落新组也不写新标记；
#   6. 删除原项目：继任组在自己的项目里仍能读 handoff、复用副本字节不变。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，一个空的隔离 PG 库）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/12（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT   默认 8953（被占就报错退出，不杀别人的进程）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎 PID。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT="${TAKO_ENGINE_PORT:-8953}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/12}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
if lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用；换 TAKO_ENGINE_PORT（不杀别人的进程）" >&2; exit 2
fi

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-replan.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG="$SCRATCH/engine.log"
PID=""
cleanup(){
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill -TERM "$PID" 2>/dev/null
    for _ in $(seq 40); do kill -0 "$PID" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$PID" 2>/dev/null
  fi
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"
export TAKO_WS_ROOT="$WS"

start_engine(){
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$PORT" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" -task-interval 2s
  ) >>"$ELOG" 2>&1 &
  PID=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$PORT/api/auth/login" 2>/dev/null; then return 0; fi
    if ! kill -0 "$PID" 2>/dev/null; then echo "引擎启动即退，日志：$ELOG" >&2; tail -20 "$ELOG" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎 30s 没起来，日志：$ELOG" >&2; exit 2
}

echo "自起引擎 127.0.0.1:$PORT · 库 $DSN · workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
start_engine
B="http://127.0.0.1:$PORT"
ml_signup replan

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
gws(){ echo "$(gdir "$1" "$2")/workspace"; }
res_of(){ echo "$(gws "$1" "$2")/results/$3/$4"; }

hash_of(){ shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1; }

# wait_orch_any $1=orchId $2=ERE $3=超时秒：组状态落进任一目标词即回 0；sets AT_LAST。
wait_orch_any(){
  local deadline=$((SECONDS + ${3:-240})) st
  AT_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_orch_status "$1"); AT_LAST="$st"
    [ -n "$st" ] && printf '%s' "$st" | grep -Eq "$2" && return 0
    sleep 1
  done
  return 1
}
wait_terminal(){ wait_orch_any "$1" '^(succeeded|failed|cancelled)$' "${2:-240}"; }
wait_task_any(){
  local deadline=$((SECONDS + ${3:-240})) st
  AT_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_task_status "$1"); AT_LAST="$st"
    [ -n "$st" ] && printf '%s' "$st" | grep -Eq "$2" && return 0
    sleep 1
  done
  return 1
}

# succ_send $1=原组 $2=请求体 JSON → sets AT_CODE/AT_BODY（真实写面，不做本地预判）。
succ_send(){ at_http POST "/api/orchestrations/$1/successors" "$2"; }
# reuse_entry $1=任务 id $2=结果版本 → `[{taskId,resultRevision}]`（用 jq 拼，不手写转义）。
reuse_entry(){ jq -cn --arg t "$1" --argjson r "${2:-1}" '[{taskId:$t,resultRevision:$r}]'; }
succ_body(){ # $1=key $2=goal $3=taskContent $4=outcome $5=reason $6=successorReason $7=reuseJSON $8=noReplayJSON [$9=workspaceId]
  local reuse="${7:-[]}" nr="${8:-[]}"
  jq -cn --arg k "$1" --arg g "$2" --arg c "$3" --arg oc "$4" --arg r "$5" --arg sr "$6" \
    --argjson reuse "$reuse" --argjson nr "$nr" --arg ws "${9:-}" \
    '{idempotencyKey:$k,goalAssessment:{outcome:$oc,evidence:"原组的目标已无法沿原路径继续；新路径依赖已核实可用"},
      reason:$r,successorReason:$sr,noReplay:$nr,reuse:$reuse,goal:$g,
      tasks:[{key:"c",content:$c,agent:"researcher"}],links:[]}
     + (if $ws != "" then {workspaceId:$ws} else {} end)' \
    || { echo "  succ_body 构建请求体失败：reuse=[$reuse] noReplay=[$nr]" >&2; return 1; }
}

WID1=$(new_project ws-replan-old)
at_probe_run "$WID1"
SID1="$SID"
WID2=$(new_project ws-replan-successor)
echo "  项目 old=$WID1 successor=$WID2 · 来源会话 $SID1"

STAMP="$(date +%s)-$$"

# ── 1. 同组修订优先：检查点 → 补中间任务 → 组成功；不产生继任组 ──────────────────
# 先让来源会话忙起来：检查点的 plan_review_required 通知只在来源会话空闲时才被认领，
# 忙线期间通知留在 pending，本脚本（人）的修订路径才是确定性的。
ml_run "调用 ask_user，问题固定为「忙线占位-$STAMP」，拿到回答后只回复 ok。" "$WID1" "$SID1"
BUSY_RID="$RID"
BUSY_QID=""
for _ in $(seq 1 120); do
  BUSY_QID=$(curl -sS "$B/api/runs/$BUSY_RID/questions?status=pending" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '.data[0].questionId // ""' 2>/dev/null)
  [ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] && break
  sleep 1
done
[ -n "$BUSY_QID" ] && [ "$BUSY_QID" != "null" ] || { echo "占位 run 没问出问题，无法制造忙线" >&2; exit 2; }
ok "来源会话已忙线（占位问题 $BUSY_QID），检查点通知会留在 pending"

TASKS_A=$(jq -cn \
  --arg a "在项目根创建文件 step-a.txt，内容写一行「A-$STAMP」。然后调用 submit_task_result，summary 写文件路径，artifacts 声明 step-a.txt（kind=file）。" \
  --arg b "项目根下应有 step-a.txt 与 step-c.txt（两份都应存在）。把两份原文依次写进 step-b.txt（先 a 的内容，再 c 的内容），然后调用 submit_task_result，summary 写 step-b.txt 的路径，artifacts 声明 step-b.txt（kind=file）。" \
  '[{key:"a",content:$a,agent:"researcher",planCheckpoint:true},{key:"b",content:$b,agent:"researcher"}]')
RESP=$(at_create_orch "acc-replan-revise-$STAMP" "$WID1" "$SID1" "同组修订不建继任组" "$TASKS_A" '[{"parent":"a","child":"b"}]')
OID_A=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_A" ] && [ "$OID_A" != "null" ] || { echo "建组失败：$RESP" >&2; exit 2; }
at_orch "$OID_A"
TA=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TB=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
echo "  场1 orchId=$OID_A a=$TA b=$TB"
if at_wait_hold "$OID_A" 300; then ok "场1 a 成功后组进入检查点暂停"; else no "场1 300s 没等到检查点暂停（a=$(at_task_status "$TA")）"; ml_summary; fi
eq "场1 初始 planRevision=0" "0" "$(at_orch_field "$OID_A" planRevision)"
eq "场1 plan_review_required 通知恰好 1 条" "1" "$(at_notif_count "$OID_A" "plan_review_required")"
eq "场1 暂停期间 b 没有执行会话" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_tasks WHERE id='$TB' AND execution_session_id IS NOT NULL")"

ADD_C=$(jq -cn --arg c "项目根下应有继承自上一步的 step-a.txt。读它，把原文抄进 step-c.txt，再另起一行写「C-$STAMP」，然后调用 submit_task_result，summary 写 step-c.txt 的路径，artifacts 声明 step-c.txt（kind=file）。" \
  '[{key:"c",content:$c,agent:"researcher"}]')
at_http POST "/api/orchestrations/$OID_A/revisions" \
  "$(jq -cn --arg k "acc-replan-insert-$STAMP" --argjson t "$ADD_C" \
    '{idempotencyKey:$k,baseRevision:0,reason:"检查点后在同组补一件中间任务",addTasks:$t,addLinks:[{parent:"a",child:"c"},{parent:"c",child:"b"}]}')"
eq "场1 修订回 code 0" "0" "$(echo "$AT_BODY" | jq -r .code)"
eq "场1 修订版本是 1" "1" "$(echo "$AT_BODY" | jq -r .data.revision)"
eq "场1 修订解除 planHold" "false" "$(echo "$AT_BODY" | jq -r .data.planHold)"
eq "场1 修订后最终节点仍是 b" "$TB" "$(echo "$AT_BODY" | jq -r .data.finalTaskId)"
TC=$(echo "$AT_BODY" | jq -r '.data.addedTaskIds[0] // ""')
[ -n "$TC" ] && [ "$TC" != "null" ] || { no "场1 修订没有回新增任务 id"; ml_summary; }

# 放开忙线：占位 run 收口，排队的陈旧复核通知被清账（不造复核 run）。
# 必须先放开：占位 run 是同项目活 run，发布准入要排除它，否则 b 会停在 publishing。
RESP=$(at_answer "$BUSY_QID" "收到")
eq "场1 放开忙线问题回 code 0" "0" "$(echo "$RESP" | jq -r .code)"
ml_wait_settled 60 || no "场1 占位 run 60s 没落定"
for _ in $(seq 1 60); do
  [ "$(at_notif_count "$OID_A" "plan_review_required" "consumed")" = "1" ] && break
  sleep 1
done
eq "场1 陈旧复核通知被清账（consumed）" "1" "$(at_notif_count "$OID_A" "plan_review_required" "consumed")"

if at_wait_orch "$OID_A" "succeeded" 600; then ok "场1 修订后组收口 succeeded"; else no "场1 600s 没收口（组 $AT_LAST）"; at_orch "$OID_A"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
OUT_B="$(cont_of "$WID1")/content/step-b.txt"
if grep -q "A-$STAMP" "$OUT_B" 2>/dev/null && grep -q "C-$STAMP" "$OUT_B" 2>/dev/null; then
  ok "场1 最终交付含 a 与 c 两段证据"
else
  no "场1 最终交付缺 a 或 c 证据（$OUT_B）"
fi
eq "场1 中间任务产物在 results 区（不进 content）" "1" \
  "$(grep -q "A-$STAMP" "$(res_of "$WID1" "$OID_A" "$TC" 1)/artifacts/step-c.txt" 2>/dev/null && echo 1 || echo 0)"
eq "场1 成功的 a 没有被重跑" "1" \
  "$(ml_sql "SELECT count(*) FROM runs r JOIN agent_tasks t ON t.execution_session_id=r.session_id WHERE t.id='$TA'")"
at_task "$TA"
eq "场1 a 结果版本=1" "1" "$(echo "$AT_TASK" | jq -r '.resultRevision // 0')"
eq "场1 没有 handoff（404 117004）" "117004" "$(curl -sS "$B/api/orchestrations/$OID_A/handoff" -H "Authorization: Bearer $TOK" | jq -r .code)"
[ ! -e "$(gdir "$WID1" "$OID_A")/successor.json" ] && ok "场1 没有 successor.json" || no "同组修订竟写了继任标记"
eq "场1 详情没有 replanOf" "null" "$(curl -sS "$B/api/orchestrations/$OID_A" -H "Authorization: Bearer $TOK" | jq -r '.data.orchestration.replanOf // "null"')"

if at_wait_delivered "$OID_A" 300; then ok "场1 交付完成"; else no "场1 300s 没交付（$(at_orch_field "$OID_A" deliveryStatus)）"; ml_summary; fi

# ── 2. 继任创建：换到新项目、复用旧结果、handoff 与副本、幂等与唯一性 ─────────
SRC_RESULT="$(res_of "$WID1" "$OID_A" "$TA" 1)"
[ -f "$SRC_RESULT/result.json" ] && ok "场2 旧结果 result.json 存在" || no "旧结果没有 result.json"
HASH_A_J=$(hash_of "$SRC_RESULT/result.json")
HASH_A_F=$(hash_of "$SRC_RESULT/artifacts/step-a.txt")
CONTENT_C="当前工作目录下有继承来的 step-a.txt（上一代任务的有效结果）。读它，把原文抄进 final-c.txt，再另起一行写「C2-$STAMP」，然后调用 submit_task_result，summary 写 final-c.txt 的路径，artifacts 声明 final-c.txt（kind=file）。"
BODY_S1=$(succ_body "acc-su1-$STAMP" "继任目标：换数据源重做最终交付" "$CONTENT_C" feasible \
  "数据源 X 已下线，原抓取步骤无法继续" "原组已成功但交付物需要换项目重建" \
  "[{\"taskId\":\"$TA\",\"resultRevision\":1}]" '["不要再次发送已经发出的外部通知"]' "$WID2")
succ_send "$OID_A" "$BODY_S1"
eq "场2 创建回 202" "202" "$AT_CODE"
S1=$(echo "$AT_BODY" | jq -r '.data.orchestrationId // ""')
eq "场2 响应 replanOf=原组" "$OID_A" "$(echo "$AT_BODY" | jq -r .data.replanOf)"
eq "场2 响应 replanDepth=1" "1" "$(echo "$AT_BODY" | jq -r .data.replanDepth)"
eq "场2 原组已终态 → 直接 active" "active" "$(echo "$AT_BODY" | jq -r .data.status)"
[ -n "$S1" ] && [ "$S1" != "null" ] || { no "没有继任组 id"; ml_summary; }

HO=$(curl -sS "$B/api/orchestrations/$S1/handoff" -H "Authorization: Bearer $TOK")
eq "场2 handoff replanOf" "$OID_A" "$(echo "$HO" | jq -r .data.replanOf)"
eq "场2 handoff replanDepth" "1" "$(echo "$HO" | jq -r .data.replanDepth)"
eq "场2 handoff reuse 条目" "$TA" "$(echo "$HO" | jq -r '.data.reuse[0].taskId')"
HB=$(echo "$HO" | jq -r .data.body)
printf '%s' "$HB" | grep -q '# 为什么重规划' && printf '%s' "$HB" | grep -q '# 为什么创建继任组' \
  && printf '%s' "$HB" | grep -q '# 目标可行性证据' && printf '%s' "$HB" | grep -q '# 哪些结果仍有效' \
  && printf '%s' "$HB" | grep -q '# 不得重放的动作' \
  && ok "场2 handoff 五段正文齐全" || no "handoff 正文缺段"
printf '%s' "$HB" | grep -q '不要再次发送已经发出的外部通知' && ok "场2 不得重放动作写入 handoff" || no "noReplay 没写进 handoff"
printf '%s' "$HB" | grep -q '数据源 X 已下线' && ok "场2 重规划原因写入 handoff" || no "reason 没写进 handoff"
printf '%s' "$HB" | grep -q 'step-a.txt' && ok "场2 复用产物的路径写进 handoff" || no "handoff 没写复用产物"
HFF="$(gdir "$WID2" "$S1")/handoff.md"
[ -f "$HFF" ] && grep -q '^kind: orchestration-replan$' "$HFF" && grep -q "^replanOf: $OID_A$" "$HFF" \
  && ok "场2 handoff 机器字段在文件里" || no "handoff 机器字段不对"
eq "场2 handoff 深度=1" "1" "$(awk -F': ' '/^replanDepth:/{print $2}' "$HFF" 2>/dev/null)"
grep -q "^  - taskId: $TA$" "$HFF" 2>/dev/null && grep -q '^    resultRevision: 1$' "$HFF" 2>/dev/null \
  && ok "场2 handoff frontmatter 带复用条目" || no "handoff frontmatter 复用条目不对"
SUCC_MARK="$(gdir "$WID1" "$OID_A")/successor.json"
[ -f "$SUCC_MARK" ] && [ "$(jq -r .successorOrchestrationId "$SUCC_MARK")" = "$S1" ] \
  && ok "场2 原组 successor.json 指向继任组" || no "原组没有正确的 successor.json"
eq "场2 successor.json 记规范化创建键" "successor:$OID_A:acc-su1-$STAMP" "$(jq -r .creationKey "$SUCC_MARK" 2>/dev/null)"
REUSE_F="$(gdir "$WID2" "$S1")/reuse.json"
eq "场2 reuse.json 记录来源组" "$OID_A" "$(jq -r .replanOf "$REUSE_F" 2>/dev/null)"
eq "场2 reuse.json 记录复用条目" "$TA" "$(jq -r '.reuse[0].taskId' "$REUSE_F" 2>/dev/null)"
eq "场2 reuse.json 记录复用版本" "1" "$(jq -r '.reuse[0].resultRevision' "$REUSE_F" 2>/dev/null)"
DST_RESULT="$(res_of "$WID2" "$S1" "$TA" 1)"
eq "场2 复用 result.json 逐字节" "$HASH_A_J" "$(hash_of "$DST_RESULT/result.json")"
eq "场2 复用产物逐字节" "$HASH_A_F" "$(hash_of "$DST_RESULT/artifacts/step-a.txt")"
eq "场2 详情追加 replanOf" "$OID_A" "$(curl -sS "$B/api/orchestrations/$S1" -H "Authorization: Bearer $TOK" | jq -r .data.orchestration.replanOf)"
eq "场2 详情追加 replanDepth" "1" "$(curl -sS "$B/api/orchestrations/$S1" -H "Authorization: Bearer $TOK" | jq -r .data.orchestration.replanDepth)"

if at_wait_orch "$S1" "succeeded" 600; then ok "场2 继任组真执行并收口 succeeded"; else no "场2 600s 没成功（$AT_LAST）"; at_orch "$S1"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
if at_wait_delivered "$S1" 300; then ok "场2 继任组交付完成"; else no "场2 继任组 300s 没交付"; ml_summary; fi
eq "场2 复用证据真的进了继任组工作现场" "2" \
  "$(grep -c -e "A-$STAMP" -e "C2-$STAMP" "$(cont_of "$WID2")/content/final-c.txt" 2>/dev/null)"
eq "场2 原组状态不改写" "succeeded" "$(at_orch_field "$OID_A" status)"

succ_send "$OID_A" "$BODY_S1"
eq "场2 同键同方案幂等回 202" "202" "$AT_CODE"
eq "场2 幂等回同一个继任组" "$S1" "$(echo "$AT_BODY" | jq -r .data.orchestrationId)"
eq "场2 原组只有一个继任组行" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$OID_A:%%'")"
BODY_CONFLICT=$(succ_body "acc-su1-$STAMP" "被悄悄改过的目标" "$CONTENT_C" feasible \
  "数据源 X 已下线，原抓取步骤无法继续" "原组已成功但交付物需要换项目重建" \
  "[{\"taskId\":\"$TA\",\"resultRevision\":1}]" '["不要再次发送已经发出的外部通知"]' "$WID2")
succ_send "$OID_A" "$BODY_CONFLICT"
eq "场2 同键异方案回 409" "409" "$AT_CODE"
eq "场2 同键异方案码 117102" "117102" "$(echo "$AT_BODY" | jq -r .code)"
BODY_SECOND=$(succ_body "acc-su1-second-$STAMP" "第二个继任者" "占位" feasible \
  "数据源 X 已下线" "第二个继任者测试" '[]' '[]' "$WID2")
succ_send "$OID_A" "$BODY_SECOND"
eq "场2 二次继任回 409" "409" "$AT_CODE"
eq "场2 二次继任码 117114" "117114" "$(echo "$AT_BODY" | jq -r .code)"

# ── 3. 原组未终态：先停原组 → waiting_previous → 调度器激活后才执行 ───────────
STAMP3="$(date +%s)-$$"
TASKS_C=$(jq -cn \
  --arg a "在项目根创建只读输入 c-a.txt，内容写一行「CA-$STAMP3」。然后调用 submit_task_result，summary 写路径，artifacts 声明 c-a.txt（kind=file）。" \
  --arg b "先调用 ask_user 工具提问「继任等待-场3」，拿到回答后只回复 ok。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"}]')
RESP=$(at_create_orch "acc-replan-wait-$STAMP3" "$WID1" "$SID1" "未终态原组先停再激活" "$TASKS_C" '[{"parent":"a","child":"b"}]')
OID_C=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_C" ] && [ "$OID_C" != "null" ] || { echo "建组失败：$RESP" >&2; exit 2; }
at_orch "$OID_C"
TCA=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TCB=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
echo "  场3 orchId=$OID_C a=$TCA b=$TCB"
if at_wait_task "$TCB" "running" 300; then ok "场3 第二件任务已在执行（模型在等人）"; else no "场3 b 没跑起来（$AT_LAST）"; ml_summary; fi
if at_wait_task_question "$TCB" 240; then ok "场3 执行者问出了问题（原组有活 run）"; else no "场3 没等到执行者提问"; ml_summary; fi
at_task "$TCA"
eq "场3 上游 a 已成功且版本=1" "1" "$(echo "$AT_TASK" | jq -r '.resultRevision // 0')"

BODY_S2=$(jq -cn --arg k "acc-su2-$STAMP3" \
  --argjson reuse "[{\"taskId\":\"$TCA\",\"resultRevision\":1}]" \
  --arg c "当前工作目录下有继承来的 c-a.txt（上一代任务的有效结果）。读它，把原文抄进 wait-d.txt，再另起一行写「D-$STAMP3」，然后调用 submit_task_result，summary 写 wait-d.txt 的路径，artifacts 声明 wait-d.txt（kind=file）。" \
  '{idempotencyKey:$k,goalAssessment:{outcome:"feasible",evidence:"换掉会卡的最终步骤，其余结果仍有效"},
    reason:"最终步骤等待人工，方案需要换路径",successorReason:"原组还有活 run，必须先收口",
    noReplay:["不要再次发送已经发出的外部通知"],reuse:$reuse,
    goal:"继承 a 的输入，重做最终交付",
    tasks:[{key:"d",content:$c,agent:"researcher"}],links:[]}')
succ_send "$OID_C" "$BODY_S2"
eq "场3 创建回 202" "202" "$AT_CODE"
S2=$(echo "$AT_BODY" | jq -r '.data.orchestrationId // ""')
ST2=$(echo "$AT_BODY" | jq -r .data.status)
case "$ST2" in
  waiting_previous|preparing) ok "场3 原组未终态 → 继任组先不可调度（$ST2）";;
  *) no "场3 期望 wait 状态，实际 $ST2";;
esac
[ -n "$S2" ] && [ "$S2" != "null" ] || { no "场3 没有继任组 id"; ml_summary; }
if wait_orch_any "$OID_C" 'cancelling|cancelled' 60; then ok "场3 原组已被持久化停止（$AT_LAST）"; else no "场3 原组 60s 没进入停止（$AT_LAST）"; ml_summary; fi
if wait_task_any "$TCB" '^cancelled$' 240; then ok "场3 原组 Task b 随停止取消"; else no "场3 Task b 240s 仍是 $AT_LAST"; ml_summary; fi
if wait_terminal "$OID_C" 240; then ok "场3 原组收口到 $AT_LAST"; else no "场3 原组 240s 没终态（$AT_LAST）"; ml_summary; fi
eq "场3 原组落到 cancelled" "cancelled" "$(at_orch_status "$OID_C")"
if at_wait_orch "$S2" "active" 300; then ok "场3 原组收口后继任组转 active"; else no "场3 继任组 300s 没激活（$AT_LAST）"; ml_summary; fi
if at_wait_orch "$S2" "succeeded" 600; then ok "场3 激活后真执行并成功"; else no "场3 600s 没成功（$AT_LAST）"; at_orch "$S2"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
if at_wait_delivered "$S2" 300; then ok "场3 继任组交付完成"; else no "场3 继任组 300s 没交付"; ml_summary; fi
eq "场3 复用证据进了继任组工作现场" "2" \
  "$(grep -c -e "CA-$STAMP3" -e "D-$STAMP3" "$(cont_of "$WID1")/content/wait-d.txt" 2>/dev/null)"
eq "场3 复用副本也在新组" "1" "$([ -f "$(res_of "$WID1" "$S2" "$TCA" 1)/result.json" ] && echo 1 || echo 0)"

# ── 4. 拒绝面：目标判断/原因/复用边界/跨租户 ──────────────────────────────────
STAMP4="$(date +%s)-$$"
TASKS_R=$(jq -cn --arg c "在项目根创建 r-input.txt，内容写一行「R-$STAMP4」。然后调用 submit_task_result，summary 写路径，artifacts 声明 r-input.txt（kind=file）。" \
  '[{key:"r",content:$c,agent:"researcher"}]')
RESP=$(at_create_orch "acc-replan-reject-$STAMP4" "$WID1" "$SID1" "拒绝面原组" "$TASKS_R" '[]')
OID_R=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_R" ] && [ "$OID_R" != "null" ] || { echo "建组失败：$RESP" >&2; exit 2; }
at_orch "$OID_R"
TR=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="r") | .taskId')
if at_wait_orch "$OID_R" "succeeded" 600; then ok "场4 拒绝面原组已成功"; else no "场4 原组 600s 没成功（$AT_LAST）"; ml_summary; fi

succ_send "$OID_R" "$(succ_body "rej-inf-$STAMP4" "x" "占位" infeasible "目标不可行" "不该建组" '[]' '[]')"
eq "场4 infeasible 回 400" "400" "$AT_CODE"
eq "场4 infeasible 码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '必须是 feasible' && ok "场4 原因点名可行性判定" || no "场4 infeasible 原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"
succ_send "$OID_R" "$(succ_body "rej-unc-$STAMP4" "x" "占位" uncertain "证据不足" "不该建组" '[]' '[]')"
eq "场4 uncertain 回 400" "400" "$AT_CODE"
eq "场4 uncertain 码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
succ_send "$OID_R" "$(succ_body "rej-noreason-$STAMP4" "x" "占位" feasible "目标可行" "" '[]' '[]')"
eq "场4 缺继任原因回 400" "400" "$AT_CODE"
eq "场4 缺继任原因码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '创建继任组的原因必填' && ok "场4 原因点名继任原因必填" || no "场4 缺原因文案不对：$(echo "$AT_BODY" | jq -r .data.reason)"

# 被取消任务：TCB 属于 OID_C，而 OID_C 在 S2 的重规划链上（链 [S2, OID_C]）。
succ_send "$S2" "$(succ_body "rej-cancelled-$STAMP4" "x" "占位" feasible "目标可行" "换路径" \
  "$(reuse_entry "$TCB" 1)" '[]')"
eq "场4 复用被取消任务回 400" "400" "$AT_CODE"
eq "场4 复用被取消任务码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '还没有成功的结果' && ok "场4 原因点名任务没有成功结果" || no "场4 取消任务原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"

succ_send "$OID_R" "$(succ_body "rej-rev-$STAMP4" "x" "占位" feasible "目标可行" "换路径" \
  "$(reuse_entry "$TR" 99)" '[]')"
eq "场4 复用错版本回 400" "400" "$AT_CODE"
eq "场4 复用错版本码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '第 99 版' && ok "场4 原因点名请求版本" || no "场4 版本原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"

succ_send "$OID_R" "$(succ_body "rej-chain-$STAMP4" "x" "占位" feasible "目标可行" "换路径" \
  "$(reuse_entry "$TA" 1)" '[]')"
eq "场4 复用链外任务回 400" "400" "$AT_CODE"
eq "场4 复用链外任务码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '不属于原组或它的重规划链' && ok "场4 原因点名链外结果" || no "场4 链外原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"
eq "场4 拒绝后原组没有继任组行" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$OID_R:%%'")"
[ ! -e "$(gdir "$WID1" "$OID_R")/successor.json" ] && ok "场4 拒绝后没有继任标记" || no "拒绝后仍写了继任标记"
eq "场4 拒绝后没有 handoff" "117004" "$(curl -sS "$B/api/orchestrations/$OID_R/handoff" -H "Authorization: Bearer $TOK" | jq -r .code)"

# 跨租户：另一账号（另一租户）对同一批 id 一律 404 117002。
SAVED_TOK="$TOK"; SAVED_TEN="$TEN"
ml_signup replan_other no_model
TOK2="$TOK"
eq "场4 跨租户读 handoff 回 404 117002" "117002" \
  "$(curl -sS "$B/api/orchestrations/$S1/handoff" -H "Authorization: Bearer $TOK2" | jq -r .code)"
BODY_CROSS=$(succ_body "cross-$STAMP4" "x" "占位" feasible "目标可行" "换路径" '[]' '[]')
CROSS_OUT=$(curl -sS -w '\n%{http_code}' -X POST "$B/api/orchestrations/$OID_R/successors" \
  -H "Authorization: Bearer $TOK2" -H 'Content-Type: application/json' -d "$BODY_CROSS" 2>/dev/null)
eq "场4 跨租户创建回 404" "404" "$(printf '%s' "$CROSS_OUT" | tail -1)"
eq "场4 跨租户创建码 117002" "117002" "$(printf '%s' "$CROSS_OUT" | sed '$d' | jq -r .code)"
TOK="$SAVED_TOK"; TEN="$SAVED_TEN"

# ── 5. 预算：沿继任链造到第 8 层，第 9 次拒绝 ──────────────────────────────────
# 每层先等原组终态才能再建下一层（preparing／waiting_previous 的原组一律 117114）。
CUR="$S1"; DEPTH=1; CHAIN_OK=1
while [ "$DEPTH" -lt 8 ]; do
  NEXT_DEPTH=$((DEPTH + 1))
  BODY_NEXT=$(succ_body "chain-$DEPTH-$STAMP" "第 $NEXT_DEPTH 层" "只回复 ok，不要调用 bash，也不要写文件；调用 submit_task_result，summary 写 ok。" feasible "链式重规划" "沿链推进" '[]' '[]')
  succ_send "$CUR" "$BODY_NEXT"
  if [ "$AT_CODE" != "202" ]; then
    no "场5 第 $NEXT_DEPTH 层创建失败（HTTP $AT_CODE：$(echo "$AT_BODY" | jq -r .message)）"; CHAIN_OK=0; break
  fi
  NEXT=$(echo "$AT_BODY" | jq -r .data.orchestrationId)
  eq "场5 第 $NEXT_DEPTH 层 replanOf=上一层" "$CUR" "$(echo "$AT_BODY" | jq -r .data.replanOf)"
  eq "场5 第 $NEXT_DEPTH 层 replanDepth" "$NEXT_DEPTH" "$(echo "$AT_BODY" | jq -r .data.replanDepth)"
  at_http POST "/api/orchestrations/$NEXT/stop"
  case "$AT_CODE" in
    202) ;;
    409) [ "$(echo "$AT_BODY" | jq -r .code)" = "117105" ] || { no "场5 第 $NEXT_DEPTH 层停止回 409 但码是 $(echo "$AT_BODY" | jq -r .code)"; CHAIN_OK=0; break; };;
    *) no "场5 第 $NEXT_DEPTH 层停止失败（HTTP $AT_CODE）"; CHAIN_OK=0; break;;
  esac
  if ! wait_terminal "$NEXT" 240; then no "场5 第 $NEXT_DEPTH 层 240s 没终态（$AT_LAST）"; CHAIN_OK=0; break; fi
  CUR="$NEXT"; DEPTH="$NEXT_DEPTH"
done
[ "$CHAIN_OK" = "1" ] && ok "场5 继任链逐层递增到第 8 层（每层都真写 handoff）"
eq "场5 第 8 层 handoff 深度" "8" "$(awk -F': ' '/^replanDepth:/{print $2}' "$(gdir "$WID2" "$CUR")/handoff.md" 2>/dev/null)"
ROWS_BEFORE=$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$CUR:%%'")
BODY_NINTH=$(succ_body "chain-9-$STAMP" "第 9 层" "占位" feasible "链式重规划" "沿链推进" '[]' '[]')
succ_send "$CUR" "$BODY_NINTH"
eq "场5 第 9 层回 400" "400" "$AT_CODE"
eq "场5 第 9 层码 117115" "117115" "$(echo "$AT_BODY" | jq -r .code)"
echo "$AT_BODY" | jq -r .data.reason | grep -q '超过上限 8' && ok "场5 原因点名上限 8" || no "场5 上限原因不对：$(echo "$AT_BODY" | jq -r .data.reason)"
eq "场5 超限没有多出行" "$ROWS_BEFORE" "$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE idempotency_key LIKE 'successor:$CUR:%%'")"
[ ! -e "$(gdir "$WID2" "$CUR")/successor.json" ] && ok "场5 超限没有写继任标记" || no "场5 超限仍写了继任标记"

# ── 6. 删除原项目：继任组在自己的项目里仍可读 handoff 与复用副本 ───────────────
if at_wait_delivered "$OID_R" 300; then ok "场6 拒绝面原组交付完成"; else no "场6 拒绝面原组 300s 没交付"; ml_summary; fi
idle_probe(){
  local a b c d e
  a=$(ml_sql "SELECT count(*) FROM runs WHERE workspace_id='$WID1' AND status='running' AND lease_until > now()")
  b=$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE workspace_id='$WID1' AND status IN ('active','cancelling','preparing','waiting_previous')")
  c=$(ml_sql "SELECT count(*) FROM agent_tasks WHERE workspace_id='$WID1' AND status IN ('pending','ready','running','publishing')")
  d=$(ml_sql "SELECT count(*) FROM agent_orchestrations WHERE workspace_id='$WID1' AND publication_status IN ('publishing','blocked')")
  e=$(ml_sql "SELECT count(*) FROM workspaces WHERE id='$WID1' AND publication_orchestration_id <> ''")
  echo "$a|$b|$c|$d|$e"
}
IDLE=""
for _ in $(seq 1 180); do
  IDLE=$(idle_probe)
  [ "$IDLE" = "0|0|0|0|0" ] && break
  sleep 1
done
eq "场6 原项目已无活 run／未收口组／在途发布" "0|0|0|0|0" "$IDLE"
HASH_COPY=$(hash_of "$DST_RESULT/artifacts/step-a.txt")
RES=$(curl -sS -X DELETE "$B/api/workspaces/$WID1" -H "Authorization: Bearer $TOK")
eq "场6 删除项目 status=deleted" "deleted" "$(echo "$RES" | jq -r .data.status)"
eq "场6 删除项目 cleanup=done" "done" "$(echo "$RES" | jq -r .data.cleanup)"
[ ! -e "$(cont_of "$WID1")" ] && ok "场6 原项目整容器消失" || no "场6 原项目容器还在"
eq "场6 原组的组容器随项目消失" "117004" \
  "$(curl -sS "$B/api/orchestrations/$OID_A/handoff" -H "Authorization: Bearer $TOK" | jq -r .code)"
eq "场6 继任组 handoff 仍可读" "$OID_A" \
  "$(curl -sS "$B/api/orchestrations/$S1/handoff" -H "Authorization: Bearer $TOK" | jq -r .data.replanOf)"
eq "场6 继任组详情 replanOf 仍可读" "$OID_A" \
  "$(curl -sS "$B/api/orchestrations/$S1" -H "Authorization: Bearer $TOK" | jq -r .data.orchestration.replanOf)"
eq "场6 复用副本字节不变" "$HASH_COPY" "$(hash_of "$DST_RESULT/artifacts/step-a.txt")"
eq "场6 继任组交付仍在自己的项目里" "2" \
  "$(grep -c -e "A-$STAMP" -e "C2-$STAMP" "$(cont_of "$WID2")/content/final-c.txt" 2>/dev/null)"

ml_summary
