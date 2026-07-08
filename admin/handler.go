package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Handler 管理后台 API 处理器
type Handler struct {
	store                  *auth.Store
	cache                  cache.TokenCache
	db                     *database.DB
	rateLimiter            *proxy.RateLimiter
	systemUpdate           *systemUpdater
	systemUpdateOnce       sync.Once
	refreshAccount         func(context.Context, int64) error
	probeUsage             func(context.Context, *auth.Account) error
	syncAccountPlanOnReset func(context.Context, *auth.Account) error
	cpuSampler             *cpuSampler
	startedAt              time.Time
	pgMaxConns             int
	redisPoolSize          int
	databaseDriver         string
	databaseLabel          string
	cacheDriver            string
	cacheLabel             string
	adminSecretEnv         string
	imageProxy             *proxy.Handler

	// 图表聚合内存缓存（10秒 TTL）
	chartCacheMu   sync.RWMutex
	chartCacheData map[string]*chartCacheEntry

	// 账号请求统计缓存（30秒 TTL）
	reqCountMu        sync.RWMutex
	reqCountCache     map[int64]*database.AccountRequestCount
	reqCountExpiresAt time.Time

	// 「主动重置次数」消耗操作的账号级互斥锁（dbID -> *sync.Mutex），
	// 串行化同一账号的并发重置，避免重复消耗与次数计数竞态。
	resetCreditLocks sync.Map

	// 重复账号合并互斥锁：串行化 mergeRefreshedDuplicateIntoExisting，
	// 防止并发导入同一身份的多个账号时互相合并、把双方都软删（账号丢失）。
	mergeDuplicateMu sync.Mutex
}

type chartCacheEntry struct {
	data      *database.ChartAggregation
	expiresAt time.Time
}

const (
	adminUsageStatsCacheNamespace  = "admin:usage-stats"
	adminChartCacheNamespace       = "admin:chart-data"
	adminAPIKeyCacheNamespace      = "api-key"
	adminAPIKeyCountNamespace      = "api-key-count"
	adminUsageStatsCacheTTL        = 5 * time.Second
	adminChartCacheTTL             = 10 * time.Second
	importFileSizeLimitBytes       = 20 * 1024 * 1024
	importFileSizeLimitLabel       = "20MB"
	accountRefreshBatchConcurrency = 4
)

func (h *Handler) getRuntimeJSON(ctx context.Context, namespace, key string, dest interface{}) bool {
	if h == nil || h.cache == nil || dest == nil {
		return false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, namespace, key)
	if err != nil {
		log.Printf("读取运行态缓存失败: namespace=%s err=%v", namespace, err)
		return false
	}
	if !ok || len(raw) == 0 {
		return false
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		log.Printf("解析运行态缓存失败: namespace=%s err=%v", namespace, err)
		return false
	}
	return true
}

func (h *Handler) setRuntimeJSON(ctx context.Context, namespace, key string, value interface{}, ttl time.Duration) {
	if h == nil || h.cache == nil || value == nil {
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		log.Printf("编码运行态缓存失败: namespace=%s err=%v", namespace, err)
		return
	}
	if err := h.cache.SetRuntime(ctx, namespace, key, payload, ttl); err != nil {
		log.Printf("写入运行态缓存失败: namespace=%s err=%v", namespace, err)
	}
}

func validateImportFileSize(fh *multipart.FileHeader) error {
	if fh.Size > importFileSizeLimitBytes {
		return fmt.Errorf("文件 %s 大小超过 %s", fh.Filename, importFileSizeLimitLabel)
	}
	return nil
}

func (h *Handler) usageProbeFunc() func(context.Context, *auth.Account) error {
	if h != nil && h.probeUsage != nil {
		return h.probeUsage
	}
	if h != nil {
		return h.ProbeUsageSnapshot
	}
	return nil
}

func (h *Handler) probeImportedAccountUsage(ctx context.Context, accountID int64, source string) {
	if h == nil || h.store == nil {
		return
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		return
	}
	if account.GetAccessToken() == "" {
		return
	}
	probeFn := h.usageProbeFunc()
	if probeFn == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := probeFn(probeCtx, account); err != nil {
		log.Printf("导入账号 %d 用量采样失败 (%s): %v", accountID, source, err)
		return
	}
	// AT / codex_at 账号的 OAuth 身份（email + account_id）在插入时无法从
	// JWT 解出，由上面的 wham 探针补齐并落库。身份既已可知，此刻回查是否与
	// 已有账号同一身份：若重复则把凭证合并进旧账号并软删本账号——与 RT 路径
	// refreshImportedAccountAndProbe 对称，补上 AT 导入/添加事后无法去重的缺口
	// （codex_at 原文轮换 + 存量 account_id 被 user_id 污染都会导致插入期判重失配）。
	h.mergeRefreshedDuplicateIntoExisting(accountID, source)
}

func (h *Handler) triggerImportedAccountUsageProbe(accountID int64, source string) {
	go h.probeImportedAccountUsage(context.Background(), accountID, source)
}

func (h *Handler) applyImportedAccountUsageState(account *auth.Account, source string) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if h.store.MarkUsage7dRateLimited(account) {
		log.Printf("导入账号 %d 已按 7d 用量耗尽标记限流 (%s)", account.DBID, source)
	}
}

func (h *Handler) refreshImportedAccountAndProbe(accountID int64, source string) {
	refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := h.refreshAccountByID(refreshCtx, accountID)
	cancel()
	if err != nil {
		log.Printf("导入账号 %d 刷新失败: %v", accountID, err)
		return
	}
	log.Printf("导入账号 %d 刷新成功", accountID)
	// 裸 RT 导入时身份要等首次刷新后才可知：此刻回查身份重复，
	// 若与已有账号同一身份则合并凭证并移除本账号（保留旧账号的用量统计）。
	if h.mergeRefreshedDuplicateIntoExisting(accountID, source) {
		return
	}
	h.probeImportedAccountUsage(context.Background(), accountID, source)
}

// mergeRefreshedDuplicateIntoExisting 检查刚刷新完的新导入账号是否与已有账号
// 同一 OAuth 身份。若重复，把新凭证（refresh_token 优先级最高，可自动续期）
// 合并进已有账号——codex_* 用量快照键不在更新集里，旧账号的用量统计与按
// 账号 ID 关联的请求历史全部保留——然后软删新插入的账号。返回 true 表示已合并。
func (h *Handler) mergeRefreshedDuplicateIntoExisting(newID int64, source string) bool {
	if h == nil || h.db == nil || h.store == nil {
		return false
	}
	// 串行化合并：并发导入同一身份的多个账号时，两个合并流程若交错执行，
	// 可能互相把对方选为“已有账号”，导致双方都被软删（账号丢失）。
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	newRow, err := h.db.GetAccountByID(ctx, newID)
	if err != nil || newRow == nil {
		return false
	}
	// 勾选"允许重复添加"导入的副本是用户故意保留的，不合并。
	if strings.EqualFold(strings.TrimSpace(newRow.GetCredential("allow_duplicate")), "true") {
		return false
	}
	email := strings.TrimSpace(newRow.GetCredential("email"))
	identity := strings.TrimSpace(newRow.GetCredential("account_id"))
	if identity == "" {
		identity = strings.TrimSpace(newRow.GetCredential("chatgpt_account_id"))
	}
	if identity == "" {
		identity = strings.TrimSpace(newRow.GetCredential("user_id"))
	}
	if email == "" || identity == "" {
		return false
	}
	oldID, err := h.db.FindActiveAccountByOAuthIdentity(ctx, email, identity, newID)
	if err != nil || oldID <= 0 {
		return false
	}

	updates := make(map[string]interface{})
	for _, key := range []string{"refresh_token", "session_token", "access_token", "access_token_type", "id_token", "expires_at", "email", "account_id", "user_id", "plan_type", "subscription_expires_at"} {
		if v := strings.TrimSpace(newRow.GetCredential(key)); v != "" {
			updates[key] = v
		}
	}
	if len(updates) == 0 {
		return false
	}
	proxyURL := strings.TrimSpace(newRow.ProxyURL)
	if proxyURL == "" {
		if oldRow, err := h.db.GetAccountByID(ctx, oldID); err == nil && oldRow != nil {
			proxyURL = strings.TrimSpace(oldRow.ProxyURL)
		}
	}
	if err := h.db.UpdateOAuthAccountCredentials(ctx, oldID, updates, proxyURL); err != nil {
		log.Printf("合并导入账号 %d 凭证到已有账号 %d 失败: %v", newID, oldID, err)
		return false
	}
	// 先软删新账号、再重载旧账号：reloadTokenAccount 会异步触发旧账号的
	// 探针→再合并，若此刻新账号仍活跃，反向查重会把旧账号合并进新账号，
	// 两边都被软删。软删前置让后续任何查重都看不到新账号。
	if err := h.db.SoftDeleteAccount(ctx, newID); err != nil {
		log.Printf("软删重复导入账号 %d 失败: %v", newID, err)
	}
	h.store.RemoveAccount(newID)
	if err := h.reloadTokenAccount(ctx, oldID, source); err != nil {
		log.Printf("合并后重载账号 %d 失败: %v", oldID, err)
	}
	h.db.InsertAccountEventAsync(newID, "deleted", fmt.Sprintf("merged_into_%d", oldID))
	h.db.InsertAccountEventAsync(oldID, "updated", "rt_upgrade_merge")
	log.Printf("导入账号 %d 与已有账号 %d 同一 OAuth 身份，已合并凭证（RT 升级）并保留用量统计 (source=%s)", newID, oldID, source)
	return true
}

func (h *Handler) deleteRuntimeCache(ctx context.Context, namespace, key string) {
	if h == nil || h.cache == nil {
		return
	}
	if err := h.cache.DeleteRuntime(ctx, namespace, key); err != nil {
		log.Printf("删除运行态缓存失败: namespace=%s err=%v", namespace, err)
	}
}

func (h *Handler) invalidateAPIKeyRuntimeCaches(ctx context.Context, apiKey string) {
	h.deleteRuntimeCache(ctx, adminAPIKeyCountNamespace, "all")
	if strings.TrimSpace(apiKey) != "" {
		h.deleteRuntimeCache(ctx, adminAPIKeyCacheNamespace, apiKey)
	}
}

func (h *Handler) getUsageStatsCached(ctx context.Context, rangeStart, rangeEnd time.Time) (*database.UsageStats, error) {
	// 只对"默认今日"区间走 5 秒缓存。
	// 带显式区间的请求种类多、命中率低,且 ClearUsageLogs 现有的失效逻辑只清 "global" key,
	// 给区间结果做缓存反而需要扩展失效接口,得不偿失,直接每次重算更简单。
	useCache := rangeStart.IsZero() && rangeEnd.IsZero()
	if useCache {
		var cached database.UsageStats
		if h.getRuntimeJSON(ctx, adminUsageStatsCacheNamespace, "global", &cached) {
			return &cached, nil
		}
	}
	stats, err := h.db.GetUsageStats(ctx, rangeStart, rangeEnd)
	if err != nil {
		return nil, err
	}
	if useCache {
		h.setRuntimeJSON(ctx, adminUsageStatsCacheNamespace, "global", stats, adminUsageStatsCacheTTL)
	}
	return stats, nil
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
	if handler.imageProxy != nil {
		handler.imageProxy.SetRuntimeCache(tc)
	}
	handler.refreshAccount = handler.refreshSingleAccount
	handler.probeUsage = handler.ProbeUsageSnapshot
	handler.syncAccountPlanOnReset = handler.syncSingleAccountPlanOnReset
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
	r.GET("/p/img/:id", h.GetSignedImageAssetFile)
	r.GET("/p/backgrounds/:filename", h.GetBackgroundAssetFile)
	r.HEAD("/p/backgrounds/:filename", h.GetBackgroundAssetFile)
	r.GET("/api/branding", h.GetBranding)
	keyUsage := r.Group("/api/key-usage")
	keyUsage.GET("/summary", h.GetPublicAPIKeyUsageSummary)
	keyUsage.GET("/me", h.GetPublicAPIKeyUsageSummary)

	// 首次初始化端点（无需鉴权，仅在系统未配置 ADMIN_SECRET 时可用）
	// 这两个端点必须注册在 adminAuthMiddleware 之外，否则会被 fail-closed 拦截。
	r.GET("/api/admin/bootstrap-status", h.GetBootstrapStatus)
	r.POST("/api/admin/bootstrap", h.PostBootstrap)

	api := r.Group("/api/admin")
	api.Use(h.adminAuthMiddleware())
	api.GET("/stats", h.GetStats)
	api.GET("/accounts", h.ListAccounts)
	api.POST("/accounts", h.AddAccount)
	api.POST("/accounts/at", h.AddATAccount)
	api.POST("/accounts/openai-responses", h.AddOpenAIResponsesAccount)
	api.POST("/accounts/openai-responses/models", h.FetchOpenAIResponsesModels)
	api.PATCH("/accounts/:id/openai-responses", h.UpdateOpenAIResponsesAccount)
	api.POST("/accounts/:id/oauth/exchange-code", h.UpdateOAuthAccountCode)
	api.POST("/accounts/import", h.ImportAccounts)
	api.POST("/accounts/sub2api/preview", h.PreviewSub2APIAccounts)
	api.POST("/accounts/sub2api/import", h.ImportFromSub2API)
	api.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
	api.DELETE("/accounts/:id", h.DeleteAccount)
	api.GET("/accounts/health-bars", h.GetAccountHealthBars)
	api.GET("/accounts/recycle-bin", h.ListRecycleBinAccounts)
	api.DELETE("/accounts/recycle-bin", h.EmptyRecycleBin)
	api.POST("/accounts/recycle-bin/batch-test", h.RecycleBinBatchTest)
	api.POST("/accounts/:id/restore", h.RestoreAccount)
	api.DELETE("/accounts/:id/purge", h.PurgeAccount)
	api.POST("/accounts/:id/refresh", h.RefreshAccount)
	api.POST("/accounts/:id/enable", h.ToggleAccountEnabled)
	api.POST("/accounts/:id/lock", h.ToggleAccountLock)
	api.POST("/accounts/:id/reset-status", h.ResetAccountStatus)
	api.POST("/accounts/:id/reset-credits", h.ResetCredits)
	api.GET("/accounts/:id/reset-credits", h.GetResetCredits)
	api.POST("/accounts/:id/invite", h.SendInvite)
	api.GET("/accounts/:id/test", h.TestConnection)
	api.GET("/accounts/:id/usage", h.GetAccountUsage)
	api.POST("/accounts/:id/usage/refresh", h.RefreshAccountUsage)
	api.GET("/accounts/:id/auth-json", h.GetAccountAuthJSON)
	api.PATCH("/accounts/:id/credit", h.UpdateAccountCredit)
	api.POST("/accounts/batch-test", h.BatchTest)
	api.POST("/accounts/batch-refresh", h.BatchRefreshAccounts)
	api.POST("/accounts/batch-delete", h.BatchDeleteAccounts)
	api.POST("/accounts/batch-update", h.BatchUpdateAccounts)
	api.POST("/accounts/batch-reset-status", h.BatchResetStatus)
	api.POST("/accounts/clean-banned", h.CleanBanned)
	api.POST("/accounts/clean-rate-limited", h.CleanRateLimited)
	api.POST("/accounts/clean-error", h.CleanError)
	api.GET("/accounts/export", h.ExportAccounts)
	api.POST("/accounts/migrate", h.MigrateAccounts)
	api.GET("/accounts/event-trend", h.GetAccountEventTrend)
	api.POST("/accounts/usage/probe", h.ForceUsageProbe)
	api.GET("/usage/stats", h.GetUsageStats)
	api.GET("/usage/api-keys", h.GetAPIKeyTokenStats)
	api.GET("/usage/logs", h.GetUsageLogs)
	api.GET("/usage/chart-data", h.GetChartData)
	api.DELETE("/usage/logs", h.ClearUsageLogs)
	api.GET("/setup-hints", h.GetSetupHints)
	api.GET("/keys", h.ListAPIKeys)
	api.POST("/keys", h.CreateAPIKey)
	api.PATCH("/keys/:id", h.UpdateAPIKey)
	api.DELETE("/keys/:id", h.DeleteAPIKey)
	api.GET("/account-groups", h.ListAccountGroups)
	api.POST("/account-groups", h.CreateAccountGroup)
	api.PATCH("/account-groups/:id", h.UpdateAccountGroup)
	api.DELETE("/account-groups/:id", h.DeleteAccountGroup)
	api.GET("/health", h.GetHealth)
	api.GET("/runtime-status", h.GetRuntimeStatus)
	api.GET("/system/update", h.GetSystemUpdate)
	api.POST("/system/update", h.PerformSystemUpdate)
	api.GET("/ops/overview", h.GetOpsOverview)
	api.GET("/ops/runtime-status", h.GetRuntimeStatus)
	api.GET("/ops/errors", h.GetOpsErrorLogs)
	api.GET("/ops/errors/export", h.ExportOpsErrorLogs)
	api.GET("/ops/errors/summary", h.GetOpsErrorSummary)
	api.GET("/settings", h.GetSettings)
	api.PUT("/settings", h.UpdateSettings)
	api.POST("/settings/background-upload", h.UploadBackgroundAsset)
	api.POST("/settings/image-storage/test", h.TestImageStorageConnection)
	api.GET("/prompt-filter/logs", h.ListPromptFilterLogs)
	api.GET("/prompt-filter/logs/match", h.MatchPromptFilterLog)
	api.DELETE("/prompt-filter/logs", h.ClearPromptFilterLogs)
	api.POST("/prompt-filter/test", h.TestPromptFilter)
	api.POST("/prompt-filter/rules/test", h.TestPromptFilterRulePattern)
	api.GET("/prompt-filter/rules", h.GetPromptFilterRules)
	api.GET("/models", h.ListModels)
	api.POST("/models/sync", h.SyncModels)
	api.GET("/image-prompts", h.ListImagePromptTemplates)
	api.POST("/image-prompts", h.CreateImagePromptTemplate)
	api.PATCH("/image-prompts/:id", h.UpdateImagePromptTemplate)
	api.DELETE("/image-prompts/:id", h.DeleteImagePromptTemplate)
	api.POST("/images/jobs", h.CreateImageGenerationJob)
	api.POST("/images/edit-jobs", h.CreateImageEditJob)
	api.GET("/images/jobs", h.ListImageGenerationJobs)
	api.GET("/images/jobs/:id", h.GetImageGenerationJob)
	api.DELETE("/images/jobs/:id", h.DeleteImageGenerationJob)
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
//
// 安全策略（fail-closed）：
//   - 未配置 ADMIN_SECRET 时一律拒绝（503），防止 /api/admin/* 裸奔。
//   - 用户应通过前端「首次初始化」页面（无鉴权的 /api/admin/bootstrap 端点）
//     设置初始密钥，或者在 .env 中显式设置 ADMIN_SECRET 后重启。
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		adminSecret, source := h.resolveAdminSecret(c.Request.Context())
		if adminSecret == "" {
			// fail-closed：拒绝并提示用户配置 ADMIN_SECRET
			security.SecurityAuditLog("ADMIN_BLOCKED_NO_SECRET", fmt.Sprintf("path=%s ip=%s", c.Request.URL.Path, c.ClientIP()))
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "管理接口未初始化：ADMIN_SECRET 尚未配置。请在浏览器访问 /admin/ 完成首次初始化，或在 .env 中设置 ADMIN_SECRET 后重启。",
				"code":  "bootstrap_required",
			})
			c.Abort()
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

	accountCounts := summarizeDashboardAccounts(accounts, h.store.Accounts())

	usageStats, _ := h.getUsageStatsCached(ctx, time.Time{}, time.Time{})
	todayReqs := int64(0)
	if usageStats != nil {
		todayReqs = usageStats.TodayRequests
	}

	c.JSON(http.StatusOK, statsResponse{
		Total:         accountCounts.total,
		Available:     accountCounts.normal,
		RateLimited:   accountCounts.rateLimited,
		Error:         accountCounts.abnormal,
		TodayRequests: todayReqs,
	})
}

type dashboardAccountCounts struct {
	total       int
	normal      int
	rateLimited int
	abnormal    int
	disabled    int
}

func summarizeDashboardAccounts(rows []*database.AccountRow, runtimeAccounts []*auth.Account) dashboardAccountCounts {
	runtimeByID := make(map[int64]*auth.Account, len(runtimeAccounts))
	for _, acc := range runtimeAccounts {
		if acc != nil {
			runtimeByID[acc.DBID] = acc
		}
	}

	var counts dashboardAccountCounts
	counts.total = len(rows)
	for _, row := range rows {
		if row == nil {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(row.Status))
		cooldownReason := strings.ToLower(strings.TrimSpace(row.CooldownReason))
		if acc, ok := runtimeByID[row.ID]; ok {
			status = strings.ToLower(strings.TrimSpace(acc.RuntimeStatus()))
			cooldownReason = ""
		}

		if !row.Enabled {
			counts.disabled++
		}
		if isDashboardAbnormalAccount(status) {
			counts.abnormal++
			continue
		}
		if isDashboardRateLimitedAccount(status, cooldownReason) {
			counts.rateLimited++
			continue
		}
		counts.normal++
	}
	return counts
}

func isDashboardAbnormalAccount(status string) bool {
	return status == "unauthorized" || status == "error"
}

func isDashboardRateLimitedAccount(status string, cooldownReason string) bool {
	switch status {
	case "rate_limited", "usage_exhausted", "quota_paused", "rate_limited_5h", "rate_limited_7d":
		return true
	}
	switch cooldownReason {
	case "rate_limited", "rate_limited_5h", "rate_limited_7d":
		return true
	}
	return false
}

// ==================== Accounts ====================

type accountResponse struct {
	ID                       int64                      `json:"id"`
	Name                     string                     `json:"name"`
	Email                    string                     `json:"email"`
	EmailDomain              string                     `json:"email_domain,omitempty"`
	ChatGPTAccountID         string                     `json:"chatgpt_account_id,omitempty"`
	PlanType                 string                     `json:"plan_type"`
	SubscriptionExpiresAt    string                     `json:"subscription_expires_at,omitempty"`
	Status                   string                     `json:"status"`
	ErrorMessage             string                     `json:"error_message,omitempty"`
	ATOnly                   bool                       `json:"at_only"`
	CreditEnabled            bool                       `json:"credit_enabled"`
	CreditSkipUsageWindow    bool                       `json:"credit_skip_usage_window"`
	SkipWarmTier             bool                       `json:"skip_warm_tier"`
	AccountType              string                     `json:"account_type,omitempty"`
	AccessTokenType          string                     `json:"access_token_type,omitempty"`
	OpenAIResponsesAPI       bool                       `json:"openai_responses_api,omitempty"`
	BaseURL                  string                     `json:"base_url,omitempty"`
	Models                   []string                   `json:"models,omitempty"`
	ModelMapping             string                     `json:"model_mapping,omitempty"`
	CustomHeaders            map[string]string          `json:"custom_headers,omitempty"`
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
	CodexUsageUpdatedAt      string                     `json:"codex_usage_updated_at,omitempty"`
	Codex5HUsageUpdatedAt    string                     `json:"codex_5h_usage_updated_at,omitempty"`
	ActiveRequests           int64                      `json:"active_requests"`
	TotalRequests            int64                      `json:"total_requests"`
	LastUsedAt               string                     `json:"last_used_at"`
	SuccessRequests          int64                      `json:"success_requests"`
	ErrorRequests            int64                      `json:"error_requests"`
	RetryErrorRequests       int64                      `json:"retry_error_requests"`
	RateLimitAttempts        int64                      `json:"rate_limit_attempts"`
	UsagePercent7d           *float64                   `json:"usage_percent_7d"`
	UsagePercent5h           *float64                   `json:"usage_percent_5h"`
	RateLimitResetCredits    *int                       `json:"rate_limit_reset_credits"`
	AutoPause5hThreshold     *float64                   `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold     *float64                   `json:"auto_pause_7d_threshold"`
	AutoPause5hDisabled      bool                       `json:"auto_pause_5h_disabled"`
	AutoPause7dDisabled      bool                       `json:"auto_pause_7d_disabled"`
	DispatchCountLimit       *int64                     `json:"dispatch_count_limit"`
	DispatchCountUsed        int64                      `json:"dispatch_count_used,omitempty"`
	DispatchCountResetAt     string                     `json:"dispatch_count_reset_at,omitempty"`
	DispatchCountLimited     bool                       `json:"dispatch_count_limited,omitempty"`
	Usage5hDetail            *accountUsageWindow        `json:"usage_5h_detail,omitempty"`
	Usage7dDetail            *accountUsageWindow        `json:"usage_7d_detail,omitempty"`
	Reset5hAt                string                     `json:"reset_5h_at,omitempty"`
	Reset7dAt                string                     `json:"reset_7d_at,omitempty"`
	Window7dKind             string                     `json:"usage_window_7d_kind,omitempty"`    // "monthly"(team 月窗)/"weekly"/""；供前端标「30天」而非误标「7天」
	Window7dSeconds          *int64                     `json:"usage_window_7d_seconds,omitempty"` // 长窗口真实周期秒数
	Billed5h                 *float64                   `json:"billed_5h"`
	Billed7d                 *float64                   `json:"billed_7d"`
	ScoreBreakdown           schedulerBreakdownResponse `json:"scheduler_breakdown"`
	LastUnauthorizedAt       string                     `json:"last_unauthorized_at,omitempty"`
	LastRateLimitedAt        string                     `json:"last_rate_limited_at,omitempty"`
	LastTimeoutAt            string                     `json:"last_timeout_at,omitempty"`
	LastServerErrorAt        string                     `json:"last_server_error_at,omitempty"`
	CooldownReason           string                     `json:"cooldown_reason,omitempty"`
	CooldownUntil            string                     `json:"cooldown_until,omitempty"`
	ModelCooldowns           []modelCooldownResponse    `json:"model_cooldowns,omitempty"`
	Enabled                  bool                       `json:"enabled"`
	Locked                   bool                       `json:"locked"`
	AllowedAPIKeyIDs         []int64                    `json:"allowed_api_key_ids"`
	Tags                     []string                   `json:"tags"`
	GroupIDs                 []int64                    `json:"group_ids"`
	// 图片配额信息
	ImageQuotaRemaining *int   `json:"image_quota_remaining,omitempty"`
	ImageQuotaTotal     *int   `json:"image_quota_total,omitempty"`
	TodayUsedCount      *int   `json:"today_used_count,omitempty"`
	ImageQuotaResetAt   string `json:"image_quota_reset_at,omitempty"`
}

type modelCooldownResponse struct {
	Model     string `json:"model"`
	Reason    string `json:"reason"`
	ResetAt   string `json:"reset_at"`
	Remaining int64  `json:"remaining_seconds"`
}

type accountUsageWindow struct {
	Requests      int64   `json:"requests"`
	Tokens        int64   `json:"tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
}

func accountEmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || strings.ContainsAny(email, " \t\r\n") {
		return ""
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.Trim(strings.TrimSpace(email[at+1:]), ".")
	if domain == "" || strings.ContainsAny(domain, " /\\:") || !strings.Contains(domain, ".") {
		return ""
	}
	return domain
}

func accountAccessTokenType(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	if tokenType := strings.TrimSpace(row.GetCredential("access_token_type")); tokenType != "" {
		return tokenType
	}
	return accessTokenTypeForToken(row.GetCredential("access_token"))
}

