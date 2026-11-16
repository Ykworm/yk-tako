#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 doc/ 树转成 doc/html/ 静态站（pandoc gfm → html5 + 链接重写）。

用法：
    python3 scripts/build-doc-html.py [--check]

产物：
    doc/html/**.html   每个 .md 一页（doc/README.md → doc/html/index.html）
    doc/html/style.css 站点样式

链接重写规则（逐条试，命中即止）：
    1. 外链（http/https/mailto）与纯锚点 #… → 原样保留。
    2. 相对 md 的标准解析命中 doc/ 里的 .md → 同结构 .html（doc/README.md → doc/html/index.html）。
    3. 命中 doc/ 里的其他已入库文件（图片等）→ 相对路径照旧。
    4. 命中仓库根的已入库文件（代码、脚本、AGENTS.md）→ GitHub blob 链接。
    5. 标准解析没命中，再按「仓库根路径」解释一次——修 doc 里写漏 ../ 的代码链接。
    6. 仓库里根本没有的（tmp/ 施工包、ref-agent 参考实现、本机绝对路径）→ 去掉链接保留文字。

只有「已被 git 跟踪」的文件才算存在：tmp/ 是施工材料、不入库，给它做链接等于给读者一条死链。
"""

import argparse
import html as htmllib
import os
import re
import subprocess
import sys
from urllib.parse import quote

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOC = os.path.join(ROOT, "doc")
OUT = os.path.join(DOC, "html")
REPO_SLUG = "Ykworm/yk-tako"
BLOB = f"https://github.com/{REPO_SLUG}/blob/main/"
STYLE = os.path.join(OUT, "style.css")
PANDOC = "pandoc"

ASSET_PAGES = [
    ("tako-architecture.html", "架构总览"),
    ("tako-run-lifecycle.html", "run 生命周期"),
    ("tako-orchestration.html", "长任务编排"),
    ("tako-data-model.html", "数据与存储"),
]

A_EL = re.compile(r"<a\b([^>]*)>(.*?)</a>", re.S)
IMG_EL = re.compile(r"<img\b([^>]*?)\bsrc=\"([^\"]*)\"([^>]*)>", re.S)
TABLE_EL = re.compile(r"<table>.*?</table>", re.S)
H1_EL = re.compile(r"<h1[^>]*>(.*?)</h1>", re.S)


# ---------------------------------------------------------------- 基础工具


def git_tracked():
    """已被 git 跟踪的文件（仓库相对路径，POSIX 分隔）。

    必须关掉 core.quotePath：默认 git 会把中文路径转成八进制转义加引号，
    那样 set 里全是 \\350\\277\\233 这种串，跟磁盘上的真实路径对不上。
    """
    out = subprocess.check_output(
        ["git", "-C", ROOT, "-c", "core.quotePath=false", "ls-files", "-z"],
        text=True,
        errors="ignore",
    )
    return {line.strip() for line in out.split("\0") if line.strip()}


def rel_from(target, start_dir):
    """仓库相对路径 → 相对 start_dir 的 POSIX 路径。"""
    return os.path.relpath(target, start_dir).replace(os.sep, "/")


def blob_url(repo_rel, anchor=""):
    """仓库内文件 → GitHub blob 链接（中文路径按段百分号编码）。"""
    quoted = "/".join(quote(part, safe="") for part in repo_rel.split("/"))
    return f"{BLOB}{quoted}{anchor}"


def tree_url(repo_rel):
    """仓库内目录 → GitHub tree 链接。"""
    quoted = "/".join(quote(part, safe="") for part in repo_rel.split("/"))
    return f"https://github.com/{REPO_SLUG}/tree/main/{quoted}"


def strip_tags(text):
    return re.sub(r"<[^>]+>", "", text)


# ---------------------------------------------------------------- 链接重写


TRACKED = set()
TRACKED_DIRS = set()
DEAD = []


def resolve(src_rel, out_rel, href):
    """决定一个 href 怎么改。

    返回 ("keep", href) 原样 / ("rel", 新相对路径) / ("blob", 绝对 URL) / ("dead", None)。
    """
    if not href or href.startswith("#"):
        return ("keep", href)

    scheme = re.match(r"^([a-zA-Z][a-zA-Z0-9+.\-]*):", href)
    if scheme:
        name = scheme.group(1).lower()
        if name in ("http", "https", "mailto"):
            return ("keep", href)
        if name == "file":
            # file:///本机绝对路径 → 能落回仓库的换成 blob，其余算死链
            rest = href[len("file://") :]
            path, _, anchor = rest.partition("#")
            if path.startswith(ROOT + os.sep):
                repo_rel = os.path.relpath(path, ROOT)
                if repo_rel in TRACKED:
                    return ("blob", blob_url(repo_rel, f"#{anchor}" if anchor else ""))
            DEAD.append((src_rel, href))
            return ("dead", None)
        return ("keep", href)

    base, _, anchor = href.partition("#")
    if not base:
        return ("keep", href)
    suffix = f"#{anchor}" if anchor else ""

    # 先按标准 markdown 解析（相对源文件所在目录），再按仓库根解释一次
    candidates = [
        os.path.normpath(os.path.join(os.path.dirname(src_rel), base)),
        os.path.normpath(base),
    ]
    for cand in candidates:
        if cand.startswith("..") or cand not in TRACKED:
            continue
        if cand.startswith("doc/") and cand.endswith(".md"):
            html_rel = "doc/html/" + cand[len("doc/") : -len(".md")] + ".html"
            if cand == "doc/README.md":
                html_rel = "doc/html/index.html"
            return ("rel", rel_from(html_rel, os.path.dirname(out_rel)) + suffix)
        if cand.startswith("doc/"):
            return ("rel", rel_from(cand, os.path.dirname(out_rel)) + suffix)
        if cand in TRACKED_DIRS:
            return ("blob", tree_url(cand) + suffix)
        return ("blob", blob_url(cand, suffix))
    DEAD.append((src_rel, href))
    return ("dead", None)


def rewrite_fragment(fragment, src_rel, out_rel):
    """把 fragment 里的 a/img 链接按规则改写；死链去掉链接保留文字。"""
    out_dir = os.path.dirname(out_rel)

    def anchor_repl(match):
        attrs, inner = match.group(1), match.group(2)
        href_m = re.search(r'\bhref="([^"]*)"', attrs)
        if not href_m:
            return match.group(0)
        href = htmllib.unescape(href_m.group(1))
        kind, value = resolve(src_rel, out_rel, href)
        if kind == "keep":
            return match.group(0)
        if kind == "dead":
            # 空锚点（pandoc 给标题生成的那种）不算死链，原样留着
            return inner if inner.strip() else match.group(0)
        new_attrs = attrs.replace(href_m.group(0), f'href="{htmllib.escape(value, quote=True)}"')
        return f"<a{new_attrs}>{inner}</a>"

    def img_repl(match):
        attrs, href, tail = match.group(1), match.group(2), match.group(3)
        alt_m = re.search(r'\balt="([^"]*)"', attrs + tail)
        alt = alt_m.group(1) if alt_m else ""
        kind, value = resolve(src_rel, out_rel, href)
        if kind == "keep":
            return match.group(0)
        if kind == "dead":
            return (
                f'<span class="missing">[图片缺失：{htmllib.escape(href)}]</span>'
            )
        return f'<img{attrs}src="{htmllib.escape(value, quote=True)}"{tail}>'

    fragment = A_EL.sub(anchor_repl, fragment)
    fragment = IMG_EL.sub(img_repl, fragment)
    # 宽表格横向滚动包一层
    fragment = TABLE_EL.sub(lambda m: f'<div class="table-wrap">{m.group(0)}</div>', fragment)
    return fragment


# ---------------------------------------------------------------- 页面外壳


def page_html(fragment, src_rel, out_rel, title):
    out_dir = os.path.dirname(out_rel)
    css = rel_from("doc/html/style.css", out_dir)
    index = rel_from("doc/html/index.html", out_dir)
    assets = rel_from("doc/assets", out_dir) + "/"
    nav = "".join(
        f'<a href="{assets}{name}">{label}</a>' for name, label in ASSET_PAGES
    )
    source = blob_url(src_rel)
    return f"""<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>{htmllib.escape(title)} · tako 文档</title>
<link rel="stylesheet" href="{css}" />
</head>
<body>
<header class="topbar">
  <a class="brand" href="{index}">tako 文档</a>
  <nav class="links">{nav}</nav>
  <a class="repo" href="https://github.com/{REPO_SLUG}" rel="noopener">GitHub</a>
</header>
<main class="doc">
{fragment}
</main>
<footer class="foot">
  <p>本页由 <code>scripts/build-doc-html.py</code> 从 <a href="{source}"><code>{htmllib.escape(src_rel)}</code></a> 自动生成。内容以仓库里的 md 为准——要改，改 md 再重跑脚本。</p>
</footer>
</body>
</html>
"""


# ---------------------------------------------------------------- 样式

STYLE_CSS = """/* tako 文档站样式（scripts/build-doc-html.py 生成） */
:root {
  --bg: #fbf8f2;
  --panel: #ffffff;
  --ink: #35271c;
  --muted: #77685a;
  --line: #e5dac8;
  --accent: #bb7738;
  --accent-deep: #543a29;
  --accent-soft: #f0dec2;
  --code-bg: #f5eee2;
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--ink);
  font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Hiragino Sans GB",
    "Microsoft YaHei", "Noto Sans CJK SC", "Segoe UI", Roboto, sans-serif;
  font-size: 16px;
  line-height: 1.75;
}
a { color: var(--accent-deep); text-decoration-color: var(--accent); text-underline-offset: 3px; }
a:hover { color: var(--accent); }

