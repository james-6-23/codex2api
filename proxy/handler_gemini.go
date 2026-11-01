package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var geminiNativeGenerationMethods = []string{
	"generateContent",
	"streamGenerateContent",
	"countTokens",
}

// GeminiListModels handles GET /v1beta/models for native Gemini clients.
func (h *Handler) GeminiListModels(c *gin.Context) {
	if h.enforceAPIKeyLimitsAndReply(c, "") {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
		return
	}
	models := geminiNativeModelEntries(h.scopedModels(ctx, row))
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// GeminiGetModel handles GET /v1beta/models/{model} for native Gemini clients.
func (h *Handler) GeminiGetModel(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	if action == "" || strings.Contains(action, ":") {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, normalizeGeminiPublicModel(action)) {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	modelID := normalizeGeminiPublicModel(action)
	for _, entry := range geminiNativeModelEntries(h.scopedModels(ctx, row)) {
		name, _ := entry["name"].(string)
		if normalizeGeminiPublicModel(name) == modelID {
			c.JSON(http.StatusOK, entry)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": gin.H{
			"code":    http.StatusNotFound,
			"message": "Not Found",
			"status":  "NOT_FOUND",
		},
	})
}

func geminiNativeModelEntries(models []api.Model) []map[string]any {
	entries := make([]map[string]any, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		id := normalizeGeminiPublicModel(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, geminiNativeModelEntry(id))
	}
	return entries
}

func geminiNativeModelEntry(id string) map[string]any {
	id = normalizeGeminiPublicModel(id)
	return map[string]any{
		"name":                       "models/" + id,
		"displayName":                id,
		"description":                id,
		"supportedGenerationMethods": append([]string(nil), geminiNativeGenerationMethods...),
	}
}

// GeminiModelsAction handles native Gemini API POST /v1beta/models/*action requests
// and routes Antigravity OAuth accounts through the Cloud Code v1internal adapter.
func (h *Handler) GeminiModelsAction(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": parts[0] + " not found.",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	model := normalizeGeminiPublicModel(parts[0])
	method := parts[1]
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(security.MaxRequestBodySize+1)))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	if len(rawBody) > security.MaxRequestBodySize {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request body too large", api.ErrorTypeInvalidRequest))
		return
	}
	if !gjson.ValidBytes(rawBody) {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid JSON in request body", api.ErrorTypeInvalidRequest))
		return
	}
	// [OutputGuard 冷启动门禁] 仅在 readiness 已有结论时强制门禁，避免直接构造
	// Handler 的旧测试在从未加载配置的全局态下被整体 503。
	if outputGuardHTTPGateArmed() && !RequireOutputGuardReadyHTTP(c) {
		return
	}

	switch method {
	case "generateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, false)
	case "streamGenerateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, true)
	case "countTokens":
		h.handleGeminiCountTokens(c, model, rawBody)
	default:
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": method + " not found.",
				"status":  "NOT_FOUND",
			},
		})
	}
}

// geminiCountTokensInboundEndpoint 返回 native countTokens 入口的入站端点标识，
// 与上游端点（v1internal:countTokens）区分，供日志、错误与用量记录归因使用。
func geminiCountTokensInboundEndpoint() string {
	return "/v1beta/models:countTokens"
}

// geminiNativeSelectionOptions 组装 native Gemini 入口（generateContent /
// streamGenerateContent / countTokens）的选号上下文。
//
// 这两个入口的候选池被 antigravityOAuthChannelAccountFilter 硬限定为 Antigravity OAuth，
// 因此渠道必须按候选池的真实归属取 antigravity：auto 渠道 Key（requestUpstreamChannel 为
// 空）走这里时，若沿用请求渠道，auth 侧 isAntigravityProvider("") 为 false，首绑 / Fresh
// 加权避险会整体失效。
func geminiNativeSelectionOptions(c *gin.Context, model string, policy auth.DispatchPolicy) auth.SelectionOptions {
	return selectionOptionsForRequestWithProvider(c, model, policy, database.UpstreamChannelAntigravity)
}

