# 32 · media_offload（对话只存 path）

> 性质：**已落地**（2026-10-21 收口）。A1–A7 随 `dcf3e6b` 入库（2026-10-18）；A3-1 至 A3-5 与 A8-1/A8-2 随 `6e4de68` 入库（2026-10-20）。
> **本篇覆盖两个半边**：半边一（存储纪律，入口图与工具回执只落占位）= A1–A2；
> 半边二（模型可见，本轮图在发送份里还原成真图 part）= **A3-1 至 A3-5，2026-10-20 已落地**。
> 施工台账：`tmp/32-media-offload/`（A1–A7 的 planning + todos + 验收实录）与 `tmp/32-media-offload/a8-pdf/`（A8）；
> **A3 五拍当时没另起台账，施工与活体验收记录只在本篇 §4**。
> **验收结论**：A1–A7 与 A8 的逐拍实录见上述台账（2026-10-18 引擎 8947/8916、2026-10-20 引擎 8909/8910）。
> 2026-10-21 活体抽验（引擎 `127.0.0.1:8942`、独立库、Redis db 8，本篇 7 项断言全过）：带图投消息拿到 sessionId；
> user 行是占位（`path=workspaces/<wid>/.media/…`，锚在工作区目录）；runlog 零 base64；
> `GET /api/media?path=…` 取到字节且与上传同长；跨租户取同一张图回 404；删默认工作区回 409 `114005`（doc/35 A8a）。
> **这一批明确不做的**：扫描件 OCR（2026-10-20 已拍）、音视频内容（2026-10-20 已拍）、
> CJK 兼容字形归一化、复杂版式不承诺、「文本只进当枪发送副本」这一另拍的半边。
> 扫描件 OCR、音视频内容不在本篇范围内。
> 本篇是 doc/16 能力表 #4b「media_offload」在 v6 的方案真源。旧线实现（doc/18，`internal/mediaoffload`）
> 已随 2026-10-03 仓体翻篇删除，doc/18 降级为旧线实现存档，不作施工面；它踩过的坑在本篇第 6 节吸收。
> 关联：doc/35（附件锚点已迁到 `workspaces/<wid>/.media/`）、doc/19v6-08 第 3 节
> （`read_media_file` 声明面、多模态消费）、doc/19-v6（存储真源划分）。
>
> 核心裁定（用户 2026-10-18 拍）：**对话里面只保存 path**。字节从进系统那一刻起就只落盘；
> 对话的三份落库面里，媒体永远只是一行占位文本，占位里带 path。
>
> 结论先给一句：把「不背字节」从旧线的发送侧技巧升级成 v6 的存储纪律——盘上是唯一字节真源，
> 对话里抄的只有路径；需要模型看图的当枪发送份临时还原成 image part，落库面始终只有占位。

---

## 1. 先演一场失败

v6 今天的对话是纯文本的：用户发不了图（`postRunsRequest` 只有 `Text`，`server.go:117`）。
但模型自己能把大字节拉进对话——唯一的一条路是 `read_media_file`。按今天的代码逐步演：

1. 用户说「看一下 assets/shot.png 帮我改配色」。模型调
   `read_media_file{"path":"assets/shot.png"}`。
2. 工具读盘，返回一行 `data:image/png;base64,iVBORw0…`——2MB 的截图 base64 后约 2.7MB，
   全在这一行文本里（`filetools.go:591`）。
3. 这行文本作为工具结果进 `TurnEntry.Text`，`EndRun` 把它落进**三处**：
   `runlog_entries` 的 tool_result 行 text 列、同一行的 payload JSON（`{name, output}`）、
   `run_model_messages` 的 text 列（`store.go:235-247`、`store.go:403-409`）。
4. 下一枪 `prepareInput` 从 model_history 重建输入，这 2.7MB 原样重发给模型。
   模型按提示「再仔细看看」又调一次，对话里再多一整行。
5. 十几轮之后，窗口和账单里可能背着几十 MB 的 base64——而模型在当前文本管道下
   **根本看不见图**（19v6-08 第 3 节挂账：data URI 暂被当文本）。这些字节纯粹在烧窗口、烧钱。

病根一句话：字节已经在盘上了（workspace 里的 shot.png），对话却把它**再抄一份进自己怀里**，
从此每轮都背着。media_offload 要改的就是「抄」这一步：对话不抄字节，只抄路径。

## 2. 一条纪律，两个半边

**纪律（用户拍）**：媒体字节永远不进对话的落库面。三份落库面——`runlog_entries`、
`run_model_messages`、帧（runlive）——里与媒体有关的内容只有一行占位文本。

字节只允许活在两个地方：**盘上的文件**、**当枪发给模型的请求体**。

这条纪律与旧线 doc/18 的设计是**反的**，翻案理由要讲清：旧线是「正本不动、只改发给模型的那一份」——
因为旧线正本在进程内存里，字节留着无所谓。v6 的正本落 PG 且目标多副本：字节进了行就是
**行体永久膨胀**（备份、复制、读侧每轮都背着），「发送时再换掉」是在错误的层补救。
把纪律移到存储面之后，发送面反而获得自由：还原与否变成按需决定，不再是救火。

两个半边，各自独立成拍：

- **半边一（存储纪律）**：入口图落盘换占位、`read_media_file` 回执换占位。
  不依赖多模态，今天就能做、今天就有收益（第 1 节的失败当场断根）。
