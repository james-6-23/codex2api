package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Handler API 路由处理器
type Handler struct {
	store      *auth.Store
	configKeys map[string]bool // 配置文件中的静态 key
	db         *database.DB
	cfg        *config.Config       // 全局配置
	deviceCfg  *DeviceProfileConfig // 设备指纹配置

	// 动态 key 缓存
	dbKeysMu    sync.RWMutex
	dbKeys      map[string]*database.APIKeyRow
	dbKeysUntil time.Time
}

func (h *Handler) nextAccountForSession(sessionID string, apiKeyID int64, exclude map[int64]bool) (*auth.Account, string) {
	return h.nextAccountForSessionWithFilter(sessionID, apiKeyID, exclude, nil)
}

func (h *Handler) nextAccountForSessionWithFilter(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	return h.store.NextForSessionWithFilter(sessionID, apiKeyID, exclude, filter)
}

func (h *Handler) shouldUseWebsocketForHTTP() bool {
	if h == nil || h.cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(h.cfg.CodexUpstreamTransport)) {
	case "ws":
		return true
	case "http", "auto":
		return false
	default:
		return h.cfg.UseWebsocket
	}
}

func (h *Handler) resolveProxyForAttempt(account *auth.Account, stickyProxyURL string) string {
	if proxyURL := strings.TrimSpace(stickyProxyURL); proxyURL != "" {
		return proxyURL
	}
	if h == nil || h.store == nil {
		return ""
	}
	return h.store.ResolveProxyForAccount(account)
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

type CodexUsageSyncResult struct {
	UsagePct7d           float64
	HasUsage7d           bool
	UsagePct5h           float64
	Reset5hAt            time.Time
	HasUsage5h           bool
	Used5hHeaders        bool
	Persisted5hOnly      bool
	Premium5hRateLimited bool
}

type codexRateLimitWindow string

const (
	codexRateLimitWindowUnknown codexRateLimitWindow = ""
	codexRateLimitWindowShort   codexRateLimitWindow = "short"
	codexRateLimitWindow5h      codexRateLimitWindow = "5h"
	codexRateLimitWindow7d      codexRateLimitWindow = "7d"
)

type codex429Decision struct {
	Premium5h bool
	ResetAt   time.Time
	Cooldown  time.Duration
}

const (
	contextAPIKeyID     = "apiKeyID"
	contextAPIKeyName   = "apiKeyName"
	contextAPIKeyMasked = "apiKeyMasked"
)

func requestAPIKeyID(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	if value, exists := c.Get(contextAPIKeyID); exists && value != nil {
		switch typed := value.(type) {
		case int64:
			return typed
		case int:
			return int64(typed)
		}
	}
	return 0
}

func sessionAffinityKey(sessionID string, apiKeyID int64) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || apiKeyID <= 0 {
		return sessionID
	}
	return fmt.Sprintf("%s::api-key:%d", sessionID, apiKeyID)
}

const proOnlySparkModel = "gpt-5.3-codex-spark"

func isProOnlyModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), proOnlySparkModel)
}

func accountFilterForModel(model string) auth.AccountFilter {
	if !isProOnlyModel(model) {
		return nil
	}
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(account.GetPlanType()), "pro")
	}
}

func effectiveRequestModel(body []byte, fallback string) string {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model != "" {
		return model
	}
	return strings.TrimSpace(fallback)
}

func noAvailableAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "无可用 Pro 账号，gpt-5.3-codex-spark 仅支持 Pro 订阅账号"
	}
	return "无可用账号，请稍后重试"
}

func noAvailableAnthropicAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "No available Pro account for gpt-5.3-codex-spark"
	}
	return "No available accounts, please retry later"
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	return &Handler{
		store:      store,
		configKeys: make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:         db,
		cfg:        cfg,
		deviceCfg:  deviceCfg,
	}
}

// NewHandlerWithDeviceProfile 创建处理器（带设备指纹配置）
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, nil, deviceCfg)
}

// refreshDBKeys 从数据库刷新密钥缓存（5 分钟）
func (h *Handler) refreshDBKeys() map[string]*database.APIKeyRow {
	h.dbKeysMu.RLock()
	if time.Now().Before(h.dbKeysUntil) {
		keys := h.dbKeys
		h.dbKeysMu.RUnlock()
		return keys
	}
	h.dbKeysMu.RUnlock()

	h.dbKeysMu.Lock()
	defer h.dbKeysMu.Unlock()

	// double check
	if time.Now().Before(h.dbKeysUntil) {
		return h.dbKeys
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		log.Printf("刷新 API Keys 缓存失败: %v", err)
		return h.dbKeys
	}

	newMap := make(map[string]*database.APIKeyRow, len(rows))
	for _, row := range rows {
		if row == nil || row.Key == "" {
			continue
		}
		newMap[row.Key] = row
	}
	h.dbKeys = newMap
	h.dbKeysUntil = time.Now().Add(5 * time.Minute)
	return newMap
}

