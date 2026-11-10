package runengine

// 本文件：pipeline 记忆的**读侧检索**（doc/65 §4.4）——BM25 召回查询、master 的
// search_memory 工具、as_of 现实回放、include_history 双向链。
//
// 与旧 ILIKE 召回（recall.go 旧实现，已随旧表结构退役）的三个区别：
//
//  1. **Go 侧零切词**：query 整句交给 pg_search 的 paradedb.tokenize（chinese_lindera），
//     切出的词以 boolean should（OR）组合；`@@@` 左边是键列 id，不是索引名。
//  2. **可见性走统一谓词**（memoryVisibilitySQL）：user 行跨项目可见、workspace 行只在本项目。
//     没有身份的调用（单测）退回启动配置的租户、放开 user 维度——生产路径总有身份。
//  3. **排序按 BM25 score**（再按证据时间、id），不是命中词数。
//
// **为什么不是 doc/65 初稿的 paradedb.match**：2026-11-10 实跑发现 lindera 把词间空格
// 切成 " " token 且索引收录它——match 整句 OR 匹配时，任何带空白的查询都命中所有含空格
// 的行（「zzzqqqxyz 无命中探针」命中全库）。所以查询侧先 tokenize、滤掉纯空白 token
// （`token ~ '\S'`），再 boolean should 组合。分词仍在扩展内做，OR 宽召回与 score 排序不变。
//
// BM25 是**词面**检索：「周末」不会命中「周六」。错误文案、注释与文档都不许把词法说成语义。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// searchMemoryToolName 是 master 的主动查询工具名（只挂 master；子代理在
// dropMemoryAndSearchTools 处剔掉）。
const searchMemoryToolName = "search_memory"

// searchMemoryChainDepth 是 include_history 沿 superseded_by 走链的深度上限
// （doc/65 §4.4：链遍历必须有 visited 集合与深度上限防环）。
const searchMemoryChainDepth = 8

// memoryBM25Row 是 BM25 命中的一行（score 只参与排序，不透出给模型）。
type memoryBM25Row struct {
	ID        string
	Scope     string
	Layer     string
	State     string
	Content   string
	Quote     string
	CreatedAt time.Time
	LeftAt    *time.Time `gorm:"column:left_active_at"`
	RemovedAt *time.Time
	ExpiresAt *time.Time
	// EvidenceAt 是证据时间（runlog_entries.created_at，按 source_run_id+source_seq 关联）。
	// 取不到证据的行是 NULL——排序放最后（NULLS LAST）。
	EvidenceAt *time.Time
	Score      float64
}

// memoryBM25Mode 区分两种查询谓词。
type memoryBM25Mode int

const (
	// memoryBM25Active 只查 active 且未过期（每回合召回与 search_memory 默认）。
	memoryBM25Active memoryBM25Mode = iota
	// memoryBM25AsOf 现实回放：现实区间覆盖 as_of 的行（含已 superseded/removed/archived
	// 的历史行——当时它们还在现实里）。
	memoryBM25AsOf
)

