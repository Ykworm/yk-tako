// skillinstall.go 是技能的写路径（doc/52 §4.6 / §6.1 C1 拍）：安装、覆盖更新、删除。
//
// 三件事必须一起看，否则顺序会读错：
//
//  1. **校验在 staging 里做完**：包先解到 `<tenant>/skills/.staging/<rand>/`，
//     zip 穿越、软链、文件数、总字节、层级、frontmatter、正文预算全在这一层判。
//     staging 是随机目录名，写它不跟任何人冲突——所以整段解压/校验都在**锁外**做
//     （§4.6 第 5 条：锁内只做 rename 搬目录 + 写行）。
//  2. **落盘 = 两次 rename**：旧同名目录先挪进 `<tenant>/skills/.trash/<name>-<unixnano>/`，
//     再把 staging 里那个技能目录 rename 成 `<root>/<name>`。中间没有「先删后建」的窗口：
//     读侧要么看见旧的、要么看见新的（正在读的 fd 也不受 rename 影响）。
//  3. **行在盘落定之后写**：rename 成功才 upsert `skill_rows`。崩在两者之间留下「目录在、行不在」，
//     这个技能暂时不进目录——安装入口遇到盘上有同名目录但没有行时**认领**它（补行），
//     `rescan` 也能补（§4.6 第 8 条）。反过来「行在、盘不在」由 rescan 清。
//
// **同名判定用视界**（§4.3）：本租户视界 = 系统层 + 本租户层。撞系统技能 → 116106（系统技能只读）；
// 撞本租户已有 → 116106；别的租户用同名不算冲突（各装各的）。并发同名不靠文件系统锁：
// 两个请求都在租户独占锁里，后到的先看到行已存在，回 116106——一个 201 一个 409（§4.6 第 6 条）。
package runengine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 安装包上限（doc/52 §9-10）：单技能 ≤10MB / ≤200 文件 / ≤5 层。
// 正文另有 16,384 token / 64KB 的预算（skillBodyMaxTokens / skillBodyMaxBytes，见 skilltools.go）。
const (
	skillPkgMaxBytes = 10 << 20
	skillPkgMaxFiles = 200
	skillPkgMaxDepth = 5
	// 仓库归档（C2）比一个技能包大得多：整仓可能有几十个技能、README、CI 配置。
	// 下载上限按 §6.5：≤50MB；解压后 ≤200MB / ≤20000 条目。
	// 层级放宽到 12：GitHub tarball 自带 `owner-repo-sha/` 前缀，再叠 `skills/x/` 就吃掉 3 层，
	// 真正的技能目录仍在 ≤5 层（findSkillCandidates 之后按单技能再核一次）。
	skillArchiveMaxBytes = 50 << 20
	skillArchiveMaxFiles = 20000
	skillArchiveMaxDepth = 12
	skillArchiveUnpacked = 200 << 20
	// skillDescriptionMaxRunes 是目录里 description 的长度上限（§6.2 的 116103）。
	// 目录是每个 run 都拼进 system prompt 的，描述不设上限等于让一个技能把别人的目录挤掉。
	skillDescriptionMaxRunes = 1024
)

// pkgLimits 是解包器的三个上限。上传包与仓库归档用两套值：
// 上传包就是给人装单个技能用的，10MB/200 文件已经足够；仓库归档是来源解析的中间物，
// 整仓必须能解开，单技能的 10MB/200 文件由 checkCandidateBudget 另外核。
type pkgLimits struct {
	maxBytes int64
	maxFiles int
	maxDepth int
	// skipSpecial 决定遇到软链/特殊文件是拒还是跳过。
	// 上传包拒绝（116105）：包里出现软链几乎总是 zip slip 的写法；
	// 仓库归档跳过：GitHub 上的仓库常把 CLAUDE.md 之类做成软链，拒绝会让整仓装不上，
	// 而跳过不会把链接带进 staging——读侧看到的只是「这个文件不存在」。
	skipSpecial bool
}

var (
	skillUploadLimits = pkgLimits{maxBytes: skillPkgMaxBytes, maxFiles: skillPkgMaxFiles, maxDepth: skillPkgMaxDepth}
	skillRepoLimits   = pkgLimits{maxBytes: skillArchiveUnpacked, maxFiles: skillArchiveMaxFiles, maxDepth: skillArchiveMaxDepth, skipSpecial: true}
)

