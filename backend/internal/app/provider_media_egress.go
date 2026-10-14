// 本参图片出站内镜：把"上游必须能取回参考素材、而本机只有内网地址"的图片素材交给上游。
//
// 优先级由 MiniMax/秘塔 V2 协议的实际契约决定：`content[].image_url.url` 同时接受公网 URL
// 和 `data:image/<小写格式>;base64,<Base64>`，所以小图直接内联，不碰任何云存储；
// 只有超出请求体预算的才镜像到出站内镜桶换公网地址。
// 只处理视频任务的图片素材；不改变主存储，也不改任何协议的请求体结构。

package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"yingce/backend/internal/model"
	"yingce/backend/internal/outbound"

	"gorm.io/gorm"
)

// providerEgressReuseVerificationWindow 内命中台账直接复用，不额外请求云上；
// 超过这个窗口才做一次存在性确认，兼顾"桶被外部清空"与每次出图多一跳网络。
const providerEgressReuseVerificationWindow = 24 * time.Hour

const providerEgressDefaultAccessURLTTL = 4 * time.Hour

// providerEgressMaxImageBytes 是单张素材的读取硬顶：整张图会先进内存算 SHA-256，
// 而 Resource.ResourceUploadMB 在自用态可达几百 MB，不能拿它当内镜的内存闸。
// 取 32 MB = 厂商单文件上限 30 MB + 2 MB 余量，让 30–32 MB 这段仍能走镜像而不是直接报错。
const providerEgressMaxImageBytes int64 = 32 << 20

// 内联预算。上游契约：单张图片 ≤ 30 MB（按文件字节）、请求体总量 ≤ 64 MB，官方建议"大文件用公网 URL，勿用 Base64"；
// Base64 膨胀 4/3，所以两条上限要分开核：单张按文件上限直接取满 30 MB（编码后 40 MB，单独一张时仍小于 64 MB），
// 合计取 40 MB（编码后约 53 MB），给提示词、时长/分辨率参数与其余 content 项留下约 10 MB 空间。
const (
	providerEgressMaxInlineImageBytes int64 = 30 << 20
	providerEgressMaxInlineTotalBytes int64 = 40 << 20
)

// providerEgressMaxAggregateBytes 是一次任务能读进内存的图片总量上限。参考图张数由渠道模型能力
// 配置决定（自用态管理员可配到 100 张），单张 32 MB 的硬顶挡不住"N 张同时进内存"，
// 而判定发生在读完所有图片之后。96 MB = 3 张单张硬顶，是当前最多参考图形态的三倍余量；
// 再大就该改用对象存储，而不是让 worker 扛住整批字节。
const providerEgressMaxAggregateBytes int64 = 96 << 20

// providerEgressKeyLength 是对象名里 HMAC 摘要段的十六进制长度（16 字节）。
const providerEgressKeyLength = 32

