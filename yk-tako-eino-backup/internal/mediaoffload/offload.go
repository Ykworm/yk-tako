// 本文件：改写消息里内联媒体的那一层。规则见包注释（store.go 文件头）。
package mediaoffload

import (
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Config 是一场会话的落盘配置。
type Config struct {
	// Store 是缓存目录。为空时不装这一层——没地方写就别改消息。
	Store *Store
	// MinBytes 小于这个字节数的媒体原样留着。为空用 DefaultMinBytes。
	MinBytes int
	// Logf 记落盘失败。为空用 log.Printf。
	Logf func(format string, args ...any)
}

// Session 是一场会话的落盘状态：记着本轮哪些媒体已经给模型看过。
// 一场会话一份；几场共用一个 Session 会把「本轮看过了」串台，
// 于是别的会话第一次看到的图直接变成占位符。
type Session struct {
	mu       sync.Mutex
	store    *Store
	minBytes int
	logf     func(format string, args ...any)
	turn     int
	seen     map[string]struct{}
}

// NewSession 建一场会话的落盘状态。cfg.Store 为空返回 nil，调用方据此决定装不装 Handler。
func NewSession(cfg Config) *Session {
	if cfg.Store == nil {
		return nil
	}
	minBytes := cfg.MinBytes
	if minBytes <= 0 {
		minBytes = DefaultMinBytes
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Session{store: cfg.Store, minBytes: minBytes, logf: logf, seen: map[string]struct{}{}}
}

// Rewrite 把这批消息里该落盘的媒体换成占位符，返回新的消息切片。
// 没有要改的返回原切片、changed=false。改的是新消息对象，调用方那批不会被就地改。
// 落盘失败只记日志、原片段留着：一次写盘失败不该让这一轮跑不下去。
func (s *Session) Rewrite(msgs []*schema.Message) (out []*schema.Message, changed bool) {
	if s == nil || len(msgs) == 0 {
		return msgs, false
	}
	start, turn := scanTurn(msgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn != s.turn {
		// 新的一轮：本轮看过的媒体重新算，这样模型再 read_file 同一张图还能看到原图。
		s.turn = turn
		s.seen = map[string]struct{}{}
	}
	out = msgs
	for i, msg := range msgs {
		if msg == nil || len(msg.UserInputMultiContent) == 0 {
			continue
		}
		parts, partChanged := s.parts(msg.UserInputMultiContent, i < start)
		if !partChanged {
			continue
		}
		if !changed {
			out = append([]*schema.Message(nil), msgs...)
			changed = true
		}
		clone := *msg
		clone.UserInputMultiContent = parts
		out[i] = &clone
	}
	return out, changed
}

// parts 逐个片段过一遍。有改动才复制切片，没改动返回原切片。
func (s *Session) parts(parts []schema.MessageInputPart, earlier bool) (out []schema.MessageInputPart, changed bool) {
	out = parts
	for i, part := range parts {
		replacement, ok := s.part(part, earlier)
		if !ok {
			continue
		}
		if !changed {
			out = append([]schema.MessageInputPart(nil), parts...)
			changed = true
		}
		out[i] = replacement
	}
	return out, changed
}

// part 决定一个片段怎么办。ok=true 表示要用返回的片段换掉原来那个。
func (s *Session) part(part schema.MessageInputPart, earlier bool) (schema.MessageInputPart, bool) {
	media, ok := inlineMedia(part)
	if !ok {
		return part, false
	}
	raw, err := decodeBase64(media.b64)
	if err != nil {
		s.logf("mediaoffload: base64 解不开，%s 片段原样留着: %v", media.kind, err)
		return part, false
	}
	if len(raw) < s.minBytes {
		return part, false
	}
	sha := SHA256(raw)
	path, err := s.store.Put(sha, extForMIME(media.mime), raw)
	if err != nil {
		s.logf("mediaoffload: %d 字节的 %s 落盘失败，原样留着: %v", len(raw), media.kind, err)
		return part, false
	}
	if !earlier {
		if _, seen := s.seen[sha]; !seen {
			// 本轮第一次见到：字节留着让模型真的看到这一眼，下一轮起就只剩占位符。
			s.seen[sha] = struct{}{}
			return part, false
		}
	}
	return placeholder(media.kind, sha, path, len(raw), media.mime), true
}

// scanTurn 找出本轮的起点和轮序号。起点是最后一条用户消息的下标，序号是用户消息的条数；
// 序号变了就是换了一轮。没有用户消息时整批都算本轮。
func scanTurn(msgs []*schema.Message) (start, turn int) {
	for i, msg := range msgs {
		if msg == nil || msg.Role != schema.User {
			continue
		}
		turn++
		start = i
	}
	return start, turn
}

// media 是一段内联媒体的三样东西。
type media struct {
	kind string
	b64  string
	mime string
}

// inlineMedia 取出片段里内联的字节。远端 URL（http/https）不算——那种本来就不占上下文。
// 四种多模态片段共用 schema.MessagePartCommon（eino v0.9.21 schema/message.go:160），
// 所以这里只认哪一个字段非空，不看 part.Type。
func inlineMedia(part schema.MessageInputPart) (media, bool) {
	common, kind := commonOf(part)
	if common == nil {
		return media{}, false
	}
	if common.Base64Data != nil && *common.Base64Data != "" {
		return media{kind: kind, b64: *common.Base64Data, mime: mimeOrFallback(common.MIMEType)}, true
	}
	if common.URL != nil {
		b64, mime, ok := parseDataURI(*common.URL)
		if !ok {
			return media{}, false
		}
		if strings.TrimSpace(common.MIMEType) != "" {
			mime = common.MIMEType
		}
		return media{kind: kind, b64: b64, mime: mimeOrFallback(mime)}, true
	}
	return media{}, false
}

// commonOf 认出片段里的多模态载荷，并给出占位符上要写的类别名。
func commonOf(part schema.MessageInputPart) (*schema.MessagePartCommon, string) {
	switch {
	case part.Image != nil:
		return &part.Image.MessagePartCommon, "image"
	case part.Audio != nil:
		return &part.Audio.MessagePartCommon, "audio"
	case part.Video != nil:
		return &part.Video.MessagePartCommon, "video"
	case part.File != nil:
		return &part.File.MessagePartCommon, "file"
	default:
		return nil, ""
	}
}

// parseDataURI 拆 RFC 2397 的 data: URI，只认 ;base64, 那一种。
// 不带 ;base64, 的 data: URI 太少见，猜还不如不管。
func parseDataURI(uri string) (b64, mime string, ok bool) {
	rest, found := strings.CutPrefix(uri, "data:")
	if !found {
		return "", "", false
	}
	head, payload, found := strings.Cut(rest, ";base64,")
	if !found || payload == "" {
		return "", "", false
	}
	// head 是 "image/png" 或 "image/png;charset=utf-8"。
	mime = head
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return payload, strings.TrimSpace(mime), true
}

// mimeOrFallback 给没有 MIME 的载荷一个兜底值，落盘扩展名会是 .bin。
func mimeOrFallback(mime string) string {
	if strings.TrimSpace(mime) == "" {
		return "application/octet-stream"
	}
	return mime
}

// decodeBase64 解 base64。先按标准的解，失败再按没有补位的解——data URI 两种都见得到。
func decodeBase64(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// placeholder 是换上去的那个文本片段。带路径是为了模型能自己把字节取回来，
// 带 sha 是为了人翻记录时能和缓存目录里的文件对上。
func placeholder(kind, sha, path string, size int, mime string) schema.MessageInputPart {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeText,
		Text: fmt.Sprintf("[%s 已落盘: sha=%s path=%s size=%dB mime=%s；要重新看就用 read_file 读这个路径]",
			kind, short, path, size, mime),
	}
}