- **半边二（模型可见）**：本轮的图在发送份里还原成真图 part；历史轮只留元数据。
  2026-10-20 已由 A3-1 至 A3-4 落地：user 图与当前 run 的 `read_media_file` 回执在发送份还原，
  同一 run 的 user 图只附在第一次成功回复之前的那一枪，落库面不动。目录没标 `vision` 的模型
  和 `TAKO_MEDIA_SEND=off` 的部署都不还原。图片 token 按 DeepSeek v41 计入窗口压力（A3-5），只算这一枪实际会发送的图。

## 3. 占位行真身

所有占位（入口图、工具回执）用同一条格式：

```
[media seq=2 name="photo.png" path=workspaces/ws_01…/.media/ab12cd34….png mime=image/png size=182340B sha=ab12cd34ef56…；要看就调 read_media_file 读这个路径]
```

PDF 入口占位用同一套字段，句尾换成「要看就调 read_pdf_text 读这个路径（可分页）」。图片和音视频仍指向 `read_media_file`。

- `path` 相对租户根——就是 `read_media_file` 现有入参的形状（`filetools.go` 的
  `resolveReadMediaPath`），同一根、同一牢笼，模型拿占位就能直接调，不需要新的放行规则。
- 2026-10-20 起按 doc/35 的附件锚点：字节落在
  `<tenantRoot>/workspaces/<wid>/.media/<sha 前 16 位>.<扩展名>`，占位 path 是
  `workspaces/<wid>/.media/<sha16>.<ext>`。`GET /api/media` 和 `read_media_file` 共用
  `VisibleWorkspace`，再在租户根下的 `workspaces/<wid>/` 读；`sessions/<sid>/...`
  直接读不到。旧 `sessions/<sid>/.media/` 由启动清理，不做两套路径并存
  （细节与验收见 doc/35）。
- `mime`/`size`/`sha` 三个字段让模型**不读文件也能判断值不值得重看**（一张 30KB 的示意图
  和一张 4MB 的大图，重看的代价不同）。
- `sha` 是内容寻址：同一张图在对话里出现多少次，盘上只有一份、占位里的 path 也相同。
- 工具回执用同一套字段，不带 `seq`/`name`。句尾是「已按路径读取。媒体字节不会写入对话记录，请勿重复调用」。
  这句话告诉模型：字节不在落库的对话里。它不表示这一枪的发送份里没有图。

**通信字段（2026-10-18 用户增补拍：name 与 seq）**：

- `seq=` 全会话累计序号（按上传先后 1、2、3……），**恒在**；`name=` 用户上传时的
  原始文件名，**可省**，值以引号包裹（文件名含空格不破坏按空格分字段的读法）。
  两者合起来是模型跟用户对话的坐标：用户说「第二张」模型对 `seq=2`，说「photo.png
  的新版本」对同 name 不同 seq 的两行。盘名不受影响——同名两版是两个内容、两个
  sha 名，区分靠 `name="photo.png"` + seq 这对字段，不靠改盘名。
- 名字的分工要分清：name/seq 帮模型**说**图（引用有坐标），不帮模型**看**图
  （看得见仍靠第 4 节多模态拍把字节拼进请求）。
- **seq 的多副本来源**：占位行只由 user 行携带；postRuns 准入时数本会话 runlog user 行
  里既有的 `[media ` 占位个数、+1 起编。同会话同时只有一个 run 在写（409 忙闸），
  计数无并发竞争；闸在 PG 不在进程，多副本下照成立。
- **name 的 sanitize**：去控制字符与换行（占位行必须保持单行，换行会破坏前端逐行解析）、
  去引号（name 值以引号包裹，内嵌引号会截断解析）、截 120 字符；清完为空 → 整个
  `name=` 字段省略，不写字面空值。
- **回执变体不带 name/seq**：工具回执发生在读盘时，无从知道会话序号；模型对回执与
  入口占位靠 `path`/`sha` 对上（同一文件的这两个字段两边一致）。

## 4. 分步

### A1 入口拍（契约）

POST /api/runs 请求体加图：`images` 数组（每项 `{mime, dataBase64, name?}`）。
服务端在一笔写里完成：校验（mime 白名单、大小上限）→ 内容寻址写入
`workspaces/<wid>/.media/`（`<sha 前 16 位>.<扩展名>`）→ user 行 `Text` = 正文 + 每图一行占位。

- 为什么内容寻址：用户把同一张图连发两次，盘上一份、占位一致，不需要去重逻辑。
- 为什么落工作区不另设缓存目录：字节进 `workspaces/<wid>/.media/` 后，`read_media_file`
  的现有牢笼（`VisibleWorkspace` + `resolveInWorkspace`）天然覆盖读回路径；旧线要另建
  `.media-cache` 是因为旧线字节来源是消息内联、无处安放——v6 入口字节从第一刻起就在
  工作区里。锚点迁移细节见 doc/35。
