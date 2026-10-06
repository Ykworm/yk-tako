// Package runengine 实现 v6 运行引擎的 API 面（doc/19-v6）。
// 本文件是内存假存储：进程内 map，接口形状由 server.go 的 handler 需要反推。
// A4 已转正（redisstore.go 的 RedisStore），server 只接 RedisStore——本文件当前无调用方，
// 留作 Redis 版的语义对照：六张 map 与 redisstore.go 的六类键一一对应。
package runengine

import "sync"

// SessionID / RunID 是实体标识符的命名类型：传反 = 编译错误（新 A8）。
// 命名 string 类型的 JSON 编解码与 string 逐字节相同，线上契约不受影响。
type SessionID string
type RunID string

// Row 是 runlog 里的一条定稿记录（doc/19 §3）。
// RunlogOffset 不落存储、读时按 index+1 注入（omitempty 保证存储形态不含它）。
type Row struct {
	RunlogOffset int    `json:"runlogOffset,omitempty"`
	RunID        RunID  `json:"runId"`
	Role         string `json:"role"`
	MessageID    string `json:"messageId,omitempty"`
	Text         string `json:"text"`
}

// Frame 是 runlive 里的一帧（doc/19 §2）。EventOffset 同理，读时注入。
type Frame struct {
	EventOffset int    `json:"eventOffset,omitempty"`
	Kind        string `json:"kind"`
	MessageID   string `json:"messageId,omitempty"`
	Text        string `json:"text,omitempty"`
	Status      string `json:"status,omitempty"`
}

// ModelMessage 是 model_history 里的一条模型侧消息（doc/19 §7 真源划分：
// runlog 记「产出了什么」，model_history 记「模型现在该看到什么」）。
// 内存版只存 role + text；doc 里「每 run 一行：闸 + 状态 + 模型侧消息」的形态随 A4/A7 存储落地再定。
type ModelMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// MemStore 内存假存储。所有方法持有同一把锁——单进程内等价于「一笔事务同生同灭」。
// runlive 永不过期（没有 TTL）；gone 的过期路径在 RedisStore 那边由 runliveTTL 承担。
type MemStore struct {
	mu           sync.Mutex
	sessions     map[SessionID]bool           // 登记：存在即已登记
	live         map[SessionID]RunID          // 闸：sessionID → runID
	runlog       map[SessionID][]Row          // sessionID → 定稿记录序列
	runlive      map[RunID][]Frame            // runID → 生成中帧序列
	draft        map[RunID]string             // runID → assistant 全文累加（run 结束时进 runlog 即删）
	modelHistory map[SessionID][]ModelMessage // sessionID → 模型侧消息序列（worker 组装输入的唯一来源，新 A6 引入）
}

func NewMemStore() *MemStore {
	return &MemStore{
		sessions:     make(map[SessionID]bool),
		live:         make(map[SessionID]RunID),
		runlog:       make(map[SessionID][]Row),
		runlive:      make(map[RunID][]Frame),
		draft:        make(map[RunID]string),
		modelHistory: make(map[SessionID][]ModelMessage),
	}
}

// Exists 报告 session 是否已登记（带 sessionId 投递时的准入检查）。
func (m *MemStore) Exists(sessionID SessionID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

// AcquireRun 抢闸并落 POST 事务（doc/19 §1）：登记（若未登记）+ 闸 + runlog user 记录 + runlive start 帧。
// false = 会话忙：调用方回 409，且本次调用什么都没有写。
func (m *MemStore) AcquireRun(sessionID SessionID, runID RunID, user Row) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.live[sessionID]; busy {
		return false
	}
	m.sessions[sessionID] = true
	m.live[sessionID] = runID
	m.runlog[sessionID] = append(m.runlog[sessionID], user)
	m.runlive[runID] = []Frame{{Kind: "start", MessageID: user.MessageID}}
	m.draft[runID] = ""
	m.modelHistory[sessionID] = append(m.modelHistory[sessionID], ModelMessage{Role: "user", Text: user.Text})
	return true
}

// ModelMessages 返回 session 的模型侧消息副本（worker 组装本次生成的输入）。
// user 消息在 AcquireRun 时已进入；assistant 消息在 EndRun(complete) 时进入——error 结局只留 user 那一半。
func (m *MemStore) ModelMessages(sessionID SessionID) []ModelMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.modelHistory[sessionID]
	out := make([]ModelMessage, len(stored))
	copy(out, stored)
	return out
}

