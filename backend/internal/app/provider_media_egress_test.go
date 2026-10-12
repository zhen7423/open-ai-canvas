package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newProviderEgressTestService(t *testing.T) (*Service, string) {
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
	if err := db.AutoMigrate(&model.SystemSetting{}, &model.UserOSSSetting{}, &model.StorageLocation{},
		&model.UserDailyUploadUsage{}, &model.Resource{}, &model.UploadReservation{},
		&model.ProviderEgressObject{}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	return &Service{repo: repository.New(db), dataDir: dataDir}, dataDir
}

// seedLocalImage 造一张指定字节的本地素材，返回落盘字节（测试要拿它比对内联还原结果）。
func seedLocalImage(t *testing.T, svc *Service, dataDir string, id string, userID string, payload []byte) []byte {
	t.Helper()
	objectKey := "users/" + userID + "/image/" + id + ".png"
	resource := model.Resource{
		ID: id, UserID: userID, Kind: "image", Status: model.ResourceStatusReady,
		Provider: "local", ObjectKey: objectKey, MimeType: "image/png", Size: int64(len(payload)),
		Width: 4, Height: 6, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := svc.repo.CreateResource(&resource); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "resources", filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	return payload
}

// seedSizedImage 按指定字节数与 MIME 造一张本地素材，返回写盘的字节。
// 内联预算是"字节数"判据，所以边界测试必须能精确控制大小与内容。
func seedSizedImage(t *testing.T, svc *Service, dataDir string, id string, userID string, fill byte, size int, mimeType string) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte{fill}, size)
	extension := ".png"
	if mimeType != "image/png" {
		extension = ".img"
	}
	objectKey := "users/" + userID + "/image/" + id + extension
	resource := model.Resource{
		ID: id, UserID: userID, Kind: "image", Status: model.ResourceStatusReady,
		Provider: "local", ObjectKey: objectKey, MimeType: mimeType, Size: int64(len(payload)),
		Width: 4, Height: 6, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := svc.repo.CreateResource(&resource); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "resources", filepath.FromSlash(objectKey))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		t.Fatal(err)
	}
	return payload
}

func configureProviderEgress(t *testing.T, svc *Service) {
	t.Helper()
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}
}

type egressUploadProbe struct {
	objectKeys []string
	sizes      []int64
	mimeTypes  []string
	rangeCalls int
}

// captureEgressUploads 把 PUT、存在性探测与签名换成可控实现。
// PUT 桩里保留 io.ReadAll 并比对 size，是为了证明字节真的流到了上传调用，而不是只报了个键名。
func captureEgressUploads(t *testing.T, signedURL string) *egressUploadProbe {
	t.Helper()
	previousPut := putOSSObject
	previousSign := signedOSSObjectURL
	previousRange := getOSSObjectRange
	previousCDN := cdnEnabled
	probe := &egressUploadProbe{}
	putOSSObject = func(setting ossSettingValue, objectKey string, mimeType string, size int64, body io.Reader) (string, error) {
		data, err := io.ReadAll(body)
		if err != nil {
			return "", err
		}
		if int64(len(data)) != size || len(data) == 0 {
			return "", fmt.Errorf("PUT 字节数 %d 与声明 %d 不符", len(data), size)
		}
		probe.objectKeys = append(probe.objectKeys, objectKey)
		probe.sizes = append(probe.sizes, size)
		probe.mimeTypes = append(probe.mimeTypes, mimeType)
		return `"etag"`, nil
	}
	signedOSSObjectURL = func(setting ossSettingValue, objectKey string, expiresAt time.Time) (string, error) {
		if !expiresAt.After(time.Now().UTC()) {
			return "", fmt.Errorf("签名地址没有未来过期时间：%v", expiresAt)
		}
		return signedURL + "?sig=mock&expires=" + strings.ToLower(expiresAt.Format(time.RFC3339)), nil
	}
	getOSSObjectRange = func(setting ossSettingValue, objectKey string, rangeHeader string) (*ossObjectStream, error) {
		probe.rangeCalls++
		return &ossObjectStream{Body: io.NopCloser(strings.NewReader("x")), StatusCode: http.StatusPartialContent}, nil
	}
	cdnEnabled = func(setting ossSettingValue) bool { return false }
	t.Cleanup(func() {
		putOSSObject = previousPut
		signedOSSObjectURL = previousSign
		getOSSObjectRange = previousRange
		cdnEnabled = previousCDN
	})
	return probe
}

func videoEgressInput(resourceID string, originalURL string) canvasGenerationInput {
	return canvasGenerationInput{
		Mode: "video",
		ReferenceImages: []providerMedia{{
			ID: "m-1", StorageKey: "resource:" + resourceID, URL: originalURL,
		}},
	}
}

func videoEgressInputOf(items ...providerMedia) canvasGenerationInput {
	return canvasGenerationInput{Mode: "video", ReferenceImages: items}
}

func requireURLNeed() providerMediaRequirement {
	return providerMediaRequirement{
		policy: providerMediaHydrationPolicy{requireURL: true, preferURL: true},
		reason: "内置视频协议要求参考素材可被上游取回",
	}
}

// dataURLSummary 供失败信息使用：内联正文可达十几 MB，绝不能整段打进测试日志。
func dataURLSummary(media providerMedia) string {
	head, body, ok := strings.Cut(media.DataURL, ",")
	if !ok {
		return fmt.Sprintf("非 data URI（%d 字节）", len(media.DataURL))
	}
	return fmt.Sprintf("%s,<%d 字节 Base64>", head, len(body))
}

// dataURLMediaType 取 `data:<媒体类型>;base64,` 里的媒体类型段。
func dataURLMediaType(t *testing.T, media providerMedia) string {
	t.Helper()
	head, _, ok := strings.Cut(media.DataURL, ",")
	if !ok {
		t.Fatalf("内联素材不是 data URI：%s", dataURLSummary(media))
	}
	if !strings.HasPrefix(head, "data:") || !strings.HasSuffix(head, ";base64") {
		t.Fatalf("内联素材头部不符：%q", head)
	}
	return strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")
}

