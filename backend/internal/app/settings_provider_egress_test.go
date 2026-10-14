package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"yingce/backend/internal/model"
)

func TestUpdateProviderEgressSettingRequiresCredentialsWhenEnabled(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: true}); err == nil ||
		!strings.Contains(err.Error(), "存储类型") {
		t.Fatalf("启用状态下空配置必须被拒：%v", err)
	}

	setting, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !setting.HasAccessKeySecret {
		t.Fatalf("公开态应报告密钥已配置：%#v", setting)
	}
	if setting.AccessKeyID != "AKID" {
		t.Fatalf("AccessKeyId 需原样回显供沿用判断：%q", setting.AccessKeyID)
	}

	reopened, err := svc.AdminProviderEgressSetting(admin)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Enabled || reopened.Bucket != "yingce-egress" {
		t.Fatalf("回读设置不符：%#v", reopened)
	}

	// 留空密钥表示沿用原密钥，不能清空。
	kept, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", PathPrefix: "canvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !kept.HasAccessKeySecret {
		t.Fatal("未填 SecretKey 时应当沿用原密钥")
	}

	// 换 AccessKeyId 却不给 SecretKey 必须被拒：否则会把新账号配上一个旧密钥。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID-NEW", PathPrefix: "canvas",
	}); err == nil || !strings.Contains(err.Error(), "SecretKey") {
		t.Fatalf("换 AccessKeyId 未配 SecretKey 必须被拒：%v", err)
	}
}

func TestProviderEgressSettingRejectsUnknownProviderAndCDNMode(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "minio", Endpoint: "http://127.0.0.1:9000", Bucket: "b",
		AccessKeyID: "ak", AccessKeySecret: "sk",
	}); err == nil || !strings.Contains(err.Error(), "仅支持") {
		t.Fatalf("未知存储类型必须被拒：%v", err)
	}

	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "b",
		AccessKeyID: "ak", AccessKeySecret: "sk", CDNBaseURL: "https://cdn.example.com",
	}); err == nil || !strings.Contains(err.Error(), "CDN 鉴权方式") {
		t.Fatalf("填了 CDN 域名却没选鉴权方式必须被拒：%v", err)
	}

	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "b",
		AccessKeyID: "ak", AccessKeySecret: "sk", CDNAuthMode: "private",
	}); err == nil || !strings.Contains(err.Error(), "CDN 鉴权方式") {
		t.Fatalf("非法 CDN 鉴权方式必须被拒：%v", err)
	}
}

func TestActiveProviderEgressSettingDisabledByDefault(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	setting, enabled, err := svc.activeProviderEgressSetting()
	if err != nil {
		t.Fatal(err)
	}
	if enabled || setting.Bucket != "" {
		t.Fatalf("未配置时不应启用：%#v enabled=%v", setting, enabled)
	}
	if _, err := svc.AdminProviderEgressSetting(admin); err != nil {
		t.Fatalf("管理员读取空设置不应报错：%v", err)
	}
}

func TestActiveProviderEgressSettingCarriesPrefixAndSecretBack(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "/canvas/",
	}); err != nil {
		t.Fatal(err)
	}
	setting, enabled, err := svc.activeProviderEgressSetting()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("配置完整后应当启用")
	}
	if setting.AccessKeySecret != "secret" {
		t.Fatal("解密后的密钥必须回到内存态，否则无法签名")
	}
	if setting.PathPrefix != "canvas" {
		t.Fatalf("前缀应被规范化：%q", setting.PathPrefix)
	}
	if prefix := providerEgressObjectPrefix(providerEgressSettingValue{PathPrefix: "canvas"}); prefix != "canvas/egress" {
		t.Fatalf("内镜前缀 = %q", prefix)
	}
	if prefix := providerEgressObjectPrefix(providerEgressSettingValue{}); prefix != "egress" {
		t.Fatalf("无前缀时内镜前缀 = %q", prefix)
	}
}

