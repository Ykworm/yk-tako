# 19v6-07 · compact（上下文压缩的 v6 形态）

> 本文是 compact 的**机制裁定真源，自足成文**：设计与链路的全部细节都在本文，不依赖任何台账
> （`tmp/` 下 planning/todos 是施工期工作文档、验收后即清理，不得作为细节去处被引用）。
> C1 已落地部分（commit `f09deb6`）标「**现状**」，§3 的 C2 施工面已于 **2026-10-11 全部落地**
> （C2 摘要调用规格 / C3 回归网 + 变异检查 / C4 doc 回填，实测记录在 tmp 台账「C 系列」节——
> 台账验收后即清理，不并入本文）；**§5 两条登记的 C5 施工面亦已于 2026-10-11 落地**
> （结构化摘要 + 预算分档 + POST 准入 + fileOps 块，A1–A7，实测在 tmp 台账「C5」节）。
> **§1 的 limit 预留已于 2026-10-12 三期重裁**：limit = window − min(window/5, 16384)（19v6-09
> §9 落地；原 ×4/5 条已翻案，差分与数值实测在 tmp 台账溢出节）。
> 设计渊源：算法取 `doc/12` §9，落点按 v6 前提重推（doc/12 的旧线载体——钩子 / `run_archive` /
> `event_log` / `compact_rag`——v6 均无对应物）；2026-10-10 吸收 WeKnora
> `internal/agent/compaction` 全包实读的借鉴（出处为历史注记，内容本体在 §3/§5 自含）。

## §0 范围与不变量

- compact 是**引擎内部行为**：发生在 `start` 帧之后、首个 `delta` 之前（worker 的 `prepareInput`，
  在 ADK 主循环起手前）——客户端所见 = 气泡在等首字，**四个 API 的字节零变化**（不加 compact 帧，
  承 19v6-90 三修原则；要展示留给前端 loop）。
- compact 压 `run_model_messages`（mh）、**不动 runlog**（19v6-03 真源划分）；compacted 原文行
  保留不删（审计原料，未来 RAG 原料——物理清除归运维面）。
- system 提示词不进折叠（WeKnora 同规；v6 里 system 由 agent Instruction 承载、不在 mh 表，
  天然满足）。
- RAG / `search_archive` **后置**：v6 无工具循环、无 Web 事件库 ⇒ `compact_rag` 没有消费者；
  被压原文行保留即未来原料。随 tool loop 立项再议。

**端到端链路（现状 = 实线，C2 施工面 = [C2] 标注）**：

```
POST → 抢闸 → goroutine Run(ctx)
  │
  ▼
prepareInput：
  读 mh 全序（user 行在 AcquireRun 时已入）→ countInput（system + 所有行文本，cl100k）
  │ ≤ limit（= window − min(window/5, 16384)）─► 快速路：原样送 ADK 主循环（零行为差异）
  ▼ 超限
foldInput：run 粒度分组 → 旧摘要 seq=0 行并入 renderFold（「此前摘要，请合并进新的摘要」）
  → n 从 20 降，每档 summarize 一次：
       同一 ChatStream 流式排干 ──[C2] 转录分级截断 ──[C2] 低温 0.3 / stall 超时 / 重试分流
       ──[C2] 三层验货（算术主检 + 字符串辅检 + 缺失处理）
  → 塞得下 → stagedCompaction{DropRunIDs, Summary, AttachRun}
  → n=0 还塞不下 → errStillOver → status=error 零写入
  ▼
ADK 主循环（T1/T2 工具环）→ delta 帧
  ▼
defer EndRun（唯一出口）：complete → 同一笔 PG 事务
  放闸 + 定稿 + UPDATE compacted=true + INSERT seq=0 摘要行（挂 AttachRun）
  error/闸失/进程死 → 零写入，下轮从原文重压
旁路（19v6-09 已立项，机制以该篇为准）：provider 报「太长」/ 成功但被截断 → 强制压缩 + 只重试一次
```

## §1 触发形态

- **触发者 = worker 发送前自测**（`prepareInput`，worker.go）：`countInput(system + 全量非
  compacted 行) > limit` 才进折叠；快速路（≤ limit）与压缩前零行为差异。**无定时器**（现役无
  调度器；长度判断只在发送前发生一次）。
- **压点 = run 边界**；**两态折叠的称呼（2026-10-11 与用户定名）**：**Run fold** = run 起手前
  的折叠（= 现役 foldInput，run 粒度下刀）；**mid-run fold** = run 内多段调用中间的折叠
  （**立项方案 = 19v6-10**，2026-10-11；机制以该篇为准）。T2 工具环落地后 run 内部已多段；
  「压 run 中间」的对象 = 引擎内存 state 里当前 run 的早段轮次——run 中途的行不在 mh
  （内存累积、EndRun 才落库），Run fold 的原料是 mh 表、够不着起手后的增长，这正是 mid-run fold 要补的洞。
