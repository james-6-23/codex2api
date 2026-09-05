package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestClaudeDeviceIDAcceptsCanonicalAndHistoricalMetadataKeys(t *testing.T) {
	for _, key := range []string{ClaudeDeviceIDCredentialKey, "Claude_device_id", "CLAUDE_DEVICE_ID"} {
		t.Run(key, func(t *testing.T) {
			account := &Account{AccountID: "account-one", CustomHeaders: map[string]string{key: "  explicit-device  "}}
			if got := account.ClaudeDeviceID(); got != "explicit-device" {
				t.Fatalf("device ID = %q, want configured value", got)
			}
		})
	}
	for _, name := range ClaudeIdentityHeaderNames {
		if strings.EqualFold(name, ClaudeDeviceIDCredentialKey) {
			t.Fatal("device ID metadata must not become an outbound identity header")
		}
	}
}

func TestClaudeDeviceIDFallbackSurvivesReloadAndIgnoresEmptyOverride(t *testing.T) {
	first := (&Account{DBID: 1, AccountID: "account-one"}).ClaudeDeviceID()
	reloaded := (&Account{DBID: 2, AccountID: "account-one", CustomHeaders: map[string]string{"Claude_device_id": " "}}).ClaudeDeviceID()
	if first != reloaded {
		t.Fatal("same account identity must retain its fallback across reloads")
	}
	if decoded, err := hex.DecodeString(first); err != nil || len(decoded) != 32 {
		t.Fatalf("fallback must be a 64-character hex identity, got %q", first)
	}
	if first == (&Account{DBID: 1, AccountID: "account-two"}).ClaudeDeviceID() {
		t.Fatal("different account identities must not share fallback devices")
	}
	if (&Account{DBID: 1}).ClaudeDeviceID() == (&Account{DBID: 2}).ClaudeDeviceID() {
		t.Fatal("accounts without an upstream ID must remain separated by database ID")
	}
}
