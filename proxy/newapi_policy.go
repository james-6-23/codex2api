package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const newAPIReplayNamespace = "prompt-filter-newapi-replay"
const newAPIIdentityContextKey = "prompt_filter_verified_newapi_identity"
const newAPIPolicyMetaContextKey = "prompt_filter_verified_newapi_policy_meta"

const (
	newAPISignatureVersionV1               = "1"
	newAPIPolicyDecisionSignatureVersionV1 = "v1"
	newAPIPolicyEventSignatureVersionV1    = "v1"
)

type newAPIIdentity struct {
	UserID    string
	ClientIP  string
	RequestID string
}

type verifiedNewAPIIdentityContext struct {
	Identity         newAPIIdentity
	BodySHA256       string
	SignatureVersion string
}

type newAPIOriginalAuditMeta struct {
	Endpoint string `json:"endpoint,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type newAPIPolicyMeta struct {
	Profile            string `json:"profile"`
	Mode               string `json:"mode"`
	Provider           string `json:"provider"`
	Protocol           string `json:"protocol"`
	OriginalEndpoint   string `json:"original_endpoint,omitempty"`
	OriginalProtocol   string `json:"original_protocol,omitempty"`
	RequestedModel     string `json:"requested_model,omitempty"`
	UpstreamModel      string `json:"upstream_model,omitempty"`
	ChannelID          int    `json:"channel_id,omitempty"`
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
}

type verifiedNewAPIPolicyContext struct {
	Identity          newAPIIdentity
	Meta              newAPIPolicyMeta
	MetaVerified      bool
	Audit             newAPIOriginalAuditMeta
	AuditMetaVerified bool
	BodySHA256        string
}

func (h *Handler) verifyNewAPIIdentity(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (newAPIIdentity, bool) {
	if c == nil || !cfg.Enabled {
		return newAPIIdentity{}, false
	}
	digest := sha256.Sum256(body)
	actualBodyDigest := hex.EncodeToString(digest[:])
	if cached, exists := c.Get(newAPIIdentityContextKey); exists {
		if identityContext, ok := cached.(verifiedNewAPIIdentityContext); ok && identityContext.BodySHA256 == actualBodyDigest {
			return identityContext.Identity, true
		}
	}
	secret := strings.TrimSpace(os.Getenv("PROMPT_FILTER_NEWAPI_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(cfg.Secret)
	}
	if secret == "" {
		return newAPIIdentity{}, false
	}
	identity := newAPIIdentity{
		UserID: strings.TrimSpace(c.GetHeader("X-NewAPI-User-ID")), ClientIP: strings.TrimSpace(c.GetHeader("X-NewAPI-Client-IP")),
		RequestID: strings.TrimSpace(c.GetHeader("X-NewAPI-Request-ID")),
	}
	timestampRaw := strings.TrimSpace(c.GetHeader("X-NewAPI-Timestamp"))
	signatureRaw := strings.TrimSpace(c.GetHeader("X-NewAPI-Signature"))
	method := strings.ToUpper(strings.TrimSpace(c.GetHeader("X-NewAPI-Method")))
	path := strings.TrimSpace(c.GetHeader("X-NewAPI-Path"))
	bodyDigest := strings.ToLower(strings.TrimSpace(c.GetHeader("X-NewAPI-Body-SHA256")))
	if identity.UserID == "" || identity.ClientIP == "" || identity.RequestID == "" || timestampRaw == "" || signatureRaw == "" || method == "" || path == "" || bodyDigest == "" {
		return newAPIIdentity{}, false
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil || absInt64(time.Now().Unix()-timestamp) > int64(cfg.MaxClockSkewSeconds) {
		return newAPIIdentity{}, false
	}
	requestPath := c.Request.URL.EscapedPath()
	if requestPath == "" {
		requestPath = c.Request.URL.Path
	}
	if method != strings.ToUpper(c.Request.Method) || path != requestPath || bodyDigest != hex.EncodeToString(digest[:]) {
		return newAPIIdentity{}, false
	}
	signatureVersion := strings.TrimSpace(c.GetHeader("X-NewAPI-Signature-Version"))
	switch signatureVersion {
	case "", newAPISignatureVersionV1:
		signatureVersion = newAPISignatureVersionV1
	default:
		return newAPIIdentity{}, false
	}
	canonical := strings.Join([]string{"v1", timestampRaw, identity.RequestID, identity.UserID, identity.ClientIP, method, path, bodyDigest}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signatureRaw))) {
		return newAPIIdentity{}, false
	}
	if h == nil || h.cache == nil {
		return newAPIIdentity{}, false
	}
	replayKey := hashRiskIdentity(identity.RequestID)
	unlock, acquired := acquirePromptRuntimeLease(c.Request.Context(), h.cache, newAPIReplayNamespace, replayKey)
	if !acquired {
		return newAPIIdentity{}, false
	}
	defer unlock()
	if _, exists, err := h.cache.GetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey); err != nil || exists {
		return newAPIIdentity{}, false
	}
	ttl := time.Duration(max(cfg.MaxClockSkewSeconds*2, 60)) * time.Second
	if err := h.cache.SetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey, []byte("1"), ttl); err != nil {
		return newAPIIdentity{}, false
	}
	c.Set(newAPIIdentityContextKey, verifiedNewAPIIdentityContext{
		Identity: identity, BodySHA256: actualBodyDigest, SignatureVersion: signatureVersion,
	})
	return identity, true
}

func (h *Handler) verifyNewAPIPolicyContext(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (verifiedNewAPIPolicyContext, bool) {
	if c == nil {
		return verifiedNewAPIPolicyContext{}, false
	}
	digest := sha256.Sum256(body)
	actualBodyDigest := hex.EncodeToString(digest[:])
	if cached, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		if policyContext, ok := cached.(verifiedNewAPIPolicyContext); ok && policyContext.BodySHA256 == actualBodyDigest {
			return policyContext, true
		}
	}
	identity, verified := h.verifyNewAPIIdentity(c, cfg, body)
	if !verified {
		return verifiedNewAPIPolicyContext{}, false
	}
	policyContext := verifiedNewAPIPolicyContext{Identity: identity, BodySHA256: actualBodyDigest}
	encoded := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta"))
	signature := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature"))
	if encoded == "" && signature == "" {
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	if encoded == "" || signature == "" || len(encoded) > 4096 {
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	secret := newAPIPolicySecret(cfg)
	bodyDigest := strings.ToLower(strings.TrimSpace(c.GetHeader("X-NewAPI-Body-SHA256")))
	canonical := strings.Join([]string{"policy-meta-v1", identity.RequestID, bodyDigest, encoded}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signature))) {
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) > 3072 || json.Unmarshal(payload, &policyContext.Meta) != nil {
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	if !normalizeVerifiedNewAPIPolicyMeta(&policyContext.Meta) {
		policyContext.Meta = newAPIPolicyMeta{}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	policyContext.MetaVerified = true
	auditProtocol := policyContext.Meta.OriginalProtocol
	if auditProtocol == "" {
		auditProtocol = policyContext.Meta.Protocol
	}
	policyContext.Audit, policyContext.AuditMetaVerified = normalizeVerifiedNewAPIOriginalAuditMeta(newAPIOriginalAuditMeta{
		Endpoint: policyContext.Meta.OriginalEndpoint,
		Protocol: auditProtocol,
		Provider: policyContext.Meta.Provider,
	})
	c.Set(newAPIPolicyMetaContextKey, policyContext)
	return policyContext, true
}

func normalizeVerifiedNewAPIOriginalAuditMeta(meta newAPIOriginalAuditMeta) (newAPIOriginalAuditMeta, bool) {
	meta.Endpoint = strings.TrimSpace(meta.Endpoint)
	meta.Protocol = strings.ToLower(strings.TrimSpace(meta.Protocol))
	meta.Provider = strings.ToLower(strings.TrimSpace(meta.Provider))
	if len(meta.Endpoint) > 256 || len(meta.Protocol) > 64 || len(meta.Provider) > 64 {
		return newAPIOriginalAuditMeta{}, false
	}
	if meta.Endpoint != "" {
		if !strings.HasPrefix(meta.Endpoint, "/") || strings.ContainsAny(meta.Endpoint, "\r\n\x00?#") {
			return newAPIOriginalAuditMeta{}, false
		}
	}
	validSlug := func(value string) bool {
		if value == "" {
			return true
		}
		for i, r := range value {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || (i > 0 && strings.ContainsRune("._:-", r)) {
				continue
			}
			return false
		}
		return true
	}
	if !validSlug(meta.Protocol) || !validSlug(meta.Provider) {
		return newAPIOriginalAuditMeta{}, false
	}
	return meta, true
}

func newAPIPolicySecret(cfg promptfilter.NewAPIConfig) string {
	secret := strings.TrimSpace(os.Getenv("PROMPT_FILTER_NEWAPI_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(cfg.Secret)
	}
	return secret
}

func normalizeVerifiedNewAPIPolicyMeta(meta *newAPIPolicyMeta) bool {
	if meta == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(meta.Profile)) {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
		meta.Profile = strings.ToLower(strings.TrimSpace(meta.Profile))
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(meta.Mode)) {
	case promptfilter.GuardModeOff, promptfilter.GuardModeShadow, promptfilter.GuardModeWarn, promptfilter.GuardModeEnforce:
		meta.Mode = strings.ToLower(strings.TrimSpace(meta.Mode))
	default:
		return false
	}
	meta.Provider = normalizedPolicyMetaToken(meta.Provider, 32)
	if meta.Provider == "" {
		meta.Provider = string(promptfilter.ModelFamilyUnknown)
	}
	meta.Protocol = normalizedPolicyMetaToken(meta.Protocol, 64)
	meta.RequestedModel = normalizedPolicyMetaToken(meta.RequestedModel, 128)
	meta.UpstreamModel = normalizedPolicyMetaToken(meta.UpstreamModel, 128)
	if meta.Protocol == "" {
		meta.Protocol = string(promptfilter.ProtocolUnknown)
	}
	if meta.ChannelID < 0 {
		meta.ChannelID = 0
	}
	meta.SessionFingerprint = strings.ToLower(strings.TrimSpace(meta.SessionFingerprint))
	if meta.SessionFingerprint != "" {
		decoded, err := hex.DecodeString(meta.SessionFingerprint)
		if err != nil || len(decoded) != 16 {
			return false
		}
	}
	return true
}

func normalizedPolicyMetaToken(value string, maxLen int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > maxLen {
		value = value[:maxLen]
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == '/' || r == ':' {
			continue
		}
		return ""
	}
	return value
}

func normalizeNewAPIPolicyWebSocketEventID(eventID string) string {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" || len(eventID) > 128 {
		return ""
	}
	for _, char := range eventID {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._:-", char) {
			continue
		}
		return ""
	}
	return eventID
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// VerifyNewAPIPolicyHandshake validates the exact signed identity headers used
// by NewAPI without invoking an upstream model or recording an offense.
func (h *Handler) VerifyNewAPIPolicyHandshake(c *gin.Context) {
	cfg := h.store.GetPromptFilterConfig()
	policyContext, ok := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "NewAPI 审计签名校验失败"})
		return
	}
	metaProvided := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta")) != "" || strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature")) != ""
	if metaProvided && !policyContext.MetaVerified {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"success": false, "message": "NewAPI 审核档案元数据签名或格式无效", "code": "policy_meta_invalid", "identity_verified": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "NewAPI 审计签名校验成功", "user_id": policyContext.Identity.UserID, "client_ip": policyContext.Identity.ClientIP, "request_id": policyContext.Identity.RequestID, "timestamp": c.GetHeader("X-NewAPI-Timestamp"), "policy_meta_verified": policyContext.MetaVerified, "policy_meta": policyContext.Meta})
}

// sendNewAPIPolicyDecision returns a structured policy event to NewAPI. NewAPI
// is the single authority for strikes and account/IP restrictions; Codex2API
// only rejects the current request and supplies verifiable decision metadata.
func (h *Handler) sendNewAPIPolicyDecision(c *gin.Context, cfg promptfilter.Config, decision promptfilter.Decision, verdict promptfilter.Verdict, body []byte, endpoint string, model string, signedBody []byte) bool {
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return false
	}
	metadata := buildNewAPIPolicyDecisionMetadata(policyContext.Identity, decision, verdict, cfg, body, endpoint, model)
	writeNewAPIPolicyDecisionHeaders(c, metadata)
	api.SendErrorWithStatus(c, newAPIPolicyDecisionAPIError(metadata), http.StatusBadRequest)
	return true
}

func newAPIPolicyDecisionAPIError(metadata newAPIPolicyDecisionMetadata) *api.APIError {
	apiErr := api.NewAPIError(api.ErrorCode("request_policy_violation"), "请求违反安全策略，本次请求已被拒绝", api.ErrorTypeInvalidRequest)
	details := gin.H{
		"request_id":         metadata.RequestID,
		"decision_id":        metadata.DecisionID,
		"action":             metadata.Action,
		"profile":            metadata.Profile,
		"reason_code":        metadata.ReasonCode,
		"severity":           metadata.Severity,
		"strike_eligible":    metadata.StrikeEligible,
		"rule_version":       metadata.RuleVersion,
		"evidence_sha256":    metadata.EvidenceSHA256,
		"signature_version":  newAPIPolicyDecisionSignatureVersionV1,
		"response_signature": metadata.Signature,
	}
	if metadata.EventID != "" {
		details["event_id"] = metadata.EventID
		details["event_signature_version"] = newAPIPolicyEventSignatureVersionV1
		details["event_signature"] = metadata.EventSignature
	}
	apiErr.Details = details
	return apiErr
}

type newAPIPolicyDecisionMetadata struct {
	RequestID      string
	DecisionID     string
	EventID        string
	Action         string
	Profile        string
	ReasonCode     string
	Severity       string
	StrikeEligible bool
	RuleVersion    string
	EvidenceSHA256 string
	EventSignature string
	Signature      string
}

func buildNewAPIPolicyDecisionMetadata(identity newAPIIdentity, decision promptfilter.Decision, verdict promptfilter.Verdict, cfg promptfilter.Config, body []byte, endpoint string, model string) newAPIPolicyDecisionMetadata {
	return buildNewAPIPolicyDecisionMetadataForEvent(identity, decision, verdict, cfg, body, endpoint, model, "")
}

func buildNewAPIPolicyDecisionMetadataForEvent(identity newAPIIdentity, decision promptfilter.Decision, verdict promptfilter.Verdict, cfg promptfilter.Config, body []byte, endpoint string, model string, eventID string) newAPIPolicyDecisionMetadata {
	evidence := strings.TrimSpace(verdict.FullText)
	if evidence == "" {
		evidence = string(body)
	}
	evidenceDigest := sha256.Sum256([]byte(evidence))
	versionPayload := strings.Join([]string{
		promptfilter.MarshalAdvancedConfig(cfg.Advanced),
		promptfilter.MarshalCustomPatterns(cfg.CustomPatterns),
		promptfilter.MarshalDisabledPatterns(cfg.DisabledPatterns),
		cfg.SensitiveWords,
		strconv.Itoa(cfg.Threshold),
		strconv.Itoa(cfg.StrictThreshold),
		strconv.FormatBool(cfg.StrictTerminalEnabled),
	}, "\n")
	versionDigest := sha256.Sum256([]byte(versionPayload))
	ruleVersion := hex.EncodeToString(versionDigest[:8])
	decisionPayload := strings.Join([]string{
		identity.RequestID,
		strings.TrimSpace(endpoint),
		strings.TrimSpace(model),
		hex.EncodeToString(evidenceDigest[:]),
		ruleVersion,
		decision.ReasonCode,
	}, "\n")
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		// A WebSocket connection carries multiple logical user requests under a
		// single signed connection request ID. Bind each decision to the local
		// frame sequence, while ordinary HTTP retries keep their stable ID.
		decisionPayload += "\n" + eventID
	}
	decisionDigest := sha256.Sum256([]byte(decisionPayload))
	severity := "high"
	if decision.Terminal {
		severity = "critical"
	} else if !decision.StrikeEligible {
		severity = "medium"
	}
	metadata := newAPIPolicyDecisionMetadata{
		RequestID:      identity.RequestID,
		DecisionID:     "dec_" + hex.EncodeToString(decisionDigest[:12]),
		EventID:        eventID,
		Action:         decision.Action,
		Profile:        decision.Profile,
		ReasonCode:     decision.ReasonCode,
		Severity:       severity,
		StrikeEligible: decision.StrikeEligible && decision.Action == promptfilter.ActionBlock,
		RuleVersion:    ruleVersion,
		EvidenceSHA256: hex.EncodeToString(evidenceDigest[:]),
	}
	if eventID != "" {
		metadata.EventSignature = signNewAPIPolicyEvent(newAPIPolicySecret(cfg.Advanced.NewAPI), metadata)
	}
	metadata.Signature = signNewAPIPolicyDecision(newAPIPolicySecret(cfg.Advanced.NewAPI), metadata)
	return metadata
}

func signNewAPIPolicyDecision(secret string, metadata newAPIPolicyDecisionMetadata) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	canonical := strings.Join([]string{
		"policy-decision-v1",
		strings.TrimSpace(metadata.RequestID),
		strings.TrimSpace(metadata.DecisionID),
		strings.TrimSpace(metadata.Action),
		strings.TrimSpace(metadata.Profile),
		strings.TrimSpace(metadata.ReasonCode),
		strings.TrimSpace(metadata.Severity),
		strconv.FormatBool(metadata.StrikeEligible),
		strings.TrimSpace(metadata.RuleVersion),
		strings.TrimSpace(metadata.EvidenceSHA256),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func signNewAPIPolicyEvent(secret string, metadata newAPIPolicyDecisionMetadata) string {
	if strings.TrimSpace(secret) == "" || strings.TrimSpace(metadata.EventID) == "" {
		return ""
	}
	canonical := strings.Join([]string{
		"policy-event-v1",
		strings.TrimSpace(metadata.RequestID),
		strings.TrimSpace(metadata.DecisionID),
		strings.TrimSpace(metadata.EventID),
		strings.TrimSpace(metadata.Action),
		strings.TrimSpace(metadata.Profile),
		strings.TrimSpace(metadata.ReasonCode),
		strings.TrimSpace(metadata.Severity),
		strconv.FormatBool(metadata.StrikeEligible),
		strings.TrimSpace(metadata.RuleVersion),
		strings.TrimSpace(metadata.EvidenceSHA256),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func writeNewAPIPolicyDecisionHeaders(c *gin.Context, metadata newAPIPolicyDecisionMetadata) {
	if c == nil {
		return
	}
	c.Header("X-Codex2API-Policy-Violation", "true")
	c.Header("X-Codex2API-Policy-Request-ID", metadata.RequestID)
	c.Header("X-Codex2API-Policy-Reason", metadata.ReasonCode)
	c.Header("X-Codex2API-Policy-Action", metadata.Action)
	c.Header("X-Codex2API-Policy-Decision-ID", metadata.DecisionID)
	if metadata.EventID != "" {
		c.Header("X-Codex2API-Policy-Event-ID", metadata.EventID)
		c.Header("X-Codex2API-Policy-Event-Signature-Version", newAPIPolicyEventSignatureVersionV1)
		c.Header("X-Codex2API-Policy-Event-Signature", metadata.EventSignature)
	}
	c.Header("X-Codex2API-Policy-Profile", metadata.Profile)
	c.Header("X-Codex2API-Policy-Rule-Version", metadata.RuleVersion)
	c.Header("X-Codex2API-Policy-Strike-Eligible", strconv.FormatBool(metadata.StrikeEligible))
	c.Header("X-Codex2API-Policy-Evidence-SHA256", metadata.EvidenceSHA256)
	c.Header("X-Codex2API-Policy-Severity", metadata.Severity)
	c.Header("X-Codex2API-Policy-Signature-Version", newAPIPolicyDecisionSignatureVersionV1)
	c.Header("X-Codex2API-Policy-Response-Signature", metadata.Signature)
	// Legacy headers remain present but no longer carry enforcement state. They
	// prevent old clients from mistaking absence of metadata for a transport error.
	c.Header("X-Codex2API-Policy-Strike", "0")
	c.Header("X-Codex2API-Policy-Ban", "false")
}
