package runengine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

func TestPDFExtract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "three.pdf")
	streams := []string{
		pdfTextStream("Page one: apple"),
		pdfTextStream("Page two: banana"),
		pdfTextStream("Page three: orange"),
	}
	if err := os.WriteFile(path, buildTestPDF(streams), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := extractPDFText(context.Background(), path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pages != 3 || got.From != 1 || got.To != 3 || got.OutOfRange || got.Scan {
		t.Fatalf("默认页窗错: %+v", got)
	}
	text, chars := formatPDFText("notes/three.pdf", got)
	if !strings.Contains(text, "[pdf path=notes/three.pdf pages=3；第 1-3 页；文本层抽取]") {
		t.Fatalf("页眉错: %q", text)
	}
	if !strings.Contains(text, "--- page 2 ---\nPage two: banana") || !strings.Contains(text, "Page one: apple") {
		t.Fatalf("页文本错: %q", text)
	}
	if chars != len("Page one: apple")+len("Page two: banana")+len("Page three: orange") {
		t.Fatalf("chars=%d", chars)
	}

	got, err = extractPDFText(context.Background(), path, 2, 2)
	if err != nil || got.From != 2 || got.To != 2 || len(got.Bodies) != 1 || got.Bodies[0].Text != "Page two: banana" {
		t.Fatalf("单页范围错: %+v err=%v", got, err)
	}

	got, err = extractPDFText(context.Background(), path, 99, 99)
	if err != nil || !got.OutOfRange || got.Pages != 3 {
		t.Fatalf("超出范围应是正常结果: %+v err=%v", got, err)
	}
	text, _ = formatPDFText("notes/three.pdf", got)
	if text != "[pdf path=notes/three.pdf pages=3；page_from 超出范围]" {
		t.Fatalf("超出范围文案错: %q", text)
	}
}

func TestPDFEmptyAndScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mix.pdf")
	raw := buildTestPDF([]string{
		pdfTextStream("only"),
		" ",
		pdfTextStream("tail"),
	})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractPDFText(context.Background(), path, 1, 3)
	if err != nil || got.Scan || len(got.Bodies) != 3 || got.Bodies[1].Text != "" || got.Bodies[1].Failed {
		t.Fatalf("空页应留下标记而不是整篇扫描: %+v err=%v", got, err)
	}
	text, _ := formatPDFText("mix.pdf", got)
	if !strings.Contains(text, "--- page 2 ---（本页无文本层）") || strings.Contains(text, "未检测到文本层") {
		t.Fatalf("空页文案错: %q", text)
	}

	scanPath := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(scanPath, buildTestPDF([]string{" ", " "}), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = extractPDFText(context.Background(), scanPath, 0, 0)
	if err != nil || !got.Scan {
		t.Fatalf("全空应是扫描件: %+v err=%v", got, err)
	}
	text, chars := formatPDFText("scan.pdf", got)
	want := "[pdf path=scan.pdf pages=2；未检测到文本层（可能是扫描件），无法抽取文本；可让用户上传页面截图]"
	if text != want || chars != 0 {
		t.Fatalf("扫描件文案错: %q chars=%d", text, chars)
	}
}

func TestPDFBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pdf")
	if err := os.WriteFile(path, []byte("this is not a pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractPDFText(context.Background(), path, 1, 1); err == nil {
		t.Fatal("坏文件应返回 error")
	}
}

func TestPDFEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc.pdf")
	if err := os.WriteFile(path, encryptedTestPDF(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractPDFText(context.Background(), path, 1, 1); err == nil || !strings.Contains(err.Error(), "无法读取 PDF") {
		t.Fatalf("加密 PDF 应是工具错误: %v", err)
	}
}

func TestPDFPageRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "badpage.pdf")
	raw := buildTestPDF([]string{
		pdfTextStream("keep-me"),
		"Tj",
		pdfTextStream("still-here"),
	})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractPDFText(context.Background(), path, 1, 3)
	if err != nil {
		t.Fatalf("单页损坏不应让整份失败: %v", err)
	}
	if len(got.Bodies) != 3 || !got.Bodies[1].Failed || got.Scan {
		t.Fatalf("坏页应单独标记: %+v", got)
	}
	text, _ := formatPDFText("badpage.pdf", got)
	if !strings.Contains(text, "keep-me") || !strings.Contains(text, "--- page 2 ---（本页无法抽取）") || !strings.Contains(text, "still-here") {
		t.Fatalf("坏页隔离失败: %q", text)
	}
}