type schedulerBreakdownResponse struct {
	UnauthorizedPenalty float64 `json:"unauthorized_penalty"`
	RateLimitPenalty    float64 `json:"rate_limit_penalty"`
	TimeoutPenalty      float64 `json:"timeout_penalty"`
	ServerPenalty       float64 `json:"server_penalty"`
	FailurePenalty      float64 `json:"failure_penalty"`
	SuccessBonus        float64 `json:"success_bonus"`
	UsagePenalty7d      float64 `json:"usage_penalty_7d"`
	UsageUrgencyBonus5h float64 `json:"usage_urgency_bonus_5h"`
	UsageUrgencyBonus7d float64 `json:"usage_urgency_bonus_7d"`
	ExpiryUrgencyBonus  float64 `json:"expiry_urgency_bonus"`
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
	usage5h, usage7d := h.getAccountUsageWindows(ctx)

	accounts := make([]accountResponse, 0, len(rows))
	for _, row := range rows {
		isOpenAIResponsesAccount := strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses)
		email := row.GetCredential("email")
		baseURL := row.GetCredential("base_url")
		if isOpenAIResponsesAccount && email == "" {
			email = baseURL
		}
		planType := row.GetCredential("plan_type")
		if isOpenAIResponsesAccount && planType == "" {
			planType = "api"
		}
		resp := accountResponse{
			ID:                       row.ID,
			Name:                     row.Name,
			Email:                    email,
			EmailDomain:              accountEmailDomain(email),
			ChatGPTAccountID:         row.GetCredential("account_id"),
			PlanType:                 planType,
			SubscriptionExpiresAt:    row.GetCredential("subscription_expires_at"),
			Status:                   row.Status,
			ErrorMessage:             row.ErrorMessage,
			ATOnly:                   !isOpenAIResponsesAccount && row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
			CreditEnabled:            row.CreditEnabled,
			CreditSkipUsageWindow:    row.CreditSkipUsageWindow,
			SkipWarmTier:             row.SkipWarmTier,
			AccountType:              row.Type,
			AccessTokenType:          accountAccessTokenType(row),
			OpenAIResponsesAPI:       isOpenAIResponsesAccount,
			BaseURL:                  baseURL,
			Models:                   row.GetCredentialStringSlice("models"),
			ModelMapping:             row.GetCredential("model_mapping"),
			CustomHeaders:            row.GetCredentialStringMap("custom_headers"),
			ProxyURL:                 row.ProxyURL,
			Enabled:                  row.Enabled,
			Locked:                   row.Locked,
			AllowedAPIKeyIDs:         row.GetCredentialInt64Slice("allowed_api_key_ids"),
			Tags:                     append([]string(nil), row.Tags...),
			ScoreBiasOverride:        nullableInt64Pointer(row.ScoreBiasOverride),
			ScoreBiasEffective:       effectiveScoreBias(planType, row.ScoreBiasOverride),
			BaseConcurrencyOverride:  nullableInt64Pointer(row.BaseConcurrencyOverride),
			BaseConcurrencyEffective: effectiveBaseConcurrency(row.BaseConcurrencyOverride, int64(h.store.GetMaxConcurrency())),
			CreatedAt:                row.CreatedAt.Format(time.RFC3339),
			UpdatedAt:                row.UpdatedAt.Format(time.RFC3339),
			CodexUsageUpdatedAt:      row.GetCredential("codex_usage_updated_at"),
			Codex5HUsageUpdatedAt:    row.GetCredential("codex_5h_usage_updated_at"),
		}
		resp.AutoPause5hThreshold = accountQuotaAutoPauseThreshold(row, "auto_pause_5h_threshold")
		resp.AutoPause7dThreshold = accountQuotaAutoPauseThreshold(row, "auto_pause_7d_threshold")
		resp.AutoPause5hDisabled = row.GetCredentialBool("auto_pause_5h_disabled")
		resp.AutoPause7dDisabled = row.GetCredentialBool("auto_pause_7d_disabled")
		resp.DispatchCountLimit = accountDispatchCountLimit(row)
		if acc, ok := accountMap[row.ID]; ok {
			acc.Mu().RLock()
			resp.GroupIDs = append([]int64(nil), acc.GroupIDs...)
			acc.Mu().RUnlock()
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
				UsageUrgencyBonus5h: debug.Breakdown.UsageUrgencyBonus5h,
				UsageUrgencyBonus7d: debug.Breakdown.UsageUrgencyBonus7d,
				ExpiryUrgencyBonus:  debug.Breakdown.ExpiryUrgencyBonus,
				LatencyPenalty:      debug.Breakdown.LatencyPenalty,
				SuccessRatePenalty:  debug.Breakdown.SuccessRatePenalty,
			}
			if usagePct, ok := acc.GetUsagePercent7d(); ok {
				resp.UsagePercent7d = &usagePct
			}
			if usagePct5h, ok := acc.GetUsagePercent5h(); ok {
				resp.UsagePercent5h = &usagePct5h
			}
			if credits, ok := acc.GetRateLimitResetCredits(); ok {
				resp.RateLimitResetCredits = &credits
			}
			if snapshot := acc.GetDispatchCountSnapshot(); snapshot.Limit > 0 {
				limit := snapshot.Limit
				resp.DispatchCountLimit = &limit
				resp.DispatchCountUsed = snapshot.Used
				resp.DispatchCountLimited = snapshot.Limited
				if !snapshot.ResetAt.IsZero() {
					resp.DispatchCountResetAt = snapshot.ResetAt.Format(time.RFC3339)
				}
			}
			if t := acc.GetReset5hAt(); !t.IsZero() {
				resp.Reset5hAt = t.Format(time.RFC3339)
			}
			if t := acc.GetReset7dAt(); !t.IsZero() {
				resp.Reset7dAt = t.Format(time.RFC3339)
			}
			if sec := acc.GetWindow7dSeconds(); sec > 0 {
				resp.Window7dSeconds = &sec
				resp.Window7dKind = acc.Window7dKind()
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
			if reason, until := acc.GetCooldownSnapshot(); !until.IsZero() && until.After(time.Now()) {
				resp.CooldownReason = reason
				resp.CooldownUntil = until.Format(time.RFC3339)
			}
			for _, cooldown := range acc.ActiveModelCooldowns() {
				resp.ModelCooldowns = append(resp.ModelCooldowns, modelCooldownResponse{
					Model:     cooldown.Model,
					Reason:    cooldown.Reason,
					ResetAt:   cooldown.ResetAt.Format(time.RFC3339),
					Remaining: int64(time.Until(cooldown.ResetAt).Seconds()),
				})
			}
			// 使用运行时状态（优先于 DB 状态）
			resp.Status = acc.RuntimeStatus()
			acc.Mu().RLock()
			resp.ErrorMessage = acc.ErrorMsg
			acc.Mu().RUnlock()
		} else if row.CooldownUntil.Valid && row.CooldownUntil.Time.After(time.Now()) {
			resp.CooldownReason = row.CooldownReason
			resp.CooldownUntil = row.CooldownUntil.Time.Format(time.RFC3339)
		}
		if resp.DispatchScore == 0 {
			resp.DispatchScore = dispatchScoreFallback(resp.SchedulerScore, resp.ScoreBiasEffective, resp.HealthTier, resp.Status)
		}
		if rc, ok := reqCounts[row.ID]; ok {
			resp.SuccessRequests = rc.SuccessCount
			resp.ErrorRequests = rc.ErrorCount
			resp.RetryErrorRequests = rc.RetryErrorCount
			resp.RateLimitAttempts = rc.RateLimitAttemptCount
		}
		if usage, ok := usage5h[row.ID]; ok {
			resp.Usage5hDetail = &accountUsageWindow{
				Requests:      usage.Requests,
				Tokens:        usage.Tokens,
				AccountBilled: usage.AccountBilled,
				UserBilled:    usage.UserBilled,
			}
		}
		if usage, ok := usage7d[row.ID]; ok {
			resp.Usage7dDetail = &accountUsageWindow{
				Requests:      usage.Requests,
				Tokens:        usage.Tokens,
				AccountBilled: usage.AccountBilled,
				UserBilled:    usage.UserBilled,
			}
		}
		accounts = append(accounts, resp)
	}

	billing5hWindows := make(map[int64]time.Time)
	billing7dWindows := make(map[int64]time.Time)
	for i := range accounts {
		acc, ok := accountMap[accounts[i].ID]
		if !ok {
			continue
		}
		if t := acc.GetReset5hAt(); !t.IsZero() {
			billing5hWindows[accounts[i].ID] = t.Add(-5 * time.Hour)
		}
		if t := acc.GetReset7dAt(); !t.IsZero() {
			// 长窗口起点 = reset - 真实周期。free/team 是月窗(约 30 天),
			// 写死减 7 天会把起点算到未来,成本恒为 0 (issue #324)。
			windowDur := 7 * 24 * time.Hour
			if sec := acc.GetWindow7dSeconds(); sec > 0 {
				windowDur = time.Duration(sec) * time.Second
			}
			billing7dWindows[accounts[i].ID] = t.Add(-windowDur)
		}
	}

	billed5h, err := h.db.GetAccountsBilledSince(ctx, billing5hWindows)
	if err != nil {
		log.Printf("批量获取账号 5h 成本失败: %v", err)
		billed5h = nil
	}
	billed7d, err := h.db.GetAccountsBilledSince(ctx, billing7dWindows)
	if err != nil {
		log.Printf("批量获取账号 7d 成本失败: %v", err)
		billed7d = nil
	}
	for i := range accounts {
		if billed, ok := billed5h[accounts[i].ID]; ok {
			accounts[i].Billed5h = &billed
		}
		if billed, ok := billed7d[accounts[i].ID]; ok {
			accounts[i].Billed7d = &billed
		}
	}

	c.JSON(http.StatusOK, accountsResponse{Accounts: accounts})
}

type updateAccountSchedulerReq struct {
	ScoreBiasOverride       json.RawMessage `json:"score_bias_override"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	SkipWarmTier            json.RawMessage `json:"skip_warm_tier"`
	AllowedAPIKeyIDs        json.RawMessage `json:"allowed_api_key_ids"`
	Tags                    json.RawMessage `json:"tags"`
	GroupIDs                json.RawMessage `json:"group_ids"`
	AutoPause5hThreshold    json.RawMessage `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    json.RawMessage `json:"auto_pause_7d_threshold"`
	AutoPause5hDisabled     json.RawMessage `json:"auto_pause_5h_disabled"`
	AutoPause7dDisabled     json.RawMessage `json:"auto_pause_7d_disabled"`
	DispatchCountLimit      json.RawMessage `json:"dispatch_count_limit"`
	ProxyURL                json.RawMessage `json:"proxy_url"`
	CustomHeaders           json.RawMessage `json:"custom_headers"`
}

type accountSchedulerUpdate struct {
	ScoreBiasOverride       database.OptionalNullInt64
	BaseConcurrencyOverride database.OptionalNullInt64
	SkipWarmTier            database.OptionalBool
	AllowedAPIKeyIDs        database.OptionalInt64Slice
	Tags                    optionalStringSlice
	GroupIDs                database.OptionalInt64Slice
	AutoPause5hThreshold    optionalFloat64
	AutoPause7dThreshold    optionalFloat64
	AutoPause5hDisabled     database.OptionalBool
	AutoPause7dDisabled     database.OptionalBool
	DispatchCountLimit      database.OptionalNullInt64
	ProxyURL                database.OptionalString
	CustomHeaders           optionalCustomHeaders
	CredentialUpdates       map[string]interface{}
}

func parseAccountSchedulerUpdate(req updateAccountSchedulerReq) (accountSchedulerUpdate, error) {
	scoreBiasOverride, err := parseOptionalIntegerField(req.ScoreBiasOverride, "score_bias_override", -200, 200)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	baseConcurrencyOverride, err := parseOptionalIntegerField(req.BaseConcurrencyOverride, "base_concurrency_override", 1, 50)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	skipWarmTier, err := parseOptionalBoolField(req.SkipWarmTier, "skip_warm_tier")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	allowedAPIKeyIDs, err := parseOptionalIntegerSliceField(req.AllowedAPIKeyIDs, "allowed_api_key_ids")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	tags, err := parseOptionalStringSliceField(req.Tags, "tags")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	groupIDs, err := parseOptionalIntegerSliceField(req.GroupIDs, "group_ids")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause5hThreshold, err := parseOptionalRatioField(req.AutoPause5hThreshold, "auto_pause_5h_threshold")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause7dThreshold, err := parseOptionalRatioField(req.AutoPause7dThreshold, "auto_pause_7d_threshold")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause5hDisabled, err := parseOptionalBoolField(req.AutoPause5hDisabled, "auto_pause_5h_disabled")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause7dDisabled, err := parseOptionalBoolField(req.AutoPause7dDisabled, "auto_pause_7d_disabled")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	dispatchCountLimit, err := parseOptionalIntegerField(req.DispatchCountLimit, "dispatch_count_limit", 0, 1000000)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}

	proxyURL, err := parseOptionalStringField(req.ProxyURL, "proxy_url", security.ValidateProxyURL)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	customHeaders, err := parseOptionalCustomHeadersField(req.CustomHeaders)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	credentialUpdates := make(map[string]interface{})
	if customHeaders.Set {
		credentialUpdates["custom_headers"] = cloneCustomHeaders(customHeaders.Values)
	}
	if autoPause5hThreshold.Set {
		credentialUpdates["auto_pause_5h_threshold"] = autoPause5hThreshold.Value
	}
	if autoPause7dThreshold.Set {
		credentialUpdates["auto_pause_7d_threshold"] = autoPause7dThreshold.Value
	}
	if autoPause5hDisabled.Set {
		credentialUpdates["auto_pause_5h_disabled"] = autoPause5hDisabled.Value
	}
	if autoPause7dDisabled.Set {
		credentialUpdates["auto_pause_7d_disabled"] = autoPause7dDisabled.Value
	}
	if dispatchCountLimit.Set {
		if dispatchCountLimit.Value.Valid {
			credentialUpdates["dispatch_count_limit"] = dispatchCountLimit.Value.Int64
		} else {
			credentialUpdates["dispatch_count_limit"] = int64(0)
		}
	}
	if len(credentialUpdates) == 0 {
		credentialUpdates = nil
	}

	return accountSchedulerUpdate{
		ScoreBiasOverride:       scoreBiasOverride,
		BaseConcurrencyOverride: baseConcurrencyOverride,
		SkipWarmTier:            skipWarmTier,
		AllowedAPIKeyIDs:        allowedAPIKeyIDs,
		Tags:                    tags,
		GroupIDs:                groupIDs,
		AutoPause5hThreshold:    autoPause5hThreshold,
		AutoPause7dThreshold:    autoPause7dThreshold,
		AutoPause5hDisabled:     autoPause5hDisabled,
		AutoPause7dDisabled:     autoPause7dDisabled,
		DispatchCountLimit:      dispatchCountLimit,
		ProxyURL:                proxyURL,
		CustomHeaders:           customHeaders,
		CredentialUpdates:       credentialUpdates,
	}, nil
}

func (u accountSchedulerUpdate) hasChanges() bool {
	return u.ScoreBiasOverride.Set ||
		u.BaseConcurrencyOverride.Set ||
		u.SkipWarmTier.Set ||
		u.AllowedAPIKeyIDs.Set ||
		u.Tags.Set ||
		u.GroupIDs.Set ||
		u.AutoPause5hThreshold.Set ||
		u.AutoPause7dThreshold.Set ||
		u.AutoPause5hDisabled.Set ||
		u.AutoPause7dDisabled.Set ||
		u.DispatchCountLimit.Set ||
		u.ProxyURL.Set
}

func optionalBoolFromPtr(value *bool) database.OptionalBool {
	if value == nil {
		return database.OptionalBool{}
	}
	return database.OptionalBool{Set: true, Value: *value}
}

