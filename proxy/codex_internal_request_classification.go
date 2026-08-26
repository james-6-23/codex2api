package proxy

import (
	"strings"

	"github.com/gin-gonic/gin"
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
