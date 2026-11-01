#!/usr/bin/env python3
"""tako 臂：跑一题，吐一行 JSON（键集 = scripts/bench/row.schema.json）。

只打公开 HTTP 面，不动 internal/runengine/。底座复用 scripts/acceptance/harness.py。
判定段（pass/reason）本臂写 null——判定归 judge.py，合并归 runner。
"""
import argparse
import json
import os
import sys
import time

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, os.path.join(REPO, "scripts", "acceptance"))
import harness  # noqa: E402

TERMINAL = ("complete", "error", "stopped", "failed")
PREFIXES = ("答案：", "答案是", "最终答案：", "最终答案", "FINAL ANSWER:", "FINAL ANSWER")
CASES = os.path.join(REPO, "scripts", "bench", "cases")


def signup():
    """建账号 + 租户 + 配模型密钥（沿用 assert_contract 的套路）。"""
    email = "bench_%d@tako.test" % time.time_ns()
    code, body = harness.call("POST", "/api/auth/signup",
                              {"email": email, "password": "changeme123"})
    d = body["data"]
    harness.call("PUT", "/api/tenants/%s/runtime-config" % d["tenant"]["id"],
                 {"modelRef": harness.MODEL, "mcpServers": {},
                  "secrets": {"DEEPSEEK_API_KEY": harness.api_key()},
                  "sandbox": {"network": False}}, d["accessToken"])
    return {"accessToken": d["accessToken"], "refreshToken": d.get("refreshToken", ""),
            "expiresIn": d.get("expiresIn", 900), "tenant": d["tenant"]["id"],
            "email": email, "issuedAt": time.time()}


def refresh(auth):
    """换新的 access token。

    access token 15 分钟就过期（`expiresIn` 900）；25 题串行跑要十几分钟，
    中途不换 token 就一路 401——跑出来的「超时」是假的。
    """
    code, body = harness.call("POST", "/api/auth/refresh",
                              {"refreshToken": auth["refreshToken"]})
    d = (body or {}).get("data") or {}
    if not d.get("accessToken"):
        return None
    auth["accessToken"] = d["accessToken"]
    auth["refreshToken"] = d.get("refreshToken", auth["refreshToken"])
    auth["expiresIn"] = d.get("expiresIn", 900)
    auth["issuedAt"] = time.time()
    return auth["accessToken"]


def ensure_token(auth):
    """快过期就换一个。auth 是 dict 就换；是裸 token（--token 复用）就原样返回。"""
    if not isinstance(auth, dict):
        return auth
    if not auth.get("refreshToken"):
        return auth.get("accessToken")
    if time.time() - auth.get("issuedAt", 0) < auth.get("expiresIn", 900) - 60:
        return auth["accessToken"]
    return refresh(auth) or auth["accessToken"]


def acall(auth, method, path, body=None):
    """打引擎，遇 401 换一次 token 重试。

    单题最长 300 秒，跑的过程中 token 也可能到点——轮询时 401 要能自愈，
    否则整题被记成假的「超时」。
    """
    token = ensure_token(auth)
    code, payload = harness.call(method, path, body, token)
    if code == 401 and isinstance(auth, dict) and auth.get("refreshToken"):
        new = refresh(auth)
        if new:
            code, payload = harness.call(method, path, body, new)
    return code, payload


def data_of(body):
    """取 data 段；body 可能不是 dict（错误体是字符串）。"""
    if not isinstance(body, dict):
        return {}
    return body.get("data") or {}


def extract(text):
    """答案抽取：末个非空行 → 去常见答句前缀。口径见 doc/56 可比性协议。"""
    lines = [l.strip() for l in (text or "").splitlines() if l.strip()]
    if not lines:
        return ""
    ans = lines[-1]
    for p in PREFIXES:
        if ans.lower().startswith(p.lower()):
            return ans[len(p):].strip()
    return ans


def pull_frames(run_id, auth):
    """跟 nextOffset 拉全帧。

    eventOffset 是**排他**游标（eventOffset=N 返回 offset>N 的帧），第一页用 0；
    nextOffset = 本页最后一帧的 offset。终止信号取 terminal 帧——越过末帧再问会返回
    status=generating，不能拿它当结束。
    """
    out, offset, empty = [], 0, 0
    while True:
        code, body = acall(auth, "GET", "/api/runs/%s/frames?eventOffset=%d" % (run_id, offset))
        data = data_of(body)
        fs = data.get("frames") or []
        out.extend(fs)
        if fs:
            offset, empty = data.get("nextOffset") or offset, 0
            if any(f.get("kind") == "terminal" for f in fs):
                return out
            continue
        empty += 1
        if empty >= 5:
            return out
        time.sleep(1)


