package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

const intelligenceOutputLimit = 256 * 1024

type intelligenceExecution struct {
	CredentialRejected bool
	Output             string
	Error              string
	Complete           bool
	Interrupted        bool
	DurationMS         int64
	ResponseModel      string
	InputTokens        int
	OutputTokens       int
	ReasoningTokens    int
	CachedTokens       int
	terminalPayload    []byte
	EffectiveContext   map[string]string
}

func intelligenceAccountChannel(account *auth.Account) string {
	switch {
	case account.IsAntigravityAPI():
		return "antigravity"
	case account.IsGrokAPI():
		return "grok"
	case account.IsClaudeOAuth():
		return "claude"
	default:
		return "codex"
	}
}

// Uses the same outbound executors as generation, with no browser request context
// or application-level retry. The detector context freezes the measurement prompt
// and model against user payload rules; identity/fingerprint/timezone still apply.
func (h *Handler) executeIntelligenceCheck(ctx context.Context, account *auth.Account, model, effort, prompt string) (result intelligenceExecution) {
	if account == nil {
		return intelligenceExecution{Interrupted: true, Error: "配置错误：该账号渠道不支持降智检测"}
	}
	claude := account.IsClaudeOAuth()
	if claude || account.IsGrokAPI() || account.IsAntigravityAPI() {
		effort = ""
	}
	if err := h.validateQualityTestForAccount(ctx, account, qualityTestRequest{Model: model, ReasoningEffort: effort}); err != nil {
		return intelligenceExecution{Interrupted: true, Error: "配置错误：" + err.Error()}
	}
	payload := map[string]any{"model": model, "stream": true}
	security := h.store.ClaudeSecurityConfig()
	if claude {
		maxTokens := int64(32768)
		if security.MaxOutputTokens > 0 && security.MaxOutputTokens < maxTokens {
			maxTokens = security.MaxOutputTokens
		}
		payload["messages"] = []map[string]any{{"role": "user", "content": prompt}}
		payload["max_tokens"] = maxTokens
	} else {
		payload["store"] = false
		payload["instructions"] = ""
		payload["input"] = []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}}
		if effort != "" {
			payload["reasoning"] = map[string]string{"effort": effort}
		}
	}
	body, _ := json.Marshal(payload)
	proxyURL := strings.TrimSpace(account.GetProxyURL())
	ctx, cancel := context.WithCancel(proxy.WithAccountBoundEgress(proxy.WithUpstreamIdentityAudit(proxy.WithCodexDetectorRequest(ctx)), proxyURL))
	defer cancel()
	fingerprint := ""
	if claude {
		fingerprint = account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault())
	} else if !account.IsRelayStyle() {
		fingerprint = account.EffectiveCodexFingerprintMode()
	}
	account.Mu().RLock()
	timezone := account.Timezone
	account.Mu().RUnlock()
	start := time.Now()
	statusCode := connectionTestTransportStatus
	var resp *http.Response
	var err error
	defer func() {
		result.DurationMS = time.Since(start).Milliseconds()
		secrets := codexTestSecrets(account)
		// Also mask configured header values if an upstream echoes them in an error.
		for key, value := range account.GetCustomHeaders() {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "auth") || strings.Contains(lower, "token") || strings.Contains(lower, "cookie") || strings.Contains(lower, "key") {
				secrets = append(secrets, value)
			}
		}
		result.Output = sanitizeCodexTestText(result.Output, secrets)
		result.Error = truncate(sanitizeCodexTestText(result.Error, secrets), 4096)
		result.ResponseModel = truncate(sanitizeCodexTestText(result.ResponseModel, secrets), 256)
		result.EffectiveContext = map[string]string{"fingerprint_mode": fingerprint, "timezone": timezone, "proxy": "direct", "transport": "http", "attempts": strconv.Itoa(proxy.UpstreamAttemptCount(ctx))}
		if proxyURL != "" {
			result.EffectiveContext["proxy"] = "configured"
		}
		if ua, ok := proxy.UpstreamIdentityAudit(ctx); ok {
			result.EffectiveContext["user_agent"] = sanitizeCodexTestText(ua, secrets)
		}
		if resp != nil && codexTestTransport(resp.Header) == "websocket" {
			result.EffectiveContext["transport"] = "websocket"
		}
		h.logConnectionTestUsage(nil, account, connectionTestUsageInput{Reason: "intelligence_check", Endpoint: connectionTestEndpoint(account), Model: model, EffectiveModel: result.ResponseModel, ReasoningEffort: effort, StatusCode: statusCode, DurationMs: int(result.DurationMS), InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, ReasoningTokens: result.ReasoningTokens, CachedTokens: result.CachedTokens, ErrorMessage: result.Error, ViaWebsocket: result.EffectiveContext["transport"] == "websocket"})
	}()
	switch {
	case claude:
		resp, err = proxy.ExecuteClaudeMessagesRequest(ctx, account, body, proxyURL, make(http.Header), fingerprint, security)
	case account.IsAntigravityAPI():
		resp, err = h.executeAntigravityConnectionTest(ctx, account, model, body, proxyURL, true)
	case account.IsRelayStyle():
		resp, err = proxy.ExecuteRelayStyleRequest(ctx, account, body, proxyURL, nil)
	default:
		resp, err = proxy.ExecuteRequest(ctx, account, body, "", proxyURL, "", h.intelligenceDeviceConfig, nil)
	}
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		result.Error = fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(raw), 4096))
		result.CredentialRejected = intelligenceCredentialRejected(raw)
		h.applyIntelligenceUpstreamFailure(account, resp, model, raw)
		return
	}
	if claude {
		proxy.SyncClaudeUsageState(h.store, account, resp)
	} else if !account.IsRelayStyle() {
		proxy.SyncCodexUsageState(h.store, account, resp)
	}
	result = readIntelligenceStream(resp.Body, claude)
	result.CredentialRejected = intelligenceCredentialRejected(result.terminalPayload)
	if ctx.Err() != nil {
		result.Complete = false
		result.Error = ctx.Err().Error()
	}
	if !claude && !account.IsAntigravityAPI() && result.Error != "" && len(result.terminalPayload) > 0 {
		h.applyResponsesUsageLimitFailure(account, resp, model, result.terminalPayload)
	}
	// Never mark an account successful/reset its authentication or quota state here.
	if result.Error != "" {
		if claude {
			applyClaudeConnectionStreamFailure(h, account, model, "failed", result.Error, resp)
		}
	}
	return
}

