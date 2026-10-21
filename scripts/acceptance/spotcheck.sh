#!/usr/bin/env bash
# 三篇收口的活体抽验（doc/28 多租户 / doc/35 项目目录 / doc/32 media_offload）。
#
# 这是**活体抽验**：起真账号、真模型、真文件落盘，逐条断言。断言写在脚本里（人读的 eq/has/nothas）。
# 契约字节的机器断言是同目录的 assert_contract.py（读 expected.json 判红绿）。
#
# 参数（全走环境变量，不再写死本机路径与端口）：
#   TAKO_BASE    引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENV    取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#
# 跑之前先起一个隔离引擎（别拿开发库当靶子），见同目录 README.md。
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8943}"
ENV="${TAKO_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/yk-tako-eino/.env}"
KEY=$(grep '^DEEPSEEK_API_KEY=' "$ENV" | head -1 | cut -d= -f2- | tr -d '"')
if [ -z "$KEY" ]; then
  echo "读不到模型密钥：$ENV（用 TAKO_ENV 指定）" >&2
  exit 2
fi
P=0; F=0
ok(){ echo "  ✅ $1"; P=$((P+1)); }
no(){ echo "  ❌ $1"; F=$((F+1)); }
eq(){ if [ "$2" = "$3" ]; then ok "$1"; else no "$1 —— 期望 [$2] 实际 [$3]"; fi; }
has(){ if echo "$2" | grep -q "$3"; then ok "$1"; else no "$1 —— [$2] 里没有 [$3]"; fi; }
nothas(){ if echo "$2" | grep -q "$3"; then no "$1 —— 不该出现 [$3]"; else ok "$1"; fi; }
J="Content-Type: application/json"
STAMP="$(date +%s)$$"
IMG=$(printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde\x00\x00\x00\x0cIDAT\x08\xd7c\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xdc\xccY\xe7\x00\x00\x00\x00IEND\xaeB\x60\x82' | base64)

echo "引擎 $B · 密钥取自 $ENV"

# reg <email> → 设全局 TOK / TEN / ROLE / REFRESH（租户 id 在 signup 响应的 data.tenant.id）
reg(){
  R=$(curl -sS -X POST $B/api/auth/signup -H "$J" -d '{"email":"'"$1"'","password":"changeme123"}')
  TOK=$(echo "$R" | jq -r .data.accessToken)
  REFRESH=$(echo "$R" | jq -r .data.refreshToken)
  TEN=$(echo "$R" | jq -r .data.tenant.id)
  ROLE=$(curl -sS $B/api/auth/me -H "Authorization: Bearer $TOK" | jq -r .data.role)
  # 配租户模型密钥，否则 POST /api/runs 回 111408「模型还不可用」
  curl -sS -o /dev/null -X PUT $B/api/tenants/$TEN/runtime-config -H "Authorization: Bearer $TOK" -H "$J" \
    -d '{"modelRef":"deepseek/deepseek-flash","mcpServers":{},"secrets":{"DEEPSEEK_API_KEY":"'"$KEY"'"},"sandbox":{"network":false}}'
}
# switch_to <tenant> —— 切租户会**换发一枚新 accessToken**，必须换手
switch_to(){
  SW=$(curl -sS -X POST $B/api/auth/switch-tenant -H "Authorization: Bearer $TOK" -H "$J" \
    -d '{"tenantId":"'"$1"'","refreshToken":"'"$REFRESH"'"}')
  TOK=$(echo "$SW" | jq -r .data.accessToken)
  REFRESH=$(echo "$SW" | jq -r .data.refreshToken)
  TEN=$(echo "$SW" | jq -r .data.activeTenant.id)
  ROLE=$(curl -sS $B/api/auth/me -H "Authorization: Bearer $TOK" | jq -r .data.role)
}

echo "═══ 一、doc/28 多租户 ═══"
reg alice_$STAMP@tako.test
ATOK=$TOK; ATEN=$TEN; AROLE=$ROLE
eq "alice role" "owner" "$AROLE"
has "alice 有个人 tenant" "$ATEN" "^t_"

TEAM=$(curl -sS -X POST $B/api/tenants -H "Authorization: Bearer $ATOK" -H "$J" -d '{"name":"TeamA"}')
TTEN=$(echo "$TEAM" | jq -r .data.tenant.id)
eq "alice 建团队 → code" "0" "$(echo "$TEAM" | jq -r .code)"
TOK=$ATOK; REFRESH=$(curl -sS -X POST $B/api/auth/login -H "$J" -d '{"email":"alice_'"$STAMP"'@tako.test","password":"changeme123"}' | jq -r .data.refreshToken)
switch_to "$TTEN"
ATOK=$TOK; ATEN=$TEN; AROLE=$ROLE
eq "alice 切到团队后 activeTenant" "$TTEN" "$ATEN"
eq "alice 在团队里 role" "owner" "$AROLE"
# 团队租户也得配模型密钥，否则 POST /api/runs 回 111408
curl -sS -o /dev/null -X PUT $B/api/tenants/$TTEN/runtime-config -H "Authorization: Bearer $ATOK" -H "$J" \
  -d '{"modelRef":"deepseek/deepseek-flash","mcpServers":{},"secrets":{"DEEPSEEK_API_KEY":"'"$KEY"'"},"sandbox":{"network":false}}'
INV=$(curl -sS -X POST $B/api/tenants/$TTEN/invitations -H "Authorization: Bearer $ATOK" -H "$J" -d '{"email":"bob_'"$STAMP"'@tako.test","role":"member"}')
ITOK=$(echo "$INV" | jq -r .data.inviteToken)
eq "alice 邀 bob → code" "0" "$(echo "$INV" | jq -r .code)"
REG=$(curl -sS -X POST $B/api/auth/register-by-invite -H "$J" -d '{"token":"'"$ITOK"'","email":"bob_'"$STAMP"'@tako.test","password":"changeme123"}')
eq "bob 用邀请注册 → code" "0" "$(echo "$REG" | jq -r .code)"

reg bob_$STAMP@tako.test
BTOK=$TOK; BTEN=$TEN
if [ "$ATEN" != "$BTEN" ]; then ok "alice 与 bob 是不同 tenant"; else no "alice 与 bob 同 tenant"; fi
BL=$(curl -sS -X POST $B/api/auth/login -H "$J" -d '{"email":"bob_'"$STAMP"'@tako.test","password":"changeme123"}')
BTOK=$(echo "$BL" | jq -r .data.accessToken); BREFRESH=$(echo "$BL" | jq -r .data.refreshToken)
SW=$(curl -sS -X POST $B/api/auth/switch-tenant -H "Authorization: Bearer $BTOK" -H "$J" -d '{"tenantId":"'"$TTEN"'","refreshToken":"'"$BREFRESH"'"}')
eq "bob 切到团队 → code" "0" "$(echo "$SW" | jq -r .code)"
BTEAMTOK=$(echo "$SW" | jq -r .data.accessToken)
BM=$(curl -sS $B/api/auth/me -H "Authorization: Bearer $BTEAMTOK")
eq "bob 切后 activeTenant" "$TTEN" "$(echo "$BM" | jq -r .data.tenantId)"
eq "bob 在团队里 role" "member" "$(echo "$BM" | jq -r .data.role)"
eq "member 邀人 → 403" "403" "$(curl -sS -o /dev/null -w '%{http_code}' -X POST $B/api/tenants/$TTEN/invitations -H "Authorization: Bearer $BTEAMTOK" -H "$J" -d '{"email":"x@y.z","role":"member"}')"
eq "owner 读团队成员 → code" "0" "$(curl -sS $B/api/tenants/$TTEN/members -H "Authorization: Bearer $ATOK" | jq -r .code)"

ARUN=$(curl -sS -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_a1","text":"记住：项目代号是 ZQ-7781"}')
ASID=$(echo "$ARUN" | jq -r .data.sessionId); ARID=$(echo "$ARUN" | jq -r .data.runId)
has "alice 投消息拿到 sessionId" "$ASID" "^s_"
eq "bob 读 alice 的会话 log → 404" "404" "$(curl -sS -o /dev/null -w '%{http_code}' $B/api/sessions/$ASID/log -H "Authorization: Bearer $BTOK")"
BF=$(curl -sS $B/api/runs/$ARID/frames -H "Authorization: Bearer $BTOK")
eq "bob 读 alice 的 run frames → 200（gone 契约，非 404）" "200" "$(curl -sS -o /dev/null -w '%{http_code}' $B/api/runs/$ARID/frames -H "Authorization: Bearer $BTOK")"
eq "bob 看到的 frames 为空" "0" "$(echo "$BF" | jq '.data.frames|length')"
eq "bob 看到的状态是 gone" "gone" "$(echo "$BF" | jq -r .data.status)"

echo "═══ 二、doc/35 项目目录 ═══"
WS=$(curl -sS -X POST $B/api/workspaces -H "Authorization: Bearer $ATOK" -H "$J" -d '{"name":"ProjA"}')
WID=$(echo "$WS" | jq -r .data.workspace.id)
eq "建项目 → code" "0" "$(echo "$WS" | jq -r .code)"
has "项目 id 是 ws_ 前缀" "$WID" "^ws_"
eq "空项目名 → 400" "400" "$(curl -sS -o /dev/null -w '%{http_code}' -X POST $B/api/workspaces -H "Authorization: Bearer $ATOK" -H "$J" -d '{"name":"  "}')"
eq "bob（跨租户）读 alice 的项目 → 404" "404" "$(curl -sS -o /dev/null -w '%{http_code}' $B/api/workspaces/$WID -H "Authorization: Bearer $BTOK")"

WR=$(curl -sS -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_w1","text":"现在几点","workspaceId":"'"$WID"'"}')
WSID=$(echo "$WR" | jq -r .data.sessionId)
has "带 workspaceId 投消息拿到 sessionId" "$WSID" "^s_"
SESS=$(curl -sS "$B/api/sessions?workspaceId=$WID" -H "Authorization: Bearer $ATOK")
has "按项目列会话含该会话" "$(echo "$SESS" | jq -c .data)" "$WSID"
UNB=$(curl -sS -X PATCH $B/api/sessions/$WSID -H "Authorization: Bearer $ATOK" -H "$J" -d '{"workspaceId":""}')
eq "解绑 → code" "0" "$(echo "$UNB" | jq -r .code)"
SESS2=$(curl -sS "$B/api/sessions?workspaceId=$WID" -H "Authorization: Bearer $ATOK")
nothas "解绑后该项目不再列它" "$(echo "$SESS2" | jq -c .data)" "$WSID"

G1=$(curl -sS -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_g1","text":"写个文件","workspaceId":"'"$WID"'"}')
G2=$(curl -sS -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_g2","text":"再来","workspaceId":"'"$WID"'"}')
eq "同项目第二场 → HTTP 409" "409" "$(curl -sS -o /dev/null -w '%{http_code}' -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_g3","text":"第三场","workspaceId":"'"$WID"'"}')"
eq "同项目第二场 → 业务码 114004" "114004" "$(echo "$G2" | jq -r .code)"

echo "═══ 三、doc/32 media_offload ═══"
MR=$(curl -sS -X POST $B/api/runs -H "Authorization: Bearer $ATOK" -H "$J" -d '{"messageId":"m_m1","text":"这张图里有什么","images":[{"mime":"image/png","dataBase64":"'"$IMG"'","name":"t.png"}]}')
MSID=$(echo "$MR" | jq -r .data.sessionId)
has "带图投消息拿到 sessionId" "$MSID" "^s_"
sleep 8
LOG=$(curl -sS "$B/api/sessions/$MSID/log?runlogOffset=0" -H "Authorization: Bearer $ATOK")
UT=$(echo "$LOG" | jq -r '.data.rows[0].text')
has "user 行是占位（带 path=）" "$UT" "path="
has "占位路径锚在 workspaces/" "$UT" "workspaces/.*\.media/"
nothas "runlog 零 base64" "$(echo "$LOG" | jq -c .data)" "base64"
MPATH=$(echo "$UT" | grep -oE 'workspaces/[^ ]*\.media/[^ ]+' | head -1)
eq "GET /api/media 取到字节" "200" "$(curl -sS -o /dev/null -w '%{http_code}' "$B/api/media?path=$MPATH" -H "Authorization: Bearer $ATOK")"
eq "取回的字节与原图同长" "$(printf '%s' "$IMG" | base64 -d | wc -c | tr -d ' ')" "$(curl -sS "$B/api/media?path=$MPATH" -H "Authorization: Bearer $ATOK" | wc -c | tr -d ' ')"
eq "跨租户取 alice 的图 → 404" "404" "$(curl -sS -o /dev/null -w '%{http_code}' "$B/api/media?path=$MPATH" -H "Authorization: Bearer $BTOK")"
DEFL=$(curl -sS $B/api/workspaces -H "Authorization: Bearer $ATOK")
DEFID=$(echo "$DEFL" | jq -r '.data.workspaces[]|select(.name=="默认")|.id')
if [ -n "$DEFID" ] && [ "$DEFID" != "null" ]; then
  DEFR=$(curl -sS -X DELETE $B/api/workspaces/$DEFID -H "Authorization: Bearer $ATOK")
  eq "删默认工作区 → 业务码 114005" "114005" "$(echo "$DEFR" | jq -r .code)"
  eq "删默认工作区 → HTTP 409" "409" "$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE $B/api/workspaces/$DEFID -H "Authorization: Bearer $ATOK")"
else
  echo "  ⚠️ 没找到默认工作区，跳过 114005 断言"
fi

echo
echo "═══ 结果：PASS $P / FAIL $F ═══"
[ "$F" -eq 0 ] || exit 1
