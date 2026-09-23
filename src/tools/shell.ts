/**
 * bash 工具 —— L1 工具安全层的"被保护对象"
 *
 * 没有危险工具，守卫就无用武之地。这个工具给 agent 真实的 shell 能力，
 * 故意不做任何拦截 —— 拦截是中间件的职责（关注点分离）：
 * 工具只负责"能干"，中间件负责"让不让干"。
 */
import { exec } from "node:child_process";
import { promisify } from "node:util";
import { tool } from "langchain";
import { z } from "zod";

const execAsync = promisify(exec);

export const bashTool = tool(
  async ({ command }) => {
    try {
      const { stdout, stderr } = await execAsync(command, {
        cwd: process.cwd(),
        timeout: 10_000,
        maxBuffer: 1024 * 1024,
      });
      const out = [stdout.trim(), stderr.trim()].filter(Boolean).join("\n");
      return out || "(命令执行成功，无输出)";
    } catch (e) {
      const err = e as { message?: string; stdout?: string; stderr?: string };
      return `命令执行失败: ${err.message ?? e}\n${err.stdout ?? ""}${err.stderr ?? ""}`.trim();
    }
  },
  {
    name: "bash",
    description: "在宿主机 shell 中执行命令并返回输出。工作目录为项目根目录，超时 10 秒。",
    schema: z.object({
      command: z.string().describe("要执行的 shell 命令"),
    }),
  },
);
