#!/usr/bin/env bash
# 多副本文件写入竞争活体（doc/65 §13.3-4 / doc/71）：两个隔离引擎共连同一 PG、共享 workspace，
# 用真实文件字节验证四件事：
#   1. 跨进程锁串行——引擎 A 持锁期间引擎 B 取同一文件的锁要等（dev 观测口计时）；
#   2. 写前版本拒绝——run 内模型 read_file 之后、write_file 之前，别人在盘上改了这个文件，
#      write_file 必须被拒且文件保持别人的版本（模型不能凭记忆整份回写）；
#   3. 原子替换可见性——write_file 落盘期间一直盯着文件的读者，看到的要么是完整旧版、
#      要么是完整新版，永远看不到半截；
#   4. 崩溃释放——持锁进程被 kill -9，锁立刻可再取（flock 随进程死亡释放，不留死锁）。
#
# markdown 模式起引擎（不连 broker、不打 MQ）——这条在没有 broker 的机器上也能跑。
# 本脚本自己起停两台隔离引擎（kill 只打自己起的 PID）：
#   靶子库：TAKO_PG_DSN 指向一个空的隔离 PG 库（两台引擎共用；脚本在里面建 pg_search 扩展）。
#   Redis：TAKO_REDIS_URL（默认 redis://127.0.0.1:6390/9，避开 migrate 脚本用的 /8）。
#   端口：TAKO_ENGINE_PORT_A / TAKO_ENGINE_PORT_B（默认 8954 / 8955，被占就报错退出）。
#   workspace：脚本自建临时目录、两台引擎共用，退出即删（文件字节断言要在受控根里做）。
# 其余参数：TAKO_PSQL / TAKO_GO / TAKO_ENV（模型密钥，版本拒绝与原子读两场要真模型）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8954}"
PORT_B="${TAKO_ENGINE_PORT_B:-8955}"
REDIS_URL="${TAKO_REDIS_URL:-redis://127.0.0.1:6390/9}"
GO="${TAKO_GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  GO="$HOME/sdk/go1.27.0/bin/go"
  [ -x "$GO" ] || { echo "找不到 go（用 TAKO_GO 指定）" >&2; exit 2; }
fi
for p in "$PORT_A" "$PORT_B"; do
  if lsof -tiTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "端口 $p 已被占用；换 TAKO_ENGINE_PORT_A/B（不杀别人的进程）" >&2; exit 2
  fi
