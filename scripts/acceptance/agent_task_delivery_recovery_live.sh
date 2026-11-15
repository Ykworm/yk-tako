#!/usr/bin/env bash
# 协调通知与交付恢复活体（doc/73；施工包 §8.3 的十场）：
#   1. pending 双副本认领——两个调度器同时盯同一条通知，恰好一场交付 run；
#   2. claimed＋run 落库、worker 还没产出时 kill -9 → 同一场 run 续跑（epoch+1）；
#   3. 交付中（流已开始）kill -9 → 同一场 run 续跑 → 交付一次；
#   4. run 已 complete、通知还没收尾时「崩溃」→ 只靠对账补交付，不新建 run；
#   5. 真实模型调用 error（租户密钥改成不可用）→ 通知 failed → 换回密钥 → 人工重试 → delivered；
#   6. 用户停止交付 run → 通知 failed(delivery_stopped) → 不自动重启 → 人工重试；
#   7. 双副本并发重试只增一次 attempt；
#   8. 结果文件可通过 GET /api/media 打开；删掉后重试回 117110；补回文件后可恢复；
#   9. 非属主 / viewer / 跨租户读与重试被拒（404 117003 / 403 111203）；
#  10. 丢掉内部唤醒（删流 + 杀进程）后只靠 PG 启动对账收敛，不重复交付。
#
# 本脚本自己起停隔离引擎（kill 只打自己起的 PID），起法沿 agent_task_two_replicas_live.sh：
#   TAKO_PG_DSN        空的隔离 PG 库（**必填**；脚本只建 pg_search 扩展，不调 /api/dev/reset）
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/14（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT_A/B  默认 8958 / 8959（被占就报错退出，不杀别人的进程）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_GO            默认 go，找不到就试 $HOME/sdk/go1.27.0/bin/go
#   TAKO_LEASE_WAIT    等租约过期的秒数，默认 35（引擎租约 TTL 30s）
#
# 造态纪律（施工包 §8.3）：error 用真实但不可用的租户密钥制造；部分输出用真实流开始后 kill -9；
# 场景 4 的「complete 后、通知收尾前」窗口在进程内只有几微秒，脚本按列的真实语义还原这个窗口
# （通知 claimed + run complete + 组 delivering），之后完全由真实调度对账收敛。其余场景全部走真实链路。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8958}"
PORT_B="${TAKO_ENGINE_PORT_B:-8959}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/14}"
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

# redis_int $1=url 其余=命令 → 打印整数回复（DEL / EXISTS 用；本机没有 redis-cli）。
redis_int(){
  python3 - "$@" <<'PY'
import sys, socket, re
url, args = sys.argv[1], sys.argv[2:]
m = re.match(r'redis://([^:/]+):(\d+)(?:/(\d+))?', url)
if not m:
    print("bad redis url: " + url, file=sys.stderr); sys.exit(2)
host, port, db = m.group(1), int(m.group(2)), int(m.group(3) or 0)
def enc(*words):
    out = b'*%d\r\n' % len(words)
    for w in words:
        b = w.encode()
        out += b'$%d\r\n%s\r\n' % (len(b), b)
    return out
s = socket.create_connection((host, port), 5)
s.sendall(enc('SELECT', str(db))); s.recv(1024)
s.sendall(enc(*args))
buf = s.recv(4096)
s.close()
if not buf.startswith(b':'):
    print(buf.decode(errors='replace').strip(), file=sys.stderr); sys.exit(1)
print(buf[1:].split(b'\r\n')[0].decode())
PY
}

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-delivery-recovery.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG_A="$SCRATCH/engine-a.log"
ELOG_B="$SCRATCH/engine-b.log"
PID_A=""; PID_B=""
cleanup(){
  for pid in "$PID_A" "$PID_B"; do
    [ -n "$pid" ] || continue
    kill -0 "$pid" 2>/dev/null || continue
    kill -TERM "$pid" 2>/dev/null
    for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$pid" 2>/dev/null
  done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"
# 所有副本同一 JWT 密钥：一台签发的 token 在另一台上同样有效（多副本同身份）。

start_engine(){ # $1=port $2=log $3=interval → 打印 PID（就绪后才回）
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" \
      -task-interval "$3"
  ) >>"$2" 2>&1 &
  local pid=$!
  for _ in $(seq 80); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 40s 没起来，日志：$2" >&2; exit 2
}

stop_engine(){ # $1=PID（只打自己起的）
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null
  for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.25; done
  kill -9 "$pid" 2>/dev/null
}

