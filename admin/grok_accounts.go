package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
	// GroupIDs 让添加时就把新账号绑进指定分组（UpdateGrokAccount 复用本结构时忽略该字段，
	// 编辑分组走账号编辑抽屉的 group_ids）。
	GroupIDs json.RawMessage `json:"group_ids"`
}

// grokCredentialsFromRequest 校验请求并构造待入库的 credentials map。
// 返回的 email 用于列表展示（OAuth 取 subject，API Key 取脱敏 key）。
func grokCredentialsFromRequest(req *addGrokAccountReq) (map[string]interface{}, string, error) {
	credentials, email, _, err := grokCredentialsFromRequestWithImportMeta(req)
	return credentials, email, err
}

type grokImportMeta struct {
	Disabled        bool
	DisabledPresent bool
	Subject         string
	FamilyID        string
}

func grokCredentialsFromRequestWithImportMeta(req *addGrokAccountReq) (map[string]interface{}, string, grokImportMeta, error) {
	var meta grokImportMeta
	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
	if err != nil {
		return nil, "", meta, err
	}
	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		// OAuth 凭据若带 access_token，会在下方从 JWT tier claim 识别真实套餐；
		// 缺失/无效 tier 保持空值。API Key 账号无订阅档位，单独标记为 "api"。
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
			return nil, "", meta, fmt.Errorf("API Key 是必填字段")
		}
		credentials["api_key"] = apiKey
		credentials["plan_type"] = "api"
		credentials["credential_family_id"] = auth.GrokCredentialFamilyID(&auth.GrokImportedCredential{APIKey: apiKey}, baseURL)
		meta.FamilyID = credentialStringValue(credentials, "credential_family_id")
		email = "xai-api-key"
	case auth.GrokAuthKindOAuth, "":
		creds, err := auth.ParseGrokAuthJSON([]byte(req.AuthJSON))
		if err != nil {
			return nil, "", meta, fmt.Errorf("解析 auth.json 失败: %w", err)
		}
		// 取第一条可用凭据（多 scope 文件通常首条即目标账号）
		cred := creds[0]
		if err := auth.ValidateGrokOAuthEndpoints(cred.TokenEndpoint, cred.OIDCIssuer); err != nil && cred.AuthKind() == auth.GrokAuthKindOAuth {
			return nil, "", meta, err
		}
		if cred.ArchivePlanType != "" {
			credentials["archive_plan_type"] = cred.ArchivePlanType
		}
		if cred.JWTPlanType != "" {
			credentials["jwt_plan_type"] = cred.JWTPlanType
			credentials["jwt_plan_trusted"] = cred.JWTPlanTrusted
		}
		if cred.DisabledPresent {
			credentials["archive_disabled"] = cred.Disabled
			credentials["archive_disabled_present"] = true
		}
		if familyID := auth.GrokCredentialFamilyID(cred, baseURL); familyID != "" {
			credentials["credential_family_id"] = familyID
			meta.FamilyID = familyID
		}
		meta.Disabled = cred.Disabled
		meta.DisabledPresent = cred.DisabledPresent
		meta.Subject = strings.TrimSpace(cred.Subject)
		if cred.AuthKind() == auth.GrokAuthKindAPIKey {
			credentials["api_key"] = cred.APIKey
			credentials["plan_type"] = "api"
			email = "xai-api-key"
			break
		}
		if strings.TrimSpace(cred.RefreshToken) == "" {
			return nil, "", meta, fmt.Errorf("auth.json 中的 OAuth 凭据缺少 refresh_token")
		}
		if strings.TrimSpace(cred.ClientID) == "" {
			return nil, "", meta, fmt.Errorf("auth.json 中的 OAuth 凭据缺少 client_id，无法刷新")
		}
		credentials["refresh_token"] = cred.RefreshToken
		credentials["grok_client_id"] = cred.ClientID
		if cred.AccessToken != "" {
			credentials["access_token"] = cred.AccessToken
		}
		if cred.PlanType != "" {
			credentials["plan_type"] = cred.PlanType
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
		return nil, "", meta, fmt.Errorf("auth_kind 必须是 oauth 或 api_key")
	}

	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			return nil, "", meta, fmt.Errorf("模型名称无效: %s", model)
		}
	}
	if len(models) > 0 {
		credentials["models"] = models
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		return nil, "", meta, err
	}
	if modelMapping != "" {
		credentials["model_mapping"] = modelMapping
	}
	if email != "" {
		credentials["email"] = email
	}
	return credentials, email, meta, nil
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

	credentials, email, importMeta, err := grokCredentialsFromRequestWithImportMeta(&req)
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
	// 分组校验放在建号之前：分组 ID 打错时不该留下一个没绑上分组的账号。
	groupIDs, err := h.resolveImportGroupIDsJSON(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	enabled := !(importMeta.DisabledPresent && importMeta.Disabled)
	id, duplicateID, err := h.db.InsertGrokAccountIfAbsent(ctx, name, credentials, req.ProxyURL, enabled)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if duplicateID > 0 {
		// 身份栅栏含回收站账号:提示持有者 ID,避免"列表明明是空的却说已存在"的死胡同。
		writeError(c, http.StatusConflict, fmt.Sprintf("Grok 凭据身份已存在（账号 ID %d，可能在回收站；批量导入可自动合并/复活）", duplicateID))
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
	message := "成功添加 Grok 账号"
	if err := h.bindImportedAccountGroups(ctx, []int64{id}, groupIDs); err != nil {
		message += "，但分组绑定失败: " + err.Error()
	}
	c.JSON(http.StatusOK, gin.H{"message": message, "id": id, "group_ids": groupIDs})
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
	// An identity change advances credential generation and invalidates prior
	// facts/capabilities. Rebuild the fenced catalog and minimal three-protocol
	// capability set asynchronously; configuration-only edits are deduplicated
	// by fresh capability rows.
	h.triggerGrokUsageProbe(id)
	h.db.InsertAccountEventAsync(id, "updated", "manual_grok")
	writeMessage(c, http.StatusOK, "Grok 账号设置已更新")
}

type batchUpdateGrokModelsReq struct {
	IDs    []int64  `json:"ids"`
	Models []string `json:"models"`
}

// BatchUpdateGrokModels 批量替换 Grok 账号的模型白名单
// （POST /api/admin/accounts/grok/batch-models）。
// 空数组 = 清空白名单（未声明，仅 grok 渠道 Key 可调度）；非空则整体替换。
// 非 Grok / 不存在的 ID 计入 failed，不中断整批。
func (h *Handler) BatchUpdateGrokModels(c *gin.Context) {
	var req batchUpdateGrokModelsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要更新的账号 ID 列表")
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	if len(models) > 200 {
		writeError(c, http.StatusBadRequest, "模型数量不能超过 200")
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}

	timeout := 15*time.Second + time.Duration(len(ids))*50*time.Millisecond
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	var success, failed int64
	for _, id := range ids {
		row, err := h.db.GetAccountByID(ctx, id)
		if err != nil {
			failed++
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamGrok) {
			failed++
			continue
		}
		if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{"models": models}); err != nil {
			failed++
			continue
		}
		if h.store != nil {
			h.store.ApplyAccountModels(id, models)
		}
		h.db.InsertAccountEventAsync(id, "updated", "batch_grok_models")
		success++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已更新 %d 个账号，失败 %d 个", success, failed),
		"success": success,
		"failed":  failed,
		"models":  models,
	})
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

	models, err := proxy.FetchGrokModelIDs(ctx, probe, h.store.ResolveProxyForAccount(probe))
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

