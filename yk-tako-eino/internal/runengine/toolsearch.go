// 本文件：工具目录分级（T4）与 BM25 检索工具（T5，2026-10-20 合批施工）。
//
// 分级规则：`mcp__` 前缀 = deferred，其余（一手工具、委派工具、tool_search 自己）= direct。
// direct 每次模型调用都声明；deferred 只有被 tool_search 激活后才进声明面。
// 可执行 ⊇ 声明 ⊇ 可检索：deferred 工具即使没声明也能被按名调用（模型猜名），
// 只是 schema 看不见——声明面收窄，执行面不动。
//
// 为什么声明面必须从「完整目录」重建、不能对当前 state.ToolInfos 做减法：
// state.ToolInfos 在首轮 BeforeModelRewriteState 之后会被持久化进 compose state，
// 下一次调用拿到的是上一次过滤后的列表——deferred 项首轮就没了，减法只会越减越少。
// 所以 box 在 BeforeAgent 里留一份完整目录（含每把工具的 ToolInfo），每次调用前
// 用「非 mcp__ 全留 + 激活集里的 mcp__」重新拼一份。box 挂 run-local ctx，不放 handler
// 字段——handler 实例跨 run 共用，字段就成了串场状态；tool_search 工具实例每 run 新建、
// 闭包持有本 run 的 box。
//
// BM25 照 pi 的 tool-search 实现移植（k1=1.2 / b=0.75 / 默认 limit 8 / 同分保文档序）：
// 文档 = 名字 + 名字（下划线换空格）+ 描述 + schema 里的属性名与描述（递归）。
// 分词 = camelCase 拆词 + 非字母数字切分 + 停用词过滤 + 粗粒度去复数。中文不进词表
// （与 pi 相同的已知边界），MCP 工具名与描述是英文，首版够用。
//
// 激活集寿命 = 只活一场 run（2026-10-20 已拍）：新 run / 崩溃续跑都从零激活，
// 回执固定写 `Loaded N tool(s) for this run...`，不承诺跨 run。
//
// 子代理（A2）：规格显式写 tools: [tool_search, ...] 时，agentSpecHandler 给该子代理的
// handler 链也挂一个 toolSearchHandler——每条规格自己的 box/完整目录/激活集，tool_search
// 只搜自己的池。继承档 / exclude_tools 档不自动加（已拍），其工具全部 direct 声明。
// resolveToolPool 把保留名 tool_search 跳过：真工具由本 handler 在子代理 BeforeAgent 末尾装入。
package runengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

const (
	toolSearchToolName     = "tool_search"
	deferredToolPrefix     = "mcp__"
	defaultToolSearchLimit = 8
	bm25K1                 = 1.2
	bm25B                  = 0.75
)

// toolSearchBox 是一场 agent run 的工具目录与激活集。
// catalog 含全部工具的 ToolInfo（direct + deferred，按装配顺序）；docs 只含 deferred 的检索文档；
// active 是已激活的 deferred 名字集。ToolSearch 工具会并发调用（Eino ToolsNode 默认并行），
// 读写都要锁。
type toolSearchBox struct {
	mu       sync.RWMutex
	catalog  []*schema.ToolInfo
	deferred map[string]struct{}
	docs     []toolSearchDoc
	active   map[string]struct{}
}

func newToolSearchBox() *toolSearchBox {
	return &toolSearchBox{
		deferred: make(map[string]struct{}),
		active:   make(map[string]struct{}),
	}
}

// add 收一把工具进完整目录；deferred 项另建检索文档。
func (b *toolSearchBox) add(info *schema.ToolInfo) error {
	if info == nil || info.Name == "" {
		return fmt.Errorf("工具目录项缺少名字")
	}
	b.catalog = append(b.catalog, info)
	if isDeferredTool(info.Name) {
		b.deferred[info.Name] = struct{}{}
		b.docs = append(b.docs, newToolSearchDoc(info))
	}
	return nil
}

// declaration 用完整目录重建本次模型调用的声明面，并返回观测用的名单。
func (b *toolSearchBox) declaration() (declared []*schema.ToolInfo, active, inactive []string, deferred int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	deferred = len(b.deferred)
	declared = make([]*schema.ToolInfo, 0, len(b.catalog))
	for _, info := range b.catalog {
		if _, d := b.deferred[info.Name]; d {
			if _, ok := b.active[info.Name]; !ok {
				inactive = append(inactive, info.Name)
				continue
			}
			active = append(active, info.Name)
		}
		declared = append(declared, info)
	}
	sort.Strings(active)
	sort.Strings(inactive)
	return declared, active, inactive, deferred
}

