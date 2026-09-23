/**
 * ② LangGraph.js 版 —— 看运行时接管了什么
 *
 * 对照 ①：while 循环不见了。createReactAgent 帮你建了一张图：
 *
 *      ┌──────────┐   tool_calls   ┌───────────┐
 *      │  agent   │ ─────────────→ │  ToolNode │
 *      │ (调模型) │ ←───────────── │ (执行工具) │
 *      └────┬─────┘   工具结果      └───────────┘
 *           │ 无 tool_calls
 *           ▼
 *          END
 *
 * 循环逻辑（toolsCondition）变成了图的"条件边"。
 * 真正值钱的是白送的三样东西：
 *   - checkpointer：每一轮状态自动落盘，thread_id 一给，重启进程会话还在（① 里没有）
 *   - stream()：图内每个节点的执行过程都能流式观测（① 里你只能干等 invoke）
 *   - interrupt：可以在任意边中断等人审批（L1 工具安全层的挂载点）
 */
import "dotenv/config";
import * as readline from "node:readline/promises";
import { HumanMessage } from "@langchain/core/messages";
import { MemorySaver } from "@langchain/langgraph";
import { createReactAgent } from "@langchain/langgraph/prebuilt";
import { model } from "../config/model";
import { tools } from "../tools";

const agent = createReactAgent({
  llm: model,
  tools,
  // MemorySaver = 内存版 checkpointer；换 PostgresSaver 就是生产级持久化
  checkpointer: new MemorySaver(),
});

const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
// 同一个 thread_id = 同一段会话；换一个就是新会话，互不干扰
const config = { configurable: { thread_id: "demo-1" } };
console.log("② LangGraph.js 版 agent。输入 quit 退出。\n");

while (true) {
  const input = (await rl.question("你: ")).trim();
  if (input === "quit" || input === "exit") break;
  if (!input) continue;

  try {
    // streamMode: "updates" = 每个节点执行完就吐一次增量，你能看到图的运转过程
    const stream = await agent.stream(
      { messages: [new HumanMessage(input)] },
      { ...config, streamMode: "updates" },
    );
    for await (const chunk of stream) {
      for (const [node, update] of Object.entries(chunk)) {
        const msgs = (update as { messages?: Array<{ content: unknown; tool_calls?: unknown[] }> })
          .messages;
        const last = msgs?.at(-1);
        if (!last) continue;
        if (node === "tools") {
          console.log(`  ← 🔧 工具结果: ${String(last.content).slice(0, 100)}`);
        } else if (last.tool_calls?.length) {
          console.log(`  → 🔧 模型决定调用工具`);
        } else {
          console.log(`\n🐙 ${last.content}\n`);
        }
      }
    }
  } catch (e) {
    console.error("出错了:", e instanceof Error ? e.message : e);
  }
}
rl.close();
