#!/usr/bin/env python3
"""两类裁判的客户端与响应适配：decision-model-preview 与 DeepSeek LLM 裁判。

纪律（施工包 §2.3、§8.1、§6.3）：
- 只做真实客户端与**可配置**的响应适配：响应里哪个字段是哪个答案，全部由
  cases/doc-verify.judge.json 的 field path 与值映射声明；本模块不内置任何
  「实测前就当真」的响应格式。配置里 status != "verified" 的解析规则一旦被使用，
  抛 JudgeConfigError——宁可判红也不猜。
- 密钥不进进程参数：外部 HTTP 走 curl 子进程，认证头经 stdin 的 curl config（-K -）传入。
- decision 密钥只从进程环境 DASHSCOPE_API_KEY 读；DeepSeek key 复用 harness 的
  TAKO_ENV 读取；两者都不落盘、不进报告。
- 本模块不主动发起调用；调用方（run/calibrate/probe --execute）显式触发。
"""
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parent
REPO_ROOT = EVAL_DIR.parents[1]
sys.path.insert(0, str(REPO_ROOT / "scripts" / "acceptance"))
import harness  # noqa: E402

DEFAULT_DECISION_URL = ("https://llm-d7zdt4gmth2v7k44.cn-beijing.maas.aliyuncs.com"
                        "/compatible-mode/v1/systemone")
DEFAULT_LLM_URL = "https://api.deepseek.com/v1/chat/completions"
Q_DIRECTIONS = {"Q1": "yes_pass", "Q2": "yes_pass", "Q3": "no_pass"}
Q_IDS = ("Q1", "Q2", "Q3")
D_IDS = ("D1", "D2", "D3")


class JudgeConfigError(Exception):
    """裁判配置不可用（未实测、字段缺失、值映射缺失）。"""


class JudgeParseError(Exception):
    """单次响应不符合已固定的解析规则（计入 e，不重试）。"""


class JudgeCallError(Exception):
    """单次调用失败：HTTP 非 2xx、超时、网络错误（计入 e，不重试）。"""


# ---------- 配置 ----------

def load_judge_config(path):
    path = Path(path)
    if not path.exists():
        raise JudgeConfigError(f"裁判配置不存在：{path}")
    cfg = json.loads(path.read_bytes().decode("utf-8"))
    if not isinstance(cfg, dict):
        raise JudgeConfigError("裁判配置顶层必须是对象")
    for kind in ("decision", "llm"):
        section = cfg.get(kind)
        if not isinstance(section, dict):
            raise JudgeConfigError(f"裁判配置缺 {kind} 段")
        if not isinstance(section.get("url"), str) or not section["url"].startswith("https://"):
            raise JudgeConfigError(f"{kind}.url 必须是 https URL")
        parse = section.get("responseParse")
        if not isinstance(parse, dict) or parse.get("status") not in ("unverified", "verified"):
            raise JudgeConfigError(f"{kind}.responseParse.status 必须是 unverified/verified")
    return cfg


def require_verified(cfg, kind):
    """实测固定（status=verified）之前，正式解析一律拒绝。"""
    parse = cfg[kind].get("responseParse") or {}
    if parse.get("status") != "verified":
        raise JudgeConfigError(
            f"{kind} 响应解析规则尚未实测固定（status={parse.get('status')!r}）；"
            f"先跑 probe --kind {kind} --execute 并把确认后的路径写回 judge.json")


# ---------- 可配置解析 ----------

def dig(obj, path):
    """按配置的路径取值：'a.b.0.c' 或 ['a','b','0','c']；任何一步取不到返回 None。"""
    if path in (None, ""):
        return None
    parts = path.split(".") if isinstance(path, str) else list(path)
    cur = obj
    for part in parts:
        if isinstance(cur, dict):
            cur = cur.get(part)
        elif isinstance(cur, list) and part.isdigit() and int(part) < len(cur):
            cur = cur[int(part)]
        else:
            return None
    return cur


def _map_value(raw, value_map, what):
    if not isinstance(value_map, dict) or not value_map:
        raise JudgeConfigError(f"{what} 缺值映射 valueMap")
    if isinstance(raw, str):
        key = raw.strip().lower()
        if key in value_map:
            return value_map[key]
    for k, v in value_map.items():
        if isinstance(raw, (int, float, bool)) and str(raw).lower() == k:
            return v
    raise JudgeParseError(f"{what} 的值 {raw!r} 不在已固定的映射表内")


