#!/usr/bin/env bash
# 持久 Task 活体脚本（doc/70；施工包 §12.3）的共享底座：**source 用，不直接执行**。
# 注册 / 拉帧 / SQL 复用 memory_live_lib.sh 的同一份（那是通用底座，ml_ 前缀是历史命名），
# 这里只加 Task／编排组自己的等待与断言帮手，免得「五个脚本各写一遍轮询 GET /api/tasks」。
#
# 参数与纪律见 memory_live_lib.sh 头注；Task 活体额外约定：
#   - 预起引擎类脚本（single / dag / crash / busy）的引擎必须开着调度循环
#     （默认 -task-interval 1s 就是开；契约断言那种 -task-interval=-1s 的引擎跑不了活体）；
#   - 建议给引擎配 TAKO_MEMORY_MODE=markdown（免 broker；这几场不验记忆行为）。
#   - kill 只打 TAKO_ENGINE_PIDS 明确指定的隔离实例或脚本自己起的进程；不调 /api/dev/reset。

source "$(dirname "${BASH_SOURCE[0]}")/memory_live_lib.sh"

# at_task $1=taskId → 把 GET /api/tasks/:id 的 data 存进 AT_TASK。
at_task(){
  AT_TASK=$(curl -sS "$B/api/tasks/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data // {}')
}

# at_orch $1=orchId → 把 GET /api/orchestrations/:id 的 data 存进 AT_ORCH（组 + tasks + links）。
at_orch(){
  AT_ORCH=$(curl -sS "$B/api/orchestrations/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data // {}')
}

# at_task_status $1=taskId → 只回状态词（网络坏了回空串，调用方当未达处理）。
at_task_status(){
  curl -sS "$B/api/tasks/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data.status // ""' 2>/dev/null
}

# at_orch_status $1=orchId → 组状态词。
at_orch_status(){
  curl -sS "$B/api/orchestrations/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r '.data.orchestration.status // ""' 2>/dev/null
}

# at_orch_field $1=orchId $2=组半字段名（deliveryStatus / deliveryRunId / finalTaskId…）。
at_orch_field(){
  curl -sS "$B/api/orchestrations/$1" -H "Authorization: Bearer $TOK" 2>/dev/null | jq -r ".data.orchestration.$2 // \"\"" 2>/dev/null
}

# at_wait_task $1=taskId $2=目标状态 $3=超时秒（默认 240）。等到即回 0；超时回 1 并把最后状态存 AT_LAST。
at_wait_task(){
  local deadline=$((SECONDS + ${3:-240})) st
  AT_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_task_status "$1")
    AT_LAST="$st"
    [ "$st" = "$2" ] && return 0
    sleep 1
  done
  return 1
}

# at_wait_orch $1=orchId $2=目标状态 $3=超时秒（默认 300）。
at_wait_orch(){
  local deadline=$((SECONDS + ${3:-300})) st
  AT_LAST=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_orch_status "$1")
    AT_LAST="$st"
    [ "$st" = "$2" ] && return 0
    sleep 1
  done
  return 1
}

# at_wait_delivered $1=orchId $2=超时秒（默认 240）：等到 deliveryStatus=delivered。
at_wait_delivered(){
  local deadline=$((SECONDS + ${2:-240})) st
  while [ "$SECONDS" -lt "$deadline" ]; do
    st=$(at_orch_field "$1" "deliveryStatus")
    [ "$st" = "delivered" ] && return 0
    sleep 1
  done
  return 1
}

# at_create_task $1=idempotencyKey $2=workspaceId $3=sourceSessionId $4=content $5=agent
# → 回应原文（调用方自己 jq）。请求体用 jq 拼，content 里的引号不用手工转义。
at_create_task(){
  jq -cn --arg k "$1" --arg w "$2" --arg s "$3" --arg c "$4" --arg a "$5" \
    '{idempotencyKey:$k,workspaceId:$w,sourceSessionId:$s,content:$c,agent:$a}' |
  curl -sS -X POST "$B/api/tasks" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @-
}

# at_create_orch $1=idempotencyKey $2=workspaceId $3=sourceSessionId $4=goal $5=tasksJSON $6=linksJSON
# tasksJSON/linksJSON 是已经拼好的 JSON 数组字面量（脚本里用 jq -cn 现拼）。
at_create_orch(){
  jq -cn --arg k "$1" --arg w "$2" --arg s "$3" --arg g "$4" \
    --argjson t "$5" --argjson l "$6" \
    '{idempotencyKey:$k,workspaceId:$w,sourceSessionId:$s,goal:$g,tasks:$t,links:$l}' |
  curl -sS -X POST "$B/api/orchestrations" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @-
}

