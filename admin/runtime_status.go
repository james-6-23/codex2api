package admin

import (
	"context"
	"net/http"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	runtimeStatusOK       = "ok"
	runtimeStatusDegraded = "degraded"
	runtimeStatusError    = "error"
)

// GetRuntimeStatus 返回机器可读的运行时依赖状态。
func (h *Handler) GetRuntimeStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	c.JSON(200, h.buildRuntimeStatus(ctx, c.Request))
}

func (h *Handler) buildRuntimeStatus(ctx context.Context, r *http.Request) runtimeStatusResponse {
	checks := make([]runtimeCheckResponse, 0, 8)
	addCheck := func(component, status, code, message string) {
		checks = append(checks, runtimeCheckResponse{
			Component: component,
			Status:    status,
			Code:      code,
			Message:   message,
		})
	}

	service := h.runtimeServiceStatus(r)
	addCheck("service", runtimeStatusOK, "service_running", "服务进程正在运行")

	dbStatus := h.runtimeDatabaseStatus(ctx)
	addCheck("database", dbStatus.Status, statusCode(dbStatus.Status, "database"), statusMessage(dbStatus.Status, "数据库连接正常", dbStatus.Error))

	cacheStatus := h.runtimeCacheStatus(ctx)
	addCheck("cache", cacheStatus.Status, statusCode(cacheStatus.Status, "cache"), statusMessage(cacheStatus.Status, "缓存连接正常", cacheStatus.Error))

	usageLog := h.runtimeUsageLogStatus()
	if usageLog.Status != runtimeStatusOK {
		addCheck("usage_log", usageLog.Status, statusCode(usageLog.Status, "usage_log"), statusMessage(usageLog.Status, "请求日志写入状态正常", "请求日志写入器不可用"))
	} else if usageLog.Enabled {
		addCheck("usage_log", runtimeStatusOK, "usage_log_enabled", "请求日志写入已开启")
	} else {
		addCheck("usage_log", runtimeStatusOK, "usage_log_disabled", "请求日志写入已按配置关闭")
	}

	probes := h.runtimeProbesStatus()
	if probes.Status != runtimeStatusOK {
		addCheck("probes", probes.Status, statusCode(probes.Status, "probes"), statusMessage(probes.Status, "后台刷新和探针配置正常", "探针运行态不可用"))
	} else if probes.LazyMode {
		addCheck("probes", runtimeStatusOK, "lazy_mode_enabled", "惰性模式已开启，仅保留 wham 用量探针")
	} else {
		addCheck("probes", runtimeStatusOK, "probes_enabled", "后台刷新和探针配置正常")
	}

	accounts := h.runtimeAccountsStatus()
	switch accounts.Status {
	case runtimeStatusDegraded:
		addCheck("accounts", runtimeStatusDegraded, "account_pool_degraded", "账号池当前没有可调度账号")
	case runtimeStatusError:
		addCheck("accounts", runtimeStatusError, "account_store_unavailable", "账号池运行态不可用")
	default:
		addCheck("accounts", runtimeStatusOK, "account_pool_ready", "账号池存在可调度账号")
	}

	imageStorage := h.runtimeImageStorageStatus()
	addCheck("image_storage", imageStorage.Status, statusCode(imageStorage.Status, "image_storage"), statusMessage(imageStorage.Status, "图片存储配置正常", imageStorage.Error))

	adminAuth := h.runtimeAdminAuthStatus(ctx)
	if adminAuth.Configured {
		addCheck("admin_auth", runtimeStatusOK, "admin_auth_configured", "管理鉴权已配置")
	} else {
		addCheck("admin_auth", runtimeStatusError, "admin_auth_disabled", "管理密钥未配置")
	}

	return runtimeStatusResponse{
		UpdatedAt:    time.Now().Format(time.RFC3339),
		Status:       overallRuntimeStatus(checks),
		Service:      service,
		Database:     dbStatus,
		Cache:        cacheStatus,
		UsageLog:     usageLog,
		Probes:       probes,
		Accounts:     accounts,
		ImageStorage: imageStorage,
		AdminAuth:    adminAuth,
		Checks:       checks,
	}
}

func (h *Handler) runtimeServiceStatus(r *http.Request) runtimeServiceStatusResponse {
	serviceURL := bootstrapPublicBaseURL(r)
	adminURL := ""
	apiBaseURL := ""
	if serviceURL != "" {
		adminURL = strings.TrimRight(serviceURL, "/") + "/admin/"
		apiBaseURL = strings.TrimRight(serviceURL, "/") + "/v1"
	}
	var uptime int64
	if !h.startedAt.IsZero() {
		uptime = int64(time.Since(h.startedAt).Seconds())
	}
	return runtimeServiceStatusResponse{
		Status:        runtimeStatusOK,
		ServiceURL:    serviceURL,
		AdminURL:      adminURL,
		APIBaseURL:    apiBaseURL,
		UptimeSeconds: uptime,
		Goroutines:    goruntime.NumGoroutine(),
		GoVersion:     goruntime.Version(),
		OS:            goruntime.GOOS,
		Arch:          goruntime.GOARCH,
		PID:           os.Getpid(),
	}
}