def extract_decision_answers(body, cfg):
    """decision 响应 → {Q1: 'yes'|'no'|'unknown', ...}。

    每个问题的字段路径来自 responseParse.questions.{Q}.path；服务原始值经 valueMap
    归一为内部 yes/no/unknown。只有服务明确表达无法判断时才映射 unknown。
    路径未配置/取不到/值不在映射表 → JudgeParseError（该项计 e）。
    """
    parse = cfg["decision"].get("responseParse") or {}
    qpaths = parse.get("questions") or {}
    value_map = parse.get("valueMap") or {}
    out = {}
    for qid in Q_IDS:
        spec = qpaths.get(qid) or {}
        raw = dig(body, spec.get("path"))
        if raw is None:
            raise JudgeParseError(f"decision 响应缺 {qid}（path={spec.get('path')!r}）")
        out[qid] = _map_value(raw, value_map, f"decision.{qid}")
        if out[qid] not in ("yes", "no", "unknown"):
            raise JudgeParseError(f"{qid} 归一化结果非法：{out[qid]!r}")
    return out


def extract_llm_verdict(body, cfg):
    """LLM 裁判响应 → {score, unknown, hardViolation, evidence}。

    contentPath 取出生成文本（JSON 串），再按 scorePath/unknownPath/hardViolationPath/
    evidencePath 在解析出的 JSON 对象内取值。hardViolation 的判定优先于分数：
    共同硬性准则违规的已知评分必须落在失败档（由 d_item_result 兑现）。
    """
    parse = cfg["llm"].get("responseParse") or {}
    content = dig(body, parse.get("contentPath"))
    if content is None:
        raise JudgeParseError(f"LLM 响应缺生成文本（contentPath={parse.get('contentPath')!r}）")
    if isinstance(content, str):
        try:
            payload = json.loads(content)
        except json.JSONDecodeError as e:
            raise JudgeParseError(f"LLM 输出不是合法 JSON：{e}")
    else:
        payload = content
    if not isinstance(payload, dict):
        raise JudgeParseError("LLM 输出 JSON 顶层不是对象")
    unknown_raw = dig(payload, parse.get("unknownPath"))
    unknown = bool(unknown_raw) if unknown_raw is not None else False
    score = dig(payload, parse.get("scorePath"))
    if not unknown:
        if isinstance(score, bool) or not isinstance(score, int):
            raise JudgeParseError(f"LLM 评分非法：{score!r}（应 1–5 整数或 unknown=true）")
        if score < 1 or score > 5:
            raise JudgeParseError(f"LLM 评分越界：{score}")
    hard = dig(payload, parse.get("hardViolationPath"))
    return {"score": score if not unknown else None,
            "unknown": unknown,
            "hardViolation": bool(hard) if hard is not None else False,
            "evidence": dig(payload, parse.get("evidencePath"))}


# ---------- 真实客户端（curl 子进程，密钥不进 argv） ----------

def curl_json(url, headers, body, timeout_sec, body_file=None):
    """POST JSON，返回 (http_status, 解析后的 body|None, 原始文本)。

    认证头等敏感头经 stdin 的 curl config（-K -）传入，进程参数里看不到密钥。
    请求体一律走文件（--data-binary @file）：没给 body_file 时写入安全临时文件
    （mode 0600，finally 删除）。body 与 curl config **绝不共享 stdin**——
    `-K -` 会把整个 stdin 当 config 解析，混写会让 curl 报
    "option -K: error encountered when reading a file"（真实探针踩过）。
    """
    config_lines = [f'header = "{k}: {v}"' for k, v in headers.items()]
    stdin_text = "\n".join(config_lines)
    tmp_path = None
    if body_file is not None:
        data_path = str(body_file)
    else:
        fd, tmp_path = tempfile.mkstemp(prefix="tako-judge-body-", suffix=".json")
        data_path = tmp_path
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(json.dumps(body, ensure_ascii=False))
        except BaseException:
            try:
                os.close(fd)
            except OSError:
                pass
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
            raise
    argv = ["curl", "-sS", "--max-time", str(timeout_sec),
            "-X", "POST", url, "-H", "Content-Type: application/json",
            "-w", "\n%{http_code}", "-K", "-",
            "--data-binary", "@" + data_path]
    try:
        try:
            r = subprocess.run(argv, input=stdin_text, capture_output=True, text=True,
                               timeout=timeout_sec + 10)
        except subprocess.TimeoutExpired:
            raise JudgeCallError(f"curl 超时（>{timeout_sec}s）")
        except OSError as e:
            raise JudgeCallError(f"curl 起不来：{e}")
    finally:
        if tmp_path is not None:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
    if r.returncode != 0:
        raise JudgeCallError(f"curl 退出码 {r.returncode}：{(r.stderr or '').strip()[:200]}"
                             f" body={(r.stdout or '')[:200]}")
    raw, _, status_text = (r.stdout or "").rstrip().rpartition("\n")
    try:
        http_status = int(status_text.strip())
    except ValueError:
        http_status = None
    if http_status is None or http_status >= 400:
        raise JudgeCallError(f"HTTP {http_status}：{raw.strip()[:240]}")
    raw = raw.strip()
    try:
        parsed = json.loads(raw) if raw else None
    except json.JSONDecodeError:
        parsed = None
    return http_status, parsed, raw


