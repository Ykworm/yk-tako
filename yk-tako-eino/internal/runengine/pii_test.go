// pii_test.go 是 PII 脱敏的钉子网（方案真源 = doc/41 §3.2；旧线口径 = doc/06 §3.3）。
//
// 金样覆盖：密钥类（doc/06 的 7 条家族 + 赋值形式）、证件类（身份证/手机/邮箱/银行卡）、
// 幂等（打码结果再扫一遍不变）、无命中（原文原样返回）、区间去重（身份证不被银行卡规则抢走）。
package runengine

import (
	"strings"
	"testing"
)

// TestMaskPIISecrets：密钥类——头尾各 4 位、中间打星（doc/06 的 maskToken 口径）。
func TestMaskPIISecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		kind PIIKind
	}{
		{"sk-proj-", "key=sk-proj-AbCdEfGhIjKlMnOpQrSt", "key=sk-p********************QrSt", PIIAPIKey},
		{"sk-ant-", "sk-ant-AbCdEfGhIjKlMnOpQrSt", "sk-a*******************QrSt", PIIAPIKey},
		{"sk- 家族", "sk-AbCdEfGhIjKlMnOpQrSt", "sk-A***************QrSt", PIIAPIKey},
		{"AWS AKIA", "AKIAIOSFODNN7EXAMPLE", "AKIA************MPLE", PIIAPIKey},
		{"Google AIza", "AIzaSyD-1234567890abcdefghijklmnopqrstu", "AIza*******************************rstu", PIIAPIKey},
		{"HuggingFace hf_", "hf_abcdefghijklmnopqrstuvwxyz01234567", "hf_a*****************************4567", PIIAPIKey},
		{"赋值形式（只掩 value）", "api_key=AbCdEfGhIjKl", "api_key=AbCd****IjKl", PIIAPIKey},
	}
	for _, c := range cases {
		masked, hits := MaskPII(c.in)
		if masked != c.want {
			t.Errorf("%s: masked = %q, want %q", c.name, masked, c.want)
		}
		if len(hits) != 1 {
			t.Errorf("%s: hits = %+v, want 1 条", c.name, hits)
			continue
		}
		if hits[0].Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q", c.name, hits[0].Kind, c.kind)
		}
	}
}

// TestMaskPIIIdentifiers：证件类——身份证保头 6 尾 4、手机保头 3 尾 4、邮箱掩本地部分保域名。
func TestMaskPIIIdentifiers(t *testing.T) {
	in := "身份证 110101199003077777，手机 13812345678，邮箱 a@b.com"
	masked, hits := MaskPII(in)
	want := "身份证 110101********7777，手机 138****5678，邮箱 *@b.com"
	if masked != want {
		t.Fatalf("masked = %q, want %q", masked, want)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %+v, want 3 条（身份证/手机/邮箱）", hits)
	}
	kinds := map[PIIKind]bool{}
	for _, h := range hits {
		kinds[h.Kind] = true
	}
	for _, k := range []PIIKind{PIIIDCard, PIIPhone, PIIEmail} {
		if !kinds[k] {
			t.Errorf("缺少 %q 命中", k)
		}
	}
	// 身份证（18 位）不该被银行卡规则抢走：区间重叠时规则更靠前的赢。
	if kinds[PIICard] {
		t.Errorf("身份证被银行卡规则重复命中（区间去重失效）")
	}
}

// TestMaskPIIIdempotent：幂等——打码结果再扫一遍不变（doc/06 的 MaskSecrets 口径）。
func TestMaskPIIIdempotent(t *testing.T) {
	for _, in := range []string{
		"key=sk-proj-AbCdEfGhIjKlMnOpQrSt",
		"身份证 110101199003077777",
		"手机 13812345678，邮箱 zhangsan@example.com",
		"api_key=AbCdEfGhIjKl",
	} {
		once, _ := MaskPII(in)
		twice, _ := MaskPII(once)
		if once != twice {
			t.Errorf("不幂等：%q → %q → %q", in, once, twice)
		}
	}
}

// TestMaskPIIHitsSpans：命中区间要能对上原文（探针的 hits 要准）。
func TestMaskPIIHitsSpans(t *testing.T) {
	in := "token=AKIAIOSFODNN7EXAMPLE end"
	_, hits := MaskPII(in)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want 1 条", hits)
	}
	h := hits[0]
	if in[h.Start:h.End] != "AKIAIOSFODNN7EXAMPLE" {
		t.Fatalf("区间 [%d:%d] = %q，不是密钥原文", h.Start, h.End, in[h.Start:h.End])
	}
}

// TestMaskPIIPlainTextUnchanged：没命中就原样返回（不产生无谓的星号）。
func TestMaskPIIPlainTextUnchanged(t *testing.T) {
	in := "今天把报告整理好了，放在 docs/report.md。"
	masked, hits := MaskPII(in)
	if masked != in {
		t.Errorf("无命中不该改文本：%q → %q", in, masked)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want 空", hits)
	}
}

// TestMaskPIIBankCard：银行卡保头 4 尾 4（16–19 位）。
func TestMaskPIIBankCard(t *testing.T) {
	in := "卡号 6222021234567890123"
	masked, hits := MaskPII(in)
	if !strings.Contains(masked, "6222") || !strings.Contains(masked, "0123") {
		t.Fatalf("masked = %q, want 保留头 4 尾 4", masked)
	}
	if strings.Contains(masked, "1234567890") {
		t.Fatalf("中间未掩：%q", masked)
	}
	if len(hits) != 1 || hits[0].Kind != PIICard {
		t.Fatalf("hits = %+v, want 1 条 bank_card", hits)
	}
}
