package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func readResponsesWSTerminalEvent(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	for i := 0; i < 16; i++ {
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket event: %v", err)
		}
		switch gjson.GetBytes(event, "type").String() {
		case "response.completed", "response.failed", "error":
			return event
		}
	}
	t.Fatal("did not receive a terminal websocket event")
	return nil
}

func newUsageLimitedRelayStore(upstreamURL string) (*auth.Store, *auth.Account) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		MaxRetries:             0,
		MaxRateLimitRetries:    0,
		IgnoreUsageLimitStatus: true,
	})
	account := &auth.Account{
		DBID:                1,
		UpstreamType:        auth.UpstreamOpenAIResponses,
		BaseURL:             upstreamURL,
		APIKey:              "relay-token",
		Models:              []string{"gpt-5.4"},
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}
	store.AddAccount(account)
	return store, account
}

func TestResponsesTurnStateAllowsOnlyBoundTurnPastWHAMLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seenBody []byte
	upstream := newContinuationRelayUpstream(t, false, &seenBody)
	store, account := newUsageLimitedRelayStore(upstream.URL)
	handler := NewHandler(store, nil, nil, nil)
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"continue"}],"stream":true}`)

	fresh := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Request.Header.Set("Session-Id", "turn-session")
	}, handler.Responses, body)
	if fresh.Code != http.StatusTooManyRequests {
		t.Fatalf("fresh status = %d, want 429; body=%s", fresh.Code, fresh.Body.String())
	}
	if code := gjson.GetBytes(fresh.Body.Bytes(), "error.code").String(); code != "rate_limit_reached" {
		t.Fatalf("fresh error.code = %q, want rate_limit_reached", code)
	}
	if len(seenBody) != 0 {
		t.Fatalf("fresh request reached upstream despite WHAM limit: %s", seenBody)
	}

	store.BindSessionAffinity("turn-session", account, "")
	continued := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Request.Header.Set("Session-Id", "turn-session")
		c.Request.Header.Set(codexTurnStateHeader, "turn-state-1")
	}, handler.Responses, body)
	if continued.Code != http.StatusOK {
		t.Fatalf("continued status = %d, want 200; body=%s", continued.Code, continued.Body.String())
	}
	if len(seenBody) == 0 {
		t.Fatal("bound turn continuation did not reach upstream")
	}
}

func TestResponsesTurnStateExpiredBindingUsesBaselineSchedulerWhenContinuousRetryDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "1ns")
	previousRuntime := CurrentRuntimeSettings()
	nextRuntime := previousRuntime
	nextRuntime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(nextRuntime)
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })

	newUpstream := func(hits *atomic.Int64) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_done","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		}))
		t.Cleanup(server.Close)
		return server
	}

	var boundHits, fallbackHits atomic.Int64
	boundUpstream := newUpstream(&boundHits)
	fallbackUpstream := newUpstream(&fallbackHits)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	bound := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: boundUpstream.URL,
		APIKey: "bound-token", Models: []string{"gpt-5.4"}, PlanType: "api",
	}
	fallback := &auth.Account{
		DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: fallbackUpstream.URL,
		APIKey: "fallback-token", Models: []string{"gpt-5.4"}, PlanType: "api",
	}
	fallback.SetSchedulerPriority(20)
	store.AddAccount(bound)
	store.AddAccount(fallback)
	store.BindSessionAffinity("expired-http-turn", bound, "")
	time.Sleep(time.Millisecond)

	handler := NewHandler(store, nil, nil, nil)
	recorder := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Request.Header.Set("Session-Id", "expired-http-turn")
		c.Request.Header.Set(codexTurnStateHeader, "turn-state")
	}, handler.Responses, []byte(`{"model":"gpt-5.4","input":"continue","stream":true}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := boundHits.Load(); got != 0 {
		t.Fatalf("expired bound account received %d request(s), want 0", got)
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("baseline scheduler fallback requests = %d, want 1", got)
	}
}

func TestResponsesTurnStateDoesNotRerouteAfterAuthoritativeLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_wrong_account","output":[]}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store, limited := newUsageLimitedRelayStore(upstream.URL)
	store.AddAccount(&auth.Account{
		DBID:         2,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "healthy-token",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	store.BindSessionAffinity("limited-turn", limited, "")
	store.MarkResponsesPremium5hRateLimited(limited, time.Now().Add(time.Hour))

	handler := NewHandler(store, nil, nil, nil)
	recorder := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Request.Header.Set("Session-Id", "limited-turn")
		c.Request.Header.Set(codexTurnStateHeader, "turn-state-hard-limited")
	}, handler.Responses, []byte(`{"model":"gpt-5.4","input":"continue","stream":true}`))

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
	}
	if code := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); code != "rate_limit_reached" {
		t.Fatalf("error.code = %q, want rate_limit_reached", code)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("active turn was rerouted after authoritative limit: upstream hits=%d", upstreamHits.Load())
	}
}

