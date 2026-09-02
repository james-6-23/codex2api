package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// Cloud Code returns google.rpc.Status envelopes on 429/503. The interesting
// parts live in error.details[]: an ErrorInfo carrying reason + metadata.model
// and a RetryInfo carrying retryDelay. Reading them lets the gateway tell a
// per-account model quota (switch account, cool down for exactly retryDelay)
// apart from a shared-pool capacity shortage (never switch, never penalise)
// and from a sub-second rate limit (wait in place instead of burning an
// account switch).
type antigravityQuotaKind string

const (
	antigravityQuotaKindNone           antigravityQuotaKind = ""
	antigravityQuotaKindRateLimit      antigravityQuotaKind = "rate_limit"
	antigravityQuotaKindQuotaExhausted antigravityQuotaKind = "quota_exhausted"
	antigravityQuotaKindModelCapacity  antigravityQuotaKind = "model_capacity"
)

const (
	antigravityReasonRateLimitExceeded      = "RATE_LIMIT_EXCEEDED"
	antigravityReasonQuotaExhausted         = "QUOTA_EXHAUSTED"
	antigravityReasonModelCapacityExhausted = "MODEL_CAPACITY_EXHAUSTED"

	// antigravityInstantRetryThreshold: a RATE_LIMIT_EXCEEDED whose retryDelay
	// is below this is cheaper to wait out on the same account than to hand
	// the request to another account (which would also lose prompt cache).
	antigravityInstantRetryThreshold = 3 * time.Second
	antigravityInstantRetryAttempts  = 2
	// antigravityQuotaExhaustedThreshold: a RATE_LIMIT_EXCEEDED with a retry
	// delay this long is really a quota window, not a burst limit.
	antigravityQuotaExhaustedThreshold = 5 * time.Minute

	// MODEL_CAPACITY_EXHAUSTED is a model-wide shortage shared by every
	// account, so switching accounts cannot help. Retry in place for a bounded
	// number of one-second waits, then let the request fail over normally.
	antigravityModelCapacityRetryAttempts = 8
	antigravityModelCapacityRetryWait     = time.Second
	antigravityModelCapacityCooldown      = 10 * time.Second

	antigravityDefaultRateLimitCooldown = 30 * time.Second
	antigravityDefaultQuotaCooldown     = 5 * time.Minute
	antigravityMaxQuotaCooldown         = 30 * time.Minute

	// antigravityGeminiFamilyCooldownKey is the synthetic model key that a
	// QUOTA_EXHAUSTED on any Gemini model also cools down. Cloud Code meters
	// Gemini usage in one shared bucket (retrieveUserQuotaSummary reports a
	// single "Gemini Models" group), so one exhausted Gemini model means the
	// account's other Gemini tiers are exhausted too.
	antigravityGeminiFamilyCooldownKey = "antigravity:gemini"
)

// antigravitySleep is the wait used for same-endpoint retries. Injectable so
// tests do not spend real seconds.
var antigravitySleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type antigravityQuotaError struct {
	Kind          antigravityQuotaKind
	Reason        string
	Model         string
	Message       string
	RetryDelay    time.Duration
	HasRetryDelay bool
}

