#!/usr/bin/env python3
"""grader：A1–A4、B1–B2、MCP 运行条件、三票函数、trial/批次汇总、基线退步。"""
import sys
import unittest
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import casefile  # noqa: E402
import evidence as ev  # noqa: E402
import grader  # noqa: E402

CASE = casefile.load_case(EVAL_DIR / "cases" / "doc-verify.json")
MANIFEST = casefile.fixture_manifest(CASE.files)


def cite(path, rnd=1, section=None, parse_error=None, oob=False):
    return {"round": rnd, "runId": f"r{rnd}", "raw": path, "path": path, "section": section,
            "line": None, "parseError": parse_error, "outOfBounds": oob}


def event(path, rnd=1):
    return {"round": rnd, "runId": f"r{rnd}", "runlogOffset": rnd, "agent": "",
            "toolCallId": f"c{rnd}{path}", "tool": "read_file", "path": path, "output": "x"}


ANS1 = "方案 B 停止推进的理由见 决策-方案B.md §2：维护成本翻倍、功能重叠。"


class TestA1(unittest.TestCase):
    def test_pass(self):
        r = grader.check_a1([cite("决策-方案B.md", 1, 2)], [event("决策-方案B.md", 1)], [], set())
        self.assertEqual(r["status"], "passed")

    def test_missing_access_failed(self):
        r = grader.check_a1([cite("决策-方案B.md", 1)], [], [], set())
        self.assertEqual(r["status"], "failed")

    def test_record_missing_invalid(self):
        r = grader.check_a1([], [], [], {2})
        self.assertEqual(r["status"], "invalid")

    def test_parse_error_invalid(self):
        r = grader.check_a1([cite(None, 1, parse_error="unclear-path")], [], [], set())
        self.assertEqual(r["status"], "invalid")

    def test_unparsed_sole_evidence_invalid(self):
        un = [{"round": 1, "raw": {"form": "bash"}}]
        r = grader.check_a1([cite("决策-方案B.md", 1)], [], un, set())
        self.assertEqual(r["status"], "invalid")

    def test_later_round_cannot_backfill(self):
        cites = [cite("决策-方案B.md", 1), cite("事故记录-0620.md", 2, 4)]
        events = [event("决策-方案B.md", 1), event("事故记录-0620.md", 2)]
        r = grader.check_a1(cites, events, [], set())
        self.assertEqual(r["status"], "passed")   # 两轮都各自有访问
        events2 = [event("决策-方案B.md", 2), event("事故记录-0620.md", 2)]
        r = grader.check_a1(cites, events2, [], set())
        self.assertEqual(r["status"], "failed")   # 第 1 轮在定稿前没有访问

    def test_any_round_failed_dominates_invalid(self):
        cites = [cite("决策-方案B.md", 1), cite("事故记录-0620.md", 2, 4),
                 cite(None, 3, parse_error="unclear-path")]
        events = [event("决策-方案B.md", 1)]     # 第 2 轮缺访问 → failed；第 3 轮解析失败 → invalid
        r = grader.check_a1(cites, events, [], set())
        self.assertEqual(r["status"], "failed")


class TestA2(unittest.TestCase):
    def test_locate(self):
        out = grader.a2_locate({3: "旧方案写着“计划 2026-09 上线”，因为“依赖方案 A 未完成”。"})
        terms = {(m["round"], m["term"]) for m in out["matches"]}
        self.assertEqual(terms, {(3, "2026-09"), (3, "依赖方案A")})
        self.assertTrue(all(m["sentence"] for m in out["matches"]))

    def test_whitespace_stripped(self):
        out = grader.a2_locate({1: "依 赖 方 案 A 未完成"})
        # 去除空白后是“依赖方案A未完成” → 命中
        self.assertEqual(len(out["matches"]), 1)


class TestA3A4(unittest.TestCase):
    def test_a3_clean(self):
        self.assertEqual(grader.check_a3([])["status"], "passed")

    def test_a3_web(self):
        self.assertEqual(grader.check_a3([{"tool": "web_search"}])["status"], "failed")

    def test_a4_no_keywords(self):
        r = grader.check_a4("随便说两句。", [], [])
        self.assertEqual(r["status"], "passed")

    def test_a4_missing_source(self):
        r = grader.check_a4(ANS1, [], [event("决策-方案B.md", 1)])
        self.assertEqual(r["status"], "failed")
        self.assertIn("点名", r["reason"])

    def test_a4_missing_access(self):
        r = grader.check_a4(ANS1, [cite("决策-方案B.md", 1)], [])
        self.assertEqual(r["status"], "failed")
        self.assertIn("访问", r["reason"])

    def test_a4_ok(self):
        r = grader.check_a4(ANS1, [cite("决策-方案B.md", 1, 2)], [event("决策-方案B.md", 1)])
        self.assertEqual(r["status"], "passed")

    def test_a4_answer_missing_invalid(self):
        self.assertEqual(grader.check_a4(None, [], [])["status"], "invalid")


