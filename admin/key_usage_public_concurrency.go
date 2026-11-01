package admin

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetPublicAPIKeyConcurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), publicAPIKeyUsageTimeout)
	defer cancel()
	row := h.publicAPIKeyUsageRow(c, ctx)
	if row == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"current": h.authCacheProxy.APIKeyConcurrency(row.ID)})
}
