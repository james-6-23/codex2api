package admin

import (
	"log"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"

	"github.com/gin-gonic/gin"
)

// Antigravity 渠道设置：目前是模型重定向——下游请求不带思考强度后缀的逻辑模型
// （gemini-3.8-flash）时，自动按配置落到某个固定档位（gemini-3.8-flash-high）。
// 默认只补位缺失的 reasoning.effort，开启覆盖后连显式 effort 也按重定向走。

type antigravitySettingsResponse struct {
	ModelRedirects          map[string]string                 `json:"model_redirects"`
	RedirectOverridesEffort bool                              `json:"redirect_overrides_effort"`
	Choices                 []proxy.AntigravityRedirectChoice `json:"choices"`
}

func buildAntigravitySettingsResponse(settings auth.AntigravitySettings) antigravitySettingsResponse {
	redirects := settings.ModelRedirects
	if redirects == nil {
		redirects = map[string]string{}
	}
	return antigravitySettingsResponse{
		ModelRedirects:          redirects,
		RedirectOverridesEffort: settings.RedirectOverridesEffort,
		Choices:                 proxy.AntigravityRedirectChoices(),
	}
}

// GetAntigravitySettings 返回当前 Antigravity 渠道设置与可选的重定向档位。
// GET /api/admin/settings/antigravity
func (h *Handler) GetAntigravitySettings(c *gin.Context) {
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(auth.ConfiguredAntigravitySettings()))
}

// UpdateAntigravitySettings 保存 Antigravity 渠道设置。model_redirects 整体替换：
// 值为空的条目表示取消该模型的重定向。
// PUT /api/admin/settings/antigravity
// {"model_redirects":{"gemini-3.8-flash":"gemini-3.8-flash-high"},"redirect_overrides_effort":false}
func (h *Handler) UpdateAntigravitySettings(c *gin.Context) {
	var req struct {
		ModelRedirects          *map[string]string `json:"model_redirects"`
		RedirectOverridesEffort *bool              `json:"redirect_overrides_effort"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.ModelRedirects == nil && req.RedirectOverridesEffort == nil {
		writeError(c, http.StatusBadRequest, "缺少 model_redirects 或 redirect_overrides_effort 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	settings := auth.ConfiguredAntigravitySettings()
	if req.ModelRedirects != nil {
		settings.ModelRedirects = *req.ModelRedirects
	}
	if req.RedirectOverridesEffort != nil {
		settings.RedirectOverridesEffort = *req.RedirectOverridesEffort
	}
	normalized, err := auth.NormalizeAntigravitySettings(settings)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := proxy.ValidateAntigravityModelRedirects(normalized.ModelRedirects); err != nil {
		writeError(c, http.StatusBadRequest, "model_redirects 非法: "+err.Error())
		return
	}
	encoded, err := auth.EncodeAntigravitySettings(normalized)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.db.SaveAntigravityConfig(c.Request.Context(), encoded); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	auth.SetConfiguredAntigravitySettings(normalized)
	log.Printf("设置已更新: antigravity_config redirects=%d overrides_effort=%v", len(normalized.ModelRedirects), normalized.RedirectOverridesEffort)
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(normalized))
}
