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

const (
	upstreamCyberPolicyUserMessage       = "此内容因可能存在网络安全风险而被标记，本次已记录。请重新表述请求；再次触发可能会停用账号。如果确认是误判，请联系管理员。"
	upstreamCyberPolicyLockedUserMessage = "此内容因可能存在网络安全风险而被标记，本次已记录并锁定当前对话。请新建对话后继续；再次触发可能会停用账号。如果确认是误判，请联系管理员解锁。"
	upstreamBioPolicyUserMessage         = "此内容因可能存在生物安全风险而被标记，本次已记录。请重新表述请求；再次触发可能会停用账号。如果确认是误判，请联系管理员。"
	upstreamBioPolicyLockedUserMessage   = "此内容因可能存在生物安全风险而被标记，本次已记录并锁定当前对话。请新建对话后继续；再次触发可能会停用账号。如果确认是误判，请联系管理员解锁。"
	defaultLocalPromptBlockMessage       = "Request contains content blocked by prompt filter"
)

func localPromptBlockMessage(cfg promptfilter.Config) string {
	if message := strings.TrimSpace(cfg.Advanced.Enforcement.LocalBlockMessage); message != "" {
		return message
	}
	return defaultLocalPromptBlockMessage
}

// promptFilterFullTextMaxRunes limits the persisted redacted blocked-request text preview.
const promptFilterFullTextMaxRunes = 32000

// promptFilterMatchContextMaxRunes keeps audit evidence useful without storing
// an entire tool result, session transcript, or attachment in the log table.
const promptFilterMatchContextMaxRunes = 2000

func (h *Handler) inspectPromptFilterOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	return h.inspectPromptFilterOpenAIWithBlockWriter(c, rawBody, endpoint, model, nil)
}

// InspectPromptFilterOpenAI exposes the same V1 Adapter/Guard path used by the
// synchronous proxy handlers to in-process V1 entry points such as async image
// jobs. writeBlock may preserve an endpoint-specific error envelope; verified
// NewAPI policy decisions still take precedence when they own the response.
func (h *Handler) InspectPromptFilterOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string, writeBlock func(*gin.Context)) bool {
	var messageWriter func(*gin.Context, string)
	if writeBlock != nil {
		messageWriter = func(c *gin.Context, _ string) { writeBlock(c) }
	}
	return h.InspectPromptFilterOpenAIWithBlockMessage(c, rawBody, endpoint, model, messageWriter)
}

// InspectPromptFilterOpenAIWithBlockMessage is the message-aware variant for
// endpoints that keep their own error envelope. The callback receives the
// configured local block message, or an empty string when the endpoint should
// retain its existing default.
func (h *Handler) InspectPromptFilterOpenAIWithBlockMessage(c *gin.Context, rawBody []byte, endpoint string, model string, writeBlock func(*gin.Context, string)) bool {
	h.capturePromptRequestIngress(c, rawBody)
	return h.inspectPromptFilterOpenAIWithBlockWriter(c, rawBody, endpoint, model, writeBlock)
}

