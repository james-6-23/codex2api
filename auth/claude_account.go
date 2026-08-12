package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// UpstreamClaude 标记 Claude Code 上游账号（upstream_type 凭据字段取值）。
//
// 凭据形态只有一种：Anthropic OAuth（Authorization Code + PKCE）产出的
// access_token + refresh_token。上游是官方 https://api.anthropic.com/v1/messages，
// 请求必须携带 anthropic-beta: oauth-2025-04-20 且 system 首块为 Claude Code 身份串，
// 否则上游返回 401/403。裸 sk-ant- API Key 不走这条链路（计费主体不同，调度语义也不同）。
const UpstreamClaude = "claude"

// Claude Code OAuth 常量，与官方 CLI 对齐：
//   - client_id 为 Anthropic 公开的 Claude Code 客户端（PKCE 公共客户端，无 secret）；
//   - redirect_uri 固定为 Anthropic 托管的回调页，用户把回调页显示的 code 粘回管理台；
//   - authorize 必须带 code=true，上游才会把 code 渲染成可复制文本而不是静默重定向。
const (
	ClaudeDefaultAPIBaseURL    = "https://api.anthropic.com"
	ClaudeDefaultAuthorizeURL  = "https://claude.ai/oauth/authorize"
	ClaudeDefaultTokenURL      = "https://api.anthropic.com/v1/oauth/token"
	ClaudeDefaultBootstrapURL  = "https://api.anthropic.com/api/claude_cli/bootstrap"
	ClaudeDefaultOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClaudeDefaultRedirectURI   = "https://platform.claude.com/oauth/code/callback"
	ClaudeDefaultOAuthScope    = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers"

	// ClaudeOAuthBetaHeader 是 OAuth 凭据访问 /v1/messages 的准入 beta 标记，缺失即 401。
	ClaudeOAuthBetaHeader = "oauth-2025-04-20"
	// ClaudeAPIVersion 是 anthropic-version 请求头取值。
	ClaudeAPIVersion = "2023-06-01"
	// ClaudeDefaultCLIVersion 参与 User-Agent 伪装，随上游 CLI 升级可用环境变量覆盖。
	ClaudeDefaultCLIVersion = "2.1.219"

	// EnvClaudeOAuthClientID / EnvClaudeCLIVersion 沿用 Grok 的部署级覆盖约定：
	// 环境变量压在系统设置之上，数据库里的值被误改时仍能从部署侧兜住。
	EnvClaudeOAuthClientID = "CLAUDE_OAUTH_CLIENT_ID"
	EnvClaudeCLIVersion    = "CLAUDE_CLI_VERSION"
)

// ClaudeOAuthClientIDMaxLen 是 client_id 的长度上限（官方 id 为 36 字符 UUID）。
const ClaudeOAuthClientIDMaxLen = 128

// configuredClaudeOAuthClientID 是系统设置里配的 client_id，随设置热更新。
var configuredClaudeOAuthClientID atomic.Value // string

// SetConfiguredClaudeOAuthClientID 热更新系统设置里的 client_id（空 = 回落到内置默认）。
func SetConfiguredClaudeOAuthClientID(clientID string) {
	configuredClaudeOAuthClientID.Store(NormalizeClaudeOAuthClientID(clientID))
}

// ConfiguredClaudeOAuthClientID 返回系统设置里配的 client_id，未配置时为空。
func ConfiguredClaudeOAuthClientID() string {
	v, _ := configuredClaudeOAuthClientID.Load().(string)
	return v
}

// ClaudeOAuthClientIDFromEnv 返回环境变量里配的 client_id（去空格后为空表示未设）。
func ClaudeOAuthClientIDFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvClaudeOAuthClientID))
}

// EffectiveClaudeOAuthClientID 返回生效的 client_id：
// 环境变量 > 系统设置 > 内置的官方 Claude Code 公开 id。
func EffectiveClaudeOAuthClientID() string {
	if v := ClaudeOAuthClientIDFromEnv(); v != "" {
		return v
	}
	if v := ConfiguredClaudeOAuthClientID(); v != "" {
		return v
	}
	return ClaudeDefaultOAuthClientID
}

