// 本文件是长期记忆的线二（doc/29 §5–§6.2）：对话原文块的可重建副本、后台索引循环与 pg_search 召回。
//
// 正本永远是 runlog_entries；dialogue_chunks 只存「一场 run 一块」的派生 passage，清掉即可由
// 未索引反查全量重建。写入方唯一 = RunDialogueIndexer；读方 = Recall 的 dialogue 半边。
package runengine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

// DialogueChunk = `dialogue_chunks`（doc/29 §5.4）：一场 run 的原文块（passage ≤4000 rune）。
// passage 列上的 pg_search 索引由 ensureDialogueIndex 手写迁移，GORM 不管理。
type DialogueChunk struct {
	RunID     RunID     `gorm:"primaryKey"`
	SessionID SessionID `gorm:"not null;index"`
	TenantID  string    `gorm:"not null;index:dialogue_chunks_scope,priority:1"` // 一期恒 default
	Workspace string    `gorm:"not null;index:dialogue_chunks_scope,priority:2"`
	Passage   string    `gorm:"not null"`
	Runes     int       `gorm:"not null"`
	Truncated bool      `gorm:"not null"`
	IndexedAt time.Time `gorm:"not null"`
}

func (DialogueChunk) TableName() string { return "dialogue_chunks" }

// dialogueSearchIndexDDL 是 pg_search 索引（doc/29 §5.4，逐字按 v0.26.2 实证语法）：
// 键列 run_id 必须写在第一位；TEXT 键列已由 B1 探针实证可用（INSERT/UPDATE 均维护索引）。
const dialogueSearchIndexDDL = `CREATE INDEX IF NOT EXISTS dialogue_chunks_search_idx
ON dialogue_chunks USING paradedb (run_id, passage)
WITH (text_fields = '{"passage": {"tokenizer": {"type": "chinese_lindera"}}}')`

// EnsureDialogueIndex 在启动装配期建手写索引（doc/29 §5.4/§8）：TAKO_RAG=off 完全不碰扩展；
// 开着但 preload 或扩展缺失 → 返回错误由 main 退出（fail fast，与模型/PG/Redis 一致）。
func (s *Store) EnsureDialogueIndex(ctx context.Context) error {
	if !s.memoryCfg.RAG {
		return nil
	}
	var preload string
	if err := s.db.WithContext(ctx).Raw(`SELECT current_setting('shared_preload_libraries')`).Scan(&preload).Error; err != nil {
		return err
	}
	loaded := false
	for _, p := range strings.Split(preload, ",") {
		if strings.TrimSpace(p) == "pg_search" {
			loaded = true
			break
		}
	}
	if !loaded {
		return fmt.Errorf("pg_search 未预加载（shared_preload_libraries=%q）：给 PG 加 shared_preload_libraries='pg_search' 并重启，或设 TAKO_RAG=off 关闭线二", preload)
	}
	var ext int
	if err := s.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_search'`).Scan(&ext).Error; err != nil {
		return err
	}
	if ext == 0 {
		return fmt.Errorf("pg_search 扩展未安装：psql -p <port> -d <db> -c 'CREATE EXTENSION pg_search CASCADE'（依赖 vector）")
	}
	return s.db.WithContext(ctx).Exec(dialogueSearchIndexDDL).Error
}

// ListDialogueChunks 是 GET /api/memory/dialogue 的读侧（doc/29 §7.2③）：runId 精确查，空参 = 最近 20 条。
func (s *Store) ListDialogueChunks(ctx context.Context, runID RunID) ([]DialogueChunk, error) {
	q := s.db.WithContext(ctx).Order("indexed_at DESC, run_id DESC").Limit(20)
	if runID != "" {
		q = q.Where("run_id = ?", runID)
	}
	var chunks []DialogueChunk
	err := q.Find(&chunks).Error
	if chunks == nil {
		chunks = []DialogueChunk{}
	}
	return chunks, err
}

// ResetDialogueChunks（B4）= 派生副本的显式重建入口：清空全部 chunk，由常驻索引循环
// 从 complete 的 runs 反查重建。删行而不是 TRUNCATE：不拿大表锁，扫描循环每轮 ≤20 条，
// 重建期间召回看到的是一份正在变长的副本（派生副本允许暂时不全）。
func (s *Store) ResetDialogueChunks(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).Exec(`DELETE FROM dialogue_chunks`)
	return res.RowsAffected, res.Error
}

// 线二索引循环的节奏与帽（doc/29 §5.1/§5.2/§8，写死不是配置）。
const (
	dialogueIndexScanInterval = 2 * time.Second // 空扫间隔
	dialogueIndexBatch        = 20              // 每轮最多索引的 run 数（反查会把历史一次全判待索引）
	dialogueChunkCap          = 4000            // passage 总帽（rune）
	dialogueSubFinalCap       = 1000            // 子代理终稿单条帽（rune）
)

