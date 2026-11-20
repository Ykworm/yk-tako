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
    # 原始字节响应（artifact 端点不进信封）：body 是 bytes，逐字比 raw / 含 raw_contains。
    if isinstance(body, (bytes, bytearray)):
        text = bytes(body).decode("utf-8", "replace")
        if "raw" in expect:
            want(text == expect["raw"], f"原始字节 {text!r} ≠ {expect['raw']!r}")
        if "raw_contains" in expect:
            missing = [s for s in expect["raw_contains"] if s not in text]
            want(not missing, f"原始字节里缺 {missing}（实有 {text[:160]!r}）")
        if bad:
            tally.no(label, "；".join(bad))
            return False
        tally.ok(label)
        return True
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
                                      {"identifier": email, "password": "changeme123"})
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

        if op == "require_status":
            # 轮询断言「某个 GET 资源的状态 N 秒内保持不变」。持久 Task 的核心契约用例
            # 需要调度关闭的引擎（起引擎带 -task-interval=-1s，施工包 §12.1）才字节确定：
            # 真调度会在 1s 内把 ready 领走。用观测判定而不是操作员自报——状态被领走
            # 就红在这里并说明起法，不往下跑出一串假红。
            path = resolve_path(step["path"], vars)
            token = vars.get(step["tokenVar"]) if step.get("tokenVar") else vars.get("tok")
            want = step["status"]
            seconds = int(step.get("seconds", 3))
            deadline = time.time() + seconds
            while time.time() < deadline:
                _, body = harness.call("GET", path, token=token)
                d = body.get("data", {}) if isinstance(body, dict) else {}
                got = d.get("status") if isinstance(d, dict) else None
                if got != want:
                    tally.no(label, f"{seconds}s 内状态离开 {want} → {got}：引擎的 task 调度没关（起引擎要带 -task-interval=-1s）")
                    return False
                time.sleep(0.5)
            tally.ok(f"{label} 状态保持 {want}")
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

        if op == "sleep":
            # 定长等待（doc/84 用例 85）：等引擎的后台索引链收敛（索引循环 2s 扫描 +
            # relay 5s 投递 + 两个消费者真实 embedding 写 ES/Milvus）。没有可观测的
            # 「收敛」读口（coverage 只反映任务行，不反映 ES/Milvus 已可查），所以
            # 用宽裕的定长等待，不用轮询假收敛。
            time.sleep(int(step["seconds"]))
            tally.ok(f"{label} 等 {step['seconds']}s")
            continue

        if op == "seed_ws":
            # 往项目容器里造文件/目录/符号链接：产出文件在**文件系统**上，SQL 造不出来。
            # 容器布局见 doc/76：<WS_ROOT>/tenants/<tid>/workspaces/<wid>/{layout.json,
            # content/, runtime/{media,tmp,memory,orchestrations}}。seed 里的路径按**逻辑路径**
            # 写（与工具回执一致），落到物理位置：
            #   .media/...            → runtime/media/...
            #   .tako/{bash,fetch,search}/... → runtime/tmp/{kind}/...
            #   其余                   → content/...
            # 写之前先打一次 GET /api/workspaces/<wid>/files：它是真实入口，会经
            # ensureProjectLayout 建好 layout.json 与两棵树。顺序不能反——先往空容器里
            # 写文件就成了「没有 layout.json 却有旧字节」的未迁移项目，入口全 409 117116。
            wid = resolve_path(step["workspace"], vars)
            container = Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "workspaces" / wid
            code, body = harness.call("GET", f"/api/workspaces/{wid}/files", token=vars.get("tok"))
            if code != 200:
                tally.no(label, f"初始化项目布局失败：HTTP {code} {body!r}")
                return False

            def seed_dest(rel):
                rel = rel.strip("/")
                if rel == ".media" or rel.startswith(".media/"):
                    return container / "runtime" / "media" / rel[len(".media/"):] if rel.startswith(".media/") else container / "runtime" / "media"
                for kind in ("bash", "fetch", "search"):
                    prefix = f".tako/{kind}"
                    if rel == prefix or rel.startswith(prefix + "/"):
                        rest = rel[len(prefix):].lstrip("/")
                        return container / "runtime" / "tmp" / kind / rest
                return container / "content" / rel

            try:
                for rel, content in (step.get("files") or {}).items():
                    f = seed_dest(rel)
                    f.parent.mkdir(parents=True, exist_ok=True)
                    f.write_text(content)
                for rel in (step.get("dirs") or []):
                    seed_dest(rel).mkdir(parents=True, exist_ok=True)
                for rel in (step.get("symlinks") or []):
                    link = seed_dest(rel)
                    if link.is_symlink() or link.exists():
                        link.unlink()
                    os.symlink("/etc", link)
            except OSError as e:
                tally.no(label, "造文件失败：%s" % e)
                return False
            tally.ok("%s 造文件 %s" % (label, wid))
            continue

        if op == "seed_unmigrated":
            # 造「非空但没有 layout.json」的旧项目容器（doc/76 §6.1）：旧字节已在盘上、
            # 从未走过新布局初始化。入口必须先拒绝并要求迁移（409 117116），不能自动
            # 把旧字节当 content。**故意不先打 files 接口**——那一步就会建布局，造不出这个态。
            # files 里的键是**容器内相对路径**（如 content/old.txt）。
            wid = resolve_path(step["workspace"], vars)
            container = Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "workspaces" / wid
            try:
                container.mkdir(parents=True, exist_ok=True)
                for rel, content in (step.get("files") or {}).items():
                    f = container / resolve_path(rel, vars).lstrip("/")
                    f.parent.mkdir(parents=True, exist_ok=True)
                    f.write_text(content)
            except OSError as e:
                tally.no(label, "造旧容器失败：%s" % e)
                return False
            tally.ok("%s 造旧容器 %s" % (label, wid))
            continue

        if op == "chmod":
            # 给容器内某个路径改权限（如把容器设成只读，让删除的物理清理失败 → cleanup=pending）。
            # 只作用于本用例自己造的项目容器。
            wid = resolve_path(step["workspace"], vars)
            container = Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "workspaces" / wid
            target = container if step.get("path", ".") == "." else container / resolve_path(step["path"], vars).lstrip("/")
            try:
                os.chmod(target, int(step["mode"], 8))
            except OSError as e:
                tally.no(label, "改权限失败：%s" % e)
                return False
            tally.ok("%s chmod %s %s" % (label, target.name, step["mode"]))
            continue

        if op == "seed_group":
            # 往**组工作区目录**里造隔离组的现场（doc/74 §5.2）：state.json + 不可变结果
            # （result.json 清单 + artifacts/ 字节）+ 验证证据。布局（doc/76 §5.1）=
            # <WS_ROOT>/tenants/<tid>/workspaces/<wid>/runtime/orchestrations/<orchID>/workspace/。
            # 这些是文件系统上的产物，SQL 造不出来；artifact / retry-publication 的 core 用例
            # 靠它把「已固化结果」摆到位，不必真跑模型。
            # artifacts 条目带 content = 写文件；不带 content = 只进清单不写文件（造「产物缺失」117110）。
            orid = resolve_path(step["orchestration"], vars)
            wid = resolve_path(step["workspace"], vars)
            code, body = harness.call("GET", f"/api/workspaces/{wid}/files", token=vars.get("tok"))
            if code != 200:
                tally.no(label, f"初始化项目布局失败：HTTP {code} {body!r}")
                return False
            gdir = (Path(harness.WS_ROOT) / "tenants" / vars["ten"] / "workspaces" / wid
                    / "runtime" / "orchestrations" / orid / "workspace")
            provider = step.get("provider", "directory")
            try:
                gdir.mkdir(parents=True, exist_ok=True)
                now = "2026-11-12T00:00:00Z"
                (gdir / "state.json").write_text(json.dumps({
                    "formatVersion": 1, "workspaceMode": "isolated", "provider": provider,
                    "createdAt": now, "updatedAt": now,
                }, ensure_ascii=False))
                for res in step.get("results") or []:
                    tid = resolve_path(res["task"], vars)
                    rev = int(res["revision"])
                    rdir = gdir / "results" / tid / str(rev)
                    (rdir / "artifacts").mkdir(parents=True, exist_ok=True)
                    manifest = []
                    for a in res.get("artifacts") or []:
                        ap = resolve_path(a["path"], vars)
                        manifest.append({"path": ap, "kind": a.get("kind", "")})
                        if "content" in a:
                            f = rdir / "artifacts" / ap
                            f.parent.mkdir(parents=True, exist_ok=True)
                            f.write_text(subst(a["content"], vars))
                    (rdir / "result.json").write_text(json.dumps({
                        "taskId": tid, "resultRevision": rev, "provider": provider,
                        "artifacts": manifest, "createdAt": now,
                        "reference": res.get("reference", "seed-ref"),
                    }, ensure_ascii=False))
                for ev in step.get("verification") or []:
                    tid = resolve_path(ev["task"], vars)
                    vdir = gdir / "verification"
                    vdir.mkdir(parents=True, exist_ok=True)
                    out = ev.get("outputPath", "")
                    (vdir / (tid + ".json")).write_text(json.dumps({
                        "taskId": tid, "status": ev.get("status", "passed"),
                        "exitCode": int(ev.get("exitCode", 0)), "outputPath": out,
                        "startedAt": now, "finishedAt": now,
                    }, ensure_ascii=False))
                    if out and "output" in ev:
                        (gdir / out).parent.mkdir(parents=True, exist_ok=True)
                        (gdir / out).write_text(subst(ev["output"], vars))
            except OSError as e:
                tally.no(label, "造组现场失败：%s" % e)
                return False
            tally.ok("%s 造组现场 %s" % (label, orid))
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
            # capture_stdout：把这条 SQL 的 stdout（如 INSERT … RETURNING id 的 id）存进 vars。
            # psql 对 DML 会追加「INSERT 0 1」命令行，取**第一行非空输出**当值。
            # 「造一行资源再拿它的 id」目前只有 SQL 一条路——HTTP 侧没有建会话的端点。
            lines = [l for l in out.splitlines() if l.strip()]
            for name in step.get("capture_stdout") or []:
                vars[name] = lines[0] if lines else ""
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
        http, body = harness.call(step["method"], path, body_in, token, raw=bool(step.get("raw")))

        for name, expr in (step.get("capture") or {}).items():
            val = harness.pick(body, expr)
            if val is not None:
                vars[name] = val

        if only_print:
            # --print 是「对齐用」：JSON 详情要整段打出来才好抄期望字节；
            # 只有原始字节（artifact／media）才截断——它可能很大。
            shown = repr(body[:200]) if isinstance(body, (bytes, bytearray)) else json.dumps(body, ensure_ascii=False)
            print(f"  {label} {step['method']} {path} → HTTP {http} {shown}")
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
        # 对话检索模式门（doc/84）：status/query 字节随 TAKO_DIALOGUE_RETRIEVAL 分叉。
        # 引擎的模式由操作员起引擎时定；跑套件时把同一个值给到运行器，两边对不上就
        # 跳过并明说，不硬跑假红（与 memory-mode 门同款约定；缺省 = pg_search）。
        dlg = os.environ.get("TAKO_DIALOGUE_RETRIEVAL", "").strip().lower()
        if case.get("needs") == "dialogue_hybrid" and dlg != "hybrid":
            tally.skip(case["id"], "要 hybrid 检索引擎：起引擎与跑本套件都带 TAKO_DIALOGUE_RETRIEVAL=hybrid（doc/84）")
            continue
        if case.get("needs") == "dialogue_pg_search" and dlg == "hybrid":
            tally.skip(case["id"], "hybrid 引擎跑不了这条（119005 只在非 hybrid 模式出现，doc/84）")
            continue
        # 每个用例自带上一个账号，别互相污染（用例 0 那条要「没配模型」的租户）
        run_case(case, key, tally, args.print, base_vars)

    if args.print:
        return 0
    failed = tally.summary()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