func (h *Handler) resolveAPIKey(key string) (*database.APIKeyRow, bool) {
	if h.configKeys[key] {
		return &database.APIKeyRow{
			ID:   0,
			Name: "config",
			Key:  key,
		}, true
	}
	dbKeys := h.refreshDBKeys()
	row, ok := dbKeys[key]
	return row, ok
}

// isValidKey 检查 key 是否有效（配置文件 + DB）
func (h *Handler) isValidKey(key string) bool {
	_, ok := h.resolveAPIKey(key)
	return ok
}

// hasAnyKeys 检查是否配置了任何密钥
func (h *Handler) hasAnyKeys() bool {
	if len(h.configKeys) > 0 {
		return true
	}
	dbKeys := h.refreshDBKeys()
	return len(dbKeys) > 0
}

// logUsage 记录请求日志（非阻塞，写入内存缓冲由后台批量 flush）
func (h *Handler) logUsage(input *database.UsageLogInput) {
	if h.db == nil || input == nil {
		return
	}
	_ = h.db.InsertUsageLog(context.Background(), input)
}

func populateAPIKeyMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func (h *Handler) logUsageForRequest(c *gin.Context, input *database.UsageLogInput) {
	populateAPIKeyMetaFromContext(c, input)
	h.logUsage(input)
}

// extractReasoningEffort 从请求体提取推理强度
// 支持 reasoning.effort（Responses API）和 reasoning_effort（Chat Completions API）
func extractReasoningEffort(body []byte) string {
	// Responses API: reasoning.effort
	if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "" {
		return effort
	}
	// Chat Completions API: reasoning_effort
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" {
		return effort
	}
	return ""
}

// extractServiceTier 从请求体提取服务等级
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "transport"
}

func classifyHTTPFailure(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized:
		return "unauthorized"
	case statusCode == http.StatusTooManyRequests:
		return "" // 429 由 applyCooldown 单独处理
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

type streamOutcome struct {
	logStatusCode  int
	failureKind    string
	failureMessage string
	penalize       bool
}

func classifyStreamOutcome(ctxErr, readErr, writeErr error, gotTerminal bool) streamOutcome {
	if gotTerminal {
		return streamOutcome{logStatusCode: http.StatusOK}
	}

	if ctxErr != nil || writeErr != nil {
		msg := "下游客户端提前断开"
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			msg = "下游请求上下文超时"
		case writeErr != nil:
			msg = fmt.Sprintf("写回下游失败: %v", writeErr)
		case ctxErr != nil:
			msg = fmt.Sprintf("下游请求提前取消: %v", ctxErr)
		}
		return streamOutcome{
			logStatusCode:  logStatusClientClosed,
			failureMessage: msg,
		}
	}

	if readErr != nil {
		kind := classifyTransportFailure(readErr)
		if kind == "" {
			kind = "transport"
		}
		return streamOutcome{
			logStatusCode:  logStatusUpstreamStreamBreak,
			failureKind:    kind,
			failureMessage: fmt.Sprintf("上游流读取失败: %v", readErr),
			penalize:       true,
		}
	}

	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "上游流提前结束，未收到终止事件",
		penalize:       true,
	}
}

