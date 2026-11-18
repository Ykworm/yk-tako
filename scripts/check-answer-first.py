#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""检查文档页的「首段是不是答案」。

为什么要有这个检查：
    搜索引擎与 AI 摘录的是**能独立成立的段落**。我们文档的惯例是开头放一段引用块
    （性质／最后核对／口径／代码落点……）——那是写给「知道文档纪律的人」看的元信息，
    不是答案。放在第一段，等于告诉 AI「这篇开头没内容」。

    规则：每页正文的第一段应当是一句自足的话（这篇讲什么、结论是什么），
    元信息放它后面或保持引用块形式都行。

用法：
    python3 scripts/check-answer-first.py            # 列出待补的页（人类可读）
    python3 scripts/check-answer-first.py --strict   # 有待补的页就退出码非零（可进 CI）

输出按目录分组，每条给出：页面、当前首段的前 60 字、建议补什么。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HTML_ROOT = os.path.join(ROOT, "doc", "html")

# 首段出现这些词开头 → 它是元信息块，不是答案
META_PREFIX = (
    "性质：",
    "最后核对",
    "口径：",
    "状态：",
    "日期：",
    "代码落点：",
    "与总纲关系：",
    "落点：",
    "自然：",
)

MAIN = re.compile(r"<main\b.*?</main>", re.S)
H1 = re.compile(r"<h1[^>]*>.*?</h1>", re.S)
P = re.compile(r"<p>(.*?)</p>", re.S)


def strip_tags(text):
    text = re.sub(r"<code>(.*?)</code>", r"\1", text, flags=re.S)
    return re.sub(r"<[^>]+>", "", text).strip()


def first_paragraph(path):
    """页面正文里 h1 之后的第一段（去掉标签）。"""
    with open(path, encoding="utf-8") as f:
        html = f.read()
    main = MAIN.search(html)
    body = main.group(0) if main else html
    h1 = H1.search(body)
    after = body[h1.end():] if h1 else body
    for match in P.finditer(after):
        text = strip_tags(match.group(1))
        if text:
            return text
    return ""


def collect():
    needs = []
    ok = 0
    for dirpath, dirnames, filenames in os.walk(HTML_ROOT):
        dirnames[:] = sorted(d for d in dirnames if d != "archive")
        for name in sorted(filenames):
            if not name.endswith(".html"):
                continue
            path = os.path.join(dirpath, name)
            rel = os.path.relpath(path, ROOT).replace(os.sep, "/")
            text = first_paragraph(path)
            if not text:
                continue
            if text.startswith(META_PREFIX):
                needs.append((rel, text))
            else:
                ok += 1
    return needs, ok


def main():
    strict = "--strict" in sys.argv
    needs, ok = collect()
    total = len(needs) + ok
    print(f"答案先行检查：{total} 页里 {len(needs)} 页首段是元信息，{ok} 页已达标")
    if not needs:
        return 0
    print()
    current = ""
    for rel, text in needs:
        group = rel.rsplit("/", 1)[0] if "/" in rel else "（站点根）"
        if group != current:
            current = group
            print(f"── {group} ──")
        print(f"  {rel.split('/')[-1]}")
        print(f"    现首段：{text[:60]}")
        print("    改法：把第一段换成一句自足的话——这篇讲什么、结论是什么；元信息挪到它后面。")
    print()
    if strict:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
