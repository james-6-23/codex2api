package admin

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// bootstrapState 跟踪初始化端点的运行状态，主要用于：
//  1. 防止并发条件下重复写入；
//  2. 简单的全局限频，避免被扫描器穷举攻击。
var bootstrapState struct {
	mu sync.Mutex

	// rateBucket: 简单的固定窗口限频，单位 = 每 windowSec 内最多 maxPerWindow 次
	windowStart atomic.Int64 // unix seconds
	count       atomic.Int64
}

const (
	bootstrapWindowSec = 60
	bootstrapMaxPerWin = 20
	bootstrapMinSecret = 8
	bootstrapMaxSecret = 256
)

// bootstrapAllowRate 使用 CAS 实现固定窗口限频：
//   - 任意时刻只有一个 goroutine 能成功翻新窗口起点，其它失败者读到的就是
//     翻新后的最新值，避免多个 goroutine 同时把 count 重置为 0。
//   - 先递增计数再判断，确保高并发下不会超过限制。
func bootstrapAllowRate() bool {
	now := time.Now().Unix()
	for {
		winStart := bootstrapState.windowStart.Load()
		if now-winStart < bootstrapWindowSec {
			break
		}
		// 仅当 windowStart 仍是我们读到的旧值时才推进；其它 goroutine 已经
		// 推进过的话直接退出循环，复用最新窗口。
		if bootstrapState.windowStart.CompareAndSwap(winStart, now) {
			bootstrapState.count.Store(0)
			break
		}
	}
	newCount := bootstrapState.count.Add(1)
	return newCount <= bootstrapMaxPerWin
}

