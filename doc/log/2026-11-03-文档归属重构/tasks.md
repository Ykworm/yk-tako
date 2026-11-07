> **性质**：执行记录（冻结）。状态以 `doc/README.md`、`doc/53` 与代码为准，本文件不再更新。
> **结果**：A1–A4 全部执行完成（2026-11-04 收口）；`docs/` 已删除，记录进 `doc/log/`，旧线/旧代进 `doc/archive/`，规则文本与 `scripts/check-doc-layout.sh` 同批落地。
> **未做**：`doc/17` 归属、`doc/blog/` 去留、状态词表统一、大文档拆分（见 planning 待拍项）。

# 文档归属重构 · todos

> 本轮只做「归属」：`docs/` 并入 `doc/`，旧线整代入 `doc/archive/`，规则文本与机器闸同步。
> 这是纯文档工程，没有 HTTP 端点；每个 action 的验收第一行是在真实目录上跑 `ls`/`grep`，等价于功能 loop 的 `curl`。
>
> **A1 开工前必须满足**：工作区现有 doc 改动已提交（见 planning.md「开工前置 P0」）。
> **每个 action 做完即停，等验收，不连续推进。**

---

## A1 · 建 `doc/log/`，搬 9 篇记录

**改什么**

1. `mkdir -p doc/log`。
2. 搬 8 篇已跟踪文件（`git mv`）：
   - `docs/2026-09-30-uncommitted-review-handoff.md`
   - `docs/2026-10-07-v6-gap-handoff.md`
   - `docs/2026-10-08-pre-t3-handoff.md`
   - `docs/2026-10-08-subagent-design-handoff.md`
   - `docs/2026-10-12-overflow-handoff.md`
   - `docs/2026-10-16-overflow-b2b3-handoff.md`
   - `docs/2026-10-19-多租户-handoff.md`
   - `docs/2026-10-09-deferred-loading.md`
3. 搬 1 篇未跟踪文件（`mv`）：`docs/2026-11-02-gaia-k5-两臂可靠性对照.md`。
4. 新建 `doc/log/README.md`：9 行表格，列「日期 / 文档 / 类型 / 一句话」。
5. 改 4 处入边：
   - `doc/28-多租户.md`：`docs/2026-10-19-多租户-handoff.md` → `doc/log/…`；
   - `doc/32-media-offload.md`：`docs/2026-09-30 handoff 问题①` → `doc/log/2026-09-30-uncommitted-review-handoff.md 问题①`；
   - `doc/53-模块地图.md` §7.2：`docs/` 段落改成 `doc/log/` + `doc/archive/` 两段；
   - `doc/README.md` §4：整表路径改到 `doc/log/`，并加一行指向 `doc/log/README.md`。
6. 把临时链接检查器 `tmp/doc-refactor/check_links.py` 写出来（只读，不修文件）。

**为什么**：记录层独立后，`docs/2026-10-19` 这类仍在服役的交接稿不会被目录名误判为过期；同时为 A2 腾空 `docs/`。

**怎么验收**

```bash
ls doc/log/*.md | wc -l          # 期望 10（9 篇 + README）
ls docs/*.md 2>/dev/null | wc -l # 期望 0（8 篇已搬；GAIA 那篇视 P1 决定）
grep -rn "docs/2026" doc AGENTS.md README.md --include='*.md'   # 期望无输出
python3 tmp/doc-refactor/check_links.py                          # 期望 broken: 0
```

**这一拍回答什么问题**：记录层能不能独立存在，且所有指向它的链接仍可解析。

---

## A2 · 建 `doc/archive/`，搬 31 篇旧线/旧代（✅ 2026-11-03）

**实际做了什么**

1. 建两个子目录：`doc/archive/old-line/`（17 篇正文 + `plans/` 8 篇 + `specs/` 2 篇）、`doc/archive/engine/`（4 篇）。
2. 搬 17 篇旧线：`doc/01`–`doc/15`（14 篇）、`doc/18`、`doc/XX-taskboard-重命名-handoff.md`、`doc/handoff-doc24-滤思考与Usage触发.md` → `doc/archive/old-line/`。
3. 搬 4 篇旧代：`doc/19-运行引擎架构-v3.md`、`doc/19-运行引擎架构-v4.md`、`doc/19-v5/` 两篇 → `doc/archive/engine/`。
4. 搬 10 篇旧线计划与设计：`docs/superpowers/{plans,specs}/` → `doc/archive/old-line/{plans,specs}/`（用户判定 `superpowers` 名字碍眼，不保留该分组）。
5. 新建 `doc/archive/README.md`：按 `old-line/`（含 plans/specs）、`engine/` 两组列文件名与一句话说明。
6. 改 `doc/README.md` §2（两组指针表）、§4（从 `docs/` 表删掉 10 行旧线计划）、`doc/53` §7、`doc/16` 头注、`doc/41` 两处引用。
7. **偏差**：归档正文里的旧路径（`docs/superpowers/...`、`doc/07...`）不批量改写，只在 `doc/archive/README.md` 头注声明「当时的位置」；只修真正断掉的 markdown 链接（`02` 的 `AGENTS.md`、一处非方案目录的文件名笔误）。

