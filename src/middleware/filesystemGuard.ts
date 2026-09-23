/**
 * FilesystemGuard —— 文件路径守卫（L1）
 *
 * 复刻自 harness-agent 的 middleware/filesystem_guard.py。
 *
 * 借 / 参考 / 重来：
 *  - 借：设计知识 —— 哪些工具算文件工具（ls/read_file/glob/grep 是读，
 *    write_file/edit_file/delete 是写）、路径参数从哪个字段取、
 *    要展开 realpath（macOS /etc → /private/etc 的符号链接坑）。
 *  - 参考：策略模型 —— 有序规则表，第一条命中的规则决定三态裁决：
 *    allow 放行 / deny 硬拒 / ask 暂停问人。
 *    注意：harness 的自研版是 **deny-only** 的降级实现（它的 `interrupt` 只是从
 *    deepagents 私有函数抄来的死注解，配置里写会被静默丢弃，程序内构造则 fail-open）；
 *    我们补回的三态，语义对齐 deepagents 上游的 interrupt-mode permission +
 *    HumanInTheLoopMiddleware，以及 Codex 的 workspace-write 提权审批。
 *    默认策略 = 工作区内放行、工作区外按 outsideWorkspace 配置（默认 ask）。
 *    敏感文件（.env/.git/.ssh）和系统目录写保持规则级硬拒——不该问的别问。
 *  - 重来：匹配实现。Python 用 wcmatch 库；这里手写一个极简 glob→RegExp
 *    转换（支持 ** 和 *），不引外部依赖。
 */
import { realpathSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { ToolMessage } from "@langchain/core/messages";
import { interrupt } from "@langchain/langgraph";
import { createMiddleware } from "langchain";

/** 与 harness 的 _FS_TOOL_OPS 对齐：哪些工具算文件工具，算读还是写 */
const FS_TOOL_OPS: Record<string, "read" | "write"> = {
  ls: "read",
  read_file: "read",
  glob: "read",
  grep: "read",
  write_file: "write",
  edit_file: "write",
  delete: "write",
};

/** 从工具参数里提取路径（对齐 harness 的 _path_from_tool_args） */
function pathFromArgs(toolName: string, params: Record<string, unknown>): string | null {
  const raw = params["path"] ?? params["file_path"];
  return typeof raw === "string" && raw.trim() ? raw.trim() : null;
}

/** 三态：allow 放行 / deny 硬拒 / ask 暂停问人（对齐 harness 的 interrupt 态、Codex 的提权审批） */
export type FsAction = "allow" | "deny" | "ask";

export interface FsRule {
  /** glob 模式列表，如 "/etc/**"、"**.env"（注意注释里不能出现 star-slash 序列） */
  paths: string[];
  /** 命中的操作类型 */
  ops: ("read" | "write")[];
  mode: FsAction;
  reason?: string;
}

/** 极简 glob → RegExp：** 跨目录，* 不跨目录。够用于路径策略。 */
function globToRegExp(glob: string): RegExp {
  const expanded = glob.startsWith("~") ? os.homedir() + glob.slice(1) : glob;
  const re = expanded
    .replace(/[.+^${}()|[\]\\]/g, "\\$&")
    .replace(/\*\*\//g, "\u0001") // **/ → 占位（可选的前缀目录）
    .replace(/\*\*/g, "\u0002")
    .replace(/\*/g, "\u0003")
    .replace(/\u0001/g, "(?:.*/)?")
    .replace(/\u0002/g, ".*")
    .replace(/\u0003/g, "[^/]*");
  return new RegExp(`^${re}$`);
}

function realpathIfAvailable(p: string): string | null {
  try {
    return realpathSync(p);
  } catch {
    return null;
  }
}

function pathMatches(pathCandidates: string[], pattern: string): boolean {
  const re = globToRegExp(pattern);
  // "/etc/**" 也要匹配 "/etc" 本身（对齐 harness 的 _expand_patterns）
  const prefixRe = pattern.endsWith("/**") ? globToRegExp(pattern.slice(0, -3)) : null;
  return pathCandidates.some((c) => re.test(c) || (prefixRe?.test(c) ?? false));
}

export interface FilesystemGuardOptions {
  /** 工作区根目录 */
  workspaceRoot: string;
  /** 有序规则表，先命中先生效 */
  rules?: FsRule[];
  /**
   * 区外路径的默认策略：
   *  - "ask"（默认）：暂停问人，批准才放行 —— 对齐 Codex 的 workspace-write + 提权审批
   *  - "deny"：硬拒
   * 注意 DEFAULT_RULES 里的敏感文件/系统目录是规则级硬拒，不受此开关影响。
   */
  outsideWorkspace?: "ask" | "deny";
}

export type FsDecision =
  | { action: "allow" }
  | { action: "deny"; reason: string }
  | { action: "ask"; reason: string };

const DEFAULT_RULES: FsRule[] = [
  { paths: ["**/.env", "**/.env.*", "**/.git/**", "**/.ssh/**", "**/id_rsa*"], ops: ["read", "write"], mode: "deny", reason: "敏感文件（密钥/凭据/版本库内部）" },
  { paths: ["/etc/**", "/private/etc/**", "/System/**", "/usr/**", "/bin/**", "/sbin/**"], ops: ["write"], mode: "deny", reason: "系统目录不可写" },
];

/** 纯判定函数，单独导出便于单测（对齐 harness 的 filesystem_guard_block_reason） */
export function filesystemGuardDecision(
  options: FilesystemGuardOptions,
  toolName: string,
  params: Record<string, unknown>,
): FsDecision {
  const allow: FsDecision = { action: "allow" };
  const op = FS_TOOL_OPS[toolName];
  if (!op) return allow;
  const rawPath = pathFromArgs(toolName, params);
  if (!rawPath) return allow;

  const root = path.resolve(options.workspaceRoot);
  const absolute = path.isAbsolute(rawPath) ? path.normalize(rawPath) : path.resolve(root, rawPath);

  // 候选路径：逻辑路径 + realpath（处理符号链接，如 macOS /etc → /private/etc）
  const candidates = [absolute];
  const real = realpathIfAvailable(absolute);
  if (real && real !== absolute) candidates.push(real);

  for (const rule of [...(options.rules ?? []), ...DEFAULT_RULES]) {
    if (!rule.ops.includes(op)) continue;
    if (rule.paths.some((p) => pathMatches(candidates, p))) {
      if (rule.mode === "allow") return allow;
      const reason = `${op === "read" ? "读取" : "写入"} ${absolute}${rule.reason ? `（${rule.reason}）` : ""}`;
      return { action: rule.mode, reason };
    }
  }

  // 默认策略：工作区内放行；区外按 outsideWorkspace 配置（默认 ask）
  const inWorkspace = candidates.some((c) => c === root || c.startsWith(root + path.sep));
  if (inWorkspace) return allow;
  const reason = `${op === "read" ? "读取" : "写入"} ${absolute}（超出工作区 ${root}）`;
  return { action: options.outsideWorkspace ?? "ask", reason };
}

export function filesystemGuardMiddleware(options: FilesystemGuardOptions) {
  return createMiddleware({
    name: "FilesystemGuardMiddleware",
    wrapToolCall: async (request, handler) => {
      const { id, name, args } = request.toolCall;
      const params = (args ?? {}) as Record<string, unknown>;
      const decision = filesystemGuardDecision(options, name, params);
      if (decision.action === "allow") return handler(request);

      if (decision.action === "ask") {
        // resume 值不能是 falsy（空 Command 报错），用字符串
        const approved = interrupt({
          kind: "fs_guard_approval",
          tool: name,
          args: params,
          message: `文件访问需要人工批准：${decision.reason}`,
        });
        if (approved === "approve") return handler(request);
        return new ToolMessage({
          content: `用户拒绝了这次 ${name} 调用（${decision.reason}）。不要重试相同路径。`,
          tool_call_id: id ?? "",
          name,
          status: "error",
        });
      }

      return new ToolMessage({
        content: `Error: permission denied —— ${decision.reason}`,
        tool_call_id: id ?? "",
        name,
        status: "error",
      });
    },
  });
}