- **摘要行 role = `user`**：输入组装侧零改动（worker 的 default 分支已把 user 行映射成
  UserMessage）；system 插会话中段部分厂商不容；旧线同选（带标记的用户消息）。
- **limit = window − min(window/5, 16384)**（`-compact-window`，默认 256000；0 = 关）。
  **2026-10-12 三期重裁落地**（闸门 = 溢出修复层一期在真 provider 上真兜住过，19v6-09 §9）：
  原 ×4/5 = 「响应空间 + 估算误差垫」双职能（cl100k 对非 OpenAI 分词器是近似值、两次检查之间
  可能插进巨型工具结果）；误差垫职能 2026-10-12 起由溢出修复层兜底承接，预留改为固定档——
  16384 = WeKnora settings.go 固定 16k（回复大小不随窗口缩放）。min 的连续档：window = 81920
  处两式相交（81920/5 = 16384），之下退化为 ×4/5（dev 小窗 600 → limit 480 逐值不变，不留新
  魔法边界）；256k 窗 limit 204800 → 239616，**吃回 34816 ≈ 35k**。原 2026-10-10「维持比例不动」
  裁定的前提（修复层缺位）已消失，据此翻案。
- **计数 = cl100k 全量 BPE**（doc/12 §3 口径，编码固定不按模型再配；`TokenCounter` 接口注入，
  装配期取词表失败即 fail fast；单测注入假计数器不触网）。**偏离登记**：不落「上条 assistant
  Usage 做基数」（v6 无 Usage 列）。**复审项**（讨论稿 `doc/24-用量计数.md`，2026-10-15 改口径、未点头）：
  场内下一 Turn 发出前滤掉 `ReasoningContent`；有上一笔 Usage 时可用
  `PromptTokens + (CompletionTokens − 思考估) + 锚点后新行` 作 mid-run 触发候选；工具 schema
  仍不计入触发；无账单时仍全量 `countInput`。点头前本条的计数裁定不动。

## §2 折叠算法（foldInput，compact.go）

- **单位**：旧线「轮」= v6 run。「已完成」= 非当前 run 的行；**error run 只留 user 半（assistant
  半本就不存在——§4 零写入家族），照压**。
- **分组**：mh 全序按 (run_id, seq) 分组；当前 run 行原样跟尾（tail），不进 keep/drop 划分；
  seq=0 行 = 旧摘要（重压时并入，见下）。
- **n 降序循环**（doc/12 §9）：keep = 最近 n 个已完成 run（n 从 20 起，实现内常量），drop =
  其余；每档 `Summarize(renderFold(drop))` → 拼装 `[摘要行] + [keep 行] + [tail]` →
  `countInput < limit` 定案；否则 n--。
- **旧摘要原地更新**：seq=0 旧摘要行**不回喂重 summarized**——renderFold 将其 prepend
  （「此前摘要，请合并进新的摘要」）作为新材料并入新摘要（防「摘要的摘要」；2026-10-10 与
  WeKnora prepare.go 机制互证等价，用户裁「加入」）。
- **二级压缩 = 不单做**：重压合并（旧摘要并入新摘要）即其实现，不是独立机制。
- **errStillOver**：n=0 仍超 limit → 不调主模型、status=error、零写入（doc/12 §9）。错误信息带
  诊断参数：`折 N 个 run（共 X tok）→ 摘要 Y tok；合计 Z tok ≥ limit L`，用于判断差多少才能过
  （摘要本身太长 / keep·tail 压不动 / limit 太紧）。
- **摘要数量：一次压缩事件 = 一份摘要（§8.2 读法 A，裁定）**。被压段（drop 集合）整体
  summarize 一次，只产出一份摘要。
  - §8.2 原文「每被压 run 一行摘要」有歧义，两种读法：
    - A（裁定）：不管这轮压了几个 run，只出**一份**摘要，挂在被压段最后那个 run 上。
    - B：压几个 run 就出几份摘要，每个 run 挂自己那份。
  - 日常一次只压 1 个 run，A 和 B 在库里看不出区别（都是 1 行摘要挂在 1 个 run 上）。
    只有一次压多个 run 才分叉：A 是 1 份覆盖整段，B 是每个 run 各 1 份。
  - 不取 B：要调 k 次 summarize；摘要行在输入里越攒越多，需要二级压缩——但二级压缩不单做
    （前条「二级压缩 = 不单做」），B 没有落地路径。
  - **跨事件不变量**：任意时刻**活摘要**（`compacted=false` 的 seq=0 行——会进模型输入的
    那份）恒 ≤ 1 份。新摘要在 EndRun 同一笔事务内整体替换旧摘要：旧行不删，被
    `UPDATE compacted=true` 翻成死行留档（其 run 已并入 DropRunIDs，compact.go）；读侧只查
    非 compacted 行（pgstore.go）——「一次事件 = 一份」是这条不变量的事件内半边。B 一次
    事件出 k 份活摘要，直接违反它，是比「没有落地路径」更强的否决理由。
