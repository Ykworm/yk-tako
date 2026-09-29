// 本文件：每个模型一个窗口大小。先看进程内存，再看配置文件，最后才向厂商查。
// 查不到用 256000，并且不写文件，避免一次失败把这个模型钉死。
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FallbackWindow 是查不到窗口大小时使用的 token 数。不写入配置文件。
const FallbackWindow = 256000

// Lookup 向当前模型查窗口大小。key 是 厂商 ID/模型 ID。返回值必须大于 0 才会被保存。
type Lookup func(ctx context.Context, key string) (int, error)

// WindowStore 记住已经查到的窗口大小。文件路径为空时只留在内存里，测试用这个。
// miss 只留在这一进程：查失败不写文件，但也不在每次发消息时再等一次网络。下次启动会再查。
type WindowStore struct {
	mu   sync.Mutex
	mem  map[string]int
	miss map[string]bool
	path string
}

// NewWindowStore 打开一份窗口大小记录。path 为空表示不落盘。
func NewWindowStore(path string) *WindowStore {
	return &WindowStore{mem: map[string]int{}, path: path}
}

// DefaultPath 是窗口大小文件，放在进程的当前目录，文件名是 model-windows.json。
// 在 yk-tako-eino 里启动时，它和编译产物 yktako、事件库 yktako-webui.db 在同一个目录。
// 当前目录读不到时返回空，调用方就只使用内存和 256000。
func DefaultPath() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return ""
	}
	return filepath.Join(cwd, "model-windows.json")
}

// Get 返回 key 对应的窗口大小。lookup 在锁外面调用，避免查询时堵住别的会话。
// 两次并发的第一次查询可能都打到厂商，后写入的那个数留在文件里。
func (s *WindowStore) Get(ctx context.Context, key string, lookup Lookup) int {
	if s == nil || key == "" {
		return FallbackWindow
	}
	if n, ok := s.cached(key); ok {
		return n
	}
	if s.missed(key) {
		return FallbackWindow
	}
	if lookup == nil {
		return FallbackWindow
	}
	n, err := lookup(ctx, key)
	if err != nil || n <= 0 {
		if found, ok := s.cached(key); ok {
			return found
		}
		s.rememberMiss(key)
		return FallbackWindow
	}
	if err := s.save(key, n); err != nil {
		// 这一进程已经记住了。文件没写成，下次启动会再查一次。
		fmt.Fprintf(os.Stderr, "compact: 写入窗口大小失败: %v\n", err)
	}
	return n
}

// cached 先看内存，再看文件。文件里有，就补进内存，这一进程不再读盘。
func (s *WindowStore) cached(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.mem[key]; ok && n > 0 {
		return n, true
	}
	n, ok := s.readFile(key)
	if !ok {
		return 0, false
	}
	s.mem[key] = n
	return n, true
}

// missed 报告这一进程里这个模型是不是已经查失败过。
func (s *WindowStore) missed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.miss[key]
}

// rememberMiss 记下查失败。不写文件，所以下次启动还会再查。
func (s *WindowStore) rememberMiss(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.miss == nil {
		s.miss = map[string]bool{}
	}
	s.miss[key] = true
}

// save 把查到的正数写入内存和文件。0 和负数不会进到这里。
func (s *WindowStore) save(key string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem == nil {
		s.mem = map[string]int{}
	}
	s.mem[key] = n
	if s.path == "" {
		return nil
	}
	return s.writeFile(key, n)
}

// readFile 从 JSON 对象里取一个正数。文件不存在或这一项没有，都当没查到。
func (s *WindowStore) readFile(key string) (int, bool) {
	if s.path == "" {
		return 0, false
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return 0, false
	}
	var saved map[string]int
	if err := json.Unmarshal(raw, &saved); err != nil {
		return 0, false
	}
	n, ok := saved[key]
	return n, ok && n > 0
}

