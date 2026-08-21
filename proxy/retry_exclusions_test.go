package proxy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestRetryAccountExclusionsSoftResetPreservesHard(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkSoftFirstTokenTimeout(1)
	exclusions.MarkHard(2)

	selection := exclusions.ForSelection()
	if !selection[1] || !selection[2] {
		t.Fatalf("selection excludes = %#v, want soft and hard accounts", selection)
	}

	if !exclusions.ResetSoft() {
		t.Fatal("ResetSoft() = false, want true")
	}
	selection = exclusions.ForSelection()
	if selection[1] {
		t.Fatalf("soft account still excluded after reset: %#v", selection)
	}
	if !selection[2] {
		t.Fatalf("hard account was cleared by soft reset: %#v", selection)
	}
}

func TestRetryAccountExclusionsHardOverridesSoft(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkSoftFirstTokenTimeout(1)
	exclusions.MarkHard(1)

	if exclusions.ResetSoft() {
		t.Fatal("ResetSoft() cleared a hard-only account")
	}
	selection := exclusions.ForSelection()
	if !selection[1] {
		t.Fatalf("hard account missing from selection excludes: %#v", selection)
	}
}

func TestRetryAccountExclusionsHardOverridesPriorTransient(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(1)
	exclusions.MarkHard(1)

	if exclusions.ResetTransient() {
		t.Fatal("hard override left a transient round entry")
	}
	if exclusions.CanContinueTransientCycle() {
		t.Fatal("hard override left the account recoverable")
	}
	if selection := exclusions.ForSelection(); !selection[1] {
		t.Fatalf("hard account missing after override: %#v", selection)
	}
}

func TestRetryAccountExclusionsContinuousCycleOnlyClearsTransient(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(1)
	exclusions.MarkHard(2)
	exclusions.MarkSoft(3)

	if !exclusions.CanContinueTransientCycle() {
		t.Fatal("continuous retry did not remember a transient failure")
	}
	if !exclusions.ResetTransient() {
		t.Fatal("ResetTransient() = false, want true")
	}
	selection := exclusions.ForSelection()
	if selection[1] {
		t.Fatalf("transient account still excluded after reset: %#v", selection)
	}
	if !selection[2] || !selection[3] {
		t.Fatalf("transient reset cleared hard or soft exclusions: %#v", selection)
	}

	finite := newRetryAccountExclusions()
	finite.MarkTransportFailure(4, 2)
	if finite.CanContinueTransientCycle() {
		t.Fatal("finite retry budget enabled continuous pool cycling")
	}
}

func TestNextBoundedRetryAccountResetsOnlyTransientRound(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(1)

	var selections []map[int64]bool
	account, _ := nextBoundedRetryAccount(exclusions, func(exclude map[int64]bool) (*auth.Account, string) {
		copyExclude := make(map[int64]bool, len(exclude))
		for id, blocked := range exclude {
			copyExclude[id] = blocked
		}
		selections = append(selections, copyExclude)
		if exclude[1] {
			return nil, ""
		}
		return &auth.Account{DBID: 1}, ""
	})
	if account == nil || account.ID() != 1 {
		t.Fatalf("bounded retry account = %#v, want account 1 after transient reset", account)
	}
	if len(selections) != 2 || !selections[0][1] || selections[1][1] {
		t.Fatalf("selection rounds = %#v, want transient exclusion then reset", selections)
	}

	hard := newRetryAccountExclusions()
	hard.MarkHard(2)
	calls := 0
	account, _ = nextBoundedRetryAccount(hard, func(exclude map[int64]bool) (*auth.Account, string) {
		calls++
		return nil, ""
	})
	if account != nil || calls != 1 {
		t.Fatalf("hard exclusion triggered an extra bounded pool pass: account=%#v calls=%d", account, calls)
	}
}

