package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestContinuousRetryReplayFailureOverridesTerminalOutcome(t *testing.T) {
	replayErrs := []error{
		errContinuousRetryReplayClosed,
		errContinuousRetryReplayLimitExceeded,
		errContinuousRetryReplayStorage,
		errContinuousRetryWSReplayInvalid,
		errContinuousRetryWSMessageTooLarge,
	}
	for _, replayErr := range replayErrs {
		t.Run(replayErr.Error(), func(t *testing.T) {
			outcome := classifyStreamOutcome(nil, replayErr, nil, true)
			if !outcome.terminalLocal || outcome.logStatusCode != http.StatusInternalServerError || outcome.failureKind != "local" || outcome.penalize {
				t.Fatalf("local replay outcome = %+v", outcome)
			}
			if outcome.failureMessage != continuousRetryLocalFailureMessage {
				t.Fatalf("local replay message = %q", outcome.failureMessage)
			}
			if selected := continuousRetryStreamFailureSelected(outcome, nil, "response.completed"); selected {
				t.Fatal("local replay failure entered the retry selector")
			}
			overlaid := overlayContinuousRetryLocalFailure(classifyResponseFailedOutcome([]byte(`{"type":"response.failed","response":{"status_code":503}}`)), replayErr)
			if !overlaid.terminalLocal || overlaid.logStatusCode != http.StatusInternalServerError {
				t.Fatalf("overlay = %+v", overlaid)
			}
		})
	}
}

func TestContinuousRetryReplayFactoryUsesTinyLimits(t *testing.T) {
	h := &Handler{continuousRetryReplayFactory: func() *continuousRetryReplay {
		return newContinuousRetryReplayWithLimits(0, 8)
	}}
	attempt := h.newContinuousRetryStreamAttempt(true, &bytes.Buffer{}, nil)
	if attempt == nil || attempt.replay == nil {
		t.Fatal("factory did not create a replay attempt")
	}
	if _, err := attempt.replay.Write([]byte("012345678")); !errors.Is(err, errContinuousRetryReplayLimitExceeded) {
		t.Fatalf("tiny replay write = %v, want limit error", err)
	}
	_ = attempt.Close()
	if replay := h.newContinuousRetryWSReplay(); replay == nil || replay.replay == nil || replay.replay.totalLimit != 8 {
		t.Fatal("factory did not apply tiny WS replay limit")
	}
}

