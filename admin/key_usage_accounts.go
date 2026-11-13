package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *Handler) keyUsagePermissions(ctx context.Context, keyID int64) (database.KeyUsageAccountPermissions, error) {
	global, err := h.db.GetKeyUsageAccountPermissions(ctx, 0)
	if err != nil {
		return global, err
	}
	key, err := h.db.GetKeyUsageAccountPermissions(ctx, keyID)
	if err != nil {
		return key, err
	}
	return database.KeyUsageAccountPermissions{ViewEnabled: global.ViewEnabled && key.ViewEnabled, ResetEnabled: global.ViewEnabled && global.ResetEnabled && key.ViewEnabled && key.ResetEnabled}, nil
}

func (h *Handler) keyUsagePermissionID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 0 {
		writeError(c, http.StatusBadRequest, "无效的 API Key ID")
		return 0, false
	}
	if id != 0 {
		if _, err := h.db.GetAPIKeyByID(c.Request.Context(), id); errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "API Key 不存在")
			return 0, false
		} else if err != nil {
			writeInternalError(c, err)
			return 0, false
		}
	}
	return id, true
}

func (h *Handler) GetKeyUsageAccountPermissions(c *gin.Context) {
	id, ok := h.keyUsagePermissionID(c)
	if !ok {
		return
	}
	p, err := h.db.GetKeyUsageAccountPermissions(c.Request.Context(), id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) SetKeyUsageAccountPermissions(c *gin.Context) {
	id, ok := h.keyUsagePermissionID(c)
	if !ok {
		return
	}
	var req struct {
		ViewEnabled  *bool `json:"view_enabled"`
		ResetEnabled *bool `json:"reset_enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ViewEnabled == nil || req.ResetEnabled == nil {
		writeError(c, http.StatusBadRequest, "需要 view_enabled 和 reset_enabled")
		return
	}
	p := database.KeyUsageAccountPermissions{ViewEnabled: *req.ViewEnabled, ResetEnabled: *req.ResetEnabled}
	if err := h.db.SetKeyUsageAccountPermissions(c.Request.Context(), id, p); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// keyUsageAccountAllowed checks both account-side reverse authorization and
// the scheduler's group/channel/plan restrictions. The fresh key row's group
// restrictions are also checked so stale scheduler state cannot widen access.
func (h *Handler) keyUsageAccountAllowed(row *database.APIKeyRow, account *auth.Account, groups, allowedKeys []int64) bool {
	if row == nil || account == nil || h.store == nil || !auth.APIKeyRowAllowsAccount(row, account) || !keyUsageAccountScopeAllowed(row, groups, allowedKeys) {
		return false
	}
	return true
}

func keyUsageAccountScopeAllowed(row *database.APIKeyRow, groups, allowedKeys []int64) bool {
	if row == nil {
		return false
	}
	if len(allowedKeys) > 0 {
		allowed := false
		for _, keyID := range allowedKeys {
			if keyID == row.ID {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	if len(row.AllowedGroupIDs) > 0 {
		allowed := false
		for _, group := range groups {
			for _, candidate := range row.AllowedGroupIDs {
				if group == candidate {
					allowed = true
					break
				}
			}
			for _, candidate := range row.Limits.NoAffinityGroupIDs {
				if group == candidate {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// An account can exist in an allowed group without being loaded in the pool
// (for example, a missing credential). Return its safe metadata while failing
// closed when its channel or plan cannot be established from persisted data.
func keyUsageUnloadedAccountAllowed(row *database.APIKeyRow, account database.KeyUsageAccountAccess, groups []int64) bool {
	if !keyUsageAccountScopeAllowed(row, groups, account.AllowedAPIKeyIDs) {
		return false
	}
	upstream := strings.ToLower(strings.TrimSpace(account.UpstreamType))
	switch row.Limits.ResolveUpstreamChannel() {
	case database.UpstreamChannelCodex:
		if upstream == "grok" || upstream == "antigravity" || upstream == "claude" {
			return false
		}
	case database.UpstreamChannelGrok, database.UpstreamChannelAntigravity, database.UpstreamChannelClaude:
		if upstream != row.Limits.ResolveUpstreamChannel() {
			return false
		}
	}
	if len(row.Limits.PlanAllow) > 0 {
		if upstream == "grok" || upstream == "antigravity" {
			return false
		}
		matched := false
		for _, plan := range row.Limits.PlanAllow {
			if strings.EqualFold(strings.TrimSpace(plan), strings.TrimSpace(account.PlanType)) && strings.TrimSpace(plan) != "" {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (h *Handler) GetPublicKeyUsageAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), publicAPIKeyUsageTimeout)
	defer cancel()
	row := h.publicAPIKeyUsageRow(c, ctx)
	if row == nil {
		return
	}
	p, err := h.keyUsagePermissions(ctx, row.ID)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !p.ViewEnabled {
		writeError(c, http.StatusNotFound, "账号用量查询未启用")
		return
	}
	if h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号池未就绪")
		return
	}
	rowsByID, err := h.db.ListKeyUsageAccountAccess(ctx)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	memberships, err := h.db.ListAccountGroupMemberships(ctx)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	groupRows, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	uncertain, err := h.db.ListKeyUsageResetUncertain(ctx)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	visibleGroups := make(map[int64]bool)
	items := make([]gin.H, 0)
	runtime := make(map[int64]*auth.Account)
	for _, account := range h.store.Accounts() {
		if account != nil {
			runtime[account.DBID] = account
		}
	}
	ids := make([]int64, 0, len(rowsByID))
	for id := range rowsByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		accountRow, account := rowsByID[id], runtime[id]
		if account == nil && !keyUsageUnloadedAccountAllowed(row, accountRow, memberships[id]) {
			continue
		}
		if account != nil && !h.keyUsageAccountAllowed(row, account, memberships[id], accountRow.AllowedAPIKeyIDs) {
			continue
		}
		groups := make([]int64, 0)
		for _, groupID := range memberships[id] {
			if len(row.AllowedGroupIDs) == 0 {
				groups = append(groups, groupID)
				continue
			}
			for _, allowed := range append(append([]int64(nil), row.AllowedGroupIDs...), row.Limits.NoAffinityGroupIDs...) {
				if groupID == allowed {
					groups = append(groups, groupID)
					break
				}
			}
		}
		for _, groupID := range groups {
			visibleGroups[groupID] = true
		}
		item := gin.H{"id": id, "name": accountRow.Name, "email": accountRow.Email, "plan_type": accountRow.PlanType, "status": "unavailable", "group_ids": groups, "reset_allowed": false, "reset_uncertain": false, "usage_percent_5h": nil, "usage_percent_7d": nil}
		if account == nil {
			items = append(items, item)
			continue
		}
		u := account.GetAccountLiveUsageSnapshot()
		pending := uncertain[resetCreditDatabaseScope(account)]
		canReset := p.ResetEnabled && !row.IsExpired(time.Now()) && !pending && isKeyUsageResetEligible(account)
		item["plan_type"], item["status"] = u.PlanType, u.Status
		item["reset_allowed"], item["reset_uncertain"] = canReset, pending
		item["active_requests"], item["dynamic_concurrency_limit"] = account.GetActiveRequests(), account.GetDynamicConcurrencyLimit()
		item["long_window_seconds"] = account.GetWindow7dSeconds()
		if u.UsagePercent5hValid {
			item["usage_percent_5h"] = u.UsagePercent5h
		}
		if u.UsagePercent7dValid {
			item["usage_percent_7d"] = u.UsagePercent7d
		}
		if !u.Reset5hAt.IsZero() {
			item["reset_5h_at"] = u.Reset5hAt
		}
		if !u.Reset7dAt.IsZero() {
			item["reset_7d_at"] = u.Reset7dAt
		}
		if !u.UsageUpdatedAt5h.IsZero() {
			item["usage_updated_at_5h"] = u.UsageUpdatedAt5h
		}
		if !u.UsageUpdatedAt.IsZero() {
			item["usage_updated_at_7d"] = u.UsageUpdatedAt
		}
		if count, ok := account.GetRateLimitResetCredits(); ok {
			item["reset_credits"] = count
		}
		items = append(items, item)
	}
	groups := make([]gin.H, 0)
	for _, group := range groupRows {
		if visibleGroups[group.ID] {
			groups = append(groups, gin.H{"id": group.ID, "name": group.Name})
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"accounts": items, "groups": groups, "reset_enabled": p.ResetEnabled})
}

// ClearKeyUsageResetUncertain is an admin-only acknowledgement after checking
// the upstream account; it does not redeem a credit or clear the cooldown.
func (h *Handler) ClearKeyUsageResetUncertain(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	if h.store == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "服务未就绪")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	lock := h.resetCreditLock(account)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	acquired, release, err := h.acquireResetCreditLease(ctx, account)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !acquired {
		writeError(c, http.StatusConflict, "该工作区正在执行额度重置")
		return
	}
	defer release()
	if err := h.db.ClearKeyUsageResetUncertain(ctx, resetCreditDatabaseScope(account)); err != nil {
		writeInternalError(c, err)
		return
	}
	security.SecurityAuditLog("KEY_USAGE_RESET_RECONCILED", fmt.Sprintf("account_id=%d ip=%s", id, security.SanitizeLog(c.ClientIP())))
	c.JSON(http.StatusOK, gin.H{"message": "已解除待核实状态"})
}

func isKeyUsageResetEligible(account *auth.Account) bool {
	return account != nil && !account.IsGrokAPI() && !account.IsAntigravityAPI() && !account.IsClaudeOAuth() && !account.IsOpenAIResponsesAPI() && account.GetAccessToken() != ""
}

func (h *Handler) ResetPublicKeyUsageAccountCredits(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), publicAPIKeyUsageTimeout)
	defer cancel()
	row := h.publicAPIKeyUsageRow(c, ctx)
	if row == nil {
		return
	}
	if row.IsExpired(time.Now()) {
		writeError(c, http.StatusForbidden, "API Key 已过期")
		return
	}
	p, err := h.keyUsagePermissions(ctx, row.ID)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !p.ResetEnabled {
		writeError(c, http.StatusForbidden, "使用重置卡未启用")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	if h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号池未就绪")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在或无权访问")
		return
	}
	lock := h.resetCreditLock(account)
	lock.Lock()
	defer lock.Unlock()
	// Re-read the key and policy after waiting for the workspace lock.
	pageEnabled, err := h.PublicAPIKeyUsagePageEnabled(ctx)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !pageEnabled {
		writeError(c, http.StatusForbidden, "API Key 自助用量页未启用")
		return
	}
	row, err = h.db.GetAPIKeyByID(ctx, row.ID)
	if err != nil || !row.Enabled || row.IsExpired(time.Now()) {
		writeError(c, http.StatusForbidden, "账号授权已变更")
		return
	}
	accountRow, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		writeError(c, http.StatusNotFound, "账号不存在或无权访问")
		return
	}
	groups, err := h.db.GetAccountGroupIDs(ctx, id)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !h.keyUsageAccountAllowed(row, account, groups, accountRow.GetCredentialInt64Slice("allowed_api_key_ids")) {
		writeError(c, http.StatusNotFound, "账号不存在或无权访问")
		return
	}
	if !isKeyUsageResetEligible(account) {
		writeError(c, http.StatusConflict, "该账号不支持使用重置卡")
		return
	}
	p, err = h.keyUsagePermissions(ctx, row.ID)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !p.ResetEnabled {
		writeError(c, http.StatusForbidden, "使用重置卡未启用")
		return
	}
	if count, ok := account.GetRateLimitResetCredits(); ok && count <= 0 {
		writeError(c, http.StatusConflict, "该账号没有可用的主动重置次数")
		return
	}
	requestID, err := uuid.Parse(c.GetHeader("Idempotency-Key"))
	if err != nil || requestID == uuid.Nil {
		writeError(c, http.StatusBadRequest, "需要有效的 Idempotency-Key UUID")
		return
	}
	consumeCtx, stop := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
	defer stop()
	security.SecurityAuditLog("KEY_USAGE_RESET_CREDIT", fmt.Sprintf("key_id=%d account_id=%d ip=%s", row.ID, id, security.SanitizeLog(c.ClientIP())))
	outcome, failure := h.consumeResetCreditLocked(consumeCtx, account, requestID.String(), fmt.Sprintf("key_usage:key_id=%d", row.ID))
	if failure != nil {
		// The upstream may have consumed the credit even when its reply was lost.
		// Conservatively fence new public requests until the normal cooldown ends.
		h.markResetCreditSuccess(account)
		log.Printf("[key-usage] reset failed: key_id=%d account_id=%d upstream_status=%d request_error_type=%T refresh_error_type=%T", row.ID, id, failure.Status, failure.RequestErr, failure.RefreshErr)
		writeError(c, http.StatusBadGateway, "使用重置卡失败，请联系管理员确认后再重试")
		return
	}
	if outcome.InProgress {
		writeError(c, http.StatusConflict, "该工作区正在执行额度重置，请稍后重试")
		return
	}
	if outcome.AlreadyHandled {
		writeError(c, http.StatusConflict, "该工作区近期已使用重置卡，请稍后重试")
		return
	}
	refreshed := waitUsageRefreshAfterReset(outcome.UsageRefreshed)
	c.JSON(http.StatusOK, gin.H{"message": "已重置额度", "usage_refreshed": refreshed, "remaining": outcome.Remaining, "windows_reset": outcome.WindowsReset})
}
