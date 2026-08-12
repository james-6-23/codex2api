package admin

import (
	"context"
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

// ==================== Claude Code OAuth 会话 ====================
//
// Anthropic 只给 Claude Code 注册了托管回调页（platform.claude.com/oauth/code/callback），
// 既没有本机回调，也没有 device code 流程。因此管理台的路径是：
// 生成授权 URL → 用户在浏览器完成授权 → 把回调页显示的 code 粘回来兑换。
// code_verifier 必须在这两步之间留存，故有本会话表。

const claudeOAuthSessionTTL = 15 * time.Minute

type claudeOAuthSession struct {
	Verifier  string
	State     string
	ProxyURL  string
	Name      string
	BaseURL   string
	Models    []string
	CreatedAt time.Time
}

type claudeOAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*claudeOAuthSession
}

var globalClaudeOAuthStore = &claudeOAuthSessionStore{sessions: make(map[string]*claudeOAuthSession)}

func init() {
	go globalClaudeOAuthStore.cleanupLoop()
}

func (s *claudeOAuthSessionStore) set(id string, sess *claudeOAuthSession) {
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
}

func (s *claudeOAuthSessionStore) get(id string) (*claudeOAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Since(sess.CreatedAt) > claudeOAuthSessionTTL {
		return nil, false
	}
	return sess, true
}

func (s *claudeOAuthSessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *claudeOAuthSessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.CreatedAt) > claudeOAuthSessionTTL {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// GenerateClaudeAuthURL 生成 Claude Code PKCE 授权 URL
// POST /api/admin/accounts/claude/oauth/auth-url
func (h *Handler) GenerateClaudeAuthURL(c *gin.Context) {
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
	baseURL, err := auth.NormalizeClaudeBaseURL(req.BaseURL)
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

	pkce, err := auth.GenerateClaudePKCE()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 PKCE 失败: "+err.Error())
		return
	}
	authURL, err := auth.BuildClaudeAuthorizationURL(auth.ClaudeAuthURLParams{
		Challenge: pkce.Challenge,
		State:     pkce.State,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成授权链接失败: "+err.Error())
		return
	}

	sessionID, err := grokOAuthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 session_id 失败")
		return
	}
	globalClaudeOAuthStore.set(sessionID, &claudeOAuthSession{
		Verifier:  pkce.Verifier,
		State:     pkce.State,
		ProxyURL:  strings.TrimSpace(req.ProxyURL),
		Name:      strings.TrimSpace(req.Name),
		BaseURL:   baseURL,
		Models:    models,
		CreatedAt: time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{
		"session_id": sessionID,
		"auth_url":   authURL,
		"state":      pkce.State,
		"expires_in": int(claudeOAuthSessionTTL.Seconds()),
	})
}

// ExchangeClaudeOAuthCode 用授权码兑换 token 并建号
// POST /api/admin/accounts/claude/oauth/exchange-code
func (h *Handler) ExchangeClaudeOAuthCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		ProxyURL  string `json:"proxy_url"`
		Name      string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		writeError(c, http.StatusBadRequest, "session_id 是必填字段")
		return
	}
	sess, ok := globalClaudeOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "授权会话不存在或已过期，请重新发起")
		return
	}
	parsed := auth.ParseClaudeAuthorizationInput(req.Code)
	if parsed.Code == "" {
		writeError(c, http.StatusBadRequest, "授权码为空：请粘贴回调页显示的 code（或完整回调 URL）")
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

	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	token, err := auth.ExchangeClaudeAuthorizationCode(ctx, auth.ClaudeExchangeParams{
		Code:     parsed.Code,
		State:    claudeFirstNonEmpty(parsed.State, sess.State),
		Verifier: sess.Verifier,
		ProxyURL: proxyURL,
	})
	if err != nil {
		writeError(c, http.StatusBadGateway, "授权码兑换失败: "+err.Error())
		return
	}
	globalClaudeOAuthStore.delete(req.SessionID)

	id, email, err := h.createClaudeOAuthAccount(ctx, createClaudeOAuthAccountInput{
		Name:     name,
		ProxyURL: proxyURL,
		BaseURL:  sess.BaseURL,
		Models:   sess.Models,
		Token:    token,
		Source:   "oauth_claude",
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "authorized",
		"message": "Claude 授权成功",
		"id":      id,
		"email":   email,
	})
}

func claudeFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

type createClaudeOAuthAccountInput struct {
	Name     string
	ProxyURL string
	BaseURL  string
	Models   []string
	Token    *auth.ClaudeTokenData
	Source   string
	// 凭据文件导入时带上文件里的身份信息：bootstrap 探针失败（网络 / 代理 / scope 不足）
	// 时用它兜底，否则列表里只剩一个没有邮箱、没有套餐的空壳账号。
	FallbackEmail            string
	FallbackPlanType         string
	FallbackAccountUUID      string
	FallbackOrganizationUUID string
}