func (h *Handler) runtimeDatabaseStatus(ctx context.Context) runtimeDatabaseResponse {
	driver := h.databaseDriver
	label := h.databaseLabel
	if h.db != nil {
		if driver == "" {
			driver = h.db.Driver()
		}
		if label == "" {
			label = h.db.Label()
		}
	}

	resp := runtimeDatabaseResponse{
		Status:   runtimeStatusError,
		Driver:   driver,
		Label:    label,
		Location: bootstrapDatabaseLocation(driver),
	}
	if h.db == nil {
		resp.Error = "database handle is nil"
		return resp
	}

	stats := h.db.Stats()
	resp.Open = stats.OpenConnections
	resp.InUse = stats.InUse
	resp.Idle = stats.Idle
	resp.MaxOpen = stats.MaxOpenConnections
	resp.WaitCount = stats.WaitCount
	if stats.MaxOpenConnections > 0 {
		resp.UsagePercent = float64(stats.OpenConnections) / float64(stats.MaxOpenConnections) * 100
	}

	if err := h.db.Ping(ctx); err != nil {
		resp.Error = security.SanitizeLog(err.Error())
		return resp
	}
	resp.Healthy = true
	resp.Status = runtimeStatusOK
	return resp
}

func (h *Handler) runtimeCacheStatus(ctx context.Context) runtimeCacheResponse {
	driver := h.cacheDriver
	label := h.cacheLabel
	if h.cache != nil {
		if driver == "" {
			driver = h.cache.Driver()
		}
		if label == "" {
			label = h.cache.Label()
		}
	}

	resp := runtimeCacheResponse{
		Status: runtimeStatusError,
		Driver: driver,
		Label:  label,
	}
	if h.cache == nil {
		resp.Error = "cache handle is nil"
		return resp
	}

	poolStats := h.cache.Stats()
	resp.TotalConns = poolStats.TotalConns
	resp.IdleConns = poolStats.IdleConns
	resp.StaleConns = poolStats.StaleConns
	resp.PoolSize = h.cache.PoolSize()
	resp.WaitCount = poolStats.WaitCount
	resp.WaitDurationNs = poolStats.WaitDurationNs
	resp.Timeouts = poolStats.Timeouts
	resp.PendingRequests = poolStats.PendingRequests
	active := poolStats.InUse()
	if resp.PoolSize > 0 {
		resp.UsagePercent = float64(active) / float64(resp.PoolSize) * 100
	}

	if err := h.cache.Ping(ctx); err != nil {
		resp.Error = security.SanitizeLog(err.Error())
		return resp
	}
	resp.Healthy = true
	resp.Status = runtimeStatusOK
	return resp
}

func (h *Handler) runtimeUsageLogStatus() runtimeUsageLogResponse {
	stats := database.UsageLogRuntimeStats{
		Mode:                 database.UsageLogModeFull,
		Enabled:              true,
		BatchSize:            200,
		FlushIntervalSeconds: 5,
	}
	if h.db != nil {
		stats = h.db.GetUsageLogRuntimeStats()
	} else {
		return runtimeUsageLogResponse{
			Status:               runtimeStatusError,
			Mode:                 stats.Mode,
			Enabled:              stats.Enabled,
			BatchSize:            stats.BatchSize,
			FlushIntervalSeconds: stats.FlushIntervalSeconds,
		}
	}
	return runtimeUsageLogResponse{
		Status:               runtimeStatusOK,
		Mode:                 stats.Mode,
		Enabled:              stats.Enabled,
		BatchSize:            stats.BatchSize,
		FlushIntervalSeconds: stats.FlushIntervalSeconds,
		BufferLength:         stats.BufferLength,
		BufferCapacity:       stats.BufferCapacity,
		BufferLimit:          stats.BufferLimit,
		DroppedTotal:         stats.DroppedTotal,
	}
}

