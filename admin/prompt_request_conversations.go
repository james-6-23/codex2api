package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
)

func (h *Handler) ListPromptRequestConversations(c *gin.Context) {
	const pageSize = 25
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	groups, err := h.db.ListPromptRequestConversations(c.Request.Context(), max(before, 0), pageSize+1)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	days, err := h.db.GetPromptRequestRetentionDays(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	var next int64
	if len(groups) > pageSize {
		groups = groups[:pageSize]
		next = groups[pageSize-1].ID
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{
		"groups": groups, "next_before": next, "enabled": h.promptRequestRecording, "retention_days": days,
	})
}

func (h *Handler) ListPromptRequestRounds(c *gin.Context) {
	const pageSize = 25
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid conversation ID")
		return
	}
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	rounds, total, err := h.db.ListPromptRequestRounds(c.Request.Context(), id, before, pageSize+1)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "Request conversation not found")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	var next int64
	if len(rounds) > pageSize {
		rounds = rounds[:pageSize]
		next = rounds[pageSize-1].ID
	}
	slices.Reverse(rounds)
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{"rounds": rounds, "total": total, "next_before": next})
}
