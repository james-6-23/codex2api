package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// UpstreamGrok 标记 Grok CLI 上游账号（upstream_type 凭据字段取值）。
// 凭据形态有两种：
//   - OAuth：access_token + refresh_token（+ client_id），走 chat-proxy 上游，可自动刷新；
//   - API Key：api_key（xai-...），走官方 API 上游，永不过期。
const UpstreamGrok = "grok"

const (
	GrokAuthKindOAuth  = "oauth"
	GrokAuthKindAPIKey = "api_key"
)

// Grok 上游默认端点：OAuth 凭据走 Grok CLI 的 chat-proxy，API Key 走官方 xAI API。
// base_url 凭据字段可覆盖（留空 = 按凭据类型自动选择）。
//
// OAuth 浏览器授权常量与官方 Grok CLI 对齐：
//   - client_id 为 xAI 公开的 Grok CLI 客户端；
//   - redirect_uri 固定为 127.0.0.1:56121（上游仅注册该回调，浏览器打不开也无妨，
//     用户把跳转失败页的完整 URL 粘贴回管理台即可兑换 code）。
const (
	GrokDefaultChatProxyBaseURL = "https://cli-chat-proxy.grok.com/v1"
	GrokDefaultAPIBaseURL       = "https://api.x.ai/v1"
	GrokDefaultOIDCIssuer       = "https://auth.x.ai"
	GrokDefaultAuthorizeURL     = GrokDefaultOIDCIssuer + "/oauth2/authorize"
	GrokDefaultTokenURL         = GrokDefaultOIDCIssuer + "/oauth2/token"
	GrokDefaultOAuthClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	GrokDefaultOAuthScope       = "openid profile email offline_access grok-cli:access api:access"
	GrokDefaultOAuthRedirectURI = "http://127.0.0.1:56121/callback"

	// EnvGrokOAuthClientID 沿用既有 grokEnv 覆盖约定（类比 proxy 包的 GROK_CLIENT_VERSION / GROK_CLIENT_IDENTIFIER）：
	// 留空回退到 GrokDefaultOAuthClientID，默认行为零变化。服务于多 client_id 部署、灰度对照与端点测试。
	EnvGrokOAuthClientID = "GROK_OAUTH_CLIENT_ID"
	// EnvGrokOAuthHostAllowlist 是部署侧显式允许的额外 OAuth issuer/token
	// endpoint 主机列表（逗号分隔）。导入文件本身无权扩大该列表，避免恶意
	// auth.json 把 refresh token 或服务端请求导向任意主机。
	EnvGrokOAuthHostAllowlist = "GROK_OAUTH_HOST_ALLOWLIST"
)

// EffectiveGrokOAuthClientID 返回生效的 OAuth client_id，优先级从高到低：
// 环境变量 GROK_OAUTH_CLIENT_ID > 系统设置 grok_config.oauth_client_id > 内置的官方
// Grok CLI 公开 id。环境变量压在系统设置之上：它属于部署级配置，数据库里的值被误改
// 或前端配错时仍能从部署侧兜住，且不需要进后台就能改回来。
func EffectiveGrokOAuthClientID() string {
	if v := GrokOAuthClientIDFromEnv(); v != "" {
		return v
	}
	if v := ConfiguredGrokOAuthClientID(); v != "" {
		return v
	}
	return GrokDefaultOAuthClientID
}

// GrokOAuthClientIDFromEnv 返回环境变量里配的 client_id（去空格后为空表示未设）。
// 管理端用它告诉用户「系统设置里的值当前被环境变量盖掉了」。
func GrokOAuthClientIDFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvGrokOAuthClientID))
}

// configuredGrokOAuthClientID 是系统设置里配的 client_id，随设置热更新。
var configuredGrokOAuthClientID atomic.Value // string

// SetConfiguredGrokOAuthClientID 热更新系统设置里的 client_id（空 = 回落到内置默认）。
func SetConfiguredGrokOAuthClientID(clientID string) {
	configuredGrokOAuthClientID.Store(NormalizeGrokOAuthClientID(clientID))
}

// ConfiguredGrokOAuthClientID 返回系统设置里配的 client_id，未配置时为空。
func ConfiguredGrokOAuthClientID() string {
	v, _ := configuredGrokOAuthClientID.Load().(string)
	return v
}

// GrokOAuthClientIDMaxLen 是 client_id 的长度上限。官方 id 是 36 字符的 UUID，
// 留出余量即可；这个值会进授权 URL 与 token 表单，不接受超长/带空白的输入。
const GrokOAuthClientIDMaxLen = 128

// NormalizeGrokOAuthClientID 归一化 client_id：去首尾空白，含空白或控制字符、
// 超长的一律视为未配置（返回空 = 回落到上一级）。
func NormalizeGrokOAuthClientID(clientID string) string {
	v := strings.TrimSpace(clientID)
	if v == "" || len(v) > GrokOAuthClientIDMaxLen {
		return ""
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ""
		}
	}
	return v
}

func (a *Account) isGrokAPILocked() bool {
	if a == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamGrok) {
		return false
	}
	return strings.TrimSpace(a.APIKey) != "" ||
		strings.TrimSpace(a.AccessToken) != "" ||
		strings.TrimSpace(a.RefreshToken) != ""
}

// IsGrokAPI 判断账号是否为 Grok 上游账号。
func (a *Account) IsGrokAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isGrokAPILocked()
}

// isRelayStyleLocked：openai_responses 中转或 Grok —— 一切「非 Codex OAuth 官方上游」
// 的账号。这类账号不参与 Codex 专属行为（wham 探针、WS 上游、manifest、alpha search）。
func (a *Account) isRelayStyleLocked() bool {
	return a.isOpenAIResponsesAPILocked() || a.isGrokAPILocked() || a.isAntigravityAPILocked() || a.isClaudeOAuthLocked()
}

// IsRelayStyle 判断账号是否为「非 Codex 官方」的外部上游账号。
func (a *Account) IsRelayStyle() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isRelayStyleLocked()
}

// GrokAuthKind 返回 Grok 账号的凭据类型（api_key / oauth）；非 Grok 账号返回空。
func (a *Account) GrokAuthKind() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return ""
	}
	if strings.TrimSpace(a.APIKey) != "" {
		return GrokAuthKindAPIKey
	}
	return GrokAuthKindOAuth
}

// GrokCredentials 返回 Grok 账号的上游 base URL 与 Bearer 凭据。
// base_url 未配置时按凭据类型选默认端点。bearer 为空表示 OAuth 账号尚未刷出 AT。
func (a *Account) GrokCredentials() (baseURL, bearer string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return "", ""
	}
	baseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if apiKey := strings.TrimSpace(a.APIKey); apiKey != "" {
		if baseURL == "" {
			baseURL = GrokDefaultAPIBaseURL
		}
		return baseURL, apiKey
	}
	if baseURL == "" {
		baseURL = GrokDefaultChatProxyBaseURL
	}
	return baseURL, strings.TrimSpace(a.AccessToken)
}

