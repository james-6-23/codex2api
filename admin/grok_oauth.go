package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// ==================== Grok Device OAuth 会话 ====================
//
// xAI / Grok CLI 正式授权路径是 RFC 8628 Device Code。
// 浏览器 redirect PKCE 会落到「复制代码到 Grok Build」页，token 受众不对时
// 会被用量探针误封，因此管理台只暴露 device start/poll。

const grokDeviceSessionTTL = 30 * time.Minute

type grokDeviceSession struct {
	DeviceCode    string
	UserCode      string
	TokenEndpoint string
	ProxyURL      string
	Name          string
	BaseURL       string
	Models        []string
	Interval      int
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

type grokDeviceSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*grokDeviceSession
}

var globalGrokDeviceStore = &grokDeviceSessionStore{sessions: make(map[string]*grokDeviceSession)}

func init() {
	go globalGrokDeviceStore.cleanupLoop()
}

func (s *grokDeviceSessionStore) set(id string, sess *grokDeviceSession) {
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
}

func (s *grokDeviceSessionStore) get(id string) (*grokDeviceSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Since(sess.CreatedAt) > grokDeviceSessionTTL {
		return nil, false
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

func (s *grokDeviceSessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *grokDeviceSessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.CreatedAt) > grokDeviceSessionTTL ||
				(!sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt)) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func grokOAuthRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartGrokDeviceAuth 启动 xAI Device Code 授权
// POST /api/admin/accounts/grok/oauth/device/start
func (h *Handler) StartGrokDeviceAuth(c *gin.Context) {
	var req struct {
		ProxyURL string   `json:"proxy_url"`
		Name     string   `json:"name"`
		BaseURL  string   `json:"base_url"`
		Models   []string `json:"models"`
	}
	_ = c.ShouldBindJSON(&req)
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}
	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	device, err := auth.StartGrokDeviceFlow(ctx, req.ProxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, "启动 Device 授权失败: "+err.Error())
		return
	}

	sessionID, err := grokOAuthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 session_id 失败")
		return
	}
	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresAt := time.Now().Add(grokDeviceSessionTTL)
	if device.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	}
	globalGrokDeviceStore.set(sessionID, &grokDeviceSession{
		DeviceCode:    device.DeviceCode,
		UserCode:      device.UserCode,
		TokenEndpoint: device.TokenEndpoint,
		ProxyURL:      strings.TrimSpace(req.ProxyURL),
		Name:          strings.TrimSpace(req.Name),
		BaseURL:       baseURL,
		Models:        models,
		Interval:      interval,
		ExpiresAt:     expiresAt,
		CreatedAt:     time.Now(),
	})

	verificationURL := strings.TrimSpace(device.VerificationURIComplete)
	if verificationURL == "" {
		verificationURL = strings.TrimSpace(device.VerificationURI)
	}

	c.JSON(http.StatusOK, gin.H{
		"session_id":                sessionID,
		"user_code":                 device.UserCode,
		"verification_uri":          device.VerificationURI,
		"verification_uri_complete": device.VerificationURIComplete,
		"verification_url":          verificationURL,
		"expires_in":                device.ExpiresIn,
		"interval":                  interval,
	})
}