// handleGeminiCountTokens 处理 native Gemini models:countTokens 请求：先过 Key 限额
// 与并发额度，候选池被硬限定为 Antigravity OAuth 并叠加跨实例冷却、渠道与预算过滤，
// 随后在重试循环内带重试排除项与亲和守卫选号，转发到上游 v1internal:countTokens。
func (h *Handler) handleGeminiCountTokens(c *gin.Context, model string, rawBody []byte) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	selectionOpts := geminiNativeSelectionOptions(c, model, dispatchPolicy)
	inboundEndpoint := geminiCountTokensInboundEndpoint()
	upstreamEndpoint := "/v1internal:countTokens"

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts,
		)
		if account == nil {
			// 选号失败只有在「候选集合全部因 Antigravity 配额硬门禁被阻断」时才改答 429
			// （D8=A，Spec §6.2 #3）：其余无号成因（选号超时、队列准入被拒、上游 404、
			// 不可重试传输错误）一律保持既有 200 + 本地估算回退。
			if writeCountTokensQuotaGateError(c, selectionErr) {
				return
			}
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolOpenAI) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
			h.store.Release(account)
			return
		}

		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		resp, reqErr := ExecuteAntigravityGeminiCountTokensRequest(upstreamCtx, account, model, rawBody, proxyURL)
		upstreamCancel()
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestError(reqErr)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBodyBytes))
			_ = resp.Body.Close()
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusNotFound {
				h.store.Release(account)
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				if refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account); refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			// countTokens 与 generateContent 打到的是同一个 Cloud Code 配额池：漏掉
			// 这里就等于 429/503 只被计数、从不写模型冷却，被上游限流的账号会立刻在
			// 下一次选号中重新入选（wireModel 联动与 family 冷却也一并丢失）。
			h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, model)
			log.Printf("Gemini native countTokens upstream error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			if shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					continue
				}
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		out, readErr := io.ReadAll(io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		h.store.Release(account)
		if readErr != nil {
			ErrorToGinResponse(c, readErr)
			return
		}
		c.Data(http.StatusOK, "application/json", out)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       durationMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}

// writeCountTokensQuotaGateError 把 native countTokens 的选号失败中「候选集合全部因
// Antigravity 配额硬门禁被阻断」这一种成因转成 429 + Retry-After + reason，返回 true
// 表示响应已写出（Spec §6.2 #3、验收 #25）。
//
// 其余成因（选号超时、队列准入被拒、上游 404、不可重试传输错误）返回 false：调用方保持
// 既有 200 + estimateInputTokens 估算回退，countTokens 是客户端计费探针，D8=A 只收紧
// 「整池配额阻断」这一个分支，绝不把估算兜底整体换成错误。
func writeCountTokensQuotaGateError(c *gin.Context, selectionErr error) bool {
	retryAfterSeconds, reason, ok := schedulerQuotaExhaustion(selectionErr)
	if !ok {
		return false
	}
	writeSchedulerQuotaExhaustedError(c, retryAfterSeconds, reason)
	return true
}

func geminiInboundEndpoint(stream bool) string {
	if stream {
		return "/v1beta/models:streamGenerateContent"
	}
	return "/v1beta/models:generateContent"
}

type geminiNativeStreamResult struct {
	usage        UsageInfo
	firstTokenMs int
	readErr      error
	writeErr     error
}

// forwardGeminiNativeStream keeps reads and downstream writes distinguishable
// for usage logging. Emit complete SSE events and flush each one so a thought
// does not sit in the HTTP buffer while the upstream prepares its answer.
func forwardGeminiNativeStream(c *gin.Context, body io.Reader, startedAt time.Time) geminiNativeStreamResult {
	return forwardGeminiNativeStreamGuarded(c, body, startedAt, nil, nil, nil)
}