// GrokRateLimitSnapshot 是 Grok 上游逐请求返回的配额余量（x-ratelimit-* 响应头）。
// 内存实时更新;由 store 后台循环按分钟批量落库(grok_rate_limit 凭据),重启后恢复,
// 账号列表的用量进度条不再因容器重启清零。
type GrokRateLimitSnapshot struct {
	LimitTokens       int64     `json:"limit_tokens,omitempty"`
	RemainingTokens   int64     `json:"remaining_tokens,omitempty"`
	LimitRequests     int64     `json:"limit_requests,omitempty"`
	RemainingRequests int64     `json:"remaining_requests,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// SetGrokRateLimitSnapshot 更新配额余量快照（时间倒流的旧观测被忽略）。
func (a *Account) SetGrokRateLimitSnapshot(snap GrokRateLimitSnapshot) {
	a.setGrokRateLimitSnapshot(snap, true)
	// 余量头是调度模式（剩余配额/顺序耗尽）的排序键来源，通知调度器重评桶内位置。
	a.notifySchedulerUsageChanged()
}

// setGrokRateLimitSnapshot 的 markDirty=false 供启动恢复用:恢复的值本来就来自
// 库里,不需要再触发一轮落库。
func (a *Account) setGrokRateLimitSnapshot(snap GrokRateLimitSnapshot, markDirty bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grokRateLimit != nil && snap.UpdatedAt.Before(a.grokRateLimit.UpdatedAt) {
		return
	}
	copied := snap
	a.grokRateLimit = &copied
	if markDirty {
		a.grokRateLimitDirty = true
		a.grokRateLimitVersion++
	}
}

// TakeGrokRateLimitSnapshotIfDirty 返回自上次持久化后有更新的快照并清除脏位;
// 无更新时 ok=false。供 store 的分钟级批量落库循环调用。
func (a *Account) TakeGrokRateLimitSnapshotIfDirty() (GrokRateLimitSnapshot, bool) {
	if a == nil {
		return GrokRateLimitSnapshot{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.grokRateLimitDirty || a.grokRateLimit == nil {
		return GrokRateLimitSnapshot{}, false
	}
	a.grokRateLimitDirty = false
	return *a.grokRateLimit, true
}

// PeekGrokRateLimitSnapshotIfDirty does not acknowledge persistence. The
// caller must invoke ConfirmGrokRateLimitSnapshotPersisted after a successful
// database commit; failures retain dirty state for the next flush.
func (a *Account) PeekGrokRateLimitSnapshotIfDirty() (GrokRateLimitSnapshot, uint64, bool) {
	if a == nil {
		return GrokRateLimitSnapshot{}, 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.grokRateLimitDirty || a.grokRateLimit == nil {
		return GrokRateLimitSnapshot{}, 0, false
	}
	return *a.grokRateLimit, a.grokRateLimitVersion, true
}

func (a *Account) ConfirmGrokRateLimitSnapshotPersisted(version uint64) {
	if a == nil || version == 0 {
		return
	}
	a.mu.Lock()
	if a.grokRateLimitVersion == version {
		a.grokRateLimitDirty = false
	}
	a.mu.Unlock()
}

// GetGrokRateLimitSnapshot 返回配额余量快照；无观测时 ok=false。
func (a *Account) GetGrokRateLimitSnapshot() (GrokRateLimitSnapshot, bool) {
	if a == nil {
		return GrokRateLimitSnapshot{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.grokRateLimit == nil {
		return GrokRateLimitSnapshot{}, false
	}
	return *a.grokRateLimit, true
}

// SetGrokContextWindow 记录上游 x-grok-context-window 的观测值（非正数忽略）。
func (a *Account) SetGrokContextWindow(window int64) {
	if a == nil || window <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grokContextWindow = window
}

// GetGrokContextWindow 返回上下文窗口观测值；未观测到时返回 0。
func (a *Account) GetGrokContextWindow() int64 {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.grokContextWindow
}

// GrokFreeQuotaSnapshot 是免费额度耗尽时从上游 429 错误体解析出的权威用量
// （tokens (actual/limit)，滚动 24h 窗口）。随 credentials 落库，重启后恢复。
type GrokFreeQuotaSnapshot struct {
	UsedTokens  int64     `json:"used_tokens"`
	LimitTokens int64     `json:"limit_tokens"`
	Model       string    `json:"model,omitempty"`
	ExhaustedAt time.Time `json:"exhausted_at"`
}

// SetGrokFreeQuotaSnapshot 更新免费额度耗尽快照（时间倒流的旧观测被忽略）。
func (a *Account) SetGrokFreeQuotaSnapshot(snap GrokFreeQuotaSnapshot) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grokFreeQuota != nil && snap.ExhaustedAt.Before(a.grokFreeQuota.ExhaustedAt) {
		return
	}
	copied := snap
	a.grokFreeQuota = &copied
}

// GetGrokFreeQuotaSnapshot 返回免费额度耗尽快照；无观测时 ok=false。
func (a *Account) GetGrokFreeQuotaSnapshot() (GrokFreeQuotaSnapshot, bool) {
	if a == nil {
		return GrokFreeQuotaSnapshot{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.grokFreeQuota == nil {
		return GrokFreeQuotaSnapshot{}, false
	}
	return *a.grokFreeQuota, true
}

// GrokChannelSupportsModel 判断 Grok 账号能否服务指定模型（grok 渠道 Key 专用）。
// 显式 Models 白名单优先；没有白名单时使用富目录的可见模型，尚未同步目录才使用
// 按凭据类型区分的保守默认集。空列表绝不再表示任意模型透传。
func (a *Account) GrokChannelSupportsModel(model string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return false
	}
	model = strings.TrimSpace(model)
	// 不复用 a.Models 的底层数组做 append:len==0 但 cap>0 时,两个并发请求会
	// 在共享 RLock 下向同一空闲容量写入,构成写-写竞态。目录分支从 nil 开始。
	var candidates []string
	if len(a.Models) > 0 {
		candidates = a.Models
	} else if a.grokRouting != nil && a.grokRouting.CatalogKnown {
		for _, route := range a.grokRouting.Models {
			if route.Hidden || (a.GrokAuthKindLocked() == GrokAuthKindAPIKey && route.SupportedInAPI != nil && !*route.SupportedInAPI) {
				continue
			}
			candidates = append(candidates, route.ModelID)
		}
	}
	// A successfully fetched empty catalog is authoritative. It must not be
	// confused with "catalog has never been fetched" and reopen defaults.
	if len(candidates) == 0 && (a.grokRouting == nil || !a.grokRouting.CatalogKnown) {
		if a.GrokAuthKindLocked() == GrokAuthKindAPIKey {
			candidates = GrokAPIKeyDefaultModelIDs()
		} else {
			candidates = GrokOAuthDefaultModelIDs()
		}
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

// GrokModels 返回 Grok 账号显式声明的模型白名单；空表示交由富目录/保守默认，
// 不表示任意模型透传。
func (a *Account) GrokModels() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return nil
	}
	return cloneStringSlice(a.Models)
}

// GrokUserID 返回 Grok 账号的上游用户标识（JWT sub，导入时存入 account_id）。
func (a *Account) GrokUserID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return ""
	}
	return strings.TrimSpace(a.AccountID)
}

// NormalizeGrokBaseURL 校验 Grok 账号的 base_url 覆盖值；空串合法（自动选默认端点）。
func NormalizeGrokBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base_url 必须是完整的 http/https URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("base_url 仅支持 http/https")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

// ==================== auth.json 导入解析 ====================

// GrokImportedCredential 是从 Grok CLI auth.json 解析出的一条逻辑凭据。
type GrokImportedCredential struct {
	AccessToken  string
	RefreshToken string
	APIKey       string
	// PlanType 是首版双写兼容字段：优先为导入文件的 archive label，文件未
	// 声明时回退到未验签 JWT tier。安全筛选不得读取它；live plan 另行保存。
	PlanType        string
	ArchivePlanType string
	JWTPlanType     string
	JWTPlanTrusted  bool
	Disabled        bool
	DisabledPresent bool
	ClientID        string
	TokenEndpoint   string
	OIDCIssuer      string
	Subject         string
	Email           string
	PrincipalType   string
	PrincipalID     string
	ExpiresAt       time.Time
}

// AuthKind 返回该凭据的类型（api_key / oauth）。
func (c *GrokImportedCredential) AuthKind() string {
	if c != nil && strings.TrimSpace(c.APIKey) != "" {
		return GrokAuthKindAPIKey
	}
	return GrokAuthKindOAuth
}

// ParseGrokAuthJSON 解析 Grok CLI 的 auth.json。兼容三种布局：
// 顶层单凭据、tokens 包装、多 scope（一个文件含多条逻辑凭据，全部返回）。
// scope 为 xai::api_key 的条目按 API Key 凭据处理。
func ParseGrokAuthJSON(raw []byte) ([]*GrokImportedCredential, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("auth.json 不是合法的 JSON: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("auth.json 必须是 JSON 对象")
	}

	type candidate struct {
		scope string
		node  map[string]any
	}
	isCredNode := func(node map[string]any) bool {
		return grokFirstString(node, "access_token", "AccessToken", "refresh_token", "RefreshToken", "key", "session_token", "SessionToken") != ""
	}
	var candidates []candidate
	if isCredNode(root) {
		candidates = append(candidates, candidate{node: root})
	} else {
		container := root
		if tokens, ok := root["tokens"].(map[string]any); ok {
			if isCredNode(tokens) {
				candidates = append(candidates, candidate{node: tokens})
			} else {
				container = tokens
			}
		}
		if len(candidates) == 0 {
			for scope, value := range container {
				if node, ok := value.(map[string]any); ok && isCredNode(node) {
					candidates = append(candidates, candidate{scope: scope, node: node})
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("auth.json 中没有可用凭据（缺少 access_token/refresh_token/key）")
	}

	result := make([]*GrokImportedCredential, 0, len(candidates))
	for _, cand := range candidates {
		// disabled/plan_type 是 CPA/Grok 归档文件的顶层元数据；tokens 或
		// scope 包装不应令它们在解析逻辑凭据时丢失。子节点显式值优先。
		node := grokInheritArchiveMetadata(cand.node, root)
		cred, err := parseGrokCredentialNode(cand.scope, node)
		if err != nil {
			return nil, err
		}
		result = append(result, cred)
	}
	return result, nil
}

func parseGrokCredentialNode(scope string, node map[string]any) (*GrokImportedCredential, error) {
	access := grokFirstString(node, "access_token", "AccessToken", "key", "session_token", "SessionToken")
	refresh := grokFirstString(node, "refresh_token", "RefreshToken")
	if access == "" && refresh == "" {
		return nil, fmt.Errorf("凭据缺少 access_token 和 refresh_token")
	}

	scopeNorm := strings.ToLower(strings.TrimSpace(scope))
	authMode := strings.ToLower(grokFirstString(node, "auth_mode", "authMode", "auth_kind", "authKind"))
	isAPIKey := authMode == "api_key" || authMode == "apikey" || authMode == "api-key" ||
		strings.Contains(scopeNorm, "api_key")

	claims := grokJWTClaims(access)
	archivePlan := strings.TrimSpace(grokFirstString(node, "plan_type", "planType"))
	jwtPlan := GrokPlanTypeFromAccessToken(access)
	legacyPlan := archivePlan
	if legacyPlan == "" {
		legacyPlan = jwtPlan
	}
	disabled, disabledPresent := grokOptionalBool(node, "disabled")
	cred := &GrokImportedCredential{
		AccessToken:     access,
		RefreshToken:    refresh,
		PlanType:        legacyPlan,
		ArchivePlanType: archivePlan,
		JWTPlanType:     jwtPlan,
		// JWT payload parsing here is intentionally unverified. It remains useful
		// as a display hint, never as an authorization or plan-routing fact.
		JWTPlanTrusted:  false,
		Disabled:        disabled,
		DisabledPresent: disabledPresent,
		ClientID:        grokFirstString(node, "client_id", "clientId", "oidc_client_id", "oidcClientId"),
		TokenEndpoint:   grokFirstString(node, "token_endpoint", "tokenEndpoint"),
		OIDCIssuer:      strings.TrimRight(grokFirstString(node, "oidc_issuer", "oidcIssuer", "issuer"), "/"),
		PrincipalType:   grokFirstString(node, "principal_type", "principalType"),
		PrincipalID:     grokFirstString(node, "principal_id", "principalId", "team_id", "teamId"),
	}
	if isAPIKey {
		cred.APIKey = access
		cred.AccessToken = ""
		cred.RefreshToken = ""
		return cred, nil
	}

	cred.Subject = grokFirstString(node, "user_id", "userId", "UserId", "sub")
	if cred.Subject == "" && claims != nil {
		cred.Subject = grokClaimString(claims, "sub")
	}
	// email：优先文件顶层字段，其次 id_token（CPA 文件把 email 放在 id_token 里），
	// 最后回退 access_token claims。
	cred.Email = grokFirstString(node, "email", "Email")
	if cred.Email == "" {
		if idClaims := grokJWTClaims(grokFirstString(node, "id_token", "idToken", "IdToken")); idClaims != nil {
			cred.Email = grokClaimString(idClaims, "email")
		}
	}
	if cred.Email == "" && claims != nil {
		cred.Email = grokClaimString(claims, "email")
	}
	if cred.PrincipalID == "" && claims != nil {
		cred.PrincipalID = grokClaimString(claims, "principal_id")
	}
	if cred.Subject == "" {
		cred.Subject = cred.PrincipalID
	}
	if cred.ClientID == "" && claims != nil {
		cred.ClientID = grokClaimString(claims, "client_id")
	}
	if expires := grokFirstString(node, "expired", "expires_at", "ExpiresAt", "expiry", "expiration"); expires != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, expires); err == nil {
				cred.ExpiresAt = t
				break
			}
		}
	}
	if cred.ExpiresAt.IsZero() && claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			cred.ExpiresAt = time.Unix(int64(exp), 0)
		}
	}
	if refresh == "" {
		return nil, fmt.Errorf("OAuth 凭据缺少 refresh_token，无法长期使用（如为 API Key 请设置 auth_mode=api_key）")
	}
	return cred, nil
}

func grokInheritArchiveMetadata(node, root map[string]any) map[string]any {
	if node == nil || root == nil {
		return node
	}
	result := make(map[string]any, len(node)+2)
	for key, value := range node {
		result[key] = value
	}
	for _, key := range []string{"disabled", "plan_type", "planType"} {
		if _, exists := result[key]; exists {
			continue
		}
		if value, exists := root[key]; exists {
			result[key] = value
		}
	}
	return result
}

func grokOptionalBool(node map[string]any, key string) (bool, bool) {
	value, ok := node[key]
	if !ok || value == nil {
		return false, ok
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return false, true
	}
}

// GrokCredentialFamilyID derives a stable, irreversible family identifier from
// identity attributes which survive AT/RT rotation. It deliberately excludes
// model/configuration fields. A token hash is only a last-resort fallback for
// malformed legacy credentials lacking a principal.
func GrokCredentialFamilyID(cred *GrokImportedCredential, baseURL string) string {
	if cred == nil {
		return ""
	}
	authKind := cred.AuthKind()
	issuer := strings.ToLower(strings.TrimRight(strings.TrimSpace(cred.OIDCIssuer), "/"))
	if issuer == "" && authKind == GrokAuthKindOAuth {
		issuer = GrokDefaultOIDCIssuer
	}
	origin := grokCredentialOrigin(baseURL, authKind)
	principal := firstNonEmptyTrimmed(cred.PrincipalID, cred.Subject)
	identity := strings.Join([]string{
		"grok-credential-family-v1",
		authKind,
		issuer,
		strings.TrimSpace(cred.ClientID),
		strings.ToLower(strings.TrimSpace(cred.PrincipalType)),
		strings.TrimSpace(principal),
		origin,
	}, "\x00")
	if principal == "" {
		fallback := firstNonEmptyTrimmed(cred.APIKey, cred.RefreshToken, cred.AccessToken)
		if fallback == "" {
			return ""
		}
		identity += "\x00fallback\x00" + fallback
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func grokCredentialOrigin(raw, authKind string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if authKind == GrokAuthKindAPIKey {
			raw = GrokDefaultAPIBaseURL
		} else {
			raw = GrokDefaultChatProxyBaseURL
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port != "" && port != "443" && port != "80" {
		host += ":" + port
	}
	return strings.ToLower(parsed.Scheme) + "://" + host
}

// grokParseTime 解析 Grok 上游的时间字符串（RFC3339 及带纳秒变体），失败返回零值。
func grokParseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func grokFirstString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := node[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// grokJWTClaims 解析 JWT payload（不验签），非 JWT 返回 nil。
func grokJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		if decoded, err = base64.StdEncoding.DecodeString(payload); err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil
	}
	return claims
}

func grokClaimString(claims map[string]any, key string) string {
	if value, ok := claims[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// ==================== OAuth 浏览器授权（PKCE） ====================

// GrokTokenData 是一次 Grok OAuth 刷新 / 授权兑换的结果。
type GrokTokenData struct {
	AccessToken  string
	RefreshToken string // 上游轮换时非空
	IDToken      string
	PlanType     string
	ExpiresAt    time.Time
}

// GrokAuthURLParams 描述生成 xAI 授权链接所需字段。
type GrokAuthURLParams struct {
	State         string
	CodeChallenge string
	RedirectURI   string
	Nonce         string
	ClientID      string
	Scope         string
	AuthorizeURL  string
}

// BuildGrokAuthorizationURL 组装 xAI OAuth 授权链接（S256 PKCE）。
func BuildGrokAuthorizationURL(params GrokAuthURLParams) (string, error) {
	state := strings.TrimSpace(params.State)
	challenge := strings.TrimSpace(params.CodeChallenge)
	if state == "" || challenge == "" {
		return "", fmt.Errorf("state 与 code_challenge 均为必填")
	}
	redirectURI := strings.TrimSpace(params.RedirectURI)
	if redirectURI == "" {
		redirectURI = GrokDefaultOAuthRedirectURI
	}
	clientID := strings.TrimSpace(params.ClientID)
	if clientID == "" {
		clientID = EffectiveGrokOAuthClientID()
	}
	scope := strings.TrimSpace(params.Scope)
	if scope == "" {
		scope = GrokDefaultOAuthScope
	}
	authorizeURL := strings.TrimSpace(params.AuthorizeURL)
	if authorizeURL == "" {
		authorizeURL = GrokDefaultAuthorizeURL
	}
	if parsed, err := url.Parse(authorizeURL); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("authorize_url 无效")
	}

	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", scope)
	query.Set("state", state)
	if nonce := strings.TrimSpace(params.Nonce); nonce != "" {
		query.Set("nonce", nonce)
	}
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("plan", "generic")
	query.Set("referrer", "codex2api")
	return authorizeURL + "?" + query.Encode(), nil
}

// GrokAuthorizationInput 是从回调 URL / query / 裸 code 解析出的授权码。
type GrokAuthorizationInput struct {
	Code          string
	State         string
	RequiresState bool // 输入含 query 时要求校验 state
}

// ParseGrokAuthorizationInput 接受完整回调 URL、query string 或裸 authorization code。
func ParseGrokAuthorizationInput(raw string) GrokAuthorizationInput {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return GrokAuthorizationInput{}
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed != nil {
		values := parsed.Query()
		if code := strings.TrimSpace(values.Get("code")); code != "" {
			return GrokAuthorizationInput{
				Code:          code,
				State:         strings.TrimSpace(values.Get("state")),
				RequiresState: true,
			}
		}
	}
	queryCandidate := strings.TrimPrefix(trimmed, "?")
	if strings.Contains(queryCandidate, "=") {
		if values, err := url.ParseQuery(queryCandidate); err == nil {
			if code := strings.TrimSpace(values.Get("code")); code != "" {
				return GrokAuthorizationInput{
					Code:          code,
					State:         strings.TrimSpace(values.Get("state")),
					RequiresState: true,
				}
			}
		}
	}
	return GrokAuthorizationInput{Code: trimmed}
}

// GrokExchangeCodeParams 描述 authorization_code 兑换所需字段。
type GrokExchangeCodeParams struct {
	Code          string
	CodeVerifier  string
	RedirectURI   string
	ClientID      string
	TokenEndpoint string
	OIDCIssuer    string
	ProxyURL      string
}

// ExchangeGrokAuthorizationCode 用 authorization_code + PKCE verifier 兑换 token。
func ExchangeGrokAuthorizationCode(ctx context.Context, params GrokExchangeCodeParams) (*GrokTokenData, error) {
	code := strings.TrimSpace(params.Code)
	verifier := strings.TrimSpace(params.CodeVerifier)
	if code == "" {
		return nil, fmt.Errorf("authorization code 为空")
	}
	if verifier == "" {
		return nil, fmt.Errorf("code_verifier 为空")
	}
	clientID := strings.TrimSpace(params.ClientID)
	if clientID == "" {
		clientID = EffectiveGrokOAuthClientID()
	}
	redirectURI := strings.TrimSpace(params.RedirectURI)
	if redirectURI == "" {
		redirectURI = GrokDefaultOAuthRedirectURI
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := ConfigureTransportProxy(transport, params.ProxyURL, nil); err != nil {
		return nil, fmt.Errorf("grok 授权兑换代理配置失败: %w", err)
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	endpoint, err := grokResolveTokenEndpoint(ctx, client, params.TokenEndpoint, params.OIDCIssuer)
	if err != nil {
		// discovery 失败时回落到已知默认 token 端点
		if strings.TrimSpace(params.TokenEndpoint) == "" {
			endpoint = GrokDefaultTokenURL
		} else {
			return nil, err
		}
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok 授权码兑换请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("grok 授权码兑换响应读取失败: %w", err)
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Code         string `json:"code"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &payload)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := strings.ToLower(firstNonEmptyTrimmed(payload.Error, payload.Code))
		if code == "" {
			code = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		detail := firstNonEmptyTrimmed(payload.ErrorDesc, code)
		return nil, fmt.Errorf("grok 授权码兑换失败: %s (status=%d)", detail, resp.StatusCode)
	}
	if payload.AccessToken == "" {
		return nil, fmt.Errorf("grok 授权码兑换响应缺少 access_token")
	}
	if payload.RefreshToken == "" {
		return nil, fmt.Errorf("grok 授权码兑换响应缺少 refresh_token（需 offline_access scope）")
	}

	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 21600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	if claims := grokJWTClaims(payload.AccessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			expiresAt = time.Unix(int64(exp), 0)
		}
	}
	return &GrokTokenData{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		IDToken:      payload.IDToken,
		PlanType:     GrokPlanTypeFromAccessToken(payload.AccessToken),
		ExpiresAt:    expiresAt,
	}, nil
}

