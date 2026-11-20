#!/usr/bin/env bash
# dialogue_hybrid_rebuild_live.sh — 历史对话混合检索（doc/84）重建组活体验收：场景 9
#   9a v1 现状：基名索引/collection + 指针 (v1, 基名, 基名)
#   9b 停引擎 → -rebuild-dialogue-retrieval 全量重放 → v2 对象 + 指针切 v2
#   9c 旧版本保留（回滚保险，绝不删）
#   9d 重跑收敛：退出码 0、不建 v3
#   9e 重启副本绑 v2（env 传 _v2 名字；客户端启动期绑定）→ status 服务面 = v2
#   9f 重启后新增 run 只进 v2（v1 无此文档）→ 语义查询命中
#   9g 登记观察：重启不传 _v2 覆盖时，EnsureDialogueRetrievalPointer 会把指针名字
#      无条件盖回 env 基名（版本号不动）——运维契约「重启副本重绑」只靠纪律保证，
#      这一步演示该脚枪，登记给主 Agent 终审。
#
# 全链路真跑：SQL 造 status='complete' 的 run（不打模型，绕开 DEEPSEEK 402），引擎自己的
# 索引循环走 chunk → outbox → relay → 双消费 → ES/Zilliz（embedding 真调）；重建由
# cmd/yktako -rebuild-dialogue-retrieval 真跑（embedding 真探针 + 真回放 + 真校验）。
#
# 用法：bash scripts/acceptance/dialogue_hybrid_rebuild_live.sh
# 参数化：DH_ENV_NAME、DH_PORT_R、DH_REDIS_DB_R、TAKO_ENV、TAKO_PSQL、TAKO_PG_HOST/PORT。
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
PORT_R="${DH_PORT_R:-8948}"
RDB_R="${DH_REDIS_DB_R:-12}"
ENV_MAIN="${DH_ENV_NAME:-rebuild$(date +%m%d%H%M%S)}"

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/dh-rebuild.XXXXXX")"
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
"$PSQL" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -qAt -c "SELECT 1" >/dev/null 2>&1 \
  || fail_fast "psql 连不上 $PG_HOST:$PG_PORT"
dh_es_conf "$ENV_FILE"
[ -n "$DH_ES_URL" ] || fail_fast ".env 缺 TAKO_ES_URL"
curl -s -o /dev/null -u "$DH_ES_USER:$DH_ES_PASS" "$DH_ES_URL" || fail_fast "ES 不可达"
echo "✓ PG/ES/.env 就绪"

echo ""
echo "===== 起环境（init + 引擎 hybrid，基名索引/collection）====="
bash "$UP" init "$ENV_MAIN" | sed 's/^/  /' || fail_fast "init 失败"
STATE_MAIN="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/state.env"
# shellcheck disable=SC1090
source "$STATE_MAIN"
DH_PG_DSN="$DH_PG_DSN"; DH_WS="$DH_WS"; DH_ES_INDEX="$DH_ES_INDEX"; DH_MILVUS_COLL="$DH_MILVUS_COLL"; DH_EVAL_BIN="$DH_EVAL_BIN"
BASE_ES="$DH_ES_INDEX"; BASE_MIL="$DH_MILVUS_COLL"
V2_ES="${BASE_ES}_v2"; V2_MIL="${BASE_MIL}_v2"
REBUILD_BIN="${TMPDIR:-/tmp}/tako-dialogue-eval/$ENV_MAIN/yktako"

BASE_R="http://127.0.0.1:$PORT_R"
bash "$UP" start "$ENV_MAIN" --tag a --port "$PORT_R" --redis-db "$RDB_R" --mode hybrid | sed 's/^/  /' \
  || fail_fast "引擎起不来"

# 跑一轮重建命令（复用 up.sh 编译好的引擎二进制；-rebuild-dialogue-retrieval
# 不起 HTTP/worker，只连 PG + ES/Milvus/embedding）。
dh_rebuild() { # dh_rebuild <日志文件>
  (
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
    export TAKO_DIALOGUE_RETRIEVAL=hybrid
    export TAKO_ES_INDEX="$BASE_ES"
    export TAKO_MILVUS_COLLECTION="$BASE_MIL"
    exec "$REBUILD_BIN" -rebuild-dialogue-retrieval -pg "$DH_PG_DSN"
  ) > "$1" 2>&1
}

dh_signup "$BASE_R" r1; TOK="$DH_TOK"
WS1="$(dh_mk_ws "$BASE_R" "$TOK" dh-ws-rebuild)"
[ -n "$WS1" ] && ok "账号 + 工作区($WS1)" || { no "建工作区失败"; exit 1; }

