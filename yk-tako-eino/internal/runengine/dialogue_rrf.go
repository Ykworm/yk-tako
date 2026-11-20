package runengine

// 本文件是 hybrid 查询链的合并与渲染（doc/84 §7.1/§7.3；施工包 §5.6）：
//
//   - RRF（Reciprocal Rank Fusion）：两臂名次从 1 起，1/(60+rank) 累加；同名 run
//     两臂都出现时分数累加；rrfScore 降序、runID 升序打破平分。BM25 分数与向量
//     距离只做臂内排序，绝不跨臂比较（两家的分数量纲不同）。
//   - dialogueHybridFuse 把两臂命中（带坐标与脱敏原文）按 RRF 融合成候选列表；
//   - renderDialogueHybridBlock 按程序模板拼 <dialogue-context>（doc/84 §7.3 逐字），
//     引用由程序拼接，不总结、不改写，长 passage 按长度截取。

import (
	"sort"
	"strings"
	"time"
)

// dialogueRRFK 是 RRF 的平滑常数（doc/84 §7.1 公式 1/(60+rank)，rank 从 1 起）。
// 60 是 Cormack 等人 2009 年论文与开源实现（Elasticsearch/OpenSearch 的 RRF）通用值。
const dialogueRRFK = 60

// dialogueRRFArmNames 是两臂在 sources 里的词表与顺序：词法在前、向量在后。
var dialogueRRFArmNames = [2]string{"lexical", "vector"}

// dialogueRRFEntry 是合并后的一条：runID + 融合分 + 来源臂（按臂顺序，只含出现过的臂）。
type dialogueRRFEntry struct {
	RunID   string
	RRF     float64
	Sources []string
}

// dialogueRRFMerge 合并两臂的 run_id 名次列表（各自已按臂内分数排好序）。
// 平分时 runID 升序打破，保证同一份输入永远得到同一份输出（可测、可对质）。
func dialogueRRFMerge(lexical, vector []string) []dialogueRRFEntry {
	entries := make(map[string]*dialogueRRFEntry, len(lexical)+len(vector))
	arms := []struct {
		name string
		ids  []string
	}{{dialogueRRFArmNames[0], lexical}, {dialogueRRFArmNames[1], vector}}
	for _, arm := range arms {
		for rank, id := range arm.ids {
			e := entries[id]
			if e == nil {
				e = &dialogueRRFEntry{RunID: id}
				entries[id] = e
			}
			e.RRF += 1.0 / float64(dialogueRRFK+rank+1)
			e.Sources = append(e.Sources, arm.name)
		}
	}
	out := make([]dialogueRRFEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RRF != out[j].RRF {
			return out[i].RRF > out[j].RRF
		}
		return out[i].RunID < out[j].RunID
	})
	return out
}

// dialogueHybridHit 是融合后的一条候选：回查原 run 的稳定坐标（session/run/定稿时间）
// + 脱敏原文 + 两臂名次与分数。Rank 0 = 不在该臂。
type dialogueHybridHit struct {
	RunID      string
	SessionID  string
	Passage    string
	FinishedAt *time.Time

	RRFScore float64
	Sources  []string

	LexRank  int
	LexScore float64
	VecRank  int
	VecScore float64
}

