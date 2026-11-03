package app

import (
	"context"
	"testing"

	"yingce/backend/internal/model"
)

func TestProviderMediaRequirementForBuiltInVideoProtocols(t *testing.T) {
	input := canvasGenerationInput{Mode: "video", Config: providerConfig{InterfaceType: string(model.ChannelInterfaceMiniMaxVideo)}}
	requirement := providerMediaRequirementFor(context.Background(), input)
	if !requirement.policy.requireURL || !requirement.policy.preferURL {
		t.Fatalf("MiniMax 视频协议必须要求公网地址：%#v", requirement.policy)
	}
	if requirement.reason == "" {
		t.Fatal("判定原因不得为空，失败文案要靠它指向该改哪里")
	}
}

func TestProviderMediaRequirementForSeedanceVideosCompatible(t *testing.T) {
	// 自建 Seedance 兼容渠道（如 yuxibox /videos）只收公网 HTTPS URL；
	// 模型名或接口名命中 Seedance 都必须打开 requireURL，否则参考图会以 data URL 内联后被上游拒绝。
	for name, config := range map[string]providerConfig{
		"model name":     {InterfaceType: "seedance-videos-compatible", Model: "seedance2.5-yx-ml"},
		"interface only": {InterfaceType: "seedance-videos-compatible", Model: "sd-2.5"},
	} {
		t.Run(name, func(t *testing.T) {
			input := canvasGenerationInput{Mode: "video", Config: config}
			requirement := providerMediaRequirementFor(context.Background(), input)
			if !requirement.policy.requireURL || !requirement.policy.preferURL {
				t.Fatalf("Seedance 视频协议必须要求公网地址：%#v", requirement.policy)
			}
			if requirement.reason == "" {
				t.Fatal("判定原因不得为空")
			}
		})
	}
}

func TestProviderMediaRequirementForPlainTextProtocol(t *testing.T) {
	input := canvasGenerationInput{Mode: "text", Config: providerConfig{InterfaceType: string(model.ChannelInterfaceChatCompletion)}}
	requirement := providerMediaRequirementFor(context.Background(), input)
	if requirement.policy.requireURL {
		t.Fatalf("普通文本协议不该要求公网地址：%#v", requirement)
	}
}

func TestProviderMediaRequirementForMaskClearsReason(t *testing.T) {
	// 遮罩走 multipart 字节路径，requireURL 被关掉的同时原因也必须清空，
	// 否则失败文案会声称"渠道要求公网地址"而实际并不要求。
	input := canvasGenerationInput{
		Mode:   "video",
		Config: providerConfig{InterfaceType: string(model.ChannelInterfaceMiniMaxVideo)},
		Mask:   &providerMedia{ID: "mask"},
	}
	requirement := providerMediaRequirementFor(context.Background(), input)
	if requirement.policy.requireURL {
		t.Fatal("遮罩场景不得要求公网地址")
	}
	if requirement.reason != "" {
		t.Fatalf("requireURL 关闭后原因必须为空：%q", requirement.reason)
	}
}
