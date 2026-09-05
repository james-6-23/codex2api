package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestClaudeSessionIdentityPrecedenceAndIsolation(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"body-session\"}"},"prompt_cache_key":"cache-session"}`)
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "header-session")
	headers.Set(downstreamAffinityHeader, "local-only")
	identity := resolveClaudeRequestSessionIdentity(headers, body)
	if identity.explicitUpstreamID != "header-session" || identity.affinityID != resolveDownstreamAffinityID(headers) {
		t.Fatalf("Claude identity lost explicit priority or local affinity: %+v", identity)
	}
	if headers.Get("Session-Id") != "" || ResolveExplicitSessionID(headers, body) != "cache-session" {
		t.Fatal("Claude session parsing must not mutate ingress or change other provider paths")
	}
	first := resolveUpstreamSessionID(41, identity.upstreamSeed, identity.explicitUpstreamID, false)
	if first != resolveUpstreamSessionID(41, identity.upstreamSeed, identity.explicitUpstreamID, false) ||
		first == resolveUpstreamSessionID(42, identity.upstreamSeed, identity.explicitUpstreamID, false) || first == "header-session" {
		t.Fatal("explicit sessions must remain stable within a key and isolated across keys")
	}
	headers.Del("X-Claude-Code-Session-Id")
	if got := resolveClaudeRequestSessionIdentity(headers, body).explicitUpstreamID; got != "body-session" {
		t.Fatalf("metadata fallback = %q", got)
	}
	if got := resolveClaudeRequestSessionIdentity(nil, []byte(`{"metadata":{"user_id":"business-user"},"prompt_cache_key":"cache-session"}`)).explicitUpstreamID; got != "cache-session" {
		t.Fatalf("business metadata must preserve generic fallback, got %q", got)
	}
}

func TestClaudeMetadataPreservesBusinessIDsAndDistinctParents(t *testing.T) {
	for _, userID := range []string{`business-user`, `{"business_id":123}`, `{"session_id":"child","parent_session_id":"parent"}`} {
		body, err := json.Marshal(map[string]any{"metadata": map[string]string{"user_id": userID}})
		if err != nil {
			t.Fatal(err)
		}
		out := injectClaudeMetadataUserID(body, claudeBodyIdentity{sessionID: "mapped-session"})
		got := gjson.GetBytes(out, "metadata.user_id").String()
		if userID == `{"session_id":"child","parent_session_id":"parent"}` {
			if gjson.Get(got, "parent_session_id").String() != "parent" || gjson.Get(got, "session_id").String() != "mapped-session" {
				t.Fatalf("distinct parent relationship changed: %s", got)
			}
		} else if got != userID {
			t.Fatalf("business user_id changed: %s -> %s", userID, got)
		}
	}
}

func TestClaudeSystemPreludeNormalizesOrderWithoutLosingFields(t *testing.T) {
	billing := `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.258.abc; cc_entrypoint=sdk; custom=keep;","cache_control":{"type":"ephemeral","ttl":"1h"},"extra":true}`
	preamble := `{"type":"text","text":"` + claudeCodeSystemPreamble + `","cache_control":{"type":"ephemeral"}}`
	user := `{"type":"text","text":"Keep the user's instructions.","extra":123}`
	for name, system := range map[string]string{
		"billing only":        `[` + billing + `,` + user + `]`,
		"out of order":        `[` + user + `,` + preamble + `,` + billing + `]`,
		"already canonical":   `[` + billing + `,` + preamble + `,` + user + `]`,
		"string billing":      gjson.Get(billing, "text").Raw,
		"string declaration":  gjson.Get(preamble, "text").Raw,
		"ordinary discussion": `[{"type":"text","text":"Explain cc_version=2.1.258 and x-anthropic-billing-header:"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			out := injectClaudeCodeSystemPrompt([]byte(`{"system":` + system + `,"messages":[]}`))
			if !json.Valid(out) {
				t.Fatalf("invalid JSON: %s", out)
			}
			blocks := gjson.GetBytes(out, "system").Array()
			if len(blocks) < 2 || !strings.HasPrefix(blocks[0].Get("text").String(), claudeBillingHeaderPrefix) || blocks[1].Get("text").String() != claudeCodeSystemPreamble {
				t.Fatalf("invalid prelude order: %s", out)
			}
			if repeated := injectClaudeCodeSystemPrompt(out); !bytes.Equal(repeated, out) {
				t.Fatalf("normalization is not idempotent: %s -> %s", out, repeated)
			}
			if name == "billing only" || name == "out of order" || name == "already canonical" {
				if blocks[0].Raw != billing || blocks[2].Raw != user {
					t.Fatalf("original block fields lost: %s", out)
				}
				aligned := alignClaudeBillingBlock(out, "claude-cli/2.1.261 (external, cli)")
				if got := gjson.GetBytes(aligned, "system.0.text").String(); got != "x-anthropic-billing-header: cc_version=2.1.261.abc; cc_entrypoint=sdk; custom=keep;" {
					t.Fatalf("unexpected billing rewrite: %q", got)
				}
				if gjson.GetBytes(aligned, "system.0.cache_control.ttl").String() != "1h" || !gjson.GetBytes(aligned, "system.0.extra").Bool() {
					t.Fatal("alignment discarded original block attributes")
				}
			}
			if name == "ordinary discussion" && len(blocks) != 3 {
				t.Fatal("ordinary user text was mistaken for a billing block")
			}
		})
	}
}

type claudeBoundaryRoundTripper func(*http.Request) (*http.Response, error)

func (f claudeBoundaryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func installClaudeBoundaryTransport(t *testing.T, account *auth.Account, transport claudeBoundaryRoundTripper) {
	t.Helper()
	key := clientPoolKey(account, "", codexTransportModeFromEnv())
	entry := &poolEntry{client: &http.Client{Transport: transport}}
	previous, existed := clientPool.Load(key)
	clientPool.Store(key, entry)
	t.Cleanup(func() {
		if existed {
			clientPool.Store(key, previous)
		} else {
			clientPool.Delete(key)
		}
	})
}

func TestClaudeOutboundIdentityMatchesAcrossSelectedAccounts(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	const session = "11111111-2222-7333-8444-555555555555"
	ctx := WithClaudeSessionID(context.Background(), session)
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"metadata":{"trace":"keep","user_id":"{\"device_id\":\"old-device\",\"account_uuid\":\"old-account\",\"session_id\":\"old-session\",\"parent_session_id\":\"old-session\",\"extra\":9007199254740993}"}}`)
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"conflicting-session"}}
	for _, accountID := range []int64{913401, 913402} {
		account := &auth.Account{DBID: accountID, UpstreamType: auth.UpstreamClaude, AccessToken: "test-token", AccountID: "account-for-test"}
		if accountID == 913402 {
			account.AccountID = "second-account"
		}
		installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
			sent, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			identity := gjson.Parse(gjson.GetBytes(sent, "metadata.user_id").String())
			if req.Header.Get("X-Claude-Code-Session-Id") != session || identity.Get("session_id").String() != session || identity.Get("parent_session_id").String() != session ||
				identity.Get("account_uuid").String() != account.AccountID || identity.Get("device_id").String() != account.ClaudeDeviceID() {
				t.Fatalf("outbound identities diverged: %s", identity.Raw)
			}
			if identity.Get("extra").Raw != "9007199254740993" || gjson.GetBytes(sent, "metadata.trace").String() != "keep" {
				t.Fatal("unrelated metadata was lost")
			}
			if req.Header.Get(auth.ClaudeDeviceIDCredentialKey) != "" {
				t.Fatal("device metadata leaked into HTTP headers")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})
		resp, err := ExecuteClaudeMessagesRequest(ctx, account, body, "", headers, auth.ClaudeFingerprintModePreserve)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}

