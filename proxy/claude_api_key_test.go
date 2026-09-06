package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const claudeAPIKeyTestBody = `{"model":"claude-sonnet-4-5","max_tokens":64,"system":"Custom system","service_tier":"auto","metadata":{"user_id":"customer"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"history","signature":"third-party-signature"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}]}`
const claudeAPIKeyTestResponse = `{"id":"msg_api","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"call_2","name":"lookup","input":{"city":"London"}}],"stop_reason":"tool_use","usage":{"input_tokens":8,"output_tokens":5}}`
const claudeAPIKeyTestStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_api","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":8,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_2","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"London\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestClaudeAPIKeyOutboundPreservesNativeBodyAndHeaders(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for i, base := range []string{"https://example.com", "https://example.com/v1/", "https://example.com/gateway/v1"} {
		t.Run(base, func(t *testing.T) {
			// No custom headers and no identity mode: the historical neutral
			// contract must stay byte-for-byte unchanged (issue #647 default).
			account := &auth.Account{DBID: int64(964510 + i), UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "upstream-key", ClaudeBaseURL: base}
			installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil || !bytes.Equal(body, []byte(claudeAPIKeyTestBody)) {
					t.Fatalf("API key body was rewritten: %s %v", body, err)
				}
				wantURL, _ := auth.ClaudeAPIEndpoint(base, "messages")
				if req.URL.String() != wantURL || req.URL.RawQuery != "" {
					t.Fatalf("URL=%s", req.URL)
				}
				if req.Header.Get("x-api-key") != "upstream-key" || req.Header.Get("Authorization") != "" || req.UserAgent() != "my-sdk/1" || req.Header.Get("anthropic-version") != "2023-06-01" || req.Header.Get("anthropic-beta") != "custom-feature-2026" {
					t.Fatalf("invalid headers: %v", req.Header)
				}
				for _, name := range []string{"x-app", "X-Claude-Code-Session-Id", "X-Stainless-Lang", "X-Stainless-Os", "X-Stainless-Retry-Count", "anthropic-dangerous-direct-browser-access"} {
					if req.Header.Get(name) != "" {
						t.Errorf("OAuth header leaked: %s", name)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(claudeAPIKeyTestResponse))}, nil
			})
			headers := http.Header{}
			headers.Set("User-Agent", "my-sdk/1")
			headers.Set("X-Stainless-Os", "Linux")
			headers.Set("Authorization", "Bearer downstream-key")
			headers.Set("x-api-key", "downstream-key")
			headers.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219,custom-feature-2026")
			policy := auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformCLIOnly, VersionPolicy: auth.ClaudeVersionPolicyMinimum, ClientVersion: "99.0.0"}
			// "force" here is the OAuth global default a caller may resolve; an
			// API Key account without its own mode must still stay neutral.
			resp, err := ExecuteClaudeMessagesRequestWithPolicy(context.Background(), account, []byte(claudeAPIKeyTestBody), "", headers, "force", policy)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}
	request := httptest.NewRequest(http.MethodPost, "https://example.com", nil)
	applyClaudeAPIKeyHeaders(request, "key", nil, true, nil, "")
	if request.UserAgent() != "Codex2API" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("anthropic-beta") != "" {
		t.Fatalf("invalid neutral headers: %v", request.Header)
	}
}

