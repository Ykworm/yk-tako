# 19v6-08 · 文件工具族（F 系列）

> **立项（2026-10-11 交审）**：T 系列（19v6-05）延拍。把 fileOps 目录名单（19v6-07 §5）
> 从「lint 对照表上的纸面名字」落成第一方真工具。本文是 F 系列的机制与契约真源；
> 施工台账在 `tmp/19-v6/filetools/`；冲突以本文为准、回头改台账。
>
> **2026-10-20**：文件工具新增 `read_pdf_text`（doc/32 A8-1）。
> `fileEffectToolNames` 那份目录名单不变。`read_pdf_text` 只登记 `firstPartyFileEffects`，不进那份名单。
> **2026-10-20 A8-2**：页码超出、无文本层、加密或损坏、非 PDF，都降级成正常结果或「错误：…」，run 继续。`TAKO_MEDIA_SEND=off` 时这把工具仍可读。PDF 正文里印着的回执行不附图。
> **2026-10-22**：同文件并发写底座落地（`filefp.go`）——`write_file` 的「先读后写」纪律、
> `edit_file` 的现读带过、`write_file`/`edit_file`/`move_file` 的 flock 写锁。成功体与既有错误
> 分支**不变**；新增的拒绝是**工具结果文本**（走既有 `wrapToolErrors` 通道），不是 HTTP 业务码。
> 机制与边界见 [doc/42](../../doc/42-产品形态收窄与共享项目退役.md) §2.4，施工台账 `tmp/文件写入并发/`。
> **2026-11-01（doc/58）**：读工具改成分页——入参换成 `offset?`/`limit?`（**`head`/`tail` 删掉**），
> 一次一页（2000 行 / 50KB / 单行 2000 字符），每次追加一行页脚；「>200KB 且无 head/tail」
> 那条错误分支**撤掉**（大文件照样分页读）。bash 侧同期改成**留尾 + 全文落盘 + 回路径**
> （`.tako/bash/<runId>/<seq>.out`，每流 64,000 字节尾窗 / 64MB 落盘帽、保留 30 天），
> 机制与出处见 [doc/58](../../doc/58-读分页与输出留存.md)。

## §0 立项由来：两个「空转」等这拍转正

背景故事（病灶）：C5-A4 落成了 fileOps 提取器 + 启动 lint（19v6-07 §5），但今天它们都在
对空气工作——`firstPartyFileEffects` 是张空表，lint 拿空表跑恒过；提取器的「注册表声明位」
分支（判定 ①）从没吃到过真声明，因为池内唯一的工具 `get_time` 不碰文件。同时模型手里
没有一只能读写工作区的手：想让它存个文件，它只能把内容喷在回复正文里。

「把这些工具落地」同时解开三件事：

1. **lint 转正**：注册表第一次非空、且每个文件工具都带 `file_effect` 声明 → lint 从
   空转变成真检查（漏声明 = 启动失败）。
2. **提取器转正**：真 tool_calls 带真声明进来 → 折叠时「改过/读过」清单第一次出现真路径，
   C5-A5 的 file-ops 块有东西可钉。
3. **模型长手**：工作区内读写文件成为模型可用的动作，防重建保险丝（19v6-07 §5 的
   WeKnora 战例）有了真实用武之地。

## §1 范围与非目标

**范围内**：目录名单里的名字全部落地为第一方工具（`read_text_file`、`read_media_file`、
`write_file`、`edit_file`、`move_file`、`create_directory`、`list_directory`、
`directory_tree`、`get_file_info`、`search_files`、`read_file`），逐个带 `file_effect`
声明进注册表，受启动 lint 管辖。2026-10-20 起文件工具另增加 `read_pdf_text`。
它不在目录名单里，启动 lint 不拿它做「名单内必须声明」的对照；`firstPartyFileEffects` 给它登记 read，父池在 `read_media_file` 之后装配。

**明确不做**（本节登记，未来用例逼出再立项）：

- **`read_multiple_files` 不发明**——MCP filesystem 服务器的后增工具，不在目录名单内
  （C5-A4 裁：按字面清单实现，不凑数）。
- **内容正则搜索**——`search_files` v1 只做固定子串；正则的 ReDoS 面不自找。
- **符号链接逃逸防护**——v1 只防 `..` 路径逃逸；symlink 指向 workspace 外是已登记盲区
  （dev 单机可接受，生产前随沙箱 loop 一并裁）。
