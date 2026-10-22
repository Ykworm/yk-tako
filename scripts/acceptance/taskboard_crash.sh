#!/usr/bin/env bash
# 任务看板活得过崩溃（doc/41 §5.1）的活体验收：kill -9 引擎 → 重启 → resume → 看板还在。
#
# 这条验的是**看板正本在 PG 表 run_tasks，不在进程内存**（旧线 doc/01 §3 的死因）。
# 断言有四条，缺一不可：
#   1. 崩前建的行，崩后一条没丢（按 id 集合比对——不是比行数：续跑 legitimately 会加新行）
#   2. resume 真的接管了同一场 run（run 行的 lease_epoch 从 1 变 2，status 回 complete）
#   3. 续跑在**同一张表**里继续建了行（看板是活的，不是张死表）
#   4. 续跑到 terminal 帧（帧流收口；拉帧必须跟着 nextOffset 走，见下）
# 另有一项**只作观察不作断言**：崩前那批行有没有被续跑的模型回头勾掉——那是模型行为，不稳定。
#
# 参数（环境变量，不写死本机路径与端口）：
#   TAKO_BASE            引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV             取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN          直连 PG 的 DSN（**必填**：行数比对靠它）
#   TAKO_PSQL            psql 可执行文件，默认 psql
#   TAKO_ENGINE_PIDS     要 kill -9 的引擎进程 PID（空格分隔，**必填**）
#   TAKO_ENGINE_RESTART  重启引擎的命令（**必填**，一行 shell 片段，后台起）
#   TAKO_LEASE_WAIT      等租约过期的秒数，默认 35（引擎 lease TTL 30s）
#
# 跑之前先起一个**隔离**引擎（别拿开发库当靶子），见同目录 README.md：
#   TAKO_ENGINE_PIDS=$(pgrep -f "yktako -engine 127.0.0.1:8943")
#   TAKO_ENGINE_RESTART='cd <仓库根>/yk-tako-eino && nohup go run ./cmd/yktako -engine 127.0.0.1:8943 ... &'
#   bash scripts/acceptance/taskboard_crash.sh
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8943}"
ENV="${TAKO_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-psql}"
DSN="${TAKO_PG_DSN:-}"
PIDS="${TAKO_ENGINE_PIDS:-}"
RESTART="${TAKO_ENGINE_RESTART:-}"
LEASE_WAIT="${TAKO_LEASE_WAIT:-35}"

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

# 拉全部帧：**必须跟着 nextOffset 走**（单批上限 W=200，terminal 帧排在第 200 帧之后——
# 一直用 eventOffset=0 就永远看不到收尾帧，19v6-04 用例 2 写的就是这条）。
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

echo "引擎 $B · 密钥取自 ${ENV} · 杀进程 [$PIDS]"

# ── 1. 注册账号 + 配租户模型（否则 POST /api/runs 回 111408）──────────────────
EM="crash_$(date +%s%N)@tako.test"
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

# ── 2. 投一场 8 步的 run（步数多才有时间窗 kill -9）──────────────────────────
RESP=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H "$J" \
  -d '{"messageId":"m_crash_1","text":"我要做八件事，请先建任务看板跟踪，每做完一件就更新状态：1 列出工作区文件 2 创建 a.txt 3 读 a.txt 4 创建 b.txt 5 读 b.txt 6 创建 c.txt 7 读 c.txt 8 把三份内容合起来写进 d.txt。"}')
RID=$(echo "$RESP" | jq -r .data.runId)
if [ -z "$RID" ] || [ "$RID" = "null" ]; then echo "投递失败：$RESP" >&2; exit 2; fi
echo "  runId = $RID"

# ── 3. 等到第一张 task 帧就 kill -9 ───────────────────────────────────────
KILLED=0
for i in $(seq 1 300); do
  fetch_frames
  NT=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="task")] | length' 2>/dev/null)
  if [ "${NT:-0}" -ge 1 ] 2>/dev/null; then
    echo "  第 $i 次轮询已有 $NT 张 task 帧 → kill -9"
    kill -9 $PIDS 2>/dev/null
    sleep 2
    KILLED=1
    break
  fi
  sleep 0.3
done
[ "$KILLED" = 1 ] || { no "没等到 task 帧（模型没建看板？）"; exit 1; }
ok "在看板建起来之后 kill -9 了引擎"

