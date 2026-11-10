#!/usr/bin/env python3
"""契约断言器：读 expected.json 逐条打引擎，比对期望字节，红打 ❌、退出码非零。

这是**机器真源那一侧的裁判**。doc/19-v6/19v6-04-验收-curl.md 是同一份内容的人读版；
两份同批改，check-expected.sh 守两边的用例号集合一致。

用法：
  python3 scripts/acceptance/assert_contract.py                 # 全跑
  python3 scripts/acceptance/assert_contract.py --only 3-       # 只跑 id 带 3- 的
  python3 scripts/acceptance/assert_contract.py --tier core     # 只跑不需要模型的
  python3 scripts/acceptance/assert_contract.py --print         # 只打印真实字节（对齐用）

退出码：0 全绿；1 有失败；2 环境不对（读不到 .env / 清单不存在）。
"""
import argparse
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import harness  # noqa: E402  同目录底座

HERE = Path(__file__).resolve().parent
MANIFEST = HERE / "expected.json"


def subst(value, vars):
    """把 {var} 换成运行时变量。"""
    if isinstance(value, str):
        if value.startswith("{") and value.endswith("}") and value[1:-1] in vars:
            return vars[value[1:-1]]
        return value
    if isinstance(value, dict):
        return {k: subst(v, vars) for k, v in value.items()}
    if isinstance(value, list):
        return [subst(v, vars) for v in value]
    return value


def resolve_path(path, vars):
    """把 path 里的 {var} 换掉（逐段替换，非整串命中也认）。"""
    out = path
    for k, v in vars.items():
        out = out.replace("{" + k + "}", str(v))
    return out


def check_step(expect, http, body, vars, tally, label):
    """比对一条期望。返回 True = 这条全过。"""
    bad = []

    # 期望里也可以写 {var}（比如按 id 找数组里的那一项）。
    expect = subst(expect, vars)

    def want(cond, why):
        if not cond:
            bad.append(why)

    if "http" in expect:
        want(http == expect["http"], f"HTTP {http} ≠ {expect['http']}")
    if not isinstance(body, dict):
        bad.append(f"响应不是 JSON 信封：{body!r}")
    else:
        if "code" in expect:
            want(body.get("code") == expect["code"], f"code {body.get('code')} ≠ {expect['code']}")
        if "message" in expect:
            want(body.get("message") == expect["message"], f"message {body.get('message')!r} ≠ {expect['message']!r}")
        data = body.get("data")
        if "data" in expect:
            want(harness.match(expect["data"], data), f"data {json.dumps(data, ensure_ascii=False)[:160]} ≠ {json.dumps(expect['data'], ensure_ascii=False)[:160]}")
        if "data_keys" in expect:
            got = list(data.keys()) if isinstance(data, dict) else None
            want(got == expect["data_keys"], f"data 键序 {got} ≠ {expect['data_keys']}")
        if "frame_kinds_contains" in expect:
            frames = data.get("frames", []) if isinstance(data, dict) else []
            kinds = {f.get("kind") for f in frames if isinstance(f, dict)}
            missing = [k for k in expect["frame_kinds_contains"] if k not in kinds]
            want(not missing, f"帧里缺 kind {missing}（实有 {sorted(k for k in kinds if k)}）")
        if "rows_len_equals" in expect:
            w = expect["rows_len_equals"]
            if isinstance(w, str) and w.startswith("{") and w.endswith("}"):
                w = vars.get(w[1:-1])
            rows = data.get("rows", []) if isinstance(data, dict) else []
            n = len(w) if isinstance(w, list) else w
            want(len(rows) == n, f"runlog 行数 {len(rows)} ≠ {n}")
        if "data_items_some" in expect:
            # 数组里**至少有一项**满足条件（如「新建的项目 isDefault=false」）。
            # 为什么不用 expect.data：那是逐位比对、长度必须相同——而列表长度会变。
            spec = expect["data_items_some"]
            items = data.get(spec["path"], []) if isinstance(data, dict) else []
            hit = any(isinstance(it, dict) and harness.match(spec["match"], it) for it in items)
            want(hit, f"数组 {spec['path']} 里没有满足 {spec['match']} 的项")
        if "data_contains" in expect:
            # 只断「含某几段」，不逐字比全文——给**逐请求变**的字节用
            # （例：bash 落盘提示行里的 run id 每次都不同，全文比不了）。
            spec = expect["data_contains"]
            hay = data.get(spec["path"], "") if isinstance(data, dict) else ""
            missing = [s for s in spec["contains"] if s not in hay]
            want(not missing, f"data.{spec['path']} 里缺 {missing}（实有 {str(hay)[:160]}）")

    if bad:
        tally.no(label, "；".join(bad))
        return False
    tally.ok(label)
    return True