func TestContinuousRetryLocalProtocolErrorsDoNotRequireCommittedWriter(t *testing.T) {
	tests := []struct {
		name  string
		write func(*gin.Context) bool
		want  string
	}{
		{name: "responses", write: writeContinuousRetryLocalResponsesError, want: `"type":"response.failed"`},
		{name: "chat", write: writeContinuousRetryLocalChatError, want: `"type":"server_error"`},
		{name: "messages", write: writeContinuousRetryLocalAnthropicError, want: `"type":"api_error"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			if !tc.write(ctx) {
				t.Fatal("local protocol helper returned false")
			}
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, tc.want) || !strings.Contains(body, continuousRetryLocalFailureMessage) {
				t.Fatalf("body = %q", body)
			}
		})
	}
}

func TestCommitResponsesStreamAttemptClearsStagedTurnStateOnReplayFailure(t *testing.T) {
	replay := newContinuousRetryReplayWithLimits(0, 32)
	if _, err := replay.Write([]byte("payload")); err != nil {
		t.Fatalf("replay write: %v", err)
	}
	if replay.file == nil {
		t.Fatal("expected file-backed replay")
	}
	if err := replay.file.Close(); err != nil {
		t.Fatalf("close replay file: %v", err)
	}
	attempt := &continuousRetryStreamAttempt{replay: replay}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Header(codexTurnStateHeader, "staged-token")
	err := (&Handler{}).commitResponsesStreamAttempt(ctx, attempt, "affinity", nil, http.Header{codexTurnStateHeader: []string{"staged-token"}})
	if !errors.Is(err, errContinuousRetryReplayStorage) {
		t.Fatalf("commit error = %v, want replay storage error", err)
	}
	if got := ctx.Writer.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("staged turn-state header survived local commit failure: %q", got)
	}
}

type continuousRetryLocalEndpointCase struct {
	name          string
	path          string
	body          string
	wantMarkers   []string
	forbidMarkers []string
	invoke        func(*Handler, *gin.Context)
	newStore      func(*testing.T, string) (*auth.Store, *auth.Account)
}

func newContinuousRetryLocalSSEServer(t *testing.T, headers http.Header) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for key, values := range headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_local\"}}\n\n"+
				"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"private-local-partial\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_local\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"private-local-partial\"}]},{\"type\":\"compaction\",\"encrypted_content\":\"local-commit-compaction-state\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
		)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func newContinuousRetryLocalNativeStore(t *testing.T, upstreamURL string) (*auth.Store, *auth.Account) {
	t.Helper()
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-access", PlanType: "pro", AccountID: "test-account"}
	store.AddAccount(account)
	return store, account
}

func newContinuousRetryLocalRelayStore(t *testing.T, upstreamURL string) (*auth.Store, *auth.Account) {
	t.Helper()
	store := newOpenAIResponsesRelayStore(upstreamURL)
	t.Cleanup(store.Stop)
	return store, store.FindByID(1)
}

func newContinuousRetryLocalGrokNativeStore(t *testing.T, upstreamURL string, protocol GrokProtocol) (*auth.Store, *auth.Account) {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "grok-4.5", MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	baseURL := strings.TrimRight(upstreamURL, "/") + "/v1"
	account := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "test-grok-key", BaseURL: baseURL,
		Models: []string{"grok-4.5"}, PlanType: "api",
	}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		Models: []auth.GrokModelRoute{{
			ModelID: "grok-4.5", BaseURL: baseURL, APIBackend: protocol,
		}},
		Capabilities: []auth.GrokProtocolCapability{{
			ModelID: "grok-4.5", Origin: baseURL, Protocol: protocol,
			Status: auth.GrokCapabilityOK, ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}},
	})
	store.AddAccount(account)
	return store, account
}

func seedContinuousRetryLocalHealth(account *auth.Account) {
	account.Mu().Lock()
	account.SuccessStreak = 4
	account.FailureStreak = 2
	account.LastSuccessAt = time.Time{}
	account.LastFailureAt = time.Time{}
	account.Mu().Unlock()
}

func assertContinuousRetryLocalHealthUnchanged(t *testing.T, account *auth.Account) {
	t.Helper()
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	if account.SuccessStreak != 4 || account.FailureStreak != 2 || !account.LastSuccessAt.IsZero() || !account.LastFailureAt.IsZero() {
		t.Fatalf("local replay failure changed account health: success=%d failure=%d last_success=%v last_failure=%v",
			account.SuccessStreak, account.FailureStreak, account.LastSuccessAt, account.LastFailureAt)
	}
}

func continuousRetryLocalRequest(t *testing.T, tc continuousRetryLocalEndpointCase, handler *Handler) (*httptest.ResponseRecorder, string) {
	t.Helper()
	body := []byte(tc.body)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Codex2API-Affinity-Key", "local-replay-test")
	identity := resolveRequestSessionIdentity(ctx.Request.Header, body)
	affinityKey := sessionAffinityKey(identity.affinityID, 0)
	tc.invoke(handler, ctx)
	return recorder, affinityKey
}

func TestContinuousRetryReplayLimitFailureTerminatesEveryHTTPSSEProtocolLocally(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)

	tests := []continuousRetryLocalEndpointCase{
		{
			name: "relay responses", path: "/v1/responses",
			body:          `{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
			wantMarkers:   []string{`"type":"response.failed"`, `"type":"server_error"`, `"code":"internal_error"`},
			forbidMarkers: []string{"private-local-partial", `"type":"response.completed"`},
			invoke:        func(h *Handler, c *gin.Context) { h.Responses(c) },
			newStore:      newContinuousRetryLocalRelayStore,
		},
		{
			name: "native responses", path: "/v1/responses",
			body:          `{"model":"gpt-5.4","input":"hello","stream":true}`,
			wantMarkers:   []string{`"type":"response.failed"`, `"type":"server_error"`, `"code":"internal_error"`},
			forbidMarkers: []string{"private-local-partial", `"type":"response.completed"`},
			invoke:        func(h *Handler, c *gin.Context) { h.Responses(c) },
			newStore:      newContinuousRetryLocalNativeStore,
		},
		{
			name: "chat completions", path: "/v1/chat/completions",
			body:          `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			wantMarkers:   []string{`"type":"server_error"`, `"code":"internal_error"`},
			forbidMarkers: []string{"private-local-partial", "data: [DONE]", `"finish_reason":"stop"`},
			invoke:        func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
			newStore:      newContinuousRetryLocalNativeStore,
		},
		{
			name: "anthropic messages", path: "/v1/messages",
			body:          `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			wantMarkers:   []string{"event: error", `"type":"api_error"`},
			forbidMarkers: []string{"private-local-partial", "message_stop"},
			invoke:        func(h *Handler, c *gin.Context) { h.Messages(c) },
			newStore:      newContinuousRetryLocalNativeStore,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := newContinuousRetryLocalSSEServer(t, nil)
			store, account := tc.newStore(t, upstream.URL)
			seedContinuousRetryLocalHealth(account)
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
				return newContinuousRetryReplayWithLimits(0, 1)
			}

			recorder, affinityKey := continuousRetryLocalRequest(t, tc, handler)
			body := recorder.Body.String()
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1; body=%q", calls.Load(), body)
			}
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(body, continuousRetryLocalFailureMessage) {
				t.Fatalf("local terminal = status %d body %q", recorder.Code, body)
			}
			for _, marker := range tc.wantMarkers {
				if !strings.Contains(body, marker) {
					t.Fatalf("local terminal missing %q: %q", marker, body)
				}
			}
			for _, marker := range tc.forbidMarkers {
				if strings.Contains(body, marker) {
					t.Fatalf("private output or success terminal %q leaked: %q", marker, body)
				}
			}
			if boundID, ok := store.SessionAffinityAccountID(affinityKey); ok {
				t.Fatalf("local failure created affinity binding to account %d", boundID)
			}
			assertContinuousRetryLocalHealthUnchanged(t, account)
		})
	}
}

