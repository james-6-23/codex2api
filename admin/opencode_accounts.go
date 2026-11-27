package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

// OpenCode 账号管理：添加/更新、批量导入（多行 API Key / JSON / Sub2API opencode_go 导出）、
// 导出、模型目录与 Go 订阅额度同步。

func (h *Handler) invalidateOpenCodeCatalogCaches() {
	if h == nil {
		return
	}
	h.expireAccountListSnapshot(database.UpstreamChannelOpenCode)
	h.accountAnalysisCacheMu.Lock()
	if h.accountAnalysisCache != nil {
		delete(h.accountAnalysisCache, database.UpstreamChannelOpenCode)
	}
	h.accountAnalysisCacheMu.Unlock()
}

func (h *Handler) registerOpenCodeRoutes(api *gin.RouterGroup) {
	api.POST("/accounts/opencode", h.AddOpenCodeAccount)
	api.PUT("/accounts/:id/opencode", h.UpdateOpenCodeAccount)
	api.POST("/accounts/opencode/import", h.ImportOpenCodeAccounts)
	api.GET("/accounts/opencode/export", h.ExportOpenCodeAccounts)
	api.POST("/accounts/:id/opencode/sync", h.SyncOpenCodeAccount)
	api.POST("/accounts/opencode/sync", h.SyncAllOpenCodeAccounts)
}

func openCodeModeForRow(row *database.AccountRow, isOpenCode bool) string {
	if !isOpenCode || row == nil {
		return ""
	}
	return auth.NormalizeOpenCodeMode(row.GetCredential(auth.OpenCodeModeCredentialKey))
}

func openCodeRowString(row *database.AccountRow, key string, isOpenCode bool) string {
	if !isOpenCode || row == nil {
		return ""
	}
	return strings.TrimSpace(row.GetCredential(key))
}

func openCodeUsageForRow(row *database.AccountRow, isOpenCode bool) json.RawMessage {
	raw := openCodeRowString(row, auth.OpenCodeUsageCredentialKey, isOpenCode)
	if raw == "" || !json.Valid([]byte(raw)) {
		return nil
	}
	return json.RawMessage(raw)
}

func openCodeProtocolRulesForRow(row *database.AccountRow, isOpenCode bool) []auth.OpenCodeProtocolRule {
	raw := openCodeRowString(row, auth.OpenCodeProtocolRulesCredentialKey, isOpenCode)
	if raw == "" {
		return nil
	}
	rules, err := auth.ParseOpenCodeProtocolRules(raw)
	if err != nil {
		return nil
	}
	return rules
}

func openCodeCatalogModelsForRow(row *database.AccountRow, isOpenCode bool) []string {
	raw := openCodeRowString(row, auth.OpenCodeCatalogModelsCredentialKey, isOpenCode)
	if raw == "" {
		return nil
	}
	var models []string
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil
	}
	return models
}

func openCodeKeyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte("opencode\x00" + strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

func openCodeUsageCredential(snap *auth.OpenCodeUsageSnapshot) string {
	if snap == nil {
		return ""
	}
	encoded, _ := json.Marshal(snap)
	return string(encoded)
}

func openCodeCatalogModelsCredential(models []string) string {
	if len(models) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(models)
	return string(encoded)
}

func sanitizeOpenCodeCustomHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		name := strings.TrimSpace(k)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("custom_headers 包含非法换行符: %s", name)
		}
		if auth.IsOpenCodeReservedHeader(name) {
			return nil, fmt.Errorf("custom_headers 不允许覆盖网关保留头: %s", name)
		}
		out[name] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

type openCodeAccountPayload struct {
	Name             string                       `json:"name"`
	Email            string                       `json:"email"`
	APIKey           string                       `json:"api_key"`
	Mode             string                       `json:"mode"`
	BaseURL          string                       `json:"base_url"`
	AnthropicBaseURL string                       `json:"anthropic_base_url"`
	ProxyURL         string                       `json:"proxy_url"`
	Models           []string                     `json:"models"`
	ModelMapping     string                       `json:"model_mapping"`
	ProtocolRules    *[]auth.OpenCodeProtocolRule `json:"protocol_rules"`
	CustomHeaders    map[string]string            `json:"custom_headers"`
	AutoSync         *bool                        `json:"auto_sync"`
}

