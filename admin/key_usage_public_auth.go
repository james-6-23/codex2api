package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func (h *Handler) publicAPIKeyUsageRow(c *gin.Context, ctx context.Context) *database.APIKeyRow {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "服务未就绪")
		return nil
	}
	enabled, err := h.PublicAPIKeyUsagePageEnabled(ctx)
	if err != nil {
		writeInternalError(c, err)
		return nil
	}
	if !enabled {
		writeError(c, http.StatusNotFound, "API Key 自助用量页未启用")
		return nil
	}
	key := extractPublicAPIKey(c)
	if key == "" {
		writeError(c, http.StatusUnauthorized, "缺少 Authorization Bearer API Key")
		return nil
	}
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		security.SecurityAuditLog("KEY_USAGE_AUTH_FAILED", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
		writeError(c, http.StatusUnauthorized, "API Key 无效或不存在")
		return nil
	}
	if err != nil {
		writeInternalError(c, err)
		return nil
	}
	// 过期或额度用尽仍可查询，管理员停用的凭据直接拒绝。
	if !row.Enabled {
		security.SecurityAuditLog("KEY_USAGE_AUTH_FAILED_DISABLED_KEY", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
		writeError(c, http.StatusUnauthorized, "API Key 已停用")
		return nil
	}
	return row
}