func TestContinuousRetryReplayCommitFailurePreservesExistingState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	resetResponseCacheStateForTest(defaultResponseCacheConfig())

	const encryptedState = "local-commit-compaction-state"
	upstream, calls := newContinuousRetryLocalSSEServer(t, http.Header{
		codexTurnStateHeader: []string{"local-commit-turn-state"},
	})
	store, account := newContinuousRetryLocalRelayStore(t, upstream.URL)
	seedContinuousRetryLocalHealth(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	runtimeCache := cache.NewMemory(1)
	handler.SetRuntimeCache(runtimeCache)
	handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
		replay := newContinuousRetryReplayWithLimits(0, 1<<20)
		replay.beforeReadForTest = func(replay *continuousRetryReplay) {
			if replay.file != nil {
				_ = replay.file.Close()
			}
		}
		return replay
	}

	tc := continuousRetryLocalEndpointCase{
		path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
		invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
	}
	body := []byte(tc.body)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Codex2API-Affinity-Key", "local-commit-existing")
	identity := resolveRequestSessionIdentity(ctx.Request.Header, body)
	affinityKey := sessionAffinityKey(identity.affinityID, 0)
	store.BindSessionAffinity(affinityKey, account, "")
	codexTurnStateOrigins.Delete(affinityKey)
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	tc.invoke(handler, ctx)
	responseBody := recorder.Body.String()
	if calls.Load() != 1 || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("commit failure = calls %d status %d body %q", calls.Load(), recorder.Code, responseBody)
	}
	if !strings.Contains(responseBody, `"type":"response.failed"`) || !strings.Contains(responseBody, `"code":"internal_error"`) || strings.Contains(responseBody, `"type":"response.completed"`) || strings.Contains(responseBody, "private-local-partial") {
		t.Fatalf("commit failure protocol terminal is invalid: %q", responseBody)
	}
	if got := recorder.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("commit failure leaked staged turn-state header %q", got)
	}
	if _, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		t.Fatal("commit failure recorded turn-state provenance")
	}
	if got := getResponseCache(responseCacheOwner(0), "resp_local"); len(got) != 0 {
		t.Fatalf("commit failure cached completed response: %s", got)
	}
	if _, ok, err := runtimeCache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, compactionContentDigest(encryptedState)); err != nil || ok {
		t.Fatalf("commit failure compaction provenance = ok %v err %v", ok, err)
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != account.ID() {
		t.Fatalf("commit failure changed existing affinity: account=%d ok=%v", boundID, ok)
	}
	assertContinuousRetryLocalHealthUnchanged(t, account)
}

