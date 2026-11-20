#!/usr/bin/env python3
"""run 流程回归：部分证据不串 trial、裁判证据命名与 raw body、rebuild 重算一致、
calibrate 逐样本留证与 unknown 比例口径、非法参数零副作用。

全部用 FakeEngine + fake_transport，不打真实引擎、不触网。
"""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import casefile  # noqa: E402
import judges  # noqa: E402
import run_eval  # noqa: E402
import test_target as tt  # noqa: E402  (FakeEngine 与路由助手)

CASE = casefile.load_case(EVAL_DIR / "cases" / "doc-verify.json")
BUDGET = dict(CASE.budget, pollIntervalMs=1, settleTimeoutSec=1, runTimeoutSec=5)


def verified_judge_file(d):
    cfg = {
        "decision": {
            "url": "https://example.com/systemone",
            "model": "decision-model-preview",
            "authPrefix": "",
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
            "authPrefix": "Bearer ",
            "temperature": 0,
            "responseFormat": "json_object",
            "candidateThresholds": [3, 4, 5],
            "threshold": 3,
            "thresholdVerified": True,
            "responseParse": {
                "status": "verified",
                "contentPath": "choices.0.message.content",
                "scorePath": "score", "unknownPath": "unknown",
                "hardViolationPath": "hardViolation", "evidencePath": "evidence",
            },
        },
    }
    p = Path(d) / "judge.json"
    p.write_text(json.dumps(cfg, ensure_ascii=False), encoding="utf-8")
    return p


def decision_body(q1="yes", q2="yes", q3="no"):
    return {"data": {"answers": {"Q1": {"value": q1}, "Q2": {"value": q2},
                                 "Q3": {"value": q3}}}}


def llm_body(score=5, unknown=False, hard=False):
    return {"choices": [{"message": {"content":
            json.dumps({"score": score, "unknown": unknown,
                        "hardViolation": hard, "evidence": "句"}, ensure_ascii=False)}}]}


def judge_transport(queue):
    return judges.fake_transport(queue)


def setup_engine(fe, fail_run=None):
    """三轮可用的桩引擎；fail_run 指定的 run 终态为 error（模拟被测系统执行失败）。"""
    signup_state = {"n": 0}

    def signup(body, p=None):
        signup_state["n"] += 1
        return (201, {"code": 0, "message": "", "data": {
            "accessToken": f"tok-{signup_state['n']}",
            "refreshToken": f"ref-{signup_state['n']}", "expiresIn": 900,
            "tenant": {"id": f"ten_{signup_state['n']}", "kind": "personal", "name": "t"},
            "user": {"id": f"u_{signup_state['n']}"}}})
    fe.route("POST", "/api/auth/signup", signup)

    def refresh(body, p=None):
        return (200, {"code": 0, "message": "", "data": {
            "accessToken": "tok-x", "refreshToken": body.get("refreshToken"),
            "expiresIn": 900}})
    fe.route("POST", "/api/auth/refresh", refresh)

    # runtime-config/mcp-status 用前缀路由 + path 后缀分发，覆盖动态租户 id。
    fe.route("PUT", "/api/tenants/ten_",
             lambda b, p=None: (200, {"code": 0, "message": "", "data": {}}))

    def tenant_get(body, p=None):
        if (p or "").endswith("/mcp-status"):
            return (200, {"code": 0, "message": "", "data": {"system": [], "custom": []}})
        if (p or "").endswith("/runtime-config"):
            return (200, {"code": 0, "message": "", "data":
                          {"modelRef": "deepseek/deepseek-flash", "mcpServers": {},
                           "sandbox": {"network": False}}})
        return (404, {"code": 112001, "message": "not found", "data": None})
    fe.route("GET", "/api/tenants/", tenant_get)
    wid = tt.workspace_routes(fe)
    tt.files_route(fe, wid)
    counter = {"n": 0}

    def create(body, p=None):
        counter["n"] += 1
        n = counter["n"]
        if body.get("text") is None:
            return (500, {"code": 111901, "message": "服务暂时不可用", "data": None})
        return (202, {"code": 0, "message": "", "data": {
            "runId": f"run_{n}",
            "sessionId": body.get("sessionId") or ("s_1" if n <= 3 else "s_2"),
            "messageId": body["messageId"]}})
    fe.route("POST", "/api/runs", create)
    for n in range(1, 7):
        session = "s_1" if n <= 3 else "s_2"
        state = tt.run_routes(fe, run_id=f"run_{n}", session_id=session,
                              register_log=False, create=False)
        if fail_run == n:
            state["run_status"][0] = "error"
    for session, upto in (("s_1", 3), ("s_2", 6)):
        snaps, acc = [], []
        for n in range(1, upto + 1):
            if (session == "s_1" and n <= 3) or (session == "s_2" and n >= 4):
                acc = acc + [r for p_ in tt.default_pages(f"run_{n}") for r in p_[1]]
                snaps.append(list(acc))
        tt.session_log_route(fe, session, snaps)
    return None