// bootstrapAllowClientIP 判断该来源 IP 是否允许执行页面初始化。
//
// 规则：
//   - loopback 永远允许；
//   - BOOTSTRAP_ALLOWED_CIDR 已设置时以其为准（逗号分隔 CIDR；设为 none 表示仅 loopback）；
//   - 未设置时默认放行私有/链路本地地址——Docker 端口映射后宿主机访问的源 IP 是
//     网桥地址(如 172.17.0.1)而非 loopback，若只认 loopback 会把最常见的本地
//     Docker 部署挡在初始化页面外(issue #199)。公网 IP 默认仍拒绝。
func bootstrapAllowClientIP(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	cidrEnv := strings.TrimSpace(os.Getenv("BOOTSTRAP_ALLOWED_CIDR"))
	if cidrEnv == "" {
		return ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if strings.EqualFold(cidrEnv, "none") {
		return false
	}
	for _, cidr := range strings.Split(cidrEnv, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func firstForwardedHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func bootstrapPublicBaseURL(r *http.Request) string {
	if r == nil {
		return ""
	}
	proto := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return strings.TrimRight(proto+"://"+host, "/")
}

func bootstrapDatabaseLocation(driver string) string {
	if strings.EqualFold(driver, "sqlite") {
		return strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	}
	host := strings.TrimSpace(os.Getenv("DATABASE_HOST"))
	name := strings.TrimSpace(os.Getenv("DATABASE_NAME"))
	port := strings.TrimSpace(os.Getenv("DATABASE_PORT"))
	if host == "" || name == "" {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + name
}

func (h *Handler) bootstrapSetupHints(r *http.Request) gin.H {
	serviceURL := bootstrapPublicBaseURL(r)
	adminURL := ""
	apiBaseURL := ""
	if serviceURL != "" {
		adminURL = strings.TrimRight(serviceURL, "/") + "/admin/"
		apiBaseURL = strings.TrimRight(serviceURL, "/") + "/v1"
	}
	databaseDriver := h.databaseDriver
	databaseLabel := h.databaseLabel
	if databaseDriver == "" && h.db != nil {
		databaseDriver = h.db.Driver()
	}
	if databaseLabel == "" && h.db != nil {
		databaseLabel = h.db.Label()
	}
	cacheDriver := h.cacheDriver
	cacheLabel := h.cacheLabel
	if cacheDriver == "" && h.cache != nil {
		cacheDriver = h.cache.Driver()
	}
	if cacheLabel == "" && h.cache != nil {
		cacheLabel = h.cache.Label()
	}

	usageLogMode := database.UsageLogModeFull
	usageLogBatchSize := 200
	usageLogFlushIntervalSeconds := 5
	if h.db != nil {
		usageLogMode = h.db.GetUsageLogMode()
		usageLogBatchSize = h.db.GetUsageLogBatchSize()
		usageLogFlushIntervalSeconds = h.db.GetUsageLogFlushIntervalSeconds()
	}

	imageBackend := imagestore.CurrentConfig().Backend
	if imageBackend == "" {
		imageBackend = imagestore.BackendLocal
	}

	return gin.H{
		"service_url":  serviceURL,
		"admin_url":    adminURL,
		"api_base_url": apiBaseURL,
		"database": gin.H{
			"driver":   databaseDriver,
			"label":    databaseLabel,
			"location": bootstrapDatabaseLocation(databaseDriver),
		},
		"cache": gin.H{
			"driver": cacheDriver,
			"label":  cacheLabel,
		},
		"data": gin.H{
			"image_local_dir":       imageAssetDir(),
			"image_storage_backend": imageBackend,
		},
		"usage": gin.H{
			"log_mode":               usageLogMode,
			"batch_size":             usageLogBatchSize,
			"flush_interval_seconds": usageLogFlushIntervalSeconds,
		},
	}
}

// GetBootstrapStatus 返回当前是否需要执行初始化（GET /api/admin/bootstrap-status）。
//
// 该端点不要求鉴权，前端 AuthGate 在拿到登录界面前会先轮询此端点：
//   - 已通过 .env 设置 ADMIN_SECRET => needs_bootstrap=false, source="env"
//   - 已写入数据库                  => needs_bootstrap=false, source="database"
//   - 两端均空                       => needs_bootstrap=true,  source="empty"
func (h *Handler) GetBootstrapStatus(c *gin.Context) {
	envSecret := strings.TrimSpace(h.adminSecretEnv)
	if envSecret != "" {
		c.JSON(http.StatusOK, gin.H{
			"needs_bootstrap": false,
			"source":          "env",
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		// 数据库异常时倾向 fail-closed：不允许 bootstrap，让运维先排查 DB
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"needs_bootstrap": false,
			"source":          "error",
			"error":           "读取系统设置失败，请检查数据库连接",
		})
		return
	}
	if settings != nil && strings.TrimSpace(settings.AdminSecret) != "" {
		c.JSON(http.StatusOK, gin.H{
			"needs_bootstrap": false,
			"source":          "database",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"needs_bootstrap": true,
		"source":          "empty",
		"setup":           h.bootstrapSetupHints(c.Request),
	})
}

// GetSetupHints 返回登录后的部署检查信息。
//
// 与公开的 bootstrap-status 不同，该接口注册在 /api/admin 下，需要管理密钥，
// 因此可以安全返回数据库位置、图片目录等部署诊断信息。
func (h *Handler) GetSetupHints(c *gin.Context) {
	c.JSON(http.StatusOK, h.bootstrapSetupHints(c.Request))
}

// PostBootstrap 接收用户在浏览器中输入的初始管理密钥并写入数据库。
//
// 安全约束：
//  1. 仅在系统未配置 ADMIN_SECRET 时可用，否则一律 409；
//  2. 通过互斥锁 + 双重检查避免并发写入；
//  3. 简单全局限频，防止扫描器穷举；
//  4. 校验最小长度（8 个 rune），避免过弱密钥；
//  5. 全程审计日志。
func (h *Handler) PostBootstrap(c *gin.Context) {
	if !bootstrapAllowClientIP(c.ClientIP()) {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_IP", "ip="+c.ClientIP())
		c.JSON(http.StatusForbidden, gin.H{"error": "当前来源 IP (" + c.ClientIP() + ") 不允许执行页面初始化。可任选其一：1) 在 .env 中设置 ADMIN_SECRET 后重建容器，跳过页面初始化直接登录；2) 设置 BOOTSTRAP_ALLOWED_CIDR 环境变量放行你的来源网段（如 BOOTSTRAP_ALLOWED_CIDR=203.0.113.0/24）。"})
		return
	}

	if !bootstrapAllowRate() {
		security.SecurityAuditLog("BOOTSTRAP_RATE_LIMITED", "ip="+c.ClientIP())
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后再试"})
		return
	}

	envSecret := strings.TrimSpace(h.adminSecretEnv)
	if envSecret != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ENV", "ip="+c.ClientIP())
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET 已通过环境变量配置，无需在页面初始化",
		})
		return
	}

	var body struct {
		AdminSecret string `json:"admin_secret"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}
	secret := strings.TrimSpace(body.AdminSecret)
	if secret == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "管理密钥不能为空"})
		return
	}
	if utf8.RuneCountInString(secret) < bootstrapMinSecret {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "管理密钥至少 8 位",
		})
		return
	}
	if len(secret) > bootstrapMaxSecret {
		c.JSON(http.StatusBadRequest, gin.H{"error": "管理密钥过长"})
		return
	}

	bootstrapState.mu.Lock()
	defer bootstrapState.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// 双重检查：进入临界区后再读一次，避免并发写入
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_READ_ERROR", "ip="+c.ClientIP()+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取系统设置失败"})
		return
	}
	if settings != nil && strings.TrimSpace(settings.AdminSecret) != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ALREADY_INITIALIZED", "ip="+c.ClientIP())
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET 已配置，无法重复初始化。如需重置，请进入「设置」页面使用现有密钥登录后修改。",
		})
		return
	}
	if settings == nil {
		settings = defaultBootstrapSettings()
	}
	settings.AdminSecret = secret

	if err := h.db.UpdateSystemSettings(ctx, settings); err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_WRITE_ERROR", "ip="+c.ClientIP()+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写入系统设置失败"})
		return
	}

	security.SecurityAuditLog("BOOTSTRAP_SUCCESS", "ip="+c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// defaultBootstrapSettings 返回 settings 表初次记录的安全默认值。
// 与 main.go 中 step 3 保持一致，避免 PostBootstrap 在数据库尚无任何记录时
// 写入空值导致后续业务设置缺失。
func defaultBootstrapSettings() *database.SystemSettings {
	return &database.SystemSettings{
		SiteName:                         database.DefaultSiteName,
		MaxConcurrency:                   2,
		GlobalRPM:                        0,
		TestModel:                        "gpt-5.4",
		TestContent:                      auth.DefaultTestContent,
		TestConcurrency:                  50,
		BackgroundRefreshIntervalMinutes: 2,
		UsageProbeMaxAgeMinutes:          10,
		RecoveryProbeIntervalMinutes:     30,
		LazyMode:                         false,
		PgMaxConns:                       50,
		RedisPoolSize:                    30,
		PromptFilterMode:                 "monitor",
		PromptFilterThreshold:            50,
		PromptFilterStrictThreshold:      90,
		PromptFilterLogMatches:           true,
		PromptFilterMaxTextLength:        81920,
		PromptFilterCustomPatterns:       "[]",
		PromptFilterDisabledPatterns:     "[]",
		ClientCompatMode:                 proxy.ClientCompatModePreserve,
		CodexMinCLIVersion:               "0.118.0",
		UsageLogMode:                     database.UsageLogModeFull,
		UsageLogBatchSize:                200,
		UsageLogFlushIntervalSeconds:     5,
		StreamFlushPolicy:                proxy.StreamFlushPolicyImmediate,
		StreamFlushIntervalMS:            20,
		FirstTokenMode:                   proxy.FirstTokenModeStrict,
		FirstTokenTimeoutSeconds:         0,
		BillingTierPolicy:                proxy.BillingTierPolicyActual,
		AffinityMode:                     "bounded",
		PublicKeyUsagePageEnabled:        true,
		CodexWSHideUpstreamErrors:        true,
		CodexWSSilentRetryEnabled:        true,
		CodexWSSilentMaxRetries:          2,
		AutoPause5hGuardBandPercent:      5,
		AutoPause5hGuardConcurrency:      1,
		SmartPacingMinConcurrency:        1,
		SmartPacingWindows:               "5h,7d",
		AutoResetCreditsBeforeExpiryMin:  60,
	}
}
