#!/usr/bin/env bash
# 存量项目容器迁移活体（doc/76 §8；施工包 §10.2 第 2 场）：脚本自起一台隔离引擎
# （调度器关闭，Task 停在 pending 才能造出「有未收口工作」的准入拒绝），再跑离线迁移命令。
#   1. 未迁移拒绝：存量容器没有 layout.json → 文件、媒体、Task 创建回 409 117116；
#   2. 存量迁移：用户文件 → content/，.media/.tako/MEMORY.md → runtime/，
#      真编排组行对应的租户级工作现场 → 项目 runtime/orchestrations/；字节哈希不变；
#   3. 幂等：再跑一次 → skipped；
#   4. 故障：认不出身份的 staging／backup 报错不删；半截 staging（身份对、无清单）清掉重来；
#   5. 准入：原组 active（真行、pending Task）时拒绝迁移，按真实停止链收口后才放行；
#   6. 并发：两个迁移进程同时跑同一项目，advisory lock 串行，最终只迁移一次；
#   7. kill -9：复制中途打断后重跑收敛，字节不丢、不留 staging/backup；
#   8. 配额：迁移后的字节计入整租户目录，超上限的上传回 429 113001；
#   9. 空容器：没有旧字节的项目就地初始化（initialized）。
#
# 参数（全部环境变量）：
#   TAKO_PG_DSN        靶子库 DSN（**必填**，空的隔离 PG 库；脚本在里面建 pg_search 扩展）
#   TAKO_ENV           取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PSQL          psql 可执行文件，默认 psql
#   TAKO_REDIS_URL     默认 redis://127.0.0.1:6390/14
#   TAKO_ENGINE_PORT   默认 8952（被占就报错退出，不杀别人的进程）
#   TAKO_GO            go 可执行文件，默认 go，找不到再试 ~/sdk/go1.27.0/bin/go
#   TAKO_MODEL         租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：不调 /api/dev/reset；不打开发实例；kill 只打脚本自己起的引擎／迁移进程。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT="${TAKO_ENGINE_PORT:-8952}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/14}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
if lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用；换 TAKO_ENGINE_PORT（不杀别人的进程）" >&2; exit 2
fi

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-migrate.XXXXXX")
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
    # 调度器关闭：编排组的 Task 停在 pending，正好用来验证迁移准入（有未收口工作 → 拒绝）。
    exec "$BIN" -engine "127.0.0.1:$PORT" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" -task-interval=-1s
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
ml_signup migrate

TEN_ROOT="$WS/tenants/$TEN"
cont_of(){ echo "$TEN_ROOT/workspaces/$1"; }
new_project(){
  local wid
  wid=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"name":"'"$1"'"}' | jq -r .data.workspace.id)
  [ -n "$wid" ] && [ "$wid" != "null" ] || { echo "建项目失败：$1" >&2; exit 2; }
  echo "$wid"
}

# migrate $1=wid（空 = 全批）→ sets MIG_OUT / MIG_RC
migrate(){
  local args=(-migrate-project-layout -pg "$DSN" -workspace "$WS")
  [ -n "${1:-}" ] && args+=(-migrate-project-id "$1")
  MIG_OUT=$("$BIN" "${args[@]}" 2>&1); MIG_RC=$?
}
status_of(){ echo "$MIG_OUT" | grep -F "项目 $1（" | sed -E 's/.*）：([a-z]+)，文件.*/\1/' | head -1; }
files_of(){ echo "$MIG_OUT" | grep -F "项目 $1（" | sed -E 's/.*文件 ([0-9]+)，.*/\1/' | head -1; }
orches_of(){ echo "$MIG_OUT" | grep -F "项目 $1（" | sed -E 's/.*编排组 ([0-9]+).*/\1/' | head -1; }