# ── 读面帮手 ──────────────────────────────────────────────────────────────────
http_as(){ # $1=METHOD $2=path $3=body(可空) $4=token → sets AT_CODE / AT_BODY
  local out=""
  if [ -n "${3:-}" ]; then
    out=$(curl -sS -w '\n%{http_code}' -X "$1" "$B$2" \
      -H "Authorization: Bearer $4" -H 'Content-Type: application/json' -d "$3" 2>/dev/null)
  else
    out=$(curl -sS -w '\n%{http_code}' -X "$1" "$B$2" \
      -H "Authorization: Bearer $4" 2>/dev/null)
  fi
  if [ -z "$out" ]; then AT_CODE=""; AT_BODY=""; return 0; fi
  AT_CODE=${out##*$'\n'}
  AT_BODY=${out%$'\n'*}
}

notif_of(){ # $1=orchId $2=kind → 通知 id（没有回空串）
  ml_sql "SELECT id FROM agent_notifications WHERE orchestration_id='$1' AND kind='$2' ORDER BY created_at DESC LIMIT 1"
}
notif_field(){ ml_sql "SELECT COALESCE($2,'') FROM agent_notifications WHERE id='$1'"; }
notif_status(){ notif_field "$1" status; }
group_field(){ ml_sql "SELECT COALESCE($2,'') FROM agent_orchestrations WHERE id='$1'"; }
run_status(){ ml_sql "SELECT COALESCE(status,'') FROM runs WHERE id='$1'"; }
run_epoch(){ ml_sql "SELECT lease_epoch FROM runs WHERE id='$1'"; }
attempt_rows(){ ml_sql "SELECT count(*) FROM agent_notification_attempts WHERE notification_id='$1'"; }
attempt_run(){ ml_sql "SELECT run_id FROM agent_notification_attempts WHERE notification_id='$1' AND attempt=$2"; }
session_runs(){ ml_sql "SELECT count(*) FROM runs WHERE session_id='$1'"; }
task_field(){ ml_sql "SELECT COALESCE($2::text,'') FROM agent_tasks WHERE id='$1'"; }

wait_notif_status(){ # $1=notifId $2=status $3=超时秒
  local deadline=$((SECONDS + ${3:-120})) st
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(notif_status "$1"); [ "$st" = "$2" ] && return 0; sleep 0.2
  done
  return 1
}

wait_claimed(){ # $1=orchId $2=超时秒 → sets NID / CRID（通知 claimed 且 claim_run_id 写上）
  local deadline=$((SECONDS + ${2:-240})) row st rid
  NID=""; CRID=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    row=$(ml_sql "SELECT COALESCE(id,'')||'|'||status||'|'||COALESCE(claim_run_id,'') FROM agent_notifications WHERE orchestration_id='$1' AND kind='orchestration_finished' ORDER BY created_at DESC LIMIT 1")
    NID=${row%%|*}; row=${row#*|}; st=${row%%|*}; rid=${row#*|}
    if [ "$st" = "claimed" ] && [ -n "$rid" ] && [ "$rid" != "$row" ]; then
      CRID=$rid; return 0
    fi
    NID=""; CRID=""
    sleep 0.05
  done
  return 1
}

wait_lease_expire(){ # $1=runId → 等到租约过期（数据库时钟）
  local deadline=$((SECONDS + LEASE_WAIT + 15)) v
  while [ "$SECONDS" -lt "$deadline" ]; do
    v=$(ml_sql "SELECT (lease_until < now()) FROM runs WHERE id='$1'")
    [ "$v" = "t" ] && return 0
    sleep 1
  done
  return 1
}

delta_count(){ # $1=runId → frames 里的非空 delta 条数
  curl -sS "$B/api/runs/$1/frames?eventOffset=0" -H "Authorization: Bearer $TOK" 2>/dev/null |
    jq -r '[.data.frames[]? | select(.kind=="delta" and ((.text // "")|length)>0)] | length' 2>/dev/null
}

wait_delta(){ # $1=runId $2=超时秒
  local deadline=$((SECONDS + ${2:-60})) n
  while [ "$SECONDS" -lt "$deadline" ]; do
    n=$(delta_count "$1"); [ "${n:-0}" -ge 1 ] 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# create_one $1=key 后缀 $2=任务正文 → sets RESP/OID/TID（单节点组，final Task 就是它）
create_one(){
  local key="acc-73-$1-$STAMP" tasks
  tasks=$(jq -cn --arg c "$2" '[{key:"final",content:$c,agent:"researcher"}]')
  RESP=$(at_create_orch "$key" "$WID" "$SRC" "73 活体：$1" "$tasks" '[]')
  OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
  if [ -z "$OID" ] || [ "$OID" = "null" ]; then echo "创建失败：$RESP" >&2; return 1; fi
  TID=$(ml_sql "SELECT id FROM agent_tasks WHERE orchestration_id='$OID' AND status <> 'blocked' ORDER BY created_at LIMIT 1")
  [ -n "$TID" ] || { echo "创建后找不到 Task：$RESP" >&2; return 1; }
  PRE_RUNS=$(session_runs "$SRC")
}

# set_tenant_key $1=apiKey：租户模型密钥整体替换（配置版本变化 → 下一次装配换客户端）。
set_tenant_key(){
  local resp
  resp=$(curl -sS -X PUT "$B/api/tenants/$TEN/runtime-config" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"modelRef":"'"$MODEL"'","mcpServers":{},"secrets":{"DEEPSEEK_API_KEY":"'"$1"'"},"sandbox":{"network":false}}')
  [ "$(echo "$resp" | jq -r .code 2>/dev/null)" = "0" ]
}

GOOD_KEY=$(ml_key)
[ -n "$GOOD_KEY" ] || { echo "读不到模型密钥：${ENV}（用 TAKO_ENV 指定）" >&2; exit 2; }

echo "引擎 A 127.0.0.1:$PORT_A · 引擎 B 127.0.0.1:$PORT_B · 共库 · Redis $REDIS_URL · workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
B="http://127.0.0.1:$PORT_A"
ml_signup delivery73

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"delivery-recovery-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
SRC="$SID"
ok "隔离引擎、项目 $WID 与来源会话 $SRC 就位"

STAMP="$(date +%s)-$$"
# doc/76：产物路径相对用户内容根 content/（容器根另有 layout.json 与 runtime/）。
WS_DIR="$WS/tenants/$TEN/workspaces/$WID/content"
mkdir -p "$WS_DIR"

# ═══ 场景 1：pending 双副本认领 ═══════════════════════════════════════════════
echo
echo "── 场景 1：两条调度器盯同一条 pending 通知，只许一场交付 run"
PID_B=$(start_engine "$PORT_B" "$ELOG_B" 2s)
create_one dual "只回复 ok，然后调用 submit_task_result，summary 写 ok。" || ml_summary
if at_wait_delivered "$OID" 300; then ok "组完成并交付"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
N1=$(notif_of "$OID" "orchestration_finished")
eq "组通知恰好 1 条" "1" "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE orchestration_id='$OID' AND kind='orchestration_finished'")"
eq "处理尝试恰好 1 行" "1" "$(attempt_rows "$N1")"
eq "通知 consumed" "consumed" "$(notif_status "$N1")"
eq "组 delivery_run_id 指向唯一一场交付" "$(attempt_run "$N1" 1)" "$(group_field "$OID" delivery_run_id)"
eq "来源会话只多一场 run（探针之外恰好一次交付）" "$((PRE_RUNS + 1))" "$(session_runs "$SRC")"
echo "  ℹ️ 领取分布：引擎 A $(grep -c 'task delivery: launching' "$ELOG_A" 2>/dev/null) 次；引擎 B $(grep -c 'task delivery: launching' "$ELOG_B" 2>/dev/null) 次"
stop_engine "$PID_B"; PID_B=""

# ═══ 场景 5：真实模型 error → failed → 人工重试 → delivered ═══════════════════
echo
echo "── 场景 5：占住来源会话让交付认领推迟，换不可用密钥后放行，制造真实模型 error"
create_one err "只回复 ok，然后调用 submit_task_result，summary 写 ok。" || ml_summary
# 来源会话里先挂一场等人回答的聊天 run：组收口后通知保持 pending（认领被会话忙挡住），
# 这是产品里真实存在的推迟路径，也让「换密钥 → 放行 → 交付 run 用坏密钥」有确定窗口。
# 忙线 run 必须挂在另一个项目（doc/76 §4 起发布准入排除同项目的其它活 run）：它若占着 WID，
# 最终 Task 的发布一直等不到静默，连 succeeded 都到不了，通知更不会产生。
WID_BUSY=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"73-delivery-busy-ws"}' | jq -r .data.workspace.id)
[ -n "$WID_BUSY" ] && [ "$WID_BUSY" != "null" ] || { no "建忙线项目失败"; ml_summary; }
PATCH=$(curl -sS -X PATCH "$B/api/sessions/$SRC" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$WID_BUSY\"}")
eq "来源会话挪到忙线项目" "0" "$(echo "$PATCH" | jq -r .code)"
ml_run "调用 ask_user，问题固定为「73-BLOCK-$STAMP」，拿到回答后只回复 ok。" "$WID_BUSY" "$SRC"
BLOCK_RID=$RID
PATCH=$(curl -sS -X PATCH "$B/api/sessions/$SRC" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d "{\"workspaceId\":\"$WID\"}")
eq "来源会话挪回任务项目" "0" "$(echo "$PATCH" | jq -r .code)"
if at_wait_task "$TID" "succeeded" 300; then
  ok "最终 Task 已 succeeded（结果已保存）"
else no "300s 没跑到 succeeded（$AT_LAST）"; fi
EXEC_RUN5=$(task_field "$TID" current_run_id)
N5=""
for _ in $(seq 1 120); do
  N5=$(notif_of "$OID" "orchestration_finished")
  [ -n "$N5" ] && [ "$(notif_status "$N5")" = "pending" ] && break
  sleep 0.5
done
if [ -n "$N5" ] && [ "$(notif_status "$N5")" = "pending" ]; then
  ok "来源会话忙时通知保持 pending（交付认领被推迟）"
else no "通知没停在 pending（$(notif_status "$N5")）"; fi
# 交付 run 装配时读租户密钥：换成不可用值后放行，第一次模型调用必然失败。
if set_tenant_key "sk-invalid-delivery-73"; then ok "租户密钥换成不可用值"; else no "换密钥失败"; fi
http_as POST "/api/runs/$BLOCK_RID/stop" "" "$TOK"
eq "占位聊天 run 已停止" "0" "$(echo "$AT_BODY" | jq -r .code)"
if [ -n "$N5" ]; then
  if wait_notif_status "$N5" failed 180; then
    ok "通知翻成 failed（真实模型 error 由交付 run 的终态对账得到）"
  else no "180s 通知没 failed（$(notif_status "$N5")）"; fi
fi
eq "机器原因 = delivery_run_error" "delivery_run_error" "$(notif_field "$N5" consume_error_code)"
eq "操作历史 1 行、状态 error" "error|1" "$(ml_sql "SELECT COALESCE(r.status,'')||'|'||count(*) FROM agent_notification_attempts a LEFT JOIN runs r ON r.id=a.run_id WHERE a.notification_id='$N5' GROUP BY r.status")"
eq "组交付状态 failed" "failed" "$(group_field "$OID" delivery_status)"
http_as GET "/api/notifications/$N5" "" "$TOK"
eq "详情 200" "200" "$AT_CODE"
eq "详情 errorCode 指向机器原因" "delivery_run_error" "$(echo "$AT_BODY" | jq -r '.data.errorCode')"
if set_tenant_key "$GOOD_KEY"; then ok "租户密钥换回可用值"; else no "恢复密钥失败"; fi
http_as POST "/api/notifications/$N5/retry" "" "$TOK"
eq "人工重试 202" "202" "$AT_CODE"
eq "新尝试 = 第 2 次" "2" "$(echo "$AT_BODY" | jq -r '.data.attempt')"
NEW_RUN=$(echo "$AT_BODY" | jq -r '.data.runId')
if at_wait_delivered "$OID" 300; then ok "重试后交付完成"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "处理尝试 2 行" "2" "$(attempt_rows "$N5")"
eq "通知 consumed" "consumed" "$(notif_status "$N5")"
eq "组 delivery_run_id = 新尝试的 run" "$NEW_RUN" "$(group_field "$OID" delivery_run_id)"
eq "Task 结果版本没被重跑改写（仍 1）" "1" "$(task_field "$TID" result_revision)"
eq "Task 当前执行 run 没换（未重跑 DAG）" "$EXEC_RUN5" "$(task_field "$TID" current_run_id)"
eq "来源会话多三场 run（占位 + 失败交付 + 重试）" "$((PRE_RUNS + 3))" "$(session_runs "$SRC")"

# ═══ 场景 6：用户停止 → 不自动重启 → 人工重试 ═════════════════════════════════
echo
echo "── 场景 6：停止交付 run → failed(delivery_stopped) → 不自动重启 → 人工重试"
stop_engine "$PID_A"
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
create_one stop "只回复一句话：正在整理交付。然后调用 submit_task_result，summary 写 ok。" || ml_summary
if wait_claimed "$OID" 300; then ok "交付 run 已认领（$CRID）"; else no "300s 没认领"; ml_summary; fi
http_as POST "/api/runs/$CRID/stop" "" "$TOK"
eq "停止请求成功" "0" "$(echo "$AT_BODY" | jq -r .code)"
N6=$NID
if wait_notif_status "$N6" failed 120; then ok "通知翻成 failed"; else no "120s 没 failed（$(notif_status "$N6")）"; fi
eq "机器原因 = delivery_stopped" "delivery_stopped" "$(notif_field "$N6" consume_error_code)"
eq "旧的交付 run 状态 stopped" "stopped" "$(run_status "$CRID")"
eq "组交付状态 failed" "failed" "$(group_field "$OID" delivery_status)"
eq "组 delivery_error_code = delivery_stopped" "delivery_stopped" "$(group_field "$OID" delivery_error_code)"
BEFORE_RUNS=$(session_runs "$SRC")
sleep 6 # 三轮调度周期：停止不得被自动重放
eq "没有自动重启：来源会话 run 数不变" "$BEFORE_RUNS" "$(session_runs "$SRC")"
eq "没有自动重启：处理尝试仍 1 行" "1" "$(attempt_rows "$N6")"
http_as GET "/api/notifications/$N6" "" "$TOK"
eq "详情结果产物为空（纯摘要可恢复）" "0" "$(echo "$AT_BODY" | jq -r '.data.result.artifacts | length')"
http_as POST "/api/notifications/$N6/retry" "" "$TOK"
eq "人工重试 202" "202" "$AT_CODE"
if at_wait_delivered "$OID" 300; then ok "重试后交付完成"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "处理尝试 2 行" "2" "$(attempt_rows "$N6")"
eq "来源会话多两场 run（停止 + 重试）" "$((PRE_RUNS + 2))" "$(session_runs "$SRC")"

# ═══ 场景 8：结果文件可打开；删除后 117110；补回后可恢复 ══════════════════════
echo
echo "── 场景 8：产物文件打开 / 删除后重试 117110 / 补回后可恢复"
ART_MARK="ART-73-$STAMP"
# 产物必须由任务在本次执行里新建（共享工作区的提交校验按基线 diff 判定「新增或修改」；
# 提前放进 content 的文件属于基线，声明成 artifact 会被 117110 前的校验拒绝）。
create_one art "在项目根创建文件 delivery-artifact.txt，内容写一行「$ART_MARK」。然后调用 submit_task_result，summary 写文件路径，artifacts 传 [{path:\"delivery-artifact.txt\",kind:\"report\"}]。" || ml_summary
if wait_claimed "$OID" 300; then ok "交付 run 已认领（$CRID）"; else no "300s 没认领"; ml_summary; fi
http_as POST "/api/runs/$CRID/stop" "" "$TOK"
N8=$NID
if wait_notif_status "$N8" failed 120; then ok "通知 failed（停掉交付，保留产物引用）"; else no "120s 没 failed"; fi
http_as GET "/api/notifications/$N8" "" "$TOK"
ART_PATH=$(echo "$AT_BODY" | jq -r '.data.result.artifacts[0].path // ""')
eq "详情给出产物路径" "delivery-artifact.txt" "$ART_PATH"
MEDIA=$(curl -sS -w '\n%{http_code}' "$B/api/media?path=workspaces/$WID/$ART_PATH" -H "Authorization: Bearer $TOK" 2>/dev/null)
MEDIA_CODE=${MEDIA##*$'\n'}; MEDIA_BODY=${MEDIA%$'\n'*}
eq "产物可打开（GET /api/media 200）" "200" "$MEDIA_CODE"
case "$MEDIA_BODY" in *"$ART_MARK"*) ok "打开的内容是任务声明的产物" ;; *) no "打开的内容不含标记" ;; esac
rm -f "$WS_DIR/$ART_PATH"
http_as POST "/api/notifications/$N8/retry" "" "$TOK"
eq "产物缺失时重试 409" "409" "$AT_CODE"
eq "业务码 117110" "117110" "$(echo "$AT_BODY" | jq -r .code)"
printf '%s\n' "$ART_MARK" > "$WS_DIR/$ART_PATH"
http_as POST "/api/notifications/$N8/retry" "" "$TOK"
eq "补回文件后重试 202" "202" "$AT_CODE"
if at_wait_delivered "$OID" 300; then ok "补回后交付完成"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "处理尝试 2 行" "2" "$(attempt_rows "$N8")"

