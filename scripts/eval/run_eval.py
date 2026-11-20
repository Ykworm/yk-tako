#!/usr/bin/env python3
"""scripts/eval 的唯一 CLI：preflight / selftest / calibrate / run / rebuild / probe。

用法见 --help。纪律：
- selftest 全确定性：人工构造证据跑判定器，不打模型、不触网。
- probe 默认只生成最小请求（dry-run）；显式 --execute 才发真实外部调用。
- calibrate / run 需要已实测固定（status=verified）的裁判配置与真实密钥；
  不满足就红着退出（退出码 2），不静默降级。
退出码（施工包 §9）：0 达标/全过；1 批次 U=0 有失败；2 前置/校准/无效 trial；
3 评测程序未预期异常。
"""
import argparse
import datetime
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parent
REPO_ROOT = EVAL_DIR.parents[1]
sys.path.insert(0, str(EVAL_DIR))
sys.path.insert(0, str(REPO_ROOT / "scripts" / "acceptance"))

import casefile
import evidence as ev
import grader
import harness
import judges
import report
import target

CASES_DIR = EVAL_DIR / "cases"
CALIB_DIR = EVAL_DIR / "calibration"
BASELINES_DIR = EVAL_DIR / "baselines"
DEFAULT_CASE = CASES_DIR / "doc-verify.json"
DEFAULT_JUDGE = CASES_DIR / "doc-verify.judge.json"


# ---------- 共通 ----------

def _load(case_path, judge_path=None):
    case = casefile.load_case(case_path)
    cfg = judges.load_judge_config(judge_path) if judge_path else None
    return case, cfg


def _fingerprint(case, judge_cfg=None, extra=None, eval_dir=None):
    """配置指纹：案例 + 引擎 + 评测程序自身 + 裁判配置，缺项记 unversioned。

    覆盖施工包「案例与配置」：判判定器/执行程序/裁判配置/阈值任何一方变化都会
    改变指纹，杜绝与旧基线的错误比较。
    - evalTreeSha256：scripts/eval 运行真源哈希（本机算，确定性）；
    - judgeConfigSha256：实际加载的裁判配置 canonical JSON 哈希。
    """
    env_file = os.environ.get("TAKO_EVAL_ENV_SH")
    vals = {}
    if env_file and Path(env_file).exists():
        for line in Path(env_file).read_text().splitlines():
            if "=" in line and not line.strip().startswith("#"):
                k, _, v = line.partition("=")
                vals[k.strip()] = v.strip().strip('"')
    commit = vals.get("ENGINE_COMMIT") or _git("rev-parse HEAD")
    dirty = (vals.get("ENGINE_DIRTY") or "").lower() in ("1", "true") or _git_dirty()
    merged_extra = {"evalTreeSha256": _eval_tree_sha256(eval_dir or EVAL_DIR),
                    "judgeConfigSha256": _judge_config_sha256(judge_cfg)}
    merged_extra.update(extra or {})
    fp = casefile.config_fingerprint(
        case_sha256=case.raw_sha256,
        model=os.environ.get("TAKO_MODEL", harness.MODEL),
        engine_commit=commit, engine_dirty=dirty,
        mcp_sha256=vals.get("MCP_JSON_SHA256"),
        tree_sha256=vals.get("TREE_SHA256"),
        extra=merged_extra)
    return fp


def _eval_tree_sha256(eval_dir):
    """scripts/eval/ 运行真源的确定性哈希：相对 POSIX 路径 + 原始字节，排序稳定。

    纳入：*.py、*.sh、cases/*.json、calibration/*.json（tests/ 下的 .py 同属评测程序）。
    排除：baselines/（基线文件不能进入自身指纹）、__pycache__、任何证据/临时产物。
    """
    import hashlib
    root = Path(eval_dir)
    rels = []
    for p in root.rglob("*"):
        if not p.is_file():
            continue
        rel = p.relative_to(root)
        parts = rel.parts
        if "__pycache__" in parts or ".git" in parts or parts[0] == "baselines":
            continue
        if p.suffix in (".py", ".sh") or \
                (p.suffix == ".json" and parts[0] in ("cases", "calibration")):
            rels.append(rel.as_posix())
    h = hashlib.sha256()
    for rel in sorted(rels):
        h.update(rel.encode("utf-8"))
        h.update(b"\0")
        h.update((root / rel).read_bytes())
        h.update(b"\0")
    return h.hexdigest()


def _judge_config_sha256(cfg):
    """实际加载的裁判配置的 canonical JSON 哈希；未提供记 unversioned。"""
    import hashlib
    if not cfg:
        return "unversioned"
    canonical = json.dumps(cfg, ensure_ascii=False, sort_keys=True)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def _git(args):
    try:
        r = subprocess.run(["git"] + args.split(), cwd=REPO_ROOT,
                           capture_output=True, text=True, timeout=10)
        return r.stdout.strip() if r.returncode == 0 else None
    except OSError:
        return None


def _git_dirty():
    out = _git("status --porcelain")
    return bool(out)


# ---------- trial 判定（run 与 rebuild 共用） ----------

def build_trial_evidence(case, rounds_data, initial, final, run_rounds=None):
    """从一轮真实/合成执行原料提取判定输入。"""
    run_rounds = run_rounds or {r["runId"]: i + 1 for i, r in enumerate(rounds_data)}
    rows = [row for r in rounds_data for row in r["runlog"]]
    frames = [f for r in rounds_data for f in r["frames"]]
    pairs, unpaired = ev.pair_tool_events(rows)
    events, unparsed = ev.extract_read_events(pairs, run_rounds, case.fixture_paths)
    answers, record_missing = {}, set()
    citations = []
    for i, r in enumerate(rounds_data, start=1):
        row, problem = ev.main_answer_row(r["runlog"], r["runId"])
        if problem is not None or not r.get("answer", {}).get("compare", {}).get("ok", False):
            record_missing.add(i)
            answers[i] = None
            continue
        answers[i] = row.get("text") or ""
        citations += ev.extract_citations(answers[i], i, r["runId"], case.files)
    violations = ev.network_violations(pairs)
    return {"answers": answers, "citations": citations, "readEvents": events,
            "unparsed": unparsed, "unpaired": unpaired, "recordMissing": record_missing,
            "violations": violations, "rows": rows, "frames": frames,
            "initialManifest": initial, "finalManifest": final}


def grade_checks(case, te, q_outcome, d_outcome, threshold):
    """A/B 程序检查 + Q/D 三票结果合成 checks 字典。q/d_outcome 见 run_judges。"""
    checks = {}
    checks["A1"] = grader.check_a1(te["citations"], te["readEvents"], te["unparsed"],
                                   te["recordMissing"])
    checks["A2"] = grader.a2_locate(te["answers"])          # 只产证据，不进必需集合
    checks["A3"] = grader.check_a3(te["violations"])
    cites1 = [c for c in te["citations"] if c.get("round") == 1]
    events1 = [e for e in te["readEvents"] if e.get("round") == 1]
    checks["A4"] = grader.check_a4(te["answers"].get(1), cites1, events1)
    checks["B1"] = grader.check_b1(te["citations"], case.files)
    final_complete = te["finalManifest"] is not None
    checks["B2"] = grader.check_b2(te["initialManifest"], te["finalManifest"], final_complete)
    for qid in judges.Q_IDS:
        item = q_outcome.get(qid, {"e": 1, "answers": []})
        checks[qid] = _q_check(qid, item)
    for did in judges.D_IDS:
        item = d_outcome.get(did, {"e": 1, "verdicts": []})
        checks[did] = _d_check(did, item, threshold)
    return checks


def _q_check(qid, item):
    status = grader.q_item_result(item.get("e", 1), item.get("answers", []),
                                  judges.Q_DIRECTIONS[qid])
    return {"status": status,
            "reason": f"三票：e={item.get('e', 1)} votes={item.get('answers', [])}",
            "evidence": item.get("rawVotes")}


