package auth

import (
	"bytes"
	"strings"

	"github.com/codex2api/database"
)

// ApplyModelCapabilities publishes only the runtime flags used by the transport.
// The full validated snapshot remains in the database for model-list fallback.
func (a *Account) ApplyModelCapabilities(snapshot database.ModelCapabilitySnapshot) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.DBID != snapshot.AccountID || a.CredentialGeneration != snapshot.CredentialGeneration {
		return
	}
	if a.codexCapabilityGeneration == snapshot.CredentialGeneration && a.codexCapabilityObservedAt > snapshot.ObservedAt {
		return
	}
	flags := make(map[string]bool)
	for model, fields := range snapshot.Models {
		if value := bytes.TrimSpace(fields["use_responses_lite"]); bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false")) {
			flags[strings.ToLower(model)] = bytes.Equal(value, []byte("true"))
		}
	}
	a.codexLiteSupport = flags
	a.codexCapabilityGeneration = snapshot.CredentialGeneration
	a.codexCapabilityObservedAt = snapshot.ObservedAt
}

func (a *Account) ModelSupportsResponsesLite(model string) (bool, bool) {
	if a == nil {
		return false, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.codexCapabilityGeneration != a.CredentialGeneration {
		return false, false
	}
	value, ok := a.codexLiteSupport[strings.ToLower(strings.TrimSpace(model))]
	return value, ok
}
