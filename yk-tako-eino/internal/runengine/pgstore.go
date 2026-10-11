// 本文件是存储层的 PG 半：四张表的 GORM model、建表（AutoMigrate），以及纯 PG 的读方法。
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
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// Session = `sessions` 表（19v6-03）：会话登记，存在即已登记（懒建于 POST 事务内）。永久状态。
type Session struct {
	ID        SessionID `gorm:"primaryKey"`
	Title     string    `gorm:"not null"` // 展示字段（会话列表用），非契约；初值 = 首条用户消息截断
	CreatedAt time.Time `gorm:"not null"`
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
	CreatedAt  time.Time  `gorm:"not null"`
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

// autoMigrate 从四个 struct 建表（dev 迁移方式，待定项①已裁：AutoMigrate 够用，无迁移文件；
// 上生产换不换显式迁移归部署面）。两个部分索引（WHERE 条件）优先走 GORM index tag；
// 若某版 GORM 表达不全，在这里补 CREATE ... IF NOT EXISTS 兜底——仍是代码内，不引迁移文件。
func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&Session{}, &Run{}, &RunModelMessage{}, &RunlogEntry{})
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

// SessionStatus 返回状态查询三要素（19v6-01 §4）：活 run id（"" = 闲着）、runlog 高水位、是否已登记。
// 单条 SQL 子查询取一致快照（原 MULTI 的等价物，预登记裁定）。liveRunId 读侧即回收：
// lease 已过的死行当场不再算活，不等清扫（清扫只为放行新 run，planning §8.3）。
func (s *Store) SessionStatus(ctx context.Context, sessionID SessionID) (liveRunID RunID, runlogOffset int, exists bool, err error) {
	var live string
	var hw int64
	err = s.db.WithContext(ctx).Raw(`
SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ?),
       COALESCE((SELECT id FROM runs WHERE session_id = ? AND status = 'running' AND lease_until > now() LIMIT 1), ''),
       COALESCE((SELECT MAX(seq) FROM runlog_entries WHERE session_id = ?), 0)`,
		sessionID, sessionID, sessionID).Row().Scan(&exists, &live, &hw)
	if err != nil {
		return "", 0, false, err
	}
	return RunID(live), int(hw), exists, nil
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
		rows = append(rows, Row{
			RunlogOffset: int(e.Seq),
			RunID:        e.RunID,
			Role:         e.Role,
			Agent:        e.Agent,
			Model:        e.Model,
			MessageID:    e.MessageID,
			Text:         e.Text,
			Payload:      e.Payload,
		})
		nextOffset = int(e.Seq)
	}
	return rows, nextOffset, true, nil
}
