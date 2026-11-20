#!/usr/bin/env bash
# dialogue_hybrid_live.sh — 历史对话混合检索（doc/84）基本组活体验收：
#   场景 1  语义增益（pg_search 对照臂漏检，hybrid 向量臂命中）
#   场景 2  跨会话可查；跨项目/跨租户/伪造 workspaceId 不可见
#   场景 4  引用带来源标注；Milvus 零命中（断链）→ 整块不发（fail-closed）
#   场景 5  重复投递/重复消费/乱序旧版本 → 幂等收敛
#   场景 6  ES/embedding/Milvus 故障 → 退避、死信、查询端 119001–119004、恢复收敛
#   场景 10 可观测：status 覆盖率真实、trace 8 个 span、日志/响应无正文无密钥
#
# 全链路真跑：SQL 造 status='complete' 的 run（expected.json 用例 65/85 同款，不打模型——
# .env 的 DEEPSEEK 已 402 欠费，场景 3 需要真模型、如实登记为环境限制），引擎自己的
# 索引循环从 chunk → outbox → relay → 双消费 → ES/Zilliz embedding 真调一路走完。
#
# 用法：bash scripts/acceptance/dialogue_hybrid_live.sh
# 参数化：DH_ENV_NAME、DH_PORT_A/B/C、DH_REDIS_DB_A/B/C、TAKO_ENV、TAKO_PSQL、TAKO_PG_HOST/PORT。
# 清理：脚本结束 destroy 自己 init 的两套环境（PG 库/ES 索引/Zilliz collection/工作区），
#       杀自己起的引擎与故障代理——不碰开发库 tako、不调 /api/dev/reset、不动别人进程。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"     # yk-tako 根
UP="$ROOT/scripts/acceptance/dialogue_hybrid_up.sh"
LIB="$ROOT/scripts/acceptance/dialogue_hybrid_live_lib.sh"
EINO="$ROOT/yk-tako-eino"
ENV_FILE="${TAKO_ENV:-$EINO/.env}"
PSQL="${TAKO_PSQL:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin/psql}"
PG_HOST="${TAKO_PG_HOST:-127.0.0.1}"; PG_PORT="${TAKO_PG_PORT:-5433}"
PORT_A="${DH_PORT_A:-8944}"; PORT_B="${DH_PORT_B:-8945}"; PORT_C="${DH_PORT_C:-8946}"
RDB_A="${DH_REDIS_DB_A:-11}"; RDB_B="${DH_REDIS_DB_B:-12}"; RDB_C="${DH_REDIS_DB_C:-13}"
ENV_MAIN="${DH_ENV_NAME:-live$(date +%m%d%H%M%S)}"
ENV_FAULT="f${ENV_MAIN#live}"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/dh-live.XXXXXX")"
PIDS_TO_KILL=()
DH_PSQL="$PSQL"; DH_ENV_FILE="$ENV_FILE"; DH_NAME="$ENV_MAIN"
# shellcheck disable=SC1090
source "$LIB"