// parseAntigravityQuotaError classifies a 429/503 error body. ok is false
// when the body carries no recognisable quota signal, in which case callers
// keep their generic handling.
func parseAntigravityQuotaError(statusCode int, body []byte) (antigravityQuotaError, bool) {
	if statusCode != http.StatusTooManyRequests && statusCode != http.StatusServiceUnavailable {
		return antigravityQuotaError{}, false
	}
	if len(body) == 0 {
		return antigravityQuotaError{}, false
	}
	result := antigravityQuotaError{
		Message: strings.TrimSpace(gjson.GetBytes(body, "error.message").String()),
	}
	rpcStatus := strings.ToUpper(strings.TrimSpace(gjson.GetBytes(body, "error.status").String()))
	for _, detail := range gjson.GetBytes(body, "error.details").Array() {
		typeName := detail.Get("@type").String()
		switch {
		case strings.HasSuffix(typeName, "google.rpc.ErrorInfo"):
			if reason := strings.ToUpper(strings.TrimSpace(detail.Get("reason").String())); reason != "" && result.Reason == "" {
				result.Reason = reason
			}
			if model := strings.TrimSpace(detail.Get("metadata.model").String()); model != "" && result.Model == "" {
				result.Model = antigravityNormalizeQuotaModel(model)
			}
			if !result.HasRetryDelay {
				if delay, ok := parseAntigravityRetryDelay(detail.Get("metadata.quotaResetDelay")); ok {
					result.RetryDelay, result.HasRetryDelay = delay, true
				}
			}
		case strings.HasSuffix(typeName, "google.rpc.RetryInfo"):
			if delay, ok := parseAntigravityRetryDelay(detail.Get("retryDelay")); ok {
				result.RetryDelay, result.HasRetryDelay = delay, true
			}
		}
	}
	lowerBody := strings.ToLower(string(body))
	switch result.Reason {
	case antigravityReasonModelCapacityExhausted:
		result.Kind = antigravityQuotaKindModelCapacity
	case antigravityReasonQuotaExhausted:
		result.Kind = antigravityQuotaKindQuotaExhausted
	case antigravityReasonRateLimitExceeded:
		if result.HasRetryDelay && result.RetryDelay >= antigravityQuotaExhaustedThreshold {
			result.Kind = antigravityQuotaKindQuotaExhausted
		} else {
			result.Kind = antigravityQuotaKindRateLimit
		}
	default:
		switch {
		case strings.Contains(lowerBody, "model_capacity_exhausted"):
			result.Kind = antigravityQuotaKindModelCapacity
		case strings.Contains(lowerBody, "quota_exhausted") || strings.Contains(lowerBody, "quota exhausted") ||
			strings.Contains(lowerBody, "exhausted your capacity on this model"):
			result.Kind = antigravityQuotaKindQuotaExhausted
		case statusCode == http.StatusTooManyRequests && (rpcStatus == "RESOURCE_EXHAUSTED" || result.HasRetryDelay):
			result.Kind = antigravityQuotaKindRateLimit
		default:
			return antigravityQuotaError{}, false
		}
	}
	return result, true
}

// parseAntigravityRetryDelay accepts the proto-JSON duration string form
// ("4m50s", "0.201506475s") and the object form ({"seconds": 4, "nanos": 5}).
func parseAntigravityRetryDelay(value gjson.Result) (time.Duration, bool) {
	if !value.Exists() {
		return 0, false
	}
	switch value.Type {
	case gjson.String:
		text := strings.TrimSpace(value.String())
		if text == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(text); err == nil && d >= 0 {
			return d, true
		}
		return 0, false
	case gjson.Number:
		if seconds := value.Float(); seconds >= 0 {
			return time.Duration(seconds * float64(time.Second)), true
		}
		return 0, false
	case gjson.JSON:
		seconds := value.Get("seconds")
		nanos := value.Get("nanos")
		if !seconds.Exists() && !nanos.Exists() {
			return 0, false
		}
		d := time.Duration(seconds.Float()*float64(time.Second)) + time.Duration(nanos.Int())
		if d < 0 {
			return 0, false
		}
		return d, true
	default:
		return 0, false
	}
}

// antigravityRetryAfterDuration parses an HTTP Retry-After header given either
// as delta-seconds or as an HTTP date.
func antigravityRetryAfterDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// antigravityNormalizeQuotaModel strips Vertex-style resource prefixes so the
// model in ErrorInfo.metadata compares equal to the wire model we sent.
func antigravityNormalizeQuotaModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(normalized, "/models/"); idx >= 0 {
		normalized = normalized[idx+len("/models/"):]
	}
	return strings.TrimPrefix(normalized, "models/")
}

// antigravitySameEndpointRetryBudget bounds the in-place retries performed
// inside ExecuteAntigravityResponsesRequest before the response is handed back
// to the handler for account-level handling.
type antigravitySameEndpointRetryBudget struct {
	capacityAttempts int
	instantAttempts  int
}

// retryDelay reports whether a retryable Cloud Code failure should be waited
// out on the same endpoint and account, and for how long.
func (b *antigravitySameEndpointRetryBudget) retryDelay(statusCode int, body []byte) (time.Duration, bool) {
	if b == nil {
		return 0, false
	}
	quota, ok := parseAntigravityQuotaError(statusCode, body)
	if !ok {
		return 0, false
	}
	switch quota.Kind {
	case antigravityQuotaKindModelCapacity:
		if b.capacityAttempts >= antigravityModelCapacityRetryAttempts {
			return 0, false
		}
		b.capacityAttempts++
		wait := antigravityModelCapacityRetryWait
		if quota.HasRetryDelay && quota.RetryDelay > 0 && quota.RetryDelay < antigravityInstantRetryThreshold {
			wait = quota.RetryDelay
		}
		return wait, true
	case antigravityQuotaKindRateLimit:
		if !quota.HasRetryDelay || quota.RetryDelay <= 0 || quota.RetryDelay >= antigravityInstantRetryThreshold {
			return 0, false
		}
		if b.instantAttempts >= antigravityInstantRetryAttempts {
			return 0, false
		}
		b.instantAttempts++
		return quota.RetryDelay, true
	default:
		return 0, false
	}
}

