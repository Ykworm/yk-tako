#!/usr/bin/env bash
# dialogue_hybrid_realrun_live.sh — 历史对话混合检索（doc/84）「真实回合端到端」活体验收。
#
# 为什么单独一个脚本：四组活体（live/crash/two_replicas/rebuild）为了绕开 DEEPSEEK 402，
# 全部用 SQL 播种 status='complete' 的 run + runlog（`dh_seed_run`），只验证了「引擎自己的
# 索引循环拾取 complete run → chunk → outbox → relay → 双消费 → ES/Milvus（embedding 真调）」。
# 它们**没验证**「用户发消息 → 真模型 run → 完成 → 被索引 → 被召回」这条从真实回合起的链路。
# DEEPSEEK 恢复后补这一段：真 run（平台密钥兜底租户密钥）→ 真对话 → 真索引 → 真召回命中。
#
# 场景 R1：真 run 端到端 + 语义命中
#   R1a 真 run 完成（settled=complete）
#   R1b 对话块落库（dialogue_chunks 恰 1 行、正文含用户原话）+ ES 该 run 恰 1 篇
#   R1c 混合召回命中：用与原文**词面不同**的查询（周末什么时候上线 ↔ 发布窗口定在周六凌晨三点），
#       dev query（带 workspaceId）合并结果必须含该 run —— 向量臂真跑通。
#
# 全链路真跑：POST /api/runs 打真模型（deepseek/deepseek-flash）；引擎自身索引循环走
# chunk → outbox → relay → 双消费 → ES/Zilliz（embedding 真调）。
#
# 用法：bash scripts/acceptance/dialogue_hybrid_realrun_live.sh
# 参数化：TAKO_ENV、TAKO_PSQL、TAKO_PG_HOST/PORT、DH_PORT、DH_REDIS_DB。
# 清理：脚本结束 destroy 自己 init 的环境（PG 库/ES 索引/Zilliz collection/工作区），
#       杀自己起的引擎——不碰开发库 tako、不调 /api/dev/reset、不动别人进程。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UP="$ROOT/scripts/acceptance/dialogue_hybrid_up.sh"
LIB="$ROOT/scripts/acceptance/dialogue_hybrid_live_lib.sh"
EINO="$ROOT/yk-tako-eino"
ENV_FILE="${TAKO_ENV:-$EINO/.env}"
PSQL="${TAKO_PSQL:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin/psql}"
PG_HOST="${TAKO_PG_HOST:-127.0.0.1}"; PG_PORT="${TAKO_PG_PORT:-5433}"
PORT="${DH_PORT:-8949}"
RDB="${DH_REDIS_DB:-11}"
ENV_MAIN="realrun$(date +%m%d%H%M%S)"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/dh-realrun.XXXXXX")"
DH_PSQL="$PSQL"; DH_ENV_FILE="$ENV_FILE"; DH_NAME="$ENV_MAIN"
# shellcheck disable=SC1090
source "$LIB"

