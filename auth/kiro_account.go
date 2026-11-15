package auth

// Kiro（Amazon Q Developer / CodeWhisperer 后端）账号在账号池中的运行时接线。
//
// access_token / refresh_token / api_key / client_secret 复用通用凭据键，因此自动
// 享有静态加密与列表脱敏；其余 Kiro 专有字段落在 kiro_ 前缀键里。协议行为移植自
// ZyphrZero/kiro.rs（见 third_party/kiro.rs/NOTICE）。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// UpstreamKiro 是 Kiro 账号的 upstream_type 判别值。
const UpstreamKiro = "kiro"

// Kiro 凭据形态。
const (
	KiroAuthKindSocial      = "social"
	KiroAuthKindIdC         = "idc"
	KiroAuthKindExternalIdP = "external_idp"
	KiroAuthKindAPIKey      = "api_key"
)

// Kiro 专有凭据键。
const (
	KiroAuthKindCredentialKey      = "kiro_auth_kind"
	KiroLoginSourceCredentialKey   = "kiro_login_source"
	KiroRegionCredentialKey        = "kiro_region"
	KiroAPIRegionCredentialKey     = "kiro_api_region"
	KiroClientIDCredentialKey      = "kiro_client_id"
	KiroStartURLCredentialKey      = "kiro_start_url"
	KiroTokenEndpointCredentialKey = "kiro_token_endpoint"
	KiroIssuerURLCredentialKey     = "kiro_issuer_url"
	KiroScopesCredentialKey        = "kiro_scopes"
	KiroProfileARNCredentialKey    = "kiro_profile_arn"
	KiroMachineIDCredentialKey     = "kiro_machine_id"
	KiroEndpointCredentialKey      = "kiro_endpoint"
	KiroUsageCredentialKey         = "kiro_usage"
	// KiroClientSecretCredentialKey 复用通用 client_secret 键以进入加密名单。
	KiroClientSecretCredentialKey = "client_secret"
)

const (
	// KiroBuilderIDProfileARN 是官方 IDE 对没有 Enterprise profile 的账号使用的占位 ARN。
	KiroBuilderIDProfileARN = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	// KiroSocialProfileARN 是 Github / Google 登录共用的 profile ARN。
	KiroSocialProfileARN = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
	KiroDefaultRegion    = "us-east-1"
	KiroEndpointIDE      = "ide"
	KiroEndpointCLI      = "cli"
)

// KiroCredential 是 Kiro 账号除令牌外的身份信息，随账号装载与刷新整体替换。
type KiroCredential struct {
	AuthKind      string
	LoginSource   string
	Region        string
	APIRegion     string
	ClientID      string
	ClientSecret  string
	StartURL      string
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	ProfileARN    string
	MachineID     string
	Endpoint      string
	// UsageResetAt 是最近一次额度快照里的下次重置时间，用于额度用尽时的冷却截止。
	UsageResetAt time.Time
}

var kiroExternalIdPAliases = map[string]struct{}{
	"external_idp": {}, "azuread": {}, "azure": {}, "entra": {}, "entra-id": {},
	"microsoft": {}, "m365": {}, "office365": {}, "external": {},
}

// NormalizeKiroAuthKind 归一化导入凭据的形态。显式声明优先；带 tokenEndpoint 的
// 一律视为企业 SSO（social / idc 都没有这个字段）；其余按材料推断。
func NormalizeKiroAuthKind(raw, tokenEndpoint string, hasAPIKey, hasClientSecret bool) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "api_key" || value == "apikey" || hasAPIKey {
		return KiroAuthKindAPIKey
	}
	if _, ok := kiroExternalIdPAliases[value]; ok || strings.TrimSpace(tokenEndpoint) != "" {
		return KiroAuthKindExternalIdP
	}
	switch value {
	case "idc", "builder-id", "builderid", "iam", "iam_sso", "sso", "enterprise":
		return KiroAuthKindIdC
	case "social", "github", "google":
		return KiroAuthKindSocial
	}
	if hasClientSecret {
		return KiroAuthKindIdC
	}
	return KiroAuthKindSocial
}

// NormalizeKiroEndpoint 只接受 ide / cli，API Key 账号默认 cli（与 kiro-cli 一致）。
func NormalizeKiroEndpoint(raw, authKind string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case KiroEndpointIDE:
		return KiroEndpointIDE
	case KiroEndpointCLI:
		return KiroEndpointCLI
	}
	if authKind == KiroAuthKindAPIKey {
		return KiroEndpointCLI
	}
	return KiroEndpointIDE
}

