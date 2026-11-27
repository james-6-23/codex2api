package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenCodeProtocolRoutingDefaultsAndCustomRules(t *testing.T) {
	// Go mode defaults
	goCases := []struct {
		model   string
		inbound string
		want    string
	}{
		{"grok-4.7", OpenCodeProtocolChatCompletions, OpenCodeProtocolResponses},
		{"gpt-5.6-luna", OpenCodeProtocolAnthropic, OpenCodeProtocolResponses},
		{"muse-spark-1.3-contributor", OpenCodeProtocolChatCompletions, OpenCodeProtocolResponses},
		{"minimax-m2.7", OpenCodeProtocolResponses, OpenCodeProtocolAnthropic},
		{"qwen3.8-max", OpenCodeProtocolChatCompletions, OpenCodeProtocolAnthropic},
		{"glm-5.3", OpenCodeProtocolResponses, OpenCodeProtocolChatCompletions},
		{"opencode-go/glm-5.3", OpenCodeProtocolChatCompletions, OpenCodeProtocolChatCompletions},
		{"opencode/minimax-m3", OpenCodeProtocolChatCompletions, OpenCodeProtocolAnthropic},
	}
	for _, tc := range goCases {
		if got := ResolveOpenCodeProtocol(tc.model, tc.inbound, OpenCodeModeGo, nil, false); got != tc.want {
			t.Errorf("Go route(%q, inbound=%q) = %q, want %q", tc.model, tc.inbound, got, tc.want)
		}
	}

	// Zen mode defaults
	zenCases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-6", OpenCodeProtocolAnthropic},
		{"qwen3.8-max", OpenCodeProtocolChatCompletions},
		{"qwen3.7-max", OpenCodeProtocolAnthropic},
		{"gpt-5.6-luna", OpenCodeProtocolResponses},
		{"deepseek-v4-pro", OpenCodeProtocolChatCompletions},
	}
	for _, tc := range zenCases {
		if got := ResolveOpenCodeProtocol(tc.model, "", OpenCodeModeZen, nil, false); got != tc.want {
			t.Errorf("Zen route(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}

	// Custom rules override built-in rules and support multi-protocol inbound passthrough
	custom, err := ParseOpenCodeProtocolRules([]OpenCodeProtocolRule{
		{Pattern: "claude-*", Protocol: OpenCodeProtocolAnthropic, Protocols: []string{OpenCodeProtocolAnthropic, OpenCodeProtocolChatCompletions}},
		{Pattern: "custom-*", Protocol: OpenCodeProtocolResponses},
	})
	if err != nil {
		t.Fatalf("ParseOpenCodeProtocolRules failed: %v", err)
	}
	// Multi-protocol rule allows same-protocol passthrough for chat_completions
	if got := ResolveOpenCodeProtocol("claude-sonnet-4-6", OpenCodeProtocolChatCompletions, OpenCodeModeZen, custom, true); got != OpenCodeProtocolChatCompletions {
		t.Fatalf("expected inbound passthrough chat_completions, got %q", got)
	}
	// Unlisted inbound falls back to preferred protocol
	if got := ResolveOpenCodeProtocol("claude-sonnet-4-6", OpenCodeProtocolResponses, OpenCodeModeZen, custom, true); got != OpenCodeProtocolAnthropic {
		t.Fatalf("expected preferred anthropic, got %q", got)
	}
	// Unmatched model when custom rules are configured does NOT fall back to built-in rules
	if got := ResolveOpenCodeProtocol("grok-4.7", OpenCodeProtocolResponses, OpenCodeModeGo, custom, true); got != OpenCodeProtocolChatCompletions {
		t.Fatalf("unmatched model under custom rules should fall back to chat_completions, got %q", got)
	}
}

func TestOpenCodeProtocolRuleValidation(t *testing.T) {
	if _, err := ParseOpenCodeProtocolRules([]OpenCodeProtocolRule{{Pattern: "a*b*", Protocol: OpenCodeProtocolResponses}}); err == nil {
		t.Fatal("expected error for multiple wildcards")
	}
	if _, err := ParseOpenCodeProtocolRules([]OpenCodeProtocolRule{{Pattern: "foo*", Protocol: "invalid"}}); err == nil {
		t.Fatal("expected error for invalid protocol")
	}
	if _, err := ParseOpenCodeProtocolRules([]OpenCodeProtocolRule{{Pattern: "foo bar", Protocol: OpenCodeProtocolResponses}}); err == nil {
		t.Fatal("expected error for whitespace in pattern")
	}
}

func TestOpenCodeUnsupportedModelsAndUserAgent(t *testing.T) {
	for _, m := range []string{"gemini-3.8-flash", "opencode/gemini-3.7-pro", "jev-1.13", "opencode-go/jev-1.13-free"} {
		if !IsOpenCodeUnsupportedModel(m) {
			t.Errorf("expected %q to be unsupported", m)
		}
	}
	for _, m := range []string{"glm-5.3", "grok-4.7", "claude-sonnet-4-6", "qwen3.8-max"} {
		if IsOpenCodeUnsupportedModel(m) {
			t.Errorf("expected %q to be supported", m)
		}
	}
	t.Setenv(OpenCodeUserAgentVersionEnv, "2.3.4")
	if got := OpenCodeUserAgent(); got != "opencode/2.3.4" {
		t.Fatalf("OpenCodeUserAgent() = %q, want opencode/2.3.4", got)
	}
	t.Setenv(OpenCodeUserAgentVersionEnv, "invalid-ver")
	if got := OpenCodeUserAgent(); got != "opencode/"+DefaultOpenCodeUserAgentVersion {
		t.Fatalf("invalid env should fall back to default UA, got %q", got)
	}
}

func TestOpenCodeFetchModelsAndUsage(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/zen/go/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"opencode-go/glm-5.3"},{"id":"gemini-3.8-flash"},{"id":"jev-1.13"},{"id":"grok-4.7"}]}`))
		case "/zen/go/v1/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":42.5,"resetsAt":"2026-12-01T10:00:00Z"},"weekly":{"status":"ok","percent":15.0,"resetsAt":"2026-12-07T00:00:00Z"},"monthly":{"status":"ok","percent":5.0,"resetsAt":"2027-01-01T00:00:00Z"}}}`))
		case "/redirect/v1/usage":
			http.Redirect(w, r, "https://evil.example/leak", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	models, err := FetchOpenCodeModels(context.Background(), srv.URL+"/zen/go/v1", "sk-oc-test", "", nil)
	if err != nil {
		t.Fatalf("FetchOpenCodeModels error: %v", err)
	}
	if gotAuth != "Bearer sk-oc-test" || gotUA != OpenCodeUserAgent() {
		t.Fatalf("unexpected headers: auth=%q ua=%q", gotAuth, gotUA)
	}
	if len(models) != 2 || models[0] != "glm-5.3" || models[1] != "grok-4.7" {
		t.Fatalf("unexpected filtered models: %#v", models)
	}

	snap, err := FetchOpenCodeGoUsage(context.Background(), srv.URL+"/zen/go/v1", "sk-oc-test", "", nil)
	if err != nil {
		t.Fatalf("FetchOpenCodeGoUsage error: %v", err)
	}
	if snap.Status != OpenCodeUsageStatusOK || snap.Rolling == nil || snap.Rolling.Percent != 42.5 {
		t.Fatalf("unexpected usage snapshot: %+v", snap)
	}

	// Redirect must be blocked so credentials cannot leak
	if _, err := FetchOpenCodeGoUsage(context.Background(), srv.URL+"/redirect/v1", "sk-oc-test", "", nil); err == nil {
		t.Fatal("expected redirect to be rejected")
	}

	// Scheduling projection picks max window percent
	acc := &Account{
		UpstreamType: UpstreamOpenCode,
		APIKey:       "sk-oc-test",
		OpenCode: OpenCodeCredential{
			Mode:  OpenCodeModeGo,
			Usage: snap,
		},
	}
	if pct := acc.openCodeUsagePercentForSchedulingLocked(time.Now()); pct != 42.5 {
		t.Fatalf("scheduling percent = %v, want 42.5", pct)
	}
}