// skillWriteErr 是写路径的结构化失败：HTTP 状态 + 业务码 + 弹窗文案。
//
// 为什么不用 fmt.Errorf 拼字符串再在上层猜码：安装有十来种失败，前端要靠码分支
// （「同名冲突，要不要覆盖？」与「包坏了」是两种交互），猜不出来。
type skillWriteErr struct {
	status int
	code   int
	msg    string
	// data 是可选的失败附带内容。116108（多候选未选择）要带候选清单：
	// 光一个码前端只能提示「请选一个」，选谁得靠 data.skills（§6.5）。
	data any
}

func (e *skillWriteErr) Error() string { return e.msg }

// skillWriteError 造一个写路径失败。
func skillWriteError(status, code int, msg string) *skillWriteErr {
	return &skillWriteErr{status: status, code: code, msg: msg}
}

// skillWriteErrorData 是带 data 的失败（目前只用于 116108 的候选清单）。
func skillWriteErrorData(status, code int, msg string, data any) *skillWriteErr {
	return &skillWriteErr{status: status, code: code, msg: msg, data: data}
}

// skillInstallEntry 是 data.installed[] 的一项（§6.1）。
// dependencies 是 frontmatter 的 requires 在这一刻的检查结果（D1 起）：与
// GET /api/skills/:name/check 同一个条目形状 {name,found,path?}——安装时就把
// 「这台机器缺什么」告诉人，缺了也照装（§7 第 2 条：缺依赖不阻断）。
type skillInstallEntry struct {
	Name         string            `json:"name"`
	Dependencies []skillDependency `json:"dependencies"`
	Warnings     []string          `json:"warnings"`
}

// skillInstallData 是 install / update 的 data。键序 installed → warnings 是契约。
type skillInstallData struct {
	Installed []skillInstallEntry `json:"installed"`
	Warnings  []string            `json:"warnings"`
}

// skillDeleteData 是 DELETE /api/skills/:name 的 data。
type skillDeleteData struct {
	Status string `json:"status"`
}

// pkgBudget 数一份包里的文件与字节，同时挡重复路径。
//
// 重复路径为什么要拒（116105）：zip 里两条 `a/SKILL.md` 是两种内容，解出来只剩后写的那个——
// 作者以为装的是第一份，实际生效的是第二份。不报错的歧义就是没人能复核的歧义。
type pkgBudget struct {
	limits pkgLimits
	files  int
	bytes  int64
	seen   map[string]bool
}

func newPkgBudget(limits pkgLimits) *pkgBudget {
	return &pkgBudget{limits: limits, seen: map[string]bool{}}
}

// add 记一条文件条目。超文件数 / 超总字节 → 116113；重复路径 → 116105。
func (b *pkgBudget) add(name string, size int64) error {
	if b.seen[name] {
		return skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
			fmt.Sprintf("安装包里有重复路径 %s", name))
	}
	b.seen[name] = true
	b.files++
	b.bytes += size
	if b.files > b.limits.maxFiles {
		return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包文件数超过上限（%d 个）", b.limits.maxFiles))
	}
	if b.bytes > b.limits.maxBytes {
		return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包解压后超过上限（%d 字节）", b.limits.maxBytes))
	}
	return nil
}

// skillPkgEntryPath 把归档里的条目名归一成相对路径，越界与绝对路径一律拒。
//
// 归一规则（zip slip 的正面清单）：反斜杠按斜杠处理（Windows 打的包）、去掉前导 `./`、
// 任何一段是 `..` 就拒、`path.Clean` 之后再查一遍前缀。层级按斜杠数算。
func skillPkgEntryPath(raw string, limits pkgLimits) (string, error) {
	p := strings.ReplaceAll(raw, "\\", "/")
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
			fmt.Sprintf("安装包里有绝对路径 %q", raw))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
				fmt.Sprintf("安装包里有越界路径 %q", raw))
		}
	}
	clean := path.Clean(p)
	if clean == "." || clean == "" {
		return "", nil
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return "", skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
			fmt.Sprintf("安装包里有越界路径 %q", raw))
	}
	if strings.Count(clean, "/") > limits.maxDepth {
		return "", skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包目录层级超过上限（%d 层）", limits.maxDepth))
	}
	return clean, nil
}

