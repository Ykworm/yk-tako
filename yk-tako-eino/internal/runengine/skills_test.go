// skills_test.go 钉住技能面（doc/52）的几处口径：frontmatter 解析、requires 的三种写法、
// 名字与目录名的一致性、扫盘跳过 . 开头的目录。
//
// 这些是「解析出来的值会进表进目录」的边界——错了不会崩，会安静地把错的依赖名
// 写进表里（例：把 `[python3]` 整体当成一个依赖名）。
package runengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSkillFrontmatter(t *testing.T) {
	body := "---\nname: doctor-registration\ndescription: 当医生提供登记信息时使用。\nrequires: [python3, ffmpeg]\n---\n\n# 医生登记\n\n正文。\n"
	fm, err := parseSkillFrontmatter(body)
	if err != nil {
		t.Fatalf("解析 frontmatter 失败: %v", err)
	}
	if fm.Name != "doctor-registration" {
		t.Errorf("name = %q", fm.Name)
	}
	if fm.Description != "当医生提供登记信息时使用。" {
		t.Errorf("description = %q", fm.Description)
	}
	if len(fm.Requires) != 2 || fm.Requires[0] != "python3" || fm.Requires[1] != "ffmpeg" {
		t.Errorf("requires = %v", fm.Requires)
	}
}

// requires 的三种写法都要拆成同样的元素列表。
// 承重的一条：`[python3]` 不是合法 JSON——只走 JSON 解析会失败，
// 回退成逗号分隔就会把整串 "[python3]" 当成一个依赖名。
func TestParseRequiresValue(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"[python3]", []string{"python3"}},
		{"[python3, ffmpeg]", []string{"python3", "ffmpeg"}},
		{`["python3", "ffmpeg"]`, []string{"python3", "ffmpeg"}},
		{"python3", []string{"python3"}},
		{"python3, ffmpeg", []string{"python3", "ffmpeg"}},
		{"", []string{}},
	}
	for _, c := range cases {
		got := parseRequiresValue(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%q → %v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q → %v，期望 %v", c.in, got, c.want)
				break
			}
		}
	}
}

// 没有 frontmatter、或 frontmatter 没闭合 → 解析失败（不是「空技能」）。
func TestParseSkillFrontmatterErrors(t *testing.T) {
	if _, err := parseSkillFrontmatter("# 标题\n\n没有 frontmatter。\n"); err == nil {
		t.Error("没有 frontmatter 时应当报错")
	}
	if _, err := parseSkillFrontmatter("---\nname: x\ndescription: y\n\n没闭合。\n"); err == nil {
		t.Error("frontmatter 未闭合时应当报错")
	}
}

// 扫盘：目录形态认 `<name>/SKILL.md`；`.` 开头的目录跳过；缺 description 的进 invalid。
func TestScanSkillRoot(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("good/SKILL.md", "---\nname: good\ndescription: 好的技能\n---\n正文\n")
	mk("bad/SKILL.md", "---\nname: bad\n---\n没有 description\n")
	mk(".staging/tmp/SKILL.md", "---\nname: tmp\ndescription: 临时目录不该被扫到\n---\n")

	entries, info, err := scanSkillRoot(root, "tenant")
	if err != nil {
		t.Fatalf("扫盘失败: %v", err)
	}
	if !info.Exists {
		t.Error("根存在时 Exists 应为 true")
	}
	if info.Count != 2 {
		t.Errorf("count = %d，期望 2（.staging 要跳过）", info.Count)
	}
	byName := map[string]skillScanEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["good"]; !ok || !e.Valid {
		t.Errorf("good 应当有效: %+v", e)
	}
	if e, ok := byName["bad"]; !ok || e.Valid || e.Reason != "missing description" {
		t.Errorf("bad 应当 invalid 且原因是 missing description: %+v", e)
	}
	if _, ok := byName["tmp"]; ok {
		t.Error(".staging 里的东西不该被扫到")
	}
}

