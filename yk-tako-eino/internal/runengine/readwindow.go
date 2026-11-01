// 本文件是 read 工具的分页窗口（doc/58 §3）：一次调用最多给模型 2000 行 / 50KB，
// 末尾附一行页脚（总行数 + 下一个 offset）。形态照 dsh 的 tool-fs read
// （packages/fs/tool-fs/src/read-render.ts）：offset 1-based、limit 行数、
// 页脚三态、被字节帽挡下的那一行丢弃且后续行不再收。
//
// 读法是**流式**的（dsh 的 buildWindow 同款）：一趟扫过去同时干三件事——
// 算整文件 sha256（写前判据要整份口径）、点总行数（页脚要给）、只收请求的那一页。
// 内存恒定：整份文件不进内存，单行也只留前 readMaxLineChars 个字符。
package runengine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// read 的三个帽（dsh 的 READ_LIMIT / READ_MAX_BYTES / READ_MAX_LINE_LENGTH）：
// 一次调用最多返回 2000 行、最多 50KB；单行超过 2000 字符的部分截掉并标注。
const (
	readWindowMaxLines = 2000
	readWindowMaxBytes = 50 * 1024
	readMaxLineChars   = 2000
)

// readLineTruncMark 是超长单行的行尾标注。
const readLineTruncMark = "…（本行超长已截断）"

// readLineByteCap 是单行在内存里最多留多少字节。
//
// 为什么不是 2000：2000 是**字符**数，一个字符最多 4 字节（UTF-8 上限）→ 2000 字符
// 最多 8000 字节。多留 8 字节是为了「这一行到底超没超 2000 字符」判得准：
// 留满 cap 且后面还有字节 → 这一行的字符数至少 2001（8005/4 向上取整）→ 一定超。
const readLineByteCap = readMaxLineChars*4 + 8

// scanTextFile 流式扫一遍文本文件：顺带算整文件 sha256、点总行数、按行交付内容。
//
// onLine 收到的是 (行号 1-based, 行文本)。行号从 1 数起、连续——**总行数要扫完才知道**
// （页脚的「共 N 行」必须是全文件的行数，不是本页的），所以 onLine 会被调满文件那么多回。
//
// 单行只交付前 readMaxLineChars 个字符，超长的行尾标 readLineTruncMark——一行可以几 MB
// （minified JS），整行进内存就能把进程吃穿。
//
// 指纹：size ≤ 64MB 时流式算 sha256（大文件降级成 (mtime,size) 弱判据，与 filefp.go 同口径）。
func scanTextFile(path string, onLine func(no int, text string)) (Fingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return Fingerprint{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Fingerprint{}, err
	}
	fp := Fingerprint{Mtime: fi.ModTime(), Size: fi.Size()}
	var src io.Reader = f
	var h hash.Hash
	if fi.Size() <= fingerprintHashMaxBytes {
		h = sha256.New()
		src = io.TeeReader(f, h)
	}

	sc := &lineScanner{}
	buf := make([]byte, 64*1024)
	no := 0
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for {
				idx := bytes.IndexByte(chunk, '\n')
				if idx < 0 {
					sc.append(chunk)
					break
				}
				sc.append(chunk[:idx])
				no++
				onLine(no, sc.finish())
				sc.reset()
				chunk = chunk[idx+1:]
			}
		}
		if rerr == io.EOF {
			// 文件末尾没有换行的最后一行（今天 split 的 TrimSuffix 口径：末尾换行不多算一行）。
			if sc.has() {
				no++
				onLine(no, sc.finish())
			}
			break
		}
		if rerr != nil {
			return fp, rerr
		}
	}
	if h != nil {
		fp.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return fp, nil
}

// lineScanner 攒当前行的字节：最多留 readLineByteCap 个，后面照读照丢。
type lineScanner struct {
	buf  []byte
	over bool // 这一行比 readLineByteCap 还长（后面的字节已经丢了）
}

func (s *lineScanner) append(chunk []byte) {
	if s.over {
		return
	}
	room := readLineByteCap - len(s.buf)
	if room <= 0 {
		s.over = true
		return
	}
	if len(chunk) > room {
		s.buf = append(s.buf, chunk[:room]...)
		s.over = true
		return
	}
	s.buf = append(s.buf, chunk...)
}

func (s *lineScanner) reset() {
	s.buf = s.buf[:0]
	s.over = false
}

func (s *lineScanner) has() bool { return len(s.buf) > 0 || s.over }

// finish 交出这一行的文本：超长的截到 readMaxLineChars 个字符并标注。
func (s *lineScanner) finish() string {
	b := s.buf
	if s.over {
		b = trimToRuneBoundary(b)
	}
	runes := []rune(string(b))
	if len(runes) > readMaxLineChars {
		return string(runes[:readMaxLineChars]) + readLineTruncMark
	}
	return string(b)
}

// trimToRuneBoundary 砍掉末尾不完整的 UTF-8 序列（cap 可能落在半个字符上）。
// 砍掉的最多 3 字节 → 剩下的仍够装 2001 个字符（8005/4 向上取整），截断判定不受影响。
func trimToRuneBoundary(b []byte) []byte {
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return b
}

// readFooter 派生页脚三态（字节定死，doc/58 §3.2）：
// 字节帽先到 → 说明只能靠 offset 跳着读；否则还有更多 → 给下一个 offset；
// 读到末尾 → 只报总行数（head/tail 删掉后，总行数是模型唯一的定位坐标）。
func readFooter(offset, total int, page []string, capped bool) string {
	last := offset + len(page) - 1
	switch {
	case capped:
		return fmt.Sprintf("（已显示第 %d-%d 行，共 %d 行；本次被 50KB 上限截断，继续读用 offset=%d）",
			offset, last, total, last+1)
	case last < total:
		return fmt.Sprintf("（已显示第 %d-%d 行，共 %d 行；继续读用 offset=%d）",
			offset, last, total, last+1)
	default:
		return fmt.Sprintf("（已读到文件末尾，共 %d 行）", total)
	}
}

// readBody 拼一页的回执：正文 + 空行 + 页脚。空页只有页脚（dsh 的 formatReadOutput 同款）。
func readBody(page []string, footer string) string {
	if len(page) == 0 {
		return footer
	}
	return strings.Join(page, "\n") + "\n\n" + footer
}
