package auth

import (
	"context"
	"strings"
	"time"

	"github.com/codex2api/database"
)

func grokFactString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func grokFactBool(payload map[string]any, keys ...string) (*bool, bool) {
	for _, key := range keys {
		if value, exists := payload[key]; exists {
			if typed, ok := value.(bool); ok {
				copyValue := typed
				return &copyValue, true
			}
		}
	}
	return nil, false
}

func grokFactExhausted(fact database.GrokAccountFact) bool {
	status := strings.ToLower(strings.TrimSpace(fact.Status))
	if status == "exhausted" || status == "balance_exhausted" {
		return true
	}
	for _, key := range []string{"exhausted", "balance_exhausted", "is_exhausted"} {
		if value, ok := fact.Payload[key].(bool); ok && value {
			return true
		}
	}
	return false
}

// GrokDispatchHardAllowed applies the current credential-generation control
// plane gates to inference dispatch. A missing/stale allow_access field keeps
// the first-party fail-open behaviour; only an explicit fresh false or an
// explicit fresh exhausted billing fact removes the account from scheduling.
func (a *Account) GrokDispatchHardAllowed(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isGrokAPILocked() {
		return false
	}
	if a.GrokFactsGeneration != a.CredentialGeneration {
		return true
	}
	if a.GrokAccessAllowed != nil && now.Before(a.GrokAccessExpiresAt) && !*a.GrokAccessAllowed {
		return false
	}
	return !(a.GrokBillingExhausted && now.Before(a.GrokBillingExpiresAt))
}

// applyGrokPersistentState projects only generation-matching, non-secret DB
// observations into one runtime account. Caller invokes this before publishing
// the account, so no account lock is needed.
func applyGrokPersistentState(account *Account, state *database.GrokAccountState) {
	if account == nil || state == nil || state.CredentialGeneration != account.CredentialGeneration {
		return
	}
	// Replacement semantics: absent/null fields in a newer snapshot must clear
	// older projections instead of accidentally preserving stale authorization.
	account.GrokLivePlan = ""
	account.GrokLivePlanObservedAt = time.Time{}
	account.GrokLivePlanExpiresAt = time.Time{}
	account.GrokLivePlanKnown = false
	account.GrokAccessAllowed = nil
	account.GrokAccessExpiresAt = time.Time{}
	account.GrokBillingExhausted = false
	account.GrokBillingExpiresAt = time.Time{}
	account.grokRouting = nil
	account.grokRuntimeModelsHint = ""
	account.grokRuntimeModelsHintOrigin = ""
	account.grokRuntimeModelsHintGeneration = 0
	var latestHintAt time.Time
	account.GrokFactsGeneration = state.CredentialGeneration
	if user, ok := state.Facts[database.GrokFactUser]; ok && user.CredentialGeneration == state.CredentialGeneration {
		plan := grokFactString(user.Payload, "subscriptionTier", "subscription_tier")
		presence := user.FieldPresence["subscriptionTier"]
		if presence == "" {
			presence = user.FieldPresence["subscription_tier"]
		}
		if plan != "" && (presence == "" || presence == "value" || presence == "present") {
			account.GrokLivePlan = plan
			account.GrokLivePlanKnown = true
			account.GrokLivePlanObservedAt = user.ObservedAt
			account.GrokLivePlanExpiresAt = user.ExpiresAt
		}
	}
	if settings, ok := state.Facts[database.GrokFactSettings]; ok && settings.CredentialGeneration == state.CredentialGeneration {
		presence := settings.FieldPresence["allow_access"]
		if presence == "value" || presence == "present" || presence == "" {
			if value, exists := grokFactBool(settings.Payload, "allow_access", "allowAccess"); exists {
				account.GrokAccessAllowed = value
				account.GrokAccessExpiresAt = settings.ExpiresAt
			}
		}
	}
	if billing, ok := state.Facts[database.GrokFactBilling]; ok && billing.CredentialGeneration == state.CredentialGeneration {
		account.GrokBillingExhausted = grokFactExhausted(billing)
		account.GrokBillingExpiresAt = billing.ExpiresAt
	}
	routing := GrokRoutingState{CredentialGeneration: state.CredentialGeneration}
	for _, catalog := range state.Catalogs {
		if catalog.Snapshot.CredentialGeneration != state.CredentialGeneration {
			continue
		}
		routing.CatalogKnown = true
		if routing.ObservedAt.IsZero() || catalog.Snapshot.ObservedAt.After(routing.ObservedAt) {
			routing.ObservedAt = catalog.Snapshot.ObservedAt
		}
		if routing.ExpiresAt.IsZero() || catalog.Snapshot.ExpiresAt.Before(routing.ExpiresAt) {
			routing.ExpiresAt = catalog.Snapshot.ExpiresAt
		}
		if strings.TrimSpace(catalog.Snapshot.ETagHint) != "" &&
			(account.grokRuntimeModelsHintGeneration == 0 || catalog.Snapshot.ETagHintObservedAt.After(latestHintAt)) {
			account.grokRuntimeModelsHint = strings.TrimSpace(catalog.Snapshot.ETagHint)
			account.grokRuntimeModelsHintOrigin = strings.TrimRight(strings.TrimSpace(catalog.Snapshot.Origin), "/")
			account.grokRuntimeModelsHintGeneration = state.CredentialGeneration
			latestHintAt = catalog.Snapshot.ETagHintObservedAt
		}
		for _, item := range catalog.Items {
			if item.CredentialGeneration != state.CredentialGeneration {
				continue
			}
			var supported *bool
			switch item.FieldPresence["supported_in_api"] {
			case "value", "present":
				value := item.SupportedInAPI
				supported = &value
			}
			routing.Models = append(routing.Models, GrokModelRoute{
				ModelID: item.ModelID, BaseURL: item.BaseURL, APIBaseURL: item.APIBaseURL,
				APIBackend: NormalizeGrokProtocol(item.APIBackend), ExtraHeaders: item.ExtraHeaders,
				SupportedInAPI: supported, Hidden: item.Hidden, ContextWindow: item.ContextWindow,
				MaxCompletionTokens: item.MaxOutputTokens, SupportsReasoningEffort: item.SupportsReasoningEffort,
				SupportsBackendSearch: item.SupportsBackendSearch, StreamToolCalls: item.StreamToolCalls,
				FirstSeenAt: item.FirstSeenAt,
			})
		}
	}
	for _, capability := range state.Capabilities {
		if capability.CredentialGeneration != state.CredentialGeneration {
			continue
		}
		routing.Capabilities = append(routing.Capabilities, GrokProtocolCapability{
			ModelID: capability.ModelID, Origin: capability.Origin,
			Protocol: NormalizeGrokProtocol(capability.Protocol), Status: capability.Status,
			ObservedAt: capability.ObservedAt, ExpiresAt: capability.ExpiresAt,
		})
	}
	if routing.CatalogKnown || len(routing.Capabilities) > 0 {
		account.grokRouting = cloneGrokRoutingState(routing)
	}
}

