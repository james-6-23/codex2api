package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	antigravityAuthorizationURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	antigravityOAuthScopes          = "openid https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs https://www.googleapis.com/auth/aicode"
	antigravityOAuthCallbackPath    = "/oauth-callback"
	antigravityOAuthClientsEnv      = "ANTIGRAVITY_OAUTH_CLIENTS"
	antigravityActiveOAuthClientEnv = "ANTIGRAVITY_OAUTH_CLIENT_KEY"
	antigravityUserAgentEnv         = "ANTIGRAVITY_USER_AGENT"
	antigravityResponseLimit        = 4 << 20

	// 官方 Antigravity 桌面端的 Desktop OAuth client。Google 把 secret 打进安装包，
	// 社区（sub2api / opencode / jcode）普遍当公开凭据内置。环境变量与系统设置
	// 都未配置时回落到这一套，账号页可以直接「开始授权」。
	AntigravityDefaultOAuthClientKey    = "official"
	AntigravityDefaultOAuthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	AntigravityDefaultOAuthClientSecret = "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"
)

func builtinAntigravityOAuthClient() antigravityOAuthClient {
	return antigravityOAuthClient{
		Key:          AntigravityDefaultOAuthClientKey,
		ClientID:     AntigravityDefaultOAuthClientID,
		ClientSecret: AntigravityDefaultOAuthClientSecret,
	}
}

var DefaultAntigravityEndpoints = AntigravityEndpoints{
	TokenURL:    "https://oauth2.googleapis.com/token",
	UserInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo",
	LoadProject: []string{
		"https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:loadCodeAssist",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
	},
	Quota: []string{
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	},
	QuotaSummary: []string{
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	},
	AICredits: []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
	},
	// onboardUser provisions the Cloud Code companion project for a Google
	// account that has never used Antigravity. loadCodeAssist returns an empty
	// cloudaicompanionProject for those accounts, and without a project the
	// account can never be dispatched. The native client onboards on the daily
	// host; production is kept as a fallback only.
	OnboardUser: []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:onboardUser",
		"https://cloudcode-pa.googleapis.com/v1internal:onboardUser",
	},
}

const (
	// antigravityOnboardPollAttempts / antigravityOnboardPollInterval bound the
	// long-running-operation poll for onboardUser. The operation normally
	// completes within one or two polls; five attempts two seconds apart is the
	// budget used by the native client.
	antigravityOnboardPollAttempts = 5
	antigravityOnboardPollInterval = 2 * time.Second
	// antigravityOnboardDefaultTier is used when loadCodeAssist advertised no
	// default tier at all.
	antigravityOnboardDefaultTier = "free-tier"
	// antigravityOnboardAPIClient mirrors the X-Goog-Api-Client header the
	// native Node client sends on the onboarding call.
	antigravityOnboardAPIClient = "gl-node/22.21.1"
)

type antigravityOAuthClient struct {
	Key          string
	ClientID     string
	ClientSecret string
}

type AntigravityOAuthClientInfo struct {
	Key      string `json:"key"`
	ClientID string `json:"client_id"`
}

type AntigravityClient struct {
	httpClient *http.Client
	endpoints  AntigravityEndpoints
	oauth      []antigravityOAuthClient
	activeKey  string
	userAgent  string
	now        func() time.Time
	// sleep is the poll delay used while waiting for onboardUser to finish.
	// Injectable so tests do not spend real seconds.
	sleep func(context.Context, time.Duration) error
}

type antigravityTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

type antigravityTierPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	QuotaTier string `json:"quotaTier"`
	Slug      string `json:"slug"`
	// Cloud Code emits camelCase (isDefault); the snake_case alias is kept so
	// stored snapshots and any proto-JSON variant still parse.
	IsDefault                          bool `json:"isDefault"`
	IsDefaultSnake                     bool `json:"is_default"`
	UserDefinedCloudaicompanionProject bool `json:"userDefinedCloudaicompanionProject"`
	AvailableCredits                   []struct {
		CreditAmount any `json:"creditAmount"`
	} `json:"availableCredits"`
}

func (t antigravityTierPayload) isDefaultTier() bool {
	return t.IsDefault || t.IsDefaultSnake
}

type antigravityLoadProjectResponse struct {
	ProjectID       string                   `json:"cloudaicompanionProject"`
	CurrentTier     *antigravityTierPayload  `json:"currentTier"`
	PaidTier        *antigravityTierPayload  `json:"paidTier"`
	AllowedTiers    []antigravityTierPayload `json:"allowedTiers"`
	IneligibleTiers []struct {
		ReasonCode string `json:"reasonCode"`
	} `json:"ineligibleTiers"`
}

// antigravityOnboardResponse is the long-running operation envelope returned
// by v1internal:onboardUser. The provisioned project appears under response
// either as a bare string or as an object carrying id.
type antigravityOnboardResponse struct {
	Done     bool `json:"done"`
	Response struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	} `json:"response"`
}

func (r antigravityOnboardResponse) projectID() string {
	raw := bytes.TrimSpace(r.Response.Project)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var asObject struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		return strings.TrimSpace(asObject.ID)
	}
	return ""
}

type antigravityQuotaResponse struct {
	Models map[string]struct {
		QuotaInfo *struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		} `json:"quotaInfo"`
		DisplayName        string          `json:"displayName"`
		IsInternal         bool            `json:"isInternal"`
		SupportsImages     *bool           `json:"supportsImages"`
		SupportsThinking   *bool           `json:"supportsThinking"`
		ThinkingBudget     *int            `json:"thinkingBudget"`
		Recommended        *bool           `json:"recommended"`
		MaxTokens          *int            `json:"maxTokens"`
		MaxOutputTokens    *int            `json:"maxOutputTokens"`
		SupportedMimeTypes map[string]bool `json:"supportedMimeTypes"`
	} `json:"models"`
	DeprecatedModelIDs map[string]struct {
		NewModelID string `json:"newModelId"`
	} `json:"deprecatedModelIds"`
}

const (
	// antigravityValidationRequiredReason 是 Google 对账号触发风控验证时错误体
	// 里 details[].reason 的取值。该栅栏是账号侧的持续状态,不完成验证每次
	// 同步都会命中,渲染时不能写成 "temporarily unavailable"。
	antigravityValidationRequiredReason = "VALIDATION_REQUIRED"
	// antigravityTOSViolationReason 是账号因违反服务条款被停用的取值,
	// 同样是持续性封禁,唯一出路是错误体里附带的申诉表单链接。
	antigravityTOSViolationReason = "TOS_VIOLATION"
)

