package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// promptFilterFullTextMaxRunes limits the persisted redacted blocked-request text preview.
const promptFilterFullTextMaxRunes = 32000
const codexAmbientSuggestionClassifierPrefix = "Classify Codex ambient suggestion candidates for policy safety."
const codex55UnrestrictedInstructionsPatternName = "codex55_unrestricted_instructions"
const promptCyberPolicyMessage = "This content was flagged for possible cybersecurity risk. If this seems wrong, start a new session or rephrase the request."

func promptCyberPolicyError() *api.APIError {
	return api.NewAPIError(
		api.ErrorCode("cyber_policy"),
		promptCyberPolicyMessage,
		api.ErrorTypeInvalidRequest,
	)
}

func sendPromptCyberPolicyBlockedOpenAI(c *gin.Context) {
	api.SendErrorWithStatus(c, promptCyberPolicyError(), http.StatusBadRequest)
}

func (h *Handler) inspectPromptFilterOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	text := promptfilter.ExtractText(rawBody, endpoint, cfg.MaxTextLength)
	c.Set(contextPromptFilterText, text)
	if verdict, ok := codexAmbientSuggestionClassifierBypass(text, cfg); ok {
		h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
		return h.inspectSemanticReviewOpenAI(c, rawBody, endpoint, model)
	}
	verdict := promptfilter.Inspect(rawBody, endpoint, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg, endpoint)
	}
	var semanticHandled bool
	var semanticBlocked bool
	if handled, blocked := h.inspectHighRiskReviewDisagreement(c, verdict, text, endpoint, model, func() {
		sendPromptCyberPolicyBlockedOpenAI(c)
	}); handled {
		semanticHandled = true
		semanticBlocked = blocked
		if !blocked && verdict.Action == promptfilter.ActionBlock {
			verdict.Action = promptfilter.ActionAllow
			verdict.Reason = "semantic review cleared local high-risk prompt filter block"
		}
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if semanticBlocked {
		return true
	}
	if verdict.Action == promptfilter.ActionBlock {
		sendPromptCyberPolicyBlockedOpenAI(c)
		return true
	}
	if semanticHandled {
		return false
	}
	return h.inspectSemanticReviewOpenAI(c, rawBody, endpoint, model)
}

func (h *Handler) inspectPromptFilterTextOpenAI(c *gin.Context, text string, endpoint string, model string) bool {
	c.Set(contextPromptFilterText, text)
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	if verdict, ok := codexAmbientSuggestionClassifierBypass(text, cfg); ok {
		h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
		return h.inspectSemanticReviewTextOpenAI(c, text, endpoint, model)
	}
	verdict := promptfilter.InspectText(text, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg, endpoint)
	}
	var semanticHandled bool
	var semanticBlocked bool
	if handled, blocked := h.inspectHighRiskReviewDisagreement(c, verdict, text, endpoint, model, func() {
		sendPromptCyberPolicyBlockedOpenAI(c)
	}); handled {
		semanticHandled = true
		semanticBlocked = blocked
		if !blocked && verdict.Action == promptfilter.ActionBlock {
			verdict.Action = promptfilter.ActionAllow
			verdict.Reason = "semantic review cleared local high-risk prompt filter block"
		}
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if semanticBlocked {
		return true
	}
	if verdict.Action == promptfilter.ActionBlock {
		sendPromptCyberPolicyBlockedOpenAI(c)
		return true
	}
	if semanticHandled {
		return false
	}
	return h.inspectSemanticReviewTextOpenAI(c, text, endpoint, model)
}