def _d_check(did, item, threshold):
    status = grader.d_item_result(item.get("e", 1), item.get("verdicts", []), threshold)
    return {"status": status,
            "reason": f"三票：e={item.get('e', 1)} verdicts={_brief(item.get('verdicts', []))}"
                      f" threshold={threshold}",
            "evidence": item.get("rawVotes")}


def _brief(verdicts):
    return [{"score": v.get("score"), "unknown": v.get("unknown"),
             "hardViolation": v.get("hardViolation")} for v in verdicts]


def run_judges(cfg, case, te, votes, timeout_sec, save=None, transport=None):
    """真实调用两类裁判（正式 run / calibrate 用）。

    返回 (q_res, d_full, threshold)：q_res/d_full 是 judges 的完整返回
    （perQ/verdicts + 逐项 calls + requestBody），调用方负责按固定文件名落盘：
    judges/decision-request.json、judges/decision-{i+1}-response.json、
    judges/llm-D{n}-request.json、judges/llm-D{n}-{i+1}-response.json。
    transport 可注入合成实现（测试/自测），默认真实 curl。
    """
    state = {"questions": [{"id": t.id, "user": t.user} for t in case.turns],
             "answers": {str(r): te["answers"].get(r) for r in (1, 2, 3)},
             "files": [{"path": f.path, "content": f.content} for f in case.files],
             "citations": te["citations"], "readEvents": te["readEvents"],
             "expected": {t.id: t.expected for t in case.turns},
             "judgmentBoundary": case.judgment_boundary}
    q_res = judges.run_decision_votes(cfg, state, case.decision_questions,
                                      votes=votes, timeout_sec=timeout_sec,
                                      save=save, transport=transport)
    threshold = (cfg["llm"].get("threshold") if cfg["llm"].get("thresholdVerified")
                 else None)
    d_full = {}
    for did in judges.D_IDS:
        rnd = case.dimensions[did]["round"]
        messages = judges.llm_dimension_messages(
            cfg, case, did, rnd, case.turns[rnd - 1].user, te["answers"].get(rnd),
            a1_result=None, citations=[c for c in te["citations"] if c.get("round") == rnd],
            read_events=[e for e in te["readEvents"] if e.get("round") == rnd])
        d_full[did] = judges.run_llm_votes(cfg, case, did, messages,
                                           votes=votes, timeout_sec=timeout_sec,
                                           save=save, name=f"llm-{did}",
                                           transport=transport)
    return q_res, d_full, threshold


def _d_items(d_full, votes):
    """judges 完整返回 → grade_checks 需要的 {did: {e, verdicts, rawVotes}}。"""
    return {did: {"e": votes - len(res["verdicts"]), "verdicts": res["verdicts"],
                  "rawVotes": res["calls"]} for did, res in d_full.items()}


# ---------- preflight ----------

def cmd_preflight(a):
    case, cfg = _load(a.case, a.judge_config if Path(a.judge_config).exists() else None)
    evd = report.new_evidence_dir(case.case_id, "preflight", base=a.out)
    findings = []

    def ok(name, cond, detail=""):
        findings.append({"item": name, "ok": bool(cond), "detail": detail})

    ok("case", True, f"{case.case_id} sha256={case.raw_sha256[:16]}… turns={len(case.turns)}")
    try:
        key = harness.api_key()
        ok("tako-env-deepseek-key", True, f"已从 {harness.ENV_PATH} 读到 DEEPSEEK_API_KEY")
    except SystemExit as e:
        ok("tako-env-deepseek-key", False, str(e))
    try:
        code, body = harness.call("GET", "/api/workspaces")
        ok("engine-auth-gate", code == 401,
           f"GET /api/workspaces → HTTP {code}（预期 401）")
    except Exception as e:
        ok("engine-auth-gate", False, f"连不上引擎 {harness.BASE}：{e}")
    ok("ws-root", Path(harness.WS_ROOT).exists(),
       f"TAKO_WS_ROOT={harness.WS_ROOT}")
    ok("dashscope-key", bool(os.environ.get("DASHSCOPE_API_KEY")),
       "DASHSCOPE_API_KEY（decision 裁判密钥）在进程环境里")
    if cfg is not None:
        for kind in ("decision", "llm"):
            st = cfg[kind]["responseParse"].get("status")
            ok(f"judge-{kind}-parse", st == "verified",
               f"responseParse.status={st}" + ("" if st == "verified"
                                               else "（需先 probe --execute 实测并写回 judge.json）"))
    obj = {"command": "preflight", "at": datetime.datetime.now().isoformat(timespec="seconds"),
           "base": harness.BASE, "model": os.environ.get("TAKO_MODEL", harness.MODEL),
           "findings": findings,
           "ready": all(f["ok"] for f in findings if f["item"] != "judge-decision-parse"
                        and f["item"] != "judge-llm-parse")}
    report.atomic_write_json(evd / "preflight.json", obj,
                             secrets_to_check=_known_secrets())
    print(f"preflight → {evd}")
    for f in findings:
        print(f"  {'✅' if f['ok'] else '❌'} {f['item']}: {f['detail']}")
    return 0 if obj["ready"] else 2


def _known_secrets():
    out = []
    try:
        out.append(harness.api_key())
    except SystemExit:
        pass
    out.append(os.environ.get("DASHSCOPE_API_KEY", ""))
    return out


# ---------- selftest（确定性，15 组关键分支 + 施工包 11.1 边界） ----------

class SelftestFail(Exception):
    pass


def _expect(label, cond, detail=""):
    if not cond:
        raise SelftestFail(f"{label} —— {detail}")
    print(f"  ✅ {label}")


def _std_answers():
    return {
        1: ("方案 B 停止推进的理由见 决策-方案B.md §2：一是维护成本翻倍（需要两套存储并行），"
            "二是与方案 D 功能重叠。结论见 决策-方案B.md §3：方案 B 不再推进。"),
        2: ("两份记录不矛盾：决策记录未列出压测问题，不等于否认该事实。"
            "停止推进的决定以 决策-方案B.md §3 为准；P99 未达标的事实来自 事故记录-0620.md §4。"),
        3: ("不能。旧方案-方案B.md 头注声明已作废、由 决策-方案B.md 取代（见 旧方案-方案B.md 与 "
            "决策-方案B.md 的头注）；其中“计划 2026-09 上线”“依赖方案 A 未完成”只是历史记载，"
            "不能作为当前恢复排期的依据。"),
    }


def _std_events(case):
    """三轮都在定稿前访问了决策记录（read_file）+ 第三轮访问旧方案。"""
    events = []
    for rnd, path in ((1, "决策-方案B.md"), (2, "决策-方案B.md"),
                      (2, "事故记录-0620.md"), (3, "旧方案-方案B.md"),
                      (3, "决策-方案B.md")):
        events.append({"round": rnd, "runId": f"r{rnd}", "runlogOffset": rnd * 10,
                       "agent": "", "toolCallId": f"call_{rnd}_{path}",
                       "tool": "read_file", "path": path,
                       "output": case.fixture(path).content[:60]})
    return events


def _manifest(case):
    return casefile.fixture_manifest(case.files)


def _std_q():
    return {qid: {"e": 0, "answers": ans} for qid, ans in
            {"Q1": ["yes"] * 3, "Q2": ["yes"] * 3, "Q3": ["no"] * 3}.items()}


def _std_d(threshold=3):
    return {did: {"e": 0, "verdicts": [{"score": 5, "unknown": False,
                                        "hardViolation": False} for _ in range(3)]}
            for did in judges.D_IDS}


_UNSET = object()


