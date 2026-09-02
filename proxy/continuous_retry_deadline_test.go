package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type continuousRetryDeadlineBlockingBody struct {
	ctx     context.Context
	started chan<- struct{}
	once    sync.Once
}

func (b *continuousRetryDeadlineBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{} })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*continuousRetryDeadlineBlockingBody) Close() error { return nil }

func TestContinuousRetryDeadlineStartsOnlyForUnlimitedBudgetAndDoesNotReset(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	deadline := &continuousRetryDeadline{duration: 80 * time.Millisecond, cancel: cancel}
	ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)

	activateContinuousRetryDeadlineForLimit(ctx, 2)
	select {
	case <-ctx.Done():
		t.Fatal("finite retry activated continuous retry deadline")
	case <-time.After(30 * time.Millisecond):
	}

	activateContinuousRetryDeadlineForLimit(ctx, -1)
	time.Sleep(50 * time.Millisecond)
	activateContinuousRetryDeadlineForLimit(ctx, -1)
	select {
	case <-ctx.Done():
	case <-time.After(60 * time.Millisecond):
		t.Fatal("repeated activation reset the continuous retry deadline")
	}
	if !errors.Is(context.Cause(ctx), errContinuousRetryDeadlineExceeded) {
		t.Fatalf("deadline cause = %v", context.Cause(ctx))
	}
}

func TestContinuousRetryDeadlineIncludesRetrySleep(t *testing.T) {
	handler, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(5000)
	ctx, cancel := context.WithCancelCause(context.Background())
	deadline := &continuousRetryDeadline{duration: 30 * time.Millisecond, cancel: cancel}
	ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
	started := time.Now()
	if handler.waitBeforeRetryWithBudget(ctx, 1, -1) {
		t.Fatal("retry sleep completed after the continuous retry deadline")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("retry sleep ignored deadline for %s", elapsed)
	}
	if !errors.Is(context.Cause(ctx), errContinuousRetryDeadlineExceeded) {
		t.Fatalf("retry sleep cause = %v", context.Cause(ctx))
	}
}

func TestContinuousRetryDeadlineCleanupRestoresRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	original := c.Request
	stop := installContinuousRetryDeadlineContext(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1})
	stop()
	if c.Request != original {
		t.Fatal("deadline cleanup did not restore request")
	}
}

func TestContinuousRetryDisabledDeadlineInstallIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
	original := c.Request
	stop := installContinuousRetryDeadlineContext(c, database.ContinuousRetryPolicy{Enabled: false, MaxDurationSeconds: 1})
	if c.Request != original {
		t.Fatal("disabled policy replaced the request or context")
	}
	stop()
	if c.Request != original {
		t.Fatal("disabled policy cleanup replaced the request")
	}
	select {
	case <-requestCtx.Done():
		t.Fatal("disabled policy canceled the request context")
	default:
	}
	if deadline := continuousRetryDeadlineForContext(requestCtx); deadline != nil {
		t.Fatal("disabled policy installed a deadline")
	}
}

func TestContinuousRetryDeadlineStopCannotCancelAfterReturning(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		ctx, cancel := context.WithCancelCause(context.Background())
		deadline := &continuousRetryDeadline{duration: time.Millisecond, cancel: cancel}
		deadline.Activate()
		deadline.Stop()
		causeAtStop := context.Cause(ctx)
		time.Sleep(2 * time.Millisecond)
		if causeAfterStop := context.Cause(ctx); causeAfterStop != causeAtStop {
			t.Fatalf("iteration %d: cause changed after Stop returned: %v -> %v", iteration, causeAtStop, causeAfterStop)
		}
	}
}

func TestContinuousRetryDeadlineSuccessClaimAndTimerHaveSingleWinner(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		ctx, cancel := context.WithCancelCause(context.Background())
		deadline := &continuousRetryDeadline{duration: time.Millisecond, cancel: cancel}
		deadline.Activate()
		claimed := deadline.ClaimSuccess()
		time.Sleep(2 * time.Millisecond)
		if claimed && context.Cause(ctx) != nil {
			t.Fatalf("iteration %d: timer fired after successful claim: %v", iteration, context.Cause(ctx))
		}
		if !claimed && !errors.Is(context.Cause(ctx), errContinuousRetryDeadlineExceeded) {
			t.Fatalf("iteration %d: failed claim without deadline cause: %v", iteration, context.Cause(ctx))
		}
	}
}

