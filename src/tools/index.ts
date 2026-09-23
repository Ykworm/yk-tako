import { tool } from "langchain";
import * as z from "zod";

/**
 * 工具定义三要素：name / description / schema。
 * description 是写给 LLM 看的"使用说明"——它决定模型什么时候、怎么用这个工具，
 * 写得好不好直接决定 agent 表现，这是 prompt engineering 的一部分。
 */
export const getCurrentTime = tool(
  async () => {
    return new Date().toLocaleString("zh-CN", { timeZone: "Asia/Shanghai" });
  },
  {
    name: "get_current_time",
    description: "获取当前的日期和时间（东八区）",
    schema: z.object({}),
  },
);

export const calculate = tool(
  async ({ expression }) => {
    // 安全起见只放行数字和基本运算符，拒绝任意代码执行
    if (!/^[\d\s+\-*/().%]+$/.test(expression)) {
      return "错误：表达式只能包含数字和 + - * / ( ) % 运算符";
    }
    try {
      // eslint-disable-next-line no-new-func
      const result = Function(`"use strict"; return (${expression})`)();
      return String(result);
    } catch {
      return `无法计算的表达式: ${expression}`;
    }
  },
  {
    name: "calculate",
    description: "计算数学表达式的值，例如 '1234*5678' 或 '(100+200)/3'",
    schema: z.object({
      expression: z.string().describe("要计算的数学表达式"),
    }),
  },
);

export const tools = [getCurrentTime, calculate];
