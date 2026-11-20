#!/usr/bin/env bash
# dialogue_hybrid_up.sh — 历史对话混合检索活体验收（doc/84）的隔离环境起停器。
#
# 每个 <name> 对应一套完全隔离的验收环境：
#   PG      5433 上独立库 tako_p5_<name>（engine 启动自迁移 + CREATE EXTENSION pg_search）
#   Redis   独立 db 号（起引擎时指定）
#   工作区  <repo>/tmp/workspace-p5-<name>
#   ES/Zilliz 验收索引与 collection：tako_dialogue_eval_<ts>_<name>（时间戳名，只删自己的）
#
# 引擎为活体（调度循环默认开，不加 -task-interval=-1s）。
#
# 用法：
#   dialogue_hybrid_up.sh init <name>                       建库/建工作区/生成验收名/编体检工具
#   dialogue_hybrid_up.sh start <name> --tag a --port 8944 --redis-db 11 [--mode hybrid|pg_search]
#       [--es-url URL] [--embed-base URL] [--milvus-uri URL] [--task-interval 1s]
#   dialogue_hybrid_up.sh stop <name> [--tag a]             停引擎（默认全部）
#   dialogue_hybrid_up.sh restart <name> --tag a            用上次完整命令重启（场景 7 用）
#   dialogue_hybrid_up.sh status <name>                     列出引擎
#   dialogue_hybrid_up.sh destroy <name>                    停引擎 + 删库/索引/collection/工作区（只删本脚本的）
#
# 参数化：TAKO_ENV（.env 路径）、TAKO_PSQL、TAKO_PG_HOST/PORT、TAKO_REDIS_ADDR、
# TAKO_EVAL_STATE_DIR、TAKO_GO。绝不碰开发库 tako、绝不调 /api/dev/reset、绝不杀别人的进程。
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"   # yk-tako 根
EINO_DIR="$REPO_DIR/yk-tako-eino"
ENV_FILE="${TAKO_ENV:-$EINO_DIR/.env}"
PSQL_BIN="${TAKO_PSQL:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin/psql}"
PG_HOST="${TAKO_PG_HOST:-127.0.0.1}"
PG_PORT="${TAKO_PG_PORT:-5433}"
REDIS_ADDR="${TAKO_REDIS_ADDR:-127.0.0.1:6390}"
STATE_ROOT="${TAKO_EVAL_STATE_DIR:-${TMPDIR:-/tmp}/tako-dialogue-eval}"
GO_BIN="${TAKO_GO:-go}"

die() { echo "dialogue_hybrid_up.sh: $*" >&2; exit 1; }

sql() { "$PSQL_BIN" "host=$PG_HOST port=$PG_PORT dbname=$1 sslmode=disable" -v ON_ERROR_STOP=1 -qAt -c "$2"; }

state_dir() { echo "$STATE_ROOT/$1"; }

require_state() {
  local dir; dir="$(state_dir "$1")"
  [ -f "$dir/state.env" ] || die "环境 $1 未 init（找不到 $dir/state.env）"
}

# port_free：端口被占先 lsof 查是谁；被别人占就失败退出（绝不杀别人的进程）。
port_free() {
  local port="$1"
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "dialogue_hybrid_up.sh: 端口 $port 已被占用（lsof 查明归属）：$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2)" >&2
    return 1
  fi
  return 0
}

cmd_init() {
  local name="$1"
  local dir; dir="$(state_dir "$name")"
  if [ -f "$dir/state.env" ] && [ "${DH_FORCE:-0}" != "1" ]; then
    die "环境 $name 已存在（$dir）。要重建先 destroy，或 DH_FORCE=1"
  fi
  mkdir -p "$dir"

  local db="tako_p5_$name"
  # 只动本命名空间的库：先删同名残留再建（残留只可能是上次本脚本建的）。
  if "$PSQL_BIN" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -tAc \
      "SELECT 1 FROM pg_database WHERE datname='$db'" | grep -q 1; then
    sql postgres "DROP DATABASE $db"
  fi
  "$PSQL_BIN" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -c "CREATE DATABASE $db" >/dev/null
  sql "$db" "CREATE EXTENSION IF NOT EXISTS pg_search CASCADE"

  local ws="$EINO_DIR/tmp/workspace-p5-$name"
  mkdir -p "$ws"

  local ts; ts="$(date +%s)"
  local es_index="tako_dialogue_eval_${ts}_$name"
  local milvus_coll="tako_dialogue_eval_${ts}_$name"
  [ "${#es_index}" -le 255 ] || die "验收名过长"

  local eval_bin="$dir/tako-dialogue-eval"
  (cd "$EINO_DIR" && PATH="$HOME/sdk/go1.27.0/bin:$PATH" ${GO_BIN} build -o "$eval_bin" ./cmd/tako-dialogue-eval)

  cat > "$dir/state.env" <<EOF
DH_NAME='$name'
DH_PG_DB='$db'
DH_PG_DSN='host=$PG_HOST port=$PG_PORT dbname=$db sslmode=disable'
DH_WS='$ws'
DH_ES_INDEX='$es_index'
DH_MILVUS_COLL='$milvus_coll'
DH_REDIS_ADDR='$REDIS_ADDR'
DH_STATE_DIR='$dir'
DH_EVAL_BIN='$eval_bin'
DH_BIN_DIR='$dir'
EOF
  echo "init ok: db=$db ws=$ws es=$es_index milvus=$milvus_coll state=$dir"
}

