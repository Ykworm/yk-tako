#!/usr/bin/env bash
# dialogue_hybrid_live_lib.sh — 历史对话混合检索活体验收（doc/84）公共库。
# 被 dialogue_hybrid_live.sh / dialogue_hybrid_crash_live.sh / dialogue_hybrid_two_replicas_live.sh /
# dialogue_hybrid_rebuild_live.sh source。提供：断言计数、curl 封装、注册/租户、
# SQL 造数（expected.json 用例 65/85 同款：status='complete' 的 run + runlog_entries，
# 让引擎自己的索引循环走完整真实链路——不打模型，绕开 DEEPSEEK 402）、收敛等待、
# ES/Milvus 计数、Langfuse v2 span 检查。
#
# 依赖：调用方已 source dialogue_hybrid_up.sh 产出的 state.env（DH_PG_DSN/DH_WS/
# DH_ES_INDEX/DH_MILVUS_COLL/DH_EVAL_BIN 等）并设置了 DH_PSQL/DH_ENV_FILE/DH_NAME。
set -u

# ---------- 断言 ----------
DH_OK=0; DH_FAIL=0; DH_REGISTERED=0

ok() { DH_OK=$((DH_OK+1)); echo "  ✓ $*"; }
no() { DH_FAIL=$((DH_FAIL+1)); echo "  ✗ $*"; }
registered() { DH_REGISTERED=$((DH_REGISTERED+1)); echo "  ▸ 登记 $*"; }
eq() { # eq <got> <want> <说明>
  if [ "$1" = "$2" ]; then ok "$3 = $2"; else no "$3：got=[$1] want=[$2]"; fi
}
dh_summary() {
  echo ""
  echo "总账：绿 $DH_OK / 红 $DH_FAIL / 登记 $DH_REGISTERED"
  [ "$DH_FAIL" -eq 0 ]
}

# ---------- 基础 ----------
dh_api_at() { # dh_api_at <base> <token> <method> <path> [body] → 响应体
  local base="$1" tok="$2" method="$3" path="$4" body="${5:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$base$path" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS -X "$method" "$base$path" -H "Authorization: Bearer $tok"
  fi
}

dh_http_code_at() { # dh_http_code_at <base> <token> <method> <path> [body] → HTTP 状态码
  local base="$1" tok="$2" method="$3" path="$4" body="${5:-}"
  if [ -n "$body" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$method" "$base$path" \
      -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d "$body"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$method" "$base$path" -H "Authorization: Bearer $tok"
  fi
}

dh_sql() { "$DH_PSQL" "${DH_PG_DSN:?DH_PG_DSN 未设}" -qAt -v ON_ERROR_STOP=1 -c "$1"; }

dh_now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# ---------- 注册/租户 ----------
# dh_signup <base> [tag]：注册账号，设 DH_TOK/DH_TEN/DH_UID/DH_EMAIL（tag 区分多账号）。
dh_signup() {
  local base="$1" tag="${2:-a}"
  local email="dh_${DH_NAME}_${tag}_$(date +%s)_$RANDOM@example.test"
  local res
  res="$(curl -sS -X POST "$base/api/auth/signup" -H 'Content-Type: application/json' \
    -d '{"email":"'"$email"'","password":"pass-12345678"}')"
  DH_TOK="$(echo "$res" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["data"]["accessToken"])')"
  DH_EMAIL="$email"
  local me
  me="$(curl -sS "$base/api/auth/me" -H "Authorization: Bearer $DH_TOK")"
  DH_TEN="$(echo "$me" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["data"]["tenantId"])')"
  DH_UID="$(echo "$me" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["data"]["userId"])')"
}

# dh_mk_ws <base> <token> <name> → workspace id
dh_mk_ws() {
  curl -sS -X POST "$1/api/workspaces" -H "Authorization: Bearer $2" \
    -H 'Content-Type: application/json' -d '{"name":"'"$3"'"}' \
    | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["data"]["workspace"]["id"])'
}

