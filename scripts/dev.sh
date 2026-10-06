#!/usr/bin/env bash
# 一键起后端引擎 + 前端 vite，两边日志带前缀混在一个终端里，Ctrl-C 一起停。
#
#   ./scripts/dev.sh
#   ENGINE_PORT=8905 WEB_PORT=5180 ./scripts/dev.sh
#
# vite 的 /api 代理目标由 TAKO_ENGINE 决定（web/vite.config.ts 里默认打 8901）。
# 没显式给 TAKO_ENGINE 时，脚本按 ENGINE_PORT 把它填好，两边必然一致；
# 显式给了 TAKO_ENGINE，就以它为准——引擎绑的也是它上面那个端口。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/yk-tako-eino"

export PATH="$HOME/sdk/go1.27.0/bin:$PATH"
export TAKO_ENGINE="${TAKO_ENGINE:-http://127.0.0.1:${ENGINE_PORT:-8901}}"
engine_port="${TAKO_ENGINE##*:}"
web_port="${WEB_PORT:-5173}"

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

# 收尾只动这个脚本自己起的东西：直接子进程 + 开局时确认空闲的那两个端口上的监听者。
shutdown() {
  trap - INT TERM EXIT
  pkill -P $$ 2>/dev/null || true
  for port in "$engine_port" "$web_port"; do
    for pid in $(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true); do
      kill "$pid" 2>/dev/null || true
    done
  done
}
trap shutdown INT TERM EXIT

run 引擎 '\033[36m' "$repo_root/yk-tako-eino" \
  go run ./cmd/yktako -engine "127.0.0.1:$engine_port"
run 前端 '\033[35m' "$repo_root/yk-tako-eino/web" \
  npm run dev -- --host 127.0.0.1 --port "$web_port" --strictPort

echo "前端 http://127.0.0.1:$web_port  →  引擎 http://127.0.0.1:$engine_port"

wait