cleanup() {
  echo ""
  echo "===== 清理（只删本脚本创建的东西）====="
  bash "$UP" destroy "$ENV_FAULT" 2>/dev/null | sed 's/^/  /'
  bash "$UP" destroy "$ENV_MAIN" 2>/dev/null | sed 's/^/  /'
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
echo "✓ PG/ES/.env 就绪"

dh_lf_conf "$ENV_FILE"
T0_ISO="$(dh_now_iso)"
echo "✓ Langfuse $DH_LF_URL / 时间窗起点 $T0_ISO"

echo ""
echo "===== 起环境（up.sh init × 2 + 引擎 A=hybrid B=pg_search）====="
bash "$UP" init "$ENV_MAIN" | sed 's/^/  /' || fail_fast "init 主环境失败"
bash "$UP" init "$ENV_FAULT" | sed 's/^/  /' || fail_fast "init 故障环境失败"
STATE_MAIN="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/state.env"
STATE_FAULT="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_FAULT/state.env"
# shellcheck disable=SC1090
source "$STATE_MAIN"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"

BASE_A="http://127.0.0.1:$PORT_A"; BASE_B="http://127.0.0.1:$PORT_B"; BASE_C="http://127.0.0.1:$PORT_C"
bash "$UP" start "$ENV_MAIN" --tag a --port "$PORT_A" --redis-db "$RDB_A" --mode hybrid | sed 's/^/  /' \
  || fail_fast "引擎 A（hybrid）起不来"
bash "$UP" start "$ENV_MAIN" --tag b --port "$PORT_B" --redis-db "$RDB_B" --mode pg_search | sed 's/^/  /' \
  || fail_fast "引擎 B（pg_search 对照）起不来"

# signups / workspaces
dh_signup "$BASE_A" t1; TEN1="$DH_TEN"; UID1="$DH_UID"; TOK1="$DH_TOK"
WS1="$(dh_mk_ws "$BASE_A" "$TOK1" dh-ws1)"; WS2="$(dh_mk_ws "$BASE_A" "$TOK1" dh-ws2)"
[ -n "$WS1" ] && [ -n "$WS2" ] && ok "租户1 账号+ws1($WS1)+ws2($WS2)" || { no "建工作区失败"; exit 1; }
dh_signup "$BASE_A" t2; TEN2="$DH_TEN"; UID2="$DH_UID"; TOK2="$DH_TOK"
WS3="$(dh_mk_ws "$BASE_A" "$TOK2" dh-ws3-t2)"
[ -n "$WS3" ] && ok "租户2 账号+ws3($WS3)" || { no "租户2 工作区失败"; exit 1; }

SID1="s${ENV_MAIN}1"; RID1="r${ENV_MAIN}1"
SID2="s${ENV_MAIN}2"; RID2="r${ENV_MAIN}2"
SID3="s${ENV_MAIN}3"; RID3="r${ENV_MAIN}3"
SID4="s${ENV_MAIN}4"; RID4="r${ENV_MAIN}4"
SID5="s${ENV_MAIN}5"; RID5="r${ENV_MAIN}5"
SID6="s${ENV_MAIN}6"; RID6="r${ENV_MAIN}6"
SID7="s${ENV_MAIN}7"; RID7="r${ENV_MAIN}7"

# 场景 10 采点容器
COV_SEEN_PENDING=""; COV_SEEN_ZERO=""; COV_SEEN_FAILED=""
Q_HTTP_119001=""; Q_HTTP_119002=""; Q_HTTP_119003=""; Q_HTTP_119004=""

# dh_q <base> <tok> <ws> <query>：完整 envelope 存 $SCRATCH/last_query.json；打印 merged runIds
# （json 必须从文件喂给 python3——裸 python3 -c 的 stdin 不是上一个命令的输出）。
dh_q() {
  curl -sS -X POST "$1/api/dev/dialogue-retrieval/query" \
    -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -d '{"query":"'"$4"'","workspaceId":"'"$3"'"}' > "$SCRATCH/last_query.json"
  python3 -c 'import json,sys;d=json.load(sys.stdin);print(" ".join(m["runId"] for m in (d.get("data") or {}).get("merged") or []))' \
    < "$SCRATCH/last_query.json" 2>/dev/null || echo "ERR"
}

echo ""
echo "═══ 场景 1：语义增益（对照臂 pg_search 先跑，hybrid 后跑）═══"
# 种子正文按 doc/84 §0 的原始故事（「发布窗口改到周六凌晨」vs「周末什么时候上线」），
# 且与 query 无任何词面重叠（连「什么时候」都不出现）——对照组的漏检才是语义漏检，
# 不是种子自己把 BM25 的共现词递了刀。
DH_TEN="$TEN1"; DH_UID="$UID1"
dh_seed_run "$SID1" "$RID1" "'$WS1'" \
  "发布窗口定在几号？我要排值班" "发布窗口已定：改到周六凌晨三点，值班安排不用动" \
  "发布前还有哪些准备事项" "回归测试跑完就能收尾，周六凌晨见"
dh_wait_jobs_settled "$RID1" 180 && ok "S1 索引任务收敛（done）" || no "S1 索引任务 180s 未收敛"
dh_wait_query_hit "$BASE_A" "$TOK1" "$WS1" "周末什么时候上线" "$RID1" 120 \
  && ok "hybrid 端到端：查询「周末什么时候上线」命中 S1" || no "hybrid 120s 未命中 S1"

# 对照臂先行：pg_search 控制臂必须语义漏检
B_MISS_BLOCK="$(dh_recall_block "$BASE_B" "$TOK1" "周末什么时候上线")"
B_MISS_HITS="$(dh_recall_hits "$B_MISS_BLOCK")"
B_MISS_BLK_TXT="$(dh_recall_block_text "$B_MISS_BLOCK")"
echo "$B_MISS_BLK_TXT" | grep -q '<dialogue-context>' && B_MISS_BLOCK_HIT=1 || B_MISS_BLOCK_HIT=0
eq "$B_MISS_HITS" "0" "pg_search 对照臂：同查询 dialogueHits（语义漏检）"
eq "$B_MISS_BLOCK_HIT" "0" "pg_search 对照臂：无 <dialogue-context> 注入块"
[ "$B_MISS_HITS" != "0" ] && registered "S1 对照臂意外命中（BM25 分词跨过了语义差）——按实际结果对质，不算通过"

# 对照臂阳性：原文词查询必须能找到（证明漏检是语义问题，不是索引缺失）
B_POS_BLOCK="$(dh_recall_block "$BASE_B" "$TOK1" "发布窗口 周六凌晨")"
B_POS_HITS="$(dh_recall_hits "$B_POS_BLOCK")"
if [ "${B_POS_HITS:-0}" -ge 1 ]; then ok "pg_search 对照臂阳性：原文词查询命中（索引本身正常）"
else no "pg_search 对照臂阳性失败：原文词都找不到，对照无效，需排查"; fi

# hybrid 臂：向量臂与合并都命中
dh_q "$BASE_A" "$TOK1" "$WS1" "周末什么时候上线" > "$SCRATCH/s1_merged.txt"
grep -qw "$RID1" "$SCRATCH/s1_merged.txt" && ok "hybrid 合并结果含 S1" || no "hybrid 合并缺 S1"
if python3 - "$SCRATCH/last_query.json" "$RID1" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
vec=[m["runId"] for m in (d.get("data") or {}).get("vector") or []]
sys.exit(0 if sys.argv[2] in vec else 1)
PY
then ok "hybrid 向量臂命中 S1（语义增益本体：pg_search 漏检的同一查询）"
else no "向量臂未命中 S1"; fi

echo ""
echo "═══ 场景 2：跨会话可查；跨项目/跨租户/伪造不可见 ═══"
dh_seed_run "$SID2" "$RID2" "'$WS1'" \
  "临时目录堆积太多怎么处理" "清理脚本的保留策略改成按天滚动，tmp 目录只留最近 7 天" \
  "会不会误删" "白名单里的目录不删，其余按天清"
dh_wait_query_hit "$BASE_A" "$TOK1" "$WS1" "临时文件保留多久" "$RID2" 120 \
  && ok "跨会话：ws1 第二场会话 S2 可查到" || no "S2 120s 未命中（跨会话查询失败）"

dh_seed_run "$SID3" "$RID3" "'$WS2'" \
  "校准参数记在哪" "Zephyrion 校准参数记录在北极档案库，只有值班长能查" \
  "谁有权限" "值班长角色可读，其余人只看摘要"
dh_wait_query_hit "$BASE_A" "$TOK1" "$WS2" "Zephyrion 校准" "$RID3" 120 \
  && ok "跨项目归属：ws2 的 S3 在 ws2 可查" || no "S3 120s 未命中（ws2 可见性失败）"
dh_wait_query_clear "$BASE_A" "$TOK1" "$WS1" "Zephyrion 校准" "$RID3" 60 \
  && ok "跨项目不可见：ws1 查不到 ws2 的 S3" || no "ws1 查到了 ws2 的 S3（越权泄漏）"

# S4 是租户2 的数据：dh_seed_run 用 DH_TEN/DH_UID 全局落行，必须先切到租户2。
# 缺这行会把 S4 种成租户1 的行——TOK2 永远查不到，而「租户1 不可见」变虚绿（live4 实测）。
DH_TEN="$TEN2"; DH_UID="$UID2"
dh_seed_run "$SID4" "$RID4" "'$WS3'" \
  "租户二的校准参数在哪" "Zephyrion 校准参数在租户二档案库 v2 目录" \
  "能同步到租户一吗" "租户间数据隔离，不同步"
dh_wait_query_hit "$BASE_A" "$TOK2" "$WS3" "Zephyrion 校准" "$RID4" 120 \
  && ok "跨租户归属：租户2 在 ws3 查到 S4" || no "S4 120s 未命中（租户2 查询失败）"
dh_wait_query_clear "$BASE_A" "$TOK1" "$WS1" "Zephyrion 校准" "$RID4" 60 \
  && ok "跨租户不可见：租户1 查不到租户2 的 S4" || no "租户1 查到了租户2 的 S4（租户越权）"

V1="$(dh_http_code_at "$BASE_A" "$TOK1" GET "/api/workspaces/$WS3/files")"
eq "$V1" "404" "租户1 GET 租户2 工作区（API 层隔离）"
FAKE_BODY="$(dh_api_at "$BASE_A" "$TOK1" POST "/api/dev/dialogue-retrieval/query" \
  '{"query":"周末什么时候上线","workspaceId":"114001"}')"
FAKE_CODE="$(echo "$FAKE_BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])' 2>/dev/null)"
eq "$FAKE_CODE" "114001" "伪造 workspaceId=114001 → 404 114001"

