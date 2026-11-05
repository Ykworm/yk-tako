// 本文件是 bash 输出的「留尾 + 全文落盘」（doc/58 §4）。
//
// 病灶：今天 bash 的 capWriter **留头丢尾**——装依赖、跑测试的报错都在尾部，
// 报错那几 KB 当场没了，模型只能原样重跑；重跑的结果可能已经变了。
//
// 改后照 dsh 的 subprocess-local OutputCollector
// （packages/subprocess/subprocess-local/src/output.ts）做：
//   - 内存里滚动保留**尾部** ≤64,000 字节/流（字节精确的尾窗，报错与最终结果都在里面）；
//   - 首次溢出时建文件，把**已有字节先补写进去**、之后边到边写 → 文件里是该流的**全文**；
//   - 装不下全文（>64MB/流）就把文件删掉、**不报路径**（dsh 的 discardSpill 纪律：
//     要么是全文，要么不存在——留半截文件比不留更坏）；
//   - 写失败 fail-open：弃掉文件、继续留尾，只记一行日志（落盘是补救手段，不是执行的前提）。
package runengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// bash 的两档上限（doc/58 §7 拍板 3，照 dsh bash-local 的默认值）：
// 内存尾窗 64,000 字节/流（dsh 的 maxOutputBytes），落盘全文 64MB/流（dsh 的 maxSpillBytes）。
const (
	bashOutputCap = 64_000
	bashSpillCap  = 64 * 1024 * 1024
)

// bashSpillTTL 是 bash 落盘目录的保留期（dsh 的 cleanupPeriodDays 默认 30 天）。
const bashSpillTTL = 30 * 24 * time.Hour

// bashSpill 是本次 bash 调用的落盘上下文：目录 + 序号（两流共用同一序号，靠扩展名区分）。
type bashSpill struct {
	dirRel string // 相对作业根的目录，如 .tako/bash/r_01ABC
	dirAbs string // 绝对目录
	seq    int    // 本 run 内第几次 bash 调用（从 1 起）
}

// newBashSpill 算本次调用的落盘位置。
//
// 目录段用 run id（noteBox 里那份，与搜索/抓取的落盘同一个 run 口径）：
// 主代理与子代理共用同一份 noteBox → 同序列顺编、天然不撞。
// 没有 noteBox 时（旁路调用）退回进程内的一段：段名带 pid、序号取进程内计数器——
// 同一段名撞名会让 O_EXCL 失败、落盘被弃掉（只剩尾窗），那样的路径不该由调用方的
// 疏忽决定。
func newBashSpill(ctx context.Context, workspace string) *bashSpill {
	runID, seq := "", 0
	if box := noteBoxFrom(ctx); box != nil {
		if box.run != "" {
			runID = string(box.run)
		}
		seq = box.tickBash()
	}
	if runID == "" {
		runID = fmt.Sprintf("r_local%d", os.Getpid())
		seq = int(atomic.AddInt64(&bashLocalSeq, 1))
	}
	runID = sanitizePathSegment(runID)
	return &bashSpill{
		dirRel: ".tako/bash/" + runID,
		dirAbs: filepath.Join(workspace, ".tako", "bash", runID),
		seq:    seq,
	}
}

// bashLocalSeq 是没有 noteBox 时的进程内序号（旁路调用专用）。
var bashLocalSeq int64

// sanitizePathSegment 把一段文本压成能当目录名的形态（挡掉分隔符与 ..）。
func sanitizePathSegment(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, s)
	if s == "" || s == "." || s == ".." {
		return "r_local"
	}
	return s
}

// tailCollector 收一个流的字节：内存留尾部 ≤cap，全文落盘（≤bashSpillCap）。
type tailCollector struct {
	cap      int
	spillCap int   // 落盘上限（0 = 用默认 bashSpillCap）
	tail     []byte
	total    int64
	dropped  bool // 丢过字节（= 超过内存上限）

	spill    *bashSpill
	ext      string // 文件扩展名：out / err
	file     *os.File
	relPath  string // 相对作业根的路径（落盘成功后才有）
	spillOff bool   // 落盘关掉了（写失败或装不下全文）
}