def signup(vars, key, with_model):
    """建账号 + 租户（可选配模型），把 token / tenant 塞进 vars。"""
    import time as _t
    email = f"acc_{_t.time_ns()}@tako.test"
    code, body = harness.call("POST", "/api/auth/signup", {"email": email, "password": "changeme123"})
    if code != 201 or not isinstance(body, dict) or body.get("code") != 0:
        raise RuntimeError(f"signup 失败：HTTP {code} {body!r}")
    d = body["data"]
    if with_model:
        hcode, hbody = harness.call(
            "PUT", f"/api/tenants/{d['tenant']['id']}/runtime-config",
            {"modelRef": harness.MODEL, "mcpServers": {},
             "secrets": {"DEEPSEEK_API_KEY": key}, "sandbox": {"network": False}},
            d["accessToken"])
        if hcode != 200:
            raise RuntimeError(f"配租户模型失败：HTTP {hcode} {hbody!r}")
    vars["tok"] = d["accessToken"]
    vars["ten"] = d["tenant"]["id"]
    vars["email"] = email
    return body


def run_case(case, key, tally, only_print, seed):
    """跑一个用例。seed = 引导阶段建好的账号（token/tenant），用例内 signup 会覆盖它。"""
    vars = dict(seed)
    print(f"── {case['id']}（{case['case']}）{case.get('desc', '')}")
    for i, step in enumerate(case.get("steps", [])):
        label = f"{case['id']}#{i + 1}"
        op = step.get("op", "http")

        if op == "signup":
            if only_print:
                body = signup(vars, key, not step.get("no_model"))
                print(f"  {label} signup → {json.dumps(body, ensure_ascii=False)[:200]}")
                continue
            try:
                body = signup(vars, key, not step.get("no_model"))
            except Exception as e:
                tally.no(label, str(e))
                return False
            if not check_step(step.get("expect", {}), 201, body, vars, tally, label):
                return False
            continue

        if op == "invite_viewer":
            # 造一个「有身份但权限不够」的 viewer 并登录（doc/52 E2 的角色闸负路径）。
            # 401 代替不了 403：前者是「没登录」，后者是「登录了、但角色不够」——两道闸，
            # 只有真拿 viewer token 打一次才证明第二道在。
            # 邮箱带时间戳：users 表邮箱全局唯一，固定串在第二轮会撞 ErrEmailTaken。
            email = f"viewer_{time.time_ns()}@tako.test"
            code, body = harness.call("POST", f"/api/tenants/{vars['ten']}/invitations",
                                      {"email": email, "role": "viewer"}, token=vars.get("tok"))
            if code != 201 or not isinstance(body, dict) or body.get("code") != 0:
                tally.no(label, f"发邀请失败：HTTP {code} {body!r}")
                return False
            invite_token = body["data"]["inviteToken"]
            code, body = harness.call("POST", "/api/auth/register-by-invite",
                                      {"token": invite_token, "email": email, "password": "changeme123"})
            if code != 201:
                tally.no(label, f"注册 viewer 失败：HTTP {code} {body!r}")
                return False
            code, body = harness.call("POST", "/api/auth/login",
                                      {"email": email, "password": "changeme123"})
            if code != 200 or not isinstance(body, dict) or body.get("code") != 0:
                tally.no(label, f"登录 viewer 失败：HTTP {code} {body!r}")
                return False
            # 新用户同时有自己的个人工作区，登录的 active tenant 不一定是被邀请的那个；
            # 不切过去的话，后面拿他的名字解析到的是**另一个租户**（实为 404 116001，不是 403）。
            access = body["data"]["accessToken"]
            code, body = harness.call("POST", "/api/auth/switch-tenant",
                                      {"tenantId": vars["ten"],
                                       "refreshToken": body["data"]["refreshToken"]}, token=access)
            if code != 200 or not isinstance(body, dict) or body.get("code") != 0:
                tally.no(label, f"切到邀请方租户失败：HTTP {code} {body!r}")
                return False
            vars["viewerTok"] = body["data"]["accessToken"]
            tally.ok(f"{label} viewer 就位")
            continue

        if op == "wait_terminal":
            rid = resolve_path(step["run"], vars)
            deadline = time.time() + int(step.get("timeout", 90))
            while time.time() < deadline:
                _, body = harness.call("GET", f"/api/runs/{rid}/frames?eventOffset=0", token=vars.get("tok"))
                frames = (body or {}).get("data", {}).get("frames", []) if isinstance(body, dict) else []
                if any(f.get("kind") == "terminal" for f in frames if isinstance(f, dict)):
                    tally.ok(f"{label} 等到 terminal")
                    break
                time.sleep(1)
            else:
                tally.no(f"{label} 等 terminal", f"{step.get('timeout', 90)}s 内没等到")
                return False
            continue

        if op == "wait_settled":
            # 等**会话落定**：terminal 帧比 DB 翻终态早一拍，
            # 只等帧会在「跑中读 session」这条上撞出假失败（liveRunId 还非空）。
            sid = resolve_path(step["session"], vars)
            deadline = time.time() + int(step.get("timeout", 30))
            while time.time() < deadline:
                _, body = harness.call("GET", f"/api/sessions/{sid}", token=vars.get("tok"))
                d = body.get("data", {}) if isinstance(body, dict) else {}
                if d.get("lastRunStatus") in ("complete", "error", "stopped") and d.get("liveRunId") == "":
                    tally.ok(f"{label} 等到定稿")
                    break
                time.sleep(0.5)
            else:
                tally.no(f"{label} 等定稿", f"{step.get('timeout', 30)}s 内会话没落定")
                return False
            continue

        if op == "seed_ws":
            # 往项目目录里造文件/目录/符号链接：产出文件在**文件系统**上，SQL 造不出来。
            # 项目目录布局 = <workspace根>/tenants/<tid>/workspaces/<wid>/（scope.go）。
            wid = resolve_path(step["workspace"], vars)
            root = Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "workspaces" / wid
            try:
                for rel, content in (step.get("files") or {}).items():
                    f = root / rel
                    f.parent.mkdir(parents=True, exist_ok=True)
                    f.write_text(content)
                for rel in (step.get("dirs") or []):
                    (root / rel).mkdir(parents=True, exist_ok=True)
                for rel in (step.get("symlinks") or []):
                    link = root / rel
                    if link.is_symlink() or link.exists():
                        link.unlink()
                    os.symlink("/etc", link)
            except OSError as e:
                tally.no(label, "造文件失败：%s" % e)
                return False
            tally.ok("%s 造文件 %s" % (label, wid))
            continue

        if op == "seed_skill":
            # 往本租户的技能根里造一个技能目录（doc/52）：<WS_ROOT>/tenants/<tid>/skills/<name>/SKILL.md。
            # 造完要跟一步 POST /api/skills/rescan 才进表——盘是正文，表是目录。
            name = resolve_path(step["name"], vars)
            root = Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "skills" / name
            try:
                (root / "SKILL.md").parent.mkdir(parents=True, exist_ok=True)
                (root / "SKILL.md").write_text(step.get("skill_md", ""))
            except OSError as e:
                tally.no(label, "造技能失败：%s" % e)
                return False
            tally.ok("%s 造技能 %s" % (label, name))
            continue

        if op == "upload_tool":
            # 工具上传（doc/52 D36 F1）：multipart 的 name 是**用户给的字段**，不是文件名；
            # 文件体就是那一个可执行文件的字节。坏名/预留名/空文件/额度都靠这一条路造。
            if step.get("tokenVar"):
                token = vars.get(step["tokenVar"])
            else:
                token = vars.get("tok") if step.get("auth", True) else None
            content = subst(step.get("content", ""), vars).encode()
            http, body = harness.call_upload(
                resolve_path(step["path"], vars), "tool.bin", content, token,
                fields={"name": subst(step.get("name", ""), vars)})
            if only_print:
                print(f"  {label} upload_tool {step['path']} → HTTP {http} {json.dumps(body, ensure_ascii=False)[:300]}")
                continue
            if not check_step(step.get("expect", {}), http, body, vars, tally, label):
                return False
            continue

        if op == "upload":
            # 技能包上传（doc/52 C1）：包体是二进制 zip，清单里只写包内文件表，这里现打一个包。
            # 这样「zip 穿越 / 缺 description / 正文超限」这些坏包也能进机器真源——
            # 它们的判据就是**包体内容**，不能靠手搓一个固定 zip 放仓库里。
            import io
            import zipfile
            buf = io.BytesIO()
            with zipfile.ZipFile(buf, "w") as z:
                for name, text in (step.get("files") or {}).items():
                    text = subst(text, vars)
                    if step.get("fill_bytes"):
                        # 正文超限用例：在文件末尾追加填充，撑到指定字节数。
                        text = text + "填" * (int(step["fill_bytes"]) // 3)
                    z.writestr(name, text)
            http, body = harness.call_upload(
                resolve_path(step["path"], vars), "pkg.zip", buf.getvalue(), vars.get("tok"))
            if only_print:
                print(f"  {label} upload {step['path']} → HTTP {http} {json.dumps(body, ensure_ascii=False)[:300]}")
                continue
            if not check_step(step.get("expect", {}), http, body, vars, tally, label):
                return False
            continue

        if "sql" in step and "method" not in step:
            # 独立的一步：只跑 SQL（造闸 / 收尾），没有请求跟着
            ok, out = harness.psql(resolve_path(step["sql"], vars))
            if not ok:
                tally.no(f"{label} SQL", out)
                return False
            tally.ok(f"{label} SQL")
            continue

        if "sql" in step:
            ok, out = harness.psql(resolve_path(step["sql"], vars))
            if not ok:
                tally.no(f"{label} SQL", out)
                return False
            print(f"  {label} SQL ok")

        path = resolve_path(step["path"], vars)
        body_in = subst(step.get("body"), vars)
        if step.get("tokenVar"):
            # 用另一个身份的 token 打（如 viewer）：值由 invite_viewer 之类的前置步塞进 vars。
            token = vars.get(step["tokenVar"])
        else:
            token = vars.get("tok") if step.get("auth", True) else None
        http, body = harness.call(step["method"], path, body_in, token)

        for name, expr in (step.get("capture") or {}).items():
            val = harness.pick(body, expr)
            if val is not None:
                vars[name] = val

        if only_print:
            print(f"  {label} {step['method']} {path} → HTTP {http} {json.dumps(body, ensure_ascii=False)[:300]}")
            continue

        if not check_step(step.get("expect", {}), http, body, vars, tally, label):
            return False
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="", help="只跑 id 含该串的用例")
    ap.add_argument("--tier", default="", choices=["", "core", "model"])
    ap.add_argument("--print", action="store_true", help="只打印真实字节（对齐用），不断言")
    args = ap.parse_args()

    if not MANIFEST.exists():
        print(f"找不到 {MANIFEST}", file=sys.stderr)
        return 2
    manifest = json.loads(MANIFEST.read_text())

    key = harness.api_key()
    have_model = bool(key)

    tally = harness.Tally()
    base_vars = {}
    print(f"引擎 {harness.BASE} · 清单 {MANIFEST.name} v{manifest.get('version')}")
    print()
    try:
        signup(base_vars, key, True)
    except Exception as e:
        print(f"引导失败（注册 + 配租户模型）：{e}", file=sys.stderr)
        return 2
    base_vars["key"] = key  # 清单里写 "{key}" 的地方（配密钥的请求体）替换成真密钥

    for case in manifest["cases"]:
        if args.only and args.only not in case["id"]:
            continue
        if args.tier and case.get("tier") != args.tier:
            continue
        if case.get("tier") == "model" and not have_model:
            tally.skip(case["id"], "没有模型密钥（.env 里没有 DEEPSEEK_API_KEY）")
            continue
        if case.get("needs") == "pg" and not harness.PG_DSN:
            tally.skip(case["id"], "没给 TAKO_PG_DSN（这条要直连 PG 造「忙」闸）")
            continue
        if case.get("needs") == "sandbox" and not harness.sandbox_available():
            tally.skip(case["id"], "这台机器没有可用的 sandbox-exec（bash 会 fail closed，doc/35）")
            continue
        # 记忆模式门（doc/65）：观测口字节随 TAKO_MEMORY_MODE 分叉。引擎的模式由操作员起引擎时
        # 定；跑套件时把同一个值给到运行器（TAKO_MEMORY_MODE），两边对不上就跳过并明说，不硬跑假红。
        mode = os.environ.get("TAKO_MEMORY_MODE", "").strip().lower()
        if case.get("needs") == "markdown-mode" and mode != "markdown":
            tally.skip(case["id"], "要 markdown 模式引擎：起引擎与跑本套件都带 TAKO_MEMORY_MODE=markdown")
            continue
        if case.get("needs") == "pipeline-mode" and mode == "markdown":
            tally.skip(case["id"], "markdown 模式引擎跑不了这条（要 pipeline 观测口）")
            continue
        # 每个用例自带上一个账号，别互相污染（用例 0 那条要「没配模型」的租户）
        run_case(case, key, tally, args.print, base_vars)

    if args.print:
        return 0
    failed = tally.summary()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
