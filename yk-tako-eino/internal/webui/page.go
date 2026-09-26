// 本文件：内嵌单页——//go:embed 把 static/index.html 焊进二进制（机制见《09》§10），
// 零构建链、零路径依赖（《10》§3.2）。
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

// handleIndex 出单页。注意 `//` 与 `go:embed` 之间不能有空格，否则静默退化（《09》§10.1）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
