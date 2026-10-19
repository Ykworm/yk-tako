package runengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 身份四表（doc/28 A0）。没有外键：多副本下靠唯一索引和事务，不靠进程内状态。

var (
	ErrEmailTaken     = errors.New("email taken")
	ErrRefreshInvalid = errors.New("refresh invalid")
	ErrNotMember      = errors.New("not a member")
	ErrLastOwner      = errors.New("last owner")
	ErrLastAdmin      = errors.New("last admin")
	ErrBadRole        = errors.New("bad role")
	ErrAlreadyMember  = errors.New("already member")
)

type User struct {
	ID              string `gorm:"primaryKey"`
	Email           string `gorm:"not null;uniqueIndex"`
	PasswordHash    string `gorm:"not null"`
	IsSystemAdmin   bool   `gorm:"not null"`
	Disabled        bool   `gorm:"not null"`
	TokenVersion    int    `gorm:"not null"`
	DefaultTenantID *string
	CreatedAt       time.Time `gorm:"not null"`
}

func (User) TableName() string { return "users" }

type Tenant struct {
	ID                string    `gorm:"primaryKey"`
	Kind              string    `gorm:"not null"`
	Name              string    `gorm:"not null"`
	Status            string    `gorm:"not null"`
	Plan              string    `gorm:"not null"`
	CreatedBy         string    `gorm:"not null"`
	CreatedAt         time.Time `gorm:"not null"`
	MaxConcurrentRuns int       `gorm:"not null;default:8"`
	MaxSubagents      int       `gorm:"not null;default:8"`
	DailyTokens       int64     `gorm:"not null;default:0"`
	MaxStorageBytes   int64     `gorm:"not null;default:0"`
}

func (TenantUsageDaily) TableName() string { return "tenant_usage_daily" }

// TenantUsageDaily 按租户按天累计 token。0 额度表示不限制。
type TenantUsageDaily struct {
	TenantID string `gorm:"primaryKey"`
	Day      string `gorm:"primaryKey"`
	Tokens   int64  `gorm:"not null;default:0"`
}

func (Tenant) TableName() string { return "tenants" }

type TenantMember struct {
	TenantID  string    `gorm:"primaryKey"`
	UserID    string    `gorm:"primaryKey"`
	Role      string    `gorm:"not null"`
	Status    string    `gorm:"not null"`
	InvitedBy string    `gorm:"not null;default:''"`
	JoinedAt  time.Time `gorm:"not null"`
}

func (TenantMember) TableName() string { return "tenant_members" }

type RefreshToken struct {
	ID         string    `gorm:"primaryKey"`
	UserID     string    `gorm:"not null;index"`
	TokenHash  string    `gorm:"not null;uniqueIndex"`
	FamilyID   string    `gorm:"not null;index"`
	TenantID   string    `gorm:"not null;default:'';index"`
	ExpiresAt  time.Time `gorm:"not null"`
	RevokedAt  *time.Time
	ReplacedBy string    `gorm:"not null;default:''"`
	CreatedAt  time.Time `gorm:"not null"`
}

func (RefreshToken) TableName() string { return "refresh_tokens" }

// MemberListing 是某个用户仍有效的成员身份，带上工作区名字供登录和 /me 列出。
type MemberListing struct {
	TenantID   string
	TenantName string
	Role       string
	Kind       string
}

func (s *Store) Signup(ctx context.Context, email, hash string) (User, Tenant, TenantMember, error) {
	uid, err := newRowID("u_")
	if err != nil {
		return User{}, Tenant{}, TenantMember{}, err
	}
	tid, err := newRowID("t_")
	if err != nil {
		return User{}, Tenant{}, TenantMember{}, err
	}
	now := time.Now()
	local := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		local = email[:i]
	}
	user := User{ID: uid, Email: email, PasswordHash: hash, CreatedAt: now}
	tenant := Tenant{ID: tid, Kind: "personal", Name: local, Status: "active", Plan: "free", CreatedBy: uid, CreatedAt: now}
	member := TenantMember{TenantID: tid, UserID: uid, Role: "owner", Status: "active", JoinedAt: now}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrEmailTaken
			}
			return err
		}
		if err := tx.Create(&tenant).Error; err != nil {
			return err
		}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
		return tx.Model(&User{}).Where("id = ?", uid).Update("default_tenant_id", tid).Error
	})
	if err != nil {
		return User{}, Tenant{}, TenantMember{}, err
	}
	user.DefaultTenantID = &tid
	return user, tenant, member, nil
}

func (s *Store) CreateTeamTenant(ctx context.Context, ownerID, name string) (Tenant, TenantMember, error) {
	tid, err := newRowID("t_")
	if err != nil {
		return Tenant{}, TenantMember{}, err
	}
	now := time.Now()
	tenant := Tenant{ID: tid, Kind: "team", Name: name, Status: "active", Plan: "free", CreatedBy: ownerID, CreatedAt: now}
	member := TenantMember{TenantID: tid, UserID: ownerID, Role: "owner", Status: "active", JoinedAt: now}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&tenant).Error; err != nil {
			return err
		}
		if err := tx.Create(&member).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Tenant{}, TenantMember{}, err
	}
	return tenant, member, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := s.db.WithContext(ctx).Where("email = ?", email).Take(&u).Error
	return u, err
}

func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	var u User
	err := s.db.WithContext(ctx).Where("id = ?", id).Take(&u).Error
	return u, err
}

func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := s.db.WithContext(ctx).Where("id = ? AND status = ?", id, "active").Take(&t).Error
	return t, err
}

