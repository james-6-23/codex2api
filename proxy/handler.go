package proxy

import (
	"context"
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

	// 动态 key 缓存
	dbKeysMu    sync.RWMutex
	dbKeys      map[string]bool
	dbKeysUntil time.Time
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, apiKeys []string, db *database.DB) *Handler {
	keyMap := make(map[string]bool)
	for _, k := range apiKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			keyMap[k] = true
		}
	}
	return &Handler{store: store, configKeys: keyMap, db: db}
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

const maxRetries = 2 // 最多重试次数（换号）

// isRetryableStatus 检查是否可重试的上游状态码
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable || code == http.StatusUnauthorized
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

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	sessionID := ResolveSessionID(c.GetHeader("Authorization"), rawBody)
	reasoningEffort := extractReasoningEffort(rawBody)

	// 2. 注入/修正 Codex 必需字段
	codexBody := rawBody
	codexBody, _ = sjson.SetBytes(codexBody, "stream", true)
	codexBody, _ = sjson.SetBytes(codexBody, "store", false)
	if !gjson.GetBytes(codexBody, "include").Exists() {
		codexBody, _ = sjson.SetBytes(codexBody, "include", []string{"reasoning.encrypted_content"})
	}

	// 3. 带重试的上游请求
	var lastErr error
	var lastStatusCode int
	var lastBody []byte

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account := h.store.Next()
		if account == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"message": "无可用账号", "type": "server_error"},
			})
			return
		}

		start := time.Now()
		resp, reqErr := ExecuteRequest(account, codexBody, sessionID)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			h.store.Release(account)
			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
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
			})
			h.applyCooldown(account, resp.StatusCode, errBody)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！透传响应并跟踪 TTFT / usage
		var firstTokenMs int
		var usage *UsageInfo
		ttftRecorded := false

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

			_ = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()

				// TTFT: 记录第一个 output_text.delta 事件的时间
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 提取 usage
				if eventType == "response.completed" {
					usage = extractUsage(data)
				}

				fmt.Fprintf(c.Writer, "data: %s\n\n", data)
				flusher.Flush()
				return eventType != "response.completed" && eventType != "response.failed"
			})
		} else {
			// 非流式收集
			var lastResponseData []byte
			_ = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					c.Data(http.StatusOK, "application/json", []byte(responseObj.Raw))
				} else {
					c.JSON(http.StatusBadGateway, gin.H{
						"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
					})
				}
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		// 记录完整 usage
		totalDuration := int(time.Since(start).Milliseconds())
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/responses",
			Model:            model,
			StatusCode:       200,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/responses",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
		}
		h.logUsage(logInput)

		resp.Body.Close()
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendUpstreamError(c, lastStatusCode, lastBody)
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
		model = "gpt-4.1"
	}
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)

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
	var lastErr error
	var lastStatusCode int
	var lastBody []byte

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account := h.store.Next()
		if account == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"message": "无可用账号", "type": "server_error"},
			})
			return
		}

		start := time.Now()
		resp, reqErr := ExecuteRequest(account, codexBody, sessionID)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			h.store.Release(account)
			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			lastErr = reqErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
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
			})
			h.applyCooldown(account, resp.StatusCode, errBody)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功！翻译响应 + TTFT 跟踪
		var firstTokenMs int
		var usage *UsageInfo
		ttftRecorded := false

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		created := time.Now().Unix()

		if isStream {
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

			_ = ReadSSEStream(resp.Body, func(data []byte) bool {
				chunk, done := TranslateStreamChunk(data, model, chunkID)

				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if eventType == "response.completed" {
					usage = extractUsage(data)
				}

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
		} else {
			var fullContent strings.Builder

			_ = ReadSSEStream(resp.Body, func(data []byte) bool {
				eventType := gjson.GetBytes(data, "type").String()
				if !ttftRecorded && eventType == "response.output_text.delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				switch eventType {
				case "response.output_text.delta":
					fullContent.WriteString(gjson.GetBytes(data, "delta").String())
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

		// 记录完整 usage
		totalDuration := int(time.Since(start).Milliseconds())
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/chat/completions",
			Model:            model,
			StatusCode:       200,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/responses",
			Stream:           isStream,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
		}
		h.logUsage(logInput)

		resp.Body.Close()
		h.store.Release(account)
		return
	}

	// 所有重试都失败
	if lastErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": "上游请求失败: " + lastErr.Error(), "type": "upstream_error"},
		})
	} else if lastStatusCode != 0 {
		h.sendUpstreamError(c, lastStatusCode, lastBody)
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
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte) {
	switch statusCode {
	case http.StatusTooManyRequests:
		cooldown := parseRetryAfter(body)
		if cooldown < 30*time.Second {
			cooldown = 30 * time.Second
		}
		if cooldown > 15*time.Minute {
			cooldown = 15 * time.Minute
		}
		log.Printf("账号 %d 被限速，冷却 %v", account.ID(), cooldown)
		account.SetCooldown(cooldown)
	case http.StatusUnauthorized:
		account.SetCooldown(5 * time.Minute)
	}
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

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body)
	h.sendUpstreamError(c, statusCode, body)
}

// ListModels 列出可用模型
func (h *Handler) ListModels(c *gin.Context) {
	models := []gin.H{
		{"id": "gpt-4.1", "object": "model", "owned_by": "openai"},
		{"id": "gpt-4.1-mini", "object": "model", "owned_by": "openai"},
		{"id": "gpt-4.1-nano", "object": "model", "owned_by": "openai"},
		{"id": "o4-mini", "object": "model", "owned_by": "openai"},
		{"id": "o3", "object": "model", "owned_by": "openai"},
		{"id": "o3-mini", "object": "model", "owned_by": "openai"},
		{"id": "codex-mini-latest", "object": "model", "owned_by": "openai"},
		{"id": "claude-sonnet-4-20250514", "object": "model", "owned_by": "anthropic"},
		{"id": "gemini-2.5-pro", "object": "model", "owned_by": "google"},
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}
