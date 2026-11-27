package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openCodeSessionHeader = "X-OpenCode-Session"

	// openCodeFrequencyLimitQuotaGuardPercent：Go 账号快照中任一窗口达到该比例后，
	// 429 一律按窗口耗尽冷却到 resets_at，不再降级为短频控冷却。
	openCodeFrequencyLimitQuotaGuardPercent = 90
)

// OpenCodeUpstreamRoute 描述一次 OpenCode 上游请求的解析结果。
type OpenCodeUpstreamRoute struct {
	Model          string
	Protocol       GrokProtocol
	NativeProtocol string
	Endpoint       string
	BaseURL        string
	Native         bool
}

func openCodeProtocolToGrok(proto string) GrokProtocol {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case auth.OpenCodeProtocolResponses:
		return GrokProtocolResponses
	case auth.OpenCodeProtocolAnthropic:
		return GrokProtocolMessages
	default:
		return GrokProtocolChatCompletions
	}
}

func grokProtocolToOpenCode(proto GrokProtocol) string {
	switch auth.NormalizeGrokProtocol(string(proto)) {
	case GrokProtocolResponses:
		return auth.OpenCodeProtocolResponses
	case GrokProtocolMessages:
		return auth.OpenCodeProtocolAnthropic
	case GrokProtocolChatCompletions:
		return auth.OpenCodeProtocolChatCompletions
	default:
		return ""
	}
}

// OpenCodeAccountModelIDs 返回单个 OpenCode 账号对外暴露并可调度的模型列表。
// 优先级：账号显式 Models 白名单 > 上游已同步目录 > Go 模式内置公开目录。
// 所有来源均剔除 gemini-* / jev-* 专有端点模型。
func OpenCodeAccountModelIDs(account *auth.Account) []string {
	if account == nil || !account.IsOpenCode() {
		return nil
	}
	cfg := account.OpenCodeConfig()
	filter := func(in []string) []string {
		out := make([]string, 0, len(in))
		seen := make(map[string]struct{}, len(in))
		for _, raw := range in {
			id := auth.StripOpenCodeModelPrefix(raw)
			if id == "" || auth.IsOpenCodeUnsupportedModel(id) {
				continue
			}
			key := strings.ToLower(id)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, id)
		}
		return out
	}
	if len(cfg.Models) > 0 {
		return filter(cfg.Models)
	}
	if len(cfg.CatalogModels) > 0 {
		return filter(cfg.CatalogModels)
	}
	return filter(auth.DefaultOpenCodeGoModelIDs())
}

// openCodeAccountSupportsModel 判断 OpenCode 账号能否服务指定模型。
func openCodeAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil || !account.IsOpenCode() {
		return false
	}
	stripped := auth.StripOpenCodeModelPrefix(model)
	if stripped == "" || auth.IsOpenCodeUnsupportedModel(stripped) {
		return false
	}
	cfg := account.OpenCodeConfig()
	if len(cfg.Models) > 0 {
		return modelIDInList(stripped, cfg.Models) || modelIDInList(strings.TrimSpace(model), cfg.Models)
	}
	if len(cfg.CatalogModels) > 0 {
		return modelIDInList(stripped, cfg.CatalogModels)
	}
	// Zen 模式尚未同步上游目录时，允许常见非受限模型进入探测/调用，避免刚添加账号即被 503 拦死。
	if cfg.Mode == auth.OpenCodeModeZen {
		return true
	}
	return modelIDInList(stripped, auth.DefaultOpenCodeGoModelIDs())
}

func openCodeChannelAccountFilterWithGate(model string, gate modelRateLimitGate) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil || !account.IsOpenCode() {
			return false
		}
		routedModel := model
		if mappedModel, ok := resolveAccountModelMapping(account, model); ok && mappedModel != "" {
			routedModel = mappedModel
		}
		if gate.blocks(account, routedModel) {
			return false
		}
		return openCodeAccountSupportsModel(account, routedModel)
	}
}

