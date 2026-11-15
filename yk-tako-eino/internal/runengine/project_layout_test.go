package runengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 新项目（容器不存在或为空）第一次使用时原地建布局：content/runtime 两棵树 + layout.json。
func TestEnsureProjectLayoutInitializesNewProject(t *testing.T) {
	root := t.TempDir()
	content, runtimeDir, err := ensureProjectLayout(root, "t1", "ws_new")
	if err != nil {
		t.Fatal(err)
	}
	container, err := projectContainerDir(root, "t1", "ws_new")
	if err != nil {
		t.Fatal(err)
	}
	if content != filepath.Join(container, projectContentDirName) || runtimeDir != filepath.Join(container, projectRuntimeDirName) {
		t.Fatalf("目录不符：content=%s runtime=%s", content, runtimeDir)
	}
	if _, err := os.Stat(projectLayoutPath(container)); err != nil {
		t.Fatalf("layout.json 没写：%v", err)
	}
	for _, d := range []string{content, runtimeDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("目录没建：%s (%v)", d, err)
		}
	}
	needs, err := projectLayoutNeedsMigration(root, "t1", "ws_new")
	if err != nil || needs {
		t.Fatalf("新布局不该要求迁移：needs=%v err=%v", needs, err)
	}
	// 幂等：第二次仍是同一对目录，不报错。
	if c2, r2, err := ensureProjectLayout(root, "t1", "ws_new"); err != nil || c2 != content || r2 != runtimeDir {
		t.Fatalf("第二次 ensure 不一致：%q %q %v", c2, r2, err)
	}
}

// 存量项目（容器已有条目、没有 layout.json）一律要求显式迁移，不猜、不双读。
func TestEnsureProjectLayoutRejectsLegacyContainer(t *testing.T) {
	root := t.TempDir()
	container, err := projectContainerDir(root, "t1", "ws_old")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(container, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, ".media", "a.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureProjectLayout(root, "t1", "ws_old"); !errors.Is(err, errProjectLayoutUnmigrated) {
		t.Fatalf("存量容器应被拒：%v", err)
	}
	needs, err := projectLayoutNeedsMigration(root, "t1", "ws_old")
	if err != nil || !needs {
		t.Fatalf("应判为需要迁移：needs=%v err=%v", needs, err)
	}
	// 只读判断不能建出空 content。
	if _, err := os.Stat(filepath.Join(container, projectContentDirName)); !os.IsNotExist(err) {
		t.Fatalf("未迁移项目不该被建出 content：%v", err)
	}
}

// 空容器不是存量：没有用户字节，原地初始化安全。
func TestEnsureProjectLayoutEmptyContainerIsNew(t *testing.T) {
	root := t.TempDir()
	container, err := projectContainerDir(root, "t1", "ws_empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureProjectLayout(root, "t1", "ws_empty"); err != nil {
		t.Fatalf("空容器应可初始化：%v", err)
	}
}

// 新工具回执的 runtime/ 前缀与历史 .tako 输出都映射到项目运行资源区。
func TestResolveRuntimeReadPath(t *testing.T) {
	runtimeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "tmp", "bash", "r_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeRoot, "tmp", "bash", "r_1", "1.out"), []byte("full"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := WorkspaceRef{RuntimeRoot: runtimeRoot}

	full, matched, err := resolveRuntimeReadPath(context.Background(), ref, "runtime/tmp/bash/r_1/1.out")
	if err != nil || !matched || full != filepath.Join(runtimeRoot, "tmp", "bash", "r_1", "1.out") {
		t.Fatalf("runtime 前缀映射不符：%q matched=%v err=%v", full, matched, err)
	}
	full, matched, err = resolveRuntimeReadPath(context.Background(), ref, ".tako/bash/r_1/1.out")
	if err != nil || !matched || full != filepath.Join(runtimeRoot, "tmp", "bash", "r_1", "1.out") {
		t.Fatalf("旧 .tako 路径映射不符：%q matched=%v err=%v", full, matched, err)
	}
	if _, matched, _ := resolveRuntimeReadPath(context.Background(), ref, "notes/a.txt"); matched {
		t.Fatal("普通内容路径不该被 runtime 分支吃掉")
	}
	// 带 .. 的路径 Clean 后不再有 runtime/ 前缀，落回普通内容解析（由 resolveInWorkspace 拒绝越界）。
	if _, matched, _ := resolveRuntimeReadPath(context.Background(), ref, "runtime/../outside"); matched {
		t.Fatal("runtime/../x 不该被当成 runtime 分支")
	}
}