echo ""
echo "═══ 场景 4：来源标注 + 断链 fail-closed（Milvus 零命中 → 整块不发）═══"
# recall 预览端点没有 run 上下文 → 只匹配 workspace_id 为空的 chunk（代码行为），用无归属种子。
DH_TEN="$TEN1"; DH_UID="$UID1"   # 回到租户1：S5 的无归属种子要落在租户1 名下
dh_seed_run "$SID5" "$RID5" "NULL" \
  "港口装载顺序怎么定" "Normandie 港口的装载顺序按涨潮时间排，提前 2 小时封区" \
  "封区期间怎么办" "封区期间走备用泊位，值班室留钥匙"
dh_wait_jobs_settled "$RID5" 180 && ok "S5（无项目归属）索引任务收敛" || no "S5 180s 未收敛"
RECALL1="$(dh_recall_block "$BASE_A" "$TOK1" "Normandie 装载顺序")"
S4_HITS="$(dh_recall_hits "$RECALL1")"
RECALL1_BLK="$(dh_recall_block_text "$RECALL1")"
if [ "${S4_HITS:-0}" -ge 1 ]; then ok "recall 端点 dialogueHits≥1（向量臂命中无归属 run）"
else no "recall dialogueHits=$S4_HITS，S5 未被召回"; fi
echo "$RECALL1_BLK" | grep -q '<dialogue-context>' && ok "注入块存在" || no "注入块缺失"
echo "$RECALL1_BLK" | grep -q '仅供参考' && ok "引用块带「仅供参考」边界声明" || no "缺「仅供参考」声明"
echo "$RECALL1_BLK" | grep -q "会话 $SID5" && ok "引用行带会话坐标" || no "引用行缺会话坐标"
echo "$RECALL1_BLK" | grep -q "run $RID5" && ok "引用行带 run 坐标" || no "引用行缺 run 坐标"
echo "$RECALL1_BLK" | grep -qE '\- \(20[0-9]{2}-[0-9]{2}-[0-9]{2}' && ok "引用行带日期（run 定稿时间）" || no "引用行缺日期"