func decodedDataURL(t *testing.T, media providerMedia) []byte {
	t.Helper()
	_, body, ok := strings.Cut(media.DataURL, ",")
	if !ok {
		t.Fatalf("内联素材缺少数据分隔符：%s", dataURLSummary(media))
	}
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("内联素材的 Base64 解不开：%v", err)
	}
	return decoded
}

func shaHexOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// 贴预算边界的夹具：字节数直接由常量推出，改一个预算数字就有一条测试变红。
func atInlinePayloadSize() int   { return int(providerEgressMaxInlineImageBytes) }
func overInlinePayloadSize() int { return int(providerEgressMaxInlineImageBytes) + 1 }

// halfTotalInlineSize 用来构造"单张都不超、只有合计超"的形态：
// 两张各取合计预算的一半，加一字节则恰好越过合计线而仍在单张预算内。
func halfTotalInlineSize() int { return int(providerEgressMaxInlineTotalBytes / 2) }

func TestPreparePublicEgressImageReferencesInlinesSmallImages(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	// 桶配好了也应当优先内联：小图根本不需要一次云上传和一次签名。
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	payload := seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("fake-png-bytes"))

	const original = "http://192.168.1.167:1888/api/public/resources/res-1/file?expires=1&signature=abc"
	input := videoEgressInput("res-1", original)
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}

	media := input.ReferenceImages[0]
	if len(probe.objectKeys) != 0 {
		t.Fatalf("预算内的小图不该上传到桶：%#v", probe.objectKeys)
	}
	if media.URL != "" {
		t.Fatalf("内联后必须清掉上游取不到的地址：%q", media.URL)
	}
	if got := dataURLMediaType(t, media); got != "image/png" {
		t.Fatalf("内联头部媒体类型 = %q", got)
	}
	if !bytes.Equal(decodedDataURL(t, media), payload) {
		t.Fatal("Base64 还原出的字节与素材不一致")
	}
	if media.MimeType != "image/png" {
		t.Fatalf("MIME = %q", media.MimeType)
	}
	if media.Bytes != int64(len(payload)) {
		t.Fatalf("字节数 = %d", media.Bytes)
	}
	if media.StorageKey != "resource:res-1" {
		t.Fatalf("存储键要留着，任务重试还得靠它定位资源：%q", media.StorageKey)
	}

	records, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("内联路径不该写镜像台账：%#v", records)
	}
}

// 上游文档写的是 `data:image/<格式>;base64,<Base64>` 且注明"<格式> 小写"，
// 而资源记录里的 MimeType 可能是历史大写值。
func TestPreparePublicEgressImageReferencesLowercasesInlineMediaType(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'x', 7, "Image/PNG")

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("小图却发生了上传：%#v", probe.objectKeys)
	}
	media := input.ReferenceImages[0]
	if got := dataURLMediaType(t, media); got != "image/png" {
		t.Fatalf("内联头部媒体类型必须小写，实际 %q（%s）", got, dataURLSummary(media))
	}
	// 结构化字段仍按资源记录如实回写，别把上游协议要求"顺手"改到数据层。
	if media.MimeType != "Image/PNG" {
		t.Fatalf("MIME 字段不该被改写：%q", media.MimeType)
	}
}

func TestPreparePublicEgressImageReferencesInlinesWhenBucketMissing(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	// 完全没配出站内镜：这正是原来必然失败的形态，小图现在靠内联直接可用。
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	payload := seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("fake-png-bytes"))

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatalf("未配置桶时小图必须能内联成功：%v", err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("没有桶却发生了上传：%#v", probe.objectKeys)
	}
	if !bytes.Equal(decodedDataURL(t, input.ReferenceImages[0]), payload) {
		t.Fatal("内联字节不符")
	}
}

func TestPreparePublicEgressImageReferencesInlinesWhenBaseUnset(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	// 没配服务器访问地址时根本签不出可用链接，小图靠内联即可交付。
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("小图不该走镜像：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].DataURL == "" {
		t.Fatal("未配置 base 时小图应被内联")
	}
}

func TestPreparePublicEgressImageReferencesHonoursPerImageInlineLimit(t *testing.T) {
	t.Run("贴线单张仍内联", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
		payload := seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'A', atInlinePayloadSize(), "image/png")

		input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatal(err)
		}
		if len(probe.objectKeys) != 0 {
			t.Fatalf("单张恰好在预算内却上传了：%#v", probe.objectKeys)
		}
		if len(decodedDataURL(t, input.ReferenceImages[0])) != len(payload) {
			t.Fatal("内联字节数与素材不符")
		}
	})

	// 一个字节之差就要换路径，否则预算判断等于没写。
	t.Run("超出一字节转镜像", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
		seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'B', overInlinePayloadSize(), "image/png")

		input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatal(err)
		}
		if len(probe.objectKeys) != 1 {
			t.Fatalf("超出单张预算应当镜像一次：%#v", probe.objectKeys)
		}
		media := input.ReferenceImages[0]
		if !strings.HasPrefix(media.URL, "https://yingce-egress.") {
			t.Fatalf("镜像后地址 = %q", media.URL)
		}
		if media.DataURL != "" {
			t.Fatalf("给出公网地址后不得再带内嵌数据：%s", dataURLSummary(media))
		}
	})
}

