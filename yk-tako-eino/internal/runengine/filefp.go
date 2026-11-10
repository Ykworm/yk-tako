package runengine

// 本文件是「同文件并发写」的底座（方案真源 = doc/71 §4/§5；产品侧口径见 doc/42 §2.4）。
//
// 三样东西各管一段，谁也替代不了谁：
//
//	① 指纹 Fingerprint    —— 一个文件此刻的样子（mtime + size + sha256）。
//	② 基准表 fileBaseline —— 「某个 agent 读到的是哪一版」——run 级内存，不落库、不跨 run 共享。
//	③ 写锁                —— 此刻有没有另一个**合作写者**正在写。
//	   跨副本靠 filelock.go 的 PG advisory lock（所有副本共享）；本机的 flock 在这里，
//	   只管同机进程之间（flock 挂在本机 inode 上，别的机器看不见它 → 不能当跨副本互斥）。
//
// 为什么三者都要：
//   - 只有锁：B 的内容在拿锁之前就基于旧版算好了 → 照样用旧版覆盖新版（两边都报成功）。
//   - 只有基准表：检查与写不是原子的 → 两者之间有缝。
//   - 锁 + 基准表，且**比对在锁内**：B 取到锁之后才去比 → 必然看到 A 写进去的东西 → 拒。
//   - 再加原子替换（atomicfile.go）：不参加锁的读者不会读到写了一半的文件。
//
// 基准表回答「我读到的是哪一版」，锁回答「此刻有没有别人在写」——前者不互斥任何东西。
// 锁是 advisory 的：bash（rm / sed -i / > 重定向）完全绕过 → 不守规矩的写者由 doc/41 的人审兜。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// fingerprintHashMaxBytes 是内容哈希的降级阈值：超过这个大小只比 (mtime, size)，不算 sha256。
//
// 为什么阈值这么高而不是 1MB（2026-10-22 用户纠正的一条）：`cp -p` / `rsync -a` / `tar -x` /
// `git checkout` **保留 mtime** → 内容变了、mtime 没变、size 又相同 → 完全检测不到；NFS 的 mtime
// 粒度常常是 1 秒 → 同一秒内的两次写 + 同大小也检测不到；而「改一个数字/名字/路径」天然同大小。
// 这三条恰好命中 tako 的真实场景（共享盘 + bash 乱改 + 多副本）→ 弱判据的适用范围必须压到极窄。
const fingerprintHashMaxBytes = 64 << 20

// lockWaitInterval / lockTimeout 是取锁的轮询节奏与上限。flock 没有内建超时，阻塞式 flock
// 还会占住一个 OS 线程 → 用 LOCK_NB 轮询，超时给模型一句「正被另一个 agent 写，稍后重试」。
const (
	lockWaitInterval = 20 * time.Millisecond
	lockTimeout      = 30 * time.Second
)

// errLockTimeout 是拿不到写锁的哨兵错误（文案在工具层给模型）。
var errLockTimeout = errors.New("file lock timeout")

// Fingerprint 是一个文件此刻的样子。
//
// 判据是 sha256（内容级）：sha 相同 = 内容同一版 = 放行（哪怕 mtime 变了，比如有人 touch 了一下）。
// mtime/size 只在大文件降级时用（>64MB 不算 sha）。**不用「mtime 变没变」当判据**——那不是内容判据。
type Fingerprint struct {
	Mtime  time.Time `json:"mtime"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"` // 空 = 降级（弱判据），见 weak()
}

// weak 报「这次是弱判据」（文件太大，只比了 mtime+size）。文案与日志要如实标出来，不装懂。
func (fp Fingerprint) weak() bool { return fp.SHA256 == "" }

// equal 比两个指纹：两边都有内容哈希就比哈希（内容同一版）；有一边没有（>64MB 降级）就比
// (mtime, size)。**内容相同就算同一版**——所以「文件被 touch 了一下但内容没变」不会误判成冲突。
func (fp Fingerprint) equal(other Fingerprint) bool {
	if fp.SHA256 != "" && other.SHA256 != "" {
		return fp.SHA256 == other.SHA256
	}
	return fp.Mtime.Equal(other.Mtime) && fp.Size == other.Size
}

