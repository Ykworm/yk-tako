#!/usr/bin/env python3
"""批量 runner：题集 × 臂 → 一行一题 → 报告（md + json）。

本轮装了两臂：tako（引擎 HTTP 面）与 dsh（npm @deepseek-ai/dsh，headless profile）。
两臂同切片、同模型（deepseek-flash）、同搜索后端（DeepSeek）、同 user message、同判定器，
**只差 harness**——要量的就是那点差。pi 臂未装：报告里单列「本轮未装」，**不折算成 0**
（doc/56 §6.5 的配对口径：缺臂的题不进通过率分母）。

用法：
  python3 scripts/bench/run_bench.py --cases gaia --arms tako,dsh
  python3 scripts/bench/run_bench.py --cases gaia --arms tako --limit 1
  python3 scripts/bench/run_bench.py --cases smoke --arms tako,dsh
  python3 scripts/bench/run_bench.py --cases smoke --arms tako --repeat 5 --write-baseline
  python3 scripts/bench/run_bench.py --cases gaia --arms tako --repeat 5 \
      --baseline scripts/bench/baselines/gaia.json --traces tmp/trace-facts.json

跑同一批题 N 次时，报告给两列（doc/45）：Pass@N = N 次里至少过一次，
pass^N = N 次全过。归因（engine_regression / model_below_baseline / …）要先有基线，
基线里只有「题 id 的短哈希 → 判定」，没有题面与答案；题面明文始终只在 tmp/。
退出码：出现 engine_regression 或 model_below_baseline 时非零——分数掉了要能挡住流水线。

题集：
  gaia   —— 明文切片在 tmp/56-可比性对照/gaia_l1_25.json（不入库）；跑之前先按入库配方
            重算 slice_hash 对不上就不跑。缺文件就先 `python3 scripts/bench/slice.py --emit <path>`。
  smoke  —— scripts/bench/cases/smoke-00*.json（自造题，三支规则各一道）。

报告落 tmp/bench-reports/<ts>/（运行产物不入库，AGENTS.md）。口径字段全套见 protocol 段。
"""
import argparse
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(HERE, "arms"))
import judge as judge_mod  # noqa: E402
import tako as tako_arm  # noqa: E402
import dsh as dsh_arm  # noqa: E402

GAIA_SLICE = os.path.join(REPO, "tmp", "56-可比性对照", "gaia_l1_25.json")
GAIA_RECIPE = os.path.join(HERE, "cases", "gaia_l1_25.json")
REPORT_ROOT = os.path.join(REPO, "tmp", "bench-reports")
SCHEMA_KEYS = json.load(open(os.path.join(HERE, "row.schema.json")))["keys"]
ARMS = {  # 本轮装了什么：没装的臂在报告里单列，不折算成 0
    "tako": {"installed": True, "entry": "scripts/bench/arms/tako.py"},
    "dsh": {"installed": True, "entry": "scripts/bench/arms/dsh.py"},
    "pi": {"installed": False, "note": "本轮未装（ref-agent/pi 无 node_modules）"},
}
# 两臂的 $ 为什么不能直接比（§4.7.5：预算单位不同，只并列不折算）。
BUDGET_COMPARABILITY = {
    "note": "两臂的 $ 不可直接比：dsh 的 usage 分缓存命中（命中按 hit 价），"
            "tako 的 usage 帧只给 promptTokens 总量、不分命中（一律按 miss 价，偏贵的上界）。",
    "comparableColumn": "usdUpperBound —— 两臂一律按缓存未命中价算，是同一条尺子；"
                        "usd 是各臂本来的账（tako 为上界）。",
}
BASELINE_DIR = os.path.join(HERE, "baselines")
PASSK_SEMANTICS = ("Pass@N = N 次里至少过一次；pass^N = N 次全过（doc/45 口径）。"
                   "N=1 时两者相同。归因只在 D1-1 有基线时给结论，否则记 unasserted——"
                   "没有基线就说「掉分」是猜。")


def baseline_path(case_set):
    return os.path.join(BASELINE_DIR, "%s.json" % case_set)


def load_baseline(path):
    if not path or not os.path.exists(path):
        return None
    return json.load(open(path))


def load_trace_facts(path):
    """traces.py --summary-json 的产物原样读进来（要进报告，保持可序列化）。"""
    if not path or not os.path.exists(path):
        return None
    return json.load(open(path))


def trace_index(raw):
    """事实表 → 两个键的索引：(taskId, runId) 与 (taskId, 轮次)。

    repeat>1 时一题有多次运行，取证要对上「哪一轮」；runId 优先，轮次兜底。
    """
    out = {}
    for t in (raw or {}).get("tasks") or []:
        if t.get("runId"):
            out[(t["taskId"], t["runId"])] = t
        out[(t["taskId"], t.get("runIndex"))] = t
    return out