// GrokIdentityFromTokens 从 id_token / access_token 提取 email 与 subject（不验签）。
// id_token 优先（含 email 声明），access_token 兜底 subject。
func GrokIdentityFromTokens(accessToken, idToken string) (email, subject string) {
	email, subject = parseGrokIDTokenIdentity(idToken)
	if subject == "" {
		subject = GrokSubjectFromAccessToken(accessToken)
	}
	if email == "" {
		if claims := grokJWTClaims(accessToken); claims != nil {
			email = grokClaimString(claims, "email")
		}
	}
	return email, subject
}

// GrokSubjectFromAccessToken 从 access_token JWT 提取 sub（不验签）。
func GrokSubjectFromAccessToken(accessToken string) string {
	claims := grokJWTClaims(accessToken)
	if claims == nil {
		return ""
	}
	return grokClaimString(claims, "sub")
}

// ==================== OAuth 刷新 ====================

// grokDiscoveryCache 缓存 OIDC discovery 出的 token_endpoint（1 小时）。
var grokDiscoveryCache = struct {
	sync.RWMutex
	entries map[string]struct {
		endpoint string
		at       time.Time
	}
}{entries: make(map[string]struct {
	endpoint string
	at       time.Time
})}

func grokAllowedOAuthURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil {
		return false
	}
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if hostname == "auth.x.ai" {
		return u.Port() == "" || u.Port() == "443"
	}
	for _, raw := range strings.Split(os.Getenv(EnvGrokOAuthHostAllowlist), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		allowedHost := raw
		allowedPort := ""
		if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
			if !strings.EqualFold(parsed.Scheme, "https") {
				continue
			}
			allowedHost = parsed.Hostname()
			allowedPort = parsed.Port()
		} else if parsed, err := url.Parse("https://" + raw); err == nil && parsed.Hostname() != "" {
			allowedHost = parsed.Hostname()
			allowedPort = parsed.Port()
		}
		if hostname != strings.ToLower(strings.TrimSuffix(allowedHost, ".")) {
			continue
		}
		// A configured port is exact. A host-only allowlist permits the normal
		// HTTPS port only, rather than silently opening arbitrary services on it.
		if allowedPort != "" {
			if u.Port() == allowedPort {
				return true
			}
			continue
		}
		if u.Port() == "" || u.Port() == "443" {
			return true
		}
	}
	return false
}

