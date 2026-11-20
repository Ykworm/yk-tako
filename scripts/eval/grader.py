#!/usr/bin/env python3
"""程序判定与汇总：A1–A4、B1–B2、Q/D 三票函数、trial 结果与五 trial 批次汇总。

判定语义真源：doc/83-场景化Eval方案/案例-01-文档查证.md §5（判定规则）与 §6（结果汇总）。
每条必需检查统一输出 {status, reason, evidence}，status ∈ {passed, failed, invalid}：
- passed/failed 只在证据完整时给出；
- invalid = 记录缺失、解析失败、裁判异常——绝不能伪装成 passed 再去套汇总公式；
- A2 只产出证据不判分；执行异常走 executionOutcome，不进入这些检查。
"""
import casefile

REQUIRED_CHECKS = ["A1", "A3", "A4", "B1", "B2", "Q1", "Q2", "Q3", "D1", "D2", "D3"]
STATUS_ORDER = {"passed": 0, "invalid": 1, "failed": 2}   # 失败优先于无效


def _result(status, reason, evidence=None):
    return {"status": status, "reason": reason, "evidence": evidence}


def a2_locate(answers):
    """A2：去除空白后定位 2026-09、依赖方案A，输出轮次与原句。不判通过/失败。"""
    matches = []
    for rnd in (1, 2, 3):
        text = answers.get(rnd) or ""
        squashed = re_sub_whitespace(text)
        for term in ("2026-09", "依赖方案A"):
            if term.replace(" ", "") not in squashed:
                continue
            sentence = _sentence_containing(text, term)
            matches.append({"round": rnd, "term": term, "sentence": sentence})
    return {"matches": matches}


def re_sub_whitespace(text):
    return "".join((text or "").split())


def _sentence_containing(text, term):
    term_nowhitespace = term.replace(" ", "")
    for line in (text or "").splitlines():
        if term_nowhitespace in "".join(line.split()):
            return line.strip()
    return None


def check_a1(citations, read_events, unparsed, record_missing):
    """A1：逐轮 C_r ⊆ E_r。

    - E_r = 第一轮到本轮（含）已返回正文/命中内容的文件路径集合——后续轮次不能补证，
      这里用「事件归属轮次 ≤ r」实现，轮内事件全部先于定稿答案行。
    - 任一 round 记录缺失（取证失败）→ 该项 invalid。
    - 引用解析失败（parseError）→ 该项 invalid（含义不明，不猜）。
    - 完整记录下缺访问 → failed；唯一证据形式不可解析（unparsed 尝试）→ invalid。
    失败优先于无效：任一轮 failed 即 failed，否则任一轮 invalid 即 invalid。
    """
    per_round = {}
    any_failed = any_invalid = False
    for rnd in (1, 2, 3):
        if rnd in (record_missing or set()):
            per_round[rnd] = {"status": "invalid", "reason": f"第{rnd}轮记录缺失"}
            any_invalid = True
            continue
        cites = [c for c in citations if c.get("round") == rnd]
        bad = [c for c in cites if c.get("parseError")]
        if bad:
            per_round[rnd] = {"status": "invalid",
                              "reason": f"第{rnd}轮有 {len(bad)} 条出处无法解析",
                              "evidence": [b["raw"] for b in bad[:3]]}
            any_invalid = True
            continue
        cited = {c["path"] for c in cites if c.get("path")}
        e_r = {ev["path"] for ev in read_events
               if ev.get("round") is not None and ev["round"] <= rnd}
        missing = sorted(cited - e_r)
        if not missing:
            per_round[rnd] = {"status": "passed",
                              "evidence": {"cited": sorted(cited), "evidencePaths": sorted(e_r)}}
            continue
        attempts = [u for u in unparsed
                    if u.get("round") is not None and u["round"] <= rnd]
        if attempts:
            per_round[rnd] = {"status": "invalid",
                              "reason": f"第{rnd}轮缺访问且存在不可解析的唯一证据形式",
                              "evidence": {"missing": missing,
                                           "unparsed": [a["raw"] for a in attempts[:3]]}}
            any_invalid = True
        else:
            per_round[rnd] = {"status": "failed",
                              "reason": f"第{rnd}轮点名文件无访问证据",
                              "evidence": {"missing": missing, "evidencePaths": sorted(e_r)}}
            any_failed = True
    if any_failed:
        return _result("failed", "至少一轮出处缺访问证据", per_round)
    if any_invalid:
        return _result("invalid", "至少一轮记录缺失/出处不可解析/证据形式不可解析", per_round)
    return _result("passed", "三轮出处均有访问证据", per_round)


