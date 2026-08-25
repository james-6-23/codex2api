package proxy

import (
	"strings"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const localSessionAccountingBypassContextKey = "local_session_accounting_bypass_v1"
const passiveInternalAuthorizationContextKey = "passive_internal_authorization_v1"

type localSessionAccountingBypass struct {
	Feature string
}

type passiveInternalAuthorization struct {
	Feature string
}

func normalizeCodexInternalRequestedModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if base, stripped := stripCompactModelSuffix(model); stripped {
		model = strings.ToLower(strings.TrimSpace(base))
	}
	return model
}

// classifyLocalGuardianApprovalRoot recovers the exact user-visible task from
// the reviewed-session marker emitted by Codex's approval reassessment turn.
// It is used only when no authenticated NewAPI policy supplied a root.
func classifyLocalGuardianApprovalRoot(c *gin.Context, body []byte) (string, bool) {
	model := trustedRequestedModel(c, gjson.GetBytes(body, "model").String())
	model = normalizeCodexInternalRequestedModel(model)
	if model != "gpt-5.6-luna" && model != "codex-auto-review" {
		return "", false
	}
	if _, ok := promptfilter.ClosedApprovalReassessmentText(body); !ok {
		return "", false
	}
	envelope := promptfilter.BuildEnvelopeWithModelsAndConfig(
		body, "/v1/responses", model, model, promptfilter.TransportHTTP, promptfilter.Config{},
	)
	rootID, ok := promptfilter.ApprovalReassessmentReviewedSession(envelope)
	rootID = strings.ToLower(strings.TrimSpace(rootID))
	return rootID, ok && validSessionGraphUUID(rootID)
}

// classifyLocalCodexAmbientSessionAccounting recognizes only a coherent native
// Codex root task marked as a system turn and using one of the known ambient
// models. It keeps direct Codex2API traffic equivalent to signed NewAPI traffic
// without granting the bypass to an arbitrary Session-Id or source label.
func classifyLocalCodexAmbientSessionAccounting(c *gin.Context, body []byte, root requestRootSessionIdentity) (string, bool) {
	if c == nil || c.Request == nil || !root.stable || root.conflict || !root.nativeRoot || root.related ||
		!strings.EqualFold(strings.TrimSpace(root.threadSource), "system") ||
		!EvaluateEngineFingerprint(c.Request.Header, body, nil) {
		return "", false
	}
	model := normalizeCodexInternalRequestedModel(trustedRequestedModel(c, gjson.GetBytes(body, "model").String()))
	switch model {
	case "gpt-5.4-mini":
		if !localAmbientSafetyBodyContract(body) {
			return "", false
		}
		envelope := promptfilter.BuildEnvelopeWithModelsAndConfig(
			body, "/v1/responses", model, model, promptfilter.TransportHTTP, promptfilter.Config{},
		)
		kind := promptfilter.ClassifyKnownApplicationPromptKind(envelope)
		if kind == "ambient_safety" || kind == "ambient_safety_drift" {
			return newAPIPassiveFeatureAmbientSafety, true
		}
	case "gpt-5.6-terra", "gpt-5.4":
		if !codexInternalBodyHasExecutionSurface(body) && localAmbientSuggestionsPrompt(body) {
			return newAPIPassiveFeatureAmbientSuggestions, true
		}
	}
	return "", false
}

// classifyVerifiedNewAPIAmbientSessionAccounting revalidates the signed
// gateway classification against the actual body seen by Codex2API. A valid
// signature authenticates who supplied the metadata; it does not make a
// client-controlled thread_source label or prompt wrapper sufficient by
// itself to bypass account/user window accounting.
func (h *Handler) classifyVerifiedNewAPIAmbientSessionAccounting(c *gin.Context, body []byte) (string, bool) {
	if h == nil || c == nil {
		return "", false
	}
	status, policyContext := h.cachedNewAPIPolicyAuditState(c)
	if (status != "verified" && status != "signed_response") || !policyContext.MetaVerified ||
		policyContext.Meta.SessionAccounting != newAPISessionAccountingBypass {
		return "", false
	}
	model := normalizeCodexInternalRequestedModel(policyContext.Meta.RequestedModel)
	switch policyContext.Meta.PassiveFeature {
	case newAPIPassiveFeatureAmbientSuggestions:
		if (model == "gpt-5.6-terra" || model == "gpt-5.4") &&
			!codexInternalBodyHasExecutionSurface(body) && localAmbientSuggestionsPrompt(body) {
			return newAPIPassiveFeatureAmbientSuggestions, true
		}
	case newAPIPassiveFeatureAmbientSafety:
		if model == "gpt-5.4-mini" && localAmbientSafetyBodyContract(body) {
			envelope := promptfilter.BuildEnvelopeWithModelsAndConfig(
				body, "/v1/responses", model, model, promptfilter.TransportHTTP, promptfilter.Config{},
			)
			kind := promptfilter.ClassifyKnownApplicationPromptKind(envelope)
			if kind == "ambient_safety" || kind == "ambient_safety_drift" {
				return newAPIPassiveFeatureAmbientSafety, true
			}
		}
	}
	return "", false
}

func localAmbientSuggestionsPrompt(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if instruction := root.Get("instructions"); instruction.Exists() && strings.TrimSpace(instruction.String()) != "" {
		return false
	}
	inputResult := root.Get("input")
	if inputResult.Type != gjson.String {
		return false
	}
	input := strings.ToLower(strings.TrimSpace(inputResult.String()))
	if !strings.Contains(input, "generate 0 to 3 hyperpersonalized suggestions") ||
		!strings.Contains(input, "what this user can do with codex in this local project") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(root.Get("text.format.type").String()), "json_schema") {
		return false
	}
	schema := root.Get("text.format.schema")
	properties := schema.Get("properties")
	suggestions := properties.Get("suggestions")
	return schema.IsObject() && strings.EqualFold(strings.TrimSpace(schema.Get("type").String()), "object") &&
		properties.IsObject() && len(properties.Map()) == 1 && suggestions.IsObject() &&
		strings.EqualFold(strings.TrimSpace(suggestions.Get("type").String()), "array")
}

func localAmbientSafetyBodyContract(body []byte) bool {
	if codexInternalBodyHasExecutionSurface(body) || !gjson.ValidBytes(body) {
		return false
	}
	const instruction = "Classify Codex ambient suggestion candidates for policy safety."
	return strings.TrimSpace(gjson.GetBytes(body, "instructions").String()) == instruction
}

func codexInternalBodyHasExecutionSurface(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return true
	}
	root := gjson.ParseBytes(body)
	for _, path := range []string{"tools", "tool_choice", "previous_response_id", "messages", "prompt", "conversation", "context_management"} {
		value := root.Get(path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		switch {
		case value.Type == gjson.String && strings.TrimSpace(value.String()) == "":
			continue
		case value.IsArray() && len(value.Array()) == 0:
			continue
		case value.IsObject() && len(value.Map()) == 0:
			continue
		default:
			return true
		}
	}
	return false
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
