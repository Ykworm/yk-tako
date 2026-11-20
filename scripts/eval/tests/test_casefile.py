#!/usr/bin/env python3
"""casefile：案例 JSON 校验、fixture 清单与小节锚点、配置指纹。"""
import json
import sys
import tempfile
import unittest
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import casefile  # noqa: E402

CASE_PATH = EVAL_DIR / "cases" / "doc-verify.json"


class TestLoadRealCase(unittest.TestCase):
    def setUp(self):
        self.case = casefile.load_case(CASE_PATH)

    def test_real_case_loads(self):
        self.assertEqual(self.case.case_id, "doc-verify")
        self.assertEqual(len(self.case.files), 4)
        self.assertEqual(len(self.case.turns), 3)
        self.assertEqual(self.case.raw_sha256 and len(self.case.raw_sha256), 64)

    def test_turns_verbatim_shape(self):
        self.assertEqual([t.id for t in self.case.turns], ["r1", "r2", "r3"])
        for t in self.case.turns:
            self.assertTrue(t.user.strip())
            self.assertTrue(t.expected)

    def test_questions_noul(self):
        for qid, q in self.case.decision_questions.items():
            self.assertEqual(q["type"], "noul")
            self.assertTrue(q["instructions"].strip())

    def test_dimensions_cover_rounds(self):
        self.assertEqual({d["round"] for d in self.case.dimensions.values()}, {1, 2, 3})
        for d in self.case.dimensions.values():
            self.assertIn("共同硬性准则", d["criteria"])

    def test_budget(self):
        self.assertEqual(self.case.budget["judgeVotes"], 3)
        self.assertEqual(self.case.budget["trials"], 5)
        for key in casefile.BUDGET_KEYS:
            self.assertGreater(self.case.budget[key], 0)


class TestValidation(unittest.TestCase):
    def _write(self, obj):
        tmp = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        json.dump(obj, tmp, ensure_ascii=False)
        tmp.close()
        self.addCleanup(Path(tmp.name).unlink)
        return tmp.name

    def _base(self):
        return json.loads(CASE_PATH.read_bytes().decode("utf-8"))

    def _bad(self, mutate, needle=""):
        obj = self._base()
        mutate(obj)
        path = self._write(obj)
        with self.assertRaises(casefile.CaseError) as ctx:
            casefile.load_case(path)
        if needle:
            self.assertIn(needle, str(ctx.exception))

    def test_unknown_schema_version(self):
        self._bad(lambda o: o.update(schemaVersion=2), "schemaVersion")

    def test_duplicate_paths(self):
        def dup(o):
            o["files"].append(dict(o["files"][0]))
        self._bad(dup, "重复")

    def test_empty_turn_user(self):
        self._bad(lambda o: o["turns"][0].update(user="  "), "台词")

    def test_turn_count_not_three(self):
        self._bad(lambda o: o["turns"].pop(), "三轮")

    def test_bad_budget(self):
        self._bad(lambda o: o["budget"].update(runTimeoutSec=-1), "runTimeoutSec")

    def test_missing_question(self):
        self._bad(lambda o: o["decisionQuestions"].pop("Q2"), "Q2")

    def test_extra_dimension(self):
        self._bad(lambda o: o["dimensions"].update(D9={"round": 1, "criteria": "x"}), "未知维度")

    def test_path_escape(self):
        def bad(o):
            o["files"][0]["path"] = "../etc/passwd"
        self._bad(bad, "..")

    def test_absolute_path(self):
        def bad(o):
            o["files"][0]["path"] = "/tmp/x.md"
        self._bad(bad)


class TestAnchorsAndManifest(unittest.TestCase):
    def setUp(self):
        self.case = casefile.load_case(CASE_PATH)

    def test_sections(self):
        f = self.case.fixture("决策-方案B.md")
        self.assertEqual(casefile.fixture_sections(f), {1: "方案背景", 2: "否决理由", 3: "结论"})

    def test_anchors(self):
        f = self.case.fixture("决策-方案B.md")
        self.assertIn("2-否决理由", casefile.fixture_anchors(f))

    def test_manifest(self):
        m = casefile.fixture_manifest(self.case.files)
        self.assertEqual(len(m), 4)
        self.assertEqual(sorted(x["path"] for x in m),
                         sorted(self.case.fixture_paths))
        for item in m:
            self.assertEqual(len(item["sha256"]), 64)


class TestFingerprint(unittest.TestCase):
    def test_unversioned_marked(self):
        fp = casefile.config_fingerprint("a" * 64, "m", None, False, None, None)
        self.assertEqual(fp["engineCommit"], "unversioned")
        self.assertEqual(fp["mcpJsonSha256"], "unversioned")
        self.assertEqual(fp["caseSha256"], "a" * 64)


if __name__ == "__main__":
    unittest.main()
