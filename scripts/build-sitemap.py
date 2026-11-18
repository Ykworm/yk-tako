#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 sitemap.xml（站点地图）。

用法：
    python3 scripts/build-sitemap.py [输出路径]      # 默认 doc/sitemap.xml

为什么在构建时生成、不提交进仓库：
    lastmod 要跟着「这次发布」走。提交进仓库的话，本地生成时的 HEAD 日期和 CI 构建时的
    HEAD 日期不同，机器验收的 --check 会一直报差异；构建时生成就没有这个问题。

lastmod 统一取 HEAD 提交日期：站点是整棵 doc/ 一起发布的，页面级时间戳没有意义，
换来的是「本次发布时间」这个对重抓优先级更实在的信号。

覆盖范围：doc/ 下所有 .html——
    doc/index.html            → /
    doc/html/xxx.html         → /html/xxx.html
    doc/assets/xxx.html       → /assets/xxx.html
"""
import os
import subprocess
import sys
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOC = os.path.join(ROOT, "doc")
HOST = "https://tako.docs.yktea.fun"

# 不进 sitemap 的目录。sitemap 是「我希望被收录的页面清单」，把下面这些塞进去
# 等于告诉搜索引擎「这些也值得收录」：
#   archive/ 被整代取代的旧线与旧引擎 —— 明确作废，是纯负资产；
#   log/     交接与施工台账 —— 对外读者没有价值（里面的少数文章靠内链与外链被发现，
#            照样能被引用，只是不由 sitemap 主动推荐）。
# 排除后仍会照常发布，只是不再主动推荐收录。
EXCLUDE_PATH_PARTS = ("/archive/", "/log/")


def site_url(repo_rel):
    """仓库相对路径 → 站点 URL（站点根 = doc/）。"""
    rel = repo_rel[len("doc/") :]
    if rel == "index.html":
        return HOST + "/"
    return f"{HOST}/{rel}"


def git_head_date():
    """HEAD 提交日期（ISO）。取不到就退回今天。"""
    try:
        out = subprocess.check_output(
            ["git", "-C", ROOT, "log", "-1", "--format=%cI"], text=True
        ).strip()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        out = ""
    return out or datetime.now(timezone.utc).strftime("%Y-%m-%d")


def collect_urls():
    urls = []
    skipped = 0
    for dirpath, dirnames, filenames in os.walk(DOC):
        dirnames[:] = sorted(d for d in dirnames if d != ".git")
        for name in sorted(filenames):
            if not name.endswith(".html"):
                continue
            rel = os.path.relpath(os.path.join(dirpath, name), ROOT).replace(os.sep, "/")
            # doc/ 根下的 .html 只有 index.html 是页面；其余是站点根的辅助文件
            # （如 Google 站点验证文件 google*.html），不是页面，不进 sitemap。
            if rel.count("/") == 1 and rel != "doc/index.html":
                skipped += 1
                continue
            url = site_url(rel)
            if any(part in url for part in EXCLUDE_PATH_PARTS):
                skipped += 1
                continue
            urls.append(url)
    # 首页排最前，其余按字典序——输出稳定，diff 干净
    return sorted(urls, key=lambda u: (u != HOST + "/", u)), skipped


def build_xml(urls, lastmod):
    lines = [
        '<?xml version="1.0" encoding="UTF-8"?>',
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">',
    ]
    for url in urls:
        lines += [
            "  <url>",
            f"    <loc>{url}</loc>",
            f"    <lastmod>{lastmod}</lastmod>",
            "  </url>",
        ]
    lines.append("</urlset>")
    return "\n".join(lines) + "\n"


def build_index_xml(sitemap_url, lastmod):
    """站点地图索引（sitemap index）。

    为什么要有这个：GSC 对「抓取失败过」的站点地图不会立刻重试，状态会一直卡在
    「无法抓取」。索引文件是一个**从未提交过的新地址**——提交它等于排一次全新的
    抓取，而抓取动作发生在站点稳定时（不在 GitHub Pages 换产物的窗口里）。
    同时它也是规范做法：以后要按章节拆成多个 sitemap，这里就是汇合点。
    """
    return (
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
        "  <sitemap>\n"
        f"    <loc>{sitemap_url}</loc>\n"
        f"    <lastmod>{lastmod}</lastmod>\n"
        "  </sitemap>\n"
        "</sitemapindex>\n"
    )


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else os.path.join(DOC, "sitemap.xml")
    urls, skipped = collect_urls()
    lastmod = git_head_date()
    xml = build_xml(urls, lastmod)
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w", encoding="utf-8") as f:
        f.write(xml)

    index_path = os.path.join(os.path.dirname(os.path.abspath(out)), "sitemap-index.xml")
    sitemap_url = f"{HOST}/{os.path.basename(out)}"
    with open(index_path, "w", encoding="utf-8") as f:
        f.write(build_index_xml(sitemap_url, lastmod))

    print(
        f"sitemap：{len(urls)} 条 URL → {os.path.relpath(out, ROOT)}"
        + (f"（排除 archive/log {skipped} 条）" if skipped else "")
    )
    print(f"索引入口：{sitemap_url} → {os.path.relpath(index_path, ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
