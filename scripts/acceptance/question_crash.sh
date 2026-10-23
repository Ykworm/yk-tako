#!/usr/bin/env bash
# 等人的现场活得过崩溃（doc/47 §3.3 Q4）的活体验收：ask_user 挂起中 kill -9 → 重启 →
# resume → 重发 question 帧 → 人答题 → 答案注回 tool_result → run 继续到 terminal。
#
# 这条验的是**等待现场在 PG 不在进程**：journal 有 intent、run_questions 有 pending 行、
# resume 的第三分支把两者对上；进程里的等待 goroutine 只是可重建的临时雇员。
# 断言覆盖（缺一不可）：
#   1. 崩前 question 帧已发、行 pending 且 tool_call_id 非空（否则 resume 对不上 intent）
#   2. resume 接管同一场 run（lease_epoch 从 1 变 2）
#   3. resume 后**重发了 question 帧**（runlive 已被 seedResume 清空，这条帧只能是第三分支发的）
#   4. 答题前该行仍是 pending（挂起现场被原样恢复，不是被当成中断），且 list?status=pending 能见到它
#   5. answer 后 tool_result ask_user 的正文**逐字等于答案**（答案确实注回了模型输入）
#   6. 一答终答：对同一行再答一次回 409 `115101`
#   7. run 收到 terminal 帧且库里 status=complete、行 answered
#
# 参数（环境变量，不写死本机路径与端口）：
#   TAKO_BASE            引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV             取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN          直连 PG 的 DSN（**必填**：行状态靠它）
#   TAKO_PSQL            psql 可执行文件，默认 psql
#   TAKO_ENGINE_PIDS     要 kill -9 的引擎进程 PID（空格分隔，**必填**）
#   TAKO_ENGINE_RESTART  重启引擎的命令（**必填**，一行 shell 片段，后台起）
#   TAKO_LEASE_WAIT      等租约过期的秒数，默认 35（引擎 lease TTL 30s）
#
# 跑之前先起一个**隔离**引擎（别拿开发库当靶子），见同目录 README.md：
#   TAKO_ENGINE_PIDS=$(pgrep -f "yktako -engine 127.0.0.1:8943")
#   TAKO_ENGINE_RESTART='cd <仓库根>/yk-tako-eino && nohup go run ./cmd/yktako -engine 127.0.0.1:8943 ... &'
#   bash scripts/acceptance/question_crash.sh
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8943}"
ENV="${TAKO_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-psql}"
DSN="${TAKO_PG_DSN:-}"
PIDS="${TAKO_ENGINE_PIDS:-}"
RESTART="${TAKO_ENGINE_RESTART:-}"
LEASE_WAIT="${TAKO_LEASE_WAIT:-35}"
ANSWER="crash-beta.txt"

P=0; F=0
ok(){ echo "  ✅ $1"; P=$((P+1)); }
no(){ echo "  ❌ $1"; F=$((F+1)); }
eq(){ if [ "$2" = "$3" ]; then ok "$1"; else no "$1 —— 期望 [$2] 实际 [$3]"; fi; }
J="Content-Type: application/json"

for v in DSN PIDS RESTART; do
  eval "val=\$$v"
  if [ -z "$val" ]; then echo "必填参数缺了：TAKO_${v^^}（见本文件头）" >&2; exit 2; fi
done
KEY=$(grep '^DEEPSEEK_API_KEY=' "$ENV" | head -1 | cut -d= -f2- | tr -d '"')
if [ -z "$KEY" ]; then echo "读不到模型密钥：${ENV}（用 TAKO_ENV 指定）" >&2; exit 2; fi

sql(){ "$PSQL" "$DSN" -tAc "$1" 2>/dev/null | tr -d ' '; }
sqlraw(){ "$PSQL" "$DSN" -tAc "$1" 2>/dev/null; }

# 拉全部帧：跟着 nextOffset 走（单批上限 W=200，terminal 可能排在后批）。
fetch_frames(){
  local off=0 body nxt
  FRAMES=""
  while :; do
    body=$(curl -sS "$B/api/runs/$RID/frames?eventOffset=$off" -H "Authorization: Bearer $TOK" 2>/dev/null)
    FRAMES="$FRAMES$body
"
    nxt=$(printf '%s' "$body" | jq -r '.data.nextOffset // 0' 2>/dev/null)
    [ -z "$nxt" ] && nxt=0
    [ "$nxt" = "$off" ] && break
    off="$nxt"
  done
}

