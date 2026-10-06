// 本文件是 v6 存储层的唯一形态：Redis（doc/19 §7「六类结构」；A4 转正后 server/worker 只接它）。
//
// 三条实现约定（tmp/19-v6/planning.md §3）：
//   - 帧/行的 JSON 里**不存** offset，读时按 LIST index 注入（offset = index+1）——
//     写侧不需要计数器，多副本并发追加也不会错号；
//   - 累加器 `tk:draft` 放 Redis 不放进程（多副本下进程内累加会丢字）；
//   - 读取一律 `LRANGE key N N+W-1`，两个拉取端点同一条规则。
//
// 跨键写一律走 Lua（`AcquireRun` / `AppendDelta` / `EndRun`）：doc/19 §7 要求 run 结束与定稿
// 「同一笔原子写」，否则客户端能观察到「闸已放、历史还没那行」的窗口。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix 是 tako 的键空间前缀（planning §5 第 1 条）。
// reset 用 `SCAN MATCH tk:*`，对未来新增的任何键天然免疫；也是误伤防线——
// 本机 6379 上跑着别的项目的活 Redis，前缀保证连错实例时 reset 只碰自己的键。
const keyPrefix = "tk:"

func keySession(sessionID SessionID) string      { return keyPrefix + "session:" + string(sessionID) }
func keyLive(sessionID SessionID) string         { return keyPrefix + "live:" + string(sessionID) }
func keyRunlog(sessionID SessionID) string       { return keyPrefix + "runlog:" + string(sessionID) }
func keyRunlive(runID RunID) string              { return keyPrefix + "runlive:" + string(runID) }
func keyDraft(runID RunID) string                { return keyPrefix + "draft:" + string(runID) }
func keyModelHistory(sessionID SessionID) string { return keyPrefix + "mh:" + string(sessionID) }

// acquireScript 是 POST 事务（doc/19 §1）：`SET NX PX` 抢到**带租约的**闸才写其余五键，抢不到**一个键都不写**。
// 闸带短租约（A7）：进程死亡后没人续约，闸到点自然蒸发、会话自动解锁——不再是「SET NX 无 TTL、持有者死了永久锁死」。
// draft/runlive 出生即带同一租约（A7-2 出生窗口）：抢闸与续约之间进程死亡，三键也到点同灭——
// 不留「闸自放了、draft/runlive 永存」的泄漏（修前它们要等首次续约才有 TTL，裸建期间死掉即成垃圾键）。
// 生成期间由 worker watchdog 定期续约（renewScript），长回复不被误放。
// KEYS: live session runlog runlive draft mh；ARGV: runID userRowJSON startFrameJSON userModelMessageJSON leaseMillis。
var acquireScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[5]) == false then
  return 0
end
redis.call('SET', KEYS[2], '1')
redis.call('RPUSH', KEYS[3], ARGV[2])
redis.call('RPUSH', KEYS[4], ARGV[3])
redis.call('PEXPIRE', KEYS[4], ARGV[5])
redis.call('SET', KEYS[5], '')
redis.call('PEXPIRE', KEYS[5], ARGV[5])
redis.call('RPUSH', KEYS[6], ARGV[4])
return 1
`)

// renewScript 是 watchdog 续约（A7 闸租约化）：**CAS 防误续**——闸仍等于本 run 才动手，否则 no-op（返 0），
// 绝不给已易主 / 已放的闸续命（防网络分区复活的老 worker 把新 run 的租约续上）。
// 三键同刷 lease：live + draft + runlive **同生同灭**——进程死了没人续约，三键一起 TTL 到期消失，
// 客户端拉到 frames `gone` + `liveRunId:""`，靠 B1 的冗余收口信号自愈，无「闸没了帧还在」的半死残留。
// KEYS: live draft runlive；ARGV: runID leaseMillis。
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('PEXPIRE', KEYS[2], ARGV[2])
redis.call('PEXPIRE', KEYS[3], ARGV[2])
return 1
`)

