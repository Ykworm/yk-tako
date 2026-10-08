// 本文件是存储层的装配与跨存储方法：Store 握住两半——PG（sessions / runs / run_model_messages /
// runlog_entries，见 pgstore.go）与 Redis（tk:runlive / tk:draft，见 redisstore.go）。
// 只有四个方法要同时动两半：AcquireRun / EndRun / RenewLease / Reset（planning §8.4 事务映射）。
//
// 跨存储排序的两条已裁边界（§8.4，2026-10-07）：
//   - POST = **PG 事务持有跨 Redis 写**：BEGIN → PG 写 → Redis 写 → 全成才 COMMIT；
//     Redis 败 → ROLLBACK → 500 零痕迹（与 409 同形「什么都没写」）。Redis 半是单条 Lua，
//     要么整段生效要么整段没写；
//   - endRun = **DB 先**、Redis 半失败不补偿：run 已定稿在权威（PG），runlive 走租约/300s 收敛成 gone，
//     客户端从 log 拿全文——B1 冗余收口兜住。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// errGateBusy / errGateLost 是两处守卫的哨兵（只在包内流转，不进 HTTP 层）：
// busy = runs INSERT 撞 one_live_run_per_session（→ 409）；lost = 放闸/续约 UPDATE 影响 0 行（→ 空转/判死）。
var (
	errGateBusy = errors.New("gate busy")
	errGateLost = errors.New("gate lost")
)

// Store 是四个 API 的存储后端（A4-3 裁：具体结构体，不抽接口；名字不再带介质——它跨两存储）。
//   - db：PG 半——会话登记 / 闸与租约 / 模型侧消息 / 历史定稿（永久状态）；
//   - rdb：Redis 半——runlive / draft 两类键（临时态）。
//   - ttl：runlive 在 run **正常结束**后的回收期限（19v6-03「生命周期」，dev 取 300s）——只是回收参数，
//     不是正确性参数：`done` 与 `gone` 收敛到同一条收口路径。
//   - leaseTTL：闸租约期限（A7，dev 取 30s）：runs.lease_until 与两键 TTL 出生即带、watchdog 续回；
//     进程死亡后没人续，三处同生同灭、会话自动解锁。同为回收参数：30s/5s = 连续 6 次续约全失败才误放。
type Store struct {
	db       *gorm.DB
	rdb      *redis.Client
	ttl      time.Duration
	leaseTTL time.Duration
}

// OpenStore 建起两半并各自探活（main 的 fail fast：任一半不可达 = 引擎起不来，
// 与 config.MustOpen 同一处置——不留半个只能回 500 的进程），然后 AutoMigrate 建四表。
// redisAddr 支持 `host:port` 或 `redis://host:port/N`（后者给测试用独立 DB index）。
func OpenStore(ctx context.Context, redisAddr, pgDSN string, ttl, leaseTTL time.Duration) (*Store, error) {
	rdb, err := openRedis(redisAddr)
	if err != nil {
		return nil, fmt.Errorf("redis %s: %w", redisAddr, err)
	}
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis %s: %w", redisAddr, err)
	}
	db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{TranslateError: true})
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("pg: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("pg: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("pg %s: %w", pgDSN, err)
	}
	if err := autoMigrate(db); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("pg migrate: %w", err)
	}
	return &Store{db: db, rdb: rdb, ttl: ttl, leaseTTL: leaseTTL}, nil
}