func (h *Handler) inspectPromptFilterOpenAIWithBlockWriter(c *gin.Context, rawBody []byte, endpoint string, model string, writeBlock func(*gin.Context, string)) bool {
	if c != nil && c.GetBool("prompt_intelligence_internal") {
		return false
	}
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	signedBody := ingressRequestBody(c, rawBody)
	if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, signedBody) {
		return true
	}
	if h.rejectLockedPromptConversation(c, cfg, signedBody, rawBody, endpoint, model) {
		return true
	}
	if passiveInternalRequestAuthorized(c) {
		// Field-classified internal turns contain transcript or original
		// user text by design. Do not recursively filter it as a fresh user prompt.
		return false
	}
	// Skip envelope construction and body traversal when neither the local
	// filter nor a body-dependent extension is enabled (issue #417).
	if !promptfilter.RequiresRequestText(cfg) {
		return false
	}
	evaluation := h.evaluatePromptGuardWithConfig(c, cfg, rawBody, signedBody, endpoint, model, promptfilter.TransportHTTP)
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", promptFilterWarningMessage(evaluation))
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	// 在发往上游供应商之前锁定会话:本次已被拦下,后续绕过本地规则的等价变形
	// 也不再有机会打到上游。
	h.lockPromptConversationOnLocalBlock(c, cfg, signedBody, endpoint, model, evaluation.Decision, verdict)
	if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, rawBody, endpoint, model, signedBody) {
		return true
	}
	if writeBlock != nil {
		writeBlock(c, strings.TrimSpace(cfg.Advanced.Enforcement.LocalBlockMessage))
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		localPromptBlockMessage(cfg),
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterTextOpenAI(c *gin.Context, text string, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil)) {
		return true
	}
	if h.rejectLockedPromptConversation(c, cfg, ingressRequestBody(c, nil), []byte(text), endpoint, model) {
		return true
	}
	if !promptfilter.RequiresRequestText(cfg) {
		return false
	}
	evaluation := h.evaluatePromptGuardTextWithConfig(c, cfg, text, endpoint, model)
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", promptFilterWarningMessage(evaluation))
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	h.lockPromptConversationOnLocalBlock(c, cfg, ingressRequestBody(c, nil), endpoint, model, evaluation.Decision, verdict)
	if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, []byte(text), endpoint, model, ingressRequestBody(c, nil)) {
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("prompt_blocked"),
		localPromptBlockMessage(cfg),
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
	return true
}

func (h *Handler) inspectPromptFilterAnthropic(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	signedBody := ingressRequestBody(c, rawBody)
	if apiErr := h.requiredNewAPIIdentityError(c, cfg.Advanced.NewAPI, signedBody); apiErr != nil {
		sendAnthropicError(c, http.StatusUnauthorized, "authentication_error", apiErr.Message)
		return true
	}
	if h.rejectLockedPromptConversation(c, cfg, signedBody, rawBody, endpoint, model) {
		return true
	}
	if passiveInternalRequestAuthorized(c) {
		return false
	}
	if !promptfilter.RequiresRequestText(cfg) {
		return false
	}
	evaluation := h.evaluatePromptGuardWithConfig(c, cfg, rawBody, signedBody, endpoint, model, promptfilter.TransportHTTP)
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", promptFilterWarningMessage(evaluation))
	}
	if verdict.Action == promptfilter.ActionBlock {
		h.lockPromptConversationOnLocalBlock(c, cfg, signedBody, endpoint, model, evaluation.Decision, verdict)
		if h.sendNewAPIPolicyDecision(c, cfg, evaluation.Decision, verdict, rawBody, endpoint, model, signedBody) {
			return true
		}
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", localPromptBlockMessage(cfg))
		return true
	}
	return false
}

func promptFilterWarningMessage(evaluation promptGuardEvaluation) string {
	if reason := strings.TrimSpace(evaluation.Verdict.Reason); reason != "" {
		return reason
	}
	if reasonCode := strings.TrimSpace(evaluation.Decision.ReasonCode); reasonCode != "" {
		return reasonCode
	}
	return "prompt_policy_warning"
}

func (h *Handler) logPromptFilterVerdict(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict) {
	h.logPromptFilterVerdictWithDecision(c, endpoint, model, source, errorCode, verdict, nil, nil)
}

func (h *Handler) logPromptGuardEvaluation(c *gin.Context, endpoint string, model string, source string, errorCode string, evaluation promptGuardEvaluation) {
	h.capturePromptRuleLearningEvidence(c, endpoint, model, evaluation)
	h.logPromptFilterVerdictWithDecision(c, endpoint, model, source, errorCode, evaluation.Verdict, &evaluation.Decision, &evaluation.Envelope)
	h.scheduleDeferredPromptGuardAudit(c, endpoint, model, source, errorCode, evaluation)
}

