package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
	deviceCfg  *DeviceProfileConfig // 设备指纹配置

	// 动态 key 缓存
	dbKeysMu    sync.RWMutex
	dbKeys      map[string]bool
	dbKeysUntil time.Time
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return &Handler{
		store:      store,
		configKeys: make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:         db,
		deviceCfg:  deviceCfg,
	}
}

// NewHandlerWithDeviceProfile 创建处理器（带设备指纹配置）
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, deviceCfg)
}

// refreshDBKeys 从数据库刷新密钥缓存（5 分钟）
func (h *Handler) refreshDBKeys() map[string]bool {
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
	vals, err := h.db.GetAllAPIKeyValues(ctx)
	if err != nil {
		log.Printf("刷新 API Keys 缓存失败: %v", err)
		return h.dbKeys
	}

	newMap := make(map[string]bool, len(vals))
	for _, v := range vals {
		newMap[v] = true
	}
	h.dbKeys = newMap
	h.dbKeysUntil = time.Now().Add(5 * time.Minute)
	return newMap
}

// isValidKey 检查 key 是否有效（配置文件 + DB）
func (h *Handler) isValidKey(key string) bool {
	if h.configKeys[key] {
		return true
	}
	dbKeys := h.refreshDBKeys()
	return dbKeys[key]
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

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	v1 := r.Group("/v1")
	v1.Use(h.authMiddleware())
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.GET("/models", h.ListModels)
}

// authMiddleware API Key 鉴权中间件
func (h *Handler) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果没有配置任何密钥，跳过鉴权
		if !h.hasAnyKeys() {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "缺少 Authorization 头",
					"type":    "authentication_error",
					"code":    "missing_api_key",
				},
			})
			c.Abort()
			return
		}

		key := strings.TrimPrefix(authHeader, "Bearer ")
		if !h.isValidKey(key) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "无效的 API Key",
					"type":    "authentication_error",
					"code":    "invalid_api_key",
				},
			})
			c.Abort()
			return
		}
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

// isCodexModelCapacityError 检测 Codex 模型容量不足错误
// 这类错误应该作为 429 处理，触发重试和冷却
func isCodexModelCapacityError(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}

	// 尝试从 JSON 中提取错误消息
	candidates := []string{
		gjson.GetBytes(errorBody, "error.message").String(),
		gjson.GetBytes(errorBody, "message").String(),
	}

	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if strings.Contains(lower, "selected model is at capacity") ||
			strings.Contains(lower, "model is at capacity. please try a different model") ||
			strings.Contains(lower, "model is currently at capacity") {
			return true
		}
	}

	return false
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

