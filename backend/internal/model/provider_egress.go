package model

import "time"

// ProviderEgressObject 记录一次"本地素材送往公网渠道"的内镜结果。
// 唯一键含落点四元组与投递指纹：换桶或改凭据后旧行自动不参与复用，
// 不会把旧桶的 objectKey 拼成新桶的域名。
type ProviderEgressObject struct {
	ID               string    `json:"id" gorm:"primaryKey;size:36"`
	UserID           string    `json:"userId" gorm:"index;size:36;uniqueIndex:idx_provider_egress_lookup,priority:1"`
	ContentSHA256    string    `json:"contentSha256" gorm:"column:content_sha256;size:64;uniqueIndex:idx_provider_egress_lookup,priority:2"`
	Provider         string    `json:"provider" gorm:"size:24;uniqueIndex:idx_provider_egress_lookup,priority:3"`
	Endpoint         string    `json:"endpoint" gorm:"size:200"`
	Bucket           string    `json:"bucket" gorm:"size:160;uniqueIndex:idx_provider_egress_lookup,priority:4"`
	ObjectKey        string    `json:"objectKey" gorm:"size:500"`
	MimeType         string    `json:"mimeType" gorm:"size:120"`
	SizeBytes        int64     `json:"sizeBytes"`
	ResourceID       string    `json:"resourceId" gorm:"index;size:36"`
	CDNMode          string    `json:"cdnMode" gorm:"column:cdn_mode;size:16"`
	DeliveryRevision string    `json:"deliveryRevision" gorm:"size:32;uniqueIndex:idx_provider_egress_lookup,priority:5"`
	CreatedAt        time.Time `json:"createdAt"`
	LastUsedAt       time.Time `json:"lastUsedAt" gorm:"index"`
}
