package runengine

// 本文件是「合作写者互斥」的锁层（方案真源 = doc/71 §4/§5；施工包 §9.1）。
//
// 三件东西各管一段，谁也替代不了谁（与 filefp.go 的指纹/基准表是同一套分工）：
//
//	① 锁（本文件）      —— 此刻有没有另一个**合作写者**正在写这个文件。
//	② 基准表 fileBaseline —— 我生成内容时读到的那一版，现在还是不是当前版。
//	③ 原子替换 atomicfile.go —— 读者此刻会不会看到只写了一半的文件。
//
// 少了 ①，检查与写入之间会插入别人；少了 ②，旧理解会覆盖新版本；少了 ③，不参加锁的
// 读者可能看到半截内容。三样都要，缺一样就有一类事故没人管。
//
// 锁的形态分两层，语义不许混：
//
//	PG advisory lock —— **所有副本共享**的一把锁。跨机器成立：副本 A 在机器 1、
//	                    副本 B 在机器 2，两者都连同一个 PG → 互斥成立。
//	                    这是生产路径的正确性来源。
//	flock            —— 只在本机的 inode 上。跨机器看不见它 → **不能**当跨副本互斥。
//	                    它在生产路径上只是 PG 锁之外的**本机附加保护**（同一台机器上
//	                    多个进程共用一把文件锁时少一轮 PG 往返）。
//
// 边界（诚实登记）：flock 与 PG 锁都只约束**合作写者**——走 tako 文件工具的那些。
// bash（rm / sed -i / > 重定向）、外部编辑器、别的服务都不取这两把锁（advisory 的本质）。
// 不能因为文件工具里有锁，就宣称整个 workspace 没有并发写风险（doc/71 §5.3）。
//
// 取锁用「非阻塞试 + 轮询」而不是阻塞式调用：阻塞式 pg_advisory_lock / 阻塞 flock
// 会让「等待上限 30 秒」变成一句空话——超时不返回 = 上限不存在。轮询节奏沿用
// filefp.go 的 lockWaitInterval / lockTimeout 口径。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"
)

// FileLocker 是合作写者的排它锁：同一时刻同一批逻辑键只能有一个写者。
//
// 「逻辑键」是调用方给的**显式字符串**，不限于文件路径——后续的 markdown 记忆模式会传
// 「tenant+user」「tenant+workspace」这类不是路径的键。实现方只保证「同键互斥、
// 不同键不互相阻塞」。
//
// 取不到锁时返回错误；等待超上限的错误是**可重试**的（errors.Is(err, errLockTimeout)）——
// 文案要让模型知道可以稍后再试，而不是「这次失败了」就放弃。
type FileLocker interface {
	// Lock 按固定顺序取这些逻辑键的排它锁，返回释放函数。
	// 释放函数必须幂等（重复调用安全），且释放后不得把仍持锁的资源留在外面。
	Lock(ctx context.Context, keys []string) (release func(), err error)
}

// ── 逻辑键 ──────────────────────────────────────────────────────────────────

// fileLockSubject 造普通文件的逻辑键：规范化绝对路径。
//
// 规范化 = Abs + Clean：同一份文件的两种写法（带 ./、带 ..、相对路径）必须落到同一个键，
// 否则两个写者会各拿一把不同的锁 → 互斥失效（这是真的会发生的事：同一个文件，
// 一个写者传 "notes/a.txt"、另一个传 "./notes/a.txt"）。
func fileLockSubject(absPath string) string {
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return filepath.Clean(absPath)
	}
	return filepath.Clean(abs)
}