// fingerprintOf 算一个路径此刻的指纹。文件不存在 → 返回错误（调用方据此判「不存在」）。
//
// 大文件只做 stat（不读内容）→ 弱判据；小文件流式算 sha256（内存恒定，不把整个文件读进内存）。
func fingerprintOf(path string) (Fingerprint, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Fingerprint{}, err
	}
	fp := Fingerprint{Mtime: fi.ModTime(), Size: fi.Size()}
	if fi.Size() > fingerprintHashMaxBytes {
		return fp, nil // 降级：弱判据
	}
	f, err := os.Open(path)
	if err != nil {
		return fp, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fp, err
	}
	fp.SHA256 = hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

// fingerprintOfBytes 从**已经在内存里**的字节算指纹（read_file 路径：内容本来就要读，hash 顺手）。
// fi 必须是同一次打开的 Stat 结果（mtime/size 与内容同源）。
func fingerprintOfBytes(fi os.FileInfo, raw []byte) Fingerprint {
	fp := Fingerprint{Mtime: fi.ModTime(), Size: fi.Size()}
	if fi.Size() > fingerprintHashMaxBytes {
		return fp // 降级：弱判据
	}
	sum := sha256.Sum256(raw)
	fp.SHA256 = hex.EncodeToString(sum[:])
	return fp
}

// ── 基准表 ────────────────────────────────────────────────────────────────

// fileBaseline 记「谁读到的是哪一版」。
//
// 键是 (agent 名, 绝对路径)：主代理与子代理各记各的——子代理不能借用主代理的「读过」，否则它凭
// 自己的记忆整份写，会抹掉主代理读之后的改动。agent 名取 callerNameFrom（root 挂空串，
// 子代理挂规格名，见 websearch.go 的 callerStampMiddleware）。
//
// 生命周期 = 一场 run：run 崩了表就没了，下一场重新读，无所谓 → **不落库、不迁移、无清理负担、
// 零额外可用性依赖**（不像 Redis 化会引入「Redis 挂了写就不可用」）。
type fileBaseline struct {
	mu sync.Mutex
	m  map[string]map[string]Fingerprint // agent → 绝对路径 → 指纹
}

func newFileBaseline() *fileBaseline {
	return &fileBaseline{m: map[string]map[string]Fingerprint{}}
}

