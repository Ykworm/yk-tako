#!/usr/bin/env bash
# 一键起后端引擎 + 前端 vite（+ 需要时一个内存态 Redis 与一个本地 Postgres），两边日志带前缀混在一个终端里，Ctrl-C 一起停。
#
#   ./scripts/dev.sh                      # 前台跑（唯一会占着终端的用法）
#   ./scripts/dev.sh restart              # 先停掉在跑的那个，再在当前终端前台起
#   ./scripts/dev.sh status               # 有没有在跑（读 pidfile，不猜端口）
#   ./scripts/dev.sh stop                 # 给它发 TERM，走的是和 Ctrl-C 同一条收尾
#   ENGINE_PORT=8905 WEB_PORT=5180 ./scripts/dev.sh
#   REDIS_PORT=6392 REDIS_SERVER_BIN=/path/to/redis-server ./scripts/dev.sh
#   PG_PORT=5434 PG_BIN_DIR=/path/to/pg/bin ./scripts/dev.sh
#   REDPANDA_PORT=9093 REDPANDA_IMAGE=redpandadata/redpanda:vX.Y.Z ./scripts/dev.sh
#   TAKO_MEMORY_MODE=markdown ./scripts/dev.sh   # 不起 Redpanda
#
# 没有 start：后台化会把「前台混排日志」和「Ctrl-C 一起停」这两条脚本的价值都赔掉。
# restart 不是后台化——它就是 stop 之后在当前终端接着前台跑一次，两条都留着。
# 多实例按引擎端口区分 pidfile，stop/restart 命中多个时要 ENGINE_PORT=<端口> 指定。
#
# vite 的 /api 代理目标由 TAKO_ENGINE 决定（web/vite.config.ts 里默认打 8901）。
# 没显式给 TAKO_ENGINE 时，脚本按 ENGINE_PORT 把它填好，两边必然一致；
# 显式给了 TAKO_ENGINE，就以它为准——引擎绑的也是它上面那个端口。
#
# 引擎的存储层是 Redis（tk:runlive/tk:runepoch 两键）+ Postgres，任一不可达就 fail fast 不起进程，
# 所以这里先把两半都备好：端口上已有监听者就复用（可能是你自己留着的那份数据，脚本不碰），
# 空闲才自己起——Redis 一个内存态实例，Postgres 一个 initdb 在 tmp/pg-<端口>/data 的本地实例。
#
# pipeline 记忆模式的抽取队列是 Redpanda（doc/65 §5.2）：默认 127.0.0.1:9092，同一条复用纪律；
# 空闲才用固定版本容器起一份（禁止 latest），topic tako.memory.extract 显式建成 8 分区。
# TAKO_MEMORY_MODE=markdown 整段跳过；调用方显式给了 TAKO_REDPANDA_BROKERS 就以它为准，不起本地容器。
#
# 登录和租户密钥封存也在这一条链上：没设 TAKO_JWT_SECRET（至少 32 字节）时引擎在 auth 处退出。
# 脚本只在调用方没给够长的值时填开发用默认值，不写进 .env。TAKO_DATA_KEY 同理，新的租户密钥写成 seal2。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/yk-tako-eino"

export PATH="$HOME/sdk/go1.27.0/bin:$PATH"
export TAKO_ENGINE="${TAKO_ENGINE:-http://127.0.0.1:${ENGINE_PORT:-8901}}"
engine_port="${TAKO_ENGINE##*:}"
web_port="${WEB_PORT:-5173}"
redis_addr="${REDIS_ADDR:-127.0.0.1:${REDIS_PORT:-6390}}"
redis_host="${redis_addr%:*}"
redis_port="${redis_addr##*:}"
pg_port="${PG_PORT:-5433}"
pg_addr="127.0.0.1:$pg_port"
pg_base="$repo_root/tmp/pg-$pg_port"
pg_data_dir="$pg_base/data"
pg_log="$pg_base/postgres.log"
pg_bin_dir=""   # 空闲时按需解析（PG_BIN_DIR → PATH → 本机已知路径）
pg_started=""   # 只对「本脚本起的」实例置位：收尾 pg_ctl stop 只认它
memory_mode="${TAKO_MEMORY_MODE:-pipeline}"
redpanda_port="${REDPANDA_PORT:-9092}"
redpanda_image="${REDPANDA_IMAGE:-redpandadata/redpanda:v25.3.18}"
container_bin=""       # docker 或 podman，按需解析
redpanda_container=""  # 本脚本起的容器的唯一名称
redpanda_started=""    # 只对「本脚本起的」容器置位：收尾 stop/rm 只认它