def _grade(case, answers, events, unparsed=(), record_missing=frozenset(),
           violations=(), initial=_UNSET, final=_UNSET, q=None, d=None, threshold=3,
           citation_answer=None):
    """selftest 判定入口：答案 → citations（可覆盖）→ 全套 checks。"""
    citations = []
    for rnd in (1, 2, 3):
        text = citation_answer.get(rnd) if citation_answer else answers.get(rnd)
        if text:
            citations += ev.extract_citations(text, rnd, f"r{rnd}", case.files)
    te = {"answers": answers, "citations": citations,
          "readEvents": [_norm_event(e, case) for e in events],
          "unparsed": list(unparsed), "recordMissing": set(record_missing),
          "violations": list(violations),
          "initialManifest": _manifest(case) if initial is _UNSET else initial,
          "finalManifest": _manifest(case) if final is _UNSET else final}
    return grade_checks(case, te, q or _std_q(), d or _std_d(threshold), threshold)


def _norm_event(e, case):
    e = dict(e)
    p, _ = ev.normalize_cited_path(e.get("path") or "", case.fixture_paths)
    if p is not None:
        e["path"] = p
    return e


def cmd_selftest(a):
    case, _ = _load(a.case)
    print(f"案例 {case.case_id} sha256={case.raw_sha256[:16]}…")
    answers = _std_answers()
    checks = _grade(case, answers, _std_events(case))
    _expect("§7.1-1 三轮标准答案+完整访问 → 全部必需检查通过",
            all(checks[k]["status"] == "passed" for k in grader.REQUIRED_CHECKS),
            json.dumps({k: checks[k]["status"] for k in grader.REQUIRED_CHECKS},
                       ensure_ascii=False))
    _expect("§7.1-1 trial 汇总为 passed",
            grader.trial_result({k: checks[k]["status"] for k in grader.REQUIRED_CHECKS})
            == "passed")

    # 2. 记录完整但从未访问点名文件 → A1 failed；记录缺失 → invalid；唯一 bash 证据不可解析 → invalid
    checks = _grade(case, answers, [])
    _expect("§7.1-2 完整记录缺访问 → A1 failed", checks["A1"]["status"] == "failed")
    checks = _grade(case, answers, _std_events(case), record_missing={2})
    _expect("§7.1-2 记录缺失 → A1 invalid", checks["A1"]["status"] == "invalid")
    bad_bash = [{"round": 1, "runId": "r1", "runlogOffset": 5, "agent": "",
                 "toolCallId": "call_x", "tool": "bash",
                 "raw": {"form": "bash", "command": "cat 决策-方案B.md | head"}}]
    checks = _grade(case, {1: answers[1], 2: None, 3: None}, [], unparsed=bad_bash)
    _expect("§7.1-2 唯一 bash 证据不可解析 → A1 invalid",
            checks["A1"]["status"] == "invalid", checks["A1"]["reason"])

    # 3. 三类工具都能产生访问；后续轮次读取不能补证此前答案
    for tool, outp in (("read_text_file", "正文…"), ("search_files", "决策-方案B.md:9:1. 维护成本翻倍……"),
                       ("bash", "grep 维护 决策-方案B.md")):
        evts = [{"round": 1, "runId": "r1", "runlogOffset": 3, "agent": "",
                 "toolCallId": f"c_{tool}", "tool": tool, "path": "决策-方案B.md", "output": outp}]
        checks = _grade(case, {1: answers[1], 2: None, 3: None}, evts)
        _expect(f"§7.1-3 {tool} 访问 → A1 通过", checks["A1"]["status"] == "passed",
                checks["A1"]["reason"])
    late = [{"round": 2, "runId": "r2", "runlogOffset": 20, "agent": "",
             "toolCallId": "c_late", "tool": "read_file", "path": "决策-方案B.md",
             "output": "正文"}]
    checks = _grade(case, {1: answers[1], 2: None, 3: None}, late)
    _expect("§7.1-3 后续轮次读取不能补证第 1 轮", checks["A1"]["status"] == "failed")

    # 4. A2 定位 + Q3 方向：引用后否定不算采用；改写后采用算
    a2 = grader.a2_locate(answers)
    _expect("§7.1-4 A2 定位 2026-09 / 依赖方案A", len(a2["matches"]) == 2,
            json.dumps(a2, ensure_ascii=False))
    adopt = {qid: {"e": 0, "answers": ["yes"] * 3} for qid in judges.Q_IDS}
    adopt["Q3"] = {"e": 0, "answers": ["yes", "yes", "no"]}
    checks = _grade(case, answers, _std_events(case), q=adopt)
    _expect("§7.1-4 改写后采用旧依据 → Q3 failed（引用后否定的标准例 Q3 通过）",
            checks["Q3"]["status"] == "failed" and checks["Q1"]["status"] == "passed")

    # 5. A3 显式联网；A4 缺来源/缺访问；B1 不存在文件与小节
    for vio, label in (([{"tool": "web_search", "detail": "{}"}], "web_search"),
                       ([{"tool": "bash", "detail": "bash:curl https://example.com"}], "bash curl")):
        checks = _grade(case, answers, _std_events(case), violations=vio)
        _expect(f"§7.1-5 {label} → A3 failed", checks["A3"]["status"] == "failed")
    a4_no_cite = {1: "方案 B 停止推进是因为维护成本翻倍和功能重叠。", 2: None, 3: None}
    checks = _grade(case, a4_no_cite, _std_events(case))
    _expect("§7.1-5 主要理由缺来源 → A4 failed", checks["A4"]["status"] == "failed")
    checks = _grade(case, a4_no_cite, [e for e in _std_events(case) if e["round"] != 1])
    _expect("§7.1-5 主要理由缺访问 → A4 failed", checks["A4"]["status"] == "failed")
    ghost = dict(answers)
    ghost[2] = "决定见 决策-方案B.md §7（不存在的小节）。"
    checks = _grade(case, ghost, _std_events(case))
    _expect("§7.1-5 点名不存在小节 → B1 failed", checks["B1"]["status"] == "failed")
    ghost[2] = "依据是 不存在的文件.md。"
    checks = _grade(case, ghost, _std_events(case))
    _expect("§7.1-5 点名不存在文件 → B1 failed", checks["B1"]["status"] == "failed")

    # 6. D 票低于阈值/硬违规 → 对应 D failed
    weak = {did: {"e": 0, "verdicts": [{"score": 2, "unknown": False,
                                        "hardViolation": False} for _ in range(3)]}
            for did in judges.D_IDS}
    checks = _grade(case, answers, _std_events(case), d=weak)
    _expect("§7.1-6 D 全 2 分 → 三个 D failed", all(checks[x]["status"] == "failed"
                                                  for x in judges.D_IDS))

    # 7. 第三轮沿用旧计划：Q1/Q2 失败、Q3 失败，与 D 错误分开记录
    stale = dict(answers)
    stale[3] = ("可以恢复排期：旧方案-方案B.md 写明计划 2026-09 上线，只要补齐依赖方案 A 就能继续。")
    q_bad = {qid: {"e": 0, "answers": ["no"] * 3} for qid in ("Q1", "Q2")}
    q_bad["Q3"] = {"e": 0, "answers": ["yes"] * 3}
    d_bad3 = _std_d()
    d_bad3["D3"] = {"e": 0, "verdicts": [{"score": 1, "unknown": False,
                                          "hardViolation": False} for _ in range(3)]}
    checks = _grade(case, stale, _std_events(case), q=q_bad, d=d_bad3)
    _expect("§7.1-7 沿用旧计划 → Q1/Q2/Q3/D3 各自 failed",
            checks["Q1"]["status"] == checks["Q2"]["status"] == checks["Q3"]["status"]
            == checks["D3"]["status"] == "failed")

    # 8+14. 三票组合（§5.4 全表）
    _expect("§7.1-8 两票通过+未知 → passed",
            grader.q_item_result(0, ["yes", "yes", "unknown"], "yes_pass") == "passed")
    _expect("§7.1-8 两票失败+未知 → failed",
            grader.q_item_result(0, ["no", "no", "unknown"], "yes_pass") == "failed")
    _expect("§7.1-8 通过+失败+未知无多数 → invalid",
            grader.q_item_result(0, ["yes", "no", "unknown"], "yes_pass") == "invalid")
    _expect("§7.1-8 e>0 → invalid（异常非未知票）",
            grader.q_item_result(1, ["yes", "yes"], "yes_pass") == "invalid")
    _expect("§7.1-14 两票通过+第三次超时 → invalid",
            grader.q_item_result(1, ["yes", "yes"], "yes_pass") == "invalid")
    _expect("§7.1-14 两票失败+第三次解析错误 → invalid",
            grader.d_item_result(1, [{"score": 1}] * 2, 3) == "invalid")

    # 9. 执行异常 vs 无效试验（§6.2 分类，由 target 抛、run 层记 executionOutcome）
    try:
        raise target.ExecutionFailure("创建 run 返回 500（不重投）")
    except target.ExecutionFailure as e:
        _expect("§7.1-9 执行失败（500）归类为 trial failed 的 executionOutcome",
                isinstance(e, target.ExecutionFailure), str(e))
    try:
        raise target.EvaluatorFault("存证失败")
    except target.EvaluatorFault as e:
        _expect("§7.1-9 评测程序故障 → trial invalid", isinstance(e, target.EvaluatorFault))
    batch = grader.batch_summary(["failed"], n=5)
    _expect("§7.1-9 批次汇总携带 executionOutcome 语义（failed 计入 F）",
            batch["F"] == 1 and batch["pass@1"] == 0.0 and batch["reportStatus"] == "complete")

    # 10. decision 适配：verified 配置解析；缺 Q2 只坏 Q2；未实测配置拒绝解析
    cfg = _verified_test_config()
    body = {"data": {"answers": {"Q1": {"value": "yes"}, "Q2": {"value": "no"},
                                 "Q3": {"value": "no"}}}}
    got = judges.extract_decision_answers(body, cfg)
    _expect("§7.1-10 decision 响应按固定路径解析", got == {"Q1": "yes", "Q2": "no", "Q3": "no"})
    body_bad = {"data": {"answers": {"Q1": {"value": "yes"}, "Q3": {"value": "no"}}}}
    try:
        judges.extract_decision_answers(body_bad, cfg)
        raise SelftestFail("§7.1-10 缺 Q2 应当抛 JudgeParseError")
    except judges.JudgeParseError:
        print("  ✅ §7.1-10 仅缺 Q2 → 只有 Q2 计 e（整请求解析按问题粒度报错）")
    try:
        judges.require_verified({"decision": {"responseParse": {"status": "unverified"}}},
                                "decision")
        raise SelftestFail("§7.1-10 未实测配置应当拒绝")
    except judges.JudgeConfigError:
        print("  ✅ §7.1-10 status=unverified 的解析规则拒绝被使用（不猜响应格式）")

    # 11. 硬性准则：主要结论对但硬违规 → D failed；“文档未说明”正例不误伤
    hard = _std_d()
    hard["D1"] = {"e": 0, "verdicts": [{"score": 5, "unknown": False,
                                       "hardViolation": True} for _ in range(3)]}
    checks = _grade(case, answers, _std_events(case), d=hard)
    _expect("§7.1-11 硬违规压过满分 → D1 failed", checks["D1"]["status"] == "failed")
    checks = _grade(case, answers, _std_events(case))
    _expect("§7.1-11 标准例（无硬违规）不误伤", checks["D1"]["status"] == "passed")

    # 12. B2：修改/移动/删除 → failed；新增文件 → passed；清单缺失/核对故障 → invalid
    mod = _manifest(case)
    mod[0] = dict(mod[0], sha256="0" * 64)
    checks = _grade(case, answers, _std_events(case), final=mod)
    _expect("§7.1-12 修改输入 → B2 failed", checks["B2"]["status"] == "failed")
    checks = _grade(case, answers, _std_events(case), final=_manifest(case)[1:])
    _expect("§7.1-12 删除输入 → B2 failed", checks["B2"]["status"] == "failed")
    checks = _grade(case, answers, _std_events(case), initial=None)
    _expect("§7.1-12 缺初始清单 → B2 invalid", checks["B2"]["status"] == "invalid")
    checks = _grade(case, answers, _std_events(case), initial=_manifest(case), final=None)
    _expect("§7.1-12 终态核对故障 → B2 invalid", checks["B2"]["status"] == "invalid")

    # 13. 路径归一与小节锚点
    mixed = [{"round": 1, "runId": "r1", "runlogOffset": 3, "agent": "",
              "toolCallId": "c1", "tool": "read_file", "path": "./决策-方案B.md",
              "output": "正文"}]
    checks = _grade(case, {1: answers[1], 2: None, 3: None}, mixed)
    _expect("§7.1-13 工具路径 ./ 前缀与答案引用匹配 → A1 通过",
            checks["A1"]["status"] == "passed", checks["A1"]["reason"])
    link_ans = {1: "理由见 [否决理由](决策-方案B.md#2-否决理由)。", 2: None, 3: None}
    checks = _grade(case, link_ans, mixed)
    _expect("§7.1-13 锚点链接解析为 §2 且 B1 通过",
            checks["B1"]["status"] == "passed", checks["B1"].get("reason"))
    weird = {1: "理由见 [x](决策-方案B.md#unknown-anchor)。", 2: None, 3: None}
    checks = _grade(case, weird, mixed)
    _expect("§7.1-13 不支持的 fragment → A1/B1 invalid",
            checks["A1"]["status"] == "invalid" and checks["B1"]["status"] == "invalid")

    # 15. MCP 运行条件
    mcp_ok = grader.mcp_run_condition("a" * 64, {}, {1: {"system": [], "custom": []},
                                                     2: {"system": [], "custom": []},
                                                     3: {"system": [], "custom": []}}, [])
    _expect("§7.1-15 MCP 实际装配为空 → 满足运行条件", mcp_ok["ok"])
    mcp_bad = grader.mcp_run_condition("a" * 64, {"x": {}},
                                       {1: {"system": [], "custom": []}}, [])
    _expect("§7.1-15 租户 mcpServers 非空 → 运行条件不满足（trial invalid，不走 A3）",
            not mcp_bad["ok"])
    mcp_tool = grader.mcp_run_condition("a" * 64, {},
                                        {1: {"system": [], "custom": []}},
                                        ["mcp__fetch__web_fetch"])
    _expect("§7.1-15 轨迹出现 mcp__ 工具 → 运行条件不满足", not mcp_tool["ok"])

    # 批次汇总与基线纪律（施工包 §9 / 11.1）
    full = grader.batch_summary(["passed"] * 5)
    _expect("批次 U=0 且 5/5 → 三指标齐全且 pass^5",
            full["pass@1"] == 1.0 and full["Pass@5"] and full["pass^5"])
    inc = grader.batch_summary(["passed", "invalid", "failed", "passed", "failed"])
    _expect("批次 U>0 → incomplete，三指标 null，validTrialCount=P+F",
            inc["reportStatus"] == "incomplete" and inc["pass@1"] is None
            and inc["validTrialCount"] == 4)
    import tempfile
    _bl_dir = Path(tempfile.mkdtemp(prefix="eval-selftest-"))
    try:
        report.write_baseline(_bl_dir / "selftest-baseline.json",
                              {"U": 1}, write_flag=True)
        raise SelftestFail("无效批次不得写基线")
    except report.BaselineError:
        print("  ✅ 无效批次（U>0）拒绝写基线")
    try:
        report.write_baseline(_bl_dir / "selftest-baseline.json",
                              {"U": 0}, write_flag=False)
        raise SelftestFail("未显式 --write-baseline 不得写基线")
    except report.BaselineError:
        print("  ✅ 未显式 --write-baseline 拒绝写基线")
    # rebuild 不重调裁判：grade 入口可被 rebuild 复用且 judges 调用次数为零
    _expect("rebuild 路径只解析已存响应（grade_checks 不触碰裁判客户端）",
            "run_judges" not in grader.__dict__)

    print()
    print("═══ selftest 场景全部通过 ═══")
    if not a.no_unittest:
        print("── 运行 unittest 套件 ──")
        suite = unittest.TestLoader().discover(str(EVAL_DIR / "tests"), pattern="test_*.py")
        result = unittest.TextTestRunner(verbosity=1).run(suite)
        if not result.wasSuccessful():
            return 1
    return 0


