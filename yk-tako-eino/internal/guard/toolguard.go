package guard

import (
	"fmt"
	"regexp"
	"strings"
)

type Severity string

const (
	Critical Severity = "CRITICAL"
	High     Severity = "HIGH"
	Medium   Severity = "MEDIUM"
	Low      Severity = "LOW"
	Info     Severity = "INFO"
)

type Rule struct {
	ID          string
	Tools       []string
	Params      []string
	Severity    Severity
	Patterns    []*regexp.Regexp
	Excludes    []*regexp.Regexp
	Description string
	Remediation string
}

var (
	shellTools = []string{"bash", "execute"}
	commandArg = []string{"command"}
)

func must(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

// DefaultRules 从 harness 的危险命令表里抽出的 10 条。安全知识与语言无关。
var DefaultRules = []Rule{
	{
		ID: "TOOL_CMD_DANGEROUS_RM", Tools: shellTools, Params: commandArg, Severity: High,
		Patterns:    []*regexp.Regexp{must(`\brm\b`), must(`\bdel\b`), must(`(?i)\bRemove-Item\b`)},
		Excludes:    []*regexp.Regexp{commentLine},
		Description: "命令包含 rm/del，可能造成数据丢失",
		Remediation: "执行删除前与用户确认",
	},
	{
		ID: "TOOL_CMD_DANGEROUS_MV", Tools: shellTools, Params: commandArg, Severity: High,
		Patterns:    []*regexp.Regexp{must(`\bmv\b`)},
		Description: "命令包含 mv，可能意外移动或覆盖文件",
		Remediation: "执行移动/重命名前与用户确认",
	},
	{
		ID: "TOOL_CMD_FS_DESTRUCTION", Tools: shellTools, Params: commandArg, Severity: Critical,
		Patterns: []*regexp.Regexp{
			must(`(?i)\bmkfs(\.[a-z0-9_]+)?\b`),
			must(`(?i)\bmke2fs\b`),
			must(`(?i)\bdd\s+.*of=/dev/`),
			must(`>\s*/dev/(sd[a-z]\d*|vd[a-z]\d*|nvme\d+n\d+(p\d+)?)`),
		},
		Description: "检测到磁盘格式化/擦除等底层破坏性命令",
		Remediation: "直接拦截。agent 不应操作裸块设备。",
	},
	{
		ID: "TOOL_CMD_DOS_FORK_BOMB", Tools: shellTools, Params: commandArg, Severity: Critical,
		Patterns: []*regexp.Regexp{
			must(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
			must(`\bkill\s+-9\s+(-1\b|1\b)`),
		},
		Description: "检测到 fork 炸弹或批量杀进程命令",
		Remediation: "立即拦截。这类命令会搞垮宿主机。",
	},
	{
		ID: "TOOL_CMD_PIPE_TO_SHELL", Tools: shellTools, Params: commandArg, Severity: Critical,
		Patterns:    []*regexp.Regexp{must(`(?i)\b(curl|wget)\b\s+.*\|.*\b(bash|sh|zsh|ash|dash)\b`)},
		Description: "检测到 curl|bash 模式：下载并立即执行远程脚本",
		Remediation: "与用户确认。agent 应先审查脚本内容再执行。",
	},
	{
		ID: "TOOL_CMD_REVERSE_SHELL", Tools: shellTools, Params: commandArg, Severity: Critical,
		Patterns: []*regexp.Regexp{
			must(`(?i)/dev/(tcp|udp)/`),
			must(`(?i)\bnc\s+.*-e\s*\S+`),
			must(`(?i)\bncat\s+.*-e\s*\S+`),
			must(`(?i)\bsocat\s+.*EXEC:`),
		},
		Description: "检测到反弹 shell / 未授权网络隧道",
		Remediation: "直接拦截。agent 无需绑定交互式 shell 或隧道。",
	},
	{
		ID: "TOOL_CMD_UNICODE_WHITESPACE", Tools: shellTools, Params: commandArg, Severity: High,
		Patterns:    []*regexp.Regexp{must("[\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]")},
		Description: "命令含 Unicode 空白字符，可能造成解析歧义绕过检查",
		Remediation: "拦截含非 ASCII 空白的命令，使用普通空格。",
	},
	{
		ID: "TOOL_CMD_PROC_ENVIRON", Tools: shellTools, Params: commandArg, Severity: High,
		Patterns:    []*regexp.Regexp{must(`/proc/(?:self|\d+)/environ(?:\b|$)`)},
		Excludes:    []*regexp.Regexp{commentLine},
		Description: "访问 /proc/*/environ，可能泄露 API Key 等环境变量",
		Remediation: "拦截对进程环境变量文件的访问。",
	},
	{
		ID: "TOOL_CMD_JQ_SYSTEM", Tools: shellTools, Params: commandArg, Severity: High,
		Patterns:    []*regexp.Regexp{must(`(?i)\bjq\b.*\bsystem\s*\(`)},
		Excludes:    []*regexp.Regexp{commentLine},
		Description: "jq 的 system() 可执行任意命令，把数据处理工具变成执行通道",
		Remediation: "拦截使用 system() 的 jq 命令。",
	},
	{
		ID: "TOOL_CMD_SUDO", Tools: shellTools, Params: commandArg, Severity: Medium,
		Patterns:    []*regexp.Regexp{must(`(?i)\bsudo\b`), must(`(?i)\bdoas\b`)},
		Excludes:    []*regexp.Regexp{commentLine},
		Description: "命令试图提升权限（sudo/doas）",
		Remediation: "agent 原则上不应需要提权，与用户确认。",
	},
}

// commentLine 的源码要和 isCommentLineExclude 里的字面量一致。
var commentLine = must(`^\s*#`)

type Mode string

const (
	ModeBlock   Mode = "block"
	ModeWarn    Mode = "warn"
	ModeApprove Mode = "require_approval"
)

type Finding struct {
	RuleID      string
	Severity    Severity
	ToolName    string
	ParamName   string
	Snippet     string
	Description string
	Remediation string
}

var blocking = map[Mode]map[Severity]bool{
	ModeBlock:   {Critical: true, High: true},
	ModeApprove: {Critical: true, High: true, Medium: true},
	ModeWarn:    {},
}

type Engine struct {
	rules   []Rule
	enabled bool
	mode    Mode
}

func NewEngine(mode Mode, rules []Rule) *Engine {
	if mode == "" {
		mode = ModeBlock
	}
	if rules == nil {
		rules = DefaultRules
	}
	return &Engine{rules: rules, enabled: true, mode: mode}
}

func (e *Engine) Configure(mode Mode) {
	if mode != "" {
		e.mode = mode
	}
}

func (e *Engine) Mode() Mode { return e.mode }

func (e *Engine) Guard(toolName string, params map[string]any) []Finding {
	if !e.enabled {
		return nil
	}
	var findings []Finding
	for name, raw := range params {
		value := ""
		if raw != nil {
			switch typed := raw.(type) {
			case string:
				value = typed
			default:
				value = strings.TrimSpace(fmt.Sprint(typed))
			}
		}
		if value == "" {
			continue
		}
		for _, rule := range e.rules {
			if len(rule.Tools) > 0 && !contains(rule.Tools, toolName) {
				continue
			}
			if len(rule.Params) > 0 && !contains(rule.Params, name) {
				continue
			}
			scanned, ok := textForRule(value, rule)
			if !ok || strings.TrimSpace(scanned) == "" {
				continue
			}
			var hit *regexp.Regexp
			for _, pattern := range rule.Patterns {
				if pattern.MatchString(scanned) {
					hit = pattern
					break
				}
			}
			if hit == nil {
				continue
			}
			loc := hit.FindStringIndex(scanned)
			start, end := 0, 0
			if loc != nil {
				start, end = loc[0], loc[1]
			}
			from := start - 40
			if from < 0 {
				from = 0
			}
			to := end + 40
			if to > len(scanned) {
				to = len(scanned)
			}
			findings = append(findings, Finding{
				RuleID: rule.ID, Severity: rule.Severity, ToolName: toolName, ParamName: name,
				Snippet: scanned[from:to], Description: rule.Description, Remediation: rule.Remediation,
			})
		}
	}
	return findings
}

func (e *Engine) ShouldBlock(findings []Finding) bool {
	blocked := blocking[e.mode]
	for _, finding := range findings {
		if blocked[finding.Severity] {
			return true
		}
	}
	return false
}

func contains(list []string, item string) bool {
	for _, candidate := range list {
		if candidate == item {
			return true
		}
	}
	return false
}

func isCommentLineExclude(pattern *regexp.Regexp) bool {
	return pattern.String() == commentLine.String()
}

// textForRule：`^\s*#` 只剥注释行，剩下的文本继续匹配。其它排除模式命中整段则本条规则不报警。
func textForRule(value string, rule Rule) (string, bool) {
	var comments, other []*regexp.Regexp
	for _, pattern := range rule.Excludes {
		if isCommentLineExclude(pattern) {
			comments = append(comments, pattern)
		} else {
			other = append(other, pattern)
		}
	}
	for _, pattern := range other {
		if pattern.MatchString(value) {
			return "", false
		}
	}
	if len(comments) == 0 {
		return value, true
	}
	var kept []string
	for _, line := range strings.Split(value, "\n") {
		skip := false
		for _, pattern := range comments {
			if pattern.MatchString(line) {
				skip = true
				break
			}
		}
		if !skip {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n"), true
}