// ValidateGrokOAuthEndpoints enforces the deployment-owned OAuth host policy.
// Imported JSON may select a path on an allowed host, but cannot introduce a
// new network destination. Empty values resolve to the official xAI issuer.
func ValidateGrokOAuthEndpoints(tokenEndpoint, issuer string) error {
	if tokenEndpoint = strings.TrimSpace(tokenEndpoint); tokenEndpoint != "" {
		parsed, err := url.Parse(tokenEndpoint)
		if err != nil || !grokAllowedOAuthURL(parsed) {
			return fmt.Errorf("grok token_endpoint 必须使用官方 xAI 主机或服务端 OAuth allowlist 中的 HTTPS 主机")
		}
	}
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		issuer = GrokDefaultOIDCIssuer
	}
	parsed, err := url.Parse(issuer)
	if err != nil || !grokAllowedOAuthURL(parsed) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("grok oidc_issuer 必须使用官方 xAI 主机或服务端 OAuth allowlist 中的 HTTPS origin")
	}
	return nil
}

func grokResolveTokenEndpoint(ctx context.Context, client *http.Client, tokenURL, issuer string) (string, error) {
	if err := ValidateGrokOAuthEndpoints(tokenURL, issuer); err != nil {
		return "", err
	}
	if tokenURL = strings.TrimSpace(tokenURL); tokenURL != "" {
		parsed, err := url.Parse(tokenURL)
		if err != nil || !grokAllowedOAuthURL(parsed) {
			return "", fmt.Errorf("grok token_endpoint 无效")
		}
		return parsed.String(), nil
	}
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		issuer = GrokDefaultOIDCIssuer
	}
	grokDiscoveryCache.RLock()
	cached, ok := grokDiscoveryCache.entries[issuer]
	grokDiscoveryCache.RUnlock()
	if ok && time.Since(cached.at) < time.Hour {
		return cached.endpoint, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("grok OIDC discovery 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("grok OIDC discovery 失败 (status=%d)", resp.StatusCode)
	}
	var document struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("grok OIDC discovery 响应解析失败: %w", err)
	}
	endpointURL, parseErr := url.Parse(document.TokenEndpoint)
	if document.TokenEndpoint == "" || parseErr != nil || !grokAllowedOAuthURL(endpointURL) {
		return "", fmt.Errorf("grok OIDC discovery 未返回可用的 token_endpoint")
	}
	grokDiscoveryCache.Lock()
	grokDiscoveryCache.entries[issuer] = struct {
		endpoint string
		at       time.Time
	}{endpoint: document.TokenEndpoint, at: time.Now()}
	grokDiscoveryCache.Unlock()
	return document.TokenEndpoint, nil
}

