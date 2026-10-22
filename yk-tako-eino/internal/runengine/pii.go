// pii.go 是 PII 脱敏的底座（方案真源 = doc/41 §3.2；旧线口径 = doc/06 §3.3）。
//
// 一份扫描与掩码规则，三个消费侧共用：
//
//	① 工具统一出口（wrapToolErrors 的成功/错误返回路径）——`cat .env` 读到的密钥、**MCP 工具**
//	   返回的外部内容，都在这一处被打码，然后才被外层的 journalTool 记账、再成帧、再进
//	   mh/runlog → **落库的就是打码后的**。
//	② 模型输入的唯一转换口（toSchemaMessages）——首轮装配、mid-run fold、救援采用都走它，
//	   打在这里三条路一次覆盖。**不能只挂 prepareInput**：折叠的原料是 mh 原文（user 行按设计
//	   存原文给页面显示）→ 只管首轮的话，折叠一次 PII 就复活（短对话安全、长对话漏）。
//	③ 记忆抽取入库前（memory.go）——记忆要复用，密钥类整条拒收，证件类打码后入库。
//
// 规则继承 doc/06 旧线（7 条密钥正则、头尾各 4 位打星、幂等），扩到证件类（身份证/手机/邮箱/银行卡）。
package runengine

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
)

// PIIKind 是命中项的类别。密钥类整条拒收（不入记忆），证件类打码后可用。
type PIIKind string

const (
	PIIAPIKey PIIKind = "api_key" // 各类 API Key / token
	PIISecret PIIKind = "secret"  // 密码类（赋值形式里带 password/passwd 的）
	PIIIDCard PIIKind = "id_card" // 身份证号
	PIIPhone  PIIKind = "phone"   // 手机号
	PIIEmail  PIIKind = "email"   // 邮箱
	PIICard   PIIKind = "bank_card"
)

// PIIHit 是一处命中：kind + 原文区间 [Start, End)。
type PIIHit struct {
	Kind  PIIKind `json:"kind"`
	Start int     `json:"start"`
	End   int     `json:"end"`
}

// IsSecretKind 报「这类命中整条拒收」（记忆线用）：密钥类不进记忆库，证件类可打码后进。
func (k PIIKind) IsSecretKind() bool {
	return k == PIIAPIKey || k == PIISecret
}

// piiRule 是一条规则。group=0 表示掩整个匹配；group>0 表示只掩第 group 个子组
// （赋值形式只掩 value）。keepHead/keepTail 是掩码后保留的头尾字符数。
type piiRule struct {
	kind               PIIKind
	re                 *regexp.Regexp
	group              int
	keepHead, keepTail int
	email              bool // 邮箱特殊：掩本地部分、保留域名
}

// piiRules 的顺序有意义：**更具体的规则在前**。Go 正则没有负向前瞻，旧线靠「起点已被更具体
// 的规则占住」避开 sk-proj- / sk-ant- 被 sk- 抢先命中；这里靠**区间去重**实现同一件事
// （重叠时保留规则序号更小的那个）。
var piiRules = []piiRule{
	// —— 密钥类（doc/06 的 7 条，顺序一致）——
	{kind: PIIAPIKey, re: regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{8,}`), keepHead: 4, keepTail: 4},
	{kind: PIIAPIKey, re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`), keepHead: 4, keepTail: 4},
	{kind: PIIAPIKey, re: regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`), keepHead: 4, keepTail: 4},
	{kind: PIIAPIKey, re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), keepHead: 4, keepTail: 4},
	{kind: PIIAPIKey, re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), keepHead: 4, keepTail: 4},
	{kind: PIIAPIKey, re: regexp.MustCompile(`hf_[0-9A-Za-z]{34}`), keepHead: 4, keepTail: 4},
	// 赋值形式：只掩 value（group 2），键名留下
	{kind: PIIAPIKey, re: regexp.MustCompile(`(?i)\b(api_key|secret_key|access_token)\s*[:=]\s*["']?([A-Za-z0-9_\-\.]{8,})["']?`), group: 2, keepHead: 4, keepTail: 4},
	{kind: PIISecret, re: regexp.MustCompile(`(?i)\b(password|passwd|pwd)\s*[:=]\s*["']?([^\s"'&;]{6,})["']?`), group: 2, keepHead: 2, keepTail: 2},

	// —— 证件类 ——
	{kind: PIIIDCard, re: regexp.MustCompile(`\b[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`), keepHead: 6, keepTail: 4},
	{kind: PIIPhone, re: regexp.MustCompile(`\b1[3-9]\d{9}\b`), keepHead: 3, keepTail: 4},
	{kind: PIIEmail, re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), keepHead: 1, keepTail: 0, email: true},
	{kind: PIICard, re: regexp.MustCompile(`\b\d{16,19}\b`), keepHead: 4, keepTail: 4},
}