echo "  -- 注入故障：删掉 S5 的 Milvus 行（ES 保持完好）--"
dh_eval delete -collection "$DH_MILVUS_COLL" -run-ids "$RID5" | sed 's/^/    /'
# Zilliz 冷启动会让 delete/count 瞬时 DeadlineExceeded（live4 实测：delete 根本没执行，
# 后面三查全成假红）。有界轮询等删除真生效（30s/60s 未生效就重删——delete 幂等）；
# 90s 仍删不掉就登记不可测——「链路没断成」不能当成「fail-closed 失效」来记。
MIL_GONE=0
for i in $(seq 1 45); do
  [ "$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID5\"" 2>/dev/null)" = "0" ] && { MIL_GONE=1; break; }
  if [ "$i" = 15 ] || [ "$i" = 30 ]; then
    dh_eval delete -collection "$DH_MILVUS_COLL" -run-ids "$RID5" >/dev/null 2>&1
  fi
  sleep 2
done
if [ "$MIL_GONE" = 1 ]; then
  ok "Milvus 行已删（S5 外部向量文档清空）"
  RECALL2="$(dh_recall_block "$BASE_A" "$TOK1" "Normandie 装载顺序")"
  S4_HITS2="$(dh_recall_hits "$RECALL2")"
  eq "${S4_HITS2:-x}" "0" "断链后 dialogueHits=0（向量臂零命中）"
  RECALL2_BLK="$(dh_recall_block_text "$RECALL2")"
  echo "$RECALL2_BLK" | grep -q '<dialogue-context>' && no "断链后仍有注入块（fail-closed 被破坏）" \
    || ok "断链后整块不发（fail-closed）：无 <dialogue-context>"
  dh_q "$BASE_A" "$TOK1" "$WS1" "周末什么时候上线" >/dev/null
  if python3 - "$SCRATCH/last_query.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))["data"]
lex=d.get("lexical") or []; vec=d.get("vector") or []; mg=d.get("merged") or []
sys.exit(0 if len(lex)>=1 and len(vec)==0 and len(mg)==0 else 1)
PY
  then ok "dev query 同步确认：lex 有候选 / vector 空 / merged 空（断链 fail-closed）"
  else no "dev query 断链行为不符"; fi
else
  registered "断链注入未生效（Zilliz 冷启动 delete 90s 未收敛）——fail-closed 三查不可测，登记跳过"
fi

echo "  -- 恢复：重置 S5 的 milvus 任务（幂等重写路径）--"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now()
  WHERE run_id='$RID5' AND target='milvus'"
dh_wait_jobs_settled "$RID5" 120 && ok "S5 milvus 任务重新收敛" || no "S5 milvus 重投 120s 未收敛"
RECALL3="$(dh_recall_block "$BASE_A" "$TOK1" "Normandie 装载顺序")"
S4_HITS3="$(dh_recall_hits "$RECALL3")"
[ "${S4_HITS3:-0}" -ge 1 ] && ok "恢复后引用块回来了（幂等重写不产生第二份）" \
  || no "恢复后 recall 仍为 0"
dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID5\"" \
  > "$SCRATCH/s4_mil.txt" 2>/dev/null
eq "$(cat "$SCRATCH/s4_mil.txt" 2>/dev/null)" "1" "S5 在 Milvus 恰好 1 行（无重复外键文档）"

echo ""
echo "═══ 场景 5：重复投递 / 重复消费 / 乱序旧版本 → 幂等收敛 ═══"
# wait_pub <run_id> <timeout>：等 relay 把 published_at 重新盖回（5s 一轮）。
wait_pub() {
  local rid="$1" t="${2:-60}" w=0
  while [ "$w" -lt "$t" ]; do
    [ "$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$rid' AND published_at IS NULL")" = "0" ] && return 0
    sleep 2; w=$((w+2))
  done
  return 1
}
dh_seed_run "$SID6" "$RID6" "'$WS1'" \
  "灰度放量怎么排" "灰度发布的放量节奏按每小时 10% 递增，异常回滚到 0" \
  "回滚谁执行" "值班长一键回滚，5 分钟内完成"
dh_wait_jobs_settled "$RID6" 180 && ok "S6 索引收敛" || no "S6 180s 未收敛"

echo "  -- 5a 重复投递：把已 done 的任务重新标未发布，relay 会再投一遍 --"
dh_sql "UPDATE dialogue_index_jobs SET published_at=NULL WHERE run_id='$RID6'"
wait_pub "$RID6" 60 && ok "relay 已重投（published_at 盖回）" || no "relay 60s 未重投"
dh_wait_jobs_settled "$RID6" 60
es6="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID6"'"}}}')"
# 消费者重写有窗口 + 混模式竞态可能让登记晚到（对账 10s 兜底）：有界轮询等计数回到 1
# 再断言，不睡一觉赌收敛。
mil6=""; w5a=0
while [ "$w5a" -lt 75 ]; do
  mil6="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID6\"" 2>/dev/null)"
  es6="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID6"'"}}}')"
  [ "$mil6" = "1" ] && [ "$es6" = "1" ] && break
  sleep 3; w5a=$((w5a+3))
done
eq "$es6" "1" "重复投递后 ES 的 S6 文档仍 1 份"
eq "$mil6" "1" "重复投递后 Milvus 的 S6 行仍 1 行"

