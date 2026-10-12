// 出站内镜对象的到期回收。上游只在生成窗口内需要这些图，长期留在云上没有任何收益。

package app

import (
	"log/slog"
	"time"
)

const providerEgressCleanupBatch = 200

const providerEgressDefaultRetentionDays = 7

// providerEgressDeleteObject 是 deleteOSSObject 的可替换别名。deleteOSSObject 本身是普通函数，
// 不做这层间接就测不到"物理删除失败不得删台账"这条规则。
var providerEgressDeleteObject = deleteOSSObject

// cleanupProviderEgressObjects 返回本轮真正回收掉的条数。
// 用忽略 Enabled 的读取路径：管理员关掉内镜后，已经镜像出去的对象仍然必须被回收。
func (s *Service) cleanupProviderEgressObjects() int {
	setting, ok, err := s.providerEgressStorageForMaintenance()
	if err != nil {
		slog.Warn("provider egress cleanup skipped: setting unreadable", "error", err)
		return 0
	}
	if !ok {
		return 0
	}
	normalized := normalizeOSSSetting(setting)
	retentionDays := providerEgressDefaultRetentionDays
	if policy, policyErr := s.RuntimePolicy(); policyErr == nil && policy.Storage.ProviderEgressRetentionDays > 0 {
		retentionDays = policy.Storage.ProviderEgressRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	records, err := s.repo.ProviderEgressObjectsExpired(cutoff, normalized.Provider, normalized.Bucket, providerEgressCleanupBatch)
	if err != nil {
		slog.Warn("provider egress cleanup query failed", "error", err)
		return 0
	}
	removed := 0
	for index := range records {
		record := records[index]
		if err := providerEgressDeleteObject(providerEgressSettingForRecord(normalized, record), record.ObjectKey); err != nil {
			// 物理删除没成功就绝不能先删记录，否则对象永久失联。
			slog.Warn("provider egress object delete failed", "id", record.ID, "objectKey", record.ObjectKey, "error", err)
			continue
		}
		if err := s.repo.DeleteProviderEgressObject(record.ID); err != nil {
			slog.Warn("provider egress ledger delete failed", "id", record.ID, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("provider egress objects cleaned", "removed", removed, "retentionDays", retentionDays, "bucket", normalized.Bucket)
	}
	return removed
}