// antigravityIsGeminiModel reports whether a public or wire model id belongs
// to the shared Gemini quota bucket.
func antigravityIsGeminiModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini")
}

// antigravityAccountModelRateLimited is the single admission check for an
// Antigravity account: the public id, the resolved wire id, and (for Gemini)
// the shared family key must all be clear.
func antigravityAccountModelRateLimited(account *auth.Account, model, wireModel string) bool {
	if account == nil {
		return false
	}
	if account.IsModelRateLimited(model) || account.IsModelRateLimited(wireModel) {
		return true
	}
	if antigravityIsGeminiModel(model) || antigravityIsGeminiModel(wireModel) {
		return account.IsModelRateLimited(antigravityGeminiFamilyCooldownKey)
	}
	return false
}

// antigravityNonPenalizingUpstreamFailure reports failures that say nothing
// about the account's health: a model-wide capacity shortage is Google's
// problem, not this credential's, so ReportRequestFailure must not count it.
func antigravityNonPenalizingUpstreamFailure(account *auth.Account, statusCode int, body []byte) bool {
	if account == nil || !account.IsAntigravityAPI() {
		return false
	}
	quota, ok := parseAntigravityQuotaError(statusCode, body)
	return ok && quota.Kind == antigravityQuotaKindModelCapacity
}

// applyAntigravityCooldown turns a Cloud Code 429/503 into a per-(account,
// model) cooldown sized from the upstream retry hint. Unrecognised 429 bodies
// fall back to the generic relay-style cooldown policy.
func applyAntigravityCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	quota, ok := parseAntigravityQuotaError(statusCode, body)
	if !ok {
		if statusCode == http.StatusTooManyRequests {
			return Apply429Cooldown(store, account, body, resp, model)
		}
		return codex429Decision{}
	}
	decision := codex429Decision{Scope: rateLimitScopeModel, Model: strings.TrimSpace(model)}
	var cooldown time.Duration
	switch quota.Kind {
	case antigravityQuotaKindModelCapacity:
		decision.Reason = "model_capacity"
		cooldown = antigravityModelCapacityCooldown
	case antigravityQuotaKindQuotaExhausted:
		decision.Reason = "quota_exhausted"
		cooldown = antigravityDefaultQuotaCooldown
		if quota.HasRetryDelay && quota.RetryDelay > 0 {
			cooldown = quota.RetryDelay
		}
	default:
		decision.Reason = "rate_limited_model"
		cooldown = antigravityDefaultRateLimitCooldown
		if quota.HasRetryDelay && quota.RetryDelay > 0 {
			cooldown = quota.RetryDelay
		}
	}
	if resp != nil && !quota.HasRetryDelay {
		if retryAfter, ok := antigravityRetryAfterDuration(resp.Header.Get("Retry-After")); ok {
			cooldown = retryAfter
		}
	}
	if cooldown < time.Second {
		cooldown = time.Second
	}
	if cooldown > antigravityMaxQuotaCooldown {
		cooldown = antigravityMaxQuotaCooldown
	}
	resetAt := time.Now().Add(cooldown)
	decision.ResetAt = resetAt
	decision.Cooldown = cooldown
	if store == nil || account == nil || decision.Model == "" {
		return decision
	}
	marked := store.MarkModelCooldownUntil(account, decision.Model, decision.Reason, resetAt)
	if !marked.ResetAt.IsZero() {
		decision.ResetAt = marked.ResetAt
		decision.Cooldown = time.Until(marked.ResetAt)
	}
	if quota.Kind == antigravityQuotaKindQuotaExhausted && (antigravityIsGeminiModel(decision.Model) || antigravityIsGeminiModel(quota.Model)) {
		store.MarkModelCooldownUntil(account, antigravityGeminiFamilyCooldownKey, decision.Reason, resetAt)
	}
	return decision
}
