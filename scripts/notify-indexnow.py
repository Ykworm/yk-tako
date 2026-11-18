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
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

ENDPOINT = "https://api.indexnow.org/indexnow"
REPO = os.environ.get("REPO", "Ykworm/yk-tako")
HOST = os.environ.get("SITE_HOST", "tako.docs.yktea.fun")


def http_get(url, headers=None):
    """取一个 URL 的正文。

    为什么要 curl 兜底：本机 python3 的证书链在某些域名上找不到本地签发者，
    urllib 会直接抛 SSLCertVerificationError；curl 用系统 CA，没这个问题。
    """
    headers = headers or {}
    try:
        request = urllib.request.Request(url, headers={"User-Agent": "yk-tako-indexnow", **headers})
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.read().decode("utf-8")
    except (urllib.error.URLError, OSError):
        cmd = ["curl", "-s", "--max-time", "30", "-H", "User-Agent: yk-tako-indexnow"]
        for name, value in headers.items():
            cmd += ["-H", f"{name}: {value}"]
        cmd.append(url)
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        return proc.stdout


def sitemap_urls():
    """站点根 sitemap.xml 里的全部 URL。

    新站第一次交给索引器时用这个：sitemap 是「我有多少页」的清单，
    IndexNow 是「现在来看」的通知——两者合起来等于一次性把站点交给它。
    """
    body = http_get(f"https://{HOST}/sitemap.xml")
    return re.findall(r"<loc>([^<]+)</loc>", body)


def changed_files(sha, token):
    """这次发布动了哪些文件（仓库相对路径）。"""
    if not sha:
        return []
    url = f"https://api.github.com/repos/{REPO}/commits/{sha}"
    headers = {"Accept": "application/vnd.github+json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    body = http_get(url, headers)
    try:
        payload = json.loads(body)
    except json.JSONDecodeError as exc:
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

    all_urls = "--all" in sys.argv[1:]
    if all_urls:
        paths = set()
        try:
            for loc in sitemap_urls():
                paths.add(loc)
        except (urllib.error.URLError, OSError) as exc:
            print(f"读 sitemap 失败，改用改动清单：{exc}")
            all_urls = False

    sha = os.environ.get("GITHUB_SHA", "").strip()
    token = os.environ.get("GITHUB_TOKEN", "").strip()

    if all_urls:
        urls = sorted(paths)
        if not urls:
            # 取不到 sitemap 就会交一份空清单——IndexNow 仍回 200，
            # 于是「通知成功但什么都没通知」。这里必须显式失败。
            print("sitemap 取不到 URL 列表，未发送通知（不当作成功）")
            return 1
    else:
        paths = {"/"}
        for name in changed_files(sha, token):
            path = file_to_url(name)
            if path:
                paths.add(path)
        # IndexNow 只认绝对 URL：传路径（"/html/x.html"）它回 400 InvalidRequestParameters。
        urls = sorted(f"https://{HOST}{path}" for path in paths)

    body = json.dumps(
        {"host": HOST, "key": key, "urlList": urls}, ensure_ascii=False
    ).encode("utf-8")
    status, detail = post(ENDPOINT, body, key, HOST)
    if status.startswith("2"):
        print(f"IndexNow 已通知 {len(urls)} 个 URL，状态 {status}")
        return 0
    print(f"IndexNow 返回 {status}：{detail}")
    return 1


def post(endpoint, body, key, host):
    """POST 通知。返回 (状态码, 响应正文)。

    为什么 curl 优先：某些网络环境下 urllib 的 TLS 行为会被服务端直接掐断
    （RemoteDisconnected），或者本机证书链不全（SSLCertVerificationError）；
    curl 用系统 CA 且行为稳定。urllib 留作 curl 不可用时的兜底。
    """
    url = f"{endpoint}?key={key}&host={host}"
    headers = {
        "Content-Type": "application/json; charset=utf-8",
        "User-Agent": "yk-tako-indexnow",
    }
    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as fh:
        fh.write(body)
        payload = fh.name
    try:
        proc = subprocess.run(
            [
                "curl", "-s", "-o", "-", "-w", "\n%{http_code}", "-X", "POST",
                # -H 要完整头行（"Name: value"），只给值会发出畸形头，服务端直接回 400。
                "-H", f"Content-Type: {headers['Content-Type']}",
                "-H", f"User-Agent: {headers['User-Agent']}",
                "--data", "@" + payload, url,
            ],
            capture_output=True,
            text=True,
            timeout=60,
        )
        parts = proc.stdout.rsplit("\n", 1)
        if len(parts) == 2 and parts[1].strip().isdigit():
            return parts[1].strip(), parts[0][:300]
    except (OSError, subprocess.SubprocessError):
        pass
    finally:
        os.unlink(payload)

    request = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return str(response.status), response.read().decode("utf-8", "ignore")[:300]
    except urllib.error.HTTPError as exc:
        return str(exc.code), exc.read().decode("utf-8", "ignore")[:300]
    except (urllib.error.URLError, OSError) as exc:
        return "000", str(exc)


if __name__ == "__main__":
    sys.exit(main())