def run_transport(d_scores, fail_at=None):
    """判定transport：Q 全对；D1/D2/D3 用 d_scores 指定分数；fail_at=(dim, voteIdx) 处抛异常。"""
    queue = [decision_body() for _ in range(3)]
    for did in ("D1", "D2", "D3"):
        for j in range(3):
            if fail_at == (did, j):
                queue.append(judges.JudgeCallError("模拟调用失败"))
            else:
                queue.append(llm_body(score=d_scores.get(did, 5)))
    return queue


class FlowCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.fe = tt.FakeEngine()
        self._call_p = mock.patch("harness.call", self.fe)
        self._call_p.start()
        self.addCleanup(self._call_p.stop)
        self._key_p = mock.patch("harness.api_key", return_value="sk-fake")
        self._key_p.start()
        self.addCleanup(self._key_p.stop)
        self._ws_p = mock.patch.object(run_eval.harness, "WS_ROOT", str(self.root / "ws"))
        (self.root / "ws").mkdir()
        self._ws_p.start()
        self.addCleanup(self._ws_p.stop)
        self.judge_path = verified_judge_file(self.root)
        self.cfg = judges.load_judge_config(self.judge_path)

    def run_batch(self, n=1, reference=False, queue=None):
        queue = queue if queue is not None else run_transport({"D2": 1})
        env = mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "sk-fake"})
        with env:
            return run_eval.run_batch(
                CASE, self.cfg, n=n, reference=reference, out=str(self.root / "out"),
                transport=judge_transport(queue))


class TestInvalidArgs(unittest.TestCase):
    """缺陷 7：非法参数在创建证据目录前退出 2，零副作用。"""

    def _run(self, extra):
        with tempfile.TemporaryDirectory() as d:
            code = run_eval.main(["run", "--case", str(EVAL_DIR / "cases" / "doc-verify.json"),
                                  "--config", str(EVAL_DIR / "cases" / "doc-verify.judge.json"),
                                  "--out", d] + extra)
            leftover = list(Path(d).iterdir())
        return code, leftover

    def test_trials_zero(self):
        code, leftover = self._run(["--trials", "0"])
        self.assertEqual(code, 2)
        self.assertEqual(leftover, [])

    def test_trials_negative(self):
        code, leftover = self._run(["--trials", "-2"])
        self.assertEqual(code, 2)
        self.assertEqual(leftover, [])

    def test_reference_with_trials(self):
        code, leftover = self._run(["--reference", "--trials", "3"])
        self.assertEqual(code, 2)
        self.assertEqual(leftover, [])


