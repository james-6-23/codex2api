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

func (h *Handler) inspectPromptFilterOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if c != nil && c.GetBool("prompt_intelligence_internal") {
		return false
	}
	if h == nil || h.store == nil {
		return false
	}
	signedBody := ingressRequestBody(c, rawBody)
	evaluation := h.evaluatePromptGuard(c, rawBody, signedBody, endpoint, model, promptfilter.TransportHTTP)
	cfg := evaluation.Config
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, rawBody, endpoint, model, signedBody) {
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		"Request contains content blocked by prompt filter",
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterTextOpenAI(c *gin.Context, text string, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	evaluation := h.evaluatePromptGuardText(c, text, endpoint, model)
	cfg := evaluation.Config
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, []byte(text), endpoint, model, ingressRequestBody(c, nil)) {
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		"Request contains content blocked by prompt filter",
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterAnthropic(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	signedBody := ingressRequestBody(c, rawBody)
	evaluation := h.evaluatePromptGuard(c, rawBody, signedBody, endpoint, model, promptfilter.TransportHTTP)
	cfg := evaluation.Config
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
	}
	if verdict.Action == promptfilter.ActionBlock {
		if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, rawBody, endpoint, model, signedBody) {
			return true
		}
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request contains content blocked by prompt filter")
		return true
	}
	return false
}

func (h *Handler) logPromptFilterVerdict(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict) {
	h.logPromptFilterVerdictWithDecision(c, endpoint, model, source, errorCode, verdict, nil, nil)
}

func (h *Handler) logPromptGuardEvaluation(c *gin.Context, endpoint string, model string, source string, errorCode string, evaluation promptGuardEvaluation) {
	h.logPromptFilterVerdictWithDecision(c, endpoint, model, source, errorCode, evaluation.Verdict, &evaluation.Decision, &evaluation.Envelope)
}

func (h *Handler) logPromptFilterVerdictWithDecision(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict, decision *promptfilter.Decision, envelope *promptfilter.RequestEnvelope) {
	if h == nil || h.db == nil || !verdict.Enabled {
		return
	}
	if source == "local_filter" && len(verdict.Matched) == 0 && verdict.Action == promptfilter.ActionAllow && verdict.ReviewError == "" && verdict.ReviewModel == "" {
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
	if envelope != nil {
		if envelope.Protocol != promptfilter.ProtocolUnknown {
			input.Protocol = string(envelope.Protocol)
		}
		if envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
			input.Provider = string(envelope.ModelFamily)
		}
	}
	h.populateVerifiedNewAPIAuditMeta(c, input)
	if decision != nil {
		input.AuditScore = decision.AuditScore
		input.PolicyProfile = decision.Profile
		input.ReasonCode = decision.ReasonCode
		input.PrimaryOrigin = string(decision.PrimaryOrigin)
		input.StrikeEligible = decision.StrikeEligible
	}
	// 被拦截（block）的请求仅记录脱敏后的检查文本预览，便于排查触发原因，
	// 同时避免把 Authorization/API Key/token 等敏感值持久化到日志。
	if verdict.Action == promptfilter.ActionBlock {
		input.FullText = promptfilter.RedactedPreview(verdict.FullText, promptFilterFullTextMaxRunes)
	}
	populatePromptFilterAPIKeyMeta(c, input)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}

func (h *Handler) populateVerifiedNewAPIAuditMeta(c *gin.Context, input *database.PromptFilterLogInput) {
	if h == nil || h.store == nil || c == nil || input == nil {
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	if !verified || !policyContext.AuditMetaVerified {
		return
	}
	if policyContext.Audit.Endpoint != "" {
		input.Endpoint = policyContext.Audit.Endpoint
	}
	if protocol := strings.TrimSpace(policyContext.Audit.Protocol); protocol != "" && !strings.EqualFold(protocol, string(promptfilter.ProtocolUnknown)) {
		input.Protocol = protocol
	}
	if provider := strings.TrimSpace(policyContext.Audit.Provider); provider != "" && !strings.EqualFold(provider, string(promptfilter.ModelFamilyUnknown)) {
		input.Provider = provider
	}
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
	verdict := promptfilter.Verdict{
		Enabled:   true,
		Mode:      cfg.Mode,
		Action:    promptfilter.ActionBlock,
		Score:     0,
		Threshold: cfg.Threshold,
		Reason:    "upstream returned cyber policy",
		// 上游 cyber_policy 没有本地提取文本，把脱敏后的上游错误体作为「详细内容」记录，
		// 方便在日志里看清触发详情，同时避免持久化敏感字段。
		FullText: promptfilter.RedactSensitive(string(body)),
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
	if verdict.Action != promptfilter.ActionWarn && verdict.Action != promptfilter.ActionBlock {
		return false
	}
	return promptfilter.NormalizeReviewConfig(cfg.Review).Ready()
}

func (h *Handler) reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	flagged, model, err := promptfilter.DefaultReviewClient.ReviewText(ctx, text, cfg.Review)
	return promptfilter.ApplyReviewResult(verdict, flagged, model, err, cfg.Review)
}
