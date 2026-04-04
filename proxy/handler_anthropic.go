package proxy

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ==================== Anthropic 错误格式 ====================

// sendAnthropicError 发送 Anthropic 格式的错误响应
func sendAnthropicError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// sendAnthropicStreamError 在流式模式中发送错误事件
func sendAnthropicStreamError(c *gin.Context, errType, message string) {
	fmt.Fprintf(c.Writer, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"%s\",\"message\":\"%s\"}}\n\n", errType, message)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// mapHTTPStatusToAnthropicError 将 HTTP 状态码映射为 Anthropic 错误类型
func mapHTTPStatusToAnthropicError(statusCode int) string {
	switch {
	case statusCode == 400:
		return "invalid_request_error"
	case statusCode == 401:
		return "authentication_error"
	case statusCode == 403:
		return "permission_error"
	case statusCode == 404:
		return "not_found_error"
	case statusCode == 429:
		return "rate_limit_error"
	case statusCode == 529:
		return "overloaded_error"
	case statusCode >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// ==================== /v1/messages Handler ====================

// Messages 处理 /v1/messages 请求（Anthropic Messages API → Codex Responses）
func (h *Handler) Messages(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	if len(rawBody) == 0 {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	// 验证 JSON
	if !gjson.ValidBytes(rawBody) {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Invalid JSON in request body")
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		sendAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
		return
	}

	// 基本验证
	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !gjson.GetBytes(rawBody, "messages").Exists() {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()

	// 2. 翻译请求: Anthropic → Codex
	modelMappingJSON := h.store.GetModelMapping()
	codexBody, originalModel, err := TranslateAnthropicToCodex(rawBody, modelMappingJSON)
	if err != nil {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request translation failed: "+err.Error())
		return
	}

	// 提取 reasoning effort（从翻译后的 codex body 中）
	reasoningEffort := extractReasoningEffort(codexBody)
	sessionID := ResolveSessionID(c.Request.Header, codexBody)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		account := h.store.NextExcluding(excludeAccounts)
		if account == nil {
			account = h.store.WaitForAvailable(c.Request.Context(), 30*time.Second)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", "All accounts rate limited")
					return
				}
				sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", "No available accounts, please retry later")
				return
			}
		}

		start := time.Now()
		proxyURL := h.store.NextProxy()
		useWebsocket := h.cfg != nil && h.cfg.UseWebsocket

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		// 兼容 Anthropic 客户端多种认证方式
		if apiKey == "" {
			for _, hdr := range []string{"x-api-key", "anthropic-auth-token"} {
				if v := strings.TrimSpace(c.GetHeader(hdr)); v != "" {
					apiKey = v
					break
				}
			}
		}

		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}

		downstreamHeaders := c.Request.Header.Clone()
		resp, reqErr := ExecuteRequest(c.Request.Context(), account, codexBody, sessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			excludeAccounts[account.ID()] = true

			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
				return
			}

			log.Printf("上游请求失败 (attempt %d, /v1/messages): %v", attempt+1, reqErr)
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
			excludeAccounts[account.ID()] = true

			log.Printf("上游返回错误 (attempt %d, status %d, /v1/messages): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/messages", resp.StatusCode, model, account.ID(), errBody)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         "/v1/messages",
				Model:            model,
				StatusCode:       resp.StatusCode,
				DurationMs:       durationMs,
				ReasoningEffort:  reasoningEffort,
				InboundEndpoint:  "/v1/messages",
				UpstreamEndpoint: "/v1/responses",
				Stream:           isStream,
			})
			h.applyCooldown(account, resp.StatusCode, errBody, resp)

			if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			// 最终错误：用 Anthropic 格式返回
			errType := mapHTTPStatusToAnthropicError(resp.StatusCode)
			msg := gjson.GetBytes(errBody, "error.message").String()
			if msg == "" {
				msg = fmt.Sprintf("Upstream returned status %d", resp.StatusCode)
			}
			sendAnthropicError(c, resp.StatusCode, errType, msg)
			return
		}

		// ========== 成功路径 ==========
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", model)
		c.Set("x-reasoning-effort", reasoningEffort)

		var firstTokenMs int
		var usage *UsageInfo
		ttftRecorded := false
		gotTerminal := false
		deltaCharCount := 0
		var readErr error
		var writeErr error
		wroteAnyBody := false

		if isStream {
			// 流式响应：逐事件翻译为 Anthropic SSE
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				sendAnthropicError(c, http.StatusInternalServerError, "api_error", "Streaming not supported")
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			translator := newAnthropicStreamTranslator(originalModel)

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// TTFT 跟踪
				if !ttftRecorded && (eventType == "response.output_text.delta" ||
					eventType == "response.reasoning_summary_text.delta" ||
					eventType == "response.reasoning_text.delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}

				// 提取 usage
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					gotTerminal = true
				}
				if eventType == "response.failed" {
					gotTerminal = true
				}

				// 翻译并写入
				events := translator.translateEvent(data)
				for _, evt := range events {
					sse := anthropicEventToSSE(evt)
					if _, err := fmt.Fprint(c.Writer, sse); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
				}
				if len(events) > 0 {
					flusher.Flush()
				}

				return eventType != "response.completed" && eventType != "response.failed"
			})

			// 流结束后补齐事件
			if writeErr == nil {
				finalEvents := translator.finalize()
				// 仅在 message_stop 未发送过时输出
				if !gotTerminal {
					for _, evt := range finalEvents {
						sse := anthropicEventToSSE(evt)
						fmt.Fprint(c.Writer, sse)
					}
					flusher.Flush()
				}
			}
		} else {
			// 非流式：缓冲所有事件后构建完整 JSON 响应
			var lastCompletedData []byte

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				if !ttftRecorded && strings.Contains(eventType, ".delta") {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if eventType == "response.output_text.delta" || eventType == "response.function_call_arguments.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					lastCompletedData = data
					gotTerminal = true
					return false
				}
				if eventType == "response.failed" {
					gotTerminal = true
					return false
				}
				return true
			})

			if lastCompletedData != nil {
				anthropicResp := buildAnthropicResponseFromCompleted(lastCompletedData, originalModel)
				c.JSON(http.StatusOK, anthropicResp)
			} else {
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "No complete response received from upstream")
			}
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重试 (attempt %d/%d, account %d, /v1/messages): %s",
				attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
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
			log.Printf("流异常结束 (account %d, /v1/messages, status %d): %s，已转发约 %d 字符",
				account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
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

		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/messages",
			Model:            model,
			StatusCode:       logStatusCode,
			DurationMs:       totalDuration,
			FirstTokenMs:     firstTokenMs,
			ReasoningEffort:  reasoningEffort,
			InboundEndpoint:  "/v1/messages",
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
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)

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
		sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed: "+lastErr.Error())
	} else if lastStatusCode != 0 {
		errType := mapHTTPStatusToAnthropicError(lastStatusCode)
		sendAnthropicError(c, lastStatusCode, errType, fmt.Sprintf("Upstream returned status %d", lastStatusCode))
	}
}