// pgLockKey 造 PG 侧的完整锁键：租户 + 逻辑键。
//
// 租户必须在键里：PG 是**所有副本共享**的一台库，不带租户 = 跨租户的文件互相等锁。
//
// 拼接用**长度前缀**（"<租户字节数>:<租户>|<逻辑键>"）而不是裸分隔符：
// 长度前缀让租户与逻辑键的边界唯一可判——分隔符出现在内容里也不会撞键
// （tenant="a|b" 与 tenant="a"+key="b" 必须是两个不同的键）。
//
// 分隔符也不能用 \x00：PG 的文本参数里不能有 NUL 字节（报 SQLSTATE 22021），
// 而这条键是当**文本参数**传给 hashtextextended 的。
func pgLockKey(tenant, subject string) string {
	return strconv.Itoa(len(tenant)) + ":" + tenant + "|" + subject
}

// tenantKeyFrom 取当前上下文的租户 id：优先工作区引用（run 路径一定有），退到登录身份。
// 都没有 → 空串（单测与旧调用方）：键仍然唯一（路径本身唯一），只是少了租户这一层隔离。
func tenantKeyFrom(ctx context.Context) string {
	if ref, ok := workspaceFrom(ctx); ok && ref.TenantID != "" {
		return ref.TenantID
	}
	if p, ok := principalFromCtx(ctx); ok && p.TenantID != "" {
		return p.TenantID
	}
	return ""
}

// dedupSortedKeys 去重并按字典序排序。
//
// 两条规则缺一就出事：
//  1. 去重：同一把锁取两次 = 自己等自己（PG 会话级 advisory lock 同键可重入，flock 则是
//     同一进程两次 open = 两个 open file description → 第二次阻塞在自己第一次上）；
//  2. 排序：A 拿 L1 再 L2、B 拿 L2 再 L1 → 互等（AB-BA 死锁）。
func dedupSortedKeys(keys []string) []string {
	uniq := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, k)
	}
	sort.Strings(uniq)
	return uniq
}

// ── PG advisory lock ────────────────────────────────────────────────────────

// pgFileLocker 用 PG 的**会话级** advisory lock 做多副本互斥。
//
// 为什么是会话级而不是事务级（pg_advisory_xact_lock）：文件 rename 不属于任何数据库
// 事务，事务一提交锁就没了 → 锁的持有期必须能超出单条 SQL。代价是要自己管连接。
//
// 会话级锁挂在 PG 后端**会话**上：**连接断开时 PG 自动释放**（进程被 kill -9 也一样，
// 因为 TCP 断了 PG 后端发现会话结束 → 释放它持有的全部 advisory lock）。
// 所以取锁必须用**独占连接**，而且绝不能把还持锁的连接还回连接池——
// 那条连接会被别的请求借去继续挂着锁，后来者就一直等不到（直到超时）。
type pgFileLocker struct {
	db       *gorm.DB
	timeout  time.Duration
	interval time.Duration
}

// newPGFileLocker 造 PG 版写锁。timeout 是等待上限（0 = 用默认 lockTimeout = 30 秒）。
func newPGFileLocker(db *gorm.DB, timeout time.Duration) *pgFileLocker {
	if timeout <= 0 {
		timeout = lockTimeout
	}
	return &pgFileLocker{db: db, timeout: timeout, interval: lockWaitInterval}
}

