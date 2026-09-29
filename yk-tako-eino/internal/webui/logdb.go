// Package webui 是最小 Web UI：HTTP 会话、SSE，以及比单条连接活得更久的事件库。
//
// 本文件是事件库。event_log 按 run 追加，seq 在写入事务里分配；
// run_archive 开跑时插入，终局信封和 is_completed 在同一事务里更新。
// REPL 不打开这个库。空路径使用内存库，只给测试；生产路径由 -web-db 传入。
package webui

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 注册 database/sql 驱动名 sqlite。纯 Go，不需要 cgo。
)

// LogDB 是 Web 进程里的那一个 SQLite。MaxOpenConns=1，再加 mu，
// 保证同一进程里 MAX(seq)+1 不会被两场 run 交错读到同一个值。
type LogDB struct {
	mu sync.Mutex
	db *sql.DB
}

// OpenLogDB 打开事件库并建表。path 为空时用内存库。
func OpenLogDB(path string) (*LogDB, error) {
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开事件库: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	d := &LogDB{db: db}
	if err := d.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

// Close 关闭底层连接。进程退出前调用，测试用它放开临时文件。
func (d *LogDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// sqliteDSN 拼 modernc 的文件 URI。_txlock=immediate 让 Begin 一开始就拿写锁，
// 避免 MAX+1 在锁升级前被另一条连接读走。busy_timeout 给锁竞争留出等待。
//
// 必须写成 file:路径，不能交给 net/url。相对路径没有前导斜杠时，url.URL
// 会编成 file://yktako-webui.db，主机名被当成库名，建表返回
// SQL logic error: out of memory (1)。
func sqliteDSN(path string) (string, error) {
	const params = "_txlock=immediate&_pragma=busy_timeout(5000)"
	if path == "" {
		return "file::memory:?cache=shared&" + params, nil
	}
	if strings.ContainsAny(path, "?\n\r") {
		return "", fmt.Errorf("事件库路径不能包含 ?")
	}
	return "file:" + path + "?" + params, nil
}

// migrate 建事件表、档案表和压缩检索表。重复打开同一文件是无害的，已有库会补上 compact_rag。
func (d *LogDB) migrate(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS event_log (
  run_id  TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  payload TEXT    NOT NULL,
  PRIMARY KEY (run_id, seq)
);
CREATE TABLE IF NOT EXISTS run_archive (
  run_id        TEXT    PRIMARY KEY,
  session_id    TEXT    NOT NULL,
  status        TEXT    NOT NULL,
  is_completed  INTEGER NOT NULL,
  user_message  TEXT    NOT NULL,
  produced_json TEXT,
  created_at    TEXT    NOT NULL,
  finished_at   TEXT
);
CREATE TABLE IF NOT EXISTS compact_rag (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL,
  run_id     TEXT    NOT NULL,
  body       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS compact_rag_session ON compact_rag(session_id);`)
	if err != nil {
		return fmt.Errorf("建事件库表: %w", err)
	}
	return nil
}

// Reconcile 把上次进程没写上终局的 run 标成 failed。
// is_completed 保持 0：这些行没有终局信封，不能算「我们知道它怎么结束的」。
// 已落盘的 event_log 不动，回放仍然可读。
func (d *LogDB) Reconcile(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `UPDATE run_archive SET status = 'failed' WHERE status = 'running'`)
	if err != nil {
		return fmt.Errorf("对账未完成的 run: %w", err)
	}
	return nil
}

// newRunID 造一场 run 的 id。不用 session.turn：那个计数重启后归零，会和库里的旧行撞车。
func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 run id: %w", err)
	}
	return "r_" + hex.EncodeToString(b[:]), nil
}

// BeginRun 在调用模型之前插入档案行，并写下 seq=1 的 start 信封。两步同一事务。
func (d *LogDB) BeginRun(ctx context.Context, runID, sessionID, userMessage, agent string) (sseEvent, error) {
	var out sseEvent
	err := d.tx(ctx, func(tx *sql.Tx) error {
		now := nowRFC3339()
		_, err := tx.ExecContext(ctx, `
INSERT INTO run_archive(run_id, session_id, status, is_completed, user_message, created_at)
VALUES (?, ?, 'running', 0, ?, ?)`, runID, sessionID, userMessage, now)
		if err != nil {
			return err
		}
		ev, err := insertEvent(ctx, tx, runID, sseEvent{
			Kind: "start", Agent: agent, Role: "user", Text: userMessage,
		})
		if err != nil {
			return err
		}
		out = ev
		return nil
	})
	if err != nil {
		return sseEvent{}, fmt.Errorf("开始 run: %w", err)
	}
	return out, nil
}

// Append 给仍在 running 的 run 追加一帧。seq 由库分配，调用方带上的 seq 不算数。
func (d *LogDB) Append(ctx context.Context, ev sseEvent) (sseEvent, error) {
	if ev.Run == "" {
		return sseEvent{}, fmt.Errorf("追加事件缺少 run")
	}
	var out sseEvent
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM run_archive WHERE run_id = ?`, ev.Run).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s 不存在", ev.Run)
		}
		if err != nil {
			return err
		}
		if status != "running" {
			return fmt.Errorf("run %s 已是 %s，不再追加", ev.Run, status)
		}
		ev.Seq = 0
		stored, err := insertEvent(ctx, tx, ev.Run, ev)
		if err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil {
		return sseEvent{}, fmt.Errorf("追加事件: %w", err)
	}
	return out, nil
}

// AppendSealed 把已经在内存里分配过序号的正文写入事件表。
// 序号必须正好是当前最大序号加一。半截流出期间不调用它。
func (d *LogDB) AppendSealed(ctx context.Context, ev sseEvent) (sseEvent, error) {
	if ev.Run == "" || ev.Seq <= 0 {
		return sseEvent{}, fmt.Errorf("落库正文缺少 run 或 seq")
	}
	ev.Partial = false
	ev.Failed = false
	var out sseEvent
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM run_archive WHERE run_id = ?`, ev.Run).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s 不存在", ev.Run)
		}
		if err != nil {
			return err
		}
		if status != "running" {
			return fmt.Errorf("run %s 已是 %s，不再追加", ev.Run, status)
		}
		var max sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM event_log WHERE run_id = ?`, ev.Run).Scan(&max); err != nil {
			return err
		}
		next := int64(1)
		if max.Valid {
			next = max.Int64 + 1
		}
		if ev.Seq != next {
			return fmt.Errorf("正文序号 %d 与库里的下一号 %d 不一致", ev.Seq, next)
		}
		stored, err := insertEventAt(ctx, tx, ev.Run, ev)
		if err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil {
		return sseEvent{}, fmt.Errorf("落库正文: %w", err)
	}
	return out, nil
}

