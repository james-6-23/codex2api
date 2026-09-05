package proxy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

var defaultPromptGuardPipeline = promptfilter.NewGuardPipeline()

const (
	promptGuardShadowQueueCapacity = 4096
	promptGuardShadowMaxBytes      = 64 * 1024 * 1024
	promptGuardShadowTaskTimeout   = 15 * time.Second
	promptGuardShadowDropLogEvery  = 5 * time.Second
)

var defaultPromptGuardShadowDispatcher = newPromptGuardShadowDispatcher()

var (
	promptGuardShadowEnqueued     atomic.Uint64
	promptGuardShadowCompleted    atomic.Uint64
	promptGuardShadowFallbackSync atomic.Uint64
	promptGuardShadowDropped      atomic.Uint64
	promptGuardShadowFailures     atomic.Uint64
	promptGuardShadowLastDropLog  atomic.Int64
)

type promptGuardShadowAuditJob struct {
	handler        *Handler
	audit          promptfilter.DeferredAudit
	auditContext   promptFilterAuditContext
	envelope       promptfilter.RequestEnvelope
	currentPreview string
	currentChars   int
	endpoint       string
	model          string
	source         string
	errorCode      string
	logMatches     bool
	queuedAt       time.Time
	retainedBytes  int64
}

type promptGuardShadowDispatcher struct {
	queue chan promptGuardShadowAuditJob
	mu    sync.Mutex

	workers        int
	desiredWorkers int
	pendingJobs    int
	pendingBytes   int64
}

type promptGuardShadowEnqueueStatus uint8

const (
	promptGuardShadowEnqueueInvalid promptGuardShadowEnqueueStatus = iota
	promptGuardShadowEnqueueAccepted
	promptGuardShadowEnqueueOverflow
)

func newPromptGuardShadowDispatcher() *promptGuardShadowDispatcher {
	return &promptGuardShadowDispatcher{queue: make(chan promptGuardShadowAuditJob, promptGuardShadowQueueCapacity)}
}

func (d *promptGuardShadowDispatcher) enqueue(job promptGuardShadowAuditJob, workers int, queueLimit int) (promptGuardShadowEnqueueStatus, string) {
	if d == nil || job.handler == nil || job.audit.SegmentCount() == 0 {
		return promptGuardShadowEnqueueInvalid, "invalid_job"
	}
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	if queueLimit < 1 {
		queueLimit = 1
	}
	if queueLimit > promptGuardShadowQueueCapacity {
		queueLimit = promptGuardShadowQueueCapacity
	}
	job.retainedBytes = int64(job.audit.ByteSize() + len(job.currentPreview))
	if job.retainedBytes <= 0 {
		return promptGuardShadowEnqueueInvalid, "empty_job"
	}
	if job.retainedBytes > promptGuardShadowMaxBytes {
		return promptGuardShadowEnqueueOverflow, "job_too_large"
	}

	d.mu.Lock()
	if d.pendingJobs >= queueLimit {
		d.mu.Unlock()
		return promptGuardShadowEnqueueOverflow, "queue_limit"
	}
	if d.pendingBytes+job.retainedBytes > promptGuardShadowMaxBytes {
		d.mu.Unlock()
		return promptGuardShadowEnqueueOverflow, "retained_bytes_limit"
	}
	d.desiredWorkers = workers
	startWorkers := d.desiredWorkers - d.workers
	if startWorkers > 0 {
		d.workers = workers
	}
	d.pendingJobs++
	d.pendingBytes += job.retainedBytes
	d.mu.Unlock()
	for range startWorkers {
		go d.worker()
	}

	select {
	case d.queue <- job:
		promptGuardShadowEnqueued.Add(1)
		return promptGuardShadowEnqueueAccepted, ""
	default:
		d.complete(job.retainedBytes)
		return promptGuardShadowEnqueueOverflow, "queue_channel_full"
	}
}

func (d *promptGuardShadowDispatcher) worker() {
	for job := range d.queue {
		func() {
			defer d.complete(job.retainedBytes)
			defer func() {
				if recovered := recover(); recovered != nil {
					promptGuardShadowFailures.Add(1)
					log.Printf("prompt guard shadow audit panic: %v", recovered)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), promptGuardShadowTaskTimeout)
			err := runDeferredPromptGuardAudit(ctx, job)
			cancel()
			if err != nil {
				promptGuardShadowFailures.Add(1)
				log.Printf("prompt guard shadow audit failed: %v", err)
				return
			}
			promptGuardShadowCompleted.Add(1)
		}()
		if d.retireWorkerIfExcess() {
			return
		}
	}
}

