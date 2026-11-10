package runengine

// 本文件：pipeline 模式的**统一抽取器**（doc/65 §4.7）。
//
// 它是「Redpanda 消费者调用的专用后台提取器」：由程序编排多次模型 Generate 与最后一次写库。
// **不是**普通 coder/researcher 子代理 —— 不创建 .tako/subagents/ 档案，不依赖 doc/70 的内部 Task。
//
// 它为什么不是一个「记一笔」的按钮：记住 / 改口 / 明确删除这三件事的判定，
// 需要**比全部当前记忆**才能做。只有 master 看到的那点上下文，它不知道哪条旧记忆
// 被这次的话取代了。所以流程是三段（doc/65 §4.7）：
//
//	拿到 job → ①从输入抽新条目 → ②候选全量分批、每批与新条目比较（模型只回序号）
//	→ ③新条目 + 全部相关候选做最终裁决 → 一个事务里写库
//
// 三条硬要求：
//
//  1. **候选全量**：可见域的 active resident + recall 全部进候选表，**不设 150 行上限**
//     （否则第 151 条之后永远没机会参与）。用分批解决「一次塞不下」。
//  2. **未扫完不写**：所有批次跑完才进最终裁决；任何一批失败 → 整 job 重试，
//     不截断、不随机挑一批、不先写一半。相关集合超预算同样明确失败。
//  3. **模型只给候选序号**：不自由填写数据库 id（模型编 id = 改错别人的记忆）。
//     序号 → 真实 id 的映射由程序掌握。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 抽取器的固定口径（doc/65 §4.7/§4.8；施工包 §11）。
const (
	// memoryBatchTokens 是一批候选的 token 预算（输入侧）。
	memoryBatchTokens = 3000
	// memoryFinalTokens 是最终合并调用的候选侧预算：**相关候选**渲染超过它 → 明确失败
	// （doc/65 §4.7：不截断、不随机选一批、不先写一半）。
	memoryFinalTokens = 12000
	// memoryMaxActions 是最终动作数上限（写前校验，超了整批拒）。
	memoryMaxActions = 20
	// memoryInrunWindow 是 inrun 取锚点前后各多少条 runlog。
	memoryInrunWindow = 10
	// memoryExtractInputCap 是抽取输入的总 rune 上限（postrun 的整场轨迹）。
	memoryExtractInputCap = 8000
	// memoryContentCap 是单条记忆正文的 rune 上限。
	memoryContentCap = 200
	// memoryQuoteCap 是用户原话的 rune 上限。
	memoryQuoteCap = 200
)

// memoryExtractor 是统一抽取器。
type memoryExtractor struct {
	store    *Store
	getModel MemoryModelFunc
	counter  TokenCounter
}

// newMemoryExtractor 造抽取器。
func newMemoryExtractor(store *Store, getModel MemoryModelFunc, counter TokenCounter) *memoryExtractor {
	return &memoryExtractor{store: store, getModel: getModel, counter: counter}
}

// memoryExtractCandidate 是候选表的一行：序号 + 记忆行。
//
// 序号（Index）是**本任务内稳定**的：按 id 排序后从 1 开始编号。
// 模型只认这个序号，不认数据库 id。
type memoryExtractCandidate struct {
	Index int
	Item  MemoryItem
}

// errPostrunWaiting 表示「postrun 的前置未满足」（同 session 还有未完成的 inrun）。
//
// 它**不是**失败：job 要保持 pending、不消耗 attempts，由对账器稍后重新叫醒。
var errPostrunWaiting = errors.New("postrun 前置未满足（同 session 还有未完成的 inrun）")

// memoryEntry 是**新条目**（doc/65 §4.7）：模型从输入里抽出、要写进记忆的内容。
//
// 字段形状按 doc/65 §4.7 逐字对应：模型输出条目只含 scope/content/quote/expires_at/valid_from
// 这五样；layer、归属列、来源指针、状态与审计时间都由程序确定。
type memoryEntry struct {
	Scope     string  `json:"scope"`
	Content   string  `json:"content"`
	Quote     string  `json:"quote"`
	ValidFrom *string `json:"valid_from"`
	ExpiresAt *string `json:"expires_at"`
}

// memoryExtractResult 是一次抽取的结果计数（观测用）。
type memoryExtractResult struct {
	Entries  int
	Items    int
	Actions  int
	Batches  int
	Relevant int
	TokIn    int
	TokOut   int
	Replay   bool
}