// extractSkillPackage 把 zip / tar.gz 解到 dest（staging 目录）。
//
// 判格式靠魔数不靠扩展名：上传者给的名字不可信，而「解不开」与「解出半个东西」
// 是两种后果——前者报错，后者是坏包进正式目录。软链与其它特殊类型一律拒（116105）：
// 软链是 zip slip 的另一种写法（链到 /etc 再写穿过去）。
func extractSkillPackage(data []byte, dest string, limits pkgLimits) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")):
		return extractZipPackage(data, dest, limits)
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return extractTarGzPackage(data, dest, limits)
	default:
		return skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
			"安装包既不是 zip 也不是 tar.gz")
	}
}

// extractZipPackage 解 zip。目录条目照建，普通文件逐个写，其余类型（软链、设备）拒绝。
func extractZipPackage(data []byte, dest string, limits pkgLimits) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return skillWriteError(http.StatusBadRequest, codeSkillPackageBad, "zip 解不开："+err.Error())
	}
	budget := newPkgBudget(limits)
	for _, f := range zr.File {
		rel, perr := skillPkgEntryPath(f.Name, limits)
		if perr != nil {
			return perr
		}
		if rel == "" {
			continue
		}
		mode := f.Mode()
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if mode&os.ModeSymlink != 0 {
			if limits.skipSpecial {
				continue
			}
			return skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
				fmt.Sprintf("安装包里有符号链接 %q", f.Name))
		}
		if mode.IsDir() || strings.HasSuffix(f.Name, "/") {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			if limits.skipSpecial {
				continue
			}
			return skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
				fmt.Sprintf("安装包里有非普通文件 %q", f.Name))
		}
		if err := budget.add(rel, int64(f.UncompressedSize64)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return skillWriteError(http.StatusBadRequest, codeSkillPackageBad, "zip 条目打不开："+err.Error())
		}
		werr := writePkgFile(target, rc, limits.maxBytes)
		_ = rc.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

// extractTarGzPackage 解 tar.gz。只认目录与普通文件；硬链/软链/设备一律拒。
func extractTarGzPackage(data []byte, dest string, limits pkgLimits) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return skillWriteError(http.StatusBadRequest, codeSkillPackageBad, "tar.gz 解不开："+err.Error())
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	budget := newPkgBudget(limits)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return skillWriteError(http.StatusBadRequest, codeSkillPackageBad, "tar.gz 解不开："+err.Error())
		}
		rel, perr := skillPkgEntryPath(hdr.Name, limits)
		if perr != nil {
			return perr
		}
		if rel == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			// PAX 全局头是归档元数据（GitHub 的 codeload tarball 第一条就是
			// `pax_global_header`），不是文件；Go 的 tar 已经把 PAX 扩展并入下一条，
			// 这两类只是该并入却没并的部分。当成「非普通文件」拒绝会让正常仓库装不上。
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := budget.add(rel, hdr.Size); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if werr := writePkgFile(target, tr, limits.maxBytes); werr != nil {
				return werr
			}
		default:
			if limits.skipSpecial {
				continue
			}
			return skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
				fmt.Sprintf("安装包里有非普通文件 %q", hdr.Name))
		}
	}
}

// writePkgFile 写一个解包条目，写入量再按总预算兜一次底。
//
// 为什么声明的大小之外还要兜：zip 头里的 uncompressed size 是作者写的，可以撒谎
// （实际流比声明的长）。预算按实际写入算，才不会出现「头里 1KB、解出 1GB」。
func writePkgFile(target string, r io.Reader, maxBytes int64) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n, cerr := io.Copy(f, io.LimitReader(r, maxBytes+1))
	if err := f.Close(); err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if n > maxBytes {
		return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包解压后超过上限（%d 字节）", maxBytes))
	}
	return nil
}

