#!/usr/bin/env python3
"""被测引擎的交互面：隔离账号、项目布局初始化、三轮执行、分页拉取、超时停止、终态核对。

复用 scripts/acceptance/harness.py 的 call()/.env 读取/路径常量（施工包 §3.2）；
token 临期刷新与 401 后重试一次借鉴 scripts/bench/arms/tako.py。
异常三分（案例 §6.2）：
- ExecutionFailure：被测系统执行失败或取证接口失败 → trial failed + executionOutcome，停止后续轮次；
- EvaluatorFault：投递/解析/存证是评测程序自己的故障 → trial invalid；
- PreconditionError：MCP 运行条件不满足 → trial invalid。
创建 run 的 500/网络中断等不确定结果绝不自动重投（messageId 不是服务端去重键）。
"""
import hashlib
import json
import os
import secrets
import sys
import time
from pathlib import Path, PurePosixPath

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts" / "acceptance"))

import casefile
import evidence as ev
import harness  # noqa: F401  (sys.path 已注入；复用 call/常量)

TERMINAL = ("complete", "error", "stopped", "failed")
EVAL_EMAIL_DOMAIN = "eval-doc-verify.tako.test"


class TargetError(Exception):
    def __init__(self, message="", **details):
        super().__init__(message)
        self.details = details


class ExecutionFailure(TargetError):
    """被测系统执行失败 / 取证接口失败：trial failed。"""


class EvaluatorFault(TargetError):
    """评测程序自身故障：trial invalid。"""


class PreconditionError(TargetError):
    """案例运行条件不满足（如 MCP 实际装配非空）：trial invalid。"""


def _data(body):
    return body.get("data") if isinstance(body, dict) else None


