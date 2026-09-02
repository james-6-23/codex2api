package admin

// Claude Code(Anthropic)OAuth 账号的后台导入端点。
//
// 提供两条导入路径:
//   1. 网页 OAuth 两步式:
//        POST /accounts/claude/oauth/auth-url      → 返回授权 URL + state
//        POST /accounts/claude/oauth/exchange-code → 用 state+code 换 token 并入库
//      服务端用一个带 TTL 的内存表按 state 暂存 verifier。
//   2. CLI 直导:
//        POST /accounts/claude/import              → 直接吃 cmd/claude_login -out 产出的
//        token JSON(access_token/refresh_token/...)入库,无需服务端 OAuth 往返。

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// claudeOAuthPending 暂存一次登录的 state→verifier(带 TTL)。
type claudeOAuthPending struct {
	verifier  string
	createdAt time.Time
}

var (
	claudeOAuthMu      sync.Mutex
	claudeOAuthPendMap = map[string]claudeOAuthPending{}
)

const claudeOAuthSessionTTL = 15 * time.Minute

func claudeOAuthPut(state, verifier string) {
	claudeOAuthMu.Lock()
	defer claudeOAuthMu.Unlock()
	// 顺带清理过期项,避免内存无限增长。
	now := time.Now()
	for k, v := range claudeOAuthPendMap {
		if now.Sub(v.createdAt) > claudeOAuthSessionTTL {
			delete(claudeOAuthPendMap, k)
		}
	}
	claudeOAuthPendMap[state] = claudeOAuthPending{verifier: verifier, createdAt: now}
}

func claudeOAuthTake(state string) (string, bool) {
	claudeOAuthMu.Lock()
	defer claudeOAuthMu.Unlock()
	p, ok := claudeOAuthPendMap[state]
	if !ok {
		return "", false
	}
	delete(claudeOAuthPendMap, state)
	if time.Since(p.createdAt) > claudeOAuthSessionTTL {
		return "", false
	}
	return p.verifier, true
}

// GenerateClaudeAuthURL 发起一次 Claude OAuth 登录,返回授权 URL 与 state。
func (h *Handler) GenerateClaudeAuthURL(c *gin.Context) {
	session, err := auth.StartClaudeLogin()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	claudeOAuthPut(session.State, session.Verifier)
	c.JSON(http.StatusOK, gin.H{
		"auth_url": session.AuthURL,
		"state":    session.State,
	})
}

