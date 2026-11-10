# 69 · 参照实现对照：rakazo 与 octop（plugin / skill / expert）

> 性质：非方案，对照记录。2026-11-08。
>
> 只回答「rakazo 与 octop 怎么管 agent、有没有 plugin / skill / expert 机制，tako 能借鉴什么」。
> rakazo 指参照实现 rakazo（本机 clone 在 `ref-agent/rakazo/`），octop 指参照实现 Octop
> （本机 clone 在 `ref-agent/Octop/`），tako 指本仓。两边现状都以代码为准，指针见 §6；
> 本篇不作施工真源——要落地任何借鉴，另立方案篇。

## 1. 一句话结论

- **rakazo**：Agent 是「Bot（长期身份）+ Subagent（短期执行）」双形态；有 **plugin**（agent-plugins
  规范，只从公共 GitHub 仓库取、逐字不执行仓库代码，只装 MCP server 或分发 skill）和 **skill**
  （SKILL.md：YAML frontmatter + 正文）；**没有** expert（全库 grep `expert`/`专家` 零命中）。
- **octop**：有显式 **expert**（专家 = 可实例化成 Agent 的模板：manifest.json + SOUL.md + skills 目录），
  其上还有 **Team**（主持人 + 成员的多 agent 协作）；插件是 `plugin.yaml + main.py` 的可执行包
  （40+ 内置），skill 是 SKILL.md + scripts/。
- **tako**：子代理规格（`agents/*.md`）与 skill 机制（doc/52，24 拍已收口）已落地、MCP client
  已接（doc/33）；**没有** plugin 外壳、**没有** expert、多 agent 协作在 doc/16 能力表里是 ⬜ 未做。
- 三家 skill 共用同一种 SKILL.md 格式（frontmatter + 正文），格式互通；差别在执行模型与环境。

## 2. 三方概览

- **rakazo**：TypeScript monorepo，个人助理式 agent（Hermes/OpenClaw 血统），核心抽象是带身份的 Bot。
- **octop**：Python agent 平台，用户挑模板建 agent、可组团队对话。
- **tako**：Go + Eino 的起引擎 agent 运行时，子代理与 skill 是出厂能力，多 agent 协作未做。

## 3. rakazo 怎么管 agent（含 plugin / skill）

### 3.1 Agent：Bot + Subagent

- **Bot** 是持续身份：一个 Bot 一条永久可见线程，自带对话历史、指令、内存、例程；Bot 列表即长期角色。
- **Subagent** 是短生命周期执行者：通过 `run_subagent` 工具调用（`packages/contracts/src/builtin-tools.ts`），
  无独立线程、不出现在 Bot 列表，可后台并行（`background: true`），结果由主 Bot 汇总。
- 管理入口：fleet 管理（Bot 管 Bot）、agent 导入（`docs/import-an-agent.md`，可从
  OpenClaw/Hermes/ChatGPT/Claude/Gemini 迁移指令/内存/skills/MCP/API key）。

### 3.2 Plugin：装 MCP 或分发 skill 的安全壳

- 遵循 **agent-plugins 规范**（`plugin.json` 里写 `$schema`，1.0.0/1.1.0）。
- 从**公共 GitHub 仓库**获取文件，**永不执行仓库代码**。
- 两种形态：**MCP-server plugin**（`mcp.json` 配远端 server 连接）与 **skill-only plugin**（只分发 skills）。
- 安装流程：获取 → 解析 plugin.json / mcp.json → 解析全部 SKILL.md → 校验 + digest → 装成
  Connector 与 Skills。
- 明确不支持：本地 stdio MCP、hooks、automations、脚本与二进制；只认公共 GitHub 仓库。
- 核心实现：`packages/adapters/src/plugin-connectors.ts`；示例 `examples/plugins/`。

### 3.3 Skill：SKILL.md + 三来源 + 四把工具

- 格式：`SKILL.md` = YAML frontmatter（name / description）+ Markdown 正文（步骤）。
- 来源三档：user（可读写）/ builtin（只读）/ plugin（只读）；目录入口 `docs/skills.md`，
  内置技能在 `/.agents/skills/`。
- 工具四把（`packages/contracts/src/builtin-tools.ts`）：`skill_read` / `skill_create` /
  `skill_update` / `skill_delete`。
- 使用方式：模型自动按 description 匹配；用户在消息开头 `/SkillName` 强制使用；routine 里 `@SkillName` 引用。
- 核心实现：`packages/core/src/agent-skill.ts`。

## 4. octop 的 expert 玩法（含 plugin / skill）

### 4.1 三层：Expert（模板） → Agent（实例） → Team（协作）

- **Expert** = 预配置的 agent 模板，目录里四样东西：
  `manifest.json`（id / 描述 / 欢迎语 / 快捷提示）、`SOUL.md`（人格 = system prompt）、
  `skills/`（该专家自带技能）、`AGENTS.md` / `USER.md`（额外文档）。
  模板目录 `src/octop/infra/agents/experts/library/`，管理 `src/octop/infra/agents/experts/catalog.py`
  （`ExpertCatalog`、`build_create_spec_from_expert()`）。