// NormalizeClaudeOAuthClientID 归一化 client_id：去首尾空白，含空白/控制字符或超长的
// 一律视为未配置（返回空 = 回落到上一级）。该值会进授权 URL 与 token 请求体。
func NormalizeClaudeOAuthClientID(clientID string) string {
	v := strings.TrimSpace(clientID)
	if v == "" || len(v) > ClaudeOAuthClientIDMaxLen {
		return ""
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ""
		}
	}
	return v
}

// EffectiveClaudeCLIVersion 返回参与 User-Agent 的 CLI 版本号。
func EffectiveClaudeCLIVersion() string {
	if v := strings.TrimSpace(os.Getenv(EnvClaudeCLIVersion)); v != "" {
		return v
	}
	return ClaudeDefaultCLIVersion
}

// ClaudeUserAgent 返回与官方 CLI 对齐的 User-Agent。
func ClaudeUserAgent() string {
	return fmt.Sprintf("claude-cli/%s (external, cli)", EffectiveClaudeCLIVersion())
}

// ClaudePlanFromRateLimitTier 把 bootstrap 返回的 organization_rate_limit_tier
// 映射为套餐键。上游档位串形如 default_claude_max_20x / default_claude_pro。
// 无法识别时返回空，由调用方决定是否留白。
func ClaudePlanFromRateLimitTier(tier string) string {
	v := strings.ToLower(strings.TrimSpace(tier))
	if v == "" {
		return ""
	}
	switch {
	case strings.Contains(v, "max_20x"), strings.Contains(v, "max20"):
		return "max20"
	case strings.Contains(v, "max_5x"), strings.Contains(v, "max5"):
		return "max5"
	case strings.Contains(v, "team"):
		return "team"
	case strings.Contains(v, "enterprise"):
		return "enterprise"
	case strings.Contains(v, "pro"):
		return "pro"
	case strings.Contains(v, "free"):
		return "free"
	default:
		return ""
	}
}

// ==================== 账号判定与凭据 ====================

func (a *Account) isClaudeAPILocked() bool {
	if a == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamClaude) {
		return false
	}
	return strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != ""
}

// IsClaudeAPI 判断账号是否为 Claude Code 上游账号。
func (a *Account) IsClaudeAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isClaudeAPILocked()
}

// ClaudeCredentials 返回请求 Claude 上游所需的 base_url 与 Bearer。
// base_url 留空时回落到官方 API 域名。
func (a *Account) ClaudeCredentials() (baseURL, bearer string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	baseURL = strings.TrimSpace(a.BaseURL)
	if baseURL == "" {
		baseURL = ClaudeDefaultAPIBaseURL
	}
	return baseURL, strings.TrimSpace(a.AccessToken)
}

// ClaudeModels 返回账号声明的模型白名单副本（空 = 不限制）。
func (a *Account) ClaudeModels() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.Models) == 0 {
		return nil
	}
	out := make([]string, len(a.Models))
	copy(out, a.Models)
	return out
}

// ClaudeChannelSupportsModel 判断账号是否可承接该模型。未声明白名单时全部放行。
func (a *Account) ClaudeChannelSupportsModel(model string) bool {
	if a == nil {
		return false
	}
	model = strings.TrimSpace(model)
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.Models) == 0 || model == "" {
		return true
	}
	for _, m := range a.Models {
		if strings.EqualFold(strings.TrimSpace(m), model) {
			return true
		}
	}
	return false
}

// GetClaudeCLIUserID 返回该账号绑定的 CLI 用户标识。上游按它识别「同一台机器」，
// 必须在建号时生成一次并跨刷新保持不变——每次请求换值会被判定为异常客户端。
func (a *Account) GetClaudeCLIUserID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.ClaudeCLIUserID)
}

// GetClaudeAccountUUID 返回 bootstrap 探针拿到的 Anthropic 账号 UUID（可能为空）。
func (a *Account) GetClaudeAccountUUID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.ClaudeAccountUUID)
}

