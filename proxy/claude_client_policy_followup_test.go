package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// The allowed-model projection must be deterministic when official rows
// overlap (claude-opus-4 vs claude-opus-4-5, claude-fable-5 vs claude-fable-5-1).
func TestProjectClaudeOfficialPricingPrefersExactThenLongestPrefix(t *testing.T) {
	parsed := map[string]database.ModelPricingOverride{
		"claude-opus-4":    {Input: 15, CachedInput: 1.5, Output: 75},
		"claude-opus-4-5":  {Input: 5, CachedInput: 0.5, Output: 25},
		"claude-fable-5":   {Input: 10, CachedInput: 1, Output: 50},
		"claude-fable-5-1": {Input: 10, CachedInput: 0.25, Output: 50},
	}
	allowed := map[string]struct{}{
		"claude-opus-4-5-20250929":  {},
		"claude-opus-4-1-20250805":  {},
		"claude-fable-5-1":          {},
		"claude-fable-5-1-20260801": {},
		"claude-fable-5":            {},
		"claude-haiku-9":            {},
		"gpt-5.5":                   {},
	}
	for i := 0; i < 50; i++ {
		got := projectClaudeOfficialPricing(allowed, parsed)
		if got["claude-opus-4-5-20250929"].CachedInput != 0.5 {
			t.Fatalf("iteration %d: opus-4-5 resolved to %+v", i, got["claude-opus-4-5-20250929"])
		}
		if got["claude-opus-4-1-20250805"].Input != 15 {
			t.Fatalf("iteration %d: opus-4-1 resolved to %+v", i, got["claude-opus-4-1-20250805"])
		}
		if got["claude-fable-5-1"].CachedInput != 0.25 || got["claude-fable-5-1-20260801"].CachedInput != 0.25 {
			t.Fatalf("iteration %d: fable-5-1 resolved to %+v / %+v", i, got["claude-fable-5-1"], got["claude-fable-5-1-20260801"])
		}
		if got["claude-fable-5"].CachedInput != 1 {
			t.Fatalf("iteration %d: fable-5 resolved to %+v", i, got["claude-fable-5"])
		}
		if _, ok := got["claude-haiku-9"]; ok {
			t.Fatalf("iteration %d: unknown model must not receive a price", i)
		}
		if _, ok := got["gpt-5.5"]; ok {
			t.Fatalf("iteration %d: non-Claude model must be skipped", i)
		}
	}
	whole := projectClaudeOfficialPricing(nil, parsed)
	if len(whole) != len(parsed) {
		t.Fatalf("empty allowed set must import every official row, got %d", len(whole))
	}
}

// Words that only appear in echoed request content must not turn an ordinary
// invalid_request_error into a client-compatibility classification.
func TestIsClaudeClientCompatibilityErrorIgnoresBodyOutsideMessage(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content: field required"},"request_id":"claude code does not support this model version required"}`)
	if isClaudeClientCompatibilityError(http.StatusBadRequest, body) {
		t.Fatal("classification must only consider the structured message fields")
	}
	gate := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Claude Code version 2.1.200 does not support this model; version 2.1.251 or later is required."}}`)
	if !isClaudeClientCompatibilityError(http.StatusBadRequest, gate) {
		t.Fatal("real version gate must still be recognized")
	}
}

// Admin-originated probes go through the legacy entry point which never
// applies the downstream client policy, so a claude_code_cli_only global
// policy must not be able to reject a nil-header probe.
func TestAdminProbeEntryPointIgnoresClientPolicy(t *testing.T) {
	decision, err := auth.ValidateClaudeClientRequest(auth.ClaudeClientPolicy{}, "", "claude-sonnet-4-5")
	if err != nil || !decision.Allowed {
		t.Fatalf("legacy entry point must allow nil-header probes: decision=%+v err=%v", decision, err)
	}
	decision, err = auth.ValidateClaudeClientRequest(auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformCLIOnly}, "", "claude-sonnet-4-5")
	if err != nil || decision.Allowed || decision.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("cli_only must deny an empty UA on the client path: decision=%+v err=%v", decision, err)
	}
}
