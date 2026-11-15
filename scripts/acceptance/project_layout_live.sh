#!/usr/bin/env bash
# 项目容器布局活体（doc/76 §5.1/§6；施工包 §10.2 第 1 场）：脚本自起一台隔离引擎，
# 真模型跑这些场——
#   1. 新项目：layout.json + content/runtime 两棵树；执行者的证据经发布才进 content，
#      组工作现场与工具全文留在 runtime，文件清单里看不到 runtime；
#   2. 媒体：入口图物理落 runtime/media，逻辑占位 workspaces/<wid>/.media 仍可读回原字节；
#   3. 项目记忆：markdown 模式的 MEMORY.md 落在 runtime/memory，不在 content；
#   4. shared／isolated：两种模式的执行证据都经发布进 content，组目录都在 runtime/orchestrations；
#   5. 删除：cleanup=done 时整容器消失；清理失败时 cleanup=pending，权限恢复后调度器补偿转 done。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，一个空的隔离 PG 库；脚本在里面建 pg_search 扩展）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/13（避开其它脚本占用的 db）
#   TAKO_ENGINE_PORT   默认 8951（被占就报错退出，不杀别人的进程）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎 PID。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT="${TAKO_ENGINE_PORT:-8951}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/13}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
if lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用；换 TAKO_ENGINE_PORT（不杀别人的进程）" >&2; exit 2
fi

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-layout.XXXXXX")
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

start_engine(){
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

echo "自起引擎 127.0.0.1:$PORT · 库 $DSN · workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
start_engine
B="http://127.0.0.1:$PORT"
ml_signup layout

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"ws-layout-live"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
CONT="$WS/tenants/$TEN/workspaces/$WID"
CONTENT="$CONT/content"
RUNTIME="$CONT/runtime"
# 先打一次文件清单：空容器就地建 layout.json 与两棵树（doc/76 §5.1 第 2 条）。
curl -sS "$B/api/workspaces/$WID/files" -H "Authorization: Bearer $TOK" >/dev/null
at_probe_run "$WID"
echo "  项目 $WID · 来源会话 $SID"

ws_dir(){ echo "$RUNTIME/orchestrations/$1/workspace"; }
files_json(){ curl -sS "$B/api/workspaces/$1/files${2:-}" -H "Authorization: Bearer $TOK"; }
task_field(){ at_task "$1"; echo "$AT_TASK" | jq -r "$2 // empty"; }

# ── 1. 新项目布局 + 内容/运行资源可见性 ─────────────────────────────────────────
eq "layout.json 落盘" "1" "$(jq -r .formatVersion "$CONT/layout.json" 2>/dev/null)"
[ -d "$CONTENT" ] && [ -d "$RUNTIME" ] && ok "content/ 与 runtime/ 两棵树都在" || no "两棵树不齐"
printf '基线内容\n' > "$CONTENT/report.md"

STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "用 bash 在工作目录执行：mkdir -p out && printf '证据-$STAMP\\n' > out/result.txt && echo 这条命令的输出会留在运行资源区。再加一条命令 seq 1 20000，让输出超过留尾上限、全文落盘。然后调用 submit_task_result，summary 写「已写 out/result.txt」，artifacts 声明 out/result.txt（kind=file）。" \
  '[{key:"a",content:$a,agent:"coder"}]')