# ═══ 场景 7：双副本并发重试只增一次 attempt ═══════════════════════════════════
echo
echo "── 场景 7：两台引擎同时收到重试，只许一个新尝试"
PID_B=$(start_engine "$PORT_B" "$ELOG_B" 2s)
create_one race "只回复 ok，然后调用 submit_task_result，summary 写 ok。" || ml_summary
if wait_claimed "$OID" 300; then ok "交付 run 已认领"; else no "300s 没认领"; ml_summary; fi
http_as POST "/api/runs/$CRID/stop" "" "$TOK"
N7=$NID
if wait_notif_status "$N7" failed 120; then ok "通知 failed，可以并发重试"; else no "120s 没 failed"; fi
RA="$SCRATCH/retry-a.json"; RB="$SCRATCH/retry-b.json"
BA="http://127.0.0.1:$PORT_A"; BB="http://127.0.0.1:$PORT_B"
curl -sS -o "$RA" -w '%{http_code}' -X POST "$BA/api/notifications/$N7/retry" -H "Authorization: Bearer $TOK" >"$SCRATCH/retry-a.code" &
PA=$!
curl -sS -o "$RB" -w '%{http_code}' -X POST "$BB/api/notifications/$N7/retry" -H "Authorization: Bearer $TOK" >"$SCRATCH/retry-b.code" &
PB=$!
wait "$PA" "$PB"
CA=$(cat "$SCRATCH/retry-a.code" 2>/dev/null); CB=$(cat "$SCRATCH/retry-b.code" 2>/dev/null)
SORTED=$(printf '%s\n%s\n' "$CA" "$CB" | sort | tr '\n' ' ')
eq "两个响应分别是 202 与 200（一个真开、一个读现状）" "200 202 " "$SORTED"
RA_ID=$(jq -r '.data.runId // ""' "$RA" 2>/dev/null); RB_ID=$(jq -r '.data.runId // ""' "$RB" 2>/dev/null)
eq "两个响应指向同一场 run" "$RA_ID" "$RB_ID"
eq "处理尝试只增一行（共 2）" "2" "$(attempt_rows "$N7")"
if [ "$CA" = "202" ]; then eq "202 的 attempt=2" "2" "$(jq -r '.data.attempt' "$RA")"; else eq "202 的 attempt=2" "2" "$(jq -r '.data.attempt' "$RB")"; fi
if at_wait_delivered "$OID" 300; then ok "并发重试后交付完成且只有一场"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "来源会话只多两场 run（停止 + 唯一重试）" "$((PRE_RUNS + 2))" "$(session_runs "$SRC")"
stop_engine "$PID_B"; PID_B=""