// forwardGeminiNativeStreamGuarded 在逐帧写出之前执行 Gemini 投影与 Guard 判定。
// guard 为 nil 时保持原有转发语义，供不关心 Guard 的既有调用方复用。
func forwardGeminiNativeStreamGuarded(c *gin.Context, body io.Reader, startedAt time.Time, guard *outputguard.RequestGuard, coord *GuardTerminationCoordinator, projector ProtocolProjector) geminiNativeStreamResult {
	var result geminiNativeStreamResult
	var usage geminiNativeUsage
	activateContinuousRetryKeepalive(c.Request.Context())
	result.readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), body, func(_ string, data []byte) bool {
		if c.Request.Context().Err() != nil {
			return false
		}
		// [OutputGuard 前置检查] 逐 candidate parts 追加检测；绝不能因为某个
		// candidate 带 finishReason 就把整个流误判为 Snapshot 而跳过逐帧判定。
		if guard != nil {
			if err := ObserveEvent(guard, projector, "", data); err != nil || guard.IsTripped() {
				reason := guard.TrippedReason()
				_ = coord.HandleTripped(reason)
				if c.Writer.Written() {
					_ = SendDownstreamGuardTerminalHTTP(c, nil, nil, "gemini", reason)
				} else {
					// Gemini SDK 不解析 SSE 错误；未提交响应头时直接给出裸 JSON 502。
					writeGeminiNativeError(c, http.StatusBadGateway, GuardTriggeredPublicMessage)
				}
				result.readErr = errModelOutputGuardTriggered
				return false
			}
		}
		usage.observe(data)
		if result.firstTokenMs == 0 && !startedAt.IsZero() && geminiNativeHasContent(data) {
			result.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
		}
		frame := "data: " + string(data) + "\n\n"
		var written int
		written, result.writeErr = c.Writer.WriteString(frame)
		if result.writeErr == nil && written != len(frame) {
			result.writeErr = io.ErrShortWrite
		}
		if result.writeErr != nil {
			return false
		}
		c.Writer.Flush()
		return true
	})
	result.usage = usage.usageInfo()
	return result
}

// writeGeminiNativeError uses the same native error envelope before and after
// SSE headers are committed. Once streaming has started, the HTTP status cannot
// change. Google SDKs expect a bare JSON error at the end of the stream; an SSE
// data error is silently discarded by the SDK bundled with Gemini CLI 0.61.0.
func writeGeminiNativeError(c *gin.Context, status int, message string) {
	statusName := "INTERNAL"
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		statusName = "UNAVAILABLE"
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		statusName = "DEADLINE_EXCEEDED"
	case http.StatusTooManyRequests:
		statusName = "RESOURCE_EXHAUSTED"
	case http.StatusBadRequest:
		statusName = "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		statusName = "UNAUTHENTICATED"
	case http.StatusForbidden:
		statusName = "PERMISSION_DENIED"
	case http.StatusNotFound:
		statusName = "NOT_FOUND"
	}
	payload := gin.H{"error": gin.H{"code": status, "message": message, "status": statusName}}
	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(status, payload)
		return
	}
	if !strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		return
	}
	data, _ := json.Marshal(payload)
	if _, err := c.Writer.Write(data); err == nil {
		c.Writer.Flush()
	}
}

// geminiRequestProjector 按入站方法显式选择 Gemini 投影模式。
// 绝不能沿用 Auto 模式从响应体推测流式性：generateContent 是单包快照，若 body 不含
// finishReason，Auto 会把它误判为流式而退化为 Append，跳过全文快照与尾检，
// 使 8KiB~9214B 的重复尾逃逸；streamGenerateContent 则必须始终按逐帧流式处理。
func geminiRequestProjector(stream bool) *GeminiProjector {
	if stream {
		return NewGeminiProjectorWithMode(GeminiProjectorModeStream)
	}
	return NewGeminiProjectorWithMode(GeminiProjectorModeNonStream)
}

