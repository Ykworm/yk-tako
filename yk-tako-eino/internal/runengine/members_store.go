package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AuditLog 记下成员、角色和平台管理员的变更。tenant_id 空 = 平台事件，租户审计看不到。
type AuditLog struct {
	ID          string    `gorm:"primaryKey"`
	TenantID    string    `gorm:"not null;default:'';index:audit_log_tenant_created,priority:1"`
	ActorUserID string    `gorm:"not null"`
	ActorRole   string    `gorm:"not null;default:''"`
	Action      string    `gorm:"not null"`
	TargetType  string    `gorm:"not null"`
	TargetID    string    `gorm:"not null"`
	Outcome     string    `gorm:"not null"`
	Details     string    `gorm:"not null;default:''"`
	CreatedAt   time.Time `gorm:"not null;index:audit_log_tenant_created,priority:2"`
}

func (AuditLog) TableName() string { return "audit_log" }

type MemberRow struct {
	UserID   string
	Email    string
	Role     string
	Status   string
	JoinedAt time.Time
}

func roleRank(role string) int {
	switch role {
	case "owner":
		return 40
	case "admin":
		return 30
	case "member":
		return 20
	case "viewer":
		return 10
	default:
		return 0
	}
}

func assignableRole(role string) bool {
	return role == "admin" || role == "member" || role == "viewer"
}

func (s *Store) ListMembers(ctx context.Context, tenantID string) ([]MemberRow, error) {
	var rows []MemberRow
	err := s.db.WithContext(ctx).
		Table("tenant_members").
		Select("tenant_members.user_id, users.email, tenant_members.role, tenant_members.status, tenant_members.joined_at").
		Joins("JOIN users ON users.id = tenant_members.user_id").
		Where("tenant_members.tenant_id = ? AND tenant_members.status = ?", tenantID, "active").
		Order("tenant_members.joined_at, tenant_members.user_id").
		Scan(&rows).Error
	return rows, err
}

func (s *Store) AddMemberByEmail(ctx context.Context, tenantID, email, role string, actor principal) (MemberRow, error) {
	if !assignableRole(role) {
		return MemberRow{}, ErrBadRole
	}
	user, err := s.GetUserByEmail(ctx, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return MemberRow{}, ErrNotMember
	}
	if err != nil {
		return MemberRow{}, err
	}
	now := time.Now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing TenantMember
		err := tx.Where("tenant_id = ? AND user_id = ?", tenantID, user.ID).Take(&existing).Error
		if err == nil && existing.Status == "active" {
			return ErrAlreadyMember
		}
		if err == nil {
			if err := tx.Model(&TenantMember{}).
				Where("tenant_id = ? AND user_id = ?", tenantID, user.ID).
				Updates(map[string]any{"role": role, "status": "active", "joined_at": now, "invited_by": actor.UserID}).Error; err != nil {
				return err
			}
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			row := TenantMember{TenantID: tenantID, UserID: user.ID, Role: role, Status: "active", InvitedBy: actor.UserID, JoinedAt: now}
			if err := tx.Create(&row).Error; err != nil {
				if isUniqueViolation(err) {
					return ErrAlreadyMember
				}
				return err
			}
		} else {
			return err
		}
		return writeAuditTx(tx, actor, tenantID, "member.added", "user", user.ID, "ok", map[string]string{"role": role, "email": email})
	})
	if err != nil {
		return MemberRow{}, err
	}
	return MemberRow{UserID: user.ID, Email: user.Email, Role: role, Status: "active", JoinedAt: now}, nil
}

func (s *Store) UpdateMemberRole(ctx context.Context, tenantID, userID, role string, actor principal) (MemberRow, error) {
	if !assignableRole(role) {
		return MemberRow{}, ErrBadRole
	}
	var out MemberRow
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		members, err := lockMembers(tx, tenantID)
		if err != nil {
			return err
		}
		var target *TenantMember
		owners := 0
		for i := range members {
			if members[i].Role == "owner" {
				owners++
			}
			if members[i].UserID == userID {
				target = &members[i]
			}
		}
		if target == nil {
			return ErrNotMember
		}
		if target.Role == "owner" && owners <= 1 {
			return ErrLastOwner
		}
		old := target.Role
		if err := tx.Model(&TenantMember{}).
			Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Update("role", role).Error; err != nil {
			return err
		}
		email := emailInTx(tx, userID)
		out = MemberRow{UserID: userID, Email: email, Role: role, Status: "active"}
		return writeAuditTx(tx, actor, tenantID, "member.role_changed", "user", userID, "ok", map[string]string{"oldRole": old, "newRole": role})
	})
	return out, err
}

func (s *Store) RemoveMember(ctx context.Context, tenantID, userID string, actor principal) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := dropMember(tx, tenantID, userID); err != nil {
			return err
		}
		return writeAuditTx(tx, actor, tenantID, "member.removed", "user", userID, "ok", nil)
	})
}

func (s *Store) LeaveTenant(ctx context.Context, tenantID, userID string, actor principal) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := dropMember(tx, tenantID, userID); err != nil {
			return err
		}
		return writeAuditTx(tx, actor, tenantID, "member.left", "user", userID, "ok", nil)
	})
}