func (h *Handler) inspectPromptFilterAnthropic(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	text := promptfilter.ExtractText(rawBody, endpoint, cfg.MaxTextLength)
	c.Set(contextPromptFilterText, text)
	if verdict, ok := codexAmbientSuggestionClassifierBypass(text, cfg); ok {
		h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
		return h.inspectSemanticReviewAnthropic(c, rawBody, endpoint, model)
	}
	verdict := promptfilter.Inspect(rawBody, endpoint, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = h.reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg, endpoint)
	}
	var semanticHandled bool
	var semanticBlocked bool
	if handled, blocked := h.inspectHighRiskReviewDisagreement(c, verdict, text, endpoint, model, func() {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", promptCyberPolicyMessage)
	}); handled {
		semanticHandled = true
		semanticBlocked = blocked
		if !blocked && verdict.Action == promptfilter.ActionBlock {
			verdict.Action = promptfilter.ActionAllow
			verdict.Reason = "semantic review cleared local high-risk prompt filter block"
		}
	}
	h.logPromptFilterVerdict(c, endpoint, model, "local_filter", "", verdict)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if semanticBlocked {
		return true
	}
	if verdict.Action == promptfilter.ActionBlock {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", promptCyberPolicyMessage)
		return true
	}
	if semanticHandled {
		return false
	}
	return h.inspectSemanticReviewAnthropic(c, rawBody, endpoint, model)
}

