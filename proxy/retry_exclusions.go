package proxy

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
)

type retryAccountExclusions struct {
	hard        map[int64]bool
	soft        map[int64]bool
	transient   map[int64]bool
	recoverable map[int64]bool
}

// websocketHTTPFallbackState carries the already-acquired account lease across
// a one-time WebSocket -> HTTP transport downgrade. A close 1009 is a transport
// limitation, not a reason to release the account and run the scheduler again.
type websocketHTTPFallbackState struct {
	forcedHTTP bool
	account    *auth.Account
	proxyURL   string
	wsElapsed  time.Duration
	source     string
	fallbackID string
	startedAt  time.Time
}

func (s *websocketHTTPFallbackState) Retain(account *auth.Account, proxyURL string, wsElapsed time.Duration, source string) {
	if s == nil || account == nil {
		return
	}
	s.forcedHTTP = true
	s.account = account
	s.proxyURL = proxyURL
	s.wsElapsed = wsElapsed
	s.source = source
	if s.startedAt.IsZero() {
		s.startedAt = time.Now().Add(-wsElapsed)
	}
	if s.fallbackID == "" {
		s.fallbackID = uuid.NewString()
	}
}

func (s *websocketHTTPFallbackState) Take() (*auth.Account, string, bool) {
	if s == nil || s.account == nil {
		return nil, "", false
	}
	account := s.account
	proxyURL := s.proxyURL
	s.account = nil
	s.proxyURL = ""
	return account, proxyURL, true
}

func (s *websocketHTTPFallbackState) ForceHTTP() bool {
	return s != nil && s.forcedHTTP
}

func (s *websocketHTTPFallbackState) WSElapsed() time.Duration {
	if s == nil {
		return 0
	}
	return s.wsElapsed
}

func (s *websocketHTTPFallbackState) ID() string {
	if s == nil {
		return ""
	}
	return s.fallbackID
}

func (s *websocketHTTPFallbackState) Source() string {
	if s == nil || s.source == "" {
		return "peer_close"
	}
	return s.source
}

func (s *websocketHTTPFallbackState) LogHTTPAttemptCompletion(endpoint string, accountID int64, attemptIndex, httpElapsedMs, httpFirstEventMs, statusCode int) {
	if s == nil || !s.forcedHTTP {
		return
	}
	wsElapsedMs := s.wsElapsed.Milliseconds()
	totalElapsedMs := wsElapsedMs + int64(httpElapsedMs)
	if !s.startedAt.IsZero() {
		totalElapsedMs = time.Since(s.startedAt).Milliseconds()
	}
	totalFirstEventMs := int64(0)
	if httpFirstEventMs > 0 {
		postFirstEventMs := int64(httpElapsedMs - httpFirstEventMs)
		if postFirstEventMs < 0 {
			postFirstEventMs = 0
		}
		totalFirstEventMs = totalElapsedMs - postFirstEventMs
		if totalFirstEventMs < 0 {
			totalFirstEventMs = 0
		}
	}
	log.Printf("WebSocket 1009 HTTP 降级尝试结束 (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=%s, status=%d, ws_elapsed_ms=%d, http_elapsed_ms=%d, http_first_event_ms=%d, total_first_event_ms=%d, total_elapsed_ms=%d)",
		s.fallbackID, s.Source(), attemptIndex, accountID, endpoint, statusCode, wsElapsedMs, httpElapsedMs, httpFirstEventMs,
		totalFirstEventMs, totalElapsedMs)
}

func newRetryAccountExclusions() *retryAccountExclusions {
	return &retryAccountExclusions{
		hard:        make(map[int64]bool),
		soft:        make(map[int64]bool),
		transient:   make(map[int64]bool),
		recoverable: make(map[int64]bool),
	}
}

func (r *retryAccountExclusions) MarkHard(accountID int64) {
	if r == nil || accountID == 0 {
		return
	}
	r.hard[accountID] = true
	delete(r.soft, accountID)
	delete(r.transient, accountID)
	delete(r.recoverable, accountID)
}

// MarkTransient excludes an account for the current pass through the pool.
// Unlike a hard exclusion, it may be cleared after every candidate has seen a
// recoverable transport, overload, or rate-limit failure in continuous mode.
func (r *retryAccountExclusions) MarkTransient(accountID int64) {
	if r == nil || accountID == 0 {
		return
	}
	if r.hard[accountID] {
		return
	}
	delete(r.soft, accountID)
	r.transient[accountID] = true
	r.recoverable[accountID] = true
}