# ═══ 场景 2：claimed＋run 落库、还没有产出时 kill -9 → 同一场 run 续跑 ════════
echo
echo "── 场景 2：认领后立刻 kill -9 → 同一场 run 续跑（epoch+1），不铸新 run"
create_one crash2 "只回复 ok，然后调用 submit_task_result，summary 写 ok。" || ml_summary
if wait_claimed "$OID" 300; then ok "交付 run 已认领（$CRID）"; else no "300s 没认领"; ml_summary; fi
D2=$(delta_count "$CRID"); echo "  ℹ️ kill 前已见 delta 条数 = ${D2:-?}（0 表示 worker 还没产出）"
kill -9 "$PID_A" 2>/dev/null; PID_A=""
sleep 2
eq "崩溃后 run 仍挂 running（判死等租约）" "running" "$(run_status "$CRID")"
eq "通知仍 claimed、claim_run_id 不变" "claimed|$CRID" "$(notif_status "$NID")|$(notif_field "$NID" claim_run_id)"
eq "处理尝试仍 1 行" "1" "$(attempt_rows "$NID")"
if wait_lease_expire "$CRID"; then ok "租约已过期"; else no "租约一直没过期"; fi
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
http_as POST "/api/runs/$CRID/resume" "" "$TOK"
eq "resume 202" "202" "$AT_CODE"
eq "接的是同一场 run" "$CRID" "$(echo "$AT_BODY" | jq -r '.data.runId // ""')"
eq "同一场 run 的 lease_epoch = 2" "2" "$(run_epoch "$CRID")"
if at_wait_delivered "$OID" 300; then ok "续跑后交付完成"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "处理尝试仍 1 行（续跑不增 attempt）" "1" "$(attempt_rows "$NID")"
eq "尝试行仍指这场 run" "$CRID" "$(attempt_run "$NID" 1)"
eq "交付 run 终态 complete" "complete" "$(run_status "$CRID")"
eq "组 delivery_run_id 仍是这场 run" "$CRID" "$(group_field "$OID" delivery_run_id)"
eq "Task 结果版本不变（仍 1）" "1" "$(task_field "$TID" result_revision)"
eq "来源会话只多一场 run（同 run 续跑）" "$((PRE_RUNS + 1))" "$(session_runs "$SRC")"