// skillPkgCandidate 是包里找到的一个候选技能（还没落盘）。
//
// RelDir 是目录形态在 staging 里的相对路径（rename 的源）；平铺形态为空（平铺包一律落成目录形态，
// 源文件是 RelFile）。
type skillPkgCandidate struct {
	Name        string
	RelDir      string
	RelFile     string
	Description string
	Requires    []string
	// Files / Bytes 是**整个技能目录**的文件数与普通文件字节和（平铺形态就是那一个文件）。
	// 初值来自入口文件（validateSkillCandidate），checkCandidateBudget 走盘时补成目录口径——
	// 与列表/详情页的 bytes 同一个数，预览卡上「字节」和人装完看到的才对得上。
	Files    int
	Bytes    int64
	Body     string
	Warnings []string
}

// findSkillCandidates 在 staging 里找候选技能（doc/52 §6.5「从归档里找技能」）。
//
// 候选 = 所有**含 SKILL.md 的目录**（任意深度）；平铺 `.md` 只在「整包里只有一个 .md」时认
// （上传包没有仓库上下文，多出来的 .md 可能是 README——猜错就是把 README 装成技能）。
//
// 名字取 frontmatter `name`，必须等于目录名（目录形态）或文件名去掉 `.md`（平铺形态）；
// 这一条与 rescan 同源（scanSkillDir/scanSkillFlat），否则「装进去的」与「重扫出来的」会打架。
func findSkillCandidates(staging string, counter TokenCounter, allowFlat, allowNameMismatch bool) ([]skillPkgCandidate, error) {
	var cands []skillPkgCandidate
	var mdFiles []string
	err := filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `.` 开头的目录（.git、.staging 残留）不是技能的一部分；跳过能省掉
			// 「包里带了 .git 就是 200 个文件超限」这种假失败。
			if p != staging && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if d.Name() == "SKILL.md" {
			relDir, err := filepath.Rel(staging, filepath.Dir(p))
			if err != nil {
				return err
			}
			relFile, err := filepath.Rel(staging, p)
			if err != nil {
				return err
			}
			cand, cerr := validateSkillCandidate(p, filepath.Base(filepath.Dir(p)), relDir, relFile, counter, allowNameMismatch)
			if cerr != nil {
				return cerr
			}
			cands = append(cands, cand)
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") {
			mdFiles = append(mdFiles, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		if !allowFlat || len(mdFiles) != 1 {
			return nil, skillWriteError(http.StatusBadRequest, codeSkillPackageEmpty,
				"包里没找到技能（没有含 SKILL.md 的目录）")
		}
		flat := mdFiles[0]
		relFile, err := filepath.Rel(staging, flat)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(flat), ".md")
		cand, cerr := validateSkillCandidate(flat, name, "", relFile, counter, allowNameMismatch)
		if cerr != nil {
			return nil, cerr
		}
		cands = append(cands, cand)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Name < cands[j].Name })
	return cands, nil
}

