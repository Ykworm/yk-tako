package runengine

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// 会话访问档位。accessNone 对外一律当成不存在（404 旧体或 frames 的 gone），
// 避免用 403 告诉别人「这场会话在，只是你不能看」。
type accessKind int

const (
	accessNone accessKind = iota
	accessRead
	accessOwner
)

var (
	ErrSessionNotFound  = errors.New("session not found")
	ErrSessionForbidden = errors.New("session forbidden")
	ErrBadVisibility    = errors.New("bad visibility")
)

// decideAccess：同 tenant 的 owner 可写；visibility=tenant 的其他成员只读；
// 跨 tenant、private 非 owner、空归属，都是 accessNone。
func decideAccess(sess Session, p principal) accessKind {
	if sess.TenantID == "" || sess.TenantID != p.TenantID || p.UserID == "" {
		return accessNone
	}
	if sess.UserID == p.UserID {
		return accessOwner
	}
	if sess.Visibility == "tenant" {
		return accessRead
	}
	return accessNone
}

// SessionAccess 按 principal 判一场会话。无行、已软删、跨租户、private 非 owner 都是 ErrSessionNotFound。
// tenant-visible 的非 owner 得到 accessRead；owner 得到 accessOwner。
func (s *Store) SessionAccess(ctx context.Context, id SessionID, p principal) (Session, accessKind, error) {
	var sess Session
	err := s.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", id).Take(&sess).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Session{}, accessNone, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, accessNone, err
	}
	kind := decideAccess(sess, p)
	if kind == accessNone {
		return Session{}, accessNone, ErrSessionNotFound
	}
	return sess, kind, nil
}

// RunSession 只拿 run 所属的 session。行不存在 found=false，调用方再决定 404 还是 gone。
func (s *Store) RunSession(ctx context.Context, runID RunID) (SessionID, bool, error) {
	var ids []SessionID
	err := s.db.WithContext(ctx).Raw(`SELECT session_id FROM runs WHERE id = ?`, runID).Scan(&ids).Error
	if err != nil {
		return "", false, err
	}
	if len(ids) == 0 || ids[0] == "" {
		return "", false, nil
	}
	return ids[0], true, nil
}

// ListSessions：scope=own（默认）只列本人；scope=tenant 只列本租户 visibility=tenant 的会话。
// workspaceID 非空时再按项目筛。空串表示不筛，游离会话留在结果里。
// 创建者用默认列表筛自己的项目时，返回挂在上面的全部会话，含别人的私人会话。
// scope=tenant 不放宽，避免借项目筛把别人的私人会话带出来。
// deleted_at 非空的行不出现，已删会话不在列表里复活。
func (s *Store) ListSessions(ctx context.Context, p principal, scope, workspaceID string) ([]Session, error) {
	q := s.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", p.TenantID)
	if scope == "tenant" {
		q = q.Where("visibility = ?", "tenant")
	} else if workspaceID != "" {
		owned, err := workspaceOwned(s.db.WithContext(ctx), workspaceID, p.TenantID, p.UserID)
		if err != nil {
			return nil, err
		}
		if !owned {
			q = q.Where("user_id = ?", p.UserID)
		}
	} else {
		q = q.Where("user_id = ?", p.UserID)
	}
	if workspaceID != "" {
		q = q.Where("workspace_id = ?", workspaceID)
	}
	var rows []Session
	err := q.Order("created_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

// SetSessionVisibility 只有 owner 能改。visibility 只接受 private 与 tenant。
func (s *Store) SetSessionVisibility(ctx context.Context, id SessionID, p principal, visibility string) error {
	if visibility != "private" && visibility != "tenant" {
		return ErrBadVisibility
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Session
		// 已软删的行不锁、不改。调用方看到的和没有这场会话一样。
		if err := tx.Raw(`SELECT id, tenant_id, user_id, visibility FROM sessions WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrSessionNotFound
		}
		kind := decideAccess(rows[0], p)
		if kind == accessNone {
			return ErrSessionNotFound
		}
		if kind != accessOwner {
			return ErrSessionForbidden
		}
		return tx.Exec(`UPDATE sessions SET visibility = ? WHERE id = ?`, visibility, id).Error
	})
}

// SetSessionWorkspace 把会话挂到项目上，或在 workspaceID 为空时解绑。
// 挂上只允许会话主人，并且先锁创建者自己的项目行，再锁会话。
// 解绑多一条：项目创建者可以把别人挂在自己项目上的会话解下来。不能改挂到别的项目。
// 这里只改列，不删会话，也不搬磁盘上的文件。
func (s *Store) SetSessionWorkspace(ctx context.Context, id SessionID, p principal, workspaceID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if workspaceID != "" {
			if err := lockOwnedWorkspace(tx, workspaceID, p.TenantID, p.UserID); err != nil {
				return err
			}
		}
		var rows []Session
		// 已软删的行不锁、不改挂。调用方看到的和没有这场会话一样。
		if err := tx.Raw(
			`SELECT id, tenant_id, user_id, visibility, workspace_id FROM sessions WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			id).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrSessionNotFound
		}
		kind := decideAccess(rows[0], p)
		allow := kind == accessOwner
		if !allow && workspaceID == "" {
			bound := ""
			if rows[0].WorkspaceID != nil {
				bound = *rows[0].WorkspaceID
			}
			if bound != "" {
				owned, err := workspaceOwned(tx, bound, p.TenantID, p.UserID)
				if err != nil {
					return err
				}
				allow = owned
			}
		}
		if !allow {
			if kind == accessNone {
				return ErrSessionNotFound
			}
			return ErrSessionForbidden
		}
		var wid any
		if workspaceID != "" {
			wid = workspaceID
		}
		return tx.Exec(`UPDATE sessions SET workspace_id = ? WHERE id = ?`, wid, id).Error
	})
}
