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

// SessionAccess 按 principal 判一场会话。无行、跨租户、private 非 owner 都是 ErrSessionNotFound。
// tenant-visible 的非 owner 得到 accessRead；owner 得到 accessOwner。
func (s *Store) SessionAccess(ctx context.Context, id SessionID, p principal) (Session, accessKind, error) {
	var sess Session
	err := s.db.WithContext(ctx).Where("id = ?", id).Take(&sess).Error
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
func (s *Store) ListSessions(ctx context.Context, p principal, scope string) ([]Session, error) {
	q := s.db.WithContext(ctx).Where("tenant_id = ?", p.TenantID)
	if scope == "tenant" {
		q = q.Where("visibility = ?", "tenant")
	} else {
		q = q.Where("user_id = ?", p.UserID)
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
		if err := tx.Raw(`SELECT id, tenant_id, user_id, visibility FROM sessions WHERE id = ? FOR UPDATE`, id).Scan(&rows).Error; err != nil {
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