# 前台跑着的这个脚本把自己登记成 pidfile，另一个终端里的 stop/status 才找得到它。
# 按引擎端口命名，同机多实例（8901 / 8914 …）互不覆盖。
dev_run_dir="${DEV_RUN_DIR:-$repo_root/tmp/dev}"
pid_file="$dev_run_dir/dev-$engine_port.pid"

# 活的 dev.sh 主进程：一行一个 `<pid> <引擎端口>`；死掉的或名不副实的 pidfile 顺手清掉。
alive_dev_pids() {
  local f pid port
  for f in "$dev_run_dir"/dev-*.pid; do
    if [ ! -f "$f" ]; then continue; fi
    port="${f##*/dev-}"
    port="${port%.pid}"
    pid="$(head -1 "$f" 2>/dev/null || true)"
    if [ -n "$pid" ] && ps -p "$pid" -o command= 2>/dev/null | grep -q 'scripts/dev\.sh'; then
      printf '%s %s\n' "$pid" "$port"
    else
      rm -f "$f"
    fi
  done
}

# 只报事实，不动任何进程：端口上的监听者可能是你自己留着的服务。
cmd_status() {
  local report pid port
  report="$(alive_dev_pids || true)"
  if [ -z "$report" ]; then
    echo "没在跑（$dev_run_dir 下没有活的 dev.sh pidfile）。"
    return 0
  fi
  while read -r pid port; do
    if [ -n "$pid" ]; then echo "在跑：dev.sh PID ${pid}，引擎 http://127.0.0.1:${port}"; fi
  done <<<"$report"
}

# stop 只发 TERM 给登记过的那个主进程，收尾仍由它的 trap 负责——不在这里数端口、挑进程杀。
cmd_stop() {
  local -a pids ports
  local pid port i n chosen="" want="${ENGINE_PORT:-8901}"
  while read -r pid port; do
    if [ -n "$pid" ]; then pids+=("$pid"); ports+=("$port"); fi
  done <<<"$(alive_dev_pids || true)"
  n="${#pids[@]}"

  if [ "$n" = 0 ]; then
    echo "没在跑（$dev_run_dir 下没有活的 dev.sh pidfile）。"
    return 0
  fi
  if [ "$n" = 1 ]; then
    chosen=0
  else
    for i in "${!pids[@]}"; do
      if [ "${ports[$i]}" = "$want" ]; then chosen="$i"; break; fi
    done
    if [ -z "$chosen" ]; then
      echo "有 $n 个 dev.sh 在跑（引擎端口 ${ports[*]}）；用 ENGINE_PORT=<端口> ./scripts/dev.sh stop 指一个。" >&2
      return 1
    fi
  fi

  echo "TERM → dev.sh PID ${pids[$chosen]}（引擎端口 ${ports[$chosen]}）…"
  kill -TERM "${pids[$chosen]}" 2>/dev/null || true
  for _ in $(seq 40); do
    if ! kill -0 "${pids[$chosen]}" 2>/dev/null; then break; fi
    sleep 0.25
  done
  if kill -0 "${pids[$chosen]}" 2>/dev/null; then
    echo "10s 内没退出（它的收尾可能卡在某个子进程上）；确认过再手动 kill -9 ${pids[$chosen]}。" >&2
    return 1
  fi
  echo "已停。"
}

running_on_port() {
  local pid port
  while read -r pid port; do
    if [ -n "$pid" ] && [ "$port" = "$1" ]; then return 0; fi
  done <<<"$(alive_dev_pids || true)"
  return 1
}

case "${1:-run}" in
  run)
    if running_on_port "$engine_port"; then
      echo "引擎端口 $engine_port 上已经有本脚本的实例在跑；先 ./scripts/dev.sh stop，或换 ENGINE_PORT。" >&2
      exit 1
    fi
    ;;
  stop)
    cmd_stop
    exit $?
    ;;
  restart)
    # 停 + 在当前终端前台再起一次：收尾仍由被停那个实例的 trap 负责，这里不数端口。
    cmd_stop || exit 1
    ;;
  status)
    cmd_status
    exit 0
    ;;
  *)
    echo "用法：./scripts/dev.sh [stop|restart|status]；前台起不要参数。" >&2
    exit 2
    ;;