- **挂点 = drop 集合里最新的那个 run**：摘要作为 `seq=0` 行挂在被压段中时间最晚的 run
  上（也就是最靠近当前 run、排在序列末尾的那个）。
  - 这是 `UNIQUE(run_id, seq)` 下**唯一合法**挂点：旧摘要所在 run 的 `seq=0` 槽已被旧摘要行
    永久占用（`compacted=true` 不释放 UNIQUE 约束），挂回旧 run 必撞唯一索引。
- **摘要提示词原则**（compactPrompt，全文在 compact.go）：按这段内容实际在谈的事做摘要，只保留
  对后续仍然有用的信息；能抽到才写栏目（已知事实 / 关键决策 / 未解决问题 / 用户偏好），没有的
  栏目不编造、直接省略；只输出摘要正文。
- **偏离登记**（相对 doc/12 §9，逐条）：无 RAG 两档舞（检索块不存在）；无 `Notify(正在压缩)`
  （帧面零变化，§0）；无 Usage 基数（§1）；**窗口文件链不搬**（旧线「内存 → model-windows.json →
  `GET /models`」查找链——v6 单厂商 dev，旗标即源）；**不复刻「摘要压进本轮 user_message」**——
  行表形态下摘要独立成 seq=0 行（§8.2 已裁）。

## §3 摘要调用规格（C2 施工面；括号注 = 现状）

- **渲染（C2 扩：标注转录 + 分级截断）**：借 WeKnora serialize.go 全形态——转录标注
  `[User]:` / `[Assistant]:` / `[Assistant tool calls]: name(k=v,…)`（参数 key 排序，保跨次
  渲染一致）/ `[Tool result <name>]:`；分级截断：工具结果 2000 / 工具参数每 value 400 字符
  封顶（rune 计 + 截断标记）。**工具环渲染缺口（2026-10-11 登记）**：现状 renderFold 只拍平
  `role: text`，assistant 工具轮的 `tool_calls` 与 tool 行的 `tool_call_id` 不渲染——调用
  名 / 参数 / 结果归属在摘要输入里整体失明。后果分级：纯问答轮结果自解释、摘要器能凭下一轮
  复述猜回大意（C1 验收即此类，故当时未暴露）；三类猜不了——结果不自解释（搜索 / 裸 JSON /
  错误码）、一 run 多次调用归属错乱、**变更类工具（write/edit/shell）整体归零**（参数才是
  历史、结果常只有一行 ok，拍平后「这次写入」等于没发生过——§5 fileOps 条「永不终止循环」
  同病因）。定性：「失明」而非「变笨」。成因：C1（10-08）先于 T2 工具环（10-10）落地，
  回放链跟上了两列（prepareInput），渲染面没回头改（第 6 拍欠账）——此条从「借形态」升级为
  「不补 = 摘要器结构性失明」。**这是重试的前提**——输入无界时重试同一份必然原样失败。
- **调用**：同一 `ChatStream` 另起一次流式生成、排干取全文（doc/12 §4；不新开 agent）。
  观测欠账：摘要流量无 tracing 标记（WeKnora 以 metadata `agent_context_compaction` 与主
  推理分离），随 C2 一并补。
- **旋钮三件套（C2；初值全部标注来源、dev 校准，拒抄常数双重标准）**：温度低温档（0.3，
  借 WeKnora 经验值）；超时 = **流式 stall 检测为主**（首 token 限时 + chunk 间隔限时，
  初值 30s/30s 待校准）＋ 宽松总上限仅防无限拖长（本质仍是墙钟，非主要判据）；重试 ≤2 次且
  **按失败原因分流**——瞬时错误（429/5xx/断连）原样重试，超长错误不重试同一份输入。
  （现状：无温度、无超时、单次尝试即败。）
- **验货（C2 升级为三层）**：① **算术主检**——completion_tokens ≥ 自设 maxTokens 上限 ⇒
  截断拒收（厂商/分词器无关；误杀为测度为零巧合，代价仅多一次重试）；② 字符串辅检——
  finish_reason ∈ {length, max_tokens, max_output_tokens} 拒收（OpenAI 系含 Kimi = length；
  Anthropic = max_tokens；Gemini 原生 = MAX_TOKENS——白名单有缺失/变体盲区，故只作辅检）；
  ③ 缺失处理——finish_reason 缺失时只看算术检 + 内容长度启发式，不放行也不误判。
  （现状：仅空摘要拒收，C1 已落地。）**plumbing 前置**：summarize 排干循环现只收 Content，
  usage/finish_reason 未接（usage 需显式 include_usage）——动工先实读 eino 流末帧暴露面。
