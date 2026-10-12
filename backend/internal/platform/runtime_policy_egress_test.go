package platform

import (
	"strings"
	"testing"
)

func TestRuntimePolicyProviderEgressRetention(t *testing.T) {
	if got := DefaultRuntimePolicy().Storage.ProviderEgressRetentionDays; got != 7 {
		t.Fatalf("缺省保留天数 = %d, want 7", got)
	}

	if err := validateRuntimePolicy(DefaultRuntimePolicy()); err != nil {
		t.Fatalf("缺省策略应当合法：%v", err)
	}

	// 0 合法，语义是"按缺省处理"：该设置整段 JSON 落库，早于本字段存在的历史策略
	// 反序列化后就是 0，把 0 判非法会让管理员只是重存一次就被自己的表单挡住。
	zero := DefaultRuntimePolicy()
	zero.Storage.ProviderEgressRetentionDays = 0
	if err := validateRuntimePolicy(zero); err != nil {
		t.Fatalf("0 天不应被拒：%v", err)
	}

	over := DefaultRuntimePolicy()
	over.Storage.ProviderEgressRetentionDays = 91
	if err := validateRuntimePolicy(over); err == nil || !strings.Contains(err.Error(), "出站内镜") {
		t.Fatalf("91 天必须被拒，实际 err = %v", err)
	}

	negative := DefaultRuntimePolicy()
	negative.Storage.ProviderEgressRetentionDays = -1
	if err := validateRuntimePolicy(negative); err == nil {
		t.Fatal("负数必须被拒")
	}

	// 自用态不覆盖 Storage，应继承同一缺省。
	if got := SelfUseRuntimePolicy().Storage.ProviderEgressRetentionDays; got != 7 {
		t.Fatalf("自用态保留天数 = %d, want 7", got)
	}
}
