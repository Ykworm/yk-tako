// filelock_test.go 是「合作写者互斥」的钉子网（方案真源 = doc/71 §4/§5；施工包 §9.1）。
//
// 钉的性质：
//  1. **同键排队**：两个写者（两个 Store = 两个连接池 = 两条 PG 会话）拿同一把键 → 第二个等；
//  2. **不同键并行**：不同路径不被串成一把全局锁；
//  3. **租户隔离**：不同租户的同一路径 → 两把不同的键，不互相等；
//  4. **等待有上限**：超上限返回**可重试**错误（errLockTimeout），默认上限 30 秒；
//  5. **持锁者死了锁能再取到**：PG 后端被杀 / 持锁进程被 kill -9 → 锁自动释放；
//  6. **顺序固定**：PG 锁 → flock 锁，不出现反向取锁；
//  7. 工具层：write_file 基于旧版本被拒、edit_file 保留他人改动（在多副本锁下）。
//
// 断言以真实锁行为、真实文件字节、真实错误返回为准。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// secondStoreOn 在同一个 PG 库上再开一个 Store —— 这是「另一个副本」的替身：
// 独立的连接池 → 独立的 PG 会话 → 会话级 advisory lock 的语义在这里才成立。
func secondStoreOn(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), testRedisURL, dsn, testTTL, testLease)
	if err != nil {
		t.Fatalf("开第二个 Store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// lockSubject 造一个普通文件的逻辑键（普通文件 = 规范化绝对路径）。
func lockSubject(name string) string { return fileLockSubject(filepath.Join("/shared", name)) }

// 两个副本拿同一把键 → 第二个必须等第一个放。
func TestPGLockSerializesSameKeyAcrossReplicas(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()
	subject := lockSubject("same.txt")

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{subject})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		relB, err := newPGFileLocker(storeB.db, 0).Lock(ctx, []string{subject})
		if err == nil {
			relB()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("A 持锁时 B 不该拿到锁（B 拿到了: err=%v）", err)
	case <-time.After(700 * time.Millisecond):
	}
	relA()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("A 放锁后 B 应拿到锁: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("A 放锁后 B 一直没拿到锁")
	}
}

// 不同键 → 不互相等（别把不同路径也串成一把全局锁）。
func TestPGLockDifferentKeysRunInParallel(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{lockSubject("a.txt")})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	defer relA()
	// B 拿另一把键：必须立刻拿到，不等 A。
	relB, err := newPGFileLocker(storeB.db, 0).Lock(ctx, []string{lockSubject("b.txt")})
	if err != nil {
		t.Fatalf("不同键不该互相阻塞: %v", err)
	}
	relB()
}

// 租户隔离：不同租户的同一路径 → 两把不同的 PG 键（PG 是全局的，租户必须在键里）。
func TestPGLockDifferentTenantsDoNotBlock(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctxA := withWorkspace(context.Background(), WorkspaceRef{TenantID: "tnt_a", Root: "/tmp/a"})
	ctxB := withWorkspace(context.Background(), WorkspaceRef{TenantID: "tnt_b", Root: "/tmp/b"})
	subject := lockSubject("shared.txt")

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctxA, []string{subject})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	defer relA()
	relB, err := newPGFileLocker(storeB.db, 0).Lock(ctxB, []string{subject})
	if err != nil {
		t.Fatalf("不同租户不该互相阻塞: %v", err)
	}
	relB()

	// 同一租户、同一路径 → 必须互斥（证明租户确实进了键里，不是碰巧不撞）。
	// 注意：这里用**另一个 Store**（另一条 PG 会话）去拿同一把键——会话级 advisory lock
	// 在同一会话里是可重入的（同一条连接再取一次会直接成功），跨会话才谈得上互斥。
	short := newPGFileLocker(storeB.db, 400*time.Millisecond)
	rel, err := short.Lock(ctxA, []string{subject})
	if err == nil {
		rel()
		t.Fatalf("同租户同路径必须互斥，B 却拿到了锁")
	}
	if !errors.Is(err, errLockTimeout) {
		t.Fatalf("同租户同路径应超时, got %v", err)
	}
}

// 等待有上限：超上限返回**可重试**错误（模型能知道可以再试）。
func TestPGLockTimeoutIsRetryable(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()
	subject := lockSubject("busy.txt")

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{subject})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	defer relA()
	start := time.Now()
	_, err = newPGFileLocker(storeB.db, 400*time.Millisecond).Lock(ctx, []string{subject})
	elapsed := time.Since(start)
	if !errors.Is(err, errLockTimeout) {
		t.Fatalf("超时应返回 errLockTimeout, got %v", err)
	}
	if elapsed < 350*time.Millisecond {
		t.Fatalf("等待 %v 就返回了，没到上限", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("等待 %v 太久，上限没生效", elapsed)
	}
}