esac

# 端口上是别人留着的服务就报错退出，不杀——这是协作约定，不是技术限制。
require_free() {
  local port="$1" who="$2" pid
  pid="$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
  if [ -n "$pid" ]; then
    echo "$who 要用的端口 $port 已被 PID $pid 占用；先自己处理，或换端口再跑。" >&2
    exit 1
  fi
}
require_free "$engine_port" "引擎"
require_free "$web_port" "前端"

# vite 7 要 node >= 20.19，而非交互 shell 里 nvm 没加载，PATH 上的 node 常是 /usr/local/bin 的旧版。
if ! node -e 'const [M, m] = process.versions.node.split(".").map(Number); process.exit(M > 20 || (M === 20 && m >= 19) ? 0 : 1)' 2>/dev/null; then
  newest_nvm_node="$(ls -d "$HOME"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -1 || true)"
  if [ -z "$newest_nvm_node" ]; then
    echo "vite 需要 node >= 20.19，当前是 $(node -v)，且 ~/.nvm 下没有可用的新版本。" >&2
    exit 1
  fi
  export PATH="$newest_nvm_node:$PATH"
fi

if [ ! -d web/node_modules ]; then
  (cd web && npm install)
fi

run() { # run <标签> <颜色> <目录> <命令...>
  local tag="$1" color="$2" dir="$3"
  shift 3
  (cd "$dir" && exec "$@" 2>&1) |
    awk -v tag="$tag" -v color="$color" '{ printf "%s[%s] %s%s\n", color, tag, $0, "\033[0m"; fflush() }' &
}

# 收尾只动这个脚本自己起的东西：直接子进程（含自己起的 Redis）+ 开局时确认空闲的那两个端口上的监听者。
# Redis 端口不进这个循环：复用的可能是别人留着的数据，只能靠 pkill -P 收走自己那份子进程。
# Postgres 同理不进循环：pg_ctl 起的 postmaster 不在本脚本进程树上（pkill -P 收不到），
# 只对自己置位过 pg_started 的那份显式 `pg_ctl stop`；复用的实例绝不碰。
# Redpanda 容器同理：只 stop/rm 置位过 redpanda_started 的那个唯一名称；复用的监听者绝不碰。
shutdown() {
  trap - INT TERM EXIT
  rm -f "$pid_file"
  pkill -P $$ 2>/dev/null || true
  for port in "$engine_port" "$web_port"; do
    for pid in $(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true); do
      kill "$pid" 2>/dev/null || true
    done
  done
  if [ -n "$pg_started" ]; then
    "$pg_bin_dir/pg_ctl" -D "$pg_data_dir" -m fast stop >/dev/null 2>&1 || true
  fi
  if [ -n "$redpanda_started" ]; then
    "$container_bin" stop "$redpanda_container" >/dev/null 2>&1 || true
    "$container_bin" rm "$redpanda_container" >/dev/null 2>&1 || true
  fi
}
trap shutdown INT TERM EXIT

mkdir -p "$dev_run_dir"
printf '%s\n' "$$" > "$pid_file"

# Redis 是引擎临时态的存储层（tk:runlive/tk:runepoch 两键），main.go 连不上就 fail fast 不进监听，
# 所以必须在引擎之前备好。端口上已有监听者就复用——那可能是你自己留着的那份数据，脚本不杀；
# 自己起的那个由收尾的 pkill -P 收走。
redis_listener="$(lsof -tiTCP:"$redis_port" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
if [ -n "$redis_listener" ]; then
  echo "Redis ${redis_addr} 已有监听者（PID ${redis_listener}），直接复用。"
else
  redis_bin="${REDIS_SERVER_BIN:-}"
  if [ -z "$redis_bin" ]; then
    for cand in "$(command -v redis-server 2>/dev/null || true)" \
      /opt/homebrew/bin/redis-server /usr/local/bin/redis-server; do
      if [ -x "$cand" ]; then redis_bin="$cand"; break; fi
    done
  fi
  if [ -z "$redis_bin" ]; then
    echo "端口 $redis_port 空闲，但找不到 redis-server：brew install redis，或用 REDIS_SERVER_BIN 指定路径，或用 REDIS_ADDR 指向已在跑的实例。" >&2
    exit 1
  fi
  # --save '' --appendonly no：dev 只要内存态，退出不会往 cwd 落 dump.rdb，每次起来都是干净键空间。
  run Redis '\033[33m' "$repo_root/yk-tako-eino" \
    "$redis_bin" --port "$redis_port" --bind "$redis_host" --save '' --appendonly no
  for _ in $(seq 40); do
    if lsof -tiTCP:"$redis_port" -sTCP:LISTEN >/dev/null 2>&1; then break; fi
    sleep 0.25
  done
  if ! lsof -tiTCP:"$redis_port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "Redis 没能在 $redis_addr 上监听起来，看上面 [Redis] 的日志。" >&2
    exit 1
  fi