// bm25MemoryRows 是召回与 search_memory 共用的 BM25 查询（doc/65 §4.4）。
//
// ORDER BY score DESC, 证据时间 DESC, id DESC（doc/65 §4.4）；证据时间取不到的行
// 排在同分最后（NULLS LAST）。resident 不进普通检索——查询打 recall_items 视图。
func (s *Store) bm25MemoryRows(ctx context.Context, query string, limit int, mode memoryBM25Mode, asOf time.Time) ([]memoryBM25Row, error) {
	if limit <= 0 {
		limit = 10
	}
	// 身份两条通道（scope.go：dataScope 是笔记与长期记忆的读写范围）：HTTP 读口把身份放
	// dataScope（memoryContext），run 内 ctx 放 principal——谁在用谁，两通道在本仓库的路径上不相交。
	// scope=tenant 是管理员预览，放开 user 维度（与 memoryRecallFilter 同口径）。
	tenantID, userID, workspaceID := memoryVisibilityArgs(ctx)
	if sc := scopeFrom(ctx); scopeActive(sc) {
		tenantID = sc.TenantID
		switch {
		case sc.Scope == "tenant":
			userID = ""
		case sc.UserID != "":
			userID = sc.UserID
		}
	}
	if tenantID == "" {
		tenantID = s.memoryCfg.TenantID
	}
	// 可见性谓词：统一两分支；没有身份时放开 user 维度（与旧召回的无身份行为同形），
	// workspace 维度没有兜底来源——没身份就看不见 workspace 行。
	vis := `(tenant_id = ? AND ((scope = 'user' AND (? = '' OR user_id = ?)) OR (scope = 'workspace' AND workspace = ?)))`
	where := vis + `
	   AND (m.expires_at IS NULL OR m.expires_at > now())`
	switch mode {
	case memoryBM25AsOf:
		// 现实区间 = [COALESCE(valid_from, created_at), LEAST(expires_at, left_active_at))。
		// left_active_at 是「首次离开 active」的时刻——对被替代的行它就是后继行的现实起点，
		// 对被删除的行就是删除时刻（doc/65 §4.4：现实终点由 expires_at 与后继行的现实起点推导）。
		where += `
	   AND COALESCE(m.valid_from, m.created_at) <= ?
	   AND COALESCE(m.expires_at, 'infinity'::timestamptz) > ?
	   AND COALESCE(m.left_active_at, 'infinity'::timestamptz) > ?`
	default:
		where += ` AND m.state = 'active'`
	}
	args := []any{query, tenantID, userID, userID, workspaceID}
	if mode == memoryBM25AsOf {
		args = append(args, asOf, asOf, asOf)
	}
	args = append(args, limit)
	var rows []memoryBM25Row
	err := s.db.WithContext(ctx).Raw(`
SELECT m.id, m.scope, m.layer, m.state, m.content, m.quote, m.created_at,
       m.left_active_at, m.removed_at, m.expires_at,
       paradedb.score(m.id) AS score,
       (SELECT e.created_at FROM runlog_entries e
         WHERE e.run_id = m.source_run_id AND e.seq = m.source_seq
         ORDER BY e.created_at LIMIT 1) AS evidence_at
  FROM recall_items m
 WHERE m.id @@@ paradedb.boolean(should => ARRAY(
         SELECT paradedb.term('content', token)
           FROM paradedb.tokenize('`+memoryTokenizerJSON+`'::jsonb, ?::text)
          WHERE token ~ '\S'))
   AND `+where+`
 ORDER BY score DESC, evidence_at DESC NULLS LAST, m.id DESC
 LIMIT ?`, args...).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ── search_memory 工具（只挂 master）─────────────────────────────────────────

// searchMemoryDesc 是 search_memory 的说明文案。0 命中是正常结果——文案要明说，
// 不然模型会把「没查到」当成工具坏了。
const searchMemoryDesc = `按关键词检索长期记忆（recall 层；resident 层每场必见，不进检索）。
query 是检索词或短语（整句交给词法检索，不是语义搜索——换个说法的词可能查不到）。
0 命中是正常结果，说明没有相关记忆。`

// searchMemoryInput 是 search_memory 的入参。
type searchMemoryInput struct {
	Query          string `json:"query" jsonschema:"description=检索词或短语；整句交给词法检索"`
	AsOf           string `json:"as_of,omitempty" jsonschema:"description=RFC3339 时点；传了就查该时点仍成立的记忆（现实回放）"`
	IncludeHistory bool   `json:"include_history,omitempty" jsonschema:"description=true 时沿替代链取历史（含被替代/删除的行）"`
	Limit          int    `json:"limit,omitempty" jsonschema:"description=最多返回几条，默认 10，上限 20"`
}

// newSearchMemoryTool 造 master 的 search_memory 工具（doc/65 §4.4）。
func newSearchMemoryTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool(searchMemoryToolName, searchMemoryDesc, func(ctx context.Context, in searchMemoryInput) (string, error) {
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return "错误：query 是空的——没有检索词就没法查。", nil
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 20 {
			limit = 20
		}
		mode := memoryBM25Active
		var asOf time.Time
		if raw := strings.TrimSpace(in.AsOf); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return fmt.Sprintf("错误：as_of 解析不了（要 RFC3339，如 2026-11-10T00:00:00Z）：%q", raw), nil
			}
			mode = memoryBM25AsOf
			asOf = t
		}
		rows, err := store.bm25MemoryRows(ctx, query, limit, mode, asOf)
		if err != nil {
			return "", fmt.Errorf("检索记忆：%w", err)
		}
		if len(rows) == 0 {
			return "没有命中的记忆。", nil
		}
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(renderSearchMemoryRow(r))
			if in.IncludeHistory {
				chain, err := store.memoryHistoryChain(ctx, r.ID, searchMemoryChainDepth)
				if err != nil {
					return "", fmt.Errorf("取历史链：%w", err)
				}
				for _, h := range chain {
					b.WriteString("  └ " + renderSearchMemoryRow(h))
				}
			}
		}
		return b.String(), nil
	})
}