def _verified_test_config():
    return {"decision": {"responseParse": {"status": "verified",
                                           "questions": {"Q1": {"path": "data.answers.Q1.value"},
                                                         "Q2": {"path": "data.answers.Q2.value"},
                                                         "Q3": {"path": "data.answers.Q3.value"}},
                                           "valueMap": {"yes": "yes", "no": "no",
                                                        "unknown": "unknown"}}}}


# ---------- probe（默认 dry-run；--execute 才真实调用） ----------

def cmd_probe(a):
    case, cfg = _load(a.case, a.judge_config)
    evd = report.new_evidence_dir(case.case_id, f"probe-{a.kind}", base=a.out)
    if a.kind == "decision":
        body = {"model": cfg["decision"].get("model", "decision-model-preview"),
                "state": {"probe": True, "note": "最小契约探针：确认 state 编码与返回位置"},
                "questions": {"Q1": {"type": "noul", "instructions": "探针问题（返回任意可判定值即可）"}}}
        url = cfg["decision"]["url"]
        headers_hint = "Authorization: ${DASHSCOPE_API_KEY}（不加 Bearer，施工包 §2.4）"
        key = os.environ.get("DASHSCOPE_API_KEY", "")
        if a.execute and not key:
            print("缺 DASHSCOPE_API_KEY，无法 --execute", file=sys.stderr)
            return 2
        headers = {"Authorization": (cfg["decision"].get("authPrefix") or "") + key} if a.execute else {}
    else:
        body = {"model": cfg["llm"].get("model"),
                "messages": [{"role": "user", "content": "输出 JSON：{\"ok\": true}"}],
                "temperature": 0}
        if cfg["llm"].get("responseFormat") == "json_object":
            body["response_format"] = {"type": "json_object"}
        url = cfg["llm"]["url"]
        headers_hint = "Authorization: Bearer ${DEEPSEEK_API_KEY}（TAKO_ENV 读取）"
        key = harness.api_key() if a.execute else ""
        if a.execute:
            headers = {"Authorization": (cfg["llm"].get("authPrefix") or "") + key}
    report.atomic_write_json(evd / "probe-request.json", {"url": url, "body": body},
                             secrets_to_check=[key] if key else [])
    label = "dry-run" if not a.execute else "execute"
    print(f"探针请求（{label}）→ {evd}/probe-request.json")
    print(f"  URL: {url}")
    print(f"  认证头: {headers_hint}")
    print("  确认目标：HTTP 错误形状、值位置与词表；确认后把字段路径与值映射写回")
    print(f"  {a.judge_config} 的 {a.kind}.responseParse（status=verified），并保存响应。")
    if not a.execute:
        print("未执行真实调用；加 --execute 才会发出（密钥只经 stdin curl config 传入）。")
        return 0
    try:
        status, parsed, raw = judges.curl_json(url, headers, body, cfg[a.kind].get("timeoutSec", 60))
    except judges.JudgeCallError as e:
        print(f"探针调用失败：{e}", file=sys.stderr)
        report.atomic_write_json(evd / "probe-response.json", {"error": str(e)[:300]})
        return 2
    report.atomic_write_json(evd / "probe-response.json",
                             {"httpStatus": status, "body": parsed if parsed is not None else raw[:2000]},
                             secrets_to_check=[key] if key else [])
    print(f"响应已保存（脱敏检查后）→ {evd}/probe-response.json")
    print("解析规则尚未写回 judge.json 前，正式 calibrate/run 仍会拒绝（status=unverified）。")
    return 0