func TestContinuousRetryDeadlineActiveEndsAfterStopOrFire(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	deadline := &continuousRetryDeadline{duration: time.Hour, cancel: cancel}
	ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
	deadline.Activate()
	if !continuousRetryDeadlineActive(ctx) {
		t.Fatal("active deadline reported inactive before stop")
	}
	deadline.Stop()
	if continuousRetryDeadlineActive(ctx) {
		t.Fatal("stopped deadline still reported active")
	}

	firedCtx, firedCancel := context.WithCancelCause(context.Background())
	firedDeadline := &continuousRetryDeadline{duration: time.Millisecond, cancel: firedCancel}
	firedCtx = context.WithValue(firedCtx, continuousRetryDeadlineContextKey{}, firedDeadline)
	firedDeadline.Activate()
	select {
	case <-firedCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("deadline timer did not fire")
	}
	if continuousRetryDeadlineActive(firedCtx) {
		t.Fatal("fired deadline still reported active")
	}
}

func TestContinuousRetryHTTPTimeoutWritesStableProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol continuousRetryHTTPProtocol
		commit   bool
		wantCode int
		contains []string
	}{
		{name: "json", protocol: continuousRetryProtocolOpenAI, wantCode: http.StatusGatewayTimeout, contains: []string{"upstream_timeout"}},
		{name: "responses SSE", protocol: continuousRetryProtocolResponses, commit: true, wantCode: http.StatusOK, contains: []string{"response.failed", "upstream_timeout", `"created_at":`}},
		{name: "chat SSE", protocol: continuousRetryProtocolChat, commit: true, wantCode: http.StatusOK, contains: []string{"upstream_timeout"}},
		{name: "anthropic SSE", protocol: continuousRetryProtocolAnthropic, commit: true, wantCode: http.StatusOK, contains: []string{"event: error", "api_error"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, tc.protocol)
			if tc.commit {
				c.Writer.WriteHeader(http.StatusOK)
				_, _ = c.Writer.WriteString(continuousRetryKeepaliveComment)
			}
			continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
			stop()
			if recorder.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantCode)
			}
			for _, want := range tc.contains {
				if !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("body %q does not contain %q", recorder.Body.String(), want)
				}
			}
		})
	}
}

func TestContinuousRetryTimeoutIsNeverSelectedAgain(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	if continuousRetryRequestErrorSelected(policy, ErrUpstreamTimeout(errContinuousRetryDeadlineExceeded)) {
		t.Fatal("catch-all selected the local continuous retry deadline")
	}
	outcome := streamOutcome{logStatusCode: http.StatusGatewayTimeout, failureKind: "continuous_retry_timeout"}
	if continuousRetryStreamFailureSelected(outcome, nil, "", policy) {
		t.Fatal("catch-all selected the stream continuous retry deadline")
	}
}

func TestErrorWriterCannotBeatContinuousRetryTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, continuousRetryProtocolOpenAI)
	continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
	ErrorToGinResponse(c, errors.New("ordinary upstream failure"))
	stop()
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", recorder.Code)
	}
	if count := strings.Count(recorder.Body.String(), ErrorCodeUpstreamTimeout); count != 1 {
		t.Fatalf("upstream_timeout count = %d, body=%q", count, recorder.Body.String())
	}
}

func TestContinuousRetryDeadlineReturnsLastUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, continuousRetryProtocolOpenAI)
	lastBody := []byte(`{"error":{"message":"last upstream failure","type":"upstream_error"}}`)
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}}
	rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, lastBody)
	continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
	stop()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if recorder.Body.String() != string(lastBody) {
		t.Fatalf("body = %q, want last upstream body %q", recorder.Body.String(), lastBody)
	}
}

func TestContinuousRetryErrorWriterReturnsLastUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, continuousRetryProtocolOpenAI)
	lastBody := []byte(`{"error":{"message":"latest selected failure","type":"upstream_error"}}`)
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}}
	rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, lastBody)
	continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
	ErrorToGinResponse(c, errors.New("later local cancellation"))
	stop()
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != string(lastBody) {
		t.Fatalf("response = %d %q, want exact latest upstream failure", recorder.Code, recorder.Body.String())
	}
}

