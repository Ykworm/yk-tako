// skillask.go 是对话内安装（doc/52 D20/D24 的 C3 拍）：模型提议、人点击、程序落盘。
//
// 为什么分成「提议」与「执行」两段：
//   - 模型不能握写盘的手。它能做的只有把「要装什么」整理成一份**暂存提议**（来源、
//     staging 目录、选中的技能名），落成 run_questions 一行 kind=install；
//   - 人的点击是唯一授权。answerQuestion 的 CAS 赢下这一行之后，由**程序**读提议、
//     加锁、rename、写行、发 skills_changed——模型看到的只是结果文本。
//
// 提议的凭据在**盘上**（`<tenant>/skills/.staging/<rand>/`）而不是内存里：工具挂起期间
// 进程可能死、可能换副本，续跑对账只认「行 + 盘」。行里的 staging 路径在收尾时
// 重新校验必须落在本租户 `.staging` 之内，路径被篡改等于提议失效。
package runengine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// 安装行的三种收尾文案（工具的 tool_result 就是它们；跨「当场批准」与「续跑对账」同一份字节）。
const (
	skillInstallCanceledText = "用户取消了安装。"
	skillInstallExpiredText  = "安装已过期（暂存目录已被清理），请重新发起安装。"
	skillInstallUnknownText  = "安装结果未知（执行者中途退出），请查看技能列表；没出现就重新发起安装。"
)

// skillInstallProposal 是 kind=install 行 args 列的内容（见 questions.go 的 args 按 kind 变义）。
//
// Skills 只存**选中的名字**：不存 `all` 标志，因为「全装」只是提议时的一次选择，
// 执行时按名字重选更窄——提议之后来源里多出一个同名新技能，不该被「当时说了全装」带进来。
type skillInstallProposal struct {
	Source     skillPreviewSource  `json:"source"`
	Staging    string              `json:"staging"`
	Skills     []string            `json:"skills"`
	Candidates []skillPreviewEntry `json:"candidates"`
	Warnings   []string            `json:"warnings"`
}

// proposalStagingPath 校验行里的 staging 路径仍落在本租户的 `.staging` 之内，并返回其真实路径。
//
// 两侧都 Abs + EvalSymlinks：符号链接是「路径看起来在根内、实际指到根外」的唯一写法。
// `.staging` 本身不存在时 EvalSymlinks 报错——那正是「已过期」，不需要区分。
func proposalStagingPath(skillsRoot, staging string) (string, bool) {
	if strings.TrimSpace(staging) == "" {
		return "", false
	}
	base, err := filepath.Abs(filepath.Join(skillsRoot, ".staging"))
	if err != nil {
		return "", false
	}
	target, err := filepath.Abs(staging)
	if err != nil || !pathWithin(base, target) {
		return "", false
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil || !pathWithin(realBase, realTarget) {
		return "", false
	}
	return realTarget, true
}

// discardInstallProposal 删掉提议的暂存目录（取消 / 装上之后都调）。
// 不碰正式目录：取消的语义是「什么都没发生」。
func discardInstallProposal(store *Store, mediaRoot, tenantID, args string) {
	var prop skillInstallProposal
	if err := json.Unmarshal([]byte(args), &prop); err != nil {
		return
	}
	root := tenantSkillsRoot(mediaRoot, tenantID)
	if staging, ok := proposalStagingPath(root, prop.Staging); ok {
		_ = os.RemoveAll(staging)
	}
}

// installApprovedProposal 是人点了「安装」之后程序做的事，返回写回 answer 的 tool_result 文本。
//
// 顺序与 C1 的写路径同一套：重新读 staging 里的候选 → 按名字选 → 锁内 rename + 写行。
// **重新扫一遍而不是信任行里的候选快照**：盘是唯一真身，行只是「当时选了什么」的记录；
// 两次读之间盘可能被清扫器动过，那时就该报「已过期」而不是拿着旧描述去安装。
//
// 失败一律转成文本而不是错误：工具那一侧在等 answer，它读到的必须是「发生了什么」，
// 而不是一个会让 run 崩掉的错误。
func (s *Server) installApprovedProposal(ctx context.Context, row RunQuestion) string {
	var prop skillInstallProposal
	if err := json.Unmarshal([]byte(row.Args), &prop); err != nil {
		return "安装失败：提议参数损坏，请重新发起安装。"
	}
	root := tenantSkillsRoot(s.mediaRoot, row.TenantID)
	staging, ok := proposalStagingPath(root, prop.Staging)
	if !ok {
		return skillInstallExpiredText
	}
	counter, err := NewCl100kCounter()
	if err != nil {
		return "安装失败：" + err.Error()
	}
	cands, err := findSkillCandidates(staging, counter, false, true)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "安装失败：" + skillErrText(err)
	}
	chosen, err := selectCandidates(cands, prop.Skills, false)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "安装失败：" + skillErrText(err)
	}
	installed, err := installSkillCandidates(ctx, s.store, s.mediaRoot, row.TenantID, staging, chosen, false)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "安装失败：" + skillErrText(err)
	}
	_ = os.RemoveAll(staging)
	names := make([]string, 0, len(installed))
	for _, e := range installed {
		names = append(names, e.Name)
		// 目录是每个 run 拼进 system prompt 的派生视图：装好就要通知别人重拉。
		s.store.PublishEvent(ctx, row.TenantID, eventSkillsChanged, e.Name)
	}
	return fmt.Sprintf("已安装技能：%s。", strings.Join(names, "、"))
}

