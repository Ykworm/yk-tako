// skillsource_test.go 钉住 C2 的解析层（doc/52 §6.5）：命令分词与白名单、来源串的
// 三种形态、SSRF 的 IP 判定、归档 URL 的构造。
//
// 这一层是纯函数，不打网络；它的错误会体现在 116109 的文案与「装到了哪个仓库」上——
// 一旦分词错了，`; rm -rf /` 会被当成参数的一部分，而 URL 构造错了会静默装错仓。
package runengine

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestParseSkillCommandOK(t *testing.T) {
	cases := []struct {
		in        string
		kind      string
		owner     string
		repo      string
		ref       string
		subpath   string
		skills    []string
		all       bool
		preview   bool
		wantWarns int
	}{
		{in: "npx skills add owner/repo", kind: "github", owner: "owner", repo: "repo"},
		{in: "skills add owner/repo --list", kind: "github", owner: "owner", repo: "repo", preview: true},
		{in: "npx skills add owner/repo@pdf", kind: "github", owner: "owner", repo: "repo", skills: []string{"pdf"}},
		{in: "npx skills add owner/repo#dev", kind: "github", owner: "owner", repo: "repo", ref: "dev"},
		{in: "npx skills add owner/repo@pdf#dev", kind: "github", owner: "owner", repo: "repo", ref: "dev", skills: []string{"pdf"}},
		{in: "npx skills add owner/repo -s pdf -s xlsx --list", kind: "github", owner: "owner", repo: "repo", skills: []string{"pdf", "xlsx"}, preview: true},
		{in: `npx skills add "owner/repo" --skill "pdf"`, kind: "github", owner: "owner", repo: "repo", skills: []string{"pdf"}},
		{in: "npx skills add owner/repo --all", kind: "github", owner: "owner", repo: "repo", all: true},
		{in: "npx skills add owner/repo --ref dev", kind: "github", owner: "owner", repo: "repo", ref: "dev"},
		{in: "npx skills add owner/repo -g -y", kind: "github", owner: "owner", repo: "repo", wantWarns: 2},
		{in: "npx skills add owner/repo -a claude", kind: "github", owner: "owner", repo: "repo", wantWarns: 1},
		{in: "npx skills add https://github.com/owner/repo/tree/main/skills/pdf",
			kind: "github", owner: "owner", repo: "repo", ref: "main", subpath: "skills/pdf"},
		{in: "npx skills add https://github.com/owner/repo", kind: "github", owner: "owner", repo: "repo"},
		{in: "npx skills add https://gitlab.com/group/sub/repo", kind: "gitlab", owner: "group/sub", repo: "repo"},
	}
	for _, c := range cases {
		spec, err := parseSkillCommand(c.in)
		if err != nil {
			t.Errorf("%q → 错误 %v", c.in, err)
			continue
		}
		namespaceOrOwner := spec.Owner
		if spec.Kind == "gitlab" {
			namespaceOrOwner = spec.Namespace
		}
		if spec.Kind != c.kind || namespaceOrOwner != c.owner || spec.Repo != c.repo ||
			spec.Ref != c.ref || spec.Subpath != c.subpath || spec.All != c.all || spec.Preview != c.preview {
			t.Errorf("%q → %+v", c.in, spec)
		}
		if len(spec.Skills) != len(c.skills) {
			t.Errorf("%q skills = %v，期望 %v", c.in, spec.Skills, c.skills)
		} else {
			for i := range c.skills {
				if spec.Skills[i] != c.skills[i] {
					t.Errorf("%q skills = %v，期望 %v", c.in, spec.Skills, c.skills)
					break
				}
			}
		}
		if len(spec.Warnings) != c.wantWarns {
			t.Errorf("%q warnings = %v，期望 %d 条", c.in, spec.Warnings, c.wantWarns)
		}
	}
}

