#!/usr/bin/env bash
# 后台命令活体（doc/82；施工包 §12.2 场景 1/2/3/13/14/15）：真模型跑 bash → 前台超时转后台 →
# 输出从首字节落盘 → 停止三态 → 跨租户/viewer 鉴权 → 无项目会话 → 短命前台不写通知 → 删除保护。
#
# 起法：按 scripts/acceptance/README.md 起一个开着调度循环的隔离引擎
# （建议 -bash-foreground-wait 3s -wait-window 8s，让「转后台」在脚本时限内必然发生），然后：
#   TAKO_BASE=http://127.0.0.1:8945 TAKO_ENV=yk-tako-eino/.env \
#   TAKO_PG_DSN="host=127.0.0.1 port=5433 dbname=tako_acc82 sslmode=disable" \
#   TAKO_PSQL="$PGBIN/psql" TAKO_WS_ROOT=<仓库>/tmp/workspace-acc82 \
#     bash scripts/acceptance/bash_job_live.sh
#
# 说明：场景 4「旧 epoch 不覆盖」与场景 11 的清理幂等不在这里——前者是单代 owner 生命周期内
# 打不出来的并发冲突（epoch 条件由 job 状态机单测覆盖），后者是 30 天保留期的循环行为
# （清理幂等由 sweepJobs 单测覆盖）；这里不假装跑过。
# 场景 1/2 的「全文路径分页」不在断言面：逻辑路径只指向 job 固定目录（落点在这里验），
# 按 offset/limit 分页读大文件是 doc/58 read_file 的既有规则（doc/58 活体覆盖），不重测。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

echo "引擎 $B · 打后台命令的登记/落盘/停止/鉴权/删除保护链"
ml_signup bjlive

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"bj-live-ws"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
at_probe_run "$WID"
ok "建好项目 $WID，来源会话 $SID 已挂上它"
MAIN_SID=$SID

# bj_run $1=给模型的指令 → 投一场 run 并等落定（frames 存 FRAMES）。
# 投递撞会话忙（交付 run 占闸 112101）由 at_run_retry 自动重试。
# 未落定不直接判红：模型可能用 bash_output(wait=true) 在 run 内等后台命令（doc/82 执行方式提示词
# 鼓励的合法分支），此时 run 会活到命令结束（最长 240s）——有活命令就记观察，无活命令才算失败。
bj_run(){
  at_run_retry "$1" "$WID" "$SID"
  if ! ml_wait_settled 120; then
    live_jobs=$(bj_sql "SELECT count(*) FROM background_jobs WHERE source_session_id='$SID' AND status IN ('running','stopping')")
    case "${live_jobs:-0}" in
      0) no "run 120s 没落定且无活命令" ;;
      *) ok "run 未落定：模型在 run 内等后台命令（$live_jobs 条仍 running，wait=true 分支）" ;;
    esac
  fi
  ml_frames
}

# bj_toolout $1=工具名 → 取该工具最后一条 tool_result 的 output 原文。
bj_toolout(){
  printf '%s' "$FRAMES" | jq -s -r --arg n "$1" \
    '[.[].data.frames[]? | select(.kind=="tool_result" and .name==$n) | .output] | last // ""' 2>/dev/null
}

# bj_job $1=jobId $2=字段 → GET /api/jobs/:id 的 data 字段。
bj_job(){
  curl -sS "$B/api/jobs/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r ".data.$2 // \"\""
}

# bj_wait_job $1=jobId $2=目标状态 $3=超时秒（默认 45）→ 到了回 0，最后状态存 BJ_LAST。
bj_wait_job(){
  local deadline=$((SECONDS + ${3:-45})) st
  BJ_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(bj_job "$1" status)
    BJ_LAST="$st"
    [ "$st" = "$2" ] && return 0
    sleep 1
  done
  return 1
}

# bj_sql 计数/取值；bj_bool 把 PG 布尔列转成 true/false 字面量。
bj_sql(){ ml_sql "$1"; }
bj_bool(){ ml_sql "SELECT CASE WHEN ($1) THEN 'true' ELSE 'false' END"; }

has2(){ case "$2" in *"$1"*) return 0;; *) return 1;; esac; }

