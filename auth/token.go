package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/config"
)

// OpenAI OAuth 常量（与 CLIProxyAPI / sub2api 一致）
const (
	TokenURL      = "https://auth.openai.com/oauth/token"
	ClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	RefreshScopes = "openid profile email"
	MaxRetries    = 3
)

// TokenData 保存一次 RT 刷新获得的 token 信息
type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    time.Time
}

// AccountInfo 解析 id_token 获得的账号信息
type AccountInfo struct {
	Email            string `json:"email"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	PlanType         string `json:"chatgpt_plan_type"`
}

// RefreshAccessToken 用 RT 换取 AT
func RefreshAccessToken(ctx context.Context, refreshToken string, proxyURL string) (*TokenData, *AccountInfo, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
		"scope":         {RefreshScopes},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := buildHTTPClient(proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("刷新请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("刷新失败 (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	td := &TokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    tokenResp.ExpiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	// 保留新 RT，如果返回空则保留旧的
	if strings.TrimSpace(td.RefreshToken) == "" {
		td.RefreshToken = refreshToken
	}

	// 解析 id_token 获取账号信息
	info := parseIDToken(tokenResp.IDToken)

	return td, info, nil
}

// RefreshWithRetry 带重试的 RT 刷新
func RefreshWithRetry(ctx context.Context, refreshToken string, proxyURL string) (*TokenData, *AccountInfo, error) {
	var lastErr error
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		td, info, err := RefreshAccessToken(ctx, refreshToken, proxyURL)
		if err == nil {
			return td, info, nil
		}

		// 不可重试错误直接返回
		if isNonRetryable(err) {
			return nil, nil, err
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("刷新失败（重试 %d 次）: %w", MaxRetries, lastErr)
}

// isNonRetryable 判断是否不可重试的认证错误
func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"invalid_grant", "invalid_client", "unauthorized_client", "access_denied"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// parseIDToken 解析 JWT id_token 的 payload（不验签）
func parseIDToken(idToken string) *AccountInfo {
	if idToken == "" {
		return &AccountInfo{}
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return &AccountInfo{}
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
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return &AccountInfo{}
		}
	}

	var claims struct {
		Email      string `json:"email"`
		OpenAIAuth *struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			PlanType         string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return &AccountInfo{}
	}

	info := &AccountInfo{Email: claims.Email}
	if claims.OpenAIAuth != nil {
		info.ChatGPTAccountID = claims.OpenAIAuth.ChatGPTAccountID
		info.PlanType = claims.OpenAIAuth.PlanType
	}
	return info
}

// authClientPool 认证请求的连接池（按 proxyURL 分组）
var authClientPool sync.Map

// buildHTTPClient 构建支持代理的 HTTP 客户端（连接池复用）
func buildHTTPClient(proxyURL string) *http.Client {
	if v, ok := authClientPool.Load(proxyURL); ok {
		return v.(*http.Client)
	}

	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	if v, loaded := authClientPool.LoadOrStore(proxyURL, client); loaded {
		return v.(*http.Client)
	}
	return client
}

// BuildHTTPClientFromConfig 从账号配置构建 HTTP 客户端
func BuildHTTPClientFromConfig(account *config.AccountConfig, globalProxy string) *http.Client {
	proxy := account.ProxyURL
	if proxy == "" {
		proxy = globalProxy
	}
	return buildHTTPClient(proxy)
}

// HashAccountID 从 account_id 生成短哈希（用于日志）
func HashAccountID(accountID string) string {
	if accountID == "" {
		return ""
	}
	h := sha256.Sum256([]byte(accountID))
	return fmt.Sprintf("%x", h[:4])
}
