package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func decodeCodexTestEvents(t *testing.T, body string) []testEvent {
	t.Helper()
	var events []testEvent
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event testEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestCodexTestRecorderCapturesWindowsIdentityAndHeaderWhitelist(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "12.5")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "1800")
	headers.Set("x-codex-secondary-used-percent", "40")
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-reset-after-seconds", "86400")
	headers.Set("x-codex-plan-type", "plus")
	headers.Set("x-request-id", "req_abc")
	headers.Set("cf-ray", "ray-1")
	headers.Set("x-ratelimit-remaining-requests", "99")
	headers.Set("openai-processing-ms", "321")
	headers.Set("retry-after", "3")
	headers.Set("set-cookie", "session=secret")
	headers.Set("authorization", "Bearer leaked")
	headers.Set("x-random", "no")
	resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(""))}

	r := newCodexTestRecorder(resp, "gpt-5.4", &auth.Account{AccessToken: "tok"}, time.Now())
	d := r.details
	if d.HTTPStatus != 200 || d.HeadersMS == nil || d.RequestID != "req_abc" || d.CFRay != "ray-1" || d.PlanType != "plus" {
		t.Fatalf("identity fields not captured: %+v", d)
	}
	if d.PrimaryWindow == nil || d.PrimaryWindow.UsedPercent == nil || *d.PrimaryWindow.UsedPercent != 12.5 ||
		d.PrimaryWindow.WindowMinutes == nil || *d.PrimaryWindow.WindowMinutes != 300 ||
		d.PrimaryWindow.ResetAfterSeconds == nil || *d.PrimaryWindow.ResetAfterSeconds != 1800 {
		t.Fatalf("primary window not parsed: %+v", d.PrimaryWindow)
	}
	if d.SecondaryWindow == nil || d.SecondaryWindow.WindowMinutes == nil || *d.SecondaryWindow.WindowMinutes != 10080 {
		t.Fatalf("secondary window not parsed: %+v", d.SecondaryWindow)
	}
	names := make(map[string]string)
	for _, header := range d.ResponseHeaders {
		names[header.Name] = header.Value
	}
	for _, want := range []string{"x-codex-primary-used-percent", "x-codex-plan-type", "x-request-id", "cf-ray", "x-ratelimit-remaining-requests", "openai-processing-ms", "retry-after"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("header %q missing from whitelist output: %+v", want, d.ResponseHeaders)
		}
	}
	for _, forbidden := range []string{"set-cookie", "authorization", "x-random"} {
		if _, ok := names[forbidden]; ok {
			t.Fatalf("header %q must not be exposed: %+v", forbidden, d.ResponseHeaders)
		}
	}
}

func TestCodexTestRecorderWithoutWindowHeadersLeavesWindowsAbsent(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
	r := newCodexTestRecorder(resp, "gpt-5.4", nil, time.Now())
	if r.details.PrimaryWindow != nil || r.details.SecondaryWindow != nil || r.details.PlanType != "" {
		t.Fatalf("absent headers must stay absent: %+v", r.details)
	}
	raw, _ := json.Marshal(r.details)
	for _, forbidden := range []string{"primary_window", "secondary_window", "usage", "duration_ms", "first_content_ms"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("unobserved field %q must be omitted: %s", forbidden, raw)
		}
	}
}