func TestRetryAccountExclusionsRequestErrorUsesSelectedPolicy(t *testing.T) {
	statusPolicy := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden},
	}
	exclusions := newRetryAccountExclusions()
	handshakeErr := continuousRetryTestHTTPError{
		status: http.StatusForbidden,
		body:   []byte(`{"error":{"code":"account_forbidden"}}`),
	}
	exclusions.MarkRequestFailure(1, handshakeErr, 0, statusPolicy)
	if !exclusions.CanContinueTransientCycle() {
		t.Fatal("selected handshake status was marked permanently excluded")
	}
	if !exclusions.ResetTransient() {
		t.Fatal("selected handshake status did not leave a transient exclusion")
	}

	transportOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	unselected := newRetryAccountExclusions()
	unselected.MarkRequestFailure(1, handshakeErr, 2, transportOnly)
	if unselected.CanContinueTransientCycle() {
		t.Fatal("transport category made an unselected handshake status recoverable")
	}
	if !unselected.ForSelection()[1] {
		t.Fatal("unselected handshake status was not hard-excluded for the request")
	}

	codePolicy := database.ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"temporarily_unavailable"}}
	codeExclusions := newRetryAccountExclusions()
	codeExclusions.MarkRequestFailure(2, errors.New("upstream temporarily_unavailable"), 0, codePolicy)
	if !codeExclusions.CanContinueTransientCycle() {
		t.Fatal("selected transport error code was marked permanently excluded")
	}

	// Exact upstream error-code selectors must inspect a real error string.
	// MarkTransportFailure has no such payload, so a code literally named
	// "transport" must not masquerade as the transport category.
	codeNamedTransport := database.ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"transport"}}
	missingError := newRetryAccountExclusions()
	missingError.MarkTransportFailure(3, 0, codeNamedTransport)
	if missingError.CanContinueTransientCycle() || !missingError.ForSelection()[3] {
		t.Fatal("synthetic transport marker matched an exact error-code selector")
	}

	selectedTransport := newRetryAccountExclusions()
	selectedTransport.MarkTransportFailure(4, 0, transportOnly)
	if !selectedTransport.CanContinueTransientCycle() {
		t.Fatal("transport category did not keep a transport failure recoverable")
	}
}

func TestRetryAccountExclusionsCatchAllKeepsUnknownFailuresInRotation(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	exclusions := newRetryAccountExclusions()
	handshakeErr := continuousRetryTestHTTPError{
		status: http.StatusTeapot,
		body:   []byte(`{"error":{"code":"future_account_error"}}`),
	}

	exclusions.MarkRequestFailure(1, handshakeErr, 0, policy)
	if !exclusions.CanContinueTransientCycle() || !exclusions.ResetTransient() {
		t.Fatal("catch-all handshake failure was not kept in the recoverable account cycle")
	}

	exclusions.MarkHTTPFailure(2, 520, []byte(`{"error":{"code":"unknown_gateway_error"}}`), 0, 0, policy)
	if !exclusions.CanContinueTransientCycle() || !exclusions.ResetTransient() {
		t.Fatal("catch-all HTTP failure was not kept in the recoverable account cycle")
	}

	outcome := streamOutcome{
		logStatusCode:  http.StatusBadRequest,
		failureKind:    "future_stream_error",
		failurePayload: []byte(`{"type":"error","error":{"code":"future_stream_error"}}`),
	}
	exclusions.MarkStreamFailureForEvent(3, outcome, "error", 0, 0, policy)
	if !exclusions.CanContinueTransientCycle() || !exclusions.ResetTransient() {
		t.Fatal("catch-all stream failure was not kept in the recoverable account cycle")
	}
}

