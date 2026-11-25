package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ResetAPIKeyWeeklyQuota renews the persisted seven-day usage of one API key
// and exempts its eligible cost from the rolling thirty-day cost limit.
// Historical billing and the other limits are preserved. This action is only
// registered in the authenticated admin route group.
//
// POST /api/admin/keys/:id/reset-weekly-quota
func (h *Handler) ResetAPIKeyWeeklyQuota(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的 API Key ID")
		return
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "请提供 JSON 请求体及 request_id UUID")
		return
	}
	requestID, err := uuid.Parse(body.RequestID)
	if err != nil || requestID == uuid.Nil {
		writeError(c, http.StatusBadRequest, "request_id 必须是非空 UUID；重试时使用同一个值")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, err := h.db.ResetAPIKeyWeeklyQuota(ctx, id, requestID.String())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "API Key 不存在")
		return
	case errors.Is(err, database.ErrAPIKeyWeeklyQuotaResetRequiresLimits):
		writeError(c, http.StatusBadRequest, "请先为此 API Key 设置大于 0 的 7 天和 30 天金额上限")
		return
	case err != nil:
		writeInternalError(c, err)
		return
	}

	// Repeat eviction on an idempotent replay too: the first response may have
	// been lost after committing the reset but before invalidating the caches.
	h.deleteRuntimeCache(ctx, adminAPIKeyCacheNamespace, result.Key)
	for _, label := range []string{"7d", "30d"} {
		h.deleteRuntimeCache(ctx, adminAPIKeyLimitsCacheNamespace, fmt.Sprintf("%d:usage:%s", id, label))
	}
	security.SecurityAuditLog("API_KEY_WEEKLY_QUOTA_RESET", fmt.Sprintf(
		"id=%d request_id=%s exempted_cost=%g replayed=%t ip=%s",
		id, result.RequestID, result.ExemptedCost, result.Replayed, security.SanitizeLog(c.ClientIP()),
	))
	c.JSON(http.StatusOK, gin.H{
		"message":       "API Key 7 天用量已重置，对应实际费用已从 30 天限额计费用量中免计",
		"request_id":    result.RequestID,
		"reset_at":      result.ResetAt,
		"exempted_cost": result.ExemptedCost,
		"replayed":      result.Replayed,
	})
}