done

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-file-race.XXXXXX")
BIN="$SCRATCH/yktako"
WS="$SCRATCH/workspace"
ELOG_A="$SCRATCH/engine-a.log"
ELOG_B="$SCRATCH/engine-b.log"
PID_A=""; PID_B=""
cleanup(){
  for pid in "$PID_A" "$PID_B"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null
      for _ in $(seq 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
      kill -9 "$pid" 2>/dev/null
    fi
  done
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

export TAKO_JWT_SECRET="${TAKO_JWT_SECRET:-dev-only-jwt-secret-change-me-32bytes}"
export TAKO_DATA_KEY="${TAKO_DATA_KEY:-data-key-aaaaaaaaaaaaaaaaaaaaaaaa32}"

# 两台引擎同一 JWT 密钥：A 签发的 token 在 B 上同样有效（多副本同身份，正是要验的场景）。
start_engine(){ # $1=port $2=log
  # 从 yk-tako-eino 起引擎：引擎在 cwd 找 .env（模型厂商目录），脚本目录下没有它就起不来。
  # exec 让子 shell 变身引擎进程——返回的 PID 就是引擎 PID。
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    export TAKO_MEMORY_MODE=markdown
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS"
  ) >>"$2" 2>&1 &
  local pid=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 30s 没起来，日志：$2" >&2; exit 2
}

echo "引擎 A 127.0.0.1:$PORT_A · 引擎 B 127.0.0.1:$PORT_B · 共库 $DSN · 共享 workspace $WS"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
PID_A=$(start_engine "$PORT_A" "$ELOG_A")
PID_B=$(start_engine "$PORT_B" "$ELOG_B")
BA="http://127.0.0.1:$PORT_A"
BB="http://127.0.0.1:$PORT_B"
B="$BA"   # lib 的注册/投递/拉帧全走 B；本脚本自带引擎，把它指到引擎 A（§3 会临时切到 B）
ml_signup filerace

# dev 锁观测口：POST /api/dev/file-lock-check {"path","holdMs"} → 拿锁、持 holdMs、回实际等了多久。
# 锁与工具层同一处（flock，共享盘上的 .tako/locks）；两台引擎共享 PG → dev 会话同目录 → 同一把锁。
lockcheck(){ # $1=base $2=path $3=holdMs
  curl -sS -m 35 -X POST "$1/api/dev/file-lock-check" -H "Authorization: Bearer $TOK" \
    -H 'Content-Type: application/json' -d '{"path":"'"$2"'","holdMs":'"$3"'}'
}

# ── 1. 跨进程锁串行：A 持锁 1.5s，B 取同一把锁必须等 ─────────────────────────────
lockcheck "$BA" race-lock.txt 1500 > "$SCRATCH/lock-a.json" 2>&1 &
sleep 0.7   # 让 A 先拿到锁
B1=$(lockcheck "$BB" race-lock.txt 0)
wait
eq "A 持锁成功" "true" "$(jq -r .data.acquired "$SCRATCH/lock-a.json" 2>/dev/null)"
BW1=$(echo "$B1" | jq -r .data.waitedMs 2>/dev/null)
if [ "${BW1:-0}" -ge 500 ] 2>/dev/null; then
  ok "B 等了 A 持锁的窗口（waitedMs=$BW1 ≥ 500）——跨进程串行"
else no "B 没等 A（waitedMs=${BW1:-?}）——锁没拦住跨进程并发"; fi
B2=$(lockcheck "$BB" race-lock.txt 0)
BW2=$(echo "$B2" | jq -r .data.waitedMs 2>/dev/null)
if [ "${BW2:-99999}" -lt 500 ] 2>/dev/null; then
  ok "A 释放后 B 立刻拿到（waitedMs=$BW2 < 500）"
else no "A 释放后 B 还在等（waitedMs=${BW2:-?}）——锁没释放"; fi

# ── 2. 写前版本拒绝：模型读到 v1 → 盘上被人改成 v2 → write_file 被拒、文件保持 v2 ──
WID=$(curl -sS -X POST "$BA/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"race-ws"}' | jq -r .data.workspace.id)
if [ -z "$WID" ] || [ "$WID" = "null" ]; then no "建项目失败"; ml_summary; fi
PROJ="$WS/tenants/$TEN/workspaces/$WID"
# 项目目录是首次使用时才在盘上建的；夹具先建目录再落 v1——否则 read_file 落空，版本拒绝不会触发。
mkdir -p "$PROJ"
printf 'SCRIPT-VERSION-1\n' > "$PROJ/race.txt"
B="$BA"
ml_run '请严格按顺序做三件事，做完只回复 ok：
1. 用 read_file 读 race.txt；
2. 用 bash 执行命令：sleep 8
3. 用 write_file 把 race.txt 整体覆盖为：MODEL-VERSION
如果第 3 步被拒绝，不要重试、不要换别的工具，直接回复 ok。' "$WID"
# 轮询到模型的 bash 调用开始（read 已完成、基准已记），趁 sleep 8 的窗口在盘上改成 v2。
SEEN_BASH=0
for _ in $(seq 100); do
  ml_frames
  n=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="tool_call" and .name=="bash")] | length' 2>/dev/null)
  if [ "${n:-0}" -ge 1 ] 2>/dev/null; then SEEN_BASH=1; break; fi
  sleep 0.3
done
if [ "$SEEN_BASH" = 1 ]; then
  sleep 1.5
  printf 'SCRIPT-VERSION-2\n' > "$PROJ/race.txt"
  ok "趁 bash sleep 的窗口把 race.txt 改成了 v2（模拟别人落盘）"
else no "没等到模型的 bash 调用（模型没按顺序做？）"; fi
ml_wait_terminal 120 || no "run 没到 terminal"
ml_frames
if printf '%s' "$FRAMES" | grep -q '它跟你读到的不一样了'; then
  ok "write_file 被版本拒绝（帧里有拒绝文案）"
else no "没看到版本拒绝文案——工具帧：$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="tool_result")] | map(.output)' 2>/dev/null | head -c 400)"; fi
eq "文件保持别人的 v2（模型没覆盖成功）" "SCRIPT-VERSION-2" "$(cat "$PROJ/race.txt" 2>/dev/null | head -1 | tr -d '[:space:]')"

