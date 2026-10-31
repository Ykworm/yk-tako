// skills_scan.go 是「盘 → 表」这一半（doc/52 §4.7）：rescan 的扫盘、frontmatter 解析、
// 与表的对账。另一半（表 → 盘）是 A/C 段的安装写路径。
//
// **rescan 不是日常步骤，是补偿动作**：装/删/改一次要动两个系统——盘上的文件、PG 里的行。
// 普通写库有事务兜底，这一笔跨了文件系统和数据库，没有能同时提交两边的机制。
// rescan 把表重新对齐到盘，是「以盘为准」的唯一入口。
//
// **扫盘范围与比对范围必须一致**：扫哪几个根，就只动那几个归属的行。
// 这不是额外规则，是 skill_rows 上 scope/tenant_id/project_id 三列存在的理由。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// skillNamePattern 是技能名的合法形状（doc/52 §9-4）。
// 名字必须与目录名（或平铺文件名）一致——它是技能的标识，也是唯一键的一部分。
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// skillFrontmatter 是技能包 frontmatter 里**只认**的那几个字段（doc/52 §2.1）。
//
// 未知键 v1 忽略、不报错，也不进目录。这里刻意不做 YAML 全集：技能包是作者写的，
// 但引擎只承诺认这几个字段；多认一个字段就多一份「作者以为生效了其实没有」的风险。
type skillFrontmatter struct {
	Name        string
	Description string
	Requires    []string
}

// skillScanEntry 是扫盘见到的一个技能目录（还没进表）。
type skillScanEntry struct {
	Name        string
	Path        string // 入口文件绝对路径
	Description string
	Requires    []string
	Bytes       int64
	Valid       bool
	Reason      string
}

// skillRootInfo 报告「这次扫了哪个根、读到几条、出了什么事」。
// 它不是保护机制，是让返回体说人话——盘出问题时运维不必猜 removed:37 是什么意思。
type skillRootInfo struct {
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Exists bool   `json:"exists"`
	Count  int    `json:"count"`
	Error  string `json:"error"`
}

// skillRescanResult 是 POST /api/skills/rescan 的 data。
type skillRescanResult struct {
	Added   []string           `json:"added"`
	Updated []string           `json:"updated"`
	Removed []string           `json:"removed"`
	Invalid []invalidSkillBody `json:"invalid"`
	Roots   []skillRootInfo    `json:"roots"`
}

// scanSkillRoot 扫一个技能根，返回这一根下的技能条目与根的状态。
//
// **空根与失败是两回事**（doc/52 §9-35）：
//   - 根**不存在**或读到 0 条 → 正常状态（这个归属下还没装技能）→ 视同空，不中止。
//   - 根**读失败**（权限、I/O 错误）→ 盘有问题 → 中止整次重扫，什么都不改。
//
// 这条区分是承重的：部署流程同步完出厂技能后调 rescan，撞上「读失败」就必须真失败
// （发布重试或回滚），而不是当成「盘是空的」把出厂技能全删了。
func scanSkillRoot(root, scope string) ([]skillScanEntry, skillRootInfo, error) {
	info := skillRootInfo{Path: root, Scope: scope}
	dirents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 根不存在 = 这个归属下还没装过技能。视同空，不中止。
			info.Exists = false
			return nil, info, nil
		}
		// 读失败 = 我没看到盘（权限、I/O）。这是唯一可靠的「盘有问题」信号。
		info.Error = err.Error()
		return nil, info, fmt.Errorf("读技能根 %s 失败: %w", root, err)
	}
	info.Exists = true
	entries := []skillScanEntry{}
	for _, dirent := range dirents {
		name := dirent.Name()
		// 目录名以 . 开头的（.staging、.trash）一律跳过——那是安装过程中的临时目录，
		// 不是技能。扫进去会让目录里冒出一堆没名字的条目。
		if strings.HasPrefix(name, ".") {
			continue
		}
		if dirent.IsDir() {
			entries = append(entries, scanSkillDir(root, name))
			continue
		}
		// 平铺形态 `<name>.md`：正文就在这个文件里（doc/52 §2.1）。
		// 它自己就是技能根，所在目录不是它的资源目录。
		if strings.HasSuffix(name, ".md") {
			entries = append(entries, scanSkillFlat(root, name))
		}
	}
	// 同一根里两份声明同一个 name → **按路径序先占的生效，后来者 invalid**
	// （doc/52 §10 A3）。最常见的一种是「目录形态与平铺形态同名」：
	// `skills/x/SKILL.md` 与 `skills/x.md` 同时在盘上。
	// 为什么不能两个都生效：名字是唯一键的一部分，两个生效行会让
	// 「这个 name 指向哪个技能」变成猜谜。
	// 为什么按路径序：得有一个确定的裁决顺序，否则两次扫可能挑到不同的那个。
	// entries 的顺序就是 ReadDir 的顺序（按文件名），所以先遇到的占位。
	claimed := map[string]string{}
	for i := range entries {
		if !entries[i].Valid {
			continue
		}
		if first, dup := claimed[entries[i].Name]; dup {
			entries[i].Valid = false
			entries[i].Reason = fmt.Sprintf("name %q 已被 %s 占用", entries[i].Name, first)
			continue
		}
		claimed[entries[i].Name] = entries[i].Path
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	info.Count = len(entries)
	return entries, info, nil
}