cmd_start() {
  local name="$1"; shift
  require_state "$name"
  local dir; dir="$(state_dir "$name")"
  # shellcheck disable=SC1090
  source "$dir/state.env"

  local tag="" port="" redis_db="" mode="hybrid" es_url="" embed_base="" milvus_uri="" task_interval="1s"
  local es_index_ovr="" milvus_coll_ovr=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --tag) tag="$2"; shift 2 ;;
      --port) port="$2"; shift 2 ;;
      --redis-db) redis_db="$2"; shift 2 ;;
      --mode) mode="$2"; shift 2 ;;
      --es-url) es_url="$2"; shift 2 ;;
      --embed-base) embed_base="$2"; shift 2 ;;
      --milvus-uri) milvus_uri="$2"; shift 2 ;;
      --task-interval) task_interval="$2"; shift 2 ;;
      --es-index) es_index_ovr="$2"; shift 2 ;;
      --milvus-coll) milvus_coll_ovr="$2"; shift 2 ;;
      *) die "start 未知参数 $1" ;;
    esac
  done
  [ -n "$tag" ] || die "start 需要 --tag"
  [ -n "$port" ] || die "start 需要 --port"
  [ -n "$redis_db" ] || die "start 需要 --redis-db"
  case "$mode" in hybrid|pg_search) ;; *) die "mode 只能是 hybrid|pg_search" ;; esac

  local engdir="$dir/eng_$tag"
  if [ -f "$engdir.pid" ] && kill -0 "$(cat "$engdir.pid")" 2>/dev/null; then
    die "引擎 $name/$tag 已在跑（pid $(cat "$engdir.pid")）"
  fi
  port_free "$port" || die "换一个 --port 再起"

  mkdir -p "$dir"
  local engenv="$engdir.env"
  cat > "$engenv" <<EOF
DH_TAG='$tag'
DH_PORT='$port'
DH_REDIS_DB='$redis_db'
DH_MODE='$mode'
DH_TASK_INTERVAL='$task_interval'
DH_ES_URL_OVERRIDE='$es_url'
DH_EMBED_BASE_OVERRIDE='$embed_base'
DH_MILVUS_URI_OVERRIDE='$milvus_uri'
EOF

  local bin="$dir/yktako"
  if [ ! -x "$bin" ]; then
    (cd "$EINO_DIR" && PATH="$HOME/sdk/go1.27.0/bin:$PATH" ${GO_BIN} build -o "$bin" ./cmd/yktako)
  fi

  # 引擎完整命令行（重启复用）：活体调度开着（--task-interval 默认 1s）。
  local cmd
  cmd="$(cat <<EOF
