package auth

// Claude Code(Anthropic)OAuth 登录模块。
//
// 本文件把 Claude Code 官方客户端的 OAuth2 + PKCE 登录流程移植进账号池，使得
// 平台可以像管理 Codex / Grok / Antigravity 账号一样，纳管多个 Claude Pro/Max
// 订阅账号并统一调度。参数对齐 Claude Code 官方客户端（client_id / 端点 / scope /
// 强制 beta 头），逆向常量参考 CLIProxyAPI(router-for-me/CLIProxyAPI)。
//
// 使用方式（服务器无本地回调场景，采用手动粘贴授权码）：
//  1. StartClaudeLogin() 生成 AuthURL + State + Verifier，把 AuthURL 交给用户在
//     浏览器打开授权；State/Verifier 由调用方短期缓存。
//  2. 用户授权后浏览器跳转到 RedirectURI?code=...#state，用户复制 code 粘回后台。
//  3. ExchangeCode() 用 code + Verifier 换取 access/refresh token 并回填账号身份。
//  4. RefreshTokens() 在 access token 临期时用 refresh token 续期。

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
)

// Claude OAuth 配置常量。对齐 Claude Code 官方客户端在链路上的取值。
const (
	// ClaudeOAuthClientID 是 Claude Code 官方客户端的公开 OAuth client_id。
	ClaudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// ClaudeOAuthAuthURL 是授权页地址（用户浏览器打开处）。
	ClaudeOAuthAuthURL = "https://claude.ai/oauth/authorize"
	// ClaudeOAuthTokenURL 同时用于授权码交换与刷新（Claude Code 走 platform.claude.com）。
	ClaudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	// ClaudeOAuthProfileURL 用 access token 换取账号身份（email / uuid / 组织）。
	ClaudeOAuthProfileURL = "https://api.anthropic.com/api/oauth/profile"
	// ClaudeOAuthRedirectURI 是官方客户端「手动粘贴授权码」模式使用的托管回调页：
	// 授权完成后页面直接把 code#state 展示给用户复制，服务器场景无需本机监听，
	// 也不会像 localhost 回调那样出现「页面打不开」的困惑。
	ClaudeOAuthRedirectURI = "https://platform.claude.com/oauth/code/callback"
	// ClaudeOAuthLocalRedirectURI 是官方客户端本地回调地址。历史 CLI session 文件
	// 未记录 redirect_uri 时按该值换码，保证旧会话仍可完成交换。
	ClaudeOAuthLocalRedirectURI = "http://localhost:54545/callback"
	// ClaudeOAuthScope 是 Claude Code 请求的权限范围，必须与官方一致。
	ClaudeOAuthScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// ClaudeOAuthScopeInference 是 Setup Token(长效令牌)申请的最小权限范围，等同
	// 官方 `claude setup-token` 命令：只能推理，不能读 profile / usage。
	ClaudeOAuthScopeInference = "user:inference"
	// ClaudeSetupTokenExpiresInSeconds 是 Setup Token 交换时请求的有效期(1 年)。
	ClaudeSetupTokenExpiresInSeconds = 365 * 24 * 60 * 60
	// ClaudeOAuthBeta 是 OAuth 凭据调用推理接口时必须声明的 anthropic-beta 值。
	ClaudeOAuthBeta = "oauth-2025-04-20"
	// ClaudeCodeBeta 是真实 Claude Code CLI 恒定的 beta 标记（haiku 模型除外，
	// 见 CLI 2.1.259 逆向：_re() 中 model 含 "haiku" 时不 push 该 beta）。
	ClaudeCodeBeta = "claude-code-20250219"

	claudeOAuthHTTPTimeout = 30 * time.Second
)