// 上限口径 = 30 秒（沿用 lockWaitInterval / lockTimeout）。
func TestPGLockDefaultTimeoutIs30Seconds(t *testing.T) {
	store, _ := newTestStoreWithDSN(t, testLease, testTTL)
	if lockTimeout != 30*time.Second {
		t.Fatalf("lockTimeout = %v, want 30s", lockTimeout)
	}
	l := newPGFileLocker(store.db, 0)
	if l.timeout != 30*time.Second {
		t.Fatalf("PG 写锁默认上限 %v, want 30s", l.timeout)
	}
	if l.interval != lockWaitInterval {
		t.Fatalf("PG 写锁轮询节奏 %v, want %v", l.interval, lockWaitInterval)
	}
}

// 真实 30 秒上限：等满上限后返回**可重试**错误——不是无限等，也不是「这次失败了」。
// （这条会真的等 30 秒：它钉的是「上限真的存在」这件事本身，短上限的版本钉不住。）
func TestPGLockRealTimeoutIsRetryable(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()
	subject := lockSubject("realtimeout.txt")

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{subject})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	defer relA()
	start := time.Now()
	_, err = newPGFileLocker(storeB.db, 0).Lock(ctx, []string{subject}) // 默认上限 = 30s
	elapsed := time.Since(start)
	if !errors.Is(err, errLockTimeout) {
		t.Fatalf("等满上限应返回 errLockTimeout, got %v", err)
	}
	if elapsed < 29*time.Second {
		t.Fatalf("等待 %v 就返回了，没到 30 秒上限", elapsed)
	}
	if elapsed > 45*time.Second {
		t.Fatalf("等待 %v 太久，上限没生效", elapsed)
	}
}

