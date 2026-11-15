package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// KiroTokenData 是一次 Kiro 刷新换出的令牌。
type KiroTokenData struct {
	AccessToken  string
	RefreshToken string
	ProfileARN   string
	ExpiresAt    time.Time
}

// kiroRefreshInvalidError 表示 refresh token 已永久失效（invalid_grant）。
type kiroRefreshInvalidError struct{ message string }

func (e *kiroRefreshInvalidError) Error() string                 { return e.message }
func (e *kiroRefreshInvalidError) PermanentRefreshFailure() bool { return true }

// KiroOSName / KiroNodeVersion 是 IDE 指纹里的运行时版本，与官方 IDE 默认一致。
const (
	KiroOSName                = "macos"
	KiroNodeVersion           = "22.22.0"
	kiroFallbackIDEVersion    = "2.3.0"
	kiroVersionMetadataURL    = "https://prod.download.desktop.kiro.dev/stable/metadata-linux-x64-stable.json"
	kiroVersionRefreshEvery   = 6 * time.Hour
	kiroRefreshRequestTimeout = 60 * time.Second
)

var kiroIDEVersion struct {
	sync.Mutex
	value     string
	fetchedAt time.Time
	inFlight  bool
}

// KiroIDEVersion 返回官方稳定版 IDE 版本号；缓存过期时在后台刷新，取不到时用兜底值。
func KiroIDEVersion() string {
	kiroIDEVersion.Lock()
	defer kiroIDEVersion.Unlock()
	if !kiroIDEVersion.inFlight && time.Since(kiroIDEVersion.fetchedAt) > kiroVersionRefreshEvery {
		kiroIDEVersion.inFlight = true
		go refreshKiroIDEVersion()
	}
	if kiroIDEVersion.value != "" {
		return kiroIDEVersion.value
	}
	return kiroFallbackIDEVersion
}