// 根不存在 = 正常状态（视同空），不是失败。读失败才是失败。
func TestScanSkillRootMissing(t *testing.T) {
	entries, info, err := scanSkillRoot(filepath.Join(t.TempDir(), "nope"), "tenant")
	if err != nil {
		t.Fatalf("根不存在应当视同空: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries 应当为空: %v", entries)
	}
	if info.Exists {
		t.Error("Exists 应当为 false")
	}
	if info.Error != "" {
		t.Errorf("Error 应当为空: %q", info.Error)
	}
}

// frontmatter 的 name 与目录名不一致 → invalid（名字是唯一键的一部分，
// 两处不一致时按哪边算都会让「这个名字指向哪个技能」变成猜谜）。
func TestScanSkillDirNameMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dirname")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: othername\ndescription: 名字对不上\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := scanSkillDir(root, "dirname")
	if entry.Valid {
		t.Errorf("名字不一致时应当 invalid: %+v", entry)
	}
}

// 空实例 → 空数组，且**元素字段序固定**（doc/52 §6.1 的 skills[] 形状）。
//
// 为什么放单测而不是活体断言：活体断言跑在共享隔离库上，表里可能有别的租户留下的行，
// 断「数组为空」就变成断数据库状态（不是断契约）。空数组这条口径由这里钉住；
// 活体那边断的是「data 有 skills[] 与 invalid[] 两个键」。
func TestSkillsResponseEmptyAndFieldOrder(t *testing.T) {
	// 空：两个数组都是空数组（不是 null——null 会让前端多判一次）。
	data := skillsResponse(nil, nil, nil, "")
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"skills":[],"invalid":[]}`; got != want {
		t.Errorf("空实例字节 = %s，期望 %s", got, want)
	}
	// 一行：字段序必须逐字固定（前端按这个顺序渲染）。
	row := skillBody{
		Name: "demo", Description: "演示技能", Source: "tenant", Path: "/p/SKILL.md",
		Enabled: true, Bytes: 123, Requires: []string{"python3"}, Missing: []string{},
		UpdatedAt: "2026-10-28T00:00:00Z", Valid: true,
	}
	raw, err = json.Marshal(skillsData{Skills: []skillBody{row}, Invalid: []invalidSkillBody{}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"skills":[{"name":"demo","description":"演示技能","source":"tenant","path":"/p/SKILL.md",` +
		`"enabled":true,"bytes":123,"requires":["python3"],"missing":[],"updatedAt":"2026-10-28T00:00:00Z","valid":true}],"invalid":[]}`
	if got := string(raw); got != want {
		t.Errorf("一行字节 =\n%s\n期望\n%s", got, want)
	}
}

