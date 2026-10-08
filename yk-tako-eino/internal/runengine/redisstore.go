// 本文件是存储层的 Redis 半：D1 落库后只剩 tk:runlive 与 tk:draft 两类键（19v6-03「三搬两留一消失」）。
//
// 热路径（AppendDelta，每 delta 一次）与 ReadFrames 零变化；跨存储的方法（AcquireRun / EndRun /
// RenewLease / Reset）在 store.go，它们经由本文件的 runliveStart / runliveFinish 两个出生/收尾助手
// 触碰 Redis——两处都是单条 Lua：要么整段生效要么整段没写，让「PG 事务持有跨 Redis 写」的
// 失败形状保持「零痕迹」（planning §8.4）。
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

func keyRunlive(runID RunID) string { return keyPrefix + "runlive:" + string(runID) }
func keyDraft(runID RunID) string   { return keyPrefix + "draft:" + string(runID) }

// acquireRedisScript：run 出生（AcquireRun 的 Redis 半）——建 tk:runlive 压 start 帧 + 建空 tk:draft，
// **两键出生即带租约**（A7-2：生产窗口里它们要等首次续约才拿到 TTL，裸建期间死掉即是垃圾帧；
// 现在与 PG 的 lease_until 同笔设立，不存在两步窗口）。单条 Lua = 原子：
// 跨存储事务里 Redis 半要么两键齐立、要么一字未写。
// KEYS: runlive draft；ARGV: startFrameJSON leaseMillis。
var acquireRedisScript = redis.NewScript(`
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], '')
redis.call('PEXPIRE', KEYS[2], ARGV[2])
return 1
`)

// renewRedisScript：把两键 TTL 同刷回 lease（A7 语义原样：draft/runlive 与闸同生同灭）。
// **CAS 不在这一半**——判死守卫是 PG 的 `UPDATE ... WHERE status='running' AND lease_until > now()`
// 的影响行数（§8.3）；PG 说本 run 还活着，才轮到刷这两键。
// KEYS: draft runlive；ARGV: leaseMillis。
var renewRedisScript = redis.NewScript(`
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[1])
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

// endRunRedisScript：run 收尾的 Redis 半（EndRun 的 Redis 半，commit 后调用）——
// 压 terminal + 设 300s TTL + 立即删 draft。**无守卫**：放闸与定稿已由 PG 事务完成（store.go EndRun），
// 闸不在就根本不会调到这里。KEYS: runlive draft；ARGV: terminalFrameJSON ttlSeconds。
var endRunRedisScript = redis.NewScript(`
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
redis.call('DEL', KEYS[2])
return redis.call('LLEN', KEYS[1])
`)

// runliveStart 出生：在 AcquireRun 的 PG 事务内调用，失败 → 上层 ROLLBACK → 500 零痕迹
// （Lua 原子：不会留半个键——这正是它被打成单条脚本的原因）。
func (s *Store) runliveStart(ctx context.Context, runID RunID, startJSON string) error {
	_, err := acquireRedisScript.Run(ctx, s.rdb,
		[]string{keyRunlive(runID), keyDraft(runID)},
		startJSON, int64(s.leaseTTL/time.Millisecond)).Result()
	return err
}

// runliveFinish 收尾：压完 terminal 后的 LLEN 即 eventOffset（旧 Lua 同一读法）。
func (s *Store) runliveFinish(ctx context.Context, runID RunID, terminalJSON string) (int, error) {
	n, err := endRunRedisScript.Run(ctx, s.rdb,
		[]string{keyRunlive(runID), keyDraft(runID)},
		terminalJSON, int64(s.ttl/time.Second)).Int64()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ReadFrames 返回 run 中 offset > from 的帧（升序，最多 w 条）与 nextOffset（空批 = from）。
// exists=false 表示 runlive 键不存在——frames 端点据此回 gone 而非 404（19v6-01 §0 URL 约定：
// 「从未存在」与「TTL 到期删除」故意不区分）。单批上限 W 与 log 共用同一条规则（planning §0 第 4 条）。
// 读快照走一笔 TxPipeline：键在读与判存在之间过期会让 status 撒谎（gone 报成 generating）。
func (s *Store) ReadFrames(ctx context.Context, runID RunID, from, w int) (frames []Frame, nextOffset int, exists bool, err error) {
	frames = []Frame{}
	nextOffset = from
	key := keyRunlive(runID)
	if w <= 0 {
		// 契约里没有「一次拿全部」：`LRANGE 0 -1` 会返全表，直接空批只答存在性。
		n, err := s.rdb.Exists(ctx, key).Result()
		if err != nil {
			return frames, nextOffset, false, err
		}
		return frames, nextOffset, n == 1, nil
	}
	pipe := s.rdb.TxPipeline()
	existsCmd := pipe.Exists(ctx, key)
	rangeCmd := pipe.LRange(ctx, key, int64(from), int64(from+w-1))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return frames, nextOffset, false, err
	}
	exists = existsCmd.Val() == 1
	for i, item := range rangeCmd.Val() {
		var f Frame
		if err := json.Unmarshal([]byte(item), &f); err != nil {
			return nil, 0, false, err
		}
		f.EventOffset = from + i + 1
		frames = append(frames, f)
		nextOffset = from + i + 1
	}
	return frames, nextOffset, exists, nil
}

// AppendDelta 追加一帧 delta 并累加 draft 全文（19v6-03「生命周期」）——每 delta 一次的热路径，D1 零变化。
// ok=false = run 不存在或已结束（draft 已随 run 结束删除，即「liveness 由 draft 承载」）。
func (s *Store) AppendDelta(ctx context.Context, runID RunID, text string) (eventOffset int, ok bool, err error) {
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
