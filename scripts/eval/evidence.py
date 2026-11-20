#!/usr/bin/env python3
"""证据提取：runlog 工具调用配对、read_events 访问证据、citations 出处解析、主答案核对。

全部输入是已经拉回并保存的 runlog 行与帧（见 target.py），本模块不打引擎。
口径真源：doc/83-场景化Eval方案/案例-01-文档查证.md §4（证据定义）与 §7.1（提取规则）。

关键不变量：
- 工具调用与结果按 (runId, agent, toolCallId) 三元组配对——不同 agent 的调用编号会相撞，
  只按 toolCallId 配对会把子代理的证据串到主代理头上。
- read_events 只从「配对成功的结果」产生；只有调用参数没有结果内容不算访问。
- citations 只从主 Agent（agent=""）的定稿答案提取；子代理终稿、用户问题、工具输出都不算。
"""
import json
import posixpath
import re
import shlex
from urllib.parse import unquote

import casefile

READ_TOOLS = ("read_file", "read_text_file")
SEARCH_TOOLS = ("search_files",)
BASH_TOOLS = ("bash",)

_ERROR_PREFIX = "错误："
# 只有页脚/省略提示的行（方括号提示行、省略统计行）不是正文。
_HINT_LINE_RE = re.compile(r"^\s*(\[.*\]|（第\s*\d+\s*/\s*\d+\s*页）|.*(已省略|省略了|omitted).*)$")
_SEARCH_HIT_RE = re.compile(r"^(.+?):(\d+):(.*)$")
_URL_RE = re.compile(r"https?://")
_BASH_META_CHARS = ("|", "&", ";", "<", ">", "$", "`", "(", ")")

# ---------- runlog 工具行配对 ----------

def parse_payload(row):
    """runlog 扩展行的 payload 是字符串化 JSON；解析失败返回 None（该行不产生证据）。"""
    text = row.get("payload")
    if not isinstance(text, str) or not text.strip():
        return None
    try:
        obj = json.loads(text)
    except json.JSONDecodeError:
        return None
    return obj if isinstance(obj, dict) else None


def pair_key(row, payload):
    """配对键 (runId, agent, toolCallId)；缺 toolCallId 的行无法配对，返回 None。"""
    tid = (payload or {}).get("toolCallId")
    if not tid:
        return None
    return (row.get("runId", ""), row.get("agent", "") or "", tid)


def pair_tool_events(rows):
    """把 runlog 行按配对键配成 [(call_row, call_payload, result_row, result_payload)]。

    只有配对成功的调用才产生访问证据。返回 (pairs, unpaired)：
    unpaired 是配不上的行（调用没结果 / 结果没调用），调用方用于「记录缺失」判定。
    """
    calls, results, unpaired = {}, {}, []
    pairs = []
    for row in sorted(rows, key=lambda r: r.get("runlogOffset", 0)):
        if row.get("role") not in ("tool_call", "tool_result"):
            continue
        payload = parse_payload(row)
        key = pair_key(row, payload)
        if key is None:
            unpaired.append(row)
            continue
        if row["role"] == "tool_call":
            if key in calls:
                unpaired.append(row)      # 同键重复调用：编号撞车，两边都不可信
                unpaired.append(calls.pop(key))
                continue
            calls[key] = (row, payload)
        else:
            if key in results:
                unpaired.append(row)
                unpaired.append(results.pop(key))
                continue
            results[key] = (row, payload)
    for key in list(calls):
        if key in results:
            call_row, call_payload = calls.pop(key)
            res_row, res_payload = results.pop(key)
            pairs.append((call_row, call_payload, res_row, res_payload))
    unpaired.extend(r for r, _ in calls.values())
    unpaired.extend(r for r, _ in results.values())
    pairs.sort(key=lambda p: p[2].get("runlogOffset", 0))   # 按结果行 offset 排序
    return pairs, unpaired


# ---------- read_events ----------

def _is_error_output(output):
    return (output or "").lstrip().startswith(_ERROR_PREFIX)