func refreshKiroIDEVersion() {
	version := ""
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, kiroVersionMetadataURL, nil); err == nil {
		if resp, err := BuildHTTPClient("").Do(req); err == nil {
			var meta struct {
				CurrentRelease string `json:"currentRelease"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta) == nil {
				version = strings.TrimSpace(meta.CurrentRelease)
			}
			resp.Body.Close()
		}
	}
	kiroIDEVersion.Lock()
	defer kiroIDEVersion.Unlock()
	kiroIDEVersion.inFlight = false
	kiroIDEVersion.fetchedAt = time.Now()
	if version != "" {
		kiroIDEVersion.value = version
	}
}

// kiroTokenRefreshFunc is swapped by tests.
var kiroTokenRefreshFunc = KiroRefreshTokens

// KiroRefreshTokens 按凭据形态刷新 access token。API Key 凭据没有刷新流程。
func KiroRefreshTokens(ctx context.Context, cred KiroCredential, refreshToken, proxyURL string) (*KiroTokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("kiro refresh_token 为空")
	}
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, kiroRefreshRequestTimeout)
	defer cancel()
	switch cred.AuthKind {
	case KiroAuthKindAPIKey:
		return nil, fmt.Errorf("kiro API Key 凭据无需刷新")
	case KiroAuthKindIdC:
		return refreshKiroIdC(ctx, client, cred, refreshToken)
	case KiroAuthKindExternalIdP:
		return refreshKiroExternalIdP(ctx, client, cred, refreshToken)
	default:
		return refreshKiroSocial(ctx, client, cred, refreshToken)
	}
}

type kiroTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ProfileARN   string `json:"profileArn"`
	ExpiresIn    int64  `json:"expiresIn"`
	// external_idp 走标准 OAuth2 字段。
	OAuthAccessToken  string `json:"access_token"`
	OAuthRefreshToken string `json:"refresh_token"`
	OAuthExpiresIn    int64  `json:"expires_in"`
}

func (r kiroTokenResponse) tokenData() *KiroTokenData {
	td := &KiroTokenData{AccessToken: firstNonEmpty(r.AccessToken, r.OAuthAccessToken), RefreshToken: firstNonEmpty(r.RefreshToken, r.OAuthRefreshToken), ProfileARN: strings.TrimSpace(r.ProfileARN)}
	expiresIn := r.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = r.OAuthExpiresIn
	}
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	td.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC()
	return td
}

func refreshKiroSocial(ctx context.Context, client *http.Client, cred KiroCredential, refreshToken string) (*KiroTokenData, error) {
	region := cred.AuthRegion()
	body, _ := json.Marshal(map[string]string{"refreshToken": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://prod."+region+".auth.desktop.kiro.dev/refreshToken", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "KiroIDE-"+KiroIDEVersion()+"-"+cred.MachineID)
	return doKiroTokenRequest(client, req, "Social")
}

func refreshKiroIdC(ctx context.Context, client *http.Client, cred KiroCredential, refreshToken string) (*KiroTokenData, error) {
	if cred.ClientID == "" || cred.ClientSecret == "" {
		return nil, fmt.Errorf("kiro IdC 刷新需要 clientId 与 clientSecret")
	}
	region := cred.AuthRegion()
	body, _ := json.Marshal(map[string]string{"clientId": cred.ClientID, "clientSecret": cred.ClientSecret, "refreshToken": refreshToken, "grantType": "refresh_token"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+region+".amazonaws.com/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/3.980.0 KiroIDE")
	req.Header.Set("User-Agent", "aws-sdk-js/3.980.0 ua/2.1 os/"+KiroOSName+" lang/js md/nodejs#"+KiroNodeVersion+" api/sso-oidc#3.980.0 m/E KiroIDE")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=4")
	return doKiroTokenRequest(client, req, "IdC")
}

func refreshKiroExternalIdP(ctx context.Context, client *http.Client, cred KiroCredential, refreshToken string) (*KiroTokenData, error) {
	if cred.ClientID == "" || cred.TokenEndpoint == "" {
		return nil, fmt.Errorf("kiro 企业 SSO 刷新需要 clientId 与 tokenEndpoint")
	}
	// 外发 refresh token 前再校验一次端点，防止带外改库把令牌送到任意主机。
	if err := ValidateKiroExternalIdPEndpoint(cred.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("kiro 企业 SSO tokenEndpoint 被拒绝: %w", err)
	}
	form := url.Values{"client_id": {cred.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	if cred.Scopes != "" {
		form.Set("scope", cred.Scopes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cred.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doKiroTokenRequest(client, req, "企业 SSO")
}

func doKiroTokenRequest(client *http.Client, req *http.Request, label string) (*KiroTokenData, error) {
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro %s 刷新请求失败: %w", label, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := string(raw)
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(text, "invalid_grant") {
			return nil, &kiroRefreshInvalidError{message: fmt.Sprintf("kiro %s refresh token 已失效 (invalid_grant): %s", label, truncateKiroError(text))}
		}
		return nil, fmt.Errorf("kiro %s 刷新失败: HTTP %d %s", label, resp.StatusCode, truncateKiroError(text))
	}
	var parsed kiroTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("kiro %s 刷新响应无法解析: %w", label, err)
	}
	td := parsed.tokenData()
	if td.AccessToken == "" {
		return nil, fmt.Errorf("kiro %s 刷新响应缺少 accessToken", label)
	}
	return td, nil
}

func truncateKiroError(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 300 {
		return text[:300] + "…"
	}
	return text
}

// RefreshKiroAccount 是上游 401 恢复与手动刷新共用的入口。
func (s *Store) RefreshKiroAccount(ctx context.Context, acc *Account) error {
	if s == nil || acc == nil || !acc.KiroRefreshable() {
		return fmt.Errorf("account is not a refreshable Kiro account")
	}
	return s.refreshKiroAccount(ctx, acc, true)
}

// refreshKiroAccount 在跨实例共享的 OAuth 刷新租约下换票：拿到锁后先重读库，
// 别的实例已刷新且可用就直接复用，避免第二次刷新消耗刚轮换出的新 RT。
func (s *Store) refreshKiroAccount(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
	cred := acc.Kiro
	rt := strings.TrimSpace(acc.RefreshToken)
	lockedAccessToken := acc.AccessToken
	dbID := acc.DBID
	proxyURL := strings.TrimSpace(acc.ProxyURL)
	cooldownActive := acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)
	acc.mu.RUnlock()

	if cred.AuthKind == KiroAuthKindAPIKey {
		return nil
	}
	if rt == "" {
		return fmt.Errorf("kiro refresh_token 为空")
	}
	lease, err := s.acquireOAuthRefreshLease(ctx, rt)
	if err != nil {
		return err
	}
	defer lease.Release()
	ctx = lease.Context()
	if changed, usable, reloadErr := s.reloadOAuthCredentialsAfterLock(ctx, acc, rt, lockedAccessToken); reloadErr == nil && changed && usable && !forceRefresh {
		s.finishReloadedOAuthRefresh(ctx, acc)
		return nil
	} else if reloadErr == nil && changed {
		acc.mu.RLock()
		rt = strings.TrimSpace(acc.RefreshToken)
		acc.mu.RUnlock()
	}

	td, err := kiroTokenRefreshFunc(ctx, cred, rt, proxyURL)
	if err != nil {
		wrapped := fmt.Errorf("kiro token 刷新失败: %w", err)
		if IsPermanentRefreshFailure(err) {
			s.markPermanentRefreshFailure(acc, wrapped)
		}
		return wrapped
	}
	updates := map[string]interface{}{
		"access_token": td.AccessToken,
		"expires_at":   td.ExpiresAt.Format(time.RFC3339),
	}
	if td.RefreshToken != "" {
		updates["refresh_token"] = td.RefreshToken
	}
	if td.ProfileARN != "" {
		updates[KiroProfileARNCredentialKey] = td.ProfileARN
	}
	if s.db != nil {
		if err := s.db.UpdateCredentials(ctx, dbID, updates); err != nil {
			return fmt.Errorf("kiro 刷新结果落库失败: %w", err)
		}
	}

	acc.mu.Lock()
	acc.AccessToken = td.AccessToken
	if td.RefreshToken != "" {
		acc.RefreshToken = td.RefreshToken
	}
	if td.ProfileARN != "" {
		acc.Kiro.ProfileARN = td.ProfileARN
	}
	acc.ExpiresAt = td.ExpiresAt
	acc.PermanentRefreshFailures = 0
	// 限流冷却保留；过期 AT 造成的 unauthorized 冷却在新令牌下不再成立。
	preserveCooldown := cooldownActive && !strings.EqualFold(strings.TrimSpace(acc.CooldownReason), "unauthorized")
	if !preserveCooldown {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
		acc.ErrorMsg = ""
	}
	if acc.Status != StatusError {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	if !preserveCooldown && s.db != nil {
		_ = s.db.ClearError(ctx, dbID)
		s.deleteCachedAccountCooldown(dbID)
	}
	return nil
}