# 写一份真实形状的存量容器（没有 layout.json：旧逻辑把用户文件、运行资源全放在容器根）。
seed_legacy(){
  local c; c=$(cont_of "$1")
  mkdir -p "$c/docs" "$c/.media" "$c/.tako/bash" "$c/.tako/fetch" "$c/.tako/search"
  printf '项目笔记\n' > "$c/notes.md"
  printf '季度,收入\nQ1,100\n' > "$c/docs/q1.csv"
  printf 'LEGACY-PNG-BYTES' > "$c/.media/legacy.png"
  printf '完整 bash 输出\n' > "$c/.tako/bash/full.txt"
  printf '<html>整页</html>\n' > "$c/.tako/fetch/page.html"
  printf '搜索结果全文\n' > "$c/.tako/search/s.txt"
  printf -- '- 旧项目记忆\n' > "$c/MEMORY.md"
  ln -s notes.md "$c/link.txt"
}

files_json(){ curl -sS "$B/api/workspaces/$1/files${2:-}" -H "Authorization: Bearer $TOK"; }

# ── 1. 未迁移拒绝 + 真编排组（调度器关着，Task 停在 pending）────────────────────
WID_A=$(new_project ws-agent-legacy)
curl -sS "$B/api/workspaces/$WID_A/files" -H "Authorization: Bearer $TOK" >/dev/null
at_probe_run "$WID_A"
SID_A="$SID"
echo "  场1 项目 $WID_A · 来源会话 $SID_A"
TASKS=$(jq -cn '[{key:"a",content:"占位：调度器关着，不会执行",agent:"coder"}]')
RESP=$(at_create_orch "acc-mig-open-$WID_A" "$WID_A" "$SID_A" "迁移准入用的未收口组" "$TASKS" '[]')
OID_A=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID_A" ] && [ "$OID_A" != "null" ] || { no "建未收口组失败：$RESP"; ml_summary; }
eq "未收口组是 active（Task pending）" "active" "$(at_orch_field "$OID_A" status)"

