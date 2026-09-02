package proxy

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptRuleLearningEvidenceContextKey = "prompt_rule_learning_evidence"

type promptRuleLearningEvidence struct {
	EvaluationState string
	Text            string
	Endpoint        string
	Protocol        string
	Provider        string
	Model           string
	Action          string
	Mode            string
	Profile         string
	Score           int
	RawScore        int
	AuditScore      int
	AuditRawScore   int
	Threshold       int
	ReasonCode      string
	Reason          string
	PrimaryOrigin   string
	StrikeEligible  bool
	ReviewModel     string
	ReviewFlagged   bool
	ReviewError     string
	Matches         []promptfilter.Match
	Envelope        promptfilter.RequestEnvelope
}

const (
	promptPolicyEvidenceQualityComplete     = "complete"
	promptPolicyEvidenceQualityContextOnly  = "context_only"
	promptPolicyEvidenceQualityInsufficient = "insufficient"
	promptPolicyLearningPromptRunes         = 20000
	promptPolicyLearningPromptBytes         = 20000
	promptPolicyLearningContextRunes        = 12000
	promptPolicyLearningContextBytes        = 12000
	promptPolicyLearningUpstreamErrorRunes  = 4000
	promptPolicyLearningUpstreamErrorBytes  = 4000
	promptPolicyLearningMetadataBytes       = 60 * 1024
)

