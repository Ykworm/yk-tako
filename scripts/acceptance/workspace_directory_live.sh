#!/usr/bin/env bash
# Directory 隔离工作区活体（doc/74 §8.2/§8.3；施工包 §12.2 第 2 场）：脚本自起一台隔离引擎
#（沿 agent_task_two_replicas_live.sh 的起法，kill 只打自己起的 PID），真模型跑四场——
#   1. 非 Git 并行 + 结果清单：a/b 并行（增/改/删），c 汇合；manifest 与真实字节对账，发布逐路径应用；
#   2. 同路径冲突：a/b 改同一文件 → c 工作目录 .tako/conflicts 留父版本 → 模型裁决 → 发布；
#   3. 项目变化：执行期间原项目被人改 → 发布受阻 workspace_changed → 恢复基线 → retry → 发布；
#   4. 部分发布恢复：400 个文件发布到一半 kill -9 → 重启按 publication/<epoch>.json 续发 → 全量字节一致。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，一个空的隔离 PG 库；脚本在里面建 pg_search 扩展）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/12（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT   默认 8958（被占就报错退出，不杀别人的进程）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎 PID。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT="${TAKO_ENGINE_PORT:-8958}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/12}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
if lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用；换 TAKO_ENGINE_PORT（不杀别人的进程）" >&2; exit 2
fi

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-ws-dir.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG="$SCRATCH/engine.log"
PID=""
cleanup(){
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill -TERM "$PID" 2>/dev/null
    for _ in $(seq 40); do kill -0 "$PID" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$PID" 2>/dev/null
  fi
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"

start_engine(){ # 起引擎并等就绪，sets PID
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$PORT" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" -task-interval 2s
  ) >>"$ELOG" 2>&1 &
  PID=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$PORT/api/auth/login" 2>/dev/null; then return 0; fi
    if ! kill -0 "$PID" 2>/dev/null; then echo "引擎启动即退，日志：$ELOG" >&2; tail -20 "$ELOG" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎 30s 没起来，日志：$ELOG" >&2; exit 2
}
kill_engine(){ # kill -9 自己起的引擎
  kill -9 "$PID" 2>/dev/null
  for _ in $(seq 40); do kill -0 "$PID" 2>/dev/null || break; sleep 0.25; done
}

echo "自起引擎 127.0.0.1:$PORT · 库 $DSN · workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
start_engine
B="http://127.0.0.1:$PORT"
WS_ROOT="$WS"
ml_signup wsdir

# 部分发布场要 bash 造 400 个文件（researcher 排除 bash）：inherit 基础池的自定义规格。
AGENT="wsdir_$(date +%s)_$$"
jq -cn --arg n "$AGENT" \
  '{name:$n,kind:"subagent",description:"Directory 隔离工作区执行者",spec:"你是隔离工作目录的执行者。按任务内容改动文件，完成后调用 submit_task_result 提交，artifacts 声明本次交付的文件（相对路径）。"}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @- >/dev/null

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"ws-dir-live"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
PROJ="$WS_ROOT/tenants/$TEN/workspaces/$WID"
mkdir -p "$PROJ"
printf '基线内容\n' > "$PROJ/report.md"
printf '待删\n' > "$PROJ/gone.md"
printf '共享基线\n' > "$PROJ/shared.txt"
at_probe_run "$WID"
echo "  项目 $WID · 来源会话 $SID"

ws_dir(){ echo "$WS_ROOT/tenants/$TEN/.tako/orchestrations/$1/workspace"; }
artifact_eq(){ # $1=taskId $2=path $3=期望内容 $4=标签
  curl -sS "$B/api/tasks/$1/artifact?path=$2" -H "Authorization: Bearer $TOK" -o "$SCRATCH/got.bin"
  printf '%s' "$3" > "$SCRATCH/want.bin"
  if cmp -s "$SCRATCH/got.bin" "$SCRATCH/want.bin"; then ok "$4"; else no "$4 —— 字节不符"; fi
}
task_field(){ at_task "$1"; echo "$AT_TASK" | jq -r "$2 // empty"; }