echo "  -- 5b 重复消费：整条重做（pending 重置）→ 幂等 upsert，外部文档不增长 --"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID6'"
dh_wait_jobs_settled "$RID6" 180 || no "S6 重做 180s 未收敛"
# dh_eval 计数在 Zilliz 冷启动会瞬时返回空（live4 实测 got=[] 假红，行真身在）——有界轮询。
es6b=""; mil6b=""; w5b=0
while [ "$w5b" -lt 75 ]; do
  es6b="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID6"'"}}}')"
  mil6b="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID6\"" 2>/dev/null)"
  [ "$es6b" = "1" ] && [ "$mil6b" = "1" ] && break
  sleep 3; w5b=$((w5b+3))
done
eq "$es6b" "1" "重做后 ES 仍 1 份"
eq "$mil6b" "1" "重做后 Milvus 仍 1 行"
es6v="$(dh_es_count "$DH_ES_INDEX" '{"query":{"bool":{"filter":[{"term":{"run_id":"'"$RID6"'"}},{"term":{"index_version":"1"}}]}}}')"
eq "$es6v" "1" "ES 文档 index_version 仍为当前版本 1"

echo "  -- 5c 乱序旧版本：index_version=0 的旧任务重投 → 忽略不落盘 --"
dh_seed_run "$SID7" "$RID7" "'$WS1'" \
  "看板埋点怎么命名" "看板的埋点字段用下划线命名，事件名小写中划线" \
  "历史字段呢" "历史驼峰字段保留，新埋点全走新规"
dh_wait_jobs_settled "$RID7" 180 && ok "S7 索引收敛" || no "S7 180s 未收敛"
dh_sql "UPDATE dialogue_index_jobs SET index_version=0, status='pending', published_at=NULL,
        done_at=NULL, lease_until=NULL, next_attempt_at=now() WHERE run_id='$RID7'"
dh_wait_jobs_settled "$RID7" 180 || no "S7 旧版本重投 180s 未收敛"
es7="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"run_id":"'"$RID7"'"}}}')"
eq "$es7" "1" "乱序旧版本重投后 ES 仍 1 份（旧版本没落盘）"
es7v="$(dh_es_count "$DH_ES_INDEX" '{"query":{"bool":{"filter":[{"term":{"run_id":"'"$RID7"'"}},{"term":{"index_version":"0"}}]}}}')"
eq "$es7v" "0" "ES 无 index_version=0 的文档（旧版本被丢弃）"
LOG_A="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/eng_a.log"
LOG_B="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/eng_b.log"
{ grep -q '忽略非当前版本任务' "$LOG_A" 2>/dev/null || grep -q '忽略非当前版本任务' "$LOG_B" 2>/dev/null; } \
  && ok "引擎日志出现「忽略非当前版本任务」（版本守卫实跑）" \
  || no "引擎日志未见版本守卫记录"
es_all_v0="$(dh_es_count "$DH_ES_INDEX" '{"query":{"term":{"index_version":"0"}}}')"
eq "$es_all_v0" "0" "全索引无任何 v0 文档（最终只有当前版本有效）"

echo ""
echo "═══ 场景 6：ES / embedding / Milvus 故障 → 退避、死信、119001–119004、恢复 ═══"
# 主环境引擎停掉（把消费组让给故障引擎），故障环境独立 PG 库/独立索引/独立 collection。
bash "$UP" stop "$ENV_MAIN" --tag a | sed 's/^/  /'

FAULT_STATE="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_FAULT/state.env"
# shellcheck disable=SC1090
source "$FAULT_STATE"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"

ES_PROXY_PORT="${DH_ES_PROXY_PORT:-7931}"; EMB_PROXY_PORT="${DH_EMB_PROXY_PORT:-7932}"
MIL_DEAD_PORT="${DH_MIL_DEAD_PORT:-9391}"
ES_UPSTREAM="$(grep -E '^TAKO_ES_URL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
EMB_UPSTREAM="$(grep -E '^TAKO_DIALOGUE_EMBEDDING_BASE_URL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
for p in "$ES_PROXY_PORT" "$EMB_PROXY_PORT"; do
  lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && fail_fast "代理端口 $p 被占，换 DH_*_PROXY_PORT"
done
python3 "$ROOT/scripts/acceptance/dialogue_hybrid_chaos_proxy.py" http \
  --listen "127.0.0.1:$ES_PROXY_PORT" --upstream "$ES_UPSTREAM" \
  --fault-file "$SCRATCH/es_fault.txt" --log "$SCRATCH/es_proxy.log" \
  >"$SCRATCH/es_proxy.out" 2>&1 </dev/null & PIDS_TO_KILL+=($!)
python3 "$ROOT/scripts/acceptance/dialogue_hybrid_chaos_proxy.py" http \
  --listen "127.0.0.1:$EMB_PROXY_PORT" --upstream "$EMB_UPSTREAM" \
  --fault-file "$SCRATCH/emb_fault.txt" --log "$SCRATCH/emb_proxy.log" \
  >"$SCRATCH/emb_proxy.out" 2>&1 </dev/null & PIDS_TO_KILL+=($!)
echo "pass" > "$SCRATCH/es_fault.txt"; echo "pass" > "$SCRATCH/emb_fault.txt"
sleep 1
curl -s -o /dev/null "http://127.0.0.1:$ES_PROXY_PORT" && ok "ES 故障代理起（pass 模式）" || no "ES 代理未监听"
curl -s -o /dev/null "http://127.0.0.1:$EMB_PROXY_PORT" && ok "embedding 故障代理起（pass 模式）" || no "embedding 代理未监听"

