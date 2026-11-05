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
	// antigravityUnclassifiedRateLimitCooldown backs off a 429 that carries no
	// google.rpc detail at all. Cloud Code answers with a bare
	// {"code":429,"status":"RESOURCE_EXHAUSTED"} when it sheds load, and that
	// body names neither a reason nor a retry delay, so nothing in it points at
	// the credential that received it. Charging it the full default bench is
	// what turns a shared upstream saturation into a self-inflicted outage:
	// every account handed the same bare 429 leaves the rotation, and a 30s
	// absence per account empties the pool. Stay above
	// antigravityInstantRetryThreshold so the request is not spun in place, but
	// short enough that the rotation refills.
	antigravityUnclassifiedRateLimitCooldown = 5 * time.Second
	antigravityDefaultQuotaCooldown          = 5 * time.Minute
	antigravityMaxQuotaCooldown              = 30 * time.Minute
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

// antigravityIsGeminiModel reports whether a public or wire model id is Gemini.
func antigravityIsGeminiModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini")
}

// antigravityGeminiQuotaCooldownKey separates Argon's independent quota pool
// from ordinary Gemini, whose models still share a family-wide quota fence.
// Use a new namespace: the legacy antigravity:gemini key could have been written
// by either pool and cannot safely be attributed to one during a rolling upgrade.
func antigravityGeminiQuotaCooldownKey(model, wireModel string) string {
	if !antigravityIsGeminiModel(model) && !antigravityIsGeminiModel(wireModel) {
		return ""
	}
	if strings.Contains(strings.ToLower(model), "argon") || strings.Contains(strings.ToLower(wireModel), "argon") {
		return "antigravity:quota-family:argon"
	}
	return "antigravity:quota-family:gemini"
}

// antigravityAccountModelRateLimited is the single admission check for an
// Antigravity account: the public id, the resolved wire id, and (for Gemini)
// the appropriate quota-family key must all be clear.
func antigravityAccountModelRateLimited(account *auth.Account, model, wireModel string) bool {
	if account == nil {
		return false
	}
	if account.IsModelRateLimited(model) || account.IsModelRateLimited(wireModel) {
		return true
	}
	if familyKey := antigravityGeminiQuotaCooldownKey(model, wireModel); familyKey != "" {
		return account.IsModelRateLimited(familyKey)
	}
	return false
}

// antigravitySharedWireCooldownKey 报告「把冷却写到公开档位名 model 上」这一动作是否
// 兼任共享 wire 冷却键的写入。命中时这次写入必须遵守 deadline 单调性（AtLeast 变体），
// 不能被最后写入者胜出地改短。
//
// 两种情形：
//  1. 账号解析出的 wire 与请求的公开档位同名（wireModel == model）。典型就是 Argon：
//     gemini-4-argon-low/medium/high 三个公开档位的 backing 都是 gemini-4-argon-high，
//     而 high 这一档的公开名与 wire 名完全相同，于是「写 public 冷却」与「写共享 wire
//     冷却」是同一次写入——public 键在这里就是共享键本身。
//  2. 目录里另有别的公开档位 / 逻辑档位把 model 解析成自己的上游 backing（请求直接
//     带原始 backing id，或原始 id 与某个公开档位同名）。
//
// 两种情形下，一个 5s 的裸 RESOURCE_EXHAUSTED 都可能把兄弟档位刚写入的 290s 共享冷却
// 改短：短 deadline 一过，同 backing 的兄弟档位又会选中刚被限流的凭据，继续收集 429。
func antigravitySharedWireCooldownKey(model, wireModel string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	if trimmed == "" {
		return false
	}
	if wireModel != "" && strings.EqualFold(strings.TrimSpace(wireModel), trimmed) {
		return true
	}
	return antigravityModelSharedBySiblings(trimmed)
}

