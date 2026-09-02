package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	antigravityImportMaxAccounts        = 500
	antigravitySyncConcurrency          = 4
	antigravityImportedVerifiedEmailKey = "antigravity_import_verified_email"
	antigravityAccessDeniedError        = "Antigravity access denied by Google"
	antigravityNoUsableEgressError      = "Antigravity 没有可用代理出口"
)

type antigravityImportRequest struct {
	Files          []string        `json:"files"`
	RefreshTokens  []string        `json:"refresh_tokens"`
	ProxyURL       string          `json:"proxy_url"`
	OAuthClientKey string          `json:"oauth_client_key"`
	ClientID       string          `json:"client_id"`
	ClientSecret   string          `json:"client_secret"`
	GroupIDs       json.RawMessage `json:"group_ids"`
	// ImportProxy 决定文件里的 proxy_url 是否注册进代理表并同步代理池。
	//
	// 该渠道的导入一直会采用文件内代理（prepareAntigravityImport 就是这么写的），
	// 只是从不入表：账号绑的是一个代理池不认识的 URL，管理页看不见、也进不了轮转。
	// 开关只控制"是否入表"，关闭时维持既有行为，不改变代理的取用优先级。
	ImportProxy bool `json:"import_proxy"`
}

type addAntigravityAccountRequest struct {
	Name           string          `json:"name"`
	AuthKind       string          `json:"auth_kind"`
	AuthJSON       string          `json:"auth_json"`
	APIKey         string          `json:"api_key"`
	Models         []string        `json:"models"`
	ModelMapping   string          `json:"model_mapping"`
	ProxyURL       string          `json:"proxy_url"`
	OAuthClientKey string          `json:"oauth_client_key"`
	ClientID       string          `json:"client_id"`
	ClientSecret   string          `json:"client_secret"`
	GroupIDs       json.RawMessage `json:"group_ids"`
}

type updateAntigravityAccountRequest struct {
	Name         *string         `json:"name"`
	AuthJSON     string          `json:"auth_json"`
	APIKey       *string         `json:"api_key"`
	Models       []string        `json:"models"`
	ModelMapping *string         `json:"model_mapping"`
	ProxyURL     *string         `json:"proxy_url"`
	GroupIDs     json.RawMessage `json:"group_ids"`
}

// antigravityModelsForPersistence keeps the durable account catalog in the
// provider's raw/wire namespace. Public IDs are accepted by admin forms, but
// must be resolved before the scheduler observes the updated credential.
func antigravityModelsForPersistence(models []string) []string {
	models = auth.NormalizeAccountModels(models)
	resolved := make([]string, 0, len(models))
	for _, model := range models {
		wireModels := proxy.AntigravityWireModelIDs(model)
		if len(wireModels) == 0 {
			resolved = append(resolved, model)
			continue
		}
		resolved = append(resolved, wireModels...)
	}
	return auth.NormalizeAccountModels(resolved)
}

func antigravityPublishedModels(rawModels []string) []string {
	return proxy.AntigravityPublishedModelIDs(auth.NormalizeAccountModels(rawModels))
}

func antigravityPublishedModelsOrDefault(rawModels []string) []string {
	if len(rawModels) == 0 {
		rawModels = auth.AntigravityDefaultModelIDs()
	}
	return antigravityPublishedModels(rawModels)
}

// antigravityPublishedModelForObservation maps a persisted physical model
// observation back to one of the logical models actually advertised by the
// account. Unlike catalog publication, a single observation naturally names
// only one backing and therefore must not be passed to PublishedModelIDs on
// its own (logical Gemini models require their complete backing set there).
func antigravityPublishedModelForObservation(model string, publishedModels []string) (string, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "", false
	}
	for _, publishedModel := range publishedModels {
		publishedModel = strings.ToLower(strings.TrimSpace(publishedModel))
		if publishedModel == model {
			return publishedModel, true
		}
		for _, wireModel := range proxy.AntigravityWireModelIDs(publishedModel) {
			if strings.EqualFold(strings.TrimSpace(wireModel), model) {
				return publishedModel, true
			}
		}
	}
	return "", false
}

// antigravityPublishedQuota clones a persisted raw quota snapshot into the
// public model namespace. A logical model may be backed by several provider
// wire models, but the admin response exposes exactly one quota entry for it.
func antigravityPublishedQuota(raw auth.AntigravityQuotaSnapshot) auth.AntigravityQuotaSnapshot {
	projected := raw
	projected.Models = []auth.AntigravityModelQuota{}
	// Forwarding rules are provider-wire implementation details and are not an
	// admin-facing model contract.
	projected.ModelForwardingRules = nil

	rawModels := make([]string, 0, len(raw.Models))
	byWireID := make(map[string]auth.AntigravityModelQuota, len(raw.Models))
	for _, model := range raw.Models {
		wireID := strings.ToLower(strings.TrimSpace(model.ModelID))
		if wireID == "" {
			continue
		}
		rawModels = append(rawModels, wireID)
		if _, exists := byWireID[wireID]; !exists {
			byWireID[wireID] = model
		}
	}
	for _, publicID := range antigravityPublishedModels(rawModels) {
		// Prefer the logical model's default effort backing so the quota shown in
		// the account UI matches an effort-less request. If that backing was not
		// returned by the provider, fall back to any available backing for the
		// same logical model instead of dropping it from the quota projection.
		wireIDs := proxy.AntigravityWireModelIDs(publicID)
		if defaultWireID, ok := proxy.AntigravityWireModelID(publicID); ok {
			wireIDs = append([]string{defaultWireID}, wireIDs...)
		}
		var (
			model auth.AntigravityModelQuota
			found bool
		)
		for _, wireID := range wireIDs {
			model, found = byWireID[strings.ToLower(strings.TrimSpace(wireID))]
			if found {
				break
			}
		}
		if !found {
			continue
		}
		model.ModelID = publicID
		model.DisplayName = publicID
		projected.Models = append(projected.Models, model)
	}
	return projected
}

func antigravityPublishedQuotaJSON(raw string) json.RawMessage {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var quota auth.AntigravityQuotaSnapshot
	if json.Unmarshal([]byte(raw), &quota) != nil {
		return nil
	}
	encoded, err := json.Marshal(antigravityPublishedQuota(quota))
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}

type antigravityImportItem struct {
	Index    int    `json:"index"`
	SubIndex int    `json:"sub_index,omitempty"`
	ID       int64  `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	OK       bool   `json:"ok"`
	Synced   bool   `json:"synced"`
	Warning  string `json:"warning,omitempty"`
	Error    string `json:"error,omitempty"`
}

type antigravityRefreshRequest struct {
	IDs []int64 `json:"ids"`
}

type antigravityRefreshItem struct {
	ID      int64  `json:"id"`
	Email   string `json:"email,omitempty"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

type antigravitySyncJob struct {
	Index       int // compact worker/result slot
	SourceIndex int // original input file/token index (1-based)
	SubIndex    int // credential ordinal within the original input (1-based)
	Credential  auth.AntigravityCredential
	ProxyURL    string
	AccountID   int64
}

type antigravitySyncJobResult struct {
	Job    antigravitySyncJob
	Result auth.AntigravitySyncResult
	Err    error
}

type parsedAntigravityCredential struct {
	Document    antigravityImportDocument
	Credential  auth.AntigravityCredential
	SourceIndex int
	SubIndex    int
}

type preparedAntigravityImport struct {
	Parsed            parsedAntigravityCredential
	Document          antigravityImportDocument
	Name              string
	ProxyURL          string
	EffectiveProxyURL string
	Models            []string
	ModelsPresent     bool
	ModelMapping      string
	ValidationError   string
}

// parseAntigravityImportInputs keeps the response index tied to the original
// input item. A single export may contain multiple accounts, so the optional
// sub-index disambiguates those credentials without renumbering later files
// when an earlier file fails to parse.
func parseAntigravityImportInputs(contents []string, defaults antigravityImportDefaults) ([]parsedAntigravityCredential, []antigravityImportItem, error) {
	parsedCredentials := make([]parsedAntigravityCredential, 0, len(contents))
	parseItems := make([]antigravityImportItem, 0)
	for contentIndex, content := range contents {
		parsed, err := parseAntigravityImportDocuments(content, defaults)
		if err != nil {
			parseItems = append(parseItems, antigravityImportItem{Index: contentIndex + 1, Error: err.Error()})
			continue
		}
		for subIndex, document := range parsed {
			parsedCredentials = append(parsedCredentials, parsedAntigravityCredential{
				Document: document, Credential: document.Credential,
				SourceIndex: contentIndex + 1, SubIndex: subIndex + 1,
			})
		}
		if len(parsedCredentials) > antigravityImportMaxAccounts {
			return nil, nil, fmt.Errorf("单次最多导入 %d 个账号", antigravityImportMaxAccounts)
		}
	}
	return parsedCredentials, parseItems, nil
}

func prepareAntigravityImport(parsed parsedAntigravityCredential, defaultProxyURL string) preparedAntigravityImport {
	document := parsed.Document
	prepared := preparedAntigravityImport{
		Parsed: parsed, Document: document, ModelsPresent: document.Models != nil,
	}
	name, err := normalizeAntigravityAccountName(document.Name)
	if err != nil {
		prepared.ValidationError = err.Error()
		return prepared
	}
	prepared.Name = name

	proxyURL := strings.TrimSpace(document.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(defaultProxyURL)
	}
	proxyURL = security.SanitizeInput(proxyURL)
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		prepared.ValidationError = "代理 URL 无效"
		return prepared
	}
	prepared.ProxyURL = proxyURL

	prepared.Models = auth.NormalizeAccountModels(document.Models)
	for _, model := range prepared.Models {
		if err := security.ValidateModelName(model); err != nil {
			prepared.ValidationError = "模型名称无效: " + model
			return prepared
		}
	}
	prepared.ModelMapping, err = normalizeAccountModelMapping(document.ModelMapping)
	if err != nil {
		prepared.ValidationError = err.Error()
	}
	return prepared
}