// PollGrokDeviceAuth 轮询 Device 授权；成功则入库
// POST /api/admin/accounts/grok/oauth/device/poll
func (h *Handler) PollGrokDeviceAuth(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		ProxyURL  string `json:"proxy_url"`
		Name      string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		writeError(c, http.StatusBadRequest, "session_id 是必填字段")
		return
	}
	sess, ok := globalGrokDeviceStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "Device 授权会话不存在或已过期，请重新发起")
		return
	}

	proxyURL := sess.ProxyURL
	if trimmed := strings.TrimSpace(security.SanitizeInput(req.ProxyURL)); trimmed != "" {
		if err := security.ValidateProxyURL(trimmed); err != nil {
			writeError(c, http.StatusBadRequest, "代理URL无效")
			return
		}
		proxyURL = trimmed
	}
	name := sess.Name
	if trimmed := strings.TrimSpace(security.SanitizeInput(req.Name)); trimmed != "" {
		name = trimmed
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result, err := auth.PollGrokDeviceToken(ctx, sess.DeviceCode, sess.TokenEndpoint, proxyURL)
	if err != nil {
		errText := err.Error()
		if strings.Contains(errText, "过期") || strings.Contains(errText, "拒绝") {
			globalGrokDeviceStore.delete(req.SessionID)
		}
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	if result.Pending {
		c.JSON(http.StatusOK, gin.H{
			"status":     "pending",
			"slow_down":  result.SlowDown,
			"interval":   sess.Interval,
			"user_code":  sess.UserCode,
			"expires_at": sess.ExpiresAt.Format(time.RFC3339),
		})
		return
	}
	if result.Token == nil {
		writeError(c, http.StatusBadGateway, "device 授权未返回 token")
		return
	}

	globalGrokDeviceStore.delete(req.SessionID)
	res, err := h.createGrokOAuthAccount(ctx, createGrokOAuthAccountInput{
		Name:          name,
		ProxyURL:      proxyURL,
		BaseURL:       sess.BaseURL,
		Models:        sess.Models,
		Token:         result.Token,
		Subject:       result.Subject,
		Email:         result.Email,
		TokenEndpoint: result.TokenEndpoint,
		Source:        "oauth_device",
		// 交互式重授权命中既有账号(含回收站)时更新凭据而非报"已存在"。
		ReauthorizeExisting: true,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}

	h.triggerGrokUsageProbe(res.ID)

	message := "Grok Device 授权成功"
	switch {
	case res.Revived:
		message = fmt.Sprintf("Grok Device 授权成功，已复活回收站中的账号 %d 并更新凭据", res.ID)
	case res.Updated:
		message = fmt.Sprintf("Grok Device 授权成功，已更新既有账号 %d 的凭据", res.ID)
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "authorized",
		"message": message,
		"id":      res.ID,
		"email":   res.Email,
		"updated": res.Updated,
		"revived": res.Revived,
	})
}

// GenerateGrokAuthURL 旧 PKCE 接口：引导改用 device flow。
func (h *Handler) GenerateGrokAuthURL(c *gin.Context) {
	writeError(c, http.StatusBadRequest, "请使用 Device Code 授权：POST /accounts/grok/oauth/device/start")
}

// ExchangeGrokOAuthCode 旧 code 兑换接口：引导改用 device flow。
func (h *Handler) ExchangeGrokOAuthCode(c *gin.Context) {
	writeError(c, http.StatusBadRequest, "请使用 Device Code 授权：POST /accounts/grok/oauth/device/poll")
}

type createGrokOAuthAccountInput struct {
	Name          string
	ProxyURL      string
	BaseURL       string
	Models        []string
	Token         *auth.GrokTokenData
	Subject       string
	Email         string
	TokenEndpoint string
	Source        string
	// ReauthorizeExisting 开启"重授权即更新"语义:凭据身份命中既有账号时,
	// 更新该账号的凭据(回收站账号顺带复活),而不是报"已存在"。
	// 关闭时命中既有身份直接报错;身份栅栏含回收站账号,报错会让"删过的号
	// 重新导入"走进死胡同,所以交互式授权与批量导入路径都应打开。
	ReauthorizeExisting bool
}

type createGrokOAuthAccountResult struct {
	ID    int64
	Email string
	// Updated 表示命中既有账号并已更新凭据(而非新建)。
	Updated bool
	// Revived 表示既有账号原先在回收站,本次已复活。
	Revived bool
}

func (h *Handler) createGrokOAuthAccount(ctx context.Context, in createGrokOAuthAccountInput) (createGrokOAuthAccountResult, error) {
	if in.Token == nil {
		return createGrokOAuthAccountResult{}, fmt.Errorf("token 为空")
	}
	clientID := auth.EffectiveGrokOAuthClientID()
	subject := strings.TrimSpace(in.Subject)
	if subject == "" {
		subject = auth.GrokSubjectFromAccessToken(in.Token.AccessToken)
	}
	email := strings.TrimSpace(in.Email)
	if email == "" {
		email = subject
	}
	planType := strings.TrimSpace(in.Token.PlanType)
	if planType == "" {
		planType = auth.GrokPlanTypeFromAccessToken(in.Token.AccessToken)
	}

	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		// access_token 的 tier claim 是套餐权威来源；缺失/无效 tier 保持空白。
		"refresh_token":       in.Token.RefreshToken,
		"access_token":        in.Token.AccessToken,
		"expires_at":          in.Token.ExpiresAt.Format(time.RFC3339),
		"grok_client_id":      clientID,
		"grok_oidc_issuer":    auth.GrokDefaultOIDCIssuer,
		"grok_token_endpoint": strings.TrimSpace(in.TokenEndpoint),
	}
	if planType != "" {
		credentials["plan_type"] = planType
	}
	if in.Token.IDToken != "" {
		credentials["id_token"] = in.Token.IDToken
	}
	// OAuth 默认走 chat-proxy（与官方 CLI 聊天路径一致）
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		baseURL = auth.GrokDefaultChatProxyBaseURL
	}
	credentials["base_url"] = baseURL
	if len(in.Models) > 0 {
		credentials["models"] = in.Models
	}
	if subject != "" {
		credentials["account_id"] = subject
		credentials["email"] = email
	}

	name := strings.TrimSpace(in.Name)
	if name == "" && email != "" {
		name = email
	}
	if name == "" {
		name = "grok-oauth"
	}

	id, duplicateID, err := h.db.InsertGrokAccountIfAbsent(ctx, name, credentials, in.ProxyURL, true)
	if err != nil {
		return createGrokOAuthAccountResult{}, err
	}
	if duplicateID > 0 {
		if !in.ReauthorizeExisting {
			return createGrokOAuthAccountResult{}, fmt.Errorf("Grok 凭据身份已存在（账号 ID %d）", duplicateID)
		}
		return h.reauthorizeGrokOAuthAccount(ctx, duplicateID, in, credentials, email)
	}
	source := in.Source
	if source == "" {
		source = "oauth_grok"
	}
	h.db.InsertAccountEventAsync(id, "added", source)

	acc := &auth.Account{
		DBID:              id,
		ProxyURL:          in.ProxyURL,
		HealthTier:        auth.HealthTierHealthy,
		UpstreamType:      auth.UpstreamGrok,
		BaseURL:           baseURL,
		Models:            in.Models,
		Email:             email,
		PlanType:          planType,
		GrokClientID:      clientID,
		GrokOIDCIssuer:    auth.GrokDefaultOIDCIssuer,
		GrokTokenEndpoint: strings.TrimSpace(in.TokenEndpoint),
		AccountID:         subject,
		AccessToken:       in.Token.AccessToken,
		RefreshToken:      in.Token.RefreshToken,
		ExpiresAt:         in.Token.ExpiresAt,
	}
	h.store.AddAccount(acc)

	security.SecurityAuditLog("GROK_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d auth_kind=oauth source=%s", id, source))
	return createGrokOAuthAccountResult{ID: id, Email: email}, nil
}

