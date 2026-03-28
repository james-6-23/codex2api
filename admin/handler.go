package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
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

	// 图表聚合内存缓存（10秒 TTL）
	chartCacheMu   sync.RWMutex
	chartCacheData map[string]*chartCacheEntry
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
		chartCacheData: make(map[string]*chartCacheEntry),
	}
	handler.refreshAccount = handler.refreshSingleAccount
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
	api.POST("/accounts/import", h.ImportAccounts)
	api.DELETE("/accounts/:id", h.DeleteAccount)
	api.POST("/accounts/:id/refresh", h.RefreshAccount)
	api.GET("/accounts/:id/test", h.TestConnection)
	api.GET("/accounts/:id/usage", h.GetAccountUsage)
	api.POST("/accounts/batch-test", h.BatchTest)
	api.POST("/accounts/clean-banned", h.CleanBanned)
	api.POST("/accounts/clean-error", h.CleanError)
	api.POST("/accounts/clean-rate-limited", h.CleanRateLimited)
	api.GET("/accounts/export", h.ExportAccounts)
	api.POST("/accounts/migrate", h.MigrateAccounts)
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
	api.GET("/proxies", h.ListProxies)
	api.POST("/proxies", h.AddProxies)
	api.DELETE("/proxies/:id", h.DeleteProxy)
	api.PATCH("/proxies/:id", h.UpdateProxy)
	api.POST("/proxies/batch-delete", h.BatchDeleteProxies)
	api.POST("/proxies/test", h.TestProxy)

	// OAuth 授权流程
	api.POST("/oauth/generate-auth-url", h.GenerateOAuthURL)
	api.POST("/oauth/exchange-code", h.ExchangeOAuthCode)
}

