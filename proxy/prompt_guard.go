package proxy

import (
	"context"
	"strings"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

var defaultPromptGuardPipeline = promptfilter.NewGuardPipeline()

type promptGuardEvaluation struct {
	Config   promptfilter.Config
	Envelope promptfilter.RequestEnvelope
	Decision promptfilter.Decision
	Verdict  promptfilter.Verdict
}

func (h *Handler) evaluatePromptGuard(c *gin.Context, rawBody []byte, signedBody []byte, endpoint string, model string, transport promptfilter.Transport) promptGuardEvaluation {
	cfg := h.store.GetPromptFilterConfig()
	requestedModel, effectiveModel, trustedProfile, profileOverride, modeOverride, providerOverride, providerOverrideSet := h.resolvePromptGuardOverrides(c, cfg, signedBody, model)
	envelope := promptfilter.BuildEnvelopeWithModels(rawBody, endpoint, requestedModel, effectiveModel, transport, cfg.MaxTextLength)
	applyPromptGuardProviderOverride(&envelope, cfg, trustedProfile, providerOverride, providerOverrideSet)
	extensionErrors := make([]string, 0, 3)
	sessionPending, err := h.enrichPromptGuardSession(c, cfg, signedBody, &envelope)
	if err != nil {
		extensionErrors = append(extensionErrors, "session_correlation: "+err.Error())
	}
	if err := h.enrichPromptGuardAttachments(promptGuardRequestContext(c), cfg, &envelope); err != nil {
		extensionErrors = append(extensionErrors, "attachment_parser: "+err.Error())
	}
	evaluation := h.evaluatePromptGuardEnvelope(c, cfg, envelope, trustedProfile, profileOverride, modeOverride)
	if err := h.commitPromptGuardSession(c, cfg, sessionPending, evaluation.Decision); err != nil {
		extensionErrors = append(extensionErrors, "session_commit: "+err.Error())
	}
	evaluation.Decision.Errors = append(evaluation.Decision.Errors, extensionErrors...)
	return evaluation
}

func (h *Handler) evaluatePromptGuardText(c *gin.Context, text string, endpoint string, model string) promptGuardEvaluation {
	cfg := h.store.GetPromptFilterConfig()
	signedBody := ingressRequestBody(c, nil)
	requestedModel, effectiveModel, trustedProfile, profileOverride, modeOverride, providerOverride, providerOverrideSet := h.resolvePromptGuardOverrides(c, cfg, signedBody, model)
	envelope := promptfilter.RequestEnvelope{
		Endpoint:       endpoint,
		Protocol:       promptfilter.ProtocolForEndpoint(endpoint),
		Transport:      promptfilter.TransportHTTP,
		RequestedModel: requestedModel,
		EffectiveModel: effectiveModel,
		ModelFamily:    promptfilter.ResolveModelFamily(requestedModel, effectiveModel),
		Segments: []promptfilter.Segment{{
			Origin: promptfilter.OriginCurrentUser,
			Role:   "user",
			Text:   text,
			Trust:  promptfilter.SegmentTrustClientSupplied,
		}},
	}
	applyPromptGuardProviderOverride(&envelope, cfg, trustedProfile, providerOverride, providerOverrideSet)
	extensionErrors := make([]string, 0, 2)
	sessionPending, err := h.enrichPromptGuardSession(c, cfg, signedBody, &envelope)
	if err != nil {
		extensionErrors = append(extensionErrors, "session_correlation: "+err.Error())
	}
	evaluation := h.evaluatePromptGuardEnvelope(c, cfg, envelope, trustedProfile, profileOverride, modeOverride)
	if err := h.commitPromptGuardSession(c, cfg, sessionPending, evaluation.Decision); err != nil {
		extensionErrors = append(extensionErrors, "session_commit: "+err.Error())
	}
	evaluation.Decision.Errors = append(evaluation.Decision.Errors, extensionErrors...)
	return evaluation
}

func promptGuardRequestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func (h *Handler) resolvePromptGuardOverrides(c *gin.Context, cfg promptfilter.Config, signedBody []byte, model string) (string, string, bool, string, string, promptfilter.ModelFamily, bool) {
	requestedModel := model
	effectiveModel := model
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified || !policyContext.MetaVerified {
		return requestedModel, effectiveModel, false, "", "", promptfilter.ModelFamilyUnknown, false
	}
	if policyContext.Meta.RequestedModel != "" {
		requestedModel = policyContext.Meta.RequestedModel
	}
	if effectiveModel == "" && policyContext.Meta.UpstreamModel != "" {
		effectiveModel = policyContext.Meta.UpstreamModel
	}
	provider := promptfilter.ModelFamily(policyContext.Meta.Provider)
	return requestedModel, effectiveModel, true, policyContext.Meta.Profile, policyContext.Meta.Mode, provider, provider != promptfilter.ModelFamilyUnknown
}

func applyPromptGuardProviderOverride(envelope *promptfilter.RequestEnvelope, cfg promptfilter.Config, trusted bool, provider promptfilter.ModelFamily, providerSet bool) {
	if envelope == nil || !trusted || !providerSet || !cfg.Advanced.Guard.AllowTrustedOverrides {
		return
	}
	switch provider {
	case promptfilter.ModelFamilyOpenAI, promptfilter.ModelFamilyAnthropic, promptfilter.ModelFamilyXAI:
		envelope.ModelFamily = provider
	}
}

func (h *Handler) evaluatePromptGuardEnvelope(c *gin.Context, cfg promptfilter.Config, envelope promptfilter.RequestEnvelope, trustedProfile bool, profileOverride string, modeOverride string) promptGuardEvaluation {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	decision := defaultPromptGuardPipeline.Evaluate(ctx, promptfilter.GuardRequest{
		Envelope:        envelope,
		Config:          cfg,
		TrustedProfile:  trustedProfile,
		ProfileOverride: profileOverride,
		ModeOverride:    modeOverride,
	})
	if cfg.Enabled && decision.Mode == promptfilter.GuardModeOff {
		return h.evaluateLegacyPromptGuard(c, ctx, cfg, envelope, decision.Profile)
	}
	verdict := decision.LegacyVerdict()
	verdict.Mode = legacyModeForPromptGuard(decision.Mode)
	// The detector selected for audit can come from history, tool output, or
	// session context. Semantic review, persisted request evidence, and the UI
	// preview must nevertheless describe only the direct current-user prompt.
	text := envelopeCurrentUserText(envelope)
	verdict.FullText = text
	verdict.TextPreview = promptfilter.RedactedPreview(text, 500)
	verdict.ExtractedChars = len([]rune(text))
	// Semantic review must examine the text that produced the enforcement
	// decision. The persisted preview intentionally contains only the current
	// user prompt, so a history/tool/session enforcement signal cannot safely be
	// reviewed against that different text: a benign current prompt could clear
	// an administrator-enabled auxiliary-layer block. Clean-prompt sampling is
	// still allowed when the pipeline itself made no enforcement decision.
	inspectCurrentPrompt := promptGuardHasCurrentUserEnforcement(decision) ||
		(decision.Action == promptfilter.ActionAllow && cfg.Advanced.Sidecar.ScanCleanEnabled)
	if inspectCurrentPrompt {
		advancedCfg := cfg
		verdict = h.applyPromptSemanticProtection(c, text, verdict, advancedCfg)
		if shouldReviewPromptGuardDecision(decision, verdict, cfg) {
			verdict = h.reviewPromptFilterVerdict(ctx, text, verdict, cfg)
		}
		if advancedCfg.Advanced.Risk.Enabled {
			verdict = h.applyPromptRisk(c, verdict, advancedCfg)
		}
	}
	decision = finalizePromptGuardDecision(decision, verdict)
	verdict.Action = decision.Action
	verdict.Score = decision.Score
	verdict.RawScore = decision.RawScore
	verdict.Reason = decision.Reason
	return promptGuardEvaluation{Config: cfg, Envelope: envelope, Decision: decision, Verdict: verdict}
}

// Guard mode off disables the extensible pipeline, not the existing prompt
// filter. The master prompt-filter switch remains the only way to turn all
// input filtering off, so adopting GuardPipeline cannot accidentally create a
// bypass during rollout or via a trusted downstream mode override.
func (h *Handler) evaluateLegacyPromptGuard(c *gin.Context, ctx context.Context, cfg promptfilter.Config, envelope promptfilter.RequestEnvelope, profile string) promptGuardEvaluation {
	text := envelopeCurrentUserText(envelope)
	verdict := promptfilter.InspectText(text, cfg)
	verdict = h.applyPromptSemanticProtection(c, text, verdict, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = h.reviewPromptFilterVerdict(ctx, text, verdict, cfg)
	}
	if cfg.Advanced.Risk.Enabled {
		verdict = h.applyPromptRisk(c, verdict, cfg)
	}
	decision := promptfilter.Decision{
		Enabled:        verdict.Enabled,
		Mode:           promptfilter.GuardModeOff,
		Profile:        profile,
		Action:         verdict.Action,
		WouldAction:    verdict.Action,
		Score:          verdict.Score,
		RawScore:       verdict.RawScore,
		AuditScore:     verdict.Score,
		AuditRawScore:  verdict.RawScore,
		Reason:         verdict.Reason,
		Terminal:       verdict.TerminalStrictHit || verdict.TerminalCategoryHit,
		StrikeEligible: verdict.Action == promptfilter.ActionBlock && verdict.SensitiveIntent && (verdict.TerminalStrictHit || verdict.TerminalCategoryHit),
	}
	if len(verdict.Matched) > 0 {
		decision.PrimaryOrigin = promptfilter.OriginCurrentUser
		decision.PrimaryDetector = "legacy_regex"
	}
	if decision.Terminal {
		decision.ReasonCode = "terminal_policy_match"
	} else if decision.Action == promptfilter.ActionBlock {
		decision.ReasonCode = "prompt_policy_match"
	} else if decision.Action == promptfilter.ActionWarn {
		decision.ReasonCode = "prompt_policy_warning"
	}
	return promptGuardEvaluation{Config: cfg, Envelope: envelope, Decision: decision, Verdict: verdict}
}

func legacyModeForPromptGuard(mode string) string {
	switch mode {
	case promptfilter.GuardModeEnforce:
		return promptfilter.ModeBlock
	case promptfilter.GuardModeWarn:
		return promptfilter.ModeWarn
	default:
		return promptfilter.ModeMonitor
	}
}

func promptGuardHasCurrentUserEnforcement(decision promptfilter.Decision) bool {
	return decision.PrimaryOrigin == promptfilter.OriginCurrentUser && decision.Action != promptfilter.ActionAllow
}

func envelopeCurrentUserText(envelope promptfilter.RequestEnvelope) string {
	parts := make([]string, 0, 2)
	for _, segment := range envelope.Segments {
		if segment.Origin == promptfilter.OriginCurrentUser || (segment.Origin == promptfilter.OriginHistory && segment.Linked) {
			if text := strings.TrimSpace(segment.Text); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func shouldReviewPromptGuardDecision(decision promptfilter.Decision, verdict promptfilter.Verdict, cfg promptfilter.Config) bool {
	if decision.Profile == promptfilter.GuardProfileStrict && decision.Terminal && decision.StrikeEligible {
		return false
	}
	return shouldReviewPromptFilterVerdict(verdict, cfg)
}

func finalizePromptGuardDecision(decision promptfilter.Decision, verdict promptfilter.Verdict) promptfilter.Decision {
	decision.Score = verdict.Score
	decision.RawScore = verdict.RawScore
	if strings.TrimSpace(verdict.Reason) != "" {
		decision.Reason = verdict.Reason
	}
	finalAction := verdict.Action
	switch decision.Mode {
	case promptfilter.GuardModeOff, promptfilter.GuardModeShadow:
		finalAction = promptfilter.ActionAllow
	case promptfilter.GuardModeWarn:
		if finalAction == promptfilter.ActionBlock {
			finalAction = promptfilter.ActionWarn
		}
	}
	decision.Action = finalAction
	if verdict.Reviewed && !verdict.ReviewFlagged {
		decision.Terminal = false
		decision.StrikeEligible = false
	}
	if finalAction != promptfilter.ActionBlock {
		decision.StrikeEligible = false
	}
	if finalAction == promptfilter.ActionWarn {
		decision.ReasonCode = "prompt_policy_warning"
	} else if finalAction == promptfilter.ActionBlock && strings.TrimSpace(decision.ReasonCode) == "" {
		decision.ReasonCode = "prompt_policy_classifier"
	} else if finalAction == promptfilter.ActionAllow && len(decision.Signals) > 0 {
		decision.ReasonCode = "prompt_policy_shadow"
	}
	return decision
}
