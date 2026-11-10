#!/usr/bin/env bash
# 记忆迁移与双向互切活体（doc/65 §6.1/§6.2 / §13.3-5）：旧结构升级、pipeline↔markdown、
# 失败预检（活 run / pending / failed）、重放幂等、域冲突、解析失败不动库。
# **不打模型、不连 broker**——引擎以 pipeline + Extract/Recall=off 起，种子走 dev 数据准备口。
#
# 本脚本**自己起停隔离引擎**（它知道 PID，kill 只打自己起的进程）：
#   靶子库：TAKO_PG_DSN 指向一个**空的隔离 PG 库**（脚本在里面建 pg_search 扩展；别指开发库 tako）。
#   Redis：TAKO_REDIS_URL（默认 redis://127.0.0.1:6390/8，用一个没别人用的 db 索引）。
#   端口：TAKO_ENGINE_PORT（默认 8953，被占就报错退出，不杀别人的进程）。
#   workspace：脚本自建临时目录，退出即删（互切要控制文件全集，不能用共享根）。
# 其余参数：TAKO_PSQL / TAKO_GO（go 可执行文件；默认 PATH 找不到时回退 $HOME/sdk/go1.27.0/bin/go）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_need_dsn

PORT="${TAKO_ENGINE_PORT:-8953}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/8}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
if lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "端口 $PORT 已被占用；换 TAKO_ENGINE_PORT（不杀别人的进程）" >&2; exit 2
fi

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-migrate-live.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG="$SCRATCH/engine.log"
EPID=""
cleanup(){
  if [ -n "$EPID" ] && kill -0 "$EPID" 2>/dev/null; then
    kill -TERM "$EPID" 2>/dev/null
    for _ in $(seq 40); do kill -0 "$EPID" 2>/dev/null || break; sleep 0.25; done
    kill -9 "$EPID" 2>/dev/null
  fi
  wait 2>/dev/null
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

B="http://127.0.0.1:$PORT"
export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"

start_engine(){ # $1=mode
  # 从 yk-tako-eino 起引擎：引擎在 cwd 找 .env（模型厂商目录），脚本目录下没有它就起不来。
  # exec 让子 shell 变身引擎进程——EPID 仍是引擎 PID，kill 照常生效。
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE="$1" TAKO_MEMORY_EXTRACT=off TAKO_MEMORY_RECALL=off
    exec "$BIN" -engine "127.0.0.1:$PORT" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS"
  ) >>"$ELOG" 2>&1 &
  EPID=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "$B/api/auth/login" 2>/dev/null; then return 0; fi
    if ! kill -0 "$EPID" 2>/dev/null; then echo "引擎启动即退，日志：$ELOG" >&2; tail -20 "$ELOG" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎 30s 没起来，日志：$ELOG" >&2; exit 2
}
stop_engine(){
  [ -n "$EPID" ] || return 0
  kill -TERM "$EPID" 2>/dev/null
  for _ in $(seq 40); do kill -0 "$EPID" 2>/dev/null || { EPID=""; wait 2>/dev/null; return 0; }; sleep 0.25; done
  kill -9 "$EPID" 2>/dev/null; EPID=""; wait 2>/dev/null
}
migrate(){ # $1=方向；输出进 MIG_OUT，退出码进 MIG_RC
  MIG_OUT=$("$BIN" -memory-migrate-to="$1" -pg "$DSN" -workspace "$WS" 2>&1); MIG_RC=$?
}

echo "隔离引擎 127.0.0.1:$PORT · 库 $DSN · 临时 workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }

# ── 1. 新结构起步 → 注册 → 造「合成旧结构」（真实租户/项目身份 + 旧列旧表） ────────
start_engine pipeline
ml_signup migrate no_model
DEF_W=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"默认项目"}' | jq -r .data.workspace.id)
if [ -z "$DEF_W" ] || [ "$DEF_W" = "null" ]; then no "建项目失败"; ml_summary; fi
# 迁移回退规则认「租户默认项目」（workspaces.is_default）；验收租户没有现成默认项目，
# 把首建项目标成默认——这是夹具，不是被验行为。
ml_sql_raw "UPDATE workspaces SET is_default=true WHERE id='$DEF_W';" >/dev/null
seed(){ # $1=layer $2=scope $3=content $4=workspaceId
  curl -sS -X POST "$B/api/dev/memory" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d '{"layer":"'"$1"'","scope":"'"$2"'","content":"'"$3"'","workspaceId":"'"$4"'"}' | jq -r .data.id
}
R_OLD=$(seed recall workspace '旧召回事实' "$DEF_W")
R_EP=$(seed recall workspace '旧 episode 行' "$DEF_W")
stop_engine
# 加回旧列并标记旧行；建旧 notes 表（user 一行 + tenant 一行）。
ml_sql_raw "ALTER TABLE memory_items ADD COLUMN kind text; ALTER TABLE memory_items ADD COLUMN topic text;" >/dev/null
ml_sql_raw "UPDATE memory_items SET kind='fact', topic='legacy-topic' WHERE id='$R_OLD';" >/dev/null
ml_sql_raw "UPDATE memory_items SET kind='episode', topic='run:legacy' WHERE id='$R_EP';" >/dev/null
ml_sql_raw "CREATE TABLE notes (id text PRIMARY KEY, workspace text NOT NULL DEFAULT '', kind text NOT NULL DEFAULT '',
  content text NOT NULL, quote text NOT NULL DEFAULT '', source_run_id text NOT NULL DEFAULT '',
  source_seq bigint NOT NULL DEFAULT 0, superseded_by text, tenant_id text NOT NULL DEFAULT '',
  user_id text NOT NULL DEFAULT '', scope text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now());" >/dev/null
ml_sql_raw "INSERT INTO notes (id, content, quote, tenant_id, user_id, scope) VALUES
  ('note_user_1', '旧笔记里的个人偏好', '我喜欢 tea', '$TEN', '$ML_UID', 'user'),
  ('note_tenant_1', '旧团队规范：代码先写测试', '', '$TEN', '', 'tenant');" >/dev/null
BEFORE_N=$(ml_sql 'SELECT count(*) FROM memory_items')

# ── 2. 重启 = 升级：旧行迁到位、旧结构清干净 ────────────────────────────────────
start_engine pipeline
eq "notes 表已退役" "" "$(ml_sql "SELECT to_regclass('notes')")"
eq "kind 列已删" "0" "$(ml_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='memory_items' AND column_name='kind'")"
eq "topic 列已删" "0" "$(ml_sql "SELECT count(*) FROM information_schema.columns WHERE table_name='memory_items' AND column_name='topic'")"
# 行数 +1：user note 变成 resident 行；tenant note 走 teamInstructions，不进表。
eq "行数不丢（+1 user note）" "$((BEFORE_N + 1))" "$(ml_sql 'SELECT count(*) FROM memory_items')"
eq "旧 note → resident/user/active（保留原 id）" "resident|user|active" "$(ml_sql "SELECT layer||'|'||scope||'|'||state FROM memory_items WHERE id='note_user_1'")"
eq "旧召回行 → recall/workspace/默认项目" "recall|workspace|active|$DEF_W" "$(ml_sql "SELECT layer||'|'||scope||'|'||state||'|'||workspace FROM memory_items WHERE id='$R_OLD'")"
# 布尔经 || 拼接渲染成 true/false，不是 t/f。
eq "episode → archived 且写 left_active_at" "archived|true" "$(ml_sql "SELECT state||'|'||(left_active_at IS NOT NULL) FROM memory_items WHERE id='$R_EP'")"
TI=$(curl -sS "$B/api/tenants/$TEN/runtime-config" -H "Authorization: Bearer $TOK" | jq -r .data.config.teamInstructions)
if echo "$TI" | grep -q '旧团队规范'; then ok "tenant note → teamInstructions"; else no "teamInstructions 没有旧团队规范：$TI"; fi