func (r *retryAccountExclusions) MarkTransportFailure(accountID int64, retryLimit int, policies ...database.ContinuousRetryPolicy) {
	r.MarkRequestFailure(accountID, nil, retryLimit, policies...)
}

// MarkRequestFailure classifies a status-bearing or plain transport error for
// account selection. Keeping the original error matters for WebSocket
// handshake failures and exact error-code selectors: the retry budget may have
// been promoted to unlimited by the policy even though the legacy transport
// classifier only sees a generic dial error.
func (r *retryAccountExclusions) MarkRequestFailure(accountID int64, err error, retryLimit int, policies ...database.ContinuousRetryPolicy) {
	if isExplicitUpstreamCyberPolicyError(err) {
		r.MarkHard(accountID)
		return
	}
	policy := continuousRetryPolicyForCall(policies)
	if _, _, statusBearing := continuousRetryHTTPErrorDetails(err); statusBearing {
		if continuousRetryRequestErrorSelected(policy, err) {
			r.MarkTransient(accountID)
			return
		}
		r.MarkHard(accountID)
		return
	}
	selected := policy.CatchesAllUpstreamFailures() || policy.HasCategory(database.ContinuousRetryCategoryTransport)
	if err != nil {
		selected = policy.MatchesTransport(err.Error())
	}
	if selected || retryLimit == -1 {
		r.MarkTransient(accountID)
		return
	}
	r.MarkHard(accountID)
}

// MarkHTTPFailure keeps account-local and deterministic failures excluded for
// the lifetime of the request, while allowing genuinely recoverable failures
// to participate in another pool cycle when continuous retry is enabled.
func (r *retryAccountExclusions) MarkHTTPFailure(accountID int64, statusCode int, body []byte, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) {
	if isExplicitUpstreamCyberPolicy(body) {
		r.MarkHard(accountID)
		return
	}
	policy := continuousRetryPolicyForCall(policies)
	if continuousRetryHTTPSelected(policy, statusCode, body) {
		r.MarkTransient(accountID)
		return
	}
	retryLimit := generalLimit
	if statusCode == http.StatusTooManyRequests {
		retryLimit = rateLimit
	}
	if retryLimit == -1 && isTransientRetryHTTPFailure(statusCode, body) {
		r.MarkTransient(accountID)
		return
	}
	r.MarkHard(accountID)
}

func (r *retryAccountExclusions) MarkStreamFailure(accountID int64, outcome streamOutcome, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) {
	r.MarkStreamFailureForEvent(accountID, outcome, "", generalLimit, rateLimit, policies...)
}

func (r *retryAccountExclusions) MarkStreamFailureForEvent(accountID int64, outcome streamOutcome, eventType string, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) {
	failureKind := strings.ToLower(strings.TrimSpace(outcome.failureKind))
	if failureKind == "cyber_policy" || isExplicitUpstreamCyberPolicy(outcome.failurePayload) {
		r.MarkHard(accountID)
		return
	}
	if continuousRetryStreamSelected(outcome, outcome.failurePayload, eventType, policies...) {
		r.MarkTransient(accountID)
		return
	}
	retryLimit := generalLimit
	if outcome.logStatusCode == http.StatusTooManyRequests || strings.Contains(failureKind, "rate_limit") {
		retryLimit = rateLimit
	}
	if retryLimit != -1 {
		r.MarkHard(accountID)
		return
	}
	if outcome.capacityShed {
		r.MarkTransient(accountID)
		return
	}
	switch failureKind {
	case "transport", "timeout", "server", "rate_limited", "rate_limited_model", "model_capacity":
		r.MarkTransient(accountID)
		return
	case "usage_limit", "usage_limited", "unauthorized", "forbidden", "deactivated_workspace", "payment_required_unknown", "version_required", "client", "cyber_policy":
		r.MarkHard(accountID)
		return
	}
	if isTransientRetryHTTPFailure(outcome.logStatusCode, nil) || outcome.logStatusCode == logStatusUpstreamStreamBreak {
		r.MarkTransient(accountID)
		return
	}
	r.MarkHard(accountID)
}