func TestCodexTestRecorderObservesResponsesEvents(t *testing.T) {
	r := newCodexTestRecorder(nil, "gpt-5.4", nil, time.Now())
	r.observe([]byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4-2026","status":"in_progress"}}`))
	if r.details.ResponseID != "resp_1" || r.details.ResponseModel != "gpt-5.4-2026" || r.details.ResponseStatus != "in_progress" {
		t.Fatalf("response.created not observed: %+v", r.details)
	}
	if r.details.FirstContentMS != nil {
		t.Fatal("first content must not be set before any text delta")
	}
	r.observe([]byte(`{"type":"response.output_text.delta","delta":"pong"}`))
	if r.details.FirstContentMS == nil {
		t.Fatal("text delta must stamp first content time")
	}
	r.observe([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}}`))
	u := r.details.Usage
	if u == nil || *u.InputTokens != 12 || *u.OutputTokens != 3 || *u.TotalTokens != 15 || *u.CachedTokens != 4 || *u.ReasoningTokens != 2 {
		t.Fatalf("usage not observed: %+v", u)
	}
	if r.details.ResponseStatus != "completed" {
		t.Fatalf("status = %q, want completed", r.details.ResponseStatus)
	}
}

func TestCodexTestRecorderObservesFailureShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		wantType string
		wantCode string
	}{
		{"status_details", `{"type":"response.failed","response":{"status":"failed","status_details":{"error":{"type":"usage_limit_reached"}}}}`, "usage_limit_reached", ""},
		{"response_error", `{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","code":"upstream_unavailable"}}}`, "server_error", "upstream_unavailable"},
		{"top_level_error_event", `{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`, "", "rate_limit_exceeded"},
		{"nested_error_event", `{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found"}}`, "invalid_request_error", "model_not_found"},
		{"plain_json_body", `{"error":{"message":"invalidated","code":"token_invalidated","type":"invalid_token"},"status":401}`, "invalid_token", "token_invalidated"},
		{"incomplete", `{"type":"response.completed","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodexTestRecorder(nil, "gpt-5.4", nil, time.Now())
			r.observe([]byte(tc.payload))
			if r.details.ErrorType != tc.wantType || r.details.ErrorCode != tc.wantCode {
				t.Fatalf("error type/code = %q/%q, want %q/%q", r.details.ErrorType, r.details.ErrorCode, tc.wantType, tc.wantCode)
			}
			if tc.name == "incomplete" && (r.details.ResponseStatus != "incomplete" || r.details.IncompleteReason != "max_output_tokens") {
				t.Fatalf("incomplete details not observed: %+v", r.details)
			}
		})
	}
}

func TestCodexTestRecorderKeepsHTTPStatusOutOfResponseStatus(t *testing.T) {
	r := newCodexTestRecorder(nil, "gpt-5.4", nil, time.Now())
	r.observe([]byte(`{"error":{"message":"invalidated","code":"token_invalidated"},"status":401,"id":"evt_1","model":7}`))
	r.observe([]byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error"}}`))
	if r.details.ResponseStatus != "" || r.details.ResponseID != "" || r.details.ResponseModel != "" {
		t.Fatalf("non-Responses objects must not populate lifecycle fields: %+v", r.details)
	}
	if r.details.ErrorCode != "token_invalidated" || r.details.ErrorType != "rate_limit_error" {
		t.Fatalf("error fields must still be observed: %+v", r.details)
	}
	r.observe([]byte(`{"object":"response","id":"resp_plain","model":"gpt-5.4","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`))
	if r.details.ResponseStatus != "completed" || r.details.ResponseID != "resp_plain" || r.details.Usage == nil || *r.details.Usage.OutputTokens != 2 {
		t.Fatalf("non-stream Responses object must be observed: %+v", r.details)
	}
}

func TestCodexTestRecorderIgnoresNonObjectFrames(t *testing.T) {
	r := newCodexTestRecorder(nil, "gpt-5.4", nil, time.Now())
	r.observe([]byte(`[DONE]`))
	r.observe([]byte(`"text"`))
	r.observe(nil)
	if r.details.ResponseID != "" || r.details.Usage != nil {
		t.Fatalf("non-object frames must be ignored: %+v", r.details)
	}
}

func TestCodexTestRecorderRedactsSecretsAndTruncates(t *testing.T) {
	token := "eyJhbGciOiJSUzI1NiJ9.super-secret-access-token"
	apiKey := "sk-relay-secret-key"
	body := `{"error":{"message":"token ` + token + ` key ` + apiKey + ` proxy http://user:pass@proxy.local:8080"}}`
	resp := &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	resp.Header.Set("x-request-id", "req-"+token)
	r := newCodexTestRecorder(resp, "gpt-5.4", &auth.Account{AccessToken: token, APIKey: apiKey}, time.Now())
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	d := r.finish()
	if strings.Contains(d.ResponseBody, token) || strings.Contains(d.ResponseBody, apiKey) || strings.Contains(d.ResponseBody, "user:pass@") {
		t.Fatalf("secrets leaked into body preview: %s", d.ResponseBody)
	}
	if !strings.Contains(d.ResponseBody, "[REDACTED]") || !strings.Contains(d.ResponseBody, "http://[REDACTED]@proxy.local:8080") {
		t.Fatalf("redaction markers missing: %s", d.ResponseBody)
	}
	if strings.Contains(d.RequestID, token) {
		t.Fatalf("secret leaked into header value: %s", d.RequestID)
	}
	if !strings.Contains(d.ResponseBody, "\n  \"error\"") {
		t.Fatalf("JSON body must be pretty printed: %s", d.ResponseBody)
	}
	if d.DurationMS == nil || d.BodyTruncated {
		t.Fatalf("unexpected finish state: %+v", d)
	}

	big := strings.Repeat("x", codexTestBodyLimit+1024)
	resp = &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(big))}
	r = newCodexTestRecorder(resp, "gpt-5.4", nil, time.Now())
	drained, err := io.ReadAll(resp.Body)
	if err != nil || len(drained) != len(big) {
		t.Fatalf("tee must never truncate the consumed stream: %d/%d %v", len(drained), len(big), err)
	}
	d = r.finish()
	if !d.BodyTruncated || len(d.ResponseBody) > codexTestBodyLimit {
		t.Fatalf("oversized body must be truncated with flag: truncated=%v len=%d", d.BodyTruncated, len(d.ResponseBody))
	}
}

