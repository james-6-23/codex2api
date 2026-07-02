package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
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
	payload, err := json.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
	if err != nil {
		payload = []byte(`{"type":"error","error":{"type":"api_error","message":"failed to encode stream error"}}`)
	}
	fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
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
	rawBody, err := readRawRequestBody(c)
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
	if h.inspectPromptFilterAnthropic(c, rawBody, "/v1/messages", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()

	// 2. 翻译请求: Anthropic → Codex
	modelMappingJSON := h.store.GetModelMapping()
	codexBody, originalModel, err := TranslateAnthropicToCodexWithModels(rawBody, modelMappingJSON, h.supportedModelIDs(c.Request.Context()))
	if err != nil {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request translation failed: "+err.Error())
		return
	}
	codexBody, _, _, _ = h.applyConfiguredModelMappingToBody(codexBody, h.supportedModelIDs(c.Request.Context()))
	effectiveModel := effectiveRequestModel(codexBody, model)
	if isImageOnlyModel(effectiveModel) {
		sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", fmt.Sprintf("model %s is only supported on /v1/images/generations and /v1/images/edits", effectiveModel))
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	// /v1/messages 同时允许官方 Codex OAuth 账号与中转（OpenAI Responses API）账号：
	// 翻译后的请求体本身就是 Responses 形态，中转账号直接以 HTTP 转发，
	// 使仅接入中转的用户也能使用 Claude Code（issue #181）。
	accountFilter := accountFilterForResponsesModel(effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

	// 提取 reasoning effort（从翻译后的 codex body 中）
	reasoningEffort := extractReasoningEffort(codexBody)
	serviceTier := extractServiceTier(codexBody)
	sessionID := ResolveSessionID(c.Request.Header, codexBody)
	explicitSessionID := ResolveExplicitSessionID(c.Request.Header, codexBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(explicitSessionID, apiKeyID)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	forceHTTPAfterWSMessageTooBig := false

	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", "All accounts rate limited")
				return
			}
			sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", noAvailableAnthropicAccountMessage(effectiveModel))
			return
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		isRelayAccount := account.IsOpenAIResponsesAPI()
		useWebsocket := shouldUseSessionScopedWebsocket(h.shouldUseWebsocketForHTTP() && !forceHTTPAfterWSMessageTooBig && !isRelayAccount, explicitSessionID)
		upstreamEndpoint := "/v1/responses"
		if isRelayAccount {
			relayBaseURL, _ := account.OpenAIResponsesCredentials()
			upstreamEndpoint = auth.OpenAIResponsesEndpoint(relayBaseURL, "/v1/responses")
		}

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
		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionID, explicitSessionID, useWebsocket)
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		var resp *http.Response
		var reqErr error
		if isRelayAccount {
			resp, reqErr = ExecuteOpenAIResponsesRequest(upstreamCtx, account, codexBody, proxyURL, downstreamHeaders)
		} else {
			resp, reqErr = ExecuteRequest(upstreamCtx, account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		}
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				log.Printf("上游 WebSocket 请求帧过大，自动降级 HTTP 重试 (attempt %d, account %d, /v1/messages): %v", attempt+1, account.ID(), reqErr)
				forceHTTPAfterWSMessageTooBig = true
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			if kind != "" && !(timedOut && shouldRetry) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %d/%d, account %d, /v1/messages): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut {
				retryExclusions.MarkHard(account.ID())
			}

			if !retryable {
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
				return
			}

			log.Printf("上游请求失败 (attempt %d, /v1/messages): %v", attempt+1, reqErr)
			if shouldRetry {
				continue
			}
			sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("上游返回错误 (attempt %d, status %d, /v1/messages): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/messages", resp.StatusCode, model, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/messages", model, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/messages",
				Model:                model,
				EffectiveModel:       effectiveModel,
				StatusCode:           resp.StatusCode,
				DurationMs:           durationMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/messages",
				UpstreamEndpoint:     upstreamEndpoint,
				Stream:               isStream,
				ViaWebsocket:         useWebsocket,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
			})

			if shouldRetry {
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
		c.Set("x-model", effectiveModel)
		c.Set("x-reasoning-effort", reasoningEffort)

		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false
		deltaCharCount := 0
		var readErr error
		var writeErr error
		wroteAnyBody := false
		var terminalFailurePayload []byte
		var anthropicResp *anthropicResponse

		if isStream {
			// 流式响应：逐事件翻译为 Anthropic SSE
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				sendAnthropicError(c, http.StatusInternalServerError, "api_error", "Streaming not supported")
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			translator := newAnthropicStreamTranslator(originalModel)
			streamWriter := newStreamFlushWriter(c.Writer, flusher)
			var pendingFirstTokenEvents bytes.Buffer

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// TTFT 跟踪
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}

				// 提取 usage
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
				}

				// 翻译并写入
				events := translator.translateEvent(data)
				if len(events) > 0 {
					var payload bytes.Buffer
					for _, evt := range events {
						payload.WriteString(anthropicEventToSSE(evt))
					}
					payloadString := payload.String()
					shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
					if shouldDefer {
						pendingFirstTokenEvents.WriteString(payloadString)
						if pendingFirstTokenEvents.Len() <= 1024*1024 {
							return eventType != "response.completed" && eventType != "response.failed"
						}
						payloadString = pendingFirstTokenEvents.String()
						pendingFirstTokenEvents.Reset()
					} else if pendingFirstTokenEvents.Len() > 0 {
						payloadString = pendingFirstTokenEvents.String() + payloadString
						pendingFirstTokenEvents.Reset()
					}
					if err := streamWriter.WriteString(payloadString); err != nil {
						writeErr = err
						return false
					}
					wroteAnyBody = true
				}

				return eventType != "response.completed" && eventType != "response.failed"
			})
			if writeErr == nil {
				writeErr = streamWriter.Flush()
			}

			// 流结束后补齐事件
			if writeErr == nil && !gotTerminal && ttftRecorded {
				finalEvents := translator.finalize()
				for _, evt := range finalEvents {
					sse := anthropicEventToSSE(evt)
					if err := streamWriter.WriteString(sse); err != nil {
						writeErr = err
						break
					}
				}
				if writeErr == nil {
					writeErr = streamWriter.Flush()
				}
			}
		} else {
			// 非流式：缓冲所有事件后构建完整 JSON 响应
			var lastCompletedData []byte
			translator := newAnthropicStreamTranslator(originalModel)
			accumulator := newAnthropicResponseAccumulator(originalModel)

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				accumulator.apply(translator.translateEvent(data))

				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isFirstTokenResultForMode(parsed, currentFirstTokenMode()) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					lastCompletedData = data
					gotTerminal = true
					return false
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					return false
				}
				return true
			})

			if lastCompletedData != nil {
				anthropicResp = accumulator.build(lastCompletedData)
			}
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		ttftGuard.Stop()
		if len(terminalFailurePayload) > 0 {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			// 流式 response.failed 也要把额度耗尽/限流账号冷却下来，
			// 否则该账号会保持高分继续被调度（与 /v1/responses 路径保持一致）。
			responseFailedDecision := h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
			if responseFailedDecision.Reason != "" {
				outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
			}
		}
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游 WebSocket 消息过大，首包前自动降级 HTTP 重试 (attempt %d, account %d, /v1/messages): %s",
				attempt+1, account.ID(), outcome.failureMessage)
			forceHTTPAfterWSMessageTooBig = true
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重试 (attempt %d/%d, account %d, /v1/messages): %s",
				attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}

		if !isStream {
			if anthropicResp != nil {
				c.JSON(http.StatusOK, anthropicResp)
			} else {
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "No complete response received from upstream")
			}
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

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

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:            account.ID(),
			Endpoint:             "/v1/messages",
			Model:                model,
			EffectiveModel:       effectiveModel,
			StatusCode:           logStatusCode,
			DurationMs:           totalDuration,
			FirstTokenMs:         firstTokenMs,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/messages",
			UpstreamEndpoint:     upstreamEndpoint,
			Stream:               isStream,
			ViaWebsocket:         useWebsocket,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogErrorMessage(logStatusCode, []byte(outcome.failureMessage))
			logInput.UpstreamErrorKind = outcome.failureKind
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
			recyclePooledClient(account, proxyURL)
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, effectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}
}
