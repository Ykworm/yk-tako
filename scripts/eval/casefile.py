#!/usr/bin/env python3
"""案例 JSON 的机器真源加载、fixture 清单与小节锚点、配置指纹。

案例规格（doc/83-场景化Eval方案/案例-01-文档查证.md）的人读版在 doc/，
本模块加载 scripts/eval/cases/doc-verify.json：缺字段、未知版本、重复路径、
空台词或非法预算一律 CaseError 退出，不带着半份案例运行。
"""
import hashlib
import json
import re
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

SCHEMA_VERSION = 1
BUDGET_KEYS = ("trials", "runTimeoutSec", "settleTimeoutSec",
               "pollIntervalMs", "judgeVotes", "judgeTimeoutSec")
QUESTION_IDS = ("Q1", "Q2", "Q3")
DIMENSION_IDS = ("D1", "D2", "D3")

_SECTION_RE = re.compile(r"^##\s+(\d+)\.\s*(.+?)\s*$", re.M)


class CaseError(Exception):
    """案例文件不合法：调用方直接退出，不降级运行。"""


@dataclass
class Fixture:
    path: str
    content: str

    @property
    def sha256(self):
        return hashlib.sha256(self.content.encode("utf-8")).hexdigest()


@dataclass
class Turn:
    id: str
    user: str
    expected: list


@dataclass
class Case:
    case_id: str
    title: str
    files: list            # list[Fixture]，顺序即案例顺序
    turns: list            # list[Turn]，恰好三轮
    judgment_boundary: str
    decision_questions: dict
    dimensions: dict
    budget: dict
    raw_sha256: str
    source_path: str

    @property
    def fixture_paths(self):
        return [f.path for f in self.files]

    def fixture(self, path):
        for f in self.files:
            if f.path == path:
                return f
        return None


def _require(cond, msg):
    if not cond:
        raise CaseError(msg)


def _check_rel_path(path, what):
    _require(isinstance(path, str) and path.strip(), f"{what} 路径为空或不是字符串")
    _require("\\" not in path, f"{what} 路径含反斜杠（只接受 POSIX 相对路径）：{path!r}")
    p = PurePosixPath(path)
    _require(not p.is_absolute(), f"{what} 路径是绝对路径：{path!r}")
    parts = p.parts
    _require(parts and parts[0] not in ("", ".", ".."), f"{what} 路径越界或为空：{path!r}")
    _require(".." not in parts, f"{what} 路径含 '..'：{path!r}")
    _require(all(part not in ("", ".", "..") for part in parts), f"{what} 路径含空段或 '.'：{path!r}")


