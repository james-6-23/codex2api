package auth

import (
	"bytes"
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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Kiro 控制面：额度、模型目录、Enterprise profile、Social / IdC 设备码登录。
// 移植自 kiro.rs src/kiro/{token_manager,auth/social,auth/idc}.rs。

const (
	// kiroUsageAPIVersion 是用量类接口固定使用的 IDE 版本，避免版本漂移影响查询结果。
	kiroUsageAPIVersion = "0.9.2"
	KiroPortalURL       = "https://app.kiro.dev"
	KiroAuthEndpoint    = "https://prod.us-east-1.auth.desktop.kiro.dev"
	KiroBuilderIDStart  = "https://view.awsapps.com/start"
	// KiroSocialCallbackPort 是官方门户接受的本地回调端口之一。
	KiroSocialCallbackPort = 3128
)

var kiroOIDCScopes = []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"}

var kiroAPIDomainSetting atomic.Value

// SetKiroAPIDomain 切换上游域名：""/"amazonaws"（q.{region}.amazonaws.com）或 "kiro"（*.kiro.dev）。
func SetKiroAPIDomain(domain string) {
	kiroAPIDomainSetting.Store(strings.ToLower(strings.TrimSpace(domain)))
}

// KiroAPIDomain 返回当前上游域名设置。
func KiroAPIDomain() string {
	value, _ := kiroAPIDomainSetting.Load().(string)
	return value
}

// KiroRuntimeHost 是对话与 MCP 主机。
func KiroRuntimeHost(region string) string {
	if KiroAPIDomain() == "kiro" {
		return "runtime." + region + ".kiro.dev"
	}
	return "q." + region + ".amazonaws.com"
}

// KiroManagementHost 是额度、模型与 profile 主机。
func KiroManagementHost(region string) string {
	if KiroAPIDomain() == "kiro" {
		return "management." + region + ".kiro.dev"
	}
	return "q." + region + ".amazonaws.com"
}

// kiroRESTRegions：用量类接口只在 us-east-1 与 eu-central-1 提供，按 SSO 区域选主端点。
func kiroRESTRegions(ssoRegion string) []string {
	if strings.HasPrefix(ssoRegion, "eu-") {
		return []string{"eu-central-1", "us-east-1"}
	}
	return []string{"us-east-1", "eu-central-1"}
}

func kiroManagementHeaders(req *http.Request, cred KiroCredential, bearer string) {
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.0 KiroIDE-"+kiroUsageAPIVersion+"-"+cred.MachineID)
	req.Header.Set("User-Agent", "aws-sdk-js/1.0.0 ua/2.1 os/"+KiroOSName+" lang/js md/nodejs#"+KiroNodeVersion+" api/codewhispererruntime#1.0.0 m/N,E KiroIDE-"+kiroUsageAPIVersion+"-"+cred.MachineID)
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if tokenType := cred.TokenTypeHeader(); tokenType != "" {
		req.Header.Set("tokentype", tokenType)
	}
	req.Close = true
}

// kiroManagementGET 依次尝试候选区域与「带 / 不带 profileArn」，返回首个成功响应体。
func kiroManagementGET(ctx context.Context, cred KiroCredential, bearer, proxyURL, path string) ([]byte, error) {
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	arn := cred.EffectiveProfileARN()
	var lastErr error
	for _, region := range kiroRESTRegions(cred.AuthRegion()) {
		arns := []string{""}
		if arn != "" {
			arns = []string{arn, ""}
		}
		for _, candidateARN := range arns {
			target := "https://" + KiroManagementHost(region) + path
			if candidateARN != "" {
				target += "&profileArn=" + url.QueryEscape(candidateARN)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return nil, err
			}
			kiroManagementHeaders(req, cred, bearer)
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return raw, nil
			}
			lastErr = fmt.Errorf("HTTP %d %s", resp.StatusCode, truncateKiroError(string(raw)))
			retryable := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest && (strings.Contains(string(raw), "Improperly formed request") || strings.Contains(string(raw), "Invalid profileArn"))
			if !retryable {
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

// KiroUsageSnapshot 是账号额度快照，存入 credentials.kiro_usage 供账号页展示与冷却截止判断。
type KiroUsageSnapshot struct {
	SubscriptionTitle string    `json:"subscription_title,omitempty"`
	SubscriptionType  string    `json:"subscription_type,omitempty"`
	Used              float64   `json:"used"`
	Limit             float64   `json:"limit"`
	BaseUsed          float64   `json:"base_used"`
	BaseLimit         float64   `json:"base_limit"`
	BonusUsed         float64   `json:"bonus_used"`
	BonusLimit        float64   `json:"bonus_limit"`
	TrialUsed         float64   `json:"trial_used"`
	TrialLimit        float64   `json:"trial_limit"`
	NextResetAt       string    `json:"next_reset_at,omitempty"`
	CreditsExpireAt   string    `json:"credits_expire_at,omitempty"`
	OverageStatus     string    `json:"overage_status,omitempty"`
	FetchedAt         time.Time `json:"fetched_at"`
}

type kiroUsageResponse struct {
	NextDateReset    *float64 `json:"nextDateReset"`
	SubscriptionInfo struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
		Type              string `json:"type"`
	} `json:"subscriptionInfo"`
	OverageConfiguration struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	UsageBreakdownList []struct {
		ResourceType              *string  `json:"resourceType"`
		CurrentUsage              *float64 `json:"currentUsage"`
		CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
		UsageLimit                *float64 `json:"usageLimit"`
		UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
		NextDateReset             *float64 `json:"nextDateReset"`
		FreeTrialInfo             *struct {
			FreeTrialStatus           string   `json:"freeTrialStatus"`
			FreeTrialExpiry           *float64 `json:"freeTrialExpiry"`
			CurrentUsage              *float64 `json:"currentUsage"`
			CurrentUsageWithPrecision *float64 `json:"currentUsageWithPrecision"`
			UsageLimit                *float64 `json:"usageLimit"`
			UsageLimitWithPrecision   *float64 `json:"usageLimitWithPrecision"`
		} `json:"freeTrialInfo"`
		Bonuses []struct {
			Status       string   `json:"status"`
			ExpiresAt    *float64 `json:"expiresAt"`
			CurrentUsage *float64 `json:"currentUsage"`
			UsageLimit   *float64 `json:"usageLimit"`
		} `json:"bonuses"`
	} `json:"usageBreakdownList"`
}

func firstFloat(values ...*float64) float64 {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return 0
}

func kiroEpochRFC3339(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
}

// ParseKiroUsage 汇总基础额度 + 生效中的免费试用 + 生效中的赠送额度。
func ParseKiroUsage(raw []byte, now time.Time) (*KiroUsageSnapshot, error) {
	var parsed kiroUsageResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("kiro 额度响应无法解析: %w", err)
	}
	snapshot := &KiroUsageSnapshot{SubscriptionTitle: parsed.SubscriptionInfo.SubscriptionTitle, SubscriptionType: parsed.SubscriptionInfo.Type, OverageStatus: parsed.OverageConfiguration.OverageStatus, FetchedAt: now.UTC()}
	index := -1
	for i, item := range parsed.UsageBreakdownList {
		if item.ResourceType != nil && strings.EqualFold(*item.ResourceType, "CREDIT") {
			index = i
			break
		}
	}
	if index < 0 && len(parsed.UsageBreakdownList) == 1 && parsed.UsageBreakdownList[0].ResourceType == nil {
		index = 0
	}
	reset := firstFloat(parsed.NextDateReset)
	if index < 0 {
		snapshot.NextResetAt = kiroEpochRFC3339(reset)
		return snapshot, nil
	}
	item := parsed.UsageBreakdownList[index]
	if reset == 0 {
		reset = firstFloat(item.NextDateReset)
	}
	snapshot.NextResetAt = kiroEpochRFC3339(reset)
	nowSeconds := float64(now.Unix())
	earliestExpiry := 0.0
	noteExpiry := func(expiry float64) {
		if expiry > 0 && (earliestExpiry == 0 || expiry < earliestExpiry) {
			earliestExpiry = expiry
		}
	}
	snapshot.BaseUsed = firstFloat(item.CurrentUsageWithPrecision, item.CurrentUsage)
	snapshot.BaseLimit = firstFloat(item.UsageLimitWithPrecision, item.UsageLimit)
	if trial := item.FreeTrialInfo; trial != nil && strings.EqualFold(trial.FreeTrialStatus, "ACTIVE") && (trial.FreeTrialExpiry == nil || *trial.FreeTrialExpiry > nowSeconds) {
		snapshot.TrialUsed = firstFloat(trial.CurrentUsageWithPrecision, trial.CurrentUsage)
		snapshot.TrialLimit = firstFloat(trial.UsageLimitWithPrecision, trial.UsageLimit)
		noteExpiry(firstFloat(trial.FreeTrialExpiry))
	}
	for _, bonus := range item.Bonuses {
		if !strings.EqualFold(bonus.Status, "ACTIVE") || bonus.ExpiresAt != nil && *bonus.ExpiresAt <= nowSeconds {
			continue
		}
		snapshot.BonusUsed += firstFloat(bonus.CurrentUsage)
		snapshot.BonusLimit += firstFloat(bonus.UsageLimit)
		noteExpiry(firstFloat(bonus.ExpiresAt))
	}
	snapshot.Used = snapshot.BaseUsed + snapshot.TrialUsed + snapshot.BonusUsed
	snapshot.Limit = snapshot.BaseLimit + snapshot.TrialLimit + snapshot.BonusLimit
	snapshot.CreditsExpireAt = kiroEpochRFC3339(earliestExpiry)
	return snapshot, nil
}

// KiroFetchUsage 查询账号额度（只读，不消耗 credit）。
func KiroFetchUsage(ctx context.Context, cred KiroCredential, bearer, proxyURL string) (*KiroUsageSnapshot, error) {
	raw, err := kiroManagementGET(ctx, cred, bearer, proxyURL, "/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true")
	if err != nil {
		return nil, fmt.Errorf("kiro 额度查询失败: %w", err)
	}
	return ParseKiroUsage(raw, time.Now())
}

// KiroFetchModels 拉取账号实际可用的模型 ID（auto 是上游路由别名，不作为可调度模型发布）。
func KiroFetchModels(ctx context.Context, cred KiroCredential, bearer, proxyURL string) ([]string, error) {
	var models []string
	seen := map[string]struct{}{}
	next := ""
	for page := 0; page < 10; page++ {
		path := "/ListAvailableModels?origin=AI_EDITOR"
		if next != "" {
			path += "&nextToken=" + url.QueryEscape(next)
		}
		raw, err := kiroManagementGET(ctx, cred, bearer, proxyURL, path)
		if err != nil {
			return nil, fmt.Errorf("kiro 模型目录查询失败: %w", err)
		}
		var parsed struct {
			Models []struct {
				ModelID string `json:"modelId"`
			} `json:"models"`
			NextToken string `json:"nextToken"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("kiro 模型目录无法解析: %w", err)
		}
		for _, model := range parsed.Models {
			id := strings.TrimSpace(model.ModelID)
			if id == "" || strings.EqualFold(id, "auto") {
				continue
			}
			if _, dup := seen[strings.ToLower(id)]; !dup {
				seen[strings.ToLower(id)] = struct{}{}
				models = append(models, id)
			}
		}
		if parsed.NextToken == "" {
			break
		}
		next = parsed.NextToken
	}
	return models, nil
}

// KiroResolveProfileARN 为 Enterprise / IdC 账号解析真实 profile ARN；Builder ID 没有 profile 时返回空。
func KiroResolveProfileARN(ctx context.Context, cred KiroCredential, bearer, proxyURL string) (string, error) {
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, region := range kiroRESTRegions(cred.AuthRegion()) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+KiroManagementHost(region)+"/", strings.NewReader(`{"maxResults":10}`))
		if err != nil {
			return "", err
		}
		kiroManagementHeaders(req, cred, bearer)
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("x-amz-target", "AmazonCodeWhispererService.ListAvailableProfiles")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed struct {
				Profiles []struct {
					Arn string `json:"arn"`
				} `json:"profiles"`
			}
			if json.Unmarshal(raw, &parsed) == nil {
				for _, profile := range parsed.Profiles {
					if strings.TrimSpace(profile.Arn) != "" {
						return strings.TrimSpace(profile.Arn), nil
					}
				}
			}
			continue
		}
		// Builder ID 账号固定返回 403 "not supported"：视为没有 Enterprise profile。
		if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "AWS Builder ID is not supported for this operation") {
			return "", nil
		}
		lastErr = fmt.Errorf("HTTP %d %s", resp.StatusCode, truncateKiroError(string(raw)))
	}
	if lastErr != nil {
		return "", fmt.Errorf("kiro profile 解析失败: %w", lastErr)
	}
	return "", nil
}

// ---- Social 登录（Github / Google）----

// KiroSocialSession 是一次门户登录的待完成状态。
type KiroSocialSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
}

func kiroBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// NewKiroSocialSession 生成 PKCE 与门户登录地址。
func NewKiroSocialSession() (KiroSocialSession, string) {
	verifierBytes := make([]byte, 32)
	_, _ = rand.Read(verifierBytes)
	stateBytes := make([]byte, 16)
	_, _ = rand.Read(stateBytes)
	session := KiroSocialSession{State: kiroBase64URL(stateBytes), CodeVerifier: kiroBase64URL(verifierBytes), RedirectURI: fmt.Sprintf("http://127.0.0.1:%d", KiroSocialCallbackPort)}
	challenge := sha256.Sum256([]byte(session.CodeVerifier))
	query := url.Values{"state": {session.State}, "code_challenge": {kiroBase64URL(challenge[:])}, "code_challenge_method": {"S256"}, "redirect_uri": {session.RedirectURI}, "redirect_from": {"KiroIDE"}}
	return session, KiroPortalURL + "/signin?" + query.Encode()
}

// KiroSocialCallback 是从浏览器回调地址里解析出的授权码。
type KiroSocialCallback struct {
	Code, State, LoginOption, Path string
}

// ParseKiroSocialCallback 解析用户粘贴回来的完整回调 URL。
func ParseKiroSocialCallback(raw string) (KiroSocialCallback, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return KiroSocialCallback{}, fmt.Errorf("回调地址无法解析: %w", err)
	}
	if parsed.Path != "/oauth/callback" && parsed.Path != "/signin/callback" {
		return KiroSocialCallback{}, fmt.Errorf("回调地址路径应为 /oauth/callback")
	}
	query := parsed.Query()
	if errText := query.Get("error"); errText != "" {
		return KiroSocialCallback{}, fmt.Errorf("门户返回错误: %s", errText)
	}
	if query.Get("code") == "" {
		return KiroSocialCallback{}, fmt.Errorf("回调地址缺少 code")
	}
	return KiroSocialCallback{Code: query.Get("code"), State: query.Get("state"), LoginOption: query.Get("login_option"), Path: parsed.Path}, nil
}

// KiroExchangeSocialCode 用授权码换取 token；redirect_uri 必须与门户回调完全一致。
func KiroExchangeSocialCode(ctx context.Context, session KiroSocialSession, callback KiroSocialCallback, proxyURL string) (*KiroTokenData, error) {
	redirect := session.RedirectURI + callback.Path
	if callback.LoginOption != "" {
		redirect += "?login_option=" + url.QueryEscape(callback.LoginOption)
	}
	body, _ := json.Marshal(map[string]string{"code": callback.Code, "code_verifier": session.CodeVerifier, "redirect_uri": redirect})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, KiroAuthEndpoint+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "KiroIDE-"+KiroIDEVersion())
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	return doKiroTokenRequest(client, req, "Social 登录")
}

// ---- IdC 设备码登录（Builder ID / Enterprise）----

// KiroDeviceSession 是一次设备码登录的待完成状态（含注册得到的 OIDC 客户端）。
type KiroDeviceSession struct {
	Region                  string `json:"region"`
	StartURL                string `json:"start_url"`
	ClientID                string `json:"-"`
	ClientSecret            string `json:"-"`
	DeviceCode              string `json:"-"`
	UserCode                string `json:"user_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresAt               time.Time
}

func kiroOIDCPost(ctx context.Context, client *http.Client, region, path string, payload any, out any) (int, []byte, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+region+".amazonaws.com"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, err
		}
	}
	return resp.StatusCode, raw, nil
}

// StartKiroDeviceLogin 注册 OIDC 客户端并发起设备授权。
func StartKiroDeviceLogin(ctx context.Context, region, startURL, proxyURL string) (*KiroDeviceSession, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = KiroDefaultRegion
	}
	startURL = strings.TrimSpace(startURL)
	if startURL == "" {
		startURL = KiroBuilderIDStart
	}
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	var registered struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	status, raw, err := kiroOIDCPost(ctx, client, region, "/client/register", map[string]any{
		"clientName": "codex2api", "clientType": "public", "scopes": kiroOIDCScopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"}, "issuerUrl": startURL,
	}, &registered)
	if err != nil || status/100 != 2 {
		return nil, fmt.Errorf("注册 OIDC 客户端失败: HTTP %d %s %v", status, truncateKiroError(string(raw)), err)
	}
	var device struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	status, raw, err = kiroOIDCPost(ctx, client, region, "/device_authorization", map[string]any{"clientId": registered.ClientID, "clientSecret": registered.ClientSecret, "startUrl": startURL}, &device)
	if err != nil || status/100 != 2 {
		return nil, fmt.Errorf("发起设备授权失败: HTTP %d %s %v", status, truncateKiroError(string(raw)), err)
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	return &KiroDeviceSession{Region: region, StartURL: startURL, ClientID: registered.ClientID, ClientSecret: registered.ClientSecret, DeviceCode: device.DeviceCode,
		UserCode: device.UserCode, VerificationURIComplete: device.VerificationURIComplete, Interval: device.Interval, ExpiresAt: time.Now().Add(time.Duration(max(device.ExpiresIn, 60)) * time.Second)}, nil
}

// ErrKiroDevicePending 表示用户尚未在浏览器里完成授权。
var ErrKiroDevicePending = fmt.Errorf("authorization_pending")

// PollKiroDeviceLogin 轮询一次设备授权结果。
func PollKiroDeviceLogin(ctx context.Context, session *KiroDeviceSession, proxyURL string) (*KiroTokenData, error) {
	if time.Now().After(session.ExpiresAt) {
		return nil, fmt.Errorf("设备授权已过期，请重新开始")
	}
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	var token kiroTokenResponse
	status, raw, err := kiroOIDCPost(ctx, client, session.Region, "/token", map[string]any{
		"clientId": session.ClientID, "clientSecret": session.ClientSecret, "grantType": "urn:ietf:params:oauth:grant-type:device_code", "deviceCode": session.DeviceCode,
	}, &token)
	if err != nil {
		return nil, err
	}
	if status/100 == 2 {
		return token.tokenData(), nil
	}
	var oidcErr struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &oidcErr)
	switch oidcErr.Error {
	case "authorization_pending", "slow_down":
		return nil, ErrKiroDevicePending
	case "expired_token":
		return nil, fmt.Errorf("设备授权已过期，请重新开始")
	case "access_denied":
		return nil, fmt.Errorf("用户拒绝了授权请求")
	}
	return nil, fmt.Errorf("设备授权失败: HTTP %d %s", status, truncateKiroError(string(raw)))
}