- **失败出口 = A error 干净失败（2026-10-11 用户裁，C2 定案）**：status=error、零写入、人看得见——
  重试耗尽 / 验货拒收 / stall 超时一律走这条 C1 现状路径（不调主模型），失败原因分类进引擎日志
  （瞬时重试耗尽 / 超长拒收 / stall / 验货拒收），事后可查。
  B / C 未裁、背景存档不删（重开时从本文取）——
  - B 折半重试（未裁·存档）：被压段砍半再试一次，成本有界；兼作「摘要输入本身超窗」的分流出路
    （超长错误原样重试必然原样失败）。
  - C Degraded 降级（未裁·存档；WeKnora 路线）：摘要器失败后原始存档流水账顶上、turn 继续。
    选 C 的前提——三层标记齐备：**有界**（尝试封顶）、**响亮**（结构化字段进日志/事件 + 观测事件
    透出）、**诚实**（摘要文本内自标记，模型知道自己手里是流水账——WeKnora 存档第一行即自白）。
  - 存档理由：tako 定位是 agent 运行时，C 曾有吸引力，但三条件缺一「糙」就从兜底变隐患；B 是
    A 与 C 之间的折中。用户裁 A：先保审计洁癖，折半 / 降级待真用例逼出再重开。

## §4 落库事务边界

- **零写入家族**：发送前只 Stage 在内存（`stagedCompaction`）；`EndRun` 仅 complete 分支
  落库——error / 闸失 / 进程死 ⇒ 零写入，下一轮从原文重压（原文从未被动过，连快照都不必有；
  旧线「失败退回快照」的 v6 等价物）。
- **单事务**：complete 时 `UPDATE compacted=true（被压行）` + `INSERT seq=0 摘要行` 与放闸、
  定稿**同一笔 PG 事务**（承 D1 事务边界）。
- runlog 无 compact 行（真源划分）；compacted 行永留（§0）。

## §5 前方登记（每一条都含完整背景；各条立项/落地状态见条目内标注）

- **切点选择规则**（需求已登记，2026-10-10 用户裁「值得加入，业务流程要讨论细化」）：
  WeKnora 形态（cutpoint.go）——从最新消息倒数、累加 token 估算，保留「最近 ~20k token」的
  逐字尾（上限 = 可用窗的 1/4）；**切点绝不落在 tool 消息前**（工具结果必须与发起它的 assistant
  消息相邻，否则 provider 拒收）；system 永不压；预算是天花板不是凑整（一条工具结果动辄上万
  token，「到线就留」会实际多留好几万）；切在 turn 中间时，给被切掉的 turn 前缀单独写一段摘要，
  保留的后半段才有上下文。**v6 的落点张力**：v6 落库是 run 粒度（compacted 标记 / seq=0 挂点 /
  `UNIQUE(run_id, seq)` / §2 重压合并全按 run），WeKnora 下刀是消息粒度——
  **路线 A**：保持 run 粒度 keep，只引进「不拆配对」精神（run 边界 = 工具往返的完整单位，天然
  不拆）；**路线 B**：run 内下刀（partial-run 压缩），seq=0 唯一挂点与重压合并语义须重推。
  **倾向先 A 后 B**（B 留待用例逼出），随 mid-run 压缩立项细化。
- **溢出修复层**（**✅ 立项方案 = 19v6-09**（2026-10-11）；本条保留作需求由来，机制以该篇为准）：两层检测——① provider **报错**匹配 24 条「太长」正则
  （Anthropic/OpenAI/Gemini/Bedrock/Kimi/MiniMax/Ollama/通义……附 4 条非溢出排除——throttling /
  service unavailable / rate limit / too many requests——防把限流误判为超长）；② **成功但被窗口
  截断**的响应（finish_reason=length 且 completion 小于自请预算 ⇒ 是自己的上限而不是窗口掐的
  才正常，反之即被窗口截断；或 prompt_tokens > window）。触发后**不信任任何估算**（被拒本身就
  是估算不可信的证据）→ 强制压缩 → **只重试一次**（overflowRecovered 标志防死循环）。价值：v6
  计数若在安全方向错了，现状就是纯死；此层落地后 §1 的 ×4/5 已重裁为固定预留、吃回窗口
  （2026-10-12 三期落地，见 §1）。
- **trimToolResults 式「压后仍超」最后手段**（借 WeKnora observe.go，2026-10-11 登记）：
  压缩已发生仍超阈时，工具结果总预算 = window/5（clamp [8192, 32768]）——全部工具结果先换
  marker（原字节数记账），预算内从最新往回还原文本，放不下的给 1/4 头 + 3/4 尾预览夹；
  copy-on-write 不动留档原文。v6 现状对此只有预留 + errStillOver（整轮 error），中间
  没有「既不 error 也能发出去」的层；与溢出修复层同宗（都是「估算错了之后还能活」）——
  **已随 19v6-09 同立项，裁定 = 落二期**（理由见其 §8 有效域登记：Run fold 的 n=0 候选
  无工具行，trim 恒空转；真主场是 mid-run 候选与救援候选）。**实证（2026-10-11 C2 验收实录，deepseek-flash）**：摘要体积 ≈ 输入 0.7× 的模型
  会把小窗（600/limit 480）下的 S+tail 顶穿 limit；errStillOver 之后同会话**每条**后续消息都重演
  同一失败（失败消息的 user 半照留、可见集只增不减，S 必不小于上轮）——会话事实上卡死到人工
  干预或换窗。**勘正（2026-10-11，19v6-09 §8）**：这次卡死的真凶是摘要体积（S+tail 候选里
  没有工具结果，trim 救不了它），已由 C5 预算帽治掉；它证明的是「没有兜底层 = 会话卡死」
  这个总命题，trim 的真缺口在 mid-run / 救援候选。