// Responses 处理 /v1/responses 请求（原生透传，无需协议翻译）
func (h *Handler) Responses(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "读取请求体失败", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model is required", "type": "invalid_request_error"},
		})
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	sessionID := ResolveSessionID(c.GetHeader("Authorization"), rawBody)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", serviceTier)
	}

	// 2. 注入/修正 Codex 必需字段
	codexBody := rawBody
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.SetBytes(codexBody, "store", false)
	if !gjson.GetBytes(codexBody, "include").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "include", []string{"reasoning.encrypted_content"})
	}

	// 自动将字符串 input 包装为数组格式（Codex 要求 input 为 list）
	inputResult := gjson.GetBytes(codexBody, "input")
	if inputResult.Exists() && inputResult.Type == gjson.String {
		codexBody, _ = sjson.SetBytes(codexBody, "input", []map[string]string{
			{"role": "user", "content": inputResult.String()},
		})
	}

	// 将 Chat Completions 风格的 reasoning_effort 自动转换为 Responses API 的 reasoning.effort
	if re := gjson.GetBytes(codexBody, "reasoning_effort"); re.Exists() && !gjson.GetBytes(codexBody, "reasoning.effort").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "reasoning.effort", re.String())
	}
	codexBody = clampReasoningEffort(codexBody)
	codexBody = sanitizeServiceTierForUpstream(codexBody)

	// 为缺少 description 的客户端执行工具补充默认描述（如 tool_search）
	codexBody = ensureToolDescriptions(codexBody)
	// 清理 function tool parameters 中上游不支持的 JSON Schema 关键字
	codexBody = sanitizeToolSchemas(codexBody)

	// 展开 previous_response_id（将缓存的历史对话上下文注入 input）
	codexBody, _ = expandPreviousResponse(codexBody)
	// 保存展开后的 input，用于在 response.completed 时缓存完整上下文
	expandedInputRaw := gjson.GetBytes(codexBody, "input").Raw

	// 删除 Codex 不支持的参数
	unsupportedFields := []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
	}
	for _, field := range unsupportedFields {
		codexBody, _ = sjson.DeleteBytes(codexBody, field)
	}

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account := h.store.Next()
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account = h.store.WaitForAvailable(c.Request.Context(), 30*time.Second)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := h.store.NextProxy()

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

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, model, account.ID(), errBody)
			h.logUsage(&database.UsageLogInput{
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

			// 判断是否应该重试：可重试状态码 或 模型容量不足错误
			capErr := isCodexModelCapacityError(errBody)
			shouldRetry := (isRetryableStatus(resp.StatusCode) || capErr) && attempt < maxRetries
			if shouldRetry {
				// Capacity error 视为 429 处理，设置短期冷却
				if capErr && resp.StatusCode != http.StatusTooManyRequests {
					h.store.MarkCooldown(account, 30*time.Second, "model_capacity")
					log.Printf("检测到模型容量不足 (account %d)，设置 30s 冷却后重试", account.ID())
				}
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
				eventType := gjson.GetBytes(data, "type").String()

				// TTFT: 记录第一个 output_text.delta 事件的时间
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				}

				// 提取 usage + service_tier
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
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
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
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
				}
			}
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClientForAccount(account)
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}
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
		h.logUsage(logInput)

		resp.Body.Close()
		if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
			h.store.PersistUsageSnapshot(account, usagePct)
		}
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
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

