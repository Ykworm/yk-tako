#!/usr/bin/env bash
# 消费者崩溃活体（doc/65 §13.3-2）：kill -9 正在处理记忆任务的引擎 → 重启 → 补投重做，
# 最终**不重复写**且顺序规则（A→B→A 不被去重吞掉）成立。
#
# 覆盖的故障窗（以实际观测为准，逐条报告）：
#   - 抽取中途被杀（没写库）→ 补投后重做，行只落一次；
#   - 写库后、确认前被杀（写库后未 ack）→ 行已落，补投重做撞上幂等，不重复；
#   - 消息未确认 → 重启后 broker 重投 / 对账器在租约过期后补投，两条路都通向「恰好一次」。
#   （**直接手动重发同一条消息**这条不单独造：脚本没有 broker 生产端工具；
#     上面两条路走的正是同一条 at-least-once + 幂等链路。）
#
# 靶子：**预起的隔离引擎**（pipeline + broker + 真模型），起法同 memory_pipeline_live.sh 头注。
# 脚本只 kill 调用方用 TAKO_ENGINE_PIDS 明确指定的 PID；重启用 TAKO_ENGINE_RESTART（一行 shell，
# 引擎命令后台化，**必须带与被杀实例相同的 -pg/-redis/-workspace/TAKO_JWT_SECRET/broker 环境**）。
# 重启出来的新实例归操作员管，脚本不杀它没启动的进程。
#
# 参数：TAKO_BASE / TAKO_ENV / TAKO_PG_DSN（必填）/ TAKO_PSQL /
#       TAKO_ENGINE_PIDS（必填，空格分隔）/ TAKO_ENGINE_RESTART（必填）/
#       TAKO_REDELIVER_WAIT（等补投的秒数上限，默认 300：租约 2 分钟 + 对账 30 秒 + 处理时间）。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"
ml_broker_check
ml_need_dsn

PIDS="${TAKO_ENGINE_PIDS:-}"
RESTART="${TAKO_ENGINE_RESTART:-}"
REDELIVER_WAIT="${TAKO_REDELIVER_WAIT:-300}"
for v in PIDS RESTART; do
  eval "val=\$$v"
  [ -n "$val" ] || { echo "必填参数缺了：TAKO_${v^^}（见本文件头）" >&2; exit 2; }
done

echo "引擎 $B · broker ${TAKO_REDPANDA_BROKERS:-?} · 杀进程 [$PIDS]"
curl -sS -m 3 -o /dev/null "$B/api/auth/login" || { echo "引擎不可达：$B" >&2; exit 2; }
ml_signup crash

# ── 1. 抓「正在处理 postrun 任务」的窗口，kill -9 ────────────────────────────────
CAUGHT=0
for attempt in 1 2 3; do
  FACT="FACT$(date +%s)$RANDOM"
  # pipeline 工具的方言是 targetLayer=recall|resident；抽取行（inrun/postrun）都落 recall 层。
  ml_run "请用 memory 工具把这条记下来：测试事实 ${FACT}：用户在这个项目里喜欢用分号结尾。targetLayer 用 recall。做完只回复 ok。"
  ml_wait_terminal 120 || { echo "run 没到 terminal（第 $attempt 次）" >&2; exit 2; }
  # postrun 在 EndRun 登记（跑完之后），消费者领走后 status='running'——0.2s 轮询抓这个窗口。
  for _ in $(seq 150); do
    if [ "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE run_id='$RID' AND kind='postrun' AND status='running'")" = "1" ]; then
      CAUGHT=1; break
    fi
    sleep 0.2
  done
  [ "$CAUGHT" = 1 ] && break
  echo "  第 $attempt 次没抓到 running 窗口（处理太快就完成了），换一场重试"
done
[ "$CAUGHT" = 1 ] || { no "三次都没抓到 postrun running 窗口（消费太快？把轮询调密）"; ml_summary; }
JOB_ID=$(ml_sql "SELECT id FROM memory_jobs WHERE run_id='$RID' AND kind='postrun'")
EP1=$(ml_sql "SELECT claim_epoch FROM memory_jobs WHERE id='$JOB_ID'")
AT1=$(ml_sql "SELECT attempts FROM memory_jobs WHERE id='$JOB_ID'")
echo "  抓到 job $JOB_ID（epoch=$EP1 attempts=$AT1）→ kill -9"
kill -9 $PIDS 2>/dev/null
sleep 1
for p in $PIDS; do
  if kill -0 "$p" 2>/dev/null; then no "进程 $p 没死"; else ok "进程 $p 已 kill -9"; fi
done
ROW_AT_KILL=$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%${FACT}%'")