func (h *Handler) createClaudeOAuthAccount(ctx context.Context, in createClaudeOAuthAccountInput) (int64, string, error) {
	if in.Token == nil {
		return 0, "", fmt.Errorf("token 为空")
	}
	clientID := auth.EffectiveClaudeOAuthClientID()
	cliUserID, err := auth.NewClaudeCLIUserID()
	if err != nil {
		return 0, "", fmt.Errorf("生成 cli_user_id 失败: %w", err)
	}

	// 身份探针失败不阻断建号：access_token 已经可用，邮箱/组织只是展示与调度提示。
	bootstrap, bootstrapErr := auth.FetchClaudeBootstrap(ctx, in.Token.AccessToken, in.ProxyURL)
	if bootstrapErr != nil {
		bootstrap = &auth.ClaudeBootstrap{}
	}
	if bootstrap.AccountEmail == "" {
		bootstrap.AccountEmail = strings.TrimSpace(in.FallbackEmail)
	}
	if bootstrap.AccountUUID == "" {
		bootstrap.AccountUUID = strings.TrimSpace(in.FallbackAccountUUID)
	}
	if bootstrap.OrganizationUUID == "" {
		bootstrap.OrganizationUUID = strings.TrimSpace(in.FallbackOrganizationUUID)
	}

	credentials := map[string]interface{}{
		"upstream_type":    auth.UpstreamClaude,
		"access_token":     in.Token.AccessToken,
		"refresh_token":    in.Token.RefreshToken,
		"expires_at":       in.Token.ExpiresAt.Format(time.RFC3339),
		"claude_client_id": clientID,
		"claude_token_url": auth.ClaudeDefaultTokenURL,
		"cli_user_id":      cliUserID,
	}
	if in.Token.Scope != "" {
		credentials["scope"] = in.Token.Scope
	}
	if bootstrap.AccountUUID != "" {
		credentials["account_uuid"] = bootstrap.AccountUUID
		credentials["account_id"] = bootstrap.AccountUUID
	}
	if bootstrap.AccountEmail != "" {
		credentials["email"] = bootstrap.AccountEmail
	}
	if bootstrap.OrganizationUUID != "" {
		credentials["organization_uuid"] = bootstrap.OrganizationUUID
	}
	if bootstrap.OrganizationName != "" {
		credentials["organization_name"] = bootstrap.OrganizationName
	}
	if bootstrap.OrganizationType != "" {
		credentials["organization_type"] = bootstrap.OrganizationType
	}
	planType := ""
	if bootstrap.OrganizationRateLimitTier != "" {
		credentials["organization_rate_limit_tier"] = bootstrap.OrganizationRateLimitTier
		if planType = auth.ClaudePlanFromRateLimitTier(bootstrap.OrganizationRateLimitTier); planType != "" {
			credentials["plan_type"] = planType
		}
	}
	// 探针没给出档位时用凭据文件里的 subscriptionType 兜底（能识别的才写）。
	if planType == "" {
		if fallback := strings.TrimSpace(in.FallbackPlanType); fallback != "" {
			planType = fallback
			credentials["plan_type"] = fallback
		}
	}
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL != "" {
		credentials["base_url"] = baseURL
	}
	if len(in.Models) > 0 {
		credentials["models"] = in.Models
	}

	email := bootstrap.AccountEmail
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = email
	}
	if name == "" {
		name = "claude-oauth"
	}

	id, err := h.db.InsertAccountWithUpstream(ctx, name, "anthropic", auth.UpstreamClaude, credentials, in.ProxyURL)
	if err != nil {
		return 0, "", err
	}
	source := in.Source
	if source == "" {
		source = "oauth_claude"
	}
	h.db.InsertAccountEventAsync(id, "added", source)

	acc := &auth.Account{
		DBID:                   id,
		ProxyURL:               in.ProxyURL,
		HealthTier:             auth.HealthTierHealthy,
		UpstreamType:           auth.UpstreamClaude,
		BaseURL:                baseURL,
		Models:                 in.Models,
		Email:                  email,
		PlanType:               planType,
		AccountID:              bootstrap.AccountUUID,
		AccessToken:            in.Token.AccessToken,
		RefreshToken:           in.Token.RefreshToken,
		ExpiresAt:              in.Token.ExpiresAt,
		ClaudeClientID:         clientID,
		ClaudeTokenURL:         auth.ClaudeDefaultTokenURL,
		ClaudeCLIUserID:        cliUserID,
		ClaudeAccountUUID:      bootstrap.AccountUUID,
		ClaudeOrganizationUUID: bootstrap.OrganizationUUID,
		ClaudeOrganizationName: bootstrap.OrganizationName,
		ClaudeRateLimitTier:    bootstrap.OrganizationRateLimitTier,
	}
	h.store.AddAccount(acc)

	security.SecurityAuditLog("CLAUDE_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d source=%s bootstrap_ok=%t", id, source, bootstrapErr == nil))
	return id, email, nil
}