// adminAuthMiddleware 管理接口鉴权中间件
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		adminSecret, _ := h.resolveAdminSecret(c.Request.Context())
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

		if adminKey != adminSecret {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "管理密钥无效或缺失",
			})
			c.Abort()
			return
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
	ID                 int64                      `json:"id"`
	Name               string                     `json:"name"`
	Email              string                     `json:"email"`
	PlanType           string                     `json:"plan_type"`
	Status             string                     `json:"status"`
	HealthTier         string                     `json:"health_tier"`
	SchedulerScore     float64                    `json:"scheduler_score"`
	ConcurrencyCap     int64                      `json:"dynamic_concurrency_limit"`
	ProxyURL           string                     `json:"proxy_url"`
	CreatedAt          string                     `json:"created_at"`
	UpdatedAt          string                     `json:"updated_at"`
	ActiveRequests     int64                      `json:"active_requests"`
	TotalRequests      int64                      `json:"total_requests"`
	LastUsedAt         string                     `json:"last_used_at"`
	SuccessRequests    int64                      `json:"success_requests"`
	ErrorRequests      int64                      `json:"error_requests"`
	UsagePercent7d     *float64                   `json:"usage_percent_7d"`
	UsagePercent5h     *float64                   `json:"usage_percent_5h"`
	Reset5hAt          string                     `json:"reset_5h_at,omitempty"`
	Reset7dAt          string                     `json:"reset_7d_at,omitempty"`
	ScoreBreakdown     schedulerBreakdownResponse `json:"scheduler_breakdown"`
	LastUnauthorizedAt string                     `json:"last_unauthorized_at,omitempty"`
	LastRateLimitedAt  string                     `json:"last_rate_limited_at,omitempty"`
	LastTimeoutAt      string                     `json:"last_timeout_at,omitempty"`
	LastServerErrorAt  string                     `json:"last_server_error_at,omitempty"`
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

	// 获取每账号的请求统计
	reqCounts, _ := h.db.GetAccountRequestCounts(ctx)

	accounts := make([]accountResponse, 0, len(rows))
	for _, row := range rows {
		resp := accountResponse{
			ID:        row.ID,
			Name:      row.Name,
			Email:     row.GetCredential("email"),
			PlanType:  row.GetCredential("plan_type"),
			Status:    row.Status,
			ProxyURL:  row.ProxyURL,
			CreatedAt: row.CreatedAt.Format(time.RFC3339),
			UpdatedAt: row.UpdatedAt.Format(time.RFC3339),
		}
		if acc, ok := accountMap[row.ID]; ok {
			resp.ActiveRequests = acc.GetActiveRequests()
			resp.TotalRequests = acc.GetTotalRequests()
			debug := acc.GetSchedulerDebugSnapshot(int64(h.store.GetMaxConcurrency()))
			resp.HealthTier = debug.HealthTier
			resp.SchedulerScore = debug.SchedulerScore
			resp.ConcurrencyCap = debug.DynamicConcurrencyLimit
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
		if rc, ok := reqCounts[row.ID]; ok {
			resp.SuccessRequests = rc.SuccessCount
			resp.ErrorRequests = rc.ErrorCount
		}
		accounts = append(accounts, resp)
	}

	c.JSON(http.StatusOK, accountsResponse{Accounts: accounts})
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

	if req.RefreshToken == "" {
		writeError(c, http.StatusBadRequest, "refresh_token 是必填字段")
		return
	}

	// 按行分割，支持批量添加
	lines := strings.Split(req.RefreshToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Refresh Token")
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

// importToken 导入时的统一 token 载体
type importToken struct {
	refreshToken string
	name         string
}

// jsonAccountEntry CLIProxyAPI 凭证 JSON 条目
type jsonAccountEntry struct {
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
}

// ImportAccounts 批量导入账号（支持 TXT / JSON）
func (h *Handler) ImportAccounts(c *gin.Context) {
	format := c.DefaultPostForm("format", "txt")
	proxyURL := c.PostForm("proxy_url")

	switch format {
	case "json":
		h.importAccountsJSON(c, proxyURL)
	default:
		h.importAccountsTXT(c, proxyURL)
	}
}

// importAccountsTXT 通过 TXT 文件导入（每行一个 RT）
func (h *Handler) importAccountsTXT(c *gin.Context, proxyURL string) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "请上传文件（字段名: file）")
		return
	}
	defer file.Close()

	if header.Size > 2*1024*1024 {
		writeError(c, http.StatusBadRequest, "文件大小不能超过 2MB")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取文件失败")
		return
	}

	// 按行分割，去重
	lines := strings.Split(string(data), "\n")
	seen := make(map[string]bool)
	var tokens []importToken
	for _, line := range lines {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "\xef\xbb\xbf") // 去除 UTF-8 BOM
		if t != "" && !seen[t] {
			seen[t] = true
			tokens = append(tokens, importToken{refreshToken: t})
		}
	}

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

		// 去除 UTF-8 BOM
		data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))

		// 尝试解析为数组，失败则尝试单对象
		var entries []jsonAccountEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			var single jsonAccountEntry
			if err := json.Unmarshal(data, &single); err != nil {
				writeError(c, http.StatusBadRequest, fmt.Sprintf("文件 %s 不是有效的 JSON 格式", fh.Filename))
				return
			}
			entries = []jsonAccountEntry{single}
		}

		for _, entry := range entries {
			rt := strings.TrimSpace(entry.RefreshToken)
			if rt == "" {
				continue
			}
			allTokens = append(allTokens, importToken{
				refreshToken: rt,
				name:         strings.TrimSpace(entry.Email),
			})
		}
	}

	if len(allTokens) == 0 {
		writeError(c, http.StatusBadRequest, "JSON 文件中未找到有效的 refresh_token")
		return
	}

	h.importAccountsCommon(c, allTokens, proxyURL)
}

