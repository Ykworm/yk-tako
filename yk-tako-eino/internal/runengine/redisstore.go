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

// keyRunlive：tk:runlive:<runID> —— LIST，按发生顺序存全部帧 JSON（start / delta / tool_call /
// tool_result / terminal）。消费端是纯 HTTP 轮询（GET /frames 按 eventOffset 拉取，doc/30 §2.1：
// 服务端无长连接、无推送），ReadFrames 从这里按 index 读，index+1 即 eventOffset。
func keyRunlive(runID RunID) string { return keyPrefix + "runlive:" + string(runID) }

// keyDraft：tk:draft:<runID> —— STRING，只存「纯文本定稿累加器」（每次 APPEND delta 的 text 字段）。
// 双重身份：① run 进行中 = liveness 哨兵（EXISTS=1 即判活依据）；② run 结束时最终值 = assistant 回复全文，
// 直接拿去写 runlog_entries 的 assistant 行，不用再从 runlive 重拼。
func keyDraft(runID RunID) string { return keyPrefix + "draft:" + string(runID) }

// keyRunepoch：tk:runepoch:<runID> —— STRING，本 run 当前执行者的接管令牌（doc/30 §7）。
// 出生 = 1、每次 claim 续跑 +1（A4）；与两键同 TTL，续约时比对+刷新，不存在/不相等 = 本执行者
// 已被接管或 run 已收尾。帧追加与续约都先看它：旧进程的写在后继执行者接管后必须失效。
func keyRunepoch(runID RunID) string { return keyPrefix + "runepoch:" + string(runID) }

// acquireRedisScript：run 出生（AcquireRun 的 Redis 半）——建 tk:runlive 压 start 帧 + 建空 tk:draft，
// **两键出生即带租约**（A7-2：生产窗口里它们要等首次续约才拿到 TTL，裸建期间死掉即是垃圾帧；
// 现在与 PG 的 lease_until 同笔设立，不存在两步窗口）。单条 Lua = 原子：
// 跨存储事务里 Redis 半要么两键齐立、要么一字未写。
// KEYS: runlive draft runepoch；ARGV: startFrameJSON leaseMillis epoch。
// 三键同笔出生：令牌键与两键同 TTL，不存在「已出生但还没令牌」的窗口。
var acquireRedisScript = redis.NewScript(`
redis.call('RPUSH',  KEYS[1], ARGV[1])  -- KEYS[1]=tk:runlive:<run>  ARGV[1]=start 帧 JSON
redis.call('PEXPIRE', KEYS[1], ARGV[2]) -- KEYS[1]=tk:runlive:<run>  ARGV[2]=lease 毫秒
redis.call('SET',     KEYS[2], '')      -- KEYS[2]=tk:draft:<run>   建空累加器
redis.call('PEXPIRE', KEYS[2], ARGV[2]) -- KEYS[2]=tk:draft:<run>   ARGV[2]=lease 毫秒
redis.call('SET',     KEYS[3], ARGV[3]) -- KEYS[3]=tk:runepoch:<run> 出生令牌（1）
redis.call('PEXPIRE', KEYS[3], ARGV[2]) -- KEYS[3]=tk:runepoch:<run> ARGV[2]=lease 毫秒
return 1
`)

// renewRedisScript：把两键 TTL 同刷回 lease（A7 语义原样：draft/runlive 与闸同生同灭）。
// **CAS 不在这一半**——判死守卫是 PG 的 `UPDATE ... WHERE status='running' AND lease_until > now()`
// 的影响行数（§8.3）；PG 说本 run 还活着，才轮到刷这两键。
// KEYS: draft runlive runepoch；ARGV: leaseMillis epoch。
// 令牌比对在刷新之前：取得 ≠ 本执行者（含键已不存在）= 0，调用方按判死处理——旧执行者
// 不能靠续约把 TTL 刷回去。PG 半已先验过 epoch，此处是 Redis 侧的同一道闸。
var renewRedisScript = redis.NewScript(`
if redis.call('GET', KEYS[3]) ~= ARGV[2] then  -- KEYS[3]=tk:runepoch  ARGV[2]=本执行者 epoch
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[1])  -- KEYS[1]=tk:draft:<run>    ARGV[1]=lease 毫秒
redis.call('PEXPIRE', KEYS[2], ARGV[1])  -- KEYS[2]=tk:runlive:<run>  ARGV[1]=lease 毫秒
redis.call('PEXPIRE', KEYS[3], ARGV[1])  -- KEYS[3]=tk:runepoch:<run> ARGV[1]=lease 毫秒
return 1
`)

