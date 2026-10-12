// 出站内镜设置：为"上游模型必须回取公网地址"的场景配置一个独立的对象存储落点。
//
// 这里的 Enabled 只描述内镜是否可用，不参与主存储选址。它刻意与 ossSettingKey 分开：
// 那份设置的 Enabled 会让 storeResource 把新上传直接改写到对象存储，
// 打开内镜绝不该产生那种副作用。

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"yingce/backend/internal/model"
	"yingce/backend/internal/outbound"

	"gorm.io/gorm"
)

// providerEgressObjectDir 是内镜对象在 PathPrefix 下的固定子目录；回收和桶生命周期规则都按它定位。
const providerEgressObjectDir = "egress"

type ProviderEgressSettingRequest struct {
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"accessKeyId"`
	AccessKeySecret string `json:"accessKeySecret"`
	PathPrefix      string `json:"pathPrefix"`
	CDNBaseURL      string `json:"cdnBaseUrl"`
	CDNAuthMode     string `json:"cdnAuthMode"`
}

type PublicProviderEgressSetting struct {
	Enabled            bool      `json:"enabled"`
	Provider           string    `json:"provider"`
	Region             string    `json:"region"`
	Endpoint           string    `json:"endpoint"`
	Bucket             string    `json:"bucket"`
	AccessKeyID        string    `json:"accessKeyId"`
	HasAccessKeySecret bool      `json:"hasAccessKeySecret"`
	PathPrefix         string    `json:"pathPrefix"`
	CDNBaseURL         string    `json:"cdnBaseUrl"`
	CDNAuthMode        string    `json:"cdnAuthMode"`
	MappedObjectCount  int64     `json:"mappedObjectCount"`
	UpdatedBy          string    `json:"updatedBy"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	ExpiresAfterHours  int       `json:"expiresAfterHours"`
}