func TestProviderEgressSettingRejectsPrivateEndpoint(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	// 内镜签名地址就是拿 Endpoint 拼的：内网 Endpoint 签出来的地址上游同样取不到，
	// 等于把本轮要修的问题搬到桶上，必须在保存时就挡住而不是等生成失败。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "s3", Region: "us-east-1", Endpoint: "http://127.0.0.1:9000", Bucket: "egress",
		AccessKeyID: "ak", AccessKeySecret: "sk",
	}); err == nil || !strings.Contains(err.Error(), "公网地址") {
		t.Fatalf("内网 Endpoint 必须被拒：%v", err)
	}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "s3", Region: "us-east-1", Endpoint: "minio.internal", Bucket: "egress",
		AccessKeyID: "ak", AccessKeySecret: "sk",
	}); err == nil || !strings.Contains(err.Error(), "公网地址") {
		t.Fatalf("内网域名 Endpoint 必须被拒：%v", err)
	}
	// 内网 Endpoint + 公网 CDN 出口是合法组合：上游走 CDN，签名不再依赖 Endpoint。
	setting, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "s3", Region: "us-east-1", Endpoint: "minio.internal", Bucket: "egress",
		AccessKeyID: "ak", AccessKeySecret: "sk", CDNBaseURL: "https://cdn.example.com", CDNAuthMode: "public",
	})
	if err != nil {
		t.Fatalf("有公网 CDN 出口时内网源站应可接受：%v", err)
	}
	if setting.CDNBaseURL != "https://cdn.example.com" || setting.CDNAuthMode != "public" {
		t.Fatalf("CDN 配置未保存：%#v", setting)
	}
}

func TestProviderEgressSettingDoesNotInheritSecretAcrossProviders(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}
	// 换厂商但 AccessKeyId 恰好相同：绝不能把上一家的密钥沿用给新厂商。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "tencent", Region: "ap-beijing",
		Endpoint: "cos.ap-beijing.myqcloud.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", PathPrefix: "canvas",
	}); err == nil || !strings.Contains(err.Error(), "SecretKey") {
		t.Fatalf("跨厂商沿用密钥必须被拒：%v", err)
	}
}

// 总闸关着时也要拿同一把尺子试桶：providerEgressSettingFromRequest 只在 Enabled 为真时校验完整性，
// 而管理员通常先关着试配置。缺字段的草稿必须在发起连接之前就被拒；
// 只断言报错文字分不出来（连接失败的文案也提到 Bucket），所以这里用探针替身证明"根本没发起连接"。
func TestProviderEgressConnectivityTestRejectsIncompleteDraftBeforeConnecting(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	previousProbe := verifyProviderEgressConnection
	probeCalls := 0
	verifyProviderEgressConnection = func(ossSettingValue, string) error {
		probeCalls++
		return nil
	}
	t.Cleanup(func() { verifyProviderEgressConnection = previousProbe })

	incomplete := ProviderEgressSettingRequest{
		Enabled: false, Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}
	if _, err := svc.TestProviderEgressSetting(admin, incomplete); err == nil || !strings.Contains(err.Error(), "请填写出站内镜的 Bucket") {
		t.Fatalf("缺 Bucket 的草稿必须按启用态完整性被拒：%v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("半成品配置不该发起真实连接，探针被调用 %d 次", probeCalls)
	}

	// 反向锁：补齐 Bucket 后，即使总闸是关着的也要允许试连通，否则管理员无法在启用前验证配置。
	complete := incomplete
	complete.Bucket = "yingce-egress"
	bucket, err := svc.TestProviderEgressSetting(admin, complete)
	if err != nil {
		t.Fatalf("完整草稿在关闸状态下也应能试连通：%v", err)
	}
	if bucket != "yingce-egress" || probeCalls != 1 {
		t.Fatalf("试连通结果不符：bucket=%q, probeCalls=%d", bucket, probeCalls)
	}
}

// 探针失败必须原样报出来：连通性测试的唯一价值就是"这次真的没连上"。
func TestProviderEgressConnectivityTestSurfacesProbeFailure(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	previousProbe := verifyProviderEgressConnection
	probeCalls := 0
	verifyProviderEgressConnection = func(ossSettingValue, string) error {
		probeCalls++
		return errors.New("AccessDenied: bucket policy denies write")
	}
	t.Cleanup(func() { verifyProviderEgressConnection = previousProbe })

	if _, err := svc.TestProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: false, Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com",
		Bucket: "yingce-egress", AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("探针失败必须向上抛出：%v", err)
	}
	if probeCalls != 1 {
		t.Fatalf("完整草稿应当发起一次探针，实际 %d 次", probeCalls)
	}
}

