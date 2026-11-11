#!/usr/bin/env bash
# 固定 DAG 活体（doc/70；施工包 §12.3 第 2 场）：真建一份 writer 执行者规格 →
# A/B 两件 researcher 并行各写一份带预定证据的文件并交结果 → C writer 读上游两份结果
# 把证据合成 final 文件 → 组收口 → 来源会话交付恰好一次。
# 断言五条：A/B/C 全 succeeded；C 的 started_at 不早于两个父任务的 finished_at（依赖序）；
# final.txt 的字节里两份证据都在（上游结果真的流进了下游输入）；交付只一次；组里 3 节点 2 边。
#
# 起法同 agent_task_single_live.sh 头注（开着调度循环的隔离引擎 + TAKO_PG_DSN）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打固定 DAG（A/B 并行 → C 汇合）的创建→依赖序→合成→交付链"
ml_signup taskdag

# 来源会话必须挂着项目（创建 Task 要求两者一致），所以先建项目、探针 run 带上它。
WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"task-dag-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"

# ── 1. 真建一份 writer 执行者规格（租户自定义子代理，工具策略 inherit）────────────
WRITER="dag_writer_$(date +%s)_$$"
SPEC="你是持久任务的执行者。任务输入里会给出上游任务留下的证据文件路径。
用 read_text_file 读齐所有证据文件，把每份证据的原文完整合并写进任务输入指定的最终文件
（write_file，一段不少），然后调用 submit_task_result：
summary 写最终文件路径，metadata 里写 evidence 列出你读到的证据文件。
只读取上游结果和授权项目文件；不要编造证据内容。"
ARES=$(jq -cn --arg n "$WRITER" --arg s "$SPEC" \
  '{name:$n,kind:"subagent",description:"DAG 活体的合成执行者",spec:$s}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @-)
eq "建 writer 规格成功（201）" "0" "$(echo "$ARES" | jq -r .code)"
eq "规格可当执行者解析（name 回读一致）" "$WRITER" "$(echo "$ARES" | jq -r '.data.agent.name // ""')"

# ── 2. 创建固定 DAG：a/b 无父即 ready，c 依赖 a+b 落 pending ─────────────────────
STAMP="$(date +%s)-$$"
KEY="acc-dag-$STAMP"
TASKS=$(jq -cn --arg a "在项目根创建文件 evidence-a.txt，内容写一行「证据A-$STAMP」。然后调用 submit_task_result，summary 里写这个文件路径。" \
  --arg b "在项目根创建文件 evidence-b.txt，内容写一行「证据B-$STAMP」。然后调用 submit_task_result，summary 里写这个文件路径。" \
  --arg c "上游任务 a 与 b 各留下一份证据文件（路径在他们结果的 summary 里）。读齐两份证据，把两份原文都写进项目根的 final.txt（一段不少），然后调用 submit_task_result，summary 写 final.txt 的路径。" \
  --arg w "$WRITER" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"},{key:"c",content:$c,agent:$w}]')
LINKS='[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]'
RESP=$(at_create_orch "$KEY" "$WID" "$SID" "把两份证据合成 final.txt" "$TASKS" "$LINKS")
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
if [ -z "$OID" ] || [ "$OID" = "null" ]; then echo "创建失败：$RESP" >&2; exit 2; fi
echo "  orchId=$OID"
eq "创建回 202 且组状态 active" "active" "$(echo "$RESP" | jq -r '.data.status')"

at_orch "$OID"
eq "组里 3 个节点" "3" "$(echo "$AT_ORCH" | jq '.tasks | length')"
eq "2 条依赖边" "2" "$(echo "$AT_ORCH" | jq '.links | length')"
# 无父节点创建即就绪；调度循环够快时读到的是已被领取的 running，两种都算「没被挡在 pending」。
for k in a b; do
  st=$(echo "$AT_ORCH" | jq -r --arg k "$k" '.tasks[] | select(.key==$k) | .status')
  if [ "$st" = "ready" ] || [ "$st" = "running" ]; then
    ok "$k 创建即就绪（ready/running，无父）"
  else
    no "$k 创建即就绪（ready/running，无父） —— 实际 [$st]"
  fi
done
eq "c 父未成功前是 pending" "pending" "$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .status')"
TID_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TID_B=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
TID_C=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
eq "finalTaskId 是汇点 c" "$TID_C" "$(at_orch_field "$OID" "finalTaskId")"

# ── 3. 等整组收口：A/B 领取执行 → 成功后 C 才被提为 ready 并领取 ─────────────────
if at_wait_orch "$OID" "succeeded" 480; then
  ok "整组收口 succeeded"
else no "480s 没收口（组状态 $AT_LAST）"; at_orch "$OID"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
at_orch "$OID"
eq "a succeeded" "succeeded" "$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .status')"
eq "b succeeded" "succeeded" "$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .status')"
eq "c succeeded" "succeeded" "$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .status')"

# 依赖序（DB 时间戳）：c 的领取不早于两个父任务的收尾——C 不可能在依赖满足前开跑。
ORDER=$(ml_sql "SELECT count(*) FROM agent_tasks c JOIN agent_tasks a ON a.id='$TID_A'
  JOIN agent_tasks b ON b.id='$TID_B'
 WHERE c.id='$TID_C' AND c.started_at IS NOT NULL
   AND c.started_at >= greatest(a.finished_at, b.finished_at)")
eq "c.started_at ≥ 两个父任务的 finished_at（依赖序）" "1" "$ORDER"

# ── 4. 产物字节：final.txt 里两份证据都在（上游结果真的流进了下游输入）────────────
PROJ="$WS_ROOT/tenants/$TEN/workspaces/$WID"
FINAL="$PROJ/final.txt"
if [ -f "$FINAL" ]; then
  ok "final.txt 落盘"
  grep -q "证据A-$STAMP" "$FINAL" && ok "证据 A 的预定字节进了 final.txt" \
    || no "final.txt 里没有证据 A（合成丢了上游）"
  grep -q "证据B-$STAMP" "$FINAL" && ok "证据 B 的预定字节进了 final.txt" \
    || no "final.txt 里没有证据 B（合成丢了上游）"
else no "final.txt 不在项目根（$FINAL）——C 没写出产物"; fi

# ── 5. 交付恰好一次 ──────────────────────────────────────────────────────────────
if at_wait_delivered "$OID" 180; then
  ok "来源会话交付完成"
else no "180s 没等到交付（deliveryStatus=$(at_orch_field "$OID" deliveryStatus)）"; ml_summary; fi
DRV=$(at_orch_field "$OID" "deliveryRunId")
[ -n "$DRV" ] && ok "交付 run 铸了（$DRV）" || no "deliveryRunId 是空"
eq "orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID" "orchestration_finished")"
eq "且已消费" "1" "$(at_notif_count "$OID" "orchestration_finished" "consumed")"
eq "来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"

ml_summary