- **结构化摘要（槽位 + 槽预算）**（2026-10-11 与用户定稿方向；立项 = C5，见末节；**✅ C5 落地
  2026-10-11**——A1 准入 / A2 槽骨架 / A3+A7 预算帽（A7 = 窗口分档 + Claude override +
  max_output 硬顶），实测在 tmp 台账「C5」节）。动机三重：
  ①**卡死教训**（本次实录：摘要 525 tok + tail 62 ≥ limit 480——摘要体积是模型随机输出，唯一
  硬保护是 4096 的调用级 cap，小窗下形同虚设）；②**散文漂移**（旧栏目无骨架，跨合并栏目互相
  吞并、越写越肥，锚点事实存活靠运气）；③**机械锚点无家**（fileOps 需要稳定地址）。
  compactPrompt 从软栏目升级为硬骨架——markdown 槽、槽序恒定、空槽省略不编造：

  1. `## 会话意图` —— 用户核心目标，一段话。合并时最稳的锚：agent 任务延续性强，意图槽漂移最小。
  2. `## 关键决策` —— 决策 + **理由**。理由必须带：重压合并时「为什么」比「是什么」耐压，
     理由是后续沿用/推翻决策的依据。
  3. `## 重要事实` —— 数字、约束、错误码、暗号类锚点。fileOps 机械块钉在全摘要末尾（见下条，
     程序拼接位——理由同下条）。
  4. `## 待办与下一步` —— 未完成事项、下一步动作。替旧「未解决问题」：agent 是任务流不是
     问答流，待办比悬而未决的问题更贴工作面。
  5. `## 用户偏好` —— 语言/风格/格式要求（旧 prompt 槽保留，成本极低）。
  6. `## 其他上下文` —— 自由尾，装不进槽的重要信息；没有就整段省略。

  **预算（治「S 支」卡死的定量面；A3 定守恒、A7 定分档）**：恒有解的推导——n=0 时输入 = S + tail，
  要保证此刻必有解，需 S ≤ limit − tail_max。tail 支 = POST 准入 limit/3（19v6-01 错误态表）；
  S 支 = 摘要调用的 maxTokens 帽，**按窗分档**（2026-10-11 A7 用户表落码，纯函数
  `summarizeCeiling(window, modelRef, maxOutput)`，装配期一次算好）：
  - 小窗（window ≤ 32768）：`min(limit/2, 1024)`——limit/2 是恒有解守恒的本体
    （S + tail_max = limit/2 + limit/3 = 5/6·limit < limit，余 limit/6 给 keep 与估算误差；
    除数 2 的来由 = A3 校准：除数 3 在 window 4000 下帽 = 1066，低于 A2 实录 S = 1221
    的模型自然产出，折叠会假性验货拒收）；1024 封顶 = 防 verbose 模型超产的反向校准。
  - 中窗（32768 < window < 1048576）：**4096**（= 原常量值，正常观察区间；256k 生产窗
    与原行为同值，生产零变化）。
  - 大窗（window ≥ 1048576）：**8192**（区间下限；不随被压段线性涨）。
  - 档间空带由下档延伸、不插值（单调不减，确定性优先；换档就拨窗）；window = 0（compact 关）
    → 4096。
  - **Claude override**：ref 的 provider 段 = `anthropic` 或含 `claude`（小写化）→ 固定档
    ×1.3（4096→5324 / 8192→10649，整数截断）；**只乘固定档**——小窗式是恒有解算术本体，
    乘了破守恒。
  - **max_output 硬顶**：env `TAKO_MODEL_MAX_OUTPUT`（正整数 tok；缺省/0 = 不顶；非法值
    启动即退）→ 帽 = min(该分档值, 值)，最后生效。
  每槽预算写进提示词（初值：意图 ≤ 100 字、决策 ≤ 300、事实 ≤ 300、待办 ≤ 200、偏好 ≤ 100、
  自由尾吃余额，dev 校准）：槽内取舍是模型的活，槽间配给是程序的活。**超槽不拒收**（逐槽拒收的
  误杀面 > 收益），由总预算 maxTokens 兜底——超的是质量不是正确性。
  **与重压合并的交互**：下一事件程序按 `##` 切分旧摘要的槽（markdown 切分，无解析脆弱面），
  槽对槽并入新调用的输入（旧槽内容 = 事实来源，新掉落段 = 增量），模型重 emit 全骨架——骨架
  恒定所以模型知道往哪放、去哪找。C5 前的存量摘要是自由散文 ⇒ 首个合并事件把旧散文整体塞进
  「其他上下文」槽过渡，此后自然结构化（迁移语义显式登记，不留隐式兼容代码）。
  **规则四条**：①空槽省略不编造（旧原则，防硬填垃圾）；②槽序恒定；③markdown 头不追 JSON
  （流式稳、人可读、回归可钉字节；逐槽检查 = `##` 切分，切不出 = 该槽按空处理并 log——不拒收，
  验货维持空/截断两道、不扩误杀面）；④总预算按上式。
  **验收口径（C5）**：单测钉槽位顺序与空槽省略——松断言，钉「结构存在」不钉「内容完备」（内容
  遵守是概率性的）；暗号 e2e 升级结构化形态（C2 话本第 ③ 拍原样重跑，存活判据不变）；C2 ⑧ 形状
  回归零变化（compactPrompt 只在摘要流内部，四端点字节天然不动）；预算遵守用日志观测（tracing
  行已带 completion），不进断言。
  **明确不做**：JSON/schema 硬编（解析脆弱、流式多一层转义风险——markdown 头已拿到结构化的
  主要收益）；增量/摊销摘要（已裁定不引进：写点干净 + 事件稀疏）；embedding 级事实抽取
  （那是 §0 召回面的活，不在摘要内做）；逐槽机械拒收（理由见上）。
  **立项形态：C5 小拍**——与「maxTokens 预算帽（A3 成比例 → A7 分档）+ POST 单条准入（tail 支）」
  合并施工；
  摘要本体链只动 compactPrompt + summarizer 预算接线 + POST 准入检查；fileOps 子项（下条）在
  foldInput 定案点接钉块（2026-10-11 A5 施工修订：原稿「foldInput 零改动」在子项合入后不再成立）；
  落库/回放零改动（摘要对存储面是 opaque string，块只是摘要文本的尾巴）。
