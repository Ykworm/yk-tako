#!/usr/bin/env python3
"""证据目录、原子写、脱敏扫描、报告与基线、rebuild。

所有 JSON 用「临时文件 + os.replace」写入；任何落盘内容先过 redact_scan
（token/密码/API key/Authorization 头不得进证据与报告，施工包 §4.3/§6.3）。
rebuild 只读已有证据重算检查与报告，不重调裁判、不重跑 trial（施工包 §9）。
"""
import datetime
import json
import os
import re
import secrets
import tempfile
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parent
REPO_ROOT = EVAL_DIR.parents[1]

SENSITIVE_KEY_RE = re.compile(
    r"(?i)(authorization\s*[:=]\s*bearer\s+)(?!\*)[A-Za-z0-9_\-.]{8,}"
    r"|(sk-[A-Za-z0-9]{8,})"
    r"|((?:DEEPSEEK|DASHSCOPE|OPENAI|ANTHROPIC|LANGFUSE)_(?:API|SECRET|PUBLIC)_?KEY\s*[:=]\s*)(?!\*)\S+"
    r"|(eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+)"          # JWT 三段式
)


def redact_scan(text, secrets_to_check=()):
    """扫描待落盘文本里的密钥痕迹。返回 findings 列表，空 = 干净。

    secrets_to_check 是本次运行已知的具体值（access/refresh token、密码、key），
    按值精确匹配；模式层再兜底 Authorization 头、sk- key、JWT、KEY= 赋值。
    """
    findings = []
    for value in secrets_to_check or ():
        if value and isinstance(value, str) and len(value) >= 8 and value in text:
            findings.append({"kind": "literal-secret", "hint": value[:4] + "…"})
    for m in SENSITIVE_KEY_RE.finditer(text or ""):
        findings.append({"kind": "pattern", "match": m.group(0)[:40] + "…"})
    return findings


# ---------- 证据目录 ----------

def new_evidence_dir(case_id, mode, base=None):
    """tmp/eval/<YYYYMMDD-HHMMSS>-<caseId>-<mode>/；同秒冲突追加短随机串。"""
    base = Path(base) if base else REPO_ROOT / "tmp" / "eval"
    ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    rand = ""
    for _ in range(10):
        cand = base / f"{ts}-{case_id}-{mode}{rand}"
        try:
            cand.mkdir(parents=True, exist_ok=False)
            return cand
        except FileExistsError:
            rand = "-" + secrets.token_hex(2)
    raise RuntimeError("证据目录同秒冲突重试耗尽")


def atomic_write_json(path, obj, secrets_to_check=()):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(obj, ensure_ascii=False, indent=1, sort_keys=False)
    findings = redact_scan(text, secrets_to_check)
    if findings:
        raise ValueError(f"拒绝写出含密钥痕迹的 JSON（{path}）：{findings[:3]}")
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
            f.write("\n")
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    return path


def atomic_write_text(path, text, secrets_to_check=()):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    findings = redact_scan(text, secrets_to_check)
    if findings:
        raise ValueError(f"拒绝写出含密钥痕迹的文本（{path}）：{findings[:3]}")
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    return path


# ---------- 报告 ----------

STATUS_MARK = {"passed": "✅", "failed": "❌", "invalid": "⚠️"}