// RunDialogueIndexer 是线二的后台常驻循环（doc/29 §5.2）：每 2s 反查「complete 且不在
// dialogue_chunks」的 run，满批不睡继续追赶（启动时历史多）。插入/更新走单语句 upsert，
// 多副本同时扫到同一 run 也不会撞主键；删行即自愈（下一轮反查重新索引）。
func (s *Store) RunDialogueIndexer(ctx context.Context) {
	if !s.memoryCfg.RAG {
		return
	}
	log.Printf("dialogue indexer: started (tenant=%s workspace=%s)", s.memoryCfg.TenantID, s.memoryCfg.Workspace)
	for {
		n, err := s.indexDialogueBatch(ctx)
		if err != nil {
			log.Printf("dialogue index: %v", err)
		}
		if n < dialogueIndexBatch {
			if !sleepCtx(ctx, dialogueIndexScanInterval) {
				return
			}
		}
	}
}

// unindexedRun 是反查结果的一行。
type unindexedRun struct {
	ID        RunID
	SessionID SessionID
}

// indexDialogueBatch 索引一批未索引 run；单条失败只记日志、不挡同批其余（下一轮自愈）。
func (s *Store) indexDialogueBatch(ctx context.Context) (int, error) {
	var runs []unindexedRun
	err := s.db.WithContext(ctx).Raw(`
SELECT r.id, r.session_id
  FROM runs r
 WHERE r.status = 'complete'
   AND NOT EXISTS (SELECT 1 FROM dialogue_chunks c WHERE c.run_id = r.id)
 ORDER BY r.finished_at, r.id
 LIMIT ?`, dialogueIndexBatch).Scan(&runs).Error
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		if err := s.indexDialogueRun(ctx, r); err != nil {
			log.Printf("dialogue index %s/%s: %v", r.SessionID, r.ID, err)
		}
	}
	return len(runs), nil
}

// indexDialogueRun 组装一场 run 的 passage 并 upsert（doc/29 §5.1/§5.3）。
func (s *Store) indexDialogueRun(ctx context.Context, r unindexedRun) error {
	var entries []RunlogEntry
	if err := s.db.WithContext(ctx).Where("run_id = ?", r.ID).Order("seq").Find(&entries).Error; err != nil {
		return err
	}
	passage, truncated := buildDialoguePassage(r.SessionID, r.ID, entries)
	return s.db.WithContext(ctx).Exec(`
INSERT INTO dialogue_chunks (run_id, session_id, tenant_id, workspace, passage, runes, truncated, indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, now())
ON CONFLICT (run_id) DO UPDATE SET
  passage = EXCLUDED.passage, runes = EXCLUDED.runes,
  truncated = EXCLUDED.truncated, indexed_at = EXCLUDED.indexed_at`,
		r.ID, r.SessionID, s.memoryCfg.TenantID, s.memoryCfg.Workspace,
		passage, utf8.RuneCountInString(passage), truncated).Error
}

// 线二召回的帽（doc/29 §6.2/§8）。
const (
	dialogueRecallItemsMax = 3   // 注入的历史对话块上限
	dialogueRecallRuneCap  = 300 // 每条 passage 的注入帽
)

// dialogueCandidate 是一条召回的原文块（finished_at 取 run 的定稿时间，用于渲染日期）。
type dialogueCandidate struct {
	RunID      RunID
	SessionID  SessionID
	Passage    string
	Score      float64
	FinishedAt *time.Time
}

// recallDialogueItems 走 pg_search BM25（doc/29 §6.2）：用户原文整个交给 paradedb.match，
// 分词在扩展内完成；`@@@` 左边是索引键列 run_id（不是 passage）。租户/工作区等值过滤。
func (s *Store) recallDialogueItems(ctx context.Context, query string) ([]dialogueCandidate, error) {
	var rows []dialogueCandidate
	err := s.db.WithContext(ctx).Raw(`
SELECT c.run_id, c.session_id, c.passage, paradedb.score(c.run_id) AS score,
       (SELECT r.finished_at FROM runs r WHERE r.id = c.run_id) AS finished_at
  FROM dialogue_chunks c
 WHERE c.run_id @@@ paradedb.match('passage', ?)
   AND c.tenant_id = ? AND c.workspace = ?
 ORDER BY score DESC
 LIMIT ?`, query, s.memoryCfg.TenantID, s.memoryCfg.Workspace, dialogueRecallItemsMax).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// buildDialoguePassage 把 runlog 机械拼成一块（doc/29 §5.1）：user 全留、root 终稿全留、
// 子代理终稿每条截 1000；总帽 4000，超出从尾部截并记 truncated。
func buildDialoguePassage(sessionID SessionID, runID RunID, entries []RunlogEntry) (string, bool) {
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, "[会话 "+string(sessionID)+" · 场 "+string(runID)+"]")
	for _, e := range entries {
		switch {
		case e.Role == "user":
			lines = append(lines, "用户："+e.Text)
		case e.Role == "assistant" && e.Agent == "":
			lines = append(lines, "助手："+e.Text)
		case e.Role == "assistant":
			lines = append(lines, "子代理("+e.Agent+")："+clipMemoryText(e.Text, dialogueSubFinalCap))
		}
	}
	passage := strings.Join(lines, "\n")
	if utf8.RuneCountInString(passage) > dialogueChunkCap {
		return clipMemoryText(passage, dialogueChunkCap), true
	}
	return passage, false
}
