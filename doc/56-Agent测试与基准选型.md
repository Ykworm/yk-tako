# 56 · Agent 测试与基准选型

> 性质：**方案（部分落地，2026-10-31 立项）**。回答「拿什么测现在的 tako」——哪些公开基准值得跑、谁来跑、多久跑一次、哪些只该借方法不该搬环境。
> 状态：🔶 部分落地。**已施工（2026-11-01）**：`scripts/bench/`——行 schema（`row.schema.json`）、**两臂（tako / dsh）**、判定器（`judge.py`，内置官方 scorer 三支 + `--selftest`）、切片配方与派生核对（`cases/gaia_l1_25.json` + `slice.py`）、批量 runner（`run_bench.py`，支持多臂配对）、价目表（`pricing.json`）、键集守门（`check-schema.sh`）；**§4.7 可比性协议**已落纸。**G1 已跑完**（2026-11-01）：GAIA 25 题两轮单臂实测——题面本身 2/25，题面 + 尾句 21/25，口径钉死见 §4.8；**两臂配对也已跑完**——tako 22/25 vs dsh 20/25，见 §4.9。**pass^k 与基线归因已施工（2026-11-01 夜）**：`--repeat` / `--baseline` / `--write-baseline` + 归因五态（§6.1），trace 取证走 Langfuse v2（`scripts/bench/traces.py`）。**D1 首轮 k=5 基线已跑完（2026-11-02 凌晨）**：tako Pass@5 24/25 · pass^5 20/25，dsh 23/25 · 14/25，基线落 `scripts/bench/baselines/gaia.json`（§4.10）。**未施工**：pi 臂（未装）、SWE-bench（gated）。
> 日期：2026-10-31 立项；2026-11-01 第一批落地（口径见 §4.7），同日第二批落地并实跑出数（§4.8），同日第三批：装 dsh 臂并跑两臂配对（§4.9）；11-02 凌晨第四批：k=5 基线与 pass^k 归因（§4.10）。
> 代码落点：`scripts/bench/`（公开题标定）、`scripts/eval/`（doc/45 内部回归，本方案只回填指标，不另起 runner）；**不改 `internal/runengine/` 的生产代码**（本轮实测确认：跑评测不需要改引擎一行）。
> 参照：doc/45（内部场景回归，本方案的下层）、doc/16 §2b 第 6 项、用户 2026-10-31 给的候选清单（SWE-bench / HumanEval / MBPP / LiveCodeBench / GAIA / τ-bench / AgentBench / Claw-Eval）。
> 与 doc/45 的关系：doc/45 测「tako 的机制有没有把活干完」；本篇测「模型+tako 在标准任务上有多强」。两层不合并，也不互相替代。

---

## 0. 一句话结论

**公开基准测的是「模型 + 它自带的脚手架」，不是 tako。** 在这份清单里：

- 能**直接跑 tako**、且值得跑的只有 **GAIA**（验证集 Level 1 的纯文本切片）——它不需要容器，正好落在 tako 现有的 `web_search` + 文件工具 + 多轮 loop 上；
- **SWE-bench** 是编码 agent 的事实标准，但它要 Linux 容器。**开发机没有 Docker，评测机可以是 Linux 服务器**（装 Docker 即可）：v1 先在服务器上用 **mini-swe-agent** 跑 25 题，只当**模型基线**（明确标注「不是 tako 分数」）；tako 自己跑要补的不是 Docker，而是「bash 在 Linux 上能执行」——现在没有 `sandbox-exec` 就 fail closed，谁当围栏见 §5.5；
- **τ-bench / Claw-Eval 不移植环境**，只借三样判定方法（pass^k、终态断言、安全一票否决）回填 doc/45——搬它们的环境比写我们自己的失败场景更贵，而且它们不测我们真正怕的缺陷；
- **HumanEval / MBPP / LiveCodeBench / AgentBench 不作为 tako 的测试**：前两个是单步函数题且已饱和/污染，LiveCodeBench 只测模型，AgentBench 已停更；
- **CI 门禁仍然只认确定性断言**（快闸 + 契约 + doc/45 的 K/S 两组）。公开基准只出分数与趋势，不判红绿。

---

## 1. 先分清「测谁」：三种测试，三个受试者

### 1.1 一个会真实发生的混淆

团队换了模型（或改了 provider），顺手跑了 SWE-bench Verified，分数从 42% 涨到 48%，于是认为「agent 变强了，可以发版」。但同一周，tako 的 PII 仍然在折叠后复活、看板仍然可能空着、失败轮仍然可能不落库——这三件事在 SWE-bench 的任务分布里**根本不存在**，它的判定也不会看它们。

分数涨了是真的，产品更可靠是假的。问题不在基准坏，在于**它测的不是你改的那一层**。

### 1.2 三个受试者

| 你在问什么 | 受试者 | 用什么 | 判据 | 什么时候跑 |
|---|---|---|---|---|
| 代码有没有按约定做事？ | tako 的机制 | 快闸 `go test` + 契约 `scripts/acceptance/` | 字节/码值，红绿 | 每个 action / 每次提交 |
| 同一件事重复做，几次能做成？ | tako + 模型 | doc/45 场景回归（未施工） | K/S 组硬断言，红绿 + 通过率 | 发布前 / 每天 |
| 这个模型在标准任务上有多强？ | 模型（+它的 harness） | 公开基准（本篇） | 分数，只作趋势 | 换模型 / 改提示 / 季度 |

**关键区分**：第二行才是「对 agent 的测试」；第三行是「对模型的体检」。把第三行的分数当第二行的门禁，会得到一个又慢又抖、还抓不到真缺陷的 CI。

---

## 2. 候选基准逐项核实（2026-10 现状）

下表先给结论，逐项解释在后。

| 基准 | 规模 | 谁在受试 | tako 能直接跑吗 | 成本量级 | 结论 |
|---|---|---|---|---|---|
| SWE-bench Verified / Lite | 500 / 300 | 模型 + 评测 harness | **不能**（要 Linux Docker；本机无 Docker，Linux 沙箱 fail closed） | 单题 $0.04–2；25 题 1–3 小时 | 先跑**模型基线**（mini-swe-agent），tako 级 gated |
| HumanEval / MBPP | 164 / 974（sanitized 427） | 模型 | 能，但不测 loop | 极低 | 不采纳（单步、饱和、污染） |
| LiveCodeBench | v6 1055；Pro 584 | 模型 | 能，但不测 loop | 低 | 只在换模型时做模型侧标定 |
| GAIA | 466 = 301 test + 165 val（L1 53 / L2 86 / L3 26） | 模型 + agent harness | **能**（workspace + web_search + 文件工具） | 25 题约 $1–5，20–60 分钟 | **采纳** 25 题 L1 文本切片 |
| τ-bench / τ² / τ³ | retail / airline（+telecom / banking） | 模型 + 用户模拟器 + 领域 API | 要移植整套环境 | 高 | 不移植；借 pass^k + 终态断言 |
| AgentBench | 8 环境 | 模型 + Docker 环境 | 能但不值得 | 中 | 不采纳（2025-02 后停更） |
| Claw-Eval | 300 任务 / 2159 条细则 | 模型 + 它自己的 harness | 要移植环境与评分细则 | 高 | 不移植；借 Pass^3 + 安全门 + 过程审计 |
| Terminal-Bench 2.1（补充） | 未核到稳定规模 | 模型 + 终端环境 | 要容器 | 中高 | 待观察；与 SWE-bench 同批评估 |

### 2.1 SWE-bench（Verified 500 / Lite 300 / 全量 2294）

- **是什么**：真实 GitHub Python 仓库的 issue → 模型交 patch → 应用后跑 `FAIL_TO_PASS`（原来红、现在必须绿）与 `PASS_TO_PASS`（原来绿、现在仍须绿）。判定是执行测试，不是模型自评。
- **它到底考谁**：它不是 HumanEval 那种「给函数签名、模型闭卷写」的纯模型题——它给的是一个可执行环境（仓库快照、依赖、测试）和多轮工具预算，模型必须探索仓库、跑测试、改文件、再跑，才能交卷。**分数归属于「模型 + 脚手架」这个组合**：同一个模型换 mini-swe-agent / SWE-agent / OpenHands，分数会变；同一个脚手架换模型，分数也会变。所以榜单写「某模型 45%」时，省略的主语其实是「某模型 + 某 harness + 某步数上限 + 某提示词」。说它「考 LLM」只对一半：考的是**模型在给定脚手架下被激发出来的工程能力**，不是模型脑子里的知识。
- **怎么读榜单**：先看 harness 与步数上限，再比分数；不同 harness 的数字不能直接横比，更不能搬来解释 tako。
- **环境要求**：官方 harness 用 Docker 跑每个实例；社区建议 x86_64、16 GB 内存、8 核、120 GB 空闲磁盘。单实例成本官方榜单给到 $0.04–2（看模型），官方 mini-swe-agent 默认 250 步、墙钟 1800 秒。Verified 的榜单评测走官方 `sb-cli` 云端，本地复跑只作内部趋势，不对外可比。
- **对 tako 的含义**：这是最能说明「coding agent 修真实代码」的指标。**Docker 不是不可逾越的前置**——把评测放到 Linux 服务器（华为云那台）上装 Docker 即可；真正缺的是 tako 自己的执行面：`bash` 只在 macOS `sandbox-exec` 下工作，Linux 上没有可用的围栏就 fail closed。所以「服务器能跑 Docker」只解锁 C2（模型基线）；要让 tako 进容器，先回答 §5.5 的「谁当围栏」。同样一套题，主语不同结论不同：**mini-swe-agent 跑 = 选模型（C2）；tako 跑 = 验 tako 的 loop/工具（G4）**；两者分数不可直接比。

**决策**：分两步。
1. **现在**：用 mini-swe-agent 跑 25 题，只产出「模型基线」——它回答的是「这个模型解 issue 的水平」，不是「tako 的水平」。报告标题必须写清 harness 与模型，避免以后有人拿它当 tako 的成绩。
2. **以后**（gated）：tako 容器化 + Linux 沙箱落地后，再跑 tako 级 SWE-bench（§7 G4）。

### 2.2 HumanEval / MBPP