// validateSkillCandidate 读一个候选的 frontmatter 与正文，跑完全部安装期校验。
//
// 码的分配（§6.2）：frontmatter 坏 → 116104；名字坏/与目录名不一致 → 116101；
// 缺 description → 116102；description 超 1024 字符 → 116103；
// 正文超 16,384 token 或 64KB → 116116。
func validateSkillCandidate(filePath, baseName, relDir, relFile string, counter TokenCounter, allowNameMismatch bool) (skillPkgCandidate, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return skillPkgCandidate{}, err
	}
	fm, ferr := parseSkillFrontmatter(string(raw))
	if ferr != nil {
		return skillPkgCandidate{}, skillWriteError(http.StatusBadRequest, codeSkillFrontmatterBad, ferr.Error())
	}
	name := baseName
	if fm.Name != "" && fm.Name != baseName {
		// 上传包：目录名就是作者给技能起的名字，不一致说明包结构错了（116101）。
		// 仓库归档：真实仓库常用带前缀的 frontmatter 名（`skills/composition-patterns/`
		// 里写 `name: vercel-composition-patterns`），这时以 frontmatter 为准，
		// 装成 `<root>/<frontmatter name>`——装完目录名与名字一致，rescan 不会判它 invalid。
		if !allowNameMismatch {
			return skillPkgCandidate{}, skillWriteError(http.StatusBadRequest, codeSkillNameBad,
				fmt.Sprintf("name %q 不等于目录名 %q", fm.Name, baseName))
		}
	}
	if fm.Name != "" {
		name = fm.Name
	}
	if !skillNamePattern.MatchString(name) {
		return skillPkgCandidate{}, skillWriteError(http.StatusBadRequest, codeSkillNameBad,
			fmt.Sprintf("技能名 %q 不合法（只能用小写字母、数字、连字符，最长 64）", name))
	}
	if strings.TrimSpace(fm.Description) == "" {
		return skillPkgCandidate{}, skillWriteError(http.StatusBadRequest, codeSkillNoDescription,
			"技能缺少 description（它是目录里「什么时候用我」的判据，不能省）")
	}
	if n := len([]rune(fm.Description)); n > skillDescriptionMaxRunes {
		return skillPkgCandidate{}, skillWriteError(http.StatusBadRequest, codeSkillDescriptionTooLong,
			fmt.Sprintf("description 超过 %d 字符", skillDescriptionMaxRunes))
	}
	if len(raw) > skillBodyMaxBytes || counter.Count(string(raw)) > skillBodyMaxTokens {
		return skillPkgCandidate{}, skillWriteError(http.StatusRequestEntityTooLarge, codeSkillBodyTooLarge,
			"技能正文超过上下文预算（16,384 token 或 64KB）")
	}
	cand := skillPkgCandidate{
		Name:        name,
		RelDir:      relDir,
		RelFile:     relFile,
		Description: fm.Description,
		Requires:    fm.Requires,
		Bytes:       int64(len(raw)),
		Body:        string(raw),
	}
	if fm.Name != "" && fm.Name != baseName {
		cand.Warnings = append(cand.Warnings,
			fmt.Sprintf("技能 %q 的目录名是 %q，按 frontmatter 的名字安装", fm.Name, baseName))
	}
	return cand, nil
}

// selectCandidates 按 form 里的选择挑要装的候选（doc/52 §6.5「选择」）。
//
// 选择为空：只有一个候选就装它；多个 → 116108（必须选）。
// `all=true` 全装；`skills` 支持尾 `*` 通配（与 `--skill` 同义）。选了但一个都没匹配 → 116111。
func selectCandidates(cands []skillPkgCandidate, skills []string, all bool) ([]skillPkgCandidate, error) {
	if all {
		return cands, nil
	}
	if len(skills) == 0 {
		if len(cands) == 1 {
			return cands, nil
		}
		return nil, skillWriteErrorData(http.StatusBadRequest, codeSkillAmbiguous,
			fmt.Sprintf("来源里有 %d 个技能，请指定要装哪些（skills 或 all）", len(cands)),
			map[string]any{"skills": previewEntries(cands)})
	}
	var out []skillPkgCandidate
	for _, cand := range cands {
		if skillSelected(skills, cand.Name) {
			out = append(out, cand)
		}
	}
	if len(out) == 0 {
		return nil, skillWriteError(http.StatusBadRequest, codeSkillPackageEmpty,
			"选中的技能不在这个包里")
	}
	return out, nil
}

// skillTrashPath 造一个不撞的回收目录：`.trash/<name>-<unixnano>/`（§4.6 第 2 条）。
// 回收而不是直接删：rename 是原子的、rm -rf 不是；「装错了立刻还能捞回来」也靠它。
func skillTrashPath(root, name string) string {
	return filepath.Join(root, ".trash", fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
}

// moveAsideToTrash 把技能根下的同名片先挪进回收目录。
//
// 目录形态与平铺形态都挪：旧技能可能是手工拷进来的 `<name>.md`，只挪目录会留下
// 「新目录装好了、旧平铺文件还在」——两者同名，rescan 会按路径序把其中一个判成 invalid。
// 返回是否真的挪了东西（没有旧东西时什么都不建，免得垃圾目录堆积）。
func moveAsideToTrash(root, name string) (bool, error) {
	dirPath := filepath.Join(root, name)
	flatPath := filepath.Join(root, name+".md")
	var targets []string
	if _, err := os.Lstat(dirPath); err == nil {
		targets = append(targets, dirPath)
	}
	if _, err := os.Lstat(flatPath); err == nil {
		targets = append(targets, flatPath)
	}
	if len(targets) == 0 {
		return false, nil
	}
	trash := skillTrashPath(root, name)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return false, err
	}
	for _, target := range targets {
		if err := os.Rename(target, filepath.Join(trash, filepath.Base(target))); err != nil {
			return false, err
		}
	}
	return true, nil
}