/* 顶栏 */
.topbar {
  position: sticky; top: 0; z-index: 10;
  display: flex; align-items: center; gap: 18px; flex-wrap: wrap;
  padding: 10px 22px;
  background: rgba(255, 255, 255, .94);
  backdrop-filter: saturate(140%) blur(8px);
  border-bottom: 1px solid var(--line);
}
.topbar .brand { font-weight: 700; color: var(--accent-deep); text-decoration: none; letter-spacing: .3px; }
.topbar .links { display: flex; gap: 14px; flex-wrap: wrap; font-size: 14px; }
.topbar .links a { color: var(--muted); text-decoration: none; }
.topbar .links a:hover { color: var(--accent); }
.topbar .repo { margin-left: auto; font-size: 14px; color: var(--muted); text-decoration: none; }

/* 正文 */
.doc {
  max-width: 980px;
  margin: 0 auto;
  padding: 34px 22px 20px;
  background: var(--panel);
  border-left: 1px solid var(--line);
  border-right: 1px solid var(--line);
  min-height: 60vh;
}
h1, h2, h3, h4 { line-height: 1.35; color: var(--accent-deep); }
h1 { font-size: 1.85em; margin: .2em 0 .7em; padding-bottom: .35em; border-bottom: 2px solid var(--accent-soft); }
h2 { font-size: 1.42em; margin: 1.9em 0 .6em; padding-bottom: .25em; border-bottom: 1px solid var(--line); }
h3 { font-size: 1.16em; margin: 1.5em 0 .45em; }
h4 { font-size: 1.03em; margin: 1.2em 0 .4em; }
p { margin: .75em 0; }