# ══════════════════════════════════════════════════════════════════════════
# 场景 1 + 2：前台超时转后台 → 输出从首字节落盘 → 终态与通知恰好一次
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 1/2：转后台 + 输出落盘 ═══"
STAMP="$(date +%s)-$$"
# 命令时长必须跑赢最坏的 run 落定延迟（provider 降速时 settle 可到 120s）：180 tick 保证
# 下面所有「running 期」断言执行时命令还在跑，与 run 何时落定解耦（2026-11-15 实翻）。
bj_run "调用 bash 工具，command 参数原样传这条命令，不要改写、不要加引号，也不要加 runInBackground 参数（不要显式后台，让它前台等待超时后自动转后台）：
for i in \$(seq 1 180); do echo tick-\$i; sleep 1; done"
OUT=$(bj_toolout bash)
BJ1=$(printf '%s' "$OUT" | jq -r '.jobId // ""' 2>/dev/null)
if [ -n "$BJ1" ]; then
  ok "前台 3s 到点返回命令号 $BJ1"
else
  no "bash 工具回执不是后台命令号：${OUT:0:200}"
  ml_summary
fi
eq "回执 status=running" "running" "$(printf '%s' "$OUT" | jq -r '.status // ""')"
eq "回执 reason=foreground_timeout" "foreground_timeout" "$(printf '%s' "$OUT" | jq -r '.reason // ""')"

eq "job 行登记为 running" "running" "$(bj_job "$BJ1" status)"
eq "job 背景化标记已翻" "true" "$(bj_job "$BJ1" backgrounded)"
eq "非终态 exitCode 是 null" "null" "$(curl -sS "$B/api/jobs/$BJ1" -H "Authorization: Bearer $TOK" | jq -r '.data.exitCode')"
eq "来源会话/来源 run 已登记" "$SID" "$(bj_job "$BJ1" sourceSessionId)"
STDOUT_REL=$(bj_job "$BJ1" stdoutPath)
eq "逻辑路径指向 job 固定目录" "runtime/tmp/bash/jobs/$BJ1/stdout.out" "$STDOUT_REL"

# 输出从首字节落盘：running 期间读两次，尾部要变长（tick 在持续追加）。
O1=$(curl -sS "$B/api/jobs/$BJ1/output" -H "Authorization: Bearer $TOK" | jq -r '.data.stdout // ""')
sleep 2
O2=$(curl -sS "$B/api/jobs/$BJ1/output" -H "Authorization: Bearer $TOK" | jq -r '.data.stdout // ""')
[ -n "$O1" ] && ok "输出首字节起可读（${#O1} 字节）" || no "running 期间 /output stdout 是空的"
[ "${#O2}" -gt "${#O1}" ] && ok "输出在增长（${#O1} → ${#O2} 字节）" || no "输出没增长（${#O1} → ${#O2}）"

# 物理落盘：项目容器的 runtime/tmp/bash/jobs/<bj>/ 下，全文文件存在。
PHYS="$WS_ROOT/tenants/$TEN/workspaces/$WID/$STDOUT_REL"
[ -s "$PHYS" ] && ok "全文落盘文件存在且非空：$PHYS" || no "全文落盘文件不存在：$PHYS"

# 终态：命令自己跑完（180 tick ≈ 180s > 最坏 settle——先见 running/输出增长，再见终态）。
if bj_wait_job "$BJ1" "succeeded" 240; then
  ok "job 跑到 succeeded"
else no "45s 没跑到 succeeded（最后状态 $BJ_LAST）"; ml_summary; fi
eq "退出码 0" "0" "$(curl -sS "$B/api/jobs/$BJ1" -H "Authorization: Bearer $TOK" | jq -r '.data.exitCode')"
[ -n "$(bj_job "$BJ1" endedAt)" ] && [ "$(bj_job "$BJ1" endedAt)" != "null" ] && ok "endedAt 已写" || no "endedAt 没写"
# 终态瞬间读 /output 可能撞上最后一次 stdout 刷盘（2026-11-15 实翻：盘上全文尾是 tick-180，
# 终态后立刻读只拿到中途的 tick）——轮询重试到 tick-180 出现为止。判定用全文 contains，
# 不是 `| tail -1`：jq -r 会在输出后再添一个换行，stdout 本身又以 \n 结尾，tail -1 拿到的
# 永远是 jq 添的那个空行（2026-11-15 实翻，三轮假败全因这条管道 bug，与引擎无关）。
STDOUT_END=""
deadline=$((SECONDS + 15))
while [ "$SECONDS" -lt "$deadline" ]; do
  STDOUT_END=$(curl -sS "$B/api/jobs/$BJ1/output" -H "Authorization: Bearer $TOK" | jq -r '.data.stdout // ""')
  has2 "tick-180" "$STDOUT_END" && break
  sleep 2
