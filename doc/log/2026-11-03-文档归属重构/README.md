# 文档归属重构（2026-11-03）

> 性质：**变更记录**——当时为什么动、动了什么、怎么验收。不是方案，也不是施工真源。
> 现行规则看 [doc/README.md](../../README.md) §5 与 [AGENTS.md](../../../AGENTS.md)「文档状态与真源」。
> 执行流水看同目录 [tasks.md](tasks.md)（冻结）。

## 病灶

旧规则按**时效**分家：`doc/` 现行、`docs/` 历史、`tmp/` 台账。但实际放法按**类型**分——交接稿进 `docs/`、方案进 `doc/`、裁定进 `doc/problems/`。两套规则打架，同一类文档出现多个家：

1. **记录被当垃圾**：`docs/2026-10-19-多租户-handoff.md` 是 doc/28 引用的施工期实测实录，还在服役，目录名却说它过期。
2. **同类文档三个家**：交接稿在 `docs/` 有 9 篇，`doc/` 根还躺着 `handoff-doc24-…` 与 `XX-taskboard-…`。
3. **检索污染**：grep 一个机制先命中 v3/v4 再命中现行，逼出「已作废不作论据」这条补丁规则。
4. **判断成本高**：新人得先读规则才知道 `doc/` 与 `docs/` 的区别，而规则本身与刚写进去的跑分文章矛盾。

根因是用会变化的「时效」当归属轴：从现行变历史要搬家，但搬家从没发生；新内容又按类型随手放进一个叫「历史」的目录。

## 拍的轴

按**类型**定家，不按时效：

| 类型 | 家 |
|---|---|
| 现行真源 | `doc/` 顶层编号文档 + `doc/19-v6/` |
| 设计裁定 | `doc/problems/` |
| 带日期的记录（交接、跑分、台账、文章） | `doc/log/` |
| 被整代取代的材料 | `doc/archive/` |
| 非方案（故事、求职材料） | `doc/blog/`、`doc/interview/` |

**时效变化不搬家，只改状态**；只有被整代取代才进 `doc/archive/`。

## 动了什么

- **A1**：`docs/` 的 9 篇日期记录 `git mv` 到 `doc/log/`，新建 [doc/log/README.md](../README.md) 索引，改 4 处入边与记录内部旧路径。
- **A2**：`doc/01`–`15`、`18`、两篇命名 handoff、`19-v5`、`superpowers/{plans,specs}` 共 31 篇移入 `doc/archive/old-line|engine`，新建 [doc/archive/README.md](../../archive/README.md)；`superpowers` 分组名按用户意见取消，计划与设计并入 `old-line/`。
- **A3**：`AGENTS.md`、根 `README.md`、`doc/README.md` §0/§5、`doc/53` §7、`doc/16` 头注改成四类归属；写入台账生命周期（todos → `doc/log/<日期>-<方案>/tasks.md`，planning 提炼成同目录 README）。
- **A4**：新增 [scripts/check-doc-layout.sh](../../../scripts/check-doc-layout.sh)（旧目录不复活、顶层无日期文件、log 命名与内容、归档清单覆盖、相对链接可解析），删除空 `docs/`，本目录作为新规则的第一个样本。

## 验收

```bash
bash scripts/check-doc-layout.sh    # doc layout: OK
bash scripts/check-master-plan.sh   # OK：doc/16 已覆盖 README §1 全部现行编号
test ! -d docs                      # docs gone
```

负向自检：在 `doc/` 顶层放一个 `2026-01-01-*.md`，脚本判红并退出 1；删除后恢复 OK。

## 关联真源

- 归属规则：[doc/README.md](../../README.md) §5
- 目录结构：[doc/53-模块地图.md](../../53-模块地图.md) §7
- 协作纪律（台账生命周期）：[AGENTS.md](../../../AGENTS.md)「台账产物」
- 记录索引：[doc/log/README.md](../README.md)
- 归档索引：[doc/archive/README.md](../../archive/README.md)
