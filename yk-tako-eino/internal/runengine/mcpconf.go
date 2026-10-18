package runengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
)

// mcp.json 的配置面（doc/33 §3）。决定连哪些 server 全在配置里，引擎零自动选择；
// 文件不存在或 mcpServers 为空 = 引擎行为与无此文件完全一致（无配置即无 MCP）。
// 形状照 Claude Code 惯例：{"mcpServers": {"<短名>": {"command","args","env"}}}。
type McpConfig struct {
	McpServers map[string]McpServerConfig `json:"mcpServers"`
}

// McpServerConfig 是一个 stdio server 的拉起参数：command+args 是子进程命令行，env 透传子进程环境。
type McpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// 短名是 mcp__<短名>__<工具名> 三段式的中段（doc/33 §4.1），收窄字符集保证拼出来的工具名无歧义。
var mcpServerNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

const defaultMcpConfPath = "mcp.json"

// loadMcpConfig 读引擎 cwd 下的 mcp.json（引擎须从 yk-tako-eino/ 起，与 agents/、workspace/ 同级）。
// 每次调用都重读文件——配置真源是文件、改完下一场 run 生效，与 agents 规格扫盘同一惯例；
// 不做进程内缓存，避免「改了配置某处还拿着旧值」的第二真源。
func loadMcpConfig(path string) (McpConfig, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return McpConfig{}, nil
	}
	if err != nil {
		return McpConfig{}, fmt.Errorf("mcp.json 读取失败: %w", err)
	}
	return parseMcpConfig(raw)
}

// parseMcpConfig：未知键拒载（DisallowUnknownFields 对齐 agents 规格 KnownFields(true) 的纪律——
// 拼写手误没有「悄悄忽略」的通道），短名与 command 在 parse 层大声报错。
func parseMcpConfig(raw []byte) (McpConfig, error) {
	var cfg McpConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return McpConfig{}, fmt.Errorf("mcp.json 解析失败（已知键：顶层 mcpServers；server 内 command/args/env）: %w", err)
	}
	for _, name := range sortedServerNames(cfg) {
		sc := cfg.McpServers[name]
		if !mcpServerNameRe.MatchString(name) {
			return McpConfig{}, fmt.Errorf("mcp.json server 短名 %q 不合法（只允许 a-z 0-9 _ -）", name)
		}
		if sc.Command == "" {
			return McpConfig{}, fmt.Errorf("mcp.json server %q 缺必填字段 command", name)
		}
	}
	return cfg, nil
}

// sortedServerNames 把 map 键定序：配置无序、响应字节必须有序（同份配置两次请求同字节）。
func sortedServerNames(cfg McpConfig) []string {
	names := make([]string, 0, len(cfg.McpServers))
	for name := range cfg.McpServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
