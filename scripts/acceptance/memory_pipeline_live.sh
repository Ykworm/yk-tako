#!/usr/bin/env bash
# pipeline 模式记忆活体（doc/65 §13.3-1）：真实 Redpanda broker + 真模型，覆盖
# 记住 → 改口 → 删除 → 每回合召回，以及 inrun 受理（工具回执）与 postrun 收拢（EndRun 登记）。
#
# 靶子：**预起的隔离引擎**（pipeline 模式 + broker 环境 + 真模型），别拿开发实例当靶子。起法：
#   cd yk-tako-eino && TAKO_MEMORY_MODE=pipeline \
#     TAKO_REDPANDA_BROKERS=<broker地址> TAKO_REDPANDA_USER=<用户> TAKO_REDPANDA_PASSWORD=<密码> \
#     TAKO_REDPANDA_MECH=SCRAM-SHA-256 \
#     go run ./cmd/yktako -engine 127.0.0.1:8943 -pg <隔离库DSN> \
#       -redis redis://127.0.0.1:6390/10 -workspace <隔离目录>
# 参数：TAKO_BASE / TAKO_ENV / TAKO_PG_DSN（必填）/ TAKO_PSQL。
# broker 不可达时明确报「缺 broker，未跑」并退出非零——不静默跳过当通过。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_broker_check
ml_need_dsn

echo "引擎 $B · broker ${TAKO_REDPANDA_BROKERS:-?} · 密钥取自 ${ENV}"
ml_signup pipe

# 模式前置：pipeline 模式下 items 观测口应可用（markdown 才回 409/118001）。
eq "观测口可用（pipeline 模式）" "0" "$(curl -sS "$B/api/memory/items?limit=1" -H "Authorization: Bearer $TOK" | jq -r .code)"

wait_all(){ # 一场 run 收口：terminal + 会话落定 + 本租户记忆任务无 pending/running
  ml_wait_terminal 120 || { no "run 没到 terminal"; ml_summary; }
  ml_wait_settled || no "会话没落定"
  ml_wait_jobs 90 || no "记忆任务 90s 没收口"
}
active_rows(){ # $1=内容词 → active recall/user 行数（抽取行落 recall 层；resident 是笔记线）
  ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%${1}%'"
}
recall_hits(){ # $1=查询词 → memoryHits
  curl -sS "$B/api/memory/recall?q=$1" -H "Authorization: Bearer $TOK" | jq -r '.data.memoryHits'
}

# ── 1. 记住：inrun 受理（工具回执）+ inrun/postrun 两笔任务都收口 + 行落库 ──────────
# pipeline 工具的方言是 targetLayer=resident|recall（markdown 才是 target=user|workspace）。
ml_run '请用 memory 工具把这条记下来：我喜欢喝茶。targetLayer 用 recall，quote 用我的原话。做完只回复 ok。'
ml_wait_terminal 120 || { no "run1 没到 terminal"; ml_summary; }
ml_frames
if printf '%s' "$FRAMES" | grep -q '已受理'; then
  ok "memory 工具当场回执「已受理」（inrun 受理）"
else no "没看到「已受理」回执"; fi
ml_wait_settled; ml_wait_jobs 90 || no "run1 的记忆任务 90s 没收口"
if [ "$(active_rows 茶)" -ge 1 ] 2>/dev/null; then
  ok "抽取器落了 recall/user 行（含「茶」，active）"
else no "没找到含「茶」的 active recall 行"; fi
eq "同一事实只留一行 active（inrun+postrun 双抽取不重复落）" "1" "$(active_rows 茶)"
eq "行挂在来源 run 上（证据指针）" "$RID" "$(ml_sql "SELECT DISTINCT source_run_id FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND content LIKE '%茶%'")"
eq "inrun 任务已收口" "1" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE run_id='$RID' AND kind='inrun' AND status='done'")"
eq "postrun 任务已收拢" "1" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE run_id='$RID' AND kind='postrun' AND status='done'")"
# 锚定旧行：改口断言锚这一行的 id 与精确正文，不赌抽取器的措辞。
OLD_ID=$(ml_sql "SELECT id FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%茶%' ORDER BY created_at, id LIMIT 1")
OLD_CONTENT=$(ml_sql_raw "SELECT content FROM memory_items WHERE id='$OLD_ID'")