class TestB1B2(unittest.TestCase):
    def test_b1_ok(self):
        r = grader.check_b1([cite("决策-方案B.md", 1, 2), cite("事故记录-0620.md", 2, 4)],
                            CASE.files)
        self.assertEqual(r["status"], "passed")

    def test_b1_unknown_file(self):
        r = grader.check_b1([cite("不存在.md", 1)], CASE.files)
        self.assertEqual(r["status"], "failed")

    def test_b1_unknown_section(self):
        r = grader.check_b1([cite("决策-方案B.md", 1, 7)], CASE.files)
        self.assertEqual(r["status"], "failed")

    def test_b1_parse_error_invalid(self):
        r = grader.check_b1([cite(None, 1, parse_error="unclear-path")], CASE.files)
        self.assertEqual(r["status"], "invalid")

    def test_b1_no_section_not_failed(self):
        r = grader.check_b1([cite("决策-方案B.md", 1, None)], CASE.files)
        self.assertEqual(r["status"], "passed")

    def test_b1_out_of_bounds_failed(self):
        r = grader.check_b1([cite("../x.md", 1, oob=True)], CASE.files)
        self.assertEqual(r["status"], "failed")

    def test_b2_ok(self):
        r = grader.check_b2(MANIFEST, MANIFEST)
        self.assertEqual(r["status"], "passed")

    def test_b2_modified(self):
        final = [dict(m) for m in MANIFEST]
        final[0] = dict(final[0], sha256="f" * 64)
        self.assertEqual(grader.check_b2(MANIFEST, final)["status"], "failed")

    def test_b2_deleted(self):
        final = [dict(m, missing=True, sha256=None) for m in MANIFEST[:3]]
        self.assertEqual(grader.check_b2(MANIFEST, final)["status"], "failed")

    def test_b2_extra_files_ignored(self):
        final = MANIFEST + [{"path": "新文件.md", "sha256": "a" * 64}]
        self.assertEqual(grader.check_b2(MANIFEST, final)["status"], "passed")

    def test_b2_missing_initial_invalid(self):
        self.assertEqual(grader.check_b2(None, MANIFEST)["status"], "invalid")

    def test_b2_final_incomplete_invalid(self):
        self.assertEqual(grader.check_b2(MANIFEST, MANIFEST, final_complete=False)["status"],
                         "invalid")


class TestMcpCondition(unittest.TestCase):
    def rounds(self, system=None, custom=None):
        return {i: {"system": system or [], "custom": custom or []} for i in (1, 2, 3)}

    def test_ok(self):
        self.assertTrue(grader.mcp_run_condition("a" * 64, {}, self.rounds(), [])["ok"])

    def test_tenant_servers(self):
        r = grader.mcp_run_condition("a" * 64, {"srv": {}}, self.rounds(), [])
        self.assertFalse(r["ok"])

    def test_round_status_nonempty(self):
        r = grader.mcp_run_condition("a" * 64, {}, self.rounds(custom=[{"name": "x"}]), [])
        self.assertFalse(r["ok"])

    def test_mcp_tool_call(self):
        r = grader.mcp_run_condition("a" * 64, {}, self.rounds(), ["mcp__fetch__x"])
        self.assertFalse(r["ok"])

    def test_missing_mcp_hash(self):
        self.assertFalse(grader.mcp_run_condition(None, {}, self.rounds(), [])["ok"])


