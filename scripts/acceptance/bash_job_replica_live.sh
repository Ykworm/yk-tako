#!/usr/bin/env bash
# 后台命令副本与崩溃活体（doc/82；施工包 §12.2 场景 4/5）：两台隔离引擎共库共 workspace——
#   场景 4：A 起的长命令，B 能读能停；owner（A 的 watcher）杀进程组收 stopped；通知恰好一条。
#   场景 5：kill -9 owner → 副本对账收 interrupted（epoch+1）、输出仍在、不自动重跑；
#           通知在崩溃后仍恰好一条，重启后被交付恰好一次。
#
# 自起停脚本（参照 memory_file_race.sh）：自己编译二进制、起停两台引擎。参数：
#   TAKO_PG_DSN   必填，空库（脚本自装 pg_search）
#   TAKO_PSQL     默认 psql
#   TAKO_ENV      模型密钥 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_GO       默认 go（不在 PATH 则退 $HOME/sdk/go1.27.0/bin/go）
#   TAKO_ENGINE_PORT_A/B  默认 8958 / 8959
#   TAKO_REDIS_URL 默认 redis://127.0.0.1:6390/9
# 纪律：kill 只打本脚本自己起的引擎；不调 /api/dev/reset。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

PORT_A="${TAKO_ENGINE_PORT_A:-8958}"
PORT_B="${TAKO_ENGINE_PORT_B:-8959}"
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

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-bj-replica.XXXXXX")
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
export TAKO_MEMORY_MODE=markdown

# 引擎旗标：短前台窗（3s 转后台）+ 短租约（10s TTL / 3s 续 / 3s 对账）——崩溃场景不必等 35s。
ENGINE_FLAGS="-bash-foreground-wait 3s -wait-window 8s -task-interval 1s -job-lease-ttl 10s -job-lease-renew 3s -job-reconcile-interval 3s"

start_engine(){ # $1=port $2=log → PID
  (
    cd "$ML_REPO_ROOT/yk-tako-eino" || exit 1
    exec "$BIN" -engine "127.0.0.1:$1" -pg "$DSN" -redis "$REDIS_URL" -workspace "$WS" $ENGINE_FLAGS
  ) >>"$2" 2>&1 &
  local pid=$!
  for _ in $(seq 60); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$1/api/auth/login" 2>/dev/null; then echo "$pid"; return 0; fi
    if ! kill -0 "$pid" 2>/dev/null; then echo "引擎($1)启动即退，日志：$2" >&2; tail -20 "$2" >&2; exit 2; fi
    sleep 0.5
  done
  echo "引擎($1) 30s 没起来，日志：$2" >&2; exit 2
}

echo "引擎 A 127.0.0.1:$PORT_A · 引擎 B 127.0.0.1:$PORT_B · 共库 $DSN"
"$PSQL" "$DSN" -c 'CREATE EXTENSION IF NOT EXISTS pg_search CASCADE;' >/dev/null 2>&1 \
  || { echo "pg_search 扩展装不上（库要预加载 pg_search，见 scripts/acceptance/README.md）" >&2; exit 2; }
(cd "$ML_REPO_ROOT/yk-tako-eino" && "$GO" build -o "$BIN" ./cmd/yktako) || { echo "构建失败" >&2; exit 2; }
PID_A=$(start_engine "$PORT_A" "$ELOG_A")
PID_B=$(start_engine "$PORT_B" "$ELOG_B")
BA="http://127.0.0.1:$PORT_A"
BB="http://127.0.0.1:$PORT_B"
B="$BA"   # lib 的注册/投递/拉帧全走 B；本脚本自带引擎，默认打 A，§里显式切
ml_signup bjreplica

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"bj-replica-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"
MAIN_SID=$SID

bj_toolout(){
  printf '%s' "$FRAMES" | jq -s -r --arg n "$1" \
    '[.[].data.frames[]? | select(.kind=="tool_result" and .name==$n) | .output] | last // ""' 2>/dev/null
}
bj_job(){ # $1=base $2=jobId $3=字段
  curl -sS "$1/api/jobs/$2" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r ".data.$3 // \"\""
}
bj_wait_job(){ # $1=base $2=jobId $3=目标状态 $4=超时秒
  local deadline=$((SECONDS + ${4:-45})) st
  BJ_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(bj_job "$1" "$2" status)
    BJ_LAST="$st"
    [ "$st" = "$3" ] && return 0
    sleep 1
  done
  return 1
}