func (h *Handler) logPromptFilterVerdictWithDecision(c *gin.Context, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict, decision *promptfilter.Decision, envelope *promptfilter.RequestEnvelope) {
	if h == nil || h.db == nil || !verdict.Enabled {
		return
	}
	// Full model review runs on every extractable request, but a clean pass with
	// no local evidence must not turn the prompt log into a full traffic archive.
	// Persist flagged results, failures, local matches, and explicit audit states.
	if source == "local_filter" && len(verdict.Matched) == 0 && verdict.Action == promptfilter.ActionAllow && verdict.ReviewError == "" && !verdict.Reviewed && !promptFilterDecisionRequiresAudit(decision) {
		return
	}
	logMatches := true
	cfg := promptfilter.DefaultConfig()
	if h.store != nil {
		cfg = h.promptFilterConfigForRequest(c)
		logMatches = cfg.LogMatches
		if source == "local_filter" && !logMatches {
			return
		}
	}
	auditContext := h.capturePromptFilterAuditContext(c)
	if verdict.Action == promptfilter.ActionBlock && decision != nil && auditContext.NewAPIPolicyStatus == "verified" {
		if cached, exists := c.Get(newAPIPolicyMetaContextKey); exists {
			if policyContext, ok := cached.(verifiedNewAPIPolicyContext); ok {
				metadata := buildNewAPIPolicyDecisionMetadataWithSecret(
					policyContext.Identity,
					*decision,
					verdict,
					cfg,
					[]byte(verdict.FullText),
					endpoint,
					model,
					"",
					policyContext.VerificationSecret,
				)
				auditContext.NewAPIPolicyStatus = "signed_response"
				auditContext.NewAPIDecisionID = metadata.DecisionID
			}
		}
	}
	input := h.buildPromptFilterLogInput(auditContext, endpoint, model, source, errorCode, verdict, decision, envelope, logMatches)
	if input == nil {
		return
	}
	priority := database.PromptFilterLogPriorityLow
	if verdict.Action == promptfilter.ActionWarn || verdict.Action == promptfilter.ActionBlock || source == "upstream_cyber_policy" {
		priority = database.PromptFilterLogPriorityHigh
	}
	// Audit persistence must never delay account selection, upstream connect, or
	// first-token forwarding. Saturation is observable through DB queue metrics
	// and deliberately has no synchronous fallback.
	_ = h.db.EnqueuePromptFilterLog(input, priority)
}

type promptFilterAuditContext struct {
	ClientIP             string
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	Endpoint             string
	Protocol             string
	Provider             string
	RequestCorrelationID string
	NewAPIPolicyStatus   string
	NewAPIPlatform       string
	NewAPIChannelID      int
	NewAPIUserID         string
	NewAPIUserName       string
	NewAPIUserEmail      string
	NewAPIUserGroup      string
	NewAPIRequestID      string
	NewAPIDecisionID     string
	SessionHash          string
	RootSessionHash      string
	ClientIPHash         string
}

