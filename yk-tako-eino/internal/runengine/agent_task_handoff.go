// 本文件是重规划交接文件与继任标记的读写（方案真源 = doc/75 §5／§6；施工包 §5.2/§5.3）。
//
// 继任组的组容器里有两份程序管理的文件（路径全部由 ID 推导，模型不能指定）：
//
//	runtime/orchestrations/<orchId>/handoff.md     仅继任组：为什么重规划、复用哪些结果、不得重放什么
//	runtime/orchestrations/<orchId>/successor.json 仅原组：指向它唯一的继任组
//
// frontmatter 只由程序按固定字段渲染（字符串先过 safeFrontmatterScalar），正文只给人读；
// 程序从不从正文反解析机器字段。写入一律「临时文件 + 原子 rename」，调用方持原组组锁。
// reuse 的产物字节在创建时复制进继任组自己的 results/ 区（doc/75 施工包 §3 裁定 3），
// 旧项目随后被删除不影响继任组；这里只负责快照复制，不做跨组依赖。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	handoffFileName    = "handoff.md"
	successorFileName  = "successor.json"
	groupReuseFileName = "reuse.json"
	handoffKind        = "orchestration-replan"
	// successorKeyPrefix 是继任创建幂等键的规范化前缀（施工包 §4.2）：
	// successor:<原编排组ID>:<调用方键>，恢复时由此取得原组坐标。
	successorKeyPrefix = "successor:"
	// successorMaxDepth 是重规划链的最大深度（doc/75 §7.3）：每一条 handoff 链 +1，超过 8 必须问人。
	successorMaxDepth = 8
	// successorReuseMax / successorNoReplayMax / successorTextMaxRunes 是请求形状上限（施工包 §4.1）。
	successorReuseMax         = 32
	successorNoReplayMax      = 16
	successorTextMaxRunes     = 8000
	successorNoReplayMaxRunes = 2000
)

// errHandoffMissing：组存在但没有（或没有完整的）重规划交接。HTTP 404 117004。
var errHandoffMissing = errors.New("find replan handoff")

// handoffReuseEntry 是 handoff frontmatter 里的一条结果引用（机器字段）。
type handoffReuseEntry struct {
	TaskID         string `json:"taskId"`
	ResultRevision int    `json:"resultRevision"`
}

// handoffDoc 是 handoff.md frontmatter 的机器字段。正文不在这里——它只供人读。
type handoffDoc struct {
	Kind            string              `json:"kind"`
	OrchestrationID string              `json:"orchestrationId"`
	ReplanOf        string              `json:"replanOf"`
	ReplanDepth     int                 `json:"replanDepth"`
	Reuse           []handoffReuseEntry `json:"reuse"`
}

// successorMark 是原组容器里的 successor.json：通用新旧 ID、创建键与时间，不含 Provider 字段。
// 一个原组最多一个继任组；再次重规划从最新组继续（doc/75 §5.2）。
type successorMark struct {
	OrchestrationID          string    `json:"orchestrationId"`
	SuccessorOrchestrationID string    `json:"successorOrchestrationId"`
	CreationKey              string    `json:"creationKey"`
	CreatedAt                time.Time `json:"createdAt"`
}

// groupReuseEntry 是继任组 reuse.json 的一条：引擎与 Provider 之间仍只传 taskId+resultRevision，
// Provider 另外需要知道旧结果的确切 Provider 类型与引用（Git commit），这两个字段只落组目录。
type groupReuseEntry struct {
	// SourceOrchestrationID 只在组目录内可读，用于崩溃恢复时回溯源结果的组容器。
	SourceOrchestrationID string         `json:"sourceOrchestrationId,omitempty"`
	TaskID                string         `json:"taskId"`
	ResultRevision        int            `json:"resultRevision"`
	Provider              string         `json:"provider,omitempty"`
	Reference             string         `json:"reference,omitempty"`
	Artifacts             []TaskArtifact `json:"artifacts,omitempty"`
}

// groupReuseFile 是继任组的输入快照清单：创建时一次写定，之后只读。
type groupReuseFile struct {
	OrchestrationID string            `json:"orchestrationId"`
	ReplanOf        string            `json:"replanOf"`
	Reuse           []groupReuseEntry `json:"reuse"`
}