func credentialBoolValue(credentials map[string]interface{}, key string) bool {
	if credentials == nil {
		return false
	}
	value, _ := credentials[key].(bool)
	return value
}

// grokPlanTypeFromCredentials 优先读取 access_token 的 tier claim，再兼容已有
// plan_type 展示值；API Key 账号没有订阅 tier，其余缺失/无效值保持空白。
func grokPlanTypeFromCredentials(credentials map[string]interface{}) string {
	if plan := auth.GrokPlanTypeFromAccessToken(credentialStringValue(credentials, "access_token")); plan != "" {
		return plan
	}
	if plan, ok := auth.ResolveGrokPlan(credentialStringValue(credentials, "plan_type")); ok {
		return plan.Key
	}
	if strings.TrimSpace(credentialStringValue(credentials, "api_key")) != "" {
		return "api"
	}
	return ""
}

// grokAccountFromCredentials 从入库用的 credentials map 构造内存态 Account，
// 供单条添加与批量文件导入共用。models/model_mapping/base_url/email 由调用方按需覆写。
func grokAccountFromCredentials(id int64, credentials map[string]interface{}, proxyURL string) *auth.Account {
	acc := &auth.Account{
		DBID:                 id,
		CredentialGeneration: 1,
		CredentialFamilyID:   credentialStringValue(credentials, "credential_family_id"),
		ProxyURL:             proxyURL,
		HealthTier:           auth.HealthTierHealthy,
		UpstreamType:         auth.UpstreamGrok,
		BaseURL:              strings.TrimRight(credentialStringValue(credentials, "base_url"), "/"),
		ModelMapping:         credentialStringValue(credentials, "model_mapping"),
		Email:                credentialStringValue(credentials, "email"),
		// 与 credentials 保持一致：OAuth 使用 tier 映射，API Key 为 api。
		PlanType:          grokPlanTypeFromCredentials(credentials),
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
	if credentialBoolValue(credentials, "archive_disabled") {
		atomic.StoreInt32(&acc.DispatchPaused, 1)
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
	// ImportProxy 打开后采用文件内携带的代理，并把它们注册进代理池；关闭时文件
	// 里的 proxy_url 一律忽略，全部账号用表单填的代理。
	ImportProxy bool `json:"import_proxy"`
	// GroupIDs 让导入时就把新账号绑进指定分组；跳过的重复账号不受影响。
	GroupIDs json.RawMessage `json:"group_ids"`
}

type grokBatchImportItem struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Updated/Revived 标记该条命中既有身份:凭据已合并进账号 ID(Revived 表示
	// 该账号原在回收站,本次已复活),而非新建。
	Updated bool `json:"updated,omitempty"`
	Revived bool `json:"revived,omitempty"`
}

const grokBatchImportMaxFiles = 5000

// 批量导入的整体超时按文件数缩放：整个循环串行地逐个文件落库，写死一个常量会让
// 每个文件分到的预算随批量增大而被摊薄（5000 个文件时只剩 12ms/个），数据库稍有抖动
// 就会中途超时、后面的文件全部报 context deadline exceeded。封顶是为了不让一个超大
// 请求无限期占住连接。
const (
	grokBatchImportBaseTimeout    = 30 * time.Second
	grokBatchImportPerFileTimeout = 100 * time.Millisecond
	grokBatchImportMaxTimeout     = 10 * time.Minute
)

func grokBatchImportTimeout(files int) time.Duration {
	if files < 0 {
		files = 0
	}
	timeout := grokBatchImportBaseTimeout + time.Duration(files)*grokBatchImportPerFileTimeout
	if timeout > grokBatchImportMaxTimeout {
		return grokBatchImportMaxTimeout
	}
	return timeout
}

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

	// 已存在 Grok 账号的 principal / credential-family 集合。family ID 不随 RT
	// 轮换变化，避免同一身份因 refresh token 已更新而被重复导入。
	existingSubjects := make(map[string]struct{})
	existingFamilies := make(map[string]struct{})
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
	if rows, listErr := h.db.ListActiveByChannel(c.Request.Context(), database.UpstreamChannelGrok); listErr == nil {
		for _, row := range rows {
			if row == nil {
				continue
			}
			if sub := strings.TrimSpace(row.GetCredential("account_id")); sub != "" {
				existingSubjects[sub] = struct{}{}
			}
			familyID := strings.TrimSpace(row.CredentialFamilyID)
			if familyID == "" {
				familyID = strings.TrimSpace(row.GetCredential("credential_family_id"))
			}
			if familyID != "" {
				existingFamilies[familyID] = struct{}{}
			}
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), grokBatchImportTimeout(len(req.Files)))
	defer cancel()
	groupIDs, err := h.resolveImportGroupIDsJSON(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	// 代理必须在任何账号落库之前注册进代理表并同步到内存代理池，顺序理由见
	// registerImportedProxyBindings。索引与 req.Files 对齐，逐文件取用。
	proxyBindings := make([]importProxyBinding, len(req.Files))
	var proxyOutcome importProxyOutcome
	if req.ImportProxy {
		for i, content := range req.Files {
			proxyBindings[i] = grokFileProxyBinding(content)
		}
		proxyOutcome, err = h.registerImportedProxyBindings(ctx, proxyBindings)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
	}

	items := make([]grokBatchImportItem, 0, len(req.Files))
	imported := 0
	createdIDs := make([]int64, 0, len(req.Files))
	for i, content := range req.Files {
		item := grokBatchImportItem{}
		fileReq := &addGrokAccountReq{
			AuthKind: auth.GrokAuthKindOAuth,
			AuthJSON: content,
			BaseURL:  req.BaseURL,
			Models:   req.Models,
		}
		credentials, email, importMeta, parseErr := grokCredentialsFromRequestWithImportMeta(fileReq)
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
		if importMeta.FamilyID != "" {
			if _, dup := existingFamilies[importMeta.FamilyID]; dup {
				item.Error = "凭据身份已存在，已跳过"
				items = append(items, item)
				continue
			}
		}

		name := email
		if name == "" {
			name = fmt.Sprintf("grok-%d", i+1)
		}
		// 文件内代理优先，缺失/非法时回落表单值。
		fileProxyURL := proxyBindings[i].url
		effectiveProxyURL := req.ProxyURL
		if fileProxyURL != "" {
			effectiveProxyURL = fileProxyURL
		}
		enabled := !(importMeta.DisabledPresent && importMeta.Disabled)
		id, duplicateID, insertErr := h.db.InsertGrokAccountIfAbsent(ctx, name, credentials, effectiveProxyURL, enabled)
		if insertErr != nil {
			item.Error = insertErr.Error()
			items = append(items, item)
			continue
		}
		if duplicateID > 0 {
			// 身份栅栏含回收站账号:命中时合并凭据(回收站复活)而非跳过,
			// 否则删过的号重新导入永远失败(issue #602)。credentials 里的
			// base_url/models 只在显式提供时存在,不会覆盖既有配置。
			//
			// 代理传空 = 保留账号原绑定。文件带来的代理是被动数据，不覆盖目标端
			// 已有的绑定（那里可能已经做过精细分配）；表单填的代理是操作员的显式
			// 换绑意图，维持既有覆盖语义。
			reauthProxyURL := effectiveProxyURL
			if fileProxyURL != "" && h.accountHasProxyBinding(ctx, duplicateID) {
				reauthProxyURL = ""
			}
			reauth, reauthErr := h.db.ReauthGrokAccount(ctx, duplicateID, credentials, "", reauthProxyURL)
			if reauthErr != nil {
				item.Error = fmt.Sprintf("合并到既有账号 %d 失败: %v", duplicateID, reauthErr)
				items = append(items, item)
				continue
			}
			h.store.RemoveAccount(duplicateID)
			if loadErr := h.store.LoadAccountByID(ctx, duplicateID); loadErr != nil {
				item.Error = fmt.Sprintf("更新账号 %d 后重载失败: %v", duplicateID, loadErr)
				items = append(items, item)
				continue
			}
			event := "updated"
			if reauth.Revived {
				event = "restored"
			}
			h.db.InsertAccountEventAsync(duplicateID, event, "grok_file_import")
			if subject != "" {
				existingSubjects[subject] = struct{}{}
			}
			if importMeta.FamilyID != "" {
				existingFamilies[importMeta.FamilyID] = struct{}{}
			}
			item.OK = true
			item.ID = duplicateID
			item.Updated = true
			item.Revived = reauth.Revived
			items = append(items, item)
			imported++
			createdIDs = append(createdIDs, duplicateID)
			h.triggerGrokUsageProbe(duplicateID)
			continue
		}
		h.db.InsertAccountEventAsync(id, "added", "grok_file_import")

		acc := grokAccountFromCredentials(id, credentials, effectiveProxyURL)
		acc.Models = models
		if baseURL != "" {
			acc.BaseURL = strings.TrimRight(baseURL, "/")
		}
		h.store.AddAccount(acc)

		if subject != "" {
			existingSubjects[subject] = struct{}{}
		}
		if importMeta.FamilyID != "" {
			existingFamilies[importMeta.FamilyID] = struct{}{}
		}
		item.OK = true
		item.ID = id
		items = append(items, item)
		imported++
		createdIDs = append(createdIDs, id)

		h.triggerGrokUsageProbe(id)
	}

	security.SecurityAuditLog("GROK_FILE_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(req.Files), imported, c.ClientIP()))
	response := gin.H{
		"total":     len(req.Files),
		"imported":  imported,
		"failed":    len(req.Files) - imported,
		"items":     items,
		"group_ids": groupIDs,
	}
	if req.ImportProxy {
		response["proxies_imported"] = proxyOutcome.inserted
		response["proxies_skipped"] = proxyOutcome.skipped
		if warning := proxyOutcome.warning(); warning != "" {
			response["proxy_warning"] = warning
		}
	}
	if err := h.bindImportedAccountGroups(ctx, createdIDs, groupIDs); err != nil {
		response["group_bind_error"] = err.Error()
	}
	c.JSON(http.StatusOK, response)
}