**验收实录**

```bash
find doc/archive -name '*.md' | wc -l   # 32（31 篇 + README）
ls doc/*.md | wc -l                     # 43（42 篇现行 + README）
python3 tmp/doc-refactor/check_links.py # broken: 0
find doc/archive -maxdepth 2 -type d    # old-line/{plans,specs}、engine
```

**这一拍回答什么问题**：历史能不能物理隔离，同时现行文档指向历史的链接仍能解析。

---

## A3 · 改规则文本，让规则与目录一致

**改什么**

1. `AGENTS.md`：
   - 「仓库结构」里 `docs/` 一行替换为 `doc/log/`、`doc/archive/`；
   - 「文档状态与真源」里 `docs/` 那段改成四类归属（现行 / 裁定 / 记录 / 归档）；
   - 「新增或修改文档时」补一条：文件名带日期必须进 `doc/log/`，被取代整代必须进 `doc/archive/`。
2. 根 `README.md`：目录树里的 `docs/` 换成 `doc/log/`、`doc/archive/`。
3. `doc/README.md` §0 / §3 / §5：
   - 「不要先读」的 `docs/` 改成 `doc/archive/`；
   - §5 规则按四类归属重写，明确「时效变化不搬家，只改状态」。
4. `doc/53-模块地图.md` §7：更新目录结构与篇数（旧写「顶层 55 篇」已过期）。
5. 检查 `doc/16-能力复刻总规划.md` 是否引用旧路径，有则改。
6. 台账生命周期（2026-11-03 拍板）写进 `AGENTS.md`「台账产物」与 `doc/README.md` §5：
   - 工作期仍用 `tmp/<方案>/planning.md` + `todos.md`，不入库；
   - 验收时把 `todos.md` 原样搬进 `doc/log/<YYYY-MM-DD>-<方案>/tasks.md`，顶部加状态头并冻结；
   - `planning.md` 的结论提炼进同目录 `README.md`，不原样留；
   - 删 `tmp/<方案>/`；`tasks.md` 只作执行记录，与 `doc/NN` 冲突以 `doc/NN` 为准，不进机器检查。
7. **（2026-11-03 已先行）** v6 版本口径：在 `doc/19-v6` index、`doc/README` §0、`AGENTS.md`、根 `README.md`、`doc/16` 首次出现处写明「v6 = 第六代运行引擎与其文档集，不是服务版本／API 版本」。

**为什么**：如果只搬目录不改规则，下一场会话仍会按旧规则把新交接稿放进不存在的 `docs/`。

**怎么验收**

```bash
grep -rn "docs/" AGENTS.md README.md doc --include='*.md' \
  | grep -v 'doc/archive/' | grep -v 'doc/log/'   # 期望无输出
bash scripts/check-master-plan.sh                  # 期望 OK
git diff --stat                                    # 人工核：只动上述文件
```

**这一拍回答什么问题**：规则文本和真实目录是否一致，下一场会话有没有明确的家可放。

---

## A4 · 加机器闸，收口

**改什么**

1. 把临时检查器转正为 `scripts/check-doc-layout.sh`，检查五件事：
   - `docs/` 不存在；
   - `doc/` 顶层没有日期命名文件；
   - `doc/log/` 顶层每个 `.md`（除 README）都以 `YYYY-MM-DD-` 开头；每个子目录也以 `YYYY-MM-DD-` 开头，且其中只能有 `README.md` 与 `tasks.md`；
   - `doc/`、`docs/` 内所有相对 `.md` 链接可解析；
   - `doc/archive/README.md` 覆盖 `doc/archive/` 下全部 `.md`。
2. 删除空目录 `docs/`。
3. 更新 `doc/16-能力复刻总规划.md` 头注的基线日期与文档体系描述。
4. 建本次重构的 `doc/log/2026-11-03-文档归属重构/`：
   - `README.md`：病灶、拍的轴、A1–A4 做了什么、验收结论、关联真源；
   - `tasks.md`：把本台账的 `todos.md` 原样搬入，顶部加「执行记录／结果／未做」状态头，冻结。
5. 跑全套验收，把结果抄进 `tmp/doc-refactor/acceptance.md`。

**为什么**：规则只靠纪律会腐化；这次改动后要有一个命令能判断「归属有没有被下一场会话破坏」。

**怎么验收**

```bash
bash scripts/check-doc-layout.sh    # 期望 OK，退出码 0
bash scripts/check-master-plan.sh   # 期望 OK，退出码 0
test ! -d docs && echo "docs gone"  # 期望 docs gone
git status --porcelain              # 人工核：只有预期改动
```

**这一拍回答什么问题**：新人下一轮进仓库，能不能只看到四类归属，且破坏归属会被脚本判红。
