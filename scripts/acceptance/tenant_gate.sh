#!/usr/bin/env bash
# 租户并发闸（doc/43）的活体验收：一个上限 2 的租户，并发打 4 次 POST /api/runs。
#
# 这条验的是**闸在 DB 里**这件事的对外形状：并发下**恰好**上限那么多被放行，其余当场被拒。
# 修之前（数-判-写在 API 层、无锁）这一发会是 4 个 202 —— 上限 2 却进来 4 场。
#
# 断言有四条，缺一不可：
#   1. 放行数 = 上限（不多不少）
#   2. 其余全部 429，且响应体逐字 = {"code":113001,"message":"这个工作区的额度用完了","data":null}
#   3. 库里这个租户的 run 行数 = 上限（run 行不删，跑完也还在 → 比「活 run 数」耐打）
#   4. 被拒的请求零痕迹：这个租户的会话行数 = 上限
#
# 参数（环境变量，不写死本机路径与端口）：
#   TAKO_BASE      引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV       取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN    直连 PG 的 DSN（**必填**：改上限与数行数靠它）
#   TAKO_PSQL      psql 可执行文件，默认 psql
#   TAKO_LIMIT     租户并发上限，默认 2
#   TAKO_BURST     并发打几次，默认 4（要 > 上限才看得出闸在拒）
#
# 跑之前先起一个**隔离**引擎（别拿开发库当靶子），见同目录 README.md。
# 注意：租户必须装好模型密钥，否则 POST /api/runs 先回 409 111408，测不到闸。
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8943}"
ENV="${TAKO_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/yk-tako-eino/.env}"
PSQL="${TAKO_PSQL:-psql}"
DSN="${TAKO_PG_DSN:-}"
LIMIT="${TAKO_LIMIT:-2}"
BURST="${TAKO_BURST:-4}"

P=0; F=0
ok(){ echo "  ✅ $1"; P=$((P+1)); }
no(){ echo "  ❌ $1"; F=$((F+1)); }
eq(){ if [ "$2" = "$3" ]; then ok "$1"; else no "$1 —— 期望 [$2] 实际 [$3]"; fi; }
J="Content-Type: application/json"

if [ -z "$DSN" ]; then echo "必填参数缺了：TAKO_PG_DSN（见本文件头）" >&2; exit 2; fi
if [ "$BURST" -le "$LIMIT" ]; then echo "并发次数（${BURST}）要大于上限（${LIMIT}）才看得出闸在拒" >&2; exit 2; fi
KEY=$(grep '^DEEPSEEK_API_KEY=' "$ENV" | head -1 | cut -d= -f2- | tr -d '"')
if [ -z "$KEY" ]; then echo "读不到模型密钥：${ENV}（用 TAKO_ENV 指定）" >&2; exit 2; fi

sql(){ "$PSQL" "$DSN" -tAc "$1" 2>/dev/null | tr -d ' '; }

# 1) 注册 → token + 租户 id（每次跑都建新租户，互不干扰）
EMAIL="gate_$(date +%s%N)@tako.test"
SIGNUP=$(curl -s -X POST "$B/api/auth/signup" -H "$J" -d "{\"email\":\"$EMAIL\",\"password\":\"changeme123\"}")
TOKEN=$(printf '%s' "$SIGNUP" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['accessToken'])")
TID=$(printf '%s' "$SIGNUP" | python3 -c "import sys,json; print(json.load(sys.stdin)['data']['tenant']['id'])")
[ -z "$TOKEN" ] || [ -z "$TID" ] && { echo "注册失败：$SIGNUP" >&2; exit 2; }

# 2) 租户装模型密钥（没装 → POST 先回 409 111408，测不到闸）
CFG=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$B/api/tenants/$TID/runtime-config" \
  -H "Authorization: Bearer $TOKEN" -H "$J" \
  -d "{\"modelRef\":\"deepseek/deepseek-flash\",\"mcpServers\":{},\"secrets\":{\"DEEPSEEK_API_KEY\":\"$KEY\"},\"sandbox\":{\"network\":false}}")
eq "租户装模型密钥" "200" "$CFG"

# 3) 上限调成 LIMIT
sql "UPDATE tenants SET max_concurrent_runs = $LIMIT WHERE id = '$TID'" >/dev/null
eq "租户上限已调成 $LIMIT" "$LIMIT" "$(sql "SELECT max_concurrent_runs FROM tenants WHERE id = '$TID'")"

# 4) 并发 BURST 打（四个不同会话 → 会话闸不参与，只考租户闸）
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
for i in $(seq 1 "$BURST"); do
  ( curl -s -o "$TMP/b$i.json" -w '%{http_code}' -X POST "$B/api/runs" \
      -H "Authorization: Bearer $TOKEN" -H "$J" \
      -d "{\"text\":\"并发闸活体验收 $i\",\"messageId\":\"m_gate_$i\"}" > "$TMP/c$i.txt" ) &
done
wait

ACCEPTED=0; REJECTED=0; OTHER=0
for i in $(seq 1 "$BURST"); do
  CODE=$(cat "$TMP/c$i.txt")
  case "$CODE" in
    202) ACCEPTED=$((ACCEPTED+1)) ;;
    429) REJECTED=$((REJECTED+1)) ;;
    *)   OTHER=$((OTHER+1)); echo "     第 $i 个请求 unexpected HTTP $CODE：$(cat "$TMP/b$i.json")" ;;
  esac
done

# 断言 1：放行数 = 上限
eq "放行数 = 上限 $LIMIT" "$LIMIT" "$ACCEPTED"
# 断言 2：其余全是 429，且响应体逐字一致
eq "被拒数 = $BURST - 上限" "$((BURST - LIMIT))" "$REJECTED"
eq "没有别的结局（既非 202 也非 429）" "0" "$OTHER"
for i in $(seq 1 "$BURST"); do
  if [ "$(cat "$TMP/c$i.txt")" = "429" ]; then
    BODY=$(cat "$TMP/b$i.json")
    eq "第 $i 个 429 的响应体逐字" '{"code":113001,"message":"这个工作区的额度用完了","data":null}' "$BODY"
  fi
done
# 断言 3：库里 run 行数 = 上限
eq "库里这个租户的 run 行数 = $LIMIT" "$LIMIT" \
  "$(sql "SELECT count(*) FROM runs r JOIN sessions s ON s.id = r.session_id WHERE s.tenant_id = '$TID'")"
# 断言 4：被拒的请求零痕迹（会话行数 = 上限）
eq "被拒请求没留下会话行（会话数 = ${LIMIT}）" "$LIMIT" \
  "$(sql "SELECT count(*) FROM sessions WHERE tenant_id = '$TID'")"

echo
echo "═══ PASS $P / FAIL $F ═══"
[ "$F" -eq 0 ] || exit 1