- ~~**真沙箱**~~ → **2026-11-03 已落地**：bash 与租户 MCP 的围栏见 `sandbox.go`
  （macOS seatbelt / Linux bwrap）。本节的路径约束仍然不是安全边界：文件工具本身不做
  沙箱，symlink 逃逸仍是 §6 的挂账。
- **四端点契约零变化**——不加帧 kind（复用 T2 的 `tool_call`/`tool_result`），不动
  19v6-01 任何字节；F 系列全部是「池内容」变化，不是「契约面」变化（同 T2 判据）。

## §2 workspace 与路径约束

所有文件工具只在 **workspace 根**内作业。这不是体验决定，是事故防线：

- **没它会怎样（具体事故）**：模型被诱导执行「把配置写到 `../../../../.env`」——
  无约束 = 引擎进程的密钥文件被覆写或读取，run 还照常 complete（工具成功了）；
  有约束 = 工具返回错误文本、模型换路子或放弃，**run 不失败**（工具错误是结果不是
  run 的死因——机制 = 装配点的 `wrapToolErrors`（eino `WrapToolWithErrorHandler`）把工具
  错误转成字符串结果回注模型，全部第一方工具统一包（tools.go）。**2026-10-11 F1 实读
  源码勘误**：eino v0.9.21 的 ToolsNode 对未包装的工具错误直接上抛、run 随之 error，
  回注并非「内建自动」——原文「ADK 工具环内建语义」表述作废）。
- **形态**：`-workspace <dir>` 旗标（默认 `./workspace`）；启动时 `MkdirAll` + 绝对化
  记录，log 一行打印根路径。工具入参 `path` **一律相对根**（POSIX 风格）；工具内部
  `join(根, path)` 后 `Clean`，结果必须仍以前缀「根 + 分隔符」开头（或等于根，目录工具），
  否则返回错误「路径越出 workspace」。
- **多副本挂账**（与 19v6-03 同列）：文件真身在磁盘 = 跨副本共享状态，多副本部署时
  workspace 必须挂共享盘；dev 单副本无感。

## §3 工具契约表（v1）

通用约定：

- **入参 `path`**：相对 workspace 根；越界 → 错误返回（见 §2）。
- **返回**：纯文本直出（同 `get_time` 约定：字符串输出不套 JSON 引号），可被 curl 一眼验证。
- **错误形态**：返回 error → 装配点的错误回注包装（`wrapToolErrors`，见 §2）转成工具结果
  回注模型，模型自行处理，run 继续（terminal 仍 `complete`）。错误文本一律「错误：…」开头；
  实现面剥掉 eino InferTool 的外壳前缀（`[LocalFunc] failed to invoke tool, …, err=`）再加锚，
  回注文本首句即原因（F1 活体引入，tools.go `toolErrorText`）。
- **上限**：各工具的体积/条数上限是**减灾档**（防一次工具调用灌爆上下文），初值 dev 校准、
  写死在契约里（不另开旗标）。**读工具分页后每页 2000 行 / 50KB / 单行 2000 字符**
  （doc/58 §3.2）——它不再是「无界读 + 超了报错」，而是**一次只给一页**。