// groupContainerDir 算一个编排组的容器目录（handoff／successor 与 workspace/ 的父目录）。
func (s *Store) groupContainerDir(o AgentOrchestration) (string, error) {
	container, err := projectContainerDir(s.workspaceRoot, o.TenantID, o.WorkspaceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(container, projectRuntimeDirName, "orchestrations", o.ID), nil
}

func handoffPath(container string) string   { return filepath.Join(container, handoffFileName) }
func successorPath(container string) string { return filepath.Join(container, successorFileName) }
func groupReusePath(container string) string {
	return filepath.Join(container, groupReuseFileName)
}

// safeFrontmatterScalar 把关进 frontmatter 的单行值：只允许 ID 字符（字母数字、_ 与 -），
// 其余直接拒绝。机器字段全是程序生成的 ID 与整数，不给模型输入拼出额外 YAML 字段的机会。
func safeFrontmatterScalar(v string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("frontmatter 字段为空")
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("frontmatter 字段含非法字符：%q", v)
	}
	return v, nil
}

// renderHandoff 按固定顺序渲染 handoff.md：frontmatter（机器字段）→ 五段正文。
// 正文里的 model 输入只作为 Markdown 文本，不进入机器解析。configNote 是程序生成的工作区
// 配置变更说明（继承时不写），附在「为什么创建继任组」段末，与模型声明分开成段。
func renderHandoff(m handoffDoc, reason, successorReason, evidence, configNote string, noReplay []string, artifactsByTask map[string][]TaskArtifact) ([]byte, error) {
	orchID, err := safeFrontmatterScalar(m.OrchestrationID)
	if err != nil {
		return nil, err
	}
	replanOf, err := safeFrontmatterScalar(m.ReplanOf)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\nkind: ")
	b.WriteString(handoffKind)
	b.WriteString("\norchestrationId: ")
	b.WriteString(orchID)
	b.WriteString("\nreplanOf: ")
	b.WriteString(replanOf)
	b.WriteString("\nreplanDepth: ")
	b.WriteString(strconv.Itoa(m.ReplanDepth))
	b.WriteString("\nreuse:")
	if len(m.Reuse) == 0 {
		b.WriteString(" []")
	}
	b.WriteString("\n")
	for _, r := range m.Reuse {
		tid, err := safeFrontmatterScalar(r.TaskID)
		if err != nil {
			return nil, err
		}
		b.WriteString("  - taskId: ")
		b.WriteString(tid)
		b.WriteString("\n    resultRevision: ")
		b.WriteString(strconv.Itoa(r.ResultRevision))
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("# 为什么重规划\n\n")
	b.WriteString(strings.TrimSpace(reason))
	b.WriteString("\n\n# 为什么创建继任组\n\n")
	b.WriteString(strings.TrimSpace(successorReason))
	if note := strings.TrimSpace(configNote); note != "" {
		b.WriteString("\n\n" + note)
	}
	b.WriteString("\n\n# 目标可行性证据\n\n")
	b.WriteString(strings.TrimSpace(evidence))
	b.WriteString("\n\n# 哪些结果仍有效\n\n")
	if len(m.Reuse) == 0 {
		b.WriteString("（这次继任不复用旧结果）\n")
	}
	for _, r := range m.Reuse {
		fmt.Fprintf(&b, "- %s（resultRevision %d）", r.TaskID, r.ResultRevision)
		if arts := artifactsByTask[r.TaskID]; len(arts) > 0 {
			paths := make([]string, 0, len(arts))
			for _, a := range arts {
				paths = append(paths, a.Path)
			}
			b.WriteString("；产物：" + strings.Join(paths, "、"))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n# 不得重放的动作\n\n")
	if len(noReplay) == 0 {
		b.WriteString("（模型没有声明不可重放的动作）\n")
	}
	for _, s := range noReplay {
		b.WriteString("- " + strings.TrimSpace(s) + "\n")
	}
	b.WriteString("\n以上由模型声明，程序未验证，也不会自动重放。\n")
	return []byte(b.String()), nil
}

// writeHandoff 原子落盘 handoff.md（shared helper：同目录临时文件 + rename）。
// 调用方持原组组锁（同一原组不能并发产生两份交接）。
func writeHandoff(container string, content []byte) error {
	if err := os.MkdirAll(container, 0o755); err != nil {
		return err
	}
	return atomicWriteFile(handoffPath(container), content, 0o644)
}

// readHandoff 读并校验 handoff.md：不存在 = (nil, "", nil)；存在但不完整 = 错误。
// 完整性只看机器字段（kind／orchestrationId 匹配、replanOf 非空、depth≥1）。
func readHandoff(container, orchestrationID string) (*handoffDoc, string, error) {
	b, err := os.ReadFile(handoffPath(container))
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	doc, body, err := parseHandoff(string(b))
	if err != nil {
		return nil, "", fmt.Errorf("重规划交接文件损坏：%w", err)
	}
	if doc.Kind != handoffKind || doc.OrchestrationID != orchestrationID ||
		doc.ReplanOf == "" || doc.ReplanDepth < 1 {
		return nil, "", fmt.Errorf("重规划交接文件不完整")
	}
	return doc, body, nil
}

// parseHandoff 解析本文件自己渲染的固定 frontmatter；正文原样返回（含分隔行后的换行）。
func parseHandoff(raw string) (*handoffDoc, string, error) {
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return nil, "", fmt.Errorf("缺少 frontmatter")
	}
	rest := norm[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("frontmatter 未闭合")
	}
	fm, body := rest[:end], rest[end+len("\n---\n"):]
	doc := &handoffDoc{}
	inReuse := false
	var cur *handoffReuseEntry
	for _, line := range strings.Split(fm, "\n") {
		if strings.HasPrefix(line, "  - ") {
			if !inReuse {
				return nil, "", fmt.Errorf("reuse 缩进不符合契约")
			}
			reuse := handoffReuseEntry{}
			doc.Reuse = append(doc.Reuse, reuse)
			cur = &doc.Reuse[len(doc.Reuse)-1]
			line = strings.TrimPrefix(line, "  - ")
		} else if strings.HasPrefix(line, "    ") {
			if cur == nil {
				return nil, "", fmt.Errorf("reuse 子字段没有归属项")
			}
			line = strings.TrimPrefix(line, "    ")
		} else {
			inReuse = false
			cur = nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case key == "reuse":
			inReuse = true
			if value != "" && value != "[]" {
				return nil, "", fmt.Errorf("reuse 必须是列表")
			}
		case key == "taskId" && cur != nil:
			cur.TaskID = value
		case key == "resultRevision" && cur != nil:
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, "", fmt.Errorf("resultRevision 不是整数")
			}
			cur.ResultRevision = n
		case key == "kind":
			doc.Kind = value
		case key == "orchestrationId":
			doc.OrchestrationID = value
		case key == "replanOf":
			doc.ReplanOf = value
		case key == "replanDepth":
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, "", fmt.Errorf("replanDepth 不是整数")
			}
			doc.ReplanDepth = n
		}
	}
	for _, r := range doc.Reuse {
		if r.TaskID == "" || r.ResultRevision < 1 {
			return nil, "", fmt.Errorf("reuse 项缺少 taskId 或 resultRevision")
		}
	}
	return doc, body, nil
}