// antigravityUpstreamFailure 保留一次上游调用的结构化失败
// (retrieveUserQuotaSummary / loadCodeAssist 等),用于把真实原因带进
// 同步 warning 而非只回 bool 或塞整坨 JSON 原文。
type antigravityUpstreamFailure struct {
	Status        int
	Reason        string
	Message       string
	ValidationURL string
	AppealURL     string
}

type antigravityErrorPayload struct {
	Error struct {
		Message string `json:"message"`
		Details []struct {
			Reason   string `json:"reason"`
			Metadata struct {
				ValidationURL string `json:"validation_url"`
				AppealURL     string `json:"appeal_url"`
			} `json:"metadata"`
		} `json:"details"`
	} `json:"error"`
}

// antigravityUpstreamFailureFrom 把 HTTP/网络失败折叠成可渲染的诊断。
func antigravityUpstreamFailureFrom(status int, err error) antigravityUpstreamFailure {
	failure := antigravityUpstreamFailure{Status: status, Message: strings.TrimSpace(safeAntigravityError(err))}
	var httpErr *antigravityHTTPError
	if !errors.As(err, &httpErr) || len(httpErr.rawBody) == 0 {
		return failure
	}
	var payload antigravityErrorPayload
	if json.Unmarshal(httpErr.rawBody, &payload) != nil {
		return failure
	}
	if message := strings.TrimSpace(payload.Error.Message); message != "" {
		failure.Message = message
	}
	for _, detail := range payload.Error.Details {
		if failure.Reason == "" {
			failure.Reason = strings.TrimSpace(detail.Reason)
		}
		if failure.ValidationURL == "" {
			failure.ValidationURL = strings.TrimSpace(detail.Metadata.ValidationURL)
		}
		if failure.AppealURL == "" {
			failure.AppealURL = strings.TrimSpace(detail.Metadata.AppealURL)
		}
	}
	return failure
}

// antigravityActionableFailureWarning 渲染一次带操作入口的上游失败。
// subject 指明是什么不可用(如 "Antigravity quota summary")。
// VALIDATION_REQUIRED / TOS_VIOLATION 都不是暂时性抖动:前者要账号所有者
// 完成验证、后者只能走申诉表单,必须把链接带出来而不是含糊的 "temporarily"。
func antigravityActionableFailureWarning(subject string, failure antigravityUpstreamFailure) string {
	switch failure.Reason {
	case antigravityValidationRequiredReason:
		message := subject + " unavailable: Google requires the account owner to verify the account"
		if failure.Message != "" {
			message += " (" + failure.Message + ")"
		}
		if failure.ValidationURL != "" {
			message += ". Sign in as this Google account in a browser and open the verification URL: " + failure.ValidationURL
		}
		return message
	case antigravityTOSViolationReason:
		message := subject + " unavailable: Google disabled this account for a Terms of Service violation"
		if failure.Message != "" {
			message += " (" + failure.Message + ")"
		}
		if failure.AppealURL != "" {
			message += ". Submit an appeal to restore access: " + failure.AppealURL
		}
		return message
	}
	detail := failure.Message
	if detail == "" && failure.Status > 0 {
		detail = fmt.Sprintf("upstream returned HTTP %d", failure.Status)
	}
	if detail == "" {
		detail = "no upstream endpoint succeeded"
	}
	return subject + " is temporarily unavailable: " + detail
}

// antigravityEntitlementsWarning 渲染 loadCodeAssist 失败。非 HTTP 错误
// (网络/刷新类)原文已足够短,原样保留;HTTP 错误的原文是整坨上游 JSON
// (appeal_url 等一次性链接埋在里面),折叠成带链接的短文案。
func antigravityEntitlementsWarning(err error) string {
	var httpErr *antigravityHTTPError
	if !errors.As(err, &httpErr) {
		return safeAntigravityError(err)
	}
	return antigravityActionableFailureWarning("Antigravity entitlements", antigravityUpstreamFailureFrom(httpErr.Status, httpErr))
}

type antigravityQuotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Buckets     []struct {
			BucketID          string   `json:"bucketId"`
			Window            string   `json:"window"`
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
			DisplayName       string   `json:"displayName"`
			Description       string   `json:"description"`
		} `json:"buckets"`
	} `json:"groups"`
}

type antigravityHTTPError struct {
	Status int
	Body   string
	// rawBody keeps the untruncated response body. Body is compacted to 512
	// bytes for rendering, which can cut structured error payloads mid-field
	// (e.g. the one-time VALIDATION_REQUIRED verification URL is ~300 bytes
	// on its own); diagnosis must parse the untouched bytes instead.
	rawBody []byte
}

func (e *antigravityHTTPError) Error() string {
	return fmt.Sprintf("Antigravity upstream returned HTTP %d: %s", e.Status, e.Body)
}

type antigravityOAuthRefreshFailure struct {
	ClientKey string
	Err       error
	Permanent bool
}

// antigravityOAuthRefreshError preserves the classification of every OAuth
// client candidate. Its rendered message remains useful for operators, but
// retry policy must use PermanentRefreshFailure instead of scanning that
// aggregate text: an invalid custom client followed by a transient fallback
// failure is still retryable.
type antigravityOAuthRefreshError struct {
	Failures []antigravityOAuthRefreshFailure
}

func (e *antigravityOAuthRefreshError) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "Antigravity token refresh failed"
	}
	failures := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		failures = append(failures, failure.ClientKey+": "+safeAntigravityError(failure.Err))
	}
	return "Antigravity token refresh failed: " + strings.Join(failures, " | ")
}

func (e *antigravityOAuthRefreshError) PermanentRefreshFailure() bool {
	if e == nil || len(e.Failures) == 0 {
		return false
	}
	for _, failure := range e.Failures {
		if !failure.Permanent {
			return false
		}
	}
	return true
}

func NewAntigravityClient(proxyURL string) (*AntigravityClient, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is unavailable")
	}
	cloned := transport.Clone()
	if err := ConfigureTransportProxy(cloned, proxyURL, nil); err != nil {
		return nil, err
	}
	return newAntigravityClient(&http.Client{Transport: cloned, Timeout: 30 * time.Second}, DefaultAntigravityEndpoints), nil
}

// AntigravityClientVersion is the Antigravity Hub release reported to Cloud
// Code. The model catalog is gated by client version: older versions are
// served a reduced catalog (e.g. without per-tier Gemini 3.7/3.8 IDs).
const AntigravityClientVersion = "2.19.1"