# 把它改回存量形状：新布局的字节搬回旧位置，再删 layout.json/runtime。
CONTA=$(cont_of "$WID_A")
shopt -s dotglob nullglob
mkdir -p "$CONTA/.media" "$CONTA/.tako"
for f in "$CONTA/content"/*; do mv "$f" "$CONTA/"; done
[ -d "$CONTA/runtime/media" ] && for f in "$CONTA/runtime/media"/*; do mv "$f" "$CONTA/.media/"; done
[ -d "$CONTA/runtime/tmp" ] && for f in "$CONTA/runtime/tmp"/*; do mv "$f" "$CONTA/.tako/"; done
[ -f "$CONTA/runtime/memory/MEMORY.md" ] && mv "$CONTA/runtime/memory/MEMORY.md" "$CONTA/MEMORY.md"
shopt -u dotglob nullglob
rm -rf "$CONTA/content" "$CONTA/runtime" "$CONTA/layout.json"
# 真编排组行对应的租户级工作现场（创建组只落 PG；工作现场在 Task 起跑时才有，这里用
# 与真实路径同形的夹具造出「旧位置已有字节」）。
OLD_GROUP="$TEN_ROOT/.tako/orchestrations/$OID_A/workspace"
mkdir -p "$OLD_GROUP/work/shared"
printf '组工作现场遗留\n' > "$OLD_GROUP/work/shared/note.txt"

eq "未迁移项目 files → 409 117116" "117116" "$(files_json "$WID_A" | jq -r .code)"
eq "未迁移项目 media → 409 117116" "117116" "$(curl -sS "$B/api/media?path=workspaces/$WID_A/.media/legacy.png" -H "Authorization: Bearer $TOK" | jq -r .code)"
BODY=$(jq -cn --arg w "$WID_A" --arg s "$SID_A" '{idempotencyKey:"mig-unmigrated",workspaceId:$w,sourceSessionId:$s,content:"x",agent:"coder"}')
eq "未迁移项目建 Task → 409 117116" "117116" "$(curl -sS -X POST "$B/api/tasks" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$BODY" | jq -r .code)"

# ── 2. 准入：有未收口组时拒绝，走真实停止链后才迁移 ─────────────────────────────
migrate "$WID_A"
eq "有未收口组 → 迁移失败退出 1" "1" "$MIG_RC"
echo "$MIG_OUT" | grep -q '未收口编排组' && ok "失败原因点名未收口编排组" || no "失败原因不对（$MIG_OUT）"
[ -e "$CONTA/layout.json" ] || ok "拒绝时没有写 layout（旧容器原样可用）" || no "拒绝时改了容器"

STOP=$(curl -sS -X POST "$B/api/orchestrations/$OID_A/stop" -H "Authorization: Bearer $TOK")
eq "停止组落 cancelled" "cancelled" "$(echo "$STOP" | jq -r .data.status)"
migrate "$WID_A"
eq "收口后迁移成功（退出 0）" "0" "$MIG_RC"
eq "场2 状态 migrated" "migrated" "$(status_of "$WID_A")"
eq "场2 编组工作现场迁入 1 个" "1" "$(orches_of "$WID_A")"
eq "layout.json 已写" "1" "$(jq -r .formatVersion "$CONTA/layout.json" 2>/dev/null)"
[ -f "$CONTA/runtime/orchestrations/$OID_A/workspace/work/shared/note.txt" ] && ok "组工作现场搬到项目 runtime/orchestrations" || no "组工作现场没搬进项目 runtime"
[ ! -e "$OLD_GROUP" ] && ok "租户级旧组目录已清掉" || no "租户级旧组目录还在"

# ── 3. 存量迁移：映射、哈希、别名读取 ─────────────────────────────────────────
WID_B=$(new_project ws-agent-legacy-files)
seed_legacy "$WID_B"
CONTB=$(cont_of "$WID_B")
HASH_NOTES=$(shasum -a 256 "$CONTB/notes.md" | cut -d' ' -f1)
HASH_MEDIA=$(shasum -a 256 "$CONTB/.media/legacy.png" | cut -d' ' -f1)
eq "存量 files → 409 117116" "117116" "$(files_json "$WID_B" | jq -r .code)"

migrate "$WID_B"
eq "场3 状态 migrated" "migrated" "$(status_of "$WID_B")"
eq "用户文件进 content" "$HASH_NOTES" "$(shasum -a 256 "$CONTB/content/notes.md" 2>/dev/null | cut -d' ' -f1)"
eq "子目录进 content" "季度,收入" "$(head -1 "$CONTB/content/docs/q1.csv" 2>/dev/null)"
eq "媒体哈希不变" "$HASH_MEDIA" "$(shasum -a 256 "$CONTB/runtime/media/legacy.png" 2>/dev/null | cut -d' ' -f1)"
eq "bash 全文进 runtime/tmp/bash" "完整 bash 输出" "$(cat "$CONTB/runtime/tmp/bash/full.txt" 2>/dev/null)"
eq "fetch 全文进 runtime/tmp/fetch" "<html>整页</html>" "$(cat "$CONTB/runtime/tmp/fetch/page.html" 2>/dev/null)"
eq "项目记忆进 runtime/memory" "- 旧项目记忆" "$(cat "$CONTB/runtime/memory/MEMORY.md" 2>/dev/null)"
eq "符号链接按链接复制" "notes.md" "$(readlink "$CONTB/content/link.txt" 2>/dev/null)"
[ ! -e "$CONTB/.media" ] && [ ! -e "$CONTB/.tako" ] && [ ! -e "$CONTB/MEMORY.md" ] && ok "旧位置不再有运行资源" || no "旧位置还剩运行资源"

LIST=$(files_json "$WID_B")
HAS_NOTES=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("notes.md") // "nil"')
HAS_DOCS=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("docs") // "nil"')
HAS_MEM=$(echo "$LIST" | jq -r '[.data.entries[].name] | index("MEMORY.md") // "nil"')
[ "$HAS_NOTES" != "nil" ] && [ "$HAS_DOCS" != "nil" ] && ok "文件清单列出 content 里的用户文件" || no "文件清单不对（$LIST）"
[ "$HAS_MEM" = "nil" ] && ok "文件清单不列 runtime/memory" || no "MEMORY.md 出现在文件清单"
curl -sS "$B/api/media?path=workspaces/$WID_B/.media/legacy.png" -H "Authorization: Bearer $TOK" -o "$SCRATCH/legacy.out"
eq "占位别名读回旧媒体字节" "LEGACY-PNG-BYTES" "$(cat "$SCRATCH/legacy.out" 2>/dev/null)"

# ── 4. 幂等：再跑一次 → skipped ────────────────────────────────────────────────
migrate "$WID_B"
eq "幂等重跑退出 0" "0" "$MIG_RC"
eq "幂等重跑 skipped" "skipped" "$(status_of "$WID_B")"

# ── 5. 故障与崩溃对账 ─────────────────────────────────────────────────────────
WID_E=$(new_project ws-agent-migrate-fault)
seed_legacy "$WID_E"
CONTE=$(cont_of "$WID_E")
REL_E="tenants/$TEN/workspaces/$WID_E"

mkdir -p "$CONTE.migrating"   # 没有身份标记 = 认不出，不删
migrate "$WID_E"
eq "身份不明 staging → 失败" "1" "$MIG_RC"
echo "$MIG_OUT" | grep -q '不删未知目录' && ok "报错点名身份标记不可读" || no "报错不对（$MIG_OUT）"
[ -d "$CONTE.migrating" ] && ok "认不出的 staging 原样留着" || no "删了认不出的目录"
rmdir "$CONTE.migrating"

mkdir -p "$CONTE.migrate-backup"
migrate "$WID_E"
eq "身份不明 backup → 失败" "1" "$MIG_RC"
[ -d "$CONTE.migrate-backup" ] && ok "认不出的 backup 原样留着" || no "删了认不出的 backup"
rmdir "$CONTE.migrate-backup"

# 身份对、清单未落（复制阶段崩）→ 清掉重来
mkdir -p "$CONTE.migrating"
printf '{"formatVersion":1,"kind":"project-layout-migrate","workspaceId":"%s","tenantId":"%s","container":"%s","createdAt":"2026-11-12T00:00:00Z"}\n' \
  "$WID_E" "$TEN" "$REL_E" > "$CONTE.migrating/.tako-migration.json"
migrate "$WID_E"
eq "半截 staging 重来后 migrated" "migrated" "$(status_of "$WID_E")"
[ ! -e "$CONTE.migrating" ] && ok "半成品 staging 已清掉" || no "半成品 staging 还在"

# ── 6. 并发：两个迁移进程同一项目，advisory lock 串行 ───────────────────────────
WID_D=$(new_project ws-agent-migrate-conc)
seed_legacy "$WID_D"
CONTD=$(cont_of "$WID_D")
mkdir -p "$CONTD/bulk"
dd if=/dev/zero of="$SCRATCH/bulk.bin" bs=32768 count=1 2>/dev/null
for i in $(seq -w 1 200); do cp "$SCRATCH/bulk.bin" "$CONTD/bulk/f_$i.bin"; done
"$BIN" -migrate-project-layout -migrate-project-id "$WID_D" -pg "$DSN" -workspace "$WS" >"$SCRATCH/d1.out" 2>&1 & P1=$!
sleep 0.05
"$BIN" -migrate-project-layout -migrate-project-id "$WID_D" -pg "$DSN" -workspace "$WS" >"$SCRATCH/d2.out" 2>&1 & P2=$!
wait "$P1"; RC1=$?
wait "$P2"; RC2=$?
eq "并发迁移两进程都退出 0" "0 0" "$RC1 $RC2"
S1=$(sed -nE 's/.*）：([a-z]+)，文件.*/\1/p' "$SCRATCH/d1.out" | head -1)
S2=$(sed -nE 's/.*）：([a-z]+)，文件.*/\1/p' "$SCRATCH/d2.out" | head -1)
eq "恰好一次真迁移 + 一次 skipped" "migrated skipped" "$(printf '%s %s\n' "$S1" "$S2" | tr ' ' '\n' | sort | tr '\n' ' ' | sed 's/ $//')"
eq "并发后文件齐全" "200" "$(find "$CONTD/content/bulk" -type f -name 'f_*.bin' 2>/dev/null | wc -l | tr -d ' ')"
[ ! -e "$CONTD.migrating" ] && [ ! -e "$CONTD.migrate-backup" ] && ok "并发后不留 staging/backup" || no "并发后留下中间目录"

