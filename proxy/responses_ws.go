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
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesWSFirstMessageTimeout = 30 * time.Second
	responsesWSWriteTimeout        = 30 * time.Second
	responsesWSFriendlyUpstreamErr = "上游服务临时繁忙，请稍后重试"
)

var responsesWSUpgrader = websocket.Upgrader{
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var errResponsesWSClientGone = errors.New("responses websocket client disconnected")

type responsesWSRetryableStreamError struct {
	outcome streamOutcome
}

func (e *responsesWSRetryableStreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.outcome.failureMessage
}

type responsesWSCloseError struct {
	code   int
	reason string
	err    error
}

func (e *responsesWSCloseError) Error() string {
	if e == nil {
		return ""
	}
	if e.err != nil {
		return e.err.Error()
	}
	return e.reason
}

func (e *responsesWSCloseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress.
// The client sends response.create JSON frames and receives upstream Responses
// events as JSON text frames.
func (h *Handler) ResponsesWebSocket(c *gin.Context) {
	if !isResponsesWebSocketUpgradeRequest(c.Request) {
		api.SendErrorWithStatus(c, api.NewAPIError(
			api.ErrCodeInvalidRequest,
			"WebSocket upgrade required (Upgrade: websocket)",
			api.ErrorTypeInvalidRequest,
		), http.StatusUpgradeRequired)
		return
	}

	conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Responses WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(int64(security.MaxRequestBodySize))

	for turn := 0; ; turn++ {
		if turn == 0 {
			_ = conn.SetReadDeadline(time.Now().Add(responsesWSFirstMessageTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				return
			}
			if turn == 0 {
				log.Printf("Responses WebSocket first message read failed: %v", err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{})

		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			apiErr := api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported websocket message type", api.ErrorTypeInvalidRequest)
			_ = writeResponsesWSError(conn, apiErr)
			closeResponsesWS(conn, websocket.CloseUnsupportedData, apiErr.Message)
			return
		}

		if err := h.forwardResponsesWebSocketTurn(c, conn, payload); err != nil {
			if errors.Is(err, errResponsesWSClientGone) {
				return
			}
			var closeErr *responsesWSCloseError
			if errors.As(err, &closeErr) {
				closeResponsesWS(conn, closeErr.code, closeErr.reason)
				return
			}
			closeResponsesWS(conn, websocket.CloseInternalServerErr, "upstream websocket proxy failed")
			return
		}
	}
}

func (h *Handler) forwardResponsesWebSocketTurn(c *gin.Context, conn *websocket.Conn, rawPayload []byte) error {
	rawBody, model, apiErr := normalizeResponsesWebSocketClientPayload(rawPayload)
	if apiErr != nil {
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	c.Set("raw_body", rawBody)
	if mappedModel != "" {
		model = mappedModel
	}
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}

	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(model)
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
	if result := validator.ValidateRequest(rules); !result.Valid {
		apiErr = validator.ToAPIError()
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}

	if len(rawBody) > security.MaxRequestBodySize {
		apiErr = api.NewAPIError(api.ErrCodeInvalidRequest, "请求体过大", api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseMessageTooBig, apiErr.Message, apiErr)
	}
	if err := security.ValidateModelName(model); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, "model 参数无效", api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}
	if h.inspectPromptFilterOpenAIForWebSocket(c, conn, rawBody, "/v1/responses", model) {
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, "prompt blocked", nil)
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}

	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	respCacheOwner := responseCacheOwner(apiKeyID)
	// 上下文压缩轮豁免首字超时看门狗（issue #381）：压缩首帧天然慢，超时换号无益。
	bodySignalCompact := requestBodyHasCompactionTrigger(rawBody)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	codexBody, expandedInputRaw := PrepareResponsesWebSocketBody(rawBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if status, msg := h.enforceAPIKeyLimits(c, effectiveModel); status != 0 {
		errType := api.ErrorTypeRateLimit
		errCode := api.ErrCodeRateLimitReached
		closeCode := websocket.CloseTryAgainLater
		if status == http.StatusForbidden {
			errType = api.ErrorTypePermission
			errCode = api.ErrCodeInvalidRequest
			closeCode = websocket.ClosePolicyViolation
		}
		apiErr = api.NewAPIError(errCode, msg, errType)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(closeCode, apiErr.Message, apiErr)
	}
	releaseAPIKeyConcurrency, concurrencyErr, ok := h.acquireAPIKeyConcurrencyForWebSocket(c)
	if !ok {
		_ = writeResponsesWSError(conn, concurrencyErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, concurrencyErr.Message, concurrencyErr)
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := accountFilterForModel(effectiveModel)
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

	wsRetrySettings := CurrentRuntimeSettings()
	hideUpstreamErrors := wsRetrySettings.CodexWSHideErrors
	silentRetryEnabled := wsRetrySettings.CodexWSSilentRetry
	maxRetries := wsRetrySettings.CodexWSSilentRetries
	if !silentRetryEnabled {
		maxRetries = 0
	}
	maxRateLimitRetries := maxRetries
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	var lastRetryableUpstreamErr *api.APIError
	retryExclusions := newRetryAccountExclusions()
	invalidEncryptedContentRetried := false
	var wsHTTPFallback websocketHTTPFallbackState
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			account, stickyProxyURL = h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		}
		if account == nil {
			if lastRetryableUpstreamErr != nil {
				apiErr = responsesWSClientUpstreamAPIError(lastRetryableUpstreamErr, hideUpstreamErrors)
			} else if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				apiErr = responsesWSUpstreamAPIError(lastStatusCode, lastBody)
			} else {
				apiErr = api.NewAPIError(api.ErrCodeServiceUnavailable, noAvailableAccountMessage(effectiveModel), api.ErrorTypeServer)
			}
			_ = writeResponsesWSError(conn, apiErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback {
			h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("Responses WebSocket upstream HTTP fallback attempt started (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}

		apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
		useWebsocket := !wsHTTPFallback.ForceHTTP()
		// 生图请求改走 HTTP 上游（客户端仍是 WS）：WebSocket 上游传输大体积
		// 图片数据会卡死（issue #220）；自然语言生图意图也需保留图片工具（issue #288）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(rawBody) {
			useWebsocket = false
		}
		// WebSocket 上游下剥离自动注入的图片工具，防止模型自主生图卡死。
		upstreamBody := codexBody
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		}
		// 在 useWebsocket 最终确定后再派生上游身份键：与 handler.go 的
		// Responses/ChatCompletions 路径一致——无显式会话默认每请求隔离上游身份，
		// WS 路径交给 ExecuteRequest 的 stateless 槽位池处理。
		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		resp, reqErr := ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("Responses WebSocket upstream close 1009; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if silentRetryEnabled && retryable && attempt < maxRetries {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			// 传输类失败粘滞同号重试:不记账号失败、不解绑亲和、不硬排除(issue #331)
			stickyRetry := shouldRetry && !timedOut && kind != "" && h.stickyTransportRetryEnabled()
			if kind != "" && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("Responses WebSocket upstream first token timeout, retrying with another account (attempt %d/%d, account %d): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut && !stickyRetry {
				retryExclusions.MarkHard(account.ID())
			}

			if !retryable {
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				_ = writeResponsesWSError(conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseInternalServerErr, clientErr.Message, reqErr)
			}
			log.Printf("Responses WebSocket upstream request failed (attempt %d): %v", attempt+1, reqErr)
			lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			if shouldRetry {
				if stickyRetry {
					log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %d/%d, ws)", account.ID(), attempt+1, maxRetries+1)
				}
				if !h.waitBeforeRetry(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				continue
			}
			apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			_ = writeResponsesWSError(conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, reqErr)
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
					}
					if codexChanged {
						codexBody = strippedCodexBody
						expandedInputRaw = responsesInputRaw(codexBody)
					}
					log.Printf("Responses WebSocket upstream rejected encrypted_content, stripped encrypted reasoning context and retried once (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("Responses WebSocket upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := false
			if silentRetryEnabled && attempt < maxRetries {
				shouldRetry = shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			}
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           resp.StatusCode,
				DurationMs:           durationMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses",
				UpstreamEndpoint:     "/v1/responses",
				Stream:               true,
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
				lastRetryableUpstreamErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
				if !h.waitBeforeRetry(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				continue
			}

			apiErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			_ = writeResponsesWSError(conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
		}

		var fallbackLog *websocketHTTPFallbackState
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			fallbackLog = &wsHTTPFallback
		}
		if err := h.streamResponsesWSUpstream(c, conn, resp, account, proxyURL, affinityKey, logModel, effectiveModel, logEffectiveModel, reasoningEffort, serviceTier, respCacheOwner, expandedInputRaw, start, ttftGuard, silentRetryEnabled, hideUpstreamErrors, useWebsocket, fallbackLog, attempt+1); err != nil {
			var retryErr *responsesWSRetryableStreamError
			if errors.As(err, &retryErr) {
				lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				if useWebsocket && isWebsocketMessageTooBigOutcome(retryErr.outcome) {
					wsElapsed := time.Since(start)
					wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(retryErr.outcome.failureMessage))
					log.Printf("Responses WebSocket upstream close 1009 before first event; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), retryErr.outcome.failureMessage)
					continue
				}
				if silentRetryEnabled && attempt < maxRetries {
					if isFirstTokenTimeoutOutcome(retryErr.outcome) {
						retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
					} else {
						retryExclusions.MarkHard(account.ID())
					}
					log.Printf("Responses WebSocket upstream stream ended before first token, retrying (attempt %d/%d, account %d): %s", attempt+1, maxRetries+1, account.ID(), retryErr.outcome.failureMessage)
					// 首字超时已白等一轮,不再叠加重试间隔;其余首包前断流按配置间隔等待
					if !isFirstTokenTimeoutOutcome(retryErr.outcome) && !h.waitBeforeRetry(c.Request.Context()) {
						return errResponsesWSClientGone
					}
					continue
				}
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				_ = writeResponsesWSError(conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
			}
			if errors.Is(err, errResponsesWSClientGone) {
				return err
			}
			if shouldRetryErr, ok := err.(*responsesWSCloseError); ok && shouldRetryErr.code == websocket.CloseTryAgainLater {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			return err
		}
		return nil
	}
}

func (h *Handler) streamResponsesWSUpstream(
	c *gin.Context,
	conn *websocket.Conn,
	resp *http.Response,
	account *auth.Account,
	proxyURL string,
	affinityKey string,
	model string,
	effectiveModel string,
	logEffectiveModel string,
	reasoningEffort string,
	serviceTier string,
	respCacheOwner string,
	expandedInputRaw string,
	start time.Time,
	ttftGuard *firstTokenTimeoutGuard,
	silentRetryEnabled bool,
	hideUpstreamErrors bool,
	viaWebsocket bool,
	fallbackLog *websocketHTTPFallbackState,
	fallbackAttempt int,
) error {
	SyncCodexUsageState(h.store, account, resp)

	account.Mu().RLock()
	c.Set("x-account-email", account.Email)
	account.Mu().RUnlock()
	c.Set("x-account-proxy", proxyURL)
	c.Set("x-model", model)
	c.Set("x-reasoning-effort", reasoningEffort)

	var firstTokenMs int
	outputBuffer := newWSPromptOutputBuffer(h.store.GetPromptFilterConfig())
	var usage *UsageInfo
	var actualServiceTier string
	ttftRecorded := false
	gotTerminal := false
	deltaCharCount := 0
	var readErr error
	var writeErr error
	clientGone := false
	var imageLogInfo imageUsageLogInfo
	var terminalFailurePayload []byte
	wroteAnyBody := false
	// 首 token 前收到不可重试的 response.failed 时置位:不把原始失败帧透传给客户端,
	// 循环外改写 error 帧并按错误类别用非正常 close code 关闭,
	// 让下游中转/计费方明确感知失败,而不是把它当成一次正常结束的会话。
	abortedForErrorClose := false
	pendingFirstTokenMessages := make([][]byte, 0, 4)
	pendingFirstTokenBytes := 0

	flushPendingFirstTokenMessages := func() bool {
		for _, pending := range pendingFirstTokenMessages {
			release, filterErr := outputBuffer.Push(pending)
			if filterErr != nil {
				writeErr = filterErr
				clientGone = true
				return false
			}
			for _, filtered := range release {
				if err := writeResponsesWSMessage(conn, filtered); err != nil {
					writeErr = err
					clientGone = true
					return false
				}
			}
			wroteAnyBody = true
		}
		pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
		pendingFirstTokenBytes = 0
		return true
	}

	readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
		parsed := gjson.ParseBytes(data)
		eventType := parsed.Get("type").String()
		ttftGuard.MarkProgress(eventType)
		isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
		if !ttftRecorded && isFirstToken {
			firstTokenMs = int(time.Since(start).Milliseconds())
			ttftRecorded = true
		}
		if eventType == "response.output_text.delta" {
			deltaCharCount += len(parsed.Get("delta").String())
		}
		if image, ok := extractImageFromOutputItemDone(data, model); ok {
			imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
		}
		if eventType == "response.completed" {
			usage = extractUsageFromResult(parsed.Get("response.usage"))
			if tier := parsed.Get("response.service_tier").String(); tier != "" {
				actualServiceTier = tier
			}
			cacheCompletedResponse(respCacheOwner, []byte(expandedInputRaw), data)
			gotTerminal = true
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
			gotTerminal = true
		}
		if !clientGone {
			shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
			if shouldDefer {
				pendingFirstTokenMessages = append(pendingFirstTokenMessages, append([]byte(nil), data...))
				pendingFirstTokenBytes += len(data)
				if pendingFirstTokenBytes <= 1024*1024 {
					return eventType != "response.completed" && eventType != "response.failed"
				}
				if !flushPendingFirstTokenMessages() {
					return false
				}
			} else {
				// 首包前收到可重试的 response.failed（额度耗尽/限流/5xx/401）时，
				// 不把失败帧下发给客户端：丢弃尚未发送的前导缓冲并提前结束读取，
				// 让外层循环透明换到健康账号重试，避免客户端反复 Reconnecting。
				// 已经向客户端写过内容（wroteAnyBody / 已记录首 token）则照常透传。
				if (silentRetryEnabled || hideUpstreamErrors) && eventType == "response.failed" && !ttftRecorded && !wroteAnyBody && responseFailedRetryable(terminalFailurePayload) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					return false
				}
				// 首 token 前的不可重试 response.failed(如 context_length_exceeded)
				// 不透传原始失败帧:丢弃前导缓冲并提前结束读取,循环外按真实错误
				// 语义返回 error 帧 + 非正常 close code(与 SSE 路径返回 4xx 对齐)。
				// 可重试的失败不在此拦截:silent retry 开启时由上面的分支换号重试,
				// 关闭时按既有约定原样透传失败帧。
				if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, clientGone) &&
					!responseFailedRetryable(terminalFailurePayload) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					abortedForErrorClose = true
					return false
				}
				if len(pendingFirstTokenMessages) > 0 && !flushPendingFirstTokenMessages() {
					return false
				}
				release, filterErr := outputBuffer.Push(data)
				if filterErr != nil {
					writeErr = filterErr
					clientGone = true
					return false
				}
				for _, filtered := range release {
					if err := writeResponsesWSMessage(conn, filtered); err != nil {
						writeErr = err
						clientGone = true
					} else {
						wroteAnyBody = true
					}
				}
			}
		}
		return eventType != "response.completed" && eventType != "response.failed"
	})
	if writeErr == nil && outputBuffer != nil {
		remaining, err := outputBuffer.Flush()
		if err != nil {
			writeErr = err
		} else {
			for _, message := range remaining {
				if err := writeResponsesWSMessage(conn, message); err != nil {
					writeErr = err
					break
				}
			}
		}
	}

	totalDuration := int(time.Since(start).Milliseconds())
	outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
	if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
		outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
	}
	ttftGuard.Stop()
	var responseFailedDecision codex429Decision
	if len(terminalFailurePayload) > 0 {
		outcome = classifyResponseFailedOutcome(terminalFailurePayload)
		responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
		if responseFailedDecision.Reason != "" {
			outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
		}
		// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
		// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
		h.logUpstreamCyberPolicy(c, "/v1/responses", model, responseFailedErrorBody(terminalFailurePayload))
	}
	if fallbackLog != nil {
		fallbackLog.LogHTTPAttemptCompletion("/v1/responses", account.ID(), fallbackAttempt, totalDuration, firstTokenMs, outcome.logStatusCode)
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, viaWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
		resp.Body.Close()
		return &responsesWSRetryableStreamError{outcome: outcome}
	}
	if silentRetryEnabled && outcome.penalize && !wroteAnyBody && c.Request.Context().Err() == nil && writeErr == nil {
		resp.Body.Close()
		if !isFirstTokenTimeoutOutcome(outcome) {
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		return &responsesWSRetryableStreamError{outcome: outcome}
	}
	if outcome.logStatusCode != http.StatusOK {
		log.Printf("Responses WebSocket stream ended abnormally (account %d, status %d): %s, relayed about %d chars", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
		if deltaCharCount > 0 && usage == nil {
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
		Endpoint:             "/v1/responses",
		Model:                model,
		EffectiveModel:       logEffectiveModel,
		StatusCode:           outcome.logStatusCode,
		DurationMs:           totalDuration,
		FirstTokenMs:         firstTokenMs,
		ReasoningEffort:      reasoningEffort,
		InboundEndpoint:      "/v1/responses",
		UpstreamEndpoint:     "/v1/responses",
		Stream:               true,
		ViaWebsocket:         viaWebsocket,
		ServiceTier:          usageTiers.ServiceTier,
		RequestedServiceTier: usageTiers.RequestedServiceTier,
		ActualServiceTier:    usageTiers.ActualServiceTier,
		BillingServiceTier:   usageTiers.BillingServiceTier,
	}
	if outcome.logStatusCode != http.StatusOK {
		logInput.ErrorMessage = usageLogErrorMessage(outcome.logStatusCode, []byte(outcome.failureMessage))
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
	applyImageUsageLogInfo(logInput, imageLogInfo)
	h.logUsageForRequest(c, logInput)

	resp.Body.Close()
	if outcome.penalize {
		recyclePooledClient(account, proxyURL)
		h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
	} else if outcome.logStatusCode == http.StatusOK {
		h.store.ClearModelCooldown(account, effectiveModel)
		h.store.ConfirmResponsesAvailableSince(account, start)
		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
	}
	h.store.Release(account)

	if writeErr != nil {
		return errResponsesWSClientGone
	}
	if abortedForErrorClose && !wroteAnyBody {
		// 首 token 前上游失败且未向客户端写过任何帧:发结构化 error 帧后按错误类别
		// 关闭连接,避免下游把"正常收尾的会话"当成功并按预估 input token 计费。
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		_ = writeResponsesWSError(conn, clientErr)
		return newResponsesWSCloseError(responsesWSCloseCodeForStatus(outcome.logStatusCode), clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && hideUpstreamErrors && len(terminalFailurePayload) > 0 && !wroteAnyBody {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, true)
		_ = writeResponsesWSError(conn, clientErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && len(terminalFailurePayload) == 0 {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		_ = writeResponsesWSError(conn, clientErr)
		return newResponsesWSCloseError(websocket.CloseInternalServerErr, clientErr.Message, apiErr)
	}
	return nil
}

func normalizeResponsesWebSocketClientPayload(raw []byte) ([]byte, string, *api.APIError) {
	trimmed := []byte(strings.TrimSpace(string(raw)))
	if len(trimmed) == 0 {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "empty websocket request payload", api.ErrorTypeInvalidRequest)
	}
	if len(trimmed) > security.MaxRequestBodySize {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "请求体过大", api.ErrorTypeInvalidRequest)
	}
	if !gjson.ValidBytes(trimmed) {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "invalid websocket request payload", api.ErrorTypeInvalidRequest)
	}

	eventType := strings.TrimSpace(gjson.GetBytes(trimmed, "type").String())
	normalized := trimmed
	switch eventType {
	case "":
		eventType = "response.create"
		var err error
		normalized, err = sjson.SetBytes(normalized, "type", eventType)
		if err != nil {
			return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "invalid websocket request payload", api.ErrorTypeInvalidRequest)
		}
	case "response.create":
	case "response.append":
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "response.append is not supported; use response.create with previous_response_id", api.ErrorTypeInvalidRequest)
	default:
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, fmt.Sprintf("unsupported websocket request type: %s", eventType), api.ErrorTypeInvalidRequest)
	}

	model := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if model == "" {
		return nil, "", api.NewAPIError(api.ErrCodeMissingField, "model is required in response.create payload", api.ErrorTypeInvalidRequest)
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(normalized, "previous_response_id").String())
	if strings.HasPrefix(previousResponseID, "msg_") {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidParameter, "previous_response_id must be a response.id (resp_*), not a message id", api.ErrorTypeInvalidRequest)
	}

	return normalized, model, nil
}