class TestRunEvidenceAndRebuild(FlowCase):
    """缺陷 2/3/4：固定证据命名、raw body 落盘、run 与 rebuild 判定一致、D 不串。"""

    def test_run_writes_named_evidence_and_rebuild_matches(self):
        setup_engine(self.fe)
        code = self.run_batch(n=1, reference=True, queue=run_transport({"D2": 1}))
        self.assertEqual(code, 1)                     # D2 失败 → 有效但不过
        evd = next((self.root / "out").glob("*-doc-verify-reference"))
        t1 = evd / "trials" / "t1"
        jd = t1 / "judges"
        # 缺陷 2：固定命名 + 请求体 + 响应含原始 body/raw
        self.assertTrue((jd / "decision-request.json").exists())
        for i in (1, 2, 3):
            p = jd / f"decision-{i}-response.json"
            self.assertTrue(p.exists(), p)
            payload = json.loads(p.read_bytes().decode("utf-8"))
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["body"]["data"]["answers"]["Q1"]["value"], "yes")
            self.assertTrue(payload["raw"])
        for did in ("D1", "D2", "D3"):
            self.assertTrue((jd / f"llm-{did}-request.json").exists())
            for i in (1, 2, 3):
                p = jd / f"llm-{did}-{i}-response.json"
                self.assertTrue(p.exists(), p)
                payload = json.loads(p.read_bytes().decode("utf-8"))
                self.assertTrue(payload["ok"])
                self.assertIn("body", payload)
        checks = json.loads((t1 / "checks.json").read_bytes().decode("utf-8"))
        self.assertEqual(checks["D1"]["status"], "passed")
        self.assertEqual(checks["D2"]["status"], "failed")
        self.assertEqual(checks["D3"]["status"], "passed")
        for qid in ("Q1", "Q2", "Q3"):
            self.assertEqual(checks[qid]["status"], "passed")
        # 缺陷 3/4：rebuild 从已存证据重算，Q/D 判定一致、D1/D2 不串
        code = run_eval.main(["rebuild", "--case", str(EVAL_DIR / "cases" / "doc-verify.json"),
                              "--config", str(self.judge_path),
                              "--evidence-dir", str(evd)])
        self.assertEqual(code, 0)
        doc = json.loads((evd / "report.json").read_bytes().decode("utf-8"))
        rebuilt = doc["trials"][0]["checks"]
        for k in ("Q1", "Q2", "Q3", "D1", "D2", "D3"):
            self.assertEqual(rebuilt[k]["status"], checks[k]["status"], k)
        self.assertEqual(doc["batch"]["F"], 1)

    def test_failed_judge_call_saved_with_error(self):
        setup_engine(self.fe)
        queue = run_transport({"D2": 1}, fail_at=("D3", 1))
        code = self.run_batch(n=1, queue=queue)
        self.assertEqual(code, 1)                     # D2 failed → 批次 U=0、有失败 → 1
        evd = next((self.root / "out").glob("*-doc-verify-n1"))
        payload = json.loads((evd / "trials" / "t1" / "judges" / "llm-D3-2-response.json")
                             .read_bytes().decode("utf-8"))
        self.assertFalse(payload["ok"])
        self.assertIn("error", payload)
        checks = json.loads((evd / "trials" / "t1" / "checks.json").read_bytes().decode("utf-8"))
        self.assertEqual(checks["D3"]["status"], "invalid")   # e>0 → invalid，不折算成未知票