# ═══ 场景 3：交付中（流已开始）kill -9 → 同一场 run 续跑 ═════════════════════
echo
echo "── 场景 3：流已开始后 kill -9 → 同一场 run 续跑 → 交付一次"
create_one crash3 "先只说一句「正在整理」。然后调用 submit_task_result，summary 写 ok。" || ml_summary
if wait_claimed "$OID" 300; then ok "交付 run 已认领（$CRID）"; else no "300s 没认领"; ml_summary; fi
if wait_delta "$CRID" 90; then ok "交付流已开始（有真实 delta）"; else no "90s 没看到 delta（故障窗口不成立，按未验记）"; fi
kill -9 "$PID_A" 2>/dev/null; PID_A=""
sleep 2
eq "崩溃后 run 仍挂 running" "running" "$(run_status "$CRID")"
if wait_lease_expire "$CRID"; then ok "租约已过期"; else no "租约一直没过期"; fi
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
http_as POST "/api/runs/$CRID/resume" "" "$TOK"
eq "resume 202" "202" "$AT_CODE"
eq "接的是同一场 run" "$CRID" "$(echo "$AT_BODY" | jq -r '.data.runId // ""')"
eq "lease_epoch = 2" "2" "$(run_epoch "$CRID")"
SEED=$(curl -sS "$B/api/runs/$CRID/frames?eventOffset=0" -H "Authorization: Bearer $TOK" 2>/dev/null |
  jq -r '[.data.frames[]? | select(.kind=="resume")] | length' 2>/dev/null)