def load_case(path):
    """加载并校验案例 JSON；任何不合法都抛 CaseError。"""
    path = Path(path)
    raw = path.read_bytes()
    try:
        obj = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise CaseError(f"案例文件不是合法 UTF-8 JSON：{e}")
    _require(isinstance(obj, dict), "案例文件顶层必须是 JSON 对象")

    _require(obj.get("schemaVersion") == SCHEMA_VERSION,
             f"未知 schemaVersion：{obj.get('schemaVersion')!r}（只接受 {SCHEMA_VERSION}）")
    case_id = obj.get("id")
    _require(isinstance(case_id, str) and case_id.strip(), "案例 id 为空")
    title = obj.get("title")
    _require(isinstance(title, str) and title.strip(), "案例 title 为空")

    files = []
    seen = set()
    for i, item in enumerate(obj.get("files") or []):
        _require(isinstance(item, dict), f"files[{i}] 不是对象")
        rel, content = item.get("path"), item.get("content")
        _check_rel_path(rel, f"files[{i}]")
        _require(rel not in seen, f"重复的文件路径：{rel}")
        seen.add(rel)
        _require(isinstance(content, str) and content != "", f"files[{i}] 内容为空")
        files.append(Fixture(path=rel, content=content))
    _require(files, "files 为空")

    turns = []
    tids = set()
    for i, item in enumerate(obj.get("turns") or []):
        _require(isinstance(item, dict), f"turns[{i}] 不是对象")
        tid, user = item.get("id"), item.get("user")
        _require(isinstance(tid, str) and tid.strip(), f"turns[{i}].id 为空")
        _require(tid not in tids, f"重复的 turn id：{tid}")
        tids.add(tid)
        _require(isinstance(user, str) and user.strip(), f"turn {tid} 的台词为空")
        expected = item.get("expected") or []
        _require(isinstance(expected, list) and all(isinstance(e, str) for e in expected),
                 f"turn {tid} 的 expected 必须是字符串数组")
        turns.append(Turn(id=tid, user=user, expected=expected))
    _require(len(turns) == 3, f"本案例固定三轮，实际 {len(turns)} 轮")

    boundary = obj.get("judgmentBoundary")
    _require(isinstance(boundary, str) and boundary.strip(), "judgmentBoundary 为空")

    questions = obj.get("decisionQuestions") or {}
    for qid in QUESTION_IDS:
        q = questions.get(qid)
        _require(isinstance(q, dict), f"缺 decisionQuestions.{qid}")
        _require(q.get("type") == "noul", f"{qid}.type 必须是 noul，实际 {q.get('type')!r}")
        _require(isinstance(q.get("instructions"), str) and q["instructions"].strip(),
                 f"{qid}.instructions 为空")
    extra_q = set(questions) - set(QUESTION_IDS)
    _require(not extra_q, f"decisionQuestions 有未知问题：{sorted(extra_q)}")

    dimensions = obj.get("dimensions") or {}
    dim_rounds = set()
    for did in DIMENSION_IDS:
        d = dimensions.get(did)
        _require(isinstance(d, dict), f"缺 dimensions.{did}")
        _require(d.get("round") in (1, 2, 3), f"{did}.round 必须是 1/2/3，实际 {d.get('round')!r}")
        _require(did not in dim_rounds, f"dimensions 轮次重复：{d.get('round')}")
        dim_rounds.add(did)
        _require(isinstance(d.get("criteria"), str) and d["criteria"].strip(), f"{did}.criteria 为空")
    extra_d = set(dimensions) - set(DIMENSION_IDS)
    _require(not extra_d, f"dimensions 有未知维度：{sorted(extra_d)}")

    budget = obj.get("budget") or {}
    for key in BUDGET_KEYS:
        v = budget.get(key)
        _require(isinstance(v, int) and not isinstance(v, bool) and v > 0,
                 f"budget.{key} 必须是正整数，实际 {v!r}")
    _require(budget["judgeVotes"] == 3, f"judgeVotes 固定为 3，实际 {budget['judgeVotes']}")

    return Case(case_id=case_id, title=title, files=files, turns=turns,
                judgment_boundary=boundary, decision_questions=questions,
                dimensions=dimensions, budget=budget,
                raw_sha256=hashlib.sha256(raw).hexdigest(), source_path=str(path))


def fixture_sections(fixture):
    """从 `## N. 标题` 解析小节：{num:int -> title:str}。空文件返回 {}。"""
    return {int(m.group(1)): m.group(2) for m in _SECTION_RE.finditer(fixture.content)}


def fixture_anchors(fixture):
    """由 `## N. 标题` 生成锚点集合（如 "2-否决理由"），供 Markdown 链接 fragment 匹配。"""
    return {f"{num}-{title}" for num, title in fixture_sections(fixture).items()}


def fixture_manifest(files):
    """初始清单：[{path, sha256}]，sha256 按原始 UTF-8 字节。"""
    return [{"path": f.path, "sha256": f.sha256} for f in files]


def config_fingerprint(case_sha256, model, engine_commit, engine_dirty, mcp_sha256,
                       tree_sha256, extra=None):
    """配置指纹：同一案例同一配置才能与基线比较。只放标识，不放密钥。

    engine_commit/dirty 描述引擎构建来源；mcp_sha256 是运行目录 mcp.json 的哈希；
    tree_sha256 是 cmd/yktako、internal/config、internal/runengine、agents 的相对路径+字节哈希。
    取不到的项记 "unversioned"，不猜补。
    """
    fp = {"caseSha256": case_sha256, "model": model,
          "engineCommit": engine_commit or "unversioned",
          "engineDirty": bool(engine_dirty),
          "mcpJsonSha256": mcp_sha256 or "unversioned",
          "sourceTreeSha256": tree_sha256 or "unversioned"}
    fp.update(extra or {})
    return fp
