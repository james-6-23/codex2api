package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Handler 管理后台 API 处理器
type Handler struct {
	store          *auth.Store
	cache          cache.TokenCache
	db             *database.DB
	rateLimiter    *proxy.RateLimiter
	refreshAccount func(context.Context, int64) error
	cpuSampler     *cpuSampler
	startedAt      time.Time
	pgMaxConns     int
	redisPoolSize  int
	databaseDriver string
	databaseLabel  string
	cacheDriver    string
	cacheLabel     string
	adminSecretEnv string
	imageProxy     *proxy.Handler

	// 图表聚合内存缓存（10秒 TTL）
	chartCacheMu   sync.RWMutex
	chartCacheData map[string]*chartCacheEntry

	// 账号请求统计缓存（30秒 TTL）
	reqCountMu        sync.RWMutex
	reqCountCache     map[int64]*database.AccountRequestCount
	reqCountExpiresAt time.Time
}

type chartCacheEntry struct {
	data      *database.ChartAggregation
	expiresAt time.Time
}

// NewHandler 创建管理后台处理器
func NewHandler(store *auth.Store, db *database.DB, tc cache.TokenCache, rl *proxy.RateLimiter, adminSecretEnv string) *Handler {
	handler := &Handler{
		store:          store,
		cache:          tc,
		db:             db,
		rateLimiter:    rl,
		cpuSampler:     newCPUSampler(),
		startedAt:      time.Now(),
		databaseDriver: db.Driver(),
		databaseLabel:  db.Label(),
		cacheDriver:    tc.Driver(),
		cacheLabel:     tc.Label(),
		adminSecretEnv: adminSecretEnv,
		imageProxy:     proxy.NewHandler(store, db, nil, nil),
		chartCacheData: make(map[string]*chartCacheEntry),
	}
	handler.refreshAccount = handler.refreshSingleAccount
	if db != nil {
		if err := db.MarkInterruptedImageJobs(context.Background()); err != nil {
			log.Printf("标记中断生图任务失败: %v", err)
		}
	}
	return handler
}

// SetPoolSizes 设置连接池大小跟踪值（由 main.go 在启动时调用）
func (h *Handler) SetPoolSizes(pgMaxConns, redisPoolSize int) {
	h.pgMaxConns = pgMaxConns
	h.redisPoolSize = redisPoolSize
}

// RegisterRoutes 注册管理 API 路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/admin")
	api.Use(h.adminAuthMiddleware())
	api.GET("/stats", h.GetStats)
	api.GET("/accounts", h.ListAccounts)
	api.POST("/accounts", h.AddAccount)
	api.POST("/accounts/at", h.AddATAccount)
	api.POST("/accounts/import", h.ImportAccounts)
	api.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
	api.DELETE("/accounts/:id", h.DeleteAccount)
	api.POST("/accounts/:id/refresh", h.RefreshAccount)
	api.POST("/accounts/:id/lock", h.ToggleAccountLock)
	api.POST("/accounts/:id/reset-status", h.ResetAccountStatus)
	api.GET("/accounts/:id/test", h.TestConnection)
	api.GET("/accounts/:id/usage", h.GetAccountUsage)
	api.GET("/accounts/:id/auth-json", h.GetAccountAuthJSON)
	api.POST("/accounts/batch-test", h.BatchTest)
	api.POST("/accounts/batch-reset-status", h.BatchResetStatus)
	api.POST("/accounts/clean-banned", h.CleanBanned)
	api.POST("/accounts/clean-rate-limited", h.CleanRateLimited)
	api.POST("/accounts/clean-error", h.CleanError)
	api.GET("/accounts/export", h.ExportAccounts)
	api.POST("/accounts/migrate", h.MigrateAccounts)
	api.GET("/accounts/event-trend", h.GetAccountEventTrend)
	api.GET("/usage/stats", h.GetUsageStats)
	api.GET("/usage/logs", h.GetUsageLogs)
	api.GET("/usage/chart-data", h.GetChartData)
	api.DELETE("/usage/logs", h.ClearUsageLogs)
	api.GET("/keys", h.ListAPIKeys)
	api.POST("/keys", h.CreateAPIKey)
	api.DELETE("/keys/:id", h.DeleteAPIKey)
	api.GET("/health", h.GetHealth)
	api.GET("/ops/overview", h.GetOpsOverview)
	api.GET("/settings", h.GetSettings)
	api.PUT("/settings", h.UpdateSettings)
	api.GET("/models", h.ListModels)
	api.POST("/models/sync", h.SyncModels)
	api.GET("/image-prompts", h.ListImagePromptTemplates)
	api.POST("/image-prompts", h.CreateImagePromptTemplate)
	api.PATCH("/image-prompts/:id", h.UpdateImagePromptTemplate)
	api.DELETE("/image-prompts/:id", h.DeleteImagePromptTemplate)
	api.POST("/images/jobs", h.CreateImageGenerationJob)
	api.GET("/images/jobs", h.ListImageGenerationJobs)
	api.GET("/images/jobs/:id", h.GetImageGenerationJob)
	api.GET("/images/assets", h.ListImageAssets)
	api.GET("/images/assets/:id/file", h.GetImageAssetFile)
	api.DELETE("/images/assets/:id", h.DeleteImageAsset)
	api.GET("/proxies", h.ListProxies)
	api.POST("/proxies", h.AddProxies)
	api.DELETE("/proxies/:id", h.DeleteProxy)
	api.PATCH("/proxies/:id", h.UpdateProxy)
	api.POST("/proxies/batch-delete", h.BatchDeleteProxies)
	api.POST("/proxies/test", h.TestProxy)

	// OAuth 授权流程
	api.POST("/oauth/generate-auth-url", h.GenerateOAuthURL)
	api.POST("/oauth/exchange-code", h.ExchangeOAuthCode)
	api.GET("/oauth/poll-callback", h.PollOAuthCallback)

	// OAuth 回调端点（无需 admin 鉴权，供 OpenAI 重定向调用）
	r.GET("/auth/callback", h.OAuthCallback)
}

// adminAuthMiddleware 管理接口鉴权中间件（增强版，增加安全审计日志）
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		adminSecret, source := h.resolveAdminSecret(c.Request.Context())
		if adminSecret == "" {
			// 未配置管理密钥，跳过鉴权
			c.Next()
			return
		}

		adminKey := c.GetHeader("X-Admin-Key")
		if adminKey == "" {
			// 兼容 Authorization: Bearer 方式
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				adminKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		// 清理输入
		adminKey = security.SanitizeInput(adminKey)

		// 使用安全比较防止时序攻击
		if !security.SecureCompare(adminKey, adminSecret) {
			// 记录安全审计日志
			security.SecurityAuditLog("ADMIN_AUTH_FAILED", fmt.Sprintf("path=%s ip=%s source=%s", c.Request.URL.Path, c.ClientIP(), source))
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "管理密钥无效或缺失",
			})
			c.Abort()
			return
		}

		// 成功认证，记录审计日志
		if security.IsSensitiveEndpoint(c.Request.URL.Path) {
			security.SecurityAuditLog("ADMIN_ACCESS", fmt.Sprintf("path=%s ip=%s method=%s", c.Request.URL.Path, c.ClientIP(), c.Request.Method))
		}

		c.Next()
	}
}

func (h *Handler) resolveAdminSecret(ctx context.Context) (string, string) {
	if h.adminSecretEnv != "" {
		return h.adminSecretEnv, "env"
	}

	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(readCtx)
	if err != nil || settings == nil || settings.AdminSecret == "" {
		return "", "disabled"
	}
	return settings.AdminSecret, "database"
}

func (h *Handler) hasConfiguredAdminSecret(ctx context.Context) bool {
	adminSecret, _ := h.resolveAdminSecret(ctx)
	return strings.TrimSpace(adminSecret) != ""
}

// ==================== Stats ====================

// GetStats 获取仪表盘统计
func (h *Handler) GetStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	accounts, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	total := len(accounts)
	available := h.store.AvailableCount()
	errCount := 0
	for _, acc := range accounts {
		if acc.Status == "error" {
			errCount++
		}
	}

	usageStats, _ := h.db.GetUsageStats(ctx)
	todayReqs := int64(0)
	if usageStats != nil {
		todayReqs = usageStats.TodayRequests
	}

	c.JSON(http.StatusOK, statsResponse{
		Total:         total,
		Available:     available,
		Error:         errCount,
		TodayRequests: todayReqs,
	})
}

// ==================== Accounts ====================

type accountResponse struct {
	ID                       int64                      `json:"id"`
	Name                     string                     `json:"name"`
	Email                    string                     `json:"email"`
	PlanType                 string                     `json:"plan_type"`
	Status                   string                     `json:"status"`
	ATOnly                   bool                       `json:"at_only"`
	HealthTier               string                     `json:"health_tier"`
	SchedulerScore           float64                    `json:"scheduler_score"`
	DispatchScore            float64                    `json:"dispatch_score"`
	ScoreBiasOverride        *int64                     `json:"score_bias_override"`
	ScoreBiasEffective       int64                      `json:"score_bias_effective"`
	BaseConcurrencyOverride  *int64                     `json:"base_concurrency_override"`
	BaseConcurrencyEffective int64                      `json:"base_concurrency_effective"`
	ConcurrencyCap           int64                      `json:"dynamic_concurrency_limit"`
	ProxyURL                 string                     `json:"proxy_url"`
	CreatedAt                string                     `json:"created_at"`
	UpdatedAt                string                     `json:"updated_at"`
	ActiveRequests           int64                      `json:"active_requests"`
	TotalRequests            int64                      `json:"total_requests"`
	LastUsedAt               string                     `json:"last_used_at"`
	SuccessRequests          int64                      `json:"success_requests"`
	ErrorRequests            int64                      `json:"error_requests"`
	UsagePercent7d           *float64                   `json:"usage_percent_7d"`
	UsagePercent5h           *float64                   `json:"usage_percent_5h"`
	Reset5hAt                string                     `json:"reset_5h_at,omitempty"`
	Reset7dAt                string                     `json:"reset_7d_at,omitempty"`
	// 5h/7d 详细用量数据（用于列表显示）
	Usage5hDetail            *timeRangeUsageResponse    `json:"usage_5h_detail,omitempty"`
	Usage7dDetail            *timeRangeUsageResponse    `json:"usage_7d_detail,omitempty"`
	// Free 账号额度信息
	FreeQuota                *freeQuotaResponse         `json:"free_quota,omitempty"`
	// 图片配额信息
	ImageQuotaRemaining      *int                       `json:"image_quota_remaining,omitempty"`
	ImageQuotaTotal          *int                       `json:"image_quota_total,omitempty"`
	TodayUsedCount           *int                       `json:"today_used_count,omitempty"`
	ImageQuotaResetAt        string                     `json:"image_quota_reset_at,omitempty"`
	ScoreBreakdown           schedulerBreakdownResponse `json:"scheduler_breakdown"`
	LastUnauthorizedAt       string                     `json:"last_unauthorized_at,omitempty"`
	LastRateLimitedAt        string                     `json:"last_rate_limited_at,omitempty"`
	LastTimeoutAt            string                     `json:"last_timeout_at,omitempty"`
	LastServerErrorAt        string                     `json:"last_server_error_at,omitempty"`
	Locked                   bool                       `json:"locked"`
	AllowedAPIKeyIDs         []int64                    `json:"allowed_api_key_ids"`
}