func TestOpenCodeGoUsageAutoRefreshDue(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	if OpenCodeGoUsageAutoRefreshDue(nil, time.Time{}, now) {
		t.Fatal("never-used account should not be due")
	}
	if !OpenCodeGoUsageAutoRefreshDue(nil, now.Add(-2*time.Minute), now) {
		t.Fatal("active account without snapshot should be due")
	}
	fresh := &OpenCodeUsageSnapshot{
		Status:        OpenCodeUsageStatusOK,
		FetchedAt:     now.Add(-2 * time.Minute),
		LastAttemptAt: now.Add(-2 * time.Minute),
	}
	if OpenCodeGoUsageAutoRefreshDue(fresh, now.Add(-time.Minute), now) {
		t.Fatal("account within 5m min floor must not refresh")
	}
	stale := &OpenCodeUsageSnapshot{
		Status:        OpenCodeUsageStatusOK,
		FetchedAt:     now.Add(-20 * time.Minute),
		LastAttemptAt: now.Add(-20 * time.Minute),
	}
	if OpenCodeGoUsageAutoRefreshDue(stale, now.Add(-25*time.Minute), now) {
		t.Fatal("account with no activity after fetch must not refresh")
	}
	if !OpenCodeGoUsageAutoRefreshDue(stale, now.Add(-2*time.Minute), now) {
		t.Fatal("active account past max-wait should be due")
	}
}