func normalizeAntigravityAccountName(value string) (string, error) {
	value = security.SanitizeInput(strings.TrimSpace(value))
	if security.ContainsXSS(value) || security.ContainsSQLInjection(value) {
		return "", errors.New("名称包含非法字符")
	}
	if utf8.RuneCountInString(value) > 100 {
		return "", errors.New("名称长度不能超过100字符")
	}
	return value, nil
}

func (h *Handler) resolveAntigravityGroupIDs(ctx context.Context, raw json.RawMessage) ([]int64, error) {
	groupIDs, err := h.resolveImportGroupIDsJSON(ctx, raw)
	if err != nil {
		return nil, err
	}
	if len(groupIDs) == 0 {
		return nil, nil
	}
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("校验分组渠道失败: %w", err)
	}
	byID := make(map[int64]database.AccountGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	for _, groupID := range groupIDs {
		group := byID[groupID]
		channel := database.NormalizeAccountGroupChannel(group.Channel)
		if channel != database.AccountGroupChannelAntigravity {
			return nil, fmt.Errorf("分组「%s」是 %s 渠道分组,不能加入 Antigravity 账号",
				group.Name, groupChannelDisplayName(channel))
		}
	}
	return groupIDs, nil
}

func antigravityRowFamilyID(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	// The dedicated column is the canonical fencing value. The JSON field is
	// retained for compatibility with older readers only.
	if familyID := strings.TrimSpace(row.CredentialFamilyID); familyID != "" {
		return familyID
	}
	return strings.TrimSpace(row.GetCredential("credential_family_id"))
}

func findAntigravityDuplicateAccountID(rows []*database.AccountRow, familyID, email, profileID string, excludeID int64) int64 {
	familyID = strings.TrimSpace(familyID)
	email = strings.ToLower(strings.TrimSpace(email))
	profileID = strings.TrimSpace(profileID)
	for _, row := range rows {
		if row == nil || row.ID == excludeID {
			continue
		}
		if familyID != "" && antigravityRowFamilyID(row) == familyID {
			return row.ID
		}
		if profileID != "" {
			// Once a verified Google subject is available, an email match alone
			// must not block a distinct subject. Subject/family are authoritative.
			if strings.TrimSpace(row.GetCredential("account_id")) == profileID {
				return row.ID
			}
			continue
		}
		if email != "" && strings.TrimSpace(row.GetCredential("account_id")) == "" && strings.EqualFold(strings.TrimSpace(row.GetCredential("email")), email) {
			return row.ID
		}
	}
	return 0
}

func appendAntigravityWarning(current, next string) string {
	current = strings.TrimSpace(current)
	next = strings.TrimSpace(next)
	if current == "" {
		return next
	}
	if next == "" {
		return current
	}
	return current + "; " + next
}

func antigravityAuthoritativeProfile(profile auth.AntigravityProfile) bool {
	return profile.VerifiedEmail && strings.TrimSpace(profile.ID) != "" && strings.TrimSpace(profile.Email) != ""
}

func antigravityAccessDeniedReason(result auth.AntigravitySyncResult) string {
	if result.Quota.Forbidden {
		return "Google quota API denied access"
	}
	if result.EntitlementsObserved && !result.Entitlements.Allowed {
		if reason := strings.TrimSpace(result.Entitlements.Reason); reason != "" {
			return reason
		}
		return "Google permissions denied access"
	}
	return ""
}

// applyAntigravityAccessFence projects authoritative Google access facts onto
// the existing persistent account-status hard fence. A failed sync may only
// add a fence when it observed an explicit denial; only a fully successful
// sync is allowed to clear an earlier denial.
func (h *Handler) applyAntigravityAccessFence(ctx context.Context, id int64, result auth.AntigravitySyncResult, syncSucceeded bool) (bool, error) {
	if h == nil || h.db == nil || id <= 0 {
		return false, nil
	}
	if reason := antigravityAccessDeniedReason(result); reason != "" {
		return h.db.SetOwnedAccountError(ctx, id, antigravityAccessDeniedError, antigravityAccessDeniedError+": "+reason)
	}
	if syncSucceeded {
		return h.db.ClearOwnedAccountError(ctx, id, antigravityAccessDeniedError)
	}
	return false, nil
}

func (h *Handler) removeAntigravityRuntimeAccount(id int64) {
	if h != nil && h.store != nil && id > 0 {
		h.store.RemoveAccount(id)
	}
}

func (h *Handler) reloadAntigravityRuntimeAccount(ctx context.Context, id int64) error {
	if h.store == nil || id <= 0 {
		return nil
	}
	h.store.RemoveAccount(id)
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		// LoadAccountByID should publish only after a complete build, but remove
		// again defensively so future loader changes cannot leave a partial or
		// stale account dispatchable after a failed control-plane mutation.
		h.store.RemoveAccount(id)
		return err
	}
	return nil
}

// reconcileAntigravityRuntimeAfterCAS publishes a durable concurrent/ambiguous
// winner only when its generation demonstrably advanced beyond the caller's
// snapshot. Re-reading the same generation after provider token progress would
// revive the potentially consumed refresh token, so that case is removed.
func (h *Handler) reconcileAntigravityRuntimeAfterCAS(ctx context.Context, id, expectedGeneration int64) (bool, error) {
	current, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		h.removeAntigravityRuntimeAccount(id)
		return false, err
	}
	if current.CredentialGeneration <= expectedGeneration {
		h.removeAntigravityRuntimeAccount(id)
		return false, nil
	}
	if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
		h.removeAntigravityRuntimeAccount(id)
		return false, err
	}
	return true, nil
}

// resolveAntigravityControlPlaneProxy applies the same account > group > pool
// > global > direct policy as inference. New accounts use DBID 0 with their
// requested proxy and target groups; existing callers pass the persisted ID
// and memberships loaded from the database.
func (h *Handler) resolveAntigravityControlPlaneProxy(accountID int64, accountProxyURL string, groupIDs []int64) (string, error) {
	accountProxyURL = strings.TrimSpace(accountProxyURL)
	if h == nil || h.store == nil {
		return accountProxyURL, nil
	}
	candidate := &auth.Account{
		DBID: accountID, ProxyURL: accountProxyURL,
		GroupIDs: append([]int64(nil), groupIDs...),
	}
	resolvedProxyURL, usable := h.store.ResolveUsableProxyForAccount(candidate)
	if !usable {
		return "", errors.New(antigravityNoUsableEgressError)
	}
	return strings.TrimSpace(resolvedProxyURL), nil
}

// AddAntigravityAccount imports one credential document and immediately syncs
// its Google identity, permissions, and quota snapshot.
func (h *Handler) AddAntigravityAccount(c *gin.Context) {
	var req addAntigravityAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	name, err := normalizeAntigravityAccountName(req.Name)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.ProxyURL = security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理 URL 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 120*time.Second)
	defer cancel()
	groupIDs, err := h.resolveAntigravityGroupIDs(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.AuthKind), auth.AntigravityAuthKindAPIKey) {
		h.addAntigravityAPIKeyAccount(c, ctx, req, name, groupIDs)
		return
	}
	effectiveProxyURL, err := h.resolveAntigravityControlPlaneProxy(0, req.ProxyURL, groupIDs)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, err.Error())
		return
	}
	defaults := antigravityImportDefaults{
		OAuthClientKey: strings.TrimSpace(req.OAuthClientKey),
		ClientID:       strings.TrimSpace(req.ClientID),
		ClientSecret:   strings.TrimSpace(req.ClientSecret),
	}
	credentials, err := parseAntigravityImportContent(req.AuthJSON, defaults)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(credentials) != 1 {
		writeError(c, http.StatusBadRequest, "单账号添加仅接受一份 Antigravity 凭据")
		return
	}
	credential := credentials[0]
	credential.ProjectID = ""
	credential.IDToken = ""
	client, err := auth.NewAntigravityClient(effectiveProxyURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	result, syncErr := client.Sync(ctx, credential)
	if syncErr == nil && !antigravityAuthoritativeProfile(result.Profile) {
		syncErr = errors.New("Antigravity sync did not return a verified Google subject")
	}
	if result.Credential.AccessToken != "" || result.Credential.RefreshToken != "" {
		credential = result.Credential
	}
	familyID := antigravityCredentialFamilyID(credential, result.Profile.ID)
	if familyID == "" {
		familyID = antigravityCredentialProvisionalFamilyID(credential)
	}
	email := strings.TrimSpace(result.Profile.Email)
	if email == "" {
		email = strings.TrimSpace(credential.Email)
	}
	credentialsMap, err := antigravityCredentialsForInsert(credential, familyID, result, syncErr)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if name == "" {
		name = strings.TrimSpace(result.Profile.Name)
	}
	if name == "" {
		name = strings.TrimSpace(credential.Name)
	}
	if name == "" {
		name = email
	}
	if name == "" {
		name = "antigravity"
	}

	h.mergeDuplicateMu.Lock()
	rows, listErr := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if listErr != nil {
		h.mergeDuplicateMu.Unlock()
		writeInternalError(c, listErr)
		return
	}
	if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, email, result.Profile.ID, 0); duplicateID > 0 {
		h.mergeDuplicateMu.Unlock()
		writeError(c, http.StatusConflict, fmt.Sprintf("Antigravity 凭据身份已存在 (id=%d)", duplicateID))
		return
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentialsMap, req.ProxyURL)
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		writeInternalError(c, err)
		return
	}

	warning := result.Warning
	if syncErr != nil {
		warning = appendAntigravityWarning(warning, syncErr.Error())
	}
	if reason := antigravityAccessDeniedReason(result); reason != "" {
		warning = appendAntigravityWarning(warning, antigravityAccessDeniedError+": "+reason)
	}
	runtimeBlocked := false
	if _, fenceErr := h.applyAntigravityAccessFence(ctx, id, result, syncErr == nil); fenceErr != nil {
		warning = appendAntigravityWarning(warning, "访问状态持久化失败: "+fenceErr.Error())
		runtimeBlocked = true
		h.removeAntigravityRuntimeAccount(id)
	}
	if err := h.bindImportedAccountGroups(ctx, []int64{id}, groupIDs); err != nil {
		warning = appendAntigravityWarning(warning, "分组绑定失败: "+err.Error())
	}
	if !runtimeBlocked {
		if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
			log.Printf("加载 Antigravity 账号 %d 到运行时失败: %v", id, err)
			warning = appendAntigravityWarning(warning, "运行时加载失败: "+err.Error())
		}
	}
	h.db.InsertAccountEventAsync(id, "added", "manual_antigravity")
	security.SecurityAuditLog("ANTIGRAVITY_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d synced=%t ip=%s", id, syncErr == nil, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"message": "成功添加 Antigravity 账号", "id": id, "email": email,
		"synced": syncErr == nil, "warning": warning, "group_ids": groupIDs,
	})
}

