package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const continuousRetryPolicyRequestContextKey = "continuous_retry_policy_snapshot"

func continuousRetryPolicyForCall(policies []database.ContinuousRetryPolicy) database.ContinuousRetryPolicy {
	if len(policies) > 0 {
		return database.NormalizeContinuousRetryPolicy(policies[0])
	}
	return database.NormalizeContinuousRetryPolicy(CurrentRuntimeSettings().ContinuousRetryPolicy)
}

func rememberContinuousRetryPolicyForRequest(c *gin.Context, policy database.ContinuousRetryPolicy) {
	if c == nil {
		return
	}
	c.Set(continuousRetryPolicyRequestContextKey, database.NormalizeContinuousRetryPolicy(policy))
}

func continuousRetryPolicyForRequest(c *gin.Context) database.ContinuousRetryPolicy {
	if c != nil {
		if value, exists := c.Get(continuousRetryPolicyRequestContextKey); exists {
			if policy, ok := value.(database.ContinuousRetryPolicy); ok {
				return database.NormalizeContinuousRetryPolicy(policy)
			}
		}
	}
	return continuousRetryPolicyForCall(nil)
}

// continuousRetryBuffersAttempts is true for both selector modes. Once an
// operator enables continuous retry, an error can arrive after arbitrary
// stream output, so the whole attempt must stay private until its terminal
// outcome is known. Catch-all only changes which upstream failures retry.
func continuousRetryBuffersAttempts(policy database.ContinuousRetryPolicy) bool {
	return database.NormalizeContinuousRetryPolicy(policy).Enabled
}

func continuousRetryPreflightPassthrough(settings RuntimeSettings) bool {
	policy := database.NormalizeContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	return settings.CodexPreflightSSEPassthrough && !policy.Enabled
}

func continuousRetryHTTPSelected(policy database.ContinuousRetryPolicy, status int, body []byte) bool {
	if !policy.Enabled {
		return false
	}
	// 明确的上游 CYB 是请求级安全终态；即使开启 catch-all 也不能换号重放。
	// An explicit upstream CYB is a request-level safety terminal and must not
	// rotate accounts even when catch-all is enabled.
	if isExplicitUpstreamCyberPolicy(body) {
		return false
	}
	if isExplicitUpstreamSafetyPolicy(body) && !policy.CatchesAllUpstreamFailures() {
		return false
	}
	// A generic 429 category should not turn a permanent billing/quota failure
	// into an endless account-rotation loop. An operator can still opt into that
	// exact marker or deliberately enable the high-risk catch-all override.
	if isPermanentQuotaFailure(body) && !policy.CatchesAllUpstreamFailures() && !policy.MatchesErrorCodes(body) {
		return false
	}
	if policy.MatchesHTTP(status, body) {
		return true
	}
	// Context-window failures are commonly returned as a 400 body (or inside a
	// response.failed frame) rather than a dedicated status. Keep the category
	// useful for both HTTP and streaming transports without making every 400
	// retryable by default.
	return policy.HasCategory(database.ContinuousRetryCategoryContextError) && isContextRetryError(body)
}

func continuousRetryLimitsForHTTP(status int, body []byte, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) (int, int) {
	policy := continuousRetryPolicyForCall(policies)
	if !continuousRetryHTTPSelected(policy, status, body) {
		return generalLimit, rateLimit
	}
	if status == http.StatusTooManyRequests {
		return generalLimit, -1
	}
	return -1, rateLimit
}

func continuousRetryLimitForRequestError(err error, generalLimit int, policies ...database.ContinuousRetryPolicy) int {
	policy := continuousRetryPolicyForCall(policies)
	if err == nil || errors.Is(err, context.Canceled) {
		return generalLimit
	}
	if isContinuousRetryLocalFailure(err) {
		return generalLimit
	}
	if isExplicitUpstreamCyberPolicyError(err) {
		return generalLimit
	}
	if status, body, ok := continuousRetryHTTPErrorDetails(err); ok {
		if continuousRetryHTTPSelected(policy, status, body) {
			return -1
		}
		// A WebSocket handshake HTTP response is not a transport failure. Keep
		// its legacy finite budget unless its status/body was selected explicitly.
		return generalLimit
	}
	if policy.MatchesTransport(err.Error()) {
		return -1
	}
	return generalLimit
}

// continuousRetryRequestErrorSelected recognizes status-bearing upstream
// failures that never materialize as an *http.Response, notably WebSocket
// handshake errors. The small interface keeps proxy independent from the
// wsrelay package while allowing exact status/error-code selectors to work on
// both transports.
type continuousRetryHTTPError interface {
	UpstreamStatusCode() int
	UpstreamErrorBody() []byte
}