// appendDeltaScript：liveness 由 `tk:draft` 承载（run 结束时它被删），不在则返 0。
// KEYS: draft runlive runepoch；ARGV: deltaFrameJSON text epoch。
// 两道守卫同真才追加：draft 在（run 活）+ 令牌相等（本执行者仍被认可）。任一不成立 = 0。
var appendDeltaScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then  -- KEYS[1]=tk:draft:<run>（哨兵：不在=run 已死）
  return 0
end
if redis.call('GET', KEYS[3]) ~= ARGV[3] then -- KEYS[3]=tk:runepoch  ARGV[3]=本执行者 epoch
  return 0
end
redis.call('RPUSH',  KEYS[2], ARGV[1])  -- KEYS[2]=tk:runlive:<run>  ARGV[1]=delta 帧 JSON（审计流追加）
redis.call('APPEND', KEYS[1], ARGV[2])  -- KEYS[1]=tk:draft:<run>    ARGV[2]=delta 纯文本（定稿累加）
return redis.call('LLEN', KEYS[2])      -- KEYS[2]=tk:runlive:<run>  返回新帧的 eventOffset
`)

// appendFrameScript：归一层追加一帧**非 delta** 帧（T2 起：tool_call / tool_result）。
// 与 delta 同守卫（draft 在 = run 活）；**不碰 draft**——draft 只承载回复正文，
// 「Σ delta == 定稿全文」的记账不被工具帧打扰（19v6-03「生命周期」）。
// KEYS: draft runlive runepoch；ARGV: frameJSON epoch。
// 守卫与 appendDelta 同式（draft 在 + 令牌相等）：旧执行者的工具/用量帧不许混进接管者的现场。
var appendFrameScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then  -- KEYS[1]=tk:draft:<run>（哨兵：不在=run 已死）
  return 0
end
if redis.call('GET', KEYS[3]) ~= ARGV[2] then -- KEYS[3]=tk:runepoch  ARGV[2]=本执行者 epoch
  return 0
end
redis.call('RPUSH', KEYS[2], ARGV[1])      -- KEYS[2]=tk:runlive:<run> ARGV[1]=工具帧 JSON（仅入审计流）
return redis.call('LLEN', KEYS[2])         -- KEYS[2]=tk:runlive:<run> 返回新帧的 eventOffset
`)

// endRunRedisScript：run 收尾的 Redis 半（EndRun 的 Redis 半，commit 后调用）——
// 压 terminal + 设 300s TTL + 立即删 draft。**无守卫**：放闸与定稿已由 PG 事务完成（store.go EndRun），
// 闸不在就根本不会调到这里。KEYS: runlive draft runepoch；ARGV: terminalFrameJSON ttlSeconds。
// 令牌键同删：run 已收尾，任何残留执行者都不该再被认为持有闸。
var endRunRedisScript = redis.NewScript(`
redis.call('RPUSH',  KEYS[1], ARGV[1])  -- KEYS[1]=tk:runlive:<run>  ARGV[1]=terminal 帧 JSON
redis.call('EXPIRE', KEYS[1], ARGV[2])  -- KEYS[1]=tk:runlive:<run>  ARGV[2]=收尾 TTL（秒）
redis.call('DEL',    KEYS[2])           -- KEYS[2]=tk:draft:<run>    删除哨兵=对外宣告 run 已死
redis.call('DEL',    KEYS[3])           -- KEYS[3]=tk:runepoch:<run> 删除令牌
return redis.call('LLEN', KEYS[1])      -- KEYS[1]=tk:runlive:<run>  返回 terminal 帧 eventOffset
`)