// Close 收两半的连接（测试与换实例用；常驻进程不调）。
func (s *Store) Close() error {
	err := s.rdb.Close()
	if sqlDB, dbErr := s.db.DB(); dbErr == nil && sqlDB != nil {
		if closeErr := sqlDB.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

// openRedis：裸地址走 Options；带 scheme 的走 ParseURL（测试传 redis://127.0.0.1:6390/1 隔离键空间）。
func openRedis(addr string) (*redis.Client, error) {
	if strings.Contains(addr, "://") {
		opt, err := redis.ParseURL(addr)
		if err != nil {
			return nil, err
		}
		return redis.NewClient(opt), nil
	}
	return redis.NewClient(&redis.Options{Addr: addr}), nil
}

// AcquireRun 抢闸并落 POST 事务（§8.4 第一行）：**PG 事务持有跨 Redis 写**——
// BEGIN → PG 写（懒建 session → 清扫死行 → INSERT runs → mh seq1 → runlog user 行）
// → Redis 写（runlive/draft 两键出生，单条 Lua）→ COMMIT。
// 唯一冲突 = 会话忙（one_live_run_per_session 部分唯一索引），整笔回滚零痕迹 → (false, nil) = 409；
// Redis 败 → ROLLBACK → error = 500，与 409 同形「什么都没写」。
// 抢闸 = INSERT runs 本身；出生即带 lease_until（A7-2 的 DB 形态：不存在「先建后补租约」的两步窗口）。
func (s *Store) AcquireRun(ctx context.Context, sessionID SessionID, runID RunID, user Row) (bool, error) {
	startJSON, err := json.Marshal(Frame{Kind: "start", MessageID: user.MessageID})
	if err != nil {
		return false, err
	}
	msgID, err := newRowID("msg_")
	if err != nil {
		return false, err
	}
	logID, err := newRowID("rl_")
	if err != nil {
		return false, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 懒建会话：存在即已登记；title 只在真建那一刻写（重复投递不重写，§8.6 待定项③的裁）。
		if err := tx.Exec(
			`INSERT INTO sessions (id, title, created_at) VALUES (?, ?, now()) ON CONFLICT (id) DO NOTHING`,
			sessionID, truncateTitle(user.Text)).Error; err != nil {
			return err
		}
		// 清扫死行（§8.3）：上一任进程死掉的活行按 lease 判死翻 error，唯一索引随之放行本 run。
		// error 形状 = mh 只留 user 半、runlog 无 assistant 行（与 run 内出错的形状同一）。
		if err := tx.Exec(
			`UPDATE runs SET status = 'error', finished_at = now(), lease_until = NULL
			 WHERE session_id = ? AND status = 'running' AND lease_until < now()`,
			sessionID).Error; err != nil {
			return err
		}
		// 抢闸 = 本 INSERT（AGENTS.md「闸放数据库不放进程」）；lease_until 走 DB 时钟——
		// 多副本对租约的判定共用一个时钟。
		if err := tx.Exec(
			`INSERT INTO runs (id, session_id, status, lease_until, created_at)
			 VALUES (?, ?, 'running', now() + make_interval(secs => ?), now())`,
			runID, sessionID, s.leaseTTL.Seconds()).Error; err != nil {
			if isUniqueViolation(err) {
				return errGateBusy
			}
			return err
		}
		if err := tx.Exec(
			`INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
			 VALUES (?, ?, ?, 1, 'user', ?, false, now())`,
			msgID, sessionID, runID, user.Text).Error; err != nil {
			return err
		}
		// seq 在事务内分配（MAX+1 兜底，UNIQUE(session_id, seq) 是底；一会话一活 run = 无并发分配）。
		if err := tx.Exec(
			`INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, created_at)
			 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
			         ?, 'user', ?, ?, now())`,
			logID, sessionID, sessionID, runID, user.MessageID, user.Text).Error; err != nil {
			return err
		}
		// Redis 半殿后：两键出生（同租约）。失败即回滚整笔——runlog 永不删行，只靠不写来保「零痕迹」。
		return s.runliveStart(ctx, runID, string(startJSON))
	})
	if err != nil {
		if errors.Is(err, errGateBusy) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EndRun 是 run 侧动作的唯一入口（doc/19-v6 index 概念表「run 结束」；§8.4 endRun 行）：
// **同一笔 PG 事务**「放闸 + 定稿」——UPDATE runs 落终态（放闸 = 没有独立解锁动作）与 complete 的
// mh/runlog assistant 行同事务，「mh 与闸同事务」就此从措辞变成事务边界（D 系列的立项理由之二）。
// staged（可 nil）是发送前 Stage 的折叠产物（C 系列）：**仅 complete 分支**并入同一笔事务——
// 被压 run 原文行 compacted=true（保留不删）+ 新 seq=0 摘要行；error / 闸失 = 零写入，
// 下一轮从原文重压（「原文从未被动过」，§10.4 落库时机的全部含义）。
//
// 守卫 = 统一谓词「闸在手上」（status='running' AND lease_until > now()，与读侧同式）：
// 影响 0 行 = 本 run 不归自己了（已放 / 已被清扫 / 租约已过）→ 空转（false, nil）。
// 它同时是竞态防线：行已翻 error 就不 INSERT——runlog 不会出现第二行 assistant。
//
// Redis 半在 commit 后：压 terminal + EXPIRE ttl + 删 draft。失败不补偿（§8.4）：闸确实放了（ok=true），
// runlive 到 TTL 收敛成 gone、客户端从 log 拿全文——B1 冗余收口兜住。
func (s *Store) EndRun(ctx context.Context, sessionID SessionID, runID RunID, status string, staged *StagedCompaction) (eventOffset, runlogOffset int, ok bool, err error) {
	var draft string
	if status == "complete" {
		// 定稿文本的真源在 draft（只有 Redis 有）；本 run 的 worker 已停止产出，无并发追加。
		v, gerr := s.rdb.Get(ctx, keyDraft(runID)).Result()
		if gerr != nil && !errors.Is(gerr, redis.Nil) {
			return 0, 0, false, gerr
		}
		draft = v
	}
	var nextSeq int64
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE runs SET status = ?, finished_at = now(), lease_until = NULL
			 WHERE id = ? AND session_id = ? AND status = 'running' AND lease_until > now()`,
			status, runID, sessionID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errGateLost
		}
		if status == "complete" {
			// 压缩落库先于定稿写（同笔事务，顺序无约束）：两写要么随本笔全成，要么整笔回滚。
			// UPDATE 用 compacted = false 条件——幂等半写防护：只压还可见的行（重复 run_id 天然去重）。
			if staged != nil {
				drop := make([]string, 0, len(staged.DropRunIDs))
				for _, id := range staged.DropRunIDs {
					drop = append(drop, string(id))
				}
				if len(drop) > 0 {
					if err := tx.Model(&RunModelMessage{}).
						Where("session_id = ? AND compacted = false AND run_id IN ?", sessionID, drop).
						Update("compacted", true).Error; err != nil {
						return err
					}
				}
				msgID, err := newRowID("msg_")
				if err != nil {
					return err
				}
				// 摘要行：seq=0 / role='user'（输入侧 default 分支映射成 UserMessage，§10.3 第 2 条）/
				// compacted=false（它就是要被读回模型输入的那条）。挂点撞 UNIQUE(run_id, seq) = 整笔失败，
				// 是「挂点唯一性」不变式的最后一道机器守卫。
				if err := tx.Exec(
					`INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
					 VALUES (?, ?, ?, 0, 'user', ?, false, now())`,
					msgID, sessionID, staged.AttachRun, staged.Summary).Error; err != nil {
					return err
				}
			}
			msgID, err := newRowID("msg_")
			if err != nil {
				return err
			}
			if err := tx.Exec(
				`INSERT INTO run_model_messages (id, session_id, run_id, seq, role, text, compacted, created_at)
				 VALUES (?, ?, ?, 2, 'assistant', ?, false, now())`,
				msgID, sessionID, runID, draft).Error; err != nil {
				return err
			}
			logID, err := newRowID("rl_")
			if err != nil {
				return err
			}
			// RETURNING 拿回本行 seq 即 runlogOffset（旧 Lua 的 LLEN 语义）。
			if err := tx.Raw(
				`INSERT INTO runlog_entries (id, session_id, seq, run_id, role, message_id, text, created_at)
				 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runlog_entries WHERE session_id = ?),
				         ?, 'assistant', NULL, ?, now())
				 RETURNING seq`,
				logID, sessionID, sessionID, runID, draft).Scan(&nextSeq).Error; err != nil {
				return err
			}
		} else {
			// 非 complete 不定稿：runlogOffset 仍返回当前高水位（旧 Lua 的 LLEN 语义原样）。
			if err := tx.Raw(
				`SELECT COALESCE(MAX(seq), 0) FROM runlog_entries WHERE session_id = ?`,
				sessionID).Scan(&nextSeq).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errGateLost) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	terminalJSON, err := json.Marshal(Frame{Kind: "terminal", Status: status})
	if err != nil {
		return 0, int(nextSeq), true, err
	}
	eventOffset, err = s.runliveFinish(ctx, runID, string(terminalJSON))
	return eventOffset, int(nextSeq), true, err
}

// RenewLease 由 worker watchdog 在生成期间定期调用（§8.3）：PG 半是 CAS 本体——
// `UPDATE runs SET lease_until = now()+lease WHERE id AND session_id AND status='running' AND lease_until > now()`，
// 影响 0 行 = 闸已不属于本 run（已放 / 已易主 / 租约已过）→ (false, nil) = 判死，调用方停续。
// `lease_until > now()` 那一半是「过期不可复活」：A7 的 Redis CAS（tk:live 蒸发即 no-op）逐字同义——
// 少了它，「过期但未清扫」的 run 会被迟到续约复活，而 draft 已蒸发，收尾会拿空文本定稿出
// 「complete 但空回复」的形状。Redis 半（两键 PEXPIRE 回 lease）在 PG 说活之后才刷；Redis 败 → error，
// watchdog 下一拍重试（PG 半已续，多续一次无害）。
func (s *Store) RenewLease(ctx context.Context, sessionID SessionID, runID RunID) (bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE runs SET lease_until = now() + make_interval(secs => ?)
		 WHERE id = ? AND session_id = ? AND status = 'running' AND lease_until > now()`,
		s.leaseTTL.Seconds(), runID, sessionID)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	if _, err := renewRedisScript.Run(ctx, s.rdb,
		[]string{keyDraft(runID), keyRunlive(runID)},
		int64(s.leaseTTL/time.Millisecond)).Result(); err != nil {
		return false, err
	}
	return true, nil
}

