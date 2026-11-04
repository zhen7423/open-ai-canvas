package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

// textModelTestNoisyPNG 造一张无法被 PNG 压掉的大图：随机像素接近熵上限，
// 保证输出体积确实超过模型单图上限，压缩路径才会真的跑起来。
func textModelTestNoisyPNG(t *testing.T, width, height int, alpha bool) []byte {
	t.Helper()
	source := rand.New(rand.NewSource(20261104))
	var encoded bytes.Buffer
	if alpha {
		img := image.NewNRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: uint8(source.Intn(256)), G: uint8(source.Intn(256)), B: uint8(source.Intn(256)), A: 255})
			}
		}
		// 左上角留一块真正的透明区域：压缩只能保留它，或把它铺成白底，不能涂黑。
		for y := 0; y < height/4; y++ {
			for x := 0; x < width/4; x++ {
				img.SetNRGBA(x, y, color.NRGBA{})
			}
		}
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal(err)
		}
		return encoded.Bytes()
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(source.Intn(256)), G: uint8(source.Intn(256)), B: uint8(source.Intn(256)), A: 255})
		}
	}
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func decodedImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestCompressImageForTextModelShrinksOversizedImage(t *testing.T) {
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	const modelLimit = 800 * 1024
	if int64(len(original)) <= modelLimit {
		t.Fatalf("fixture must exceed the model limit, got %d bytes", len(original))
	}
	compressed := compressImageForTextModel(original, "image/png", modelLimit)
	if !compressed.Compressed || compressed.MIME != "image/jpeg" {
		t.Fatalf("oversized image was not converted: %+v", compressed)
	}
	if int64(len(compressed.Data)) > modelLimit {
		t.Fatalf("compressed image is still %d bytes", len(compressed.Data))
	}
	if max(compressed.Width, compressed.Height) > textModelImageMaxEdge || compressed.Width <= 0 || compressed.Height <= 0 {
		t.Fatalf("compressed image kept unexpected dimensions %dx%d", compressed.Width, compressed.Height)
	}
	if decoded := decodedImage(t, compressed.Data); decoded.Bounds().Dx() != compressed.Width || decoded.Bounds().Dy() != compressed.Height {
		t.Fatal("reported dimensions do not match the encoded copy")
	}
}

func TestCompressImageForTextModelKeepsImageUnderLimitUntouched(t *testing.T) {
	original := textModelTestNoisyPNG(t, 900, 700, false)
	limit := int64(len(original))
	untouched := compressImageForTextModel(original, "image/png", limit)
	if untouched.Compressed || untouched.MIME != "image/png" || !bytes.Equal(untouched.Data, original) {
		t.Fatal("an image inside the model limit must be forwarded byte for byte")
	}
}

// 上限为 0 表示模型没有声明单图上限：没有可压的目标尺寸，必须原样发送。
func TestCompressImageForTextModelLeavesUnlimitedModelAlone(t *testing.T) {
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	untouched := compressImageForTextModel(original, "image/png", 0)
	if untouched.Compressed || !bytes.Equal(untouched.Data, original) {
		t.Fatal("a model without a size limit must receive the original bytes")
	}
}

func TestCompressImageForTextModelKeepsTransparentAreaVisible(t *testing.T) {
	original := textModelTestNoisyPNG(t, 900, 700, true)
	const modelLimit = 400 * 1024
	compressed := compressImageForTextModel(original, "image/png", modelLimit)
	if !compressed.Compressed || int64(len(compressed.Data)) > modelLimit {
		t.Fatalf("transparent image was not compressed into the model limit: %+v", compressed)
	}
	decoded := decodedImage(t, compressed.Data)
	red, green, blue, alpha := decoded.At(5, 5).RGBA()
	switch compressed.MIME {
	case "image/png":
		if alpha == 0xffff {
			t.Fatal("PNG copy lost the transparent area")
		}
	case "image/jpeg":
		// 铺白底：透明区域转 JPEG 后必须是亮色，不能变成黑块。
		if red>>8 < 200 || green>>8 < 200 || blue>>8 < 200 {
			t.Fatalf("transparent area was flattened to %d,%d,%d instead of white", red>>8, green>>8, blue>>8)
		}
	default:
		t.Fatalf("unexpected MIME after compression: %s", compressed.MIME)
	}
}