func (h *Handler) applyIntelligenceUpstreamFailure(account *auth.Account, resp *http.Response, model string, raw []byte) {
	if account.IsAntigravityAPI() {
		return
	}
	detail := truncate(sanitizeCodexTestText(string(raw), codexTestSecrets(account)), 500)
	if account.IsClaudeOAuth() {
		syncClaudeTestUsageState(h.store, account, model, resp, raw)
		if resp.StatusCode == http.StatusUnauthorized {
			h.store.MarkCooldownWithError(account, 5*time.Minute, "unauthorized", detail)
		}
		return
	}
	if !account.IsRelayStyle() {
		proxy.SyncCodexUsageState(h.store, account, resp)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", detail)
	case http.StatusPaymentRequired:
		if proxy.IsDeactivatedWorkspaceError(raw) {
			h.store.MarkDeactivatedWorkspace(account, detail)
		} else {
			h.store.MarkError(account, detail)
		}
	case http.StatusForbidden:
		if proxy.IsAgentRuntimeDeletedError(raw) {
			h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", detail)
		} else if proxy.IsDeactivatedWorkspaceError(raw) {
			h.store.MarkDeactivatedWorkspace(account, detail)
		}
	case http.StatusTooManyRequests:
		if account.IsRelayStyle() {
			h.store.MarkCooldown(account, time.Minute, "rate_limited")
		} else {
			proxy.Apply429Cooldown(h.store, account, raw, resp, model)
		}
	}
}