cleanup() {
  echo ""
  echo "===== 清理（只删本脚本创建的东西）====="
  bash "$UP" stop "$ENV_MAIN" 2>/dev/null | sed 's/^/  /'
  bash "$UP" destroy "$ENV_MAIN" 2>/dev/null | sed 's/^/  /'
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

fail_fast() { echo "基础设施前置检查失败：$*"; exit 1; }

echo "===== 前置检查 ====="
[ -f "$ENV_FILE" ] || fail_fast ".env 不在 $ENV_FILE"
command -v jq >/dev/null 2>&1 || fail_fast "缺 jq"
"$PSQL" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -qAt -c "SELECT 1" >/dev/null 2>&1 \
  || fail_fast "psql 连不上 $PG_HOST:$PG_PORT"
dh_es_conf "$ENV_FILE"
[ -n "$DH_ES_URL" ] || fail_fast ".env 缺 TAKO_ES_URL"
DEEPSEEK_KEY="$(grep -E '^DEEPSEEK_API_KEY=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
[ -n "$DEEPSEEK_KEY" ] || fail_fast ".env 缺 DEEPSEEK_API_KEY（真 run 需要模型密钥）"
echo "✓ PG/ES/.env/jq 就绪"

echo ""
echo "===== 起环境（init + 引擎 hybrid）====="
bash "$UP" init "$ENV_MAIN" | sed 's/^/  /' || fail_fast "init 失败"
STATE_MAIN="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/state.env"
# shellcheck disable=SC1090
source "$STATE_MAIN"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"
BASE_R="http://127.0.0.1:$PORT"
bash "$UP" start "$ENV_MAIN" --tag a --port "$PORT" --redis-db "$RDB" --mode hybrid | sed 's/^/  /' \
  || fail_fast "引擎起不来"

# 租户装模型密钥：否则 POST /api/runs 回 111408「模型还不可用」。与生产口径一致
# （密钥值只从 .env 读、不进日志）。
dh_signup "$BASE_R" r1
TOK="$DH_TOK"; TEN="$DH_TEN"
CFG_BODY="$(jq -cn --arg k "$DEEPSEEK_KEY" \
  '{modelRef:"deepseek/deepseek-flash",mcpServers:{},secrets:{DEEPSEEK_API_KEY:$k},sandbox:{network:false}}')"
RC="$(dh_api_at "$BASE_R" "$TOK" PUT "/api/tenants/$TEN/runtime-config" "$CFG_BODY")"
eq "$(printf '%s' "$RC" | jq -r '.code // "x"')" "0" "租户模型密钥配置成功"
WS1="$(dh_mk_ws "$BASE_R" "$TOK" dh-ws-realrun)"
[ -n "$WS1" ] && ok "账号 + 工作区($WS1)" || { no "建工作区失败"; exit 1; }

# 发一场真 run（真模型）。sets DH_RID/DH_SID。
dh_real_run() { # dh_real_run <text> [sessionId]
  local body resp
  body="$(jq -cn --arg t "$1" --arg m "m_real_$(date +%s%N)" --arg w "$WS1" --arg s "${2:-}" \
    '{messageId:$m,text:$t,workspaceId:$w} + (if $s != "" then {sessionId:$s} else {} end)')"
  resp="$(dh_api_at "$BASE_R" "$TOK" POST "/api/runs" "$body")"
  DH_RID="$(printf '%s' "$resp" | jq -r '.data.runId // ""')"
  DH_SID="$(printf '%s' "$resp" | jq -r '.data.sessionId // ""')"
}
# 等会话落定（真 run 可能几十秒）。
dh_real_settled() { # dh_real_settled <sid> [timeout]
  local t=0 st live b
  while [ "$t" -lt "${2:-300}" ]; do
    b="$(dh_api_at "$BASE_R" "$TOK" GET "/api/sessions/$1")"
    st="$(printf '%s' "$b" | jq -r '.data.lastRunStatus // ""')"
    live="$(printf '%s' "$b" | jq -r '.data.liveRunId // ""')"
    case "$st" in
      complete) [ -z "$live" ] && return 0 ;;
      error|stopped) return 1 ;;
    esac
    sleep 2; t=$((t+2))
  done
  return 1
}

echo ""
echo "═══ 场景 R1：真实回合端到端（真 run → 真索引 → 真召回）═══"
dh_real_run "记一下：发布窗口定在周六凌晨三点，值班按周六排。"
RID1="$DH_RID"; SID1="$DH_SID"
[ -n "$RID1" ] && ok "真 run 投递成功（run=$RID1 session=${SID1}）" || { no "真 run 投递失败（回 111408？）"; dh_summary; exit 1; }

dh_real_settled "$SID1" 300 && ok "真 run 完成（settled=complete）" || no "真 run 300s 未完成"
dh_wait_jobs_settled "$RID1" 180 && ok "该 run 索引收敛" || no "该 run 180s 未收敛"

echo "  -- R1b 对话块 + ES 落库 --"
eq "$(dh_sql "SELECT count(*) FROM dialogue_chunks WHERE run_id='$RID1'")" "1" "dialogue_chunks 恰 1 行"
PASSAGE="$(dh_sql "SELECT passage FROM dialogue_chunks WHERE run_id='$RID1'")"
case "$PASSAGE" in
  *"发布窗口定在周六凌晨三点"*) ok "对话块正文含用户原话（原文保真）" ;;
  *) no "对话块正文未见用户原话" ;;
esac
eq "$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID1"'"}}}')" "1" "ES 该 run 恰 1 篇"

echo "  -- R1c 混合召回命中（词面不同的查询）--"
# 「周末什么时候上线」与原文「发布窗口定在周六凌晨三点」词面不重叠：BM25 会漏，靠向量臂命中。
dh_wait_query_hit "$BASE_R" "$TOK" "$WS1" "周末什么时候上线" "$RID1" 180 \
  && ok "真 run 的对话被 hybrid 召回命中（向量臂真跑通）" \
  || no "真 run 的对话 180s 未被 hybrid 召回命中"

dh_summary
