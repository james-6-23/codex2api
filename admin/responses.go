package admin

import (
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type errorResponse struct {
	Error string `json:"error"`
}

type messageResponse struct {
	Message string `json:"message"`
}

type statsResponse struct {
	Total         int   `json:"total"`
	Available     int   `json:"available"`
	RateLimited   int   `json:"rate_limited"`
	Error         int   `json:"error"`
	TodayRequests int64 `json:"today_requests"`
}

type accountsResponse struct {
	Accounts []accountResponse `json:"accounts"`
}

type createAccountResponse struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

type healthResponse struct {
	Status    string `json:"status"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

type usageLogsResponse struct {
	Logs []*database.UsageLog `json:"logs"`
}

type apiKeysResponse struct {
	Keys []*MaskedAPIKeyRow `json:"keys"`
}

// MaskedAPIKeyRow API Key 响应（含脱敏和完整 key）
type MaskedAPIKeyRow struct {
	ID              int64                    `json:"id"`
	Name            string                   `json:"name"`
	Key             string                   `json:"key"`
	RawKey          string                   `json:"raw_key"`
	QuotaLimit      float64                  `json:"quota_limit"`
	QuotaUsed       float64                  `json:"quota_used"`
	TotalUsed       float64                  `json:"total_used"`
	ResetCount      int                      `json:"reset_count"`
	LastResetAt     *string                  `json:"last_reset_at"`
	ExpiresAt       *string                  `json:"expires_at"`
	AllowedGroupIDs []int64                  `json:"allowed_group_ids"`
	Limits          database.APIKeyLimits    `json:"limits"`
	WindowUsage     *APIKeyWindowUsageDetail `json:"window_usage,omitempty"`
	Status          string                   `json:"status"`
	LastUsedAt      *string                  `json:"last_used_at,omitempty"`
	CreatedAt       string                   `json:"created_at"`
}

// APIKeyWindowUsageDetail 5h/7d/30d 滑动窗口内的累计成本
type APIKeyWindowUsageDetail struct {
	Cost5h  float64 `json:"cost_5h"`
	Cost7d  float64 `json:"cost_7d"`
	Cost30d float64 `json:"cost_30d"`
}

// NewMaskedAPIKeyRow 创建 API Key 响应
func NewMaskedAPIKeyRow(row *database.APIKeyRow) *MaskedAPIKeyRow {
	var expiresAt *string
	if row.ExpiresAt.Valid {
		formatted := row.ExpiresAt.Time.Format(time.RFC3339)
		expiresAt = &formatted
	}
	var lastResetAt *string
	if row.LastResetAt.Valid {
		formatted := row.LastResetAt.Time.Format(time.RFC3339)
		lastResetAt = &formatted
	}
	status := "active"
	if row.IsExpired(time.Now()) {
		status = "expired"
	} else if row.IsQuotaExhausted() {
		status = "quota_exhausted"
	}
	return &MaskedAPIKeyRow{
		ID:              row.ID,
		Name:            row.Name,
		Key:             security.MaskAPIKey(row.Key),
		RawKey:          row.Key,
		QuotaLimit:      row.QuotaLimit,
		QuotaUsed:       row.QuotaUsed,
		TotalUsed:       row.TotalUsed,
		ResetCount:      row.ResetCount,
		LastResetAt:     lastResetAt,
		ExpiresAt:       expiresAt,
		AllowedGroupIDs: append([]int64(nil), row.AllowedGroupIDs...),
		Limits:          row.Limits,
		Status:          status,
		CreatedAt:       row.CreatedAt.Format(time.RFC3339),
	}
}

type createAPIKeyResponse struct {
	ID              int64   `json:"id"`
	Key             string  `json:"key"`
	Name            string  `json:"name"`
	QuotaLimit      float64 `json:"quota_limit"`
	QuotaUsed       float64 `json:"quota_used"`
	ExpiresAt       *string `json:"expires_at"`
	AllowedGroupIDs []int64 `json:"allowed_group_ids"`
}

type opsOverviewResponse struct {
	UpdatedAt      string              `json:"updated_at"`
	UptimeSeconds  int64               `json:"uptime_seconds"`
	DatabaseDriver string              `json:"database_driver"`
	DatabaseLabel  string              `json:"database_label"`
	CacheDriver    string              `json:"cache_driver"`
	CacheLabel     string              `json:"cache_label"`
	CPU            opsCPUResponse      `json:"cpu"`
	Memory         opsMemoryResponse   `json:"memory"`
	Runtime        opsRuntimeResponse  `json:"runtime"`
	Requests       opsRequestsResponse `json:"requests"`
	Postgres       opsDatabaseResponse `json:"postgres"`
	Redis          opsRedisResponse    `json:"redis"`
	Traffic        opsTrafficResponse  `json:"traffic"`
}

type opsCPUResponse struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type opsMemoryResponse struct {
	Percent      float64 `json:"percent"`
	UsedBytes    uint64  `json:"used_bytes"`
	TotalBytes   uint64  `json:"total_bytes"`
	ProcessBytes uint64  `json:"process_bytes"`
}

type opsRuntimeResponse struct {
	Goroutines        int `json:"goroutines"`
	AvailableAccounts int `json:"available_accounts"`
	TotalAccounts     int `json:"total_accounts"`
}

type opsRequestsResponse struct {
	Active int64 `json:"active"`
	Total  int64 `json:"total"`
}

type opsDatabaseResponse struct {
	Healthy      bool    `json:"healthy"`
	Open         int     `json:"open"`
	InUse        int     `json:"in_use"`
	Idle         int     `json:"idle"`
	MaxOpen      int     `json:"max_open"`
	WaitCount    int64   `json:"wait_count"`
	UsagePercent float64 `json:"usage_percent"`
}

type opsRedisResponse struct {
	Healthy      bool    `json:"healthy"`
	TotalConns   uint32  `json:"total_conns"`
	IdleConns    uint32  `json:"idle_conns"`
	StaleConns   uint32  `json:"stale_conns"`
	PoolSize     int     `json:"pool_size"`
	UsagePercent float64 `json:"usage_percent"`
}

type opsTrafficResponse struct {
	QPS           float64 `json:"qps"`
	QPSPeak       float64 `json:"qps_peak"`
	TPS           float64 `json:"tps"`
	TPSPeak       float64 `json:"tps_peak"`
	RPM           float64 `json:"rpm"`
	TPM           float64 `json:"tpm"`
	ErrorRate     float64 `json:"error_rate"`
	TodayRequests int64   `json:"today_requests"`
	TodayTokens   int64   `json:"today_tokens"`
	RPMLimit      int     `json:"rpm_limit"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

type runtimeStatusResponse struct {
	UpdatedAt    string                       `json:"updated_at"`
	Status       string                       `json:"status"`
	Service      runtimeServiceStatusResponse `json:"service"`
	Database     runtimeDatabaseResponse      `json:"database"`
	Cache        runtimeCacheResponse         `json:"cache"`
	UsageLog     runtimeUsageLogResponse      `json:"usage_log"`
	Probes       runtimeProbesResponse        `json:"probes"`
	Accounts     runtimeAccountsResponse      `json:"accounts"`
	ImageStorage runtimeImageStorageResponse  `json:"image_storage"`
	AdminAuth    runtimeAdminAuthResponse     `json:"admin_auth"`
	Checks       []runtimeCheckResponse       `json:"checks"`
}

type runtimeCheckResponse struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type runtimeServiceStatusResponse struct {
	Status        string `json:"status"`
	ServiceURL    string `json:"service_url"`
	AdminURL      string `json:"admin_url"`
	APIBaseURL    string `json:"api_base_url"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Goroutines    int    `json:"goroutines"`
	GoVersion     string `json:"go_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	PID           int    `json:"pid"`
}

type runtimeDatabaseResponse struct {
	Status       string  `json:"status"`
	Driver       string  `json:"driver"`
	Label        string  `json:"label"`
	Location     string  `json:"location"`
	Healthy      bool    `json:"healthy"`
	Error        string  `json:"error,omitempty"`
	Open         int     `json:"open"`
	InUse        int     `json:"in_use"`
	Idle         int     `json:"idle"`
	MaxOpen      int     `json:"max_open"`
	WaitCount    int64   `json:"wait_count"`
	UsagePercent float64 `json:"usage_percent"`
}

type runtimeCacheResponse struct {
	Status       string  `json:"status"`
	Driver       string  `json:"driver"`
	Label        string  `json:"label"`
	Healthy      bool    `json:"healthy"`
	Error        string  `json:"error,omitempty"`
	TotalConns   uint32  `json:"total_conns"`
	IdleConns    uint32  `json:"idle_conns"`
	StaleConns   uint32  `json:"stale_conns"`
	PoolSize     int     `json:"pool_size"`
	UsagePercent float64 `json:"usage_percent"`
}

type runtimeUsageLogResponse struct {
	Status               string `json:"status"`
	Mode                 string `json:"mode"`
	Enabled              bool   `json:"enabled"`
	BatchSize            int    `json:"batch_size"`
	FlushIntervalSeconds int    `json:"flush_interval_seconds"`
	BufferLength         int    `json:"buffer_length"`
	BufferCapacity       int    `json:"buffer_capacity"`
}

type runtimeProbesResponse struct {
	Status                             string `json:"status"`
	LazyMode                           bool   `json:"lazy_mode"`
	BackgroundRefreshIntervalMinutes   int    `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes            int    `json:"usage_probe_max_age_minutes"`
	UsageProbeConcurrency              int    `json:"usage_probe_concurrency"`
	UsageProbeResponsesFallbackEnabled bool   `json:"usage_probe_responses_fallback_enabled"`
	RecoveryProbeIntervalMinutes       int    `json:"recovery_probe_interval_minutes"`
	UsageProbeRunning                  bool   `json:"usage_probe_running"`
	RecoveryProbeRunning               bool   `json:"recovery_probe_running"`
	AutoCleanupRunning                 bool   `json:"auto_cleanup_running"`
}

type runtimeAccountsResponse struct {
	Status         string         `json:"status"`
	Total          int            `json:"total"`
	Available      int            `json:"available"`
	ActiveRequests int64          `json:"active_requests"`
	TotalRequests  int64          `json:"total_requests"`
	StatusCounts   map[string]int `json:"status_counts"`
}

type runtimeImageStorageResponse struct {
	Status   string `json:"status"`
	Backend  string `json:"backend"`
	LocalDir string `json:"local_dir,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Healthy  bool   `json:"healthy"`
	Error    string `json:"error,omitempty"`
}

type runtimeAdminAuthResponse struct {
	Status     string `json:"status"`
	Source     string `json:"source"`
	Configured bool   `json:"configured"`
}

func writeError(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, errorResponse{Error: message})
}

func writeMessage(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, messageResponse{Message: message})
}

func writeInternalError(c *gin.Context, err error) {
	writeError(c, http.StatusInternalServerError, err.Error())
}