func TestCodexTestSecretsCollectsTokenAndAPIKey(t *testing.T) {
	account := &auth.Account{AccessToken: " at-1 ", APIKey: "sk-2"}
	got := codexTestSecrets(account)
	if len(got) != 2 || got[0] != "at-1" || got[1] != "sk-2" {
		t.Fatalf("codexTestSecrets() = %v", got)
	}
	if got := codexTestSecrets(&auth.Account{}); len(got) != 0 {
		t.Fatalf("empty credentials must yield no secrets: %v", got)
	}
	if got := codexTestSecrets(nil); got != nil {
		t.Fatalf("nil account must yield nil: %v", got)
	}
}

func newCodexDiagnosticsTestHandler(t *testing.T, upstream http.HandlerFunc) (*Handler, *auth.Account, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "sk-test-secret",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	return &Handler{store: store}, account, server
}

func serveCodexDiagnosticsTest(handler *Handler) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/api/admin/accounts/:id/test", handler.TestConnection)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/accounts/42/test", nil))
	return recorder
}

func TestConnectionCodexEmitsDiagnosticsFramesAroundSuccess(t *testing.T) {
	handler, _, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req_ok")
		w.Header().Set("x-codex-plan-type", "plus")
		w.Header().Set("x-codex-primary-used-percent", "5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-after-seconds", "100")
		w.WriteHeader(http.StatusOK)
		// Echo the bearer secret back so the preview must redact it.
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"gpt-4o-mini\",\"auth\":\"" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"))
	})
	recorder := serveCodexDiagnosticsTest(handler)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	events := decodeCodexTestEvents(t, recorder.Body.String())
	if len(events) < 5 || events[0].Type != "test_start" || events[1].Type != "diagnostics" {
		t.Fatalf("expected test_start then initial diagnostics: %+v", events)
	}
	initial := events[1].CodexDiagnostics
	if initial == nil || initial.HTTPStatus != 200 || initial.DurationMS != nil || initial.RequestID != "req_ok" || initial.PlanType != "plus" || initial.PrimaryWindow == nil {
		t.Fatalf("initial diagnostics must carry headers without duration: %+v", initial)
	}
	if events[1].Diagnostics != nil {
		t.Fatal("Codex test must not populate the Claude diagnostics field")
	}
	terminal, final := events[len(events)-2], events[len(events)-1]
	if terminal.Type != "test_complete" || !terminal.Success || final.Type != "diagnostics" {
		t.Fatalf("terminal result must be followed by final diagnostics: %+v", events)
	}
	d := final.CodexDiagnostics
	if d == nil || d.DurationMS == nil || d.FirstContentMS == nil || d.ResponseID != "resp_ok" || d.ResponseModel != "gpt-4o-mini" || d.ResponseStatus != "completed" {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
	if d.Usage == nil || d.Usage.InputTokens == nil || *d.Usage.InputTokens != 7 || d.Usage.CachedTokens == nil || *d.Usage.CachedTokens != 0 {
		t.Fatalf("usage not carried: %+v", d.Usage)
	}
	if strings.Contains(d.ResponseBody, "sk-test-secret") || !strings.Contains(d.ResponseBody, "[REDACTED]") {
		t.Fatalf("API key leaked into body preview: %s", d.ResponseBody)
	}
	if !strings.Contains(d.ResponseBody, "response.output_text.delta") {
		t.Fatalf("body preview must include the streamed frames: %s", d.ResponseBody)
	}
	for _, event := range events {
		if event.Type == "content" && strings.Contains(event.Text, "耗时") {
			return
		}
	}
	t.Fatalf("existing duration trailer must be preserved: %+v", events)
}

