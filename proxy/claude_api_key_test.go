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
			account := &auth.Account{DBID: int64(964510 + i), UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "upstream-key", ClaudeBaseURL: base, CustomHeaders: map[string]string{"User-Agent": "claude-cli/1.0.0", "x-app": "cli"}}
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
				for _, name := range []string{"x-app", "X-Claude-Code-Session-Id", "X-Stainless-Lang", "anthropic-dangerous-direct-browser-access"} {
					if req.Header.Get(name) != "" {
						t.Errorf("OAuth header leaked: %s", name)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(claudeAPIKeyTestResponse))}, nil
			})
			headers := http.Header{}
			headers.Set("User-Agent", "my-sdk/1")
			headers.Set("Authorization", "Bearer downstream-key")
			headers.Set("x-api-key", "downstream-key")
			headers.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219,custom-feature-2026")
			policy := auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformCLIOnly, VersionPolicy: auth.ClaudeVersionPolicyMinimum, ClientVersion: "99.0.0"}
			resp, err := ExecuteClaudeMessagesRequestWithPolicy(context.Background(), account, []byte(claudeAPIKeyTestBody), "", headers, "force", policy)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}
	request := httptest.NewRequest(http.MethodPost, "https://example.com", nil)
	applyClaudeAPIKeyHeaders(request, "key", nil, true)
	if request.UserAgent() != "Codex2API" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("anthropic-beta") != "" {
		t.Fatalf("invalid neutral headers: %v", request.Header)
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