func (s *Store) TransferOwner(ctx context.Context, tenantID, targetUserID string, actor principal) error {
	if targetUserID == actor.UserID {
		return ErrBadRole
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		members, err := lockMembers(tx, tenantID)
		if err != nil {
			return err
		}
		var actorRow, target *TenantMember
		for i := range members {
			if members[i].UserID == actor.UserID {
				actorRow = &members[i]
			}
			if members[i].UserID == targetUserID {
				target = &members[i]
			}
		}
		if actorRow == nil || actorRow.Role != "owner" {
			return ErrBadRole
		}
		if target == nil {
			return ErrNotMember
		}
		if err := tx.Model(&TenantMember{}).Where("tenant_id = ? AND user_id = ?", tenantID, actor.UserID).Update("role", "admin").Error; err != nil {
			return err
		}
		if err := tx.Model(&TenantMember{}).Where("tenant_id = ? AND user_id = ?", tenantID, targetUserID).Update("role", "owner").Error; err != nil {
			return err
		}
		return writeAuditTx(tx, actor, tenantID, "owner.transferred", "user", targetUserID, "ok", map[string]string{"from": actor.UserID})
	})
}

func (s *Store) ListAllTenants(ctx context.Context) ([]Tenant, error) {
	var rows []Tenant
	err := s.db.WithContext(ctx).Order("created_at, id").Find(&rows).Error
	return rows, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var rows []User
	err := s.db.WithContext(ctx).Order("created_at, id").Find(&rows).Error
	return rows, err
}

func (s *Store) BootstrapSystemAdmin(ctx context.Context, email string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&User{}).Where("email = ?", email).Update("is_system_admin", true)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (s *Store) SetSystemAdmin(ctx context.Context, userID string, on bool, actor principal) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []string
		if err := tx.Raw(`SELECT id FROM users WHERE is_system_admin = true FOR UPDATE`).Scan(&ids).Error; err != nil {
			return err
		}
		if !on {
			others := 0
			for _, id := range ids {
				if id != userID {
					others++
				}
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		if err := tx.Model(&User{}).Where("id = ?", userID).Update("is_system_admin", on).Error; err != nil {
			return err
		}
		action := "admin.revoked"
		if on {
			action = "admin.promoted"
		}
		return writeAuditTx(tx, actor, "", action, "user", userID, "ok", nil)
	})
}

func (s *Store) ListAudit(ctx context.Context, tenantID string, platform bool, limit int) ([]AuditLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := s.db.WithContext(ctx).Model(&AuditLog{})
	if platform {
		q = q.Where("tenant_id = ?", "")
	} else {
		q = q.Where("tenant_id = ?", tenantID)
	}
	var rows []AuditLog
	err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

func lockMembers(tx *gorm.DB, tenantID string) ([]TenantMember, error) {
	var rows []TenantMember
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND status = ?", tenantID, "active").
		Find(&rows).Error
	return rows, err
}

func dropMember(tx *gorm.DB, tenantID, userID string) error {
	members, err := lockMembers(tx, tenantID)
	if err != nil {
		return err
	}
	var target *TenantMember
	owners := 0
	for i := range members {
		if members[i].Role == "owner" {
			owners++
		}
		if members[i].UserID == userID {
			target = &members[i]
		}
	}
	if target == nil {
		return ErrNotMember
	}
	if target.Role == "owner" && owners <= 1 {
		return ErrLastOwner
	}
	if err := tx.Where("tenant_id = ? AND user_id = ?", tenantID, userID).Delete(&TenantMember{}).Error; err != nil {
		return err
	}
	if err := tx.Model(&RefreshToken{}).
		Where("user_id = ? AND tenant_id = ? AND revoked_at IS NULL", userID, tenantID).
		Update("revoked_at", time.Now()).Error; err != nil {
		return err
	}
	var other string
	if err := tx.Raw(
		`SELECT tenant_id FROM tenant_members WHERE user_id = ? AND status = 'active' ORDER BY joined_at LIMIT 1`,
		userID).Scan(&other).Error; err != nil {
		return err
	}
	if other == "" {
		return tx.Model(&User{}).Where("id = ? AND default_tenant_id = ?", userID, tenantID).Update("default_tenant_id", nil).Error
	}
	return tx.Model(&User{}).Where("id = ? AND default_tenant_id = ?", userID, tenantID).Update("default_tenant_id", other).Error
}

func emailInTx(tx *gorm.DB, userID string) string {
	var email string
	_ = tx.Raw(`SELECT email FROM users WHERE id = ?`, userID).Scan(&email).Error
	return email
}

func writeAuditTx(tx *gorm.DB, actor principal, tenantID, action, targetType, targetID, outcome string, details any) error {
	id, err := newRowID("al_")
	if err != nil {
		return err
	}
	raw := ""
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	role := actor.Role
	if tenantID == "" && actor.SystemAdmin {
		role = "system"
	}
	row := AuditLog{
		ID: id, TenantID: tenantID, ActorUserID: actor.UserID, ActorRole: role,
		Action: action, TargetType: targetType, TargetID: targetID, Outcome: outcome,
		Details: raw, CreatedAt: time.Now(),
	}
	return tx.Create(&row).Error
}