func isTransientRetryHTTPFailure(statusCode int, body []byte) bool {
	if isPermanentQuotaFailure(body) {
		return false
	}
	switch statusCode {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isPermanentQuotaFailure(body []byte) bool {
	if IsUsageLimitReachedError(body) {
		return true
	}
	value := strings.ToLower(strings.Join([]string{
		firstGJSONString(body, "error.code", "response.error.code", "response.status_details.error.code", "code"),
		firstGJSONString(body, "error.type", "response.error.type", "response.status_details.error.type", "type"),
		firstGJSONString(body, "error.message", "response.error.message", "response.status_details.error.message", "message"),
	}, " "))
	for _, marker := range []string{
		"insufficient_quota",
		"quota_exceeded",
		"quota_exhausted",
		"billing_hard_limit",
		"billing_limit_reached",
		"spend_limit",
		"credit_balance",
		"insufficient_balance",
		"usage_limited",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (r *retryAccountExclusions) MarkSoftFirstTokenTimeout(accountID int64) {
	r.MarkSoft(accountID)
}

// MarkSoft 把账号加入本次请求的软排除集：调度选号时跳过它，但账号池试完后由
// ResetSoft 清空重来，不会永久搁置请求。用于"重试时暂时避开该账号但不惩罚它"。
func (r *retryAccountExclusions) MarkSoft(accountID int64) {
	if r == nil || accountID == 0 {
		return
	}
	if r.hard[accountID] || r.transient[accountID] {
		return
	}
	r.soft[accountID] = true
}

func (r *retryAccountExclusions) ResetSoft() bool {
	if r == nil || len(r.soft) == 0 {
		return false
	}
	r.soft = make(map[int64]bool)
	return true
}

func (r *retryAccountExclusions) ResetTransient() bool {
	if r == nil || len(r.transient) == 0 {
		return false
	}
	r.transient = make(map[int64]bool)
	return true
}

// hasResettable reports request-local exclusions that the selection loop
// clears itself once the pool pass is exhausted (soft first, then transient).
func (r *retryAccountExclusions) hasResettable() bool {
	return r != nil && (len(r.soft) > 0 || len(r.transient) > 0)
}

// forHardSelection is the exclude set left once every resettable exclusion
// has been cleared.
func (r *retryAccountExclusions) forHardSelection() map[int64]bool {
	if r == nil || len(r.hard) == 0 {
		return nil
	}
	exclude := make(map[int64]bool, len(r.hard))
	for id := range r.hard {
		exclude[id] = true
	}
	return exclude
}

// onlyResettableExclusionsBlock reports that a queue wait carrying exclude
// cannot make progress, but clearing this request's soft/transient
// exclusions would expose a candidate. Saturation is ignored on both sides,
// so a pool that is merely busy still waits normally.
func (h *Handler) onlyResettableExclusionsBlock(affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, exclude map[int64]bool, filter auth.AccountFilter, preserveBinding bool, policy auth.DispatchPolicy) bool {
	if !exclusions.hasResettable() {
		return false
	}
	if h.store.HasDispatchCandidate(affinityKey, apiKeyID, exclude, filter, preserveBinding, policy) {
		return false
	}
	return h.store.HasDispatchCandidate(affinityKey, apiKeyID, exclusions.forHardSelection(), filter, preserveBinding, policy)
}

func (r *retryAccountExclusions) CanContinueTransientCycle() bool {
	return r != nil && len(r.recoverable) > 0
}

func (r *retryAccountExclusions) ForSelection() map[int64]bool {
	if r == nil || (len(r.hard) == 0 && len(r.soft) == 0 && len(r.transient) == 0) {
		return nil
	}
	exclude := make(map[int64]bool, len(r.hard)+len(r.soft)+len(r.transient))
	for id := range r.hard {
		exclude[id] = true
	}
	for id := range r.soft {
		exclude[id] = true
	}
	for id := range r.transient {
		exclude[id] = true
	}
	return exclude
}

// nextBoundedRetryAccount gives hard-capped image/media loops one additional
// pool pass after every policy-selected transient candidate has been tried.
// It never waits and never changes the caller's total-attempt cap.
func nextBoundedRetryAccount(exclusions *retryAccountExclusions, selectAccount func(map[int64]bool) (*auth.Account, string)) (*auth.Account, string) {
	return nextBoundedRetryAccountWithContext(context.Background(), nil, exclusions, selectAccount)
}

func guardRetryAccountContext(ctx context.Context, releaseAccount func(*auth.Account), account *auth.Account, proxyURL string) (*auth.Account, string) {
	if account != nil && ctx != nil && ctx.Err() != nil {
		if releaseAccount != nil {
			releaseAccount(account)
		}
		return nil, ""
	}
	return account, proxyURL
}

func nextBoundedRetryAccountWithContext(ctx context.Context, releaseAccount func(*auth.Account), exclusions *retryAccountExclusions, selectAccount func(map[int64]bool) (*auth.Account, string)) (*auth.Account, string) {
	if selectAccount == nil {
		return nil, ""
	}
	account, proxyURL := selectAccount(exclusions.ForSelection())
	account, proxyURL = guardRetryAccountContext(ctx, releaseAccount, account, proxyURL)
	if account != nil || !exclusions.ResetTransient() {
		return account, proxyURL
	}
	account, proxyURL = selectAccount(exclusions.ForSelection())
	return guardRetryAccountContext(ctx, releaseAccount, account, proxyURL)
}

// nextContinuousRetryAccount preserves the endpoint's normal bounded account
// selection until a policy-selected failure has marked at least one account as
// recoverable. From that point it keeps polling the pool until an account is
// available or the downstream request is canceled.
func nextContinuousRetryAccount(ctx context.Context, exclusions *retryAccountExclusions, selectAccount func(map[int64]bool) (*auth.Account, string), releaseAccount func(*auth.Account)) (*auth.Account, string) {
	for {
		account, proxyURL := nextBoundedRetryAccountWithContext(ctx, releaseAccount, exclusions, selectAccount)
		if account != nil || exclusions == nil || !exclusions.CanContinueTransientCycle() {
			return account, proxyURL
		}
		if !waitForContinuousPoolRetry(ctx) {
			return nil, ""
		}
	}
}

// retryAllowedByEndpointCap preserves the endpoint's ordinary finite cap, but
// lets an explicitly selected continuous-retry failure cross it. The boolean
// is deliberately separate from the numeric retry budget: a legacy/internal
// -1 budget must not silently opt a media request into the high-risk mode.
func retryAllowedByEndpointCap(attempt, maxAttempts int, continuousSelected bool) bool {
	return continuousSelected || attempt < maxAttempts-1
}

const continuousPoolRetryPollInterval = 5 * time.Second

func waitForContinuousPoolRetry(ctx context.Context) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if ctx == nil {
		time.Sleep(continuousPoolRetryPollInterval)
		return true
	}
	timer := time.NewTimer(continuousPoolRetryPollInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil && keepalive.Active() {
			return keepalive.Keepalive() == nil
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// dispatchAccountWaitTimeout is one queue admission. Tests shorten it so a
// saturated pool can fail without sleeping the production interval.
var dispatchAccountWaitTimeout = 30 * time.Second

// waitForRetryAccountAvailable keeps one queue admission for the normal
// 30-second wait, including continuous-retry SSE/WebSocket heartbeats.
func (h *Handler) waitForRetryAccountAvailable(ctx context.Context, affinityKey string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, preserveBinding bool, policy auth.DispatchPolicy) (*auth.Account, string, error) {
	account, proxyURL, _, err := h.waitForRetryAccountAvailableWithGuard(ctx, affinityKey, apiKeyID, exclude, filter, preserveBinding, auth.SelectionOptions{Policy: policy})
	return account, proxyURL, err
}

// waitForRetryAccountAvailableWithGuard 按完整 SelectionOptions 排队等待一次队列准入，
// 持续重试心跳激活时把心跳挂进等待回调以保持连接活跃；未选中账号时一并丢弃亲和守卫。
func (h *Handler) waitForRetryAccountAvailableWithGuard(ctx context.Context, affinityKey string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, preserveBinding bool, opts auth.SelectionOptions) (*auth.Account, string, auth.SessionAffinityGuard, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var heartbeat auth.SchedulerWaitHeartbeat
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil && keepalive.Active() && continuousRetryKeepaliveInterval > 0 {
		nextHeartbeat := time.Now().Add(continuousRetryKeepaliveDelay(keepalive))
		heartbeat = func() (time.Duration, error) {
			if step := time.Until(nextHeartbeat); step > 0 {
				return step, nil
			}
			if err := keepalive.Keepalive(); err != nil {
				return 0, err
			}
			step := continuousRetryKeepaliveDelay(keepalive)
			if step <= 0 {
				step = continuousRetryKeepaliveInterval
			}
			nextHeartbeat = time.Now().Add(step)
			return step, nil
		}
	}
	account, proxyURL, guard, err := h.store.WaitForDispatchAvailableForProvider(ctx, affinityKey, dispatchAccountWaitTimeout, apiKeyID, exclude, filter, opts, preserveBinding, heartbeat)
	account, proxyURL = guardRetryAccountContext(ctx, h.store.Release, account, proxyURL)
	if account == nil {
		guard = auth.SessionAffinityGuard{}
	}
	return account, proxyURL, guard, err
}

func (h *Handler) nextRetryAccountForSession(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter) (*auth.Account, string) {
	return h.nextRetryAccount(ctx, affinityKey, apiKeyID, exclusions, filter, false, auth.DispatchPolicyStandard)
}

func (h *Handler) nextRetryAccountForSessionWithDispatch(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string) {
	return h.nextRetryAccount(ctx, affinityKey, apiKeyID, exclusions, filter, false, policy)
}

// nextRetryAccountForSessionWithDispatchGuard 是会话重试选号中同时携带
// SelectionOptions 与亲和守卫的变体：不保留绑定（preserveBinding=false），
// 排除项取自本次请求的重试排除集。
func (h *Handler) nextRetryAccountForSessionWithDispatchGuard(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter, opts auth.SelectionOptions) (*auth.Account, string, auth.SessionAffinityGuard, error) {
	return h.nextRetryAccountWithGuard(ctx, affinityKey, apiKeyID, exclusions, filter, false, opts)
}

// nextRetryAccountForSessionWithGuard 以 standard 策略做会话重试选号，并返回亲和守卫。
func (h *Handler) nextRetryAccountForSessionWithGuard(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter) (*auth.Account, string, auth.SessionAffinityGuard, error) {
	return h.nextRetryAccountWithGuard(ctx, affinityKey, apiKeyID, exclusions, filter, false, auth.SelectionOptions{Policy: auth.DispatchPolicyStandard})
}

// nextRetryAccountForContinuation 以 standard 策略做续链重试选号（preserveBinding=true）：
// 存在绑定时锁定已绑定账号，无绑定时沿用底层 Fresh 选号；需要渠道上下文时改用
// nextRetryAccountForContinuationWithSelection。
func (h *Handler) nextRetryAccountForContinuation(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter) (*auth.Account, string) {
	return h.nextRetryAccount(ctx, affinityKey, apiKeyID, exclusions, filter, true, auth.DispatchPolicyStandard)
}

// nextRetryAccountForContinuationWithSelection 是续链（preserveBinding=true）重试的
// SelectionOptions 变体：续链虽然只认已绑定的账号，但选号实现仍需要 Provider / Policy
// 才能走对渠道分支。传 policy-only 的 SelectionOptions 会让 auto 渠道下的 Antigravity
// 续链请求丢掉加权避险上下文。候选池已显式排除 Antigravity 的入口（compact 执行器 /
// native remote compaction）例外，见 selectionOptionsForCandidatePool。
func (h *Handler) nextRetryAccountForContinuationWithSelection(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter, opts auth.SelectionOptions) (*auth.Account, string, error) {
	account, proxyURL, _, err := h.nextRetryAccountWithGuard(ctx, affinityKey, apiKeyID, exclusions, filter, true, opts)
	return account, proxyURL, err
}

// nextRetryAccount 在重试排除项与选号预算内完成一次重试选号，只返回账号与代理地址；
// 需要亲和守卫与队列准入错误时改用 nextRetryAccountWithGuard。
func (h *Handler) nextRetryAccount(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter, preserveBinding bool, policy auth.DispatchPolicy) (*auth.Account, string) {
	account, proxyURL, _, _ := h.nextRetryAccountWithGuard(ctx, affinityKey, apiKeyID, exclusions, filter, preserveBinding, auth.SelectionOptions{Policy: policy})
	return account, proxyURL
}

// accountSelectionTimeout covers the initial synchronous selection as well as
// pool waits/retry cycles. The upstream first-token guard starts too late to
// protect this phase. This context is canceled on return, so it never limits a
// successfully selected account's subsequent streaming response.
const accountSelectionTimeout = 30 * time.Second

func selectionFilterWithContext(ctx context.Context, filter auth.AccountFilter) auth.AccountFilter {
	return func(acc *auth.Account) bool {
		if ctx.Err() != nil {
			return false
		}
		return (filter == nil || filter(acc)) && ctx.Err() == nil
	}
}

// nextRetryAccountWithGuard 是重试选号的底层实现：先在 accountSelectionTimeout 预算内
// 做一次同步选号（过滤器同时受该预算约束），失败后触发调度状态对账并排队等待。阻塞者
// 只剩本次请求自身的软 / 暂态排除项时跳过等待直接重置，硬排除、账号冷却与过滤器始终
// 生效；预算耗尽返回上下文错误，队列准入被拒时上抛准入错误；候选集合全部因 Antigravity
// 配额硬门禁被阻断时跳过排队等待，直接上抛类型化的配额阻断错误。
func (h *Handler) nextRetryAccountWithGuard(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter, preserveBinding bool, opts auth.SelectionOptions) (*auth.Account, string, auth.SessionAffinityGuard, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.store == nil {
		return nil, "", auth.SessionAffinityGuard{}, nil
	}
	policy := opts.Policy
	ctx, cancelSelection := context.WithTimeout(ctx, accountSelectionTimeout)
	defer cancelSelection()
	filter = selectionFilterWithContext(ctx, filter)
	for {
		if ctx.Err() != nil {
			return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
		}
		exclude := exclusions.ForSelection()
		var account *auth.Account
		var stickyProxyURL string
		var guard auth.SessionAffinityGuard
		if preserveBinding {
			// 续链同样带上 SelectionOptions：Provider 为空时与 policy-only 行为一致，
			// 携带渠道时底层选号（含容量借用 / 首次绑定分支）不会丢掉加权避险上下文。
			account, stickyProxyURL = h.store.NextForContinuationWithDispatchForProvider(opts, affinityKey, apiKeyID, exclude, filter)
		} else {
			account, stickyProxyURL, guard = h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, opts)
		}
		if account != nil {
			if ctx.Err() != nil {
				h.store.Release(account)
				return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
			}
			return account, stickyProxyURL, guard, nil
		}
		if ctx.Err() != nil {
			return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
		}
		// 配额硬门禁（Spec §6.2 #2、验收 #38）：候选集合全部因 Antigravity 配额耗尽被
		// 阻断时，以类型化原因直接上浮，交给 schedulerSelectionAPIError 映射 429 +
		// Retry-After。可信 resetAt 常在数小时之后，进入
		// waitForRetryAccountAvailableWithGuard 的排队等待只会白耗本次请求的重试预算。
		// 混合原因（部分配额、部分容量 / 冷却 / 鉴权）不满足「全部因配额阻断」，
		// 仍走既有排队与 503 路径。
		if quotaErr := h.store.AntigravityQuotaGateExhaustion(affinityKey, apiKeyID, exclude, filter, preserveBinding, opts.Model); quotaErr != nil {
			return nil, "", auth.SessionAffinityGuard{}, quotaErr
		}
		h.store.TriggerDispatchStateReconcileAsync()
		// When this request's own soft/transient exclusions are all that stand
		// between it and a candidate, no release or wakeup can satisfy a queue
		// wait that still carries them: indexed engines would burn the whole
		// selection budget before reaching the reset below (issue #719). Skip
		// straight to the reset, as the legacy engine already does; hard
		// exclusions, cooldowns and filters stay in force, and the caller's
		// retry interval/backoff still paces the next upstream attempt.
		if !h.onlyResettableExclusionsBlock(affinityKey, apiKeyID, exclusions, exclude, filter, preserveBinding, policy) {
			var admissionErr error
			account, stickyProxyURL, guard, admissionErr = h.waitForRetryAccountAvailableWithGuard(ctx, affinityKey, apiKeyID, exclude, filter, preserveBinding, opts)
			if account != nil {
				if ctx.Err() != nil {
					h.store.Release(account)
					return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
				}
				return account, stickyProxyURL, guard, nil
			}
			if ctx.Err() != nil {
				return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
			}
			if admissionErr != nil {
				return nil, "", auth.SessionAffinityGuard{}, admissionErr
			}
		}
		if !exclusions.ResetSoft() {
			if exclusions.ResetTransient() {
				log.Printf("可恢复故障已遍历账号池，清空本轮暂态排除并进入下一轮重试")
				continue
			}
			if !exclusions.CanContinueTransientCycle() {
				return nil, "", auth.SessionAffinityGuard{}, nil
			}
			if !waitForContinuousPoolRetry(ctx) {
				return nil, "", auth.SessionAffinityGuard{}, ctx.Err()
			}
			continue
		}
		log.Printf("首字超时账号池已试完，清空本次请求软排除并进入下一轮重试")
	}
}

func isFirstTokenTimeoutOutcome(outcome streamOutcome) bool {
	return outcome.failureKind == "timeout"
}