type timeRangeUsageResponse struct {
	Requests      int64   `json:"requests"`
	Tokens        int64   `json:"tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
}

type freeQuotaResponse struct {
	RemainingTokens int64   `json:"remaining_tokens"` // 剩余 Token 数
	RemainingAmount float64 `json:"remaining_amount"` // 剩余金额（美元）
	TotalTokens     int64   `json:"total_tokens"`     // 总 Token 数
	TotalAmount     float64 `json:"total_amount"`     // 总金额（美元）
	ResetAt         string  `json:"reset_at"`         // 重置时间
}

type schedulerBreakdownResponse struct {
	UnauthorizedPenalty float64 `json:"unauthorized_penalty"`
	RateLimitPenalty    float64 `json:"rate_limit_penalty"`
	TimeoutPenalty      float64 `json:"timeout_penalty"`
	ServerPenalty       float64 `json:"server_penalty"`
	FailurePenalty      float64 `json:"failure_penalty"`
	SuccessBonus        float64 `json:"success_bonus"`
	UsagePenalty7d      float64 `json:"usage_penalty_7d"`
	LatencyPenalty      float64 `json:"latency_penalty"`
	SuccessRatePenalty  float64 `json:"success_rate_penalty"`
}

// ListAccounts 获取账号列表
func (h *Handler) ListAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	h.store.TriggerUsageProbeAsync()
	h.store.TriggerRecoveryProbeAsync()

	rows, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 合并内存中的调度指标
	accountMap := make(map[int64]*auth.Account)
	for _, acc := range h.store.Accounts() {
		accountMap[acc.DBID] = acc
	}

	// 获取每账号近 7 天请求统计（带 30 秒内存缓存）
	reqCounts := h.getCachedRequestCounts()

	accounts := make([]accountResponse, 0, len(rows))
	for _, row := range rows {
		resp := accountResponse{
			ID:                       row.ID,
			Name:                     row.Name,
			Email:                    row.GetCredential("email"),
			PlanType:                 row.GetCredential("plan_type"),
			Status:                   row.Status,
			ATOnly:                   row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
			ProxyURL:                 row.ProxyURL,
			Locked:                   row.Locked,
			AllowedAPIKeyIDs:         row.GetCredentialInt64Slice("allowed_api_key_ids"),
			ScoreBiasOverride:        nullableInt64Pointer(row.ScoreBiasOverride),
			ScoreBiasEffective:       effectiveScoreBias(row.GetCredential("plan_type"), row.ScoreBiasOverride),
			BaseConcurrencyOverride:  nullableInt64Pointer(row.BaseConcurrencyOverride),
			BaseConcurrencyEffective: effectiveBaseConcurrency(row.BaseConcurrencyOverride, int64(h.store.GetMaxConcurrency())),
			CreatedAt:                row.CreatedAt.Format(time.RFC3339),
			UpdatedAt:                row.UpdatedAt.Format(time.RFC3339),
		}
		if acc, ok := accountMap[row.ID]; ok {
			resp.ActiveRequests = acc.GetActiveRequests()
			resp.TotalRequests = acc.GetTotalRequests()
			debug := acc.GetSchedulerDebugSnapshot(int64(h.store.GetMaxConcurrency()))
			resp.HealthTier = debug.HealthTier
			resp.SchedulerScore = debug.SchedulerScore
			resp.ConcurrencyCap = debug.DynamicConcurrencyLimit
			if dispatchScore, ok := reflectFloat64Field(debug, "DispatchScore"); ok {
				resp.DispatchScore = dispatchScore
			}
			if scoreBiasEffective, ok := reflectInt64Field(debug, "ScoreBiasEffective"); ok {
				resp.ScoreBiasEffective = scoreBiasEffective
			}
			if baseConcurrencyEffective, ok := reflectInt64Field(debug, "BaseConcurrencyEffective"); ok {
				resp.BaseConcurrencyEffective = baseConcurrencyEffective
			}
			resp.ScoreBreakdown = schedulerBreakdownResponse{
				UnauthorizedPenalty: debug.Breakdown.UnauthorizedPenalty,
				RateLimitPenalty:    debug.Breakdown.RateLimitPenalty,
				TimeoutPenalty:      debug.Breakdown.TimeoutPenalty,
				ServerPenalty:       debug.Breakdown.ServerPenalty,
				FailurePenalty:      debug.Breakdown.FailurePenalty,
				SuccessBonus:        debug.Breakdown.SuccessBonus,
				UsagePenalty7d:      debug.Breakdown.UsagePenalty7d,
				LatencyPenalty:      debug.Breakdown.LatencyPenalty,
				SuccessRatePenalty:  debug.Breakdown.SuccessRatePenalty,
			}
			if usagePct, ok := acc.GetUsagePercent7d(); ok {
				resp.UsagePercent7d = &usagePct
			}
			if usagePct5h, ok := acc.GetUsagePercent5h(); ok {
				resp.UsagePercent5h = &usagePct5h
			}
			if t := acc.GetReset5hAt(); !t.IsZero() {
				resp.Reset5hAt = t.Format(time.RFC3339)
			}
			if t := acc.GetReset7dAt(); !t.IsZero() {
				resp.Reset7dAt = t.Format(time.RFC3339)
			}
			if t := acc.GetLastUsedAt(); !t.IsZero() {
				resp.LastUsedAt = t.Format(time.RFC3339)
			}
			if !debug.LastUnauthorizedAt.IsZero() {
				resp.LastUnauthorizedAt = debug.LastUnauthorizedAt.Format(time.RFC3339)
			}
			if !debug.LastRateLimitedAt.IsZero() {
				resp.LastRateLimitedAt = debug.LastRateLimitedAt.Format(time.RFC3339)
			}
			if !debug.LastTimeoutAt.IsZero() {
				resp.LastTimeoutAt = debug.LastTimeoutAt.Format(time.RFC3339)
			}
			if !debug.LastServerErrorAt.IsZero() {
				resp.LastServerErrorAt = debug.LastServerErrorAt.Format(time.RFC3339)
			}
			// 使用运行时状态（优先于 DB 状态）
			resp.Status = acc.RuntimeStatus()
		}
		if resp.DispatchScore == 0 {
			resp.DispatchScore = dispatchScoreFallback(resp.SchedulerScore, resp.ScoreBiasEffective, resp.HealthTier, resp.Status)
		}
		if rc, ok := reqCounts[row.ID]; ok {
			resp.SuccessRequests = rc.SuccessCount
			resp.ErrorRequests = rc.ErrorCount
		}
		accounts = append(accounts, resp)
	}

	// 并发查询所有账号的 5h/7d 详细用量数据和图片配额
	type usageResult struct {
		accountID           int64
		usage5h             *timeRangeUsageResponse
		usage7d             *timeRangeUsageResponse
		imageQuotaRemaining *int
		imageQuotaTotal     *int
		imageQuotaResetAt   string
	}
	usageResults := make(chan usageResult, len(accounts))
	var wg sync.WaitGroup

	for i := range accounts {
		wg.Add(1)
		go func(acc *accountResponse, row *database.AccountRow) {
			defer wg.Done()
			result := usageResult{accountID: acc.ID}

			// 查询 5h 用量
			if usage5h, err := h.db.GetTimeRangeUsage(ctx, acc.ID, 5*time.Hour); err == nil && usage5h != nil {
				result.usage5h = &timeRangeUsageResponse{
					Requests:      usage5h.Requests,
					Tokens:        usage5h.Tokens,
					AccountBilled: usage5h.AccountBilled,
					UserBilled:    usage5h.UserBilled,
				}
			}

			// 查询 7d 用量
			if usage7d, err := h.db.GetTimeRangeUsage(ctx, acc.ID, 7*24*time.Hour); err == nil && usage7d != nil {
				result.usage7d = &timeRangeUsageResponse{
					Requests:      usage7d.Requests,
					Tokens:        usage7d.Tokens,
					AccountBilled: usage7d.AccountBilled,
					UserBilled:    usage7d.UserBilled,
				}
			}

			// 获取图片配额信息（仅对 active 状态的账号）
			if acc.Status == "active" || acc.Status == "ready" {
				accessToken := row.GetCredential("access_token")
				if accessToken != "" {
					if quota, err := h.fetchImageQuota(ctx, accessToken, row.ProxyURL); err == nil && quota != nil {
						result.imageQuotaRemaining = &quota.Remaining
						result.imageQuotaTotal = &quota.Total
						if !quota.ResetAt.IsZero() {
							result.imageQuotaResetAt = quota.ResetAt.Format(time.RFC3339)
						}
					}
				}
			}

			usageResults <- result
		}(&accounts[i], rows[i])
	}

	// 等待所有查询完成
	go func() {
		wg.Wait()
		close(usageResults)
	}()

	// 将用量数据和图片配额填充到对应的账号
	usageMap := make(map[int64]usageResult)
	for result := range usageResults {
		usageMap[result.accountID] = result
	}

	for i := range accounts {
		if result, ok := usageMap[accounts[i].ID]; ok {
			accounts[i].Usage5hDetail = result.usage5h
			accounts[i].Usage7dDetail = result.usage7d
			accounts[i].ImageQuotaRemaining = result.imageQuotaRemaining
			accounts[i].ImageQuotaTotal = result.imageQuotaTotal
			accounts[i].ImageQuotaResetAt = result.imageQuotaResetAt
		}
	}

	c.JSON(http.StatusOK, accountsResponse{Accounts: accounts})
}

type updateAccountSchedulerReq struct {
	ScoreBiasOverride       json.RawMessage `json:"score_bias_override"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	AllowedAPIKeyIDs        json.RawMessage `json:"allowed_api_key_ids"`
}

