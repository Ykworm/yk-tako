#!/usr/bin/env python3
"""dsh 臂：跑一题，吐一行 JSON（键集 = scripts/bench/row.schema.json）。

走 dsh 的 headless profile：`dsh --profile headless --json "<题面>"`——一题一个新会话，
最终答案在 stdout 的 `final` 事件里，事件流还带 status / tool_call / usage。

口径对齐 doc/56 §4.7：同一切片、同模型（deepseek-flash）、同搜索后端（DeepSeek）、
同 user message、同判定器。**只有 harness 不同**——那才是要量的东西。

与 tako 臂的预算口径差异（§4.7.5：并列不折算）：
  - tako 的 usage 帧只给 promptTokens 总量、不分缓存命中；dsh 的 usage **分** inputTokens
    （未命中）/ cacheReadTokens / cacheWriteTokens，三者相加才是计费输入。
  - dsh 的 `modelCalls` = step_start 事件数（一次模型往返 = 一步），不是厂商账单口径。
"""
import argparse
import glob
import json
import os
import subprocess
import sys
import time

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, os.path.join(REPO, "scripts", "acceptance"))
import harness  # noqa: E402

DSH_VERSION = os.environ.get("DSH_VERSION", "0.2.1-rc.5")
WORK_ROOT = os.path.join(REPO, "tmp", "bench-dsh", "work")
PREFIXES = ("答案：", "答案是", "最终答案：", "最终答案", "FINAL ANSWER:", "FINAL ANSWER")


def npm_cache_dir():
    """npm 的 cache 目录（不一定是 ~/.npm——本机在 /Applications/MxSrvs/cache/nodejs）。"""
    try:
        out = subprocess.run(["npm", "config", "get", "cache"], capture_output=True,
                             text=True, timeout=30).stdout.strip()
        if out:
            return out
    except (OSError, subprocess.SubprocessError):
        pass
    return os.path.expanduser("~/.npm")


def dsh_bin():
    """找 dsh 可执行文件。

    优先用 npx 缓存里已装好的那份（免得每题都问一次 npm 仓库）；版本要对得上 DSH_VERSION。
    找不到就退回 npx 现装。
    """
    env = os.environ.get("DSH_BIN")
    if env and os.path.exists(env):
        return [env]
    cache = os.path.join(npm_cache_dir(), "_npx")
    hits = []
    for pkg in glob.glob(os.path.join(cache, "*", "node_modules", "@deepseek-ai", "dsh")):
        try:
            version = json.load(open(os.path.join(pkg, "package.json")))["version"]
        except (OSError, KeyError, json.JSONDecodeError):
            continue
        if version != DSH_VERSION:
            continue
        binp = os.path.join(os.path.dirname(os.path.dirname(pkg)), ".bin", "dsh")
        if os.path.exists(binp):
            hits.append(binp)
    if hits:
        return [sorted(hits, key=os.path.getmtime, reverse=True)[0]]
    return ["npx", "--yes", "@deepseek-ai/dsh@%s" % DSH_VERSION]


def extract(text):
    """答案抽取：末个非空行 → 去常见答句前缀。口径与 tako 臂同一套（doc/56 §4.7.3）。"""
    lines = [l.strip() for l in (text or "").splitlines() if l.strip()]
    if not lines:
        return ""
    ans = lines[-1]
    for p in PREFIXES:
        if ans.lower().startswith(p.lower()):
            return ans[len(p):].strip()
    return ans


def run_one(question, task_id, timeout):
    """跑一题，返回一行（执行段）。判定段（pass/reason）留给 judge.py。"""
    ws = os.path.join(WORK_ROOT, (task_id or "adhoc")[:16])
    os.makedirs(ws, exist_ok=True)
    env = dict(os.environ)
    env["DEEPSEEK_API_KEY"] = harness.api_key()
    t0 = time.time()
    try:
        proc = subprocess.run(dsh_bin() + ["--profile", "headless", "--json", question],
                              cwd=ws, env=env, capture_output=True, text=True,
                              timeout=timeout)
        rc, stdout, stderr = proc.returncode, proc.stdout, proc.stderr
    except subprocess.TimeoutExpired as e:
        rc, stdout, stderr = 124, (e.stdout or b"").decode("utf-8", "replace"), ""
    wall = round(time.time() - t0, 1)

    final_text, tools, calls = "", [], 0
    prompt = completion = total = 0
    cache_read = cache_write = 0
    session_id = ""
    saw_final = False
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        kind = ev.get("type")
        if kind == "final":
            final_text, saw_final = ev.get("text") or "", True
        elif kind == "session":
            session_id = ev.get("sessionId") or ""
        elif kind == "tool_call":
            tools.append(ev.get("tool") or "")
        elif kind == "status" and ev.get("phase") == "step_start":
            calls += 1
        elif kind == "status" and ev.get("phase") == "step_end":
            u = ev.get("usage") or {}
            # dsh 的口径：inputTokens 是不含缓存的输入，缓存读写单列，三者相加才是计费输入。
            prompt += ((u.get("inputTokens") or 0) + (u.get("cacheReadTokens") or 0)
                       + (u.get("cacheWriteTokens") or 0))
            completion += u.get("outputTokens") or 0
            total += u.get("totalTokens") or 0
            cache_read += u.get("cacheReadTokens") or 0
            cache_write += u.get("cacheWriteTokens") or 0

    if rc == 124:
        status = "timeout"
    elif rc != 0 or not saw_final:
        status = "error"
    else:
        status = "complete"
    return {"taskId": task_id, "arm": "dsh", "runId": "", "sessionId": session_id,
            "workspaceId": ws, "status": status, "answer": extract(final_text),
            "modelCalls": calls, "toolCalls": len(tools),
            "promptTokens": prompt, "completionTokens": completion,
            "totalTokens": total, "wallSec": wall, "tools": tools,
            "pass": None, "reason": None,
            # 额外信息（runner 会剥掉，不进 row.schema.json 的 16 键）：
            "_cacheRead": cache_read, "_cacheWrite": cache_write,
            "_stderr": (stderr or "")[-400:]}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--question", help="题面")
    ap.add_argument("--task", help="题 id（只用于给工作目录起名）")
    ap.add_argument("--timeout", type=float, default=300)
    a = ap.parse_args()
    if not a.question:
        ap.error("要 --question")
    print(json.dumps(run_one(a.question, a.task or "adhoc", a.timeout), ensure_ascii=False))


if __name__ == "__main__":
    main()