- **Agent** = 模板实例化：用户挑模板 → `AgentCreateSpec`（含 `template_name`）→ 落库 `agents` 表
  （`src/octop/infra/agents/manager.py`）；可另挂 MBTI 人格（`persona.py` + `mbti_profiles.py`）、
  指定默认模型。
- **Team** = 主持人与成员的群聊协作：主持人（kind=team，至少 2 名成员）只有轻量工具
  （`agent_list` / `ask_agent` / `memory_search`），通过 `ask_agent` 异步派工给成员，
  成员气泡直接上用户时间线（`src/octop/infra/agents/teams/service.py`，设计见 `docs/expert-teams.md`）。

### 4.2 其它机制

- **Plugin**：`plugin.yaml`（id/version/name/kind）+ `main.py` 入口 + 可选 `ui/`；
  `PluginManager`（`src/octop/infra/agents/plugins/manager.py`）支持路径 / ZIP / URL 安装，
  内置插件 40+（`plugins/bundled/`）。与 rakazo 相反，**octop 的插件是可执行代码**。
- **Skill**：`SKILL.md` + `scripts/` + `references/`；来源三路：expert 自带、plugin 附带、
  SkillHub 市场（`src/octop/infra/skills/`，`ResolvedSkillPackage`）。

## 5. 逐项对比

- **Agent 形态**
  - rakazo：Bot（长期身份，一条永久线程）+ Subagent（短期并行执行者，run_subagent 工具）。
  - octop：Agent 是模板实例（图灵完整的可执行 agent），还有 Team 群聊层。
  - tako：manager + 文件式子代理规格（`agents/*.md`），子代理是父池子集的工具型执行者，
    有持久化与 resume（doc/39）；没有「持续身份」概念，子代理不拥有自己的线程。

- **Skill 格式**
  - 三家同构：SKILL.md（YAML frontmatter + Markdown 正文）——格式互通是最低成本的借鉴面。
  - rakazo：skill 是知识单元，四把工具读写，身份证在 description。
  - octop：skill 可带 scripts 可选脚本，来源含市场。
  - tako：manager 自己 `read_skill`/`use_skill` 读正文、自己执行，正文步骤展开成看板任务
    （`run_tasks` 加 `skill_name`），压缩时正文可折、任务不折——执行模型三家里最程序化（doc/52）。

- **Plugin**
  - rakazo：静态安全壳——只装 MCP server 或分发 skill，永不执行仓库代码，只认公共 GitHub。
  - octop：可执行功能包——plugin.yaml + main.py，能带 UI。
  - tako：**没有** plugin 概念；能力已被两块覆盖——MCP client（doc/33，stdio 本地 server、
    工具直接进池）+ skill 安装写面（doc/52 C 系列：zip/tar.gz/来源串/`npx skills add` 命令串，
    staging 校验拒 zip slip/软链）。缺的是「把 MCP server 配置 + skills + 元数据打包成可分发产物」
    这层外壳。

- **Expert / Team**
  - rakazo：无。
  - octop：有 expert（模板→实例）+ Team（主持人派工）。
  - tako：有「出厂规格 = 模板、装配进租户 = 实例」的影子（doc/34），但**没有**「用户从带人格的
    专家模板里挑」的产品入口，也没有 Team 派工层（doc/16 多 agent 协作 ⬜ 未做）。

## 6. 出处

- rakazo：`ref-agent/rakazo/`——`packages/core/src/agent-skill.ts`（skill 核心）、
  `packages/contracts/src/builtin-tools.ts`（run_subagent 与 skill 工具）、
  `packages/adapters/src/plugin-connectors.ts`（plugin 安装）、`examples/plugins/`、
  `docs/{skills,import-an-agent,fleet-management,build-a-plugin}.md`、`skills-lock.json`。
- octop：`ref-agent/Octop/`——`src/octop/infra/agents/experts/{catalog.py,library/}`（expert）、
  `src/octop/infra/agents/{manager.py,persona.py,mbti_profiles.py}`（agent 与人格）、
  `src/octop/infra/agents/teams/service.py` + `docs/expert-teams.md`（team）、
  `src/octop/infra/agents/plugins/{manager.py,bundled/}`（plugin）、
  `src/octop/infra/skills/`（skill）。
- tako：`agents/coder.md`、`agents/researcher.md`、`internal/runengine/agents.go`（子代理规格）、
  doc/52（技能）、doc/33（MCP）、doc/34（租户装配）、doc/39（子代理持久化与 resume）。

## 7. 对 tako 的借鉴点（只列候选，落地另立方案篇）

1. **skill 格式已同构**，若做交换/导入可先对齐 rakazo/octop 的 SKILL.md 子集，成本最低。
2. **plugin 外壳**两条路线可择：rakazo 的「静态声明、不执行代码」与 octop 的「可执行包」；
   tako 现有沙箱与 staging 校验已覆盖后者的安全下限，缺的是产物打包与分发协议。
3. **expert 玩法** tako 缺的只有产品入口（选模板建 agent）与 Team 派工层；
   规格→装配机制已有同构基础，Team 需引入主持人/成员两档工具面与 `ask_agent` 派工语义。