#!/usr/bin/env python3
"""dialogue_hybrid_chaos_proxy.py — 历史对话混合检索场景 6 的故障注入代理（doc/84）。

在验收引擎与真实上游之间插一层：验收引擎把 TAKO_ES_URL / TAKO_DIALOGUE_EMBEDDING_BASE_URL /
TAKO_MILVUS_URI 指到本代理，代理按故障清单文件把请求放行或注入故障。引擎侧客户端
（HTTP 503/500、坏 JSON、维度不符、TCP 拒连）走到与生产同款的错误分类路径，而上游本身
完好无损——脚本结束后上游没有任何残留。

用法：
  # HTTP 透传/故障（ES 9200、embedding OpenAI 兼容接口）
  python3 dialogue_hybrid_chaos_proxy.py http --listen 127.0.0.1:PORT --upstream https://es.example:9200 \
      --fault-file /tmp/fault.txt --log /tmp/proxy.log

  # TCP 黑洞（Milvus 的 gRPC+TLS 没法在 HTTP 层伪装，直接断连接）
  python3 dialogue_hybrid_chaos_proxy.py tcp --listen 127.0.0.1:PORT --upstream host:port \
      --fault-file /tmp/fault.txt --log /tmp/proxy.log

故障清单文件（每请求重读，改文件即换故障，不用重启代理）：
  空或 "pass"   → 透传到真实上游
  "503" / "500" / "400" → 直接回该状态码 + {"chaos":true}（http 模式；400 用于
                  验证 4xx → 永久分类 es_permanent / embedding_bad_response）
  "garbage"     → 回 200，body 是坏 JSON（embedding_bad_response 分类）（http 模式）
  "dim8"        → 回 200，OpenAI embeddings 形状但向量维度 8（embedding_dimension_mismatch
                  → milvus_dimension_mismatch 分类）（http 模式）
  "sleep<N>"    → 先睡 N 秒再透传（crash 组场景 7d 用：把消费的 claim→done 窗口拉长到
                  秒级，kill -9 才能稳定打在「已领取、done 未写回」上；透传本身不受影响）
  "tcpdown"     → accept 后立刻断开（tcp 模式）
  其他          → 透传
"""
import argparse
import http.server
import json
import re
import select
import socket
import socketserver
import sys
import threading
import time
import urllib.error
import urllib.request


def read_fault(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().strip().lower()
    except OSError:
        return "pass"


class HTTPProxy(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # 静默默认 stderr 行
        pass

    def _chaos_response(self, status: int, body: bytes, ctype: str):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._handle("GET")

    def do_POST(self):
        self._handle("POST")

    def do_PUT(self):
        self._handle("PUT")

    def do_DELETE(self):
        self._handle("DELETE")

    def do_HEAD(self):
        self._handle("HEAD")

    def do_PATCH(self):
        self._handle("PATCH")

    def _handle(self, method: str):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        fault = read_fault(self.server.fault_file)
        self.server.log_line(f"{method} {self.path} fault={fault}")

        m = re.match(r"sleep(\d+)$", fault)
        if m:
            time.sleep(int(m.group(1)))
            self.server.log_line(f"  slept {m.group(1)}s, now pass")
            fault = "pass"

        if fault in ("503", "500", "400"):
            self._chaos_response(int(fault), b'{"chaos":true}', "application/json")
            return
        if fault == "garbage":
            self._chaos_response(200, b"chaos-not-json{{{", "text/plain")
            return
        if fault == "dim8":
            self._dim8_response(body)
            return

        self._forward(method, body)

    def _dim8_response(self, req_body):
        n = 1
        try:
            parsed = json.loads(req_body or b"{}")
            inp = parsed.get("input", [])
            n = len(inp) if isinstance(inp, list) else 1
        except Exception:
            n = 1
        vec = [0.01] * 8
        payload = {
            "object": "list",
            "model": "chaos-dim8",
            "data": [
                {"object": "embedding", "index": i, "embedding": vec} for i in range(n)
            ],
            "usage": {"prompt_tokens": 0, "total_tokens": 0},
        }
        self._chaos_response(200, json.dumps(payload).encode(), "application/json")

    def _forward(self, method: str, body):
        url = self.server.upstream + self.path
        headers = {}
        for k, v in self.headers.items():
            if k.lower() in ("host", "connection", "content-length", "transfer-encoding", "accept-encoding"):
                continue
            headers[k] = v
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                rbody = resp.read()
                self.send_response(resp.status)
                ctype = resp.headers.get("Content-Type", "application/json")
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(rbody)))
                self.end_headers()
                if method != "HEAD":
                    self.wfile.write(rbody)
            self.server.log_line(f"  -> upstream {resp.status} {len(rbody)}B {time.time()-t0:.2f}s")
        except urllib.error.HTTPError as e:
            rbody = e.read()
            self._chaos_response(e.code, rbody, e.headers.get("Content-Type", "application/json") if e.headers else "application/json")
            self.server.log_line(f"  -> upstream HTTPError {e.code} {len(rbody)}B")
        except Exception as e:  # 上游连接失败 → 502（引擎按瞬时分类）
            self.server.log_line(f"  -> upstream FAILED {e!r}")
            try:
                self._chaos_response(502, str(e).encode(), "text/plain")
            except Exception:
                pass


class ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, addr, handler, upstream, fault_file, log_path):
        self.upstream = upstream.rstrip("/")
        self.fault_file = fault_file
        self.log_path = log_path
        self.log_lock = threading.Lock()
        super().__init__(addr, handler)

    def log_line(self, line: str):
        with self.log_lock:
            try:
                with open(self.log_path, "a", encoding="utf-8") as f:
                    f.write(f"{time.strftime('%H:%M:%S')} {line}\n")
            except OSError:
                pass


def run_http(args):
    srv = ThreadingHTTPServer(args.listen, HTTPProxy, args.upstream, args.fault_file, args.log)
    print(f"http proxy {args.listen} -> {args.upstream} fault-file={args.fault_file}", flush=True)
    srv.serve_forever()


class TCPRelay(socketserver.BaseRequestHandler):
    def handle(self):
        fault = read_fault(self.server.fault_file)
        self.server.log_line(f"tcp conn from {self.client_address} fault={fault}")
        if fault == "tcpdown":
            try:
                self.request.close()
            except OSError:
                pass
            return
        try:
            up = socket.create_connection(self.server.upstream, timeout=15)
        except Exception as e:
            self.server.log_line(f"  -> upstream connect FAILED {e!r}")
            try:
                self.request.close()
            except OSError:
                pass
            return
        socks = [self.request, up]
        try:
            while True:
                r, _, x = select.select(socks, [], socks, 30)
                if x:
                    break
                if not r:
                    continue
                for s in r:
                    data = s.recv(65536)
                    if not data:
                        return
                    (up if s is self.request else self.request).sendall(data)
        except OSError:
            pass
        finally:
            try:
                up.close()
            except OSError:
                pass


class TCPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, addr, handler, upstream, fault_file, log_path):
        self.upstream = (upstream[0], int(upstream[1]))
        self.fault_file = fault_file
        self.log_path = log_path
        self.log_lock = threading.Lock()
        super().__init__(addr, handler)

    def log_line(self, line: str):
        with self.log_lock:
            try:
                with open(self.log_path, "a", encoding="utf-8") as f:
                    f.write(f"{time.strftime('%H:%M:%S')} {line}\n")
            except OSError:
                pass


def run_tcp(args):
    up = args.upstream.rsplit(":", 1)
    srv = TCPServer(args.listen, TCPRelay, up, args.fault_file, args.log)
    print(f"tcp proxy {args.listen} -> {args.upstream} fault-file={args.fault_file}", flush=True)
    srv.serve_forever()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["http", "tcp"])
    ap.add_argument("--listen", required=True, help="127.0.0.1:PORT")
    ap.add_argument("--upstream", required=True, help="http:// URL 或 host:port")
    ap.add_argument("--fault-file", required=True)
    ap.add_argument("--log", required=True)
    args = ap.parse_args()
    host, port = args.listen.rsplit(":", 1)
    args.listen = (host, int(port))
    (run_http if args.mode == "http" else run_tcp)(args)


if __name__ == "__main__":
    sys.exit(main())