- **fileOps 式机械路径保留**（2026-10-11 与用户细化；立项 = 上条结构化摘要的子项，槽位即地址；
  **✅ C5 落地 2026-10-11**——A4 提取器 / A5 钉块与三档收口，实测在 tmp 台账「C5」节）。
  被压段里读写过的文件路径**用程序从 tool_calls 机械提取**（**全链路 0% LLM**），钉在摘要**末尾**的
  专用块里（程序在 summarize 定案后拼接——线上实测块恒在末位、恰一份；解析取末位最后一份，
  模型 echo 旧块也盖不过程序钉的那份），跨压缩逐代继承。动机（WeKnora 战例）：散文最先丢路径——丢掉
  「我写过 /workspace/output/deck.html」的记忆，模型会重建磁盘上已有的文件，大文件分块写 +
  重启 = 永不终止的循环。
  **提取时机与原料**：fold 定案时对本事件 drop 集合的 tool_calls 列（arguments JSON 已在库）
  提取；更早段的路径已由上一代块继承，不重复提取。输出 = 新提取 ∪ 继承块的合并，钉到新摘要
  （旧块随旧摘要翻死）。
  **收集容器 = 三张清单，条目是三元组**。一次压缩事件进行中的收集结构（fileops.go 的核型）：

  ```go
  type fileOpEntry struct { path string; tool string; runID string } // 路径 + 来源工具 + 来源 run
  type fileOps struct {
      read    []fileOpEntry // read 系工具碰过的
      written []fileOpEntry // write 系碰过的
      edited  []fileOpEntry // edit 系碰过的（独立收集只为保住来源注记）
  }
  ```

  三条清单各管一类效果；edited 与 written 在渲染时合并进「改过」组，但来源注记各自保留
  （块的每行要写「（edit_file，r_XXX）」，所以条目必须带工具名，不能是裸字符串）。
  追加规则：同一张清单内按 path 去重（保留先进的一条）；事件若继承了旧块，旧条目先进池、
  新提取的排后——**这个先后顺序就是淘汰顺序**。
  **上限与淘汰（三档降级链，2026-10-11 用户裁定）**：常态**不封顶**——抓多少记多少，块随
  路径自然增长（保住路径是这条机制存在的全部理由）。钉块的收口检查 = **重跑 fold 自己的
  验收**（2026-10-11 A5 施工中用户拍死口径；原稿「S + 块 + tail ≤ 预算帽（时值
  min(4096, limit/2)，A7 起为分档）」
  已弃——帽 = 摘要自身的预算帽，块无预算可言；且算式不含 keep，过了它发送候选仍可能 ≥ limit）：
  把「摘要+块」并回本轮发送候选，重数 system + 摘要+块 + keep + tail < limit —— 不过 → 减灾档，
  两张表**各**砍到 50 条（读过 50、改过 50，公平对待、无「先吃读过」的优先级）重渲染再验；
  仍超 → 折叠失败走干净 A（errStillOver，C2 已有链，不新造）。
  砍法 = 按进池先后淘汰最老，两表各自独立
  执行（「改过」满不动「读过」——resolve 保证两表无交集，没有跨表协调）。
  **块格式**（稳定标记 + 版本位，程序可解析）：

  ```
  <!-- tako:file-ops v1 -->
  改过:
  - /workspace/output/deck.html（write_file，r_XXXX）
  读过:
  - /workspace/spec.md（read_file，r_XXXX）
  <!-- /tako:file-ops -->
  ```

  每条注记来源工具与 run id（审计回指）；`v1` 是格式版本位——格式演进按版本解析，解析失败 =
  该块按空处理 + log（不拒收，验货不扩面的同一原则）。
  **判定三分支**（对每条工具调用依次问、先答上的为准；全链路 0% LLM，每条规则一个 fixture）：

  **① 注册表声明位**。v6 第一方工具的注册元数据加一个字段 `file_effect`，取值
  `none | read | write | edit`；**不写 = 空串 = 按 none 处理**（零值默认，现有工具零改动，
  2026-10-11 用户裁定）：工具在注册表中、字段为空 → 无记录，**不进 ②**——想让提取
  搭理它就得显式声明 read/write/edit；工具**不在注册表**（外来 MCP 工具）→ 走 ②。
  配套**启动 lint**（装配
  阶段跑一次，不在提取热路径）：注册表中工具名**精确等于**目录里的某个名字、且 `file_effect`
  为空串 → 启动报错。目录 v1 完整名单（常量表，11 个名字，出处见下注）：

  ```
  read_text_file  read_media_file  write_file  edit_file  move_file
  create_directory  list_directory  directory_tree  get_file_info  search_files
  read_file
      ← 11 名全数出自 npm 包 @modelcontextprotocol/server-filesystem@1.0.0
        （package.json repository = github.com/modelcontextprotocol/servers.git）
        dist/server.js 里 14 个 registerTool 中的 11 个（2026-10-11 实读；未收 3 个 =
        list_allowed_directories / list_directory_with_sizes / read_multiple_files）。
        README 列 13 个 = 文档滞后摘掉 deprecated read_file，不作依据；read_file 真身
        即该服务器（dist 内标「Read File (Deprecated)」「DEPRECATED: Use read_text_file
        instead」，read_text_file 的废弃前身），不是 OpenHands / Open Interpreter 惯用名
        （OpenHands LLM 面 = str_replace_editor + edit_file(LLM editor)，两条独立拆解来源
        一致 = 间接证据；Open Interpreter 无任何证据）。业界惯用三件套的本地一手旁证 =
        hermes-agent（ref-agent/hermes-agent/agent/tool_dispatch_helpers.py:51-52 的
        _PATH_SCOPED_READERS = {read_file, search_files}、_PATH_SCOPED_WRITERS =
        {write_file, patch}）。
  ```

  **数字按 v1 快照语义读**（2026-10-11 用户裁定，✅ F12 勘误落定）：快照 = 当下出处并集
  数出的数，随出处演化与第一方增补而变、不固定——「11」不是恒定契约。

  显式写 `none` 即豁免（= 作者表态「我知道这名字像文件工具，我确实不碰」）。lint 用精确名单
  而 ② 用前缀，因为两张表误报代价不对称：② 误报 = 清单多一行（宁多抓），lint 误报 = 阻断
  启动（宁漏不误杀）；名字像但不精确命中的（如 read_sensor）不受 lint 管辖。
  **② 外来工具前缀表**（只服务注册表里没有的外部 MCP 工具）。匹配语义：**名字转小写后做
  前缀比较**。写侧 v1 清单：`write edit create save append touch`；读侧 v1 清单：
  `read cat ls grep glob find head tail stat`。命中写侧 → written 桶；命中读侧 → read 桶；
  **两侧都不命中、但参数 JSON 里有非空 `path` 字段 → 归 written 桶**（这就是「歧义一律归
  改过」的唯一触发点：无法判定的按强信号记）；参数里没有 path 字段 → 不产生记录。
  **③ shell 写意图表（v1）**。输入 = 参数 JSON 的 command 字段；按空白切词后逐词扫描。
  每条规则 = 输入 → 抓到的路径，按此实现：

  ```
  重定向：词等于 > 、 >> 、2> 、2>>  → 取下一个词、剥掉首尾引号为路径
      "cat a.md > /tmp/x.md"        → /tmp/x.md
      "echo hi >> '/tmp/y file'"    → /tmp/y file
  写命令表（词等于命令名时按规则取参）：
      cp / mv：最后一个词            "cp a b"           → b
      tee / touch / mkdir / install：命令之后所有不以 - 开头的词
                                     "touch d1 d2"      → d1, d2
      sed / perl：词列里存在独立的 -i 时（v1 只认独立 token，-ie / -i.bak 不碰）取最后一个词
                                     "sed -i s/a/b/ f.md" → f.md
      dd：所有 of= 前缀的词，取 = 后的值   "dd if=in of=out" → out
  ```

  一条命令可抓多个路径（`"cp a b && > c"` → b 和 c），全部记入。明确不碰（扫描不命中就
  当这条命令没有写，**不产生记录**）：变量展开（`> $OUT`）、命令替换（`$(...)`）、heredoc
  （`<<EOF`）、解释器内写盘（`python -c ...`）。shell 只抓写：无重定向的纯读命令
  （cat / grep / ls）不产生记录——清单的用途是防重建，写侧才是强信号。
  **歧义一律归「改过」**：误判改过的代价是一行无害条目（上限自然淘汰），
  误判读过的代价是防再生保险丝断在正主上。
  **有界性边界（诚实登记）**：解释器内部写文件（python heredoc 写盘等）对模式规则不可见——
  不追完备；组织面收口 = 工具注册规约鼓励文件写走专用工具（声明位给了这个杠杆），shell 规则
  只兜底。分类是调用时刻的形状判断，不做事后磁盘校验（文件可能被外部进程改/删，维持「调用
  形状即真相」的简单性）。
  **与 §3 标注转录的分工**：转录把「调了什么工具、什么参数」写进摘要器的**输入**（帮忙，会
  失败——记不记由模型）；本条把路径钉在摘要**输出**（保管，不会失败）。分工原则立在这条上：
  **LLM 做取舍（什么是值得记的事），程序做保管（什么是必须不死的事实）**。原料等 file 类工具
  （write/edit/shell）进池后自然就位。
  **验收口径**：提取规则表单测（write/edit/shell 模式各一例 + 歧义归改过一例，纯函数）；
  块解析/合并/淘汰的往返测试；e2e = 带文件工具的会话压一遍后路径仍在新摘要块内（钉字节）。