// MaskPII 扫一遍文本，返回打码后的文本与命中列表。
//
// 幂等：打码结果再扫一遍，命中不到（掩码把分隔字符换成了 `*`，正则的字符类不含 `*`）。
func MaskPII(text string) (string, []PIIHit) {
	if text == "" {
		return text, nil
	}
	type span struct {
		start, end int
		kind       PIIKind
		rule       int
	}
	var spans []span
	for ri, r := range piiRules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[0], m[1]
			if r.group > 0 {
				if 2*r.group+1 >= len(m) {
					continue
				}
				s, e = m[2*r.group], m[2*r.group+1]
			}
			if e <= s {
				continue
			}
			spans = append(spans, span{start: s, end: e, kind: r.kind, rule: ri})
		}
	}
	if len(spans) == 0 {
		return text, nil
	}
	// 去重：起点相同取规则更靠前的（更具体）；区间重叠只留先到的那个。
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		if spans[i].rule != spans[j].rule {
			return spans[i].rule < spans[j].rule
		}
		return spans[i].end > spans[j].end
	})
	kept := make([]span, 0, len(spans))
	for _, sp := range spans {
		overlap := false
		for _, k := range kept {
			if sp.start < k.end && k.start < sp.end {
				overlap = true
				break
			}
		}
		if !overlap {
			kept = append(kept, sp)
		}
	}
	// 从后往前替换：右边的替换不改变左边的下标。
	out := text
	hits := make([]PIIHit, 0, len(kept))
	for i := len(kept) - 1; i >= 0; i-- {
		sp := kept[i]
		out = out[:sp.start] + maskSpan(text[sp.start:sp.end], sp.kind, piiRules[sp.rule]) + out[sp.end:]
		hits = append(hits, PIIHit{Kind: sp.kind, Start: sp.start, End: sp.end})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Start < hits[j].Start })
	return out, hits
}

// maskSpan 按规则掩一段文本。邮箱特殊：掩本地部分、保留域名（域名要留着给人核对）。
func maskSpan(s string, kind PIIKind, r piiRule) string {
	if r.email {
		at := strings.LastIndex(s, "@")
		if at <= 0 {
			return maskKeep(s, 1, 0)
		}
		return maskKeep(s[:at], 1, 0) + s[at:]
	}
	return maskKeep(s, r.keepHead, r.keepTail)
}

// maskKeep 保留头尾各若干字符，中间打星（长度与原文同 → 下游的字节数不跳变）。
func maskKeep(s string, head, tail int) string {
	r := []rune(s)
	n := len(r)
	if n == 0 {
		return s
	}
	if n <= head+tail {
		return strings.Repeat("*", n)
	}
	return string(r[:head]) + strings.Repeat("*", n-head-tail) + string(r[n-tail:])
}

// piiToolWrapper 在工具结果返回前打码（出工具方向）。
//
// 它套在 error handler 之外：error handler 已把工具错误变成「错误：…」文本结果，
// 所以这里看到的一律是成功返回 → 成功与错误两条路径都打得到。
type piiToolWrapper struct{ inner tool.BaseTool }

func (w piiToolWrapper) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return w.inner.Info(ctx)
}

func (w piiToolWrapper) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	it, ok := w.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("工具不可调用")
	}
	out, err := it.InvokableRun(ctx, argumentsInJSON, opts...)
	if err != nil {
		return "", err
	}
	masked, _ := MaskPII(out)
	return masked, nil
}

// hasSecretPII 报「命中里有密钥类」（记忆线用：这类整条拒收，不进记忆库）。
func hasSecretPII(hits []PIIHit) bool {
	for _, h := range hits {
		if h.Kind.IsSecretKind() {
			return true
		}
	}
	return false
}

// devPiiCheck 是 PII 打码探针（脚手架：长期保留，纯函数无状态——对齐 gate-check 先例）。
//
//	POST /api/dev/pii-check {"text":"key=sk-proj-AbCdEfGhIjKlMnOpQrSt"}
//	200 {"code":0,"message":"","data":{"masked":"key=sk-p******************QrSt","hits":[{"kind":"api_key","start":4,"end":30}]}}
func (s *Server) devPiiCheck(c *gin.Context) {
	if _, ok := s.requirePrincipal(c); !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	masked, hits := MaskPII(req.Text)
	if hits == nil {
		hits = []PIIHit{}
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		Masked string   `json:"masked"`
		Hits   []PIIHit `json:"hits"`
	}{Masked: masked, Hits: hits})
}

// MaskPIIKeepUserMasked 供模型输入侧用：把 mh 行文本里的 PII 打码。
// 工具产出的那几行在入库前已打码（①），再打一次是无害的幂等操作；用户行则是第一次被打码。
func MaskPIIKeepUserMasked(text string) string {
	masked, _ := MaskPII(text)
	return masked
}