func TestPDFPageTextSpacing(t *testing.T) {
	asciiFar := pdf.Content{Text: []pdf.Text{
		{FontSize: 12, X: 0, Y: 100, W: 30, S: "Hello"},
		{FontSize: 12, X: 80, Y: 100, W: 30, S: "World"},
	}}
	if got := pdfPageText(asciiFar); got != "Hello World" {
		t.Fatalf("ASCII 空隙应补空格: %q", got)
	}
	asciiNear := pdf.Content{Text: []pdf.Text{
		{FontSize: 12, X: 0, Y: 100, W: 30, S: "Hello"},
		{FontSize: 12, X: 32, Y: 100, W: 30, S: "World"},
	}}
	if got := pdfPageText(asciiNear); got != "HelloWorld" {
		t.Fatalf("小空隙不应补空格: %q", got)
	}
	cjk := pdf.Content{Text: []pdf.Text{
		{FontSize: 12, X: 0, Y: 100, W: 12, S: "山"},
		{FontSize: 12, X: 40, Y: 100, W: 12, S: "西"},
	}}
	if got := pdfPageText(cjk); got != "山西" {
		t.Fatalf("中文不应插空格: %q", got)
	}
	rows := pdf.Content{Text: []pdf.Text{
		{FontSize: 12, X: 0, Y: 80, W: 20, S: "bottom"},
		{FontSize: 12, X: 0, Y: 100, W: 10, S: "top"},
	}}
	if got := pdfPageText(rows); got != "top\nbottom" {
		t.Fatalf("Y 更大的行应在前: %q", got)
	}
}

