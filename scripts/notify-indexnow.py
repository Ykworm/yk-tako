#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""发布后通知索引器（IndexNow）。

用法（CI 里）：
    INDEXNOW_KEY=<key> GITHUB_TOKEN=<token> GITHUB_SHA=<sha> python3 scripts/notify-indexnow.py

为什么需要：
    Bing 不会因为站点更新就立刻重抓，要等它例行巡检——几天到几周。IndexNow 是
    「这几个 URL 我改了，你现在来看」的约定。参与方：Bing、Yandex、Seznam、Naver、
    Yep、Amazon、Internet Archive。**Google 不参与**——Google 那边只能靠 sitemap 与抓取。

只通知这次真改过的页：改动文件从 GitHub API 取该 commit 的清单。
    IndexNow 官方不建议反复提交没变的 URL，发太勤会 429。

返回码含义：200 = 对方收到了（不等于已收录）；202 = key 还在校验；403 = key 无效；
422 = URL 的 host 与 key 不匹配；429 = 发太频繁。
"""
import json
import os
import sys
import urllib.error
import urllib.request

ENDPOINT = "https://api.indexnow.org/indexnow"
REPO = os.environ.get("REPO", "Ykworm/yk-tako")
HOST = os.environ.get("SITE_HOST", "tako.docs.yktea.fun")


def changed_files(sha, token):
    """这次发布动了哪些文件（仓库相对路径）。"""
    if not sha:
        return []
    url = f"https://api.github.com/repos/{REPO}/commits/{sha}"
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "yk-tako-indexnow",
            **({"Authorization": f"Bearer {token}"} if token else {}),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            payload = json.load(response)
    except (urllib.error.URLError, json.JSONDecodeError, OSError) as exc:
        print(f"取改动清单失败：{exc}")
        return []
    return [item.get("filename", "") for item in payload.get("files", [])]


def file_to_url(name):
    """改动的文件 → 受影响的页面 URL；返回 None 表示与页面无关。"""
    if not name.startswith("doc/"):
        # 脚本、workflow 改了 → 整站可能都变了，但先只报首页，够 Bing 重新发现。
        return "/"
    rest = name[len("doc/") :]
    if rest == "index.html":
        return "/"
    if rest == "README.md":
        return "/html/index.html"
    if rest.startswith("html/") and rest.endswith(".html"):
        return "/" + rest
    if rest.startswith("assets/") and rest.endswith(".html"):
        return "/" + rest
    if rest.endswith(".md"):
        # md 改了 → 对应的网页版变了
        return "/html/" + rest[:-3] + ".html"
    if rest in ("robots.txt", "sitemap.xml", "style.css") or rest.startswith("html/style"):
        return "/"
    return None


def main():
    key = os.environ.get("INDEXNOW_KEY", "").strip()
    if not key:
        print("未配置 INDEXNOW_KEY，跳过 IndexNow 通知（不影响发布）")
        return 0

    sha = os.environ.get("GITHUB_SHA", "").strip()
    token = os.environ.get("GITHUB_TOKEN", "").strip()
    urls = {"/"}
    for name in changed_files(sha, token):
        url = file_to_url(name)
        if url:
            urls.add(url)

    body = json.dumps(
        {"host": HOST, "key": key, "urlList": sorted(urls)}, ensure_ascii=False
    ).encode("utf-8")
    request = urllib.request.Request(
        f"{ENDPOINT}?key={key}&host={HOST}",
        data=body,
        headers={
            "Content-Type": "application/json; charset=utf-8",
            "User-Agent": "yk-tako-indexnow",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            print(f"IndexNow 已通知 {len(urls)} 个 URL，状态 {response.status}")
            return 0
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "ignore")[:300]
        print(f"IndexNow 返回 {exc.code}：{detail}")
        return 1
    except (urllib.error.URLError, OSError) as exc:
        print(f"IndexNow 通知失败：{exc}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