func (h *Handler) findDuplicateOpenCodeAccount(ctx context.Context, apiKey string, excludeID int64) (int64, error) {
	target := openCodeKeyFingerprint(apiKey)
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelOpenCode)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row == nil || row.ID == excludeID {
			continue
		}
		if existingKey := strings.TrimSpace(row.GetCredential("api_key")); existingKey != "" && openCodeKeyFingerprint(existingKey) == target {
			return row.ID, nil
		}
	}
	return 0, nil
}

func normalizeOpenCodeAccountModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		id := auth.StripOpenCodeModelPrefix(raw)
		if id == "" || auth.IsOpenCodeUnsupportedModel(id) {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

type openCodeCreateResult struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Mode     string   `json:"mode"`
	Models   int      `json:"models"`
	Warnings []string `json:"warnings,omitempty"`
}

func (h *Handler) createOpenCodeAccount(ctx context.Context, payload openCodeAccountPayload) (*openCodeCreateResult, int, error) {
	if h == nil || h.db == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("OpenCode 账号存储未初始化")
	}
	apiKey := strings.TrimSpace(payload.APIKey)
	if apiKey == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("api_key 不能为空")
	}
	if !auth.IsValidOpenCodeMode(payload.Mode) {
		return nil, http.StatusBadRequest, fmt.Errorf("mode 仅支持 go 或 zen")
	}
	mode := auth.NormalizeOpenCodeMode(payload.Mode)

	baseURL, err := auth.NormalizeOpenCodeBaseURL(payload.BaseURL)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	anthropicBaseURL, err := auth.NormalizeOpenCodeBaseURL(payload.AnthropicBaseURL)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("anthropic_base_url 无效: %w", err)
	}
	proxyURL := strings.TrimSpace(payload.ProxyURL)
	if proxyURL != "" {
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("代理 URL 无效")
		}
	}
	customHeaders, err := sanitizeOpenCodeCustomHeaders(payload.CustomHeaders)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	var parsedRules []auth.OpenCodeProtocolRule
	hasCustomRules := false
	if payload.ProtocolRules != nil {
		hasCustomRules = true
		parsedRules, err = auth.ParseOpenCodeProtocolRules(*payload.ProtocolRules)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	if existing, err := h.findDuplicateOpenCodeAccount(ctx, apiKey, 0); err != nil {
		return nil, http.StatusInternalServerError, err
	} else if existing > 0 {
		return nil, http.StatusConflict, fmt.Errorf("OpenCode API Key 已存在 (id=%d)", existing)
	}

	name := strings.TrimSpace(payload.Name)
	email := strings.TrimSpace(payload.Email)
	if name == "" {
		if email != "" {
			name = email
		} else {
			suffix := apiKey
			if len(suffix) > 6 {
				suffix = suffix[len(suffix)-6:]
			}
			name = fmt.Sprintf("opencode-%s-%s", mode, suffix)
		}
	}
	planType := "opencode go"
	if mode == auth.OpenCodeModeZen {
		planType = "opencode zen"
	}

	creds := map[string]any{
		"upstream_type":                auth.UpstreamOpenCode,
		"api_key":                      apiKey,
		auth.OpenCodeModeCredentialKey: mode,
		"plan_type":                    planType,
	}
	if email != "" {
		creds["email"] = email
	}
	if baseURL != "" {
		creds["base_url"] = baseURL
	}
	if anthropicBaseURL != "" {
		creds[auth.OpenCodeAnthropicBaseURLCredentialKey] = anthropicBaseURL
	}
	if hasCustomRules {
		creds[auth.OpenCodeProtocolRulesCredentialKey] = auth.EncodeOpenCodeProtocolRules(parsedRules)
	}
	if models := normalizeOpenCodeAccountModels(payload.Models); len(models) > 0 {
		creds["models"] = models
	}
	if mapping := strings.TrimSpace(payload.ModelMapping); mapping != "" {
		creds["model_mapping"] = mapping
	}
	if len(customHeaders) > 0 {
		creds["custom_headers"] = customHeaders
	}

	var warnings []string
	var syncedModels []string
	autoSync := payload.AutoSync == nil || *payload.AutoSync
	if autoSync {
		effectiveBase := baseURL
		if effectiveBase == "" {
			effectiveBase = auth.DefaultOpenCodeBaseURL(mode)
		}
		effectiveProxy := proxyURL
		if effectiveProxy == "" && h.store != nil {
			effectiveProxy = h.store.ResolveProxyForAccount(nil)
		}
		if models, fetchErr := auth.FetchOpenCodeModels(ctx, effectiveBase, apiKey, effectiveProxy, customHeaders); fetchErr != nil {
			warnings = append(warnings, "模型列表拉取失败: "+fetchErr.Error())
		} else if len(models) > 0 {
			syncedModels = models
			creds[auth.OpenCodeCatalogModelsCredentialKey] = openCodeCatalogModelsCredential(models)
		}
		if mode == auth.OpenCodeModeGo {
			if snap, usageErr := auth.FetchOpenCodeGoUsage(ctx, effectiveBase, apiKey, effectiveProxy, customHeaders); usageErr != nil {
				warnings = append(warnings, "Go 额度同步失败: "+usageErr.Error())
				if snap != nil {
					creds[auth.OpenCodeUsageCredentialKey] = openCodeUsageCredential(snap)
				}
			} else if snap != nil {
				creds[auth.OpenCodeUsageCredentialKey] = openCodeUsageCredential(snap)
			}
		}
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, creds, proxyURL)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("保存 OpenCode 账号失败: %w", err)
	}
	if h.store != nil {
		if loadErr := h.store.LoadAccountByID(ctx, id); loadErr != nil {
			log.Printf("[opencode] 加载新账号 %d 到号池失败: %v", id, loadErr)
		}
	}
	h.invalidateOpenCodeCatalogCaches()
	return &openCodeCreateResult{
		ID:       id,
		Name:     name,
		Mode:     mode,
		Models:   len(syncedModels),
		Warnings: warnings,
	}, http.StatusOK, nil
}

