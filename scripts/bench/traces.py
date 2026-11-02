#!/usr/bin/env python3
"""把一场 run 的 trace 从 Langfuse 拉回终端：树 / 工具序列 / 模型序列 / 末个 assistant 文本。

这是 bench 归因的取数件（doc/56 D1-0）。没有它，「这道题为什么挂」只能人登平台翻页面；
D1-1 要跑 250 场 run，人眼归因做不完、也不可复现。

数据面（2026-11-01 实测）：
  GET <base>/api/public/v2/observations?filter=[{"type":"string","column":"traceId",
      "operator":"=","value":"<hex>"}]&limit=200&fields=core,basic,usage,io,metadata,model
  读接口是 v2 observations；旧 v1 资源（/api/public/traces 等）2026-11-16 下线，不要用。
  凭据读 yk-tako-eino/.env 的 LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY / LANGFUSE_BASE_URL，
  不进仓库。

两种入参：
  --trace <hex>                       直接给根 span id（排查单场）
  --report <json> --task <id>         从报告的 details[task].arms.tako.runs[].traceId 取；
                                      顺带交叉核对 span 上的 tako.run.id 与报告 runId 一致
  --report <json> --all               逐题逐轮打印（归因时用）

报告是新格式（bench/3）时一题一臂有多次运行，逐轮取 facts；旧报告（标量 traceId）也能读。

为什么走 curl 而不是 urllib：本机 python 的 certifi 没有代理 CA，直连 Langfuse 会
CERTIFICATE_VERIFY_FAILED；curl 用系统信任库。这不是「顺手」，是踩过的坑。
"""
import argparse
import json
import os
import subprocess
import sys
import time
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
ENV_PATH = os.environ.get("TAKO_ENV", os.path.join(REPO, "yk-tako-eino", ".env"))


def env_value(name, fallback=""):
    try:
        text = open(ENV_PATH, encoding="utf-8").read()
    except OSError:
        raise SystemExit("读不到 .env：%s（用 TAKO_ENV 指定）" % ENV_PATH)
    for line in text.splitlines():
        line = line.strip()
        if line.startswith(name + "="):
            return line.split("=", 1)[1].strip().strip('"').strip("'")
    return fallback


def fetch_observations(trace_id, limit=200):
    """按 traceId 拉回全部 span。返回 list；trace 不存在时返回 []。"""
    base = env_value("LANGFUSE_BASE_URL", "https://us.cloud.langfuse.com").rstrip("/")
    pk = env_value("LANGFUSE_PUBLIC_KEY")
    sk = env_value("LANGFUSE_SECRET_KEY")
    if not pk or not sk:
        raise SystemExit("缺少 LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY（看 %s）" % ENV_PATH)
    flt = json.dumps([{"type": "string", "column": "traceId", "operator": "=", "value": trace_id}],
                     separators=(",", ":"))
    cmd = ["curl", "-sS", "-G", "-u", "%s:%s" % (pk, sk),
           "--data-urlencode", "filter=" + flt,
           "--data-urlencode", "limit=%d" % limit,
           "--data-urlencode", "fields=core,basic,usage,io,metadata,model",
           "-w", "\n%{http_code}", base + "/api/public/v2/observations"]
    # 读接口限流（实测 30 次/分钟）：125 场全量拉必须退避重试，否则拉到一半 429 整批作废。
    for _ in range(8):
        r = subprocess.run(cmd, capture_output=True, text=True)
        if r.returncode != 0:
            raise SystemExit("curl 失败：%s" % r.stderr.strip())
        body, _, code = r.stdout.rpartition("\n")
        code = code.strip()
        if code == "429":
            try:
                wait = int(json.loads(body)["details"]["retryAfterSeconds"]) + 2
            except Exception:
                wait = 21
            time.sleep(wait)
            continue
        if code != "200":
            raise SystemExit("Langfuse 回 %s：%s" % (code, body[:300]))
        return json.loads(body).get("data") or []
    raise SystemExit("Langfuse 连续限流，放弃 trace %s" % trace_id)


def parse_json(value):
    if value in (None, ""):
        return None
    if isinstance(value, (dict, list)):
        return value
    try:
        return json.loads(value)
    except Exception:
        return None


def text_of_output(obs):
    """GENERATION/AGENT 的 output 是消息数组的 JSON 串（实测形状）→ 拼出文本部分。"""
    msgs = parse_json(obs.get("output"))
    if not isinstance(msgs, list):
        return ""
    out = []
    for m in msgs:
        for p in (m.get("parts") or []):
            if p.get("type") == "text" and p.get("content"):
                out.append(p["content"])
    return "\n".join(out).strip()


def usage_of(obs):
    u = obs.get("usageDetails") or {}
    return (u.get("input") or 0, u.get("output") or 0)