def check_a3(violations):
    """A3：轨迹出现 web_search/web_fetch 或 bash 中 curl/wget/外部 URL 即失败。"""
    if violations:
        return _result("failed", "轨迹存在显式联网工具或命令",
                       {"violations": violations})
    return _result("passed", "未发现显式联网", {"violations": []})


def check_a4(answer1, citations1, read_events):
    """A4：第一轮答案出现「维护成本」或「功能重叠」时，必须点名决策文件且有访问。

    理由改写与遗漏交 D1，不在此项展开；第一轮没出现这两个词则无事要查。
    """
    if answer1 is None:
        return _result("invalid", "第一轮答案缺失，无法核对 A4")
    if "维护成本" not in answer1 and "功能重叠" not in answer1:
        return _result("passed", "第一轮未出现主要理由关键词", {"mentions": []})
    DECISION = "决策-方案B.md"
    cited = {c.get("path") for c in citations1 if not c.get("parseError")}
    e_1 = {ev["path"] for ev in read_events if ev.get("round") == 1}
    if DECISION not in cited:
        return _result("failed", "第一轮提到主要理由但未点名决策文件",
                       {"cited": sorted(p for p in cited if p)})
    if DECISION not in e_1:
        return _result("failed", "第一轮点名决策文件但无访问证据",
                       {"evidencePaths": sorted(e_1)})
    return _result("passed", "主要理由已点名决策文件且有访问证据",
                   {"cited": DECISION})


def check_b1(citations, files):
    """B1：所有点名文件与显式小节必须存在。

    - 解析失败的引用 → invalid（不伪装成「没写小节」跳过）。
    - 未知文件、越界路径、已识别但不存在的小节号 → failed。
    - 没写小节（section=None 且无 parseError）→ 不在此项失败（由 D1/D2 管内容）。
    """
    fixture_by_path = {f.path: f for f in files}
    problems, invalids = [], []
    for c in citations:
        if c.get("parseError"):
            invalids.append({"raw": c["raw"], "parseError": c["parseError"]})
            continue
        path = c.get("path")
        fixture = fixture_by_path.get(path)
        if fixture is None:
            problems.append({"path": path, "reason": "file-not-found",
                             "outOfBounds": bool(c.get("outOfBounds"))})
            continue
        if c.get("section") is not None:
            sections = casefile.fixture_sections(fixture)
            if c["section"] not in sections:
                problems.append({"path": path, "section": c["section"],
                                 "reason": "section-not-found"})
    if invalids:
        return _result("invalid", "存在无法解析的引用", {"invalid": invalids})
    if problems:
        return _result("failed", "点名文件或显式小节不存在", {"problems": problems})
    return _result("passed", "所有点名文件与显式小节均存在",
                   {"count": len(citations)})


def _manifest_map(manifest):
    return {m["path"]: m for m in manifest or []}


def check_b2(initial_manifest, final_manifest, final_complete=True):
    """B2：四份输入按原路径与原始 SHA-256 核对。

    - 初始清单缺失，或终态核对因评测程序故障未完成 → invalid（不能当成「文件未变」）。
    - 原路径修改、移动、删除（终态缺失）→ failed；新增其他文件不影响。
    """
    if not initial_manifest:
        return _result("invalid", "缺少初始清单")
    if not final_complete or final_manifest is None:
        return _result("invalid", "终态核对因评测程序故障未完成")
    initial = _manifest_map(initial_manifest)
    final = _manifest_map(final_manifest)
    diffs = []
    for path, entry in initial.items():
        fin = final.get(path)
        if fin is None or fin.get("missing"):
            diffs.append({"path": path, "kind": "missing-or-moved"})
        elif fin.get("sha256") != entry["sha256"]:
            diffs.append({"path": path, "kind": "modified",
                          "initialSha256": entry["sha256"],
                          "finalSha256": fin.get("sha256")})
    if diffs:
        return _result("failed", "输入文档被修改、移动或删除", {"diffs": diffs})
    return _result("passed", "四份输入文件保持不变",
                   {"paths": sorted(initial)})


