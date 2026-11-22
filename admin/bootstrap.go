package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
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
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// bootstrapState 跟踪初始化端点的运行状态，主要用于：
//  1. 防止并发条件下重复写入；
//  2. 简单的全局限频，只统计令牌错误的请求，避免被扫描器穷举。
var bootstrapState struct {
	mu sync.Mutex

	// rateBucket: 简单的固定窗口限频，单位 = 每 windowSec 内最多 maxPerWindow 次
	windowStart atomic.Int64 // unix seconds
	count       atomic.Int64
}

// bootstrapTokenState 持有页面初始化所需的一次性令牌。令牌只存在于进程内存，
// 生成时打印到服务日志：能完成初始化的必须是能读到服务端日志的部署者，而不是
// "来源 IP 看起来像内网"的任何人——Docker Desktop / rootless 容器的端口转发和
// 私网反代都会把公网请求的来源变成私网地址。
var bootstrapTokenState struct {
	mu    sync.Mutex
	token string
}

const (
	bootstrapWindowSec  = 60
	bootstrapMaxPerWin  = 20
	bootstrapMinSecret  = 8
	bootstrapMaxSecret  = 256
	bootstrapTokenBytes = 16
)

// EnsureBootstrapToken 返回当前的一次性初始化令牌，尚未生成时生成并打印到日志。
// 令牌在初始化成功后作废；之后若管理密钥被清空、再次进入待初始化状态，会重新生成。
func EnsureBootstrapToken() (string, error) {
	bootstrapTokenState.mu.Lock()
	defer bootstrapTokenState.mu.Unlock()
	if bootstrapTokenState.token != "" {
		return bootstrapTokenState.token, nil
	}
	buf := make([]byte, bootstrapTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	bootstrapTokenState.token = hex.EncodeToString(buf)
	log.Printf("[bootstrap] 初始化令牌 (bootstrap token): %s", bootstrapTokenState.token)
	log.Println("[bootstrap] 首次初始化页面需要填写该令牌；仅当前进程有效，重启后重新生成，初始化成功后立即作废。")
	return bootstrapTokenState.token, nil
}

// bootstrapTokenMatches 以常量时间比较提交的令牌。令牌尚未生成时一律不匹配。
func bootstrapTokenMatches(submitted string) bool {
	submitted = strings.ToLower(strings.TrimSpace(submitted))
	bootstrapTokenState.mu.Lock()
	defer bootstrapTokenState.mu.Unlock()
	if bootstrapTokenState.token == "" || submitted == "" {
		return false
	}
	return security.SecureCompare(submitted, bootstrapTokenState.token)
}

// revokeBootstrapToken 在初始化成功后作废令牌。
func revokeBootstrapToken() {
	bootstrapTokenState.mu.Lock()
	bootstrapTokenState.token = ""
	bootstrapTokenState.mu.Unlock()
}

// bootstrapAuditIP 生成审计日志里的来源字段：ip 是直连对端，与按可信代理解析出的
// 客户端 IP 不同时额外记 forwarded，便于事后区分真实对端与转发头声称的地址。
func bootstrapAuditIP(c *gin.Context) string {
	peer := c.RemoteIP()
	forwarded := c.ClientIP()
	if forwarded == "" || forwarded == peer {
		return "ip=" + peer
	}
	return "ip=" + peer + " forwarded=" + forwarded
}

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

// bootstrapAllowClientIP 判断该来源 IP 是否允许执行页面初始化。调用方必须传入
// 直连 TCP 对端地址(c.RemoteIP())而不是 c.ClientIP()：后者在对端属于可信代理时
// 取自 X-Forwarded-For，而私网网段默认可信，请求方可以随意声称自己是内网地址。
// 这道门只是纵深防御，真正的授权凭据是一次性初始化令牌。
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
	// 运行时清空管理密钥后重新进入待初始化状态时，这里负责补发令牌到日志。
	if _, err := EnsureBootstrapToken(); err != nil {
		log.Printf("[bootstrap] 生成初始化令牌失败: %v", err)
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
//  2. 必须携带启动日志里的一次性初始化令牌；来源 IP 门按直连对端判断，只作纵深防御；
//  3. 通过互斥锁 + 双重检查避免并发写入；
//  4. 令牌错误的请求计入全局限频，令牌正确的请求不受限频影响，扫描器打满限频也挡不住部署者；
//  5. 校验最小长度（8 个 rune），避免过弱密钥；
//  6. 全程审计日志。
func (h *Handler) PostBootstrap(c *gin.Context) {
	auditIP := bootstrapAuditIP(c)
	if peer := c.RemoteIP(); !bootstrapAllowClientIP(peer) {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_IP", auditIP)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "当前来源 IP (" + peer + ") 不允许执行页面初始化。可任选其一：1) 在 .env 中设置 ADMIN_SECRET 后重建容器，跳过页面初始化直接登录；2) 设置 BOOTSTRAP_ALLOWED_CIDR 环境变量放行该来源网段（如 BOOTSTRAP_ALLOWED_CIDR=203.0.113.0/24）。该判断只看与服务直连的 TCP 对端，不读取 X-Forwarded-For；经反向代理访问时请放行反代所在网段。",
			"code":  "bootstrap_ip_forbidden",
		})
		return
	}

	envSecret := strings.TrimSpace(h.adminSecretEnv)
	if envSecret != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ENV", auditIP)
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET 已通过环境变量配置，无需在页面初始化",
		})
		return
	}

	// 先确认确实处于待初始化状态再校验令牌：已初始化时令牌早已作废，直接回 409
	// 比回"令牌无效"更准确；这一信息 bootstrap-status 本来就公开。
	preCtx, preCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	current, err := h.db.GetSystemSettings(preCtx)
	preCancel()
	if err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_READ_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取系统设置失败"})
		return
	}
	if current != nil && strings.TrimSpace(current.AdminSecret) != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ALREADY_INITIALIZED", auditIP)
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET 已配置，无法重复初始化。如需重置，请进入「设置」页面使用现有密钥登录后修改。",
		})
		return
	}
	if _, err := EnsureBootstrapToken(); err != nil {
		security.SecurityAuditLog("BOOTSTRAP_TOKEN_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成初始化令牌失败"})
		return
	}

	var body struct {
		AdminSecret    string `json:"admin_secret"`
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}
	if !bootstrapTokenMatches(body.BootstrapToken) {
		if !bootstrapAllowRate() {
			security.SecurityAuditLog("BOOTSTRAP_RATE_LIMITED", auditIP)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后再试"})
			return
		}
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_TOKEN", auditIP)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "初始化令牌无效。请在服务日志中搜索 bootstrap token 获取令牌（例如 docker compose logs codex2api | grep \"bootstrap token\"）；重启服务会生成新令牌。",
			"code":  "bootstrap_token_invalid",
		})
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
		security.SecurityAuditLog("BOOTSTRAP_DB_READ_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取系统设置失败"})
		return
	}
	if settings != nil && strings.TrimSpace(settings.AdminSecret) != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ALREADY_INITIALIZED", auditIP)
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
		security.SecurityAuditLog("BOOTSTRAP_DB_WRITE_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写入系统设置失败"})
		return
	}

	revokeBootstrapToken()
	security.SecurityAuditLog("BOOTSTRAP_SUCCESS", auditIP)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// defaultBootstrapSettings 返回 settings 表初次记录的安全默认值。
