package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== Anthropic 错误格式 ====================

const upstreamErrorBodyReadMaxBytes = 1 << 20

// sendAnthropicError 发送 Anthropic 格式的错误响应
func sendAnthropicError(c *gin.Context, statusCode int, errType, message string) {
	if !claimContinuousRetryTerminal(c, continuousRetryProtocolAnthropic) {
		return
	}
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// writeAnthropicStreamErrorEvent 通过流写入器发送 Anthropic 协议的流内 error 事件。
// 用于正文已下发、无法整段静默重试的上游失败：下游网关/客户端（Claude Code 等）
// 能识别 error 事件并自行重试；伪造 stop_reason=end_turn 的干净收尾会让下游把
// 截断/失败响应当成功，既无从感知也无从重试（issue #435）。
func writeAnthropicStreamErrorEvent(w *streamFlushWriter, errType, message string, details ...gin.H) error {
	errorBody := gin.H{
		"type":    errType,
		"message": message,
	}
	if len(details) > 0 && details[0] != nil {
		errorBody["details"] = details[0]
	}
	payload, err := json.Marshal(gin.H{
		"type":  "error",
		"error": errorBody,
	})
	if err != nil {
		payload = []byte(`{"type":"error","error":{"type":"api_error","message":"failed to encode stream error"}}`)
	}
	if err := w.WriteString(fmt.Sprintf("event: error\ndata: %s\n\n", payload)); err != nil {
		return err
	}
	return w.Flush()
}

// rejectAnthropicMessagesRequest 入口校验拒绝：回 Anthropic 错误 JSON 并打一行控制台日志。
// 这一阶段尚未选号，不产生 usage log；没有这行日志，"请求发不进来"在网关侧完全不可见
// （issue #435：下游客户端请求被静默拒绝却无从排查）。
func rejectAnthropicMessagesRequest(c *gin.Context, statusCode int, errType, message string) {
	log.Printf("/v1/messages 入口拒绝 (status %d, %s): %s (ip=%s, ua=%q)",
		statusCode, errType, message, c.ClientIP(), c.Request.UserAgent())
	sendAnthropicError(c, statusCode, errType, message)
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

// applyMessagesModelMapping 对翻译后的 codexBody 套用全局模型映射与思考强度别名。
// 别名注入会同时写入顶层 reasoning_effort（Chat 形态字段）与 reasoning.effort；
// 本路径的 codexBody 已是 Responses 形态且不再经过 PrepareResponsesBody 净化，
// 顶层字段原样发到上游会触发 400 Unsupported parameter（issue #412），在此剥离。
func (h *Handler) applyMessagesModelMapping(codexBody []byte, supportedModels []string) []byte {
	codexBody, _, _, _ = h.applyConfiguredModelMappingToBody(codexBody, supportedModels)
	codexBody, _ = sjson.DeleteBytes(codexBody, "reasoning_effort")
	return codexBody
}

// resolveMessagesRoutingBody 用廉价 stub 完成模型映射与 effort/tier 提取，
// 避免在选号前把整段 Anthropic messages 转成有损 Codex Responses。
func (h *Handler) resolveMessagesRoutingBody(rawBody []byte, requestedModel string, supportedModels []string) []byte {
	mappingJSON := ""
	if h != nil && h.store != nil {
		mappingJSON = h.store.GetModelMapping()
	}
	mapped := resolveAnthropicModel(requestedModel, mappingJSON, supportedModels)
	stub, err := sjson.SetBytes([]byte(`{}`), "model", mapped)
	if err != nil {
		stub = []byte(`{"model":"` + mapped + `"}`)
	}
	if effort := strings.TrimSpace(gjson.GetBytes(rawBody, "output_config.effort").String()); effort != "" {
		stub, _ = sjson.SetBytes(stub, "reasoning.effort", normalizeReasoningEffortForModel(effort, mapped))
	} else {
		stub, _ = sjson.SetBytes(stub, "reasoning.effort", resolveReasoningEffort(nil, mapped))
	}
	if shouldUseCodexPriorityForAnthropicSpeed(gjson.GetBytes(rawBody, "speed").String()) {
		if upstreamTier, ok := upstreamServiceTier("priority"); ok {
			stub, _ = sjson.SetBytes(stub, "service_tier", upstreamTier)
		}
	}
	return h.applyMessagesModelMapping(stub, supportedModels)
}

type anthropicCodexTranslation struct {
	body []byte
	err  error
	done bool
}

func (h *Handler) translateAnthropicMessagesToCodexOnce(state *anthropicCodexTranslation, rawBody []byte, supportedModels []string) ([]byte, error) {
	if state == nil {
		mappingJSON := ""
		if h != nil && h.store != nil {
			mappingJSON = h.store.GetModelMapping()
		}
		body, _, err := TranslateAnthropicToCodexWithModels(rawBody, mappingJSON, supportedModels)
		if err != nil {
			return nil, err
		}
		return h.applyMessagesModelMapping(body, supportedModels), nil
	}
	if state.done {
		return state.body, state.err
	}
	state.done = true
	mappingJSON := ""
	if h != nil && h.store != nil {
		mappingJSON = h.store.GetModelMapping()
	}
	body, _, err := TranslateAnthropicToCodexWithModels(rawBody, mappingJSON, supportedModels)
	if err != nil {
		state.err = err
		return nil, err
	}
	state.body = h.applyMessagesModelMapping(body, supportedModels)
	return state.body, nil
}

// ==================== /v1/messages Handler ====================

// Messages 处理 /v1/messages 请求（Anthropic Messages API → Codex Responses）
func (h *Handler) Messages(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		rejectAnthropicMessagesRequest(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	h.capturePromptRequestIngress(c, rawBody)

	if len(rawBody) == 0 {
		rejectAnthropicMessagesRequest(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	// 验证 JSON
	if !gjson.ValidBytes(rawBody) {
		rejectAnthropicMessagesRequest(c, http.StatusBadRequest, "invalid_request_error", "Invalid JSON in request body")
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		rejectAnthropicMessagesRequest(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
		return
	}

	// 基本验证
	model := gjson.GetBytes(rawBody, "model").String()
	if model == "" {
		rejectAnthropicMessagesRequest(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !gjson.GetBytes(rawBody, "messages").Exists() {
		rejectAnthropicMessagesRequest(c, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	if h.inspectPromptFilterAnthropic(c, rawBody, "/v1/messages", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)

	// 2. 选号前只解析模型/effort/tier，不把整段 Messages 转成有损 Codex 体。
	// Grok 账号选中后再走一次 TranslateAnthropicToResponsesForGrok；
	// Codex / OpenAI 中转仍按需翻译成 Codex-safe Responses。
	supportedModels := h.supportedModelIDs(c.Request.Context())
	routingBody := h.resolveMessagesRoutingBody(rawBody, model, supportedModels)
	originalModel := model
	effectiveModel := effectiveRequestModel(routingBody, model)
	if isMediaOnlyModel(effectiveModel) {
		sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", fmt.Sprintf("model %s is only supported on %s", effectiveModel, mediaOnlyModelEndpoints(effectiveModel)))
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
	accountFilter = h.applyUpstreamChannelFilter(c, effectiveModel, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// scope 并发位在选中账号后才能占，请求退出时统一释放（issue #439 v2）。
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolAnthropic)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetrySSEKeepalive(c, isStream, "text/event-stream; charset=utf-8")
	defer stopRetryKeepalive()
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		activateContinuousRetryKeepalive(c.Request.Context())
	}

	reasoningEffort := extractReasoningEffort(routingBody)
	serviceTier := extractServiceTier(routingBody)
	ruleIdentity := h.payloadRuleIdentity(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	var codexTranslation anthropicCodexTranslation
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	var wsHTTPFallback websocketHTTPFallbackState

	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	capacityShedRetries := map[int64]int{}
	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			account, stickyProxyURL = h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		}
		if account == nil {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolAnthropic) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				if isStream && writeCommittedAnthropicRetryError(c, "rate_limit_error", "All accounts rate limited") {
					return
				}
				sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", "All accounts rate limited")
				return
			}
			// 候选被 scope 预算剔空时给出真实原因（issue #439）。
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				if isStream && writeCommittedAnthropicRetryError(c, "rate_limit_error", msg) {
					return
				}
				sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", msg)
				return
			}
			if isStream && writeCommittedAnthropicRetryError(c, "overloaded_error", noAvailableAnthropicAccountMessage(effectiveModel)) {
				return
			}
			sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", noAvailableAnthropicAccountMessage(effectiveModel))
			return
		}
		if attempt > 0 {
			clearNewAPIUpstreamCyberPolicyDecision(c)
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback && !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinity(c.Request.Context(), h.store, affinityKey, account, proxyURL) {
				h.store.Release(account)
				return
			}
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("上游 WebSocket 1009 后启动 HTTP 降级尝试 (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/messages, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}
		isRelayAccount := account.IsRelayStyle()
		attemptEffectiveModel := effectiveModel
		useWebsocket := h.shouldUseWebsocketForHTTP() && !wsHTTPFallback.ForceHTTP() && !isRelayAccount
		upstreamEndpoint := "/v1/responses"
		if isRelayAccount {
			upstreamEndpoint = relayUpstreamEndpointForProtocol(account, GrokProtocolMessages, attemptEffectiveModel)
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
		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		// 身份按 attempt 附加实际选中账号维度：account_* 门随重试换号重新匹配（issue #410）。
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		var resp *http.Response
		var reqErr error
		if isRelayAccount {
			upstreamBody := routingBody
			if !account.IsGrokAPI() {
				var translateErr error
				upstreamBody, translateErr = h.translateAnthropicMessagesToCodexOnce(&codexTranslation, rawBody, supportedModels)
				if translateErr != nil {
					ttftGuard.Stop()
					h.store.Release(account)
					if isStream && writeCommittedAnthropicRetryError(c, "invalid_request_error", "Request translation failed") {
						return
					}
					sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request translation failed: "+translateErr.Error())
					return
				}
			}
			if mappedBody, mappedModel, ok := h.applyAccountModelMappingToBody(upstreamBody, account); ok {
				upstreamBody = mappedBody
				attemptEffectiveModel = mappedModel
			}
			resp, reqErr = executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
				return ExecuteRelayStyleProtocolRequest(upstreamCtx, account, GrokProtocolMessages, rawBody, upstreamBody, proxyURL, downstreamHeaders)
			})
		} else {
			codexBody, translateErr := h.translateAnthropicMessagesToCodexOnce(&codexTranslation, rawBody, supportedModels)
			if translateErr != nil {
				ttftGuard.Stop()
				h.store.Release(account)
				if isStream && writeCommittedAnthropicRetryError(c, "invalid_request_error", "Request translation failed") {
					return
				}
				sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request translation failed: "+translateErr.Error())
				return
			}
			// service_tier 记账按 payload 规则改写后的值归因（仅 Codex 路径套用规则）。
			serviceTier = EffectiveRequestedServiceTier(codexBody, attemptEffectiveModel, downstreamHeaders, attemptIdentity)
			resp, reqErr = executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
				return ExecuteRequest(upstreamCtx, account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
			})
		}
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/messages", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("上游 WebSocket 1009，保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/messages, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			}
			// Buffered transport retries stay on the same account without penalizing,
			// unbinding, or excluding it (issue #331). Busy-acquire timeouts rotate
			// because waiting again on the same key would repeat the queue (issue #413).
			// 缓冲传输重试保留同一账号且不处罚、不解绑、不排除（issue #331）；
			// busy acquire 超时会轮换账号，避免同一 key 重复排队（issue #413）。
			stickyRetry := continuousRetryBuffersAttempts(continuousRetryPolicy) &&
				h.shouldStickyTransportRetry(reqErr, kind, timedOut, shouldRetry, continuousRetryPolicy)
			if retryable && shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable && !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %s, account %d, /v1/messages): %v", retryAttemptProgress(attempt, maxRetries), account.ID(), reqErr)
				if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), true, generalRetries, retryLimit) {
					return
				}
				continue
			}
			if retryable && !timedOut && !stickyRetry {
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			}

			if !retryable {
				var structured *Error
				if errors.As(reqErr, &structured) && structured.HTTPStatus == http.StatusBadRequest {
					if isStream && writeCommittedAnthropicRetryError(c, "invalid_request_error", structured.Message) {
						return
					}
					sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", structured.Message)
					return
				}
				if isStream && writeCommittedAnthropicRetryError(c, "api_error", "Upstream request failed") {
					return
				}
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
				return
			}

			log.Printf("上游请求失败 (attempt %d, /v1/messages): %v", attempt+1, reqErr)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return
				}
				if !h.bindBufferedStickyRetryAffinity(c.Request.Context(), affinityKey, account, proxyURL, stickyRetry, continuousRetryPolicy) {
					return
				}
				if stickyRetry {
					log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %s, /v1/messages)", account.ID(), retryAttemptProgress(attempt, maxRetries))
				}
				continue
			}
			if isStream && writeCommittedAnthropicRetryError(c, "api_error", "Upstream request failed") {
				return
			}
			sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/messages", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, readErr := readAllLimited(resp.Body, upstreamErrorBodyReadMaxBytes)
			if readErr != nil {
				errBody = []byte(`{"error":{"message":"Upstream error response exceeded the safe read limit","type":"api_error"}}`)
			}
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if continuousRetryCommitExpired(c, continuousRetryProtocolAnthropic) {
				h.store.Release(account)
				return
			}
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("上游返回错误 (attempt %d, status %d, /v1/messages): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/messages", resp.StatusCode, model, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/messages", model, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, attemptEffectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/messages",
				Model:                  model,
				EffectiveModel:         attemptEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/messages",
				UpstreamEndpoint:       upstreamEndpoint,
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				clearNewAPIUpstreamCyberPolicyDecision(c)
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			if isExplicitUpstreamCyberPolicy(errBody) {
				if isStream && writeCommittedAnthropicRetryError(c, "invalid_request_error", upstreamCyberPolicyResponseMessage(c)) {
					return
				}
				sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", upstreamCyberPolicyResponseMessage(c))
				return
			}

			// 最终错误：用 Anthropic 格式返回。
			// 上游账号 401（OAuth token 失效）是账号侧问题，不是下游客户端凭证无效；
			// 原样以 401 透传会让客户端误判自己的 key 失效（issue #323），改写为 503。
			if resp.StatusCode == http.StatusUnauthorized && !isMissingScopeUnauthorized(errBody) {
				if isStream && writeCommittedAnthropicRetryError(c, "overloaded_error", "账号池暂无可用账号（上游账号鉴权失效），请稍后重试") {
					return
				}
				sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", "账号池暂无可用账号（上游账号鉴权失效），请稍后重试")
				return
			}
			// 上游账号 403 也是账号侧问题（额度/套餐/工作区受限）：换号重试耗尽后仍 403，
			// 原样透传会让 Claude Code 误判自身无权限而停工（issue #396），改写为 503 池级错误。
			if resp.StatusCode == http.StatusForbidden {
				if isStream && writeCommittedAnthropicRetryError(c, "overloaded_error", "账号池暂无可用账号（上游账号被拒绝访问：额度/套餐或工作区受限），请稍后重试") {
					return
				}
				sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", "账号池暂无可用账号（上游账号被拒绝访问：额度/套餐或工作区受限），请稍后重试")
				return
			}
			errType := mapHTTPStatusToAnthropicError(resp.StatusCode)
			msg := usageLogErrorMessage(resp.StatusCode, errBody)
			if msg == "" || msg == fmt.Sprintf("HTTP %d", resp.StatusCode) {
				msg = fmt.Sprintf("Upstream returned status %d", resp.StatusCode)
			}
			if isStream && writeCommittedAnthropicRetryError(c, errType, msg) {
				return
			}
			sendAnthropicError(c, resp.StatusCode, errType, msg)
			return
		}
		if isGrokNativeRouteResponse(resp) {
			downstreamFlusher, _ := c.Writer.(http.Flusher)
			streamAttempt := h.newContinuousRetryStreamAttempt(isStream && continuousRetryBuffersAttempts(continuousRetryPolicy), c.Writer, downstreamFlusher)
			usage, outcome, wroteAnyBody, firstTokenMs := forwardGrokNativeResponseTo(c, resp, GrokProtocolMessages, isStream, start, ttftGuard.Stop, streamAttempt.writerOr(c.Writer), streamAttempt.flusherOr(downstreamFlusher))
			totalDuration := int(time.Since(start).Milliseconds())
			ttftGuard.Stop()
			resp.Body.Close()
			downstreamWrote := streamAttempt.downstreamWrote(wroteAnyBody)
			if shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, downstreamWrote, c.Request.Context().Err(), nil, continuousRetryPolicy) {
				rememberContinuousRetryStreamFailure(c.Request.Context(), outcome, outcome.failurePayload)
				_ = streamAttempt.Close()
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				retryExclusions.MarkStreamFailure(account.ID(), outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				retryOrdinal, retryLimit := retryStateForStreamOutcome(outcome, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			if outcome.logStatusCode == http.StatusOK {
				if !claimContinuousRetrySuccess(c, continuousRetryProtocolAnthropic) {
					_ = streamAttempt.Close()
					h.store.Release(account)
					return
				}
				copyGrokNativeResponseHeaders(c, resp.Header)
				if commitErr := h.commitStreamAttempt(c, streamAttempt); commitErr != nil {
					if isContinuousRetryLocalFailure(commitErr) {
						outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
					} else {
						abortContinuousRetryCommitFailure(h, account, resp, streamAttempt)
						return
					}
				}
			}
			_ = streamAttempt.Close()
			if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
				h.store.BindSessionAffinity(affinityKey, account, proxyURL)
			}
			nativeHTTPError := !downstreamWrote && outcome.logStatusCode != http.StatusOK && c.Request.Context().Err() == nil
			if outcome.terminalLocal && c.Request.Context().Err() == nil {
				writeContinuousRetryLocalAnthropicError(c)
			} else if nativeHTTPError {
				h.sendGrokNativeHTTPError(c, GrokProtocolMessages, outcome)
			}
			logInput := &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/messages", Model: model,
				EffectiveModel: attemptEffectiveModel, StatusCode: outcome.logStatusCode,
				DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/messages", UpstreamEndpoint: upstreamEndpoint,
				Stream: isStream, ViaWebsocket: false, AttemptIndex: attempt + 1,
			}
			if usage != nil {
				logInput.PromptTokens, logInput.CompletionTokens, logInput.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
				logInput.InputTokens, logInput.OutputTokens = usage.InputTokens, usage.OutputTokens
				logInput.ReasoningTokens, logInput.CachedTokens = usage.ReasoningTokens, usage.CachedTokens
			}
			if outcome.logStatusCode != http.StatusOK {
				logInput.UpstreamErrorKind = outcome.failureKind
				logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
			}
			h.logUsageForRequest(c, logInput)
			if outcome.penalize {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			} else if outcome.logStatusCode == http.StatusOK {
				h.store.ClearModelCooldown(account, attemptEffectiveModel)
				h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
			}
			h.store.Release(account)
			return
		}

		// ========== 成功路径 ==========
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", attemptEffectiveModel)
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
		var preContentErrorCandidate []byte
		contentStarted := false
		var anthropicResp *anthropicResponse
		promptPolicyIncidentID := ""
		upstreamCyberPolicyLogged := false
		var streamAttempt *continuousRetryStreamAttempt

		if isStream {
			// 流式响应：逐事件翻译为 Anthropic SSE
			setSSEStreamHeaders(c, "text/event-stream; charset=utf-8")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				sendAnthropicError(c, http.StatusInternalServerError, "api_error", "Streaming not supported")
				resp.Body.Close()
				h.store.Release(account)
				return
			}

			translator := newAnthropicStreamTranslator(originalModel)
			streamAttempt = h.newContinuousRetryStreamAttempt(continuousRetryBuffersAttempts(continuousRetryPolicy), c.Writer, flusher)
			streamWriter := h.newAttemptStreamFlushWriter(c, streamAttempt, c.Writer, flusher)
			var pendingFirstTokenEvents bytes.Buffer
			// contentStarted 用严格口径（isFirstTokenResult）跟踪"首个真实内容帧"，
			// 专供流提交决策（缓冲/重试窗口/failed 抑制）使用；ttftRecorded 按
			// first_token_mode 可能是 loose 口径，只用于首字统计。loose 模式会把
			// output_item.added 等纯结构帧当"首字"，若拿它做流提交门，结构帧一到
			// 就落盘 200，首包前静默重试窗口被过早关闭（issue #435）。
			readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, func(sseEvent string, data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)

				// TTFT 跟踪
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if !contentStarted && isFirstTokenResult(parsed) {
					contentStarted = true
				}
				if contentStarted {
					preContentErrorCandidate = nil
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}

				// 提取 usage
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				visibleBody := wroteAnyBody && !continuousRetryBuffersAttempts(continuousRetryPolicy)
				if eventType == "error" && continuousRetryBuffersAttempts(continuousRetryPolicy) {
					// The attempt is private in continuous mode. Do not translate or
					// write an upstream error event to the real ResponseWriter.
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					return false
				}
				if !contentStarted && !visibleBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
					preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
					return true
				}

				// 首 token 前的 response.failed 不翻译进下游流（issue #412）：
				// 可重试（5xx/429 等）时吞掉事件，交由循环外静默换号重试；
				// 不可重试或重试耗尽时中止转发，循环外按真实错误码返回 JSON。
				// 否则 handleFailed 会把失败翻译成 stop_reason=end_turn 的"正常空结束"，
				// 下游网关会把它当成功计一条 0 token 请求且无从重试。
				if shouldSuppressRetryableResponseFailedBeforeFirstTokenWithBudgets(eventType, terminalFailurePayload, contentStarted, visibleBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
					pendingFirstTokenEvents.Reset()
					return false
				}
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentStarted, visibleBody, writeErr != nil) {
					pendingFirstTokenEvents.Reset()
					return false
				}
				// 正文已下发后收到 response.failed：无法整段重试，且 handleFailed 会把失败
				// 翻译成 stop_reason=end_turn 的干净收尾，下游把截断响应当成功、无从感知
				// 与重试（issue #435）。按 Anthropic 协议改发流内 error 事件后中止转发。
				if (eventType == "response.failed" || eventType == "error") && visibleBody && writeErr == nil {
					failurePayload := terminalUpstreamErrorPayload(data)
					if eventType == "response.failed" && len(terminalFailurePayload) > 0 {
						failurePayload = terminalFailurePayload
					}
					failedOutcome := classifyResponseFailedOutcome(failurePayload)
					terminalFailurePayload = append([]byte(nil), failurePayload...)
					gotTerminal = true
					var policyDetails gin.H
					if isExplicitUpstreamCyberPolicy(failurePayload) {
						promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/messages", model, responseFailedErrorBody(failurePayload), upstreamCyberPolicyAttempt{
							Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: failedOutcome.logStatusCode,
							AccountID: account.ID(), AttemptIndex: attempt + 1,
						}))
						upstreamCyberPolicyLogged = true
						failedOutcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
						if metadata, delegated := newAPIUpstreamCyberPolicyDecision(c); delegated {
							policyDetails = gin.H{"codex2api_policy": newAPIPolicyDecisionDetails(metadata)}
						}
					}
					if err := writeAnthropicStreamErrorEvent(streamWriter, mapHTTPStatusToAnthropicError(failedOutcome.logStatusCode), failedOutcome.failureMessage, policyDetails); err != nil {
						writeErr = err
					}
					return false
				}

				// 翻译并写入
				events := translator.translateEvent(data)
				if len(events) > 0 {
					var payload bytes.Buffer
					for _, evt := range events {
						payload.WriteString(anthropicEventToSSE(evt))
					}
					payloadString := payload.String()
					// 首个真实内容帧之前的所有帧（结构帧 output_item.added /
					// content_part.added 也算）一律先缓冲：一旦写出任何字节，首包前
					// 静默换号重试窗口就永久关闭（issue #435）。内容帧到达（含思考
					// delta，几乎紧跟结构帧，不违反 issue #207 的思考及时性）即整段
					// flush，之后不再缓冲。
					shouldDefer := !contentStarted && !gotTerminal
					if shouldDefer {
						pendingFirstTokenEvents.WriteString(payloadString)
						if pendingFirstTokenEvents.Len() <= 1024*1024 {
							return !isResponsesTerminalEvent(eventType)
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

				return !isResponsesTerminalEvent(eventType)
			})
			// 仅在真的写过 body 时才做收尾 flush：flusher.Flush 会先提交 HTTP 200 header，
			// 零写入时提前 flush 会让循环外按真实错误码返回的 JSON 失效（status 已定型为 200）。
			if writeErr == nil && wroteAnyBody {
				writeErr = streamWriter.Flush()
			}

			// 流结束但未收到终止事件（上游断流）：已写过 body 时无法整段重试，
			// 也不能像旧逻辑那样伪造 stop_reason=end_turn 的干净收尾——下游会把
			// 截断响应当成功，既无从感知也无从重试（issue #435）。按 Anthropic
			// 协议发流内 error 事件，下游网关/客户端可识别并自行重试。
			// 未写过 body 的断流不走这里：循环外静默换号重试或按真实错误码返回 JSON。
			if writeErr == nil && !gotTerminal && wroteAnyBody && c.Request.Context().Err() == nil {
				// 错误类型保持 overloaded_error（Anthropic 协议枚举，官方 SDK 对其自动
				// 重试）；message 里带稳定标识 upstream_stream_break 供下游编程识别
				// (issue #473)。
				if err := writeAnthropicStreamErrorEvent(streamWriter, "overloaded_error", "Upstream stream interrupted before completion (upstream_stream_break)"); err != nil {
					log.Printf("写入流内 error 事件失败 (/v1/messages): %v", err)
				}
			}
		} else {
			// 非流式：缓冲所有事件后构建完整 JSON 响应
			var lastCompletedData []byte
			translator := newAnthropicStreamTranslator(originalModel)
			accumulator := newAnthropicResponseAccumulator(originalModel)

			readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, func(sseEvent string, data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				if eventType == "error" {
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					preContentErrorCandidate = nil
					return false
				}
				accumulator.apply(translator.translateEvent(data))

				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isFirstTokenResultForMode(parsed, currentFirstTokenMode()) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if isResponsesSuccessTerminalEvent(eventType) {
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
					preContentErrorCandidate = nil
					return false
				}
				if isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
					preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
				}
				return true
			})

			if lastCompletedData != nil {
				anthropicResp = accumulator.build(lastCompletedData)
			}
		}

		// 断流检测 + token 估算
			totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
		outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
		terminalFailurePayload, _ = resolvePreContentRetryErrorCandidate(terminalFailurePayload, preContentErrorCandidate, contentStarted, wroteAnyBody, gotTerminal, readErr, c.Request.Context().Err(), writeErr)
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		ttftGuard.Stop()
		if len(terminalFailurePayload) > 0 && !outcome.terminalLocal {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			if !upstreamCyberPolicyLogged {
				promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/messages", model, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: outcome.logStatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
			}
			if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
				outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
			}
			// 流式 response.failed 也要把额度耗尽/限流账号冷却下来，
			// 否则该账号会保持高分继续被调度（与 /v1/responses 路径保持一致）。
			var responseFailedDecision codex429Decision
			if withContinuousRetryDeadlinePending(c.Request.Context(), func() {
				responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, attemptEffectiveModel)
			}) {
				outcome = applyResponseFailedDecisionKind(outcome, terminalFailurePayload, responseFailedDecision)
			} else {
				outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
			}
		}
		outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			wsHTTPFallback.LogHTTPAttemptCompletion("/v1/messages", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
		}
		downstreamWrote := streamAttempt.downstreamWrote(wroteAnyBody)
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, downstreamWrote, c.Request.Context().Err(), writeErr) {
			_ = streamAttempt.Close()
			wsElapsed := time.Since(start)
			resp.Body.Close()
			wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(outcome.failureMessage))
			log.Printf("上游 WebSocket 1009，首包前保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/messages, ws_elapsed_ms=%d): %s",
				wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), outcome.failureMessage)
			continue
		}
		if shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, downstreamWrote, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
			rememberContinuousRetryStreamFailure(c.Request.Context(), outcome, terminalFailurePayload)
			_ = streamAttempt.Close()
			clearNewAPIUpstreamCyberPolicyDecision(c)
			h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/messages", Model: model, EffectiveModel: attemptEffectiveModel,
				StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/messages", UpstreamEndpoint: upstreamEndpoint, Stream: isStream, ViaWebsocket: useWebsocket,
				AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
				ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			}, promptPolicyIncidentID)
			log.Printf("上游流在首包前断开，重试 (attempt %s, account %d, /v1/messages): %s",
				retryAttemptProgress(attempt, maxRetries), account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			SyncCodexUsageState(h.store, account, resp)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.unbindOrRetainAffinityForCapacityShed(retryExclusions, affinityKey, account, proxyURL, outcome, capacityShedRetries, continuousRetryPolicy)
			if !isFirstTokenTimeoutOutcome(outcome) && !outcome.capacityShed &&
				retryLimitForStreamOutcome(outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) == -1 {
				retryExclusions.MarkStreamFailure(account.ID(), outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			}
			retryOrdinal, retryLimit := retryStateForStreamOutcome(outcome, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), isFirstTokenTimeoutOutcome(outcome), retryOrdinal, retryLimit, resp) {
				return
			}
			continue
		}
		if outcome.logStatusCode == http.StatusOK {
			if !claimContinuousRetrySuccess(c, continuousRetryProtocolAnthropic) {
				_ = streamAttempt.Close()
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			if commitErr := h.commitStreamAttempt(c, streamAttempt); commitErr != nil {
				if isContinuousRetryLocalFailure(commitErr) {
					outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
				} else {
					abortContinuousRetryCommitFailure(h, account, resp, streamAttempt)
					return
				}
			}
		}
		_ = streamAttempt.Close()

		if isStream && outcome.terminalLocal && c.Request.Context().Err() == nil {
			writeContinuousRetryLocalAnthropicError(c)
		} else if isStream && !downstreamWrote && writeErr == nil && c.Request.Context().Err() == nil && outcome.logStatusCode != http.StatusOK {
			// 流式：首包前上游失败、未向下游写过任何字节（收尾 flush 有 wroteAnyBody 守卫，
			// 200 header 尚未提交）——按真实错误码返回 Anthropic 错误 JSON，而不是空 200 流，
			// 让下游网关/客户端能感知失败并自行重试（issue #412）。
			statusCode := outcome.logStatusCode
			if statusCode < 400 || statusCode > 599 || statusCode == logStatusUpstreamStreamBreak {
				statusCode = http.StatusBadGateway
			}
			if !writeCommittedAnthropicRetryError(c, mapHTTPStatusToAnthropicError(statusCode), outcome.failureMessage) {
				c.Header("Content-Type", "application/json; charset=utf-8")
				sendAnthropicError(c, statusCode, mapHTTPStatusToAnthropicError(statusCode), outcome.failureMessage)
			}
		}
		if !isStream {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolAnthropic) {
				// The deadline owns the terminal response.
			} else if anthropicResp != nil {
				c.JSON(http.StatusOK, anthropicResp)
			} else {
				sendAnthropicError(c, http.StatusBadGateway, "api_error", "No complete response received from upstream")
			}
		}

		if !continuousRetryBuffersAttempts(continuousRetryPolicy) || continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
			h.store.BindSessionAffinity(affinityKey, account, proxyURL)
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

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:              account.ID(),
			Endpoint:               "/v1/messages",
			Model:                  model,
			EffectiveModel:         attemptEffectiveModel,
			StatusCode:             logStatusCode,
			DurationMs:             totalDuration,
			FirstTokenMs:           firstTokenMs,
			ReasoningEffort:        reasoningEffort,
			InboundEndpoint:        "/v1/messages",
			UpstreamEndpoint:       upstreamEndpoint,
			Stream:                 isStream,
			ViaWebsocket:           useWebsocket,
			ServiceTier:            usageTiers.ServiceTier,
			RequestedServiceTier:   usageTiers.RequestedServiceTier,
			ActualServiceTier:      usageTiers.ActualServiceTier,
			BillingServiceTier:     usageTiers.BillingServiceTier,
			PromptPolicyIncidentID: promptPolicyIncidentID,
			AttemptIndex:           attempt + 1,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogFailureMessage(logStatusCode, outcome.failureMessage)
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
		SyncCodexUsageState(h.store, account, resp)
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, attemptEffectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		return
	}
}