func shouldTransparentRetryStream(outcome streamOutcome, attempt int, maxRetries int, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if attempt >= maxRetries {
		return false
	}
	if !outcome.penalize {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

func imageGenerationOutputKey(item gjson.Result) string {
	if key := strings.TrimSpace(item.Get("id").String()); key != "" {
		return key
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return ""
	}
	return strings.TrimSpace(item.Get("output_format").String()) + "|" + result
}

func extractResponseImageGenerationOutput(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := imageGenerationOutputKey(item)
	if key != "" && seen != nil {
		if _, ok := seen[key]; ok {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	raw := []byte(item.Raw)
	var output map[string]any
	if err := json.Unmarshal(raw, &output); err == nil && addImageStatsToMap(output) {
		if annotated, err := json.Marshal(output); err == nil {
			raw = annotated
		}
	}
	return json.RawMessage(raw), true
}

func appendMissingResponseImageOutputs(responseJSON []byte, imageOutputs []json.RawMessage) []byte {
	if len(responseJSON) == 0 {
		return responseJSON
	}
	var response map[string]any
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return responseJSON
	}

	seen := make(map[string]struct{})
	changed := false
	outputs, _ := response["output"].([]any)
	for _, rawOutput := range outputs {
		outputMap, ok := rawOutput.(map[string]any)
		if !ok {
			continue
		}
		if firstNonEmptyAnyString(outputMap["type"]) != "image_generation_call" {
			continue
		}
		outputBytes, err := json.Marshal(outputMap)
		if err != nil {
			continue
		}
		item := gjson.ParseBytes(outputBytes)
		if key := imageGenerationOutputKey(item); key != "" {
			seen[key] = struct{}{}
		}
		if addImageStatsToMap(outputMap) {
			changed = true
		}
	}

	for _, rawImage := range imageOutputs {
		if len(rawImage) == 0 || !gjson.ValidBytes(rawImage) {
			continue
		}
		item := gjson.ParseBytes(rawImage)
		key := imageGenerationOutputKey(item)
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		var decoded any
		if err := json.Unmarshal(rawImage, &decoded); err != nil {
			continue
		}
		if outputMap, ok := decoded.(map[string]any); ok {
			addImageStatsToMap(outputMap)
		}
		outputs = append(outputs, decoded)
		changed = true
	}
	if !changed {
		return responseJSON
	}
	response["output"] = outputs
	merged, err := json.Marshal(response)
	if err != nil {
		return responseJSON
	}
	return merged
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	auth := h.authMiddleware()

	// /v1 前缀路由（标准路径）
	v1 := r.Group("/v1")
	v1.Use(auth)
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.POST("/responses/compact", h.ResponsesCompact)
	v1.POST("/images/generations", h.ImagesGenerations)
	v1.POST("/images/edits", h.ImagesEdits)
	v1.POST("/messages", h.Messages)
	v1.GET("/models", h.ListModels)

	// 无前缀路由（兼容 base_url 已包含 /v1 的客户端）
	r.POST("/chat/completions", auth, h.ChatCompletions)
	r.POST("/responses", auth, h.Responses)
	r.POST("/responses/compact", auth, h.ResponsesCompact)
	r.POST("/images/generations", auth, h.ImagesGenerations)
	r.POST("/images/edits", auth, h.ImagesEdits)
	r.POST("/messages", auth, h.Messages)
	r.GET("/models", auth, h.ListModels)

	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(auth)
	codexDirect.POST("/responses", h.Responses)
	codexDirect.POST("/responses/*subpath", func(c *gin.Context) {
		subpath := strings.TrimSpace(c.Param("subpath"))
		if subpath == "/compact" || strings.HasPrefix(subpath, "/compact/") {
			h.ResponsesCompact(c)
			return
		}
		h.Responses(c)
	})
}

// authMiddleware API Key 鉴权中间件（增强版，带安全日志）
//
// 安全策略（fail-closed）：
//   - 默认情况下，未配置任何 API Key 时直接拒绝请求（503），避免裸奔账号池。
//   - 仅当显式设置 CODEX_ALLOW_ANONYMOUS=true 时才在无密钥情况下放行（兼容内网/测试）。
func (h *Handler) authMiddleware() gin.HandlerFunc {
	allowAnonymous := h.cfg != nil && h.cfg.AllowAnonymousV1
	return func(c *gin.Context) {
		// 如果没有配置任何密钥
		if !h.hasAnyKeys() {
			if allowAnonymous {
				// 显式允许匿名访问（旧行为，仅在 CODEX_ALLOW_ANONYMOUS=true 时启用）
				c.Next()
				return
			}
			// fail-closed：未配置 API Key 即拒绝，避免账号池被未授权调用
			security.SecurityAuditLog("V1_BLOCKED_NO_KEYS", fmt.Sprintf("path=%s ip=%s", c.Request.URL.Path, c.ClientIP()))
			api.SendError(c, api.NewAPIError(
				api.ErrCodeServiceUnavailable,
				"Service is not configured: no API key has been created yet. Please add at least one API key in the admin dashboard, or set CODEX_ALLOW_ANONYMOUS=true to disable this check.",
				api.ErrorTypeServer,
			))
			c.Abort()
			return
		}

		authHeader := c.GetHeader("Authorization")
		// 兼容 Anthropic 客户端的多种认证方式:
		// - x-api-key: Anthropic SDK 默认方式
		// - ANTHROPIC_AUTH_TOKEN: Claude Code 通过此环境变量设置，
		//   实际发送为 Authorization: Bearer <token>（已被上面覆盖）
		//   或 anthropic-auth-token 自定义 header
		if authHeader == "" {
			for _, h := range []string{"x-api-key", "anthropic-auth-token"} {
				if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
					authHeader = "Bearer " + v
					break
				}
			}
		}
		if authHeader == "" {
			// Use standardized error format from api package
			api.SendError(c, api.ErrMissingAPIKey)
			c.Abort()
			return
		}

		// 清理输入
		authHeader = security.SanitizeInput(authHeader)

		key := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		apiKeyRow, ok := h.resolveAPIKey(key)
		if !ok {
			// 记录安全审计日志（脱敏）
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			// Use standardized error format from api package
			api.SendError(c, api.ErrInvalidAPIKey)
			c.Abort()
			return
		}
		c.Set(contextAPIKeyID, apiKeyRow.ID)
		c.Set(contextAPIKeyName, strings.TrimSpace(apiKeyRow.Name))
		c.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKeyRow.Key))
		c.Set("apiKey", key)
		c.Next()
	}
}