// installCandidates 在租户独占锁内把选中的候选落到技能根并写行（§4.6 第 2/5/8 条）。
//
// 锁内只做三件事：查冲突、rename 搬目录、upsert 行。解压与校验收到的候选已经是内存里的
// 成品（staging 里），这里不再碰网络与压缩。
//
// replace = update：同名表行就是**这次要换掉的那一行**，所以冲突判定排除同桶自己
// （nameTakenInView）。install 则相反——同桶已有生效行就是真冲突，想换内容走 update。
func (s *Server) installCandidates(ctx context.Context, tenantID, staging string, chosen []skillPkgCandidate, replace bool) ([]skillInstallEntry, error) {
	return installSkillCandidates(ctx, s.store, s.mediaRoot, tenantID, staging, chosen, replace)
}

// installSkillCandidates 是真身：不依赖 *Server，因为对话内安装的收尾（doc/52 C3）
// 可能发生在续跑对账里——那里只有 Store 与 workspace 根。
func installSkillCandidates(ctx context.Context, store *Store, mediaRoot, tenantID, staging string, chosen []skillPkgCandidate, replace bool) ([]skillInstallEntry, error) {
	var installed []skillInstallEntry
	err := store.withSkillLock(ctx, tenantID, false, func() error {
		root := tenantSkillsRoot(mediaRoot, tenantID)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		// 顺手清过期垃圾（§4.6 第 4 条）：清扫只在已有锁内发生，不在启动时扫盘。
		// 本次要 rename 走的那份 staging 豁免——对话内安装的提议可能挂起超过 24 小时，
		// 把它当垃圾删掉，下一步 rename 就 ENOENT，用户看到裸 I/O 错误而不是「安装已过期」。
		sweepTempDirsKeeping(root, time.Now(), stagingKeep(root, staging))
		for _, cand := range chosen {
			// 冲突检查必须在锁内：锁外查到「没有」到 rename 之间，另一个并发安装可能已经写行。
			var (
				taken bool
				err   error
			)
			if replace {
				taken, err = store.nameTakenInView(ctx, tenantID, "tenant", cand.Name)
			} else {
				taken, err = store.skillNameTaken(ctx, tenantID, cand.Name)
			}
			if err != nil {
				return err
			}
			if taken {
				return skillWriteError(http.StatusConflict, codeSkillConflict,
					fmt.Sprintf("本租户视界内已有同名技能 %q（含出厂技能）", cand.Name))
			}
			if _, err := moveAsideToTrash(root, cand.Name); err != nil {
				return err
			}
			destDir := filepath.Join(root, cand.Name)
			src := filepath.Join(staging, filepath.FromSlash(cand.RelDir))
			if cand.RelDir == "" {
				// 平铺形态：包里只有一个 .md。落成标准目录形态——装进来的技能一律带资源目录
				// 的资格，这份 .md 就是它的 SKILL.md。
				if err := os.MkdirAll(destDir, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(destDir, "SKILL.md"), []byte(cand.Body), 0o644); err != nil {
					return err
				}
			} else if err := os.Rename(src, destDir); err != nil {
				return err
			}
			entry := skillScanEntry{
				Name:        cand.Name,
				Path:        filepath.Join(destDir, "SKILL.md"),
				Description: cand.Description,
				Requires:    cand.Requires,
				Bytes:       dirBytes(destDir),
				Valid:       true,
			}
			if err := store.upsertSkillRow(ctx, tenantID, "tenant", entry); err != nil {
				return err
			}
			installed = append(installed, skillInstallEntry{
				Name:         cand.Name,
				Dependencies: skillDependencies(cand.Requires, tenantStorageRoot(mediaRoot, tenantID)),
				Warnings:     []string{},
			})
		}
		return nil
	})
	return installed, err
}