// GrokRefreshParams 描述一次 Grok OAuth refresh_token 交换所需的全部字段。
type GrokRefreshParams struct {
	RefreshToken  string
	ClientID      string
	TokenEndpoint string
	OIDCIssuer    string
	PrincipalType string
	PrincipalID   string
	ProxyURL      string
}

// grokRefreshPermanentError 标记不可重试的刷新失败（invalid_grant / invalid_client），
// 账号应转入 error 状态而非退避重试。
type grokRefreshPermanentError struct{ code string }

func (e *grokRefreshPermanentError) Error() string {
	return "grok OAuth 刷新永久失败: " + e.code
}

// IsGrokRefreshPermanentError 判断刷新错误是否为永久失败（RT 已失效）。
func IsGrokRefreshPermanentError(err error) bool {
	var permanent *grokRefreshPermanentError
	return errors.As(err, &permanent)
}

// RefreshGrokAccessToken 用 refresh_token 交换新的 Grok access_token。
func RefreshGrokAccessToken(ctx context.Context, params GrokRefreshParams) (*GrokTokenData, error) {
	if strings.TrimSpace(params.RefreshToken) == "" {
		return nil, fmt.Errorf("grok refresh_token 为空")
	}
	if strings.TrimSpace(params.ClientID) == "" {
		// 浏览器授权链路默认使用 Grok CLI 公开 client_id
		params.ClientID = EffectiveGrokOAuthClientID()
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := ConfigureTransportProxy(transport, params.ProxyURL, nil); err != nil {
		return nil, fmt.Errorf("grok 刷新代理配置失败: %w", err)
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	endpoint, err := grokResolveTokenEndpoint(ctx, client, params.TokenEndpoint, params.OIDCIssuer)
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {params.RefreshToken},
		"client_id":     {params.ClientID},
	}
	if params.PrincipalType != "" {
		form.Set("principal_type", params.PrincipalType)
	}
	if params.PrincipalID != "" {
		form.Set("principal_id", params.PrincipalID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok token 刷新请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("grok token 刷新响应读取失败: %w", err)
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Code         string `json:"code"`
	}
	_ = json.Unmarshal(body, &payload)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := strings.ToLower(firstNonEmptyTrimmed(payload.Error, payload.Code))
		if code == "invalid_grant" || code == "invalid_client" {
			return nil, &grokRefreshPermanentError{code: code}
		}
		if code == "" {
			code = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		return nil, fmt.Errorf("grok token 刷新失败: %s (status=%d)", code, resp.StatusCode)
	}
	if payload.AccessToken == "" {
		return nil, fmt.Errorf("grok token 刷新响应缺少 access_token")
	}

	expiresIn := payload.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 21600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	if claims := grokJWTClaims(payload.AccessToken); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			expiresAt = time.Unix(int64(exp), 0)
		}
	}
	return &GrokTokenData{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		IDToken:      payload.IDToken,
		PlanType:     GrokPlanTypeFromAccessToken(payload.AccessToken),
		ExpiresAt:    expiresAt,
	}, nil
}