def attr_of(obs, name):
    """取 span 原始属性：Langfuse 把属性拍平成 metadata["attributes.<原名>"]（实测）。"""
    return (obs.get("metadata") or {}).get("attributes." + name)


def gen_fact(obs):
    """一次模型调用：token / 首字延迟 / 结束原因。

    TTFT 与 finish_reasons 在 span 属性里（`gen_ai.response.time_to_first_chunk` /
    `gen_ai.response.finish_reasons`）；Langfuse 的顶层 `timeToFirstToken` 留空，别拿它。
    """
    i, o = usage_of(obs)
    return {"name": obs.get("name"), "model": obs.get("model"),
            "input": i, "output": o, "latency": obs.get("latency"),
            "ttft": attr_of(obs, "gen_ai.response.time_to_first_chunk"),
            "finish": attr_of(obs, "gen_ai.response.finish_reasons"),
            "start": obs.get("startTime")}


def tool_call_line(obs):
    """工具序列一行：名字 + 参数 + 回执首行 + 是否失败（「错误：」前缀口径）。"""
    name = obs.get("name") or "?"
    args = obs.get("input")
    if isinstance(args, str):
        args = args.strip()
    else:
        args = json.dumps(args, ensure_ascii=False)
    out = obs.get("output")
    out = out if isinstance(out, str) else json.dumps(out, ensure_ascii=False)
    first = (out or "").strip().splitlines()[0] if (out or "").strip() else ""
    failed = first.startswith("错误：")
    return {"name": name, "args": args, "resultFirst": first, "failed": failed}


def build(obs_list):
    """排树 + 四段事实。"""
    by_id = {o["id"]: o for o in obs_list}
    children = {}
    roots = []
    for o in obs_list:
        p = o.get("parentObservationId")
        if p and p in by_id:
            children.setdefault(p, []).append(o)
        else:
            roots.append(o)
    order = lambda o: o.get("startTime") or ""
    for lst in children.values():
        lst.sort(key=order)
    roots.sort(key=order)

    span = [o for o in obs_list if o.get("type") == "SPAN"]
    gens = [o for o in obs_list if o.get("type") == "GENERATION"]
    tools = [o for o in obs_list if o.get("type") == "TOOL"]
    tin = sum(usage_of(g)[0] for g in gens)
    tout = sum(usage_of(g)[1] for g in gens)
    run_ids = set()
    for o in obs_list:
        # Langfuse 把 span 属性拍平成 metadata["attributes.<原名>"]（实测，别写成嵌套 attributes）
        v = (o.get("metadata") or {}).get("attributes.tako.run.id")
        if v:
            run_ids.add(v)
    last_text = ""
    for g in sorted(gens, key=order):
        t = text_of_output(g)
        if t:
            last_text = t
    return {"roots": roots, "children": children, "spans": len(span),
            "generations": gens, "tools": tools,
            "tokens": (tin, tout, tin + tout), "runIds": sorted(run_ids),
            "lastText": last_text}


def print_tree(info, trace_id):
    print("trace %s  （%d span：SPAN %d / GENERATION %d / TOOL %d）" % (
        trace_id, len(info["generations"]) + len(info["tools"]) + info["spans"],
        info["spans"], len(info["generations"]), len(info["tools"])))

    def walk(o, prefix, last):
        lat = o.get("latency")
        name = o.get("name") or "?"
        kind = o.get("type") or "?"
        extra = ""
        if kind == "GENERATION":
            i, out = usage_of(o)
            extra = "  in=%d out=%d" % (i, out)
        elif kind == "TOOL":
            tl = tool_call_line(o)
            extra = "  " + ("失败 " if tl["failed"] else "") + tl["args"][:60]
        print("%s%s%-38s %-10s %6.0fms%s" % (
            prefix, "└ " if last else "├ ", name[:38], kind,
            (lat or 0) * 1000, extra))
        kids = info["children"].get(o["id"], [])
        for i, k in enumerate(kids):
            walk(k, prefix + ("  " if last else "│ "), i == len(kids) - 1)

    for i, r in enumerate(info["roots"]):
        walk(r, "", i == len(info["roots"]) - 1)


