// 本文件是存储层的 PG 半：五张表的 GORM model、建表（AutoMigrate），以及纯 PG 的读方法。
//
// 边界（planning §8）：**GORM 只管「行 ↔ struct」**——契约 JSON（Row/Frame 的字段序）由 Go 结构体
// 渲染，不经 GORM；全设计零 JSONB。表名/列名即 19v6-03 与 planning §8.2 的定稿形态。
//
// 「闸在手上」的统一谓词（三处共用：读侧 / endRun 守卫 / 清扫的补集）：
//
//	status='running' AND lease_until > now()
//
// ——它就是现役 Redis 形态「tk:live 键存在且等于本 run」的 DB 等价物（A7 的租约语义）。
package runengine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// Session = `sessions` 表（19v6-03 + doc/28 A1）：会话登记，存在即已登记（懒建于 POST 事务内）。永久状态。
// TenantID/UserID 是归属；Visibility 默认 private，tenant 表示同租户成员只读。
type Session struct {
	ID         SessionID `gorm:"primaryKey"`
	Title      string    `gorm:"not null"` // 展示字段（会话列表用），非契约；初值 = 首条用户消息截断
	TenantID   string    `gorm:"not null;default:'';index:sessions_tenant_visibility,priority:1"`
	UserID     string    `gorm:"not null;default:''"`
	Visibility string    `gorm:"not null;default:private;index:sessions_tenant_visibility,priority:2"`
	AgentID    string    `gorm:"not null;default:''"`
	CreatedAt  time.Time `gorm:"not null;index:sessions_tenant_visibility,priority:3"`
}

func (Session) TableName() string { return "sessions" }

// Run = `runs` 表：run = 闸 + 状态 + 租约（执行单元本身），不存消息文本（消息在 run_model_messages）。
// 闸本体 = 部分唯一索引 one_live_run_per_session（status='running' 的行上 session_id 唯一）——
// INSERT 冲突就是 409 的证据（AGENTS.md「闸放数据库不放进程」，19v6-01 §1 点名此索引名）。
type Run struct {
	ID         RunID      `gorm:"primaryKey"`
	SessionID  SessionID  `gorm:"not null;uniqueIndex:one_live_run_per_session,where:status = 'running';index:runs_session_status_lease,priority:1"`
	Status     string     `gorm:"not null;index:runs_session_status_lease,priority:2"` // running / complete / error / stopped
	LeaseUntil *time.Time `gorm:"index:runs_session_status_lease,priority:3"`          // 活行租约（终态置 NULL）；出生即带，watchdog 续
	// LeaseEpoch 是接管令牌（doc/30 §7）：出生 = 1，每一次 claim 续跑 +1；所有写点带它，
	// 被接管后旧进程的每次写都自然失败。default:1 让 AutoMigrate 给存量 running 行补 1。
	LeaseEpoch int64     `gorm:"not null;default:1"`
	CreatedAt  time.Time `gorm:"not null"`
	FinishedAt *time.Time
}

func (Run) TableName() string { return "runs" }

