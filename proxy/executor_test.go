package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestReadSSEStream_MergesMultilineData(t *testing.T) {
	input := strings.NewReader("data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n" +
		"data: [DONE]\n\n")

	var events []string
	err := ReadSSEStream(input, func(data []byte) bool {
		events = append(events, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStream returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	want := "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}"
	if events[0] != want {
		t.Fatalf("unexpected merged event: got %q want %q", events[0], want)
	}
}

func TestClassifyStreamOutcome(t *testing.T) {
	tests := []struct {
		name         string
		ctxErr       error
		readErr      error
		writeErr     error
		gotTerminal  bool
		wantStatus   int
		wantKind     string
		wantPenalize bool
	}{
		{
			name:        "terminal success",
			gotTerminal: true,
			wantStatus:  200,
		},
		{
			name:         "client canceled",
			ctxErr:       context.Canceled,
			wantStatus:   logStatusClientClosed,
			wantPenalize: false,
		},
		{
			name:         "upstream timeout",
			readErr:      errors.New("read timeout"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "timeout",
			wantPenalize: true,
		},
		{
			name:         "websocket message too big",
			readErr:      errors.New("websocket read error: websocket: close 1009 (message too big)"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     upstreamErrorKindMessageTooBig,
			wantPenalize: true,
		},
		{
			name:         "upstream early eof",
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "transport",
			wantPenalize: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyStreamOutcome(tc.ctxErr, tc.readErr, tc.writeErr, tc.gotTerminal)
			if outcome.logStatusCode != tc.wantStatus {
				t.Fatalf("status mismatch: got %d want %d", outcome.logStatusCode, tc.wantStatus)
			}
			if outcome.failureKind != tc.wantKind {
				t.Fatalf("failure kind mismatch: got %q want %q", outcome.failureKind, tc.wantKind)
			}
			if outcome.penalize != tc.wantPenalize {
				t.Fatalf("penalize mismatch: got %v want %v", outcome.penalize, tc.wantPenalize)
			}
		})
	}
}

func TestShouldFallbackWebsocketMessageTooBigToHTTP(t *testing.T) {
	outcome := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    upstreamErrorKindMessageTooBig,
		failureMessage: "上游流读取失败: websocket read error: websocket: close 1009 (message too big)",
		penalize:       true,
	}

	if !shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, nil, nil) {
		t.Fatal("expected websocket message-too-big before first downstream bytes to fall back to HTTP")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, false, false, nil, nil) {
		t.Fatal("HTTP upstream should not fall back again")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, true, nil, nil) {
		t.Fatal("should not fall back after downstream body has been written")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, context.Canceled, nil) {
		t.Fatal("should not fall back after downstream context is canceled")
	}
}

func TestClassifyResponseFailedOutcome(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"An error occurred while processing your request. Please include the request ID req-123."}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusInternalServerError)
	}
	if outcome.failureKind != "server" {
		t.Fatalf("failure kind = %q, want server", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("response.failed server error should be penalized")
	}
	if !strings.Contains(outcome.failureMessage, "server_error") || !strings.Contains(outcome.failureMessage, "req-123") {
		t.Fatalf("failure message = %q, want upstream code and request id", outcome.failureMessage)
	}
}

func TestClassifyResponseFailedOutcomeInvalidRequest(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_value","type":"invalid_request_error","message":"Invalid input"}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
	}
	if outcome.failureKind != "client" {
		t.Fatalf("failure kind = %q, want client", outcome.failureKind)
	}
	if outcome.penalize {
		t.Fatal("client-side response.failed should not penalize account")
	}
}

func TestClassifyResponseFailedOutcomeUsageLimit(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusTooManyRequests)
	}
	if outcome.failureKind != "usage_limit" {
		t.Fatalf("failure kind = %q, want usage_limit", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("usage_limit response.failed should penalize account")
	}
	if !IsUsageLimitReachedError(payload) {
		t.Fatal("nested response.failed usage_limit_reached should be detected")
	}
}