func TestPreparePublicEgressImageReferencesHonoursTotalInlineBudget(t *testing.T) {
	t.Run("合计贴线全部内联", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
		// 合计贴线用「合计预算的一半 × 2」，不能拿单张贴线当夹具：
		// 单张预算（30 MB）大于合计预算（40 MB）的一半，两张贴线必然破合计线。
		half := halfTotalInlineSize()
		first := seedSizedImage(t, svc, dataDir, "res-a", "user-1", 'A', half, "image/png")
		second := seedSizedImage(t, svc, dataDir, "res-b", "user-1", 'B', half, "image/png")

		input := videoEgressInputOf(
			providerMedia{ID: "m-1", StorageKey: "resource:res-a", URL: "http://192.168.1.167:1888/a"},
			providerMedia{ID: "m-2", StorageKey: "resource:res-b", URL: "http://192.168.1.167:1888/b"},
		)
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatal(err)
		}
		if len(probe.objectKeys) != 0 {
			t.Fatalf("合计在预算内却上传了：%#v", probe.objectKeys)
		}
		if !bytes.Equal(decodedDataURL(t, input.ReferenceImages[0]), first) ||
			!bytes.Equal(decodedDataURL(t, input.ReferenceImages[1]), second) {
			t.Fatal("两张素材都要各自内联正确")
		}
	})

	// 单张都没超，合计超了也要整体镜像：内联是"整请求"级别的判断。
	t.Run("合计超预算整体镜像", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
		// 夹具由合计预算推出：两张各「合计/2 + 1 字节」，单张都远在单张预算内，只有总和越线。
		sizes := []int{halfTotalInlineSize() + 1, halfTotalInlineSize() + 1}
		for _, size := range sizes {
			if int64(size) > providerEgressMaxInlineImageBytes {
				t.Fatalf("夹具单张 %d 字节已越过单张预算，这条用例证明不了「只看合计」", size)
			}
		}
		if int64(len(sizes))*int64(sizes[0]) <= providerEgressMaxInlineTotalBytes {
			t.Fatalf("夹具合计 %d 字节仍在预算内，这条用例证明不了超线镜像", int64(len(sizes))*int64(sizes[0]))
		}
		fills := []byte{'A', 'B', 'C'}
		ids := []string{"res-a", "res-b", "res-c"}
		items := make([]providerMedia, 0, len(sizes))
		for index, size := range sizes {
			seedSizedImage(t, svc, dataDir, ids[index], "user-1", fills[index], size, "image/png")
			items = append(items, providerMedia{
				ID: fmt.Sprintf("m-%d", index+1), StorageKey: "resource:" + ids[index],
				URL: "http://192.168.1.167:1888/" + ids[index],
			})
		}

		input := videoEgressInputOf(items...)
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatal(err)
		}
		if len(probe.objectKeys) != len(sizes) {
			t.Fatalf("合计超预算应当 %d 张全部镜像：%#v", len(sizes), probe.objectKeys)
		}
		seen := map[string]bool{}
		for index, key := range probe.objectKeys {
			if seen[key] {
				t.Fatalf("不同内容落到同一对象键：%q", key)
			}
			seen[key] = true
			if probe.sizes[index] != int64(sizes[index]) {
				t.Fatalf("第 %d 张上传字节 = %d", index, probe.sizes[index])
			}
		}
		for index := range input.ReferenceImages {
			media := &input.ReferenceImages[index]
			if !strings.HasPrefix(media.URL, "https://yingce-egress.") {
				t.Fatalf("第 %d 项未改写为内镜地址：%q", index, media.URL)
			}
			if media.DataURL != "" {
				t.Fatalf("第 %d 项镜像后仍带内嵌数据：%s", index, dataURLSummary(*media))
			}
		}
	})
}

func TestProviderEgressMirrorWritesLedgerWithUnguessableObjectKey(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.oss-cn-beijing.aliyuncs.com/canvas/egress/user-1/img.png")
	payload := seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'M', overInlinePayloadSize(), "image/png")

	const privateURL = "http://192.168.1.167:1888/api/public/resources/res-1/file?expires=1&signature=abc"
	input := videoEgressInput("res-1", privateURL)
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	got := input.ReferenceImages[0].URL
	if !strings.HasPrefix(got, "https://yingce-egress.") {
		t.Fatalf("改写后的地址 = %q", got)
	}
	if !strings.Contains(got, "?sig=mock") {
		t.Fatalf("签发结果未进入地址：%q", got)
	}
	if len(probe.objectKeys) != 1 {
		t.Fatalf("PUT 次数 = %d, want 1：%#v", len(probe.objectKeys), probe.objectKeys)
	}
	key := probe.objectKeys[0]
	// 对象名必须是 <前缀>/egress/<userID>/<32位十六进制 HMAC>.png，且不得是像素 SHA 本身：
	// 用 sha256(像素) 当对象名等于让任何看过原图的人推出永久地址。
	shaHex := shaHexOf(payload)
	if !strings.HasPrefix(key, "canvas/egress/user-1/") {
		t.Fatalf("对象键前缀不符：%q", key)
	}
	if !strings.HasSuffix(key, ".png") {
		t.Fatalf("对象键未按 MIME 取扩展名：%q", key)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, "canvas/egress/user-1/"), ".png")
	if len(name) != 32 || strings.IndexFunc(name, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) >= 0 {
		t.Fatalf("随机段必须是 32 位十六进制：%q", name)
	}
	if name == shaHex[:32] {
		t.Fatal("对象名不得直接是内容 SHA 的前缀")
	}
	if strings.Contains(key, shaHex) {
		t.Fatalf("对象键不得含内容指纹：%q", key)
	}
	if probe.sizes[0] != int64(len(payload)) {
		t.Fatalf("声明字节 = %d", probe.sizes[0])
	}
	if probe.mimeTypes[0] != "image/png" {
		t.Fatalf("上传 MIME = %q", probe.mimeTypes[0])
	}

	// 同图第二次不得再 PUT。
	second := videoEgressInput("res-1", got)
	if err := svc.preparePublicEgressImageReferences("user-1", &second, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 1 {
		t.Fatalf("命中台账后仍重复上传：%#v", probe.objectKeys)
	}
	if probe.rangeCalls != 0 {
		t.Fatalf("24 小时内的复用不该做存在性探测：%d", probe.rangeCalls)
	}
	if second.ReferenceImages[0].URL != got {
		t.Fatalf("复用后地址漂移：%q", second.ReferenceImages[0].URL)
	}

	records, err := svc.repo.ProviderEgressObjectsExpired(time.Now().Add(10*time.Minute), "aliyun", "yingce-egress", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("台账行数 = %d, want 1", len(records))
	}
	if records[0].ResourceID != "res-1" || records[0].Bucket != "yingce-egress" || records[0].Provider != "aliyun" {
		t.Fatalf("台账内容不符：%#v", records[0])
	}
	if records[0].ContentSHA256 != shaHex {
		t.Fatalf("台账指纹与字节实算不符：%q", records[0].ContentSHA256)
	}
	if len(records[0].ContentSHA256) != 64 {
		t.Fatalf("SHA 长度 = %d：%q", len(records[0].ContentSHA256), records[0].ContentSHA256)
	}
}

