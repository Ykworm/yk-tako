// 本文件：同会话历史查找（doc/21）——search_history 工具 + 词模式读路径。
//
// 病灶（doc/21 §1）：用户在同一个会话里追问更早一轮的原文。那一行定稿在 runlog_entries
// （页面刷新仍看得见），但对话变长后折叠把它从模型输入里压掉、换上摘要——摘要没抄那行、
// 模型也没调 memory 记笔记。用户再发一条「上次那个错误码是什么」，模型的三样输入
// （系统提示 + 摘要 + 窗口内近期行）里都没有它，只能说不记得或编一个。
//
// 端到端链路（每环可点名）：用户同会话再发一条 → 模型调 search_history(query=原文里的词)
// → 程序从 ctx 的 noteBox 取 (session, run)——工具参数里没有会话号，模型传别的 sessionId
// 也翻不到别人 → 只读 runlog_entries：session 等于本场、run_id 排除本场（本场 user 行抢闸时
// 已入库，不排除会把问句自己挤进名额；本场工具行 EndRun 才入库，查询时本就不在）、role 在
// 名单里、每个词一个 ILIKE 条件同时成立、seq 从新到旧取 limit 条 → 每行以第一个词首次出现处
// 为中心切前后各 200 字的片段 → 拼成「命中 N 条。…」文本进当轮 tool_result → 模型读出错误码、
// 答用户。片段只进当轮工具结果，下一场的系统提示不因此变长。
//
// 有界性（doc/21 §3.2）：片段化 + 4000 字总帽不是锦上添花——没有它们，一次查询把刚折掉的
// 正文原样灌回窗口，下一轮又触发折叠，这把工具就变成折叠的抽水机。
//
// 数据边界（doc/21 §3）：历史唯一真源 = runlog_entries，工具只读它（不写、也不读
// run_model_messages）。它解决的是「下一场来问」：本场中途刚被折掉、还没提交的那段拿不到。
//
// 两种模式（doc/21 §3.3）：默认按词查（query）；around_seq > 0 时不看 query 和 roles，
// 改取 seq 落在 [around_seq − window, around_seq + window] 的行（含自己、不排除本场——
// 相邻的 user 行 / tool_call 行就是上下文），每行从行首起最多 800 字。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// 校验线与钳制（doc/21 §3.2/§3.3 / planning L1、L3：limit / window 超上限钳到上限，不报错；
// 「字」= rune）。
const (
	searchQueryWordsMax   = 8    // 词数上限
	searchQueryWordRunes  = 200  // 单词语长上限
	searchLimitDefault    = 5    // limit 不传 = 5
	searchLimitMax        = 10   // limit 上限（钳）
	searchSnippetHalf     = 200  // 词模式片段：命中点向前、向后各取 200 字
	searchResultCap       = 4000 // 命中块 / 窗口块拼接串的 rune 帽（L4：首行总结与截断行不计）
	searchWindowDefault   = 2    // window 不传 = 2
	searchWindowMax       = 5    // window 上限（钳）
	searchWindowLineRunes = 800  // 窗口模式每行从行首起最多取的字数
)

// 默认 roles：tool_call 不搜——它的 text 是 name(args)，路径多半已在 fileOps 块里
// （doc/21 §3.1）；要看调用参数得显式写上 tool_call。
var searchDefaultRoles = []string{"user", "assistant", "tool_result"}

// searchRoleSet 是 roles 取值域 = runlog 的四个 role；名单外的值返回非法句、不查库。
var searchRoleSet = map[string]bool{
	"user": true, "assistant": true, "tool_call": true, "tool_result": true,
}

// 固定结果句（逐字照 doc/21；业务校验与空结果都是普通工具结果、不经 Go error 通道——planning L9）。
const (
	searchQueryEmptyText    = "query 为空。传入原文里会原样出现的词。"
	searchQueryWordLongText = "有一个词超过 200 字。"
	searchNoHitText         = "没有命中。"
	searchTruncatedText     = "其余命中已截断。"
	// searchRolesInvalidText：doc 未钉字面，动工时定稿（planning §3-D 建议稿原样采纳）。
	searchRolesInvalidText = "错误：roles 只认 user、assistant、tool_call、tool_result，用逗号分隔。"
)

func searchQueryTooManyText(n int) string {
	return fmt.Sprintf("query 拆出 %d 个词，最多 %d 个。", n, searchQueryWordsMax)
}

// searchNoSeqText：窗口模式的负路径——本会话没有这个 seq（planning L9：普通结果文本）。
func searchNoSeqText(seq int64) string {
	return fmt.Sprintf("本会话没有 seq=%d。", seq)
}