// get 查某个 agent 对某个路径的基准指纹；没有这一行 = 「这个 agent 从没读过它」。
func (b *fileBaseline) get(agent, path string) (Fingerprint, bool) {
	if b == nil {
		return Fingerprint{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	fp, ok := b.m[agent][path]
	return fp, ok
}

// set 记一笔基准。read_file 成功后、写成功后各调一次（写后刷新 = 连续改同一文件不误报）。
func (b *fileBaseline) set(agent, path string, fp Fingerprint) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	byAgent, ok := b.m[agent]
	if !ok {
		byAgent = map[string]Fingerprint{}
		b.m[agent] = byAgent
	}
	byAgent[path] = fp
}

type fileBaselineKey struct{}

// withFileBaseline 把基准表挂进 ctx（run 级，worker.run 装配点）。
func withFileBaseline(ctx context.Context, b *fileBaseline) context.Context {
	return context.WithValue(ctx, fileBaselineKey{}, b)
}

// fileBaselineFrom 取 ctx 上的基准表；没有（单测、旧调用方）→ nil（读写的判据退化为「查不到即未读过」）。
func fileBaselineFrom(ctx context.Context) *fileBaseline {
	b, _ := ctx.Value(fileBaselineKey{}).(*fileBaseline)
	return b
}

// agentNameOf 取当前调用者身份：root = ""，子代理 = 规格名。
func agentNameOf(ctx context.Context) string {
	if name, ok := callerNameFrom(ctx); ok {
		return name
	}
	return ""
}

// ── 写锁 ──────────────────────────────────────────────────────────────────

// lockDirFor 算锁目录：租户根下的 .tako/locks。
//
// 放租户根而不是工作区根：同一租户的所有会话/项目共用一把锁目录；放工作区里会被 agent 看见
// （工作区是 agent 的家目录，多出来的目录会被 list_directory 列出来）。锁目录在 tenants/<tid>/ 下，
// agent 的根是 tenants/<tid>/{workspaces,sessions}/… → agent 看不见它。
func lockDirFor(ctx context.Context, root string) string {
	if ref, ok := workspaceFrom(ctx); ok && ref.TenantRoot != "" {
		return filepath.Join(ref.TenantRoot, ".tako", "locks")
	}
	return filepath.Join(root, ".tako", "locks")
}

// lockBuckets 是锁文件的桶数。锁键 = sha256(路径) 首字节 % 桶数 → **锁文件数恒 ≤ 桶数**。
//
// 为什么不「一个路径一个锁文件」：一个项目写几千个文件就会在锁目录堆几千个空文件
// （ext4 的大目录会退化、e2fsck 会告警），而**清理又不能随便做**——unlink 一个正被持有的锁文件，
// 后来者会 open 到**新 inode** → 两边同时以为自己独占了 → 互斥失效；要清只能在租户删除时连目录清，
// 那又太迟。桶锁把目录大小钉死在常数级，两个问题一起消失。
//
// 代价：不同路径可能撞同一桶（1/256）→ 假串行（多等一会儿），无正确性问题。
const lockBuckets = 256

// lockPathFor 算某个绝对路径对应的锁文件：桶号 + ".lock"。
//
// 用哈希不用路径原文：路径里有分隔符、可能中文、可能超长。锁**文件**而不是目标文件：目标可能
// 还不存在（新建场景没法锁）；且 flock 锁的是 inode——目标文件被删掉又重建 → inode 变 → 锁失效，
// 而锁文件我们从不删 → 锁稳定。
func lockPathFor(lockDir, absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	return filepath.Join(lockDir, fmt.Sprintf("%02x.lock", int(sum[0])%lockBuckets))
}

// lockPaths 取这些路径的排它写锁（粒度=文件），返回释放函数。
//
// 这是 lockPathsCtx 的无 ctx 包装：给不需要取消的调用方用（既有单测、dev 端点）。
// 需要在等待期间听取消的走 lockPathsCtx。
func lockPaths(lockDir string, paths []string) (func(), error) {
	return lockPathsCtx(context.Background(), lockDir, paths)
}

// lockPathsCtx 同 lockPaths，但等待期间听 ctx 的取消（run 被停 → 立刻放掉排队）。
//
// 规则（三条，缺一就死锁）：
//  1. 路径**去重**（同一把锁不取两次）——flock 挂在 open file description 上，同一进程两次 open
//     同一文件是两个不同的 open file description → 第二次会阻塞在**自己**的第一次上。
//  2. 路径**按字典序**取（防 AB-BA 死锁）：A 拿 L1 再 L2、B 拿 L2 再 L1 → 互等。
//  3. 逐把取，任一把失败就把已拿到的**全部**释放再返回错误。
//
// 拿不到锁的两种结局：超时（30s）→ errLockTimeout（可重试）；其余错误 → 原样返回。
// flock 是 advisory 的：bash 不受这把锁约束（它不调 flock）→ bash 那层由 doc/41 的人审兜。
// 跨副本互斥不靠它（靠 filelock.go 的 PG advisory lock）——flock 挂在本机 inode 上，
// 别的机器看不见。
func lockPathsCtx(ctx context.Context, lockDir string, paths []string) (func(), error) {
	uniq := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	if len(uniq) == 0 {
		return func() {}, nil
	}
	sort.Strings(uniq) // 字典序：防 AB-BA 死锁

	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("建锁目录失败：%v", err)
	}

	type held struct {
		fd  *os.File
		pth string
	}
	var locks []held
	release := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(locks[i].fd.Fd()), syscall.LOCK_UN)
			_ = locks[i].fd.Close()
		}
		locks = nil
	}

	for _, p := range uniq {
		fd, err := os.OpenFile(lockPathFor(lockDir, p), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			release()
			return nil, fmt.Errorf("开锁文件失败：%v", err)
		}
		deadline := time.Now().Add(lockTimeout)
		for {
			err = syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				_ = fd.Close()
				release()
				return nil, fmt.Errorf("取写锁失败：%v", err)
			}
			if time.Now().After(deadline) {
				_ = fd.Close()
				release()
				return nil, errLockTimeout
			}
			select {
			case <-ctx.Done():
				_ = fd.Close()
				release()
				return nil, ctx.Err()
			case <-time.After(lockWaitInterval):
			}
		}
		locks = append(locks, held{fd: fd, pth: p})
	}
	return release, nil
}
