package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 单模型探测结果分类。
const (
	modelProbeAvailable   = "available"   // 200 完成且有输出：账号确认可用该模型
	modelProbeUnsupported = "unsupported" // 上游明确拒绝：账号套餐不支持该模型
	modelProbeThrottled   = "throttled"   // 429/用量耗尽：模型可能可用但当前被限流，结果不可靠
	modelProbeError       = "error"       // 其他错误：超时/传输/鉴权/5xx 等，无法判定
)

// 单模型探测并发上限，避免一次点击对同一账号打出过多并发请求。
const modelProbeMaxConcurrency = 6

type modelProbeResult struct {
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// modelProbeEvent 是探测过程推送给前端的 SSE 事件。
// type: start（下发全部待测模型）| testing（某模型开始探测）| result（某模型出结果）| done（结束，含可用集合）
type modelProbeEvent struct {
	Type      string   `json:"type"`
	Total     int      `json:"total,omitempty"`
	Current   int      `json:"current,omitempty"`
	Models    []string `json:"models,omitempty"`
	Model     string   `json:"model,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Available []string `json:"available,omitempty"`
}

// ProbeAccountModels 用账号自身凭据并发探测系统模型列表（已排除 image 模型），
// 判定每个模型是否可用。全程只读，不回写账号调度状态（冷却/错误/成功）。
// stream=true 时以 SSE 逐模型推送进度，否则一次性返回 JSON。
// POST /api/admin/accounts/:id/models/probe
func (h *Handler) ProbeAccountModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}
	if account.IsRelayStyle() && !account.IsClaudeOAuth() {
		writeError(c, http.StatusBadRequest, "中转/Grok 账号不支持模型探测")
		return
	}
	if !account.IsCodexAgentIdentity() && account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 Access Token，请先刷新")
		return
	}

	models := proxy.TextTestModelIDs(c.Request.Context(), h.db)
	if account.IsClaudeOAuth() {
		models = claudeProbeModelIDs(account)
	}
	streaming := strings.EqualFold(c.Query("stream"), "true")

	if len(models) == 0 {
		if streaming {
			setupSSE(c)
			sendSSEJSON(c, modelProbeEvent{Type: "start", Total: 0, Models: []string{}})
			sendSSEJSON(c, modelProbeEvent{Type: "done", Available: []string{}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"available": []string{}, "results": []modelProbeResult{}})
		return
	}

	concurrency := h.store.GetTestConcurrency()
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > modelProbeMaxConcurrency {
		concurrency = modelProbeMaxConcurrency
	}

	if streaming {
		h.streamProbeModels(c, account, models, concurrency)
		return
	}

	results := h.runProbeModels(c.Request.Context(), account, models, concurrency, nil)
	available := collectAvailableModels(results)
	c.JSON(http.StatusOK, gin.H{
		"available": available,
		"results":   results,
	})
}

// streamProbeModels 以 SSE 逐模型推送探测进度。
func (h *Handler) streamProbeModels(c *gin.Context, account *auth.Account, models []string, concurrency int) {
	setupSSE(c)
	sendSSEJSON(c, modelProbeEvent{Type: "start", Total: len(models), Models: models})

	events := make(chan modelProbeEvent, len(models)+1)
	ctx := c.Request.Context()
	go func() {
		results := h.runProbeModels(ctx, account, models, concurrency, func(ev modelProbeEvent) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		})
		select {
		case events <- modelProbeEvent{Type: "done", Available: collectAvailableModels(results)}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for ev := range events {
		sendSSEJSON(c, ev)
	}
}

// runProbeModels 并发探测所有模型；onEvent 非空时逐模型回调 testing/result 事件（用于 SSE）。
func (h *Handler) runProbeModels(ctx context.Context, account *auth.Account, models []string, concurrency int, onEvent func(modelProbeEvent)) []modelProbeResult {
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		results   = make([]modelProbeResult, len(models))
		sem       = make(chan struct{}, concurrency)
		completed int
		total     = len(models)
	)
	for i, model := range models {
		wg.Add(1)
		go func(idx int, m string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				results[idx] = modelProbeResult{Model: m, Outcome: modelProbeError, Detail: "探测已取消"}
				completed++
				current := completed
				mu.Unlock()
				if onEvent != nil {
					onEvent(modelProbeEvent{Type: "result", Model: m, Outcome: modelProbeError, Detail: "探测已取消", Current: current, Total: total})
				}
				return
			}
			defer func() { <-sem }()

			if onEvent != nil {
				onEvent(modelProbeEvent{Type: "testing", Model: m})
			}
			outcome, detail := h.probeAccountModel(ctx, account, m)
			mu.Lock()
			results[idx] = modelProbeResult{Model: m, Outcome: outcome, Detail: detail}
			completed++
			current := completed
			mu.Unlock()
			if onEvent != nil {
				onEvent(modelProbeEvent{Type: "result", Model: m, Outcome: outcome, Detail: detail, Current: current, Total: total})
			}
		}(i, model)
	}
	wg.Wait()
	return results
}

func collectAvailableModels(results []modelProbeResult) []string {
	available := make([]string, 0, len(results))
	for _, r := range results {
		if r.Outcome == modelProbeAvailable {
			available = append(available, r.Model)
		}
	}
	available = auth.NormalizeAccountModels(available)
	sort.Strings(available)
	return available
}

// probeAccountModel 对单个模型发起最小探测请求并分类结果。不回写任何账号状态。
func (h *Handler) probeAccountModel(ctx context.Context, account *auth.Account, model string) (string, string) {
	if account != nil && account.IsClaudeOAuth() {
		return h.probeClaudeAccountModel(ctx, account, model)
	}
	probeCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()

	payload := buildConnectionTestPayload(h.store, model)
	resp, err := proxy.ExecuteRequest(probeCtx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		if msg, ok := batchTestContextFailure(probeCtx, err); ok {
			return modelProbeError, msg
		}
		return modelProbeError, err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return readProbeStream(probeCtx, resp)
	case http.StatusTooManyRequests:
		return modelProbeThrottled, "上游返回 429 限流"
	case http.StatusBadRequest:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		if proxy.IsCodexModelUnsupportedError(body) {
			return modelProbeUnsupported, "账号套餐不支持该模型"
		}
		return modelProbeError, fmt.Sprintf("上游返回 400: %s", truncate(string(body), 200))
	case http.StatusUnauthorized:
		return modelProbeError, "账号授权失败（401）"
	default:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		return modelProbeError, fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

func claudeProbeModelIDs(account *auth.Account) []string {
	models := proxy.DefaultClaudeModelIDsForAccount(account)
	filtered := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if !strings.HasPrefix(strings.ToLower(model), "claude-") {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, model)
	}
	if len(filtered) > 0 {
		return filtered
	}
	if account != nil {
		account.Mu().RLock()
		explicit := len(account.Models) > 0
		account.Mu().RUnlock()
		if explicit {
			// An explicit but invalid whitelist is a configuration error, not a
			// reason to probe an unrelated fallback model.
			return nil
		}
	}
	return []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"}
}

// claudeProbeMaxTokens leaves room for thinking plus a short answer. Respect
// the configured output cap even when it only permits a thinking-only reply.
const claudeProbeMaxTokens = 4096

func claudeProbeTokenBudget(securityCfg auth.ClaudeSecurityConfig) int64 {
	if securityCfg.MaxOutputTokens > 0 && securityCfg.MaxOutputTokens < claudeProbeMaxTokens {
		return securityCfg.MaxOutputTokens
	}
	return claudeProbeMaxTokens
}

func buildClaudeModelProbePayload(model string, securityCfg auth.ClaudeSecurityConfig) []byte {
	model = strings.TrimSpace(model)
	return []byte(fmt.Sprintf(`{"model":%q,"max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"Reply with OK."}]}`, model, claudeProbeTokenBudget(securityCfg)))
}

func (h *Handler) probeClaudeAccountModel(ctx context.Context, account *auth.Account, model string) (string, string) {
	probeCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()
	if h == nil || h.store == nil {
		return modelProbeError, "Claude 探测缺少运行时账号池"
	}
	securityCfg := h.store.ClaudeSecurityConfig()
	resp, err := proxy.ExecuteClaudeMessagesRequest(
		probeCtx,
		account,
		buildClaudeModelProbePayload(model, securityCfg),
		h.store.ResolveProxyForAccount(account),
		nil,
		account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault()),
		securityCfg,
	)
	if err != nil {
		if msg, ok := batchTestContextFailure(probeCtx, err); ok {
			return modelProbeError, msg
		}
		return modelProbeError, err.Error()
	}
	if resp == nil {
		return modelProbeError, "Claude 探测未返回响应"
	}
	defer resp.Body.Close()
	// Model probing is an administrative read-only check. Do not feed the
	// response into the live usage/cooldown synchronizer: a model-specific 429
	// with a 100% window header must not quarantine the account (or affect a
	// different model) merely because an operator inspected availability.
	switch resp.StatusCode {
	case http.StatusOK:
		return readClaudeProbeStream(probeCtx, resp)
	case http.StatusTooManyRequests:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		lowerBody := strings.ToLower(string(body))
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.details.error_code").String()), "credits_required") ||
			strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()), "credits_required") ||
			(strings.Contains(lowerBody, "usage credits") && strings.Contains(lowerBody, "required")) {
			return modelProbeUnsupported, "上游模型需要 usage credits，当前账号套餐不可用"
		}
		return modelProbeThrottled, "上游返回 429 限流"
	case http.StatusBadRequest, http.StatusForbidden:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		if strings.Contains(strings.ToLower(string(body)), "model") && strings.Contains(strings.ToLower(string(body)), "not") {
			return modelProbeUnsupported, "账号套餐不支持该模型"
		}
		return modelProbeError, fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	default:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		return modelProbeError, fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// readClaudeProbeStream classifies native Anthropic Messages SSE without
// pretending message_start/message_stop are OpenAI response events.
func readClaudeProbeStream(ctx context.Context, resp *http.Response) (string, string) {
	status, detail := readClaudeMessagesStream(ctx, resp, nil)
	switch status {
	case "success":
		return modelProbeAvailable, "模型响应正常"
	case "rate_limited":
		return modelProbeThrottled, detail
	default:
		return modelProbeError, detail
	}
}

// readClaudeMessagesStream consumes native Anthropic Messages SSE. The
// callback receives only visible text deltas; it is optional for model probes
// and used by the account connection-test UI.
func readClaudeMessagesStream(ctx context.Context, resp *http.Response, onText func(string)) (string, string) {
	if resp == nil || resp.Body == nil {
		return "failed", "Claude 探测响应为空"
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !strings.Contains(contentType, "text/event-stream") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "failed", err.Error()
		}
		typ := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "type").String()))
		if typ == "message" {
			text := claudeMessageContentText(body)
			if text != "" && onText != nil {
				onText(text)
			}
			// thinking 模型可能只产出 thinking 块（预算被 thinking 吃光或
			// 模型自行决定不回答），同样证明账号与模型可用。
			if text == "" && !claudeMessageHasThinking(body) {
				return "failed", "Claude 探测未返回文本内容"
			}
			return "success", "测试通过"
		}
		if typ == "error" {
			if isClaudeProbeRateLimited(body) {
				return "rate_limited", formatClaudeProbeError(body, "上游返回限流错误")
			}
			return "failed", formatClaudeProbeError(body, "上游返回 Claude 错误")
		}
		return "failed", "Claude 探测响应格式未知"
	}
	hasContent := false
	hasThinking := false
	gotTerminal := false
	lastEvent := []byte(nil)
	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastEvent = append(lastEvent[:0], data...)
		typ := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "type").String()))
		switch typ {
		case "message":
			if text := claudeMessageContentText(data); text != "" {
				hasContent = true
				if onText != nil {
					onText(text)
				}
			}
			hasThinking = hasThinking || claudeMessageHasThinking(data)
			gotTerminal = true
			return false
		case "content_block_start":
			// thinking 块出现即说明模型已开始生成（adaptive thinking 模型
			// 可能整条响应只有 thinking，仍视为账号与模型可用）。
			if gjson.GetBytes(data, "content_block.type").String() == "thinking" {
				hasThinking = true
			}
		case "content_block_delta":
			switch gjson.GetBytes(data, "delta.type").String() {
			case "thinking_delta", "signature_delta":
				hasThinking = true
			}
			if text := gjson.GetBytes(data, "delta.text").String(); strings.TrimSpace(text) != "" {
				hasContent = true
				if onText != nil {
					onText(text)
				}
			}
		case "message_stop":
			gotTerminal = true
			return false
		case "error":
			gotTerminal = true
			return false
		}
		return true
	})
	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return "failed", msg
		}
		return "failed", readErr.Error()
	}
	if typ := strings.ToLower(strings.TrimSpace(gjson.GetBytes(lastEvent, "type").String())); typ == "error" {
		if isClaudeProbeRateLimited(lastEvent) {
			return "rate_limited", formatClaudeProbeError(lastEvent, "上游返回限流错误")
		}
		return "failed", formatClaudeProbeError(lastEvent, "上游返回 Claude 错误")
	}
	if !gotTerminal {
		return "failed", "Claude 探测未返回 message_stop"
	}
	// adaptive thinking 模型（opus-4.5/5、sonnet-4.5）在小预算下可能整条
	// 响应只有 thinking 块而 stop_reason=max_tokens；thinking 输出本身即证明
	// 账号与模型可用，不应误报"未返回文本内容"。
	if !hasContent && !hasThinking {
		return "failed", "Claude 探测未返回文本内容"
	}
	return "success", "测试通过"
}