- 准入（**2026-10-18 翻案**）：图**不折算**进 413 准入线——初版把 base64 原文喂给 cl100k
  折进 `AdmitText` 预算，两个后来的事实把它打翻：① tiktoken-go 对 MB 级 base64 是分钟级
  （300KB 实测 39s，3MB 截图≈分钟，而用户随手截图就 3MB）；② deepseek-flash 实测按张
  平价 ~384 token，cl100k 数字节差三个数量级——就算跑得动也是拒错人（文本管道下 3MB 图
  实际只发一行占位进模型窗口）。图的门槛退回硬上限（≤5MB/张、≤4 张、白名单，字节比较瞬时，
  在 `ingestImages` 里、token 准入之前）；`AdmitText` 签名去 blobs（「不折算」编译层钉死）。
  图片进窗口的 token 按 DeepSeek v41 另计，只加实际会发送的图（A3-5 已落地）。`AdmitText` 仍只数正文。usage 仍是已经发出去的请求的锚点，估算只补锚点之后新增的图。不回这套字节折算。翻案活体：3MB 图 POST 43ms 落占位行（翻案前 300KB 就要 39s）。
- 验收（curl 手打）：POST 带 1×1 PNG 的 base64 → 202 → `GET /api/sessions/{id}/log`
  的 user 行 text 里出现占位行（含 path/mime/size/sha 四字段、无 base64）→
  `workspaces/<wid>/.media/` 下出现该 sha 文件 → 再发一次同图，盘上仍只有一份。

### A2 工具回执拍

`read_media_file` 的返回从 data URI 单行**改成占位行**（第 3 节同格式）。

- 现在就改、不等 translator 的理由：当前文本管道下模型本来就把 data URI 当无意义的
  长文本看，换成占位是**零损失**；留着 data URI 则第 1 节的失败继续发生——模型每调一次，
  对话里就永久多一行几 MB 的 base64。
- 注意 v6 与旧线的分工差异：旧线的落盘动作发生在「消息内联字节 → 缓存」这一步；
  v6 里 `read_media_file` 读的文件本来就在工作区，**这一拍没有落盘动作，只有回执换字**。
  落盘只发生在 A1（用户从浏览器带来的字节）。
- A3-2 起回执句尾是「媒体字节不会写入对话记录，请勿重复调用」（`mediaReceiptPlaceholder`）。
  这句是给模型的状态说明：字节不在对话里，但下一枪发送份会把图附在 tool 消息上；
  落库的 tool_result 行、payload 与帧仍是这句占位。
- 验收（curl）：让模型读一张 100KB 的图 → `GET /log` 的 tool_result 行 text 与 payload
  output 都是占位行，不含 `base64` 字样 → 同场对话下一枪的 usage 帧 promptTokens
  不出现 MB 级跳变。

### A3 多模态拍（2026-10-20 分五拍；A3-1 至 A3-5 已落地）

发送侧包壳在新文件 `internal/runengine/media_send.go`，挂在 overflow guard **里面**
（模型调用点最内层）；它只改这一次模型调用的输入副本，存储面（`runlog_entries`、
`run_model_messages`、帧）零改动。guard 的折叠、计数与救援看到的仍是占位文本。

A3 的五拍：

| 拍 | 内容 | 状态 |
|---|---|---|
| A3-1 | 用户图发送：最后一条 user 的占位 → image part；历史 user 占位改成元数据说明行 | ✅ 2026-10-20 |
| A3-2 | 工具回执发送：最后一条 user 之后的 tool 回执 → tool 角色 image part；按 ToolCallID 只发一次 | ✅ 2026-10-20 |
| A3-3 | 第一枪闸：当前 run 的 user 图只还原第一枪，后续枪不重复背 | ✅ 2026-10-20 |
| A3-4 | 目录 `vision` 标记 + `TAKO_MEDIA_SEND` 总开关 + `deepseek-flash` 目录条目修正 | ✅ 2026-10-20 |
| A3-5 | 图片 token 计数（按 DeepSeek v41 口径进窗口） | ✅ 2026-10-20 |

**发送份形状（A3-1/A3-2）**：

- user/tool 消息变成 `Content=""` + `UserInputMultiContent`（text part + image part）。
  text 里保留 `seq/name/path/mime/size/sha`，句尾是 attached marker；
  `Content` 必须清空，否则 openai 适配器因两字段同时非空报 `ErrContentFieldsMisused`。
- attached marker 明写「像素已随本条消息（或本条工具结果）直接发送，不要调用
  `read_media_file` 或 bash 读取该文件」。2026-10-20 活体证明：只说「已附上」时，模型仍会
  照占位句去读文件，拿到「字节不在对话里」的回执后反而否认自己刚看到的像素。
- 历史 user 占位（最后一条 user 之前）换成说明行：保留 seq/name/path/mime/size/sha。
  图片和音视频写明本轮不再附像素，问当时的分析直接引用随后 assistant 的回复，明确要求重看时再调 `read_media_file`。
  PDF 另写一句：该 PDF 只在当时那条消息里出现，需要重读时调 `read_pdf_text`，不要为核对重复抽取。历史 tool 回执不还原。
- 可还原格式只 `image/png`、`image/jpeg`、`image/gif`、`image/webp`；入口白名单里的
  BMP/SVG/PDF/音视频继续留占位。读盘复用 `resolveReadMediaPath`（与 `read_media_file`
  同一把 `VisibleWorkspace` + 路径闸），再校验普通文件、≤5MB、size 与 sha 前缀一致；
  任一失败保留原占位并记 `media skip ... reason=...`，不阻断 run。
- 工具回执只在**下一枪的发送份**里还原；tool_result 的落库行、payload 与帧逐字是占位。
  A8-1 起，这条 tool 消息还要过双闸：最后一条 user 之后的 assistant 消息先收成 ToolCallID 到工具名的映射，只有映射到 `read_media_file`、且该行是「已按路径读取」回执时才还原。映射缺失不还原，也不读盘。入口 user 占位不查这句回执。细节见 A8。
  deepseek-flash 接受 tool 角色 content 数组里的 image_url，已在本机活体验证。