// SessionStatus 返回状态查询（doc/19 §4）三要素：活 run id（""= 闲着）、runlog 高水位、是否已登记。
func (m *MemStore) SessionStatus(sessionID SessionID) (liveRunID RunID, runlogOffset int, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sessions[sessionID] {
		return "", 0, false
	}
	return m.live[sessionID], len(m.runlog[sessionID]), true
}

// ReadFrames 返回 run 中 offset > from 的帧（升序，最多 w 条）与 nextOffset（空批 = from）。
// exists=false 表示 runlive 键不存在——frames 端点据此回 gone 而非 404（doc/19 §0 URL 约定）。
func (m *MemStore) ReadFrames(runID RunID, from, w int) (frames []Frame, nextOffset int, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.runlive[runID]
	if !ok {
		return []Frame{}, from, false
	}
	frames = make([]Frame, 0, w)
	nextOffset = from
	// index i 的 offset = i+1；offset > from 即 i >= from。
	for i := from; i < len(stored) && len(frames) < w; i++ {
		f := stored[i]
		f.EventOffset = i + 1
		frames = append(frames, f)
		nextOffset = i + 1
	}
	return frames, nextOffset, true
}

// ReadLog 同上，session 级定稿记录。exists=false = session 未登记，log 端点回 404。
func (m *MemStore) ReadLog(sessionID SessionID, from, w int) (rows []Row, nextOffset int, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sessions[sessionID] {
		return []Row{}, from, false
	}
	stored := m.runlog[sessionID]
	rows = make([]Row, 0, w)
	nextOffset = from
	for i := from; i < len(stored) && len(rows) < w; i++ {
		r := stored[i]
		r.RunlogOffset = i + 1
		rows = append(rows, r)
		nextOffset = i + 1
	}
	return rows, nextOffset, true
}

// AppendDelta 追加一帧 delta 并累加 draft 全文（doc/19 §7 worker 行为）。
// ok=false = run 不存在或已结束（draft 已随 endRun 删除，即「liveness 由 draft 承载」）。
func (m *MemStore) AppendDelta(runID RunID, text string) (eventOffset int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, live := m.draft[runID]; !live {
		return 0, false
	}
	m.runlive[runID] = append(m.runlive[runID], Frame{Kind: "delta", Text: text})
	m.draft[runID] += text
	return len(m.runlive[runID]), true
}

// Reset 全局归零（dev-only 端点 /api/dev/reset 的底座，todos 新 A5）：清空全部六张 map，返回清掉的条目数。
// 闸（live）、帧/日志/草稿与模型侧消息在同一把锁内一起消失——「reset = 完整一致的新世界，无残留」。
// Redis 版（SCAN MATCH tk:* + UNLINK）随真正的 A4 存储落地一并替换本实现（planning §4 转正表）。
func (m *MemStore) Reset() (deleted int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted = len(m.sessions) + len(m.live) + len(m.runlog) + len(m.runlive) + len(m.draft) + len(m.modelHistory)
	m.sessions = make(map[SessionID]bool)
	m.live = make(map[SessionID]RunID)
	m.runlog = make(map[SessionID][]Row)
	m.runlive = make(map[RunID][]Frame)
	m.draft = make(map[RunID]string)
	m.modelHistory = make(map[SessionID][]ModelMessage)
	return deleted
}

// EndRun 是 run 侧动作的唯一入口（doc/19 概念表「run 结束」；planning §5 第 6 条）。
// 无条件：压 terminal（带终态 status）+ 放闸 + 删 draft；有条件：仅 status=="complete" 把 draft 全文
// RPUSH 进 runlog（定稿）+ 进 model_history（assistant 入模型视野）。内存版一把锁包住 = 一笔原子写；
// ok=false = session 不拥有该 run（reset 已过 / 闸已放），调用方据此知道本次收尾是空转。
func (m *MemStore) EndRun(sessionID SessionID, runID RunID, status string) (eventOffset, runlogOffset int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live[sessionID] != runID {
		return 0, 0, false
	}
	m.runlive[runID] = append(m.runlive[runID], Frame{Kind: "terminal", Status: status})
	if status == "complete" {
		m.runlog[sessionID] = append(m.runlog[sessionID], Row{RunID: runID, Role: "assistant", Text: m.draft[runID]})
		m.modelHistory[sessionID] = append(m.modelHistory[sessionID], ModelMessage{Role: "assistant", Text: m.draft[runID]})
	}
	delete(m.live, sessionID)
	delete(m.draft, runID)
	return len(m.runlive[runID]), len(m.runlog[sessionID]), true
}