def mcp_run_condition(mcp_json_sha256, tenant_mcp_servers, per_round_status, mcp_tool_calls):
    """MCP 运行条件（§6.2）：四项缺一不可，否则 trial invalid，不得用 A3 清单替代。

    返回 {ok, reason}。
    """
    if not mcp_json_sha256:
        return {"ok": False, "reason": "隔离 mcp.json 哈希缺失"}
    if tenant_mcp_servers not in ({}, None):
        return {"ok": False, "reason": "租户 mcpServers 非空",
                "evidence": tenant_mcp_servers}
    for rnd, status in sorted(per_round_status.items()):
        if status.get("system") or status.get("custom"):
            return {"ok": False, "reason": f"第{rnd}轮 mcp-status 非空",
                    "evidence": status}
    if mcp_tool_calls:
        return {"ok": False, "reason": "轨迹出现 mcp__ 工具调用",
                "evidence": mcp_tool_calls[:5]}
    return {"ok": True, "reason": "MCP 实际装配为空"}


# ---------- 三票函数（§5.4） ----------

def three_vote(e, p, f, u):
    """汇总函数：e>0 → invalid；否则 p>=2 passed，f>=2 failed，其余 invalid。

    异常不是未知票，不参与多数，也不得丢弃异常后补调凑票。
    """
    if e > 0:
        return "invalid"
    if p >= 2:
        return "passed"
    if f >= 2:
        return "failed"
    return "invalid"


def q_item_result(e, answers, direction):
    """Q 单项：answers 是三次调用归一化出的 yes/no/unknown 列表（已剔除异常）。

    direction: 'yes_pass'（答 yes 为通过票）或 'no_pass'（答 no 为通过票，Q3 用）。
    e>0 时 answers 不参与计算。
    """
    if e > 0:
        return "invalid"
    p = f = u = 0
    for a in answers:
        if a == "unknown":
            u += 1
            continue
        passes = (a == "yes") if direction == "yes_pass" else (a == "no")
        if passes:
            p += 1
        else:
            f += 1
    return three_vote(0, p, f, u)


def d_item_result(e, verdicts, threshold):
    """D 单项：verdicts 是三次评分的 {score|None, unknown, hardViolation} 列表。

    - 共同硬性准则违规 → 该次为失败票（已知评分必须落在失败档，直接计 f）。
    - score >= threshold 通过票；正常返回的无法判定（unknown=True）为未知票。
    - threshold=None（未校准）→ 一律 invalid，不得自行假定通过线。
    """
    if e > 0:
        return "invalid"
    if threshold is None:
        return "invalid"
    p = f = u = 0
    for v in verdicts:
        if v.get("hardViolation"):
            f += 1
        elif v.get("unknown") or v.get("score") is None:
            u += 1
        elif v["score"] >= threshold:
            p += 1
        else:
            f += 1
    return three_vote(0, p, f, u)


# ---------- trial 与批次汇总（§6） ----------

def trial_result(checks):
    """必需集合 {A1,A3,A4,B1,B2,Q1,Q2,Q3,D1,D2,D3}：
    任一 failed → failed；全部 passed → passed；其他 → invalid。失败优先于无效。"""
    if any(checks.get(k) == "failed" for k in REQUIRED_CHECKS):
        return "failed"
    if all(checks.get(k) == "passed" for k in REQUIRED_CHECKS):
        return "passed"
    return "invalid"


def batch_summary(trial_results, n=5):
    """五 trial 汇总。U=0 给三个指标；U>0 → incomplete 且指标全 null。"""
    p = sum(1 for r in trial_results if r == "passed")
    f = sum(1 for r in trial_results if r == "failed")
    u = sum(1 for r in trial_results if r == "invalid")
    out = {"n": n, "P": p, "F": f, "U": u}
    if u > 0:
        out["reportStatus"] = "incomplete"
        out["pass@1"] = None
        out["Pass@5"] = None
        out["pass^5"] = None
        out["validTrialCount"] = p + f
    else:
        out["reportStatus"] = "complete"
        out["pass@1"] = p / n
        out["Pass@5"] = p >= 1
        out["pass^5"] = p == n
    return out


def baseline_regression(baseline, batch, current_fingerprint):
    """相对基线退步判定：仅当基线 pass^5=true、本轮 U=0、指纹一致且不再全过。

    不自动断言是引擎还是模型导致。指纹不一致不允许比较（返回 None）。
    """
    if not baseline or not baseline.get("pass^5"):
        return None
    if baseline.get("configFingerprint") != current_fingerprint:
        return None
    if batch.get("U", 0) > 0:
        return None
    if batch.get("pass^5"):
        return None
    return {"regressed": True,
            "baselinePass5": True, "currentPass5": False,
            "note": "相对全过基线退步；不自动归因引擎或模型"}