// 详情体（A4）：键序 skill → body 固定；skill 元素与列表同一份 skillBodyOf 拼出来，
// 字段序不会走样。body 是盘上逐字原文，这里的断言只管信封形状。
func TestSkillDetailDataFieldOrder(t *testing.T) {
	detail := skillDetailData{
		Skill: skillBody{
			Name: "demo", Description: "演示技能", Source: "tenant", Path: "/p/SKILL.md",
			Enabled: true, Bytes: 123, Requires: []string{}, Missing: []string{},
			UpdatedAt: "2026-10-28T00:00:00Z", Valid: true,
		},
		Body: "---\nname: demo\n---\n",
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"skill":{"name":"demo","description":"演示技能","source":"tenant","path":"/p/SKILL.md",` +
		`"enabled":true,"bytes":123,"requires":[],"missing":[],"updatedAt":"2026-10-28T00:00:00Z","valid":true},` +
		`"body":"---\nname: demo\n---\n"}`
	if got := string(raw); got != want {
		t.Errorf("详情体字节 =\n%s\n期望\n%s", got, want)
	}
}

// **SkillRow.Valid 不能带 gorm 的 default:true**（2026-10-28 A2 踩到）：
// GORM 插入时跳过零值字段，带默认值就意味着 Valid=false 根本不进 INSERT，
// 数据库落回 true——坏技能被当成生效技能进了目录，reason 却挂在行上，
// 看起来像「校验失败但可用」。这条断言直接读结构体 tag，不依赖数据库。
func TestSkillRowValidHasNoDefault(t *testing.T) {
	field, ok := reflect.TypeOf(SkillRow{}).FieldByName("Valid")
	if !ok {
		t.Fatal("SkillRow 没有 Valid 字段")
	}
	tag := field.Tag.Get("gorm")
	if strings.Contains(tag, "default:true") {
		t.Errorf("Valid 的 gorm tag 带 default:true，GORM 会跳过零值 → false 永远写不进去: %q", tag)
	}
	if !strings.Contains(tag, "not null") {
		t.Errorf("Valid 的 gorm tag 应当 not null: %q", tag)
	}
}

// 主键必须带桶：系统桶（tenant_id 空串）与租户桶可以有同名行——
// 系统行全集群一份、租户行归这一家，两者是不同的行。
// 主键不带租户就会撞（同一张表里两个 sk_pdf）。
func TestSkillRowIDCarriesBucket(t *testing.T) {
	sys := skillRowID("", "pdf")
	tenant := skillRowID("t_01M5", "pdf")
	if sys == tenant {
		t.Errorf("系统行与租户行的主键撞了: %q", sys)
	}
	if skillRowID("t_01M5", "pdf") != tenant {
		t.Errorf("同一个桶同一个名字的主键应当稳定")
	}
}

// 同一根里两份声明同一个 name → 按路径序先占的生效，后来者 invalid
// （doc/52 §10 A3）。真实形态是「目录形态与平铺形态同名」：
// `skills/x/SKILL.md` 与 `skills/x.md` 同时在盘上——两份的 name 都是 x。
// 两个都生效会让「这个 name 指向哪个技能」变成猜谜。
func TestScanSkillRootDuplicateName(t *testing.T) {
	root := t.TempDir()
	// 平铺形态
	if err := os.WriteFile(filepath.Join(root, "x.md"),
		[]byte("---\nname: x\ndescription: 平铺的那份\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目录形态
	dir := filepath.Join(root, "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: x\ndescription: 目录的那份\n---\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, _, err := scanSkillRoot(root, "tenant")
	if err != nil {
		t.Fatalf("扫盘失败: %v", err)
	}
	valid := 0
	invalid := 0
	for _, e := range entries {
		if e.Name != "x" {
			continue
		}
		if e.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid != 1 || invalid != 1 {
		t.Errorf("同名两份应当一成一败（先占的生效），实际 valid=%d invalid=%d", valid, invalid)
	}
}

// frontmatter 的折行写法（真实仓库里极常见）必须读出完整 description：
// `description:` 换行缩进、`>` 折行、`requires:` 块序列三种都要认。
// 只读第一行会把合法技能判成「缺少 description」，整仓都装不上。
func TestParseSkillFrontmatterMultiline(t *testing.T) {
	body := "---\nname: x\ndescription: >\n  first line\n  second line\nrequires:\n  - python3\n  - ffmpeg\n---\n正文\n"
	fm, err := parseSkillFrontmatter(body)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Description != "first line second line" {
		t.Errorf("description = %q", fm.Description)
	}
	if len(fm.Requires) != 2 || fm.Requires[0] != "python3" || fm.Requires[1] != "ffmpeg" {
		t.Errorf("requires = %v", fm.Requires)
	}

	plain := "---\nname: y\ndescription:\n  folded plain\n  continues here\n---\n"
	fm, err = parseSkillFrontmatter(plain)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Description != "folded plain continues here" {
		t.Errorf("plain 续行 description = %q", fm.Description)
	}
}