func TestContinuousRetryCommittedWritersReturnLastFailureOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol continuousRetryHTTPProtocol
		write    func(*gin.Context) bool
		contains string
	}{
		{name: "responses", protocol: continuousRetryProtocolResponses, write: func(c *gin.Context) bool { return writeCommittedResponsesRetryError(c, "later error") }, contains: "response.failed"},
		{name: "chat", protocol: continuousRetryProtocolChat, write: func(c *gin.Context) bool { return writeCommittedChatRetryError(c, "later error") }, contains: `"code":"upstream_503"`},
		{name: "anthropic", protocol: continuousRetryProtocolAnthropic, write: func(c *gin.Context) bool { return writeCommittedAnthropicRetryError(c, "api_error", "later error") }, contains: "event: error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true, MaxDurationSeconds: 1}, tc.protocol)
			c.Writer.WriteHeader(http.StatusOK)
			_, _ = c.Writer.WriteString(continuousRetryKeepaliveComment)
			resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}}
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, []byte(`{"error":{"message":"latest selected failure"}}`))
			continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
			if !tc.write(c) {
				t.Fatal("committed timeout writer returned false")
			}
			stop()
			body := recorder.Body.String()
			if !strings.Contains(body, tc.contains) || strings.Count(body, "latest selected failure") != 1 {
				t.Fatalf("body = %q, want one converted latest upstream failure", body)
			}
			if strings.Contains(body, continuousRetryTimeoutMessage) {
				t.Fatalf("body used generic timeout despite remembered failure: %q", body)
			}
			if tc.protocol == continuousRetryProtocolResponses && !strings.Contains(body, `"created_at":`) {
				t.Fatalf("Responses failure is missing created_at: %q", body)
			}
		})
	}
}

func TestContinuousRetryTimeoutCleanupCompetesWithTerminalClaim(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "terminal"}[claim], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			deadline := &continuousRetryDeadline{duration: 20 * time.Millisecond, cancel: cancel}
			ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
			var bound atomic.Bool
			if !withContinuousRetryDeadlinePendingCleanup(ctx, func() { bound.Store(true) }, func() { bound.Store(false) }) {
				t.Fatal("initial pending action was rejected")
			}
			deadline.Activate()
			if claim && !deadline.ClaimSuccess() {
				t.Fatal("terminal claim unexpectedly lost")
			}
			time.Sleep(40 * time.Millisecond)
			if got := bound.Load(); got != claim {
				t.Fatalf("bound = %v, want %v", got, claim)
			}
		})
	}
}

func TestContinuousRetryDeadlineCanFireDuringPendingSideEffect(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	deadline := &continuousRetryDeadline{duration: 20 * time.Millisecond, cancel: cancel}
	ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan bool, 1)
	go func() {
		finished <- withContinuousRetryDeadlinePendingCleanup(ctx, func() {
			close(started)
			<-release
		}, nil)
	}()
	deadline.Activate()
	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pending side effect did not start")
	}
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("deadline was blocked by a pending side effect")
	}
	close(release)
	select {
	case ok := <-finished:
		if ok {
			t.Fatal("pending side effect reported success after deadline")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pending side effect did not finish")
	}
}

func TestContinuousRetryAffinityTimeoutCleanupPreservesOnlyExistingSameAccount(t *testing.T) {
	tests := []struct {
		name          string
		previousID    int64
		wantBinding   bool
		wantAccountID int64
	}{
		{name: "fresh binding is removed"},
		{name: "existing same-account binding is preserved", previousID: 1, wantBinding: true, wantAccountID: 1},
		{name: "superseded different-account binding is not revived", previousID: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			defer store.Stop()
			current := &auth.Account{DBID: 1, Status: auth.StatusReady}
			previous := &auth.Account{DBID: 2, Status: auth.StatusReady}
			store.AddAccount(current)
			store.AddAccount(previous)
			const affinityKey = "deadline-affinity-cleanup"
			if tc.previousID == current.ID() {
				store.BindSessionAffinity(affinityKey, current, "")
			} else if tc.previousID == previous.ID() {
				store.BindSessionAffinity(affinityKey, previous, "")
			}

			ctx, cancel := context.WithCancelCause(context.Background())
			deadline := &continuousRetryDeadline{duration: 20 * time.Millisecond, cancel: cancel}
			ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
			if !bindContinuousRetrySessionAffinity(ctx, store, affinityKey, current, "") {
				t.Fatal("affinity bind was rejected before deadline")
			}
			deadline.Activate()
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
				t.Fatal("deadline did not fire")
			}

			gotID, gotBinding := store.SessionAffinityAccountID(affinityKey)
			if gotBinding != tc.wantBinding || gotID != tc.wantAccountID {
				t.Fatalf("binding = (%d, %v), want (%d, %v)", gotID, gotBinding, tc.wantAccountID, tc.wantBinding)
			}
		})
	}
}