// UpdateAccountScheduler 更新账号调度配置。
func (h *Handler) UpdateAccountScheduler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req updateAccountSchedulerReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	scoreBiasOverride, err := parseOptionalIntegerField(req.ScoreBiasOverride, "score_bias_override", -200, 200)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	baseConcurrencyOverride, err := parseOptionalIntegerField(req.BaseConcurrencyOverride, "base_concurrency_override", 1, 50)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	allowedAPIKeyIDs, err := parseOptionalIntegerSliceField(req.AllowedAPIKeyIDs, "allowed_api_key_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if allowedAPIKeyIDs.Set {
		missingAPIKeyIDs, err := h.findMissingAPIKeyIDs(ctx, allowedAPIKeyIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "校验 API Key 失败: "+err.Error())
			return
		}
		if len(missingAPIKeyIDs) > 0 {
			values := make([]string, 0, len(missingAPIKeyIDs))
			for _, value := range missingAPIKeyIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_api_key_ids 包含不存在的 API Key ID: "+strings.Join(values, ", "))
			return
		}
	}

	if err := h.db.UpdateAccountSchedulerConfig(ctx, id, scoreBiasOverride, baseConcurrencyOverride, allowedAPIKeyIDs); err != nil {
		if err == sql.ErrNoRows {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "更新账号调度配置失败: "+err.Error())
		return
	}
	if h.store != nil {
		h.store.ApplyAccountSchedulerOverrides(id, nullableInt64Pointer(scoreBiasOverride), nullableInt64Pointer(baseConcurrencyOverride))
		if allowedAPIKeyIDs.Set {
			h.store.ApplyAccountAllowedAPIKeys(id, allowedAPIKeyIDs.Values)
		}
	}

	writeMessage(c, http.StatusOK, "账号调度配置已更新")
}

func parseOptionalIntegerField(raw json.RawMessage, field string, minValue, maxValue int64) (sql.NullInt64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return sql.NullInt64{}, nil
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return sql.NullInt64{}, fmt.Errorf("%s 必须是整数或 null", field)
	}
	value, err := number.Int64()
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("%s 必须是整数或 null", field)
	}
	if value < minValue || value > maxValue {
		return sql.NullInt64{}, fmt.Errorf("%s 超出范围，必须在 %d..%d 之间", field, minValue, maxValue)
	}
	return sql.NullInt64{Int64: value, Valid: true}, nil
}

func parseOptionalIntegerSliceField(raw json.RawMessage, field string) (database.OptionalInt64Slice, error) {
	if len(raw) == 0 {
		return database.OptionalInt64Slice{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalInt64Slice{Set: true, Values: []int64{}}, nil
	}

	var values []json.Number
	if err := json.Unmarshal(raw, &values); err != nil {
		return database.OptionalInt64Slice{}, fmt.Errorf("%s 必须是整数数组或 null", field)
	}
	if len(values) == 0 {
		return database.OptionalInt64Slice{Set: true, Values: []int64{}}, nil
	}

	unique := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, number := range values {
		value, err := number.Int64()
		if err != nil {
			return database.OptionalInt64Slice{}, fmt.Errorf("%s 必须是整数数组或 null", field)
		}
		if value <= 0 {
			return database.OptionalInt64Slice{}, fmt.Errorf("%s 中的值必须是正整数", field)
		}
		if _, exists := unique[value]; exists {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return database.OptionalInt64Slice{Set: true, Values: result}, nil
}

func (h *Handler) findMissingAPIKeyIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[int64]struct{}, len(keys))
	for _, key := range keys {
		if key == nil {
			continue
		}
		existing[key.ID] = struct{}{}
	}

	missing := make([]int64, 0)
	for _, id := range ids {
		if _, ok := existing[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	return missing, nil
}

func nullableInt64Pointer(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

func effectiveScoreBias(planType string, override sql.NullInt64) int64 {
	if override.Valid {
		return override.Int64
	}
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "pro", "plus", "team":
		return 50
	default:
		return 0
	}
}

func effectiveBaseConcurrency(override sql.NullInt64, defaultValue int64) int64 {
	if override.Valid {
		return override.Int64
	}
	return defaultValue
}

func dispatchScoreFallback(schedulerScore float64, scoreBiasEffective int64, healthTier string, status string) float64 {
	if schedulerScore == 0 {
		return 0
	}
	if !allowScoreBias(healthTier, status) {
		return schedulerScore
	}
	return schedulerScore + float64(scoreBiasEffective)
}

func allowScoreBias(healthTier string, status string) bool {
	if status != "" && status != "active" {
		return false
	}
	switch strings.ToLower(healthTier) {
	case "healthy", "warm":
		return true
	default:
		return false
	}
}

// 这里优先读取 auth 层并行实现新增的 runtime/debug 字段，字段名约定为：
// DispatchScore / ScoreBiasEffective / BaseConcurrencyEffective。
// 若主分支尚未集成这些字段，则回退到管理层可推导的兼容值，避免阻塞前后端联调。
func reflectFloat64Field(value interface{}, field string) (float64, bool) {
	v := reflect.Indirect(reflect.ValueOf(value))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Float32, reflect.Float64:
		return f.Convert(reflect.TypeOf(float64(0))).Float(), true
	default:
		return 0, false
	}
}

func reflectInt64Field(value interface{}, field string) (int64, bool) {
	v := reflect.Indirect(reflect.ValueOf(value))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return f.Int(), true
	default:
		return 0, false
	}
}

// getCachedRequestCounts 返回带 30 秒 TTL 的账号请求统计缓存
func (h *Handler) getCachedRequestCounts() map[int64]*database.AccountRequestCount {
	h.reqCountMu.RLock()
	if h.reqCountCache != nil && time.Now().Before(h.reqCountExpiresAt) {
		cached := h.reqCountCache
		h.reqCountMu.RUnlock()
		return cached
	}
	h.reqCountMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	counts, err := h.db.GetAccountRequestCounts(ctx)
	if err != nil {
		log.Printf("获取账号请求统计失败: %v", err)
		return make(map[int64]*database.AccountRequestCount)
	}

	h.reqCountMu.Lock()
	h.reqCountCache = counts
	h.reqCountExpiresAt = time.Now().Add(30 * time.Second)
	h.reqCountMu.Unlock()

	return counts
}

type addAccountReq struct {
	Name         string `json:"name"`
	RefreshToken string `json:"refresh_token"`
	ProxyURL     string `json:"proxy_url"`
}

// AddAccount 添加新账号（支持批量：refresh_token 按行分割）
func (h *Handler) AddAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 输入验证和清理
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if req.RefreshToken == "" {
		writeError(c, http.StatusBadRequest, "refresh_token 是必填字段")
		return
	}

	// 检查XSS和SQL注入
	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	// 验证名称长度
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	// 验证代理URL
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	// 按行分割，支持批量添加
	lines := strings.Split(req.RefreshToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(security.SanitizeInput(line))
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Refresh Token")
		return
	}

	// 限制批量添加数量
	if len(tokens) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多添加100个账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0

	for i, rt := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		id, err := h.db.InsertAccount(ctx, name, rt, req.ProxyURL)
		if err != nil {
			log.Printf("批量添加账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual")

		// 热加载：直接加入内存池
		newAcc := &auth.Account{
			DBID:         id,
			RefreshToken: rt,
			ProxyURL:     req.ProxyURL,
		}
		h.store.AddAccount(newAcc)

		// 异步刷新 AT
		go func(accountID int64) {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
				log.Printf("新账号 %d 刷新失败: %v", accountID, err)
			} else {
				log.Printf("新账号 %d 刷新成功，已加入号池", accountID)
			}
		}(id)
	}

	// 记录安全审计日志
	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d failed=%d ip=%s", successCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功添加 %d 个账号", successCount)
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": msg,
		"success": successCount,
		"failed":  failCount,
	})
}

// addATAccountReq AT 模式添加账号请求
type addATAccountReq struct {
	Name        string `json:"name"`
	AccessToken string `json:"access_token"`
	ProxyURL    string `json:"proxy_url"`
}

// AddATAccount 添加 AT-only 账号（支持批量：access_token 按行分割）
func (h *Handler) AddATAccount(c *gin.Context) {
	var req addATAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if req.AccessToken == "" {
		writeError(c, http.StatusBadRequest, "access_token 是必填字段")
		return
	}

	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	// 按行分割，支持批量添加
	lines := strings.Split(req.AccessToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Access Token")
		return
	}

	if len(tokens) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多添加100个账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0

	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		id, err := h.db.InsertATAccount(ctx, name, at, req.ProxyURL)
		if err != nil {
			log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual_at")

		// 解析 AT JWT 提取账号信息（email、plan_type、account_id、过期时间）
		atInfo := auth.ParseAccessToken(at)

		// 热加载到内存池（AT-only，无 RT）
		newAcc := &auth.Account{
			DBID:        id,
			AccessToken: at,
			ExpiresAt:   time.Now().Add(1 * time.Hour),
			ProxyURL:    req.ProxyURL,
		}
		if atInfo != nil {
			newAcc.Email = atInfo.Email
			newAcc.AccountID = atInfo.ChatGPTAccountID
			newAcc.PlanType = atInfo.PlanType
			if !atInfo.ExpiresAt.IsZero() {
				newAcc.ExpiresAt = atInfo.ExpiresAt
			}
		}
		h.store.AddAccount(newAcc)

		// 将解析到的信息持久化到数据库
		if atInfo != nil {
			creds := map[string]interface{}{
				"email":      atInfo.Email,
				"account_id": atInfo.ChatGPTAccountID,
				"plan_type":  atInfo.PlanType,
				"expires_at": newAcc.ExpiresAt.Format(time.RFC3339),
			}
			if err := h.db.UpdateCredentials(ctx, id, creds); err != nil {
				log.Printf("AT 账号 %d 更新 credentials 失败: %v", id, err)
			}
		}
		log.Printf("AT 账号 %d 已加入号池 (id=%d, email=%s)", i+1, id, newAcc.Email)
	}

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d failed=%d ip=%s", successCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功添加 %d 个 AT 账号", successCount)
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": msg,
		"success": successCount,
		"failed":  failCount,
	})
}