// writeSuccessorMark 原子写原组的 successor.json。调用方持原组组锁。
func writeSuccessorMark(container string, m successorMark) error {
	if err := os.MkdirAll(container, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(successorPath(container), b, 0o644)
}

// readSuccessorMark 读原组的 successor.json；不存在 = nil。
func readSuccessorMark(container string) (*successorMark, error) {
	b, err := os.ReadFile(successorPath(container))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m successorMark
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("继任标记损坏：%w", err)
	}
	return &m, nil
}

// writeGroupReuse 原子写继任组的输入快照清单。调用方持原组组锁；文件只写一次。
func writeGroupReuse(container string, f groupReuseFile) error {
	if err := os.MkdirAll(container, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return atomicWriteFile(groupReusePath(container), b, 0o644)
}

// readGroupReuse 读继任组的输入快照清单；不存在 = nil（非继任组或快照尚未写入）。
func readGroupReuse(container string) (*groupReuseFile, error) {
	b, err := os.ReadFile(groupReusePath(container))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f groupReuseFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("复用快照清单损坏：%w", err)
	}
	return &f, nil
}

// copyImmutableResult 把一个旧组的不变结果逐字复制进新组自己的 results/ 区。
//
// 为什么复制字节而不是留旧组引用：跨项目复用与「旧项目随后删除」都要求继任组不依赖旧容器；
// 复制之后引擎与 Provider 仍只按 taskId+resultRevision 读新组（施工包 §3 裁定 3）。
// 复制前先核验旧结果与声明产物可读；目标目录必须尚不存在，发现已存在即拒绝，不做覆盖。
func copyImmutableResult(oldG, newG *GroupHandle, ref ResultRef) error {
	res, err := readImmutableResult(oldG, ref)
	if err != nil {
		return fmt.Errorf("复用结果 %s/%d 不可读：%w", ref.TaskID, ref.ResultRevision, err)
	}
	for _, a := range res.Artifacts {
		if _, err := readResultArtifact(oldG, ref, a.Path); err != nil {
			return fmt.Errorf("复用结果 %s/%d 的产物 %s 不可读：%w", ref.TaskID, ref.ResultRevision, a.Path, err)
		}
	}
	src := oldG.resultDir(ref.TaskID, ref.ResultRevision)
	dst := newG.resultDir(ref.TaskID, ref.ResultRevision)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("复用结果 %s/%d 的目标目录已存在", ref.TaskID, ref.ResultRevision)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := copyTreeInto(src, dst); err != nil {
		return fmt.Errorf("复制复用结果 %s/%d 失败：%w", ref.TaskID, ref.ResultRevision, err)
	}
	return nil
}

