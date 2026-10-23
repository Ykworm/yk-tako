#!/usr/bin/env bash
# SSE 重连自愈的活体验收（doc/46 的事件流 · 2026-10-23 复审新增）。
#
# 验的是一件很隐蔽的事：**页面静止时**没人调用 api 层 → 没人去刷新那个短效
# access token；而 SSE 连接恰好断了（引擎重启 / 网络抖动 / 代理超时）→
# onerror 重建时用的还是**过期**的 token → 服务端一律 401 → 再 onerror → ……
# 无限重连，把引擎日志刷满（实测未修复时每 3 秒一次 401、零次 200）。
#
# 修好之后：onerror 先自己刷一次 token，再用新 token 重建 → 401 只出现一次。
#
# 用法：
#   bash scripts/acceptance/sse_reconnect.sh
#
# 依赖：node（playwright 已装在 web/ 下）、go、curl。一个隔离引擎实例
# （端口 8944、库 tako_acc、redis db 9、workspace 在 tmp/workspace-sse）。
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"

ENGINE_PORT=8944
WEB_PORT=5174
BIN=/tmp/yktako-acc
LOG=/tmp/yktako-acc-sse.log
ENGINE="http://127.0.0.1:${ENGINE_PORT}"
WEB="http://127.0.0.1:${WEB_PORT}"

cleanup() {
  pkill -f "engine 127.0.0.1:${ENGINE_PORT}" 2>/dev/null || true
  pkill -f "vite.*--port ${WEB_PORT}" 2>/dev/null || true
}
trap cleanup EXIT

echo "── 编译隔离引擎（短 access token TTL）"
(cd "$repo/yk-tako-eino" && PATH="$HOME/sdk/go1.27.0/bin:$PATH" go build -o "$BIN" ./cmd/yktako) || exit 1

# 隔离库要有 pg_search 扩展（少了引擎起不来）
PGBIN="${TAKO_PGBIN:-$HOME/dev/ykcode/learn-golang/otel-ch-native/bin/postgres/bin}"
if [ -x "$PGBIN/psql" ]; then
  "$PGBIN/createdb" -h 127.0.0.1 -p 5433 tako_acc 2>/dev/null || true
  "$PGBIN/psql" -h 127.0.0.1 -p 5433 -d tako_acc -c "CREATE EXTENSION pg_search CASCADE;" >/dev/null 2>&1 || true
fi

: > "$LOG"
echo "── 起隔离引擎（access token 20 秒过期）"
# **必须在 yk-tako-eino/ 目录下起**：引擎从 cwd 找 .env（模型密钥），
# 找不到就「没有可用的模型厂商」直接退出——上一次就栽在这儿。
(cd "$repo/yk-tako-eino" && \
  TAKO_JWT_SECRET='dev-only-jwt-secret-change-me-32bytes' \
  TAKO_DATA_KEY='data-key-aaaaaaaaaaaaaaaaaaaaaaaa32' \
  TAKO_ACCESS_TTL=20s \
    nohup "$BIN" -engine "127.0.0.1:${ENGINE_PORT}" \
      -pg "host=127.0.0.1 port=5433 dbname=tako sslmode=disable" \
      -redis "redis://127.0.0.1:6390/9" \
      -workspace "$repo/tmp/workspace-sse" >> "$LOG" 2>&1 &)

echo "── 起 vite（代理到隔离引擎）"
(cd "$repo/yk-tako-eino/web" && TAKO_ENGINE="$ENGINE" nohup npm run dev -- --host 127.0.0.1 --port "${WEB_PORT}" --strictPort > "$repo/tmp/vite-${WEB_PORT}.log" 2>&1 &)

engine_code=000
for _ in $(seq 1 40); do
  engine_code=$(curl -s -o /dev/null -w '%{http_code}' "$ENGINE/api/auth/me" 2>/dev/null)
  [ "$engine_code" = "401" ] && break
  sleep 1
done
if [ "$engine_code" != "401" ]; then
  echo "隔离引擎没起来（$ENGINE/api/auth/me = $engine_code），日志尾："
  tail -5 "$LOG"
  exit 1
fi
for _ in $(seq 1 40); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$WEB/" 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 1
done
[ "$code" = "200" ] || { echo "vite 没起来（$WEB）"; exit 1; }

echo "── 跑验证（要等 token 过期 + 断线 + 自愈，约 50 秒）"
cd "$repo/yk-tako-eino/web"
TAKO_WEB_BASE="$WEB" TAKO_ENGINE_BASE="$ENGINE" TAKO_ENGINE_BIN="$BIN" \
TAKO_SSE_LOG="$LOG" TAKO_REPO="$repo" \
  node "$repo/scripts/acceptance/sse_reconnect.mjs"
rc=$?

if [ "$rc" -eq 0 ]; then
  echo "✅ SSE 重连自愈：断线后 401 有界、事件流恢复"
else
  echo "❌ SSE 重连自愈验收失败"
fi
exit "$rc"
