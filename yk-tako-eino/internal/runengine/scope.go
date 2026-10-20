package runengine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	scopeKey
	workspaceKey
)

// dataScope 是笔记和长期记忆的读写范围。Scope=user 只属于一个人；Scope=tenant 是团队共享。
// 零值表示这条调用还在旧的无身份路径上（单测），查询不加新过滤。
type dataScope struct {
	TenantID string
	UserID   string
	Scope    string
}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func principalFromCtx(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey).(principal)
	return p, ok && p.UserID != ""
}

func withScope(ctx context.Context, sc dataScope) context.Context {
	if sc.Scope == "" {
		sc.Scope = "user"
	}
	return context.WithValue(ctx, scopeKey, sc)
}

func scopeFrom(ctx context.Context) dataScope {
	sc, _ := ctx.Value(scopeKey).(dataScope)
	return sc
}

func scopeActive(sc dataScope) bool {
	return sc.TenantID != "" || sc.UserID != ""
}

// WorkspaceRef 是一场会话的作业目录。文件工具和 bash 从 ctx 取它，不再吃启动时的全局路径。
type WorkspaceRef struct {
	TenantID   string
	SessionID  SessionID
	Root       string
	TenantRoot string // <workspace>/tenants/<tenant>，存储帽数整棵租户目录
	MaxBytes   int64
	// Reserve 在落盘前占住字节。没设时 write_file 仍用目录大小做一次检查。
	Reserve func(adding int) (func(), error)
}

func withWorkspace(ctx context.Context, ref WorkspaceRef) context.Context {
	return context.WithValue(ctx, workspaceKey, ref)
}

func workspaceFrom(ctx context.Context) (WorkspaceRef, bool) {
	ref, ok := ctx.Value(workspaceKey).(WorkspaceRef)
	return ref, ok && ref.Root != ""
}

func activeRoot(ctx context.Context, fallback string) string {
	if ref, ok := workspaceFrom(ctx); ok {
		return ref.Root
	}
	return fallback
}

// tenantStorageRoot 是一个租户在 workspace 下的目录。存储帽数这整棵树，不单数一场会话。
func tenantStorageRoot(root, tenantID string) string {
	return filepath.Join(root, "tenants", tenantID)
}

// workspaceDir 算出项目目录，不创建它。id 只允许单段路径，避免从库里的值走出去。
// 删除用它做 RemoveAll：目录还没建过时，删一个不存在的路径也成功。
func workspaceDir(root, tenantID, workspaceID string) (string, error) {
	if root == "" || tenantID == "" || workspaceID == "" || workspaceID == "." || workspaceID == ".." ||
		strings.ContainsRune(workspaceID, os.PathSeparator) || strings.ContainsRune(workspaceID, '/') {
		return "", os.ErrInvalid
	}
	return filepath.Join(root, "tenants", tenantID, "workspaces", workspaceID), nil
}