# ── 1. 非 Git 并行 + 结果清单：a 增改、b 增删、c 汇合 ─────────────────────────────
STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "在工作目录创建 alpha.txt（内容恰好一行「甲-$STAMP」），并把 report.md 改成恰好一行「甲改-$STAMP」。然后调用 submit_task_result，summary 写改动说明，artifacts 声明 alpha.txt 与 report.md（kind=file）。" \
  --arg b "在工作目录创建 beta.txt（内容恰好一行「乙-$STAMP」），并用 bash 执行 rm gone.md 把它删除。然后调用 submit_task_result，summary 写改动说明，artifacts 声明 beta.txt（kind=file）。" \
  --arg c "工作目录里已有上游留下的 alpha.txt 与 beta.txt。读齐两份，创建 final.txt：第一行是 alpha.txt 的原文，第二行是 beta.txt 的原文（各一行，不多不少）。然后调用 submit_task_result，summary 写 final.txt 路径，artifacts 声明 final.txt（kind=file）。" \
  --arg g "$AGENT" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:$g},{key:"c",content:$c,agent:"researcher"}]')
RESP=$(at_create_orch "acc-wsdir-par-$STAMP" "$WID" "$SID" "非 Git 并行：增改删 + 汇合" "$TASKS" \
  '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]' "isolated")
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID" ] && [ "$OID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID"
TID_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TID_B=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
TID_C=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
echo "  场1 orchId=$OID a=$TID_A b=$TID_B c=$TID_C"
if at_wait_orch "$OID" "succeeded" 300; then ok "场1 整组收口 succeeded"; else no "场1 没收口（$AT_LAST）"; at_orch "$OID"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
GD=$(ws_dir "$OID")
eq "组 Provider 固定为 directory" "directory" "$(jq -r .provider "$GD/state.json")"
# 结果清单与真实字节对账：a 增 alpha 改 report，b 增 beta 删 gone。
MA="$GD/results/$TID_A/1/manifest.json"
eq "a 清单 added 含 alpha.txt" "alpha.txt" "$(jq -r '.added[0].path' "$MA")"
eq "a 清单 modified 含 report.md" "report.md" "$(jq -r '.modified[0].path' "$MA")"
eq "a 清单 alpha 哈希与产物字节一致" "$(jq -r '.added[0].sha256' "$MA")" "$(shasum -a 256 "$GD/results/$TID_A/1/artifacts/alpha.txt" | cut -d' ' -f1)"
MB="$GD/results/$TID_B/1/manifest.json"
eq "b 清单 deleted 含 gone.md" "gone.md" "$(jq -r '.deleted[0].path' "$MB")"
artifact_eq "$TID_C" "final.txt" "甲-$STAMP
乙-$STAMP
" "c 的汇合产物字节逐字一致"
# 发布逐路径应用：增的在、改的对、删的没了。
eq "发布后项目 alpha.txt" "甲-$STAMP" "$(cat "$PROJ/alpha.txt")"
eq "发布后项目 report.md 被改" "甲改-$STAMP" "$(cat "$PROJ/report.md")"
[ ! -f "$PROJ/gone.md" ] && ok "发布后项目 gone.md 已删" || no "发布后 gone.md 还在"
eq "发布后项目 final.txt" "甲-$STAMP
乙-$STAMP" "$(cat "$PROJ/final.txt")"
eq "发布状态 published" "published" "$(task_field "$TID_C" '.workspace.publication.status')"
if at_wait_delivered "$OID" 180; then ok "场1 交付完成"; else no "场1 没交付"; ml_summary; fi
eq "场1 来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"

# ── 2. 同路径冲突：a/b 改 shared.txt 不同内容 → c 裁决 ───────────────────────────
STAMP2="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "把 shared.txt 改成恰好一行「甲改-$STAMP2」。然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  --arg b "把 shared.txt 改成恰好一行「乙改-$STAMP2」。然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  --arg c "上游两个父任务都改了 shared.txt，工作目录里保留了先到的版本，后到的父版本放在 .tako/conflicts/ 下（路径在 prompt 的冲突清单里）。裁决：把 shared.txt 改成恰好两行，第一行「甲改-$STAMP2」、第二行「乙改-$STAMP2」。然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  '[{key:"a",content:$a,agent:"researcher"},{key:"b",content:$b,agent:"researcher"},{key:"c",content:$c,agent:"researcher"}]')
RESP=$(at_create_orch "acc-wsdir-conf-$STAMP2" "$WID" "$SID" "两个父任务改同一文件，c 裁决冲突" "$TASKS" \
  '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]' "isolated")