func TestShouldRecyclePooledClient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "connection shutting down",
			err:  errors.New("http2: client connection is shutting down"),
			want: true,
		},
		{
			name: "connection reset",
			err:  errors.New("read tcp: connection reset by peer"),
			want: true,
		},
		{
			name: "broken pipe",
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "plain timeout",
			err:  errors.New("read timeout"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRecyclePooledClient(tc.err); got != tc.want {
				t.Fatalf("shouldRecyclePooledClient() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldTransparentRetryStream(t *testing.T) {
	retryable := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream failed before first byte",
		penalize:       true,
	}

	if !shouldTransparentRetryStream(retryable, 0, 2, false, nil, nil) {
		t.Fatal("expected early upstream failure to be transparently retried")
	}
	if shouldTransparentRetryStream(retryable, 2, 2, false, nil, nil) {
		t.Fatal("expected retry to stop at maxRetries")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, true, nil, nil) {
		t.Fatal("expected retry to stop after downstream already received bytes")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, false, context.Canceled, nil) {
		t.Fatal("expected retry to stop when downstream context is canceled")
	}
}

func TestApplyCodexRequestHeadersUsesSessionIDWithoutConversationID(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}
	cfg := &DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
	}
	downstreamHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", cfg, downstreamHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Session_id"); got != "cache-key-1" {
		t.Fatalf("Session_id = %q", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := req.Header.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want fallback %q", got, Originator)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	for _, name := range []string{"X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Os", "X-Stainless-Arch"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexRequestHeadersUsesMinimalFallbackByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("User-Agent"); got != latestCodexCLIUserAgentPrefix {
		t.Fatalf("User-Agent = %q, want minimal Codex CLI %q", got, latestCodexCLIUserAgentPrefix)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOfficialClientHeaders(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent":            []string{"codex_vscode/1.2.3"},
		"Originator":            []string{"codex_vscode"},
		"Version":               []string{"1.2.3"},
		"X-Codex-Turn-State":    []string{"turn-state"},
		"X-Codex-Turn-Metadata": []string{"turn-metadata"},
		"X-Client-Request-Id":   []string{"req-123"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "codex_vscode/1.2.3" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("Originator = %q", got)
	}
	if got := req.Header.Get("Version"); got != "1.2.3" {
		t.Fatalf("Version = %q", got)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id"} {
		if got := req.Header.Get(name); got != downstreamHeaders.Get(name) {
			t.Fatalf("%s = %q, want %q", name, got, downstreamHeaders.Get(name))
		}
	}
}

func TestApplyCodexRequestHeadersAutoUpgradesOldCodexCLI(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464"},
		"Originator": []string{Originator},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got == downstreamHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved old CLI UA %q", got)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersAutoDoesNotUpgradeEmbeddedCodexToken(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	spoofedUA := "Mozilla/5.0 codex_cli_rs/0.117.0"
	downstreamHeaders := http.Header{
		"User-Agent": []string{spoofedUA},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != spoofedUA {
		t.Fatalf("User-Agent = %q, want legacy-preserved spoofed UA %q", got, spoofedUA)
	}
	if got := req.Header.Get("Version"); got != "0.117.0" {
		t.Fatalf("Version = %q, want parsed legacy version 0.117.0", got)
	}
}

func TestApplyCodexRequestHeadersFallsBackForNonOfficialClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"curl/8.0"},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != latestCodexCLIUserAgentPrefix {
		t.Fatalf("User-Agent = %q, want %q", got, latestCodexCLIUserAgentPrefix)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want %q", got, Originator)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOpenCodeClient(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"opencode/0.5.0"},
		"Originator": []string{"opencode"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "opencode/0.5.0" {
		t.Fatalf("User-Agent = %q, want %q", got, "opencode/0.5.0")
	}
	if got := req.Header.Get("Originator"); got != "opencode" {
		t.Fatalf("Originator = %q, want %q", got, "opencode")
	}
}

func TestCodexTransportModeDefaultsToStandard(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "")
	if _, ok := newCodexTransport("").(*http.Transport); !ok {
		t.Fatalf("newCodexTransport default = %T, want *http.Transport", newCodexTransport(""))
	}
}

func TestCodexTransportModeCanUseUTLSChrome(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	if _, ok := newCodexTransport("").(*utlsRoundTripper); !ok {
		t.Fatalf("newCodexTransport utls_chrome = %T, want *utlsRoundTripper", newCodexTransport(""))
	}
}

func TestClientPoolKeyIncludesTransportMode(t *testing.T) {
	acc := &auth.Account{DBID: 42}
	standard := clientPoolKey(acc, "http://proxy", codexTransportModeStandard)
	utlsChrome := clientPoolKey(acc, "http://proxy", codexTransportModeUTLSChrome)
	if standard == utlsChrome {
		t.Fatalf("clientPoolKey should include transport mode, got %q", standard)
	}
}

func TestIsolateCodexSessionIDUsesAPIKeyScope(t *testing.T) {
	raw := "session-1"
	if got := IsolateCodexSessionID(0, raw); got != raw {
		t.Fatalf("IsolateCodexSessionID without api key = %q, want %q", got, raw)
	}
	first := IsolateCodexSessionID(1, raw)
	second := IsolateCodexSessionID(2, raw)
	if first == raw || second == raw || first == second {
		t.Fatalf("expected distinct isolated session ids, got first=%q second=%q raw=%q", first, second, raw)
	}
}

func TestResolveSessionIDPrefersContinuityHeaders(t *testing.T) {
	headers := http.Header{
		"Session_id":      []string{"session-from-header"},
		"Conversation_id": []string{"conversation-from-header"},
		"Authorization":   []string{"Bearer sk-test-123"},
	}

	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "session-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "session-from-header")
	}

	headers.Del("Session_id")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "conversation-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "conversation-from-header")
	}

	headers.Del("Conversation_id")
	headers.Set("Idempotency-Key", "idempotency-key-1")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "idempotency-key-1" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "idempotency-key-1")
	}
}