// installFromMultipart 是 install 与 update 的共用体：读包 → staging 解包 → 找候选 → 选择 →
// 锁内落盘。target 非空 = update（包里的技能名必须等于 target）。
//
// **失败时的形状**：staging 目录由 defer 删掉，正式目录一个字节没动（§4.6 第 3 条）。
// 多技能安装是逐个落盘的，中途失败已装上的那些留在盘上（不做跨技能回滚——回滚要
// 记住每个技能的旧目录，收益不抵复杂度；重试会把剩下的装完，同名的那几个会回 116106，人看得见）。
func (s *Server) installFromMultipart(c *gin.Context, p principal, target string) {
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	data, err := readSkillUpload(c)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}

	root := tenantSkillsRoot(s.mediaRoot, p.TenantID)
	stageRoot := filepath.Join(root, ".staging")
	stagingID, err := newRowID("")
	if err != nil {
		internal(c, err)
		return
	}
	staging := filepath.Join(stageRoot, stagingID)
	// 解压与校验在**锁外**（§4.6 第 5 条）：慢包的压缩时间不该占着租户锁。
	// staging 是随机目录名，别人不会写到同一个地方。
	defer func() { _ = os.RemoveAll(staging) }()
	if err := extractSkillPackage(data, staging, skillUploadLimits); err != nil {
		writeSkillWriteError(c, err)
		return
	}
	counter, err := NewCl100kCounter()
	if err != nil {
		internal(c, err)
		return
	}
	cands, err := findSkillCandidates(staging, counter, true, false)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	if target != "" {
		var keep []skillPkgCandidate
		for _, cand := range cands {
			if cand.Name == target {
				keep = append(keep, cand)
			}
		}
		if len(keep) == 0 {
			writeEnvelope(c, http.StatusBadRequest, codeSkillPackageEmpty,
				fmt.Sprintf("包里没有名为 %q 的技能", target), nil)
			return
		}
		cands = keep
	}
	chosen, err := selectCandidates(cands, c.PostFormArray("skills"), formBool(c, "all"))
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	if target != "" {
		// update 只覆盖**本租户已有**的技能：系统技能只读（116106），别人的看不见（116001）。
		// 在这一步先判一次是给一个干净的错误；锁内还会再按同一口径查一遍（并发窗口）。
		row, found, rerr := s.store.GetSkill(ctx, p.TenantID, target)
		if rerr != nil {
			internal(c, rerr)
			return
		}
		if !found {
			writeEnvelope(c, http.StatusNotFound, codeSkillMissing, "找不到这个技能", nil)
			return
		}
		if row.Scope != "tenant" {
			writeEnvelope(c, http.StatusConflict, codeSkillConflict, "系统技能只读，请复制到团队层再改", nil)
			return
		}
	}
	installed, err := s.installCandidates(ctx, p.TenantID, staging, chosen, target != "")
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	status := http.StatusCreated
	if target != "" {
		status = http.StatusOK
	}
	s.publishSkillEvents(c.Request.Context(), p.TenantID, installed)
	writeEnvelope(c, status, codeOK, "", skillInstallData{Installed: installed, Warnings: []string{}})
}

// readSkillUpload 读 multipart 里的 file=*.zip / *.tar.gz，最多 10MB。
//
// 限额挂在请求体上：多 1MB 的 multipart 头余量，超了在 ParseMultipartForm 就报错，
// 不把 10MB 以外的字节读进内存。预览与安装共用这一段（两边的「包多大」必须同口径）。
func readSkillUpload(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, skillPkgMaxBytes+(1<<20))
	fileHeader, err := c.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			return nil, skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
				fmt.Sprintf("安装包超过上限（%d 字节）", skillPkgMaxBytes))
		}
		return nil, skillWriteError(http.StatusBadRequest, codeSkillPackageBad,
			"请用 multipart 上传 file=*.zip 或 *.tar.gz")
	}
	if fileHeader.Size > skillPkgMaxBytes {
		return nil, skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包超过上限（%d 字节）", skillPkgMaxBytes))
	}
	f, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, skillPkgMaxBytes+1))
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > skillPkgMaxBytes {
		return nil, skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("安装包超过上限（%d 字节）", skillPkgMaxBytes))
	}
	return data, nil
}

