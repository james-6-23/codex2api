package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const antigravityTransportTestModel = "gemini-3.6-flash-low"

// antigravityTransportUpstream records the single Cloud Code envelope the
// gateway sent, so a transport test can assert both the forwarded request and
// the translated downstream response.
type antigravityTransportUpstream struct {
	mu       sync.Mutex
	envelope map[string]any
}

func (u *antigravityTransportUpstream) request() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.envelope == nil {
		return nil
	}
	request, _ := u.envelope["request"].(map[string]any)
	return request
}

func newAntigravityTransportTestHandler(t *testing.T, body string) (*Handler, *antigravityTransportUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorded := &antigravityTransportUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope map[string]any
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("decode upstream envelope: %v", err)
		}
		recorded.mu.Lock()
		recorded.envelope = envelope
		recorded.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+body+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, MaxRetries: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:                 595,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               auth.AntigravityDefaultModelIDs(),
	})
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), recorded
}

func invokeAntigravityTransport(t *testing.T, handler *Handler, path, body string, dispatch func(*Handler, *gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	dispatch(handler, ctx)
	return recorder
}

func antigravityForwardedToolNames(t *testing.T, request map[string]any) []string {
	t.Helper()
	if request == nil {
		t.Fatal("upstream received no request envelope")
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("function declarations were not forwarded upstream: %#v", request)
	}
	declarations, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		name, _ := declaration.(map[string]any)["name"].(string)
		names = append(names, name)
	}
	return names
}

// Antigravity used to be filtered out of /v1/chat/completions entirely, so
// every advertised Gemini model answered 503 no_available_account for clients
// that speak Chat (issue #595).
func TestChatCompletionsRoutesAntigravityToolCall(t *testing.T) {
	handler, upstream := newAntigravityTransportTestHandler(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"query":"value"},"id":"call_1"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`)

	recorder := invokeAntigravityTransport(t, handler, "/v1/chat/completions", `{
		"model":"`+antigravityTransportTestModel+`",
		"messages":[{"role":"user","content":"look it up"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`, (*Handler).ChatCompletions)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, body)
	}
	if names := antigravityForwardedToolNames(t, upstream.request()); len(names) != 1 || names[0] != "lookup" {
		t.Fatalf("forwarded declarations = %#v", names)
	}
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"lookup"`) {
		t.Fatalf("tool call was not translated into the Chat response: %q", body)
	}
}

func TestChatCompletionsStreamsAntigravityToolCall(t *testing.T) {
	handler, upstream := newAntigravityTransportTestHandler(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"query":"value"},"id":"call_1"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`)

	recorder := invokeAntigravityTransport(t, handler, "/v1/chat/completions", `{
		"model":"`+antigravityTransportTestModel+`",
		"stream":true,
		"messages":[{"role":"user","content":"look it up"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}]
	}`, (*Handler).ChatCompletions)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, body)
	}
	if names := antigravityForwardedToolNames(t, upstream.request()); len(names) != 1 || names[0] != "lookup" {
		t.Fatalf("forwarded declarations = %#v", names)
	}
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"lookup"`) {
		t.Fatalf("streamed tool call missing: %q", body)
	}
	if got := strings.Count(body, "data: [DONE]\n\n"); got != 1 {
		t.Fatalf("stream [DONE] count = %d, want 1; body=%q", got, body)
	}
}

func TestMessagesRoutesAntigravityToolCall(t *testing.T) {
	handler, upstream := newAntigravityTransportTestHandler(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"query":"value"},"id":"call_1"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`)

	recorder := invokeAntigravityTransport(t, handler, "/v1/messages", `{
		"model":"`+antigravityTransportTestModel+`",
		"max_tokens":256,
		"messages":[{"role":"user","content":"look it up"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}]
	}`, (*Handler).Messages)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", recorder.Code, body)
	}
	if names := antigravityForwardedToolNames(t, upstream.request()); len(names) != 1 || names[0] != "lookup" {
		t.Fatalf("forwarded declarations = %#v", names)
	}
	if !strings.Contains(body, `"tool_use"`) || !strings.Contains(body, `"lookup"`) {
		t.Fatalf("tool call was not translated into the Messages response: %q", body)
	}
}