func (h *Handler) capturePromptFilterAuditContext(c *gin.Context) promptFilterAuditContext {
	if c == nil {
		return promptFilterAuditContext{}
	}
	input := &database.PromptFilterLogInput{ClientIP: c.ClientIP()}
	// Logging must never initiate signature replay-cache I/O. Prompt evaluation
	// has already verified and cached metadata when it was needed; response-side
	// cyber-policy logging simply omits metadata if no verified context exists.
	h.populateCachedVerifiedNewAPIAuditMeta(c, input)
	populatePromptFilterAPIKeyMeta(c, input)
	newAPIStatus, policyContext := h.cachedNewAPIPolicyAuditState(c)
	sessionHash := ""
	rootSessionHash := ""
	newAPIChannelID := 0
	newAPIUserName, newAPIUserEmail, newAPIUserGroup := "", "", ""
	if (newAPIStatus == "verified" || newAPIStatus == "signed_response") && policyContext.MetaVerified {
		newAPIChannelID = policyContext.Meta.ChannelID
		sessionHash = hashRiskIdentity(policyContext.Meta.SessionFingerprint)
		newAPIUserName = policyContext.Meta.UserName
		newAPIUserEmail = policyContext.Meta.UserEmail
		newAPIUserGroup = policyContext.Meta.UserGroup
	} else {
		// Conversation locking falls back to the Codex-local identity whenever a
		// verified platform/session identity is unavailable. Audit correlation must
		// make the same choice for optional unsigned bindings, disabled bindings,
		// and failed optional verification—not only for completely unbound keys.
		sessionHash = promptConversationLockFallbackSessionHash(c)
	}
	// Preserve exact-session accounting only when root resolution is not
	// authoritative. A signed root-capable sender may explicitly report that
	// the root is unavailable; that state must not fall back to the leaf and
	// temporarily consume an operational account window.
	rootSessionHash = ""
	// RootSessionHash is deliberately operational-only. Prompt risk profiles
	// and CYB evidence keep the exact leaf SessionHash, while account/session
	// observations collapse hidden Guardian and sub-agent leaves to the main
	// user-visible task.
	if newAPIStatus == "verified" || newAPIStatus == "signed_response" || newAPIStatus == "unbound" {
		rootBody := ingressRequestBody(c, nil)
		if len(rootBody) == 0 && isResponsesWebSocketUpgradeRequest(c.Request) {
			// WebSocket request headers belong to the connection, while the root
			// session graph normally lives in the current response.create frame.
			// The immutable HTTP ingress body is intentionally absent for WS, so
			// use only this frame's already-cached body for operational grouping.
			if frameBody, ok := rawRequestBodyFromContext(c); ok {
				rootBody = frameBody
			}
		}
		rootIdentity := h.resolveRequestRootSessionIdentityForContext(c, rootBody)
		if rootIdentity.stable && !rootIdentity.conflict && rootIdentity.sessionID != "" {
			rootSessionHash = hashRiskIdentity(rootIdentity.sessionID)
		} else if !rootIdentity.authoritative {
			// Unbound/legacy traffic keeps exact-session compatibility. A
			// root-capable signed sender can instead report that the current WS
			// frame has no usable root yet; in that case leave the operational
			// identity empty so its leaf is not temporarily counted as a second
			// account window before a later frame binds the real root.
			rootSessionHash = sessionHash
		}
	}
	clientIP := input.ClientIP
	if (newAPIStatus == "verified" || newAPIStatus == "signed_response") && strings.TrimSpace(policyContext.Identity.ClientIP) != "" {
		clientIP = policyContext.Identity.ClientIP
	}
	return promptFilterAuditContext{
		ClientIP:             input.ClientIP,
		APIKeyID:             input.APIKeyID,
		APIKeyName:           input.APIKeyName,
		APIKeyMasked:         input.APIKeyMasked,
		Endpoint:             input.Endpoint,
		Protocol:             input.Protocol,
		Provider:             input.Provider,
		RequestCorrelationID: ensurePromptPolicyRequestCorrelationID(c),
		NewAPIPolicyStatus:   newAPIStatus,
		NewAPIPlatform:       policyContext.Platform,
		NewAPIChannelID:      newAPIChannelID,
		NewAPIUserID:         policyContext.Identity.UserID,
		NewAPIUserName:       newAPIUserName,
		NewAPIUserEmail:      newAPIUserEmail,
		NewAPIUserGroup:      newAPIUserGroup,
		NewAPIRequestID:      policyContext.Identity.RequestID,
		SessionHash:          sessionHash,
		RootSessionHash:      rootSessionHash,
		ClientIPHash:         hashRiskIdentity(clientIP),
	}
}

func (h *Handler) cachedNewAPIPolicyAuditState(c *gin.Context) (string, verifiedNewAPIPolicyContext) {
	if c == nil || h == nil || h.store == nil {
		return "unbound", verifiedNewAPIPolicyContext{}
	}
	binding, bound := h.resolvePromptFilterNewAPIBinding(c)
	if !bound {
		return "unbound", verifiedNewAPIPolicyContext{}
	}
	if !binding.Enabled {
		return "binding_disabled", verifiedNewAPIPolicyContext{Platform: normalizedNewAPIPlatform(binding.PlatformCode)}
	}
	if cached, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		if policyContext, ok := cached.(verifiedNewAPIPolicyContext); ok && policyContext.APIKeyID == requestAPIKeyID(c) {
			return "verified", policyContext
		}
	}
	if strings.TrimSpace(c.GetHeader("X-NewAPI-Signature")) == "" {
		return "unsigned_request", verifiedNewAPIPolicyContext{Platform: normalizedNewAPIPlatform(binding.PlatformCode)}
	}
	return "verification_failed", verifiedNewAPIPolicyContext{Platform: normalizedNewAPIPlatform(binding.PlatformCode)}
}

