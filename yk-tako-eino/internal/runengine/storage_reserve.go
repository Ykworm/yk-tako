package runengine

import (
	"context"

	"gorm.io/gorm"
)

// TenantStorage 是一场写入还没落盘时占住的字节。配额判断在这一行的锁里做，
// 两个并发上传不能都拿着同一份目录大小通过。
type TenantStorage struct {
	TenantID string `gorm:"primaryKey"`
	Reserved int64  `gorm:"not null;default:0"`
}

func (TenantStorage) TableName() string { return "tenant_storage" }

// ReserveStorage 在持有租户行锁时数目录，再加上在途预留。超帽返回 errStorageQuota，不改预留。
// release 在写入成功或失败后都要调：成功后字节已经在目录里，预留必须放开。
func (s *Store) ReserveStorage(ctx context.Context, tenantID string, adding, maxBytes int64, used func() int64) (func(), error) {
	noop := func() {}
	if maxBytes <= 0 || adding <= 0 || tenantID == "" {
		return noop, nil
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO tenant_storage (tenant_id, reserved) VALUES (?, 0) ON CONFLICT (tenant_id) DO NOTHING`, tenantID).Error; err != nil {
			return err
		}
		var reserved int64
		if err := tx.Raw(`SELECT reserved FROM tenant_storage WHERE tenant_id = ? FOR UPDATE`, tenantID).Scan(&reserved).Error; err != nil {
			return err
		}
		var have int64
		if used != nil {
			have = used()
		}
		if reserved+adding+have > maxBytes {
			return errStorageQuota
		}
		return tx.Exec(`UPDATE tenant_storage SET reserved = ? WHERE tenant_id = ?`, reserved+adding, tenantID).Error
	})
	if err != nil {
		return nil, err
	}
	return func() {
		_ = s.db.WithContext(context.WithoutCancel(ctx)).Exec(
			`UPDATE tenant_storage SET reserved = GREATEST(reserved - ?, 0) WHERE tenant_id = ?`, adding, tenantID).Error
	}, nil
}