// GetClaudeOrganizationUUID 返回账号所属组织 UUID（bootstrap 探针写入，可能为空）。
func (a *Account) GetClaudeOrganizationUUID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.ClaudeOrganizationUUID)
}

// NormalizeClaudeBaseURL 校验并归一化 Claude 上游 base_url。
// 留空表示使用官方默认；只接受 http/https 且不带 query/fragment 的绝对地址。
func NormalizeClaudeBaseURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("base_url 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url 必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url 缺少主机名")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("base_url 不能包含查询参数或锚点")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// ==================== PKCE 与授权 URL ====================

// ClaudePKCE 是一次授权会话的 PKCE 材料。
type ClaudePKCE struct {
	Verifier  string
	Challenge string
	State     string
}

func claudeRandomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GenerateClaudePKCE 生成 code_verifier / code_challenge(S256) / state。
func GenerateClaudePKCE() (*ClaudePKCE, error) {
	verifier, err := claudeRandomURLSafe(32)
	if err != nil {
		return nil, fmt.Errorf("生成 code_verifier 失败: %w", err)
	}
	state, err := claudeRandomURLSafe(24)
	if err != nil {
		return nil, fmt.Errorf("生成 state 失败: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	return &ClaudePKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		State:     state,
	}, nil
}

// NewClaudeCLIUserID 生成 CLI 用户标识（32 字节十六进制，与官方 CLI 形态一致）。
func NewClaudeCLIUserID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// ClaudeAuthURLParams 是构造授权 URL 的入参。
type ClaudeAuthURLParams struct {
	ClientID     string
	AuthorizeURL string
	RedirectURI  string
	Scope        string
	Challenge    string
	State        string
}

// BuildClaudeAuthorizationURL 构造 Claude Code 的 PKCE 授权 URL。
//
// prompt=login 是多账号池的关键：Anthropic 所有账号共用同一个 client_id，不强制重新
// 认证时第二次授权会沿用浏览器里的既有会话，上游按「会话接管」作废前一个账号的
// refresh_token 家族——池子里的老号会集体掉线。
func BuildClaudeAuthorizationURL(params ClaudeAuthURLParams) (string, error) {
	clientID := NormalizeClaudeOAuthClientID(params.ClientID)
	if clientID == "" {
		clientID = EffectiveClaudeOAuthClientID()
	}
	if strings.TrimSpace(params.Challenge) == "" {
		return "", fmt.Errorf("code_challenge 为空")
	}
	authorizeURL := strings.TrimSpace(params.AuthorizeURL)
	if authorizeURL == "" {
		authorizeURL = ClaudeDefaultAuthorizeURL
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return "", fmt.Errorf("authorize_url 解析失败: %w", err)
	}
	redirectURI := strings.TrimSpace(params.RedirectURI)
	if redirectURI == "" {
		redirectURI = ClaudeDefaultRedirectURI
	}
	scope := strings.TrimSpace(params.Scope)
	if scope == "" {
		scope = ClaudeDefaultOAuthScope
	}

	q := url.Values{
		"code":                  {"true"},
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {scope},
		"code_challenge":        {params.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {strings.TrimSpace(params.State)},
		"prompt":                {"login"},
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ClaudeAuthorizationInput 是从用户粘贴内容里解析出的 code / state。
type ClaudeAuthorizationInput struct {
	Code  string
	State string
}

// ParseClaudeAuthorizationInput 兼容三种粘贴形态：
//   - 裸 code；
//   - 回调页给出的 "code#state" 复合串；
//   - 完整回调 URL（含 ?code=&state=）。
func ParseClaudeAuthorizationInput(raw string) ClaudeAuthorizationInput {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ClaudeAuthorizationInput{}
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if u, err := url.Parse(v); err == nil {
			q := u.Query()
			rawCode := strings.TrimSpace(q.Get("code"))
			state := strings.TrimSpace(q.Get("state"))
			if rawCode == "" && u.Fragment != "" {
				if fq, ferr := url.ParseQuery(u.Fragment); ferr == nil {
					rawCode = strings.TrimSpace(fq.Get("code"))
					if state == "" {
						state = strings.TrimSpace(fq.Get("state"))
					}
				}
			}
			// 回调 URL 里的 code 本身也可能是 code#state 复合串。
			if code, embedded := splitClaudeCodeState(rawCode); code != "" {
				return ClaudeAuthorizationInput{Code: code, State: firstNonEmptyTrimmed(embedded, state)}
			}
		}
	}
	code, state := splitClaudeCodeState(v)
	return ClaudeAuthorizationInput{Code: code, State: state}
}

func splitClaudeCodeState(raw string) (code, state string) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", ""
	}
	if idx := strings.Index(v, "#"); idx >= 0 {
		return strings.TrimSpace(v[:idx]), strings.TrimSpace(v[idx+1:])
	}
	return v, ""
}

// ==================== token 交换与刷新 ====================

// ClaudeTokenData 是一次 token 交换/刷新的产出。
type ClaudeTokenData struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	ExpiresAt    time.Time
}