func TestResponsesReconcilesEndpointEnabledDirectlyInDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_reconciled","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "responses-reconcile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	insertDisabled := func(name, baseURL string) int64 {
		t.Helper()
		id, err := db.InsertOpenAIResponsesAccount(ctx, name, map[string]interface{}{
			"upstream_type": auth.UpstreamOpenAIResponses,
			"base_url":      baseURL,
			"api_key":       "sk-test",
			"models":        []string{"gpt-5.4"},
		}, "")
		if err != nil {
			t.Fatalf("InsertOpenAIResponsesAccount(%s): %v", name, err)
		}
		if err := db.SetAccountEnabled(ctx, id, false); err != nil {
			t.Fatalf("SetAccountEnabled(%d, false): %v", id, err)
		}
		return id
	}
	insertDisabled("still-disabled", "https://disabled.example")
	healthyID := insertDisabled("newly-enabled", upstream.URL)

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		MaxRetries:           0,
		MaxRateLimitRetries:  0,
		FastSchedulerEnabled: true,
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	if err := db.SetAccountEnabled(ctx, healthyID, true); err != nil {
		t.Fatalf("SetAccountEnabled(%d, true): %v", healthyID, err)
	}

	handler := NewHandler(store, db, nil, nil)
	recorder := invokeResponsesHandler(t, handler.Responses, []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after DB reconciliation; body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("newly enabled upstream hits = %d, want 1", upstreamHits.Load())
	}
}

func TestResponsesCopiesCodexTurnStateResponseHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set(codexTurnStateHeader, "turn-state-upstream")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_relay","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newContinuationRelayStore(upstream.URL)
	handler := NewHandler(store, nil, nil, nil)
	recorder := invokeResponsesHandler(t, handler.Responses, []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	if got := recorder.Header().Get(codexTurnStateHeader); got != "turn-state-upstream" {
		t.Fatalf("%s = %q, want turn-state-upstream", codexTurnStateHeader, got)
	}
}

func TestResponsesWebSocketTurnStateRetainsLimitedAccountOnlyWithinTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	served := make(chan int64, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		served <- account.ID()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`data: {"type":"response.completed","response":{"id":"resp_done","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		IgnoreUsageLimitStatus: true,
	})
	limited := &auth.Account{
		DBID:                1,
		AccessToken:         "limited",
		PlanType:            "plus",
		AccountID:           "limited",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}
	limited.SetSchedulerPriority(20)
	healthy := &auth.Account{DBID: 2, AccessToken: "healthy", PlanType: "plus", AccountID: "healthy"}
	store.AddAccount(limited)
	store.AddAccount(healthy)
	store.BindSessionAffinity("ws-turn", limited, "")

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	run := func(payload string) {
		t.Helper()
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			if response != nil {
				t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
			}
			t.Fatalf("dial websocket failed: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatalf("write websocket request: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
		if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
			t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
		}
	}

	run(`{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"ws-turn","input":"tool output","client_metadata":{"x-codex-turn-state":"turn-1"}}`)
	run(`{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"ws-turn","input":"new turn"}`)

	if first := <-served; first != limited.ID() {
		t.Fatalf("same-turn account = %d, want limited bound account %d", first, limited.ID())
	}
	if second := <-served; second != healthy.ID() {
		t.Fatalf("new-turn account = %d, want healthy account %d", second, healthy.ID())
	}
}

func TestResponsesWebSocketTurnStateExpiredBindingUsesBaselineSchedulerWhenContinuousRetryDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "1ns")
	previousRuntime := CurrentRuntimeSettings()
	nextRuntime := previousRuntime
	nextRuntime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	nextRuntime.CodexWSSilentRetry = false
	nextRuntime.CodexWSSilentRetries = 0
	ApplyRuntimeSettings(nextRuntime)
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	served := make(chan int64, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		served <- account.ID()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`data: {"type":"response.completed","response":{"id":"resp_done","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	bound := &auth.Account{DBID: 1, AccessToken: "bound-token", PlanType: "plus", AccountID: "bound"}
	fallback := &auth.Account{DBID: 2, AccessToken: "fallback-token", PlanType: "plus", AccountID: "fallback"}
	fallback.SetSchedulerPriority(20)
	store.AddAccount(bound)
	store.AddAccount(fallback)
	store.BindSessionAffinity("expired-ws-turn", bound, "")
	time.Sleep(time.Millisecond)

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	request := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"expired-ws-turn","input":"continue","client_metadata":{"x-codex-turn-state":"turn-state"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	event := readResponsesWSTerminalEvent(t, conn)
	if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
		t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
	}
	select {
	case accountID := <-served:
		if accountID != fallback.ID() {
			t.Fatalf("selected account = %d, want baseline fallback account %d", accountID, fallback.ID())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream account selection")
	}
}

func TestResponsesWebSocketFreshTurnReportsUsageLimitAs429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var upstreamCalls atomic.Int64
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, fmt.Errorf("unexpected upstream call for account %d", account.ID())
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		IgnoreUsageLimitStatus: true,
	})
	store.AddAccount(&auth.Account{
		DBID:                1,
		AccessToken:         "limited",
		PlanType:            "plus",
		AccountID:           "limited",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"new turn"}`)); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket error event: %v", err)
	}
	if code := gjson.GetBytes(event, "error.code").String(); code != "rate_limit_reached" {
		t.Fatalf("error.code = %q, want rate_limit_reached; body=%s", code, event)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
	}
}

func TestResponsesWebSocketTurnStateDoesNotRerouteAfterAuthoritativeLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var upstreamCalls atomic.Int64
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_wrong_account"}}` + "\n\n")),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		IgnoreUsageLimitStatus: true,
	})
	limited := &auth.Account{
		DBID:                1,
		AccessToken:         "limited-token",
		AccountID:           "limited-account",
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}
	store.AddAccount(limited)
	store.AddAccount(&auth.Account{
		DBID:        2,
		AccessToken: "healthy-token",
		AccountID:   "healthy-account",
		PlanType:    "plus",
	})
	store.BindSessionAffinity("ws-limited-turn", limited, "")
	store.MarkResponsesPremium5hRateLimited(limited, time.Now().Add(time.Hour))

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"ws-limited-turn","input":"continue","client_metadata":{"x-codex-turn-state":"turn-hard-limited"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket error event: %v", err)
	}
	if code := gjson.GetBytes(event, "error.code").String(); code != "rate_limit_reached" {
		t.Fatalf("error.code = %q, want rate_limit_reached; body=%s", code, event)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("active WebSocket turn was rerouted after authoritative limit: upstream calls=%d", upstreamCalls.Load())
	}
}

