#!/usr/bin/env python3
"""judges：配置校验、可配置解析、密钥纪律、投票编排。"""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import judges  # noqa: E402

JUDGE_JSON = EVAL_DIR / "cases" / "doc-verify.judge.json"


def verified_cfg():
    return {
        "decision": {
            "url": "https://example.com/systemone",
            "responseParse": {
                "status": "verified",
                "questions": {"Q1": {"path": "data.answers.Q1.value"},
                              "Q2": {"path": "data.answers.Q2.value"},
                              "Q3": {"path": "data.answers.Q3.value"}},
                "valueMap": {"yes": "yes", "no": "no", "unknown": "unknown"},
            },
            "voteDirection": {"Q1": "yes_pass", "Q2": "yes_pass", "Q3": "no_pass"},
        },
        "llm": {
            "url": "https://api.deepseek.com/v1/chat/completions",
            "model": "deepseek-v4-pro",
            "temperature": 0,
            "responseFormat": "json_object",
            "responseParse": {
                "status": "verified",
                "contentPath": "choices.0.message.content",
                "scorePath": "score",
                "unknownPath": "unknown",
                "hardViolationPath": "hardViolation",
                "evidencePath": "evidence",
            },
        },
    }


class TestConfig(unittest.TestCase):
    def test_real_judge_json_loads_current_verification_state(self):
        cfg = judges.load_judge_config(JUDGE_JSON)
        self.assertEqual(cfg["decision"]["model"], "decision-model-preview")
        self.assertEqual(cfg["llm"]["model"], "deepseek-v4-pro")
        # decision 尚未实测，仍必须拒绝；LLM 已用真实探针固定响应位置。
        self.assertEqual(cfg["decision"]["responseParse"]["status"], "unverified")
        self.assertEqual(cfg["llm"]["responseParse"]["status"], "verified")
        self.assertEqual(cfg["llm"]["responseParse"]["contentPath"], "choices.0.message.content")
        self.assertFalse(cfg["llm"]["thresholdVerified"])
        self.assertIsNone(cfg["llm"]["threshold"])
        self.assertEqual(cfg["decision"]["authPrefix"], "")           # 不加 Bearer
        self.assertEqual(cfg["llm"]["authPrefix"], "Bearer ")

    def test_missing_file(self):
        with self.assertRaises(judges.JudgeConfigError):
            judges.load_judge_config("/nonexistent/judge.json")

    def test_require_verified_blocks_unverified_decision_only(self):
        cfg = judges.load_judge_config(JUDGE_JSON)
        with self.assertRaises(judges.JudgeConfigError):
            judges.require_verified(cfg, "decision")
        judges.require_verified(cfg, "llm")

    def test_missing_section(self):
        bad = Path(os.devnull)
        with self.assertRaises((judges.JudgeConfigError, json.JSONDecodeError)):
            judges.load_judge_config(bad)


class TestParseDecision(unittest.TestCase):
    def setUp(self):
        self.cfg = verified_cfg()

    def test_ok(self):
        body = {"data": {"answers": {"Q1": {"value": "yes"}, "Q2": {"value": "no"},
                                     "Q3": {"value": "unknown"}}}}
        self.assertEqual(judges.extract_decision_answers(body, self.cfg),
                         {"Q1": "yes", "Q2": "no", "Q3": "unknown"})

    def test_missing_one_question(self):
        body = {"data": {"answers": {"Q1": {"value": "yes"}, "Q3": {"value": "no"}}}}
        with self.assertRaises(judges.JudgeParseError):
            judges.extract_decision_answers(body, self.cfg)

    def test_unmapped_value(self):
        body = {"data": {"answers": {"Q1": {"value": "maybe"},
                                     "Q2": {"value": "no"}, "Q3": {"value": "no"}}}}
        with self.assertRaises(judges.JudgeParseError):
            judges.extract_decision_answers(body, self.cfg)


class TestParseLlm(unittest.TestCase):
    def setUp(self):
        self.cfg = verified_cfg()

    def _body(self, payload):
        return {"choices": [{"message": {"content": json.dumps(payload, ensure_ascii=False)}}]}

    def test_score(self):
        v = judges.extract_llm_verdict(
            self._body({"score": 4, "unknown": False, "hardViolation": False, "evidence": "句"}),
            self.cfg)
        self.assertEqual((v["score"], v["unknown"], v["hardViolation"]), (4, False, False))

    def test_unknown(self):
        v = judges.extract_llm_verdict(
            self._body({"score": None, "unknown": True, "evidence": ""}), self.cfg)
        self.assertTrue(v["unknown"])
        self.assertIsNone(v["score"])

    def test_hard_violation(self):
        v = judges.extract_llm_verdict(
            self._body({"score": 5, "unknown": False, "hardViolation": True}), self.cfg)
        self.assertTrue(v["hardViolation"])

    def test_bad_json(self):
        body = {"choices": [{"message": {"content": "不是 json"}}]}
        with self.assertRaises(judges.JudgeParseError):
            judges.extract_llm_verdict(body, self.cfg)

    def test_score_out_of_range(self):
        with self.assertRaises(judges.JudgeParseError):
            judges.extract_llm_verdict(self._body({"score": 6, "unknown": False}), self.cfg)

    def test_content_nested_object(self):
        v = judges.extract_llm_verdict({"choices": [{"message": {"content":
                {"score": 3, "unknown": False, "hardViolation": False}}}]}, self.cfg)
        self.assertEqual(v["score"], 3)