// reauthorizeGrokOAuthAccount 把同一 OAuth 身份的新凭据合并进占位账号:
// 回收站账号复活,正常账号原地更新 token(对齐 Codex OAuth 重授权语义)。
// base_url/models/name/proxy 只有本次授权显式提供时才覆盖既有配置。
func (h *Handler) reauthorizeGrokOAuthAccount(ctx context.Context, accountID int64, in createGrokOAuthAccountInput, credentials map[string]interface{}, email string) (createGrokOAuthAccountResult, error) {
	overlay := make(map[string]interface{}, len(credentials))
	for key, value := range credentials {
		overlay[key] = value
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		delete(overlay, "base_url")
	}
	if len(in.Models) == 0 {
		delete(overlay, "models")
	}
	// token 不带 email claim 时 credentials["email"] 只是 subject 兜底,
	// 不能拿它覆盖既有账号的真实 email。
	if strings.TrimSpace(in.Email) == "" {
		delete(overlay, "email")
	}

	reauth, err := h.db.ReauthGrokAccount(ctx, accountID, overlay, strings.TrimSpace(in.Name), strings.TrimSpace(in.ProxyURL))
	if err != nil {
		return createGrokOAuthAccountResult{}, fmt.Errorf("重授权更新账号 %d 失败: %w", accountID, err)
	}

	// 无论账号原先是否在内存池(回收站账号不在),都按"移除后重载"拿到最新凭据。
	h.store.RemoveAccount(accountID)
	if loadErr := h.store.LoadAccountByID(ctx, accountID); loadErr != nil {
		return createGrokOAuthAccountResult{}, fmt.Errorf("重授权后重载账号 %d 失败: %w", accountID, loadErr)
	}

	source := in.Source
	if source == "" {
		source = "oauth_grok"
	}
	event := "updated"
	if reauth.Revived {
		event = "restored"
	}
	h.db.InsertAccountEventAsync(accountID, event, source)
	security.SecurityAuditLog("GROK_ACCOUNT_REAUTHORIZED", fmt.Sprintf("account_id=%d revived=%t source=%s", accountID, reauth.Revived, source))
	return createGrokOAuthAccountResult{ID: accountID, Email: email, Updated: true, Revived: reauth.Revived}, nil
}
