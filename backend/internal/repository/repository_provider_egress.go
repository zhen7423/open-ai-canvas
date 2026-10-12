package repository

import (
	"time"

	"yingce/backend/internal/model"
)

// ProviderEgressObject 按内容寻址查已有内镜映射。列名与 model.ProviderEgressObject 的
// gorm column 标签一致（content_sha256、cdn_mode），不走 GORM 默认命名，避免手改字段名时静默错位。
func (r *Repository) ProviderEgressObject(userID string, contentSHA256 string, provider string, bucket string, deliveryRevision string) (*model.ProviderEgressObject, error) {
	var object model.ProviderEgressObject
	err := r.db.First(&object,
		"user_id = ? AND content_sha256 = ? AND provider = ? AND bucket = ? AND delivery_revision = ?",
		userID, contentSHA256, provider, bucket, deliveryRevision).Error
	if err != nil {
		return nil, err
	}
	return &object, nil
}

func (r *Repository) CreateProviderEgressObject(object *model.ProviderEgressObject) error {
	return r.db.Create(object).Error
}

// TouchProviderEgressObject 只前移使用时间，不改内容映射；用于回收策略的"最近未使用"判据。
func (r *Repository) TouchProviderEgressObject(id string, usedAt time.Time) error {
	return r.db.Model(&model.ProviderEgressObject{}).
		Where("id = ?", id).
		UpdateColumn("last_used_at", usedAt).Error
}

func (r *Repository) DeleteProviderEgressObject(id string) error {
	return r.db.Delete(&model.ProviderEgressObject{}, "id = ?", id).Error
}

// ProviderEgressObjectsExpired 只扫描当前落点（provider + bucket）下的到期行。
// 按投递指纹过滤会漏掉换凭据前的历史对象，所以不过滤指纹；
// 换桶后旧桶里的对象交给该桶的生命周期规则回收，本任务不猜旧凭据去删。
func (r *Repository) ProviderEgressObjectsExpired(before time.Time, provider string, bucket string, limit int) ([]model.ProviderEgressObject, error) {
	objects := make([]model.ProviderEgressObject, 0, limit)
	err := r.db.Where("last_used_at < ? AND provider = ? AND bucket = ?", before, provider, bucket).
		Order("last_used_at asc").Limit(limit).Find(&objects).Error
	return objects, err
}

// AllProviderEgressObjects 供管理端"清空映射"逐行删除云上对象使用。
func (r *Repository) AllProviderEgressObjects(limit int) ([]model.ProviderEgressObject, error) {
	objects := make([]model.ProviderEgressObject, 0, limit)
	err := r.db.Order("last_used_at asc").Limit(limit).Find(&objects).Error
	return objects, err
}

func (r *Repository) ProviderEgressObjectCount() (int64, error) {
	var count int64
	err := r.db.Model(&model.ProviderEgressObject{}).Count(&count).Error
	return count, err
}