class TestDig(unittest.TestCase):
    def test_paths(self):
        obj = {"a": [{"b": 1}], "c": {"d": "x"}}
        self.assertEqual(judges.dig(obj, "a.0.b"), 1)
        self.assertEqual(judges.dig(obj, ["c", "d"]), "x")
        self.assertIsNone(judges.dig(obj, "a.5.b"))
        self.assertIsNone(judges.dig(obj, None))


class TestVoting(unittest.TestCase):
    def test_decision_votes_full_request_failure_hits_all(self):
        cfg = verified_cfg()
        t = judges.fake_transport([judges.JudgeCallError("boom"),
                                   {"data": {"answers": {"Q1": {"value": "yes"},
                                                         "Q2": {"value": "yes"},
                                                         "Q3": {"value": "no"}}}}])
        with mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "k"}):
            res = judges.run_decision_votes(cfg, {"s": 1}, {"Q1": {}, "Q2": {}, "Q3": {}},
                                            votes=2, transport=t)
        self.assertEqual(res["perQ"]["Q1"]["e"], 1)
        self.assertEqual(res["perQ"]["Q1"]["answers"], ["yes"])

    def test_no_automatic_retry(self):
        cfg = verified_cfg()
        t = judges.fake_transport([judges.JudgeCallError("timeout"),
                                   judges.JudgeCallError("timeout"),
                                   judges.JudgeCallError("timeout")])
        with mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "k"}):
            res = judges.run_decision_votes(cfg, {}, {}, votes=3, transport=t)
        self.assertEqual(res["perQ"]["Q2"]["e"], 3)

    def test_decision_key_from_env_only(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(judges.JudgeConfigError):
                judges.decision_key()
        with mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "sk-test-123"}):
            self.assertEqual(judges.decision_key(), "sk-test-123")

    def test_llm_messages_include_rubric(self):
        import casefile
        case = casefile.load_case(EVAL_DIR / "cases" / "doc-verify.json")
        msgs = judges.llm_dimension_messages(verified_cfg(), case, "D1", 1,
                                             case.turns[0].user, "答案", {"status": "passed"},
                                             [], [])
        self.assertEqual(msgs[0]["role"], "system")
        self.assertIn("评分锚点", msgs[1]["content"])
        self.assertIn("共同硬性准则", msgs[1]["content"])
        self.assertIn("决策-方案B.md", msgs[1]["content"])


class TestCallsCarryRawBody(unittest.TestCase):
    """calls 必须带服务原始 body/raw（rebuild 重算的依据），失败调用存 {ok:false,error}。"""

    def test_decision_calls_have_body_and_raw(self):
        cfg = verified_cfg()
        body = {"data": {"answers": {"Q1": {"value": "yes"}, "Q2": {"value": "yes"},
                                     "Q3": {"value": "no"}}}}
        t = judges.fake_transport([body, judges.JudgeCallError("boom")])
        saved = []
        with mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "k"}):
            res = judges.run_decision_votes(cfg, {"s": 1}, {"Q1": {}, "Q2": {}, "Q3": {}},
                                            votes=2, transport=t,
                                            save=lambda i, kind, p: saved.append((kind, p)))
        self.assertTrue(res["calls"][0]["ok"])
        self.assertEqual(res["calls"][0]["body"], body)
        self.assertTrue(res["calls"][0]["raw"])
        self.assertFalse(res["calls"][1]["ok"])
        self.assertIn("error", res["calls"][1])
        self.assertEqual(res["perQ"]["Q1"]["e"], 1)
        self.assertEqual([k for k, _ in saved], ["decision", "decision"])

    def test_llm_calls_named_and_have_body(self):
        cfg = verified_cfg()
        case = None
        body = {"choices": [{"message": {"content":
                json.dumps({"score": 4, "unknown": False, "hardViolation": False})}}]}
        t = judges.fake_transport([body])
        saved = []
        with mock.patch.object(judges, "llm_key", return_value="sk"):
            res = judges.run_llm_votes(cfg, case, "D2", [{"role": "user", "content": "x"}],
                                       votes=1, transport=t, name="llm-D2",
                                       save=lambda i, kind, p: saved.append((kind, p)))
        self.assertEqual(res["calls"][0]["body"], body)
        self.assertEqual(res["calls"][0]["verdict"]["score"], 4)
        self.assertEqual(saved[0][0], "llm-D2")       # 命名落盘前缀由调用方控制，D 间不串
        self.assertIn("requestBody", res)