func (h *Handler) addAntigravityAPIKeyAccount(c *gin.Context, ctx context.Context, req addAntigravityAccountRequest, name string, groupIDs []int64) {
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		writeError(c, http.StatusBadRequest, "API Key 是必填字段")
		return
	}
	requestedModels := auth.NormalizeAccountModels(req.Models)
	for _, model := range requestedModels {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, "模型名称无效: "+model)
			return
		}
	}
	models := antigravityModelsForPersistence(requestedModels)
	mapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if name == "" {
		name = "antigravity-api-key"
	}
	familyID := antigravityAPIKeyFamilyID(apiKey)
	credentials := map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": apiKey,
		"plan_type": "api", "email": "google-api-key", "credential_family_id": familyID,
		"models": models, "model_mapping": mapping,
	}
	h.mergeDuplicateMu.Lock()
	rows, listErr := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if listErr == nil {
		for _, row := range rows {
			if antigravityRowFamilyID(row) == familyID {
				h.mergeDuplicateMu.Unlock()
				writeError(c, http.StatusConflict, "Antigravity API Key 已存在")
				return
			}
		}
	}
	if listErr != nil {
		h.mergeDuplicateMu.Unlock()
		writeInternalError(c, listErr)
		return
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentials, req.ProxyURL)
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	warning := ""
	if err := h.bindImportedAccountGroups(ctx, []int64{id}, groupIDs); err != nil {
		warning = "分组绑定失败: " + err.Error()
	}
	if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
		warning = appendAntigravityWarning(warning, "运行时加载失败: "+err.Error())
	}
	h.db.InsertAccountEventAsync(id, "added", "manual_antigravity_api_key")
	c.JSON(http.StatusOK, gin.H{"message": "成功添加 Antigravity API Key", "id": id, "email": "google-api-key", "synced": true, "warning": warning, "group_ids": groupIDs})
}

type batchUpdateAntigravityModelsRequest struct {
	IDs    []int64  `json:"ids"`
	Models []string `json:"models"`
}

func (h *Handler) FetchAntigravityModels(c *gin.Context) {
	var req addAntigravityAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	requestedModels := auth.NormalizeAccountModels(req.Models)
	for _, model := range requestedModels {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, "模型名称无效: "+model)
			return
		}
	}
	models := antigravityPublishedModelsOrDefault(antigravityModelsForPersistence(requestedModels))
	c.JSON(http.StatusOK, gin.H{"models": models})
}

func (h *Handler) BatchUpdateAntigravityModels(c *gin.Context) {
	var req batchUpdateAntigravityModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要更新的账号 ID")
		return
	}
	requestedModels := auth.NormalizeAccountModels(req.Models)
	for _, model := range requestedModels {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, "模型名称无效: "+model)
			return
		}
	}
	models := antigravityModelsForPersistence(requestedModels)
	publishedModels := antigravityPublishedModels(models)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	success, failed := 0, 0
	for _, id := range ids {
		row, err := h.db.GetAccountByID(ctx, id)
		if err != nil || !strings.EqualFold(row.GetCredential("upstream_type"), auth.UpstreamAntigravity) {
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
		success++
	}
	c.JSON(http.StatusOK, gin.H{"success": success, "failed": failed, "models": publishedModels})
}

// UpdateAntigravityAccount updates editable metadata and optionally replaces
// the OAuth credential. Credential progress is fenced even when a later sync
// stage fails, so a rotated refresh token is not discarded.
func (h *Handler) UpdateAntigravityAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var req updateAntigravityAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Name != nil {
		value, nameErr := normalizeAntigravityAccountName(*req.Name)
		if nameErr != nil {
			writeError(c, http.StatusBadRequest, nameErr.Error())
			return
		}
		req.Name = &value
	}
	if req.ProxyURL != nil {
		value := security.SanitizeInput(strings.TrimSpace(*req.ProxyURL))
		if err := security.ValidateProxyURL(value); err != nil {
			writeError(c, http.StatusBadRequest, "代理 URL 无效")
			return
		}
		req.ProxyURL = &value
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 120*time.Second)
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
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamAntigravity) {
		writeError(c, http.StatusBadRequest, "仅 Antigravity 账号支持该设置")
		return
	}
	groupUpdate, err := parseOptionalIntegerSliceField(req.GroupIDs, "group_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if groupUpdate.Set {
		groupUpdate.Values, err = h.resolveAntigravityGroupIDs(ctx, req.GroupIDs)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(row.GetCredential("api_key")) != "" {
		h.updateAntigravityAPIKeyAccount(c, ctx, row, req, groupUpdate)
		return
	}

	var syncResult auth.AntigravitySyncResult
	credentialsChanged := false
	accessWarning := ""
	if strings.TrimSpace(req.AuthJSON) != "" {
		var effectiveGroupIDs []int64
		if groupUpdate.Set {
			effectiveGroupIDs = append([]int64(nil), groupUpdate.Values...)
		} else {
			effectiveGroupIDs, err = h.db.GetAccountGroupIDs(ctx, row.ID)
			if err != nil {
				writeInternalError(c, err)
				return
			}
		}
		effectiveAccountProxyURL := strings.TrimSpace(row.ProxyURL)
		if req.ProxyURL != nil {
			effectiveAccountProxyURL = *req.ProxyURL
		}
		effectiveProxyURL, resolveErr := h.resolveAntigravityControlPlaneProxy(row.ID, effectiveAccountProxyURL, effectiveGroupIDs)
		if resolveErr != nil {
			writeError(c, http.StatusServiceUnavailable, resolveErr.Error())
			return
		}
		parsed, parseErr := parseAntigravityImportContent(req.AuthJSON, antigravityImportDefaults{
			OAuthClientKey: row.GetCredential("oauth_client_key"),
			ClientID:       row.GetCredential("antigravity_client_id"),
			ClientSecret:   row.GetCredential("antigravity_client_secret"),
		})
		if parseErr != nil {
			writeError(c, http.StatusBadRequest, parseErr.Error())
			return
		}
		if len(parsed) != 1 {
			writeError(c, http.StatusBadRequest, "凭据替换仅接受一份 Antigravity 凭据")
			return
		}
		parsed[0].ProjectID = ""
		parsed[0].IDToken = ""
		client, clientErr := auth.NewAntigravityClient(effectiveProxyURL)
		if clientErr != nil {
			writeError(c, http.StatusBadRequest, clientErr.Error())
			return
		}
		preSyncFamilyID := antigravityCredentialFamilyID(parsed[0], "")
		syncResult, err = client.Sync(ctx, parsed[0])
		if err == nil && !antigravityAuthoritativeProfile(syncResult.Profile) {
			err = errors.New("Antigravity sync did not return a verified Google subject")
		}
		if err != nil {
			providerProgress := antigravityFailureHasProgress(row, syncResult)
			persistOutcome := h.persistFailedAntigravitySync(ctx, row, syncResult, err, preSyncFamilyID)
			warning := persistOutcome.Warning
			var fenceErr error
			if persistOutcome.ProgressPersisted || (!providerProgress && persistOutcome.Mutated) {
				h.invalidateAccountSnapshotCaches()
				_, fenceErr = h.applyAntigravityAccessFence(ctx, id, syncResult, false)
			}
			if reason := antigravityAccessDeniedReason(syncResult); reason != "" {
				warning = appendAntigravityWarning(warning, antigravityAccessDeniedError+": "+reason)
			}
			if fenceErr != nil {
				warning = appendAntigravityWarning(warning, "访问状态持久化失败: "+fenceErr.Error())
				h.removeAntigravityRuntimeAccount(id)
			} else if persistOutcome.ProgressPersisted || (!providerProgress && persistOutcome.Mutated) {
				if reloadErr := h.reloadAntigravityRuntimeAccount(ctx, id); reloadErr != nil {
					log.Printf("保存失败同步结果后重载 Antigravity 账号 %d 运行时状态失败: %v", id, reloadErr)
					warning = appendAntigravityWarning(warning, "运行时重载失败: "+reloadErr.Error())
				}
			} else if persistOutcome.ConcurrentWinner {
				if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
					warning = appendAntigravityWarning(warning, "运行时重载失败: "+reconcileErr.Error())
				}
			} else if providerProgress {
				// Google may already have consumed/rotated the source refresh token.
				// Without a durable progress write, the old runtime credential is unsafe.
				h.removeAntigravityRuntimeAccount(id)
			}
			message := appendAntigravityWarning(err.Error(), warning)
			writeError(c, http.StatusBadGateway, message)
			return
		}
		familyID := antigravityCredentialFamilyID(syncResult.Credential, syncResult.Profile.ID)
		updates, updateErr := antigravityCredentialUpdates(syncResult, row)
		if updateErr != nil {
			writeInternalError(c, updateErr)
			return
		}
		updates["credential_family_id"] = familyID

		h.mergeDuplicateMu.Lock()
		rows, listErr := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
		if listErr == nil {
			if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, syncResult.Profile.Email, syncResult.Profile.ID, id); duplicateID > 0 {
				h.mergeDuplicateMu.Unlock()
				writeError(c, http.StatusConflict, fmt.Sprintf("Antigravity 凭据身份已存在 (id=%d)", duplicateID))
				return
			}
		}
		if listErr != nil {
			h.mergeDuplicateMu.Unlock()
			writeInternalError(c, listErr)
			return
		}
		_, applied, casErr := h.db.ReplaceAccountCredentialsCAS(ctx, id, row.CredentialGeneration, familyID, updates)
		h.mergeDuplicateMu.Unlock()
		if casErr != nil {
			if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
				log.Printf("Antigravity 凭据替换 CAS 错误后无法确认 durable state (account=%d): %v", id, reconcileErr)
			}
			writeInternalError(c, casErr)
			return
		}
		if !applied {
			if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
				log.Printf("Antigravity 凭据替换 CAS 冲突后重载 durable winner 失败 (account=%d): %v", id, reconcileErr)
			}
			writeError(c, http.StatusConflict, "账号凭据已被其他操作更新，请重试")
			return
		}
		credentialsChanged = true
		if _, fenceErr := h.applyAntigravityAccessFence(ctx, id, syncResult, true); fenceErr != nil {
			h.removeAntigravityRuntimeAccount(id)
			writeInternalError(c, fmt.Errorf("持久化 Antigravity 访问状态失败: %w", fenceErr))
			return
		}
		if reason := antigravityAccessDeniedReason(syncResult); reason != "" {
			accessWarning = antigravityAccessDeniedError + ": " + reason
		}
	}
	runtimeMutationPersisted := credentialsChanged
	reloadAfterPartialMutation := func() {
		if !runtimeMutationPersisted {
			return
		}
		if reloadErr := h.reloadAntigravityRuntimeAccount(ctx, id); reloadErr != nil {
			log.Printf("Antigravity 部分编辑落库后重载 durable state 失败 (account=%d): %v", id, reloadErr)
			h.removeAntigravityRuntimeAccount(id)
		}
	}
	if req.ProxyURL != nil {
		if err := h.db.UpdateAccountProxyURL(ctx, id, *req.ProxyURL); err != nil {
			reloadAfterPartialMutation()
			writeInternalError(c, err)
			return
		}
		runtimeMutationPersisted = true
	}
	if req.Name != nil {
		if err := h.db.UpdateAccountName(ctx, id, *req.Name); err != nil {
			reloadAfterPartialMutation()
			writeInternalError(c, err)
			return
		}
		runtimeMutationPersisted = true
	} else if credentialsChanged && strings.TrimSpace(syncResult.Profile.Name) != "" {
		if nameErr := h.db.UpdateAccountName(ctx, id, syncResult.Profile.Name); nameErr == nil {
			runtimeMutationPersisted = true
		}
	}
	if groupUpdate.Set {
		if err := h.db.SetAccountGroups(ctx, id, groupUpdate.Values); err != nil {
			reloadAfterPartialMutation()
			writeInternalError(c, err)
			return
		}
		runtimeMutationPersisted = true
	}
	warning := appendAntigravityWarning(syncResult.Warning, accessWarning)
	if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
		log.Printf("重载 Antigravity 账号 %d 运行时状态失败: %v", id, err)
		warning = appendAntigravityWarning(warning, "运行时重载失败: "+err.Error())
	}
	h.db.InsertAccountEventAsync(id, "updated", "manual_antigravity")
	security.SecurityAuditLog("ANTIGRAVITY_ACCOUNT_UPDATED", fmt.Sprintf("account_id=%d credential_changed=%t ip=%s", id, credentialsChanged, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"message": "Antigravity 账号设置已更新", "warning": warning})
}