// AntigravityUserAgent is shared by control-plane and generation requests so
// Cloud Code sees one consistent client version. ANTIGRAVITY_USER_AGENT
// overrides both.
func AntigravityUserAgent() string {
	if userAgent := strings.TrimSpace(os.Getenv(antigravityUserAgentEnv)); userAgent != "" {
		return userAgent
	}
	return "antigravity/hub/" + AntigravityClientVersion + " windows/amd64"
}

// antigravityVersionFromUserAgent extracts the version from either the
// "antigravity/hub/<v> <platform>" or legacy "antigravity/<v> <platform>" form.
func antigravityVersionFromUserAgent(userAgent string) string {
	product, _, _ := strings.Cut(strings.TrimSpace(userAgent), " ")
	lower := strings.ToLower(product)
	for _, prefix := range []string{"antigravity/hub/", "antigravity/"} {
		if strings.HasPrefix(lower, prefix) {
			if version := product[len(prefix):]; version != "" {
				return version
			}
		}
	}
	return AntigravityClientVersion
}

func newAntigravityClient(httpClient *http.Client, endpoints AntigravityEndpoints) *AntigravityClient {
	clients, activeKey := effectiveAntigravityOAuthClients()
	return &AntigravityClient{
		httpClient: httpClient,
		endpoints:  endpoints,
		oauth:      clients,
		activeKey:  activeKey,
		userAgent:  AntigravityUserAgent(),
		now:        time.Now,
	}
}

func antigravityOAuthClientsFromEnv() []antigravityOAuthClient {
	clients := make([]antigravityOAuthClient, 0)
	for _, entry := range strings.Split(os.Getenv(antigravityOAuthClientsEnv), ";") {
		parts := strings.Split(entry, "|")
		if len(parts) < 3 {
			continue
		}
		candidate := antigravityOAuthClient{
			Key:      strings.ToLower(strings.TrimSpace(parts[0])),
			ClientID: strings.TrimSpace(parts[1]), ClientSecret: strings.TrimSpace(parts[2]),
		}
		if candidate.Key == "" || candidate.ClientID == "" || candidate.ClientSecret == "" {
			continue
		}
		replaced := false
		for i := range clients {
			if clients[i].Key == candidate.Key {
				clients[i] = candidate
				replaced = true
				break
			}
		}
		if !replaced {
			clients = append(clients, candidate)
		}
	}
	return clients
}

func antigravityActiveOAuthKeyFromEnv() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv(antigravityActiveOAuthClientEnv)))
}

func (c *AntigravityClient) resolveOAuthClient(clientKey string) (antigravityOAuthClient, error) {
	if len(c.oauth) == 0 {
		return antigravityOAuthClient{}, fmt.Errorf("no Antigravity OAuth clients configured; add one in the admin settings page or set %s", antigravityOAuthClientsEnv)
	}
	clientKey = strings.ToLower(strings.TrimSpace(clientKey))
	if clientKey == "" {
		clientKey = strings.ToLower(strings.TrimSpace(c.activeKey))
	}
	for _, candidate := range c.oauth {
		if strings.EqualFold(candidate.Key, clientKey) {
			return candidate, nil
		}
	}
	return antigravityOAuthClient{}, fmt.Errorf("unknown Antigravity OAuth client key: %s", clientKey)
}