// TestClaudeAPIKeyAccountCustomHeadersApply covers issue #647 item 1: account
// custom_headers reach the upstream and win over the neutral defaults, while the
// gateway-owned reserved headers can never be overridden.
func TestClaudeAPIKeyAccountCustomHeadersApply(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	account := &auth.Account{DBID: 964530, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "upstream-key", ClaudeBaseURL: "https://example.com", CustomHeaders: map[string]string{
		"User-Agent":        "gateway-client/2.0",
		"x-app":             "cli",
		"X-Gateway-Tenant":  "team-a",
		"anthropic-version": "2024-01-01",
		// Reserved: must be ignored even when a legacy row contains them.
		"x-api-key":     "evil",
		"Authorization": "Bearer evil",
		"Accept":        "text/plain",
		"Content-Type":  "text/plain",
	}}
	calls := 0
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.UserAgent() != "gateway-client/2.0" || req.Header.Get("X-App") != "cli" || req.Header.Get("X-Gateway-Tenant") != "team-a" || req.Header.Get("anthropic-version") != "2024-01-01" {
			t.Fatalf("custom headers not applied: %v", req.Header)
		}
		if req.Header.Get("x-api-key") != "upstream-key" || req.Header.Get("Authorization") != "" || req.Header.Get("Accept") != "application/json" || req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("reserved header overridden: %v", req.Header)
		}
		if req.Header.Get("X-Stainless-Lang") != "" {
			t.Fatalf("identity emulation must stay off without a mode: %v", req.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(claudeAPIKeyTestResponse))}, nil
	})
	headers := http.Header{}
	headers.Set("User-Agent", "my-sdk/1")
	resp, err := ExecuteClaudeMessagesRequest(context.Background(), account, []byte(claudeAPIKeyTestBody), "", headers, "")
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	_ = resp.Body.Close()
}

// TestClaudeAPIKeyClientIdentityEmulation covers issue #647 item 2: the
// optional Claude Code client identity for API Key accounts (preserve/force),
// layered under custom headers and never copying OAuth session state.
func TestClaudeAPIKeyClientIdentityEmulation(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	cliUA := "claude-cli/" + auth.EffectiveClaudeCLIVersion() + " (external, cli)"
	cases := []struct {
		name     string
		mode     string
		custom   map[string]string
		incoming map[string]string
		wantUA   string
		wantOS   string
		wantApp  string
		wantFill bool
	}{
		{name: "force_non_cli", mode: "force", incoming: map[string]string{"User-Agent": "opencode/1.2", "X-Stainless-Os": "Windows"}, wantUA: cliUA, wantOS: "MacOS", wantApp: "cli", wantFill: true},
		{name: "force_real_cli_overridden", mode: "force", incoming: map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-Os": "Windows"}, wantUA: cliUA, wantOS: "MacOS", wantApp: "cli", wantFill: true},
		{name: "preserve_real_cli_kept", mode: "preserve", incoming: map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-Os": "Windows"}, wantUA: "claude-cli/1.0.0 (external, cli)", wantOS: "Windows", wantApp: "cli", wantFill: true},
		{name: "preserve_non_cli_acts_as_force", mode: "preserve", incoming: map[string]string{"User-Agent": "opencode/1.2", "X-Stainless-Os": "Windows"}, wantUA: cliUA, wantOS: "MacOS", wantApp: "cli", wantFill: true},
		{name: "custom_headers_win_over_identity", mode: "force", custom: map[string]string{"User-Agent": "gateway-client/2.0", "X-Stainless-OS": "Linux"}, incoming: map[string]string{"User-Agent": "opencode/1.2"}, wantUA: "gateway-client/2.0", wantOS: "Linux", wantApp: "cli", wantFill: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &auth.Account{DBID: int64(964540 + i), UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "upstream-key", ClaudeBaseURL: "https://example.com", ClaudeFingerprintMode: tc.mode, CustomHeaders: tc.custom}
			calls := 0
			installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
				calls++
				if body, _ := io.ReadAll(req.Body); !bytes.Equal(body, []byte(claudeAPIKeyTestBody)) {
					t.Fatalf("identity emulation must not touch the body: %s", body)
				}
				if req.UserAgent() != tc.wantUA || req.Header.Get("X-Stainless-Os") != tc.wantOS || req.Header.Get("X-App") != tc.wantApp {
					t.Fatalf("identity headers: %v", req.Header)
				}
				if tc.wantFill {
					for name, want := range map[string]string{"X-Stainless-Lang": "js", "X-Stainless-Runtime": "node", "X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "600", "anthropic-dangerous-direct-browser-access": "true"} {
						if got := req.Header.Get(name); got != want {
							t.Fatalf("%s=%q want %q", name, got, want)
						}
					}
				}
				if req.Header.Get("x-api-key") != "upstream-key" || req.Header.Get("Authorization") != "" || req.Header.Get("X-Claude-Code-Session-Id") != "" || strings.Contains(req.Header.Get("anthropic-beta"), "oauth") {
					t.Fatalf("OAuth state leaked into API key request: %v", req.Header)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(claudeAPIKeyTestResponse))}, nil
			})
			headers := http.Header{}
			for name, value := range tc.incoming {
				headers.Set(name, value)
			}
			headers.Set("anthropic-beta", "oauth-2025-04-20")
			// Pass the neutral caller value: the mode must come from the account.
			resp, err := ExecuteClaudeMessagesRequest(context.Background(), account, []byte(claudeAPIKeyTestBody), "", headers, "")
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			_ = resp.Body.Close()
		})
	}
	// The recorded upstream User-Agent must be the final one.
	ctx := withUserAgentAudit(context.Background())
	request := httptest.NewRequest(http.MethodPost, "https://example.com", nil).WithContext(ctx)
	applyClaudeAPIKeyHeaders(request, "key", http.Header{"User-Agent": {"opencode/1.2"}}, false, map[string]string{"User-Agent": "final/1"}, "force")
	if got, known := upstreamUserAgentAudit(ctx); !known || got != "final/1" {
		t.Fatalf("audit recorded %q (known=%t)", got, known)
	}
}

