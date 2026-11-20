#!/usr/bin/env bash
# dialogue_hybrid_two_replicas_live.sh — 历史对话混合检索（doc/84）双副本组活体验收：场景 8。
#
# 两台引擎（同 PG 库、同 Redpanda broker、同 ES 索引、同 Milvus collection、各自独立
# Redis db）同时起、同时消费。断言（doc/84 §9.8 / 施工包 §8.3 场景 8）：
#   8a  双副本共存：两台 status 都是 hybrid 且活跃指针一致（同索引同 collection 同代次）
#   8b  同一目标任务只有一个有效 claim_epoch：并发消费下每个任务 claim_epoch=1、attempts=1，
#       外部文档 ES/Milvus 各恰好 1 份（SKIP LOCKED + claim_epoch 互斥的真实效果）
#   8c  重复消息幂等（旧消息不能复活/覆盖）：把已 done 的任务 published_at 清空 → relay 重投
#       → 两副本都收到同一条消息 → 没人能再领取（done 不可领取）→ claim_epoch 不动、文档不增
#   8d  并发播种多场 run：全部收敛后逐任务 done、外部逐 run 恰好 1 份、总量守恒、无死信
#
# 全链路真跑：SQL 造 status='complete' 的 run（expected.json 用例 65/85 同款，不打模型
# ——DEEPSEEK 402 欠费），两副本的索引循环/relay/消费者真实竞争同一批任务。
#
# 用法：bash scripts/acceptance/dialogue_hybrid_two_replicas_live.sh
# 参数化：DH_ENV_NAME、DH_PORT_R1/R2、DH_REDIS_DB_R1/R2、TAKO_ENV、TAKO_PSQL、TAKO_PG_HOST/PORT。
# 清理：脚本结束 destroy 自己 init 的环境——不碰开发库、不调 /api/dev/reset、不动别人进程。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"     # yk-tako 根
UP="$ROOT/scripts/acceptance/dialogue_hybrid_up.sh"
LIB="$ROOT/scripts/acceptance/dialogue_hybrid_live_lib.sh"
ENV_FILE="${TAKO_ENV:-$ROOT/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin/psql}"
PG_HOST="${TAKO_PG_HOST:-127.0.0.1}"; PG_PORT="${TAKO_PG_PORT:-5433}"
PORT_R1="${DH_PORT_R1:-8947}"; PORT_R2="${DH_PORT_R2:-8948}"
RDB_R1="${DH_REDIS_DB_R1:-14}"; RDB_R2="${DH_REDIS_DB_R2:-15}"
ENV_NAME="${DH_ENV_NAME:-rep$(date +%m%d%H%M%S)}"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/dh-rep.XXXXXX")"
DH_PSQL="$PSQL"; DH_ENV_FILE="$ENV_FILE"; DH_NAME="$ENV_NAME"
# shellcheck disable=SC1090
source "$LIB"

cleanup() {
  echo ""
  echo "===== 清理（只删本脚本创建的东西）====="
  bash "$UP" destroy "$ENV_NAME" 2>/dev/null | sed 's/^/  /'
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
echo "✓ PG/ES/.env 就绪"

echo ""
echo "===== 起环境（一套共享依赖 + 两台 hybrid 引擎）====="
bash "$UP" init "$ENV_NAME" | sed 's/^/  /' || fail_fast "init 失败"
STATE="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_NAME/state.env"
# shellcheck disable=SC1090
source "$STATE"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"

BASE1="http://127.0.0.1:$PORT_R1"; BASE2="http://127.0.0.1:$PORT_R2"
bash "$UP" start "$ENV_NAME" --tag r1 --port "$PORT_R1" --redis-db "$RDB_R1" --mode hybrid | sed 's/^/  /' \
  || fail_fast "副本 r1 起不来"
bash "$UP" start "$ENV_NAME" --tag r2 --port "$PORT_R2" --redis-db "$RDB_R2" --mode hybrid | sed 's/^/  /' \
  || fail_fast "副本 r2 起不来"

dh_signup "$BASE1" t1
TOK1="$DH_TOK"
WS1="$(dh_mk_ws "$BASE1" "$TOK1" dh-rep-ws1)"
[ -n "$WS1" ] && ok "账号+工作区 $WS1（共享同一租户）" || fail_fast "建工作区失败"

# job_row <run_id>：status|epoch|attempts 串（两个 target 各一行）。
job_rows() {
  dh_sql "SELECT target||':'||status||'|epoch='||claim_epoch||'|att='||attempts
          FROM dialogue_index_jobs WHERE run_id='$1' ORDER BY target"
}

echo ""
echo "═══ 8a 双副本共存：status 与活跃指针一致 ══"
ST1="$(dh_api_at "$BASE1" "$TOK1" GET "/api/dev/dialogue-retrieval/status")"
ST2="$(dh_api_at "$BASE2" "$TOK1" GET "/api/dev/dialogue-retrieval/status")"
PTR1="$(echo "$ST1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["pointer"])')"
PTR2="$(echo "$ST2" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["pointer"])')"
M1="$(echo "$ST1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["mode"])')"
M2="$(echo "$ST2" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["mode"])')"
eq "$M1" "hybrid" "副本1 mode=hybrid"
eq "$M2" "hybrid" "副本2 mode=hybrid"
eq "$PTR1" "$PTR2" "两副本活跃指针逐字节一致（同索引/同 collection/同代次）"
echo "  pointer: $PTR1"