func (c *AntigravityClient) BuildOAuthAuthorizationURL(redirectURI, state, codeChallenge, clientKey string) (string, AntigravityOAuthClientInfo, error) {
	redirectURI = strings.TrimSpace(redirectURI)
	// State is an opaque CSRF nonce. Validate surrounding whitespace without
	// normalizing the value that is sent to Google or compared by the callback
	// session.
	if redirectURI == "" || strings.TrimSpace(state) == "" || strings.TrimSpace(codeChallenge) == "" {
		return "", AntigravityOAuthClientInfo{}, errors.New("redirect_uri, state, and code_challenge are required")
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed == nil || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Opaque != "" ||
		!strings.EqualFold(parsed.Scheme, "http") || parsed.Host == "" || parsed.Path != antigravityOAuthCallbackPath ||
		(parsed.Hostname() != "127.0.0.1" && !strings.EqualFold(parsed.Hostname(), "localhost")) || parsed.Port() == "" {
		return "", AntigravityOAuthClientInfo{}, errors.New("invalid Antigravity OAuth redirect_uri")
	}
	client, err := c.resolveOAuthClient(clientKey)
	if err != nil {
		return "", AntigravityOAuthClientInfo{}, err
	}
	params := url.Values{
		"client_id":              {client.ClientID},
		"redirect_uri":           {redirectURI},
		"response_type":          {"code"},
		"scope":                  {antigravityOAuthScopes},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {state},
		"code_challenge":         {codeChallenge},
		"code_challenge_method":  {"S256"},
	}
	return antigravityAuthorizationURL + "?" + params.Encode(), AntigravityOAuthClientInfo{Key: client.Key, ClientID: client.ClientID}, nil
}

func (c *AntigravityClient) ExchangeOAuthAuthorizationCode(ctx context.Context, code, redirectURI, codeVerifier, clientKey string) (AntigravityCredential, error) {
	code = strings.TrimSpace(code)
	redirectURI = strings.TrimSpace(redirectURI)
	codeVerifier = strings.TrimSpace(codeVerifier)
	if code == "" || redirectURI == "" || codeVerifier == "" {
		return AntigravityCredential{}, errors.New("authorization code, redirect_uri, and code_verifier are required")
	}
	client, err := c.resolveOAuthClient(clientKey)
	if err != nil {
		return AntigravityCredential{}, err
	}
	form := url.Values{
		"client_id":     {client.ClientID},
		"client_secret": {client.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return AntigravityCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	var token antigravityTokenResponse
	if err := c.doJSON(req, &token); err != nil {
		return AntigravityCredential{}, fmt.Errorf("Antigravity authorization code exchange failed: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return AntigravityCredential{}, errors.New("Antigravity OAuth server returned no access_token")
	}
	credential := AntigravityCredential{
		AccessToken:    strings.TrimSpace(token.AccessToken),
		RefreshToken:   strings.TrimSpace(token.RefreshToken),
		IDToken:        strings.TrimSpace(token.IDToken),
		OAuthClientKey: client.Key,
		ClientID:       client.ClientID,
		ClientSecret:   client.ClientSecret,
		Scope:          strings.TrimSpace(token.Scope),
	}
	if token.ExpiresIn > 0 {
		credential.ExpiresAt = c.now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return credential, nil
}

func (c *AntigravityClient) Sync(ctx context.Context, credential AntigravityCredential) (AntigravitySyncResult, error) {
	credential.AccessToken = strings.TrimSpace(credential.AccessToken)
	credential.RefreshToken = strings.TrimSpace(credential.RefreshToken)
	if credential.AccessToken == "" && credential.RefreshToken == "" {
		return AntigravitySyncResult{}, errors.New("Antigravity credential has no access_token or refresh_token")
	}

	if credential.AccessToken == "" || (!credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(c.now().Add(5*time.Minute))) {
		if err := c.refreshCredential(ctx, &credential); err != nil {
			return AntigravitySyncResult{}, err
		}
	}

	profile, entitlements, entitlementErr, err := c.fetchIdentityContext(ctx, &credential, true)
	if err != nil && credential.RefreshToken != "" && isAntigravityUnauthorized(err) {
		if refreshErr := c.refreshCredential(ctx, &credential); refreshErr != nil {
			return AntigravitySyncResult{Credential: credential}, refreshErr
		}
		profile, entitlements, entitlementErr, err = c.fetchIdentityContext(ctx, &credential, true)
	}
	if err != nil {
		return AntigravitySyncResult{Credential: credential}, err
	}

	quota, quotaErr := c.fetchQuota(ctx, credential.AccessToken, credential.ProjectID)
	if quotaErr != nil && isAntigravityUnauthorized(quotaErr) && credential.RefreshToken != "" {
		if refreshErr := c.refreshCredential(ctx, &credential); refreshErr != nil {
			return antigravityPartialSyncResult(credential, profile, entitlements, entitlementErr), refreshErr
		}
		// The imported access token and refresh token can belong to different
		// principals. After refreshing, rebuild every identity-bound snapshot
		// before retrying quota so a mixed Google identity cannot be persisted.
		profile, entitlements, entitlementErr, err = c.fetchIdentityContext(ctx, &credential, false)
		if err != nil {
			return antigravityPartialSyncResult(credential, profile, entitlements, entitlementErr), err
		}
		quota, quotaErr = c.fetchQuota(ctx, credential.AccessToken, credential.ProjectID)
	}
	if quotaErr != nil {
		result := antigravityPartialSyncResult(credential, profile, entitlements, entitlementErr)
		result.Quota = quota
		return result, quotaErr
	}
	if quota.Forbidden {
		entitlements.Allowed = false
		entitlements.Reason = "Google quota API denied access"
	}
	quotaGroups, quotaGroupsObserved, quotaSummaryFailure := c.fetchQuotaSummary(ctx, credential.AccessToken, credential.ProjectID)
	quota.Groups = quotaGroups
	aiCredits, aiCreditsObserved := c.fetchAICredits(ctx, credential.AccessToken)
	quota.AICredits = aiCredits

	result := AntigravitySyncResult{
		Credential: credential, Profile: profile, Entitlements: entitlements, Quota: quota,
		EntitlementsObserved: entitlementErr == nil,
		QuotaGroupsObserved:  quotaGroupsObserved,
		AICreditsObserved:    aiCreditsObserved,
	}
	warnings := make([]string, 0, 3)
	if entitlementErr != nil {
		warnings = append(warnings, antigravityEntitlementsWarning(entitlementErr))
	}
	if !quotaGroupsObserved {
		warnings = append(warnings, antigravityActionableFailureWarning("Antigravity quota summary", quotaSummaryFailure))
	}
	if !aiCreditsObserved {
		warnings = append(warnings, "Antigravity AI credits are temporarily unavailable")
	}
	result.Warning = strings.Join(warnings, "; ")
	return result, nil
}

func (c *AntigravityClient) fetchIdentityContext(ctx context.Context, credential *AntigravityCredential, preserveProject bool) (AntigravityProfile, AntigravityEntitlements, error, error) {
	if credential == nil {
		return AntigravityProfile{}, AntigravityEntitlements{}, nil, errors.New("Antigravity credential is required")
	}
	profile, err := c.fetchProfile(ctx, credential.AccessToken)
	if err != nil {
		return AntigravityProfile{}, AntigravityEntitlements{}, err, err
	}
	if !profile.VerifiedEmail {
		err := errors.New("Google user profile did not confirm a verified email")
		return profile, AntigravityEntitlements{}, err, err
	}
	credential.Email = profile.Email
	credential.Name = profile.Name
	credential.AvatarURL = profile.Picture

	projectID := ""
	if preserveProject {
		projectID = strings.TrimSpace(credential.ProjectID)
	}
	userProjectID := strings.TrimSpace(credential.UserProjectID)
	entitlements, entitlementErr := c.fetchEntitlements(ctx, credential.AccessToken, userProjectID)
	if entitlementErr == nil && entitlements.ProjectID == "" && entitlements.CurrentTier != nil && userProjectID != "" {
		// Already onboarded to a tier that runs against a caller-owned project:
		// loadCodeAssist never echoes that project back.
		entitlements.ProjectID = userProjectID
	}
	if entitlements.ProjectID == "" {
		entitlements.ProjectID = projectID
	}
	if entitlementErr == nil && entitlements.ProjectID == "" {
		// loadCodeAssist succeeded but the account has no companion project
		// yet. Provision one the way the native client does; otherwise the
		// account is stored healthy but is never dispatchable.
		onboarded, onboardErr := c.onboardProject(ctx, credential.AccessToken, entitlements, userProjectID)
		if onboardErr != nil {
			entitlementErr = fmt.Errorf("Antigravity project provisioning (onboardUser) failed: %w", onboardErr)
		} else {
			entitlements.ProjectID = onboarded
		}
	}
	credential.ProjectID = entitlements.ProjectID
	return profile, entitlements, entitlementErr, nil
}

// antigravityOnboardTierID picks the tier onboardUser should provision:
// the advertised default tier first, then the current tier, then free-tier.
func antigravityOnboardTierID(entitlements AntigravityEntitlements) string {
	for _, tier := range entitlements.AllowedTiers {
		if tier.IsDefault && strings.TrimSpace(tier.ID) != "" {
			return strings.TrimSpace(tier.ID)
		}
	}
	if entitlements.CurrentTier != nil && strings.TrimSpace(entitlements.CurrentTier.ID) != "" {
		return strings.TrimSpace(entitlements.CurrentTier.ID)
	}
	return antigravityOnboardDefaultTier
}

// antigravityProjectRequiredError explains why onboardUser finished without a
// project: only the free/consumer tiers get a Google-managed project.
func antigravityProjectRequiredError(tierID string, entitlements AntigravityEntitlements) error {
	userDefined := false
	for _, tier := range entitlements.AllowedTiers {
		if tier.ID == tierID && tier.UserDefinedProject {
			userDefined = true
		}
	}
	if entitlements.CurrentTier != nil && entitlements.CurrentTier.UserDefinedProject {
		userDefined = true
	}
	reason := "onboardUser completed without a project id"
	if userDefined {
		reason = fmt.Sprintf("tier %q requires a caller-owned Google Cloud project", tierID)
	}
	return fmt.Errorf("%s; Workspace / enterprise accounts (Gemini Code Assist Standard or Enterprise) are not given a managed project, set the GCP project ID on this account and sync again", reason)
}

// onboardProject calls v1internal:onboardUser and polls the returned
// long-running operation until it reports done with a project id.
func (c *AntigravityClient) onboardProject(ctx context.Context, accessToken string, entitlements AntigravityEntitlements, userProjectID string) (string, error) {
	if len(c.endpoints.OnboardUser) == 0 {
		return "", errors.New("no onboardUser endpoint configured")
	}
	tierID := antigravityOnboardTierID(entitlements)
	metadata := map[string]any{
		"ideType":    "ANTIGRAVITY",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}
	payload := map[string]any{"tierId": tierID, "metadata": metadata}
	// The free tier runs on a Google-managed project; naming a project there
	// makes onboardUser fail with FAILED_PRECONDITION.
	if userProjectID != "" && tierID != antigravityOnboardDefaultTier {
		payload["cloudaicompanionProject"] = userProjectID
		metadata["duetProject"] = userProjectID
	}
	var lastErr error
	for _, endpoint := range c.endpoints.OnboardUser {
		for attempt := range antigravityOnboardPollAttempts {
			if attempt > 0 {
				if err := c.sleepFor(ctx, antigravityOnboardPollInterval); err != nil {
					return "", err
				}
			}
			var response antigravityOnboardResponse
			status, err := c.postJSONWithHeaders(ctx, endpoint, accessToken, payload, &response, map[string]string{
				"X-Goog-Api-Client": antigravityOnboardAPIClient,
			})
			if err != nil {
				lastErr = err
				if status != http.StatusTooManyRequests && status < http.StatusInternalServerError {
					// Non-retryable on this host; a 404 means the host does not
					// serve onboardUser, so try the next endpoint.
					break
				}
				continue
			}
			if projectID := response.projectID(); projectID != "" {
				return projectID, nil
			}
			if !response.Done {
				lastErr = errors.New("onboardUser operation is still pending")
				continue
			}
			if userProjectID != "" {
				return userProjectID, nil
			}
			lastErr = antigravityProjectRequiredError(tierID, entitlements)
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("onboardUser did not return a project id")
	}
	return "", lastErr
}

func (c *AntigravityClient) sleepFor(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func antigravityPartialSyncResult(credential AntigravityCredential, profile AntigravityProfile, entitlements AntigravityEntitlements, entitlementErr error) AntigravitySyncResult {
	result := AntigravitySyncResult{
		Credential: credential, Profile: profile, Entitlements: entitlements,
		EntitlementsObserved: entitlementErr == nil,
	}
	if entitlementErr != nil {
		result.Warning = antigravityEntitlementsWarning(entitlementErr)
	}
	return result
}

func (c *AntigravityClient) refreshCredential(ctx context.Context, credential *AntigravityCredential) error {
	if credential == nil || strings.TrimSpace(credential.RefreshToken) == "" {
		return errors.New("Antigravity refresh_token is required")
	}
	candidates := c.oauthCandidates(*credential)
	if len(candidates) == 0 {
		return errors.New("no Antigravity OAuth client is configured")
	}
	failures := make([]antigravityOAuthRefreshFailure, 0, len(candidates))
	for _, candidate := range candidates {
		form := url.Values{
			"client_id": {candidate.ClientID}, "client_secret": {candidate.ClientSecret},
			"refresh_token": {credential.RefreshToken}, "grant_type": {"refresh_token"},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", c.userAgent)
		var token antigravityTokenResponse
		err = c.doJSON(req, &token)
		if err == nil && strings.TrimSpace(token.AccessToken) != "" {
			credential.AccessToken = strings.TrimSpace(token.AccessToken)
			if strings.TrimSpace(token.RefreshToken) != "" {
				credential.RefreshToken = strings.TrimSpace(token.RefreshToken)
			}
			// A refresh response that omits id_token must not leave the old
			// principal's token attached to the new access token. The profile
			// lookup is the authoritative identity check performed afterwards.
			credential.IDToken = strings.TrimSpace(token.IDToken)
			if strings.TrimSpace(token.Scope) != "" {
				credential.Scope = strings.TrimSpace(token.Scope)
			}
			if token.ExpiresIn > 0 {
				credential.ExpiresAt = c.now().Add(time.Duration(token.ExpiresIn) * time.Second)
			}
			// Persist the OAuth client as one coherent credential tuple. Updating
			// only the key would make stale custom client credentials masquerade
			// as the successful fallback on the next refresh; oauthCandidates
			// would then de-duplicate the real configured fallback by that key.
			credential.OAuthClientKey = candidate.Key
			credential.ClientID = candidate.ClientID
			credential.ClientSecret = candidate.ClientSecret
			return nil
		}
		failures = append(failures, antigravityOAuthRefreshFailure{
			ClientKey: candidate.Key,
			Err:       err,
			Permanent: isPermanentAntigravityOAuthClientError(err),
		})
		if !shouldTryNextAntigravityOAuthClient(err) {
			break
		}
	}
	return &antigravityOAuthRefreshError{Failures: failures}
}

func (c *AntigravityClient) oauthCandidates(credential AntigravityCredential) []antigravityOAuthClient {
	result := make([]antigravityOAuthClient, 0, len(c.oauth)+1)
	seen := make(map[string]struct{})
	push := func(client antigravityOAuthClient) {
		key := strings.ToLower(strings.TrimSpace(client.Key))
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(client.ClientID))
		}
		if key == "" || client.ClientID == "" || client.ClientSecret == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		client.Key = key
		result = append(result, client)
	}
	if strings.TrimSpace(credential.ClientID) != "" && strings.TrimSpace(credential.ClientSecret) != "" {
		push(antigravityOAuthClient{Key: credential.OAuthClientKey, ClientID: credential.ClientID, ClientSecret: credential.ClientSecret})
	}
	for _, preferred := range []string{credential.OAuthClientKey, c.activeKey} {
		preferred = strings.ToLower(strings.TrimSpace(preferred))
		for _, client := range c.oauth {
			if client.Key == preferred {
				push(client)
			}
		}
	}
	for _, client := range c.oauth {
		push(client)
	}
	// 导入的官方桌面端 refresh_token 往往没有自带 client 凭据；自定义 client
	// 刷新失败时再试一次内置官方 Desktop client。
	push(builtinAntigravityOAuthClient())
	return result
}

const (
	antigravityProfileFetchAttempts  = 3
	antigravityProfileRetryBaseDelay = 200 * time.Millisecond
)

func isTransientAntigravityTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *antigravityHTTPError
	return !errors.As(err, &httpErr)
}

func (c *AntigravityClient) fetchProfile(ctx context.Context, accessToken string) (AntigravityProfile, error) {
	var profile AntigravityProfile
	var lastErr error
	for attempt := range antigravityProfileFetchAttempts {
		if attempt > 0 {
			if err := c.sleepFor(ctx, antigravityProfileRetryBaseDelay*time.Duration(attempt)); err != nil {
				return AntigravityProfile{}, lastErr
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoints.UserInfoURL, nil)
		if err != nil {
			return AntigravityProfile{}, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", c.userAgent)
		profile = AntigravityProfile{}
		lastErr = c.doJSON(req, &profile)
		if lastErr == nil {
			break
		}
		if !isTransientAntigravityTransportError(lastErr) {
			return AntigravityProfile{}, lastErr
		}
	}
	if lastErr != nil {
		return AntigravityProfile{}, lastErr
	}
	profile.Email = strings.TrimSpace(profile.Email)
	if profile.Email == "" {
		return AntigravityProfile{}, errors.New("Google user profile has no email")
	}
	if strings.TrimSpace(profile.Name) == "" {
		profile.Name = profile.Email
	}
	return profile, nil
}

func (c *AntigravityClient) fetchEntitlements(ctx context.Context, accessToken, userProjectID string) (AntigravityEntitlements, error) {
	metadata := map[string]any{"ideType": "ANTIGRAVITY"}
	request := map[string]any{"metadata": metadata}
	if userProjectID != "" {
		request["cloudaicompanionProject"] = userProjectID
		metadata["duetProject"] = userProjectID
	}
	var lastErr error
	for _, endpoint := range c.endpoints.LoadProject {
		var payload antigravityLoadProjectResponse
		status, err := c.postJSON(ctx, endpoint, accessToken, request, &payload)
		if err == nil {
			return normalizeAntigravityEntitlements(payload, c.now()), nil
		}
		lastErr = err
		if status != http.StatusTooManyRequests && status < http.StatusInternalServerError {
			break
		}
	}
	return AntigravityEntitlements{UpdatedAt: c.now().UTC()}, lastErr
}

func normalizeAntigravityEntitlements(payload antigravityLoadProjectResponse, now time.Time) AntigravityEntitlements {
	result := AntigravityEntitlements{Allowed: true, ProjectID: strings.TrimSpace(payload.ProjectID), UpdatedAt: now.UTC()}
	result.CurrentTier = convertAntigravityTier(payload.CurrentTier)
	result.PaidTier = convertAntigravityTier(payload.PaidTier)
	for i := range payload.AllowedTiers {
		result.AllowedTiers = append(result.AllowedTiers, *convertAntigravityTier(&payload.AllowedTiers[i]))
	}
	for _, tier := range payload.IneligibleTiers {
		result.IneligibleTiers = append(result.IneligibleTiers, AntigravityIneligibleTier{ReasonCode: strings.TrimSpace(tier.ReasonCode)})
	}
	result.Restricted = len(result.IneligibleTiers) > 0
	if result.Restricted {
		result.Reason = "Google marked one or more subscription tiers as ineligible"
	}
	if result.PaidTier != nil {
		result.EffectiveTier = firstNonEmpty(result.PaidTier.Name, result.PaidTier.ID)
	}
	if result.EffectiveTier == "" && !result.Restricted && result.CurrentTier != nil {
		result.EffectiveTier = firstNonEmpty(result.CurrentTier.Name, result.CurrentTier.ID)
	}
	if result.EffectiveTier == "" && len(result.AllowedTiers) > 0 {
		selected := result.AllowedTiers[0]
		for _, tier := range result.AllowedTiers {
			if tier.IsDefault {
				selected = tier
				break
			}
		}
		result.EffectiveTier = firstNonEmpty(selected.Name, selected.ID)
		if result.Restricted && result.EffectiveTier != "" {
			result.EffectiveTier += " (Restricted)"
		}
	}
	return result
}

func convertAntigravityTier(tier *antigravityTierPayload) *AntigravityTier {
	if tier == nil {
		return nil
	}
	return &AntigravityTier{
		ID: strings.TrimSpace(tier.ID), Name: strings.TrimSpace(tier.Name),
		QuotaTier: strings.TrimSpace(tier.QuotaTier), Slug: strings.TrimSpace(tier.Slug), IsDefault: tier.isDefaultTier(),
		UserDefinedProject: tier.UserDefinedCloudaicompanionProject,
	}
}

func (c *AntigravityClient) fetchQuota(ctx context.Context, accessToken, projectID string) (AntigravityQuotaSnapshot, error) {
	payload := map[string]any{}
	if strings.TrimSpace(projectID) != "" {
		payload["project"] = strings.TrimSpace(projectID)
	}
	var lastErr error
	for endpointIndex, endpoint := range c.endpoints.Quota {
		current := payload
		retriedWithoutProject := false
		for {
			var response antigravityQuotaResponse
			status, err := c.postJSON(ctx, endpoint, accessToken, current, &response)
			if err == nil {
				return normalizeAntigravityQuota(response, c.now()), nil
			}
			lastErr = err
			if status == http.StatusForbidden && len(current) > 0 && !retriedWithoutProject {
				current = map[string]any{}
				retriedWithoutProject = true
				continue
			}
			if status == http.StatusForbidden {
				if endpointIndex == len(c.endpoints.Quota)-1 {
					return AntigravityQuotaSnapshot{Models: []AntigravityModelQuota{}, Forbidden: true, UpdatedAt: c.now().UTC()}, nil
				}
				break
			}
			if status == http.StatusUnauthorized {
				return AntigravityQuotaSnapshot{}, err
			}
			if status != 0 && status != http.StatusTooManyRequests && status < http.StatusInternalServerError {
				return AntigravityQuotaSnapshot{}, err
			}
			break
		}
	}
	return AntigravityQuotaSnapshot{}, lastErr
}

func normalizeAntigravityQuota(payload antigravityQuotaResponse, now time.Time) AntigravityQuotaSnapshot {
	result := AntigravityQuotaSnapshot{
		Models: []AntigravityModelQuota{}, ModelForwardingRules: map[string]string{}, UpdatedAt: now.UTC(),
	}
	for modelID, model := range payload.Models {
		if validAntigravityDiscoveredModelID(strings.TrimSpace(modelID)) {
			result.CatalogModelIDs = append(result.CatalogModelIDs, strings.TrimSpace(modelID))
			if model.IsInternal {
				result.InternalModelIDs = append(result.InternalModelIDs, strings.TrimSpace(modelID))
			}
		}
		if model.QuotaInfo == nil || !isTrackedAntigravityModel(modelID) {
			continue
		}
		var fraction *float64
		fractionPresent := false
		if model.QuotaInfo.RemainingFraction != nil {
			value := *model.QuotaInfo.RemainingFraction
			if value < 0 {
				value = 0
			}
			if value > 1 {
				value = 1
			}
			fraction = &value
			fractionPresent = true
		}
		percent := 0
		if fractionPresent {
			percent = int(*fraction * 100)
		}
		result.Models = append(result.Models, AntigravityModelQuota{
			ModelID: modelID, DisplayName: model.DisplayName,
			RemainingFraction: fraction, FractionPresent: fractionPresent,
			RemainingPercent: percent, ResetTime: model.QuotaInfo.ResetTime,
			SupportsImages: model.SupportsImages, SupportsThinking: model.SupportsThinking,
			ThinkingBudget: model.ThinkingBudget, Recommended: model.Recommended,
			MaxTokens: model.MaxTokens, MaxOutputTokens: model.MaxOutputTokens,
			SupportedMIMEs: model.SupportedMimeTypes,
		})
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].ModelID < result.Models[j].ModelID })
	sort.Strings(result.CatalogModelIDs)
	sort.Strings(result.InternalModelIDs)
	for oldID, rule := range payload.DeprecatedModelIDs {
		if strings.TrimSpace(oldID) != "" && strings.TrimSpace(rule.NewModelID) != "" {
			result.ModelForwardingRules[oldID] = strings.TrimSpace(rule.NewModelID)
		}
	}
	if len(result.ModelForwardingRules) == 0 {
		result.ModelForwardingRules = nil
	}
	return result
}

func isTrackedAntigravityModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"gemini", "claude", "gpt", "image", "imagen"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// NormalizeAntigravityWindow 把上游 window 字段规整为白名单内的稳定标识。
// 白名单之外（daily/monthly/未知）一律 ok=false，调用方不得据此做出配额判定。
func NormalizeAntigravityWindow(raw string) (string, bool) {
	norm := strings.ToLower(strings.TrimSpace(raw))
	switch norm {
	case "5h", "five_hour", "5_hour", "fivehour":
		return "5h", true
	case "weekly", "week", "7d", "seven_day":
		return "weekly", true
	default:
		return "", false
	}
}

// ValidateResetTime 解析 RFC3339 的 reset_time，并检查它晚于 observedAt 且落在
// 对应窗口的物理上限内（5h <= 6h，weekly <= 8d）。任何不成立的情形返回零值，
// 由调用方回落到 TTL 推算，绝不把越界时标当成权威重置时刻。
func ValidateResetTime(resetStr string, observedAt time.Time, window string) (time.Time, bool) {
	if strings.TrimSpace(resetStr) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(resetStr))
	if err != nil {
		return time.Time{}, false
	}
	if !parsed.After(observedAt) {
		return time.Time{}, false
	}
	diff := parsed.Sub(observedAt)
	switch window {
	case "5h":
		if diff > 6*time.Hour {
			return time.Time{}, false
		}
	case "weekly":
		if diff > 8*24*time.Hour {
			return time.Time{}, false
		}
	default:
		return time.Time{}, false
	}
	return parsed, true
}

// ClassifyAntigravityFraction 按 Spec §3.2 把单个桶数值收敛为四态（absent 由
// summary_integrity 权威清单在合并层判定，解析层只区分 known_positive/known_zero/unknown）。
func ClassifyAntigravityFraction(value *float64, present bool) string {
	if !present || value == nil {
		return "unknown"
	}
	v := *value
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0.0 || v > 1.0 {
		return "unknown"
	}
	if v == 0.0 {
		return "known_zero"
	}
	return "known_positive"
}

// ApplyQuotaGroupsV2 按 Spec §3.2 合并一次 groups 观测：
//   - incoming 为空：整体残缺，不得推进成功（返回 advanced=false），保留 prev；
//   - incoming 非空：替换所有已返回桶（坏桶按 presence 判 unknown，绝不复活旧零），
//     未返回旧桶原样保留 observed_at/observation_seq（不洗新）；
//   - 仅 summaryIntegrity=true（完整权威清单）才删除未返回桶。
func ApplyQuotaGroupsV2(prev *AntigravityQuotaSnapshot, incoming []AntigravityQuotaGroup, summaryIntegrity bool, observedAt time.Time, ttlSeconds int, seq int64) (AntigravityQuotaSnapshot, bool) {
	if len(incoming) == 0 {
		if prev == nil {
			return AntigravityQuotaSnapshot{SchemaVersion: 2}, false
		}
		return *prev, false
	}
	next := AntigravityQuotaSnapshot{SchemaVersion: 2}
	if prev != nil {
		next.SourceMetadata = prev.SourceMetadata
		next.Models = prev.Models
		next.UpdatedAt = prev.UpdatedAt
	}
	returned := make(map[string]bool)
	for _, group := range incoming {
		g := group
		for i := range g.Buckets {
			b := g.Buckets[i]
			if w, known := NormalizeAntigravityWindow(b.Window); known {
				b.Window = w
			}
			b.ObservedAt = observedAt
			b.ObservationSeq = seq
			b.TTLSeconds = ttlSeconds
			if !b.FractionPresent {
				b.RemainingFraction = nil // 坏桶隔离：unknown，绝不沿用旧数值
			}
			returned[b.BucketID+"|"+b.Window] = true
			g.Buckets[i] = b
		}
		next.Groups = append(next.Groups, g)
	}
	if prev != nil && !summaryIntegrity {
		for _, group := range prev.Groups {
			var retained []AntigravityQuotaBucket
			for _, b := range group.Buckets {
				if !returned[b.BucketID+"|"+b.Window] {
					retained = append(retained, b)
				}
			}
			if len(retained) > 0 {
				next.Groups = append(next.Groups, AntigravityQuotaGroup{DisplayName: group.DisplayName, Description: group.Description, Buckets: retained})
			}
		}
	}
	return next, true
}

func (c *AntigravityClient) fetchQuotaSummary(ctx context.Context, accessToken, projectID string) ([]AntigravityQuotaGroup, bool, antigravityUpstreamFailure) {
	payload := map[string]any{}
	if strings.TrimSpace(projectID) != "" {
		payload["project"] = strings.TrimSpace(projectID)
	}
	var failure antigravityUpstreamFailure
	for _, endpoint := range c.endpoints.QuotaSummary {
		var response antigravityQuotaSummaryResponse
		status, err := c.postJSON(ctx, endpoint, accessToken, payload, &response)
		if err != nil {
			failure = antigravityUpstreamFailureFrom(status, err)
			if status == http.StatusForbidden || status == http.StatusNotFound {
				continue
			}
			if status >= http.StatusBadRequest && status < http.StatusInternalServerError && status != http.StatusTooManyRequests {
				return nil, false, failure
			}
			continue
		}
		groups := make([]AntigravityQuotaGroup, 0, len(response.Groups))
		for _, group := range response.Groups {
			item := AntigravityQuotaGroup{DisplayName: group.DisplayName, Description: group.Description, Buckets: []AntigravityQuotaBucket{}}
			for _, bucket := range group.Buckets {
				// 保留上游真实指针与 presence：缺失值保持 nil/false（unknown），
				// 绝不退化成 0.0，否则坏桶会被下游当成"配额耗尽"硬封账号。
				var fraction *float64
				fractionPresent := false
				if bucket.RemainingFraction != nil {
					value := *bucket.RemainingFraction
					fraction = &value
					fractionPresent = true
				}
				item.Buckets = append(item.Buckets, AntigravityQuotaBucket{
					BucketID: bucket.BucketID, Window: bucket.Window,
					RemainingFraction: fraction, FractionPresent: fractionPresent,
					ResetTime: bucket.ResetTime, DisplayName: bucket.DisplayName, Description: bucket.Description,
				})
			}
			groups = append(groups, item)
		}
		return groups, true, failure
	}
	return nil, false, failure
}

func (c *AntigravityClient) fetchAICredits(ctx context.Context, accessToken string) (*AntigravityAICredits, bool) {
	for _, endpoint := range c.endpoints.AICredits {
		var response antigravityLoadProjectResponse
		_, err := c.postJSON(ctx, endpoint, accessToken, map[string]any{
			"metadata": map[string]any{"ide_type": "ANTIGRAVITY", "ide_version": antigravityVersionFromUserAgent(c.userAgent), "ide_name": "antigravity"},
		}, &response)
		if err != nil {
			continue
		}
		if response.PaidTier == nil || len(response.PaidTier.AvailableCredits) == 0 {
			return nil, true
		}
		amount := response.PaidTier.AvailableCredits[0].CreditAmount
		// Proto3 JSON omits zero-valued fields: an entry without creditAmount
		// (current clients receive one with only creditType and the usage
		// minimum) means a zero balance, not an unreadable response.
		if amount == nil {
			return &AntigravityAICredits{}, true
		}
		if credits, ok := parseAntigravityNumber(amount); ok {
			return &AntigravityAICredits{Credits: credits}, true
		}
	}
	return nil, false
}

func parseAntigravityNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	case string:
		value, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return value, err == nil
	default:
		return 0, false
	}
}

func (c *AntigravityClient) postJSON(ctx context.Context, endpoint, accessToken string, payload any, output any) (int, error) {
	return c.postJSONWithHeaders(ctx, endpoint, accessToken, payload, output, nil)
}

func (c *AntigravityClient) postJSONWithHeaders(ctx context.Context, endpoint, accessToken string, payload any, output any, extraHeaders map[string]string) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}
	err = c.doJSON(req, output)
	if httpErr := (*antigravityHTTPError)(nil); errors.As(err, &httpErr) {
		return httpErr.Status, err
	}
	return 0, err
}

