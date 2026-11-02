#!/usr/bin/env bash
# Linux bash 沙箱（bubblewrap）的活体验收。
#
# 为什么要有它：Linux 上没有 sandbox-exec，2026-11-03 之前 bash 是 fail closed 的——
# 客户传 PDF 问「主要讲什么」，模型连 pdftotext 都跑不了。这次补了 bwrap 后端，
# 但「代码里调了 bwrap」不等于「隔离真的成立」：挂载表写错一项，就是读得到宿主 .env
# 或者写得到兄弟租户。所以必须在 Debian 机器上真打五条：
#   1. 沙箱里能干活（pwd / echo）
#   2. 越界读被挡（../.env、/etc/shadow、~/.ssh）
#   3. 越界写被挡（宿主 HOME 下留不下文件）
#   4. 出站网络通（D37 与 macOS 对齐；--unshare-all 会误卸 net）
#   5. 超时帽生效且不留孤儿进程
#
# 参数（环境变量，不写死本机端口与路径）：
#   TAKO_BASE         引擎地址，默认 http://127.0.0.1:8943
#   TAKO_ENGINE_LOG   引擎日志文件（可选）。给了就断言启动行 `bash sandbox: bwrap`
#
# 前提：靶子引擎必须带 -dev=true（dev bash 端点只在 dev 模式注册），并且是**隔离实例**
#（别拿开发库当靶子：本脚本会注册一个一次性账号）。起法见同目录 README.md。
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8943}"
LOG="${TAKO_ENGINE_LOG:-}"
J="Content-Type: application/json"

P=0; F=0
ok(){ echo "  ✅ $1"; P=$((P+1)); }
no(){ echo "  ❌ $1"; F=$((F+1)); }
has(){ case "$1" in *"$2"*) ok "$3" ;; *) no "$3 —— 输出里没有「$2」：$1" ;; esac; }
hasnt(){ case "$1" in *"$2"*) no "$3 —— 输出里出现了「$2」：$1" ;; *) ok "$3" ;; esac; }

command -v jq >/dev/null || { echo "需要 jq" >&2; exit 2; }

echo "引擎 $B${LOG:+ · 日志 $LOG}"

# ── 0. 启动行：一眼看见后端 ───────────────────────────────────────────────
if [ -n "$LOG" ]; then
  if grep -q 'bash sandbox: bwrap' "$LOG"; then ok "启动日志：bash sandbox: bwrap"; else
    no "启动日志里没有 bash sandbox: bwrap（实际：$(grep -o 'bash sandbox: .*' "$LOG" | tail -1)）"
  fi
else
  echo "  · 没给 TAKO_ENGINE_LOG，跳过启动行断言（响应里的 sandboxed 仍会检查）"
fi

# ── 登录：一次性账号，不碰开发库里的存量账号 ───────────────────────────────
EM="sandbox_$(date +%s)_$RANDOM@tako.test"
SIGNUP=$(curl -sS -X POST "$B/api/auth/signup" -H "$J" \
  -d '{"email":"'"$EM"'","password":"changeme123"}')
TOK=$(printf '%s' "$SIGNUP" | jq -r '.data.accessToken // empty')
if [ -z "$TOK" ]; then echo "注册失败：$SIGNUP" >&2; exit 2; fi

post_bash(){ # post_bash <命令> → 打印 data.output；同时校验 code/sandboxed
  local cmd="$1" body
  body=$(jq -n --arg c "$cmd" '{cmd:$c}' | curl -sS -X POST "$B/api/dev/workspace/bash" \
    -H "Authorization: Bearer $TOK" -H "$J" -d @-)
  local code sb
  code=$(printf '%s' "$body" | jq -r '.code')
  sb=$(printf '%s' "$body" | jq -r '.data.sandboxed // false')
  if [ "$code" != "0" ]; then
    no "bash 调用失败（code=$code）：$body"
    printf '\n'
    return
  fi
  if [ "$sb" != "true" ]; then no "sandboxed 不是 true：$body"; else ok "sandboxed=true"; fi
  printf '%s' "$body" | jq -r '.data.output'
}

# ── 1. 能干活 ────────────────────────────────────────────────────────────
OUT=$(post_bash 'pwd; echo BWRAP-OK')
has "$OUT" "BWRAP-OK" "沙箱内能执行命令"
has "$OUT" "/sessions/" "工作目录是会话根"

# ── 2. 越界读：密钥、影子文件、私钥都读不到 ─────────────────────────────────
OUT=$(post_bash 'cat ../.env 2>&1; cat ../../../../.env 2>&1; cat /etc/shadow 2>&1; cat ~/.ssh/id_rsa 2>&1; echo ESC-DONE')
has "$OUT" "ESC-DONE" "越界读命令跑完了（不是整个 bash 起不来）"
hasnt "$OUT" "DEEPSEEK" "读不到引擎 .env"
hasnt "$OUT" "sk-" "读不到任何 API key"
hasnt "$OUT" "root:" "读不到 /etc/shadow"
hasnt "$OUT" "PRIVATE KEY" "读不到 ~/.ssh 私钥"

# ── 3. 越界写：宿主 HOME 下留不下文件 ────────────────────────────────────────
OUTSIDE="$HOME/tako-linux-sandbox-outside.txt"
rm -f "$OUTSIDE"
OUT=$(post_bash "echo bad > '$OUTSIDE'; echo OUT-DONE")
has "$OUT" "OUT-DONE" "越界写命令跑完了"
if [ -e "$OUTSIDE" ]; then no "宿主 HOME 下被写出了文件：$OUTSIDE"; rm -f "$OUTSIDE"; else ok "宿主 HOME 下没有留下文件"; fi

# ── 4. 出站网络：D37 与 macOS 对齐 ─────────────────────────────────────────
OUT=$(post_bash "curl -sS -m 8 -o /dev/null -w 'NET-HTTP=%{http_code}' https://example.com; echo")
has "$OUT" "NET-HTTP=200" "沙箱内出站网络通（与 macOS D37 同语义）"

# ── 5. 超时帽 + 孤儿进程 ──────────────────────────────────────────────────
OUT=$(post_bash 'sleep 10; echo NEVER')
has "$OUT" "bash 超时" "超时帽生效"
hasnt "$OUT" "NEVER" "超时后没有继续跑完"
sleep 1
if pgrep -af 'sleep 10' >/dev/null 2>&1; then no "超时后还有孤儿进程：$(pgrep -af 'sleep 10')"; else ok "超时后没有孤儿进程"; fi

echo
echo "结果：通过 $P · 失败 $F"
[ "$F" -eq 0 ]