| 工具 | 声明 | 入参（JSON） | 返回文本（成功） | 错误分支 |
|---|---|---|---|---|
| `write_file` | write | `path`,`content` | `已写入 <path>（<n> 字节）` | 越界；**没读过就写**（覆盖已存在文件前必须先 `read_file`）；**内容与读到的不一致**（被 bash 或另一个 agent 改过）；**读过但文件已没了**（被删/被移走）；拿不到写锁（30s 内被占） |
| `read_text_file` | read | `path`,`offset?`(起始行，1-based，默认 1),`limit?`(本次最多读多少行，默认 2000、上限 2000) | 文件文本原文（纯文本直出）+ 空行 + 一行页脚（doc/58 §3.2 三态：还有更多 / 被 50KB 截断 / 读完了） | 不存在；是目录；`offset` 非正整数；`limit` 非正整数或 >2000；`offset` 超出文件末尾 |
| `edit_file` | edit | `path`,`old_text`,`new_text`,`replace_all?`(默认 false) | `已编辑 <path>（替换 n 处）` | 越界；`old_text` 找不到；多处出现且未置 `replace_all`；拿不到写锁 |
| `read_file` | read | 同 `read_text_file` | 同 `read_text_file` | 同 |
| `move_file` | edit | `path`(源),`destination` | `已移动 <path> → <destination>` | 越界（任一端）；目标已存在（不覆盖） |
| `create_directory` | write | `path` | `已建目录 <path>`（已存在 = 幂等成功，返回「已存在」） | 越界 |
| `list_directory` | read | `path` | 每行一个条目，目录以 `/` 结尾，按名排序，不含 `.`/`..`；空目录返回 `（空目录）` | 越界；不是目录 |
| `directory_tree` | read | `path`,`depth?`(默认 3，上限 5) | 缩进树（两空格一级），目录以 `/` 结尾；超深处标 `…`；节点上限 500、超出标 `（已截断）` | 越界；不是目录 |
| `get_file_info` | read | `path` | 四行：`size: <n> 字节` / `modified: <RFC3339>` / `mode: <-rw-r--r-->` / `type: 文件\|目录` | 越界；不存在 |
| `search_files` | read | `path`(起始目录),`query`(固定子串),`file_glob?`(如 `*.go`) | 命中行 `relpath:行号:行内容`，上限 50 条、超出标 `（更多命中已省略）`；单文件 >1MB 跳过 | 越界；不是目录 |
| `read_media_file` | read | `path` | 单行 `[media path=… mime=… size=…B sha=…；已按路径读取。媒体字节不会写入对话记录，请勿重复调用]`（doc/32 A2/A3-2） | 越界；不存在；>5MB（提示太大） |
| `read_pdf_text` | read | `path`,`page_from?`,`page_to?` | 页眉 `[pdf path=… pages=N；第 a-b 页；文本层抽取]`，随后 `--- page N ---` 与该页文本。默认第 1-3 页，单次最多 10 页、16KB。扫描件与页码超出是正常结果 | 越界；不存在；不是 `.pdf`；不是普通文件；>5MB；加密或损坏 |

语义裁定：

- **`write_file` 覆盖语义**：文件已存在 → 照写覆盖（返回行带「（覆盖）」标记）。MCP 原版
  靠提示词劝谨慎，v1 用标记代替劝告——模型在工具结果里看得见自己覆盖了什么。
- **`write_file` 的「先读后写」纪律（2026-10-22 落地）**：覆盖**已存在**的文件前必须先
  `read_file` 读它。**没读过 → 拒**；读过但文件在此期间被改过（bash、另一个 agent）→ **拒**，
  并在错误文案里引导「先 read_file 重读；要做局部修改请用 edit_file」；读过但文件被删/被移走
  → 拒（别重建）。理由：`write_file` 是**整份覆盖**语义，凭记忆整份回写会把读之后的改动**静默**
  抹回旧版（两道 run 都报「成功」，没有一方收到错误）。这与 Kimi 的 "Read before overwriting an
  existing file" 同款，但 Kimi 那条只是**工具描述里的一句话**（prompt 级），tako 做成**程序级**。
  判据 = 从磁盘现算的 `sha256`（内容级；>64MB 降级为 mtime+size 并显式标注弱判据）。
  底座见 [doc/42](../../doc/42-产品形态收窄与共享项目退役.md) §2.4 与 `tmp/文件写入并发/`。
- **`edit_file` = 精确串替换**（不做 MCP 的 pattern-matching 多编辑阵）：`replace_all=false`
  时原文必须**恰好一处**命中（0 处或 >1 处都报错，>1 处错误文本带出现次数）——把
  「编辑哪一处」的决定权留在模型手里，程序不做模糊匹配。
- **`edit_file` 是局部修改的首选**：它在**编辑那一刻现读**磁盘、写回时用的也是它 → 别的写者
  在你读之后落盘的改动会被**原样带过去**（不会丢）。所以「改一处」用 `edit_file`、「整份替换」
  用 `write_file`，两者语义不同、判据不同（前者看锚点、后者看整份指纹）。
- **部署前提（2026-10-22 落地时写死）**：这套文件级防护的地基是**多副本 + 同一块共享盘**，
  且**那块盘要支持跨机 POSIX 锁**（CephFS ✅／NFSv4 ✅／NFSv3 ⚠️／块存储多机挂载 ❌）。
  ① 多副本的 `-workspace` 必须指向同一块盘（引擎启动时会警告，`main.go`），否则文件根本不在一处；
  ② 锁跨机是否成立取决于存储——本地盘与单副本下 flock 只在本机有效（跨机那几毫秒窗口靠写前比对兜）；
  ③ **NFS 的崩溃恢复有 lease 延迟**（默认 ~30 秒）：持锁进程死后，别的副本要等 lease 过期才拿得到锁。
  ④ **锁文件用桶锁（256 桶）、不清理**（结论，2026-10-22）：锁键 = `sha256(路径)` 首字节 % 256 →
  锁文件数**恒 ≤ 256**，锁目录永远不大。**为什么不用「一路径一锁文件」**：一个项目写几千个文件就会在锁目录
  堆几千个空文件（ext4 的大目录会退化、e2fsck 会告警），而**清理又不能随便做**——unlink 一个正被持有的锁文件，
  后来者会 open 到**新 inode** → 两边同时以为自己独占了 → 互斥失效；要清只能在**租户删除**时连目录清，那又太迟。
  桶锁把两个问题一起消失。代价：不同路径可能撞同一桶（1/256）→ 假串行，无正确性问题。