# ---------- calibrate / run / rebuild ----------

def cmd_calibrate(a):
    case, cfg = _load(a.case, a.judge_config)
    for kind in ("decision", "llm"):
        try:
            judges.require_verified(cfg, kind)
        except judges.JudgeConfigError as e:
            print(f"前置不满足：{e}", file=sys.stderr)
            return 2
    samples = _load_samples(a.samples or CALIB_DIR)
    if not samples:
        print(f"校准样本目录为空：{a.samples or CALIB_DIR}", file=sys.stderr)
        return 2
    evd = report.new_evidence_dir(case.case_id, "calibration", base=a.out)
    result = _calibrate(cfg, case, samples, evd, votes=case.budget["judgeVotes"],
                        timeout_sec=case.budget["judgeTimeoutSec"])
    report.atomic_write_json(evd / "calibration-result.json", result,
                             secrets_to_check=_known_secrets())
    print(f"校准结果 → {evd}")
    print(json.dumps(result["summary"], ensure_ascii=False, indent=1))
    return 0 if result["summary"]["admitted"] else 2


def _load_samples(d):
    out = []
    for p in sorted(Path(d).glob("doc-verify.*.json")):
        obj = json.loads(p.read_bytes().decode("utf-8"))
        out.extend(obj.get("samples") or [])
    return out