type exchangeClaudeCodeReq struct {
	State string `json:"state"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	// ProxyURL 指定固定代理;留空且 UseProxyPool=true 时从代理池自动取一个。
	ProxyURL     string `json:"proxy_url"`
	UseProxyPool bool   `json:"use_proxy_pool"`
	// Timezone 账号绑定的 IANA 时区(如 Asia/Shanghai),用于指纹一致性;空=不指定。
	Timezone string `json:"timezone"`
}

// resolveClaudeLoginProxy 决定本次登录/导入使用并固定到账号的代理:
// 显式 proxy_url 优先;否则若 use_proxy_pool=true 则从代理池轮询取一个。
// 返回的代理会同时用于 OAuth 交换、后续刷新与推理出站,保证 IP 一致(防风控)。
func (h *Handler) resolveClaudeLoginProxy(rawURL string, usePool bool) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL != "" {
		if err := security.ValidateProxyURL(rawURL); err != nil {
			return "", err
		}
		return rawURL, nil
	}
	if usePool && h.store != nil {
		return strings.TrimSpace(h.store.NextProxy()), nil
	}
	return "", nil
}

// ExchangeClaudeOAuthCode 用 state+code 换取 token 并把账号写入池子。
func (h *Handler) ExchangeClaudeOAuthCode(c *gin.Context) {
	var req exchangeClaudeCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	req.State = strings.TrimSpace(req.State)
	req.Code = strings.TrimSpace(req.Code)
	if req.State == "" || req.Code == "" {
		writeError(c, http.StatusBadRequest, "state 与 code 均为必填")
		return
	}
	proxyURL, err := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	verifier, ok := claudeOAuthTake(req.State)
	if !ok {
		writeError(c, http.StatusBadRequest, "登录会话已过期或不存在，请重新获取授权 URL")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	client := auth.NewClaudeAuth(proxyURL)
	td, err := client.ExchangeCode(ctx, req.Code, req.State, verifier)
	if err != nil {
		writeError(c, http.StatusBadGateway, "换取 token 失败: "+err.Error())
		return
	}
	h.insertClaudeAccount(c, ctx, req.Name, proxyURL, req.Timezone, td, "manual_claude_oauth")
}

type importClaudeTokenReq struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	AccountID    string `json:"account_id"`
	ExpiresAt    string `json:"expires_at"`
	Name         string `json:"name"`
	ProxyURL     string `json:"proxy_url"`
	UseProxyPool bool   `json:"use_proxy_pool"`
	Timezone     string `json:"timezone"`
}

// ImportClaudeToken 直接吃 cmd/claude_login -out 产出的 token JSON 入库。
func (h *Handler) ImportClaudeToken(c *gin.Context) {
	if c.Request.Body == nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, claudeCredentialExportMaxBytes+1))
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取凭据失败")
		return
	}
	documents, err := parseClaudeImportDocuments(raw)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	// Keep the legacy single-document response shape while allowing a portable
	// JSON array / {accounts:[...]} bundle to use the same endpoint.
	ctx, cancel := context.WithTimeout(c.Request.Context(), claudeImportTimeout(len(documents)))
	defer cancel()
	items := make([]claudeImportResultItem, 0, len(documents))
	for _, document := range documents {
		item := claudeImportResultItem{}
		proxyURL := strings.TrimSpace(document.ProxyURL)
		if proxyURL == "" || document.UseProxyPool {
			proxyURL, err = h.resolveClaudeLoginProxy(proxyURL, document.UseProxyPool)
			if err != nil {
				item.Error = "代理URL无效"
				item.status = http.StatusBadRequest
				items = append(items, item)
				continue
			}
		}
		expiresAt := time.Now().Add(30 * time.Minute)
		if rawExpires := strings.TrimSpace(document.ExpiresAt); rawExpires != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, rawExpires); parseErr == nil {
				expiresAt = parsed
			}
		}
		name := security.SanitizeInput(document.Name)
		resolvedGroupIDs, missingGroups, groupErr := h.resolveClaudeGroupRefs(ctx, document.GroupRefs)
		if groupErr != nil {
			item.Error = "分组映射失败: " + groupErr.Error()
			item.status = http.StatusInternalServerError
			items = append(items, item)
			continue
		}
		td := &auth.ClaudeTokenData{
			AccessToken:  document.AccessToken,
			RefreshToken: document.RefreshToken,
			Email:        document.Email,
			AccountUUID:  document.AccountID,
			PlanType:     document.PlanType,
			ExpiresAt:    expiresAt,
		}
		created, createErr := h.createClaudeAccount(ctx, name, proxyURL, document.Timezone, td, "manual_claude_import", &claudeAccountImportOptions{
			Models:             document.Models,
			PlanType:           document.PlanType,
			FingerprintMode:    document.ClaudeFingerprintMode,
			FingerprintHeaders: document.FingerprintHeaders,
			Tags:               document.Tags,
			GroupRefs:          document.GroupRefs,
			ResolvedGroupIDs:   resolvedGroupIDs,
			SkipModelFetch:     len(documents) > 1,
			Enabled:            document.Enabled,
		})
		if createErr != nil {
			item.Error = createErr.Error()
			if typedErr, ok := createErr.(*claudeAccountCreateError); ok {
				item.status = typedErr.Status
			}
			items = append(items, item)
			continue
		}
		item.OK = true
		item.ID = created.ID
		item.Email = created.Email
		item.Warnings = append(item.Warnings, created.Warnings...)
		security.SecurityAuditLog("CLAUDE_ACCOUNT_IMPORTED", fmt.Sprintf("account_id=%d ip=%s", created.ID, c.ClientIP()))
		if len(missingGroups) > 0 {
			item.Warnings = append(item.Warnings, "部分分组未找到: "+strings.Join(missingGroups, ", "))
		}
		items = append(items, item)
	}
	if len(documents) == 1 {
		item := items[0]
		if !item.OK {
			status := item.status
			if status <= 0 {
				status = http.StatusInternalServerError
				if strings.Contains(item.Error, "已存在") || strings.Contains(item.Error, "duplicate") {
					status = http.StatusConflict
				}
			}
			writeError(c, status, item.Error)
			return
		}
		response := gin.H{"message": "成功添加 Claude 账号", "id": item.ID, "email": item.Email}
		if len(item.Warnings) > 0 {
			response["warnings"] = item.Warnings
		}
		c.JSON(http.StatusOK, response)
		return
	}
	imported := 0
	for _, item := range items {
		if item.OK {
			imported++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"total": len(documents), "imported": imported, "failed": len(documents) - imported, "items": items,
	})
}

// RefreshClaudeModels 重新拉取指定 Claude 账号真实可用的模型并落库(动态维护,
// 不用重新导入)。路由 POST /accounts/:id/claude/models。
func (h *Handler) RefreshClaudeModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		writeError(c, http.StatusBadRequest, "该账号不是 Claude 账号")
		return
	}
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	if accessToken == "" {
		writeError(c, http.StatusBadRequest, "账号缺少 access_token,请先刷新或重新导入")
		return
	}
	models, ferr := auth.NewClaudeAuth(h.resolveClaudeModelProxy(id, row.ProxyURL)).FetchModels(ctx, accessToken)
	if ferr != nil {
		writeError(c, http.StatusBadGateway, "拉取可用模型失败: "+ferr.Error())
		return
	}
	if len(models) == 0 {
		writeError(c, http.StatusBadGateway, "未拉到任何可用模型")
		return
	}
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{"models": models}); err != nil {
		writeInternalError(c, err)
		return
	}
	// 直接更新内存账号的 Models,即时生效(LoadAccountByID 对已存在账号是 no-op)。
	if h.store != nil {
		if acc := h.store.FindByID(id); acc != nil {
			acc.Mu().Lock()
			acc.Models = append([]string(nil), models...)
			acc.Mu().Unlock()
		}
	}
	h.invalidateClaudeCatalogCaches()
	c.JSON(http.StatusOK, gin.H{"message": "已更新可用模型", "models": models, "count": len(models)})
}

// RefreshAllClaudeModels 为所有 Claude 账号重新拉取真实可用模型(定价页"模型目录"用)。
// 路由 POST /accounts/claude/models/refresh。
func (h *Handler) RefreshAllClaudeModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelClaude)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	refreshed, failed := 0, 0
	allModels := map[string]struct{}{}
	for _, row := range rows {
		accessToken := strings.TrimSpace(row.GetCredential("access_token"))
		if accessToken == "" {
			failed++
			continue
		}
		models, ferr := auth.NewClaudeAuth(h.resolveClaudeModelProxy(row.ID, row.ProxyURL)).FetchModels(ctx, accessToken)
		if ferr != nil || len(models) == 0 {
			failed++
			continue
		}
		if err := h.db.UpdateCredentials(ctx, row.ID, map[string]interface{}{"models": models}); err != nil {
			failed++
			continue
		}
		if h.store != nil {
			if acc := h.store.FindByID(row.ID); acc != nil {
				acc.Mu().Lock()
				acc.Models = append([]string(nil), models...)
				acc.Mu().Unlock()
			}
		}
		for _, m := range models {
			allModels[m] = struct{}{}
		}
		refreshed++
	}
	if refreshed > 0 {
		h.invalidateClaudeCatalogCaches()
	}
	c.JSON(http.StatusOK, gin.H{
		"message":     "已刷新 Claude 账号可用模型",
		"refreshed":   refreshed,
		"failed":      failed,
		"model_count": len(allModels),
	})
}

// resolveClaudeModelProxy mirrors the request path's proxy precedence for
// control-plane model discovery: an account-level/managed group proxy wins,
// then the row's persisted proxy is used as a safe fallback when the account
// is not currently present in the runtime store.
func (h *Handler) resolveClaudeModelProxy(id int64, fallback string) string {
	if h != nil && h.store != nil {
		if account := h.store.FindByID(id); account != nil {
			if resolved := strings.TrimSpace(h.store.ResolveProxyForAccount(account)); resolved != "" {
				return resolved
			}
		}
	}
	return strings.TrimSpace(fallback)
}

func (h *Handler) invalidateClaudeCatalogCaches() {
	if h == nil {
		return
	}
	h.expireAccountListSnapshot(database.UpstreamChannelClaude)
	h.accountAnalysisCacheMu.Lock()
	if h.accountAnalysisCache != nil {
		delete(h.accountAnalysisCache, database.UpstreamChannelClaude)
	}
	h.accountAnalysisCacheMu.Unlock()
}

// insertClaudeAccount 把一份 Claude token 落库并加载进运行时池子(去重按 account_id)。
// timezone 为空时不指定时区。会为该账号生成一套稳定的 Claude Code 指纹并随凭据落库,
// 之后每次上游请求原样套用(见 proxy/claude_upstream.go)。
// claudePlanOrDefault 取 profile 推导的档位,空则回退通用 "claude"。
func claudePlanOrDefault(plan string) string {
	if p := strings.TrimSpace(plan); p != "" {
		return p
	}
	return "claude"
}

func shouldScheduleClaudeImportWarmup(opts *claudeAccountImportOptions) bool {
	return opts == nil || opts.Enabled == nil || *opts.Enabled
}

func (h *Handler) insertClaudeAccount(c *gin.Context, ctx context.Context, name, proxyURL, timezone string, td *auth.ClaudeTokenData, source string) {
	created, err := h.createClaudeAccount(ctx, name, proxyURL, timezone, td, source, nil)
	if err != nil {
		status := http.StatusInternalServerError
		if createErr, ok := err.(*claudeAccountCreateError); ok && createErr.Status > 0 {
			status = createErr.Status
		}
		writeError(c, status, err.Error())
		return
	}
	security.SecurityAuditLog("CLAUDE_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d ip=%s", created.ID, c.ClientIP()))
	response := gin.H{
		"message": "成功添加 Claude 账号",
		"id":      created.ID,
		"email":   created.Email,
	}
	if len(created.Warnings) > 0 {
		response["warnings"] = created.Warnings
	}
	c.JSON(http.StatusOK, response)
}

// createClaudeAccount is the shared insertion path for OAuth and portable
// credential imports. It never writes token values to logs or response bodies.
func (h *Handler) createClaudeAccount(ctx context.Context, name, proxyURL, timezone string, td *auth.ClaudeTokenData, source string, opts *claudeAccountImportOptions) (claudeAccountCreateResult, error) {
	if h == nil || h.db == nil || td == nil {
		return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusInternalServerError, Message: "Claude 账号存储未初始化"}
	}
	email := strings.TrimSpace(td.Email)
	accountUUID := strings.TrimSpace(td.AccountUUID)

	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusBadRequest, Message: "代理URL无效"}
		}
	}
	if name == "" {
		name = email
	}
	if name == "" {
		name = "claude"
	}

	// 未显式指定时区时,回退到 ClaudeCode 全局默认(系统设置里配置)。
	if strings.TrimSpace(timezone) == "" && h.store != nil {
		timezone = h.store.ClaudeDefaultTimezone()
	}
	if err := validateAccountTimezone(timezone); err != nil {
		return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusBadRequest, Message: err.Error()}
	}

	// 生成稳定指纹(UA / x-app / x-stainless-*),存进 custom_headers 供请求期套用。
	fingerprint := auth.GenerateClaudeFingerprint(timezone)
	customHeaders := fingerprint.Headers()
	if opts != nil && len(opts.FingerprintHeaders) > 0 {
		normalized, err := normalizeClaudeFingerprintHeaders(opts.FingerprintHeaders)
		if err != nil {
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusBadRequest, Message: err.Error()}
		}
		for key, value := range normalized {
			customHeaders[key] = value
		}
	}

	// 动态拉取该账号**真实可用**的模型(Anthropic /v1/models),存进 credentials.models;
	// 失败不阻断导入(DefaultClaudeModelIDsForAccount 会回退到内置兜底集)。
	var claudeModels []string
	if opts != nil && len(opts.Models) > 0 {
		models, modelErr := normalizeClaudeImportModels(opts.Models)
		if modelErr != nil {
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusBadRequest, Message: modelErr.Error()}
		}
		claudeModels = models
	} else if opts != nil && opts.SkipModelFetch {
		// A large bundle should not serialize one upstream /v1/models request per
		// account. Leave the catalog empty so the normal default Claude model set
		// is used; operators can refresh the catalog explicitly after import.
	} else if models, ferr := auth.NewClaudeAuth(proxyURL).FetchModels(ctx, td.AccessToken); ferr == nil && len(models) > 0 {
		claudeModels = models
	} else if ferr != nil {
		log.Printf("拉取 Claude 账号可用模型失败(将用兜底集): %v", ferr)
	}
	planType := claudePlanOrDefault(td.PlanType)
	if opts != nil && strings.TrimSpace(opts.PlanType) != "" {
		planType = claudePlanOrDefault(opts.PlanType)
	}
	fingerprintMode := ""
	if opts != nil && strings.TrimSpace(opts.FingerprintMode) != "" {
		if !auth.IsValidClaudeFingerprintMode(opts.FingerprintMode) {
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusBadRequest, Message: "claude_fingerprint_mode must be preserve, force, or empty"}
		}
		fingerprintMode = auth.NormalizeClaudeFingerprintMode(opts.FingerprintMode)
	}

	credentials := map[string]interface{}{
		"upstream_type":  auth.UpstreamClaude,
		"access_token":   td.AccessToken,
		"refresh_token":  td.RefreshToken,
		"expires_at":     td.ExpiresAt.Format(time.RFC3339),
		"email":          email,
		"account_id":     accountUUID,
		"plan_type":      planType,
		"custom_headers": customHeaders,
		"timezone":       strings.TrimSpace(timezone),
	}
	if fingerprintMode != "" {
		credentials[auth.ClaudeFingerprintModeCredentialKey] = fingerprintMode
	}
	if len(claudeModels) > 0 {
		credentials["models"] = claudeModels
	}
	// 查重与插入置于同一临界区，避免并发导入同一账号各插一条（TOCTOU）。
	// 复用 antigravity/grok 相同的合并去重锁，跨 provider 一致。
	h.mergeDuplicateMu.Lock()
	rows, listErr := h.db.ListActiveByChannel(ctx, database.UpstreamChannelClaude)
	if listErr != nil {
		h.mergeDuplicateMu.Unlock()
		return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusInternalServerError, Message: "查询 Claude 账号失败: " + listErr.Error()}
	}
	for _, row := range rows {
		if accountUUID != "" && strings.EqualFold(strings.TrimSpace(row.GetCredential("account_id")), accountUUID) {
			h.mergeDuplicateMu.Unlock()
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusConflict, Message: fmt.Sprintf("Claude 账号已存在 (id=%d)", row.ID)}
		}
		// A refresh token is itself a stable credential identity. Check it even
		// when the provider also supplied an account_id; providers may rotate or
		// omit that identifier while leaving the same refresh token valid.
		if refreshToken := strings.TrimSpace(td.RefreshToken); refreshToken != "" &&
			strings.TrimSpace(row.GetCredential("refresh_token")) == refreshToken {
			h.mergeDuplicateMu.Unlock()
			return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusConflict, Message: fmt.Sprintf("Claude 凭据已存在 (id=%d)", row.ID)}
		}
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "anthropic", auth.UpstreamClaude, credentials, proxyURL)
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		return claudeAccountCreateResult{}, &claudeAccountCreateError{Status: http.StatusInternalServerError, Message: "保存 Claude 账号失败: " + err.Error()}
	}

	if h.store != nil {
		h.store.AddAccount(&auth.Account{
			DBID:                  id,
			ProxyURL:              proxyURL,
			HealthTier:            auth.HealthTierHealthy,
			UpstreamType:          auth.UpstreamClaude,
			AccessToken:           td.AccessToken,
			RefreshToken:          td.RefreshToken,
			ExpiresAt:             td.ExpiresAt,
			AccountID:             accountUUID,
			Email:                 email,
			PlanType:              planType,
			ClaudeFingerprintMode: fingerprintMode,
			CustomHeaders:         customHeaders,
			Models:                claudeModels,
		})
	}
	warnings := make([]string, 0, 2)
	if opts != nil {
		if len(opts.Tags) > 0 {
			if err := h.db.UpdateAccountTags(ctx, id, opts.Tags); err != nil {
				log.Printf("Claude 账号 %d 标签保存失败: %v", id, err)
				warnings = append(warnings, "标签保存失败")
			} else if h.store != nil {
				h.store.ApplyAccountTags(id, opts.Tags)
			}
		}
		if len(opts.ResolvedGroupIDs) > 0 {
			if err := h.bindImportedAccountGroups(ctx, []int64{id}, opts.ResolvedGroupIDs); err != nil {
				log.Printf("Claude 账号 %d 分组绑定失败: %v", id, err)
				warnings = append(warnings, "分组绑定失败")
			}
		}
		if opts.Enabled != nil && !*opts.Enabled {
			if err := h.db.SetAccountEnabled(ctx, id, false); err != nil {
				log.Printf("Claude 账号 %d 启用状态保存失败: %v", id, err)
				warnings = append(warnings, "启用状态保存失败")
			} else if h.store != nil {
				h.store.ApplyAccountEnabled(id, false)
			}
		}
	}

	h.db.InsertAccountEventAsync(id, "added", source)
	// Keep Claude imports on the bounded warmup queue. ProbeUsageSnapshot routes
	// this account to Anthropic Messages and never to WHAM/Responses.
	if h.store != nil && shouldScheduleClaudeImportWarmup(opts) {
		h.scheduleImportedAccountWarmup(h.store.FindByID(id), id, source)
	}
	return claudeAccountCreateResult{ID: id, Email: email, Warnings: warnings}, nil
}
