package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

// 订阅状态查询与手动刷新：状态对象由服务端按业务时区计算，前端不再自行按
// 本机时间判定"今日到期/已超时"。手动刷新绕过后台探针的 6 小时节流，但有独立
// 的短限流——订阅端点在 Cloudflare 后面靠指纹伪装通过，连点只会抬高被拦概率。

// subscriptionManualRefreshMinInterval 同一账号两次手动刷新的最小间隔。
const subscriptionManualRefreshMinInterval = 30 * time.Second

// subscriptionStatusViewForRow 从 DB 行组装订阅状态对象（列表/详情共用）。
func subscriptionStatusViewForRow(row *database.AccountRow, planType string) *auth.SubscriptionStatusView {
	return subscriptionStatusViewForRowAt(row, planType, time.Now())
}

func subscriptionStatusViewForRowAt(row *database.AccountRow, planType string, now time.Time) *auth.SubscriptionStatusView {
	if row == nil {
		return nil
	}
	var expiresAt time.Time
	if raw := strings.TrimSpace(row.GetCredential("subscription_expires_at")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			expiresAt = parsed
		}
	}
	meta := auth.SubscriptionMetaFromCredentials(row.GetCredential)
	return auth.BuildSubscriptionStatusView(planType, expiresAt, meta, now, time.Local)
}

// GetAccountSubscription 返回账号当前订阅状态对象（幂等，不打上游）。
// GET /api/accounts/:id/subscription
func (h *Handler) GetAccountSubscription(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil {
		c.JSON(http.StatusOK, gin.H{"supported": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"supported": true, "subscription": view})
}

// RefreshAccountSubscription 立即向订阅提供方查询一次并返回结果分类与最新状态。
// POST /api/accounts/:id/subscription/refresh
//
// 结果 outcome：updated / unchanged / no_subscription / unsupported / failed。
// 失败不改 HTTP 状态码（仍 200），由 outcome + error 表达；只有账号不存在、
// 参数非法与限流才走 4xx。
func (h *Handler) RefreshAccountSubscription(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}
	// 只有 Codex/ChatGPT 账号有订阅提供方可查；其他渠道直接报不支持。
	if account.IsOpenAIResponsesAPI() || account.IsGrokAPI() || account.IsAntigravityAPI() || account.IsClaudeOAuth() || account.IsClaudeAPIKey() {
		c.JSON(http.StatusOK, gin.H{"outcome": string(proxy.SubscriptionSyncUnsupported)})
		return
	}

	now := time.Now()
	if !auth.SubscriptionPlanTracked(account.GetPlanType(), account.GetSubscriptionExpiresAt()) {
		c.JSON(http.StatusOK, gin.H{"outcome": string(proxy.SubscriptionSyncUnsupported)})
		return
	}
	if checkedAt := account.SubscriptionMetaSnapshot().CheckedAt; !checkedAt.IsZero() {
		if elapsed := now.Sub(checkedAt); elapsed >= 0 && elapsed < subscriptionManualRefreshMinInterval {
			retryAfter := int((subscriptionManualRefreshMinInterval - elapsed).Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":        "刚刚已查询过订阅状态，请稍后再试",
				"retry_after":  retryAfter,
				"subscription": account.SubscriptionStatusView(now),
			})
			return
		}
	}
	if !account.BeginSubscriptionSync() {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":        "订阅状态同步进行中",
			"retry_after":  5,
			"subscription": account.SubscriptionStatusView(now),
		})
		return
	}
	defer account.EndSubscriptionSync()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result := proxy.SyncSubscriptionExpiry(ctx, h.store, account, h.store.ResolveProxyForAccount(account))

	resp := gin.H{"outcome": string(result.Outcome)}
	if result.Err != nil {
		resp["error"] = proxy.SubscriptionErrorSummary(result.Err)
	}
	if view := account.SubscriptionStatusView(time.Now()); view != nil {
		resp["subscription"] = view
	}
	if !result.ExpiresAt.IsZero() {
		resp["subscription_expires_at"] = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
}

// 账号列表「订阅状态」筛选值。expiring_* 含今日到期；pending/failed 看同步状态；
// unknown 是"套餐需要跟踪但从未拿到任何到期信息"。
const (
	subscriptionFilterActive        = "active"
	subscriptionFilterExpiring9d    = "expiring_9d"
	subscriptionFilterExpiring3d    = "expiring_3d"
	subscriptionFilterExpiringToday = "expiring_today"
	subscriptionFilterExpired       = "expired"
	subscriptionFilterGracePeriod   = "grace_period"
	subscriptionFilterPending       = "pending"
	subscriptionFilterFailed        = "failed"
	subscriptionFilterUnknown       = "unknown"
)

var validSubscriptionFilters = map[string]bool{
	"": true, "all": true,
	subscriptionFilterActive: true, subscriptionFilterExpiring9d: true, subscriptionFilterExpiring3d: true,
	subscriptionFilterExpiringToday: true, subscriptionFilterExpired: true, subscriptionFilterGracePeriod: true,
	subscriptionFilterPending: true, subscriptionFilterFailed: true, subscriptionFilterUnknown: true,
}

// subscriptionFilterMatches 按服务端算出的订阅状态对象过滤列表项；不跟踪订阅的
// 账号（api / 无到期时间的 free）任何筛选值都不命中。
func subscriptionFilterMatches(item *accountListSnapshotItem, filter string, now time.Time) bool {
	if item == nil || item.Row == nil {
		return false
	}
	view := subscriptionStatusViewForRowAt(item.Row, item.PlanType, now)
	if view == nil {
		return false
	}
	switch filter {
	case subscriptionFilterActive:
		return view.BusinessStatus == auth.SubscriptionStatusActive
	case subscriptionFilterExpiring9d:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday ||
			(view.BusinessStatus == auth.SubscriptionStatusActive && view.DaysRemaining <= 9)
	case subscriptionFilterExpiring3d:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday ||
			(view.BusinessStatus == auth.SubscriptionStatusActive && view.DaysRemaining <= 3)
	case subscriptionFilterExpiringToday:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday
	case subscriptionFilterExpired:
		return view.BusinessStatus == auth.SubscriptionStatusExpired
	case subscriptionFilterGracePeriod:
		return view.BusinessStatus == auth.SubscriptionStatusGracePeriod
	case subscriptionFilterPending:
		return view.SyncState == auth.SubscriptionSyncPending
	case subscriptionFilterFailed:
		return view.SyncState == auth.SubscriptionSyncFailed
	case subscriptionFilterUnknown:
		return view.BusinessStatus == auth.SubscriptionStatusUnknown && view.SyncState != auth.SubscriptionSyncPending
	}
	return true
}