def write_report_md(path, report):
    """人读报告：每个失败定位到轮次、原句/出处/工具调用，只给总分的报告不合格。"""
    lines = [f"# Eval 报告：{report['caseId']}",
             "",
             f"- 生成时间：{report.get('generatedAt', '')}",
             f"- 案例 SHA-256：`{report.get('caseSha256', '')[:16]}…`",
             f"- 配置指纹：引擎 {report.get('configFingerprint', {}).get('engineCommit', 'unversioned')}"
             f" / 模型 {report.get('configFingerprint', {}).get('model', 'unversioned')}",
             f"- 批次状态：**{report['batch']['reportStatus']}**"
             f"（P={report['batch']['P']} F={report['batch']['F']} U={report['batch']['U']}）"]
    if report["batch"].get("U", 0) == 0:
        lines.append(f"- 指标：pass@1={report['batch']['pass@1']}"
                     f" Pass@5={report['batch']['Pass@5']} pass^5={report['batch']['pass^5']}")
    else:
        lines.append("- 指标：incomplete（有无效 trial，三项指标为 null，不缩小分母）")
    lines += ["", "## 逐 trial 结果", ""]
    lines.append("| trial | 结果 | A1 | A3 | A4 | B1 | B2 | Q1 | Q2 | Q3 | D1 | D2 | D3 |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for t in report.get("trials", []):
        checks = t.get("checks", {})
        cells = [t.get("trialId", "?"), t.get("result", "?")]
        for k in ("A1", "A3", "A4", "B1", "B2", "Q1", "Q2", "Q3", "D1", "D2", "D3"):
            cells.append(STATUS_MARK.get(checks.get(k, {}).get("status"), "?"))
        lines.append("| " + " | ".join(cells) + " |")
    lines += ["", "## 失败定位", ""]
    located = False
    for t in report.get("trials", []):
        for name, check in sorted((t.get("checks") or {}).items()):
            if check.get("status") != "failed":
                continue
            located = True
            lines.append(f"### {t.get('trialId')} · {name}")
            lines.append(f"- 原因：{check.get('reason', '')}")
            ev = check.get("evidence")
            if ev is not None:
                lines.append("- 证据：")
                lines.append("```json")
                lines.append(json.dumps(ev, ensure_ascii=False, indent=1)[:2000])
                lines.append("```")
            lines.append("")
        if t.get("executionOutcome"):
            located = True
            lines.append(f"### {t.get('trialId')} · 执行异常")
            lines.append(f"- {t['executionOutcome']}")
            lines.append("")
    if not located:
        lines.append("（没有 failed 项；invalid 项见 checks.json 原始证据。）")
    lines += ["", "## 指标口径", "",
              "- pass@1 = P/5（本批单次成功率估计）；Pass@5 = 至少一次成功；pass^5 = 五次全过。",
              "- U>0 时批次 incomplete，三项指标一律 null，validTrialCount=P+F；不删失败样本、不缩小分母。",
              "- 完整报告可由 `run_eval.py rebuild --evidence-dir <本目录>` 从原始输入重算。"]
    atomic_write_text(path, "\n".join(lines) + "\n")


# ---------- 基线 ----------

class BaselineError(Exception):
    """基线写入被拒（条件不满足或文件已存在且未显式允许覆盖）。"""


def write_baseline(path, record, write_flag=False, replace_flag=False):
    """显式基线：U=0 且显式 --write-baseline 才写；已有文件必须再给 --replace-baseline。

    record 必须已含 {caseSha256, configFingerprint, n, trials, P, F, U, pass@1,
    Pass@5, pass^5}；不记录密钥。
    """
    path = Path(path)
    if record.get("U", 1) != 0:
        raise BaselineError("有无效 trial 的批次不能建立基线")
    if not write_flag:
        raise BaselineError("未显式给 --write-baseline")
    if path.exists() and not replace_flag:
        raise BaselineError(f"基线已存在：{path}；要覆盖必须再给 --replace-baseline")
    atomic_write_json(path, record)
    return path


def load_baseline(path):
    path = Path(path)
    if not path.exists():
        return None
    return json.loads(path.read_bytes().decode("utf-8"))


# ---------- rebuild ----------

def rebuild_checks(evidence_dir, case, run_grading):
    """从已有证据重算检查与报告。

    run_grading(trial_dir) -> {checks, result, executionOutcome} 由 run_eval 注入：
    只读 rounds/、citations.json、read-events.json、judges/ 与清单，重算 A/B；
    Q/D 只解析已保存的裁判原始响应（缺原始响应 → 对应检查 invalid），绝不重调裁判。
    """
    evidence_dir = Path(evidence_dir)
    trials = []
    for trial_dir in sorted((evidence_dir / "trials").glob("*")):
        if not trial_dir.is_dir():
            continue
        trials.append(run_grading(trial_dir))
    report = {"caseId": case.case_id, "caseSha256": case.raw_sha256,
              "generatedAt": datetime.datetime.now().isoformat(timespec="seconds"),
              "rebuilt": True, "trials": trials}
    return report