SID1="s${ENV_MAIN}1"; RID1="r${ENV_MAIN}1"
SID2="s${ENV_MAIN}2"; RID2="r${ENV_MAIN}2"
SID3="s${ENV_MAIN}3"; RID3="r${ENV_MAIN}3"
dh_seed_run "$SID1" "$RID1" "'$WS1'" \
  "发布窗口定在几号？我要排值班" "发布窗口已定：改到周六凌晨三点，值班安排不用动" \
  "发布前还有哪些准备事项" "回归测试跑完就能收尾，周六凌晨见"
dh_seed_run "$SID2" "$RID2" "'$WS1'" \
  "临时目录堆积太多怎么处理" "清理脚本的保留策略改成按天滚动，tmp 目录只留最近 7 天" \
  "会不会误删" "白名单里的目录不删，其余按天清"
dh_seed_run "$SID3" "$RID3" "'$WS1'" \
  "港口装载顺序怎么定" "Normandie 港口的装载顺序按涨潮时间排，提前 2 小时封区" \
  "封区期间怎么办" "封区期间走备用泊位，值班室留钥匙"
for rid in "$RID1" "$RID2" "$RID3"; do
  dh_wait_jobs_settled "$rid" 180 && ok "$rid 索引收敛" || no "$rid 180s 未收敛"
done

echo ""
echo "═══ 场景 9a：v1 现状（基名对象 + 指针 v1 非版本化形态）═══"
eq "$(dh_es_count "$BASE_ES")" "3" "v1 ES 文档数 = 3"
mil1="$(dh_eval count -collection "$BASE_MIL" 2>/dev/null)"
eq "$mil1" "3" "v1 Milvus 行数 = 3"
PTR="$(dh_sql "SELECT index_version||'|'||es_index||'|'||milvus_collection FROM dialogue_retrieval_pointer WHERE id=1")"
eq "$PTR" "1|$BASE_ES|$BASE_MIL" "指针 = (v1, 基名, 基名)——非版本化形态，首轮重建必建 v2"

echo ""
echo "═══ 场景 9b：停引擎 → 全量重建 → v2 对象 + 指针切换 ═══"
bash "$UP" stop "$ENV_MAIN" --tag a | sed 's/^/  /'
echo "  -- $REBUILD_BIN -rebuild-dialogue-retrieval（真探针/真回放/真校验）--"
dh_rebuild "$SCRATCH/rebuild1.log"
rc1=$?
if [ "$rc1" = 0 ]; then
  ok "重建退出码 0"
  grep -E 'rebuild-dialogue-retrieval: ' "$SCRATCH/rebuild1.log" | sed 's/^/    /' | tail -8
else
  no "重建退出码 $rc1"
  tail -20 "$SCRATCH/rebuild1.log" | sed 's/^/    /'
fi
dh_es_exists "$V2_ES" && ok "ES v2 索引存在：$V2_ES" || no "ES v2 索引缺失"
[ "$(dh_mil_has "$V2_MIL" 2>/dev/null)" = "true" ] && ok "Milvus v2 collection 存在：$V2_MIL" \
  || no "Milvus v2 collection 缺失"
eq "$(dh_es_count "$V2_ES")" "3" "v2 ES 文档数 = 3（全量重放）"
milv2="$(dh_eval count -collection "$V2_MIL" 2>/dev/null)"
eq "$milv2" "3" "v2 Milvus 行数 = 3（全量重放）"
PTR="$(dh_sql "SELECT index_version||'|'||es_index||'|'||milvus_collection FROM dialogue_retrieval_pointer WHERE id=1")"
eq "$PTR" "2|$V2_ES|$V2_MIL" "指针一笔事务切到 (v2, _v2, _v2)"

echo ""
echo "═══ 场景 9c：旧版本保留（回滚保险）═══"
dh_es_exists "$BASE_ES" && ok "旧 ES 索引保留：$BASE_ES" || no "旧 ES 索引不见了（违背「绝不删旧版本」）"
[ "$(dh_mil_has "$BASE_MIL" 2>/dev/null)" = "true" ] && ok "旧 Milvus collection 保留：$BASE_MIL" \
  || no "旧 collection 不见了（违背「绝不删旧版本」）"
eq "$(dh_es_count "$BASE_ES")" "3" "旧 ES 索引内容原样（3 份）"

echo ""
echo "═══ 场景 9d：重跑收敛（指针已版本化 → 只增量校验，不建 v3）═══"
dh_rebuild "$SCRATCH/rebuild2.log"
rc2=$?
if [ "$rc2" = 0 ]; then
  ok "重跑退出码 0"
  grep -qE '已是活跃版本' "$SCRATCH/rebuild2.log" \
    && ok "日志确认走的是增量校验路径（不建不切）" \
    || registered "重跑退出码 0 但日志未见「已是活跃版本」字样，按实际日志对质"
else
  no "重跑退出码 $rc2"
  tail -20 "$SCRATCH/rebuild2.log" | sed 's/^/    /'
