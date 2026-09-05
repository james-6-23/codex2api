package admin

import (
	"context"
	"log"
	"net/http"

	"github.com/codex2api/database"

	"github.com/gin-gonic/gin"
)

// 管理台可见渠道设置：控制仪表盘与账号管理里展示哪些上游渠道。
// 只影响管理台展示，不影响调度与 API 行为；Codex 作为兜底渠道始终显示。

type visibleChannelsResponse struct {
	Channels []string `json:"channels"`
	All      []string `json:"all"`
	Fallback string   `json:"fallback"`
}

func (h *Handler) visibleChannels(ctx context.Context) []string {
	if h == nil || h.db == nil {
		return database.NormalizeVisibleChannels(nil)
	}
	cfg, err := h.db.LoadVisibleChannelsConfig(ctx)
	if err != nil {
		log.Printf("读取可见渠道设置失败，按全部显示处理: %v", err)
		return database.NormalizeVisibleChannels(nil)
	}
	return cfg.Effective()
}

func buildVisibleChannelsResponse(channels []string) visibleChannelsResponse {
	return visibleChannelsResponse{
		Channels: channels,
		All:      append([]string(nil), database.AllUpstreamChannels...),
		Fallback: database.FallbackVisibleChannel,
	}
}

// GetVisibleChannelsSettings 返回当前可见渠道。
// GET /api/admin/settings/visible-channels
func (h *Handler) GetVisibleChannelsSettings(c *gin.Context) {
	c.JSON(http.StatusOK, buildVisibleChannelsResponse(h.visibleChannels(c.Request.Context())))
}

// UpdateVisibleChannelsSettings 保存可见渠道；未知渠道被丢弃，兜底渠道自动补回。
// PUT /api/admin/settings/visible-channels  {"channels":["codex","grok"]}
func (h *Handler) UpdateVisibleChannelsSettings(c *gin.Context) {
	var req struct {
		Channels []string `json:"channels"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Channels == nil {
		writeError(c, http.StatusBadRequest, "缺少 channels 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	normalized := database.NormalizeVisibleChannels(req.Channels)
	if err := h.db.SaveVisibleChannelsConfig(c.Request.Context(),
		database.VisibleChannelsConfig{Channels: normalized}); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, buildVisibleChannelsResponse(normalized))
}
