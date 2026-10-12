package app

import (
	"errors"
	"testing"
	"time"

	"yingce/backend/internal/model"
)

func TestCleanupProviderEgressObjectsRemovesExpiredAndKeepsLedgerOnFailure(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)

	previousDelete := providerEgressDeleteObject
	deletedKeys := []string{}
	failKey := "canvas/egress/user-1/keep.png"
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		if objectKey == failKey {
			return errors.New("object lock held")
		}
		deletedKeys = append(deletedKeys, objectKey)
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	now := time.Now()
	records := []model.ProviderEgressObject{
		{ID: "old-ok", UserID: "user-1", ContentSHA256: "a", Provider: "aliyun", Bucket: "yingce-egress",
			ObjectKey: "canvas/egress/user-1/gone.png", DeliveryRevision: "rev", LastUsedAt: now.Add(-30 * 24 * time.Hour)},
		{ID: "old-fail", UserID: "user-1", ContentSHA256: "b", Provider: "aliyun", Bucket: "yingce-egress",
			ObjectKey: failKey, DeliveryRevision: "rev", LastUsedAt: now.Add(-30 * 24 * time.Hour)},
		{ID: "fresh", UserID: "user-1", ContentSHA256: "c", Provider: "aliyun", Bucket: "yingce-egress",
			ObjectKey: "canvas/egress/user-1/fresh.png", DeliveryRevision: "rev", LastUsedAt: now},
	}
	for index := range records {
		if err := svc.repo.CreateProviderEgressObject(&records[index]); err != nil {
			t.Fatal(err)
		}
	}

	removed := svc.cleanupProviderEgressObjects()
	if removed != 1 {
		t.Fatalf("成功回收条数 = %d, want 1", removed)
	}
	if len(deletedKeys) != 1 || deletedKeys[0] != "canvas/egress/user-1/gone.png" {
		t.Fatalf("删除键集合 = %#v", deletedKeys)
	}
	remaining, err := svc.repo.ProviderEgressObjectsExpired(now.Add(-24*time.Hour), "aliyun", "yingce-egress", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 物理删除失败的那条必须留在台账里，等下一轮重试；未过期的那条不该被碰。
	if len(remaining) != 1 || remaining[0].ID != "old-fail" {
		t.Fatalf("物理删除失败不得删台账：%#v", remaining)
	}
}

func TestCleanupProviderEgressObjectsSkipsWhenUnconfigured(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	// 完全没配置内镜：清理必须安静跳过，不能因为设置缺失而报错或删任何东西。
	previousDelete := providerEgressDeleteObject
	calls := 0
	providerEgressDeleteObject = func(ossSettingValue, string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "orphan", UserID: "user-1", ContentSHA256: "z", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/orphan.png", DeliveryRevision: "rev",
		LastUsedAt: time.Now().Add(-400 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if removed := svc.cleanupProviderEgressObjects(); removed != 0 {
		t.Fatalf("内镜未配置却回收了 %d 条", removed)
	}
	if calls != 0 {
		t.Fatalf("内镜未配置却调用了删除 %d 次", calls)
	}
}

func TestCleanupProviderEgressObjectsHonoursRetentionDays(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	previousDelete := providerEgressDeleteObject
	deletedKeys := []string{}
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		deletedKeys = append(deletedKeys, objectKey)
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	now := time.Now()
	// 10 天前的对象：缺省 7 天保留期应当删；若保留期被读成 0 或更大档位则不应删。
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "ten-days", UserID: "user-1", ContentSHA256: "t", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/ten.png", DeliveryRevision: "rev", LastUsedAt: now.Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// 3 天前的对象必须留下。
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "three-days", UserID: "user-1", ContentSHA256: "h", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/three.png", DeliveryRevision: "rev", LastUsedAt: now.Add(-3 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	svc.cleanupProviderEgressObjects()

	if len(deletedKeys) != 1 || deletedKeys[0] != "canvas/egress/user-1/ten.png" {
		t.Fatalf("按 7 天保留期应只删 10 天前那条：%#v", deletedKeys)
	}
}

func TestCleanupProviderEgressObjectsIgnoresOtherBuckets(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)

	previousDelete := providerEgressDeleteObject
	deletedKeys := []string{}
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		deletedKeys = append(deletedKeys, objectKey)
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	old := time.Now().Add(-30 * 24 * time.Hour)
	// 当前配置指向 yingce-egress；旧桶里的行不能拿当前凭据去猜删。
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "other-bucket", UserID: "user-1", ContentSHA256: "x", Provider: "aliyun", Bucket: "abandoned-bucket",
		ObjectKey: "canvas/egress/user-1/other.png", DeliveryRevision: "rev-old", LastUsedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "current-bucket", UserID: "user-1", ContentSHA256: "y", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/mine.png", DeliveryRevision: "rev-old", LastUsedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	if removed := svc.cleanupProviderEgressObjects(); removed != 1 {
		t.Fatalf("回收条数 = %d, want 1", removed)
	}
	if len(deletedKeys) != 1 || deletedKeys[0] != "canvas/egress/user-1/mine.png" {
		t.Fatalf("不该碰别的桶：%#v", deletedKeys)
	}
	// 别的桶的行必须还在，由该桶的生命周期规则负责。
	leftovers, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 || leftovers[0].ID != "other-bucket" {
		t.Fatalf("残留台账不符：%#v", leftovers)
	}
}

func TestCleanupProviderEgressObjectsStillRunsWhenDisabled(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	// 管理员关掉内镜开关。已经镜像出去的对象仍然必须被回收，否则公网副本永久留存。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: false, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}

	previousDelete := providerEgressDeleteObject
	deletedKeys := []string{}
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		deletedKeys = append(deletedKeys, objectKey)
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "expired", UserID: "user-1", ContentSHA256: "z", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/expired.png", DeliveryRevision: "rev",
		LastUsedAt: time.Now().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if removed := svc.cleanupProviderEgressObjects(); removed != 1 {
		t.Fatalf("内镜停用后仍应回收已镜像对象，removed = %d", removed)
	}
	if len(deletedKeys) != 1 {
		t.Fatalf("删除集合 = %#v", deletedKeys)
	}
}