func TestClearProviderEgressMappingsDeletesObjectsBeforeRows(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	previousDelete := providerEgressDeleteObject
	attempted := []string{}
	failKey := "canvas/egress/user-1/stuck.png"
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		attempted = append(attempted, objectKey)
		if objectKey == failKey {
			return errors.New("bucket policy denies delete")
		}
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	now := time.Now()
	for _, record := range []model.ProviderEgressObject{
		{ID: "ok", UserID: "user-1", ContentSHA256: "a", Provider: "aliyun", Bucket: "yingce-egress", ObjectKey: "canvas/egress/user-1/a.png", DeliveryRevision: "rev", LastUsedAt: now},
		{ID: "stuck", UserID: "user-1", ContentSHA256: "b", Provider: "aliyun", Bucket: "yingce-egress", ObjectKey: failKey, DeliveryRevision: "rev", LastUsedAt: now},
	} {
		item := record
		if err := svc.repo.CreateProviderEgressObject(&item); err != nil {
			t.Fatal(err)
		}
	}

	deleted, remaining, err := svc.ClearProviderEgressMappings(admin)
	if err == nil || !strings.Contains(err.Error(), "未能删除") {
		t.Fatalf("对象删不掉时必须报错：%v", err)
	}
	if deleted != 1 {
		t.Fatalf("成功清除条数 = %d, want 1", deleted)
	}
	// 报错时也必须把"还剩几条"如实带回去，前端才知道不是清空完成了。
	if remaining != 1 {
		t.Fatalf("残留条数 = %d, want 1", remaining)
	}
	// 只删记录不删对象会留下无人认领的公网副本，所以两条都必须发起删除。
	if len(attempted) != 2 {
		t.Fatalf("对象删除发起集合 = %#v, want 2 条", attempted)
	}
	for _, key := range []string{"canvas/egress/user-1/a.png", failKey} {
		found := false
		for _, item := range attempted {
			if item == key {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("未发起删除 %q：%#v", key, attempted)
		}
	}
	// 删除失败的那行必须保留，否则对象彻底失联。
	leftovers, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 || leftovers[0].ID != "stuck" {
		t.Fatalf("残留台账不符：%#v", leftovers)
	}
}

func TestClearProviderEgressMappingsLeavesForeignDeliveriesAlone(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	// 当前落点由 configureProviderEgress 决定：aliyun / oss-cn-beijing.aliyuncs.com / yingce-egress。
	configureProviderEgress(t, svc)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	previousDelete := providerEgressDeleteObject
	attempted := []string{}
	usedSettings := []ossSettingValue{}
	providerEgressDeleteObject = func(setting ossSettingValue, objectKey string) error {
		attempted = append(attempted, objectKey)
		usedSettings = append(usedSettings, setting)
		return nil
	}
	t.Cleanup(func() { providerEgressDeleteObject = previousDelete })

	now := time.Now()
	for _, record := range []model.ProviderEgressObject{
		{ID: "home", UserID: "user-1", ContentSHA256: "h", Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress", ObjectKey: "canvas/egress/user-1/home.png", DeliveryRevision: "rev", LastUsedAt: now},
		{ID: "other-bucket", UserID: "user-1", ContentSHA256: "o", Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "someone-else", ObjectKey: "canvas/egress/user-1/other.png", DeliveryRevision: "rev", LastUsedAt: now},
		{ID: "other-vendor", UserID: "user-1", ContentSHA256: "v", Provider: "tencent", Endpoint: "cos.ap-beijing.myqcloud.com", Bucket: "yingce-egress", ObjectKey: "canvas/egress/user-1/tencent.png", DeliveryRevision: "rev", LastUsedAt: now},
	} {
		item := record
		if err := svc.repo.CreateProviderEgressObject(&item); err != nil {
			t.Fatal(err)
		}
	}

	deleted, remaining, err := svc.ClearProviderEgressMappings(admin)
	if err != nil {
		t.Fatalf("清空其他落点的行不该报错：%v", err)
	}
	if deleted != 1 {
		t.Fatalf("只应删除当前落点的行，deleted = %d, want 1", deleted)
	}
	// 剩下的两条必须如实报出来，前端才能说明"没清完"而不是"清空成功"。
	if remaining != 2 {
		t.Fatalf("残留条数 = %d, want 2", remaining)
	}
	if len(attempted) != 1 || attempted[0] != "canvas/egress/user-1/home.png" {
		t.Fatalf("对其他落点发起了删除：%#v", attempted)
	}
	for _, setting := range usedSettings {
		if setting.Provider != "aliyun" || setting.Bucket != "yingce-egress" || setting.Endpoint != "oss-cn-beijing.aliyuncs.com" {
			t.Fatalf("把当前凭据用在了别的落点上：%#v", setting)
		}
	}
	leftovers, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 2 {
		t.Fatalf("其他落点的映射应当原样保留：%#v", leftovers)
	}
}

func TestClearProviderEgressMappingsRefusesWithoutCredentials(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	// 没有可用配置时盲删记录，等于让云上对象失去唯一回收入口。
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "orphan", UserID: "user-1", ContentSHA256: "z", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/orphan.png", DeliveryRevision: "rev", LastUsedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ClearProviderEgressMappings(admin); err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("凭据不可用时必须拒绝清空：%v", err)
	}
	leftovers, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 {
		t.Fatalf("被拒绝的清空不得动台账：%#v", leftovers)
	}
}

func TestProviderEgressSettingEncryptsSecretAtRest(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "super-secret-value", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}

	stored, err := svc.repo.SystemSetting(providerEgressSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	// 明文密钥绝不能进库；同时公开态 DTO 也不得回显 SecretKey。
	if strings.Contains(stored.ValueJSON, "super-secret-value") {
		t.Fatalf("设置正文泄露明文密钥：%s", stored.ValueJSON)
	}
	if !strings.Contains(stored.ValueJSON, encryptedSettingPrefix) {
		t.Fatalf("SecretKey 未按既有方式加密落库：%s", stored.ValueJSON)
	}
	public := publicProviderEgressSetting(stored, normalizeProviderEgressSetting(providerEgressSettingValue{
		Enabled: true, Provider: "aliyun", AccessKeyID: "AKID", AccessKeySecret: "super-secret-value",
	}))
	if strings.Contains(fmt.Sprint(public), "super-secret-value") {
		t.Fatal("公开态 DTO 泄露密钥")
	}
}

func TestProviderEgressSettingRequiresAdmin(t *testing.T) {
	// 用内镜专用夹具：它迁移了台账表，而共享的资源夹具不含该表。
	svc, _ := newProviderEgressTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}
	// 内镜桶能公开读取创作素材，权限面必须只有管理员。
	plain := &model.User{ID: "user-1", Role: model.UserRoleUser}
	for name, call := range map[string]func() error{
		"读取": func() error { _, err := svc.AdminProviderEgressSetting(plain); return err },
		"保存": func() error {
			_, err := svc.UpdateProviderEgressSetting(plain, ProviderEgressSettingRequest{})
			return err
		},
		"测试": func() error {
			_, err := svc.TestProviderEgressSetting(plain, ProviderEgressSettingRequest{})
			return err
		},
		"清空": func() error { _, _, err := svc.ClearProviderEgressMappings(plain); return err },
	} {
		if err := call(); err == nil {
			t.Fatalf("普通用户执行 %s 必须被拒", name)
		} else if !strings.Contains(err.Error(), "管理员") && !strings.Contains(err.Error(), "权限") {
			t.Fatalf("%s 的拒绝原因应当是权限不足：%v", name, err)
		}
	}
	// 管理员拒绝路径不得留下副作用：先放一行台账，非管理员"清空"被拒后它必须还在。
	if err := svc.repo.CreateProviderEgressObject(&model.ProviderEgressObject{
		ID: "keep-me", UserID: "user-1", ContentSHA256: "k", Provider: "aliyun", Bucket: "yingce-egress",
		ObjectKey: "canvas/egress/user-1/keep.png", DeliveryRevision: "rev", LastUsedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	leftovers, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 || leftovers[0].ID != "keep-me" {
		t.Fatalf("非管理员操作产生了副作用：%#v", leftovers)
	}
}

func TestProviderEgressMasterSwitchTogglesBothWays(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	full := ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}
	if _, err := svc.UpdateProviderEgressSetting(admin, full); err != nil {
		t.Fatal(err)
	}
	// 关
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: false}); err != nil {
		t.Fatalf("只发 enabled:false 应能关闸：%v", err)
	}
	// 开：只发 enabled:true 必须能原样开回来，否则 API 用户必须重填 SecretKey（而它从不回显）。
	enabled, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: true})
	if err != nil {
		t.Fatalf("只发 enabled:true 应能开闸：%v", err)
	}
	if !enabled.Enabled || enabled.Bucket != "yingce-egress" || !enabled.HasAccessKeySecret {
		t.Fatalf("开闸后配置不完整：%#v", enabled)
	}
}