func (h *Handler) runtimeProbesStatus() runtimeProbesResponse {
	resp := runtimeProbesResponse{Status: runtimeStatusError}
	if h.store == nil {
		return resp
	}
	resp.Status = runtimeStatusOK
	resp.LazyMode = h.store.GetLazyMode()
	resp.CodexOAuthKeepaliveEnabled = h.store.GetCodexOAuthKeepalive()
	resp.ClaudeOAuthKeepaliveEnabled = h.store.GetClaudeOAuthKeepalive()
	resp.BackgroundRefreshIntervalMinutes = h.store.GetBackgroundRefreshIntervalMinutes()
	resp.UsageProbeMaxAgeMinutes = h.store.GetUsageProbeMaxAgeMinutes()
	resp.UsageProbeConcurrency = h.store.GetUsageProbeConcurrency()
	resp.UsageProbeResponsesFallbackEnabled = h.store.UsageProbeResponsesFallbackEnabled()
	resp.RecoveryProbeIntervalMinutes = h.store.GetRecoveryProbeIntervalMinutes()
	resp.UsageProbeRunning = h.store.UsageProbeRunning()
	resp.RecoveryProbeRunning = h.store.RecoveryProbeRunning()
	resp.AutoCleanupRunning = h.store.AutoCleanupRunning()
	return resp
}

func (h *Handler) runtimeAccountsStatus() runtimeAccountsResponse {
	resp := runtimeAccountsResponse{
		Status:       runtimeStatusError,
		StatusCounts: map[string]int{},
	}
	if h.store == nil {
		return resp
	}

	resp.Total = h.store.AccountCount()
	resp.Available = h.store.AvailableCount()
	resp.Status = runtimeStatusOK
	if resp.Total == 0 || resp.Available == 0 {
		resp.Status = runtimeStatusDegraded
	}
	for _, acc := range h.store.Accounts() {
		if acc == nil {
			continue
		}
		status := acc.RuntimeStatus()
		resp.StatusCounts[status]++
		resp.ActiveRequests += acc.GetActiveRequests()
		resp.TotalRequests += acc.GetTotalRequests()
	}
	return resp
}

func (h *Handler) runtimeImageStorageStatus() runtimeImageStorageResponse {
	cfg := imagestore.CurrentConfig()
	backend := strings.TrimSpace(cfg.Backend)
	if backend == "" {
		backend = imagestore.BackendLocal
	}
	resp := runtimeImageStorageResponse{
		Status:  runtimeStatusOK,
		Backend: backend,
		Healthy: true,
		Prefix:  strings.TrimSuffix(cfg.Prefix, "/"),
	}

	switch backend {
	case imagestore.BackendLocal:
		localDir := strings.TrimSpace(imagestore.LocalDir())
		if localDir == "" {
			localDir = strings.TrimSpace(cfg.LocalDir)
		}
		if localDir == "" {
			localDir = imageAssetDir()
		}
		resp.LocalDir = localDir
		if _, err := os.Stat(localDir); err != nil {
			resp.Status = runtimeStatusDegraded
			resp.Healthy = false
			resp.Error = security.SanitizeLog(err.Error())
		}
	case imagestore.BackendS3:
		resp.Bucket = cfg.Bucket
		if strings.TrimSpace(cfg.Bucket) == "" {
			resp.Status = runtimeStatusDegraded
			resp.Healthy = false
			resp.Error = "S3 bucket is empty"
		}
	default:
		resp.Status = runtimeStatusError
		resp.Healthy = false
		resp.Error = "unknown image storage backend"
	}
	return resp
}

func (h *Handler) runtimeAdminAuthStatus(ctx context.Context) runtimeAdminAuthResponse {
	if strings.TrimSpace(h.adminSecretEnv) != "" {
		return runtimeAdminAuthResponse{
			Status:     runtimeStatusOK,
			Source:     "env",
			Configured: true,
		}
	}
	if h.db == nil {
		return runtimeAdminAuthResponse{
			Status:     runtimeStatusError,
			Source:     "disabled",
			Configured: false,
		}
	}
	secret, source := h.resolveAdminSecret(ctx)
	configured := strings.TrimSpace(secret) != ""
	status := runtimeStatusOK
	if !configured {
		status = runtimeStatusError
	}
	return runtimeAdminAuthResponse{
		Status:     status,
		Source:     source,
		Configured: configured,
	}
}

func overallRuntimeStatus(checks []runtimeCheckResponse) string {
	overall := runtimeStatusOK
	for _, check := range checks {
		switch check.Status {
		case runtimeStatusError:
			return runtimeStatusError
		case runtimeStatusDegraded:
			overall = runtimeStatusDegraded
		}
	}
	return overall
}

func statusCode(status, component string) string {
	switch status {
	case runtimeStatusOK:
		return component + "_ok"
	case runtimeStatusDegraded:
		return component + "_degraded"
	default:
		return component + "_error"
	}
}

func statusMessage(status, okMessage, errMessage string) string {
	if status == runtimeStatusOK {
		return okMessage
	}
	if strings.TrimSpace(errMessage) != "" {
		return errMessage
	}
	if status == runtimeStatusDegraded {
		return "当前组件处于降级状态"
	}
	return "当前组件异常"
}