func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "读取请求体失败", "type": "invalid_request_error"},
		})
		return
	}

	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		model = "gpt-5.4"
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "请求翻译失败: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}

	sessionID := ResolveSessionID(c.GetHeader("Authorization"), codexBody)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account := h.store.Next()
		if account == nil {
			// 排队等待可用账号（最多 30s）
			account = h.store.WaitForAvailable(c.Request.Context(), 30*time.Second)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"message": "无可用账号，请稍后重试", "type": "server_error"},
				})
				return
			}
		}

		start := time.Now()
		proxyURL := h.store.NextProxy()

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

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, model, account.ID(), errBody)
			h.logUsage(&database.UsageLogInput{
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

			// 判断是否应该重试：可重试状态码 或 模型容量不足错误
			capErr := isCodexModelCapacityError(errBody)
			shouldRetry := (isRetryableStatus(resp.StatusCode) || capErr) && attempt < maxRetries
			if shouldRetry {
				// Capacity error 视为 429 处理，设置短期冷却
				if capErr && resp.StatusCode != http.StatusTooManyRequests {
					h.store.MarkCooldown(account, 30*time.Second, "model_capacity")
					log.Printf("检测到模型容量不足 (account %d)，设置 30s 冷却后重试", account.ID())
				}
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
			streamTranslator := NewStreamTranslator(chunkID, model)
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

				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数（文本 + function call 参数）
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				if chunk != nil {
					chunk, _ = sjson.SetBytes(chunk, "created", created)
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
				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				switch eventType {
				case "response.output_text.delta":
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
					fullContent.WriteString(gjson.GetBytes(data, "delta").String())
				case "response.function_call_arguments.delta":
					deltaCharCount += len(gjson.GetBytes(data, "delta").String())
				case "response.completed":
					usage = extractUsage(data)
					if tier := gjson.GetBytes(data, "response.service_tier").String(); tier != "" {
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

			result := []byte(`{}`)
			result, _ = sjson.SetBytes(result, "id", chunkID)
			result, _ = sjson.SetBytes(result, "object", "chat.completion")
			result, _ = sjson.SetBytes(result, "created", created)
			result, _ = sjson.SetBytes(result, "model", model)
			result, _ = sjson.SetBytes(result, "choices.0.index", 0)
			result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")

			if len(toolCalls) > 0 {
				// 有工具调用: 设置 tool_calls 和对应的 finish_reason
				contentStr := fullContent.String()
				if contentStr != "" {
					result, _ = sjson.SetBytes(result, "choices.0.message.content", contentStr)
				} else {
					result, _ = sjson.SetRawBytes(result, "choices.0.message.content", []byte("null"))
				}
				for i, tc := range toolCalls {
					prefix := fmt.Sprintf("choices.0.message.tool_calls.%d", i)
					result, _ = sjson.SetBytes(result, prefix+".id", tc.ID)
					result, _ = sjson.SetBytes(result, prefix+".type", "function")
					result, _ = sjson.SetBytes(result, prefix+".function.name", tc.Name)
					result, _ = sjson.SetBytes(result, prefix+".function.arguments", tc.Arguments)
				}
				result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "tool_calls")
			} else {
				result, _ = sjson.SetBytes(result, "choices.0.message.content", fullContent.String())
				result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")
			}

			if usage != nil {
				result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
				result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
				result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
			}

			compactResult = result
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClientForAccount(account)
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			resp.Body.Close()
			h.store.Release(account)
			lastErr = readErr
			if lastErr == nil {
				lastErr = errors.New(outcome.failureMessage)
			}
			continue
		}
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
		h.logUsage(logInput)

		resp.Body.Close()
		if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
			h.store.PersistUsageSnapshot(account, usagePct)
		}
		if outcome.penalize {
			recyclePooledClientForAccount(account)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
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
		chunk, done := TranslateStreamChunk(data, model, chunkID)
		if chunk != nil {
			chunk, _ = sjson.SetBytes(chunk, "created", created)
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

	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "id", chunkID)
	result, _ = sjson.SetBytes(result, "object", "chat.completion")
	result, _ = sjson.SetBytes(result, "created", created)
	result, _ = sjson.SetBytes(result, "model", model)
	result, _ = sjson.SetBytes(result, "choices.0.index", 0)
	result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")
	result, _ = sjson.SetBytes(result, "choices.0.message.content", fullContent.String())
	result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")

	if usage != nil {
		result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
		result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
		result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
	}

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

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	switch statusCode {
	case http.StatusTooManyRequests:
		cooldown := h.compute429Cooldown(account, body, resp)
		log.Printf("账号 %d 被限速 (plan=%s)，冷却 %v", account.ID(), account.GetPlanType(), cooldown)
		h.store.MarkCooldown(account, cooldown, "rate_limited")
	case http.StatusUnauthorized:
		h.store.MarkCooldown(account, 5*time.Minute, "unauthorized")
	}
}

// compute429Cooldown 根据计划类型和 Codex 响应精确计算 429 冷却时间
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
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

	case "team", "pro", "enterprise":
		// Team/Pro 有 5h + 7d 双窗口，需要判断是哪个窗口触发了限制
		return h.detectTeamCooldownWindow(resp)

	default:
		// 未知套餐，保守默认 5 小时
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow 通过响应头判断 Team/Pro 账号是哪个窗口触发的限制
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
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

	type windowData struct {
		usedPct   float64
		resetSec  float64
		windowMin float64
		valid     bool
	}

	parseWindow := func(usedStr, windowStr, resetStr string) windowData {
		if usedStr == "" {
			return windowData{}
		}
		return windowData{
			usedPct:   parseFloat(usedStr),
			windowMin: parseFloat(windowStr),
			resetSec:  parseFloat(resetStr),
			valid:     true,
		}
	}

	primary := parseWindow(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseWindow(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	// 归一化：小窗口 (≤360min) → 5h，大窗口 (>360min) → 7d
	var w5h, w7d windowData
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

// SupportedModels 支持的模型列表（全局共享）
var SupportedModels = []string{
	"gpt-5.4", "gpt-5.4-mini", "gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
	"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
	"gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex",
}

// ListModels 列出可用模型
func (h *Handler) ListModels(c *gin.Context) {
	models := make([]gin.H, 0, len(SupportedModels))
	for _, id := range SupportedModels {
		models = append(models, gin.H{"id": id, "object": "model", "owned_by": "openai"})
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}