- 工具图去重靠 `foldBox` 的 `mediaSentTools`（已确认发过）与 `mediaPendingTools`
  （本枪已附、待确认）：每次模型调用把可还原回执记 pending；root 成功回复后在 worker
  drain 分支 `commitMediaSend()`，失败/取消/零帧不 commit，下一枪重试同一 ToolCallID。
  没有 `foldBox` 的旁路调用不去重。
- user 图的第一枪闸用同一容器的 `mediaSentUser` / `mediaPendingUser`。`restoreUserMedia`
  看到已经确认发过，就不再读盘，占位原样留在输入里。`commitMediaSend()` 把本枪附过的
  user 图和工具图一起收成已发送；这一枪没有附上 user 图时，不把 user 标志置上，读盘失败
  的下一枪还能再试。Stream 要等 worker 把流排干才知道成功，提交点只在 drain 分支；
  `Generate` 一次调用就拿到完整回复，成功返回后由 `mediaSendModel` 自己提交。
  续跑或接管会新建 `foldBox`，标志回到 false，保守再还原一次。
- **两把闸都开才还原（A3-4）**。用户把 SVG，或一张图发给目录里没标 `vision` 的模型时，
  包壳如果照旧读盘、把像素塞进请求，provider 会拒收这一枪，整场 run 变成 error。
  `TAKO_MEDIA_SEND` 回答这次部署发不发图：未设置是开，字面 `off` 才关。关的时候
  `NewWorker` 不把 `mediaRestoreHandler` 挂进 root 的模型链，模型看到的输入与占位逐字节相同，
  日志里没有 `media send`。目录字段 `vision` 回答这个模型收不收图，缺省 false。
  `pack` 按 model id 从厂商条目里取，目录没有这个 id 就是 false。`assembleRun` 把它写进
  `RunProfile.Vision`。每次调用前包壳读 `profileFrom(ctx).Vision`；ctx 里没有 profile
  （启动自检、旁路）时用装配默认值 false，不读盘。false 且输入里真有占位时，打一行
  `media skip <session>/<run>: reason=model_no_vision`，输入原样返回。纯文本不打这行。
  多张占位也只打这一行，不按张计。
- 官方 deepseek 条目的模型 id 从 `deepseek-v4-flash` 改成 `deepseek-flash`
  （2026-10-20 `GET https://api.deepseek.com/models` 只返回 `deepseek-flash` 和
  `deepseek-v4-pro`）。只给 `deepseek-flash` 标 `"vision": true`。`deepseek-v4-pro`
  没有收图实测，不标。别的厂商条目里的 `deepseek-v4-flash` 是那些厂商自己的模型 id，不动。
- 还原白名单是 png、jpeg、gif、webp。bmp、svg、pdf、音频、视频走
  `media skip reason=mime_not_restorable`，不读盘。缺文件、超过 5MB、sha 不符、
  路径越出作业根、符号链接指向根外、不是普通文件，都保留原占位并各打一行 `media skip`，
  run 继续。`media send` 只在这枪输入里出现过占位时打。行尾 `img=<n>tok`
  是这一枪实际还原成功的图，按 DeepSeek v41 加总。纯文本不打这行。800×800 是 422。
  像素少于 544×544 时计算器先放大再投影，100×100 是 184。
- 包壳只挂在 root worker 上。子代理自己的 handler 链不含它，子代理里的模型调用不还原。
  子代理折叠如果和 root 共用同一套长度估算，也会给占位计价；那只是多出来的折叠压力，
  像素仍然不进子代理的请求。
- 图片 token（A3-5）只加这一枪还会发出去的图：本 run 还没成功发过 user 图时，给最后一条
  user 计价；这条 user 之后、`ToolCallID` 还没提交的工具回执也计价。更早的 user 图和已经
  发出去的图不加。计算器读文件头上的原始宽高，发送前不先缩小。`ModelMessage.MediaTokens`
  不落库、不进帧。`countInput` 把这个数加进窗口压力，所以发送前准备、回合中折叠、溢出守卫
  都能看见这张图。计价要求目录 `vision: true` 并且 `provider` 是 `deepseek`；别的视觉模型
  没有计算器，这一行是 0。`AdmitText` 仍只数正文。厂商 usage 仍是已经完成的请求的锚点，
  锚点之后的新行才补还没发出去的图。占位写完之后若有人换掉文件，发送侧因 sha 不符跳过，
  计数器仍可能按缓存里的原尺寸收费，单张最多 1024；usage 仍是已经发出去的那一枪的实情。

**验收（2026-10-20 活体，dev 库）**：

- A3-1：首轮红图（发图后只回复 ok）→ complete；同会话纯文本问「刚才那张图什么颜色」→
  assistant 答出颜色，本 run 零 `media send`；首轮 user 行仍是占位，PG 两条
  `LIKE '%base64%'` 均为 0。
- A3-2：另一条纯文本 run 让模型按占位 path 调 `read_media_file` → complete，assistant
  答「红色」，日志 `media send ... user=0 tool=1`；同一 run 再调 `get_time`，后续枪不再
  出现同一 ToolCallID 的 `tool=1`；`GET /api/runs/<id>/frames` 的 tool_result output 仍是
  占位、无 base64；PG 零 base64。
