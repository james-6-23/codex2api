package admin

import (
	"context"
	"fmt"
	"github.com/codex2api/auth"
	"io"
	"net/http"
	"strings"
	"testing"
)

const intelligenceSwitchWire = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n"

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