bash "$UP" start "$ENV_FAULT" --tag c --port "$PORT_C" --redis-db "$RDB_C" --mode hybrid \
  --es-url "http://127.0.0.1:$ES_PROXY_PORT" --embed-base "http://127.0.0.1:$EMB_PROXY_PORT" \
  | sed 's/^/  /' || fail_fast "故障引擎 C 起不来"
dh_signup "$BASE_C" t3
WS4="$(dh_mk_ws "$BASE_C" "$DH_TOK" dh-ws4-fault)"
SID9="s${ENV_FAULT}9"; RID9="r${ENV_FAULT}9"
dh_seed_run "$SID9" "$RID9" "'$WS4'" \
  "算子挂了怎么办" "算子的退化回退开关在配置中心 hot key 上，拨到 0 即回退上一版" \
  "回退要多久" "拨开关 30 秒生效"
dh_wait_query_hit "$BASE_C" "$DH_TOK" "$WS4" "算子回退开关" "$RID9" 180 \
  && ok "健康链路（经代理 pass）：S9 全链真跑通" || no "S9 180s 未命中（代理链路故障？）"

echo "  -- 6a ES 503：瞬时退避（es_unavailable）--"
echo "503" > "$SCRATCH/es_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID9' AND target='es'"
wait_es_class() { # wait_es_class <target> <class> <timeout>
  local tgt="$1" cls="$2" t="${3:-90}" w=0
  while [ "$w" -lt "$t" ]; do
    row="$(dh_sql "SELECT status||'|'||coalesce(last_error,'')||'|'||attempts FROM dialogue_index_jobs
            WHERE run_id='$RID9' AND target='$tgt'")"
    echo "$row" | grep -q "$cls" && { echo "$row"; return 0; }
    sleep 2; w=$((w+2))
  done
  echo "$row"; return 1
}
ES_ROW="$(wait_es_class es 'es_unavailable' 90)"
echo "$ES_ROW" | grep -q 'es_unavailable' && ok "ES 503 → last_error=es_unavailable（机器可读分类）" \
  || no "es_unavailable 未出现：[$ES_ROW]"
ES_NA="$(dh_sql "SELECT next_attempt_at > now() FROM dialogue_index_jobs
                 WHERE run_id='$RID9' AND target='es'")"
eq "$ES_NA" "t" "退避已挂：next_attempt_at 在未来（1 分钟档）"
# 查询端 119001：ES 臂挂时 dev query 必须 503 119001
Q_ES="$(curl -s -w '\n%{http_code}' -X POST "$BASE_C/api/dev/dialogue-retrieval/query" \
  -H "Authorization: Bearer $DH_TOK" -H 'Content-Type: application/json' \
  -d '{"query":"算子回退开关","workspaceId":"'"$WS4"'"}')"
Q_ES_HTTP="$(echo "$Q_ES" | tail -1)"; Q_ES_CODE="$(echo "$Q_ES" | head -1 | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])' 2>/dev/null)"
eq "$Q_ES_HTTP" "503" "ES 臂挂 → HTTP 503"; eq "$Q_ES_CODE" "119001" "ES 臂挂 → code 119001"
Q_HTTP_119001="$Q_ES_HTTP/$Q_ES_CODE"

echo "  -- 6b ES 400：永久死信（es_permanent，不重试）--"
echo "400" > "$SCRATCH/es_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID9' AND target='es'"
ES_ROW="$(wait_es_class es 'es_permanent' 90)"
echo "$ES_ROW" | grep -q 'failed|es_permanent|1' && ok "ES 400 → es_permanent 直接 failed（死信，0 退避）" \
  || no "es_permanent 死信不符：[$ES_ROW]"
COV_JSON="$(dh_api_at "$BASE_C" "$DH_TOK" GET "/api/dev/dialogue-retrieval/status")"
COV_FAILED="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("failed"))' 2>/dev/null)"
[ "${COV_FAILED:-0}" -ge 1 ] && ok "status 覆盖率真实反映死信：coverage.failed≥1（=$COV_FAILED）" \
  || no "coverage.failed=$COV_FAILED 与 SQL 死信不符"
COV_SEEN_FAILED="$COV_FAILED"

echo "  -- 6c ES 恢复：故障撤除 + 死信重置 → 收敛（人工干预路径）--"
echo "pass" > "$SCRATCH/es_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, last_error='', next_attempt_at=now() WHERE run_id='$RID9' AND target='es'"
dh_wait_jobs_settled "$RID9" 120 && ok "ES 任务恢复收敛" || no "ES 恢复 120s 未收敛"
dh_q "$BASE_C" "$DH_TOK" "$WS4" "算子回退开关" >/dev/null
grep -q '"code":0' "$SCRATCH/last_query.json" && ok "ES 恢复后查询正常（119001 消失）" || no "ES 恢复后查询仍红"

echo "  -- 6d embedding 500：瞬时（embedding_unavailable）+ 查询端 119003 --"
echo "500" > "$SCRATCH/emb_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID9' AND target='milvus'"
EMB_ROW="$(wait_es_class milvus 'embedding_unavailable' 90)"
echo "$EMB_ROW" | grep -q 'embedding_unavailable' && ok "embedding 500 → embedding_unavailable（瞬时退避）" \
  || no "embedding_unavailable 未出现：[$EMB_ROW]"
Q_EMB="$(curl -s -w '\n%{http_code}' -X POST "$BASE_C/api/dev/dialogue-retrieval/query" \
  -H "Authorization: Bearer $DH_TOK" -H 'Content-Type: application/json' \
  -d '{"query":"算子回退开关","workspaceId":"'"$WS4"'"}')"
