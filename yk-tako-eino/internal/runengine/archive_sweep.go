// 档案清扫（doc/37 D4 + doc/39 S3）。一轮做三件事，共用 cleaner_state 的租约和 archive 游标：
// 过窗 run 的 compacted 原文行、过窗软删会话的全部模型消息、过窗的子代理 wire 文件。
// runlog、runs、usage、记忆、seq=0 摘要行不在前两类里。摘要行只在会话自己过窗被整场清掉时一起走。
package runengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

const archiveSweepBatch = 200

// ErrSweepNotClaimed 表示 lease 行还在别人的 5 分钟租约里。本轮不删任何东西。
var ErrSweepNotClaimed = errors.New("sweep not claimed")

type sweepCursor struct {
	n    int64
	edge time.Time
	full bool
}

// SweepArchived 抢租约，按 watermark 删掉过窗档案，再扫 wire 文件。
// 没抢到返回 ErrSweepNotClaimed，watermark 仍是库里现在的值。
// workspace 为空时只动表，不动文件。batchLimit < 1 时用 archiveSweepBatch。
func (s *Store) SweepArchived(ctx context.Context, ttl time.Duration, batchLimit int, workspace string) (int64, time.Time, error) {
	if ttl <= 0 {
		return 0, time.Time{}, fmt.Errorf("archive ttl must be > 0")
	}
	if batchLimit < 1 {
		batchLimit = archiveSweepBatch
	}
	owner, err := sweepOwner()
	if err != nil {
		return 0, time.Time{}, err
	}
	wm, err := s.archiveWatermark(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	res := s.db.WithContext(ctx).Exec(
		`UPDATE cleaner_state SET owner = ?, expires_at = now() + interval '5 min' WHERE key = 'lease' AND expires_at < now()`,
		owner)
	if res.Error != nil {
		return 0, wm, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, wm, ErrSweepNotClaimed
	}
	defer s.releaseSweepLease(ctx, owner)

	var cutoff time.Time
	if err := s.db.WithContext(ctx).Raw(`SELECT now() - make_interval(secs => ?)`, ttl.Seconds()).Scan(&cutoff).Error; err != nil {
		return 0, wm, err
	}
	var swept int64
	for i := 0; i < 1000; i++ {
		c1, err := s.sweepCompactedBatch(ctx, cutoff, wm, batchLimit)
		if err != nil {
			return swept, wm, err
		}
		c2, err := s.sweepDeletedSessionBatch(ctx, cutoff, wm, batchLimit)
		if err != nil {
			return swept, wm, err
		}
		swept += c1.n + c2.n
		next := advanceSweepWatermark(wm, c1, c2)
		if !c1.full && !c2.full {
			wm = next
			break
		}
		if !next.After(wm) {
			break
		}
		wm = next
	}
	if err := s.writeArchiveWatermark(ctx, wm); err != nil {
		return swept, wm, err
	}
	swept += sweepSubagentFiles(workspace, ttl, time.Now().UTC())
	return swept, wm, nil
}

func advanceSweepWatermark(prev time.Time, cursors ...sweepCursor) time.Time {
	var fulls []time.Time
	for _, c := range cursors {
		if c.full && !c.edge.IsZero() {
			fulls = append(fulls, c.edge)
		}
	}
	if len(fulls) > 0 {
		wm := fulls[0]
		for _, t := range fulls[1:] {
			if t.Before(wm) {
				wm = t
			}
		}
		if wm.After(prev) {
			return wm
		}
		return prev
	}
	wm := prev
	for _, c := range cursors {
		if c.edge.After(wm) {
			wm = c.edge
		}
	}
	return wm
}

func (s *Store) archiveWatermark(ctx context.Context) (time.Time, error) {
	var rows []time.Time
	err := s.db.WithContext(ctx).Raw(`SELECT watermark FROM cleaner_state WHERE key = 'archive'`).Scan(&rows).Error
	if err != nil {
		return time.Time{}, err
	}
	if len(rows) == 0 {
		return time.Time{}, errors.New("cleaner_state archive row missing")
	}
	return rows[0], nil
}

func (s *Store) writeArchiveWatermark(ctx context.Context, wm time.Time) error {
	res := s.db.WithContext(ctx).Exec(`UPDATE cleaner_state SET watermark = ? WHERE key = 'archive'`, wm)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("cleaner_state archive row missing")
	}
	return nil
}