// 与 main.go 中 step 3 保持一致，避免 PostBootstrap 在数据库尚无任何记录时
// 写入空值导致后续业务设置缺失。
func defaultBootstrapSettings() *database.SystemSettings {
	promptFilterCfg := promptfilter.RecommendedConfig()
	return &database.SystemSettings{
		SiteName:                          database.DefaultSiteName,
		MaxConcurrency:                    2,
		CodexTelemetryEnabled:             false, // 实验性:模拟遥测默认不外发,由部署者显式开启
		GlobalRPM:                         0,
		TestModel:                         auth.DefaultTestModel,
		TestContent:                       auth.DefaultTestContent,
		TestConcurrency:                   50,
		BackgroundRefreshIntervalMinutes:  2,
		UsageProbeMaxAgeMinutes:           10,
		RecoveryProbeIntervalMinutes:      30,
		LazyMode:                          false,
		PgMaxConns:                        50,
		RedisPoolSize:                     30,
		PromptFilterMode:                  promptFilterCfg.Mode,
		PromptFilterThreshold:             promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:       promptFilterCfg.StrictThreshold,
		PromptFilterStrictTerminalEnabled: promptFilterCfg.StrictTerminalEnabled,
		PromptFilterAdvancedConfig:        promptfilter.MarshalAdvancedConfig(promptFilterCfg.Advanced),
		PromptFilterLogMatches:            true,
		PromptFilterMaxTextLength:         81920,
		PromptFilterCustomPatterns:        "[]",
		PromptFilterDisabledPatterns:      "[]",
		ClientCompatMode:                  proxy.ClientCompatModePreserve,
		CodexMinCLIVersion:                "0.160.0",
		UsageLogMode:                      database.UsageLogModeFull,
		UsageLogBatchSize:                 200,
		UsageLogFlushIntervalSeconds:      5,
		StreamFlushPolicy:                 proxy.StreamFlushPolicyImmediate,
		StreamFlushIntervalMS:             20,
		FirstTokenMode:                    proxy.FirstTokenModeLoose,
		FirstTokenTimeoutSeconds:          0,
		BillingTierPolicy:                 proxy.BillingTierPolicyActual,
		AffinityMode:                      "bounded",
		GrokConfig:                        `{"affinity_mode":"strict","probe_enabled":false,"probe_interval_minutes":30,"max_rate_limit_retries":0,"oauth_client_id":"","follow_up_effort_enabled":false,"follow_up_tool_effort":"medium","follow_up_small_effort":"low","quality_guard_enabled":false,"quality_guard_max_attempts":6,"quality_guard_hold_timeout_sec":30,"quality_guard_on_exhausted":"fail_closed","quality_guard_account_cooldown_hours":12}`,
		PublicKeyUsagePageEnabled:         true,
		PublicImageStudioPageEnabled:      true,
		CodexWSHideUpstreamErrors:         true,
		CodexWSSilentRetryEnabled:         true,
		CodexWSSilentMaxRetries:           2,
		CodexWSStatelessSlots:             proxy.DefaultRuntimeSettings().CodexWSStatelessSlots,
		CodexWSDownstreamKeepaliveSlots:   database.DefaultCodexWSDownstreamKeepaliveSlots,
		AutoPause5hGuardBandPercent:       5,
		AutoPause5hGuardConcurrency:       1,
		SmartPacingMinConcurrency:         1,
		SmartPacingWindows:                "5h,7d",
		AutoResetCreditsBeforeExpiryMin:   60,
		UTLSShutdownTimeoutMinutes:        30,
	}
}