// formBool 读一个形如 `all=true` 的表单布尔。认 true/1/yes，其余算 false——
// 表单是松散输入，写「all=on」的人期待的是「开」，不是 400。
func formBool(c *gin.Context, key string) bool {
	switch strings.ToLower(strings.TrimSpace(c.PostForm(key))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// writeSkillWriteError 把写路径失败翻成信封。非 skillWriteErr（I/O、rename 失败）走 500：
// 那是盘/库的故障，不是用户输入的问题，给业务码只会让前端以为可以改输入重试。
func writeSkillWriteError(c *gin.Context, err error) {
	var werr *skillWriteErr
	if errors.As(err, &werr) {
		writeEnvelope(c, werr.status, werr.code, werr.msg, werr.data)
		return
	}
	internal(c, err)
}

// postSkillsInstall 是 POST /api/skills/install（§6.1 / §6.5）：三种输入共用这一个端点。
//
// multipart = 上传包；JSON = {source} 或 {command}。Content-Type 是可靠的分流依据
// （curl -F 与前端 FormData 都自动带 multipart/form-data），不靠「body 里有没有 file」猜。
func (s *Server) postSkillsInstall(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		s.installFromMultipart(c, p, "")
		return
	}
	s.installFromSource(c, p)
}

// postSkillUpdate 是 POST /api/skills/:name/update：覆盖更新本租户的一个技能。
func (s *Server) postSkillUpdate(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	s.installFromMultipart(c, p, c.Param("name"))
}

// postSkillDelete 是 DELETE /api/skills/:name：删本租户的技能（§4.5）。
//
// 顺序：**先 rename 再删行**。rename 之后这个技能立刻从目录里消失（下一次 run 起手看不到），
// 行删失败也只剩一条指向 .trash 的行——rescan 会把它清掉。反过来先删行，rename 失败就
// 留下「盘上有、表里没有」：技能还能被人手工找回来，但 rescan 会把它当新技能装回目录
// （它确实还在盘上），用户看到的是「删了又回来了」。
//
// 删行与删禁用项在**同一笔事务**里（§4.5）：否则「技能删了、禁用项还留着」会让下一次
// 同名重装永远不出现在目录里。
func (s *Server) postSkillDelete(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	name := c.Param("name")
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	err := s.store.withSkillLock(ctx, p.TenantID, false, func() error {
		row, found, err := s.store.GetSkill(ctx, p.TenantID, name)
		if err != nil {
			return err
		}
		if !found {
			return skillWriteError(http.StatusNotFound, codeSkillMissing, "找不到这个技能")
		}
		if row.Scope != "tenant" {
			return skillWriteError(http.StatusConflict, codeSkillConflict, "系统技能只读，不能删")
		}
		root := tenantSkillsRoot(s.mediaRoot, p.TenantID)
		sweepTempDirs(root, time.Now())
		if _, err := moveAsideToTrash(root, name); err != nil {
			return err
		}
		return s.store.deleteSkillAndDisable(ctx, p.TenantID, name)
	})
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	s.publishEvent(c.Request.Context(), p.TenantID, eventSkillsChanged, name)
	writeEnvelope(c, http.StatusOK, codeOK, "", skillDeleteData{Status: "deleted"})
}

// publishSkillEvents 给一批装好的技能各发一条 skills_changed（doc/52 D24）。
// 事件是优化不是真源：目录的真相永远在 GET /api/skills，丢了只是别人晚一拍重拉。
func (s *Server) publishSkillEvents(ctx context.Context, tenantID string, installed []skillInstallEntry) {
	for _, e := range installed {
		s.publishEvent(ctx, tenantID, eventSkillsChanged, e.Name)
	}
}

// deleteSkillAndDisable 删一行技能行与它在本租户的禁用项（同一笔事务，§4.5）。
func (s *Store) deleteSkillAndDisable(ctx context.Context, tenantID, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tenant_id = ? AND name = ?", tenantID, name).Delete(&SkillRow{}).Error; err != nil {
			return err
		}
		return tx.Where("tenant_id = ? AND name = ?", tenantID, name).Delete(&SkillDisable{}).Error
	})
}
