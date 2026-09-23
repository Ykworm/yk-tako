/**
 * 文件读写工具 —— filesystem_guard 的"被保护对象"
 *
 * 同样故意不做路径校验：越权拦截交给 FilesystemGuardMiddleware。
 * 工具名刻意对齐 deepagents/harness 的文件工具命名（read_file / write_file），
 * 这样守卫的规则表可以原样复用。
 */
import { readFile, writeFile } from "node:fs/promises";
import { tool } from "langchain";
import { z } from "zod";

export const readFileTool = tool(
  async ({ path }) => {
    try {
      const content = await readFile(path, "utf-8");
      return content.length > 4000 ? `${content.slice(0, 4000)}\n...(截断，共 ${content.length} 字符)` : content;
    } catch (e) {
      return `读取失败: ${e instanceof Error ? e.message : e}`;
    }
  },
  {
    name: "read_file",
    description: "读取指定路径的文件内容（绝对路径或相对项目根目录）。",
    schema: z.object({
      path: z.string().describe("文件路径"),
    }),
  },
);

export const writeFileTool = tool(
  async ({ path, content }) => {
    try {
      await writeFile(path, content, "utf-8");
      return `已写入 ${path}（${content.length} 字符）`;
    } catch (e) {
      return `写入失败: ${e instanceof Error ? e.message : e}`;
    }
  },
  {
    name: "write_file",
    description: "把内容写入指定路径的文件（会覆盖已有文件）。",
    schema: z.object({
      path: z.string().describe("文件路径"),
      content: z.string().describe("要写入的完整内容"),
    }),
  },
);