// renderSearchMemoryRow 渲染一条命中（带状态与日期，模型要能判断新旧）。
func renderSearchMemoryRow(r memoryBM25Row) string {
	var b strings.Builder
	b.WriteString("- [" + r.Layer + "/" + r.Scope + " · " + r.State + "] " + memoryItemDate(r.CreatedAt) + " " + r.Content)
	if r.Quote != "" {
		b.WriteString("（原话：" + clipMemoryText(r.Quote, memoryQuoteCap) + "）")
	}
	if r.State == MemoryStateSuperseded && r.LeftAt != nil {
		b.WriteString("（被替代于 " + memoryItemDate(*r.LeftAt) + "）")
	}
	if r.State == MemoryStateRemoved && r.RemovedAt != nil {
		b.WriteString("（删除于 " + memoryItemDate(*r.RemovedAt) + "）")
	}
	if r.ExpiresAt != nil {
		b.WriteString("（有效期至 " + memoryItemDate(*r.ExpiresAt) + "）")
	}
	b.WriteString("\n")
	return b.String()
}

// memoryHistoryChain 沿 superseded_by **双向**取链：后继方向（本行指向谁）与前驱方向
// （谁指向本行）。visited 集合防环、深度上限防长链（doc/65 §4.4）。跨层指针照走
// （可以走到常驻行），常驻行本身不参与 BM25 普通检索。
func (s *Store) memoryHistoryChain(ctx context.Context, rootID string, depth int) ([]memoryBM25Row, error) {
	visited := map[string]bool{rootID: true}
	var out []memoryBM25Row
	frontier := []string{rootID}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, id := range frontier {
			// 后继：本行 superseded_by 指向的行。
			var succID *string
			if err := s.db.WithContext(ctx).Raw(
				`SELECT superseded_by FROM memory_items WHERE id = ?`, id).Scan(&succID).Error; err != nil {
				return nil, err
			}
			if succID != nil && *succID != "" && !visited[*succID] {
				visited[*succID] = true
				var row memoryBM25Row
				if err := s.db.WithContext(ctx).Raw(`
SELECT id, scope, layer, state, content, quote, created_at, left_active_at, removed_at, expires_at
  FROM memory_items WHERE id = ?`, *succID).Scan(&row).Error; err != nil {
					return nil, err
				}
				if row.ID != "" {
					out = append(out, row)
					next = append(next, row.ID)
				}
			}
			// 前驱：superseded_by 指向本行的行。
			var preds []memoryBM25Row
			if err := s.db.WithContext(ctx).Raw(`
SELECT id, scope, layer, state, content, quote, created_at, left_active_at, removed_at, expires_at
  FROM memory_items WHERE superseded_by = ?`, id).Scan(&preds).Error; err != nil {
				return nil, err
			}
			for _, p := range preds {
				if visited[p.ID] {
					continue
				}
				visited[p.ID] = true
				out = append(out, p)
				next = append(next, p.ID)
			}
		}
		frontier = next
	}
	return out, nil
}

// dropMemoryAndSearchTools 从子代理入口快照里剔掉 memory 与 search_memory 两个工具
// （doc/65 §4.5/§4.4：普通 subagent 不得继承——子代理只读当场冻结的快照）。
//
// 剔在 base（继承档 / exclude_tools 档 / tools 白名单档三档的共同来源），一份过滤管三条路。
func dropMemoryAndSearchTools(ctx context.Context, tools []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			out = append(out, t) // 拿不到名的原样保留：过滤不该顺手改装配结果
			continue
		}
		if info.Name == memoryToolName || info.Name == searchMemoryToolName {
			continue
		}
		out = append(out, t)
	}
	return out
}