set -a
source '$ENV_FILE'
set +a
export TAKO_DIALOGUE_RETRIEVAL='$mode'
# 验收处置：同库混模式副本（pg_search+hybrid）会丢 outbox 登记（已知缺陷，P5 不改引擎），
# 用产品自己的对账器把损失收敛到 10s；多副本真源允许 env 覆盖（dialogue_retrieval.go:229）。
export TAKO_DIALOGUE_RECONCILE_INTERVAL='10s'
export TAKO_ES_INDEX='$DH_ES_INDEX'
export TAKO_MILVUS_COLLECTION='$DH_MILVUS_COLL'
export TAKO_REDIS_ADDR='$REDIS_ADDR'
export TAKO_JWT_SECRET='dev-only-jwt-secret-change-me-32bytes'
export TAKO_DATA_KEY='data-key-aaaaaaaaaaaaaaaaaaaaaaaa32'
export TAKO_LANGFUSE_SAMPLING='1'
${es_url:+export TAKO_ES_URL='$es_url';}
${embed_base:+export TAKO_DIALOGUE_EMBEDDING_BASE_URL='$embed_base';}
${milvus_uri:+export TAKO_MILVUS_URI='$milvus_uri';}
${es_index_ovr:+export TAKO_ES_INDEX='$es_index_ovr';}
${milvus_coll_ovr:+export TAKO_MILVUS_COLLECTION='$milvus_coll_ovr';}
exec '$bin' --task-interval '$task_interval' --engine "127.0.0.1:$port" --pg "$DH_PG_DSN" --redis "redis://$REDIS_ADDR/$redis_db" --workspace "$DH_WS"
EOF
)"
  printf '%s\n' "$cmd" > "$engdir.cmd"
  # 引擎输出进日志文件（不 tail，不丢输出；场景 10 与场景 5 的日志断言要读它）。
  bash -c "$cmd" >> "$engdir.log" 2>&1 </dev/null &
  echo $! > "$engdir.pid"
  # 给引擎一点时间起监听（任何 HTTP 响应即就绪，与 workspace_crash_live 同款探活）。
  # Zilliz serverless 冷启动让启动预检可拖 45s+（P5 实测 33s），给足 150s。
  for _ in $(seq 1 300); do
    if curl -s -m 1 -o /dev/null "http://127.0.0.1:$port/api/auth/login" 2>/dev/null; then
      echo "start ok: $name/$tag pid=$(cat "$engdir.pid") port=$port mode=$mode redis-db=$redis_db"
      return 0
    fi
    kill -0 "$(cat "$engdir.pid")" 2>/dev/null || break   # 进程死了就别傻等
    sleep 0.5
  done
  echo "start FAILED: $name/$tag 150 秒内没等到端口监听（日志 $engdir.log）" >&2
  return 1
}

cmd_stop() {
  local name="$1"; shift
  require_state "$name"
  local dir; dir="$(state_dir "$name")"
  local want=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --tag) want="$2"; shift 2 ;;
      *) die "stop 未知参数 $1" ;;
    esac
  done
  local any=0
  for pidfile in "$dir"/eng_*.pid; do
    [ -f "$pidfile" ] || continue
    local tag; tag="$(basename "$pidfile" .pid)"; tag="${tag#eng_}"
    if [ -n "$want" ] && [ "$tag" != "$want" ]; then continue; fi
    any=1
    local pid; pid="$(cat "$pidfile")"
    if kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
      kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null || true
      echo "stop ok: $name/$tag (pid $pid)"
    else
      echo "stop ok: $name/$tag (pid $pid 已不在)"
    fi
    rm -f "$pidfile"
  done
  [ "$any" = 1 ] || echo "stop ok: $name 无在跑引擎"
}

cmd_restart() {
  local name="$1"; shift
  require_state "$name"
  local dir; dir="$(state_dir "$name")"
  local tag="${1:-}"
  [ -n "$tag" ] || die "restart 需要 --tag <tag>"
  local pidfile="$dir/eng_$tag.pid"
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    cmd_stop "$name" --tag "$tag" >/dev/null
  fi
  local cmdfile="$dir/eng_$tag.cmd"
  [ -f "$cmdfile" ] || die "没有历史命令可重启（$cmdfile 不存在）"
  bash -c "$(cat "$cmdfile")" >> "$dir/eng_$tag.log" 2>&1 </dev/null &
  echo $! > "$pidfile"
  # shellcheck disable=SC1090
  source "$dir/eng_$tag.env"
  for _ in $(seq 1 300); do
    if curl -s -m 1 -o /dev/null "http://127.0.0.1:$DH_PORT/api/auth/login" 2>/dev/null; then
      echo "restart ok: $name/$tag pid=$(cat "$pidfile") port=$DH_PORT"
      return 0
    fi
    kill -0 "$(cat "$pidfile")" 2>/dev/null || break
    sleep 0.5
  done
  echo "restart FAILED: $name/${tag}（日志 $dir/eng_$tag.log）" >&2
  return 1
}

cmd_status() {
  local name="$1"
  require_state "$name"
  local dir; dir="$(state_dir "$name")"
  # shellcheck disable=SC1090
  source "$dir/state.env"
  echo "env $name: db=$DH_PG_DB es=$DH_ES_INDEX milvus=$DH_MILVUS_COLL"
  for pidfile in "$dir"/eng_*.pid; do
    [ -f "$pidfile" ] || continue
    local tag; tag="$(basename "$pidfile" .pid)"; tag="${tag#eng_}"
    local pid; pid="$(cat "$pidfile")"
    if kill -0 "$pid" 2>/dev/null; then
      echo "  engine $tag pid=$pid RUNNING"
    else
      echo "  engine $tag pid=$pid dead"
    fi
  done
}