eq "resume 帧已落 runlive" "1" "$SEED"
if at_wait_delivered "$OID" 300; then ok "续跑后交付完成"; else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "处理尝试仍 1 行" "1" "$(attempt_rows "$NID")"
eq "组 delivery_run_id 仍是这场 run" "$CRID" "$(group_field "$OID" delivery_run_id)"
eq "来源会话只多一场 run" "$((PRE_RUNS + 1))" "$(session_runs "$SRC")"

# ═══ 场景 10：唤醒丢失 → 只靠 PG 启动对账收敛 ═════════════════════════════════
echo
echo "── 场景 10：通知停在 pending、删掉唤醒流并杀进程，重启只靠 PG 对账收敛"
# 换一场干净来源会话：前面场景在 SRC 里留过 ask_user 指令，交付 run 会连同会话历史一起
# 读进去，可能照着历史再问一遍（交付工具面没有 ask_user → run error，测的就不是本场景了）。
at_probe_run "$WID"
SRC="$SID"
create_one wake "只回复 ok，然后调用 submit_task_result，summary 写 ok。" || ml_summary
# 占住来源会话：忙检查的唯一输入就是 runs 上的「status=running + 租约有效」这一行。
# 直接插一行、不给 worker（没人续约）——正是「用户那场聊天被 kill -9 打断后没人恢复」的形状；
# 不用模型占位，避免未完成的问题/工具调用留在会话历史里把交付 run 带偏。
BUSY10="r_busy10_$STAMP"
ml_sql "INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, created_at)
        VALUES ('$BUSY10', '$SRC', 'running', 1, now() + make_interval(secs => 60), now())" >/dev/null \
  || { no "插入忙 run 失败"; ml_summary; }