func (d *promptGuardShadowDispatcher) retireWorkerIfExcess() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workers <= d.desiredWorkers {
		return false
	}
	d.workers--
	return true
}

func (d *promptGuardShadowDispatcher) complete(retainedBytes int64) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.pendingJobs > 0 {
		d.pendingJobs--
	}
	d.pendingBytes -= retainedBytes
	if d.pendingBytes < 0 {
		d.pendingBytes = 0
	}
	d.mu.Unlock()
}

type promptGuardEvaluation struct {
	Config   promptfilter.Config
	Envelope promptfilter.RequestEnvelope
	Decision promptfilter.Decision
	Verdict  promptfilter.Verdict
}

func (h *Handler) evaluatePromptGuard(c *gin.Context, rawBody []byte, signedBody []byte, endpoint string, model string, transport promptfilter.Transport) promptGuardEvaluation {
	cfg := h.promptFilterConfigForRequest(c)
	return h.evaluatePromptGuardWithConfig(c, cfg, rawBody, signedBody, endpoint, model, transport)
}

func (h *Handler) evaluatePromptGuardWithConfig(c *gin.Context, cfg promptfilter.Config, rawBody []byte, signedBody []byte, endpoint string, model string, transport promptfilter.Transport) promptGuardEvaluation {
	requestedModel, effectiveModel, trustedProfile, profileOverride, modeOverride, providerOverride, providerOverrideSet := h.resolvePromptGuardOverrides(c, cfg, signedBody, model)
	envelope := promptfilter.BuildEnvelopeWithModelsAndConfig(
		rawBody,
		endpoint,
		requestedModel,
		effectiveModel,
		transport,
		cfg,
	)
	applyPromptGuardProviderOverride(&envelope, cfg, trustedProfile, providerOverride, providerOverrideSet)
	extensionErrors := make([]string, 0, 3)
	var sessionPending *promptSessionCorrelationPending
	adapterOnly := envelope.AdapterUnclassified && len(envelope.Segments) == 0
	if !adapterOnly {
		var err error
		sessionPending, err = h.enrichPromptGuardSession(c, cfg, signedBody, &envelope)
		if err != nil {
			extensionErrors = append(extensionErrors, "session_correlation: "+err.Error())
		}
		if err := h.enrichPromptGuardAttachments(promptGuardRequestContext(c), cfg, &envelope); err != nil {
			extensionErrors = append(extensionErrors, "attachment_parser: "+err.Error())
		}
	}
	evaluation := h.evaluatePromptGuardEnvelope(c, cfg, envelope, trustedProfile, profileOverride, modeOverride)
	if !adapterOnly && evaluation.Decision.ApplicationPromptKind == "" {
		if err := h.commitPromptGuardSession(c, cfg, sessionPending, evaluation.Decision); err != nil {
			extensionErrors = append(extensionErrors, "session_commit: "+err.Error())
		}
	}
	evaluation.Decision.Errors = append(evaluation.Decision.Errors, extensionErrors...)
	return evaluation
}

func (h *Handler) evaluatePromptGuardText(c *gin.Context, text string, endpoint string, model string) promptGuardEvaluation {
	cfg := h.promptFilterConfigForRequest(c)
	return h.evaluatePromptGuardTextWithConfig(c, cfg, text, endpoint, model)
}