func readIntelligenceStream(body io.Reader, claude bool) (result intelligenceExecution) {
	var output strings.Builder
	claudeStop := ""
	// Responses may contain commentary and final_answer messages. Track individual
	// output/content indices so a terminal without output can still select only the
	// final message, and output_text.done replaces rather than duplicates deltas.
	type responseText struct {
		phase string
		parts map[int]string
	}
	messages := map[int]*responseText{}
	textBytes := 0
	message := func(index int) *responseText {
		if messages[index] == nil {
			messages[index] = &responseText{parts: map[int]string{}}
		}
		return messages[index]
	}
	setText := func(index, part int, text string, appendDelta bool) bool {
		m := message(index)
		if appendDelta {
			text = m.parts[part] + text
		}
		textBytes += len(text) - len(m.parts[part])
		if textBytes > intelligenceOutputLimit {
			result.Error = "最终答案超过 256 KiB 上限"
			return false
		}
		m.parts[part] = text
		return true
	}
	readItem := func(index int, item gjson.Result) bool {
		m := message(index)
		m.phase = item.Get("phase").String()
		if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" {
			m.phase = "excluded"
			return true
		}
		for i, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" && !setText(index, i, part.Get("text").String(), false) {
				return false
			}
		}
		return true
	}
	appendText := func(text string) bool {
		if output.Len()+len(text) > intelligenceOutputLimit {
			result.Error = "最终答案超过 256 KiB 上限"
			return false
		}
		output.WriteString(text)
		return true
	}
	readUsage := func(usage gjson.Result) {
		for _, entry := range []struct {
			key   string
			value *int
		}{{"input_tokens", &result.InputTokens}, {"output_tokens", &result.OutputTokens}, {"output_tokens_details.reasoning_tokens", &result.ReasoningTokens}, {"input_tokens_details.cached_tokens", &result.CachedTokens}} {
			if value := usage.Get(entry.key); value.Exists() {
				*entry.value = int(value.Int())
			}
		}
		if claude {
			if value := usage.Get("cache_read_input_tokens"); value.Exists() {
				result.CachedTokens = int(value.Int())
			}
		}
	}
	err := proxy.ReadSSEStreamWithEvent(io.LimitReader(body, 16*1024*1024), func(event string, data []byte) bool {
		if !gjson.ValidBytes(data) {
			result.Error = "上游返回无效事件"
			return false
		}
		root := gjson.ParseBytes(data)
		kind := root.Get("type").String()
		if kind == "" {
			kind = event
		}
		if claude {
			switch kind {
			case "message_start":
				result.ResponseModel = root.Get("message.model").String()
				readUsage(root.Get("message.usage"))
			case "content_block_start":
				if root.Get("content_block.type").String() == "text" {
					return appendText(root.Get("content_block.text").String())
				}
			case "content_block_delta":
				if root.Get("delta.type").String() == "text_delta" {
					return appendText(root.Get("delta.text").String())
				}
			case "message_delta":
				claudeStop = root.Get("delta.stop_reason").String()
				readUsage(root.Get("usage"))
			case "message_stop":
				if claudeStop != "end_turn" && claudeStop != "stop_sequence" {
					result.Error = "Claude 未完整完成答案: " + claudeStop
				} else {
					result.Complete = true
				}
				return false
			case "error":
				result.Error = formatUpstreamTestError(data, "Claude 上游错误")
				return false
			}
			return true
		}
		if model := root.Get("response.model").String(); model != "" {
			result.ResponseModel = model
		}
		readUsage(root.Get("response.usage"))
		switch kind {
		case "response.output_item.added", "response.output_item.done":
			return readItem(int(root.Get("output_index").Int()), root.Get("item"))
		case "response.output_text.delta":
			return setText(int(root.Get("output_index").Int()), int(root.Get("content_index").Int()), root.Get("delta").String(), true)
		case "response.output_text.done":
			return setText(int(root.Get("output_index").Int()), int(root.Get("content_index").Int()), root.Get("text").String(), false)
		case "response.completed":
			status := root.Get("response.status").String()
			if status != "" && status != "completed" {
				result.Error = formatUpstreamTestError(data, "上游未完整完成: "+status)
				return false
			}
			// The terminal response is authoritative if it contains output. Never grade
			// reasoning summaries, tool arguments, or an earlier partial answer.
			if items := root.Get("response.output"); items.Exists() {
				messages = map[int]*responseText{}
				textBytes = 0
				for index, item := range items.Array() {
					if !readItem(index, item) {
						return false
					}
				}
			}
			result.Complete = true
			return false
		case "response.failed", "response.incomplete", "error":
			result.terminalPayload = append([]byte(nil), data...)
			result.Error = formatUpstreamTestError(data, "上游未完整完成答案")
			return false
		}
		return true
	})
	if !claude {
		hasFinal := false
		indices := make([]int, 0, len(messages))
		for index, m := range messages {
			indices = append(indices, index)
			hasFinal = hasFinal || m.phase == "final_answer"
		}
		sort.Ints(indices)
		for _, index := range indices {
			m := messages[index]
			if m.phase != "" && m.phase != "final_answer" || hasFinal && m.phase != "final_answer" {
				continue
			}
			parts := make([]int, 0, len(m.parts))
			for part := range m.parts {
				parts = append(parts, part)
			}
			sort.Ints(parts)
			for _, part := range parts {
				if !appendText(m.parts[part]) {
					break
				}
			}
		}
	}
	result.Output = output.String()
	if err != nil && !result.Complete && result.Error == "" {
		result.Error = "读取上游响应失败: " + err.Error()
	}
	if !result.Complete && result.Error == "" {
		result.Error = "上游流已中断，未收到成功终态"
	}
	return
}

func intelligenceCredentialRejected(payload []byte) bool {
	for _, path := range []string{"error.code", "response.error.code"} {
		switch gjson.GetBytes(payload, path).String() {
		case "token_revoked", "account_deactivated", "workspace_deactivated", "organization_deactivated":
			return true
		}
	}
	return false
}
