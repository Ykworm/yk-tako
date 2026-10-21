#!/usr/bin/env python3
"""19v6-04 用例矩阵实跑：逐字节打印真实响应，供人肉对齐期望字节。

这是**对齐工具**（打印），不是断言器。断言器是同目录的 assert_contract.py：
它读 expected.json 逐条比对、判红绿、失败非零退出。

参数（全走环境变量，不再写死本机路径与端口）：

  TAKO_BASE    引擎地址，默认 http://127.0.0.1:8943
  TAKO_ENV    取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
  TAKO_MODEL  租户里配的模型 ref，默认 deepseek/deepseek-flash

跑之前先起一个隔离引擎（别拿开发库当靶子），见同目录 README.md。
"""
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

B = os.environ.get("TAKO_BASE", "http://127.0.0.1:8943").rstrip("/")
ENV = Path(os.environ.get("TAKO_ENV", Path(__file__).resolve().parents[2] / "yk-tako-eino" / ".env"))
MODEL = os.environ.get("TAKO_MODEL", "deepseek/deepseek-flash")
STAMP = f"{int(time.time())}{os.getpid()}"


def read_key():
    """从 .env 里取模型密钥。取不到就早点退出——跑一半才炸最浪费时间。"""
    try:
        text = ENV.read_text()
    except OSError:
        print(f"读不到 .env：{ENV}（用 TAKO_ENV 指定）", file=sys.stderr)
        sys.exit(2)
    m = re.search(r'^DEEPSEEK_API_KEY="?([^"\n]+)"?', text, re.M)
    if not m or not m.group(1).strip():
        print(f"{ENV} 里没有 DEEPSEEK_API_KEY", file=sys.stderr)
        sys.exit(2)
    return m.group(1).strip()


def call(method, path, body=None, token=None, raw=False):
    req = urllib.request.Request(B + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            code, payload = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, payload = e.code, e.read()
    if raw:
        return code, payload
    try:
        return code, json.loads(payload)
    except Exception:
        return code, payload.decode("utf-8", "replace")


def show(label, code, payload):
    if isinstance(payload, (dict, list)):
        payload = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    print(f"{label} HTTP {code} → {payload}")


KEY = read_key()

print(f"引擎 {B} · 密钥取自 {ENV} · 模型 {MODEL}")
print()
print("──── 认证前置（doc/28 起）────")
c, sign = call("POST", "/api/auth/signup",
               {"email": f"v6c4_{STAMP}@tako.test", "password": "changeme123"})
show("signup", c, {"code": sign["code"], "message": sign["message"],
                   "data": {k: sign["data"][k] for k in ("user", "tenant", "membership", "tokenType", "expiresIn")}})
TOK = sign["data"]["accessToken"]
TEN = sign["data"]["tenant"]["id"]
c, _ = call("PUT", f"/api/tenants/{TEN}/runtime-config",
            {"modelRef": MODEL, "mcpServers": {},
             "secrets": {"DEEPSEEK_API_KEY": KEY}, "sandbox": {"network": False}}, TOK)
print(f"租户模型配置 PUT HTTP {c}")

print()
print("──── 无 Authorization（四个端点）────")
for m, p, b in [("POST", "/api/runs", {"messageId": "m_x", "text": "hi"}),
                ("GET", "/api/sessions/s_nope", None),
                ("GET", "/api/runs/r_nope/frames", None),
                ("GET", "/api/sessions/s_nope/log", None)]:
    c, p2 = call(m, p, b)
    show(f"{m} {p}", c, p2)

print()
print("──── 用例 1：投递 → 轮询 → 定稿 ────")
c, p1 = call("POST", "/api/runs", {"messageId": "m_1", "text": "hi"}, TOK)
show("POST /api/runs", c, p1)
SID, RID = p1["data"]["sessionId"], p1["data"]["runId"]
c, sess = call("GET", f"/api/sessions/{SID}", token=TOK)
show("GET /api/sessions（跑中）", c, sess)
frames = None
for _ in range(90):
    c, frames = call("GET", f"/api/runs/{RID}/frames?eventOffset=0", token=TOK)
    if any(f["kind"] == "terminal" for f in frames.get("frames", [])):
        break
    time.sleep(1)
show("GET /frames（定稿后）", c, frames)
c, sess = call("GET", f"/api/sessions/{SID}", token=TOK)
show("GET /api/sessions（定稿后）", c, sess)
c, log = call("GET", f"/api/sessions/{SID}/log?runlogOffset=0", token=TOK)
show("GET /log", c, {"nextOffset": log.get("nextOffset"),
                     "rows": [{k: r.get(k) for k in ("runlogOffset", "role", "agent", "model", "text")} for r in log.get("rows", [])]})

print()
print("──── 用例 2：游标增量 & 越界 ────")
c, p2 = call("GET", f"/api/sessions/{SID}/log?runlogOffset=1", token=TOK)
show("log?runlogOffset=1", c, {"nextOffset": p2.get("nextOffset"),
                               "rows": [{"runlogOffset": r.get("runlogOffset"), "role": r.get("role")} for r in p2.get("rows", [])]})
c, p2 = call("GET", f"/api/sessions/{SID}/log?runlogOffset=999", token=TOK)
show("log?runlogOffset=999", c, p2)
c, p2 = call("GET", f"/api/runs/{RID}/frames?eventOffset=999", token=TOK)
show("frames?eventOffset=999", c, p2)

print()
print("──── 用例 3：未知 sessionId / 未知 run ────")
c, p2 = call("POST", "/api/runs", {"sessionId": "s_nope", "messageId": "m_9", "text": "hi"}, TOK)
show("POST 带未知 sessionId", c, p2)
c, p2 = call("GET", "/api/sessions/s_nope/log", token=TOK)
show("GET 未知 session log", c, p2)
c, p2 = call("GET", "/api/sessions/s_nope", token=TOK)
show("GET 未知 session", c, p2)
c, p2 = call("GET", "/api/runs/r_nope/frames", token=TOK)
show("GET 未知 run frames", c, p2)

print()
print("──── 用例 4：忙时诚实拒绝（用一场还在跑的 run）────")
print("  ⚠️ 这一条靠时序：投完立刻再投，模型够慢才撞得到闸。")
print("     要确定性断言（把 run 拨成 running + 活租约），跑 assert_contract.py 的 4-busy-rejected。")
c, p3 = call("POST", "/api/runs", {"messageId": "m_busy", "text": "现在几点，请多等一会儿"}, TOK)
print(f"起一场 run → HTTP {c}  session={(p3.get('data') or {}).get('sessionId')}")
BSID = (p3.get("data") or {}).get("sessionId")
c, p4 = call("POST", "/api/runs", {"sessionId": BSID, "messageId": "m_b2", "text": "急"}, TOK)
show("同会话再投（忙）", c, p4)
c, p5 = call("GET", f"/api/sessions/{BSID}/log?runlogOffset=0", token=TOK)
print(f"忙拒后 runlog 行数 = {len(p5.get('rows', []))}（忙拒不该留下 m_b2 的痕迹）")