// ==================== /v1/responses ====================

// getMaxRetries 从 store 读取可配置的最大重试次数
func (h *Handler) getMaxRetries() int {
	return h.store.GetMaxRetries()
}

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
)

// isRetryableStatus 检查是否可重试的上游状态码
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable || code == http.StatusUnauthorized || code == http.StatusInternalServerError
}

func parseUsageLimitDetails(body []byte) (usageLimitDetails, bool) {
	if len(body) == 0 {
		return usageLimitDetails{}, false
	}
	if gjson.GetBytes(body, "error.type").String() != "usage_limit_reached" {
		return usageLimitDetails{}, false
	}
	return usageLimitDetails{
		message:         gjson.GetBytes(body, "error.message").String(),
		planType:        gjson.GetBytes(body, "error.plan_type").String(),
		resetsAt:        gjson.GetBytes(body, "error.resets_at").Int(),
		resetsInSeconds: gjson.GetBytes(body, "error.resets_in_seconds").Int(),
	}, true
}

// Responses 处理 /v1/responses 请求（原生透传，增强输入验证）
func (h *Handler) Responses(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(h.supportedModelIDs(c.Request.Context())))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}

	// 2. 准备上游请求体（Unmarshal→map→Marshal，一次序列化）
	codexBody, expandedInputRaw := PrepareResponsesBody(rawBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	accountFilter := accountFilterForModel(effectiveModel)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, excludeAccounts, accountFilter)
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": noAvailableAccountMessage(effectiveModel), "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		useWebsocket := h.shouldUseWebsocketForHTTP()

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionID)
		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, model, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/responses", model, errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/responses",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/responses",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！透传响应并跟踪 TTFT / usage
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		var responseJSON []byte
		var imageLogInfo imageUsageLogInfo

		if isStream {
			// 流式透传 + TTFT 跟踪
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// TTFT: 记录第一个 output_text.delta 事件的时间
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if image, ok := extractImageFromOutputItemDone(data, model); ok {
					imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
				}

				// 提取 usage + service_tier
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
					writeErr = err
					return false
				}
				wroteAnyBody = true
				flusher.Flush()
				return eventType != "response.completed" && eventType != "response.failed"
			})
		} else {
			// 非流式收集
			var lastResponseData []byte
			imageOutputs := make([]json.RawMessage, 0, 1)
			seenImageOutputs := make(map[string]struct{})
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if imageOutput, ok := extractResponseImageGenerationOutput(data, seenImageOutputs); ok {
					imageOutputs = append(imageOutputs, imageOutput)
				}
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					gotTerminal = true
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					responseJSON = []byte(responseObj.Raw)
					responseJSON = appendMissingResponseImageOutputs(responseJSON, imageOutputs)
					imageLogInfo = imageUsageLogInfoFromResponseJSON(responseJSON)
				}
			}
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			SyncCodexUsageState(h.store, account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/responses, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3 // 粗略估算: 约 3 字符 = 1 token
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if !isStream {
			if responseJSON != nil {
				c.Data(http.StatusOK, "application/json", responseJSON)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		applyImageUsageLogInfo(logInput, imageLogInfo)
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		SyncCodexUsageState(h.store, account, resp)
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

// ResponsesCompact 处理 /v1/responses/compact 请求（非流式压缩接口，透传到上游 /responses/compact）
func (h *Handler) ResponsesCompact(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(h.supportedModelIDs(c.Request.Context())))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if isImageOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses/compact", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}

	// compact 强制非流式
	rawBody, _ = sjson.SetBytes(rawBody, "stream", false)

	// 准备上游请求体
	codexBody, _ := PrepareCompactResponsesBody(rawBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	accountFilter := accountFilterForModel(effectiveModel)

	// 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, excludeAccounts, accountFilter)
		if account == nil {
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": noAvailableAccountMessage(effectiveModel), "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionID)
		resp, reqErr := ExecuteCompactRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("compact 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			logUpstreamError("/v1/responses/compact", resp.StatusCode, model, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/responses/compact", model, errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/responses/compact",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/responses/compact",
				UpstreamEndpoint: "/v1/responses/compact",
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功：直接透传响应体
		SyncCodexUsageState(h.store, account, resp)

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 提取 usage 用于日志
		promptTokens := int(gjson.GetBytes(respBody, "usage.input_tokens").Int())
		completionTokens := int(gjson.GetBytes(respBody, "usage.output_tokens").Int())
		totalTokens := int(gjson.GetBytes(respBody, "usage.total_tokens").Int())
		reasoningTokens := int(gjson.GetBytes(respBody, "usage.output_tokens_details.reasoning_tokens").Int())
		cachedTokens := int(gjson.GetBytes(respBody, "usage.input_tokens_details.cached_tokens").Int())

		actualServiceTier := gjson.GetBytes(respBody, "service_tier").String()
		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)

		totalDuration := int(time.Since(start).Milliseconds())
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses/compact",
			Model:            model,
			StatusCode:       http.StatusOK,
			DurationMs:       totalDuration,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
			InputTokens:      promptTokens,
			OutputTokens:     completionTokens,
			ReasoningTokens:  reasoningTokens,
			CachedTokens:     cachedTokens,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses/compact",
			UpstreamEndpoint: "/v1/responses/compact",
			ServiceTier:      resolvedServiceTier,
		})

		h.store.Release(account)
		c.Data(http.StatusOK, "application/json", respBody)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(h.supportedModelIDs(c.Request.Context())))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		model = "gpt-5.4"
	}
	if isImageOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}

	// 2. 翻译请求：OpenAI Chat → Codex Responses
	codexBody, err := TranslateRequest(rawBody)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request translation failed: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	accountFilter := accountFilterForModel(effectiveModel)

	sessionID := ResolveSessionID(c.Request.Header, codexBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionID, apiKeyID)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool) // 重试时排除已失败的账号

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, excludeAccounts, accountFilter)
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": noAvailableAccountMessage(effectiveModel), "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		useWebsocket := h.shouldUseWebsocketForHTTP()

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionID)
		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			// 不可重试的结构化错误直接返回
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, model, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/chat/completions", model, errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/chat/completions",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/chat/completions",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
				ServiceTier:      serviceTier,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！翻译响应 + TTFT 跟踪
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		var compactResult []byte

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		created := time.Now().Unix()

		if isStream {
			streamTranslator := NewStreamTranslator(chunkID, model, created)
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				chunk, done := streamTranslator.Translate(data)

				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数（文本 + function call 参数）
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				if chunk != nil {
					if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", chunk); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
					flusher.Flush()
				}
				if done {
					if _, err := fmt.Fprintf(c.Writer, "data: [DONE]\n\n"); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
					flusher.Flush()
					return false
				}
				return true
			})
		} else {
			var fullContent strings.Builder
			var toolCalls []ToolCallResult

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				switch eventType {
				case "response.output_text.delta":
					delta := parsed.Get("delta").String()
					deltaCharCount += len(delta)
					fullContent.WriteString(delta)
				case "response.function_call_arguments.delta":
					deltaCharCount += len(parsed.Get("delta").String())
				case "response.completed":
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 从 response.output 提取 function_call 项
					toolCalls = ExtractToolCallsFromOutput(data)
					gotTerminal = true
					return false
				case "response.failed":
					gotTerminal = true
					return false
				}
				return true
			})

			compactResult = BuildCompactResponse(chunkID, model, created, fullContent.String(), toolCalls, usage)
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			SyncCodexUsageState(h.store, account, resp)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/chat/completions, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if !isStream {
			if compactResult != nil {
				c.Data(http.StatusOK, "application/json", compactResult)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		resolvedServiceTier := resolveServiceTier(actualServiceTier, serviceTier)
		c.Set("x-service-tier", resolvedServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
			ServiceTier:      resolvedServiceTier,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		SyncCodexUsageState(h.store, account, resp)
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
	}
}

// handleStreamResponse 处理流式响应（翻译 Codex → OpenAI）
func (h *Handler) handleStreamResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "streaming not supported", "type": "server_error"},
		})
		return
	}

	err := ReadSSEStream(body, func(data []byte) bool {
		chunk, done := TranslateStreamChunk(data, model, chunkID, created)
		if chunk != nil {
			fmt.Fprintf(c.Writer, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		if done {
			fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			return false
		}
		return true
	})

	if err != nil {
		log.Printf("读取上游流失败: %v", err)
	}
}

// handleCompactResponse 处理非流式响应
func (h *Handler) handleCompactResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	var fullContent strings.Builder
	var usage *UsageInfo

	_ = ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			fullContent.WriteString(delta)
		case "response.completed":
			usage = extractUsage(data)
			return false
		case "response.failed":
			return false
		}
		return true
	})

	result := BuildCompactResponse(chunkID, model, created, fullContent.String(), nil, usage)

	c.Data(http.StatusOK, "application/json", result)
}