// DefaultClaudeAllowedBetaHeaders 是真实 Claude Code CLI（2.1.259 逆向 chunk 内
// beta 注册表）在链路上出现过的 beta 值集合，外加已知下游客户端
// （opencode/ai-sdk anthropic provider）固定携带的 fine-grained-tool-streaming
// beta。当管理端未显式配置 ClaudeSecurityConfig.AllowedBetaHeaders 时，下游
// 透传的 anthropic-beta 以该集合为白名单，避免任意第三方 beta 名混入 OAuth
// 凭据的出站请求。
var DefaultClaudeAllowedBetaHeaders = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"context-1m-2025-08-07",
	"interleaved-thinking-2025-05-14",
	"fine-grained-tool-streaming-2025-05-14",
	"redact-thinking-2026-02-12",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"advanced-tool-use-2025-11-20",
	"tool-search-tool-2025-10-19",
	"effort-2025-11-24",
	"task-budgets-2026-03-13",
	"prompt-caching-evict-2026-05-12",
	"fallback-credit-2026-06-01",
	"extended-cache-ttl-2025-04-11",
	"fast-mode-2026-02-01",
	"structured-outputs-2025-12-15",
	"web-search-2025-03-05",
	"afk-mode-2026-01-31",
	"advisor-tool-2026-03-01",
	"cache-diagnosis-2026-04-07",
	"context-hint-2026-04-09",
	"mcp-servers-2025-12-04",
	"files-api-2025-04-14",
	"environments-2025-11-01",
	"ccr-byoc-2025-07-29",
	"per-turn-control-2026-07-01",
	"mid-conversation-tool-changes-2026-07-01",
	"server-side-fallback-2026-06-01",
	"server-side-fallback-2026-07-01",
	"auto-mode-classifier-2026-07-16",
	"thinking-display-updates-2026-08-18",
}

// ClaudePKCECodes 保存一对 PKCE 校验码（RFC 7636，S256）。
type ClaudePKCECodes struct {
	CodeVerifier  string
	CodeChallenge string
}

// ClaudeLoginSession 是一次登录发起后需要短期保存的上下文。ExchangeCode 时回传。
type ClaudeLoginSession struct {
	AuthURL  string `json:"auth_url"`
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	// RedirectURI 是本次授权 URL 使用的回调地址；交换时必须原样回传。空值表示
	// 历史会话(本地回调)。
	RedirectURI string `json:"redirect_uri,omitempty"`
	// Scope 是本次申请的权限范围;空=完整 Claude Code scope。
	Scope string `json:"scope,omitempty"`
	// SetupToken 为 true 表示本次申请的是长效 Setup Token(仅 user:inference)。
	SetupToken bool `json:"setup_token,omitempty"`
}

// ClaudeLoginOptions 控制一次登录申请的形态。零值 = 完整 OAuth + 托管回调页。
type ClaudeLoginOptions struct {
	// SetupToken 为 true 时申请长效 Setup Token:scope 收窄为 user:inference,
	// 交换时附带 expires_in=1 年,拿到的 access_token 没有 refresh_token。
	SetupToken bool
	// RedirectURI 覆盖回调地址;空=ClaudeOAuthRedirectURI(托管回调页)。
	RedirectURI string
}

// ExchangeOptions 返回与本会话匹配的交换参数。
func (s *ClaudeLoginSession) ExchangeOptions() ClaudeExchangeOptions {
	if s == nil {
		return ClaudeExchangeOptions{}
	}
	redirect := strings.TrimSpace(s.RedirectURI)
	if redirect == "" {
		// 旧版 session 文件没有 redirect_uri 字段,当时默认是本地回调。
		redirect = ClaudeOAuthLocalRedirectURI
	}
	return ClaudeExchangeOptions{RedirectURI: redirect, SetupToken: s.SetupToken}
}

// ClaudeExchangeOptions 控制授权码交换。
type ClaudeExchangeOptions struct {
	// RedirectURI 必须与生成授权 URL 时一致;空=ClaudeOAuthRedirectURI。
	RedirectURI string
	// SetupToken 为 true 时请求 1 年有效期,并跳过 profile 补全(scope 不含 user:profile)。
	SetupToken bool
}