// 持锁的 PG 后端被杀 → 锁自动释放（会话结束 → PG 释放该会话的 advisory lock）。
func TestPGLockReleasedWhenBackendDies(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()
	subject := lockSubject("dies.txt")

	relA, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{subject})
	if err != nil {
		t.Fatalf("A 取锁失败: %v", err)
	}
	// 找到持有 advisory 锁的那个 PG 后端。
	var pid int
	if err := storeA.db.Raw(
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted LIMIT 1`).Scan(&pid).Error; err != nil {
		t.Fatalf("查 advisory 锁持有者: %v", err)
	}
	if pid == 0 {
		t.Fatalf("没找到持有 advisory 锁的 backend")
	}
	// 杀掉它（等价于那个副本的连接断了 / 进程崩了）。
	if err := storeB.db.Exec(`SELECT pg_terminate_backend(?)`, pid).Error; err != nil {
		t.Fatalf("pg_terminate_backend: %v", err)
	}
	// 锁应当能再被取到。
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		rel, err := newPGFileLocker(storeB.db, 500*time.Millisecond).Lock(ctx, []string{subject})
		if err == nil {
			rel()
			break
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("backend 死了但锁没释放: %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 死掉那条连接的释放不能炸（连接已死 → 解锁失败 → 丢弃连接）。
	relA()
}

// 持锁进程被 kill -9 → 它的 PG 会话随之结束 → 锁自动释放。
func TestPGLockReleasedWhenHolderProcessDies(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	ctx := context.Background()
	subject := lockSubject("killed.txt")
	ready := filepath.Join(t.TempDir(), "ready")

	cmd := exec.Command(os.Args[0], "-test.run=TestPGFileLockHolderHelper")
	cmd.Env = append(os.Environ(),
		"TAKO_TEST_PG_DSN="+dsn,
		"TAKO_TEST_LOCK_SUBJECT="+subject,
		"TAKO_TEST_LOCK_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("持锁子进程没起来")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 子进程活着 → 拿不到锁。
	if rel, err := newPGFileLocker(storeA.db, 500*time.Millisecond).Lock(ctx, []string{subject}); err == nil {
		rel()
		t.Fatalf("子进程持锁期间不该拿到锁")
	}
	// kill -9：子进程不释放、也不解锁——靠它的会话结束让 PG 放锁。
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("杀持锁进程失败: %v", err)
	}
	_, _ = cmd.Process.Wait()

	deadline = time.Now().Add(20 * time.Second)
	for {
		rel, err := newPGFileLocker(storeA.db, 500*time.Millisecond).Lock(ctx, []string{subject})
		if err == nil {
			rel()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("持锁进程死了但锁没释放: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestPGFileLockHolderHelper 是上面那个测试的子进程侧：拿锁 → 落 ready → 挂着等被强杀。
func TestPGFileLockHolderHelper(t *testing.T) {
	dsn := os.Getenv("TAKO_TEST_PG_DSN")
	subject := os.Getenv("TAKO_TEST_LOCK_SUBJECT")
	ready := os.Getenv("TAKO_TEST_LOCK_READY")
	if dsn == "" || subject == "" || ready == "" {
		t.Skip("不是 helper 进程")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		os.Exit(2)
	}
	rel, err := newPGFileLocker(db, 0).Lock(context.Background(), []string{subject})
	if err != nil {
		os.Exit(3)
	}
	_ = rel
	if err := os.WriteFile(ready, []byte("ok"), 0o644); err != nil {
		os.Exit(4)
	}
	// 模拟崩溃：等父进程验完「锁确实被占着」，再被强杀 → 锁由 PG 随会话结束释放。
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// 同进程两个 goroutine 拿同一把键 → 也必须排队。
//
// 这条不是「顺手加的」：PG 会话级 advisory lock 在**同一会话**里是可重入的
// （同键再取一次直接成功、计数 +1）。如果两个 goroutine 拿到的是同一条连接
// （同一个 PG 会话），第二个会立刻「拿到锁」→ 互斥形同虚设。
// 这条测试同时钉住「池子给并发请求不同的连接」。
func TestPGLockSameProcessSerializes(t *testing.T) {
	store, _ := newTestStoreWithDSN(t, testLease, testTTL)
	ctx := context.Background()
	subject := lockSubject("sameproc.txt")

	rel, err := newPGFileLocker(store.db, 0).Lock(ctx, []string{subject})
	if err != nil {
		t.Fatalf("取锁失败: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		rel2, err := newPGFileLocker(store.db, 0).Lock(ctx, []string{subject})
		if err == nil {
			rel2()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("同进程第二个写者不该拿到锁（拿到了: %v）", err)
	case <-time.After(700 * time.Millisecond):
	}
	rel()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("放锁后第二个写者应拿到锁: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("放锁后第二个写者一直没拿到锁")
	}
}

// recordingLocker 记下取锁/放锁的先后，用来钉「PG → flock」的固定顺序。
type recordingLocker struct {
	name string
	mu   *sync.Mutex
	seq  *[]string
}

func (r recordingLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	r.mu.Lock()
	*r.seq = append(*r.seq, r.name)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		*r.seq = append(*r.seq, r.name+"-release")
		r.mu.Unlock()
	}, nil
}

// 两层锁的顺序固定：先 PG 再 flock；释放反过来（后取的先放）。
func TestChainLockerTakesPGThenFlock(t *testing.T) {
	var mu sync.Mutex
	var seq []string
	chain := newChainFileLocker(
		recordingLocker{name: "pg", mu: &mu, seq: &seq},
		recordingLocker{name: "flock", mu: &mu, seq: &seq},
	)
	rel, err := chain.Lock(context.Background(), []string{lockSubject("x.txt")})
	if err != nil {
		t.Fatalf("取锁失败: %v", err)
	}
	rel()
	mu.Lock()
	got := strings.Join(seq, ",")
	mu.Unlock()
	if want := "pg,flock,flock-release,pg-release"; got != want {
		t.Fatalf("取放锁顺序 %q, want %q", got, want)
	}
}

// 逻辑键规范化：同一个文件的两种写法必须落到同一把键（否则互斥失效）。
func TestFileLockSubjectNormalizesPaths(t *testing.T) {
	base := "/ws/notes"
	a := fileLockSubject(filepath.Join(base, "a.txt"))
	b := fileLockSubject(filepath.Join(base, "./a.txt"))
	c := fileLockSubject(filepath.Join(base, "b", "..", "a.txt"))
	if a != b || a != c {
		t.Fatalf("同一文件的几种写法应归一到同一键: %q %q %q", a, b, c)
	}
	d := fileLockSubject(filepath.Join(base, "b.txt"))
	if a == d {
		t.Fatalf("不同文件不该同键")
	}
}

// write_file 在多副本锁下：基于旧版本仍被拒（锁不替代基准表）。
func TestWriteFileRejectsStaleBaselineUnderMultiReplicaLock(t *testing.T) {
	store, _ := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	ctx = WithFileLocker(ctx, newChainFileLocker(
		newPGFileLocker(store.db, 0),
		newFlockFileLocker(root),
	))
	readToolFor(t, root).InvokableRun(ctx, `{"path":"a.txt"}`) // 建基准
	// 另一个写者（bash / 别的副本）改了内容
	if err := os.WriteFile(p, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := writeToolFor(t, root)
	_, err := it.InvokableRun(ctx, `{"path":"a.txt","content":"new"}`)
	if err == nil || !strings.Contains(err.Error(), "跟你读到的不一样") {
		t.Fatalf("版本过期应被拒, got err=%v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "other" {
		t.Fatalf("磁盘被旧版覆盖了：%q", got)
	}
}

// edit_file 在多副本锁下：别人改的别处要保留（结果基于最新内容算）。
func TestEditFileKeepsForeignChangeUnderMultiReplicaLock(t *testing.T) {
	store, _ := newTestStoreWithDSN(t, testLease, testTTL)
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%02d", i)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	ctx = WithFileLocker(ctx, newChainFileLocker(
		newPGFileLocker(store.db, 0),
		newFlockFileLocker(root),
	))
	readToolFor(t, root).InvokableRun(ctx, `{"path":"f.txt"}`)

	// 别人改了第 5 行
	foreign := append([]string(nil), lines...)
	foreign[5] = "line05-CHANGED-BY-B"
	if err := os.WriteFile(p, []byte(strings.Join(foreign, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	rawEdit, err := newEditFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := rawEdit.(tool.InvokableTool)
	out, err := it.InvokableRun(ctx, `{"path":"f.txt","old_text":"line20","new_text":"line20-BY-A"}`)
	if err != nil {
		t.Fatalf("edit_file 报错: %v", err)
	}
	if want := "已编辑 f.txt（替换 1 处）"; out != want {
		t.Fatalf("edit_file 返回 %q, want %q", out, want)
	}
	got, _ := os.ReadFile(p)
	gotLines := strings.Split(string(got), "\n")
	if gotLines[5] != "line05-CHANGED-BY-B" {
		t.Fatalf("别人的改动被抹掉了：%q", gotLines[5])
	}
	if gotLines[20] != "line20-BY-A" {
		t.Fatalf("本次改动没落上：%q", gotLines[20])
	}
}

// 多路径取锁：去重 + 按字典序（防 AB-BA 死锁）。
func TestPGLockDeduplicatesAndSortsKeys(t *testing.T) {
	got := dedupSortedKeys([]string{"/b", "/a", "/b", "", "/c"})
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("去重排序后 %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去重排序后 %v, want %v", got, want)
		}
	}
	if n := len(dedupSortedKeys(nil)); n != 0 {
		t.Fatalf("空列表应返回空, got %d", n)
	}
}

// tracedLocker 包住真锁，记录进入/退出的先后——用来证明两个副本的临界区**不交叠**。
type tracedLocker struct {
	inner FileLocker
	name  string
	mu    *sync.Mutex
	seq   *[]string
}

func (t tracedLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	rel, err := t.inner.Lock(ctx, keys)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	*t.seq = append(*t.seq, "enter-"+t.name)
	t.mu.Unlock()
	return func() {
		rel()
		t.mu.Lock()
		*t.seq = append(*t.seq, "exit-"+t.name)
		t.mu.Unlock()
	}, nil
}

// 两个副本写同一路径 → 临界区不交叠（enter-exit-enter-exit，不能 enter-enter）。
func TestWritersAcrossReplicasDoNotOverlap(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()
	subject := lockSubject("overlap.txt")

	var mu sync.Mutex
	var seq []string
	lockerA := tracedLocker{inner: newPGFileLocker(storeA.db, 0), name: "A", mu: &mu, seq: &seq}
	lockerB := tracedLocker{inner: newPGFileLocker(storeB.db, 0), name: "B", mu: &mu, seq: &seq}

	var wg sync.WaitGroup
	for _, l := range []FileLocker{lockerA, lockerB} {
		wg.Add(1)
		go func(l FileLocker) {
			defer wg.Done()
			rel, err := l.Lock(ctx, []string{subject})
			if err != nil {
				t.Errorf("取锁失败: %v", err)
				return
			}
			time.Sleep(150 * time.Millisecond)
			rel()
		}(l)
	}
	wg.Wait()
	mu.Lock()
	got := strings.Join(seq, ",")
	mu.Unlock()
	if got != "enter-A,exit-A,enter-B,exit-B" && got != "enter-B,exit-B,enter-A,exit-A" {
		t.Fatalf("两个写者的临界区交叠了：%q", got)
	}
}

// 逻辑键不必是路径：markdown 记忆模式会传「tenant+user」「tenant+workspace」这类键
// （后续阶段的用法）。这里钉住「同键互斥、不同键不互斥」在非路径键上同样成立。
func TestPGLockAcceptsNonPathLogicalKeys(t *testing.T) {
	storeA, dsn := newTestStoreWithDSN(t, testLease, testTTL)
	storeB := secondStoreOn(t, dsn)
	ctx := context.Background()

	// 用户文件键（租户由 ctx 提供，键里只剩 user 标识）。
	userKey := "user:alice"
	rel, err := newPGFileLocker(storeA.db, 0).Lock(ctx, []string{userKey})
	if err != nil {
		t.Fatalf("取用户键失败: %v", err)
	}
	// 另一个用户 / 另一个项目 → 不该被这把锁挡住。
	for _, other := range []string{"user:bob", "workspace:proj-1"} {
		rel2, err := newPGFileLocker(storeB.db, 0).Lock(ctx, []string{other})
		if err != nil {
			t.Fatalf("不同逻辑键不该互相阻塞(%s): %v", other, err)
		}
		rel2()
	}
	// 同一个用户键 → 必须互斥。
	if rel2, err := newPGFileLocker(storeB.db, 400*time.Millisecond).Lock(ctx, []string{userKey}); err == nil {
		rel2()
		t.Fatalf("同一逻辑键必须互斥，却拿到了锁")
	} else if !errors.Is(err, errLockTimeout) {
		t.Fatalf("同一逻辑键应超时, got %v", err)
	}
	rel()
}

// ctx 上没有写锁提供者 → 退化成现有 flock 行为（单测与旧调用方不炸）。
func TestLockFilePathsFallsBackToFlock(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	ctx := context.Background()
	if fileLockerFrom(ctx) != nil {
		t.Fatalf("裸 ctx 不该有写锁提供者")
	}
	rel, err := lockFilePaths(ctx, root, []string{target})
	if err != nil {
		t.Fatalf("退化的 flock 取锁失败: %v", err)
	}
	rel()
}

// recordingKeysLocker 记下它拿到的逻辑键，用来钉「哪些路径进了锁」。
type recordingKeysLocker struct {
	mu   *sync.Mutex
	keys *[]string
}

func (r recordingKeysLocker) Lock(ctx context.Context, keys []string) (func(), error) {
	r.mu.Lock()
	*r.keys = append(*r.keys, keys...)
	r.mu.Unlock()
	return func() {}, nil
}

// move_file：源与目标**都**进多副本锁（漏掉目标 = 别人在目标路径上写时我们照样挪过去）。
func TestMoveFileLocksBothSourceAndTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	ctx := WithFileLocker(context.Background(), recordingKeysLocker{mu: &mu, keys: &got})
	raw, err := newMoveFileTool(root)
	if err != nil {
		t.Fatal(err)
	}
	it := raw.(tool.InvokableTool)
	out, err := it.InvokableRun(ctx, `{"path":"a.txt","destination":"sub/b.txt"}`)
	if err != nil || out != "已移动 a.txt → sub/b.txt" {
		t.Fatalf("move_file: out=%q err=%v", out, err)
	}
	mu.Lock()
	keys := append([]string(nil), got...)
	mu.Unlock()
	wantSrc := fileLockSubject(filepath.Join(root, "a.txt"))
	wantDst := fileLockSubject(filepath.Join(root, "sub", "b.txt"))
	if len(keys) != 2 {
		t.Fatalf("进了锁的路径 %d 个, want 2: %v", len(keys), keys)
	}
	for _, want := range []string{wantSrc, wantDst} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("锁键 %v 里缺 %q（源与目标都要进锁）", keys, want)
		}
	}
}

// write_file 走原子替换：保留原权限、目标目录不留临时文件。
func TestWriteFileAtomicReplaceKeepsPermissionsAndLeavesNoTemp(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := withFileBaseline(context.Background(), newFileBaseline())
	readToolFor(t, root).InvokableRun(ctx, `{"path":"a.txt"}`)
	it := writeToolFor(t, root)
	if _, err := it.InvokableRun(ctx, `{"path":"a.txt","content":"new"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("覆盖写后权限 %o, want 600（原子替换要保留原权限）", got)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new" {
		t.Fatalf("磁盘内容 %q, want %q", got, "new")
	}
	if temps := tempFilesIn(t, root); len(temps) != 0 {
		t.Fatalf("目标目录留了临时文件：%v", temps)
	}
}