class TestPartialTrialIsolation(FlowCase):
    """缺陷 1：异常 trial 只保存当前实例的真实部分证据，不混入上一 trial。"""

    def test_second_trial_failure_keeps_own_partial_only(self):
        setup_engine(self.fe, fail_run=5)             # 第 2 个 trial 的第 2 轮 error
        code = self.run_batch(n=2, queue=run_transport({"D2": 1}))
        self.assertEqual(code, 1)                     # t1/t2 均 failed，U=0 → 1
        evd = next((self.root / "out").glob("*-doc-verify-n2"))
        t1, t2 = evd / "trials" / "t1", evd / "trials" / "t2"
        r1 = json.loads((t1 / "result.json").read_bytes().decode("utf-8"))
        r2 = json.loads((t2 / "result.json").read_bytes().decode("utf-8"))
        self.assertEqual(r1["result"], "failed")      # D2 低分 → failed
        self.assertEqual(r2["result"], "failed")
        self.assertIn("error", r2["executionOutcome"])
        # t2 只有自己的第 1 轮；没有第 2/3 轮，也没有 checks（裁判没跑）
        self.assertTrue((t2 / "rounds" / "r1").is_dir())
        self.assertFalse((t2 / "rounds" / "r2").exists())
        self.assertFalse((t2 / "judges").exists())
        # 身份隔离：两个 trial 的租户不同，t2 的 identity 不是借 t1 的
        id1 = json.loads((t1 / "identity.json").read_bytes().decode("utf-8"))
        id2 = json.loads((t2 / "identity.json").read_bytes().decode("utf-8"))
        self.assertNotEqual(id1["tenantId"], id2["tenantId"])
        self.assertTrue((t2 / "fixture-initial.json").exists())
        self.assertTrue((t2 / "fixture-final.json").exists())   # 失败后重读了终态
        # rebuild 不崩：执行失败 trial 保留 result/executionOutcome
        code = run_eval.main(["rebuild", "--case", str(EVAL_DIR / "cases" / "doc-verify.json"),
                              "--config", str(self.judge_path), "--evidence-dir", str(evd)])
        self.assertEqual(code, 0)
        doc = json.loads((evd / "report.json").read_bytes().decode("utf-8"))
        rows = {t["trialId"]: t for t in doc["trials"]}
        self.assertEqual(rows["t2"]["result"], "failed")
        self.assertIn("error", rows["t2"]["executionOutcome"])
        # rebuild 规则：缺原始裁判响应 → 对应检查 invalid，不重调裁判、不伪装成新结果；
        # 部分轮次的程序检查用真实保存的证据重算。
        self.assertEqual(rows["t2"]["checks"]["Q1"]["status"], "invalid")
        self.assertEqual(rows["t2"]["checks"]["D1"]["status"], "invalid")
        self.assertEqual(rows["t2"]["checks"]["B2"]["status"], "passed")