// dialogueHybridFuse 把两臂命中按 RRF 融合，返回按 rrfScore 降序 / runID 升序的候选。
// 同名 run 两臂都出现时，坐标与原文两臂内容一致（同一份 passage 写进两个目标），
// 取词法臂的为准，向量臂只补名次与分数；只在向量臂出现时用向量臂的。
func dialogueHybridFuse(lex []dialogueLexHit, vec []dialogueVecHit) []dialogueHybridHit {
	lexIDs := make([]string, len(lex))
	for i, h := range lex {
		lexIDs[i] = h.RunID
	}
	vecIDs := make([]string, len(vec))
	for i, h := range vec {
		vecIDs[i] = h.RunID
	}
	merged := dialogueRRFMerge(lexIDs, vecIDs)

	byID := make(map[string]*dialogueHybridHit, len(merged))
	for i, h := range lex {
		byID[h.RunID] = &dialogueHybridHit{
			RunID: h.RunID, SessionID: h.SessionID, Passage: h.Passage, FinishedAt: h.FinishedAt,
			LexRank: i + 1, LexScore: h.Score,
		}
	}
	for i, h := range vec {
		if e, ok := byID[h.RunID]; ok {
			e.VecRank, e.VecScore = i+1, h.Score
			if e.FinishedAt == nil {
				e.FinishedAt = h.FinishedAt
			}
			continue
		}
		byID[h.RunID] = &dialogueHybridHit{
			RunID: h.RunID, SessionID: h.SessionID, Passage: h.Passage, FinishedAt: h.FinishedAt,
			VecRank: i + 1, VecScore: h.Score,
		}
	}

	out := make([]dialogueHybridHit, 0, len(merged))
	for _, m := range merged {
		h := byID[m.RunID]
		h.RRFScore = m.RRF
		h.Sources = m.Sources
		out = append(out, *h)
	}
	return out
}

// dialogueContextNoteHybrid 是混合检索块的说明行（doc/84 §7.3 逐字）。
const dialogueContextNoteHybrid = "以下历史对话检索结果仅供参考，不代表完整历史，也不一定是当前事实。"

// renderDialogueHybridBlock 渲染 hybrid 的 <dialogue-context> 块（doc/84 §7.3）：
// 每条引用带日期、会话 id、run id 与脱敏原文片段；最多 dialogueRecallItemsMax 条、
// 每条 passage 截 dialogueRecallRuneCap rune；原文只转义防块边界伪造，不总结不改写。
// Milvus 零命中时调用方根本不会走到这里（候选为空 = 整块不生成）。
func renderDialogueHybridBlock(hits []dialogueHybridHit) (string, int) {
	if len(hits) == 0 {
		return "", 0
	}
	var b strings.Builder
	b.WriteString("<dialogue-context>\n")
	b.WriteString(dialogueContextNoteHybrid)
	b.WriteString("\n")
	n := 0
	for _, h := range hits {
		if n >= dialogueRecallItemsMax {
			break
		}
		line := dialogueHybridCitationLine(h)
		if line == "" {
			continue // 整条 passage 只剩坐标头：没有可引用的原文，跳过
		}
		b.WriteString(line)
		n++
	}
	if n == 0 {
		return "", 0
	}
	b.WriteString("</dialogue-context>")
	return b.String(), n
}

// dialogueHybridCitationLine 拼一条引用行：`- (日期, 会话 sid, run rid) 原文片段`。
// 日期取 run 定稿时间（与 pg_search 渲染同款 2006-01-02；零值时间渲染空串，
// 不让模型对着 0001-01-01 猜）。
func dialogueHybridCitationLine(h dialogueHybridHit) string {
	passage := clipMemoryText(dialogueHybridCitationPassage(h.Passage), dialogueRecallRuneCap)
	if strings.TrimSpace(passage) == "" {
		return ""
	}
	date := ""
	if h.FinishedAt != nil {
		date = h.FinishedAt.Format("2006-01-02")
	}
	return "- (" + date + ", 会话 " + string(h.SessionID) + ", run " + h.RunID + ") " +
		escapeRecallHTML(passage) + "\n"
}

// dialogueHybridCitationPassage 去掉 passage 首行的坐标头「[会话 x · 场 y]」：
// 引用前缀已经带 session/run，坐标不重复贴一遍；其余原文一字不动（这不是改写，
// 只是去重）。没有坐标头的 passage 原样返回。
func dialogueHybridCitationPassage(p string) string {
	if !strings.HasPrefix(p, "[会话 ") {
		return p
	}
	i := strings.IndexByte(p, '\n')
	if i < 0 {
		return "" // 整条只剩坐标头
	}
	return p[i+1:]
}
