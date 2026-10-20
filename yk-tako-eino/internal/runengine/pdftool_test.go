package runengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func TestReadPDFTextTool(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "a8.pdf")
	raw := buildTestPDF([]string{
		pdfTextStream("Page one: apple"),
		pdfTextStream("Page two: banana"),
		pdfTextStream("Page three: orange"),
	})
	if err := os.WriteFile(pdfPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.pdf"), 0o755); err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, mediaMaxBytes+1)
	copy(huge, []byte("%PDF-1.4\n"))
	if err := os.WriteFile(filepath.Join(root, "big.pdf"), huge, 0o644); err != nil {
		t.Fatal(err)
	}

	rawTool, err := newReadPDFTextTool(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	it := rawTool.(tool.InvokableTool)
	ctx := withNoteBox(context.Background(), newNoteBox("s8", "r8", 1))
	buf := captureLog(t)

	out, err := it.InvokableRun(ctx, `{"path":"a8.pdf"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[pdf path=a8.pdf pages=3；第 1-3 页；文本层抽取]") {
		t.Fatalf("默认页窗页眉错: %q", out)
	}
	if !strings.Contains(out, "--- page 1 ---") || !strings.Contains(out, "--- page 2 ---") || !strings.Contains(out, "Page two: banana") {
		t.Fatalf("缺页标记或正文: %q", out)
	}
	if !strings.Contains(buf.String(), "pdf read s8/r8: pages=1-3 chars=") {
		t.Fatalf("缺 pdf read 日志: %q", buf.String())
	}

	out, err = it.InvokableRun(ctx, `{"path":"a8.pdf","page_from":2,"page_to":2}`)
	if err != nil || !strings.Contains(out, "--- page 2 ---") || strings.Contains(out, "Page one: apple") || !strings.Contains(out, "Page two: banana") {
		t.Fatalf("指定第 2 页错: out=%q err=%v", out, err)
	}

	out, err = it.InvokableRun(ctx, `{"path":"a8.pdf","page_from":0,"page_to":0}`)
	if err != nil || !strings.Contains(out, "第 1-3 页") {
		t.Fatalf("显式 0 应走默认: out=%q err=%v", out, err)
	}

	if _, err = it.InvokableRun(ctx, `{"path":"notes.txt"}`); err == nil || !strings.Contains(err.Error(), "不是 PDF") {
		t.Fatalf("非 PDF 应拒绝: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"missing.pdf"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失应报不存在: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"../secret.pdf"}`); err == nil {
		t.Fatal("越界路径应拒绝")
	}
	if _, err = it.InvokableRun(ctx, fmt.Sprintf(`{"path":"%s"}`, pdfPath)); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("绝对路径应拒绝: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"dir.pdf"}`); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("目录应拒绝: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"big.pdf"}`); err == nil || !strings.Contains(err.Error(), "5MB") {
		t.Fatalf("超 5MB 应拒绝: %v", err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"sessions/s1/.media/a.pdf"}`); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("旧 sessions 路径应拒绝: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "enc.pdf"), encryptedTestPDF(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = it.InvokableRun(ctx, `{"path":"enc.pdf"}`); err == nil || !strings.Contains(err.Error(), "无法读取 PDF") {
		t.Fatalf("加密 PDF 应是工具错误: %v", err)
	}
	out, err = it.InvokableRun(ctx, `{"path":"a8.pdf","page_from":99,"page_to":99}`)
	if err != nil || out != "[pdf path=a8.pdf pages=3；page_from 超出范围]" {
		t.Fatalf("页码超出应是正常结果: out=%q err=%v", out, err)
	}
}
