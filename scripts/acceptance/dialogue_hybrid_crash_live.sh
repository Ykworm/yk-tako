#!/usr/bin/env bash
# dialogue_hybrid_crash_live.sh — 历史对话混合检索（doc/84）崩溃组活体验收：场景 7 四窗口。
#
#   7a  PG 提交后 kill -9     chunk+outbox 两行已提交、relay 还没发布（published_at 空）
#   7b  ES 成功后 kill -9     ES 文档已写（embedding 故障让 Milvus 停在退避），进程死
#   7c  Milvus 成功后 kill -9  Milvus 行已写（ES 503 让 ES 停在退避），进程死
#   7d  done 写回前 kill -9   任务已领取（claim_epoch≥1、running）、embedding 被注入
#                             sleep12 拉长执行窗 → 稳定打在「外部写未完、done 未写回」
#
# 每窗断言：up.sh restart（同命令真二进制重启）后任务收敛 done、该 run 的 ES 文档与
# Milvus 行都恰好 1 份（幂等恢复、文档数不增）、attempts/claim_epoch 留下补做证据。
#
# kill -9 的对象是 up.sh 起的真二进制（go build -o <state>/yktako 后 exec，pid 就是引擎
# 进程本身），不是 go run 的父进程。重启用 up.sh restart（历史命令原样重放）。
#
# 全链路真跑：SQL 造 status='complete' 的 run（expected.json 用例 65/85 同款，不打模型
# ——DEEPSEEK 402 欠费），引擎自身索引循环 chunk→outbox→relay→双消费→ES/Zilliz 真调。
#
# 用法：bash scripts/acceptance/dialogue_hybrid_crash_live.sh
# 参数化：DH_ENV_NAME、DH_PORT、DH_REDIS_DB、DH_ES_PROXY_PORT、DH_EMB_PROXY_PORT、
#         TAKO_ENV、TAKO_PSQL、TAKO_PG_HOST/PORT。
# 清理：脚本结束 destroy 自己 init 的环境与故障代理——不碰开发库、不调 /api/dev/reset。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"     # yk-tako 根
UP="$ROOT/scripts/acceptance/dialogue_hybrid_up.sh"
LIB="$ROOT/scripts/acceptance/dialogue_hybrid_live_lib.sh"
ENV_FILE="${TAKO_ENV:-$ROOT/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin/psql}"
PG_HOST="${TAKO_PG_HOST:-127.0.0.1}"; PG_PORT="${TAKO_PG_PORT:-5433}"
PORT_A="${DH_PORT:-8944}"; RDB_A="${DH_REDIS_DB:-13}"
ES_PROXY_PORT="${DH_ES_PROXY_PORT:-7941}"; EMB_PROXY_PORT="${DH_EMB_PROXY_PORT:-7942}"
ENV_NAME="${DH_ENV_NAME:-crash$(date +%m%d%H%M%S)}"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/dh-crash.XXXXXX")"
PIDS_TO_KILL=()
DH_PSQL="$PSQL"; DH_ENV_FILE="$ENV_FILE"; DH_NAME="$ENV_NAME"
# shellcheck disable=SC1090
source "$LIB"

