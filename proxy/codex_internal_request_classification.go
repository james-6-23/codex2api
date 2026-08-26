package proxy

import (
	"strings"

	"github.com/gin-gonic/gin"
)

const localSessionAccountingBypassContextKey = "local_session_accounting_bypass_v1"
const passiveInternalAuthorizationContextKey = "passive_internal_authorization_v1"

type localSessionAccountingBypass struct {
	Feature string
}

type passiveInternalAuthorization struct {
	Feature string
}

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

// classifyLocalCodexAmbientSessionAccounting recognizes a coherent standalone
// Codex system task from its native session fields. Payload wording, model
// names and client fingerprints deliberately do not participate: those values
// are release details, while thread_source is the protocol classification.
// When the API key configured a project-title group, the same system field is
// routed by classifyProjectTitleRequest instead of being treated as ambient.
func classifyLocalCodexAmbientSessionAccounting(c *gin.Context, _ []byte, root requestRootSessionIdentity) (string, bool) {
	if c == nil || !root.stable || root.conflict || !root.nativeRoot || root.related ||
		!strings.EqualFold(strings.TrimSpace(root.threadSource), "system") {
		return "", false
	}
	if row := apiKeyRowFromContext(c); row != nil && row.Limits.ProjectTitleGroupID > 0 {
		return "", false
	}
	return newAPIPassiveFeatureSystemPassive, true
}

// classifyVerifiedNewAPIAmbientSessionAccounting consumes NewAPI's signed
// field classification without reparsing the request body. Signature and
// metadata normalization are the trust boundary; Codex2API must not become
// coupled to model aliases or prompt templates owned by a client release.
func (h *Handler) classifyVerifiedNewAPIAmbientSessionAccounting(c *gin.Context, _ []byte) (string, bool) {
	if h == nil || c == nil {
		return "", false
	}
	status, policyContext := h.cachedNewAPIPolicyAuditState(c)
	if (status != "verified" && status != "signed_response") || !policyContext.MetaVerified ||
		policyContext.Meta.SessionAccounting != newAPISessionAccountingBypass {
		return "", false
	}
	feature := strings.TrimSpace(policyContext.Meta.PassiveFeature)
	if feature == "" {
		return "", false
	}
	return feature, true
}

func resetCodexInternalRequestClassificationFrame(c *gin.Context) {
	if c == nil {
		return
	}
	setLocalSessionAccountingBypass(c, "", false)
	c.Set(projectTitleRequestContextKey, nil)
	c.Set(relatedSessionObservationContextKey, nil)
	c.Set(passiveInternalAuthorizationContextKey, nil)
	cacheTrustedRequestedModel(c, "")
}

func setPassiveInternalAuthorization(c *gin.Context, feature string, enabled bool) {
	if c == nil {
		return
	}
	if !enabled {
		c.Set(passiveInternalAuthorizationContextKey, nil)
		return
	}
	c.Set(passiveInternalAuthorizationContextKey, passiveInternalAuthorization{Feature: strings.TrimSpace(feature)})
}

func passiveInternalRequestAuthorized(c *gin.Context) bool {
	if c == nil {
		return false
	}
	raw, exists := c.Get(passiveInternalAuthorizationContextKey)
	authorization, ok := raw.(passiveInternalAuthorization)
	return exists && ok && strings.TrimSpace(authorization.Feature) != ""
}

func setLocalSessionAccountingBypass(c *gin.Context, feature string, enabled bool) {
	if c == nil {
		return
	}
	if !enabled {
		c.Set(localSessionAccountingBypassContextKey, nil)
		return
	}
	c.Set(localSessionAccountingBypassContextKey, localSessionAccountingBypass{Feature: feature})
}

func requestSessionAccountingBypass(c *gin.Context) bool {
	if c == nil {
		return false
	}
	raw, exists := c.Get(localSessionAccountingBypassContextKey)
	_, ok := raw.(localSessionAccountingBypass)
	return exists && ok
}
