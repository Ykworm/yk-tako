/**
 * API Key PII 脱敏（L1）
 *
 * 复刻自 harness-agent 的 middleware/pii.py —— 它自己也是"框架自带
 * PIIMiddleware + 自定义检测器"的组合，因为内置检测器不认识 API Key。
 * JS 版同样自带 piiMiddleware，所以我们做一模一样的事：
 * 正则知识借过来，挂到 JS 框架的原生 PII 中间件上。
 *
 * mask 策略效果：sk-4b829b7b-...-18b0025594f2 → sk-4*****...*****94f2
 */
import { piiMiddleware } from "langchain";
import type { PIIMatch } from "langchain";

/** 模式表与 harness pii.py 的 _PATTERNS 一一对应 */
const PATTERNS: Array<[string, RegExp]> = [
  ["openai_project", /sk-proj-[A-Za-z0-9_-]{20,}/g],
  ["anthropic", /sk-ant-[A-Za-z0-9_-]{20,}/g],
  ["openai", /sk-(?!proj-|ant-)[A-Za-z0-9_-]{20,}/g],
  ["aws_access_key_id", /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g],
  ["google_api_key", /\bAIza[0-9A-Za-z_-]{35}\b/g],
  ["huggingface", /\bhf_[A-Za-z0-9]{30,}\b/g],
  // api_key=... / secret_key: ... 形式的显式赋值，只标 value 部分
  ["generic_api_key_assignment", /(?:api[_\s-]?key|secret[_\s-]?key|access[_\s-]?token)\s*[=:]\s*['"]?([A-Za-z0-9_\-+/=]{20,})['"]?/gi],
];

/** 与 harness 的 detect_pii 对应：返回匹配区间列表（JS 版 PIIMatch 形状为 {text, start, end}） */
export function detectApiKeys(text: string): PIIMatch[] {
  const seenStarts = new Set<number>();
  const matches: PIIMatch[] = [];
  for (const [, pattern] of PATTERNS) {
    pattern.lastIndex = 0;
    for (const m of text.matchAll(pattern)) {
      // 有捕获组时只脱敏 value 部分，保留 "api_key=" 前缀
      const value = m[1] ?? m[0];
      const start = m.index + (m[1] ? m[0].indexOf(m[1]) : 0);
      if (seenStarts.has(start)) continue;
      seenStarts.add(start);
      matches.push({ text: value, start, end: start + value.length });
    }
  }
  return matches.sort((a, b) => a.start - b.start);
}

/**
 * applyToInput: 用户消息进模型前脱敏
 * applyToToolResults: 工具输出（如 cat .env 的结果）也脱敏 ——
 * 这是 harness 文档里特别强调的场景：粘贴的报错日志、.env 内容
 * 会进会话日志和下一轮上下文。
 */
export const apiKey_Pii_Middleware = piiMiddleware("api_key_pii", {
  detector: detectApiKeys,
  strategy: "mask",
  applyToInput: true,
  applyToToolResults: true,
});