RESP=$(at_create_orch "acc-layout-iso-$STAMP" "$WID" "$SID" "隔离执行写一份证据并发布" "$TASKS" '[]' "isolated")
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID" ] && [ "$OID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  场1 orchId=$OID"
if at_wait_orch "$OID" "succeeded" 300; then ok "场1 整组收口 succeeded"; else no "场1 没收口（$AT_LAST）"; at_orch "$OID"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
if at_wait_delivered "$OID" 180; then ok "场1 交付完成"; else no "场1 没交付"; ml_summary; fi
GD=$(ws_dir "$OID")
[ -d "$GD" ] && ok "组工作现场在项目 runtime/orchestrations/<id>/workspace" || no "组工作现场不在项目 runtime"
eq "执行证据经发布进 content" "证据-$STAMP" "$(cat "$CONTENT/out/result.txt" 2>/dev/null)"
[ ! -e "$CONTENT/runtime" ] && ok "content 里没有 runtime" || no "content 里出现了 runtime"
[ ! -e "$CONTENT/layout.json" ] && ok "layout.json 在容器根、不在 content" || no "layout.json 落进了 content"
[ -n "$(ls -A "$GD/tmp/bash" 2>/dev/null)" ] && ok "bash 全文留在组运行资源区（不写 content）" || no "组运行资源区没有 bash 全文"
LIST=$(files_json "$WID")
HAS_BASE=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("report.md") // "nil"')
[ "$HAS_BASE" != "nil" ] && ok "文件清单含基线 report.md" || no "文件清单里没有 report.md（$LIST）"
HAS_OUT=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("out") // "nil"')
[ "$HAS_OUT" != "nil" ] && ok "发布出的 out/ 出现在文件清单" || no "文件清单里没有 out/（$LIST）"
HAS_RT=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("runtime") // "nil"')
[ "$HAS_RT" = "nil" ] && ok "文件清单不列 runtime" || no "文件清单列出了 runtime（$LIST）"
# runtime 路径不能从文件出口读到（内容根之外）
eq "files?path=runtime → 404 114101" "114101" "$(files_json "$WID" '?path=runtime' | jq -r .code)"

# ── 2. 引擎媒体落 runtime/media，占位别名读回原字节 ─────────────────────────────
IMG=$(printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde\x00\x00\x00\x0cIDAT\x08\xd7c\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xdc\xccY\xe7\x00\x00\x00\x00IEND\xaeB\x60\x82' | base64)
RESP=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"messageId":"m_layout_media_'"$STAMP"'","text":"这张图里是什么？只回复 ok","workspaceId":"'"$WID"'","images":[{"mime":"image/png","dataBase64":"'"$IMG"'","name":"probe.png"}]}')
SID_MEDIA=$(echo "$RESP" | jq -r .data.sessionId)
[ -n "$SID_MEDIA" ] && [ "$SID_MEDIA" != "null" ] || { no "带图投递失败：$RESP"; ml_summary; }
SID="$SID_MEDIA"
ml_wait_settled 120 || no "媒体场会话 120s 没落定"
MEDIA_FILE=$(find "$RUNTIME/media" -type f 2>/dev/null | head -1)
[ -n "$MEDIA_FILE" ] && ok "入口图物理落在 runtime/media" || no "runtime/media 里没有入口图"
if [ -n "$MEDIA_FILE" ]; then
  NAME=$(basename "$MEDIA_FILE")
  curl -sS "$B/api/media?path=workspaces/$WID/.media/$NAME" -H "Authorization: Bearer $TOK" -o "$SCRATCH/media.out"
  printf '%s' "$IMG" | base64 -d > "$SCRATCH/media.want"
  if cmp -s "$SCRATCH/media.out" "$SCRATCH/media.want"; then ok "占位别名 .media/ 读回原字节"; else no "媒体字节不一致"; fi
fi
[ ! -e "$CONTENT/.media" ] && ok "content 里没有 .media" || no "content 里出现了 .media"

# ── 3. 项目记忆落 runtime/memory ───────────────────────────────────────────────
SID=""
ml_run '请用 memory 工具记住：这个项目的代号是「蓝鲸-LAYOUT」。target 用 workspace。做完只回复 ok。' "$WID"
ml_wait_terminal 120 || no "记忆场没到 terminal"
ml_wait_settled 120 || no "记忆场会话没落定"
MF="$RUNTIME/memory/MEMORY.md"
if [ -f "$MF" ] && grep -q '蓝鲸-LAYOUT' "$MF"; then ok "MEMORY.md 在 runtime/memory 且含代号"; else no "MEMORY.md 没写出来（$( [ -f "$MF" ] && cat "$MF" || echo 不存在)）"; fi
[ ! -e "$CONTENT/MEMORY.md" ] && ok "content 里没有 MEMORY.md" || no "MEMORY.md 落进了 content"

# 记忆场之后的会话作为任务来源（它挂着这个项目）。
at_probe_run "$WID"