// copyTreeInto 递归复制目录内容（保留相对布局，文件权限 0644，目标文件必须不存在）。
// 结果目录里只有程序写出的普通文件（result.json / artifacts/ / manifest.json）。
func copyTreeInto(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("复用结果里出现非普通文件：%s", rel)
		}
		return copyFilePerm(path, target, 0o644)
	})
}

// reuseApplication 是一次「复用结果带进工作现场」的回执：带入了哪些相对路径、
// 哪些路径因现场已有不同内容而未覆盖（交模型裁决）。
type reuseApplication struct {
	Applied   []string
	Conflicts []string
	Note      string
}

// applyGroupReuse 把继任组的输入快照带进无父 Task 的工作现场（doc/75 §7.4）：
// reuse.json 里的每条旧结果，把 results/<旧taskId>/<rev>/artifacts/ 整棵树逐字复制到工作根。
// 字节已相同 = 幂等跳过；目标已存在但字节不同 = 不覆盖，记一条冲突进 PromptOverlay。
// 只读快照、不回头读旧组容器——旧项目被删除不影响这里（复制在创建继任组时已完成）。
func applyGroupReuse(g *GroupHandle, workRoot string) (reuseApplication, error) {
	var app reuseApplication
	rf, err := readGroupReuse(filepath.Dir(g.Dir))
	if err != nil {
		return app, err
	}
	if rf == nil || len(rf.Reuse) == 0 {
		return app, nil
	}
	for _, e := range rf.Reuse {
		ref := ResultRef{TaskID: e.TaskID, ResultRevision: e.ResultRevision}
		if _, err := readImmutableResult(g, ref); err != nil {
			return app, fmt.Errorf("复用结果 %s/%d 不可读：%w", e.TaskID, e.ResultRevision, err)
		}
		artRoot := filepath.Join(g.resultDir(ref.TaskID, ref.ResultRevision), "artifacts")
		if _, err := os.Stat(artRoot); os.IsNotExist(err) {
			continue // 没声明产物：可复用的只是交接摘要，没有文件可带
		} else if err != nil {
			return app, err
		}
		err := filepath.Walk(artRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("复用结果里出现非普通文件：%s", path)
			}
			rel, err := filepath.Rel(artRoot, path)
			if err != nil {
				return err
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("复用结果路径越界：%s", rel)
			}
			dst := filepath.Join(workRoot, rel)
			curSHA, exists, err := hashFileIfExists(dst)
			if err != nil {
				return err
			}
			srcSHA, _, err := hashFile(path)
			if err != nil {
				return err
			}
			if exists {
				if curSHA != srcSHA {
					app.Conflicts = append(app.Conflicts, rel)
				}
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := copyFilePerm(path, dst, 0o644); err != nil {
				return err
			}
			app.Applied = append(app.Applied, rel)
			return nil
		})
		if err != nil {
			return app, fmt.Errorf("带入复用结果 %s/%d 失败：%w", e.TaskID, e.ResultRevision, err)
		}
	}
	sort.Strings(app.Applied)
	sort.Strings(app.Conflicts)
	var b strings.Builder
	if len(app.Applied) > 0 {
		b.WriteString("\n\n本次工作现场已带入前代任务仍有效的结果（创建继任组时固化的快照）：")
		for _, p := range app.Applied {
			b.WriteString("\n- " + p)
		}
		b.WriteString("\n这些文件是输入基线，不等于本次交付；本次改了什么就声明什么。")
	}
	if len(app.Conflicts) > 0 {
		b.WriteString("\n\n以下路径在工作现场里已有与复用结果不同的内容，程序没有覆盖，需要你裁决：")
		for _, p := range app.Conflicts {
			b.WriteString("\n- " + p)
		}
	}
	app.Note = b.String()
	return app, nil
}

