package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type opKind string

const (
	opRead  opKind = "read"
	opWrite opKind = "write"
)

var fsToolOps = map[string]opKind{
	"ls": opRead, "read_file": opRead, "glob": opRead, "grep": opRead,
	"write_file": opWrite, "edit_file": opWrite, "delete": opWrite,
}

type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
	Ask   Action = "ask"
)

type FsRule struct {
	Paths  []string
	Ops    []opKind
	Mode   Action
	Reason string
}

type Options struct {
	WorkspaceRoot    string
	Rules            []FsRule
	OutsideWorkspace Action
}

type Decision struct {
	Action Action
	Reason string
}

var defaultFsRules = []FsRule{
	{Paths: []string{"**/.env", "**/.env.*", "**/.git/**", "**/.ssh/**", "**/id_rsa*"}, Ops: []opKind{opRead, opWrite}, Mode: Deny, Reason: "敏感文件（密钥/凭据/版本库内部）"},
	{Paths: []string{"/etc/**", "/private/etc/**", "/System/**", "/usr/**", "/bin/**", "/sbin/**"}, Ops: []opKind{opWrite}, Mode: Deny, Reason: "系统目录不可写"},
}

func pathFromArgs(params map[string]any) string {
	for _, key := range []string{"path", "file_path"} {
		raw, ok := params[key].(string)
		if ok && strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

func globToRegExp(glob string) *regexp.Regexp {
	expanded := glob
	if strings.HasPrefix(glob, "~") {
		expanded = os.Getenv("HOME") + glob[1:]
	}
	var b strings.Builder
	for i := 0; i < len(expanded); i++ {
		switch expanded[i] {
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(expanded[i])
		default:
			b.WriteByte(expanded[i])
		}
	}
	re := b.String()
	re = strings.ReplaceAll(re, `**/`, "\x01")
	re = strings.ReplaceAll(re, `**`, "\x02")
	re = strings.ReplaceAll(re, `*`, "\x03")
	re = strings.ReplaceAll(re, "\x01", `(?:.*/)?`)
	re = strings.ReplaceAll(re, "\x02", `.*`)
	re = strings.ReplaceAll(re, "\x03", `[^/]*`)
	return regexp.MustCompile("^" + re + "$")
}

func pathMatches(candidates []string, pattern string) bool {
	re := globToRegExp(pattern)
	var prefix *regexp.Regexp
	if strings.HasSuffix(pattern, "/**") {
		prefix = globToRegExp(pattern[:len(pattern)-3])
	}
	for _, candidate := range candidates {
		if re.MatchString(candidate) {
			return true
		}
		if prefix != nil && prefix.MatchString(candidate) {
			return true
		}
	}
	return false
}

func realpathIfAvailable(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return real
}

// Decide 是纯函数：allow 放行，deny 硬拒，ask 暂停问人。
func Decide(options Options, toolName string, params map[string]any) Decision {
	op, ok := fsToolOps[toolName]
	if !ok {
		return Decision{Action: Allow}
	}
	rawPath := pathFromArgs(params)
	if rawPath == "" {
		return Decision{Action: Allow}
	}
	root, err := filepath.Abs(options.WorkspaceRoot)
	if err != nil {
		root = options.WorkspaceRoot
	}
	absolute := rawPath
	if !filepath.IsAbs(rawPath) {
		absolute = filepath.Join(root, rawPath)
	}
	absolute = filepath.Clean(absolute)
	candidates := []string{absolute}
	if real := realpathIfAvailable(absolute); real != "" && real != absolute {
		candidates = append(candidates, real)
	}
	rules := append(append([]FsRule{}, options.Rules...), defaultFsRules...)
	for _, rule := range rules {
		if !containsOp(rule.Ops, op) {
			continue
		}
		matched := false
		for _, pattern := range rule.Paths {
			if pathMatches(candidates, pattern) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if rule.Mode == Allow {
			return Decision{Action: Allow}
		}
		verb := "读取"
		if op == opWrite {
			verb = "写入"
		}
		reason := verb + " " + absolute
		if rule.Reason != "" {
			reason += "（" + rule.Reason + "）"
		}
		return Decision{Action: rule.Mode, Reason: reason}
	}
	inWorkspace := false
	for _, candidate := range candidates {
		if candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator)) {
			inWorkspace = true
			break
		}
	}
	if inWorkspace {
		return Decision{Action: Allow}
	}
	verb := "读取"
	if op == opWrite {
		verb = "写入"
	}
	outside := options.OutsideWorkspace
	if outside == "" {
		outside = Ask
	}
	return Decision{Action: outside, Reason: verb + " " + absolute + "（超出工作区 " + root + "）"}
}

func containsOp(ops []opKind, op opKind) bool {
	for _, candidate := range ops {
		if candidate == op {
			return true
		}
	}
	return false
}