type providerEgressSettingValue struct {
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"accessKeyId"`
	AccessKeySecret string `json:"accessKeySecret"`
	PathPrefix      string `json:"pathPrefix"`
	CDNBaseURL      string `json:"cdnBaseUrl"`
	CDNAuthMode     string `json:"cdnAuthMode"`
}

func (s *Service) AdminProviderEgressSetting(actor *model.User) (*PublicProviderEgressSetting, error) {
	if err := s.RequireAdmin(actor); err != nil {
		return nil, err
	}
	setting, value, err := s.readProviderEgressSetting()
	if err != nil {
		return nil, err
	}
	public := publicProviderEgressSetting(setting, value)
	count, countErr := s.repo.ProviderEgressObjectCount()
	if countErr != nil {
		// 台账只用于展示条数，读不到不该挡住管理员打开设置页。
		slog.Warn("provider egress ledger count unavailable", "error", countErr)
	} else {
		public.MappedObjectCount = count
	}
	// 内镜地址的有效期来自后台"模型输入资源地址有效期"，管理员需要知道实际是多少。
	s.applyProviderEgressTTLHint(&public)
	return &public, nil
}

func (s *Service) UpdateProviderEgressSetting(actor *model.User, req ProviderEgressSettingRequest) (*PublicProviderEgressSetting, error) {
	if err := s.RequireAdmin(actor); err != nil {
		return nil, err
	}
	currentSetting, current, err := s.readProviderEgressSetting()
	if err != nil {
		return nil, err
	}
	next, err := providerEgressSettingFromRequest(req, current)
	if err != nil {
		return nil, err
	}
	setting := &model.SystemSetting{Key: providerEgressSettingKey, UpdatedBy: actor.ID}
	if currentSetting != nil {
		setting.CreatedAt = currentSetting.CreatedAt
	}
	if err := s.saveProviderEgressSetting(setting, next); err != nil {
		return nil, err
	}
	public := publicProviderEgressSetting(setting, next)
	s.applyProviderEgressTTLHint(&public)
	return &public, nil
}

// applyProviderEgressTTLHint 把"厂商访问地址有效期"折进公开态，供页面说明镜像素材可被公网读多久。
// GET 与 PATCH 都要填：否则保存成功后回包覆盖页面 state，这句提示会凭空消失到下次刷新。
func (s *Service) applyProviderEgressTTLHint(public *PublicProviderEgressSetting) {
	if policy, err := s.RuntimePolicy(); err == nil && policy.Storage.ProviderAccessURLTTLSeconds > 0 {
		public.ExpiresAfterHours = policy.Storage.ProviderAccessURLTTLSeconds / 3600
	}
}

// verifyProviderEgressConnection 是 verifyOSSConnection 的可替换别名。
// 不做这层间接就测不到"配置没填完时根本不发起连接"这条规则——测试只能看到一句也提到 Bucket 的
// 连接失败，看不出校验有没有先把请求挡下来。
var verifyProviderEgressConnection = verifyOSSConnection

// TestProviderEgressSetting 写入并读回一个极小探针对象后删除，确认密钥、桶和前缀真的可用。
func (s *Service) TestProviderEgressSetting(actor *model.User, req ProviderEgressSettingRequest) (string, error) {
	if err := s.RequireAdmin(actor); err != nil {
		return "", err
	}
	_, current, err := s.readProviderEgressSetting()
	if err != nil {
		return "", err
	}
	value, err := providerEgressSettingFromRequest(req, current)
	if err != nil {
		return "", err
	}
	// 连通性测试必须和"启用态的完整性校验"用同一把尺子：providerEgressSettingFromRequest 只在
	// Enabled 为真时校验，而管理员常在关着的状态下先试桶和密钥。不补这一步，
	// 一个保存时会被拒的半成品配置会报"连通性正常"，管理员带着假信心打开总闸。
	probeValue := value
	probeValue.Enabled = true
	if err := validateProviderEgressValue(probeValue); err != nil {
		return "", err
	}
	setting := normalizeOSSSetting(value.storageSettings())
	if err := verifyProviderEgressConnection(setting, providerEgressObjectPrefix(providerEgressSettingValue{PathPrefix: setting.PathPrefix})+"/_probe/healthcheck.bin"); err != nil {
		return "", err
	}
	return setting.Bucket, nil
}

// providerEgressClearScanLimit 是"清空映射"单次扫描的台账行数上限。这是管理员显式发起的
// 破坏性操作，不能像周期清理那样按 200 一批慢慢啃；超出上限的行不会静默消失，会以 remaining 如实回给前端。
const providerEgressClearScanLimit = 5000

// ClearProviderEgressMappings 删除已镜像的云上对象并一并丢弃复用映射，迫使下次出图重新上传。
// 返回本次删除的行数与台账剩余行数（剩余 = 其他落点 + 删除失败 + 超出扫描上限）。
//
// 必须先删对象再删记录：回收是行驱动的，只删记录等于让这些公网副本彻底失去回收入口。
// 凭据不可用时直接拒绝，而不是盲删记录。
// 只处理与当前配置同 provider、同 endpoint、同 bucket 的行：其余落点留给对应凭据或该桶的生命周期规则，
// 绝不把当前密钥签名后发给别家厂商的接入点——那等于把凭据线索交给第三方。
func (s *Service) ClearProviderEgressMappings(actor *model.User) (int64, int64, error) {
	if err := s.RequireAdmin(actor); err != nil {
		return 0, 0, err
	}
	setting, ok, err := s.providerEgressStorageForMaintenance()
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, BadAuthRequest("出站内镜的存储配置不可用，无法删除已镜像的对象；请先补齐配置，或直接在桶上清理 egress 前缀")
	}
	normalized := normalizeOSSSetting(setting)
	records, err := s.repo.AllProviderEgressObjects(providerEgressClearScanLimit)
	if err != nil {
		return 0, 0, fmt.Errorf("读取出站内镜台账失败：%w", err)
	}
	deleted := int64(0)
	failed := 0
	skipped := 0
	for index := range records {
		record := records[index]
		if !providerEgressDeliveryMatches(normalized, record) {
			skipped++
			continue
		}
		if err := providerEgressDeleteObject(providerEgressSettingForRecord(normalized, record), record.ObjectKey); err != nil {
			failed++
			slog.Warn("provider egress object delete failed on clear", "id", record.ID, "objectKey", record.ObjectKey, "error", err)
			continue
		}
		if err := s.repo.DeleteProviderEgressObject(record.ID); err != nil {
			failed++
			slog.Warn("provider egress ledger delete failed on clear", "id", record.ID, "error", err)
			continue
		}
		deleted++
	}
	remaining, err := s.repo.ProviderEgressObjectCount()
	if err != nil {
		return deleted, 0, fmt.Errorf("读取出站内镜台账条数失败：%w", err)
	}
	slog.Info("provider egress mappings cleared", "deleted", deleted, "failed", failed, "skipped", skipped, "remaining", remaining, "actor", actor.ID)
	if failed > 0 {
		return deleted, remaining, fmt.Errorf("有 %d 个出站内镜对象未能删除，对应映射已保留；请检查桶权限，或改用桶生命周期规则清理", failed)
	}
	return deleted, remaining, nil
}

// sameEgressDeliveryValue 行内或当前缺该维度时按同一落点处理：早期行可能不写 endpoint，
// 只有明确记着"别家的值"才判成其他落点。
func sameEgressDeliveryValue(recordValue string, currentValue string) bool {
	recordValue = strings.TrimSpace(recordValue)
	currentValue = strings.TrimSpace(currentValue)
	if recordValue == "" || currentValue == "" {
		return true
	}
	return strings.EqualFold(recordValue, currentValue)
}

func providerEgressDeliveryMatches(current ossSettingValue, record model.ProviderEgressObject) bool {
	return sameEgressDeliveryValue(record.Provider, current.Provider) &&
		sameEgressDeliveryValue(record.Endpoint, current.Endpoint) &&
		sameEgressDeliveryValue(record.Bucket, current.Bucket)
}

// providerEgressStorageForMaintenance 与 activeProviderEgressSetting 一样返回可用配置，
// 但忽略 Enabled：管理员关掉内镜后，已经镜像出去的对象仍然必须能被回收。
func (s *Service) providerEgressStorageForMaintenance() (ossSettingValue, bool, error) {
	_, value, err := s.readProviderEgressSetting()
	if err != nil {
		return ossSettingValue{}, false, err
	}
	if strings.TrimSpace(value.Provider) == "" {
		return ossSettingValue{}, false, nil
	}
	setting := s.storageSettingWithRuntimePolicy(normalizeOSSSetting(value.storageSettings()))
	if err := validateProviderEgressStorage(setting); err != nil {
		return ossSettingValue{}, false, nil
	}
	return setting, true, nil
}

// providerEgressSettingForRecord 用行内记录的落点三件套覆盖当前配置。
// 换过厂商后当前密钥多半不适用于旧桶，此时删除失败并保留记录，由日志和报错暴露，不会静默漏删。
func providerEgressSettingForRecord(current ossSettingValue, record model.ProviderEgressObject) ossSettingValue {
	setting := normalizeOSSSetting(current)
	if strings.TrimSpace(record.Provider) != "" {
		setting.Provider = record.Provider
	}
	if strings.TrimSpace(record.Endpoint) != "" {
		setting.Endpoint = record.Endpoint
	}
	if strings.TrimSpace(record.Bucket) != "" {
		setting.Bucket = record.Bucket
	}
	// 删除走源站，不经过 CDN 出口。
	setting.CDNBaseURL = ""
	setting.Delivery.CDNAuthMode = ""
	return setting
}

func (s *Service) readProviderEgressSetting() (*model.SystemSetting, providerEgressSettingValue, error) {
	setting, err := s.repo.SystemSetting(providerEgressSettingKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, providerEgressSettingValue{}, nil
	}
	if err != nil {
		return nil, providerEgressSettingValue{}, err
	}
	value := providerEgressSettingValue{}
	if strings.TrimSpace(setting.ValueJSON) != "" {
		if err := json.Unmarshal([]byte(setting.ValueJSON), &value); err != nil {
			return nil, providerEgressSettingValue{}, errors.New("出站内镜配置格式无效")
		}
	}
	needsMigration := value.AccessKeySecret != "" && !strings.HasPrefix(value.AccessKeySecret, encryptedSettingPrefix)
	secret, err := s.decryptSettingSecret(value.AccessKeySecret)
	if err != nil {
		return nil, providerEgressSettingValue{}, err
	}
	value.AccessKeySecret = secret
	value = normalizeProviderEgressSetting(value)
	if needsMigration {
		if err := s.saveProviderEgressSetting(setting, value); err != nil {
			return nil, providerEgressSettingValue{}, err
		}
	}
	return setting, value, nil
}

func (s *Service) saveProviderEgressSetting(setting *model.SystemSetting, value providerEgressSettingValue) error {
	stored := normalizeProviderEgressSetting(value)
	var err error
	stored.AccessKeySecret, err = s.encryptSettingSecret(stored.AccessKeySecret)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	setting.ValueJSON = string(encoded)
	return s.repo.SaveSystemSetting(setting)
}

// activeProviderEgressSetting 返回存储层可直接使用的内镜配置；第二个返回值表示内镜是否可用。
// 未启用时不报错：本期语义是"没配内镜就不镜像"，由真正需要镜像的调用方产出明确失败原因。
func (s *Service) activeProviderEgressSetting() (ossSettingValue, bool, error) {
	_, value, err := s.readProviderEgressSetting()
	if err != nil {
		return ossSettingValue{}, false, err
	}
	if !value.Enabled {
		return ossSettingValue{}, false, nil
	}
	setting := s.storageSettingWithRuntimePolicy(normalizeOSSSetting(value.storageSettings()))
	if err := validateProviderEgressStorage(setting); err != nil {
		return ossSettingValue{}, false, err
	}
	return setting, true, nil
}

func providerEgressSettingFromRequest(req ProviderEgressSettingRequest, current providerEgressSettingValue) (providerEgressSettingValue, error) {
	// 除 enabled 外全为空的请求是"翻总闸"，不是"重填配置"：必须原样保留桶、前缀与密钥。
	// 公开态从不回显 SecretKey，一旦关掉时清空，管理员再打开就必须手上有密钥原文。
	// 关：不做完整性校验（就是要把一个可能已不可用的配置停掉）。
	// 开：必须校验既有配置仍然完整，否则会凭空"启用"出一个一用就失败的内镜。
	if providerEgressRequestOnlyToggles(req) {
		preserved := normalizeProviderEgressSetting(current)
		if !req.Enabled {
			preserved.Enabled = false
			return preserved, nil
		}
		preserved.Enabled = true
		if err := validateProviderEgressValue(preserved); err != nil {
			return preserved, err
		}
		return preserved, nil
	}
	next := normalizeProviderEgressSetting(providerEgressSettingValue{
		Enabled:         req.Enabled,
		Provider:        req.Provider,
		Region:          req.Region,
		Endpoint:        req.Endpoint,
		Bucket:          req.Bucket,
		AccessKeyID:     req.AccessKeyID,
		AccessKeySecret: req.AccessKeySecret,
		PathPrefix:      req.PathPrefix,
		CDNBaseURL:      req.CDNBaseURL,
		CDNAuthMode:     req.CDNAuthMode,
	})
	// SecretKey 从不回显，所以"留空"必须能表示沿用。只有 provider 与 AccessKeyId 都没变时才沿用，
	// 换厂商时沿用上一家的密钥会静默指向另一个云；换了 AccessKeyId 却不给 SecretKey 会被
	// 下面的校验挡住，避免新账号配旧密钥。
	if next.AccessKeySecret == "" && next.AccessKeyID == current.AccessKeyID && next.Provider == current.Provider {
		next.AccessKeySecret = current.AccessKeySecret
	}
	if !next.Enabled {
		return next, nil
	}
	if err := validateProviderEgressValue(next); err != nil {
		return next, err
	}
	return next, nil
}

// validateProviderEgressValue 是"内镜处于启用状态时必须满足的条件"的唯一来源：
// 显式保存配置与只翻开关两条路径共用，避免规则在两处漂移。
func validateProviderEgressValue(value providerEgressSettingValue) error {
	if value.Provider == "" {
		return BadAuthRequest("请选择出站内镜的存储类型")
	}
	if value.Provider != aliyunOSSProvider && value.Provider != tencentCOSProvider && value.Provider != qiniuKodoProvider && value.Provider != s3Provider {
		return BadAuthRequest("出站内镜仅支持阿里云 OSS、腾讯云 COS、七牛云 Kodo 和通用 S3")
	}
	if value.Endpoint == "" {
		return BadAuthRequest("请填写出站内镜的 Endpoint")
	}
	if value.Bucket == "" {
		return BadAuthRequest("请填写出站内镜的 Bucket")
	}
	if value.AccessKeyID == "" {
		return BadAuthRequest("请填写出站内镜的 AccessKeyId")
	}
	if value.AccessKeySecret == "" {
		return BadAuthRequest("请填写出站内镜的 SecretKey")
	}
	switch value.CDNAuthMode {
	case "", "public", "qiniu":
	default:
		return BadAuthRequest("出站内镜的 CDN 鉴权方式仅支持 public 或 qiniu")
	}
	if value.CDNBaseURL != "" && value.CDNAuthMode == "" {
		return BadAuthRequest("配置了 CDN 域名时必须选择 CDN 鉴权方式")
	}
	// 内镜地址必须上游可达：签名 URL 就是拿 Endpoint 拼的，内网 Endpoint（自建 MinIO 等）
	// 签出来的地址上游一样取不到，等于把本轮要修的问题搬到桶上。有 CDN 出口时以 CDN 域名为准。
	if value.CDNBaseURL == "" {
		if outbound.IsNonPublicHost(providerEgressHost(value.Endpoint)) {
			return BadAuthRequest("出站内镜的 Endpoint 必须是上游可访问的公网地址；内网 Endpoint 签出的地址上游取不到")
		}
	} else if outbound.IsNonPublicHost(providerEgressHost(value.CDNBaseURL)) {
		return BadAuthRequest("出站内镜的 CDN 域名必须是上游可访问的公网地址")
	}
	return nil
}

// providerEgressHost 从"带或不带 scheme 的地址"里取主机名。Endpoint 允许写成
// oss-cn-beijing.aliyuncs.com 这种无 scheme 形式，url.Parse 会把它塞进 Path，所以留一层兜底。
func providerEgressHost(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil {
		if host := strings.TrimSpace(parsed.Hostname()); host != "" {
			return host
		}
	}
	value = strings.TrimPrefix(strings.TrimPrefix(value, "//"), "http://")
	value = strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "//")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}
	return value
}

// providerEgressRequestOnlyToggles 判断这次请求是否只翻了开关：任何一项配置字段有值都按"重填配置"处理，
// 让原有校验与覆盖逻辑照旧生效，避免把一个写坏的请求当成纯开关。
func providerEgressRequestOnlyToggles(req ProviderEgressSettingRequest) bool {
	return strings.TrimSpace(req.Provider) == "" &&
		strings.TrimSpace(req.Region) == "" &&
		strings.TrimSpace(req.Endpoint) == "" &&
		strings.TrimSpace(req.Bucket) == "" &&
		strings.TrimSpace(req.AccessKeyID) == "" &&
		strings.TrimSpace(req.AccessKeySecret) == "" &&
		strings.TrimSpace(req.PathPrefix) == "" &&
		strings.TrimSpace(req.CDNBaseURL) == "" &&
		strings.TrimSpace(req.CDNAuthMode) == ""
}

func validateProviderEgressStorage(setting ossSettingValue) error {
	if setting.Provider == "" || setting.Bucket == "" || setting.Endpoint == "" || setting.AccessKeyID == "" || setting.AccessKeySecret == "" {
		return BadAuthRequest("出站内镜的存储配置不完整")
	}
	return nil
}

func normalizeProviderEgressSetting(value providerEgressSettingValue) providerEgressSettingValue {
	value.Provider = strings.ToLower(strings.TrimSpace(value.Provider))
	value.Region = strings.TrimSpace(value.Region)
	value.Endpoint = strings.TrimRight(strings.TrimSpace(value.Endpoint), "/")
	value.Bucket = strings.TrimSpace(value.Bucket)
	value.AccessKeyID = strings.TrimSpace(value.AccessKeyID)
	value.AccessKeySecret = strings.TrimSpace(value.AccessKeySecret)
	value.PathPrefix = strings.Trim(strings.TrimSpace(value.PathPrefix), "/")
	value.CDNBaseURL = strings.TrimRight(strings.TrimSpace(value.CDNBaseURL), "/")
	value.CDNAuthMode = strings.ToLower(strings.TrimSpace(value.CDNAuthMode))
	return value
}

// storageSettings 把内镜设置映射成存储层通用配置。CDN 只在这里按需注入，
// 主存储的 CDN 与投递策略不参与。
func (v providerEgressSettingValue) storageSettings() ossSettingValue {
	setting := ossSettingValue{
		Enabled:         true,
		Provider:        v.Provider,
		Region:          v.Region,
		Endpoint:        v.Endpoint,
		Bucket:          v.Bucket,
		AccessKeyID:     v.AccessKeyID,
		AccessKeySecret: v.AccessKeySecret,
		PathPrefix:      v.PathPrefix,
		CDNBaseURL:      v.CDNBaseURL,
	}
	// Delivery 是嵌套结构体字段；app 包对 internal/storage 的引用统一走别名层，
	// 这里逐字段赋值而不是引 storage.DeliverySettings 字面量。
	setting.Delivery.CDNAuthMode = v.CDNAuthMode
	return setting
}

func providerEgressObjectPrefix(value providerEgressSettingValue) string {
	prefix := strings.Trim(strings.TrimSpace(value.PathPrefix), "/")
	if prefix == "" {
		return providerEgressObjectDir
	}
	return prefix + "/" + providerEgressObjectDir
}

func publicProviderEgressSetting(setting *model.SystemSetting, value providerEgressSettingValue) PublicProviderEgressSetting {
	result := PublicProviderEgressSetting{
		Enabled:            value.Enabled,
		Provider:           value.Provider,
		Region:             value.Region,
		Endpoint:           value.Endpoint,
		Bucket:             value.Bucket,
		AccessKeyID:        value.AccessKeyID,
		HasAccessKeySecret: value.AccessKeySecret != "",
		PathPrefix:         value.PathPrefix,
		CDNBaseURL:         value.CDNBaseURL,
		CDNAuthMode:        value.CDNAuthMode,
	}
	if setting != nil {
		result.UpdatedBy = setting.UpdatedBy
		result.CreatedAt = setting.CreatedAt
		result.UpdatedAt = setting.UpdatedAt
	}
	return result
}