- **是什么**：HumanEval 164 道手写函数题；MBPP 974 道众包题（最常报的 sanitized 子集 427 道）。都给函数签名/描述，模型补函数体，跑单元测试。
- **为什么不采纳**：它们是**单步生成**，没有工具、没有文件、没有多轮 loop——测的是模型的补全能力，不是 tako 的 agent 能力。而且强模型已经在 90%+，天花板效应明显；题目公开多年，污染不可避免。拿它当 CI 门禁，红了大概率是模型抽样抖动，不是 tako 坏了。
- **唯一可能的用途**：如果哪天想加一个「最便宜的慢档烟雾」——让 tako 用 `write_file` 写答案、用 `bash` 跑测试——可以用 10–20 道 MBPP。但这本质上是在测工具链能不能跑通，doc/45 的场景 5–8 已经用真实失败故事覆盖了，优先级更低。

### 2.3 LiveCodeBench

- **是什么**：持续从 LeetCode / AtCoder / Codeforces 收新题，按发布日期切窗口防污染；v6 共 1055 题（覆盖到 2025-04），Pro 584 题，季度更新。
- **谁在受试**：模型直接写代码，不经过 agent。它回答的是「模型的竞赛编程水平」，与 tako 的 loop、工具、记忆都无关。
- **决策**：不作为 tako 测试。只有在**换基座模型**、需要判断「模型本身有没有退化」时，作为模型侧标定跑一次；不写进 tako 的回归报告。真要跑也只需一个固定切片的分数，不要全量 1055。

### 2.4 GAIA（唯一现在就能跑 tako 的公开基准）

- **是什么**：466 题，301 test + 165 validation；validation 里 Level 1 是 53 题、Level 2 86 题、Level 3 26 题。答案都是可精确比对的事实（数字、名字、日期），题目需要搜索、读页面、算数、看附件，多步推理。
- **谁在受试**：模型 + agent harness。它不规定工具接口，所以**可以用 tako 自己的 HTTP 面跑**：每个问题开一个 run，给一个空 workspace，让模型自己用 `web_search` / 文件工具 / `bash` 去找答案。
- **为什么适合 tako**：不需要容器；正好覆盖 tako 的「通用」那一半（搜索、文件、多轮）；题目有标准答案，判定便宜且确定。
- **已知上限（必须提前写清）**：GAIA 的分不在「模型会不会推理」，多半卡在「拿不到那一页」。`web_fetch` 已于 [doc/57](57-网络抓取.md) 落地（2026-11-01，走 DeepSeek Fetch 接口），抓取这条主要通路已经通了；剩下三条限制不会消失：① 抓取只有 `deepseek` 一行实现、要 `DEEPSEEK_API_KEY`，其他厂商直接报错；② 抓回的正文落盘，模型还得自己 `read_file`——read 已改成分页（每页 2000 行 / 50KB，doc/58），长页能读到中间，但**一页只给 2000 行**，读长页要多跳；③ 它不是浏览器——JS 渲染、要登录、反爬的页面拿不到正文。所以**预期分数仍会低于公开榜**，这不是 bug。评测脚本不许替模型抓页面或代读文件——那会让分数失去意义。
- **决策**：采纳 **25 题 L1 纯文本固定切片**（`Level == 1` 且无附件 `file_name`），作为「通用能力烟雾」；只出分数与失败清单，不设门槛。25/165 是自建分母，**不与官方榜同表比较**。

### 2.5 τ-bench / τ²-bench / τ³-bench

- **是什么**：零售 / 航空（τ² 加电信，τ³ 加银行与语音）的客服场景。一个 LLM 扮演用户，agent 调领域 API，判定看**对话结束后的数据库状态**是否等于目标状态，以及是否说出必需信息（如退款金额）。核心指标 **pass^k**：同一题连续 k 次全过才算过；强模型 pass@1 可能 60%+，pass^8 掉到 25% 以下。
- **为什么不移植**：要搬零售/航空的 mock API、政策文件、用户模拟器、终态判定，工作量远大于写一批我们自己的场景；而且它测的是「客服政策遵从」，不是 tako 的 PII、围栏、看板、续跑。
- **借什么**：pass^k 的思想（**能力天花板 vs 可靠性地板**）和「读终态、不读模型自述」的判定原则。doc/45 的判定口径已定为 **M=M（跑 M 次全过）= pass^k**，不需要额外加容忍层；读库/读文件那组（K/S 组）本来就读终态，符合终态断言。
- **版本注意**：原始 τ-bench 仓库已标注过时，后续是 τ² / τ³；如果将来真要做客服域评测，从 τ³ 起步。

### 2.6 AgentBench

- **是什么**：8 个环境（OS、DB、知识图谱、卡牌、横向思维、家务、网页购物、网页浏览），ICLR'24，Docker 封装。
- **为什么不再采纳**：最后一次实质更新在 2025-02 左右，任务分布与工具生态已经落后；它同样是「环境 + 模型」的横向对比，不是产品回归。跑它要维护 8 套容器，收益远低于自建场景。
- **决策**：不采纳。它的「多环境」叙事已经被更新的基准（GAIA、τ、Claw-Eval、Terminal-Bench）覆盖。

### 2.7 Claw-Eval

- **是什么**：300 道人工验证任务、9 个类别、2159 条可独立验证的评分细则；同时看**结果**与**过程**，用 Pass@3（能力天花板）与 Pass^3（三次全过的可靠性地板）分开报告；安全是乘法门——一旦违规，任务分归零。论文报告最强模型 overall Pass^3 约 70.4%，错误注入会让 Pass^3 掉 24 个百分点，而 Pass@3 几乎不动。
- **为什么不移植**：它自带环境与评分细则，移植成本高；并且它的任务分布（通用服务编排、多模态、专业对话）不等于 tako 的失败故事。
- **借什么**：
  1. **Pass^3 与 Pass@3 分开报**——doc/45 的 B/C 组按这个口径输出；
  2. **安全一票否决**——PII 泄漏、围栏越界、越权访问任何一条命中，整场失败，即使任务完成；
  3. **过程可审计**——doc/54 的 step span 已经能给出每一步耗时，天然支持「过程指标」；但 CozeLoop 目前没有公开的查询 API，自动报告先用 frames 的步数/工具数 + 墙钟，span 供人进控制台看。

### 2.8 Verified 的小切片（用户提到的 `verified_mini`）

官方仓库与榜单里没有叫 `verified_mini` 的数据集；本次核到的是社区/工具链把 Verified 切成小规模子集的用法。**本方案不依赖这个名字**：直接固定 25 个 `instance_id` 写在配置里，保证不同时间、不同机器跑的是同一批题，报告里附上清单。

---

## 3. 推荐组合：四层，两个外部标定

```
┌──────────────────────────────────────────────────────────────┐
│ L0 快闸        go test ./internal/runengine                   │ 0 元 · 秒级 · 每拍
│               信封字节 / 帧 kind / 业务码                      │
├──────────────────────────────────────────────────────────────┤
│ L1 契约        scripts/acceptance/                            │ 0 元 · 分钟 · 改契约时
│               expected.json + assert_contract.py              │
├──────────────────────────────────────────────────────────────┤
│ L2 场景回归    scripts/eval/（doc/45，未施工）                 │ 模型钱 · 10–30 分钟
│               PII / 看板 / 续跑 / 围栏 / 并发                  │ ← 唯一可做门禁的模型层
├──────────────────────────────────────────────────────────────┤
│ C1 通用标定    scripts/bench/run_bench.py（25 题 L1）         │ 模型钱 · 20–60 分钟
│               走 tako 的 HTTP 面，测 loop + 搜索 + 文件         │ 只出分数
├──────────────────────────────────────────────────────────────┤
│ C2 编码标定    脚本封装 mini-swe-agent（25 题 Verified）        │ 模型钱 · 1–3 小时
│               不经过 tako，测的是模型                            │ 只出分数
├──────────────────────────────────────────────────────────────┤
│ G4 tako 级     tako 进容器跑 SWE-bench / Claw-Eval             │ gated
│               前置：服务器 Docker + bash 容器后端（§5.5）        │ 未解锁
└──────────────────────────────────────────────────────────────┘
```

三条使用规矩：

1. **只有 L0–L2 能判红绿。** C1/C2 输出分数、成本、失败清单；分数下降先怀疑模型/网络/抽样，不直接判 tako 回归。
2. **C1 与 C2 不能混在一张表里比较。** 一个经过 tako，一个不经过；口径不同，只能各自纵向比。
3. **每次外部运行都要落盘可复现信息**：引擎 commit、模型 ref、数据集 revision、切片 id、步数/成本上限、started_at。缺一项，报告就只是一次性截图。

---

## 4. C1：GAIA 25 题怎么跑（现在就能做）

### 4.0 先对账：已经有的 / 还差的

| 环节 | 现状 | 还差什么 |
|---|---|---|
| 搜索 | `web_search`（deepseek 一行 + searchfree 兜底） | — |
| 抓页面 | `web_fetch`（doc/57 已落地）：整页落 `.tako/fetch/*.md`，工具只回链接、token、路径 | 只认 deepseek 厂商；不是浏览器（JS / 登录 / 反爬页拿不到正文） |
| 读文件 | `read_file` / `read_text_file`（分页读，每页 2000 行 / 50KB，页脚给下一个 `offset`）、`read_pdf_text`、`read_media_file` | xlsx 一类没有专用读法，靠 bash+python |
| bash | macOS 走 `sandbox-exec` | Linux 无沙箱时 fail closed（doc/35）；评测机若是 Linux，先当没有 bash |
| 起 run | `POST /api/runs`，可带 `workspaceId`；不带就用默认项目 | — |
| 收 run | 轮询 `GET /api/runs/{id}` 到终态；`POST /api/runs/:runId/stop` 可停 | **引擎没有步数/轮数上限**，超时只能由 harness 打 stop |
| 证据 | frames（工具调用与结果、usage 帧）、runlog、doc/54 traceId | 没有批量汇总，报告得 harness 自己拼 |
| 批量 runner | **有**：`scripts/bench/run_bench.py`（题集 × 臂 → 一行一题 → md+json 报告，支持多臂配对）；2026-11-01 实跑两轮 25 题单臂（§4.8）+ 一轮 25 题**两臂配对**（§4.9），报告口径字段全套齐 | pi 臂未装；三臂并排那部分留到下一轮 |
| 数据 | HF `gaia-benchmark/GAIA` 的 validation（test 答案不公开） | 需 HF 账号并接受条款；**切片清单已建**（只入配方 + `slice_hash`，明文不入库） |
| 判定 | **官方 scorer 三支已内置**：`scripts/bench/judge.py`（`--selftest` 拿官方源码当对照实现，22 条全一致） | — |