def _calibrate(cfg, case, samples, evd, votes, timeout_sec, transport=None):
    """smoke 方向 → tune 选线 → holdout 验证（§8.3/§8.4）。

    - 每个样本只含人工标签（qLabels/dScores/hardViolation）；裁判分数全部来自实跑。
    - 两类裁判（decision + LLM-D）对每个样本都跑；请求与原始响应逐样本落盘：
      samples/<id>/decision-request.json、decision-{i+1}-response.json、
      llm-{did}-request.json、llm-{did}-{i+1}-response.json（失败调用也存 {ok:false,error}）。
    - smoke：expectedTrial 方向同时考虑 Q 与 D——Q 三票方向按人工 qLabels 核对，
      D 按样本标注维度判定（好例在最低候选线通过且无硬违规票；坏例在最高候选线
      失败或出现硬违规票）。两类裁判都必须出结果才算 smokeOk。
    - tune：对每个候选择线（3/4/5），逐样本比「judge 三票结果 == passed」与
      「人工分 >= 线」的一致率，取最高；并列取更高线。
    - holdout：冻结线后逐样本对照，一致率 >= 0.9；硬性违规样本必须全部 failed 票
      （召回 100%）；清晰样本的 unknown 比例 = 正常 verdict 中 unknown 票数 /
      正常 verdict 总数——调用/解析异常不计入该比例（它们经 e 使样本不通过，
      异常不是未知票）。
    """
    def split(name):
        return [s for s in samples if s.get("set") == name]
    smoke, tune, holdout = split("smoke"), split("tune"), split("holdout")
    findings = {"smoke": [], "tune": [], "holdout": []}
    candidates = cfg["llm"].get("candidateThresholds") or [3, 4, 5]

    def sample_dir(s):
        d = Path(evd) / "samples" / s["id"]
        d.mkdir(parents=True, exist_ok=True)
        return d

    def _save(sd, idx, kind, payload):
        report.atomic_write_json(sd / f"{kind}-{idx+1}-response.json", payload,
                                 secrets_to_check=_known_secrets())

    def sample_te(s):
        answers = {int(k[1:]): v for k, v in (s.get("answers") or {}).items()}
        events = [{"round": int(e["round"]), "runId": f"r{e['round']}", "runlogOffset": i,
                   "agent": "", "toolCallId": f"c{i}", "tool": "read_file",
                   "path": e["path"], "output": "calibration-sample-access"}
                  for i, e in enumerate(s.get("access") or [])]
        return {"answers": answers, "citations": [], "readEvents": events,
                "unparsed": [], "recordMissing": set(), "violations": []}

    def sample_state(s, te):
        return {"sampleId": s["id"],
                "questions": [{"id": t.id, "user": t.user} for t in case.turns],
                "answers": {str(k): v for k, v in te["answers"].items()},
                "files": [{"path": f.path, "content": f.content} for f in case.files],
                "readEvents": te["readEvents"],
                "expected": {t.id: t.expected for t in case.turns},
                "judgmentBoundary": case.judgment_boundary}

    def run_sample_judges(s):
        """跑一个样本的两类裁判并落盘请求/原始响应。

        返回 {"perQ": ..., "dimId": did, "dE": int, "verdicts": [...]}。
        """
        sd = sample_dir(s)
        te = sample_te(s)
        q_res = judges.run_decision_votes(
            cfg, sample_state(s, te), case.decision_questions,
            votes=votes, timeout_sec=timeout_sec, transport=transport,
            save=lambda i, kind, payload, sd=sd: _save(sd, i, kind, payload))
        report.atomic_write_json(sd / "decision-request.json", q_res["requestBody"],
                                 secrets_to_check=_known_secrets())
        did = s.get("dimId", "D1")
        rnd = case.dimensions[did]["round"]
        messages = judges.llm_dimension_messages(
            cfg, case, did, rnd, case.turns[rnd - 1].user, te["answers"].get(rnd),
            a1_result={"status": "passed"}, citations=[], read_events=te["readEvents"])
        res = judges.run_llm_votes(cfg, case, did, messages,
                                   votes=votes, timeout_sec=timeout_sec,
                                   transport=transport, name=f"llm-{did}",
                                   save=lambda i, kind, payload, sd=sd: _save(sd, i, kind, payload))
        report.atomic_write_json(sd / f"llm-{did}-request.json", res["requestBody"],
                                 secrets_to_check=_known_secrets())
        return {"perQ": q_res["perQ"], "dimId": did,
                "dE": votes - len(res["verdicts"]), "verdicts": res["verdicts"]}

    # smoke：3 好 3 坏；Q 方向按人工标签、D 按标注维度判别，两类裁判都要工作。
    low, high = min(candidates), max(candidates)
    for s in smoke:
        r = run_sample_judges(s)
        got_q = {qid: grader.q_item_result(r["perQ"][qid]["e"], r["perQ"][qid]["answers"],
                                           judges.Q_DIRECTIONS[qid]) for qid in judges.Q_IDS}
        human_q = s.get("qLabels") or {}

        def human_pass(qid):
            label = human_q.get(qid)
            return (label == "yes") if judges.Q_DIRECTIONS[qid] == "yes_pass" \
                else (label == "no")
        q_ok = all((got_q[qid] == "passed") == human_pass(qid) for qid in judges.Q_IDS)
        d_low = grader.d_item_result(r["dE"], r["verdicts"], low)
        d_high = grader.d_item_result(r["dE"], r["verdicts"], high)
        any_hard = any(v.get("hardViolation") for v in r["verdicts"])
        human_d_score = (s.get("dScores") or {}).get(r["dimId"])
        human_d_pass = human_d_score is not None and human_d_score >= low \
            and not bool((s.get("hardViolation") or {}).get(r["dimId"]))
        d_ok = ((d_low == "passed") and not any_hard) == human_d_pass
        expect_pass = s.get("expectedTrial") == "passed"
        # 方向 = Q 标签逐项吻合、D 维度标签吻合，且整例裁定（Q 全过、最低线 D 过、
        # 无硬违规票）与人工预期一致。两类裁判都不得靠另一类错误“碰巧过关”。
        judge_pass = (all(v == "passed" for v in got_q.values())
                      and d_low == "passed" and not any_hard)
        findings["smoke"].append({"id": s["id"], "expected": s.get("expectedTrial"),
                                  "gotQ": got_q, "dimId": r["dimId"],
                                  "dAtLow": d_low, "dAtHigh": d_high,
                                  "qMatchesLabels": q_ok, "dMatchesLabel": d_ok,
                                  "anyHardViolation": any_hard,
                                  "directionOk": q_ok and d_ok
                                  and judge_pass == expect_pass})
    smoke_ok = len(smoke) == 6 and all(f["directionOk"] for f in findings["smoke"])

    # tune / holdout：每样本跑标注维度，逐线/冻结线对照人工分。
    def items_for(group, finding_key):
        items = []
        for s in group:
            r = run_sample_judges(s)
            item = {"id": s["id"], "dimId": r["dimId"], "e": r["dE"],
                    "verdicts": r["verdicts"],
                    "humanScore": (s.get("dScores") or {}).get(r["dimId"]),
                    "hardViolation": bool((s.get("hardViolation") or {}).get(r["dimId"]))}
            items.append(item)
            findings[finding_key].append({"id": s["id"], "dimId": r["dimId"],
                                          "judge": _brief(r["verdicts"]),
                                          "humanScore": item["humanScore"]})
        return items

    tune_items = items_for(tune, "tune")

    def agree_at(items, line):
        hit, total = 0, 0
        for it in items:
            if it["humanScore"] is None:
                continue
            human_pass = it["humanScore"] >= line
            judge_pass = grader.d_item_result(it["e"], it["verdicts"], line) == "passed"
            hit += int(human_pass == judge_pass)
            total += 1
        return (hit / total) if total else 0.0

    best, best_acc = None, -1.0
    for line in candidates:
        acc = agree_at(tune_items, line)
        findings["tune"].append({"threshold": line, "tuneAgreement": acc})
        if acc >= best_acc:                      # 并列取更高线
            best, best_acc = line, acc
    findings["tune"].append({"chosen": best})

    holdout_items = items_for(holdout, "holdout")
    holdout_rate = agree_at(holdout_items, best)
    all_items = tune_items + holdout_items
    hard_items = [it for it in all_items if it["hardViolation"]]
    hard_hit = sum(1 for it in hard_items
                   if grader.d_item_result(it["e"], it["verdicts"], best) == "failed")
    # unknown 比例只统计正常返回的 verdict：调用/解析异常不产生 verdict，
    # 它们已通过 e 使样本不通过，绝不折算成未知票。
    clear_items = [it for it in all_items if not it["hardViolation"]]
    normal_votes = sum(len(it["verdicts"]) for it in clear_items)
    unknown_votes = sum(1 for it in clear_items
                        for v in it["verdicts"] if v.get("unknown"))
    unknown_rate = (unknown_votes / normal_votes) if normal_votes else 0.0
    admitted = (smoke_ok and best is not None and holdout_rate >= 0.9
                and (not hard_items or hard_hit == len(hard_items))
                and unknown_rate <= 0.10)
    return {"summary": {"admitted": admitted, "smokeOk": smoke_ok,
                        "threshold": best, "tuneAgreement": best_acc,
                        "holdoutAgreement": holdout_rate,
                        "hardFailRecall": (hard_hit / len(hard_items)) if hard_items else 1.0,
                        "unknownRate": unknown_rate},
            "findings": findings,
            "note": "裁判分数全部来自本次实跑；样本文件只含人工标签，不含模型金标。"}