// importToken 导入时的统一 token 载体
type importToken struct {
	refreshToken string
	accessToken  string // AT-only 兼容路径
	name         string
	email        string
	idToken      string
	accountID    string
	planType     string
	expiresAt    string
}

// jsonAccountEntry CLIProxyAPI 凭证 JSON 条目
type jsonAccountEntry struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	PlanType     string `json:"plan_type"`
	Expired      string `json:"expired"`
	ExpiresAt    string `json:"expires_at"`
}

type sub2apiImportPayload struct {
	Accounts []sub2apiAccountEntry `json:"accounts"`
}

type sub2apiAccountEntry struct {
	Name        string                    `json:"name"`
	Credentials sub2apiAccountCredentials `json:"credentials"`
}

type sub2apiAccountCredentials struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	PlanType     string `json:"plan_type"`
	ExpiresAt    string `json:"expires_at"`
	Expired      string `json:"expired"`
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func trimUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, utf8BOM)
}

// parseImportJSONTokens 同时兼容现有扁平 JSON 和 Sub2Api 顶层对象。
func parseImportJSONTokens(data []byte) ([]importToken, error) {
	data = trimUTF8BOM(data)
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid import json")
	}

	if tokens := parseFlatJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	if tokens := parseSub2APIJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	return nil, nil
}

func parseFlatJSONImportTokens(data []byte) []importToken {
	var entries []jsonAccountEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return jsonAccountEntriesToTokens(entries)
	}

	var single jsonAccountEntry
	if err := json.Unmarshal(data, &single); err == nil {
		return jsonAccountEntriesToTokens([]jsonAccountEntry{single})
	}

	return nil
}

func jsonAccountEntriesToTokens(entries []jsonAccountEntry) []importToken {
	tokens := make([]importToken, 0, len(entries))
	for _, entry := range entries {
		rt := strings.TrimSpace(entry.RefreshToken)
		at := strings.TrimSpace(entry.AccessToken)
		email := strings.TrimSpace(entry.Email)
		name := firstNonEmpty(entry.Name, email)

		if rt != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken: rt,
				accessToken:  at,
				name:         name,
				email:        email,
				idToken:      strings.TrimSpace(entry.IDToken),
				accountID:    strings.TrimSpace(entry.AccountID),
				planType:     strings.TrimSpace(entry.PlanType),
				expiresAt:    firstNonEmpty(entry.ExpiresAt, entry.Expired),
			})
		}
	}
	return tokens
}

func parseSub2APIJSONImportTokens(data []byte) []importToken {
	var payload sub2apiImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}

	tokens := make([]importToken, 0, len(payload.Accounts))
	for _, account := range payload.Accounts {
		rt := strings.TrimSpace(account.Credentials.RefreshToken)
		at := strings.TrimSpace(account.Credentials.AccessToken)
		name := strings.TrimSpace(account.Name)
		email := strings.TrimSpace(account.Credentials.Email)

		if name == "" {
			name = email
		}

		if rt != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken: rt,
				accessToken:  at,
				name:         name,
				email:        email,
				idToken:      strings.TrimSpace(account.Credentials.IDToken),
				accountID:    strings.TrimSpace(account.Credentials.AccountID),
				planType:     strings.TrimSpace(account.Credentials.PlanType),
				expiresAt:    firstNonEmpty(account.Credentials.ExpiresAt, account.Credentials.Expired),
			})
		}
	}

	return tokens
}

// ImportAccounts 批量导入账号（支持 TXT / JSON）
func (h *Handler) ImportAccounts(c *gin.Context) {
	format := c.DefaultPostForm("format", "txt")
	proxyURL := c.PostForm("proxy_url")

	switch format {
	case "json":
		h.importAccountsJSON(c, proxyURL)
	case "at_txt":
		h.importAccountsATTXT(c, proxyURL)
	default:
		h.importAccountsTXT(c, proxyURL)
	}
}

type uploadedImportFile struct {
	name string
	data []byte
}

func readUploadedImportFiles(c *gin.Context) ([]uploadedImportFile, error) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, fmt.Errorf("解析表单失败")
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		return nil, fmt.Errorf("请上传文件（字段名: file）")
	}

	result := make([]uploadedImportFile, 0, len(files))
	for _, fh := range files {
		if fh.Size > 2*1024*1024 {
			return nil, fmt.Errorf("文件 %s 大小超过 2MB", fh.Filename)
		}

		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("打开文件 %s 失败", fh.Filename)
		}
		data, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取文件 %s 失败", fh.Filename)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("关闭文件 %s 失败", fh.Filename)
		}

		result = append(result, uploadedImportFile{name: fh.Filename, data: data})
	}
	return result, nil
}

func importTokensFromTextFiles(files []uploadedImportFile, makeToken func(string) importToken) []importToken {
	seen := make(map[string]bool)
	var tokens []importToken
	for _, file := range files {
		lines := strings.Split(string(trimUTF8BOM(file.data)), "\n")
		for _, line := range lines {
			t := strings.TrimSpace(line)
			if t != "" && !seen[t] {
				seen[t] = true
				tokens = append(tokens, makeToken(t))
			}
		}
	}
	return tokens
}

// importAccountsTXT 通过 TXT 文件导入（每行一个 RT）
func (h *Handler) importAccountsTXT(c *gin.Context, proxyURL string) {
	files, err := readUploadedImportFiles(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	tokens := importTokensFromTextFiles(files, func(token string) importToken {
		return importToken{refreshToken: token}
	})
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "文件中未找到有效的 Refresh Token")
		return
	}

	h.importAccountsCommon(c, tokens, proxyURL)
}

// importAccountsJSON 通过 JSON 文件导入（兼容 CLIProxyAPI 凭证格式）
func (h *Handler) importAccountsJSON(c *gin.Context, proxyURL string) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		writeError(c, http.StatusBadRequest, "解析表单失败")
		return
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(c, http.StatusBadRequest, "请上传至少一个 JSON 文件")
		return
	}

	var allTokens []importToken

	for _, fh := range files {
		if fh.Size > 2*1024*1024 {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("文件 %s 大小超过 2MB", fh.Filename))
			return
		}

		f, err := fh.Open()
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("打开文件 %s 失败", fh.Filename))
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("读取文件 %s 失败", fh.Filename))
			return
		}

		tokens, err := parseImportJSONTokens(data)
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("文件 %s 不是有效的 JSON 格式", fh.Filename))
			return
		}

		allTokens = append(allTokens, tokens...)
	}

	if len(allTokens) == 0 {
		writeError(c, http.StatusBadRequest, "JSON 文件中未找到有效的 refresh_token 或 access_token")
		return
	}

	h.importAccountsCommon(c, allTokens, proxyURL)
}

// importEvent SSE 导入进度事件
type importEvent struct {
	Type      string `json:"type"` // progress | complete
	Current   int    `json:"current"`
	Total     int    `json:"total"`
	Success   int    `json:"success"`
	Duplicate int    `json:"duplicate"`
	Failed    int    `json:"failed"`
}

func sendImportEvent(c *gin.Context, e importEvent) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}

func setupSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

