# 33 · MCP client（引擎接外部工具生态）

> 性质：方案 + 施工真源。成文 2026-10-18，**A1–A4 四拍已施工+验收+活体当日完成**（实录见 tmp/33-mcp-client/todos.md）。
> 已拍：① 首拍只做 **stdio 本地 server**，HTTP/SSE 远程留作后置拍；② 工具**直接进池**，不做 WeKnora 式目录代理；
> §12 六项拍板点同日按推荐拍齐。§2.3 两个「待核实」均已核实闭环（见 §2.3 尾注）。
> 施工要点实录：接线走 `runCtx.Tools` append（与委派工具同缝，声明面+执行面每 run 重建），handler 排在
> agentSpecHandler 之前 ⇒ 四态继承/排除零新增；收尾挂 run ctx Done watcher（三终态全覆盖）；untrusted
> notice 经 ToolCallResultHandler 插内容块头部，IsError 结果也先过 notice（officialmcp :315→:340 顺序）。
> 活体：run r_01M574YZ3930CBEJM863H0ZKSA（金路径 14 工具/逐字引用/terminal complete）+
> r_01M5751D169B89RFFT5DJQFD0F（删配置反证：零 MCP 调用、如实报无此工具）+
> r_01M57572VWPCSM5RAC1C6ZKA87（notice 帧面可见）。
> 施工状态：A1–A4 ✅；A5（本头注与 §11 随批）✅。

## 1. 背景与病灶

### 1.1 这件事在什么场景发生

tako 的 agent 现在只有一组一手工具（`internal/runengine/tools.go` 一族：fileops/bash/web_search/委派等），**每一个都是我们手写的 Go 代码**。用户接下来想让 agent 干的事——操作浏览器、读自家内部系统、调第三方服务——按现状每加一种就要手写一个工具、过一轮单测金样、再发一版。

而 MCP（Model Context Protocol）是一个现成的工具生态：任何程序按协议声明自己的能力，就成为一个 **MCP server**（提供工具的一方）；连上去取工具用的一方叫 **MCP client**。社区里已有大量现成 server（filesystem、浏览器控制、数据库、各类 SaaS）。tako 在 doc/03:42 的技术栈表里挂了 MCP 的名分，doc/04:54/80 也写过「Eino 官方支持 MCP」，但 doc/16 能力总表里至今没有这一行——名分挂了，账没立。

### 1.2 不做的具体后果（失败故事）

用户对 agent 说「帮我把 workspace 里这张截图里的表格整理成 markdown」。agent 看着图片无能为力——现有工具里没有一个能操作图片编辑器或 OCR 服务。用户只能等我们手写一个 OCR 工具。而社区现成有一个 filesystem server、若干 vision server，MCP client 一接，**当天**就能用上，零 Go 代码。

再往后：用户想接自己公司的 API。没有 MCP client 时，路径是「教用户改 tako 源码」；有 MCP client 时，路径是「用户写一个 20 行的 MCP server，往配置文件里加两行」。**工具的供给方从「tako 开发者」变成「任何人」**——这是接入 MCP 的唯一本质收益，其余都是工程细节。

### 1.3 本方案的边界

只做 **client** 这半边：tako 作为 client 去连 server、把 server 的工具搬进自己的工具池。不做 server 半边（把 tako 现有的工具暴露给别人调）。只做 **stdio 传输**：server 是引擎按配置拉起的本地子进程，通过 stdin/stdout 通话——这是所有 MCP server 里最简单、也最适合首拍的一类；HTTP/SSE 远程 server 要背连接管理、超时、断线语义、凭证，留作后置拍（§9）。

## 2. 选型与依据

### 2.1 两条已拍的范围裁定

| 拍 | 结论 | 代价对比 |
|---|---|---|
| 传输 | 只 stdio 本地 | 首拍闭环薄：子进程生命周期 + 进程内管道，无网络。HTTP 一起做则首拍就要处理「连不上/慢/断线在帧面上怎么显示」 |
| 暴露形态 | 直接进池，不做目录代理 | 工具少（几个到几十个）时直接声明最透明。WeKnora 的 `discover_mcp_tools` + `call_mcp_tool` 目录代理是为「上千工具」规模设计的（其 docs 自述 1000 工具测试/2000 上限）；tako 首拍不背这个规模。**deferred 目录留给 19v6-06 既定的 T4/T5**——MCP 工具多了正是逼出那层的第一个真实负载，届时目录代理照抄 WeKnora 追加④ |