def write_json(path, obj):
    """先写临时文件再换名：报告是花钱跑出来的，中途崩不许留半截。"""
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def arm_runs(det, arm):
    """取某题某臂的逐次结果。兼容旧报告（标量字段）与新报告（runs 列表）。"""
    ad = (det.get("arms") or {}).get(arm) or {}
    if ad.get("runs") is not None:
        return ad["runs"]
    return [ad] if ad else []


def attribute(arm, task_id, runs, base, traces):
    """归因五态：本轮判定 + 基线 + trace 取证。

    归因不是「猜原因」，是可核对的判定：
      incomplete          —— 有任一轮没跑完（超时/error），先说没跑完
      model_baseline      —— 基线该题就挂，本轮也挂：模型就这水平
      engine_regression   —— 基线过、本轮挂，且 trace 里工具有「错误：」或 trace 缺失/串号
      model_below_baseline—— 基线过、本轮挂，trace 正常（工具没报错、span 齐）：模型本轮没做对
      unasserted          —— 没基线或缺 trace，证据不够不下结论
    通过时：基线过记 ok，基线挂记 above_baseline（不进失败统计）。
    """
    if not runs:
        return "unasserted"
    if any(r.get("status") != "complete" for r in runs):
        return "incomplete"
    passed = all(r.get("pass") for r in runs)
    b = find_baseline_entry(base, arm, task_id)
    if passed:
        return "above_baseline" if (b is not None and not b.get("passK")) else "ok"
    if b is None:
        return "unasserted"
    if not b.get("passK"):
        return "model_baseline"
    if not traces:
        return "unasserted"
    facts = []
    for i, r in enumerate(runs):
        f = traces.get((task_id, r.get("runId"))) if r.get("runId") else None
        if not f:  # 只有「没有 runId 的旧事实」才按轮次兜底；runId 对不上不许借用别轮证据
            cand = traces.get((task_id, i))
            f = cand if cand and (not cand.get("runId") or cand.get("runId") == r.get("runId")) else None
        if f:
            facts.append(f)
    if len(facts) < len(runs):
        return "unasserted"  # 有一轮缺 trace：证据不齐，不下结论
    if any(f.get("toolFailures") or not f.get("runIdMatches", True) for f in facts):
        return "engine_regression"
    return "model_below_baseline"


def label(task_id):
    """表里的题名：短 id 全显，长 id（GAIA 的 UUID）截 8 位。"""
    return task_id if len(task_id) <= 12 else task_id[:8]


def task_key(task_id):
    """基线文件里的题键：task_id 的短哈希。

    GAIA 的 task_id 也不进版本库（doc/56 §6.5 的「只入配方+hash」口径），
    所以基线表按哈希键存，读写两端用同一个函数算。
    """
    return hashlib.sha256(task_id.encode()).hexdigest()[:12]


def run_detail(row):
    """一次运行落进 details 的那份（钱与用时都留，rebuild 才能只重算不重跑）。"""
    return {"pass": bool(row["pass"]), "reason": row["reason"], "status": row["status"],
            "runId": row["runId"], "sessionId": row["sessionId"], "traceId": "",
            "ts": row["_ts"].astimezone().isoformat(timespec="seconds"),
            "cacheRead": row.get("_cacheRead") or 0,
            "cacheWrite": row.get("_cacheWrite") or 0,
            "usd": row["_usd"], "usdUpperBound": row["_usdUpper"],
            "modelCalls": row["modelCalls"], "toolCalls": row["toolCalls"],
            "totalTokens": row["totalTokens"], "wallSec": row["wallSec"],
            "answer": row["answer"]}


def finalize_details(details, arms, base, traces):
    """给每题每臂算 Pass@N / pass^N / 归因——三列都从 runs 派生，不另存状态。"""
    for tid, d in details.items():
        for arm in arms:
            ad = (d.get("arms") or {}).get(arm)
            if not ad:
                continue
            runs = arm_runs(d, arm)
            ad["passAtN"] = any(r.get("pass") for r in runs)
            ad["passK"] = bool(runs) and all(r.get("pass") for r in runs)
            ad["attribution"] = attribute(arm, tid, runs, base, traces)


def find_baseline_entry(base, arm, task_id):
    """基线表按哈希键存；旧表按明文 task_id 存的也能读。"""
    arms = (base or {}).get("arms") or {}
    table = arms.get(arm) or {}
    return table.get(task_key(task_id)) or table.get(task_id)


def write_baseline(path, case_set, protocol, details, arms):
    """落基线：只有判定，没有题面/答案/题号明文。"""
    out = {"caseSet": case_set, "createdAt": now_iso(), "repeat": protocol.get("repeat", 1),
           "modelRef": protocol.get("modelRef"), "engineCommit": protocol.get("engineCommit"),
           "sliceHash": (protocol.get("slice") or {}).get("sliceHash"),
           "taskKey": "sha256(task_id)[:12]", "arms": {}}
    for arm in arms:
        table = {}
        for tid, d in details.items():
            ad = (d.get("arms") or {}).get(arm)
            if not ad:
                continue
            table[task_key(tid)] = {"passAtN": bool(ad.get("passAtN")),
                                    "passK": bool(ad.get("passK")),
                                    "verdicts": [bool(r.get("pass")) for r in arm_runs(d, arm)]}
        out["arms"][arm] = table
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    json.dump(out, open(path, "w"), ensure_ascii=False, indent=2, sort_keys=True)
    return out