// refreshGrokAccount 刷新 Grok OAuth 账号的 AT。API Key 账号无需刷新，直接返回。
// 与 Codex 刷新共用 tokenCache 的跨进程刷新锁，避免多副本重复消费 refresh token
// （Grok 的 RT 家族会轮换，重复消费会导致 invalid_grant）。
func (s *Store) refreshGrokAccount(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
	authKindIsAPIKey := strings.TrimSpace(acc.APIKey) != ""
	rt := acc.RefreshToken
	dbID := acc.DBID
	clientID := acc.GrokClientID
	tokenEndpoint := acc.GrokTokenEndpoint
	oidcIssuer := acc.GrokOIDCIssuer
	principalType := acc.GrokPrincipalType
	principalID := acc.GrokPrincipalID
	generation := acc.CredentialGeneration
	familyID := acc.CredentialFamilyID
	acc.mu.RUnlock()

	if authKindIsAPIKey {
		return nil
	}
	if strings.TrimSpace(rt) == "" {
		return fmt.Errorf("grok refresh_token 为空")
	}
	if generation <= 0 {
		generation = 1
	}

	// A stable family lease covers the whole RT rotation boundary. Re-read the
	// complete credential document after acquiring it: another instance may
	// already have advanced generation while this caller was waiting.
	if strings.TrimSpace(familyID) == "" && s.db != nil {
		ensured, ensureErr := s.db.EnsureAccountCredentialFamilyID(ctx, dbID, "")
		if ensureErr != nil {
			return fmt.Errorf("grok credential family 初始化失败: %w", ensureErr)
		}
		familyID = ensured
		if strings.TrimSpace(familyID) == "" {
			return fmt.Errorf("grok credential family 初始化失败: family id 为空")
		}
	}
	var refreshLocks *grokOAuthRefreshLocks
	if familyID != "" {
		locks, leaseErr := s.acquireGrokOAuthRefreshLocks(ctx, dbID, familyID)
		if leaseErr != nil {
			return leaseErr
		}
		refreshLocks = locks
		defer refreshLocks.Release()
		ctx = refreshLocks.Context()
		// Always reload after both rolling-upgrade locks are held. Old versions
		// write rotated AT/RT without advancing credential_generation, so checking
		// generation alone cannot detect that they refreshed while this instance
		// was waiting on the legacy account-id lock.
		if changed, usable, reloadErr := s.reloadGrokCredentialsAfterFamilyLease(ctx, acc, generation); reloadErr != nil {
			return reloadErr
		} else if changed && usable {
			// A forced caller that waited for an already-completed rotation belongs
			// to the same refresh batch. Reusing the freshly committed AT prevents a
			// second forced call from immediately consuming the newly rotated RT.
			s.finishReloadedOAuthRefresh(ctx, acc)
			return nil
		}
		acc.mu.RLock()
		authKindIsAPIKey = strings.TrimSpace(acc.APIKey) != ""
		rt, clientID, tokenEndpoint, oidcIssuer = acc.RefreshToken, acc.GrokClientID, acc.GrokTokenEndpoint, acc.GrokOIDCIssuer
		principalType, principalID, generation = acc.GrokPrincipalType, acc.GrokPrincipalID, acc.CredentialGeneration
		acc.mu.RUnlock()
		if authKindIsAPIKey {
			return nil
		}
	}
	if strings.TrimSpace(rt) == "" {
		return fmt.Errorf("grok refresh_token 为空")
	}

	// Legacy account-id lock is retained only when a stable family cannot be
	// established (for example a transient test account without a DB row).
	if refreshLocks == nil && s.tokenCache != nil {
		acquired, lockErr := s.tokenCache.AcquireRefreshLock(ctx, dbID, 30*time.Second)
		if lockErr != nil {
			if s.tokenCache.SharedAcrossInstances() {
				return fmt.Errorf("获取 Grok 跨实例刷新锁失败: %w", lockErr)
			}
			log.Printf("[账号 %d] 获取 grok 本地刷新锁失败: %v", dbID, lockErr)
		}
		if !acquired && lockErr == nil {
			token, waitErr := s.tokenCache.WaitForRefreshComplete(ctx, dbID, 30*time.Second)
			if !forceRefresh && waitErr == nil && token != "" {
				acc.mu.Lock()
				acc.AccessToken = token
				if planType := GrokPlanTypeFromAccessToken(token); planType != "" {
					acc.PlanType = planType
				}
				if expiresAt := grokAccessTokenExpiry(token); !expiresAt.IsZero() {
					acc.ExpiresAt = expiresAt
				} else {
					acc.ExpiresAt = time.Now().Add(30 * time.Minute)
				}
				// 以当前状态判定冷却,不用入口快照:等待期间可能新设了冷却。
				if !(acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)) {
					acc.Status = StatusReady
				}
				acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
				acc.mu.Unlock()
				s.invalidateRoutingSchedulers()
				s.fastSchedulerUpdate(acc)
				return nil
			}
			if !forceRefresh {
				return fmt.Errorf("账号 %d 正在刷新，请稍后重试", dbID)
			}
		}
		if acquired {
			defer s.tokenCache.ReleaseRefreshLock(ctx, dbID)
		}
	}

	// 从这里起进入 RT 消费临界区:上游一旦完成 RT 轮换,调用方取消(浏览器断开、
	// 管理端短超时)会让新 RT 在"响应未读完/CAS 未提交"窗口内永久丢失,账号被
	// 错误打成 error 且只能人工重导。切换到不随调用方取消、仅受 lease hold
	// 期限约束的 ctx,保证交换与落库原子完成。
	if refreshLocks != nil {
		ctx = refreshLocks.CriticalContext()
	}
	proxyURL := s.ResolveProxyForAccount(acc)
	if strings.TrimSpace(proxyURL) == "" && s.GetProxyPoolEnabled() {
		return fmt.Errorf("账号 %d 代理池已启用但无可用代理，已拒绝直连刷新", dbID)
	}
	td, err := RefreshGrokAccessToken(ctx, GrokRefreshParams{
		RefreshToken:  rt,
		ClientID:      clientID,
		TokenEndpoint: tokenEndpoint,
		OIDCIssuer:    oidcIssuer,
		PrincipalType: principalType,
		PrincipalID:   principalID,
		ProxyURL:      proxyURL,
	})
	if err != nil {
		if IsGrokRefreshPermanentError(err) {
			// A concurrent rotation can make an old RT return invalid_grant. Only
			// mark the account invalid if generation is still the one requested.
			if s.db != nil {
				if current, _, readErr := s.db.GetAccountCredentialState(ctx, dbID); readErr == nil && current != generation {
					_, _, _ = s.reloadGrokCredentialsAfterFamilyLease(ctx, acc, generation)
					return nil
				}
			}
			acc.mu.Lock()
			acc.Status = StatusError
			acc.ErrorMsg = err.Error()
			acc.mu.Unlock()
			s.fastSchedulerUpdate(acc)
			if s.db != nil {
				_ = s.db.SetError(ctx, dbID, err.Error())
			}
		}
		return err
	}

	newGeneration := generation
	if s.db != nil {
		credentials := grokRefreshedCredentialUpdates(td)
		var applied bool
		var casErr error
		newGeneration, applied, casErr = s.db.UpdateAccountCredentialsCAS(ctx, dbID, generation, credentials)
		if casErr != nil {
			return fmt.Errorf("grok 刷新凭据 CAS 写库失败: %w", casErr)
		}
		if !applied {
			_, _, reloadErr := s.reloadGrokCredentialsAfterFamilyLease(ctx, acc, generation)
			if reloadErr != nil {
				return reloadErr
			}
			return nil
		}
		_ = s.db.ClearError(ctx, dbID)
	} else {
		// Transient accounts used by connectivity tests have no durable identity
		// row; keep their generation stable while preserving DB-first semantics
		// for every persisted account.
		newGeneration = generation
	}

	// Publish only after the CAS commit. This prevents an unpersisted rotated RT
	// from becoming the sole live copy when a database write fails.
	acc.mu.Lock()
	acc.invalidateGrokPersistentStateLocked(newGeneration)
	acc.AccessToken = td.AccessToken
	if td.PlanType != "" {
		acc.PlanType = td.PlanType
	}
	if td.RefreshToken != "" {
		acc.RefreshToken = td.RefreshToken
	}
	acc.ExpiresAt = td.ExpiresAt
	acc.ErrorMsg = ""
	// 冷却判定基于当前内存状态:入口快照到这里可能隔了近 10 分钟(等 lease +
	// HTTP 刷新),期间设置/延长的冷却是更新的事实,AT 续期成功不代表限流解除。
	if !(acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)) {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	if s.db != nil {
		// CAS 已把上一代目录/能力/事实随代盖章,重新投影回内存,避免刷新后
		// 路由退化为保守默认集并等 30 秒扫描兜底。失败不致命。
		if reloadErr := s.ReloadGrokPersistentState(ctx, dbID); reloadErr != nil {
			log.Printf("[账号 %d] 刷新后重载 Grok 持久状态失败: %v", dbID, reloadErr)
		}
	}
	if s.tokenCache != nil {
		if ttl := time.Until(td.ExpiresAt) - 5*time.Minute; ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, dbID, td.AccessToken, ttl)
		}
	}
	return nil
}

