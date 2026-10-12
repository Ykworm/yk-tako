// 本文件：溢出检出①（19v6-09 §4）——provider 拒收「太长」的报错文本分类 + 窗真值提取（纯函数层）。
//
// 病灶（19v6-09 §1 戏二）：发送前的 cl100k 估算在多 provider 世界系统性有误差面（分词器偏差、
// 工具 schema 不计入），估算放行 → provider 以「太长」打回 → 今天这只是一句普通 error 日志：
// run 死、历史不变、下条消息再死一次。「该压缩了」这个信息明明写在报错里，却没人读。
// 本文件就是「读它」：命中表 24 条 + 排除表 4 条，**先排除后命中**，照抄 WeKnora
// internal/agent/compaction/overflow.go（2026-10-11 实读版本，19v6-09 §4），一条不改；
// 另加窗真值提取（D4 校准的输入之一；2026-10-12 活体校准：真报错里窗是候选独立数中的最小者）。
//
// 为什么排除表先跑（19v6-09 §4）：Bedrock 的限流报文是「ThrottlingException: Too many tokens,
// please wait before trying again」——命中表第 23 条「too many tokens」会把它误判成超长。
// 误判代价不对称：溢出误判成限流 = 同一份超窗请求重试到耗尽（慢死）；限流误判成溢出 =
// 白白压掉历史换一次重试（历史冤死）。两害相权先认排除。
//
// 组件错误形状（实读结论，fixtures 钉在 overflow_test.go）：eino-ext openai 系把 provider
// 错误体包成 `error, status code: 400, status: 400 Bad Request, message: <provider 原文>`
// （components/model/openai types.go APIError.Error / convOrigAPIError）；claude 系包成
// `create new streaming message fail: <SDK 错误原文>`（components/model/claude claude.go
// Stream）。两家都保留 provider 原文 → 在 .Error() 上子串匹配即可。验证范围 = v6 在用协议
// 两家（doc §4 原话：其余 22 条照抄，不逐一搭环境）。
//
// A1 只做观测接线（worker event.Err 分支的 detect 行，行为零变化：run 仍 status=error、
// 零写入）；救援机械件（谓词/强折/重跑）是 A2。
package runengine

import (
	"regexp"
	"strconv"
)

// overflowPatterns 是「太长」命中表（24 条，照抄 WeKnora，一条不改；(?i) 不分大小写）。
// 模型调用返回的 error 取 .Error() 文本逐条匹配，命中即判溢出；括注 = 该条出处厂商。
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),                                  // Anthropic
	regexp.MustCompile(`(?i)request_too_large`),                                   // Anthropic（HTTP 413）
	regexp.MustCompile(`(?i)input is too long for requested model`),               // Amazon Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                          // OpenAI
	regexp.MustCompile(`(?i)exceeds (the )?(model'?s )?maximum context length`),   // OpenAI 兼容代理
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),              // Google Gemini
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                        // xAI Grok
	regexp.MustCompile(`(?i)reduce the length of the messages`),                   // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                // OpenRouter
	regexp.MustCompile(`(?i)exceeds (the )?maximum allowed input length`),         // OpenRouter / Poolside
	regexp.MustCompile(`(?i)is longer than the model'?s context length`),          // Together AI
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                            // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                  // llama.cpp
	regexp.MustCompile(`(?i)greater than the context length`),                     // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                        // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                          // Kimi
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`), // Mistral
	regexp.MustCompile(`(?i)but the configured context size is`),                  // DS4
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                       // z.ai
	regexp.MustCompile(`(?i)prompt too long; exceeded (max )?context length`),     // Ollama
	regexp.MustCompile(`(?i)range of input length should be`),                     // DashScope / 通义
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                       // 通用
	regexp.MustCompile(`(?i)too many tokens`),                                     // 通用
	regexp.MustCompile(`(?i)token limit exceeded`),                                // 通用
}

// nonOverflowPatterns 是排除表（4 条）：命中表里「too many tokens」这类词会撞上限流报文
// （Bedrock 的「ThrottlingException: Too many tokens, please wait」正是如此）。先过本表，
// 命中即判非溢出——顺序是语义不是风格（见文件头）。
var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)throttling`),
	regexp.MustCompile(`(?i)service unavailable`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
}

// IsOverflowError 判一个模型调用错误是不是「请求装不下窗」。nil = false。
// 先在排除表上过一遍（命中即判非溢出），再看命中表——顺序见文件头。
func IsOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, p := range nonOverflowPatterns {
		if p.MatchString(msg) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(msg) {
			return true
		}
	}
	return false
}

// windowCandidateFloor 是窗真值候选的下限（doc §6 原话：防把「retry 3 times」的 3 当真值）。
const windowCandidateFloor = 1024

// windowNumberPattern 抓报错文本里的数字串（是否独立数由 isEmbeddedNumber 旁判——RE2 无环视）。
var windowNumberPattern = regexp.MustCompile(`\d+`)

// extractWindow 从报错文本里提窗真值 W'（D4 校准的输入之一：被拒本身已证明「真窗 < 被拒量」，
// 拿原 limit 当验收线 = 用谎言校对谎言）。
//
// 规则（2026-10-12 活体校准修订，证据 = tmp/19-v6/evidence-overflow/a1-provider-probe.md）：
// 独立整数（两侧不紧邻字母/数字/下划线，防 request_id 十六进制串混入）里 **≥1024 的取最小**。
// 报文形状恒是「maximum … is W。you requested P（P1 in the messages, P2 in the completion）」——
// W 是被超过的上限，P 及其分解都不小于 W → 最小者即窗。live 实录（DeepSeek，2026-10-12）：
// `maximum context length is 1048576 tokens. However, you requested 1400065 tokens
// (1400049 in the messages, 16 in the completion)` → 取 1048576。
// doc §6 的原文规则（「不超过被拒估算 C_rej 的最大整数」）在真栈上不成立：cl100k 对本栈
// DeepSeek 分词器高估一倍余，C_rej 比报文里所有数都大，该规则取到 1400065（= 被拒量）而非窗。
// 已知弱侧：报错若携带比窗更小的分解数，会取到分解数——保守方向（D4 折更狠，安全侧），
// 错偏由 D4 的 min 与盲降出口兜底；提不出 = ok false（调用方盲降一档，D4 后半句）。
func extractWindow(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	msg := err.Error()
	best, found := 0, false
	for _, loc := range windowNumberPattern.FindAllStringIndex(msg, -1) {
		if isEmbeddedNumber(msg, loc[0]-1) || isEmbeddedNumber(msg, loc[1]) {
			continue
		}
		n, cerr := strconv.Atoi(msg[loc[0]:loc[1]])
		if cerr != nil || n < windowCandidateFloor {
			continue
		}
		if !found || n < best {
			best, found = n, true
		}
	}
	return best, found
}

// isEmbeddedNumber 判 msg[idx] 处是否让相邻数字串算不上独立数（越过边界 = 不算）。
// 字母/数字/下划线都算「同属一个大 token」——request_id 的十六进制串（`ce623e6986b8` 里的
// 6986）由此被挡在候补之外。
func isEmbeddedNumber(msg string, idx int) bool {
	if idx < 0 || idx >= len(msg) {
		return false
	}
	c := msg[idx]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}