- A3-3：同一会话再发一张红图，并要求第一次回复同时调用 `get_time` 和 `read_media_file`。
  run `r_01M5C64DM13Q7WXC8E7HR80Y52` complete，assistant「红色」。日志两行：
  `media send … user=1 tool=0 skipped=0`，随后 `media send … user=0 tool=1 skipped=0`。
  `GET /api/runs/<rid>/frames` 里 `read_media_file` 的 tool_result 仍是占位；`/log` 的 user 行
  和 tool_result 行都没有 base64。PG 两条 `LIKE '%base64%'` 均为 0。
  前一条 run `r_01M5C60364VYQYEP5KWF9YMXRM` 里模型只调了 `get_time` 就收束：首枪 `user=1`，
  下一枪没有 `media send` 行。user 图已经提交，`get_time` 回执没有图，`seen=0` 就不打行。
- A3-4：SVG 入口 run `r_01M5C71732H6Z2Y28ZT4EEEM11` complete。首枪日志
  `media send … user=0 tool=0 skipped=1`，并有 `media skip … reason=mime_not_restorable`。
  模型随后调了 `read_media_file`，回执仍是 SVG 占位，后面几枪 `user=0 skipped=2`（用户占位和回执都不可还原），run 没有因此 error。user 行仍是占位。
  缺文件 run `r_01M5C724QPRPTQG4M8JPZ5J3SE` complete，tool_result 是「文件不存在：nope.png」，没有崩栈。
  `TAKO_MEDIA_SEND=off` 重启后红图 run `r_01M5C73NMHRRJCBM3VGN099EE4` complete，引擎日志只有启动行 `media: send=false`，没有 `media send` 也没有 `media skip`；assistant 答「看不到」。
  这三场 PG `runlog_entries` 与 `run_model_messages` 的 `LIKE '%base64%'` 都是 0。
- A3-5：临时引擎 `127.0.0.1:8907`，`-compact-window 940`（limit 752），workspace `/tmp/a35-ws`，
  模型 `deepseek/deepseek-flash`。800×800 红图 run `r_01M5C8Y5115EBGZBYBCH045WYC`
  （session `s_01M5C8Y50YGFGCNP476S8R48KZ`）complete。日志
  `media send s_01M5C8Y50YGFGCNP476S8R48KZ/r_01M5C8Y5115EBGZBYBCH045WYC: user=1 tool=0 skipped=0 img=422tok`。
  assistant「暗红色（深红偏绯红）」。100×100 红图 run `r_01M5C8Y70SZ2FZMA4DWCK51ND0`
  （session `s_01M5C8Y70QQJJP3H5Y2RGN2CV5`）complete，日志 `img=184tok`，assistant「红色」。
  同一会话先发热身 run `r_01M5C8Y917BHZ3W02M599GNN1G`（session `s_01M5C8Y916QM7X8PV4D6BNXZ1D`），
  估算 260，没有「进入折叠」，assistant「好」。再发文本加同一张 800×800 图，
  run `r_01M5C8YB12HDMVSVYGRBPJY0JJ` complete。进模型前日志
  `compact s_01M5C8Y916QM7X8PV4D6BNXZ1D/r_01M5C8YB12HDMVSVYGRBPJY0JJ: 输入 814 tok > limit 752，进入折叠`，
  折完 `发送输入 715 tok < limit 752`，随后 `media send … user=1 tool=0 skipped=0 img=422tok`，assistant「红」。
  814 − 422 = 392，低于 752，过线的是这张图的 422。四场两表 `LIKE '%base64%'` 都是 0，占位行 3 条。
  940 这个窗口小于工具声明的体积：热身那一枪厂商 `prompt_tokens` 3445，溢出守卫打了「实数超窗」，
  回复已经落库。加图那一枪回复之后厂商 `prompt_tokens` 3982，守卫又强折一次备下轮。
  `countInput` 不计工具声明，折叠触发线看的是估算。
- 单测覆盖解析、最后一条 user 与其后 tool 才还原、历史边界、读盘失败、召回标签边界、
  工具图 commit 后不重复、user 图只在成功回复后算已发、guard 重试共用同一条 pending、回执金样、
  SVG/BMP/PDF 不还原、缺文件、超上限、sha 不符、`../` 与 symlink 越界、非普通文件、
  `Vision=false` 原样返回并打一行 `model_no_vision`、`TAKO_MEDIA_SEND=off` 不挂包壳、
  目录 `deepseek/deepseek-flash` 的 `vision` 为 true 而 `deepseek-v4-pro` 为 false，
  以及 A3-5 的 v41 向量、四格式读头、`countInput` 加 `MediaTokens`、只给未发送的图标价、
  `media send` 行 `img=422tok`；
  全量 `go test ./internal/runengine` 26.0s 绿。

### A4 前端拍

占位行在页面渲染成图（气泡里直接看图），前端需要一个取文件的 GET 端点
（牢笼校验后按 path 回流）。端点形状随 A1 的契约拍一起定。

### A6 通信把手拍（2026-10-18 增补，拍号沿 tmp/32 todos 编号）

请求体 `images` 每项加可选 `name`（用户上传文件名）；占位行入口变体加
`name=`/`seq=` 两列（第 3 节通信字段小节）。落库列零改动（占位仍是 text）。

