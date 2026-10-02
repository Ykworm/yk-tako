// 本文件：REPL 的会话内存。REPL 不打开 Web 的事件库，检索只留在这一进程里。
package compact

import (
	"fmt"
	"io"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// ReplMemory 实现 Memory，并多三个 REPL 才用的方法：BeginTurn、FinishTurn、AbortTurn。
// 一轮成功结束后，下一轮要读的消息只从 FinishTurn 的结果来，调用方不要再自己追加。
type ReplMemory struct {
	mu     sync.Mutex
	turns  []replTurn
	live   *replLive
	snap   []replTurn
	staged bool
	rows   []ragRow
	out    io.Writer
	seq    int
}

// replTurn 是 REPL 里一轮已经结束的对话。
type replTurn struct {
	id   string
	msgs []*schema.Message
}

// replLive 是 REPL 里正在进行的这一轮。
type replLive struct {
	id       string
	user     string
	combined string
}

// ragRow 是 REPL 进程里的一条旧原文。
type ragRow struct {
	runID string
	body  string
}

// 编译期校验 ReplMemory 实现了 Memory。
var _ Memory = (*ReplMemory)(nil)

// NewReplMemory 建一份 REPL 内存。out 用来打印「正在压缩」；为空则不打印。
func NewReplMemory(out io.Writer) *ReplMemory {
	return &ReplMemory{out: out}
}

// BeginTurn 开始一轮。id 形如 repl-1，只在这一进程里用，不写 SQL。
func (m *ReplMemory) BeginTurn(user string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.live = &replLive{id: fmt.Sprintf("repl-%d", m.seq), user: user}
	m.staged = false
	m.snap = nil
}

// Prompt 拼出这一轮送给模型的消息：已经结束的轮，加上这一句用户原话。
func (m *ReplMemory) Prompt(user string) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*schema.Message
	for _, turn := range m.turns {
		out = append(out, turn.msgs...)
	}
	out = append(out, schema.UserMessage(user))
	return out
}

// FinishTurn 把这一轮收进内存，并返回下一轮应使用的完整消息。
// 压缩过时，用户消息用接过摘要的那句，已结束轮只留 Stage 留下的那些。
func (m *ReplMemory) FinishTurn(produced []*schema.Message) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live != nil {
		text := m.live.user
		if m.live.combined != "" {
			text = m.live.combined
		}
		user := schema.UserMessage(text)
		copied := CloneMessages(produced)
		msgs := append([]*schema.Message{user}, copied...)
		m.turns = append(m.turns, replTurn{id: m.live.id, msgs: msgs})
	}
	m.live = nil
	m.staged = false
	m.snap = nil
	return m.flatLocked()
}

// AbortTurn 丢掉这一轮。如果已经 Stage 过，已结束轮退回快照。
func (m *ReplMemory) AbortTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.staged {
		m.turns = m.snap
	}
	m.live = nil
	m.staged = false
	m.snap = nil
}

// SessionID 实现 Memory。REPL 没有事件库里的会话行。
func (m *ReplMemory) SessionID() string { return "repl" }

// OriginalUser 实现 Memory。返回的是 BeginTurn 时的原话。
func (m *ReplMemory) OriginalUser() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live == nil {
		return ""
	}
	return m.live.user
}

// CurrentRunID 实现 Memory。
func (m *ReplMemory) CurrentRunID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live == nil {
		return ""
	}
	return m.live.id
}

// CompletedRunIDs 实现 Memory。顺序与 turns 一致。
func (m *ReplMemory) CompletedRunIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.turns))
	for i, turn := range m.turns {
		out[i] = turn.id
	}
	return out
}

// Stage 实现 Memory。第一次调用留下快照。之后用快照而不是已经缩短的 turns 计算留下哪些轮，
// 这样同一轮里第二次压缩不会把第一次换掉的轮从快照里弄丢。
func (m *ReplMemory) Stage(c Commit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live == nil {
		return fmt.Errorf("compact: 没有正在进行的轮次")
	}
	if !m.staged {
		m.snap = append([]replTurn(nil), m.turns...)
		m.staged = true
	}
	m.turns = keepTurns(m.snap, c.KeptRunIDs)
	m.live.combined = c.CombinedUser
	return nil
}

// Notify 实现 Memory。只打印短句，不打印摘要正文。
func (m *ReplMemory) Notify(text string) {
	m.mu.Lock()
	out := m.out
	m.mu.Unlock()
	if out == nil || text == "" {
		return
	}
	fmt.Fprintln(out, text)
}

// WriteRAG 实现 Memory。同一进程里追加一条原文。
func (m *ReplMemory) WriteRAG(runID, body string) error {
	if runID == "" || body == "" {
		return nil
	}
	m.mu.Lock()
	m.rows = append(m.rows, ragRow{runID: runID, body: body})
	m.mu.Unlock()
	return nil
}

// SearchRAG 实现 Memory。用词重叠在进程内的原文里查。
func (m *ReplMemory) SearchRAG(query string) (string, error) {
	m.mu.Lock()
	bodies := make([]string, len(m.rows))
	for i, row := range m.rows {
		bodies[i] = row.body
	}
	m.mu.Unlock()
	return SearchBodies(query, bodies), nil
}

// flatLocked 把已结束轮展开成消息。调用方须持有 mu。
func (m *ReplMemory) flatLocked() []*schema.Message {
	var out []*schema.Message
	for _, turn := range m.turns {
		out = append(out, turn.msgs...)
	}
	return out
}

// keepTurns 按快照的原顺序留下 id 在集合里的轮。
func keepTurns(turns []replTurn, ids []string) []replTurn {
	keep := map[string]struct{}{}
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	var out []replTurn
	for _, turn := range turns {
		if _, ok := keep[turn.id]; ok {
			out = append(out, turn)
		}
	}
	return out
}