# ── 3. 原子替换可见性：write_file 落盘期间盯盘，只许看到完整旧版或完整新版 ────────
ATOM="$PROJ/race-atomic.txt"
python3 -c "
import sys
with open(sys.argv[1], 'w') as f:
    for i in range(20000):
        f.write('OLD-%05d ' % i + 'x' * 40 + '\n')
    f.write('END-OF-OLD')
" "$ATOM"
OLD_SZ=$(wc -c < "$ATOM" | tr -d ' ')
POLL_OUT="$SCRATCH/poll.states"; : > "$POLL_OUT"
POLL_CNT="$SCRATCH/poll.count"
(
  n=0
  while [ ! -f "$SCRATCH/stop-poll" ]; do
    n=$((n+1))
    if [ ! -f "$ATOM" ]; then st="missing"
    else
      tl=$(tail -c 16 "$ATOM" 2>/dev/null | tr -d '[:space:]')
      case "$tl" in
        *END-OF-OLD)  st="old-complete" ;;
        *END-OF-RACE) st="new-complete" ;;
        *)            st="partial|size=$(wc -c < "$ATOM" | tr -d ' ')|tail=$tl" ;;
      esac
    fi
    grep -qxF "$st" "$POLL_OUT" 2>/dev/null || printf '%s\n' "$st" >> "$POLL_OUT"
    sleep 0.02
  done
  echo "$n" > "$POLL_CNT"
) &
POLLER=$!
B="$BB"
ml_run '请按顺序做两件事，做完只回复 ok：
1. 用 read_file 读 race-atomic.txt（读开头一页就行，不用读完）；
2. 用 write_file 把 race-atomic.txt 整体覆盖为下面两行内容：
RACE-NEW-BEGIN
END-OF-RACE' "$WID"
ml_wait_terminal 120 || no "原子读 run 没到 terminal"
sleep 1
touch "$SCRATCH/stop-poll"; wait "$POLLER" 2>/dev/null
POLL_N=$(cat "$POLL_CNT" 2>/dev/null || echo 0)
BAD=$(grep -cv -e '^old-complete$' -e '^new-complete$' "$POLL_OUT" 2>/dev/null)
BAD="${BAD:-0}"
eq "盯盘 ${POLL_N} 次，观测状态只有完整旧版/完整新版（partial/missing ${BAD} 种）" "0" "$BAD"
if [ -s "$POLL_OUT" ]; then
  echo "  观测到的状态：$(tr '\n' ' ' < "$POLL_OUT")"
fi
if grep -q '^END-OF-RACE$' "$ATOM" && grep -q '^RACE-NEW-BEGIN$' "$ATOM" && [ "$(wc -l < "$ATOM" | tr -d ' ')" -le 3 ]; then
  ok "最终文件就是新版两行"
else no "最终文件不对：$(head -c 200 "$ATOM")"; fi
ml_frames
if printf '%s' "$FRAMES" | grep -q '已写入 race-atomic.txt'; then
  ok "write_file 走了工具的正常回执（覆盖语义）"
else no "没看到 write_file 回执"; fi

# ── 4. 崩溃释放：A 持锁时 kill -9，B 立刻能拿到 ──────────────────────────────────
lockcheck "$BA" race-crash.txt 30000 > "$SCRATCH/lock-crash.json" 2>&1 &
sleep 1   # A 已持锁进入 hold 睡眠
kill -9 "$PID_A" 2>/dev/null
sleep 0.5
if kill -0 "$PID_A" 2>/dev/null; then no "引擎 A 没被杀掉"; else ok "持锁中的引擎 A 已 kill -9"; fi
PID_A=""   # 陷阱里别再对它收尸
B3=$(lockcheck "$BB" race-crash.txt 0)
BW3=$(echo "$B3" | jq -r .data.waitedMs 2>/dev/null)
ACQ3=$(echo "$B3" | jq -r .data.acquired 2>/dev/null)
if [ "$ACQ3" = "true" ] && [ "${BW3:-99999}" -lt 5000 ] 2>/dev/null; then
  ok "崩溃后 B 立刻拿到锁（waitedMs=$BW3 < 5000）——flock 随进程死亡释放"
else no "崩溃后 B 拿锁不对（acquired=$ACQ3 waitedMs=${BW3:-?}）——锁像泄漏了"; fi

ml_summary