// importEvent SSE 导入进度事件
type importEvent struct {
	Type      string `json:"type"`                // progress | complete
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

// importAccountsCommon 公共的去重、并发插入、SSE 进度推送逻辑
func (h *Handler) importAccountsCommon(c *gin.Context, tokens []importToken, proxyURL string) {
	// 文件内去重
	seen := make(map[string]bool)
	var unique []importToken
	for _, t := range tokens {
		if !seen[t.refreshToken] {
			seen[t.refreshToken] = true
			unique = append(unique, t)
		}
	}

	// 数据库去重（独立短超时）
	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()
	existingRTs, err := h.db.GetAllRefreshTokens(dedupeCtx)
	if err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		existingRTs = make(map[string]bool)
	}

	var newTokens []importToken
	duplicateCount := 0
	for _, t := range unique {
		if existingRTs[t.refreshToken] {
			duplicateCount++
		} else {
			newTokens = append(newTokens, t)
		}
	}

	total := len(unique)

	if len(newTokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个 RT 已存在，无需导入", total),
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

			newAcc := &auth.Account{
				DBID:         id,
				RefreshToken: tok.refreshToken,
				ProxyURL:     proxyURL,
			}
			h.store.AddAccount(newAcc)

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

	// 标记为 deleted 而非物理删除
	if err := h.db.SetError(ctx, id, "deleted"); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}

	// 从内存池移除
	h.store.RemoveAccount(id)

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

			filter := database.UsageLogFilter{
				Start:    startTime,
				End:      endTime,
				Page:     page,
				PageSize: pageSize,
				Email:    c.Query("email"),
				Model:    c.Query("model"),
				Endpoint: c.Query("endpoint"),
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

// ListAPIKeys 获取所有 API 密钥
func (h *Handler) ListAPIKeys(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if keys == nil {
		keys = []*database.APIKeyRow{}
	}
	c.JSON(http.StatusOK, apiKeysResponse{Keys: keys})
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

// CreateAPIKey 创建新 API 密钥
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Name = ""
	}
	if req.Name == "" {
		req.Name = "default"
	}

	key := req.Key
	if key == "" {
		key = generateKey()
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	id, err := h.db.InsertAPIKey(ctx, req.Name, key)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}

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
	MaxConcurrency        int    `json:"max_concurrency"`
	GlobalRPM             int    `json:"global_rpm"`
	TestModel             string `json:"test_model"`
	TestConcurrency       int    `json:"test_concurrency"`
	ProxyURL              string `json:"proxy_url"`
	PgMaxConns            int    `json:"pg_max_conns"`
	RedisPoolSize         int    `json:"redis_pool_size"`
	AutoCleanUnauthorized bool   `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited  bool   `json:"auto_clean_rate_limited"`
	AdminSecret           string `json:"admin_secret"`
	AdminAuthSource       string `json:"admin_auth_source"`
	AutoCleanFullUsage    bool   `json:"auto_clean_full_usage"`
	ProxyPoolEnabled      bool   `json:"proxy_pool_enabled"`
	FastSchedulerEnabled  bool   `json:"fast_scheduler_enabled"`
	MaxRetries            int    `json:"max_retries"`
	AllowRemoteMigration  bool   `json:"allow_remote_migration"`
	DatabaseDriver        string `json:"database_driver"`
	DatabaseLabel         string `json:"database_label"`
	CacheDriver           string `json:"cache_driver"`
	CacheLabel            string `json:"cache_label"`
}

type updateSettingsReq struct {
	MaxConcurrency        *int    `json:"max_concurrency"`
	GlobalRPM             *int    `json:"global_rpm"`
	TestModel             *string `json:"test_model"`
	TestConcurrency       *int    `json:"test_concurrency"`
	ProxyURL              *string `json:"proxy_url"`
	PgMaxConns            *int    `json:"pg_max_conns"`
	RedisPoolSize         *int    `json:"redis_pool_size"`
	AutoCleanUnauthorized *bool   `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited  *bool   `json:"auto_clean_rate_limited"`
	AdminSecret           *string `json:"admin_secret"`
	AutoCleanFullUsage    *bool   `json:"auto_clean_full_usage"`
	ProxyPoolEnabled      *bool   `json:"proxy_pool_enabled"`
	FastSchedulerEnabled  *bool   `json:"fast_scheduler_enabled"`
	MaxRetries            *int    `json:"max_retries"`
	AllowRemoteMigration  *bool   `json:"allow_remote_migration"`
}

// GetSettings 获取当前系统设置
func (h *Handler) GetSettings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	dbSettings, _ := h.db.GetSystemSettings(ctx)
	_, adminAuthSource := h.resolveAdminSecret(c.Request.Context())
	adminSecret := ""
	if dbSettings != nil && adminAuthSource != "env" {
		adminSecret = dbSettings.AdminSecret
	}
	c.JSON(http.StatusOK, settingsResponse{
		MaxConcurrency:        h.store.GetMaxConcurrency(),
		GlobalRPM:             h.rateLimiter.GetRPM(),
		TestModel:             h.store.GetTestModel(),
		TestConcurrency:       h.store.GetTestConcurrency(),
		ProxyURL:              h.store.GetProxyURL(),
		PgMaxConns:            h.pgMaxConns,
		RedisPoolSize:         h.redisPoolSize,
		AutoCleanUnauthorized: h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:  h.store.GetAutoCleanRateLimited(),
		AdminSecret:           adminSecret,
		AdminAuthSource:       adminAuthSource,
		AutoCleanFullUsage:    h.store.GetAutoCleanFullUsage(),
		ProxyPoolEnabled:      h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:  h.store.FastSchedulerEnabled(),
		MaxRetries:            h.store.GetMaxRetries(),
		AllowRemoteMigration:  h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:        h.databaseDriver,
		DatabaseLabel:         h.databaseLabel,
		CacheDriver:           h.cacheDriver,
		CacheLabel:            h.cacheLabel,
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

	// 持久化保存到数据库
	err := h.db.UpdateSystemSettings(c.Request.Context(), &database.SystemSettings{
		MaxConcurrency:        h.store.GetMaxConcurrency(),
		GlobalRPM:             h.rateLimiter.GetRPM(),
		TestModel:             h.store.GetTestModel(),
		TestConcurrency:       h.store.GetTestConcurrency(),
		ProxyURL:              h.store.GetProxyURL(),
		PgMaxConns:            h.pgMaxConns,
		RedisPoolSize:         h.redisPoolSize,
		AutoCleanUnauthorized: h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:  h.store.GetAutoCleanRateLimited(),
		AdminSecret:           currentAdminSecret,
		AutoCleanFullUsage:    h.store.GetAutoCleanFullUsage(),
		ProxyPoolEnabled:      h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:  h.store.FastSchedulerEnabled(),
		MaxRetries:            h.store.GetMaxRetries(),
		AllowRemoteMigration:  h.store.GetAllowRemoteMigration() && hasAdminSecret,
	})
	if err != nil {
		log.Printf("无法持久化保存设置: %v", err)
	}

	if h.store.GetAutoCleanUnauthorized() || h.store.GetAutoCleanRateLimited() {
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
		MaxConcurrency:        h.store.GetMaxConcurrency(),
		GlobalRPM:             h.rateLimiter.GetRPM(),
		TestModel:             h.store.GetTestModel(),
		TestConcurrency:       h.store.GetTestConcurrency(),
		ProxyURL:              h.store.GetProxyURL(),
		PgMaxConns:            h.pgMaxConns,
		RedisPoolSize:         h.redisPoolSize,
		AutoCleanUnauthorized: h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:  h.store.GetAutoCleanRateLimited(),
		AdminSecret:           adminSecretForDisplay,
		AdminAuthSource:       adminAuthSource,
		AutoCleanFullUsage:    h.store.GetAutoCleanFullUsage(),
		ProxyPoolEnabled:      h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:  h.store.FastSchedulerEnabled(),
		MaxRetries:            h.store.GetMaxRetries(),
		AllowRemoteMigration:  h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:        h.databaseDriver,
		DatabaseLabel:         h.databaseLabel,
		CacheDriver:           h.cacheDriver,
		CacheLabel:            h.cacheLabel,
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
		tokens = append(tokens, importToken{refreshToken: rt, name: name})
	}

	log.Printf("远程迁移: 从 %s 拉取到 %d 个账号，开始导入", remoteURL, len(tokens))
	h.importAccountsCommon(c, tokens, "")
}

// ==================== Models ====================

// ListModels 返回支持的模型列表（供前端设置页使用）
func (h *Handler) ListModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": proxy.SupportedModels})
}

// ==================== 清理 ====================

// CleanBanned 清理封禁（unauthorized）账号
func (h *Handler) CleanBanned(c *gin.Context) {
	h.cleanByStatus(c, "unauthorized")
}

// CleanError 清理错误（error）账号
func (h *Handler) CleanError(c *gin.Context) {
	h.cleanByStatus(c, "error")
}

// CleanRateLimited 清理限流（rate_limited）账号
func (h *Handler) CleanRateLimited(c *gin.Context) {
	h.cleanByStatus(c, "rate_limited")
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

