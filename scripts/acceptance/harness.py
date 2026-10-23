"""scripts/acceptance 的共享底座：HTTP 调用、.env 取密钥、psql 补丁、断言计数。

三个脚本（matrix.py / assert_contract.py / spotcheck.sh）共用这一份，
免得「三处各写一遍取密钥与拼 URL」——那种重复正是脚本烂掉的开始。

环境变量（全走变量，不再写死本机路径与端口）：
  TAKO_BASE    引擎地址，默认 http://127.0.0.1:8943
  TAKO_ENV    取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
  TAKO_MODEL  租户里配的模型 ref，默认 deepseek/deepseek-flash
  TAKO_PG_DSN 直连 PG 的 DSN（造「忙」闸要用；没给就跳过需要它的用例并明说）
  TAKO_PSQL   psql 可执行文件，默认 psql
  TAKO_WS_ROOT 引擎的 workspace 根（造项目目录里的文件要用），默认 <仓库根>/tmp/workspace-acc
"""
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
BASE = os.environ.get("TAKO_BASE", "http://127.0.0.1:8943").rstrip("/")
ENV_PATH = Path(os.environ.get("TAKO_ENV", REPO_ROOT / "yk-tako-eino" / ".env"))
MODEL = os.environ.get("TAKO_MODEL", "deepseek/deepseek-flash")
PG_DSN = os.environ.get("TAKO_PG_DSN", "")
PSQL = os.environ.get("TAKO_PSQL", "psql")
# 引擎的 workspace 根：项目目录在 <root>/tenants/<tid>/workspaces/<wid>/（scope.go）。
# 「往项目目录里造文件」这类步骤要用它——产出文件在文件系统上，SQL 造不出来。
WS_ROOT = Path(os.environ.get("TAKO_WS_ROOT", REPO_ROOT / "tmp" / "workspace-acc"))


def api_key():
    """从 .env 取模型密钥。取不到就早点退出——跑一半才炸最浪费时间。"""
    try:
        text = ENV_PATH.read_text()
    except OSError:
        print(f"读不到 .env：{ENV_PATH}（用 TAKO_ENV 指定）", file=sys.stderr)
        sys.exit(2)
    m = re.search(r'^DEEPSEEK_API_KEY="?([^"\n]+)"?', text, re.M)
    if not m or not m.group(1).strip():
        print(f"{ENV_PATH} 里没有 DEEPSEEK_API_KEY", file=sys.stderr)
        sys.exit(2)
    return m.group(1).strip()


def call(method, path, body=None, token=None, raw=False):
    """打一次引擎。返回 (http 码, 解析后的 json | 原始字符串 | 原始 bytes)。"""
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data) as r:
            code, payload = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, payload = e.code, e.read()
    if raw:
        return code, payload
    try:
        return code, json.loads(payload)
    except Exception:
        return code, payload.decode("utf-8", "replace")


def psql(sql):
    """直连 PG 跑一句 SQL。返回 (True, stdout) 或 (False, 原因)。

    psql 或 DSN 缺失时**不静默跳过**：调用方要把「为什么没跑」说清楚。
    """
    if not PG_DSN:
        return False, "没给 TAKO_PG_DSN（直连 PG 的 DSN）"
    try:
        r = subprocess.run([PSQL, PG_DSN, "-tAc", sql], capture_output=True, text=True, timeout=20)
    except FileNotFoundError:
        return False, f"找不到 {PSQL}（用 TAKO_PSQL 指定 psql 路径）"
    except subprocess.TimeoutExpired:
        return False, "psql 超时"
    if r.returncode != 0:
        return False, f"psql 失败：{(r.stderr or '').strip()[:200]}"
    return True, r.stdout.strip()


def pick(obj, path):
    """按 'data.tenant.id' 取值；取不到返回 None。"""
    cur = obj
    for part in path.split("."):
        if isinstance(cur, dict):
            cur = cur.get(part)
        else:
            return None
        if cur is None:
            return None
    return cur


def match(expected, actual):
    """按期望比对实际。

    - dict：**子集**语义（只断期望里点名的键）；要连键集与键序一起断，用 expect.data_keys。
    - '$any' 是通配（值会变但形状不变），可放在 dict 里当整对象通配。
    - list：逐位比对，长度必须相同。
    """
    if isinstance(expected, dict) and "$any" in expected:
        return True
    if isinstance(expected, dict):
        if not isinstance(actual, dict):
            return False
        return all(k in actual and match(v, actual[k]) for k, v in expected.items())
    if isinstance(expected, list):
        if not isinstance(actual, list) or len(expected) != len(actual):
            return False
        return all(match(e, a) for e, a in zip(expected, actual))
    return expected == actual


class Tally:
    """红绿计数。收尾用：FAIL > 0 → 退出码非零。"""

    def __init__(self):
        self.passed = 0
        self.failed = 0
        self.skipped = 0
        self.failures = []

    def ok(self, label):
        self.passed += 1
        print(f"  ✅ {label}")

    def no(self, label, detail=""):
        self.failed += 1
        self.failures.append(label)
        print(f"  ❌ {label}" + (f" —— {detail}" if detail else ""))

    def skip(self, label, why):
        self.skipped += 1
        print(f"  ⏭️  {label} —— 跳过：{why}")

    def summary(self):
        print()
        print(f"═══ PASS {self.passed} / FAIL {self.failed} / SKIP {self.skipped} ═══")
        for f in self.failures:
            print(f"  失败：{f}")
        return self.failed