# ── 3. 再重启 = 幂等：行数与 teamInstructions 都不变 ─────────────────────────────
stop_engine
start_engine pipeline
eq "二次启动行数不变（幂等）" "$((BEFORE_N + 1))" "$(ml_sql 'SELECT count(*) FROM memory_items')"
TI2=$(curl -sS "$B/api/tenants/$TEN/runtime-config" -H "Authorization: Bearer $TOK" | jq -r .data.config.teamInstructions)
eq "teamInstructions 不翻倍" "$TI" "$TI2"

# ── 4. pipeline→markdown：导出 active resident（含迁移来的 note 行） ─────────────
# 补种两条迁移后才有的 resident（user 一条 + workspace 一条），让 USER.md 有 2 条 user 条目、
# 导入数=3 可断言；导出归档断言锚 note_user_1（迁移产物）+ 这两条新种。
R_RES2=$(seed resident user '导入前就有的常驻偏好' '')
R_WS_RES=$(seed resident workspace '项目级常驻规矩' "$DEF_W")
stop_engine
migrate markdown
eq "互切 →markdown 退出码 0" "0" "$MIG_RC"
UF="$WS/tenants/$TEN/users/$ML_UID/USER.md"
MF="$WS/tenants/$TEN/workspaces/$DEF_W/MEMORY.md"
if [ -f "$UF" ] && grep -q '旧笔记里的个人偏好' "$UF" && grep -q '导入前就有的常驻偏好' "$UF"; then
  ok "USER.md 含两条 user 条目：$(cat "$UF")"
else no "USER.md 内容不对：$(cat "$UF" 2>/dev/null)"; fi
if [ -f "$MF" ] && grep -q '项目级常驻规矩' "$MF"; then ok "MEMORY.md 含 workspace 条目"; else no "MEMORY.md 内容不对"; fi
eq "导出归档：迁移来的 note 行" "archived" "$(ml_sql "SELECT state FROM memory_items WHERE id='note_user_1'")"
eq "导出归档：原 resident user 行" "archived" "$(ml_sql "SELECT state FROM memory_items WHERE id='$R_RES2'")"
eq "导出归档：workspace resident 行" "archived" "$(ml_sql "SELECT state FROM memory_items WHERE id='$R_WS_RES'")"
eq "归档写 left_active_at" "t" "$(ml_sql "SELECT (left_active_at IS NOT NULL) FROM memory_items WHERE id='note_user_1'")"
eq "批次表记 pipeline_to_markdown" "1" "$(ml_sql "SELECT count(*) FROM memory_migration_batches WHERE direction='pipeline_to_markdown'")"
UF_HASH=$(shasum -a 256 "$UF" | cut -d' ' -f1)

# ── 5. 重放导出：没有 active resident → 一个文件不动 ─────────────────────────────
migrate markdown
eq "重放导出退出码 0" "0" "$MIG_RC"
eq "重放导出文件字节不变" "$UF_HASH" "$(shasum -a 256 "$UF" | cut -d' ' -f1)"
eq "重放导出不新增批次" "1" "$(ml_sql "SELECT count(*) FROM memory_migration_batches WHERE direction='pipeline_to_markdown'")"

# ── 6. markdown→pipeline：文件是真源，插行 source=model、文件保留 ────────────────
migrate pipeline
eq "互切 →pipeline 退出码 0" "0" "$MIG_RC"
eq "导入行数 = 文件条目数（2 user + 1 workspace）" "3" "$(ml_sql "SELECT count(*) FROM memory_items WHERE layer='resident' AND state='active' AND source='model'")"
eq "导入行 quote 为空、证据指针为空（不伪造历史）" "0" "$(ml_sql "SELECT count(*) FROM memory_items WHERE layer='resident' AND state='active' AND source='model' AND (quote<>'' OR source_run_id<>'' OR source_session_id<>'')")"
eq "批次表记 markdown_to_pipeline" "1" "$(ml_sql "SELECT count(*) FROM memory_migration_batches WHERE direction='markdown_to_pipeline'")"
eq "源文件保留作来源" "$UF_HASH" "$(shasum -a 256 "$UF" | cut -d' ' -f1)"
N_AFTER_IMPORT=$(ml_sql 'SELECT count(*) FROM memory_items')