// searchHistoryDesc 是工具说明（逐字照 doc/21 §3.1，不改写）：什么时候该用写在 schema 上，
// Instruction 的字节不动。
const searchHistoryDesc = `在本会话已经落库的历史里按词查找原文。摘要和笔记里都没有、用户在问本会话更早一轮的错误码、原句或工具返回中的一截时使用。query 传原文里会原样出现的词；多个词用空格分开，必须同时出现。around_seq 传上一次结果里的 seq，用来看相邻几行。不要用它读文件的当前内容，也不要把查到的长文写进 memory。`

// searchHistoryInput 是 search_history 的入参（参数名与表述照 doc/21 §3.1 参数表）。
// 全部字段 omitempty（schema 层非必填）：两种模式各带各的参数，schema 层必填会让另一模式
// 被 provider 直接拒（同 memory 先例）。
// roles 的描述用 jsonschema_description 独立标签：描述里的 ASCII 逗号（user,assistant,
// tool_result）在 jsonschema 标签里会被 splitOnUnescapedCommas 切成两个选项，`\,` 转义
// 又过不了 go vet 的 structtag 检查——独立标签不走逗号切分，是库里给带逗号描述的出口。
type searchHistoryInput struct {
	Query     string `json:"query,omitempty" jsonschema:"description=要找的词。空格分开。around_seq 大于 0 时忽略"`
	AroundSeq int64  `json:"around_seq,omitempty" jsonschema:"description=上一次结果里的 seq。大于 0 时改看这一行的相邻行"`
	Limit     int    `json:"limit,omitempty" jsonschema:"description=命中条数。不传是 5，最大 10。只在按词查找时用"`
	Window    int    `json:"window,omitempty" jsonschema:"description=around_seq 左右各取几行。不传是 2，最大 5"`
	Roles     string `json:"roles,omitempty" jsonschema_description:"逗号分隔。不传是 user,assistant,tool_result。只在按词查找时用"`
}

// newSearchHistoryTool 造 search_history 工具。身份从 ctx 的 noteBox 取（session/run）——
// 缺 noteBox = Go error（wrapToolErrors 包成「错误：…」工具结果），不查库（照 memory 先例）。
// 业务校验（词三句 / roles 非法）与空结果都是普通结果文本（planning L8 顺序：空 → 超 8 词 →
// 有词超 200 字 → roles → 查库）；around_seq > 0 时跳过词与 roles 的全部校验（含空 query），
// 直接窗口模式（doc/21 §3.3）。底座故障（DB 断）上抛给包装层兜底。
func newSearchHistoryTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("search_history", searchHistoryDesc, func(ctx context.Context, in searchHistoryInput) (string, error) {
		box := noteBoxFrom(ctx)
		if box == nil {
			return "", errors.New("search_history 工具缺 run 上下文（noteBox 无 session/run）")
		}
		if in.AroundSeq > 0 {
			window := in.Window
			if window <= 0 {
				window = searchWindowDefault
			}
			if window > searchWindowMax {
				window = searchWindowMax
			}
			ok, err := store.runlogHasSeq(ctx, box.session, in.AroundSeq)
			if err != nil {
				return "", err
			}
			if !ok {
				return searchNoSeqText(in.AroundSeq), nil
			}
			rows, err := store.searchRunlogWindow(ctx, box.session, in.AroundSeq-int64(window), in.AroundSeq+int64(window))
			if err != nil {
				return "", err
			}
			return renderSearchWindow(rows, in.AroundSeq, window), nil
		}
		words := strings.Fields(in.Query)
		switch {
		case len(words) == 0:
			return searchQueryEmptyText, nil
		case len(words) > searchQueryWordsMax:
			return searchQueryTooManyText(len(words)), nil
		}
		for _, w := range words {
			if utf8.RuneCountInString(w) > searchQueryWordRunes {
				return searchQueryWordLongText, nil
			}
		}
		roles, ok := parseSearchRoles(in.Roles)
		if !ok {
			return searchRolesInvalidText, nil
		}
		limit := in.Limit
		if limit <= 0 {
			limit = searchLimitDefault
		}
		if limit > searchLimitMax {
			limit = searchLimitMax
		}
		patterns := make([]string, len(words))
		for i, w := range words {
			patterns[i] = ilikePattern(w)
		}
		hits, err := store.searchRunlog(ctx, box.session, box.run, roles, patterns, limit)
		if err != nil {
			return "", err
		}
		return renderSearchHits(hits, words[0]), nil
	})
}

