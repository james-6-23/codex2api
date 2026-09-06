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
	"sync/atomic"
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
	responsesWSFirstMessageTimeout        = 30 * time.Second
	responsesWSWriteTimeout               = 30 * time.Second
	responsesWSFriendlyUpstreamErr        = "上游服务临时繁忙，请稍后重试"
	newAPIPolicyWebSocketEventField       = "__newapi_policy_event_id"
	newAPIPolicyWebSocketCapabilityHeader = "X-Codex2API-Policy-Event-ID"
	newAPIPolicyWebSocketCapabilityV1     = "v1"
	responsesWSInboundQueueCapacity       = 16
)

var responsesWSUpgrader = websocket.Upgrader{
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var errResponsesWSClientGone = errors.New("responses websocket client disconnected")

type responsesWSRetryableStreamError struct {
	outcome   streamOutcome
	eventType string
}

func (e *responsesWSRetryableStreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.outcome.failureMessage
}

// responsesWSContinuationNotFoundError 表示上游认不出请求里的 previous_response_id。
// 续链 id 只在创建它的账号上存在，除了换号外，上游侧的响应丢失/未落库也会走到这里。
// 外层循环据此把请求降级为自包含请求重试一次，而不是把 400 直接甩给客户端（issue #400）。
type responsesWSContinuationNotFoundError struct{}

func (e *responsesWSContinuationNotFoundError) Error() string {
	return "upstream rejected previous_response_id"
}

// isPreviousResponseNotFoundBody 识别上游的 previous_response_not_found 错误，
// 同时兼容 HTTP 错误体与流内 response.failed / error 帧两种载荷形态。
func isPreviousResponseNotFoundBody(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	// {"error":{...}} / {"response":{"error":{...}}} 归一化后取 error.*；
	// 裸 error 帧（{"type":"error","code":...,"message":...}）再看顶层字段。
	body := responseFailedErrorBody(payload)
	for _, path := range []string{"error.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "previous_response_not_found") {
			return true
		}
	}
	for _, path := range []string{"error.message", "message"} {
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if message == "previous_response_id is not available for this user" ||
			strings.Contains(message, "previous response with id") {
			return true
		}
	}
	return false
}

// degradeResponsesWSContinuationBody 把续链请求降级为自包含请求：先用本地响应缓存
// 把历史 items 补回 input，再剥离 previous_response_id。剥离后请求不再依赖上游会话
// 状态，可以落到任意账号继续，而不是整轮失败。
func degradeResponsesWSContinuationBody(codexBody []byte, cacheOwner string) []byte {
	expanded, _ := expandPreviousResponse(codexBody, cacheOwner)
	expanded, _ = sjson.DeleteBytes(expanded, "previous_response_id")
	return expanded
}

type responsesWSCloseError struct {
	code   int
	reason string
	err    error
}

type responsesWSForwardOptions struct {
	auditEndpoint        string
	transformClientEvent func([]byte) []byte
	onResponseCompleted  func([]byte)
}

// responsesWSInboundMessage is produced by the connection's only reader. A
// dedicated read pump keeps receiving close/control frames while an upstream
// turn is backing off, so a client disconnect can cancel an unlimited retry
// loop without introducing a second concurrent WebSocket reader.
type responsesWSInboundMessage struct {
	messageType int
	payload     []byte
	turn        int
	err         error
	queuedBytes *atomic.Int64
}

type responsesWSInboundObserver func(responsesWSInboundMessage)

func (m *responsesWSInboundMessage) releaseQueueBudget() {
	if m == nil || m.queuedBytes == nil {
		return
	}
	m.queuedBytes.Add(-int64(len(m.payload)))
	m.queuedBytes = nil
}

func startResponsesWSReadPump(ctx context.Context, conn *websocket.Conn, observers ...responsesWSInboundObserver) (context.Context, <-chan responsesWSInboundMessage, <-chan struct{}, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithCancel(ctx)
	messages := make(chan responsesWSInboundMessage, responsesWSInboundQueueCapacity)
	done := make(chan struct{})
	var queuedBytes atomic.Int64
	queueByteLimit := int64(security.MaxRequestBodySize)
	if queueByteLimit < 1 {
		queueByteLimit = int64(security.DefaultMaxRequestBodySize)
	}
	go func() {
		defer close(done)
		defer close(messages)
		for turn := 0; ; turn++ {
			if turn == 0 {
				_ = conn.SetReadDeadline(time.Now().Add(responsesWSFirstMessageTimeout))
			} else {
				_ = conn.SetReadDeadline(time.Time{})
			}
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				// Cancel before notifying the consumer. If the bounded handoff
				// buffer is already full, the active upstream turn still
				// observes cancellation and will stop without waiting for it.
				cancel()
				select {
				case messages <- responsesWSInboundMessage{turn: turn, err: err}:
				default:
				}
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			payloadBytes := int64(len(payload))
			if queuedBytes.Add(payloadBytes) > queueByteLimit {
				queuedBytes.Add(-payloadBytes)
				cancel()
				return
			}
			message := responsesWSInboundMessage{
				messageType: messageType,
				payload:     payload,
				turn:        turn,
				queuedBytes: &queuedBytes,
			}
			// Observers must stay non-blocking. Realtime uses this hook to signal a
			// response.cancel while the serial consumer is waiting on an upstream
			// retry; the read pump remains the connection's only reader.
			for _, observer := range observers {
				if observer != nil {
					observer(message)
				}
			}
			select {
			case messages <- message:
			case <-readCtx.Done():
				queuedBytes.Add(-payloadBytes)
				return
			default:
				queuedBytes.Add(-payloadBytes)
				// A turn is processed serially. The bounded count and byte budgets
				// absorb normal Realtime event bursts without allowing a stalled
				// upstream turn to retain unbounded client input.
				// Never block the only reader here: doing so would hide a later
				// close frame and leave an unlimited upstream retry running.
				cancel()
				return
			}
		}
	}()
	return readCtx, messages, done, cancel
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
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, nil) {
			return
		}
	}

	conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		log.Printf("Responses WebSocket upgrade failed: %v", err)
		return
	}
	conn.SetReadLimit(int64(security.MaxRequestBodySize))
	requestCtx, messages, readPumpDone, cancel := startResponsesWSReadPump(c.Request.Context(), conn)
	c.Request = c.Request.WithContext(requestCtx)
	defer func() {
		cancel()
		_ = conn.Close()
		<-readPumpDone
	}()

	for {
		var message responsesWSInboundMessage
		var ok bool
		select {
		case message, ok = <-messages:
		case <-requestCtx.Done():
			return
		}
		if !ok {
			return
		}
		message.releaseQueueBudget()
		if message.err != nil {
			if websocket.IsCloseError(message.err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				return
			}
			if message.turn == 0 {
				log.Printf("Responses WebSocket first message read failed: %v", message.err)
			}
			return
		}
		if requestCtx.Err() != nil {
			return
		}

		if message.messageType != websocket.TextMessage && message.messageType != websocket.BinaryMessage {
			apiErr := api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported websocket message type", api.ErrorTypeInvalidRequest)
			_ = writeResponsesWSError(conn, apiErr)
			closeResponsesWS(conn, websocket.CloseUnsupportedData, apiErr.Message)
			return
		}

		payload, forwardedEventID := stripNewAPIPolicyWebSocketEventID(message.payload)
		if forwardedEventID == "" {
			forwardedEventID = fmt.Sprintf("responses:%d", message.turn)
		}
		if err := h.forwardResponsesWebSocketTurn(c, conn, payload, forwardedEventID, nil); err != nil {
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

func newAPIPolicyWebSocketUpgradeHeaders() http.Header {
	header := make(http.Header)
	header.Set(newAPIPolicyWebSocketCapabilityHeader, newAPIPolicyWebSocketCapabilityV1)
	return header
}

func stripNewAPIPolicyWebSocketEventID(payload []byte) ([]byte, string) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, ""
	}
	eventID := normalizeNewAPIPolicyWebSocketEventID(gjson.GetBytes(payload, newAPIPolicyWebSocketEventField).String())
	cleaned, err := sjson.DeleteBytes(payload, newAPIPolicyWebSocketEventField)
	if err != nil {
		return payload, ""
	}
	return cleaned, eventID
}