// scanSkillFlat 扫一个平铺形态的技能文件（`<name>.md`）。
//
// 平铺形态**只有正文、没有资源**：`<name>.md` 自己就是技能根，所在目录不是它的资源目录
// （否则 `path:"别的技能.md"` 就能读到隔壁技能，doc/52 §2.1）。
func scanSkillFlat(root, fileName string) skillScanEntry {
	path := filepath.Join(root, fileName)
	name := strings.TrimSuffix(fileName, ".md")
	entry := skillScanEntry{Name: name, Path: path}
	body, err := os.ReadFile(path)
	if err != nil {
		entry.Valid = false
		entry.Reason = "read failed"
		return entry
	}
	entry.Bytes = int64(len(body))
	fm, ferr := parseSkillFrontmatter(string(body))
	if ferr != nil {
		entry.Valid = false
		entry.Reason = ferr.Error()
		return entry
	}
	entry.Description = fm.Description
	entry.Requires = fm.Requires
	if fm.Name != "" && fm.Name != name {
		entry.Valid = false
		entry.Reason = fmt.Sprintf("name %q 不等于文件名 %q", fm.Name, name)
		return entry
	}
	if !skillNamePattern.MatchString(name) {
		entry.Valid = false
		entry.Reason = "name 不合法"
		return entry
	}
	if strings.TrimSpace(fm.Description) == "" {
		entry.Valid = false
		entry.Reason = "missing description"
		return entry
	}
	entry.Valid = true
	return entry
}

// scanSkillDir 扫一个技能目录，解析 frontmatter，算字节数。
//
// 包形态两种（doc/52 §2.1）：目录形态 `<name>/SKILL.md`、平铺形态 `<name>.md`。
// 目录形态优先——目录里有 SKILL.md 就用它；没有就退回平铺文件。
func scanSkillDir(root, dirName string) skillScanEntry {
	dirPath := filepath.Join(root, dirName)
	entryFile := filepath.Join(dirPath, "SKILL.md")
	flatFile := filepath.Join(root, dirName+".md")

	entryPath := entryFile
	body, err := os.ReadFile(entryFile)
	if err != nil {
		// 没有 SKILL.md → 看平铺形态。
		body, err = os.ReadFile(flatFile)
		if err != nil {
			return skillScanEntry{
				Name:   dirName,
				Path:   dirPath,
				Valid:  false,
				Reason: "no SKILL.md",
				Bytes:  dirBytes(dirPath),
			}
		}
		entryPath = flatFile
	}

	entry := skillScanEntry{Name: dirName, Path: entryPath, Bytes: dirBytes(dirPath)}
	fm, ferr := parseSkillFrontmatter(string(body))
	if ferr != nil {
		entry.Valid = false
		entry.Reason = ferr.Error()
		return entry
	}
	entry.Description = fm.Description
	entry.Requires = fm.Requires
	// 名字：frontmatter 写了就用它，没写用目录名。写了就必须与目录名一致——
	// 名字是唯一键的一部分，两处不一致时按哪边算都会让「这个名字指向哪个技能」变成猜谜。
	if fm.Name != "" && fm.Name != dirName {
		entry.Valid = false
		entry.Reason = fmt.Sprintf("name %q 不等于目录名 %q", fm.Name, dirName)
		return entry
	}
	if fm.Name == "" {
		fm.Name = dirName
	}
	entry.Name = fm.Name
	if !skillNamePattern.MatchString(entry.Name) {
		entry.Valid = false
		entry.Reason = "name 不合法"
		return entry
	}
	if strings.TrimSpace(fm.Description) == "" {
		entry.Valid = false
		entry.Reason = "missing description"
		return entry
	}
	entry.Valid = true
	return entry
}