// search 在未激活的 deferred 工具里做 BM25 检索，命中的名字写进激活集。
// 已激活的工具不再出现在候选里（pi 同语义：重复搜索不会重复加载）。
func (b *toolSearchBox) search(query string, limit int) []toolSearchHit {
	b.mu.Lock()
	defer b.mu.Unlock()
	candidates := make([]toolSearchDoc, 0, len(b.docs))
	descriptions := make(map[string]string, len(b.docs))
	for _, doc := range b.docs {
		if _, ok := b.active[doc.name]; ok {
			continue
		}
		candidates = append(candidates, doc)
		descriptions[doc.name] = doc.description
	}
	matches := rankToolSearch(query, candidates, limit)
	hits := make([]toolSearchHit, 0, len(matches))
	for _, match := range matches {
		b.active[match.Name] = struct{}{}
		hits = append(hits, toolSearchHit{Name: match.Name, Description: descriptions[match.Name]})
	}
	return hits
}

// toolSearchHandler 挂两个钩子：BeforeAgent 建目录并按 box 造 tool_search；
// BeforeModelRewriteState 每次调模型前重建 state.ToolInfos。
type toolSearchHandler struct {
	*adk.BaseChatModelAgentMiddleware
}

func newToolSearchHandler() *toolSearchHandler {
	return &toolSearchHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

type toolSearchBoxKey struct{}

func (h *toolSearchHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	box := newToolSearchBox()
	for _, t := range runCtx.Tools {
		info, err := t.Info(ctx)
		if err != nil {
			return ctx, runCtx, fmt.Errorf("toolsearch: 工具目录读取失败: %w", err)
		}
		if err := box.add(info); err != nil {
			return ctx, runCtx, fmt.Errorf("toolsearch: 工具目录读取失败: %w", err)
		}
	}
	ts, err := newToolSearchTool(box)
	if err != nil {
		return ctx, runCtx, fmt.Errorf("toolsearch: 装配 %s 失败: %w", toolSearchToolName, err)
	}
	info, err := ts.Info(ctx)
	if err != nil {
		return ctx, runCtx, fmt.Errorf("toolsearch: 装配 %s 失败: %w", toolSearchToolName, err)
	}
	// tool_search 自己是 direct：进目录并排在装配序末尾，与 runCtx.Tools 的顺序一致。
	if err := box.add(info); err != nil {
		return ctx, runCtx, fmt.Errorf("toolsearch: 装配 %s 失败: %w", toolSearchToolName, err)
	}
	// 与其余一手工具同一条错误回注语义：工具错误变成 `错误：...` 结果，run 继续。
	runCtx.Tools = append(runCtx.Tools, wrapToolErrors(ts))
	log.Printf("toolsearch: catalog=%d deferred=%d", len(box.catalog), len(box.deferred))
	return context.WithValue(ctx, toolSearchBoxKey{}, box), runCtx, nil
}

func (h *toolSearchHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	box, ok := ctx.Value(toolSearchBoxKey{}).(*toolSearchBox)
	if !ok || box == nil {
		return ctx, state, nil
	}
	declared, active, inactive, deferred := box.declaration()
	state.ToolInfos = declared
	log.Printf("toolsearch: declared=%d deferred=%d active=%v inactive=%v", len(declared), deferred, active, inactive)
	return ctx, state, nil
}

// toolSearchInput 是 tool_search 的入参：query 必填，limit 缺省 8。
type toolSearchInput struct {
	Query string `json:"query" jsonschema:"description=搜索关键词（工具名、描述或参数里的词）"`
	Limit *int   `json:"limit,omitempty" jsonschema:"description=最多返回几把工具，缺省 8"`
}

const toolSearchToolDesc = "搜索本 agent 尚未直接声明的工具（例如 MCP 服务器的工具）。给出关键词，" +
	"命中工具的 schema 会在本 run 的下一次模型调用里可见，随后就能调用；已加载的工具不会重复返回。中文关键词不保证命中，优先用工具名里的英文词。"

func newToolSearchTool(box *toolSearchBox) (tool.InvokableTool, error) {
	return utils.InferTool(toolSearchToolName, toolSearchToolDesc,
		func(_ context.Context, in toolSearchInput) (string, error) {
			query := strings.TrimSpace(in.Query)
			if query == "" {
				return "", fmt.Errorf("query 不能为空")
			}
			limit := defaultToolSearchLimit
			if in.Limit != nil {
				limit = *in.Limit
			}
			if limit <= 0 {
				return "", fmt.Errorf("limit 必须是正整数")
			}
			hits := box.search(query, limit)
			names := make([]string, 0, len(hits))
			for _, hit := range hits {
				names = append(names, hit.Name)
			}
			log.Printf("toolsearch: query=%q loaded=%v", query, names)
			if len(hits) == 0 {
				return "No matching tools found.", nil
			}
			noun := "tools"
			if len(hits) == 1 {
				noun = "tool"
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Loaded %d %s for this run:", len(hits), noun)
			for _, hit := range hits {
				fmt.Fprintf(&b, "\n- %s: %s", hit.Name, firstLine(hit.Description))
			}
			return b.String(), nil
		})
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

func isDeferredTool(name string) bool {
	return strings.HasPrefix(name, deferredToolPrefix)
}

// toolSearchDoc 是一把 deferred 工具的检索文档。
type toolSearchDoc struct {
	name        string
	description string
	text        string
}

type toolSearchHit struct {
	Name        string
	Description string
}

// newToolSearchDoc 拼检索文本：名字、名字（下划线换空格）、描述、schema 里的属性名与描述。
// 名字出现两次是照 pi：工具名是强信号，重复一次提高权重。
func newToolSearchDoc(info *schema.ToolInfo) toolSearchDoc {
	parts := []string{info.Name, strings.ReplaceAll(info.Name, "_", " "), info.Desc}
	if info.ParamsOneOf != nil {
		if js, err := info.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
			if raw, err := json.Marshal(js); err == nil {
				var schemaText any
				if err := json.Unmarshal(raw, &schemaText); err == nil {
					collectToolSchemaText(schemaText, &parts)
				}
			}
		}
	}
	return toolSearchDoc{
		name:        info.Name,
		description: info.Desc,
		text:        strings.Join(parts, " "),
	}
}

// collectToolSchemaText 递归收 schema 里的 description / properties 名 / items / 组合子。
func collectToolSchemaText(node any, parts *[]string) {
	obj, ok := node.(map[string]any)
	if !ok {
		return
	}
	if desc, ok := obj["description"].(string); ok && strings.TrimSpace(desc) != "" {
		*parts = append(*parts, desc)
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			*parts = append(*parts, name)
			collectToolSchemaText(props[name], parts)
		}
	}
	collectToolSchemaText(obj["items"], parts)
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := obj[key].([]any); ok {
			for _, variant := range variants {
				collectToolSchemaText(variant, parts)
			}
		}
	}
}