// importAccountsCommon 公共的去重、并发插入、SSE 进度推送逻辑（支持 RT 和 AT-only 混合导入）
func (h *Handler) importAccountsCommon(c *gin.Context, tokens []importToken, proxyURL string) {
	// 文件内去重（RT 和 AT 分别去重）
	seenRT := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []importToken
	for _, t := range tokens {
		if t.refreshToken != "" {
			if !seenRT[t.refreshToken] {
				seenRT[t.refreshToken] = true
				unique = append(unique, t)
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
		} else if t.accessToken != "" {
			if !seenAT[t.accessToken] {
				seenAT[t.accessToken] = true
				unique = append(unique, t)
			}
		}
	}

	// 数据库去重（独立短超时）
	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()

	log.Printf("导入解析: 文件内 %d 条, 去重后 %d 条（%d 条文件内重复）", len(tokens), len(unique), len(tokens)-len(unique))

	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		existingRTs = make(map[string]bool)
	}

	// 存在 AT-only token 时额外查询已有 AT
	hasAT := len(seenAT) > 0
	var existingATs map[string]bool
	if hasAT {
		existingATs, err = h.db.GetAllAccessTokens(dedupeCtx)
		if err != nil {
			log.Printf("查询已有 AT 失败: %v", err)
			existingATs = make(map[string]bool)
		}
	}

	var newTokens []importToken
	duplicateCount := 0
	for _, t := range unique {
		switch {
		case t.refreshToken != "":
			if existingRTs[t.refreshToken] {
				duplicateCount++
			} else if t.accessToken != "" && existingATs[t.accessToken] {
				duplicateCount++
			} else {
				newTokens = append(newTokens, t)
			}
		case t.accessToken != "":
			if existingATs[t.accessToken] {
				duplicateCount++
			} else {
				newTokens = append(newTokens, t)
			}
		}
	}

	total := len(unique)

	log.Printf("导入去重: 总计 %d 条, 数据库已存在 %d 条, 待导入 %d 条", total, duplicateCount, len(newTokens))

	if len(newTokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个 Token 已存在，无需导入", total),
			"success":   0,
			"duplicate": duplicateCount,
			"failed":    0,
			"total":     total,
		})
		return
	}

	// 切换到 SSE 流式响应
	setupSSE(c)

	var successCount int64
	var failCount int64
	var current int64
	sem := make(chan struct{}, 20) // 并发插入上限
	var wg sync.WaitGroup

	// 进度推送 goroutine：定时发送，避免每条都写造成 IO 瓶颈
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cur := int(atomic.LoadInt64(&current))
				suc := int(atomic.LoadInt64(&successCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Duplicate: duplicateCount, Failed: fai,
				})
			case <-done:
				return
			}
		}
	}()

	for i, t := range newTokens {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, tok importToken) {
			defer wg.Done()
			defer func() { <-sem }()

			name := tok.name

			if tok.accessToken != "" && tok.refreshToken == "" {
				// AT-only 导入路径
				if name == "" {
					name = fmt.Sprintf("at-import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, err := h.db.InsertATAccount(insertCtx, name, tok.accessToken, proxyURL)
				insertCancel()

				if err != nil {
					log.Printf("导入 AT 账号 %d/%d 失败: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				atomic.AddInt64(&successCount, 1)
				atomic.AddInt64(&current, 1)
				h.db.InsertAccountEventAsync(id, "added", "import_at")

				seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
					accessToken:  tok.accessToken,
					idToken:      tok.idToken,
					accountID:    tok.accountID,
					email:        tok.email,
					planType:     tok.planType,
					expiresAtRaw: tok.expiresAt,
				})
				newAcc := accountFromCredentialSeed(id, proxyURL, seed)
				if len(tokenCredentialMap(seed)) > 0 {
					credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
					_ = h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed))
					credCancel()
				}
				h.store.AddAccount(newAcc)
			} else {
				// RT 导入路径；如果导入文件里同时带 AT，则先沿用它，后台调度到期前再刷新。
				if name == "" {
					name = fmt.Sprintf("import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, err := h.db.InsertAccount(insertCtx, name, tok.refreshToken, proxyURL)
				insertCancel()

				if err != nil {
					log.Printf("导入账号 %d/%d 失败: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				atomic.AddInt64(&successCount, 1)
				atomic.AddInt64(&current, 1)
				h.db.InsertAccountEventAsync(id, "added", "import")

				seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
					refreshToken: tok.refreshToken,
					accessToken:  tok.accessToken,
					idToken:      tok.idToken,
					accountID:    tok.accountID,
					email:        tok.email,
					planType:     tok.planType,
					expiresAtRaw: tok.expiresAt,
				})
				if len(tokenCredentialMap(seed)) > 0 {
					credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
					if err := h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed)); err != nil {
						log.Printf("导入账号 %d 更新 token 信息失败: %v", id, err)
					}
					credCancel()
				}
				newAcc := accountFromCredentialSeed(id, proxyURL, seed)
				h.store.AddAccount(newAcc)

				if tok.accessToken == "" {
					// 后台异步刷新，不阻塞导入流程
					go func(accountID int64) {
						refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						if err := h.store.RefreshSingle(refreshCtx, accountID); err != nil {
							log.Printf("导入账号 %d 刷新失败: %v", accountID, err)
						} else {
							log.Printf("导入账号 %d 刷新成功", accountID)
						}
					}(id)
				}
			}
		}(i, t)
	}

	wg.Wait()
	close(done)

	// 发送完成事件
	suc := int(atomic.LoadInt64(&successCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("导入完成: success=%d, duplicate=%d, failed=%d, total=%d", suc, duplicateCount, fai, total)
}

// importAccountsATTXT 通过 TXT 文件导入 AT-only 账号（每行一个 Access Token）
func (h *Handler) importAccountsATTXT(c *gin.Context, proxyURL string) {
	files, err := readUploadedImportFiles(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	tokens := importTokensFromTextFiles(files, func(token string) importToken {
		return importToken{accessToken: token}
	})
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "文件中未找到有效的 Access Token")
		return
	}

	h.importAccountsCommon(c, tokens, proxyURL)
}

// GetAccountUsage 查询单个账号的用量统计
func (h *Handler) GetAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	detail, err := h.db.GetAccountUsageStats(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// DeleteAccount 删除账号
func (h *Handler) DeleteAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// 软删除：保留账号数据与事件记录，但从运行时池和 active 列表中移除。
	if err := h.db.SoftDeleteAccount(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}

	// 从内存池移除
	h.store.RemoveAccount(id)
	h.db.InsertAccountEventAsync(id, "deleted", "manual")

	writeMessage(c, http.StatusOK, "账号已删除")
}

// RefreshAccount 手动刷新账号 AT
func (h *Handler) RefreshAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	refreshFn := h.refreshAccount
	if refreshFn == nil {
		refreshFn = h.refreshSingleAccount
	}
	if err := refreshFn(ctx, id); err != nil {
		if strings.Contains(err.Error(), "不存在") {
			writeError(c, http.StatusNotFound, err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "刷新失败: "+err.Error())
		return
	}

	writeMessage(c, http.StatusOK, "账号刷新成功")
}

// ToggleAccountLock 切换账号的锁定状态
func (h *Handler) ToggleAccountLock(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		Locked bool `json:"locked"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.SetAccountLocked(ctx, id, req.Locked); err != nil {
		writeError(c, http.StatusInternalServerError, "更新锁定状态失败: "+err.Error())
		return
	}

	// 同步更新内存中的状态
	if acc := h.store.FindByID(id); acc != nil {
		if req.Locked {
			atomic.StoreInt32(&acc.Locked, 1)
		} else {
			atomic.StoreInt32(&acc.Locked, 0)
		}
	}

	if req.Locked {
		writeMessage(c, http.StatusOK, "账号已锁定")
	} else {
		writeMessage(c, http.StatusOK, "账号已解锁")
	}
}

// ResetAccountStatus 重置单个账号状态为正常
func (h *Handler) ResetAccountStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}

	h.store.ClearCooldown(acc)
	acc.ClearUsageCache()
	writeMessage(c, http.StatusOK, "账号状态已重置")
}

// BatchResetStatus 批量重置账号状态为正常
func (h *Handler) BatchResetStatus(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要重置的账号 ID 列表")
		return
	}

	success := 0
	fail := 0
	for _, id := range req.IDs {
		acc := h.store.FindByID(id)
		if acc == nil {
			fail++
			continue
		}
		h.store.ClearCooldown(acc)
		acc.ClearUsageCache()
		success++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已重置 %d 个账号状态", success),
		"success": success,
		"failed":  fail,
	})
}

func (h *Handler) refreshSingleAccount(ctx context.Context, id int64) error {
	if h == nil || h.store == nil {
		return fmt.Errorf("账号池未初始化")
	}
	return h.store.RefreshSingle(ctx, id)
}

// ==================== Health ====================

// GetHealth 系统健康检查（扩展版）
func (h *Handler) GetHealth(c *gin.Context) {
	c.JSON(http.StatusOK, healthResponse{
		Status:    "ok",
		Available: h.store.AvailableCount(),
		Total:     h.store.AccountCount(),
	})
}

// ==================== Usage ====================

// GetUsageStats 获取使用统计
func (h *Handler) GetUsageStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	stats, err := h.db.GetUsageStats(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// GetChartData 返回图表聚合数据（服务端分桶 + 内存缓存）
func (h *Handler) GetChartData(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	bucketStr := c.DefaultQuery("bucket_minutes", "5")

	startTime, e1 := time.Parse(time.RFC3339, startStr)
	endTime, e2 := time.Parse(time.RFC3339, endStr)
	if e1 != nil || e2 != nil {
		writeError(c, http.StatusBadRequest, "start/end 参数格式错误，需要 RFC3339 格式")
		return
	}
	bucketMinutes, _ := strconv.Atoi(bucketStr)
	if bucketMinutes < 1 {
		bucketMinutes = 5
	}

	// 检查内存缓存（10秒 TTL）
	cacheKey := fmt.Sprintf("%s|%s|%d", startStr, endStr, bucketMinutes)
	h.chartCacheMu.RLock()
	if entry, ok := h.chartCacheData[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		h.chartCacheMu.RUnlock()
		c.JSON(http.StatusOK, entry.data)
		return
	}
	h.chartCacheMu.RUnlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	result, err := h.db.GetChartAggregation(ctx, startTime, endTime, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 写入缓存
	h.chartCacheMu.Lock()
	h.chartCacheData[cacheKey] = &chartCacheEntry{
		data:      result,
		expiresAt: time.Now().Add(10 * time.Second),
	}
	// 清理过期条目（延迟清理，避免内存泄漏）
	for k, v := range h.chartCacheData {
		if time.Now().After(v.expiresAt) {
			delete(h.chartCacheData, k)
		}
	}
	h.chartCacheMu.Unlock()

	c.JSON(http.StatusOK, result)
}

// GetUsageLogs 获取使用日志
func (h *Handler) GetUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	startStr := c.Query("start")
	endStr := c.Query("end")

	if startStr != "" && endStr != "" {
		startTime, e1 := time.Parse(time.RFC3339, startStr)
		endTime, e2 := time.Parse(time.RFC3339, endStr)
		if e1 != nil || e2 != nil {
			writeError(c, http.StatusBadRequest, "start/end 参数格式错误，需要 RFC3339 格式")
			return
		}

		// 有 page 参数 → 服务端分页（Usage 页面表格）
		if pageStr := c.Query("page"); pageStr != "" {
			page, _ := strconv.Atoi(pageStr)
			pageSize := 20
			if ps := c.Query("page_size"); ps != "" {
				if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 200 {
					pageSize = n
				}
			}
			var apiKeyID *int64
			if apiKeyIDStr := c.Query("api_key_id"); apiKeyIDStr != "" {
				parsed, err := strconv.ParseInt(apiKeyIDStr, 10, 64)
				if err != nil || parsed <= 0 {
					writeError(c, http.StatusBadRequest, "api_key_id 参数无效，需要正整数")
					return
				}
				apiKeyID = &parsed
			}

			filter := database.UsageLogFilter{
				Start:    startTime,
				End:      endTime,
				Page:     page,
				PageSize: pageSize,
				Email:    c.Query("email"),
				Model:    c.Query("model"),
				Endpoint: c.Query("endpoint"),
				APIKeyID: apiKeyID,
			}
			if fastStr := c.Query("fast"); fastStr != "" {
				v := fastStr == "true"
				filter.FastOnly = &v
			}
			if streamStr := c.Query("stream"); streamStr != "" {
				v := streamStr == "true"
				filter.StreamOnly = &v
			}

			result, err := h.db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				writeInternalError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
			return
		}

		// 无 page 参数 → 返回全量（Dashboard 图表聚合）
		logs, err := h.db.ListUsageLogsByTimeRange(ctx, startTime, endTime)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if logs == nil {
			logs = []*database.UsageLog{}
		}
		c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
		return
	}

	// 回退：limit 模式
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	logs, err := h.db.ListRecentUsageLogs(ctx, limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if logs == nil {
		logs = []*database.UsageLog{}
	}
	c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
}

// ClearUsageLogs 清空所有使用日志
func (h *Handler) ClearUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if err := h.db.ClearUsageLogs(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "日志已清空"})
}

// ==================== API Keys ====================

// ListAPIKeys 获取所有 API 密钥（脱敏版本）
func (h *Handler) ListAPIKeys(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 转换为脱敏响应
	maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
	for _, k := range keys {
		maskedKeys = append(maskedKeys, NewMaskedAPIKeyRow(k))
	}

	c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
}

type createKeyReq struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// generateKey 生成随机 API Key
func generateKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

// CreateAPIKey 创建新 API 密钥（增强版，带输入验证）
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Name = ""
	}

	// 输入验证和清理
	req.Name = security.SanitizeInput(req.Name)
	if req.Name == "" {
		req.Name = "default"
	}

	// 验证名称长度
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}

	// 检查XSS
	if security.ContainsXSS(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}

	key := req.Key
	if key == "" {
		key = generateKey()
	} else {
		// 验证用户提供的key格式
		key = security.SanitizeInput(key)
		if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
			writeError(c, http.StatusBadRequest, "API Key格式无效")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	id, err := h.db.InsertAPIKey(ctx, req.Name, key)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}

	// 记录安全审计日志
	security.SecurityAuditLog("API_KEY_CREATED", fmt.Sprintf("id=%d name=%s ip=%s", id, security.SanitizeLog(req.Name), c.ClientIP()))

	c.JSON(http.StatusOK, createAPIKeyResponse{
		ID:   id,
		Key:  key,
		Name: req.Name,
	})
}

// DeleteAPIKey 删除 API 密钥
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.DeleteAPIKey(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "已删除")
}

// ==================== Settings ====================

type settingsResponse struct {
	MaxConcurrency                   int    `json:"max_concurrency"`
	GlobalRPM                        int    `json:"global_rpm"`
	TestModel                        string `json:"test_model"`
	TestConcurrency                  int    `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes int    `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes          int    `json:"usage_probe_max_age_minutes"`
	RecoveryProbeIntervalMinutes     int    `json:"recovery_probe_interval_minutes"`
	ProxyURL                         string `json:"proxy_url"`
	PgMaxConns                       int    `json:"pg_max_conns"`
	RedisPoolSize                    int    `json:"redis_pool_size"`
	AutoCleanUnauthorized            bool   `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited             bool   `json:"auto_clean_rate_limited"`
	AdminSecret                      string `json:"admin_secret"`
	AdminAuthSource                  string `json:"admin_auth_source"`
	AutoCleanFullUsage               bool   `json:"auto_clean_full_usage"`
	AutoCleanError                   bool   `json:"auto_clean_error"`
	AutoCleanExpired                 bool   `json:"auto_clean_expired"`
	ProxyPoolEnabled                 bool   `json:"proxy_pool_enabled"`
	FastSchedulerEnabled             bool   `json:"fast_scheduler_enabled"`
	MaxRetries                       int    `json:"max_retries"`
	AllowRemoteMigration             bool   `json:"allow_remote_migration"`
	DatabaseDriver                   string `json:"database_driver"`
	DatabaseLabel                    string `json:"database_label"`
	CacheDriver                      string `json:"cache_driver"`
	CacheLabel                       string `json:"cache_label"`
	ExpiredCleaned                   int    `json:"expired_cleaned,omitempty"`
	ModelMapping                     string `json:"model_mapping"`
	ResinURL                         string `json:"resin_url"`
	ResinPlatformName                string `json:"resin_platform_name"`
}