def _decision_headers(cfg, key):
    auth = (cfg["decision"].get("authPrefix") or "") + key
    return {"Authorization": auth}


def _llm_headers(cfg, key):
    auth = (cfg["llm"].get("authPrefix") or "") + key
    return {"Authorization": auth}


def decision_key():
    """decision 密钥只从进程环境读取。"""
    key = os.environ.get("DASHSCOPE_API_KEY", "").strip()
    if not key:
        raise JudgeConfigError("缺 DASHSCOPE_API_KEY（decision 裁判密钥，只从进程环境读）")
    return key


def llm_key():
    """DeepSeek 裁判密钥走 harness 的 TAKO_ENV 既有读取，不另起读取路径。"""
    try:
        return harness.api_key()
    except SystemExit as e:
        raise JudgeConfigError(str(e))


# ---------- 投票编排（e 计数、不重试） ----------

def run_decision_votes(cfg, state, questions, votes=3, timeout_sec=60,
                       transport=None, key=None, save=None):
    """同一完整 state 独立调 votes 次，每次同时回答 Q1–Q3。

    返回 {"perQ": {qid: {"e": int, "answers": [yes/no/unknown, ...]（长度=正常次数）},
                   "rawVotes": [...]},
          "calls": [{"ok": true, "answers", "body": 服务原始响应体, "raw": 原始文本}
                    | {"ok": false, "error"}, ...],
          "requestBody": 请求体}
    整请求失败/解析失败 → 该次三问各计 e（Q 一次请求同生共死）；单项缺/坏值只计该项 e。
    body/raw 随 calls 保存，rebuild 只读已存证据重算，不重调裁判。
    save(call_index, kind, payload) 由调用方落盘原始请求/响应。
    """
    require_verified(cfg, "decision")
    transport = transport or curl_json
    key = key if key is not None else decision_key()
    body = {"model": cfg["decision"].get("model", "decision-model-preview"),
            "state": state, "questions": questions}
    per_q = {qid: {"e": 0, "answers": [], "rawVotes": []} for qid in Q_IDS}
    calls = []
    for i in range(votes):
        try:
            _status, parsed, raw = transport(cfg["decision"]["url"],
                                             _decision_headers(cfg, key), body, timeout_sec)
            if parsed is None:
                raise JudgeParseError(f"响应不是合法 JSON：{raw[:160]}")
            answers = extract_decision_answers(parsed, cfg)
            calls.append({"ok": True, "answers": answers, "body": parsed, "raw": raw})
            for qid in Q_IDS:
                per_q[qid]["answers"].append(answers[qid])
                per_q[qid]["rawVotes"].append({"body": parsed, "raw": raw})
        except (JudgeCallError, JudgeParseError) as e:
            calls.append({"ok": False, "error": str(e)[:300]})
            for qid in Q_IDS:
                per_q[qid]["e"] += 1
        if save:
            save(i, "decision", calls[-1])
    return {"perQ": per_q, "calls": calls, "requestBody": body}