// AddOpenCodeAccount POST /api/accounts/opencode
func (h *Handler) AddOpenCodeAccount(c *gin.Context) {
	var req openCodeAccountPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	res, status, err := h.createOpenCodeAccount(c.Request.Context(), req)
	if err != nil {
		writeError(c, status, err.Error())
		return
	}
	c.JSON(http.StatusOK, res)
}

// UpdateOpenCodeAccount PUT /api/accounts/:id/opencode
func (h *Handler) UpdateOpenCodeAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	row, err := h.db.GetAccountByID(c.Request.Context(), id)
	if err != nil || row == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenCode) {
		writeError(c, http.StatusBadRequest, "该账号不是 OpenCode 渠道账号")
		return
	}
	var req struct {
		Name             *string                      `json:"name"`
		Email            *string                      `json:"email"`
		APIKey           *string                      `json:"api_key"`
		Mode             *string                      `json:"mode"`
		BaseURL          *string                      `json:"base_url"`
		AnthropicBaseURL *string                      `json:"anthropic_base_url"`
		ProxyURL         *string                      `json:"proxy_url"`
		Models           *[]string                    `json:"models"`
		ModelMapping     *string                      `json:"model_mapping"`
		ProtocolRules    *[]auth.OpenCodeProtocolRule `json:"protocol_rules"`
		ClearRules       bool                         `json:"clear_protocol_rules"`
		CustomHeaders    *map[string]string           `json:"custom_headers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	creds := map[string]any{}

	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		newKey := strings.TrimSpace(*req.APIKey)
		if existing, dupErr := h.findDuplicateOpenCodeAccount(c.Request.Context(), newKey, id); dupErr != nil {
			writeError(c, http.StatusInternalServerError, dupErr.Error())
			return
		} else if existing > 0 {
			writeError(c, http.StatusConflict, fmt.Sprintf("OpenCode API Key 已存在 (id=%d)", existing))
			return
		}
		creds["api_key"] = newKey
	}
	if req.Mode != nil {
		if !auth.IsValidOpenCodeMode(*req.Mode) {
			writeError(c, http.StatusBadRequest, "mode 仅支持 go 或 zen")
			return
		}
		mode := auth.NormalizeOpenCodeMode(*req.Mode)
		creds[auth.OpenCodeModeCredentialKey] = mode
		if mode == auth.OpenCodeModeZen {
			creds["plan_type"] = "opencode zen"
			creds[auth.OpenCodeUsageCredentialKey] = ""
		} else {
			creds["plan_type"] = "opencode go"
		}
	}
	if req.Email != nil {
		creds["email"] = strings.TrimSpace(*req.Email)
	}
	if req.BaseURL != nil {
		baseURL, err := auth.NormalizeOpenCodeBaseURL(*req.BaseURL)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		creds["base_url"] = baseURL
	}
	if req.AnthropicBaseURL != nil {
		anthropicBaseURL, err := auth.NormalizeOpenCodeBaseURL(*req.AnthropicBaseURL)
		if err != nil {
			writeError(c, http.StatusBadRequest, "anthropic_base_url 无效: "+err.Error())
			return
		}
		creds[auth.OpenCodeAnthropicBaseURLCredentialKey] = anthropicBaseURL
	}
	if req.ClearRules {
		creds[auth.OpenCodeProtocolRulesCredentialKey] = ""
	} else if req.ProtocolRules != nil {
		parsed, err := auth.ParseOpenCodeProtocolRules(*req.ProtocolRules)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		creds[auth.OpenCodeProtocolRulesCredentialKey] = auth.EncodeOpenCodeProtocolRules(parsed)
	}
	if req.Models != nil {
		creds["models"] = normalizeOpenCodeAccountModels(*req.Models)
	}
	if req.ModelMapping != nil {
		creds["model_mapping"] = strings.TrimSpace(*req.ModelMapping)
	}
	if req.CustomHeaders != nil {
		cleaned, err := sanitizeOpenCodeCustomHeaders(*req.CustomHeaders)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if cleaned == nil {
			cleaned = map[string]string{}
		}
		creds["custom_headers"] = cleaned
	}
	if req.ProxyURL != nil {
		proxyURL := strings.TrimSpace(*req.ProxyURL)
		if proxyURL != "" {
			if err := security.ValidateProxyURL(proxyURL); err != nil {
				writeError(c, http.StatusBadRequest, "代理 URL 无效")
				return
			}
		}
		if err := h.db.UpdateAccountProxyURL(c.Request.Context(), id, proxyURL); err != nil {
			writeError(c, http.StatusInternalServerError, "更新代理失败: "+err.Error())
			return
		}
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		if err := h.db.UpdateAccountName(c.Request.Context(), id, strings.TrimSpace(*req.Name)); err != nil {
			writeError(c, http.StatusInternalServerError, "更新名称失败: "+err.Error())
			return
		}
	}
	if len(creds) > 0 {
		if err := h.db.UpdateCredentials(c.Request.Context(), id, creds); err != nil {
			writeError(c, http.StatusInternalServerError, "更新 OpenCode 凭据失败: "+err.Error())
			return
		}
	}
	updated, err := h.db.GetAccountByID(c.Request.Context(), id)
	if err == nil && updated != nil && h.store != nil {
		h.store.ApplyOpenCodeAccountRow(updated)
	}
	h.invalidateOpenCodeCatalogCaches()
	c.JSON(http.StatusOK, gin.H{"id": id, "updated": true})
}

// parseOpenCodeImportEntries 兼容三种导入输入：
//  1. Sub2API 导出 JSON（{accounts: [{platform: "opencode_go", credentials: {...}}]}）
//  2. OpenCode 账号对象或数组 JSON（[{name, api_key, mode, base_url, ...}]）
//  3. 纯文本每行一条 API Key 或 "name,api_key"
func parseOpenCodeImportEntries(content any, defaultMode, defaultBaseURL, defaultAnthropicBaseURL, defaultProxyURL string) ([]openCodeAccountPayload, error) {
	if text, ok := content.(string); ok {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return nil, fmt.Errorf("导入内容为空")
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var decoded any
			if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
				return nil, fmt.Errorf("导入 JSON 解析失败: %w", err)
			}
			content = decoded
		} else {
			lines := strings.Split(trimmed, "\n")
			out := make([]openCodeAccountPayload, 0, len(lines))
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				var name, apiKey string
				if idx := strings.IndexAny(line, ",\t"); idx >= 0 {
					left := strings.TrimSpace(line[:idx])
					right := strings.TrimSpace(line[idx+1:])
					if right != "" {
						name, apiKey = left, right
					} else {
						apiKey = left
					}
				} else {
					apiKey = line
				}
				if apiKey == "" {
					continue
				}
				out = append(out, openCodeAccountPayload{
					Name:             name,
					APIKey:           apiKey,
					Mode:             defaultMode,
					BaseURL:          defaultBaseURL,
					AnthropicBaseURL: defaultAnthropicBaseURL,
					ProxyURL:         defaultProxyURL,
				})
			}
			if len(out) == 0 {
				return nil, fmt.Errorf("未识别到有效的 OpenCode API Key")
			}
			return out, nil
		}
	}

	var rawItems []any
	switch typed := content.(type) {
	case map[string]any:
		if list, ok := typed["accounts"].([]any); ok {
			rawItems = list
		} else {
			rawItems = []any{typed}
		}
	case []any:
		rawItems = typed
	default:
		return nil, fmt.Errorf("无法识别的导入格式")
	}

	strField := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}

	out := make([]openCodeAccountPayload, 0, len(rawItems))
	for i, raw := range rawItems {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("第 %d 项不是对象", i+1)
		}
		// 若是混合平台的 Sub2API 导出文件，自动跳过非 opencode/opencode_go 条目。
		if platform := strings.ToLower(strField(entry, "platform", "upstream_type")); platform != "" &&
			platform != "opencode" && platform != "opencode_go" && platform != "opencode-go" {
			continue
		}
		cred := entry
		if nested, ok := entry["credentials"].(map[string]any); ok {
			cred = nested
		}
		apiKey := strField(cred, "api_key", "apiKey", "key")
		if apiKey == "" {
			apiKey = strField(entry, "api_key", "apiKey", "key")
		}
		if apiKey == "" {
			return nil, fmt.Errorf("第 %d 项缺少 api_key", i+1)
		}
		mode := strField(cred, "opencode_mode", "account_mode", "mode")
		if mode == "" {
			mode = strField(entry, "opencode_mode", "account_mode", "mode")
		}
		if mode == "" {
			mode = defaultMode
		}
		baseURL := strField(cred, "base_url", "baseUrl")
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
		anthropicBaseURL := strField(cred, "opencode_anthropic_base_url", "anthropic_base_url", "anthropicBaseUrl")
		if anthropicBaseURL == "" {
			if urlsMap, ok := cred["api_base_urls"].(map[string]any); ok {
				if v, ok := urlsMap["anthropic"].(string); ok {
					anthropicBaseURL = strings.TrimSpace(v)
				}
			}
		}
		if anthropicBaseURL == "" {
			anthropicBaseURL = defaultAnthropicBaseURL
		}
		proxyURL := strField(entry, "proxy_url", "proxyUrl")
		if proxyURL == "" {
			proxyURL = defaultProxyURL
		}
		item := openCodeAccountPayload{
			Name:             strField(entry, "name"),
			Email:            firstNonEmptyAdminStr(strField(entry, "email"), strField(cred, "email")),
			APIKey:           apiKey,
			Mode:             mode,
			BaseURL:          baseURL,
			AnthropicBaseURL: anthropicBaseURL,
			ProxyURL:         proxyURL,
			ModelMapping:     firstNonEmptyAdminStr(strField(cred, "model_mapping"), strField(entry, "model_mapping")),
		}
		if rawRules := firstNonNil(cred["opencode_protocol_rules"], cred["protocol_rules"], entry["protocol_rules"]); rawRules != nil {
			if parsed, err := auth.ParseOpenCodeProtocolRules(rawRules); err == nil && len(parsed) > 0 {
				item.ProtocolRules = &parsed
			}
		}
		if rawModels, ok := cred["models"].([]any); ok {
			for _, m := range rawModels {
				if s, ok := m.(string); ok && strings.TrimSpace(s) != "" {
					item.Models = append(item.Models, strings.TrimSpace(s))
				}
			}
		}
		if rawHeaders, ok := cred["custom_headers"].(map[string]any); ok {
			item.CustomHeaders = make(map[string]string, len(rawHeaders))
			for hk, hv := range rawHeaders {
				if s, ok := hv.(string); ok {
					item.CustomHeaders[hk] = s
				}
			}
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未找到可导入的 OpenCode 账号")
	}
	return out, nil
}

func firstNonEmptyAdminStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// ImportOpenCodeAccounts POST /api/accounts/opencode/import
func (h *Handler) ImportOpenCodeAccounts(c *gin.Context) {
	var req struct {
		Content          any    `json:"content"`
		Mode             string `json:"mode"`
		BaseURL          string `json:"base_url"`
		AnthropicBaseURL string `json:"anthropic_base_url"`
		ProxyURL         string `json:"proxy_url"`
		AutoSync         *bool  `json:"auto_sync"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !auth.IsValidOpenCodeMode(req.Mode) {
		writeError(c, http.StatusBadRequest, "mode 仅支持 go 或 zen")
		return
	}
	entries, err := parseOpenCodeImportEntries(req.Content, req.Mode, req.BaseURL, req.AnthropicBaseURL, req.ProxyURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	type importItemResult struct {
		Index    int      `json:"index"`
		ID       int64    `json:"id,omitempty"`
		Name     string   `json:"name,omitempty"`
		Mode     string   `json:"mode,omitempty"`
		Status   string   `json:"status"`
		Error    string   `json:"error,omitempty"`
		Warnings []string `json:"warnings,omitempty"`
	}
	results := make([]importItemResult, 0, len(entries))
	imported, duplicates, failed := 0, 0, 0
	for i, item := range entries {
		item.AutoSync = req.AutoSync
		res, status, createErr := h.createOpenCodeAccount(c.Request.Context(), item)
		if createErr != nil {
			st := "failed"
			if status == http.StatusConflict {
				st = "duplicate"
				duplicates++
			} else {
				failed++
			}
			results = append(results, importItemResult{
				Index:  i + 1,
				Name:   item.Name,
				Mode:   auth.NormalizeOpenCodeMode(item.Mode),
				Status: st,
				Error:  createErr.Error(),
			})
			continue
		}
		imported++
		results = append(results, importItemResult{
			Index:    i + 1,
			ID:       res.ID,
			Name:     res.Name,
			Mode:     res.Mode,
			Status:   "imported",
			Warnings: res.Warnings,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"total":      len(entries),
		"imported":   imported,
		"duplicates": duplicates,
		"failed":     failed,
		"results":    results,
	})
}

// ExportOpenCodeAccounts GET /api/accounts/opencode/export
func (h *Handler) ExportOpenCodeAccounts(c *gin.Context) {
	rows, err := h.db.ListActiveByChannel(c.Request.Context(), database.UpstreamChannelOpenCode)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 OpenCode 账号失败: "+err.Error())
		return
	}
	idFilter := map[int64]bool{}
	if idsParam := strings.TrimSpace(c.Query("ids")); idsParam != "" {
		for _, part := range strings.Split(idsParam, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
				idFilter[id] = true
			}
		}
	}
	type exportEntry struct {
		ID               int64                       `json:"id"`
		Name             string                      `json:"name"`
		Email            string                      `json:"email,omitempty"`
		Platform         string                      `json:"platform"`
		Mode             string                      `json:"mode"`
		APIKey           string                      `json:"api_key"`
		BaseURL          string                      `json:"base_url,omitempty"`
		AnthropicBaseURL string                      `json:"anthropic_base_url,omitempty"`
		ProxyURL         string                      `json:"proxy_url,omitempty"`
		Models           []string                    `json:"models,omitempty"`
		ModelMapping     string                      `json:"model_mapping,omitempty"`
		ProtocolRules    []auth.OpenCodeProtocolRule `json:"protocol_rules,omitempty"`
		CustomHeaders    map[string]string           `json:"custom_headers,omitempty"`
	}
	exported := make([]exportEntry, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		if len(idFilter) > 0 && !idFilter[row.ID] {
			continue
		}
		exported = append(exported, exportEntry{
			ID:               row.ID,
			Name:             row.Name,
			Email:            strings.TrimSpace(row.GetCredential("email")),
			Platform:         auth.UpstreamOpenCode,
			Mode:             openCodeModeForRow(row, true),
			APIKey:           strings.TrimSpace(row.GetCredential("api_key")),
			BaseURL:          strings.TrimSpace(row.GetCredential("base_url")),
			AnthropicBaseURL: openCodeRowString(row, auth.OpenCodeAnthropicBaseURLCredentialKey, true),
			ProxyURL:         strings.TrimSpace(row.ProxyURL),
			Models:           row.GetCredentialStringSlice("models"),
			ModelMapping:     strings.TrimSpace(row.GetCredential("model_mapping")),
			ProtocolRules:    openCodeProtocolRulesForRow(row, true),
			CustomHeaders:    row.GetCredentialStringMap("custom_headers"),
		})
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    exported,
	}, "", "  ")
	filename := fmt.Sprintf("opencode-accounts-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

type openCodeSyncResult struct {
	ID       int64                       `json:"id"`
	Mode     string                      `json:"mode"`
	Models   []string                    `json:"models"`
	Usage    *auth.OpenCodeUsageSnapshot `json:"usage,omitempty"`
	Warnings []string                    `json:"warnings,omitempty"`
}

func (h *Handler) syncOpenCodeAccountByID(ctx context.Context, id int64) (*openCodeSyncResult, error) {
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil || row == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenCode) {
		return nil, fmt.Errorf("该账号不是 OpenCode 渠道账号")
	}
	apiKey := strings.TrimSpace(row.GetCredential("api_key"))
	if apiKey == "" {
		return nil, fmt.Errorf("账号缺少 api_key")
	}
	mode := openCodeModeForRow(row, true)
	baseURL := strings.TrimSpace(row.GetCredential("base_url"))
	if baseURL == "" {
		baseURL = auth.DefaultOpenCodeBaseURL(mode)
	}
	proxyURL := strings.TrimSpace(row.ProxyURL)
	if proxyURL == "" && h.store != nil {
		proxyURL = h.store.ResolveProxyForAccount(h.store.FindByID(id))
	}
	customHeaders := row.GetCredentialStringMap("custom_headers")

	creds := map[string]any{}

	var warnings []string
	var models []string
	modelsErr := error(nil)
	models, modelsErr = auth.FetchOpenCodeModels(ctx, baseURL, apiKey, proxyURL, customHeaders)
	if modelsErr != nil {
		warnings = append(warnings, "模型列表同步失败: "+modelsErr.Error())
		models = openCodeCatalogModelsForRow(row, true)
	} else {
		creds[auth.OpenCodeCatalogModelsCredentialKey] = openCodeCatalogModelsCredential(models)
	}

	var usage *auth.OpenCodeUsageSnapshot
	var usageErr error
	if mode == auth.OpenCodeModeGo {
		usage, usageErr = auth.FetchOpenCodeGoUsage(ctx, baseURL, apiKey, proxyURL, customHeaders)
		if usage != nil {
			creds[auth.OpenCodeUsageCredentialKey] = openCodeUsageCredential(usage)
		}
		if usageErr != nil {
			warnings = append(warnings, "Go 额度同步失败: "+usageErr.Error())
		}
	}

	if modelsErr != nil && (mode != auth.OpenCodeModeGo || usageErr != nil) {
		// 即使两者都失败，如果拿到了失败快照（如 401）也落库便于前端展示诊断。
		if len(creds) > 0 {
			_ = h.db.UpdateCredentials(ctx, id, creds)
			if updated, getErr := h.db.GetAccountByID(ctx, id); getErr == nil && updated != nil && h.store != nil {
				h.store.ApplyOpenCodeAccountRow(updated)
			}
			h.invalidateOpenCodeCatalogCaches()
		}
		return nil, fmt.Errorf("%s", strings.Join(warnings, "; "))
	}

	if len(creds) > 0 {
		if err := h.db.UpdateCredentials(ctx, id, creds); err != nil {
			return nil, fmt.Errorf("保存同步结果失败: %w", err)
		}
	}
	if updated, getErr := h.db.GetAccountByID(ctx, id); getErr == nil && updated != nil && h.store != nil {
		h.store.ApplyOpenCodeAccountRow(updated)
	}
	h.invalidateOpenCodeCatalogCaches()
	return &openCodeSyncResult{
		ID:       id,
		Mode:     mode,
		Models:   models,
		Usage:    usage,
		Warnings: warnings,
	}, nil
}