fi

# Postgres 是引擎永久状态的存储层（sessions/runs/run_model_messages/runlog_entries 四表），
# 与 Redis 同一条纪律：已有监听者就复用（可能是你自己留着的实例，脚本不碰不建不写）；
# 空闲才自己起——首跑 initdb 到 tmp/pg-<端口>/data，此后直接 pg_ctl start。
pg_listener="$(lsof -tiTCP:"$pg_port" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
if [ -n "$pg_listener" ]; then
  echo "Postgres ${pg_addr} 已有监听者（PID ${pg_listener}），直接复用。"
else
  if [ -z "$pg_bin_dir" ]; then
    for cand in "$(dirname "$(command -v psql 2>/dev/null || echo .)" 2>/dev/null)" \
      /opt/homebrew/opt/postgresql@17/bin /opt/homebrew/bin /usr/local/opt/postgresql@17/bin \
      "$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin"; do
      if [ -x "$cand/initdb" ] && [ -x "$cand/pg_ctl" ]; then pg_bin_dir="$cand"; break; fi
    done
  fi
  if [ -z "$pg_bin_dir" ]; then
    echo "端口 $pg_port 空闲，但找不到 initdb/pg_ctl：装个 Postgres，或用 PG_BIN_DIR 指定 bin 目录，或用 PG_PORT 指向已有实例。" >&2
    exit 1
  fi
  if [ ! -f "$pg_data_dir/PG_VERSION" ]; then
    mkdir -p "$pg_base"
    "$pg_bin_dir/initdb" -D "$pg_data_dir" --no-locale --encoding=UTF8 >/dev/null
    echo "initdb → ${pg_data_dir}（首次拉起，数据在仓的 tmp/ 下）"
  fi
  "$pg_bin_dir/pg_ctl" -D "$pg_data_dir" -l "$pg_log" \
    -o "-p $pg_port -k /tmp -c listen_addresses=127.0.0.1" -w start >/dev/null
  pg_started=1
  if ! "$pg_bin_dir/pg_isready" -h 127.0.0.1 -p "$pg_port" >/dev/null 2>&1; then
    echo "Postgres 没能在 ${pg_addr} 上就绪，看 ${pg_log}。" >&2
    exit 1
  fi
  # tako 库懒建：只在自己起的实例上建（复用别人的实例不写一个字）。
  if ! "$pg_bin_dir/psql" -h 127.0.0.1 -p "$pg_port" -d postgres -tAc \
    "SELECT 1 FROM pg_database WHERE datname='tako'" | grep -q 1; then
    "$pg_bin_dir/createdb" -h 127.0.0.1 -p "$pg_port" tako
  fi
fi