type promptPolicyLearningContextSegment struct {
	Origin    string `json:"origin"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text"`
	Linked    bool   `json:"linked,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Trust     string `json:"trust,omitempty"`
}

type promptPolicyLearningBundle struct {
	Version       int                                  `json:"version"`
	Quality       string                               `json:"quality"`
	PromptText    string                               `json:"prompt_text,omitempty"`
	Context       []promptPolicyLearningContextSegment `json:"context,omitempty"`
	UpstreamError string                               `json:"upstream_error,omitempty"`
	Transport     string                               `json:"transport,omitempty"`
	StatusCode    int                                  `json:"status_code,omitempty"`
	AttemptIndex  int                                  `json:"attempt_index,omitempty"`
	ReviewModel   string                               `json:"review_model,omitempty"`
	ReviewFlagged bool                                 `json:"review_flagged,omitempty"`
	ReviewError   string                               `json:"review_error,omitempty"`
}

type upstreamCyberPolicyAttempt struct {
	Transport    string
	StatusCode   int
	AccountID    int64
	AttemptIndex int
}

type promptPolicyRoutingSnapshot struct {
	AccountName             string
	AccountPlatform         string
	AccountGroupIDs         []int64
	AccountGroupNames       []string
	APIKeyAllowedGroupIDs   []int64
	APIKeyAllowedGroupNames []string
}

func (h *Handler) capturePromptPolicyRoutingSnapshot(accountID, apiKeyID int64) promptPolicyRoutingSnapshot {
	if h == nil || h.store == nil {
		return promptPolicyRoutingSnapshot{}
	}
	snapshot := promptPolicyRoutingSnapshot{}
	if account := h.store.FindByID(accountID); account != nil {
		account.Mu().RLock()
		snapshot.AccountName = strings.TrimSpace(account.Email)
		snapshot.AccountPlatform = strings.TrimSpace(account.UpstreamType)
		account.Mu().RUnlock()
		if snapshot.AccountPlatform == "" {
			snapshot.AccountPlatform = database.UpstreamChannelCodex
		}
		snapshot.AccountGroupIDs = account.GroupIDSnapshot()
		snapshot.AccountGroupNames = h.store.ResolveGroupNames(snapshot.AccountGroupIDs)
	}
	snapshot.APIKeyAllowedGroupIDs = h.store.GetAPIKeyAllowedGroups(apiKeyID)
	snapshot.APIKeyAllowedGroupNames = h.store.ResolveGroupNames(snapshot.APIKeyAllowedGroupIDs)
	return snapshot
}

// capturePromptRuleLearningEvidence retains the immutable local decision that
// was synchronously applied before forwarding. Deferred shadow audits remain
// separate logs and never rewrite this snapshot.
func (h *Handler) capturePromptRuleLearningEvidence(c *gin.Context, endpoint, model string, evaluation promptGuardEvaluation) {
	if c == nil {
		return
	}
	text := strings.TrimSpace(promptGuardReviewText(evaluation.Decision, evaluation.Envelope))
	protocol := ""
	if evaluation.Envelope.Protocol != promptfilter.ProtocolUnknown {
		protocol = string(evaluation.Envelope.Protocol)
	}
	provider := ""
	if evaluation.Envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
		provider = string(evaluation.Envelope.ModelFamily)
	}
	c.Set(promptRuleLearningEvidenceContextKey, promptRuleLearningEvidence{
		EvaluationState: database.PromptPolicyEvaluationCompleted,
		Text:            text,
		Endpoint:        endpoint,
		Protocol:        protocol,
		Provider:        provider,
		Model:           model,
		Action:          evaluation.Verdict.Action,
		Mode:            evaluation.Verdict.Mode,
		Profile:         evaluation.Decision.Profile,
		Score:           evaluation.Verdict.Score,
		RawScore:        evaluation.Verdict.RawScore,
		AuditScore:      evaluation.Decision.AuditScore,
		AuditRawScore:   evaluation.Decision.AuditRawScore,
		Threshold:       evaluation.Verdict.Threshold,
		ReasonCode:      evaluation.Decision.ReasonCode,
		Reason:          evaluation.Decision.Reason,
		PrimaryOrigin:   string(evaluation.Decision.PrimaryOrigin),
		StrikeEligible:  evaluation.Decision.StrikeEligible,
		ReviewModel:     evaluation.Verdict.ReviewModel,
		ReviewFlagged:   evaluation.Verdict.ReviewFlagged,
		ReviewError:     evaluation.Verdict.ReviewError,
		Matches:         append([]promptfilter.Match(nil), evaluation.Verdict.Matched...),
		Envelope:        evaluation.Envelope,
	})
}

func (h *Handler) enqueueUpstreamCyberPolicyEvidence(c *gin.Context, endpoint, model, errorCode string, body []byte, attempt upstreamCyberPolicyAttempt) (string, bool) {
	if h == nil || h.db == nil || c == nil {
		return "", false
	}
	requestCorrelationID := ensurePromptPolicyRequestCorrelationID(c)
	incidentID := uuid.NewString()
	captured, available := h.promptRuleLearningEvidenceForUpstreamFailure(c, endpoint, model)
	if endpoint == "" {
		endpoint = captured.Endpoint
	}
	if model == "" {
		model = captured.Model
	}
	audit := h.capturePromptFilterAuditContext(c)
	routing := h.capturePromptPolicyRoutingSnapshot(attempt.AccountID, audit.APIKeyID)
	protocol := captured.Protocol
	if audit.Protocol != "" {
		protocol = audit.Protocol
	}
	provider := captured.Provider
	if audit.Provider != "" {
		provider = audit.Provider
	}
	platform := promptPolicyPlatform(c)
	transport := strings.TrimSpace(attempt.Transport)
	if transport == "" {
		transport = string(promptfilter.TransportHTTP)
	}
	matchesJSON, _ := json.Marshal(captured.Matches)
	if len(matchesJSON) == 0 || string(matchesJSON) == "null" {
		matchesJSON = []byte("[]")
	}
	preview := promptfilter.RedactedPreview(captured.Text, 2000)
	checkText := promptfilter.RedactedPreview(captured.Text, promptFilterFullTextMaxRunes)
	learningSourceText := strings.TrimSpace(envelopeDirectCurrentUserText(captured.Envelope))
	if learningSourceText == "" && captured.PrimaryOrigin == string(promptfilter.OriginApplicationCandidate) {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	if learningSourceText == "" && len(captured.Envelope.Segments) == 0 {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	learningPrompt := promptPolicyRedactedLearningText(learningSourceText, promptPolicyLearningPromptRunes, promptPolicyLearningPromptBytes)
	learningContext, learningContextText := promptPolicyLearningContext(captured.Envelope)
	evidenceQuality := promptPolicyEvidenceQualityInsufficient
	fingerprintText := learningSourceText
	if fingerprintText != "" {
		evidenceQuality = promptPolicyEvidenceQualityComplete
	} else if strings.TrimSpace(learningContextText) != "" {
		evidenceQuality = promptPolicyEvidenceQualityContextOnly
		fingerprintText = learningContextText
	}
	fingerprint := promptfilter.PromptEvidenceFingerprint(fingerprintText)
	if fingerprint == "" {
		// Evidence without any extractable request text belongs to one operational
		// quarantine bucket. Using the request correlation ID here created one
		// permanently unlearnable candidate per CY incident.
		fingerprint = promptfilter.StableEvidenceFingerprint("cyber-insufficient", endpoint+"\x00"+protocol+"\x00"+provider+"\x00"+errorCode)
	}
	state := captured.EvaluationState
	if state == "" {
		state = database.PromptPolicyEvaluationUnavailable
	}
	outcome := promptPolicyLocalOutcome(captured)
	localComparison := promptPolicyLocalComparison(captured, available)
	incident := database.PromptPolicyIncidentInput{
		IncidentID:              incidentID,
		RequestCorrelationID:    requestCorrelationID,
		AttemptIndex:            attempt.AttemptIndex,
		Transport:               transport,
		Endpoint:                endpoint,
		Protocol:                protocol,
		Provider:                provider,
		Model:                   model,
		StatusCode:              attempt.StatusCode,
		AccountID:               attempt.AccountID,
		AccountName:             routing.AccountName,
		AccountPlatform:         routing.AccountPlatform,
		AccountGroupIDs:         routing.AccountGroupIDs,
		AccountGroupNames:       routing.AccountGroupNames,
		APIKeyID:                audit.APIKeyID,
		APIKeyName:              audit.APIKeyName,
		APIKeyMasked:            audit.APIKeyMasked,
		APIKeyAllowedGroupIDs:   routing.APIKeyAllowedGroupIDs,
		APIKeyAllowedGroupNames: routing.APIKeyAllowedGroupNames,
		Platform:                platform,
		NewAPIPolicyStatus:      audit.NewAPIPolicyStatus,
		NewAPIPlatform:          audit.NewAPIPlatform,
		NewAPIUserID:            audit.NewAPIUserID,
		NewAPIUserName:          audit.NewAPIUserName,
		NewAPIUserEmail:         audit.NewAPIUserEmail,
		NewAPIUserGroup:         audit.NewAPIUserGroup,
		NewAPIRequestID:         audit.NewAPIRequestID,
		SessionHash:             audit.SessionHash,
		ClientIPHash:            audit.ClientIPHash,
		SourceRef:               promptPolicySourceRef(c, requestCorrelationID),
		UpstreamErrorCode:       errorCode,
		UpstreamError:           promptfilter.RedactedPreview(promptfilter.RedactSensitive(string(body)), 8192),
		LocalEvaluationState:    state,
		LocalOutcome:            outcome,
		LocalAction:             captured.Action,
		LocalThreshold:          captured.Threshold,
		LocalMode:               captured.Mode,
		LocalPolicyProfile:      captured.Profile,
		LocalReasonCode:         captured.ReasonCode,
		LocalReason:             captured.Reason,
		LocalPrimaryOrigin:      captured.PrimaryOrigin,
		LocalStrikeEligible:     captured.StrikeEligible,
		LocalReviewModel:        captured.ReviewModel,
		LocalReviewFlagged:      captured.ReviewFlagged,
		LocalReviewError:        captured.ReviewError,
		LocalMatchedPatterns:    string(matchesJSON),
		PromptFingerprint:       fingerprint,
		PromptPreview:           preview,
		PromptText:              checkText,
		PromptAvailable:         available,
		LocalComparison:         localComparison,
		ObservedAt:              time.Now().UTC(),
	}
	if state == database.PromptPolicyEvaluationCompleted {
		incident.LocalScore = promptPolicyInt(captured.Score)
		incident.LocalRawScore = promptPolicyInt(captured.RawScore)
		incident.LocalAuditScore = promptPolicyInt(captured.AuditScore)
		incident.LocalAuditRawScore = promptPolicyInt(captured.AuditRawScore)
	}
	learningBundle := promptPolicyLearningBundle{
		Version: 1, Quality: evidenceQuality, PromptText: learningPrompt, Context: learningContext,
		UpstreamError: promptPolicyRedactedLearningText(promptfilter.RedactSensitive(string(body)), promptPolicyLearningUpstreamErrorRunes, promptPolicyLearningUpstreamErrorBytes),
		Transport:     transport, StatusCode: attempt.StatusCode, AttemptIndex: attempt.AttemptIndex,
		ReviewModel: captured.ReviewModel, ReviewFlagged: captured.ReviewFlagged,
		ReviewError: promptfilter.RedactedPreview(captured.ReviewError, 1000),
	}
	metadataFields := map[string]any{
		"incident_id": incidentID, "request_correlation_id": requestCorrelationID,
		"error_code": errorCode, "endpoint": endpoint, "local_evaluation_state": state,
		"local_outcome": outcome, "local_action": captured.Action, "local_score": incident.LocalScore,
		"local_audit_score": incident.LocalAuditScore, "local_reason_code": captured.ReasonCode,
		"local_matches": captured.Matches, "platform": platform, "prompt_available": available, "local_comparison": localComparison,
		"account_id": attempt.AccountID, "account_groups": routing.AccountGroupNames,
		"newapi_policy_status": audit.NewAPIPolicyStatus, "newapi_platform": audit.NewAPIPlatform,
		"newapi_channel_id": audit.NewAPIChannelID,
		"evidence_quality":  evidenceQuality, "learning_evidence": learningBundle,
	}
	metadata := marshalPromptPolicyEvidenceMetadata(metadataFields, learningBundle, len(captured.Matches))
	rationale := "上游返回 cyber_policy，等待归因和候选规则审核"
	if evidenceQuality == promptPolicyEvidenceQualityInsufficient {
		rationale = "上游返回 cyber_policy，但请求文本证据不足；仅归档并等待补证，不得用于自动学习"
	}
	candidate := database.PromptRuleCandidateInput{
		Fingerprint:   fingerprint,
		Kind:          database.PromptRuleCandidateKindEvidence,
		Source:        database.PromptRuleCandidateSourceUpstreamCyberPolicy,
		SamplePreview: preview,
		Rationale:     rationale,
	}
	evidence := database.PromptRuleCandidateEvidenceInput{
		SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy,
		SourceRef:  incident.SourceRef,
		SourceRefHash: promptfilter.StableEvidenceFingerprint(
			"cyber-incident", incidentID,
		),
		SamplePreview:          preview,
		MetadataJSON:           string(metadata),
		Protocol:               protocol,
		Provider:               provider,
		Model:                  model,
		APIKeyID:               audit.APIKeyID,
		APIKeyName:             audit.APIKeyName,
		PromptPolicyIncidentID: incidentID,
		ObservedAt:             incident.ObservedAt,
	}
	accepted := h.db.EnqueuePromptPolicyIncident(&incident, &candidate, &evidence)
	if accepted {
		if policy, subjectKey, trusted := h.promptRiskTrustPolicyForRequest(c); trusted {
			h.suspendPromptRiskTrustPolicy(policy, subjectKey, "上游返回 cyber_policy，恢复同步模型复核")
		}
	}
	return incidentID, accepted
}

func promptPolicyLocalComparison(captured promptRuleLearningEvidence, promptAvailable bool) string {
	if captured.EvaluationState == database.PromptPolicyEvaluationLegacyUnknown {
		return database.PromptPolicyComparisonLegacyUnknown
	}
	if captured.EvaluationState != database.PromptPolicyEvaluationCompleted {
		return database.PromptPolicyComparisonNotComparable
	}
	if promptPolicyLocalOutcome(captured) != database.PromptPolicyOutcomeNoHit {
		return database.PromptPolicyComparisonLocalDetected
	}
	if !promptAvailable {
		return database.PromptPolicyComparisonEvidenceUnavailable
	}
	return database.PromptPolicyComparisonConfirmedMiss
}

func (h *Handler) promptRuleLearningEvidenceForUpstreamFailure(c *gin.Context, endpoint, model string) (promptRuleLearningEvidence, bool) {
	if raw, exists := c.Get(promptRuleLearningEvidenceContextKey); exists {
		if captured, ok := raw.(promptRuleLearningEvidence); ok {
			return captured, strings.TrimSpace(captured.Text) != ""
		}
	}
	return h.capturePromptRuleLearningEvidenceOnUpstreamFailure(c, endpoint, model)
}

// capturePromptRuleLearningEvidenceOnUpstreamFailure is a rare-path fallback
// used only after CY when local evaluation did not run. It extracts and redacts
// the prompt without assigning a synthetic allow score.
func (h *Handler) capturePromptRuleLearningEvidenceOnUpstreamFailure(c *gin.Context, endpoint, model string) (promptRuleLearningEvidence, bool) {
	fallback := promptRuleLearningEvidence{
		EvaluationState: database.PromptPolicyEvaluationUnavailable,
		Endpoint:        endpoint,
		Model:           model,
	}
	if h == nil || h.store == nil || c == nil {
		return fallback, false
	}
	raw, exists := c.Get("raw_body")
	body, ok := raw.([]byte)
	if !exists || !ok || len(body) == 0 {
		return fallback, false
	}
	cfg := h.promptFilterConfigForRequest(c)
	envelope := promptfilter.BuildEnvelopeWithModelsAndPerformance(
		body, endpoint, model, model, promptfilter.TransportHTTP, cfg.MaxTextLength, cfg.Advanced.Guard.Performance,
	)
	text := strings.TrimSpace(envelopeCurrentUserText(envelope))
	if text == "" {
		return fallback, false
	}
	fallback.EvaluationState = database.PromptPolicyEvaluationNotRun
	fallback.Text = text
	fallback.Envelope = envelope
	fallback.Mode = cfg.Mode
	fallback.Threshold = cfg.Threshold
	if envelope.Protocol != promptfilter.ProtocolUnknown {
		fallback.Protocol = string(envelope.Protocol)
	}
	if envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
		fallback.Provider = string(envelope.ModelFamily)
	}
	return fallback, true
}

func promptPolicyLearningContext(envelope promptfilter.RequestEnvelope) ([]promptPolicyLearningContextSegment, string) {
	segments := make([]promptPolicyLearningContextSegment, 0, 8)
	parts := make([]string, 0, 8)
	remainingBytes := promptPolicyLearningContextBytes
	for _, segment := range envelope.Segments {
		if remainingBytes <= 0 || len(segments) >= 8 || !promptPolicyLearningContextSegmentEligible(segment) {
			continue
		}
		text := promptPolicyRedactedLearningText(segment.Text, promptPolicyLearningContextRunes, remainingBytes)
		if text == "" {
			continue
		}
		remainingBytes -= len(text)
		segments = append(segments, promptPolicyLearningContextSegment{
			Origin: string(segment.Origin), Role: segment.Role, Text: text,
			Linked: segment.Linked, Truncated: segment.Truncated, Trust: string(segment.Trust),
		})
		parts = append(parts, string(segment.Origin)+": "+text)
	}
	return segments, strings.Join(parts, "\n")
}

func promptPolicyRedactedLearningText(text string, maxRunes, maxBytes int) string {
	value := strings.TrimSpace(promptfilter.RedactedPreview(text, maxRunes))
	if value == "" || maxBytes <= 0 {
		return ""
	}
	return promptPolicyTruncateBytes(value, maxBytes)
}

func promptPolicyTruncateBytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

// marshalPromptPolicyEvidenceMetadata 收缩元数据直到编码后体积不超过
// promptPolicyLearningMetadataBytes。学习包各字段的预算是原始字节,而
// json.Marshal 会把 <>& 与控制字符转义成 \u00XX(最高 6 倍膨胀),原始
// 预算无法保证编码后大小;若不按编码结果复查,超限元数据会被下游 64 KiB
// 校验整体拒绝,进而丢掉整条事件记录。收缩顺序:match JSON → Prompt 文本
// 减半 → 上下文段逐段丢弃 → 错误文本,最终兜底丢弃学习包本身,保证事件
// 永远可持久化。
func marshalPromptPolicyEvidenceMetadata(fields map[string]any, bundle promptPolicyLearningBundle, matchCount int) []byte {
	encoded, _ := json.Marshal(fields)
	if len(encoded) <= promptPolicyLearningMetadataBytes {
		return encoded
	}
	delete(fields, "local_matches")
	fields["local_matches_count"] = matchCount
	encoded, _ = json.Marshal(fields)
	for len(encoded) > promptPolicyLearningMetadataBytes {
		switch {
		case len(bundle.PromptText) > 256:
			bundle.PromptText = promptPolicyTruncateBytes(bundle.PromptText, len(bundle.PromptText)/2)
		case len(bundle.Context) > 0:
			bundle.Context = bundle.Context[:len(bundle.Context)-1]
		case len(bundle.UpstreamError) > 256:
			bundle.UpstreamError = promptPolicyTruncateBytes(bundle.UpstreamError, len(bundle.UpstreamError)/2)
		case bundle.PromptText != "" || bundle.UpstreamError != "" || bundle.ReviewError != "":
			bundle.PromptText, bundle.UpstreamError, bundle.ReviewError = "", "", ""
		default:
			delete(fields, "learning_evidence")
			encoded, _ = json.Marshal(fields)
			return encoded
		}
		fields["learning_evidence"] = bundle
		encoded, _ = json.Marshal(fields)
	}
	return encoded
}

func promptPolicyLearningContextSegmentEligible(segment promptfilter.Segment) bool {
	if strings.TrimSpace(segment.Text) == "" || segment.Origin == promptfilter.OriginCurrentUser || segment.Origin == promptfilter.OriginApplicationCandidate {
		return false
	}
	switch segment.Origin {
	case promptfilter.OriginHistory:
		return segment.Linked
	case promptfilter.OriginToolOutput, promptfilter.OriginToolArguments, promptfilter.OriginSessionContext, promptfilter.OriginAttachmentContent:
		return segment.Trust != promptfilter.SegmentTrustServerInjected
	default:
		return false
	}
}

func promptPolicyLocalOutcome(captured promptRuleLearningEvidence) string {
	switch captured.Action {
	case promptfilter.ActionBlock:
		return database.PromptPolicyOutcomeBlock
	case promptfilter.ActionWarn:
		return database.PromptPolicyOutcomeWarn
	}
	if captured.EvaluationState == database.PromptPolicyEvaluationCompleted && (captured.AuditScore > 0 || len(captured.Matches) > 0) {
		return database.PromptPolicyOutcomeAuditHit
	}
	return database.PromptPolicyOutcomeNoHit
}

func promptPolicyPlatform(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if raw, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		if policyContext, ok := raw.(verifiedNewAPIPolicyContext); ok && strings.TrimSpace(policyContext.Platform) != "" {
			return policyContext.Platform
		}
	}
	if raw, exists := c.Get(newAPIIdentityContextKey); exists {
		if identityContext, ok := raw.(verifiedNewAPIIdentityContext); ok {
			return identityContext.Platform
		}
	}
	return ""
}

func promptPolicySourceRef(c *gin.Context, fallback string) string {
	if c != nil {
		for _, name := range []string{"X-NewAPI-Request-ID", "X-Request-ID"} {
			if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
				return value
			}
		}
	}
	return fallback
}

func promptPolicyInt(value int) *int {
	result := value
	return &result
}

func acceptedPromptPolicyIncidentID(incidentID string, accepted bool) string {
	if !accepted {
		return ""
	}
	return incidentID
}

func upstreamPromptPolicyTransport(stream, viaWebsocket bool) string {
	if viaWebsocket {
		return "websocket"
	}
	if stream {
		return "sse"
	}
	return "http"
}

func (h *Handler) logPromptPolicyRetryUsage(c *gin.Context, input database.UsageLogInput, incidentID string) {
	if strings.TrimSpace(incidentID) == "" {
		return
	}
	input.PromptPolicyIncidentID = incidentID
	input.IsRetryAttempt = true
	h.logUsageForRequest(c, &input)
}