# ── 2. 重启 → 等补投把任务做完 ──────────────────────────────────────────────────
# 用子 bash 跑重启串，不用 eval：RESTART 以「&」结尾时，eval 会把 >/dev/null 这条
# 重定向漏到脚本自己的 stdout 上（全部输出丢失、管道尾永不 EOF、脚本挂在引擎子进程上）。
bash -c "$RESTART" >/dev/null 2>&1 </dev/null
for _ in $(seq 60); do
  curl -sS -m 2 -o /dev/null "$B/api/auth/login" 2>/dev/null && break
  sleep 1
done
curl -sS -m 3 -o /dev/null "$B/api/auth/login" || { no "重启后引擎没起来"; ml_summary; }
ok "引擎重启完成"
DONE=0
for _ in $(seq "$REDELIVER_WAIT"); do
  if [ "$(ml_sql "SELECT status FROM memory_jobs WHERE id='$JOB_ID'")" = "done" ]; then DONE=1; break; fi
  sleep 1
done
eq "崩溃的任务被补投做完（≤${REDELIVER_WAIT}s）" "done" "$(ml_sql "SELECT status FROM memory_jobs WHERE id='$JOB_ID'")"

# ── 3. 恰好一次：行只落一条；补投证据（epoch/attempts 涨，或杀前已写库） ─────────────
eq "含 ${FACT} 的 active 行恰好 1 条（不重复写）" "1" "$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%${FACT}%'")"
EP2=$(ml_sql "SELECT claim_epoch FROM memory_jobs WHERE id='$JOB_ID'")
AT2=$(ml_sql "SELECT attempts FROM memory_jobs WHERE id='$JOB_ID'")
if [ "${EP2:-0}" -ge 2 ] 2>/dev/null || [ "${AT2:-0}" -ge 2 ] 2>/dev/null; then
  ok "补投证据：epoch $EP1→$EP2，attempts $AT1→$AT2（租约过期补投/未 ack 重投）"
elif [ "${ROW_AT_KILL:-0}" -ge 1 ] 2>/dev/null; then
  ok "杀到时行已落库（写库后未 ack 的相邻态，epoch=$EP2 不再涨）——幂等挡住重复"
else
  no "没有补投证据也没有杀前落库（epoch=$EP2 attempts=$AT2 杀前行数=$ROW_AT_KILL）"
fi
eq "全程没有 failed 记忆任务" "0" "$(ml_sql "SELECT count(*) FROM memory_jobs WHERE tenant_id='$TEN' AND status='failed'")"

# ── 4. 顺序规则：A→B→A 改口，最终态是茶、历史链完整、不被内容去重吞掉 ───────────────
# 行落 recall 层（inrun targetLayer=recall）——末尾的 recall 端点断言才查得到。
wait_all(){
  ml_wait_terminal 120 || { no "run 没到 terminal"; ml_summary; }
  ml_wait_settled || no "会话没落定"
  ml_wait_jobs 90 || no "记忆任务 90s 没收口"
}
ml_run '请用 memory 工具把这条记下来：我喜欢喝茶。targetLayer 用 recall。做完只回复 ok。'
wait_all
T1=$(ml_sql "SELECT id FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%茶%' ORDER BY created_at, id LIMIT 1")
ml_run '改口：我不喝茶了，现在喜欢喝咖啡。请用 memory 工具更新这条记忆（targetLayer 用 recall）。做完只回复 ok。' '' "$SID"
wait_all
ml_run '又改回来了：咖啡不喝了，现在喜欢喝茶。请用 memory 工具更新这条记忆（targetLayer 用 recall）。做完只回复 ok。' '' "$SID"
wait_all
T3=$(ml_sql "SELECT id FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%茶%' ORDER BY created_at, id LIMIT 1")
if [ -n "$T1" ] && [ -n "$T3" ] && [ "$T1" != "$T3" ]; then
  ok "A→B→A 产生了新行（$T1 → $T3），没被内容去重吞掉"
else no "A→B→A 被吞（T1=$T1 T3=$T3）"; fi
eq "active 的茶行恰好 1 条" "1" "$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND state='active' AND content LIKE '%茶%'")"
eq "改回的茶行 active" "active" "$(ml_sql "SELECT state FROM memory_items WHERE id='$T3'")"
eq "第一代茶行留痕 superseded" "superseded" "$(ml_sql "SELECT state FROM memory_items WHERE id='$T1'")"
if [ "$(ml_sql "SELECT count(*) FROM memory_items WHERE tenant_id='$TEN' AND layer='recall' AND scope='user' AND content LIKE '%咖啡%' AND state='superseded'")" -ge 1 ] 2>/dev/null; then
  ok "咖啡代也留痕 superseded"
else no "咖啡行没留痕（不是 superseded）"; fi
H=$(curl -sS "$B/api/memory/recall?q=喜欢喝茶" -H "Authorization: Bearer $TOK" | jq -r '.data.memoryHits')
if [ "${H:-0}" -ge 1 ] 2>/dev/null; then ok "最终态可召回：茶（memoryHits=$H）"; else no "茶没召回（${H:-?}）"; fi

ml_summary