// RunModelMessage = `run_model_messages` 表：run 内的模型侧消息序列（model_history 的落库形态）。
// worker 组装模型输入的唯一读源（单表：WHERE session_id AND compacted=false ORDER BY run_id, seq）。
// 表名即真源划分——模型侧投影，历史展示永不读它（只读 runlog）。
// 主键 id（msg_<ULID>）供未来 tool_call/HITL 引用某条消息；与契约字段 messageId（客户端生成的 m_1）是两回事。
// T2 加 tool_calls / tool_call_id 两列：重建 OpenAI 工具消息（assistant 带 calls、tool 带回指）。
// 一次工具循环的 4 行样例（同一 run 内）：
//
//	Seq  Role       Text                                         ToolCalls                                                                                             ToolCallID  说明
//	1    user       "现在几点？"                                  ""                                                                                                    ""          POST 投递
//	2    assistant  ""                                           `[{"id":"call_abc","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"Asia/Shanghai\"}"}}]`  ""          模型返回工具调用，原生 JSON 不重编码
//	3    tool       "2026-10-10T15:30:00+08:00"                 ""                                                                                                    "call_abc"  工具结果，tool_call_id 必须与上一行某条 id 字面完全相等（模型靠字符串关联，非行号）
//	4    assistant  "现在是 15:30。"                              ""                                                                                                    ""          终稿正文
type RunModelMessage struct {
	ID         string    `gorm:"primaryKey"`
	SessionID  SessionID `gorm:"not null;index:run_model_messages_input,where:compacted = false"` // 冗余列：消息不跨 run/session 移动
	RunID      RunID     `gorm:"not null;uniqueIndex:run_model_messages_run_seq,priority:1"`
	Seq        int       `gorm:"not null;uniqueIndex:run_model_messages_run_seq,priority:2"` // run 内序号，从 1 起；0 = compact 摘要行
	Role       string    `gorm:"not null"`                                                   // OpenAI 标准枚举：system/user/assistant/tool（本步产生 user/assistant/tool）
	Text       string    `gorm:"not null"`
	ToolCalls  string    // assistant 行：[]schema.ToolCall 的原生 JSON（Index 保留，回放/续轮原样）——可空
	ToolCallID string    // tool 行：回指 assistant 行 tool_calls 里的 id——可空
	Compacted  bool      `gorm:"not null"` // true = 被 compact 压掉的原文行：保留作审计，不进模型输入
	CreatedAt  time.Time `gorm:"not null"`
}

func (RunModelMessage) TableName() string { return "run_model_messages" }

// RunlogEntry = `runlog_entries` 表：历史真源；seq 从 1 起连续——**seq 即 runlogOffset 的列形态**，
// 契约词汇（runlogOffset/nextOffset）不变；列名避用 offset（PG 保留字）。
// 只增不删：「seq N 恒指同一条消息」是客户端对齐游标只前进的地基。
// T2 加 agent / payload 两列：agent 空 = root（T2 恒空，T3 起子代理行非空）；
// payload 只有工具扩展行（role=tool_call/tool_result）有，存帧同款结构化负载的 JSON。
// T3-2 加 model 列：产出该行的模型生效 ref——user 行为空（非模型产物），root 行 = 主 ref，
// 子代理行 = 该规格的生效 ref（md 改 model 的持久验证面，19v6-05 §6）。
type RunlogEntry struct {
	ID        string    `gorm:"primaryKey"` // rl_<ULID>
	SessionID SessionID `gorm:"not null;uniqueIndex:runlog_entries_session_seq,priority:1"`
	Seq       int64     `gorm:"not null;uniqueIndex:runlog_entries_session_seq,priority:2"` // session 内序号 = runlogOffset
	RunID     RunID     `gorm:"not null"`
	Role      string    `gorm:"not null"` // user / assistant / tool_call / tool_result（扩展行随 T2 工具循环）
	Agent     string    // 产出该行的 agent 名；空 = root
	Model     string    // 产出该行的模型生效 ref（T3-2）；空 = 非模型产物（user 行）
	MessageID string    // 仅 user 行（契约：assistant 行无此字段）——可空
	Text      string    `gorm:"not null"`
	Payload   string    // 工具扩展行的结构化 JSON（{"name","args"|"output"}）——可空
	CreatedAt time.Time `gorm:"not null"`
}

func (RunlogEntry) TableName() string { return "runlog_entries" }