/* 文档头的引用块 */
blockquote {
  margin: 1em 0 1.6em;
  padding: 14px 18px;
  background: #fdf6ea;
  border-left: 4px solid var(--accent);
  border-radius: 0 6px 6px 0;
  color: #5a4a3b;
}
blockquote p { margin: .35em 0; }
blockquote > p:first-child { margin-top: 0; }
blockquote > p:last-child { margin-bottom: 0; }

/* 代码 */
code {
  font-family: "SF Mono", ui-monospace, Menlo, Consolas, "Liberation Mono", monospace;
  font-size: .88em;
  background: var(--code-bg);
  padding: .12em .38em;
  border-radius: 4px;
  word-break: break-word;
}
pre {
  background: var(--code-bg);
  border: 1px solid var(--line);
  border-radius: 8px;
  padding: 14px 16px;
  overflow-x: auto;
  line-height: 1.6;
}
pre code { background: none; padding: 0; font-size: .86em; }

/* 表格 */
.table-wrap { overflow-x: auto; margin: 1em 0; }
table { border-collapse: collapse; width: 100%; font-size: .92em; }
th, td { border: 1px solid var(--line); padding: 8px 11px; text-align: left; vertical-align: top; }
th { background: var(--accent-soft); color: var(--accent-deep); font-weight: 700; }
tbody tr:nth-child(even) { background: #fcf8f1; }

/* 列表与图片 */
ul, ol { padding-left: 1.5em; }
li { margin: .28em 0; }
li > ul, li > ol { margin: .2em 0; }
hr { border: 0; border-top: 1px solid var(--line); margin: 2em 0; }
img { max-width: 100%; height: auto; }
.missing { color: var(--muted); font-size: .9em; font-style: italic; }

/* 页脚 */
.foot {
  max-width: 980px; margin: 0 auto; padding: 18px 22px 40px;
  color: var(--muted); font-size: 13px;
}
.foot p { margin: 0; }

@media (max-width: 760px) {
  body { font-size: 15px; }
  .doc { border-left: 0; border-right: 0; padding: 22px 16px 12px; }
  .topbar { gap: 10px; padding: 9px 16px; }
  .topbar .repo { margin-left: 0; }
  .foot { padding: 14px 16px 30px; }
}
"""


# ---------------------------------------------------------------- 主流程


def collect_sources():
    """doc/ 下所有「已被 git 跟踪」的 .md（跳过 doc/html/ 与生成物）。

    只转已入库的文档：doc/interview/ 等目录被 .gitignore 挡着（简历、面试材料），
    转进去等于把私有材料发到公开仓。
    """
    sources = []
    for dirpath, dirnames, filenames in os.walk(DOC):
        dirnames[:] = sorted(d for d in dirnames if os.path.join(dirpath, d) != OUT)
        for name in sorted(filenames):
            if not name.endswith(".md"):
                continue
            src = os.path.join(dirpath, name)
            rel = os.path.relpath(src, ROOT).replace(os.sep, "/")
            if rel not in TRACKED:
                continue
            if rel == "doc/README.md":
                out_rel = "doc/html/index.html"
            else:
                out_rel = "doc/html/" + rel[len("doc/") : -len(".md")] + ".html"
            sources.append((src, rel, out_rel))
    return sources


def convert(src, out_rel, src_rel):
    proc = subprocess.run(
        [PANDOC, "-f", "gfm", "-t", "html5", "--wrap=none", src],
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"pandoc 失败：{src_rel}\n{proc.stderr.strip()}")
    fragment = proc.stdout
    fragment = rewrite_fragment(fragment, src_rel, out_rel)
    h1 = H1_EL.search(fragment)
    title = htmllib.unescape(strip_tags(h1.group(1))).strip() if h1 else os.path.basename(out_rel)
    return page_html(fragment, src_rel, out_rel, title)


def audit_links():
    """查生成站里的本地链接有没有断。

    <code> 块先剔掉——文档正文里常有「md 里写 <img src="logo.png"> 会坏图」这类举例，
    那不是真链接，不能算进死链。
    """
    broken = []
    total = 0
    link_re = re.compile(r'\b(?:href|src)="([^"]*)"')
    for dirpath, _, filenames in os.walk(OUT):
        for name in sorted(filenames):
            if not name.endswith(".html"):
                continue
            path = os.path.join(dirpath, name)
            with open(path, encoding="utf-8") as f:
                body = f.read()
            body = re.sub(r"<code>.*?</code>", "", body, flags=re.S)
            for raw in link_re.findall(body):
                if raw.startswith(("http://", "https://", "mailto:", "#", "data:")):
                    continue
                total += 1
                target = raw.split("#")[0]
                if not target:
                    continue
                resolved = os.path.normpath(os.path.join(dirpath, target))
                if not os.path.exists(resolved):
                    broken.append(f"{os.path.relpath(path, ROOT)} -> {raw}")
    return total, broken


def md_to_html(target):
    """doc 相对路径（可带 ../ 前缀）→ html 树里的对应路径。

    "../53-模块地图.md"  → "../html/53-模块地图.html"
    "60-多会话并发.md"    → "html/60-多会话并发.html"（说明页的 page-data 里是这种，
                          前缀由页面脚本自己补 "../"）
    "../README.md"       → "../html/index.html"
    """
    match = re.match(r"^((?:\.\./)*)(.*)$", target)
    prefix, rest = match.group(1), match.group(2)
    if not rest.endswith(".md"):
        return target
    if rest == "README.md":
        return f"{prefix}html/index.html"
    return f"{prefix}html/{rest[:-3]}.html"


def rewrite_asset_pages(write=True):
    """把四张说明页里指向 doc 的 .md 链接改成指向 doc/html 的 .html 链接。

    两处来源：
      1. 页面里的静态 <a href="...md">；
      2. page-data JSON 里的 "path"（节点挂的现行文档）与 "href"（章节链接）。
    JSON 不整体重新序列化——原文是混合缩进的，整体 dump 会把整个文件改成另一种格式。
    """
    changed = []
    for name, _ in ASSET_PAGES:
        path = os.path.join(DOC, "assets", name)
        if not os.path.exists(path):
            continue
        with open(path, encoding="utf-8") as f:
            text = f.read()

        def sub_attr(match):
            head, target, tail = match.group(1), match.group(2), match.group(3)
            new = md_to_html(target)
            return head + new + tail if new != target else match.group(0)

        new_text = re.sub(r'(<a\b[^>]*?\bhref=")([^"]*\.md)(")', sub_attr, text)
        new_text = re.sub(r'("(?:path|href)":")([^"]*\.md)(")', sub_attr, new_text)

        if new_text != text:
            changed.append(f"doc/assets/{name}")
            if write:
                with open(path, "w", encoding="utf-8") as f:
                    f.write(new_text)
    return changed


def main():
    parser = argparse.ArgumentParser(description="生成 doc/html 静态站")
    parser.add_argument("--check", action="store_true", help="只比对不写盘：有差异或断链就非零退出")
    parser.add_argument(
        "--audit-only",
        action="store_true",
        help="只查已生成站的链接（不需要 pandoc，可在 CI 里跑）",
    )
    args = parser.parse_args()

    if args.audit_only:
        total, broken = audit_links()
        print(f"生成站本地链接：{total} 条，断链 {len(broken)} 条")
        for item in broken[:20]:
            print(f"  断链：{item}")
        return 1 if broken else 0

    global TRACKED, TRACKED_DIRS
    TRACKED = git_tracked()
    TRACKED_DIRS = set()
    for path in TRACKED:
        parts = path.split("/")
        for i in range(1, len(parts)):
            TRACKED_DIRS.add("/".join(parts[:i]))

    sources = collect_sources()
    if not sources:
        print("没找到 doc/ 下已入库的 .md")
        return 1

    os.makedirs(OUT, exist_ok=True)
    expected = {"doc/html/style.css"}
    changed = []
    for src, src_rel, out_rel in sources:
        expected.add(out_rel)
        html = convert(src, out_rel, src_rel)
        dst = os.path.join(ROOT, out_rel)
        old = None
        if os.path.exists(dst):
            with open(dst, encoding="utf-8") as f:
                old = f.read()
        if old != html:
            changed.append(out_rel)
            if not args.check:
                os.makedirs(os.path.dirname(dst), exist_ok=True)
                with open(dst, "w", encoding="utf-8") as f:
                    f.write(html)

    style_dst = os.path.join(ROOT, "doc/html/style.css")
    style_old = None
    if os.path.exists(style_dst):
        with open(style_dst, encoding="utf-8") as f:
            style_old = f.read()
    if style_old != STYLE_CSS:
        changed.append("doc/html/style.css")
        if not args.check:
            with open(style_dst, "w", encoding="utf-8") as f:
                f.write(STYLE_CSS)

    # 清掉上一次生成留下的孤儿（源文档被删、被 .gitignore 挡住、路径改名）
    stale = []
    for dirpath, _, filenames in os.walk(OUT):
        for name in filenames:
            path = os.path.join(dirpath, name)
            rel = os.path.relpath(path, ROOT).replace(os.sep, "/")
            if rel not in expected:
                stale.append(rel)
    for rel in stale:
        changed.append(rel)
        if not args.check:
            os.remove(os.path.join(ROOT, rel))
    for dirpath, dirnames, _ in os.walk(OUT, topdown=False):
        for name in dirnames:
            path = os.path.join(dirpath, name)
            if not os.listdir(path):
                os.rmdir(path)

    # 说明页里指向 doc 的 .md 链接也要改道到 html 树
    changed.extend(rewrite_asset_pages(write=not args.check))

    print(f"doc/html：{len(sources)} 页，{'需要更新' if changed else '已是最新'}（{len(changed)} 个文件有差异）")
    if DEAD:
        print(f"死链 {len(DEAD)} 条（已去掉链接保留文字）：")
        seen = {}
        for src_rel, href in DEAD:
            seen.setdefault(href, []).append(src_rel)
        for href, files in sorted(seen.items(), key=lambda kv: -len(kv[1])):
            print(f"  {href}  ×{len(files)}（例：{files[0]}）")

    total, broken = audit_links()
    print(f"生成站本地链接：{total} 条，断链 {len(broken)} 条")
    for item in broken[:20]:
        print(f"  断链：{item}")

    if args.check and (changed or broken):
        for name in changed[:20]:
            print(f"  待更新：{name}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