func TestPDFClampAndTruncate(t *testing.T) {
	streams := make([]string, 12)
	for i := range streams {
		streams[i] = pdfTextStream(fmt.Sprintf("p%d", i+1))
	}
	path := filepath.Join(t.TempDir(), "many.pdf")
	if err := os.WriteFile(path, buildTestPDF(streams), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractPDFText(context.Background(), path, 1, 12)
	if err != nil || !got.Clamped || got.From != 1 || got.To != 10 || len(got.Bodies) != 10 {
		t.Fatalf("10 页夹取错: %+v err=%v", got, err)
	}
	text, _ := formatPDFText("many.pdf", got)
	if !strings.Contains(text, "单次最多 10 页，已夹取") || strings.Contains(text, "--- page 11 ---") {
		t.Fatalf("夹取说明错: %q", text)
	}

	long := strings.Repeat("A", pdfTextLimit)
	one := filepath.Join(t.TempDir(), "long.pdf")
	if err := os.WriteFile(one, buildTestPDF([]string{pdfTextStream(long)}), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = extractPDFText(context.Background(), one, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	text, chars := formatPDFText("long.pdf", got)
	if !strings.Contains(text, "（本页文本超过单次上限，已截断；请缩小页范围）") {
		t.Fatalf("缺截断说明: %q", text)
	}
	if chars >= len(long) || chars == 0 {
		t.Fatalf("截断后仍应留下一部分页文本: chars=%d", chars)
	}
	notice := "\n（本页文本超过单次上限，已截断；请缩小页范围）"
	if !strings.HasSuffix(text, notice) {
		t.Fatalf("截断说明应在末尾")
	}
	if len(text)-len(notice) > pdfTextLimit {
		t.Fatalf("截断前正文超过 16KB: %d", len(text)-len(notice))
	}
}

func TestPDFCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := extractPDFText(ctx, filepath.Join(t.TempDir(), "missing.pdf"), 1, 1); err == nil {
		t.Fatal("已取消的 ctx 应直接返回 error")
	}
}

// encryptedTestPDF 是一份空密码打不开的标准加密 PDF。库在 Open 时返回密码错误，
// 抽文本应把它收成「无法读取 PDF」，而不是当成扫描件。
func encryptedTestPDF() []byte {
	o := strings.Repeat("O", 32)
	u := strings.Repeat("U", 32)
	id := strings.Repeat("I", 16)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	xref := b.Len()
	b.WriteString("xref\n0 1\n0000000000 65535 f \n")
	fmt.Fprintf(&b, "trailer\n<< /Size 1 /ID [(%s)] /Encrypt << /Filter /Standard /V 1 /R 2 /Length 40 /O (%s) /U (%s) /P 0 >> >>\n", id, o, u)
	fmt.Fprintf(&b, "startxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// buildIdentityPDF 造一份 Identity-H 的单页 PDF，文本层按 ToUnicode 还原成 text 原文。
// 用来把中文回执句送进抽取器。产品单测不依赖 cupsfilter。
func buildIdentityPDF(text string) []byte {
	runes := []rune(text)
	var hex strings.Builder
	var bf strings.Builder
	const chunk = 100
	for start := 0; start < len(runes); start += chunk {
		end := start + chunk
		if end > len(runes) {
			end = len(runes)
		}
		fmt.Fprintf(&bf, "%d beginbfchar\n", end-start)
		for i := start; i < end; i++ {
			cid := i + 1
			fmt.Fprintf(&hex, "%04X", cid)
			r := runes[i]
			if r > 0xFFFF {
				hi := 0xD800 + ((r - 0x10000) >> 10)
				lo := 0xDC00 + ((r - 0x10000) & 0x3FF)
				fmt.Fprintf(&bf, "<%04X> <%04X%04X>\n", cid, hi, lo)
				continue
			}
			fmt.Fprintf(&bf, "<%04X> <%04X>\n", cid, r)
		}
		bf.WriteString("endbfchar\n")
	}
	cmap := "/CIDInit /ProcSet findresource begin\n" +
		"12 dict begin\n" +
		"begincmap\n" +
		"/CIDSystemInfo << /Registry (A8) /Ordering (UCS) /Supplement 0 >> def\n" +
		"/CMapName /A8 def\n" +
		"1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n" +
		bf.String() +
		"endcmap\n" +
		"CMapName currentdict /CMap defineresource pop\n" +
		"end end\n"
	content := "BT /F1 12 Tf <" + hex.String() + "> Tj ET"

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, 7)
	writeObj := func(n int, body string) {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	writeStream := func(n int, payload string) {
		writeObj(n, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(payload), payload))
	}
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObj(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 6 0 R >> >> >>")
	writeStream(4, content)
	writeStream(5, cmap)
	writeObj(6, "<< /Type /Font /Subtype /Type0 /BaseFont /Receipt /Encoding /Identity-H /ToUnicode 5 0 R /FirstChar 32 /LastChar 126 >>")
	xrefAt := b.Len()
	b.WriteString("xref\n0 7\n0000000000 65535 f \n")
	for n := 1; n <= 6; n++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[n])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 7 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefAt)
	return b.Bytes()
}

func pdfTextStream(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\', '(', ')':
			b.WriteByte('\\')
		}
		b.WriteByte(text[i])
	}
	return "BT\n/F1 12 Tf\n72 720 Td\n(" + b.String() + ") Tj\nET"
}

// buildTestPDF 造一份库能打开的多页 PDF。产品不依赖外部命令，单测也不依赖。
func buildTestPDF(streams []string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	writeObj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets)-1, body)
	}
	kids := make([]string, len(streams))
	for i := range streams {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	writeObj("<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(streams)))
	writeObj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, stream := range streams {
		contentNum := 5 + 2*i
		writeObj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", contentNum))
		payload := stream + "\n"
		writeObj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(payload), payload))
	}
	xrefAt := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n", len(offsets))
	b.WriteString("0000000000 65535 f \n")
	for i := 1; i < len(offsets); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefAt)
	return b.Bytes()
}