done
has2 "tick-180" "$STDOUT_END" && ok "终态后 /output 窗里有最后的 tick（尾部 40 字节：${STDOUT_END: -40}）" || no "15s 内 /output 始终没有 tick-180（stdout 尾 40 字节：${STDOUT_END: -40}；末次信封头 300 字节：$(curl -sS "$B/api/jobs/$BJ1/output" -H "Authorization: Bearer $TOK" | head -c 300)）"

# 通知恰好一条，且最终被消费（来源会话空闲后由交付 run 收口，不重复交付）。
NOTIF_JOB="$BJ1"
deadline=$((SECONDS + 120)); NF_STATUS=""
while [ "$SECONDS" -lt "$deadline" ]; do
  NF_STATUS=$(bj_sql "SELECT status FROM agent_notifications WHERE background_job_id='$BJ1' AND kind='bash_job_finished' ORDER BY created_at DESC LIMIT 1")
  [ "$NF_STATUS" = "consumed" ] || [ "$NF_STATUS" = "delivered" ] && break
  sleep 2
done
eq "bash_job_finished 通知恰好一条" "1" "$(bj_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id='$BJ1' AND kind='bash_job_finished'")"
case "$NF_STATUS" in consumed|delivered) ok "通知被消费（status=$NF_STATUS，不重复交付）";;
  *) no "通知没被消费（status=$NF_STATUS）";; esac
# 交付去重按机制断言，不按模型分支：要么走交付 run（attempt=1 且非就地认领），要么来源 run
# 趁活就地认领（claimed_in_place=true，不建 run、不写 attempt 行——doc/82 §4-12）。两条都要求
# 通知最终被消费；「恰好一次」由上面两条计数共同保证。
ATTS=$(bj_sql "SELECT count(*) FROM agent_notification_attempts a JOIN agent_notifications n ON n.id=a.notification_id WHERE n.background_job_id='$BJ1'")
INLINE=$(bj_sql "SELECT coalesce(bool_or(claimed_in_place), false)::text FROM agent_notifications WHERE background_job_id='$BJ1' AND kind='bash_job_finished'")
if [ "$ATTS" = "1" ] && [ "$INLINE" = "false" ]; then
  ok "交付 attempt 恰好一条（交付 run 路径）"
elif [ "$ATTS" = "0" ] && [ "$INLINE" = "true" ]; then
  ok "就地认领不写 attempt（来源 run 趁活收口，不重复交付）"
else
  no "交付路径异常：attempt=$ATTS claimed_in_place=$INLINE"
fi

# ══════════════════════════════════════════════════════════════════════════
# 场景 3：停止三态 + 鉴权（跨租户 404 / viewer 读 404 停 403）
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 3：停止三态 + 鉴权 ═══"
# sleep 300：run 最坏落定 120s，命令必须在首停时还在跑（否则 200/stopping 断言假红）。
# 指令长版化（与场景 1 同款纪律）：交付 run 的「bash 未装配」拒绝会经 dialog-context 召回进
# 本场上下文，模型会先调 tool_search 探测再放弃（2026-11-15 实翻）——显式禁止探测、点名直接调。
bj_run "调用 bash 工具，command 参数原样传这条命令，不要改写、不要加引号、不要调 tool_search（bash 就在工具面里，直接调用即可）、也不要加 runInBackground 参数：
sleep 300"
OUT=$(bj_toolout bash)
BJ3=$(printf '%s' "$OUT" | jq -r '.jobId // ""' 2>/dev/null)
[ -n "$BJ3" ] && ok "长命令拿到命令号 $BJ3" || { no "sleep 300 没拿到命令号：${OUT:0:200}"; ml_summary; }
bj_wait_job "$BJ3" "running" 10 || true

AT_CODE=""; AT_BODY=""
at_http POST "/api/jobs/$BJ3/stop"
eq "首停回 200 stopping" "200" "$AT_CODE"
eq "stopRequested=true" "true" "$(printf '%s' "$AT_BODY" | jq -r '.data.stopRequested')"
eq "状态 stopping（中间态）" "stopping" "$(printf '%s' "$AT_BODY" | jq -r '.data.status')"