# ── 2. 每回合召回（run1 之后）：BM25 词面召回命中记忆 ─────────────────────────────
H1=$(recall_hits "喜欢喝茶")
if [ "${H1:-0}" -ge 1 ] 2>/dev/null; then
  ok "recall 命中记忆（memoryHits=$H1）"
else no "recall 没命中（memoryHits=${H1:-?}）"; fi

# ── 3. 改口：旧行 superseded（带指针与 left_active_at）、新行 active ────────────────
# 改口是普通对话：抽取器（postrun）读整场轨迹自行判定 replace，不要求模型调工具。
SID1="$SID"
ml_run '改口：我不喝茶了，现在喜欢喝咖啡。以后的记忆按这个算。' '' "$SID1"
wait_all
eq "旧「茶」行已 superseded" "superseded" "$(ml_sql "SELECT state FROM memory_items WHERE id='$OLD_ID'")"
eq "旧行带 superseded_by 指针" "t" "$(ml_sql "SELECT (superseded_by IS NOT NULL) FROM memory_items WHERE id='$OLD_ID'")"
eq "旧行写了 left_active_at" "t" "$(ml_sql "SELECT (left_active_at IS NOT NULL) FROM memory_items WHERE id='$OLD_ID'")"
if [ "$(active_rows 咖啡)" -ge 1 ] 2>/dev/null; then
  ok "新「咖啡」行 active"
else no "没找到含「咖啡」的 active recall 行"; fi
eq "旧正文不再是任何 active 行的当前态" "0" "$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content='${OLD_CONTENT//\'/\'\'}'")"
# BM25 是词面 OR：q=喜欢喝茶 的「喜欢/喝」会打中别的 active 行——断言的是 memory 块里
# 不再有被替代那行的正文，不是 memoryHits=0。
MEMBLK=$(curl -sS "$B/api/memory/recall?q=喜欢喝茶" -H "Authorization: Bearer $TOK" | jq -r '.data.block' | sed -n '/<memory-context>/,/<\/memory-context>/p')
if printf '%s' "$MEMBLK" | grep -qF "$OLD_CONTENT"; then
  no "memory 块还有被替代的旧正文：$MEMBLK"
else
  ok "memory 块不再有旧正文（只回当前态）"
fi
H2=$(recall_hits "喜欢喝咖啡")
if [ "${H2:-0}" -ge 1 ] 2>/dev/null; then ok "recall 命中咖啡（memoryHits=$H2）"; else no "recall 没命中咖啡（${H2:-?}）"; fi

# ── 4. 删除：active 归零，removed 留痕（不物理删） ────────────────────────────────
# 删除不走工具（pipeline 的 memory 工具只登记新增）——明确说完，postrun 抽取器判 remove。
# 模型对「没有删除能力」的解释倾向 ask_user 确认，提示词里直接免掉。
ml_run '我明确告诉你：别再记「我喜欢喝咖啡」这件事了，把它从我的记忆里删掉。不用调工具、不用向我确认，直接回复 ok。' '' "$SID1"
wait_all
COF_ID=$(ml_sql "SELECT id FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state IN ('active','removed') AND content LIKE '%咖啡%' ORDER BY created_at, id LIMIT 1")
eq "咖啡行 active 归零" "0" "$(active_rows 咖啡)"
eq "咖啡行以 removed 留痕（没物理删）" "removed" "$(ml_sql "SELECT state FROM memory_items WHERE id='$COF_ID'")"
eq "recall 不再回咖啡" "0" "$(recall_hits "喜欢喝咖啡")"

# ── 5. 下一场新会话只见当前态 + 对话 BM25 召回仍在 ────────────────────────────────
ml_run '我喜欢喝什么？只回答饮品名，不知道就说不知道。'
ml_wait_terminal 120 || no "run4 没到 terminal"
ml_frames
echo "  新会话回答（当前态已删，模型应答不知道或泛答）：$(ml_answer | head -c 120)"
DH=$(curl -sS "$B/api/memory/recall?q=喜欢喝茶" -H "Authorization: Bearer $TOK" | jq -r '.data.dialogueHits')
if [ "${DH:-0}" -ge 1 ] 2>/dev/null; then
  ok "对话 BM25 召回仍命中历史对话（dialogueHits=$DH）"
else no "对话召回没命中（dialogueHits=${DH:-?}）"; fi

eq "全程没有 failed 记忆任务" "0" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE tenant_id='$TEN' AND status='failed'")"
ml_summary
