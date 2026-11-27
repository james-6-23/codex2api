package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestOpenCodeProtocolRoutingAndHeaders(t *testing.T) {
	type capturedReq struct {
		path      string
		auth      string
		xAPIKey   string
		anthVer   string
		userAgent string
		session   string
		body      string
	}
	var last capturedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = capturedReq{
			path:      r.URL.Path,
			auth:      r.Header.Get("Authorization"),
			xAPIKey:   r.Header.Get("x-api-key"),
			anthVer:   r.Header.Get("anthropic-version"),
			userAgent: r.Header.Get("User-Agent"),
			session:   r.Header.Get("X-OpenCode-Session"),
			body:      string(b),
		}
		switch r.URL.Path {
		case "/zen/go/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		case "/zen/go/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`))
		case "/zen/go/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	acc := &auth.Account{
		DBID:         701,
		UpstreamType: auth.UpstreamOpenCode,
		APIKey:       "sk-oc-secret",
		BaseURL:      srv.URL + "/zen/go/v1",
		OpenCode: auth.OpenCodeCredential{
			Mode: auth.OpenCodeModeGo,
		},
		CustomHeaders: map[string]string{
			"X-Custom-Tag":  "canary",
			"Authorization": "Bearer evil-override-must-be-ignored",
		},
	}

	// 1. Same-protocol Chat Completions passthrough (glm-5.3 -> chat_completions)
	inHeaders := http.Header{"X-Session-Affinity": []string{"ses_opencode_123"}}
	resp, err := ExecuteOpenCodeProtocolRequest(
		context.Background(), acc, GrokProtocolChatCompletions,
		[]byte(`{"model":"opencode-go/glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		nil, "", inHeaders,
	)
	if err != nil {
		t.Fatalf("chat passthrough failed: %v", err)
	}
	_ = resp.Body.Close()
	if !isGrokNativeRouteResponse(resp) {
		t.Fatal("expected native passthrough marker for same-protocol chat request")
	}
	if last.path != "/zen/go/v1/chat/completions" || last.auth != "Bearer sk-oc-secret" || last.session != "ses_opencode_123" || last.userAgent != auth.OpenCodeUserAgent() {
		t.Fatalf("unexpected chat request: %+v", last)
	}
	if strings.Contains(last.body, "opencode-go/") {
		t.Fatalf("expected opencode-go/ prefix stripped from body, got %s", last.body)
	}

	// 2. Same-protocol Responses passthrough (grok-4.7 -> responses) with UUID fallback session
	resp, err = ExecuteOpenCodeProtocolRequest(
		context.Background(), acc, GrokProtocolResponses,
		[]byte(`{"model":"grok-4.7","input":"hi","stream":false}`),
		nil, "", nil,
	)
	if err != nil {
		t.Fatalf("responses passthrough failed: %v", err)
	}
	_ = resp.Body.Close()
	if last.path != "/zen/go/v1/responses" || last.auth != "Bearer sk-oc-secret" || last.session == "" {
		t.Fatalf("unexpected responses request: %+v", last)
	}

	// 3. Same-protocol Anthropic Messages passthrough (minimax-m2.7 -> anthropic, uses x-api-key)
	resp, err = ExecuteOpenCodeProtocolRequest(
		context.Background(), acc, GrokProtocolMessages,
		[]byte(`{"model":"minimax-m2.7","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"{\"session_id\":\"anth-sess-9\"}"}}`),
		nil, "", nil,
	)
	if err != nil {
		t.Fatalf("messages passthrough failed: %v", err)
	}
	_ = resp.Body.Close()
	if !isGrokNativeRouteResponse(resp) {
		t.Fatal("expected native passthrough marker for same-protocol messages request")
	}
	if last.path != "/zen/go/v1/messages" || last.xAPIKey != "sk-oc-secret" || last.auth != "" || last.anthVer != "2023-06-01" || last.session != "anth-sess-9" {
		t.Fatalf("unexpected messages request: %+v", last)
	}

	// 4. Unsupported model (gemini-* / jev-*) must be rejected locally with 400
	if _, err := ExecuteOpenCodeProtocolRequest(
		context.Background(), acc, GrokProtocolChatCompletions,
		[]byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`),
		nil, "", nil,
	); err == nil {
		t.Fatal("expected unsupported gemini model to return error")
	}
}

func TestOpenCodeZenChatToResponsesDropsTrailingChunksAfterDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		// Trailing non-spec Zen metadata after [DONE]
		_, _ = w.Write([]byte("data: {\"choices\":[],\"cost\":\"0\"}\n\n"))
	}))
	defer srv.Close()

	acc := &auth.Account{
		DBID:         702,
		UpstreamType: auth.UpstreamOpenCode,
		APIKey:       "sk-oc-zen",
		BaseURL:      srv.URL + "/zen/v1",
		OpenCode:     auth.OpenCodeCredential{Mode: auth.OpenCodeModeZen},
	}
	// Cross-protocol: Responses inbound -> Chat Completions upstream (deepseek-v4-flash) -> adapted back to Responses SSE
	resp, err := ExecuteOpenCodeProtocolRequest(
		context.Background(), acc, GrokProtocolResponses,
		[]byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`),
		[]byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`),
		"", nil,
	)
	if err != nil {
		t.Fatalf("cross-protocol request failed: %v", err)
	}
	defer resp.Body.Close()
	if isGrokNativeRouteResponse(resp) {
		t.Fatal("cross-protocol route should NOT be marked native")
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read adapted stream failed: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "response.completed") || !strings.Contains(text, "hello") {
		t.Fatalf("expected adapted response.completed with hello, got: %s", text)
	}
	if strings.Contains(text, `"cost":"0"`) || strings.Contains(text, "response.failed") {
		t.Fatalf("trailing post-[DONE] chunk leaked or failed stream: %s", text)
	}
}

func TestApplyOpenCodeCooldownSemantics(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	resetAt := time.Now().UTC().Add(3 * time.Hour)
	goAcc := &auth.Account{
		DBID:         801,
		UpstreamType: auth.UpstreamOpenCode,
		APIKey:       "sk-go",
		OpenCode: auth.OpenCodeCredential{
			Mode: auth.OpenCodeModeGo,
			Usage: &auth.OpenCodeUsageSnapshot{
				Status:    auth.OpenCodeUsageStatusOK,
				FetchedAt: time.Now().UTC(),
				Rolling:   &auth.OpenCodeUsageWindow{Status: "ok", Percent: 95, ResetsAt: resetAt},
			},
		},
	}

	// Cloudflare 403 -> short rate_limited cooldown (~2m), not banned/error
	d := applyOpenCodeCooldown(store, goAcc, http.StatusForbidden, []byte("<html>Cloudflare error code: 1010</html>"), nil, "glm-5.3")
	if d.Reason != "rate_limited" || d.Cooldown < time.Minute || d.Cooldown > 5*time.Minute {
		t.Fatalf("expected short rate_limited cooldown for CF 403, got %+v", d)
	}

	// Go 429 near exhausted (>=90%) -> cooldown until rolling resets_at (~3h)
	d = applyOpenCodeCooldown(store, goAcc, http.StatusTooManyRequests, []byte(`{"error":{"message":"Too many requests"}}`), nil, "glm-5.3")
	if d.Reason != "usage_limited" || d.Cooldown < 2*time.Hour {
		t.Fatalf("expected Go window cooldown (~3h) when rolling=95%%, got %+v", d)
	}

	// Go 429 pure frequency limit when usage < 90% -> short 1m cooldown
	goAcc.OpenCode.Usage.Rolling.Percent = 20
	d = applyOpenCodeCooldown(store, goAcc, http.StatusTooManyRequests, []byte(`{"error":{"message":"Rate limit exceeded"}}`), nil, "glm-5.3")
	if d.Reason != "rate_limited" || d.Cooldown != time.Minute {
		t.Fatalf("expected 1m frequency cooldown when rolling=20%%, got %+v", d)
	}

	// Zen insufficient balance -> recoverable 20m usage_limited cooldown
	zenAcc := &auth.Account{
		DBID:         802,
		UpstreamType: auth.UpstreamOpenCode,
		APIKey:       "sk-zen",
		OpenCode:     auth.OpenCodeCredential{Mode: auth.OpenCodeModeZen},
	}
	d = applyOpenCodeCooldown(store, zenAcc, http.StatusTooManyRequests, []byte(`{"error":{"message":"Insufficient balance"}}`), nil, "claude-sonnet-4-6")
	if d.Reason != "usage_limited" || d.Cooldown != 20*time.Minute {
		t.Fatalf("expected 20m recoverable cooldown for Zen insufficient balance, got %+v", d)
	}
}