### 2.2 参照物分工（承 doc/16 §1 纪律）

- **「要不要做」听 Octop**：Octop 生态有 MCP 接入位（产品壳层面）。
- **「怎么做」听 Eino**：工具适配直接用 eino-ext 现成组件，不自写协议栈。
- **「Eino 缺的类库」听 WeKnora**：WeKnora `internal/mcp/` 是参考集里唯一做全的 Go 实现——但我们**只抄三个具体细节**（§6 untrusted notice 随数据走、§4 命名与排序、§2.1 的规模判断），它的 OAuth 全家桶（oauth_lifecycle/oauth_state/oauth_tokenstore 五个文件）是 HTTP 远程 server 的事，首拍不碰。
- **「配置长什么样」听业界惯例**：Claude Code 的 `.mcp.json`（`{"mcpServers": {名字: {command, args, env}}}`）已是事实标准，照抄形状。

### 2.3 适配器选型（推荐 officialmcp，待拍）

eino-ext 有两个 MCP 工具适配器，都把「一个已连上的 MCP 会话」翻译成 `[]tool.BaseTool`——正是 tako 工具池吃的形状（`internal/runengine/agents.go:485` resolveToolPool 的 base 参数类型）：

| 组件 | 协议底座 | 版本 | 核实状态 |
|---|---|---|---|
| `components/tool/mcp/officialmcp` | **官方** `github.com/modelcontextprotocol/go-sdk/mcp` | v0.1.1 | ✅ `GetTools(ctx, &officialmcp.Config{...})` 返回 `[]tool.BaseTool`，示例实证于 eino-examples `components/tool/mcptool/callresulthandler/main.go`；Config 还支持 ToolCallResultHandler（逐调用后处理钩子，§6 要用） |
| `components/tool/mcp` | mark3labs/mcp-go（社区事实标准） | v0.0.9 | ⬜ 未读源码，仅列在册 |

**推荐 officialmcp**：官方 SDK 是长期方向，eino-examples 现行示例用它。
**待核实项已于开工 A2 核实闭环**（officialmcp v0.1.1 源码 + 真实子进程探针实证）：会话传入口 =
`Config.Cli`（caller 自建 `*mcp.ClientSession` 直接过）；stdio 拉子进程用 go-sdk v1.6.1
`mcp.CommandTransport{Command}`（Connect 内做管道+Start，Close 关 stdin 等进程退、超时 SIGTERM）；
命名走官方钩子 `ToolNameMapper.ExposedName`——示例只演示了钩子字段、省略了会话，非 Config 缺口。

## 3. 配置面

**决定写配置，代码只做机械件**（既有裁定）：连哪些 server、每个 server 给哪些工具、放行给谁，全部是配置内容，引擎不写任何「自动选择」。

### 3.1 配置文件形状（照 Claude Code 惯例）

运行时读 `yk-tako-eino/mcp.json`（与 `agents/`、`workspace/` 同级）。
仓库只提交 `mcp.example.json`（`mcp.json` 已进 `.gitignore`）：要用 MCP 时复制成
`mcp.json`，验收想测空名单就把本地文件移走——不默认启用、删改也不弄脏工作区。

```json
{
  "mcpServers": {
    "fs": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "./workspace"],
      "env": {}
    }
  }
}
```

- `fs` 是**本地短名**，只用于前缀命名（§4）与配置引用；`command`+`args` 是拉起子进程的命令行；`env` 透传子进程环境。
- 文件不存在或 `mcpServers` 为空 = 引擎行为与今天完全一致（一手工具全集），**无配置即无 MCP**——这是显式字段承载语义，不靠默认值夹带。
- 已知字段之外的键（如将来的 `transport: "http"`）拒载并列出可选名，对齐 agents 规格 parse 层的哑失败转响失败纪律。