func continuousRetryHTTPErrorDetails(err error) (int, []byte, bool) {
	var upstreamErr continuousRetryHTTPError
	if !errors.As(err, &upstreamErr) {
		return 0, nil, false
	}
	status := upstreamErr.UpstreamStatusCode()
	if status < 100 || status > 999 {
		return 0, nil, false
	}
	return status, upstreamErr.UpstreamErrorBody(), true
}

// isExplicitUpstreamCyberPolicyError also inspects statusless structured
// upstream errors. Their status cannot participate in HTTP selection, but the
// machine-readable CYB body must still retain hard-stop precedence.
func isExplicitUpstreamCyberPolicyError(err error) bool {
	var upstreamErr continuousRetryHTTPError
	return errors.As(err, &upstreamErr) && isExplicitUpstreamCyberPolicy(upstreamErr.UpstreamErrorBody())
}

func continuousRetryRequestErrorSelected(policy database.ContinuousRetryPolicy, err error) bool {
	if !policy.Enabled || err == nil {
		return false
	}
	if errors.Is(err, errContinuousRetryDeadlineExceeded) || isContinuousRetryLocalFailure(err) {
		return false
	}
	if isExplicitUpstreamCyberPolicyError(err) {
		return false
	}
	status, body, ok := continuousRetryHTTPErrorDetails(err)
	if !ok {
		if !policy.CatchesAllUpstreamFailures() || errors.Is(err, context.Canceled) {
			return false
		}
		var upstreamErr *Error
		return errors.As(err, &upstreamErr) &&
			upstreamErr.Type == ErrorTypeUpstreamError
	}
	return continuousRetryHTTPSelected(policy, status, body)
}

func continuousRetryStreamSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	policy := continuousRetryPolicyForCall(policies)
	if !policy.Enabled || outcome.terminalLocal {
		return false
	}
	eventType = normalizedUpstreamSSEEventType(eventType, payload)
	if !isActualUpstreamStreamFailure(outcome, eventType) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(outcome.failureKind), "cyber_policy") || isExplicitUpstreamCyberPolicy(payload) || isExplicitUpstreamCyberPolicy(outcome.failurePayload) {
		return false
	}
	if isExplicitUpstreamSafetyPolicy(payload) && !policy.CatchesAllUpstreamFailures() {
		return false
	}
	if isPermanentQuotaFailure(responseFailedErrorBody(payload)) && !policy.CatchesAllUpstreamFailures() && !policy.MatchesErrorCodes(payload) {
		return false
	}
	if policy.CatchesAllUpstreamFailures() {
		return true
	}
	// Stream outcomes also use synthetic statuses for logging (an unknown error
	// frame falls back to 500 and a broken stream uses 598). Those values are not
	// HTTP evidence and must not activate an HTTP selector.
	if status, evidenced := responseFailedStatusCodeWithEvidence(payload); evidenced {
		if continuousRetryHTTPSelected(policy, status, payload) {
			return true
		}
	}
	if policy.MatchesErrorCodes(payload) {
		return true
	}
	if policy.HasCategory(database.ContinuousRetryCategoryContextError) && isContextRetryError(payload) {
		return true
	}
	if eventType == "response.failed" && policy.HasCategory(database.ContinuousRetryCategoryResponseFailed) {
		return true
	}
	// stream_error covers an explicit in-stream error event as well as a read
	// failure or abnormal EOF. response.failed remains its own selector: a
	// provider failure terminal must not enter stream_error merely because its
	// synthetic/log status resembles a broken stream.
	if eventType == "error" && policy.HasCategory(database.ContinuousRetryCategoryStreamError) {
		return true
	}
	streamReadFailure := eventType != "response.failed" &&
		(outcome.logStatusCode == logStatusUpstreamStreamBreak ||
			outcome.failureKind == "transport" || outcome.failureKind == "timeout")
	return streamReadFailure &&
		(policy.HasCategory(database.ContinuousRetryCategoryTransport) ||
			policy.HasCategory(database.ContinuousRetryCategoryStreamError))
}

// isActualUpstreamStreamFailure is the success gate shared by selective and
// catch-all policies. A catch-all changes which failures are retried; it must
// never turn a clean terminal response into another paid upstream attempt.
func isActualUpstreamStreamFailure(outcome streamOutcome, eventType string) bool {
	if outcome.terminalLocal || outcome.logStatusCode == logStatusClientClosed {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "error", "response.failed":
		return true
	}
	if outcome.penalize || strings.TrimSpace(outcome.failureKind) != "" {
		return true
	}
	return outcome.logStatusCode != 0 && outcome.logStatusCode != http.StatusOK
}