// IsSocialLogin 判断是否为 Github / Google 登录。
func (c KiroCredential) IsSocialLogin() bool {
	if c.AuthKind == KiroAuthKindSocial {
		return true
	}
	source := strings.ToLower(strings.TrimSpace(c.LoginSource))
	return source == "github" || source == "google"
}

// AuthRegion 是 token 刷新使用的区域。
func (c KiroCredential) AuthRegion() string {
	if region := strings.TrimSpace(c.Region); region != "" {
		return region
	}
	return KiroDefaultRegion
}

// EffectiveAPIRegion 是数据面请求区域：显式 api_region > 真实 profileArn 的区域 >
// 凭据区域 > 默认区域。Enterprise profile 必须访问 profile 所属区域的端点。
func (c KiroCredential) EffectiveAPIRegion() string {
	if region := strings.TrimSpace(c.APIRegion); region != "" {
		return region
	}
	if region := kiroProfileARNRegion(c.EffectiveProfileARN()); region != "" {
		return region
	}
	return c.AuthRegion()
}

// EffectiveProfileARN 返回可发给上游的真实 ARN（跳过 Builder ID 占位符）。
func (c KiroCredential) EffectiveProfileARN() string {
	arn := strings.TrimSpace(c.ProfileARN)
	if arn == "" || arn == KiroBuilderIDProfileARN {
		return ""
	}
	return arn
}

// StreamingProfileARN 是对话与 MCP 请求必带的 ARN；缺失时按官方 IDE 回退到
// Social 共享 ARN 或 Builder ID 占位 ARN。API Key 凭据不带 ARN。
func (c KiroCredential) StreamingProfileARN() string {
	if c.AuthKind == KiroAuthKindAPIKey {
		return ""
	}
	if arn := strings.TrimSpace(c.ProfileARN); arn != "" {
		return arn
	}
	if c.IsSocialLogin() {
		return KiroSocialProfileARN
	}
	return KiroBuilderIDProfileARN
}

// TokenTypeHeader 是 CodeWhisperer 请求的 tokentype 头；没有则为空。
func (c KiroCredential) TokenTypeHeader() string {
	switch c.AuthKind {
	case KiroAuthKindAPIKey:
		return "API_KEY"
	case KiroAuthKindExternalIdP:
		return "EXTERNAL_IDP"
	}
	return ""
}

func kiroProfileARNRegion(arn string) string {
	parts := strings.Split(strings.TrimSpace(arn), ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[2] != "codewhisperer" || parts[3] == "" || parts[4] == "" {
		return ""
	}
	if !strings.HasPrefix(parts[5], "profile/") || len(parts[5]) == len("profile/") {
		return ""
	}
	return parts[3]
}

// NormalizeKiroMachineID 接受 64 位十六进制或 UUID（展开为 64 位），其余返回空。
func NormalizeKiroMachineID(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) == 64 && isHexString(value) {
		return strings.ToLower(value)
	}
	compact := strings.ReplaceAll(value, "-", "")
	if len(compact) == 32 && isHexString(compact) {
		compact = strings.ToLower(compact)
		return compact + compact
	}
	return ""
}