// CDN 公有读给出的地址不带签名参数，等于把可长期读取的公网地址交给第三方，
// 所以"走 CDN"和"CDN 签名不可用时回源签名"两条都必须有断言，不能只测回源那一支。
func TestPreparePublicEgressImageAccessURLUsesCDNBranch(t *testing.T) {
	t.Run("CDN 可用时不签源站", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		captureEgressUploads(t, "https://origin.example.com/x.png")
		seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'C', overInlinePayloadSize(), "image/png")

		previousCDN := cdnEnabled
		previousSignCDN := signCDNURL
		previousSign := signedOSSObjectURL
		originSignCalls := 0
		cdnEnabled = func(ossSettingValue) bool { return true }
		signCDNURL = func(ossSettingValue, string, time.Time) (string, error) {
			return "https://cdn.example.com/canvas/egress/user-1/img.png", nil
		}
		signedOSSObjectURL = func(ossSettingValue, string, time.Time) (string, error) {
			originSignCalls++
			return "https://origin.example.com/signed", nil
		}
		t.Cleanup(func() {
			cdnEnabled = previousCDN
			signCDNURL = previousSignCDN
			signedOSSObjectURL = previousSign
		})

		input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatal(err)
		}
		if got := input.ReferenceImages[0].URL; got != "https://cdn.example.com/canvas/egress/user-1/img.png" {
			t.Fatalf("CDN 可用时应给出无查询参数的 CDN 地址：%q", got)
		}
		if originSignCalls != 0 {
			t.Fatalf("CDN 可用却仍签了源站地址：%d 次", originSignCalls)
		}
	})

	t.Run("CDN 签名不可用时回源签名", func(t *testing.T) {
		svc, dataDir := newProviderEgressTestService(t)
		configureProviderEgress(t, svc)
		t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
		captureEgressUploads(t, "https://origin.example.com/x.png")
		seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'D', overInlinePayloadSize(), "image/png")

		previousCDN := cdnEnabled
		previousSignCDN := signCDNURL
		previousSign := signedOSSObjectURL
		cdnEnabled = func(ossSettingValue) bool { return true }
		signCDNURL = func(ossSettingValue, string, time.Time) (string, error) {
			return "", errors.New("CDN 用户访问鉴权方式未配置或不支持")
		}
		signedOSSObjectURL = func(ossSettingValue, string, time.Time) (string, error) {
			return "https://yingce-egress.oss-cn-beijing.aliyuncs.com/canvas/egress/user-1/img.png?sig=mock", nil
		}
		t.Cleanup(func() {
			cdnEnabled = previousCDN
			signCDNURL = previousSignCDN
			signedOSSObjectURL = previousSign
		})

		input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
		if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
			t.Fatalf("CDN 签名不支持时应当回源签名而不是失败：%v", err)
		}
		if got := input.ReferenceImages[0].URL; !strings.HasPrefix(got, "https://yingce-egress.") || !strings.Contains(got, "?sig=mock") {
			t.Fatalf("未回落到源站签名地址：%q", got)
		}
	})
}

func TestPreparePublicEgressImageReferencesReuploadsWhenObjectGone(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'G', overInlinePayloadSize(), "image/png")

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 1 {
		t.Fatalf("首次应当上传：%#v", probe.objectKeys)
	}

	// 把台账伪装成很久没用过，并让存在性探测报"对象不存在"，模拟桶被外部清空。
	records, err := svc.repo.ProviderEgressObjectsExpired(time.Now().Add(time.Hour), "aliyun", "yingce-egress", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("台账行数 = %d", len(records))
	}
	if err := svc.repo.TouchProviderEgressObject(records[0].ID, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	previousRange := getOSSObjectRange
	getOSSObjectRange = func(setting ossSettingValue, objectKey string, rangeHeader string) (*ossObjectStream, error) {
		probe.rangeCalls++
		return nil, fmt.Errorf("no such object")
	}
	t.Cleanup(func() { getOSSObjectRange = previousRange })

	second := videoEgressInput("res-1", input.ReferenceImages[0].URL)
	if err := svc.preparePublicEgressImageReferences("user-1", &second, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if probe.rangeCalls != 1 {
		t.Fatalf("超窗复用应当探测一次：%d", probe.rangeCalls)
	}
	if len(probe.objectKeys) != 2 {
		t.Fatalf("对象确实不在时应当重传：%#v", probe.objectKeys)
	}
	if probe.objectKeys[0] != probe.objectKeys[1] {
		t.Fatalf("同内容应当落在同一对象键：%q vs %q", probe.objectKeys[0], probe.objectKeys[1])
	}
}

func TestPreparePublicEgressImageReferencesSkips(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		requireURL bool
		mimeType   string
		provider   string
		expectPuts int
	}{
		{name: "非视频模式", mode: "image", requireURL: true, mimeType: "image/png", provider: "local", expectPuts: 0},
		{name: "渠道不要求公网地址", mode: "video", requireURL: false, mimeType: "image/png", provider: "local", expectPuts: 0},
		{name: "非图片素材", mode: "video", requireURL: true, mimeType: "video/mp4", provider: "local", expectPuts: 0},
		{name: "已在对象存储", mode: "video", requireURL: true, mimeType: "image/png", provider: "aliyun", expectPuts: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			svc, dataDir := newProviderEgressTestService(t)
			configureProviderEgress(t, svc)
			t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
			probe := captureEgressUploads(t, "https://ignored.example.com/x.png")

			extension := ".png"
			if testCase.mimeType == "video/mp4" {
				extension = ".mp4"
			}
			objectKey := "users/user-1/media/skip" + extension
			resource := model.Resource{
				ID: "res-skip", UserID: "user-1", Kind: "image", Status: model.ResourceStatusReady,
				Provider: testCase.provider, ObjectKey: objectKey, MimeType: testCase.mimeType, Size: 7,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			if testCase.provider == "aliyun" {
				resource.Endpoint = "oss-cn-beijing.aliyuncs.com"
				resource.Bucket = "media"
			}
			if err := svc.repo.CreateResource(&resource); err != nil {
				t.Fatal(err)
			}
			if testCase.provider == "local" {
				path := filepath.Join(dataDir, "resources", filepath.FromSlash(objectKey))
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("payload"), 0o640); err != nil {
					t.Fatal(err)
				}
			}

			const keep = "http://192.168.1.167:1888/keep"
			input := videoEgressInput("res-skip", keep)
			input.Mode = testCase.mode
			requirement := providerMediaRequirement{policy: providerMediaHydrationPolicy{requireURL: testCase.requireURL}}
			if err := svc.preparePublicEgressImageReferences("user-1", &input, requirement); err != nil {
				t.Fatal(err)
			}
			if len(probe.objectKeys) != testCase.expectPuts {
				t.Fatalf("PUT 次数 = %d, want %d", len(probe.objectKeys), testCase.expectPuts)
			}
			if input.ReferenceImages[0].URL != keep {
				t.Fatalf("不该改写时被改写：%q", input.ReferenceImages[0].URL)
			}
			if input.ReferenceImages[0].DataURL != "" {
				t.Fatalf("不该内联时被内联：%s", dataURLSummary(input.ReferenceImages[0]))
			}
			if input.ReferenceImages[0].StorageKey != "resource:res-skip" {
				t.Fatal("存储键不应被清掉")
			}
		})
	}
}