// 入口图物理落 runtime/media；占位 path 里的 .media/ 读回同一物理目录，其余路径读 content。
func TestMediaPlaceholderReadsRuntimeMedia(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	root := t.TempDir()
	ws, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "媒体项目")
	if err != nil {
		t.Fatal(err)
	}
	content, runtimeDir, err := ensureProjectLayout(root, testOwner.TenantID, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	mediaFile := filepath.Join(runtimeDir, projectMediaDirName, "a.png")
	if err := os.MkdirAll(filepath.Dir(mediaFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaFile, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCtx := withWorkspace(withPrincipal(ctx, testOwner), WorkspaceRef{
		TenantID: testOwner.TenantID, Root: content, RuntimeRoot: runtimeDir,
		TenantRoot: root, WorkspaceID: ws.ID,
	})
	got, err := resolveReadMediaPath(runCtx, s, root, root, "workspaces/"+ws.ID+"/.media/a.png")
	if err != nil || got != mediaFile {
		t.Fatalf(".media 占位应读 runtime/media：%q %v", got, err)
	}
	got, err = resolveReadMediaPath(runCtx, s, root, root, "workspaces/"+ws.ID+"/notes.txt")
	if err != nil || got != filepath.Join(content, "notes.txt") {
		t.Fatalf("正文占位应读 content：%q %v", got, err)
	}
	// 存量布局（容器有条目、无 layout.json）在显式迁移前拒绝，不给只读旁路。
	legacy, err := s.CreateWorkspace(ctx, testOwner.TenantID, testOwner.UserID, "存量项目")
	if err != nil {
		t.Fatal(err)
	}
	container, err := projectContainerDir(root, testOwner.TenantID, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(container, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, ".media", "b.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyCtx := withWorkspace(withPrincipal(ctx, testOwner), WorkspaceRef{
		TenantID: testOwner.TenantID, Root: root, TenantRoot: root, WorkspaceID: legacy.ID,
	})
	if _, err := resolveReadMediaPath(legacyCtx, s, root, root, "workspaces/"+legacy.ID+"/.media/b.png"); err == nil {
		t.Fatal("存量布局的项目在迁移前应拒绝读取")
	}
}

// runtimeOutputDir：有运行资源根走 runtime/tmp/<kind>，纯会话退回旧 .tako/<kind>。
func TestRuntimeOutputDir(t *testing.T) {
	fallback := t.TempDir()
	runtimeRoot := t.TempDir()
	ctx := withWorkspace(context.Background(), WorkspaceRef{Root: fallback, RuntimeRoot: runtimeRoot})
	phys, rel := runtimeOutputDir(ctx, fallback, "bash")
	if phys != filepath.Join(runtimeRoot, "tmp", "bash") || rel != "runtime/tmp/bash" {
		t.Fatalf("项目布局不符：phys=%s rel=%s", phys, rel)
	}
	phys, rel = runtimeOutputDir(context.Background(), fallback, "fetch")
	if phys != filepath.Join(fallback, ".tako", "fetch") || rel != ".tako/fetch" {
		t.Fatalf("纯会话布局不符：phys=%s rel=%s", phys, rel)
	}
}

// 新项目第一次被多个入口同时使用：并发初始化与「要不要迁移」的只读判断交错时，
// 不能在「content 已建、layout 还没写」的窗口里把新项目判成存量（409 117116）。
func TestEnsureProjectLayoutConcurrentFirstUse(t *testing.T) {
	root := t.TempDir()
	const pairs = 32
	errs := make(chan error, pairs*2)
	for i := 0; i < pairs; i++ {
		go func() {
			_, _, err := ensureProjectLayout(root, "t1", "ws_race")
			errs <- err
		}()
		go func() {
			needs, err := projectLayoutNeedsMigration(root, "t1", "ws_race")
			if err == nil && needs {
				err = errors.New("并发首次使用期间被判成存量容器")
			}
			errs <- err
		}()
	}
	for i := 0; i < pairs*2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	needs, err := projectLayoutNeedsMigration(root, "t1", "ws_race")
	if err != nil || needs {
		t.Fatalf("初始化完成后仍要求迁移：needs=%v err=%v", needs, err)
	}
}