cleanup() {
  echo ""
  echo "===== 清理（只删本脚本创建的东西）====="
  bash "$UP" destroy "$ENV_NAME" 2>/dev/null | sed 's/^/  /'
  for pid in "${PIDS_TO_KILL[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null && echo "  killed proxy $pid"
  done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

fail_fast() { echo "基础设施前置检查失败：$*"; exit 1; }

echo "===== 前置检查 ====="
[ -f "$ENV_FILE" ] || fail_fast ".env 不在 $ENV_FILE"
"$PSQL" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -qAt -c "SELECT 1" >/dev/null 2>&1 \
  || fail_fast "psql 连不上 $PG_HOST:$PG_PORT"
dh_es_conf "$ENV_FILE"
[ -n "$DH_ES_URL" ] || fail_fast ".env 缺 TAKO_ES_URL"
curl -s -o /dev/null -u "$DH_ES_USER:$DH_ES_PASS" "$DH_ES_URL" || fail_fast "ES 不可达"
for p in "$ES_PROXY_PORT" "$EMB_PROXY_PORT"; do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && fail_fast "代理端口 $p 被占，换 DH_*_PROXY_PORT"
done
echo "✓ PG/ES/.env 就绪"

echo ""
echo "===== 起环境（init + 故障代理 + hybrid 引擎）====="
bash "$UP" init "$ENV_NAME" | sed 's/^/  /' || fail_fast "init 失败"
STATE="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_NAME/state.env"
# shellcheck disable=SC1090
source "$STATE"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"

ES_UPSTREAM="$(grep -E '^TAKO_ES_URL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
EMB_UPSTREAM="$(grep -E '^TAKO_DIALOGUE_EMBEDDING_BASE_URL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
echo "pass" > "$SCRATCH/es_fault.txt"; echo "pass" > "$SCRATCH/emb_fault.txt"
python3 "$ROOT/scripts/acceptance/dialogue_hybrid_chaos_proxy.py" http \
  --listen "127.0.0.1:$ES_PROXY_PORT" --upstream "$ES_UPSTREAM" \
  --fault-file "$SCRATCH/es_fault.txt" --log "$SCRATCH/es_proxy.log" \
  >"$SCRATCH/es_proxy.out" 2>&1 </dev/null & PIDS_TO_KILL+=($!)
python3 "$ROOT/scripts/acceptance/dialogue_hybrid_chaos_proxy.py" http \
  --listen "127.0.0.1:$EMB_PROXY_PORT" --upstream "$EMB_UPSTREAM" \
  --fault-file "$SCRATCH/emb_fault.txt" --log "$SCRATCH/emb_proxy.log" \
  >"$SCRATCH/emb_proxy.out" 2>&1 </dev/null & PIDS_TO_KILL+=($!)
sleep 1
curl -s -o /dev/null "http://127.0.0.1:$ES_PROXY_PORT" && ok "ES 故障代理起（pass 模式）" || fail_fast "ES 代理未监听"
curl -s -o /dev/null "http://127.0.0.1:$EMB_PROXY_PORT" && ok "embedding 故障代理起（pass 模式）" || fail_fast "embedding 代理未监听"

BASE_A="http://127.0.0.1:$PORT_A"
bash "$UP" start "$ENV_NAME" --tag a --port "$PORT_A" --redis-db "$RDB_A" --mode hybrid \
  --es-url "http://127.0.0.1:$ES_PROXY_PORT" --embed-base "http://127.0.0.1:$EMB_PROXY_PORT" \
  | sed 's/^/  /' || fail_fast "引擎起不来"

dh_signup "$BASE_A" t1
WS1="$(dh_mk_ws "$BASE_A" "$DH_TOK" dh-crash-ws1)"
[ -n "$WS1" ] && ok "账号+工作区 $WS1" || fail_fast "建工作区失败"

# kill_engine：对真二进制 kill -9 并确认端口释放。
kill_engine() {
  local dir; dir="$(state_dir_env)"
  local pid; pid="$(cat "$dir/eng_a.pid" 2>/dev/null)" || { no "没有 eng_a.pid 可杀"; return 1; }
  echo "  kill -9 引擎 pid=$pid"
  kill -9 "$pid" 2>/dev/null || true
  sleep 1
  if kill -0 "$pid" 2>/dev/null; then no "进程 $pid 没死"; return 1; fi
  ok "引擎已 kill -9（pid $pid）"
  rm -f "$dir/eng_a.pid"
}
state_dir_env() { echo "${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_NAME"; }

# restart_engine：故障撤到 pass 后 up.sh restart（同命令原样重放）。
restart_engine() {
  echo "pass" > "$SCRATCH/es_fault.txt"; echo "pass" > "$SCRATCH/emb_fault.txt"
  bash "$UP" restart "$ENV_NAME" a | sed 's/^/  /' || { no "restart 失败"; return 1; }
}

# seed_and_wait_rows <sid> <rid>：播种并等 dialogue_index_jobs 两行登记（索引循环拾取）。
seed_and_wait_rows() {
  local sid="$1" rid="$2" w=0
  dh_seed_run "$sid" "$rid" "'$WS1'" \
    "故障演练脚手架怎么搭" "演练环境的故障注入开关在 chaos 配置组，拨到 1 即生效" \
    "演练完怎么收尾" "收尾脚本只删自己建的资源，其余一律不动"
  while [ "$w" -lt 30 ]; do
    [ "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$rid'")" = "2" ] && return 0
    sleep 0.5; w=$((w+1))
  done
  return 1
}

echo ""
echo "═══ 7a PG 提交后 kill -9（chunk+outbox 已提交，relay 未发布）═══"
CAUGHT=0
for att in 1 2 3; do
  RID1="rcrash${att}a"; SID1="scrash${att}a"
  seed_and_wait_rows "$SID1" "$RID1" || { no "第 $att 次 30s 未见任务登记（索引循环没拾取？）"; continue; }
  # 抓「两行都 pending 且未发布」的窗口（relay 5s 一轮，窗口 ~0–6s，0.1s 轮询足够密）。
  for _ in $(seq 120); do
    N="$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$RID1'
                 AND status='pending' AND published_at IS NULL")"
    T="$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
    [ "$N" = "2" ] && [ "$T" = "2" ] && { CAUGHT=1; break; }
    sleep 0.1
  done
  [ "$CAUGHT" = 1 ] && break
  echo "  第 $att 次没抓到未发布窗口（relay 抢先发布了），换一场重试"
done
if [ "$CAUGHT" = 1 ]; then
  ok "抓到 PG 提交后窗口：run=$RID1 两任务 pending 且 published_at 空"
  ES_AT_KILL="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID1"'"}}}')"
  MIL_AT_KILL="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID1\"")"
  echo "  kill 时外部状态：ES=$ES_AT_KILL Milvus=$MIL_AT_KILL"
  kill_engine
  restart_engine || fail_fast "7a 重启失败"
  dh_wait_jobs_settled "$RID1" 300 && ok "7a 重启后任务收敛 done（≤300s）" || no "7a 任务 300s 未收敛"
  es1="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID1"'"}}}')"
  mil1="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID1\"")"
  eq "$es1" "1" "7a 恢复后 ES 文档恰好 1 份（文档数不增）"
  eq "$mil1" "1" "7a 恢复后 Milvus 行恰好 1 行"
  att1="$(dh_sql "SELECT min(attempts)||'/'||max(attempts) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
  echo "  7a attempts=$att1（补做证据）"