// SyncOpenCodeAccount POST /api/accounts/:id/opencode/sync
func (h *Handler) SyncOpenCodeAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	res, syncErr := h.syncOpenCodeAccountByID(c.Request.Context(), id)
	if syncErr != nil {
		writeError(c, http.StatusBadGateway, syncErr.Error())
		return
	}
	c.JSON(http.StatusOK, res)
}

// SyncAllOpenCodeAccounts POST /api/accounts/opencode/sync
func (h *Handler) SyncAllOpenCodeAccounts(c *gin.Context) {
	rows, err := h.db.ListActiveByChannel(c.Request.Context(), database.UpstreamChannelOpenCode)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 OpenCode 账号失败: "+err.Error())
		return
	}
	type itemResult struct {
		ID       int64    `json:"id"`
		Name     string   `json:"name"`
		Success  bool     `json:"success"`
		Models   int      `json:"models,omitempty"`
		Error    string   `json:"error,omitempty"`
		Warnings []string `json:"warnings,omitempty"`
	}
	results := make([]itemResult, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, row := range rows {
		if row == nil {
			continue
		}
		wg.Add(1)
		go func(idx int, r *database.AccountRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			syncRes, syncErr := h.syncOpenCodeAccountByID(c.Request.Context(), r.ID)
			if syncErr != nil {
				results[idx] = itemResult{ID: r.ID, Name: r.Name, Success: false, Error: syncErr.Error()}
				return
			}
			results[idx] = itemResult{
				ID:       r.ID,
				Name:     r.Name,
				Success:  true,
				Models:   len(syncRes.Models),
				Warnings: syncRes.Warnings,
			}
		}(i, row)
	}
	wg.Wait()
	succeeded, failed := 0, 0
	for _, r := range results {
		if r.ID == 0 {
			continue
		}
		if r.Success {
			succeeded++
		} else {
			failed++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"total":     succeeded + failed,
		"succeeded": succeeded,
		"failed":    failed,
		"results":   results,
	})
}