echo ""
echo "═══ 8b 同一目标任务只有一个有效 claim_epoch（并发消费）═══"
RID1="rrep1"
dh_seed_run "srep1" "$RID1" "'$WS1'" \
  "双副本并发怎么验证" "并发验证看不变量：领取互斥、写回守卫、外部文档唯一" \
  "万一两个副本都写呢" "SKIP LOCKED 保证只有一个副本领到，另一个空手"
dh_wait_jobs_settled "$RID1" 180 && ok "8b 任务收敛（两副本竞争下正常完成）" || no "8b 任务 180s 未收敛"
ROWS="$(job_rows "$RID1")"
echo "  $ROWS"
EP1="$(dh_sql "SELECT max(claim_epoch) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
AT1="$(dh_sql "SELECT max(attempts) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
eq "$EP1" "1" "claim_epoch=1（只有一个副本领到，另一个没领成）"
eq "$AT1" "1" "attempts=1（没有第二个副本重做）"
es1="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID1"'"}}}')"
mil1="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID1\"")"
eq "$es1" "1" "8b ES 文档恰好 1 份"
eq "$mil1" "1" "8b Milvus 行恰好 1 行"
eq "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$RID1'")" "2" "任务行恰好 2 条（es+milvus 各一，重复登记被唯一约束挡住）"

echo ""
echo "═══ 8c 重复消息幂等：done 后重投同一条消息，无人能领取、无人能覆盖 ═══"
dh_sql "UPDATE dialogue_index_jobs SET published_at=NULL WHERE run_id='$RID1'"
WP=0; w8c=0
while [ "$w8c" -lt 60 ]; do
  [ "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$RID1' AND published_at IS NULL")" = "0" ] && { WP=1; break; }
  sleep 2; w8c=$((w8c+2))
done
[ "$WP" = 1 ] && ok "relay 已重投（published_at 盖回——至少一次投递）" || no "relay 60s 未重投"
sleep 8   # 给两副本的消费者都留出收消息、尝试领取的时间窗
dh_wait_jobs_settled "$RID1" 60
EP1B="$(dh_sql "SELECT max(claim_epoch) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
AT1B="$(dh_sql "SELECT max(attempts) FROM dialogue_index_jobs WHERE run_id='$RID1'")"
eq "$EP1B" "1" "重投后 claim_epoch 仍=1（done 任务不可领取，重复消息被丢弃）"
eq "$AT1B" "1" "重投后 attempts 仍=1（没有副本重做）"
es1b="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID1"'"}}}')"
mil1b="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID1\"")"
eq "$es1b" "1" "重投后 ES 仍 1 份（旧消息没有写出第二份）"
eq "$mil1b" "1" "重投后 Milvus 仍 1 行"
eq "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$RID1' AND status='done'")" "2" "两任务仍 done"

echo ""
echo "═══ 8d 并发播种三场 run：总量守恒、逐 run 唯一、无死信 ═══"
for i in 2 3 4; do
  dh_seed_run "srep$i" "rrep$i" "'$WS1'" \
    "并发播种第${i}场的验证点" "每场 run 的任务被两副本抢领，最终每份外部文档只有一行" \
    "第${i}场的收尾断言" "任务 done、claim_epoch=1、文档不增"
done
ALLSET=1
for i in 2 3 4; do
  dh_wait_jobs_settled "rrep$i" 180 || { no "rrep$i 180s 未收敛"; ALLSET=0; }
done
[ "$ALLSET" = 1 ] && ok "三场 run 全部收敛" || true
for i in 2 3 4; do
  ri="rrep$i"
  echo "  $ri: $(job_rows "$ri" | tr '\n' ' ')"
  epi="$(dh_sql "SELECT max(claim_epoch) FROM dialogue_index_jobs WHERE run_id='$ri'")"
  ati="$(dh_sql "SELECT max(attempts) FROM dialogue_index_jobs WHERE run_id='$ri'")"
  eq "$epi" "1" "$ri claim_epoch=1"
  eq "$ati" "1" "$ri attempts=1"
  esi="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$ri"'"}}}')"
  mili="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$ri\"")"
  eq "$esi" "1" "$ri ES 恰好 1 份"
  eq "$mili" "1" "$ri Milvus 恰好 1 行"
done

echo "  -- 总账 --"
eq "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE status='failed'")" "0" "全程无 failed 死信"
PG_RUNS="$(dh_sql "SELECT count(*) FROM dialogue_chunks")"
ES_ALL="$(dh_es_count "$DH_ES_INDEX")"
MIL_ALL="$(dh_eval count -collection "$DH_MILVUS_COLL")"
echo "  PG chunk=$PG_RUNS ES=$ES_ALL Milvus=$MIL_ALL"
eq "$ES_ALL" "$PG_RUNS" "ES 总数 == PG chunk 数"
eq "$MIL_ALL" "$PG_RUNS" "Milvus 总数 == PG chunk 数"
COV_JSON="$(dh_api_at "$BASE1" "$TOK1" GET "/api/dev/dialogue-retrieval/status")"
COV_LAG="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("consumerLag"))' 2>/dev/null)"
eq "${COV_LAG:-1}" "0" "消费积压归零（consumerLag=0，两副本共组消费完）"
COV_PENDING="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("pending"))' 2>/dev/null)"
eq "${COV_PENDING:-1}" "0" "无遗留 pending 任务"

dh_summary