// antigravityModelSharedBySiblings 报告固定档位目录或逻辑档位目录中是否存在「其它」公开
// 档位，把 model 解析成自己的上游 backing。model 自身的定义不算：公开档位解析到自己的
// backing 是常态，只有别的档位也骑上来才构成共享键（公开名与自己同名的情形由调用方的
// wireModel == model 分支单独覆盖）。
func antigravityModelSharedBySiblings(model string) bool {
	for _, catalog := range [][]antigravityPublicModelDefinition{
		antigravityPublicModelCatalog,
		antigravityLogicalCompatibilityCatalog,
	} {
		for _, definition := range catalog {
			if strings.EqualFold(definition.id, model) {
				continue
			}
			for _, wire := range AntigravityWireModelIDs(definition.id) {
				if strings.EqualFold(wire, model) {
					return true
				}
			}
		}
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
// fall back to the generic relay-style cooldown policy, and a recognised body
// that carries no hint at all is backed off briefly instead of being charged
// the full default bench.
//
// Live dispatch also feeds the scheduling penalty accumulator through here (see
// auth.RecordAntigravityRateLimitEvent); admin connectivity probes reuse the
// same cooldown mapping without that accounting via ApplyAntigravityCooldown.
func applyAntigravityCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	return applyAntigravityCooldownWithPenalty(store, account, statusCode, body, resp, model, true)
}

// antigravityRateLimitEvent normalises one 429/503 observation into the event
// shape the fresh-selection penalty understands. parsed is false when the body
// carried no recognisable google.rpc signal at all.
func antigravityRateLimitEvent(statusCode int, model, wireModel string, quota antigravityQuotaError, parsed bool, resetAt time.Time) auth.AntigravityRateLimitEvent {
	event := auth.AntigravityRateLimitEvent{
		StatusCode:  statusCode,
		PublicModel: strings.TrimSpace(model),
		WireModel:   strings.TrimSpace(wireModel),
		ResetAt:     resetAt,
		ObservedAt:  time.Now(),
	}
	if antigravityIsGeminiModel(event.PublicModel) || antigravityIsGeminiModel(event.WireModel) {
		event.Family = antigravityGeminiQuotaCooldownKey(model, wireModel)
	}
	if !parsed {
		// A 429 with no google.rpc detail says nothing about this credential,
		// so it is charged half a penalty instead of a full one.
		event.Kind = "unstructured_rate_limit"
		event.Scope = auth.AGScopeUnknown
		event.Weight = auth.AntigravityPenaltyWeightUnclassifiedRateLimit
		return event
	}
	event.Kind = string(quota.Kind)
	switch quota.Kind {
	case antigravityQuotaKindModelCapacity:
		// A model-wide shortage is Google's problem; switching accounts cannot
		// help, so the scheduler must not steer away from this credential.
		event.Scope = auth.AGScopeSharedCapacity
		event.Weight = auth.AntigravityPenaltyWeightNone
	case antigravityQuotaKindQuotaExhausted:
		// Already covered by the long per-model cooldown plus its quota
		// family cooldown; a short-term penalty would only double-count it.
		event.Scope = auth.AGScopeAccountQuota
		event.Weight = auth.AntigravityPenaltyWeightNone
	default:
		event.Scope = auth.AGScopeModelRateLimit
		if quota.Reason != "" || quota.HasRetryDelay {
			event.Weight = auth.AntigravityPenaltyWeightExplicitRateLimit
		} else {
			event.Weight = auth.AntigravityPenaltyWeightUnclassifiedRateLimit
		}
	}
	return event
}

// applyAntigravityCooldownWithPenalty 是 Antigravity 429/503 冷却落地的共同实现：解析
// 结构化 google.rpc 信号定出冷却时长（优先尊重 RetryInfo 与 Retry-After 头，无明确提示
// 时按 Kind 分类默认值处理，裸限流退到短回退；完全解析失败的非结构化 429 走通用 429 策略），
// 写入公开模型键。对于共享的 wire 键统一只延长、不改短；Gemini family 冷却键仅在
// QUOTA_EXHAUSTED 且归属 Gemini 模型时写入且同样只延长不改短。recordPenalty 为 false
// 时只落冷却、不进进程内避险惩罚累加器（admin 连接性探测走这一档）。
func applyAntigravityCooldownWithPenalty(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string, recordPenalty bool) codex429Decision {
	quota, ok := parseAntigravityQuotaError(statusCode, body)
	if !ok {
		if statusCode == http.StatusTooManyRequests {
			// wire 解析要在通用 429 策略之前完成：它既决定下面的共享键判定，也决定
			// 兄弟档位的 wire 补写。
			wireModel := ""
			if account != nil {
				if wire, resolved := antigravityResolvePublicModelForAccount(account, model); resolved {
					wireModel = strings.TrimSpace(wire)
				}
			}
			// 公开档位名本身就是共享 wire 键时（gemini-4-argon-high），先记住既有 deadline：
			// 通用 429 策略按「最后写入者胜出」覆盖 public 冷却，会把同账号兄弟档位刚写入的
			// 长冷却改短。
			sharedWireKey := store != nil && account != nil && antigravitySharedWireCooldownKey(model, wireModel)
			var previous auth.ModelCooldown
			if sharedWireKey {
				previous, _ = account.ModelCooldownFor(model)
			}
			decision := Apply429Cooldown(store, account, body, resp, model)
			// 非结构化 429（Cloud Code 负载抑制时只回 {"code":429,"status":
			// "RESOURCE_EXHAUSTED"}）走通用 429 策略，而通用策略只按 public id 落冷却。
			// 多个公开档位共用一个 wire 模型时，同 wire 的兄弟档位仍会被选中，账号带着
			// 刚被限流的凭据再次上游。这里补写 wire 冷却；解析不到 wire（非目录模型 /
			// 账号不具备该 wire）时跳过。wire 是共享冷却键，用 AtLeast 变体写入，避免本次
			// 更短的 deadline 把同账号上更长的 wire 冷却改短。
			//
			// public 兼任共享 wire 键时（wireModel == decision.Model）没有单独的 wire 键可
			// 补写：通用策略那次覆盖写就是共享键写入，因此这里把被改短的长冷却写回。
			// 只处理 model 作用域且本次确实写了冷却的情况，账号作用域由账号冷却负责，
			// 不应在这里被模型冷却复活。
			if !previous.ResetAt.IsZero() && decision.Scope == rateLimitScopeModel && !decision.ResetAt.IsZero() &&
				previous.ResetAt.After(decision.ResetAt) && strings.EqualFold(strings.TrimSpace(decision.Model), strings.TrimSpace(model)) {
				if restored := store.MarkModelCooldownUntilAtLeast(account, decision.Model, previous.Reason, previous.ResetAt); !restored.ResetAt.IsZero() {
					decision.ResetAt = restored.ResetAt
					decision.Cooldown = time.Until(restored.ResetAt)
				}
			}
			if store != nil && account != nil && decision.Model != "" && !decision.ResetAt.IsZero() {
				if wireModel != "" && !strings.EqualFold(wireModel, decision.Model) {
					store.MarkModelCooldownUntilAtLeast(account, wireModel, decision.Reason, decision.ResetAt)
				}
			}
			if recordPenalty {
				auth.RecordAntigravityRateLimitEvent(account, antigravityRateLimitEvent(statusCode, decision.Model, wireModel, antigravityQuotaError{}, false, decision.ResetAt))
			}
			return decision
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
		switch {
		case quota.HasRetryDelay && quota.RetryDelay > 0:
			// Google told us exactly how long to wait.
			cooldown = quota.RetryDelay
		case quota.Reason != "":
			// A named rate limit (RATE_LIMIT_EXCEEDED) without a delay is still
			// an account-level signal, so keep the conservative default.
			cooldown = antigravityDefaultRateLimitCooldown
		default:
			// A bare RESOURCE_EXHAUSTED: no ErrorInfo, no RetryInfo. Nothing in
			// it says this credential is the reason, so back off briefly rather
			// than taking the account out of rotation for the full bench.
			cooldown = antigravityUnclassifiedRateLimitCooldown
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
	// wire 解析必须在 public 冷却写入之前完成：公开档位名与 wire 同名时，下面这次
	// public 写入就是共享 wire 键的唯一一次写入，写入变体要据此选择。
	wireModel := ""
	if wire, resolved := antigravityResolvePublicModelForAccount(account, decision.Model); resolved {
		wireModel = strings.TrimSpace(wire)
	}
	// public 冷却默认保持「最后写入者胜出」的既有覆盖语义。唯一例外是 public 名本身
	// 兼任共享 wire 冷却键（gemini-4-argon-high 既是公开档位，也是 low/medium 的上游
	// backing）：此时覆盖写会把兄弟档位刚写入的 290s 共享冷却改成 5s，短窗口一过
	// low/medium 又选中刚被限流的凭据。这类键与 wire、family 一样属于同账号上的共享
	// 冷却键，必须用 AtLeast 变体，只延长、不改短。
	sharedWireKey := antigravitySharedWireCooldownKey(decision.Model, wireModel)
	var marked auth.ModelCooldown
	if sharedWireKey {
		marked = store.MarkModelCooldownUntilAtLeast(account, decision.Model, decision.Reason, resetAt)
	} else {
		marked = store.MarkModelCooldownUntil(account, decision.Model, decision.Reason, resetAt)
	}
	if !marked.ResetAt.IsZero() {
		decision.ResetAt = marked.ResetAt
		decision.Cooldown = time.Until(marked.ResetAt)
	}
	// Public tiers may share one upstream backing (gemini-3.8-flash-low/medium/
	// high all ride gemini-3.8-flash-tiered). Cooling only the public id leaves
	// the sibling tiers selectable on the very account the upstream just
	// throttled, so the wire id is cooled with the same deadline.
	//
	// wire 与 family 都是同账号上的共享冷却键，会被多个公开档位的事件并发写入，因此
	// 用 AtLeast 变体：high 的 290s RATE_LIMIT_EXCEEDED 之后，同账号 low 的 5s 裸
	// RESOURCE_EXHAUSTED 只能延长、不能把共享 deadline 改短，否则短窗口一过兄弟档位
	// 又会选中刚被限流的凭据。public 与 wire 同名时没有第二个键可写，单调性由上面的
	// sharedWireKey 分支保证。
	if wireModel != "" && !strings.EqualFold(wireModel, decision.Model) {
		store.MarkModelCooldownUntilAtLeast(account, wireModel, decision.Reason, decision.ResetAt)
	}
	if quota.Kind == antigravityQuotaKindQuotaExhausted {
		familyKey := antigravityGeminiQuotaCooldownKey(decision.Model, wireModel)
		// Upstream metadata can identify the Gemini pool behind a non-Gemini
		// request. Use it only when the request has no known Gemini family.
		if familyKey == "" {
			familyKey = antigravityGeminiQuotaCooldownKey(quota.Model, "")
		}
		if familyKey != "" {
			store.MarkModelCooldownUntilAtLeast(account, familyKey, decision.Reason, decision.ResetAt)
		}
	}
	if recordPenalty {
		auth.RecordAntigravityRateLimitEvent(account, antigravityRateLimitEvent(statusCode, decision.Model, wireModel, quota, true, decision.ResetAt))
	}
	return decision
}

// ApplyAntigravityCooldown lets admin connection tests reuse the Cloud Code
// 429/503 quota mapping used by live dispatch: a per-(account, model) cooldown
// sized from Google's structured retry hint, with the generic 429 policy as
// fallback. It reports whether a cooldown was actually recorded so callers can
// classify the failure as rate limiting instead of a broken account.
//
// A connectivity probe is not user traffic: it keeps the cooldown write but
// stays out of the live scheduling penalty accumulator, otherwise one manual
// test button would bias fresh selection away from an otherwise healthy
// account.
func ApplyAntigravityCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) bool {
	if statusCode != http.StatusTooManyRequests && statusCode != http.StatusServiceUnavailable {
		return false
	}
	decision := applyAntigravityCooldownWithPenalty(store, account, statusCode, body, resp, model, false)
	return !decision.ResetAt.IsZero()
}