# bj_wait_toolout $1=超时秒 → 轮询帧直到 bash 的 tool_result 带回命令号（存 BJ4/BJ5 之外
# 由调用方自取 OUT）。**不等 run 落定**：3s 前台窗一过回执就在帧里，与 provider 的落定
# 延迟解耦（2026-11-15 实翻：settle 可拖到 120s，等落定再断言 running 期必假红）。
bj_wait_toolout(){
  local deadline=$((SECONDS + ${1:-60})) out jid
  while [ "$SECONDS" -lt "$deadline" ]; do
    ml_frames
    out=$(bj_toolout bash)
    jid=$(printf '%s' "$out" | jq -r '.jobId // ""' 2>/dev/null)
    if [ -n "$jid" ] && [ "$jid" != "null" ]; then
      OUT="$out"
      return 0
    fi
    sleep 2
  done
  OUT=""
  return 1
}

# ══════════════════════════════════════════════════════════════════════════
# 场景 4：双副本——A 起的命令，B 读得到、停得掉；owner 杀组收 stopped；通知恰好一条
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 4：双副本读停同一 job ═══"
STAMP="$(date +%s)-$$"
# 投递撞会话忙（前一场的交付 run 占闸 112101）由 at_run_retry 自动重试。
at_run_retry "调用 bash 工具，command 参数原样传：sleep 90" "$WID" "$MAIN_SID"
# 不等落定，轮询帧拿命令号；B 的读/停断言全在命令还活着的窗口内（sleep 90 >> 取回执+断言）。
if bj_wait_toolout 60; then
  BJ4=$(printf '%s' "$OUT" | jq -r '.jobId // ""')
  ok "A 起的长命令拿到命令号 $BJ4"
else
  no "60s 没等到转后台回执"; ml_summary
fi

eq "B 读同一 job 状态一致" "running" "$(bj_job "$BB" "$BJ4" status)"
eq "B 读同一 job 的来源会话一致" "$MAIN_SID" "$(bj_job "$BB" "$BJ4" sourceSessionId)"

ST=$(curl -sS -X POST "$BB/api/jobs/$BJ4/stop" -H "Authorization: Bearer $TOK")
eq "B 停止 → 回 stopping" "stopping" "$(printf '%s' "$ST" | jq -r '.data.status // ""')"
eq "B 停止 → stopRequested=true" "true" "$(printf '%s' "$ST" | jq -r '.data.stopRequested // ""')"
if bj_wait_job "$BA" "$BJ4" "stopped" 20; then
  ok "owner（A watcher）杀组收 stopped"
else no "20s 没到 stopped（$BJ_LAST）"; ml_summary; fi
ml_wait_settled 90 || no "场景 4 来源 run 90s 没落定"
sleep 3   # 让可能的交付窗口走完
eq "这条命令的 bash_job_finished 通知恰好一条" "1" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id='$BJ4' AND kind='bash_job_finished'")"

# ══════════════════════════════════════════════════════════════════════════
# 场景 5：kill -9 owner——副本对账收 interrupted、输出仍在、不自动重跑、通知不重复交付
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 5：kill -9 owner ═══"
at_run_retry "调用 bash 工具，command 参数原样传：echo crashed-$STAMP-started; sleep 60" "$WID" "$MAIN_SID"
# 同场景 4：不等落定，轮询帧拿命令号，kill -9 落在命令还活着的窗口内。
if bj_wait_toolout 60; then
  BJ5=$(printf '%s' "$OUT" | jq -r '.jobId // ""')
  ok "崩溃场景的命令号 $BJ5"
else
  no "60s 没等到转后台回执"; ml_summary
fi
# 等启动确认（process_group_id 落库），再杀
deadline=$((SECONDS + 15))
while [ "$SECONDS" -lt "$deadline" ]; do
  PGID=$(ml_sql "SELECT process_group_id FROM background_jobs WHERE id='$BJ5'")
  [ -n "$PGID" ] && break
  sleep 1
