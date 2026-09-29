// 本文件：词重叠检索。空查询不返回全部原文，中文没有空格时用连续两个字。
package compact

import (
	"strings"
	"testing"
)

// TestSearchBodiesChineseBigram 钉住：没有空格的中文查询能命中正文里的两个字，空查询返回空。
func TestSearchBodiesChineseBigram(t *testing.T) {
	if got := SearchBodies("", []string{"压缩算法"}); got != "" {
		t.Fatalf("空查询 = %q", got)
	}
	if got := SearchBodies("   ", []string{"压缩算法"}); got != "" {
		t.Fatalf("空白查询 = %q", got)
	}
	bodies := []string{"这里没有", "这段讲压缩算法的来历"}
	got := SearchBodies("压缩算法", bodies)
	if !strings.Contains(got, "压缩算法") {
		t.Fatalf("命中 = %q", got)
	}
	if strings.Contains(got, "这里没有") {
		t.Fatalf("未命中的原文不该出现：%q", got)
	}
}

// TestSearchBodiesTopThreeAndClip 钉住：最多 3 条，分数高的在前，单条超过 800 个字会被截断。
func TestSearchBodiesTopThreeAndClip(t *testing.T) {
	long := strings.Repeat("长", 900) + "压缩算法"
	bodies := []string{
		long,
		"只提到压缩",
		"压缩",
		"也只提到压缩",
		"无关",
	}
	got := SearchBodies("压缩算法", bodies)
	parts := strings.Split(got, "\n")
	if len(parts) != ragHitLimit {
		t.Fatalf("条数 = %d，want %d，正文 %q", len(parts), ragHitLimit, got)
	}
	if gotRunes := len([]rune(parts[0])); gotRunes != ragHitRunes {
		t.Fatalf("第一条 %d 个字，want %d", gotRunes, ragHitRunes)
	}
	if strings.Contains(got, "无关") {
		t.Fatalf("未命中的原文不该出现：%q", got)
	}
}