func TestResolveExplicitSessionIDDoesNotUseAPIKeyFallback(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-test-123"}}

	if got := ResolveExplicitSessionID(headers, []byte(`{}`)); got != "" {
		t.Fatalf("ResolveExplicitSessionID() = %q, want empty", got)
	}
	if got := ResolveSessionID(headers, []byte(`{}`)); got == "" {
		t.Fatal("ResolveSessionID() should still generate API-key fallback")
	}
}

func TestExecuteRequestExplicitFalseBypassesForcedWebsocket(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	wsCalled := false
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalled = true
		return nil, errors.New("websocket should not be used")
	}

	_, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{}, false)
	if err == nil {
		t.Fatal("ExecuteRequest() error = nil, want missing account error after bypassing websocket")
	}
	if wsCalled {
		t.Fatal("WebsocketExecuteFunc was called despite explicit useWebsocket=false")
	}
}

func TestExecuteRequestForcedWebsocketFallsBackToHTTPWithoutExplicitSession(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	// per-api-key 模式：恢复 v2 旧行为，断言 prompt_cache_key 跨请求确定性（回归保护 8ea79aa）。
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	wsCalled := false
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalled = true
		return nil, errors.New("websocket should not be used for sessionless requests")
	}

	_, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{})
	if err == nil {
		t.Fatal("ExecuteRequest() error = nil, want HTTP path missing account error")
	}
	if wsCalled {
		t.Fatal("WebsocketExecuteFunc was called for a sessionless request")
	}
}

func TestExecuteRequestForcedWebsocketUsesExplicitSession(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	nextSettings.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	var gotSessionID string
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		gotSessionID = sessionID
		if poolRouteKey != "" {
			t.Fatalf("explicit session poolRouteKey = %q, want empty", poolRouteKey)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, []byte(`{"model":"gpt-5.4"}`), "explicit-session-1", "", "sk-local", nil, http.Header{})
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	resp.Body.Close()
	if gotSessionID != "explicit-session-1" {
		t.Fatalf("sessionID = %q, want explicit-session-1", gotSessionID)
	}
}

// TestResolveUpstreamSessionID 覆盖上游身份键派生的各分支。
func TestResolveUpstreamSessionID(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	setMode := func(mode string) {
		next := previousSettings
		next.RequestIsolationMode = mode
		ApplyRuntimeSettings(next)
	}

	// 1. WS + 无显式会话：恒为 ""（交给 stateless 路径），与隔离模式无关。
	setMode(RequestIsolationModeIsolated)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit isolated: got %q, want empty", got)
	}
	setMode(RequestIsolationModePerAPIKey)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit per-api-key: got %q, want empty", got)
	}

	// 2. HTTP + 无显式会话 + isolated：每请求唯一（两次不同、均非空）。
	setMode(RequestIsolationModeIsolated)
	a := resolveUpstreamSessionID(7, "sess-abc", "", false)
	b := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if a == "" || a == b {
		t.Fatalf("HTTP isolated: got %q / %q, want distinct non-empty", a, b)
	}

	// 3. HTTP + 无显式会话 + per-api-key：确定性（两次一致，= IsolateCodexSessionID）。
	setMode(RequestIsolationModePerAPIKey)
	c := resolveUpstreamSessionID(7, "sess-abc", "", false)
	d := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if c == "" || c != d {
		t.Fatalf("HTTP per-api-key: got %q / %q, want identical deterministic", c, d)
	}
	if want := IsolateCodexSessionID(7, "sess-abc"); c != want {
		t.Fatalf("HTTP per-api-key: got %q, want %q", c, want)
	}

	// 4. 显式会话：两模式都走确定性 IsolateCodexSessionID（不隔离用户主动声明的会话）。
	for _, mode := range []string{RequestIsolationModeIsolated, RequestIsolationModePerAPIKey} {
		setMode(mode)
		got := resolveUpstreamSessionID(7, "sess-xyz", "sess-xyz", false)
		if want := IsolateCodexSessionID(7, "sess-xyz"); got != want {
			t.Fatalf("explicit session mode=%s: got %q, want %q", mode, got, want)
		}
	}
}