func (s *Service) preparePublicEgressImageReferences(userID string, input *canvasGenerationInput, requirement providerMediaRequirement) error {
	if input == nil || input.Mode != "video" || input.Mask != nil || !requirement.policy.requireURL {
		return nil
	}
	// 渠道自己也部署在内网时（自建 NewAPI、内网方舟代理等），它取得到我们签出的内网地址，
	// 这里必须整体跳过：强行要求内镜会让原本可用的任务变成失败。
	if providerEgressUpstreamIsPrivate(input.Config.BaseURL) {
		return nil
	}
	needsMirror, err := s.providerEgressRequiredForLocalResources()
	if err != nil {
		return err
	}
	if !needsMirror {
		return nil
	}

	// 先判后读，分两趟：第一趟只看资源记录（不碰任何字节流），第二趟才开流读字节算指纹。
	// 合计闸必须落在开流之前——参考图张数由渠道模型能力配置决定（自用态可配到 100 张），
	// 逐张 32 MB 硬顶挡不住"N 张同时进内存"。
	type providerEgressCandidate struct {
		media      *providerMedia
		resourceID string
		size       int64
	}
	candidates := make([]providerEgressCandidate, 0, len(input.ReferenceImages))
	for index := range input.ReferenceImages {
		media := &input.ReferenceImages[index]
		// 只交付我们自己在内网签出的 http(s) 地址。厂商私有引用（方舟 asset://）由
		// prepareArkPrivateAssetReferences 负责，覆盖它会打断已经谈好的可信素材链路。
		if !providerEgressURLNeedsMirror(media.URL) {
			continue
		}
		resourceID, ok := providerMediaResourceID(media.StorageKey)
		if !ok {
			continue
		}
		// 先看资源记录再决定要不要读字节：对象存储里的素材绝不能为了判断而发起云端读取。
		resource, err := s.repo.ResourceForUser(userID, resourceID)
		if err != nil {
			return fmt.Errorf("读取出站内镜参考素材信息失败：%w", err)
		}
		if resource.Status != model.ResourceStatusReady {
			return errors.New("任务参考资源尚未上传完成")
		}
		if !strings.HasPrefix(strings.ToLower(resource.MimeType), "image/") {
			continue
		}
		if resourceUsesObjectStorage(resource) {
			continue
		}
		if resource.Size > providerEgressMaxImageBytes {
			return fmt.Errorf("参考图片超过出站内镜单张上限 %d 字节", providerEgressMaxImageBytes)
		}
		candidates = append(candidates, providerEgressCandidate{media: media, resourceID: resourceID, size: resource.Size})
	}
	if len(candidates) == 0 {
		return nil
	}

	var declaredBytes int64
	for _, candidate := range candidates {
		declaredBytes += candidate.size
		if declaredBytes > providerEgressMaxAggregateBytes {
			return fmt.Errorf("本次参考图片合计 %d 字节超过出站内镜单次处理上限 %d 字节，请减少参考图数量或改用对象存储",
				declaredBytes, providerEgressMaxAggregateBytes)
		}
	}

	// 先读后决策，是因为"能不能内联"取决于整个请求的图片合计，而不是单张。
	targets := make([]providerEgressTarget, 0, len(candidates))
	var totalBytes int64
	var maxSingle int64
	for _, candidate := range candidates {
		// OpenResource 已经把归属校验过的资源记录一起返回，这里不用再查一次。
		resource, body, err := s.openLocalResourceForEgress(userID, candidate.resourceID)
		if err != nil {
			return err
		}
		payload, err := readProviderEgressPayload(body, providerEgressMaxImageBytes, resource.MimeType, resource.Width, resource.Height)
		closeErr := body.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			slog.Warn("provider egress payload close failed", "resourceId", candidate.resourceID, "error", closeErr)
		}
		totalBytes += payload.size
		if payload.size > maxSingle {
			maxSingle = payload.size
		}
		targets = append(targets, providerEgressTarget{media: candidate.media, resourceID: candidate.resourceID, payload: payload})
	}
	if len(targets) == 0 {
		return nil
	}

	// 交付形态与"能不能出桶"取自同一次读取，免得一次任务里模式来自新快照、配置来自旧快照。
	_, value, err := s.readProviderEgressSetting()
	if err != nil {
		return err
	}
	if !value.providerEgressAlwaysMirror() && maxSingle <= providerEgressMaxInlineImageBytes && totalBytes <= providerEgressMaxInlineTotalBytes {
		for _, target := range targets {
			applyProviderEgressInlineData(target.media, target.payload)
		}
		return nil
	}
	// 走到这里说明需要公网地址：要么超出内联预算，要么管理员选了"全部镜像"。
	// 读取已经在上面完成，这里只做镜像与改写。
	return s.mirrorProviderEgressImages(value, userID, targets, requirement.reason)
}

// applyProviderEgressInlineData 把素材以内联 data URI 交给上游。
// MIME 必须小写：上游文档写的是 `data:image/<格式>;base64,<Base64>` 且注明"<格式> 小写"，
// 而资源记录里的 MimeType 可能是历史大写值。
func applyProviderEgressInlineData(media *providerMedia, payload providerEgressPayload) {
	media.URL = ""
	media.DataURL = dataURL(strings.ToLower(payload.mimeType), payload.data)
	media.MimeType = payload.mimeType
	media.Bytes = payload.size
	media.Width = payload.width
	media.Height = payload.height
	media.DurationMs = payload.durationMs
}