// Write 收一段字节：先算总长，再按需落盘，最后推进尾窗。
//
// 落盘的时机照 dsh：**首次溢出**（尾窗装不下下一块）才建文件；建文件时把内存里已有的
// 字节补写进去——此刻内存里装的是整条流的前缀（此前没丢过字节），补上就是全文。
func (c *tailCollector) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if c.spill != nil && !c.spillOff && (len(c.tail)+len(p) > c.cap || c.file != nil) {
		c.spillAll(p)
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.cap {
		cut := len(c.tail) - c.cap
		copy(c.tail, c.tail[cut:]) // copy 是 memmove 语义，重叠安全
		c.tail = c.tail[:c.cap]
		c.dropped = true
	}
	return len(p), nil
}

// spillAll 把这一块写进落盘文件；第一次写要先补上已有的字节。
func (c *tailCollector) spillAll(chunk []byte) {
	limit := c.spillCap
	if limit <= 0 {
		limit = bashSpillCap
	}
	if c.total > int64(limit) {
		// 装不下全文 → 删文件、不报路径（dsh 的 discardSpill 纪律）。
		c.discardSpill()
		return
	}
	if c.file == nil {
		f, rel, err := c.openSpill()
		if err != nil {
			log.Printf("bash-spill: 落盘失败，只保留尾部: %v", err)
			c.spillOff = true
			return
		}
		c.file, c.relPath = f, rel
		if _, err := f.Write(c.tail); err != nil {
			log.Printf("bash-spill: 写已有字节失败，只保留尾部: %v", err)
			c.discardSpill()
			return
		}
	}
	if _, err := c.file.Write(chunk); err != nil {
		log.Printf("bash-spill: 写失败，只保留尾部: %v", err)
		c.discardSpill()
	}
}

// openSpill 建落盘文件（一次性、不覆盖）。
func (c *tailCollector) openSpill() (*os.File, string, error) {
	if err := os.MkdirAll(c.spill.dirAbs, 0o755); err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%d.%s", c.spill.seq, c.ext)
	full := filepath.Join(c.spill.dirAbs, name)
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, c.spill.dirRel + "/" + name, nil
}

// discardSpill 弃掉落盘文件（关掉落盘）：半截文件比没有更坏。
func (c *tailCollector) discardSpill() {
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
	if c.relPath != "" {
		os.Remove(filepath.Join(c.spill.dirAbs, filepath.Base(c.relPath)))
		c.relPath = ""
	}
	c.spillOff = true
}

// closeSpill 收尾：关文件（内容已经是全文）。
func (c *tailCollector) closeSpill() {
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
}

// text 是给模型看的那一份（尾部窗口）。
func (c *tailCollector) text() string { return string(c.tail) }

// note 是这一流给模型的回执行行（doc/58 §4.1 的字节）：
// 落了盘 → 给路径（模型自己 read_file 分页读）；没落成 → 说清只剩尾部。
func (c *tailCollector) note(name string) string {
	if c.relPath != "" {
		return fmt.Sprintf("\n（%s 完整输出已存到 %s，共 %d 字节；用 read_file 分页读它）", name, c.relPath, c.total)
	}
	if c.dropped {
		return fmt.Sprintf("\n（%s 超 %d 字节，已只保留尾部——完整输出未能落盘）", name, c.cap)
	}
	return ""
}

// sweepBashSpills 删掉 <作业根>/.tako/bash/ 下 mtime 超过 30 天的 <runId>/ 目录
// （doc/58 §4.4：dsh 的 cleanupPeriodDays 默认 30）。
//
// 触发点是**每次 bash 调用前**（dsh 是 CLI 进程、启动时扫一次；tako 是长驻引擎，
// 启动那一刻还不存在的项目永远扫不到）。失败只记日志，不影响这条命令执行。
func sweepBashSpills(workspace string, now time.Time) {
	base := filepath.Join(workspace, ".tako", "bash")
	entries, err := os.ReadDir(base)
	if err != nil {
		return // 目录不存在是常态（还没落过盘）
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name())
		info, err := e.Info()
		if err != nil {
			continue // 扫的过程中被别的副本删了——正是我们要的结果
		}
		if now.Sub(info.ModTime()) <= bashSpillTTL {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			log.Printf("bash-sweep: %v", err)
		}
	}
}