func (h *Handler) updateAntigravityAPIKeyAccount(c *gin.Context, ctx context.Context, row *database.AccountRow, req updateAntigravityAccountRequest, groupUpdate database.OptionalInt64Slice) {
	updates := map[string]interface{}{"upstream_type": auth.UpstreamAntigravity}
	requestedAPIKey := ""
	familyID := ""
	apiKeyChanged := false
	if req.APIKey != nil {
		apiKey := strings.TrimSpace(*req.APIKey)
		if apiKey == "" {
			writeError(c, http.StatusBadRequest, "API Key 不能为空")
			return
		}
		requestedAPIKey = apiKey
		updates["api_key"] = apiKey
	}
	requestedModels := auth.NormalizeAccountModels(req.Models)
	if req.Models != nil {
		for _, model := range requestedModels {
			if err := security.ValidateModelName(model); err != nil {
				writeError(c, http.StatusBadRequest, "模型名称无效: "+model)
				return
			}
		}
		models := antigravityModelsForPersistence(requestedModels)
		updates["models"] = models
	}
	if req.ModelMapping != nil {
		mapping, err := normalizeAccountModelMapping(*req.ModelMapping)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		updates["model_mapping"] = mapping
	}
	var groups []int64
	if groupUpdate.Set {
		groups = append([]int64(nil), groupUpdate.Values...)
	}

	h.mergeDuplicateMu.Lock()
	currentRow, currentErr := h.db.GetAccountByID(ctx, row.ID)
	if currentErr != nil {
		h.mergeDuplicateMu.Unlock()
		writeInternalError(c, currentErr)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(currentRow.GetCredential("upstream_type")), auth.UpstreamAntigravity) || strings.TrimSpace(currentRow.GetCredential("api_key")) == "" {
		h.mergeDuplicateMu.Unlock()
		writeError(c, http.StatusConflict, "账号凭据类型已被其他操作更新，请刷新后重试")
		return
	}
	if currentRow.CredentialGeneration != row.CredentialGeneration {
		h.mergeDuplicateMu.Unlock()
		if reloadErr := h.reloadAntigravityRuntimeAccount(ctx, row.ID); reloadErr != nil {
			log.Printf("Antigravity API Key stale editor 冲突后重载 durable winner 失败 (account=%d): %v", row.ID, reloadErr)
			h.removeAntigravityRuntimeAccount(row.ID)
		}
		writeError(c, http.StatusConflict, "账号凭据已被其他操作更新，请重试")
		return
	}
	if req.APIKey != nil {
		apiKeyChanged = requestedAPIKey != strings.TrimSpace(currentRow.GetCredential("api_key"))
	}
	if apiKeyChanged {
		familyID = antigravityAPIKeyFamilyID(requestedAPIKey)
		updates["credential_family_id"] = familyID
		// Every provider observation below belongs to the old Google key. Clear
		// it in the same identity CAS so neither runtime nor admin state can
		// inherit stale quota, permission, catalog, or capability proof.
		updates["antigravity_capabilities"] = ""
		updates["antigravity_capability_last_probe_at"] = ""
		updates["antigravity_catalog_source"] = ""
		updates["antigravity_catalog_verified"] = false
		updates["antigravity_sync_error"] = ""
		updates["antigravity_sync_warning"] = ""
		updates["antigravity_quota"] = ""
		updates["antigravity_permissions"] = ""
		updates["antigravity_entitlements"] = ""
		updates["antigravity_last_synced_at"] = ""
	}
	if familyID != "" {
		rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
		if err != nil {
			h.mergeDuplicateMu.Unlock()
			writeInternalError(c, err)
			return
		}
		if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, "", "", currentRow.ID); duplicateID > 0 {
			h.mergeDuplicateMu.Unlock()
			writeError(c, http.StatusConflict, fmt.Sprintf("Antigravity API Key 已存在 (id=%d)", duplicateID))
			return
		}
	}
	var applied bool
	var persistErr error
	if apiKeyChanged {
		_, applied, persistErr = h.db.ReplaceAccountCredentialsCAS(ctx, currentRow.ID, currentRow.CredentialGeneration, familyID, updates)
	} else {
		// Even model/mapping-only edits are generation-fenced. Advancing the
		// generation is conservative but prevents a stale editor from overwriting
		// a concurrent key rotation while retaining its newer canonical family.
		_, applied, persistErr = h.db.UpdateAccountCredentialsCAS(ctx, currentRow.ID, currentRow.CredentialGeneration, updates)
	}
	if persistErr != nil {
		h.mergeDuplicateMu.Unlock()
		if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, row.ID, currentRow.CredentialGeneration); reconcileErr != nil {
			log.Printf("Antigravity API Key CAS 错误后无法确认 durable state (account=%d): %v", row.ID, reconcileErr)
		}
		writeInternalError(c, persistErr)
		return
	}
	h.mergeDuplicateMu.Unlock()
	if !applied {
		if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, row.ID, currentRow.CredentialGeneration); reconcileErr != nil {
			log.Printf("Antigravity API Key CAS 冲突后重载 durable winner 失败 (account=%d): %v", row.ID, reconcileErr)
		}
		writeError(c, http.StatusConflict, "账号凭据已被其他操作更新，请重试")
		return
	}
	runtimeMutationPersisted := true
	if apiKeyChanged {
		if _, err := h.db.ClearOwnedAccountError(ctx, row.ID, antigravityAccessDeniedError); err != nil {
			h.removeAntigravityRuntimeAccount(row.ID)
			writeInternalError(c, fmt.Errorf("清除 Antigravity API Key 旧访问栅栏失败: %w", err))
			return
		}
	}
	if req.ProxyURL != nil {
		if err := h.db.UpdateAccountProxyURL(ctx, row.ID, *req.ProxyURL); err != nil {
			if runtimeMutationPersisted {
				h.removeAntigravityRuntimeAccount(row.ID)
			}
			writeInternalError(c, err)
			return
		}
	}
	if req.Name != nil {
		if err := h.db.UpdateAccountName(ctx, row.ID, *req.Name); err != nil {
			if runtimeMutationPersisted {
				h.removeAntigravityRuntimeAccount(row.ID)
			}
			writeInternalError(c, err)
			return
		}
	}
	if groupUpdate.Set {
		if err := h.db.SetAccountGroups(ctx, row.ID, groups); err != nil {
			if runtimeMutationPersisted {
				h.removeAntigravityRuntimeAccount(row.ID)
			}
			writeInternalError(c, err)
			return
		}
	}
	if err := h.reloadAntigravityRuntimeAccount(ctx, row.ID); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	h.db.InsertAccountEventAsync(row.ID, "updated", "manual_antigravity_api_key")
	writeMessage(c, http.StatusOK, "Antigravity API Key 设置已更新")
}

// RefreshAntigravityQuota currently performs the same authenticated sync as a
// credential refresh because quota, permissions, and project metadata share it.
func (h *Handler) RefreshAntigravityQuota(c *gin.Context) {
	h.RefreshAntigravityAccount(c)
}