OID2=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID2" ] && [ "$OID2" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID2"
TID_C2=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
TID_B2=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
echo "  场2 orchId=$OID2 c=$TID_C2"
if at_wait_orch "$OID2" "succeeded" 300; then ok "场2 整组收口 succeeded"; else no "场2 没收口（$AT_LAST）"; at_orch "$OID2"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
GD2=$(ws_dir "$OID2")
[ -s "$GD2/provider/conflicts/$TID_C2.json" ] && ok "c 的冲突记录在组目录（程序检出）" || no "c 没有冲突记录"
[ -f "$GD2/work/$TID_C2/.tako/conflicts/$TID_B2/shared.txt" ] && ok "后到父版本留在 c 工作目录的 .tako/conflicts（不覆盖）" || no "冲突父版本没留进 .tako/conflicts"
artifact_eq "$TID_C2" "shared.txt" "甲改-$STAMP2
乙改-$STAMP2
" "c 裁决后的产物字节逐字一致"
eq "发布后项目 shared.txt 是裁决结果" "甲改-$STAMP2
乙改-$STAMP2" "$(cat "$PROJ/shared.txt")"
if at_wait_delivered "$OID2" 180; then ok "场2 交付完成"; else no "场2 没交付"; ml_summary; fi

# ── 3. 项目变化：执行期间原项目被改 → 受阻 → 恢复基线 → retry → 发布 ───────────────
STAMP3="$(date +%s)-$$"
CONTENT3="把 report.md 的内容改成恰好一行「结果-$STAMP3」。然后调用 submit_task_result，summary 写「完成」，artifacts 声明 report.md（kind=file）。"
RESP=$(at_create_task "acc-wsdir-chg-$STAMP3" "$WID" "$SID" "$CONTENT3" "researcher" "isolated")
TID3=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID3=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID3" ] && [ "$TID3" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD3=$(ws_dir "$OID3")
echo "  场3 taskId=$TID3 orchId=$OID3"
for i in $(seq 1 60); do [ -f "$GD3/state.json" ] && break; sleep 1; done
[ -f "$GD3/state.json" ] || { no "60s 没等到组准备"; ml_summary; }
sleep 2
printf '人改\n' > "$PROJ/report.md"   # 执行期间原项目前进（非基线内容）
if at_wait_task "$TID3" "blocked" 300; then ok "场3 发布受阻 blocked"; else no "场3 没到 blocked（$AT_LAST）"; at_task "$TID3"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "受阻原因 workspace_changed" "workspace_changed" "$(task_field "$TID3" '.workspace.publication.errorCode')"
eq "受阻时项目 report.md 没被覆盖" "人改" "$(cat "$PROJ/report.md")"
# 人工恢复：把项目文件恢复到基线内容，再 retry-publication（处理 run 重新提交同一份工作）。
cp "$GD3/provider/baseline/report.md" "$PROJ/report.md"
RES=$(curl -sS -X POST "$B/api/tasks/$TID3/retry-publication" -H "Authorization: Bearer $TOK")
eq "retry-publication 202" "0" "$(echo "$RES" | jq -r .code)"
eq "publicationEpoch 涨到 2" "2" "$(echo "$RES" | jq -r '.data.publicationEpoch // 0')"
if at_wait_task "$TID3" "succeeded" 300; then ok "场3 恢复基线后重新发布 succeeded"; else no "场3 没到 succeeded（$AT_LAST）"; at_task "$TID3"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "重新提交生成新版本（resultRevision=2）" "2" "$(task_field "$TID3" '.resultRevision')"
eq "发布后项目 report.md 是结果内容" "结果-$STAMP3" "$(cat "$PROJ/report.md")"
eq "场3 恰好两场执行 run（首次 + 处理）" "2" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID3')")"
if at_wait_delivered "$OID3" 180; then ok "场3 交付完成"; else no "场3 没交付"; ml_summary; fi