func TestConnectionCodexNon200CarriesErrorDiagnostics(t *testing.T) {
	handler, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("cf-ray", "ray-401")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated.","code":"token_invalidated","type":"invalid_request_error"}}`))
	})
	events := decodeCodexTestEvents(t, serveCodexDiagnosticsTest(handler).Body.String())
	if len(events) < 4 || events[len(events)-2].Type != "error" || events[len(events)-1].Type != "diagnostics" {
		t.Fatalf("error must be followed by final diagnostics: %+v", events)
	}
	d := events[len(events)-1].CodexDiagnostics
	if d == nil || d.HTTPStatus != 401 || d.CFRay != "ray-401" || d.ErrorCode != "token_invalidated" || d.ErrorType != "invalid_request_error" || d.DurationMS == nil {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
	if !strings.Contains(d.ResponseBody, "token_invalidated") {
		t.Fatalf("body preview missing: %s", d.ResponseBody)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized (state handling must be unchanged)", got)
	}
}

func TestConnectionCodexStreamFailureRecordsErrorType(t *testing.T) {
	handler, _, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fail\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_fail\",\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"code\":\"upstream_unavailable\",\"message\":\"boom\"}}}\n\n"))
	})
	events := decodeCodexTestEvents(t, serveCodexDiagnosticsTest(handler).Body.String())
	if len(events) < 4 || events[len(events)-2].Type != "error" || events[len(events)-1].Type != "diagnostics" {
		t.Fatalf("error must be followed by final diagnostics: %+v", events)
	}
	d := events[len(events)-1].CodexDiagnostics
	if d == nil || d.ResponseID != "resp_fail" || d.ResponseStatus != "failed" || d.ErrorType != "server_error" || d.ErrorCode != "upstream_unavailable" || d.FirstContentMS != nil {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
}

func TestConnectionCodexRequestFailureCarriesDiagnostics(t *testing.T) {
	handler, account, server := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	server.Close()
	_ = account
	events := decodeCodexTestEvents(t, serveCodexDiagnosticsTest(handler).Body.String())
	if len(events) != 2 || events[0].Type != "test_start" || events[1].Type != "error" {
		t.Fatalf("request failure must yield test_start then error: %+v", events)
	}
	d := events[1].CodexDiagnostics
	if d == nil || d.DurationMS == nil || d.HTTPStatus != 0 || d.Model == "" {
		t.Fatalf("request failure must still attach duration diagnostics: %+v", d)
	}
	if !strings.HasPrefix(events[1].Error, "请求失败") {
		t.Fatalf("error text = %q", events[1].Error)
	}
}