// appendDeltaScript：liveness 由 `tk:draft` 承载（run 结束时它被删），不在则返 0。
// KEYS: draft runlive；ARGV: deltaFrameJSON text。
var appendDeltaScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
redis.call('RPUSH', KEYS[2], ARGV[1])
redis.call('APPEND', KEYS[1], ARGV[2])
return redis.call('LLEN', KEYS[2])
`)

// endRunScript 是「run 结束 + 定稿」的同一笔原子写（doc/19 §7 生命周期、planning §5 第 6 条）。
// 守卫：闸必须仍等于本 run（复位/闸已放之后调用 = 空转，返 {0,0,0}）——双写的危险从这一侧进来，
// 不挡的话 runlog 会出现两行 assistant。
// 无条件：压 terminal + `EXPIRE` + 放闸 + 删 draft；有条件：仅 status=='complete' 定稿
// （draft 全文 RPUSH 进 runlog + 进 model_history）。
// assistant 那两条记录的 JSON 在 Lua 内拼：draft 全文只有 Redis 知道，而**字段序必须与 Go 侧
// 结构体一致**（读出来的字节是契约），所以用字面拼接 + `cjson.encode` 只负责转义字符串值，
// 不把整个对象交给 cjson（它的键序不保证）。
// KEYS: live runlive draft runlog mh；ARGV: runID terminalFrameJSON ttlSeconds status。
var endRunScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return {0, 0, 0}
end
redis.call('RPUSH', KEYS[2], ARGV[2])
redis.call('EXPIRE', KEYS[2], ARGV[3])
local eventOffset = redis.call('LLEN', KEYS[2])
if ARGV[4] == 'complete' then
  local text = redis.call('GET', KEYS[3])
  if text == false then text = '' end
  redis.call('RPUSH', KEYS[4], '{"runId":' .. cjson.encode(ARGV[1]) .. ',"role":"assistant","text":' .. cjson.encode(text) .. '}')
  redis.call('RPUSH', KEYS[5], '{"role":"assistant","text":' .. cjson.encode(text) .. '}')
end
redis.call('DEL', KEYS[1])
redis.call('DEL', KEYS[3])
return {1, eventOffset, redis.call('LLEN', KEYS[4])}
`)

// RedisStore 是四个 API 的存储后端。
//   - ttl：runlive 在 run **正常结束**后的回收期限（doc/19 §7「设 TTL」，dev 取 300s）——只是回收参数，
//     不是正确性参数：`done` 与 `gone` 收敛到同一条收口路径。
//   - leaseTTL：闸（`tk:live`）与 draft/runlive 在**生成期间**的租约期限（A7，dev 取 30s）。抢闸时 `PX` 设上，
//     watchdog 每 leaseRenew 续回；进程死亡后没人续约，三键到点同生同灭。同为回收参数：误放概率随 leaseTTL
//     放宽指数下降（30s/5s = 连续 6 次续约全失败才误放）。
type RedisStore struct {
	rdb      *redis.Client
	ttl      time.Duration
	leaseTTL time.Duration
}

func NewRedisStore(addr string, ttl, leaseTTL time.Duration) *RedisStore {
	return &RedisStore{rdb: redis.NewClient(&redis.Options{Addr: addr}), ttl: ttl, leaseTTL: leaseTTL}
}