// process 处理一条任务。
//
// 返回 errPostrunWaiting = 前置未满足（调用方保持 pending、不消耗 attempts）。
// 其它错误 = 本次处理失败（调用方按退避回 pending / 第三次 failed）。
//
// 三段流（doc/65 §4.7）：
//
//	① 抽新条目：从输入抽出这一场要记的内容；
//	② 分批筛相关：每批候选与**新条目**比较，模型只回候选序号；
//	③ 最终裁决：新条目 + **全部相关候选** → 动作列表 → 一个事务写库。
//
// 全部批次跑完之前不写库；相关集合超预算明确失败；没有新条目直接收尾（不打候选）。
func (e *memoryExtractor) process(ctx context.Context, job *MemoryJob) (memoryExtractResult, error) {
	var res memoryExtractResult
	if job == nil {
		return res, errors.New("空任务")
	}
	// 重放闸：这一批已经提交过 → 直接收尾，不重新分析（doc/65 §4.8）。
	if job.BatchCommittedAt != nil {
		res.Replay = true
		return res, nil
	}
	// postrun 前置：同 session 还有未完成的 inrun → 等。
	// 消费者在认领前查过一次；这里再查是防「检查与认领之间又登记了 inrun」的窄窗口。
	if job.Kind == MemoryJobKindPostrun {
		waiting, err := e.store.hasUnfinishedInrun(ctx, job.SessionID)
		if err != nil {
			return res, err
		}
		if waiting {
			return res, errPostrunWaiting
		}
	}
	m, err := e.memoryModel(ctx, job)
	if err != nil {
		return res, err
	}
	input, err := e.buildInput(ctx, job)
	if err != nil {
		return res, err
	}

	// ① 新条目：没有要记的 → 空批次收尾，不打候选（省掉整轮模型调用）。
	entries, err := e.extractEntries(ctx, m, job, input)
	if err != nil {
		return res, err
	}
	res.Entries = len(entries)
	if len(entries) == 0 {
		res.Items, res.Actions = 0, 0
		if _, err := e.applyActions(ctx, job, nil, nil); err != nil {
			return res, err
		}
		return res, nil
	}

	// ② 分批筛相关：候选全量（不设 150 行上限），每批与新条目比较。
	candidates, err := e.loadCandidates(ctx, job)
	if err != nil {
		return res, err
	}
	relevant, batches, tokIn, tokOut, err := e.screenCandidates(ctx, m, job, entries, candidates)
	if err != nil {
		res.Batches, res.TokIn, res.TokOut = batches, tokIn, tokOut
		return res, err
	}
	res.Batches, res.Relevant, res.TokIn, res.TokOut = batches, len(relevant), tokIn, tokOut

	// ③ 最终裁决：新条目 + 全部相关候选（不是全部候选——分批的意义就在这）。
	byIndex := make(map[int]memoryExtractCandidate, len(candidates))
	for _, c := range candidates {
		byIndex[c.Index] = c
	}
	relevantCands := make([]memoryExtractCandidate, 0, len(relevant))
	for _, i := range relevant {
		if c, ok := byIndex[i]; ok {
			relevantCands = append(relevantCands, c)
		}
	}
	actions, err := e.finalActions(ctx, m, job, entries, relevantCands)
	if err != nil {
		return res, err
	}
	res.Actions = len(actions)
	res.Items, err = e.applyActions(ctx, job, candidates, actions)
	if err != nil {
		return res, err
	}
	return res, nil
}

// memoryModel 解析抽取模型：租户模型优先，其次启动时给的解析函数。
func (e *memoryExtractor) memoryModel(ctx context.Context, job *MemoryJob) (model.BaseChatModel, error) {
	if job != nil && job.TenantID != "" && e.store.tenantModels != nil {
		return e.store.tenantChat(ctx, job.TenantID)
	}
	if e.getModel == nil {
		return nil, errors.New("抽取模型未装配")
	}
	return e.getModel(ctx)
}