func (h *Handler) logPromptFilterVerdictWithAuditContext(_ context.Context, auditContext promptFilterAuditContext, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict, decision *promptfilter.Decision, envelope *promptfilter.RequestEnvelope, logMatches bool) error {
	if h == nil || h.db == nil {
		return nil
	}
	input := h.buildPromptFilterLogInput(auditContext, endpoint, model, source, errorCode, verdict, decision, envelope, logMatches)
	if input == nil {
		return nil
	}
	// Deferred shadow evaluation already runs outside the request, but its
	// persistence still shares the same bounded low-priority queue so it cannot
	// compete synchronously with block/warn audit writes for database latency.
	_ = h.db.EnqueuePromptFilterLog(input, database.PromptFilterLogPriorityLow)
	return nil
}

func (h *Handler) buildPromptFilterLogInput(auditContext promptFilterAuditContext, endpoint string, model string, source string, errorCode string, verdict promptfilter.Verdict, decision *promptfilter.Decision, envelope *promptfilter.RequestEnvelope, logMatches bool) *database.PromptFilterLogInput {
	if h == nil || !verdict.Enabled {
		return nil
	}
	if source == "local_filter" && len(verdict.Matched) == 0 && verdict.Action == promptfilter.ActionAllow && verdict.ReviewError == "" && !verdict.Reviewed && !promptFilterDecisionRequiresAudit(decision) {
		return nil
	}
	if source == "local_filter" && !logMatches {
		return nil
	}
	input := &database.PromptFilterLogInput{
		Source:               source,
		Endpoint:             endpoint,
		Model:                model,
		Action:               verdict.Action,
		Mode:                 verdict.Mode,
		Score:                verdict.Score,
		Threshold:            verdict.Threshold,
		MatchedPatterns:      promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:          promptfilter.RedactedPreview(verdict.TextPreview, 500),
		MatchContext:         promptfilter.RedactedPreview(verdict.MatchContext, promptFilterMatchContextMaxRunes),
		ClientIP:             auditContext.ClientIP,
		ErrorCode:            errorCode,
		ReviewModel:          verdict.ReviewModel,
		ReviewFlagged:        verdict.ReviewFlagged,
		ReviewError:          verdict.ReviewError,
		Reviewed:             verdict.Reviewed,
		ReviewConfidence:     verdict.ReviewConfidence,
		ReviewThreshold:      verdict.ReviewThreshold,
		ReviewReason:         promptfilter.RedactedPreview(verdict.ReviewReason, 500),
		ReviewEndpoint:       verdict.ReviewEndpoint,
		ReviewRequestMode:    verdict.ReviewRequestMode,
		ReviewLatencyMS:      verdict.ReviewLatencyMS,
		RequestCorrelationID: auditContext.RequestCorrelationID,
		NewAPIPolicyStatus:   auditContext.NewAPIPolicyStatus,
		NewAPIPlatform:       auditContext.NewAPIPlatform,
		NewAPIUserID:         auditContext.NewAPIUserID,
		NewAPIUserName:       auditContext.NewAPIUserName,
		NewAPIUserEmail:      auditContext.NewAPIUserEmail,
		NewAPIUserGroup:      auditContext.NewAPIUserGroup,
		NewAPIRequestID:      auditContext.NewAPIRequestID,
		NewAPIDecisionID:     auditContext.NewAPIDecisionID,
		SessionHash:          auditContext.SessionHash,
		ClientIPHash:         auditContext.ClientIPHash,
	}
	if envelope != nil {
		if envelope.Protocol != promptfilter.ProtocolUnknown {
			input.Protocol = string(envelope.Protocol)
		}
		if envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
			input.Provider = string(envelope.ModelFamily)
		}
	}
	if auditContext.Endpoint != "" {
		input.Endpoint = auditContext.Endpoint
	}
	if auditContext.Protocol != "" {
		input.Protocol = auditContext.Protocol
	}
	if auditContext.Provider != "" {
		input.Provider = auditContext.Provider
	}
	if decision != nil {
		input.AuditScore = decision.AuditScore
		input.PolicyProfile = decision.Profile
		input.ReasonCode = decision.ReasonCode
		input.PrimaryOrigin = string(decision.PrimaryOrigin)
		input.StrikeEligible = decision.StrikeEligible
	}
	// The adapter-unclassified audit otherwise carries no evidence. Surface the
	// unrecognized typed-payload names (schema-only, no user content) so operators
	// can see which future block/item type needs an explicit adapter mapping.
	if decision != nil && decision.ReasonCode == promptfilter.ReasonCodeAdapterUnclassified &&
		input.MatchContext == "" && envelope != nil && len(envelope.AdapterUnclassifiedTypes) > 0 {
		input.MatchContext = "unclassified_types: " + strings.Join(envelope.AdapterUnclassifiedTypes, ", ")
	}
	// 被拦截（block）的请求仅记录脱敏后的检查文本预览，便于排查触发原因，
	// 同时避免把 Authorization/API Key/token 等敏感值持久化到日志。
	if verdict.Action == promptfilter.ActionBlock {
		input.FullText = promptfilter.RedactedPreview(verdict.FullText, promptFilterFullTextMaxRunes)
	}
	input.APIKeyID = auditContext.APIKeyID
	input.APIKeyName = auditContext.APIKeyName
	input.APIKeyMasked = auditContext.APIKeyMasked
	return input
}