func (h *Handler) forwardResponsesWebSocketTurn(c *gin.Context, conn *websocket.Conn, rawPayload []byte, policyEventID string, options *responsesWSForwardOptions) (returnErr error) {
	if apiErr := h.refreshNewAPIWebSocketBinding(c, time.Now()); apiErr != nil {
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	// Each response.create is a separate logical request. Keep the verified
	// connection identity only while its binding remains valid, and never reuse a
	// prior frame's config or body digest.
	resetPromptRequestSecurityFrame(c)
	resetPromptPolicyRequestCorrelationID(c)
	resetUpstreamRequestTrace(c)
	quotaParentRequest := c.Request
	if err := h.refreshAPIKeyModelRequestQuotaTurn(c); err != nil {
		return writeResponsesWSError(conn, apiKeyModelRequestError(err).apiErr)
	}
	defer func() { c.Request = quotaParentRequest }()
	c.Set(promptGuardPolicyEventIDContextKey, policyEventID)
	rawBody, model, apiErr := normalizeResponsesWebSocketClientPayload(rawPayload)
	if apiErr != nil {
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	// WebSocket turn metadata is frame-local. Cache a complete zero-or-set
	// snapshot before any body rewrite so a later frame can never inherit the
	// prior frame's compaction badges.
	compactionMeta := requestBodyCompactionMeta(rawBody)
	cacheRequestCompactionMeta(c, compactionMeta)

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
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
	auditEndpoint := "/v1/responses"
	if options != nil {
		if configured := strings.TrimSpace(options.auditEndpoint); configured != "" {
			auditEndpoint = configured
		}
	}
	if blocked, delegated := h.inspectPromptFilterOpenAIForWebSocket(c, conn, rawBody, auditEndpoint, model, policyEventID); blocked {
		// A verified NewAPI connection owns warning/ban state. Keep the upstream
		// WebSocket alive after returning the signed decision so NewAPI can show
		// the first warning and accept another frame; it closes both peers only
		// when its own configured punishment threshold is reached.
		if delegated {
			return nil
		}
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
	hasPreviousResponse := strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String()) != ""
	turnContinuation := codexWSTurnContinuationToken(rawBody) != ""
	_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)
	respCacheOwner := responseCacheOwner(apiKeyID)
	ruleIdentity := h.payloadRuleIdentity(c)
	// 上下文压缩轮豁免首字超时看门狗（issue #381）：压缩首帧天然慢，超时换号无益。
	bodySignalCompact := compactionMeta.ProtocolTriggered
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	codexBody, expandedInputRaw := PrepareResponsesWebSocketBody(rawBody)
	// strip 策略：剥离图片工具能力声明后作为普通文本请求继续（issue #411）。
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
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
	// Only a request that passed payload, prompt-policy and API-key admission may
	// replace the active owner. Claim before concurrency/account acquisition so
	// the old request can release those leases for the new one.
	if preemptCtx, cleanupPreempt, armed := h.beginResponsesWSSessionPreemption(c.Request.Context(), c, rawBody, sessionIdentity); armed {
		originalRequest := c.Request
		c.Request = originalRequest.WithContext(preemptCtx)
		defer func() {
			cleanupPreempt()
			c.Request = originalRequest
		}()
		if preemptCtx.Err() != nil {
			return errResponsesWSClientGone
		}
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
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// resolveCompactionAffinity 只在已知来源相互冲突时报错；缓存故障按未知
	// 来源处理，保持正常调度。
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		apiErr = compactionProvenanceConflictAPIError()
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
	}
	// scope 并发位在选中账号后才能占，请求退出时统一释放（issue #439 v2）。
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	wsRetrySettings := CurrentRuntimeSettings()
	hideUpstreamErrors := wsRetrySettings.CodexWSHideErrors
	silentRetryEnabled := wsRetrySettings.CodexWSSilentRetry
	continuousRetryPolicy := database.NormalizeContinuousRetryPolicy(wsRetrySettings.ContinuousRetryPolicy)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryDeadlineContext(c, continuousRetryPolicy)
	timeoutTerminalWritten := false
	writeTimeoutTerminal := func() error {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamTimeout, continuousRetryTimeoutMessage, api.ErrorTypeUpstream)
		if lastFailure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			message := usageLogErrorMessage(lastFailure.status, lastFailure.body)
			if message == "" {
				message = fmt.Sprintf("Upstream returned HTTP %d", lastFailure.status)
			}
			apiErr = api.NewAPIError(api.ErrorCode(fmt.Sprintf("upstream_%d", lastFailure.status)), message, api.ErrorTypeUpstream)
		}
		if !timeoutTerminalWritten {
			_ = writeResponsesWSError(conn, apiErr)
			timeoutTerminalWritten = true
		}
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	defer func() {
		timedOut := settleContinuousRetryDeadline(c.Request.Context())
		if timedOut {
			returnErr = writeTimeoutTerminal()
		}
		stopRetryDeadline()
	}()
	stopRetryKeepalive := installContinuousRetryWSKeepalive(c, conn)
	defer stopRetryKeepalive()
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		activateContinuousRetryKeepalive(c.Request.Context())
	}
	// The continuous selector is independent from the legacy finite WebSocket
	// silent-retry switch. Selected failures use its unlimited budget even when
	// the legacy switch is disabled; unselected failures retain old semantics.
	retryEnabled := silentRetryEnabled || continuousRetryPolicy.Enabled
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
	// 官方 Codex 每个 turn 都使用新的 ModelClientSession，并只在同一 turn 的
	// 后续请求里回送 x-codex-turn-state。只有该信号和既有绑定同时存在时，才允许
	// 忽略本地 WHAM 100% 快照；previous_response_id 本身不足以证明这是活跃 turn。
	continuationPinned := turnContinuation && turnHasBinding
	continuationDegraded := false
	degradeContinuation := func(reason string, attempt int) {
		continuationDegraded = true
		continuationPinned = false
		codexBody = degradeResponsesWSContinuationBody(codexBody, respCacheOwner)
		expandedInputRaw = responsesInputRaw(codexBody)
		log.Printf("Responses WebSocket continuation degraded: %s, stripped previous_response_id and retried once (attempt %d)", reason, attempt)
	}
	preserveContinuationBinding := func() bool {
		return continuationPinned || (hasPreviousResponse && !continuationDegraded)
	}
	// 续链 id 上游已经明确不认识时，turn-state 钉号不能再挡住降级：
	// 钉死只约束「别换号」，不该把 previous_response_not_found 原样甩给
	// Codex CLI（它几乎每轮都带回 x-codex-turn-state，#541）。
	canDegradeContinuation := func() bool {
		return hasPreviousResponse && !continuationDegraded
	}
	var wsHTTPFallback websocketHTTPFallbackState
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	var affinityGuard auth.SessionAffinityGuard
	for attempt := 0; ; attempt++ {
		if c.Request.Context().Err() != nil {
			return errResponsesWSClientGone
		}
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			affinityGuard = auth.SessionAffinityGuard{}
			if !continuationPinned && hasPreviousResponse && !continuationDegraded {
				// 绑定账号已被本次请求硬排除（上一轮 429/5xx 等）时不必再等它 30s：
				// 排除在本请求内不会解除，直接剥离 previous_response_id 换号。
				// 这与 turn-state 的 WHAM 例外无关：即使不是活跃 turn，旧续链
				// id 也不能被带到一个明确不同的账号上反复失败。
				if boundID, bound := h.store.SessionAffinityAccountID(affinityKey); bound {
					if exclude := retryExclusions.ForSelection(); exclude[boundID] {
						degradeContinuation(fmt.Sprintf("bound account %d excluded by this request", boundID), attempt+1)
					}
				}
			}
			if attempt == 0 && compactionAffinity.Known && !continuationPinned {
				account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			}
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			} else if continuationPinned {
				account, stickyProxyURL = h.nextRetryAccountForContinuationWithDispatch(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy)
			} else {
				account, stickyProxyURL, affinityGuard = h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy)
			}
		}
		if account == nil {
			if c.Request.Context().Err() != nil {
				return errResponsesWSClientGone
			}
			if compactionAffinity.Known {
				apiErr = compactionUpstreamUnavailableAPIError()
			} else if lastRetryableUpstreamErr != nil {
				apiErr = responsesWSClientUpstreamAPIError(lastRetryableUpstreamErr, hideUpstreamErrors)
			} else if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				apiErr = responsesWSUpstreamAPIError(lastStatusCode, lastBody)
			} else if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				// 候选被 scope 预算剔空（issue #439）：按限流语义回帧，而不是「无可用账号」。
				apiErr = api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit)
			} else if h.store.HasUsageLimitedCandidateWithDispatch(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				apiErr = api.NewAPIError(api.ErrCodeRateLimitReached, "Codex 账号用量窗口已达上限", api.ErrorTypeRateLimit)
			} else {
				apiErr = api.NewAPIError(api.ErrCodeServiceUnavailable, noAvailableAccountMessage(effectiveModel), api.ErrorTypeServer)
			}
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSError(conn, apiErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
		}
		if attempt > 0 {
			clearNewAPIUpstreamCyberPolicyDecision(c)
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback && !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return errResponsesWSClientGone
			}
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
		// 身份按 attempt 附加实际选中账号维度：account_* 门随重试换号重新匹配（issue #410）。
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
		useWebsocket := !wsHTTPFallback.ForceHTTP()
		// 生图请求改走 HTTP 上游（客户端仍是 WS）：WebSocket 上游传输大体积
		// 图片数据会卡死（issue #220）；自然语言生图意图也需保留图片工具（issue #288）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(rawBody) {
			useWebsocket = false
		}
		// 体积达到已学习的 1009 阈值时直接首发 HTTP,跳过 WS 必败等待(issue #404)。
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] 请求体 %dKB 达到已学习的 1009 体积阈值，直接走 HTTP 上游 (endpoint=/v1/responses, ingress=ws)", len(codexBody)/1024)
			}
		}
		// WebSocket 上游下剥离自动注入的图片工具，防止模型自主生图卡死。
		upstreamBody := codexBody
		attemptExpandedInputRaw := expandedInputRaw
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		} else if prevID := strings.TrimSpace(gjson.GetBytes(codexBody, "previous_response_id").String()); prevID != "" {
			// HTTP 上游不支持续链（executor 出站前会剥掉 previous_response_id）：
			// 命中本地响应缓存时先把历史展开进 input[] 再剥离，避免降级后静默失忆；
			// 未命中只能按原样继续，明确记日志便于诊断（issue #548）。只改本次
			// attempt 的出站体，后续换回 WS 的重试仍用原始续链请求。
			if cached := getResponseCache(respCacheOwner, prevID); cached != nil {
				upstreamBody = degradeResponsesWSContinuationBody(codexBody, respCacheOwner)
				attemptExpandedInputRaw = responsesInputRaw(upstreamBody)
				log.Printf("Responses WebSocket HTTP 降级：previous_response_id=%s 已用本地缓存展开为自包含请求 (account=%d)", prevID, account.ID())
			} else {
				log.Printf("Responses WebSocket HTTP 降级：previous_response_id=%s 本地缓存未命中，上游侧会话历史将不可用 (account=%d)", prevID, account.ID())
			}
		}
		// service_tier 记账按 payload 规则改写后的值归因（覆写 service_tier 的规则才生效）。
		serviceTier = EffectiveRequestedServiceTier(upstreamBody, effectiveModel, downstreamHeaders, attemptIdentity)
		// 在 useWebsocket 最终确定后再派生上游身份键：与 handler.go 的
		// Responses/ChatCompletions 路径一致——无显式会话默认每请求隔离上游身份，
		// WS 路径交给 ExecuteRequest 的 stateless 槽位池处理。
		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		})
		durationMs := int(time.Since(start).Milliseconds())
		if c.Request.Context().Err() != nil {
			ttftGuard.Stop()
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			h.store.Release(account)
			return errResponsesWSClientGone
		}

		if reqErr != nil {
			if quotaErr := apiKeyModelRequestError(reqErr); quotaErr != nil {
				ttftGuard.Stop()
				h.store.Release(account)
				// A model-specific budget must not close the connection for other models.
				return writeResponsesWSError(conn, quotaErr.apiErr)
			}
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
				globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("Responses WebSocket upstream close 1009; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			shouldRetry := retryEnabled && retryable && shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			// 传输类失败粘滞同号重试:不记账号失败、不解绑亲和、不硬排除(issue #331)
			stickyRetry := h.shouldStickyTransportRetry(reqErr, kind, timedOut, shouldRetry, continuousRetryPolicy)
			if retryable && kind != "" && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable && !stickyRetry && !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("Responses WebSocket upstream first token timeout, retrying with another account (attempt %s, account %d): %v", retryAttemptProgress(attempt, maxRetries), account.ID(), reqErr)
				if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), true, generalRetries, retryLimit) {
					return errResponsesWSClientGone
				}
				continue
			}
			if retryable && !timedOut && !stickyRetry {
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			}

			if !retryable {
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				_ = writeResponsesWSError(conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseInternalServerErr, clientErr.Message, reqErr)
			}
			log.Printf("Responses WebSocket upstream request failed (attempt %d): %v", attempt+1, reqErr)
			lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return errResponsesWSClientGone
				}
				if !h.bindBufferedStickyRetryAffinity(c.Request.Context(), affinityKey, account, proxyURL, stickyRetry, continuousRetryPolicy) {
					return errResponsesWSClientGone
				}
				if stickyRetry {
					log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %s, ws)", account.ID(), retryAttemptProgress(attempt, maxRetries))
				}
				continue
			}
			apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSError(conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, reqErr)
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, _ := io.ReadAll(resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if c.Request.Context().Err() != nil {
				h.store.Release(account)
				return errResponsesWSClientGone
			}

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
					if !preserveContinuationBinding() {
						h.store.UnbindSessionAffinity(affinityKey, account.ID())
					}
					continue
				}
			}

			// 上游认不出续链 id：账号本身是好的，别记失败也别排除它，
			// 降级成自包含请求后原地重试一次（issue #400）。
			if canDegradeContinuation() && isPreviousResponseNotFoundBody(errBody) {
				degradeContinuation(fmt.Sprintf("upstream rejected previous_response_id on account %d", account.ID()), attempt+1)
				SyncCodexUsageState(h.store, account, resp)
				h.store.Release(account)
				continue
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			if !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, maxRateLimitRetries, continuousRetryPolicy)

			log.Printf("Responses WebSocket upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := retryEnabled && shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses",
				UpstreamEndpoint:       "/v1/responses",
				Stream:                 true,
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
				lastRetryableUpstreamErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return errResponsesWSClientGone
				}
				continue
			}
			if metadata, delegated := newAPIUpstreamCyberPolicyDecision(c); delegated && metadata.EventID != "" {
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				_ = writeResponsesWSError(conn, newAPIPolicyDecisionAPIError(metadata))
				return nil
			}

			apiErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSError(conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
		}

		var fallbackLog *websocketHTTPFallbackState
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			fallbackLog = &wsHTTPFallback
		}
		preserveAffinity := preserveContinuationBinding()
		allowContinuationDegrade := canDegradeContinuation()
		if err := h.streamResponsesWSUpstream(c, conn, resp, account, proxyURL, affinityKey, affinityGuard, preserveAffinity, allowContinuationDegrade, logModel, effectiveModel, logEffectiveModel, reasoningEffort, serviceTier, respCacheOwner, attemptExpandedInputRaw, start, ttftGuard, retryEnabled, hideUpstreamErrors, useWebsocket, fallbackLog, attempt+1, options, continuousRetryPolicy); err != nil {
			if continuousRetryDeadlineExceeded(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			var continuationErr *responsesWSContinuationNotFoundError
			if canDegradeContinuation() && errors.As(err, &continuationErr) {
				// 账号已在流内释放，未记失败也未解绑：剥离续链 id 后原地再试一次。
				// turn-state 钉号同样走这条路：上游已经说找不到 id，换号无益，剥 id 才能继续。
				degradeContinuation(fmt.Sprintf("upstream rejected previous_response_id on account %d", account.ID()), attempt+1)
				continue
			}
			var retryErr *responsesWSRetryableStreamError
			if errors.As(err, &retryErr) {
				lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				if useWebsocket && isWebsocketMessageTooBigOutcome(retryErr.outcome) {
					wsElapsed := time.Since(start)
					globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
					wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(retryErr.outcome.failureMessage))
					log.Printf("Responses WebSocket upstream close 1009 before first event; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), retryErr.outcome.failureMessage)
					continue
				}
				eventType := strings.TrimSpace(retryErr.eventType)
				if retryEnabled && shouldTransparentRetryStreamEventWithBudgets(retryErr.outcome, eventType, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries, false, c.Request.Context().Err(), nil, continuousRetryPolicy) {
					rememberContinuousRetryStreamFailure(c.Request.Context(), retryErr.outcome, retryErr.outcome.failurePayload)
					if isFirstTokenTimeoutOutcome(retryErr.outcome) {
						retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
					} else {
						retryExclusions.MarkStreamFailureForEvent(account.ID(), retryErr.outcome, eventType, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
					}
					retryOrdinal, retryLimit := retryStateForStreamEvent(retryErr.outcome, eventType, generalRetries, rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
					log.Printf("Responses WebSocket upstream stream ended before first token, retrying (attempt %s, account %d): %s", retryAttemptProgress(retryOrdinal-1, retryLimit), account.ID(), retryErr.outcome.failureMessage)
					// 有限首字超时已白等一轮；无限预算仍强制退避，避免无等待循环。
					if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), isFirstTokenTimeoutOutcome(retryErr.outcome), retryOrdinal, retryLimit, resp) {
						return errResponsesWSClientGone
					}
					continue
				}
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				_ = writeResponsesWSError(conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
			}
			if errors.Is(err, errResponsesWSClientGone) {
				return err
			}
			if shouldRetryErr, ok := err.(*responsesWSCloseError); ok && shouldRetryErr.code == websocket.CloseTryAgainLater && !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			return err
		}
		return nil
	}
}

