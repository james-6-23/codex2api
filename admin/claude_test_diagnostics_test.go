package admin

import (
	"context"
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

func claudeDiagnosticFrame(value string) string { return "data: " + value + "\n\n" }

func TestClaudeTestDiagnosticsMergeCumulativeUsage(t *testing.T) {
	body := claudeDiagnosticFrame(`{"type":"message_start","message":{"id":"msg_test","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":0,"output_tokens":1,"cache_read_input_tokens":120,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}}}}`) +
		claudeDiagnosticFrame(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"working"}}`) +
		claudeDiagnosticFrame(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"pong"}}`) +
		claudeDiagnosticFrame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`) +
		claudeDiagnosticFrame(`{"type":"message_delta","usage":{"output_tokens":6}}`) +
		claudeDiagnosticFrame(`{"type":"message_stop"}`)
	headers := make(http.Header)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("request-id", "req_test")
	headers.Set("anthropic-organization-id", "org_test")
	resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
	r := newClaudeTestRecorder(resp, "claude-haiku-4-5", "force", "", time.Now().Add(-10*time.Millisecond))
	var text strings.Builder
	status, _ := readClaudeMessagesStreamObserved(context.Background(), resp, func(value string) { text.WriteString(value) }, r.observe)
	d := r.finish()
	if status != "success" || text.String() != "pong" || d.Model != "claude-haiku-4-5" || d.ResponseModel != "claude-haiku-4-5-20251001" {
		t.Fatalf("unexpected test result: %s, %q, %+v", status, text.String(), d)
	}
	if d.RequestID != "req_test" || d.OrganizationID != "org_test" || d.MessageID != "msg_test" || d.StopReason != "end_turn" || d.FingerprintMode != "force" {
		t.Fatalf("missing response metadata: %+v", d)
	}
	if d.Usage == nil || d.Usage.InputTokens == nil || *d.Usage.InputTokens != 0 || *d.Usage.OutputTokens != 6 || *d.Usage.CacheReadTokens != 120 || *d.Usage.CacheCreationTokens != 30 {
		t.Fatalf("usage must merge cumulative snapshots without summing: %+v", d.Usage)
	}
	if d.Usage.CacheCreation == nil || *d.Usage.CacheCreation.FiveMinute != 10 || *d.Usage.CacheCreation.OneHour != 20 {
		t.Fatalf("cache creation breakdown lost: %+v", d.Usage.CacheCreation)
	}
	if d.FirstContentMS == nil || d.HeadersMS == nil || d.DurationMS == nil || *d.FirstContentMS < *d.HeadersMS || *d.DurationMS < *d.FirstContentMS {
		t.Fatalf("invalid timing order: %+v", d)
	}
	if d.ResponseBody != body || d.BodyTruncated {
		t.Fatal("short SSE response should be preserved")
	}
}

func TestClaudeTestDiagnosticsJSONAndMissingUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantUsage  bool
	}{
		{"missing", `{"type":"message","model":"claude-haiku-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`, false},
		{"zero", `{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":0,"output_tokens":2,"cache_creation_input_tokens":null}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}
			r := newClaudeTestRecorder(resp, "claude-haiku-4-5", "preserve", "", time.Now())
			status, _ := readClaudeMessagesStreamObserved(context.Background(), resp, nil, r.observe)
			d := r.finish()
			if status != "success" || (d.Usage != nil) != tc.wantUsage {
				t.Fatalf("unexpected JSON result: %s %+v", status, d)
			}
			if tc.wantUsage && (d.Usage.InputTokens == nil || *d.Usage.InputTokens != 0 || d.Usage.CacheCreationTokens != nil) {
				t.Fatalf("unknown fields must stay absent, zero must remain zero: %+v", d.Usage)
			}
			if !json.Valid([]byte(d.ResponseBody)) {
				t.Fatalf("JSON preview is not JSON: %q", d.ResponseBody)
			}
		})
	}
}

func TestClaudeTestDiagnosticsBoundedCaptureDoesNotTruncateParser(t *testing.T) {
	longText := strings.Repeat("x", claudeTestBodyLimit*2)
	frame, _ := json.Marshal(map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": longText}})
	body := claudeDiagnosticFrame(string(frame)) + claudeDiagnosticFrame(`{"type":"message_delta","usage":{"output_tokens":900},"delta":{"stop_reason":"max_tokens"}}`) + claudeDiagnosticFrame(`{"type":"message_stop"}`)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	r := newClaudeTestRecorder(resp, "claude-test", "preserve", "", time.Now())
	var received int
	status, _ := readClaudeMessagesStreamObserved(context.Background(), resp, func(text string) { received += len(text) }, r.observe)
	d := r.finish()
	if status != "success" || received != len(longText) || !d.BodyTruncated || len(d.ResponseBody) > claudeTestBodyLimit || *d.Usage.OutputTokens != 900 || d.StopReason != "max_tokens" {
		t.Fatalf("capture must not consume/truncate the parser stream: %s, received=%d, preview=%d, truncated=%v", status, received, len(d.ResponseBody), d.BodyTruncated)
	}
}

func TestClaudeTestDiagnosticsFilterHeadersAndRedactBody(t *testing.T) {
	const secret = "claude-access-token-to-hide"
	headers := make(http.Header)
	headers.Set("request-id", "req_safe")
	headers.Set("anthropic-organization-id", "12345678-1234-1234-1234-123456789012")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0")
	headers.Set("Authorization", "Bearer "+secret)
	headers.Set("Set-Cookie", "session=private")
	headers.Set("X-Admin-Key", "private-key")
	headers.Set("anthropic-custom-secret", "private-value")
	body := `{"type":"error","error":{"type":"authentication_error","message":"` + secret + `"},"refresh_token":"private-refresh"}`
	resp := &http.Response{StatusCode: 401, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
	r := newClaudeTestRecorder(resp, "claude-test", "preserve", secret, time.Now())
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.observe(data)
	d := r.finish()
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "private-refresh", "private-key", "session=private", "private-value"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("diagnostics leaked %q", forbidden)
		}
	}
	if len(d.ResponseHeaders) != 3 || d.OrganizationID != "12345678-1234-1234-1234-123456789012" || d.ErrorType != "authentication_error" {
		t.Fatalf("safe identifiers and quota headers should survive: %+v", d)
	}
	// Also redact a known token that straddles the preview boundary.
	r = newClaudeTestRecorder(nil, "claude-test", "preserve", secret, time.Now())
	_, _ = r.capture.Write([]byte(strings.Repeat("x", claudeTestBodyLimit-8) + secret + strings.Repeat("z", 100)))
	d = r.finish()
	if strings.Contains(d.ResponseBody, secret[:8]) || !d.BodyTruncated {
		t.Fatal("token prefix leaked at preview boundary")
	}
}

func TestClaudeTestHandlerEmitsDiagnosticsForSuccessAndFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		terminal string
	}{
		{"success", 200, `{"type":"message","id":"msg_1","model":"claude-haiku-4-5","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":2,"output_tokens":1}}`, "test_complete"},
		{"rejected", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "error"},
		{"stream_error", 200, claudeDiagnosticFrame(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/1/test", nil)
			h := &Handler{store: auth.NewStore(nil, nil, nil)}
			account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamClaude, AccessToken: "test-secret"}
			headers := make(http.Header)
			headers.Set("request-id", "req_handler")
			if strings.HasPrefix(tc.body, "data:") {
				headers.Set("Content-Type", "text/event-stream")
			}
			resp := &http.Response{StatusCode: tc.status, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body))}
			outcome := ""
			h.handleClaudeConnectionTest(c, account, resp, "claude-haiku-4-5", time.Now(), "force", true, false, &outcome, 1)
			var events []testEvent
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event testEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				events = append(events, event)
			}
			if len(events) < 3 || events[len(events)-2].Type != tc.terminal || events[len(events)-1].Type != "diagnostics" {
				t.Fatalf("terminal result must be followed by final diagnostics: %+v", events)
			}
			d := events[len(events)-1].Diagnostics
			if d == nil || d.HTTPStatus != tc.status || d.DurationMS == nil || d.RequestID != "req_handler" || d.ResponseBody == "" {
				t.Fatalf("incomplete final diagnostics: %+v", d)
			}
		})
	}
}