// Lock 取这些逻辑键的 PG advisory lock（跨副本互斥）。
//
// 步骤：
//  1. 去重 + 按字典序排（防 AB-BA 死锁）；
//  2. 从连接池取**独占连接**（会话级锁必须钉在一个 PG 会话上）；
//  3. 逐个键轮询 pg_try_advisory_lock（非阻塞试；拿不到就等一个 interval 再试），
//     全部键拿到才算取到；中途超时 → 释放已拿到的键并返回可重试错误；
//  4. 返回释放函数：先解锁，再把连接还回池子。
func (l *pgFileLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	subjects := dedupSortedKeys(keys)
	if len(subjects) == 0 {
		return func() {}, nil
	}
	if l == nil || l.db == nil {
		return nil, errors.New("取写锁失败：没有数据库连接")
	}
	sqlDB, err := l.db.DB()
	if err != nil {
		return nil, fmt.Errorf("取写锁失败：取数据库连接池失败：%w", err)
	}
	// 独占连接：会话级 advisory lock 挂在**这条连接对应的 PG 会话**上。
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("取写锁失败：取独占连接失败：%w", err)
	}
	tenant := tenantKeyFrom(ctx)
	deadline := time.Now().Add(l.timeout)
	held := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		key := pgLockKey(tenant, subject)
		for {
			var ok bool
			// pg_try_advisory_lock 是**非阻塞**的：拿不到就返回 false，锁状态不变
			// （false 时不计数、不会留下半把锁）。不能用阻塞式 pg_advisory_lock——
			// 那会让「等待上限」形同虚设。
			if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&ok); err != nil {
				// 连接坏了 → 这条会话上的锁随会话一起去死（PG 自动释放），不必手动解锁。
				discardConn(conn)
				return nil, fmt.Errorf("取写锁失败：%w", err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				// 超时 = 可重试：稍后重试大概率能拿到。
				if rerr := releasePGConn(conn, held); rerr != nil {
					discardConn(conn)
				}
				return nil, errLockTimeout
			}
			select {
			case <-ctx.Done():
				if rerr := releasePGConn(conn, held); rerr != nil {
					discardConn(conn)
				}
				return nil, ctx.Err()
			case <-time.After(l.interval):
			}
		}
		held = append(held, key)
	}
	// 正常释放：先解锁，再把连接还回池子（还回去之前必须已经不再持锁）。
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := releasePGConn(conn, held); err != nil {
				// 解锁失败 → 连接状态不可信 → 宁可断线也不把持锁连接还回池子。
				log.Printf("filelock: 解锁失败，丢弃连接：%v", err)
				discardConn(conn)
				return
			}
		})
	}
	return release, nil
}