func TestPreparePublicEgressImageReferencesSkipsWhenBaseIsPublic(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	// 公网形态的 base（穿隧道或已上公网）：判定不看 DNS，只看域名形态，所以这里不需要真服务器。
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "https://canvas.trycloudflare.com")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	const original = "https://canvas.trycloudflare.com/api/public/resources/res-1/file?expires=1&signature=abc"
	input := videoEgressInput("res-1", original)
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("base 已公网可达却仍镜像：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].URL != original {
		t.Fatalf("不该改写时被改写：%q", input.ReferenceImages[0].URL)
	}
	// 上游已经拿得到地址，就别再撑请求体。
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatalf("base 已公网可达时不该内联：%s", dataURLSummary(input.ReferenceImages[0]))
	}
}

func TestPreparePublicEgressImageBucketMissingFailsLoudlyWithInlineContext(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	// 没配桶：小图本来照样能内联，只有超出预算的才必须报错。
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'N', overInlinePayloadSize(), "image/png")

	const original = "http://192.168.1.167:1888/x"
	input := videoEgressInput("res-1", original)
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	if err == nil {
		t.Fatal("超出内联预算且桶未配置时必须明确失败")
	}
	message := err.Error()
	for _, want := range []string{"出站内镜", "超过可内联的上限", "未启用或未配置出站内镜"} {
		if !strings.Contains(message, want) {
			t.Fatalf("报错缺少 %q，实际：%s", want, message)
		}
	}
	// 报错要说明是哪个判定要求素材可取回，管理员才知道不是渠道配错了。
	if !strings.Contains(message, "内置视频协议要求参考素材可被上游取回") {
		t.Fatalf("失败原因要说明是哪个判定要求公网地址：%s", message)
	}
	// 前端提示里不能出现桶名/密钥线索。
	for _, leak := range []string{"AKID", "secret", "yingce-egress", "aliyuncs"} {
		if strings.Contains(message, leak) {
			t.Fatalf("报错泄露了 %q：%s", leak, message)
		}
	}
	if len(probe.objectKeys) != 0 {
		t.Fatal("未配置内镜却发生了上传")
	}
	if input.ReferenceImages[0].URL != original {
		t.Fatalf("失败路径不得改写地址：%q", input.ReferenceImages[0].URL)
	}
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatal("失败路径不得留下半套内联数据")
	}
}

func TestPreparePublicEgressImageReferencesIgnoresForeignStorageKeys(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")

	input := canvasGenerationInput{Mode: "video", ReferenceImages: []providerMedia{
		{ID: "m-1", StorageKey: "", URL: "https://public.example.com/a.png"},
		{ID: "m-2", StorageKey: "resource:", URL: "http://192.168.1.167:1888/b"},
		{ID: "m-3", StorageKey: "upload:whatever", URL: "http://192.168.1.167:1888/c"},
	}}
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	wantURLs := []string{
		"https://public.example.com/a.png",
		"http://192.168.1.167:1888/b",
		"http://192.168.1.167:1888/c",
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("非本仓库资源引用不该被镜像：%#v", probe.objectKeys)
	}
	for index := range input.ReferenceImages {
		got := input.ReferenceImages[index].URL
		if got != wantURLs[index] {
			t.Fatalf("第 %d 项地址被改写：%q, want %q", index, got, wantURLs[index])
		}
		if input.ReferenceImages[index].DataURL != "" {
			t.Fatalf("第 %d 项被内联：%s", index, dataURLSummary(input.ReferenceImages[index]))
		}
	}
}

func TestPreparePublicEgressImageReferencesLeavesMaskPathAlone(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	mask := &providerMedia{ID: "mask", StorageKey: "resource:res-1", URL: "http://192.168.1.167:1888/mask"}
	input.Mask = mask
	requirement := requireURLNeed()
	// 遮罩场景下 hydration 会把 requireURL 关掉；这里显式模拟"带遮罩的请求"。
	requirement.policy.requireURL = true
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requirement); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("带遮罩的请求不该走出站内镜：%#v", probe.objectKeys)
	}
	// 遮罩本身和参考图地址都必须原样保留：multipart 路径要的是真实字节，不是 URL。
	if mask.URL != "http://192.168.1.167:1888/mask" {
		t.Fatalf("遮罩地址被改写：%q", mask.URL)
	}
	if input.ReferenceImages[0].URL != "http://192.168.1.167:1888/x" {
		t.Fatalf("参考图地址被改写：%q", input.ReferenceImages[0].URL)
	}
	if mask.DataURL != "" || input.ReferenceImages[0].DataURL != "" {
		t.Fatal("遮罩路径不得被注入内嵌数据")
	}
}

func TestPreparePublicEgressImageReferencesLeavesVendorAssetRefsAlone(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	// 方舟可信素材由 prepareArkPrivateAssetReferences 改写成 asset://，并保留 resource: 存储键。
	// 出站内镜若覆盖它，等于把已经谈好的可信素材路径打回普通外链。
	input := videoEgressInput("res-1", "asset://424010985738629")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if input.ReferenceImages[0].URL != "asset://424010985738629" {
		t.Fatalf("厂商私有引用被覆盖：%q", input.ReferenceImages[0].URL)
	}
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatalf("asset:// 素材不该被内联：%s", dataURLSummary(input.ReferenceImages[0]))
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("asset:// 素材不该再镜像：%#v", probe.objectKeys)
	}
}