// BatchImportAntigravityAccounts imports portable Antigravity exports, manager
// exports, raw refresh tokens, and credential-store JSON. Enabled accounts are
// published only after persistence completes; disabled imports remain absent
// from the runtime pool.
func (h *Handler) BatchImportAntigravityAccounts(c *gin.Context) {
	var req antigravityImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理 URL 无效")
		return
	}
	groupCtx, groupCancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	groupIDs, err := h.resolveAntigravityGroupIDs(groupCtx, req.GroupIDs)
	groupCancel()
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	defaults := antigravityImportDefaults{
		OAuthClientKey: strings.TrimSpace(req.OAuthClientKey),
		ClientID:       strings.TrimSpace(req.ClientID), ClientSecret: strings.TrimSpace(req.ClientSecret),
	}
	contents := append([]string(nil), req.Files...)
	contents = append(contents, req.RefreshTokens...)
	if len(contents) == 0 {
		writeError(c, http.StatusBadRequest, "未提供 Antigravity 凭据")
		return
	}

	parsedCredentials, parseItems, parseErr := parseAntigravityImportInputs(contents, defaults)
	if parseErr != nil {
		writeError(c, http.StatusBadRequest, parseErr.Error())
		return
	}
	if len(parsedCredentials) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"message": "没有可导入的 Antigravity 凭据", "items": parseItems})
		return
	}

	// 代理注册必须早于任何账号落库，顺序理由见 registerImportedProxyBindings。
	// 规范化结果写回 document，让账号绑定的 URL 与代理表里的那条完全一致——两者
	// 只要差一个尾斜杠，账号就会被判为绑了一个不在池里的托管代理而不可调度。
	var proxyOutcome importProxyOutcome
	if req.ImportProxy {
		proxyBindings := make([]importProxyBinding, len(parsedCredentials))
		for i, parsed := range parsedCredentials {
			proxyBindings[i] = importProxyBinding{url: parsed.Document.ProxyURL, enabled: parsed.Document.ProxyEnabled}
		}
		proxyCtx, proxyCancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		outcome, registerErr := h.registerImportedProxyBindings(proxyCtx, proxyBindings)
		proxyCancel()
		if registerErr != nil {
			writeError(c, http.StatusInternalServerError, registerErr.Error())
			return
		}
		proxyOutcome = outcome
		for i := range parsedCredentials {
			parsedCredentials[i].Document.ProxyURL = proxyBindings[i].url
		}
	}

	preparedImports := make([]preparedAntigravityImport, 0, len(parsedCredentials))
	for _, parsedCredential := range parsedCredentials {
		prepared := prepareAntigravityImport(parsedCredential, req.ProxyURL)
		if prepared.ValidationError == "" && prepared.Document.AuthKind == auth.AntigravityAuthKindOAuth {
			effectiveProxyURL, resolveErr := h.resolveAntigravityControlPlaneProxy(0, prepared.ProxyURL, groupIDs)
			if resolveErr != nil {
				prepared.ValidationError = resolveErr.Error()
			} else {
				prepared.EffectiveProxyURL = effectiveProxyURL
			}
		}
		preparedImports = append(preparedImports, prepared)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), antigravityImportTimeout(len(parsedCredentials)))
	defer cancel()
	jobs := make([]antigravitySyncJob, 0, len(preparedImports))
	for _, prepared := range preparedImports {
		if prepared.ValidationError != "" || prepared.Document.AuthKind != auth.AntigravityAuthKindOAuth {
			continue
		}
		credential := prepared.Document.Credential
		credential.ProjectID = ""
		credential.IDToken = ""
		jobs = append(jobs, antigravitySyncJob{
			Index: len(jobs), SourceIndex: prepared.Parsed.SourceIndex, SubIndex: prepared.Parsed.SubIndex,
			Credential: credential, ProxyURL: prepared.EffectiveProxyURL,
		})
	}
	syncResults := runAntigravitySyncJobs(ctx, jobs)
	type importPosition struct {
		SourceIndex int
		SubIndex    int
	}
	syncResultsByPosition := make(map[importPosition]antigravitySyncJobResult, len(syncResults))
	for _, result := range syncResults {
		syncResultsByPosition[importPosition{SourceIndex: result.Job.SourceIndex, SubIndex: result.Job.SubIndex}] = result
	}

	h.mergeDuplicateMu.Lock()
	existingFamilies := make(map[string]struct{})
	existingSubjects := make(map[string]struct{})
	existingUnverifiedEmails := make(map[string]struct{})
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if err != nil {
		h.mergeDuplicateMu.Unlock()
		writeInternalError(c, err)
		return
	}
	for _, row := range rows {
		familyID := antigravityRowFamilyID(row)
		if familyID == "" && strings.TrimSpace(row.GetCredential("api_key")) != "" {
			familyID = antigravityAPIKeyFamilyID(row.GetCredential("api_key"))
		}
		if familyID != "" {
			existingFamilies[familyID] = struct{}{}
		}
		subject := strings.TrimSpace(row.GetCredential("account_id"))
		email := strings.ToLower(strings.TrimSpace(row.GetCredential("email")))
		if subject != "" {
			existingSubjects[subject] = struct{}{}
		} else if email != "" && strings.TrimSpace(row.GetCredential("api_key")) == "" {
			existingUnverifiedEmails[email] = struct{}{}
		}
	}

	items := append([]antigravityImportItem(nil), parseItems...)
	createdIDs := make([]int64, 0, len(preparedImports))
	runtimeBlockedIDs := make(map[int64]struct{})
	imported := 0
	synced := 0
	for _, prepared := range preparedImports {
		document := prepared.Document
		item := antigravityImportItem{Index: prepared.Parsed.SourceIndex, SubIndex: prepared.Parsed.SubIndex}
		if prepared.ValidationError != "" {
			item.Error = prepared.ValidationError
			items = append(items, item)
			continue
		}

		if document.AuthKind == auth.AntigravityAuthKindAPIKey {
			apiKey := strings.TrimSpace(document.APIKey)
			familyID := antigravityAPIKeyFamilyID(apiKey)
			if _, exists := existingFamilies[familyID]; exists {
				item.Error = "凭据身份已存在，已跳过"
				items = append(items, item)
				continue
			}
			credentialsMap := map[string]any{
				"upstream_type": auth.UpstreamAntigravity, "api_key": apiKey,
				"plan_type": "api", "email": "google-api-key", "credential_family_id": familyID,
				"models": prepared.Models, "model_mapping": prepared.ModelMapping,
			}
			name := prepared.Name
			if name == "" {
				name = "antigravity-api-key"
			}
			id, insertErr := h.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentialsMap, prepared.ProxyURL)
			if insertErr != nil {
				item.Error = insertErr.Error()
				items = append(items, item)
				continue
			}
			existingFamilies[familyID] = struct{}{}
			if document.DisabledPresent && document.Disabled {
				h.removeAntigravityRuntimeAccount(id)
				if disableErr := h.db.SetAccountEnabled(ctx, id, false); disableErr != nil {
					_ = h.db.SetError(ctx, id, "Antigravity import failed to persist disabled state")
					item.ID = id
					item.Error = "保存账号禁用状态失败"
					items = append(items, item)
					continue
				}
				runtimeBlockedIDs[id] = struct{}{}
				h.removeAntigravityRuntimeAccount(id)
			}
			h.db.InsertAccountEventAsync(id, "added", "antigravity_import_api_key")
			item.ID = id
			item.Email = "google-api-key"
			item.OK = true
			item.Synced = true
			items = append(items, item)
			createdIDs = append(createdIDs, id)
			imported++
			synced++
			continue
		}

		position := importPosition{SourceIndex: prepared.Parsed.SourceIndex, SubIndex: prepared.Parsed.SubIndex}
		syncResult, ok := syncResultsByPosition[position]
		if !ok {
			item.Error = "Antigravity 同步任务未返回结果"
			items = append(items, item)
			continue
		}
		credential := syncResult.Job.Credential
		if syncResult.Result.Credential.AccessToken != "" || syncResult.Result.Credential.RefreshToken != "" {
			credential = syncResult.Result.Credential
		}
		if syncResult.Err == nil {
			item.Email = syncResult.Result.Profile.Email
			item.Synced = true
			item.Warning = security.MaskSensitiveData(syncResult.Result.Warning)
		} else {
			item.Email = strings.TrimSpace(syncResult.Result.Profile.Email)
			if item.Email == "" {
				item.Email = strings.TrimSpace(credential.Email)
			}
			item.Warning = appendAntigravityWarning(
				security.MaskSensitiveData(syncResult.Result.Warning),
				security.MaskSensitiveData(syncResult.Err.Error()),
			)
		}
		familyID := antigravityCredentialFamilyID(credential, syncResult.Result.Profile.ID)
		if familyID == "" {
			familyID = antigravityCredentialProvisionalFamilyID(credential)
		}
		if _, exists := existingFamilies[familyID]; familyID != "" && exists {
			item.Error = "凭据身份已存在，已跳过"
			items = append(items, item)
			continue
		}
		emailKey := strings.ToLower(strings.TrimSpace(item.Email))
		profileID := strings.TrimSpace(syncResult.Result.Profile.ID)
		_, duplicateSubject := existingSubjects[profileID]
		_, duplicateEmail := existingUnverifiedEmails[emailKey]
		if (profileID != "" && duplicateSubject) || (profileID == "" && emailKey != "" && duplicateEmail) {
			item.Error = "Google 账号已存在，已跳过"
			items = append(items, item)
			continue
		}
		credentialsMap, credentialsErr := antigravityCredentialsForInsert(credential, familyID, syncResult.Result, syncResult.Err)
		if credentialsErr != nil {
			item.Error = credentialsErr.Error()
			items = append(items, item)
			continue
		}
		if prepared.ModelsPresent {
			credentialsMap["models"] = prepared.Models
		}
		if prepared.ModelMapping != "" {
			credentialsMap["model_mapping"] = prepared.ModelMapping
		}
		name := prepared.Name
		if name == "" {
			name = strings.TrimSpace(syncResult.Result.Profile.Name)
		}
		if name == "" {
			name = strings.TrimSpace(credential.Name)
		}
		if name == "" {
			name = strings.TrimSpace(item.Email)
		}
		if name == "" {
			name = fmt.Sprintf("antigravity-%d", syncResult.Job.SourceIndex)
			if syncResult.Job.SubIndex > 1 {
				name = fmt.Sprintf("%s-%d", name, syncResult.Job.SubIndex)
			}
		}
		id, insertErr := h.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentialsMap, prepared.ProxyURL)
		if insertErr != nil {
			item.Error = insertErr.Error()
			items = append(items, item)
			continue
		}
		if familyID != "" {
			existingFamilies[familyID] = struct{}{}
		}
		if profileID != "" {
			existingSubjects[profileID] = struct{}{}
		} else if emailKey != "" {
			existingUnverifiedEmails[emailKey] = struct{}{}
		}
		if document.DisabledPresent && document.Disabled {
			h.removeAntigravityRuntimeAccount(id)
			if disableErr := h.db.SetAccountEnabled(ctx, id, false); disableErr != nil {
				_ = h.db.SetError(ctx, id, "Antigravity import failed to persist disabled state")
				item.ID = id
				item.Error = "保存账号禁用状态失败"
				items = append(items, item)
				continue
			}
			runtimeBlockedIDs[id] = struct{}{}
			h.removeAntigravityRuntimeAccount(id)
		}
		h.db.InsertAccountEventAsync(id, "added", "antigravity_import")
		item.ID = id
		item.OK = true
		if reason := antigravityAccessDeniedReason(syncResult.Result); reason != "" {
			item.Warning = appendAntigravityWarning(item.Warning, antigravityAccessDeniedError+": "+reason)
		}
		if _, fenceErr := h.applyAntigravityAccessFence(ctx, id, syncResult.Result, syncResult.Err == nil); fenceErr != nil {
			item.Warning = appendAntigravityWarning(item.Warning, "访问状态持久化失败: "+fenceErr.Error())
			runtimeBlockedIDs[id] = struct{}{}
			h.removeAntigravityRuntimeAccount(id)
		}
		items = append(items, item)
		createdIDs = append(createdIDs, id)
		imported++
		if item.Synced {
			synced++
		}
	}
	h.mergeDuplicateMu.Unlock()

	groupWarning := ""
	if err := h.bindImportedAccountGroups(ctx, createdIDs, groupIDs); err != nil {
		groupWarning = "分组绑定失败: " + err.Error()
	}
	for _, id := range createdIDs {
		if _, blocked := runtimeBlockedIDs[id]; blocked {
			h.removeAntigravityRuntimeAccount(id)
			continue
		}
		if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
			log.Printf("加载 Antigravity 账号 %d 到运行时失败: %v", id, err)
			groupWarning = appendAntigravityWarning(groupWarning, fmt.Sprintf("账号 %d 运行时加载失败: %v", id, err))
		}
	}
	if groupWarning != "" {
		for index := range items {
			if items[index].ID > 0 && items[index].OK {
				items[index].Warning = appendAntigravityWarning(items[index].Warning, groupWarning)
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Index != items[j].Index {
			return items[i].Index < items[j].Index
		}
		return items[i].SubIndex < items[j].SubIndex
	})
	total := len(parsedCredentials) + len(parseItems)
	security.SecurityAuditLog("ANTIGRAVITY_ACCOUNT_IMPORTED", fmt.Sprintf("total=%d imported=%d synced=%d ip=%s", total, imported, synced, c.ClientIP()))
	degraded := 0
	for _, item := range items {
		if item.OK && (!item.Synced || strings.TrimSpace(item.Warning) != "") {
			degraded++
		}
	}
	response := gin.H{
		"total": total, "imported": imported,
		"synced": synced, "degraded": degraded,
		"failed": total - imported, "items": items,
		"group_ids": groupIDs, "warning": groupWarning,
	}
	if req.ImportProxy {
		response["proxies_imported"] = proxyOutcome.inserted
		response["proxies_skipped"] = proxyOutcome.skipped
		if warning := proxyOutcome.warning(); warning != "" {
			response["proxy_warning"] = warning
		}
	}
	c.JSON(http.StatusOK, response)
}