# ── 7. kill -9：复制中途打断，重跑收敛 ─────────────────────────────────────────
WID_F=$(new_project ws-agent-migrate-crash)
seed_legacy "$WID_F"
CONTF=$(cont_of "$WID_F")
mkdir -p "$CONTF/big"
dd if=/dev/zero of="$SCRATCH/big.bin" bs=65536 count=1 2>/dev/null
for i in $(seq -w 1 300); do cp "$SCRATCH/big.bin" "$CONTF/big/g_$i.bin"; done
printf 'kill -9 前的内容\n' > "$CONTF/big/g_001.bin"
"$BIN" -migrate-project-layout -migrate-project-id "$WID_F" -pg "$DSN" -workspace "$WS" >"$SCRATCH/f1.out" 2>&1 & PK=$!
sleep 0.08
kill -9 "$PK" 2>/dev/null
wait "$PK" 2>/dev/null
MIG_OUT=""; MIG_RC=0
for _ in $(seq 1 3); do
  migrate "$WID_F"
  [ "$MIG_RC" -eq 0 ] && break
done
eq "kill -9 后重跑退出 0" "0" "$MIG_RC"
case "$(status_of "$WID_F")" in
  migrated|skipped|recovered) ok "kill -9 后收敛（$(status_of "$WID_F")）";;
  *) no "kill -9 后状态异常（$MIG_OUT）";;