func (s *Store) ListMemberships(ctx context.Context, userID string) ([]MemberListing, error) {
	var rows []MemberListing
	err := s.db.WithContext(ctx).
		Table("tenant_members").
		Select("tenant_members.tenant_id, tenants.name as tenant_name, tenant_members.role, tenants.kind").
		Joins("JOIN tenants ON tenants.id = tenant_members.tenant_id").
		Where("tenant_members.user_id = ? AND tenant_members.status = ? AND tenants.status = ?", userID, "active", "active").
		Order("tenant_members.joined_at, tenant_members.tenant_id").
		Scan(&rows).Error
	return rows, err
}

// ActiveTenantFor 取这个用户当前要进的工作区：default 仍有效就用它，否则用最早加入的那个。
func (s *Store) ActiveTenantFor(ctx context.Context, user User) (Tenant, MemberListing, []MemberListing, error) {
	rows, err := s.ListMemberships(ctx, user.ID)
	if err != nil {
		return Tenant{}, MemberListing{}, nil, err
	}
	if len(rows) == 0 {
		return Tenant{}, MemberListing{}, nil, ErrNotMember
	}
	pick := rows[0]
	if user.DefaultTenantID != nil {
		for _, row := range rows {
			if row.TenantID == *user.DefaultTenantID {
				pick = row
				break
			}
		}
	}
	tenant, err := s.GetTenant(ctx, pick.TenantID)
	if err != nil {
		return Tenant{}, MemberListing{}, nil, err
	}
	return tenant, pick, rows, nil
}

func (s *Store) HasActiveMembership(ctx context.Context, userID, tenantID string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).
		Table("tenant_members").
		Joins("JOIN tenants ON tenants.id = tenant_members.tenant_id").
		Where("tenant_members.user_id = ? AND tenant_members.tenant_id = ? AND tenant_members.status = ? AND tenants.status = ?",
			userID, tenantID, "active", "active").
		Count(&n).Error
	return n > 0, err
}

func (s *Store) IssueRefreshToken(ctx context.Context, userID, tenantID string, ttl time.Duration) (string, error) {
	raw, hash, id, family, err := newRefreshMaterial()
	if err != nil {
		return "", err
	}
	now := time.Now()
	row := RefreshToken{
		ID: id, UserID: userID, TenantID: tenantID, TokenHash: hash, FamilyID: family,
		ExpiresAt: now.Add(ttl), CreatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return "", err
	}
	return raw, nil
}

// RotateRefreshToken 把一张还活着的 refresh 换成同 family 的新行。旧行记下 replaced_by。
// 已经撤销的再拿来用，整组 family 一起撤销。
func (s *Store) RotateRefreshToken(ctx context.Context, presentedHash string, ttl time.Duration) (string, string, error) {
	raw, hash, id, _, err := newRefreshMaterial()
	if err != nil {
		return "", "", err
	}
	var userID string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row RefreshToken
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", presentedHash).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRefreshInvalid
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if row.RevokedAt != nil || !row.ExpiresAt.After(now) {
			if row.RevokedAt != nil {
				if err := tx.Model(&RefreshToken{}).
					Where("family_id = ? AND revoked_at IS NULL", row.FamilyID).
					Update("revoked_at", now).Error; err != nil {
					return err
				}
			}
			return ErrRefreshInvalid
		}
		fresh := RefreshToken{
			ID: id, UserID: row.UserID, TenantID: row.TenantID, TokenHash: hash, FamilyID: row.FamilyID,
			ExpiresAt: now.Add(ttl), CreatedAt: now,
		}
		if err := tx.Create(&fresh).Error; err != nil {
			return err
		}
		if err := tx.Model(&RefreshToken{}).Where("id = ?", row.ID).Updates(map[string]any{
			"revoked_at":  now,
			"replaced_by": id,
		}).Error; err != nil {
			return err
		}
		userID = row.UserID
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return userID, raw, nil
}

// SwitchDefaultTenant 确认成员身份后，撤销这张 refresh，并把 default 指到目标工作区。
// 非成员时 refresh 和 default 都不改。
func (s *Store) SwitchDefaultTenant(ctx context.Context, userID, tenantID, presentedHash string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ok, err := membershipInTx(tx, userID, tenantID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotMember
		}
		var row RefreshToken
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", presentedHash).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRefreshInvalid
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if row.UserID != userID || row.RevokedAt != nil || !row.ExpiresAt.After(now) {
			return ErrRefreshInvalid
		}
		if err := tx.Model(&RefreshToken{}).Where("id = ?", row.ID).Update("revoked_at", now).Error; err != nil {
			return err
		}
		return tx.Model(&User{}).Where("id = ?", userID).Update("default_tenant_id", tenantID).Error
	})
}

func (s *Store) RevokeUserTenantRefresh(ctx context.Context, userID, tenantID string) error {
	return s.db.WithContext(ctx).Model(&RefreshToken{}).
		Where("user_id = ? AND tenant_id = ? AND revoked_at IS NULL", userID, tenantID).
		Update("revoked_at", time.Now()).Error
}

func (s *Store) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	return s.db.WithContext(ctx).Model(&RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", time.Now()).Error
}

func membershipInTx(tx *gorm.DB, userID, tenantID string) (bool, error) {
	var n int64
	err := tx.Table("tenant_members").
		Joins("JOIN tenants ON tenants.id = tenant_members.tenant_id").
		Where("tenant_members.user_id = ? AND tenant_members.tenant_id = ? AND tenant_members.status = ? AND tenants.status = ?",
			userID, tenantID, "active", "active").
		Count(&n).Error
	return n > 0, err
}

func hashRefresh(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newRefreshMaterial() (raw, hash, id, family string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	hash = hashRefresh(raw)
	id, err = newRowID("rt_")
	if err != nil {
		return "", "", "", "", err
	}
	family, err = newRowID("f_")
	if err != nil {
		return "", "", "", "", err
	}
	return raw, hash, id, family, nil
}
