# Super-agent 设计文章（转载摘录）

> 性质：参考资料，不是方案真源。
> 原文：《Exploring Super-agent Design: Towards a More Autonomous Agent System》，
> DeepSeek Harness 团队，2026-10-31，微信公众号首发（页面标 20:29 北京时间）。
> **它不是 arXiv 论文**，没有 DOI / abs 页。知乎专栏转载 `https://zhuanlan.zhihu.com/p/2099987320443237060` 返回 403，抓不到。
> 本文件取自 APPSO 转载（53ai 镜像，2026-10-31 22:03），该页正文末尾约 30% 需登录，未获取。
> 抓取日期：2026-11-15。

## 来源

- 转载正文：https://www.53ai.com/news/LargeLanguageModel/2026103145736.html
- 开源原型：https://github.com/deepseek-ai/dsh-superagent-prototype （MIT，单次提交 6337fc8）
- 本地源码快照：`/Users/yuweikang/dev/ykcode/ref-agent/dsh-superagent-prototype`
- 量子位报道：https://www.qbitai.com/2026/10/506904.html

## 问题定义

模型已能独立完成单任务，但人同时只能跟进 3–5 个活跃 Session。单任务上限取决于模型，
多任务上限取决于人的注意力。现有 Agent 以 Session 为基本单位、彼此独立，协调工作回到人手上。

四项能力：跨会话连续交互、多任务高效并行、主动响应环境事件、长期在线与异常恢复。

金句：**Session 应当是系统的实现细节，而不应是用户的认知负担。**

## 五个模块

### 1 双层架构（Router / Worker）

Router 是常驻全局协调节点，统一接收群聊 / GitHub / 邮件等平台信息，判断分配、维护跨任务上下文；
官方原话说它「甚至可以不配备 Bash 工具」，配较轻量模型。Worker 上下文隔离、挂全套工具、
通常需要能力最强的模型，不直接对用户说话，出口统一在 Router。

与 Agent Team 的区别：Team Lead 要钻进任务细节、推理负载重；Router 只处理任务之间的高层关联。

订阅直通：Worker 接手一个 GitHub PR 后直接订阅后续评论，修改意见直达它，不回 Router 绕一圈。
Router 只管新任务冷启动与跨任务协调。

### 2 事件驱动（Trigger / Thread）

群聊消息、邮件、日程提醒、GitHub 评论、CI 结果都可成为 Trigger。
两级过滤：程序规则快速排除无关消息（Fast Path），再交给大模型结合上下文判断（Slow Path）。

投机执行：内部用户群配了「静默复现并修复」策略——有人反馈 Bug 时后台先复现、分析、找修复；
只有确认问题存在且找到相对简单的修复才提交给开发人员，否则全程安静。

### 3 面向人的交互

只在有人必须行动、或能用上结果时才说话；一次只问一个具体问题；按影响停下而不是按工作量；
草稿标 unfinished，失败报失败。IM 单次回复不超过 100 词（写在原型 `docs/004-prompts.md`）。

运营红线（`docs/000-philosophy.md`）：不要用它刷量——批量 PR、广播消息、伪工作给团队的比拿走的少。
核心原则：**Take as little human attention as possible, and give as much value as possible.**

### 4 计算状态与执行环境分离

异步 ReAct Loop：耗时工具调用转后台，前台会话继续收消息，完成后再送回结果。
该机制已在 DeepSeek Harness 实现，但尚未默认开启。

存算分离：执行循环在云端、会话状态在数据库，代码仓库 / 文件 / 工具链可留在用户设备；
本地只部署轻量 daemon，接收云端命令并返回结果，不保存会话状态。设备离线后重新上线可补做。
支持 BYOM（Bring Your Own Machine）与云端托管机器。

### 5 Watchdog

独立的 AI 监督员，检查任务是否有进展、是否陷入无效循环，发现异常尝试修复。
**文章只在模块清单里挂了名字，没有展开判定逻辑与兜底；开源原型里也没有对应实现。**

## 三组实验数据（官方自测，合成负载，Worker 用 Mock）

环境：群聊 + GitHub CI 消息流，单次约 300 条消息、其中约 20 条工作请求；编码动作用只消耗时间的工具代替。
每种方案重复 5 次。

| 方案 | p50 | p95 |
|---|---|---|
| Super-agent | 12.4 分钟 | 15.7 分钟 |
| 单 Session + Subagent（中性提示词） | — | 29.5 分钟 |

差异来自调度：中性提示下 5 次运行模型 0 次调用 Subagent，所有工作堆在主 Session 被新消息插队；
改成优先委派的提示词后 5 次里 4 次委派。

拆掉事件直通：完成时间几乎不动，Router 决策次数 +30%，读缓存 token 从 2323 万涨到 3177 万（+37%）。

压测：Trigger 速率 1 → 40 条/分钟，Router 单请求耗时 2.5s → 3.9s，单个决定分摊推理时间 6.4s → 1.6s。
机制是批处理：上一轮推理期间到达的事件积压到下一轮一并注入，batch 从 1.0 到 2.8。

官方自注：仅作初步参考；多会话的读缓存 token 天然高于单 Session，是「以更多 token 换更短且更稳定的完成时间」。

## 经典概念的对应（官方结语：没有提出全新理论）

| 经典思想 | 在 Super-agent 里的位置 |
|---|---|
| Fast Path / Slow Path | 程序规则先筛噪音，LLM 只判候选 |
| Cache + Inline & Spill | 上下文窗口当缓存管，决定什么留窗口、什么换出 |
| 引用图 / 多跳展开 | Trigger、Thread、Worker 各赋唯一 ID，按需展开（官方叫 Chain of Recall） |
| Speculative Execution | 群里还在讨论，后台已提修复 PR |
| 存算分离 | 云端执行循环 + 端侧轻量 daemon |
| Control Loop | Watchdog |

第 5.2 节对齐 Anthropic 的「脑手分离」（Decoupling the brain from the hands），差别在场景：
Anthropic 讲托管服务按需创建沙箱；这里要接的是可能间歇在线、有未提交修改与本地凭据的个人机器。

## 四个开放问题（第 8 节）

1. 双层架构怎么扩展：Router 何时水平切分、Worker 之间何时需要直接通信、给 Router 的事件判别与批量分派做专项训练。
2. 脱离 Session 之后人机协作怎么设计——把交互锚在聊天窗口上更像路径依赖的结果。
3. 上下文边界与 A2A：组织怎么配备 Super-agent 本质是在划分上下文共享与隔离的边界。
4. 安全与隐私：云端 Agent Loop 一旦被入侵或提示词注入攻破，其连接的全部端侧设备都面临被控制的风险；
   即使没被攻破，端侧凭据若能进模型上下文，正常执行中也有泄露风险——要让 Agent 在完全不接触凭据本身的情况下用凭据。

## 与开源原型的关系

README 首段：研究原型，非官方产品；发布版做过改编，
「Some designs discussed in the article are directions for future development and are not implemented in this prototype.」
拿着架构图去找对应实现会找不到。