// tenantChat 解析该租户的抽取模型（租户 runtime config 的 modelRef + 密钥）。
//
// 租户没配就报错（让本次 job 失败重试，不影响启动与其它租户的 run）。
func (s *Store) tenantChat(ctx context.Context, tenantID string) (model.BaseChatModel, error) {
	if s.tenantModels == nil {
		return nil, errors.New("租户模型未装配")
	}
	cfg, raw, err := s.RuntimeConfigRecord(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	secrets, err := unsealSecretMap(cfg.Secrets)
	if err != nil {
		return nil, err
	}
	ref := effectiveModelRef("", cfg.ModelRef, s.tenantModels.RootRef())
	built, _, err := s.tenantModels.Resolve(ctx, tenantID, ref, raw, secrets)
	if err != nil {
		return nil, err
	}
	log.Printf("memory model tenant=%s ref=%s provider=%s", tenantID, built.Ref, built.Provider)
	return built.Chat, nil
}

// buildInput 装配抽取输入。
//
// inrun：用户原话 + 锚点前后各 10 条 runlog（**锚点是那句原话**）。
// postrun：整场的有界轨迹（沿用 8000 rune 上限与工具行淘汰顺序）。
//
// 两者给模型的是**同一份判定标准与同一个记忆视野**，唯一差别是 postrun 能看整场
// （能发现反复出现的模式）。
func (e *memoryExtractor) buildInput(ctx context.Context, job *MemoryJob) (string, error) {
	if job.Kind == MemoryJobKindInrun {
		return e.buildInrunInput(ctx, job)
	}
	return e.buildRunlogInput(ctx, job)
}

// buildInrunInput 取锚点原话前后各 10 条。
func (e *memoryExtractor) buildInrunInput(ctx context.Context, job *MemoryJob) (string, error) {
	var b strings.Builder
	b.WriteString("【用户刚说的话】" + clipMemoryText(job.Quote, memoryQuoteCap) + "\n")
	anchor := job.SourceSeq
	var entries []RunlogEntry
	err := e.store.db.WithContext(ctx).
		Where("run_id = ? AND seq BETWEEN ? AND ?", job.RunID, anchor-memoryInrunWindow, anchor+memoryInrunWindow).
		Order("seq").Find(&entries).Error
	if err != nil {
		return "", err
	}
	for _, en := range entries {
		b.WriteString(runlogLine(en))
		b.WriteString("\n")
	}
	return clipMemoryText(b.String(), memoryExtractInputCap), nil
}

// buildRunlogInput 取整场的有界轨迹（postrun 用）。
func (e *memoryExtractor) buildRunlogInput(ctx context.Context, job *MemoryJob) (string, error) {
	var entries []RunlogEntry
	if err := e.store.db.WithContext(ctx).Where("run_id = ?", job.RunID).Order("seq").Find(&entries).Error; err != nil {
		return "", err
	}
	var b strings.Builder
	for _, en := range entries {
		b.WriteString(runlogLine(en))
		b.WriteString("\n")
	}
	return clipMemoryText(b.String(), memoryExtractInputCap), nil
}

// runlogLine 把一条 runlog 渲染成一行（工具行先丢最旧的、总量封顶在调用方）。
func runlogLine(e RunlogEntry) string {
	switch e.Role {
	case "user":
		return "用户：" + clipMemoryText(e.Text, memoryContentCap)
	case "assistant":
		if e.Agent != "" {
			return "助手(" + e.Agent + ")：" + clipMemoryText(e.Text, memoryContentCap)
		}
		return "助手：" + clipMemoryText(e.Text, memoryContentCap)
	case "tool_call":
		return "工具调用：" + clipMemoryText(e.Text, 200)
	case "tool_result":
		return "工具结果：" + clipMemoryText(e.Text, 200)
	default:
		return clipMemoryText(e.Text, memoryContentCap)
	}
}

// loadCandidates 读候选表：可见域**全部** active 的 resident + recall。
//
// 不设行数上限（doc/65 §4.7）：第 151 条之后也要有机会参与。
// 按 id 排序 → 序号稳定 → 模型只认序号。
func (e *memoryExtractor) loadCandidates(ctx context.Context, job *MemoryJob) ([]memoryExtractCandidate, error) {
	items, err := e.store.listActiveMemoryForExtraction(ctx, job)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	out := make([]memoryExtractCandidate, 0, len(items))
	for i, it := range items {
		out = append(out, memoryExtractCandidate{Index: i + 1, Item: it})
	}
	return out, nil
}

// extractEntries 抽新条目（三段流的第①步）：一次模型调用，从输入里抽出这一场
// 要写进长期记忆的条目（doc/65 §4.7 的五字段形状）。
//
// 单个条目字段非法 → **丢该条目**，其他条目继续（doc/65 §4.8 失败语义表）；
// 整体 JSON 不可解析 → 整 job 重试。
func (e *memoryExtractor) extractEntries(ctx context.Context, m model.BaseChatModel, job *MemoryJob, input string) ([]memoryEntry, error) {
	out, err := e.callModel(ctx, m, memoryExtractPrompt, input)
	if err != nil {
		return nil, err
	}
	entries, err := parseMemoryEntryList(out)
	if err != nil {
		return nil, fmt.Errorf("解析新条目输出：%w", err)
	}
	kept := make([]memoryEntry, 0, len(entries))
	for _, en := range entries {
		en.Scope = strings.TrimSpace(en.Scope)
		en.Content = strings.TrimSpace(en.Content)
		en.Quote = strings.TrimSpace(en.Quote)
		switch {
		case en.Scope != MemoryScopeUser && en.Scope != MemoryScopeWorkspace:
			continue // scope 非法 → 丢该条目
		case en.Content == "" || utf8.RuneCountInString(en.Content) > memoryContentCap:
			continue // content 空 / 超长 → 丢该条目
		}
		// quote 只截不拒：它是用户原话的引用，超长截断不丢事实。
		en.Quote = clipMemoryText(en.Quote, memoryQuoteCap)
		kept = append(kept, en)
	}
	return kept, nil
}

// parseMemoryEntryList 解析新条目输出：{"entries":[...]}。
//
// （不叫 parseMemoryEntries：memory_markdown.go 已有一个同名函数，那边的 entries
// 是 markdown 文件里的行，这边的是 JSON 抽取输出——两个形状，两个名字。）
func parseMemoryEntryList(raw string) ([]memoryEntry, error) {
	raw = stripCodeFence(raw)
	var out struct {
		Entries []memoryEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// screenCandidates 分批筛相关候选（三段流的第②步）。
//
// 每批只带**这一批**的候选（带序号），与**新条目**比较；模型回「哪几个序号相关」。
// 全部批次跑完才汇总——这里任何一批失败 → 整个 job 失败重试（未扫完不写）。
//
// 返回相关候选的**序号集**（已滤掉越界序号；模型编序号 = 当它没说过）。
func (e *memoryExtractor) screenCandidates(ctx context.Context, m model.BaseChatModel, job *MemoryJob, entries []memoryEntry, candidates []memoryExtractCandidate) (relevant []int, batches, tokIn, tokOut int, err error) {
	entriesPayload, err := renderMemoryEntries(entries)
	if err != nil {
		return nil, batches, tokIn, tokOut, err
	}
	for _, batch := range e.batchCandidates(candidates) {
		batches++
		payload, err := e.renderBatch(batch)
		if err != nil {
			return nil, batches, tokIn, tokOut, err
		}
		out, err := e.callModel(ctx, m, memoryScreenPrompt, entriesPayload+"\n\n【已有记忆（这一批）】\n"+payload)
		if err != nil {
			return nil, batches, tokIn, tokOut, err
		}
		tokIn += e.count(entriesPayload + payload)
		tokOut += e.count(out)
		idxs, err := parseRelevantIndices(out)
		if err != nil {
			// 单批输出不可解析 → 整 job 重试（不截断、不先写一半）。
			return nil, batches, tokIn, tokOut, fmt.Errorf("解析候选筛选输出：%w", err)
		}
		for _, i := range idxs {
			if i >= 1 && i <= len(candidates) {
				relevant = append(relevant, i)
			}
		}
	}
	return relevant, batches, tokIn, tokOut, nil
}

// batchCandidates 按 token 预算分批。
//
// 分批是**给模型看的**，不改变「全部候选都要参与」这件事：
// 每条候选都恰好进一批，只是不同批在不同次调用里出现。
func (e *memoryExtractor) batchCandidates(candidates []memoryExtractCandidate) [][]memoryExtractCandidate {
	if len(candidates) == 0 {
		return nil
	}
	var batches [][]memoryExtractCandidate
	cur := []memoryExtractCandidate{}
	used := 0
	for _, c := range candidates {
		cost := e.count(c.Item.Content) + 16 // 序号与正文
		if len(cur) > 0 && used+cost > memoryBatchTokens {
			batches = append(batches, cur)
			cur = []memoryExtractCandidate{}
			used = 0
		}
		cur = append(cur, c)
		used += cost
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

// renderBatch 渲染一批候选（序号 + 层 + 正文 + 状态）。
func (e *memoryExtractor) renderBatch(batch []memoryExtractCandidate) (string, error) {
	var b strings.Builder
	for _, c := range batch {
		b.WriteString(fmt.Sprintf("#%d [%s/%s] %s\n", c.Index, c.Item.Layer, c.Item.Scope, c.Item.Content))
	}
	return b.String(), nil
}

// finalActions 最终裁决（三段流的第③步）：新条目 + **全部相关候选** → 动作列表。
//
// 不再摆全部候选——分批筛的意义就是让最终调用只看相关集合。
// 相关集合渲染超过 memoryFinalTokens → **明确失败**（整 job 重试）：
// 不截断、不随机选一批、不先写一半（doc/65 §4.7）。
func (e *memoryExtractor) finalActions(ctx context.Context, m model.BaseChatModel, job *MemoryJob, entries []memoryEntry, relevant []memoryExtractCandidate) ([]memoryAction, error) {
	entriesPayload, err := renderMemoryEntries(entries)
	if err != nil {
		return nil, err
	}
	payload, err := e.renderBatch(relevant)
	if err != nil {
		return nil, err
	}
	if used := e.count(entriesPayload + payload); used > memoryFinalTokens {
		return nil, fmt.Errorf("最终相关集合 %d token 超预算 %d（不截断，整 job 重试）", used, memoryFinalTokens)
	}
	out, err := e.callModel(ctx, m, memoryFinalPrompt, entriesPayload+"\n\n【与新条目相关的已有记忆】\n"+payload)
	if err != nil {
		return nil, err
	}
	actions, err := parseMemoryActions(out)
	if err != nil {
		return nil, fmt.Errorf("解析最终动作：%w", err)
	}
	if len(actions) > memoryMaxActions {
		return nil, fmt.Errorf("最终动作 %d 条，超过上限 %d 条", len(actions), memoryMaxActions)
	}
	return actions, nil
}

// renderMemoryEntries 把新条目渲染给模型（筛选与最终裁决共用同一份）。
func renderMemoryEntries(entries []memoryEntry) (string, error) {
	var b strings.Builder
	for i, en := range entries {
		b.WriteString(fmt.Sprintf("新条目%d [scope=%s] %s", i+1, en.Scope, en.Content))
		if en.Quote != "" {
			b.WriteString("（原话：" + en.Quote + "）")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// callModel 调一次模型（temperature=0，让同一输入给同一结果）。
func (e *memoryExtractor) callModel(ctx context.Context, m model.BaseChatModel, system, user string) (string, error) {
	out, err := m.Generate(ctx, []*schema.Message{
		schema.SystemMessage(system),
		schema.UserMessage(user),
	}, model.WithTemperature(0))
	if err != nil {
		return "", fmt.Errorf("抽取调用：%w", err)
	}
	if out == nil {
		return "", errors.New("抽取调用返回空消息")
	}
	return out.Content, nil
}

// count 数 token（没有计数器时按 rune 估：宁可少批也不超预算）。
func (e *memoryExtractor) count(s string) int {
	if e.counter != nil {
		return e.counter.Count(s)
	}
	return len([]rune(s))
}

// parseRelevantIndices 解析候选筛选输出：{"relevant":[3,17]}。
func parseRelevantIndices(raw string) ([]int, error) {
	raw = stripCodeFence(raw)
	var out struct {
		Relevant []int `json:"relevant"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out.Relevant, nil
}

// stripCodeFence 去掉模型输出可能带的 ```json 围栏。
func stripCodeFence(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	return strings.TrimSpace(raw)
}

// ── 抽取器的系统提示 ─────────────────────────────────────────────────────────

// memoryExtractPrompt 是**抽新条目**那一步的系统提示（三段流的第①步）。
//
// 这一步从输入里抽出「这一场要写进记忆的条目」——形状按 doc/65 §4.7：
// scope/content/quote/valid_from/expires_at 五样，layer 与来源指针由程序补。
// 记住 / 改口 / 删除三种意图都先抽成条目，动作裁决留给最终调用。
const memoryExtractPrompt = `你是 tako 的记忆抽取器。给你一段对话现场（场中：用户刚说的话及其上下文；定稿后：整场 run 的轨迹）。
从中抽出**这一场要写进长期记忆的新条目**，输出纯 JSON（可带 ` + "```json" + ` 围栏）：
{"entries":[{"scope":"user|workspace","content":"一条记忆，一句完整的话","quote":"支撑它的用户原话，没有就空串","valid_from":null,"expires_at":null}]}
规则：
- scope=user：跟这个人走、跨项目都成立的偏好或事实；scope=workspace：只在本项目成立的约定。
- content 一条只说一件事，来自对话里明确说过的话或它的直接蕴含；不要推演、不要写没依据的话。
- 用户要删除或改掉某条旧记忆时也抽出来：content 写成「删除：要删的事」或「改口：新说法」。
- 有效期只在用户明确说了才填（RFC3339 格式），没说就 null。
- 没有值得记的就输出 {"entries":[]}。
- 不要写密钥、token、密码。`

// memoryScreenPrompt 是**候选筛选**那一步的系统提示（三段流的第②步）。
//
// 这一步只回答一个问题：「这一批记忆里，哪几条与**新条目**有关」。
// 它不改任何记忆、不产生新条目 —— 改与不改是最终裁决那一步的事。
const memoryScreenPrompt = `你是 tako 的记忆筛选器。给你这次要写入的**新条目**，以及**一批**已有记忆（带编号）。
只回答一个问题：这一批里哪几条与新条目**有关**（会被这次的内容取代、印证、合并、冲突，或就是同一件事，或用户说了要删它）。
输出纯 JSON（可带 ` + "```json" + ` 围栏）：{"relevant":[序号,序号]}
规则：
- 序号照抄我给的编号，不要编。
- 没有任何一条相关就输出 {"relevant":[]}。
- 不输出解释，不输出别的字段。`

// memoryFinalPrompt 是**最终裁决**那一步的系统提示（三段流的第③步）。
//
// 它决定「这次的话到底改了什么」：insert / replace / merge / conflict /
// covered_by_resident / supersede_by_resident / remove。
//
// 目标一律用**候选序号**（我给的编号），不要写数据库 id。
const memoryFinalPrompt = `你是 tako 的记忆裁决器。给你这次抽出的**新条目**，以及与它们相关的已有记忆（带编号）。
判断最终动作，输出**动作列表**（纯 JSON，可带 ` + "```json" + ` 围栏）：
{"actions":[{"op":"insert|replace|merge|conflict|covered_by_resident|supersede_by_resident|remove","targets":[编号],"resident":编号,"scope":"user|workspace","content":"新正文","quote":"用户原话","valid_from":null,"expires_at":null}]}
规则：
- op=insert：一条新的事实。scope 只能是 user（跟这个人走，跨项目）或 workspace（只在本项目）。
- op=replace：用户改了口。targets 指向被取代的旧条目，content 是新正文。
- op=merge：多条合成一条。targets 指向被合并的旧条目，content 是合成后的正文，**不能引入旧条目里没有的事实**。
- op=conflict：两条互相矛盾。targets 指向这两条（**不要替用户选一个赢**）。
- op=covered_by_resident：常驻记忆已经覆盖了这次的内容，不需要再写一条召回，targets 留空。
- op=supersede_by_resident：targets 指向被常驻记忆覆盖的旧召回条目，resident 指向那条常驻行的编号。
- op=remove：**只有用户明确说「别记了 / 删掉」时才用**，targets 指向要删的条目。
- merge/conflict 的 targets 必须是**两条不同的已有记忆**（#编号）——新条目不是已有记忆，不能当目标；只有一条相关记忆时用不了这两个操作。
- 新条目与某条已有记忆是同一件事、且那条记忆仍然正确 → 不用动作（{"actions":[]}），已有记忆就是当前态。
- targets 里的编号必须是我给的编号，不要编，不要写数据库 id。
- insert/replace/merge 的 scope/content/quote 按新条目照抄或按它收敛，不要另起炉灶。
- 动作最多 20 条。没有要动的就输出 {"actions":[]}。
- 不要写密钥、token、密码。`

// logExtract 记一次抽取的观测行。
func logExtract(job *MemoryJob, res memoryExtractResult, err error) {
	if err != nil {
		log.Printf("memory extract fail job=%s attempt=%d: %v", job.ID, job.Attempts, err)
		return
	}
	log.Printf("memory extract done job=%s kind=%s run=%s entries=%d items=%d actions=%d batches=%d relevant=%d tok_in=%d tok_out=%d",
		job.ID, job.Kind, job.RunID, res.Entries, res.Items, res.Actions, res.Batches, res.Relevant, res.TokIn, res.TokOut)
}
