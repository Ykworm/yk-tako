/**
 * ToolGuard —— 工具参数安全守卫（L1）
 *
 * 复刻自 harness-agent 的 security/tool_guard/ + middleware/tool_guard.py。
 *
 * 三层复刻策略的体现：
 *  - 借：下面的 RULES 正则规则表，是从它的 dangerous_shell_commands.yaml
 *    提炼的代表性子集（rm/mv/磁盘销毁/fork炸弹/管道执行/反弹shell/Unicode空白等）。
 *    这是安全知识，与语言无关，不借白不借。
 *  - 参考：ToolGuardEngine 的设计 —— 三模式（block/warn/require_approval）
 *    × 严重度阈值决定拦不拦；guard() 返回 findings，空数组 = 安全。
 *  - 重来：挂载机制。Python 版用 AgentMiddleware.awrap_tool_call；
 *    这里用 LangChain.js 的 createMiddleware({ wrapToolCall })，
 *    审批暂停用 LangGraph.js 原生 interrupt()，恢复用 Command({ resume })。
 */
import { ToolMessage } from "@langchain/core/messages";
import { interrupt } from "@langchain/langgraph";
import { createMiddleware } from "langchain";

// ─────────────────── 规则表（借自 harness 的 YAML，TS 化） ───────────────────

export type GuardSeverity = "CRITICAL" | "HIGH" | "MEDIUM" | "LOW" | "INFO";

export interface GuardRule {
  id: string;
  /** 命中的工具名；缺省 = 所有工具 */
  tools?: string[];
  /** 命中的参数名；缺省 = 所有参数 */
  params?: string[];
  severity: GuardSeverity;
  patterns: RegExp[];
  /**
   * 排除模式。`/^\s*#/` 按行丢掉注释行，剩下的文本继续匹配。
   * 其它模式命中整段参数时，本条规则不报警。
   */
  excludePatterns?: RegExp[];
  description: string;
  remediation: string;
}

const SHELL = ["bash", "execute"];
const COMMAND = ["command"];