func (h *Handler) evaluatePromptGuardTextWithConfig(c *gin.Context, cfg promptfilter.Config, text string, endpoint string, model string) promptGuardEvaluation {
	signedBody := ingressRequestBody(c, nil)
	requestedModel, effectiveModel, trustedProfile, profileOverride, modeOverride, providerOverride, providerOverrideSet := h.resolvePromptGuardOverrides(c, cfg, signedBody, model)
	envelope := promptfilter.BuildTextEnvelopeWithModelsAndConfig(
		text,
		endpoint,
		requestedModel,
		effectiveModel,
		promptfilter.TransportHTTP,
		cfg,
	)
	applyPromptGuardProviderOverride(&envelope, cfg, trustedProfile, providerOverride, providerOverrideSet)
	extensionErrors := make([]string, 0, 2)
	var sessionPending *promptSessionCorrelationPending
	adapterOnly := envelope.AdapterUnclassified && len(envelope.Segments) == 0
	if !adapterOnly {
		var err error
		sessionPending, err = h.enrichPromptGuardSession(c, cfg, signedBody, &envelope)
		if err != nil {
			extensionErrors = append(extensionErrors, "session_correlation: "+err.Error())
		}
	}
	evaluation := h.evaluatePromptGuardEnvelope(c, cfg, envelope, trustedProfile, profileOverride, modeOverride)
	if !adapterOnly && evaluation.Decision.ApplicationPromptKind == "" {
		if err := h.commitPromptGuardSession(c, cfg, sessionPending, evaluation.Decision); err != nil {
			extensionErrors = append(extensionErrors, "session_commit: "+err.Error())
		}
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
	requestedModel := trustedRequestedModel(c, model)
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
	// NewAPI metadata can describe the original provider/protocol/model for
	// audit attribution, but it is not an enforcement control plane. The global
	// GuardPipeline is the sole authority for mode and review profile.
	profileOverride := ""
	modeOverride := ""
	provider := promptfilter.ModelFamily(policyContext.Meta.Provider)
	return requestedModel, effectiveModel, true, profileOverride, modeOverride, provider, provider != promptfilter.ModelFamilyUnknown
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
	if cfg.Enabled && decision.Mode == promptfilter.GuardModeOff && decision.ReasonCode != promptfilter.ReasonCodeAdapterUnclassified {
		return h.evaluateLegacyPromptGuard(c, ctx, cfg, envelope, decision.Profile)
	}
	verdict := decision.LegacyVerdict()
	verdict.Mode = legacyModeForPromptGuard(decision.Mode)
	// Normal audit evidence describes the direct current-user prompt. The narrow
	// high-confidence tool-output enforcement exception instead records the exact
	// triggering segment, avoiding an empty or misleading block log. Semantic
	// review remains limited by promptGuardShouldInspect and cannot clear an
	// auxiliary block using unrelated current-user text.
	text := promptGuardAuditText(decision, envelope)
	verdict.FullText = text
	verdict.TextPreview = promptfilter.RedactedPreview(text, 500)
	verdict.ExtractedChars = len([]rune(text))
	// Normal entry review receives the current user request. A narrowly qualified
	// upstream-compatibility block instead reviews the exact auxiliary evidence;
	// that result may add context, but it cannot clear the deterministic block or
	// turn auxiliary provenance into a user strike.
	reviewText := promptGuardReviewText(decision, envelope)
	compatibilityBlock := promptGuardAuxiliaryCompatibilityBlock(decision)
	inspectCurrentPrompt := promptGuardShouldInspect(decision, cfg)
	if inspectCurrentPrompt {
		advancedCfg := cfg
		verdict = h.applyPromptSemanticProtection(c, text, verdict, advancedCfg)
		if shouldReviewPromptGuardDecision(decision, verdict, cfg) {
			if policy, subjectKey, trusted := h.promptRiskTrustPolicyForRequest(c); trusted && promptRiskTrustCanBypassReview(decision, verdict, reviewText) && !promptRiskTrustReviewRequired(c, cfg, policy, subjectKey) {
				verdict.Reason = "adaptive trusted profile bypassed synchronous model review"
				decision.ReasonCode = "adaptive_trust_review_bypass"
				h.recordPromptRiskTrustBypass(c, policy, subjectKey, promptRiskTrustBypassedSignalNames(decision, verdict))
			} else {
				localTrustRisk := trusted && promptRiskTrustShouldSuspend(decision, verdict)
				verdict = h.reviewPromptFilterVerdict(ctx, reviewText, verdict, cfg)
				if trusted && verdict.ReviewError != "" {
					promptRiskTrustReleaseReviewLease(subjectKey)
				}
				var terminalBypassed bool
				verdict, terminalBypassed = applyPromptGuardTerminalModelBypass(verdict, envelope, cfg)
				if terminalBypassed {
					localTrustRisk = false
				}
				if compatibilityBlock {
					verdict = retainPromptGuardAuxiliaryCompatibilityBlock(verdict)
				}
				if trusted && !localTrustRisk && verdict.ReviewModel != "" && verdict.ReviewError == "" && !verdict.ReviewFlagged && verdict.Action == promptfilter.ActionAllow {
					h.recordPromptRiskTrustModelReview(c, policy, subjectKey)
				}
				reviewTrustRisk := promptRiskTrustReviewShouldSuspend(verdict)
				if trusted && (localTrustRisk || reviewTrustRisk) {
					h.suspendPromptRiskTrustPolicy(policy, subjectKey, "模型复核或本地高危规则命中")
				}
			}
		}
		if advancedCfg.Advanced.Risk.Enabled {
			verdict = h.applyPromptRisk(c, verdict, advancedCfg)
		}
	}
	decision = finalizePromptGuardDecision(decision, verdict)
	if decision.ApplicationPromptKind != "" && (decision.PrimaryOrigin == "" || decision.PrimaryOrigin == promptfilter.OriginSessionContext) {
		decision.ReasonCode = "application_prompt_" + decision.ApplicationPromptKind
	}
	verdict.Action = decision.Action
	verdict.Score = decision.Score
	verdict.RawScore = decision.RawScore
	verdict.Reason = decision.Reason
	return promptGuardEvaluation{Config: cfg, Envelope: envelope, Decision: decision, Verdict: verdict}
}

func (h *Handler) scheduleDeferredPromptGuardAudit(c *gin.Context, endpoint string, model string, source string, errorCode string, evaluation promptGuardEvaluation) {
	if h == nil || h.db == nil || source != "local_filter" || !evaluation.Config.LogMatches {
		return
	}
	audit, ok := evaluation.Decision.DeferredAudit()
	if !ok {
		return
	}
	envelopeMeta := evaluation.Envelope
	envelopeMeta.Segments = nil
	job := promptGuardShadowAuditJob{
		handler:        h,
		audit:          audit,
		auditContext:   h.capturePromptFilterAuditContext(c),
		envelope:       envelopeMeta,
		currentPreview: promptfilter.RedactedPreview(evaluation.Verdict.TextPreview, 500),
		currentChars:   evaluation.Verdict.ExtractedChars,
		endpoint:       endpoint,
		model:          model,
		source:         source,
		errorCode:      errorCode,
		logMatches:     evaluation.Config.LogMatches,
		queuedAt:       time.Now(),
	}
	performance := evaluation.Config.Advanced.Guard.Performance
	status, reason := defaultPromptGuardShadowDispatcher.enqueue(job, performance.ShadowWorkers, performance.ShadowQueueSize)
	if status == promptGuardShadowEnqueueAccepted {
		return
	}
	promptGuardShadowDropped.Add(1)
	logPromptGuardShadowDrop(job, reason)
}

func logPromptGuardShadowDrop(job promptGuardShadowAuditJob, reason string) {
	now := time.Now().UnixNano()
	last := promptGuardShadowLastDropLog.Load()
	if last != 0 && time.Duration(now-last) < promptGuardShadowDropLogEvery {
		return
	}
	if !promptGuardShadowLastDropLog.CompareAndSwap(last, now) {
		return
	}
	log.Printf(
		"prompt guard shadow audit dropped: reason=%s dropped_total=%d segments=%d bytes=%d endpoint=%s model=%s",
		reason,
		promptGuardShadowDropped.Load(),
		job.audit.SegmentCount(),
		job.audit.ByteSize(),
		job.endpoint,
		job.model,
	)
}

func runDeferredPromptGuardAudit(ctx context.Context, job promptGuardShadowAuditJob) error {
	if job.handler == nil || job.audit.SegmentCount() == 0 {
		return nil
	}
	if !promptGuardShadowLoggingEnabled(job) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if wait := time.Since(job.queuedAt); wait >= 5*time.Second {
		log.Printf("prompt guard shadow audit queue wait=%dms segments=%d bytes=%d endpoint=%s model=%s", wait.Milliseconds(), job.audit.SegmentCount(), job.audit.ByteSize(), job.endpoint, job.model)
	}
	decision := defaultPromptGuardPipeline.EvaluateDeferred(ctx, job.audit)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("deferred evaluation deadline: %w", err)
	}
	if len(decision.Signals) == 0 && len(decision.Errors) == 0 {
		return nil
	}
	decision.ReasonCode = "prompt_policy_shadow_async"
	decision.Action = promptfilter.ActionAllow
	decision.Score = 0
	decision.RawScore = 0
	decision.StrikeEligible = false
	decision.Terminal = false
	verdict := decision.LegacyVerdict()
	verdict.Mode = legacyModeForPromptGuard(decision.Mode)
	verdict.Action = promptfilter.ActionAllow
	verdict.Score = 0
	verdict.RawScore = 0
	verdict.TextPreview = job.currentPreview
	verdict.ExtractedChars = job.currentChars
	verdict.Reason = decision.Reason
	if len(decision.Errors) > 0 {
		verdict.ReviewError = "shadow audit: " + strings.Join(decision.Errors, "; ")
	}
	// Configuration may change while this job waits or scans. The detector uses
	// the immutable request-time snapshot, but a newly disabled master/logging
	// switch must stop stale queued evidence from being persisted.
	if !promptGuardShadowLoggingEnabled(job) {
		return nil
	}
	if err := job.handler.logPromptFilterVerdictWithAuditContext(
		ctx,
		job.auditContext,
		job.endpoint,
		job.model,
		job.source,
		job.errorCode,
		verdict,
		&decision,
		&job.envelope,
		job.logMatches,
	); err != nil {
		return fmt.Errorf("persist deferred audit: %w", err)
	}
	return nil
}