// UpdateAccountScheduler 更新账号调度配置。
// UpdateAccountCredit 更新账号信用设置
func (h *Handler) UpdateAccountCredit(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		CreditEnabled         *bool `json:"credit_enabled"`
		CreditSkipUsageWindow *bool `json:"credit_skip_usage_window"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}

	// 传入 *bool：nil = 不修改该字段
	if err := h.store.UpdateAccountCredit(id, req.CreditEnabled, req.CreditSkipUsageWindow); err != nil {
		writeError(c, http.StatusInternalServerError, "更新信用设置失败: "+err.Error())
		return
	}

	acc = h.store.FindByID(id)
	if acc != nil {
		c.JSON(http.StatusOK, gin.H{"message": "信用设置已更新", "credit_enabled": acc.CreditEnabled, "credit_skip_usage_window": acc.CreditSkipUsageWindow})
	} else {
		c.JSON(http.StatusOK, gin.H{"message": "信用设置已更新"})
	}
}

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

	update, err := parseAccountSchedulerUpdate(req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if update.AllowedAPIKeyIDs.Set {
		missingAPIKeyIDs, err := h.findMissingAPIKeyIDs(ctx, update.AllowedAPIKeyIDs.Values)
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
	if update.GroupIDs.Set {
		missingGroupIDs, err := h.db.VerifyAccountGroupIDs(ctx, update.GroupIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "校验账号分组失败: "+err.Error())
			return
		}
		if len(missingGroupIDs) > 0 {
			values := make([]string, 0, len(missingGroupIDs))
			for _, value := range missingGroupIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "group_ids 包含不存在的分组 ID: "+strings.Join(values, ", "))
			return
		}
	}

	if err := h.db.UpdateAccountSchedulerMetadata(ctx, id, update.ScoreBiasOverride, update.BaseConcurrencyOverride, update.SkipWarmTier, update.AllowedAPIKeyIDs, database.OptionalStringSlice{Set: update.Tags.Set, Values: update.Tags.Values}, update.GroupIDs, update.ProxyURL, update.CredentialUpdates); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "更新账号调度配置失败: "+err.Error())
		return
	}
	h.applyAccountSchedulerRuntimeUpdate(id, update)

	writeMessage(c, http.StatusOK, "账号调度配置已更新")
}

func (h *Handler) applyAccountSchedulerRuntimeUpdate(id int64, update accountSchedulerUpdate) {
	if h.store == nil {
		return
	}
	if update.ScoreBiasOverride.Set || update.BaseConcurrencyOverride.Set || update.SkipWarmTier.Set {
		h.store.ApplyAccountSchedulerOverridePatch(
			id,
			update.ScoreBiasOverride.Set,
			nullableInt64Pointer(update.ScoreBiasOverride.Value),
			update.BaseConcurrencyOverride.Set,
			nullableInt64Pointer(update.BaseConcurrencyOverride.Value),
			optionalBoolPtr(update.SkipWarmTier),
		)
	}
	if update.AllowedAPIKeyIDs.Set {
		h.store.ApplyAccountAllowedAPIKeys(id, update.AllowedAPIKeyIDs.Values)
	}
	if update.AutoPause5hThreshold.Set || update.AutoPause7dThreshold.Set || update.AutoPause5hDisabled.Set || update.AutoPause7dDisabled.Set {
		h.store.ApplyAccountQuotaAutoPauseConfig(
			id,
			optionalFloat64Ptr(update.AutoPause5hThreshold),
			optionalFloat64Ptr(update.AutoPause7dThreshold),
			optionalBoolPtr(update.AutoPause5hDisabled),
			optionalBoolPtr(update.AutoPause7dDisabled),
		)
	}
	if update.DispatchCountLimit.Set {
		h.store.ApplyAccountDispatchCountLimit(id, nullableInt64Pointer(update.DispatchCountLimit.Value))
	}
	if update.Tags.Set {
		h.store.ApplyAccountTags(id, update.Tags.Values)
	}
	if update.GroupIDs.Set {
		h.store.ApplyAccountGroups(id, update.GroupIDs.Values)
	}
	if update.ProxyURL.Set {
		h.store.ApplyAccountProxyURL(id, update.ProxyURL.Value)
	}
	if update.CustomHeaders.Set {
		h.store.ApplyAccountCustomHeaders(id, update.CustomHeaders.Values)
	}
}

type optionalCustomHeaders struct {
	Set    bool
	Values map[string]string
}

func parseOptionalCustomHeadersField(raw json.RawMessage) (optionalCustomHeaders, error) {
	if len(raw) == 0 {
		return optionalCustomHeaders{}, nil
	}
	if string(raw) == "null" {
		return optionalCustomHeaders{Set: true}, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return optionalCustomHeaders{}, fmt.Errorf("custom_headers 必须是对象或 null")
	}
	normalized, err := normalizeCustomHeaders(headers)
	if err != nil {
		return optionalCustomHeaders{}, err
	}
	return optionalCustomHeaders{Set: true, Values: normalized}, nil
}

func normalizeCustomHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	if len(headers) > 64 {
		return nil, fmt.Errorf("custom_headers 最多支持 64 个请求头")
	}
	out := make(map[string]string, len(headers))
	for rawName, rawValue := range headers {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if len(name) > 128 || !isValidHeaderName(name) {
			return nil, fmt.Errorf("custom_headers 包含无效请求头名称: %s", name)
		}
		value := strings.TrimSpace(rawValue)
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("custom_headers.%s 不能包含换行符", name)
		}
		if len(value) > 8192 {
			return nil, fmt.Errorf("custom_headers.%s 不能超过 8192 字符", name)
		}
		out[http.CanonicalHeaderKey(name)] = value
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func normalizeAccountModelMapping(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("模型映射必须是 JSON 对象")
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", fmt.Errorf("模型映射必须是 JSON 对象")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("模型映射格式错误")
		}
		key, ok := keyTok.(string)
		if !ok || strings.TrimSpace(key) == "" {
			return "", fmt.Errorf("模型映射的源模型不能为空")
		}
		// 源模型别名会进入 /v1/models 响应、模型校验和使用日志，
		// 必须与 models 列表同标准校验，防止任意字符串注入。
		if err := security.ValidateModelName(strings.TrimSpace(key)); err != nil {
			return "", fmt.Errorf("模型映射的源模型 %q 无效: %w", key, err)
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return "", fmt.Errorf("模型映射的目标模型必须是字符串")
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("模型映射的目标模型不能为空")
		}
		if err := security.ValidateModelName(strings.TrimSpace(value)); err != nil {
			return "", fmt.Errorf("模型映射的目标模型 %q 无效: %w", value, err)
		}
	}
	endTok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("模型映射格式错误")
	}
	end, ok := endTok.(json.Delim)
	if !ok || end != '}' {
		return "", fmt.Errorf("模型映射格式错误")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("模型映射只能包含一个 JSON 对象")
	}
	return raw, nil
}

func isValidHeaderName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return name != ""
}

func cloneCustomHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[name] = value
	}
	return out
}

type optionalStringSlice struct {
	Set    bool
	Values []string
}

type optionalFloat64 struct {
	Set   bool
	Value float64
}

func accountQuotaAutoPauseThreshold(row *database.AccountRow, key string) *float64 {
	value, ok := row.GetCredentialFloat64(key)
	if !ok || value <= 0 {
		return nil
	}
	if value > 1 {
		value = 1
	}
	return &value
}

func accountDispatchCountLimit(row *database.AccountRow) *int64 {
	value, ok := row.GetCredentialInt64("dispatch_count_limit")
	if !ok || value <= 0 {
		return nil
	}
	if value > 1000000 {
		value = 1000000
	}
	return &value
}

func parseOptionalStringSliceField(raw json.RawMessage, field string) (optionalStringSlice, error) {
	if len(raw) == 0 {
		return optionalStringSlice{}, nil
	}
	if string(raw) == "null" {
		return optionalStringSlice{Set: true, Values: []string{}}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return optionalStringSlice{}, fmt.Errorf("%s 必须是字符串数组或 null", field)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		clean := strings.TrimSpace(value)
		if clean == "" {
			continue
		}
		if utf8.RuneCountInString(clean) > 40 {
			return optionalStringSlice{}, fmt.Errorf("%s 单个标签不能超过 40 字符", field)
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	if len(out) > 32 {
		return optionalStringSlice{}, fmt.Errorf("%s 最多 32 个标签", field)
	}
	return optionalStringSlice{Set: true, Values: out}, nil
}

func parseOptionalStringField(raw json.RawMessage, field string, validator func(string) error) (database.OptionalString, error) {
	if len(raw) == 0 {
		return database.OptionalString{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalString{Set: true, Value: ""}, nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return database.OptionalString{}, fmt.Errorf("%s 必须是字符串或 null", field)
	}
	value = strings.TrimSpace(value)
	if validator != nil {
		if err := validator(value); err != nil {
			return database.OptionalString{}, fmt.Errorf("%s 无效: %w", field, err)
		}
	}
	return database.OptionalString{Set: true, Value: value}, nil
}

func parseOptionalIntegerField(raw json.RawMessage, field string, minValue, maxValue int64) (database.OptionalNullInt64, error) {
	if len(raw) == 0 {
		return database.OptionalNullInt64{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalNullInt64{Set: true}, nil
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return database.OptionalNullInt64{}, fmt.Errorf("%s 必须是整数或 null", field)
	}
	value, err := number.Int64()
	if err != nil {
		return database.OptionalNullInt64{}, fmt.Errorf("%s 必须是整数或 null", field)
	}
	if value < minValue || value > maxValue {
		return database.OptionalNullInt64{}, fmt.Errorf("%s 超出范围，必须在 %d..%d 之间", field, minValue, maxValue)
	}
	return database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: value, Valid: true}}, nil
}

func parseOptionalRatioField(raw json.RawMessage, field string) (optionalFloat64, error) {
	if len(raw) == 0 {
		return optionalFloat64{}, nil
	}
	if string(raw) == "null" {
		return optionalFloat64{Set: true, Value: 0}, nil
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return optionalFloat64{}, fmt.Errorf("%s 必须是 0..1 之间的小数或 null", field)
	}
	value, err := number.Float64()
	if err != nil {
		return optionalFloat64{}, fmt.Errorf("%s 必须是 0..1 之间的小数或 null", field)
	}
	if value < 0 || value > 1 {
		return optionalFloat64{}, fmt.Errorf("%s 超出范围，必须在 0..1 之间", field)
	}
	return optionalFloat64{Set: true, Value: value}, nil
}

func parseOptionalBoolField(raw json.RawMessage, field string) (database.OptionalBool, error) {
	if len(raw) == 0 {
		return database.OptionalBool{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalBool{Set: true, Value: false}, nil
	}

	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return database.OptionalBool{}, fmt.Errorf("%s 必须是布尔值或 null", field)
	}
	return database.OptionalBool{Set: true, Value: value}, nil
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

func optionalFloat64Ptr(value optionalFloat64) *float64 {
	if !value.Set {
		return nil
	}
	v := value.Value
	return &v
}

func optionalBoolPtr(value database.OptionalBool) *bool {
	if !value.Set {
		return nil
	}
	v := value.Value
	return &v
}

func effectiveScoreBias(planType string, override sql.NullInt64) int64 {
	if override.Valid {
		return override.Int64
	}
	// 与 auth.defaultScoreBiasForPlan 保持一致；k12 是教育版 team (issue #282)
	switch auth.NormalizePlanType(planType) {
	case "pro", "plus", "team", "k12":
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

func (h *Handler) getAccountUsageWindows(ctx context.Context) (map[int64]*database.AccountTimeRangeUsage, map[int64]*database.AccountTimeRangeUsage) {
	now := time.Now()
	usage5h, err := h.db.GetAccountTimeRangeUsage(ctx, now.Add(-5*time.Hour))
	if err != nil {
		log.Printf("获取账号 5h 用量统计失败: %v", err)
		usage5h = make(map[int64]*database.AccountTimeRangeUsage)
	}
	usage7d, err := h.db.GetAccountTimeRangeUsage(ctx, now.AddDate(0, 0, -7))
	if err != nil {
		log.Printf("获取账号 7d 用量统计失败: %v", err)
		usage7d = make(map[int64]*database.AccountTimeRangeUsage)
	}
	return usage5h, usage7d
}

type addAccountReq struct {
	Name           string            `json:"name"`
	RefreshToken   string            `json:"refresh_token"`
	SessionToken   string            `json:"session_token"`
	ProxyURL       string            `json:"proxy_url"`
	CustomHeaders  map[string]string `json:"custom_headers"`
	AllowDuplicate bool              `json:"allow_duplicate"`
}

func splitAccountCredentialLines(raw string, sanitize bool) []string {
	lines := strings.Split(raw, "\n")
	tokens := make([]string, 0, len(lines))
	for _, line := range lines {
		token := strings.TrimSpace(line)
		if sanitize {
			token = strings.TrimSpace(security.SanitizeInput(token))
		}
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// accountCredentialDedup 跟踪 RT/ST 原文去重（用于 RT/ST 单账号/批量添加路径）。
// 身份型（OAuth）去重在文件导入与 AT 路径单独处理，这里只覆盖加入时无法解出身份的 RT/ST。
type accountCredentialDedup struct {
	existingRT map[string]bool
	existingST map[string]bool
	seenRT     map[string]bool
	seenST     map[string]bool
}

func (h *Handler) newAccountCredentialDedup(ctx context.Context) *accountCredentialDedup {
	d := &accountCredentialDedup{
		seenRT: make(map[string]bool),
		seenST: make(map[string]bool),
	}
	var err error
	if d.existingRT, err = h.db.GetAllRefreshTokens(ctx); err != nil {
		log.Printf("查询已有 RT 失败: %v", err)
		d.existingRT = make(map[string]bool)
	}
	if d.existingST, err = h.db.GetAllSessionTokens(ctx); err != nil {
		log.Printf("查询已有 ST 失败: %v", err)
		d.existingST = make(map[string]bool)
	}
	return d
}

// checkAndMark 返回 true 表示该 seed 与已有库或本批次重复（应跳过）；非重复时记录其凭证。
func (d *accountCredentialDedup) checkAndMark(seed tokenCredentialSeed) bool {
	rt := strings.TrimSpace(seed.refreshToken)
	st := strings.TrimSpace(seed.sessionToken)
	if rt != "" {
		if d.existingRT[rt] || d.seenRT[rt] {
			return true
		}
	} else if st != "" {
		if d.existingST[st] || d.seenST[st] {
			return true
		}
	}
	if rt != "" {
		d.seenRT[rt] = true
	}
	if st != "" {
		d.seenST[st] = true
	}
	return false
}

// AddAccount 添加新账号（支持批量：refresh_token/session_token 按行分割）
func (h *Handler) AddAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 输入验证和清理
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if strings.TrimSpace(req.RefreshToken) == "" && strings.TrimSpace(req.SessionToken) == "" {
		writeError(c, http.StatusBadRequest, "refresh_token 或 session_token 是必填字段")
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
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.CustomHeaders = customHeaders

	// 按行分割，支持批量添加。refresh_token 与 session_token 同时填写时，
	// session_token 可填写一行应用到所有 RT，也可与 RT 行数一一对应。
	refreshTokens := splitAccountCredentialLines(req.RefreshToken, true)
	sessionTokens := splitAccountCredentialLines(req.SessionToken, true)
	total := len(refreshTokens)
	if total == 0 {
		total = len(sessionTokens)
	}
	if len(refreshTokens) > 0 && len(sessionTokens) > 1 && len(sessionTokens) != len(refreshTokens) {
		writeError(c, http.StatusBadRequest, "session_token 行数需为 1 或与 refresh_token 行数一致")
		return
	}

	var seeds []tokenCredentialSeed
	for i := 0; i < total; i++ {
		seed := tokenCredentialSeed{allowDuplicate: req.AllowDuplicate, customHeaders: customHeaders}
		if len(refreshTokens) > 0 {
			seed.refreshToken = refreshTokens[i]
		}
		if len(sessionTokens) == 1 {
			seed.sessionToken = sessionTokens[0]
		} else if len(sessionTokens) > 1 {
			seed.sessionToken = sessionTokens[i]
		}
		seeds = append(seeds, seed)
	}

	if len(seeds) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效的 Refresh Token 或 Session Token")
		return
	}

	// 限制批量添加数量
	if len(seeds) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多添加100个账号")
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamAddAccounts(c, req, seeds)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0
	duplicateCount := 0

	var dedup *accountCredentialDedup
	if !req.AllowDuplicate {
		dedup = h.newAccountCredentialDedup(ctx)
	}

	for i, seed := range seeds {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(seeds) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		if dedup != nil && dedup.checkAndMark(seed) {
			duplicateCount++
			log.Printf("添加账号 %d 已存在（RT/ST 重复），跳过", i+1)
			continue
		}

		id, err := h.db.InsertAccountWithCredentials(ctx, name, tokenCredentialMap(seed), req.ProxyURL)
		if err != nil {
			log.Printf("批量添加账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual")

		// 热加载：直接加入内存池
		newAcc := accountFromCredentialSeed(id, req.ProxyURL, seed)
		h.store.AddAccount(newAcc)

		if newAcc.GetAccessToken() != "" {
			h.triggerImportedAccountUsageProbe(id, "manual_add")
		} else if !h.store.GetLazyMode() {
			// 异步刷新 AT，刷新成功后立即做 wham 用量采样。
			go h.refreshImportedAccountAndProbe(id, "manual_add_refresh")
		}
	}

	// 记录安全审计日志
	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d duplicate=%d failed=%d ip=%s", successCount, duplicateCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功添加 %d 个账号", successCount)
	if duplicateCount > 0 {
		msg += fmt.Sprintf("，%d 个重复跳过", duplicateCount)
	}
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   msg,
		"success":   successCount,
		"duplicate": duplicateCount,
		"failed":    failCount,
	})
}

func (h *Handler) streamAddAccounts(c *gin.Context, req addAccountReq, seeds []tokenCredentialSeed) {
	setupSSE(c)

	total := len(seeds)
	successCount := 0
	failCount := 0
	duplicateCount := 0
	sendImportEvent(c, importEvent{
		Type: "progress", Current: 0, Total: total,
		Success: 0, Duplicate: 0, Failed: 0,
	})

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	var dedup *accountCredentialDedup
	if !req.AllowDuplicate {
		dedup = h.newAccountCredentialDedup(ctx)
	}

	for i, seed := range seeds {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(seeds) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		if dedup != nil && dedup.checkAndMark(seed) {
			duplicateCount++
			sendImportEvent(c, importEvent{
				Type: "progress", Current: i + 1, Total: total,
				Success: successCount, Duplicate: duplicateCount, Failed: failCount,
			})
			continue
		}

		id, err := h.db.InsertAccountWithCredentials(ctx, name, tokenCredentialMap(seed), req.ProxyURL)
		if err != nil {
			log.Printf("批量添加账号 %d 失败: %v", i+1, err)
			failCount++
			sendImportEvent(c, importEvent{
				Type: "progress", Current: i + 1, Total: total,
				Success: successCount, Duplicate: duplicateCount, Failed: failCount,
			})
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual")

		newAcc := accountFromCredentialSeed(id, req.ProxyURL, seed)
		h.store.AddAccount(newAcc)

		if newAcc.GetAccessToken() != "" {
			h.triggerImportedAccountUsageProbe(id, "manual_add")
		} else if !h.store.GetLazyMode() {
			go h.refreshImportedAccountAndProbe(id, "manual_add_refresh")
		}

		sendImportEvent(c, importEvent{
			Type: "progress", Current: i + 1, Total: total,
			Success: successCount, Duplicate: duplicateCount, Failed: failCount,
		})
	}

	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d duplicate=%d failed=%d ip=%s", successCount, duplicateCount, failCount, c.ClientIP()))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: successCount, Duplicate: duplicateCount, Failed: failCount,
	})
}

// addATAccountReq AT 模式添加账号请求
type addATAccountReq struct {
	Name           string            `json:"name"`
	AccessToken    string            `json:"access_token"`
	ProxyURL       string            `json:"proxy_url"`
	CustomHeaders  map[string]string `json:"custom_headers"`
	AllowDuplicate bool              `json:"allow_duplicate"`
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
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.CustomHeaders = customHeaders

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

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamAddATAccounts(c, req, tokens)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0
	updatedCount := 0
	duplicateCount := 0

	// AT 去重：非身份型 AT-only（无法从 JWT 解出 email + 工作区/用户 ID，如 codex_at）
	// 按 access_token 原文去重；身份型 AT 由 upsertOAuthIdentityAccount 按 OAuth 身份
	//（email + account_id/user_id）去重/更新——AT 会轮换，仅按原文去重会重复导入同一账号。
	// 勾选"允许重复添加"时跳过全部去重，强制新建。
	existingATs := make(map[string]bool)
	seenAT := make(map[string]bool)
	if !req.AllowDuplicate {
		if got, err := h.db.GetAllAccessTokens(ctx); err != nil {
			log.Printf("查询已有 AT 失败: %v", err)
		} else {
			existingATs = got
		}
	}

	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
			accessToken:    at,
			allowDuplicate: req.AllowDuplicate,
			customHeaders:  customHeaders,
		})
		if !req.AllowDuplicate && seed.email != "" && (seed.accountID != "" || seed.userID != "") {
			id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, req.ProxyURL, seed, "manual_at")
			if err != nil {
				log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
				failCount++
				continue
			}
			if updated {
				// 已有账号只更新凭证，不计入"新增"。
				updatedCount++
				log.Printf("AT 账号 %d 命中已有身份并更新凭证 (id=%d)", i+1, id)
			} else {
				successCount++
				log.Printf("AT 账号 %d 已加入号池 (id=%d)", i+1, id)
			}
			continue
		}

		if !req.AllowDuplicate {
			if existingATs[at] || seenAT[at] {
				duplicateCount++
				log.Printf("AT 账号 %d 已存在（access_token 重复），跳过", i+1)
				continue
			}
			seenAT[at] = true
		}

		id, err := h.db.InsertATAccount(ctx, name, at, req.ProxyURL)
		if err != nil {
			log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual_at")

		// 热加载到内存池（AT-only，无 RT）。codex_at 不走 JWT 解码，
		// 身份信息后续由 wham 用量查询补齐。
		newAcc := accountFromCredentialSeed(id, req.ProxyURL, seed)
		h.store.AddAccount(newAcc)

		// 将解析/识别到的信息持久化到数据库。
		if creds := tokenCredentialMap(seed); len(creds) > 0 {
			if err := h.db.UpdateCredentials(ctx, id, creds); err != nil {
				log.Printf("AT 账号 %d 更新 credentials 失败: %v", id, err)
			}
		}
		// 触发 wham 用量探针：codex_at 的身份此刻未知，探针补齐身份后会回查
		// 并合并同身份的已有账号（见 probeImportedAccountUsage）。
		h.triggerImportedAccountUsageProbe(id, "manual_at")
		log.Printf("AT 账号 %d 已加入号池 (id=%d, email=%s)", i+1, id, newAcc.Email)
	}

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, updatedCount, duplicateCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("成功新增 %d 个 AT 账号", successCount)
	if updatedCount > 0 {
		msg += fmt.Sprintf("，%d 个已有账号更新", updatedCount)
	}
	if duplicateCount > 0 {
		msg += fmt.Sprintf("，%d 个重复跳过", duplicateCount)
	}
	if failCount > 0 {
		msg += fmt.Sprintf("，%d 个失败", failCount)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   msg,
		"success":   successCount,
		"updated":   updatedCount,
		"duplicate": duplicateCount,
		"failed":    failCount,
	})
}

// streamAddATAccounts 以 SSE 流式推送 AT 批量添加进度（与 streamAddAccounts 对齐）。
func (h *Handler) streamAddATAccounts(c *gin.Context, req addATAccountReq, tokens []string) {
	setupSSE(c)

	total := len(tokens)
	successCount := 0
	failCount := 0
	updatedCount := 0
	duplicateCount := 0
	sendImportEvent(c, importEvent{
		Type: "progress", Current: 0, Total: total,
		Success: 0, Updated: 0, Duplicate: 0, Failed: 0,
	})

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	existingATs := make(map[string]bool)
	seenAT := make(map[string]bool)
	if !req.AllowDuplicate {
		if got, err := h.db.GetAllAccessTokens(ctx); err != nil {
			log.Printf("查询已有 AT 失败: %v", err)
		} else {
			existingATs = got
		}
	}

	progress := func(current int) {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: current, Total: total,
			Success: successCount, Updated: updatedCount, Duplicate: duplicateCount, Failed: failCount,
		})
	}

	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if total > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		seed := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: at, allowDuplicate: req.AllowDuplicate, customHeaders: req.CustomHeaders})
		if !req.AllowDuplicate && seed.email != "" && (seed.accountID != "" || seed.userID != "") {
			id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, req.ProxyURL, seed, "manual_at")
			if err != nil {
				log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
				failCount++
			} else if updated {
				// 已有账号只更新凭证，不计入"新增"（重复添加时新增应为 0）。
				updatedCount++
				log.Printf("AT 账号 %d 命中已有身份并更新凭证 (id=%d)", i+1, id)
			} else {
				successCount++
				log.Printf("AT 账号 %d 已加入号池 (id=%d)", i+1, id)
			}
			progress(i + 1)
			continue
		}

		if !req.AllowDuplicate {
			if existingATs[at] || seenAT[at] {
				duplicateCount++
				progress(i + 1)
				continue
			}
			seenAT[at] = true
		}

		id, err := h.db.InsertATAccount(ctx, name, at, req.ProxyURL)
		if err != nil {
			log.Printf("添加 AT 账号 %d 失败: %v", i+1, err)
			failCount++
			progress(i + 1)
			continue
		}

		successCount++
		h.db.InsertAccountEventAsync(id, "added", "manual_at")
		newAcc := accountFromCredentialSeed(id, req.ProxyURL, seed)
		h.store.AddAccount(newAcc)
		if creds := tokenCredentialMap(seed); len(creds) > 0 {
			if err := h.db.UpdateCredentials(ctx, id, creds); err != nil {
				log.Printf("AT 账号 %d 更新 credentials 失败: %v", id, err)
			}
		}
		// 与非流式路径一致：探针补齐 codex_at 身份后回查合并同身份的已有账号。
		h.triggerImportedAccountUsageProbe(id, "manual_at")
		progress(i + 1)
	}

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, updatedCount, duplicateCount, failCount, c.ClientIP()))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: successCount, Updated: updatedCount, Duplicate: duplicateCount, Failed: failCount,
	})
}

type addOpenAIResponsesAccountReq struct {
	Name          string            `json:"name"`
	BaseURL       string            `json:"base_url"`
	APIKey        string            `json:"api_key"`
	Models        []string          `json:"models"`
	ModelMapping  string            `json:"model_mapping"`
	ProxyURL      string            `json:"proxy_url"`
	CustomHeaders map[string]string `json:"custom_headers"`
}

type fetchOpenAIResponsesModelsReq struct {
	AccountID int64  `json:"account_id"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	ProxyURL  string `json:"proxy_url"`
}

func (h *Handler) AddOpenAIResponsesAccount(c *gin.Context) {
	var req addOpenAIResponsesAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	req.APIKey = strings.TrimSpace(req.APIKey)
	baseURL, err := auth.NormalizeOpenAIResponsesBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	models := auth.NormalizeOpenAIResponsesModels(req.Models)

	if req.APIKey == "" {
		writeError(c, http.StatusBadRequest, "API Key 是必填字段")
		return
	}
	if len(models) == 0 {
		writeError(c, http.StatusBadRequest, "至少需要添加一个模型")
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
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	existing, err := h.db.GetAllOpenAIAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if existing[req.APIKey] {
		writeError(c, http.StatusConflict, "该 API Key 已存在")
		return
	}

	name := req.Name
	if name == "" {
		name = "openai-responses"
	}
	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"base_url":      baseURL,
		"api_key":       req.APIKey,
		"models":        models,
		"model_mapping": modelMapping,
		"plan_type":     "api",
		"email":         baseURL,
	}
	if len(customHeaders) > 0 {
		credentials["custom_headers"] = cloneCustomHeaders(customHeaders)
	}
	id, err := h.db.InsertOpenAIResponsesAccount(ctx, name, credentials, req.ProxyURL)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.db.InsertAccountEventAsync(id, "added", "manual_openai_responses")

	h.store.AddAccount(&auth.Account{
		DBID:          id,
		ProxyURL:      req.ProxyURL,
		HealthTier:    auth.HealthTierHealthy,
		UpstreamType:  auth.UpstreamOpenAIResponses,
		BaseURL:       baseURL,
		APIKey:        req.APIKey,
		Models:        models,
		ModelMapping:  modelMapping,
		CustomHeaders: customHeaders,
		Email:         baseURL,
		PlanType:      "api",
	})

	security.SecurityAuditLog("OPENAI_RESPONSES_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d models=%d ip=%s", id, len(models), c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"message": "成功添加 OpenAI Responses API 账号",
		"id":      id,
	})
}

func (h *Handler) FetchOpenAIResponsesModels(c *gin.Context) {
	var req fetchOpenAIResponsesModelsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	req.APIKey = strings.TrimSpace(req.APIKey)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if req.AccountID > 0 && req.APIKey == "" {
		row, err := h.db.GetAccountByID(c.Request.Context(), req.AccountID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusNotFound, "账号不存在")
				return
			}
			writeInternalError(c, err)
			return
		}
		if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses) {
			writeError(c, http.StatusBadRequest, "仅 OpenAI Responses API 账号支持使用已保存的 API Key 获取模型")
			return
		}
		req.APIKey = row.GetCredential("api_key")
		if strings.TrimSpace(req.BaseURL) == "" {
			req.BaseURL = row.GetCredential("base_url")
		}
		if strings.TrimSpace(req.ProxyURL) == "" {
			req.ProxyURL = row.ProxyURL
		}
	}
	baseURL, err := auth.NormalizeOpenAIResponsesBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.APIKey == "" {
		writeError(c, http.StatusBadRequest, "API Key 是必填字段")
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	models, err := fetchOpenAIResponsesModelIDs(ctx, baseURL, req.APIKey, req.ProxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"models":   models,
		"base_url": baseURL,
	})
}

func (h *Handler) UpdateOpenAIResponsesAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req addOpenAIResponsesAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	req.APIKey = strings.TrimSpace(req.APIKey)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses) {
		writeError(c, http.StatusBadRequest, "仅 OpenAI Responses API 账号支持账号设置")
		return
	}

	baseURL, err := auth.NormalizeOpenAIResponsesBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	models := auth.NormalizeOpenAIResponsesModels(req.Models)
	if len(models) == 0 {
		writeError(c, http.StatusBadRequest, "至少需要添加一个模型")
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
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}

	name := req.Name
	if name == "" {
		name = row.Name
	}
	if name == "" {
		name = "openai-responses"
	}

	credentials := map[string]interface{}{
		"upstream_type":  auth.UpstreamOpenAIResponses,
		"base_url":       baseURL,
		"models":         models,
		"model_mapping":  modelMapping,
		"plan_type":      "api",
		"email":          baseURL,
		"custom_headers": cloneCustomHeaders(customHeaders),
	}
	if req.APIKey != "" {
		credentials["api_key"] = req.APIKey
	}
	if req.APIKey == "" && strings.TrimSpace(row.GetCredential("api_key")) == "" {
		writeError(c, http.StatusBadRequest, "API Key 是必填字段")
		return
	}

	if err := h.db.UpdateOpenAIResponsesAccount(ctx, id, name, credentials, req.ProxyURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if h.store != nil {
		h.store.ApplyOpenAIResponsesConfig(id, baseURL, req.APIKey, models, modelMapping, req.ProxyURL)
		h.store.ApplyAccountCustomHeaders(id, customHeaders)
	}
	h.db.InsertAccountEventAsync(id, "updated", "manual_openai_responses")

	writeMessage(c, http.StatusOK, "OpenAI Responses API 账号设置已更新")
}

func fetchOpenAIResponsesModelIDs(ctx context.Context, baseURL, apiKey, proxyURL string) ([]string, error) {
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/models")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
		return nil, fmt.Errorf("代理URL无效: %w", err)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建模型列表请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 /v1/models 失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if message == "" {
			message = strings.TrimSpace(string(body))
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("/v1/models 返回 %d: %s", resp.StatusCode, message)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 /v1/models 响应失败: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		models = append(models, item.ID)
	}
	models = auth.NormalizeOpenAIResponsesModels(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("/v1/models 未返回可用模型")
	}
	return models, nil
}

// importToken 导入时的统一 token 载体
type importToken struct {
	refreshToken          string
	sessionToken          string
	accessToken           string // AT-only 兼容路径
	name                  string
	email                 string
	idToken               string
	accountID             string
	chatgptAccountID      string // sub2api 等导出格式中的 ChatGPT 账号唯一标识，用于精确去重
	planType              string
	expiresAt             string
	codex7DUsedPercent    string
	codex7DResetAt        string
	codex5HUsedPercent    string
	codex5HResetAt        string
	codex5HUsageUpdatedAt string
	codexUsageUpdatedAt   string
}

// jsonAccountEntry CLIProxyAPI 凭证 JSON 条目
type jsonAccountEntry struct {
	RefreshToken          string                 `json:"refresh_token"`
	SessionToken          string                 `json:"session_token"`
	SessionTokenCamel     string                 `json:"sessionToken"`
	AccessToken           string                 `json:"access_token"`
	AccessTokenCamel      string                 `json:"accessToken"`
	IDToken               string                 `json:"id_token"`
	IDTokenCamel          string                 `json:"idToken"`
	AccountID             string                 `json:"account_id"`
	ChatGPTAccountID      string                 `json:"chatgpt_account_id"`
	Email                 string                 `json:"email"`
	Name                  string                 `json:"name"`
	PlanType              string                 `json:"plan_type"`
	PlanTypeCamel         string                 `json:"planType"`
	User                  jsonAccountUser        `json:"user"`
	Account               jsonAccountAccount     `json:"account"`
	Expired               importJSONScalarString `json:"expired"`
	ExpiresAt             importJSONScalarString `json:"expires_at"`
	Expires               importJSONScalarString `json:"expires"`
	Codex7DUsedPercent    importJSONScalarString `json:"codex_7d_used_percent"`
	Codex7DResetAt        string                 `json:"codex_7d_reset_at"`
	Codex5HUsedPercent    importJSONScalarString `json:"codex_5h_used_percent"`
	Codex5HResetAt        string                 `json:"codex_5h_reset_at"`
	Codex5HUsageUpdatedAt string                 `json:"codex_5h_usage_updated_at"`
	CodexUsageUpdatedAt   string                 `json:"codex_usage_updated_at"`
}

type jsonAccountUser struct {
	Email string `json:"email"`
	ID    string `json:"id"`
	Name  string `json:"name"`
}

type jsonAccountAccount struct {
	PlanType      string `json:"plan_type"`
	PlanTypeCamel string `json:"planType"`
	ID            string `json:"id"`
}

type sub2apiImportPayload struct {
	Accounts []sub2apiAccountEntry `json:"accounts"`
}

type sub2apiAccountEntry struct {
	Name        string                    `json:"name"`
	Credentials sub2apiAccountCredentials `json:"credentials"`
}

type sub2apiAccountCredentials struct {
	RefreshToken          string                 `json:"refresh_token"`
	SessionToken          string                 `json:"session_token"`
	SessionTokenCamel     string                 `json:"sessionToken"`
	AccessToken           string                 `json:"access_token"`
	AccessTokenCamel      string                 `json:"accessToken"`
	IDToken               string                 `json:"id_token"`
	IDTokenCamel          string                 `json:"idToken"`
	AccountID             string                 `json:"account_id"`
	ChatGPTAccountID      string                 `json:"chatgpt_account_id"`
	Email                 string                 `json:"email"`
	PlanType              string                 `json:"plan_type"`
	PlanTypeCamel         string                 `json:"planType"`
	User                  jsonAccountUser        `json:"user"`
	Account               jsonAccountAccount     `json:"account"`
	ExpiresAt             importJSONScalarString `json:"expires_at"`
	Expired               importJSONScalarString `json:"expired"`
	Expires               importJSONScalarString `json:"expires"`
	Codex7DUsedPercent    importJSONScalarString `json:"codex_7d_used_percent"`
	Codex7DResetAt        string                 `json:"codex_7d_reset_at"`
	Codex5HUsedPercent    importJSONScalarString `json:"codex_5h_used_percent"`
	Codex5HResetAt        string                 `json:"codex_5h_reset_at"`
	Codex5HUsageUpdatedAt string                 `json:"codex_5h_usage_updated_at"`
	CodexUsageUpdatedAt   string                 `json:"codex_usage_updated_at"`
}

type importJSONScalarString string

func (v *importJSONScalarString) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var raw interface{}
	if err := decoder.Decode(&raw); err != nil {
		return err
	}

	switch value := raw.(type) {
	case string:
		*v = importJSONScalarString(strings.TrimSpace(value))
	case json.Number:
		*v = importJSONScalarString(value.String())
	case bool:
		*v = importJSONScalarString(strconv.FormatBool(value))
	default:
		*v = ""
	}

	return nil
}

func (v importJSONScalarString) String() string {
	return strings.TrimSpace(string(v))
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
		st := firstNonEmpty(entry.SessionToken, entry.SessionTokenCamel)
		at := firstNonEmpty(entry.AccessToken, entry.AccessTokenCamel)
		idTok := firstNonEmpty(entry.IDToken, entry.IDTokenCamel)
		email := firstNonEmpty(entry.Email, entry.User.Email)
		name := firstNonEmpty(entry.Name, entry.User.Name, email)
		planType := firstNonEmpty(entry.PlanType, entry.PlanTypeCamel, entry.Account.PlanType, entry.Account.PlanTypeCamel)
		accID := firstNonEmpty(entry.AccountID, entry.User.ID, entry.Account.ID)
		expiresAt := firstNonEmpty(entry.ExpiresAt.String(), entry.Expired.String(), entry.Expires.String())

		if rt != "" || st != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken:          rt,
				sessionToken:          st,
				accessToken:           at,
				name:                  name,
				email:                 email,
				idToken:               idTok,
				accountID:             strings.TrimSpace(entry.AccountID),
				chatgptAccountID:      firstNonEmpty(entry.ChatGPTAccountID, accID),
				planType:              planType,
				expiresAt:             expiresAt,
				codex7DUsedPercent:    strings.TrimSpace(entry.Codex7DUsedPercent.String()),
				codex7DResetAt:        strings.TrimSpace(entry.Codex7DResetAt),
				codex5HUsedPercent:    strings.TrimSpace(entry.Codex5HUsedPercent.String()),
				codex5HResetAt:        strings.TrimSpace(entry.Codex5HResetAt),
				codex5HUsageUpdatedAt: strings.TrimSpace(entry.Codex5HUsageUpdatedAt),
				codexUsageUpdatedAt:   strings.TrimSpace(entry.CodexUsageUpdatedAt),
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
		c := account.Credentials
		rt := strings.TrimSpace(c.RefreshToken)
		st := firstNonEmpty(c.SessionToken, c.SessionTokenCamel)
		at := firstNonEmpty(c.AccessToken, c.AccessTokenCamel)
		idTok := firstNonEmpty(c.IDToken, c.IDTokenCamel)
		name := firstNonEmpty(account.Name, c.User.Name)
		email := firstNonEmpty(c.Email, c.User.Email)

		if name == "" {
			name = email
		}
		planType := firstNonEmpty(c.PlanType, c.PlanTypeCamel, c.Account.PlanType, c.Account.PlanTypeCamel)
		accID := firstNonEmpty(c.AccountID, c.User.ID, c.Account.ID)
		expiresAt := firstNonEmpty(c.ExpiresAt.String(), c.Expired.String(), c.Expires.String())

		if rt != "" || st != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken:          rt,
				sessionToken:          st,
				accessToken:           at,
				name:                  name,
				email:                 email,
				idToken:               idTok,
				accountID:             strings.TrimSpace(c.AccountID),
				chatgptAccountID:      firstNonEmpty(c.ChatGPTAccountID, accID),
				planType:              planType,
				expiresAt:             expiresAt,
				codex7DUsedPercent:    strings.TrimSpace(c.Codex7DUsedPercent.String()),
				codex7DResetAt:        strings.TrimSpace(c.Codex7DResetAt),
				codex5HUsedPercent:    strings.TrimSpace(c.Codex5HUsedPercent.String()),
				codex5HResetAt:        strings.TrimSpace(c.Codex5HResetAt),
				codex5HUsageUpdatedAt: strings.TrimSpace(c.Codex5HUsageUpdatedAt),
				codexUsageUpdatedAt:   strings.TrimSpace(c.CodexUsageUpdatedAt),
			})
		}
	}

	return tokens
}