func TestContinuousRetryReplayWriteFailurePreservesExistingAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)

	upstream, calls := newContinuousRetryLocalSSEServer(t, nil)
	store, account := newContinuousRetryLocalRelayStore(t, upstream.URL)
	seedContinuousRetryLocalHealth(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
		return newContinuousRetryReplayWithLimits(0, 1)
	}
	tc := continuousRetryLocalEndpointCase{
		path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
		invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
	}
	body := []byte(tc.body)
	req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex2API-Affinity-Key", "local-write-existing")
	affinityKey := sessionAffinityKey(resolveRequestSessionIdentity(req.Header, body).affinityID, 0)
	store.BindSessionAffinity(affinityKey, account, "")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req
	tc.invoke(handler, ctx)

	if calls.Load() != 1 || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("write failure = calls %d status %d body %q", calls.Load(), recorder.Code, recorder.Body.String())
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != account.ID() {
		t.Fatalf("write failure changed existing affinity: account=%d ok=%v", boundID, ok)
	}
	assertContinuousRetryLocalHealthUnchanged(t, account)
}

func TestContinuousRetrySuccessfulReplayBindsCommittedWinner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)

	upstream, calls := newContinuousRetryLocalSSEServer(t, nil)
	store, account := newContinuousRetryLocalRelayStore(t, upstream.URL)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	tc := continuousRetryLocalEndpointCase{
		path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
		invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
	}
	recorder, affinityKey := continuousRetryLocalRequest(t, tc, handler)
	if calls.Load() != 1 || recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("successful replay = calls %d status %d body %q", calls.Load(), recorder.Code, recorder.Body.String())
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != account.ID() {
		t.Fatalf("committed winner was not bound: account=%d ok=%v", boundID, ok)
	}
}

type continuousRetryNativeTransportScenario struct {
	calls      atomic.Int32
	accountIDs []int64
}

func newContinuousRetryNativeTransportScenario(t *testing.T) (*auth.Store, *continuousRetryNativeTransportScenario) {
	t.Helper()
	previousExecute := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExecute })

	scenario := &continuousRetryNativeTransportScenario{}
	WebsocketExecuteFunc = func(_ context.Context, account *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		scenario.accountIDs = append(scenario.accountIDs, account.ID())
		if scenario.calls.Add(1) == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_sticky\"}}\n\n" +
					"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"sticky-success\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sticky\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 1, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.SetRetryIntervalMS(0)
	store.SetTransportRetryPolicy(transportRetryPolicySticky)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token-one", PlanType: "pro", AccountID: "test-account-one"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "test-token-two", PlanType: "pro", AccountID: "test-account-two"})
	return store, scenario
}

func TestContinuousRetryBufferedStickyTransportRetryKeepsSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexForceWebsocket = true
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	ApplyRuntimeSettings(settings)

	tests := []continuousRetryLocalEndpointCase{
		{
			name: "responses", path: "/v1/responses",
			body:   `{"model":"gpt-5.4","input":"hello","stream":true}`,
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name: "chat completions", path: "/v1/chat/completions",
			body:   `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name: "anthropic messages", path: "/v1/messages",
			body:   `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, scenario := newContinuousRetryNativeTransportScenario(t)
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

			recorder, affinityKey := continuousRetryLocalRequest(t, tc, handler)
			if scenario.calls.Load() != 2 || len(scenario.accountIDs) != 2 {
				t.Fatalf("transport attempts = calls %d accounts %v; body=%q", scenario.calls.Load(), scenario.accountIDs, recorder.Body.String())
			}
			if scenario.accountIDs[0] != scenario.accountIDs[1] {
				t.Fatalf("buffered sticky retry rotated accounts: %v", scenario.accountIDs)
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "sticky-success") {
				t.Fatalf("sticky retry response = status %d body %q", recorder.Code, recorder.Body.String())
			}
			if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != scenario.accountIDs[1] {
				t.Fatalf("committed sticky winner affinity = account %d ok=%v, attempts=%v", boundID, ok, scenario.accountIDs)
			}
		})
	}
}