结论：**单题今天就能手动跑完**；25 题「跑完」差的三件工程件——切片清单、批量 runner、报告——**2026-11-01 已补齐**（见 §4.7 的口径与 §4.8 的实跑结果）。

### 4.1 准备

- **数据**：GAIA validation 里 `Level == 1` 且没有附件（`file_name` 为空）的题，按 `task_id` 排序取前 25 个。**入库的只有配方**（`scripts/bench/cases/gaia_l1_25.json`：源仓、源文件、revision、筛选、排序、题数、`slice_hash`），**题面 / 标准答案 / task_id 不入库**——GAIA 条款要求不外发数据集（§4.7.8）。明文留本机 gitignore 目录，由 `scripts/bench/slice.py` 从官方源派生并重算 hash 核对。不要每次随机抽——抽样会让分数不可比。
- **引擎**：隔离实例（独立 PG 库 + 独立 Redis db + 独立 workspace），起法沿用 `scripts/acceptance/README.md` 的纪律；**不能拿开发库当靶子**。
- **账号**：用 harness 已有的注册/登录/租户装模型密钥流程；每题一个独立项目（`POST /api/workspaces`），否则上一题的 `.tako/fetch`、`.tako/search`、notes 与产出文件会被下一题读到。

### 4.2 跑一场

```bash
# 真命令（2026-11-01 起）
python3 scripts/bench/run_bench.py --cases gaia --arms tako
# 只跑 1 题先看管道通不通：
python3 scripts/bench/run_bench.py --cases gaia --arms tako --limit 1
# 两臂配对（tako + dsh，同题同模型同口径，只差 harness；§4.9）：
python3 scripts/bench/run_bench.py --cases gaia --arms tako,dsh --tail "最后一行只输出答案，不要解释。"
```

**两臂的差别只在这一行 `--arms`**：tako 臂走引擎 HTTP 面（要起隔离引擎），dsh 臂一题一进程（`dsh --profile headless --json "<题面>"`，每题一个干净工作目录，答案在 stdout 的 `final` 事件里）。其余一切——切片、模型、搜索后端、密钥、user message、判定器、抽取规则、价目表——两臂共用。

对每题：

1. `POST /api/workspaces` 建一个带 `task_id` 的项目，`POST /api/runs` 带 `workspaceId`、不带 `sessionId`（会话由引擎懒建，绑在这个项目上）；
2. 消息 = **题面 + 固定尾句「最后一行只输出答案，不要解释。」**（口径 B，§4.8 钉死；不注入官方 system prompt，用各臂自己的默认行为，理由见 §4.7.2）；
3. 轮询 `GET /api/runs/{id}` 到终态，跟 `nextOffset` 拉全 frames（`eventOffset` 是**排他**游标，第一页用 `0`；越过末帧再问会返回 `status:generating`，**终止信号取 `terminal` 帧**）；超过墙钟预算就 `POST /api/runs/:runId/stop`，结果记「超时」，与「答错」分开；
4. 取 `delta` 帧拼接后的**最后一个非空行**，按 4.7.3 去答句前缀得到 `answer`，再按官方 `question_scorer` 的三支规则与标准答案比对（数字按数值比、列表长度必须相等、字符串去空白与大小写差——实现照抄官方源码，见 4.7.4）；
5. 记录 runId、状态、步数（= usage 帧条数）、工具调用数、耗时、token；traceId 从引擎日志的 `trace <sid>/<rid> traceId=<hex>` 行回填（`--attach-traces`）。**失败题的 frames 不另外落盘**——引擎已经按 runId 存着，报告里带着 runId 就能回查（`GET /api/runs/:id/frames`），再抄一份只是多一份会走样的副本。

**并发**：v1 串行跑 25 题。并行会让墙钟、限流、成本三件事互相干扰，先把口径跑稳。

### 4.3 判定与报告

- **红绿**：不判。每题只记 ✅/❌ 与原因（超时、run error、答案不符）。
- **报告**：落 `tmp/bench-reports/<ts>/gaia-25-<ts>.{md,json}`（**运行产物不入库**，AGENTS.md；`scripts/bench/` 只留代码与机器真源），包含分数、成本、逐题结果、失败 runId、模型 ref、引擎 commit、数据集 revision、切片清单 hash。口径字段清单见 §4.7.6。
- **基线**：第一次跑出来的分数就是基线。之后只在**同切片、同 user message、同模型 ref** 下比；模型或提示变了，重新建基线，不要把两条线画在一张图上（§4.7.7 第 2 条）。

### 4.4 这一层回答不了什么

- 回答不了「tako 的 PII 有没有漏」——不在题面里；
- 回答不了「看板会不会空」——GAIA 不检查看板；
- 回答不了「围栏有没有被绕」——GAIA 题库没有恶意命令；
- 回答不了「长任务会不会失控」——引擎没有步数上限，制止它的是 harness 的 stop，不是 tako 自己；
- 回答不了「浏览器能力」——`web_fetch` 只取单页正文，不做渲染与交互。

### 4.5 浏览器要不要补：先量后建，不出客户端版本

「CLI agent 没法跑 GAIA」是误判——浏览器是**工具**，不是产品形态。pi 是 CLI，它的 `packages/evals` 也只跑自家行为评测（冒烟 + 文档审计 + 文档提效对照，Docker 里跑 vitest-evals），**不跑 GAIA**；跑公开榜的 agent 是把搜索 API、无头浏览器、文件系统当工具喂给模型，模型用 tool call 调，跟有没有 GUI 客户端无关。

tako 补浏览器最省的一条是**挂 MCP**（doc/33）：Playwright MCP 是 stdio server，写进 `mcp.json`，工具以 `mcp__playwright__*` 进池，内核零改动。代价在运行环境——Node + Chromium 下载、Linux 依赖库、沙箱与租户围栏要放行。

但先别做，按证据决定：

1. 先 fetch-only 跑完 25 题，失败按四类记账：抓取失败 / 需要 JS 渲染 / 附件读不了 / 推理错；
2. 「前两类」占比低（比如 < 1/4）就不补——补了也只是把少数题的分捡回来；
3. 占比高再挂 Playwright MCP 重跑同一批：同模型、同题、只换工具面，差额就是浏览器值多少分。

有一条红线：评测 harness **不许自己替模型打开页面**（包括借用 IDE 的浏览器 MCP）。工具面的差别必须发生在 tako 之内，否则分数就不是 tako 的。

还有一个生产问题的提前记号：系统级 MCP 不围栏、且全租户共享（doc/33）。浏览器会带着 cookie 与登录态，**生产不能这样挂**——评测机可以，租户级挂载要另测围栏是否放行 Chromium。

### 4.6 「让模型自己装浏览器、自己起服务」为什么现在不成立

四条硬约束，都来自现有代码，不是模型能力问题：

| 约束 | 真身 | 后果 |
|---|---|---|
| 工具面在 run 起手固定 | `mcp.json` 在引擎 cwd、每场 run 起手扫盘（doc/33）；文件工具与 bash 的写都限项目根 | 模型改不了自己的工具表；就算改到配置文件，也要下一场 run 才生效 |
| bash 单命令 30 秒 | `sandbox.go:17` 的 `bashTimeout` | 下浏览器、装依赖、跑构建大概率超时；dev server 活不过 30 秒 |
| 网络只放行出站 | `sandbox.go` 的 profile 只有 `network-outbound` | 本地监听端口的服务起不来 |
| 写只到项目根 | profile 的 `file-write*` 只给 root；HOME 缓存不可写，`/private/var`（TMPDIR）只读 | npm / pip / playwright 默认缓存失败；装在 HOME 下的解释器被 PATH 过滤成 `command not found` |

例外：`/opt`、`/Library`、`/usr` 等系统目录可读，所以 Homebrew 的解释器（`/opt/homebrew`）能用；`git clone`、在项目内建 venv 或 `node_modules` 也能做——**前提是缓存环境变量重定向进项目根**。

所以「下载仓库 → 装依赖 → 跑测试」在环境配合下部分可行；「装浏览器」「起本地服务」当前不可行。要补，先改 bash 单命令预算与缓存重定向（引擎能力，另开一拍），不要评测脚本里替模型做。

**三个上限要分开看**（「输出帽太小」这个直觉的准确边界）：

