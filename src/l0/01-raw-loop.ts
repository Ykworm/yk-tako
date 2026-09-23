/**
 * ① 裸写版 —— pi 式 agent 主循环（复习）
 *
 * 核心就一个 while(true)：
 *   1. 把对话历史发给模型
 *   2. 模型要么直接回答（结束），要么返回 tool_calls（执行工具，把结果塞回历史，继续循环）
 *
 * 你会看到：这个循环本身只有十几行——agent 的"智能"全在模型和工具里，
 * 循环只是个苦力。问题出在别的事上：状态持久化？中断恢复？流式？多入口？
 * 这些才是后面框架要解决的问题。
 */
import "dotenv/config";
import * as readline from "node:readline/promises";
import {
  AIMessage,
  HumanMessage,
  ToolMessage,
  type BaseMessage,
} from "@langchain/core/messages";
import type { StructuredToolInterface } from "@langchain/core/tools";
import { model } from "../config/model";
import { tools } from "../tools";

const toolsByName: Record<string, StructuredToolInterface> = Object.fromEntries(
  tools.map((t) => [t.name, t]),
);
// bindTools：把工具的 name/description/schema 以 function calling 格式告诉模型
const llmWithTools = model.bindTools(tools);

async function chat(userInput: string, history: BaseMessage[]): Promise<void> {
  history.push(new HumanMessage(userInput));

  while (true) {
    const ai = (await llmWithTools.invoke(history)) as AIMessage;
    history.push(ai);

    // 没有工具调用 = 模型想直接回答，本轮结束
    if (!ai.tool_calls || ai.tool_calls.length === 0) {
      console.log(`\n🐙 ${ai.content}\n`);
      return;
    }

    // 执行所有工具调用，结果以 ToolMessage 形式塞回历史
    for (const call of ai.tool_calls) {
      console.log(`  → 🔧 ${call.name}(${JSON.stringify(call.args)})`);
      const t = toolsByName[call.name!];
      const result = await t.invoke(call.args);
      console.log(`  ← ${String(result).slice(0, 100)}`);
      history.push(
        new ToolMessage({
          content: String(result),
          tool_call_id: call.id!,
          name: call.name!,
        }),
      );
    }
    // 继续 while：模型拿到工具结果后再决定下一步
  }
}

const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
const history: BaseMessage[] = [];
console.log("① 裸写版 agent（pi 式 while 循环）。输入 quit 退出。\n");

while (true) {
  const input = (await rl.question("你: ")).trim();
  if (input === "quit" || input === "exit") break;
  if (!input) continue;
  try {
    await chat(input, history);
  } catch (e) {
    console.error("出错了:", e instanceof Error ? e.message : e);
  }
}
rl.close();
