// 本文件：压缩时那一次检索的词重叠。没有向量模型，命中的正文再截断后放进提示。
package compact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ragHitLimit 是一次检索放进提示的条数。再多就会把刚腾出来的位置填回去。
	ragHitLimit = 3
	// ragHitRunes 是单条命中放进提示的字数上限。
	ragHitRunes = 800
	// ragTermLimit 避免把一整段用户原话拆成过多的检索词。
	ragTermLimit = 32
)

// SearchBodies 在若干段旧原文里按词重叠取最多 3 条。查询为空或没有命中时返回空字符串。
func SearchBodies(query string, bodies []string) string {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return ""
	}
	type hit struct {
		score int
		index int
		text  string
	}
	var hits []hit
	for i, body := range bodies {
		score := overlap(terms, body)
		if score == 0 || strings.TrimSpace(body) == "" {
			continue
		}
		hits = append(hits, hit{score: score, index: i, text: clipRunes(strings.TrimSpace(body), ragHitRunes)})
	}
	if len(hits) == 0 {
		return ""
	}
	// 分数高的在前；分数相同保持写入顺序，测试和回放才稳定。
	for i := 1; i < len(hits); i++ {
		j := i
		for j > 0 && (hits[j].score > hits[j-1].score || (hits[j].score == hits[j-1].score && hits[j].index < hits[j-1].index)) {
			hits[j], hits[j-1] = hits[j-1], hits[j]
			j--
		}
	}
	if len(hits) > ragHitLimit {
		hits = hits[:ragHitLimit]
	}
	parts := make([]string, len(hits))
	for i, item := range hits {
		parts[i] = item.text
	}
	return strings.Join(parts, "\n")
}

// queryTerms 把查询拆成词。中文没有空格时再取连续两个字，否则整句对不上正文里的词。
func queryTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	var out []string
	seen := map[string]struct{}{}
	add := func(term string) {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			return
		}
		if _, ok := seen[term]; ok {
			return
		}
		if len(out) >= ragTermLimit {
			return
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	for _, field := range fields {
		add(field)
		if !containsCJK(field) {
			continue
		}
		runes := []rune(field)
		if len(runes) < 2 {
			continue
		}
		for i := 0; i+2 <= len(runes); i++ {
			add(string(runes[i : i+2]))
		}
	}
	return out
}

// overlap 数有多少个词出现在正文里。大小写不敏感，中文不受影响。
func overlap(terms []string, body string) int {
	hay := strings.ToLower(body)
	n := 0
	for _, term := range terms {
		if strings.Contains(hay, term) {
			n++
		}
	}
	return n
}

// containsCJK 判断这段里有没有汉字，用来决定要不要拆二字词。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// clipRunes 按字截断。用字节截会把汉字切断。
func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}
