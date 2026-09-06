package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestAntigravityFlash38DiscoveryAndAccountIsolation(t *testing.T) {
	want := []string{"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high"}
	account := &auth.Account{DBID: 3801, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", Models: []string{"gemini-3.8-flash-tiered"}}
	if got := antigravityPublicModelsForAccount(account); !reflect.DeepEqual(got, want) {
		t.Fatalf("3.8 catalog = %v, want %v", got, want)
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(account)
	handler := &Handler{store: store}
	listed := handler.supportedModelIDs(context.Background())
	old := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "old-token", Models: []string{"gemini-3.7-flash-tiered"}}
	for _, model := range want {
		if !strings.Contains("|"+strings.Join(listed, "|")+"|", "|"+model+"|") {
			t.Fatalf("model discovery omitted %q: %v", model, listed)
		}
		if !antigravityChannelAccountFilter(model)(account) || antigravityChannelAccountFilter(model)(old) {
			t.Fatalf("account-specific admission lost for %s", model)
		}
	}
	if antigravityChannelAccountFilter("gemini-3.8-flash-tiered")(account) {
		t.Fatal("raw wire ID leaked into the public route")
	}
	if got := antigravityGeminiResolvedModel("gemini-3.7-flash-medium", nil); got != "gemini-3.7-flash-tiered" {
		t.Fatalf("old model silently upgraded: %s", got)
	}
	if antigravityChannelAccountFilter("gemini-3.8-flash-medium")(&auth.Account{AccessToken: "codex-token"}) {
		t.Fatal("Antigravity request admitted a Codex account")
	}
	t.Setenv(auth.AntigravityExperimentalInteractionsEnv, "")
	if antigravityChannelAccountFilter("gemini-3.8-flash-medium")(&auth.Account{UpstreamType: auth.UpstreamAntigravity, APIKey: "test-key", Models: account.Models}) {
		t.Fatal("model upgrade enabled experimental Interactions")
	}
}

func TestAntigravityFlash38WireAndThinkingLevels(t *testing.T) {
	for _, tc := range []struct{ model, effort, level string }{
		{"gemini-3.8-flash-low", "high", "LOW"},
		{"gemini-3.8-flash-medium", "low", "MEDIUM"},
		{"gemini-3.8-flash-high", "low", "HIGH"},
		{"gemini-3.8-flash", "", "LOW"},
		{"gemini-3.8-flash", "low", "LOW"},
		{"gemini-3.8-flash", "high", "HIGH"},
		{"gemini-3.8-flash", "minimal", "LOW"},
		{"gemini-3.8-flash-tiered", "high", "HIGH"},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			body := `{"input":"hello"}`
			if tc.effort != "" {
				body = `{"input":"hello","reasoning":{"effort":"` + tc.effort + `"}}`
			}
			payload, err := responsesToGeminiInternal([]byte(body), "project", tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if payload["model"] != "gemini-3.8-flash-tiered" {
				t.Fatalf("wrong wire model: %v", payload["model"])
			}
			cfg, ok := payload["request"].(map[string]any)["generationConfig"].(map[string]any)
			if !ok {
				t.Fatal("missing generationConfig")
			}
			thinking, ok := cfg["thinkingConfig"].(map[string]any)
			if !ok || thinking["thinkingLevel"] != tc.level {
				t.Fatalf("thinking = %v, want %s", thinking, tc.level)
			}
			if _, exists := thinking["thinkingBudget"]; exists {
				t.Fatal("3.8 inherited an unverified numeric thinking budget")
			}
			if antigravityGeminiMaxOutputTokens(payload["model"].(string)) != 65536 {
				t.Fatal("3.8 output capacity is stale")
			}
		})
	}
}

func TestAntigravityFlash38ExecutorJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var received map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Error(err)
			}
			if stream {
				if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" {
					t.Errorf("wrong SSE route: %v", r.URL)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7}}}\n\n")
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`)
			}
		}))
		previous := antigravityOAuthEndpointBases
		antigravityOAuthEndpointBases = []string{server.URL}
		account := &auth.Account{DBID: 3802, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}}
		resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.8-flash-high", []byte(`{"input":"hello"}`), stream, "")
		antigravityOAuthEndpointBases = previous
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if received["model"] != "gemini-3.8-flash-tiered" {
			t.Fatalf("actual HTTP wire = %v", received["model"])
		}
		if !strings.Contains(string(body), "gemini-3.8-flash-high") || !strings.Contains(string(body), "ok") {
			t.Fatalf("downstream response lost public identity or content: %s", body)
		}
		if stream && !strings.Contains(string(body), "response.completed") {
			t.Fatalf("stream did not complete: %s", body)
		}
	}
}