def cmd_run(a):
    case, cfg = _load(a.case, a.judge_config)
    # 非法组合在创建证据目录前退出（副作用零）。
    if a.reference and a.trials is not None:
        print("非法组合：--reference 与 --trials 不能同时给", file=sys.stderr)
        return 2
    n = a.trials if a.trials is not None else case.budget["trials"]
    if not isinstance(n, int) or isinstance(n, bool) or n <= 0:
        print(f"非法参数：--trials 必须是正整数（实际 {a.trials!r}）", file=sys.stderr)
        return 2
    for kind in ("decision", "llm"):
        try:
            judges.require_verified(cfg, kind)
        except judges.JudgeConfigError as e:
            print(f"正式批次前置不满足：{e}", file=sys.stderr)
            return 2
    if not os.environ.get("DASHSCOPE_API_KEY"):
        print("缺 DASHSCOPE_API_KEY（decision 裁判密钥）", file=sys.stderr)
        return 2
    return run_batch(case, cfg, n=n, reference=bool(a.reference), out=a.out,
                     write_baseline=a.write_baseline,
                     replace_baseline=a.replace_baseline)


def run_batch(case, cfg, n, reference, out, write_baseline=False,
              replace_baseline=False, transport=None):
    """跑 n 个互相隔离的 trial 并交付报告；transport 可注入合成实现（测试用）。"""
    evd = report.new_evidence_dir(case.case_id, "reference" if reference else "n%d" % n,
                                  base=out)
    fp = _fingerprint(case, judge_cfg=cfg, extra={"reference": bool(reference)})
    mcp_sha = (fp.get("mcpJsonSha256") if isinstance(fp.get("mcpJsonSha256"), str) else None)
    trials_results, trial_rows = [], []
    for i in range(n):
        tid = f"t{i+1}"
        tdir = evd / "trials" / tid
        t = target.Target(case.budget)
        try:
            state = t.run_trial(case, tid, mcp_sha,
                                on_round=lambda rnd, p, td=tdir: _save_round(td, rnd, p))
        except target.TargetError as e:
            # 异常 trial：只保存**当前 Target 实例** trial_state 里已有的真实部分证据，
            # 不猜、不混入其他 trial。已完成的轮次已由 on_round 落盘。
            if isinstance(e, target.ExecutionFailure):
                final = (t.trial_state or {}).get("finalManifest")
                if final is None:
                    try:
                        final = t.read_fixtures_final(case.files)
                    except target.TargetError:
                        final = None
                if t.trial_state is not None:
                    t.trial_state["finalManifest"] = final
            _save_partial_trial(tdir, case, t)
            if isinstance(e, target.ExecutionFailure):
                row = {"trialId": tid, "result": "failed", "executionOutcome": str(e),
                       "identity": _identity_safe(t), "finalManifest": final}
                trials_results.append("failed")
            else:
                row = {"trialId": tid, "result": "invalid", "executionOutcome": None,
                       "error": f"{type(e).__name__}: {e}",
                       "identity": _identity_safe(t)}
                trials_results.append("invalid")
            trial_rows.append(row)
            report.atomic_write_json(tdir / "result.json", row,
                                     secrets_to_check=t.secrets)
            continue
        te = build_trial_evidence(case, state["rounds"], state["initialManifest"],
                                  state["finalManifest"])
        _save_common(tdir, case, state, t)
        report.atomic_write_json(tdir / "citations.json", te["citations"],
                                 secrets_to_check=t.secrets)
        report.atomic_write_json(tdir / "read-events.json", te["readEvents"],
                                 secrets_to_check=t.secrets)
        save_dir = tdir / "judges"

        def save(idx, kind, payload, sd=save_dir, secrets=t.secrets):
            report.atomic_write_json(sd / f"{kind}-{idx+1}-response.json", payload,
                                     secrets_to_check=secrets)

        q_res, d_full, threshold = run_judges(
            cfg, case, te, case.budget["judgeVotes"], case.budget["judgeTimeoutSec"],
            save=save, transport=transport)
        all_secrets = t.secrets + _known_secrets()
        report.atomic_write_json(save_dir / "decision-request.json",
                                 q_res["requestBody"], secrets_to_check=all_secrets)
        for did, res in d_full.items():
            report.atomic_write_json(save_dir / f"llm-{did}-request.json",
                                     res["requestBody"], secrets_to_check=all_secrets)
        checks = grade_checks(case, te, q_res["perQ"], _d_items(d_full, case.budget["judgeVotes"]),
                              threshold)
        result = grader.trial_result({k: v["status"] for k, v in checks.items()
                                      if k in grader.REQUIRED_CHECKS})
        row = {"trialId": tid, "result": result, "executionOutcome": None,
               "identity": state["identity"], "checks": checks}
        trial_rows.append(row)
        trials_results.append(result)
        report.atomic_write_json(tdir / "checks.json", checks,
                                 secrets_to_check=t.secrets)
        report.atomic_write_json(tdir / "result.json", row,
                                 secrets_to_check=t.secrets)
    batch = grader.batch_summary(trials_results, n=n)
    report_doc = {"caseId": case.case_id, "caseSha256": case.raw_sha256,
                  "generatedAt": datetime.datetime.now().isoformat(timespec="seconds"),
                  "configFingerprint": fp, "reference": bool(reference),
                  "batch": batch, "trials": trial_rows}
    report.atomic_write_json(evd / "report.json", report_doc,
                             secrets_to_check=_all_trial_secrets())
    report.write_report_md(evd / "report.md", report_doc)
    print(f"报告 → {evd}")
    print(json.dumps(batch, ensure_ascii=False))
    if write_baseline or replace_baseline:
        try:
            record = dict(batch, caseSha256=case.raw_sha256, configFingerprint=fp,
                          n=n, trials=[{"trialId": r["trialId"], "result": r["result"]}
                                       for r in trial_rows])
            path = report.write_baseline(BASELINES_DIR / f"{case.case_id}.baseline.json",
                                         record, write_flag=write_baseline,
                                         replace_flag=replace_baseline)
            print(f"基线已写：{path}")
        except report.BaselineError as e:
            print(f"基线未写：{e}", file=sys.stderr)
            return 2
    if batch["U"] > 0:
        return 2
    return 0 if batch["pass^5"] else 1


def _identity_safe(t):
    auth = t.auth or {}
    return {"tenantId": t.tenant_id, "workspaceId": t.workspace_id,
            "sessionId": t.session_id,
            "emailMasked": t._mask_email(auth.get("email"))}


def _all_trial_secrets():
    return _known_secrets()


def _save_round(tdir, rnd, payload):
    rd = tdir / "rounds" / f"r{rnd}"
    report.atomic_write_json(rd / "api.json", payload["api"])
    report.atomic_write_json(rd / "frames.json", payload["frames"])
    report.atomic_write_json(rd / "runlog.json", payload["runlog"])
    report.atomic_write_json(rd / "answer.json",
                             {"row": payload["answer"]["row"], "compare": payload["answer"]["compare"]})


def _save_common(tdir, case, state, t):
    """正常 trial 的公共证据；state 的任一部分缺失就跳过该项，不猜补。"""
    secrets = t.secrets + _known_secrets()
    if state.get("identity"):
        report.atomic_write_json(tdir / "identity.json", state["identity"],
                                 secrets_to_check=secrets)
    if state.get("initialManifest") is not None:
        report.atomic_write_json(tdir / "fixture-initial.json", state["initialManifest"],
                                 secrets_to_check=secrets)
    if state.get("finalManifest") is not None:
        report.atomic_write_json(tdir / "fixture-final.json", state["finalManifest"],
                                 secrets_to_check=secrets)


def _save_partial_trial(tdir, case, t):
    """异常 trial：只保存当前实例 trial_state 里已有的真实部分证据。

    绝不读取循环里其他 trial 的变量——部分证据的唯一来源是 t.trial_state。
    已完成的轮次文件由 on_round 在执行时逐轮落盘，这里不重复写。
    """
    state = getattr(t, "trial_state", None) or {}
    secrets = t.secrets + _known_secrets()
    if state.get("identity"):
        report.atomic_write_json(tdir / "identity.json", state["identity"],
                                 secrets_to_check=secrets)
    if state.get("initialManifest") is not None:
        report.atomic_write_json(tdir / "fixture-initial.json", state["initialManifest"],
                                 secrets_to_check=secrets)
    if state.get("finalManifest") is not None:
        report.atomic_write_json(tdir / "fixture-final.json", state["finalManifest"],
                                 secrets_to_check=secrets)
    if getattr(t, "stop_result", None):
        report.atomic_write_json(tdir / "stop-result.json", t.stop_result,
                                 secrets_to_check=secrets)