if at_wait_task "$TID" "succeeded" 300; then ok "最终 Task succeeded"; else no "300s 没 succeeded"; fi
N10=""
for _ in $(seq 1 120); do
  N10=$(notif_of "$OID" "orchestration_finished")
  [ -n "$N10" ] && [ "$(notif_status "$N10")" = "pending" ] && break
  sleep 0.5
done
if [ -n "$N10" ] && [ "$(notif_status "$N10")" = "pending" ]; then
  ok "通知停在 pending（等待来源会话空闲，认领还没发生）"
else no "通知没停在 pending（$(notif_status "$N10")）"; fi
eq "组已收口且 deliveryStatus=pending" "succeeded|pending" "$(group_field "$OID" status)|$(group_field "$OID" delivery_status)"
REDIS_DEL=$(redis_int "$REDIS_URL" DEL tk:agent-task-wake 2>/dev/null)
echo "  ℹ️ 删唤醒流：DEL tk:agent-task-wake -> ${REDIS_DEL:-?}"
kill -9 "$PID_A" 2>/dev/null; PID_A=""
sleep 2
# 忙 run 的租约仍在（没人续约），重启后的前几轮认领会继续被会话忙挡住；
# 租约过期后，调度周期那一轮按 AcquireRun 同一条扫尾把死行翻 error，再铸交付 run——
# 没有任何事件被消费，收敛只来自 PG 行状态与周期对账。
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
if at_wait_delivered "$OID" 300; then
  ok "重启后的 PG 对账完成交付（没有可消费的唤醒事件）"
else no "300s 没交付（$(group_field "$OID" delivery_status)）"; fi
eq "通知恰好 1 条" "1" "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE orchestration_id='$OID' AND kind='orchestration_finished'")"
eq "处理尝试恰好 1 行（不重复交付）" "1" "$(attempt_rows "$N10")"
eq "通知 consumed" "consumed" "$(notif_status "$N10")"
eq "组只有一场交付 run" "$(attempt_run "$N10" 1)" "$(group_field "$OID" delivery_run_id)"
RUN10=$(attempt_run "$N10" 1)

# ═══ 场景 4：run complete、通知未收尾 → 对账补交付，不新建 run ════════════════
echo
echo "── 场景 4：run 已 complete、通知还没收尾的崩溃窗口 → 启动对账补交付"
# 这个窗口在进程内只有「EndRun 落 PG」到「ReconcileDeliveryRun」之间的几微秒，无法用 kill 稳定命中；
# 按列的真实语义还原：run 是场景 10 真实跑完的那一场，只把通知与组退回收尾前的状态。
stop_engine "$PID_A"
"$PSQL" "$DSN" -c "UPDATE agent_notifications SET status='claimed', consumed_at=NULL, updated_at=now() WHERE id='$N10';" >/dev/null \
  || { no "还原通知窗口失败"; ml_summary; }