type updateSettingsReq struct {
	MaxConcurrency                   *int    `json:"max_concurrency"`
	GlobalRPM                        *int    `json:"global_rpm"`
	TestModel                        *string `json:"test_model"`
	TestConcurrency                  *int    `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes *int    `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes          *int    `json:"usage_probe_max_age_minutes"`
	RecoveryProbeIntervalMinutes     *int    `json:"recovery_probe_interval_minutes"`
	ProxyURL                         *string `json:"proxy_url"`
	PgMaxConns                       *int    `json:"pg_max_conns"`
	RedisPoolSize                    *int    `json:"redis_pool_size"`
	AutoCleanUnauthorized            *bool   `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited             *bool   `json:"auto_clean_rate_limited"`
	AdminSecret                      *string `json:"admin_secret"`
	AutoCleanFullUsage               *bool   `json:"auto_clean_full_usage"`
	AutoCleanError                   *bool   `json:"auto_clean_error"`
	AutoCleanExpired                 *bool   `json:"auto_clean_expired"`
	ProxyPoolEnabled                 *bool   `json:"proxy_pool_enabled"`
	FastSchedulerEnabled             *bool   `json:"fast_scheduler_enabled"`
	MaxRetries                       *int    `json:"max_retries"`
	AllowRemoteMigration             *bool   `json:"allow_remote_migration"`
	ModelMapping                     *string `json:"model_mapping"`
	ResinURL                         *string `json:"resin_url"`
	ResinPlatformName                *string `json:"resin_platform_name"`
}

// GetSettings 获取当前系统设置
func (h *Handler) GetSettings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	dbSettings, _ := h.db.GetSystemSettings(ctx)
	_, adminAuthSource := h.resolveAdminSecret(c.Request.Context())
	adminSecret := ""
	var resinURL, resinPlatformName string
	if dbSettings != nil && adminAuthSource != "env" {
		adminSecret = dbSettings.AdminSecret
	}
	if dbSettings != nil {
		resinURL = dbSettings.ResinURL
		resinPlatformName = dbSettings.ResinPlatformName
	}
	c.JSON(http.StatusOK, settingsResponse{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      adminSecret,
		AdminAuthSource:                  adminAuthSource,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                   h.databaseDriver,
		DatabaseLabel:                    h.databaseLabel,
		CacheDriver:                      h.cacheDriver,
		CacheLabel:                       h.cacheLabel,
		ModelMapping:                     h.store.GetModelMapping(),
		ResinURL:                         resinURL,
		ResinPlatformName:                resinPlatformName,
	})
}

// UpdateSettings 更新系统设置（实时生效）
func (h *Handler) UpdateSettings(c *gin.Context) {
	var req updateSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	currentAdminSecret := ""
	if dbSettings, err := h.db.GetSystemSettings(c.Request.Context()); err == nil && dbSettings != nil {
		currentAdminSecret = dbSettings.AdminSecret
	}
	if req.AdminSecret != nil {
		if h.adminSecretEnv == "" {
			currentAdminSecret = *req.AdminSecret
			log.Printf("设置已更新: admin_secret (长度=%d)", len(currentAdminSecret))
		} else {
			log.Printf("检测到环境变量 ADMIN_SECRET，忽略前端提交的 admin_secret")
		}
	}
	hasAdminSecret := strings.TrimSpace(currentAdminSecret) != "" || strings.TrimSpace(h.adminSecretEnv) != ""

	if req.MaxConcurrency != nil {
		v := *req.MaxConcurrency
		if v < 1 {
			v = 1
		}
		if v > 50 {
			v = 50
		}
		h.store.SetMaxConcurrency(v)
		log.Printf("设置已更新: max_concurrency = %d", v)
	}

	if req.GlobalRPM != nil {
		v := *req.GlobalRPM
		if v < 0 {
			v = 0
		}
		h.rateLimiter.UpdateRPM(v)
		log.Printf("设置已更新: global_rpm = %d", v)
	}

	if req.TestModel != nil && *req.TestModel != "" {
		h.store.SetTestModel(*req.TestModel)
		log.Printf("设置已更新: test_model = %s", *req.TestModel)
	}

	if req.TestConcurrency != nil {
		v := *req.TestConcurrency
		if v < 1 {
			v = 1
		}
		if v > 200 {
			v = 200
		}
		h.store.SetTestConcurrency(v)
		log.Printf("设置已更新: test_concurrency = %d", v)
	}

	if req.BackgroundRefreshIntervalMinutes != nil {
		v := *req.BackgroundRefreshIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 1440 {
			v = 1440
		}
		h.store.SetBackgroundRefreshInterval(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: background_refresh_interval_minutes = %d", v)
	}

	if req.UsageProbeMaxAgeMinutes != nil {
		v := *req.UsageProbeMaxAgeMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetUsageProbeMaxAge(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: usage_probe_max_age_minutes = %d", v)
	}

	if req.RecoveryProbeIntervalMinutes != nil {
		v := *req.RecoveryProbeIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetRecoveryProbeInterval(time.Duration(v) * time.Minute)
		log.Printf("设置已更新: recovery_probe_interval_minutes = %d", v)
	}

	if req.ProxyURL != nil {
		h.store.SetProxyURL(*req.ProxyURL)
		log.Printf("设置已更新: proxy_url = %s", *req.ProxyURL)
	}

	if req.PgMaxConns != nil {
		v := *req.PgMaxConns
		if v < 5 {
			v = 5
		}
		if v > 500 {
			v = 500
		}
		h.db.SetMaxOpenConns(v)
		h.pgMaxConns = v
		log.Printf("设置已更新: pg_max_conns = %d", v)
	}

	if req.RedisPoolSize != nil {
		v := *req.RedisPoolSize
		if v < 5 {
			v = 5
		}
		if v > 500 {
			v = 500
		}
		h.cache.SetPoolSize(v)
		h.redisPoolSize = v
		log.Printf("设置已更新: redis_pool_size = %d", v)
	}

	if req.AutoCleanUnauthorized != nil {
		h.store.SetAutoCleanUnauthorized(*req.AutoCleanUnauthorized)
		log.Printf("设置已更新: auto_clean_unauthorized = %t", *req.AutoCleanUnauthorized)
	}

	if req.AutoCleanRateLimited != nil {
		h.store.SetAutoCleanRateLimited(*req.AutoCleanRateLimited)
		log.Printf("设置已更新: auto_clean_rate_limited = %t", *req.AutoCleanRateLimited)
	}

	if req.AutoCleanFullUsage != nil {
		h.store.SetAutoCleanFullUsage(*req.AutoCleanFullUsage)
		log.Printf("设置已更新: auto_clean_full_usage = %t", *req.AutoCleanFullUsage)
	}

	if req.AutoCleanError != nil {
		h.store.SetAutoCleanError(*req.AutoCleanError)
		log.Printf("设置已更新: auto_clean_error = %t", *req.AutoCleanError)
	}

	var expiredCleaned int
	if req.AutoCleanExpired != nil {
		h.store.SetAutoCleanExpired(*req.AutoCleanExpired)
		log.Printf("设置已更新: auto_clean_expired = %t", *req.AutoCleanExpired)
		// 开启时立即同步执行一次清理
		if *req.AutoCleanExpired {
			expiredCleaned = h.store.CleanExpiredNow()
		}
	}

	if req.ProxyPoolEnabled != nil {
		h.store.SetProxyPoolEnabled(*req.ProxyPoolEnabled)
		if *req.ProxyPoolEnabled {
			_ = h.store.ReloadProxyPool()
		}
		log.Printf("设置已更新: proxy_pool_enabled = %t", *req.ProxyPoolEnabled)
	}

	if req.FastSchedulerEnabled != nil {
		h.store.SetFastSchedulerEnabled(*req.FastSchedulerEnabled)
		log.Printf("设置已更新: fast_scheduler_enabled = %t", *req.FastSchedulerEnabled)
	}

	if req.MaxRetries != nil {
		v := *req.MaxRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		h.store.SetMaxRetries(v)
		log.Printf("设置已更新: max_retries = %d", v)
	}

	if req.AllowRemoteMigration != nil {
		if *req.AllowRemoteMigration && !hasAdminSecret {
			writeError(c, http.StatusBadRequest, "请先设置管理密钥，再启用远程迁移")
			return
		}
		h.store.SetAllowRemoteMigration(*req.AllowRemoteMigration)
		log.Printf("设置已更新: allow_remote_migration = %t", *req.AllowRemoteMigration)
	} else if !hasAdminSecret {
		h.store.SetAllowRemoteMigration(false)
	}

	if req.ModelMapping != nil {
		h.store.SetModelMapping(*req.ModelMapping)
		log.Printf("设置已更新: model_mapping")
	}

	// Resin 粘性代理池配置
	resinURL := ""
	resinPlatformName := ""
	if existSettings, err := h.db.GetSystemSettings(c.Request.Context()); err == nil && existSettings != nil {
		resinURL = existSettings.ResinURL
		resinPlatformName = existSettings.ResinPlatformName
	}
	if req.ResinURL != nil {
		resinURL = *req.ResinURL
		log.Printf("设置已更新: resin_url")
	}
	if req.ResinPlatformName != nil {
		resinPlatformName = *req.ResinPlatformName
		log.Printf("设置已更新: resin_platform_name")
	}
	if req.ResinURL != nil || req.ResinPlatformName != nil {
		proxy.SetResinConfig(&proxy.ResinConfig{
			BaseURL:      resinURL,
			PlatformName: resinPlatformName,
		})
		if strings.TrimSpace(resinURL) != "" && strings.TrimSpace(resinPlatformName) != "" {
			auth.ResinRequestDecorator = func(targetURL, accountID string) string {
				return proxy.BuildReverseProxyURL(targetURL)
			}
		} else {
			auth.ResinRequestDecorator = nil
		}
	}

	// 持久化保存到数据库
	err := h.db.UpdateSystemSettings(c.Request.Context(), &database.SystemSettings{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      currentAdminSecret,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && hasAdminSecret,
		ModelMapping:                     h.store.GetModelMapping(),
		ResinURL:                         resinURL,
		ResinPlatformName:                resinPlatformName,
	})
	if err != nil {
		log.Printf("无法持久化保存设置: %v", err)
	}

	if h.store.GetAutoCleanUnauthorized() || h.store.GetAutoCleanRateLimited() || h.store.GetAutoCleanError() {
		h.store.TriggerAutoCleanupAsync()
	}

	adminSecretForDisplay := currentAdminSecret
	adminAuthSource := func() string {
		_, source := h.resolveAdminSecret(c.Request.Context())
		return source
	}()
	if adminAuthSource == "env" {
		adminSecretForDisplay = ""
	}

	c.JSON(http.StatusOK, settingsResponse{
		MaxConcurrency:                   h.store.GetMaxConcurrency(),
		GlobalRPM:                        h.rateLimiter.GetRPM(),
		TestModel:                        h.store.GetTestModel(),
		TestConcurrency:                  h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes: h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:          h.store.GetUsageProbeMaxAgeMinutes(),
		RecoveryProbeIntervalMinutes:     h.store.GetRecoveryProbeIntervalMinutes(),
		ProxyURL:                         h.store.GetProxyURL(),
		PgMaxConns:                       h.pgMaxConns,
		RedisPoolSize:                    h.redisPoolSize,
		AutoCleanUnauthorized:            h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:             h.store.GetAutoCleanRateLimited(),
		AdminSecret:                      adminSecretForDisplay,
		AdminAuthSource:                  adminAuthSource,
		AutoCleanFullUsage:               h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                   h.store.GetAutoCleanError(),
		AutoCleanExpired:                 h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                 h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:             h.store.FastSchedulerEnabled(),
		MaxRetries:                       h.store.GetMaxRetries(),
		AllowRemoteMigration:             h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                   h.databaseDriver,
		DatabaseLabel:                    h.databaseLabel,
		CacheDriver:                      h.cacheDriver,
		CacheLabel:                       h.cacheLabel,
		ExpiredCleaned:                   expiredCleaned,
		ModelMapping:                     h.store.GetModelMapping(),
		ResinURL:                         resinURL,
		ResinPlatformName:                resinPlatformName,
	})
}

// ==================== 导出 & 迁移 ====================

type cpaExportEntry struct {
	Type         string `json:"type"`
	Email        string `json:"email"`
	Expired      string `json:"expired"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	AccessToken  string `json:"access_token"`
	LastRefresh  string `json:"last_refresh"`
	RefreshToken string `json:"refresh_token"`
}

type accountAuthJSONTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type accountAuthJSON struct {
	AuthMode     string                `json:"auth_mode"`
	OpenAIAPIKey *string               `json:"OPENAI_API_KEY"`
	Tokens       accountAuthJSONTokens `json:"tokens"`
	LastRefresh  string                `json:"last_refresh"`
}

// GetAccountAuthJSON 生成单账号可用于 Codex CLI 的 auth.json。
func (h *Handler) GetAccountAuthJSON(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}

	refreshToken := row.GetCredential("refresh_token")
	accessToken := row.GetCredential("access_token")
	idToken := row.GetCredential("id_token")
	accountID := row.GetCredential("account_id")
	if refreshToken == "" {
		writeError(c, http.StatusBadRequest, "该账号没有 refresh_token，无法生成 auth.json")
		return
	}
	if accessToken == "" || idToken == "" {
		writeError(c, http.StatusBadRequest, "账号缺少 access_token 或 id_token，请先刷新账号后再生成 auth.json")
		return
	}
	if accountID == "" {
		if info := auth.ParseIDToken(idToken); info != nil {
			accountID = info.ChatGPTAccountID
		}
	}
	if accountID == "" {
		if info := auth.ParseAccessToken(accessToken); info != nil {
			accountID = info.ChatGPTAccountID
		}
	}
	if accountID == "" {
		writeError(c, http.StatusBadRequest, "账号缺少 account_id，请先刷新账号后再生成 auth.json")
		return
	}

	c.Header("Content-Disposition", `attachment; filename="auth.json"`)
	c.JSON(http.StatusOK, accountAuthJSON{
		AuthMode:     "chatgpt",
		OpenAIAPIKey: nil,
		Tokens: accountAuthJSONTokens{
			IDToken:      idToken,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			AccountID:    accountID,
		},
		LastRefresh: row.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// ExportAccounts 导出账号（CPA JSON 格式）
func (h *Handler) ExportAccounts(c *gin.Context) {
	filter := c.DefaultQuery("filter", "healthy")
	idsParam := c.Query("ids")
	remote := c.Query("remote")

	// 远程调用需检查 allow_remote_migration
	if remote == "true" {
		if !h.hasConfiguredAdminSecret(c.Request.Context()) {
			writeError(c, http.StatusForbidden, "请先设置管理密钥，再启用远程迁移")
			return
		}
		if !h.store.GetAllowRemoteMigration() {
			writeError(c, http.StatusForbidden, "远程迁移未启用，请在系统设置中开启")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.db.ListActive(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}

	// 按指定 ID 过滤
	var idSet map[int64]bool
	if idsParam != "" {
		idSet = make(map[int64]bool)
		for _, s := range strings.Split(idsParam, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				idSet[id] = true
			}
		}
	}

	// 构建运行时状态映射（用于健康过滤）
	runtimeMap := make(map[int64]*auth.Account)
	if filter == "healthy" {
		for _, acc := range h.store.Accounts() {
			runtimeMap[acc.DBID] = acc
		}
	}

	var entries []cpaExportEntry
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			acc, ok := runtimeMap[row.ID]
			if !ok || acc.RuntimeStatus() != "active" {
				continue
			}
		}
		rt := row.GetCredential("refresh_token")
		if rt == "" {
			continue
		}
		entries = append(entries, cpaExportEntry{
			Type:         "codex",
			Email:        row.GetCredential("email"),
			Expired:      row.GetCredential("expires_at"),
			IDToken:      row.GetCredential("id_token"),
			AccountID:    row.GetCredential("account_id"),
			AccessToken:  row.GetCredential("access_token"),
			LastRefresh:  row.UpdatedAt.Format(time.RFC3339),
			RefreshToken: rt,
		})
	}

	if entries == nil {
		entries = []cpaExportEntry{}
	}
	c.JSON(http.StatusOK, entries)
}

type migrateReq struct {
	URL      string `json:"url"`
	AdminKey string `json:"admin_key"`
}

// MigrateAccounts 从远程 codex2api 实例迁移健康账号（SSE 流式进度）
func (h *Handler) MigrateAccounts(c *gin.Context) {
	if !h.hasConfiguredAdminSecret(c.Request.Context()) {
		writeError(c, http.StatusForbidden, "请先设置管理密钥，再使用远程迁移")
		return
	}

	var req migrateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.URL == "" || req.AdminKey == "" {
		writeError(c, http.StatusBadRequest, "url 和 admin_key 是必填字段")
		return
	}

	remoteURL := strings.TrimRight(req.URL, "/")
	exportURL := remoteURL + "/api/admin/accounts/export?filter=healthy&remote=true"

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer fetchCancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, exportURL, nil)
	if err != nil {
		writeError(c, http.StatusBadRequest, "构建请求失败: "+err.Error())
		return
	}
	httpReq.Header.Set("X-Admin-Key", req.AdminKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(httpReq)
	if err != nil {
		writeError(c, http.StatusBadGateway, "连接远程实例失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeError(c, http.StatusBadGateway, fmt.Sprintf("远程实例返回错误 (%d): %s", resp.StatusCode, string(body)))
		return
	}

	var remoteAccounts []cpaExportEntry
	if err := json.NewDecoder(resp.Body).Decode(&remoteAccounts); err != nil {
		writeError(c, http.StatusBadGateway, "解析远程数据失败: "+err.Error())
		return
	}

	if len(remoteAccounts) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "远程实例没有可迁移的健康账号", "total": 0, "imported": 0, "duplicate": 0, "failed": 0})
		return
	}

	// 转换为 importToken 格式，复用 importAccountsCommon
	var tokens []importToken
	for _, entry := range remoteAccounts {
		rt := strings.TrimSpace(entry.RefreshToken)
		if rt == "" {
			continue
		}
		name := entry.Email
		if name == "" {
			name = "migrate"
		}
		tokens = append(tokens, importToken{
			refreshToken: rt,
			accessToken:  strings.TrimSpace(entry.AccessToken),
			name:         name,
			email:        strings.TrimSpace(entry.Email),
			idToken:      strings.TrimSpace(entry.IDToken),
			accountID:    strings.TrimSpace(entry.AccountID),
			expiresAt:    strings.TrimSpace(entry.Expired),
		})
	}

	log.Printf("远程迁移: 从 %s 拉取到 %d 个账号，开始导入", remoteURL, len(tokens))
	h.importAccountsCommon(c, tokens, "")
}