func antigravityImportTimeout(accounts int) time.Duration {
	timeout := 45*time.Second + time.Duration(accounts)*15*time.Second/time.Duration(antigravitySyncConcurrency)
	if timeout > 15*time.Minute {
		return 15 * time.Minute
	}
	return timeout
}

func runAntigravitySyncJobs(ctx context.Context, jobs []antigravitySyncJob) []antigravitySyncJobResult {
	results := make([]antigravitySyncJobResult, len(jobs))
	queue := make(chan antigravitySyncJob)
	var wg sync.WaitGroup
	workers := antigravitySyncConcurrency
	if len(jobs) < workers {
		workers = len(jobs)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				job.Credential.ProjectID = ""
				client, err := auth.NewAntigravityClient(job.ProxyURL)
				if err != nil {
					results[job.Index] = antigravitySyncJobResult{Job: job, Err: err}
					continue
				}
				result, err := client.Sync(ctx, job.Credential)
				if err == nil && !antigravityAuthoritativeProfile(result.Profile) {
					err = errors.New("Antigravity sync did not return a verified Google subject")
				}
				results[job.Index] = antigravitySyncJobResult{Job: job, Result: result, Err: err}
			}
		}()
	}
	for _, job := range jobs {
		select {
		case queue <- job:
		case <-ctx.Done():
			results[job.Index] = antigravitySyncJobResult{Job: job, Err: ctx.Err()}
		}
	}
	close(queue)
	wg.Wait()
	return results
}

func antigravityCredentialFamilyID(credential auth.AntigravityCredential, profileID string) string {
	kind := "refresh"
	value := strings.TrimSpace(credential.RefreshToken)
	if strings.TrimSpace(profileID) != "" {
		kind = "google-sub"
		value = strings.TrimSpace(profileID)
	}
	if value == "" {
		value = strings.TrimSpace(credential.Email)
		kind = "email"
	}
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("antigravity-credential-v1\x00" + kind + "\x00" + value))
	return "ag_" + hex.EncodeToString(sum[:])
}

func antigravityAPIKeyFamilyID(apiKey string) string {
	sum := sha256.Sum256([]byte("antigravity-api-key-v1\x00" + strings.TrimSpace(apiKey)))
	return "agk_" + hex.EncodeToString(sum[:])
}

func antigravityCredentialProvisionalFamilyID(credential auth.AntigravityCredential) string {
	if strings.TrimSpace(credential.RefreshToken) != "" {
		return antigravityCredentialFamilyID(credential, "")
	}
	value := strings.TrimSpace(credential.AccessToken)
	if value != "" {
		sum := sha256.Sum256([]byte("antigravity-provisional-v1\x00access\x00" + value))
		return "ag_" + hex.EncodeToString(sum[:])
	}
	return antigravityCredentialFamilyID(credential, "")
}