- **mid-run fold**（run 内下刀 / 压 run 中间，**✅ 立项方案 = 19v6-10**（2026-10-11）；本条保留作需求由来，机制以该篇为准）：run 内多段调用随 T 系列落地后，触发面 / 落库不变量的第二个写点 / 增量计数三件事已在该篇一并裁定。**立项时的不变量闸门（2026-10-11 讨论登记）**：
  「run 内 assistant(tool_calls) 与其 tool 结果相邻且同 run_id」今天由 EndRun 的写入顺序保证，
  Run fold 因此免费获得「不拆配对」（折叠单位 ⊇ 配对单位，无需检查）；mid-run fold / HITL
  行插入立项当天该不变量失效，必须连开三件套——消息粒度下刀（seq=0 挂点与重压合并语义重推）、
  role 回扩检查（切点不得落在 tool 消息前，候选切点回扩到发起 assistant）、在途交换豁免
  （最新 assistant(tc) 与未出结果的工具语义上压不得）。**触发面的裁定背景**：不采用 eino 内置
  summarization middleware——它是全量替换（无 keep-recent 保留区，mid-loop 触发会把刚拿到的
  tool result 一起压掉）、无终止防线（压后仍超会每轮重复重压）、默认计数器 chars/4 对中文低估
  3–4 倍、纯内存态与 mh 落库冲突（跨 run 蒸发）；正确姿势 = 自写 middleware 挂
  `BeforeModelRewriteState`，里面调 v6 自己的折叠链（§2）与落库语义（§4）。
