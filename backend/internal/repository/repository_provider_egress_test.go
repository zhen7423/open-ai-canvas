package repository

import (
	"testing"
	"time"

	"yingce/backend/internal/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newProviderEgressRepository(t *testing.T) *Repository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.ProviderEgressObject{}); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func TestProviderEgressObjectLookupAndUniqueReuse(t *testing.T) {
	repo := newProviderEgressRepository(t)
	first := &model.ProviderEgressObject{
		ID: "eg-1", UserID: "user-1", ContentSHA256: "aaa", Provider: "aliyun",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce", ObjectKey: "canvas/egress/user-1/aaa.png",
		MimeType: "image/png", SizeBytes: 12, ResourceID: "res-1", DeliveryRevision: "rev-1",
		CreatedAt: time.Now(), LastUsedAt: time.Now(),
	}
	if err := repo.CreateProviderEgressObject(first); err != nil {
		t.Fatal(err)
	}
	found, err := repo.ProviderEgressObject("user-1", "aaa", "aliyun", "yingce", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != "eg-1" {
		t.Fatalf("查回 %q", found.ID)
	}
	if err := repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "eg-2", UserID: "user-1", ContentSHA256: "aaa", Provider: "aliyun",
		Bucket: "yingce", ObjectKey: "dup.png", DeliveryRevision: "rev-1",
	}); err == nil {
		t.Fatal("相同内容同桶同投递指纹应当冲突")
	}
	// 投递指纹变化即视为另一条映射。
	if err := repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "eg-3", UserID: "user-1", ContentSHA256: "aaa", Provider: "aliyun",
		Bucket: "yingce", ObjectKey: "rev2.png", DeliveryRevision: "rev-2", LastUsedAt: time.Now(),
	}); err != nil {
		t.Fatalf("换投递指纹应可新建：%v", err)
	}
	if count, err := repo.ProviderEgressObjectCount(); err != nil || count != 2 {
		t.Fatalf("台账计数 = %d, err = %v", count, err)
	}
	if err := repo.TouchProviderEgressObject("eg-1", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	expired, err := repo.ProviderEgressObjectsExpired(time.Now().Add(-24*time.Hour), "aliyun", "yingce", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != "eg-1" {
		t.Fatalf("到期扫描结果 = %#v", expired)
	}
	// 换桶后旧桶的行不得进入当前桶的回收扫描，否则会拿当前凭据去猜别处的对象。
	otherBucket, err := repo.ProviderEgressObjectsExpired(time.Now().Add(-24*time.Hour), "aliyun", "other-bucket", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherBucket) != 0 {
		t.Fatalf("别的桶不该被扫到：%#v", otherBucket)
	}
	if err := repo.DeleteProviderEgressObject("eg-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ProviderEgressObject("user-1", "aaa", "aliyun", "yingce", "rev-1"); err == nil {
		t.Fatal("删除后不应再查得到")
	}
}

func TestAllProviderEgressObjectsListsAcrossBuckets(t *testing.T) {
	repo := newProviderEgressRepository(t)
	now := time.Now()
	seed := []model.ProviderEgressObject{
		{ID: "eg-1", UserID: "user-1", ContentSHA256: "a", Provider: "aliyun", Bucket: "old", ObjectKey: "k/1", DeliveryRevision: "rev-1", LastUsedAt: now.Add(-time.Hour)},
		{ID: "eg-2", UserID: "user-1", ContentSHA256: "b", Provider: "qiniu", Bucket: "new", ObjectKey: "k/2", DeliveryRevision: "rev-2", LastUsedAt: now},
	}
	for index := range seed {
		if err := repo.CreateProviderEgressObject(&seed[index]); err != nil {
			t.Fatal(err)
		}
	}
	// 全量列举跨桶：管理端"清空映射"要逐行按各自的落点删对象。
	all, err := repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != "eg-1" || all[1].ID != "eg-2" {
		t.Fatalf("全量列举结果 = %#v", all)
	}
	if all[0].Bucket != "old" || all[1].Provider != "qiniu" {
		t.Fatal("列举必须带上行自身的落点信息，否则无法按行删除")
	}
	if limited, err := repo.AllProviderEgressObjects(1); err != nil || len(limited) != 1 {
		t.Fatalf("列举应受上限约束：%d err=%v", len(limited), err)
	}
	if count, err := repo.ProviderEgressObjectCount(); err != nil || count != 2 {
		t.Fatalf("台账计数 = %d, err = %v", count, err)
	}
}