# ---------- SQL 造数（不打模型；expected.json 用例 65/85 同款） ----------
# dh_seed_run <sid> <rid> <wid-or-NULL> <u1> <t1> <u2> <t2>
#   在当前租户/用户（DH_TEN/DH_UID）下造 status='complete' 的 run + 四条 runlog_entries
#   （user/assistant 问答对两条），索引器会把整场 run 切成 chunk。
dh_seed_run() {
  local sid="$1" rid="$2" wid="$3" u1="$4" t1="$5" u2="$6" t2="$7"
  dh_sql "INSERT INTO sessions (id, title, tenant_id, user_id, visibility, workspace_id, kind, created_at)
    SELECT '$sid', 'dh seed', '$DH_TEN', '$DH_UID', 'private', $wid, 'chat', now()
    WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE id='$sid')"
  dh_sql "INSERT INTO runs (id, session_id, status, lease_epoch, lease_until, created_at, finished_at, workspace_id)
    VALUES ('$rid', '$sid', 'complete', 1, now(), now(), now(), $wid)
    ON CONFLICT (id) DO NOTHING"
  dh_sql "INSERT INTO runlog_entries (id, session_id, seq, run_id, role, agent, model, message_id, text, payload, created_at) VALUES
    ('rl_${rid}a', '$sid', 1, '$rid', 'user', '', '', '', '$(echo "$u1" | sed "s/'/''/g")', '', now()),
    ('rl_${rid}b', '$sid', 2, '$rid', 'assistant', '', '', '', '$(echo "$t1" | sed "s/'/''/g")', '', now()),
    ('rl_${rid}c', '$sid', 3, '$rid', 'user', '', '', '', '$(echo "$u2" | sed "s/'/''/g")', '', now()),
    ('rl_${rid}d', '$sid', 4, '$rid', 'assistant', '', '', '', '$(echo "$t2" | sed "s/'/''/g")', '', now())
    ON CONFLICT (id) DO NOTHING"
}

# ---------- 收敛等待 ----------
# dh_wait_jobs_settled <run_id> <timeout_sec>：该 run 的 ES/Milvus 两条 job 都 done（或 failed）。
dh_wait_jobs_settled() {
  local rid="$1" timeout="${2:-120}"
  local waited=0
  while [ "$waited" -lt "$timeout" ]; do
    local unsettled rows
    rows="$(dh_sql "SELECT count(*) FROM dialogue_index_jobs WHERE run_id='$rid'")"
    unsettled="$(dh_sql "SELECT count(*) FROM dialogue_index_jobs
      WHERE run_id='$rid' AND (status NOT IN ('done','failed') OR done_at IS NULL)")"
    # 行数>0 是硬条件：行不存在（种子失败/索引循环竞态丢登记）绝不能当收敛假绿。
    [ "$unsettled" = "0" ] && [ "${rows:-0}" -ge 1 ] && return 0
    sleep 1; waited=$((waited+1))
  done
  return 1
}

# dh_query_merged_runs <base> <token> <workspace> <query> → merged 数组里的 runId 列表
dh_query_merged_runs() {
  curl -sS -X POST "$1/api/dev/dialogue-retrieval/query" \
    -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -d '{"query":"'"$4"'","workspaceId":"'"$3"'"}' \
    | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
    if d.get('code')!=0 or not d.get('data'): print('ERR'); sys.exit()
    print(' '.join(m['runId'] for m in (d['data'].get('merged') or [])))
except Exception:
    print('ERR')"
}

dh_wait_query_hit() { # dh_wait_query_hit <base> <tok> <ws> <query> <run_id> [timeout_sec]
  local base="$1" tok="$2" ws="$3" q="$4" rid="$5" timeout="${6:-120}"
  local waited=0
  while [ "$waited" -lt "$timeout" ]; do
    dh_query_merged_runs "$base" "$tok" "$ws" "$q" | grep -qw "$rid" && return 0
    sleep 2; waited=$((waited+2))
  done
  return 1
}

dh_wait_query_clear() { # dh_wait_query_clear <base> <tok> <ws> <query> <run_id> [timeout_sec]
  local base="$1" tok="$2" ws="$3" q="$4" rid="$5" timeout="${6:-120}"
  local waited=0
  while [ "$waited" -lt "$timeout" ]; do
    dh_query_merged_runs "$base" "$tok" "$ws" "$q" | grep -qw "$rid" || return 0
    sleep 2; waited=$((waited+2))
  done
  return 1
}

# dh_recall_block <base> <token> <query> → memory recall 的整块响应（q 走 urlencode）
dh_recall_block() {
  curl -sS -G "$1/api/memory/recall" -H "Authorization: Bearer $2" --data-urlencode "q=$3"
}

# Go/gin 的 JSON 编码器会把 < > & 转义成 \u003c \u003e \u0026——对原始响应体 grep
# `<dialogue-context>` 永远落空（既会误红也会假绿）。要判断注入块内容，先解码再查。
dh_recall_block_text() { # dh_recall_block_text <recall响应json> → 解码后的 block 字段
  echo "$1" | python3 -c 'import json,sys
try: print((json.load(sys.stdin).get("data") or {}).get("block") or "")
except Exception: print("")'
}
dh_recall_hits() { # dh_recall_hits <recall响应json> → dialogueHits 数字
  echo "$1" | python3 -c 'import json,sys
v=(json.load(sys.stdin).get("data") or {}).get("dialogueHits")
print("" if v is None else v)'
}

# ---------- ES ----------
DH_ES_URL=""; DH_ES_USER=""; DH_ES_PASS=""
dh_es_conf() { # dh_es_conf <.env 路径>
  local f="$1"
  DH_ES_URL="$(grep -E '^TAKO_ES_URL=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  DH_ES_USER="$(grep -E '^TAKO_ES_USERNAME=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  DH_ES_PASS="$(grep -E '^TAKO_ES_PASSWORD=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
}

dh_es() { # dh_es <method> <path> [body] → 响应体
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -sS -X "$m" -u "$DH_ES_USER:$DH_ES_PASS" "$DH_ES_URL$p" -H 'Content-Type: application/json' -d "$b"
  else
    curl -sS -X "$m" -u "$DH_ES_USER:$DH_ES_PASS" "$DH_ES_URL$p"
  fi
}

dh_es_count() { # dh_es_count <index> [count-query-json] → 数字（索引不存在=0）
  local idx="$1" q="${2:-}" body
  if [ -n "$q" ]; then
    body="$(dh_es POST "/$idx/_count" "$q")"
  else
    body="$(dh_es GET "/$idx/_count")"
  fi
  if echo "$body" | grep -q '"count"'; then
    echo "$body" | python3 -c 'import json,sys;print(json.load(sys.stdin)["count"])'
  else
    echo 0
  fi
}

dh_es_exists() { # dh_es_exists <index> → HEAD 200 = 存在
  [ "$(curl -s -o /dev/null -w '%{http_code}' -u "$DH_ES_USER:$DH_ES_PASS" "$DH_ES_URL/$1")" = "200" ]
}

# ---------- Milvus ----------
# dh_eval <子命令...>：eval 工具统一入口。工具从**进程环境**读 TAKO_MILVUS_URI/TOKEN/DB/USE_TLS
# （cmd/tako-dialogue-eval/main.go:38-41），而 live 脚本进程故意不 source 整份 .env（避免把全部
# TAKO_* 泄进进程）——这里从 DH_ENV_FILE 精确取四个变量注入给子进程；USE_TLS 的 true 归一成 1
# （工具按 =="1" 判）。
dh_eval() {
  local uri token db tls
  uri="$(grep -E '^TAKO_MILVUS_URI=' "$DH_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  token="$(grep -E '^TAKO_MILVUS_TOKEN=' "$DH_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  db="$(grep -E '^TAKO_MILVUS_DB=' "$DH_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  tls="$(grep -E '^TAKO_MILVUS_USE_TLS=' "$DH_ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  [ "$tls" = "true" ] || [ "$tls" = "TRUE" ] && tls=1
  TAKO_MILVUS_URI="$uri" TAKO_MILVUS_TOKEN="$token" TAKO_MILVUS_DB="$db" TAKO_MILVUS_USE_TLS="$tls" \
    "$DH_EVAL_BIN" "$@"
}
dh_mil_has() { dh_eval has -collection "$1"; }
dh_mil_count() { # dh_mil_count <collection> [expr]
  if [ -n "${2:-}" ]; then
    dh_eval count -collection "$1" -expr "$2"
  else
    dh_eval count -collection "$1"
  fi
}
dh_mil_drop() { dh_eval drop -collection "$1"; }

# ---------- Langfuse v2 ----------
DH_LF_URL=""; DH_LF_PK=""; DH_LF_SK=""
dh_lf_conf() { # dh_lf_conf <.env 路径>
  local f="$1"
  DH_LF_URL="$(grep -E '^LANGFUSE_BASE_URL=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  DH_LF_PK="$(grep -E '^LANGFUSE_PUBLIC_KEY=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
  DH_LF_SK="$(grep -E '^LANGFUSE_SECRET_KEY=' "$f" | tail -1 | cut -d= -f2- | tr -d '"' | tr -d "'")"
}

# dh_lf_span <name> <from_iso> → 该时间窗后该 span 的观测条数（0=没有）
dh_lf_span() {
  local name="$1" from_iso="$2"
  # --noproxy '*'：Qoder shell 注入的 HTTP(S)_PROXY 会掐断到 Langfuse 云的 curl
  # （HTTP2 framing / SSL_ERROR_SYSCALL，live4 实测 8 个 span 全被误判 0）；直连可达。
  curl -sS --noproxy '*' -u "$DH_LF_PK:$DH_LF_SK" \
    "$DH_LF_URL/api/public/v2/observations?fields=core,basic&fromStartTime=$from_iso&name=$name&limit=1" \
    2>/dev/null | python3 -c 'import json,sys
try: print(len(json.load(sys.stdin).get("data",[])))
except Exception: print(0)'
}