def print_facts(info):
    tin, tout, total = info["tokens"]
    print("")
    print("工具 %d 次（失败 %d）：" % (len(info["tools"]), sum(
        1 for t in info["tools"] if tool_call_line(t)["failed"])))
    for i, t in enumerate(sorted(info["tools"], key=lambda o: o.get("startTime") or ""), 1):
        tl = tool_call_line(t)
        print("  %2d. %-10s 参数 %s  回执首行 %r%s" % (
            i, tl["name"], tl["args"][:80], tl["resultFirst"][:60],
            "  ← 失败" if tl["failed"] else ""))
    print("模型 %d 枪：input %d / output %d / total %d" % (
        len(info["generations"]), tin, tout, total))
    for i, g in enumerate(sorted(info["generations"], key=lambda o: o.get("startTime") or ""), 1):
        f = gen_fact(g)
        print("  %2d. %-22s in=%-6d out=%-5d TTFT=%-7s finish=%s 总耗时=%.2fs" % (
            i, (f["name"] or "?")[:22], f["input"], f["output"],
            ("%.2fs" % f["ttft"]) if isinstance(f["ttft"], (int, float)) else "—",
            f["finish"] or "—", f["latency"] or 0))
    print("末个 assistant 文本：%r" % info["lastText"])
    if info["runIds"]:
        print("span 上的 tako.run.id：%s" % ", ".join(info["runIds"]))


def run_trace(trace_id, as_json=False):
    obs = fetch_observations(trace_id)
    if not obs:
        raise SystemExit("Langfuse 里找不到 trace %s（可能没上报，或超出保留期）" % trace_id)
    info = build(obs)
    if as_json:
        print(json.dumps({
            "traceId": trace_id, "spans": info["spans"],
            "generations": len(info["generations"]),
            "models": [gen_fact(g) for g in sorted(
                info["generations"], key=lambda o: o.get("startTime") or "")],
            "tools": [tool_call_line(t) for t in info["tools"]],
            "tokens": info["tokens"], "runIds": info["runIds"],
            "lastText": info["lastText"],
        }, ensure_ascii=False, indent=2))
        return info
    print_tree(info, trace_id)
    print_facts(info)
    return info


def summary_row(task_id, trace_id, info, expect_run_id):
    tools = [tool_call_line(t) for t in info["tools"]]
    ok = (expect_run_id in info["runIds"]) if expect_run_id else True
    return {
        "taskId": task_id, "traceId": trace_id, "runId": expect_run_id, "runIdMatches": ok,
        "toolCount": len(tools), "toolFailures": sum(1 for t in tools if t["failed"]),
        "toolSeq": [t["name"] for t in tools],
        "modelCalls": len(info["generations"]),
        "tokens": info["tokens"], "lastText": info["lastText"],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--trace", default="", help="根 span id（hex）")
    ap.add_argument("--report", default="", help="报告 json（取 details[task].arms.tako.traceId）")
    ap.add_argument("--task", default="", help="报告里的 task id")
    ap.add_argument("--all", action="store_true", help="逐题打印报告里的全部 tako trace")
    ap.add_argument("--json", action="store_true", help="单 trace 以 JSON 打印（便于下游）")
    ap.add_argument("--summary-json", default="", help="把逐题事实写成 JSON（配 --report 用）")
    a = ap.parse_args()

    if a.trace:
        run_trace(a.trace, a.json)
        return 0
    if not a.report:
        ap.error("要 --trace，或 --report +（--task / --all）")

    data = json.load(open(a.report))
    details = data.get("details") or {}
    tasks = sorted(details) if a.all or not a.task else [a.task]
    facts, missing = [], []
    for tid in tasks:
        ad = (details.get(tid) or {}).get("arms", {}).get("tako") or {}
        runs = ad.get("runs")
        if runs is None:  # bench/2 的旧报告：一题一臂一次运行、traceId 是标量
            runs = [{"traceId": ad.get("traceId"), "runId": ad.get("runId")}]
        multi = len(runs) > 1
        for i, run in enumerate(runs):
            trace_id, run_id = run.get("traceId"), run.get("runId")
            label = "%s#%d" % (tid[:8], i + 1) if multi else tid
            if not trace_id:
                missing.append({"taskId": tid, "runIndex": i, "runId": run_id})
                continue
            obs = fetch_observations(trace_id)
            if not obs:
                missing.append({"taskId": tid, "runIndex": i, "runId": run_id})
                print("%s：Langfuse 里没有 trace %s" % (label, trace_id), file=sys.stderr)
                continue
            info = build(obs)
            row = summary_row(tid, trace_id, info, run_id)
            row["runIndex"] = i
            facts.append(row)
            if not a.summary_json:
                print("=" * 78)
                print("%s（runId %s，一致=%s）" % (label, run_id, row["runIdMatches"]))
                print_tree(info, trace_id)
                print_facts(info)
            elif not row["runIdMatches"]:
                print("%s：span 上的 tako.run.id 与报告 runId 不一致（串 trace）" % label,
                      file=sys.stderr)
    if a.summary_json:
        json.dump({"source": a.report, "tasks": facts, "missing": missing},
                  open(a.summary_json, "w"), ensure_ascii=False, indent=2)
        print("写出 %d 份运行事实、%d 份缺 trace → %s" % (
            len(facts), len(missing), a.summary_json))
    if a.task and missing:
        print("%s 的 trace 取不到（见上）" % a.task, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