var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {}, "by": {},
	"for": {}, "from": {}, "in": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {},
	"that": {}, "the": {}, "this": {}, "to": {}, "with": {},
}

var (
	camelLowerUpper = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	camelUpperWord  = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
)

// tokenize 照 pi：拆 camelCase、非字母数字切分、转小写、去停用词、粗粒度去复数。
func tokenize(text string) []string {
	text = camelLowerUpper.ReplaceAllString(text, "$1 $2")
	text = camelUpperWord.ReplaceAllString(text, "$1 $2")
	text = strings.ToLower(text)
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	terms := make([]string, 0, len(raw))
	for _, term := range raw {
		if _, stop := stopWords[term]; stop {
			continue
		}
		terms = append(terms, stem(term))
	}
	return terms
}

func stem(term string) string {
	switch {
	case len(term) > 4 && strings.HasSuffix(term, "ies"):
		return term[:len(term)-3] + "y"
	case len(term) > 4 && (strings.HasSuffix(term, "ches") || strings.HasSuffix(term, "shes") ||
		strings.HasSuffix(term, "sses") || strings.HasSuffix(term, "xes") || strings.HasSuffix(term, "zes")):
		return term[:len(term)-2]
	case len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss"):
		return term[:len(term)-1]
	default:
		return term
	}
}

// rankToolSearch 是 Okapi BM25；同分保文档顺序（sort.SliceStable）。
func rankToolSearch(query string, docs []toolSearchDoc, limit int) []toolSearchMatch {
	queryTerms := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for _, term := range tokenize(query) {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		queryTerms = append(queryTerms, term)
	}
	if len(queryTerms) == 0 || len(docs) == 0 || limit <= 0 {
		return nil
	}

	counts := make([]map[string]int, len(docs))
	lengths := make([]int, len(docs))
	totalLength := 0
	for i, doc := range docs {
		count := make(map[string]int)
		for _, term := range tokenize(doc.text) {
			count[term]++
			lengths[i]++
		}
		counts[i] = count
		totalLength += lengths[i]
	}
	averageLength := float64(totalLength) / float64(len(docs))
	if averageLength == 0 {
		averageLength = 1
	}

	docCount := float64(len(docs))
	idf := make(map[string]float64, len(queryTerms))
	for _, term := range queryTerms {
		frequency := 0
		for _, count := range counts {
			if count[term] > 0 {
				frequency++
			}
		}
		idf[term] = math.Log(1 + (docCount-float64(frequency)+0.5)/(float64(frequency)+0.5))
	}

	matches := make([]toolSearchMatch, 0, len(docs))
	for i, doc := range docs {
		score := 0.0
		for _, term := range queryTerms {
			count := counts[i][term]
			if count == 0 {
				continue
			}
			norm := bm25K1 * (1 - bm25B + bm25B*float64(lengths[i])/averageLength)
			score += idf[term] * (float64(count) * (bm25K1 + 1) / (float64(count) + norm))
		}
		if score > 0 {
			matches = append(matches, toolSearchMatch{Name: doc.name, Score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

type toolSearchMatch struct {
	Name  string
	Score float64
}