// workspaceRoot 把挂了项目的会话的作业根落到 <root>/tenants/<tenant>/workspaces/<wid>/。
// 目录在第一场 run 起跑时建。
func workspaceRoot(root, tenantID, workspaceID string) (string, error) {
	dir, err := workspaceDir(root, tenantID, workspaceID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runWorkspace 按会话行选作业根。挂了项目就用项目目录，否则仍是这场会话自己的目录。
func runWorkspace(ctx context.Context, w *Worker, tenantID string, sessionID SessionID) (string, error) {
	wid, err := w.store.SessionWorkspaceID(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if wid != "" {
		return workspaceRoot(w.workspace, tenantID, wid)
	}
	return sessionWorkspace(w.workspace, tenantID, sessionID)
}

// sessionDir 算出会话目录，不创建它。id 只允许单段路径。删除用它做 RemoveAll。
func sessionDir(root, tenantID string, sessionID SessionID) (string, error) {
	id := string(sessionID)
	if root == "" || tenantID == "" || id == "" || id == "." || id == ".." ||
		strings.ContainsRune(id, os.PathSeparator) || strings.ContainsRune(id, '/') {
		return "", os.ErrInvalid
	}
	return filepath.Join(root, "tenants", tenantID, "sessions", id), nil
}

// sessionWorkspace 把作业根落到 <root>/tenants/<tenant>/sessions/<session>。
func sessionWorkspace(root, tenantID string, sessionID SessionID) (string, error) {
	if tenantID == "" || sessionID == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(root, "tenants", tenantID, "sessions", string(sessionID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// directoryBytes 累加目录里普通文件的大小。目录不存在当作 0。
func directoryBytes(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

func (s *Store) CountLiveRuns(ctx context.Context, tenantID string) (int, error) {
	var n int64
	err := s.db.WithContext(ctx).Raw(`
SELECT COUNT(*) FROM runs r
  JOIN sessions s ON s.id = r.session_id
 WHERE s.tenant_id = ? AND r.status = 'running' AND r.lease_until > now()`, tenantID).Scan(&n).Error
	return int(n), err
}

func (s *Store) TenantQuota(ctx context.Context, tenantID string) (maxRuns, maxSub int, dailyTokens, maxStorage int64, err error) {
	var row struct {
		MaxConcurrentRuns int
		MaxSubagents      int
		DailyTokens       int64
		MaxStorageBytes   int64
	}
	err = s.db.WithContext(ctx).Raw(
		`SELECT max_concurrent_runs, max_subagents, daily_tokens, max_storage_bytes FROM tenants WHERE id = ?`, tenantID).Scan(&row).Error
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if row.MaxConcurrentRuns <= 0 {
		row.MaxConcurrentRuns = 8
	}
	if row.MaxSubagents <= 0 {
		row.MaxSubagents = 8
	}
	return row.MaxConcurrentRuns, row.MaxSubagents, row.DailyTokens, row.MaxStorageBytes, nil
}

func (s *Store) DailyTokenUsage(ctx context.Context, tenantID string, day time.Time) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(tokens), 0) FROM tenant_usage_daily WHERE tenant_id = ? AND day = ?`,
		tenantID, day.UTC().Format("2006-01-02")).Scan(&n).Error
	return n, err
}

func (s *Store) AddDailyTokens(ctx context.Context, tenantID string, tokens int64) error {
	if tenantID == "" || tokens == 0 {
		return nil
	}
	day := time.Now().UTC().Format("2006-01-02")
	return s.db.WithContext(ctx).Exec(`
INSERT INTO tenant_usage_daily (tenant_id, day, tokens) VALUES (?, ?, ?)
ON CONFLICT (tenant_id, day) DO UPDATE SET tokens = tenant_usage_daily.tokens + EXCLUDED.tokens`,
		tenantID, day, tokens).Error
}

// trySubagentSlot 按租户占一个子代理槽。store 没有 redis 或 ctx 没有身份时直接放行，单测的进程内 sem 仍有效。
func (s *Store) trySubagentSlot(ctx context.Context) (bool, error) {
	p, ok := principalFromCtx(ctx)
	if !ok || s.rdb == nil {
		return true, nil
	}
	_, maxSub, _, _, err := s.TenantQuota(ctx, p.TenantID)
	if err != nil {
		return false, err
	}
	key := "tk:subagents:" + p.TenantID
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	_ = s.rdb.Expire(ctx, key, 30*time.Minute).Err()
	if int(n) > maxSub {
		_ = s.rdb.Decr(ctx, key).Err()
		return false, nil
	}
	return true, nil
}

func (s *Store) releaseSubagentSlot(ctx context.Context) {
	p, ok := principalFromCtx(ctx)
	if !ok || s.rdb == nil {
		return
	}
	_ = s.rdb.Decr(ctx, "tk:subagents:"+p.TenantID).Err()
}