esac
eq "kill -9 后字节不丢" "kill -9 前的内容" "$(cat "$CONTF/content/big/g_001.bin" 2>/dev/null)"
eq "kill -9 后文件数对" "300" "$(find "$CONTF/content/big" -type f -name 'g_*.bin' 2>/dev/null | wc -l | tr -d ' ')"
[ ! -e "$CONTF.migrating" ] && [ ! -e "$CONTF.migrate-backup" ] && ok "kill -9 后不留中间目录" || no "kill -9 后留下中间目录"

# ── 8. 配额按整租户目录计算：迁移后的字节不能绕过上限 ───────────────────────────
ml_sql "UPDATE tenants SET max_storage_bytes = 4096 WHERE id = '$TEN'" >/dev/null
IMG=$(printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde\x00\x00\x00\x0cIDAT\x08\xd7c\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xdc\xccY\xe7\x00\x00\x00\x00IEND\xaeB\x60\x82' | base64)
RUNS_BEFORE=$(ml_sql "SELECT count(*) FROM runs WHERE workspace_id = '$WID_B'")
RESP=$(curl -sS -X POST "$B/api/runs" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"messageId":"m_mig_quota_'"$WID_B"'","text":"只回复 ok","workspaceId":"'"$WID_B"'","images":[{"mime":"image/png","dataBase64":"'"$IMG"'","name":"q.png"}]}')
eq "迁移后字节计入配额 → 429 113001" "113001" "$(echo "$RESP" | jq -r .code)"
eq "配额拒绝时不建 run" "$RUNS_BEFORE" "$(ml_sql "SELECT count(*) FROM runs WHERE workspace_id = '$WID_B'")"
ml_sql "UPDATE tenants SET max_storage_bytes = 0 WHERE id = '$TEN'" >/dev/null

# ── 9. 空容器就地初始化 + 全批有界 ─────────────────────────────────────────────
WID_G=$(new_project ws-agent-empty)
migrate "$WID_G"
eq "空容器 initialized" "initialized" "$(status_of "$WID_G")"
eq "空容器写 layout" "1" "$(jq -r .formatVersion "$(cont_of "$WID_G")/layout.json" 2>/dev/null)"

migrate ""
eq "全批迁移退出 0" "0" "$MIG_RC"
BAD=$(echo "$MIG_OUT" | sed -nE 's/.*）：([a-z]+)，文件.*/\1/p' | grep -cvE '^(initialized|migrated|recovered|skipped)$')
eq "全批里没有 failed" "0" "$BAD"

ml_summary
