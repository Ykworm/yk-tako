/**
 * L1 演示 —— 工具安全层三件套
 *
 * 对照 L0③：agent 多了一圈"安检"。三件套各管一件事：
 *
 *   用户输入 ──→ [PII 脱敏] ──→ 模型 ──→ 决定调工具
 *                                          │
 *              ┌───────────────────────────┤
 *              ▼                           ▼
 *      [ToolGuard 查参数]         [FilesystemGuard 查路径]
 *              │                           │
 *        放行/警告/拦截/审批          放行/拦截
 *              │                           │
 *              └──────────→ 工具执行 ←──────┘
 *                            │
 *                  [PII 脱敏工具结果] ──→ 回到模型
 *
 * 试试这些输入：
 *   用 bash 执行 echo hello                → 安全，直接放行
 *   用 bash 执行 rm -rf /tmp/test          → 触发审批（y/n 决定生死）
 *   读取 /etc/passwd                       → 文件守卫拦截（工作区外）
 *   读取 .env                              → 文件守卫拦截（敏感文件）
 *   我的 key 是 sk-abc123...（20+位）       → PII 打码后进模型
 *
 * 斜杠命令：/mode block|warn|require_approval 切换 ToolGuard 模式
 */
import "dotenv/config";
import * as readline from "node:readline/promises";
import { HumanMessage } from "@langchain/core/messages";
import type { BaseChatModel } from "@langchain/core/language_models/chat_models";
import { Command, MemorySaver } from "@langchain/langgraph";
import type { StateSnapshot } from "@langchain/langgraph";
import type { AnyAgentMiddleware } from "langchain";
import { createAgent } from "langchain";
import { model, modelRef } from "../config/model";
import { filesystemGuardMiddleware } from "../middleware/filesystemGuard";
import { apiKey_Pii_Middleware } from "../middleware/pii";
import { toolGuardMiddleware, type ToolGuardMode } from "../middleware/toolGuard";
import { calculate, getCurrentTime } from "../tools";
import { readFileTool, writeFileTool } from "../tools/files";
import { bashTool } from "../tools/shell";

const toolGuard = toolGuardMiddleware({ mode: "require_approval" });
const filesystemGuard = filesystemGuardMiddleware({ workspaceRoot: process.cwd() });

const agent = createAgent({
  // model 的运行时类型是联合（OpenAI|Anthropic），泛型推断会炸；收窄到公共基类即可
  model: model as BaseChatModel,
  // 数组字面量让 TS 推成 tuple 类型；数组变量展开会让 createAgent 的重载推断失败
  tools: [getCurrentTime, calculate, bashTool, readFileTool, writeFileTool],
  // 中间件顺序 = 执行顺序；三个守卫互相独立，顺序不影响正确性
  middleware: [toolGuard.middleware, filesystemGuard, apiKey_Pii_Middleware] as AnyAgentMiddleware[],
  // interrupt 需要 checkpointer：暂停时状态落盘，恢复时从这里继续
  checkpointer: new MemorySaver(),
});

const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
rl.on("close", () => process.exit(0));
const config = { configurable: { thread_id: "l1-demo" } };

console.log(`L1 工具安全层演示。模型 ${modelRef}。`);
console.log(`ToolGuard 模式: ${toolGuard.engine.currentMode}（/mode 可切换）。输入 quit 退出。\n`);

/** 打印流式增量（与 L0② 相同） */
function printChunk(chunk: Record<string, unknown>): void {
  for (const [node, update] of Object.entries(chunk)) {
    const msgs = (update as { messages?: Array<{ content: unknown; tool_calls?: unknown[] }> }).messages;
    const last = msgs?.at(-1);
    if (!last) continue;
    if (node === "tools") {
      console.log(`  ← 🔧 工具结果: ${String(last.content).slice(0, 120)}`);
    } else if (last.tool_calls?.length) {
      console.log(`  → 🔧 模型决定调用工具`);
    } else {
      console.log(`\n🐙 ${last.content}\n`);
    }
  }
}

/** 一轮对话：流式输出 → 若遇 interrupt 则逐个请人裁决 → 恢复执行，直到图跑完 */
async function runTurn(input: string): Promise<void> {
  let streamInput: { messages: HumanMessage[] } | Command = { messages: [new HumanMessage(input)] };
  for (;;) {
    const stream = await agent.stream(streamInput, { ...config, streamMode: "updates" });
    for await (const chunk of stream) printChunk(chunk as Record<string, unknown>);

    // agent 的泛型推断在 getState 上塌缩成 never，显式标注 StateSnapshot
    const state = (await agent.getState(config)) as StateSnapshot;
    const interrupts = state.tasks?.flatMap((t) => t.interrupts ?? []) ?? [];
    if (!state.next?.length || interrupts.length === 0) return;

    const intr = interrupts[0];
    const payload = intr.value as { kind?: string; tool?: string; args?: Record<string, unknown>; message?: string };
    console.log("\n⏸️  ═══════════════ 人工审批（interrupt）═══════════════");
    console.log(`工具: ${payload.tool}   参数: ${JSON.stringify(payload.args)}`);
    console.log(payload.message ?? "");
    const ans = (await rl.question("批准执行？[y/N] ")).trim().toLowerCase();
    const approved = ans === "y" || ans === "yes";
    console.log(approved ? "✅ 已批准，继续执行\n" : "🚫 已拒绝\n");
    streamInput = new Command({ resume: approved ? "approve" : "reject" });
  }
}

while (true) {
  const input = (await rl.question("你: ")).trim();
  if (input === "quit" || input === "exit") break;
  if (!input) continue;

  // /mode 切换 ToolGuard 模式，演示三种策略的行为差异
  const modeMatch = input.match(/^\/mode\s+(block|warn|require_approval)$/);
  if (modeMatch) {
    toolGuard.engine.configure({ mode: modeMatch[1] as ToolGuardMode });
    console.log(`ToolGuard 模式已切换为: ${toolGuard.engine.currentMode}\n`);
    continue;
  }

  try {
    await runTurn(input);
  } catch (e) {
    console.error("出错了:", e instanceof Error ? e.message : e);
  }
}
rl.close();