// RunJournal = `run_journal` 表（doc/30 §6.2）：在途 run 的崩溃账本，**终态即删**（不是历史）。
// 与 runlog 的分工：runlog 只记已定稿的消息、永不删；账本记「模型说到哪、哪些工具可能跑过」，
// 供崩溃续跑重放出模型输入，正常结束/清扫/停止后立即清空。
// seq 由 PG bigserial 发号（GORM autoIncrement）：root 多个工具在 ToolsNode 里并行执行，
// MAX+1 会撞号；重放按 call id 归组，不按 seq 猜配对（§9.1）。
// 三个 role：assistant（模型一枪的合并全文 + tool_calls）/ tool_call（执行前 intent）/
// tool_result（执行后回执）。空串列一律写 ”（不是 NULL）——dev 端点逐字段出，便于 curl 断言。
type RunJournal struct {
	Seq        int64     `gorm:"primaryKey;autoIncrement"`
	RunID      RunID     `gorm:"not null;index:run_journal_run_id"`
	Epoch      int64     `gorm:"not null"`
	Role       string    `gorm:"not null"`
	Text       string    `gorm:"not null"`
	ToolCalls  string    `gorm:"not null;default:''"`
	ToolCallID string    `gorm:"not null;default:''"`
	ToolName   string    `gorm:"not null;default:''"`
	Args       string    `gorm:"not null;default:''"`
	Model      string    `gorm:"not null;default:''"`
	Usage      string    `gorm:"not null;default:''"`
	CreatedAt  time.Time `gorm:"not null"`
}

func (RunJournal) TableName() string { return "run_journal" }

// autoMigrate 从九个 struct 建表（dev 迁移方式，待定项①已裁：AutoMigrate 够用，无迁移文件；
// 上生产换不换显式迁移归部署面）。部分索引（WHERE 条件）优先走 GORM index tag；
// 若某版 GORM 表达不全，在这里补 CREATE ... IF NOT EXISTS 兜底——仍是代码内，不引迁移文件。
// 后三张是 doc/29 长期记忆：memory_items / memory_jobs（线一）与 dialogue_chunks（线二，
// pg_search 索引另由 ensureDialogueIndex 手写；建表本身不需要扩展）。
func autoMigrate(db *gorm.DB) error {
	// 旧库上的 memory_items_dedup 不含 user_id。先丢掉，AutoMigrate 按新列重建，
	// 再建一次显式部分索引，保证 ON CONFLICT 的列和 WHERE 与代码一致。
	_ = db.Exec(`DROP INDEX IF EXISTS memory_items_dedup`).Error
	_ = db.Exec(`DROP INDEX IF EXISTS idx_memory_items_memory_items_dedup`).Error
	if err := db.AutoMigrate(&Session{}, &Run{}, &RunModelMessage{}, &RunlogEntry{}, &Note{}, &RunJournal{},
		&MemoryItem{}, &MemoryJob{}, &DialogueChunk{},
		&User{}, &Tenant{}, &TenantMember{}, &RefreshToken{}, &AuditLog{}, &TenantUsageDaily{},
		&TenantInvitation{}, &TenantAgent{}, &AgentDefault{}, &TenantSetting{}, &TenantStorage{}); err != nil {
		return err
	}
	_ = db.Exec(`DROP INDEX IF EXISTS memory_items_dedup`).Error
	return db.Exec(`CREATE UNIQUE INDEX memory_items_dedup
ON memory_items (tenant_id, user_id, workspace, kind, content)
WHERE kind <> 'episode'`).Error
}

// newRowID 生成行级代理主键：前缀 + ULID（同 s_/r_ 的铸法）。
func newRowID(prefix string) (string, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return prefix + id.String(), nil
}

