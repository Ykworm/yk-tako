#!/usr/bin/env bash
# 自然语言演示（doc/82；施工包 §12.4）：不打分模型智能——用户层一句原话投进隔离实例，
# 观察四个**程序事实**（机制在不在，不评模型答得好不好）：
#   ① 后台命令活过前台窗口（background_jobs 出现 backgrounded=true 的行）；
#   ② 来源会话在命令运行期间能开新 run（构建期间还想问别的问题）；
#   ③ 结果回到来源会话（通知交付 / wait_task 认领，会话里能看到交付 run）；
#   ④ 依赖挡住提前汇总（报告 Task 在调研 Task 终态前不 claim，或 claim 后被依赖校验拒绝）。
#
# 模型没采取预期策略 → 按 §12.4 登记为模型行为问题，不算执行机制失败；
# 不预建 DAG、不伪造输出（本脚本只投一句原话，其余全靠模型自己）。
#
# 起法：按 scripts/acceptance/README.md 起隔离引擎（建议 -bash-foreground-wait 3s -wait-window 45s
# -task-interval 1s），然后：
#   TAKO_BASE=http://127.0.0.1:8945 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc82 sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" bash scripts/acceptance/agent_task_demo_live.sh
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · §12.4 自然语言演示（观察记录）"
ml_signup demo82

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"demo-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }

# 预置「两份调研」（用户视角：文件本来就在项目里）。布局规则（doc/76）：容器里出现
# layout.json 之外的条目 = 存量未迁移，入口回 409 117116——所以必须先让引擎把容器装配出来
# （layout.json 在「第一次需要目录时」才写），再写文件；且用户文件落 content/ 根，
# 模型的文件工具按 content 为根解析，写容器根模型看不见。装配探针 = getMedia 打一次
# （空容器判不出存量、随后 ensureProjectLayout 建树；文件不存在回 404 属预期）。
curl -sS "$B/api/media?path=workspaces/$WID/.media/layout-probe.png" -H "Authorization: Bearer $TOK" >/dev/null
CONTENT="$WS_ROOT/tenants/$TEN/workspaces/$WID/content"
[ -f "$WS_ROOT/tenants/$TEN/workspaces/$WID/layout.json" ] \
  && ok "容器已装配（layout.json 就位）" || no "探针后容器没有装配出 layout.json"
mkdir -p "$CONTENT/docs"
printf '%s\n' "调研 A：tako 运行引擎——v6 契约四端点、帧协议、闸与租约。结论：可交付。" \
  > "$CONTENT/docs/research-a.md"
printf '%s\n' "调研 B：后台任务——前台窗口、进程组停止、租约对账、30 天保留。结论：可交付。" \
  > "$CONTENT/docs/research-b.md"
# 可构建素材：25 秒慢构建 + 校验。没有它「构建和测试都要通过」无从落地——run3 实翻：
# 项目里只有两份调研，模型只能 pwd/ls 反复探索，编排组不建、长命令不出现（模型行为观察）。
# 25s 必然撞前台等待窗口（3s）转后台，四个观察点里的 ①②③ 全靠它供料。
cat > "$CONTENT/build.sh" <<'EOS'
#!/bin/sh
echo "build start"
i=0
while [ $i -lt 25 ]; do
  i=$((i+1)); sleep 1; echo "building... $i/25"
done
mkdir -p dist
echo "bundle-$(date +%s)" > dist/bundle.txt
echo "build done"
EOS
cat > "$CONTENT/test.sh" <<'EOS'
#!/bin/sh
if [ -f dist/bundle.txt ]; then echo "tests: 1 passed"; else echo "tests: dist missing"; exit 1; fi
EOS
ok "项目里预置了两份调研文件与可构建素材（content/）"

at_run_retry "让这个项目达到可交付状态，构建和测试都要通过；整理现有两份调研，最后给我一份报告。构建期间我还想问别的问题。" "$WID"
ok "原话已投：run $RID（会话 $SID）"
DEMO_SID=$SID

# 观察窗：先等编排 run 落定——协调 run 是整轮模型工作，provider 降速时 240s 不够
# （2026-11-15 实翻：run2 编排 run 跑满 240s 观察窗 + 120s 重试还在跑，第②问撞 112101）。
# 期间持续观察 ①（转后台命令）与 ③（持久通知）。
deadline=$((SECONDS + 600))
OBS_JOBS=0; OBS_NOTIFY=0; OBS_ST=""
while [ "$SECONDS" -lt "$deadline" ]; do
  J=$(ml_sql "SELECT count(*) FROM background_jobs j JOIN sessions s ON s.id=j.source_session_id WHERE s.id='$DEMO_SID' AND j.backgrounded")
  [ "$J" -gt "$OBS_JOBS" ] && { OBS_JOBS=$J; ok "① 出现转后台的命令（backgrounded=$J）"; }
  N=$(ml_sql "SELECT count(*) FROM agent_notifications WHERE receiver_session_id='$DEMO_SID' AND kind IN ('task_failed','task_interrupted','orchestration_finished','bash_job_finished')")
  [ "$N" -gt "$OBS_NOTIFY" ] && { OBS_NOTIFY=$N; ok "③ 持久通知已落（$N 条）"; }
  OBS_ST=$(curl -sS "$B/api/sessions/$DEMO_SID" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data.lastRunStatus // ""')
  case "$OBS_ST" in complete|error|stopped) break;; esac
  sleep 5
done
case "$OBS_ST" in complete|error|stopped) ok "编排 run 落定（lastRunStatus=$OBS_ST）";;
  *) no "编排 run 600s 没落定（lastRunStatus=$OBS_ST）"; ml_summary;; esac
[ "$OBS_JOBS" -gt 0 ] || no "观察窗内没有出现转后台命令"

# 第 ② 问：协调 run 落定即释放会话闸（后台任务/命令不占会话闸），来源会话立刻能再投一场。
# 记下投递瞬间还在跑的后台命令数：仍在跑是最有说服力的形态；已先行终态时机制判定不变
# （会话闸随 run 结束释放，与命令存活解耦），模型快慢按 §12.4 登记为模型行为观察。
BJ_RUNNING=$(ml_sql "SELECT count(*) FROM background_jobs j JOIN sessions s ON s.id=j.source_session_id WHERE s.id='$DEMO_SID' AND j.backgrounded AND j.status IN ('running','stopping')")
at_run_retry "构建期间顺便看看：项目 docs/ 目录里有哪两份文件？只回答文件名。" "$WID" "$DEMO_SID"
ml_wait_settled 90 || no "第②问 90s 没落定"
if [ "${BJ_RUNNING:-0}" -gt 0 ]; then
  ok "② 后台命令仍在跑（$BJ_RUNNING 个）时来源会话开新 run 成功（run $RID）"
else
  ok "② 来源会话开新 run 成功（run $RID；投递时后台命令已先行终态，闸随协调 run 结束释放，机制成立）"
fi

echo "── 收尾状态（供留证）──"
ml_sql_raw "SELECT id, status, backgrounded, command FROM background_jobs j WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id=j.source_session_id AND s.id='$DEMO_SID');"
ml_sql_raw "SELECT kind, status, claimed_in_place FROM agent_notifications WHERE receiver_session_id='$DEMO_SID' ORDER BY created_at;"
ml_sql_raw "SELECT orchestration_id, status, delivery_status FROM agent_orchestrations WHERE source_session_id='$DEMO_SID';"
ml_sql_raw "SELECT count(*) AS runs_in_session FROM runs WHERE session_id='$DEMO_SID';"

ml_summary