// skillErrText 把写路径错误压成一句人能读的话。非 skillWriteErr（I/O）保留原文——
// 那是盘的问题，藏起来只会让人反复重试。
func skillErrText(err error) string {
	if werr, ok := err.(*skillWriteErr); ok {
		return werr.msg
	}
	return err.Error()
}

// skillInstallInput 是 skill_install 的入参。与 preview/install 端点的 JSON 同义：
// 模型通常把用户的原话（command）整条贴进来，也可能自己整理成 source。
type skillInstallInput struct {
	Source  string   `json:"source,omitempty" jsonschema:"description=来源串：owner/repo、GitHub/GitLab 仓库 URL、或 .zip/.tar.gz 直链；与 command 二选一"`
	Command string   `json:"command,omitempty" jsonschema:"description=用户原话里的安装命令（如 npx skills add owner/repo --skill x）；只读解析、不执行 npx；与 source 二选一"`
	Skills  []string `json:"skills,omitempty" jsonschema:"description=只装这些技能名（可带尾 *）；来源里有多个技能而用户没指定时不要猜，先问用户"`
	All     bool     `json:"all,omitempty" jsonschema:"description=true 表示来源里的技能全装；仅在用户明确说全装时用"`
}

// newSkillInstallTool 造 skill_install（doc/52 §2.8：只进 manager 池，普通子代理已剔除）。
//
// 它自己不写正式目录：解析 → 落 staging → 写 kind=install 行 → 发 question 帧 → 等人。
// 人批准后由端点执行（见 answerQuestion / settleInstallAnswer）。
func newSkillInstallTool(store *Store, mediaRoot string) (tool.BaseTool, error) {
	return utils.InferTool("skill_install",
		"从一条来源安装技能：把来源串或用户的安装命令解析成候选技能，暂存后请用户确认。"+
			"本工具不会直接装——用户点确认后才落盘；挂起期间不烧 token，确认结果作为这次调用的结果回到同一轮。"+
			"来源里有多个技能而用户没指定时，先问清楚要装哪些，不要猜。",
		func(ctx context.Context, in skillInstallInput) (string, error) {
			return handleSkillInstall(ctx, store, mediaRoot, in)
		})
}

func handleSkillInstall(ctx context.Context, store *Store, mediaRoot string, in skillInstallInput) (string, error) {
	p, ok := principalFromCtx(ctx)
	if !ok {
		return "", fmt.Errorf("skill_install 不可用（没有租户身份）")
	}
	// 只有管理员能发起：安装授权是租户级的，viewer/member 发起也批不动（端点会 403），
	// 早拒省掉一次抓取与一个挂起行。
	if !p.SystemAdmin && roleRank(p.Role) < roleRank("admin") {
		return "安装技能需要管理员发起：请让团队的管理员来做，或把来源告诉管理员。", nil
	}
	if in.All && len(in.Skills) > 0 {
		return "skills 与 all 只能给一个。", nil
	}
	spec, err := parseSkillInput(in.Source, in.Command)
	if err != nil {
		return "", err
	}
	skills := append(append([]string{}, spec.Skills...), in.Skills...)
	res, err := resolveSkillSource(ctx, mediaRoot, p.TenantID, spec)
	if err != nil {
		return "", err
	}
	chosen, err := selectCandidates(res.cands, skills, spec.All || in.All)
	if err != nil {
		_ = os.RemoveAll(res.staging)
		return "", err
	}
	warnings := append([]string{}, res.warnings...)
	for _, cand := range chosen {
		warnings = append(warnings, cand.Warnings...)
	}
	selected := make([]string, 0, len(chosen))
	for _, cand := range chosen {
		selected = append(selected, cand.Name)
	}
	prop := skillInstallProposal{
		Source:     previewSourceOf(spec, res.archiveURL),
		Staging:    res.staging,
		Skills:     selected,
		Candidates: previewEntries(res.cands),
		Warnings:   warnings,
	}
	text := fmt.Sprintf("要安装技能 %s（来源 %s），请确认。", strings.Join(selected, "、"), res.archiveURL)
	row, err := openWaitRow(ctx, store, questionKindInstall, "skill_install", prop, text)
	if err != nil {
		_ = os.RemoveAll(res.staging)
		return "", err
	}
	answered, err := waitForAnswer(ctx, store, row.ID)
	if err != nil {
		// run 被停/进程收摊：行留 pending（清扫器收 stale），暂存目录由 G1 的 24h 清扫兜底；
		// 这里先删，免得一次次停止攒垃圾。
		_ = os.RemoveAll(res.staging)
		return "", err
	}
	return answered.Answer, nil
}