// handleGeminiGenerateContent 处理 native Gemini generateContent /
// streamGenerateContent 请求：候选池被硬限定为 Antigravity OAuth 并叠加跨实例冷却、
// 渠道与预算过滤，按 stream 决定是否安装 SSE 心跳，并在持续重试策略下带重试排除项
// 与亲和守卫选号转发上游。
func (h *Handler) handleGeminiGenerateContent(c *gin.Context, model string, rawBody []byte, stream bool) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	// [OutputGuard 状态机初始化] attempt 循环外单次初始化，跨 attempt 继承事件
	// 与语义字节预算；Gemini 流式与非流式共用同一个请求级状态机。
	requestGuard := outputguard.NewRequestGuard(nativeGuardCoreConfig())
	geminiProjector := geminiRequestProjector(stream)

	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolGemini)
	defer stopRetryDeadline()
	// Gemini CLI's bundled Google SDK expects data events and cannot skip SSE
	// comments. An empty response keeps the connection alive without content.
	stopRetryKeepalive := installContinuousRetrySSEKeepaliveWithOptions(c, stream, continuousRetrySSEKeepaliveOptions{
		contentType: "text/event-stream; charset=utf-8",
		payload:     "data: {}\n\n",
	})
	defer stopRetryKeepalive()
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		activateContinuousRetryKeepalive(c.Request.Context())
	}

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	selectionOpts := geminiNativeSelectionOptions(c, model, dispatchPolicy)
	inboundEndpoint := geminiInboundEndpoint(stream)
	upstreamEndpoint := antigravityUpstreamEndpoint(stream)

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts,
		)
		if account == nil {
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolGemini) {
				return
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				setConcurrencySaturatedRetryAfter(c)
				c.JSON(http.StatusServiceUnavailable, concurrencySaturatedError())
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(model))
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return
			}
		}

		// Native Gemini forwards the stream directly and does not drain usage
		// after a disconnect. Keep this context alive through body consumption,
		// while letting a canceled client stop a blocked upstream read immediately.
		upstreamCtx, upstreamCancel := context.WithCancel(c.Request.Context())
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteAntigravityGeminiRequest(upstreamCtx, account, model, rawBody, stream, proxyURL)
		})
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			upstreamCancel()
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				if claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) && c.Request.Context().Err() == nil {
					writeGeminiNativeError(c, StatusCodeFromError(reqErr), "Upstream Gemini request failed")
				}
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBodyBytes))
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			_ = resp.Body.Close()
			upstreamCancel()
			if continuousRetryCommitExpired(c, continuousRetryProtocolGemini) {
				h.store.Release(account)
				return
			}
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account)
				if refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					log.Printf("Antigravity OAuth token refreshed after upstream 401 (account=%d, endpoint=%s)", account.ID(), inboundEndpoint)
					continue
				}
				log.Printf("Antigravity OAuth refresh failed after upstream 401 (account=%d, endpoint=%s): %v", account.ID(), inboundEndpoint, refreshErr)
			}

			antigravityRefreshFailed := resp.StatusCode == http.StatusUnauthorized && antigravityRefreshRetried[account.ID()]
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" && !antigravityRefreshFailed && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("Gemini native upstream error (attempt %d, status %d, %s): %s", attempt+1, resp.StatusCode, inboundEndpoint, upstreamErrorConsoleBody(errBody))
			logUpstreamError(inboundEndpoint, resp.StatusCode, model, account.ID(), errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, model)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            stream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) {
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// [OutputGuard] 本次 attempt 的终止协调器：命中时先取消上游并关闭 body，
		// 再丢弃任何尚未写出的下游缓冲。
		guardCoord := NewGuardTerminationCoordinator(requestGuard, upstreamCancel, resp.Body)

		if stream {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			c.Status(resp.StatusCode)
			result := forwardGeminiNativeStreamGuarded(c, resp.Body, start, requestGuard, guardCoord, geminiProjector)
			_ = resp.Body.Close()
			upstreamCancel()
			h.store.Release(account)
			durationMs = int(time.Since(start).Milliseconds())
			ctxErr := continuousRetryContextError(c.Request.Context())
			// The native reader only returns a clean EOF after a real Gemini
			// terminal. A successful upstream read still requires delivery to the
			// downstream client before this request can be recorded as successful.
			completed := result.readErr == nil && result.writeErr == nil && ctxErr == nil
			outcome := classifyStreamOutcome(ctxErr, result.readErr, result.writeErr, completed)
			var upstreamStreamErr *antigravityStreamError
			if ctxErr == nil && result.writeErr == nil && errors.As(result.readErr, &upstreamStreamErr) {
				outcome = h.antigravityStreamErrorOutcome(account, upstreamStreamErr, resp, model)
			}
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
			}
			// Guard 终态绝对覆盖 499/598/continuation 分类，绝不允许断流分类遮蔽。
			outcome = OverlayGuardOutcome(outcome, requestGuard)
			if outcome.penalize {
				h.reportStreamOutcomeFailure(account, outcome, time.Since(start))
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if outcome.logStatusCode != http.StatusOK && !outcome.guardTripped {
				if !writeContinuousRetryTimeoutResponse(c, continuousRetryProtocolGemini) && ctxErr == nil && result.writeErr == nil {
					status, message := http.StatusBadGateway, "Upstream Gemini stream failed before completion"
					if upstreamStreamErr != nil {
						// Gemini clients retry RESOURCE_EXHAUSTED/UNAVAILABLE themselves,
						// so keep the upstream status instead of a generic 502.
						status = upstreamStreamErr.StatusCode
						if text := strings.TrimSpace(gjson.GetBytes(upstreamStreamErr.Body, "error.message").String()); text != "" {
							message = text
						}
					}
					writeGeminiNativeError(c, status, message)
				}
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        outcome.logStatusCode,
				DurationMs:        durationMs,
				FirstTokenMs:      result.firstTokenMs,
				PromptTokens:      result.usage.PromptTokens,
				CompletionTokens:  result.usage.CompletionTokens,
				InputTokens:       result.usage.InputTokens,
				OutputTokens:      result.usage.OutputTokens,
				ReasoningTokens:   result.usage.ReasoningTokens,
				CachedTokens:      result.usage.CachedTokens,
				TotalTokens:       result.usage.TotalTokens,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            true,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: outcome.failureKind,
				ErrorMessage:      usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			})
			return
		}

		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		upstreamCancel()
		if readErr != nil {
			h.store.ReportRequestFailure(account, "upstream_read_error", time.Duration(durationMs)*time.Millisecond)
			h.store.Release(account)
			if claimContinuousRetryTerminal(c, continuousRetryProtocolGemini) && c.Request.Context().Err() == nil {
				writeGeminiNativeError(c, http.StatusBadGateway, "Failed to read upstream Gemini response")
			}
			return
		}
		// [OutputGuard 全量 Snapshot 检查] 保留现有 8MiB 有界读取，但在任何 JSON
		// 写入下游之前对完整响应执行一次 Snapshot 判定。
		if err := ObserveEvent(requestGuard, geminiProjector, "", out); err != nil || requestGuard.IsTripped() {
			reason := requestGuard.TrippedReason()
			guardOutcome := guardCoord.HandleTripped(reason)
			h.store.Release(account)
			writeGeminiNativeError(c, http.StatusBadGateway, GuardTriggeredPublicMessage)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        guardOutcome.logStatusCode,
				DurationMs:        int(time.Since(start).Milliseconds()),
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            false,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: guardOutcome.failureKind,
				ErrorMessage:      usageLogFailureMessage(guardOutcome.logStatusCode, guardOutcome.failureMessage),
			})
			return
		}
		usage := geminiNativeUsageFromBody(out)
		if !claimContinuousRetrySuccess(c, continuousRetryProtocolGemini) {
			h.store.Release(account)
			return
		}
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Status(resp.StatusCode)
		_, _ = c.Writer.Write(out)
		h.store.Release(account)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       int(time.Since(start).Milliseconds()),
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			InputTokens:      usage.InputTokens,
			OutputTokens:     usage.OutputTokens,
			ReasoningTokens:  usage.ReasoningTokens,
			CachedTokens:     usage.CachedTokens,
			TotalTokens:      usage.TotalTokens,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			Stream:           false,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}