func TestPreparePublicEgressImageReferencesSkipsPrivateChannel(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	// 自建 NewAPI / 内网代理这类渠道本身就在地网，它取得到我们签出的内网地址。
	input := videoEgressInput("res-1", "http://192.168.1.167:1888/api/public/resources/res-1/file")
	input.Config = providerConfig{BaseURL: "http://192.168.1.50:3000"}
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("内网渠道不该强制内镜：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].URL != "http://192.168.1.167:1888/api/public/resources/res-1/file" {
		t.Fatalf("内网渠道场景地址被改写：%q", input.ReferenceImages[0].URL)
	}
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatalf("内网渠道场景不该内联，白撑请求体：%s", dataURLSummary(input.ReferenceImages[0]))
	}
}

// 渠道地址没写 scheme（`192.168.1.50:3000`）时 url.Parse 解析不出 Host。
// 若因此当作公网渠道，就会对本来能取到内网链接的自建中转强行内联/镜像——正是这条守卫要避免的回归。
func TestPreparePublicEgressImageReferencesSkipsSchemelessPrivateChannel(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedLocalImage(t, svc, dataDir, "res-1", "user-1", []byte("payload"))

	const original = "http://192.168.1.167:1888/api/public/resources/res-1/file"
	input := videoEgressInput("res-1", original)
	input.Config = providerConfig{BaseURL: "192.168.1.50:3000"}
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("无 scheme 的内网渠道也被处理了：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].URL != original || input.ReferenceImages[0].DataURL != "" {
		t.Fatalf("内网渠道的地址与内联字段都该原样保留：url=%q", input.ReferenceImages[0].URL)
	}
}

// 签发失败同样来自云 SDK，报错带接入点与密钥线索；任务正文会被持久化并展示，必须与上传失败一样脱敏。
func TestPreparePublicEgressImageSigningFailureHidesVendorDetail(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'S', overInlinePayloadSize(), "image/png")

	previousSign := signedOSSObjectURL
	signedOSSObjectURL = func(ossSettingValue, string, time.Time) (string, error) {
		return "", errors.New("SignatureDoesNotMatch: host=yingce-egress.oss-cn-beijing.aliyuncs.com, AKId=AKID")
	}
	t.Cleanup(func() { signedOSSObjectURL = previousSign })

	const original = "http://192.168.1.167:1888/keep"
	input := videoEgressInput("res-1", original)
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	if err == nil {
		t.Fatal("签发失败必须向上抛出")
	}
	message := err.Error()
	for _, leak := range []string{"AKID", "oss-cn-beijing", "yingce-egress", "aliyuncs"} {
		if strings.Contains(message, leak) {
			t.Fatalf("任务失败正文泄露了 %q：%s", leak, message)
		}
	}
	if !strings.Contains(message, "签发访问地址失败") {
		t.Fatalf("报错要指向签发环节：%s", message)
	}
	if input.ReferenceImages[0].URL != original {
		t.Fatalf("失败时不得改写地址：%q", input.ReferenceImages[0].URL)
	}
}

func TestPreparePublicEgressImageUploadFailureHidesVendorDetail(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	captureEgressUploads(t, "https://ignored.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'U', overInlinePayloadSize(), "image/png")

	previousPut := putOSSObject
	putOSSObject = func(ossSettingValue, string, string, int64, io.Reader) (string, error) {
		// 厂商 SDK 的报错 routinely 带接入点、HostId 与密钥线索。
		return "", errors.New("AccessDenied: host=yingce-egress.oss-cn-beijing.aliyuncs.com, AKId=AKID, please check")
	}
	t.Cleanup(func() { putOSSObject = previousPut })

	const original = "http://192.168.1.167:1888/keep"
	input := videoEgressInput("res-1", original)
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	if err == nil {
		t.Fatal("上传失败必须向上抛出")
	}
	for _, leak := range []string{"AKID", "oss-cn-beijing", "yingce-egress", "aliyun"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("任务失败正文泄露了 %q：%v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "出站内镜") {
		t.Fatalf("报错要能指向出站内镜配置：%v", err)
	}
	if input.ReferenceImages[0].URL != original {
		t.Fatalf("失败时不得改写地址：%q", input.ReferenceImages[0].URL)
	}
}

func TestPreparePublicEgressImageRotationUsesDistinctKeys(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	seedSizedImage(t, svc, dataDir, "res-1", "user-1", 'R', overInlinePayloadSize(), "image/png")

	input := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	firstKey := probe.objectKeys[0]

	// 换密钥会改变投递指纹。若两次落在同一个对象键上，旧行到期时清理会删掉新行仍在用的对象。
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", AccessKeySecret: "secret-rotated", PathPrefix: "canvas",
	}); err != nil {
		t.Fatal(err)
	}
	second := videoEgressInput("res-1", "http://192.168.1.167:1888/x")
	if err := svc.preparePublicEgressImageReferences("user-1", &second, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 2 {
		t.Fatalf("换密钥后应重新上传一份：%#v", probe.objectKeys)
	}
	if probe.objectKeys[1] == firstKey {
		t.Fatalf("两个投递指纹必须落在不同对象键，否则旧行到期会删掉在用对象：%q", probe.objectKeys[1])
	}

	records, err := svc.repo.AllProviderEgressObjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("台账行数 = %d, want 2", len(records))
	}
	if records[0].ContentSHA256 != records[1].ContentSHA256 || records[0].DeliveryRevision == records[1].DeliveryRevision {
		t.Fatalf("同内容 + 不同投递指纹才成立：%#v", records)
	}
	if records[0].ObjectKey == records[1].ObjectKey {
		t.Fatal("两行共用同一对象键")
	}
}