func TestProviderEgressSwitchRefusesToEnableIncompleteConfig(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	// 从没配过桶：只发 enabled:true 不能凭空开出一个不可用的内镜，必须要求填写。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: true}); err == nil ||
		!strings.Contains(err.Error(), "存储类型") {
		t.Fatalf("无配置时开启必须被拒并指向需填的字段：%v", err)
	}
	// 配好后又改空 Endpoint（显式重填路径）同样被拒。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Endpoint: "oss-cn-beijing.aliyuncs.com",
		Bucket: "yingce-egress", AccessKeyID: "AKID", AccessKeySecret: "sk",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "sk",
	}); err == nil || !strings.Contains(err.Error(), "Endpoint") {
		t.Fatalf("显式重填时缺 Endpoint 必须被拒：%v", err)
	}
}

func TestProviderEgressSaveResponseCarriesTTLHint(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	// 页面保存后直接用 PATCH 回包覆盖 state：回包若缺有效期，"可被公网读取 N 小时"这句会凭空消失。
	saved, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ExpiresAfterHours <= 0 {
		t.Fatalf("保存回包缺少有效期提示：%+v", saved.ExpiresAfterHours)
	}
	read, err := svc.AdminProviderEgressSetting(admin)
	if err != nil {
		t.Fatal(err)
	}
	if read.ExpiresAfterHours != saved.ExpiresAfterHours {
		t.Fatalf("GET 与 PATCH 的有效期不一致：%d vs %d", read.ExpiresAfterHours, saved.ExpiresAfterHours)
	}
}

