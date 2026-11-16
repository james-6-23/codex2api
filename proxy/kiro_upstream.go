package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Kiro 上游执行器：Anthropic Messages 入、Anthropic Messages 出（SSE 或 JSON）。
// 端点与请求头移植自 kiro.rs src/kiro/endpoint/{ide,cli}.rs。

const kiroUpstreamErrorReadLimit = 64 << 10

func kiroRuntimeHost(region string) string { return auth.KiroRuntimeHost(region) }

// kiroUpstreamRequest 构造发往 generateAssistantResponse 的 HTTP 请求。
func kiroUpstreamRequest(ctx context.Context, requestAuth auth.KiroRequestAuth, kiroBody kiroRequest) (*http.Request, error) {
	cred := requestAuth.Credential
	kiroBody.ProfileARN = cred.StreamingProfileARN()
	host := kiroRuntimeHost(cred.EffectiveAPIRegion())
	payload, err := json.Marshal(kiroBody)
	if err != nil {
		return nil, err
	}
	url := "https://" + host + "/generateAssistantResponse"
	contentType := "application/json"
	if cred.Endpoint == auth.KiroEndpointCLI {
		payload = kiroCLIBody(payload)
		url = "https://" + host + "/"
		contentType = "application/x-amz-json-1.0"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("Authorization", "Bearer "+requestAuth.BearerToken)
	if tokenType := cred.TokenTypeHeader(); tokenType != "" {
		req.Header.Set("tokentype", tokenType)
	}
	version := auth.KiroIDEVersion()
	if cred.Endpoint == auth.KiroEndpointCLI {
		req.Header.Set("x-amz-target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
		req.Header.Set("x-amzn-codewhisperer-optout", "false")
		req.Header.Set("x-amz-user-agent", "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.14474 os/"+auth.KiroOSName+" lang/rust/1.92.0 m/F app/AmazonQ-For-CLI")
		req.Header.Set("User-Agent", "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.14474 os/"+auth.KiroOSName+" lang/rust/1.92.0 md/appVersion-"+version+" app/AmazonQ-For-CLI")
		return req, nil
	}
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-"+version+"-"+cred.MachineID)
	req.Header.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/"+auth.KiroOSName+" lang/js md/nodejs#"+auth.KiroNodeVersion+" api/codewhispererstreaming#1.0.34 m/E KiroIDE-"+version+"-"+cred.MachineID)
	return req, nil
}

// kiroCLIBody：kiro-cli 的请求来源是 KIRO_CLI，且不带 agentContinuationId 与历史 modelId。
// 只改消息自身的 origin；工具参数与 schema 里的同名字段保持原样。
func kiroCLIBody(payload []byte) []byte {
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		return payload
	}
	state, ok := body["conversationState"].(map[string]any)
	if !ok {
		return payload
	}
	delete(state, "agentContinuationId")
	setOrigin := func(message any, history bool) {
		user, ok := kiroMapValue(message, "userInputMessage").(map[string]any)
		if !ok {
			return
		}
		if user["origin"] == "AI_EDITOR" {
			user["origin"] = "KIRO_CLI"
		}
		if history {
			delete(user, "modelId")
		}
	}
	setOrigin(state["currentMessage"], false)
	if history, ok := state["history"].([]any); ok {
		for _, entry := range history {
			setOrigin(entry, true)
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return encoded
}

// ExecuteKiroMessagesRequest 把 Anthropic Messages 请求发给 Kiro。成功时返回 Anthropic SSE
// （stream=true）或 Anthropic JSON；上游错误按语义映射状态码并保留原始错误体，
// 供 applyKiroCooldown 判定冷却。
func ExecuteKiroMessagesRequest(ctx context.Context, account *auth.Account, body []byte, proxyOverride string) (*http.Response, error) {
	inbound, err := parseKiroInboundRequest(body)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	schema := kiroInjectStructuredInstruction(inbound)
	if kiroOnlyWebSearchTool(inbound) || kiroWebSearchAmongTools(inbound) {
		return executeKiroWebSearchMessages(ctx, account, inbound, proxyOverride, schema)
	}
	conversion, err := convertKiroRequest(inbound, kiroPurposeGenerate)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	return executeKiroConversion(ctx, account, inbound, conversion, proxyOverride, schema)
}

func executeKiroConversion(ctx context.Context, account *auth.Account, inbound *kiroInboundRequest, conversion *kiroConversion, proxyOverride string, schema map[string]any) (*http.Response, error) {
	upstream, failure, err := kiroOpenUpstream(ctx, account, inbound.Model, conversion, proxyOverride)
	if err != nil || failure != nil {
		return failure, err
	}
	stream := newKiroStreamContext(inbound.Model, kiroEstimateInputTokens(inbound), inbound.Thinking.enabled(), conversion)
	stream.cache = kiroComputeCacheUsage(ctx, inbound)
	if !inbound.Stream || schema != nil {
		// 结构化输出要等全文到齐才能抽出合规 JSON，流式请求也整条缓冲后再回放。
		var events []kiroSSEEvent
		kiroDrainUpstream(upstream, stream, func(event kiroSSEEvent) error { events = append(events, event); return nil })
		if schema != nil {
			events = kiroRewriteStructuredEvents(events, schema)
		}
		if inbound.Stream {
			var body bytes.Buffer
			for _, event := range events {
				body.Write(event.bytes())
			}
			return kiroSSEResponse(body.Bytes()), nil
		}
		return kiroAnthropicJSONResponse(events, stream)
	}
	return kiroStreamingResponse(upstream, stream, func(event kiroSSEEvent, write func([]byte) error) error {
		return write(event.bytes())
	}), nil
}

// kiroOpenUpstream 发出请求。返回成功的上游流（调用方负责关闭），或按 Kiro 错误语义
// 映射了状态码、保留原始错误体的合成响应，或传输错误。
func kiroOpenUpstream(ctx context.Context, account *auth.Account, model string, conversion *kiroConversion, proxyOverride string) (*http.Response, *http.Response, error) {
	requestAuth := account.KiroRequestAuth()
	if requestAuth.BearerToken == "" {
		// 只有 refresh token、尚未换票的账号交给 handler 的 401 刷新路径。
		return nil, kiroSyntheticResponse(http.StatusUnauthorized, []byte(`{"message":"The bearer token included in the request is invalid."}`)), nil
	}
	if err := ConsumeAPIKeyModelRequestQuota(ctx, strings.TrimSpace(model)); err != nil {
		return nil, nil, err
	}
	proxyURL := requestAuth.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	proxyURL = requestProxyURL(ctx, proxyURL)
	request := conversion.Request
	for attempt := 0; ; attempt++ {
		req, err := kiroUpstreamRequest(ctx, requestAuth, request)
		if err != nil {
			return nil, nil, ErrInternalError("创建 Kiro 请求失败", err)
		}
		RecordUpstreamUserAgent(ctx, req.Header.Get("User-Agent"))
		client := *getRequestClient(ctx, account, proxyURL)
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := doTracedUpstreamRequest(&client, req, account, proxyURL)
		if err != nil {
			if shouldRecyclePooledClient(err) {
				recycleRequestClient(ctx, account, proxyURL)
			}
			return nil, nil, ErrUpstream(0, "请求 Kiro 失败", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, kiroUpstreamErrorReadLimit))
		resp.Body.Close()
		// 上游拒绝历史思考签名不是账号问题：去掉历史 reasoningContent 后同号重试一次。
		if attempt == 0 && resp.StatusCode == http.StatusBadRequest && kiroIsThinkingSignatureInvalid(string(raw)) && kiroStripHistoryReasoning(&request) {
			log.Printf("[Kiro] 上游拒绝历史思考签名，移除 history reasoningContent 后重试 (account=%d)", account.ID())
			continue
		}
		failure := kiroSyntheticResponse(kiroMapUpstreamStatus(resp.StatusCode, string(raw)), raw)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			failure.Header.Set("Retry-After", retryAfter)
		}
		return nil, failure, nil
	}
}

func kiroIsThinkingSignatureInvalid(body string) bool {
	const code = "THINKING_SIGNATURE_INVALID"
	if !gjson.Valid(body) {
		return strings.TrimSpace(body) == code
	}
	parsed := gjson.Parse(body)
	return parsed.Get("reason").String() == code || parsed.Get("code").String() == code || parsed.Get("__type").String() == code
}

// kiroStripHistoryReasoning 只移除历史 assistant 的原生 reasoning，其余字段不动。
func kiroStripHistoryReasoning(request *kiroRequest) bool {
	history := append([]kiroHistoryEntry(nil), request.ConversationState.History...)
	changed := false
	for i, entry := range history {
		if entry.Assistant != nil && entry.Assistant.ReasoningContent != nil {
			stripped := *entry.Assistant
			stripped.ReasoningContent = nil
			history[i].Assistant = &stripped
			changed = true
		}
	}
	if changed {
		request.ConversationState.History = history
	}
	return changed
}

// kiroDrainUpstream 同步跑完整条上游流并关闭它。
func kiroDrainUpstream(upstream *http.Response, stream *kiroStreamContext, sink func(kiroSSEEvent) error) {
	defer upstream.Body.Close()
	_ = kiroPumpSSE(upstream.Body, stream, sink)
}

// kiroStreamingResponse 在后台把上游流交给 render 写进管道；下游断开时写失败、停止读取。
func kiroStreamingResponse(upstream *http.Response, stream *kiroStreamContext, render func(kiroSSEEvent, func([]byte) error) error) *http.Response {
	reader, writer := io.Pipe()
	write := func(chunk []byte) error {
		_, err := writer.Write(chunk)
		return err
	}
	go func() {
		defer upstream.Body.Close()
		writer.CloseWithError(kiroPumpSSE(upstream.Body, stream, func(event kiroSSEEvent) error { return render(event, write) }))
	}()
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: reader, ContentLength: -1, Request: upstream.Request}
}

func kiroSSEResponse(body []byte) *http.Response {
	response := kiroSyntheticResponse(http.StatusOK, body)
	response.Header.Set("Content-Type", "text/event-stream")
	return response
}

func kiroSyntheticResponse(status int, body []byte) *http.Response {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

// kiroPumpSSE 逐帧转换并交给 sink。sink 失败（下游断开）时返回该错误。
func kiroPumpSSE(source io.Reader, stream *kiroStreamContext, sink func(kiroSSEEvent) error) error {
	emit := func(events []kiroSSEEvent) error {
		for _, event := range events {
			if err := sink(event); err != nil {
				return err
			}
		}
		return nil
	}
	if err := emit(stream.initialEvents()); err != nil {
		return err
	}
	frames := newKiroFrameReader(source)
	for {
		frame, err := frames.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Printf("[Kiro] 上游事件帧损坏: %v", err)
			return emit(stream.errorEvents("api_error", kiroInterruptedMessage("malformed event frame from upstream")))
		}
		// 与 kiro.rs 一致：error / exception 帧（ContentLengthExceededException 除外）立即中断，
		// 已输出的部分内容不能伪装成正常结束。
		if frame.MessageType != "event" && frame.ExceptionType != "ContentLengthExceededException" {
			code, message := kiroFrameFailure(frame)
			log.Printf("[Kiro] 上游流内 %s: %s: %s", frame.MessageType, code, message)
			errorType, clientMessage := kiroClassifyUpstreamFailure(code, message)
			return emit(stream.errorEvents(errorType, clientMessage))
		}
		if err := emit(stream.processFrame(frame)); err != nil {
			return err
		}
	}
	return emit(stream.finalEvents())
}

func kiroFrameFailure(frame kiroFrame) (string, string) {
	code := frame.ErrorCode
	if code == "" {
		code = frame.ExceptionType
	}
	message := strings.TrimSpace(gjson.GetBytes(frame.Payload, "message").String())
	if message == "" {
		message = strings.TrimSpace(string(frame.Payload))
	}
	return code, message
}

func kiroInterruptedMessage(reason string) string {
	return "Upstream response stream was interrupted: " + reason + ". Retry the request."
}

// kiroSanitizeUpstreamCode 只保留异常类型名（去掉 com.amazon...# 前缀，限制字符与长度），不回传上游原文。
func kiroSanitizeUpstreamCode(code string) string {
	if index := strings.LastIndexAny(code, "#.:"); index >= 0 {
		code = code[index+1:]
	}
	var out strings.Builder
	for _, r := range code {
		if out.Len() >= 64 {
			break
		}
		if r == '_' || r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// kiroClassifyUpstreamFailure 移植 kiro.rs classify_upstream_failure：按类型名与说明归类，
// 说明只用于归类，不出现在返回给客户端的消息里。
func kiroClassifyUpstreamFailure(code, message string) (string, string) {
	label := kiroSanitizeUpstreamCode(code)
	if label == "" {
		label = "unknown upstream error"
	}
	codeLower := strings.ToLower(code)
	lower := codeLower + " " + strings.ToLower(message)
	has := func(needles ...string) bool {
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				return true
			}
		}
		return false
	}
	switch {
	case has("throttl", "rate exceeded", "rate limit", "too many requests", "toomanyrequests"):
		return "rate_limit_error", "Upstream rate limit reached (" + label + "). Retry after a short wait."
	case has("serviceunavailable", "service unavailable", "overloaded", "high load", "insufficient capacity", "temporarily unavailable", "temporarily_unavailable"):
		return "overloaded_error", "Upstream model is overloaded (" + label + "). Retry later."
	case has("input is too long", "prompt is too long", "too many tokens", "context window", "context length", "exceeds the maximum"):
		return "invalid_request_error", "prompt is too long: the upstream rejected the input as exceeding the model context (" + label + ")."
	case has("accessdenied", "access denied", "unauthorized", "forbidden"):
		return "permission_error", "Upstream denied access (" + label + ")."
	case strings.Contains(codeLower, "validation"):
		return "invalid_request_error", "Upstream rejected the request (" + label + ")."
	}
	return "api_error", kiroInterruptedMessage("upstream reported " + label)
}

// kiroFailureStatus 是非流式路径的 HTTP 状态码，与 Anthropic 错误类型对应（kiro.rs StreamFailure::status）。
func kiroFailureStatus(errorType string) int {
	switch errorType {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return http.StatusServiceUnavailable
	case "invalid_request_error":
		return http.StatusBadRequest
	case "permission_error":
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

// kiroAnthropicJSONResponse 把聚合后的事件序列返回成非流式 Anthropic 响应。
func kiroAnthropicJSONResponse(events []kiroSSEEvent, stream *kiroStreamContext) (*http.Response, error) {
	message, failure := kiroAggregateEvents(events)
	if failure != nil {
		encoded, _ := json.Marshal(map[string]any{"type": "error", "error": failure})
		errorType, _ := failure["type"].(string)
		return kiroSyntheticResponse(kiroFailureStatus(errorType), encoded), nil
	}
	message["id"] = stream.messageID
	message["model"] = stream.model
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, ErrInternalError("序列化 Kiro 响应失败", err)
	}
	return kiroSyntheticResponse(http.StatusOK, encoded), nil
}

// kiroAggregateEvents 把 SSE 事件序列还原成一条 Anthropic message。
func kiroAggregateEvents(events []kiroSSEEvent) (map[string]any, map[string]any) {
	message := map[string]any{"type": "message", "role": "assistant", "stop_sequence": nil}
	blocks := map[int]map[string]any{}
	partialJSON := map[int]*strings.Builder{}
	var order []int
	for _, event := range events {
		index, _ := event.Data["index"].(int)
		switch event.Event {
		case "error":
			failure, _ := event.Data["error"].(map[string]any)
			return nil, failure
		case "content_block_start":
			block, _ := event.Data["content_block"].(map[string]any)
			copied := map[string]any{}
			for key, value := range block {
				copied[key] = value
			}
			blocks[index] = copied
			order = append(order, index)
		case "content_block_delta":
			block := blocks[index]
			delta, _ := event.Data["delta"].(map[string]any)
			if block == nil || delta == nil {
				continue
			}
			switch delta["type"] {
			case "text_delta":
				block["text"] = fmt.Sprint(block["text"]) + fmt.Sprint(delta["text"])
			case "thinking_delta":
				block["thinking"] = fmt.Sprint(block["thinking"]) + fmt.Sprint(delta["thinking"])
			case "signature_delta":
				block["signature"] = delta["signature"]
			case "input_json_delta":
				if partialJSON[index] == nil {
					partialJSON[index] = &strings.Builder{}
				}
				partialJSON[index].WriteString(fmt.Sprint(delta["partial_json"]))
			}
		case "message_delta":
			if delta, ok := event.Data["delta"].(map[string]any); ok {
				message["stop_reason"] = delta["stop_reason"]
			}
			message["usage"] = event.Data["usage"]
		}
	}
	content := make([]any, 0, len(order))
	for _, index := range order {
		block := blocks[index]
		if block["type"] == "tool_use" {
			input := map[string]any{}
			if raw := partialJSON[index]; raw != nil && strings.TrimSpace(raw.String()) != "" {
				_ = json.Unmarshal([]byte(raw.String()), &input)
			}
			block["input"] = input
		}
		if block["type"] == "text" && fmt.Sprint(block["text"]) == "" && len(order) > 1 {
			continue
		}
		content = append(content, block)
	}
	message["content"] = content
	return message, nil
}

// kiroEstimateInputTokens 是上游没给精确用量与上下文百分比时的输入估算。
func kiroEstimateInputTokens(req *kiroInboundRequest) int {
	total := 0
	for _, text := range req.systemTexts() {
		total += kiroCountTokens(text)
	}
	for _, message := range req.Messages {
		text, isString, blocks := kiroContent(message.Content)
		if isString {
			total += kiroCountTokens(text)
		}
		for _, block := range blocks {
			total += kiroCountBlockTokens(block)
		}
	}
	for _, tool := range req.Tools {
		total += kiroCountTokens(tool.Name) + kiroCountTokens(tool.Description) + kiroCountTokens(string(tool.InputSchema))
	}
	return max(total, 1)
}

func kiroCountBlockTokens(block map[string]any) int {
	switch kiroString(block, "type") {
	case "tool_use":
		input, _ := json.Marshal(block["input"])
		return kiroCountTokens(kiroString(block, "name")) + kiroCountTokens(string(input))
	case "tool_result":
		switch content := block["content"].(type) {
		case string:
			return kiroCountTokens(content)
		case []any:
			total := 0
			for _, item := range content {
				if child, ok := item.(map[string]any); ok {
					total += kiroCountBlockTokens(child)
				}
			}
			return total
		}
		return 0
	case "thinking":
		return kiroCountTokens(kiroString(block, "thinking"))
	case "image":
		return 1600
	}
	return kiroCountTokens(kiroString(block, "text"))
}

// kiroCountTokens：非西文字符按 4 个单位计，短文本放大系数更高（移植自 kiro.rs token.rs）。
func kiroCountTokens(text string) int {
	units := 0.0
	for _, r := range text {
		if r <= 0x024F || r >= 0x1E00 && r <= 0x1EFF || r >= 0x2C60 && r <= 0x2C7F || r >= 0xA720 && r <= 0xA7FF || r >= 0xAB30 && r <= 0xAB6F {
			units++
		} else {
			units += 4
		}
	}
	tokens := units / 4
	switch {
	case tokens < 100:
		tokens *= 1.5
	case tokens < 200:
		tokens *= 1.3
	case tokens < 300:
		tokens *= 1.25
	case tokens < 800:
		tokens *= 1.2
	}
	return int(tokens)
}

// ---- 上游错误分类（移植自 kiro.rs src/kiro/endpoint/mod.rs）----

func kiroErrorReasons(body string) []string {
	parsed := gjson.Parse(body)
	return []string{parsed.Get("reason").String(), parsed.Get("error.reason").String()}
}

func kiroBodyHasReason(body string, reasons ...string) bool {
	if !gjson.Valid(body) {
		for _, reason := range reasons {
			if strings.Contains(body, reason) {
				return true
			}
		}
		return false
	}
	for _, found := range kiroErrorReasons(body) {
		for _, reason := range reasons {
			if found == reason {
				return true
			}
		}
	}
	return false
}

func kiroIsMonthlyRequestLimit(body string) bool {
	return kiroBodyHasReason(body, "MONTHLY_REQUEST_COUNT", "OVERAGE_REQUEST_LIMIT_EXCEEDED")
}

func kiroIsBearerTokenInvalid(body string) bool {
	return strings.Contains(body, "The bearer token included in the request is invalid")
}

func kiroIsAccountRateLimited(body string) bool {
	if gjson.Valid(body) {
		switch gjson.Get(body, "reason").String() {
		case "USER_REQUEST_RATE_EXCEEDED", "CREDIT_CONSUMPTION_RATE_EXCEEDED", "SERVICE_REQUEST_RATE_EXCEEDED":
			return true
		}
	}
	message := body
	if value := gjson.Get(body, "message"); value.Exists() {
		message = value.String()
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "suspicious activity") && strings.Contains(lower, "temporary limits") ||
		strings.Contains(lower, "5-minute credit limit exceeded") ||
		strings.TrimRight(lower, ".") == "too many requests, please wait before trying again"
}

func kiroIsAccountSuspended(body string) bool {
	if strings.Contains(body, "TEMPORARILY_SUSPENDED") && (!gjson.Valid(body) || kiroBodyHasReason(body, "TEMPORARILY_SUSPENDED")) {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "suspended") && (strings.Contains(lower, "locked your account") || strings.Contains(lower, "locked it"))
}

func kiroIsGatewayTimeout(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(body, "524") && (strings.Contains(lower, "status code") || strings.Contains(lower, "gateway timeout") || strings.Contains(lower, "server-side issue"))
}

func kiroIsClientValidationError(body string) bool {
	if strings.Contains(body, "TOOL_USE_RESULT_MISMATCH") || strings.Contains(body, "TOOL_SCHEMA_INVALID") {
		if !gjson.Valid(body) || kiroBodyHasReason(body, "TOOL_USE_RESULT_MISMATCH", "TOOL_SCHEMA_INVALID") {
			return true
		}
	}
	return strings.Contains(body, "Expected toolResult blocks")
}

// kiroMapUpstreamStatus 把 Kiro 的错误语义映射到通用冷却与重试逻辑认识的状态码。
func kiroMapUpstreamStatus(status int, body string) int {
	switch {
	case kiroIsBearerTokenInvalid(body):
		return http.StatusUnauthorized
	case kiroIsMonthlyRequestLimit(body), kiroIsAccountRateLimited(body):
		return http.StatusTooManyRequests
	case kiroIsAccountSuspended(body):
		return http.StatusForbidden
	case kiroIsClientValidationError(body):
		return http.StatusBadRequest
	case status == 524 || kiroIsGatewayTimeout(body):
		return http.StatusBadGateway
	}
	return status
}

const (
	kiroAccountThrottleCooldown  = 30 * time.Minute
	kiroGenericRateLimitCooldown = time.Minute
)

// kiroRefreshAfterUnauthorized 在上游 401 后刷新一次 Kiro 令牌；成功时调用方同号重试。
// retried 与 Antigravity 共用同一张按请求去重的表。
func (h *Handler) kiroRefreshAfterUnauthorized(ctx context.Context, account *auth.Account, status int, retried map[int64]bool) bool {
	if status != http.StatusUnauthorized || h == nil || h.store == nil || !account.KiroRefreshable() || retried[account.ID()] {
		return false
	}
	retried[account.ID()] = true
	if err := h.store.RefreshKiroAccount(ctx, account); err != nil {
		log.Printf("Kiro token refresh failed after upstream 401 (account=%d): %v", account.ID(), err)
		return false
	}
	log.Printf("Kiro token refreshed after upstream 401 (account=%d)", account.ID())
	return true
}

// applyKiroCooldown：额度用尽冷却到重置日，封号标错误，账号级限流冷却 30 分钟（kiro.rs 默认）。
func applyKiroCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, model string) codex429Decision {
	text := string(body)
	message := upstreamAccountErrorMessage(statusCode, body)
	switch {
	case kiroIsMonthlyRequestLimit(text):
		resetAt := account.KiroUsageResetAt()
		if !resetAt.After(time.Now()) {
			now := time.Now().UTC()
			resetAt = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		}
		store.MarkCooldownWithErrorExactDuration(account, time.Until(resetAt), "usage_limit", message)
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "usage_limit", ResetAt: resetAt, Cooldown: time.Until(resetAt)}
	case kiroIsAccountSuspended(text):
		store.MarkError(account, "Kiro 账号已被暂停: "+message)
	case kiroIsAccountRateLimited(text):
		store.MarkCooldownWithErrorExactDuration(account, kiroAccountThrottleCooldown, "rate_limited", message)
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited", ResetAt: time.Now().Add(kiroAccountThrottleCooldown), Cooldown: kiroAccountThrottleCooldown}
	case statusCode == http.StatusTooManyRequests:
		store.MarkCooldownWithErrorExactDuration(account, kiroGenericRateLimitCooldown, "rate_limited", message)
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited", ResetAt: time.Now().Add(kiroGenericRateLimitCooldown), Cooldown: kiroGenericRateLimitCooldown}
	case statusCode == http.StatusUnauthorized:
		store.MarkCooldownWithError(account, 5*time.Minute, "unauthorized", message)
	}
	_ = model
	return codex429Decision{}
}

// ApplyKiroCooldown 让管理台测连复用 Kiro 的冷却语义（月度额度、账号限流、封禁）。
func ApplyKiroCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte) {
	applyKiroCooldown(store, account, statusCode, body, "")
}