func TestClaudeHandlerSessionStaysStableAcrossFailover(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1}
	store := auth.NewStore(nil, nil, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	runtime := previous
	runtime.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(runtime)
	var sessionIDs []string
	for _, accountID := range []int64{913411, 913412} {
		account := &auth.Account{DBID: accountID, UpstreamType: auth.UpstreamClaude, AccessToken: "test-token", AccountID: "test-account", Status: auth.StatusReady, Models: []string{"claude-sonnet-4-6"}}
		store.AddAccount(account)
		installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
			sent, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			sessionIDs = append(sessionIDs, req.Header.Get("X-Claude-Code-Session-Id"))
			if gjson.Get(gjson.GetBytes(sent, "metadata.user_id").String(), "session_id").String() != sessionIDs[len(sessionIDs)-1] {
				t.Fatal("handler emitted inconsistent identity")
			}
			status := http.StatusOK
			response := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
			if len(sessionIDs) == 1 {
				status = http.StatusServiceUnavailable
				response = `{"type":"error","error":{"type":"overloaded_error","message":"try another account"}}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
		})
	}
	invoke := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
		handler.Messages(ctx)
		return recorder
	}
	recorder := invoke()
	if recorder.Code != 200 || len(sessionIDs) != 2 || sessionIDs[0] == "" || sessionIDs[0] != sessionIDs[1] {
		t.Fatalf("failover changed session: status=%d sessions=%v body=%s", recorder.Code, sessionIDs, recorder.Body.String())
	}
	if recorder := invoke(); recorder.Code != 200 || len(sessionIDs) != 3 || sessionIDs[2] == sessionIDs[0] {
		t.Fatalf("independent request must remain isolated: status=%d sessions=%v", recorder.Code, sessionIDs)
	}
}