# Redpanda 是 pipeline 记忆模式的抽取队列（doc/65 §5.2）。markdown 模式整段跳过——
# 引擎在这个模式下不装配消费者，broker 环境变量留着也不生效。
# 与 Redis/PG 同一条纪律：端口上已有监听者只复用（可能是你自己留着的 broker 和数据，脚本不碰）；
# 空闲才自己起一个固定版本容器，唯一名称，收尾只 stop/rm 自己起的那份。
if [ "$memory_mode" != "markdown" ]; then
  if [ -n "${TAKO_REDPANDA_BROKERS:-}" ]; then
    # 调用方显式指向了某个 broker（比如远程真集群），以它为准，不起本地容器。
    echo "Redpanda 用调用方给的 TAKO_REDPANDA_BROKERS，不起本地容器。"
  else
    redpanda_listener="$(lsof -tiTCP:"$redpanda_port" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
    if [ -n "$redpanda_listener" ]; then
      echo "Redpanda 127.0.0.1:${redpanda_port} 已有监听者（PID ${redpanda_listener}），直接复用。"
      echo "复用实例的 topic tako.memory.extract 需自行确认为 8 分区；引擎启动时还会探 topic 存在性（不存在即退出）。"
    else
      container_bin="$(command -v docker 2>/dev/null || command -v podman 2>/dev/null || true)"
      if [ -z "$container_bin" ]; then
        echo "端口 $redpanda_port 空闲，但找不到 docker/podman：装一个容器运行时，或用 REDPANDA_PORT 指向已有 broker，或 export TAKO_REDPANDA_BROKERS 指向远程集群，或 TAKO_MEMORY_MODE=markdown 跳过。" >&2
        exit 1
      fi
      redpanda_container="tako-dev-redpanda-${redpanda_port}-$$"
      redpanda_started=1
      "$container_bin" run -d --name "$redpanda_container" \
        -p "127.0.0.1:${redpanda_port}:9092" \
        "$redpanda_image" \
        redpanda start --mode dev-container --smp 1 --memory 1G \
        --kafka-addr PLAINTEXT://0.0.0.0:9092 \
        --advertise-kafka-addr "PLAINTEXT://127.0.0.1:${redpanda_port}" >/dev/null
      # 等 Kafka API 真正可用（容器起跑 ≠ broker 就绪），最多 60s。
      redpanda_ready=""
      for _ in $(seq 120); do
        if "$container_bin" exec "$redpanda_container" rpk cluster info >/dev/null 2>&1; then
          redpanda_ready=1
          break
        fi
        if [ "$("$container_bin" inspect -f '{{.State.Running}}' "$redpanda_container" 2>/dev/null || true)" != "true" ]; then
          echo "Redpanda 容器退出了，看日志：$container_bin logs $redpanda_container" >&2
          exit 1
        fi
        sleep 0.5
      done
      if [ -z "$redpanda_ready" ]; then
        echo "Redpanda 60s 内没就绪，看日志：$container_bin logs $redpanda_container" >&2
        exit 1
      fi
      # 显式建 8 分区，不依赖自动创建（自动创建的分区数随 broker 配置走，可能不是 8）。
      # 已存在时 create 报错可忽略，下面的 describe 会把关分区数。
      "$container_bin" exec "$redpanda_container" rpk topic create tako.memory.extract --partitions 8 >/dev/null 2>&1 || true
      redpanda_parts="$("$container_bin" exec "$redpanda_container" rpk topic describe tako.memory.extract 2>/dev/null | awk '/^PARTITIONS/ {print $2}')"
      if [ "$redpanda_parts" != "8" ]; then
        echo "topic tako.memory.extract 分区数不是 8（读到：${redpanda_parts:-空}）；请自行处理后重跑。" >&2
        exit 1
      fi
      echo "Redpanda 127.0.0.1:${redpanda_port} 就绪（容器 ${redpanda_container}，topic tako.memory.extract 8 分区）。"
    fi
    export TAKO_REDPANDA_BROKERS="127.0.0.1:${redpanda_port}"
  fi
fi

# 调用方已经 export 的值优先。这里的两把只给本机开发，不打印内容。
jwt_secret="${TAKO_JWT_SECRET-}"
if [ "${#jwt_secret}" -lt 32 ]; then
  export TAKO_JWT_SECRET='dev-only-jwt-secret-change-me-32bytes'
  echo "TAKO_JWT_SECRET 不够 32 字节，这次用开发默认值。要换自己的，先 export 再跑。"
fi
data_key="${TAKO_DATA_KEY-}"
if [ "${#data_key}" -lt 32 ]; then
  export TAKO_DATA_KEY='data-key-aaaaaaaaaaaaaaaaaaaaaaaa32'
  echo "TAKO_DATA_KEY 不够 32 字节，这次用开发默认值。新的租户密钥写成 seal2，已有的 seal1 仍用登录密钥解开。"
fi

run 引擎 '\033[36m' "$repo_root/yk-tako-eino" \
  go run ./cmd/yktako -engine "127.0.0.1:$engine_port" -redis "$redis_addr" \
  -pg "host=127.0.0.1 port=$pg_port dbname=tako sslmode=disable" \
  -workspace "$repo_root/tmp/workspace"
run 前端 '\033[35m' "$repo_root/yk-tako-eino/web" \
  npm run dev -- --host 127.0.0.1 --port "$web_port" --strictPort

echo "前端 http://127.0.0.1:$web_port  →  引擎 http://127.0.0.1:$engine_port  (redis $redis_addr, pg $pg_addr/tako, memory=$memory_mode)"

wait