else
  no "7a 三次都没抓到未发布窗口——登记并跳过（见 p5-交付.md）"
fi

echo ""
echo "═══ 7b ES 成功后 kill -9（ES 文档已写，Milvus 停在退避）═══"
echo "500" > "$SCRATCH/emb_fault.txt"     # embedding 500 → milvus 任务退避，ES 臂不受影响
CAUGHT=0
for att in 1 2 3; do
  RID2="rcrash${att}b"; SID2="scrash${att}b"
  seed_and_wait_rows "$SID2" "$RID2" || { no "7b 第 $att 次 30s 未见任务登记"; continue; }
  for _ in $(seq 150); do
    ESC="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID2"'"}}}')"
    MS="$(dh_sql "SELECT coalesce(status,'none') FROM dialogue_index_jobs WHERE run_id='$RID2' AND target='milvus'")"
    [ "$ESC" = "1" ] && [ "$MS" != "done" ] && { CAUGHT=1; break; }
    sleep 0.2
  done
  [ "$CAUGHT" = 1 ] && break
  echo "  7b 第 $att 次没抓到窗口，换一场重试"
done
if [ "$CAUGHT" = 1 ]; then
  ok "抓到 ES 成功后窗口：run=$RID2 ES 文档已写、milvus 任务=$MS"
  MIL_AT_KILL="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID2\"")"
  echo "  kill 时外部状态：ES=1 Milvus=$MIL_AT_KILL；milvus 任务行：$(dh_sql "SELECT status||'|attempts='||attempts||'|err='||coalesce(last_error,'') FROM dialogue_index_jobs WHERE run_id='$RID2' AND target='milvus'")"
  kill_engine
  restart_engine || fail_fast "7b 重启失败"
  dh_wait_jobs_settled "$RID2" 300 && ok "7b 重启后任务收敛 done" || no "7b 任务 300s 未收敛"
  es2="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID2"'"}}}')"
  mil2="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID2\"")"
  eq "$es2" "1" "7b 恢复后 ES 文档仍恰好 1 份（崩溃前那份没被重复写）"
  eq "$mil2" "1" "7b 恢复后 Milvus 行恰好 1 行"
  att2="$(dh_sql "SELECT min(attempts)||'/'||max(attempts) FROM dialogue_index_jobs WHERE run_id='$RID2'")"
  echo "  7b attempts=$att2（milvus 退避→补做证据）"
else
  no "7b 三次都没抓到窗口——登记并跳过"
fi

echo ""
echo "═══ 7c Milvus 成功后 kill -9（Milvus 行已写，ES 停在退避）═══"
echo "503" > "$SCRATCH/es_fault.txt"       # ES 503 → es 任务退避，milvus 臂不受影响
CAUGHT=0
for att in 1 2 3; do
  RID3="rcrash${att}c"; SID3="scrash${att}c"
  seed_and_wait_rows "$SID3" "$RID3" || { no "7c 第 $att 次 30s 未见任务登记"; continue; }
  for _ in $(seq 200); do
    MILC="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID3\"")"
    ESJS="$(dh_sql "SELECT coalesce(status,'none') FROM dialogue_index_jobs WHERE run_id='$RID3' AND target='es'")"
    [ "$MILC" = "1" ] && [ "$ESJS" != "done" ] && { CAUGHT=1; break; }
    sleep 0.2
  done
  [ "$CAUGHT" = 1 ] && break
  echo "  7c 第 $att 次没抓到窗口，换一场重试"
