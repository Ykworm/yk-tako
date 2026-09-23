/**
 * ③ deepagents 版 —— 看"harness"又白送了什么
 *
 * 对照 ②：代码几乎没变，但 createDeepAgent 在 createReactAgent 之上
 * 又叠了一整套中间件（middleware）。试试这些对话，感受差异：
 *
 *   - "帮我规划一下：先查现在时间，再算 1234*5678，最后写个总结"
 *     → 它会先建一个 todo 清单（任务规划中间件），逐项执行
 *   - "把结果写到 /tmp/tako-notes.txt"
 *     → 它有虚拟文件系统工具（read/write/edit 文件）
 *   - 复杂任务它会自动 spawn 子代理在隔离上下文里干活，只汇报结论
 *
 * 这三样正是我们能力阶梯的 L2（规划）、L3（上下文/文件）、L4（子代理）。
 * Octop 的 harness-agent = createDeepAgent + 24 个自研中间件。
 * 我们后面每爬一层阶梯，就是往这里插一个自己写的中间件。
 */
import "dotenv/config";
import * as readline from "node:readline/promises";
import { HumanMessage } from "@langchain/core/messages";
import { MemorySaver } from "@langchain/langgraph";
import { createDeepAgent } from "deepagents";
import { model } from "../config/model";
import { tools } from "../tools";

const agent = createDeepAgent({
  model,
  tools,
  systemPrompt: "你是 yk-tako，一个正在成长中的 AI 工作伙伴。回答简洁、说中文。",
  checkpointer: new MemorySaver(),
});

const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
const config = { configurable: { thread_id: "demo-1" } };
console.log("③ deepagents 版 agent（完整 harness）。输入 quit 退出。\n");

while (true) {
  const input = (await rl.question("你: ")).trim();
  if (input === "quit" || input === "exit") break;
  if (!input) continue;

  try {
    const stream = await agent.stream(
      { messages: [new HumanMessage(input)] },
      { ...config, streamMode: "updates" },
    );
    for await (const chunk of stream) {
      for (const [node, update] of Object.entries(chunk)) {
        const msgs = (
          update as { messages?: Array<{ content: unknown; tool_calls?: Array<{ name: string }> }> }
        ).messages;
        const last = msgs?.at(-1);
        if (!last) continue;
        if (last.tool_calls?.length) {
          console.log(`  → 🔧 [${node}] 调用: ${last.tool_calls.map((c) => c.name).join(", ")}`);
        } else if (node === "tools") {
          console.log(`  ← 🔧 工具结果: ${String(last.content).slice(0, 100)}`);
        } else if (typeof last.content === "string" && last.content) {
          console.log(`\n🐙 ${last.content}\n`);
        }
      }
    }
  } catch (e) {
    console.error("出错了:", e instanceof Error ? e.message : e);
  }
}
rl.close();