// Exists 报告 session 是否已登记（带 sessionId 投递时的准入检查）。
func (s *Store) Exists(ctx context.Context, sessionID SessionID) (bool, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&Session{}).Where("id = ?", sessionID).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// CountMediaPlaceholders 数会话里已有的 [media 占位**出现次数**（doc/32 A6）——一条 user 行
// 可带最多 4 个占位，按行数会漏编。唯一用途是给新消息的图起 seq（全会话累计序号）：
// 占位行只由 user 行携带，同会话同时只有一个 run 在写（409 忙闸），准入期读无并发竞争；
// 计数在 PG 不在进程，多副本下成立。marker 无 LIKE 通配符（无 % _），原样匹配安全。
func (s *Store) CountMediaPlaceholders(ctx context.Context, sessionID SessionID) (int, error) {
	const marker = "[media "
	var n int64
	err := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Select("COALESCE(SUM((length(text) - length(replace(text, ?, ''))) / length(?)), 0)", marker, marker).
		Where("session_id = ? AND role = 'user' AND text LIKE ?", sessionID, "%"+marker+"%").
		Scan(&n).Error
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// SessionStatus 返回状态查询五要素（19v6-01 §4 三要素 + doc/17 §4 手动停止加的 lastRunStatus
// + doc/30 A1 加的 lastRunId）：活 run id（"" = 闲着）、runlog 高水位、是否已登记、
// 该会话最新一场 run 的终态（无 run = ""）、该 run 的 id（无 run = ""）。
// 单条 SQL 子查询取一致快照（原 MULTI 的等价物，预登记裁定）。liveRunId 读侧即回收：
// lease 已过的死行当场不再算活，不等清扫（清扫只为放行新 run，planning §8.3）。
// lastRunStatus 是 closeOut 无 terminal 帧路径（刷新 / 掉线重连）判断「停过没」的依据——
// stopped 不写 assistant 行，只看 log 会把「已停止」猜成「失败」。
// running + 租约已过（doc/30 A1）读侧派生为 interrupted：崩溃后的 run 第一次对状态端点可见，
// 不必等下一次 POST 的清扫把它翻成 error。lastRunId 与 lastRunStatus 同一 ORDER BY，指同一场 run。
func (s *Store) SessionStatus(ctx context.Context, sessionID SessionID, p principal) (liveRunID RunID, runlogOffset int, lastStatus string, lastRunID RunID, exists bool, err error) {
	if _, _, err := s.SessionAccess(ctx, sessionID, p); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return "", 0, "", "", false, nil
		}
		return "", 0, "", "", false, err
	}
	var live, last string
	var hw int64
	err = s.db.WithContext(ctx).Raw(`
SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ?),
       COALESCE((SELECT id FROM runs WHERE session_id = ? AND status = 'running' AND lease_until > now() LIMIT 1), ''),
       COALESCE((SELECT MAX(seq) FROM runlog_entries WHERE session_id = ?), 0),
       COALESCE((SELECT id FROM runs WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT 1), ''),
       COALESCE((SELECT CASE WHEN status = 'running' AND lease_until > now() THEN 'running'
                             WHEN status = 'running' THEN 'interrupted'
                             ELSE status END
                 FROM runs WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT 1), '')`,
		sessionID, sessionID, sessionID, sessionID, sessionID).Row().Scan(&exists, &live, &hw, &last, &lastStatus)
	if err != nil {
		return "", 0, "", "", false, err
	}
	return RunID(live), int(hw), lastStatus, RunID(last), exists, nil
}

// RunState 是 GET /api/runs/{id} 的读侧派生结果（doc/30 §5.1）。状态不新增存储值：
// running + 租约过期当场派生 interrupted，resumeCount 由 epoch-1 推出，resumable 只是它与上限的比较。
type RunState struct {
	RunID       RunID
	SessionID   SessionID
	Status      string
	ResumeCount int
	Resumable   bool
}