// ==================== 通用辅助 ====================

// parseRetryAfter 解析上游 429 响应中的重试时间（参考 CLIProxyAPI codex_executor.go:689-708）
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 2 * time.Minute
	}

	// 解析 error.resets_at (Unix timestamp)
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			d := time.Until(resetTime)
			if d > 0 {
				return d
			}
		}
	}

	// 解析 error.resets_in_seconds
	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return time.Duration(secs) * time.Second
	}

	// 默认 2 分钟
	return 2 * time.Minute
}

func isMissingScopeUnauthorized(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code != "missing_scope" {
		return false
	}

	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "api.responses.write") {
		return true
	}

	return strings.Contains(msg, "scope")
}

func parseRetryAfterResetAt(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}

	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(now) {
			return resetTime, true
		}
	}

	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return now.Add(time.Duration(secs) * time.Second), true
	}

	return time.Time{}, false
}

func codexWindowType(windowMinutes float64) codexRateLimitWindow {
	switch {
	case windowMinutes >= 1440:
		return codexRateLimitWindow7d
	case windowMinutes >= 60:
		return codexRateLimitWindow5h
	case windowMinutes > 0:
		return codexRateLimitWindowShort
	default:
		return codexRateLimitWindowUnknown
	}
}

type codexWindowUsage struct {
	usedPct   float64
	resetSec  float64
	windowMin float64
	valid     bool
}

