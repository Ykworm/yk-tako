#!/usr/bin/env python3
"""target：FakeEngine 打桩覆盖施工包 11.1 的执行边界。

- 项目布局未初始化时准备程序报错，而不是制造 409 的旧项目；
- runlog/frames 多页、terminal 早于 runlog、401 刷新一次、创建 run 不确定失败不重投；
- 超时停止调用 stop 并保存结果；MCP 运行条件不满足即 invalid。
"""
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import casefile  # noqa: E402
import target  # noqa: E402

CASE = casefile.load_case(EVAL_DIR / "cases" / "doc-verify.json")
BUDGET = dict(CASE.budget, pollIntervalMs=1, settleTimeoutSec=1, runTimeoutSec=5)


class FakeEngine:
    """按 (method, path) 前缀路由的桩引擎；记录调用供断言。"""

    def __init__(self):
        self.routes = {}
        self.calls = []
        self.tokens = {"access": "tok-1", "refresh": "ref-1"}

    def route(self, method, path_prefix, handler):
        self.routes[(method, path_prefix)] = handler

    def __call__(self, method, path, body=None, token=None):
        self.calls.append((method, path, body, token))
        # 最长前缀优先：/api/runs/x/frames 必须先于 /api/runs/x 命中。
        for (m, prefix), handler in sorted(self.routes.items(),
                                           key=lambda kv: -len(kv[0][1])):
            if m == method and path.startswith(prefix):
                return handler(body, path)
        return (404, {"code": 112001, "message": "not found", "data": None})


def signup_routes(fe, tenant="ten_1"):
    def signup(body, p=None):
        return (201, {"code": 0, "message": "", "data": {
            "accessToken": fe.tokens["access"], "refreshToken": fe.tokens["refresh"],
            "expiresIn": 900,
            "tenant": {"id": tenant, "kind": "personal", "name": "t"},
            "user": {"id": "u_1"}}})
    fe.route("POST", "/api/auth/signup", signup)

    def refresh(body, p=None):
        if body.get("refreshToken") == "ref-1":
            fe.tokens["access"] = "tok-2"
            return (200, {"code": 0, "message": "", "data": {
                "accessToken": "tok-2", "refreshToken": "ref-1", "expiresIn": 900}})
        return (401, {"code": 111103, "message": "bad refresh", "data": None})
    fe.route("POST", "/api/auth/refresh", refresh)
    return tenant


def config_routes(fe, tenant):
    fe.route("PUT", f"/api/tenants/{tenant}/runtime-config",
             lambda b, p=None: (200, {"code": 0, "message": "", "data": {}}))
    fe.route("GET", f"/api/tenants/{tenant}/runtime-config",
             lambda b, p=None: (200, {"code": 0, "message": "", "data":
                              {"modelRef": "deepseek/deepseek-flash", "mcpServers": {},
                               "sandbox": {"network": False}}}))
    fe.route("GET", f"/api/tenants/{tenant}/mcp-status",
             lambda b, p=None: (200, {"code": 0, "message": "", "data": {"system": [], "custom": []}}))


def workspace_routes(fe, wid="ws_1"):
    fe.route("POST", "/api/workspaces",
             lambda b, p=None: (201, {"code": 0, "message": "", "data":
                              {"workspace": {"id": wid, "name": b["name"]}}}))
    return wid


def files_route(fe, wid, code=200):
    if code == 200:
        fe.route("GET", f"/api/workspaces/{wid}/files",
                 lambda b, p=None: (200, {"code": 0, "message": "", "data": {"files": []}}))
    else:
        fe.route("GET", f"/api/workspaces/{wid}/files",
                 lambda b, p=None: (code, {"code": 117116, "message": "项目需要迁移", "data": None}))


def default_pages(run_id):
    return [([{"eventOffset": 1, "kind": "start", "agent": "", "model": "",
              "messageId": "m"},
             {"eventOffset": 2, "kind": "delta", "agent": "", "model": "m",
              "text": "答案"},
             {"eventOffset": 3, "kind": "terminal", "agent": "", "model": "",
              "status": "complete"}],
            [{"runlogOffset": 1, "runId": run_id, "role": "user", "agent": "",
              "text": "q"},
             {"runlogOffset": 2, "runId": run_id, "role": "assistant",
              "agent": "", "text": "答案"}])]


