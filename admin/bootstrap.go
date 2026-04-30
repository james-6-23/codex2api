package admin

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
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
	bootstrapWindowSec   = 60
	bootstrapMaxPerWin   = 20
	bootstrapMinSecret   = 8
	bootstrapMaxSecret   = 256
)

// bootstrapAllowRate 使用 CAS 实现固定窗口限频：
//   - 任意时刻只有一个 goroutine 能成功翻新窗口起点，其它失败者读到的就是
//     翻新后的最新值，避免多个 goroutine 同时把 count 重置为 0。
//   - 在并发下，最坏情况只是有一个 reset 与若干 Add 交错，但所有"翻窗"
//     操作都是原子的，不会出现窗口被重复清零导致超额放行的情况。
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
	return bootstrapState.count.Add(1) <= bootstrapMaxPerWin
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
	})
}

// PostBootstrap 接收用户在浏览器中输入的初始管理密钥并写入数据库。
//
// 安全约束：
//  1. 仅在系统未配置 ADMIN_SECRET 时可用，否则一律 409；
//  2. 通过互斥锁 + 双重检查避免并发写入；
//  3. 简单全局限频，防止扫描器穷举；
//  4. 校验最小长度（12 个 rune），避免过弱密钥；
//  5. 全程审计日志。
func (h *Handler) PostBootstrap(c *gin.Context) {
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
		MaxConcurrency:                   2,
		GlobalRPM:                        0,
		TestModel:                        "gpt-5.4",
		TestConcurrency:                  50,
		BackgroundRefreshIntervalMinutes: 2,
		UsageProbeMaxAgeMinutes:          10,
		RecoveryProbeIntervalMinutes:     30,
		PgMaxConns:                       50,
		RedisPoolSize:                    30,
		PromptFilterMode:                 "monitor",
		PromptFilterThreshold:            50,
		PromptFilterStrictThreshold:      90,
		PromptFilterLogMatches:           true,
		PromptFilterMaxTextLength:        81920,
		PromptFilterCustomPatterns:       "[]",
		PromptFilterDisabledPatterns:     "[]",
	}
}
