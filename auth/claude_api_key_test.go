package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClaudeAPIKeyKindAndOAuthSkips(t *testing.T) {
	for _, raw := range []string{"api_key", " API_Key "} {
		if !IsValidClaudeAuthKind(raw) || NormalizeClaudeAuthKind(raw, true) != ClaudeAuthKindAPIKey || InferClaudeAuthKind(raw, "sk-ant-oat01-test", "rt") != ClaudeAuthKindAPIKey {
			t.Fatalf("explicit API key kind lost: %q", raw)
		}
	}
	if InferClaudeAuthKind("", "sk-ant-api03-key", "") != ClaudeAuthKindOAuth {
		t.Fatal("legacy credentials must not silently turn into API keys")
	}
	acc := &Account{DBID: 964501, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindAPIKey, AccessToken: "key", ExpiresAt: time.Now().Add(-time.Hour), CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.1"}, Status: StatusReady}
	if !acc.IsClaudeAPIKey() || acc.IsClaudeSetupToken() || !acc.IsRelayStyle() || acc.EffectiveClaudeAuthKind() != ClaudeAuthKindAPIKey {
		t.Fatal("API key must stay in the native Claude channel")
	}
	if (&Account{ClaudeAuthKind: ClaudeAuthKindAPIKey}).IsClaudeAPIKey() {
		t.Fatal("non-Claude account misclassified")
	}
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(acc)
	if err := store.refreshClaudeAccount(context.Background(), acc, true); err != nil {
		t.Fatalf("API key refresh must be a no-op: %v", err)
	}
	if acc.NeedsUsageProbe(time.Minute) {
		t.Fatal("API key must not schedule subscription usage probes")
	}
	persister := &recordingPersister{}
	if changed, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.300"); err != nil || changed != 0 || len(persister.calls) != 0 {
		t.Fatalf("API key fingerprint must not be generated/refreshed: %d %v", changed, err)
	}
	if _, _, changed := store.ApplyClaudePlanFromCreditsRequired(context.Background(), acc); changed {
		t.Fatal("API key must not infer a subscription plan")
	}
}

func TestClaudeAPIEndpoint(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://example.com", "https://example.com/v1/messages"},
		{"https://example.com/", "https://example.com/v1/messages"},
		{"http://127.0.0.1:1234/gateway///", "http://127.0.0.1:1234/gateway/v1/messages"},
		{"https://example.com/gateway/v1/", "https://example.com/gateway/v1/messages"},
	} {
		got, err := ClaudeAPIEndpoint(tc.base, "messages")
		if err != nil || got != tc.want {
			t.Fatalf("endpoint %q = %q, %v", tc.base, got, err)
		}
	}
	for _, invalid := range []string{"", " example.com", " https://example.com", "https://example.com/ ", "ftp://example.com", "https://", "https://example.com/a\tb", "https://user:pass@example.com", "https://example.com?key=x", "https://example.com#frag"} {
		if _, err := NormalizeClaudeBaseURL(invalid); err == nil {
			t.Fatalf("invalid base URL accepted: %q", invalid)
		}
	}
}

func TestClaudeAPIKeyModelDiscovery(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/gateway/v1/models" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-beta") != "" || r.Header.Get("anthropic-version") == "" || strings.Contains(r.UserAgent(), "claude-cli") {
			t.Errorf("invalid discovery request: %s %v", r.URL, r.Header)
		}
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a+cursor"}`))
		} else {
			if r.URL.Query().Get("after_id") != "claude-a+cursor" {
				t.Error("cursor was not URL encoded")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a"},{"id":"claude-b"}]}`))
		}
	}))
	t.Cleanup(server.Close)
	client := NewClaudeAuth("")
	models, err := client.FetchModelsWithCredentials(context.Background(), "test-key", ClaudeAuthKindAPIKey, server.URL+"/gateway/v1/")
	if err != nil || strings.Join(models, ",") != "claude-a,claude-b" || requests != 2 {
		t.Fatalf("models=%v requests=%d err=%v", models, requests, err)
	}
}

func TestClaudeAPIKeyModelDiscoveryDoesNotFollowRedirect(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Location", "/unexpected")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	_, err := NewClaudeAuth("").FetchModelsWithCredentials(context.Background(), "test-key", ClaudeAuthKindAPIKey, server.URL)
	if err == nil || requests != 1 {
		t.Fatalf("redirect must fail without forwarding credentials: requests=%d err=%v", requests, err)
	}
}
