#!/usr/bin/env bash
# 持久 Task 崩溃活体（doc/70 §10；施工包 §12.3 第 3 场）：A 执行中 kill -9 隔离引擎 →
# 重启后 Task 读侧派生 interrupted、C 不启动 → 显式 resume 接管同一代执行 →
# A 在**同一场 run**（epoch+1）里跑完 → C 恰好被执行一次 → 旧代交不上结果（result_epoch=新代）→ 交付一次。
#
# 与 taskboard_crash.sh 同一契约：kill 只打 TAKO_ENGINE_PIDS 指定的隔离实例；
# 重启命令必须带与被杀实例相同的 -pg / -redis / -workspace / TAKO_JWT_SECRET（与调度循环开着）。
#   TAKO_BASE            引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV             取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN          直连 PG 的 DSN（**必填**）
#   TAKO_PSQL            psql 可执行文件，默认 psql
#   TAKO_ENGINE_PIDS     要 kill -9 的引擎进程 PID（**必填**，只指隔离实例）
#   TAKO_ENGINE_RESTART  重启命令（**必填**，一行 shell 片段，后台起，调度循环要开着）
#   TAKO_LEASE_WAIT      等租约过期的秒数，默认 35（引擎 lease TTL 30s）
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn
PIDS="${TAKO_ENGINE_PIDS:-}"
RESTART="${TAKO_ENGINE_RESTART:-}"
LEASE_WAIT="${TAKO_LEASE_WAIT:-35}"
for v in PIDS RESTART; do
  eval "val=\$$v"
  [ -n "$val" ] || { echo "必填参数缺了：TAKO_${v^^}（见本文件头）" >&2; exit 2; }
done

echo "引擎 $B · 杀进程 [$PIDS] · 打 A 执行中崩溃 → resume → C 只跑一次的链"
ml_signup taskcrash

# 来源会话必须挂着项目（创建 Task 要求两者一致），所以先建项目、探针 run 带上它。
WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-crash-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"

WRITER="crash_writer_$(date +%s)_$$"
SPEC="你是持久任务的执行者。任务输入里会给出上游留下的证据文件路径。
用 read_text_file 读它，把原文写进任务输入指定的最终文件（write_file），
然后调用 submit_task_result，summary 写最终文件路径。"
jq -cn --arg n "$WRITER" --arg s "$SPEC" \
  '{name:$n,kind:"subagent",description:"崩溃活体的合成执行者",spec:$s}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @- >/dev/null

# ── 1. 两层 DAG：a 写证据（执行中会被杀），c 等父成功才轮到它 ─────────────────────
STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "在项目根创建文件 crash-evidence.txt，内容写一行「证据-$STAMP」。然后调用 submit_task_result，summary 写文件路径。" \
  --arg c "上游任务 a 留下一份证据文件（路径在其结果的 summary 里）。读它，把原文写进项目根的 crash-final.txt，然后调用 submit_task_result，summary 写最终文件路径。" \
  --arg w "$WRITER" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"c",content:$c,agent:$w}]')
RESP=$(at_create_orch "acc-crash-$STAMP" "$WID" "$SID" "崩溃后恢复并合成证据" "$TASKS" '[{"parent":"a","child":"c"}]')
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID" ] && [ "$OID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID"
TID_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TID_C=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
echo "  orchId=$OID a=$TID_A c=$TID_C"