// ResolveOpenCodeUpstreamRoute 根据账号模式、自定义 protocol_rules、模型名与入站协议，
// 确定发往 OpenCode 的上游协议、模型名与最终 URL。
func ResolveOpenCodeUpstreamRoute(account *auth.Account, model string, inbound GrokProtocol) OpenCodeUpstreamRoute {
	cfg := account.OpenCodeConfig()
	wireModel := auth.StripOpenCodeModelPrefix(model)
	inboundOpenCode := grokProtocolToOpenCode(inbound)
	selected := auth.ResolveOpenCodeProtocol(wireModel, inboundOpenCode, cfg.Mode, cfg.ProtocolRules, cfg.HasCustomRules)
	targetProto := openCodeProtocolToGrok(selected)

	baseURL := cfg.BaseURL
	var endpoint string
	switch selected {
	case auth.OpenCodeProtocolResponses:
		endpoint = auth.OpenCodeEndpointURL(baseURL, "/v1/responses")
	case auth.OpenCodeProtocolAnthropic:
		baseURL = cfg.AnthropicBaseURL
		endpoint = auth.OpenCodeEndpointURL(baseURL, "/v1/messages")
	default:
		endpoint = auth.OpenCodeEndpointURL(baseURL, "/v1/chat/completions")
	}

	normalizedInbound := auth.NormalizeGrokProtocol(string(inbound))
	return OpenCodeUpstreamRoute{
		Model:          wireModel,
		Protocol:       targetProto,
		NativeProtocol: selected,
		Endpoint:       endpoint,
		BaseURL:        baseURL,
		Native:         normalizedInbound != "" && targetProto == normalizedInbound,
	}
}

func sanitizeOpenCodeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 || strings.ContainsAny(raw, "\r\n") {
		return ""
	}
	return raw
}

func openCodeSessionIDFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if v := sanitizeOpenCodeSessionID(gjson.GetBytes(body, "prompt_cache_key").String()); v != "" {
		return v
	}
	if v := sanitizeOpenCodeSessionID(gjson.GetBytes(body, "session_id").String()); v != "" {
		return v
	}
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID != "" {
		if strings.HasPrefix(userID, "{") {
			if sid := sanitizeOpenCodeSessionID(gjson.Get(userID, "session_id").String()); sid != "" {
				return sid
			}
		}
		if sid := sanitizeOpenCodeSessionID(userID); sid != "" {
			return sid
		}
	}
	return ""
}

func isOfficialOpenCodeURL(targetURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "opencode.ai")
}

// resolveOpenCodeSessionID 解析出站 X-OpenCode-Session（OpenCode Go 必填会话标识，用于前缀缓存与风控契约）。
func resolveOpenCodeSessionID(incoming http.Header, cfg auth.OpenCodeAccountConfig, targetURL string, bodies ...[]byte) string {
	if incoming != nil {
		for _, key := range []string{
			openCodeSessionHeader,
			"X-Session-Affinity",
			"Session-Id",
			"Session_id",
			"Conversation-Id",
			"Conversation_id",
			"X-Session-ID",
		} {
			if v := sanitizeOpenCodeSessionID(incoming.Get(key)); v != "" {
				return v
			}
		}
	}
	for _, body := range bodies {
		if v := openCodeSessionIDFromBody(body); v != "" {
			return v
		}
	}
	for _, body := range bodies {
		if seed := sanitizeOpenCodeSessionID(deriveContentSessionSeed(body)); seed != "" {
			return seed
		}
	}
	for key, value := range cfg.CustomHeaders {
		if strings.EqualFold(strings.TrimSpace(key), openCodeSessionHeader) {
			if v := sanitizeOpenCodeSessionID(value); v != "" {
				return v
			}
		}
	}
	if cfg.Mode == auth.OpenCodeModeGo || (isOfficialOpenCodeURL(targetURL) && strings.Contains(strings.ToLower(targetURL), "/zen/go")) {
		return uuid.NewString()
	}
	return ""
}