// mirrorProviderEgressImages 是超出内联预算时的兜底：镜像到出站内镜桶并改写为公网地址。
// 桶未启用或不可用时明确失败，不静默回退成上游取不到的内网地址。
func (s *Service) mirrorProviderEgressImages(value providerEgressSettingValue, userID string, targets []providerEgressTarget, reason string) error {
	var totalBytes int64
	for _, target := range targets {
		totalBytes += target.payload.size
	}
	setting, enabled, err := s.activeProviderEgressFromValue(value)
	if err != nil {
		return err
	}
	if !enabled {
		return BadAuthRequest(providerEgressMirrorDisabledMessage(value, totalBytes, reason))
	}
	for _, target := range targets {
		objectKey, err := s.storeProviderEgressImage(setting, userID, target.resourceID, target.payload)
		if err != nil {
			return err
		}
		accessURL, err := s.providerEgressAccessURL(setting, objectKey)
		if err != nil {
			// 签名失败来自云 SDK，报错常带接入点、HostId 与密钥线索；任务正文会被持久化展示，只进日志。
			slog.Error("provider egress sign access url failed", "provider", setting.Provider, "bucket", setting.Bucket, "objectKey", objectKey, "error", err)
			return errors.New("出站内镜签发访问地址失败，请检查后台出站内镜的 Endpoint、CDN 与密钥配置")
		}
		target.media.URL = accessURL
		target.media.DataURL = ""
		target.media.MimeType = target.payload.mimeType
		target.media.Bytes = target.payload.size
		target.media.DurationMs = target.payload.durationMs
	}
	return nil
}

// providerEgressMirrorDisabledMessage 按交付形态说清"为什么这次必须出桶"：内联优先档是预算被超出，
// 全部镜像档是管理员选了强制出桶。把后者也说成"超过可内联的上限"会把运维指向错误的旋钮。
func providerEgressMirrorDisabledMessage(value providerEgressSettingValue, totalBytes int64, reason string) string {
	if value.providerEgressAlwaysMirror() {
		return fmt.Sprintf(
			"参考图片需按后台选定的“全部镜像”交付形态换成公网地址（本次合计 %d 字节），当前渠道要求素材可取回（%s），但后台未启用或未配置出站内镜",
			totalBytes, defaultString(strings.TrimSpace(reason), "渠道要求素材可取回"))
	}
	return fmt.Sprintf(
		"参考图片合计 %d 字节，超过可内联的上限（单张 %d 字节 / 合计 %d 字节），需要启用出站内镜换公网地址；当前渠道要求素材可取回（%s），但后台未启用或未配置出站内镜",
		totalBytes, providerEgressMaxInlineImageBytes, providerEgressMaxInlineTotalBytes,
		defaultString(strings.TrimSpace(reason), "渠道要求素材可取回"))
}

// providerEgressUpstreamIsPrivate 判断渠道地址是否本身就在地网。BaseURL 为空（由系统渠道 ID 稍后解析）
// 时无法判断，按"公网渠道"处理，保持本期只兜底公网渠道的范围。
// 没写 scheme 的渠道地址（形如 `192.168.1.50:3000`）url.Parse 解析不出 Host，必须退回按原始字符串判定：
// 直接返回 false 会让本来能取到内网链接的自建渠道被强制内联/镜像，正是这条守卫要避免的回归。
func providerEgressUpstreamIsPrivate(baseURL string) bool {
	value := strings.TrimSpace(baseURL)
	if value == "" {
		return false
	}
	if parsed, err := url.Parse(value); err == nil {
		if host := strings.TrimSpace(parsed.Hostname()); host != "" {
			return outbound.IsNonPublicHost(host)
		}
	}
	return outbound.IsNonPublicHost(value)
}