- **同文件写锁（2026-10-22 落地）**：`write_file`／`edit_file`／`move_file` 的写路径取
  **flock 排它锁**（粒度=文件，锁文件在 `tenants/<tid>/.tako/locks/<sha16(绝对路径)>.lock`）。
  同机跨进程、跨副本都串；跨机看共享盘是否支持 POSIX 锁（CephFS / NFSv4 支持，NFS 崩溃后要等
  lease 过期 ~30s）。**比对在锁内做**——锁让「检查—写」成为原子的一段，否则检查与写之间有缝。
  flock 是 **advisory** 的：bash（`rm`／`sed -i`／`>` 重定向）绕过它 → 不守规矩的写者由
  人工审批闸兜（[doc/41](../../doc/41-任务看板与人工审批.md)）。
- **`read_file` 与 `read_text_file` 同体**：OpenHands / Open Interpreter 系的惯用名
  （19v6-07 §5 出处），同参同行为，仅名字与描述不同。两个都落地的原因：目录名单照单
  全收，别名让两系生态的 prompt/习惯在 tako 上直接可跑。
- **读工具改成「分页 + 页脚」（doc/58 §3 落地，2026-11-01）**：
  - **入参只剩 `path`/`offset?`/`limit?`**——`head`/`tail` **删掉**（dsh 与 pi 各自独立同形：
    read 只有这三个参数）。一套截读方言比两套好：模型不必在「前 N 行／后 N 行／第几行起」
    之间做选择题，也没有「两个参数打架」的组合要判。**代价**：读尾部要两跳（先拿总行数再
    `offset` 跳过去）；**缓解**：页脚恒带总行数。
  - **一次一页**：2000 行 / 50KB / 单行 2000 字符，先到先算。**超长单行不切半行**——按字符
    截到帽内、行尾标 `…（本行超长已截断）`。
  - **页脚恒在**（三态，字节定死）：`（已显示第 A-B 行，共 N 行；继续读用 offset=C）`／
    `（已显示第 A-B 行，共 N 行；本次被 50KB 上限截断，继续读用 offset=C）`／
    `（已读到文件末尾，共 N 行）`。**为什么「读完了」也要带总行数**：截读方言只剩一套之后，
    总行数是模型唯一的定位坐标。
  - **大文件不再报错**：改前「>200KB 且无 head/tail」那条错误分支**已撤**——超 200KB 的文件
    照样分页读，读得到中间。
  - **指纹不变**：`write_file` 的「先读后写」判据仍是**整文件** sha256（流式算的），只读一页
    不影响冲突检测。
- **`read_media_file` 的回执与发送份是两件事**：回执（落库、payload、帧）永远是上面的
  占位行；A3-2 在下一枪的模型输入副本里把回执换成 tool 角色的 image part，模型因此能看见图。
  工具描述与回执里的「媒体字节不会写入对话记录」说的是落库纪律，不是说模型看不到图。
  同一 run 里，用户随消息带来的图只附在第一次成功回复之前的那一枪；模型要再看，调
  `read_media_file`，下一枪的回执发送份会把图附上。这道闸记在 `foldBox` 上，续跑新建容器会再发一次。
  发送份只在两把闸都开时才换成图：目录里这条模型标了 `vision`，并且进程的 `TAKO_MEDIA_SEND` 不是 `off`。
  关闸或模型不收图时，模型看到的仍是占位。细节见 doc/32 A3。
- **`read_pdf_text` 读的是文本层，不是图片回执**。工具结果写入 runlog 和 `run_model_messages`，按页和 16KB 封顶。它不产生「已按路径读取」回执，发送侧也不会因为它被调用就把图附上。PDF 正文里即使印着一行完整的媒体回执，也不附图：还原 tool 消息要同时满足两件事，ToolCallID 在最后一条 user 之后的 assistant 消息里对应 `read_media_file`，并且该行带「已按路径读取」。映射缺失按不还原处理。
- **`head`/`tail` 已删**（doc/58 §3，2026-11-01）：读工具的截读方言只剩 `offset`/`limit` 一套，
  「同时给出 head 与 tail → 错误」那条分支随它们一起消失。