func TestContinuousRetryDisabledMessagesStickyTransportRetryRotatesAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexForceWebsocket = true
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)

	store, scenario := newContinuousRetryNativeTransportScenario(t)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	tc := continuousRetryLocalEndpointCase{
		path:   "/v1/messages",
		body:   `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
		invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
	}

	recorder, affinityKey := continuousRetryLocalRequest(t, tc, handler)
	if scenario.calls.Load() != 2 || len(scenario.accountIDs) != 2 {
		t.Fatalf("transport attempts = calls %d accounts %v; body=%q", scenario.calls.Load(), scenario.accountIDs, recorder.Body.String())
	}
	if scenario.accountIDs[0] == scenario.accountIDs[1] {
		t.Fatalf("policy-disabled retry stayed on the failed account: %v", scenario.accountIDs)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "sticky-success") {
		t.Fatalf("rotated retry response = status %d body %q", recorder.Code, recorder.Body.String())
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); !ok || boundID != scenario.accountIDs[1] {
		t.Fatalf("rotated winner affinity = account %d ok=%v, attempts=%v", boundID, ok, scenario.accountIDs)
	}
}

func TestContinuousRetryBufferedRelayTransportRetryKeepsSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexForceWebsocket = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	ApplyRuntimeSettings(settings)

	var calls atomic.Int32
	authorizations := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test upstream does not support connection hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack first transport attempt: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"relay-sticky-success\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_relay_sticky\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
		)
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-4.1-direct", MaxRetries: 1, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.SetRetryIntervalMS(0)
	store.SetTransportRetryPolicy(transportRetryPolicySticky)
	for id, key := range []string{"test-relay-key-one", "test-relay-key-two"} {
		store.AddAccount(&auth.Account{
			DBID: int64(id + 1), UpstreamType: auth.UpstreamOpenAIResponses,
			BaseURL: upstream.URL, APIKey: key, Models: []string{"gpt-4.1-direct"}, PlanType: "api",
		})
	}
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	tc := continuousRetryLocalEndpointCase{
		path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
		invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
	}
	recorder, affinityKey := continuousRetryLocalRequest(t, tc, handler)
	if calls.Load() != 2 {
		t.Fatalf("relay transport attempts = %d, want 2; body=%q", calls.Load(), recorder.Body.String())
	}
	firstAuthorization := <-authorizations
	secondAuthorization := <-authorizations
	if firstAuthorization == "" || firstAuthorization != secondAuthorization {
		t.Fatalf("buffered relay sticky retry changed account credentials: first=%q second=%q", firstAuthorization, secondAuthorization)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "relay-sticky-success") {
		t.Fatalf("relay sticky retry response = status %d body %q", recorder.Code, recorder.Body.String())
	}
	if _, ok := store.SessionAffinityAccountID(affinityKey); !ok {
		t.Fatal("relay sticky winner did not leave a committed affinity binding")
	}
}

func TestContinuousRetryResponsesWebSocketBufferedTransportRetrySticky(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	ApplyRuntimeSettings(settings)

	first, second := runWSTransportRetryScenario(t, transportRetryPolicySticky)
	if first != second {
		t.Fatalf("buffered Responses WS sticky retry rotated accounts: first=%d second=%d", first, second)
	}
}

func TestContinuousRetryCapacityShedRetainsPendingAffinity(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-access", PlanType: "pro"}
	store.AddAccount(account)
	handler := &Handler{store: store}
	retries := map[int64]int{}
	exclusions := newRetryAccountExclusions()
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	handler.unbindOrRetainAffinityForCapacityShed(exclusions, "capacity-local", account, "", streamOutcome{capacityShed: true}, retries, policy)
	if retries[account.ID()] != 1 {
		t.Fatalf("capacity retry count = %d, want 1", retries[account.ID()])
	}
	if boundID, ok := store.SessionAffinityAccountID("capacity-local"); !ok || boundID != account.ID() {
		t.Fatalf("capacity retry lost same-account affinity: account=%d ok=%v", boundID, ok)
	}
}

func TestContinuousRetryGrokNativeReplayLimitIsLocalAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	resetResponseCacheStateForTest(defaultResponseCacheConfig())

	tests := []struct {
		name          string
		protocol      GrokProtocol
		path          string
		body          string
		upstreamPath  string
		upstreamSSE   string
		wantMarkers   []string
		forbidMarkers []string
		invoke        func(*Handler, *gin.Context)
	}{
		{
			name: "responses", protocol: GrokProtocolResponses, path: "/v1/responses",
			body:         `{"model":"grok-4.5","input":"hello","stream":true}`,
			upstreamPath: "/v1/responses",
			upstreamSSE: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_grok_local\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"private-grok-partial\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_grok_local\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
			wantMarkers:   []string{`"type":"response.failed"`, `"type":"server_error"`, `"code":"internal_error"`},
			forbidMarkers: []string{"private-grok-partial", `"type":"response.completed"`},
			invoke:        func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name: "chat completions", protocol: GrokProtocolChatCompletions, path: "/v1/chat/completions",
			body:         `{"model":"grok-4.5","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			upstreamPath: "/v1/chat/completions",
			upstreamSSE: "data: {\"id\":\"chatcmpl_grok_local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"chatcmpl_grok_local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"private-grok-partial\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"chatcmpl_grok_local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
			wantMarkers:   []string{`"type":"server_error"`, `"code":"internal_error"`},
			forbidMarkers: []string{"private-grok-partial", "data: [DONE]", `"finish_reason":"stop"`},
			invoke:        func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name: "anthropic messages", protocol: GrokProtocolMessages, path: "/v1/messages",
			body:         `{"model":"grok-4.5","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			upstreamPath: "/v1/messages",
			upstreamSSE: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_grok_local\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"private-grok-partial\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantMarkers:   []string{"event: error", `"type":"api_error"`},
			forbidMarkers: []string{"private-grok-partial", "event: message_stop"},
			invoke:        func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			seenPaths := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				seenPaths <- r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.upstreamSSE)
			}))
			t.Cleanup(upstream.Close)
			store, account := newContinuousRetryLocalGrokNativeStore(t, upstream.URL, tc.protocol)
			seedContinuousRetryLocalHealth(account)
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			handler.SetRuntimeCache(cache.NewMemory(1))
			handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
				return newContinuousRetryReplayWithLimits(0, 1)
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestBody := []byte(tc.body)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(requestBody))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("X-Codex2API-Affinity-Key", "local-grok-native-"+string(tc.protocol))
			affinityKey := sessionAffinityKey(resolveRequestSessionIdentity(ctx.Request.Header, requestBody).affinityID, 0)
			codexTurnStateOrigins.Delete(affinityKey)
			t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
			tc.invoke(handler, ctx)

			body := recorder.Body.String()
			if calls.Load() != 1 {
				t.Fatalf("Grok native upstream calls = %d, want exactly one; body=%q", calls.Load(), body)
			}
			seenPath := <-seenPaths
			if seenPath != tc.upstreamPath {
				t.Fatalf("Grok native upstream = calls %d path %q, want one %q; body=%q", calls.Load(), seenPath, tc.upstreamPath, body)
			}
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(body, continuousRetryLocalFailureMessage) {
				t.Fatalf("Grok native local terminal = status %d body %q", recorder.Code, body)
			}
			for _, marker := range tc.wantMarkers {
				if !strings.Contains(body, marker) {
					t.Fatalf("Grok native local terminal missing %q: %q", marker, body)
				}
			}
			for _, marker := range tc.forbidMarkers {
				if strings.Contains(body, marker) {
					t.Fatalf("Grok native private output or success terminal %q leaked: %q", marker, body)
				}
			}
			if boundID, ok := store.SessionAffinityAccountID(affinityKey); ok {
				t.Fatalf("Grok native local failure created affinity binding to account %d", boundID)
			}
			if _, ok := codexTurnStateOrigins.Load(affinityKey); ok {
				t.Fatal("Grok native local failure recorded turn-state provenance")
			}
			if got := getResponseCache(responseCacheOwner(0), "resp_grok_local"); len(got) != 0 {
				t.Fatalf("Grok native local failure cached completed response: %s", got)
			}
			if account.HasActiveCooldown() || account.IsModelRateLimited("grok-4.5") {
				t.Fatal("Grok native local failure changed account cooldown state")
			}
			assertContinuousRetryLocalHealthUnchanged(t, account)
		})
	}
}

func TestContinuousRetryImageReplayLimitIsLocalProtocolFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.image_generation_call.partial_image","partial_image_b64":"`+strings.Repeat("A", 256)+`","partial_image_index":0}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"created_at":1710000000,"output":[]}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	store, account := newContinuousRetryLocalNativeStore(t, upstream.URL)
	seedContinuousRetryLocalHealth(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
		return newContinuousRetryReplayWithLimits(0, 1)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	responsesBody := []byte(`{"model":"gpt-5.4","input":"draw a test image","tools":[{"type":"image_generation","model":"gpt-image-2"}],"stream":true}`)
	handler.forwardImagesRequest(ctx, "/v1/images/generations", "gpt-image-2", "gpt-image-2", "gpt-image-2", responsesBody, "b64_json", "image_generation", true)

	body := recorder.Body.String()
	// Images publishes its existing connected comment before model output, so a
	// later local replay failure must stay in-protocol on the already-committed
	// 200 stream. 生图流先发既有 connected 注释保活，后续本地失败必须用 SSE
	// 错误收尾，不能伪造新的 HTTP 状态。
	if calls.Load() != 1 || recorder.Code != http.StatusOK {
		t.Fatalf("image local failure = calls %d status %d body %q", calls.Load(), recorder.Code, body)
	}
	for _, marker := range []string{"event: error", `"type":"server_error"`, `"code":"internal_error"`, continuousRetryLocalFailureMessage} {
		if !strings.Contains(body, marker) {
			t.Fatalf("image local failure missing %q: %q", marker, body)
		}
	}
	if !strings.HasPrefix(body, imageStreamConnectedComment) {
		t.Fatalf("image stream lost connected prelude: %q", body)
	}
	if strings.Contains(body, strings.Repeat("A", 64)) || strings.Contains(body, "image_generation.completed") || strings.Contains(body, "data: [DONE]") {
		t.Fatalf("private image output or success terminal leaked: %q", body)
	}
	if account.HasActiveCooldown() || account.IsModelRateLimited("gpt-image-2") {
		t.Fatal("image local replay failure changed account cooldown state")
	}
	assertContinuousRetryLocalHealthUnchanged(t, account)
}

func TestContinuousRetryResponsesWSReplayLimitWritesErrorBeforeClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	previousExecute := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExecute })
	var calls atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"private-ws-partial\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws_local\",\"status\":\"completed\"}}\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-access", PlanType: "pro", AccountID: "test-account"}
	store.AddAccount(account)
	seedContinuousRetryLocalHealth(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.continuousRetryReplayFactory = func() *continuousRetryReplay {
		return newContinuousRetryReplayWithLimits(0, 1)
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	requestBody := []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)
	headers := http.Header{"X-Codex2API-Affinity-Key": []string{"local-ws-affinity"}}
	affinityKey := sessionAffinityKey(resolveRequestSessionIdentity(headers, requestBody).affinityID, 0)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Responses websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, requestBody); err != nil {
		t.Fatalf("write Responses websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read local error frame: %v", err)
	}
	if gjson.GetBytes(event, "type").String() != "error" || gjson.GetBytes(event, "error.type").String() != "server_error" || gjson.GetBytes(event, "error.code").String() != "server_error" || gjson.GetBytes(event, "error.message").String() != continuousRetryLocalFailureMessage {
		t.Fatalf("local websocket error frame = %s", event)
	}
	if strings.Contains(string(event), "private-ws-partial") || strings.Contains(string(event), "response.completed") {
		t.Fatalf("private websocket output leaked: %s", event)
	}
	_, _, closeErr := conn.ReadMessage()
	var websocketClose *websocket.CloseError
	if !errors.As(closeErr, &websocketClose) || websocketClose.Code != websocket.CloseInternalServerErr {
		t.Fatalf("websocket close = %v, want 1011", closeErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("websocket upstream calls = %d, want exactly 1", calls.Load())
	}
	if boundID, ok := store.SessionAffinityAccountID(affinityKey); ok {
		t.Fatalf("websocket local failure created affinity binding to account %d", boundID)
	}
	assertContinuousRetryLocalHealthUnchanged(t, account)
}
