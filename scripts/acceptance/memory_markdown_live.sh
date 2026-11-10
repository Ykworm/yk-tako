#!/usr/bin/env bash
# markdown 模式活体（doc/65 §13.3-3）：**无 broker**，真实对话写 USER.md / MEMORY.md，
# 改口、删除后下一场读到新快照，memory_jobs 全程为零。
#
# 断言分两类：
#   系统字节（必须过）：模式门 409/118001、文件真实字节、memory_jobs=0、dev 观测口与文件一致；
#   模型行为（本脚本就是判红器）：模型按指示调 memory 工具、下一场答出当前记忆。
#
# 靶子：预起的**隔离 markdown 引擎**（TAKO_MEMORY_MODE=markdown，不起消费者、不连 broker），
#       起法见同目录 README.md。别拿开发实例当靶子。
# 参数：TAKO_BASE / TAKO_ENV / TAKO_PG_DSN（必填）/ TAKO_PSQL / TAKO_WS_ROOT（必填，= 引擎 -workspace）
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 密钥取自 ${ENV} · workspace ${WS_ROOT}"
ml_signup markdown

# ── 0. 模式门：items/jobs/recall 在 markdown 模式一律 409/118001，不能回空数组假装没记忆 ──
for ep in 'items' 'jobs' 'recall?q=%E8%8C%B6'; do
  resp=$(curl -sS -w '\n%{http_code}' "$B/api/memory/$ep" -H "Authorization: Bearer $TOK")
  code=$(echo "$resp" | tail -1); body=$(echo "$resp" | sed '$d')
  eq "GET /api/memory/$ep → HTTP 409" "409" "$code"
  eq "  业务码 118001" "118001" "$(echo "$body" | jq -r .code)"
  eq "  文案固定" "当前记忆模式不提供这个观测口" "$(echo "$body" | jq -r .message)"
  eq "  data=null" "null" "$(echo "$body" | jq -c .data)"
done
resp=$(curl -sS -w '\n%{http_code}' "$B/api/memory/dialogue?q=%E8%8C%B6" -H "Authorization: Bearer $TOK")
eq "dialogue 两模式都保留 → HTTP 200" "200" "$(echo "$resp" | tail -1)"

# ── 1. 记住（user 层）：真实对话 → USER.md 真实字节 ─────────────────────────────
ml_run '请用 memory 工具记住：我喜欢喝茶。target 用 user。做完只回复 ok。'
ml_wait_terminal 120 || { no "run1 没到 terminal"; ml_summary; }
ml_wait_settled || no "run1 会话没落定"
UF="$WS_ROOT/tenants/$TEN/users/$ML_UID/USER.md"
if [ -f "$UF" ] && grep -q '茶' "$UF"; then ok "USER.md 落盘且含「茶」：$(cat "$UF")"; else no "USER.md 没写出来（$( [ -f "$UF" ] && cat "$UF" || echo 不存在)）"; fi
eq "memory_jobs 为零（markdown 不登记任务）" "0" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE tenant_id='$TEN'")"
# dev 只读观测口与文件字节一致
body=$(curl -sS "$B/api/dev/memory-file?target=user" -H "Authorization: Bearer $TOK")
eq "dev memory-file 原文与盘上字节一致" "$(cat "$UF" 2>/dev/null)" "$(echo "$body" | jq -r .data.content)"
eq "dev memory-file cap=1375" "1375" "$(echo "$body" | jq -r .data.cap)"
eq "dev memory-file usageKnown=true" "true" "$(echo "$body" | jq -r .data.usageKnown)"

# ── 2. workspace 层：建项目 → MEMORY.md ────────────────────────────────────────
WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"记忆活体项目"}' | jq -r .data.workspace.id)
if [ -z "$WID" ] || [ "$WID" = "null" ]; then no "建项目失败"; ml_summary; fi
ml_run '请用 memory 工具记住：这个项目的部署端口是 8943。target 用 workspace。做完只回复 ok。' "$WID"
ml_wait_terminal 120 || no "run2 没到 terminal"
ml_wait_settled || no "run2 会话没落定"
MF="$WS_ROOT/tenants/$TEN/workspaces/$WID/MEMORY.md"
if [ -f "$MF" ] && grep -q '8943' "$MF"; then ok "MEMORY.md 落盘且含「8943」：$(cat "$MF")"; else no "MEMORY.md 没写出来"; fi
body=$(curl -sS "$B/api/dev/memory-file?target=workspace&workspaceId=$WID" -H "Authorization: Bearer $TOK")
eq "workspace 观测口 cap=2200" "2200" "$(echo "$body" | jq -r .data.cap)"

# ── 3. 改口：replace 后文件里只有新口 ──────────────────────────────────────────
ml_run '我改口了：不喜欢喝茶了，现在喜欢喝咖啡。请用 memory 工具把「我喜欢喝茶」那条改成「我喜欢喝咖啡」（target=user，action=replace）。做完只回复 ok。'
ml_wait_terminal 120 || no "run3 没到 terminal"
ml_wait_settled || no "run3 会话没落定"
if grep -q '咖啡' "$UF" 2>/dev/null && ! grep -q '茶' "$UF" 2>/dev/null; then
  ok "改口后 USER.md 只有新口：$(cat "$UF")"
else
  no "改口后 USER.md 字节不对：$(cat "$UF" 2>/dev/null)"
fi

# ── 4. 下一场读新快照：新会话问偏好，答案应是咖啡（快照注入的模型侧判红） ────────
ml_run '我喜欢喝什么？只回答饮品名，不要解释。'
ml_wait_terminal 120 || no "run4 没到 terminal"
ml_wait_settled || no "run4 会话没落定"
ml_frames
ans=$(ml_answer)
if echo "$ans" | grep -q '咖啡'; then ok "下一场答出新快照（咖啡）：$ans"; else no "下一场没答出新快照：$ans"; fi

# ── 5. 删除：remove 后文件里不再有这条 ─────────────────────────────────────────
ml_run '请用 memory 工具删掉「我喜欢喝咖啡」那条记忆（target=user，action=remove）。做完只回复 ok。'
ml_wait_terminal 120 || no "run5 没到 terminal"
ml_wait_settled || no "run5 会话没落定"
if ! grep -q '咖啡' "$UF" 2>/dev/null; then ok "删除后 USER.md 不含「咖啡」（字节：$(cat "$UF" 2>/dev/null)）"; else no "删除后 USER.md 还含「咖啡」"; fi
body=$(curl -sS "$B/api/dev/memory-file?target=user" -H "Authorization: Bearer $TOK")
eq "删除后 entryCount=0" "0" "$(echo "$body" | jq -r .data.entryCount)"

# ── 6. 全程无任务、无 broker 依赖 ──────────────────────────────────────────────
eq "memory_jobs 全程为零" "0" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE tenant_id='$TEN'")"
eq "memory_items 无行（markdown 不写 PG 记忆表）" "0" "$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN'")"

ml_summary