func parseCodexWindowUsage(usedStr, windowStr, resetStr string) codexWindowUsage {
	if usedStr == "" {
		return codexWindowUsage{}
	}
	return codexWindowUsage{
		usedPct:   parseFloat(usedStr),
		windowMin: parseFloat(windowStr),
		resetSec:  parseFloat(resetStr),
		valid:     true,
	}
}

func classifyCodex429Window(resp *http.Response, now time.Time) (codexRateLimitWindow, time.Time, bool) {
	if resp == nil {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)

	var exhausted []codexWindowUsage
	if primary.valid && primary.usedPct >= 100 {
		exhausted = append(exhausted, primary)
	}
	if secondary.valid && secondary.usedPct >= 100 {
		exhausted = append(exhausted, secondary)
	}
	if len(exhausted) == 0 {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	chosen := exhausted[0]
	for _, candidate := range exhausted[1:] {
		if candidate.windowMin > chosen.windowMin {
			chosen = candidate
		}
	}

	var resetAt time.Time
	if chosen.resetSec > 0 {
		resetAt = now.Add(time.Duration(chosen.resetSec) * time.Second)
	}
	return codexWindowType(chosen.windowMin), resetAt, !resetAt.IsZero()
}

func responseHasCodex5hHeaders(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	if primary.valid && codexWindowType(primary.windowMin) == codexRateLimitWindow5h {
		return true
	}

	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)
	return secondary.valid && codexWindowType(secondary.windowMin) == codexRateLimitWindow5h
}

func classify429RateLimit(account *auth.Account, body []byte, resp *http.Response, now time.Time) codex429Decision {
	if account != nil && account.IsPremium5hPlan() {
		windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
		exactResetAt, hasExactReset := parseRetryAfterResetAt(body, now)

		switch windowType {
		case codexRateLimitWindow5h:
			if hasExactReset {
				resetAt = exactResetAt
			} else if !hasWindowReset {
				resetAt = now.Add(5 * time.Hour)
			}
			return codex429Decision{
				Premium5h: true,
				ResetAt:   resetAt,
				Cooldown:  time.Until(resetAt),
			}
		case codexRateLimitWindow7d, codexRateLimitWindowShort:
			// 明确不是 5h 窗口时，保持原有 cooldown 语义。
		default:
			if hasExactReset {
				return codex429Decision{
					Premium5h: true,
					ResetAt:   exactResetAt,
					Cooldown:  time.Until(exactResetAt),
				}
			}
			resetAt = now.Add(5 * time.Hour)
			return codex429Decision{
				Premium5h: true,
				ResetAt:   resetAt,
				Cooldown:  5 * time.Hour,
			}
		}
	}

	cooldown := compute429Cooldown(account, body, resp)
	return codex429Decision{Cooldown: cooldown}
}

