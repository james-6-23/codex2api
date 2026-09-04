package proxy

import (
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const localSessionAccountingBypassContextKey = "local_session_accounting_bypass_v1"
const passiveInternalAuthorizationContextKey = "passive_internal_authorization_v1"

// classifyNativeRelatedInternal accepts a coherent standalone Codex child
// graph. Prompt wording, model aliases and client-fingerprint policy belong at
// the edge gateway; Codex2API consumes only the resolved session fields.
func classifyNativeRelatedInternal(identity requestRootSessionIdentity) bool {
	if !identity.nativeRoot || !identity.stable || identity.conflict ||
		!identity.related || strings.TrimSpace(identity.sessionID) == "" {
		return false
	}
	threadSource := strings.TrimSpace(identity.threadSource)
	return threadSource != "" && !strings.EqualFold(threadSource, "user")
}

// classifyLocalCodexIndependentSessionAccounting recognizes a coherent
// standalone Codex-owned background root from its native session fields.
// Payload wording, model names and client fingerprints deliberately do not
// participate: thread_source is the protocol classification. Every non-user
// source remains an independent, non-window request.
func classifyLocalCodexIndependentSessionAccounting(c *gin.Context, root requestRootSessionIdentity) bool {
	if c == nil || !root.stable || root.conflict || !root.nativeRoot || root.related ||
		strings.TrimSpace(root.threadSource) == "" || strings.EqualFold(strings.TrimSpace(root.threadSource), "user") {
		return false
	}
	return true
}

// verifiedNewAPISessionAccountingBypass consumes NewAPI's signed
// field classification without reparsing the request body. Signature and
// metadata normalization are the trust boundary; Codex2API must not become
// coupled to model aliases or prompt templates owned by a client release.
func (h *Handler) verifiedNewAPISessionAccountingBypass(c *gin.Context) bool {
	if h == nil || c == nil {
		return false
	}
	status, policyContext := h.cachedNewAPIPolicyAuditState(c)
	if (status != "verified" && status != "signed_response") || !policyContext.MetaVerified ||
		policyContext.Meta.SessionAccounting != newAPISessionAccountingBypass {
		return false
	}
	return strings.TrimSpace(policyContext.Meta.PassiveFeature) != ""
}

func (h *Handler) classifyPassiveInternalRequest(c *gin.Context, root requestRootSessionIdentity) bool {
	if root.conflict {
		return false
	}
	if passiveInternalRequestAuthorized(c) {
		return true
	}
	if h == nil || h.store == nil || !h.store.PassiveInternalModelsEnabled() {
		return false
	}
	status, policyContext := h.cachedNewAPIPolicyAuditState(c)
	verified := (status == "verified" || status == "signed_response") && policyContext.MetaVerified
	if !verified {
		return classifyLocalCodexIndependentSessionAccounting(c, root)
	}
	if h.verifiedNewAPISessionAccountingBypass(c) {
		return true
	}
	source := strings.TrimSpace(policyContext.Meta.ThreadSource)
	return root.authoritative && !root.stable &&
		policyContext.Meta.RootSessionState == newAPIPolicyRootSessionUnavailable &&
		source != "" && !strings.EqualFold(source, "user")
}

func (h *Handler) passiveInternalModelsAllowed(c *gin.Context) bool {
	return h != nil && h.store != nil && h.store.PassiveInternalModelsEnabled() && passiveInternalRequestAuthorized(c)
}

func (h *Handler) passiveInternalModelValidator(c *gin.Context, body []byte, validate api.ValidationRule) api.ValidationRule {
	return func(value gjson.Result, path string) *api.ValidationError {
		validationError := validate(value, path)
		if validationError == nil || h == nil || h.store == nil || !h.store.PassiveInternalModelsEnabled() {
			return validationError
		}
		signedBody := ingressRequestBody(c, body)
		if c != nil && isResponsesWebSocketUpgradeRequest(c.Request) {
			signedBody = nil
		}
		h.primeNewAPIPolicyContext(c, signedBody)
		h.resolveRequestRootSessionIdentityForContext(c, body)
		if h.passiveInternalModelsAllowed(c) {
			return nil
		}
		return validationError
	}
}

func resetCodexInternalRequestClassificationFrame(c *gin.Context) {
	if c == nil {
		return
	}
	setLocalSessionAccountingBypass(c, false)
	c.Set(relatedSessionObservationContextKey, nil)
	c.Set(passiveInternalAuthorizationContextKey, nil)
	cacheTrustedRequestedModel(c, "")
}

func setPassiveInternalAuthorization(c *gin.Context, enabled bool) {
	if c == nil {
		return
	}
	c.Set(passiveInternalAuthorizationContextKey, enabled)
}

func passiveInternalRequestAuthorized(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(passiveInternalAuthorizationContextKey)
}

func setLocalSessionAccountingBypass(c *gin.Context, enabled bool) {
	if c == nil {
		return
	}
	c.Set(localSessionAccountingBypassContextKey, enabled)
}

func requestSessionAccountingBypass(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(localSessionAccountingBypassContextKey)
}