// RunStatus 取一场 run 的对外状态；未知 run → found=false（HTTP 层回 404）。
// 单条 SQL 一并拿回租约是否仍然有效——判定用 PG now()，多副本共用一个时钟，不用进程钟。
// interrupted 是派生值不是存储值（runs.status 仍是 running）：读侧不要求先有人清扫。
func (s *Store) RunStatus(ctx context.Context, runID RunID, resumeMax int) (st RunState, found bool, err error) {
	var row struct {
		SessionID  SessionID
		Status     string
		LeaseEpoch int64
		Alive      bool
	}
	res := s.db.WithContext(ctx).Raw(`
SELECT session_id, status, lease_epoch, COALESCE(lease_until > now(), false) AS alive
  FROM runs WHERE id = ?`, runID).Scan(&row)
	if res.Error != nil {
		return RunState{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return RunState{}, false, nil
	}
	status := row.Status
	if status == "running" {
		if row.Alive {
			status = "running"
		} else {
			status = "interrupted"
		}
	}
	count := int(row.LeaseEpoch - 1)
	if count < 0 {
		count = 0
	}
	return RunState{
		RunID:       runID,
		SessionID:   row.SessionID,
		Status:      status,
		ResumeCount: count,
		Resumable:   status == "interrupted" && count < resumeMax,
	}, true, nil
}

// InsertJournal 追加一行账本（doc/30 §6.3 的三处写入共用）。seq 由 bigserial 发号，不读回。
// A3 起是短事务：先 FOR SHARE 锁 runs 行验「仍在跑且令牌还是我的」，再 INSERT——防「claim
// 提交后旧 epoch 的 intent 才落账」（旧执行者的写必须自然失败）。FOR SHARE 与 claim 的
// 行锁互斥：claim 提交时本事务要么已提交（账本属于旧代，重放会看到它）、要么被挡到 claim
// 之后重验（0 行 → errJournalGateLost）。写失败由调用方升级为 run error（账本是恢复唯一依据，
// 不静默降级）。
func (s *Store) InsertJournal(ctx context.Context, row RunJournal) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var one int
		res := tx.Raw(
			`SELECT 1 FROM runs WHERE id = ? AND status = 'running' AND lease_epoch = ? FOR SHARE`,
			row.RunID, row.Epoch).Scan(&one)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errJournalGateLost
		}
		return tx.Exec(
			`INSERT INTO run_journal (run_id, epoch, role, text, tool_calls, tool_call_id, tool_name, args, model, usage, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, now())`,
			row.RunID, row.Epoch, row.Role, row.Text, row.ToolCalls, row.ToolCallID,
			row.ToolName, row.Args, row.Model, row.Usage).Error
	})
}

// JournalRows 读本 run 的账本全序（seq 升序）。重放按 call id 归组，seq 只用于稳定顺序。
func (s *Store) JournalRows(ctx context.Context, runID RunID) ([]RunJournal, error) {
	var rows []RunJournal
	err := s.db.WithContext(ctx).Where("run_id = ?", runID).Order("seq").Find(&rows).Error
	return rows, err
}

// DeleteJournal 删本 run 的账本（EndRun / StopRun / 清扫三处在各自事务里内联同一条 DELETE；
// 本方法留给需要独立删除的调用方）。终态即删：账本不是历史，是恢复期的临时现场。
func (s *Store) DeleteJournal(ctx context.Context, runID RunID) error {
	return s.db.WithContext(ctx).Exec(`DELETE FROM run_journal WHERE run_id = ?`, runID).Error
}

// JournalView 是 dev 端点的一笔读：run 的当前 epoch + 账本全序。found=false = run 不存在
// （账本可能有孤儿行，但端点契约是 404——账本只对存在的 run 有意义）。
func (s *Store) JournalView(ctx context.Context, runID RunID) (epoch int64, rows []RunJournal, found bool, err error) {
	res := s.db.WithContext(ctx).Raw(`SELECT lease_epoch FROM runs WHERE id = ?`, runID).Scan(&epoch)
	if res.Error != nil {
		return 0, nil, false, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, nil, false, nil
	}
	rows, err = s.JournalRows(ctx, runID)
	if err != nil {
		return 0, nil, false, err
	}
	return epoch, rows, true, nil
}

