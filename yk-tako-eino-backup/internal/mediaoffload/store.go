// Package mediaoffload 把历史里的内联媒体写到磁盘，换成一行占位符。
//
// 为什么要这一层：多模态片段里的字节是 base64 内联在消息上的，一旦进了历史，
// 之后每一轮都要把它重新发给模型——一张 1.5 MiB 的 PNG 约合几十万 token，
// 而且前缀缓存从那条消息起就废了。压缩（internal/compact）按张数估算图片
// （estimate.go:21），能把这种历史整轮摘要掉，但在一轮里反复调模型时救不了。
//
// 规则（对齐 harness 的 middleware/media_offload.py）：
//   - 上一轮的媒体：一律换成占位符；
//   - 本轮第一次见到：字节留在消息里让模型真的看一眼，同时落盘；
//   - 本轮已经看过（含同一次请求里重复出现的同一张图）：换成占位符。
//     模型要再看，用 read_file 读占位符里那条路径，读回来的是一张新图，
//     于是又回到「本轮第一次」。
//
// 落盘不改会话内存里的正本：Rewrite 返回新的消息对象，正本仍留着原始字节，
// 事件库回放的画面不受影响。
package mediaoffload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultDir 是缓存目录名，放在工作区根下。以 . 开头，ls 默认不吵人。
	DefaultDir = ".media-cache"
	// DefaultMinBytes 是落盘门槛。比这小不值得换占位符：占位符自己几十字节，
	// 模型要再看还得花一次 read_file 往返，比直接重发字节贵。
	DefaultMinBytes = 4 * 1024
	// DefaultMaxBytes 是单个媒体落盘的上限。再大就不是「省上下文」而是塞爆磁盘了。
	DefaultMaxBytes = 32 << 20
)

// Store 是内容寻址的媒体缓存目录：文件名就是字节的 SHA-256，同一份媒体只落一次。
type Store struct {
	dir      string
	maxBytes int
}

// NewStore 建一个缓存目录句柄。dir 为空时用当前目录下的 DefaultDir。
// maxBytes 小于等于 0 时用 DefaultMaxBytes。
func NewStore(dir string, maxBytes int) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(".", DefaultDir)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}
	return &Store{dir: dir, maxBytes: maxBytes}
}

// Dir 是缓存目录的绝对路径。
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Put 把 raw 写成 <dir>/<sha><ext> 并返回绝对路径。
// 内容寻址，所以已存在就直接返回，不再写一遍。超过上限时拒绝，调用方会把原片段留着。
func (s *Store) Put(sha, ext string, raw []byte) (string, error) {
	if s == nil {
		return "", fmt.Errorf("mediaoffload: 没有缓存目录")
	}
	if len(raw) > s.maxBytes {
		return "", fmt.Errorf("mediaoffload: %d 字节超过上限 %d", len(raw), s.maxBytes)
	}
	path := filepath.Join(s.dir, sha+ext)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// SHA256 算字节的十六进制摘要，既当文件名也当占位符里的身份证。
func SHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// extForMIME 把 MIME 换成扩展名。常见的几个手写，免得依赖平台的 MIME 表；
// 认不出的一律 .bin——名字只是给人看的，取回靠的是路径。
func extForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}
