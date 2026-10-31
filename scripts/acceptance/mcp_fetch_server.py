#!/usr/bin/env python3
"""最小 MCP stdio server：一个工具 fetch_probe(url) —— 起 curl 打指定 URL，回报 HTTP 码。

用途（doc/52 B3 验收 5）：租户 MCP 子进程跑在 `sandboxWrap` 的围栏里，这个夹具证明
围栏的「出站网络开」对它同样生效——工具跑得通、拿到 200，就说明 MCP 与 bash 共享同一份 profile。

协议：JSON-RPC 2.0，一行一条（MCP stdio transport）。只实现握手 + tools/list + tools/call，
不依赖任何第三方包（沙箱里没有 pip 装依赖这回事）。
"""
import json
import os
import subprocess
import sys


def reply(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def handle(req):
    method = req.get("method")
    rid = req.get("id")
    if method == "initialize":
        params = req.get("params") or {}
        return {"jsonrpc": "2.0", "id": rid, "result": {
            "protocolVersion": params.get("protocolVersion", "2024-11-05"),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "tako-fetch-probe", "version": "1.0.0"},
        }}
    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": rid, "result": {"tools": [{
            "name": "fetch_probe",
            "description": "起 curl 打一个 URL，返回 HTTP 状态码。",
            "inputSchema": {
                "type": "object",
                "properties": {"url": {"type": "string", "description": "要访问的 URL"}},
                "required": ["url"],
            },
        }]}}
    if method == "tools/call":
        params = req.get("params") or {}
        url = (params.get("arguments") or {}).get("url", "")
        # 正文落到脚本自己所在的目录（= 租户存储根，围栏内可写）；写 /dev/null 会被
        # 围栏拒（file-write 只给围栏根），-o /dev/null 会以 rc=56 假失败。
        sink = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".fetch_probe.out")
        try:
            out = subprocess.run(
                ["curl", "-s", "-m", "10", "-o", sink,
                 "-w", "HTTP %{http_code}", url],
                capture_output=True, text=True, timeout=15)
            text = f"{out.stdout.strip()}\nstderr: {out.stderr.strip()[:200]}"
        except Exception as exc:  # noqa: BLE001 —— 夹具把任何失败原样报回
            text = f"probe failed: {exc}"
        finally:
            try:
                os.remove(sink)
            except OSError:
                pass
        return {"jsonrpc": "2.0", "id": rid, "result": {
            "content": [{"type": "text", "text": text}], "isError": False}}
    if method == "ping":
        return {"jsonrpc": "2.0", "id": rid, "result": {}}
    if rid is None:  # 通知（notifications/initialized 等）：不回
        return None
    return {"jsonrpc": "2.0", "id": rid,
            "error": {"code": -32601, "message": f"method not found: {method}"}}


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(req, list):  # 批量请求
            out = [r for r in (handle(x) for x in req) if r is not None]
            if out:
                reply(out)
            continue
        resp = handle(req)
        if resp is not None:
            reply(resp)


if __name__ == "__main__":
    main()
