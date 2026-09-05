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

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Rule IDs identify persistent counters. Allowing a schedule or pattern to change
// under the same ID could split a week's budget or silently reuse another budget.
func normalizeAdminAPIKeyModelRequestLimits(in, existing []database.APIKeyModelRequestLimit) ([]database.APIKeyModelRequestLimit, error) {
	byID := make(map[string]database.APIKeyModelRequestLimit, len(existing))
	for _, rule := range existing {
		byID[rule.ID] = rule
	}
	for _, rule := range in {
		if id := strings.TrimSpace(rule.ID); id != "" {
			if _, ok := byID[id]; !ok {
				return nil, fmt.Errorf("limits.model_request_limits: 未知规则 ID %q；新增规则请省略 id，由服务端生成", id)
			}
		}
	}
	clean, err := database.NormalizeAPIKeyModelRequestLimits(in)
	if err != nil {
		return nil, err
	}
	for _, rule := range clean {
		old, ok := byID[rule.ID]
		if !ok {
			continue
		}
		if rule.Model != old.Model || rule.Window != old.Window || rule.Timezone != old.Timezone || rule.ResetWeekday != old.ResetWeekday || rule.ResetTime != old.ResetTime {
			return nil, fmt.Errorf("limits.model_request_limits: 规则 %q 的模型与重置时间不可直接修改；请删除旧规则并新增规则。修改次数上限时请保留原 id", rule.ID)
		}
	}
	return clean, nil
}

// GetAPIKeyModelRequestUsage reads the same durable counters used at dispatch.
// GET /api/admin/keys/:id/model-request-usage
func (h *Handler) GetAPIKeyModelRequestUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的 API Key ID")
		return
	}
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "服务未就绪")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && row == nil) {
		writeError(c, http.StatusNotFound, "API Key 不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	usage, err := h.db.GetAPIKeyModelRequestUsage(ctx, id, row.Limits.ModelRequestLimits, time.Now())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if usage == nil {
		usage = []database.APIKeyModelRequestUsage{}
	}
	c.JSON(http.StatusOK, gin.H{"model_request_usage": usage})
}