// ModelMessages 返回 session 的模型侧消息全序（worker 组装本次生成的输入）——单表、双列过滤，无 JOIN。
// ORDER BY run_id, seq：run_id 是 ULID，字典序即 run 时序；seq 同理（0=摘要行排在本 run 最前）。
// user 半在 AcquireRun 入、assistant/tool 半在 EndRun(complete) 逐轮入——error 结局只留 user 那一半。
// run_id/seq 一并读出：compact 的分组与 seq=0 识别靠它（C 系列）；
// 被压行（compacted=true）读不出来是**有意的**——「压掉」= 不回模型输入，只在库里留档。
// T2：tool_calls 列在这里解成 []schema.ToolCall（坏 JSON = 硬错误，不静默吞——归一层写的，坏了就是坏了）。
func (s *Store) ModelMessages(ctx context.Context, sessionID SessionID) ([]ModelMessage, error) {
	var rows []RunModelMessage
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND compacted = false", sessionID).
		Order("run_id, seq").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ModelMessage, 0, len(rows))
	for _, r := range rows {
		var calls []schema.ToolCall
		if r.ToolCalls != "" {
			if err := json.Unmarshal([]byte(r.ToolCalls), &calls); err != nil {
				return nil, fmt.Errorf("mh %s tool_calls: %w", r.ID, err)
			}
		}
		out = append(out, ModelMessage{
			RunID: r.RunID, Seq: r.Seq, Role: r.Role, Text: r.Text,
			ToolCalls: calls, ToolCallID: r.ToolCallID,
		})
	}
	return out, nil
}

// runlogUserSeq 返回某 run 的 user 行在 runlog 的 seq（笔记的 source_seq 来源，M1）。
// AcquireRun 保证每 run 恰一条 user 行；查不到时落 0（违例态，但不必在这条读路径上拦截——
// 它只喂笔记行的溯源列，不参与任何判定）。
func (s *Store) runlogUserSeq(ctx context.Context, sessionID SessionID, runID RunID) (int64, error) {
	var seq int64
	err := s.db.WithContext(ctx).Model(&RunlogEntry{}).
		Where("session_id = ? AND run_id = ? AND role = 'user'", sessionID, runID).
		Select("COALESCE(MAX(seq), 0)").Scan(&seq).Error
	return seq, err
}

// ReadLog 返回 session 中 seq > from 的定稿记录（升序，最多 w 条）与 nextOffset（空批 = from）。
// exists=false = session 未登记，log 端点据此回 404。W 截断规则与 frames 共用同一条
// （planning §0 第 4 条），这里实现为 `WHERE seq > from ORDER BY seq LIMIT w`；w<=0 直接空批——
// 契约里没有「一次拿全部」。seq 即 runlogOffset（列形态），读侧只做字段注入。
func (s *Store) ReadLog(ctx context.Context, sessionID SessionID, from, w int) (rows []Row, nextOffset int, exists bool, err error) {
	rows = []Row{}
	nextOffset = from
	exists, err = s.Exists(ctx, sessionID)
	if err != nil || !exists {
		return rows, nextOffset, exists, err
	}
	if w <= 0 {
		return rows, nextOffset, true, nil
	}
	var entries []RunlogEntry
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND seq > ?", sessionID, from).
		Order("seq").Limit(w).Find(&entries).Error; err != nil {
		return nil, 0, false, err
	}
	for _, e := range entries {
		row := Row{
			RunlogOffset: int(e.Seq),
			RunID:        e.RunID,
			Role:         e.Role,
			Agent:        e.Agent,
			Model:        e.Model,
			MessageID:    e.MessageID,
			Text:         e.Text,
			Payload:      e.Payload,
		}
		// assistant 行 payload = usage JSON（doc/27 §4b）：解出注入 usage 字段、原文不透出；
		// 无 payload（user 行 / 账单缺失）不写该字段。tool 扩展行 payload 照旧原样透出。
		if e.Role == "assistant" && e.Payload != "" {
			var bill UsageBill
			if err := json.Unmarshal([]byte(e.Payload), &bill); err == nil {
				row.Usage = &bill
				row.Payload = ""
			}
		}
		rows = append(rows, row)
		nextOffset = int(e.Seq)
	}
	return rows, nextOffset, true, nil
}