# frame_field <jq 过滤>：在当前 FRAMES 的**所有批次**里取字段（-s 把多份响应拼成一个流，
# 过滤用 `.[].data.frames[]?`；忘了 -s 会变成「每批各算一次」）。
frame_field(){
  printf '%s' "$FRAMES" | jq -s -r "$1" 2>/dev/null
}

echo "引擎 $B · 密钥取自 ${ENV} · 杀进程 [$PIDS]"

# ── 1. 注册账号 + 配租户模型（否则 POST /api/runs 回 111408）──────────────────
EM="qcrash_$(date +%s%N)@tako.test"
SIGNUP=$(curl -sS -X POST "$B/api/auth/signup" -H "$J" \
  -d '{"email":"'"$EM"'","password":"changeme123"}')
TOK=$(echo "$SIGNUP" | jq -r .data.accessToken)
TEN=$(echo "$SIGNUP" | jq -r .data.tenant.id)
if [ -z "$TOK" ] || [ "$TOK" = "null" ] || [ -z "$TEN" ] || [ "$TEN" = "null" ]; then
  echo "注册失败：$SIGNUP" >&2; exit 2
fi
curl -sS -o /dev/null -X PUT "$B/api/tenants/$TEN/runtime-config" -H "Authorization: Bearer $TOK" -H "$J" \
  -d '{"modelRef":"deepseek/deepseek-flash","mcpServers":{},"secrets":{"DEEPSEEK_API_KEY":"'"$KEY"'"},"sandbox":{"network":false}}'
ok "注册账号并配好租户模型"

# ── 2. 投一场必先问人的 run ────────────────────────────────────────────────
RESP=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H "$J" \
  -d '{"messageId":"m_qcrash_1","text":"第一步必须调用 ask_user 工具问我：要创建哪个文件？options 给 [\"crash-alpha.txt\", \"crash-beta.txt\"]。拿到我的回答后，用 write_file 创建我选的那个文件，内容写 chosen，然后直接结束。"}')
RID=$(echo "$RESP" | jq -r .data.runId)
if [ -z "$RID" ] || [ "$RID" = "null" ]; then echo "投递失败：$RESP" >&2; exit 2; fi
echo "  runId = $RID"

# ── 3. 等到第一张 question 帧 → 记行 id/args → kill -9 ──────────────────────
QID=""; KILLED=0
for i in $(seq 1 300); do
  fetch_frames
  QID=$(frame_field '[.[].data.frames[]? | select(.kind=="question") | .messageId] | first // empty' | head -1)
  if [ -n "$QID" ] && [ "$QID" != "null" ]; then
    echo "  第 $i 次轮询已见 question 帧（行 $QID）→ kill -9"
    kill -9 $PIDS 2>/dev/null
    sleep 2
    KILLED=1
    break
  fi
  sleep 0.3
done
[ "$KILLED" = 1 ] || { no "没等到 question 帧（模型没调 ask_user？）"; exit 1; }
ok "在 ask_user 挂起中 kill -9 了引擎"

QSTATUS=$(sql "SELECT status FROM run_questions WHERE id='$QID'")
QTCID=$(sql "SELECT tool_call_id FROM run_questions WHERE id='$QID'")
eq "崩前行仍是 pending" "pending" "$QSTATUS"
[ -n "$QTCID" ] && ok "崩前行带 tool_call_id（resume 能对到 intent）" || no "崩前行 tool_call_id 为空（resume 对不上）"

# ── 4. 等租约过期 → 重启引擎 → resume ────────────────────────────────────
echo "  等租约过期 ${LEASE_WAIT}s…"
sleep "$LEASE_WAIT"
# 用子 shell 关住重定向：RESTART 里以 `&` 结尾时，后台任务会把重定向带进本脚本，
# 让 stdout 永久指向 /dev/null、外层 `| tail` 等不到 EOF（2026-10-23 实踩）。
( eval "$RESTART" ) </dev/null >/dev/null 2>&1
for i in $(seq 1 40); do
  sleep 1
  if curl -sS -m 2 -o /dev/null "$B/api/auth/login" 2>/dev/null; then break; fi