func replayResponsesWSSuccess(replay *continuousRetryWSReplay, outputBuffer *wsPromptOutputBuffer, writeMessage func([]byte) error) (bool, error) {
	if replay == nil {
		return false, nil
	}
	if writeMessage == nil {
		return false, errors.New("nil Responses WebSocket replay writer")
	}
	wroteAny := false
	writeFiltered := func(messages [][]byte) error {
		for _, message := range messages {
			if err := writeMessage(message); err != nil {
				return err
			}
			wroteAny = true
		}
		return nil
	}
	if err := replay.ForEachMessage(func(payload []byte) error {
		if outputBuffer == nil {
			return writeFiltered([][]byte{payload})
		}
		release, err := outputBuffer.Push(payload)
		if err != nil {
			return err
		}
		return writeFiltered(release)
	}); err != nil {
		return wroteAny, err
	}
	if outputBuffer == nil {
		return wroteAny, nil
	}
	remaining, err := outputBuffer.Flush()
	if err != nil {
		return wroteAny, err
	}
	if err := writeFiltered(remaining); err != nil {
		return wroteAny, err
	}
	return wroteAny, nil
}

func (h *Handler) streamResponsesWSUpstream(
	c *gin.Context,
	conn *websocket.Conn,
	resp *http.Response,
	account *auth.Account,
	proxyURL string,
	affinityKey string,
	affinityGuard auth.SessionAffinityGuard,
	preserveAffinity bool,
	allowContinuationDegrade bool,
	model string,
	effectiveModel string,
	logEffectiveModel string,
	reasoningEffort string,
	serviceTier string,
	respCacheOwner string,
	expandedInputRaw string,
	start time.Time,
	ttftGuard *firstTokenTimeoutGuard,
	retryEnabled bool,
	hideUpstreamErrors bool,
	viaWebsocket bool,
	fallbackLog *websocketHTTPFallbackState,
	fallbackAttempt int,
	options *responsesWSForwardOptions,
	continuousRetryPolicy database.ContinuousRetryPolicy,
) error {
	account.Mu().RLock()
	c.Set("x-account-email", account.Email)
	account.Mu().RUnlock()
	c.Set("x-account-proxy", proxyURL)
	c.Set("x-model", model)
	c.Set("x-reasoning-effort", reasoningEffort)

	var firstTokenMs int
	outputBuffer := newWSPromptOutputBuffer(h.promptFilterConfigForRequest(c))
	var usage *UsageInfo
	var actualServiceTier string
	ttftRecorded := false
	// contentTokenSeen 用严格判定（与 first_token_mode 无关）。loose 模式下
	// codex.rate_limits / metadata 会置位 ttftRecorded；本机 2004 还开了
	// preflight passthrough，这两帧会先写出并置位 wroteAnyBody。若用它们做
	// 「首包前」判断，previous_response_not_found 降级在真实上游上永远进不去（#541）。
	contentTokenSeen := false
	preflightSettings := CurrentRuntimeSettings()
	preflightSettings.ContinuousRetryPolicy = continuousRetryPolicy
	preflightPassthrough := continuousRetryPreflightPassthrough(preflightSettings)
	gotTerminal := false
	deltaCharCount := 0
	var readErr error
	var writeErr error
	clientGone := false
	var imageLogInfo imageUsageLogInfo
	var terminalFailurePayload []byte
	var terminalFailureClientPayload []byte
	var preContentErrorCandidate []byte
	var completedResponsePayload []byte
	terminalFailureEventType := ""
	wroteAnyBody := false
	var wsReplay *continuousRetryWSReplay
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		wsReplay = h.newContinuousRetryWSReplay()
	}
	writeClientMessage := func(payload []byte) error {
		return writeResponsesWSMessage(conn, payload)
	}
	// 首 token 前收到不可重试的 response.failed 时置位:不把原始失败帧透传给客户端,
	// 循环外改写 error 帧并按错误类别用非正常 close code 关闭,
	// 让下游中转/计费方明确感知失败,而不是把它当成一次正常结束的会话。
	abortedForErrorClose := false
	// 续链请求在首包前被上游以 previous_response_not_found 拒绝时置位：不透传失败帧，
	// 交回外层剥离 previous_response_id 后重试一次（issue #400）。
	continuationNotFound := false
	pendingFirstTokenMessages := make([][]byte, 0, 4)
	pendingFirstTokenBytes := 0

	flushPendingFirstTokenMessages := func() bool {
		for _, pending := range pendingFirstTokenMessages {
			release, filterErr := outputBuffer.Push(pending)
			if filterErr != nil {
				writeErr = filterErr
				return false
			}
			wrotePending := false
			for _, filtered := range release {
				if err := writeClientMessage(filtered); err != nil {
					writeErr = err
					clientGone = true
					return false
				}
				wrotePending = true
			}
			wroteAnyBody = wroteAnyBody || wrotePending
		}
		pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
		pendingFirstTokenBytes = 0
		return true
	}

	readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, func(sseEvent string, data []byte) bool {
		if wsReplay == nil {
			h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
		}
		parsed := gjson.ParseBytes(data)
		eventType := normalizedUpstreamSSEEventType(sseEvent, data)
		clientData := data
		if options != nil && options.transformClientEvent != nil {
			if transformed := options.transformClientEvent(data); len(transformed) > 0 {
				clientData = transformed
			}
		}
		// 容量降载码（server_is_overloaded/slow_down）对 Codex CLI 是致命错误：
		// 一旦要透传给客户端就改写为可重试的 server_error。冷却/计费/日志用的
		// terminalFailurePayload 取改写前的原始 data，不受影响。
		clientData = sanitizeCapacityShedEventForClient(eventType, clientData)
		ttftGuard.MarkProgress(eventType)
		isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
		if !ttftRecorded && isFirstToken {
			firstTokenMs = int(time.Since(start).Milliseconds())
			ttftRecorded = true
		}
		if !contentTokenSeen && isFirstTokenResult(parsed) {
			contentTokenSeen = true
		}
		if contentTokenSeen {
			preContentErrorCandidate = nil
		}
		if eventType == "response.output_text.delta" {
			deltaCharCount += len(parsed.Get("delta").String())
		}
		if image, ok := extractImageFromOutputItemDone(data, model); ok {
			imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
		}
		if isResponsesSuccessTerminalEvent(eventType) {
			usage = extractUsageFromResult(parsed.Get("response.usage"))
			if tier := parsed.Get("response.service_tier").String(); tier != "" {
				actualServiceTier = tier
			}
			if eventType == "response.completed" {
				completedResponsePayload = append([]byte(nil), data...)
			}
			gotTerminal = true
			preContentErrorCandidate = nil
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
			terminalFailureEventType = eventType
			gotTerminal = true
			preContentErrorCandidate = nil
		}
		if wsReplay != nil {
			// Continuous retry keeps the complete attempt private. Any upstream
			// error event is authoritative and ends the attempt immediately, even if
			// the provider later appends a contradictory response.completed frame.
			// Selective and catch-all modes differ only in whether the classified
			// failure below is selected for another attempt.
			if eventType == "error" {
				terminalFailurePayload = append([]byte(nil), data...)
				terminalFailureEventType = eventType
				gotTerminal = true
			}
			if eventType == "error" || eventType == "response.failed" {
				if allowContinuationDegrade && !contentTokenSeen && isPreviousResponseNotFoundBody(data) {
					continuationNotFound = true
					return false
				}
				terminalFailureClientPayload = append([]byte(nil), clientData...)
				return false
			}
			if err := wsReplay.WriteMessage(clientData); err != nil {
				writeErr = err
				return false
			}
			return !isResponsesSuccessTerminalEvent(eventType)
		}
		standaloneErrorAfterOutput := eventType == "error" && wroteAnyBody
		if !contentTokenSeen && !wroteAnyBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
			preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
			return true
		}
		if standaloneErrorAfterOutput {
			terminalFailurePayload = append([]byte(nil), data...)
			terminalFailureEventType = "error"
			gotTerminal = true
		}
		if !clientGone {
			// 可重试的 error 帧（上游降载先导帧）与生命周期帧一样缓冲：立即写出
			// 会置位 wroteAnyBody，随后的 response.failed 就进不了首包前静默换号分支。
			// previous_response_not_found 的先导 error 帧同样缓冲：它按 invalid_request
			// 分类不属于可重试帧，立即写出会置位 wroteAnyBody，随后的 response.failed
			// 就进不了下面的续链降级分支。
			shouldDefer := shouldDeferPreContentSSEEvent(eventType, contentTokenSeen, gotTerminal, preflightPassthrough) ||
				(!contentTokenSeen && !wroteAnyBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy)) ||
				(allowContinuationDegrade && !contentTokenSeen && !gotTerminal && eventType == "error" && isPreviousResponseNotFoundBody(data))
			if shouldDefer {
				pendingFirstTokenMessages = append(pendingFirstTokenMessages, append([]byte(nil), clientData...))
				pendingFirstTokenBytes += len(clientData)
				if pendingFirstTokenBytes <= 1024*1024 {
					return !isResponsesTerminalEvent(eventType)
				}
				if !flushPendingFirstTokenMessages() {
					return false
				}
			} else {
				// 首包前收到可重试的 response.failed（额度耗尽/限流/5xx/401）时，
				// 不把失败帧下发给客户端：丢弃尚未发送的前导缓冲并提前结束读取，
				// 让外层循环透明换到健康账号重试，避免客户端反复 Reconnecting。
				// 已经向客户端写过内容（wroteAnyBody / 已记录首 token）则照常透传。
				if (retryEnabled || hideUpstreamErrors) && eventType == "response.failed" && !contentTokenSeen && !wroteAnyBody && continuousRetryStreamFailureSelected(classifyResponseFailedOutcome(terminalFailurePayload), terminalFailurePayload, eventType, continuousRetryPolicy) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					return false
				}
				// 续链 id 上游认不出：换号重试没用（换了还是找不到），但剥离续链 id
				// 后同一账号就能继续。前置元数据（rate_limits / metadata）即使已经
				// 写出也不算内容，不能挡住降级——真实 ChatGPT WS 几乎总会先推这两帧。
				if allowContinuationDegrade && eventType == "response.failed" && !contentTokenSeen &&
					isPreviousResponseNotFoundBody(terminalFailurePayload) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					continuationNotFound = true
					return false
				}
				// 首 token 前的不可重试 response.failed(如 context_length_exceeded)
				// 不透传原始失败帧:丢弃前导缓冲并提前结束读取,循环外按真实错误
				// 语义返回 error 帧 + 非正常 close code(与 SSE 路径返回 4xx 对齐)。
				// 可重试的失败不在此拦截:silent retry 开启时由上面的分支换号重试,
				// 关闭时按既有约定原样透传失败帧。
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentTokenSeen, wroteAnyBody, clientGone) &&
					!continuousRetryStreamFailureSelected(classifyResponseFailedOutcome(terminalFailurePayload), terminalFailurePayload, eventType, continuousRetryPolicy) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					abortedForErrorClose = true
					return false
				}
				if len(pendingFirstTokenMessages) > 0 && !flushPendingFirstTokenMessages() {
					return false
				}
				release, filterErr := outputBuffer.Push(clientData)
				if filterErr != nil {
					writeErr = filterErr
					return false
				}
				for _, filtered := range release {
					if err := writeClientMessage(filtered); err != nil {
						writeErr = err
						clientGone = true
					} else {
						wroteAnyBody = true
					}
				}
			}
		}
		return !standaloneErrorAfterOutput && !isResponsesTerminalEvent(eventType)
	})
	if writeErr == nil && outputBuffer != nil && wsReplay == nil {
		remaining, err := outputBuffer.Flush()
		if err != nil {
			writeErr = err
		} else {
			for _, message := range remaining {
				if err := writeClientMessage(message); err != nil {
					writeErr = err
					break
				}
				wroteAnyBody = true
			}
		}
	}

	// 续链 id 失效且首包前拦下：账号无过错，不记失败也不解绑，交回外层降级重试。
	if continuationNotFound && !contentTokenSeen && writeErr == nil && c.Request.Context().Err() == nil {
		_ = wsReplay.Close()
		ttftGuard.Stop()
		resp.Body.Close()
		h.store.Release(account)
		return &responsesWSContinuationNotFoundError{}
	}

	totalDuration := int(time.Since(start).Milliseconds())
	outcome := classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
	outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
	var candidatePromoted bool
	terminalFailurePayload, candidatePromoted = resolvePreContentRetryErrorCandidate(terminalFailurePayload, preContentErrorCandidate, contentTokenSeen, wroteAnyBody, gotTerminal, readErr, c.Request.Context().Err(), writeErr)
	if candidatePromoted {
		terminalFailureEventType = "error"
	} else if len(terminalFailurePayload) > 0 && terminalFailureEventType == "" {
		terminalFailureEventType = gjson.GetBytes(terminalFailurePayload, "type").String()
	}
	if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
		outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
	}
	ttftGuard.Stop()
	var responseFailedDecision codex429Decision
	promptPolicyIncidentID := ""
	if len(terminalFailurePayload) > 0 && !outcome.terminalLocal {
		outcome = classifyResponseFailedOutcome(terminalFailurePayload)
		if withContinuousRetryDeadlinePending(c.Request.Context(), func() {
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
		}) {
			outcome = applyResponseFailedDecisionKind(outcome, terminalFailurePayload, responseFailedDecision)
		} else {
			outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
		}
		// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
		// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
		promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", model, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
			Transport: upstreamPromptPolicyTransport(true, viaWebsocket), StatusCode: outcome.logStatusCode,
			AccountID: account.ID(), AttemptIndex: fallbackAttempt,
		}))
		if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
			outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
		}
	}
	outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
	if fallbackLog != nil {
		fallbackLog.LogHTTPAttemptCompletion("/v1/responses", account.ID(), fallbackAttempt, totalDuration, firstTokenMs, outcome.logStatusCode)
	}
	downstreamWroteBeforeCommit := wroteAnyBody && wsReplay == nil
	transparentRetrySelected := retryEnabled && continuousRetryStreamFailureSelected(outcome, terminalFailurePayload, terminalFailureEventType, continuousRetryPolicy) && !downstreamWroteBeforeCommit && c.Request.Context().Err() == nil && writeErr == nil
	if metadata, delegated := newAPIUpstreamCyberPolicyDecision(c); delegated && metadata.EventID != "" && !transparentRetrySelected {
		_ = wsReplay.Close()
		// WebSocket response headers were committed during the upgrade. Carry the
		// signed CYB decision in a per-turn error frame so NewAPI can count it and
		// decide whether this is the first warning or a ban-worthy recurrence.
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			resp.Body.Close()
			h.store.Release(account)
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSError(conn, newAPIPolicyDecisionAPIError(metadata))
		return nil
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, viaWebsocket, downstreamWroteBeforeCommit, c.Request.Context().Err(), writeErr) {
		_ = wsReplay.Close()
		resp.Body.Close()
		return &responsesWSRetryableStreamError{outcome: outcome, eventType: terminalFailureEventType}
	}
	if transparentRetrySelected {
		_ = wsReplay.Close()
		clearNewAPIUpstreamCyberPolicyDecision(c)
		h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
			AccountID: account.ID(), Endpoint: "/v1/responses", Model: model, EffectiveModel: logEffectiveModel,
			StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
			InboundEndpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", Stream: true, ViaWebsocket: viaWebsocket,
			AttemptIndex: fallbackAttempt, UpstreamErrorKind: outcome.failureKind,
			ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
		}, promptPolicyIncidentID)
		resp.Body.Close()
		if !isFirstTokenTimeoutOutcome(outcome) {
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		if !preserveAffinity {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		}
		return &responsesWSRetryableStreamError{outcome: outcome, eventType: terminalFailureEventType}
	}
	if outcome.logStatusCode == http.StatusOK {
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			if wsReplay != nil {
				_ = wsReplay.Close()
			}
			resp.Body.Close()
			h.store.Release(account)
			return errResponsesWSClientGone
		}
		SyncCodexUsageState(h.store, account, resp)
		if wsReplay != nil {
			committedAny, commitErr := replayResponsesWSSuccess(wsReplay, outputBuffer, writeClientMessage)
			wroteAnyBody = wroteAnyBody || committedAny
			if commitErr != nil {
				writeErr = commitErr
				// Filtering, replay storage, and downstream writes are local commit
				// failures. They terminate this turn and must never reopen an upstream
				// attempt that already reached a successful protocol terminal.
				outcome = classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), nil, writeErr, false)
				outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
			}
		}
	}
	downstreamWrote := wroteAnyBody
	if outcome.logStatusCode == http.StatusOK && writeErr == nil && downstreamWrote {
		if wsReplay != nil {
			_ = wsReplay.ForEachMessage(func(payload []byte) error {
				h.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
				return nil
			})
		}
		if len(completedResponsePayload) > 0 {
			if options != nil && options.onResponseCompleted != nil {
				options.onResponseCompleted(append([]byte(nil), completedResponsePayload...))
			}
			// Only committed response.completed events become continuation history.
			// Failed attempts and locally blocked/unwritable replays leave no cache.
			cacheCompletedResponse(respCacheOwner, []byte(expandedInputRaw), completedResponsePayload)
		}
	}
	_ = wsReplay.Close()
	if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
		h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
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
		AccountID:              account.ID(),
		Endpoint:               "/v1/responses",
		Model:                  model,
		EffectiveModel:         logEffectiveModel,
		StatusCode:             outcome.logStatusCode,
		DurationMs:             totalDuration,
		FirstTokenMs:           firstTokenMs,
		ReasoningEffort:        reasoningEffort,
		InboundEndpoint:        "/v1/responses",
		UpstreamEndpoint:       "/v1/responses",
		Stream:                 true,
		ViaWebsocket:           viaWebsocket,
		ServiceTier:            usageTiers.ServiceTier,
		RequestedServiceTier:   usageTiers.RequestedServiceTier,
		ActualServiceTier:      usageTiers.ActualServiceTier,
		BillingServiceTier:     usageTiers.BillingServiceTier,
		PromptPolicyIncidentID: promptPolicyIncidentID,
		AttemptIndex:           fallbackAttempt,
	}
	if outcome.logStatusCode != http.StatusOK {
		logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
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
		h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
		if !preserveAffinity {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		}
	} else if outcome.logStatusCode == http.StatusOK {
		h.store.ClearModelCooldown(account, effectiveModel)
		h.store.ConfirmResponsesAvailableSince(account, start)
		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
	}
	if outcome.logStatusCode == http.StatusOK {
		h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
	} else {
		h.store.Release(account)
	}
	if outcome.terminalLocal {
		apiErr := api.NewAPIError(api.ErrCodeServerError, continuousRetryLocalFailureMessage, api.ErrorTypeServer)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseInternalServerErr, apiErr.Message, apiErr)
	}
	if c.Request.Context().Err() != nil {
		return errResponsesWSClientGone
	}

	if errors.Is(writeErr, promptfilter.ErrOutputBlocked) {
		apiErr := api.NewAPIError(api.ErrorCode("response_policy_violation"), "模型输出违反安全策略", api.ErrorTypeInvalidRequest)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	if writeErr != nil {
		return errResponsesWSClientGone
	}
	if abortedForErrorClose && !downstreamWrote {
		// 首 token 前上游失败且未向客户端写过任何帧:发结构化 error 帧后按错误类别
		// 关闭连接,避免下游把"正常收尾的会话"当成功并按预估 input token 计费。
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		preserveErrorCode := isPreviousResponseNotFoundBody(terminalFailurePayload)
		if preserveErrorCode {
			// This is deterministic continuation state, not an infrastructure error.
			// Preserve the official code so Codex can classify the failed turn even
			// when generic upstream-error details are hidden.
			apiErr = api.NewAPIError(api.ErrorCode("previous_response_not_found"), outcome.failureMessage, api.ErrorTypeInvalidRequest)
		}
		clientErr := apiErr
		if !preserveErrorCode {
			clientErr = responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		}
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSError(conn, clientErr)
		return newResponsesWSCloseError(responsesWSCloseCodeForStatus(outcome.logStatusCode), clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && !hideUpstreamErrors && len(terminalFailureClientPayload) > 0 && !downstreamWrote {
		// An unselected selective-mode failure still ends the logical turn. Its
		// preceding attempt output remains discarded, while the terminal upstream
		// failure is returned without running the local output filter.
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		if err := writeClientMessage(terminalFailureClientPayload); err != nil {
			return errResponsesWSClientGone
		}
		return nil
	}
	if outcome.logStatusCode != http.StatusOK && hideUpstreamErrors && len(terminalFailurePayload) > 0 && !downstreamWrote {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, true)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSError(conn, clientErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && len(terminalFailurePayload) == 0 {
		errCode := api.ErrCodeUpstreamError
		if outcome.logStatusCode == logStatusUpstreamStreamBreak {
			// 断流(598)用稳定错误码 upstream_stream_break，下游可编程识别并重试
			// (issue #473)；其余上游异常保持通用 upstream_error。
			errCode = api.ErrorCode(ErrorCodeUpstreamStreamBreak)
		}
		apiErr := api.NewAPIError(errCode, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
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

func (h *Handler) inspectPromptFilterOpenAIForWebSocket(c *gin.Context, conn *websocket.Conn, rawBody []byte, endpoint string, model string, policyEventID string) (blocked bool, delegatedToNewAPI bool) {
	if h == nil || h.store == nil {
		return false, false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if item, locked := h.activePromptConversationLock(c, cfg, nil, endpoint, model); locked {
		restriction := promptCyberRestrictionDecision(item, cfg)
		profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
		switch profile {
		case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
		default:
			profile = promptfilter.GuardProfileBalanced
		}
		decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: profile, ReasonCode: restriction.ReasonCode, Terminal: true}
		verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: restriction.Message, FullText: restriction.ReasonCode}
		if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil); verified {
			metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, rawBody, endpoint, model, policyEventID, policyContext.VerificationSecret)
			writeNewAPIPolicyDecisionHeaders(c, metadata)
			writePromptCyberRestrictionHeaders(c, restriction)
			_ = writeResponsesWSError(conn, promptCyberRestrictionAPIError(restriction, newAPIPolicyDecisionDetails(metadata)))
			return true, true
		}
		_ = writeResponsesWSError(conn, promptCyberRestrictionAPIError(restriction, nil))
		return true, false
	}
	// Keep disabled filters off the WebSocket request-body hot path too.
	if !promptfilter.RequiresRequestText(cfg) {
		return false, false
	}
	evaluation := h.evaluatePromptGuardWithConfig(c, cfg, rawBody, nil, endpoint, model, promptfilter.TransportWebSocket)
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action != promptfilter.ActionBlock {
		return false, false
	}
	// 与 HTTP 入口一致:本地 block 立即锁定会话,使后续绕过本地正则的等价变形
	// 也无法通过这条 WS 会话到达上游。这条 WS 路径持有独立的 block 逻辑,漏掉
	// 这一步会让整个前置扼杀在 Codex 的 WebSocket 通道上失效。
	h.lockPromptConversationOnLocalBlock(c, cfg, nil, endpoint, model, evaluation.Decision, verdict)
	errorCode := api.ErrorCode("prompt_blocked")
	errorMessage := localPromptBlockMessage(cfg)
	if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil); verified {
		metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, evaluation.Decision, verdict, cfg, rawBody, endpoint, model, policyEventID, policyContext.VerificationSecret)
		writeNewAPIPolicyDecisionHeaders(c, metadata)
		_ = writeResponsesWSError(conn, newAPILocalPromptPolicyDecisionAPIError(metadata, cfg))
		return true, true
	}
	_ = writeResponsesWSError(conn, api.NewAPIError(errorCode, errorMessage, api.ErrorTypeInvalidRequest))
	return true, false
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
	if isExplicitUpstreamCyberPolicy(body) {
		return api.NewAPIError(api.ErrCodeInvalidRequest, upstreamCyberPolicyUserMessage, api.ErrorTypeInvalidRequest)
	}
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