class TestCalibrateEvidenceAndUnknownRate(FlowCase):
    """缺陷 5/6：逐样本留证（两类裁判）+ unknown 比例只统计正常 verdict。"""

    def _samples(self):
        def smp(sid, set_, expect, dim, qlabels, dscore, hard):
            return {"id": sid, "set": set_, "handLabeled": True, "dimId": dim,
                    "expectedTrial": expect,
                    "answers": {"r1": "理由见 决策-方案B.md §2。", "r2": "不矛盾。",
                                "r3": "不能恢复排期；旧方案已作废。"},
                    "access": [{"round": 1, "path": "决策-方案B.md"}],
                    "qLabels": qlabels, "dScores": {dim: dscore},
                    "hardViolation": {dim: hard}}
        good = smp("s-good", "smoke", "passed", "D1",
                   {"Q1": "yes", "Q2": "yes", "Q3": "no"}, 5, False)
        bad = smp("s-bad", "smoke", "failed", "D1",
                  {"Q1": "no", "Q2": "no", "Q3": "yes"}, 1, True)
        more_good = [smp(f"s-g{i}", "smoke", "passed", "D1",
                         {"Q1": "yes", "Q2": "yes", "Q3": "no"}, 5, False)
                     for i in range(2, 6)]          # smoke 固定 6 例（§8.4）
        t1 = smp("s-t1", "tune", None, "D1",
                 {"Q1": "yes", "Q2": "yes", "Q3": "no"}, 5, False)
        t2 = smp("s-t2", "tune", None, "D1",
                 {"Q1": "yes", "Q2": "yes", "Q3": "no"}, 5, False)
        return [good, bad] + more_good + [t1, t2]

    def _transport(self):
        queue = []
        # good：Q 对、D1 三分全 5
        queue += [decision_body() for _ in range(3)] + [llm_body(5) for _ in range(3)]
        # bad：Q 反、D1 硬违规 1 分
        queue += [decision_body("no", "no", "yes") for _ in range(3)] + [llm_body(1, hard=True) for _ in range(3)]
        # 另有 4 个好例（smoke 固定 6 例），响应模式与 good 相同
        for _ in range(4):
            queue += [decision_body() for _ in range(3)] + [llm_body(5) for _ in range(3)]
        # t1：D1 = [unknown, 5, 5] → 1 张未知票 / 3 张正常票
        queue += [decision_body() for _ in range(3)]
        queue += [llm_body(None, unknown=True), llm_body(5), llm_body(5)]
        # t2：D1 = [5, 5, 调用异常] → 异常不产生 verdict，不计入 unknown 分母
        queue += [decision_body() for _ in range(3)]
        queue += [llm_body(5), llm_body(5), judges.JudgeCallError("模拟超时")]
        return judge_transport(queue)

    def test_calibrate_saves_per_sample_and_counts_unknown_only_from_normal_verdicts(self):
        evd = self.root / "cal"
        with mock.patch.object(judges, "llm_key", return_value="sk-fake"), \
                mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "sk-fake"}):
            result = run_eval._calibrate(self.cfg, CASE, self._samples(), evd,
                                         votes=3, timeout_sec=5,
                                         transport=self._transport())
        # 缺陷 6：unknown = 1 / (3+2) = 0.2（t1 三张正常票含一张未知；
        # t2 只有两张正常票——调用异常不产生 verdict，不进分子分母）
        self.assertAlmostEqual(result["summary"]["unknownRate"], 0.2)
        self.assertTrue(result["summary"]["smokeOk"])
        self.assertEqual(result["summary"]["hardFailRecall"], 1.0)
        # 缺陷 5：每个样本的 decision 与 D 裁判请求/原始响应都落盘
        for sid in ("s-good", "s-bad", "s-g2", "s-t1", "s-t2"):
            sd = evd / "samples" / sid
            self.assertTrue((sd / "decision-request.json").exists(), sid)
            self.assertTrue((sd / "decision-1-response.json").exists(), sid)
            self.assertTrue((sd / "llm-D1-request.json").exists(), sid)
            self.assertTrue((sd / "llm-D1-1-response.json").exists(), sid)
        # t2 的第三次调用失败也保存 {ok:false,error}
        payload = json.loads((evd / "samples" / "s-t2" / "llm-D1-3-response.json")
                             .read_bytes().decode("utf-8"))
        self.assertFalse(payload["ok"])
        self.assertIn("error", payload)
        # 响应含原始 body
        payload = json.loads((evd / "samples" / "s-good" / "llm-D1-1-response.json")
                             .read_bytes().decode("utf-8"))
        self.assertTrue(payload["ok"])
        self.assertIn("body", payload)

    def test_smoke_requires_q_and_d_labels_not_overall_accident(self):
        """坏例不能靠 D 失败掩盖 decision 全错；好例也不能因整例失败而误判方向吻合。"""
        def good(sid):
            return {"id": sid, "set": "smoke", "handLabeled": True, "dimId": "D1",
                    "expectedTrial": "passed",
                    "answers": {"r1": "理由见 决策-方案B.md §2。", "r2": "不矛盾。",
                                "r3": "不能恢复排期；旧方案已作废。"},
                    "access": [{"round": 1, "path": "决策-方案B.md"}],
                    "qLabels": {"Q1": "yes", "Q2": "yes", "Q3": "no"},
                    "dScores": {"D1": 5}, "hardViolation": {"D1": False}}
        samples = [good(f"s-qbad-{i}") for i in range(1, 7)]
        queue = []
        for _ in samples:
            # decision 三问全与人工标签相反；D 满分——旧口径会靠“整例 failed=预期外”掩盖 Q 错。
            queue += [decision_body("no", "no", "yes") for _ in range(3)]
            queue += [llm_body(5) for _ in range(3)]
        with mock.patch.object(judges, "llm_key", return_value="sk-fake"), \
                mock.patch.dict(os.environ, {"DASHSCOPE_API_KEY": "sk-fake"}):
            result = run_eval._calibrate(self.cfg, CASE, samples, self.root / "cal-qbad",
                                         votes=3, timeout_sec=5,
                                         transport=judge_transport(queue))
        self.assertFalse(result["summary"]["smokeOk"])
        row = result["findings"]["smoke"][0]
        self.assertFalse(row["qMatchesLabels"])
        self.assertTrue(row["dMatchesLabel"])
        self.assertFalse(row["directionOk"])