Q_EMB_HTTP="$(echo "$Q_EMB" | tail -1)"; Q_EMB_CODE="$(echo "$Q_EMB" | head -1 | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])' 2>/dev/null)"
eq "$Q_EMB_HTTP" "503" "embedding 挂 → HTTP 503"; eq "$Q_EMB_CODE" "119003" "embedding 挂 → code 119003"
Q_HTTP_119003="$Q_EMB_HTTP/$Q_EMB_CODE"

echo "  -- 6e embedding 坏 JSON：永久（embedding_bad_response）--"
echo "garbage" > "$SCRATCH/emb_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID9' AND target='milvus'"
EMB_ROW="$(wait_es_class milvus 'embedding_bad_response' 90)"
echo "$EMB_ROW" | grep -q 'failed|embedding_bad_response|1' && ok "坏 JSON → embedding_bad_response 直接死信" \
  || no "embedding_bad_response 死信不符：[$EMB_ROW]"

echo "  -- 6f embedding 维度不符：永久（embedding_dimension_mismatch）+ 查询端 119004 --"
echo "dim8" > "$SCRATCH/emb_fault.txt"
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, next_attempt_at=now() WHERE run_id='$RID9' AND target='milvus'"
EMB_ROW="$(wait_es_class milvus 'embedding_dimension_mismatch' 90)"
echo "$EMB_ROW" | grep -q 'failed|embedding_dimension_mismatch|1' && ok "8 维假向量 → embedding_dimension_mismatch 死信" \
  || no "embedding_dimension_mismatch 不符：[$EMB_ROW]"
Q_DIM="$(curl -s -w '\n%{http_code}' -X POST "$BASE_C/api/dev/dialogue-retrieval/query" \
  -H "Authorization: Bearer $DH_TOK" -H 'Content-Type: application/json' \
  -d '{"query":"算子回退开关","workspaceId":"'"$WS4"'"}')"
Q_DIM_HTTP="$(echo "$Q_DIM" | tail -1)"; Q_DIM_CODE="$(echo "$Q_DIM" | head -1 | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])' 2>/dev/null)"
eq "$Q_DIM_HTTP" "409" "维度不符 → HTTP 409"; eq "$Q_DIM_CODE" "119004" "维度不符 → code 119004"
Q_HTTP_119004="$Q_DIM_HTTP/$Q_DIM_CODE"

echo "  -- 6g Milvus 永久缺件：运行中删 collection（写入持续失败 → 退避至死信）+ 查询端 119002 --"
echo "pass" > "$SCRATCH/emb_fault.txt"
dh_eval drop -collection "$DH_MILVUS_COLL" | sed 's/^/    /'
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, last_error='', next_attempt_at=now() WHERE run_id='$RID9' AND target='milvus'"
# doTransient 把 SDK 报错统一包成 milvus_unavailable（瞬时）——三次退避到死信；
# 逐次把 next_attempt_at 拉回现在，缩短 1/5/30 分钟三档的实跑等待。
MIL_ROW=""; w6g=0
while [ "$w6g" -lt 150 ]; do
  MIL_ROW="$(dh_sql "SELECT status||'|'||coalesce(last_error,'')||'|'||attempts FROM dialogue_index_jobs
          WHERE run_id='$RID9' AND target='milvus'")"
  case "$MIL_ROW" in
    failed*milvus_unavailable*) break ;;
  esac
  dh_sql "UPDATE dialogue_index_jobs SET next_attempt_at=now()
          WHERE run_id='$RID9' AND target='milvus' AND status='pending' AND attempts>0" >/dev/null
  sleep 2; w6g=$((w6g+2))
done
echo "$MIL_ROW" | grep -q 'failed|milvus_unavailable|3' \
  && ok "collection 被删 → 三次退避后死信（failed|milvus_unavailable|3，写入侧缺件按瞬时分类）" \
  || no "Milvus 死信形态不符：[$MIL_ROW]"
Q_MIL="$(curl -s -w '\n%{http_code}' -X POST "$BASE_C/api/dev/dialogue-retrieval/query" \
  -H "Authorization: Bearer $DH_TOK" -H 'Content-Type: application/json' \
  -d '{"query":"算子回退开关","workspaceId":"'"$WS4"'"}')"
Q_MIL_HTTP="$(echo "$Q_MIL" | tail -1)"; Q_MIL_CODE="$(echo "$Q_MIL" | head -1 | python3 -c 'import json,sys;print(json.load(sys.stdin)["code"])' 2>/dev/null)"
eq "$Q_MIL_HTTP" "503" "Milvus 臂挂 → HTTP 503"; eq "$Q_MIL_CODE" "119002" "Milvus 臂挂 → code 119002"
Q_HTTP_119002="$Q_MIL_HTTP/$Q_MIL_CODE"

echo "  -- 6h Milvus 恢复：重启引擎（EnsureCollection 重建）+ 死信重置 → 收敛 --"
bash "$UP" restart "$ENV_FAULT" c | sed 's/^/  /'
dh_sql "UPDATE dialogue_index_jobs SET status='pending', published_at=NULL, done_at=NULL,
        lease_until=NULL, attempts=0, last_error='', next_attempt_at=now() WHERE run_id='$RID9' AND target='milvus'"