# ── 2. 等 a 真的跑起来（currentRunId 写上、run 在租约内）再 kill -9 ───────────────
KILLED=0
for i in $(seq 1 60); do
  CUR=$(curl -sS "$B/api/tasks/$TID_A" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data.currentRunId // ""')
  if [ -n "$CUR" ] && [ "$CUR" != "null" ]; then
    LIVE=$(ml_sql "SELECT count(*) FROM runs WHERE id='$CUR' AND status='running' AND lease_until > now()")
    [ "$LIVE" = "1" ] && { KILLED=1; break; }
  fi
  sleep 1
done
[ "$KILLED" = 1 ] || { no "60s 没等到 a 领取并拿到活租约"; ml_summary; }
RUN_A="$CUR"
ok "a 已在执行（run $RUN_A，租约活着）→ kill -9"
kill -9 $PIDS 2>/dev/null
sleep 2

# ── 3. 崩后现场：a 的 run 仍挂 running、c 没被领走 ───────────────────────────────
eq "a 的 run 仍挂 running（判死等租约，不在引擎里）" "1" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE id='$RUN_A' AND status='running'")"
C_SESS=$(ml_sql "SELECT COALESCE(execution_session_id,'') FROM agent_tasks WHERE id='$TID_C'")
eq "c 没有执行会话（一个都没领走）" "0" \
  "$(ml_sql "SELECT count(*) FROM runs WHERE session_id IN (SELECT execution_session_id FROM agent_tasks WHERE id='$TID_C' AND execution_session_id IS NOT NULL)")"

# ── 4. 等租约过期 → 重启 → 读侧派生 interrupted ─────────────────────────────────
echo "  等租约过期 ${LEASE_WAIT}s…"
sleep "$LEASE_WAIT"
eval "$RESTART" >/dev/null 2>&1
for i in $(seq 1 40); do
  sleep 1
  curl -sS -m 2 -o /dev/null "$B/api/auth/login" 2>/dev/null && break
done
ST=""
for i in $(seq 1 20); do
  ST=$(at_task_status "$TID_A")
  [ "$ST" = "interrupted" ] && break
  sleep 1
done
eq "重启后 a 读侧派生 interrupted（存储仍 running）" "interrupted" "$ST"
eq "a 的 run 行没被翻态（resume 的前提）" "running" "$(ml_sql "SELECT status FROM runs WHERE id='$RUN_A'")"

# ── 5. 显式 resume：接管同一场 run（epoch+1），不铸新 run ────────────────────────
RES=$(curl -sS -X POST "$B/api/tasks/$TID_A/resume" -H "Authorization: Bearer $TOK")
eq "resume 成功（code 0）" "0" "$(echo "$RES" | jq -r .code)"
eq "接的是同一场 run" "$RUN_A" "$(echo "$RES" | jq -r '.data.runId // ""')"
RC=$(echo "$RES" | jq -r '.data.resumeCount // 0')
if [ "${RC:-0}" -ge 1 ] 2>/dev/null; then ok "resumeCount=$RC（接管第 2 代）"; else no "resumeCount 没涨（$RC）"; fi
eq "run 行 lease_epoch 涨到 2" "2" "$(ml_sql "SELECT lease_epoch FROM runs WHERE id='$RUN_A'")"
# 中断通知由「调度器 tick」或「接管事务」先到者写入，resume 之后才保证存在；
# dedup 保证两路只留一条，所以在这里查恰好 1 条。
eq "task_interrupted 通知恰好 1 条（tick/接管先到者写，dedup）" "1" "$(at_notif_count "$OID" "task_interrupted")"

# ── 6. 续跑收口：a succeeded → c 恰好被执行一次 → 交付一次 ───────────────────────
if at_wait_task "$TID_A" "succeeded" 300; then
  ok "a 续跑到 succeeded"
else no "300s 没跑到 succeeded（最后状态 $AT_LAST）"; ml_summary; fi
# 旧代写不进结果：被接受的结果必须出自当前代（resume 后 epoch=2）。
eq "a 的 result_epoch = 当前代（旧代提交无效的证据）" "2" \
  "$(ml_sql "SELECT result_epoch FROM agent_tasks WHERE id='$TID_A'")"
eq "a 的 result_run_id 指回同一场 run" "$RUN_A" \
  "$(ml_sql "SELECT result_run_id FROM agent_tasks WHERE id='$TID_A'")"

if at_wait_task "$TID_C" "succeeded" 300; then
  ok "c 在父成功后被领取并跑完"
else no "c 没跑到 succeeded（最后状态 $AT_LAST）"; ml_summary; fi
C_RUNS=$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID_C')")
eq "c 恰好一场执行 run（崩溃没有让它重影）" "1" "$C_RUNS"
FINAL="$WS_ROOT/tenants/$TEN/workspaces/$WID/crash-final.txt"
if [ -f "$FINAL" ] && grep -q "证据-$STAMP" "$FINAL"; then
  ok "crash-final.txt 带着崩溃前一代的证据字节"
else no "crash-final.txt 缺证据字节（$FINAL）"; fi

if at_wait_delivered "$OID" 180; then
  ok "来源会话交付完成"
else no "180s 没等到交付"; ml_summary; fi
eq "orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID" "orchestration_finished")"
eq "task_interrupted 通知恰好 1 条（去重）" "1" "$(at_notif_count "$OID" "task_interrupted")"
# §9.1：task_interrupted 与 orchestration_finished 都向来源会话投递——探针 + 两场交付 = 3。
eq "来源会话恰好三场 run（探针 + 中断交付 + 最终交付）" "3" "$(at_session_runs "$SID")"

ml_summary
