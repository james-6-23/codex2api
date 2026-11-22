package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type imageStudioPublicPricing struct {
	UserBillingMode string  `json:"user_billing_mode"`
	ImageUnitPrice  float64 `json:"image_unit_price,omitempty"`
}

type imageStudioQuotaResponse struct {
	ImagePricing        map[string]imageStudioPublicPricing `json:"image_pricing"`
	QuotaLimit          float64                             `json:"quota_limit"`
	QuotaUsed           float64                             `json:"quota_used"`
	QuotaRemaining      *float64                            `json:"quota_remaining"`
	ExpiresAt           *string                             `json:"expires_at"`
	Status              string                              `json:"status"`
	RefreshAfterSeconds int                                 `json:"refresh_after_seconds"`
}

// GetPortalImageQuota is a read-only credential check, independent of the public
// usage-report switch. Exhausted/expired keys may inspect their own quota, while
// generation routes continue to enforce the existing /v1 authorization rules.
func (h *Handler) GetPortalImageQuota(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "服务未就绪")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	enabled, err := h.PublicImageStudioPageEnabled(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !enabled {
		writeError(c, http.StatusNotFound, "生图门户未启用")
		return
	}
	key := extractPublicAPIKey(c)
	if key == "" {
		writeError(c, http.StatusUnauthorized, "缺少 Authorization Bearer API Key")
		return
	}
	// Read the database row directly: auth-cache snapshots exclude live usage.
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			security.SecurityAuditLog("IMAGE_STUDIO_QUOTA_AUTH_FAILED", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
			writeError(c, http.StatusUnauthorized, "API Key 无效或不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !row.Enabled {
		writeError(c, http.StatusUnauthorized, "API Key 已停用")
		return
	}
	result := imageStudioQuotaResponse{
		QuotaLimit: row.QuotaLimit,
		QuotaUsed:  row.QuotaUsed,
		Status:     "active",
		// Usage settlement is asynchronous. Let the UI refresh again after a
		// flush interval instead of forcing a global log flush on every read.
		RefreshAfterSeconds: h.db.GetUsageLogFlushIntervalSeconds() + 1,
	}
	result.ImagePricing = make(map[string]imageStudioPublicPricing)
	for _, base := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		for _, suffix := range []string{"", "-2k", "-4k"} {
			model := base + suffix
			p := database.GetModelPricing(model)
			price := imageStudioPublicPricing{UserBillingMode: database.UserBillingModeToken}
			if p.UserBillingMode == database.UserBillingModePerImage {
				price.UserBillingMode, price.ImageUnitPrice = p.UserBillingMode, p.ImageUnitPrice
			}
			result.ImagePricing[model] = price
		}
	}
	if row.QuotaLimit > 0 {
		remaining := max(0, row.QuotaLimit-row.QuotaUsed)
		result.QuotaRemaining = &remaining
	}
	if row.ExpiresAt.Valid {
		expires := row.ExpiresAt.Time.Format(time.RFC3339)
		result.ExpiresAt = &expires
	}
	if row.IsExpired(time.Now()) {
		result.Status = "expired"
	} else if row.IsQuotaExhausted() {
		result.Status = "quota_exhausted"
	}
	c.JSON(http.StatusOK, result)
}