func importTokenCredentialIdentity(t importToken) string {
	switch {
	case t.refreshToken != "":
		return "rt:" + t.refreshToken
	case t.sessionToken != "":
		return "st:" + t.sessionToken
	case t.accessToken != "":
		return "at:" + t.accessToken
	default:
		return ""
	}
}

func importCredentialFingerprint(refreshToken, sessionToken, accessToken string) string {
	return strings.TrimSpace(refreshToken) + "\x00" + strings.TrimSpace(sessionToken) + "\x00" + strings.TrimSpace(accessToken)
}

func importTokenCredentialFingerprint(t importToken, conflicts map[string]bool) string {
	seed := importTokenSeed(t, conflicts)
	return importCredentialFingerprint(seed.refreshToken, seed.sessionToken, seed.accessToken)
}

func importAccountCredentialFingerprint(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	return importCredentialFingerprint(
		row.GetCredential("refresh_token"),
		row.GetCredential("session_token"),
		row.GetCredential("access_token"),
	)
}

func conflictingImportChatGPTIDs(tokens []importToken) map[string]bool {
	identitiesByID := make(map[string]map[string]struct{})
	for _, t := range tokens {
		id := strings.TrimSpace(t.chatgptAccountID)
		if id == "" {
			continue
		}
		identity := importTokenCredentialIdentity(t)
		if identity == "" {
			continue
		}
		identities := identitiesByID[id]
		if identities == nil {
			identities = make(map[string]struct{}, 1)
			identitiesByID[id] = identities
		}
		identities[identity] = struct{}{}
	}

	conflicts := make(map[string]bool)
	for id, identities := range identitiesByID {
		if len(identities) > 1 {
			conflicts[id] = true
		}
	}
	return conflicts
}

func reliableImportChatGPTID(t importToken, conflicts map[string]bool) string {
	id := strings.TrimSpace(t.chatgptAccountID)
	if id == "" || conflicts[id] {
		return ""
	}
	return id
}

func importStoredAccountID(t importToken, conflicts map[string]bool) string {
	if strings.TrimSpace(t.accountID) != "" {
		return strings.TrimSpace(t.accountID)
	}
	return reliableImportChatGPTID(t, conflicts)
}

func importTokenSeed(t importToken, conflicts map[string]bool) tokenCredentialSeed {
	return normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken:          t.refreshToken,
		sessionToken:          t.sessionToken,
		accessToken:           t.accessToken,
		idToken:               t.idToken,
		accountID:             importStoredAccountID(t, conflicts),
		email:                 t.email,
		planType:              t.planType,
		expiresAtRaw:          t.expiresAt,
		codex7DUsedPercent:    t.codex7DUsedPercent,
		codex7DResetAt:        t.codex7DResetAt,
		codex5HUsedPercent:    t.codex5HUsedPercent,
		codex5HResetAt:        t.codex5HResetAt,
		codex5HUsageUpdatedAt: t.codex5HUsageUpdatedAt,
		codexUsageUpdatedAt:   t.codexUsageUpdatedAt,
	})
}

func importTokenOAuthIdentityKey(t importToken, conflicts map[string]bool) string {
	seed := importTokenSeed(t, conflicts)
	email := strings.ToLower(strings.TrimSpace(seed.email))
	accountID := strings.TrimSpace(seed.accountID)
	if accountID == "" && strings.TrimSpace(t.accountID) == "" {
		accountID = strings.TrimSpace(t.chatgptAccountID)
	}
	// 个人账号可能只有 user_id（无工作区 account_id），用它兜底做身份键，
	// 否则文件导入会退化为凭证原文比对，AT 轮换后重复导入。
	if accountID == "" {
		accountID = strings.TrimSpace(seed.userID)
	}
	if email == "" || accountID == "" {
		return ""
	}
	return email + "\x00" + accountID
}

// ImportAccounts 批量导入账号（支持 TXT / JSON）
func (h *Handler) ImportAccounts(c *gin.Context) {
	format := c.DefaultPostForm("format", "txt")
	proxyURL := c.PostForm("proxy_url")
	allowDuplicate := parseBoolForm(c.PostForm("allow_duplicate"))
	customHeaders, err := parseCustomHeadersForm(c.PostForm("custom_headers"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	switch format {
	case "json":
		h.importAccountsJSON(c, proxyURL, allowDuplicate, customHeaders)
	case "json_at":
		h.importAccountsJSONPreferAT(c, proxyURL, allowDuplicate, customHeaders)
	case "at_txt":
		h.importAccountsATTXT(c, proxyURL, allowDuplicate, customHeaders)
	default:
		h.importAccountsTXT(c, proxyURL, allowDuplicate, customHeaders)
	}
}

// parseBoolForm 解析表单中的布尔开关（1/true/yes/on 视为真）。
func parseBoolForm(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseCustomHeadersForm(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(trimmed), &headers); err != nil {
		return nil, fmt.Errorf("custom_headers 必须是 JSON 对象")
	}
	return normalizeCustomHeaders(headers)
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
		if err := validateImportFileSize(fh); err != nil {
			return nil, err
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
func (h *Handler) importAccountsTXT(c *gin.Context, proxyURL string, allowDuplicate bool, customHeaders ...map[string]string) {
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

	h.importAccountsCommon(c, tokens, proxyURL, allowDuplicate, firstCustomHeaders(customHeaders))
}

// importAccountsJSON 通过 JSON 文件导入（兼容 CLIProxyAPI 凭证格式）
func (h *Handler) importAccountsJSON(c *gin.Context, proxyURL string, allowDuplicate bool, customHeaders ...map[string]string) {
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
		if err := validateImportFileSize(fh); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
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

	h.importAccountsCommon(c, allTokens, proxyURL, allowDuplicate, firstCustomHeaders(customHeaders))
}

// importAccountsJSONPreferAT 通过 JSON 文件导入，但只信任 access_token，
// 用于一些导出工具中 refresh_token / session_token 是占位/重复值的场景。
func (h *Handler) importAccountsJSONPreferAT(c *gin.Context, proxyURL string, allowDuplicate bool, customHeaders ...map[string]string) {
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
		if err := validateImportFileSize(fh); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
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

		for _, t := range tokens {
			if strings.TrimSpace(t.accessToken) == "" {
				continue
			}
			t.refreshToken = ""
			t.sessionToken = ""
			allTokens = append(allTokens, t)
		}
	}

	if len(allTokens) == 0 {
		writeError(c, http.StatusBadRequest, "JSON 文件中未找到有效的 access_token")
		return
	}

	h.importAccountsCommon(c, allTokens, proxyURL, allowDuplicate, firstCustomHeaders(customHeaders))
}

func firstCustomHeaders(headers []map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	return headers[0]
}

// importEvent SSE 导入进度事件
type importEvent struct {
	Type      string `json:"type"` // progress | complete
	Current   int    `json:"current"`
	Total     int    `json:"total"`
	Success   int    `json:"success"`
	Updated   int    `json:"updated"`
	Duplicate int    `json:"duplicate"`
	Failed    int    `json:"failed"`
}

func sendImportEvent(c *gin.Context, e importEvent) {
	sendSSEJSON(c, e)
}

func setupSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

func sendSSEJSON(c *gin.Context, event any) {
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("序列化 SSE 事件失败: %v", err)
		return
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		log.Printf("写入 SSE 事件失败: %v", err)
		return
	}
	c.Writer.Flush()
}

// importAccountsCommon 公共的去重、并发插入、SSE 进度推送逻辑（支持 RT 和 AT-only 混合导入）
func (h *Handler) importAccountsCommon(c *gin.Context, tokens []importToken, proxyURL string, allowDuplicate bool, customHeaders ...map[string]string) {
	importCustomHeaders := firstCustomHeaders(customHeaders)
	// 文件内去重：
	// 1) 当条目可解析出 email + account_id 时，以它作为 OAuth 身份键；
	//    同身份同 RT/ST/AT 折叠，同身份不同 RT/ST/AT 整组跳过，避免任选一个覆盖。
	// 2) 没有 OAuth 身份时，退回到 RT / ST / AT 顺序去重（兼容旧导出格式）。
	// 3) 同一份文件内若出现"同一个 RT 对应多个不同 chatgpt_account_id"，
	//    会被全部保留为独立账号；数据库层面 refresh_token 没有 UNIQUE 约束，因此安全。
	conflictingChatGPTIDs := conflictingImportChatGPTIDs(tokens)
	type oauthIdentityImportState struct {
		count        int
		fingerprints map[string]struct{}
	}
	oauthIdentityStates := make(map[string]*oauthIdentityImportState)
	for _, t := range tokens {
		oauthIdentity := importTokenOAuthIdentityKey(t, conflictingChatGPTIDs)
		if oauthIdentity == "" {
			continue
		}
		state := oauthIdentityStates[oauthIdentity]
		if state == nil {
			state = &oauthIdentityImportState{fingerprints: make(map[string]struct{}, 1)}
			oauthIdentityStates[oauthIdentity] = state
		}
		state.count++
		state.fingerprints[importTokenCredentialFingerprint(t, conflictingChatGPTIDs)] = struct{}{}
	}

	seenOAuthIdentity := make(map[string]bool)
	seenRT := make(map[string]bool)
	seenST := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []importToken
	ambiguousOAuthIdentityCount := 0
	for _, t := range tokens {
		oauthIdentity := importTokenOAuthIdentityKey(t, conflictingChatGPTIDs)
		if oauthIdentity != "" {
			state := oauthIdentityStates[oauthIdentity]
			if state != nil && len(state.fingerprints) > 1 {
				if !seenOAuthIdentity[oauthIdentity] {
					ambiguousOAuthIdentityCount += state.count
					seenOAuthIdentity[oauthIdentity] = true
				}
				continue
			}
			if seenOAuthIdentity[oauthIdentity] {
				continue
			}
			seenOAuthIdentity[oauthIdentity] = true
			if t.refreshToken != "" {
				seenRT[t.refreshToken] = true
			}
			if t.sessionToken != "" {
				seenST[t.sessionToken] = true
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
			unique = append(unique, t)
			continue
		}
		if t.refreshToken != "" {
			if !seenRT[t.refreshToken] {
				seenRT[t.refreshToken] = true
				unique = append(unique, t)
			}
			if t.sessionToken != "" {
				seenST[t.sessionToken] = true
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
		} else if t.sessionToken != "" {
			if !seenST[t.sessionToken] {
				seenST[t.sessionToken] = true
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

	fileDuplicateCount := len(tokens) - len(unique) - ambiguousOAuthIdentityCount
	if fileDuplicateCount < 0 {
		fileDuplicateCount = 0
	}
	log.Printf("导入解析: 文件内 %d 条, 去重后 %d 条（%d 条文件内重复，%d 条 OAuth 身份冲突跳过）", len(tokens), len(unique), fileDuplicateCount, ambiguousOAuthIdentityCount)

	var newTokens []importToken
	duplicateCount := ambiguousOAuthIdentityCount

	if allowDuplicate {
		// 允许重复添加：跳过数据库去重，所有解析出的 token 均作为新账号导入。
		newTokens = tokens
		duplicateCount = 0
	} else {
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
		hasST := len(seenST) > 0
		var existingSTs map[string]bool
		if hasST {
			existingSTs, err = h.db.GetAllSessionTokens(dedupeCtx)
			if err != nil {
				log.Printf("查询已有 ST 失败: %v", err)
				existingSTs = make(map[string]bool)
			}
		}

		for _, t := range unique {
			oauthIdentity := importTokenOAuthIdentityKey(t, conflictingChatGPTIDs)
			if oauthIdentity != "" {
				seed := importTokenSeed(t, conflictingChatGPTIDs)
				if duplicateID, err := h.findOAuthIdentityDuplicate(dedupeCtx, seed, 0); err != nil {
					log.Printf("查询已有 OAuth 身份失败: %v", err)
				} else if duplicateID > 0 {
					row, err := h.db.GetAccountByID(dedupeCtx, duplicateID)
					if err != nil {
						log.Printf("查询已有 OAuth 账号 %d 失败: %v", duplicateID, err)
					} else if importAccountCredentialFingerprint(row) == importTokenCredentialFingerprint(t, conflictingChatGPTIDs) {
						duplicateCount++
						continue
					}
				}
				newTokens = append(newTokens, t)
				continue
			}
			switch {
			case t.refreshToken != "":
				if existingRTs[t.refreshToken] {
					duplicateCount++
				} else if t.sessionToken != "" && existingSTs[t.sessionToken] {
					duplicateCount++
				} else if t.accessToken != "" && existingATs[t.accessToken] {
					duplicateCount++
				} else {
					newTokens = append(newTokens, t)
				}
			case t.sessionToken != "":
				if existingSTs[t.sessionToken] {
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
	}

	total := len(unique) + ambiguousOAuthIdentityCount
	if allowDuplicate {
		total = len(tokens)
	}

	log.Printf("导入去重: 总计 %d 条, 数据库已存在 %d 条, 待导入 %d 条", total, duplicateCount, len(newTokens))

	if len(newTokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   fmt.Sprintf("所有 %d 个 Token 已存在或已跳过，无需导入", total),
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
	var updatedCount int64
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
				upd := int(atomic.LoadInt64(&updatedCount))
				fai := int(atomic.LoadInt64(&failCount))
				sendImportEvent(c, importEvent{
					Type: "progress", Current: cur + duplicateCount, Total: total,
					Success: suc, Updated: upd, Duplicate: duplicateCount, Failed: fai,
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

			seed := importTokenSeed(tok, conflictingChatGPTIDs)
			seed.allowDuplicate = allowDuplicate
			seed.customHeaders = cloneCustomHeaders(importCustomHeaders)
			importSource := "import"
			if tok.accessToken != "" && tok.refreshToken == "" {
				importSource = "import_at"
			}
			if !allowDuplicate && seed.email != "" && (seed.accountID != "" || seed.userID != "") {
				if name == "" {
					if importSource == "import_at" {
						name = fmt.Sprintf("at-import-%d", idx+1)
					} else {
						name = fmt.Sprintf("import-%d", idx+1)
					}
				}

				upsertCtx, upsertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, updated, err := h.upsertOAuthIdentityAccount(upsertCtx, name, proxyURL, seed, importSource)
				upsertCancel()
				if err != nil {
					log.Printf("导入账号 %d/%d 更新或写入失败: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				if updated {
					// 已有账号只更新凭证，不计入"新增"。
					atomic.AddInt64(&updatedCount, 1)
				} else {
					atomic.AddInt64(&successCount, 1)
				}
				atomic.AddInt64(&current, 1)
				if h.store != nil {
					if acc := h.store.FindByID(id); acc != nil {
						h.applyImportedAccountUsageState(acc, importSource)
						if acc.GetAccessToken() == "" && !h.store.GetLazyMode() {
							go h.refreshImportedAccountAndProbe(id, importSource+"_refresh")
						}
					}
				}
				return
			}

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

				newAcc := accountFromCredentialSeed(id, proxyURL, seed)
				if len(tokenCredentialMap(seed)) > 0 {
					credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
					_ = h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed))
					credCancel()
				}
				h.store.AddAccount(newAcc)
				h.applyImportedAccountUsageState(newAcc, "import_at")
				if newAcc.GetAccessToken() != "" {
					h.triggerImportedAccountUsageProbe(id, "import_at")
				}
			} else {
				// RT 导入路径；如果导入文件里同时带 AT，则先沿用它，后台调度到期前再刷新。
				if name == "" {
					name = fmt.Sprintf("import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				var id int64
				var err error
				if tok.refreshToken != "" {
					id, err = h.db.InsertAccount(insertCtx, name, tok.refreshToken, proxyURL)
				} else {
					id, err = h.db.InsertAccountWithCredentials(insertCtx, name, tokenCredentialMap(seed), proxyURL)
				}
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

				if len(tokenCredentialMap(seed)) > 0 {
					credCtx, credCancel := context.WithTimeout(context.Background(), 3*time.Second)
					if err := h.db.UpdateCredentials(credCtx, id, tokenCredentialMap(seed)); err != nil {
						log.Printf("导入账号 %d 更新 token 信息失败: %v", id, err)
					}
					credCancel()
				}
				newAcc := accountFromCredentialSeed(id, proxyURL, seed)
				h.store.AddAccount(newAcc)
				h.applyImportedAccountUsageState(newAcc, "import")

				if newAcc.GetAccessToken() != "" {
					h.triggerImportedAccountUsageProbe(id, "import")
				} else if !h.store.GetLazyMode() {
					// 后台异步刷新，不阻塞导入流程；刷新成功后立即做 wham 用量采样。
					go h.refreshImportedAccountAndProbe(id, "import_refresh")
				}
			}
		}(i, t)
	}

	wg.Wait()
	close(done)

	// 发送完成事件
	suc := int(atomic.LoadInt64(&successCount))
	upd := int(atomic.LoadInt64(&updatedCount))
	fai := int(atomic.LoadInt64(&failCount))
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Updated: upd, Duplicate: duplicateCount, Failed: fai,
	})

	log.Printf("导入完成: success=%d, updated=%d, duplicate=%d, failed=%d, total=%d", suc, upd, duplicateCount, fai, total)
}

// importAccountsATTXT 通过 TXT 文件导入 AT-only 账号（每行一个 Access Token）
func (h *Handler) importAccountsATTXT(c *gin.Context, proxyURL string, allowDuplicate bool, customHeaders ...map[string]string) {
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

	h.importAccountsCommon(c, tokens, proxyURL, allowDuplicate, firstCustomHeaders(customHeaders))
}

// GetAccountUsage 查询单个账号的用量统计
func (h *Handler) GetAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	days := 30
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 3650 {
			writeError(c, http.StatusBadRequest, "days 参数无效，需要 0-3650 的整数")
			return
		}
		days = parsed
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	detail, err := h.db.GetAccountUsageStats(ctx, id, days)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// RefreshAccountUsage 同步刷新单个账号的用量快照（优先走零成本的 wham 端点），
// 完成后返回该账号最新的 5h/7d 用量字段，供前端用量列即时更新进度条。
// POST /api/admin/accounts/:id/usage/refresh
func (h *Handler) RefreshAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	if err := h.ProbeUsageSnapshot(ctx, account); err != nil {
		writeError(c, http.StatusBadGateway, fmt.Sprintf("刷新用量失败: %s", err.Error()))
		return
	}

	resp := gin.H{"refreshed": true}
	if pct, ok := account.GetUsagePercent5h(); ok {
		resp["usage_percent_5h"] = pct
	}
	if pct, ok := account.GetUsagePercent7d(); ok {
		resp["usage_percent_7d"] = pct
	}
	if t := account.GetReset5hAt(); !t.IsZero() {
		resp["reset_5h_at"] = t.Format(time.RFC3339)
	}
	if t := account.GetReset7dAt(); !t.IsZero() {
		resp["reset_7d_at"] = t.Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
}

type batchAccountIDsRequest struct {
	IDs []int64 `json:"ids"`
}

type batchUpdateAccountsReq struct {
	updateAccountSchedulerReq
	IDs     []int64 `json:"ids"`
	Enabled *bool   `json:"enabled"`
	Locked  *bool   `json:"locked"`
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
	if err := h.deleteAccountByID(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}

	writeMessage(c, http.StatusOK, "账号已删除")
}

func (h *Handler) deleteAccountByID(ctx context.Context, id int64) error {
	if err := h.db.SoftDeleteAccount(ctx, id); err != nil {
		return err
	}
	h.store.RemoveAccount(id)
	h.db.InsertAccountEventAsync(id, "deleted", "manual")
	return nil
}

type recycleBinAccountResponse struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	Email              string   `json:"email"`
	PlanType           string   `json:"plan_type"`
	ATOnly             bool     `json:"at_only"`
	AccessTokenType    string   `json:"access_token_type,omitempty"`
	OpenAIResponsesAPI bool     `json:"openai_responses_api"`
	BaseURL            string   `json:"base_url,omitempty"`
	Models             []string `json:"models,omitempty"`
	CreatedAt          string   `json:"created_at"`
	DeletedAt          string   `json:"deleted_at,omitempty"`
	LastTestStatus     string   `json:"last_test_status,omitempty"`
	LastTestAt         string   `json:"last_test_at,omitempty"`
}

// ListRecycleBinAccounts 获取回收站账号列表
// GET /api/admin/accounts/recycle-bin
func (h *Handler) ListRecycleBinAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.db.ListDeleted(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	accounts := make([]recycleBinAccountResponse, 0, len(rows))
	for _, row := range rows {
		isOpenAIResponsesAccount := strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses)
		email := row.GetCredential("email")
		baseURL := row.GetCredential("base_url")
		if isOpenAIResponsesAccount && email == "" {
			email = baseURL
		}
		planType := row.GetCredential("plan_type")
		if isOpenAIResponsesAccount && planType == "" {
			planType = "api"
		}
		resp := recycleBinAccountResponse{
			ID:                 row.ID,
			Name:               row.Name,
			Email:              email,
			PlanType:           planType,
			ATOnly:             !isOpenAIResponsesAccount && row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
			AccessTokenType:    accountAccessTokenType(row),
			OpenAIResponsesAPI: isOpenAIResponsesAccount,
			BaseURL:            baseURL,
			Models:             row.GetCredentialStringSlice("models"),
			CreatedAt:          row.CreatedAt.Format(time.RFC3339),
			LastTestStatus:     row.GetCredential("recycle_last_test_status"),
			LastTestAt:         row.GetCredential("recycle_last_test_at"),
		}
		if row.DeletedAt.Valid {
			resp.DeletedAt = row.DeletedAt.Time.Format(time.RFC3339)
		} else if !row.UpdatedAt.IsZero() {
			// 旧数据可能没有 deleted_at；软删除会刷新 updated_at，用它兜底。
			resp.DeletedAt = row.UpdatedAt.Format(time.RFC3339)
		}
		accounts = append(accounts, resp)
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

// RestoreAccount 将回收站中的账号恢复到账号池
// POST /api/admin/accounts/:id/restore
func (h *Handler) RestoreAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.restoreAccountByID(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "回收站中不存在该账号")
			return
		}
		if errors.Is(err, errDuplicateOAuthIdentity) {
			writeError(c, http.StatusConflict, "恢复失败: "+err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "账号已恢复")
}

// restoreAccountByID 将回收站账号恢复为 active 并重新加入运行时池。
func (h *Handler) restoreAccountByID(ctx context.Context, id int64) error {
	row, err := h.db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil {
		return err
	}
	seed := tokenCredentialSeedFromAccountRow(row)
	if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, id); err != nil {
		return err
	} else if duplicateID > 0 {
		return fmt.Errorf("%w: 已存在相同 OAuth 账号 (id=%d)，请先删除正常账号或清理回收站账号", errDuplicateOAuthIdentity, duplicateID)
	}

	if err := h.db.RestoreAccount(ctx, id); err != nil {
		return err
	}
	if h.store != nil {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			log.Printf("恢复账号 %d 后加载运行时失败: %v", id, err)
			return fmt.Errorf("恢复账号后加载运行时失败: %w", err)
		}
	}
	h.db.InsertAccountEventAsync(id, "restored", "recycle_bin")
	return nil
}

func tokenCredentialSeedFromAccountRow(row *database.AccountRow) tokenCredentialSeed {
	if row == nil {
		return tokenCredentialSeed{}
	}
	return normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken:          row.GetCredential("refresh_token"),
		sessionToken:          row.GetCredential("session_token"),
		accessToken:           row.GetCredential("access_token"),
		idToken:               row.GetCredential("id_token"),
		accountID:             firstNonEmpty(row.GetCredential("account_id"), row.GetCredential("chatgpt_account_id")),
		email:                 row.GetCredential("email"),
		planType:              row.GetCredential("plan_type"),
		expiresAtRaw:          row.GetCredential("expires_at"),
		codex7DUsedPercent:    row.GetCredential("codex_7d_used_percent"),
		codex7DResetAt:        row.GetCredential("codex_7d_reset_at"),
		codex5HUsedPercent:    row.GetCredential("codex_5h_used_percent"),
		codex5HResetAt:        row.GetCredential("codex_5h_reset_at"),
		codex5HUsageUpdatedAt: row.GetCredential("codex_5h_usage_updated_at"),
		codexUsageUpdatedAt:   row.GetCredential("codex_usage_updated_at"),
	})
}

// PurgeAccount 从回收站彻底删除账号（物理删除）
// DELETE /api/admin/accounts/:id/purge
func (h *Handler) PurgeAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.PurgeAccount(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "回收站中不存在该账号")
			return
		}
		writeError(c, http.StatusInternalServerError, "彻底删除失败: "+err.Error())
		return
	}
	h.store.RemoveAccount(id)
	security.SecurityAuditLog("ACCOUNT_PURGED", fmt.Sprintf("account_id=%d ip=%s", id, c.ClientIP()))
	writeMessage(c, http.StatusOK, "账号已彻底删除")
}

// emptyRecycleBinConfirmToken 清空回收站的确认令牌；调用方必须在请求体中
// 显式携带，防止误调用或脚本一键清空导致账号被不可逆地物理删除。
const emptyRecycleBinConfirmToken = "EMPTY-RECYCLE-BIN"

// EmptyRecycleBin 清空回收站
// DELETE /api/admin/accounts/recycle-bin
// 请求体必须携带 {"confirm":"EMPTY-RECYCLE-BIN"}，否则拒绝执行。
func (h *Handler) EmptyRecycleBin(c *gin.Context) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	if strings.TrimSpace(req.Confirm) != emptyRecycleBinConfirmToken {
		writeError(c, http.StatusBadRequest, `清空回收站需要确认：请求体需携带 confirm="EMPTY-RECYCLE-BIN"`)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	purged, err := h.db.PurgeDeletedAccounts(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "清空回收站失败: "+err.Error())
		return
	}
	security.SecurityAuditLog("RECYCLE_BIN_EMPTIED", fmt.Sprintf("purged=%d ip=%s", purged, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"message": "回收站已清空", "purged": purged})
}