func claudeMessageContentText(data []byte) string {
	var text strings.Builder
	for _, item := range gjson.GetBytes(data, "content").Array() {
		if item.Get("type").String() == "text" {
			text.WriteString(item.Get("text").String())
		}
	}
	return text.String()
}

// claudeMessageHasThinking reports whether a (non-streaming) Claude message
// carries thinking blocks, which still proves the model generated output.
func claudeMessageHasThinking(data []byte) bool {
	for _, item := range gjson.GetBytes(data, "content").Array() {
		if item.Get("type").String() == "thinking" {
			return true
		}
	}
	return false
}

func isClaudeProbeRateLimited(data []byte) bool {
	raw := strings.ToLower(string(data))
	return strings.Contains(raw, "rate_limit") || strings.Contains(raw, "rate limit") || strings.Contains(raw, "overloaded")
}

func formatClaudeProbeError(data []byte, fallback string) string {
	message := strings.TrimSpace(gjson.GetBytes(data, "error.message").String())
	if message == "" {
		message = fallback
	}
	return truncate(message, 200)
}

// readProbeStream 读取探测 SSE 流并分类，能从终止事件里识别出"账号不支持该模型"。
// 不回写任何账号状态。
func readProbeStream(ctx context.Context, resp *http.Response) (string, string) {
	hasContent := false
	gotTerminal := false
	outcome := ""
	detail := ""
	var lastUpstreamEvent []byte

	classifyFailure := func(data []byte, fallback string) (string, string) {
		if proxy.IsUsageLimitReachedError(data) {
			return modelProbeThrottled, formatUpstreamTestError(data, "上游用量耗尽")
		}
		if proxy.IsCodexModelUnsupportedError(data) {
			return modelProbeUnsupported, "账号套餐不支持该模型"
		}
		return modelProbeError, formatUpstreamTestError(data, fallback)
	}

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				outcome, detail = classifyFailure(data, "上游返回 "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				outcome = modelProbeError
				detail = formatNoOutputUpstreamError(data)
				return false
			}
			outcome = modelProbeAvailable
			detail = "探测通过"
			return false
		case "response.failed":
			gotTerminal = true
			outcome, detail = classifyFailure(data, "上游返回 response.failed")
			return false
		case "error":
			gotTerminal = true
			outcome, detail = classifyFailure(data, "上游返回 error 事件")
			return false
		}
		return true
	})

	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return modelProbeError, msg
		}
		return modelProbeError, readErr.Error()
	}
	if outcome != "" {
		return outcome, detail
	}
	if !gotTerminal {
		return modelProbeError, formatMissingTerminalUpstreamError(lastUpstreamEvent)
	}
	return modelProbeError, "上游探测未返回明确结果"
}