// ReloadGrokPersistentState refreshes an already-published account after an
// admin catalog sync or capability probe.
func (s *Store) ReloadGrokPersistentState(ctx context.Context, accountID int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	state, err := s.db.GetGrokAccountState(ctx, accountID)
	if err != nil {
		return err
	}
	account := s.FindByID(accountID)
	if account == nil {
		return nil
	}
	account.mu.Lock()
	if account.CredentialGeneration <= 0 {
		account.CredentialGeneration = state.CredentialGeneration
		account.CredentialFamilyID = state.Identity.CredentialFamilyID
	}
	if account.CredentialGeneration == state.CredentialGeneration {
		applyGrokPersistentState(account, state)
	}
	account.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(account)
	return nil
}

// ApplyPersistedGrokState is the no-I/O counterpart used by catalog sync code
// that already holds a freshly persisted aggregate.
func (s *Store) ApplyPersistedGrokState(accountID int64, state *database.GrokAccountState) bool {
	if s == nil || state == nil {
		return false
	}
	account := s.FindByID(accountID)
	if account == nil {
		return false
	}
	account.mu.Lock()
	if account.CredentialGeneration <= 0 {
		account.CredentialGeneration = state.CredentialGeneration
		account.CredentialFamilyID = state.Identity.CredentialFamilyID
	}
	if account.CredentialGeneration != state.CredentialGeneration {
		account.mu.Unlock()
		return false
	}
	applyGrokPersistentState(account, state)
	account.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(account)
	return true
}

// InvalidateGrokPersistentState clears observations immediately after a
// credential CAS advances the account generation.
func (a *Account) invalidateGrokPersistentStateLocked(newGeneration int64) {
	a.CredentialGeneration = newGeneration
	a.GrokLivePlan = ""
	a.GrokLivePlanObservedAt = time.Time{}
	a.GrokLivePlanExpiresAt = time.Time{}
	a.GrokLivePlanKnown = false
	a.GrokAccessAllowed = nil
	a.GrokAccessExpiresAt = time.Time{}
	a.GrokBillingExhausted = false
	a.GrokBillingExpiresAt = time.Time{}
	a.GrokFactsGeneration = 0
	a.grokRouting = nil
	a.grokRuntimeModelsHint = ""
	a.grokRuntimeModelsHintOrigin = ""
	a.grokRuntimeModelsHintGeneration = 0
}