func promptGuardShadowLoggingEnabled(job promptGuardShadowAuditJob) bool {
	if job.handler == nil || !job.logMatches {
		return false
	}
	if job.handler.store == nil {
		return true
	}
	cfg := job.handler.store.GetPromptFilterConfig()
	return cfg.Enabled && cfg.LogMatches
}

// Guard mode off disables the extensible pipeline, not the existing prompt
// filter. The master prompt-filter switch remains the only way to turn all
// input filtering off, so adopting GuardPipeline cannot accidentally create a
// bypass through a trusted downstream mode override.
func (h *Handler) evaluateLegacyPromptGuard(c *gin.Context, ctx context.Context, cfg promptfilter.Config, envelope promptfilter.RequestEnvelope, profile string) promptGuardEvaluation {
	text := envelopeCurrentUserText(envelope)
	verdict := promptfilter.InspectText(text, cfg)
	verdict = h.applyPromptSemanticProtection(c, text, verdict, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		if policy, subjectKey, trusted := h.promptRiskTrustPolicyForRequest(c); trusted && verdict.Action == promptfilter.ActionAllow && verdict.Score == 0 && verdict.RawScore == 0 && len(verdict.Matched) == 0 && strings.TrimSpace(text) != "" && !promptRiskTrustReviewRequired(c, cfg, policy, subjectKey) {
			verdict.Reason = "adaptive trusted profile bypassed synchronous model review"
			h.recordPromptRiskTrustBypass(c, policy, subjectKey, nil)
		} else {
			localTrustRisk := trusted && (verdict.Action != promptfilter.ActionAllow || verdict.Score > 0 || verdict.RawScore > 0 || len(verdict.Matched) > 0)
			verdict = h.reviewPromptFilterVerdict(ctx, text, verdict, cfg)
			if trusted && verdict.ReviewError != "" {
				promptRiskTrustReleaseReviewLease(subjectKey)
			}
			var terminalBypassed bool
			verdict, terminalBypassed = applyPromptGuardTerminalModelBypass(verdict, envelope, cfg)
			if terminalBypassed {
				localTrustRisk = false
			}
			verdict = promptfilter.ApplyReviewMode(verdict, cfg.Mode)
			if trusted && !localTrustRisk && verdict.ReviewModel != "" && verdict.ReviewError == "" && !verdict.ReviewFlagged && verdict.Action == promptfilter.ActionAllow {
				h.recordPromptRiskTrustModelReview(c, policy, subjectKey)
			}
			reviewTrustRisk := promptRiskTrustReviewShouldSuspend(verdict)
			if trusted && (localTrustRisk || reviewTrustRisk) {
				h.suspendPromptRiskTrustPolicy(policy, subjectKey, "模型复核或本地高危规则命中")
			}
		}
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

func promptGuardHasReviewableEnforcement(decision promptfilter.Decision) bool {
	if decision.Action == promptfilter.ActionAllow {
		return false
	}
	return decision.PrimaryOrigin == promptfilter.OriginCurrentUser || decision.PrimaryOrigin == promptfilter.OriginApplicationCandidate
}

func promptGuardShouldInspect(decision promptfilter.Decision, cfg promptfilter.Config) bool {
	if decision.ReasonCode == promptfilter.ReasonCodeAdapterUnclassified {
		return false
	}
	if promptfilter.NormalizeReviewConfig(cfg.Review).Ready() {
		return true
	}
	if promptGuardHasReviewableEnforcement(decision) {
		return true
	}
	if decision.Action != promptfilter.ActionAllow || !cfg.Advanced.Sidecar.ScanCleanEnabled {
		return false
	}
	// A verified ambient application prompt has already had its fixed policy
	// boilerplate removed. Its dynamic candidate is therefore safe to send to a
	// configured semantic scanner even when the local rules found no match.
	return decision.ApplicationPromptKind == "" || strings.TrimSpace(decision.ReviewText) != ""
}

func promptGuardReviewText(decision promptfilter.Decision, envelope promptfilter.RequestEnvelope) string {
	if decision.ApplicationPromptKind != "" || decision.PrimaryOrigin == promptfilter.OriginApplicationCandidate || decision.PrimaryOrigin == promptfilter.OriginCurrentUser || promptGuardAuxiliaryCompatibilityBlock(decision) {
		if candidate := strings.TrimSpace(decision.ReviewText); candidate != "" {
			return candidate
		}
	}
	return envelopeCurrentUserText(envelope)
}

func promptGuardAuxiliaryCompatibilityBlock(decision promptfilter.Decision) bool {
	return decision.Action == promptfilter.ActionBlock &&
		decision.PrimaryOrigin == promptfilter.OriginToolOutput &&
		!decision.StrikeEligible && !decision.Terminal &&
		strings.TrimSpace(decision.ReviewText) != ""
}

func retainPromptGuardAuxiliaryCompatibilityBlock(verdict promptfilter.Verdict) promptfilter.Verdict {
	verdict.Action = promptfilter.ActionBlock
	verdict.TerminalStrictHit = false
	verdict.TerminalCategoryHit = false
	verdict.SensitiveIntent = false
	if !verdict.ReviewFlagged {
		verdict.Reason = "upstream compatibility guard retained auxiliary risk block"
	}
	return verdict
}

func applyPromptGuardTerminalModelBypass(verdict promptfilter.Verdict, envelope promptfilter.RequestEnvelope, cfg promptfilter.Config) (promptfilter.Verdict, bool) {
	if !verdict.Reviewed || verdict.ReviewError != "" || verdict.ReviewFlagged ||
		(!verdict.TerminalStrictHit && !verdict.TerminalCategoryHit) {
		return verdict, false
	}
	requestedModel := strings.ToLower(strings.TrimSpace(envelope.RequestedModel))
	if requestedModel == "" {
		return verdict, false
	}
	matched := false
	for _, model := range cfg.Advanced.Enforcement.TerminalBypassModels {
		if strings.EqualFold(strings.TrimSpace(model), requestedModel) {
			matched = true
			break
		}
	}
	if !matched {
		return verdict, false
	}
	verdict.Action = promptfilter.ActionAllow
	verdict.TerminalStrictHit = false
	verdict.TerminalCategoryHit = false
	verdict.Reason = "clean prompt review cleared terminal match for exempt requested model"
	return verdict, true
}

func promptGuardAuditText(decision promptfilter.Decision, envelope promptfilter.RequestEnvelope) string {
	if decision.Action == promptfilter.ActionBlock && decision.PrimaryOrigin == promptfilter.OriginToolOutput {
		if evidence := strings.TrimSpace(decision.ReviewText); evidence != "" {
			return evidence
		}
	}
	return promptGuardReviewText(decision, envelope)
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
	return shouldReviewPromptFilterVerdict(verdict, cfg)
}

// promptGuardDetectorExternalReview 标记这样一种 block:本地过滤器放行,唯一
// 证据是外部审核模型打了 flag。
const promptGuardDetectorExternalReview = "external_review"

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
	if verdict.Reviewed && verdict.ReviewFlagged && finalAction != promptfilter.ActionAllow && len(verdict.Matched) == 0 &&
		(decision.PrimaryOrigin == "" || decision.PrimaryOrigin == promptfilter.OriginCurrentUser) {
		decision.PrimaryOrigin = promptfilter.OriginCurrentUser
		decision.PrimaryDetector = promptGuardDetectorExternalReview
		decision.StrikeEligible = false
	}
	if decision.ApplicationPromptKind != "" && finalAction != promptfilter.ActionAllow && decision.PrimaryOrigin == "" {
		decision.PrimaryOrigin = promptfilter.OriginApplicationCandidate
	}
	if decision.PrimaryOrigin == promptfilter.OriginApplicationCandidate {
		decision.StrikeEligible = false
	}
	if verdict.Reviewed && !verdict.ReviewFlagged && finalAction == promptfilter.ActionAllow {
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