func promptFilterDecisionRequiresAudit(decision *promptfilter.Decision) bool {
	return decision != nil && decision.ReasonCode == promptfilter.ReasonCodeAdapterUnclassified
}

func (h *Handler) populateVerifiedNewAPIAuditMeta(c *gin.Context, input *database.PromptFilterLogInput) {
	if h == nil || h.store == nil || c == nil || input == nil {
		return
	}
	cfg := h.promptFilterConfigForRequest(c)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	if !verified || !policyContext.AuditMetaVerified {
		return
	}
	applyVerifiedNewAPIAuditMeta(policyContext, input)
}

func (h *Handler) populateCachedVerifiedNewAPIAuditMeta(c *gin.Context, input *database.PromptFilterLogInput) {
	if h == nil || c == nil || input == nil {
		return
	}
	cached, exists := c.Get(newAPIPolicyMetaContextKey)
	if !exists {
		return
	}
	policyContext, ok := cached.(verifiedNewAPIPolicyContext)
	if !ok || !policyContext.AuditMetaVerified {
		return
	}
	applyVerifiedNewAPIAuditMeta(policyContext, input)
}

func applyVerifiedNewAPIAuditMeta(policyContext verifiedNewAPIPolicyContext, input *database.PromptFilterLogInput) {
	if input == nil {
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

func (h *Handler) logUpstreamCyberPolicy(c *gin.Context, endpoint string, model string, body []byte, attempts ...upstreamCyberPolicyAttempt) (string, bool) {
	errorCode := upstreamCyberPolicyCode(responseFailedErrorBody(body))
	if errorCode == "" {
		return "", false
	}
	attempt := upstreamCyberPolicyAttempt{}
	if len(attempts) > 0 {
		attempt = attempts[0]
	}
	incidentID, accepted := h.enqueueUpstreamCyberPolicyEvidence(c, endpoint, model, errorCode, body, attempt)
	// NewAPI owns strike counting and account punishment. Emission intentionally
	// does not depend on the local async audit queue: a temporary Codex2API audit
	// storage failure must not turn a verified upstream CYB into an untracked one.
	metadata, delegated := h.emitNewAPIUpstreamCyberPolicyDecision(c, endpoint, model, body)
	if delegated {
		// 明确的上游 CYB 始终保留会话锁与用户冷却；catch-all 不能把安全终态
		// 降级成可透明轮换的中间失败。
		// Explicit upstream CYB always retains the conversation lock and user
		// cooldown; catch-all cannot downgrade this safety terminal.
		metadata.ConversationLocked = h.lockPromptConversationAfterUpstreamCYB(c, endpoint, model, incidentID, metadata)
		c.Set(newAPIUpstreamCyberDecisionContextKey, metadata)
	} else {
		// Without a verified NewAPI identity, do not apply a Key-wide strike or
		// cooldown. Keep the replay guard scoped to a stable Codex session or to
		// the exact prompt fingerprint plus API Key and client IP.
		locked := h.lockPromptConversationAfterUnsignedUpstreamCYB(c, endpoint, model, incidentID, errorCode)
		_, hasSession := promptConversationLockFallbackIdentity(c)
		if c != nil {
			c.Set(upstreamPolicyResponseContextKey, newAPIPolicyDecisionMetadata{
				ReasonCode: newAPIUpstreamCyberPolicyReasonCode, UpstreamPolicyCode: errorCode,
				ConversationLocked: locked && hasSession,
			})
		}
	}
	return incidentID, accepted
}

// attachUpstreamCyberPolicyStreamDecision records an explicit upstream CYB
// before its terminal SSE frame is written, then embeds the signed NewAPI
// decision in that frame. Callers use the boolean to avoid logging the same
// terminal event again during stream cleanup.
func (h *Handler) attachUpstreamCyberPolicyStreamDecision(c *gin.Context, endpoint string, model string, body []byte, attempt upstreamCyberPolicyAttempt) ([]byte, string, bool) {
	if !isExplicitUpstreamCyberPolicy(body) {
		return body, "", false
	}
	incidentID, accepted := h.logUpstreamCyberPolicy(c, endpoint, model, responseFailedErrorBody(body), attempt)
	metadata, delegated := newAPIUpstreamCyberPolicyDecision(c)
	if delegated {
		body = attachNewAPIPolicyDecisionToResponseFailed(body, metadata)
	}
	return body, acceptedPromptPolicyIncidentID(incidentID, accepted), true
}

func upstreamCyberPolicyCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	for _, path := range []string{"codex_error_info", "error.codex_error_info", "error.code", "error.type", "code", "type"} {
		switch value := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String())); value {
		case "cyber_policy", "bio_policy":
			return value
		}
	}
	return ""
}

