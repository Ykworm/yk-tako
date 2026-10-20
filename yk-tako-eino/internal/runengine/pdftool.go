package runengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// readPDFTextInput 是 read_pdf_text 的入参。
// page_from / page_to 用指针，是为了区分「没写」和「写了 0」：两者都走默认页窗。
type readPDFTextInput struct {
	Path     string `json:"path" jsonschema:"description=占位行里的 path 原文，必须是 .pdf。不要改写。"`
	PageFrom *int   `json:"page_from,omitempty" jsonschema:"description=起始页，从 1 计。不写，或小于等于 0，则从第 1 页起。"`
	PageTo   *int   `json:"page_to,omitempty" jsonschema:"description=结束页，含本页。不写，或小于等于 0，则从起始页起共 3 页。单次最多 10 页。"`
}

// newReadPDFTextTool 造 read_pdf_text：按页抽 PDF 文本层，正文作为工具结果返回。
// 路径闸与 read_media_file 同一把 resolveReadMediaPath。扫描件返回正常结果；
// 打不开、加密、损坏返回 Go error，由 wrapToolErrors 收成「错误：…」，run 继续。
func newReadPDFTextTool(store *Store, root string) (tool.BaseTool, error) {
	return utils.InferTool("read_pdf_text",
		"按页读取 PDF 的文本层。path 用占位行里的 path 原文，扩展名必须是 .pdf。"+
			"page_from 与 page_to 可省略，默认第 1-3 页；单次最多 10 页、16KB。"+
			"没有文本层的扫描件会说明读不到。不要用它读图片或音视频。",
		func(ctx context.Context, in readPDFTextInput) (string, error) {
			full, err := resolveReadMediaPath(ctx, store, root, in.Path)
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(full)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("文件不存在：%s", in.Path)
				}
				return "", fmt.Errorf("无法访问 %s：%v", in.Path, err)
			}
			if !fi.Mode().IsRegular() {
				if fi.IsDir() {
					return "", fmt.Errorf("%s 是目录不是文件", in.Path)
				}
				return "", fmt.Errorf("%s 不是普通文件", in.Path)
			}
			if strings.ToLower(filepath.Ext(in.Path)) != ".pdf" {
				return "", fmt.Errorf("%s 不是 PDF 文件", in.Path)
			}
			if fi.Size() > mediaMaxBytes {
				return "", fmt.Errorf("文件 %s 有 %d 字节、超过 5MB 上限", in.Path, fi.Size())
			}
			from, to := 0, 0
			if in.PageFrom != nil {
				from = *in.PageFrom
			}
			if in.PageTo != nil {
				to = *in.PageTo
			}
			got, err := extractPDFText(ctx, full, from, to)
			if err != nil {
				return "", err
			}
			text, chars := formatPDFText(in.Path, got)
			logPDFRead(ctx, got.From, got.To, chars)
			return text, nil
		})
}

// logPDFRead 在正常结果上记一行（含扫描件和页码超出）。Go error 不记这行。
// 没有 noteBox 的旁路调用用问号，避免空指针。
func logPDFRead(ctx context.Context, from, to, chars int) {
	session, run := "?", "?"
	if box := noteBoxFrom(ctx); box != nil {
		session = string(box.session)
		run = string(box.run)
	}
	log.Printf("pdf read %s/%s: pages=%d-%d chars=%d", session, run, from, to, chars)
}
