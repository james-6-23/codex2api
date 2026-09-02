package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// claudeGlobalConfigDTO 是 ClaudeCode 全局配置的读写载体(系统设置里的独立模块)。
// 全体 Claude 账号默认遵守;个体账号可在「编辑账号」里覆盖。
type claudeGlobalConfigDTO struct {
	FingerprintMode    string `json:"fingerprint_mode"`     // preserve / force(空=preserve)
	DefaultTimezone    string `json:"default_timezone"`     // 导入 Claude 账号的默认 IANA 时区
	SessionWindowLimit int64  `json:"session_window_limit"` // 默认并发会话窗口数(0=跟随全局)
	auth.ClaudeSecurityConfig
}

// GetClaudeConfig 返回当前 ClaudeCode 全局配置(取自运行时 Store 访问器)。
func (h *Handler) GetClaudeConfig(c *gin.Context) {
	security := h.store.ClaudeSecurityConfig()
	c.JSON(http.StatusOK, claudeGlobalConfigDTO{
		FingerprintMode:      h.store.ClaudeFingerprintModeDefault(),
		DefaultTimezone:      h.store.ClaudeDefaultTimezone(),
		SessionWindowLimit:   h.store.ClaudeSessionWindowLimit(),
		ClaudeSecurityConfig: security,
	})
}

// UpdateClaudeConfig 校验并持久化 ClaudeCode 全局配置,同时热更新运行时 Store。
func (h *Handler) UpdateClaudeConfig(c *gin.Context) {
	var req claudeGlobalConfigDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	mode := auth.NormalizeClaudeFingerprintMode(req.FingerprintMode)
	if !auth.IsValidClaudeFingerprintMode(req.FingerprintMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fingerprint_mode must be one of: preserve, force"})
		return
	}
	tz := strings.TrimSpace(req.DefaultTimezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "default_timezone must be a valid IANA timezone, e.g. Asia/Shanghai"})
			return
		}
	}
	window := req.SessionWindowLimit
	if window < 0 {
		window = 0
	}
	if window > 1000 {
		window = 1000
	}
	security := auth.NormalizeClaudeSecurityConfig(req.ClaudeSecurityConfig)

	cfg := auth.ClaudeConfig{
		FingerprintMode:      mode,
		DefaultTimezone:      tz,
		SessionWindowLimit:   window,
		ClaudeSecurityConfig: security,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode config"})
		return
	}
	if err := h.db.UpdateClaudeConfig(c.Request.Context(), string(raw)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
		return
	}

	// 热更新运行时 Store,无需重启即生效。
	h.store.SetClaudeFingerprintModeDefault(mode)
	h.store.SetClaudeDefaultTimezone(tz)
	h.store.SetClaudeSessionWindowLimit(window)
	h.store.SetClaudeSecurityConfig(security)

	c.JSON(http.StatusOK, gin.H{
		"message":                 "已保存 ClaudeCode 全局配置",
		"fingerprint_mode":        mode,
		"default_timezone":        tz,
		"session_window_limit":    window,
		"allow_service_tier":      security.AllowServiceTier,
		"allow_inference_geo":     security.AllowInferenceGeo,
		"allow_speed":             security.AllowSpeed,
		"allow_safety_identifier": security.AllowSafetyIdentifier,
		"allowed_beta_headers":    security.AllowedBetaHeaders,
		"max_output_tokens":       security.MaxOutputTokens,
		"max_tool_count":          security.MaxToolCount,
		"max_tool_schema_bytes":   security.MaxToolSchemaBytes,
	})
}
