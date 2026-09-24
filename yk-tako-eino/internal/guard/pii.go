package guard

import (
	"regexp"
	"sort"
)

// span 是文本里一段待脱敏的区间。
type span struct {
	start int
	end   int
}

// 顺序和 harness 的检测器一致。Go 的正则没有负向前瞻，openai 这一条靠“起点已被更具体的规则占住”来避开 sk-proj- / sk-ant-。
var piiPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`(?i)(?:api[_\s-]?key|secret[_\s-]?key|access[_\s-]?token)\s*[=:]\s*['"]?([A-Za-z0-9_\-+/=]{20,})['"]?`),
}

// MaskSecrets 把 API Key 打成头尾保留、中间打星。同一段文本再跑一遍不会二次命中。
func MaskSecrets(text string) string {
	if text == "" {
		return text
	}
	var spans []span
	seen := map[int]bool{}
	for i, pattern := range piiPatterns {
		matches := pattern.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			start, end := match[0], match[1]
			if i == len(piiPatterns)-1 && len(match) >= 4 && match[2] >= 0 {
				start, end = match[2], match[3]
			}
			if seen[start] {
				continue
			}
			seen[start] = true
			spans = append(spans, span{start, end})
		}
	}
	if len(spans) == 0 {
		return text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	for i := len(spans) - 1; i >= 0; i-- {
		item := spans[i]
		if item.start < 0 || item.end > len(text) || item.start >= item.end {
			continue
		}
		text = text[:item.start] + maskToken(text[item.start:item.end]) + text[item.end:]
	}
	return text
}

// maskToken 保留头尾各 4 位、中间打星；太短的整个打星。
func maskToken(value string) string {
	if len(value) <= 8 {
		return "********"[:len(value)]
	}
	return value[:4] + "*****...*****" + value[len(value)-4:]
}