func (c *AntigravityClient) doJSON(req *http.Request, output any) error {
	response, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, antigravityResponseLimit+1))
	if err != nil {
		return err
	}
	if len(body) > antigravityResponseLimit {
		return errors.New("Antigravity upstream response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &antigravityHTTPError{Status: response.StatusCode, Body: compactAntigravityBody(body), rawBody: body}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Antigravity response: %w", err)
	}
	return nil
}

func compactAntigravityBody(body []byte) string {
	value := strings.Join(strings.Fields(string(body)), " ")
	if len(value) > 512 {
		value = value[:512]
	}
	if value == "" {
		return "empty response"
	}
	return value
}

func shouldTryNextAntigravityOAuthClient(err error) bool {
	var httpErr *antigravityHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	return httpErr.Status == http.StatusBadRequest || httpErr.Status == http.StatusUnauthorized ||
		strings.Contains(body, "unauthorized_client") || strings.Contains(body, "invalid_client")
}

func isPermanentAntigravityOAuthClientError(err error) bool {
	var httpErr *antigravityHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status < http.StatusBadRequest || httpErr.Status >= http.StatusInternalServerError {
		return false
	}
	var payload struct {
		Error string `json:"error"`
	}
	code := ""
	if json.Unmarshal([]byte(httpErr.Body), &payload) == nil {
		code = strings.ToLower(strings.TrimSpace(payload.Error))
	}
	if code == "" {
		body := strings.ToLower(strings.Trim(strings.TrimSpace(httpErr.Body), "\"'"))
		if values, parseErr := url.ParseQuery(body); parseErr == nil {
			code = strings.ToLower(strings.TrimSpace(values.Get("error")))
		}
		if code == "" {
			code = body
		}
	}
	switch code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied":
		return true
	default:
		return false
	}
}

func isAntigravityUnauthorized(err error) bool {
	var httpErr *antigravityHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusUnauthorized
}

func safeAntigravityError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