"$PSQL" "$DSN" -c "UPDATE agent_orchestrations SET delivery_status='delivering', delivery_run_id='$RUN10', updated_at=now() WHERE id='$OID';" >/dev/null \
  || { no "还原组窗口失败"; ml_summary; }
eq "还原后通知仍 claimed、run 已 complete" "claimed|complete" "$(notif_status "$N10")|$(run_status "$RUN10")"
BEFORE4=$(session_runs "$SRC")
PID_A=$(start_engine "$PORT_A" "$ELOG_A" 2s)
if at_wait_delivered "$OID" 120; then ok "启动对账把组补成 delivered"; else no "120s 没 delivered"; fi
eq "通知被对账收成 consumed" "consumed" "$(notif_status "$N10")"
eq "没有新建 run" "$BEFORE4" "$(session_runs "$SRC")"
eq "处理尝试仍 1 行" "1" "$(attempt_rows "$N10")"
eq "组 delivery_run_id 仍是原 run" "$RUN10" "$(group_field "$OID" delivery_run_id)"

# ═══ 场景 9：非属主 / viewer / 跨租户 ═════════════════════════════════════════
echo
echo "── 场景 9：非属主 / viewer / 跨租户的读与重试被拒"
http_as GET "/api/notifications/$N10" "" "$TOK"
eq "属主读 200" "200" "$AT_CODE"
NOAUTH=$(curl -sS -w '\n%{http_code}' "$B/api/notifications/$N10" 2>/dev/null)
eq "无 token 读 401" "401" "${NOAUTH##*$'\n'}"
http_as GET "/api/notifications/an_doesnotexist" "" "$TOK"
eq "未知通知 404" "404" "$AT_CODE"
eq "未知通知码 117003" "117003" "$(echo "$AT_BODY" | jq -r .code)"
STRANGER=$(curl -sS -X POST "$B/api/auth/signup" -H 'Content-Type: application/json' \
  -d '{"email":"stranger73_'"$STAMP"'@tako.test","password":"pw_'"$STAMP"'_73"}')
STOK=$(echo "$STRANGER" | jq -r .data.accessToken)
[ -n "$STOK" ] && [ "$STOK" != "null" ] || { no "跨租户账号注册失败"; STOK=""; }
if [ -n "$STOK" ]; then
  http_as GET "/api/notifications/$N10" "" "$STOK"
  eq "跨租户读 404" "404" "$AT_CODE"
  eq "跨租户读码 117003" "117003" "$(echo "$AT_BODY" | jq -r .code)"
fi
VEM="viewer73_$STAMP@tako.test"
INV=$(curl -sS -X POST "$B/api/tenants/$TEN/invitations" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"email":"'"$VEM"'","role":"viewer"}')
VTOKEN=$(echo "$INV" | jq -r '.data.inviteToken // ""')
[ -n "$VTOKEN" ] && [ "$VTOKEN" != "null" ] || { no "发 viewer 邀请失败：$INV"; VTOK=""; }
if [ -n "${VTOKEN:-}" ] && [ "$VTOKEN" != "null" ]; then
  curl -sS -o /dev/null -X POST "$B/api/auth/register-by-invite" -H 'Content-Type: application/json' \
    -d '{"token":"'"$VTOKEN"'","email":"'"$VEM"'","password":"changeme123"}'
  VLOGIN=$(curl -sS -X POST "$B/api/auth/login" -H 'Content-Type: application/json' \
    -d '{"identifier":"'"$VEM"'","password":"changeme123"}')
  VACCESS=$(echo "$VLOGIN" | jq -r .data.accessToken)
  VREFRESH=$(echo "$VLOGIN" | jq -r .data.refreshToken)
  VSW=$(curl -sS -X POST "$B/api/auth/switch-tenant" -H "Authorization: Bearer $VACCESS" -H 'Content-Type: application/json' \
    -d '{"tenantId":"'"$TEN"'","refreshToken":"'"$VREFRESH"'"}')
  VTOK=$(echo "$VSW" | jq -r .data.accessToken)
  if [ -n "$VTOK" ] && [ "$VTOK" != "null" ]; then
    http_as GET "/api/notifications/$N10" "" "$VTOK"
    eq "viewer 读 404（非属主）" "404" "$AT_CODE"
    http_as POST "/api/notifications/$N10/retry" "" "$VTOK"
    eq "viewer 重试 403" "403" "$AT_CODE"
    eq "viewer 重试码 111203" "111203" "$(echo "$AT_BODY" | jq -r .code)"
  else
    no "viewer 切租户失败：$VSW"
  fi
fi

ml_summary
