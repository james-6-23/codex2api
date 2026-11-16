package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Kiro 账号管理：导入（kiro.rs credentials.json / Kiro IDE kiro-auth-token.json /
// Kiro Account Manager 导出）、导出、Social 与设备码登录、额度与模型同步。

func (h *Handler) registerKiroRoutes(api *gin.RouterGroup) {
	api.POST("/accounts/kiro/import", h.ImportKiroAccounts)
	api.GET("/accounts/kiro/export", h.ExportKiroAccounts)
	api.POST("/accounts/kiro/social/auth-url", h.StartKiroSocialLogin)
	api.POST("/accounts/kiro/social/exchange", h.FinishKiroSocialLogin)
	api.POST("/accounts/kiro/device/start", h.StartKiroDeviceLogin)
	api.POST("/accounts/kiro/device/poll", h.PollKiroDeviceLogin)
	api.POST("/accounts/:id/kiro/sync", h.SyncKiroAccount)
	api.POST("/accounts/kiro/sync", h.SyncAllKiroAccounts)
}

func kiroAuthKindForRow(row *database.AccountRow, isKiro bool) string {
	if !isKiro {
		return ""
	}
	return auth.NormalizeKiroAuthKind(row.GetCredential(auth.KiroAuthKindCredentialKey), "", row.GetCredential("api_key") != "", false)
}

func kiroRowString(row *database.AccountRow, key string, isKiro bool) string {
	if !isKiro {
		return ""
	}
	return strings.TrimSpace(row.GetCredential(key))
}

// kiroUsageCredential 把额度快照存成 JSON 字符串：凭据读取（GetCredential）只认字符串值。
func kiroUsageCredential(usage *auth.KiroUsageSnapshot) string {
	encoded, _ := json.Marshal(usage)
	return string(encoded)
}

func kiroUsageForRow(row *database.AccountRow, isKiro bool) json.RawMessage {
	raw := kiroRowString(row, auth.KiroUsageCredentialKey, isKiro)
	if raw == "" || !json.Valid([]byte(raw)) {
		return nil
	}
	return json.RawMessage(raw)
}

// kiroImportCredential 是三种来源格式归一后的单个账号。
type kiroImportCredential struct {
	Email         string
	AccessToken   string
	RefreshToken  string
	ExpiresAt     time.Time
	APIKey        string
	Credential    auth.KiroCredential
	RawAuthMethod string
}

func kiroMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// kiroParseExpiry 接受 RFC3339 字符串或毫秒 / 秒时间戳。
func kiroParseExpiry(value any) time.Time {
	switch typed := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(typed)); err == nil {
			return parsed
		}
		if number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return kiroParseExpiry(number)
		}
	case float64:
		if typed > 1e12 {
			return time.UnixMilli(int64(typed))
		}
		if typed > 0 {
			return time.Unix(int64(typed), 0)
		}
	}
	return time.Time{}
}