// providerEgressURLNeedsMirror 只处理"上游取不到的 http(s) 地址"。空地址同样需要给一个，
// 非 http 的厂商私有引用（asset://、gs:// 等）一律不动。
func providerEgressURLNeedsMirror(rawURL string) bool {
	value := strings.ToLower(strings.TrimSpace(rawURL))
	if value == "" {
		return true
	}
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

// openLocalResourceForEgress 复用既有的归属校验读取入口，同时把校验过的资源记录交回调用方。
// 原始错误只进日志：OpenResource 失败常带数据目录下的绝对路径，而任务失败正文会被持久化并在界面展示。
func (s *Service) openLocalResourceForEgress(userID string, resourceID string) (*model.Resource, io.ReadCloser, error) {
	resource, body, err := s.OpenResource(userID, resourceID)
	if err != nil {
		slog.Warn("provider egress open resource failed", "userId", userID, "resourceId", resourceID, "error", err)
		return nil, nil, errors.New("读取出站内镜参考素材失败，请确认该素材文件仍在服务器上")
	}
	if resourceUsesObjectStorage(resource) {
		_ = body.Close()
		return nil, nil, errors.New("出站内镜只处理服务器本地素材")
	}
	return resource, body, nil
}

// providerMediaResourceID 只认 hydration 写入的 resource: 前缀存储键。
func providerMediaResourceID(storageKey string) (string, bool) {
	trimmed := strings.TrimSpace(storageKey)
	if !strings.HasPrefix(trimmed, "resource:") {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(trimmed, "resource:"))
	if id == "" {
		return "", false
	}
	return id, true
}

// providerEgressRequiredForLocalResources 回答"本机签出的地址上游能不能取"。
// 读取 base 失败时按"需要交付给上游"返回，不往上抛错误：本函数的唯一调用点在 hydration 之后，
// 而 hydration 对 requireURL 渠道必然先调 providerResourceURL，base 未配置时那里就已经报
// "服务器本地存储尚未配置服务器访问地址"，走不到这里。所以这条分支是防御性的，
// 不代表"没配 base 也能靠内联出图"——那种配置下任务会以缺配置错误结束。
func (s *Service) providerEgressRequiredForLocalResources() (bool, error) {
	base, err := s.publicResourceBaseURL()
	if err != nil || base == nil {
		return true, nil
	}
	return outbound.IsNonPublicHost(base.Hostname()), nil
}

// providerEgressTarget 是一条已判定需要交给上游的图片素材：媒体指针、来源资源 ID 与已读入的字节。
type providerEgressTarget struct {
	media      *providerMedia
	resourceID string
	payload    providerEgressPayload
}

type providerEgressPayload struct {
	data       []byte
	sha256Hex  string
	size       int64
	mimeType   string
	width      int
	height     int
	durationMs int64
}

// declaredMimeType 来自资源记录，字节嗅探只作兜底：normalizedMediaMimeType 会优先采信
// 非空声明值，这与 hydration 内联路径的口径保持一致。
func readProviderEgressPayload(reader io.Reader, limit int64, declaredMimeType string, width int, height int) (providerEgressPayload, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		// 读文件失败的原始错误带绝对路径，不进任务正文，只进日志。
		slog.Warn("provider egress payload read failed", "limit", limit, "error", err)
		return providerEgressPayload{}, errors.New("读取参考素材内容失败，请确认该素材文件仍可读取")
	}
	if int64(len(data)) > limit {
		return providerEgressPayload{}, fmt.Errorf("参考图片超过出站内镜上限 %d 字节", limit)
	}
	if len(data) == 0 {
		return providerEgressPayload{}, errors.New("参考图片内容为空")
	}
	sum := sha256.Sum256(data)
	return providerEgressPayload{
		data:      data,
		sha256Hex: hex.EncodeToString(sum[:]),
		size:      int64(len(data)),
		mimeType:  normalizedMediaMimeType(declaredMimeType, data),
		width:     width,
		height:    height,
	}, nil
}