// grokRefreshedCredentialUpdates keeps the legacy runtime plan_type projection
// in sync with its real source. A refreshed access-token tier is an unverified
// JWT display hint, never an archive label or an authorization fact. The
// archive_plan_type field is intentionally absent so token rotation cannot
// overwrite import-file metadata.
func grokRefreshedCredentialUpdates(td *GrokTokenData) map[string]interface{} {
	credentials := map[string]interface{}{
		"access_token":     td.AccessToken,
		"expires_at":       td.ExpiresAt.Format(time.RFC3339),
		"jwt_plan_type":    td.PlanType,
		"jwt_plan_trusted": false,
	}
	if td.PlanType != "" {
		credentials["plan_type"] = td.PlanType
	}
	if td.RefreshToken != "" {
		credentials["refresh_token"] = td.RefreshToken
	}
	if td.IDToken != "" {
		credentials["id_token"] = td.IDToken
	}
	return credentials
}

func (s *Store) reloadGrokCredentialsAfterFamilyLease(ctx context.Context, acc *Account, expectedGeneration int64) (changed, usable bool, err error) {
	if s == nil || s.db == nil || acc == nil || acc.DBID <= 0 {
		return false, false, nil
	}
	row, err := s.db.GetAccountByID(ctx, acc.DBID)
	if err != nil {
		return false, false, err
	}
	// Compare the complete persisted identity even when generation is unchanged:
	// a legacy binary can rotate AT/RT through UpdateCredentials without knowing
	// about credential_generation. The rolling-upgrade bridge lock guarantees it
	// is no longer writing while this snapshot is applied.
	acc.mu.Lock()
	previousGeneration := acc.CredentialGeneration
	previousFamilyID := acc.CredentialFamilyID
	previousRefreshToken := acc.RefreshToken
	previousAccessToken := acc.AccessToken
	previousAPIKey := acc.APIKey
	previousUpstreamType := acc.UpstreamType
	previousBaseURL := acc.BaseURL
	previousClientID := acc.GrokClientID
	previousTokenEndpoint := acc.GrokTokenEndpoint
	previousIssuer := acc.GrokOIDCIssuer
	previousPrincipalType := acc.GrokPrincipalType
	previousPrincipalID := acc.GrokPrincipalID
	previousAccountID := acc.AccountID
	previousEmail := acc.Email
	previousPlanType := acc.PlanType
	previousExpiresAt := acc.ExpiresAt
	if row.CredentialGeneration != acc.CredentialGeneration {
		acc.invalidateGrokPersistentStateLocked(row.CredentialGeneration)
	}
	acc.CredentialFamilyID = row.CredentialFamilyID
	acc.RefreshToken = strings.TrimSpace(row.GetCredential("refresh_token"))
	acc.AccessToken = strings.TrimSpace(row.GetCredential("access_token"))
	acc.APIKey = strings.TrimSpace(row.GetCredential("api_key"))
	acc.UpstreamType = strings.TrimSpace(row.GetCredential("upstream_type"))
	acc.BaseURL = strings.TrimRight(strings.TrimSpace(row.GetCredential("base_url")), "/")
	acc.GrokClientID = row.GetCredential("grok_client_id")
	acc.GrokTokenEndpoint = row.GetCredential("grok_token_endpoint")
	acc.GrokOIDCIssuer = row.GetCredential("grok_oidc_issuer")
	acc.GrokPrincipalType = row.GetCredential("grok_principal_type")
	acc.GrokPrincipalID = row.GetCredential("grok_principal_id")
	acc.AccountID = row.GetCredential("account_id")
	acc.Email = row.GetCredential("email")
	if acc.APIKey != "" {
		acc.PlanType = "api"
	} else if plan := row.GetCredential("plan_type"); plan != "" {
		acc.PlanType = plan
	} else {
		acc.PlanType = GrokPlanTypeFromAccessToken(acc.AccessToken)
	}
	acc.ExpiresAt = parseOAuthCredentialExpiry(row.GetCredential("expires_at"))
	changed = previousGeneration != acc.CredentialGeneration ||
		previousFamilyID != acc.CredentialFamilyID ||
		previousRefreshToken != acc.RefreshToken ||
		previousAccessToken != acc.AccessToken ||
		previousAPIKey != acc.APIKey ||
		previousUpstreamType != acc.UpstreamType ||
		strings.TrimRight(strings.TrimSpace(previousBaseURL), "/") != acc.BaseURL ||
		previousClientID != acc.GrokClientID ||
		previousTokenEndpoint != acc.GrokTokenEndpoint ||
		previousIssuer != acc.GrokOIDCIssuer ||
		previousPrincipalType != acc.GrokPrincipalType ||
		previousPrincipalID != acc.GrokPrincipalID ||
		previousAccountID != acc.AccountID ||
		previousEmail != acc.Email ||
		previousPlanType != acc.PlanType ||
		!previousExpiresAt.Equal(acc.ExpiresAt)
	usable = acc.AccessToken != "" && (acc.ExpiresAt.IsZero() || time.Until(acc.ExpiresAt) > 5*time.Minute)
	acc.mu.Unlock()
	if changed {
		s.invalidateRoutingSchedulers()
	}
	_ = expectedGeneration // retained for call-site compatibility and audit logs.
	return changed, usable, nil
}