// seedResumeScript：接管后的 Redis 重建（doc/30 §8.3）。三件事一笔原子：
// 令牌先设成新值（旧执行者从这一刻起所有帧写失效）→ 旧 runlive 整个丢掉、压 resume 帧
// （崩溃前的半截 delta 从未进账本，不留在新流里）→ draft 用 SET 覆盖为账本正文拼接
// （之后的新 delta 照旧 APPEND，EndRun 时 draft = 崩溃前已定稿正文 + 新生成正文）。
// KEYS: runlive draft runepoch；ARGV: newEpoch resumeFrameJSON leaseMillis seedText。
var seedResumeScript = redis.NewScript(`
redis.call('SET',     KEYS[3], ARGV[1])  -- KEYS[3]=tk:runepoch:<run> ARGV[1]=新令牌
redis.call('DEL',     KEYS[1])           -- KEYS[1]=tk:runlive:<run>  旧帧全部丢掉
redis.call('RPUSH',   KEYS[1], ARGV[2])  -- KEYS[1]=tk:runlive:<run>  ARGV[2]=resume 帧 JSON
redis.call('PEXPIRE', KEYS[1], ARGV[3])  -- KEYS[1]=tk:runlive:<run>  ARGV[3]=lease 毫秒
redis.call('SET',     KEYS[2], ARGV[4])  -- KEYS[2]=tk:draft:<run>    ARGV[4]=种子正文
redis.call('PEXPIRE', KEYS[2], ARGV[3])  -- KEYS[2]=tk:draft:<run>    ARGV[3]=lease 毫秒
return 1
`)

// runliveStart 出生：在 AcquireRun 的 PG 事务内调用，失败 → 上层 ROLLBACK → 500 零痕迹
// （Lua 原子：不会留半个键——这正是它被打成单条脚本的原因）。
func (s *Store) runliveStart(ctx context.Context, runID RunID, epoch int64, startJSON string) error {
	// KEYS[1]=tk:runlive（主写对象放第一位，见 acquireRedisScript 注释）
	// KEYS[2]=tk:draft；KEYS[3]=tk:runepoch
	// ARGV[1]=start 帧 JSON；ARGV[2]=lease 毫秒（与 PG lease_until 同值）；ARGV[3]=出生令牌
	_, err := acquireRedisScript.Run(ctx, s.rdb,
		[]string{keyRunlive(runID), keyDraft(runID), keyRunepoch(runID)},
		startJSON, int64(s.leaseTTL/time.Millisecond), epoch).Result()
	return err
}

// seedResume 重建接管者的 Redis 现场（claim 提交后调用）：令牌换成新代、runlive 重置为
// 一条 resume 帧、draft 覆盖为账本正文拼接。失败由调用方 AbandonClaim 补偿租约并回 500。
func (s *Store) seedResume(ctx context.Context, runID RunID, epoch int64, frameJSON, seed string) error {
	_, err := seedResumeScript.Run(ctx, s.rdb,
		[]string{keyRunlive(runID), keyDraft(runID), keyRunepoch(runID)},
		epoch, frameJSON, int64(s.leaseTTL/time.Millisecond), seed).Result()
	return err
}

// runliveFinish 收尾：压完 terminal 后的 LLEN 即 eventOffset（旧 Lua 同一读法）。
// 注意：这步在 PG 事务 COMMIT **之后**调用（§8.4 endRun 行——DB 先定，Redis 随后收敛），
// 所以哪怕 Redis 败了也不回滚；runlive 靠 300s TTL 自删，客户端从 log 拿全文兜住。
func (s *Store) runliveFinish(ctx context.Context, runID RunID, terminalJSON string) (int, error) {
	// KEYS[1]=tk:runlive（主写对象放第一位，与出生对称）
	// KEYS[2]=tk:draft；KEYS[3]=tk:runepoch
	// ARGV[1]=terminal 帧 JSON；ARGV[2]=收尾 TTL 秒（Store.ttl，dev=300s）
	n, err := endRunRedisScript.Run(ctx, s.rdb,
		[]string{keyRunlive(runID), keyDraft(runID), keyRunepoch(runID)},
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
		// 契约里没有「一次拿全部」：`LRANGE 0 -1` 会返全表，所以 w<=0 作为「只答存在性」的探测口。
		// 典型用法：客户端拿到 200 POST 响应后先问一次（from=0,w=0），拿 exists=true 再开始真正轮询。
		n, err := s.rdb.Exists(ctx, key).Result()
		if err != nil {
			return frames, nextOffset, false, err
		}
		return frames, nextOffset, n == 1, nil
	}
	// TxPipeline：把 EXISTS + LRANGE 打包成服务端原子快照——防下面这串竞态：
	//   T1: EXISTS → 1（以为还活着）
	//   T1 与 T2 之间：TTL 过期、键被删
	//   T2: LRANGE → 空（以为还在生成，只是没新帧）
	// 结果：前端以为 run 在空转，实际已 gone。同 Pipeline 保证两条命令看到同一个键状态。
	pipe := s.rdb.TxPipeline()
	existsCmd := pipe.Exists(ctx, key)
	rangeCmd := pipe.LRange(ctx, key, int64(from), int64(from+w-1)) // from=from（eventOffset > from ⇒ LIST index ≥ from）
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return frames, nextOffset, false, err
	}
	exists = existsCmd.Val() == 1
	for i, item := range rangeCmd.Val() {
		var f Frame
		if err := json.Unmarshal([]byte(item), &f); err != nil {
			return nil, 0, false, err
		}
		// eventOffset = LIST index（0-based）+ 1。from 已经是「跳过前 from 个」，
		// 所以第 i 条在列表里的位置 = from+i，+1 即 1-based 序号。
		f.EventOffset = from + i + 1
		frames = append(frames, f)
		// nextOffset = 这批最后一条的 eventOffset；下一轮传它来拿「> nextOffset」的新帧。
		nextOffset = from + i + 1
	}
	return frames, nextOffset, exists, nil
}