// HasEvent 报告事件表里是否已有这个序号。
func (d *LogDB) HasEvent(ctx context.Context, runID string, seq int64) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE run_id = ? AND seq = ?`, runID, seq).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RestoreMessage 把浏览器补来的一条已完成正文写入库。
// 这轮还在跑则不写，交给正在流出的进程自己收尾。
// 已经有这个序号则不写。写入后若档案还没有产出，把这句收成已完成轮次。
func (d *LogDB) RestoreMessage(ctx context.Context, sessionID string, ev sseEvent) (bool, error) {
	if ev.Run == "" || ev.Seq <= 0 || ev.Text == "" {
		return false, fmt.Errorf("补发缺少 run、seq 或正文")
	}
	var stored bool
	err := d.tx(ctx, func(tx *sql.Tx) error {
		var status, owner string
		var produced sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT status, session_id, produced_json FROM run_archive WHERE run_id = ?`, ev.Run).Scan(&status, &owner, &produced)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s 不存在", ev.Run)
		}
		if err != nil {
			return err
		}
		if owner != sessionID {
			return fmt.Errorf("run %s 不属于这个会话", ev.Run)
		}
		if status == "running" {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE run_id = ? AND seq = ?`, ev.Run, ev.Seq).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		var max sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM event_log WHERE run_id = ?`, ev.Run).Scan(&max); err != nil {
			return err
		}
		limit := int64(1)
		if max.Valid {
			limit = max.Int64 + 1
		}
		if ev.Seq > limit {
			return fmt.Errorf("补发序号 %d 超过下一号 %d", ev.Seq, limit)
		}
		ev.Partial = false
		ev.Failed = false
		if ev.Kind == "" {
			ev.Kind = "message"
		}
		if _, err := insertEventAt(ctx, tx, ev.Run, ev); err != nil {
			return err
		}
		if !produced.Valid {
			raw, err := json.Marshal([]*schemaMessage{{Role: "assistant", Content: ev.Text}})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE run_archive
   SET status = 'completed', is_completed = 1, produced_json = ?, finished_at = COALESCE(finished_at, ?)
 WHERE run_id = ?`, string(raw), nowRFC3339(), ev.Run); err != nil {
				return err
			}
		}
		stored = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("补发正文: %w", err)
	}
	return stored, nil
}

// schemaMessage 只用于补发时拼一条助手消息，避免 logdb 依赖 eino 的消息类型。
type schemaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ArchivePatch 是成功收尾时和 produced_json 同一笔写入的档案修改。
// 发送前不改 run_archive。这一轮没完成时调用方不要把补丁传进来。
type ArchivePatch struct {
	// UserMessage 非空时覆盖当前这一行的 user_message。空字符串表示这一列保持原话。
	UserMessage string
	// DropRunIDs 这些行的 produced_json 清成 NULL，CompletedTurns 就不再读它们。
	DropRunIDs []string
}

// Finish 写下唯一的终局信封，并把档案标成 completed。两步同一事务。
// kind 只接受 complete / error / stop。只有 complete 写入 producedJSON；
// 失败和停止不把产出放进模型历史，档案里也不留。
// 档案行已经不是 running 时整笔回滚，因此第二次收尾不会多出一帧。
func (d *LogDB) Finish(ctx context.Context, runID, kind, text string, produced []byte) (sseEvent, error) {
	return d.finish(ctx, runID, kind, text, produced, nil)
}

// FinishWithPatch 在 Finish 的同一事务里改当前行的 user_message，并清空被换掉的行。
// 事务失败时两样都不留。kind 不是 complete 时补丁被忽略。
func (d *LogDB) FinishWithPatch(ctx context.Context, runID, kind, text string, produced []byte, patch ArchivePatch) (sseEvent, error) {
	return d.finish(ctx, runID, kind, text, produced, &patch)
}

// finish 是 Finish 和 FinishWithPatch 的共同写入。patch 为 nil 时行为和原来的 Finish 一样。
func (d *LogDB) finish(ctx context.Context, runID, kind, text string, produced []byte, patch *ArchivePatch) (sseEvent, error) {
	switch kind {
	case "complete", "error", "stop":
	default:
		return sseEvent{}, fmt.Errorf("终局 kind 非法: %s", kind)
	}
	var out sseEvent
	err := d.tx(ctx, func(tx *sql.Tx) error {
		ev, err := insertEvent(ctx, tx, runID, sseEvent{Kind: kind, Text: text})
		if err != nil {
			return err
		}
		var producedArg any
		if kind == "complete" && len(produced) > 0 {
			producedArg = string(produced)
		}
		res, err := tx.ExecContext(ctx, `
UPDATE run_archive
   SET status = 'completed', is_completed = 1, produced_json = ?, finished_at = ?
 WHERE run_id = ? AND status = 'running'`, producedArg, nowRFC3339(), runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("run %s 不能收尾", runID)
		}
		if kind == "complete" && patch != nil {
			if err := applyArchivePatch(ctx, tx, runID, patch); err != nil {
				return err
			}
		}
		out = ev
		return nil
	})
	if err != nil {
		return sseEvent{}, fmt.Errorf("收尾 run: %w", err)
	}
	return out, nil
}

// applyArchivePatch 在调用方的事务里改档案。当前 run 的 produced_json 不清空。
func applyArchivePatch(ctx context.Context, tx *sql.Tx, runID string, patch *ArchivePatch) error {
	if patch.UserMessage != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE run_archive SET user_message = ? WHERE run_id = ?`, patch.UserMessage, runID); err != nil {
			return err
		}
	}
	for _, id := range patch.DropRunIDs {
		if id == "" || id == runID || strings.HasPrefix(id, "idx:") {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run_archive SET produced_json = NULL WHERE run_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// readAll 按 seq 读出一场 run 的全部事件。给重启后把列表装回内存用。
func (d *LogDB) readAll(ctx context.Context, runID string) ([]sseEvent, error) {
	var all []sseEvent
	var from int64
	for {
		batch, err := d.ReadAfter(ctx, runID, from, 200)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return all, nil
		}
		all = append(all, batch...)
		from = batch[len(batch)-1].Seq
		if len(batch) < 200 {
			return all, nil
		}
	}
}

// storedTurn 是一轮已经写上产出的档案。下一轮模型只读这些行。
type storedTurn struct {
	RunID        string
	UserMessage  string
	ProducedJSON string
}

// HistoryRun 是一条已结束 run 的档案和事件，用于历史分页 API 的响应。
type HistoryRun struct {
	RunID     string     `json:"runId"`
	CreatedAt string     `json:"createdAt"`
	Status    string     `json:"status"`
	Events    []sseEvent `json:"events"`
}

// HistoryPage 是一页历史 run，用于历史分页 API 的响应。
type HistoryPage struct {
	Runs       []HistoryRun `json:"runs"`
	NextCursor string       `json:"nextCursor"`
}

// CompletedTurns 返回本会话已完成、且写了产出的轮次，按开跑时间排序。
// 失败、停止和对账成 failed 的行没有 produced_json，不在结果里。
// 压缩收尾时被清空的行同样被跳过。
func (d *LogDB) CompletedTurns(ctx context.Context, sessionID string) ([]storedTurn, error) {
	rows, err := d.db.QueryContext(ctx, `
SELECT run_id, user_message, produced_json
  FROM run_archive
 WHERE session_id = ? AND status = 'completed' AND produced_json IS NOT NULL
 ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取已完成轮次: %w", err)
	}
	defer rows.Close()
	var out []storedTurn
	for rows.Next() {
		var turn storedTurn
		if err := rows.Scan(&turn.RunID, &turn.UserMessage, &turn.ProducedJSON); err != nil {
			return nil, err
		}
		out = append(out, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// HistoryPage 返回会话里已结束的 run，按 created_at 降序，最多 limit 条。
// cursor 非空时格式为 "<created_at>|<run_id>"，只返回更早的 run。
// 不返回 status='running' 的行——正在跑的 run 由 /api/session 报告。
func (d *LogDB) HistoryPage(ctx context.Context, sessionID string, cursor string, limit int) (HistoryPage, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var beforeTime, beforeRunID string
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 2)
		if len(parts) != 2 {
			return HistoryPage{}, fmt.Errorf("cursor 格式错误，应为 <created_at>|<run_id>")
		}
		beforeTime, beforeRunID = parts[0], parts[1]
	}

	var rows *sql.Rows
	var err error
	if beforeTime == "" {
		rows, err = d.db.QueryContext(ctx, `
SELECT run_id, created_at, status
  FROM run_archive
 WHERE session_id = ? AND status != 'running'
 ORDER BY created_at DESC, run_id DESC
 LIMIT ?`, sessionID, limit+1)
	} else {
		rows, err = d.db.QueryContext(ctx, `
SELECT run_id, created_at, status
  FROM run_archive
 WHERE session_id = ? AND status != 'running'
   AND (created_at < ? OR (created_at = ? AND run_id < ?))
 ORDER BY created_at DESC, run_id DESC
 LIMIT ?`, sessionID, beforeTime, beforeTime, beforeRunID, limit+1)
	}
	if err != nil {
		return HistoryPage{}, fmt.Errorf("查询历史 run: %w", err)
	}
	defer rows.Close()

	type runMeta struct {
		runID     string
		createdAt string
		status    string
	}
	var metas []runMeta
	for rows.Next() {
		var m runMeta
		if err := rows.Scan(&m.runID, &m.createdAt, &m.status); err != nil {
			return HistoryPage{}, err
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return HistoryPage{}, err
	}

	var nextCursor string
	if len(metas) > limit {
		metas = metas[:limit]
		last := metas[len(metas)-1]
		nextCursor = last.createdAt + "|" + last.runID
	}

	runs := make([]HistoryRun, 0, len(metas))
	for _, m := range metas {
		events, err := d.readAll(ctx, m.runID)
		if err != nil {
			return HistoryPage{}, fmt.Errorf("读取 run %s 的事件: %w", m.runID, err)
		}
		runs = append(runs, HistoryRun{
			RunID:     m.runID,
			CreatedAt: m.createdAt,
			Status:    m.status,
			Events:    events,
		})
	}

	return HistoryPage{Runs: runs, NextCursor: nextCursor}, nil
}

// WriteRAG 把一轮被换掉的原文追加进 compact_rag。空正文和 idx: 占位不写。
// 调用方把写入失败记成日志，不因此取消这一轮。
func (d *LogDB) WriteRAG(ctx context.Context, sessionID, runID, body string) error {
	if strings.TrimSpace(body) == "" || sessionID == "" || runID == "" || strings.HasPrefix(runID, "idx:") {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.ExecContext(ctx, `INSERT INTO compact_rag(session_id, run_id, body) VALUES (?, ?, ?)`, sessionID, runID, body)
	if err != nil {
		return fmt.Errorf("写入 compact_rag: %w", err)
	}
	return nil
}

// RAGBodies 按写入顺序返回这一会话的检索原文。没有行时是空切片。
func (d *LogDB) RAGBodies(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT body FROM compact_rag WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取 compact_rag: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		out = append(out, body)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadAfter 返回 seq 严格大于 from 的帧，按 seq 升序，最多 limit 条。
// from<=0 表示从头。超过最大 seq、或 run 不存在，都是空切片而不是错误。
func (d *LogDB) ReadAfter(ctx context.Context, runID string, from int64, limit int) ([]sseEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.QueryContext(ctx, `
SELECT payload FROM event_log
 WHERE run_id = ? AND seq > ?
 ORDER BY seq
 LIMIT ?`, runID, from, limit)
	if err != nil {
		return nil, fmt.Errorf("读取事件: %w", err)
	}
	defer rows.Close()
	var out []sseEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf("解析事件: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// archiveView 是测试和对账要看的档案字段。不对外暴露 SQL。
type archiveView struct {
	Status       string
	IsCompleted  int
	UserMessage  string
	ProducedJSON sql.NullString
}

func (d *LogDB) lookupArchive(ctx context.Context, runID string) (archiveView, error) {
	var v archiveView
	err := d.db.QueryRowContext(ctx, `
SELECT status, is_completed, user_message, produced_json
  FROM run_archive WHERE run_id = ?`, runID).Scan(&v.Status, &v.IsCompleted, &v.UserMessage, &v.ProducedJSON)
	if err != nil {
		return archiveView{}, err
	}
	return v, nil
}

// insertEvent 在调用方的事务里分配 seq 并插入。列上的 seq 与 payload 里的 seq 是这一个整数。
func insertEvent(ctx context.Context, tx *sql.Tx, runID string, ev sseEvent) (sseEvent, error) {
	var max sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM event_log WHERE run_id = ?`, runID).Scan(&max); err != nil {
		return sseEvent{}, err
	}
	seq := int64(1)
	if max.Valid {
		seq = max.Int64 + 1
	}
	ev.Run = runID
	ev.Seq = seq
	return insertEventAt(ctx, tx, runID, ev)
}

// insertEventAt 按调用方给的序号插入。列上的 seq 与 payload 里的 seq 是这一个整数。
func insertEventAt(ctx context.Context, tx *sql.Tx, runID string, ev sseEvent) (sseEvent, error) {
	ev.Run = runID
	if ev.TS == "" {
		ev.TS = nowRFC3339()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return sseEvent{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_log(run_id, seq, payload) VALUES (?, ?, ?)`, runID, ev.Seq, string(payload)); err != nil {
		return sseEvent{}, err
	}
	return ev, nil
}

func (d *LogDB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = d.txOnce(ctx, fn)
		if err == nil || !isUnique(err) || attempt == 1 {
			return err
		}
	}
	return err
}

func (d *LogDB) txOnce(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
