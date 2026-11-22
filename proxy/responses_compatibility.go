package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// strictResponsesCompatibilityHeader lets clients opt in to fail-closed
// validation when the Codex upstream cannot preserve an official Responses
// field. The default remains backward compatible because the Codex channel
// still needs to normalize fields that the upstream rejects.
const strictResponsesCompatibilityHeader = "X-Codex2API-Strict-Compatibility"

// These top-level fields are currently normalized away by the Codex relay.
// Keep this list explicit: adding a field here is a compatibility decision,
// not a generic JSON sanitizer change.
var codexRelayResponsesCompatibilityFields = []string{
	"max_output_tokens",
	"max_tokens",
	"max_completion_tokens",
	"context_management",
	"temperature",
	"top_p",
	"frequency_penalty",
	"presence_penalty",
	"logprobs",
	"top_logprobs",
	"n",
	"seed",
	"stop",
	"user",
	"logit_bias",
	"metadata",
	"stream_options",
	"truncation",
	"disable_response_storage",
	"prompt_cache_retention",
	"safety_identifier",
	"verbosity",
	"serviceTier",
}

// Selection, rather than an API key's channel label, determines which payload
// preparation runs. The Codex/auto pools can also select a Responses relay.
func selectedCodexResponsesCompatibilityError(c *gin.Context, account *auth.Account, rawBody []byte) *api.APIError {
	if account == nil || account.IsRelayStyle() || !strictResponsesCompatibilityRequested(c) {
		return nil
	}
	return validateCodexRelayResponsesCompatibility(rawBody)
}

func sendResponsesCompatibilityError(c *gin.Context, err *api.APIError) {
	if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
		return
	}
	if retryKeepaliveCommitted(c) && strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		payload, _ := json.Marshal(gin.H{"type": "response.failed", "response": gin.H{"status": "failed", "error": err}})
		_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	api.SendError(c, err)
}

func strictResponsesCompatibilityRequested(c *gin.Context) bool {
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.GetHeader(strictResponsesCompatibilityHeader))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_STRICT_RESPONSES_COMPAT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// validateCodexRelayResponsesCompatibility returns an actionable error before
// translation can silently change the request. It is deliberately opt-in so
// existing clients keep the historical Codex normalization behavior until
// they have migrated to a capability-aware route.
func validateCodexRelayResponsesCompatibility(rawBody []byte) *api.APIError {
	var body map[string]json.RawMessage
	if len(rawBody) == 0 || json.Unmarshal(rawBody, &body) != nil {
		return nil
	}
	issues := make([]map[string]string, 0, len(codexRelayResponsesCompatibilityFields))
	addIssue := func(field, reason string) {
		issues = append(issues, map[string]string{"field": field, "reason": reason})
	}
	nonNull := func(field string) (json.RawMessage, bool) {
		value, present := body[field]
		return value, present && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
	}
	for _, field := range codexRelayResponsesCompatibilityFields {
		if _, present := nonNull(field); !present {
			continue
		}
		addIssue(field, "Codex relay currently cannot preserve this Responses field")
	}
	// false/null request synchronous execution and do not require background
	// lifecycle endpoints. A string is the existing image background extension.
	if background, present := nonNull("background"); present && !bytes.Equal(bytes.TrimSpace(background), []byte("false")) {
		var imageBackground string
		if json.Unmarshal(background, &imageBackground) != nil {
			addIssue("background", "Codex relay does not implement background response lifecycle")
		}
	}
	if _, present := nonNull("conversation"); present {
		addIssue("conversation", "Codex relay does not implement the Conversations API")
	}
	if store, present := nonNull("store"); present && !bytes.Equal(bytes.TrimSpace(store), []byte("false")) {
		addIssue("store", "Codex relay cannot provide official stored-response retrieval")
	}
	if tier, present := nonNull("service_tier"); present {
		var requested string
		_ = json.Unmarshal(tier, &requested)
		requested = strings.TrimSpace(requested)
		if _, preserved := upstreamServiceTier(requested); !preserved && requested != "auto" {
			addIssue("service_tier", "Codex relay would discard the requested service tier")
		}
	}
	var model string
	_ = json.Unmarshal(body["model"], &model)
	checkEffort := func(field string, value json.RawMessage) {
		var requested string
		if json.Unmarshal(value, &requested) != nil || normalizeReasoningEffortForModel(requested, model) != strings.ToLower(strings.TrimSpace(requested)) {
			addIssue(field, "Codex relay would change the requested reasoning effort")
		}
	}
	if effort, present := nonNull("reasoning_effort"); present {
		checkEffort("reasoning_effort", effort)
	}
	var reasoning map[string]json.RawMessage
	if json.Unmarshal(body["reasoning"], &reasoning) == nil {
		if effort, present := reasoning["effort"]; present && !bytes.Equal(bytes.TrimSpace(effort), []byte("null")) {
			checkEffort("reasoning.effort", effort)
		}
	}
	// The legacy Chat response_format alias is translated to text.format.
	if format, present := nonNull("response_format"); present {
		var decoded map[string]any
		if json.Unmarshal(format, &decoded) != nil || responsesTextFormatFromResponseFormat(decoded) == nil {
			addIssue("response_format", "Codex relay cannot translate this response format")
		}
	}
	for _, issue := range codexResponsesSchemaCompatibilityIssues(rawBody) {
		addIssue(issue["field"], issue["reason"])
	}
	if len(issues) == 0 {
		return nil
	}
	return api.NewAPIErrorWithDetails(
		api.ErrCodeUnsupportedFeature,
		"请求包含当前 Codex relay 无法原样保留的 Responses 字段",
		api.ErrorTypeInvalidRequest,
		map[string]any{
			"transport": "codex-relay",
			"fields":    issues,
			"hint":      "移除这些字段，或使用支持原生 Responses 的上游配置",
		},
	)
}
