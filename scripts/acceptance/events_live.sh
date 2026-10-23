#!/usr/bin/env bash
# 变更事件流（doc/46 §3.3，用例 9）的活体验收：curl -N 挂读 /api/events，真发起改名/删除，
# 断言 SSE 帧字节（id: 行 + data: 行的 type/id 字段）。
#
# 这条是**长连接读取**，进不了 expected.json 的短连接断言器（那里只登记 declared_not_asserted），
# 由本脚本负责判红——任何一条不过，退出码非零。机器真源的原则不变：散文（19v6-04 用例 9）、
# 清单（expected.json）、断言器（本脚本 + contract_test.go 的字节快闸）三处同一份契约。
#
# 断言五条：
#   1. 挂读后改名 → 收到 workspace_renamed，id 精确
#   2. 新连接先静默（$ 语义：不回放历史）
#   3. 新连接收到**之后**的 workspace_deleted
#   4. Last-Event-ID 续传：收到新事件、不回放旧事件
#   5. ?access_token= 挂读（EventSource 姿势，浏览器带不了 Authorization 头）
#
# 参数（环境变量，不写死本机路径与端口）：
#   TAKO_BASE   引擎地址，默认 http://127.0.0.1:8901（开发实例；换隔离实例照着 README 起）
#   TAKO_ENV    取测试账号的 .env，默认 <仓库根>/yk-tako-eino/.env
#
# 用法：bash scripts/acceptance/events_live.sh
set -uo pipefail

B="${TAKO_BASE:-http://127.0.0.1:8901}"
ENV="${TAKO_ENV:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/yk-tako-eino/.env}"

python3 - "$B" "$ENV" <<'PY'
import json, re, subprocess, sys, time, urllib.request, urllib.error
from pathlib import Path

BASE, ENV = sys.argv[1], Path(sys.argv[2])
def gv(k):
    m = re.search(rf'^{k}="?([^"\n]+)"?', ENV.read_text(), re.M)
    return m.group(1).strip() if m else ""

def call(method, path, body=None, tok=None):
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if tok: req.add_header("Authorization", "Bearer " + tok)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data, timeout=15) as r: return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e: return e.code, json.loads(e.read())

c, b = call("POST", "/api/auth/login", {"email": gv("TAKO_DEV_TEST_EMAIL"), "password": gv("TAKO_DEV_TEST_PASSWORD")})
if b.get("code") != 0:
    print(f"  ❌ 登录失败 {b}"); sys.exit(1)
TOK = b["data"]["accessToken"]

results = []
def check(name, cond, detail=""):
    results.append(bool(cond))
    print(("  ✅ " if cond else "  ❌ ") + name + ("" if cond else f" —— {detail[:160]}"))

def stream(extra_args, path="/api/events"):
    f = open(f"/tmp/tako-events-{time.time_ns()}.txt", "w")
    p = subprocess.Popen(["curl", "-sN", BASE + path] + extra_args, stdout=f, stderr=subprocess.DEVNULL)
    return p, f

def text(f):
    f.flush()
    try:
        with open(f.name) as r: return r.read()
    except OSError:
        return ""

c, b = call("POST", "/api/workspaces", {"name": "事件流验收"}, TOK)
WID = b["data"]["workspace"]["id"]
c, b = call("GET", "/api/sessions", tok=TOK)
SID = b["data"]["sessions"][0]["id"]

# 1) 挂读 + 改名
p1, f1 = stream(["-H", "Authorization: Bearer " + TOK])
time.sleep(1)
c, b = call("PATCH", f"/api/workspaces/{WID}", {"name": "事件流验收-改"}, TOK)
time.sleep(1.5)
t1 = text(f1)
check("挂读收到 workspace_renamed 且 id 精确", '"type":"workspace_renamed"' in t1 and f'"id":"{WID}"' in t1, t1)
m = re.findall(r"^id: (\S+)", t1, re.M)
SEQ = m[-1] if m else ""
p1.terminate()

# 2)+3) 新连接不回放、收新事件
p2, f2 = stream(["-H", "Authorization: Bearer " + TOK])
time.sleep(2)
t2a = text(f2)
c, b = call("DELETE", f"/api/workspaces/{WID}", tok=TOK)
time.sleep(1.5)
t2b = text(f2)
check("新连接先静默（$ 语义，不回放历史）", t2a == "", repr(t2a[:120]))
check("新连接收到之后的 workspace_deleted", '"type":"workspace_deleted"' in t2b and f'"id":"{WID}"' in t2b, t2b)
p2.terminate()

# 4) Last-Event-ID 续传
if SEQ:
    p3, f3 = stream(["-H", "Authorization: Bearer " + TOK, "-H", f"Last-Event-ID: {SEQ}"])
    time.sleep(1)
    c, b = call("DELETE", f"/api/sessions/{SID}", tok=TOK)
    time.sleep(1.5)
    t3 = text(f3)
    check("续传收到新 session_deleted 且 id 精确", '"type":"session_deleted"' in t3 and f'"id":"{SID}"' in t3, t3)
    check("续传不回放旧事件", "workspace_renamed" not in t3, t3)
    p3.terminate()
else:
    check("续传收到新 session_deleted 且 id 精确", False, "前一步没拿到 seq，无法续传")
    results.append(True)  # 不回放一条不缺位计数

# 5) query token（EventSource 姿势）
p4, f4 = stream([])
p4b = subprocess.Popen(["curl", "-sN", f"{BASE}/api/events?access_token={TOK}"], stdout=f4, stderr=subprocess.DEVNULL)
p4.terminate()
time.sleep(1)
c, b = call("POST", "/api/workspaces", {"name": "事件流验收2"}, TOK)
WID2 = b["data"]["workspace"]["id"]
c, b = call("PATCH", f"/api/workspaces/{WID2}", {"name": "事件流验收2-改"}, TOK)
time.sleep(1.5)
t4 = text(f4)
check("query token 挂读收到事件", '"type":"workspace_renamed"' in t4, t4)
p4b.terminate()
call("DELETE", f"/api/workspaces/{WID2}", tok=TOK)

print(f"用例 9 事件流：{sum(results)}/{len(results)} 过")
sys.exit(0 if all(results) else 1)
PY