// parseSearchRoles 解析 roles 参数：空 = 默认三档；按逗号拆、逐项去空白，任一项不在四个
// role 取值域内 → ok=false（调用方返回非法句、不查库）。
func parseSearchRoles(s string) ([]string, bool) {
	if strings.TrimSpace(s) == "" {
		return searchDefaultRoles, true
	}
	items := strings.Split(s, ",")
	roles := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if !searchRoleSet[it] {
			return nil, false
		}
		roles = append(roles, it)
	}
	return roles, true
}

// ilikePattern 构造一个词的 ILIKE 模式：`%` + 词 + `%`（SQL 侧配 ESCAPE '\'）。
// 词里的 `\`、`%`、`_` 先加反斜杠转义——不转义时模型/用户传入的 `%` 会变成通配符，
// 一条查询扫出整段历史。
func ilikePattern(word string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range word {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

// searchSnippet 切词模式片段：以第一个词在该行 text 里第一次出现的位置（rune 下标）为中心，
// 向前、向后各取 searchSnippetHalf 字；行比窗口短 = 整行返回（无省略标记）。切断处独占一行
// 标 `……（前略）` / `……（后略）`（doc/21 §3.2）。
// 定位大小写不敏感（L2）：SQL 的 ILIKE 大小写不敏感而 Go 的 Index 敏感——两边 ToLower 再定位，
// 否则问句写 e_deck_17 时 SQL 命中、这里定不到中心。ILIKE 命中而 ToLower 定位不到的
// 理论失配（大小写表差异）按 pos=0 兜底：退回行首窗口，仍有界、不炸。
func searchSnippet(text, word string) string {
	runes := []rune(text)
	pos := runeIndexFold(runes, word)
	if pos < 0 {
		pos = 0
	}
	start := pos - searchSnippetHalf
	if start < 0 {
		start = 0
	}
	end := pos + searchSnippetHalf
	if end > len(runes) {
		end = len(runes)
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("……（前略）\n")
	}
	b.WriteString(string(runes[start:end]))
	if end < len(runes) {
		b.WriteString("\n……（后略）")
	}
	return b.String()
}

// runeIndexFold 在 hay 里找 needle 第一次出现的位置（大小写不敏感），返回 rune 下标（-1 = 没找到）。
// Go 的 ToLower 是逐 rune 简单映射（不改变 rune 数），byte 下标换算回 rune 下标安全。
func runeIndexFold(hay []rune, needle string) int {
	if needle == "" {
		return 0
	}
	lower := strings.ToLower(string(hay))
	i := strings.Index(lower, strings.ToLower(needle))
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(lower[:i])
}

// renderSearchHits 渲染词模式结果（doc/21 §3.2 样本）：首行「命中 N 条。范围：…」+ `---` 分隔的
// 命中块（新到旧；头行 seq/role/agent，agent 空 = 主代理、等号后留空）+ 片段。
// 全部命中块拼接串超 searchResultCap（L4：只计块拼接串，首行总结与截断行不计）→ 从末位
// （最旧）整块丢起，末尾加「其余命中已截断。」——一次工具结果不能把刚折掉的正文原样灌回窗口。
// 0 条 = 整段结果就是「没有命中。」。
func renderSearchHits(hits []RunlogEntry, firstWord string) string {
	if len(hits) == 0 {
		return searchNoHitText
	}
	blocks := make([]string, len(hits))
	for i, h := range hits {
		blocks[i] = fmt.Sprintf("seq=%d role=%s agent=%s\n%s", h.Seq, h.Role, h.Agent, searchSnippet(h.Text, firstWord))
	}
	// 单块永不超过帽（片段有界在上游保证），len>1 的守卫防病态输入产出空结果。
	truncated := false
	for len(blocks) > 1 && utf8.RuneCountInString(strings.Join(blocks, "\n---\n")) > searchResultCap {
		blocks = blocks[:len(blocks)-1]
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "命中 %d 条。范围：本会话里本场 run 之前已提交的行。", len(blocks))
	for _, blk := range blocks {
		b.WriteString("\n---\n")
		b.WriteString(blk)
	}
	if truncated {
		b.WriteString("\n" + searchTruncatedText)
	}
	return b.String()
}

// windowLine 渲染窗口模式的一行：从行首起最多 searchWindowLineRunes 字——超出时截断、
// 在片段后独占一行写 `……（后略）`（与词模式的后略标记同一视觉语言）。
// 从行首起取（不是命中点居中）：窗口模式的用途是看上下文，行首就是模型的阅读起点（doc/21 §3.3）。
func windowLine(text string) string {
	runes := []rune(text)
	if len(runes) <= searchWindowLineRunes {
		return text
	}
	return string(runes[:searchWindowLineRunes]) + "\n……（后略）"
}

// renderSearchWindow 渲染窗口模式结果（doc/21 §3.3 样本）：首行「seq=N 的窗口，前后各 W 行。」+
// `---` 分隔的窗口块（seq 升序；头行与词模式同形）。
// 总帽 searchResultCap（L4 同口径：只计块拼接串）→ 从离 around_seq 最远的行丢起（L5：
// 距离相同先丢 seq 较大者，确定性序），末尾加「其余命中已截断。」。
// rows 为空是理论不可达（存在性前置已保证 seq=N 自己落在窗口里）——按负路径兜底。
func renderSearchWindow(rows []RunlogEntry, aroundSeq int64, window int) string {
	if len(rows) == 0 {
		return searchNoSeqText(aroundSeq)
	}
	blocks := make([]string, len(rows))
	drops := make([]int, len(rows))
	for i, h := range rows {
		blocks[i] = fmt.Sprintf("seq=%d role=%s agent=%s\n%s", h.Seq, h.Role, h.Agent, windowLine(h.Text))
		drops[i] = i
	}
	sort.Slice(drops, func(a, b int) bool {
		da, db := absInt64(rows[drops[a]].Seq-aroundSeq), absInt64(rows[drops[b]].Seq-aroundSeq)
		if da != db {
			return da > db
		}
		return rows[drops[a]].Seq > rows[drops[b]].Seq
	})
	kept := make([]bool, len(rows))
	for i := range kept {
		kept[i] = true
	}
	joinKept := func() string {
		parts := make([]string, 0, len(blocks))
		for i, blk := range blocks {
			if kept[i] {
				parts = append(parts, blk)
			}
		}
		return strings.Join(parts, "\n---\n")
	}
	live, truncated := len(rows), false
	for _, d := range drops {
		if live <= 1 || utf8.RuneCountInString(joinKept()) <= searchResultCap {
			break
		}
		kept[d] = false
		live--
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seq=%d 的窗口，前后各 %d 行。", aroundSeq, window)
	for i, blk := range blocks {
		if !kept[i] {
			continue
		}
		b.WriteString("\n---\n")
		b.WriteString(blk)
	}
	if truncated {
		b.WriteString("\n" + searchTruncatedText)
	}
	return b.String()
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// searchRunlog 是 search_history 的读路径（词模式，只读 runlog_entries）：session 等值、
// run_id 排除本场（抢闸时入库的本场 user 行不能占名额）、role 在名单里、每个词一个 ILIKE
// 条件（全 AND）、seq 从新到旧取 limit 条。不建新索引（doc/21 §3.2：扫描量 = 本会话里本场
// 之前的行数，行数大到查询变慢再单独立项）。
func (s *Store) searchRunlog(ctx context.Context, sessionID SessionID, excludeRunID RunID, roles, patterns []string, limit int) ([]RunlogEntry, error) {
	q := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Where("session_id = ? AND run_id <> ?", sessionID, excludeRunID).
		Where("role IN ?", roles)
	for _, p := range patterns {
		q = q.Where(`text ILIKE ? ESCAPE '\'`, p)
	}
	var rows []RunlogEntry
	if err := q.Order("seq DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// runlogHasSeq 是窗口模式的前置存在性查询（§3-F 第三条 SQL）：本会话没有这个 seq 时
// 整段结果 = 「本会话没有 seq=N。」（planning L9），也就不必再查窗口。
func (s *Store) runlogHasSeq(ctx context.Context, sessionID SessionID, seq int64) (bool, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Where("session_id = ? AND seq = ?", sessionID, seq).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// searchRunlogWindow 是窗口模式的取数（§3-F 第二条 SQL）：本会话 seq 落在 [lo, hi] 的行、
// 按 seq 升序。**不排除本场 run**（doc/21 §3.3 字面）：调用方从上一笔结果拿到的 seq，
// 相邻的本场 user 行 / tool_call 行正是上下文。
func (s *Store) searchRunlogWindow(ctx context.Context, sessionID SessionID, lo, hi int64) ([]RunlogEntry, error) {
	var rows []RunlogEntry
	if err := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Where("session_id = ? AND seq BETWEEN ? AND ?", sessionID, lo, hi).
		Order("seq ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