// Ping 是启动期探针（main 调用）：Redis 不可达就不起进程，
// 与 config.MustOpen 同一处置——不留半个只能 409 的引擎。
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// Exists 报告 session 是否已登记（带 sessionId 投递时的准入检查）。
func (s *RedisStore) Exists(ctx context.Context, sessionID SessionID) (bool, error) {
	n, err := s.rdb.Exists(ctx, keySession(sessionID)).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// AcquireRun 抢闸并落 POST 事务：登记 + 闸 + runlog user 记录 + runlive start 帧 + 空 draft + model_history user 半。
// false = 会话忙（调用方回 409），本次调用什么都没写。
func (s *RedisStore) AcquireRun(ctx context.Context, sessionID SessionID, runID RunID, user Row) (bool, error) {
	userJSON, err := json.Marshal(user)
	if err != nil {
		return false, err
	}
	startJSON, err := json.Marshal(Frame{Kind: "start", MessageID: user.MessageID})
	if err != nil {
		return false, err
	}
	modelJSON, err := json.Marshal(ModelMessage{Role: "user", Text: user.Text})
	if err != nil {
		return false, err
	}
	n, err := acquireScript.Run(ctx, s.rdb, []string{
		keyLive(sessionID), keySession(sessionID), keyRunlog(sessionID),
		keyRunlive(runID), keyDraft(runID), keyModelHistory(sessionID),
	}, string(runID), userJSON, startJSON, modelJSON, int64(s.leaseTTL/time.Millisecond)).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RenewLease 由 worker watchdog 在生成期间定期调用（A7 闸租约化）：CAS 通过（闸仍属本 run）才把
// live/draft/runlive 三键的 TTL 同刷回 leaseTTL。ok=false = 闸已不属于本 run（已放 / 已易主 / 已到期），
// 续约 no-op——调用方据此判定本 run 已被判死。
func (s *RedisStore) RenewLease(ctx context.Context, sessionID SessionID, runID RunID) (bool, error) {
	n, err := renewScript.Run(ctx, s.rdb, []string{
		keyLive(sessionID), keyDraft(runID), keyRunlive(runID),
	}, string(runID), int64(s.leaseTTL/time.Millisecond)).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ModelMessages 返回 session 的模型侧消息全序（worker 组装本次生成的输入）。
// user 半在 AcquireRun 入、assistant 半在 EndRun(complete) 入——error 结局只留 user 那一半。
func (s *RedisStore) ModelMessages(ctx context.Context, sessionID SessionID) ([]ModelMessage, error) {
	raw, err := s.rdb.LRange(ctx, keyModelHistory(sessionID), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ModelMessage, 0, len(raw))
	for _, item := range raw {
		var m ModelMessage
		if err := json.Unmarshal([]byte(item), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// SessionStatus 返回状态查询三要素（doc/19 §4）：活 run id（""= 闲着）、runlog 高水位、是否已登记。
// 三条命令走 TxPipeline（MULTI/EXEC）取一致快照，避免「读到活 run 却读到旧高水位」。
func (s *RedisStore) SessionStatus(ctx context.Context, sessionID SessionID) (liveRunID RunID, runlogOffset int, exists bool, err error) {
	pipe := s.rdb.TxPipeline()
	existsCmd := pipe.Exists(ctx, keySession(sessionID))
	liveCmd := pipe.Get(ctx, keyLive(sessionID))
	lenCmd := pipe.LLen(ctx, keyRunlog(sessionID))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return "", 0, false, err
	}
	if existsCmd.Val() == 0 {
		return "", 0, false, nil
	}
	live, err := liveCmd.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return "", 0, false, err
	}
	return RunID(live), int(lenCmd.Val()), true, nil
}

// ReadFrames 返回 run 中 offset > from 的帧（升序，最多 w 条）与 nextOffset（空批 = from）。
// exists=false 表示 runlive 键不存在——frames 端点据此回 gone 而非 404（doc/19 §0 URL 约定：
// 「从未存在」与「TTL 到期删除」故意不区分）。
func (s *RedisStore) ReadFrames(ctx context.Context, runID RunID, from, w int) (frames []Frame, nextOffset int, exists bool, err error) {
	raw, exists, err := s.readBatch(ctx, keyRunlive(runID), "", from, w)
	frames = []Frame{}
	nextOffset = from
	if err != nil || !exists {
		return frames, nextOffset, exists, err
	}
	for i, item := range raw {
		var f Frame
		if err := json.Unmarshal([]byte(item), &f); err != nil {
			return nil, 0, false, err
		}
		f.EventOffset = from + i + 1
		frames = append(frames, f)
		nextOffset = from + i + 1
	}
	return frames, nextOffset, true, nil
}

// ReadLog 同上，session 级定稿记录。exists=false = session 未登记，log 端点回 404。
func (s *RedisStore) ReadLog(ctx context.Context, sessionID SessionID, from, w int) (rows []Row, nextOffset int, exists bool, err error) {
	raw, exists, err := s.readBatch(ctx, keyRunlog(sessionID), keySession(sessionID), from, w)
	rows = []Row{}
	nextOffset = from
	if err != nil || !exists {
		return rows, nextOffset, exists, err
	}
	for i, item := range raw {
		var r Row
		if err := json.Unmarshal([]byte(item), &r); err != nil {
			return nil, 0, false, err
		}
		r.RunlogOffset = from + i + 1
		rows = append(rows, r)
		nextOffset = from + i + 1
	}
	return rows, nextOffset, true, nil
}

// readBatch 是两个拉取端点共用的那一条规则：`LRANGE key from from+w-1`。
// gateKey 非空时先查它（log 的准入 = session 登记；frames 的准入 = runlive 自己）。
// 同一 TxPipeline 内取快照：键在读与判存在之间过期会让 status 撒谎（gone 报成 generating）。
// w<=0 直接空批——`LRANGE 0 -1` 会返全表，而契约里没有「一次拿全部」。
func (s *RedisStore) readBatch(ctx context.Context, listKey, gateKey string, from, w int) (raw []string, exists bool, err error) {
	if w <= 0 {
		if gateKey == "" {
			gateKey = listKey
		}
		n, err := s.rdb.Exists(ctx, gateKey).Result()
		if err != nil {
			return nil, false, err
		}
		return nil, n == 1, nil
	}
	pipe := s.rdb.TxPipeline()
	gate := gateKey
	if gate == "" {
		gate = listKey
	}
	existsCmd := pipe.Exists(ctx, gate)
	rangeCmd := pipe.LRange(ctx, listKey, int64(from), int64(from+w-1))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, false, err
	}
	return rangeCmd.Val(), existsCmd.Val() == 1, nil
}

// AppendDelta 追加一帧 delta 并累加 draft 全文（doc/19 §7 worker 行为）。
// ok=false = run 不存在或已结束（draft 已随 run 结束删除，即「liveness 由 draft 承载」）。
func (s *RedisStore) AppendDelta(ctx context.Context, runID RunID, text string) (eventOffset int, ok bool, err error) {
	frameJSON, err := json.Marshal(Frame{Kind: "delta", Text: text})
	if err != nil {
		return 0, false, err
	}
	n, err := appendDeltaScript.Run(ctx, s.rdb, []string{keyDraft(runID), keyRunlive(runID)}, frameJSON, text).Int64()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	return int(n), true, nil
}

// EndRun 是 run 侧动作的唯一入口（doc/19 概念表「run 结束」；planning §5 第 6 条）。
// ok=false = session 不拥有该 run（reset 已过 / 闸已放），本次收尾空转。
func (s *RedisStore) EndRun(ctx context.Context, sessionID SessionID, runID RunID, status string) (eventOffset, runlogOffset int, ok bool, err error) {
	terminalJSON, err := json.Marshal(Frame{Kind: "terminal", Status: status})
	if err != nil {
		return 0, 0, false, err
	}
	res, err := endRunScript.Run(ctx, s.rdb, []string{
		keyLive(sessionID), keyRunlive(runID), keyDraft(runID), keyRunlog(sessionID), keyModelHistory(sessionID),
	}, string(runID), terminalJSON, int64(s.ttl/time.Second), status).Slice()
	if err != nil {
		return 0, 0, false, err
	}
	if len(res) != 3 {
		return 0, 0, false, errors.New("endRun: unexpected script reply")
	}
	acquired, _ := res[0].(int64)
	if acquired == 0 {
		return 0, 0, false, nil
	}
	event, _ := res[1].(int64)
	logged, _ := res[2].(int64)
	return int(event), int(logged), true, nil
}

// Reset 全局归零（dev-only 端点 /api/dev/reset 的底座）：`SCAN MATCH tk:*` 游标循环 + `UNLINK`，返回删除**键数**。
// 用前缀 SCAN 而不是逐个键名硬编码：以后新增任何 `tk:` 键，reset 不用改（通用规则而非特例）。
// 闸与帧/日志/草稿/模型侧消息一起消失——「reset = 完整一致的新世界，空重启与 reset 等价」。
func (s *RedisStore) Reset(ctx context.Context) (deleted int, err error) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, keyPrefix+"*", 500).Result()
		if err != nil {
			return deleted, err
		}
		if len(keys) > 0 {
			n, err := s.rdb.Unlink(ctx, keys...).Result()
			if err != nil {
				return deleted, err
			}
			deleted += int(n)
		}
		if next == 0 {
			return deleted, nil
		}
		cursor = next
	}
}