func TestProviderEgressDeliveryModeSurvivesToggleOnlyPatch(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	saved, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, DeliveryMode: providerEgressDeliveryAlwaysMirror, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.DeliveryMode != providerEgressDeliveryAlwaysMirror {
		t.Fatalf("保存后公开态应回显交付形态：%#v", saved)
	}

	// 只翻开关必须连同模式一起保留：SecretKey 从不回显，清掉就等于逼管理员重打密钥，
	// 与之前"只发 enabled 冲掉桶配置"是同一类缺陷。
	disabled, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: false})
	if err != nil {
		t.Fatalf("只发 enabled:false 应能关闸：%v", err)
	}
	if disabled.DeliveryMode != providerEgressDeliveryAlwaysMirror || disabled.Bucket != "yingce-egress" || !disabled.HasAccessKeySecret {
		t.Fatalf("关闸后模式与配置应原样保留：%#v", disabled)
	}
	enabled, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: true})
	if err != nil {
		t.Fatalf("只发 enabled:true 应能开回来：%v", err)
	}
	if enabled.DeliveryMode != providerEgressDeliveryAlwaysMirror {
		t.Fatalf("开回后交付形态漂移：%#v", enabled)
	}

	// 显式改回内联优先也要真的写进去。
	back, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, DeliveryMode: providerEgressDeliveryInlineFirst, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.DeliveryMode != providerEgressDeliveryInlineFirst {
		t.Fatalf("切回内联优先失败：%#v", back)
	}
}

func TestProviderEgressSettingRejectsUnknownDeliveryMode(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}

	// 关着也要拒：存进库的坏值不会因为总闸关着就无害，下一次启用会带着它上线。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: false, DeliveryMode: "mirror_all"}); err == nil ||
		!strings.Contains(err.Error(), "交付形态") {
		t.Fatalf("非法交付形态必须被拒：%v", err)
	}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, DeliveryMode: "mirror_all", Provider: "aliyun",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret",
	}); err == nil || !strings.Contains(err.Error(), "交付形态") {
		t.Fatalf("启用态保存路径同样要拒非法值：%v", err)
	}
	for _, mode := range []string{providerEgressDeliveryInlineFirst, providerEgressDeliveryAlwaysMirror} {
		if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: false, DeliveryMode: mode}); err != nil {
			t.Fatalf("合法值 %s 不该被拒：%v", mode, err)
		}
	}
}

// 旧库的 provider_egress JSON 里根本没有 deliveryMode 这个键，读出来必须按内联优先理解，
// 不能因为缺字段就变成"未知形态"或误走镜像。
func TestProviderEgressLegacySettingReadsAsInlineFirst(t *testing.T) {
	svc := newResourceTestService(t)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}
	setProviderEgressDeliveryModeRaw(t, svc, "")

	public, err := svc.AdminProviderEgressSetting(admin)
	if err != nil {
		t.Fatal(err)
	}
	if public.DeliveryMode != providerEgressDeliveryInlineFirst {
		t.Fatalf("缺 deliveryMode 键的旧记录应读成内联优先，实际 %#v", public.DeliveryMode)
	}
}
