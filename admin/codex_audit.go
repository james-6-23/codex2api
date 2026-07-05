package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetCodexAuditReport(c *gin.Context) {
	end := time.Now()
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, "end 参数格式无效，需要 RFC3339")
			return
		}
		end = parsed
	}

	start := time.Time{}
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, "start 参数格式无效，需要 RFC3339")
			return
		}
		start = parsed
	}
	if start.IsZero() {
		hours := 0.5
		if raw := strings.TrimSpace(c.Query("hours")); raw != "" {
			if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed > 0 {
				hours = parsed
			}
		}
		if hours > 168 {
			hours = 168
		}
		start = end.Add(-time.Duration(hours * float64(time.Hour)))
	}

	bucketMinutes := positiveQueryInt(c, "bucket_minutes", 5)
	limit := positiveQueryInt(c, "limit", 20)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	report, err := h.db.BuildCodexAuditReport(ctx, database.CodexAuditQuery{
		Start:         start,
		End:           end,
		BucketMinutes: bucketMinutes,
		Limit:         limit,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, report)
}