done
[ -n "$PGID" ] && ok "job 已启动确认（pgid=$PGID）" || { no "15s 没等到启动确认"; ml_summary; }
kill -9 "$PID_A" 2>/dev/null
wait "$PID_A" 2>/dev/null
ok "kill -9 引擎 A（owner watcher 一起没了）"
# 输出文件还在（落盘不随进程死）：
PHYS=$(find "$WS" -path "*/jobs/$BJ5/stdout.out" 2>/dev/null | head -1)
[ -s "$PHYS" ] && ok "输出文件仍在磁盘：$PHYS" || no "输出文件丢了：$PHYS"
CRASH_OUT=$(cat "$PHYS" 2>/dev/null)
case "$CRASH_OUT" in *"crashed-$STAMP-started"*) ok "死前输出可读";; *) no "死前输出丢了：$(head -c 120 "$PHYS" 2>/dev/null)";; esac

# 副本对账：租约过期 → interrupted + epoch+1（B 的对账循环周期 3s，租约 10s）
if bj_wait_job "$BB" "$BJ5" "interrupted" 60; then
  ok "副本对账收 interrupted"
else no "60s 没到 interrupted（$BJ_LAST）"; ml_summary; fi
EQ=$(ml_sql "SELECT epoch FROM background_jobs WHERE id='$BJ5'")
eq "epoch 已 +1（旧代写不回来）" "2" "$EQ"

# 不自动重跑：命令只有一行登记、没有新 run 行、残余进程已死
eq "没有新的登记行（不自动重放）" "1" "$(ml_sql "SELECT count(*) FROM background_jobs WHERE command LIKE '%sleep 60%' AND source_session_id='$MAIN_SID'")"
# §7 边界（bash_job_sweeper.go）：对账只杀 ExecutorID 指向本副本的进程组——B 不是 owner，
# 不跨副本杀；owner watcher 已随 kill -9 消失，残余（sleep 60）按命令自然结束退出。
deadline=$((SECONDS + 75)); PGONE=""
while [ "$SECONDS" -lt "$deadline" ]; do
  ps -p "$PGID" >/dev/null 2>&1 || { PGONE=y; break; }
  sleep 3
done
[ -n "$PGONE" ] && ok "残余进程组已消亡（无人杀，命令自然结束）" || no "命令结束后进程组 75s 还在"

eq "崩溃也只写一条通知" "1" "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id='$BJ5' AND kind='bash_job_finished'")"

# 重启 A（与被杀实例同一份 pg/redis/workspace/密钥），等通知被交付恰好一次
PID_A=$(start_engine "$PORT_A" "$ELOG_A")
ok "重启引擎 A（PID $PID_A）"
deadline=$((SECONDS + 120)); NF5=""
while [ "$SECONDS" -lt "$deadline" ]; do
  NF5=$(ml_sql "SELECT status FROM agent_notifications WHERE background_job_id='$BJ5' AND kind='bash_job_finished' ORDER BY created_at DESC LIMIT 1")
  [ "$NF5" = "consumed" ] || [ "$NF5" = "delivered" ] && break
  sleep 2
done
case "$NF5" in consumed|delivered) ok "崩溃后的通知被消费（status=$NF5）";; *) no "通知没被消费（status=$NF5）";; esac
eq "交付 attempt 恰好一条" "1" "$(ml_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.background_job_id='$BJ5'")"
sleep 12   # 再等一轮：空闲会话不该再冒出第二场交付
# 交付路径两种（doc/82 §4-12）：来源 run 趁活就地认领（不建交付 run、不写 attempt）；否则走
# 交付 run + attempt 行。所以行数不写死：探针 1 + 来源 2 + attempt 条数（每条通知恰好一场交付）。
DELIV=$(ml_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.background_job_id IN ('$BJ4','$BJ5') AND n.kind='bash_job_finished'")
eq "两条通知都已消费" "0" \
  "$(ml_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id IN ('$BJ4','$BJ5') AND kind='bash_job_finished' AND status<>'consumed'")"
eq "不重复交付：会话 run 行数稳定（探针+两来源+交付路径）" "$((3 + ${DELIV:-0}))" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id='$MAIN_SID'")"

ml_summary