class TestVotes(unittest.TestCase):
    def test_q_directions(self):
        self.assertEqual(grader.q_item_result(0, ["yes", "yes", "no"], "yes_pass"), "passed")
        self.assertEqual(grader.q_item_result(0, ["no", "no", "yes"], "no_pass"), "passed")
        self.assertEqual(grader.q_item_result(0, ["no", "no", "unknown"], "yes_pass"), "failed")

    def test_e_dominates(self):
        self.assertEqual(grader.q_item_result(1, ["yes", "yes"], "yes_pass"), "invalid")
        self.assertEqual(grader.three_vote(1, 2, 0, 0), "invalid")

    def test_no_majority_invalid(self):
        self.assertEqual(grader.three_vote(0, 1, 1, 1), "invalid")

    def test_d_threshold(self):
        v = [{"score": 3, "unknown": False, "hardViolation": False} for _ in range(3)]
        self.assertEqual(grader.d_item_result(0, v, 3), "passed")
        self.assertEqual(grader.d_item_result(0, v, 4), "failed")

    def test_d_hard_violation_overrides_score(self):
        v = [{"score": 5, "unknown": False, "hardViolation": True} for _ in range(3)]
        self.assertEqual(grader.d_item_result(0, v, 3), "failed")

    def test_d_unknown_vote(self):
        v = [{"score": None, "unknown": True, "hardViolation": False} for _ in range(3)]
        self.assertEqual(grader.d_item_result(0, v, 3), "invalid")

    def test_d_threshold_none_invalid(self):
        v = [{"score": 5, "unknown": False, "hardViolation": False} for _ in range(3)]
        self.assertEqual(grader.d_item_result(0, v, None), "invalid")


class TestSummaries(unittest.TestCase):
    def all_passed_checks(self):
        return {k: "passed" for k in grader.REQUIRED_CHECKS}

    def test_trial_passed_failed_invalid(self):
        self.assertEqual(grader.trial_result(self.all_passed_checks()), "passed")
        c = self.all_passed_checks()
        c["A1"] = "failed"
        self.assertEqual(grader.trial_result(c), "failed")
        c = self.all_passed_checks()
        c["D2"] = "invalid"
        self.assertEqual(grader.trial_result(c), "invalid")
        # 失败优先于无效
        c["A3"] = "failed"
        self.assertEqual(grader.trial_result(c), "failed")

    def test_a2_not_in_required(self):
        self.assertNotIn("A2", grader.REQUIRED_CHECKS)

    def test_batch_complete(self):
        b = grader.batch_summary(["passed", "failed", "passed", "failed", "passed"])
        self.assertEqual(b["reportStatus"], "complete")
        self.assertEqual(b["pass@1"], 0.6)
        self.assertTrue(b["Pass@5"])
        self.assertFalse(b["pass^5"])

    def test_batch_all_pass(self):
        b = grader.batch_summary(["passed"] * 5)
        self.assertTrue(b["pass^5"])

    def test_batch_incomplete(self):
        b = grader.batch_summary(["passed", "invalid", "failed", "passed", "passed"])
        self.assertEqual(b["reportStatus"], "incomplete")
        self.assertIsNone(b["pass@1"])
        self.assertIsNone(b["Pass@5"])
        self.assertIsNone(b["pass^5"])
        self.assertEqual(b["validTrialCount"], 4)

    def test_baseline_regression_rules(self):
        fp = {"caseSha256": "a" * 64}
        good_base = {"pass^5": True, "configFingerprint": fp}
        self.assertIsNotNone(grader.baseline_regression(good_base, {"U": 0, "pass^5": False}, fp))
        self.assertIsNone(grader.baseline_regression(good_base, {"U": 0, "pass^5": True}, fp))
        self.assertIsNone(grader.baseline_regression(good_base, {"U": 1, "pass^5": False}, fp))
        self.assertIsNone(grader.baseline_regression(good_base, {"U": 0, "pass^5": False},
                                                     {"caseSha256": "b" * 64}))
        weak_base = {"pass^5": False, "configFingerprint": fp}
        self.assertIsNone(grader.baseline_regression(weak_base, {"U": 0, "pass^5": False}, fp))


class TestCitationsIntegration(unittest.TestCase):
    """用真实解析器走一遍 grader：链接锚点/不存在小节/越界路径。"""

    def test_anchor_link_passes_b1(self):
        ans = {1: "理由见 [否决理由](决策-方案B.md#2-否决理由)。", 2: None, 3: None}
        cites = ev.extract_citations(ans[1], 1, "r1", CASE.files)
        events = [event("决策-方案B.md", 1)]
        a1 = grader.check_a1(cites, events, [], set())
        b1 = grader.check_b1(cites, CASE.files)
        self.assertEqual((a1["status"], b1["status"]), ("passed", "passed"))

    def test_bad_section_fails_b1(self):
        ans = "理由见 决策-方案B.md §7。"
        cites = ev.extract_citations(ans, 1, "r1", CASE.files)
        self.assertEqual(grader.check_b1(cites, CASE.files)["status"], "failed")


if __name__ == "__main__":
    unittest.main()