func antigravityCredentialsForInsert(credential auth.AntigravityCredential, familyID string, result auth.AntigravitySyncResult, syncErr error) (map[string]any, error) {
	if strings.TrimSpace(familyID) == "" {
		familyID = antigravityCredentialProvisionalFamilyID(credential)
	}
	credentials := map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "refresh_token": credential.RefreshToken,
		"access_token": credential.AccessToken, "id_token": credential.IDToken,
		"email": credential.Email, "name": credential.Name, "avatar_url": credential.AvatarURL,
		"project_id": credential.ProjectID, "oauth_client_key": credential.OAuthClientKey,
		"antigravity_client_id": credential.ClientID, "antigravity_client_secret": credential.ClientSecret,
		"oauth_scope": credential.Scope, "credential_family_id": familyID,
		"antigravity_last_sync_attempt_at": time.Now().UTC().Format(time.RFC3339),
	}
	if credential.VerifiedEmailPresent {
		// This is an imported claim only. The canonical verified_email field is
		// populated exclusively from live Google userinfo below.
		credentials[antigravityImportedVerifiedEmailKey] = credential.VerifiedEmail
	}
	if !credential.ExpiresAt.IsZero() {
		credentials["expires_at"] = credential.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if syncErr != nil {
		// Use the credential that will actually be inserted when Sync returned
		// an empty result (for example, a token endpoint failure). The helper
		// deliberately quarantines unverified credentials and clears any
		// identity-bound fields that could not be re-observed.
		failureResult := result
		failureResult.Credential = credential
		failureUpdates, _ := antigravityFailureCredentialUpdates(nil, failureResult, syncErr, familyID)
		for key, value := range failureUpdates {
			credentials[key] = value
		}
		return credentials, nil
	}
	updates, err := antigravityCredentialUpdates(result, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range updates {
		credentials[key] = value
	}
	return credentials, nil
}

func antigravitySnapshotSource(row *database.AccountRow, result auth.AntigravitySyncResult) *database.AccountRow {
	if row == nil {
		return nil
	}
	previousID := strings.TrimSpace(row.GetCredential("account_id"))
	nextID := strings.TrimSpace(result.Profile.ID)
	if previousID != "" {
		if nextID == "" || previousID != nextID {
			return nil
		}
		return row
	}
	previousEmail := strings.TrimSpace(row.GetCredential("email"))
	nextEmail := strings.TrimSpace(result.Profile.Email)
	if previousEmail != "" {
		if nextEmail == "" || !strings.EqualFold(previousEmail, nextEmail) {
			return nil
		}
	}
	return row
}

func antigravityCredentialUpdates(result auth.AntigravitySyncResult, previous *database.AccountRow) (map[string]any, error) {
	originalPrevious := previous
	trustedPrevious := previous != nil && antigravityRowHasVerifiedIdentity(previous)
	sameIdentityPrevious := trustedPrevious && antigravitySnapshotSource(previous, result) != nil
	if !sameIdentityPrevious {
		previous = nil
	}
	quotaSnapshot := result.Quota
	if previous != nil && (!result.QuotaGroupsObserved || !result.AICreditsObserved) {
		var previousQuota auth.AntigravityQuotaSnapshot
		if raw := strings.TrimSpace(previous.GetCredential("antigravity_quota")); raw != "" && json.Unmarshal([]byte(raw), &previousQuota) == nil {
			if !result.QuotaGroupsObserved {
				quotaSnapshot.Groups = previousQuota.Groups
			}
			if !result.AICreditsObserved {
				quotaSnapshot.AICredits = previousQuota.AICredits
			}
		}
	}
	quota, err := json.Marshal(quotaSnapshot)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(quotaSnapshot.Models))
	for _, model := range quotaSnapshot.Models {
		models = append(models, model.ModelID)
	}
	updates := map[string]any{
		"upstream_type": auth.UpstreamAntigravity,
		"access_token":  result.Credential.AccessToken, "refresh_token": result.Credential.RefreshToken,
		"id_token": result.Credential.IDToken, "email": result.Profile.Email,
		"name": result.Profile.Name, "avatar_url": result.Profile.Picture,
		"verified_email": result.Profile.VerifiedEmail,
		"account_id":     result.Profile.ID, "project_id": "",
		"models":           models,
		"oauth_client_key": result.Credential.OAuthClientKey, "oauth_scope": result.Credential.Scope,
		"antigravity_client_id": result.Credential.ClientID, "antigravity_client_secret": result.Credential.ClientSecret,
		"antigravity_quota":      string(quota),
		"antigravity_sync_error": "", "antigravity_sync_warning": result.Warning,
		"antigravity_last_synced_at":       result.Quota.UpdatedAt.UTC().Format(time.RFC3339),
		"antigravity_last_sync_attempt_at": time.Now().UTC().Format(time.RFC3339),
	}
	if result.Credential.VerifiedEmailPresent {
		// Preserve the imported claim separately from the live, authoritative
		// userinfo result. This metadata is never projected as verified_email.
		updates[antigravityImportedVerifiedEmailKey] = result.Credential.VerifiedEmail
	}
	if sameIdentityPrevious {
		projectID := result.Credential.ProjectID
		if !result.EntitlementsObserved && strings.TrimSpace(projectID) == "" {
			projectID = previous.GetCredential("project_id")
		}
		updates["project_id"] = projectID
	}
	if originalPrevious != nil && !sameIdentityPrevious {
		updates["id_token"] = ""
	}
	if !sameIdentityPrevious && !result.EntitlementsObserved {
		// loadProject is the authority for the project/tier. A quota response
		// obtained while that authority is unavailable may have used a stale
		// project carried by the imported token, so do not publish it under a
		// newly discovered subject.
		updates["models"] = []string{}
		updates["antigravity_quota"] = ""
		updates["antigravity_last_synced_at"] = ""
	}
	if result.EntitlementsObserved {
		entitlements, marshalErr := json.Marshal(result.Entitlements)
		if marshalErr != nil {
			return nil, marshalErr
		}
		updates["project_id"] = result.Entitlements.ProjectID
		updates["plan_type"] = result.Entitlements.EffectiveTier
		updates["antigravity_permissions"] = string(entitlements)
		updates["antigravity_entitlements"] = string(entitlements)
	} else if previous == nil {
		// Identity replacement uses a merge-CAS to preserve unrelated account
		// metadata. Clear identity-bound snapshots when the new permissions are unknown.
		updates["plan_type"] = ""
		updates["antigravity_permissions"] = ""
		updates["antigravity_entitlements"] = ""
	}
	if previous == nil && !result.EntitlementsObserved {
		updates["project_id"] = ""
	}
	updates["expires_at"] = ""
	if !result.Credential.ExpiresAt.IsZero() {
		updates["expires_at"] = result.Credential.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return updates, nil
}

func antigravityCredentialFailureUpdates(result auth.AntigravitySyncResult, syncErr error) map[string]any {
	updates := map[string]any{
		"upstream_type":                    auth.UpstreamAntigravity,
		"antigravity_sync_error":           antigravityErrorString(syncErr),
		"antigravity_sync_warning":         result.Warning,
		"antigravity_last_sync_attempt_at": time.Now().UTC().Format(time.RFC3339),
	}
	credential := result.Credential
	if credential.VerifiedEmailPresent {
		updates[antigravityImportedVerifiedEmailKey] = credential.VerifiedEmail
	}
	for key, value := range map[string]string{
		"access_token": credential.AccessToken, "refresh_token": credential.RefreshToken,
		"id_token": credential.IDToken, "oauth_client_key": credential.OAuthClientKey,
		"oauth_scope": credential.Scope, "antigravity_client_id": credential.ClientID,
		"antigravity_client_secret": credential.ClientSecret,
	} {
		if strings.TrimSpace(value) != "" {
			updates[key] = value
		}
	}
	if !credential.ExpiresAt.IsZero() {
		updates["expires_at"] = credential.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if antigravityAuthoritativeProfile(result.Profile) {
		updates["email"] = result.Profile.Email
		updates["name"] = result.Profile.Name
		updates["avatar_url"] = result.Profile.Picture
		updates["verified_email"] = result.Profile.VerifiedEmail
		updates["account_id"] = result.Profile.ID
		if result.EntitlementsObserved {
			updates["project_id"] = credential.ProjectID
		} else {
			updates["project_id"] = ""
		}
	}
	if result.EntitlementsObserved {
		if encoded, err := json.Marshal(result.Entitlements); err == nil {
			updates["plan_type"] = result.Entitlements.EffectiveTier
			updates["antigravity_permissions"] = string(encoded)
			updates["antigravity_entitlements"] = string(encoded)
		}
	}
	return updates
}

func antigravityErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *Handler) RefreshAntigravityAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	item := h.refreshAntigravityAccount(ctx, id)
	if !item.OK {
		status := http.StatusBadGateway
		if item.Error == "账号不存在" {
			status = http.StatusNotFound
		}
		writeError(c, status, item.Error)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (h *Handler) BatchRefreshAntigravityAccounts(c *gin.Context) {
	var req antigravityRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请选择要刷新的账号")
		return
	}
	if len(ids) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多刷新 100 个账号")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), antigravityImportTimeout(len(ids)))
	defer cancel()
	items := make([]antigravityRefreshItem, 0, len(ids))
	success := 0
	for _, id := range ids {
		item := h.refreshAntigravityAccount(ctx, id)
		items = append(items, item)
		if item.OK {
			success++
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": success, "failed": len(ids) - success, "items": items})
}

func (h *Handler) refreshAntigravityAccount(ctx context.Context, id int64) antigravityRefreshItem {
	item := antigravityRefreshItem{ID: id}
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			item.Error = "账号不存在"
		} else {
			item.Error = err.Error()
		}
		return item
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamAntigravity) {
		item.Error = "仅 Antigravity 账号支持该操作"
		return item
	}
	credential := antigravityCredentialFromRow(row)
	// Let loadCodeAssist establish the project for the refreshed principal. A
	// project carried from the prior subject can otherwise be sent to quota
	// after a token rotation and produce an ambiguous snapshot.
	credential.ProjectID = ""
	credential.IDToken = ""
	preSyncFamilyID := antigravityRowFamilyID(row)
	if antigravityRowHasVerifiedIdentity(row) {
		// A known subject cannot safely remain the family for a token whose
		// replacement subject has not been reverified. Move to the pre-refresh
		// refresh-token lineage as the provisional family instead.
		preSyncFamilyID = antigravityCredentialProvisionalFamilyID(credential)
	}
	if preSyncFamilyID == "" {
		preSyncFamilyID = antigravityCredentialProvisionalFamilyID(credential)
	}
	groupIDs, err := h.db.GetAccountGroupIDs(ctx, row.ID)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	proxyURL, err := h.resolveAntigravityControlPlaneProxy(row.ID, row.ProxyURL, groupIDs)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	client, err := auth.NewAntigravityClient(proxyURL)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	result, err := client.Sync(ctx, credential)
	if err == nil && !antigravityAuthoritativeProfile(result.Profile) {
		err = errors.New("Antigravity sync did not return a verified Google subject")
	}
	if err != nil {
		item.Email = strings.TrimSpace(result.Profile.Email)
		if item.Email == "" {
			item.Email = row.GetCredential("email")
		}
		item.Error = err.Error()
		providerProgress := antigravityFailureHasProgress(row, result)
		persistOutcome := h.persistFailedAntigravitySync(ctx, row, result, err, preSyncFamilyID)
		item.Warning = appendAntigravityWarning(item.Warning, persistOutcome.Warning)
		var fenceErr error
		if persistOutcome.ProgressPersisted || (!providerProgress && persistOutcome.Mutated) {
			h.invalidateAccountSnapshotCaches()
			_, fenceErr = h.applyAntigravityAccessFence(ctx, id, result, false)
		}
		if reason := antigravityAccessDeniedReason(result); reason != "" {
			item.Warning = appendAntigravityWarning(item.Warning, antigravityAccessDeniedError+": "+reason)
		}
		if fenceErr != nil {
			item.Warning = appendAntigravityWarning(item.Warning, "访问状态持久化失败: "+fenceErr.Error())
			h.removeAntigravityRuntimeAccount(id)
		} else if persistOutcome.ProgressPersisted || (!providerProgress && persistOutcome.Mutated) {
			if reloadErr := h.reloadAntigravityRuntimeAccount(ctx, id); reloadErr != nil {
				log.Printf("保存失败同步结果后重载 Antigravity 账号 %d 运行时状态失败: %v", id, reloadErr)
				item.Warning = appendAntigravityWarning(item.Warning, "运行时重载失败: "+reloadErr.Error())
			}
		} else if persistOutcome.ConcurrentWinner {
			if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
				item.Warning = appendAntigravityWarning(item.Warning, "运行时重载失败: "+reconcileErr.Error())
			}
		} else if providerProgress {
			h.removeAntigravityRuntimeAccount(id)
		}
		item.Error = appendAntigravityWarning(item.Error, item.Warning)
		return item
	}
	updates, err := antigravityCredentialUpdates(result, row)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	familyID := antigravityCredentialFamilyID(result.Credential, result.Profile.ID)
	h.mergeDuplicateMu.Lock()
	rows, listErr := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if listErr == nil {
		if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, result.Profile.Email, result.Profile.ID, id); duplicateID > 0 {
			h.mergeDuplicateMu.Unlock()
			item.Error = fmt.Sprintf("Antigravity credential identity already exists (id=%d)", duplicateID)
			return item
		}
	}
	if listErr != nil {
		h.mergeDuplicateMu.Unlock()
		item.Error = listErr.Error()
		return item
	}
	var applied bool
	if familyID != "" && antigravityRowFamilyID(row) != familyID {
		_, applied, err = h.db.ReplaceAccountCredentialsCAS(ctx, id, row.CredentialGeneration, familyID, updates)
	} else {
		_, applied, err = h.db.UpdateAccountCredentialsCAS(ctx, id, row.CredentialGeneration, updates)
	}
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		item.Error = err.Error()
		if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
			log.Printf("Antigravity 手工刷新 CAS 错误后无法确认 durable state (account=%d): %v", id, reconcileErr)
			item.Warning = appendAntigravityWarning(item.Warning, "运行时重载失败: "+reconcileErr.Error())
			item.Error = appendAntigravityWarning(item.Error, item.Warning)
		}
		return item
	}
	if !applied {
		item.Error = "账号凭据已被其他操作更新，请重试"
		if _, reconcileErr := h.reconcileAntigravityRuntimeAfterCAS(ctx, id, row.CredentialGeneration); reconcileErr != nil {
			log.Printf("Antigravity 手工刷新 CAS 冲突后重载 durable winner 失败 (account=%d): %v", id, reconcileErr)
			item.Warning = appendAntigravityWarning(item.Warning, "运行时重载失败: "+reconcileErr.Error())
			item.Error = appendAntigravityWarning(item.Error, item.Warning)
		}
		return item
	}
	if strings.TrimSpace(result.Profile.Name) != "" {
		_ = h.db.UpdateAccountName(ctx, id, result.Profile.Name)
	}
	if _, fenceErr := h.applyAntigravityAccessFence(ctx, id, result, true); fenceErr != nil {
		h.removeAntigravityRuntimeAccount(id)
		item.Error = "持久化 Antigravity 访问状态失败: " + fenceErr.Error()
		return item
	}
	if reason := antigravityAccessDeniedReason(result); reason != "" {
		item.Warning = appendAntigravityWarning(item.Warning, antigravityAccessDeniedError+": "+reason)
	}
	if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
		log.Printf("刷新后重载 Antigravity 账号 %d 运行时状态失败: %v", id, err)
		item.Warning = appendAntigravityWarning(item.Warning, "运行时重载失败: "+err.Error())
	}
	h.db.InsertAccountEventAsync(id, "updated", "antigravity_refresh")
	item.Email = result.Profile.Email
	item.Warning = appendAntigravityWarning(result.Warning, item.Warning)
	item.Message = "Antigravity 账号信息与额度已刷新"
	item.OK = true
	return item
}