- 改动三处：`postImage` 加 `Name` 字段 → postRuns 在 ingest 前数会话既有占位得 seq 起点
  （`pgstore.CountMediaPlaceholders`，第 3 节的多副本来源）→ `mediaPlaceholder` 加两列
  （实现序：seq 在前、name 引号包裹）；name sanitize 同节。
- 验收（curl 手打，✅ 2026-10-18 实测全过）：①POST 带 `name:"photo.png"` → user 行占位
  `[media seq=1 name="photo.png" …]`；②同会话再传另一张同名图 → `seq=2`、盘上两个 sha 文件；
  ③不带 name → 占位无 `name=`、seq 照排；④name 含换行+引号 → sanitize 成单行 `badnamex.png`；
  ⑤同内容重传 → 盘同文件、占位照发 seq 照涨（身份按出现次数、存储按内容）；
  PG 复核 runlog 仍零 base64。
- 旧会话兼容：存量占位行无 name/seq 两列——前端解析按「缺字段省略」处理，
  不做迁移（历史行本来就是只读事实）。

### A7 前端名显拍（2026-10-18 增补）

`web/` 的占位行解析正则扩 `seq`/`name`（三捕获组全可缺省容忍：新行、存量行、工具回执
变体三种形状同一把正则）；气泡 `<img>` 的 alt/title 用 name，文件片显示 name，
刷新 reconcile 后附件真名仍显示（修掉「刷新后文件名丢失」的现存缺口）。

- 验收（浏览器，✅ 2026-10-18 实测全过）：发带名图 → 气泡图 alt=name 且真渲染
  （60×60 经 /api/media 回流）；刷新 reconcile 后 alt 仍是 name；无名图 alt 退化
  「图片 {seq}」；回执行（无 name/seq）alt 退化 mime+size。
- 施工实录：第一版正则把 name 写在 seq 前、后端是 seq 在前——用户气泡不出图、
  占位行却从正文消失（剥了媒体、丢了引用），按行为当场定位改正则。
  「剥了却没渲染」正是解析失败的指纹：媒体被正则吃掉、media 列表却为空的反面
  不成立——display 剥行与 media 提取是同一次 exec，二者只能同真同假。

### A8 PDF 文本读取（2026-10-20；A8-1 与 A8-2 已施工并活体验收）

用户把一份 40 页的报告 PDF 发进会话，问第 3 页的结论。字节已经落在 `workspaces/<wid>/.media/<sha16>.pdf`，user 行多了一行占位。模型若调 `read_media_file`，拿到的仍是一行元信息。PDF 也不在可还原成图片的四种格式里，发送侧不会把它变成 image part。模型只能说自己看不到内容。上传成功和可读之间断了一环。

A8-1 补文本层这一环。模型按占位 path 调 `read_pdf_text`，工具按页抽出文字，作为普通工具结果写入 runlog 和 `run_model_messages`。扫描件返回正常说明，run 继续。音视频不处理，也不做 OCR。

链路：用户 POST 带 PDF → `postRuns` 落盘并写占位 → 模型看到句尾，调用 `read_pdf_text` → `resolveReadMediaPath` 把 path 收进工作区 → `pdftext.go` 逐页抽文本 → 工具结果落库 → 下一枪模型读到页文本并回答。图片回执链在旁边：只有模型自己调了 `read_media_file`，下一枪才把图附上。引擎不因为历史里还有一张图就重发。

**工具契约。** `read_pdf_text` 声明 read，父池排在 `read_media_file` 后面。`path` 必填，与 `read_media_file` 共用 `resolveReadMediaPath`。解析成功后还要是普通文件、扩展名 `.pdf`、不超过 5MB，否则返回工具错误。`page_from` 可省，默认 1。`page_to` 可省，默认从起点起共 3 页，并夹到总页数。单次最多 10 页；请求超过 10 页而且这个窗口后面还有没读的页时，页眉写「单次最多 10 页，已夹取」。

成功时先给一行 `[pdf path=… pages=N；第 a-b 页；文本层抽取]`，再按页写 `--- page N ---` 和该页文本。空页写「本页无文本层」。单页解析失败写「本页无法抽取」，其他页继续。单次正文上限 16KB，超过时在 rune 边界截断，末尾写「（本页文本超过单次上限，已截断；请缩小页范围）」。`page_from` 超过总页数时返回 `[pdf … pages=N；page_from 超出范围]`，这是正常结果。请求范围内每一页都没有文字时返回 `[pdf … pages=N；未检测到文本层（可能是扫描件），无法抽取文本；可让用户上传页面截图]`，也是正常结果。加密、损坏、上下文已取消返回 Go error，`wrapToolErrors` 收成「错误：…」，run 继续。

抽字在 `pdftext.go`，库是 `github.com/ledongthuc/pdf`（pin `v0.0.0-20260907135840-6c8c28e0e8a0`，BSD-3，纯 Go）。字形按 Y 分行、按 X 从左到右排。相邻字形的空隙超过左侧字号的 0.2 倍，并且两侧都是 ASCII 字母或数字时，才补一个空格。中文两侧不补。不承诺版式还原，也不做 CJK 兼容字形归一，不另存派生文本。成功返回后引擎打一行 `pdf read <sid>/<run>: pages=a-b chars=…`，chars 是写进正文的页文本 rune 数。

