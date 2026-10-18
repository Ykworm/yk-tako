// 本文件是长期记忆的线一（doc/29 §4–§6）：抽取任务的存储形态、后台抽取循环与词法召回。
//
// 一份原料两条加工线：本文件只碰 memory_items / memory_jobs；dialogue_chunks 在 dialogue.go。
// 写入方唯一 = 抽取循环（RunMemoryExtractor）；读方 = 召回函数 Recall（注入与 /api/memory/recall
// 共用同一份实现）。run 的主链路只在 EndRun 里多插一行 pending job，其余全在后台。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// MemoryConfig 是两线的运行配置（doc/29 §8）。TenantID 一期恒 "default"（doc/28 落地后只改
// 这里）；Workspace 与笔记/文件工具同一个根；三个开关同时管后台循环与召回：
//   - Extract：run 终态是否登记抽取 job（关 = 线一召回照旧读已有记忆）；
//   - Recall：线一是否召回；
//   - RAG：线二是否索引与召回。
//
// 放在 Store 上而不是散在三处：EndRun（同事务插 job）、Worker（注入）、Server（预览端点）
// 读同一份值，开关不会出现「端点开了、注入没开」的假象。
type MemoryConfig struct {
	TenantID  string
	Workspace string
	Extract   bool
	Recall    bool
	RAG       bool
}

// ConfigureMemory 在启动装配期调一次（workspace 绝对化之后）。TenantID 空 = default。
func (s *Store) ConfigureMemory(cfg MemoryConfig) {
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	s.memoryCfg = cfg
}

// MemoryConfig 返回当前配置（只读快照）。
func (s *Store) MemoryConfig() MemoryConfig { return s.memoryCfg }

// 线一抽取循环的节奏与任务生命周期（doc/29 §4.5）。
const (
	memoryExtractTick = 2 * time.Second // 空轮询间隔
	memoryJobLease    = 2 * time.Minute // running 租约：进程崩溃后别的副本按它捡回
	memoryMaxAttempts = 3               // 三次后 failed（run 早已 complete）
)

// memoryRetryBackoff[i] 是第 i+1 次失败后的退避（1min / 5min；第三次直接 failed）。
var memoryRetryBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// MemoryItem = `memory_items`（doc/29 §7.1）：一条跨会话记忆。只增不改（一期无 update/delete）。
// 两条部分唯一索引：
//   - memory_items_dedup：同租户/工作区/kind/内容只留一行（episode 除外）；
//   - memory_items_episode_once：episode 每 run 一条，重放 safe。
type MemoryItem struct {
	ID              string    `gorm:"primaryKey"` // mem_<ULID>
	TenantID        string    `gorm:"not null;uniqueIndex:memory_items_dedup,priority:1,where:kind <> 'episode';index:memory_items_recall,priority:1;index:memory_items_topic,priority:1"`
	Workspace       string    `gorm:"not null;uniqueIndex:memory_items_dedup,priority:2;index:memory_items_recall,priority:2;index:memory_items_topic,priority:2"`
	Kind            string    `gorm:"not null;uniqueIndex:memory_items_dedup,priority:3;index:memory_items_recall,priority:3"` // fact | preference | decision | episode
	Content         string    `gorm:"not null;uniqueIndex:memory_items_dedup,priority:4"`
	Topic           string    `gorm:"not null;index:memory_items_topic,priority:3"` // 小写 kebab-case；episode 用 run:<runId>
	SourceSessionID SessionID `gorm:"not null"`
	SourceRunID     RunID     `gorm:"not null;uniqueIndex:memory_items_episode_once,where:kind = 'episode'"`
	SourceSeq       int64     `gorm:"not null"` // 回原话现场：runlog seq
	CreatedAt       time.Time `gorm:"not null"`
}

func (MemoryItem) TableName() string { return "memory_items" }