def run_routes(fe, run_id="run_1", session_id="s_1", pages=None, register_log=True,
               create=True):
    """pages: [(frames_page, runlog_page)]。register_log=False 时不注册 session log
    （standard_setup 会把三轮的 runlog 累计成一条 session 路由，避免互相覆盖）；
    create=False 时不注册 POST /api/runs（由调用方统一按序发 runId）。"""
    pages = pages if pages is not None else default_pages(run_id)
    for frames_page, runlog_page in pages:
        delta = "".join(f.get("text", "") for f in frames_page
                        if f.get("kind") == "delta" and not f.get("agent"))
        assistants = [r["text"] for r in runlog_page
                      if r.get("role") == "assistant" and not r.get("agent")]
        assert len(assistants) <= 1
        if assistants:
            assert assistants[0] == delta, "fixture 必须满足主答案字节一致"

    state = {"frames_asked": 0, "run_status": ["complete"]}

    if create:
        def create(body, p=None):
            if body.get("text") is None:
                return (500, {"code": 111901, "message": "服务暂时不可用", "data": None})
            d = {"runId": run_id, "sessionId": body.get("sessionId") or session_id,
                 "messageId": body["messageId"]}
            return (202, {"code": 0, "message": "", "data": d})
        fe.route("POST", "/api/runs", create)

    def run_status(body, p=None):
        status = state["run_status"][0]
        return (200, {"code": 0, "message": "", "data": {"status": status}})
    fe.route("GET", f"/api/runs/{run_id}", run_status)

    def stop(body, p=None):
        return (200, {"code": 0, "message": "", "data": {"stopped": True}})
    fe.route("POST", f"/api/runs/{run_id}/stop", stop)

    def frames(body, p=None):
        idx = state["frames_asked"]
        state["frames_asked"] += 1
        if idx >= len(pages):
            return (200, {"code": 0, "message": "", "data": {"frames": [], "nextOffset": 0}})
        page = pages[idx][0]
        nxt = max(f["eventOffset"] for f in page) if page else 0
        return (200, {"code": 0, "message": "", "data": {"frames": page,
                                                          "nextOffset": nxt}})
    fe.route("GET", f"/api/runs/{run_id}/frames", frames)

    if register_log:
        session_log_route(fe, session_id, [p[1] for p in pages])
    return state


def session_log_route(fe, session_id, snapshots):
    """session 级 runlog：snapshots = 每次重拉（runlogOffset=0）时可见的全量行快照，
    按 runlogOffset 排他过滤——模拟「terminal 早于定稿历史」的 settle 过程。"""
    import re as _re
    state = {"snap": -1}

    def log(body, p=None):
        m = _re.search(r"runlogOffset=(\d+)", p or "")
        offset = int(m.group(1)) if m else 0
        if offset == 0:
            state["snap"] = min(state["snap"] + 1, len(snapshots) - 1)
        snap = snapshots[max(state["snap"], 0)]
        rows = [r for r in snap if r["runlogOffset"] > offset]
        nxt = max((r["runlogOffset"] for r in rows), default=offset)
        return (200, {"code": 0, "message": "", "data": {"rows": rows,
                                                          "nextOffset": nxt}})
    fe.route("GET", f"/api/sessions/{session_id}/log", log)


class TargetCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.ws_root = Path(self.tmp.name) / "ws"
        self.ws_root.mkdir()
        self.fe = FakeEngine()
        self._key_patcher = mock.patch("harness.api_key", return_value="sk-fake-key")
        self._key_patcher.start()
        self.addCleanup(self._key_patcher.stop)

    def start_engine(self, fn=None):
        patcher = mock.patch("harness.call", fn or self.fe)
        patcher.start()
        self.addCleanup(patcher.stop)

    def new_target(self):
        return target.Target(BUDGET, ws_root=self.ws_root)

    def standard_setup(self, pages=None, per_round=None):
        """per_round: 每轮一组 pages；session log 按轮累计注册一次。"""
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)

        def round_pages(i):
            if per_round:
                return per_round[i - 1]
            if pages:
                return pages
            return default_pages(f"run_{i}")

        def round_rows(i):
            seen = {}
            for p in (round_pages(i) or []):
                for r in p[1]:
                    seen[r["runlogOffset"]] = r
            return [seen[k] for k in sorted(seen)]

        # POST /api/runs 全 session 只注册一次，按调用顺序发 run_1/run_2/run_3。
        counter = {"n": 0}

        def create(body, p=None):
            counter["n"] += 1
            if body.get("text") is None:
                return (500, {"code": 111901, "message": "服务暂时不可用", "data": None})
            return (202, {"code": 0, "message": "", "data": {
                "runId": f"run_{counter['n']}",
                "sessionId": body.get("sessionId") or "s_1",
                "messageId": body["messageId"]}})
        self.fe.route("POST", "/api/runs", create)

        acc = []
        snapshots = []
        for i in (1, 2, 3):
            run_routes(self.fe, run_id=f"run_{i}", session_id="s_1",
                       pages=round_pages(i), register_log=False, create=False)
            acc = acc + round_rows(i)
            snapshots.append(list(acc))
        session_log_route(self.fe, "s_1", snapshots)
        return tenant, wid