func grokAccessTokenExpiry(token string) time.Time {
	if claims := grokJWTClaims(token); claims != nil {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			return time.Unix(int64(exp), 0)
		}
	}
	return time.Time{}
}

// ApplyGrokConfig 热更新运行时 Grok 账号的可编辑配置（凭据留空 = 不改）。
func (s *Store) ApplyGrokConfig(dbID int64, baseURL, apiKey string, models []string, modelMapping, proxyURL string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	// UpdateGrokAccount persists credentials before calling this method. Reload
	// the authoritative identity fence so an in-process account cannot keep
	// routing with observations from the previous credential generation. This
	// also covers identity changes other than api_key (for example base_url),
	// which the old in-memory-only comparison missed.
	type persistedGrokIdentity struct {
		loaded                                                         bool
		generation                                                     int64
		familyID, upstreamType, baseURL, apiKey, accessToken           string
		refreshToken, clientID, tokenEndpoint, issuer                  string
		principalType, principalID, accountID, email, planType, expiry string
	}
	persisted := persistedGrokIdentity{}
	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if row, err := s.db.GetAccountByID(ctx, dbID); err == nil {
			persisted = persistedGrokIdentity{
				loaded: true, generation: row.CredentialGeneration, familyID: row.CredentialFamilyID,
				upstreamType: row.GetCredential("upstream_type"), baseURL: row.GetCredential("base_url"),
				apiKey: row.GetCredential("api_key"), accessToken: row.GetCredential("access_token"),
				refreshToken: row.GetCredential("refresh_token"), clientID: row.GetCredential("grok_client_id"),
				tokenEndpoint: row.GetCredential("grok_token_endpoint"), issuer: row.GetCredential("grok_oidc_issuer"),
				principalType: row.GetCredential("grok_principal_type"), principalID: row.GetCredential("grok_principal_id"),
				accountID: row.GetCredential("account_id"), email: row.GetCredential("email"),
				planType: row.GetCredential("plan_type"), expiry: row.GetCredential("expires_at"),
			}
		}
		cancel()
	}
	normalizedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if persisted.loaded {
		normalizedBaseURL = strings.TrimRight(strings.TrimSpace(persisted.baseURL), "/")
	}
	acc.mu.Lock()
	identityChanged := normalizedBaseURL != strings.TrimRight(strings.TrimSpace(acc.BaseURL), "/") ||
		(strings.TrimSpace(apiKey) != "" && strings.TrimSpace(apiKey) != strings.TrimSpace(acc.APIKey))
	if persisted.generation > 0 && persisted.generation != acc.CredentialGeneration {
		acc.invalidateGrokPersistentStateLocked(persisted.generation)
		identityChanged = false // invalidation was already performed above.
	}
	if persisted.loaded {
		acc.CredentialFamilyID = persisted.familyID
		acc.UpstreamType = persisted.upstreamType
		acc.APIKey = strings.TrimSpace(persisted.apiKey)
		acc.AccessToken = strings.TrimSpace(persisted.accessToken)
		acc.RefreshToken = strings.TrimSpace(persisted.refreshToken)
		acc.GrokClientID = persisted.clientID
		acc.GrokTokenEndpoint = persisted.tokenEndpoint
		acc.GrokOIDCIssuer = persisted.issuer
		acc.GrokPrincipalType = persisted.principalType
		acc.GrokPrincipalID = persisted.principalID
		acc.AccountID = persisted.accountID
		acc.Email = persisted.email
		if persisted.planType != "" {
			acc.PlanType = persisted.planType
		}
		acc.ExpiresAt = parseOAuthCredentialExpiry(persisted.expiry)
	} else {
		acc.UpstreamType = UpstreamGrok
		if strings.TrimSpace(apiKey) != "" {
			acc.APIKey = strings.TrimSpace(apiKey)
		}
	}
	acc.BaseURL = normalizedBaseURL
	if identityChanged {
		// The database updater is responsible for the authoritative generation;
		// discard any runtime observations immediately so old facts cannot route
		// the newly configured key before the account is reloaded.
		acc.GrokLivePlanKnown = false
		acc.GrokAccessAllowed = nil
		acc.GrokBillingExhausted = false
		acc.GrokFactsGeneration = 0
		acc.grokRouting = nil
	}
	acc.Models = normalizeModelList(models)
	acc.ModelMapping = strings.TrimSpace(modelMapping)
	acc.ProxyURL = strings.TrimSpace(proxyURL)
	if acc.Status != StatusError {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	return true
}