class Target:
    """一个 trial 一个实例：独立账号/租户/项目/session。"""

    def __init__(self, budget, ws_root=None, base=None, model=None):
        self.budget = budget
        self.base = (base or harness.BASE).rstrip("/")
        self.model = model or harness.MODEL
        self.ws_root = Path(ws_root or harness.WS_ROOT)
        self.auth = None            # {accessToken, refreshToken, expiresIn, issuedAt, tenant, userId}
        self.secrets = []           # 不落盘的具体值（token/密码/key 指纹外的原文）
        self.workspace_id = None
        self.tenant_id = None
        self.session_id = None
        self.mcp_json_sha256 = None
        # 当前 trial 的真实部分证据：run_trial 边执行边维护，异常时只保存这里已有的，
        # 绝不猜测、绝不混入其他 trial 的数据。
        self.trial_state = None     # {trialId, caseId, identity, initialManifest, rounds, finalManifest}
        self.stop_result = None     # 超时停止的保存结果（只用于执行异常）

    # ---------- 认证 ----------

    def signup(self):
        password = secrets.token_urlsafe(24)
        email = f"eval_{secrets.token_hex(6)}_{int(time.time())}@{EVAL_EMAIL_DOMAIN}"
        code, body = harness.call("POST", "/api/auth/signup",
                                  {"email": email, "password": password})
        if code != 201 or not _data(body):
            raise EvaluatorFault(f"注册失败：HTTP {code} {str(body)[:200]}")
        d = _data(body)
        tok = d.get("accessToken")
        if not tok:
            raise EvaluatorFault(f"注册响应缺 accessToken：{str(body)[:200]}")
        self.secrets += [password, tok, d.get("refreshToken") or ""]
        tenant = (d.get("tenant") or {}).get("id")
        if not tenant:
            raise EvaluatorFault("注册响应缺 tenant.id")
        self.auth = {"accessToken": tok, "refreshToken": d.get("refreshToken") or "",
                     "expiresIn": d.get("expiresIn") or 900,
                     "issuedAt": time.time(), "tenant": tenant,
                     "email": email, "userId": (d.get("user") or {}).get("id")}
        self.tenant_id = tenant
        return self.auth

    def _refresh(self):
        code, body = harness.call("POST", "/api/auth/refresh",
                                  {"refreshToken": self.auth["refreshToken"]})
        d = _data(body) or {}
        if code != 200 or not d.get("accessToken"):
            raise EvaluatorFault(f"刷新 token 失败：HTTP {code} {str(body)[:200]}")
        self.secrets.append(d["accessToken"])
        self.auth.update(accessToken=d["accessToken"],
                         refreshToken=d.get("refreshToken") or self.auth["refreshToken"],
                         expiresIn=d.get("expiresIn") or self.auth["expiresIn"],
                         issuedAt=time.time())
        return self.auth["accessToken"]

    def _token(self):
        a = self.auth
        if a["refreshToken"] and time.time() - a["issuedAt"] >= a["expiresIn"] - 60:
            return self._refresh()
        return a["accessToken"]

    def api(self, method, path, body=None):
        """打引擎；401 只刷新重试一次（bench 方式）。"""
        token = self._token()
        code, payload = harness.call(method, path, body, token)
        if code == 401 and self.auth.get("refreshToken"):
            token = self._refresh()
            code, payload = harness.call(method, path, body, token)
        return code, payload

    # ---------- 租户配置与 MCP 运行条件 ----------

    def configure_tenant(self, deepseek_key):
        if not deepseek_key:
            raise PreconditionError("缺 DEEPSEEK_API_KEY（TAKO_ENV），租户装不了模型密钥")
        body = {"modelRef": self.model, "mcpServers": {},
                "secrets": {"DEEPSEEK_API_KEY": deepseek_key},
                "sandbox": {"network": False}}
        code, resp = self.api("PUT", f"/api/tenants/{self.tenant_id}/runtime-config", body)
        if code != 200:
            raise EvaluatorFault(f"配租户失败：HTTP {code} {str(resp)[:200]}")
        self.secrets.append(deepseek_key)
        code, resp = self.api("GET", f"/api/tenants/{self.tenant_id}/runtime-config")
        if code != 200:
            raise EvaluatorFault(f"读租户配置失败：HTTP {code}")
        d = _data(resp) or {}
        if d.get("mcpServers") not in ({}, None):
            raise PreconditionError(f"租户 mcpServers 非空：{d.get('mcpServers')}")
        return {"modelRef": d.get("modelRef"), "mcpServers": d.get("mcpServers"),
                "sandbox": d.get("sandbox")}

    def mcp_status(self):
        code, resp = self.api("GET", f"/api/tenants/{self.tenant_id}/mcp-status")
        if code != 200:
            raise EvaluatorFault(f"mcp-status 失败：HTTP {code} {str(resp)[:200]}")
        d = _data(resp) or {}
        return {"system": d.get("system") or [], "custom": d.get("custom") or []}

    def check_mcp_empty(self, round_no, mcp_json_sha256):
        """每轮开跑前核对 MCP 实际装配为空；任何一项不满足 → trial invalid。"""
        status = self.mcp_status()
        problems = []
        if not mcp_json_sha256:
            problems.append("隔离 mcp.json 哈希缺失")
        if status["system"] or status["custom"]:
            problems.append(f"第{round_no}轮 mcp-status 非空")
        if problems:
            raise PreconditionError("；".join(problems))
        return status

    # ---------- 项目布局与输入文件 ----------

    def create_workspace(self, name):
        code, body = self.api("POST", "/api/workspaces", {"name": name})
        d = _data(body) or {}
        ws = d.get("workspace") or {}
        if code != 201 or not ws.get("id"):
            raise EvaluatorFault(f"建项目失败：HTTP {code} {str(body)[:200]}")
        self.workspace_id = ws["id"]
        return self.workspace_id

    def init_layout(self):
        """先 GET /api/workspaces/<wid>/files 让引擎建 layout.json 与 content/，再写 fixture。

        顺序不得反（assert_contract.py 的 seed_ws 同款）：先写文件就成了未迁移项目，
        入口全 409——本程序报错退出，绝不制造旧项目。
        """
        code, body = self.api("GET", f"/api/workspaces/{self.workspace_id}/files")
        if code != 200:
            raise EvaluatorFault(
                f"初始化项目布局失败：HTTP {code} {str(body)[:200]}（不写 fixture，不制造旧项目）")

    def _content_root(self):
        return self.ws_root / "tenants" / self.tenant_id / "workspaces" / self.workspace_id / "content"

    @staticmethod
    def _safe_rel(path):
        """fixture 只允许干净的相对 POSIX 路径：拒绝绝对、.. 、空段。"""
        if not isinstance(path, str) or "\\" in path:
            raise EvaluatorFault(f"fixture 路径非法：{path!r}")
        p = PurePosixPath(path)
        if p.is_absolute() or not p.parts or any(s in ("", ".", "..") for s in p.parts):
            raise EvaluatorFault(f"fixture 路径越界：{path!r}")
        return p

    def write_fixtures(self, files):
        """只在 content/ 下写四份 fixture；写完立刻记 {path, sha256} 初始清单。"""
        root = self._content_root()
        manifest = []
        for f in files:
            rel = self._safe_rel(f.path)
            dest = root.joinpath(*rel.parts)
            try:
                dest.parent.mkdir(parents=True, exist_ok=True)
                if dest.is_symlink():
                    raise EvaluatorFault(f"fixture 目标是符号链接：{f.path}")
                dest.write_bytes(f.content.encode("utf-8"))
            except OSError as e:
                raise EvaluatorFault(f"写 fixture 失败 {f.path}: {e}")
            manifest.append({"path": f.path,
                             "sha256": hashlib.sha256(f.content.encode("utf-8")).hexdigest()})
        return manifest

    def read_fixtures_final(self, files):
        """三轮结束后在原路径重读原始字节；缺失显式记 missing。"""
        root = self._content_root()
        final = []
        for f in files:
            rel = self._safe_rel(f.path)
            dest = root.joinpath(*rel.parts)
            try:
                if dest.is_symlink() or not dest.is_file():
                    final.append({"path": f.path, "sha256": None, "missing": True})
                    continue
                data = dest.read_bytes()
                final.append({"path": f.path,
                              "sha256": hashlib.sha256(data).hexdigest(),
                              "missing": False})
            except OSError as e:
                raise EvaluatorFault(f"终态核对读文件失败 {f.path}: {e}")
        return final

    # ---------- 三轮执行 ----------

    def create_run(self, text, message_id, session_id=None):
        body = {"text": text, "workspaceId": self.workspace_id, "messageId": message_id}
        if session_id:
            body["sessionId"] = session_id
        try:
            code, resp = self.api("POST", "/api/runs", body)
        except Exception as e:
            raise ExecutionFailure(f"创建 run 网络中断：{e}（不重投）")
        if code == 202:
            d = _data(resp) or {}
            if not d.get("runId"):
                raise ExecutionFailure(f"创建 run 响应缺 runId：{str(resp)[:200]}")
            return d
        if code == 500:
            raise ExecutionFailure(f"创建 run 返回 500（不重投）：{str(resp)[:200]}")
        raise EvaluatorFault(f"创建 run 返回 HTTP {code}：{str(resp)[:200]}")

    def wait_terminal(self, run_id, timeout_sec):
        deadline = time.time() + timeout_sec
        status = ""
        while time.time() < deadline:
            code, body = self.api("GET", f"/api/runs/{run_id}")
            if code != 200:
                raise ExecutionFailure(f"查询 run 终态失败：HTTP {code} {str(body)[:200]}")
            status = (_data(body) or {}).get("status") or ""
            if status in TERMINAL:
                return status
            time.sleep(self.budget["pollIntervalMs"] / 1000.0)
        stop_result = self.stop_run(run_id)
        raise ExecutionFailure(
            f"run 超过 {timeout_sec}s 未终态，已停止", stop_result=stop_result)

    def stop_run(self, run_id):
        """只用于执行异常（超时）；保存停止结果到 self.stop_result。"""
        try:
            code, body = self.api("POST", f"/api/runs/{run_id}/stop", {})
            self.stop_result = {"httpStatus": code,
                                "body": body if isinstance(body, (dict, str)) else str(body)[:300]}
        except Exception as e:
            self.stop_result = {"httpStatus": None, "error": str(e)[:200]}
        return self.stop_result

    # ---------- 取证 ----------

    def pull_frames(self, run_id):
        """eventOffset 排他游标，从 0 起，nextOffset 续拉到 terminal。"""
        frames, offset, stalls = [], 0, 0
        for _ in range(1000):
            code, body = self.api("GET", f"/api/runs/{run_id}/frames?eventOffset={offset}")
            if code != 200:
                raise ExecutionFailure(f"拉 frames 失败：HTTP {code} {str(body)[:200]}")
            d = _data(body) or {}
            page = d.get("frames") or []
            frames.extend(page)
            if any(f.get("kind") == "terminal" for f in page):
                return frames
            nxt = d.get("nextOffset")
            nxt = offset if nxt is None else nxt
            if not page and nxt == offset:
                stalls += 1
                if stalls >= max(3, self.budget["settleTimeoutSec"]):
                    return frames
                time.sleep(1)
            else:
                stalls = 0
                offset = nxt
        raise ExecutionFailure("拉 frames 超过 1000 页，疑似游标不前进")

    def pull_runlog(self, session_id):
        """runlogOffset 排他游标，从 0 起，续拉到空页。"""
        rows, offset = [], 0
        for _ in range(1000):
            code, body = self.api("GET", f"/api/sessions/{session_id}/log?runlogOffset={offset}")
            if code != 200:
                raise ExecutionFailure(f"拉 runlog 失败：HTTP {code} {str(body)[:200]}")
            d = _data(body) or {}
            page = d.get("rows") or []
            rows.extend(page)
            nxt = d.get("nextOffset")
            nxt = offset if nxt is None else nxt
            if not page:
                return rows
            offset = nxt
        raise ExecutionFailure("拉 runlog 超过 1000 页，疑似游标不前进")

    def wait_answer_row(self, session_id, run_id):
        """terminal 可能早于定稿历史可见：等主答案行落定，settleTimeoutSec 后仍缺行 → 取证失败。"""
        deadline = time.time() + self.budget["settleTimeoutSec"]
        while time.time() < deadline:
            rows = self.pull_runlog(session_id)
            row, problem = ev.main_answer_row(rows, run_id)
            if problem is None:
                return rows, row
            time.sleep(0.5)
        rows = self.pull_runlog(session_id)
        raise ExecutionFailure(f"settle {self.budget['settleTimeoutSec']}s 后主答案行仍缺（取证失败）")

    # ---------- trial 主流程 ----------

    def run_trial(self, case, trial_id, mcp_json_sha256, on_round=None):
        """执行一个 trial 的完整三轮；返回 self.trial_state（调用方负责落盘与判定）。

        on_round(round_no, payload) 让调用方逐轮存 api/frames/runlog/answer。
        任一 round 非 complete → ExecutionFailure（停止后续轮次）；MCP 非空 → PreconditionError。
        执行边推进边把真实部分写入 self.trial_state：异常时调用方只保存这里已有的，
        不猜测、不混入其他 trial。
        """
        self.trial_state = {"caseId": case.case_id, "trialId": trial_id,
                            "identity": None, "initialManifest": None,
                            "rounds": [], "finalManifest": None}
        self._current_trial_id = trial_id
        self.stop_result = None
        self.signup()
        self.configure_tenant(harness.api_key())
        self.create_workspace(f"eval-{case.case_id}-{trial_id}")
        self.init_layout()
        self.mcp_json_sha256 = mcp_json_sha256
        initial = self.write_fixtures(case.files)
        self.trial_state["initialManifest"] = initial
        self._refresh_identity(case)
        for idx, turn in enumerate(case.turns, start=1):
            self.check_mcp_empty(idx, mcp_json_sha256)
            message_id = f"m_{trial_id}_{turn.id}"
            run = self.create_run(turn.user, message_id,
                                  session_id=self.session_id)
            if self.session_id and run.get("sessionId") != self.session_id:
                raise EvaluatorFault("后续轮 sessionId 变化（应为同一 session）")
            self.session_id = run["sessionId"]
            self._refresh_identity(case)
            status = self.wait_terminal(run["runId"], self.budget["runTimeoutSec"])
            if status != "complete":
                raise ExecutionFailure(f"第{idx}轮终态 {status}（停止后续轮次）")
            frames = self.pull_frames(run["runId"])
            rows, answer_row = self.wait_answer_row(self.session_id, run["runId"])
            cmp = ev.compare_answer(answer_row, frames, run["runId"])
            if not cmp["ok"]:
                raise ExecutionFailure(f"第{idx}轮主答案核对失败：{cmp['reason']}（取证失败）")
            payload = {"turnId": turn.id, "runId": run["runId"], "messageId": message_id,
                       "api": run, "frames": frames, "runlog": rows,
                       "answer": {"row": answer_row, "compare": cmp}}
            if on_round:
                on_round(idx, payload)
            self.trial_state["rounds"].append(payload)
            self._refresh_identity(case)
        final = self.read_fixtures_final(case.files)
        state = self.trial_state
        state["finalManifest"] = final
        self._refresh_identity(case)
        return state

    def _refresh_identity(self, case):
        """identity 随执行增量刷新：中途失败时也能保存当前真实身份，不用事后猜。"""
        state = self.trial_state
        if state is None:
            return
        auth = self.auth or {}
        state["identity"] = {"caseId": case.case_id,
                             "trialId": state.get("trialId"),
                             "tenantId": self.tenant_id, "workspaceId": self.workspace_id,
                             "sessionId": self.session_id, "userId": auth.get("userId"),
                             "emailMasked": self._mask_email(auth.get("email")),
                             "runs": [{"turnId": r["turnId"], "runId": r["runId"]}
                                      for r in state["rounds"]]}

    @staticmethod
    def _mask_email(email):
        if not email or "@" not in email:
            return "unknown"
        name, domain = email.split("@", 1)
        return (name[:3] + "…@" + domain) if len(name) > 3 else ("…@" + domain)
