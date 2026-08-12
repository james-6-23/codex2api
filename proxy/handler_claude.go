package proxy

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ==================== /v1/messages → Claude Code 原生上游 ====================
//
// 与 Codex 路径的根本差别：不翻译。下游 /v1/messages 的请求体与上游 Anthropic
// Messages API 形态一致，响应也一致，因此整条链路是字节级透传。走一趟
// Anthropic→Codex→Anthropic 往返只会丢字段（thinking 签名、cache_control、
// tool_use 的分片 JSON 等），且把 Claude 独有能力压成与 Codex 的交集。

// claudeChannelAccountFilter 是 Claude 渠道的账号过滤器：仅 Claude 账号；
// 账号声明了 models 白名单则要求命中，未声明则按默认 Claude 模型集放行。
func claudeChannelAccountFilter(model string) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil || !account.IsClaudeAPI() {
			return false
		}
		routedModel := model
		if mappedModel, ok := resolveAccountModelMapping(account, model); ok && mappedModel != "" {
			routedModel = mappedModel
		}
		if routedModel != "" && account.IsModelRateLimited(routedModel) {
			return false
		}
		if len(account.ClaudeModels()) > 0 {
			return account.ClaudeChannelSupportsModel(routedModel)
		}
		return routedModel == "" || modelIDInList(routedModel, DefaultClaudeModelIDs())
	}
}

// hasClaudeCandidate 判断池子里是否存在能承接该模型的 Claude 账号。
// Messages 用它决定走原生透传还是既有的 Codex 翻译路径——池里没有 Claude 账号时，
// 行为与本特性引入前完全一致。
func (h *Handler) hasClaudeCandidate(model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	filter := claudeChannelAccountFilter(model)
	for _, acc := range h.store.Accounts() {
		if filter(acc) {
			return true
		}
	}
	return false
}

// claudeStreamUsage 累积 Anthropic SSE 里的分片用量。
// input/cache 计数只在 message_start 出现一次，output_tokens 在 message_delta
// 里逐步刷新（取最后一次，不是累加）。
type claudeStreamUsage struct {
	input       int
	output      int
	cacheRead   int
	cacheCreate int
	seen        bool
}

func (u *claudeStreamUsage) observe(eventData string) {
	parsed := gjson.Parse(eventData)
	switch parsed.Get("type").String() {
	case "message_start":
		usage := parsed.Get("message.usage")
		if !usage.Exists() {
			return
		}
		u.seen = true
		u.input = int(usage.Get("input_tokens").Int())
		u.cacheRead = int(usage.Get("cache_read_input_tokens").Int())
		u.cacheCreate = int(usage.Get("cache_creation_input_tokens").Int())
		if out := usage.Get("output_tokens"); out.Exists() {
			u.output = int(out.Int())
		}
	case "message_delta":
		usage := parsed.Get("usage")
		if !usage.Exists() {
			return
		}
		u.seen = true
		if out := usage.Get("output_tokens"); out.Exists() {
			u.output = int(out.Int())
		}
		// 少数情况下 message_delta 也会回带输入侧计数，取非零值覆盖。
		if in := usage.Get("input_tokens"); in.Exists() && in.Int() > 0 {
			u.input = int(in.Int())
		}
	}
}

func (u *claudeStreamUsage) toUsageInfo() *UsageInfo {
	if !u.seen {
		return nil
	}
	// cache 读写都计入输入总量，命中部分再单独标记为 cached_tokens，与 Codex 侧口径一致。
	return newUsageInfo(u.input+u.cacheRead+u.cacheCreate, u.output, 0, u.cacheRead)
}

// claudeUsageFromJSONBody 解析非流式响应体的 usage。
func claudeUsageFromJSONBody(body []byte) *UsageInfo {
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return nil
	}
	input := int(usage.Get("input_tokens").Int())
	cacheRead := int(usage.Get("cache_read_input_tokens").Int())
	cacheCreate := int(usage.Get("cache_creation_input_tokens").Int())
	output := int(usage.Get("output_tokens").Int())
	return newUsageInfo(input+cacheRead+cacheCreate, output, 0, cacheRead)
}