func TestRetryAccountExclusionsCatchAllMarksExplicitCyberPolicyHard(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	body := []byte(`{"error":{"code":"cyber_policy"}}`)
	streamPayload := []byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"cyber_policy"}}}`)

	tests := []struct {
		name string
		mark func(*retryAccountExclusions)
	}{
		{
			name: "status-bearing request error",
			mark: func(exclusions *retryAccountExclusions) {
				exclusions.MarkRequestFailure(1, continuousRetryTestHTTPError{status: http.StatusForbidden, body: body}, -1, policy)
			},
		},
		{
			name: "statusless request error",
			mark: func(exclusions *retryAccountExclusions) {
				exclusions.MarkRequestFailure(1, &Error{Code: "cyber_policy", Message: "blocked", Type: ErrorTypeUpstreamError}, -1, policy)
			},
		},
		{
			name: "HTTP failure",
			mark: func(exclusions *retryAccountExclusions) {
				exclusions.MarkHTTPFailure(1, http.StatusInternalServerError, body, -1, -1, policy)
			},
		},
		{
			name: "stream failure",
			mark: func(exclusions *retryAccountExclusions) {
				outcome := classifyResponseFailedOutcome(streamPayload)
				outcome.penalize = true
				exclusions.MarkStreamFailureForEvent(1, outcome, "response.failed", -1, -1, policy)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exclusions := newRetryAccountExclusions()
			tt.mark(exclusions)
			if exclusions.CanContinueTransientCycle() || exclusions.ResetTransient() {
				t.Fatal("explicit CYB remained eligible for another account-pool cycle")
			}
			if !exclusions.ForSelection()[1] {
				t.Fatal("explicit CYB account was not hard-excluded")
			}
		})
	}
}

func TestRetryAccountExclusionsHTTPFailureClassification(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		transient  bool
	}{
		{name: "429 rate limit", statusCode: http.StatusTooManyRequests, body: []byte(`{"error":{"code":"rate_limit_exceeded"}}`), transient: true},
		{name: "502", statusCode: http.StatusBadGateway, transient: true},
		{name: "503", statusCode: http.StatusServiceUnavailable, transient: true},
		{name: "504", statusCode: http.StatusGatewayTimeout, transient: true},
		{name: "usage limit wrapped as 500", statusCode: http.StatusInternalServerError, body: []byte(`{"error":{"type":"usage_limit_reached"}}`)},
		{name: "insufficient quota", statusCode: http.StatusTooManyRequests, body: []byte(`{"error":{"code":"insufficient_quota"}}`)},
		{name: "401", statusCode: http.StatusUnauthorized},
		{name: "403", statusCode: http.StatusForbidden},
		{name: "404", statusCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientRetryHTTPFailure(tt.statusCode, tt.body); got != tt.transient {
				t.Fatalf("isTransientRetryHTTPFailure(%d) = %v, want %v", tt.statusCode, got, tt.transient)
			}
		})
	}
}

func TestPermanentQuotaMarkersNeverEnterContinuousCycles(t *testing.T) {
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
		t.Run(marker, func(t *testing.T) {
			for _, field := range []string{"code", "message"} {
				t.Run(field, func(t *testing.T) {
					body := []byte(`{"error":{"` + field + `":"` + marker + `"}}`)
					if !isPermanentQuotaFailure(body) {
						t.Fatalf("quota marker %q in %s was not classified as permanent", marker, field)
					}

					httpExclusions := newRetryAccountExclusions()
					httpExclusions.MarkHTTPFailure(1, http.StatusTooManyRequests, body, -1, -1)
					if httpExclusions.CanContinueTransientCycle() || httpExclusions.ResetTransient() {
						t.Fatalf("HTTP quota marker %q in %s entered a continuous retry cycle", marker, field)
					}

					payload := []byte(`{"type":"response.failed","response":{"error":{"` + field + `":"` + marker + `"}}}`)
					outcome := classifyResponseFailedOutcome(payload)
					if outcome.logStatusCode != http.StatusTooManyRequests || outcome.failureKind != "usage_limit" || !streamOutcomeUsesRateLimitBudget(outcome) {
						t.Fatalf("stream quota outcome = %#v, want permanent usage-limit semantics", outcome)
					}
					streamExclusions := newRetryAccountExclusions()
					streamExclusions.MarkStreamFailure(1, outcome, -1, -1)
					if streamExclusions.CanContinueTransientCycle() || streamExclusions.ResetTransient() {
						t.Fatalf("stream quota marker %q in %s entered a continuous retry cycle", marker, field)
					}
				})
			}
		})
	}
}

func TestApplyResponseFailedDecisionKindPreservesMessageOnlyPermanentQuota(t *testing.T) {
	decision := codex429Decision{Scope: rateLimitScopeModel, Reason: "rate_limited_model"}
	for _, marker := range []string{
		"insufficient_quota",
		"billing_hard_limit",
		"spend_limit",
		"credit_balance",
		"usage_limited",
	} {
		t.Run(marker, func(t *testing.T) {
			payload := []byte(`{"type":"response.failed","response":{"error":{"message":"` + marker + `"}}}`)
			outcome := classifyResponseFailedOutcome(payload)
			outcome = applyResponseFailedDecisionKind(outcome, payload, decision)
			if outcome.logStatusCode != http.StatusTooManyRequests || outcome.failureKind != "usage_limit" {
				t.Fatalf("response.failed outcome for %q = %#v, want permanent usage-limit semantics", marker, outcome)
			}

			exclusions := newRetryAccountExclusions()
			exclusions.MarkStreamFailure(1, outcome, -1, -1)
			if exclusions.CanContinueTransientCycle() || exclusions.ResetTransient() {
				t.Fatalf("response.failed quota marker %q entered a continuous retry cycle", marker)
			}
		})
	}
}

func TestRetryAccountExclusionsUsesIndependentBudgets(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkHTTPFailure(1, http.StatusServiceUnavailable, nil, -1, 1)
	exclusions.MarkHTTPFailure(2, http.StatusTooManyRequests, []byte(`{"error":{"code":"rate_limit_exceeded"}}`), -1, 1)

	if !exclusions.ResetTransient() {
		t.Fatal("unlimited general failure was not marked transient")
	}
	selection := exclusions.ForSelection()
	if selection[1] {
		t.Fatalf("general transient account remained excluded: %#v", selection)
	}
	if !selection[2] {
		t.Fatalf("finite 429 budget was pulled into the general unlimited cycle: %#v", selection)
	}
}

func TestRetryAccountExclusionsStreamFailureClassification(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkStreamFailure(1, streamOutcome{failureKind: "transport", logStatusCode: logStatusUpstreamStreamBreak}, -1, 0)
	exclusions.MarkStreamFailure(2, streamOutcome{failureKind: "usage_limit", logStatusCode: http.StatusTooManyRequests}, -1, -1)

	if !exclusions.ResetTransient() {
		t.Fatal("transport stream failure was not marked transient")
	}
	selection := exclusions.ForSelection()
	if selection[1] {
		t.Fatalf("transport stream failure remained excluded: %#v", selection)
	}
	if !selection[2] {
		t.Fatalf("usage-limit stream failure was cleared: %#v", selection)
	}
}

func TestWaitForContinuousPoolRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if waitForContinuousPoolRetry(ctx) {
		t.Fatal("canceled pool wait returned true")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled pool wait returned too slowly: %v", elapsed)
	}
}

func TestNextContinuousRetryAccountReleasesSelectionAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	account := &auth.Account{DBID: 41, AccessToken: "token", Status: auth.StatusReady}
	released := 0

	got, _ := nextContinuousRetryAccount(ctx, newRetryAccountExclusions(), func(map[int64]bool) (*auth.Account, string) {
		cancel()
		return account, ""
	}, func(got *auth.Account) {
		if got == account {
			released++
		}
	})

	if got != nil {
		t.Fatalf("nextContinuousRetryAccount returned account %d after cancellation", got.ID())
	}
	if released != 1 {
		t.Fatalf("canceled selection release count = %d, want 1", released)
	}
}

func TestNextBoundedRetryAccountReleasesSelectedAccountForCanceledContext(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)
	ctx, cancel := context.WithCancel(context.Background())

	got, _ := nextBoundedRetryAccountWithContext(ctx, store.Release, newRetryAccountExclusions(), func(exclude map[int64]bool) (*auth.Account, string) {
		selected := store.NextExcludingWithDispatch(0, exclude, nil, auth.DispatchPolicyStandard)
		cancel()
		if selected == nil {
			return nil, ""
		}
		return selected, selected.GetProxyURL()
	})
	if got != nil {
		t.Fatalf("nextBoundedRetryAccount returned account %d for canceled context", got.ID())
	}
	if active := atomic.LoadInt64(&account.ActiveRequests); active != 0 {
		t.Fatalf("ActiveRequests after canceled selection = %d, want 0", active)
	}
}

func TestNextRetryAccountStartsNewTransientCycle(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store}

	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(account.ID())
	got, _ := h.nextRetryAccountForSession(context.Background(), "", 0, exclusions, nil)
	if got != account {
		t.Fatalf("next retry account = %p, want %p after transient cycle reset", got, account)
	}
	store.Release(got)
}

func TestNextRetryAccountDoesNotCyclePermanentFailures(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store}

	exclusions := newRetryAccountExclusions()
	exclusions.MarkHard(account.ID())
	start := time.Now()
	got, _ := h.nextRetryAccountForSession(context.Background(), "", 0, exclusions, nil)
	if got != nil {
		store.Release(got)
		t.Fatalf("permanently excluded account was selected: %d", got.ID())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("permanent-only pool did not fail promptly: %v", elapsed)
	}
}

func TestNextRetryAccountContinuousWaitHonorsCancellation(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	h := &Handler{store: store}
	exclusions := newRetryAccountExclusions()
	exclusions.MarkTransient(99)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, _ := h.nextRetryAccountForSession(ctx, "", 0, exclusions, nil)
	if got != nil {
		t.Fatalf("empty pool returned account %d", got.ID())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("continuous pool wait ignored cancellation: %v", elapsed)
	}
}

func TestIsFirstTokenTimeoutOutcome(t *testing.T) {
	if !isFirstTokenTimeoutOutcome(firstTokenTimeoutOutcome(10)) {
		t.Fatal("first-token timeout outcome should be classified as timeout")
	}
	if isFirstTokenTimeoutOutcome(streamOutcome{failureKind: "transport"}) {
		t.Fatal("transport outcome should not be classified as first-token timeout")
	}
}

func TestWebsocketHTTPFallbackStateRetainsLeaseOnce(t *testing.T) {
	account := &auth.Account{DBID: 7}
	var state websocketHTTPFallbackState
	state.Retain(account, "http://proxy.example", 1500*time.Millisecond, "local_read_limit")

	if !state.ForceHTTP() {
		t.Fatal("ForceHTTP() = false after retaining a WebSocket 1009 fallback")
	}
	if state.ID() == "" {
		t.Fatal("fallback correlation ID is empty")
	}
	if state.Source() != "local_read_limit" {
		t.Fatalf("source = %q, want local_read_limit", state.Source())
	}
	gotAccount, gotProxy, ok := state.Take()
	if !ok || gotAccount != account || gotProxy != "http://proxy.example" {
		t.Fatalf("Take() = (%p, %q, %v), want retained account/proxy", gotAccount, gotProxy, ok)
	}
	if _, _, ok := state.Take(); ok {
		t.Fatal("second Take() reused the retained account lease")
	}
	if !state.ForceHTTP() {
		t.Fatal("ForceHTTP() reset after consuming the retained lease")
	}
}

func TestWebsocketHTTPFallbackStateLogsAttemptsWithoutInventingFirstEvent(t *testing.T) {
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	account := &auth.Account{DBID: 7}
	var state websocketHTTPFallbackState
	state.Retain(account, "", 1500*time.Millisecond, "peer_close")
	fallbackID := state.ID()
	state.LogHTTPAttemptCompletion("/v1/responses", account.ID(), 2, 500, 0, logStatusUpstreamStreamBreak)

	firstAttempt := logs.String()
	for _, want := range []string{
		"fallback_id=" + fallbackID,
		"attempt=2",
		"http_first_event_ms=0",
		"total_first_event_ms=0",
	} {
		if !strings.Contains(firstAttempt, want) {
			t.Fatalf("first attempt log missing %q: %s", want, firstAttempt)
		}
	}

	logs.Reset()
	state.LogHTTPAttemptCompletion("/v1/responses", account.ID(), 3, 500, 100, 200)
	secondAttempt := logs.String()
	if !strings.Contains(secondAttempt, "fallback_id="+fallbackID) || !strings.Contains(secondAttempt, "attempt=3") {
		t.Fatalf("subsequent attempt lost fallback correlation: %s", secondAttempt)
	}
	if strings.Contains(secondAttempt, "total_first_event_ms=0") {
		t.Fatalf("observed first event was not included in cumulative timing: %s", secondAttempt)
	}
}
