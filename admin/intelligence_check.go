package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	ic "github.com/codex2api/internal/intelligencecheck"
	"github.com/gin-gonic/gin"
)

func (h *Handler) registerIntelligenceCheckRoutes(api *gin.RouterGroup) {
	api.GET("/intelligence-check/settings", h.GetIntelligenceSettings)
	api.PUT("/intelligence-check/settings", h.SaveIntelligenceSettings)
	api.GET("/intelligence-check/accounts", h.ListIntelligenceAccounts)
	api.PATCH("/intelligence-check/accounts", h.SetIntelligenceAccounts)
	api.POST("/intelligence-check/runs", h.CreateIntelligenceRuns)
	api.GET("/intelligence-check/runs", h.ListIntelligenceRuns)
	api.GET("/intelligence-check/runs/:id", h.GetIntelligenceRun)
	api.GET("/intelligence-check/bars", h.GetIntelligenceBars)
}
func intelligenceAPIError(c *gin.Context, err error) {
	var conflict *database.IntelligenceCheckTargetConflictError
	if errors.As(err, &conflict) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(), "code": "intelligence_check_target_conflict",
			"target_group_id": conflict.TargetGroupID, "target_group_channel": conflict.TargetGroupChannel,
			"conflict_count": conflict.ConflictCount, "conflicts": conflict.Conflicts,
		})
		return
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, database.ErrIntelligenceCheckRevisionConflict):
		status = http.StatusConflict
	case errors.Is(err, sql.ErrNoRows):
		status = http.StatusNotFound
	case errors.Is(err, database.ErrIntelligenceCheckUnsupportedAccount), errors.Is(err, database.ErrIntelligenceCheckTargetGroup):
		status = http.StatusBadRequest
	}
	c.JSON(status, gin.H{"error": err.Error()})
}
func (h *Handler) GetIntelligenceSettings(c *gin.Context) {
	settings, err := h.db.GetIntelligenceCheckSettings(c.Request.Context())
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, settings)
}
func (h *Handler) SaveIntelligenceSettings(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2*1024*1024)
	var settings ic.Settings
	if err := c.ShouldBindJSON(&settings); err != nil {
		c.JSON(400, gin.H{"error": "无效的检测配置"})
		return
	}
	if err := ic.ValidateSettings(settings); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	saved, err := h.db.SaveIntelligenceCheckSettings(c.Request.Context(), settings)
	if err != nil {
		if errors.Is(err, database.ErrIntelligenceCheckRevisionConflict) || errors.Is(err, database.ErrIntelligenceCheckTargetGroup) {
			intelligenceAPIError(c, err)
		} else {
			c.JSON(400, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(200, saved)
}
func intelligencePagination(c *gin.Context) (page, size int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		page = 100000
	}
	if size < 1 || size > 100 {
		size = 20
	}
	return
}
func (h *Handler) ListIntelligenceAccounts(c *gin.Context) {
	page, size := intelligencePagination(c)
	id, _ := strconv.ParseInt(c.Query("account_id"), 10, 64)
	items, total, err := h.db.ListIntelligenceCheckAccounts(c.Request.Context(), database.IntelligenceCheckAccountFilter{AccountID: id, Search: strings.TrimSpace(c.Query("search")), Channel: c.Query("channel"), Status: c.Query("status"), Offset: (page - 1) * size, Limit: size})
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items, "total": total, "page": page, "page_size": size})
}
func intelligenceIDs(ids []int64, maxCount int) ([]int64, error) {
	if len(ids) == 0 || len(ids) > maxCount {
		return nil, fmt.Errorf("请选择 1–%d 个账号", maxCount)
	}
	seen := map[int64]bool{}
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("无效的账号 ID")
		}
		if !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result, nil
}
func (h *Handler) SetIntelligenceAccounts(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var body struct {
		AccountIDs []int64 `json:"account_ids"`
		Enabled    *bool   `json:"enabled"`
	}
	if c.ShouldBindJSON(&body) != nil || body.Enabled == nil {
		c.JSON(400, gin.H{"error": "请提供账号和检测开关"})
		return
	}
	ids, err := intelligenceIDs(body.AccountIDs, 200)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err = h.db.SetIntelligenceCheckEnabled(c.Request.Context(), ids, *body.Enabled); err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, gin.H{"updated": len(ids)})
}
func (h *Handler) CreateIntelligenceRuns(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var body struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if c.ShouldBindJSON(&body) != nil {
		c.JSON(400, gin.H{"error": "无效的账号集合"})
		return
	}
	ids, err := intelligenceIDs(body.AccountIDs, 200)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	settings, err := h.db.GetIntelligenceCheckSettings(c.Request.Context())
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	enabledQuestion := false
	for _, q := range settings.Questions {
		enabledQuestion = enabledQuestion || q.Enabled
	}
	if !enabledQuestion {
		c.JSON(400, gin.H{"error": "请先保存至少一道启用题目"})
		return
	}
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			c.JSON(400, gin.H{"error": fmt.Sprintf("账号 %d 不在运行时池中", id)})
			return
		}
		model, effort := settings.ModelForChannel(intelligenceAccountChannel(account))
		if err = h.validateQualityTestForAccount(c.Request.Context(), account, qualityTestRequest{Model: model, ReasoningEffort: effort}); err != nil {
			c.JSON(400, gin.H{"error": fmt.Sprintf("账号 %d: %s", id, err)})
			return
		}
	}
	runIDs, err := h.db.EnqueueIntelligenceChecks(c.Request.Context(), ids, time.Now())
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	runs := make([]database.IntelligenceCheckRun, 0, len(runIDs))
	for _, id := range runIDs {
		run, e := h.db.GetIntelligenceCheckRun(c.Request.Context(), id)
		if e != nil {
			intelligenceAPIError(c, e)
			return
		}
		runs = append(runs, run)
	}
	c.JSON(http.StatusAccepted, gin.H{"runs": runs})
}
func (h *Handler) ListIntelligenceRuns(c *gin.Context) {
	page, size := intelligencePagination(c)
	id, _ := strconv.ParseInt(c.Query("account_id"), 10, 64)
	items, total, err := h.db.ListIntelligenceCheckRuns(c.Request.Context(), database.IntelligenceCheckRunFilter{AccountID: id, Search: strings.TrimSpace(c.Query("search")), Channel: c.Query("channel"), Status: c.Query("status"), Offset: (page - 1) * size, Limit: size})
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items, "total": total, "page": page, "page_size": size})
}
func (h *Handler) GetIntelligenceRun(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "无效的检测 ID"})
		return
	}
	run, err := h.db.GetIntelligenceCheckRun(c.Request.Context(), id)
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, gin.H{"run": run})
}
func (h *Handler) GetIntelligenceBars(c *gin.Context) {
	parts := strings.Split(c.Query("account_ids"), ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			c.JSON(400, gin.H{"error": "无效的账号 ID"})
			return
		}
		ids = append(ids, id)
	}
	ids, err := intelligenceIDs(ids, 500)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	bars, err := h.db.IntelligenceCheckBars(c.Request.Context(), ids)
	if err != nil {
		intelligenceAPIError(c, err)
		return
	}
	c.JSON(200, gin.H{"bars": bars})
}