class TestFingerprint(unittest.TestCase):
    """配置指纹覆盖：裁判配置/阈值变化、评测源码变化都会改变指纹；
    baselines/ 不进自身指纹；基线退步只在指纹完全一致时判定。"""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.cfg_a = judges.load_judge_config(verified_judge_file(self.root))
        import copy
        self.cfg_b = copy.deepcopy(self.cfg_a)
        self.cfg_b["llm"]["threshold"] = 4

    def test_judge_cfg_threshold_changes_fingerprint(self):
        fa = run_eval._fingerprint(CASE, judge_cfg=self.cfg_a)
        fb = run_eval._fingerprint(CASE, judge_cfg=self.cfg_b)
        self.assertNotEqual(fa["judgeConfigSha256"], fb["judgeConfigSha256"])
        for k in ("caseSha256", "evalTreeSha256", "engineCommit"):
            self.assertEqual(fa[k], fb[k], k)

    def test_no_judge_cfg_marked_unversioned(self):
        fp = run_eval._fingerprint(CASE)
        self.assertEqual(fp["judgeConfigSha256"], "unversioned")

    def test_eval_source_change_changes_fingerprint_and_baselines_ignored(self):
        import shutil
        dst = self.root / "evalcopy"
        shutil.copytree(EVAL_DIR, dst,
                        ignore=shutil.ignore_patterns("__pycache__", "baselines"))
        fa = run_eval._fingerprint(CASE, judge_cfg=self.cfg_a, eval_dir=dst)
        # 改一个评测源码文件内容（只改临时副本，不动真实源码）→ 指纹变化
        f = dst / "grader.py"
        f.write_bytes(f.read_bytes() + b"\n# fingerprint-test\n")
        fb = run_eval._fingerprint(CASE, judge_cfg=self.cfg_a, eval_dir=dst)
        self.assertNotEqual(fa["evalTreeSha256"], fb["evalTreeSha256"])
        # baselines/（基线文件不能进入自身指纹）与 __pycache__ 被排除
        (dst / "baselines").mkdir(exist_ok=True)
        (dst / "baselines" / "x.baseline.json").write_text('{"pass^5": true}', encoding="utf-8")
        (dst / "__pycache__").mkdir(exist_ok=True)
        (dst / "__pycache__" / "junk.py").write_text("junk", encoding="utf-8")
        fc = run_eval._fingerprint(CASE, judge_cfg=self.cfg_a, eval_dir=dst)
        self.assertEqual(fb["evalTreeSha256"], fc["evalTreeSha256"])

    def test_baseline_regression_requires_exact_fingerprint(self):
        import copy
        fa = run_eval._fingerprint(CASE, judge_cfg=self.cfg_a)
        base = {"pass^5": True, "configFingerprint": fa}
        current = {"U": 0, "pass^5": False}
        self.assertIsNotNone(run_eval.grader.baseline_regression(base, current, fa))
        for tampered in ("judgeConfigSha256", "evalTreeSha256", "caseSha256"):
            fb = copy.deepcopy(fa)
            fb[tampered] = "0" * 64
            self.assertIsNone(run_eval.grader.baseline_regression(base, current, fb),
                              tampered)


if __name__ == "__main__":
    unittest.main()