func (h *Handler) inspectPromptFilterOpenAIForWebSocket(c *gin.Context, conn *websocket.Conn, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.Inspect(rawBody, endpoint, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		text := promptfilter.ExtractText(rawBody, endpoint, cfg.MaxTextLength)
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	errorCode := api.ErrorCode("prompt_blocked")
	errorMessage := "Request contains content blocked by prompt filter"
	if identity, verified := verifyNewAPIIdentity(c, cfg.Advanced.NewAPI); verified {
		strike, ban := h.recordNewAPIOffense(c, cfg, identity)
		h.writeNewAPIPolicyHeaders(c, strike, ban)
		errorCode = api.ErrorCode("request_policy_violation")
		errorMessage = "请求违规"
	}
	_ = writeResponsesWSError(conn, api.NewAPIError(errorCode, errorMessage, api.ErrorTypeInvalidRequest))
	return true
}

func isResponsesWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func writeResponsesWSError(conn *websocket.Conn, apiErr *api.APIError) error {
	if apiErr == nil {
		apiErr = api.NewAPIError(api.ErrCodeServerError, "Internal server error", api.ErrorTypeServer)
	}
	payload, err := json.Marshal(struct {
		Type  string        `json:"type"`
		Error *api.APIError `json:"error"`
	}{
		Type:  "error",
		Error: apiErr,
	})
	if err != nil {
		return err
	}
	return writeResponsesWSMessage(conn, payload)
}

func responsesWSClientUpstreamAPIError(apiErr *api.APIError, hideUpstreamErrors bool) *api.APIError {
	if !hideUpstreamErrors {
		return apiErr
	}
	return api.NewAPIError(api.ErrCodeUpstreamError, responsesWSFriendlyUpstreamErr, api.ErrorTypeUpstream)
}

func writeResponsesWSMessage(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return errResponsesWSClientGone
	}
	_ = conn.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func closeResponsesWS(conn *websocket.Conn, code int, reason string) {
	if conn == nil {
		return
	}
	reason = truncateWebSocketCloseReason(reason)
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(responsesWSWriteTimeout))
}

func truncateWebSocketCloseReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= 120 {
		return reason
	}
	return reason[:120]
}

func newResponsesWSCloseError(code int, reason string, err error) error {
	return &responsesWSCloseError{
		code:   code,
		reason: truncateWebSocketCloseReason(reason),
		err:    err,
	}
}

// responsesWSCloseCodeForStatus 把上游失败的 HTTP 语义状态码映射为 WebSocket close code:
// 429 → 1013(稍后重试);其余 4xx 确定性客户端错误 → 1008(策略拒绝);5xx → 1011。
func responsesWSCloseCodeForStatus(statusCode int) int {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return websocket.CloseTryAgainLater
	case statusCode >= 400 && statusCode < 500:
		return websocket.ClosePolicyViolation
	default:
		return websocket.CloseInternalServerErr
	}
}

func responsesWSUpstreamAPIError(statusCode int, body []byte) *api.APIError {
	message := usageLogErrorMessage(statusCode, body)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", statusCode)
	}
	errCode := api.ErrCodeUpstreamError
	errType := api.ErrorTypeUpstream
	switch statusCode {
	case http.StatusTooManyRequests:
		errCode = api.ErrCodeRateLimitReached
		errType = api.ErrorTypeRateLimit
	case http.StatusUnauthorized, http.StatusForbidden:
		errCode = api.ErrCodeInvalidAuth
		errType = api.ErrorTypeAuthentication
	case http.StatusBadRequest:
		errCode = api.ErrCodeInvalidRequest
		errType = api.ErrorTypeInvalidRequest
	}
	return api.NewAPIError(errCode, message, errType)
}