// BatchDeleteAccounts 批量删除账号；stream=true 时以 SSE 返回实时进度。
// POST /api/admin/accounts/batch-delete
func (h *Handler) BatchDeleteAccounts(c *gin.Context) {
	var req batchAccountIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要删除的账号 ID 列表")
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchDeleteAccounts(c, ids)
		return
	}

	success, fail := h.runBatchDeleteAccounts(c.Request.Context(), ids, nil)
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已删除 %d 个账号，失败 %d 个", success, fail),
		"deleted": success,
		"success": success,
		"failed":  fail,
	})
}

func uniqueAccountIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func (h *Handler) streamBatchDeleteAccounts(c *gin.Context, ids []int64) {
	setupSSE(c)
	total := len(ids)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_delete", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_delete"})
		return
	}

	success, fail := h.runBatchDeleteAccounts(c.Request.Context(), ids, func(event batchOperationEvent) {
		sendSSEJSON(c, event)
	})
	sendSSEJSON(c, batchOperationEvent{
		Type:    "complete",
		Action:  "batch_delete",
		Current: total,
		Total:   total,
		Success: success,
		Failed:  fail,
		Deleted: success,
	})
}

func (h *Handler) runBatchDeleteAccounts(ctx context.Context, ids []int64, onProgress func(batchOperationEvent)) (int64, int64) {
	total := len(ids)
	var success int64
	var fail int64

	for i, id := range ids {
		if ctx.Err() != nil {
			fail += int64(total - i)
			break
		}

		err := h.deleteAccountByID(ctx, id)
		event := batchOperationEvent{
			Type:      "progress",
			Action:    "batch_delete",
			Current:   i + 1,
			Total:     total,
			AccountID: id,
		}
		if err != nil {
			fail++
			event.Error = err.Error()
			if errors.Is(err, sql.ErrNoRows) {
				event.Error = "账号不存在"
			}
		} else {
			success++
			event.Deleted = success
			event.Message = "账号已删除"
		}
		event.Success = success
		event.Failed = fail
		if onProgress != nil {
			onProgress(event)
		}
	}

	return success, fail
}

// BatchUpdateAccounts 批量更新账号启用、锁定和调度元信息。
// POST /api/admin/accounts/batch-update
func (h *Handler) BatchUpdateAccounts(c *gin.Context) {
	var req batchUpdateAccountsReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要更新的账号 ID 列表")
		return
	}

	schedulerUpdate, err := parseAccountSchedulerUpdate(req.updateAccountSchedulerReq)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	enabled := optionalBoolFromPtr(req.Enabled)
	locked := optionalBoolFromPtr(req.Locked)
	if !enabled.Set && !locked.Set && !schedulerUpdate.hasChanges() {
		writeError(c, http.StatusBadRequest, "请提供要更新的字段")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if schedulerUpdate.AllowedAPIKeyIDs.Set {
		missingAPIKeyIDs, err := h.findMissingAPIKeyIDs(ctx, schedulerUpdate.AllowedAPIKeyIDs.Values)
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
	if schedulerUpdate.GroupIDs.Set {
		missingGroupIDs, err := h.db.VerifyAccountGroupIDs(ctx, schedulerUpdate.GroupIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "校验账号分组失败: "+err.Error())
			return
		}
		if len(missingGroupIDs) > 0 {
			values := make([]string, 0, len(missingGroupIDs))
			for _, value := range missingGroupIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "group_ids 包含不存在的分组 ID: "+strings.Join(values, ", "))
			return
		}
	}

	updatedIDs, err := h.db.BatchUpdateAccountMetadata(ctx, ids, database.BatchAccountMetadataUpdate{
		Enabled:                 enabled,
		Locked:                  locked,
		ScoreBiasOverride:       schedulerUpdate.ScoreBiasOverride,
		BaseConcurrencyOverride: schedulerUpdate.BaseConcurrencyOverride,
		SkipWarmTier:            schedulerUpdate.SkipWarmTier,
		AllowedAPIKeyIDs:        schedulerUpdate.AllowedAPIKeyIDs,
		Tags:                    database.OptionalStringSlice{Set: schedulerUpdate.Tags.Set, Values: schedulerUpdate.Tags.Values},
		GroupIDs:                schedulerUpdate.GroupIDs,
		ProxyURL:                schedulerUpdate.ProxyURL,
		CredentialUpdates:       schedulerUpdate.CredentialUpdates,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "批量更新账号失败: "+err.Error())
		return
	}

	if h.store != nil {
		for _, id := range updatedIDs {
			if enabled.Set {
				h.store.ApplyAccountEnabled(id, enabled.Value)
			}
			if locked.Set {
				if acc := h.store.FindByID(id); acc != nil {
					if locked.Value {
						atomic.StoreInt32(&acc.Locked, 1)
					} else {
						atomic.StoreInt32(&acc.Locked, 0)
					}
				}
			}
			h.applyAccountSchedulerRuntimeUpdate(id, schedulerUpdate)
		}
	}

	success := int64(len(updatedIDs))
	failed := int64(len(ids)) - success
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已更新 %d 个账号，失败 %d 个", success, failed),
		"success": success,
		"failed":  failed,
	})
}

// RefreshAccount 手动刷新账号 AT
func (h *Handler) RefreshAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	if err := h.refreshAccountByID(c.Request.Context(), id); err != nil {
		if strings.Contains(err.Error(), "不存在") {
			writeError(c, http.StatusNotFound, err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "刷新失败: "+err.Error())
		return
	}

	writeMessage(c, http.StatusOK, "账号刷新成功")
}

// BatchRefreshAccounts 批量刷新账号 AT；stream=true 时以 SSE 返回实时进度。
// POST /api/admin/accounts/batch-refresh
func (h *Handler) BatchRefreshAccounts(c *gin.Context) {
	var req batchAccountIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	ids := uniqueAccountIDs(req.IDs)
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要刷新的账号 ID 列表")
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchRefreshAccounts(c, ids)
		return
	}

	success, fail := h.runBatchRefreshAccounts(c.Request.Context(), ids, nil)
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已刷新 %d 个账号，失败 %d 个", success, fail),
		"success": success,
		"failed":  fail,
	})
}

func (h *Handler) refreshAccountByID(ctx context.Context, id int64) error {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	refreshFn := h.refreshAccount
	if refreshFn == nil {
		refreshFn = h.refreshSingleAccount
	}
	if err := refreshFn(refreshCtx, id); err != nil {
		return err
	}

	// 刷新成功后顺带做一次零成本 wham 用量探针，从服务端权威数据同步订阅到期时间与用量。
	// 续费后 access/id token 里的 chatgpt_subscription_active_until 不一定立即更新（会滞后），
	// 仅靠 token 刷新会让"有效期"长期停留在旧值；wham/usage 返回的是服务端当前订阅到期时间。
	// （issue #300）
	if probe := h.usageProbeFunc(); probe != nil && h.store != nil {
		if acc := h.store.FindByID(id); acc != nil {
			probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
			if err := probe(probeCtx, acc); err != nil {
				log.Printf("[账号 %d] 刷新后用量/订阅到期探针失败（忽略）: %v", id, err)
			}
			probeCancel()
		}
	}
	return nil
}

func (h *Handler) streamBatchRefreshAccounts(c *gin.Context, ids []int64) {
	setupSSE(c)
	total := len(ids)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_refresh", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_refresh"})
		return
	}

	events := make(chan batchOperationEvent, len(ids)+1)
	ctx := c.Request.Context()
	go func() {
		success, fail := h.runBatchRefreshAccounts(ctx, ids, func(event batchOperationEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		select {
		case events <- batchOperationEvent{
			Type:    "complete",
			Action:  "batch_refresh",
			Current: total,
			Total:   total,
			Success: success,
			Failed:  fail,
		}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for event := range events {
		sendSSEJSON(c, event)
	}
}

func (h *Handler) runBatchRefreshAccounts(ctx context.Context, ids []int64, onProgress func(batchOperationEvent)) (int64, int64) {
	total := len(ids)
	var (
		success   int64
		fail      int64
		completed int64
		wg        sync.WaitGroup
		sem       = make(chan struct{}, accountRefreshBatchConcurrency)
	)

	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&fail, 1)
				emitBatchRefreshProgress(onProgress, id, total, &completed, &success, &fail, "刷新已取消", true)
				return
			}
			defer func() { <-sem }()

			if err := h.refreshAccountByID(ctx, id); err != nil {
				atomic.AddInt64(&fail, 1)
				emitBatchRefreshProgress(onProgress, id, total, &completed, &success, &fail, err.Error(), true)
				return
			}

			atomic.AddInt64(&success, 1)
			emitBatchRefreshProgress(onProgress, id, total, &completed, &success, &fail, "账号刷新成功", false)
		}()
	}

	wg.Wait()
	return atomic.LoadInt64(&success), atomic.LoadInt64(&fail)
}

func emitBatchRefreshProgress(
	onProgress func(batchOperationEvent),
	accountID int64,
	total int,
	completedCount *int64,
	successCount *int64,
	failedCount *int64,
	message string,
	failed bool,
) {
	if onProgress == nil {
		return
	}
	current := int(atomic.AddInt64(completedCount, 1))
	event := batchOperationEvent{
		Type:      "progress",
		Action:    "batch_refresh",
		Current:   current,
		Total:     total,
		Success:   atomic.LoadInt64(successCount),
		Failed:    atomic.LoadInt64(failedCount),
		AccountID: accountID,
		Message:   message,
	}
	if failed {
		event.Error = message
	}
	onProgress(event)
}

// ToggleAccountEnabled 切换账号是否参与调度选择
func (h *Handler) ToggleAccountEnabled(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.SetAccountEnabled(ctx, id, *req.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "更新启用状态失败: "+err.Error())
		return
	}

	h.store.ApplyAccountEnabled(id, *req.Enabled)

	if *req.Enabled {
		writeMessage(c, http.StatusOK, "账号已启用")
	} else {
		writeMessage(c, http.StatusOK, "账号已禁用")
	}
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
	h.syncAccountPlanAfterReset(c.Request.Context(), acc)
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
		h.syncAccountPlanAfterReset(c.Request.Context(), acc)
		success++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已重置 %d 个账号状态", success),
		"success": success,
		"failed":  fail,
	})
}

func (h *Handler) syncAccountPlanAfterReset(_ context.Context, acc *auth.Account) {
	if h == nil || h.syncAccountPlanOnReset == nil || acc == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.syncAccountPlanOnReset(ctx, acc); err != nil {
			log.Printf("[账号 %d] 重置后同步 Codex plan type 失败: %v", acc.DBID, err)
		}
	}()
}

