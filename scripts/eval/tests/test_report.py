#!/usr/bin/env python3
"""report：原子写、脱敏扫描、证据目录、基线纪律、人读报告。"""
import json
import sys
import tempfile
import unittest
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import report  # noqa: E402


class TestAtomicWrite(unittest.TestCase):
    def test_json_roundtrip(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.json"
            report.atomic_write_json(p, {"a": 1, "中文": "值"})
            self.assertEqual(json.loads(p.read_bytes().decode("utf-8")),
                             {"a": 1, "中文": "值"})

    def test_secret_value_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(ValueError):
                report.atomic_write_json(Path(d) / "x.json", {"t": "abcdefghij"},
                                         secrets_to_check=["abcdefghij"])

    def test_authorization_pattern_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(ValueError):
                report.atomic_write_json(
                    Path(d) / "x.json", {"h": "Authorization: Bearer sk-abcdef123456789"})

    def test_jwt_pattern_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcabcabcabc"
            with self.assertRaises(ValueError):
                report.atomic_write_text(Path(d) / "x.txt", f"token={token}")


class TestRedactScan(unittest.TestCase):
    def test_clean_text(self):
        self.assertEqual(report.redact_scan("普通报告内容，无密钥"), [])

    def test_key_assignment(self):
        findings = report.redact_scan("DEEPSEEK_API_KEY=sk-live123456")
        self.assertTrue(findings)

    def test_literal_token(self):
        findings = report.redact_scan("Authorization: Bearer tok1234567890",
                                      secrets_to_check=["tok1234567890"])
        kinds = {f["kind"] for f in findings}
        self.assertIn("literal-secret", kinds)


class TestEvidenceDir(unittest.TestCase):
    def test_naming_and_collision(self):
        with tempfile.TemporaryDirectory() as d:
            p1 = report.new_evidence_dir("doc-verify", "n5", base=d)
            p2 = report.new_evidence_dir("doc-verify", "n5", base=d)
            self.assertNotEqual(p1, p2)
            name = p1.name
            self.assertRegex(name, r"^\d{8}-\d{6}-doc-verify-n5$")
            self.assertTrue(p1.is_dir())


class TestBaseline(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.path = Path(self.dir.name) / "b.json"

    def tearDown(self):
        self.dir.cleanup()

    def record(self):
        return {"U": 0, "P": 3, "F": 2, "pass@1": 0.6, "Pass@5": True, "pass^5": False,
                "caseSha256": "a" * 64, "configFingerprint": {}, "n": 5, "trials": []}

    def test_write_ok(self):
        report.write_baseline(self.path, self.record(), write_flag=True)
        self.assertEqual(report.load_baseline(self.path)["P"], 3)

    def test_refused_without_flag(self):
        with self.assertRaises(report.BaselineError):
            report.write_baseline(self.path, self.record())

    def test_refused_when_u_positive(self):
        rec = self.record()
        rec["U"] = 1
        with self.assertRaises(report.BaselineError):
            report.write_baseline(self.path, rec, write_flag=True)

    def test_refused_overwrite_without_replace(self):
        report.write_baseline(self.path, self.record(), write_flag=True)
        with self.assertRaises(report.BaselineError):
            report.write_baseline(self.path, self.record(), write_flag=True)
        report.write_baseline(self.path, self.record(), write_flag=True,
                              replace_flag=True)

    def test_load_missing(self):
        self.assertIsNone(report.load_baseline(Path(self.dir.name) / "none.json"))


class TestReportMd(unittest.TestCase):
    def test_failed_items_located(self):
        with tempfile.TemporaryDirectory() as d:
            doc = {"caseId": "doc-verify", "caseSha256": "a" * 64,
                   "generatedAt": "now",
                   "configFingerprint": {"engineCommit": "abc", "model": "m"},
                   "batch": {"reportStatus": "complete", "P": 0, "F": 1, "U": 0,
                             "pass@1": 0.0, "Pass@5": False, "pass^5": False},
                   "trials": [{"trialId": "t1", "result": "failed",
                               "checks": {"A1": {"status": "failed",
                                                 "reason": "缺访问",
                                                 "evidence": {"missing": ["决策-方案B.md"]}}},
                               "executionOutcome": None}]}
            p = Path(d) / "report.md"
            report.write_report_md(p, doc)
            text = p.read_bytes().decode("utf-8")
            self.assertIn("t1 · A1", text)
            self.assertIn("缺访问", text)
            self.assertIn("决策-方案B.md", text)

    def test_incomplete_batch_says_so(self):
        with tempfile.TemporaryDirectory() as d:
            doc = {"caseId": "c", "caseSha256": "a" * 64, "generatedAt": "n",
                   "configFingerprint": {}, "batch": {"reportStatus": "incomplete",
                                                      "P": 1, "F": 0, "U": 1},
                   "trials": []}
            p = Path(d) / "r.md"
            report.write_report_md(p, doc)
            self.assertIn("incomplete", p.read_bytes().decode("utf-8"))


if __name__ == "__main__":
    unittest.main()