func (h *Handler) logPromptFilterVerdict(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict) {
	if h == nil || h.db == nil || !verdict.Enabled {
		return
	}
	if source == "local_filter" && len(verdict.Matched) == 0 && !verdict.Reviewed {
		return
	}
	if h.store != nil {
		cfg := h.store.GetPromptFilterConfig()
		if source == "local_filter" && !cfg.LogMatches {
			return
		}
	}
	input := &database.PromptFilterLogInput{
		Source:          source,
		Endpoint:        endpoint,
		Model:           model,
		Action:          verdict.Action,
		Mode:            verdict.Mode,
		Score:           verdict.Score,
		Threshold:       verdict.Threshold,
		MatchedPatterns: promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:     promptfilter.RedactedPreview(verdict.TextPreview, 500),
		ClientIP:        c.ClientIP(),
		ErrorCode:       errorCode,
		ReviewModel:     verdict.ReviewModel,
		ReviewFlagged:   verdict.ReviewFlagged,
		ReviewError:     verdict.ReviewError,
	}
	// 被拦截（block）的请求仅记录脱敏后的检查文本预览，便于排查触发原因，
	// 同时避免把 Authorization/API Key/token 等敏感值持久化到日志。
	if verdict.Action == promptfilter.ActionBlock {
		input.FullText = promptfilter.RedactedPreview(verdict.FullText, promptFilterFullTextMaxRunes)
	}
	populatePromptFilterAPIKeyMeta(c, input)
	input.ClientRequestID = strings.TrimSpace(c.GetHeader("X-Client-Request-Id"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}

func (h *Handler) logUpstreamCyberPolicy(c *gin.Context, endpoint string, model string, body []byte) {
	if h == nil || h.store == nil {
		return
	}
	errorCode := upstreamCyberPolicyCode(body)
	if errorCode == "" {
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	reqText := ""
	if v, ok := c.Get(contextPromptFilterText); ok {
		if s, ok2 := v.(string); ok2 {
			reqText = strings.TrimSpace(s)
		}
	}
	upstreamReason := promptfilter.RedactSensitive(string(body))
	fullText := upstreamReason
	if reqText != "" {
		fullText = `【上游拦截原因】
` + upstreamReason + `

【请求内容】
` + reqText
	}
	verdict := promptfilter.Verdict{
		Enabled:     true,
		Mode:        cfg.Mode,
		Action:      promptfilter.ActionBlock,
		Score:       0,
		Threshold:   cfg.Threshold,
		Reason:      "upstream returned cyber policy",
		TextPreview: reqText,
		FullText:    fullText,
	}
	h.logPromptFilterVerdict(c, endpoint, model, "upstream_cyber_policy", errorCode, verdict)
}

func upstreamCyberPolicyCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	raw := string(body)
	for _, path := range []string{"codex_error_info", "error.codex_error_info", "error.code", "code"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); strings.EqualFold(value, "cyber_policy") {
			return "cyber_policy"
		}
	}
	if strings.Contains(strings.ToLower(raw), "cyber_policy") || strings.Contains(strings.ToLower(raw), "cyber security risk") {
		return "cyber_policy"
	}
	return ""
}

func populatePromptFilterAPIKeyMeta(c *gin.Context, input *database.PromptFilterLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func shouldReviewPromptFilterVerdict(verdict promptfilter.Verdict, cfg promptfilter.Config) bool {
	if promptFilterVerdictIsFinal(verdict) {
		return false
	}
	review := promptfilter.NormalizeReviewConfig(cfg.Review)
	if !review.Ready() {
		return false
	}
	if verdict.Action == promptfilter.ActionWarn || verdict.Action == promptfilter.ActionBlock {
		return true
	}
	return review.All && verdict.Action == promptfilter.ActionAllow
}

func promptFilterVerdictIsFinal(verdict promptfilter.Verdict) bool {
	for _, match := range verdict.Matched {
		if match.Name == codex55UnrestrictedInstructionsPatternName {
			return true
		}
	}
	return false
}

func promptFilterAllowedHighRisk(verdict promptfilter.Verdict) bool {
	if promptFilterVerdictIsFinal(verdict) {
		return false
	}
	return verdict.Action == promptfilter.ActionAllow &&
		promptfilter.IsHighRiskReviewVerdict(verdict)
}

func promptFilterBlockedByLocalHighRisk(verdict promptfilter.Verdict) bool {
	if promptFilterVerdictIsFinal(verdict) {
		return false
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	if promptfilter.IsHighRiskReviewVerdict(verdict) {
		return true
	}
	threshold := verdict.Threshold
	if threshold <= 0 {
		threshold = promptfilter.DefaultThreshold
	}
	return verdict.Score >= threshold || verdict.RawScore >= threshold
}

func (h *Handler) inspectHighRiskReviewDisagreement(c *gin.Context, verdict promptfilter.Verdict, text string, endpoint string, model string, writeBlock func()) (bool, bool) {
	if !promptFilterAllowedHighRisk(verdict) && !promptFilterBlockedByLocalHighRisk(verdict) {
		return false, false
	}
	blocked := h.inspectSemanticReviewDisagreementText(c, text, endpoint, model, writeBlock)
	return true, blocked
}

func codexAmbientSuggestionClassifierBypass(text string, cfg promptfilter.Config) (promptfilter.Verdict, bool) {
	if !isCodexAmbientSuggestionClassifier(text) {
		return promptfilter.Verdict{}, false
	}
	cfg = promptfilter.NormalizeConfig(cfg)
	return promptfilter.Verdict{
		Enabled:   cfg.Enabled,
		Mode:      cfg.Mode,
		Action:    promptfilter.ActionAllow,
		Score:     0,
		Threshold: cfg.Threshold,
		Matched: []promptfilter.Match{{
			Name:     "internal_policy_classifier_bypass",
			Weight:   0,
			Category: "meta_safety",
		}},
		Reason:      "allowed internal Codex ambient suggestion policy classifier",
		TextPreview: text,
		FullText:    text,
	}, true
}

func isCodexAmbientSuggestionClassifier(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, codexAmbientSuggestionClassifierPrefix) {
		return false
	}
	lower := strings.ToLower(trimmed)
	required := []string{
		"ambient suggestion candidates",
		"suggestion_id:",
		"return a json object",
		"\"exclude\"",
		"only output the json object",
	}
	for _, needle := range required {
		if !strings.Contains(lower, needle) {
			return false
		}
	}
	return true
}

func (h *Handler) reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config, endpoint string) promptfilter.Verdict {
	flagged, model, err := promptfilter.DefaultReviewClient.ReviewText(ctx, text, cfg.Review, endpoint)
	verdict = promptfilter.ApplyReviewResult(verdict, flagged, model, err, cfg.Review)
	if err == nil && !flagged && len(verdict.Matched) == 0 {
		verdict.Reason = "prompt review cleared request"
	}
	if err == nil && flagged {
		switch promptfilter.NormalizeConfig(cfg).Mode {
		case promptfilter.ModeBlock:
			verdict.Action = promptfilter.ActionBlock
		case promptfilter.ModeWarn:
			verdict.Action = promptfilter.ActionWarn
		}
		verdict.Reason = "prompt review flagged request"
	}
	return verdict
}