# ── 4. 部分发布恢复：400 个文件发布到一半 kill -9 → 重启续发 ───────────────────────
STAMP4="$(date +%s)-$$"
CONTENT4="用 bash 在工作目录执行这条命令造 400 个文件：mkdir -p data && for i in \$(seq -w 1 400); do head -c 524288 /dev/zero | tr '\\0' 'A' > \"data/f\$i.bin\"; done。造完后调用 submit_task_result，summary 写「400 个文件」，artifacts 只声明 data/f001.bin（kind=file）即可（发布按改动清单全量应用，不必逐个声明）。"
RESP=$(at_create_task "acc-wsdir-partial-$STAMP4" "$WID" "$SID" "$CONTENT4" "$AGENT" "isolated")
TID4=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID4=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID4" ] && [ "$TID4" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD4=$(ws_dir "$OID4")
echo "  场4 taskId=$TID4 orchId=$OID4"
# 等发布真的开始且只完成一部分（0 < done < 400）再 kill -9。
PUBFILE=""
for i in $(seq 1 600); do
  for f in "$GD4"/publication/[0-9]*.json; do
    [ -f "$f" ] || continue
    DONE=$(jq '[.entries[] | select(.status=="done")] | length' "$f" 2>/dev/null)
    TOTAL=$(jq '.entries | length' "$f" 2>/dev/null)
    if [ "${DONE:-0}" -gt 0 ] 2>/dev/null && [ "${DONE:-0}" -lt "${TOTAL:-0}" ] 2>/dev/null; then PUBFILE="$f"; break; fi
  done
  [ -n "$PUBFILE" ] && break
  sleep 0.05
done
[ -n "$PUBFILE" ] || { no "30s 没等到部分发布窗口（0<done<total）"; at_task "$TID4"; echo "$AT_TASK" | jq -c '{status,workspace}'; ml_summary; }
DONE_AT_KILL=$(jq '[.entries[] | select(.status=="done")] | length' "$PUBFILE")
EPOCH=$(jq -r .epoch "$PUBFILE")
ok "发布到一半（done=$DONE_AT_KILL/400，epoch=$EPOCH）→ kill -9"
kill_engine
sleep 1
# 崩后现场：发布记录留着部分进度，项目里只落了已 done 的那部分。
eq "崩后发布记录仍在（部分进度）" "$DONE_AT_KILL" "$(jq '[.entries[] | select(.status=="done")] | length' "$PUBFILE")"
start_engine
# 重启对账：按记录续发，done 的核验、pending 的补上，同一 epoch 不重铸。
if at_wait_task "$TID4" "succeeded" 300; then ok "场4 重启续发后 succeeded"; else no "场4 没到 succeeded（$AT_LAST）"; at_task "$TID4"; echo "$AT_TASK" | jq -c '{status,workspace}'; tail -30 "$ELOG"; ml_summary; fi
eq "续发沿用同一 epoch（不重铸）" "$EPOCH" "$(task_field "$TID4" '.workspace.publication.epoch')"
eq "发布记录全量 done" "400" "$(jq '[.entries[] | select(.status=="done")] | length' "$GD4/publication/$EPOCH.json")"
eq "项目里 400 个文件齐了" "400" "$(ls "$PROJ/data" | wc -l | tr -d ' ')"
# 抽三个文件核字节（每个恰好 524288 个 A）。
for f in f001.bin f200.bin f400.bin; do
  SZ=$(wc -c < "$PROJ/data/$f" | tr -d ' ')
  SHA=$(shasum -a 256 "$PROJ/data/$f" | cut -d' ' -f1)
  WANT=$(head -c 524288 /dev/zero | tr '\0' 'A' | shasum -a 256 | cut -d' ' -f1)
  if [ "$SZ" = "524288" ] && [ "$SHA" = "$WANT" ]; then ok "data/$f 字节一致（524288 个 A）"; else no "data/$f 字节不符（$SZ 字节）"; fi
done
eq "场4 恰好一场执行 run（崩溃没让它重影）" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID4')")"
if at_wait_delivered "$OID4" 180; then ok "场4 交付完成"; else no "场4 没交付"; ml_summary; fi
eq "orchestration_finished 通知恰好 1 条" "1" "$(at_notif_count "$OID4" "orchestration_finished")"

ml_summary