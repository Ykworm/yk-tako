#!/usr/bin/env bash
# 起专用隔离引擎实例给 scripts/eval 用（施工包 §6.1）。
#
# 参数全部来自环境：
#   TAKO_EVAL_PG_DSN    专用 PG 库 DSN（必填，如 host=127.0.0.1 port=5433 dbname=tako_eval sslmode=disable）
#   TAKO_EVAL_REDIS_URL 专用 Redis（必填，如 redis://127.0.0.1:6390/14）
#   TAKO_EVAL_PORT      引擎端口，默认 8973；被占用先 lsof 查明并退出，不杀已有进程
#   TAKO_ENV            模型密钥 .env 路径（必填，只载入进程环境，不打印不复制）
#
# 产物（tmp/eval/runtime-<ts>/）：engine.pid、engine.log、env.sh（不含密钥）、
# mcp.json（{"mcpServers":{}}）、workspace/、system-skills/（空）。
# 配置指纹（commit/dirty/mcp.json SHA-256/源码树哈希）写进 env.sh，供 run_eval 采集。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
pg_dsn="${TAKO_EVAL_PG_DSN:?请设 TAKO_EVAL_PG_DSN（专用 PG 库 DSN）}"
redis_url="${TAKO_EVAL_REDIS_URL:?请设 TAKO_EVAL_REDIS_URL（专用 Redis，建议独立 db）}"
port="${TAKO_EVAL_PORT:-8973}"
env_file="${TAKO_ENV:?请设 TAKO_ENV（模型密钥 .env 路径）}"
[ -f "$env_file" ] || { echo "TAKO_ENV 指向的文件不存在：$env_file" >&2; exit 1; }
# 转绝对路径：引擎从运行目录启动，相对路径在启动子 shell 里解析不到。
case "$env_file" in
  /*) ;;
  *) env_file="$(cd "$(dirname "$env_file")" && pwd)/$(basename "$env_file")" ;;
esac

# 端口纪律：查是谁占的，退出让调用者决定，绝不杀别人进程。
busy_pid="$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | head -1 || true)"
if [ -n "$busy_pid" ]; then
  echo "端口 $port 已被 PID $busy_pid 占用；先自行处理或换 TAKO_EVAL_PORT。" >&2
  exit 1
fi

ts="$(date +%Y%m%d-%H%M%S)"
run_dir="$repo_root/tmp/eval/runtime-$ts"
mkdir -p "$run_dir/workspace" "$run_dir/system-skills"

# 构建引擎二进制（与 dev 同一条 Go 工具链）。
export PATH="$HOME/sdk/go1.27.0/bin:$PATH"
( cd "$repo_root/yk-tako-eino" && go build -o "$run_dir/yktako" ./cmd/yktako )

# 空 MCP 配置：不读取/改写开发用 yk-tako-eino/mcp.json。
printf '{"mcpServers":{}}\n' > "$run_dir/mcp.json"
mcp_sha="$(shasum -a 256 "$run_dir/mcp.json" | awk '{print $1}')"

# 配置指纹：引擎 commit/dirty + 相关源码树哈希（cmd/yktako、internal/config、
# internal/runengine、agents 的相对路径+字节；不读 .env/workspace/tmp）。
commit="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || echo unversioned)"
dirty="0"
if [ "$commit" != "unversioned" ] && [ -n "$(git -C "$repo_root" status --porcelain 2>/dev/null)" ]; then
  dirty="1"
fi
tree_sha="$(python3 - "$repo_root" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
dirs = ["yk-tako-eino/cmd/yktako", "yk-tako-eino/internal/config",
        "yk-tako-eino/internal/runengine", "yk-tako-eino/agents"]
h = hashlib.sha256()
for d in dirs:
    for base, _subdirs, files in os.walk(os.path.join(root, d)):
        _subdirs.sort()
        for name in sorted(files):
            p = os.path.join(base, name)
            h.update(os.path.relpath(p, root).encode())
            h.update(b"\0")
            with open(p, "rb") as fh:
                h.update(fh.read())
            h.update(b"\0")
print(h.hexdigest())
PY
)"

# 引擎环境：markdown 记忆 + RAG off（不依赖 Redpanda）；密钥不够长就给一次性随机值，
# 只进引擎进程环境，不落 env.sh。
jwt_secret="${TAKO_JWT_SECRET-}"
if [ "${#jwt_secret}" -lt 32 ]; then
  jwt_secret="eval-jwt-$(openssl rand -hex 24)"
fi
data_key="${TAKO_DATA_KEY-}"
if [ "${#data_key}" -lt 32 ]; then
  data_key="eval-data-$(openssl rand -hex 24)"
fi

# 从运行目录启动二进制；mcp.json 相对 cwd 生效；TAKO_ENV 在启动子 shell 里载入，
# 供模型解析与 Langfuse 对照使用——不打印、不复制其内容。
(
  cd "$run_dir"
  set -a
  # shellcheck disable=SC1090
  . "$env_file"
  set +a
  export TAKO_MEMORY_MODE=markdown
  export TAKO_RAG=off
  export TAKO_JWT_SECRET="$jwt_secret"
  export TAKO_DATA_KEY="$data_key"
  exec ./yktako \
    -engine "127.0.0.1:$port" \
    -redis "$redis_url" \
    -pg "$pg_dsn" \
    -workspace "$run_dir/workspace" \
    -agents-dir "$repo_root/yk-tako-eino/agents" \
    -system-skills-dir "$run_dir/system-skills" \
    -dev=false
) > "$run_dir/engine.log" 2>&1 &
engine_pid=$!
echo "$engine_pid" > "$run_dir/engine.pid"

# 等 HTTP 就绪：未认证的 GET /api/workspaces 预期 401；其他连接失败继续等。
ready=""
for _ in $(seq 1 120); do
  if ! kill -0 "$engine_pid" 2>/dev/null; then
    echo "引擎进程已退出，看 $run_dir/engine.log" >&2
    exit 1
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$port/api/workspaces" 2>/dev/null || echo 000)"
  if [ "$code" = "401" ]; then ready=1; break; fi
  sleep 0.5
done
if [ -z "$ready" ]; then
  echo "引擎 60s 内未就绪（最后 HTTP ${code}），看 ${run_dir}/engine.log" >&2
  exit 1
fi

# env.sh：只放地址与指纹，不放任何密钥。
cat > "$run_dir/env.sh" <<EOF
# 由 scripts/eval/up.sh 生成；只含地址与配置指纹，不含密钥。
RUN_DIR="$run_dir"
ENGINE_PID="$engine_pid"
TAKO_BASE="http://127.0.0.1:$port"
TAKO_WS_ROOT="$run_dir/workspace"
TAKO_EVAL_PG_DSN="$pg_dsn"
TAKO_EVAL_REDIS_URL="$redis_url"
TAKO_EVAL_ENV_SH="$run_dir/env.sh"
ENGINE_COMMIT="$commit"
ENGINE_DIRTY="$dirty"
MCP_JSON_SHA256="$mcp_sha"
TREE_SHA256="$tree_sha"
EOF

echo "隔离引擎就绪：http://127.0.0.1:${port}（PID ${engine_pid}，运行目录 ${run_dir}）"
echo "后续命令前先载入：set -a; . \"$run_dir/env.sh\"; set +a"
echo "（再按需 export TAKO_ENV=$env_file 与 DASHSCOPE_API_KEY）"
