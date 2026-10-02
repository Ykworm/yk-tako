package runengine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store 承载 doc/19 §7 的五类键。游标不写在值里：帧/记录的 offset 一律
// 读时由 LIST index+1 注入，写侧无计数器，多副本追加不会错号。
type Store struct {
	rdb     *redis.Client
	liveTTL time.Duration
}

func NewStore(rdb *redis.Client, liveTTL time.Duration) *Store {
	return &Store{rdb: rdb, liveTTL: liveTTL}
}

func kSession(id string) string { return "tk:session:" + id }
func kLive(session string) string { return "tk:live:" + session }
func kRunlog(session string) string { return "tk:runlog:" + session }
func kRunlive(run string) string  { return "tk:runlive:" + run }
func kDraft(run string) string    { return "tk:draft:" + run }

type Frame struct {
	EventOffset int    `json:"eventOffset"`
	Kind        string `json:"kind"`
	MessageID   string `json:"messageId,omitempty"`
	Text        string `json:"text,omitempty"`
	Status      string `json:"status,omitempty"`
}

type Row struct {
	RunlogOffset int    `json:"runlogOffset"`
	Run          string `json:"run"`
	Role         string `json:"role"`
	MessageID    string `json:"messageId,omitempty"`
	Text         string `json:"text"`
}

// 落盘形状 = 对外形状去掉 offset 字段。
type storedFrame struct {
	Kind      string `json:"kind"`
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text,omitempty"`
	Status    string `json:"status,omitempty"`
}

type storedRow struct {
	Run       string `json:"run"`
	Role      string `json:"role"`
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text"`
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// escapeText 返回 text 的 JSON 字符串体内编码（去掉外层引号）。
// JSON 转义按字符逐一定义、与上下文无关，所以「逐片段转义后 APPEND」
// 拼出来的就是「拼接全文的转义」，Finalize 的 Lua 可直接嵌进 assistant 行。
func escapeText(s string) string {
	q := mustJSON(s) // JSON 字符串本身
	return q[1 : len(q)-1]
}

func (s *Store) RegisterSession(ctx context.Context, id string) error {
	return s.rdb.Set(ctx, kSession(id), "1", 0).Err()
}

func (s *Store) SessionExists(ctx context.Context, id string) (bool, error) {
	n, err := s.rdb.Exists(ctx, kSession(id)).Result()
	return n == 1, err
}

// AcquireRun = 闸 + 同笔两写（runlog user 行、runlive start 帧）。
// 抢不到闸返回 false 且不写任何键（doc/19 §1「整笔回滚 = 409」的存储级形态）。
func (s *Store) AcquireRun(ctx context.Context, session, run, messageID, userText string) (bool, error) {
	userRow := mustJSON(storedRow{Run: run, Role: "user", MessageID: messageID, Text: userText})
	startFrame := mustJSON(storedFrame{Kind: "start", MessageID: messageID})
	script := redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX') then
  redis.call('RPUSH', KEYS[2], ARGV[2])
  redis.call('RPUSH', KEYS[3], ARGV[3])
  return 1
end
return 0
`)
	n, err := script.Run(ctx, s.rdb, []string{kLive(session), kRunlog(session), kRunlive(run)},
		run, userRow, startFrame).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) AppendDelta(ctx context.Context, run, text string) error {
	pipe := s.rdb.TxPipeline()
	pipe.Append(ctx, kDraft(run), escapeText(text))
	pipe.RPush(ctx, kRunlive(run), mustJSON(storedFrame{Kind: "delta", Text: text}))
	_, err := pipe.Exec(ctx)
	return err
}

// Finalize = 一笔 Lua 里做完五件事：压 terminal、设 TTL、读 draft 拼 assistant 行进 runlog、
// 释放闸、删 draft。只设 TTL 不删 runlive——键被 TTL 收走之后才叫 gone。
func (s *Store) Finalize(ctx context.Context, session, run string) error {
	terminal := mustJSON(storedFrame{Kind: "terminal", Status: "complete"})
	ttlSec := int64(s.liveTTL / time.Second)
	script := redis.NewScript(`
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
local draft = redis.call('GET', KEYS[4]) or ''
redis.call('RPUSH', KEYS[2], '{"run":"'..ARGV[3]..'","role":"assistant","text":"'..draft..'"}')
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[4])
return 1
`)
	return script.Run(ctx, s.rdb,
		[]string{kRunlive(run), kRunlog(session), kLive(session), kDraft(run)},
		terminal, ttlSec, run).Err()
}

// ReadLive 按单批上限 w 拉帧（LRANGE from..from+w-1），并判定三态：
// 键已不在 → gone；末帧是 terminal → done；否则 generating。
func (s *Store) ReadLive(ctx context.Context, run string, from, w int) ([]Frame, int, string, error) {
	lastRaw, err := s.rdb.LIndex(ctx, kRunlive(run), -1).Result()
	if err == redis.Nil {
		return nil, from, "gone", nil
	}
	if err != nil {
		return nil, 0, "", err
	}
	var last storedFrame
	if err := json.Unmarshal([]byte(lastRaw), &last); err != nil {
		return nil, 0, "", err
	}
	status := "generating"
	if last.Kind == "terminal" {
		status = "done"
	}
	raws, err := s.rdb.LRange(ctx, kRunlive(run), int64(from), int64(from+w-1)).Result()
	if err != nil {
		return nil, 0, "", err
	}
	frames := make([]Frame, 0, len(raws))
	for i, raw := range raws {
		var f storedFrame
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return nil, 0, "", err
		}
		frames = append(frames, Frame{EventOffset: from + i + 1, Kind: f.Kind, MessageID: f.MessageID, Text: f.Text, Status: f.Status})
	}
	return frames, from + len(frames), status, nil
}

// ReadLog 按单批上限 w 拉定稿记录；空批 next = from（doc/19 §3，客户端可直接续拉）。
func (s *Store) ReadLog(ctx context.Context, session string, from, w int) ([]Row, int, error) {
	raws, err := s.rdb.LRange(ctx, kRunlog(session), int64(from), int64(from+w-1)).Result()
	if err != nil {
		return nil, 0, err
	}
	rows := make([]Row, 0, len(raws))
	for i, raw := range raws {
		var r storedRow
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, 0, err
		}
		rows = append(rows, Row{RunlogOffset: from + i + 1, Run: r.Run, Role: r.Role, MessageID: r.MessageID, Text: r.Text})
	}
	return rows, from + len(rows), nil
}

// RunlogHighWater = GET /api/session 的 runlogOffset（LLEN）。
func (s *Store) RunlogHighWater(ctx context.Context, session string) (int, error) {
	n, err := s.rdb.LLen(ctx, kRunlog(session)).Result()
	return int(n), err
}

// LiveRun = 闸当前持有者，即 liveRunId；没人持有返回 ""。
func (s *Store) LiveRun(ctx context.Context, session string) (string, error) {
	v, err := s.rdb.Get(ctx, kLive(session)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}