// messagesViaClaudeUpstream 是 /v1/messages 的原生透传路径。
// 返回 true 表示请求已在本路径完结（响应已写出或错误已返回）。
func (h *Handler) messagesViaClaudeUpstream(c *gin.Context, rawBody []byte, model string, isStream bool) bool {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return true
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return true
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := claudeChannelAccountFilter(model)
	accountFilter = h.withModelCooldownFilter(model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	retryExclusions := newRetryAccountExclusions()
	var lastStatusCode int

	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests {
				sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", "All Claude accounts rate limited")
				return true
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				sendAnthropicError(c, http.StatusTooManyRequests, "rate_limit_error", msg)
				return true
			}
			sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", noAvailableAnthropicAccountMessage(model))
			return true
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

		upstreamBody := rawBody
		attemptModel := model
		if mappedBody, mappedModel, mapped := h.applyAccountModelMappingToBody(upstreamBody, account); mapped {
			upstreamBody = mappedBody
			attemptModel = mappedModel
		}

		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)

		resp, reqErr := ExecuteClaudeRequest(upstreamCtx, account, upstreamBody, proxyURL, c.Request.Header.Clone())
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := retryable && shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			if shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("Claude 上游首字超时，换号重试 (attempt %d/%d, account %d): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut {
				retryExclusions.MarkHard(account.ID())
			}
			log.Printf("Claude 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				continue
			}
			sendAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream request failed")
			return true
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("Claude 上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, string(errBody))
			logUpstreamError("/v1/messages", resp.StatusCode, model, account.ID(), errBody)
			decision := applyClaudeCooldown(h.store, account, resp.StatusCode, errBody, resp, attemptModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          "/v1/messages",
				Model:             model,
				EffectiveModel:    attemptModel,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				InboundEndpoint:   "/v1/messages",
				UpstreamEndpoint:  "/v1/messages",
				Stream:            isStream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				continue
			}
			// 上游 401/403 是账号侧问题，不是下游客户端凭证无效。原样透传会让
			// Claude Code 误判自己的 key 失效并停工（issue #323 / #396），改写为 503。
			if resp.StatusCode == http.StatusUnauthorized {
				sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", "账号池暂无可用账号（上游账号鉴权失效），请稍后重试")
				return true
			}
			if resp.StatusCode == http.StatusForbidden {
				sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", "账号池暂无可用账号（上游账号被拒绝访问：额度/套餐或工作区受限），请稍后重试")
				return true
			}
			errType := mapHTTPStatusToAnthropicError(resp.StatusCode)
			msg := gjson.GetBytes(errBody, "error.message").String()
			if msg == "" {
				msg = "Upstream returned status " + http.StatusText(resp.StatusCode)
			}
			sendAnthropicError(c, resp.StatusCode, errType, msg)
			return true
		}

		// ========== 成功路径：字节级透传 ==========
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", attemptModel)

		usage, firstTokenMs, streamErr := h.relayClaudeResponse(c, resp, isStream, start, ttftGuard)
		resp.Body.Close()
		ttftGuard.Stop()
		h.store.Release(account)
		durationMs = int(time.Since(start).Milliseconds())

		statusCode := http.StatusOK
		if streamErr != nil {
			// 正文已下发，无法整段重试：只把失败如实记进用量日志。
			log.Printf("Claude 响应转发中断 (account %d): %v", account.ID(), streamErr)
			statusCode = http.StatusBadGateway
		}
		usageLog := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         "/v1/messages",
			Model:            model,
			EffectiveModel:   attemptModel,
			StatusCode:       statusCode,
			DurationMs:       durationMs,
			FirstTokenMs:     firstTokenMs,
			InboundEndpoint:  "/v1/messages",
			UpstreamEndpoint: "/v1/messages",
			Stream:           isStream,
			AttemptIndex:     attempt + 1,
		}
		if usage != nil {
			usageLog.PromptTokens = usage.PromptTokens
			usageLog.CompletionTokens = usage.CompletionTokens
			usageLog.TotalTokens = usage.TotalTokens
			usageLog.InputTokens = usage.InputTokens
			usageLog.OutputTokens = usage.OutputTokens
			usageLog.ReasoningTokens = usage.ReasoningTokens
			usageLog.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, usageLog)
		return true
	}
}

// relayClaudeResponse 把上游响应原样转给下游，顺带取用量与首字时延。
func (h *Handler) relayClaudeResponse(c *gin.Context, resp *http.Response, isStream bool, start time.Time, ttftGuard *firstTokenTimeoutGuard) (*UsageInfo, int, error) {
	if !isStream {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, 0, err
		}
		ttftGuard.MarkProgress("message")
		c.Header("Content-Type", "application/json")
		c.Status(http.StatusOK)
		if _, err := c.Writer.Write(body); err != nil {
			return claudeUsageFromJSONBody(body), 0, err
		}
		return claudeUsageFromJSONBody(body), int(time.Since(start).Milliseconds()), nil
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		sendAnthropicError(c, http.StatusInternalServerError, "api_error", "Streaming not supported")
		return nil, 0, nil
	}
	streamWriter := h.newStreamFlushWriter(c, c.Writer, flusher)

	usage := &claudeStreamUsage{}
	firstTokenMs := 0
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if data, isData := strings.CutPrefix(strings.TrimRight(string(line), "\r\n"), "data: "); isData {
				usage.observe(data)
				eventType := gjson.Get(data, "type").String()
				ttftGuard.MarkProgress(eventType)
				if firstTokenMs == 0 && eventType == "content_block_delta" {
					firstTokenMs = int(time.Since(start).Milliseconds())
				}
			}
			if writeErr := streamWriter.WriteString(string(line)); writeErr != nil {
				return usage.toUsageInfo(), firstTokenMs, writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			// 上游断流：正文已在路上，按 Anthropic 协议补一个流内 error 事件，
			// 下游网关/客户端能识别并自行重试（与 issue #435 同一处理口径）。
			if writeErr := writeAnthropicStreamErrorEvent(streamWriter, "overloaded_error", "Upstream stream interrupted before completion (upstream_stream_break)"); writeErr != nil {
				log.Printf("写入流内 error 事件失败 (/v1/messages, claude): %v", writeErr)
			}
			_ = streamWriter.Flush()
			return usage.toUsageInfo(), firstTokenMs, err
		}
	}
	if err := streamWriter.Flush(); err != nil {
		return usage.toUsageInfo(), firstTokenMs, err
	}
	return usage.toUsageInfo(), firstTokenMs, nil
}