func TestPreparePublicEgressImageReferencesEnforcesOwnership(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")
	// 合法资源属于 user-1，但任务以 user-2 身份发起。用超预算尺寸，保证内联分支也无法静默放行。
	seedSizedImage(t, svc, dataDir, "res-victim", "user-1", 'O', overInlinePayloadSize(), "image/png")

	input := videoEgressInput("res-victim", "http://192.168.1.167:1888/x")
	err := svc.preparePublicEgressImageReferences("user-2", &input, requireURLNeed())
	if err == nil || !strings.Contains(err.Error(), "读取出站内镜参考素材信息失败") {
		t.Fatalf("跨用户资源必须被归属校验挡下：%v", err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("越权请求不得产生上传：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].URL != "http://192.168.1.167:1888/x" {
		t.Fatalf("失败不得改写地址：%q", input.ReferenceImages[0].URL)
	}
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatal("越权请求不得把别人的字节内联进请求")
	}
}

func TestPreparePublicEgressImageReferencesRejectsOversizedImage(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")

	// 只造记录不造文件：字节数预检必须在开流之前生效。
	objectKey := "users/user-1/image/huge.png"
	if err := svc.repo.CreateResource(&model.Resource{
		ID: "res-huge", UserID: "user-1", Kind: "image", Status: model.ResourceStatusReady,
		Provider: "local", ObjectKey: objectKey, MimeType: "image/png",
		Size: providerEgressMaxImageBytes + 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	input := videoEgressInput("res-huge", "http://192.168.1.167:1888/x")
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	// 措辞要精确到"单张上限"：预检若被删掉，这里会变成"读取出站内镜参考素材失败"（文件本就不存在），
	// 断言就再也证明不了"开流之前先拦"。
	if err == nil || !strings.Contains(err.Error(), "超过出站内镜单张上限") {
		t.Fatalf("超限素材必须明确失败：%v", err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatal("超限素材不得上传")
	}
	if input.ReferenceImages[0].DataURL != "" {
		t.Fatal("超限素材不得内联")
	}
}

func TestProviderEgressMasterSwitchOffStillInlinesAndBlocksOversized(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	// 只翻开关，不重填桶信息：这是管理端"关掉总闸"的真实调用形状。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{Enabled: false}); err != nil {
		t.Fatalf("关闭总闸不应报错：%v", err)
	}
	reopened, err := svc.AdminProviderEgressSetting(admin)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Enabled {
		t.Fatal("总闸未生效")
	}
	// 关掉之后必须还能原样打开：桶信息与密钥不能被一次"关"冲掉。
	if reopened.Bucket != "yingce-egress" || reopened.Provider != "aliyun" || !reopened.HasAccessKeySecret {
		t.Fatalf("关闭总闸冲掉了既有配置：%#v", reopened)
	}

	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://yingce-egress.example.com/x.png")
	smallPayload := seedLocalImage(t, svc, dataDir, "res-small", "user-1", []byte("fake-png-bytes"))
	seedSizedImage(t, svc, dataDir, "res-big", "user-1", 'S', overInlinePayloadSize(), "image/png")

	// 总闸关的是"出站内镜"这条交付通道，内联不碰桶，所以小图照常可用。
	const original = "http://192.168.1.167:1888/api/public/resources/res-small/file"
	input := videoEgressInput("res-small", original)
	if err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed()); err != nil {
		t.Fatalf("总闸关闭不应影响预算内的小图内联：%v", err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("总闸关闭却仍上传了素材：%#v", probe.objectKeys)
	}
	if !bytes.Equal(decodedDataURL(t, input.ReferenceImages[0]), smallPayload) {
		t.Fatal("总闸关闭时小图内联字节不符")
	}

	// 超出内联预算才真的需要桶：这时必须明确失败并说明原因。
	big := videoEgressInput("res-big", "http://192.168.1.167:1888/big")
	err = svc.preparePublicEgressImageReferences("user-1", &big, requireURLNeed())
	if err == nil || !strings.Contains(err.Error(), "未启用或未配置出站内镜") {
		t.Fatalf("总闸关闭时超预算素材必须明确失败：%v", err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("总闸关闭却镜像了素材：%#v", probe.objectKeys)
	}

	// 重新打开即恢复可用，无需重填凭据。
	if _, err := svc.UpdateProviderEgressSetting(admin, ProviderEgressSettingRequest{
		Enabled: true, Provider: "aliyun", Region: "oss-cn-beijing",
		Endpoint: "oss-cn-beijing.aliyuncs.com", Bucket: "yingce-egress",
		AccessKeyID: "AKID", PathPrefix: "canvas",
	}); err != nil {
		t.Fatalf("只带 AccessKeyId 重新开启应沿用原 SecretKey：%v", err)
	}
	reopened, err = svc.AdminProviderEgressSetting(admin)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Enabled || !reopened.HasAccessKeySecret {
		t.Fatalf("重新开启后密钥丢失：%#v", reopened)
	}
	// 小图此时仍优先内联：开了桶不等于放弃更省事的 base64 通道。
	another := videoEgressInput("res-small", original)
	if err := svc.preparePublicEgressImageReferences("user-1", &another, requireURLNeed()); err != nil {
		t.Fatal(err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("桶可用后小图不该放弃内联：%#v", probe.objectKeys)
	}
	if another.ReferenceImages[0].DataURL == "" {
		t.Fatal("桶可用后小图仍应内联")
	}
	resume := videoEgressInput("res-big", "http://192.168.1.167:1888/big")
	if err := svc.preparePublicEgressImageReferences("user-1", &resume, requireURLNeed()); err != nil {
		t.Fatalf("重新开启后超预算素材应恢复镜像：%v", err)
	}
	if len(probe.objectKeys) != 1 {
		t.Fatalf("重新开启后未镜像：%#v", probe.objectKeys)
	}
}

// 内联预算是从上游契约倒推的常数，不是随手取整：
// 单张图片 ≤ 30 MB（按文件字节）、请求体总量 ≤ 64 MB，且官方建议"大文件用公网 URL，勿用 Base64"。
// 两条上限作用在不同量纲上，所以核账要分开：单张按原图字节核，请求体按 Base64 编码后（膨胀 4/3）核。
func TestProviderEgressInlineBudgetsStayUnderVendorCaps(t *testing.T) {
	const maxImageFileBytes int64 = 30 << 20
	const maxRequestBodyBytes int64 = 64 << 20
	encoded := func(raw int64) int64 { return (raw + 2) / 3 * 4 }

	// 单张预算直接按厂商文件上限取满，不越线。
	if providerEgressMaxInlineImageBytes > maxImageFileBytes {
		t.Fatalf("单张内联预算 %d 超过上游单图上限 %d", providerEgressMaxInlineImageBytes, maxImageFileBytes)
	}
	// 单张贴满时编码后仍要能单独装进请求体，否则这张图谁也交付不了。
	singleEncoded := encoded(providerEgressMaxInlineImageBytes)
	if singleEncoded >= maxRequestBodyBytes {
		t.Fatalf("单张贴线内联后 %d 字节已顶到上游 64 MB 请求体上限", singleEncoded)
	}
	totalEncoded := encoded(providerEgressMaxInlineTotalBytes)
	if totalEncoded >= maxRequestBodyBytes {
		t.Fatalf("整请求内联后 %d 字节已顶到上游 64 MB 请求体上限", totalEncoded)
	}
	// 余量按绝对值核：提示词、时长/分辨率参数与其余 content 项至少要有 8 MB 空间。
	if headroom := maxRequestBodyBytes - totalEncoded; headroom < 8<<20 {
		t.Fatalf("内联预算余量只剩 %d 字节，不足 8 MB", headroom)
	}
	// 读取硬顶必须容得下贴线的单张，否则预检会把"本可内联"的图先拦成失败。
	if providerEgressMaxInlineImageBytes > providerEgressMaxImageBytes {
		t.Fatalf("单张内联预算 %d 超过读取硬顶 %d，贴线的图永远读不进来",
			providerEgressMaxInlineImageBytes, providerEgressMaxImageBytes)
	}
	if providerEgressMaxInlineImageBytes > providerEgressMaxInlineTotalBytes {
		t.Fatalf("单张预算 %d 大于合计预算 %d，等价于单张判据失效",
			providerEgressMaxInlineImageBytes, providerEgressMaxInlineTotalBytes)
	}
	// 合计闸只兜"整批进内存"，不能把正常的内联请求挡掉。
	if providerEgressMaxAggregateBytes < providerEgressMaxInlineTotalBytes {
		t.Fatalf("合计处理上限 %d 低于内联合计预算 %d，预算内的请求也会先失败",
			providerEgressMaxAggregateBytes, providerEgressMaxInlineTotalBytes)
	}
	if providerEgressMaxAggregateBytes < providerEgressMaxImageBytes {
		t.Fatalf("合计处理上限 %d 低于单张硬顶 %d，单张贴线的图都读不进来",
			providerEgressMaxAggregateBytes, providerEgressMaxImageBytes)
	}
}

// 合计闸必须落在开流之前：参考图张数由渠道模型能力配置决定（自用态管理员可配到 100 张），
// 逐张 32 MB 硬顶挡不住"N 张同时进内存"。夹具只造记录不造文件，
// 所以一旦预检被删掉，这里会变成"读文件失败"而不是合计报错——断言因此能证明"开流之前就拦住了"。
func TestPreparePublicEgressImageReferencesCapsAggregateBytesBeforeOpening(t *testing.T) {
	svc, _ := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")

	perImage := providerEgressMaxImageBytes
	count := int(providerEgressMaxAggregateBytes/perImage) + 1
	items := make([]providerMedia, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("res-many-%d", index)
		if err := svc.repo.CreateResource(&model.Resource{
			ID: id, UserID: "user-1", Kind: "image", Status: model.ResourceStatusReady,
			Provider: "local", ObjectKey: "users/user-1/image/" + id + ".png", MimeType: "image/png",
			Size: perImage, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		items = append(items, providerMedia{
			ID: fmt.Sprintf("m-%d", index), StorageKey: "resource:" + id,
			URL: "http://192.168.1.167:1888/" + id,
		})
	}

	input := videoEgressInputOf(items...)
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	if err == nil || !strings.Contains(err.Error(), "超过出站内镜单次处理上限") {
		t.Fatalf("%d 张 × %d 字节必须被合计闸挡下：%v", count, perImage, err)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("合计超限却发生了上传：%#v", probe.objectKeys)
	}
	for index := range input.ReferenceImages {
		if input.ReferenceImages[index].DataURL != "" {
			t.Fatalf("合计超限时不得留下半套内联数据：%s", dataURLSummary(input.ReferenceImages[index]))
		}
	}
}

// 素材文件缺失/被移走时，OpenResource 的原始错误带数据目录下的绝对路径。
// 任务失败正文会被持久化并在界面展示，所以正文里只能有通用文案，路径只进日志。
func TestPreparePublicEgressImageMissingFileHidesServerPath(t *testing.T) {
	svc, dataDir := newProviderEgressTestService(t)
	configureProviderEgress(t, svc)
	t.Setenv("CANVAS_PUBLIC_BASE_URL", "http://192.168.1.167:1888")
	probe := captureEgressUploads(t, "https://ignored.example.com/x.png")

	// 只建记录、不写文件：读取必然失败，正好检查失败正文泄露了什么。
	objectKey := "users/user-1/image/gone.png"
	if err := svc.repo.CreateResource(&model.Resource{
		ID: "res-gone", UserID: "user-1", Kind: "image", Status: model.ResourceStatusReady,
		Provider: "local", ObjectKey: objectKey, MimeType: "image/png", Size: 4096,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	input := videoEgressInput("res-gone", "http://192.168.1.167:1888/x")
	err := svc.preparePublicEgressImageReferences("user-1", &input, requireURLNeed())
	if err == nil {
		t.Fatal("素材文件缺失必须失败")
	}
	message := err.Error()
	for _, leak := range []string{dataDir, "/resources/", objectKey, string(filepath.Separator) + "resources"} {
		if leak != "" && strings.Contains(message, leak) {
			t.Fatalf("任务失败正文泄露了服务器路径 %q：%s", leak, message)
		}
	}
	if !strings.Contains(message, "读取出站内镜参考素材失败") {
		t.Fatalf("失败文案要指向出站内镜读取环节：%s", message)
	}
	if len(probe.objectKeys) != 0 {
		t.Fatalf("读取失败却发生了上传：%#v", probe.objectKeys)
	}
	if input.ReferenceImages[0].URL != "http://192.168.1.167:1888/x" || input.ReferenceImages[0].DataURL != "" {
		t.Fatal("失败路径不得改写地址或留下内联数据")
	}
}