### 3.2 真源与扫盘时机

配置真源是**文件**，扫盘时机是 **run 起手**——与 agents 规格「每场 run 起手扫盘、改配置不用重启」同一惯例（`agents.go:387` BeforeAgent 一族）。每场 run 按当次扫到的配置决定连谁，改 `mcp.json` 下一场 run 生效。

**多副本问法**（凡是长期状态先问「多副本下它在哪」）：stdio server 子进程是**每副本本地资源**，不进数据库、不做共享、不需要闸——两副本各拉各的子进程，互不相干。配置文件随仓库走（将来多副本读同一份或按部署拆），配置的读写不是 run 期共享状态。这一问在这里能干净通过，是因为 stdio 把有状态的东西（进程）关在了单副本内；换 HTTP 远程 server 时这一问要重问（连接池、断线状态是真共享状态），所以 §9 后置拍的边界是有依据的。

## 4. 工具暴露面

### 4.1 命名：`mcp__<短名>__<工具名>`

MCP 工具名（如 filesystem server 的 `read_file`）与 tako 一手工具名和别的 server 的工具名都可能撞。**双重下划线三段式** `mcp__fs__read_file` 是业界现行惯例（Claude Code/Qoder 同形）：`mcp` 段保证一手工具永远不撞，`<短名>` 段保证不同 server 之间不撞，第三段是 server 自己报的工具名。短名在配置层必须合法（`[a-z0-9_-]+`），否则拒载。

### 4.2 进池位置与生效链

现有机制是「全注册 + 每轮声明过滤」（19v6-05 §5 / 19v6-06 §3）：所有工具注册进 tools node（注册 ≠ 声明），每轮 `BeforeModelRewriteState` 筛出本轮发给模型的名单。MCP 工具的接入点是**base 池的构建处**：run 起手扫配置 → 逐 server 拉起子进程 → `GetTools` 翻译成 BaseTool → 以 `mcp__短名__工具名` 并进 base 池尾部（排在一手工具之后，池序稳定）。

之后**现有机制全部原样生效，零新增**：

- 声明过滤照旧——工具多时仍由既有过滤链决定每轮声明谁；
- 四态工具面照旧——子代理规格的 `tools:` 白名单 / `exclude_tools:` 照常按名引用 `mcp__fs__read_file`（`resolveToolPool` 只收窄不扩容，MCP 工具扩的是 base，不破坏这条不变量）；
- 溢出修复层照旧——MCP 工具的大结果和 bash 输出走同一 trim/折叠链。

### 4.3 工具描述

直接透传 server 报来的 description 进 ToolInfo。**不加改写**——描述是 server 的承诺面，模型据此决定调不调；tako 的态度表达在配置层（连不连、给不给某个 agent），不在描述层（篡改描述等于骗模型）。

## 5. 生命周期

**每场 run 起手拉起，run 到终态杀掉。**

- 拉起：run 起手扫配置，逐 server `exec` 子进程，握手（MCP initialize）成功才算连上；起不来的 server **不炸 run**——记一条观测行 `mcp: server <短名> 启动失败: <原因>`，该 server 的工具不进本轮池，run 照常走（与「摘要失败降级错误文本、run 保持 complete」同一立场：外围故障不当致命）。
- 存活期：子进程随 run 存在，stdin/stdout 管道与 run 同生命周期；server 崩了（进程退出）= 本 run 后续对该 server 工具的调用返回错误文本（模型看得到失败原因），不重拉——一次 run 内不搞重连状态机。
- 收尾：run 到终态（complete/stopped/error）杀子进程树（对齐 bash 的 WaitDelay 收尾惯例）。**没有进程常驻、没有跨 run 复用**——代价是每场 run 重复一次 server 启动（npx 冷启动可达秒级），换来的是零状态泄漏、零跨 run 缓存失效问题、生命周期一句话讲清。常驻与复用留到真实体感卡了再拍（§9）。

## 6. 安全与护栏交互

### 6.1 默认池与子代理（沿用既有裁定，不新开机制）