# ── 4. 崩前：行 id 集合 + 计数 + 最后一次改动时间 ─────────────────────────
BEFORE_IDS=$(sql "SELECT string_agg(id, ',' ORDER BY id) FROM run_tasks WHERE run_id='$RID'")
BEFORE_N=$(sql "SELECT count(*) FROM run_tasks WHERE run_id='$RID'")
BEFORE_STAMP=$(sql "SELECT max(updated_at) FROM run_tasks WHERE run_id='$RID'")
if [ -z "$BEFORE_N" ] || [ "$BEFORE_N" = "0" ]; then
  no "崩前 run_tasks 一行都没有（看板没落库？）"; exit 1
fi
ok "崩前 run_tasks 有 ${BEFORE_N} 行"

# ── 5. 等租约过期 → 重启引擎 → resume ────────────────────────────────────
echo "  等租约过期 ${LEASE_WAIT}s…"
sleep "$LEASE_WAIT"
eval "$RESTART" >/dev/null 2>&1
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

# ── 6. 轮询到 terminal ───────────────────────────────────────────────────
TERM=0
for i in $(seq 1 180); do
  fetch_frames
  K=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="terminal")] | length' 2>/dev/null)
  if [ "${K:-0}" -ge 1 ] 2>/dev/null; then TERM=1; break; fi
  sleep 1
done
[ "$TERM" = 1 ] && ok "续跑到 terminal 帧" || no "续跑没等到 terminal 帧"

# ── 7. 崩后：老行一条没丢 + lease_epoch 变 2 + 老行还能被改 ────────────────
AFTER_IDS=$(sql "SELECT string_agg(id, ',' ORDER BY id) FROM run_tasks WHERE run_id='$RID'")
AFTER_N=$(sql "SELECT count(*) FROM run_tasks WHERE run_id='$RID'")
LOST=$(python3 - "$BEFORE_IDS" "$AFTER_IDS" <<'PY'
import sys
b = set(filter(None, sys.argv[1].split(",")))
a = set(filter(None, sys.argv[2].split(",")))
print(len(b - a))
PY
)
eq "崩前的行一条没丢（丢 ${LOST} 条）" "0" "$LOST"
echo "  行数：崩前 ${BEFORE_N} → 崩后 ${AFTER_N}（续跑会加新行，属正常）"

EPOCH=$(sql "SELECT lease_epoch FROM runs WHERE id='$RID'")
if [ "${EPOCH:-0}" -ge 2 ] 2>/dev/null; then
  ok "resume 接管了同一场 run（lease_epoch=${EPOCH}）"
else
  no "lease_epoch 没涨（期望 ≥2，实际 ${EPOCH:-?}）"
fi
RST=$(sql "SELECT status FROM runs WHERE id='$RID'")
eq "这场 run 最终是 complete" "complete" "$RST"

# 老行在续跑里**还能被改**（不是一张死表）：崩前那批 id 里至少有一行的 updated_at 变了。
# ⚠️ 这一条**是模型行为**：续跑的模型不一定回头去勾崩前那几条（它可能直接建剩下的）。
# 2026-10-23 实跑两种都出现过（勾了 / 没勾）→ 只能作观察，不作硬断言（同 doc/19-v6-04 用例 8 的处置）。
# 「看板是活的」由下面那条硬断言兜：续跑在**同一张表**里继续建了行。
IDSQL=$(python3 - "$BEFORE_IDS" <<'PY'
import sys
ids = [i for i in sys.argv[1].split(",") if i]
print(",".join("'" + i + "'" for i in ids))
PY
)
TOUCHED=$(sql "SELECT count(*) FROM run_tasks WHERE id IN ($IDSQL) AND updated_at > '$BEFORE_STAMP'")
if [ "${TOUCHED:-0}" -ge 1 ] 2>/dev/null; then
  echo "  ℹ️ 崩前那批行在续跑里被继续更新（${TOUCHED} 行）——模型回头勾了旧任务（不作断言）"
else
  echo "  ℹ️ 崩前那批行续跑后没被动——模型没回头勾旧任务（不作断言，见上方说明）"
fi

# 看板是活的：续跑在**同一张表**里继续建了行（崩后行数 > 崩前行数）
if [ "${AFTER_N:-0}" -gt "${BEFORE_N:-0}" ] 2>/dev/null; then
  ok "续跑在同一张表里继续建了任务（${BEFORE_N} → ${AFTER_N} 行）"
else
  no "续跑没在同一张表里建新行（${BEFORE_N} → ${AFTER_N}）"
fi

echo
echo "通过 $P 条，失败 $F 条"
[ "$F" -eq 0 ] || exit 1
