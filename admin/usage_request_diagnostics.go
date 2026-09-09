package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetUsageRequestDiagnostics(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的日志 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	detail, err := h.db.GetUsageRequestDiagnostics(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "日志不存在或已清理")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取请求诊断失败")
		return
	}
	c.JSON(http.StatusOK, detail)
}