def _substantive_lines(output):
    """去掉错误前缀与纯提示行后仍有内容的行列表；空 = 不算取得正文。"""
    if _is_error_output(output):
        return []
    lines = []
    for line in (output or "").splitlines():
        s = line.strip()
        if not s or _HINT_LINE_RE.match(s):
            continue
        lines.append(s)
    return lines


def _normalize_event_path(raw, known_paths, known_names):
    """工具结果里的路径 → workspace 相对 POSIX 路径；无法确定返回 None。

    工具自身带围栏，但自测与未来的引擎版本可能给出 ./x.md 或 trial 内绝对路径，
    这里统一归一（与 citations 同一函数族，见 normalize_cited_path）。
    """
    if not isinstance(raw, str) or not raw.strip():
        return None
    p = unquote(raw.strip()).replace("\\", "/")
    p = re.sub(r"^\./+", "", p)
    parts = []
    for seg in p.split("/"):
        if seg in ("", "."):
            continue
        if seg == "..":
            if parts:
                parts.pop()
            else:
                return None            # 越出 workspace 根：工具路径不应如此，不猜测
            continue
        parts.append(seg)
    if not parts:
        return None
    norm = "/".join(parts)
    if norm in known_paths:
        return norm
    if norm in known_names:
        return known_names[norm]
    if norm.endswith(".md"):
        return norm                     # 未知 .md 也保留，交 B1 失败
    return None


def _bash_command_form(command):
    """解析 bash 的 grep/rg 字面路径形式。

    返回 (path, kind) 或 None：
    - 有管道、重定向、命令替换、复合命令 → None（不可解析形式）。
    - 非 grep/rg、缺路径或多于一个路径参数 → None。
    只接受独立命令 + 字面路径，这是案例 §7.2 的硬性口径。
    """
    if not isinstance(command, str) or not command.strip():
        return None
    if any(ch in command for ch in _BASH_META_CHARS):
        return None
    try:
        tokens = shlex.split(command, posix=True)
    except ValueError:
        return None
    if not tokens:
        return None
    exe = posixpath.basename(tokens[0])
    if exe not in ("grep", "rg"):
        return None
    args = [t for t in tokens[1:] if not t.startswith("-")]
    if len(args) < 2:                    # 至少要 pattern + path
        return None
    if len(args) > 2:                    # 多个路径 = 非「字面路径」单文件形式，不猜
        return None
    path = args[1]
    if not path or path.startswith("-"):
        return None
    return (path, exe)