done
RES=""
for i in $(seq 1 20); do
  RES=$(curl -sS -X POST "$B/api/runs/$RID/resume" -H "Authorization: Bearer $TOK")
  RC=$(echo "$RES" | jq -r .code)
  if [ "$RC" = "0" ]; then break; fi
  sleep 2
done
if [ "$(echo "$RES" | jq -r .code)" = "0" ]; then
  ok "resume 接管成功：$(echo "$RES" | jq -c .data)"
else
  no "resume 没接上：$RES"; exit 1
fi

EPOCH=$(sql "SELECT lease_epoch FROM runs WHERE id='$RID'")
if [ "${EPOCH:-0}" -ge 2 ] 2>/dev/null; then
  ok "resume 接管了同一场 run（lease_epoch=${EPOCH}）"
else
  no "lease_epoch 没涨（期望 ≥2，实际 ${EPOCH:-?}）"
fi

# ── 5. resume 后重发了 question 帧；答题前行仍 pending ─────────────────────
REEMIT=0
for i in $(seq 1 60); do
  fetch_frames
  NQ=$(frame_field '[.[].data.frames[]? | select(.kind=="question")] | length')
  if [ "${NQ:-0}" -ge 1 ] 2>/dev/null; then
    REEMIT=1
    break
  fi
  sleep 0.5
done
if [ "$REEMIT" = 1 ]; then
  ok "resume 后重发了 question 帧（第三分支重新挂起）"
else
  no "resume 后没有 question 帧（runlive 里只剩 resume 帧）"
fi
REQID=$(frame_field '[.[].data.frames[]? | select(.kind=="question") | .messageId] | first // empty' | head -1)
eq "重发的帧指回同一行" "$QID" "$REQID"
QSTATUS=$(sql "SELECT status FROM run_questions WHERE id='$QID'")
eq "答题前行仍是 pending" "pending" "$QSTATUS"
PLIST=$(curl -sS "$B/api/runs/$RID/questions?status=pending" -H "Authorization: Bearer $TOK" \
  | jq -r --arg q "$QID" '[.data[]? | select(.questionId==$q)] | length')
eq "list?status=pending 见到这一行" "1" "$PLIST"

# ── 6. 答题 → 答案逐字注回 tool_result → terminal ───────────────────────────
ANS=$(curl -sS -X POST "$B/api/questions/$QID/answer" -H "Authorization: Bearer $TOK" -H "$J" \
  -d '{"answer":"'"$ANSWER"'"}')
eq "answer 端点成功" "0" "$(echo "$ANS" | jq -r .code)"
ANS2=$(curl -sS -X POST "$B/api/questions/$QID/answer" -H "Authorization: Bearer $TOK" -H "$J" \
  -d '{"answer":"再来一次"}')
eq "一答终答：再答回 409 115101" "115101" "$(echo "$ANS2" | jq -r .code)"

TERM=0
for i in $(seq 1 180); do
  fetch_frames
  K=$(frame_field '[.[].data.frames[]? | select(.kind=="terminal")] | length')
  if [ "${K:-0}" -ge 1 ] 2>/dev/null; then TERM=1; break; fi
  sleep 1
done
[ "$TERM" = 1 ] && ok "续跑到 terminal 帧" || no "续跑没等到 terminal 帧"

# 注回的是**模型输入与 runlog 投影**，不是一条实时 tool_result 帧：续跑时 ask_user 没有
# 真的再执行一遍（at-most-once），它的结果由 replay 换进 turn；EndRun 才把它投影进 runlog。
# terminal 帧比 DB 提交早一拍，所以轮询等行出现。
TOOLOUT=""
for i in $(seq 1 20); do
  TOOLOUT=$(sqlraw "SELECT text FROM runlog_entries WHERE run_id='$RID' AND role='tool_result' AND payload::jsonb->>'name'='ask_user' ORDER BY seq LIMIT 1")
  [ -n "$TOOLOUT" ] && break
  sleep 0.5
done
eq "runlog 里 ask_user 的结果逐字等于答案（答案由第三分支注回）" "$ANSWER" "$TOOLOUT"

RST=$(sql "SELECT status FROM runs WHERE id='$RID'")
eq "这场 run 最终是 complete" "complete" "$RST"
QSTATUS=$(sql "SELECT status FROM run_questions WHERE id='$QID'")
eq "行最终是 answered" "answered" "$QSTATUS"

echo
echo "通过 $P 条，失败 $F 条"
[ "$F" -eq 0 ] || exit 1
