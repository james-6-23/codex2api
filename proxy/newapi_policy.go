package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const newAPIReplayNamespace = "prompt-filter-newapi-replay"
const newAPIIdentityContextKey = "prompt_filter_verified_newapi_identity"
const newAPIPolicyMetaContextKey = "prompt_filter_verified_newapi_policy_meta"
const newAPIBindingContextKey = "prompt_filter_newapi_binding"
const newAPIUpstreamCyberDecisionContextKey = "prompt_filter_newapi_upstream_cyber_decision"

const (
	newAPISignatureVersionV1               = "1"
	newAPIPolicyDecisionSignatureVersionV1 = "v1"
	newAPIPolicyEventSignatureVersionV1    = "v1"
	newAPIUpstreamCyberPolicyReasonCode    = "upstream_cyber_policy"
)

type newAPIIdentity struct {
	UserID    string
	ClientIP  string
	RequestID string
}

type verifiedNewAPIIdentityContext struct {
	Identity           newAPIIdentity
	BodySHA256         string
	APIKeyID           int64
	Platform           string
	VerificationSecret string
}

type newAPIOriginalAuditMeta struct {
	Endpoint string `json:"endpoint,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type newAPIPolicyMeta struct {
	PlatformID         string `json:"platform_id,omitempty"`
	UserName           string `json:"user_name,omitempty"`
	UserEmail          string `json:"user_email,omitempty"`
	UserGroup          string `json:"user_group,omitempty"`
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
	Identity           newAPIIdentity
	APIKeyID           int64
	Platform           string
	VerificationSecret string
	Meta               newAPIPolicyMeta
	MetaVerified       bool
	Audit              newAPIOriginalAuditMeta
	AuditMetaVerified  bool
	BodySHA256         string
}

type resolvedPromptFilterNewAPIBinding struct {
	APIKeyID int64
	Binding  database.PromptFilterNewAPIBinding
	Bound    bool
}

func (h *Handler) resolvePromptFilterNewAPIBinding(c *gin.Context) (database.PromptFilterNewAPIBinding, bool) {
	if c == nil || h == nil || h.store == nil {
		return database.PromptFilterNewAPIBinding{}, false
	}
	apiKeyID := requestAPIKeyID(c)
	if cached, ok := c.Get(newAPIBindingContextKey); ok {
		if resolved, valid := cached.(resolvedPromptFilterNewAPIBinding); valid && resolved.APIKeyID == apiKeyID {
			return resolved.Binding, resolved.Bound
		}
	}
	binding, bound := h.store.GetPromptFilterNewAPIBinding(apiKeyID)
	c.Set(newAPIBindingContextKey, resolvedPromptFilterNewAPIBinding{APIKeyID: apiKeyID, Binding: binding, Bound: bound})
	return binding, bound
}

// refreshNewAPIWebSocketBinding enforces binding revocation at every logical
// WebSocket turn. Policy-only changes are refreshed in place, while tenant,
// enablement, signature requirements, deletion, or expired secret grace force
// a reconnect so an identity verified under an obsolete binding cannot live
// indefinitely on a long-running connection.
func (h *Handler) refreshNewAPIWebSocketBinding(c *gin.Context, now time.Time) *api.APIError {
	if c == nil || h == nil || h.store == nil {
		return nil
	}
	apiKeyID := requestAPIKeyID(c)
	cachedValue, cached := c.Get(newAPIBindingContextKey)
	resolved, valid := cachedValue.(resolvedPromptFilterNewAPIBinding)
	if !cached || !valid || resolved.APIKeyID != apiKeyID {
		return nil
	}
	current, currentBound := h.store.GetPromptFilterNewAPIBinding(apiKeyID)
	revoked := func() *api.APIError {
		return api.NewAPIError(
			api.ErrorCode("newapi_websocket_binding_changed"),
			"NewAPI 平台绑定或密钥已变更，请重新连接后再试",
			api.ErrorTypeAuthentication,
		)
	}
	if !resolved.Bound {
		if currentBound {
			return revoked()
		}
		return nil
	}
	if !currentBound || current.PlatformCode != resolved.Binding.PlatformCode || current.Enabled != resolved.Binding.Enabled || current.RequireSignedIdentity != resolved.Binding.RequireSignedIdentity {
		return revoked()
	}
	identityValue, identityCached := c.Get(newAPIIdentityContextKey)
	identity, identityValid := identityValue.(verifiedNewAPIIdentityContext)
	if current.Enabled && (current.RequireSignedIdentity || identityCached) {
		if !identityValid || identity.APIKeyID != apiKeyID || identity.Platform != normalizedNewAPIPlatform(current.PlatformCode) || !promptFilterBindingAcceptsSecret(current, identity.VerificationSecret, now) {
			return revoked()
		}
	}
	c.Set(newAPIBindingContextKey, resolvedPromptFilterNewAPIBinding{APIKeyID: apiKeyID, Binding: current, Bound: true})
	return nil
}

func promptFilterBindingAcceptsSecret(binding database.PromptFilterNewAPIBinding, secret string, now time.Time) bool {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return false
	}
	if current := strings.TrimSpace(binding.Secret); current != "" && hmac.Equal([]byte(current), []byte(secret)) {
		return true
	}
	previous := strings.TrimSpace(binding.PreviousSecret)
	return previous != "" && binding.PreviousSecretExpiresAt != nil && binding.PreviousSecretExpiresAt.After(now) && hmac.Equal([]byte(previous), []byte(secret))
}

func normalizedNewAPIPlatform(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "" {
		return value
	}
	return "bound"
}

func newAPIRuntimeScope(apiKeyID int64, platform string) string {
	return fmt.Sprintf("api-key:%d:platform:%s", apiKeyID, hashRiskIdentity(normalizedNewAPIPlatform(platform)))
}

type newAPISecretCandidate struct {
	Secret string
}

func (h *Handler) newAPIIdentitySecrets(c *gin.Context, now time.Time) (apiKeyID int64, platform string, enabled bool, candidates []newAPISecretCandidate) {
	apiKeyID = requestAPIKeyID(c)
	if binding, bound := h.resolvePromptFilterNewAPIBinding(c); bound {
		platform = normalizedNewAPIPlatform(binding.PlatformCode)
		if !binding.Enabled {
			return apiKeyID, platform, false, nil
		}
		if secret := strings.TrimSpace(binding.Secret); secret != "" {
			candidates = append(candidates, newAPISecretCandidate{Secret: secret})
		}
		if previous := strings.TrimSpace(binding.PreviousSecret); previous != "" && binding.PreviousSecretExpiresAt != nil && binding.PreviousSecretExpiresAt.After(now) {
			if len(candidates) == 0 || !hmac.Equal([]byte(candidates[0].Secret), []byte(previous)) {
				candidates = append(candidates, newAPISecretCandidate{Secret: previous})
			}
		}
		// A configured API-key binding is a hard identity boundary. Never borrow
		// another key's secret, even if this binding is disabled or incomplete.
		return apiKeyID, platform, true, candidates
	}
	// Unbound keys never accept NewAPI identity, regardless of whether other
	// bindings exist or retired global environment/database values are present.
	return apiKeyID, normalizedNewAPIPlatform(""), false, nil
}

func (h *Handler) verifyNewAPIIdentity(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (newAPIIdentity, bool) {
	verified, ok := h.verifyNewAPIIdentityContext(c, cfg, body)
	return verified.Identity, ok
}

func (h *Handler) verifyNewAPIIdentityContext(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (verifiedNewAPIIdentityContext, bool) {
	if c == nil || !cfg.Enabled {
		return verifiedNewAPIIdentityContext{}, false
	}
	_, actualBodyDigest := promptRequestBodyDigest(c, body)
	if cached, exists := c.Get(newAPIIdentityContextKey); exists {
		if identityContext, ok := cached.(verifiedNewAPIIdentityContext); ok && identityContext.BodySHA256 == actualBodyDigest {
			return identityContext, true
		}
	}
	apiKeyID, platform, enabled, secretCandidates := h.newAPIIdentitySecrets(c, time.Now())
	if !enabled || len(secretCandidates) == 0 {
		return verifiedNewAPIIdentityContext{}, false
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
		return verifiedNewAPIIdentityContext{}, false
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil || absInt64(time.Now().Unix()-timestamp) > int64(cfg.MaxClockSkewSeconds) {
		return verifiedNewAPIIdentityContext{}, false
	}
	requestPath := c.Request.URL.EscapedPath()
	if requestPath == "" {
		requestPath = c.Request.URL.Path
	}
	if method != strings.ToUpper(c.Request.Method) || path != requestPath || bodyDigest != actualBodyDigest {
		return verifiedNewAPIIdentityContext{}, false
	}
	switch strings.TrimSpace(c.GetHeader("X-NewAPI-Signature-Version")) {
	case "", newAPISignatureVersionV1:
	default:
		return verifiedNewAPIIdentityContext{}, false
	}
	canonical := strings.Join([]string{"v1", timestampRaw, identity.RequestID, identity.UserID, identity.ClientIP, method, path, bodyDigest}, "\n")
	verifiedSecret := ""
	for _, candidate := range secretCandidates {
		mac := hmac.New(sha256.New, []byte(candidate.Secret))
		_, _ = mac.Write([]byte(canonical))
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(expected), []byte(strings.ToLower(signatureRaw))) {
			verifiedSecret = candidate.Secret
			break
		}
	}
	if verifiedSecret == "" {
		return verifiedNewAPIIdentityContext{}, false
	}
	if h == nil || h.cache == nil {
		return verifiedNewAPIIdentityContext{}, false
	}
	runtimeScope := newAPIRuntimeScope(apiKeyID, platform)
	replayKey := runtimeScope + ":request:" + hashRiskIdentity(identity.RequestID)
	unlock, acquired := acquirePromptRuntimeLease(c.Request.Context(), h.cache, newAPIReplayNamespace, replayKey)
	if !acquired {
		return verifiedNewAPIIdentityContext{}, false
	}
	defer unlock()
	if _, exists, err := h.cache.GetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey); err != nil || exists {
		return verifiedNewAPIIdentityContext{}, false
	}
	ttl := time.Duration(max(cfg.MaxClockSkewSeconds*2, 60)) * time.Second
	if err := h.cache.SetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey, []byte("1"), ttl); err != nil {
		return verifiedNewAPIIdentityContext{}, false
	}
	verified := verifiedNewAPIIdentityContext{
		Identity: identity, BodySHA256: actualBodyDigest, APIKeyID: apiKeyID,
		Platform: platform, VerificationSecret: verifiedSecret,
	}
	c.Set(newAPIIdentityContextKey, verified)
	return verified, true
}

func (h *Handler) verifyNewAPIPolicyContext(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (verifiedNewAPIPolicyContext, bool) {
	if c == nil || !cfg.Enabled {
		return verifiedNewAPIPolicyContext{}, false
	}
	_, actualBodyDigest := promptRequestBodyDigest(c, body)
	if cached, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		if policyContext, ok := cached.(verifiedNewAPIPolicyContext); ok && policyContext.BodySHA256 == actualBodyDigest {
			return policyContext, true
		}
	}
	identityContext, verified := h.verifyNewAPIIdentityContext(c, cfg, body)
	if !verified {
		return verifiedNewAPIPolicyContext{}, false
	}
	identity := identityContext.Identity
	binding, bound := h.resolvePromptFilterNewAPIBinding(c)
	bound = bound && binding.Enabled
	policyContext := verifiedNewAPIPolicyContext{
		Identity: identity, BodySHA256: actualBodyDigest,
		APIKeyID: identityContext.APIKeyID, Platform: identityContext.Platform,
		VerificationSecret: identityContext.VerificationSecret,
	}
	encoded := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta"))
	signature := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature"))
	if encoded == "" && signature == "" {
		if bound {
			return verifiedNewAPIPolicyContext{}, false
		}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	if encoded == "" || signature == "" || len(encoded) > 4096 {
		if bound {
			return verifiedNewAPIPolicyContext{}, false
		}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	bodyDigest := strings.ToLower(strings.TrimSpace(c.GetHeader("X-NewAPI-Body-SHA256")))
	canonical := strings.Join([]string{"policy-meta-v1", identity.RequestID, bodyDigest, encoded}, "\n")
	mac := hmac.New(sha256.New, []byte(policyContext.VerificationSecret))
	_, _ = mac.Write([]byte(canonical))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signature))) {
		if bound {
			return verifiedNewAPIPolicyContext{}, false
		}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) > 3072 || json.Unmarshal(payload, &policyContext.Meta) != nil {
		if bound {
			return verifiedNewAPIPolicyContext{}, false
		}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	if !normalizeVerifiedNewAPIPolicyMeta(&policyContext.Meta) {
		policyContext.Meta = newAPIPolicyMeta{}
		if bound {
			return verifiedNewAPIPolicyContext{}, false
		}
		c.Set(newAPIPolicyMetaContextKey, policyContext)
		return policyContext, true
	}
	if bound && !strings.EqualFold(policyContext.Meta.PlatformID, normalizedNewAPIPlatform(binding.PlatformCode)) {
		return verifiedNewAPIPolicyContext{}, false
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

func normalizeVerifiedNewAPIPolicyMeta(meta *newAPIPolicyMeta) bool {
	if meta == nil {
		return false
	}
	if strings.TrimSpace(meta.PlatformID) != "" {
		platformID, ok := database.NormalizePromptFilterPlatformCode(meta.PlatformID)
		if !ok {
			return false
		}
		meta.PlatformID = platformID
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
	var ok bool
	if meta.UserName, ok = normalizedVerifiedNewAPIIdentityText(meta.UserName, 128); !ok {
		return false
	}
	if meta.UserEmail, ok = normalizedVerifiedNewAPIIdentityText(meta.UserEmail, 320); !ok {
		return false
	}
	if meta.UserGroup, ok = normalizedVerifiedNewAPIIdentityText(meta.UserGroup, 100); !ok {
		return false
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

func normalizedVerifiedNewAPIIdentityText(value string, maxRunes int) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	for _, r := range runes {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return string(runes), true
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

func (h *Handler) requiredNewAPIIdentityError(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) *api.APIError {
	binding, bound := h.resolvePromptFilterNewAPIBinding(c)
	if !bound || !binding.Enabled || !binding.RequireSignedIdentity {
		return nil
	}
	if _, verified := h.verifyNewAPIPolicyContext(c, cfg, body); verified {
		return nil
	}
	code := api.ErrorCode("newapi_signed_identity_invalid")
	message := "NewAPI 身份签名校验失败，该 Codex2API Key 仅接受其绑定平台的签名请求"
	if strings.TrimSpace(c.GetHeader("X-NewAPI-Signature")) == "" {
		code = api.ErrorCode("newapi_signed_identity_required")
		message = "该 Codex2API Key 要求绑定平台提供 NewAPI 身份签名"
	} else if strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta")) == "" || strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature")) == "" {
		code = api.ErrorCode("newapi_platform_identity_required")
		message = "该 Codex2API Key 要求绑定平台提供已签名的平台身份元数据"
	}
	// This is an authentication boundary failure, not prompt-policy evidence.
	// Do not emit violation/strike/ban headers and do not record an offense.
	return api.NewAPIError(code, message, api.ErrorTypeAuthentication)
}

func (h *Handler) requiresNewAPISignedIdentity(c *gin.Context) bool {
	binding, bound := h.resolvePromptFilterNewAPIBinding(c)
	return bound && binding.Enabled && binding.RequireSignedIdentity
}

// enforceRequiredNewAPIIdentityAtIngress runs after Codex2API API-key auth, so
// the correct one-to-one binding is known.  Body-based V1 signatures are
// verified against the exact ingress bytes and the body is restored/cached for
// the downstream handler.  Authentication failures never enter prompt-policy
// strike, ban, risk, or session state.
func (h *Handler) enforceRequiredNewAPIIdentityAtIngress(c *gin.Context) bool {
	if !h.requiresNewAPISignedIdentity(c) {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if strings.TrimSpace(c.GetHeader("X-NewAPI-Signature")) == "" {
		return h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, nil)
	}
	var body []byte
	if c != nil && c.Request != nil && c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		var err error
		body, err = readRawRequestBody(c)
		if err != nil {
			if requestUsesAnthropicErrorEnvelope(c) {
				sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
				return true
			}
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest), http.StatusBadRequest)
			return true
		}
		setIngressRequestBodyIfAbsent(c, body)
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
	}
	return h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, body)
}

func (h *Handler) rejectRequiredNewAPIIdentity(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) bool {
	apiErr := h.requiredNewAPIIdentityError(c, cfg, body)
	if apiErr == nil {
		return false
	}
	if requestUsesAnthropicErrorEnvelope(c) {
		sendAnthropicError(c, http.StatusUnauthorized, string(apiErr.Type), apiErr.Message)
		return true
	}
	api.SendErrorWithStatus(c, apiErr, http.StatusUnauthorized)
	return true
}

func requestUsesAnthropicErrorEnvelope(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := strings.TrimSuffix(c.Request.URL.Path, "/")
	return path == "/v1/messages" || path == "/v1/messages/count_tokens" || path == "/messages" || path == "/messages/count_tokens"
}

// VerifyNewAPIPolicyHandshake validates the exact signed identity headers used
// by NewAPI without invoking an upstream model or recording an offense.
func (h *Handler) VerifyNewAPIPolicyHandshake(c *gin.Context) {
	cfg := h.promptFilterConfigForRequest(c)
	if _, identityVerified := h.verifyNewAPIIdentityContext(c, cfg.Advanced.NewAPI, nil); !identityVerified {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "NewAPI 审计签名校验失败"})
		return
	}
	policyContext, ok := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil)
	if !ok {
		metaProvided := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta")) != "" || strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature")) != ""
		if metaProvided {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"success": false, "message": "NewAPI 审核档案元数据签名或格式无效", "code": "policy_meta_invalid", "identity_verified": true})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "NewAPI 审计签名校验失败"})
		return
	}
	metaProvided := strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta")) != "" || strings.TrimSpace(c.GetHeader("X-NewAPI-Policy-Meta-Signature")) != ""
	if metaProvided && !policyContext.MetaVerified {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"success": false, "message": "NewAPI 审核档案元数据签名或格式无效", "code": "policy_meta_invalid", "identity_verified": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "NewAPI 审计签名校验成功", "user_id": policyContext.Identity.UserID, "client_ip": policyContext.Identity.ClientIP, "request_id": policyContext.Identity.RequestID, "timestamp": c.GetHeader("X-NewAPI-Timestamp"), "platform": policyContext.Platform, "policy_meta_verified": policyContext.MetaVerified, "policy_meta": policyContext.Meta})
}

// sendNewAPIPolicyDecision returns a structured policy event to NewAPI. NewAPI
// is the single authority for strikes and account/IP restrictions; Codex2API
// only rejects the current request and supplies verifiable decision metadata.
func (h *Handler) sendNewAPIPolicyDecision(c *gin.Context, cfg promptfilter.Config, decision promptfilter.Decision, verdict promptfilter.Verdict, body []byte, endpoint string, model string, signedBody []byte) bool {
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return false
	}
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, body, endpoint, model, "", policyContext.VerificationSecret)
	writeNewAPIPolicyDecisionHeaders(c, metadata)
	if requestUsesAnthropicErrorEnvelope(c) {
		message := localPromptBlockMessage(cfg)
		if metadata.ReasonCode == promptConversationLockedReasonCode {
			message = promptConversationLockedMessage
		}
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", message)
		return true
	}
	api.SendErrorWithStatus(c, newAPILocalPromptPolicyDecisionAPIError(metadata, cfg), http.StatusBadRequest)
	return true
}

// emitNewAPIUpstreamCyberPolicyDecision delegates punishment to NewAPI only
// after Codex2API has observed an explicit cyber_policy response from the
// upstream provider. Local prompt matches, external-review verdicts and other
// upstream 4xx responses never use this path and therefore cannot add a strike.
func (h *Handler) emitNewAPIUpstreamCyberPolicyDecision(c *gin.Context, endpoint string, model string, upstreamBody []byte) (newAPIPolicyDecisionMetadata, bool) {
	if c == nil || upstreamCyberPolicyCode(upstreamBody) == "" {
		return newAPIPolicyDecisionMetadata{}, false
	}
	cfg := h.promptFilterConfigForRequest(c)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	if !verified {
		return newAPIPolicyDecisionMetadata{}, false
	}
	profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
	switch profile {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
	default:
		profile = promptfilter.GuardProfileBalanced
	}
	decision := promptfilter.Decision{
		Action:         promptfilter.ActionBlock,
		Profile:        profile,
		ReasonCode:     newAPIUpstreamCyberPolicyReasonCode,
		StrikeEligible: cfg.Advanced.Enforcement.CYBStrikeEnabled,
		Terminal:       true,
	}
	verdict := promptfilter.Verdict{
		Action:   promptfilter.ActionBlock,
		FullText: string(upstreamBody),
	}
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(
		policyContext.Identity,
		decision,
		verdict,
		cfg,
		upstreamBody,
		endpoint,
		model,
		promptGuardPolicyEventID(c),
		policyContext.VerificationSecret,
	)
	c.Set(newAPIUpstreamCyberDecisionContextKey, metadata)
	// Ordinary HTTP and pre-first-token SSE failures have not committed their
	// response yet, so NewAPI can consume the signed decision from headers.
	// A Responses WebSocket turn uses the signed error envelope below instead.
	if metadata.EventID == "" && !c.Writer.Written() {
		writeNewAPIPolicyDecisionHeaders(c, metadata)
	}
	return metadata, true
}

func newAPIUpstreamCyberPolicyDecision(c *gin.Context) (newAPIPolicyDecisionMetadata, bool) {
	if c == nil {
		return newAPIPolicyDecisionMetadata{}, false
	}
	value, exists := c.Get(newAPIUpstreamCyberDecisionContextKey)
	if !exists {
		return newAPIPolicyDecisionMetadata{}, false
	}
	metadata, ok := value.(newAPIPolicyDecisionMetadata)
	return metadata, ok && metadata.DecisionID != ""
}

func upstreamCyberPolicyResponseMessage(c *gin.Context) string {
	if metadata, ok := newAPIUpstreamCyberPolicyDecision(c); ok && metadata.ReasonCode == newAPIUpstreamCyberPolicyReasonCode {
		return newAPIPolicyDecisionAPIError(metadata).Message
	}
	return upstreamCyberPolicyUserMessage
}

func newAPIPolicyDecisionAPIError(metadata newAPIPolicyDecisionMetadata) *api.APIError {
	message := "请求违反安全策略，本次请求已被拒绝"
	if metadata.ReasonCode == newAPIUpstreamCyberPolicyReasonCode {
		if metadata.ConversationLocked {
			message = upstreamCyberPolicyLockedUserMessage
		} else {
			message = upstreamCyberPolicyUserMessage
		}
	} else if metadata.ReasonCode == promptConversationLockedReasonCode {
		message = promptConversationLockedMessage
	} else if metadata.ReasonCode == promptUserCyberCooldownReasonCode {
		message = promptUserCyberCooldownMessage
	}
	apiErr := api.NewAPIError(api.ErrorCode("request_policy_violation"), message, api.ErrorTypeInvalidRequest)
	apiErr.Details = newAPIPolicyDecisionDetails(metadata)
	return apiErr
}

func newAPILocalPromptPolicyDecisionAPIError(metadata newAPIPolicyDecisionMetadata, cfg promptfilter.Config) *api.APIError {
	apiErr := newAPIPolicyDecisionAPIError(metadata)
	switch metadata.ReasonCode {
	case newAPIUpstreamCyberPolicyReasonCode, promptConversationLockedReasonCode, promptUserCyberCooldownReasonCode:
		return apiErr
	default:
		apiErr.Message = localPromptBlockMessage(cfg)
		return apiErr
	}
}

func newAPIPolicyDecisionDetails(metadata newAPIPolicyDecisionMetadata) gin.H {
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
	return details
}

// attachNewAPIPolicyDecisionToResponseFailed carries the same signed decision
// used by response headers inside an SSE terminal event. This is the fallback
// for streams whose HTTP headers were committed before upstream returned CYB.
func attachNewAPIPolicyDecisionToResponseFailed(body []byte, metadata newAPIPolicyDecisionMetadata) []byte {
	if len(body) == 0 || metadata.DecisionID == "" {
		return body
	}
	details, err := json.Marshal(newAPIPolicyDecisionDetails(metadata))
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "response.error.details.codex2api_policy", details)
	if err != nil {
		return body
	}
	if message := newAPIPolicyDecisionAPIError(metadata).Message; message != "" {
		if withMessage, setErr := sjson.SetBytes(updated, "response.error.message", message); setErr == nil {
			updated = withMessage
		}
	}
	return updated
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
	// ConversationLocked is local response state only. It is intentionally not
	// included in the signed decision canonical form or forwarded as a policy
	// punishment header.
	ConversationLocked bool
}

// strikeEligibleForDecision reports whether a decision may accrue toward a
// NewAPI account ban (an irreversible consequence), and is the single source of
// truth for that boundary.
//
// Two paths can accrue a strike, both requiring an actual block:
//   - An explicit upstream cyber_policy response — the authoritative signal,
//     gated by CYBStrikeEnabled (unchanged behavior).
//   - A local highest-confidence severe violation — a current-user, sensitive,
//     terminal strict/category hit (guard pipeline sets decision.StrikeEligible
//     under exactly that conjunction) that is also terminal — gated by
//     LocalSevereStrikeEnabled. This lets a locally strangled request still
//     accrue toward the ban without first reaching the upstream, while keeping
//     the false-ban surface at the highest-confidence tier only.
//
// A conversation-lock repeat is explicitly excluded: once a conversation is
// locked, every later attempt would otherwise re-accrue and drive a single
// violation straight to the ban threshold.
func strikeEligibleForDecision(decision promptfilter.Decision, cfg promptfilter.Config) bool {
	if decision.Action != promptfilter.ActionBlock {
		return false
	}
	if decision.ReasonCode == promptConversationLockedReasonCode {
		return false
	}
	if decision.ReasonCode == newAPIUpstreamCyberPolicyReasonCode {
		return decision.StrikeEligible
	}
	return cfg.Advanced.Enforcement.LocalSevereStrikeEnabled && decision.StrikeEligible && decision.Terminal
}

func buildNewAPIPolicyDecisionMetadataWithSecret(identity newAPIIdentity, decision promptfilter.Decision, verdict promptfilter.Verdict, cfg promptfilter.Config, body []byte, endpoint string, model string, eventID string, verificationSecret string) newAPIPolicyDecisionMetadata {
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
		RequestID:  identity.RequestID,
		DecisionID: "dec_" + hex.EncodeToString(decisionDigest[:12]),
		EventID:    eventID,
		Action:     decision.Action,
		Profile:    decision.Profile,
		ReasonCode: decision.ReasonCode,
		Severity:   severity,
		// Strike eligibility (whether this decision may accrue toward a NewAPI
		// account ban) is centralized in strikeEligibleForDecision so the
		// irreversible-consequence boundary has one testable source of truth.
		StrikeEligible: strikeEligibleForDecision(decision, cfg),
		RuleVersion:    ruleVersion,
		EvidenceSHA256: hex.EncodeToString(evidenceDigest[:]),
	}
	if eventID != "" {
		metadata.EventSignature = signNewAPIPolicyEvent(verificationSecret, metadata)
	}
	metadata.Signature = signNewAPIPolicyDecision(verificationSecret, metadata)
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

// clearNewAPIUpstreamCyberPolicyDecision removes only the client-facing state
// produced by a failed upstream attempt. The audit incident has already been
// recorded and is intentionally retained. Call this only when another
// upstream attempt is actually about to start, so a final failure still
// exposes the last decision to NewAPI.
func clearNewAPIUpstreamCyberPolicyDecision(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(newAPIUpstreamCyberDecisionContextKey, nil)
	if c.Writer == nil || c.Writer.Written() {
		return
	}
	for _, name := range []string{
		"X-Codex2API-Policy-Violation",
		"X-Codex2API-Policy-Request-ID",
		"X-Codex2API-Policy-Reason",
		"X-Codex2API-Policy-Action",
		"X-Codex2API-Policy-Decision-ID",
		"X-Codex2API-Policy-Event-ID",
		"X-Codex2API-Policy-Event-Signature-Version",
		"X-Codex2API-Policy-Event-Signature",
		"X-Codex2API-Policy-Profile",
		"X-Codex2API-Policy-Rule-Version",
		"X-Codex2API-Policy-Strike-Eligible",
		"X-Codex2API-Policy-Evidence-SHA256",
		"X-Codex2API-Policy-Severity",
		"X-Codex2API-Policy-Signature-Version",
		"X-Codex2API-Policy-Response-Signature",
		"X-Codex2API-Policy-Strike",
		"X-Codex2API-Policy-Ban",
	} {
		c.Writer.Header().Del(name)
	}
}
