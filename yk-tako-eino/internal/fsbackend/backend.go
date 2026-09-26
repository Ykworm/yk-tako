// Package fsbackend 是给 DeepAgent 用的本地文件后端。
// 官方 eino-ext/adk/backend/local 会带上 PDF 解析依赖，这门课只需要读写和搜索。
//
// Backend 实现 github.com/cloudwego/eino/adk/filesystem 包 backend.go:243 定义的
// Backend 接口（eino v0.9.21）；deep.Config.Backend 要的就是这个接口。
package fsbackend

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
)

// Backend 是直接读写本机文件的本地后端：相对路径相对 Root 解析，
// 绝对路径原样使用，不把访问限制在 Root 里。越界和敏感文件防护是
// guard 层（internal/guard）的事，这个后端只管读写。
//
// 实现接口：filesystem.Backend —— github.com/cloudwego/eino/adk/filesystem
// 包的 backend.go:243（eino v0.9.21），覆盖 LsInfo / Read / Write / Edit /
// GrepRaw / GlobInfo 六个方法。
type Backend struct {
	Root string
}

// 编译期校验 Backend 实现了 filesystem.Backend。
var _ filesystem.Backend = (*Backend)(nil)

// New 把 root 转成绝对路径建后端。
func New(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Backend{Root: abs}, nil
}

// resolve 把请求路径落成绝对路径：空路径给 Root，相对路径拼在 Root 下，
// 绝对路径原样使用。Clean 之后 .. 可能指到 Root 外，这里不拦。
func (b *Backend) resolve(path string) (string, error) {
	if path == "" {
		return b.Root, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.Root, path)
	}
	return filepath.Clean(path), nil
}

// LsInfo 实现 filesystem.Backend 的 LsInfo 方法：DeepAgent 的 ls 工具执行时回调到这里
// （adk/middlewares/filesystem 生成的内置文件工具，下同）；列目录，逐项带上大小和修改时间。
func (b *Backend) LsInfo(_ context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	dir, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfo(filepath.Join(dir, entry.Name()), info))
	}
	return out, nil
}

// Read 实现 filesystem.Backend 的 Read 方法：DeepAgent 的 read_file 工具执行时回调到这里；
// 读文件，支持按行 offset/limit 取片段，超 8000 字截断，免得一次灌爆上下文。
func (b *Backend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if req.Offset > 1 || req.Limit > 0 {
		lines := strings.Split(text, "\n")
		start := req.Offset - 1
		if start < 0 {
			start = 0
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if req.Limit > 0 && start+req.Limit < end {
			end = start + req.Limit
		}
		text = strings.Join(lines[start:end], "\n")
	}
	if len(text) > 8000 {
		text = text[:8000] + "\n...(截断)"
	}
	return &filesystem.FileContent{Content: text}, nil
}

// Write 实现 filesystem.Backend 的 Write 方法：DeepAgent 的 write_file 工具执行时回调到这里；
// 覆盖写文件，父目录不存在就先建。
func (b *Backend) Write(_ context.Context, req *filesystem.WriteRequest) error {
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(req.Content), 0o644)
}

// Edit 实现 filesystem.Backend 的 Edit 方法：DeepAgent 的 edit_file 工具执行时回调到这里；
// 按 old_string 替换；默认要求唯一匹配，replace_all 才全量替换。
func (b *Backend) Edit(_ context.Context, req *filesystem.EditRequest) error {
	if req.OldString == "" {
		return fmt.Errorf("old_string 不能为空")
	}
	if req.OldString == req.NewString {
		return fmt.Errorf("old_string 和 new_string 相同")
	}
	path, err := b.resolve(req.FilePath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	count := strings.Count(text, req.OldString)
	if count == 0 {
		return fmt.Errorf("没有找到要替换的文本")
	}
	if !req.ReplaceAll && count != 1 {
		return fmt.Errorf("匹配了 %d 处，需要唯一匹配或 replace_all", count)
	}
	var next string
	if req.ReplaceAll {
		next = strings.ReplaceAll(text, req.OldString, req.NewString)
	} else {
		next = strings.Replace(text, req.OldString, req.NewString, 1)
	}
	return os.WriteFile(path, []byte(next), 0o644)
}

// GrepRaw 实现 filesystem.Backend 的 GrepRaw 方法：DeepAgent 的 grep 工具执行时回调到这里；
// 在目录里逐行搜正则，最多收 40 条就停。
func (b *Backend) GrepRaw(_ context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	pattern := req.Pattern
	if req.CaseInsensitive && !strings.HasPrefix(pattern, "(?i)") {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	root, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	var matches []filesystem.GrepMatch
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if len(matches) >= 40 {
			return fs.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, filesystem.GrepMatch{Path: path, Line: i + 1, Content: line})
				if len(matches) >= 40 {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return matches, err
}

// GlobInfo 实现 filesystem.Backend 的 GlobInfo 方法：DeepAgent 的 glob 工具执行时回调到这里；
// 按 glob 找文件或目录，最多收 80 条就停。
func (b *Backend) GlobInfo(_ context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	root, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	re := globToRegexp(req.Pattern)
	var out []filesystem.FileInfo
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if rel == "." || !re.MatchString(rel) && !re.MatchString(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, fileInfo(path, info))
		if len(out) >= 80 {
			return fs.SkipAll
		}
		return nil
	})
	return out, err
}

// fileInfo 把标准库的 FileInfo 转成后端接口要的形状。
func fileInfo(path string, info fs.FileInfo) filesystem.FileInfo {
	return filesystem.FileInfo{
		Path:       path,
		IsDir:      info.IsDir(),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
	}
}

// globToRegexp 把 glob 编译成正则：**/ 可跨目录，* 不跨 /。
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(glob[i])
		default:
			b.WriteByte(glob[i])
		}
	}
	re := strings.ReplaceAll(b.String(), `**/`, `(?:.*/)?`)
	re = strings.ReplaceAll(re, `**`, `.*`)
	re = strings.ReplaceAll(re, `*`, `[^/]*`)
	return regexp.MustCompile("^" + re + "$")
}