// dirBytes 数一个目录里所有普通文件的字节之和。
// 平铺形态没有目录，entryPath 指向 <name>.md，这时数那个文件自己。
func dirBytes(dirPath string) int64 {
	var total int64
	_ = filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// parseSkillFrontmatter 解析 SKILL.md 的 YAML frontmatter。
//
// 只认 name / description / requires 三个键；未知键忽略（doc/52 §2.1）。
// 不做 YAML 全集是刻意的：多认一个字段就多一份「作者以为生效了其实没有」的风险。
//
// frontmatter 缺失 → 报错；缺 description → 报错（这个由调用方判，因为「缺 description」
// 是「坏包」而不是「解析失败」）。
func parseSkillFrontmatter(body string) (skillFrontmatter, error) {
	var fm skillFrontmatter
	// 去掉 UTF-8 BOM（作者从 Word 粘过来的技能包常常带）再认 frontmatter。
	trimmed := strings.TrimLeft(body, " \t\r\n")
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return fm, errors.New("missing frontmatter")
	}
	rest := trimmed[3:]
	rest = strings.TrimLeft(rest, "\r\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, errors.New("frontmatter 未闭合")
	}
	block := rest[:end]
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// 缩进行是上一条键的续行；readFrontmatterScalar 已经把需要的部分读走了。
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			fm.Name = unquoteSkillScalar(readFrontmatterScalar(lines, &i, value))
		case "description":
			fm.Description = unquoteSkillScalar(readFrontmatterScalar(lines, &i, value))
		case "requires":
			if value != "" {
				fm.Requires = parseRequiresValue(value)
				continue
			}
			// YAML 块序列：
			//   requires:
			//     - python3
			//     - ffmpeg
			list := []string{}
			for i+1 < len(lines) {
				next := strings.TrimSpace(strings.TrimRight(lines[i+1], "\r"))
				if !strings.HasPrefix(next, "-") {
					break
				}
				if item := strings.Trim(strings.TrimSpace(next[1:]), `"'`); item != "" {
					list = append(list, item)
				}
				i++
			}
			fm.Requires = list
		}
	}
	return fm, nil
}

// readFrontmatterScalar 读一个键的值，并把后续的缩进续行并进来。
//
// 为什么必须认续行：真实技能常把 description 写成折行（`description:` 换行后
// 缩进两格），仓库里也常见 `description: >`。只读第一行会把这些技能判成
// 「缺少 description」——一个合法技能因为用了 YAML 的合法写法而装不上。
//
// 支持三种：单行值；`>`（折行，续行用空格拼）/ `|`（保留换行）；裸值后的缩进续行
// （YAML 的 plain multi-line，同样用空格拼）。*idx 停在最后一条续行上。
func readFrontmatterScalar(lines []string, idx *int, first string) string {
	trimmed := strings.TrimSpace(first)
	blockStyle := byte(0)
	if trimmed == ">" || trimmed == "|" {
		blockStyle = trimmed[0]
	}
	if blockStyle == 0 && trimmed != "" {
		return trimmed
	}
	var parts []string
	for *idx+1 < len(lines) {
		next := strings.TrimRight(lines[*idx+1], "\r")
		if strings.TrimSpace(next) == "" {
			if blockStyle != 0 {
				*idx++
				continue
			}
			break
		}
		if !strings.HasPrefix(next, " ") && !strings.HasPrefix(next, "\t") {
			break
		}
		parts = append(parts, strings.TrimSpace(next))
		*idx++
	}
	if blockStyle == '|' {
		return strings.Join(parts, "\n")
	}
	return strings.Join(parts, " ")
}