// MemoryJob = `memory_jobs`（doc/29 §4.1/§4.5）：run 终态登记的抽取任务（outbox）。
// 一个 run 一条（run_id 唯一）；claim 走 SKIP LOCKED，失败指数退避 1/5/30 分钟，3 次 failed。
type MemoryJob struct {
	ID            string    `gorm:"primaryKey"`           // mj_<ULID>
	RunID         RunID     `gorm:"not null;uniqueIndex"` // 一个 run 一条
	SessionID     SessionID `gorm:"not null"`
	TenantID      string    `gorm:"not null"` // 一期恒 default
	Workspace     string    `gorm:"not null"`
	Status        string    `gorm:"not null;index:memory_jobs_claim,priority:1"` // pending | running | done | failed
	Attempts      int       `gorm:"not null"`
	LeaseUntil    *time.Time
	NextAttemptAt time.Time `gorm:"not null;index:memory_jobs_claim,priority:2"`
	LastError     string
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

func (MemoryJob) TableName() string { return "memory_jobs" }

// ListMemoryItems 是 GET /api/memory/items 的读侧（doc/29 §7.2①）：kind 空 = 全部（含 episode），
// limit 默认 50、上限 200；排序 created_at DESC, id DESC。
func (s *Store) ListMemoryItems(ctx context.Context, kind string, limit int) ([]MemoryItem, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := s.db.WithContext(ctx).Order("created_at DESC, id DESC").Limit(limit)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var items []MemoryItem
	err := q.Find(&items).Error
	if items == nil {
		items = []MemoryItem{}
	}
	return items, err
}

// ListMemoryJobs 是 GET /api/memory/jobs 的读侧（doc/29 §7.2②）：runId 空 = 最近 20 条。
func (s *Store) ListMemoryJobs(ctx context.Context, runID RunID) ([]MemoryJob, error) {
	q := s.db.WithContext(ctx).Order("created_at DESC, id DESC").Limit(20)
	if runID != "" {
		q = q.Where("run_id = ?", runID)
	}
	var jobs []MemoryJob
	err := q.Find(&jobs).Error
	if jobs == nil {
		jobs = []MemoryJob{}
	}
	return jobs, err
}

// MemoryModelFunc 按需解析抽取模型（doc/29 §8）：TAKO_MEMORY_MODEL 为空时返回主模型客户端；
// 非空时走 ModelRegistry 懒解析。解析失败只让当前 job 失败重试，不影响启动与 run。
type MemoryModelFunc func(ctx context.Context) (model.BaseChatModel, error)

// extractedItem 是抽取模型输出并过校验后的一条候选记忆。
type extractedItem struct {
	Kind    string
	Topic   string
	Content string
}

// extractStats 是一次 job 的观测计数（日志三行的 done 行）。
type extractStats struct {
	Episode int
	Items   int
	Dup     int
	TokIn   int
	TokOut  int
	Replay  bool // 重放闸命中：本 run 已有记忆行，未调模型
}

// claimedMemoryJob 是 claim 成功后的任务现场（attempts 已含本次）。
type claimedMemoryJob struct {
	ID        string
	RunID     RunID
	SessionID SessionID
	TenantID  string
	Workspace string
	Attempts  int
	CreatedAt time.Time
}

// RunMemoryExtractor 是线一的后台常驻循环（doc/29 §4.5）：每 2s claim 一条可捡任务，
// 处理成功写 items + done；失败按 1/5/30 分钟退避，三次 failed。只依赖 Store 与抽取模型，
// 不碰 run 执行路径；进程重启后 pending / 租约过期的 job 会被任何副本重新捡起。
func (s *Store) RunMemoryExtractor(ctx context.Context, getModel MemoryModelFunc, counter TokenCounter) {
	if !s.memoryCfg.Extract || s.memoryCfg.Workspace == "" {
		return
	}
	log.Printf("memory extractor: started (tenant=%s workspace=%s)", s.memoryCfg.TenantID, s.memoryCfg.Workspace)
	for {
		job, err := s.claimMemoryJob(ctx)
		if err != nil {
			log.Printf("memory extract claim: %v", err)
			if !sleepCtx(ctx, memoryExtractTick) {
				return
			}
			continue
		}
		if job == nil {
			if !sleepCtx(ctx, memoryExtractTick) {
				return
			}
			continue
		}
		log.Printf("memory extract claim %s run=%s attempt=%d", job.ID, job.RunID, job.Attempts)
		stats, err := s.processMemoryJob(ctx, job, getModel, counter)
		if err != nil {
			log.Printf("memory extract fail %s attempt=%d: %v", job.ID, job.Attempts, err)
			if ferr := s.failMemoryJob(ctx, job, err); ferr != nil {
				log.Printf("memory extract fail %s: 状态回写失败: %v", job.ID, ferr)
			}
			continue
		}
		if stats.Replay {
			log.Printf("memory extract done %s: replay skip（本 run 的记忆行已由先前 attempt 写入）", job.ID)
			continue
		}
		log.Printf("memory extract done %s: episode=%d items=%d dup=%d tok_in=%d tok_out=%d",
			job.ID, stats.Episode, stats.Items, stats.Dup, stats.TokIn, stats.TokOut)
	}
}

// claimMemoryJob 抢一条可捡任务（doc/29 §4.5）：pending 到期，或 running 但租约已过（崩溃恢复）。
// SKIP LOCKED 让多副本各捡各的；attempts 在 claim 时 +1，失败退避按它算。
func (s *Store) claimMemoryJob(ctx context.Context) (*claimedMemoryJob, error) {
	var job claimedMemoryJob
	res := s.db.WithContext(ctx).Raw(`
WITH picked AS (
  SELECT id FROM memory_jobs
   WHERE (status = 'pending' AND next_attempt_at <= now())
      OR (status = 'running' AND lease_until < now())
   ORDER BY next_attempt_at, created_at
   LIMIT 1
   FOR UPDATE SKIP LOCKED
)
UPDATE memory_jobs j
   SET status = 'running', attempts = j.attempts + 1,
       lease_until = now() + make_interval(secs => ?), updated_at = now()
  FROM picked
 WHERE j.id = picked.id
 RETURNING j.id, j.run_id, j.session_id, j.tenant_id, j.workspace, j.attempts, j.created_at`,
		memoryJobLease.Seconds()).Scan(&job)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	return &job, nil
}

// 线一抽取的帽与词表（doc/29 §4.2–§4.4、§8；数值写死不是配置）。
const (
	memoryExtractInputCap = 8000 // 一场 run 轨迹拼进抽取调用的总帽（rune）
	memoryExtractItemsMax = 5    // 每场最多抽 5 条（episode 另算）
	memoryContentCap      = 200  // 单条 content 上限（rune）
	memoryTopicCap        = 40   // topic 上限（含首字符）
	memorySubFinalCap     = 1000 // 子代理终稿进抽取输入的单条帽
	memoryToolArgsCap     = 200  // tool_call args 单条帽
	memoryToolResultCap   = 500  // tool_result 输出单条帽
	memoryExtractTimeout  = 90 * time.Second
)

// memoryTopicPattern 是 topic 的机械校验（doc/29 §4.4 第 2 条）：小写 kebab-case，
// 首字符字母数字，总长 ≤40。自由文本会让「同 topic 取最新」失效。
var memoryTopicPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// memoryKinds 是 items 的 kind 词表（固定写死；episode 由 episode 字段单独生成，不在词表里）。
var memoryKinds = map[string]bool{"fact": true, "preference": true, "decision": true}

// processMemoryJob 处理一条已 claim 的任务：读本 run 轨迹 + 已有笔记 → 调抽取模型 →
// 机械校验 → items + done 同一笔事务（doc/29 §4.2–§4.5）。
func (s *Store) processMemoryJob(ctx context.Context, job *claimedMemoryJob, getModel MemoryModelFunc, counter TokenCounter) (extractStats, error) {
	// 重放闸（doc/29 §4.5）：items 与 job=done 同笔事务，本 run 已有记忆行 = 上次 attempt 已完整
	// 提交，重放直接翻 done、不再调模型。生成模型即使 temperature=0 也会改写措辞，内容级去重
	// 兜不住「同一事实换个说法」；验收里的「重放假 job 不翻倍」由这道闸保证。
	var written int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM memory_items WHERE source_run_id = ?`, job.RunID).Scan(&written).Error; err != nil {
		return extractStats{}, err
	}
	if written > 0 {
		if err := s.markMemoryJobDone(ctx, job); err != nil {
			return extractStats{}, err
		}
		return extractStats{Replay: true}, nil
	}
	seq, err := s.runlogUserSeq(ctx, job.SessionID, job.RunID)
	if err != nil {
		return extractStats{}, err
	}
	input, err := s.buildExtractInput(ctx, job)
	if err != nil {
		return extractStats{}, err
	}
	if getModel == nil {
		return extractStats{}, errors.New("memory extractor: 抽取模型未装配")
	}
	extractCtx, cancel := context.WithTimeout(ctx, memoryExtractTimeout)
	defer cancel()
	m, err := getModel(extractCtx)
	if err != nil {
		return extractStats{}, fmt.Errorf("解析抽取模型: %w", err)
	}
	out, err := m.Generate(extractCtx, []*schema.Message{
		schema.SystemMessage(memoryExtractPrompt),
		schema.UserMessage(input),
	}, model.WithTemperature(0))
	if err != nil {
		return extractStats{}, fmt.Errorf("抽取调用: %w", err)
	}
	if out == nil {
		return extractStats{}, errors.New("抽取调用返回空消息")
	}
	episode, items, err := parseExtraction(out.Content)
	if err != nil {
		return extractStats{}, fmt.Errorf("解析抽取输出: %w", err)
	}
	stats, err := s.writeMemoryJobResult(ctx, job, seq, episode, items)
	if err != nil {
		return extractStats{}, err
	}
	if counter != nil {
		stats.TokIn = counter.Count(input)
		stats.TokOut = counter.Count(out.Content)
	}
	return stats, nil
}

// memoryExtractPrompt 是抽取器的系统提示（doc/29 §4.3 的五条要点，逐条对应一个已知失败）。
const memoryExtractPrompt = `你是 tako 的记忆抽取器。输入是一场已完成 run 的对话轨迹与已有笔记。
只抽取跨会话仍然成立、未来仍然有用的信息。输出纯 JSON（可带 ` + "```json" + ` 围栏）：
{"episode":"本场一句话摘要（≤200字）","items":[{"kind":"fact|preference|decision","topic":"小写-kebab-case，≤40字符","content":"≤200字"}]}
规则：
- 只抽跨会话仍然成立的东西；不要抽「已修复 X」这类过期即失效的过程细节。
- **用户在消息里明说的项目/环境事实必须抽成 fact**：端口、路径、版本、命令、默认值、实例归属
  （如「dev 库是 PostgreSQL 5433 端口，5432 是别的实例」→ fact，content 保留数字与归属）。
  这类事实是本 schema 的首要用途，宁多勿漏；同一句里有几条独立事实就抽几条。
- 不抽密钥、token、密码。
- 已有笔记里已经有的，不要重复。
- topic 用小写 kebab-case（如 project.dev-pg-port），≤40 字符。
- 没有值得记的就输出空 items。
- kind 只能是 fact（项目/环境事实）、preference（用户工作偏好）、decision（做过的选择与理由）。
- 可用条目少于 5 条时就少写；不要为凑数编造。`

// buildExtractInput 把本 run 的 runlog 拼成有界文本（doc/29 §4.2）：
// user 与 root 终稿全留；子代理终稿每条 1000 rune；tool_call args 200、tool_result 500。
// 总帽 8000：先整行丢最旧的工具行，仍超再整行丢最旧的子代理终稿；极端情况下从头截断
// （user 在最前优先保，root 终稿可能被截尾——只在单场本身超过 8000 rune 时发生）。
func (s *Store) buildExtractInput(ctx context.Context, job *claimedMemoryJob) (string, error) {
	var entries []RunlogEntry
	if err := s.db.WithContext(ctx).Where("run_id = ?", job.RunID).Order("seq").Find(&entries).Error; err != nil {
		return "", err
	}
	type line struct {
		text string
		tool bool
		sub  bool
	}
	lines := make([]line, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Role == "user":
			lines = append(lines, line{text: "用户：" + e.Text})
		case e.Role == "assistant" && e.Agent == "":
			lines = append(lines, line{text: "助手：" + e.Text})
		case e.Role == "assistant":
			lines = append(lines, line{text: "子代理(" + e.Agent + ")：" + clipMemoryText(e.Text, memorySubFinalCap), sub: true})
		case e.Role == "tool_call":
			var p toolCallPayload
			if json.Unmarshal([]byte(e.Payload), &p) == nil && p.Name != "" {
				lines = append(lines, line{text: "工具调用：" + p.Name + "(" + clipMemoryText(p.Args, memoryToolArgsCap) + ")", tool: true})
			} else {
				lines = append(lines, line{text: "工具调用：" + clipMemoryText(e.Text, memoryToolArgsCap), tool: true})
			}
		case e.Role == "tool_result":
			var p toolResultPayload
			if json.Unmarshal([]byte(e.Payload), &p) == nil && p.Name != "" {
				lines = append(lines, line{text: "工具结果：" + clipMemoryText(p.Output, memoryToolResultCap), tool: true})
			} else {
				lines = append(lines, line{text: "工具结果：" + clipMemoryText(e.Text, memoryToolResultCap), tool: true})
			}
		}
	}
	// 超帽的丢弃顺序（doc/29 §4.2）：最旧工具行 → 最旧子代理终稿 → 从头截断。
	total := 0
	for _, l := range lines {
		total += utf8.RuneCountInString(l.text) + 1
	}
	if total > memoryExtractInputCap {
		for i := range lines {
			if total <= memoryExtractInputCap {
				break
			}
			if lines[i].tool {
				total -= utf8.RuneCountInString(lines[i].text) + 1
				lines[i].text = ""
			}
		}
	}
	if total > memoryExtractInputCap {
		for i := range lines {
			if total <= memoryExtractInputCap {
				break
			}
			if lines[i].sub {
				total -= utf8.RuneCountInString(lines[i].text) + 1
				lines[i].text = ""
			}
		}
	}
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.text != "" {
			parts = append(parts, l.text)
		}
	}
	body := strings.Join(parts, "\n")
	body = clipMemoryText(body, memoryExtractInputCap)
	notes, err := s.listLiveNotes(ctx, job.Workspace)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(notes) > 0 {
		b.WriteString("【已有笔记（不要重复）】\n")
		for _, n := range notes {
			b.WriteString("- ")
			b.WriteString(n.Content)
			b.WriteString("\n")
		}
	}
	b.WriteString("【本场轨迹】\n")
	b.WriteString(body)
	return b.String(), nil
}

// extractionOutput / extractionItem 是模型输出的 JSON 形状（doc/29 §4.3）。
type extractionOutput struct {
	Episode string `json:"episode"`
	Items   []struct {
		Kind    string `json:"kind"`
		Topic   string `json:"topic"`
		Content string `json:"content"`
	} `json:"items"`
}

// parseExtraction 剥 JSON 围栏并逐条机械校验（doc/29 §4.4）：坏条目丢弃并记数，不整 job 失败；
// JSON 本身不可解析才返回错误（模型/配置问题，走 job 重试）。
func parseExtraction(raw string) (episode string, items []extractedItem, err error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	var out extractionOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", nil, err
	}
	episode = clipMemoryText(strings.TrimSpace(out.Episode), memoryContentCap)
	for _, it := range out.Items {
		if len(items) >= memoryExtractItemsMax {
			break
		}
		kind := strings.TrimSpace(it.Kind)
		topic := strings.TrimSpace(it.Topic)
		content := strings.TrimSpace(it.Content)
		if !memoryKinds[kind] {
			continue
		}
		if !memoryTopicPattern.MatchString(topic) || utf8.RuneCountInString(topic) > memoryTopicCap {
			continue
		}
		if content == "" || utf8.RuneCountInString(content) > memoryContentCap {
			continue
		}
		if scanNoteInvisible(content) != "" || scanNoteInvisible(topic) != "" {
			continue
		}
		if strings.Contains(content, "<memory-context") || strings.Contains(content, "</memory-context") ||
			strings.Contains(content, "<dialogue-context") {
			continue
		}
		items = append(items, extractedItem{Kind: kind, Topic: topic, Content: content})
	}
	return episode, items, nil
}

// clipRunes 按 rune 截断、无标记（与 compact 的 truncateRunes 不同：这里的文本会进抽取
// 输入/校验，标记串只会污染模型输出与长度判定）。
func clipMemoryText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// markMemoryJobDone 是重放闸的收尾：只翻本次 attempt（attempts 相等）的 running 行，
// 被别的副本重新 claim 后旧 attempt 的迟到结果不改状态。
func (s *Store) markMemoryJobDone(ctx context.Context, job *claimedMemoryJob) error {
	return s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET status = 'done', lease_until = NULL, last_error = '', updated_at = now()
		 WHERE id = ? AND status = 'running' AND attempts = ?`,
		job.ID, job.Attempts).Error
}