if bj_wait_job "$BJ3" "stopped" 20; then
  ok "owner 杀进程组后收 stopped"
else no "20s 没到 stopped（最后状态 $BJ_LAST）"; ml_summary; fi
eq "被停命令 exitCode 是 null（不伪装退出码）" "null" "$(curl -sS "$B/api/jobs/$BJ3" -H "Authorization: Bearer $TOK" | jq -r '.data.exitCode')"
PGID=$(bj_sql "SELECT process_group_id FROM background_jobs WHERE id='$BJ3'")
if [ -n "$PGID" ] && ! ps -p "$PGID" >/dev/null 2>&1; then
  ok "进程组 $PGID 已退出"
else no "进程组还在（pgid=$PGID）"; fi

at_http POST "/api/jobs/$BJ3/stop"
eq "终态重复停止 200 回当前态" "200" "$AT_CODE"
eq "重复停止 stopRequested=false" "false" "$(printf '%s' "$AT_BODY" | jq -r '.data.stopRequested')"
eq "重复停止回 stopped" "stopped" "$(printf '%s' "$AT_BODY" | jq -r '.data.status')"

at_http GET "/api/jobs/bj_notexist0000000000000000"
eq "未知 id 读 → 404 117117" "404" "$AT_CODE"
eq "未知 id 业务码" "117117" "$(printf '%s' "$AT_BODY" | jq -r '.code')"
at_http POST "/api/jobs/bj_notexist0000000000000000/stop"
eq "未知 id 停止 → 404 117117" "404" "$AT_CODE"
eq "未知 id 停止业务码" "117117" "$(printf '%s' "$AT_BODY" | jq -r '.code')"

# 跨租户：另一个租户的用户读/停 → 统一 404（不给探测通道）。
# ml_signup 覆写 TOK/TEN/ML_UID（SID 不动）：三个都要先存后还原——ML_UID 忘还原会让
# 场景 13 的成员默认项目断言拿错 user_id（2026-11-15 实翻）。viewer 邀请与场景 14+
# 的投递也全靠还原后的身份（114001，同日实翻车）。
XTOK=$TOK; XTEN=$TEN; XUID=$ML_UID
ml_signup bjcross
at_http GET "/api/jobs/$BJ3"
eq "跨租户读 → 404" "404" "$AT_CODE"
eq "跨租户读业务码" "117117" "$(printf '%s' "$AT_BODY" | jq -r '.code')"
at_http POST "/api/jobs/$BJ3/stop"
eq "跨租户停止 → 404" "404" "$AT_CODE"
TOK=$XTOK; TEN=$XTEN; ML_UID=$XUID

# viewer：同租户只读角色。读面不可见（不是会话属主）→ 404；写面（停止）→ 403 111203 中间件。
INV=$(curl -sS -X POST "$B/api/tenants/$TEN/invitations" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"email":"bjviewer-'"$STAMP"'@tako.test","role":"viewer"}')
ITOK=$(printf '%s' "$INV" | jq -r '.data.inviteToken // ""')
[ -n "$ITOK" ] && ok "owner 邀 viewer 成功" || { no "邀请失败：$INV"; ml_summary; }
REG=$(curl -sS -X POST "$B/api/auth/register-by-invite" -H 'Content-Type: application/json' \
  -d '{"token":"'"$ITOK"'","email":"bjviewer-'"$STAMP"'@tako.test","password":"changeme123"}')