func TestParseSkillCommandRejects(t *testing.T) {
	cases := []string{
		"",
		"npx skills",
		"npx skills list",
		"npx skills remove x",
		"npx skills add",
		"npx skills add x; rm -rf /",
		"npx skills add o/r | cat",
		"npx skills add o/r `id`",
		"npx skills add o/r $(id)",
		"npx skills add o/r > /tmp/x",
		`npx skills add "o/r`,
		"npx skills add o/r --all --skill pdf",
		"npx skills add o/r --ref a #b",
		"npx skills add o/r --unknown",
		"npx skills add o/r extra",
		"npx skills add ./local",
		"npx skills add /abs/path",
		"npx skills add file:///etc/passwd",
		"npx skills add git@github.com:o/r.git",
		"npx skills add ssh://git@github.com/o/r",
		"npx skills add pdf",
		"npx skills add 'o/r\nrm'",
	}
	for _, c := range cases {
		if _, err := parseSkillCommand(c); err == nil {
			t.Errorf("%q 应当被拒（116109）", c)
		} else {
			var werr *skillWriteErr
			if !errors.As(err, &werr) || werr.code != codeSkillSourceBad {
				t.Errorf("%q 的错误码不是 116109：%v", c, err)
			}
		}
	}
}

func TestParseSkillSourceForms(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		want string // Display()
	}{
		{"owner/repo", "github", "owner/repo"},
		{"owner/repo.git", "github", "owner/repo"},
		{"owner/repo@pdf#v1", "github", "owner/repo#v1"},
		{"https://github.com/o/r", "github", "o/r"},
		{"https://github.com/o/r.git", "github", "o/r"},
		{"https://github.com/o/r/tree/main/skills/pdf", "github", "o/r#main:skills/pdf"},
		{"https://gitlab.com/g/sub/r", "gitlab", "g/sub/r"},
		{"https://gitlab.com/g/sub/r/-/tree/v2/skills/x", "gitlab", "g/sub/r#v2:skills/x"},
		{"https://example.com/pkgs/x.tar.gz", "archive", "https://example.com/pkgs/x.tar.gz"},
		{"https://example.com/pkgs/x.tgz", "archive", "https://example.com/pkgs/x.tgz"},
		{"https://example.com/pkgs/x.zip", "archive", "https://example.com/pkgs/x.zip"},
	}
	for _, c := range cases {
		spec, err := parseSkillSource(c.in)
		if err != nil {
			t.Errorf("%q → 错误 %v", c.in, err)
			continue
		}
		if spec.Kind != c.kind {
			t.Errorf("%q kind = %q，期望 %q", c.in, spec.Kind, c.kind)
		}
		if got := spec.Display(); got != c.want {
			t.Errorf("%q Display = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestParseSkillSourceRejects(t *testing.T) {
	cases := []string{
		"git@github.com:o/r.git",
		"ssh://git@github.com/o/r",
		"git://github.com/o/r",
		"file:///tmp/x.zip",
		"./x.zip",
		"../x.zip",
		"/tmp/x.zip",
		"~/x.zip",
		"http://user:pw@example.com/x.zip",
		"ftp://example.com/x.zip",
		"https://example.com/path",
		"https://github.com/o",
		"https://github.com/o/r/blob/main/x.md",
		"https://gitlab.com/onlyone",
		"o/r/extra",
		"o/r@",
		"o/r#",
		"o r",
	}
	for _, c := range cases {
		if _, err := parseSkillSource(c); err == nil {
			t.Errorf("%q 应当被拒（116109）", c)
		}
	}
}

func TestSkillArchiveURLGithub(t *testing.T) {
	spec := skillSourceSpec{Kind: "github", Owner: "o", Repo: "r"}
	u, err := skillArchiveURL(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://codeload.github.com/o/r/tar.gz/HEAD" {
		t.Errorf("url = %q", u)
	}
	spec.Ref = "feature/x"
	u, err = skillArchiveURL(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://codeload.github.com/o/r/tar.gz/feature/x" {
		t.Errorf("带斜杠的 ref url = %q", u)
	}
}

func TestIsPublicSkillIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"100.64.0.1", false},
		{"0.0.0.0", false},
		{"224.0.0.1", false},
		{"::1", false},
		{"fc00::1", false},
		{"fd12:3456::1", false},
		{"fe80::1", false},
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
	}
	for _, c := range cases {
		if got := isPublicSkillIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("%s → %v，期望 %v", c.ip, got, c.want)
		}
	}
}

// 解析出来的命令只当结构体用：Display 不应把元字符或原始整串带回去。
func TestSkillSourceDisplayNoShell(t *testing.T) {
	spec, err := parseSkillCommand("npx skills add o/r --skill pdf")
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Display()
	for _, bad := range []string{";", "|", "$", "npx", "--skill"} {
		if strings.Contains(got, bad) {
			t.Errorf("Display = %q 含 %q", got, bad)
		}
	}
}