// ClaudeTokenData 是登录/刷新后得到的令牌与账号身份。
type ClaudeTokenData struct {
	AccessToken      string
	RefreshToken     string
	Email            string
	AccountUUID      string
	OrganizationUUID string
	OrganizationName string
	// PlanType 是由 profile 推导的订阅档位(pro / max-5x / max-20x / team / …)。
	PlanType string
	// ExpiresAt 是本次 access token 的过期时刻（本地时钟）。
	ExpiresAt time.Time
	// AuthKind 标记凭据形态:ClaudeAuthKindOAuth(可刷新)或 ClaudeAuthKindSetupToken
	// (长效、无 RT)。空值按是否携带 refresh_token 推断。
	AuthKind string
}

// claudeTokenResponse 映射 Anthropic OAuth token 端点的响应体。
type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

// claudeOAuthProfile 映射 profile 端点的响应体。
type claudeOAuthProfile struct {
	Account struct {
		UUID         string `json:"uuid"`
		Email        string `json:"email"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
		// OrganizationType 是订阅档位判定主键(实测 2026-08):
		// claude_pro / claude_max / claude_team / claude_enterprise / claude_free。
		OrganizationType string `json:"organization_type"`
		// RateLimitTier 区分 Max 档倍率(如含 "5x" / "20x")。
		RateLimitTier string `json:"rate_limit_tier"`
	} `json:"organization"`
}

// DeriveClaudePlanType 由 profile 推导展示用套餐档位:
// pro / max-5x / max-20x / max / team / enterprise / free;无法判定时回退 "claude"。
func DeriveClaudePlanType(p *claudeOAuthProfile) string {
	if p == nil {
		return "claude"
	}
	orgType := strings.ToLower(strings.TrimSpace(p.Organization.OrganizationType))
	tier := strings.ToLower(strings.TrimSpace(p.Organization.RateLimitTier))
	switch {
	case strings.Contains(orgType, "max") || p.Account.HasClaudeMax:
		if strings.Contains(tier, "20x") {
			return "max-20x"
		}
		if strings.Contains(tier, "5x") {
			return "max-5x"
		}
		return "max"
	case strings.Contains(orgType, "enterprise"):
		return "enterprise"
	case strings.Contains(orgType, "team"):
		return "team"
	case strings.Contains(orgType, "pro") || p.Account.HasClaudePro:
		return "pro"
	case strings.Contains(orgType, "free"):
		return "free"
	}
	return "claude"
}

// claudeAuthCodeExchangeRequest 是授权码交换请求体。字段顺序刻意对齐官方客户端在
// 链路上的键序（map 会被 encoding/json 按字母重排，可能触发风控），故用结构体固定。
type claudeAuthCodeExchangeRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
	// ExpiresIn 仅 Setup Token 交换时携带(秒),请求长效令牌。
	ExpiresIn int `json:"expires_in,omitempty"`
}

// ClaudeAuth 封装 Claude OAuth 登录/刷新所需的 HTTP 客户端。
//
// 采用主/备双客户端 + 自动回退:
//   - primary:uTLS 浏览器指纹客户端,规避 Anthropic 域名上的 Cloudflare 指纹拦截;
//   - fallback:标准 http 客户端(ALPN 自动协商 h1/h2,兼容性更好)。
//
// 当 primary 出现传输错误或被判定为挑战(403)时,自动改用 fallback 重试。这样无论
// 拦截来自指纹、强制 h2 还是网络层,都能提高登录/刷新成功率。
type ClaudeAuth struct {
	primary  *http.Client
	fallback *http.Client
}

// NewClaudeAuth 创建一个 Claude OAuth 客户端。proxyURL 为空时走直连。
func NewClaudeAuth(proxyURL string) *ClaudeAuth {
	proxyURL = strings.TrimSpace(proxyURL)
	primary := buildUTLSHTTPClient(proxyURL)
	if primary == nil {
		primary = buildPlainClaudeOAuthClient(proxyURL)
	} else if primary.Timeout == 0 {
		primary.Timeout = claudeOAuthHTTPTimeout
	}
	return &ClaudeAuth{primary: primary, fallback: buildPlainClaudeOAuthClient(proxyURL)}
}

// buildPlainClaudeOAuthClient 构建标准(非 uTLS)代理感知 HTTP 客户端,用作回退。
func buildPlainClaudeOAuthClient(proxyURL string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		_ = ConfigureTransportProxy(tr, proxyURL, nil)
	}
	return &http.Client{Transport: tr, Timeout: claudeOAuthHTTPTimeout}
}

// doWithFallback 用 primary 发送请求;传输错误或 403 挑战时,用 fallback 以全新请求
// 重试。bodyBytes 为请求体(GET 传 nil);decorate 用于附加 Authorization 等额外头。
func (o *ClaudeAuth) doWithFallback(ctx context.Context, method, url string, bodyBytes []byte, decorate func(*http.Request)) (*http.Response, error) {
	build := func() (*http.Request, error) {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, err
		}
		applyClaudeOAuthAxiosHeaders(req)
		if decorate != nil {
			decorate(req)
		}
		return req, nil
	}

	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := o.primary.Do(req)
	if err == nil && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	// primary 传输失败或被 403 挑战 → 用标准客户端重试。
	if resp != nil {
		_ = resp.Body.Close()
	}
	retryReq, buildErr := build()
	if buildErr != nil {
		if err != nil {
			return nil, err
		}
		return nil, buildErr
	}
	return o.fallback.Do(retryReq)
}

// GenerateClaudePKCE 生成一对 PKCE 校验码（S256）。
func GenerateClaudePKCE() (*ClaudePKCECodes, error) {
	verifierBytes := make([]byte, 96)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("生成 PKCE verifier 失败: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return &ClaudePKCECodes{CodeVerifier: verifier, CodeChallenge: challenge}, nil
}

// generateClaudeOAuthState 生成用于防 CSRF 的随机 state。
func generateClaudeOAuthState() (string, error) {
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", fmt.Errorf("生成 OAuth state 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(stateBytes), nil
}

// BuildClaudeAuthURL 用给定 state 与 PKCE 拼装完整 OAuth 授权 URL(托管回调页)。
func BuildClaudeAuthURL(state string, pkce *ClaudePKCECodes) (string, error) {
	return BuildClaudeAuthURLWithOptions(state, pkce, ClaudeLoginOptions{})
}

// BuildClaudeAuthURLWithOptions 按登录选项拼装授权 URL:Setup Token 模式只申请
// user:inference;redirect_uri 可覆盖(必须与后续交换一致)。
func BuildClaudeAuthURLWithOptions(state string, pkce *ClaudePKCECodes, opts ClaudeLoginOptions) (string, error) {
	if pkce == nil {
		return "", fmt.Errorf("缺少 PKCE 校验码")
	}
	scope := ClaudeOAuthScope
	if opts.SetupToken {
		scope = ClaudeOAuthScopeInference
	}
	redirect := strings.TrimSpace(opts.RedirectURI)
	if redirect == "" {
		redirect = ClaudeOAuthRedirectURI
	}
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {ClaudeOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {scope},
		"code_challenge":        {pkce.CodeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return ClaudeOAuthAuthURL + "?" + params.Encode(), nil
}

// StartClaudeLogin 发起一次完整 OAuth 登录：生成 state + PKCE 并返回授权 URL 与需缓存的上下文。
func StartClaudeLogin() (*ClaudeLoginSession, error) {
	return StartClaudeLoginWithOptions(ClaudeLoginOptions{})
}

// StartClaudeLoginWithOptions 按选项发起登录(完整 OAuth 或 Setup Token)。返回的
// 会话记录了 redirect_uri / scope / 令牌形态,交换时用 ExchangeOptions() 回传。
func StartClaudeLoginWithOptions(opts ClaudeLoginOptions) (*ClaudeLoginSession, error) {
	pkce, err := GenerateClaudePKCE()
	if err != nil {
		return nil, err
	}
	state, err := generateClaudeOAuthState()
	if err != nil {
		return nil, err
	}
	redirect := strings.TrimSpace(opts.RedirectURI)
	if redirect == "" {
		redirect = ClaudeOAuthRedirectURI
	}
	opts.RedirectURI = redirect
	authURL, err := BuildClaudeAuthURLWithOptions(state, pkce, opts)
	if err != nil {
		return nil, err
	}
	scope := ClaudeOAuthScope
	if opts.SetupToken {
		scope = ClaudeOAuthScopeInference
	}
	return &ClaudeLoginSession{
		AuthURL:     authURL,
		State:       state,
		Verifier:    pkce.CodeVerifier,
		RedirectURI: redirect,
		Scope:       scope,
		SetupToken:  opts.SetupToken,
	}, nil
}

// parseClaudeCodeAndState 从用户粘贴的内容里拆出授权码与可能附带的 state。支持:
//   - 托管回调页展示的 code#state;
//   - 整条回调 URL(…/callback?code=XXX&state=YYY);
//   - 纯 code。
func parseClaudeCodeAndState(code string) (parsedCode, parsedState string) {
	raw := strings.TrimSpace(code)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil {
			q := u.Query()
			if c := strings.TrimSpace(q.Get("code")); c != "" {
				return c, strings.TrimSpace(q.Get("state"))
			}
		}
	}
	splits := strings.Split(raw, "#")
	parsedCode = strings.TrimSpace(splits[0])
	if len(splits) > 1 {
		parsedState = strings.TrimSpace(splits[1])
	}
	return
}

// ExchangeCode 用授权码 + PKCE verifier 换取 access/refresh token，并回填账号身份。
// 使用默认交换参数(托管回调页、完整 OAuth)。
//
//   - code：用户从回调页复制的授权码（可含 #state 片段或整条回调 URL）。
//   - state：StartClaudeLogin 返回的 state。
//   - verifier：StartClaudeLogin 返回的 verifier。
func (o *ClaudeAuth) ExchangeCode(ctx context.Context, code, state, verifier string) (*ClaudeTokenData, error) {
	return o.ExchangeCodeWithOptions(ctx, code, state, verifier, ClaudeExchangeOptions{})
}

// ExchangeCodeWithOptions 是 ExchangeCode 的带选项版本:redirect_uri 必须与生成
// 授权 URL 时一致;SetupToken 模式请求 1 年有效期且不补 profile。
func (o *ClaudeAuth) ExchangeCodeWithOptions(ctx context.Context, code, state, verifier string, opts ClaudeExchangeOptions) (*ClaudeTokenData, error) {
	if strings.TrimSpace(verifier) == "" {
		return nil, fmt.Errorf("缺少 PKCE verifier")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	newCode, newState := parseClaudeCodeAndState(code)
	if newCode == "" {
		return nil, fmt.Errorf("授权码为空")
	}
	effectiveState := state
	if newState != "" {
		effectiveState = newState
	}
	return o.exchangeAuthorizationCode(ctx, newCode, verifier, effectiveState, opts)
}

// exchangeAuthorizationCode 是 PKCE 流程最后一步,被浏览器授权与 sessionKey 登录共用。
func (o *ClaudeAuth) exchangeAuthorizationCode(ctx context.Context, code, verifier, state string, opts ClaudeExchangeOptions) (*ClaudeTokenData, error) {
	redirect := strings.TrimSpace(opts.RedirectURI)
	if redirect == "" {
		redirect = ClaudeOAuthRedirectURI
	}
	reqBody := claudeAuthCodeExchangeRequest{
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  redirect,
		ClientID:     ClaudeOAuthClientID,
		CodeVerifier: verifier,
		State:        state,
	}
	if opts.SetupToken {
		reqBody.ExpiresIn = ClaudeSetupTokenExpiresInSeconds
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化授权码交换请求失败: %w", err)
	}

	body, status, err := o.doClaudeOAuthPost(ctx, ClaudeOAuthTokenURL, jsonBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("授权码交换失败 (status %d): %s", status, string(body))
	}

	var tokenResp claudeTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, fmt.Errorf("token 响应缺少 access_token")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
		if opts.SetupToken {
			expiresIn = ClaudeSetupTokenExpiresInSeconds
		}
	}
	td := &ClaudeTokenData{
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		Email:            tokenResp.Account.EmailAddress,
		AccountUUID:      tokenResp.Account.UUID,
		OrganizationUUID: tokenResp.Organization.UUID,
		OrganizationName: tokenResp.Organization.Name,
		ExpiresAt:        time.Now().Add(time.Duration(expiresIn) * time.Second),
		AuthKind:         ClaudeAuthKindOAuth,
	}
	if opts.SetupToken {
		// Setup Token 只有 user:inference,profile 端点必 403;不打无意义请求。
		td.AuthKind = ClaudeAuthKindSetupToken
		td.RefreshToken = ""
		return td, nil
	}
	// 用 profile 端点补齐 token 响应可能缺失的身份字段。
	if profile, errProfile := o.FetchProfile(ctx, tokenResp.AccessToken); errProfile == nil && profile != nil {
		if v := strings.TrimSpace(profile.Account.UUID); v != "" {
			td.AccountUUID = v
		}
		if v := strings.TrimSpace(profile.Account.Email); v != "" {
			td.Email = v
		}
		if v := strings.TrimSpace(profile.Organization.UUID); v != "" {
			td.OrganizationUUID = v
		}
		if v := strings.TrimSpace(profile.Organization.Name); v != "" {
			td.OrganizationName = v
		}
		td.PlanType = DeriveClaudePlanType(profile)
	}
	return td, nil
}

// RefreshTokens 用 refresh token 续期。Anthropic 若未返回新的 refresh token，则沿用旧值。
func (o *ClaudeAuth) RefreshTokens(ctx context.Context, refreshToken string) (*ClaudeTokenData, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("缺少 refresh token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 刷新请求体键序对齐官方客户端。
	reqBody := map[string]string{
		"client_id":     ClaudeOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"scope":         ClaudeOAuthScope,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化刷新请求失败: %w", err)
	}

	body, status, err := o.doClaudeOAuthPost(ctx, ClaudeOAuthTokenURL, jsonBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("token 刷新失败 (status %d): %s", status, string(body))
	}

	var tokenResp claudeTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("解析刷新响应失败: %w", err)
	}
	if strings.TrimSpace(tokenResp.RefreshToken) == "" {
		tokenResp.RefreshToken = refreshToken
	}
	td := &ClaudeTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}
	if profile, errProfile := o.FetchProfile(ctx, tokenResp.AccessToken); errProfile == nil && profile != nil {
		td.Email = strings.TrimSpace(profile.Account.Email)
		td.AccountUUID = strings.TrimSpace(profile.Account.UUID)
		td.OrganizationUUID = strings.TrimSpace(profile.Organization.UUID)
		td.OrganizationName = strings.TrimSpace(profile.Organization.Name)
		td.PlanType = DeriveClaudePlanType(profile)
	}
	return td, nil
}

// FetchProfile 用 access token 拉取账号身份。
func (o *ClaudeAuth) FetchProfile(ctx context.Context, accessToken string) (*claudeOAuthProfile, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("缺少 access token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := o.doWithFallback(ctx, http.MethodGet, ClaudeOAuthProfileURL, nil, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	})
	if err != nil {
		return nil, fmt.Errorf("profile 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("读取 profile 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 profile 失败 (status %d): %s", resp.StatusCode, string(body))
	}
	var profile claudeOAuthProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("解析 profile 响应失败: %w", err)
	}
	if strings.TrimSpace(profile.Account.UUID) == "" {
		return nil, fmt.Errorf("profile 响应缺少账号 UUID")
	}
	return &profile, nil
}

// ClaudeModelsListURL 是 Anthropic 官方模型列表端点(返回该凭据真实可用的模型)。
const ClaudeModelsListURL = "https://api.anthropic.com/v1/models"

// FetchModels 用 access token 拉取该账号**真实可用**的模型 ID 列表(动态发现,
// 不写死)。分页拉全(has_more/last_id)。失败时由调用方回退到内置兜底集。
func (o *ClaudeAuth) FetchModels(ctx context.Context, accessToken string) ([]string, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("缺少 access token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := make([]string, 0, 16)
	seen := map[string]struct{}{}
	afterID := ""
	for page := 0; page < 10; page++ { // 上限保护,正常一两页即可拉全
		url := ClaudeModelsListURL + "?limit=100"
		if afterID != "" {
			url += "&after_id=" + afterID
		}
		resp, err := o.doWithFallback(ctx, http.MethodGet, url, nil, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("anthropic-beta", ClaudeOAuthBeta)
		})
		if err != nil {
			return nil, fmt.Errorf("拉取 Claude 模型列表失败: %w", err)
		}
		body, readErr := readClaudeOAuthResponseBody(resp)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取模型列表响应失败: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("获取模型列表失败 (status %d): %s", resp.StatusCode, string(body))
		}
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("解析模型列表失败: %w", err)
		}
		for _, m := range parsed.Data {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			if _, ok := seen[strings.ToLower(id)]; ok {
				continue
			}
			seen[strings.ToLower(id)] = struct{}{}
			ids = append(ids, id)
		}
		if !parsed.HasMore || strings.TrimSpace(parsed.LastID) == "" {
			break
		}
		afterID = parsed.LastID
	}
	return ids, nil
}

// doClaudeOAuthPost 发送一个 axios 伪装的 OAuth POST，返回解码后的响应体与状态码。
func (o *ClaudeAuth) doClaudeOAuthPost(ctx context.Context, endpoint string, jsonBody []byte) ([]byte, int, error) {
	resp, err := o.doWithFallback(ctx, http.MethodPost, endpoint, jsonBody, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("OAuth 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取 OAuth 响应失败: %w", err)
	}
	return body, resp.StatusCode, nil
}

// applyClaudeOAuthAxiosHeaders 复刻官方客户端 OAuth 控制面请求的 axios 头，降低被
// Cloudflare 拦截的概率。
func applyClaudeOAuthAxiosHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "axios/1.15.2")
	req.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	// 注意:本模块的 HTTP 客户端是 HTTP/2(buildUTLSHTTPClient 强制 h2)。HTTP/2
	// 协议禁止 Connection / Keep-Alive 等逐跳头,设置它们会让 Go 的 http2 transport
	// 直接以 "invalid Connection request header" 拒发请求(登录/刷新全失败)。因此这里
	// 不设置 Connection: close 也不置 req.Close——h2 本就不携带这些头。
}

// readClaudeOAuthResponseBody 读取并按 Content-Encoding 解码响应体。
// 因为我们手动设置了 Accept-Encoding，Go 的 transport 不会自动解压，需自行处理。
func readClaudeOAuthResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("响应体为空")
	}
	encoded, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	encodings := strings.Split(strings.Join(resp.Header.Values("Content-Encoding"), ","), ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		encoding := strings.ToLower(strings.TrimSpace(encodings[i]))
		if encoding == "" || encoding == "identity" {
			continue
		}
		encoded, err = decodeClaudeOAuthEncoding(encoded, encoding)
		if err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func decodeClaudeOAuthEncoding(encoded []byte, encoding string) ([]byte, error) {
	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return nil, fmt.Errorf("解码 gzip 响应失败: %w", err)
		}
		reader = gz
	case "deflate":
		if zr, err := zlib.NewReader(bytes.NewReader(encoded)); err == nil {
			reader = zr
		} else {
			reader = flate.NewReader(bytes.NewReader(encoded))
		}
	case "br":
		reader = io.NopCloser(brotli.NewReader(bytes.NewReader(encoded)))
	default:
		return nil, fmt.Errorf("不支持的 Content-Encoding: %q", encoding)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("解码 %s 响应失败: %w", encoding, err)
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("关闭 %s 解码器失败: %w", encoding, err)
	}
	return decoded, nil
}