func openCodeConnectionTestModel(account *auth.Account, requested, channelDefault string) (string, error) {
	models := proxy.OpenCodeAccountModelIDs(account)
	requested = auth.StripOpenCodeModelPrefix(requested)
	if requested != "" {
		if auth.IsOpenCodeUnsupportedModel(requested) {
			return "", fmt.Errorf("OpenCode 标准网关不支持模型: %s", requested)
		}
		return requested, nil
	}
	if def := auth.StripOpenCodeModelPrefix(channelDefault); def != "" && !auth.IsOpenCodeUnsupportedModel(def) {
		return def, nil
	}
	for _, m := range models {
		if strings.EqualFold(m, auth.DefaultOpenCodeTestModel) {
			return m, nil
		}
	}
	if len(models) > 0 {
		return models[0], nil
	}
	return auth.DefaultOpenCodeTestModel, nil
}

func (h *Handler) openCodeChannelModels() []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(list []string) {
		for _, m := range list {
			m = strings.TrimSpace(m)
			key := strings.ToLower(m)
			if m == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, m)
		}
	}
	if h != nil && h.store != nil {
		for _, acc := range h.store.Accounts() {
			if acc != nil && acc.IsOpenCode() {
				add(proxy.OpenCodeAccountModelIDs(acc))
			}
		}
	}
	if len(out) == 0 {
		add(auth.DefaultOpenCodeGoModelIDs())
	}
	sort.Strings(out)
	return out
}

