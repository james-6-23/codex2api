package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// addGrokAccountReq 是新增/更新 Grok 账号的请求体。
// AuthKind 取 api_key / oauth；oauth 时 AuthJSON 为 Grok CLI auth.json 原文，
// api_key 时 APIKey 为 xAI API Key。
type addGrokAccountReq struct {
	Name         string   `json:"name"`
	AuthKind     string   `json:"auth_kind"`
	AuthJSON     string   `json:"auth_json"`
	APIKey       string   `json:"api_key"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	ModelMapping string   `json:"model_mapping"`
	ProxyURL     string   `json:"proxy_url"`
}

// grokCredentialsFromRequest 校验请求并构造待入库的 credentials map。
// 返回的 email 用于列表展示（OAuth 取 subject，API Key 取脱敏 key）。
func grokCredentialsFromRequest(req *addGrokAccountReq) (map[string]interface{}, string, error) {
	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
	if err != nil {
		return nil, "", err
	}
	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		// 默认按 OAuth 订阅账号的免费档展示；API Key 账号无订阅档位，下方分支改回 "api"。
		// billing 探针成功后 OAuth 账号会被纠正为真实套餐（free/SuperGrok/Heavy）。
		"plan_type": "free",
	}
	if baseURL != "" {
		credentials["base_url"] = baseURL
	}

	authKind := strings.ToLower(strings.TrimSpace(req.AuthKind))
	email := ""
	switch authKind {
	case auth.GrokAuthKindAPIKey:
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			return nil, "", fmt.Errorf("API Key 是必填字段")
		}
		credentials["api_key"] = apiKey
		credentials["plan_type"] = "api"
		email = "xai-api-key"
	case auth.GrokAuthKindOAuth, "":
		creds, err := auth.ParseGrokAuthJSON([]byte(req.AuthJSON))
		if err != nil {
			return nil, "", fmt.Errorf("解析 auth.json 失败: %w", err)
		}
		// 取第一条可用凭据（多 scope 文件通常首条即目标账号）
		cred := creds[0]
		if cred.AuthKind() == auth.GrokAuthKindAPIKey {
			credentials["api_key"] = cred.APIKey
			credentials["plan_type"] = "api"
			email = "xai-api-key"
			break
		}
		if strings.TrimSpace(cred.RefreshToken) == "" {
			return nil, "", fmt.Errorf("auth.json 中的 OAuth 凭据缺少 refresh_token")
		}
		if strings.TrimSpace(cred.ClientID) == "" {
			return nil, "", fmt.Errorf("auth.json 中的 OAuth 凭据缺少 client_id，无法刷新")
		}
		credentials["refresh_token"] = cred.RefreshToken
		credentials["grok_client_id"] = cred.ClientID
		if cred.AccessToken != "" {
			credentials["access_token"] = cred.AccessToken
		}
		if !cred.ExpiresAt.IsZero() {
			credentials["expires_at"] = cred.ExpiresAt.Format(time.RFC3339)
		}
		if cred.TokenEndpoint != "" {
			credentials["grok_token_endpoint"] = cred.TokenEndpoint
		}
		if cred.OIDCIssuer != "" {
			credentials["grok_oidc_issuer"] = cred.OIDCIssuer
		}
		if cred.PrincipalType != "" {
			credentials["grok_principal_type"] = cred.PrincipalType
		}
		if cred.PrincipalID != "" {
			credentials["grok_principal_id"] = cred.PrincipalID
		}
		if cred.Subject != "" {
			credentials["account_id"] = cred.Subject
			email = cred.Subject
		}
		// 凭据文件带真实 email 时优先用它（CPA / 带 email 的 auth.json）。
		if cred.Email != "" {
			email = cred.Email
		}
	default:
		return nil, "", fmt.Errorf("auth_kind 必须是 oauth 或 api_key")
	}

	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			return nil, "", fmt.Errorf("模型名称无效: %s", model)
		}
	}
	if len(models) > 0 {
		credentials["models"] = models
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		return nil, "", err
	}
	if modelMapping != "" {
		credentials["model_mapping"] = modelMapping
	}
	if email != "" {
		credentials["email"] = email
	}
	return credentials, email, nil
}

// AddGrokAccount 新增一个 Grok 上游账号（POST /api/admin/accounts/grok）。
func (h *Handler) AddGrokAccount(c *gin.Context) {
	var req addGrokAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	credentials, email, err := grokCredentialsFromRequest(&req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	name := req.Name
	if name == "" {
		name = "grok"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "xai", auth.UpstreamGrok, credentials, req.ProxyURL)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.db.InsertAccountEventAsync(id, "added", "manual_grok")

	models := auth.NormalizeAccountModels(req.Models)
	acc := grokAccountFromCredentials(id, credentials, req.ProxyURL)
	acc.Models = models
	acc.ModelMapping = strings.TrimSpace(req.ModelMapping)
	acc.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	acc.Email = email
	h.store.AddAccount(acc)

	// 异步 billing 探针：拉取套餐/周月额度，避免被 ChatGPT wham 误封
	h.triggerGrokUsageProbe(id)

	security.SecurityAuditLog("GROK_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d auth_kind=%s models=%d ip=%s", id, acc.GrokAuthKind(), len(models), c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"message": "成功添加 Grok 账号", "id": id})
}

// UpdateGrokAccount 更新 Grok 账号的可编辑配置（PATCH /api/admin/accounts/:id/grok）。
// 仅更新 base_url / models / model_mapping / proxy / api_key（凭据留空则不改）。
func (h *Handler) UpdateGrokAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var req addGrokAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamGrok) {
		writeError(c, http.StatusBadRequest, "仅 Grok 账号支持该设置")
		return
	}

	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	apiKey := strings.TrimSpace(req.APIKey)

	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		"base_url":      baseURL,
		"models":        models,
		"model_mapping": modelMapping,
	}
	if apiKey != "" {
		credentials["api_key"] = apiKey
	}
	if err := h.db.UpdateCredentials(ctx, id, credentials); err != nil {
		writeInternalError(c, err)
		return
	}
	// proxy_url 是独立列，不在 credentials 里；UpdateCredentials 不会写它。
	// 编辑是整体重写语义（空值即清空代理），须单独持久化，否则代理只落到内存
	// store，重载 / 重启 / 后台刷新后被 DB 旧值覆盖，表现为"添加代理不生效"。
	if err := h.db.UpdateAccountProxyURL(ctx, id, req.ProxyURL); err != nil {
		writeInternalError(c, err)
		return
	}
	if req.Name != "" {
		_ = h.db.UpdateAccountName(ctx, id, req.Name)
	}
	if h.store != nil {
		h.store.ApplyGrokConfig(id, baseURL, apiKey, models, modelMapping, req.ProxyURL)
	}
	h.db.InsertAccountEventAsync(id, "updated", "manual_grok")
	writeMessage(c, http.StatusOK, "Grok 账号设置已更新")
}

// FetchGrokModels 用请求内凭据或已保存账号凭据探测 Grok 上游模型目录
// （POST /api/admin/accounts/grok/models）。
func (h *Handler) FetchGrokModels(c *gin.Context) {
	var req addGrokAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	credentials, _, err := grokCredentialsFromRequest(&req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	// 构造临时账号做探测，不入库、不入池
	probe := &auth.Account{
		UpstreamType:      auth.UpstreamGrok,
		BaseURL:           strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"),
		ProxyURL:          security.SanitizeInput(req.ProxyURL),
		APIKey:            credentialStringValue(credentials, "api_key"),
		AccessToken:       credentialStringValue(credentials, "access_token"),
		RefreshToken:      credentialStringValue(credentials, "refresh_token"),
		GrokClientID:      credentialStringValue(credentials, "grok_client_id"),
		GrokTokenEndpoint: credentialStringValue(credentials, "grok_token_endpoint"),
		GrokOIDCIssuer:    credentialStringValue(credentials, "grok_oidc_issuer"),
		GrokPrincipalType: credentialStringValue(credentials, "grok_principal_type"),
		GrokPrincipalID:   credentialStringValue(credentials, "grok_principal_id"),
		AccountID:         credentialStringValue(credentials, "account_id"),
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	// OAuth 凭据无 AT 时先刷一次
	if probe.APIKey == "" && probe.AccessToken == "" && probe.RefreshToken != "" {
		td, refreshErr := auth.RefreshGrokAccessToken(ctx, auth.GrokRefreshParams{
			RefreshToken:  probe.RefreshToken,
			ClientID:      probe.GrokClientID,
			TokenEndpoint: probe.GrokTokenEndpoint,
			OIDCIssuer:    probe.GrokOIDCIssuer,
			PrincipalType: probe.GrokPrincipalType,
			PrincipalID:   probe.GrokPrincipalID,
			ProxyURL:      probe.ProxyURL,
		})
		if refreshErr != nil {
			writeError(c, http.StatusBadGateway, fmt.Sprintf("Grok 凭据刷新失败: %s", refreshErr.Error()))
			return
		}
		probe.AccessToken = td.AccessToken
	}

	models, err := proxy.FetchGrokModelIDs(ctx, probe)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"models": models})
}

func credentialStringValue(credentials map[string]interface{}, key string) string {
	if credentials == nil {
		return ""
	}
	if value, ok := credentials[key].(string); ok {
		return value
	}
	return ""
}

// grokPlanTypeOrDefault 取 credentials 里的 plan_type，缺失时回落到免费档
// （现有写入路径都会显式设置，这里仅作防御性兜底）。
func grokPlanTypeOrDefault(credentials map[string]interface{}) string {
	if plan := strings.TrimSpace(credentialStringValue(credentials, "plan_type")); plan != "" {
		return plan
	}
	return "free"
}

// grokAccountFromCredentials 从入库用的 credentials map 构造内存态 Account，
// 供单条添加与批量文件导入共用。models/model_mapping/base_url/email 由调用方按需覆写。
func grokAccountFromCredentials(id int64, credentials map[string]interface{}, proxyURL string) *auth.Account {
	acc := &auth.Account{
		DBID:         id,
		ProxyURL:     proxyURL,
		HealthTier:   auth.HealthTierHealthy,
		UpstreamType: auth.UpstreamGrok,
		BaseURL:      strings.TrimRight(credentialStringValue(credentials, "base_url"), "/"),
		ModelMapping: credentialStringValue(credentials, "model_mapping"),
		Email:        credentialStringValue(credentials, "email"),
		// 与 credentials 保持一致（OAuth 默认 free、API Key 为 api）；不再写死 "api"。
		PlanType:          grokPlanTypeOrDefault(credentials),
		GrokClientID:      credentialStringValue(credentials, "grok_client_id"),
		GrokTokenEndpoint: credentialStringValue(credentials, "grok_token_endpoint"),
		GrokOIDCIssuer:    credentialStringValue(credentials, "grok_oidc_issuer"),
		GrokPrincipalType: credentialStringValue(credentials, "grok_principal_type"),
		GrokPrincipalID:   credentialStringValue(credentials, "grok_principal_id"),
		AccountID:         credentialStringValue(credentials, "account_id"),
		APIKey:            credentialStringValue(credentials, "api_key"),
		AccessToken:       credentialStringValue(credentials, "access_token"),
		RefreshToken:      credentialStringValue(credentials, "refresh_token"),
	}
	if models, ok := credentials["models"].([]string); ok {
		acc.Models = models
	}
	if exp := credentialStringValue(credentials, "expires_at"); exp != "" {
		if t, err := time.Parse(time.RFC3339, exp); err == nil {
			acc.ExpiresAt = t
		}
	}
	return acc
}

// batchImportGrokReq 是 CPA / auth.json 文件批量导入的请求体。
// Files 每项是一个凭据文件的原始 JSON 内容（CPA 单对象或 Grok CLI auth.json 均可）。
type batchImportGrokReq struct {
	Files    []string `json:"files"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	ProxyURL string   `json:"proxy_url"`
}