func TestCodexTestRecorderDetectsWebsocketTransportAndMetadataFrames(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Upgrade", "websocket")
	headers.Set("Sec-WebSocket-Accept", "abc")
	headers.Set("Content-Type", "text/event-stream")
	resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(""))}
	r := newCodexTestRecorder(resp, "gpt-5.4", nil, time.Now())
	if r.details.Transport != "websocket" {
		t.Fatalf("transport = %q, want websocket", r.details.Transport)
	}
	r.observe([]byte(`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":33,"window_minutes":300,"resets_in_seconds":900},"secondary":{"used_percent":8,"window_minutes":10080}}}`))
	if r.details.PlanType != "pro" || r.details.PrimaryWindow == nil || *r.details.PrimaryWindow.UsedPercent != 33 || *r.details.PrimaryWindow.ResetAfterSeconds != 900 || r.details.SecondaryWindow == nil || *r.details.SecondaryWindow.WindowMinutes != 10080 {
		t.Fatalf("codex.rate_limits frame not observed: %+v primary=%+v secondary=%+v", r.details, r.details.PrimaryWindow, r.details.SecondaryWindow)
	}
	r.observe([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"turn-1","set-cookie":"nope","x-request-id":"req_ws"}}`))
	names := make(map[string]string)
	for _, header := range r.details.ResponseHeaders {
		names[header.Name] = header.Value
	}
	if names["x-codex-turn-state"] != "turn-1" || names["x-request-id"] != "req_ws" {
		t.Fatalf("metadata headers not merged: %+v", r.details.ResponseHeaders)
	}
	if _, leaked := names["set-cookie"]; leaked {
		t.Fatalf("metadata cookie must be filtered: %+v", r.details.ResponseHeaders)
	}
	if r.details.ResponseID != "" {
		t.Fatalf("metadata frames must not be mistaken for response objects: %+v", r.details)
	}

	plain := newCodexTestRecorder(&http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, "gpt-5.4", nil, time.Now())
	if plain.details.Transport != "http" {
		t.Fatalf("transport = %q, want http", plain.details.Transport)
	}
}

func TestCodexTestRecorderObservesSafetyBuffering(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-codex-safety-buffering-enabled", "true")
	headers.Set("x-codex-safety-buffering-faster-model", "gpt-5.6-luna")
	resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(""))}
	r := newCodexTestRecorder(resp, "gpt-6-astra", nil, time.Now())
	if r.details.SafetyBufferingEnabled == nil || !*r.details.SafetyBufferingEnabled || r.details.SafetyBufferingFasterModel != "gpt-5.6-luna" || r.details.SafetyBuffered {
		t.Fatalf("safety buffering headers not observed: %+v", r.details)
	}
	r.observe([]byte(`{"type":"response.output_text.delta","delta":"hi","safety_buffering":false}`))
	if r.details.SafetyBuffered {
		t.Fatal("safety_buffering=false must not mark the turn as buffered")
	}
	r.observe([]byte(`{"type":"response.output_text.delta","delta":"hi","safety_buffering":true}`))
	if !r.details.SafetyBuffered {
		t.Fatal("safety_buffering=true must mark the turn as buffered")
	}

	// WS 路径:头只在 codex.response.metadata 帧里出现。
	ws := newCodexTestRecorder(nil, "gpt-6-astra", nil, time.Now())
	ws.observe([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-safety-buffering-enabled":"false","x-codex-safety-buffering-faster-model":"gpt-5.6-luna"}}`))
	if ws.details.SafetyBufferingEnabled == nil || *ws.details.SafetyBufferingEnabled || ws.details.SafetyBufferingFasterModel != "gpt-5.6-luna" {
		t.Fatalf("metadata frame safety buffering not observed: %+v", ws.details)
	}

	none := newCodexTestRecorder(&http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, "gpt-6-astra", nil, time.Now())
	raw, _ := json.Marshal(none.details)
	if strings.Contains(string(raw), "safety_buffer") {
		t.Fatalf("absent safety buffering must be omitted: %s", raw)
	}
}

func TestCodexTestRecorderHonorsRequestIDHeaderOverride(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-request-id", "generic")
	headers.Set("x-custom-trace", "custom")
	resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(""))}
	account := &auth.Account{UpstreamRequestIDHeader: "X-Custom-Trace"}
	if got := newCodexTestRecorder(resp, "gpt-5.4", account, time.Now()).details.RequestID; got != "custom" {
		t.Fatalf("request id = %q, want override header value", got)
	}
	headers.Del("x-request-id")
	headers.Set("x-oai-request-id", "oai")
	if got := newCodexTestRecorder(resp, "gpt-5.4", nil, time.Now()).details.RequestID; got != "oai" {
		t.Fatalf("request id = %q, want x-oai-request-id fallback", got)
	}
}