cmd_destroy() {
  local name="$1"
  local dir; dir="$(state_dir "$name")"
  if [ ! -f "$dir/state.env" ]; then
    echo "destroy ok: $name 无环境可清"
    return 0
  fi
  # shellcheck disable=SC1090
  source "$dir/state.env"

  cmd_stop "$name" >/dev/null || true

  # PG：只 drop 本命名空间的库。
  if "$PSQL_BIN" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -tAc \
      "SELECT 1 FROM pg_database WHERE datname='$DH_PG_DB'" 2>/dev/null | grep -q 1; then
    "$PSQL_BIN" "host=$PG_HOST port=$PG_PORT dbname=postgres sslmode=disable" -c "DROP DATABASE IF EXISTS $DH_PG_DB" >/dev/null 2>&1 || true
    echo "destroy: dropped db $DH_PG_DB"
  fi

  # ES：只删本环境时间戳名（含 _v2/_v3 重建版；版本后缀是下划线，见 dialogueVersionedName=%s_v%d）。
  local es_url es_user es_pass
  es_url="$(grep -E '^TAKO_ES_URL=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  es_user="$(grep -E '^TAKO_ES_USERNAME=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  es_pass="$(grep -E '^TAKO_ES_PASSWORD=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  if [ -n "$es_url" ]; then
    local v
    for v in "" "_v2" "_v3"; do
      local idx="$DH_ES_INDEX$v"
      if curl -s -o /dev/null -w '%{http_code}' -u "$es_user:$es_pass" "$es_url/$idx" | grep -q 200; then
        curl -s -XDELETE -u "$es_user:$es_pass" "$es_url/$idx" >/dev/null
        echo "destroy: dropped ES index $idx"
      fi
    done
  fi

  # Milvus：只 drop 本环境时间戳名（含 _v2/_v3 重建版）。两个坑都要堵：
  #   （1）eval 工具从**进程环境**读 TAKO_MILVUS_URI/TOKEN/DB/USE_TLS，本脚本不 source 整份 .env，
  #        必须显式注入——否则 drop 必失败、静默把 collection 留在实例上，累积顶满 Zilliz 上限；
  #   （2）版本后缀是下划线 `_v2/_v3`（dialogueVersionedName=%s_v%d），不是连字符。
  # Zilliz serverless 冷启动偶发超时（P4 实测 33s+），drop 失败重试 3 次。
  if [ -x "$DH_EVAL_BIN" ]; then
    local m_uri m_tok m_db m_tls
    m_uri="$(grep -E '^TAKO_MILVUS_URI=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
    m_tok="$(grep -E '^TAKO_MILVUS_TOKEN=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
    m_db="$(grep -E '^TAKO_MILVUS_DB=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
    m_tls="$(grep -E '^TAKO_MILVUS_USE_TLS=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
    if [ "$m_tls" = "true" ] || [ "$m_tls" = "TRUE" ]; then m_tls=1; fi
    if [ -n "$m_uri" ]; then
      local v
      for v in "" "_v2" "_v3"; do
        local coll="$DH_MILVUS_COLL$v"
        local dropped=0
        for _ in 1 2 3; do
          if TAKO_MILVUS_URI="$m_uri" TAKO_MILVUS_TOKEN="$m_tok" TAKO_MILVUS_DB="$m_db" TAKO_MILVUS_USE_TLS="$m_tls" \
             "$DH_EVAL_BIN" drop -collection "$coll" >/dev/null 2>&1; then dropped=1; break; fi
          sleep 5
        done
        [ "$dropped" = 1 ] && echo "destroy: dropped Milvus collection $coll"
      done
    fi
  fi

  rm -rf "$DH_WS" "$dir"
  echo "destroy ok: ${name}（工作区与状态目录已删）"
}

cmd="${1:-}"; shift || true
case "$cmd" in
  init)    [ $# -ge 1 ] || die "init 需要 <name>"; cmd_init "$1" ;;
  start)   [ $# -ge 1 ] || die "start 需要 <name>"; cmd_start "$@" ;;
  stop)    [ $# -ge 1 ] || die "stop 需要 <name>"; cmd_stop "$@" ;;
  restart) [ $# -ge 1 ] || die "restart 需要 <name>"; cmd_restart "$@" ;;
  status)  [ $# -ge 1 ] || die "status 需要 <name>"; cmd_status "$1" ;;
  destroy) [ $# -ge 1 ] || die "destroy 需要 <name>"; cmd_destroy "$1" ;;
  *) die "用法: dialogue_hybrid_up.sh init|start|stop|restart|status|destroy ..." ;;
esac
