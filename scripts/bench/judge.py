#!/usr/bin/env python3
"""判定器：GAIA 官方 `question_scorer` 的逐条移植（数字 / 列表 / 字符串三支）。

出处：HF Space `gaia-benchmark/leaderboard` 的 `scorer.py`
      sha256 0d44c07f3046eec521697c22e3eaca8719cc81e422a8eaf32695c5f22bdac6e2
      抓取日期 2026-11-01（走 hf-mirror；huggingface.co 直连不通）
移植只去掉官方那版「数字解析失败就 print 一行」的副作用——它会污染 stdout。
判定只读世界状态，不读 agent 自述（doc/45 §6.2）。

`--selftest` 不靠记忆：把官方那份源码抓下来当**对照实现**，逐条比对本移植的结果。
"""
import argparse
import contextlib
import hashlib
import io
import json
import os
import re
import string
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
CASES = os.path.join(HERE, "cases")
OFFICIAL_URL = "https://hf-mirror.com/spaces/gaia-benchmark/leaderboard/raw/main/scorer.py"
OFFICIAL_SHA = "0d44c07f3046eec521697c22e3eaca8719cc81e422a8eaf32695c5f22bdac6e2"


def is_float(element):
    """官方 is_float：float() 能不能解（只捕 ValueError）。"""
    try:
        float(element)
        return True
    except ValueError:
        return False


def normalize_number_str(number_str):
    """官方 normalize_number_str：剔掉 $ % , 再按 float 解；解不了算 inf。"""
    for char in ["$", "%", ","]:
        number_str = number_str.replace(char, "")
    try:
        return float(number_str)
    except ValueError:
        return float("inf")


def normalize_str(input_str, remove_punct=True):
    """官方 normalize_str：去所有空白 →（可选）删 ASCII 标点 → 小写。"""
    no_spaces = re.sub(r"\s", "", input_str)
    if remove_punct:
        translator = str.maketrans("", "", string.punctuation)
        return no_spaces.lower().translate(translator)
    return no_spaces.lower()


def split_string(s, char_list=[",", ";"]):
    """官方 split_string：按 , 或 ; 切。"""
    return re.split("[%s]" % "".join(char_list), s)


def question_scorer(model_answer, ground_truth):
    """官方 question_scorer 的移植，返回 (pass, 走了哪支)。

    支名 number / list / string 只用于归类；判不过一律报 "mismatch"（见 judge）。
    """
    if model_answer is None:
        model_answer = "None"
    if is_float(ground_truth):
        return normalize_number_str(model_answer) == float(ground_truth), "number"
    if any(char in ground_truth for char in [",", ";"]):
        gt_elems = split_string(ground_truth)
        ma_elems = split_string(model_answer)
        if len(gt_elems) != len(ma_elems):
            return False, "list"
        for gt_elem, ma_elem in zip(gt_elems, ma_elems):
            if is_float(gt_elem):
                if normalize_number_str(ma_elem) != float(gt_elem):
                    return False, "list"
            elif normalize_str(ma_elem, remove_punct=False) != normalize_str(gt_elem, remove_punct=False):
                return False, "list"
        return True, "list"
    return normalize_str(model_answer) == normalize_str(ground_truth), "string"


def judge(case, answer, workspace_dir=None):
    """判定一个 case。返回 {"pass": bool, "reason": str}。

    case 里取 "answer" 当标准答案。workspace_dir 留着：将来有题要看世界状态
    （文件/页面）时，判定只读世界状态、不读 agent 自述（doc/45 §6.2）。

    reason 的取值：判过 = 走了哪支（number / list / string）；判不过 = "mismatch"。
    """
    gold = case.get("answer")
    ok, branch = question_scorer(answer, gold)
    return {"pass": ok, "reason": branch if ok else "mismatch"}


def fetch_official(path=None):
    """取官方 scorer 源码并验 sha256。取不到就抛——不许拿记忆顶替。

    下载走 curl：系统 python 的证书库认不了镜像站的证书链，curl 认得了。
    """
    path = path or os.environ.get("GAIA_SCORER_PATH") or "/tmp/gaia_scorer.py"
    if not os.path.exists(path):
        r = subprocess.run(["curl", "-fsSL", "--retry", "3", OFFICIAL_URL, "-o", path],
                           capture_output=True, text=True)
        if r.returncode != 0:
            raise SystemExit("curl 取官方 scorer 失败：%s" % (r.stderr or "").strip()[:200])
    raw = open(path, "rb").read()
    digest = hashlib.sha256(raw).hexdigest()
    if digest != OFFICIAL_SHA:
        raise SystemExit("官方 scorer 的 sha256 对不上：%s（期望 %s）" % (digest, OFFICIAL_SHA))
    return path


SELFTEST_CASES = [
    ("2", "2"), (" 2 ", "2"), ("$2", "2"), ("2.0", "2"), ("1,000", "1000"),
    ("3", "2"), ("abc", "2"), ("", "2"), ("nan", "nan"),
    ("rockhopper penguin", "Rockhopper Penguin"), ("THE SEA GULL", "The seagull"),
    ("a-b", "ab"), ("a b", "ab"), ("", "abc"),
    ("a,b", "a,b"), ("a, b", "a,b"), ("a,b,c", "a,b"), ("1,2,3", "1,2,3"),
    ("1,2,3", "1,2"), ("a; b", "a,b"), ("a-b", "a,b"), ("1,2", "1, 2"),
]


def selftest(path=None):
    """拿官方实现当对照实现，逐条比对本移植。"""
    path = fetch_official(path)
    import importlib.util
    import types
    # 官方那份 import numpy 但一个 np 都没用；塞个空模块抵掉，别为它装依赖。
    sys.modules.setdefault("numpy", types.ModuleType("numpy"))
    spec = importlib.util.spec_from_file_location("gaia_official_scorer", path)
    official = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(official)
    bad = 0
    for model_answer, ground_truth in SELFTEST_CASES:
        with contextlib.redirect_stdout(io.StringIO()):
            expect = official.question_scorer(model_answer, ground_truth)
        got, reason = question_scorer(model_answer, ground_truth)
        same = bool(expect) == bool(got)
        if not same:
            bad += 1
        print("%-22r | %-20r | 期望 %-5s | 实得 %-5s | %s" % (
            model_answer, ground_truth, bool(expect), bool(got), "一致" if same else "不一致"))
    print("对照 %d 条，不一致 %d 条" % (len(SELFTEST_CASES), bad))
    return 1 if bad else 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--case", help="题 id（题面与标准答案取自 cases/<id>.json）")
    ap.add_argument("--gold", help="直接给标准答案（不查题面文件）")
    ap.add_argument("--answer", help="被判定的那一行答案")
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        return selftest()
    if a.answer is None or not (a.case or a.gold):
        ap.error("要 --selftest，或 --case/--gold 加 --answer")
    gold = a.gold
    if gold is None:
        path = os.path.join(CASES, "%s.json" % a.case)
        gold = json.load(open(path))["answer"]
    print(json.dumps(judge({"answer": gold}, a.answer, None),
                     ensure_ascii=False, separators=(",", ":")))


if __name__ == "__main__":
    sys.exit(main() or 0)