[ "$(printf '%s' "$REG" | jq -r .code)" = "0" ] || { no "viewer 注册失败：$REG"; ml_summary; }
VLOGIN=$(curl -sS -X POST "$B/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"identifier":"bjviewer-'"$STAMP"'@tako.test","password":"changeme123"}')
VREFRESH=$(printf '%s' "$VLOGIN" | jq -r '.data.refreshToken')
VSW=$(curl -sS -X POST "$B/api/auth/switch-tenant" -H "Authorization: Bearer $(printf '%s' "$VLOGIN" | jq -r .data.accessToken)" -H 'Content-Type: application/json' \
  -d '{"tenantId":"'"$TEN"'","refreshToken":"'"$VREFRESH"'"}')
VTOK=$(printf '%s' "$VSW" | jq -r '.data.accessToken')
eq "viewer 角色就位" "viewer" "$(curl -sS "$B/api/auth/me" -H "Authorization: Bearer $VTOK" | jq -r .data.role)"
V404=$(curl -sS "$B/api/jobs/$BJ3" -H "Authorization: Bearer $VTOK")
eq "viewer 读 job → 404 117117（不可见同形）" "117117" "$(printf '%s' "$V404" | jq -r '.code')"
V403=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$B/api/jobs/$BJ3/stop" -H "Authorization: Bearer $VTOK")
eq "viewer 停止 → 403（写面中间件）" "403" "$V403"

# ══════════════════════════════════════════════════════════════════════════
# 场景 14：短命前台——登记行在（Backgrounded=false）、不进默认列表、不写通知
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 14：短命前台 ═══"
bj_run "调用 bash 工具，command 参数原样传：echo short-lived-$STAMP"
OUT=$(bj_toolout bash)
has2 "退出码：0" "$OUT" && ok "窗口内完成沿用旧结果字节（退出码：0…）" || no "前台短命令字节变了：${OUT:0:120}"
BJ14=$(bj_sql "SELECT id FROM background_jobs WHERE command LIKE '%short-lived-$STAMP%' ORDER BY created_at DESC LIMIT 1")
[ -n "$BJ14" ] && ok "短命令也登记了行（$BJ14）" || no "短命令没登记行"
eq "登记行 Backgrounded=false" "false" "$(bj_bool "SELECT backgrounded FROM background_jobs WHERE id='$BJ14'")"
eq "登记行终态 succeeded" "succeeded" "$(bj_sql "SELECT status FROM background_jobs WHERE id='$BJ14'")"
INLIST=$(curl -sS "$B/api/workspaces/$WID/jobs" -H "Authorization: Bearer $TOK" | jq -r --arg j "$BJ14" '[.data.jobs[] | select(.jobId==$j)] | length')
eq "默认列表不含短命令" "0" "$INLIST"
eq "短命令没有 bash_job_finished 通知" "0" "$(bj_sql "SELECT count(*) FROM agent_notifications WHERE background_job_id='$BJ14'")"

# ══════════════════════════════════════════════════════════════════════════
# 场景 13：不带 workspaceId 的 run 落成员默认项目（doc/42「不再有游离会话」：引擎对不带
# 项目id 的 run 当场复用/创建该成员的默认项目）——job 挂默认项目、输出落项目目录、
# 进默认项目列表。旧前提「无项目会话、输出走 .tako 回退」已随形态收窄废除；
# .tako/bash 回退只剩防御性死分支（施工包与 doc/42 的矛盾 2026-11-15 已上报用户裁定）。
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 13：不带项目的 run 落默认项目 ═══"
ml_run "调用 bash 工具，command 参数原样传：echo default-ws-$STAMP; sleep 15"
ml_wait_settled 120 || no "不带项目的 run 120s 没落定"
ml_frames
OUT=$(bj_toolout bash)
BJ13=$(printf '%s' "$OUT" | jq -r '.jobId // ""' 2>/dev/null)
[ -n "$BJ13" ] && ok "不带项目的 run 拿到命令号 $BJ13" || no "长命令没转后台：${OUT:0:200}"
DWID=$(bj_sql "SELECT workspace_id FROM background_jobs WHERE id='$BJ13'")
eq "job 行 workspaceId 是成员默认项目" "true" \
  "$(bj_bool "SELECT is_default FROM workspaces WHERE id='$DWID' AND user_id='$ML_UID' AND tenant_id='$TEN'")"
REL=$(bj_job "$BJ13" stdoutPath)
eq "逻辑路径指向 job 固定目录（不再走 .tako 回退）" "runtime/tmp/bash/jobs/$BJ13/stdout.out" "$REL"
PHYS="$WS_ROOT/tenants/$TEN/workspaces/$DWID/$REL"
[ -s "$PHYS" ] && ok "输出落默认项目容器：$PHYS" || no "默认项目输出文件不存在：$PHYS"
INWS=$(curl -sS "$B/api/workspaces/$DWID/jobs" -H "Authorization: Bearer $TOK" | jq -r --arg j "$BJ13" '[.data.jobs[] | select(.jobId==$j)] | length')
eq "job 进默认项目的项目列表" "1" "$INWS"
AT_CODE=""; at_http GET "/api/jobs/$BJ13"
eq "单件端点照常可用（GET /api/jobs/:id）" "200" "$AT_CODE"
# 收尾停掉，别留 15s 尾巴拖慢后面
at_http POST "/api/jobs/$BJ13/stop"; bj_wait_job "$BJ13" "stopped" 15 || true
SID=$MAIN_SID   # ml_run 覆写了 SID：后面的跨会话过滤与场景 15 都要回主会话
# 项目列表跨会话过滤：bj1 的 job 用 sessionId 过滤。
F1=$(curl -sS "$B/api/workspaces/$WID/jobs?sessionId=$SID" -H "Authorization: Bearer $TOK" | jq -r --arg j "$BJ1" '[.data.jobs[] | select(.jobId==$j)] | length')
eq "列表 sessionId 过滤命中本会话 job" "1" "$F1"

# ══════════════════════════════════════════════════════════════════════════
# 场景 15：删除保护——running job 挡住删项目/删会话，已写 stop_requested；终态后可删
# ══════════════════════════════════════════════════════════════════════════
echo "═══ 场景 15：删除保护 ═══"
# sleep 240：命令必须在 DELETE 打出来时还在跑（run 最坏落定 120s；2026-11-15 实翻：
# sleep 90 被慢 settle 拖到先终态，DELETE 200 假红）。停止由 watcher 收口，不拖脚本墙钟。
bj_run "调用 bash 工具，command 参数原样传：sleep 240"
OUT=$(bj_toolout bash)
BJ15=$(printf '%s' "$OUT" | jq -r '.jobId // ""' 2>/dev/null)
[ -n "$BJ15" ] && ok "删除保护用的长命令 $BJ15" || { no "sleep 240 没转后台：${OUT:0:200}"; ml_summary; }

DWS=$(curl -sS -w '\n%{http_code}' -X DELETE "$B/api/workspaces/$WID" -H "Authorization: Bearer $TOK")
DCODE=${DWS##*$'\n'}; DBODY=${DWS%$'\n'*}
eq "有 running job 时删项目 → 409" "409" "$DCODE"
eq "项目行没删（还在）" "1" "$(bj_sql "SELECT count(*) FROM workspaces WHERE id='$WID'")"
eq "stop_requested 已写上" "true" "$(bj_bool "SELECT stop_requested FROM background_jobs WHERE id='$BJ15'")"
DSES=$(curl -sS -w '\n%{http_code}' -X DELETE "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK")
DCODE=${DSES##*$'\n'}
eq "有 running job 时删会话 → 409" "409" "$DCODE"
eq "会话行没删" "1" "$(bj_sql "SELECT count(*) FROM sessions WHERE id='$SID' AND deleted_at IS NULL")"

at_http POST "/api/jobs/$BJ15/stop"
bj_wait_job "$BJ15" "stopped" 20 || no "停止后 20s 没到 stopped（$BJ_LAST）"
# 终态通知的交付 run 也要等：通知落库后调度器给来源会话铸交付 run（真模型一轮），
# 它活着时 DELETE 会话/项目都被「活 run」挡成 409——必须等通知消费完（2026-11-15 实翻）。
deadline=$((SECONDS + 150)); NF15=""
while [ "$SECONDS" -lt "$deadline" ]; do
  NF15=$(bj_sql "SELECT status FROM agent_notifications WHERE background_job_id='$BJ15' AND kind='bash_job_finished' ORDER BY created_at DESC LIMIT 1")
  [ "$NF15" = "consumed" ] || [ "$NF15" = "delivered" ] && break
  sleep 2
done
case "$NF15" in
  consumed|delivered) ok "终态通知已消费（$NF15），交付 run 不再占会话";;
  *) no "通知没消费（status=$NF15），终态 DELETE 会撞交付 run";;
esac
DSES=$(curl -sS -w '\n%{http_code}' -X DELETE "$B/api/sessions/$SID" -H "Authorization: Bearer $TOK")
DCODE=${DSES##*$'\n'}
eq "终态后删会话 → 200" "200" "$DCODE"
DWS=$(curl -sS -w '\n%{http_code}' -X DELETE "$B/api/workspaces/$WID" -H "Authorization: Bearer $TOK")
DCODE=${DWS##*$'\n'}
eq "终态后删项目 → 200" "200" "$DCODE"

ml_summary
