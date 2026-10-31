// skillresolve.go 把「一条来源」走完预览/安装共用的前半段（doc/52 §6.5 C2 拍）：
// 抓取 → 落到 staging → 解包 → 找候选 → 按子目录过滤 → 单技能预算复核。
// 预览在拿到候选后就停（不碰技能根、不写行）；安装接着做选择与落盘。
//
// 为什么预览也落 staging：解包与校验要看见真实文件（frontmatter、正文预算、
// 文件数），不可能只靠 HTTP 头判断。staging 是随机目录，预览结束立刻删；
// 崩在中间由 G1 的 24h 清扫兜底。这仍然满足「预览不写正式目录」——
// 失败/放弃时技能根一个字节没动。
package runengine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// skillRequestInput 是 preview / install 的 JSON 入参（§6.5 三种输入的两种）。
// 上传包走 multipart，不进这个结构。
type skillRequestInput struct {
	Source  string   `json:"source"`
	Command string   `json:"command"`
	Skills  []string `json:"skills"`
	All     bool     `json:"all"`
}

// skillPreviewSource 是 data.source 的形状（键序固定）。
// 不适用的字段留空串而不是省略：前端一张卡片按固定键取值，少一个键就多一处判空。
type skillPreviewSource struct {
	Kind      string `json:"kind"`
	Owner     string `json:"owner"`
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`
	Ref       string `json:"ref"`
	Subpath   string `json:"subpath"`
	URL       string `json:"url"`
}

// skillPreviewEntry 是预览卡上的一行候选（§6.5）。键序 name → description →
// bytes → files → requires 是契约：bytes 是整个技能目录的字节和，files 是目录里的普通文件数
// （C5 起给，卡片上「多大、多少文件」两件事都要能核对，不看模型转述）。
type skillPreviewEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Bytes       int64    `json:"bytes"`
	Files       int      `json:"files"`
	Requires    []string `json:"requires"`
}

// skillPreviewData 是 POST /api/skills/preview 的 data。键序 source → skills →
// selected → ambiguous → warnings 是契约。
//
// selected 是「按这份输入会装哪几个」（C5）：预览卡靠它标出选中集，用户再勾选改动。
// 没给选择（多候选）时是空数组而不是 null——前端按数组画，少一个键就多一处判空。
type skillPreviewData struct {
	Source    skillPreviewSource  `json:"source"`
	Skills    []skillPreviewEntry `json:"skills"`
	Selected  []string            `json:"selected"`
	Ambiguous bool                `json:"ambiguous"`
	Warnings  []string            `json:"warnings"`
}

// resolvedSkillSource 是预览/安装共用前半段的产出。staging 由调用方负责删除。
type resolvedSkillSource struct {
	spec       skillSourceSpec
	archiveURL string
	staging    string
	cands      []skillPkgCandidate
	warnings   []string
}

// newSkillStaging 在 `<tenant>/skills/.staging/<rand>/` 建一个解包目录。
func newSkillStaging(mediaRoot, tenantID string) (string, error) {
	root := tenantSkillsRoot(mediaRoot, tenantID)
	id, err := newRowID("")
	if err != nil {
		return "", err
	}
	staging := filepath.Join(root, ".staging", id)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	return staging, nil
}

// resolveSkillSource 抓取并解析一个来源的候选技能。不做选择、不落正式目录。
//
// allowFlat=false：从仓库/直链来的包**不猜平铺 .md**——仓里的 README 也是 .md，
// 猜错就是把 README 装成技能（§6.5）。上传包才允许「整包恰好一个 .md」。
func resolveSkillSource(ctx context.Context, mediaRoot, tenantID string, spec skillSourceSpec) (*resolvedSkillSource, error) {
	archiveURL, err := skillArchiveURL(ctx, spec)
	if err != nil {
		return nil, err
	}
	data, err := getSkillURL(ctx, archiveURL, skillArchiveMaxBytes)
	if err != nil {
		return nil, err
	}
	staging, err := newSkillStaging(mediaRoot, tenantID)
	if err != nil {
		return nil, err
	}
	res := &resolvedSkillSource{
		spec:       spec,
		archiveURL: archiveURL,
		staging:    staging,
		warnings:   append([]string{}, spec.Warnings...),
	}
	// 从这里起的失败都要把 staging 删掉；成功时调用方 defer。
	fail := func(err error) (*resolvedSkillSource, error) {
		_ = os.RemoveAll(staging)
		return nil, err
	}
	if err := extractSkillPackage(data, staging, skillRepoLimits); err != nil {
		return fail(err)
	}
	counter, err := NewCl100kCounter()
	if err != nil {
		return fail(err)
	}
	cands, err := findSkillCandidates(staging, counter, false, true)
	if err != nil {
		return fail(err)
	}
	if spec.Subpath != "" {
		cands = filterCandidatesBySubpath(cands, spec.Subpath)
		if len(cands) == 0 {
			return fail(skillWriteError(http.StatusBadRequest, codeSkillPackageEmpty,
				fmt.Sprintf("来源的 %q 子目录里没有含 SKILL.md 的技能", spec.Subpath)))
		}
	}
	seen := map[string]bool{}
	for i := range cands {
		cand := &cands[i]
		if seen[cand.Name] {
			return fail(skillWriteError(http.StatusConflict, codeSkillConflict,
				fmt.Sprintf("来源里有两个技能都叫 %q，请只选一个（skills）", cand.Name)))
		}
		seen[cand.Name] = true
		if err := checkCandidateBudget(staging, cand); err != nil {
			return fail(err)
		}
	}
	res.cands = cands
	if len(cands) > 1 {
		res.warnings = append(res.warnings,
			fmt.Sprintf("来源里有 %d 个技能：%s", len(cands), candidateNames(cands)))
	}
	return res, nil
}

// filterCandidatesBySubpath 只留位于子目录里的候选。
//
// 归档带一层 `owner-repo-sha/` 前缀，所以子目录在 RelDir 的中间或末尾，
// 不能只比前缀。边界用 `/` 卡住，避免 `pdf` 命中 `pdf-extra`。
func filterCandidatesBySubpath(cands []skillPkgCandidate, subpath string) []skillPkgCandidate {
	sub := path.Clean(strings.Trim(subpath, "/"))
	if sub == "" || sub == "." || sub == "/" {
		return cands
	}
	var out []skillPkgCandidate
	for _, cand := range cands {
		rel := path.Clean(strings.ReplaceAll(cand.RelDir, "\\", "/"))
		if rel == sub || strings.HasPrefix(rel, sub+"/") ||
			strings.HasSuffix(rel, "/"+sub) || strings.Contains(rel, "/"+sub+"/") {
			out = append(out, cand)
		}
	}
	return out
}

// checkCandidateBudget 对单个技能目录复核 §9-10 的三条上限（10MB / 200 文件 / 5 层），
// 顺带把 Files / Bytes 补成**目录口径**（C5 起预览卡要显示，装完列表里的 bytes 也是这个数）。
//
// 仓库归档的解包预算放宽到 200MB/20000 条目（整仓），所以「单个技能不能太大」
// 必须在这里单独核一遍——否则一个 50MB 的技能会让每个 run 的目录与 read_skill
// 都背上不该有的重量。
func checkCandidateBudget(staging string, cand *skillPkgCandidate) error {
	if cand.RelDir == "" {
		// 平铺形态只有一个文件：validateSkillCandidate 已把它的字节数写进 Bytes。
		cand.Files = 1
		if cand.Bytes > skillPkgMaxBytes {
			return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
				fmt.Sprintf("技能 %q 超过单个上限（%d 字节）", cand.Name, skillPkgMaxBytes))
		}
		return nil
	}
	dir := filepath.Join(staging, filepath.FromSlash(cand.RelDir))
	var (
		files int
		bytes int64
	)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			return nil
		}
		if strings.Count(filepath.ToSlash(rel), "/") > skillPkgMaxDepth {
			return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
				fmt.Sprintf("技能 %q 的目录层级超过上限（%d 层）", cand.Name, skillPkgMaxDepth))
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files++
		bytes += info.Size()
		if files > skillPkgMaxFiles {
			return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
				fmt.Sprintf("技能 %q 的文件数超过上限（%d 个）", cand.Name, skillPkgMaxFiles))
		}
		if bytes > skillPkgMaxBytes {
			return skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
				fmt.Sprintf("技能 %q 超过单个上限（%d 字节）", cand.Name, skillPkgMaxBytes))
		}
		return nil
	})
	if err != nil {
		return err
	}
	cand.Files = files
	cand.Bytes = bytes
	return nil
}

// candidateNames 给错误文案与 warnings 用：`a、b、c`。
func candidateNames(cands []skillPkgCandidate) string {
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Name)
	}
	return strings.Join(names, "、")
}

// previewEntries 把候选翻成预览卡的行。Requires / Selected 保证非 nil（前端按数组画）。
func previewEntries(cands []skillPkgCandidate) []skillPreviewEntry {
	out := make([]skillPreviewEntry, 0, len(cands))
	for _, c := range cands {
		req := append([]string{}, c.Requires...)
		if req == nil {
			req = []string{}
		}
		out = append(out, skillPreviewEntry{
			Name:        c.Name,
			Description: c.Description,
			Bytes:       c.Bytes,
			Files:       c.Files,
			Requires:    req,
		})
	}
	return out
}

// selectedNames 算出「按这份选择会装哪几个」，给预览卡的选中集（C5）。
//
// 与 selectCandidates 同一套匹配规则，但**不报错**：预览只是把事实摆出来，
// 用户写错一个名字时卡片照出现（选中集少一个），到点「安装」才由 116111 拒绝。
// 没给选择且只有一个候选时，选中它——这是 selectCandidates 的默认，卡片要跟执行走同一套。
func selectedNames(cands []skillPkgCandidate, skills []string, all bool) []string {
	out := []string{}
	if !all && len(skills) == 0 && len(cands) == 1 {
		return []string{cands[0].Name}
	}
	for _, cand := range cands {
		if all || skillSelected(skills, cand.Name) {
			out = append(out, cand.Name)
		}
	}
	return out
}

// skillSelected 是候选名与一条选择词是否匹配（支持尾 `*` 前缀）。
func skillSelected(skills []string, name string) bool {
	for _, want := range skills {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if want == "*" || want == name ||
			(strings.HasSuffix(want, "*") && strings.HasPrefix(name, strings.TrimSuffix(want, "*"))) {
			return true
		}
	}
	return false
}

// previewSourceOf 把规格翻成 data.source。URL 是**实际要抓的地址**（GitLab 的默认分支
// 已经解析过），不是用户原话——卡片上要能核对「它到底去拿了什么」。
func previewSourceOf(spec skillSourceSpec, archiveURL string) skillPreviewSource {
	return skillPreviewSource{
		Kind:      spec.Kind,
		Owner:     spec.Owner,
		Namespace: spec.Namespace,
		Repo:      spec.Repo,
		Ref:       spec.Ref,
		Subpath:   spec.Subpath,
		URL:       archiveURL,
	}
}

// postSkillsPreview 是 POST /api/skills/preview（§6.1 / §6.5）：只解析不写盘。
func (s *Server) postSkillsPreview(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		s.previewFromMultipart(c, p)
		return
	}
	var in skillRequestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeSkillSourceBad,
			"请求体要是 {source} 或 {command} 的 JSON", nil)
		return
	}
	if in.All && len(in.Skills) > 0 {
		writeEnvelope(c, http.StatusBadRequest, codeSkillSourceBad, "skills 与 all 只能给一个", nil)
		return
	}
	spec, err := parseSkillInput(in.Source, in.Command)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	res, err := resolveSkillSource(ctx, s.mediaRoot, p.TenantID, spec)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	defer os.RemoveAll(res.staging)
	skills := append(append([]string{}, spec.Skills...), in.Skills...)
	chosen := len(in.Skills) > 0 || in.All || len(spec.Skills) > 0 || spec.All
	warnings := append([]string{}, res.warnings...)
	for _, cand := range res.cands {
		warnings = append(warnings, cand.Warnings...)
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", skillPreviewData{
		Source:    previewSourceOf(spec, res.archiveURL),
		Skills:    previewEntries(res.cands),
		Selected:  selectedNames(res.cands, skills, spec.All || in.All),
		Ambiguous: len(res.cands) > 1 && !chosen,
		Warnings:  warnings,
	})
}

// previewFromMultipart 预览一个上传包。与 install 的区别只有「落不落正式目录」，
// 所以解包、找候选、预算复核全部复用同一套 helper。
func (s *Server) previewFromMultipart(c *gin.Context, p principal) {
	data, err := readSkillUpload(c)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	staging, err := newSkillStaging(s.mediaRoot, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	defer os.RemoveAll(staging)
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
	// 上传包同样按「单个技能」复核一遍并补 Files/Bytes：预览卡的两列数不能因为
	// 入口是文件上传就空着（整包预算只保总量，不保单个技能）。
	for i := range cands {
		if err := checkCandidateBudget(staging, &cands[i]); err != nil {
			writeSkillWriteError(c, err)
			return
		}
	}
	warnings := []string{}
	for _, cand := range cands {
		warnings = append(warnings, cand.Warnings...)
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", skillPreviewData{
		Source:    skillPreviewSource{Kind: "upload", URL: "multipart:file"},
		Skills:    previewEntries(cands),
		Selected:  selectedNames(cands, c.PostFormArray("skills"), formBool(c, "all")),
		Ambiguous: len(cands) > 1,
		Warnings:  warnings,
	})
}

// installFromSource 是 POST /api/skills/install 的 JSON 分支：来源串/命令串
// 与上传包走同一个落点（§6.5 末段）。
func (s *Server) installFromSource(c *gin.Context, p principal) {
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	var in skillRequestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeSkillSourceBad,
			"请求体要是 {source} 或 {command} 的 JSON（上传包请用 multipart）", nil)
		return
	}
	if in.All && len(in.Skills) > 0 {
		writeEnvelope(c, http.StatusBadRequest, codeSkillSourceBad, "skills 与 all 只能给一个", nil)
		return
	}
	spec, err := parseSkillInput(in.Source, in.Command)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	skills := append(append([]string{}, spec.Skills...), in.Skills...)
	res, err := resolveSkillSource(ctx, s.mediaRoot, p.TenantID, spec)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	defer os.RemoveAll(res.staging)
	chosen, err := selectCandidates(res.cands, skills, spec.All || in.All)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	installed, err := s.installCandidates(ctx, p.TenantID, res.staging, chosen, false)
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	// warnings 只报**选中**技能的提示：没装的候选有什么毛病，与这次安装无关。
	warnings := append([]string{}, res.warnings...)
	for _, cand := range chosen {
		warnings = append(warnings, cand.Warnings...)
	}
	s.publishSkillEvents(ctx, p.TenantID, installed)
	writeEnvelope(c, http.StatusCreated, codeOK, "",
		skillInstallData{Installed: installed, Warnings: warnings})
}