// unquoteSkillScalar 去掉成对的首尾引号。YAML 单双引号都认，只剥一层。
func unquoteSkillScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseRequiresValue 解析 requires 的值。
//
// 两种写法都认：
//   - YAML 流式序列：`requires: [python3, ffmpeg]`（**元素不加引号**，这不是合法 JSON）
//   - 逗号分隔裸串：`requires: python3, ffmpeg`
//   - JSON 数组：`requires: ["python3", "ffmpeg"]`（合法 JSON，也认）
//
// 为什么不能只走 JSON：`[python3]` 解析失败，若直接回退成逗号分隔，就会把整串
// `[python3]` 当成一个元素存进去——表里就出现了「依赖名叫 [python3] 的程序」这种
// 永远找不到的东西。剥掉方括号再按逗号切才对。
func parseRequiresValue(value string) []string {
	value = strings.TrimSpace(value)
	out := []string{}
	if value == "" {
		return out
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		inner := value[1 : len(value)-1]
		// 先试 JSON（元素带引号的合法写法）。
		var list []string
		if err := json.Unmarshal([]byte(value), &list); err == nil {
			for _, item := range list {
				if item = strings.TrimSpace(item); item != "" {
					out = append(out, item)
				}
			}
			return out
		}
		// YAML 流式序列：剥方括号后按逗号切，每项去掉可能带的引号。
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, `"'`)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// skillScanRoot 一条扫描根：扫哪儿、归属谁。
//
// 归属两元组（scope + tenantID）就是**比对范围**：扫哪几个根，就只动那几个归属的行。
// 这不是额外规则，是 skill_rows 上 scope/tenant_id/project_id 三列存在的理由。
type skillScanRoot struct {
	Path     string
	Scope    string // system / tenant
	TenantID string // 系统行为空串；租户行为租户 id
	// EmptyMeansEmpty 为真时，根读到 0 条是正常状态（照常删本归属的行）。
	// 系统根为假——它读到 0 条只能是盘坏了或挂错了，不能当成「系统技能都没了」
	// （doc/52 §9-35 口径②）。
	EmptyMeansEmpty bool
	// MissingIsError 为真时，根读失败（不是「不存在」）→ 整次重扫中止。
	// 系统根为真：系统技能根读失败 = 盘没挂上（doc/52 §9-35 口径①）。
	MissingIsError bool
}

// rescanRoots 扫若干根，与表对账，写回行。
//
// **每个根只比对自己归属的行**：租户级重扫扫本租户根，就只动本租户的行；
// 全量重扫扫系统根 + 所有租户根，就动全部行。系统行不参与租户级比对——
// 否则租户级重扫会把出厂技能的行删掉。
//
// 幂等：同样的盘扫两次，第二次 added/updated/removed 都是空。
func (s *Store) rescanRoots(ctx context.Context, roots []skillScanRoot) (skillRescanResult, error) {
	res := skillRescanResult{Added: []string{}, Updated: []string{}, Removed: []string{}, Invalid: []invalidSkillBody{}, Roots: []skillRootInfo{}}
	for _, root := range roots {
		entries, info, err := scanSkillRoot(root.Path, root.Scope)
		if err != nil {
			// 读失败 = 我没看到盘。整次中止，**什么都不改**——
			// 半途而废的重扫会把「没看到的盘」当成「盘上没了」，删掉别人的行。
			res.Roots = append(res.Roots, info)
			return res, err
		}
		res.Roots = append(res.Roots, info)
		if !info.Exists && root.MissingIsError {
			// 系统根不存在 = 盘没挂上（它必须被部署流程同步出来）。
			return res, fmt.Errorf("系统技能根 %s 不存在", root.Path)
		}
		if info.Count == 0 && !root.EmptyMeansEmpty {
			// 系统根读到 0 条 → 只报数，**不删系统层任何行**。
			continue
		}
		if err := s.reconcileRoot(ctx, root, entries, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// reconcileRoot 把一个根扫出来的条目与该归属的表行对账。
func (s *Store) reconcileRoot(ctx context.Context, root skillScanRoot, entries []skillScanEntry, res *skillRescanResult) error {
	var existing []SkillRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND scope = ?", root.TenantID, root.Scope).
		Find(&existing).Error; err != nil {
		return err
	}
	byName := make(map[string]SkillRow, len(existing))
	for _, row := range existing {
		byName[row.Name] = row
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Name] = true
		old, ok := byName[entry.Name]
		if !ok {
			res.Added = append(res.Added, entry.Name)
			if err := s.upsertSkillRow(ctx, root.TenantID, root.Scope, entry); err != nil {
				return err
			}
			continue
		}
		if skillRowChanged(old, entry) {
			res.Updated = append(res.Updated, entry.Name)
			if err := s.upsertSkillRow(ctx, root.TenantID, root.Scope, entry); err != nil {
				return err
			}
			continue
		}
	}
	// 盘上没了 → 表里的行删掉。**只删这个归属的行**。
	for _, row := range existing {
		if !seen[row.Name] {
			res.Removed = append(res.Removed, row.Name)
			if err := s.db.WithContext(ctx).
				Where("tenant_id = ? AND name = ?", root.TenantID, row.Name).
				Delete(&SkillRow{}).Error; err != nil {
				return err
			}
		}
	}
	// invalid 收集：扫出来没通过校验的条目。
	for _, entry := range entries {
		if !entry.Valid {
			res.Invalid = append(res.Invalid, invalidSkillBody{Path: entry.Path, Reason: entry.Reason})
		}
	}
	return nil
}

// skillRowChanged 判断盘上这一条与表里那一行有没有实质变化。
// 比 description / path / bytes / requires / valid——updated_at 不算（每次扫都会变）。
func skillRowChanged(old SkillRow, entry skillScanEntry) bool {
	if old.Description != entry.Description || old.Path != entry.Path || old.Bytes != entry.Bytes {
		return true
	}
	if old.Valid != entry.Valid || old.Reason != entry.Reason {
		return true
	}
	return old.Requires != marshalSkillRequires(entry.Requires)
}

// marshalSkillRequires 把 requires 列表序列成 JSON 字符串（存表用）。
func marshalSkillRequires(list []string) string {
	if list == nil {
		list = []string{}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// upsertSkillRow 写一行技能目录。
//
// 写**失效**行时走特殊路径：生效行有部分唯一索引 (tenant_id, name) WHERE valid，
// 同名的 invalid 行可以并存——所以先删掉同名的旧行再插（rescan 把一条 invalid
// 转成生效时，也是同一套动作）。
//
// **跨桶同名要显式比**（doc/52 §4.7）：唯一索引的桶是 tenant_id，
// 「系统行 vs 某租户行」这一对跨桶，索引管不到。系统技能叫 pdf、某个租户也装了个
// pdf ——两份都在各自租户的视界里生效不冲突（系统行全集群共享，租户行归这一家），
// 但**同一个租户的视界**里不能有两个叫 pdf 的（doc/52 §4.3）。
// 所以写一行前要查：系统视界里（系统行 + 本租户行）是否已经有同名生效行。
func (s *Store) upsertSkillRow(ctx context.Context, tenantID, scope string, entry skillScanEntry) error {
	requires := marshalSkillRequires(entry.Requires)
	if entry.Valid {
		taken, err := s.nameTakenInView(ctx, tenantID, scope, entry.Name)
		if err != nil {
			return err
		}
		if taken {
			entry.Valid = false
			entry.Reason = fmt.Sprintf("name %q 在本租户视界内已被占用", entry.Name)
			requires = marshalSkillRequires(entry.Requires)
		}
	}
	if !entry.Valid {
		// 失效行：先清掉同名的旧行（不管它 valid 与否），再插一条 invalid。
		if err := s.db.WithContext(ctx).
			Where("tenant_id = ? AND name = ?", tenantID, entry.Name).
			Delete(&SkillRow{}).Error; err != nil {
			return err
		}
		return s.db.WithContext(ctx).Create(&SkillRow{
			ID:          skillRowID(tenantID, entry.Name),
			Scope:       scope,
			TenantID:    tenantID,
			ProjectID:   "",
			Name:        entry.Name,
			Path:        entry.Path,
			Description: entry.Description,
			Requires:    requires,
			Bytes:       entry.Bytes,
			Valid:       false,
			Reason:      entry.Reason,
		}).Error
	}
	// 生效行：先删同名的旧行（valid 的与 invalid 的都删），再插。
	// 为什么不走 upsert：部分唯一索引是 WHERE valid，ON CONFLICT 推不出冲突目标。
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND name = ?", tenantID, entry.Name).
		Delete(&SkillRow{}).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Create(&SkillRow{
		ID:          skillRowID(tenantID, entry.Name),
		Scope:       scope,
		TenantID:    tenantID,
		ProjectID:   "",
		Name:        entry.Name,
		Path:        entry.Path,
		Description: entry.Description,
		Requires:    requires,
		Bytes:       entry.Bytes,
		Valid:       true,
		Reason:      "",
	}).Error
}

// ListTenantIDs 列出租户 id（系统管理员的全量重扫要扫每个租户的技能根）。
func (s *Store) ListTenantIDs(ctx context.Context) ([]string, error) {
	var rows []Tenant
	if err := s.db.WithContext(ctx).
		Select("id").Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}

// skillNameTaken 报「这个名字在目标租户视界里是否已有生效行」——**包括同桶同名的自己**。
//
// 与 nameTakenInView 的分工：那个给 rescan 重写行用（同桶同名那行就是「它自己」，
// 不能判成被别人占了）；安装不是重写，同桶已有生效行就是真冲突——想换内容是 update，
// 想改名是 install + delete（doc/52 §4.3/§4.4）。
func (s *Store) skillNameTaken(ctx context.Context, tenantID, name string) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&SkillRow{}).
		Where("name = ? AND valid AND (scope = 'system' OR tenant_id = ?)", name, tenantID).
		Count(&count).Error
	return count > 0, err
}

// nameTakenInView 报「这个名字在目标租户的视界里是否已被**别的**行占用」。
//
// 视界 = 系统层 + 本租户层（doc/52 §4.3）。别的租户的行不在视界里——
// 两家各装一个 pdf 不冲突（各装各的，本来就互不可见）。
//
// 写系统行时（scope=system，tenantID 空串），视界是「系统行 + 所有租户的行」——
// 系统技能全集群共享，它的名字不能和任何一个租户装的重名（否则那个租户视界里
// 就出现了两个 pdf）。这时查全部租户桶。
//
// **同桶同名的那一行不算**（excludeTenantID/excludeScope）：rescan 重写一行时，
// 表里已经有一行同名的（就是它自己），不能说「被别人占了」——那样每次重扫
// 都会把自己判成 invalid。
//
// 「先占的生效」指的是**表里已有的生效行**，不是扫描顺序：所以这个查询读的是
// 当前表状态，全量重扫里系统根先扫也不会翻转裁决。
func (s *Store) nameTakenInView(ctx context.Context, tenantID, scope, name string) (bool, error) {
	var count int64
	q := s.db.WithContext(ctx).Model(&SkillRow{}).
		Where("name = ? AND valid", name)
	if scope == "system" {
		// 系统行：视界覆盖所有租户（系统技能对每个租户都可见）。
		q = q.Where("scope = 'system' OR tenant_id <> ''")
	} else {
		// 租户行：视界 = 系统层 + 本租户层。
		q = q.Where("scope = 'system' OR tenant_id = ?", tenantID)
	}
	// 同桶同名的那一行是「它自己」，不算被别人占。
	q = q.Where("NOT (scope = ? AND tenant_id = ?)", scope, tenantID)
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// skillRowID 造一行的主键。
//
// **主键必须带桶**：同一个桶里同一个名字只有一行（valid 的），但**系统桶与租户桶
// 可以有同名行**（系统行是全集群一份，租户行是这一家的）——主键不带租户就会撞。
// 同名的 invalid 行也要能并存（盘上有几份同名东西就记几条），所以主键还得带一点区分。
func skillRowID(tenantID, name string) string {
	return "sk_" + tenantID + "_" + name
}

// postSkillsRescan 是 POST /api/skills/rescan：以盘为准重扫。
//
// **同一个端点，两条路**（doc/52 §6.1）：
//   - 系统管理员：扫系统根 + 每个租户根，比对全部行（全局独占锁）。
//   - 租户 owner/admin：只扫本租户根、只比对本租户的行（全局共享 + 租户独占）。
//
// 权限：两条路都要求「系统管理员」或「租户 owner/admin」，其余一律 403。
// 系统管理员**不必**是某个租户的 admin——运维账号只管出厂技能的发布同步，
// 它不一定是任何租户的成员。
//
// 锁：扫盘在锁内（doc/52 §4.7）。为什么扫盘要锁：全量重扫会「删掉表里盘上没有的行」，
// 而另一个副本可能刚 rename 完目录并 upsert 了行——扫盘不锁，两者相遇就是
// 「目录里有技能、表里没有」，这个技能从目录里消失且没有任何报错。
func (s *Server) postSkillsRescan(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})

	if p.SystemAdmin {
		s.postSystemSkillsRescan(c, p, ctx)
		return
	}
	if roleRank(p.Role) < roleRank("admin") {
		writeEnvelope(c, http.StatusForbidden, codeForbidden, "没有权限做这件事", nil)
		return
	}
	root := tenantSkillsRoot(s.mediaRoot, p.TenantID)
	var res skillRescanResult
	err := s.store.withSkillLock(ctx, p.TenantID, false, func() error {
		// 重扫也是「安装/删除类写操作」的一个入口，顺手清过期垃圾（§4.6 第 4 条）。
		sweepTempDirs(root, time.Now())
		var err error
		res, err = s.store.rescanRoots(ctx, []skillScanRoot{{
			Path: root, Scope: "tenant", TenantID: p.TenantID, EmptyMeansEmpty: true,
		}})
		return err
	})
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", res)
}

// postSystemSkillsRescan 是系统管理员的全量重扫：扫系统根 + 每个租户根，比对全部行。
//
// **全局独占锁**（D27）：全量重扫是唯一能删到别的租户的行的操作，必须等所有
// 租户写做完。租户级重扫取全局共享 + 租户独占，两者互斥——
// 共享与独占必须是同一个 key，否则 exclusive 成了摆设。
func (s *Server) postSystemSkillsRescan(c *gin.Context, p principal, ctx context.Context) {
	// 租户根先扫、系统根后扫——顺序承重（doc/52 §4.7「先占的生效」）：
	// 视界内同名时，**先写进表的那一行占住名字**，后来者记 invalid。
	// 租户先扫 → 租户已有的生效行占住名字，系统行让位（§4.7 明写：
	// 「系统 rescan 写生效行前查有没有租户已经占了这个名字，命中就把系统那行记 invalid」）。
	// 反过来先扫系统根，就会让系统行每次重扫都抢走名字，把租户的行翻成 invalid——
	// 那是「扫描顺序决定裁决」，不是「先占的生效」。
	roots := []skillScanRoot{}
	tenantIDs, err := s.store.ListTenantIDs(ctx)
	if err != nil {
		internal(c, err)
		return
	}
	for _, tid := range tenantIDs {
		roots = append(roots, skillScanRoot{
			Path: tenantSkillsRoot(s.mediaRoot, tid), Scope: "tenant", TenantID: tid,
			EmptyMeansEmpty: true,
		})
	}
	roots = append(roots, skillScanRoot{
		// 系统根：读失败 → 整次中止（盘没挂上）；读到 0 条 → 只报数、不删系统层行
		// （出厂技能由发布同步保证非空，读到 0 条只能是盘坏了或挂错了）。
		Path: s.systemSkillsDir, Scope: "system", TenantID: "",
		EmptyMeansEmpty: false, MissingIsError: true,
	})
	var res skillRescanResult
	err = s.store.withSkillLock(ctx, "", true, func() error {
		// 全量重扫覆盖所有租户根与系统根：每个根的点目录各清一次。
		now := time.Now()
		for _, r := range roots {
			sweepTempDirs(r.Path, now)
		}
		var err error
		res, err = s.store.rescanRoots(ctx, roots)
		return err
	})
	if err != nil {
		internal(c, err)
		return
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", res)
}

// tenantSkillsRoot 算租户技能根：<workspace>/tenants/<tid>/skills（doc/52 §4.2）。
// 真身放租户根，项目删除时不会带走它。
func tenantSkillsRoot(workspaceRoot, tenantID string) string {
	return filepath.Join(tenantStorageRoot(workspaceRoot, tenantID), "skills")
}

// withSkillLock 在库级分布式锁下跑一段临界区（doc/52 §3 D27）。
//
// **锁放数据库、不放进程内**：多副本下进程内的互斥闸上线即失效。
// **必须是会话级锁（pg_advisory_lock），不是事务级（pg_advisory_xact_lock）**——
// xact lock 在事务提交/回滚时就释放，而扫盘不在事务里（盘与表跨两个系统）。
//
// 两级语义（D27）：
//   - 普通写（安装/更新/删除/**租户** rescan）：**全局共享** + 本租户独占。
//     共享之间不互斥 → 两个租户各扫各的盘，不用互相排队；租户独占 → 同一租户的两个
//     写操作串行，不会互踩。
//   - 全量 rescan（系统管理员）：**全局独占** → 等所有租户写做完再动表。
//
// 用两把不同 key 的锁（全局一把、每租户一把）而不是一把锁两种模式——
// 全局共享锁与全局独占锁必须是**同一个 key**，否则两者互不阻塞，exclusive 就成了摆设。
//
// **必须拿住同一条连接**：连接池换连接就等于漏锁（锁跟着连接走）。
// 这里的做法是把整个临界区塞进 gorm 的 db.Connection(...)——它钉住一条连接，
// 回调返回才还回池子。
//
// tenantID 为空表示「不取租户锁」（系统管理员的全量重扫取全局独占即可）。
func (s *Store) withSkillLock(ctx context.Context, tenantID string, globalExclusive bool, fn func() error) error {
	return s.db.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		globalKey := "tako:skill:global"
		if globalExclusive {
			if err := tx.Exec(`SELECT pg_advisory_lock(hashtext(?))`, globalKey).Error; err != nil {
				return err
			}
			defer tx.Exec(`SELECT pg_advisory_unlock(hashtext(?))`, globalKey)
		} else {
			if err := tx.Exec(`SELECT pg_advisory_lock_shared(hashtext(?))`, globalKey).Error; err != nil {
				return err
			}
			defer tx.Exec(`SELECT pg_advisory_unlock_shared(hashtext(?))`, globalKey)
		}
		if tenantID != "" {
			tenantKey := "tako:skill:tenant:" + tenantID
			if err := tx.Exec(`SELECT pg_advisory_lock(hashtext(?))`, tenantKey).Error; err != nil {
				return err
			}
			defer tx.Exec(`SELECT pg_advisory_unlock(hashtext(?))`, tenantKey)
		}
		return fn()
	})
}