// ClaudeBootstrap 是 claude_cli/bootstrap 探针返回的账号身份。
type ClaudeBootstrap struct {
	AccountUUID               string
	AccountEmail              string
	OrganizationUUID          string
	OrganizationName          string
	OrganizationType          string
	OrganizationRateLimitTier string
}

// ClaudeExchangeParams 是 authorization_code 兑换入参。
type ClaudeExchangeParams struct {
	Code        string
	State       string
	Verifier    string
	ClientID    string
	TokenURL    string
	RedirectURI string
	ProxyURL    string
}

// ClaudeRefreshParams 是 refresh_token 刷新入参。
type ClaudeRefreshParams struct {
	RefreshToken string
	ClientID     string
	TokenURL     string
	ProxyURL     string
}

// claudeRefreshPermanentError 标记不可重试的刷新失败（invalid_grant / invalid_client），
// 账号应转入 error 状态而非退避重试。
type claudeRefreshPermanentError struct{ code string }

func (e *claudeRefreshPermanentError) Error() string {
	return "claude OAuth 刷新永久失败: " + e.code
}

// IsClaudeRefreshPermanentError 判断刷新错误是否为永久失败（RT 已失效）。
func IsClaudeRefreshPermanentError(err error) bool {
	var permanent *claudeRefreshPermanentError
	return errors.As(err, &permanent)
}

func claudeHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := ConfigureTransportProxy(transport, proxyURL, nil); err != nil {
		return nil, fmt.Errorf("claude 代理配置失败: %w", err)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// claudeAllowedTokenEndpoint 限制 token 端点只能落在 Anthropic 官方域，
// 避免刷新凭据被误配的 token_url 带到第三方主机。
func claudeAllowedTokenEndpoint(u *url.URL) bool {
	if u == nil || u.Scheme != "https" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.anthropic.com", "console.anthropic.com", "claude.ai":
		return true
	default:
		return false
	}
}

func claudeResolveTokenURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		v = ClaudeDefaultTokenURL
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("claude token_url 解析失败: %w", err)
	}
	if !claudeAllowedTokenEndpoint(u) {
		return "", fmt.Errorf("claude token_url 不在允许的 Anthropic 域内: %s", u.Host)
	}
	return u.String(), nil
}