# ── 4. shared／isolated：两种模式都经发布进 content ────────────────────────────
for MODE in shared isolated; do
  STAMP2="$(date +%s)-$$-$MODE"
  TASKS=$(jq -cn --arg a "在工作目录创建 $MODE.txt，内容恰好一行「$MODE-$STAMP2」。然后调用 submit_task_result，summary 写路径，artifacts 声明 $MODE.txt（kind=file）。" \
    '[{key:"a",content:$a,agent:"coder"}]')
  RESP=$(at_create_orch "acc-layout-$MODE-$STAMP2" "$WID" "$SID" "$MODE 模式写证据" "$TASKS" '[]' "$MODE")
  OID2=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
  [ -n "$OID2" ] && [ "$OID2" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
  if at_wait_orch "$OID2" "succeeded" 300; then ok "$MODE 组收口 succeeded"; else no "$MODE 组没收口（$AT_LAST）"; at_orch "$OID2"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
  if at_wait_delivered "$OID2" 180; then ok "$MODE 组交付完成"; else no "$MODE 组没交付"; ml_summary; fi
  eq "$MODE 证据经发布进 content" "$MODE-$STAMP2" "$(cat "$CONTENT/$MODE.txt" 2>/dev/null)"
  [ -d "$(ws_dir "$OID2")" ] && ok "$MODE 组目录在 runtime/orchestrations" || no "$MODE 组目录不在 runtime"
done

# ── 5. 删除：done 与 pending + 补偿 ────────────────────────────────────────────
RES=$(curl -sS -X DELETE "$B/api/workspaces/$WID" -H "Authorization: Bearer $TOK")
eq "删除项目 status=deleted" "deleted" "$(echo "$RES" | jq -r .data.status)"
eq "删除项目 cleanup=done" "done" "$(echo "$RES" | jq -r .data.cleanup)"
[ ! -e "$CONT" ] && ok "清理完成后整容器消失" || no "容器还在：$(ls "$CONT" 2>/dev/null | head -3)"
RES=$(curl -sS -w '\n%{http_code}' "$B/api/workspaces/$WID/files" -H "Authorization: Bearer $TOK")
eq "删除后文件出口 404 114001" "114001" "$(echo "$RES" | sed '$d' | jq -r .code)"
RES=$(curl -sS -X DELETE "$B/api/workspaces/$WID" -H "Authorization: Bearer $TOK")
eq "重复删除 404 114001" "114001" "$(echo "$RES" | jq -r .code)"

WID2=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"ws-layout-pending"}' | jq -r .data.workspace.id)
[ -n "$WID2" ] && [ "$WID2" != "null" ] || { no "建 pending 项目失败"; ml_summary; }
CONT2="$WS/tenants/$TEN/workspaces/$WID2"
curl -sS "$B/api/workspaces/$WID2/files" -H "Authorization: Bearer $TOK" >/dev/null
printf '待清\n' > "$CONT2/content/stuck.txt"
chmod 0555 "$CONT2"   # 让整容器删不掉：清理失败 → pending（不冒充 done）
RES=$(curl -sS -X DELETE "$B/api/workspaces/$WID2" -H "Authorization: Bearer $TOK")
eq "清理失败时 cleanup=pending" "pending" "$(echo "$RES" | jq -r .data.cleanup)"
eq "pending 记录落库" "pending" "$(ml_sql "SELECT status FROM project_cleanups WHERE workspace_id='$WID2'")"
[ -e "$CONT2" ] && ok "pending 时容器仍在（不谎报已清完）" || no "pending 但容器已经没了"
chmod 0755 "$CONT2"
# 调度器补偿（task-interval 2s）：标 done + 发 workspace_deleted。
for _ in $(seq 1 40); do
  ST=$(ml_sql "SELECT status FROM project_cleanups WHERE workspace_id='$WID2'")
  [ "$ST" = "done" ] && break
  sleep 1
done
eq "权限恢复后补偿转 done" "done" "$(ml_sql "SELECT status FROM project_cleanups WHERE workspace_id='$WID2'")"
[ ! -e "$CONT2" ] && ok "补偿后整容器消失" || no "补偿后容器还在"

ml_summary