// AppendDelta 追加一帧 delta 并累加 draft 全文（19v6-03「生命周期」）——每 delta 一次的热路径，D1 零变化。
// model 是产出该 delta 的模型生效 ref（T3-1b）：本口只服务 root 正文（子代理 delta 走 AppendFrame、
// 不累 draft）。ok=false = run 不存在或已结束（draft 已随 run 结束删除，即「liveness 由 draft 承载」），
// 或本执行者已不是当前令牌持有者（doc/30 §7：旧进程的帧不许混进接管者的现场）。
func (s *Store) AppendDelta(ctx context.Context, runID RunID, epoch int64, model, text string) (eventOffset int, ok bool, err error) {
	// 帧本身在写侧不填 EventOffset——读时 ReadFrames 按 LIST index+1 统一注入，写侧只读 LLEN 当返回值。
	frameJSON, err := json.Marshal(Frame{Kind: "delta", Model: model, Text: text})
	if err != nil {
		return 0, false, err
	}
	// KEYS 顺序 [draft, runlive, runepoch]（哨兵第一位，见 appendDeltaScript 注释）：
	//   KEYS[1]=tk:draft    —— liveness 守卫 + 定稿累加器（APPEND text）
	//   KEYS[2]=tk:runlive  —— 审计流（RPUSH 帧 JSON）
	//   KEYS[3]=tk:runepoch —— 令牌守卫
	// ARGV[1]=delta 帧 JSON；ARGV[2]=delta 纯文本（draft 累加用，避免再从 JSON 反解一次）；ARGV[3]=epoch
	n, err := appendDeltaScript.Run(ctx, s.rdb,
		[]string{keyDraft(runID), keyRunlive(runID), keyRunepoch(runID)}, frameJSON, text, epoch).Int64()
	if err != nil {
		return 0, false, err
	}
	// 返回 0 = draft 不存在（run 判死）→ ok=false，上层 worker 停止推送。
	if n == 0 {
		return 0, false, nil
	}
	return int(n), true, nil
}

// AppendFrame 追加一帧工具帧（T2 归一层写口）。ok=false = run 判死（与 AppendDelta 同一判活口径：
// draft 不在或令牌不符即 no-op）。EventOffset 写侧不设——读时按 LIST index+1 注入（同 delta/start/terminal）。
func (s *Store) AppendFrame(ctx context.Context, runID RunID, epoch int64, frame Frame) (ok bool, err error) {
	// 注意：frame 由调用方（worker）填 Kind（tool_call / tool_result）和 payload 字段，
	// 这里只做 JSON 序列化——存储层不关心工具帧内部长什么样。
	frameJSON, err := json.Marshal(frame)
	if err != nil {
		return false, err
	}
	// KEYS 顺序 [draft, runlive, runepoch]（与 AppendDelta 一致）：
	//   KEYS[1]=tk:draft    —— 仅作 liveness 守卫（**不 APPEND**，工具帧不参与定稿正文）
	//   KEYS[2]=tk:runlive  —— 审计流（RPUSH 帧 JSON）
	//   KEYS[3]=tk:runepoch —— 令牌守卫
	// ARGV[1]=工具帧 JSON；ARGV[2]=epoch
	n, err := appendFrameScript.Run(ctx, s.rdb,
		[]string{keyDraft(runID), keyRunlive(runID), keyRunepoch(runID)}, string(frameJSON), epoch).Int64()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