func (h *Handler) refreshOpenCodeChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelOpenCode, Added: []string{}}
	if h == nil || h.store == nil {
		return result
	}
	before := h.openCodeChannelModels()
	h.probePlanGroups(ctx, database.UpstreamChannelOpenCode, h.planGroupsFor(func(a *auth.Account) bool { return a.IsOpenCode() }), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			syncRes, err := h.syncOpenCodeAccountByID(ctx, group.Sample.ID())
			if err != nil {
				return 0, nil, err
			}
			return len(syncRes.Models), syncRes.Models, nil
		})
	result.Added = newlyAddedModels(before, h.openCodeChannelModels())
	return result
}

// StartOpenCodeSync 启动低频 OpenCode Go 后台额度同步（每 5 分钟检查一次活跃 Go 账号，
// 遵循最小 5 分钟下限、15 分钟最大等待窗口与 background_quiet_hours 静默时段）。
func (h *Handler) StartOpenCodeSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runDueOpenCodeGoUsageSync(ctx)
			}
		}
	}()
}

func (h *Handler) runDueOpenCodeGoUsageSync(ctx context.Context) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	now := time.Now()
	if h.store.BackgroundInspectionPaused(now) {
		return
	}
	accounts := h.store.Accounts()
	refreshed, failed := 0, 0
	for _, acc := range accounts {
		if acc == nil || !acc.IsOpenCode() || !acc.IsOpenCodeGoMode() {
			continue
		}
		if acc.RuntimeStatus() == "disabled" || acc.RuntimeStatus() == "error" {
			continue
		}
		cfg := acc.OpenCodeConfig()
		lastUsed := acc.GetLastUsedAt()
		if !auth.OpenCodeGoUsageAutoRefreshDue(cfg.Usage, lastUsed, now) {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		_, err := h.syncOpenCodeAccountByID(syncCtx, acc.ID())
		cancel()
		if err != nil {
			failed++
			log.Printf("[OpenCode 同步] 账号 %d 刷新失败: %v", acc.ID(), err)
			continue
		}
		refreshed++
	}
	if refreshed+failed > 0 {
		log.Printf("[OpenCode 同步] 完成: 成功 %d, 失败 %d", refreshed, failed)
	}
}