type grokBatchImportItem struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

const grokBatchImportMaxFiles = 500

// BatchImportGrokAccounts 批量导入 Grok 凭据文件（POST /api/admin/accounts/grok/import）。
// 每个文件独立解析入库，按 subject / refresh_token 去重（批内 + 与现有账号）。
func (h *Handler) BatchImportGrokAccounts(c *gin.Context) {
	var req batchImportGrokReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	if len(req.Files) == 0 {
		writeError(c, http.StatusBadRequest, "未提供任何文件内容")
		return
	}
	if len(req.Files) > grokBatchImportMaxFiles {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个文件", grokBatchImportMaxFiles))
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

	// 已存在 Grok 账号的 subject 集合，用于跳过重复导入（每个凭据文件 sub 唯一）。
	existingSubjects := make(map[string]struct{})
	if h.store != nil {
		for _, acc := range h.store.Accounts() {
			if !acc.IsGrokAPI() {
				continue
			}
			if sub := strings.TrimSpace(acc.GrokUserID()); sub != "" {
				existingSubjects[sub] = struct{}{}
			}
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	items := make([]grokBatchImportItem, 0, len(req.Files))
	imported := 0
	for i, content := range req.Files {
		item := grokBatchImportItem{}
		fileReq := &addGrokAccountReq{
			AuthKind: auth.GrokAuthKindOAuth,
			AuthJSON: content,
			BaseURL:  req.BaseURL,
			Models:   req.Models,
		}
		credentials, email, parseErr := grokCredentialsFromRequest(fileReq)
		if parseErr != nil {
			item.Error = parseErr.Error()
			items = append(items, item)
			continue
		}
		item.Email = email
		subject := credentialStringValue(credentials, "account_id")
		if subject != "" {
			if _, dup := existingSubjects[subject]; dup {
				item.Error = "账号已存在，已跳过"
				items = append(items, item)
				continue
			}
		}

		name := email
		if name == "" {
			name = fmt.Sprintf("grok-%d", i+1)
		}
		id, insertErr := h.db.InsertAccountWithUpstream(ctx, name, "xai", auth.UpstreamGrok, credentials, req.ProxyURL)
		if insertErr != nil {
			item.Error = insertErr.Error()
			items = append(items, item)
			continue
		}
		h.db.InsertAccountEventAsync(id, "added", "grok_file_import")

		acc := grokAccountFromCredentials(id, credentials, req.ProxyURL)
		acc.Models = models
		if baseURL != "" {
			acc.BaseURL = strings.TrimRight(baseURL, "/")
		}
		h.store.AddAccount(acc)

		if subject != "" {
			existingSubjects[subject] = struct{}{}
		}
		item.OK = true
		item.ID = id
		items = append(items, item)
		imported++

		h.triggerGrokUsageProbe(id)
	}

	security.SecurityAuditLog("GROK_FILE_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(req.Files), imported, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"total":    len(req.Files),
		"imported": imported,
		"failed":   len(req.Files) - imported,
		"items":    items,
	})
}