- **`move_file` 的 `destination` 不进 fileOps 清单**：提取器 `pathsFromArgs` 只收 `path`
  字段（C5-A4 形状），destination 是登记盲区（§6 挂账）——防重建保险丝关心「内容写过
  哪些路径」，移动的目的地暂不入账。

## §4 装配与登记

- **每拍两个登记点**：工具进 `NewWorker` 的 `ToolsConfig.Tools` 池（ADK 声明面 + 执行面），
  `firstPartyFileEffects`（19v6-07 §5 声明位 v1 载体）同步加 `名字 → 声明值` 行。
- **lint 转正叙事**：F1 起注册表非空且全部带声明 → `lintFileEffects` 第一次「真的检查」；
  此后任何文件工具漏声明 = 启动失败。名单（`fileEffectToolNames`）与第一方名单 1:1 重合
  是预期态——lint 同时罩住未来的外来 MCP filesystem 服务器（挂进来不声明即拦）。
- **工具描述面**：`description` 写给模型看——说清 workspace 相对路径约定、覆盖语义、
  体积上限。系统 prompt 不动（同 T2：描述自足，不污染 Instruction）。
- **子代理继承**：池内工具对子代理规格的白名单/继承机制天然可见（19v6-05 §4 的工具面四态），
  F 系列零额外工作。**2026-10-16 起多一步**（`doc/26` §7 登记的长期维护点）：新工具进父池会让
  researcher 的有效名单变长（它走 `exclude_tools` 排除路，父池加什么就默认到达什么）⇒
  `agents_test.go` 的 `TestShippedAgentSpecToolGuards` 红一次，红的时候当场决定这把给不给 researcher
  （给 ⇒ 改金样；不给 ⇒ 往 `exclude_tools` 加一行、金样不动），并在 `agent tools researcher:` 日志行里复核。
  这也是「新工具必须登记 `file_effect`（显式 `none` 也算）」的第二个理由：金样的名字宇宙派生自
  `firstPartyFileEffects` 的键 ∪ `{get_time}`，不进注册表的新工具不在它的视野里。

## §5 与 fileOps / C5 的咬合

- **提取器 ① 分支转正**：真 `write_file`/`edit_file`/… 调用进来，声明位分支第一次吃到
  真声明 → 折叠时「改过/读过」清单出现真路径。
- **e2e 活体**：C5-A5（钉块）落地后，含文件操作的会话压出 seq0 摘要、其中出现
  `tako:file-ops v1` 块 = 全链活体。F12 收口时 C5-A5 未落地 → 抽验挂账到 A5 之后
  （2026-10-11 台账注明；A5 转正拍跑此 e2e 时窗口取 ≥3000——600 窗是 A3 实测 + F12
  复现的退化窗：budget 帽下拒收耗尽走干净 A，无 seq0 可查）。
- **序列裁定建议（交审拍板）**：**C5-A5/A6 先收口、F1 随后**——A5 的活体验收前提是「池内
  无文件工具 → 无块」，先收口验收面干净，F1 随即成为 fileOps 的正式转正拍（一块验）。
  若用户要求 F1 先行：A5 的活体验收仍成立（C2 话本不调文件工具 → 无块），只是
  「转正拍」叙事稀释，两序不冲突。

## §6 挂账

- `move_file.destination` 进提取清单（随「提取器收参数面扩展」用例）。
- symlink 逃逸（沙箱 loop 已收口 bash/MCP；文件工具这条仍挂账）。
- `search_files` 正则/大小写开关（用例逼出）。
- 19v6-07 §5 数字与出处勘误 **已于 2026-10-11（F12）全部落定**：两处「12 个名字」→ 11
  （F 系列立项当日提前清账）；出处注记按升级证据整段重写（11 名 = npm @modelcontextprotocol/
  server-filesystem@1.0.0 的 dist/server.js 里 14 个 registerTool 中的 11 个；README 列 13
  系文档滞后、不作依据；read_file = 该服务器 read_text_file 的 deprecated 前身，非
  OpenHands / Open Interpreter 惯用名——旧论断「OpenHands 有 read_file 惯例」已被升级证据
  推翻）。依据 = 2026-10-11 实读 dist/server.js（14 名单）、package.json、README（13 名单）
  与 hermes-agent tool_dispatch_helpers.py:51-52。