class TestLayoutOrder(TargetCase):
    def test_layout_not_initialized_aborts_before_fixture_write(self):
        self.start_engine()
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid, code=409)
        t = self.new_target()
        t.signup()
        t.configure_tenant("sk-fake")
        t.create_workspace("x")
        with self.assertRaises(target.EvaluatorFault):
            t.init_layout()
        content = self.ws_root / "tenants" / tenant / "workspaces" / wid / "content"
        self.assertFalse(content.exists(), "布局初始化失败时不许写 fixture（不制造旧项目）")

    def test_layout_initialized_then_fixtures_written(self):
        self.start_engine()
        tenant, wid = self.standard_setup()
        t = self.new_target()
        t.signup()
        t.configure_tenant("sk-fake")
        t.create_workspace("x")
        t.init_layout()
        manifest = t.write_fixtures(CASE.files)
        self.assertEqual(len(manifest), 4)
        final = t.read_fixtures_final(CASE.files)
        stripped = [{"path": f["path"], "sha256": f["sha256"]} for f in final]
        self.assertEqual(manifest, stripped)
        # 顺序断言：GET files 必须先于写 fixture（这里体现为先于建 run）
        methods = [c[:2] for c in self.fe.calls]
        files_idx = methods.index(("GET", f"/api/workspaces/{wid}/files"))
        self.assertNotIn(("POST", "/api/runs"), methods[:files_idx])


class TestFixtureSafety(TargetCase):
    def test_dotdot_and_absolute_rejected(self):
        t = self.new_target()
        with self.assertRaises(target.EvaluatorFault):
            t._safe_rel("../x.md")
        with self.assertRaises(target.EvaluatorFault):
            t._safe_rel("/abs/x.md")

    def test_symlink_rejected(self):
        self.start_engine()
        tenant, wid = self.standard_setup()
        t = self.new_target()
        t.signup()
        t.create_workspace("x")
        t.init_layout()
        content = self.ws_root / "tenants" / tenant / "workspaces" / wid / "content"
        dest = content / "决策-方案B.md"
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.symlink_to("/etc/passwd")
        with self.assertRaises(target.EvaluatorFault):
            t.write_fixtures(CASE.files)


