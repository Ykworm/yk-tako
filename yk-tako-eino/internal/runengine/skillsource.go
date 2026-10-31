// skillsource.go 是技能来源的解析层（doc/52 §6.5 C2 拍）：把用户手里的一串东西
// 变成结构化参数，**从不拼回字符串、从不交给 exec**。
//
// 用户手里的东西有三种：`owner/repo`、一个 https 链接、或从 README 复制的整条
// `npx skills add …` 命令。三种都是**输入格式**——tako 不装 Node，也不为装一个
// Markdown 包引入执行面。这里只做分词与字段识别；抓取与校验在 skillfetch.go /
// skillresolve.go。
//
// 为什么要自己分词而不是 strings.Fields：命令里有引号（`--skill "a b"`），
// 而任何一条漏网的 `;` `|` `$` 都可能让「看起来是参数」的东西在别处被当成命令。
// 分完词仍然逐 token 拒这些字符——不经过 shell 的保证不能只靠「我们没调 exec」。
package runengine

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// skillSourceSpec 是一条来源解析后的结构。Kind 决定抓取方式：
//   - github：owner/repo（可带 ref / 子目录）→ 官方 archive tarball；
//   - gitlab：namespace/repo（可带 ref / 子目录）→ 官方 archive tarball；
//   - archive：一个 .zip/.tar.gz/.tgz 直链，原样 GET。
//
// Skills/All 是「装哪几个」的选择，不属于来源本身；Preview 只对命令里的 --list 有意义。
type skillSourceSpec struct {
	Kind      string   `json:"kind"`
	Owner     string   `json:"owner,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Ref       string   `json:"ref,omitempty"`
	Subpath   string   `json:"subpath,omitempty"`
	URL       string   `json:"url,omitempty"`
	Skills    []string `json:"skills,omitempty"`
	All       bool     `json:"all,omitempty"`
	Preview   bool     `json:"preview,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Display 是给错误文案与卡片用的一行来源（不是可再次解析的输入）。
func (s skillSourceSpec) Display() string {
	switch s.Kind {
	case "github":
		out := s.Owner + "/" + s.Repo
		if s.Ref != "" {
			out += "#" + s.Ref
		}
		if s.Subpath != "" {
			out += ":" + s.Subpath
		}
		return out
	case "gitlab":
		out := s.Namespace + "/" + s.Repo
		if s.Ref != "" {
			out += "#" + s.Ref
		}
		if s.Subpath != "" {
			out += ":" + s.Subpath
		}
		return out
	case "archive":
		return s.URL
	}
	return ""
}

// sourceErr 是来源解析失败（116109）。状态固定 400：输入问题，不是服务端故障。
func sourceErr(msg string) *skillWriteErr {
	return skillWriteError(400, codeSkillSourceBad, msg)
}

// parseSkillInput 是 preview / install 的共用入口：source 与 command 二选一。
func parseSkillInput(source, command string) (skillSourceSpec, error) {
	source = strings.TrimSpace(source)
	command = strings.TrimSpace(command)
	switch {
	case source == "" && command == "":
		return skillSourceSpec{}, sourceErr("请给 source（owner/repo 或链接）或 command（npx skills add …）")
	case source != "" && command != "":
		return skillSourceSpec{}, sourceErr("source 与 command 只能给一个")
	case command != "":
		return parseSkillCommand(command)
	default:
		return parseSkillSource(source)
	}
}

// splitSkillCommand 按空白切词，认单/双引号；引号不配对 → 116109。
//
// 空引号（`--skill ""`）会产出一个空 token，调用方按「缺值」处理。
func splitSkillCommand(cmd string) ([]string, error) {
	var (
		out     []string
		cur     strings.Builder
		quote   rune
		started bool
	)
	flush := func() {
		if started || cur.Len() > 0 {
			out = append(out, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\n' || r == '\r':
			return nil, sourceErr("命令里不能有换行")
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, sourceErr("命令里的引号没有配对")
	}
	flush()
	return out, nil
}

// commandMetaChars 是分词后仍拒的字符：它们只可能是别处的语法，不可能是来源/名字的一部分。
// URL 里的 `?` `&` `=` `#` `@` `/` `:` 是合法的，不在此列。
const commandMetaChars = ";|<>`$()"

// parseSkillCommand 解析 `npx skills add …`（§6.5 的白名单）。
//
// 只认 add；`--list` 只预览；社区 CLI 的作用域/交互开关接受并进 warnings。
// 未知 flag、多余位置参数、矛盾组合（--all 与 --skill 同给、#ref 与 --ref 不一致）
// 一律 116109——宁可让用户改一条命令，也不要替他猜「他其实想装什么」。
func parseSkillCommand(cmd string) (skillSourceSpec, error) {
	toks, err := splitSkillCommand(cmd)
	if err != nil {
		return skillSourceSpec{}, err
	}
	if len(toks) == 0 {
		return skillSourceSpec{}, sourceErr("命令是空的")
	}
	for _, t := range toks {
		if strings.ContainsAny(t, commandMetaChars) {
			return skillSourceSpec{}, sourceErr(fmt.Sprintf("命令里不能有 shell 元字符（%s）", commandMetaChars))
		}
	}
	i := 0
	if toks[i] == "npx" {
		i++
	}
	if i >= len(toks) || toks[i] != "skills" {
		return skillSourceSpec{}, sourceErr("只支持 npx skills add …（不执行安装器，只解析参数）")
	}
	i++
	if i >= len(toks) || toks[i] != "add" {
		sub := "（空）"
		if i < len(toks) {
			sub = toks[i]
		}
		return skillSourceSpec{}, sourceErr(fmt.Sprintf("只支持 add，不支持 %q（list/find/remove/update/check/init/use 都不认）", sub))
	}
	i++

	var (
		positional string
		skills     []string
		all        bool
		preview    bool
		refFlag    string
		warnings   []string
	)
	needValue := func(flag string) (string, error) {
		i++
		if i >= len(toks) || toks[i] == "" {
			return "", sourceErr(fmt.Sprintf("%s 后面缺值", flag))
		}
		return toks[i], nil
	}
	for ; i < len(toks); i++ {
		t := toks[i]
		switch t {
		case "--skill", "-s":
			v, err := needValue(t)
			if err != nil {
				return skillSourceSpec{}, err
			}
			skills = append(skills, v)
		case "--all":
			all = true
		case "--list":
			preview = true
		case "--ref":
			v, err := needValue(t)
			if err != nil {
				return skillSourceSpec{}, err
			}
			refFlag = v
		case "-g", "--global", "-p", "--project", "--copy", "-y", "--yes":
			warnings = append(warnings, fmt.Sprintf("已忽略 %s（tako 只有一个落点：租户技能根）", t))
		case "-a", "--agent":
			v, err := needValue(t)
			if err != nil {
				return skillSourceSpec{}, err
			}
			warnings = append(warnings, fmt.Sprintf("已忽略 %s %s（tako 没有「目标 agent 目录」）", t, v))
		case "--all-agents":
			warnings = append(warnings, "已忽略 --all-agents（tako 没有「目标 agent 目录」）")
		default:
			if strings.HasPrefix(t, "-") {
				return skillSourceSpec{}, sourceErr(fmt.Sprintf("不认识的参数 %s", t))
			}
			if positional != "" {
				return skillSourceSpec{}, sourceErr("命令里只能有一个来源参数")
			}
			positional = t
		}
	}
	if positional == "" {
		return skillSourceSpec{}, sourceErr("命令里没有来源（npx skills add <owner>/<repo>）")
	}
	if all && len(skills) > 0 {
		return skillSourceSpec{}, sourceErr("--all 与 --skill 不能同时给")
	}
	spec, err := parseSkillSource(positional)
	if err != nil {
		return skillSourceSpec{}, err
	}
	if spec.Kind == "archive" && (all || len(skills) > 0) {
		return skillSourceSpec{}, sourceErr("直链包不接受 --all/--skill（先预览看候选）")
	}
	if refFlag != "" {
		if spec.Ref != "" && spec.Ref != refFlag {
			return skillSourceSpec{}, sourceErr("来源里的 #ref 与 --ref 不一致")
		}
		spec.Ref = refFlag
	}
	spec.Skills = append(spec.Skills, skills...)
	spec.All = all
	spec.Preview = preview
	spec.Warnings = append(spec.Warnings, warnings...)
	return spec, nil
}

// parseSkillSource 解析不带命令前缀的来源串。
func parseSkillSource(raw string) (skillSourceSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return skillSourceSpec{}, sourceErr("来源是空的")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return skillSourceSpec{}, sourceErr("来源里不能有空白")
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "git@"), strings.HasPrefix(lower, "ssh://"), strings.HasPrefix(lower, "git://"):
		return skillSourceSpec{}, sourceErr("只支持公开 https 来源（SSH 与私有仓请先本地打包上传）")
	case strings.HasPrefix(lower, "file://"), strings.HasPrefix(raw, "./"), strings.HasPrefix(raw, "../"),
		strings.HasPrefix(raw, "/"), strings.HasPrefix(raw, "~/"):
		return skillSourceSpec{}, sourceErr("不支持服务端本地路径；本机目录请打包上传，运维手工拷目录可用 rescan")
	}
	if strings.Contains(lower, "://") {
		return parseSkillSourceURL(raw)
	}
	return parseOwnerRepo(raw)
}

// parseOwnerRepo 解析 `owner/repo`、`owner/repo@skill`、`owner/repo#ref`。
//
// 顺序：先切 `#ref` 再切 `@skill`——README 里两种写法都有，`owner/repo@skill#ref`
// 里 `#` 在最后。名字只允许 GitHub 的字符集，避免把 npm 包名或一句人话当仓库。
func parseOwnerRepo(raw string) (skillSourceSpec, error) {
	base := raw
	var (
		ref    string
		skills []string
	)
	if i := strings.Index(base, "#"); i >= 0 {
		ref = strings.TrimPrefix(base[i+1:], "refs/heads/")
		base = base[:i]
		if ref == "" {
			return skillSourceSpec{}, sourceErr("# 后面缺 ref")
		}
	}
	if i := strings.Index(base, "@"); i >= 0 {
		skill := base[i+1:]
		base = base[:i]
		if skill == "" {
			return skillSourceSpec{}, sourceErr("@ 后面缺技能名")
		}
		skills = append(skills, skill)
	}
	parts := strings.Split(base, "/")
	if len(parts) != 2 {
		return skillSourceSpec{}, sourceErr(fmt.Sprintf("来源 %q 不是 owner/repo 形式（也不像公开链接）", raw))
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if err := checkRepoName(owner, "owner"); err != nil {
		return skillSourceSpec{}, err
	}
	if err := checkRepoName(repo, "repo"); err != nil {
		return skillSourceSpec{}, err
	}
	if ref != "" && strings.ContainsAny(ref, " \t?#") {
		return skillSourceSpec{}, sourceErr("ref 不合法")
	}
	return skillSourceSpec{Kind: "github", Owner: owner, Repo: repo, Ref: ref, Skills: skills}, nil
}

// checkRepoName 只放 GitHub/GitLab 名字的字符集。空串也拒。
func checkRepoName(v, what string) error {
	if v == "" || v == "." || v == ".." {
		return sourceErr(fmt.Sprintf("%s 为空", what))
	}
	for _, r := range v {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			return sourceErr(fmt.Sprintf("%s %q 含不支持的字符", what, v))
		}
	}
	return nil
}

// parseSkillSourceURL 解析 GitHub / GitLab / 归档直链三种 https 来源。
func parseSkillSourceURL(raw string) (skillSourceSpec, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return skillSourceSpec{}, sourceErr(fmt.Sprintf("来源链接解析不了：%q", raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return skillSourceSpec{}, sourceErr("只支持 http(s) 链接")
	}
	if u.User != nil {
		return skillSourceSpec{}, sourceErr("链接里不能带账号密码（私有用本地打包）")
	}
	host := strings.ToLower(u.Hostname())
	segs := splitURLPath(u.Path)
	switch host {
	case "github.com", "www.github.com":
		return parseGitHubURL(segs)
	case "gitlab.com", "www.gitlab.com":
		return parseGitLabURL(segs)
	}
	if isArchivePath(u.Path) {
		return skillSourceSpec{Kind: "archive", URL: raw}, nil
	}
	return skillSourceSpec{}, sourceErr("只支持 GitHub / GitLab 仓库或 .zip/.tar.gz 直链")
}

// splitURLPath 切路径段并丢掉空段（尾部斜杠不产生空串）。
func splitURLPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isArchivePath 只认三种归档后缀（§6.5「直链」）。查询串不参与判断。
func isArchivePath(p string) bool {
	lower := strings.ToLower(p)
	return strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}

// parseGitHubURL 认 `https://github.com/o/r[.git]` 与 `…/tree/<ref>/<subpath>`。
func parseGitHubURL(segs []string) (skillSourceSpec, error) {
	if len(segs) < 2 {
		return skillSourceSpec{}, sourceErr("GitHub 链接缺 owner/repo")
	}
	owner := segs[0]
	repo := strings.TrimSuffix(segs[1], ".git")
	if err := checkRepoName(owner, "owner"); err != nil {
		return skillSourceSpec{}, err
	}
	if err := checkRepoName(repo, "repo"); err != nil {
		return skillSourceSpec{}, err
	}
	spec := skillSourceSpec{Kind: "github", Owner: owner, Repo: repo}
	rest := segs[2:]
	if len(rest) == 0 {
		return spec, nil
	}
	if len(rest) >= 2 && rest[0] == "tree" {
		spec.Ref = rest[1]
		spec.Subpath = path.Join(rest[2:]...)
		return spec, nil
	}
	return skillSourceSpec{}, sourceErr("只支持仓库主页与 /tree/<ref>/<子目录> 两种 GitHub 链接")
}

// parseGitLabURL 认 `https://gitlab.com/<namespace…>/<repo>` 与
// `…/-/tree/<ref>/<subpath>`。命名空间可以有多级（group/subgroup/repo）。
func parseGitLabURL(segs []string) (skillSourceSpec, error) {
	tree := -1
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "-" && segs[i+1] == "tree" {
			tree = i
			break
		}
	}
	repoEnd := len(segs)
	if tree >= 0 {
		repoEnd = tree
	}
	if repoEnd < 2 {
		return skillSourceSpec{}, sourceErr("GitLab 链接缺 namespace/repo")
	}
	repo := strings.TrimSuffix(segs[repoEnd-1], ".git")
	namespace := strings.Join(segs[:repoEnd-1], "/")
	if err := checkRepoName(repo, "repo"); err != nil {
		return skillSourceSpec{}, err
	}
	for _, part := range strings.Split(namespace, "/") {
		if err := checkRepoName(part, "namespace"); err != nil {
			return skillSourceSpec{}, err
		}
	}
	spec := skillSourceSpec{Kind: "gitlab", Namespace: namespace, Repo: repo}
	if tree >= 0 {
		if tree+2 >= len(segs) {
			return skillSourceSpec{}, sourceErr("GitLab 链接缺 ref")
		}
		spec.Ref = segs[tree+2]
		spec.Subpath = path.Join(segs[tree+3:]...)
	}
	return spec, nil
}