func TestResponsesWebSocketUpgradeTurnStateDoesNotAuthorizeFreshFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	served := make(chan int64, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		served <- account.ID()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`data: {"type":"response.completed","response":{"id":"resp_fresh","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		IgnoreUsageLimitStatus: true,
	})
	limited := &auth.Account{
		DBID:                1,
		AccessToken:         "limited-token",
		AccountID:           "limited-account",
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}
	healthy := &auth.Account{DBID: 2, AccessToken: "healthy-token", AccountID: "healthy-account", PlanType: "plus"}
	store.AddAccount(limited)
	store.AddAccount(healthy)
	store.BindSessionAffinity("ws-upgrade-turn", limited, "")

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	upgradeHeaders := make(http.Header)
	upgradeHeaders.Set(codexTurnStateHeader, "connection-scoped-token")
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", upgradeHeaders)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"ws-upgrade-turn","input":"fresh turn"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, event, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read websocket response: %v", err)
	} else if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
		t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
	}
	if accountID := <-served; accountID != healthy.ID() {
		t.Fatalf("fresh frame used account %d, want healthy account %d", accountID, healthy.ID())
	}
}

func TestResponsesWebSocketPinnedTurnDegradesPreviousResponseFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	prevSettings := CurrentRuntimeSettings()
	nextSettings := prevSettings
	nextSettings.FirstTokenMode = FirstTokenModeLoose
	nextSettings.CodexPreflightSSEPassthrough = true
	ApplyRuntimeSettings(nextSettings)
	t.Cleanup(func() { ApplyRuntimeSettings(prevSettings) })

	var attempts atomic.Int32
	var retriedWithoutContinuation atomic.Bool
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attempts.Add(1)
		if gjson.GetBytes(requestBody, "previous_response_id").String() != "" {
			body := `data: {"type":"codex.rate_limits","plan_type":"plus"}` + "\n\n" +
				`data: {"type":"codex.response.metadata","headers":{"x-codex-turn-state":"turn"}}` + "\n\n" +
				`data: {"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"missing response"}}}` + "\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		retriedWithoutContinuation.Store(true)
		completed := `data: {"type":"response.completed","response":{"id":"resp_new","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(completed))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, IgnoreUsageLimitStatus: true})
	account := &auth.Account{DBID: 1, AccessToken: "token", AccountID: "account", PlanType: "plus"}
	store.AddAccount(account)
	store.BindSessionAffinity("ws-pinned-failure", account, "")

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"ws-pinned-failure","previous_response_id":"resp_missing","input":"continue","client_metadata":{"x-codex-turn-state":"turn-state"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("write websocket request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	event := readResponsesWSTerminalEvent(t, conn)
	if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
		t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
	}
	if !retriedWithoutContinuation.Load() {
		t.Fatal("pinned turn-state still forwarded previous_response_not_found instead of degrading")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (rejected + degraded retry)", got)
	}
}
