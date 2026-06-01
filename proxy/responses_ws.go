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
)

var responsesWSUpgrader = websocket.Upgrader{
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var errResponsesWSClientGone = errors.New("responses websocket client disconnected")

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

	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionID, apiKeyID)
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

	accountFilter := accountFilterForModel(effectiveModel)
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)
	invalidEncryptedContentRetried := false
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, excludeAccounts, accountFilter)
		if account == nil {
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter)
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					apiErr = responsesWSUpstreamAPIError(lastStatusCode, lastBody)
				} else {
					apiErr = api.NewAPIError(api.ErrCodeServiceUnavailable, noAvailableAccountMessage(effectiveModel), api.ErrorTypeServer)
				}
				_ = writeResponsesWSError(conn, apiErr)
				return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

		apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()
		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionID)

		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		resp, reqErr := ExecuteRequest(upstreamCtx, account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, true)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
				_ = writeResponsesWSError(conn, apiErr)
				return newResponsesWSCloseError(websocket.CloseInternalServerErr, apiErr.Message, reqErr)
			}
			log.Printf("Responses WebSocket upstream request failed (attempt %d): %v", attempt+1, reqErr)
			if shouldRetryRequestError(reqErr, &generalRetries, maxRetries) {
				continue
			}
			apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			_ = writeResponsesWSError(conn, apiErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, reqErr)
		}

		if resp.StatusCode != http.StatusOK {
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
			excludeAccounts[account.ID()] = true

			log.Printf("Responses WebSocket upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
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

			apiErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
			_ = writeResponsesWSError(conn, apiErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
		}

		if err := h.streamResponsesWSUpstream(c, conn, resp, account, proxyURL, affinityKey, logModel, effectiveModel, logEffectiveModel, reasoningEffort, serviceTier, expandedInputRaw, start); err != nil {
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
	expandedInputRaw string,
	start time.Time,
) error {
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
	gotTerminal := false
	deltaCharCount := 0
	var readErr error
	var writeErr error
	clientGone := false
	var imageLogInfo imageUsageLogInfo
	var terminalFailurePayload []byte

	readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
		parsed := gjson.ParseBytes(data)
		eventType := parsed.Get("type").String()
		if !ttftRecorded && isFirstTokenEvent(eventType) {
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
			cacheCompletedResponse([]byte(expandedInputRaw), data)
			gotTerminal = true
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
			gotTerminal = true
		}
		if !clientGone {
			if err := writeResponsesWSMessage(conn, data); err != nil {
				writeErr = err
				clientGone = true
			}
		}
		return eventType != "response.completed" && eventType != "response.failed"
	})

	totalDuration := int(time.Since(start).Milliseconds())
	outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
	if len(terminalFailurePayload) > 0 {
		outcome = classifyResponseFailedOutcome(terminalFailurePayload)
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
	SyncCodexUsageState(h.store, account, resp)
	if outcome.penalize {
		recyclePooledClient(account, proxyURL)
		h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
	} else if outcome.logStatusCode == http.StatusOK {
		h.store.ClearModelCooldown(account, effectiveModel)
		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
	}
	h.store.Release(account)

	if writeErr != nil {
		return errResponsesWSClientGone
	}
	if outcome.logStatusCode != http.StatusOK && len(terminalFailurePayload) == 0 {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseInternalServerErr, apiErr.Message, apiErr)
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
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	_ = writeResponsesWSError(conn, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		"Request contains content blocked by prompt filter",
		api.ErrorTypeInvalidRequest,
	))
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