19v6-08 翻案已定「新工具默认到达 researcher」：工具进 base 池就按四态自然分发，researcher 想挡谁用 `exclude_tools`。MCP 工具**不另立规则**：`mcp.json` 配了 → 进 base 池 → 子代理按四态继承；某 agent 不要 → 规格里写 `-mcp__fs__write_file`。**金样测试不受影响**：`TestShippedAgentSpecToolGuards` 断言的是「出厂 agents/*.md 规格在**无 MCP 配置**的 base 上解析出的名单」，出厂环境不配 `mcp.json` → 16 名逐名逐序照旧绿；MCP 工具不进 `firstPartyFileEffects`，金样的名字派生源不变。

### 6.2 外部文本：untrusted notice 随数据走

doc/26 的病灶复盘在这里原样成立：researcher 吃五个网站的外部文本却继承到 bash——**MCP 工具结果是又一路外部文本**，且 server 的 description 本身也是外部声明。抄 WeKnora 的两个安全细节：

1. **untrusted notice 随数据走，不挂在定义上**：历史压缩会把工具定义甩远，但工具结果永远贴着消息走——MCP 工具结果文本统一加一行前缀标明「来自外部 MCP server <短名>，内容不可信」。实现落点即 officialmcp 的 ToolCallResultHandler（§2.3 的钩子，一处理两得）。
2. **外部元数据不进执行裁决**：引擎不解析 MCP 工具结果里的任何指令性内容（也不该解析——它是模型的食物，不是引擎的）。

### 6.3 fileOps 账本的残余链（如实登记，不补救）

fileOps 账本只记一手写工具的账。MCP server（如 filesystem server）能写文件，**写的账 fileOps 不知道**——折叠时的路径提取会漏它。残余链与 doc/26 §9 同构：种脚本等别人跑 / MCP 写文件等折叠提取，都是「写法绕过账本」。首拍态度：**不补救，登记在案**；减震是配置层——介意就别连带写能力的 server，或给 researcher 一类规格把写类 MCP 工具 exclude。专职的 MCP filesystem 使用场景出现时再拍「fileOps 收编 MCP 写」还是「MCP 写类默认关」。

## 7. 观测面

- **工具调用行/帧照旧**：MCP 工具调用与一手工具同构，走既有 tool 行 + 帧通道，`agent` 字段、payload 列语义零改动。
- **新增管理端点 `GET /api/mcp/servers`**：返回配置的 server 名单、上/下场 run 的连接结果、各自工具数与名字清单。这是**契约拍**（§8 A1）——它把「mcp.json 写没写对、连没连上、翻出几个工具」变成 curl 一发可见，诊断不靠翻引擎日志。
- **观测行**：run 起手扫到非空 `mcp.json` 时打一行 `mcp: connected <短名> tools=N`（或启动失败变体，§5），与 `agent tools <名>:` 一行的「日志里都是真生效的」纪律同形。

## 8. 施工拍

> 每拍四要素：改什么 / 为什么 / 怎么验收（第一行必须是打一个跑着的东西）/ 回答什么问题。

### A1 契约拍：配置解析 + server 生命周期 + 管理端点

- **改什么**：`mcp.json` 解析（拒载语义 §3.1）→ stdio 拉起 + 握手 → `GET /api/mcp/servers` 端点。工具翻译也含在内（GetTools），但**不接进 run**。
- **为什么先拍这个**：契约先行——管理端点是本方案唯一 curl 打得到的新面，先把它打出来，A2 的 run 内行为才有可对照的「应然名单」。
- **怎么验收**：
  ```bash
  # 1. 写 mcp.json 配一个 filesystem server，起引擎
  curl -s 127.0.0.1:PORT/api/mcp/servers
  # 期望字节（形）：{"servers":[{"name":"fs","state":"connected","tools":
  #   ["mcp__fs__read_file","mcp__fs__list_directory", ...]}],"count":1}
  # 2. 改坏 command 指向不存在的程序 → curl 应见 "state":"failed" + 原因，引擎不崩
  # 3. mv mcp.json /tmp/ → curl 应见 {"servers":[],"count":0}（验完移回）
  ```
- **回答什么问题**：stdio 拉起 + 官方 SDK 握手 + GetTools 三件在真实进程里通不通（含 §2.3 待核实项）。

### A2 接通拍：工具进池 + run 内真实调用

- **改什么**：base 池并入口（§4.2）+ 观测行（§7）。机制零新增，纯接线。
- **怎么验收**：
  ```bash
  # 起引擎（带 fs server 配置），起一场 run
  curl -s -X POST 127.0.0.1:PORT/sessions -d '{"title":"mcp"}'
  curl -s -X POST 127.0.0.1:PORT/sessions/<id>/runs \
    -d '{"query":"用 read_file 读 workspace/notes.md 开头三行，原样引用"}'
  # 期望：tool 行 name=mcp__fs__read_file；引用内容与文件头三行逐字一致；terminal complete
  # 反证：mv mcp.json /tmp/ 再跑同话本 → agent 报「没有这个工具」，无伪造执行痕迹（验完移回）
  ```
- **回答什么问题**：MCP 工具经声明过滤、四态池、溢出层全链路跑通；模型真的会调它。

### A3 护栏收口拍：untrusted notice + 单测 + doc 收口

- **改什么**：ToolCallResultHandler 挂 notice（§6.2）；单测两件——配置解析拒载（坏短名/未知键/空文件）、命名拼接与撞名（同工具名双 server 不撞、与一手名不撞）；doc/16 加能力行。
- **怎么验收**：`go test runengine` 全量绿 + vet 干净；活体一场：MCP 工具结果帧里 notice 前缀可见。
- **回答什么问题**：护栏交互（§6）不是纸面推演。

## 9. 后置拍（方案内挂账，非「不做」）

| 项 | 启动条件 |
|---|---|
| HTTP/SSE 远程 server | 出现第一个真实远程 server 需求；届时连连接管理/超时/断线帧面语义 + WeKnora OAuth 一族一起拍 |
| deferred 目录代理（T4/T5） | MCP 工具总数到了声明成本可见的量级（几十 → 上百）；照 WeKnora 追加④抄五步机制 |
| server 进程常驻/跨 run 复用 | 每场 run 的启动延迟被真实体感卡到 |
| fileOps 收编 MCP 写 / MCP 写类默认关 | 第一个「用 MCP server 当主力写文件」的用例出现 |
| 按租户配 server（doc/28 缝） | 多租户落地后，`mcp.json` 从仓库级拆出按人级 |

## 10. 明确不做

- **MCP server 半边**：不把 tako 工具暴露给别人。
- **OAuth/凭证管理**：HTTP 拍的事，首拍 stdio 无此面。
- **目录代理、BM25 检索**：T4/T5 的既定归属，本方案不提前。
- **引擎解析 MCP 结果内容**：外部文本只给模型，不给引擎当指令（§6.2）。

## 11. 随批 doc 改动（A5 已随批落地）

- doc/16 能力总表第 11 行已加（状态 ✅，台账指向本 doc A1–A4）。
- doc/19-v6/19v6-06 §6 已补：第一个真实负载已经到场——MCP client（doc/33）落地后，T4/T5 的目录代理以 MCP 工具为第一个真实负载开工。

## 12. 拍板点清单（2026-10-18 按推荐拍齐）

| # | 项 | 推荐 | 状态 |
|---|---|---|---|
| 1 | 适配器 officialmcp（官方 go-sdk 底座） | 推荐 | 已拍（A1 落地） |
| 2 | `mcp.json` 位置与形状（§3.1） | 推荐 | 已拍（A1 落地） |
| 3 | 生命周期 = 每 run 起手拉起、终态杀、不常驻（§5） | 推荐 | 已拍（A4 落地，ctx watcher 收尾） |
| 4 | MCP 工具默认进 base 池、子代理按四态继承、挡用 exclude（§6.1，沿用 19v6-08） | 沿用 | 已确认（handler 排 agentSpecHandler 之前，入口快照天然带 MCP 工具） |
| 5 | `GET /api/mcp/servers` 作为契约拍（§8 A1） | 推荐 | 已拍（A1 落地） |
| 6 | fileOps 残余链不补救、登记在案（§6.3） | 推荐 | 已拍 |