func TestClaudeAPIKeyMessagesHandlerAndUsage(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-api-key.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			id, err := db.InsertAccountWithUpstream(context.Background(), "API", "anthropic", "claude", map[string]interface{}{"upstream_type": "claude", "claude_auth_kind": "api_key", "access_token": "key", "claude_base_url": "https://example.com"}, "")
			if err != nil {
				t.Fatal(err)
			}
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
			t.Cleanup(store.Stop)
			store.SetClaudeSecurityConfig(auth.ClaudeSecurityConfig{MaxOutputTokens: 1})
			store.SetClaudeClientPolicy(auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformCLIOnly})
			account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "key", ClaudeBaseURL: "https://example.com", Status: auth.StatusReady}
			store.AddAccount(account)
			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			body := strings.TrimSuffix(claudeAPIKeyTestBody, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
			calls := 0
			installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
				calls++
				sent, _ := io.ReadAll(req.Body)
				if string(sent) != body {
					t.Fatalf("handler applied OAuth normalization: %s", sent)
				}
				payload, contentType := claudeAPIKeyTestResponse, "application/json"
				if stream {
					payload, contentType = claudeAPIKeyTestStream, "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			handler.Messages(c)
			if recorder.Code != http.StatusOK || calls != 1 || !strings.Contains(recorder.Body.String(), "tool_use") || !strings.Contains(recorder.Body.String(), "call_2") {
				t.Fatalf("handler=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
			}
			db.FlushUsageLogs()
			usage, err := db.GetAccountTimeRangeUsage(context.Background(), time.Now().Add(-time.Minute))
			if err != nil || usage[id] == nil || usage[id].Tokens == 0 {
				t.Fatalf("usage log not recorded: %+v %v", usage[id], err)
			}
		})
	}
}

func TestClaudeAPIKeyHTTPFailuresUseSharedCooldowns(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, status := range []int{401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: int64(964520 + status), UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, ClaudeBaseURL: "https://example.com", AccessToken: "key", Status: auth.StatusReady}
			store.AddAccount(account)
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			calls := 0
			installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"api_error","message":"upstream rejected request"}}`))}, nil
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(claudeAPIKeyTestBody))
			handler.Messages(c)
			if calls != 1 || recorder.Code == http.StatusOK {
				t.Fatalf("failure path calls=%d status=%d", calls, recorder.Code)
			}
			if status == 401 || status == 429 {
				if !account.HasActiveCooldown() {
					t.Fatalf("status %d did not cool down API key account", status)
				}
			}
		})
	}
}