type antigravityFailedSyncPersistOutcome struct {
	Mutated           bool
	ProgressPersisted bool
	ConcurrentWinner  bool
	PersistenceFailed bool
	Warning           string
}

func (h *Handler) persistFailedAntigravitySync(ctx context.Context, row *database.AccountRow, result auth.AntigravitySyncResult, syncErr error, fallbackFamilyID string) antigravityFailedSyncPersistOutcome {
	if row == nil {
		return antigravityFailedSyncPersistOutcome{PersistenceFailed: true, Warning: "Antigravity source account is unavailable"}
	}
	basicUpdates := antigravityCredentialFailureUpdates(result, syncErr)
	if !antigravityFailureHasProgress(row, result) {
		mutated, err := h.db.MergeAccountCredentialsForGeneration(ctx, row.ID, row.CredentialGeneration, basicUpdates)
		if err != nil {
			return antigravityFailedSyncPersistOutcome{PersistenceFailed: true, Warning: "failed to record Antigravity sync error: " + err.Error()}
		}
		return antigravityFailedSyncPersistOutcome{Mutated: mutated, ConcurrentWinner: !mutated}
	}

	failureUpdates, familyID := antigravityFailureCredentialUpdates(row, result, syncErr, fallbackFamilyID)
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if err != nil {
		return antigravityFailedSyncPersistOutcome{PersistenceFailed: true, Warning: "failed to check Antigravity identity uniqueness: " + err.Error()}
	}
	if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, result.Profile.Email, result.Profile.ID, row.ID); duplicateID > 0 {
		// A verified subject already owned by another row must not be silently
		// moved. Record the failure on the source and leave both identities
		// untouched; the process mutex serializes this check/write decision.
		warning := fmt.Sprintf("refreshed Google identity already belongs to account %d", duplicateID)
		// Do not merge rotated tokens or observed profile fields here: those
		// belong to the duplicate target. Persist only the source-row failure
		// marker and attempt metadata, preserving its prior credential lineage.
		duplicateFailure := map[string]any{
			"upstream_type":                    auth.UpstreamAntigravity,
			"antigravity_sync_error":           antigravityErrorString(syncErr),
			"antigravity_sync_warning":         result.Warning,
			"antigravity_last_sync_attempt_at": time.Now().UTC().Format(time.RFC3339),
		}
		mutated, mergeErr := h.db.MergeAccountCredentialsForGeneration(ctx, row.ID, row.CredentialGeneration, duplicateFailure)
		if mergeErr != nil {
			warning = appendAntigravityWarning(warning, "failed to record Antigravity sync error on source account: "+mergeErr.Error())
		}
		return antigravityFailedSyncPersistOutcome{Mutated: mutated, PersistenceFailed: mergeErr != nil, Warning: warning}
	}
	applied, persistErr := h.persistAntigravityFailureCredential(ctx, row, familyID, failureUpdates)
	if persistErr != nil {
		return antigravityFailedSyncPersistOutcome{PersistenceFailed: true, Warning: "failed to preserve refreshed Antigravity credential: " + persistErr.Error()}
	}
	if !applied {
		return antigravityFailedSyncPersistOutcome{ConcurrentWinner: true, Warning: "credential changed concurrently; refreshed token was not persisted"}
	}
	return antigravityFailedSyncPersistOutcome{Mutated: true, ProgressPersisted: true}
}

func (h *Handler) persistAntigravityFailureCredential(ctx context.Context, row *database.AccountRow, familyID string, updates map[string]any) (bool, error) {
	if row == nil {
		return false, sql.ErrNoRows
	}
	if familyID == "" || antigravityRowFamilyID(row) != familyID {
		_, applied, err := h.db.ReplaceAccountCredentialsCAS(ctx, row.ID, row.CredentialGeneration, familyID, updates)
		return applied, err
	}
	_, applied, err := h.db.UpdateAccountCredentialsCAS(ctx, row.ID, row.CredentialGeneration, updates)
	return applied, err
}

func antigravityFailureCredentialUpdates(row *database.AccountRow, result auth.AntigravitySyncResult, syncErr error, fallbackFamilyID string) (map[string]any, string) {
	updates := antigravityCredentialFailureUpdates(result, syncErr)
	profileObserved := antigravityAuthoritativeProfile(result.Profile)
	familyID := strings.TrimSpace(fallbackFamilyID)
	if familyID == "" {
		familyID = antigravityCredentialProvisionalFamilyID(result.Credential)
	}
	trustedSameIdentity := false
	if profileObserved {
		familyID = antigravityCredentialFamilyID(result.Credential, result.Profile.ID)
		trustedSameIdentity = antigravitySnapshotSource(row, result) != nil && antigravityRowHasVerifiedIdentity(row)
	}
	if trustedSameIdentity {
		if !result.EntitlementsObserved {
			delete(updates, "project_id")
		}
		return updates, familyID
	}

	updates["models"] = []string{}
	updates["antigravity_quota"] = ""
	updates["antigravity_last_synced_at"] = ""
	if !result.EntitlementsObserved || !profileObserved {
		updates["project_id"] = ""
		updates["plan_type"] = ""
		updates["antigravity_permissions"] = ""
		updates["antigravity_entitlements"] = ""
	}
	if !profileObserved {
		// A rotated credential without a reverified subject is quarantined under
		// its provisional credential family. Do not leave a new token paired with
		// the old Google principal or its display metadata.
		updates["account_id"] = ""
		updates["email"] = ""
		updates["name"] = ""
		updates["avatar_url"] = ""
		updates["verified_email"] = false
	}
	// Never carry an old principal's token across a provisional or subject
	// replacement transition.
	updates["id_token"] = ""
	if result.Credential.ExpiresAt.IsZero() {
		updates["expires_at"] = ""
	}
	return updates, familyID
}

func antigravityFailureHasProgress(row *database.AccountRow, result auth.AntigravitySyncResult) bool {
	if row == nil {
		return false
	}
	credential := result.Credential
	if strings.TrimSpace(credential.AccessToken) != "" && credential.AccessToken != row.GetCredential("access_token") {
		return true
	}
	if strings.TrimSpace(credential.RefreshToken) != "" && credential.RefreshToken != row.GetCredential("refresh_token") {
		return true
	}
	if antigravityAuthoritativeProfile(result.Profile) || result.EntitlementsObserved {
		return true
	}
	return false
}

func antigravityRowHasVerifiedIdentity(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	return strings.TrimSpace(row.GetCredential("account_id")) != "" ||
		(row.GetCredentialBool("verified_email") && strings.TrimSpace(row.GetCredential("email")) != "")
}

func antigravityCredentialFromRow(row *database.AccountRow) auth.AntigravityCredential {
	credential := auth.AntigravityCredential{
		AccessToken: row.GetCredential("access_token"), RefreshToken: row.GetCredential("refresh_token"),
		IDToken: row.GetCredential("id_token"), Email: row.GetCredential("email"), Name: row.GetCredential("name"),
		AvatarURL: row.GetCredential("avatar_url"), ProjectID: row.GetCredential("project_id"),
		OAuthClientKey: row.GetCredential("oauth_client_key"),
		ClientID:       row.GetCredential("antigravity_client_id"), ClientSecret: row.GetCredential("antigravity_client_secret"),
		Scope: row.GetCredential("oauth_scope"),
	}
	if raw := strings.TrimSpace(row.GetCredential("expires_at")); raw != "" {
		credential.ExpiresAt, _ = time.Parse(time.RFC3339, raw)
	}
	if value := row.GetCredentialOptionalBool(antigravityImportedVerifiedEmailKey); value != nil {
		credential.VerifiedEmail = *value
		credential.VerifiedEmailPresent = true
	}
	return credential
}