func (s *Store) releaseSweepLease(ctx context.Context, owner string) {
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE cleaner_state SET expires_at = now() WHERE key = 'lease' AND owner = ?`, owner).Error; err != nil {
		log.Printf("clean-sweep release: %v", err)
	}
}

func sweepOwner() (string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid()), nil
}

// sweepCompactedBatch 删过窗且已终态的 run 上 compacted 原文行。seq=0 摘要行留着。
// 边界取这一批里最晚的 finished_at，同刻的行一并删掉，避免 LIMIT 把同一时间切成两半后游标跳过后半。
func (s *Store) sweepCompactedBatch(ctx context.Context, cutoff, wm time.Time, batchLimit int) (sweepCursor, error) {
	edge, n, err := s.sweepEdge(ctx, `
WITH picked AS (
  SELECT r.finished_at AS edge
  FROM run_model_messages m
  JOIN runs r ON r.id = m.run_id
  WHERE m.compacted AND m.seq <> 0
    AND r.finished_at IS NOT NULL
    AND r.finished_at < ? AND r.finished_at > ?
  ORDER BY r.finished_at
  LIMIT ?
)
SELECT COUNT(*) AS n, MAX(edge) AS edge FROM picked`, cutoff, wm, batchLimit)
	if err != nil || n == 0 {
		return sweepCursor{}, err
	}
	res := s.db.WithContext(ctx).Exec(`
DELETE FROM run_model_messages m
 USING runs r
 WHERE m.run_id = r.id
   AND m.compacted AND m.seq <> 0
   AND r.finished_at IS NOT NULL
   AND r.finished_at > ? AND r.finished_at <= ? AND r.finished_at < ?`, wm, edge, cutoff)
	if res.Error != nil {
		return sweepCursor{}, res.Error
	}
	return sweepCursor{n: res.RowsAffected, edge: edge, full: n == int64(batchLimit)}, nil
}

// sweepDeletedSessionBatch 删过窗软删会话的全部模型消息，含 compacted=false 和 seq=0。
// 会话行、runs、runlog 留着。
func (s *Store) sweepDeletedSessionBatch(ctx context.Context, cutoff, wm time.Time, batchLimit int) (sweepCursor, error) {
	edge, n, err := s.sweepEdge(ctx, `
WITH picked AS (
  SELECT deleted_at AS edge
  FROM sessions
  WHERE deleted_at IS NOT NULL
    AND deleted_at < ? AND deleted_at > ?
  ORDER BY deleted_at
  LIMIT ?
)
SELECT COUNT(*) AS n, MAX(edge) AS edge FROM picked`, cutoff, wm, batchLimit)
	if err != nil || n == 0 {
		return sweepCursor{}, err
	}
	res := s.db.WithContext(ctx).Exec(`
DELETE FROM run_model_messages
 WHERE session_id IN (
   SELECT id FROM sessions
    WHERE deleted_at IS NOT NULL
      AND deleted_at > ? AND deleted_at <= ? AND deleted_at < ?)`, wm, edge, cutoff)
	if res.Error != nil {
		return sweepCursor{}, res.Error
	}
	return sweepCursor{n: res.RowsAffected, edge: edge, full: n == int64(batchLimit)}, nil
}

func (s *Store) sweepEdge(ctx context.Context, query string, args ...any) (time.Time, int64, error) {
	var row struct {
		N    int64
		Edge sql.NullTime
	}
	if err := s.db.WithContext(ctx).Raw(query, args...).Scan(&row).Error; err != nil {
		return time.Time{}, 0, err
	}
	if row.N == 0 || !row.Edge.Valid {
		return time.Time{}, 0, nil
	}
	return row.Edge.Time, row.N, nil
}

// sweepSubagentFiles 是清扫的第三类：只删 wire 文件，不碰表。
// 有 terminal 且 finished_at + ttl < now 的删；没有 terminal 且 created_at + ttl < now 的删。
// 相等不算过期，和 resume 的 Before 同一条线。删失败记日志，下轮再试。
func sweepSubagentFiles(root string, ttl time.Duration, now time.Time) int64 {
	if root == "" || ttl <= 0 {
		return 0
	}
	matches, err := filepath.Glob(filepath.Join(root, "tenants", "*", "sessions", "*", ".tako", "subagents", "*.jsonl"))
	if err != nil {
		log.Printf("clean-sweep files: %v", err)
		return 0
	}
	var n int64
	for _, path := range matches {
		expired, err := subagentWireExpired(path, ttl, now)
		if err != nil {
			log.Printf("clean-sweep file %s: %v", path, err)
			continue
		}
		if !expired {
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("clean-sweep file %s: %v", path, err)
			continue
		}
		n++
	}
	return n
}

func subagentWireExpired(path string, ttl time.Duration, now time.Time) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	lines := dropPartialTail(splitJSONL(b))
	if len(lines) == 0 {
		return false, nil
	}
	var term wireTerminal
	if json.Unmarshal(lines[len(lines)-1], &term) == nil && term.Type == "terminal" {
		finished, err := parseWireTime(term.FinishedAt)
		if err != nil {
			return false, nil
		}
		return finished.Add(ttl).Before(now), nil
	}
	var meta wireMetadata
	if json.Unmarshal(lines[0], &meta) != nil || meta.Type != "metadata" {
		return false, nil
	}
	created, err := parseWireTime(meta.CreatedAt)
	if err != nil {
		return false, nil
	}
	return created.Add(ttl).Before(now), nil
}

func (w *Worker) startArchiveSweeper() {
	if w == nil || w.store == nil || w.archiveTTL <= 0 {
		return
	}
	ttl := w.archiveTTL
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			swept, wm, err := w.store.SweepArchived(context.Background(), ttl, archiveSweepBatch, w.workspace)
			if err != nil && !errors.Is(err, ErrSweepNotClaimed) {
				log.Printf("clean-sweep: %v", err)
				continue
			}
			if errors.Is(err, ErrSweepNotClaimed) {
				swept = 0
			}
			log.Printf("clean-sweep: swept=%d watermark=%s", swept, wm.UTC().Format(time.RFC3339Nano))
		}
	}()
}

// devCleanSweep 同步跑一轮清扫。dev 组才挂这条路由，不带登录。
// 没抢到租约也是 200，swept 为 0，这样两个进程对打时输的一方有明确字节。
func (s *Server) devCleanSweep(c *gin.Context) {
	if s.worker == nil || s.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sweep_failed"})
		return
	}
	ttl := s.worker.archiveTTL
	if ttl <= 0 {
		log.Printf("POST /api/dev/clean-sweep: archive ttl 未配置")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sweep_failed"})
		return
	}
	swept, wm, err := s.store.SweepArchived(c.Request.Context(), ttl, archiveSweepBatch, s.worker.workspace)
	if errors.Is(err, ErrSweepNotClaimed) {
		swept = 0
		err = nil
	}
	if err != nil {
		log.Printf("POST /api/dev/clean-sweep: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sweep_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"swept":     swept,
		"watermark": wm.UTC().Format(time.RFC3339Nano),
	})
}