fi
PTR="$(dh_sql "SELECT index_version||'|'||es_index||'|'||milvus_collection FROM dialogue_retrieval_pointer WHERE id=1")"
eq "$PTR" "2|$V2_ES|$V2_MIL" "重跑后指针仍是 v2"
dh_es_exists "${BASE_ES}_v3" && no "竟然建了 v3 索引" || ok "无 v3 ES 索引"
[ "$(dh_mil_has "${BASE_MIL}_v3" 2>/dev/null)" = "true" ] && no "竟然建了 v3 collection" || ok "无 v3 collection"

echo ""
echo "═══ 场景 9e：重启副本绑 v2（env 传 _v2 名；客户端启动期绑定）═══"
# P3 契约「重启副本重绑」：运行时客户端从 env 绑名（dialogue_retrieval.go 装配），
# 指针只是代次登记——不传 _v2 覆盖重启，引擎仍会去查基名（v1）。
bash "$UP" start "$ENV_MAIN" --tag a --port "$PORT_R" --redis-db "$RDB_R" --mode hybrid \
  --es-index "$V2_ES" --milvus-coll "$V2_MIL" | sed 's/^/  /' || fail_fast "重启（v2 绑定）失败"
STATUS_JSON="$(dh_api_at "$BASE_R" "$TOK" GET "/api/dev/dialogue-retrieval/status")"
RT_ES="$(echo "$STATUS_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["lexical"] or {}).get("index") or "")' 2>/dev/null)"
RT_MIL="$(echo "$STATUS_JSON" | python3 -c 'import json,sys;print((json.load(sys.stdin)["data"]["vector"] or {}).get("collection") or "")' 2>/dev/null)"
eq "$RT_ES" "$V2_ES" "status 服务面 lexical.index = v2（运行时真绑了 v2）"
eq "$RT_MIL" "$V2_MIL" "status 服务面 vector.collection = v2"

echo ""
echo "═══ 场景 9f：重启后新增 run 只进 v2 → 语义查询命中 ═══"
SID4="s${ENV_MAIN}4"; RID4="r${ENV_MAIN}4"
dh_seed_run "$SID4" "$RID4" "'$WS1'" \
  "灰度放量怎么排" "灰度发布的放量节奏按每小时 10% 递增，异常回滚到 0" \
  "回滚谁执行" "值班长一键回滚，5 分钟内完成"
dh_wait_jobs_settled "$RID4" 180 && ok "RID4 索引收敛" || no "RID4 180s 未收敛"
eq "$(dh_es_count "$V2_ES" '{"query":{"term":{"run_id":"'"$RID4"'"}}}')" "1" "新 run 落进 v2 索引"
eq "$(dh_es_count "$BASE_ES" '{"query":{"term":{"run_id":"'"$RID4"'"}}}')" "0" "v1 索引没有新 run（服务面确实换了）"
dh_wait_query_hit "$BASE_R" "$TOK" "$WS1" "周末什么时候上线" "$RID1" 120 \
  && ok "重启后语义查询命中 S1（v2 里的旧数据可查）" || no "重启后 120s 未命中 S1"
dh_wait_query_hit "$BASE_R" "$TOK" "$WS1" "灰度异常了怎么办" "$RID4" 60 \
  && ok "重启后新 run 可查" || no "新 run 60s 未命中"

echo ""
echo "═══ 场景 9g（登记观察）：重启不传 _v2 覆盖 → 指针名字被盖回基名 ═══"
# EnsureDialogueRetrievalPointer 对已存在行无条件 UPDATE 名字（dialogue_index_jobs.go:107-110），
# 版本号不动——运维忘传覆盖时，指针会指着 v1 对象却报 v2。演示一次，登记不判过。
bash "$UP" stop "$ENV_MAIN" --tag a >/dev/null
bash "$UP" start "$ENV_MAIN" --tag a --port "$PORT_R" --redis-db "$RDB_R" --mode hybrid >/dev/null 2>&1 \
  && echo "  （基名引擎已起，只用于观察指针行）"
PTR="$(dh_sql "SELECT index_version||'|'||es_index||'|'||milvus_collection FROM dialogue_retrieval_pointer WHERE id=1")"
echo "  重启后指针 = $PTR"
if [ "$PTR" = "2|$BASE_ES|$BASE_MIL" ]; then
  registered "脚枪实锤：版本号仍 2 但名字被盖回基名——「重启副本重绑」只靠运维纪律，无程序闸"
elif [ "$PTR" = "2|$V2_ES|$V2_MIL" ]; then
  ok "指针名字未被盖回（引擎启动期没有覆盖指针名字）"
else
  registered "指针形态出乎意料：[$PTR]，按实际字节对质"
fi

dh_summary
