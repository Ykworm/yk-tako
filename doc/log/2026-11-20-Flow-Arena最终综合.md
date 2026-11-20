# Flow arena 最终综合记录

> 性质：评审与综合记录，不是施工真源。
> 日期：2026-11-20。
> 输入：[85](../85-动态工作流Flow.md)、[86-GPT](../86-动态工作流Flow-GPT.md)、[86-ds](../86-动态工作流Flow-ds.md)。
> 结果：最终 base = **85**；唯一候选落地真源是 [doc/85](../85-动态工作流Flow.md)，仍为待点头、未施工。两份 86 保留候选记录，不作施工真源。
> 验证边界：本轮没有再次生成候选，没有新增外部交叉评审，也没有运行 Node、模型、沙箱或引擎。用户已确认 86-GPT 的先前 arena 与交叉评审真实完成；本记录是在最新简化需求上进行最终 Pick、Graft、Verify。

## 1. 为什么本轮不是直接采用 86-GPT

86-GPT 的 §18 已记录上一轮三候选、交叉评审、选 base 与嫁接，双方当时选 C，强在整轮求值、副作用处理和完整故障矩阵。那是有效的历史输入，但其需求仍包含 revision、inputHash、自动资源终止和 Flow 专属文件发布替换。

本轮之前，用户已把产品模型进一步收敛为：一次 Flow 一份不可变 JS、历史 append-only、无在线脚本编辑、无 revision/inputHash、无 JS 时间/步数自动判死，并要求持久执行历史、心跳和用户停止。因此本轮重新按最新需求评分，而不是照搬上一轮名次。

## 2. 评分标准与结果

每项 0–5，是设计审查判断，不是运行成绩。

| 维度 | 85 | 86-GPT | 86-ds | 判断依据 |
|---|---:|---:|---:|---|
| 最新产品契约匹配 | 5 | 2 | 2 | 85 原生是不可变脚本、无 revision/inputHash、执行历史与心跳；两份 86 仍以 revision 和自动资源终止为主 |
| 动态表达与宿主面 | 5 | 5 | 3 | 三者都保留 JS 动态计划；85/GPT 使用现代 JS 与小宿主面，ds 的语法和 async 能力依赖未验证的纯 Go 引擎子集 |
| 并发与崩溃重放正确性 | 4 | 5 | 2 | GPT 的整轮求值历史最完整；85 以 callSeq + deliverySeq 覆盖结果可见顺序；ds 只有调用序号，未解决 B 先交付导致后续分支变化 |
| 副作用、权限与多副本 | 3 | 5 | 3 | GPT 对未知结果、容量、文件现场与发布最完整；85 原先只有一般中断纪律，需移植 |
| 维护成本与运行时风险 | 4 | 2 | 3 | 85 的 Node 是明确依赖但机制较直接；GPT 增加 QuickJS-WASM、整轮协议和六表；ds 去掉 Node 却引入未验证的 async/微任务驱动风险 |
| 交付与验收完整度 | 4 | 5 | 4 | GPT 的故障矩阵最广；85 已有完整 API/页面/双副本/kill 线，移植后补足关键缺口；ds 另有引擎 PoC 前置风险 |
| 合计 | 25 | 24 | 17 | 不是按总分机械决定；最终决定还看最新需求与维护边界 |

86-GPT 在通用可靠性上最强，但整份采用会重新引入用户已经明确删除的 revision/inputHash、自动 JS 资源终止，以及 QuickJS-WASM 和文件发布替换的额外范围。85 保留同一个动态工作流心智模型，且需要移植的缺口可以局部补上，所以选 85。

## 3. 最终移植项

### 从 86-GPT 移植

1. **未知副作用先处理再恢复子模型**：有工具 intent 无 result 时，程序先进入 `needs_review/outcome_unknown`；只能补交可验证证据或放弃调用，解决后才沿原 run、epoch+1 继续。不是先让模型恢复，再靠提示词要求它别重做。
2. **严格结果契约**：schema 由 Go 程序校验；helper 返回显式成功/失败对象，合法 `value:null` 不再与失败混淆；未等待调用、未处理 rejection 和运行中后台命令使 return 回 `unjoined_calls`。
3. **父/子容量保护**：同步命名 Flow Agent 的父 run 占容量时，一个保留位绑定整个 Flow，跨连续子调用、调用间隙、暂停、needs_review 和父 detached 持有；子调用终态只把 in_use 翻回 held，不归还全局池，最终收拢或停止后才释放。
4. **显式求值轮次**：最小 Node 反例证明 deliverySeq 不足后，最终稿补成一轮一个结果输入、排空 Promise 作业、整批命令事务提交、确认后再下一轮；恢复逐轮比较 settlement 与完整输出。采用协议思想，不搬 QuickJS-WASM 和六表。

### 从 86-ds 移植

- **固定运行时前置能力验证**：施工包获批后的第一道门槛验证 Node async/Promise、协议、重放、心跳、进程组停止、资源约束与两个 OS 沙箱；失败就回方案修订，不静默换执行器或无沙箱降级。
- 保留两者共同强调的**小宿主面**：只有 agent 是真执行动词，parallel/pipeline 在 runner 内组合。

### 已在 85 中保留并加强

- 不可变脚本与 args；
- 同一 Flow 内 append-only 动态计划；
- callSeq 标识调用，flow_turns 的 turnSeq + settlement + 整批 commands 保存实际求值路径；
- `flow_events` 持久执行历史；
- 心跳、unresponsive 与用户停止；
- 不按 JS 执行时间或步数自动判死，但保留进程、Agent、并发与费用上限。