done
if [ "$CAUGHT" = 1 ]; then
  ok "抓到 Milvus 成功后窗口：run=$RID3 Milvus 行已写、es 任务=$ESJS"
  ES_AT_KILL="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID3"'"}}}')"
  echo "  kill 时外部状态：ES=$ES_AT_KILL Milvus=1；es 任务行：$(dh_sql "SELECT status||'|attempts='||attempts||'|err='||coalesce(last_error,'') FROM dialogue_index_jobs WHERE run_id='$RID3' AND target='es'")"
  kill_engine
  restart_engine || fail_fast "7c 重启失败"
  dh_wait_jobs_settled "$RID3" 300 && ok "7c 重启后任务收敛 done" || no "7c 任务 300s 未收敛"
  es3="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID3"'"}}}')"
  mil3="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID3\"")"
  eq "$es3" "1" "7c 恢复后 ES 文档恰好 1 份（退避中的任务重启后补齐）"
  eq "$mil3" "1" "7c 恢复后 Milvus 行仍恰好 1 行"
  att3="$(dh_sql "SELECT min(attempts)||'/'||max(attempts) FROM dialogue_index_jobs WHERE run_id='$RID3'")"
  echo "  7c attempts=$att3（es 退避→补做证据）"
else
  no "7c 三次都没抓到窗口——登记并跳过"
fi

echo ""
echo "═══ 7d done 写回前 kill -9（已领取 running、embedding 注入 sleep12 拉长执行窗）═══"
CAUGHT=0
for att in 1 2 3; do
  RID4="rcrash${att}d"; SID4="scrash${att}d"
  seed_and_wait_rows "$SID4" "$RID4" || { no "7d 第 $att 次 30s 未见任务登记"; continue; }
  echo "sleep12" > "$SCRATCH/emb_fault.txt"   # milvus 任务的 embedding 调用睡 12s——执行窗拉到秒级
  for _ in $(seq 150); do
    ROW="$(dh_sql "SELECT id||'|'||claim_epoch FROM dialogue_index_jobs
                   WHERE run_id='$RID4' AND target='milvus' AND status='running' LIMIT 1")"
    [ -n "$ROW" ] && { CAUGHT=1; break; }
    sleep 0.1
  done
  [ "$CAUGHT" = 1 ] && break
  echo "  7d 第 $att 次没抓到 running 窗口，换一场重试"
done
if [ "$CAUGHT" = 1 ]; then
  JOBID="${ROW%%|*}"; EP_AT_KILL="${ROW##*|}"
  ok "抓到 done 写回前窗口：milvus 任务 $JOBID running（claim_epoch=$EP_AT_KILL），embedding 在途"
  ES_AT_KILL="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID4"'"}}}')"
  MIL_AT_KILL="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID4\"")"
  echo "  kill 时外部状态：ES=$ES_AT_KILL Milvus=$MIL_AT_KILL（ES 臂已完成，milvus 写入在途未完成）"
  kill_engine
  restart_engine || fail_fast "7d 重启失败"
  echo "  等租约过期（2min）+ 补做收敛……"
  dh_wait_jobs_settled "$RID4" 300 && ok "7d 重启后任务收敛 done（租约过期→接管→幂等补做）" || no "7d 任务 300s 未收敛"
  es4="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID4"'"}}}')"
  mil4="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID4\"")"
  eq "$es4" "1" "7d 恢复后 ES 文档恰好 1 份"
  eq "$mil4" "1" "7d 恢复后 Milvus 行恰好 1 行（执行中途被杀也没写出两份）"
  EP_NOW="$(dh_sql "SELECT claim_epoch FROM dialogue_index_jobs WHERE id='$JOBID'")"
  if [ "${EP_NOW:-0}" -gt "${EP_AT_KILL:-0}" ] 2>/dev/null; then
    ok "接管证据：claim_epoch $EP_AT_KILL → $EP_NOW（旧代次被新代次接管）"
  else
    no "claim_epoch 未增长（$EP_AT_KILL → $EP_NOW）——补做路径存疑"
  fi
else
  no "7d 三次都没抓到 running 窗口——登记并跳过"
fi

echo ""
echo "═══ 收尾一致性（四窗全场景无死信、全索引计数守恒）═══"
eq "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE status='failed'")" "0" "全程无 failed 死信"
PG_RUNS="$(dh_sql "SELECT count(DISTINCT run_id) FROM dialogue_chunks")"
ES_ALL="$(dh_es_count "$DH_ES_INDEX")"
MIL_ALL="$(dh_eval count -collection "$DH_MILVUS_COLL")"
echo "  总账：PG chunk run 数=$PG_RUNS ES 文档=$ES_ALL Milvus 行=$MIL_ALL"
eq "$ES_ALL" "$PG_RUNS" "ES 全索引文档数 == PG chunk run 数（最终一致）"
eq "$MIL_ALL" "$PG_RUNS" "Milvus 全 collection 行数 == PG chunk run 数（最终一致）"

dh_summary