// handoffView 是 GET /api/orchestrations/:id/handoff 的 data（字段序 = 施工包 §4.3）。
type handoffView struct {
	OrchestrationID string              `json:"orchestrationId"`
	ReplanOf        string              `json:"replanOf"`
	ReplanDepth     int                 `json:"replanDepth"`
	Reuse           []handoffReuseEntry `json:"reuse"`
	Body            string              `json:"body"`
}

// GetOrchestrationHandoff 读继任组的交接文件。可见性沿既有属主链（来源会话属主）。
// 组不存在／不可见 117002；组在但没有完整 handoff 117004。
func (s *Store) GetOrchestrationHandoff(ctx context.Context, p principal, id string) (handoffView, error) {
	var rows []AgentOrchestration
	if err := s.db.WithContext(ctx).Raw(`
SELECT o.* FROM agent_orchestrations o
  JOIN sessions s ON s.id = o.source_session_id
 WHERE o.id = ? AND o.tenant_id = ? AND s.user_id = ?`, id, p.TenantID, p.UserID).
		Scan(&rows).Error; err != nil {
		return handoffView{}, err
	}
	if len(rows) == 0 {
		return handoffView{}, errOrchNotFound
	}
	o := rows[0]
	container, err := s.groupContainerDir(o)
	if err != nil {
		return handoffView{}, err
	}
	doc, body, err := readHandoff(container, o.ID)
	if err != nil {
		return handoffView{}, err
	}
	if doc == nil {
		return handoffView{}, errHandoffMissing
	}
	reuse := doc.Reuse
	if reuse == nil {
		reuse = []handoffReuseEntry{}
	}
	return handoffView{
		OrchestrationID: o.ID, ReplanOf: doc.ReplanOf, ReplanDepth: doc.ReplanDepth,
		Reuse: reuse, Body: body,
	}, nil
}

// handoffReplanOf 读某组的 replanOf／depth；没有 handoff 返回 ("",0,nil)。
// 供详情读面追加可选字段与创建继任组时计算链深。
func (s *Store) handoffReplanOf(o AgentOrchestration) (string, int, error) {
	container, err := s.groupContainerDir(o)
	if err != nil {
		return "", 0, err
	}
	doc, _, err := readHandoff(container, o.ID)
	if err != nil || doc == nil {
		return "", 0, err
	}
	return doc.ReplanOf, doc.ReplanDepth, nil
}

// replanChain 回溯一个组的所有前代（含自身，最近的在前），用于复用成员校验。
// 每跳都从组容器现读 handoff；链最多 8 层，超过即数据异常（创建时已拦）。
func (s *Store) replanChain(ctx context.Context, start AgentOrchestration) ([]AgentOrchestration, error) {
	chain := []AgentOrchestration{start}
	cur := start
	for depth := 0; depth < successorMaxDepth; depth++ {
		prevID, _, err := s.handoffReplanOf(cur)
		if err != nil {
			return nil, err
		}
		if prevID == "" {
			return chain, nil
		}
		var prev AgentOrchestration
		if err := s.db.WithContext(ctx).Where("id = ?", prevID).Take(&prev).Error; err != nil {
			return nil, err
		}
		chain = append(chain, prev)
		cur = prev
	}
	return chain, nil
}

// reuseArtifactsByTask 是 handoff 正文渲染的附件索引（taskId → 声明产物）。
func reuseArtifactsByTask(entries []groupReuseEntry) map[string][]TaskArtifact {
	out := make(map[string][]TaskArtifact, len(entries))
	for _, e := range entries {
		out[e.TaskID] = e.Artifacts
	}
	return out
}

// sortedReuseRefs 把快照清单转成稳定顺序的通用引用（handoff frontmatter 与响应共用）。
func sortedReuseRefs(entries []groupReuseEntry) []handoffReuseEntry {
	out := make([]handoffReuseEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, handoffReuseEntry{TaskID: e.TaskID, ResultRevision: e.ResultRevision})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}