// Apply429Cooldown 统一处理 429 对账号状态的影响，premium 5h 场景优先写入显式限流态。
func Apply429Cooldown(store *auth.Store, account *auth.Account, body []byte, resp *http.Response) codex429Decision {
	decision := classify429RateLimit(account, body, resp, time.Now())
	if store == nil || account == nil {
		return decision
	}
	if decision.Premium5h {
		store.MarkPremium5hRateLimited(account, decision.ResetAt)
		return decision
	}
	store.MarkCooldown(account, decision.Cooldown, "rate_limited")
	return decision
}

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	switch statusCode {
	case http.StatusTooManyRequests:
		decision := Apply429Cooldown(h.store, account, body, resp)
		if decision.Premium5h {
			log.Printf("账号 %d 触发 premium 5h 限流 (plan=%s)，重置时间 %s", account.ID(), account.GetPlanType(), decision.ResetAt.Format(time.RFC3339))
			return
		}
		log.Printf("账号 %d 被限速 (plan=%s)，冷却 %v", account.ID(), account.GetPlanType(), decision.Cooldown)
	case http.StatusUnauthorized:
		// 原子标志瞬间置位，阻止其他并发请求再选到该账号
		atomic.StoreInt32(&account.Disabled, 1)

		if isMissingScopeUnauthorized(body) {
			log.Printf("账号 %d 收到 missing_scope 401，保留在号池", account.ID())
			atomic.StoreInt32(&account.Disabled, 0)
			return
		}

		if h.store.GetAutoCleanUnauthorized() {
			// 开启自动清理时，401 立即从号池删除
			log.Printf("账号 %d 收到 401，立即清理", account.ID())
			if h.db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.SetError(ctx, account.ID(), "deleted")
				cancel()
				h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_401")
			}
			h.store.RemoveAccount(account.ID())
		} else {
			h.store.MarkCooldown(account, 5*time.Minute, "unauthorized")
		}
	}
}

// compute429Cooldown 根据计划类型和 Codex 响应精确计算 429 冷却时间
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	return compute429Cooldown(account, body, resp)
}

func compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	// 1. 优先使用 Codex 响应体中的精确重置时间
	if resetDuration := parseRetryAfter(body); resetDuration > 2*time.Minute {
		// parseRetryAfter 默认返回 2min（无数据），超过 2min 说明解析到了真实的 resets_at/resets_in_seconds
		if resetDuration > 7*24*time.Hour {
			resetDuration = 7 * 24 * time.Hour // 最多 7 天
		}
		return resetDuration
	}

	// 2. 没有精确重置时间，根据套餐类型 + 用量窗口推断
	planType := strings.ToLower(account.GetPlanType())

	switch planType {
	case "free":
		// Free 只有 7d 窗口，429 = 额度耗尽，冷却 7 天
		return 7 * 24 * time.Hour

	case "team", "teamplus", "pro", "plus", "enterprise":
		// Team/Pro/Plus 有 5h + 7d 双窗口，需要判断是哪个窗口触发了限制
		return detectTeamCooldownWindow(resp)

	default:
		// 未知套餐，保守默认 5 小时
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow 通过响应头判断 Team/Pro/Plus 账号是哪个窗口触发的限制
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
	return detectTeamCooldownWindow(resp)
}

func detectTeamCooldownWindow(resp *http.Response) time.Duration {
	if resp == nil {
		return 5 * time.Hour // 保守默认
	}

	// Codex 返回两组窗口头：primary 和 secondary
	// x-codex-primary-window-minutes / x-codex-primary-used-percent
	// x-codex-secondary-window-minutes / x-codex-secondary-used-percent
	// 用量 >= 100% 的窗口就是触发限制的窗口

	primaryUsed := parseFloat(resp.Header.Get("x-codex-primary-used-percent"))
	primaryWindowMin := parseFloat(resp.Header.Get("x-codex-primary-window-minutes"))
	secondaryUsed := parseFloat(resp.Header.Get("x-codex-secondary-used-percent"))
	secondaryWindowMin := parseFloat(resp.Header.Get("x-codex-secondary-window-minutes"))

	// 找到 used >= 100% 的窗口
	primaryExhausted := primaryUsed >= 100
	secondaryExhausted := secondaryUsed >= 100

	switch {
	case primaryExhausted && secondaryExhausted:
		// 两个窗口都满了，取较大窗口的冷却时间
		return windowMinutesToCooldown(max(primaryWindowMin, secondaryWindowMin))
	case primaryExhausted:
		return windowMinutesToCooldown(primaryWindowMin)
	case secondaryExhausted:
		return windowMinutesToCooldown(secondaryWindowMin)
	default:
		// 都没满但还是 429，可能是短时 burst 限制
		return 5 * time.Hour
	}
}

// windowMinutesToCooldown 根据窗口分钟数决定冷却时长
func windowMinutesToCooldown(windowMinutes float64) time.Duration {
	switch {
	case windowMinutes >= 1440: // >= 1 天 → 7d 窗口
		return 7 * 24 * time.Hour
	case windowMinutes >= 60: // >= 1 小时 → 5h 窗口
		return 5 * time.Hour
	default:
		return 30 * time.Minute // 短窗口
	}
}

