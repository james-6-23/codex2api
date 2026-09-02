package auth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudeClientDecisionStatusAndMessage(t *testing.T) {
	decision, err := ValidateClaudeClientRequest(ClaudeClientPolicy{VersionPolicy: ClaudeVersionPolicyMinimum, ClientVersion: "2.1.251"}, "claude-cli/2.1.200 (external, cli)", "claude-sonnet-4-5")
	if err != nil || decision.Allowed {
		t.Fatalf("expected denial, got %+v err=%v", decision, err)
	}
	if decision.HTTPStatus() != 426 {
		t.Fatalf("too-old version must map to 426, got %d", decision.HTTPStatus())
	}
	if msg := decision.DetailMessage(); !strings.Contains(msg, "detected 2.1.200") || !strings.Contains(msg, "required 2.1.251") {
		t.Fatalf("detail message = %q", msg)
	}
	decision, err = ValidateClaudeClientRequest(ClaudeClientPolicy{Platform: ClaudeClientPlatformCLIOnly}, "Mozilla/5.0", "claude-sonnet-4-5")
	if err != nil || decision.Allowed || decision.HTTPStatus() != 400 {
		t.Fatalf("platform denial must map to 400, got %+v err=%v", decision, err)
	}
}

// An account override that no longer normalizes (e.g. minimum with a cleared
// version) must keep the valid global policy instead of failing open.
func TestClaudeClientPolicyForAccountFallsBackToGlobal(t *testing.T) {
	s := &Store{}
	s.SetClaudeClientPolicy(ClaudeClientPolicy{Platform: ClaudeClientPlatformCLIOnly})
	account := &Account{ClaudeVersionPolicyOverride: string(ClaudeVersionPolicyMinimum)}
	got := s.ClaudeClientPolicyForAccount(account)
	if got.Platform != ClaudeClientPlatformCLIOnly || got.VersionPolicy != ClaudeVersionPolicyPassthrough {
		t.Fatalf("invalid override must fall back to global policy, got %+v", got)
	}
	account = &Account{ClaudeClientPlatformOverride: string(ClaudeClientPlatformAny)}
	if got := s.ClaudeClientPolicyForAccount(account); got.Platform != ClaudeClientPlatformAny {
		t.Fatalf("valid override must win over global, got %+v", got)
	}
	s.SetClaudeClientPolicy(ClaudeClientPolicy{VersionPolicy: ClaudeVersionPolicyMinimum})
	if got := s.ClaudeClientPolicy(); got.VersionPolicy != ClaudeVersionPolicyPassthrough {
		t.Fatalf("invalid global policy must normalize to default, got %+v", got)
	}
}

func TestClaudeUsageWindowOmitsZeroResetAt(t *testing.T) {
	raw, err := json.Marshal([]ClaudeUsageWindow{
		{Name: "7d_fable", Utilization: 12.5, ModelScoped: true, ModelFamily: "fable"},
		{Name: "5h", Utilization: 1, ResetAt: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "0001-01-01") {
		t.Fatalf("zero reset_at must be omitted: %s", raw)
	}
	if !strings.Contains(string(raw), `"reset_at":"2026-09-08T00:00:00Z"`) {
		t.Fatalf("real reset_at must round-trip: %s", raw)
	}
	var back []ClaudeUsageWindow
	if err := json.Unmarshal(raw, &back); err != nil || len(back) != 2 || !back[0].ResetAt.IsZero() || back[1].ResetAt.IsZero() {
		t.Fatalf("round trip failed: %+v err=%v", back, err)
	}
}