func TestCompressImageForTextModelLeavesUnknownCodecAlone(t *testing.T) {
	original := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 512*1024)
	untouched := compressImageForTextModel(original, "image/heic", 1024)
	if untouched.Compressed || !bytes.Equal(untouched.Data, original) {
		t.Fatal("an undecodable image must not be rewritten")
	}
}

// textModelTestService 用纯 Go 的 modernc 驱动起服务：本机 CI 容器可能没有 C 编译器，
// cgo 版 go-sqlite3 在这些机器上起不来，而这里只验证资源读取与压缩交付。
func textModelTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+newID()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Resource{}, &model.SystemSetting{}, &model.CanvasProject{}); err != nil {
		t.Fatal(err)
	}
	return &Service{repo: repository.New(db), dataDir: t.TempDir(), disablePiRuntime: true}, db
}

func textModelTestResource(t *testing.T, s *Service, db *gorm.DB, id string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.dataDir, "resources"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "resources", id+".png"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Resource{ID: id, UserID: "user", Kind: "image", Status: model.ResourceStatusReady, Provider: "local", ObjectKey: id + ".png", MimeType: "image/png", Size: int64(len(data)), Width: 1400, Height: 1000}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestHydrateProviderMediaKeepsImageUnderModelLimit(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	textModelTestResource(t, s, db, "ref-fits", original)
	media := providerMedia{StorageKey: "resource:ref-fits", MimeType: "image/png"}
	// 30 MB 是文本模型默认声明的单张上限：没超过就不该动这张图。
	if err := s.hydrateProviderMedia("user", &media, providerMediaHydrationPolicy{imageOnly: true, maxBytes: 30 << 20}); err != nil {
		t.Fatal(err)
	}
	if media.DataURL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(original) {
		t.Fatal("an image inside the model limit must be forwarded byte for byte")
	}
	if media.Bytes != int64(len(original)) || media.MimeType != "image/png" || media.Width != 1400 || media.Height != 1000 {
		t.Fatalf("untouched image metadata changed: %s %dx%d %d bytes", media.MimeType, media.Width, media.Height, media.Bytes)
	}
}

func TestHydrateProviderMediaCompressesImageAboveModelLimit(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	textModelTestResource(t, s, db, "ref-big", original)
	const modelLimit = 800 * 1024
	media := providerMedia{StorageKey: "resource:ref-big", MimeType: "image/png"}
	if err := s.hydrateProviderMedia("user", &media, providerMediaHydrationPolicy{imageOnly: true, maxBytes: modelLimit}); err != nil {
		t.Fatal(err)
	}
	mimeType, payload, ok := splitCreationDataURL(media.DataURL)
	if !ok || mimeType != "image/jpeg" {
		t.Fatalf("vision copy was not compressed: %q", media.DataURL[:min(len(media.DataURL), 64)])
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > modelLimit || media.Bytes != int64(len(data)) {
		t.Fatalf("vision copy is %d bytes (media says %d, limit %d)", len(data), media.Bytes, modelLimit)
	}
	if media.MimeType != "image/jpeg" || media.Width > textModelImageMaxEdge || media.Height > textModelImageMaxEdge {
		t.Fatalf("vision copy metadata was not updated: %s %dx%d", media.MimeType, media.Width, media.Height)
	}
}

// 压不进模型上限时只能"这张不附"，不能让整个模型步骤失败：
// 图片已经在对话历史里，失败会让模型这一步什么都看不到。
func TestHydrateGenerationMediaOmitsUnreachableImageWithoutFailing(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 900, 700, false)
	textModelTestResource(t, s, db, "ref-tight", original)
	// 0.5 KB 的上限连最小缩略图都装不下，压缩必然够不到。
	limits := TextReferenceConfig{MaxImages: 2, MaxImageBytes: 512}
	inspection := cloudAgentImageInspection{Receipt: map[string]any{"nodeId": "cat", "bytes": len(original)}, ImageURL: "resource:ref-tight", Purpose: "看画面"}
	canonical := canonicalAgentRequest{Messages: []map[string]any{{"role": "user", "content": cloudAgentImageContentParts(inspection)}}}
	input := canvasGenerationInput{Mode: "text", Prompt: "描述图片", ReferenceImages: []providerMedia{{StorageKey: "resource:ref-tight", MimeType: "image/png"}}, AgentRequests: &agentToolRequests{Canonical: &canonical}, Config: providerConfig{Model: "text-test", InterfaceType: string(model.ChannelInterfaceChatCompletion), APIKey: "test-only", CapabilityConfig: &ModelCapabilityConfig{Text: &TextCapabilityConfig{References: limits}}}}
	if err := s.hydrateGenerationMedia("user", &input, providerMediaHydrationPolicy{}); err != nil {
		t.Fatalf("one unreachable image must not fail the whole model step: %v", err)
	}
	if strings.TrimSpace(input.ReferenceImages[0].DataURL) != "" || !input.ReferenceImages[0].Omitted {
		t.Fatal("an unreachable image must be marked as omitted, not sent as pixels")
	}
	if dropped := cloudAgentDropOmittedImages(&input); dropped != 1 {
		t.Fatalf("omitted image was not dropped from the request: %d", dropped)
	}
	if len(input.ReferenceImages) != 0 {
		t.Fatal("an omitted image must leave the reference list resolveAgentResourcePlaceholders reads")
	}
	raw, _ := json.Marshal(input.AgentRequests.Canonical.Messages)
	if strings.Contains(string(raw), `"type":"image_url"`) || strings.Contains(string(raw), "resource:ref-tight") {
		t.Fatalf("request still carries the omitted image part or its storage key: %s", raw)
	}
	if !strings.Contains(string(raw), "没有看到它的画面") {
		t.Fatalf("request must tell the model the image was not seen: %s", raw)
	}
}

// 第二步及以后必须命中缓存：连源文件删掉都能发出同样的图，
// 说明这条路径没有再把文件读一遍、更没有重新解码编码。
func TestHydrateProviderMediaReusesCompressedCopy(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	textModelTestResource(t, s, db, "ref-cache", original)
	const modelLimit = 800 * 1024
	first := providerMedia{StorageKey: "resource:ref-cache", MimeType: "image/png"}
	started := time.Now()
	if err := s.hydrateProviderMedia("user", &first, providerMediaHydrationPolicy{imageOnly: true, maxBytes: modelLimit}); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(started)
	if err := os.Remove(filepath.Join(s.dataDir, "resources", "ref-cache.png")); err != nil {
		t.Fatal(err)
	}
	second := providerMedia{StorageKey: "resource:ref-cache", MimeType: "image/png"}
	started = time.Now()
	if err := s.hydrateProviderMedia("user", &second, providerMediaHydrationPolicy{imageOnly: true, maxBytes: modelLimit}); err != nil {
		t.Fatalf("cached copy was not reused: %v", err)
	}
	warm := time.Since(started)
	if second.DataURL != first.DataURL || second.Bytes != first.Bytes || second.Width != first.Width {
		t.Fatal("cached copy differs from the freshly compressed one")
	}
	if warm > cold/2 {
		t.Fatalf("cache did not save work: cold=%v warm=%v", cold, warm)
	}
	// 上限不同就不能复用：同一张图在不同模型预算下的结果是不同的。
	other := providerMedia{StorageKey: "resource:ref-cache", MimeType: "image/png"}
	if err := s.hydrateProviderMedia("user", &other, providerMediaHydrationPolicy{imageOnly: true, maxBytes: 300 * 1024}); err == nil && other.DataURL == first.DataURL {
		t.Fatal("a different model limit reused the wrong compressed copy")
	}
}

// 看图的图片必然内联真实像素：签名 URL 交付会让上游取回原始字节，压缩与缓存全部失效。
// hydrateGenerationMedia 会用 imageOnly 覆盖 preferURL，这条守卫是给直接传组合策略的调用方兜底。
func TestHydrateProviderMediaRejectsImageOnlyWithPreferredObjectURL(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	if err := db.Create(&model.Resource{ID: "ref-oss", UserID: "user", Kind: "image", Status: model.ResourceStatusReady, Provider: "aliyun", Bucket: "private-bucket", ObjectKey: "users/user/image/ref-oss.png", MimeType: "image/png", Size: int64(len(original)), Width: 1400, Height: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	media := providerMedia{StorageKey: "resource:ref-oss", MimeType: "image/png"}
	err := s.hydrateProviderMedia("user", &media, providerMediaHydrationPolicy{imageOnly: true, preferURL: true, maxBytes: 30 << 20})
	if !errors.Is(err, errTextModelImageLimitNeedsPixels) {
		t.Fatalf("imageOnly must not hand the upstream a signed object URL: %v", err)
	}
	if media.URL != "" {
		t.Fatal("the guarded path must not fill in a signed URL")
	}
	// 去掉 imageOnly（普通生成任务）时仍按原协议优先公网地址，不受这条守卫影响。
	plain := providerMedia{StorageKey: "resource:ref-oss", MimeType: "image/png"}
	if err := s.hydrateProviderMedia("user", &plain, providerMediaHydrationPolicy{preferURL: true}); err != nil {
		t.Skipf("本用例夹具未配置对象存储密钥，公网地址分支无法在此验证：%v", err)
	}
	if plain.URL == "" || plain.DataURL != "" {
		t.Fatalf("non-vision hydration lost its preferred object URL: %#v", plain)
	}
}

func TestHydrateGenerationMediaUnlimitedModelKeepsLargeImage(t *testing.T) {
	s, db := textModelTestService(t)
	original := textModelTestNoisyPNG(t, 900, 700, false)
	textModelTestResource(t, s, db, "ref-unlimited", original)
	media := providerMedia{StorageKey: "resource:ref-unlimited", MimeType: "image/png"}
	if err := s.hydrateProviderMedia("user", &media, providerMediaHydrationPolicy{imageOnly: true, maxBytes: 0}); err != nil {
		t.Fatalf("an unlimited model must not be rejected by the compression hard cap: %v", err)
	}
	if media.DataURL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(original) {
		t.Fatal("an unlimited model must receive the original bytes")
	}
}

func TestEcommerceVisionImagesFollowRecognitionModelLimit(t *testing.T) {
	original := textModelTestNoisyPNG(t, 1400, 1000, false)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)
	provider := providerConfig{CapabilityConfig: &ModelCapabilityConfig{Text: &TextCapabilityConfig{References: TextReferenceConfig{MaxImageBytes: 800 * 1024}}}}
	cfg := ecommerceProviderConfig{TextProvider: &provider}

	prepared := ecommerceVisionImages(cfg, []string{dataURL})
	if len(prepared) != 1 || !strings.HasPrefix(prepared[0], "data:image/jpeg;base64,") {
		t.Fatalf("vision copy was not compressed: %q", prepared[0][:min(len(prepared[0]), 48)])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(prepared[0], "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(decoded)) > 800*1024 {
		t.Fatalf("vision copy is %d bytes", len(decoded))
	}

	// 同一条链路：上限放得比图大时不压缩，字节原样。
	roomy := providerConfig{CapabilityConfig: &ModelCapabilityConfig{Text: &TextCapabilityConfig{References: TextReferenceConfig{MaxImageBytes: 30 << 20}}}}
	if got := ecommerceVisionImages(ecommerceProviderConfig{TextProvider: &roomy}, []string{dataURL}); got[0] != dataURL {
		t.Fatal("an image inside the recognition model limit must be forwarded byte for byte")
	}
	// 没有模型能力配置（旧的内置文本渠道）时不压缩，保持既有行为。
	if got := ecommerceVisionImages(ecommerceProviderConfig{}, []string{dataURL}); got[0] != dataURL {
		t.Fatal("a provider without a capability profile must keep the original data URL")
	}
}