func prepareOpenCodeOutboundBody(route OpenCodeUpstreamRoute, inbound GrokProtocol, inboundBody, responsesBody []byte) ([]byte, bool, error) {
	normalizedInbound := auth.NormalizeGrokProtocol(string(inbound))
	// 同协议直通：保留客户端原始结构（含 stream=false），仅改写模型名。
	if route.Native && normalizedInbound != "" && len(inboundBody) > 0 {
		body, err := rewriteGrokProtocolModel(inboundBody, route.Model)
		if err != nil {
			return nil, false, err
		}
		return body, grokProtocolBodyStream(body), nil
	}

	// 跨协议转换：先归一到规范 Responses 体，再转换到目标协议并强制 stream=true 供下游投影。
	var (
		canonical []byte
		err       error
	)
	switch normalizedInbound {
	case GrokProtocolChatCompletions:
		if len(inboundBody) > 0 {
			canonical, err = TranslateChatToResponsesForGrok(inboundBody)
		}
	case GrokProtocolMessages:
		if len(inboundBody) > 0 {
			canonical, _, err = TranslateAnthropicToResponsesForGrok(inboundBody, "", nil)
		}
	default:
		canonical = responsesBody
	}
	if err != nil {
		return nil, false, err
	}
	if len(canonical) == 0 {
		canonical = responsesBody
	}
	if len(canonical) == 0 {
		canonical = inboundBody
	}
	canonical, err = rewriteGrokProtocolModel(canonical, route.Model)
	if err != nil {
		return nil, false, err
	}

	var outbound []byte
	switch route.Protocol {
	case GrokProtocolChatCompletions:
		outbound, err = convertResponsesToChatRequest(canonical)
	case GrokProtocolMessages:
		outbound, err = convertResponsesToMessagesRequest(canonical)
	default:
		outbound = canonical
	}
	if err != nil {
		return nil, false, err
	}
	outbound, err = rewriteGrokProtocolModel(outbound, route.Model)
	if err != nil {
		return nil, false, err
	}
	if !grokProtocolBodyStream(outbound) {
		outbound, err = sjson.SetBytes(outbound, "stream", true)
		if err != nil {
			return nil, false, err
		}
		if route.Protocol == GrokProtocolChatCompletions {
			if withUsage, usageErr := sjson.SetBytes(outbound, "stream_options.include_usage", true); usageErr == nil {
				outbound = withUsage
			}
		}
	}
	return outbound, true, nil
}

func applyOpenCodeRequestHeaders(req *http.Request, route OpenCodeUpstreamRoute, cfg auth.OpenCodeAccountConfig, incoming http.Header, stream bool, bodies ...[]byte) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Accept-Encoding", "gzip, br, deflate")
	req.Header.Set("User-Agent", auth.OpenCodeUserAgent())

	if route.Protocol == GrokProtocolMessages {
		version := "2023-06-01"
		if incoming != nil {
			if v := strings.TrimSpace(incoming.Get("anthropic-version")); v != "" && !strings.ContainsAny(v, "\r\n") {
				version = v
			}
			if beta := strings.TrimSpace(incoming.Get("anthropic-beta")); beta != "" && !strings.ContainsAny(beta, "\r\n") {
				req.Header.Set("anthropic-beta", beta)
			}
		}
		req.Header.Set("anthropic-version", version)
	}

	// 应用账号自定义头（跳过认证/传输保留头，但允许管理员显式覆盖 User-Agent 等非保留头）。
	for name, value := range cfg.CustomHeaders {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || auth.IsOpenCodeReservedHeader(trimmed) {
			continue
		}
		if strings.ContainsAny(trimmed, "\r\n") || strings.ContainsAny(value, "\r\n") {
			continue
		}
		req.Header.Set(trimmed, value)
	}

	// 网关拥有认证头，最后写入以防覆盖。
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	if route.Protocol == GrokProtocolMessages {
		req.Header.Set("x-api-key", cfg.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	if sessionID := resolveOpenCodeSessionID(incoming, cfg, route.Endpoint, bodies...); sessionID != "" {
		req.Header.Set(openCodeSessionHeader, sessionID)
	}
}

// ExecuteOpenCodeProtocolRequest 是 OpenCode 渠道统一协议执行入口：
//   - 按模型规则分流到 Responses / Chat Completions / Anthropic Messages；
//   - 同协议直通打标复用原生透传链路；
//   - 跨协议自动转换并将上游 SSE 投影回规范 Responses SSE。
func ExecuteOpenCodeProtocolRequest(ctx context.Context, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	resetUpstreamAttemptTrace(ctx)
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || !account.IsOpenCode() {
		return nil, ErrNoAvailableAccount()
	}
	cfg := account.OpenCodeConfig()
	if cfg.APIKey == "" {
		return nil, ErrNoAvailableAccount()
	}
	model := strings.TrimSpace(gjson.GetBytes(responsesBody, "model").String())
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(inboundBody, "model").String())
	}
	if auth.IsOpenCodeUnsupportedModel(model) {
		return nil, ErrBadRequest(fmt.Sprintf("Model '%s' is not supported on OpenCode standard gateway (gemini models require Google SDK endpoint, jev models require System One endpoint)", model))
	}

	route := ResolveOpenCodeUpstreamRoute(account, model, inbound)
	if route.Endpoint == "" {
		return nil, ErrNoAvailableAccount()
	}
	outboundBody, upstreamStream, err := prepareOpenCodeOutboundBody(route, inbound, inboundBody, responsesBody)
	if err != nil {
		return nil, ErrBadRequest("OpenCode protocol conversion failed: " + err.Error())
	}

	proxyURL := cfg.ProxyURL
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = strings.TrimSpace(proxyOverride)
	}
	quotaModel := model
	if quotaModel == "" {
		quotaModel = route.Model
	}
	if err := ConsumeAPIKeyModelRequestQuota(ctx, quotaModel); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, route.Endpoint, bytes.NewReader(outboundBody))
	if err != nil {
		return nil, ErrInternalError("创建 OpenCode 上游请求失败", err)
	}
	applyOpenCodeRequestHeaders(req, route, cfg, headers, upstreamStream, inboundBody, responsesBody, outboundBody)

	resp, doErr := doTracedUpstreamRequest(getPooledClient(account, proxyURL), req, account, proxyURL)
	if doErr != nil {
		if shouldRecyclePooledClient(doErr) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 OpenCode 上游失败", doErr)
	}
	decodeGrokResponseEncoding(resp)

	if route.Native && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		resp.Header.Set(grokNativeRouteHeader, "1")
	} else {
		adaptGrokProtocolResponse(resp, route.Protocol, route.Model, upstreamStream)
	}
	return resp, nil
}