// DeriveKiroMachineID 在凭据未带 machineId 时按 kiro.rs 的规则派生；导入时落库，
// 之后 refresh token 轮换也不会改变设备指纹。
func DeriveKiroMachineID(raw, refreshToken, apiKey string) string {
	if normalized := NormalizeKiroMachineID(raw); normalized != "" {
		return normalized
	}
	seed := ""
	switch {
	case strings.TrimSpace(apiKey) != "":
		seed = "KiroAPIKey/" + strings.TrimSpace(apiKey)
	case strings.TrimSpace(refreshToken) != "":
		seed = "KotlinNativeAPI/" + strings.TrimSpace(refreshToken)
	default:
		random := make([]byte, 16)
		_, _ = rand.Read(random)
		seed = "KiroFallback/" + hex.EncodeToString(random)
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func isHexString(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

var kiroExternalIdPHostSuffixes = []string{".microsoftonline.com", ".microsoftonline.us", ".microsoftonline.cn"}

// ValidateKiroExternalIdPEndpoint 防止导入的 tokenEndpoint 把 refresh token 送到任意主机：
// 必须 https、非 IP、命中 Microsoft Entra 允许列表。导入与每次刷新前都校验。
func ValidateKiroExternalIdPEndpoint(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("IdP 端点 URL 无法解析: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("IdP 端点 URL 必须为 https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("IdP 端点 URL 缺少 host")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("IdP 端点 host 不能是 IP 字面量")
	}
	for _, suffix := range kiroExternalIdPHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return nil
		}
	}
	return fmt.Errorf("IdP 端点 host %q 不在允许列表内", host)
}

// KiroCredentialFromRow 从数据库行投影出 Kiro 身份信息。
func KiroCredentialFromRow(row *database.AccountRow) KiroCredential {
	if row == nil {
		return KiroCredential{}
	}
	get := func(key string) string { return strings.TrimSpace(row.GetCredential(key)) }
	cred := KiroCredential{
		LoginSource:   get(KiroLoginSourceCredentialKey),
		Region:        get(KiroRegionCredentialKey),
		APIRegion:     get(KiroAPIRegionCredentialKey),
		ClientID:      get(KiroClientIDCredentialKey),
		ClientSecret:  get(KiroClientSecretCredentialKey),
		StartURL:      get(KiroStartURLCredentialKey),
		TokenEndpoint: get(KiroTokenEndpointCredentialKey),
		IssuerURL:     get(KiroIssuerURLCredentialKey),
		Scopes:        get(KiroScopesCredentialKey),
		ProfileARN:    get(KiroProfileARNCredentialKey),
		MachineID:     get(KiroMachineIDCredentialKey),
	}
	cred.AuthKind = NormalizeKiroAuthKind(get(KiroAuthKindCredentialKey), cred.TokenEndpoint, get("api_key") != "", cred.ClientSecret != "")
	cred.Endpoint = NormalizeKiroEndpoint(get(KiroEndpointCredentialKey), cred.AuthKind)
	cred.MachineID = DeriveKiroMachineID(cred.MachineID, get("refresh_token"), get("api_key"))
	if resetAt, err := time.Parse(time.RFC3339, gjsonString(row.GetCredential(KiroUsageCredentialKey), "next_reset_at")); err == nil {
		cred.UsageResetAt = resetAt
	}
	return cred
}

// applyKiroRowLocked 在装载时填充 Kiro 专有字段。调用方持有账号的独占访问。
func (a *Account) applyKiroRowLocked(row *database.AccountRow) {
	a.Kiro = KiroCredentialFromRow(row)
	a.Email = strings.TrimSpace(row.GetCredential("email"))
	a.PlanType = strings.TrimSpace(row.GetCredential("plan_type"))
	if a.Kiro.AuthKind == KiroAuthKindAPIKey {
		a.RefreshToken = ""
		a.ExpiresAt = time.Time{}
		if a.Status != StatusError {
			a.HealthTier = HealthTierHealthy
		}
	}
	if a.PlanType == "" {
		a.PlanType = "kiro"
	}
}

func (a *Account) isKiroLocked() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamKiro) {
		return false
	}
	return strings.TrimSpace(a.APIKey) != "" || strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != ""
}

// IsKiro 判断账号是否为 Kiro 上游账号。
func (a *Account) IsKiro() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isKiroLocked()
}

// KiroRequestAuth 是一次上游请求所需的凭据快照。
type KiroRequestAuth struct {
	Credential  KiroCredential
	BearerToken string
	ExpiresAt   time.Time
	ProxyURL    string
}

// KiroRequestAuth 取一次请求用的凭据快照；API Key 账号的 bearer 就是 ksk_ 密钥。
func (a *Account) KiroRequestAuth() KiroRequestAuth {
	if a == nil {
		return KiroRequestAuth{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	bearer := strings.TrimSpace(a.AccessToken)
	if a.Kiro.AuthKind == KiroAuthKindAPIKey {
		bearer = strings.TrimSpace(a.APIKey)
	}
	return KiroRequestAuth{Credential: a.Kiro, BearerToken: bearer, ExpiresAt: a.ExpiresAt, ProxyURL: strings.TrimSpace(a.ProxyURL)}
}

// KiroRefreshable 报告上游 401 能否靠刷新恢复。
func (a *Account) KiroRefreshable() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isKiroLocked() && a.Kiro.AuthKind != KiroAuthKindAPIKey && strings.TrimSpace(a.RefreshToken) != ""
}

// KiroUsageResetAt 返回额度下次重置时间；未知时为零值。
func (a *Account) KiroUsageResetAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Kiro.UsageResetAt
}

func gjsonString(raw, path string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return ""
	}
	value, _ := parsed[path].(string)
	return value
}

// SetKiroProfileARN 在懒解析 Enterprise profile 后更新内存态。
func (a *Account) SetKiroProfileARN(arn string) {
	if a == nil || strings.TrimSpace(arn) == "" {
		return
	}
	a.mu.Lock()
	a.Kiro.ProfileARN = strings.TrimSpace(arn)
	a.mu.Unlock()
}
