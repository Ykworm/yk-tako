#!/usr/bin/env python3
"""从官方源派生 GAIA 切片，并核对它是不是「这轮跑的那 25 题」。

入库的只有配方（scripts/bench/cases/gaia_l1_25.json）：源仓、源文件、revision、
筛选、排序、题数、slice_hash。题面 / 标准答案 / task_id **不入库**——GAIA 条款要求
不外发数据集（dataset card「not reshare … outside of a gated or private repository」；
leaderboard content.py「Please do not repost the public dev set」）。明文留在本机 gitignore
目录里照常可读、可调优；hash 是**复现装置**不是保密装置。

用法：
  python3 scripts/bench/slice.py --verify          # 派生 → 重算 hash → 与入库配方比对，不一致非零退出
  python3 scripts/bench/slice.py --emit <path>     # 把明文切片写到本地路径（不入库）

下载走 hf-mirror（huggingface.co 直连不通）；gated 数据集要 Authorization: Bearer $HF_TOKEN。
读 parquet 要 pyarrow：本机没有时用 uv 起一个临时环境重跑本脚本，不污染全局。
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
RECIPE = os.path.join(HERE, "cases", "gaia_l1_25.json")
CACHE_DIR = os.path.join(REPO, "tmp", "56-可比性对照")
TOKEN_FILES = [os.path.expanduser("~/.cache/huggingface/token")]


def reexec_with_pyarrow():
    """本机没 pyarrow 就借 uv 起个临时环境重跑自己（一次性依赖，不装进系统）。"""
    if os.environ.get("BENCH_SLICE_REEXEC"):
        raise SystemExit("装不上 pyarrow：pip install pyarrow 或用 uv 跑")
    if not shutil.which("uv"):
        raise SystemExit("本机没 pyarrow 也没有 uv：pip install pyarrow")
    os.environ["BENCH_SLICE_REEXEC"] = "1"
    os.execvp("uv", ["uv", "run", "--with", "pyarrow", "--python", sys.executable,
                      "python3", os.path.abspath(__file__)] + sys.argv[1:])


def load_recipe():
    return json.load(open(RECIPE))


def token():
    """gated 数据集的令牌：环境变量优先，其次 HF CLI 的本地令牌文件。令牌不入库。"""
    t = os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN")
    if t:
        return t.strip()
    for p in TOKEN_FILES:
        if os.path.exists(p):
            t = open(p).read().strip()
            if t:
                return t
    raise SystemExit("没有 HF_TOKEN：gated 数据集要令牌（环境变量 HF_TOKEN，或先跑 hf auth login）")


def curl(url, dest, headers=()):
    """用 curl 取文件。系统 python 的证书库认不了镜像站的证书链，curl 认得了。"""
    cmd = ["curl", "-fsSL", "--retry", "3"]
    for h in headers:
        cmd += ["-H", h]
    cmd += [url, "-o", dest]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit("curl 失败 %d：%s" % (r.returncode, (r.stderr or "").strip()[:300]))


def fetch_parquet(recipe, dest):
    """按配方里的 revision 下源文件，校验 sha256 与字节数。已有缓存就复用。"""
    url = "%s/datasets/%s/resolve/%s/%s" % (
        recipe.get("mirror", "https://hf-mirror.com").rstrip("/"),
        recipe["source_repo"], recipe["source_revision_main"], recipe["source_file"])
    want = recipe.get("source_file_sha256")
    if os.path.exists(dest):
        raw = open(dest, "rb").read()
        if not want or hashlib.sha256(raw).hexdigest() == want:
            return dest
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    curl(url, tmp, ["Authorization: Bearer " + token()])
    raw = open(tmp, "rb").read()
    got = hashlib.sha256(raw).hexdigest()
    if want and got != want:
        raise SystemExit("源文件 sha256 对不上：%s（配方期望 %s）" % (got, want))
    if recipe.get("source_file_bytes") and len(raw) != recipe["source_file_bytes"]:
        raise SystemExit("源文件字节数对不上：%d（配方期望 %d）" % (len(raw), recipe["source_file_bytes"]))
    os.replace(tmp, dest)
    return dest


def derive(rows, recipe):
    """按配方筛选 + 排序 + 截断，产出 cases 列表。"""
    flt = recipe["filter"]
    pool = []
    for r in rows:
        if flt.get("level") is not None and str(r.get("Level", "")).strip() != str(flt["level"]):
            continue
        if flt.get("file_name_empty") and (r.get("file_name") or "").strip():
            continue
        pool.append(r)
    pool.sort(key=lambda r: r["task_id"])
    size = recipe["slice_size"]
    cases = [{"task_id": r["task_id"], "question": r["Question"], "answer": r["Final answer"]}
             for r in pool[:size]]
    return cases, len(pool)


def slice_hash(cases):
    """按配方里的 hash 口径重算（见 cases/gaia_l1_25.json 的 slice_hash_recipe）。"""
    payload = json.dumps(cases, ensure_ascii=False, sort_keys=True).encode("utf-8")
    return "sha256:" + hashlib.sha256(payload).hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--verify", action="store_true", help="派生并与入库配方核对")
    ap.add_argument("--emit", help="把明文切片写到本地路径")
    ap.add_argument("--parquet", help="已下载的 parquet 路径（不给就用缓存/现下）")
    a = ap.parse_args()
    if not (a.verify or a.emit):
        ap.error("要 --verify 或 --emit")

    recipe = load_recipe()
    try:
        import pyarrow.parquet as pq
    except ImportError:
        reexec_with_pyarrow()

    parquet = a.parquet or os.path.join(
        CACHE_DIR, "gaia_official_metadata.level1.parquet")
    fetch_parquet(recipe, parquet)

    rows = pq.read_table(parquet).to_pylist()
    cases, pool = derive(rows, recipe)
    got = slice_hash(cases)

    if a.emit:
        out = {
            "slice_id": recipe["slice_id"],
            "source_repo": recipe["source_repo"],
            "source_file": recipe["source_file"],
            "source_revision_main": recipe["source_revision_main"],
            "filter": recipe["filter"],
            "sort": recipe["sort"],
            "pool_level1_text_only": pool,
            "slice_size": len(cases),
            "slice_hash": got,
            "cases": cases,
        }
        os.makedirs(os.path.dirname(os.path.abspath(a.emit)), exist_ok=True)
        json.dump(out, open(a.emit, "w"), ensure_ascii=False, indent=2)
        print("明文切片已写 %s（题数 %d，池 %d）" % (a.emit, len(cases), pool))

    if a.verify:
        problems = []
        if pool != recipe.get("pool_level1_text_only"):
            problems.append("题池 %d ≠ 配方 %s" % (pool, recipe.get("pool_level1_text_only")))
        if len(cases) != recipe["slice_size"]:
            problems.append("切片题数 %d ≠ 配方 %d" % (len(cases), recipe["slice_size"]))
        if got != recipe["slice_hash"]:
            problems.append("slice_hash %s ≠ 配方 %s" % (got, recipe["slice_hash"]))
        print("题池 %d；切片 %d 题；slice_hash %s" % (pool, len(cases), got))
        if problems:
            for p in problems:
                print("❌ " + p)
            return 1
        print("✅ 与入库配方一致（题数 / 池大小 / slice_hash 三项全对）")
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