// OpenCodeConnectionTestBody 构造用于管理台连通性测试的最小请求体。
func OpenCodeConnectionTestBody(account *auth.Account, model, testContent string) (GrokProtocol, []byte) {
	route := ResolveOpenCodeUpstreamRoute(account, model, "")
	prompt := auth.NormalizeTestContent(testContent)
	switch route.Protocol {
	case GrokProtocolResponses:
		body := []byte(`{"model":"","input":"","max_output_tokens":16,"store":false,"stream":false}`)
		body, _ = sjson.SetBytes(body, "model", route.Model)
		body, _ = sjson.SetBytes(body, "input", prompt)
		return GrokProtocolResponses, body
	case GrokProtocolMessages:
		body := []byte(`{"model":"","max_tokens":16,"stream":false,"messages":[{"role":"user","content":""}]}`)
		body, _ = sjson.SetBytes(body, "model", route.Model)
		body, _ = sjson.SetBytes(body, "messages.0.content", prompt)
		return GrokProtocolMessages, body
	default:
		body := []byte(`{"model":"","max_tokens":16,"stream":false,"messages":[{"role":"user","content":""}]}`)
		body, _ = sjson.SetBytes(body, "model", route.Model)
		body, _ = sjson.SetBytes(body, "messages.0.content", prompt)
		return GrokProtocolChatCompletions, body
	}
}

func openCodeRetryAfterDuration(resp *http.Response, now time.Time) time.Duration {
	if resp == nil || resp.Header == nil {
		return 0
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if parsed, err := http.ParseTime(raw); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 0
}

func openCodeBodyIndicatesCloudflareBlock(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "error code: 1010") ||
		strings.Contains(lower, "cloudflare") ||
		strings.Contains(lower, "<html")
}

func openCodeBodyIndicatesInsufficientBalance(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "insufficient balance") ||
		strings.Contains(lower, "insufficient_credit") ||
		strings.Contains(lower, "insufficient credits") ||
		strings.Contains(lower, "balance is not enough") ||
		strings.Contains(lower, "no enough balance") ||
		strings.Contains(lower, "余额不足")
}

func openCodeBodyIndicatesFrequencyLimit(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	freqMatched := false
	for _, marker := range []string{
		"too many requests",
		"rate limit exceeded",
		"rate_limit_exceeded",
		"ratelimit exceeded",
		"requests per minute",
		"rpm",
		"速率限制",
		"请求过于频繁",
	} {
		if strings.Contains(lower, marker) {
			freqMatched = true
			break
		}
	}
	if !freqMatched {
		return false
	}
	for _, quotaMarker := range []string{"quota", "usage limit", "insufficient", "balance", "rolling", "weekly", "monthly", "额度", "已用尽"} {
		if strings.Contains(lower, quotaMarker) {
			return false
		}
	}
	return true
}

func openCodeBodyIndicatesQuotaExhausted(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "quota will reset") ||
		strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "rolling limit") ||
		strings.Contains(lower, "weekly limit") ||
		strings.Contains(lower, "monthly limit")
}

// applyOpenCodeCooldown 按 OpenCode 上游错误语义执行智能冷却：
//   - 401：认证失效冷却（不直接误删凭据）；
//   - 403：区分 Cloudflare WAF（短冷却，不永久封号）、配额耗尽（按 Go 窗口重置点冷却）与其他 403；
//   - 429：优先区分频控与 Go 窗口耗尽（有 Go 快照则冷却至最早未来 resets_at），Zen 余额不足按可恢复长冷却处理。
func openCodeErrorMessage(body []byte, statusCode int) string {
	for _, path := range []string{"error.message", "message", "error"} {
		if v := strings.TrimSpace(gjson.GetBytes(body, path).String()); v != "" && !strings.HasPrefix(v, "{") {
			if len(v) > 300 {
				return v[:300]
			}
			return v
		}
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		if len(trimmed) > 240 {
			return trimmed[:240]
		}
		return trimmed
	}
	return fmt.Sprintf("OpenCode upstream HTTP %d", statusCode)
}

func applyOpenCodeCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if store == nil || account == nil {
		return codex429Decision{}
	}
	now := time.Now()
	retryAfter := openCodeRetryAfterDuration(resp, now)
	message := openCodeErrorMessage(body, statusCode)
	cfg := account.OpenCodeConfig()

	cooldownToGoReset := func(fallback time.Duration, reason string) codex429Decision {
		d := fallback
		if cfg.Mode == auth.OpenCodeModeGo {
			if resetAt := auth.OpenCodeEarliestFutureReset(cfg.Usage, now); resetAt != nil {
				if wait := time.Until(*resetAt); wait > 0 {
					d = wait
				}
			}
		}
		if retryAfter > 0 && (d <= 0 || retryAfter > d) {
			d = retryAfter
		}
		if d < 15*time.Second {
			d = time.Minute
		}
		store.MarkCooldownWithErrorExactDuration(account, d, reason, message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: reason, Model: model}
	}

	switch statusCode {
	case http.StatusUnauthorized:
		d := 30 * time.Minute
		store.MarkCooldownWithErrorExactDuration(account, d, "unauthorized", message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "unauthorized", Model: model}
	case http.StatusForbidden:
		if openCodeBodyIndicatesCloudflareBlock(body) {
			d := 2 * time.Minute
			if retryAfter > d {
				d = retryAfter
			}
			store.MarkCooldownWithErrorExactDuration(account, d, "rate_limited", "Cloudflare WAF challenge: "+message)
			return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "rate_limited", Model: model}
		}
		if openCodeBodyIndicatesQuotaExhausted(body) {
			return cooldownToGoReset(5*time.Minute, "usage_limited")
		}
		if openCodeBodyIndicatesInsufficientBalance(body) {
			d := 20 * time.Minute
			store.MarkCooldownWithErrorExactDuration(account, d, "usage_limited", message)
			return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "usage_limited", Model: model}
		}
		d := 10 * time.Minute
		store.MarkCooldownWithErrorExactDuration(account, d, "unauthorized", message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "unauthorized", Model: model}
	case http.StatusPaymentRequired:
		d := 20 * time.Minute
		store.MarkCooldownWithErrorExactDuration(account, d, "usage_limited", message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "usage_limited", Model: model}
	case http.StatusTooManyRequests:
		if openCodeBodyIndicatesInsufficientBalance(body) {
			d := 20 * time.Minute
			store.MarkCooldownWithErrorExactDuration(account, d, "usage_limited", message)
			return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "usage_limited", Model: model}
		}
		if openCodeBodyIndicatesFrequencyLimit(body) &&
			(cfg.Mode != auth.OpenCodeModeGo || !auth.OpenCodeUsageNearExhausted(cfg.Usage, openCodeFrequencyLimitQuotaGuardPercent)) {
			d := time.Minute
			if retryAfter > 0 {
				d = retryAfter
			}
			store.MarkCooldownWithErrorExactDuration(account, d, "rate_limited", message)
			return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "rate_limited", Model: model}
		}
		if cfg.Mode == auth.OpenCodeModeGo && (openCodeBodyIndicatesQuotaExhausted(body) || auth.OpenCodeEarliestFutureReset(cfg.Usage, now) != nil) {
			return cooldownToGoReset(5*time.Minute, "usage_limited")
		}
		d := time.Minute
		if retryAfter > 0 {
			d = retryAfter
		}
		store.MarkCooldownWithErrorExactDuration(account, d, "rate_limited", message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "rate_limited", Model: model}
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		d := 30 * time.Second
		if retryAfter > d {
			d = retryAfter
		}
		store.MarkCooldownWithErrorExactDuration(account, d, "rate_limited", message)
		return codex429Decision{Cooldown: d, ResetAt: now.Add(d), Reason: "rate_limited", Model: model}
	}
	return codex429Decision{}
}

// ApplyOpenCodeCooldown 供管理台连通性测试复用 OpenCode 冷却语义。
func ApplyOpenCodeCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	applyOpenCodeCooldown(store, account, statusCode, body, resp, "")
}