**占位与历史行按 mime 分流。** PDF 入口占位句尾是「要看就调 read_pdf_text 读这个路径（可分页）」。图片和音视频仍指向 `read_media_file`。历史 PDF 行不说「不再附带像素」。图片历史行点名 `read_media_file`，和「重看由模型自己调工具」对齐。`read_media_file` 的回执句尾仍是「已按路径读取」，不改成指向 `read_pdf_text`。

**回执双闸。** PDF 正文可以逐字印出「已按路径读取」，再带上一行完整的 `[media …]`。发送侧若只按行匹配，就会去读那行里的 path，把用户没点名的图附进下一枪。`restoreToolMedia` 先收集最后一条 user 之后 assistant 消息里的 ToolCallID 和工具名。一条 tool 消息要还原，映射必须是 `read_media_file`，并且该行的 `receipt` 为真。两个条件缺一个就不还原，映射缺失也不读盘。入口 user 占位不查回执句，否则第一次发图的占位会被自己挡掉。伪造行放在 `read_text_file` 结果中间时不还原；同一行放在真正的 `read_media_file` 结果里才还原。

researcher 走排除路，排除清单仍只有 bash，所以它的有效名单多了 `read_pdf_text`。coder 的白名单不含这把。

**不做。** 扫描件 OCR、音视频内容、把抽出的文本改成只进当枪发送副本。扫描件只返回「未检测到文本层」，音视频仍走 `read_media_file` 的元信息回执。

**A8-1 活体验收（2026-10-20）。** 临时引擎 `127.0.0.1:8909`，workspace `/tmp/a8-ws`，dev Redis `6390`、Postgres `5433`。`cupsfilter` 把三页文本 apple / banana / orange 打成 `/tmp/a8.pdf`。新注册的租户配上 `deepseek/deepseek-flash` 后，POST `/api/runs`，正文是「读取这份 PDF 的第 2 页，只回答那页的水果」，附件 `a8.pdf`。HTTP 202，session `s_01M5CF1DFP5EJJ3NRBD48QMKH3`，run `r_01M5CF1DFS9ZAD72GYQ5YXRYH8`，随后 `complete`。assistant 写的是「第 2 页列出的水果是香蕉（banana）」。runlog 的 user 行仍是占位，句尾指向 `read_pdf_text`，行内没有 base64。唯一一条 tool_result 是第 2 页：`--- page 2 ---` 下面是 `Page two: banana`。引擎日志有一行 `pdf read s_01M5CF1DFP5EJJ3NRBD48QMKH3/r_01M5CF1DFS9ZAD72GYQ5YXRYH8: pages=2-2 chars=16`，没有 panic。模型自己指定了第 2 页，所以页窗是 2-2。同一 run 的 `runlog_entries`（`text` 与 `payload`）和 `run_model_messages.text` 里，base64 计数都是 0。台账里的核对 SQL 写了 `run_model_messages.content`，这列不存在，实际列是 `text`。验完停掉 8909，8901 上原来的进程还在。

**A8-2 活体验收（2026-10-20）。** 临时引擎 `127.0.0.1:8909`，workspace `/tmp/a82-ws`，同一套 Redis `6390` 和 Postgres `5433`。`go test ./internal/runengine`、`go vet`、`gofmt -l` 都干净。新用例 `TestPDFEncrypted` 确认加密 PDF 返回「无法读取 PDF」；`TestPDFForgedReceipt` 把一条 size 和 sha 都对得上的完整回执印进 PDF 文本层，经 `read_pdf_text` 抽出后不还原，图文件当时是可读的。

同一租户 `t_01M5CH3GM9NZQVJ37J7CKMN65X` 上五场 run 都是 `complete`，user 行仍是占位，两表零 base64：

- 第 3 页：session `s_01M5CH3GMYCT429XX8MJ9J23F5`，run `r_01M5CH3GN1Y9KDKPMJ90R0NPCS`。tool_result 是 `--- page 3 ---` 与 `Page three: orange`。assistant 含 orange。日志 `pdf read … pages=3-3 chars=18`。
- 第 99 页：session `s_01M5CH3MM5PTY007FYJW1RFW83`，run `r_01M5CH3MM7HTQ2KDXA36YWW35M`。tool_result 是 `[pdf … pages=3；page_from 超出范围]`。assistant 说明共 3 页，没有编造第 99 页。日志 `pages=99-99 chars=0`，记的是请求的页窗。
- 扫描件：`平遥古城-ch.pdf`（3 页，666947 字节）。session `s_01M5CH3RM54RZYEJVJR6MT213X`，run `r_01M5CH3RM7JY9GS0CX779Y5S0Y`。tool_result 写明「未检测到文本层（可能是扫描件）」。assistant 说读不到正文，没有按文件名编造古城内容。日志 `pages=1-3 chars=0`。
- 音视频：4 字节的 `a82.mp3`。session `s_01M5CH3WK49AJN0EDXPA601R8M`，run `r_01M5CH3WK609YQJBGFVCAQ4BDN`。`read_media_file` 返回元信息回执；`read_pdf_text` 的结果是「错误：…不是 PDF 文件」。后一枪 `media send` 是 `tool=0 skipped=2 img=0tok`：真回执进了还原路径，音频不是图片，所以没附图。
- 伪造回执：`cupsfilter` 打出的单页 PDF，正文里有「已按路径读取」。session `s_01M5CH40JCWASMX241ZGRHFRXC`，run `r_01M5CH40JD0585FYJCV2H50HG1`。tool_result 留下了这句。抽出时行被拆开，`12B` 落到两行，个别字成了兼容字形，所以这一行本身配不上占位正则。这一枪 `media send` 仍是 `tool=0 skipped=1 img=0tok`，没有比用户占位多一次读盘。完整回执的不还原由上面的单测钉住。