// writeFile 合并这一项后原子替换。读改写都在 save 的锁里，避免两笔写入互相覆盖。
func (s *WindowStore) writeFile(key string, n int) error {
	saved := map[string]int{}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &saved)
	}
	if saved == nil {
		saved = map[string]int{}
	}
	saved[key] = n
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "model-windows-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil {
		_ = os.Remove(tmpName)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(tmpName)
		return cerr
	}
	return os.Rename(tmpName, s.path)
}

// HTTPLookup 对厂商的 BaseURL 发 GET /models。apiKey 只放进 Authorization，不写日志。
// modelID 用来在返回的列表里对上这一项。
func HTTPLookup(baseURL, apiKey, modelID string) Lookup {
	return func(ctx context.Context, _ string) (int, error) {
		if strings.TrimSpace(baseURL) == "" || modelID == "" {
			return 0, fmt.Errorf("compact: 缺少模型地址或模型 ID")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(baseURL), nil)
		if err != nil {
			return 0, err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("compact: GET /models 返回 %d", resp.StatusCode)
		}
		return WindowFromModelsJSON(body, modelID)
	}
}

// modelsURL 拼模型列表地址。BaseURL 已经以 /models 结尾时不再加一段。
func modelsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(strings.ToLower(base), "/models") {
		return base
	}
	return base + "/models"
}

// windowFields 是读取顺序。命中第一个大于 0 的字段就停。
var windowFields = []string{
	"context_window",
	"context_length",
	"max_model_len",
	"max_context_length",
	"max_input_tokens",
}

// WindowFromModelsJSON 从 /models 的 JSON 里读出 modelID 的窗口大小。
// 接受 data 数组、models 数组、顶层数组，或单个带 id 的对象。数字也可以是字符串。
func WindowFromModelsJSON(raw []byte, modelID string) (int, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("compact: 解析 /models: %w", err)
	}
	for _, item := range modelObjects(parsed) {
		if !matchesModel(item, modelID) {
			continue
		}
		if n := positiveField(item); n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("compact: 模型 %s 没有窗口大小", modelID)
}

// modelObjects 把几种常见的列表形状收成对象数组。有 data 或 models 时用那一层，不再把外壳当成一个模型。
func modelObjects(v any) []map[string]any {
	switch node := v.(type) {
	case []any:
		return mapsOnly(node)
	case map[string]any:
		if items, ok := arrayField(node, "data"); ok {
			return items
		}
		if items, ok := arrayField(node, "models"); ok {
			return items
		}
		return []map[string]any{node}
	default:
		return nil
	}
}

// arrayField 取出对象里的数组字段，并只留下其中的对象元素。
func arrayField(node map[string]any, key string) ([]map[string]any, bool) {
	raw, ok := node[key]
	if !ok {
		return nil, false
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	return mapsOnly(arr), true
}

// mapsOnly 丢掉数组里不是对象的元素。
func mapsOnly(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if ok {
			out = append(out, obj)
		}
	}
	return out
}

// matchesModel 用 id、name、model 三个字段对模型 ID。三者有一个相同就算这一项。
func matchesModel(item map[string]any, modelID string) bool {
	if modelID == "" {
		return false
	}
	for _, key := range []string{"id", "name", "model"} {
		if text, ok := item[key].(string); ok && text == modelID {
			return true
		}
	}
	return false
}

// positiveField 按 windowFields 的顺序取第一个大于 0 的数。
func positiveField(item map[string]any) int {
	for _, key := range windowFields {
		if n, ok := asPositive(item[key]); ok {
			return n
		}
	}
	return 0
}

// asPositive 接受 JSON 数字和数字字符串。0 与负数不当作窗口大小。
func asPositive(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 && n < 1e15 {
			return int(n), true
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil && parsed > 0 {
			return parsed, true
		}
	case json.Number:
		parsed, err := n.Int64()
		if err == nil && parsed > 0 {
			return int(parsed), true
		}
	}
	return 0, false
}