def llm_dimension_messages(cfg, case, dimension_id, round_no, user_text, answer_text,
                           a1_result, citations, read_events):
    """构造 D 裁判输入：轮次问答 + 四份文件原文 + 判断边界 + 访问/出处检查结果 + 准则。"""
    files_block = "\n\n".join(f"### {f.path}\n```\n{f.content}\n```" for f in case.files)
    cite_block = json.dumps(citations, ensure_ascii=False, indent=1)
    events_block = json.dumps(read_events, ensure_ascii=False, indent=1)
    dim = case.dimensions[dimension_id]
    rubric = RUBRIC_TEXT.format(anchors=RUBRIC_ANCHORS)
    prompt = DIMENSION_PROMPT.format(
        round_no=round_no, user_text=user_text, answer_text=answer_text or "（无答案）",
        boundary=case.judgment_boundary, criteria=dim["criteria"], rubric=rubric,
        files_block=files_block, a1_status=(a1_result or {}).get("status", "unknown"),
        citations=cite_block, read_events=events_block,
        common_hard=COMMON_HARD_RULE)
    return [{"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": prompt}]


SYSTEM_PROMPT = "你是严格的答案质量裁判。只依据给定材料判定，输出 JSON。"
COMMON_HARD_RULE = ("共同硬性准则：不得添加材料不支持的技术、现状或因果结论；"
                    "明确说“文档未说明”或作为待核实问题提出不算断言。")
RUBRIC_ANCHORS = ("5=该轮必需内容、来源和边界全部正确，表达清楚，无额外无依据结论；"
                  "4=所有必需项仍正确且完整，只有不影响事实、出处或边界的轻微表达问题；"
                  "3=有实质遗漏、含混或定位不完整，但还不是共同硬性准则违规；"
                  "2=主要事实、来源或旧依据处理错误，或多个必需项缺失；"
                  "1=共同硬性准则违规，或回答与现行记录的核心结论相反。")
RUBRIC_TEXT = "评分锚点（1–5）：" + "{anchors}"
DIMENSION_PROMPT = """请评价第 {round_no} 轮回答。输出 JSON：{{"score": 1-5 或 null, "unknown": true/false, "hardViolation": true/false, "evidence": "答案原句与依据"}}。

【本轮问题】
{user_text}

【本轮回答】
{answer_text}

【判定边界】
{boundary}

【评分准则】
{criteria}

【硬性准则（违反则 hardViolation=true，评分必须落在失败档）】
{common_hard}

{rubric}

【出处检查结果（A1/B1）】{a1_status}
{citations}

【访问证据 read_events】
{read_events}

【四份文件原文】
{files_block}
"""


def run_llm_votes(cfg, case, dimension_id, messages, votes=3, timeout_sec=60,
                  transport=None, key=None, save=None, name="llm"):
    """每个维度独立评分 votes 次；每次调用/解析异常记 e，不重试不补票。

    calls 逐项带服务原始 body/raw（{"ok": true, "verdict", "body", "raw"}），
    失败调用保存 {"ok": false, "error"}——rebuild 从已存证据重算时不再猜。
    save(call_index, name, payload)：name 由调用方给（如 "llm-D1"），落盘文件名前缀。
    """
    require_verified(cfg, "llm")
    transport = transport or curl_json
    key = key if key is not None else llm_key()
    body = {"model": cfg["llm"].get("model"),
            "messages": messages,
            "temperature": cfg["llm"].get("temperature", 0)}
    if cfg["llm"].get("responseFormat") == "json_object":
        body["response_format"] = {"type": "json_object"}
    verdicts, calls = [], []
    for i in range(votes):
        try:
            _status, parsed, raw = transport(cfg["llm"]["url"], _llm_headers(cfg, key),
                                             body, timeout_sec)
            if parsed is None:
                raise JudgeParseError(f"响应不是合法 JSON：{raw[:160]}")
            verdict = extract_llm_verdict(parsed, cfg)
            verdicts.append(verdict)
            calls.append({"ok": True, "verdict": verdict, "body": parsed, "raw": raw})
        except (JudgeCallError, JudgeParseError) as e:
            calls.append({"ok": False, "error": str(e)[:300]})
        if save:
            save(i, name, calls[-1])
    return {"verdicts": verdicts, "calls": calls, "requestBody": body}


# ---------- 校准用的合成 transport（不打真实外部） ----------

def fake_transport(responses):
    """测试/自测用：按顺序吐出响应对象，异常对象直接抛。绝不触网。"""
    queue = list(responses)

    def _t(url, headers, body, timeout_sec):
        if not queue:
            raise JudgeCallError("fake transport 耗尽")
        item = queue.pop(0)
        if isinstance(item, Exception):
            raise item
        return None, item, json.dumps(item, ensure_ascii=False)

    return _t