class TestThreeRounds(TargetCase):
    def test_full_trial_three_rounds_same_session(self):
        self.start_engine()
        self.standard_setup()
        t = self.new_target()
        bundle = t.run_trial(CASE, "t1", "a" * 64)
        self.assertEqual(len(bundle["rounds"]), 3)
        self.assertEqual(bundle["identity"]["sessionId"], "s_1")
        self.assertEqual(len({r["runId"] for r in bundle["rounds"]}), 3)
        for r in bundle["rounds"]:
            self.assertTrue(r["answer"]["compare"]["ok"])
        mcp_calls = [c for c in self.fe.calls if "mcp-status" in c[1]]
        self.assertEqual(len(mcp_calls), 3)

    def test_multi_page_frames_and_runlog(self):
        self.start_engine()
        pages = [([{"eventOffset": 1, "kind": "start", "agent": "", "model": "", "messageId": "m"}],
                  [{"runlogOffset": 1, "runId": "run_1", "role": "user", "agent": "", "text": "q"}]),
                 ([{"eventOffset": 2, "kind": "delta", "agent": "", "model": "m", "text": "答案"},
                   {"eventOffset": 3, "kind": "terminal", "agent": "", "model": "", "status": "complete"}],
                  [{"runlogOffset": 2, "runId": "run_1", "role": "assistant", "agent": "", "text": "答案"}])]
        self.standard_setup(pages=pages)
        t = self.new_target()
        t.signup()
        frames = t.pull_frames("run_1")
        rows = t.pull_runlog("s_1")
        self.assertEqual(len(frames), 3)
        self.assertEqual(len(rows), 2)

    def test_terminal_before_runlog_settle_waits(self):
        self.start_engine()
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)
        run_routes(self.fe, run_id="run_1", session_id="s_1", register_log=False)
        # 第 1 次重拉：terminal 已可见但定稿历史还没有 assistant 行；第 2 次才有。
        session_log_route(self.fe, "s_1", [
            [{"runlogOffset": 1, "runId": "run_1", "role": "user", "agent": "", "text": "q"}],
            [{"runlogOffset": 1, "runId": "run_1", "role": "user", "agent": "", "text": "q"},
             {"runlogOffset": 2, "runId": "run_1", "role": "assistant", "agent": "", "text": ""}],
        ])
        t = self.new_target()
        t.signup()
        rows, ans = t.wait_answer_row("s_1", "run_1")
        self.assertIsNotNone(ans)

    def test_401_refreshes_once_and_retries(self):
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)
        used_tokens = []

        def guarded(method, path, body=None, token=None):
            used_tokens.append(token)
            if token == "tok-1" and path.startswith(f"/api/workspaces/{wid}/files"):
                return (401, {"code": 111102, "message": "请先登录", "data": None})
            return self.fe(method, path, body, token)

        self.start_engine(guarded)
        t = self.new_target()
        t.signup()
        code, _ = t.api("GET", f"/api/workspaces/{wid}/files")
        self.assertEqual(code, 200)
        bearer = [x for x in used_tokens if x]
        self.assertEqual(bearer, ["tok-1", "tok-2"])   # 401 后刷新一次并重试

    def test_persistent_401_not_retried_twice(self):
        tenant = signup_routes(self.fe)
        used_tokens = []

        def guarded(method, path, body=None, token=None):
            used_tokens.append(token)
            if path.startswith(f"/api/tenants/{tenant}/runtime-config"):
                return (401, {"code": 111102, "message": "请先登录", "data": None})
            return self.fe(method, path, body, token)

        self.start_engine(guarded)
        t = self.new_target()
        t.signup()
        # configure 走 api()：第一次 401 → 刷新 → 第二次仍 401 → 不再重试，原样上抛
        with self.assertRaises(target.EvaluatorFault):
            t.configure_tenant("sk-fake")
        bearer = [x for x in used_tokens if x]
        self.assertEqual(bearer, ["tok-1", "tok-2"])

    def test_create_run_500_no_retry(self):
        self.start_engine()
        self.standard_setup()
        t = self.new_target()
        t.signup()
        with self.assertRaises(target.ExecutionFailure):
            t.create_run(None, "m_1")
        creates = [c for c in self.fe.calls if c[0] == "POST" and c[1] == "/api/runs"]
        self.assertEqual(len(creates), 1)   # 不确定失败不自动重投

    def test_non_complete_terminal_stops_following_rounds(self):
        self.start_engine()
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)
        state = run_routes(self.fe, run_id="run_1")
        state["run_status"][0] = "error"
        t = self.new_target()
        with self.assertRaises(target.ExecutionFailure) as ctx:
            t.run_trial(CASE, "t1", "a" * 64)
        self.assertIn("error", str(ctx.exception))
        creates = [c for c in self.fe.calls if c[1] == "/api/runs"]
        self.assertEqual(len(creates), 1)   # 后续轮次被停止

    def test_timeout_calls_stop(self):
        self.start_engine()
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)
        state = run_routes(self.fe, run_id="run_1")
        state["run_status"][0] = "running"
        budget = dict(BUDGET, runTimeoutSec=1, pollIntervalMs=10)
        t = target.Target(budget, ws_root=self.ws_root)
        with self.assertRaises(target.ExecutionFailure) as ctx:
            t.run_trial(CASE, "t1", "a" * 64)
        stops = [c for c in self.fe.calls if c[1] == "/api/runs/run_1/stop"]
        self.assertEqual(len(stops), 1)
        self.assertIn("已停止", str(ctx.exception))

    def test_mcp_nonempty_invalid(self):
        self.start_engine()
        tenant = signup_routes(self.fe)
        config_routes(self.fe, tenant)
        wid = workspace_routes(self.fe)
        files_route(self.fe, wid)
        self.fe.route("GET", f"/api/tenants/{tenant}/mcp-status",
                      lambda b, p=None: (200, {"code": 0, "message": "", "data":
                                       {"system": [{"name": "fetch", "state": "connected"}],
                                        "custom": []}}))
        t = self.new_target()
        with self.assertRaises(target.PreconditionError):
            t.run_trial(CASE, "t1", "a" * 64)


if __name__ == "__main__":
    unittest.main()