def extract_read_events(pairs, run_rounds, known_paths):
    """从配对成功的工具结果整理 read_events。

    run_rounds: {runId: round_no}；known_paths: fixture 路径集合。
    返回 (events, unparsed)：
    - events: [{round, runId, runlogOffset, agent, toolCallId, tool, path, output}]
    - unparsed: [{round, runId, runlogOffset, agent, toolCallId, tool, raw}]
      ——想做文件访问、但形式不支持解析的调用（如带管道的 bash）；它是
      「唯一证据不可解析 → A1 无效」的依据，绝不猜测它读到了什么。
    """
    known_names = {posixpath.basename(p)[:-3]: p for p in known_paths
                   if p.endswith(".md")}
    known_names.update({posixpath.basename(p): p for p in known_paths})
    events, unparsed = [], []

    def add_unparsed(call_row, call_payload, raw):
        unparsed.append({
            "round": run_rounds.get(call_row.get("runId")),
            "runId": call_row.get("runId"),
            "runlogOffset": call_row.get("runlogOffset"),
            "agent": call_row.get("agent", "") or "",
            "toolCallId": (call_payload or {}).get("toolCallId"),
            "tool": call_row.get("text", "").split("(", 1)[0].strip() or None,
            "raw": raw,
        })

    for call_row, call_payload, res_row, res_payload in pairs:
        run_id = call_row.get("runId")
        rnd = run_rounds.get(run_id)
        tool = (call_payload or {}).get("name") or ""
        args_raw = (call_payload or {}).get("args") or "{}"
        try:
            args = json.loads(args_raw) if isinstance(args_raw, str) else dict(args_raw or {})
        except json.JSONDecodeError:
            args = {}
        if not isinstance(args, dict):
            args = {}
        output = (res_payload or {}).get("output")
        output = output if isinstance(output, str) else json.dumps(output, ensure_ascii=False)
        base = {"round": rnd, "runId": run_id,
                "runlogOffset": res_row.get("runlogOffset"),
                "agent": call_row.get("agent", "") or "",
                "toolCallId": (call_payload or {}).get("toolCallId")}

        if tool in READ_TOOLS:
            lines = _substantive_lines(output)
            path = _normalize_event_path(args.get("path"), known_paths, known_names)
            if not lines or path is None:
                add_unparsed(call_row, call_payload,
                             {"form": "read", "path": args.get("path"), "outputHead": output[:120]})
                continue
            events.append(dict(base, tool=tool, path=path, output=output))
        elif tool in SEARCH_TOOLS:
            lines = _substantive_lines(output)
            if not lines:
                continue                          # 无命中 / 只有省略提示：不产生记录
            hit = False
            for line in lines:
                m = _SEARCH_HIT_RE.match(line)
                if not m:
                    continue                      # 命中统计行等，跳过
                path = _normalize_event_path(m.group(1), known_paths, known_names)
                if path is None or not m.group(3).strip():
                    continue
                hit = True
                events.append(dict(base, tool=tool, path=path, output=line))
            if not hit:
                continue
        elif tool in BASH_TOOLS:
            form = _bash_command_form(args.get("command"))
            if form is None:
                if _substantive_lines(output) or args.get("command"):
                    add_unparsed(call_row, call_payload,
                                 {"form": "bash", "command": args.get("command"),
                                  "outputHead": output[:120]})
                continue
            raw_path, _exe = form
            lines = _substantive_lines(output)
            path = _normalize_event_path(raw_path, known_paths, known_names)
            if not lines or path is None:
                add_unparsed(call_row, call_payload,
                             {"form": "bash", "command": args.get("command"),
                              "outputHead": output[:120]})
                continue
            events.append(dict(base, tool=tool, path=path, output=output))
        # 其他工具（TaskCreate 等）只计数，不产生访问证据。
    return events, unparsed


# ---------- citations ----------

# 文件名 token：先匹配已知 fixture 路径/文件名（含省略 .md 的已知名），再匹配未知 xxx.md。
_CJK = r"\u4e00-\u9fff"
_FILE_SPAN_RE = re.compile(
    r"(?P<span>/?(?:[A-Za-z0-9_.\-一-鿿]+/)*[A-Za-z0-9_.\-一-鿿]+(?:\.md)?(?:#[^\s，。；、（）()\[\]<>]*)?(?::\d+)?)")
_MD_LINK_RE = re.compile(r"\[[^\]]*\]\((?P<target><?[^)\s<>]+>?)\)")
_SECTION_RES = (
    re.compile(r"§\s*(\d+)"),
    re.compile(r"第\s*(\d+)\s*节"),
    re.compile(r"##\s+(\d+)\s*(?:\.|(?=\s|$))"),
)
_LINEREF_RE = re.compile(r"(?:#L(\d+))|(?::(\d+)$)")


def _fixture_name_maps(known_paths):
    names = {}
    for p in known_paths:
        base = posixpath.basename(p)
        names[base] = p
        if base.endswith(".md"):
            names[base[:-3]] = p
    return names


def normalize_cited_path(raw_path, known_paths, abs_prefixes=()):
    """答案/链接里的路径 → (path, outOfBounds)。

    - 去 Markdown 包裹与尖括号，URL 解码一次，POSIX 化。
    - 绝对路径：属于本 trial workspace（前缀匹配 abs_prefixes）→ 转相对；否则 outOfBounds。
    - 相对路径：消除 ./ 与 .、.. 段；越出根的可解析路径保留原样并标 outOfBounds。
    - 已知 fixture 省略 .md 的名字补扩展名；空/不可判定返回 (None, False)，由调用方标 parseError。
    """
    if not isinstance(raw_path, str):
        return None, False
    p = unquote(raw_path.strip().strip("<>").strip())
    p = p.replace("\\", "/")
    p = re.sub(r"^\./+", "", p)
    if not p or p in (".", ".."):
        return None, False
    if p.startswith("/"):
        for prefix in abs_prefixes:
            prefix = prefix.rstrip("/") + "/"
            if p.startswith(prefix):
                rel = p[len(prefix):]
                return normalize_cited_path(rel, known_paths)
        return p, True                      # 可解析但越出本 trial workspace
    parts = []
    out = False
    for seg in p.split("/"):
        if seg in ("", "."):
            continue
        if seg == "..":
            if parts:
                parts.pop()
            else:
                out = True                  # 越界：保留原文语义，标记交 B1 失败
                parts.append(seg)
            continue
        parts.append(seg)
    norm = "/".join(parts)
    if not norm:
        return None, False
    names = _fixture_name_maps(known_paths)
    if norm in names:
        return names[norm], out
    if norm.endswith(".md"):
        return norm, out                    # 未知 .md 保留，交 B1 失败
    return None, False                      # 不是可识别的路径形式