func (s *Service) storeProviderEgressImage(setting ossSettingValue, userID string, resourceID string, payload providerEgressPayload) (string, error) {
	normalized := normalizeOSSSetting(setting)
	revision := storageDeliveryRevision(normalized)
	objectKey, err := s.providerEgressObjectKey(normalized, userID, payload, revision)
	if err != nil {
		return "", err
	}
	existing, err := s.repo.ProviderEgressObject(userID, payload.sha256Hex, normalized.Provider, normalized.Bucket, revision)
	switch {
	case err == nil && existing != nil:
		if time.Since(existing.LastUsedAt) < providerEgressReuseVerificationWindow || providerEgressObjectLive(normalized, existing.ObjectKey) {
			if touchErr := s.repo.TouchProviderEgressObject(existing.ID, time.Now()); touchErr != nil {
				// 使用时间只影响回收时机，写不进去不该让生成失败。
				slog.Warn("provider egress ledger touch failed", "id", existing.ID, "error", touchErr)
			}
			return existing.ObjectKey, nil
		}
		// 台账与桶漂移：删掉这一行并按未命中重传。
		if deleteErr := s.repo.DeleteProviderEgressObject(existing.ID); deleteErr != nil {
			return "", fmt.Errorf("清理失效出站内镜台账失败：%w", deleteErr)
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
	default:
		return "", fmt.Errorf("查询出站内镜台账失败：%w", err)
	}

	if _, err := putOSSObject(normalized, objectKey, payload.mimeType, payload.size, bytes.NewReader(payload.data)); err != nil {
		// 厂商 SDK 的报错常带接入点、HostId 甚至密钥线索；这些不进任务正文，只进日志。
		slog.Error("provider egress upload failed", "provider", normalized.Provider, "bucket", normalized.Bucket, "objectKey", objectKey, "error", err)
		return "", errors.New("出站内镜上传图片失败，请检查后台出站内镜的桶、密钥与 Endpoint 配置")
	}
	record := &model.ProviderEgressObject{
		ID: newID(), UserID: userID, ContentSHA256: payload.sha256Hex,
		Provider: normalized.Provider, Endpoint: normalized.Endpoint, Bucket: normalized.Bucket,
		ObjectKey: objectKey, MimeType: payload.mimeType, SizeBytes: payload.size,
		ResourceID: resourceID, CDNMode: normalized.Delivery.CDNAuthMode, DeliveryRevision: revision,
		CreatedAt: time.Now(), LastUsedAt: time.Now(),
	}
	if err := s.repo.CreateProviderEgressObject(record); err != nil {
		// 并发首传撞唯一索引时读回已有行复用，避免同一张图留下两条台账。
		if raced, lookupErr := s.repo.ProviderEgressObject(userID, payload.sha256Hex, normalized.Provider, normalized.Bucket, revision); lookupErr == nil && raced != nil {
			return raced.ObjectKey, nil
		}
		slog.Error("provider egress ledger write failed", "objectKey", objectKey, "error", err)
		return "", errors.New("写出站内镜台账失败，请稍后重试")
	}
	return objectKey, nil
}

// providerEgressObjectKey 用服务端密钥对 (userID, 内容指纹, 投递指纹) 做 HMAC 作为对象名。
//
// 两个理由：一是对象名不可猜——桶即便配了公有读 CDN，`sha256(像素)` 是任何人重算得出的，
// 直接当对象名等于给看过原图的人一条永久可用的推导路径；二是投递指纹进了摘要，
// 换凭据或改 CDN 后新映射落在新对象上，不会和旧行共用同一个键，避免旧行到期时把新行仍在用的对象删掉。
func (s *Service) providerEgressObjectKey(setting ossSettingValue, userID string, payload providerEgressPayload, revision string) (string, error) {
	key, err := s.settingsEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("读取出站内镜对象名密钥失败：%w", err)
	}
	mac := hmac.New(sha256.New, key)
	// 内容指纹参与计算前统一小写，避免大小写差异造成同一张图落到两个对象。
	mac.Write([]byte(strings.ToLower(fmt.Sprintf("yingce-egress|%s|%s|%s", strings.TrimSpace(userID), payload.sha256Hex, revision))))
	digest := hex.EncodeToString(mac.Sum(nil))[:providerEgressKeyLength]
	return path.Join(providerEgressObjectPrefix(providerEgressSettingValue{PathPrefix: setting.PathPrefix}),
		safeProviderEgressSegment(userID), digest+"."+providerEgressExtension(payload.mimeType)), nil
}

// safeProviderEgressSegment 用户 ID 本就是固定长度标识，这里只防御性剔掉任何路径分隔符。
func safeProviderEgressSegment(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.TrimSpace(value))
	return defaultString(cleaned, "unknown")
}

func providerEgressExtension(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	case "image/avif":
		return "avif"
	case "image/tiff":
		return "tiff"
	case "image/heic":
		return "heic"
	default:
		return "img"
	}
}

// providerEgressObjectLive 用首字节 Range 读确认对象还在，不下整张图。
func providerEgressObjectLive(setting ossSettingValue, objectKey string) bool {
	stream, err := getOSSObjectRange(normalizeOSSSetting(setting), objectKey, "bytes=0-0")
	if err != nil {
		return false
	}
	if stream == nil {
		return false
	}
	if stream.Body != nil {
		defer stream.Body.Close()
	}
	return stream.StatusCode == http.StatusOK || stream.StatusCode == http.StatusPartialContent
}

func (s *Service) providerEgressAccessURL(setting ossSettingValue, objectKey string) (string, error) {
	normalized := normalizeOSSSetting(setting)
	ttl := normalized.Runtime.ProviderAccessURLTTL
	if ttl <= 0 {
		ttl = providerEgressDefaultAccessURLTTL
	}
	expires := time.Now().UTC().Add(ttl)
	// CDN 公有读给出的地址不带查询参数，对上游最省事；签名不可用或模式不支持时回源签名。
	if cdnEnabled(normalized) {
		if signed, err := signCDNURL(normalized, objectKey, expires); err == nil && strings.TrimSpace(signed) != "" {
			return signed, nil
		}
	}
	return signedOSSObjectURL(s.storageSettingWithRuntimePolicy(normalized), objectKey, expires)
}