dh_wait_jobs_settled "$RID9" 180 && ok "Milvus 重建后任务收敛" || no "Milvus 恢复 180s 未收敛"
mil9="$(dh_eval count -collection "$DH_MILVUS_COLL" -expr "run_id == \"$RID9\"" 2>/dev/null)"
eq "$mil9" "1" "恢复后 Milvus 的 S9 恰好 1 行"
dh_wait_query_hit "$BASE_C" "$DH_TOK" "$WS4" "算子回退开关" "$RID9" 60 \
  && ok "全链路恢复：查询重新命中" || no "恢复后查询未命中"

echo "  -- 6i Milvus 不可用：启动预检挡下（引擎拒起）--"
bash "$UP" stop "$ENV_FAULT" --tag c >/dev/null
if bash "$UP" start "$ENV_FAULT" --tag d --port "$((PORT_C+1))" --redis-db "$((RDB_C+1))" --mode hybrid \
     --milvus-uri "127.0.0.1:$MIL_DEAD_PORT" >/dev/null 2>&1; then
  no "Milvus 指向死端口竟然起成功了（预检失效）"
  bash "$UP" stop "$ENV_FAULT" --tag d >/dev/null
else
  ok "Milvus 不可用 → 启动预检失败，引擎拒起（fail-fast）"
fi
rm -f "${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_FAULT/eng_d.pid" 2>/dev/null
bash "$UP" start "$ENV_FAULT" --tag c --port "$PORT_C" --redis-db "$RDB_C" --mode hybrid \
  --es-url "http://127.0.0.1:$ES_PROXY_PORT" --embed-base "http://127.0.0.1:$EMB_PROXY_PORT" \
  | sed 's/^/  /' || fail_fast "故障引擎 C 重启失败"

echo ""
echo "═══ 场景 10：可观测（覆盖率真实 / 8 span / 无正文无密钥）═══"
COV_JSON="$(dh_api_at "$BASE_C" "$DH_TOK" GET "/api/dev/dialogue-retrieval/status")"
COV_PENDING="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("pending"))' 2>/dev/null)"
COV_RETRY="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("retry"))' 2>/dev/null)"
COV_LAG="$(echo "$COV_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["coverage"]).get("consumerLag"))' 2>/dev/null)"
COV_SQL="$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE status='pending'")"
eq "$COV_PENDING" "$COV_SQL" "status 覆盖率与 SQL 真值一致（pending）"
[ "${COV_LAG:-1}" = "0" ] && ok "消费积压归零（consumerLag=0）" || no "consumerLag=$COV_LAG 未归零"

SPANS_OK=1
for sp in dialogue.outbox.relay dialogue.index.es dialogue.index.embedding dialogue.index.milvus \
          dialogue.recall.es dialogue.recall.embedding dialogue.recall.milvus dialogue.recall.rrf; do
  n="$(dh_lf_span "$sp" "$T0_ISO")"
  if [ "${n:-0}" -ge 1 ]; then ok "trace span $sp ×$n"
  else no "trace 缺 span $sp"; SPANS_OK=0; fi
done

LOG_C="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_FAULT/eng_c.log"
SECRET_EMB="$(grep -E '^TAKO_DIALOGUE_EMBEDDING_API_KEY=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
SECRET_MIL="$(grep -E '^TAKO_MILVUS_TOKEN=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
LOG_CLEAN=1
# 只挑「只出现在对话正文、从不进任何请求 URL」的短语。GIN 访问日志会回显脚本自己发的
# q= 查询串（live4 的 Normandie 红就是它——查询词与种子词撞了，不是正文泄漏）；
# 正文专属词 0 命中才算干净。
for lg in "$LOG_A" "$LOG_C"; do
  for pat in "值班安排不用动" "备用泊位" "值班室留钥匙" "每小时 10% 递增" "拨开关 30 秒生效" \
             "$DH_ES_PASS" "$SECRET_EMB" "$SECRET_MIL" "$DH_LF_SK"; do
    [ -z "$pat" ] && continue
    if [ -f "$lg" ] && grep -q -- "$pat" "$lg" 2>/dev/null; then
      no "日志泄漏 [$pat] → $lg"; LOG_CLEAN=0
    fi
  done
done
[ "$LOG_CLEAN" = 1 ] && ok "两台引擎日志无正文、无 ES/embedding/Milvus/Langfuse 密钥"
Q_RAW="$(cat "$SCRATCH/last_query.json")"
echo "$Q_RAW" | grep -q '算子的退化回退开关' && no "query 响应携带正文" \
  || ok "dev query 响应只有坐标与分数（无正文）"

# 汇总
echo ""
echo "场景 10 采点：119001=$Q_HTTP_119001 119002=$Q_HTTP_119002 119003=$Q_HTTP_119003 119004=$Q_HTTP_119004"
registered "场景 3（active 与原文分离）需真模型，DEEPSEEK 402 环境限制，不伪造（见 p5-交付.md）"
registered "场景 6 embedding_over_limit 不可达：chunk 上限 4000 rune < embedding 6000 token 上限（设计使然）"
registered "场景 6 milvus_unavailable 的「运行中 TCP 断链」注入不可行（SDK TLS 证书校验钉死真域名，本地代理必握手失败）；已用两条替代路径覆盖：启动预检拒起（6i）+ 运行中删 collection 走 milvus_unavailable 三退避死信（6g）"
dh_summary