# at_notif_count $1=orchId $2=kind [$3=status] → 通知行数（计数用，去空白）。
at_notif_count(){
  local cond="orchestration_id='$1' AND kind='$2'"
  [ -n "${3:-}" ] && cond="$cond AND status='$3'"
  ml_sql "SELECT count(*) FROM agent_notifications WHERE $cond"
}

# at_session_runs $1=sessionId → 该会话的 run 行数（交付只一场的断言用）。
at_session_runs(){
  ml_sql "SELECT count(*) FROM runs WHERE session_id='$1'"
}

# at_probe_run $1=workspaceId：起一场「只回复 ok」的探针 run 造来源会话（**必须挂着项目**——
# 创建 Task 要求来源会话的 workspace_id 与命令一致），等落定。sets SID（来源会话 id）。
# 超时退出非零（调用方直接失败——没有来源会话后面全免谈）。
at_probe_run(){
  ml_run '只回复 ok' "$1"
  ml_wait_settled 60 || { echo "探针 run 60s 没落定，来源会话造不出来" >&2; exit 2; }
}

# at_http $1=METHOD $2=path $3=请求体（可省）→ sets AT_CODE / AT_BODY。
# 带当前 TOK；-w 把状态码放最后一行，正文原样保留（JSON 本身不带换行，切分安全）。
at_http(){
  local out=""
  if [ -n "${3:-}" ]; then
    out=$(curl -sS -w '\n%{http_code}' -X "$1" "$B$2" \
      -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$3" 2>/dev/null)
  else
    out=$(curl -sS -w '\n%{http_code}' -X "$1" "$B$2" \
      -H "Authorization: Bearer $TOK" 2>/dev/null)
  fi
  if [ -z "$out" ]; then AT_CODE=""; AT_BODY=""; return 0; fi
  AT_CODE=${out##*$'\n'}
  AT_BODY=${out%$'\n'*}
}

# at_revision $1=orchId $2=请求体 JSON → 响应原文（POST /revisions）。
at_revision(){
  curl -sS -X POST "$B/api/orchestrations/$1/revisions" \
    -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$2" 2>/dev/null
}

# at_continue $1=orchId $2=baseRevision → 响应原文（POST /continue）。
at_continue(){
  curl -sS -X POST "$B/api/orchestrations/$1/continue" \
    -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
    -d "{\"baseRevision\":$2}" 2>/dev/null
}

# at_revisions $1=orchId → 修订历史响应原文。
at_revisions(){
  curl -sS "$B/api/orchestrations/$1/revisions" -H "Authorization: Bearer $TOK" 2>/dev/null
}

# at_wait_hold $1=orchId $2=超时秒（默认 240）：等到组 planHold=true。
at_wait_hold(){
  local deadline=$((SECONDS + ${2:-240})) v
  while [ "$SECONDS" -lt "$deadline" ]; do
    v=$(at_orch_field "$1" planHold)
    [ "$v" = "true" ] && return 0
    sleep 1
  done
  return 1
}

# at_attention $1=workspaceId → 「需要我处理」聚合响应原文。
at_attention(){
  curl -sS "$B/api/workspaces/$1/task-attention" -H "Authorization: Bearer $TOK" 2>/dev/null
}

# at_task_questions $1=taskId [$2=status] → Task 问题行响应原文。
at_task_questions(){
  local q="$B/api/tasks/$1/questions"
  [ -n "${2:-}" ] && q="$q?status=$2"
  curl -sS "$q" -H "Authorization: Bearer $TOK" 2>/dev/null
}

# at_wait_task_question $1=taskId $2=超时秒（默认 180）：等这件 Task 当前执行出现 pending 问题。
# sets AT_QID / AT_QUESTION（问题全文）。
at_wait_task_question(){
  local deadline=$((SECONDS + ${2:-180})) body
  AT_QID=""; AT_QUESTION=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(at_task_questions "$1" pending)
    AT_QID=$(echo "$body" | jq -r '.data[0].questionId // ""' 2>/dev/null)
    AT_QUESTION=$(echo "$body" | jq -r '.data[0].question // ""' 2>/dev/null)
    [ -n "$AT_QID" ] && [ "$AT_QID" != "null" ] && return 0
    sleep 1
  done
  return 1
}

# at_answer $1=questionId $2=answer [$3=decision] → 响应原文（POST /api/questions/:id/answer）。
at_answer(){
  local body
  if [ -n "${3:-}" ]; then
    body=$(jq -cn --arg a "$2" --arg d "$3" '{answer:$a,decision:$d}')
  else
    body=$(jq -cn --arg a "$2" '{answer:$a}')
  fi
  curl -sS -X POST "$B/api/questions/$1/answer" \
    -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d "$body" 2>/dev/null
}
