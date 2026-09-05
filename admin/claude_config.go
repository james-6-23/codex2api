package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// claudeGlobalConfigDTO 是 ClaudeCode 全局配置的读写载体(系统设置里的独立模块)。
// 全体 Claude 账号默认遵守;个体账号可在「编辑账号」里覆盖。
type claudeGlobalConfigDTO struct {
	FingerprintMode    string `json:"fingerprint_mode"`     // preserve / force(空=preserve)
	DefaultTimezone    string `json:"default_timezone"`     // 导入 Claude 账号的默认 IANA 时区
	SessionWindowLimit int64  `json:"session_window_limit"` // 默认并发会话窗口数(0=跟随全局)
	auth.ClaudeClientPolicy
	auth.ClaudeSecurityConfig
	CLIVersionSyncEnabled       *bool `json:"cli_version_sync_enabled"`
	CLIVersionSyncIntervalHours int   `json:"cli_version_sync_interval_hours"`
	// FirstTokenTimeoutSeconds：Claude 路径首字超时秒数（缺失=默认 120，0=跟随全局）。
	FirstTokenTimeoutSeconds *int `json:"first_token_timeout_seconds"`
	// StreamKeepaliveEnabled：Claude 流式首字前是否发 SSE 保活注释（缺失=开启）。
	StreamKeepaliveEnabled *bool `json:"stream_keepalive_enabled"`
	// 以下三项只读；PUT 忽略。
	SyncedCLIVersion    string `json:"synced_cli_version"`
	BuiltinCLIVersion   string `json:"builtin_cli_version"`
	EffectiveCLIVersion string `json:"effective_cli_version"`
}

// GetClaudeConfig 返回当前 ClaudeCode 全局配置(取自运行时 Store 访问器)。
func (h *Handler) GetClaudeConfig(c *gin.Context) {
	security := h.store.ClaudeSecurityConfig()
	c.JSON(http.StatusOK, claudeGlobalConfigDTO{
		FingerprintMode:             h.store.ClaudeFingerprintModeDefault(),
		DefaultTimezone:             h.store.ClaudeDefaultTimezone(),
		SessionWindowLimit:          h.store.ClaudeSessionWindowLimit(),
		ClaudeClientPolicy:          h.store.ClaudeClientPolicy(),
		ClaudeSecurityConfig:        security,
		CLIVersionSyncEnabled:       boolPtr(h.store.ClaudeCLIVersionSyncEnabled()),
		CLIVersionSyncIntervalHours: h.store.ClaudeCLIVersionSyncIntervalHours(),
		FirstTokenTimeoutSeconds:    claudeIntPtr(h.store.ClaudeFirstTokenTimeoutSeconds()),
		StreamKeepaliveEnabled:      boolPtr(h.store.ClaudeStreamKeepaliveEnabled()),
		SyncedCLIVersion:            auth.ClaudeSyncedCLIVersion(),
		BuiltinCLIVersion:           auth.BuiltinClaudeCLIVersion,
		EffectiveCLIVersion:         auth.EffectiveClaudeCLIVersion(),
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
	clientPolicy, err := auth.NormalizeClaudeClientPolicy(req.ClaudeClientPolicy)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	security := auth.NormalizeClaudeSecurityConfig(req.ClaudeSecurityConfig)
	syncEnabled := req.CLIVersionSyncEnabled == nil || *req.CLIVersionSyncEnabled
	syncInterval := auth.NormalizeClaudeCLIVersionSyncIntervalHours(req.CLIVersionSyncIntervalHours)
	firstTokenTimeout := auth.NormalizeClaudeFirstTokenTimeoutSeconds(req.FirstTokenTimeoutSeconds)
	streamKeepalive := req.StreamKeepaliveEnabled == nil || *req.StreamKeepaliveEnabled

	cfg := auth.ClaudeConfig{
		FingerprintMode:             mode,
		DefaultTimezone:             tz,
		SessionWindowLimit:          window,
		ClaudeClientPolicy:          clientPolicy,
		ClaudeSecurityConfig:        security,
		CLIVersionSyncEnabled:       boolPtr(syncEnabled),
		CLIVersionSyncIntervalHours: syncInterval,
		FirstTokenTimeoutSeconds:    claudeIntPtr(firstTokenTimeout),
		StreamKeepaliveEnabled:      boolPtr(streamKeepalive),
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
	h.store.SetClaudeClientPolicy(clientPolicy)
	h.store.SetClaudeSecurityConfig(security)
	h.store.SetClaudeCLIVersionSync(syncEnabled, syncInterval)
	h.store.SetClaudeFirstTokenTimeoutSeconds(firstTokenTimeout)
	h.store.SetClaudeStreamKeepaliveEnabled(streamKeepalive)

	c.JSON(http.StatusOK, gin.H{
		"message":                         "已保存 ClaudeCode 全局配置",
		"fingerprint_mode":                mode,
		"default_timezone":                tz,
		"session_window_limit":            window,
		"client_platform":                 clientPolicy.Platform,
		"version_policy":                  clientPolicy.VersionPolicy,
		"client_version":                  clientPolicy.ClientVersion,
		"allow_service_tier":              security.AllowServiceTier,
		"allow_inference_geo":             security.AllowInferenceGeo,
		"allow_speed":                     security.AllowSpeed,
		"allow_safety_identifier":         security.AllowSafetyIdentifier,
		"allowed_beta_headers":            security.AllowedBetaHeaders,
		"max_output_tokens":               security.MaxOutputTokens,
		"max_tool_count":                  security.MaxToolCount,
		"max_tool_schema_bytes":           security.MaxToolSchemaBytes,
		"cli_version_sync_enabled":        syncEnabled,
		"cli_version_sync_interval_hours": syncInterval,
		"first_token_timeout_seconds":     firstTokenTimeout,
		"stream_keepalive_enabled":        streamKeepalive,
	})
}

// boolPtr 返回指向给定 bool 值的指针，便于构造「显式布尔字段」的 JSON DTO。
func boolPtr(v bool) *bool { return &v }

// claudeIntPtr 返回指向给定 int 值的指针，用于「缺失与显式 0 有别」的 JSON DTO 字段。
func claudeIntPtr(v int) *int { return &v }

// claudeCLIVersionSyncResponse 在同步结果之上附加一个可选的 warning 字段：
// 抓取+持久化成功、但指纹回写部分失败时，仍以 200 响应并携带 warning，
// 而不是把整次同步判为失败。
type claudeCLIVersionSyncResponse struct {
	*proxy.ClaudeCLIVersionSyncResult
	Warning string `json:"warning,omitempty"`
}

// SyncClaudeCLIVersion 供设置页「立即同步」调用：拉取最新 Claude Code CLI 版本并回写账号指纹。
// proxy.SyncClaudeCLIVersion 的 err 可能只是抓取成功、持久化成功之后的指纹回写部分失败，
// 因此只在抓取阶段就失败（没有 FetchedVersion）时才判 502；否则仍按 200 返回结果，
// 并把该 err 作为 warning 字段透出，方便前端提示但不阻断已生效的版本同步。
func (h *Handler) SyncClaudeCLIVersion(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	result, err := proxy.SyncClaudeCLIVersion(ctx, h.db, h.store, proxyURL)
	if err != nil && (result == nil || result.FetchedVersion == "") {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	resp := claudeCLIVersionSyncResponse{ClaudeCLIVersionSyncResult: result}
	if err != nil {
		resp.Warning = err.Error()
	}
	c.JSON(http.StatusOK, resp)
}