- **防每轮压缩闸：反向登记（明确不需要，2026-10-11）**：WeKnora 有三重闸防「压了又立刻
  再压」——ErrNothingToCompact / compactionExhaustedAt（记消息数）/ minFreedFraction（释放量
  ≥20% 否则标 exhausted）——前提是其摘要活在 turn 内存、历史跨轮还在。v6 不需要等价物：
  被压行落库即不可见，每次重压的原文集合自然收缩（新 run + 一份活摘要），不存在同段历史
  反复触发；n 降档 + errStillOver 已覆盖。登记此条，免得将来拿 WeKnora 对拍时重推一遍。

## §6 验收用例（C1 验收结论：2026-10-08 通过，用户裁「直接算通过」）

验收话本：会话攒到超窗（dev 用小窗口旗标逼出），发下一轮时引擎自动把保留集合之外的已完成 run
压成摘要，模型凭摘要仍能回忆早已挤出的内容（锚点：先告诉模型暗号「蓝莓七号」，压完再问）。

判定标准：

- **压缩发生**：引擎日志出「输入 N tok > limit M，进入折叠」→「压 k 个 run，摘要 S tok，挂
  r_X；发送 T tok < limit M」。
- **库形态（psql 断言）**：被压 run 的原文行 `compacted=t`（原文仍在库）；新摘要行
  `seq=0 / role=user / compacted=f` 挂在最后折入的 run；读侧热查询只见非 compacted 行。
- **记忆连续**：压完后的轮次答出暗号（来源只能是摘要——原文已挤出输入）。
- **字节零变化**：四端点（POST / session / frames / log）响应形状与压缩前逐字节同形。
- **负路径**：ErrStillOver 时 frames 只有 start + terminal{error}（无 delta）、log 无 assistant
  行、session 收口 `liveRunId:""`、psql 该轮零 compacted 写入。