// parseKiroImport 自动识别导入内容的格式并归一。
func parseKiroImport(content any) ([]kiroImportCredential, error) {
	if text, ok := content.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &decoded); err != nil {
			return nil, fmt.Errorf("导入内容不是合法 JSON: %w", err)
		}
		content = decoded
	}
	var entries []any
	switch typed := content.(type) {
	case map[string]any:
		if accounts, ok := typed["accounts"].([]any); ok {
			entries = accounts
		} else {
			entries = []any{typed}
		}
	case []any:
		entries = typed
	default:
		return nil, fmt.Errorf("无法识别的导入格式")
	}
	out := make([]kiroImportCredential, 0, len(entries))
	for index, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("第 %d 项不是对象", index+1)
		}
		cred := entry
		if nested, ok := entry["credentials"].(map[string]any); ok {
			cred = nested
		}
		item := kiroImportCredential{
			Email:         kiroMapString(entry, "email"),
			AccessToken:   kiroMapString(cred, "accessToken", "access_token"),
			RefreshToken:  kiroMapString(cred, "refreshToken", "refresh_token"),
			APIKey:        kiroMapString(cred, "kiroApiKey", "apiKey", "api_key"),
			ExpiresAt:     kiroParseExpiry(cred["expiresAt"]),
			RawAuthMethod: kiroMapString(cred, "authMethod", "auth_method"),
		}
		if item.APIKey == "" {
			item.APIKey = kiroMapString(entry, "kiroApiKey")
		}
		if item.Email == "" {
			item.Email = kiroMapString(cred, "email")
		}
		item.Credential = auth.KiroCredential{
			LoginSource:   firstKiroValue(kiroMapString(cred, "provider"), kiroMapString(entry, "idp", "provider")),
			Region:        firstKiroValue(kiroMapString(cred, "authRegion", "region"), kiroMapString(entry, "region")),
			APIRegion:     kiroMapString(cred, "apiRegion"),
			ClientID:      kiroMapString(cred, "clientId", "client_id"),
			ClientSecret:  kiroMapString(cred, "clientSecret", "client_secret"),
			StartURL:      kiroMapString(cred, "startUrl"),
			TokenEndpoint: kiroMapString(cred, "tokenEndpoint"),
			IssuerURL:     kiroMapString(cred, "issuerUrl"),
			Scopes:        kiroMapString(cred, "scopes"),
			ProfileARN:    firstKiroValue(kiroMapString(cred, "profileArn"), kiroMapString(entry, "profileArn")),
			MachineID:     firstKiroValue(kiroMapString(entry, "machineId"), kiroMapString(cred, "machineId")),
			Endpoint:      kiroMapString(cred, "endpoint"),
		}
		if item.RefreshToken == "" && item.APIKey == "" {
			return nil, fmt.Errorf("第 %d 项缺少 refreshToken 或 kiroApiKey", index+1)
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可导入的账号")
	}
	return out, nil
}

func firstKiroValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type kiroCreateResult struct {
	ID       int64    `json:"id"`
	Email    string   `json:"email,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// createKiroAccount 校验、换票、补全身份信息后入库，并装载进号池。日志与响应不含令牌。
func (h *Handler) createKiroAccount(ctx context.Context, item kiroImportCredential, name, proxyURL string) (*kiroCreateResult, int, error) {
	if h == nil || h.db == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("Kiro 账号存储未初始化")
	}
	cred := item.Credential
	cred.AuthKind = auth.NormalizeKiroAuthKind(item.RawAuthMethod, cred.TokenEndpoint, item.APIKey != "", cred.ClientSecret != "")
	cred.Endpoint = auth.NormalizeKiroEndpoint(cred.Endpoint, cred.AuthKind)
	if cred.AuthKind == auth.KiroAuthKindExternalIdP {
		if err := auth.ValidateKiroExternalIdPEndpoint(cred.TokenEndpoint); err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	if cred.AuthKind == auth.KiroAuthKindIdC && (cred.ClientID == "" || cred.ClientSecret == "") {
		return nil, http.StatusBadRequest, fmt.Errorf("IdC 凭据缺少 clientId / clientSecret")
	}
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("代理URL无效")
		}
	}
	if existing, err := h.findDuplicateKiroAccount(ctx, item.RefreshToken, item.APIKey); err != nil {
		return nil, http.StatusInternalServerError, err
	} else if existing > 0 {
		return nil, http.StatusConflict, fmt.Errorf("Kiro 凭据已存在 (id=%d)", existing)
	}
	cred.MachineID = auth.DeriveKiroMachineID(cred.MachineID, item.RefreshToken, item.APIKey)
	var warnings []string
	bearer := item.APIKey
	if cred.AuthKind != auth.KiroAuthKindAPIKey {
		if item.AccessToken == "" || time.Until(item.ExpiresAt) < 5*time.Minute {
			td, err := auth.KiroRefreshTokens(ctx, cred, item.RefreshToken, proxyURL)
			if err != nil {
				return nil, http.StatusBadGateway, fmt.Errorf("refresh token 换取 access token 失败: %w", err)
			}
			item.AccessToken, item.ExpiresAt = td.AccessToken, td.ExpiresAt
			if td.RefreshToken != "" {
				item.RefreshToken = td.RefreshToken
			}
			if td.ProfileARN != "" {
				cred.ProfileARN = td.ProfileARN
			}
		}
		bearer = item.AccessToken
		if cred.EffectiveProfileARN() == "" && !cred.IsSocialLogin() {
			if arn, err := auth.KiroResolveProfileARN(ctx, cred, bearer, proxyURL); err != nil {
				warnings = append(warnings, "profile 解析失败，将按 Builder ID 处理")
			} else if arn != "" {
				cred.ProfileARN = arn
			}
		}
	}
	credentials := map[string]interface{}{
		"upstream_type":                     auth.UpstreamKiro,
		auth.KiroAuthKindCredentialKey:      cred.AuthKind,
		auth.KiroMachineIDCredentialKey:     cred.MachineID,
		auth.KiroEndpointCredentialKey:      cred.Endpoint,
		auth.KiroLoginSourceCredentialKey:   cred.LoginSource,
		auth.KiroRegionCredentialKey:        cred.Region,
		auth.KiroAPIRegionCredentialKey:     cred.APIRegion,
		auth.KiroClientIDCredentialKey:      cred.ClientID,
		auth.KiroClientSecretCredentialKey:  cred.ClientSecret,
		auth.KiroStartURLCredentialKey:      cred.StartURL,
		auth.KiroTokenEndpointCredentialKey: cred.TokenEndpoint,
		auth.KiroIssuerURLCredentialKey:     cred.IssuerURL,
		auth.KiroScopesCredentialKey:        cred.Scopes,
		auth.KiroProfileARNCredentialKey:    cred.ProfileARN,
		"email":                             item.Email,
	}
	for key, value := range credentials {
		if text, ok := value.(string); ok && text == "" {
			delete(credentials, key)
		}
	}
	if cred.AuthKind == auth.KiroAuthKindAPIKey {
		credentials["api_key"] = item.APIKey
	} else {
		credentials["access_token"] = item.AccessToken
		credentials["refresh_token"] = item.RefreshToken
		credentials["expires_at"] = item.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if models, err := auth.KiroFetchModels(ctx, cred, bearer, proxyURL); err == nil && len(models) > 0 {
		credentials["models"] = models
	} else if err != nil {
		warnings = append(warnings, "模型目录同步失败，暂用默认目录")
	}
	if usage, err := auth.KiroFetchUsage(ctx, cred, bearer, proxyURL); err == nil {
		credentials[auth.KiroUsageCredentialKey] = kiroUsageCredential(usage)
		if usage.SubscriptionTitle != "" {
			credentials["plan_type"] = usage.SubscriptionTitle
		}
	} else {
		warnings = append(warnings, "额度查询失败")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = firstKiroValue(item.Email, "kiro")
	}
	h.mergeDuplicateMu.Lock()
	if existing, err := h.findDuplicateKiroAccount(ctx, item.RefreshToken, item.APIKey); err != nil || existing > 0 {
		h.mergeDuplicateMu.Unlock()
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return nil, http.StatusConflict, fmt.Errorf("Kiro 凭据已存在 (id=%d)", existing)
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "kiro", auth.UpstreamKiro, credentials, proxyURL)
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("保存 Kiro 账号失败: %w", err)
	}
	if h.store != nil {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			log.Printf("Kiro 账号 %d 装载进号池失败: %v", id, err)
			warnings = append(warnings, "已保存，但装载进号池失败，请重载账号")
		}
	}
	return &kiroCreateResult{ID: id, Email: item.Email, Warnings: warnings}, http.StatusOK, nil
}

func (h *Handler) findDuplicateKiroAccount(ctx context.Context, refreshToken, apiKey string) (int64, error) {
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelKiro)
	if err != nil {
		return 0, fmt.Errorf("查询 Kiro 账号失败: %w", err)
	}
	for _, row := range rows {
		if refreshToken != "" && strings.TrimSpace(row.GetCredential("refresh_token")) == refreshToken {
			return row.ID, nil
		}
		if apiKey != "" && strings.TrimSpace(row.GetCredential("api_key")) == apiKey {
			return row.ID, nil
		}
	}
	return 0, nil
}

type kiroImportRequest struct {
	Content      any    `json:"content"`
	Name         string `json:"name"`
	ProxyURL     string `json:"proxy_url"`
	UseProxyPool bool   `json:"use_proxy_pool"`
}

// ImportKiroAccounts 批量导入；单项失败不影响其他项，逐项返回结果。
func (h *Handler) ImportKiroAccounts(c *gin.Context) {
	var req kiroImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	items, err := parseKiroImport(req.Content)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	proxyURL, err := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	type itemResult struct {
		*kiroCreateResult
		Index int    `json:"index"`
		Error string `json:"error,omitempty"`
	}
	results := make([]itemResult, 0, len(items))
	imported := 0
	for index, item := range items {
		name := req.Name
		if len(items) > 1 {
			name = ""
		}
		created, _, err := h.createKiroAccount(c.Request.Context(), item, name, proxyURL)
		if err != nil {
			results = append(results, itemResult{Index: index, Error: err.Error()})
			continue
		}
		imported++
		results = append(results, itemResult{kiroCreateResult: created, Index: index})
		security.SecurityAuditLog("KIRO_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d ip=%s", created.ID, c.ClientIP()))
	}
	if imported > 0 {
		h.invalidateClaudeCatalogCaches()
	}
	c.JSON(http.StatusOK, gin.H{"imported": imported, "failed": len(items) - imported, "results": results})
}

// ExportKiroAccounts 导出为 Kiro Account Manager（默认）或 kiro.rs credentials.json 格式。
func (h *Handler) ExportKiroAccounts(c *gin.Context) {
	rows, err := h.db.ListActiveByChannel(c.Request.Context(), database.UpstreamChannelKiro)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询 Kiro 账号失败")
		return
	}
	rawIDs, idsPresent := c.GetQuery("ids")
	ids, err := parseAntigravityExportIDSet(rawIDs, idsPresent)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "kam")))
	kiroRS, kam := []map[string]any{}, []map[string]any{}
	for _, row := range rows {
		if len(ids) > 0 && !ids[row.ID] {
			continue
		}
		cred := auth.KiroCredentialFromRow(row)
		expires := kiroParseExpiry(row.GetCredential("expires_at"))
		if format == "kiro-rs" {
			entry := map[string]any{"refreshToken": row.GetCredential("refresh_token"), "accessToken": row.GetCredential("access_token"), "authMethod": cred.AuthKind, "provider": cred.LoginSource,
				"clientId": cred.ClientID, "clientSecret": cred.ClientSecret, "region": cred.Region, "apiRegion": cred.APIRegion, "startUrl": cred.StartURL, "profileArn": cred.ProfileARN,
				"machineId": cred.MachineID, "endpoint": cred.Endpoint, "tokenEndpoint": cred.TokenEndpoint, "issuerUrl": cred.IssuerURL, "scopes": cred.Scopes, "kiroApiKey": row.GetCredential("api_key"), "email": row.GetCredential("email")}
			if !expires.IsZero() {
				entry["expiresAt"] = expires.UTC().Format(time.RFC3339)
			}
			kiroRS = append(kiroRS, kiroCompactMap(entry))
			continue
		}
		// 导入器认得的认证字段全部带上，External IdP / IdC 账号导出再导入不丢配置。
		credentials := kiroCompactMap(map[string]any{"accessToken": row.GetCredential("access_token"), "refreshToken": row.GetCredential("refresh_token"), "authMethod": cred.AuthKind,
			"provider": cred.LoginSource, "clientId": cred.ClientID, "clientSecret": cred.ClientSecret, "region": cred.Region, "apiRegion": cred.APIRegion, "startUrl": cred.StartURL,
			"endpoint": cred.Endpoint, "tokenEndpoint": cred.TokenEndpoint, "issuerUrl": cred.IssuerURL, "scopes": cred.Scopes, "kiroApiKey": row.GetCredential("api_key")})
		if !expires.IsZero() {
			credentials["expiresAt"] = expires.UnixMilli()
		}
		account := map[string]any{"id": uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("codex2api-kiro-%d", row.ID))).String(), "email": row.GetCredential("email"), "idp": cred.LoginSource,
			"machineId": cred.MachineID, "profileArn": cred.ProfileARN, "credentials": credentials, "status": "active", "tags": []any{}, "createdAt": row.CreatedAt.UnixMilli(), "lastUsedAt": row.UpdatedAt.UnixMilli()}
		var usage auth.KiroUsageSnapshot
		if json.Unmarshal([]byte(row.GetCredential(auth.KiroUsageCredentialKey)), &usage) == nil && !usage.FetchedAt.IsZero() {
			percent := 0
			if usage.Limit > 0 {
				percent = int(usage.Used * 100 / usage.Limit)
			}
			account["subscription"] = map[string]any{"title": usage.SubscriptionTitle, "type": usage.SubscriptionType}
			account["usage"] = map[string]any{"current": int(usage.Used), "limit": int(usage.Limit), "percentUsed": percent, "lastUpdated": usage.FetchedAt.UnixMilli()}
		}
		kam = append(kam, account)
	}
	security.SecurityAuditLog("KIRO_ACCOUNTS_EXPORTED", fmt.Sprintf("count=%d format=%s ip=%s", len(kam)+len(kiroRS), format, c.ClientIP()))
	stamp := time.Now().Format("2006-01-02_15-04-05")
	if format == "kiro-rs" {
		writeSecretDownloadHeaders(c, "kiro-rs-credentials-"+stamp+".json")
		c.JSON(http.StatusOK, kiroRS)
		return
	}
	writeSecretDownloadHeaders(c, "kiro-account-manager-export-"+stamp+".json")
	c.JSON(http.StatusOK, gin.H{"version": "1.0", "exportedAt": time.Now().UnixMilli(), "accounts": kam, "groups": []any{}, "tags": []any{}})
}

func kiroCompactMap(values map[string]any) map[string]any {
	for key, value := range values {
		if text, ok := value.(string); ok && text == "" {
			delete(values, key)
		}
	}
	return values
}

// ---- 交互式登录 ----

type kiroLoginPending struct {
	social    auth.KiroSocialSession
	device    *auth.KiroDeviceSession
	proxyURL  string
	name      string
	expiresAt time.Time
}

var kiroLoginSessions sync.Map

func kiroLoginPut(id string, pending kiroLoginPending) {
	kiroLoginSessions.Range(func(key, value any) bool {
		if time.Now().After(value.(kiroLoginPending).expiresAt) {
			kiroLoginSessions.Delete(key)
		}
		return true
	})
	kiroLoginSessions.Store(id, pending)
}

func kiroLoginGet(id string) (kiroLoginPending, bool) {
	value, ok := kiroLoginSessions.Load(id)
	if !ok || time.Now().After(value.(kiroLoginPending).expiresAt) {
		return kiroLoginPending{}, false
	}
	return value.(kiroLoginPending), true
}

type kiroLoginRequest struct {
	Name         string `json:"name"`
	ProxyURL     string `json:"proxy_url"`
	UseProxyPool bool   `json:"use_proxy_pool"`
	StartURL     string `json:"start_url"`
	Region       string `json:"region"`
	Session      string `json:"session"`
	CallbackURL  string `json:"callback_url"`
}

// StartKiroSocialLogin 返回 Kiro 门户登录地址。门户只回调到本机端口，远端部署时让用户把
// 浏览器地址栏里的回调 URL 粘回来。
func (h *Handler) StartKiroSocialLogin(c *gin.Context) {
	var req kiroLoginRequest
	_ = c.ShouldBindJSON(&req)
	proxyURL, err := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	session, portalURL := auth.NewKiroSocialSession()
	kiroLoginPut(session.State, kiroLoginPending{social: session, proxyURL: proxyURL, name: req.Name, expiresAt: time.Now().Add(15 * time.Minute)})
	c.JSON(http.StatusOK, gin.H{"auth_url": portalURL, "session": session.State, "redirect_uri": session.RedirectURI})
}

// FinishKiroSocialLogin 用粘贴回来的回调 URL 换票并创建账号。
func (h *Handler) FinishKiroSocialLogin(c *gin.Context) {
	var req kiroLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	callback, err := auth.ParseKiroSocialCallback(req.CallbackURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := firstKiroValue(callback.State, req.Session)
	pending, ok := kiroLoginGet(sessionID)
	if !ok || pending.social.State == "" {
		writeError(c, http.StatusBadRequest, "登录会话不存在或已过期，请重新开始")
		return
	}
	kiroLoginSessions.Delete(sessionID)
	td, err := auth.KiroExchangeSocialCode(c.Request.Context(), pending.social, callback, pending.proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	source := "Google"
	if strings.EqualFold(callback.LoginOption, "github") {
		source = "Github"
	}
	item := kiroImportCredential{AccessToken: td.AccessToken, RefreshToken: td.RefreshToken, ExpiresAt: td.ExpiresAt, RawAuthMethod: auth.KiroAuthKindSocial,
		Credential: auth.KiroCredential{LoginSource: source, ProfileARN: td.ProfileARN}}
	h.respondKiroCreate(c, item, pending.name, pending.proxyURL)
}

// StartKiroDeviceLogin 发起 Builder ID / Enterprise 设备码登录。
func (h *Handler) StartKiroDeviceLogin(c *gin.Context) {
	var req kiroLoginRequest
	_ = c.ShouldBindJSON(&req)
	proxyURL, err := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	session, err := auth.StartKiroDeviceLogin(c.Request.Context(), req.Region, req.StartURL, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	id := uuid.NewString()
	kiroLoginPut(id, kiroLoginPending{device: session, proxyURL: proxyURL, name: req.Name, expiresAt: session.ExpiresAt})
	c.JSON(http.StatusOK, gin.H{"session": id, "user_code": session.UserCode, "verification_uri_complete": session.VerificationURIComplete, "interval": session.Interval, "expires_at": session.ExpiresAt})
}

// PollKiroDeviceLogin 轮询设备码登录；完成后创建账号。
func (h *Handler) PollKiroDeviceLogin(c *gin.Context) {
	var req kiroLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	pending, ok := kiroLoginGet(req.Session)
	if !ok || pending.device == nil {
		writeError(c, http.StatusBadRequest, "登录会话不存在或已过期，请重新开始")
		return
	}
	td, err := auth.PollKiroDeviceLogin(c.Request.Context(), pending.device, pending.proxyURL)
	if err == auth.ErrKiroDevicePending {
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
		return
	}
	kiroLoginSessions.Delete(req.Session)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	source := "BuilderId"
	if pending.device.StartURL != auth.KiroBuilderIDStart {
		source = "Enterprise"
	}
	item := kiroImportCredential{AccessToken: td.AccessToken, RefreshToken: td.RefreshToken, ExpiresAt: td.ExpiresAt, RawAuthMethod: auth.KiroAuthKindIdC, Credential: auth.KiroCredential{
		LoginSource: source, Region: pending.device.Region, StartURL: pending.device.StartURL, ClientID: pending.device.ClientID, ClientSecret: pending.device.ClientSecret, ProfileARN: td.ProfileARN,
	}}
	h.respondKiroCreate(c, item, pending.name, pending.proxyURL)
}

func (h *Handler) respondKiroCreate(c *gin.Context, item kiroImportCredential, name, proxyURL string) {
	created, status, err := h.createKiroAccount(c.Request.Context(), item, name, proxyURL)
	if err != nil {
		writeError(c, status, err.Error())
		return
	}
	h.invalidateClaudeCatalogCaches()
	security.SecurityAuditLog("KIRO_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d ip=%s", created.ID, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"status": "completed", "id": created.ID, "email": created.Email, "warnings": created.Warnings})
}

// ---- 额度与模型同步 ----

// kiroChannelModels 返回号池里 Kiro 账号模型目录的并集（定价页与模型目录用）；没有 Kiro 账号时为空。
func (h *Handler) kiroChannelModels() []string {
	if h == nil || h.store == nil {
		return nil
	}
	seen := map[string]struct{}{}
	models := make([]string, 0)
	for _, account := range h.store.Accounts() {
		if !account.IsKiro() {
			continue
		}
		for _, model := range proxy.KiroAccountModelIDs(account) {
			key := strings.ToLower(strings.TrimSpace(model))
			if _, ok := seen[key]; ok || key == "" {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, strings.TrimSpace(model))
		}
	}
	sort.Strings(models)
	return models
}

func (h *Handler) refreshKiroChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelKiro, Added: []string{}}
	if h == nil || h.store == nil || h.db == nil {
		return result
	}
	before := h.kiroChannelModels()
	// 额度是逐号的，组内每个成员都同步一次，模型目录随之更新。
	h.probePlanGroups(ctx, database.UpstreamChannelKiro, h.planGroupsFor(func(a *auth.Account) bool { return a.IsKiro() }), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			var lastErr error
			count := 0
			for _, member := range group.Members {
				_, models, err := h.syncKiroAccount(ctx, member.ID())
				if err != nil && len(models) == 0 {
					lastErr = err
					continue
				}
				count = max(count, len(models))
			}
			return count, nil, lastErr
		})
	result.Added = newlyAddedModels(before, h.kiroChannelModels())
	return result
}

// syncKiroAccount 刷新一个账号的额度快照与模型目录，并重新装载进号池。
func (h *Handler) syncKiroAccount(ctx context.Context, id int64) (*auth.KiroUsageSnapshot, []string, error) {
	account := h.store.FindByID(id)
	if account == nil || !account.IsKiro() {
		return nil, nil, fmt.Errorf("Kiro 账号不存在")
	}
	if account.KiroRefreshable() && account.NeedsRefresh() {
		if err := h.store.RefreshKiroAccount(ctx, account); err != nil {
			return nil, nil, err
		}
	}
	requestAuth := account.KiroRequestAuth()
	usage, usageErr := auth.KiroFetchUsage(ctx, requestAuth.Credential, requestAuth.BearerToken, requestAuth.ProxyURL)
	models, modelErr := auth.KiroFetchModels(ctx, requestAuth.Credential, requestAuth.BearerToken, requestAuth.ProxyURL)
	if usageErr != nil && modelErr != nil {
		return nil, nil, usageErr
	}
	updates := map[string]interface{}{}
	if usageErr == nil {
		updates[auth.KiroUsageCredentialKey] = kiroUsageCredential(usage)
		if usage.SubscriptionTitle != "" {
			updates["plan_type"] = usage.SubscriptionTitle
		}
	}
	if modelErr == nil && len(models) > 0 {
		updates["models"] = models
	}
	if err := h.db.UpdateCredentials(ctx, id, updates); err != nil {
		return nil, nil, err
	}
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		return nil, nil, err
	}
	h.invalidateClaudeCatalogCaches()
	// 与 kiro.rs 一致：额度查不到就报错，不让旧快照冒充最新额度；模型目录已照常保存。
	if usageErr != nil {
		return nil, models, fmt.Errorf("Kiro 额度查询失败: %w", usageErr)
	}
	return usage, models, nil
}

// SyncKiroAccount 刷新单个账号的额度与模型目录。
func (h *Handler) SyncKiroAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	usage, models, err := h.syncKiroAccount(c.Request.Context(), id)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": usage, "models": models})
}

// SyncAllKiroAccounts 逐个刷新所有 Kiro 账号。
func (h *Handler) SyncAllKiroAccounts(c *gin.Context) {
	refreshed, failed, err := h.syncAllKiroAccounts(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询 Kiro 账号失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"refreshed": refreshed, "failed": failed})
}

func (h *Handler) syncAllKiroAccounts(ctx context.Context) (refreshed, failed int, err error) {
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelKiro)
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if _, _, err := h.syncKiroAccount(ctx, row.ID); err != nil {
			failed++
			log.Printf("Kiro 账号 %d 同步失败: %v", row.ID, err)
			continue
		}
		refreshed++
	}
	return refreshed, failed, nil
}

// StartKiroSync 每 30 分钟同步一次 Kiro 账号的额度与模型目录（与 Antigravity 配额巡检同频）。
func (h *Handler) StartKiroSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if refreshed, failed, err := h.syncAllKiroAccounts(ctx); err != nil {
					log.Printf("[Kiro 同步] 查询账号失败: %v", err)
				} else if refreshed+failed > 0 {
					log.Printf("[Kiro 同步] 完成: 成功 %d, 失败 %d", refreshed, failed)
				}
			}
		}
	}()
}
