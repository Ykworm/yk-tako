// pdftext.go 是 read_pdf_text 的文本层抽取（doc/32 A8-1）。
// 只抽 PDF 里已经有的文字，不做 OCR，也不把抽出来的文本另存一份。
// 换解析库时只改这一层，工具回给模型的那几句格式留在 formatPDFText。
package runengine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

const (
	pdfMaxPages  = 10
	pdfTextLimit = 16 << 10
)

// pdfPageBody 是请求范围内的一页。Failed 表示这一页解析中途 panic，Text 不再使用。
type pdfPageBody struct {
	Num    int
	Text   string
	Failed bool
}

// pdfText 是一次抽取的结构化结果，还没拼成给模型看的字符串。
// From/To 是夹过默认值和页数之后、真正去读的页窗（1-based，含两端）。
// Clamped 为真表示调用方要的页数超过 10，而且文档在这个窗口后面还有没读的页。
type pdfText struct {
	Pages      int
	From, To   int
	Clamped    bool
	OutOfRange bool
	Scan       bool
	Bodies     []pdfPageBody
}

// extractPDFText 打开 full，按页抽出文本层。
// from/to 小于等于 0 表示用默认：起点 1，终点是起点往后共 3 页。
// 单次最多读 10 页。page_from 超过总页数时 OutOfRange 为真，不返回 error。
// 请求范围内每一页都没有文字、也没有解析失败时 Scan 为真。
// 打不开、加密、结构坏、ctx 已取消：返回 error，调用方把它交给 wrapToolErrors。
func extractPDFText(ctx context.Context, full string, from, to int) (out pdfText, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out = pdfText{}
			err = fmt.Errorf("无法读取 PDF：%v", rec)
		}
	}()
	if err := ctx.Err(); err != nil {
		return pdfText{}, err
	}
	f, r, err := pdf.Open(full)
	if err != nil {
		return pdfText{}, fmt.Errorf("无法读取 PDF：%v", err)
	}
	defer f.Close()

	n := r.NumPage()
	if n <= 0 {
		return pdfText{}, fmt.Errorf("PDF 没有页面")
	}
	out.Pages = n
	if from <= 0 {
		from = 1
	}
	if to <= 0 {
		to = from + 2
	}
	if to < from {
		to = from
	}
	capped := false
	if to-from+1 > pdfMaxPages {
		to = from + pdfMaxPages - 1
		capped = true
	}
	out.From, out.To = from, to
	if from > n {
		out.OutOfRange = true
		return out, nil
	}
	if to > n {
		to = n
		out.To = to
	}
	out.Clamped = capped && out.To-out.From+1 == pdfMaxPages && n > out.To

	bodies := make([]pdfPageBody, 0, out.To-out.From+1)
	anyText := false
	anyFailed := false
	for page := out.From; page <= out.To; page++ {
		if err := ctx.Err(); err != nil {
			return pdfText{}, err
		}
		body := pdfPageBody{Num: page}
		content, pageErr := pdfPageContent(r.Page(page))
		if pageErr != nil {
			body.Failed = true
			anyFailed = true
		} else {
			body.Text = strings.TrimSpace(pdfPageText(content))
			if body.Text != "" {
				anyText = true
			}
		}
		bodies = append(bodies, body)
	}
	out.Bodies = bodies
	out.Scan = !anyText && !anyFailed
	return out, nil
}

// pdfPageContent 把单页解析包进 recover。库的 Content 遇到坏操作符会 panic
// （例如内容流里只有一个 Tj），一页坏不能把整份 PDF 变成工具错误。
func pdfPageContent(p pdf.Page) (content pdf.Content, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			content = pdf.Content{}
			err = fmt.Errorf("本页无法抽取")
		}
	}()
	if p.V.IsNull() {
		return pdf.Content{}, nil
	}
	return p.Content(), nil
}