// Reset 全局归零（dev-only 端点 /api/dev/reset 的底座，§8.4）：PG 四表 TRUNCATE + `SCAN tk:*` + UNLINK，
// 返回 Redis 删除**键数**（响应字节与旧形态一致）。
// 四表同清：`runs` 必须清——它是闸的落点，漏清则幽灵 running 行永久残留
// （清扫只在新 POST 时按 session_id 触发，旧 session 已 404，再没人替它扫）。
func (s *Store) Reset(ctx context.Context) (deleted int, err error) {
	if err := s.db.WithContext(ctx).Exec(
		`TRUNCATE TABLE sessions, runs, run_model_messages, runlog_entries`).Error; err != nil {
		return 0, err
	}
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

// isUniqueViolation 认「唯一约束冲突」：TranslateError 路径翻成 gorm.ErrDuplicatedKey；
// 兜底按 SQLSTATE 23505 直认（raw Exec 等路径可能不经翻译层）。
// 它是「插入失败本身就是闸」的唯一判定点——认漏 = 会话忙报成 500。
func isUniqueViolation(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// truncateTitle 截首条用户消息作会话 title 初值（§8.6 待定项③：初值取截断、之后不重写）。
// 按 rune 截、80 字——只影响展示列，不进四个 API 的契约字节。
func truncateTitle(text string) string {
	const maxRunes = 80
	r := []rune(text)
	if len(r) <= maxRunes {
		return text
	}
	return string(r[:maxRunes])
}