func isExplicitUpstreamCyberPolicy(body []byte) bool {
	return upstreamCyberPolicyCode(responseFailedErrorBody(body)) != ""
}

func upstreamPolicyUserMessage(errorCode string, locked bool) string {
	if errorCode == "bio_policy" {
		if locked {
			return upstreamBioPolicyLockedUserMessage
		}
		return upstreamBioPolicyUserMessage
	}
	if locked {
		return upstreamCyberPolicyLockedUserMessage
	}
	return upstreamCyberPolicyUserMessage
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
	return cfg.Enabled && promptfilter.ShouldReviewVerdict(verdict, cfg.Review)
}

func (h *Handler) reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if strings.TrimSpace(text) == "" {
		return verdict
	}
	startedAt := time.Now()
	outcome, err := promptfilter.DefaultReviewClient.ReviewTextDetailed(ctx, text, cfg.Review)
	reviewed := promptfilter.ApplyReviewOutcome(verdict, outcome, err, cfg.Review)
	normalized := promptfilter.NormalizeReviewConfig(cfg.Review)
	latencyMS := time.Since(startedAt).Milliseconds()
	reviewed.ReviewLatencyMS = &latencyMS
	reviewed.ReviewReason = strings.TrimSpace(outcome.Reason)
	reviewed.ReviewEndpoint = strings.TrimSpace(outcome.Endpoint)
	reviewed.ReviewRequestMode = normalized.Adapter.RequestMode
	if err == nil {
		if normalized.Adapter.RequestMode == promptfilter.ReviewRequestModeModerations && outcome.DecisionCategory != "" {
			score := outcome.DecisionScore
			threshold := outcome.DecisionThreshold
			reviewed.ReviewConfidence = &score
			reviewed.ReviewThreshold = &threshold
		} else if normalized.Adapter.RequestMode == promptfilter.ReviewRequestModeChatCompletions {
			confidence := outcome.Confidence
			threshold := normalized.Adapter.ConfidenceThreshold
			reviewed.ReviewConfidence = &confidence
			reviewed.ReviewThreshold = &threshold
		}
	}
	return reviewed
}
