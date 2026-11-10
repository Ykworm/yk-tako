#!/usr/bin/env bash
# 项目删除的记忆级联活体（doc/65 §3.3 / §13.3-6）：
#   删项目 → scope='workspace' 的记忆**两层都清**（resident + recall）、该项目的 memory_jobs 清掉；
#   scope='user' 的行跨项目仍可见；USER.md 一个字节不动；runs / runlog_entries 不动（运行档案留着）。
#
# 靶子：预起的隔离引擎（markdown 或 pipeline 均可——级联在存储层，与模式无关；
#       本脚本按 markdown 起法准备 USER.md  fixture）。别拿开发实例当靶子。
# 参数：TAKO_BASE / TAKO_ENV / TAKO_PG_DSN（必填）/ TAKO_PSQL / TAKO_WS_ROOT（必填）
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 密钥取自 ${ENV} · workspace ${WS_ROOT}"
ml_signup projdel

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"待删项目"}' | jq -r .data.workspace.id)
if [ -z "$WID" ] || [ "$WID" = "null" ]; then no "建项目失败"; ml_summary; fi

# 一场真 run 挂在项目上（runs/runlog 的「不动」断言要有真行）。
ml_run '只回复 ok，不要做任何其他事。' "$WID"
ml_wait_terminal 120 || { no "run 没到 terminal"; ml_summary; }
ml_wait_settled || no "会话没落定"
RUN_N=$(ml_sql "SELECT count(*) FROM runs WHERE workspace_id='$WID'")
LOG_N=$(ml_sql "SELECT count(*) FROM runlog_entries WHERE run_id='$RID'")

# 种子：workspace 两层各一行 + user 一行（dev 数据准备口，不是产品管理 API）。
seed(){ # $1=layer $2=scope $3=content $4=workspaceId
  curl -sS -X POST "$B/api/dev/memory" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"layer":"'"$1"'","scope":"'"$2"'","content":"'"$3"'","workspaceId":"'"$4"'"}' | jq -r .data.id
}
MW_R=$(seed resident workspace '项目级常驻规矩' "$WID")
MW_C=$(seed recall workspace '项目级召回事实' "$WID")
MU=$(seed resident user '跨项目的个人偏好' '')
if [ -z "$MW_R" ] || [ "$MW_R" = "null" ] || [ -z "$MW_C" ] || [ "$MW_C" = "null" ] || [ -z "$MU" ] || [ "$MU" = "null" ]; then
  no "dev 种子没种上"; ml_summary
fi
# USER.md fixture：直接写盘（本脚本验的是「删项目不碰它」，不是写它的链路）。
UF="$WS_ROOT/tenants/$TEN/users/$ML_UID/USER.md"
mkdir -p "$(dirname "$UF")"
printf '用户偏好 fixture：喝茶' > "$UF"
UF_HASH=$(shasum -a 256 "$UF" | cut -d' ' -f1)

# ── 删项目 ─────────────────────────────────────────────────────────────────────
resp=$(curl -sS -X DELETE "$B/api/workspaces/$WID" -H "Authorization: Bearer $TOK")
eq "DELETE 成功" "deleted" "$(echo "$resp" | jq -r .data.status)"

eq "workspace resident 行已清" "" "$(ml_sql "SELECT id FROM memory_items WHERE id='$MW_R'")"
eq "workspace recall 行已清" "" "$(ml_sql "SELECT id FROM memory_items WHERE id='$MW_C'")"
eq "scope=workspace 两层都无残留" "0" "$(ml_sql "SELECT count(*) FROM memory_items WHERE scope='workspace' AND workspace='$WID'")"
eq "user 行跨项目仍在且 active" "active" "$(ml_sql "SELECT state FROM memory_items WHERE id='$MU'")"
eq "USER.md 字节不变" "$UF_HASH" "$(shasum -a 256 "$UF" 2>/dev/null | cut -d' ' -f1)"
eq "runs 行数不动" "$RUN_N" "$(ml_sql "SELECT count(*) FROM runs WHERE workspace_id='$WID'")"
eq "runlog 行数不动" "$LOG_N" "$(ml_sql "SELECT count(*) FROM runlog_entries WHERE run_id='$RID'")"

# user 行跨项目仍可见：pipeline 模式用观测口直接断言（user 行不挂项目，任何项目上下文都可见）；
# markdown 模式观测口关闭（409），退到存储层断言（workspace 列本来就空，删项目不改它）。
items=$(curl -sS "$B/api/memory/items?limit=200" -H "Authorization: Bearer $TOK")
if [ "$(echo "$items" | jq -r .code)" = "0" ]; then
  if echo "$items" | jq -e --arg id "$MU" '.data.items[] | select(.id==$id)' >/dev/null; then
    ok "user 行在观测口仍可见（跨项目）"
  else
    no "user 行在观测口看不到了"
  fi
else
  eq "（markdown）user 行不挂被删项目" "" "$(ml_sql "SELECT workspace FROM memory_items WHERE id='$MU'")"
fi

ml_summary