// pdfPageText 把一页的字形排成可读的行。
// PDF 的 Y 从下往上增长，所以 Y 更大的行先输出。同一行按 X 从左到右。
// 相邻字形的空隙大于左侧字号的 0.2 倍，并且两侧都是 ASCII 字母或数字时，才补一个空格。
// 中文两侧不补。目标是模型能找到句子，不是还原版式。
func pdfPageText(content pdf.Content) string {
	glyphs := make([]pdf.Text, 0, len(content.Text))
	for _, g := range content.Text {
		if g.S == "" {
			continue
		}
		glyphs = append(glyphs, g)
	}
	if len(glyphs) == 0 {
		return ""
	}
	lines := make([]pdfLine, 0)
	for _, g := range glyphs {
		placed := false
		for i := range lines {
			if math.Abs(lines[i].y-g.Y) <= pdfYTolerance(lines[i].gs[0].FontSize, g.FontSize) {
				lines[i].gs = append(lines[i].gs, g)
				placed = true
				break
			}
		}
		if !placed {
			lines = append(lines, pdfLine{y: g.Y, gs: []pdf.Text{g}})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].y > lines[j].y })
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, joinPDFGlyphs(line.gs))
	}
	return strings.Join(parts, "\n")
}

type pdfLine struct {
	y  float64
	gs []pdf.Text
}

func pdfYTolerance(a, b float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if m <= 0 {
		return 1
	}
	return 0.4 * m
}

func joinPDFGlyphs(gs []pdf.Text) string {
	sort.SliceStable(gs, func(i, j int) bool { return gs[i].X < gs[j].X })
	var b strings.Builder
	for i, g := range gs {
		if i > 0 && pdfGlyphNeedsSpace(gs[i-1], g) {
			b.WriteByte(' ')
		}
		b.WriteString(g.S)
	}
	return b.String()
}

func pdfGlyphNeedsSpace(left, right pdf.Text) bool {
	if left.FontSize <= 0 {
		return false
	}
	gap := right.X - (left.X + left.W)
	if gap <= 0.2*left.FontSize {
		return false
	}
	return pdfEdgeASCIIAlnum(left.S, true) && pdfEdgeASCIIAlnum(right.S, false)
}

func pdfEdgeASCIIAlnum(s string, last bool) bool {
	if s == "" {
		return false
	}
	var r rune
	if last {
		r, _ = utf8.DecodeLastRuneInString(s)
	} else {
		r, _ = utf8.DecodeRuneInString(s)
	}
	return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// formatPDFText 把抽取结果拼成工具返回的正文。path 是调用方传入的路径原文。
// 返回的第二个值是真正写进正文的页文本的 rune 数（不含页眉、页标记和截断说明）。
// 整段正文超过 16KB 时在 rune 边界截断，并在末尾追加说明；说明本身可以超出 16KB。
func formatPDFText(path string, got pdfText) (string, int) {
	if got.OutOfRange {
		return fmt.Sprintf("[pdf path=%s pages=%d；page_from 超出范围]", path, got.Pages), 0
	}
	if got.Scan {
		return fmt.Sprintf("[pdf path=%s pages=%d；未检测到文本层（可能是扫描件），无法抽取文本；可让用户上传页面截图]", path, got.Pages), 0
	}
	header := fmt.Sprintf("[pdf path=%s pages=%d；第 %d-%d 页；文本层抽取", path, got.Pages, got.From, got.To)
	if got.Clamped {
		header += "；单次最多 10 页，已夹取"
	}
	header += "]"

	var b strings.Builder
	b.WriteString(header)
	chars := 0
	truncated := false
	for _, page := range got.Bodies {
		if page.Failed {
			line := fmt.Sprintf("\n--- page %d ---（本页无法抽取）", page.Num)
			if b.Len()+len(line) > pdfTextLimit {
				truncated = true
				break
			}
			b.WriteString(line)
			continue
		}
		if page.Text == "" {
			line := fmt.Sprintf("\n--- page %d ---（本页无文本层）", page.Num)
			if b.Len()+len(line) > pdfTextLimit {
				truncated = true
				break
			}
			b.WriteString(line)
			continue
		}
		marker := fmt.Sprintf("\n--- page %d ---\n", page.Num)
		room := pdfTextLimit - b.Len() - len(marker)
		if room <= 0 {
			truncated = true
			break
		}
		text := page.Text
		if len(text) > room {
			text = clipUTF8(text, room)
			truncated = true
		}
		b.WriteString(marker)
		b.WriteString(text)
		chars += utf8.RuneCountInString(text)
		if truncated {
			break
		}
	}
	if truncated {
		b.WriteString("\n（本页文本超过单次上限，已截断；请缩小页范围）")
	}
	return b.String(), chars
}

func clipUTF8(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := 0
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 || i+size > max {
			break
		}
		i += size
		cut = i
	}
	return s[:cut]
}