// SyncCodexUsageState 解析 Codex 响应头并完成 7d / 5h 快照持久化与 premium 5h 提前限流。
func SyncCodexUsageState(store *auth.Store, account *auth.Account, resp *http.Response) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || resp == nil {
		return result
	}

	result.Used5hHeaders = responseHasCodex5hHeaders(resp)
	result.UsagePct7d, result.HasUsage7d = parseCodexUsageHeaders(resp, account)
	if store != nil {
		if result.HasUsage7d {
			store.PersistUsageSnapshot(account, result.UsagePct7d)
		} else if result.Used5hHeaders {
			store.PersistUsageSnapshot5hOnly(account)
			result.Persisted5hOnly = true
		}
	}

	result.UsagePct5h, result.Reset5hAt, result.HasUsage5h = account.GetUsageSnapshot5h()
	if result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 {
		if store != nil {
			store.MarkPremium5hRateLimited(account, result.Reset5hAt)
		}
		result.Premium5hRateLimited = true
	}

	return result
}

// parseCodexUsageHeaders 从 Codex 响应头解析 5h/7d 用量百分比
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil {
		return 0, false
	}

	// 解析 primary 和 secondary 窗口
	primaryUsedStr := resp.Header.Get("x-codex-primary-used-percent")
	primaryWindowStr := resp.Header.Get("x-codex-primary-window-minutes")
	primaryResetStr := resp.Header.Get("x-codex-primary-reset-after-seconds")
	secondaryUsedStr := resp.Header.Get("x-codex-secondary-used-percent")
	secondaryWindowStr := resp.Header.Get("x-codex-secondary-window-minutes")
	secondaryResetStr := resp.Header.Get("x-codex-secondary-reset-after-seconds")

	primary := parseCodexWindowUsage(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseCodexWindowUsage(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	// 归一化：小窗口 (≤360min) → 5h，大窗口 (>360min) → 7d
	var w5h, w7d codexWindowUsage
	now := time.Now()

	if primary.valid && secondary.valid {
		if primary.windowMin >= secondary.windowMin {
			w7d, w5h = primary, secondary
		} else {
			w7d, w5h = secondary, primary
		}
	} else if primary.valid {
		if primary.windowMin <= 360 && primary.windowMin > 0 {
			w5h = primary
		} else {
			w7d = primary
		}
	} else if secondary.valid {
		if secondary.windowMin <= 360 && secondary.windowMin > 0 {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}

	// 写入 5h
	if w5h.valid {
		resetAt := now.Add(time.Duration(w5h.resetSec) * time.Second)
		account.SetUsageSnapshot5h(w5h.usedPct, resetAt)
	}

	// 写入 7d
	if w7d.valid {
		resetAt := now.Add(time.Duration(w7d.resetSec) * time.Second)
		account.SetReset7dAt(resetAt)
		account.SetUsagePercent7d(w7d.usedPct)
		return w7d.usedPct, true
	}

	return 0, false
}

// ParseCodexUsageHeaders 从响应头提取并更新账号用量信息
func ParseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	return parseCodexUsageHeaders(resp, account)
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

// sendUpstreamError 发送上游错误响应给客户端
func (h *Handler) sendUpstreamError(c *gin.Context, statusCode int, body []byte) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("上游返回错误 (status %d): %s", statusCode, string(body)),
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", statusCode),
		},
	})
}

// sendFinalUpstreamError 重试用尽后的最终错误响应：识别 usage_limit_reached 改写为 503，其余透传
func (h *Handler) sendFinalUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if statusCode == http.StatusTooManyRequests {
		if details, ok := parseUsageLimitDetails(body); ok {
			if details.resetsInSeconds > 0 {
				c.Header("Retry-After", fmt.Sprintf("%d", details.resetsInSeconds))
			}

			message := "账号池额度已耗尽，请稍后重试"
			if details.message != "" {
				message = fmt.Sprintf("%s：%s", message, details.message)
			}

			errInfo := gin.H{
				"message": message,
				"type":    "server_error",
				"code":    "account_pool_usage_limit_reached",
			}
			if details.planType != "" {
				errInfo["plan_type"] = details.planType
			}
			if details.resetsAt != 0 {
				errInfo["resets_at"] = details.resetsAt
			}
			if details.resetsInSeconds != 0 {
				errInfo["resets_in_seconds"] = details.resetsInSeconds
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errInfo})
			return
		}
	}

	h.sendUpstreamError(c, statusCode, body)
}

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// ListModels 列出可用模型
func (h *Handler) ListModels(c *gin.Context) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	modelIDs := h.supportedModelIDs(ctx)
	models := make([]api.Model, 0, len(modelIDs))
	now := time.Now().Unix()
	for _, id := range modelIDs {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "openai",
		})
	}
	api.SendList(c, "list", models)
}

func (h *Handler) supportedModelIDs(ctx context.Context) []string {
	return SupportedModelIDs(ctx, h.db)
}