def now_iso():
    return datetime.datetime.now().astimezone().isoformat(timespec="seconds")


def git(*args):
    r = subprocess.run(["git"] + list(args), cwd=REPO, capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def load_gaia_cases(slice_path):
    """读明文切片，并按入库配方重算 slice_hash——对不上就不跑。"""
    recipe = json.load(open(GAIA_RECIPE))
    if not os.path.exists(slice_path):
        raise SystemExit("明文切片不在 %s；先跑：python3 scripts/bench/slice.py --emit %s"
                         % (slice_path, slice_path))
    data = json.load(open(slice_path))
    cases = [{"task_id": c["task_id"], "question": c["question"], "answer": c["answer"]}
             for c in data["cases"]]
    got = hashlib.sha256(
        json.dumps(cases, ensure_ascii=False, sort_keys=True).encode()).hexdigest()
    if "sha256:" + got != recipe["slice_hash"]:
        raise SystemExit("slice_hash 对不上：%s（配方 %s）——这轮跑的就不是那 25 题"
                         % (got, recipe["slice_hash"]))
    return cases, recipe


def load_smoke_cases():
    out = []
    for name in sorted(os.listdir(os.path.join(HERE, "cases"))):
        if not name.startswith("smoke-") or not name.endswith(".json"):
            continue
        d = json.load(open(os.path.join(HERE, "cases", name)))
        out.append({"task_id": d["id"], "question": d["question"], "answer": d["answer"]})
    return out


def is_peak(ts):
    """官方口径：peak = UTC 01:00–04:00 与 06:00–10:00，周一至周五（不含中国法定节假日）。"""
    t = ts.astimezone(datetime.timezone.utc)
    if t.weekday() >= 5:
        return False
    h = t.hour
    return (1 <= h < 4) or (6 <= h < 10)


def cost_usd(row, price, cache_aware=True):
    """按价目表算这一题的钱。

    两臂的 usage 口径不同，钱也不同算（doc/56 §4.7.5：并列不折算）：
      - dsh 的 usage 分缓存命中/未命中 → 命中部分按 hit 价、其余按 miss 价，是真价；
      - tako 的 usage 帧只给 promptTokens 总量、不分命中 → 一律按 miss 价算（偏贵的上界）。

    `cache_aware=False` 时一律按 miss 价算——那是**两臂可比的唯一口径**（见
    protocol.budgetComparability）：把 dsh 的 93% 缓存命中也当 miss 算，$ 才在同一条尺子上。
    """
    if not price:
        return None
    p = price["perMillionTokens"]
    peak = is_peak(row["_ts"])
    key = "peak" if peak else "offPeak"
    cin = row["promptTokens"] / 1e6 * p["inputCacheMiss"][key]
    cache_read = row.get("_cacheRead") or 0
    if cache_aware and cache_read:
        cin -= cache_read / 1e6 * p["inputCacheMiss"][key]
        cin += cache_read / 1e6 * p["inputCacheHit"][key]
    out = row["completionTokens"] / 1e6 * p["output"][key]
    return round(cin + out, 6)


def build_protocol(case_set, recipe, tail, timeout, price, arms, repeat=1):
    not_installed = [a for a in ("tako", "dsh", "pi") if a not in arms]
    return {
        "harnessVersion": "bench/3",
        "startedAt": now_iso(),
        "caseSet": case_set,
        "repeat": repeat,
        "passKSemantics": PASSK_SEMANTICS,
        "arms": {a: ARMS.get(a, {"installed": False, "note": "未知臂"}) for a in arms},
        "armsNotInstalled": not_installed,
        "modelRef": os.environ.get("TAKO_MODEL", "deepseek/deepseek-flash"),
        "engineBase": os.environ.get("TAKO_BASE", "http://127.0.0.1:8943"),
        "engineCommit": git("rev-parse", "HEAD"),
        "engineDirty": bool(git("status", "--porcelain")),
        "judge": {
            "provenance": "HF Space gaia-benchmark/leaderboard/scorer.py",
            "sha256": judge_mod.OFFICIAL_SHA,
            "fetchedAt": "2026-11-01",
            "branches": ["number", "list", "string"],
            "failReason": "mismatch",
        },
        "answerExtraction": "concat(frames[kind=delta].text) → 最后一个非空行 → "
                            "去答句前缀（答案：/ 答案是 / 最终答案：/ FINAL ANSWER:）",
        "userMessage": ("题面 + 固定尾句「%s」" % tail) if tail else "题面本身",
        "systemPrompt": "各臂用各自默认：tako = internal/config/model.go 的编译期常量；"
                        "dsh = headless profile 的 system-prompt 插件（coding agent 英文提示）。"
                        "官方 system prompt 两臂都不注入",
        "budget": {"perQuestionTimeoutSec": timeout,
                   "maxModelCalls": "tako 有步数闸 MaxIterations=50（worker.go；本轮单轮最多 27，未触顶）；"
                                    "dsh 未见步数闸；两臂最终都由每题 300s 墙钟截断"},
        "trace": {"provider": "CozeLoop + Langfuse（双发）", "captureContent": True,
                  "note": "capture 开着：GAIA 题面与答案随 span 发到两个私有 workspace。"
                          "归因取证走 Langfuse v2 observations（scripts/bench/traces.py）。"
                          "只有 tako 臂有 trace（dsh 没有 OTel）",
                  "attributionInputs": {"baseline": None, "traceFacts": None}},
        "contamination": {
            "gaiaAnswersOnOpenWeb": True,
            "note": "GAIA dev set 被大量转载，模型用 web_search 能直接命中标注答案集。"
                    "本轮量的是 runtime 的产出形态，不是模型能力上限；两臂同条件才可比。",
        },
        "answerLanguage": {
            "takoDefault": "中文（system prompt「回答简洁、说中文」）",
            "dshDefault": "英文（system prompt「You are a coding agent powered by the {{model}} model.」）",
            "officialScorer": "string 支只做「去空白 + 删 ASCII 标点 + 小写」，不翻译",
            "note": "中文长句对英文 gold 必挂——格式失配要与内容失配分开归因。",
        },
        "pricing": price or {"note": "没有价目表：$ 列留空"},
        "budgetComparability": BUDGET_COMPARABILITY,
        "slice": {
            "sliceId": (recipe or {}).get("slice_id"),
            "sourceRepo": (recipe or {}).get("source_repo"),
            "sourceFile": (recipe or {}).get("source_file"),
            "sourceRevisionMain": (recipe or {}).get("source_revision_main"),
            "sliceSize": (recipe or {}).get("slice_size"),
            "sliceHash": (recipe or {}).get("slice_hash"),
            "plaintextInRepo": False,
        },
    }


def arm_protocol(arm):
    """每臂自己的口径事实（版本、模型、入口）。"""
    if arm == "dsh":
        return {
            "harness": "dsh --profile headless --json（npm @deepseek-ai/dsh）",
            "version": dsh_arm.DSH_VERSION,
            "profile": "headless",
            "model": "deepseek-flash（deepseek-official 路由，profile 默认）",
            "searchProvider": "deepseek-official（与 tako 臂同一个搜索后端、同一把密钥）",
            "systemPrompt": "headless profile 的 system-prompt 插件（英文 coding agent 提示）",
            "perQuestionCwd": "tmp/bench-dsh/work/<task_id>（每题一个干净目录）",
            "budgetColumns": "usage 事件分 inputTokens / cacheReadTokens / cacheWriteTokens / outputTokens；"
                              "modelCalls = step_start 事件数",
        }
    return {
        "harness": "tako 引擎 HTTP 面（POST /api/runs）",
        "model": os.environ.get("TAKO_MODEL", "deepseek/deepseek-flash"),
        "searchProvider": "deepseek /v1/tools/search",
        "systemPrompt": "internal/config/model.go 的编译期常量（中文向）",
        "perQuestionCwd": "引擎侧独立项目（POST /api/workspaces）",
        "budgetColumns": "usage 帧只给 promptTokens 总量、不分缓存命中 → $ 按缓存未命中价算（偏贵的上界）",
    }


def render_md(protocol, summary, rows, details, case_set, arms):
    title = "GAIA L1 文本切片" if case_set == "gaia" else case_set
    lines = ["# %s · %s 跑分" % (title, "+".join(arms)), "",
             "## 口径", ""]
    for k, v in protocol.items():
        if isinstance(v, dict):
            lines.append("- **%s**：%s" % (k, json.dumps(v, ensure_ascii=False)))
        else:
            lines.append("- **%s**：%s" % (k, v))

    for arm in arms:
        arows = [r for r in rows if r["arm"] == arm]
        lines += ["", "## 逐题 · %s" % arm, "",
                  "| 题 | Pass@N | pass^N | 逐次 | 通过轮次 | 归因 | 模型调用 | 工具调用 | token | $（本臂口径） | $（同口径上界） | 墙钟 |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for tid, d in details.items():
            ad = (d["arms"] or {}).get(arm)
            if not ad:
                continue
            trs = [r for r in arows if r["taskId"] == tid]
            n = len(trs)
            npass = sum(1 for r in trs if r["pass"])
            seq = "".join("✅" if r["pass"] else "❌" for r in trs)
            runs = ad.get("runs") or [ad]
            usd = round(sum((a.get("usd") or 0) for a in runs), 6) \
                if any(a.get("usd") is not None for a in runs) else None
            usd_up = round(sum((a.get("usdUpperBound") or 0) for a in runs), 6) \
                if any(a.get("usdUpperBound") is not None for a in runs) else None
            lines.append("| %s | %s | %s | %s | %d/%d | %s | %d | %d | %d | %s | %s | %ss |" % (
                label(tid), "✅" if ad.get("passAtN") else "❌", "✅" if ad.get("passK") else "❌",
                seq or "—", npass, n, ad.get("attribution") or "—",
                sum(r["modelCalls"] for r in trs), sum(r["toolCalls"] for r in trs),
                sum(r["totalTokens"] for r in trs),
                usd if usd is not None else "—", usd_up if usd_up is not None else "—",
                round(sum(r["wallSec"] for r in trs), 1)))
        s = summary["byArm"][arm]
        lines += ["", "**%s：Pass@N %d / %d；pass^N %d / %d**（共 %d 次运行）" % (
            arm, s["passAtN"], s["total"], s["passK"], s["total"], s["runs"]),
            "", "归因：%s" % "；".join("%s %d" % (k, v)
                                       for k, v in (s.get("byAttribution") or {}).items())
                  if s.get("byAttribution") else "归因：无基线/无 trace，全部 unasserted", ""]
        bad = [r for r in arows if r["pass"] is not True]
        if bad:
            lines += ["### %s 没过的轮次" % arm, ""]
            for r in bad:
                d = details[r["taskId"]]
                lines.append("- `%s`（%s）：答案 %r，gold %r，原因 %s" % (
                    r["taskId"], r["status"], r["answer"], d["gold"], r["reason"]))
            lines.append("")

    if len(arms) >= 2:
        lines += ["## 配对（同一题、同一模型、同一搜索后端、同一判定器，只差 harness）", "",
                  "| 题 | %s | %s | 一致 |" % (arms[0], arms[1]),
                  "|---|---|---|---|"]
        both = only0 = only1 = neither = 0
        for tid, d in details.items():
            verdicts = []
            for arm in arms[:2]:
                verdicts.append(bool((d["arms"].get(arm) or {}).get("passAtN")))
            v0, v1 = verdicts
            if v0 and v1:
                both += 1
                mark = "都过"
            elif v0:
                only0 += 1
                mark = "只有 %s 过" % arms[0]
            elif v1:
                only1 += 1
                mark = "只有 %s 过" % arms[1]
            else:
                neither += 1
                mark = "都没过"
            lines.append("| %s | %s | %s | %s |" % (
                label(tid), "✅" if v0 else "❌", "✅" if v1 else "❌", mark))
        lines += ["", "都过 %d；只有 %s 过 %d；只有 %s 过 %d；都没过 %d" % (
            both, arms[0], only0, arms[1], only1, neither), ""]

    lines += ["## 预算：两臂的 $ 不是同一把尺子", "",
              "| 臂 | $（本臂口径） | $（同口径上界） | token | 模型调用 | 工具调用 | 墙钟 |",
              "|---|---|---|---|---|---|---|"]
    for arm in arms:
        s = summary["byArm"][arm]
        lines.append("| %s | %s | %s | %s | %s | %s | %ss |" % (
            arm, s.get("usd"), s.get("usdUpperBound"), s["totalTokens"], s["modelCalls"],
            s["toolCalls"], s["wallSec"]))
    lines += ["", protocol.get("budgetComparability", {}).get("note", ""),
              "同口径上界 = 两臂一律按**缓存未命中**价算（把 dsh 的缓存命中也当没命中算）——"
              "只有这一列在两臂之间可比。", ""]

    missing = [a for a in ("pi",) if not ARMS[a]["installed"]]
    if missing:
        lines += ["## 本轮未装的臂", "",
                  "- %s" % "；".join("%s：%s" % (a, ARMS[a]["note"]) for a in missing),
                  "", "缺臂的题不进通过率分母，也不折算成 0。", ""]
    return "\n".join(lines)


def build_summary(rows, details, arms, price):
    """按臂汇总 + 配对 + pass^N。rows 是逐次运行，细节（过没过）看 details。"""
    summary = {"total": len(details), "byArm": {}}
    for arm in arms:
        arows = [r for r in rows if r["arm"] == arm]
        ads = [(tid, d["arms"][arm]) for tid, d in details.items()
               if (d.get("arms") or {}).get(arm)]
        summary["byArm"][arm] = {
            "total": len(ads),
            "runs": len(arows),
            "passed": sum(1 for r in arows if r["pass"]),
            "failed": sum(1 for r in arows if not r["pass"]),
            "passAtN": sum(1 for _, ad in ads if ad.get("passAtN")),
            "passK": sum(1 for _, ad in ads if ad.get("passK")),
            "modelCalls": sum(r["modelCalls"] for r in arows),
            "toolCalls": sum(r["toolCalls"] for r in arows),
            "totalTokens": sum(r["totalTokens"] for r in arows),
            "wallSec": round(sum(r["wallSec"] for r in arows), 1),
            "usd": round(sum(r["_usd"] for r in arows), 4) if price else None,
            "usdUpperBound": round(sum(r["_usdUpper"] for r in arows), 4) if price else None,
            "byReason": {k: sum(1 for r in arows if r["reason"] == k)
                         for k in sorted({r["reason"] for r in arows})},
            "byAttribution": {k: sum(1 for _, ad in ads if ad.get("attribution") == k)
                              for k in sorted({ad.get("attribution") for _, ad in ads if ad.get("attribution")})},
        }
    if len(arms) >= 2:
        x, y = arms[0], arms[1]
        both = only_x = only_y = neither = 0
        for tid, d in details.items():
            px = bool((d["arms"].get(x) or {}).get("passAtN"))
            py = bool((d["arms"].get(y) or {}).get("passAtN"))
            if px and py:
                both += 1
            elif px:
                only_x += 1
            elif py:
                only_y += 1
            else:
                neither += 1
        summary["paired"] = {"%s vs %s" % (x, y): {
            "bothPass": both, "only%s" % x: only_x,
            "only%s" % y: only_y, "bothFail": neither,
            "note": "按 Pass@N 配对（N 次里至少过一次）；pass^N 见各自臂的逐题表",
        }}
    return summary


def rebuild(report_path, baseline_override="", traces_override=""):
    """从已存的报告 json 重算派生的钱与摘要、重渲染 md——不重跑题。

    改的是口径字段（比如加一列可比的钱）时，重跑 25 题要再烧一次钱与一小时墙钟；
    rows + details 里已经有全部原料，重算即可。
    归因也可以后补：trace 是异步上报的，跑完那一刻可能还没到 Langfuse；
    先用 traces.py 拉成事实文件，再 --rebuild --traces <facts> 回填。
    """
    data = json.load(open(report_path))
    if baseline_override:
        data["baseline"] = load_baseline(baseline_override)
        if not data["baseline"]:
            raise SystemExit("--rebuild 的基线读不到：%s" % baseline_override)
    if traces_override:
        data["traceFacts"] = load_trace_facts(traces_override)
        if data["traceFacts"] is None:
            raise SystemExit("--rebuild 的 trace 事实读不到：%s" % traces_override)
    price = None
    ppath = os.path.join(HERE, "pricing.json")
    if os.path.exists(ppath):
        price = json.load(open(ppath))
    data["protocol"].setdefault("budgetComparability", BUDGET_COMPARABILITY)
    details = data.get("details", {})
    started = data["protocol"].get("startedAt")
    rows = []
    for tid, det in details.items():
        for arm, ad in det["arms"].items():
            src_rows = [r for r in data["rows"]
                        if r["taskId"] == tid and r["arm"] == arm]
            if not src_rows:
                continue
            if ad.get("runs") is None:  # bench/2 的旧报告：一题一臂只有标量
                ad["runs"] = [{**src_rows[0],
                               "usd": ad.get("usd"), "usdUpperBound": ad.get("usdUpperBound"),
                               "runId": ad.get("runId") or src_rows[0].get("runId"),
                               "sessionId": ad.get("sessionId") or "",
                               "traceId": ad.get("traceId") or "",
                               "ts": ad.get("ts") or started,
                               "cacheRead": ad.get("cacheRead") or 0,
                               "cacheWrite": ad.get("cacheWrite") or 0}]
            for i, run in enumerate(ad["runs"]):
                src = src_rows[i] if i < len(src_rows) else src_rows[0]
                row = dict(src)
                row["_ts"] = datetime.datetime.fromisoformat(run.get("ts") or started)
                row["_cacheRead"] = run.get("cacheRead") or 0
                row["_cacheWrite"] = run.get("cacheWrite") or 0
                row["_usd"] = cost_usd(row, price)
                row["_usdUpper"] = cost_usd(row, price, cache_aware=False)
                run["usd"] = row["_usd"]
                run["usdUpperBound"] = row["_usdUpper"]
                rows.append(row)
    arms = list(data["protocol"].get("arms", {}).keys())
    data["protocol"].setdefault("trace", {})["attributionInputs"] = {
        "baseline": baseline_override or None,
        "baselineRepeat": (data.get("baseline") or {}).get("repeat"),
        "traceFacts": traces_override or None,
        "note": "归因是后补的：trace 异步上报，跑完那一刻拉会大面积 unasserted。",
    }
    finalize_details(details, arms, data.get("baseline"), trace_index(data.get("traceFacts")))
    data["summary"] = build_summary(rows, details, arms, price)
    clean = [{k: r[k] for k in SCHEMA_KEYS} for r in rows]
    data["rows"] = clean
    write_json(report_path, data)
    md_path = report_path[:-5] + ".md"
    open(md_path, "w").write(render_md(data["protocol"], data["summary"], clean,
                                       details, data["protocol"].get("caseSet", "gaia"), arms))
    print("重算并重渲染 → %s" % report_path)
    red = sorted({ad.get("attribution") for d in details.values()
                  for ad in (d.get("arms") or {}).values()
                  if ad.get("attribution") in ("engine_regression", "model_below_baseline")})
    if red:
        print("归因有红：%s → 退出码 1" % "、".join(red), file=sys.stderr)
        return 1
    return 0


def attach_traces(report_path, log_path):
    """把引擎日志里的 run 根 span traceId 贴回报告。

    引擎每跑完一场 run 打一行 `trace <sessionId>/<runId> traceId=<hex>`（worker.go）。
    CozeLoop 没有公开查询 API，所以「这轮有 25 个根 span」的可核对凭据就是这 25 个 traceId：
    拿它们去控制台按 traceId 查得到，才算数。
    """
    if not os.path.exists(log_path):
        raise SystemExit("找不到引擎日志 %s" % log_path)
    ids = {}
    for line in open(log_path, encoding="utf-8", errors="replace"):
        m = re.search(r"trace (\S+)/(\S+) traceId=([0-9a-f]+)", line)
        if m:
            ids[m.group(2)] = m.group(3)
    data = json.load(open(report_path))
    hit, total = 0, 0
    for d in data.get("details", {}).values():
        for arm, ad in (d.get("arms") or {}).items():
            for run in arm_runs(d, arm):
                if not run.get("runId"):
                    continue
                total += 1
                tid = ids.get(run["runId"])
                if tid:
                    run["traceId"] = tid
                    hit += 1
    data["protocol"]["traceIds"] = {
        "source": "引擎日志的 run 根 span 行（worker.go）",
        "engineLog": log_path,
        "runs": total,
        "matched": hit,
    }
    write_json(report_path, data)
    md_path = report_path[:-5] + ".md"
    if os.path.exists(md_path):
        md = open(md_path).read()
        md += ("\n## trace 核对\n\n本轮 %d/%d 个 run 的根 span traceId"
               "（引擎日志取的，拿去 CozeLoop / Langfuse 按 traceId 查得到才算数）：\n\n" % (hit, total))
        for d in data.get("details", {}).values():
            for arm, ad in (d.get("arms") or {}).items():
                for run in arm_runs(d, arm):
                    if run.get("traceId"):
                        md += "- `%s`（%s） → `%s`\n" % (run.get("runId"), arm, run["traceId"])
        open(md_path, "w").write(md)
    print("贴回 traceId %d/%d → %s" % (hit, total, report_path))
    return 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cases", default="gaia", help="gaia | smoke")
    ap.add_argument("--arms", default="tako", help="逗号分隔（tako / dsh；pi 未装）")
    ap.add_argument("--limit", type=int, default=0, help="只跑前 N 题")
    ap.add_argument("--timeout", type=float, default=300, help="单题墙钟上限（秒）")
    ap.add_argument("--repeat", type=int, default=1, help="同一题跑 N 次（Pass@N / pass^N）")
    ap.add_argument("--tail", default="", help="题面后追加的固定尾句（默认不加）")
    ap.add_argument("--slice", default=GAIA_SLICE, help="明文切片路径")
    ap.add_argument("--out", default="", help="报告目录（默认 tmp/bench-reports/<ts>）")
    ap.add_argument("--baseline", default="",
                    help="基线 json（不给就用 scripts/bench/baselines/<cases>.json，存在才读）")
    ap.add_argument("--write-baseline", nargs="?", const="__default__", default="",
                    help="把本轮判定写成基线（不给路径就写默认位置）")
    ap.add_argument("--traces", default="",
                    help="traces.py --summary-json 的产物：归因取证（工具失败/串号）")
    ap.add_argument("--engine-log", default=os.environ.get("TAKO_ENGINE_LOG", ""),
                    help="引擎日志路径（用来取 run 根 span 的 traceId）")
    ap.add_argument("--attach-traces", default="", help="只给已有报告贴 traceId，不跑题")
    ap.add_argument("--rebuild", default="", help="只给已有报告重算派生的钱与摘要、重渲染 md，不跑题")
    a = ap.parse_args()
    if a.rebuild:
        return rebuild(a.rebuild, a.baseline, a.traces)
    if a.attach_traces:
        return attach_traces(a.attach_traces, a.engine_log)
    if a.repeat < 1:
        raise SystemExit("--repeat 至少是 1")
    arms = [x.strip() for x in a.arms.split(",") if x.strip()]
    for arm in arms:
        if not ARMS.get(arm, {}).get("installed"):
            raise SystemExit("臂 %s 本轮未装：%s" % (arm, ARMS.get(arm, {}).get("note")))

    recipe = None
    if a.cases == "gaia":
        cases, recipe = load_gaia_cases(a.slice)
    elif a.cases == "smoke":
        cases = load_smoke_cases()
    else:
        raise SystemExit("未知题集 %s" % a.cases)
    if a.limit:
        cases = cases[:a.limit]

    price = None
    ppath = os.path.join(HERE, "pricing.json")
    if os.path.exists(ppath):
        price = json.load(open(ppath))

    base_path = a.baseline or baseline_path(a.cases)
    base = load_baseline(base_path)
    if a.baseline and not base:
        raise SystemExit("给了 --baseline 但读不到：%s" % a.baseline)
    traces_raw = load_trace_facts(a.traces)
    if a.traces and traces_raw is None:
        raise SystemExit("给了 --traces 但读不到：%s" % a.traces)
    traces = trace_index(traces_raw)

    ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    outdir = a.out or os.path.join(REPORT_ROOT, ts)
    os.makedirs(outdir, exist_ok=True)

    protocol = build_protocol(a.cases, recipe, a.tail, a.timeout, price, arms, a.repeat)
    protocol["trace"]["attributionInputs"] = {
        "baseline": base_path if base else None,
        "baselineRepeat": (base or {}).get("repeat"),
        "baselineNote": ("基线按 pass^N 判定比对；本轮 repeat 与基线不同则 pass^N 口径不一致，"
                         "只作参考" if base and (base or {}).get("repeat") != a.repeat else None),
        "traceFacts": a.traces or None,
    }
    auth = None
    if "tako" in arms:
        auth = tako_arm.signup()
        print("tenant: %s %s" % (auth["tenant"], auth["email"]), file=sys.stderr)
        protocol["tenant"] = auth["tenant"]
        protocol["accessTokenRefresh"] = ("每题前查剩余有效期，不足 60s 先 POST /api/auth/refresh；"
                                          "中途 401 自动换一次重试")
    protocol["armDetails"] = {arm: arm_protocol(arm) for arm in arms}

    details = {c["task_id"]: {"gold": c["answer"], "question": c["question"], "arms": {}}
               for c in cases}
    rows = []
    total_runs = len(cases) * len(arms) * a.repeat
    n = 0
    for rep in range(1, a.repeat + 1):
        for c in cases:
            for arm in arms:
                n += 1
                t0 = time.time()
                if arm == "tako":
                    row = tako_arm.run_one(auth, c["question"] + a.tail, c["task_id"], a.timeout)
                elif arm == "dsh":
                    row = dsh_arm.run_one(c["question"] + a.tail, c["task_id"], a.timeout)
                else:
                    raise SystemExit("没有 %s 臂的 runner" % arm)
                verdict = judge_mod.judge({"answer": c["answer"]}, row["answer"])
                row["pass"] = bool(verdict["pass"]) if row["status"] == "complete" else False
                row["reason"] = verdict["reason"] if row["status"] == "complete" else row["status"]
                row["_ts"] = datetime.datetime.fromtimestamp(t0)
                row["_usd"] = cost_usd(row, price)
                row["_usdUpper"] = cost_usd(row, price, cache_aware=False)
                details[c["task_id"]]["arms"].setdefault(arm, {"runs": []})["runs"].append(
                    run_detail(row))
                print("[%d/%d r%d] %s %s %s status=%s %ss calls=%s tools=%s answer=%r" % (
                    n, total_runs, rep, c["task_id"][:8], arm,
                    "PASS" if row["pass"] else "FAIL",
                    row["status"], row["wallSec"], row["modelCalls"], row["toolCalls"],
                    row["answer"]), file=sys.stderr, flush=True)
                rows.append(row)

    finalize_details(details, arms, base, traces)
    summary = build_summary(rows, details, arms, price)
    base_name = ("gaia-25-%s" % ts if a.cases == "gaia" else "%s-%s" % (a.cases, ts))
    if a.repeat > 1:
        base_name += "-x%d" % a.repeat
    # rows 只放 row.schema.json 那 16 个键（机器守门要判它）；额外明细另放 details。
    clean = [{k: r[k] for k in SCHEMA_KEYS} for r in rows]
    json_path = os.path.join(outdir, base_name + ".json")
    report = {"protocol": protocol, "summary": summary, "rows": clean, "details": details}
    if base:
        report["baseline"] = base
    if traces_raw:
        report["traceFacts"] = traces_raw
    write_json(json_path, report)
    open(os.path.join(outdir, base_name + ".md"), "w").write(
        render_md(protocol, summary, clean, details, a.cases, arms))
    print("\n%s；报告 %s/%s.{md,json}" % (
        " / ".join("%s Pass@N %d/%d pass^N %d/%d" % (
            arm, summary["byArm"][arm]["passAtN"], summary["byArm"][arm]["total"],
            summary["byArm"][arm]["passK"], summary["byArm"][arm]["total"]) for arm in arms),
        outdir, base_name))
    if a.engine_log and os.path.exists(a.engine_log):
        attach_traces(json_path, a.engine_log)

    if a.write_baseline:
        wpath = baseline_path(a.cases) if a.write_baseline == "__default__" else a.write_baseline
        write_baseline(wpath, a.cases, protocol, details, arms)
        print("基线 → %s" % wpath)

    red = sorted({ad.get("attribution") for d in details.values()
                  for ad in d["arms"].values()
                  if ad.get("attribution") in ("engine_regression", "model_below_baseline")})
    if red:
        print("\n归因有红：%s → 退出码 1（有基线且本轮更差）" % "、".join(red), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