def _resolve_fragment(fragment, fixture):
    """fragment → (section, line, parseError)。锚点来自 fixture 的 `## N. 标题`。"""
    if fragment is None or fragment == "":
        return None, None, None
    frag = unquote(fragment).strip()
    m = re.fullmatch(r"L(\d+)", frag, re.I)
    if m:
        return None, int(m.group(1)), None
    if fixture is not None:
        anchors = {f"{num}-{title}": num
                   for num, title in casefile.fixture_sections(fixture).items()}
        if frag in anchors:
            return anchors[frag], None, None
    return None, None, "unsupported-fragment"


def extract_citations(answer_text, rnd, run_id, files, abs_prefixes=()):
    """从一轮主 Agent 定稿答案提取出处。

    files: casefile.Fixture 列表。返回 [{round, runId, raw, path, section, line,
    parseError, outOfBounds}]：
    - path 为 workspace 相对 POSIX 路径；outOfBounds 的可解析路径保留并交 B1 失败。
    - fragment 不支持/路径含义不明 → parseError，相关 A1/B1 无效（不伪装成「没写小节」）。
    - 同一行内，小节/行号引用挂到同行最近一个文件名；裸 §2 不凭空捏造文件。
    """
    known_paths = [f.path for f in files]
    by_path = {f.path: f for f in files}
    citations = []
    if not answer_text:
        return citations

    def add(raw, path_raw, fragment, section_text, line_text, out):
        path, oob = (normalize_cited_path(path_raw, known_paths, abs_prefixes)
                     if path_raw is not None else (None, out))
        if path is None:
            citations.append({"round": rnd, "runId": run_id, "raw": raw, "path": None,
                              "section": None, "line": None,
                              "parseError": "unclear-path", "outOfBounds": False})
            return
        fixture = by_path.get(path)
        sec, line, perr = _resolve_fragment(fragment, fixture)
        if section_text is not None:
            sec = int(section_text)
        if line_text is not None:
            line = int(line_text)
        citations.append({"round": rnd, "runId": run_id, "raw": raw, "path": path,
                          "section": sec, "line": line, "parseError": perr,
                          "outOfBounds": bool(oob)})

    for line_text in answer_text.splitlines():
        tokens = []
        occupied = [False] * len(line_text)

        def occupy(m):
            for i in range(m.start(), m.end()):
                occupied[i] = True

        for m in _MD_LINK_RE.finditer(line_text):
            occupy(m)
            target = m.group("target").strip("<>")
            path_part, _, fragment = target.partition("#")
            tokens.append((m.start(), m.end(), m.group(0), path_part, fragment or None,
                           None, None))
        for m in _FILE_SPAN_RE.finditer(line_text):
            if any(occupied[m.start():m.end()]):
                continue
            span = m.group("span")
            base = span
            fragment = None
            line_no = None
            if "#" in span:
                base, _, fragment = span.partition("#")
            lm = re.search(r":(\d+)$", base)
            if lm:
                line_no = lm.group(1)
                base = base[:lm.start()]
            is_known = base in _fixture_name_maps(known_paths) or base.endswith(".md")
            if not is_known:
                continue
            occupy(m)
            tokens.append((m.start(), m.end(), span, base, fragment, None, line_no))
        for sre in _SECTION_RES:
            for m in sre.finditer(line_text):
                if any(occupied[m.start():m.end()]):
                    continue
                occupy(m)
                tokens.append((m.start(), m.end(), m.group(0), None, None, m.group(1), None))
        tokens.sort(key=lambda t: t[0])

        last_file = None
        for start, end, raw, path_part, fragment, section_text, line_no in tokens:
            if path_part is not None:
                add(raw, path_part, fragment, None, line_no, False)
                last_file = citations[-1]
            elif section_text is not None or line_no is not None:
                if last_file is None:
                    continue                  # 裸小节引用，无同行文件：不捏造出处
                if section_text is not None:
                    last_file["section"] = int(section_text)
                if line_no is not None:
                    last_file["line"] = int(line_no)
                if last_file["raw"] and raw not in last_file["raw"]:
                    last_file["raw"] += " " + raw
    return citations