def run_one(auth, question, task_id, timeout):
    """跑一题，返回一行（执行段）。"""
    code, body = acall(auth, "POST", "/api/workspaces",
                       {"name": "bench-" + (task_id or "adhoc")[:12]})
    ws = data_of(body).get("workspace")
    if not ws:
        # 建项目就失败（token 过期、并发闸、名字冲突…）：记成一行失败，别让整场崩掉。
        return {"taskId": task_id, "arm": "tako", "runId": "", "sessionId": "",
                "workspaceId": "", "status": "error", "answer": "",
                "modelCalls": 0, "toolCalls": 0, "promptTokens": 0, "completionTokens": 0,
                "totalTokens": 0, "wallSec": 0.0, "tools": [], "pass": None, "reason": None}
    wid = ws["id"]
    t0 = time.time()
    code, body = acall(auth, "POST", "/api/runs",
                       {"text": question, "workspaceId": wid})
    run_data = data_of(body)
    if not run_data.get("runId"):
        return {"taskId": task_id, "arm": "tako", "runId": "", "sessionId": "",
                "workspaceId": wid, "status": "error", "answer": "",
                "modelCalls": 0, "toolCalls": 0, "promptTokens": 0, "completionTokens": 0,
                "totalTokens": 0, "wallSec": round(time.time() - t0, 1), "tools": [],
                "pass": None, "reason": None}
    run_id, session_id = run_data["runId"], run_data["sessionId"]
    status = ""
    while time.time() - t0 < timeout:
        code, body = acall(auth, "GET", "/api/runs/%s" % run_id)
        status = data_of(body).get("status") or ""
        if status in TERMINAL:
            break
        time.sleep(2)
    if status not in TERMINAL:
        status = "timeout"
        acall(auth, "POST", "/api/runs/%s/stop" % run_id, {})
    frames = pull_frames(run_id, auth)
    text = "".join(f.get("text") or "" for f in frames if f.get("kind") == "delta")
    usage = [f for f in frames if f.get("kind") == "usage"]
    tools = [f.get("name") for f in frames if f.get("kind") == "tool_call"]
    summed = lambda k: sum((u.get(k) or 0) for u in usage)  # noqa: E731
    return {"taskId": task_id, "arm": "tako", "runId": run_id, "sessionId": session_id,
            "workspaceId": wid, "status": status, "answer": extract(text),
            "modelCalls": len(usage), "toolCalls": len(tools),
            "promptTokens": summed("promptTokens"),
            "completionTokens": summed("completionTokens"),
            "totalTokens": summed("totalTokens"),
            "wallSec": round(time.time() - t0, 1), "tools": tools,
            "pass": None, "reason": None}


def load_case(task_id):
    """读题面文件。切片配方文件里没有题面——那类要传 question。"""
    path = os.path.join(CASES, "%s.json" % task_id)
    if not os.path.exists(path):
        return None, "找不到题面文件 %s" % path
    data = json.load(open(path))
    if "question" not in data:
        return None, "%s 是切片配方，没有题面；用 --question 传" % path
    return data, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--task", help="题 id（题面取自 scripts/bench/cases/<id>.json）")
    ap.add_argument("--question", help="直接给题面（优先于 --task 查文件）")
    ap.add_argument("--token", help="复用已有 accessToken；不给就自己注册")
    ap.add_argument("--timeout", type=float, default=300)
    a = ap.parse_args()
    if a.question:
        question = a.question
    else:
        if not a.task:
            ap.error("要 --task 或 --question")
        case, err = load_case(a.task)
        if err:
            ap.error(err)
        question = case["question"]
    token = a.token
    if not token:
        auth = signup()
        token = auth["accessToken"]
        print("tenant: %s %s" % (auth["tenant"], auth["email"]), file=sys.stderr)
    print(json.dumps(run_one(token, question, a.task, a.timeout), ensure_ascii=False))


if __name__ == "__main__":
    main()