- **下载体积**：落盘，不进 stdout。300MB 的仓库或依赖写进项目根即可，输出帽不参与；挡住它的是 30 秒墙钟。
- **回给模型的输出**：`bashOutputCap` 是 **64,000 字节/流**，且**留尾**（报错与最终结果都在尾部），**全文落盘并回路径**（`.tako/bash/<runId>/<seq>.out`，64MB/流，装不下全文就删文件不报路径）——2026-11-01 doc/58 落地。改前是 32KB/流、**留头丢尾、不留全文**：装依赖、跑测试的错误通常在尾部，模型会看到开头、看不到报错，这比「帽小」更糟。pi 的形状是留尾 50KB / 2000 行，并把全文写进临时文件、回路径（`packages/coding-agent/src/core/tools/truncate.ts`、`packages/coding-agent/src/core/bash-executor.ts`）——tako 现在与它同形。
- **阅读上限**：`read_file` **分页读**（`offset` 1-based + `limit` 行），每页 2000 行 / 50KB，页脚恒带总行数与下一个 `offset`，**没有 200KB 硬拒绝**——2026-11-01 doc/58 落地。改前是无界读超过 200KB 直接拒绝，且只有 head/tail、没有 offset 分页（[filetools.go](file:///Users/yuweikang/dev/ykcode/learn/yk-tako/yk-tako-eino/internal/runengine/filetools.go)）。pi 的 `read` 同样是 `offset`（1-based）+ `limit`（行）分页、2000 行 / 50KB 一页、页尾给下一个 `offset`（`packages/coding-agent/src/core/tools/read.ts`）——与 bash 那条一样是**另一处独立参照**（两家参考实现各自收敛到同一形状，不是某一个人的口味）。

所以 coding agent 该补的是「留尾 + 全文落盘 + 回路径」与「分页读」，不是单纯把 32KB 调大；另外单命令超时要可配置（pi 是每次调用可选 `timeout`，默认不限）。**这两条已落地**：[doc/58](58-读分页与输出留存.md)（read 加 `offset`/`limit` 分页 + bash 留尾落盘，2026-11-01 A1–A8 收口）——剩下的缺口是「单命令超时不可配」，仍挂着。

**所以 C1 是烟雾，不是门禁。** 它证明的是「通用任务链路能跑完、搜索/文件工具能用、多轮不崩」，不是「产品可靠」。

### 4.7 可比性协议：以后谁跑出来的数字，能不能跟我们放一张表

这一节是**口径真源**（2026-11-01 落纸）。没有它，GAIA 的分数只是一张截图：下一个人不知道那 25% 是在什么提示词、什么切片、什么判定规则下跑出来的，也无法判断自己那份数是不是同一个东西。

#### 4.7.1 受试者是谁

**受试者是 runtime，不是模型。** 公开基准的一个分数里混着三样东西：模型能力、harness（脚手架）质量、runtime（工具面、围栏、上下文管理）质量。本轮量的是 **tako 作为 runtime** 的成绩，模型固定为 `deepseek/deepseek-flash`。所以：

- 换了模型分数变了 → 变的是模型那一份，同 runtime / 同切片 / 同口径，**可比**；
- 改了 tako 的工具面分数变了 → 变的是 runtime 那一份，同上，**可比**；
- 换了「从模型输出里抽答案」的规则 → **不可比**（口径本身变了）。所以抽取规则写死在本节。

#### 4.7.2 钉死的变量（动了就要重建基线）

| 变量 | 本轮取值 | 谁定的 |
|---|---|---|
| 模型 ref | `deepseek/deepseek-flash` | 租户 runtime-config |
| 引擎 | 隔离实例（独立 PG 库 `tako_acc` + Redis db 7 + 独立 workspace），**不拿开发库当靶子** | 靶子纪律（`scripts/acceptance/README.md`） |
| user message | **题面 + 固定尾句「最后一行只输出答案，不要解释。」**（口径 B，§4.8 钉死；尾句之外的差异一律不许加） | 用户 2026-11-01 拍板 + §4.8 的口径决定 |
| system prompt | **各臂用各自默认**：tako = `internal/config/model.go` 的编译期常量；dsh = headless profile 的英文 coding agent 提示。两臂都不注入官方 system prompt（run API 没有 per-run system prompt 字段） | 引擎侧；为评测改它就等于改了生产代码（§9 禁止） |
| 切片 | GAIA validation L1 纯文本前 25 题，`slice_hash` 钉死 | `scripts/bench/cases/gaia_l1_25.json` |
| 判定 | GAIA 官方 `question_scorer` 三支 | 见 4.7.4 |
| 答案抽取 | 末个非空行 + 去答句前缀 | 见 4.7.3 |
| 单题墙钟 | 300s；超时由 harness 打 `POST /api/runs/:id/stop` | 引擎没有步数闸（§4.0） |

**为什么官方 system prompt 不注入**：官方 leaderboard 拿提交文件里的 `model_answer` 直接归一化比对，**它自己不解析 `FINAL ANSWER:`**；而 leaderboard 的 `content.py` 原文是 "We **advise** you to use the system prompt…"——是建议，不是要求。官方真正要求的是「提交的 `model_answer` 那个字符串」能被 scorer 判对，所以责任落在 harness 的**抽取规则**上，不在提示词上。

#### 4.7.3 答案抽取规则（本仓自定，官方无此规则）

1. 把该 run 全部 `delta` 帧的 `text` 拼起来——**答案只在 delta 帧里**，`terminal` 帧的 `text` 是空的；
2. 取**最后一个非空行**；
3. 去掉常见答句前缀（`答案：` / `答案是` / `最终答案：` / `最终答案` / `FINAL ANSWER:` / `FINAL ANSWER`），其余原样。

报告必须带 `answerExtraction` 字段写明这段规则。**口径字段不齐的数字不对外**（4.7.7 第 3 条）。

#### 4.7.4 判定规则（照抄官方，不自创）

出处：HF Space `gaia-benchmark/leaderboard` 的 `scorer.py`，sha256 `0d44c07f3046eec521697c22e3eaca8719cc81e422a8eaf32695c5f22bdac6e2`，2026-11-01 抓取（走 hf-mirror；huggingface.co 直连不通）。三支：

| 标准答案形态 | 官方规则 |
|---|---|
| 数字 | 剔掉 `$ % ,` 后按 float 比 |
| 含 `,` 或 `;` | 当列表：切开，**长度必须相等**，逐元素按 float 比，或按「去所有空白 + 小写、**保留标点**」比 |
| 字符串 | 两边「去所有空白 + 删 ASCII 标点 + 小写」后精确比 |

`scripts/bench/judge.py` 是这三支的移植；`--selftest` 把官方源码抓下来当**对照实现**逐条比对（22 条，不一致即非零退出）——口径不是凭记忆自创的。

**已知边界**（不是我们的失误，是官方口径）：官方只删 ASCII 标点，所以全角括号不归一——「（Rockhopper penguin）」与「Rockhopper penguin」判不等；列表支要求**元素个数相等**，所以「a, b, c」与「c, b, a」也判不等。

#### 4.7.5 三臂驱动面与「一行的公约」

三臂（tako / pi / dsh）产出的**每一行**键集必须等于 `scripts/bench/row.schema.json`（`scripts/bench/check-schema.sh` 机器守门，不等即非零退出）。这一行只装三臂都产得出的东西——`traceId`/`spanId` **不在里面**（pi / dsh 没有 OTel；trace 归报告的口径字段）。

**预算单位不同，只并列不折算**：模型调用次数 / 工具调用次数 / token 数 / 美元数这四列在臂之间**不可换算**（pi 的工具面与 tako 不同，一次工具调用花的 token 也不同；`modelCalls` 两臂也不是同一个口径）。报告里四列并排摆着，不合成「总分」，不折算成「等效 token」。

**$ 更是有一道专门的坑**：dsh 的 usage 分缓存命中（命中按 hit 价），tako 的 usage 帧只给总量（一律按 miss 价，是上界）。所以报告给两列：`usd`（各臂本来的账）与 `usdUpperBound`（两臂一律按 miss 价，**唯一可比的一列**）。§4.9.4 是这一坑的实撞。

**缺臂的题不进分母**：本轮 pi 未装（dsh 已装，见 §4.9），报告里单列「本轮未装」，**不折算成 0**（doc/56 §6.5 的配对口径）。

#### 4.7.6 报告必带的口径字段

少一个就不算报告（数字失去可比性）：模型 ref / 切片 hash + 数据集 revision / 引擎 commit / harness 版本 / started_at / 单题预算 / 判定出处 / 答案抽取规则 / 价目表出处 / trace 内容开关状态。

#### 4.7.7 三条禁语

1. **不许把模型基线当 tako 成绩**：mini-swe-agent 跑出来的 SWE-bench 分是「模型 + 它的 harness」，写进报告必须标「不是 tako 分数」；
2. **不许把不同口径的数画在一张图上**：换过抽取规则、换过切片、换过 user message 的两轮数据不并排；
3. **口径不齐的数字不对外**：对外的分数必须带齐 4.7.6 的字段。

#### 4.7.8 本轮必须登记的事实

- **GAIA 题面会发到 CozeLoop 与 Langfuse**：trace 的内容开关开着（dev 排障要看输入输出），题面与答案会随 span 落到 CozeLoop 私有 workspace（api.coze.cn）与 Langfuse Cloud（us.cloud.langfuse.com，2026-11-01 起双发，见 doc/54 §3.1）。这是登记在案的代价，不是漏洞。
- **切片明文不入库**：GAIA 条款（dataset card「not reshare … outside of a gated or private repository」；leaderboard `content.py`「Please do not repost the public dev set」）要求不外发数据集。仓库只入**配方 + `slice_hash`**，明文留本机 gitignore 目录；hash 是**复现装置**不是保密装置——本地照常可读、可调优。
- **公开网上查得到标注答案（2026-11-01 实跑撞到）**：GAIA 的 dev set 被大量转载，模型一次 `web_search`（把题面关键词拼上 "GAIA"）就命中了别人整理的答案集——本轮实测第 1 题就是这样答的。后果有两层：**分数不能当「模型能力上限」读**（这一轮量的是 runtime 的产出形态）；**三臂同条件才可比**（谁都能查答案，所以查不查是各臂自己的行为差异，也算差异本身）。禁语再加一条：**不许把 GAIA dev set 的分说成模型水平**。
- **tako 默认 system prompt 是中文向，官方 scorer 不翻译**：默认提示词是「你是 yk-tako……回答简洁、说中文」（`internal/config/model.go`），官方 scorer 的字符串支只做「去所有空白 + 删 ASCII 标点 + 小写」，**不翻译**。中文长句对英文 gold **必挂**。所以本轮的主要失配是**格式**不是内容——这类失配要单独归因，别混进「模型答错」。

#### 4.7.9 取数面的硬事实（2026-11-01 实跑撞出来的）

这几条不写在协议里，取数的人会**静默取错**——取错时接口不报错，只是帧少了、答案空了、题目记成超时。

| 事实 | 怎么表现 | 取错的后果 |
|---|---|---|
| `eventOffset` 是**排他**游标 | 请求 `eventOffset=N` 返回 **offset > N** 的帧，所以第一页必须传 **0** | 传 1 就漏掉第一帧；后面每一页的起点都错位 |
| **只有 `terminal` 帧是结束信号** | 越过末帧再问，返回空帧 + `status:"generating"` | 拿 status 当结束 → 早停丢答案；等 status 变 done → 每题白等 5 轮空轮询 |
| 日志端点是 **session 级** | 日志在 `GET /api/sessions/:id/log`；**没有** `GET /api/runs/:id/log` | 照计划里的端点写 → 404 → 误判「没日志」 |
| **access token 15 分钟过期** | `expiresIn: 900`；signup 响应里就带 `refreshToken`（refresh 时轮换，旧的不该再用） | 25 题串行跑到第 15 分钟后一路 401 → 轮询等满 300s → 记成**假的「超时」**（本轮第一次跑就挂在第 22 题） |
| 答案只在 **delta** 帧 | `terminal` 帧的 `text` 是空的 | 从 terminal 帧取答案 → 永远空字符串 |

### 4.8 这一轮实跑的数：两轮差 19 题，差的全在「答案长什么样」

2026-11-01 用同一条管道（`scripts/bench/run_bench.py`）跑了两遍同一切片、同一模型、同一隔离引擎、同一判定口径，**只差 user message 末尾有没有那句尾句**：

| 口径 | user message | 通过 | 超时 | 模型调用 | 工具调用 | token | 墙钟 | $ |
|---|---|---|---|---|---|---|---|---|
| A | 题面本身 | **2 / 25** | 2 | 176 | 240 | 3,186,753 | 1338.6s | 0.5088 |
| B | 题面 + 「最后一行只输出答案，不要解释。」 | **21 / 25** | 0 | 152 | 195 | 2,660,933 | 712.8s | 0.4194 |

报告在 `tmp/bench-reports/20261101-172026/`（A）与 `20261101-174314/`（B）；两轮各 25 个 run 的根 span traceId 都从引擎日志对上了（25/25），可在 CozeLoop 按 traceId 查。

**A 的 23 个失败里，21 个是格式，不是内容**——答案就在最后一行，只是被中文散文、引用或括号包着：

| gold | A 的答案行 | 判 |
|---|---|---|
| `Wojciech` | 来源：波兰版《人人都爱雷蒙德》……他在《Magda M.》里饰演的是 **Wojciech** Płaska。 | 人对，格式不对 |
| `green, white` | 颜色按字母序：**green, white** | **gold 原样在里面**，前面多四个字 |
| `The seagull glided peacefully to my chair.` | （拆法：THE + SEAGULL + GLIDED + PEACEFULLY + TO + MY + CHAIR） | gold 拆开写回 |
| `519` | 来源：Baseball-Reference / Sportsmogul 1977 Yankees 打击表（两处数据一致） | 真没答上来 |

**B 剩下的 4 个失败全是内容**：519→525、Guava→Pineapple、inference→element、22→21 年。没有一个是格式问题。

**尾句还让模型少绕路**：墙钟 1338.6s → 712.8s、工具调用 240 → 195、$ 0.509 → 0.419。A 口径里模型爱在答案后面加一段「顺带提醒一个易混点……」，B 口径里它直接给答案。

**所以这一轮量到的是：分数的绝大部分不是模型能力，是 runtime 的输出形态。**

**口径钉死（protocol 决定）**：

1. **user message 钉成 B 口径（题面 + 尾句）**。理由与 §4.7.2 一脉相承：官方 leaderboard 比的是提交文件里 `model_answer` 那个字符串，「从模型输出里抽哪一段算答案」是 harness 的责任；A 口径量的是「中文自由文本恰好落在最后一行且逐字等于英文 gold」这种几乎撞不上的巧合，不是可复现的协议。
2. **但这个数不许说成「模型在 GAIA 上的水平」**：GAIA dev set 被大量转载，模型用 `web_search` 能直接命中标注答案集（§4.7.8）。它是 **tako runtime 在这个口径下的产出形态**。
3. **两个口径的数不并排画在一张图上**（§4.7.7 第 2 条）：A、B 各落一份独立报告，协议字段（`userMessage`）不同，比的时候只比同口径的。
4. **不许拿去跟官方 leaderboard 的分直接比**：官方用它自己的 system prompt 与自己的抽取口径，我们用 tako 默认 system prompt 与自定抽取（§4.7.2 / §4.7.3）。
5. **下一轮想把这两个数变成基线**，走 D1（`--repeat 5` + 归因三态）：单次 21/25 里有多少是「本来就会」有多少是「撞对了」，现在还分不开。

### 4.9 两臂配对：同一个模型、同一批题，只差 harness

2026-11-01 18:13 把同一 25 题、同一个模型（`deepseek-flash`）、同一个搜索后端（DeepSeek 自己的 `/v1/tools/search`）、同一把密钥、同一句尾句、同一套判定与抽取规则，交给两个 harness：

| 臂 | harness | system prompt | 每题的工作面 |
|---|---|---|---|
| **tako** | 本仓引擎 HTTP 面（`POST /api/runs`） | `internal/config/model.go` 的编译期常量（中文向） | 引擎侧独立项目（`POST /api/workspaces`） |
| **dsh** | `dsh --profile headless --json`（npm `@deepseek-ai/dsh@0.2.1-rc.5`） | headless profile 的英文 coding agent 提示 | `tmp/bench-dsh/work/<task_id>`，每题一个干净目录 |

报告在 `tmp/bench-reports/20261101-181303/gaia-25-20261101-181303.{md,json}`；25 个 run 的根 span traceId 25/25 从引擎日志对上（CozeLoop 控制台那一步要人在浏览器里按 traceId 查，脚本侧无公开查询 API）。

| 臂 | 通过 | 超时 | 模型调用 | 工具调用 | token | 墙钟 | $（本臂口径） | $（同口径上界） |
|---|---|---|---|---|---|---|---|---|
| tako | **22 / 25** | 1 | 193 | 263 | 4,187,875 | 1187.0s | 0.6572 | 0.6572 |
| dsh | **20 / 25** | 1 | 280 | 324 | 7,328,913 | 2507.5s | 0.1520 | 1.1416 |

配对：**都过 18；只有 tako 过 4；只有 dsh 过 2；都没过 1**。

#### 4.9.1 这一轮**不能**给两臂排名

同一条 tako 臂、同一口径（协议逐项比对过：同模型、同引擎 commit、同判定器、同抽取规则、同用户消息、同 300s 上限）、隔一小时重跑一遍：

- **总分几乎没动**：21/25 → 22/25，差 1 题。
- **逐题判决翻了 7 题**（28%）：企鹅题、519 题、映射题、Guava 题、Annie Levin 题、inference 题、22 年题。

拿「总分差 1」和「逐题翻 7」这两句摆在一起，正是这一轮最该记住的东西：**总分稳定是巧合，不是可复现性**。3 题由过变不过、4 题由不过变过，几乎对冲掉；换一组题或换一次采样，总分就会动。

**同一量纲下，两臂之间的分歧比同一臂重跑还小**：

| 比较 | 逐题分歧 | 比率 |
|---|---|---|
| 同一臂、隔一小时重跑（tako 17:43 vs 18:13） | 7 / 25 | **28%** |
| 两臂并排（tako vs dsh，同一轮） | 6 / 25 | **24%** |

也就是说：**换 harness 带来的行为差异，比把同一个 harness 重跑一遍还小**。两臂在这一层上根本分不开，任何「谁更强」的说法都没有地基。

7 道翻转题的成因（每题两轮各跑一遍，答案原文见报告 `rows.answer`）：

| 题 | gold | 17:43 的答案 | 18:13 的答案 | 翻在哪 |
|---|---|---|---|---|
| 4b650a35 | Guava | Pineapple | Guava | **读题风格**：0 次工具调用、输入一字不差；题面唯一的分歧点是「第 1 句与第 3 句是否同时生效」——当成并行要求就自相矛盾（→ Pineapple），当成条件+主分支就写 Guava。两种读法都站得住，gold 取后者 |
| 0383a3ee | Rockhopper penguin | `**Rockhopper penguin**` | 企鹅 | **语言**：知识对，用中文答 → 官方 scorer 不翻译，必挂 |
| 7d4a7d1d | 22 | 21 年 | 22 | **语言 + 单位**：一个又错又带单位，一个对 |
| 3f57289b | 519 | 525 | 519 | **路径**：工具调用 4 → 3，答出不同数字，一对一错 |
| 7673d772 | inference | element | inference | **路径**：工具调用 11 → 19，一对一错 |
| 46719c30 | Mapping Human Oriented… | 论文原题名 | A new software agent 'learning' algorithm | **走偏**：这轮用了 web_fetch + bash，末轮答了别的东西 |
| 5188369a | Annie Levin | Annie Levin | （空，300s 撞上限被 stop） | **超时**：墙钟 303s，reason=timeout |

**最硬的一条：引擎把温度锁到 0，provider 仍然给出不同答案。** `internal/config/model.go:58` 写死 `temp := float32(0)`，而 4b650a35 那题**一次工具都没调**，输入逐字节相同，两轮答案一个是 Pineapple（错）一个是 Guava（对）。所以这个不稳定性**不是我们没锁温度，也不是 harness 的锅**——是被测对象（provider 侧的批处理/路由/浮点归约顺序）本身不可复现。评测里凡是「跑一次看结果」的设计都要按这个前提重估。

**顺带一个更贵的发现：同一个臂的「工作量」也在漂。** 两轮之间，同一批 25 题、同一个模型：模型调用 152 → 193（+27%）、工具调用 195 → 263、token 2.66M → 4.19M（**+57%**）、墙钟 712.8s → 1187.0s、成本 $0.42 → $0.66（口径同上，tako 侧是上界）。工具选择也在漂（企鹅题这轮用了 3 次 `web_fetch`，上一轮一次没用）。**所以单轮的成本数字同样不能当稳态报。**

要排名得走 D1（`--repeat 5` + pass^k + 归因三态）。

#### 4.9.2 6 道分歧题：全是格式与超时，没有一道是「不会」

| 题 | gold | tako 的答案 | dsh 的答案 | 谁过 |
|---|---|---|---|---|
| 0383a3ee | Rockhopper penguin | 企鹅 | Rockhopper penguin | dsh |
| 5188369a | Annie Levin | （300s 超时，空答案） | Annie Levin | dsh |
| 3cef3a44 | broccoli, celery, fresh basil, lettuce, sweet potatoes | 同 gold | broccoli, celery, lettuce, sweet potatoes（少一项） | tako |
| 5d0080cb | 0.1777 | 0.1777 | 0.1777 m³（带单位） | tako |
| 7673d772 | inference | inference | （300s 超时，空答案） | tako |
| 7d4a7d1d | 22 | 22 | 22 years（带单位） | tako |

两类失败各有各的形状，**方向正好相反**：

- **tako 输在语言**：中文 system prompt 让它用中文答（「企鹅」），而官方 scorer 的字符串支只做「去空白 + 删 ASCII 标点 + 小写」，**不翻译** → 必挂。这不是模型不会，是提示词方向与 gold 的语言相反。
- **dsh 输在「多说一句」**：英文 coding agent 的习惯是答完补上下文（`22 years`、`0.1777 m³`），官方 scorer 的数字支只剔 `$ % ,` 再按 float 比，**带单位即挂**；列表支还要求**元素个数相等**，少一项就挂。

同一道题（46719c30）两臂答成了**同一个错答案**（“A new software agent ‘learning’ algorithm”），说明两边都被同一份网上材料带偏——GAIA dev set 被转载这件事对两臂同样成立（§4.7.8）。

#### 4.9.3 两臂的工具面差不多，差在 prompt 体积与步数

| 臂 | 工具调用构成 |
|---|---|
| tako | web_fetch 85、web_search 73、read_file 68、bash 35、memory 2（合计 263） |
| dsh | web_fetch 166、web_search 83、bash 66、grep 3、job 5、read 1（合计 324） |

两臂都靠 `web_search` + `web_fetch` + `bash`，工具面接近。差的是 **dsh 的 prompt 更大、步数更多**：280 次模型调用 vs 193，token 7.3M vs 4.2M，墙钟 2507s vs 1187s（**2.1 倍**）。dsh 的系统提示是 coding agent 的长英文提示（实测每轮 prompt 约 11k token），tako 的是一句中文常量（约 6.5k）。

**两臂的 `modelCalls` 不是同一个口径**：tako = usage 帧条数（厂商账单口径），dsh = `step_start` 事件数（harness 步数）。并列看可以，折算成「等效调用次数」不行（§4.7.5）。

**但更要紧的是：这 25 题里，agent 运行时只占一部分。** tako 臂有 **8 题（32%）是「1 次模型调用 + 0 次工具」**——agent 循环一次都没跑，是纯 LLM 题（Guava 题就是其中之一）；dsh 臂 9 题。按被测物拆开：

| 被测的东西 | 25 题里覆盖到多少 |
|---|---|
| LLM 本身（含它 28% 的逐题不可复现，§4.9.1） | 全部 25 题 |
| 系统提示词（harness 的一半） | 全部 25 题 |
| **agent 运行时**（工具面、循环、上下文、超时） | **只有 17 题**，另 8 题一次工具都没调 |

**两臂那 6 处分歧里，只有 1 处落在零工具题上**（`3cef3a44`：两臂都只调 1 次模型、0 次工具，tako 给完整列表、dsh 少一项）——**那处分歧 100% 来自两个 harness 的系统提示词不同**，与运行时无关。

**按「agent 有没有真的跑」分层看**：

| 子集 | 题数 | tako | dsh | 配对（都过／只 tako／只 dsh／都没过） | 两臂分歧 | 同臂重跑翻转 |
|---|---|---|---|---|---|---|
| 零工具（tako 侧判定） | 8 | **8 / 8** | 7 / 8 | 7 ／ 1 ／ 0 ／ 0 | 1 题（12%） | 1 题（12%） |
| 用工具（tako 侧判定） | 17 | **14 / 17** | 13 / 17 | 11 ／ 3 ／ 2 ／ 1 | 5 题（29%） | 6 题（35%） |

- **零工具那 8 题上，两臂就是同一个 LLM**：7 题答案**逐字相同**（逻辑题、大小写题、算术题都在内），唯一分歧是 `3cef3a44` 少说一项——那是系统提示词的差别。（顺带：官方 scorer 的「去空白 + 删 ASCII 标点 + 小写」在这 8 题里放行了至少 3 题——`right`/`Right`、`Maktay Mato Apple`/`Maktay mato apple`、`THE SEA GULL…`/`The seagull…`。）
- **用工具那 17 题上，两臂的 5 处分歧全是「说话方式」与「什么时候收工」**：3 题格式（中文答案、带单位）、2 题超时（**tako 超时 1 题、dsh 超时 1 题，两臂各撞一次 300s 上限**），**没有一题是知识**。而且**分歧率 29% 仍低于同臂重跑在这 17 题上的 35%**。

**结论要说准**：「GAIA 上 tako ≈ dsh」**不是**「我们的运行时跟别人的运行时打平」，而是「在这 25 道题里，能体现运行时差异的地方只有 17 题，而且这 17 题跑的还是同一套通用工具」。更要紧的是：**tako 独有的那些面（子代理、项目隔离、文件落盘、多租户、安全门）在 GAIA 里一道题都没碰到**——GAIA 只有「搜网页 + 读文件 + 跑 bash」。

所以 **GAIA 上就算 tako 拿满分，也证明不了 tako 这个产品比 dsh 强**；它证明的是「同一个 deepseek-flash + 中文提示词 + 一套通用搜索工具，能答对几道公开题」。GAIA 这条线能回答的只有「模型 + 提示词 + 预算」；**tako 的产品能力要去 doc/45 的场景化 eval 测**（K/S/B/C 四组：准备/动作/判定/证据），那里才有项目隔离、落盘、子代理、安全门这些 tako 独有的面。

#### 4.9.4 $ 的两列不能互相比

| 臂 | $（本臂口径） | $（同口径上界） |
|---|---|---|
| tako | 0.6572 | 0.6572 |
| dsh | 0.1520 | 1.1416 |

- **本臂口径**：dsh 的 usage 分缓存命中（本轮 **93% 的 prompt token 是缓存命中**，按 hit 价 $0.003/M）；tako 的 usage 帧只给 `promptTokens` 总量、不分命中，一律按 miss 价 $0.15/M 算——**是上界**。
- **同口径上界**：两臂一律按 miss 价算（把 dsh 的缓存命中也当没命中算）→ **dsh $1.14 比 tako $0.66 更贵**。

「dsh 真便宜 4 倍」的直觉来自把两列并排看——那是口径错，不是结论。**真账单谁更省测不出来**：tako 的缓存拆分引擎没给，tako 那一列的上界有多松、dsh 那一列有多实，都不知道。

#### 4.9.5 这一轮能说与不能说的

**能说**：同一个模型 + 同一批题，**换个 harness 就让 6 题的对错翻转，方向由提示词的语言与「多说一句」的倾向决定**。GAIA 这类「答案逐字比对」的基准，量到的主要是 runtime 的输出形态——这与 §4.8 的结论是同一件事的第二条证据。

**不能说**：

1. **不能说 tako 比 dsh 强（或弱）**——逐题分歧率 24%，比同一臂隔一小时重跑的 28% 还小；两臂在这一层分不开（§4.9.1）；
2. **不能说 dsh 真便宜 4 倍**——$ 的两列口径不同，同口径下反而是 dsh 更贵；
3. **不能说这是模型在 GAIA 上的水平**——GAIA dev set 被大量转载，两臂都可能被网上的答案集带偏（有一道题两臂答成同一个错答案）；
4. **不能说两臂的 trace 可比**——只有 tako 臂有 OTel trace（dsh 没有），traceId 25/25 只覆盖 tako 臂的 25 个 run；
5. **不能说单轮的成本数字是稳态**——同一个臂隔一小时重跑，token +57%、模型调用 +27%、成本 $0.42 → $0.66（§4.9.1）。

**协议决定**：

6. **两臂并排跑不改变「缺臂不进分母」那条**——pi 仍未装，配对分母就是 25；
7. **下一轮 B3 的价值在重复，不在换臂**：pi 臂装上后是三臂并排，但**先做 D1（`--repeat 5`）**——没有重复次数，单次配对数连噪声都分不出来（§4.9.1）；
8. **超时、语言失配、读题风格要分开记账，别混进「不会」**：本轮 7 道翻转里，1 道是撞 300s 上限（timeout）、2 道是答案语言/单位与 gold 不匹配、1 道（Guava 题）是**读题风格与出题人不一致**——那题没有任何知识可不会，两种读法都自洽，只是 gold 取了其中一种。真正「答出不同事实」的只有 2 道。归因三态（`model_baseline` / `harness_format` / `flaky_runtime`）要把这几类分开，其中「读题风格」这一类还得单独立项：GAIA 里有一批**条件句陷阱题**（条件句 + 禁令 + 一个输出词），gold 是出题人的判断而非客观事实，人来判也可能给出另一个答案。

### 4.10 D1：k=5 首轮基线——「能做对一次」与「次次做对」差在哪

2026-11-01 23:37 – 11-02 04:08，同一批 25 题、同一模型、同一隔离引擎（`yktako-d1`，8953）、同一 B 口径（题面 + 尾句「最后一行只输出答案，不要解释。」，§4.8），**每题每臂跑 5 次、串行不并发**（并发会破坏「同一时刻只有一场 run 在同一台引擎上」这个可比性前提）：

```bash
python3 scripts/bench/run_bench.py --cases gaia --arms tako,dsh --repeat 5 \
  --tail "最后一行只输出答案，不要解释。" --write-baseline --engine-log tmp/d1-engine.log
```

报告在 `tmp/bench-reports/20261101-233715/gaia-25-20261101-233715-x5.{md,json}`；125 个 tako run 的根 span traceId 125/125 从引擎日志对上，归因取证经 `traces.py --all` 拉回 125 份事实后 `--rebuild --traces` 回填。

| 臂 | Pass@5 | pass^5 | 未完成轮次 | 模型调用 | 工具调用 | token | 墙钟合计 | $（本臂口径） | $（同口径上界） |
|---|---|---|---|---|---|---|---|---|---|
| tako | **24 / 25** | **20 / 25** | 1 | 605 | 668 | 10,796,281 | 3124.2s | 1.6954 | 1.6954 |
| dsh | **23 / 25** | **14 / 25** | 22 | 1472 | 1570 | 34,480,868 | 13170.6s | 0.5511 | 5.3433 |

配对：**Pass@5 都过 23、只有 tako 过 1、只有 dsh 过 0、都没过 1；pass^5 都过 13、只有 tako 过 7、只有 dsh 过 1、都没过 4。**

**两臂的步数预算（核过代码，不是口径推断）**：tako 有步数闸——`adk.NewChatModelAgent` 的 `MaxIterations=50`（`internal/runengine/worker.go`；本轮单轮最多 27 次模型调用，未触顶）；dsh 没找到步数闸；**真正截断两臂的是每题 300s 墙钟**——dsh 的 22 个未完成轮次都是撞它，不是模型主动停。

**这一轮量到的三件事**：

1. **Pass@5 与 pass^5 的差就是「不稳」的价钱**。tako 24 → 20：4 题 5 次里错 1 次（`3cef3a44` 11110、`42576abe` 11100、`4b6bb5f7` 10011、`5188369a` T0111 其中一次超时）；dsh 23 → 14：9 题不稳，**其中 22 个未完成轮次落在 8 道题上**——dsh 的缺口主要是 300s 内跑不完，不是答错。
2. **同臂重跑的噪声结论没被推翻，只是被量化了**（§4.9.1 的单轮分歧率 28%）：k=5 下 tako 有 4 题、dsh 有 9 题在「过/不过」之间摆动。**所以仍然不给两臂排名**：pass^5 的 13/7/1/4 里，dsh 那 4 道「都没过」有 2 道是 5 轮全超时——那是步数预算与 harness 的差，不是知识差。
3. **归因**：tako `incomplete` 1 / `model_baseline` 4 / `ok` 20；dsh `incomplete` 8 / `model_baseline` 3 / `ok` 14。基线是**本轮自身**（首次真值，§6.1 的「抄真值」），所以没有 `engine_regression` / `model_below_baseline`，退出码 0；以后每轮拿 `scripts/bench/baselines/gaia.json` 对照，红了才拦门禁。

**基线文件**：`scripts/bench/baselines/gaia.json`——只存 `sha256(task_id)[:12]`、`passAtN`、`passK`、逐次判决，**不含题面/答案/题号明文**；键为 `{caseSet, repeat: 5, modelRef, engineCommit, sliceHash, arms}`，模型或口径一变就要重建。

**两个施工期实撞（以后跑之前先看这两条）**：

- **k=5 必须带钉死的尾句**。第一版忘了 `--tail`，跑的是 §4.8 已判死的 A 口径（题面本身）：39 次里只过 5 次、答案全是散文，与 A 轮 2/25 同档；跑到 39/250 时停掉作废。**同一个脚本，少一个参数就是另一个协议的数。**
- **Langfuse 读接口限流 30 次/分钟**（`429 {"limit":30,"retryAfterSeconds":19}`）。125 场全量拉一次就会撞上；`traces.py` 已按 `retryAfterSeconds` 退避重试，勿再用 for 循环裸拉。

---

## 5. C2：SWE-bench 25 题模型基线（不经过 tako）

### 5.1 为什么用 mini-swe-agent

- 它是官方榜单在用的轻量 harness（bash + 文件编辑 + 提交三件套），口径公开、可复现；
- 它把「模型能力」与「我们自己的 agent 实现」隔开——这一层不掺 tako 的 bug，也不给 tako 贴金；
- 25 题切片足以看趋势，成本可控。

### 5.2 在哪里跑

**跑在 Linux 服务器上，不跑在 Mac 开发机上。** Mac 没有 Docker，且 arm64 模拟 x86 镜像会把单题从几分钟拖到更久。两条可行路径：

1. **华为云 Linux 机器**（已有部署链路）：装 Docker，按官方 harness 构建/拉镜像；
2. **云端沙箱**（e2b / 火山云等）：mini-swe-agent 原生支持，按实例计费。

v1 推荐路径 1：机器已经存在，数据不出自己的云。上机前核四件事：**CPU 架构是 x86_64、磁盘 ≥120 GB、Docker 可用（root 或 docker 组）、能拉镜像和下载 HF 数据集**（国内网络先配镜像/代理）。这四件是运维检查，不是代码改动。

### 5.3 配置与口径

```bash
mini-extra swebench \
  --subset verified --split test --slice "0:25" \
  -m <model-ref> -c swebench.yaml \
  -o reports/swebench-verified-25-<date> -w 4
```

- `step_limit: 250` 是官方默认，**不要压到 40**——实测压太低会把「已改好、只差交卷」的实例判 0，整体低估；
- `wall_time_limit_seconds: 1800`；`cost_limit` 按模型设，防止个别实例烧穿；
- 报告标题固定写：`model=<ref> · harness=mini-swe-agent · verified 25 题切片 · 不代表 tako 能力`；
- 结果落 `preds.json` 与每题的轨迹文件，评测用官方 harness 跑 `FAIL_TO_PASS` / `PASS_TO_PASS`。

### 5.4 什么时候跑

- 换基座模型 / 换 provider；
- 大改系统提示里与编码相关的约定（但注意：这条只影响 tako，不影响 C2）；
- 季度体检。

**不接 CI**：25 题要 1–3 小时、要钱、要外部机器，而且分数抖动；把它塞进 CI 只会让人学会忽略红灯。

### 5.5 Docker 能用，但 tako 还没有「用 Docker 的手」

先把三个问题分开：

| 问题 | 答案 | 意味着 |
|---|---|---|
| 评测机能不能跑 Docker？ | **能**（Linux 服务器装 Docker） | C2 立即可跑；G4 有容器底座 |
| tako 能不能把 bash 放进容器？ | **现在不能** | `bash` 没有容器后端；Linux 上无 `sandbox-exec` 直接 fail closed |
| 仓库里有没有 tako 镜像？ | **没有**（无 Dockerfile / compose） | G4 要先写镜像，不是改一行 flag |

**为什么 tako 现在进不了容器**：`bash` 的执行路径是本地进程 + macOS seatbelt（`sandbox.go`）；`filetools` 直接读写工作区目录。Linux 上 `sandboxAvailable()` 为假，`requireSandbox` 就拒绝执行——这是对的，因为它不假装隔离。但把引擎放进一个专用评测容器后，**容器本身就是围栏**，前提变了。

两条架构，二选一：

**A. 引擎在容器外，bash 执行后端换成 `docker exec`（v1 推荐）**

- 容器由评测脚本按实例起好，`/testbed` 绑到宿主目录；tako 的 `-workspace` 指这个宿主目录（文件工具照常读写，看到的是同一批文件）；
- `bash` 不再本地 fork，而是 `docker exec -w /testbed <cid> bash -lc <cmd>`；容器无外网、只挂当前实例；
- 引擎的 PG / Redis / HTTP 全不动，多实例评测只需换 workspace + 容器 id；
- 改动集中在 `bash.go` / `sandbox.go` 的执行后端（新增一个「容器后端」），**不新增模型可见工具**；
- 风险：容器内绝对路径（`/testbed/...`）与宿主工作区路径不一致——模型用相对路径或容器路径都能工作，评测提示里固定工作目录说明即可。

**B. 引擎在容器内（以后整体容器化部署时更顺）**

- 整个 `yktako` 进程和 `/testbed` 同容器；PG / Redis 走外部地址；harness 通过映射端口打 `POST /api/runs`；
- bash 在容器内本地执行，需要显式模式：要么补 Linux 围栏（bwrap），要么 `-sandbox=container`（只在检测到容器时生效、启动日志明示、生产禁用）；
- 简单，但每实例多一份引擎配置、端口与日志管理；评测脚本要管引擎生命周期。

**推荐 A**：它把「隔离」交给容器，把「执行位置」抽成 bash 的一个后端，引擎本身保持现在的单进程形态；B 留到我们真要做「引擎整体容器化部署」时再收。

**G4 的前置因此变成三件可验收的小事**：① 写一个能跑引擎的 Dockerfile；② bash 容器后端跑通一条 `echo`；③ 起一个 SWE-bench 实例容器，让 tako 读到 issue、改文件、`git diff` 拿到 patch。三件都绿了，再谈 25 题。

---

## 6. 从 τ-bench / Claw-Eval 借到 doc/45 的三样东西

这三样不需要新目录、不需要移植环境，只是在 doc/45 的 runner 里加字段和一条判定原则。

### 6.1 pass^k：把「能做对一次」与「次次做对」分开

**本仓库的口径就是 pass^k 本身**（2026-11-01 拍板）：跑 M 次，**M 次全过**才算这个案例通过——不是「N 次里至少 M 次过」。所以 doc/45 的判定字段是：

```yaml
repeat: 5          # 跑 M=5 次
# 判定：passed == M 才算过（= pass^5，严格全过）
```

报告同时给 **Pass@N**（N 次里至少 1 次过，能力天花板）与 **pass^k**（k 次全过，可靠性地板）。两个数差得越大，说明模型越「靠运气」——但**只有 pass^k 参与判红**，Pass@N 只是信息。

B 组还有一层：**模型水平低本身不算回归**。`baseline`（首次实测抄真值）用来把「红」分成两种——`model_baseline`（红但等于基线 = 已知的模型水平，**不拦门禁**）与 `model_below_baseline`（红且低于基线 = 要查，**拦门禁**）。基线只影响归因，不改变判定。C 组也照此输出，但不判红。

**已落地（2026-11-01 夜）**：`run_bench.py --repeat N` 出 Pass@N / pass^N 两列与逐次明细；基线用 `--write-baseline` 落 `scripts/bench/baselines/<题集>.json`，键是 `sha256(task_id)[:12]`——**题面、gold、task_id 明文都不入库**。归因按「判定 + 基线 + trace 取证」三份证据推：

| 归因 | 判据 |
|---|---|
| `incomplete` | 任一轮没跑完（超时 / error） |
| `model_baseline` | 基线该题本就没全过 |
| `engine_regression` | 基线过、本轮挂，且 trace 里工具有「错误：」回执或 span 上的 runId 与本轮对不上（串号） |
| `model_below_baseline` | 基线过、本轮挂，trace 正常（工具回执无错、runId 一致） |
| `unasserted` | 没基线或缺 trace：证据不齐，不下结论 |

取证走 Langfuse（`traces.py`，doc/54 §3.1）；trace 是异步上报的，所以流程是「跑 → 贴 traceId → 拉事实 → `--rebuild` 回填归因」，`--rebuild` 也执行红码规则。**只有 `engine_regression` / `model_below_baseline` 让退出码非零**。

### 6.2 终态断言：读世界，不读模型自述

τ-bench 的判定不看对话里说了什么漂亮话，只看结束后的数据库状态。doc/45 的 K/S 两组已经是这个路子（`db_rows` / `db_no_plaintext` / `db_value_contains` / `file_content`）；补一条成文原则：

> **判定只读任务结束后的世界状态（库、文件、帧），不读模型在回复里的自我汇报。** 模型说「已写入」不算，文件里有没有才算。

### 6.3 安全一票否决

Claw-Eval 把安全当乘法门：命中一条违规，任务分归零。doc/45 加一个场景级开关：

```yaml
safety_gate: true   # 命中 pg 里的明文密钥 / 围栏越界 / 越权访问 → 整场 fail
```

即使 `run_status=complete`、任务看上去做完了，只要安全项命中，这条场景就是红的。PII 那几条场景（doc/45 §1 的 `k-pii-mask` 与 `b-pii-model-input` / `b-pii-fold` / `b-pii-memory`）默认打开。

### 6.4 过程指标：有 span，但先别指望自动报告

doc/54 已经给 recall / compact / 每个工具调用打了 span，能看每一步耗时。但 CozeLoop 目前没有公开的查询 API（search / query / list 都 404），自动报告拿不到 span 数据。v1 的过程指标只用：

- frames 能数出来的：步数、工具调用数、token；
- harness 能测的：墙钟；
- traceId：贴进报告，人想看细节就进 CozeLoop 控制台。

等 CozeLoop 开放查询或我们自建 collector，再把 span 耗时接进报告。

### 6.5 从 pi 借「对照实验协议」，不移植它的代码

pi 的 `packages/evals` 是一套 A/B 评测器，核到的骨架在 `src/plan.ts` / `src/report.ts`：

- **任务计划** = `case × variant × runNumber`；变体是 `without_docs` / `with_docs` 两臂，`runNumber` 控制重复次数；
- **顺序交替**：奇数次先跑 control，偶数次先跑 treatment，降低缓存与时段偏置；
- **配对比较**：同一 `(case, model, runNumber)` 的两臂才算一对；缺一臂进 `blockedPairs`，**不折算成 0 分**，报告单独列；
- **报告字段**：每个 case 集的 `lift`、两臂通过率、flags（`no-lift` / `negative-delta` / `control-saturated` / `treatment-saturated` / `flaky`），以及配对后的 tokens / toolCalls / totalMs / cost 均值差；
- **产物**：每臂 session 快照按 hash 落盘，`protocolDigest` 钉住协议版本；存在 blocked pair 时非零退出。

这套协议与语言无关。**不要移植 `vitest-evals`**：那会把 Node 测试栈拖进 Go + Python 的评测面。只借协议，在 `scripts/eval/`（doc/45）实现一次，`scripts/bench/` 复用。

对 tako 直接能用的三种变体：

| 变体对 | 回答什么问题 |
|---|---|
| `without_skill` / `with_skill` | 技能（doc/52）到底是帮忙还是添乱 |
| `base` / `plus_browser` | 浏览器工具值多少分（§4.5 第 3 步） |
| `prompt_a` / `prompt_b` | 系统提示词、工具描述的改动有没有净收益 |

**Eino 这边没有现成的可 import 的 eval 框架。** 核过三处：`eino` v0.9.21 的包目录只有 adk / callbacks / components / compose / flow / schema / utils，没有 eval；`eino-ext` 顶层是 acp / adk / callbacks / components / devops / libs / skills，`devops` 下只有 `dev` 与 `model`；`eino-examples` 的 devops 只有 debug / visualize。总览页写的「评测能力」属于 Eino DevOps 工具链的叙事，不是包。Eino 真正给的是 callbacks（观测切面），doc/54 已经在用。结论：**评测 runner 自己写，不等上游**。

**与 pass^k 的分工**：pass^k 是同一臂内重复（可靠性地板），A/B 是跨两臂配对（改动有没有净收益），两者正交，可以叠加使用。

---

## 7. 分阶段落地（每步的输入来自上一步）

| 阶段 | 做什么 | 解决了什么 | 验收（第一行必须是打一个跑着的东西） |
|---|---|---|---|
| **G0** | 本文点头 | 先定「测谁、用哪几个、多久跑」 | 用户点头 |
| **G1** | GAIA 25 题 runner + 第一份基线报告 | 有一个**今天就能跑 tako** 的外部标定；同时暴露「没有 fetch 工具」的能力缺口 | 起隔离引擎 → 跑 1 题：`python3 scripts/bench/run_bench.py --cases gaia --arms tako --limit 1` 输出 runId + 提取答案 + 对错；再跑 25 题出报告。**2026-11-01 已完成**：两轮 25 题单臂（A 口径 2/25、B 口径 21/25，§4.8）+ 一轮 25 题**两臂配对**（tako 22/25 vs dsh 20/25，§4.9），报告在 `tmp/bench-reports/`，口径钉死见 §4.8 / §4.9 |
| **G2** | SWE-bench 25 题模型基线（华为云 Linux） | 换模型时有一个**不掺 tako** 的能力参照 | 在 Linux 机器上 `mini-extra swebench-single <id>` 跑通 1 题；再跑 25 题出 `preds.json` + resolved 率 |
| **G3** | 把终态断言 / safety_gate / 报告的 pass^k 列回填 doc/45（**pass^k 口径已在 doc/45 E0 对齐**，本步只搬另两项） | 场景回归的判据从「过/不过」升级为「可靠性 + 安全门」 | **依赖 doc/45 E1–E2 已落地**；同一场景 `repeat:5` 跑出 `3/5` 的报告（判定不过；归因 `model_baseline` 时退出码仍 0） |
| **G4** | tako 级 SWE-bench（gated：服务器 Docker 已具备，缺执行后端） | 真正回答「tako 修真实仓库的能力」 | 先过三件小事：① 引擎 Dockerfile 起得来；② bash 容器后端跑通 `echo`（`docker exec` 打一个跑着的容器）；③ 1 题端到端：issue → run → `git diff` → 官方测试 |
| **G5** | A/B 对照协议（§6.5）：`scripts/eval/` 支持 `base` / `variant` 两臂、重复、配对报告 | 回答「这次改动到底有没有用」，而不是「跑完看一眼」 | 同一批 N 题、两臂各 3 次，变体先设空臂：报告 `eligiblePairs = 3N`、`lift ≈ 0`；再把变体故意退化一次，报告出 `negative-delta` 且非零退出 |

**顺序理由**：

- G1 先做，因为它是唯一零新增基础设施、又能直接打到 tako 的一层；它 concurrently 验证了「外部题 → 我们的 HTTP 面」这条管道，后面 G2 的报告也能共用同一套落盘格式。
- G2 独立于 G1，但需要外部机器；放在 G1 后是因为先把「我们的报告长什么样」定下来，避免两边各写一套。
- G3 必须在 doc/45 的 E1–E2 之后，否则没有 runner 可改；它搬的是**口径之外的方法**（终态断言 / safety_gate / 报告的 pass^k 列），口径本身已在 doc/45 E0 前移对齐。
- G4 卡在 Linux 沙箱 + 容器化，是另一条工程线；不在本方案的 v1 里假装能做完。
- G5 与 G1 并行即可：它不依赖外部数据集，先用 3–5 个自有场景把「计划 → 两臂 → 配对 → 报告」跑通；§4.5 的浏览器实验要靠它才有可比口径。

---

## 8. 成本与节奏（按当前 DeepSeek 价位估算，下单前以实际账单为准）

| 项 | 单次成本 | 墙钟 | 频率 | 谁看 |
|---|---|---|---|---|
| L0 快闸 | 0 | 秒 | 每拍 | 开发者 |
| L1 契约 | 0 | 分钟 | 改契约字节时 | 开发者 |
| L2 场景回归（doc/45 全量） | 约 $1–3 | 10–30 分钟 | 发布前 / 每天 | 开发者 |
| C1 GAIA 25 题 | 约 $1–5 | 20–60 分钟 | 换模型 / 月度 | 开发者 |
| C2 SWE-bench 25 题 | 约 $1–10（DeepSeek 量级） | 1–3 小时 | 换模型 / 季度 | 开发者 |
| G4 tako 级 SWE | 约 $5–50（视模型与重试） | 半天级 | 大版本前 | 开发者 |

注意：C2 的单价跨度来自模型差异（官方榜单 $0.04–2/题）；这里按 DeepSeek 量级估，换贵模型要重新算。

**C1 这一行的实测值（2026-11-01，deepseek-flash）**：题面 + 尾句口径 25 题 = **$0.42 / 墙钟 713s**；题面本身口径 = **$0.51 / 墙钟 1339s**（尾句让模型少绕路，见 §4.8）。价目按官方页、输入 token 一律按**缓存未命中**算（引擎的 usage 帧不区分），是偏贵的上界。

**两臂配对那一轮（2026-11-01，§4.9）**：tako 臂 25 题 = **$0.66 / 墙钟 1187s**；dsh 臂 = **$0.15 / 墙钟 2508s**。这两个 $ **不能互相比**——dsh 的 93% prompt token 是缓存命中（按 hit 价），tako 的 usage 帧不分命中（按 miss 价算，是上界）；同一条尺子（都按 miss 价）下是 dsh $1.14 vs tako $0.66。**真账单谁更省测不出来**，tako 那一侧缺缓存拆分。

---

## 9. v1 明确不做

- **不接 CI**：仓库没有 CI 基建；外部基准又慢又抖，接进去只会制造「习惯性忽略的红灯」。
- **不跑全量**：SWE-bench 500 / LiveCodeBench 1055 / Claw-Eval 300 全量不在 v1；固定小切片足够看趋势。
- **不做多模态 GAIA / Claw-Eval 全量**：多模态题需要附件与视觉链路，先把纯文本链路跑稳。
- **不为评测改 `internal/runengine/`**：评测站在引擎外面看它；发现缺陷另开一拍修引擎，不在评测里开后门（同 doc/45 §5）。
- **不在评测脚本里替模型补能力**：没有 `web_fetch` 就不在 runner 里偷偷抓页面；分数必须反映真实能力。
- **不拿公开分数做营销**：口径（模型 + harness + 切片）不写清的数字不对外。
- **不在 Apple Silicon 本机跑 x86 评测镜像**：模拟层会把时间和结果都拖坏。

---

## 10. 收尾：所以这解决了什么

tako 今天能自动判红的是「接口有没有按约定回」（L0/L1），下一步要补的是「活有没有干完」（L2）；而**「模型有多强」是第三个问题，不能混进前两个**。

这份方案把第三个问题单独拎出来，并承认它的边界：

- **能直接问 tako 的，只有 GAIA**——所以先跑它，25 题、纯文本、只出分数；
- **SWE-bench 是模型基线，不是 tako 成绩**——先用 mini-swe-agent 跑，等容器与 Linux 沙箱落地再让 tako 自己上场；
- **τ-bench / Claw-Eval 不搬环境，只借判定方法**——pass^k、终态断言、安全一票否决进 doc/45，成本低、收益直接；
- **CI 门禁仍然只认确定性断言**——公开基准是体检报告，不是录取通知书。

一句话：**先把「测谁」钉死，再谈跑哪个榜。** 否则分数涨了，产品该漏的还在漏。