# ---------- 主答案核对 ----------

def main_answer_row(rows, run_id):
    """该 run 下主 Agent（agent=""）定稿答案行。

    返回 (row, problem)：row 唯一时 problem=None；零行 'missing'、多行 'ambiguous'。
    """
    cands = [r for r in rows
             if r.get("runId") == run_id and r.get("role") == "assistant"
             and (r.get("agent", "") or "") == ""]
    if len(cands) == 1:
        return cands[0], None
    return None, "missing" if not cands else "ambiguous"


def root_delta_text(frames, run_id):
    """frames 中该 run 全部 kind=delta、agent="" 的 text 按帧序拼接。"""
    deltas = [f for f in frames
              if f.get("runId", run_id) == run_id
              and f.get("kind") == "delta" and (f.get("agent", "") or "") == ""]
    deltas.sort(key=lambda f: f.get("eventOffset", 0))
    return "".join(f.get("text") or "" for f in deltas)


def compare_answer(row, frames, run_id):
    """runlog 主答案行 text 与 frames root delta 的字节比较。

    返回 {ok, reason, runlogChars, framesChars}：零/多行或字节不一致 → ok=False，
    由调用方按取证失败（invalid）处理，绝不二选一蒙一个。
    """
    ans_row, problem = main_answer_row([row] if row else [], run_id)
    if problem:
        return {"ok": False, "reason": "answer-" + problem,
                "runlogChars": 0, "framesChars": 0}
    runlog_text = ans_row.get("text") or ""
    frames_text = root_delta_text(frames, run_id)
    if runlog_text != frames_text:
        return {"ok": False, "reason": "answer-bytes-mismatch",
                "runlogChars": len(runlog_text), "framesChars": len(frames_text)}
    return {"ok": True, "reason": None,
            "runlogChars": len(runlog_text), "framesChars": len(frames_text)}


# ---------- A3 输入：显式联网扫描 ----------

def network_violations(pairs):
    """轨迹中的显式联网证据：web_search/web_fetch 工具，或 bash 命令含 curl/wget/外部 URL。

    返回 [{round, runId, tool, detail}]；空列表 = 未发现。MCP 非空不在此项判定
    （那是运行条件，trial 直接 invalid，见 grader.mcp_run_condition）。
    """
    out = []
    for call_row, call_payload, _res_row, _res_payload in pairs:
        tool = (call_payload or {}).get("name") or ""
        args_raw = (call_payload or {}).get("args") or "{}"
        try:
            args = json.loads(args_raw) if isinstance(args_raw, str) else dict(args_raw or {})
        except json.JSONDecodeError:
            args = {}
        if not isinstance(args, dict):
            args = {}
        if tool in ("web_search", "web_fetch"):
            out.append({"round": None, "runId": call_row.get("runId"),
                        "tool": tool, "detail": args_raw[:200]})
        elif tool in BASH_TOOLS:
            cmd = args.get("command") or ""
            hit = None
            for pat in (r"\bcurl\b", r"\bwget\b", _URL_RE):
                m = re.search(pat, cmd)
                if m:
                    hit = m.group(0)
                    break
            if hit:
                out.append({"round": None, "runId": call_row.get("runId"),
                            "tool": tool, "detail": "bash:" + cmd[:200]})
    return out