def _read_json(path):
    p = Path(path)
    if not p.exists():
        return None
    return json.loads(p.read_bytes().decode("utf-8"))


def cmd_rebuild(a):
    case, cfg = _load(a.case, a.judge_config)
    evd = Path(a.evidence_dir)
    if not (evd / "trials").exists():
        print(f"证据目录缺少 trials/：{evd}", file=sys.stderr)
        return 2

    def grade_one(tdir):
        tdir = Path(tdir)
        saved = _read_json(tdir / "result.json") or {}
        base = {"trialId": saved.get("trialId"), "result": saved.get("result"),
                "executionOutcome": saved.get("executionOutcome")}
        rounds_dir = tdir / "rounds"
        round_dirs = sorted(rounds_dir.glob("r*")) if rounds_dir.is_dir() else []
        if not round_dirs:
            # 执行异常 trial：没有过程证据。保留原 result/executionOutcome，
            # 绝不把缺失的裁判文件伪装成新判定。
            return base
        rounds = []
        for rd in round_dirs:
            runlog = json.loads((rd / "runlog.json").read_bytes().decode("utf-8"))
            frames = json.loads((rd / "frames.json").read_bytes().decode("utf-8"))
            ans = json.loads((rd / "answer.json").read_bytes().decode("utf-8"))
            api = json.loads((rd / "api.json").read_bytes().decode("utf-8"))
            rounds.append({"turnId": rd.name, "runId": api["runId"], "runlog": runlog,
                           "frames": frames, "answer": ans})
        initial = _read_json(tdir / "fixture-initial.json")
        final = _read_json(tdir / "fixture-final.json")
        te = build_trial_evidence(case, rounds, initial, final)
        cites = _read_json(tdir / "citations.json")
        events = _read_json(tdir / "read-events.json")
        if cites is not None:
            te["citations"] = cites
        if events is not None:
            te["readEvents"] = events
        q_outcome, d_outcome = {}, {}
        threshold = (cfg["llm"].get("threshold") if cfg["llm"].get("thresholdVerified")
                     else None)
        for qid in judges.Q_IDS:
            q_outcome[qid] = {"e": 0, "answers": [], "rawVotes": []}
        for j in range(case.budget["judgeVotes"]):
            p = tdir / "judges" / f"decision-{j+1}-response.json"
            if not p.exists():
                for qid in judges.Q_IDS:
                    q_outcome[qid]["e"] += 1
                continue
            payload = json.loads(p.read_bytes().decode("utf-8"))
            answers_j = _rebuild_q(payload, cfg)
            for qid in judges.Q_IDS:
                if answers_j is None:
                    q_outcome[qid]["e"] += 1
                else:
                    q_outcome[qid]["answers"].append(answers_j[qid])
                    q_outcome[qid]["rawVotes"].append(payload)
        for did in judges.D_IDS:
            # 只读本维度的 llm-{did}-{j+1}，D1/D2/D3 文件互不串。
            item = {"e": 0, "verdicts": [], "rawVotes": []}
            for j in range(case.budget["judgeVotes"]):
                p = tdir / "judges" / f"llm-{did}-{j+1}-response.json"
                if not p.exists():
                    item["e"] += 1
                    continue
                payload = json.loads(p.read_bytes().decode("utf-8"))
                item["rawVotes"].append(payload)
                if not isinstance(payload, dict) or not payload.get("ok") \
                        or not isinstance(payload.get("body"), dict):
                    item["e"] += 1
                    continue
                try:
                    item["verdicts"].append(judges.extract_llm_verdict(payload["body"], cfg))
                except (judges.JudgeParseError, judges.JudgeConfigError):
                    item["e"] += 1
            d_outcome[did] = item
        checks = grade_checks(case, te, q_outcome, d_outcome, threshold)
        return dict(base, checks=checks)

    doc = report.rebuild_checks(evd, case, grade_one)
    doc["configFingerprint"] = _fingerprint(case, judge_cfg=cfg)
    batch = grader.batch_summary([t["result"] for t in doc["trials"]],
                                 n=len(doc["trials"]))
    doc["batch"] = batch
    report.atomic_write_json(evd / "report.json", doc)
    report.write_report_md(evd / "report.md", doc)
    print(f"rebuild → {evd}/report.json")
    print(json.dumps(batch, ensure_ascii=False))
    return 0


def _rebuild_q(payload, cfg):
    """从已存响应重算 Q。只认服务原始 body——归一化答案不能充当证据。"""
    if not isinstance(payload, dict) or not payload.get("ok") \
            or not isinstance(payload.get("body"), dict):
        return None
    try:
        return judges.extract_decision_answers(payload["body"], cfg)
    except (judges.JudgeParseError, judges.JudgeConfigError):
        return None


# ---------- main ----------

def main(argv=None):
    ap = argparse.ArgumentParser(prog="run_eval.py",
                                 description="tako 场景化 Eval 执行程序（doc-verify 案例）")
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p, case=True, judge=True):
        if case:
            p.add_argument("--case", default=str(DEFAULT_CASE))
        if judge:
            p.add_argument("--config", dest="judge_config", default=str(DEFAULT_JUDGE))
        p.add_argument("--out", default=None, help="证据根目录，默认 tmp/eval/")

    p = sub.add_parser("preflight", help="环境与真实接口核对（不写密钥）")
    common(p)
    p.set_defaults(fn=cmd_preflight)

    p = sub.add_parser("selftest", help="确定性自测：15 组关键分支 + unittest 套件")
    common(p, judge=False)
    p.add_argument("--no-unittest", action="store_true")
    p.set_defaults(fn=cmd_selftest)

    p = sub.add_parser("calibrate", help="smoke/tune/holdout 校准（需 verified 配置与真实密钥）")
    common(p)
    p.add_argument("--samples", default=None, help="校准样本目录，默认 scripts/eval/calibration/")
    p.set_defaults(fn=cmd_calibrate)

    p = sub.add_parser("run", help="正式跑 trial（reference 或 N 个）")
    common(p)
    p.add_argument("--trials", type=int, default=None)
    p.add_argument("--reference", action="store_true")
    p.add_argument("--write-baseline", action="store_true")
    p.add_argument("--replace-baseline", action="store_true")
    p.set_defaults(fn=cmd_run)

    p = sub.add_parser("rebuild", help="只读已有证据重算检查与报告（不重调裁判）")
    common(p)
    p.add_argument("--evidence-dir", required=True)
    p.set_defaults(fn=cmd_rebuild)

    p = sub.add_parser("probe", help="最小契约探针（默认 dry-run；--execute 才真实调用）")
    common(p)
    p.add_argument("--kind", choices=["decision", "llm"], required=True)
    p.add_argument("--execute", action="store_true")
    p.set_defaults(fn=cmd_probe)

    a = ap.parse_args(argv)
    try:
        return a.fn(a)
    except casefile.CaseError as e:
        print(f"案例文件不合法：{e}", file=sys.stderr)
        return 2
    except judges.JudgeConfigError as e:
        print(f"裁判配置不可用：{e}", file=sys.stderr)
        return 2
    except Exception as e:                      # 未预期的评测程序异常：尽力留证据
        print(f"未预期异常：{type(e).__name__}: {e}", file=sys.stderr)
        import traceback
        traceback.print_exc()
        return 3


if __name__ == "__main__":
    sys.exit(main())