// writeMemoryJobResult 是「items + done」的同一笔事务（doc/29 §4.4）：episode 按 source_run_id
// 唯一、items 按租户/工作区/kind/内容唯一——重放同一 job 不会翻倍；job 只由本次 attempt
// （attempts 相等）翻 done，被别的副本重新 claim 后旧 attempt 的迟到结果不改状态。
func (s *Store) writeMemoryJobResult(ctx context.Context, job *claimedMemoryJob, sourceSeq int64,
	episode string, items []extractedItem) (extractStats, error) {
	var stats extractStats
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if episode != "" {
			id, err := newRowID("mem_")
			if err != nil {
				return err
			}
			res := tx.Exec(
				`INSERT INTO memory_items (id, tenant_id, workspace, kind, content, topic,
				         source_session_id, source_run_id, source_seq, created_at)
				 VALUES (?, ?, ?, 'episode', ?, ?, ?, ?, ?, now())
				 ON CONFLICT (source_run_id) WHERE kind = 'episode' DO NOTHING`,
				id, job.TenantID, job.Workspace, episode, "run:"+string(job.RunID),
				job.SessionID, job.RunID, sourceSeq)
			if res.Error != nil {
				return res.Error
			}
			stats.Episode = int(res.RowsAffected)
		}
		for _, it := range items {
			id, err := newRowID("mem_")
			if err != nil {
				return err
			}
			res := tx.Exec(
				`INSERT INTO memory_items (id, tenant_id, workspace, kind, content, topic,
				         source_session_id, source_run_id, source_seq, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, now())
				 ON CONFLICT (tenant_id, workspace, kind, content) WHERE kind <> 'episode' DO NOTHING`,
				id, job.TenantID, job.Workspace, it.Kind, it.Content, it.Topic,
				job.SessionID, job.RunID, sourceSeq)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				stats.Items++
			} else {
				stats.Dup++
			}
		}
		return tx.Exec(
			`UPDATE memory_jobs SET status = 'done', lease_until = NULL, last_error = '', updated_at = now()
			 WHERE id = ? AND status = 'running' AND attempts = ?`,
			job.ID, job.Attempts).Error
	})
	return stats, err
}

// failMemoryJob 记一次失败（doc/29 §4.5）：第 1/2 次回落 pending 并按 1/5 分钟退避；
// 第 3 次翻 failed（run 早已 complete，只留日志与 last_error）。
func (s *Store) failMemoryJob(ctx context.Context, job *claimedMemoryJob, cause error) error {
	msg := cause.Error()
	var err error
	if job.Attempts >= memoryMaxAttempts {
		err = s.db.WithContext(ctx).Exec(
			`UPDATE memory_jobs SET status = 'failed', lease_until = NULL, last_error = ?, updated_at = now()
			 WHERE id = ? AND status = 'running' AND attempts = ?`,
			msg, job.ID, job.Attempts).Error
		return err
	}
	backoff := memoryRetryBackoff[job.Attempts-1]
	err = s.db.WithContext(ctx).Exec(
		`UPDATE memory_jobs SET status = 'pending', lease_until = NULL, last_error = ?,
		        next_attempt_at = now() + make_interval(secs => ?), updated_at = now()
		 WHERE id = ? AND status = 'running' AND attempts = ?`,
		msg, backoff.Seconds(), job.ID, job.Attempts).Error
	return err
}

// sleepCtx 等一拍；ctx 取消返回 false（循环退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