# ── 7. 重放导入：同批文件 → 直接退出，不重复插 ───────────────────────────────────
migrate pipeline
eq "重放导入退出码 0" "0" "$MIG_RC"
eq "重放导入不重复插行" "$N_AFTER_IMPORT" "$(ml_sql 'SELECT count(*) FROM memory_items')"
eq "重放导入不新增批次" "1" "$(ml_sql "SELECT count(*) FROM memory_migration_batches WHERE direction='markdown_to_pipeline'")"

# ── 8. 域冲突：文件改了（新哈希）但目标域还有 active resident → 拒绝 ──────────────
printf '\n§\n冲突探针条目' >> "$UF"
migrate pipeline
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q '域冲突'; then
  ok "域冲突拒绝且点名文件"
else no "域冲突没拒绝：rc=$MIG_RC out=$MIG_OUT"; fi
eq "域冲突一行没插" "$N_AFTER_IMPORT" "$(ml_sql 'SELECT count(*) FROM memory_items')"
# 官方解法：先导出归档（文件从 PG 重生成），再导入。
migrate markdown
eq "冲突后先导出归档成功" "0" "$MIG_RC"
migrate pipeline
eq "归档后再导入成功" "0" "$MIG_RC"
eq "往返不是重放（新导入批次）" "2" "$(ml_sql "SELECT count(*) FROM memory_migration_batches WHERE direction='markdown_to_pipeline'")"

# ── 9. 失败预检：活 run / pending job 挡两个方向；failed job 只挡 →markdown ────────
ml_sql_raw "INSERT INTO runs (id, session_id, status, lease_until, created_at) VALUES ('r_mig_live', 's_mig_live', 'running', now() + interval '10 minutes', now());" >/dev/null
migrate markdown
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q 'r_mig_live'; then ok "活 run 挡住互切并点名 run"; else no "活 run 没挡住：rc=$MIG_RC out=$MIG_OUT"; fi
ml_sql_raw "DELETE FROM runs WHERE id='r_mig_live';" >/dev/null
ml_sql_raw "INSERT INTO memory_jobs (id, run_id, status, kind, next_attempt_at, created_at, updated_at) VALUES ('mj_mig_pending', 'r_mig_live', 'pending', 'postrun', now(), now(), now());" >/dev/null
migrate markdown
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q 'mj_mig_pending'; then ok "pending job 挡住 →markdown"; else no "pending job 没挡住"; fi
migrate pipeline
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q 'mj_mig_pending'; then ok "pending job 挡住 →pipeline"; else no "pending job 没挡住 →pipeline"; fi
ml_sql_raw "UPDATE memory_jobs SET status='failed', last_error='探针失败原因' WHERE id='mj_mig_pending';" >/dev/null
migrate markdown
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q '探针失败原因'; then ok "failed job 挡 →markdown 且列出 last_error"; else no "failed job 没挡住 →markdown"; fi
migrate pipeline
eq "failed job 不挡 →pipeline（重放路径退出 0）" "0" "$MIG_RC"
ml_sql_raw "DELETE FROM memory_jobs WHERE id='mj_mig_pending';" >/dev/null

# ── 10. 解析失败：文件读不懂 → 拒绝，库一个字节不动 ──────────────────────────────
cp "$UF" "$SCRATCH/USER.md.bak"
printf ' 首尾带空格，过不了规范形态' > "$UF"
N_BEFORE_PARSE=$(ml_sql 'SELECT count(*) FROM memory_items')
migrate pipeline
if [ "$MIG_RC" -ne 0 ] && echo "$MIG_OUT" | grep -q '解析'; then ok "解析失败拒绝"; else no "解析失败没拒绝：rc=$MIG_RC"; fi
eq "解析失败库不动" "$N_BEFORE_PARSE" "$(ml_sql 'SELECT count(*) FROM memory_items')"
cp "$SCRATCH/USER.md.bak" "$UF"
migrate pipeline
eq "恢复后重放退出 0" "0" "$MIG_RC"

ml_summary