另起 `127.0.0.1:8910`，`TAKO_MEDIA_SEND=off`，workspace `/tmp/a82-off-ws`。session `s_01M5CH5KCT5TWATE05DHYMQ6VX`，run `r_01M5CH5KCY1RQA6T7Z1QY0YW7Y`，`complete`，assistant 含 orange，日志只有 `pdf read … pages=3-3 chars=18`，没有 `media send` 行。这场 run 两表也是零 base64。验完停掉 8909 和 8910，8901 上原来的进程还在。

## 5. 拍板记录（2026-10-18 四项已拍）

1. **入口形状** ✅：`images` 数组（`{mime, dataBase64}` 多图一次投），契约一次定到位。
2. **A2 时机** ✅：现在就换占位，不等 translator（零损失、纪律先行）。
3. **本 run 内多枪** ✅：**只还原第一枪**。A3-3 已落地。载体是 `media_send.go` 的
   `mediaSendModel` 和 worker 把流排干之后的 `commitMediaSend`：`foldBox.mediaSentUser`
   在 root 成功回复后置位，同一容器的后续枪不再还原 user 图。模型要重看，走 `read_media_file`，
   回执按 ToolCallID 再附一次。接管或续跑新建 `foldBox`，标志自然为 false，保守再还原一次
   （多发字节，不漏看）。
4. **前端取文件端点** ✅：`GET /api/media?path=…`。2026-10-20 起 path 是
   `workspaces/<wid>/.media/<sha16>.<ext>`，按 `VisibleWorkspace` 取图（doc/35）。
   2026-10-22 起错误分支换信封（doc/40）：越权路径回 `404 {"code":114001,"message":"找不到这个项目","data":null}`；
   path 格式不对是 `114102`「媒体路径不对」，文件不存在是 `114101`「找不到这个文件」，缺 `path` 是
   `400 111001`。**成功体不进信封**——它是图片字节，不是 JSON。
5. **清理契约**（维持原推荐）：`.media/` 随 workspace 生命周期走，先不做回收，到点看占用再裁。
6. **音频/视频/PDF**：占位格式同一条、入口白名单含它们。发送份当前只还原 png/jpeg/gif/webp 四种栅格图。
   PDF 的文本层走 `read_pdf_text`（A8-1），不把 PDF 字节当图片发送。音视频仍只有元信息占位。
7. **能力标记** ✅：目录每条模型的 `vision` 缺省 false。它回答这个模型收不收图。
   官方 `deepseek-flash` 标 true，`deepseek-v4-pro` 不标。
8. **部署总闸** ✅：`TAKO_MEDIA_SEND` 默认开，字面 `off` 才关。它回答这次部署发不发图。
   与第 7 条都真，包壳才把占位还原成图。

## 6. doc/18 的教训，哪些还有效

- 旧线 bug「同一轮 `read_file` 重读后随即又被换占位」（`doc/log/2026-09-30-uncommitted-review-handoff.md` 问题①）：
  根因是旧线靠「user 消息条数」启发式判「本轮」，条数一变 seen 就乱。v6 的判定坐标是
  结构化的：最后一条 user 之后才是当前 run 的 tool 回执，工具图按 ToolCallID 记账；
  这一类 bug **没有存在的土壤**。
- 旧线 bug「压缩后 user 条数下降导致 seen 不重置」（同 handoff 问题②）：同上，启发式已被
  结构化坐标与 `foldBox` 的 per-run 记账取代。
- 缓存清理契约（同 handoff 问题③-⑤）：v6 下降级为 workspace 生命周期问题，见待拍⑤。

## 7. 所以这解决了什么

第 1 节那场失败里，用户损失的是窗口和账单：模型每看一眼图，对话就永久多背 2.7MB，
十轮下来光图就占掉半个窗口，而模型全程没真正看见过图。本方案把它换成三件确定性的事：
**字节只存一份**（工作区、内容寻址）；**对话只抄路径**（三份落库面零字节，
历史和备份不再膨胀）；**看图变成明确的当枪发送副本**——A3-1/A3-2 已让当前 user 图与
`read_media_file` 回执在下一枪带上 image part，模型真的看得见，而落库面仍只有占位。
多副本目标栈下，这条纪律同时把「PG 行体膨胀」和「每轮重发大字节」两个账一起结了。
A3-3 把同一张 user 图收成「本 run 成功回复之后不再重复发送」；模型要重看，走工具回执那一条。
A3-5 把这一枪还会发出去的图片按 DeepSeek v41 加进窗口压力：800×800 是 422，像素不足 544×544 的 100×100 先放大再投影，是 184。
发送前准备看见超限就折叠。活体用 `-compact-window 940`（limit 752），加图那一枪估算 814，其中 422 来自这张图，剩下 392 低于触发线。
更早的 user 图和已经发出去的图不加。`AdmitText` 仍只数正文。厂商 usage 仍是已经完成的请求的锚点，估算只补锚点之后还没发出去的图。
A8-1 让 PDF 的文本层按页进入工具结果，user 行仍只有占位；图片要再次进入请求，仍得模型自己调 `read_media_file`。