func normalizedUpstreamSSEEventType(eventName string, payload []byte) string {
	// An explicit SSE error event is authoritative even when a non-standard
	// provider puts an error subtype (for example invalid_request_error) in the
	// JSON `type` field. Otherwise that frame can be mistaken for ordinary data
	// and leak through catch-all mode.
	if eventType := strings.TrimSpace(eventName); strings.EqualFold(eventType, "error") {
		return "error"
	}
	if eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String()); eventType != "" {
		return eventType
	}
	if eventType := strings.TrimSpace(eventName); eventType != "" {
		return eventType
	}
	if errorValue := gjson.GetBytes(payload, "error"); errorValue.Exists() && errorValue.Type != gjson.Null {
		return "error"
	}
	return ""
}

var emptyUpstreamErrorEventPayload = []byte(`{"type":"error","error":{"type":"upstream_error","code":"upstream_error","message":"Upstream returned an empty error event"}}`)

func terminalUpstreamErrorPayload(payload []byte) []byte {
	if len(bytes.TrimSpace(payload)) == 0 {
		return append([]byte(nil), emptyUpstreamErrorEventPayload...)
	}
	return append([]byte(nil), payload...)
}

// continuousRetryStreamFailureSelected includes the legacy retryable outcome
// classification and the opt-in selector. The former preserves existing
// finite WebSocket behavior; the latter allows a policy to deliberately opt
// into account-scoped 4xx/context/error-frame failures whose legacy outcome is
// not penalized.
func continuousRetryStreamFailureSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	if outcome.terminalLocal || strings.EqualFold(strings.TrimSpace(outcome.failureKind), "continuous_retry_timeout") {
		return false
	}
	if isExplicitUpstreamCyberPolicy(payload) || isExplicitUpstreamCyberPolicy(outcome.failurePayload) || strings.EqualFold(strings.TrimSpace(outcome.failureKind), "cyber_policy") {
		return false
	}
	return outcome.penalize || continuousRetryStreamSelected(outcome, payload, eventType, policies...)
}

func isContextRetryError(payload []byte) bool {
	value := strings.ToLower(string(payload))
	for _, marker := range []string{
		"context_length_exceeded",
		"context_window_exceeded",
		"context_window",
		"string_above_max_length",
		"input_too_long",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// isExplicitUpstreamSafetyPolicy identifies deterministic provider safety
// refusals so selective policies do not accidentally turn a broad 4xx/5xx
// category into an endless loop. Catch-all may still select non-CYB refusals,
// while explicit cyber_policy always remains a hard stop. Match structured
// machine codes only so a harmless mention of "content policy" in an error
// message cannot disable a genuinely recoverable selective retry.
// 这里只匹配结构化机器码，不扫描自由文本 message，避免误伤可恢复故障。
func isExplicitUpstreamSafetyPolicy(payload []byte) bool {
	if isExplicitUpstreamCyberPolicy(payload) {
		return true
	}
	body := responseFailedErrorBody(payload)
	for _, path := range []string{
		"error.code",
		"error.type",
		"response.error.code",
		"response.error.type",
		"response.status_details.error.code",
		"response.status_details.error.type",
		"incomplete_details.reason",
		"response.incomplete_details.reason",
		"code",
		"type",
	} {
		marker := strings.ToLower(strings.TrimSpace(firstGJSONString(body, path)))
		switch marker {
		case "content_policy",
			"content_policy_violation",
			"content_filter",
			"invalid_prompt",
			"jailbreak",
			"refusal",
			"sanitizer_error",
			"image_generation_user_error",
			"unsupported_country_region_territory",
			"policy_violation",
			"responsible_ai_policy_violation",
			"responsibleaipolicyviolation",
			"safety_policy",
			"safety_policy_violation",
			"safety_violation",
			"image_safety_violation",
			"safety_blocked",
			"blocked_by_safety",
			"moderation_blocked",
			"blocked_by_moderation":
			return true
		}
	}
	return false
}

func continuousRetryLimitsForStream(outcome streamOutcome, payload []byte, eventType string, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) (int, int) {
	if !continuousRetryStreamSelected(outcome, payload, eventType, policies...) {
		return generalLimit, rateLimit
	}
	if streamOutcomeUsesRateLimitBudget(outcome) {
		return generalLimit, -1
	}
	return -1, rateLimit
}