export const DEFAULT_RULES: GuardRule[] = [
  {
    id: "TOOL_CMD_DANGEROUS_RM",
    tools: SHELL,
    params: COMMAND,
    severity: "HIGH",
    patterns: [/\brm\b/i, /\bdel\b/i, /\bRemove-Item\b/i],
    excludePatterns: [/^\s*#/],
    description: "命令包含 rm/del，可能造成数据丢失",
    remediation: "执行删除前与用户确认",
  },
  {
    id: "TOOL_CMD_DANGEROUS_MV",
    tools: SHELL,
    params: COMMAND,
    severity: "HIGH",
    patterns: [/\bmv\b/i],
    description: "命令包含 mv，可能意外移动或覆盖文件",
    remediation: "执行移动/重命名前与用户确认",
  },
  {
    id: "TOOL_CMD_FS_DESTRUCTION",
    tools: SHELL,
    params: COMMAND,
    severity: "CRITICAL",
    patterns: [
      /\bmkfs(\.[a-z0-9_]+)?\b/i,
      /\bmke2fs\b/i,
      /\bdd\s+.*of=\/dev\//i,
      />\s*\/dev\/(sd[a-z]\d*|vd[a-z]\d*|nvme\d+n\d+(p\d+)?)/i,
    ],
    description: "检测到磁盘格式化/擦除等底层破坏性命令",
    remediation: "直接拦截。agent 不应操作裸块设备。",
  },
  {
    id: "TOOL_CMD_DOS_FORK_BOMB",
    tools: SHELL,
    params: COMMAND,
    severity: "CRITICAL",
    patterns: [/:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:/, /\bkill\s+-9\s+(-1\b|1\b)/],
    description: "检测到 fork 炸弹或批量杀进程命令",
    remediation: "立即拦截。这类命令会搞垮宿主机。",
  },
  {
    id: "TOOL_CMD_PIPE_TO_SHELL",
    tools: SHELL,
    params: COMMAND,
    severity: "CRITICAL",
    patterns: [/\b(curl|wget)\b\s+.*\|.*\b(bash|sh|zsh|ash|dash)\b/i],
    description: "检测到 curl|bash 模式：下载并立即执行远程脚本",
    remediation: "与用户确认。agent 应先审查脚本内容再执行。",
  },
  {
    id: "TOOL_CMD_REVERSE_SHELL",
    tools: SHELL,
    params: COMMAND,
    severity: "CRITICAL",
    patterns: [/\/dev\/(tcp|udp)\//i, /\bnc\s+.*-e\s*\S+/i, /\bncat\s+.*-e\s*\S+/i, /\bsocat\s+.*EXEC:/i],
    description: "检测到反弹 shell / 未授权网络隧道",
    remediation: "直接拦截。agent 无需绑定交互式 shell 或隧道。",
  },
  {
    id: "TOOL_CMD_UNICODE_WHITESPACE",
    tools: SHELL,
    params: COMMAND,
    severity: "HIGH",
    patterns: [/[\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]/],
    description: "命令含 Unicode 空白字符，可能造成解析歧义绕过检查",
    remediation: "拦截含非 ASCII 空白的命令，使用普通空格。",
  },
  {
    id: "TOOL_CMD_PROC_ENVIRON",
    tools: SHELL,
    params: COMMAND,
    severity: "HIGH",
    patterns: [/\/proc\/(?:self|\d+)\/environ(?:\b|$)/],
    excludePatterns: [/^\s*#/],
    description: "访问 /proc/*/environ，可能泄露 API Key 等环境变量",
    remediation: "拦截对进程环境变量文件的访问。",
  },
  {
    id: "TOOL_CMD_JQ_SYSTEM",
    tools: SHELL,
    params: COMMAND,
    severity: "HIGH",
    patterns: [/\bjq\b.*\bsystem\s*\(/i],
    excludePatterns: [/^\s*#/],
    description: "jq 的 system() 可执行任意命令，把数据处理工具变成执行通道",
    remediation: "拦截使用 system() 的 jq 命令。",
  },
  {
    id: "TOOL_CMD_SUDO",
    tools: SHELL,
    params: COMMAND,
    severity: "MEDIUM",
    patterns: [/\bsudo\b/i, /\bdoas\b/i],
    excludePatterns: [/^\s*#/],
    description: "命令试图提升权限（sudo/doas）",
    remediation: "agent 原则上不应需要提权，与用户确认。",
  },
];

// ─────────────── 引擎（参考 harness 的 ToolGuardEngine 设计） ───────────────

export type ToolGuardMode = "block" | "warn" | "require_approval";

export interface GuardFinding {
  ruleId: string;
  severity: GuardSeverity;
  toolName: string;
  paramName: string;
  snippet: string;
  description: string;
  remediation: string;
}

const COMMENT_LINE = /^\s*#/;

function isCommentLineExclude(pattern: RegExp): boolean {
  return pattern.source === COMMENT_LINE.source && pattern.flags === COMMENT_LINE.flags;
}

/**
 * `/^\s*#/` 先按行丢掉注释行，剩下的文本继续匹配。
 * 其它排除模式命中整段参数时返回 null，本条规则不报警。
 */
function textForRule(value: string, rule: GuardRule): string | null {
  const excludes = rule.excludePatterns ?? [];
  const other = excludes.filter((p) => !isCommentLineExclude(p));
  if (other.some((p) => p.test(value))) return null;
  const comments = excludes.filter(isCommentLineExclude);
  if (comments.length === 0) return value;
  return value
    .split("\n")
    .filter((line) => !comments.some((p) => p.test(line)))
    .join("\n");
}

/** 各模式下"哪些严重度需要拦下来"——照搬 harness 的映射表 */
const BLOCKING_SEVERITIES: Record<ToolGuardMode, ReadonlySet<GuardSeverity>> = {
  block: new Set(["CRITICAL", "HIGH"]),
  require_approval: new Set(["CRITICAL", "HIGH", "MEDIUM"]),
  warn: new Set(), // warn 模式永不拦截
};

export class ToolGuardEngine {
  private rules: GuardRule[];
  private enabled: boolean;
  private mode: ToolGuardMode;

  constructor(opts: { enabled?: boolean; mode?: ToolGuardMode; rules?: GuardRule[] } = {}) {
    this.enabled = opts.enabled ?? true;
    this.mode = opts.mode ?? "block";
    this.rules = opts.rules ?? DEFAULT_RULES;
  }

  /** 运行时热改配置（对应 Python 版的 configure()） */
  configure(opts: { enabled?: boolean; mode?: ToolGuardMode }): void {
    if (opts.enabled !== undefined) this.enabled = opts.enabled;
    if (opts.mode !== undefined) this.mode = opts.mode;
  }

  get currentMode(): ToolGuardMode {
    return this.mode;
  }

  get isEnabled(): boolean {
    return this.enabled;
  }

  /** 扫描工具参数，返回告警列表（空 = 安全，直接放行） */
  guard(toolName: string, params: Record<string, unknown>): GuardFinding[] {
    if (!this.enabled) return [];
    const applicable = this.rules.filter((r) => !r.tools?.length || r.tools.includes(toolName));
    const findings: GuardFinding[] = [];
    for (const [paramName, raw] of Object.entries(params)) {
      const value = raw == null ? "" : String(raw);
      if (!value) continue;
      for (const rule of applicable) {
        if (rule.params?.length && !rule.params.includes(paramName)) continue;
        const scanned = textForRule(value, rule);
        if (scanned == null || !scanned.trim()) continue;
        const hit = rule.patterns.find((p) => p.test(scanned));
        if (!hit) continue;
        const m = scanned.match(hit);
        const idx = m?.index ?? 0;
        findings.push({
          ruleId: rule.id,
          severity: rule.severity,
          toolName,
          paramName,
          snippet: scanned.slice(Math.max(0, idx - 40), idx + (m?.[0].length ?? 0) + 40),
          description: rule.description,
          remediation: rule.remediation,
        });
      }
    }
    return findings;
  }

  /** 当前模式下这批告警是否需要拦下来（拦截或审批） */
  shouldBlock(findings: GuardFinding[]): boolean {
    const blocking = BLOCKING_SEVERITIES[this.mode];
    return findings.some((f) => blocking.has(f.severity));
  }
}

// ─────────── 中间件（重来：LangChain.js 原生 wrapToolCall 挂载点） ───────────

function formatFindings(findings: GuardFinding[], header: string): string {
  const lines = [header, `Tool: ${findings[0]?.toolName ?? "?"}`, ""];
  for (const f of findings.slice(0, 5)) {
    lines.push(`- [${f.severity}] ${f.description}`);
    lines.push(`  snippet: ${JSON.stringify(f.snippet)}`);
    lines.push(`  remediation: ${f.remediation}`);
  }
  if (findings.length > 5) lines.push(`... 以及另外 ${findings.length - 5} 条告警`);
  return lines.join("\n");
}

export interface ToolGuardMiddlewareHandle {
  middleware: ReturnType<typeof createMiddleware>;
  /** 暴露引擎引用，方便 REPL 里运行时切换模式 */
  engine: ToolGuardEngine;
}

/**
 * 工作机制（与 Python 版 awrap_tool_call 逐条对应）：
 *  1. 工具执行前，工具名+参数进引擎做正则匹配
 *  2. 无命中 → handler(request) 直接放行
 *  3. warn → 打印警告但放行（模棱两可的命令亮黄牌）
 *  4. require_approval → interrupt() 暂停整张图等人裁决：
 *     批准 → 继续执行；拒绝 → 返回"用户拒绝"的错误 ToolMessage
 *  5. block → 返回错误 ToolMessage，工具根本不执行
 */
export function toolGuardMiddleware(
  opts: { enabled?: boolean; mode?: ToolGuardMode; rules?: GuardRule[] } = {},
): ToolGuardMiddlewareHandle {
  const engine = new ToolGuardEngine(opts);

  const middleware = createMiddleware({
    name: "ToolGuardMiddleware",
    wrapToolCall: async (request, handler) => {
      const { id, name, args } = request.toolCall;
      const params = (args ?? {}) as Record<string, unknown>;
      if (!engine.isEnabled) return handler(request);

      const findings = engine.guard(name, params);
      if (findings.length === 0) return handler(request);

      if (engine.currentMode === "warn") {
        console.warn(`\n⚠️  [tool_guard] ${formatFindings(findings, "检测到风险参数（warn 模式，放行）")}\n`);
        return handler(request);
      }

      if (engine.currentMode === "require_approval" && engine.shouldBlock(findings)) {
        // 注意：resume 值不能是 false——LangGraph 会把 falsy 的 resume 当成空 Command 报错
        const approved = interrupt({
          kind: "tool_guard_approval",
          tool: name,
          args: params,
          message: formatFindings(findings, "工具调用需要人工批准（命中风险规则）"),
        });
        if (approved === "approve") return handler(request);
        return new ToolMessage({
          content: `用户拒绝了这次 ${name} 调用。不要重试相同命令；如需类似操作请先与用户商量替代方案。`,
          tool_call_id: id ?? "",
          name,
          status: "error",
        });
      }

      if (engine.shouldBlock(findings)) {
        return new ToolMessage({
          content: formatFindings(findings, "工具调用被安全策略拦截（command guard）"),
          tool_call_id: id ?? "",
          name,
          status: "error",
        });
      }

      return handler(request);
    },
  });

  return { middleware, engine };
}

