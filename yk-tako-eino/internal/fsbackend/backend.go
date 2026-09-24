// 给 DeepAgent 用的本地文件后端。
// 官方 eino-ext/adk/backend/local 会带上 PDF 解析依赖，这门课只需要读写和搜索。
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

type Backend struct {
	Root string
}

func New(root string) (*Backend, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Backend{Root: abs}, nil
}

func (b *Backend) resolve(path string) (string, error) {
	if path == "" {
		return b.Root, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.Root, path)
	}
	return filepath.Clean(path), nil
}

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

func fileInfo(path string, info fs.FileInfo) filesystem.FileInfo {
	return filesystem.FileInfo{
		Path:       path,
		IsDir:      info.IsDir(),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
	}
}

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