func TestContinuousRetryBindingKeepsCapacitySpilloverRequestLocal(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	bound := &auth.Account{DBID: 1, AccessToken: "bound", Status: auth.StatusReady}
	fallback := &auth.Account{DBID: 2, AccessToken: "fallback", Status: auth.StatusReady}
	store.AddAccount(bound)
	store.AddAccount(fallback)
	const affinityKey = "continuous-capacity-spillover"
	store.BindSessionAffinity(affinityKey, bound, "")

	held := store.TakePreferredAccountWithDispatch(bound.ID(), 0, nil, nil, auth.DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held account = %p, want bound account %p", held, bound)
	}
	defer store.Release(held)

	selected, proxyURL, guard := store.NextForSessionWithDispatchGuard(affinityKey, 0, nil, nil, auth.DispatchPolicyStandard)
	if selected != fallback || !guard.PreservesExisting() {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("selection = %p guard=%v, want fallback %p with preserved binding", selected, guard.PreservesExisting(), fallback)
	}
	defer store.Release(selected)
	if !bindContinuousRetrySessionAffinityWithGuard(context.Background(), store, affinityKey, selected, proxyURL, guard) {
		t.Fatal("capacity spillover binding was rejected")
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != bound.ID() {
		t.Fatalf("binding = (%d, %v), want original account %d", boundID, ok, bound.ID())
	}
}

func TestResponsesCompactContinuousRetryDeadlineReturnsLatestFailureAndReleasesState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:            true,
			Categories:         []string{database.ContinuousRetryCategoryHTTP5xx},
			MaxDurationSeconds: 1,
		}
		return current
	})

	lastBody := []byte(`{"error":{"message":"latest upstream 503","type":"upstream_error","code":"upstream_503"}}`)
	var attempts atomic.Int32
	secondAttempt := make(chan struct{}, 1)
	upstreamCanceled := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(lastBody)
			return
		}
		select {
		case secondAttempt <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"partial":`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				upstreamCanceled <- struct{}{}
				return
			case <-releaseUpstream:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(" ")); err != nil {
					upstreamCanceled <- struct{}{}
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	defer store.Stop()
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "test-direct-key",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
		Status:       auth.StatusReady,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-4.1-direct","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", "deadline-affinity-test")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req

	started := time.Now()
	handler.ResponsesCompact(c)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("handler exceeded wall-clock bound: %s", elapsed)
	}
	select {
	case <-secondAttempt:
	default:
		t.Fatal("deadline did not cover an active retry request")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("deadline did not cancel the active upstream request")
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != string(lastBody) {
		t.Fatalf("response = %d %q, want exact latest upstream failure", recorder.Code, recorder.Body.String())
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests = %d, want 0", got)
	}
	if account.FailureStreak != 1 {
		t.Fatalf("FailureStreak = %d, want only the completed 503 attempt to be penalized", account.FailureStreak)
	}
	affinityKey := sessionAffinityKey(ResolveSessionID(req.Header, body), 0)
	if _, ok := store.SessionAffinityAccountID(affinityKey); ok {
		t.Fatal("deadline left a fresh session affinity binding")
	}
}

func TestGrokImagesContinuousRetryDeadlineCancelsActiveBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:            true,
			Categories:         []string{database.ContinuousRetryCategoryHTTP5xx},
			MaxDurationSeconds: 1,
		}
		return current
	})

	lastBody := []byte(`{"error":{"message":"latest Grok media 503","type":"upstream_error","code":"upstream_503"}}`)
	var attempts atomic.Int32
	secondAttempt := make(chan struct{}, 1)
	upstreamCanceled := make(chan struct{}, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(lastBody)
			return
		}
		select {
		case secondAttempt <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				select {
				case upstreamCanceled <- struct{}{}:
				default:
				}
				return
			case <-releaseUpstream:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(" ")); err != nil {
					select {
					case upstreamCanceled <- struct{}{}:
					default:
					}
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	defer store.Stop()
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamGrok,
		BaseURL:      upstream.URL,
		APIKey:       "test-grok-key",
		Models:       []string{grokImagineImageQualityModel},
		Status:       auth.StatusReady,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"grok-imagine","prompt":"deadline image","response_format":"url"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req

	started := time.Now()
	handler.ImagesGenerations(c)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("handler exceeded wall-clock bound: %s", elapsed)
	}
	select {
	case <-secondAttempt:
	default:
		t.Fatal("deadline did not cover an active Grok media retry")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("deadline did not cancel the active Grok media body read")
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != string(lastBody) {
		t.Fatalf("response = %d %q, want exact latest upstream failure", recorder.Code, recorder.Body.String())
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests = %d, want 0", got)
	}
}

func TestResponsesWebSocketContinuousRetryDeadlineWritesOneErrorAndCloses1013(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	settings := previousSettings
	settings.CodexWSSilentRetry = false
	settings.CodexWSHideErrors = false
	settings.CodexWSSilentRetries = 0
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:            true,
		Categories:         []string{database.ContinuousRetryCategoryHTTP5xx},
		MaxDurationSeconds: 1,
	}
	ApplyRuntimeSettings(settings)

	var attempts atomic.Int32
	activeRead := make(chan struct{}, 1)
	activeAccount := make(chan *auth.Account, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"latest websocket 503"}}`)),
			}, nil
		}
		activeAccount <- account
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &continuousRetryDeadlineBlockingBody{ctx: ctx, started: activeRead},
		}, nil
	}
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const apiKey = "sk-deadline-ws-1234567890"
	apiKeyID, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name: "deadline websocket",
		Key:  apiKey,
		Limits: database.APIKeyLimits{
			MaxConcurrency: 1,
			ScopeLimits: []database.APIKeyScopeLimit{{
				ScopeType: database.APIKeyScopeTypeAccount, ScopeID: 2, MaxConcurrency: 1,
			}},
		},
	})
	if err != nil {
		t.Fatalf("insert API key: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	t.Cleanup(store.Stop)
	accounts := []*auth.Account{
		{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"},
		{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"},
	}
	for _, account := range accounts {
		store.AddAccount(account)
	}
	const sessionID = "deadline-websocket-session"
	store.BindSessionAffinity(sessionAffinityKey(sessionID, apiKeyID), accounts[0], "")
	handler := NewHandler(store, db, &config.Config{}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	headers := http.Header{"Authorization": []string{"Bearer " + apiKey}}
	headers.Set("Session_id", sessionID)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Responses websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write Responses websocket request: %v", err)
	}
	select {
	case <-activeRead:
	case <-time.After(2 * time.Second):
		t.Fatal("continuous retry did not reach the active websocket stream read")
	}
	account := <-activeAccount
	if got := atomic.LoadInt64(&handler.apiKeyConcurrencyLimiter().counter(apiKeyID).inflight); got != 1 {
		t.Fatalf("API key inflight during websocket retry = %d, want 1", got)
	}
	if got := APIKeyScopeInflight(apiKeyID, database.APIKeyScopeTypeAccount, 2); got != 1 {
		t.Fatalf("scope inflight during websocket retry = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 1 {
		t.Fatalf("ActiveRequests during websocket retry = %d, want 1", got)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	errorFrames := 0
	for {
		_, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			var closeErr *websocket.CloseError
			if !errors.As(readErr, &closeErr) {
				t.Fatalf("read Responses websocket terminal: %v", readErr)
			}
			if closeErr.Code != websocket.CloseTryAgainLater {
				t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseTryAgainLater)
			}
			break
		}
		if gjson.GetBytes(payload, "type").String() == "error" {
			errorFrames++
		}
	}
	if errorFrames != 1 {
		t.Fatalf("error frames = %d, want exactly 1", errorFrames)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}

	releaseDeadline := time.Now().Add(500 * time.Millisecond)
	for (atomic.LoadInt64(&accounts[0].ActiveRequests) != 0 || atomic.LoadInt64(&accounts[1].ActiveRequests) != 0) && time.Now().Before(releaseDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, releasedAccount := range accounts {
		if got := atomic.LoadInt64(&releasedAccount.ActiveRequests); got != 0 {
			t.Fatalf("account %d ActiveRequests after websocket deadline = %d, want 0", releasedAccount.ID(), got)
		}
	}
	if got := atomic.LoadInt64(&handler.apiKeyConcurrencyLimiter().counter(apiKeyID).inflight); got != 0 {
		t.Fatalf("API key inflight after websocket deadline = %d, want 0", got)
	}
	if got := APIKeyScopeInflight(apiKeyID, database.APIKeyScopeTypeAccount, 2); got != 0 {
		t.Fatalf("scope inflight after websocket deadline = %d, want 0", got)
	}
}