// releasePGConn 逆序解锁这批键，然后**把连接还回连接池**。
//
// 解锁失败时**不**还池（还回去 = 锁还挂着 + 这条连接会被借去干别的），
// 改由调用方 discardConn 让它真的断开。
func releasePGConn(conn *sql.Conn, keys []string) error {
	var firstErr error
	for i := len(keys) - 1; i >= 0; i-- {
		var released bool
		if err := conn.QueryRowContext(context.Background(),
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, keys[i]).Scan(&released); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !released && firstErr == nil {
			firstErr = fmt.Errorf("写锁未持有（key %q）", keys[i])
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return conn.Close()
}

// discardConn 让这条连接**真的断开**，而不是回到连接池。
//
// 为什么必须真断：会话级 advisory lock 挂在 PG 后端会话上。连接还回池子 = 锁还挂着，
// 而这条连接会被别的请求借去用 → 后来者一直等不到锁（等到超时也拿不到）。
// database/sql 的 Conn.Close() 只是「还回池子」；要让驱动丢弃这条连接，得在 Raw 回调里
// 返回 driver.ErrBadConn —— database/sql 收到它就关闭底层驱动连接
// （closemuRUnlockCondReleaseConn → close → dc.releaseConn → putConn(ErrBadConn) → 关闭驱动连接）。
// PG 那边会话一断，**它自动释放这个会话持有的全部 advisory lock** →
// 异常路径与崩溃路径都不会留下死锁。
func discardConn(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// ── 本机 flock ──────────────────────────────────────────────────────────────

// flockFileLocker 是本机 flock 锁：只在本机的进程之间互斥。
//
// 它**不能**当跨副本互斥用（flock 挂在本机 inode 上，别的机器看不见它）。
// 生产路径上它是 PG 锁之外的本机附加保护；单测与旧调用方没有 PG 时，它是唯一的那把。
type flockFileLocker struct {
	root string
}

// newFlockFileLocker 造本机 flock 版写锁。root 是作业根（锁目录的兜底根）。
func newFlockFileLocker(root string) *flockFileLocker {
	return &flockFileLocker{root: root}
}

// Lock 取这些逻辑键的本机 flock 锁。锁目录按租户分（tenants/<tid>/.tako/locks），
// 所以租户隔离由目录承担，键里不再带租户。
//
// 两侧的键**不一样**是故意的：
//   - PG 侧带租户（pgLockKey）→ 全局唯一（PG 是所有副本共享的一台库）；
//   - flock 侧用逻辑键本身 → flock 的锁目录本来就是**每租户一个**，租户隔离由目录承担，
//     键里再带一遍只会让锁文件散到别的桶，也和既有 lockPathFor 的口径保持一致
//     （dev 端点报的锁文件路径仍是工具真正用的那把）。
func (l *flockFileLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	return lockPathsCtx(ctx, lockDirFor(ctx, l.root), keys)
}

// ── 两层串联 ────────────────────────────────────────────────────────────────

// chainFileLocker 按**固定顺序**取两层锁：先 PG（跨副本），再 flock（本机）。
//
// 顺序固定是硬要求：反序（先 flock 再 PG）会出现「A 拿 flock 等 PG、B 拿 PG 等 flock」
// 的环。两层各管一段，不互相替代。
type chainFileLocker struct {
	pg    FileLocker
	local FileLocker
}

// newChainFileLocker 造两层串联锁。任一层为 nil 就只用另一层。
func newChainFileLocker(pg, local FileLocker) *chainFileLocker {
	return &chainFileLocker{pg: pg, local: local}
}

// Lock 先取 PG 锁再取 flock 锁；任一层失败都把已取到的全部释放再返回错误。
func (c *chainFileLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	if c == nil {
		return func() {}, nil
	}
	if c.pg == nil {
		if c.local == nil {
			return func() {}, nil
		}
		return c.local.Lock(ctx, keys)
	}
	rel, err := c.pg.Lock(ctx, keys)
	if err != nil {
		return nil, err
	}
	if c.local == nil {
		return rel, nil
	}
	rel2, err := c.local.Lock(ctx, keys)
	if err != nil {
		rel() // PG 锁先放掉，别把它漏在连接池外面
		return nil, err
	}
	return func() {
		rel2()
		rel()
	}, nil
}

// ── ctx 注入 ────────────────────────────────────────────────────────────────

type fileLockerKey struct{}

// WithFileLocker 把写锁提供者挂进 ctx。
//
// 走 ctx 而不是改工具工厂的签名：工具工厂（newWriteFileTool 等）拿不到数据库，
// 改签名会波及工具池装配与既有单测。ctx 注入与 fileBaselineFrom（filefp.go）同一套路。
func WithFileLocker(ctx context.Context, l FileLocker) context.Context {
	return context.WithValue(ctx, fileLockerKey{}, l)
}

// fileLockerFrom 取 ctx 上的写锁提供者；没有 → nil（调用方退化成只有本机 flock）。
func fileLockerFrom(ctx context.Context) FileLocker {
	l, _ := ctx.Value(fileLockerKey{}).(FileLocker)
	return l
}

// lockFilePaths 是文件工具取写锁的唯一入口。
//
// keys 传的是**逻辑键**：普通文件传规范化绝对路径（fileLockSubject）；
// 后续的 markdown 记忆模式会传 tenant+user / tenant+workspace 这类逻辑键。
//
// ctx 上没有写锁提供者时退化成现有的本机 flock（单测与旧调用方不炸）——
// 这是诚实的兜底：没有数据库就没有跨副本互斥，flock 只在本机有效。
func lockFilePaths(ctx context.Context, root string, paths []string) (func(), error) {
	subjects := make([]string, 0, len(paths))
	for _, p := range paths {
		subjects = append(subjects, fileLockSubject(p))
	}
	if locker := fileLockerFrom(ctx); locker != nil {
		return locker.Lock(ctx, subjects)
	}
	return lockPathsCtx(ctx, lockDirFor(ctx, root), paths)
}