// ==================== Models ====================

// ListModels 返回支持的模型列表（供前端设置页使用）
func (h *Handler) ListModels(c *gin.Context) {
	catalog, _ := proxy.ListModelCatalog(c.Request.Context(), h.db)
	c.JSON(http.StatusOK, catalog)
}

// SyncModels 从官方 Codex 模型页同步模型注册表。
func (h *Handler) SyncModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	result, err := proxy.SyncOfficialCodexModels(ctx, h.db)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// ==================== 账号趋势 ====================

// GetAccountEventTrend 获取账号增删趋势聚合数据
func (h *Handler) GetAccountEventTrend(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	if startStr == "" || endStr == "" {
		writeError(c, http.StatusBadRequest, "start 和 end 参数为必填")
		return
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "start 时间格式无效（需 RFC3339）")
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "end 时间格式无效（需 RFC3339）")
		return
	}

	bucketMinutes := 60
	if bStr := c.Query("bucket_minutes"); bStr != "" {
		if b, err := strconv.Atoi(bStr); err == nil && b > 0 {
			bucketMinutes = b
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	trend, err := h.db.GetAccountEventTrend(ctx, start, end, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"trend": trend})
}

// ==================== 清理 ====================

// CleanBanned 清理封禁（unauthorized）账号
func (h *Handler) CleanBanned(c *gin.Context) {
	h.cleanByStatus(c, "unauthorized")
}

// CleanRateLimited 清理限流（rate_limited）账号
func (h *Handler) CleanRateLimited(c *gin.Context) {
	h.cleanByStatus(c, "rate_limited")
}

// CleanError 清理错误（error）账号
func (h *Handler) CleanError(c *gin.Context) {
	h.cleanByStatus(c, "error")
}

// cleanByStatus 按运行时状态清理账号
func (h *Handler) cleanByStatus(c *gin.Context, targetStatus string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	cleaned := h.store.CleanByRuntimeStatus(ctx, targetStatus)

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已清理 %d 个账号", cleaned), "cleaned": cleaned})
}

// ==================== Proxies ====================

// ListProxies 获取代理列表
func (h *Handler) ListProxies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	proxies, err := h.db.ListProxies(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "获取代理列表失败")
		return
	}
	if proxies == nil {
		proxies = []*database.ProxyRow{}
	}
	c.JSON(http.StatusOK, gin.H{"proxies": proxies})
}

// AddProxies 添加代理（支持批量）
func (h *Handler) AddProxies(c *gin.Context) {
	var req struct {
		URLs  []string `json:"urls"`
		URL   string   `json:"url"`
		Label string   `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 合并单条和批量
	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		writeError(c, http.StatusBadRequest, "请提供至少一个代理 URL")
		return
	}

	// 过滤空行
	cleaned := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" {
			cleaned = append(cleaned, u)
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	inserted, err := h.db.InsertProxies(ctx, cleaned, req.Label)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "添加代理失败")
		return
	}

	// 刷新代理池
	_ = h.store.ReloadProxyPool()

	c.JSON(http.StatusOK, gin.H{
		"message":  fmt.Sprintf("成功添加 %d 个代理", inserted),
		"inserted": inserted,
		"total":    len(cleaned),
	})
}

// DeleteProxy 删除单个代理
func (h *Handler) DeleteProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的代理 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.DeleteProxy(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除代理失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": "代理已删除"})
}

// UpdateProxy 更新代理（启用/禁用/改标签）
func (h *Handler) UpdateProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的代理 ID")
		return
	}

	var req struct {
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.UpdateProxy(ctx, id, req.Label, req.Enabled); err != nil {
		writeError(c, http.StatusInternalServerError, "更新代理失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": "代理已更新"})
}

// BatchDeleteProxies 批量删除代理
func (h *Handler) BatchDeleteProxies(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要删除的代理 ID 列表")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	deleted, err := h.db.DeleteProxies(ctx, req.IDs)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "批量删除失败")
		return
	}

	_ = h.store.ReloadProxyPool()
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已删除 %d 个代理", deleted), "deleted": deleted})
}

// TestProxy 测试代理连通性与出口 IP 位置
func (h *Handler) TestProxy(c *gin.Context) {
	var req struct {
		URL  string `json:"url"`
		ID   int64  `json:"id"`
		Lang string `json:"lang"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.URL == "" {
		writeError(c, http.StatusBadRequest, "请提供代理 URL")
		return
	}

	// 创建使用指定代理的 HTTP client
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, req.URL, baseDialer); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": fmt.Sprintf("代理 URL 格式错误: %v", err)})
		return
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}

	apiLang := req.Lang
	if apiLang == "" {
		apiLang = "en"
	}
	start := time.Now()
	resp, err := client.Get(fmt.Sprintf("http://ip-api.com/json/?lang=%s&fields=status,message,country,regionName,city,isp,query", apiLang))
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": fmt.Sprintf("连接失败: %v", err), "latency_ms": latencyMs})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := gjson.ParseBytes(body)

	if result.Get("status").String() != "success" {
		c.JSON(http.StatusOK, gin.H{"success": false, "error": result.Get("message").String(), "latency_ms": latencyMs})
		return
	}

	ip := result.Get("query").String()
	country := result.Get("country").String()
	region := result.Get("regionName").String()
	city := result.Get("city").String()
	isp := result.Get("isp").String()
	location := country + "·" + region + "·" + city

	// 持久化测试结果
	if req.ID > 0 {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		_ = h.db.UpdateProxyTestResult(ctx, req.ID, ip, location, latencyMs)
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"ip":         ip,
		"country":    country,
		"region":     region,
		"city":       city,
		"isp":        isp,
		"latency_ms": latencyMs,
		"location":   location,
	})
}

// imageQuotaInfo 图片配额信息
type imageQuotaInfo struct {
	Remaining int       `json:"remaining"`
	Total     int       `json:"total"`
	ResetAt   time.Time `json:"reset_at"`
}

// fetchImageQuota 获取账号的图片配额信息
// 调用 /backend-api/conversation/init 端点获取图片配额数据
func (h *Handler) fetchImageQuota(ctx context.Context, accessToken, proxyURL string) (*imageQuotaInfo, error) {
	reqBody := []byte(`{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":-480,"system_hints":["picture_v2"]}`)

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/conversation/init", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}

	// 设置请求头（参考 gpt2api 的实现）
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")

	// 创建 HTTP 客户端（支持代理）
	client := &http.Client{Timeout: 15 * time.Second}
	if proxyURL != "" {
		transport, err := h.createProxyTransport(proxyURL)
		if err == nil {
			client.Transport = transport
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("conversation/init http=%d body=%s", resp.StatusCode, string(body))
	}

	data, _ := io.ReadAll(resp.Body)

	// 解析响应
	var payload struct {
		LimitsProgress []struct {
			FeatureName string `json:"feature_name"`
			Remaining   *int   `json:"remaining"`
			ResetAfter  string `json:"reset_after"`
			MaxValue    *int   `json:"max_value"`
			Cap         *int   `json:"cap"`
			Total       *int   `json:"total"`
			Limit       *int   `json:"limit"`
		} `json:"limits_progress"`
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}

	info := &imageQuotaInfo{
		Remaining: -1,
		Total:     -1,
	}

	// 查找 image_gen 相关的配额信息
	for _, item := range payload.LimitsProgress {
		featureName := strings.ToLower(item.FeatureName)
		if !strings.Contains(featureName, "image") {
			continue
		}

		if item.Remaining != nil {
			if info.Remaining < 0 || *item.Remaining < info.Remaining {
				info.Remaining = *item.Remaining
			}
		}

		// 获取 total（优先级：MaxValue > Cap > Total > Limit）
		if item.MaxValue != nil && *item.MaxValue > info.Total {
			info.Total = *item.MaxValue
		} else if item.Cap != nil && *item.Cap > info.Total {
			info.Total = *item.Cap
		} else if item.Total != nil && *item.Total > info.Total {
			info.Total = *item.Total
		} else if item.Limit != nil && *item.Limit > info.Total {
			info.Total = *item.Limit
		}

		// 解析重置时间
		if item.ResetAfter != "" {
			if t, e := time.Parse(time.RFC3339, item.ResetAfter); e == nil {
				if info.ResetAt.IsZero() || t.Before(info.ResetAt) {
					info.ResetAt = t
				}
			}
		}
	}

	// 如果没有找到配额信息，返回 nil
	if info.Remaining < 0 {
		return nil, nil
	}

	return info, nil
}