func (h *Handler) syncSingleAccountPlanOnReset(ctx context.Context, acc *auth.Account) error {
	if h == nil || h.store == nil || acc == nil || acc.IsOpenAIResponsesAPI() || acc.GetAccessToken() == "" {
		return nil
	}
	model, err := h.connectionTestModelForAccount(ctx, acc, "")
	if err != nil {
		return err
	}
	resp, err := proxy.ExecuteRequest(ctx, acc, buildConnectionTestPayload(h.store, model), "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	proxy.SyncCodexUsageState(h.store, acc, resp)
	return nil
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

// GetUsageStats 获取使用统计。
// 支持可选 query 参数 start/end (RFC3339);未传时回落"今日"行为。
func (h *Handler) GetUsageStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	rangeStart, rangeEnd, err := parseUsageStatsRange(c.Query("start"), c.Query("end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	stats, err := h.getUsageStatsCached(ctx, rangeStart, rangeEnd)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// parseUsageStatsRange 解析 /usage/stats 的可选 start/end query。
// 任一为空则当作零值由调用方决定回退行为(默认"今日");两者都填则要求均合法。
func parseUsageStatsRange(startStr, endStr string) (time.Time, time.Time, error) {
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)
	var start, end time.Time
	if startStr != "" {
		t, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start 参数格式错误，需要 RFC3339")
		}
		start = t
	}
	if endStr != "" {
		t, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end 参数格式错误，需要 RFC3339")
		}
		end = t
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end 必须晚于 start")
	}
	return start, end, nil
}

// GetAPIKeyTokenStats 返回按 API Key 聚合的 token 用量列表（issue #162）。
// 支持可选 query 参数 start/end (RFC3339)；缺省回落到"今日"。
// 不分页/不限条数：前端做排序、搜索、分页。
func (h *Handler) GetAPIKeyTokenStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()

	rangeStart, rangeEnd, err := parseUsageStatsRange(c.Query("start"), c.Query("end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	items, err := h.db.ListAPIKeyTokenStats(ctx, rangeStart, rangeEnd)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if items == nil {
		items = []database.APIKeyTokenStat{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
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

	var cached database.ChartAggregation
	if h.getRuntimeJSON(ctx, adminChartCacheNamespace, cacheKey, &cached) {
		result := &cached
		h.chartCacheMu.Lock()
		h.chartCacheData[cacheKey] = &chartCacheEntry{
			data:      result,
			expiresAt: time.Now().Add(adminChartCacheTTL),
		}
		h.chartCacheMu.Unlock()
		c.JSON(http.StatusOK, result)
		return
	}

	result, err := h.db.GetChartAggregation(ctx, startTime, endTime, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.setRuntimeJSON(ctx, adminChartCacheNamespace, cacheKey, result, adminChartCacheTTL)

	// 写入缓存
	h.chartCacheMu.Lock()
	h.chartCacheData[cacheKey] = &chartCacheEntry{
		data:      result,
		expiresAt: time.Now().Add(adminChartCacheTTL),
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

func parseOpsErrorPositiveInt64(c *gin.Context, name string) (*int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("%s 参数无效，需要正整数", name))
		return nil, false
	}
	return &parsed, true
}

func parseOpsErrorLogFilter(c *gin.Context, withPaging bool) (database.UsageLogFilter, bool) {
	endTime := time.Now()
	startTime := endTime.Add(-1 * time.Hour)
	startStr := strings.TrimSpace(c.Query("start"))
	endStr := strings.TrimSpace(c.Query("end"))
	if startStr != "" || endStr != "" {
		if startStr == "" || endStr == "" {
			writeError(c, http.StatusBadRequest, "start/end 参数需要同时提供")
			return database.UsageLogFilter{}, false
		}
		parsedStart, e1 := time.Parse(time.RFC3339, startStr)
		parsedEnd, e2 := time.Parse(time.RFC3339, endStr)
		if e1 != nil || e2 != nil {
			writeError(c, http.StatusBadRequest, "start/end 参数格式错误，需要 RFC3339 格式")
			return database.UsageLogFilter{}, false
		}
		startTime = parsedStart
		endTime = parsedEnd
	}

	apiKeyID, ok := parseOpsErrorPositiveInt64(c, "api_key_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	accountID, ok := parseOpsErrorPositiveInt64(c, "account_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}

	filter := database.UsageLogFilter{
		Start:           startTime,
		End:             endTime,
		Page:            1,
		PageSize:        20,
		Email:           strings.TrimSpace(c.Query("email")),
		Model:           strings.TrimSpace(c.Query("model")),
		Endpoint:        strings.TrimSpace(c.Query("endpoint")),
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		ErrorOnly:       true,
		IncludeCanceled: true,
		ErrorKind:       strings.TrimSpace(c.Query("error_kind")),
		Query:           strings.TrimSpace(c.Query("q")),
	}

	status := strings.TrimSpace(c.Query("status"))
	if status == "" {
		status = strings.TrimSpace(c.Query("status_code"))
	}
	switch strings.ToLower(status) {
	case "", "all":
	case "4xx", "5xx":
		filter.StatusFamily = strings.ToLower(status)
	default:
		statusCode, err := strconv.Atoi(status)
		if err != nil || statusCode < 100 || statusCode > 599 {
			writeError(c, http.StatusBadRequest, "status/status_code 参数无效")
			return database.UsageLogFilter{}, false
		}
		filter.StatusCode = statusCode
	}

	if fastStr := c.Query("fast"); fastStr != "" {
		v := fastStr == "true"
		filter.FastOnly = &v
	}
	if streamStr := c.Query("stream"); streamStr != "" {
		v := streamStr == "true"
		filter.StreamOnly = &v
	}

	if withPaging {
		if pageStr := c.Query("page"); pageStr != "" {
			if page, err := strconv.Atoi(pageStr); err == nil && page > 0 {
				filter.Page = page
			}
		}
		if ps := c.Query("page_size"); ps != "" {
			if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 500 {
				filter.PageSize = n
			}
		}
	}

	return filter, true
}

type opsErrorExportFile struct {
	Version           int                   `json:"version"`
	GeneratedAt       time.Time             `json:"generated_at"`
	Range             opsErrorExportRange   `json:"range"`
	Filters           opsErrorExportFilters `json:"filters"`
	Options           opsErrorExportOptions `json:"options"`
	TotalMatched      int                   `json:"total_matched"`
	ExcludedCount     int                   `json:"excluded_count"`
	ExportedCount     int                   `json:"exported_count"`
	DuplicatesRemoved int                   `json:"duplicates_removed"`
	Errors            []opsErrorExportEntry `json:"errors"`
}

type opsErrorExportRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type opsErrorExportFilters struct {
	Email        string `json:"email,omitempty"`
	Model        string `json:"model,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	APIKeyID     *int64 `json:"api_key_id,omitempty"`
	AccountID    *int64 `json:"account_id,omitempty"`
	FastOnly     *bool  `json:"fast_only,omitempty"`
	StreamOnly   *bool  `json:"stream_only,omitempty"`
	StatusCode   int    `json:"status_code,omitempty"`
	StatusFamily string `json:"status_family,omitempty"`
	ErrorKind    string `json:"error_kind,omitempty"`
	Query        string `json:"query,omitempty"`
}

type opsErrorExportOptions struct {
	Dedupe              bool  `json:"dedupe"`
	ExcludedStatusCodes []int `json:"excluded_status_codes,omitempty"`
}

type opsErrorExportEntry struct {
	Signature          string    `json:"signature"`
	Occurrences        int       `json:"occurrences"`
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	SampleIDs          []int64   `json:"sample_ids"`
	AffectedAccountIDs []int64   `json:"affected_account_ids,omitempty"`
	AffectedAPIKeyIDs  []int64   `json:"affected_api_key_ids,omitempty"`
	ID                 int64     `json:"id"`
	CreatedAt          time.Time `json:"created_at"`
	StatusCode         int       `json:"status_code"`
	ErrorKind          string    `json:"error_kind"`
	ErrorMessage       string    `json:"error_message"`
	AccountID          int64     `json:"account_id"`
	AccountName        string    `json:"account_name"`
	AccountEmail       string    `json:"account_email"`
	APIKeyID           int64     `json:"api_key_id"`
	APIKeyName         string    `json:"api_key_name"`
	APIKeyMasked       string    `json:"api_key_masked"`
	Endpoint           string    `json:"endpoint"`
	UpstreamEndpoint   string    `json:"upstream_endpoint"`
	Model              string    `json:"model"`
	EffectiveModel     string    `json:"effective_model"`
	Stream             bool      `json:"stream"`
	DurationMs         int       `json:"duration_ms"`
	FirstTokenMs       int       `json:"first_token_ms"`
	IsRetryAttempt     bool      `json:"is_retry_attempt"`
	AttemptIndex       int       `json:"attempt_index"`
}

// GetOpsErrorLogs 获取运维错误日志
func (h *Handler) GetOpsErrorLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, true)
	if !ok {
		return
	}
	result, err := h.db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ExportOpsErrorLogs 导出运维错误日志 JSON。
func (h *Handler) ExportOpsErrorLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, false)
	if !ok {
		return
	}
	dedupe := parseBoolQueryDefault(c, "dedupe", true)
	excludedStatusCodes, excludedStatusSet, ok := parseExcludedStatusCodes(c.Query("exclude_status"))
	if !ok {
		writeError(c, http.StatusBadRequest, "exclude_status 参数无效")
		return
	}

	logs, err := h.db.ListUsageLogsByFilter(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	exportFile := buildOpsErrorExportFile(logs, filter, dedupe, excludedStatusCodes, excludedStatusSet)
	body, err := json.MarshalIndent(exportFile, "", "  ")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	body = append(body, '\n')

	filename := fmt.Sprintf("ops-errors-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func parseBoolQueryDefault(c *gin.Context, name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(c.Query(name)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseExcludedStatusCodes(raw string) ([]int, map[int]bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, map[int]bool{}, true
	}
	seen := map[int]bool{}
	var statuses []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, err := strconv.Atoi(part)
		if err != nil || code < 100 || code > 599 {
			return nil, nil, false
		}
		if !seen[code] {
			seen[code] = true
			statuses = append(statuses, code)
		}
	}
	sort.Ints(statuses)
	return statuses, seen, true
}

func buildOpsErrorExportFile(logs []*database.UsageLog, filter database.UsageLogFilter, dedupe bool, excludedStatusCodes []int, excludedStatusSet map[int]bool) opsErrorExportFile {
	exportFile := opsErrorExportFile{
		Version:      1,
		GeneratedAt:  time.Now(),
		Range:        opsErrorExportRange{Start: filter.Start, End: filter.End},
		Filters:      opsErrorExportFiltersFromUsageFilter(filter),
		Options:      opsErrorExportOptions{Dedupe: dedupe, ExcludedStatusCodes: excludedStatusCodes},
		TotalMatched: len(logs),
		Errors:       []opsErrorExportEntry{},
	}

	filteredLogs := make([]*database.UsageLog, 0, len(logs))
	for _, logRow := range logs {
		if logRow == nil {
			continue
		}
		if excludedStatusSet[logRow.StatusCode] {
			exportFile.ExcludedCount++
			continue
		}
		filteredLogs = append(filteredLogs, logRow)
	}

	if !dedupe {
		for _, logRow := range filteredLogs {
			entry := newOpsErrorExportEntry(logRow)
			exportFile.Errors = append(exportFile.Errors, entry)
		}
		exportFile.ExportedCount = len(exportFile.Errors)
		return exportFile
	}

	entryBySignature := make(map[string]int)
	for _, logRow := range filteredLogs {
		entry := newOpsErrorExportEntry(logRow)
		if idx, exists := entryBySignature[entry.Signature]; exists {
			exportFile.Errors[idx].merge(logRow)
			continue
		}
		entryBySignature[entry.Signature] = len(exportFile.Errors)
		exportFile.Errors = append(exportFile.Errors, entry)
	}
	sort.SliceStable(exportFile.Errors, func(i, j int) bool {
		if exportFile.Errors[i].Occurrences != exportFile.Errors[j].Occurrences {
			return exportFile.Errors[i].Occurrences > exportFile.Errors[j].Occurrences
		}
		return exportFile.Errors[i].LastSeen.After(exportFile.Errors[j].LastSeen)
	})
	exportFile.ExportedCount = len(exportFile.Errors)
	exportFile.DuplicatesRemoved = len(filteredLogs) - len(exportFile.Errors)
	return exportFile
}

func opsErrorExportFiltersFromUsageFilter(filter database.UsageLogFilter) opsErrorExportFilters {
	return opsErrorExportFilters{
		Email:        filter.Email,
		Model:        filter.Model,
		Endpoint:     filter.Endpoint,
		APIKeyID:     filter.APIKeyID,
		AccountID:    filter.AccountID,
		FastOnly:     filter.FastOnly,
		StreamOnly:   filter.StreamOnly,
		StatusCode:   filter.StatusCode,
		StatusFamily: filter.StatusFamily,
		ErrorKind:    filter.ErrorKind,
		Query:        filter.Query,
	}
}

func newOpsErrorExportEntry(logRow *database.UsageLog) opsErrorExportEntry {
	entry := opsErrorExportEntry{
		Signature:          opsErrorSignature(logRow),
		Occurrences:        1,
		FirstSeen:          logRow.CreatedAt,
		LastSeen:           logRow.CreatedAt,
		SampleIDs:          []int64{logRow.ID},
		AffectedAccountIDs: appendUniqueInt64(nil, logRow.AccountID, 50),
		AffectedAPIKeyIDs:  appendUniqueInt64(nil, logRow.APIKeyID, 50),
		ID:                 logRow.ID,
		CreatedAt:          logRow.CreatedAt,
		StatusCode:         logRow.StatusCode,
		ErrorKind:          logRow.UpstreamErrorKind,
		ErrorMessage:       logRow.ErrorMessage,
		AccountID:          logRow.AccountID,
		AccountName:        logRow.AccountName,
		AccountEmail:       logRow.AccountEmail,
		APIKeyID:           logRow.APIKeyID,
		APIKeyName:         logRow.APIKeyName,
		APIKeyMasked:       logRow.APIKeyMasked,
		Endpoint:           firstNonEmpty(logRow.InboundEndpoint, logRow.Endpoint),
		UpstreamEndpoint:   logRow.UpstreamEndpoint,
		Model:              logRow.Model,
		EffectiveModel:     logRow.EffectiveModel,
		Stream:             logRow.Stream,
		DurationMs:         logRow.DurationMs,
		FirstTokenMs:       logRow.FirstTokenMs,
		IsRetryAttempt:     logRow.IsRetryAttempt,
		AttemptIndex:       logRow.AttemptIndex,
	}
	return entry
}

func (entry *opsErrorExportEntry) merge(logRow *database.UsageLog) {
	entry.Occurrences++
	if logRow.CreatedAt.Before(entry.FirstSeen) {
		entry.FirstSeen = logRow.CreatedAt
	}
	if logRow.CreatedAt.After(entry.LastSeen) {
		entry.LastSeen = logRow.CreatedAt
	}
	entry.SampleIDs = appendUniqueInt64(entry.SampleIDs, logRow.ID, 20)
	entry.AffectedAccountIDs = appendUniqueInt64(entry.AffectedAccountIDs, logRow.AccountID, 50)
	entry.AffectedAPIKeyIDs = appendUniqueInt64(entry.AffectedAPIKeyIDs, logRow.APIKeyID, 50)
}

func opsErrorSignature(logRow *database.UsageLog) string {
	parts := []string{
		strconv.Itoa(logRow.StatusCode),
		strings.TrimSpace(logRow.UpstreamErrorKind),
		strings.Join(strings.Fields(logRow.ErrorMessage), " "),
		firstNonEmpty(logRow.InboundEndpoint, logRow.Endpoint),
		strings.TrimSpace(logRow.UpstreamEndpoint),
		strings.TrimSpace(logRow.Model),
		strings.TrimSpace(logRow.EffectiveModel),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:12])
}

func appendUniqueInt64(values []int64, value int64, limit int) []int64 {
	if value <= 0 || (limit > 0 && len(values) >= limit) {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// GetOpsErrorSummary 获取运维错误日志概览
func (h *Handler) GetOpsErrorSummary(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, false)
	if !ok {
		return
	}
	result, err := h.db.GetUsageErrorSummary(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
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
				if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 500 {
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
			var accountID *int64
			if accountIDStr := c.Query("account_id"); accountIDStr != "" {
				parsed, err := strconv.ParseInt(accountIDStr, 10, 64)
				if err != nil || parsed <= 0 {
					writeError(c, http.StatusBadRequest, "account_id 参数无效，需要正整数")
					return
				}
				accountID = &parsed
			}

			filter := database.UsageLogFilter{
				Start:     startTime,
				End:       endTime,
				Page:      page,
				PageSize:  pageSize,
				Email:     c.Query("email"),
				Model:     c.Query("model"),
				Endpoint:  c.Query("endpoint"),
				APIKeyID:  apiKeyID,
				AccountID: accountID,
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
	h.deleteRuntimeCache(ctx, adminUsageStatsCacheNamespace, "global")
	h.chartCacheMu.Lock()
	h.chartCacheData = make(map[string]*chartCacheEntry)
	h.chartCacheMu.Unlock()
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

	// 检查是否有任何 key 配置了窗口 cost limit
	var need5h, need7d, need30d bool
	for _, k := range keys {
		if k.Limits.CostLimit5h > 0 {
			need5h = true
		}
		if k.Limits.CostLimit7d > 0 {
			need7d = true
		}
		if k.Limits.CostLimit30d > 0 {
			need30d = true
		}
	}

	// 按需批量查询窗口用量
	var cost5h, cost7d, cost30d map[int64]float64
	if need5h {
		cost5h, _ = h.db.GetAllAPIKeysWindowCost(ctx, 5*time.Hour)
	}
	if need7d {
		cost7d, _ = h.db.GetAllAPIKeysWindowCost(ctx, 7*24*time.Hour)
	}
	if need30d {
		cost30d, _ = h.db.GetAllAPIKeysWindowCost(ctx, 30*24*time.Hour)
	}

	// 转换为脱敏响应
	maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
	for _, k := range keys {
		mk := NewMaskedAPIKeyRow(k)
		if k.Limits.CostLimit5h > 0 || k.Limits.CostLimit7d > 0 || k.Limits.CostLimit30d > 0 {
			detail := &APIKeyWindowUsageDetail{}
			if cost5h != nil {
				detail.Cost5h = cost5h[k.ID]
			}
			if cost7d != nil {
				detail.Cost7d = cost7d[k.ID]
			}
			if cost30d != nil {
				detail.Cost30d = cost30d[k.ID]
			}
			mk.WindowUsage = detail
		}
		maskedKeys = append(maskedKeys, mk)
	}

	c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
}

type createKeyReq struct {
	Name            string                 `json:"name"`
	Key             string                 `json:"key"`
	QuotaLimit      *float64               `json:"quota_limit"`
	Quota           *float64               `json:"quota"`
	ExpiresAt       string                 `json:"expires_at"`
	ExpiresInDays   *int                   `json:"expires_in_days"`
	AllowedGroupIDs json.RawMessage        `json:"allowed_group_ids"`
	Limits          *database.APIKeyLimits `json:"limits"`
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

	quotaLimit := 0.0
	if req.Quota != nil {
		quotaLimit = *req.Quota
	}
	if req.QuotaLimit != nil {
		quotaLimit = *req.QuotaLimit
	}
	if quotaLimit < 0 {
		writeError(c, http.StatusBadRequest, "额度限制不能小于 0")
		return
	}
	if quotaLimit > 1000000000 {
		writeError(c, http.StatusBadRequest, "额度限制过大")
		return
	}

	expiresAt, err := parseAPIKeyExpiresAt(req.ExpiresAt, req.ExpiresInDays)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	allowedGroupIDs, err := parseOptionalIntegerSliceField(req.AllowedGroupIDs, "allowed_group_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
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
	if allowedGroupIDs.Set {
		missing, err := h.db.VerifyAccountGroupIDs(ctx, allowedGroupIDs.Values)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if len(missing) > 0 {
			values := make([]string, 0, len(missing))
			for _, value := range missing {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_group_ids 包含不存在的分组 ID: "+strings.Join(values, ", "))
			return
		}
	}

	var limits database.APIKeyLimits
	if req.Limits != nil {
		limits = sanitizeAPIKeyLimits(*req.Limits)
	}

	id, err := h.db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{
		Name:            req.Name,
		Key:             key,
		QuotaLimit:      quotaLimit,
		ExpiresAt:       expiresAt,
		AllowedGroupIDs: allowedGroupIDs.Values,
		Limits:          limits,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建失败: "+err.Error())
		return
	}
	if allowedGroupIDs.Set {
		values := dedupeInt64(allowedGroupIDs.Values)
		if h.store != nil {
			h.store.SetAPIKeyAllowedGroups(id, values)
		}
	}
	if h.store != nil {
		h.store.SetAPIKeyAllowedPlans(id, limits.PlanAllow)
	}
	h.invalidateAPIKeyRuntimeCaches(ctx, key)

	// 记录安全审计日志
	security.SecurityAuditLog("API_KEY_CREATED", fmt.Sprintf("id=%d name=%s ip=%s", id, security.SanitizeLog(req.Name), c.ClientIP()))

	var expiresAtResponse *string
	if expiresAt.Valid {
		formatted := expiresAt.Time.Format(time.RFC3339)
		expiresAtResponse = &formatted
	}
	c.JSON(http.StatusOK, createAPIKeyResponse{
		ID:              id,
		Key:             key,
		Name:            req.Name,
		QuotaLimit:      quotaLimit,
		QuotaUsed:       0,
		ExpiresAt:       expiresAtResponse,
		AllowedGroupIDs: dedupeInt64(allowedGroupIDs.Values),
	})
}

type updateAPIKeyReq struct {
	Name            *string                `json:"name"`
	QuotaLimit      json.RawMessage        `json:"quota_limit"`
	Quota           json.RawMessage        `json:"quota"`
	ResetQuota      *bool                  `json:"reset_quota"`
	ExpiresAt       json.RawMessage        `json:"expires_at"`
	ExpiresInDays   *int                   `json:"expires_in_days"`
	AllowedGroupIDs json.RawMessage        `json:"allowed_group_ids"`
	Limits          *database.APIKeyLimits `json:"limits"`
}

func (h *Handler) UpdateAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	var req updateAPIKeyReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	allowedGroupIDs, err := parseOptionalIntegerSliceField(req.AllowedGroupIDs, "allowed_group_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	quotaLimit, quotaLimitSet, err := parseOptionalAPIKeyQuota(req.QuotaLimit, req.Quota)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	expiresAt, expiresAtSet, err := parseOptionalAPIKeyExpiration(req.ExpiresAt, req.ExpiresInDays)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "API Key 不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if req.Name != nil {
		name := security.SanitizeInput(*req.Name)
		if strings.TrimSpace(name) == "" {
			writeError(c, http.StatusBadRequest, "名称不能为空")
			return
		}
		if utf8.RuneCountInString(name) > 100 {
			writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
			return
		}
		if security.ContainsXSS(name) {
			writeError(c, http.StatusBadRequest, "名称包含非法字符")
			return
		}
		req.Name = &name
	}
	if quotaLimitSet {
		if quotaLimit > 1000000000 {
			writeError(c, http.StatusBadRequest, "额度限制不能超过 1000000000")
			return
		}
	}
	var allowedGroupValues []int64
	if allowedGroupIDs.Set {
		missing, err := h.db.VerifyAccountGroupIDs(ctx, allowedGroupIDs.Values)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if len(missing) > 0 {
			values := make([]string, 0, len(missing))
			for _, value := range missing {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_group_ids 包含不存在的分组 ID: "+strings.Join(values, ", "))
			return
		}
		allowedGroupValues = dedupeInt64(allowedGroupIDs.Values)
	}
	update := database.APIKeyUpdate{
		QuotaLimit:         quotaLimit,
		QuotaLimitSet:      quotaLimitSet,
		ResetQuota:         req.ResetQuota != nil && *req.ResetQuota,
		ExpiresAt:          expiresAt,
		ExpiresAtSet:       expiresAtSet,
		AllowedGroupIDs:    allowedGroupValues,
		AllowedGroupIDsSet: allowedGroupIDs.Set,
	}
	if req.Name != nil {
		update.Name = *req.Name
		update.NameSet = true
	}
	if req.Limits != nil {
		update.Limits = sanitizeAPIKeyLimits(*req.Limits)
		update.LimitsSet = true
	}
	if err := h.db.UpdateAPIKey(ctx, id, update); err != nil {
		writeInternalError(c, err)
		return
	}
	if allowedGroupIDs.Set && h.store != nil {
		h.store.SetAPIKeyAllowedGroups(id, allowedGroupValues)
	}
	if update.LimitsSet && h.store != nil {
		h.store.SetAPIKeyAllowedPlans(id, update.Limits.PlanAllow)
	}
	h.invalidateAPIKeyRuntimeCaches(ctx, row.Key)
	writeMessage(c, http.StatusOK, "API Key 已更新")
}

// sanitizeAPIKeyLimits 把请求体里来的 limits 归一:负值置 0,空白模型名过滤,字符串小写。
// 同时配置 ModelAllow + ModelDeny 时白名单优先(在 enforce 时已生效),这里不强制清空黑名单。
func sanitizeAPIKeyLimits(in database.APIKeyLimits) database.APIKeyLimits {
	clean := func(items []string) []string {
		if len(items) == 0 {
			return nil
		}
		seen := make(map[string]struct{}, len(items))
		out := make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			lower := strings.ToLower(item)
			if _, ok := seen[lower]; ok {
				continue
			}
			seen[lower] = struct{}{}
			out = append(out, item)
		}
		return out
	}
	out := database.APIKeyLimits{
		ModelAllow:     clean(in.ModelAllow),
		ModelDeny:      clean(in.ModelDeny),
		PlanAllow:      cleanPlanAllow(in.PlanAllow),
		RPM:            maxInt(in.RPM, 0),
		RPD:            maxInt(in.RPD, 0),
		MaxConcurrency: maxInt(in.MaxConcurrency, 0),
		CostLimit5h:    maxFloat(in.CostLimit5h, 0),
		CostLimit7d:    maxFloat(in.CostLimit7d, 0),
		CostLimit30d:   maxFloat(in.CostLimit30d, 0),
		TokenLimit5h:   maxInt64(in.TokenLimit5h, 0),
		TokenLimit7d:   maxInt64(in.TokenLimit7d, 0),
		TokenLimit30d:  maxInt64(in.TokenLimit30d, 0),
	}
	return out
}

// knownAPIKeyPlanFilters 是账号套餐白名单允许的取值集合。与前端 PlanMultiSelect 的
// 选项、以及 Accounts 页的套餐筛选保持一致(按原始 plan_type 精确匹配,pro 与 prolite
// 相互独立)。未知值在 cleanPlanAllow 中被丢弃,避免把打字错误写进过滤条件后导致该
// Key 永远选不到账号。
var knownAPIKeyPlanFilters = map[string]struct{}{
	"free": {}, "plus": {}, "pro": {}, "prolite": {}, "team": {}, "k12": {}, "go": {},
}

// cleanPlanAllow 归一账号套餐白名单:小写去空白、丢弃未知值并去重。
func cleanPlanAllow(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		plan := strings.ToLower(strings.TrimSpace(item))
		if plan == "" {
			continue
		}
		if _, ok := knownAPIKeyPlanFilters[plan]; !ok {
			continue
		}
		if _, ok := seen[plan]; ok {
			continue
		}
		seen[plan] = struct{}{}
		out = append(out, plan)
	}
	return out
}

func maxInt(v, lo int) int {
	if v < lo {
		return lo
	}
	return v
}

func maxInt64(v, lo int64) int64 {
	if v < lo {
		return lo
	}
	return v
}

func maxFloat(v, lo float64) float64 {
	if v < lo {
		return lo
	}
	return v
}

func parseOptionalAPIKeyQuota(quotaLimitRaw, quotaRaw json.RawMessage) (float64, bool, error) {
	raw := quotaLimitRaw
	if len(raw) == 0 {
		raw = quotaRaw
	}
	if len(raw) == 0 {
		return 0, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, true, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("额度限制必须是数字")
	}
	if value < 0 {
		return 0, true, fmt.Errorf("额度限制不能小于 0")
	}
	return value, true, nil
}

func parseOptionalAPIKeyExpiration(raw json.RawMessage, expiresInDays *int) (sql.NullTime, bool, error) {
	if expiresInDays != nil {
		expiresAt, err := parseAPIKeyExpiresAt("", expiresInDays)
		return expiresAt, true, err
	}
	if len(raw) == 0 {
		return sql.NullTime{}, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return sql.NullTime{}, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return sql.NullTime{}, true, fmt.Errorf("过期时间格式无效")
	}
	expiresAt, err := parseAPIKeyExpiresAt(value, nil)
	return expiresAt, true, err
}

func parseAPIKeyExpiresAt(raw string, expiresInDays *int) (sql.NullTime, error) {
	if expiresInDays != nil {
		if *expiresInDays < 0 {
			return sql.NullTime{}, fmt.Errorf("过期天数不能小于 0")
		}
		if *expiresInDays > 0 {
			if *expiresInDays > 3650 {
				return sql.NullTime{}, fmt.Errorf("过期天数不能超过 3650 天")
			}
			return sql.NullTime{Time: time.Now().AddDate(0, 0, *expiresInDays), Valid: true}, nil
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sql.NullTime{}, nil
	}
	layouts := []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}
	var parsed time.Time
	var err error
	for _, layout := range layouts {
		if layout == time.RFC3339 {
			parsed, err = time.Parse(layout, raw)
		} else {
			parsed, err = time.ParseInLocation(layout, raw, time.Local)
		}
		if err == nil {
			if layout == "2006-01-02" {
				parsed = parsed.Add(24*time.Hour - time.Nanosecond)
			}
			if !parsed.After(time.Now()) {
				return sql.NullTime{}, fmt.Errorf("过期时间必须晚于当前时间")
			}
			return sql.NullTime{Time: parsed, Valid: true}, nil
		}
	}
	return sql.NullTime{}, fmt.Errorf("过期时间格式无效")
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

	keyToInvalidate := ""
	if row, err := h.db.GetAPIKeyByID(ctx, id); err == nil && row != nil {
		keyToInvalidate = row.Key
	}
	if err := h.db.DeleteAPIKey(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	if h.store != nil {
		h.store.SetAPIKeyAllowedGroups(id, nil)
		h.store.SetAPIKeyAllowedPlans(id, nil)
	}
	h.invalidateAPIKeyRuntimeCaches(ctx, keyToInvalidate)
	writeMessage(c, http.StatusOK, "已删除")
}

// ==================== Settings ====================

type settingsResponse struct {
	SiteName                           string  `json:"site_name"`
	SiteLogo                           string  `json:"site_logo"`
	BackgroundImage                    string  `json:"background_image"`
	BackgroundOpacity                  int     `json:"background_opacity"`
	BackgroundBlur                     int     `json:"background_blur"`
	BackgroundGlassOpacity             int     `json:"background_glass_opacity"`
	BackgroundGlassBlur                int     `json:"background_glass_blur"`
	MaxConcurrency                     int     `json:"max_concurrency"`
	GlobalRPM                          int     `json:"global_rpm"`
	TestModel                          string  `json:"test_model"`
	TestContent                        string  `json:"test_content"`
	TestConcurrency                    int     `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes   int     `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes            int     `json:"usage_probe_max_age_minutes"`
	UsageProbeConcurrency              int     `json:"usage_probe_concurrency"`
	UsageProbeResponsesFallbackEnabled bool    `json:"usage_probe_responses_fallback_enabled"`
	RecoveryProbeIntervalMinutes       int     `json:"recovery_probe_interval_minutes"`
	LazyMode                           bool    `json:"lazy_mode"`
	ProxyURL                           string  `json:"proxy_url"`
	PgMaxConns                         int     `json:"pg_max_conns"`
	RedisPoolSize                      int     `json:"redis_pool_size"`
	AutoCleanUnauthorized              bool    `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited               bool    `json:"auto_clean_rate_limited"`
	AdminSecret                        string  `json:"admin_secret"`
	AdminAuthSource                    string  `json:"admin_auth_source"`
	AutoCleanFullUsage                 bool    `json:"auto_clean_full_usage"`
	AutoCleanError                     bool    `json:"auto_clean_error"`
	AutoCleanExpired                   bool    `json:"auto_clean_expired"`
	ProxyPoolEnabled                   bool    `json:"proxy_pool_enabled"`
	FastSchedulerEnabled               bool    `json:"fast_scheduler_enabled"`
	CodexForceWebsocket                bool    `json:"codex_force_websocket"`
	CodexWSKeepaliveEnabled            bool    `json:"codex_ws_keepalive_enabled"`
	CodexWSKeepaliveIntervalSec        int     `json:"codex_ws_keepalive_interval_sec"`
	CodexWSHideUpstreamErrors          bool    `json:"codex_ws_hide_upstream_errors"`
	CodexWSSilentRetryEnabled          bool    `json:"codex_ws_silent_retry_enabled"`
	CodexWSSilentMaxRetries            int     `json:"codex_ws_silent_max_retries"`
	SchedulerMode                      string  `json:"scheduler_mode"`
	AffinityMode                       string  `json:"affinity_mode"`
	MaxRetries                         int     `json:"max_retries"`
	MaxRateLimitRetries                int     `json:"max_rate_limit_retries"`
	RetryIntervalMS                    int     `json:"retry_interval_ms"`
	TransportRetryPolicy               string  `json:"transport_retry_policy"`
	AllowRemoteMigration               bool    `json:"allow_remote_migration"`
	DatabaseDriver                     string  `json:"database_driver"`
	DatabaseLabel                      string  `json:"database_label"`
	CacheDriver                        string  `json:"cache_driver"`
	CacheLabel                         string  `json:"cache_label"`
	ExpiredCleaned                     int     `json:"expired_cleaned,omitempty"`
	ModelMapping                       string  `json:"model_mapping"`
	CodexModelMapping                  string  `json:"codex_model_mapping"`
	ReasoningEffortModels              string  `json:"reasoning_effort_models"`
	ResinURL                           string  `json:"resin_url"`
	ResinPlatformName                  string  `json:"resin_platform_name"`
	PromptFilterEnabled                bool    `json:"prompt_filter_enabled"`
	PromptFilterMode                   string  `json:"prompt_filter_mode"`
	PromptFilterThreshold              int     `json:"prompt_filter_threshold"`
	PromptFilterStrictThreshold        int     `json:"prompt_filter_strict_threshold"`
	PromptFilterLogMatches             bool    `json:"prompt_filter_log_matches"`
	PromptFilterMaxTextLength          int     `json:"prompt_filter_max_text_length"`
	PromptFilterSensitiveWords         string  `json:"prompt_filter_sensitive_words"`
	PromptFilterCustomPatterns         string  `json:"prompt_filter_custom_patterns"`
	PromptFilterDisabledPatterns       string  `json:"prompt_filter_disabled_patterns"`
	PromptFilterReviewEnabled          bool    `json:"prompt_filter_review_enabled"`
	PromptFilterReviewAPIKeyConfigured bool    `json:"prompt_filter_review_api_key_configured"`
	PromptFilterReviewAPIKeyCount      int     `json:"prompt_filter_review_api_key_count"`
	PromptFilterReviewBaseURL          string  `json:"prompt_filter_review_base_url"`
	PromptFilterReviewModel            string  `json:"prompt_filter_review_model"`
	PromptFilterReviewTimeoutSeconds   int     `json:"prompt_filter_review_timeout_seconds"`
	PromptFilterReviewFailClosed       bool    `json:"prompt_filter_review_fail_closed"`
	ClientCompatMode                   string  `json:"client_compat_mode"`
	CodexMinCLIVersion                 string  `json:"codex_min_cli_version"`
	CodexUserAgentConfig               string  `json:"codex_user_agent_config"`
	UsageLogMode                       string  `json:"usage_log_mode"`
	UsageLogBatchSize                  int     `json:"usage_log_batch_size"`
	UsageLogFlushIntervalSeconds       int     `json:"usage_log_flush_interval_seconds"`
	StreamFlushPolicy                  string  `json:"stream_flush_policy"`
	StreamFlushIntervalMS              int     `json:"stream_flush_interval_ms"`
	FirstTokenMode                     string  `json:"first_token_mode"`
	FirstTokenTimeoutSeconds           int     `json:"first_token_timeout_seconds"`
	BillingTierPolicy                  string  `json:"billing_tier_policy"`
	ShowFullUsageNumbers               bool    `json:"show_full_usage_numbers"`
	PublicKeyUsagePageEnabled          bool    `json:"public_key_usage_page_enabled"`
	ImageStorageBackend                string  `json:"image_storage_backend"`
	ImageS3Endpoint                    string  `json:"image_s3_endpoint"`
	ImageS3Region                      string  `json:"image_s3_region"`
	ImageS3Bucket                      string  `json:"image_s3_bucket"`
	ImageS3AccessKey                   string  `json:"image_s3_access_key"`
	ImageS3SecretKey                   string  `json:"image_s3_secret_key"`
	ImageS3Prefix                      string  `json:"image_s3_prefix"`
	ImageS3ForcePathStyle              bool    `json:"image_s3_force_path_style"`
	AutoPause5hThreshold               float64 `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold               float64 `json:"auto_pause_7d_threshold"`
	AutoPause5hGuardBandPercent        float64 `json:"auto_pause_5h_guard_band_percent"`
	AutoPause5hGuardConcurrency        int     `json:"auto_pause_5h_guard_concurrency"`
	SmartPacingEnabled                 bool    `json:"smart_pacing_enabled"`
	SmartPacingMinConcurrency          int     `json:"smart_pacing_min_concurrency"`
	SmartPacingWindows                 string  `json:"smart_pacing_windows"`
}

type updateSettingsReq struct {
	SiteName                           *string  `json:"site_name"`
	SiteLogo                           *string  `json:"site_logo"`
	BackgroundImage                    *string  `json:"background_image"`
	BackgroundOpacity                  *int     `json:"background_opacity"`
	BackgroundBlur                     *int     `json:"background_blur"`
	BackgroundGlassOpacity             *int     `json:"background_glass_opacity"`
	BackgroundGlassBlur                *int     `json:"background_glass_blur"`
	MaxConcurrency                     *int     `json:"max_concurrency"`
	GlobalRPM                          *int     `json:"global_rpm"`
	TestModel                          *string  `json:"test_model"`
	TestContent                        *string  `json:"test_content"`
	TestConcurrency                    *int     `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes   *int     `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes            *int     `json:"usage_probe_max_age_minutes"`
	UsageProbeConcurrency              *int     `json:"usage_probe_concurrency"`
	UsageProbeResponsesFallbackEnabled *bool    `json:"usage_probe_responses_fallback_enabled"`
	RecoveryProbeIntervalMinutes       *int     `json:"recovery_probe_interval_minutes"`
	LazyMode                           *bool    `json:"lazy_mode"`
	ProxyURL                           *string  `json:"proxy_url"`
	PgMaxConns                         *int     `json:"pg_max_conns"`
	RedisPoolSize                      *int     `json:"redis_pool_size"`
	AutoCleanUnauthorized              *bool    `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited               *bool    `json:"auto_clean_rate_limited"`
	AdminSecret                        *string  `json:"admin_secret"`
	AutoCleanFullUsage                 *bool    `json:"auto_clean_full_usage"`
	AutoCleanError                     *bool    `json:"auto_clean_error"`
	AutoCleanExpired                   *bool    `json:"auto_clean_expired"`
	ProxyPoolEnabled                   *bool    `json:"proxy_pool_enabled"`
	FastSchedulerEnabled               *bool    `json:"fast_scheduler_enabled"`
	CodexForceWebsocket                *bool    `json:"codex_force_websocket"`
	CodexWSKeepaliveEnabled            *bool    `json:"codex_ws_keepalive_enabled"`
	CodexWSKeepaliveIntervalSec        *int     `json:"codex_ws_keepalive_interval_sec"`
	CodexWSHideUpstreamErrors          *bool    `json:"codex_ws_hide_upstream_errors"`
	CodexWSSilentRetryEnabled          *bool    `json:"codex_ws_silent_retry_enabled"`
	CodexWSSilentMaxRetries            *int     `json:"codex_ws_silent_max_retries"`
	SchedulerMode                      *string  `json:"scheduler_mode"`
	AffinityMode                       *string  `json:"affinity_mode"`
	MaxRetries                         *int     `json:"max_retries"`
	MaxRateLimitRetries                *int     `json:"max_rate_limit_retries"`
	RetryIntervalMS                    *int     `json:"retry_interval_ms"`
	TransportRetryPolicy               *string  `json:"transport_retry_policy"`
	AllowRemoteMigration               *bool    `json:"allow_remote_migration"`
	ModelMapping                       *string  `json:"model_mapping"`
	CodexModelMapping                  *string  `json:"codex_model_mapping"`
	ReasoningEffortModels              *string  `json:"reasoning_effort_models"`
	ResinURL                           *string  `json:"resin_url"`
	ResinPlatformName                  *string  `json:"resin_platform_name"`
	PromptFilterEnabled                *bool    `json:"prompt_filter_enabled"`
	PromptFilterMode                   *string  `json:"prompt_filter_mode"`
	PromptFilterThreshold              *int     `json:"prompt_filter_threshold"`
	PromptFilterStrictThreshold        *int     `json:"prompt_filter_strict_threshold"`
	PromptFilterLogMatches             *bool    `json:"prompt_filter_log_matches"`
	PromptFilterMaxTextLength          *int     `json:"prompt_filter_max_text_length"`
	PromptFilterSensitiveWords         *string  `json:"prompt_filter_sensitive_words"`
	PromptFilterCustomPatterns         *string  `json:"prompt_filter_custom_patterns"`
	PromptFilterDisabledPatterns       *string  `json:"prompt_filter_disabled_patterns"`
	PromptFilterReviewEnabled          *bool    `json:"prompt_filter_review_enabled"`
	PromptFilterReviewAPIKey           *string  `json:"prompt_filter_review_api_key"`
	PromptFilterReviewBaseURL          *string  `json:"prompt_filter_review_base_url"`
	PromptFilterReviewModel            *string  `json:"prompt_filter_review_model"`
	PromptFilterReviewTimeoutSeconds   *int     `json:"prompt_filter_review_timeout_seconds"`
	PromptFilterReviewFailClosed       *bool    `json:"prompt_filter_review_fail_closed"`
	ClientCompatMode                   *string  `json:"client_compat_mode"`
	CodexMinCLIVersion                 *string  `json:"codex_min_cli_version"`
	CodexUserAgentConfig               *string  `json:"codex_user_agent_config"`
	UsageLogMode                       *string  `json:"usage_log_mode"`
	UsageLogBatchSize                  *int     `json:"usage_log_batch_size"`
	UsageLogFlushIntervalSeconds       *int     `json:"usage_log_flush_interval_seconds"`
	StreamFlushPolicy                  *string  `json:"stream_flush_policy"`
	StreamFlushIntervalMS              *int     `json:"stream_flush_interval_ms"`
	FirstTokenMode                     *string  `json:"first_token_mode"`
	FirstTokenTimeoutSeconds           *int     `json:"first_token_timeout_seconds"`
	BillingTierPolicy                  *string  `json:"billing_tier_policy"`
	ShowFullUsageNumbers               *bool    `json:"show_full_usage_numbers"`
	PublicKeyUsagePageEnabled          *bool    `json:"public_key_usage_page_enabled"`
	ImageStorageBackend                *string  `json:"image_storage_backend"`
	ImageS3Endpoint                    *string  `json:"image_s3_endpoint"`
	ImageS3Region                      *string  `json:"image_s3_region"`
	ImageS3Bucket                      *string  `json:"image_s3_bucket"`
	ImageS3AccessKey                   *string  `json:"image_s3_access_key"`
	ImageS3SecretKey                   *string  `json:"image_s3_secret_key"`
	ImageS3Prefix                      *string  `json:"image_s3_prefix"`
	ImageS3ForcePathStyle              *bool    `json:"image_s3_force_path_style"`
	AutoPause5hThreshold               *float64 `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold               *float64 `json:"auto_pause_7d_threshold"`
	AutoPause5hGuardBandPercent        *float64 `json:"auto_pause_5h_guard_band_percent"`
	AutoPause5hGuardConcurrency        *int     `json:"auto_pause_5h_guard_concurrency"`
	SmartPacingEnabled                 *bool    `json:"smart_pacing_enabled"`
	SmartPacingMinConcurrency          *int     `json:"smart_pacing_min_concurrency"`
	SmartPacingWindows                 *string  `json:"smart_pacing_windows"`
}

type brandingResponse struct {
	SiteName               string `json:"site_name"`
	SiteLogo               string `json:"site_logo"`
	BackgroundImage        string `json:"background_image"`
	BackgroundOpacity      int    `json:"background_opacity"`
	BackgroundBlur         int    `json:"background_blur"`
	BackgroundGlassOpacity int    `json:"background_glass_opacity"`
	BackgroundGlassBlur    int    `json:"background_glass_blur"`
}

const maxSiteLogoBytes = 600 * 1024
const maxBackgroundImageBytes = 2 * 1024 * 1024
const maxBackgroundVideoBytes = 40 * 1024 * 1024
const maxBackgroundImageAssetUploadBytes = 20 * 1024 * 1024
const maxBackgroundVideoAssetUploadBytes = 40 * 1024 * 1024
const maxBackgroundAssetUploadBytes = maxBackgroundVideoAssetUploadBytes
const maxSiteLogoURLChars = 4096
const maxBackgroundImageURLChars = 20000
const defaultBackgroundOpacity = 18
const maxBackgroundBlur = 24
const defaultBackgroundGlassOpacity = 58
const defaultBackgroundGlassBlur = 5
const maxBackgroundGlassBlur = 20
const defaultBackgroundAssetDir = "/data/backgrounds"
const backgroundAssetURLPrefix = "/p/backgrounds/"

type brandingBackgroundConfig struct {
	Image        string `json:"image"`
	Opacity      int    `json:"opacity"`
	Blur         int    `json:"blur"`
	GlassOpacity int    `json:"glass_opacity"`
	GlassBlur    int    `json:"glass_blur"`
}

type backgroundAssetUploadResponse struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Bytes    int    `json:"bytes"`
}

func normalizeSiteLogo(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:image/") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("网站图标 data URL 格式无效")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("网站图标 base64 数据无效")
		}
		if len(decoded) > maxSiteLogoBytes {
			return "", fmt.Errorf("网站图标不能超过 600KB")
		}
		return value, nil
	case strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"):
		if len(value) > maxSiteLogoURLChars {
			return "", fmt.Errorf("网站图标 URL 过长")
		}
		return value, nil
	case strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//"):
		if len(value) > maxSiteLogoURLChars {
			return "", fmt.Errorf("网站图标路径过长")
		}
		return value, nil
	default:
		return "", fmt.Errorf("网站图标仅支持 http(s) URL、站内路径或 data:image base64")
	}
}

func normalizeBackgroundImage(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:image/") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("背景图 data URL 格式无效")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("背景图 base64 数据无效")
		}
		if len(decoded) > maxBackgroundImageBytes {
			return "", fmt.Errorf("背景图不能超过 2MB")
		}
		return value, nil
	case strings.HasPrefix(lower, "data:video/mp4") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("动态壁纸 data URL 格式无效")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("动态壁纸 base64 数据无效")
		}
		if len(decoded) > maxBackgroundVideoBytes {
			return "", fmt.Errorf("动态壁纸不能超过 40MB")
		}
		return value, nil
	case strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"):
		if len(value) > maxBackgroundImageURLChars {
			return "", fmt.Errorf("背景图 URL 过长")
		}
		return value, nil
	case strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//"):
		if len(value) > maxBackgroundImageURLChars {
			return "", fmt.Errorf("背景图路径过长")
		}
		return value, nil
	default:
		return "", fmt.Errorf("背景仅支持 http(s) URL、站内路径、data:image base64 或 data:video/mp4 base64")
	}
}

func backgroundAssetDir() string {
	if dir := strings.TrimSpace(os.Getenv("BACKGROUND_ASSET_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("IMAGE_ASSET_DIR")); dir != "" {
		clean := filepath.Clean(dir)
		parent := filepath.Dir(clean)
		if parent != "." && parent != string(os.PathSeparator) {
			return filepath.Join(parent, "backgrounds")
		}
		return filepath.Join(clean, "backgrounds")
	}
	if dbPath := strings.TrimSpace(os.Getenv("DATABASE_PATH")); dbPath != "" {
		return filepath.Join(filepath.Dir(dbPath), "backgrounds")
	}
	return defaultBackgroundAssetDir
}

func backgroundAssetPath(filename string) (string, bool) {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "" || name == "." || name != strings.TrimSpace(filename) {
		return "", false
	}
	dir, err := filepath.Abs(backgroundAssetDir())
	if err != nil {
		return "", false
	}
	full, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func backgroundAssetURL(filename string) string {
	return backgroundAssetURLPrefix + filename
}

func validateConnectionTestContent(content string) (string, error) {
	normalized := auth.NormalizeTestContent(content)
	if len([]rune(normalized)) > auth.MaxTestContentRunes {
		return "", fmt.Errorf("test_content 不能超过 %d 个字符", auth.MaxTestContentRunes)
	}
	return normalized, nil
}

func randomBackgroundAssetFilename(ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	if ext == "" {
		ext = "bin"
	}
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d.%s", time.Now().UnixNano(), ext)
	}
	return fmt.Sprintf("%d-%s.%s", time.Now().UnixNano(), hex.EncodeToString(b), ext)
}

func declaredBackgroundMediaType(filename, contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/svg+xml", "video/mp4":
		if contentType == "image/jpg" {
			return "image/jpeg"
		}
		return contentType
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".mp4":
		return "video/mp4"
	default:
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); byExt != "" {
			return strings.ToLower(strings.TrimSpace(strings.Split(byExt, ";")[0]))
		}
		return ""
	}
}

func looksLikeSVG(data []byte) bool {
	sample := strings.ToLower(string(data))
	return strings.Contains(sample, "<svg") && !strings.Contains(sample, "<script")
}

func looksLikeWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

func looksLikeMP4(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

func normalizeBackgroundUploadMedia(filename, contentType string, data []byte) (string, string, error) {
	if len(data) == 0 {
		return "", "", fmt.Errorf("背景文件为空")
	}
	declared := declaredBackgroundMediaType(filename, contentType)
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	switch detected {
	case "image/png":
		return "image/png", "png", nil
	case "image/jpeg":
		return "image/jpeg", "jpg", nil
	case "image/webp":
		return "image/webp", "webp", nil
	}
	switch declared {
	case "image/webp":
		if looksLikeWebP(data) {
			return "image/webp", "webp", nil
		}
	case "image/svg+xml":
		if looksLikeSVG(data) {
			return "image/svg+xml", "svg", nil
		}
	case "video/mp4":
		if looksLikeMP4(data) {
			return "video/mp4", "mp4", nil
		}
	}
	return "", "", fmt.Errorf("背景仅支持 PNG、JPG、WebP、SVG 或 MP4")
}

func backgroundUploadLimitBytes(mimeType string) int {
	if mimeType == "video/mp4" {
		return maxBackgroundVideoAssetUploadBytes
	}
	return maxBackgroundImageAssetUploadBytes
}

func backgroundUploadTooLargeMessage(mimeType string) string {
	if mimeType == "video/mp4" {
		return "MP4 动态壁纸不能超过 40MB"
	}
	return "背景图片不能超过 20MB"
}

func (h *Handler) UploadBackgroundAsset(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "请选择背景文件")
		return
	}
	if fh.Size <= 0 {
		writeError(c, http.StatusBadRequest, "背景文件为空")
		return
	}
	if fh.Size > maxBackgroundAssetUploadBytes {
		writeError(c, http.StatusBadRequest, "背景文件不能超过 40MB")
		return
	}
	file, err := fh.Open()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBackgroundAssetUploadBytes+1))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(data) > maxBackgroundAssetUploadBytes {
		writeError(c, http.StatusBadRequest, "背景文件不能超过 40MB")
		return
	}
	mimeType, ext, err := normalizeBackgroundUploadMedia(fh.Filename, fh.Header.Get("Content-Type"), data)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) > backgroundUploadLimitBytes(mimeType) {
		writeError(c, http.StatusBadRequest, backgroundUploadTooLargeMessage(mimeType))
		return
	}

	if err := os.MkdirAll(backgroundAssetDir(), 0o755); err != nil {
		writeInternalError(c, fmt.Errorf("创建背景目录失败: %w", err))
		return
	}
	filename := randomBackgroundAssetFilename(ext)
	fullPath, ok := backgroundAssetPath(filename)
	if !ok {
		writeInternalError(c, fmt.Errorf("背景文件路径无效"))
		return
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		writeInternalError(c, fmt.Errorf("保存背景文件失败: %w", err))
		return
	}

	c.JSON(http.StatusOK, backgroundAssetUploadResponse{
		URL:      backgroundAssetURL(filename),
		Filename: filename,
		MimeType: mimeType,
		Bytes:    len(data),
	})
}

func (h *Handler) GetBackgroundAssetFile(c *gin.Context) {
	fullPath, ok := backgroundAssetPath(c.Param("filename"))
	if !ok {
		writeError(c, http.StatusNotFound, "背景文件不存在")
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		writeError(c, http.StatusNotFound, "背景文件不存在")
		return
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.File(fullPath)
}

func normalizeBackgroundOpacity(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeBackgroundBlur(value int) int {
	if value < 0 {
		return 0
	}
	if value > maxBackgroundBlur {
		return maxBackgroundBlur
	}
	return value
}

func normalizeBackgroundGlassOpacity(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeBackgroundGlassBlur(value int) int {
	if value < 0 {
		return 0
	}
	if value > maxBackgroundGlassBlur {
		return maxBackgroundGlassBlur
	}
	return value
}

func normalizeBackgroundConfig(cfg brandingBackgroundConfig) brandingBackgroundConfig {
	image, err := normalizeBackgroundImage(cfg.Image)
	if err != nil {
		image = ""
	}
	opacity := normalizeBackgroundOpacity(cfg.Opacity)
	if opacity == 0 && strings.TrimSpace(image) != "" && cfg.Opacity == 0 {
		opacity = 0
	}
	return brandingBackgroundConfig{
		Image:        image,
		Opacity:      opacity,
		Blur:         normalizeBackgroundBlur(cfg.Blur),
		GlassOpacity: normalizeBackgroundGlassOpacity(cfg.GlassOpacity),
		GlassBlur:    normalizeBackgroundGlassBlur(cfg.GlassBlur),
	}
}

func defaultBackgroundConfig() brandingBackgroundConfig {
	return brandingBackgroundConfig{
		Opacity:      defaultBackgroundOpacity,
		GlassOpacity: defaultBackgroundGlassOpacity,
		GlassBlur:    defaultBackgroundGlassBlur,
	}
}

func decodeBackgroundConfig(raw string) brandingBackgroundConfig {
	cfg := defaultBackgroundConfig()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultBackgroundConfig()
	}
	return normalizeBackgroundConfig(cfg)
}

func encodeBackgroundConfig(cfg brandingBackgroundConfig) string {
	cfg = normalizeBackgroundConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func brandingFromSettings(settings *database.SystemSettings) brandingResponse {
	resp := brandingResponse{SiteName: database.DefaultSiteName}
	bg := defaultBackgroundConfig()
	if settings == nil {
		resp.BackgroundOpacity = bg.Opacity
		resp.BackgroundGlassOpacity = bg.GlassOpacity
		resp.BackgroundGlassBlur = bg.GlassBlur
		return resp
	}
	resp.SiteName = database.NormalizeSiteName(settings.SiteName)
	resp.SiteLogo = strings.TrimSpace(settings.SiteLogo)
	bg = decodeBackgroundConfig(settings.BackgroundConfig)
	resp.BackgroundImage = bg.Image
	resp.BackgroundOpacity = bg.Opacity
	resp.BackgroundBlur = bg.Blur
	resp.BackgroundGlassOpacity = bg.GlassOpacity
	resp.BackgroundGlassBlur = bg.GlassBlur
	return resp
}

// GetBranding 获取公开站点品牌配置（无需管理密钥）。
func (h *Handler) GetBranding(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		log.Printf("读取站点品牌配置失败: %v", err)
		c.JSON(http.StatusOK, brandingFromSettings(nil))
		return
	}
	c.JSON(http.StatusOK, brandingFromSettings(settings))
}

// GetSettings 获取当前系统设置
func (h *Handler) GetSettings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	dbSettings, _ := h.db.GetSystemSettings(ctx)
	_, adminAuthSource := h.resolveAdminSecret(c.Request.Context())
	adminSecret := ""
	var resinURL, resinPlatformName string
	branding := brandingFromSettings(dbSettings)
	showFullUsageNumbers := false
	publicKeyUsagePageEnabled := true
	if dbSettings != nil && adminAuthSource != "env" {
		adminSecret = dbSettings.AdminSecret
	}
	if dbSettings != nil {
		resinURL = dbSettings.ResinURL
		resinPlatformName = dbSettings.ResinPlatformName
		showFullUsageNumbers = dbSettings.ShowFullUsageNumbers
		publicKeyUsagePageEnabled = dbSettings.PublicKeyUsagePageEnabled
	}
	promptFilterCfg := h.store.GetPromptFilterConfig()
	runtimeCfg := proxy.CurrentRuntimeSettings()
	imgCfg := imagestore.CurrentConfig()
	imgPrefix := strings.TrimSuffix(imgCfg.Prefix, "/")
	bgCfg := defaultBackgroundConfig()
	if dbSettings != nil {
		bgCfg = decodeBackgroundConfig(dbSettings.BackgroundConfig)
	}
	c.JSON(http.StatusOK, settingsResponse{
		SiteName:                           branding.SiteName,
		SiteLogo:                           branding.SiteLogo,
		BackgroundImage:                    bgCfg.Image,
		BackgroundOpacity:                  bgCfg.Opacity,
		BackgroundBlur:                     bgCfg.Blur,
		BackgroundGlassOpacity:             bgCfg.GlassOpacity,
		BackgroundGlassBlur:                bgCfg.GlassBlur,
		MaxConcurrency:                     h.store.GetMaxConcurrency(),
		GlobalRPM:                          h.rateLimiter.GetRPM(),
		TestModel:                          h.store.GetTestModel(),
		TestContent:                        h.store.GetTestContent(),
		TestConcurrency:                    h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes:   h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:            h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:              h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled: h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:       h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                           h.store.GetLazyMode(),
		ProxyURL:                           h.store.GetProxyURL(),
		PgMaxConns:                         h.pgMaxConns,
		RedisPoolSize:                      h.redisPoolSize,
		AutoCleanUnauthorized:              h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:               h.store.GetAutoCleanRateLimited(),
		AdminSecret:                        adminSecret,
		AdminAuthSource:                    adminAuthSource,
		AutoCleanFullUsage:                 h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                     h.store.GetAutoCleanError(),
		AutoCleanExpired:                   h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                   h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:               h.store.FastSchedulerEnabled(),
		CodexForceWebsocket:                h.store.CodexForceWebsocket(),
		CodexWSKeepaliveEnabled:            h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:        h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:          h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:          h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:            h.store.CodexWSSilentMaxRetries(),
		SchedulerMode:                      h.store.GetSchedulerMode(),
		AffinityMode:                       h.store.GetAffinityMode(),
		MaxRetries:                         h.store.GetMaxRetries(),
		MaxRateLimitRetries:                h.store.GetMaxRateLimitRetries(),
		RetryIntervalMS:                    h.store.GetRetryIntervalMS(),
		TransportRetryPolicy:               h.store.GetTransportRetryPolicy(),
		AllowRemoteMigration:               h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                     h.databaseDriver,
		DatabaseLabel:                      h.databaseLabel,
		CacheDriver:                        h.cacheDriver,
		CacheLabel:                         h.cacheLabel,
		ModelMapping:                       h.store.GetModelMapping(),
		CodexModelMapping:                  h.store.GetCodexModelMapping(),
		ReasoningEffortModels:              h.store.GetReasoningEffortModels(),
		ResinURL:                           resinURL,
		ResinPlatformName:                  resinPlatformName,
		PromptFilterEnabled:                promptFilterCfg.Enabled,
		PromptFilterMode:                   promptFilterCfg.Mode,
		PromptFilterThreshold:              promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:        promptFilterCfg.StrictThreshold,
		PromptFilterLogMatches:             promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:          promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:         promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:         promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PromptFilterDisabledPatterns:       promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:          promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKeyConfigured: promptFilterCfg.Review.APIKey != "",
		PromptFilterReviewAPIKeyCount:      len(promptFilterCfg.Review.APIKeyList()),
		PromptFilterReviewBaseURL:          promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:            promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:   promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:       promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                   runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                 runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:               runtimeCfg.CodexUserAgentConfig,
		UsageLogMode:                       h.db.GetUsageLogMode(),
		UsageLogBatchSize:                  h.db.GetUsageLogBatchSize(),
		UsageLogFlushIntervalSeconds:       h.db.GetUsageLogFlushIntervalSeconds(),
		StreamFlushPolicy:                  runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:              runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                     runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:           runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                  runtimeCfg.BillingTierPolicy,
		ShowFullUsageNumbers:               showFullUsageNumbers,
		PublicKeyUsagePageEnabled:          publicKeyUsagePageEnabled,
		ImageStorageBackend:                imgCfg.Backend,
		ImageS3Endpoint:                    imgCfg.Endpoint,
		ImageS3Region:                      imgCfg.Region,
		ImageS3Bucket:                      imgCfg.Bucket,
		ImageS3AccessKey:                   imgCfg.AccessKey,
		ImageS3SecretKey:                   imgCfg.SecretKey,
		ImageS3Prefix:                      imgPrefix,
		ImageS3ForcePathStyle:              imgCfg.ForcePathStyle,
		AutoPause5hThreshold:               h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:               h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:        h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:        h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                 h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:          h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                 h.store.GetSmartPacingWindows(),
	})
}

// UpdateSettings 更新系统设置（实时生效）
func (h *Handler) UpdateSettings(c *gin.Context) {
	var req updateSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.AutoPause5hThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_5h_threshold", *req.AutoPause5hThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.AutoPause7dThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_7d_threshold", *req.AutoPause7dThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.AutoPause5hGuardBandPercent != nil {
		if *req.AutoPause5hGuardBandPercent < 0 || *req.AutoPause5hGuardBandPercent > 100 {
			writeError(c, http.StatusBadRequest, "auto_pause_5h_guard_band_percent 需在 0 到 100 之间")
			return
		}
	}
	if req.AutoPause5hGuardConcurrency != nil {
		if *req.AutoPause5hGuardConcurrency < 0 || *req.AutoPause5hGuardConcurrency > 1000 {
			writeError(c, http.StatusBadRequest, "auto_pause_5h_guard_concurrency 需在 0 到 1000 之间")
			return
		}
	}
	if req.SmartPacingMinConcurrency != nil {
		if *req.SmartPacingMinConcurrency < 1 || *req.SmartPacingMinConcurrency > 1000 {
			writeError(c, http.StatusBadRequest, "smart_pacing_min_concurrency 需在 1 到 1000 之间")
			return
		}
	}
	if req.SmartPacingWindows != nil {
		switch strings.ToLower(strings.TrimSpace(*req.SmartPacingWindows)) {
		case "5h,7d", "7d,5h", "5h", "7d", "":
		default:
			writeError(c, http.StatusBadRequest, "smart_pacing_windows 仅支持 5h,7d / 5h / 7d")
			return
		}
	}

	currentAdminSecret := ""
	siteName := database.DefaultSiteName
	siteLogo := ""
	bgCfg := defaultBackgroundConfig()
	showFullUsageNumbers := false
	publicKeyUsagePageEnabled := true
	existingSettings, _ := h.db.GetSystemSettings(c.Request.Context())
	if existingSettings != nil {
		currentAdminSecret = existingSettings.AdminSecret
		siteName = database.NormalizeSiteName(existingSettings.SiteName)
		siteLogo = strings.TrimSpace(existingSettings.SiteLogo)
		bgCfg = decodeBackgroundConfig(existingSettings.BackgroundConfig)
		showFullUsageNumbers = existingSettings.ShowFullUsageNumbers
		publicKeyUsagePageEnabled = existingSettings.PublicKeyUsagePageEnabled
	}
	if req.AdminSecret != nil {
		if h.adminSecretEnv == "" {
			currentAdminSecret = *req.AdminSecret
			log.Printf("设置已更新: admin_secret (长度=%d)", len(currentAdminSecret))
		} else {
			log.Printf("检测到环境变量 ADMIN_SECRET，忽略前端提交的 admin_secret")
		}
	}
	if req.SiteName != nil {
		siteName = database.NormalizeSiteName(*req.SiteName)
		log.Printf("设置已更新: site_name = %s", siteName)
	}
	if req.SiteLogo != nil {
		normalized, err := normalizeSiteLogo(*req.SiteLogo)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		siteLogo = normalized
		log.Printf("设置已更新: site_logo (长度=%d)", len(siteLogo))
	}
	if req.BackgroundImage != nil {
		normalized, err := normalizeBackgroundImage(*req.BackgroundImage)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		bgCfg.Image = normalized
		log.Printf("设置已更新: background_image (长度=%d)", len(bgCfg.Image))
	}
	if req.BackgroundOpacity != nil {
		bgCfg.Opacity = normalizeBackgroundOpacity(*req.BackgroundOpacity)
		log.Printf("设置已更新: background_opacity = %d", bgCfg.Opacity)
	}
	if req.BackgroundBlur != nil {
		bgCfg.Blur = normalizeBackgroundBlur(*req.BackgroundBlur)
		log.Printf("设置已更新: background_blur = %d", bgCfg.Blur)
	}
	if req.BackgroundGlassOpacity != nil {
		bgCfg.GlassOpacity = normalizeBackgroundGlassOpacity(*req.BackgroundGlassOpacity)
		log.Printf("设置已更新: background_glass_opacity = %d", bgCfg.GlassOpacity)
	}
	if req.BackgroundGlassBlur != nil {
		bgCfg.GlassBlur = normalizeBackgroundGlassBlur(*req.BackgroundGlassBlur)
		log.Printf("设置已更新: background_glass_blur = %d", bgCfg.GlassBlur)
	}
	hasAdminSecret := strings.TrimSpace(currentAdminSecret) != "" || strings.TrimSpace(h.adminSecretEnv) != ""
	runtimeCfg := proxy.CurrentRuntimeSettings()
	usageLogMode := h.db.GetUsageLogMode()
	usageLogBatchSize := h.db.GetUsageLogBatchSize()
	usageLogFlushIntervalSeconds := h.db.GetUsageLogFlushIntervalSeconds()

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

	if req.TestContent != nil {
		testContent, err := validateConnectionTestContent(*req.TestContent)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		h.store.SetTestContent(testContent)
		log.Printf("设置已更新: test_content (长度=%d)", len([]rune(testContent)))
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

	if req.UsageProbeConcurrency != nil {
		v := *req.UsageProbeConcurrency
		if v < 1 {
			v = 1
		}
		if v > 128 {
			v = 128
		}
		h.store.SetUsageProbeConcurrency(v)
		log.Printf("设置已更新: usage_probe_concurrency = %d", v)
	}

	if req.UsageProbeResponsesFallbackEnabled != nil {
		h.store.SetUsageProbeResponsesFallbackEnabled(*req.UsageProbeResponsesFallbackEnabled)
		log.Printf("设置已更新: usage_probe_responses_fallback_enabled = %t", *req.UsageProbeResponsesFallbackEnabled)
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

	if req.LazyMode != nil {
		h.store.SetLazyMode(*req.LazyMode)
		log.Printf("设置已更新: lazy_mode = %t", *req.LazyMode)
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
		if *req.ProxyPoolEnabled {
			if err := h.store.ReloadProxyPool(); err != nil {
				writeError(c, http.StatusInternalServerError, "代理池刷新失败: "+err.Error())
				return
			}
		}
		h.store.SetProxyPoolEnabled(*req.ProxyPoolEnabled)
		log.Printf("设置已更新: proxy_pool_enabled = %t", *req.ProxyPoolEnabled)
	}

	if req.FastSchedulerEnabled != nil {
		h.store.SetFastSchedulerEnabled(*req.FastSchedulerEnabled)
		log.Printf("设置已更新: fast_scheduler_enabled = %t", *req.FastSchedulerEnabled)
	}

	if req.CodexForceWebsocket != nil {
		h.store.SetCodexForceWebsocket(*req.CodexForceWebsocket)
		runtimeCfg.CodexForceWebsocket = *req.CodexForceWebsocket
		log.Printf("设置已更新: codex_force_websocket = %t", *req.CodexForceWebsocket)
	}

	if req.CodexWSKeepaliveEnabled != nil {
		h.store.SetCodexWSKeepaliveEnabled(*req.CodexWSKeepaliveEnabled)
		log.Printf("设置已更新: codex_ws_keepalive_enabled = %t", *req.CodexWSKeepaliveEnabled)
	}

	if req.CodexWSKeepaliveIntervalSec != nil {
		h.store.SetCodexWSKeepaliveIntervalSec(*req.CodexWSKeepaliveIntervalSec)
		log.Printf("设置已更新: codex_ws_keepalive_interval_sec = %d", *req.CodexWSKeepaliveIntervalSec)
	}

	if req.CodexWSHideUpstreamErrors != nil {
		h.store.SetCodexWSHideUpstreamErrors(*req.CodexWSHideUpstreamErrors)
		runtimeCfg.CodexWSHideErrors = *req.CodexWSHideUpstreamErrors
		log.Printf("设置已更新: codex_ws_hide_upstream_errors = %t", *req.CodexWSHideUpstreamErrors)
	}

	if req.CodexWSSilentRetryEnabled != nil {
		h.store.SetCodexWSSilentRetryEnabled(*req.CodexWSSilentRetryEnabled)
		runtimeCfg.CodexWSSilentRetry = *req.CodexWSSilentRetryEnabled
		log.Printf("设置已更新: codex_ws_silent_retry_enabled = %t", *req.CodexWSSilentRetryEnabled)
	}

	if req.CodexWSSilentMaxRetries != nil {
		v := *req.CodexWSSilentMaxRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		h.store.SetCodexWSSilentMaxRetries(v)
		runtimeCfg.CodexWSSilentRetries = v
		log.Printf("设置已更新: codex_ws_silent_max_retries = %d", v)
	}

	if req.SchedulerMode != nil {
		h.store.SetSchedulerMode(*req.SchedulerMode)
		log.Printf("设置已更新: scheduler_mode = %s", *req.SchedulerMode)
	}

	if req.AffinityMode != nil {
		h.store.SetAffinityMode(*req.AffinityMode)
		log.Printf("设置已更新: affinity_mode = %s", *req.AffinityMode)
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

	if req.MaxRateLimitRetries != nil {
		v := *req.MaxRateLimitRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		h.store.SetMaxRateLimitRetries(v)
		log.Printf("设置已更新: max_rate_limit_retries = %d", v)
	}

	if req.RetryIntervalMS != nil {
		v := *req.RetryIntervalMS
		if v < 0 {
			v = 0
		}
		if v > 30000 {
			v = 30000
		}
		h.store.SetRetryIntervalMS(v)
		log.Printf("设置已更新: retry_interval_ms = %d", v)
	}

	if req.TransportRetryPolicy != nil {
		v := database.NormalizeTransportRetryPolicy(*req.TransportRetryPolicy)
		h.store.SetTransportRetryPolicy(v)
		log.Printf("设置已更新: transport_retry_policy = %s", v)
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
	if req.CodexModelMapping != nil {
		h.store.SetCodexModelMapping(*req.CodexModelMapping)
		log.Printf("设置已更新: codex_model_mapping")
	}
	if req.ReasoningEffortModels != nil {
		normalized, err := proxy.NormalizeReasoningEffortModelsJSON(*req.ReasoningEffortModels, proxy.SupportedModelIDs(c.Request.Context(), h.db))
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		h.store.SetReasoningEffortModels(normalized)
		log.Printf("设置已更新: reasoning_effort_models")
	}

	if req.ClientCompatMode != nil {
		runtimeCfg.ClientCompatMode = proxy.NormalizeClientCompatMode(*req.ClientCompatMode)
		log.Printf("设置已更新: client_compat_mode = %s", runtimeCfg.ClientCompatMode)
	}
	if req.CodexMinCLIVersion != nil {
		runtimeCfg.CodexMinCLIVersion = strings.TrimSpace(*req.CodexMinCLIVersion)
		log.Printf("设置已更新: codex_min_cli_version = %s", runtimeCfg.CodexMinCLIVersion)
	}
	if req.CodexUserAgentConfig != nil {
		normalized, err := proxy.NormalizeCodexUserAgentConfigJSON(*req.CodexUserAgentConfig)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		runtimeCfg.CodexUserAgentConfig = normalized
		log.Printf("设置已更新: codex_user_agent_config")
	}
	if req.StreamFlushPolicy != nil {
		runtimeCfg.StreamFlushPolicy = proxy.NormalizeStreamFlushPolicy(*req.StreamFlushPolicy)
		log.Printf("设置已更新: stream_flush_policy = %s", runtimeCfg.StreamFlushPolicy)
	}
	if req.StreamFlushIntervalMS != nil {
		runtimeCfg.StreamFlushIntervalMS = *req.StreamFlushIntervalMS
		log.Printf("设置已更新: stream_flush_interval_ms = %d", runtimeCfg.StreamFlushIntervalMS)
	}
	if req.FirstTokenMode != nil {
		runtimeCfg.FirstTokenMode = proxy.NormalizeFirstTokenMode(*req.FirstTokenMode)
		log.Printf("设置已更新: first_token_mode = %s", runtimeCfg.FirstTokenMode)
	}
	if req.FirstTokenTimeoutSeconds != nil {
		runtimeCfg.FirstTokenTimeoutSec = *req.FirstTokenTimeoutSeconds
		log.Printf("设置已更新: first_token_timeout_seconds = %d", runtimeCfg.FirstTokenTimeoutSec)
	}
	if req.BillingTierPolicy != nil {
		runtimeCfg.BillingTierPolicy = proxy.NormalizeBillingTierPolicy(*req.BillingTierPolicy)
		log.Printf("设置已更新: billing_tier_policy = %s", runtimeCfg.BillingTierPolicy)
	}
	if req.ShowFullUsageNumbers != nil {
		showFullUsageNumbers = *req.ShowFullUsageNumbers
		log.Printf("设置已更新: show_full_usage_numbers = %t", showFullUsageNumbers)
	}
	if req.PublicKeyUsagePageEnabled != nil {
		publicKeyUsagePageEnabled = *req.PublicKeyUsagePageEnabled
		log.Printf("设置已更新: public_key_usage_page_enabled = %t", publicKeyUsagePageEnabled)
	}
	if req.AutoPause5hThreshold != nil || req.AutoPause7dThreshold != nil {
		t5h := h.store.GetGlobalAutoPause5hThreshold()
		t7d := h.store.GetGlobalAutoPause7dThreshold()
		if req.AutoPause5hThreshold != nil {
			t5h = *req.AutoPause5hThreshold
		}
		if req.AutoPause7dThreshold != nil {
			t7d = *req.AutoPause7dThreshold
		}
		h.store.SetGlobalAutoPauseThresholds(t5h, t7d)
		log.Printf("设置已更新: auto_pause thresholds 5h=%.4f 7d=%.4f", t5h, t7d)
	}
	if req.AutoPause5hGuardBandPercent != nil {
		h.store.SetAutoPause5hGuardBandPercent(*req.AutoPause5hGuardBandPercent)
		log.Printf("设置已更新: auto_pause_5h_guard_band_percent = %.2f", *req.AutoPause5hGuardBandPercent)
	}
	if req.AutoPause5hGuardConcurrency != nil {
		h.store.SetAutoPause5hGuardConcurrency(*req.AutoPause5hGuardConcurrency)
		log.Printf("设置已更新: auto_pause_5h_guard_concurrency = %d", *req.AutoPause5hGuardConcurrency)
	}
	if req.SmartPacingEnabled != nil {
		h.store.SetSmartPacingEnabled(*req.SmartPacingEnabled)
		log.Printf("设置已更新: smart_pacing_enabled = %t", *req.SmartPacingEnabled)
	}
	if req.SmartPacingMinConcurrency != nil {
		h.store.SetSmartPacingMinConcurrency(*req.SmartPacingMinConcurrency)
		log.Printf("设置已更新: smart_pacing_min_concurrency = %d", *req.SmartPacingMinConcurrency)
	}
	if req.SmartPacingWindows != nil {
		h.store.SetSmartPacingWindows(*req.SmartPacingWindows)
		log.Printf("设置已更新: smart_pacing_windows = %s", h.store.GetSmartPacingWindows())
	}
	runtimeCfg = proxy.ApplyRuntimeSettings(runtimeCfg)

	usageLogChanged := false
	if req.UsageLogMode != nil {
		usageLogMode = database.NormalizeUsageLogMode(*req.UsageLogMode)
		usageLogChanged = true
		log.Printf("设置已更新: usage_log_mode = %s", usageLogMode)
	}
	if req.UsageLogBatchSize != nil {
		usageLogBatchSize = database.NormalizeUsageLogBatchSize(*req.UsageLogBatchSize)
		usageLogChanged = true
		log.Printf("设置已更新: usage_log_batch_size = %d", usageLogBatchSize)
	}
	if req.UsageLogFlushIntervalSeconds != nil {
		usageLogFlushIntervalSeconds = database.NormalizeUsageLogFlushIntervalSeconds(*req.UsageLogFlushIntervalSeconds)
		usageLogChanged = true
		log.Printf("设置已更新: usage_log_flush_interval_seconds = %d", usageLogFlushIntervalSeconds)
	}
	if usageLogChanged {
		h.db.SetUsageLogConfig(usageLogMode, usageLogBatchSize, usageLogFlushIntervalSeconds)
		usageLogMode = h.db.GetUsageLogMode()
		usageLogBatchSize = h.db.GetUsageLogBatchSize()
		usageLogFlushIntervalSeconds = h.db.GetUsageLogFlushIntervalSeconds()
	}

	promptFilterCfg := h.store.GetPromptFilterConfig()
	promptFilterChanged := false
	if req.PromptFilterEnabled != nil {
		promptFilterCfg.Enabled = *req.PromptFilterEnabled
		promptFilterChanged = true
	}
	if req.PromptFilterMode != nil {
		promptFilterCfg.Mode = *req.PromptFilterMode
		promptFilterChanged = true
	}
	if req.PromptFilterThreshold != nil {
		promptFilterCfg.Threshold = *req.PromptFilterThreshold
		promptFilterChanged = true
	}
	if req.PromptFilterStrictThreshold != nil {
		promptFilterCfg.StrictThreshold = *req.PromptFilterStrictThreshold
		promptFilterChanged = true
	}
	if req.PromptFilterLogMatches != nil {
		promptFilterCfg.LogMatches = *req.PromptFilterLogMatches
		promptFilterChanged = true
	}
	if req.PromptFilterMaxTextLength != nil {
		promptFilterCfg.MaxTextLength = *req.PromptFilterMaxTextLength
		promptFilterChanged = true
	}
	if req.PromptFilterSensitiveWords != nil {
		promptFilterCfg.SensitiveWords = *req.PromptFilterSensitiveWords
		promptFilterChanged = true
	}
	if req.PromptFilterCustomPatterns != nil {
		patterns, err := promptfilter.ParseCustomPatterns(*req.PromptFilterCustomPatterns)
		if err != nil {
			writeError(c, http.StatusBadRequest, "Prompt 检查自定义规则 JSON 无效: "+err.Error())
			return
		}
		promptFilterCfg.CustomPatterns = patterns
		promptFilterChanged = true
	}
	if req.PromptFilterDisabledPatterns != nil {
		disabled, err := promptfilter.ParseDisabledPatterns(*req.PromptFilterDisabledPatterns)
		if err != nil {
			writeError(c, http.StatusBadRequest, "Prompt 检查禁用规则 JSON 无效: "+err.Error())
			return
		}
		promptFilterCfg.DisabledPatterns = disabled
		promptFilterChanged = true
	}
	if req.PromptFilterReviewEnabled != nil {
		promptFilterCfg.Review.Enabled = *req.PromptFilterReviewEnabled
		promptFilterChanged = true
	}
	if req.PromptFilterReviewAPIKey != nil {
		if key := strings.TrimSpace(*req.PromptFilterReviewAPIKey); key != "" {
			promptFilterCfg.Review.APIKey = key
			promptFilterChanged = true
		}
	}
	if req.PromptFilterReviewBaseURL != nil {
		promptFilterCfg.Review.BaseURL = strings.TrimSpace(*req.PromptFilterReviewBaseURL)
		promptFilterChanged = true
	}
	if req.PromptFilterReviewModel != nil {
		promptFilterCfg.Review.Model = strings.TrimSpace(*req.PromptFilterReviewModel)
		promptFilterChanged = true
	}
	if req.PromptFilterReviewTimeoutSeconds != nil {
		promptFilterCfg.Review.TimeoutSeconds = *req.PromptFilterReviewTimeoutSeconds
		promptFilterChanged = true
	}
	if req.PromptFilterReviewFailClosed != nil {
		promptFilterCfg.Review.FailClosed = *req.PromptFilterReviewFailClosed
		promptFilterChanged = true
	}
	if promptFilterChanged {
		promptFilterCfg = promptfilter.NormalizeConfig(promptFilterCfg)
		if promptFilterCfg.Review.Enabled && strings.TrimSpace(promptFilterCfg.Review.APIKey) == "" {
			writeError(c, http.StatusBadRequest, "Prompt 检查二次审查已启用时必须填写审查 API Key")
			return
		}
		if err := promptfilter.ValidateReviewConfig(promptFilterCfg.Review); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt 检查审查配置无效: "+err.Error())
			return
		}
		if _, err := promptfilter.NewEngine(promptFilterCfg); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt 检查规则无效: "+err.Error())
			return
		}
		h.store.SetPromptFilterConfig(promptFilterCfg)
		log.Printf("设置已更新: prompt_filter enabled=%t mode=%s threshold=%d", promptFilterCfg.Enabled, promptFilterCfg.Mode, promptFilterCfg.Threshold)
	}

	// Resin 粘性代理池配置
	resinURL := ""
	resinPlatformName := ""
	if existingSettings != nil {
		resinURL = existingSettings.ResinURL
		resinPlatformName = existingSettings.ResinPlatformName
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

	// 图片存储后端配置
	imgCfg := imagestore.CurrentConfig()
	imgChanged := false
	if req.ImageStorageBackend != nil {
		imgCfg.Backend = *req.ImageStorageBackend
		imgChanged = true
	}
	if req.ImageS3Endpoint != nil {
		imgCfg.Endpoint = *req.ImageS3Endpoint
		imgChanged = true
	}
	if req.ImageS3Region != nil {
		imgCfg.Region = *req.ImageS3Region
		imgChanged = true
	}
	if req.ImageS3Bucket != nil {
		imgCfg.Bucket = *req.ImageS3Bucket
		imgChanged = true
	}
	if req.ImageS3AccessKey != nil {
		imgCfg.AccessKey = *req.ImageS3AccessKey
		imgChanged = true
	}
	if req.ImageS3SecretKey != nil {
		imgCfg.SecretKey = *req.ImageS3SecretKey
		imgChanged = true
	}
	if req.ImageS3Prefix != nil {
		imgCfg.Prefix = *req.ImageS3Prefix
		imgChanged = true
	}
	if req.ImageS3ForcePathStyle != nil {
		imgCfg.ForcePathStyle = *req.ImageS3ForcePathStyle
		imgChanged = true
	}
	imgCfg.LocalDir = imageAssetDir()
	if imgChanged {
		if err := imagestore.Configure(imgCfg); err != nil {
			writeError(c, http.StatusBadRequest, "图片存储配置无效: "+err.Error())
			return
		}
		// Configure 内部 Normalize 过，重新读出来用于持久化
		imgCfg = imagestore.CurrentConfig()
		log.Printf("设置已更新: image_storage_backend = %s", imgCfg.Backend)
	}
	imgConfigJSON, encodeErr := imagestore.EncodeConfigJSON(imgCfg)
	if encodeErr != nil {
		log.Printf("图片存储配置序列化失败: %v", encodeErr)
		imgConfigJSON = "{}"
	}

	// 持久化保存到数据库
	err := h.db.UpdateSystemSettings(c.Request.Context(), &database.SystemSettings{
		SiteName:                           siteName,
		SiteLogo:                           siteLogo,
		MaxConcurrency:                     h.store.GetMaxConcurrency(),
		GlobalRPM:                          h.rateLimiter.GetRPM(),
		TestModel:                          h.store.GetTestModel(),
		TestContent:                        h.store.GetTestContent(),
		TestConcurrency:                    h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes:   h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:            h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:              h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled: h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:       h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                           h.store.GetLazyMode(),
		ProxyURL:                           h.store.GetProxyURL(),
		PgMaxConns:                         h.pgMaxConns,
		RedisPoolSize:                      h.redisPoolSize,
		AutoCleanUnauthorized:              h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:               h.store.GetAutoCleanRateLimited(),
		AdminSecret:                        currentAdminSecret,
		AutoCleanFullUsage:                 h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                     h.store.GetAutoCleanError(),
		AutoCleanExpired:                   h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                   h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:               h.store.FastSchedulerEnabled(),
		CodexForceWebsocket:                h.store.CodexForceWebsocket(),
		CodexWSKeepaliveEnabled:            h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:        h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:          h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:          h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:            h.store.CodexWSSilentMaxRetries(),
		SchedulerMode:                      h.store.GetSchedulerMode(),
		AffinityMode:                       h.store.GetAffinityMode(),
		MaxRetries:                         h.store.GetMaxRetries(),
		MaxRateLimitRetries:                h.store.GetMaxRateLimitRetries(),
		RetryIntervalMS:                    h.store.GetRetryIntervalMS(),
		TransportRetryPolicy:               h.store.GetTransportRetryPolicy(),
		AllowRemoteMigration:               h.store.GetAllowRemoteMigration() && hasAdminSecret,
		ModelMapping:                       h.store.GetModelMapping(),
		CodexModelMapping:                  h.store.GetCodexModelMapping(),
		ReasoningEffortModels:              h.store.GetReasoningEffortModels(),
		ResinURL:                           resinURL,
		ResinPlatformName:                  resinPlatformName,
		PromptFilterEnabled:                promptFilterCfg.Enabled,
		PromptFilterMode:                   promptFilterCfg.Mode,
		PromptFilterThreshold:              promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:        promptFilterCfg.StrictThreshold,
		PromptFilterLogMatches:             promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:          promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:         promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:         promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PromptFilterDisabledPatterns:       promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:          promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKey:           promptFilterCfg.Review.APIKey,
		PromptFilterReviewBaseURL:          promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:            promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:   promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:       promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                   runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                 runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:               runtimeCfg.CodexUserAgentConfig,
		UsageLogMode:                       usageLogMode,
		UsageLogBatchSize:                  usageLogBatchSize,
		UsageLogFlushIntervalSeconds:       usageLogFlushIntervalSeconds,
		StreamFlushPolicy:                  runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:              runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                     runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:           runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                  runtimeCfg.BillingTierPolicy,
		ShowFullUsageNumbers:               showFullUsageNumbers,
		PublicKeyUsagePageEnabled:          publicKeyUsagePageEnabled,
		ImageStorageConfig:                 imgConfigJSON,
		BackgroundConfig:                   encodeBackgroundConfig(bgCfg),
		AutoPause5hThreshold:               h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:               h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:        h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:        h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                 h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:          h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                 h.store.GetSmartPacingWindows(),
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
		SiteName:                           siteName,
		SiteLogo:                           siteLogo,
		BackgroundImage:                    bgCfg.Image,
		BackgroundOpacity:                  bgCfg.Opacity,
		BackgroundBlur:                     bgCfg.Blur,
		BackgroundGlassOpacity:             bgCfg.GlassOpacity,
		BackgroundGlassBlur:                bgCfg.GlassBlur,
		MaxConcurrency:                     h.store.GetMaxConcurrency(),
		GlobalRPM:                          h.rateLimiter.GetRPM(),
		TestModel:                          h.store.GetTestModel(),
		TestContent:                        h.store.GetTestContent(),
		TestConcurrency:                    h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes:   h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:            h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:              h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled: h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:       h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                           h.store.GetLazyMode(),
		ProxyURL:                           h.store.GetProxyURL(),
		PgMaxConns:                         h.pgMaxConns,
		RedisPoolSize:                      h.redisPoolSize,
		AutoCleanUnauthorized:              h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:               h.store.GetAutoCleanRateLimited(),
		AdminSecret:                        adminSecretForDisplay,
		AdminAuthSource:                    adminAuthSource,
		AutoCleanFullUsage:                 h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                     h.store.GetAutoCleanError(),
		AutoCleanExpired:                   h.store.GetAutoCleanExpired(),
		ProxyPoolEnabled:                   h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:               h.store.FastSchedulerEnabled(),
		CodexForceWebsocket:                h.store.CodexForceWebsocket(),
		CodexWSKeepaliveEnabled:            h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:        h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:          h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:          h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:            h.store.CodexWSSilentMaxRetries(),
		SchedulerMode:                      h.store.GetSchedulerMode(),
		AffinityMode:                       h.store.GetAffinityMode(),
		MaxRetries:                         h.store.GetMaxRetries(),
		MaxRateLimitRetries:                h.store.GetMaxRateLimitRetries(),
		AllowRemoteMigration:               h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                     h.databaseDriver,
		DatabaseLabel:                      h.databaseLabel,
		CacheDriver:                        h.cacheDriver,
		CacheLabel:                         h.cacheLabel,
		ExpiredCleaned:                     expiredCleaned,
		ModelMapping:                       h.store.GetModelMapping(),
		CodexModelMapping:                  h.store.GetCodexModelMapping(),
		ReasoningEffortModels:              h.store.GetReasoningEffortModels(),
		ResinURL:                           resinURL,
		ResinPlatformName:                  resinPlatformName,
		PromptFilterEnabled:                promptFilterCfg.Enabled,
		PromptFilterMode:                   promptFilterCfg.Mode,
		PromptFilterThreshold:              promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:        promptFilterCfg.StrictThreshold,
		PromptFilterLogMatches:             promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:          promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:         promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:         promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PromptFilterDisabledPatterns:       promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:          promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKeyConfigured: promptFilterCfg.Review.APIKey != "",
		PromptFilterReviewAPIKeyCount:      len(promptFilterCfg.Review.APIKeyList()),
		PromptFilterReviewBaseURL:          promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:            promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:   promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:       promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                   runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                 runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:               runtimeCfg.CodexUserAgentConfig,
		UsageLogMode:                       usageLogMode,
		UsageLogBatchSize:                  usageLogBatchSize,
		UsageLogFlushIntervalSeconds:       usageLogFlushIntervalSeconds,
		StreamFlushPolicy:                  runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:              runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                     runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:           runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                  runtimeCfg.BillingTierPolicy,
		ShowFullUsageNumbers:               showFullUsageNumbers,
		ImageStorageBackend:                imgCfg.Backend,
		ImageS3Endpoint:                    imgCfg.Endpoint,
		ImageS3Region:                      imgCfg.Region,
		ImageS3Bucket:                      imgCfg.Bucket,
		ImageS3AccessKey:                   imgCfg.AccessKey,
		ImageS3SecretKey:                   imgCfg.SecretKey,
		ImageS3Prefix:                      strings.TrimSuffix(imgCfg.Prefix, "/"),
		ImageS3ForcePathStyle:              imgCfg.ForcePathStyle,
		AutoPause5hThreshold:               h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:               h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:        h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:        h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                 h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:          h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                 h.store.GetSmartPacingWindows(),
	})
}

type testImageStorageReq struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	AccessKey      string `json:"access_key"`
	SecretKey      string `json:"secret_key"`
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
}

// TestImageStorageConnection 用提交的字段临时构造一次 S3Backend，调用 HeadBucket 验证可达性。
// 不修改任何持久化状态，便于"保存前先点测试连接"。
func (h *Handler) TestImageStorageConnection(c *gin.Context) {
	var req testImageStorageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	cfg := imagestore.Config{
		Backend:        imagestore.BackendS3,
		Endpoint:       req.Endpoint,
		Region:         req.Region,
		Bucket:         req.Bucket,
		AccessKey:      req.AccessKey,
		SecretKey:      req.SecretKey,
		Prefix:         req.Prefix,
		ForcePathStyle: req.ForcePathStyle,
	}.Normalize()
	if err := cfg.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	backend, err := imagestore.NewS3Backend(cfg)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := backend.HeadBucket(ctx); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "bucket": cfg.Bucket})
}

// ==================== 导出 & 迁移 ====================

type cpaExportEntry struct {
	Type                  string `json:"type"`
	Email                 string `json:"email"`
	PlanType              string `json:"plan_type,omitempty"`
	Codex7DUsedPercent    string `json:"codex_7d_used_percent,omitempty"`
	Codex7DResetAt        string `json:"codex_7d_reset_at,omitempty"`
	Codex5HUsedPercent    string `json:"codex_5h_used_percent,omitempty"`
	Codex5HResetAt        string `json:"codex_5h_reset_at,omitempty"`
	Codex5HUsageUpdatedAt string `json:"codex_5h_usage_updated_at,omitempty"`
	CodexUsageUpdatedAt   string `json:"codex_usage_updated_at,omitempty"`
	Expired               string `json:"expired"`
	IDToken               string `json:"id_token"`
	AccountID             string `json:"account_id"`
	AccessToken           string `json:"access_token"`
	LastRefresh           string `json:"last_refresh"`
	RefreshToken          string `json:"refresh_token"`
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
		if errors.Is(err, sql.ErrNoRows) {
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
		at := row.GetCredential("access_token")
		// AT-only accounts (没有 refresh_token,只靠 access_token,常用于规避
		// add-phone 的 Plus 号) 也需要可导出与可迁移。仅当两个凭证都缺失才跳过。
		if rt == "" && at == "" {
			continue
		}
		// account_id 在凭据中存储为 chatgpt_account_id（新字段）或 account_id（历史字段）
		accountID := row.GetCredential("chatgpt_account_id")
		if accountID == "" {
			accountID = row.GetCredential("account_id")
		}
		entries = append(entries, cpaExportEntry{
			Type:                  "codex",
			Email:                 row.GetCredential("email"),
			PlanType:              row.GetCredential("plan_type"),
			Codex7DUsedPercent:    row.GetCredential("codex_7d_used_percent"),
			Codex7DResetAt:        row.GetCredential("codex_7d_reset_at"),
			Codex5HUsedPercent:    row.GetCredential("codex_5h_used_percent"),
			Codex5HResetAt:        row.GetCredential("codex_5h_reset_at"),
			Codex5HUsageUpdatedAt: row.GetCredential("codex_5h_usage_updated_at"),
			CodexUsageUpdatedAt:   row.GetCredential("codex_usage_updated_at"),
			Expired:               row.GetCredential("expires_at"),
			IDToken:               row.GetCredential("id_token"),
			AccountID:             accountID,
			AccessToken:           at,
			LastRefresh:           row.UpdatedAt.Format(time.RFC3339),
			RefreshToken:          rt,
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
	parsedURL, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeError(c, http.StatusBadRequest, "url 必须是完整的 http/https 地址")
		return
	}

	remoteURL := strings.TrimRight(parsedURL.String(), "/")
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

	// 转换为 importToken 格式，复用 importAccountsCommon (原生支持 AT-only 混合导入)
	var tokens []importToken
	for _, entry := range remoteAccounts {
		rt := strings.TrimSpace(entry.RefreshToken)
		at := strings.TrimSpace(entry.AccessToken)
		// 至少需要一种凭证;两者都为空表示账号根本没有可用凭证。
		if rt == "" && at == "" {
			continue
		}
		name := entry.Email
		if name == "" {
			name = "migrate"
		}
		tokens = append(tokens, importToken{
			refreshToken:          rt,
			accessToken:           at,
			name:                  name,
			email:                 strings.TrimSpace(entry.Email),
			idToken:               strings.TrimSpace(entry.IDToken),
			accountID:             strings.TrimSpace(entry.AccountID),
			planType:              strings.TrimSpace(entry.PlanType),
			expiresAt:             strings.TrimSpace(entry.Expired),
			codex7DUsedPercent:    strings.TrimSpace(entry.Codex7DUsedPercent),
			codex7DResetAt:        strings.TrimSpace(entry.Codex7DResetAt),
			codex5HUsedPercent:    strings.TrimSpace(entry.Codex5HUsedPercent),
			codex5HResetAt:        strings.TrimSpace(entry.Codex5HResetAt),
			codex5HUsageUpdatedAt: strings.TrimSpace(entry.Codex5HUsageUpdatedAt),
			codexUsageUpdatedAt:   strings.TrimSpace(entry.CodexUsageUpdatedAt),
		})
	}

	log.Printf("远程迁移: 从 %s 拉取到 %d 个账号，开始导入", remoteURL, len(tokens))
	h.importAccountsCommon(c, tokens, "", false)
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

// CleanRateLimited 一键清理所有限流账号（含 premium 5h、free 7d、usage_exhausted）
func (h *Handler) CleanRateLimited(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	cleaned := h.store.CleanRateLimitedManual(ctx)

	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已清理 %d 个账号", cleaned), "cleaned": cleaned})
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
		writeError(c, http.StatusInternalServerError, "添加代理失败: "+err.Error())
		return
	}

	if err := h.store.ReloadProxyPool(); err != nil {
		log.Printf("代理已添加，但代理池刷新失败: %v", err)
	}

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
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "代理不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "删除代理失败")
		return
	}

	if err := h.store.ReloadProxyPool(); err != nil {
		log.Printf("代理已删除，但代理池刷新失败: %v", err)
	}
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
		URL     *string `json:"url"`
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.UpdateProxy(ctx, id, req.URL, req.Label, req.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "代理不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "更新代理失败")
		return
	}

	if err := h.store.ReloadProxyPool(); err != nil {
		log.Printf("代理已更新，但代理池刷新失败: %v", err)
	}
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
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请提供代理 URL")
		return
	}
	proxyURL := strings.TrimSpace(req.URL)
	if proxyURL == "" {
		writeError(c, http.StatusBadRequest, "请提供代理 URL")
		return
	}

	// 创建使用指定代理的 HTTP client
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
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
		if err := h.db.UpdateProxyTestResult(ctx, req.ID, ip, location, latencyMs); err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "error": "代理测试结果保存失败: " + err.Error(), "latency_ms": latencyMs})
			return
		}
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