class TestCurlJsonLocalServer(unittest.TestCase):
    """真实 curl × 本机 stdlib server（不触外网）：

    - body_file=None：body 经安全临时文件完整到达，Authorization 头经 stdin config
      到达，argv 不含密钥，临时文件 finally 删除（回归 `-K -` 混写 stdin 的传输层 bug）。
    - body_file 分支：沿用调用方文件。
    """

    def setUp(self):
        import threading
        from http.server import BaseHTTPRequestHandler, HTTPServer
        received = {}

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                n = int(self.headers.get("Content-Length", 0))
                received["body"] = self.rfile.read(n)
                received["auth"] = self.headers.get("Authorization")
                received["content_type"] = self.headers.get("Content-Type")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"ok": true}')

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.received = received
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self._shutdown)

    def _shutdown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    @property
    def url(self):
        return "http://127.0.0.1:%d/x" % self.server.server_address[1]

    def test_body_via_temp_file_header_via_stdin(self):
        import subprocess
        captured = {}
        real_run = subprocess.run

        def spy(argv, **kw):
            captured["argv"] = list(argv)
            return real_run(argv, **kw)

        key = "sk-live-secret-123"
        with mock.patch("subprocess.run", spy):
            status, parsed, _raw = judges.curl_json(
                self.url, {"Authorization": key}, {"中文": "值", "n": 1}, 10)
        self.assertEqual(status, 200)
        self.assertEqual(parsed, {"ok": True})
        # 服务器收到完整 JSON body 与 Authorization 头
        self.assertEqual(json.loads(self.received["body"].decode("utf-8")),
                         {"中文": "值", "n": 1})
        self.assertEqual(self.received["auth"], key)
        self.assertEqual(self.received["content_type"], "application/json")
        # argv 不含密钥；body 走 @<tmp>，stdin 只留 config
        self.assertFalse(any(key in a for a in captured["argv"]))
        at_args = [a for a in captured["argv"] if a.startswith("@")]
        self.assertEqual(len(at_args), 1)
        tmp = Path(at_args[0][1:])
        self.assertTrue(tmp.name.startswith("tako-judge-body-"))
        self.assertFalse(tmp.exists(), "临时 body 文件必须 finally 删除")

    def test_body_file_branch_uses_caller_file(self):
        import subprocess
        captured = {}
        real_run = subprocess.run

        def spy(argv, **kw):
            captured["argv"] = list(argv)
            return real_run(argv, **kw)

        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False,
                                         encoding="utf-8") as f:
            json.dump({"from": "file"}, f)
            path = f.name
        self.addCleanup(os.unlink, path)
        with mock.patch("subprocess.run", spy):
            status, parsed, _ = judges.curl_json(
                self.url, {"Authorization": "k"}, None, 10, body_file=path)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(self.received["body"].decode("utf-8")),
                         {"from": "file"})
        self.assertIn("@" + path, captured["argv"])


class TestCurlSecrecy(unittest.TestCase):
    def test_key_not_in_argv(self):
        captured = {}

        def fake_run(argv, **kw):
            captured["argv"] = argv
            captured["stdin"] = kw.get("input", "")

            class R:
                returncode = 0
                stdout = '{"ok": true}\n200'
                stderr = ""
            return R()

        with mock.patch("subprocess.run", fake_run):
            status, parsed, _ = judges.curl_json(
                "https://api.deepseek.com/v1/chat/completions",
                {"Authorization": "Bearer sk-super-secret-key"}, {"x": 1}, 30)
        self.assertEqual(status, 200)
        self.assertTrue(parsed["ok"])
        self.assertFalse(any("sk-super-secret-key" in a for a in captured["argv"]))
        self.assertIn("sk-super-secret-key", captured["stdin"])   # 只经 stdin config

    def test_http_error_raises(self):
        def fake_run(argv, **kw):
            class R:
                returncode = 0
                stdout = '{"error": "x"}\n500'
                stderr = ""
            return R()

        with mock.patch("subprocess.run", fake_run):
            with self.assertRaises(judges.JudgeCallError):
                judges.curl_json("https://x", {}, {}, 30)


if __name__ == "__main__":
    unittest.main()