## 4. 明确拒绝的移植

- **QuickJS-WASM + 六表完整方案**：会改变运行时并显著扩大存储面；最终稿只采用必要的求值轮次语义，以 Node + flow_runs/calls/turns/events 四表表达。固定 Node 能否可靠判定每轮 Promise 作业排空，仍是 §14.0 的实测门槛，不能预称已验证。
- **纯 Go 内嵌 JS 引擎**：无 Node 是优势，但 async 挂起/恢复与宿主微任务泵仍未证实，语法子集也会增加模型改写成本。
- **revision、inputHash、在线编辑、前缀复用与后缀失效**：与用户最新拍定的不可变脚本和 append-only 历史冲突。
- **Flow 专属 worktree、B/E/N 文件发布替换**：是独立的大问题，不借本次综合把施工面扩大到另一套发布系统；第一版文件操作继续走普通 Agent 现有权限与写入保护。
- **按 JS 时间/步数自动判死**：最终使用执行历史、心跳、用户停止和资源约束。
- **释放父运行槽来让子执行**：会改变全仓容量定义；最终采用预留一个子位。

## 5. 复核发现的缺口与修正

另一轮只读评审指出 deliverySeq 不足后，本轮在 Node v22.23.2 独立复现了最小反例：同样按 B→A 交付结果，同一 Promise 检查点得到 `repairA→repairB`，分检查点得到 `repairB→repairA`。因此旧结论“按原投递顺序即可闭合”已撤回，85 改为显式求值轮次，并把该反例纳入验收。

容量复核也成立：若每个子调用终态就释放保留位，连续调用之间仍可能被其他父 run 抢走容量。85 现将保留位绑定整个同步 Flow，并明确 held→in_use→held、暂停、父 detached、失租与最终释放生命周期。

指出的 schema 示例问题不成立：85 当前已明确 `items: {type:"string"}`，files 及其元素都不是可空值，未做无谓修改。

## 6. 可复核文档检查与验证边界

此前“10 项存在、5 项不存在”只有一次性内联命令，没有留下复核入口。现将本轮更新后的命令完整保留如下。它只检查文档关键词和候选边界，**不是机制正确性测试，也不是持久验收资产**。

在仓库根执行：

```bash
python3 - <<'PY'
from pathlib import Path
root = Path.cwd()
p = (root / 'doc/85-动态工作流Flow.md').read_text()
required = [
    '一次 Flow 一份不可变 JS',
    'flow_turns',
    'evaluate {turnSeq',
    'committed {turnSeq}',
    'Promise 作业',
    'needs_review/replay_mismatch',
    'child_capacity_state',
    'held 在子调用之间',
    'paused、needs_review',
    'outcome_unknown',
    'FlowCallOutcome',
    'unjoined_calls',
    '不设置 JS 执行时间或执行步数自动终止',
]
for token in required:
    assert token in p, f'缺少最终契约：{token}'
forbidden = [
    'deliverySeq', 'delivery_seq',
    'POST /api/flows/:id/revisions',
    'flow_script_revisions', 'input_hash', 'reused_from_call_id',
    '新 revision 从头重放',
]
for token in forbidden:
    assert token not in p, f'残留已否决设计：{token}'
for name in ['doc/86-动态工作流Flow-GPT.md', 'doc/86-动态工作流Flow-ds.md']:
    text = (root / name).read_text()
    assert '不作施工真源' in text
print(f'文档关键词检查通过：{len(required)} 项必需、{len(forbidden)} 项禁用、2 份候选边界')
PY
```

| 故障/需求 | 最终稿中的对应机制 | 当前结论 |
|---|---|---|
| B 先于 A，且结果间 Promise 轮次不同 | 一轮一个 settlement，排空作业后整批提交；turnSeq 固定输入输出，恢复逐轮比较 | 协议已写明；Node 排空能力待 §14.0 实测 |
| 子 A 完成后其他父 run 抢位，后续 B 无槽 | 保留位绑定整个同步 Flow，A 后回 held，B 复用；暂停/父中断继续持有 | 生命周期已写明；DB 并发验收待施工 |
| 工具 intent 已写、result 未知 | needs_review 阻止子模型先恢复；证据/放弃后再续跑 | 设计已写明；运行待施工 |
| 合法 null 与失败 | 显式 ok/value/error，严格 JSON/schema | 设计已写明；运行待施工 |
| 漏 await 或 Promise.race 输家未收拢 | failed/unjoined_calls | 设计已写明；运行待施工 |
| 纯 JS 死循环/无限微任务 | 轮次不返回、历史停最后活动、心跳失联、用户停止、资源约束 | 设计已写明；运行待施工 |
| 人工修改历史计划 | 无在线编辑、无 revision/inputHash；同 Flow 追加未来工作 | 文档检查可复核 |
| 多份施工真源 | 85 唯一后续施工方案，86 两份仅记录 | 文档检查可复核 |

本次最终复核已实际通过：上述公开命令回 `13 项必需、7 项禁用、2 份候选边界`；最小 Node 反例再次得到同检查点 `repairA→repairB`、分检查点 `repairB→repairA`；`check-master-plan.sh` 无漏引，`check-doc-layout.sh` 检查 754 条 Markdown 链接并通过，生成站 1821 条本地链接零断链，`git diff --check` 通过。固定 Node 的逐轮 Promise 排空能力、模型、沙箱与活体故障验证仍全部留待获批施工，本文不宣称已经通过。