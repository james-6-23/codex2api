package admin

import (
	"context"
	"fmt"
	"github.com/codex2api/auth"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const intelligenceSwitchWire = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n"

// TestIntelligenceSourceSwitchesGrokUsesResponsesExecutor covers the default Grok Responses backend.
func TestIntelligenceSourceSwitchesGrokUsesResponsesExecutor(t *testing.T) {
	called := 0
	h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, intelligenceSwitchWire)
	})
	account.UpstreamType = auth.UpstreamGrok
	account.Models = []string{"grok-4.5"}
	got := h.executeIntelligenceCheck(context.Background(), account, "grok-4.5", "", "Return 21")
	if called != 1 || !got.Complete || got.Output != "21" || got.Error != "" {
		t.Fatalf("Grok execution: called=%d result=%+v", called, got)
	}
	if intelligenceAccountChannel(account) != "grok" {
		t.Fatal("wrong source")
	}
}

// TestIntelligenceSourceSwitchesGrokHTTPFailures reuses native auth isolation and free-quota accounting.
func TestIntelligenceSourceSwitchesGrokHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"unauthorized", `{"error":{"code":"invalid_token"}}`, "credential_refresh", http.StatusUnauthorized},
		{"free quota", `{"code":"subscription:free-usage-exhausted","error":"tokens (actual/limit): 42/40"}`, "usage_limited", http.StatusTooManyRequests},
		{"rate limited", `{"error":{"message":"too many requests"}}`, "rate_limited", http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			account.UpstreamType = auth.UpstreamGrok
			account.Models = []string{"grok-4.5"}
			account.PlanType = "free"
			if tc.status == http.StatusUnauthorized {
				account.APIKey, account.AccessToken = "", "expired-access"
				// Keep this transport fixture out of the store's async refresh lookup;
				// the shared mapper still takes its OAuth recovery branch.
				account.DBID = 0
			}
			started := time.Now()
			got := h.executeIntelligenceCheck(context.Background(), account, "grok-4.5", "", "Return 21")
			if got.Error == "" || got.Complete || got.Interrupted || account.GetCooldownReason() != tc.reason || account.IsBanned() {
				t.Fatalf("result=%+v reason=%s banned=%v", got, account.GetCooldownReason(), account.IsBanned())
			}
			if tc.name == "free quota" {
				snapshot, ok := account.GetGrokFreeQuotaSnapshot()
				if !ok || snapshot.UsedTokens != 42 || snapshot.LimitTokens != 40 || snapshot.Model != "grok-4.5" {
					t.Fatalf("lost free quota snapshot: %+v %v", snapshot, ok)
				}
			}
			if tc.name == "rate limited" && account.CooldownUtil.Before(started.Add(119*time.Second)) {
				t.Fatalf("Retry-After ignored: %v", account.CooldownUtil)
			}
		})
	}
}

// TestIntelligenceSourceSwitchesAntigravityUsesDedicatedExecutor keeps Antigravity Claude on its source adapter.
func TestIntelligenceSourceSwitchesAntigravityUsesDedicatedExecutor(t *testing.T) {
	h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) { t.Error("generic relay must not be called") })
	account.UpstreamType = auth.UpstreamAntigravity
	account.Models = []string{"gemini-3.8-flash-medium", "claude-sonnet-5-5-low"}
	calls := 0
	h.antigravityCapabilityProbe = func(ctx context.Context, a *auth.Account, model string, body []byte, stream bool, egress string) (*http.Response, error) {
		calls++
		if a != account || !stream || model != "claude-sonnet-5-5-low" || !strings.Contains(string(body), "Return 21") {
			t.Errorf("wrong dedicated probe: model=%s body=%s", model, body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(intelligenceSwitchWire))}, nil
	}
	result := h.executeIntelligenceCheck(context.Background(), account, "claude-sonnet-5-5-low", "", "Return 21")
	if calls != 1 || !result.Complete || result.Output != "21" || result.Error != "" {
		t.Fatalf("Antigravity execution: calls=%d result=%+v", calls, result)
	}
	if intelligenceAccountChannel(account) != "antigravity" {
		t.Fatal("Antigravity Claude misclassified as independent Claude")
	}
}

// TestIntelligenceSourceSwitchesGrokCatalogRouting honors a model-specific URL and Chat backend.
func TestIntelligenceSourceSwitchesGrokCatalogRouting(t *testing.T) {
	h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("account base URL must not override the model catalog")
		http.Error(w, "wrong backend", http.StatusNotFound)
	})
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"grok-4.5\",\"choices\":[{\"delta\":{\"content\":\"21\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	account.UpstreamType = auth.UpstreamGrok
	account.Models = []string{"grok-4.5"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.5", BaseURL: backend.URL + "/v1", APIBackend: auth.GrokProtocolChatCompletions}}})
	got := h.executeIntelligenceCheck(context.Background(), account, "grok-4.5", "", "Return 21")
	if calls != 1 || !got.Complete || got.Output != "21" || got.Error != "" {
		t.Fatalf("catalog execution: calls=%d result=%+v", calls, got)
	}
}

// TestIntelligenceSourceSwitchesAntigravityFailures separates rejected credentials from model quota.
func TestIntelligenceSourceSwitchesAntigravityFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, account, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) { t.Error("generic executor called") })
			account.UpstreamType = auth.UpstreamAntigravity
			account.APIKey = "" // OAuth without refresh metadata makes the refresh fail locally.
			account.AccessToken = "expired-token"
			account.Models = []string{"gemini-3.8-flash-tiered"}
			calls := 0
			h.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"probe rejected"}}`))}, nil
			}
			got := h.executeIntelligenceCheck(context.Background(), account, "gemini-3.8-flash-medium", "", "Return 21")
			if calls != 1 || got.CredentialRejected != (status == http.StatusUnauthorized) {
				t.Fatalf("calls=%d result=%+v", calls, got)
			}
			if account.IsBanned() || account.HasActiveCooldown() {
				t.Fatal("Antigravity must not inherit a Codex ban/global quota cooldown")
			}
			if status == http.StatusUnauthorized && account.Status != auth.StatusError {
				t.Fatalf("final 401 not recorded: %v", account.Status)
			}
			if status == http.StatusTooManyRequests && account.Status != auth.StatusReady {
				t.Fatalf("429 changed account state: %v", account.Status)
			}
		})
	}
}