type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func claudePostToken(ctx context.Context, endpoint string, payload map[string]string, client *http.Client) (*ClaudeTokenData, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ClaudeUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("claude token 响应读取失败: %w", err)
	}

	var parsed claudeTokenResponse
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := strings.ToLower(strings.TrimSpace(parsed.Error))
		if code == "invalid_grant" || code == "invalid_client" {
			return nil, &claudeRefreshPermanentError{code: code}
		}
		if code == "" {
			code = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		if detail := strings.TrimSpace(parsed.ErrorDesc); detail != "" {
			return nil, fmt.Errorf("claude token 失败: %s (%s, status=%d)", code, detail, resp.StatusCode)
		}
		return nil, fmt.Errorf("claude token 失败: %s (status=%d)", code, resp.StatusCode)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return nil, fmt.Errorf("claude token 响应缺少 access_token")
	}

	expiresIn := parsed.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &ClaudeTokenData{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		Scope:        parsed.Scope,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

// ExchangeClaudeAuthorizationCode 用授权码兑换 Claude OAuth token。
func ExchangeClaudeAuthorizationCode(ctx context.Context, params ClaudeExchangeParams) (*ClaudeTokenData, error) {
	code, embeddedState := splitClaudeCodeState(params.Code)
	if code == "" {
		return nil, fmt.Errorf("claude 授权码为空")
	}
	if strings.TrimSpace(params.Verifier) == "" {
		return nil, fmt.Errorf("claude code_verifier 为空")
	}
	clientID := NormalizeClaudeOAuthClientID(params.ClientID)
	if clientID == "" {
		clientID = EffectiveClaudeOAuthClientID()
	}
	redirectURI := strings.TrimSpace(params.RedirectURI)
	if redirectURI == "" {
		redirectURI = ClaudeDefaultRedirectURI
	}
	endpoint, err := claudeResolveTokenURL(params.TokenURL)
	if err != nil {
		return nil, err
	}
	client, err := claudeHTTPClient(params.ProxyURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return claudePostToken(ctx, endpoint, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         firstNonEmptyTrimmed(embeddedState, params.State),
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": strings.TrimSpace(params.Verifier),
	}, client)
}

// RefreshClaudeAccessToken 用 refresh_token 交换新的 Claude access_token。
func RefreshClaudeAccessToken(ctx context.Context, params ClaudeRefreshParams) (*ClaudeTokenData, error) {
	rt := strings.TrimSpace(params.RefreshToken)
	if rt == "" {
		return nil, fmt.Errorf("claude refresh_token 为空")
	}
	clientID := NormalizeClaudeOAuthClientID(params.ClientID)
	if clientID == "" {
		clientID = EffectiveClaudeOAuthClientID()
	}
	endpoint, err := claudeResolveTokenURL(params.TokenURL)
	if err != nil {
		return nil, err
	}
	client, err := claudeHTTPClient(params.ProxyURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return claudePostToken(ctx, endpoint, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"client_id":     clientID,
	}, client)
}

// FetchClaudeBootstrap 探测账号身份（邮箱 / 组织 / 限额档位）。
// 失败不应阻断建号：access_token 本身已经可用，身份信息只是展示与调度提示。
func FetchClaudeBootstrap(ctx context.Context, accessToken, proxyURL string) (*ClaudeBootstrap, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil, fmt.Errorf("access_token 为空")
	}
	client, err := claudeHTTPClient(proxyURL, 10*time.Second)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ClaudeDefaultBootstrapURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ClaudeUserAgent())
	req.Header.Set("anthropic-beta", ClaudeOAuthBetaHeader)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude bootstrap 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("claude bootstrap 响应读取失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("claude bootstrap 失败 (status=%d)", resp.StatusCode)
	}

	var payload struct {
		OAuthAccount struct {
			AccountUUID               string `json:"account_uuid"`
			AccountEmail              string `json:"account_email"`
			OrganizationUUID          string `json:"organization_uuid"`
			OrganizationName          string `json:"organization_name"`
			OrganizationType          string `json:"organization_type"`
			OrganizationRateLimitTier string `json:"organization_rate_limit_tier"`
		} `json:"oauth_account"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("claude bootstrap 响应解析失败: %w", err)
	}
	acct := payload.OAuthAccount
	if strings.TrimSpace(acct.AccountUUID) == "" && strings.TrimSpace(acct.AccountEmail) == "" {
		return nil, fmt.Errorf("claude bootstrap 响应缺少 oauth_account")
	}
	return &ClaudeBootstrap{
		AccountUUID:               strings.TrimSpace(acct.AccountUUID),
		AccountEmail:              strings.TrimSpace(acct.AccountEmail),
		OrganizationUUID:          strings.TrimSpace(acct.OrganizationUUID),
		OrganizationName:          strings.TrimSpace(acct.OrganizationName),
		OrganizationType:          strings.TrimSpace(acct.OrganizationType),
		OrganizationRateLimitTier: strings.TrimSpace(acct.OrganizationRateLimitTier),
	}, nil
}

// ==================== Store 刷新链路 ====================

// refreshClaudeAccount 刷新 Claude OAuth 账号的 AT。
// 与 Codex / Grok 共用 tokenCache 的跨进程刷新锁：Anthropic 的 RT 会轮换，
// 多副本重复消费同一个 RT 会直接踢出 invalid_grant。
func (s *Store) refreshClaudeAccount(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
	rt := acc.RefreshToken
	dbID := acc.DBID
	clientID := acc.ClaudeClientID
	tokenURL := acc.ClaudeTokenURL
	cooldownUntil := acc.CooldownUtil
	cooldownReason := acc.CooldownReason
	activeCooldown := acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)
	acc.mu.RUnlock()

	if strings.TrimSpace(rt) == "" {
		return fmt.Errorf("claude refresh_token 为空")
	}

	if s.tokenCache != nil {
		acquired, lockErr := s.tokenCache.AcquireRefreshLock(ctx, dbID, 30*time.Second)
		if lockErr != nil {
			log.Printf("[账号 %d] 获取 claude 刷新锁失败: %v", dbID, lockErr)
		}
		if !acquired && lockErr == nil {
			token, waitErr := s.tokenCache.WaitForRefreshComplete(ctx, dbID, 30*time.Second)
			if !forceRefresh && waitErr == nil && token != "" {
				acc.mu.Lock()
				acc.AccessToken = token
				// 等锁期间拿到的是别的副本刷出来的 AT，本地没有它的 expires_in，
				// 保守按 30 分钟兜底，到期再走一次正常刷新。
				acc.ExpiresAt = time.Now().Add(30 * time.Minute)
				if !activeCooldown {
					acc.Status = StatusReady
					acc.CooldownUtil = time.Time{}
					acc.CooldownReason = ""
				}
				acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
				acc.mu.Unlock()
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

	td, err := RefreshClaudeAccessToken(ctx, ClaudeRefreshParams{
		RefreshToken: rt,
		ClientID:     clientID,
		TokenURL:     tokenURL,
		ProxyURL:     s.ResolveProxyForAccount(acc),
	})
	if err != nil {
		if IsClaudeRefreshPermanentError(err) {
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

	acc.mu.Lock()
	acc.AccessToken = td.AccessToken
	if td.RefreshToken != "" {
		acc.RefreshToken = td.RefreshToken
	}
	acc.ExpiresAt = td.ExpiresAt
	acc.ErrorMsg = ""
	if activeCooldown {
		acc.Status = StatusCooldown
		acc.CooldownUtil = cooldownUntil
		acc.CooldownReason = cooldownReason
	} else {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	if s.tokenCache != nil {
		if ttl := time.Until(td.ExpiresAt) - 5*time.Minute; ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, dbID, td.AccessToken, ttl)
		}
	}

	if s.db != nil {
		credentials := map[string]interface{}{
			"access_token": td.AccessToken,
			"expires_at":   td.ExpiresAt.Format(time.RFC3339),
		}
		if td.RefreshToken != "" {
			credentials["refresh_token"] = td.RefreshToken
		}
		if td.Scope != "" {
			credentials["scope"] = td.Scope
		}
		if err := s.db.UpdateCredentials(ctx, dbID, credentials); err != nil {
			log.Printf("[账号 %d] claude 刷新后写库失败: %v", dbID, err)
		} else {
			_ = s.db.ClearError(ctx, dbID)
		}
	}
	return nil
}

// ApplyClaudeConfig 热更新运行时 Claude 账号的可编辑配置。
func (s *Store) ApplyClaudeConfig(dbID int64, baseURL string, models []string, modelMapping, proxyURL string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.BaseURL = strings.TrimSpace(baseURL)
	acc.Models = NormalizeAccountModels(models)
	acc.ModelMapping = strings.TrimSpace(modelMapping)
	acc.ProxyURL = strings.TrimSpace(proxyURL)
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}
