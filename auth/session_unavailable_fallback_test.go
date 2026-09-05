package auth

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

// newHardWindowFallbackTestStore creates one bound account and one account
// that can receive a fresh root request. The account-session maps are
// initialized explicitly because these tests exercise the same path used by
// stores restored from the runtime cache.
func newHardWindowFallbackTestStore() (*Store, *Account, *Account) {
	bound := &Account{
		DBID:                          1,
		AccessToken:                   "bound-token",
		Status:                        StatusReady,
		HealthTier:                    HealthTierHealthy,
		SessionCapacityEnabled:        true,
		SessionCapacityMax:            2,
		SessionCapacityIdleTTLSeconds: 3600,
	}
	fallback := &Account{
		DBID:        2,
		AccessToken: "fallback-token",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	store := &Store{
		accounts:                []*Account{bound, fallback},
		maxConcurrency:          2,
		accountSessions:         make(map[int64]map[string]*accountSessionState),
		accountSessionsHydrated: make(map[int64]bool),
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.publishAccountSnapshot(store.accounts)
	return store, bound, fallback
}

func bindHardWindowFallbackTestRoot(t *testing.T, store *Store, account *Account, key string) {
	t.Helper()
	if !store.AdmitAccountSession(account, key, time.Now()) {
		t.Fatal("failed to admit root session")
	}
	store.BindSessionAffinity(key, account, "")
}

func TestHardWindowFreshRootFallsBackWhenOwnerIsUnavailable(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Store, *Account)
	}{
		{
			name: "quota auto pause",
			setup: func(store *Store, account *Account) {
				account.AutoPause5hThreshold = 0.95
				account.UsagePercent5h = 99
				account.UsagePercent5hValid = true
				account.Reset5hAt = time.Now().Add(time.Hour)
				account.recomputeEffectiveAutoPause(store)
			},
		},
		{
			name: "usage window exhausted",
			setup: func(_ *Store, account *Account) {
				account.PlanType = "free"
				account.UsagePercent7d = 100
				account.UsagePercent7dValid = true
				account.Reset7dAt = time.Now().Add(time.Hour)
			},
		},
		{
			name: "active cooldown",
			setup: func(_ *Store, account *Account) {
				account.Status = StatusCooldown
				account.CooldownUtil = time.Now().Add(time.Hour)
			},
		},
		{
			name: "status error",
			setup: func(_ *Store, account *Account) {
				account.Status = StatusError
			},
		},
		{
			name: "disabled",
			setup: func(_ *Store, account *Account) {
				atomic.StoreInt32(&account.Disabled, 1)
			},
		},
		{
			name: "dispatch paused",
			setup: func(_ *Store, account *Account) {
				atomic.StoreInt32(&account.DispatchPaused, 1)
			},
		},
		{
			name: "banned health tier",
			setup: func(_ *Store, account *Account) {
				account.HealthTier = HealthTierBanned
			},
		},
		{
			name: "missing credential",
			setup: func(_ *Store, account *Account) {
				account.AccessToken = ""
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, bound, fallback := newHardWindowFallbackTestStore()
			const rootKey = "hard-window-unavailable-root"
			bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
			test.setup(store, bound)

			selected, proxyURL := store.NextForSession(rootKey, 0, nil)
			if selected != fallback {
				if selected != nil {
					store.Release(selected)
				}
				t.Fatalf("selected account = %#v, want fallback account %d", selected, fallback.DBID)
			}
			if proxyURL != "" {
				t.Fatalf("fallback proxy = %q, want empty", proxyURL)
			}
			store.Release(selected)
			if _, found := store.AccountSessionAccountID(rootKey, time.Now()); found {
				t.Fatal("unavailable owner still holds the root account-session window")
			}
			if _, found := store.SessionAffinityAccountID(rootKey); found {
				t.Fatal("unavailable owner still holds ordinary session affinity")
			}
		})
	}
}

func TestHardWindowUnavailableOwnerKeepsStrictContinuations(t *testing.T) {
	store, bound, _ := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-strict-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	bound.AutoPause5hThreshold = 0.95
	bound.UsagePercent5h = 99
	bound.UsagePercent5hValid = true
	bound.Reset5hAt = time.Now().Add(time.Hour)
	bound.recomputeEffectiveAutoPause(store)

	if selected, _ := store.NextForContinuationWithFilter(rootKey, 0, nil, nil); selected != nil {
		store.Release(selected)
		t.Fatalf("continuation migrated to account %d; want strict owner", selected.DBID)
	}
	if selected, _ := store.NextForSession(RelatedSessionAffinityKey(rootKey), 0, nil); selected != nil {
		store.Release(selected)
		t.Fatalf("related request migrated to account %d; want strict owner", selected.DBID)
	}
}

func TestHardWindowBusyOwnerDoesNotMigrate(t *testing.T) {
	store, bound, _ := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-busy-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.maxConcurrency = 1
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatal("failed to occupy bound account")
	}
	defer store.Release(held)

	selected, _ := store.NextForSession(rootKey, 0, nil)
	if selected != nil {
		store.Release(selected)
		t.Fatalf("busy owner migrated to account %d; want wait/no candidate", selected.DBID)
	}
}

func TestHardWindowFailurePinDoesNotBlockQuotaMigration(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-pinned-quota-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	bound.AutoPause7dThreshold = 0.95
	bound.UsagePercent7d = 99
	bound.UsagePercent7dValid = true
	bound.Reset7dAt = time.Now().Add(time.Hour)
	bound.recomputeEffectiveAutoPause(store)

	selected, _ := store.NextForSession(rootKey, 0, nil)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("pinned quota owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFailurePinWithIgnoreUsageStillMigratesFreshRoot(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-pinned-ignore-quota-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	// The ignore flag is a continuation escape hatch. It must not make a fresh
	// root request stay pinned to an account whose usage window is full.
	bound.ignoreUsageLimitStatus = true
	bound.AutoPause7dThreshold = 0.95
	bound.UsagePercent7d = 99
	bound.UsagePercent7dValid = true
	bound.Reset7dAt = time.Now().Add(time.Hour)
	bound.recomputeEffectiveAutoPause(store)

	selected, _ := store.NextForSession(rootKey, 0, nil)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("pinned ignore-usage owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFailurePinDoesNotBlockSparkUsageFallback(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-pinned-spark-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	bound.PlanType = "pro"
	bound.UsagePercentSpark = 100
	bound.UsagePercentSparkValid = true
	bound.ResetSparkAt = time.Now().Add(time.Hour)
	fallback.PlanType = "pro"

	selected, _, _ := store.nextForSessionWithFilter(rootKey, 0, nil, nil, false, DispatchPolicySpark)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("pinned spark-exhausted owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFailurePinDoesNotBlockCachedCooldownFallback(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	store.tokenCache = cache.NewMemory(4)
	t.Cleanup(func() { store.tokenCache.Close() })
	const rootKey = "hard-window-pinned-cached-cooldown-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	store.setCachedAccountCooldown(bound.DBID, "rate_limited_7d", time.Now().Add(time.Hour))

	selected, _, _ := store.nextForSessionWithFilter(rootKey, 0, nil, nil, false, DispatchPolicyStandard)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("cached-cooldown pinned owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFailurePinDoesNotBlockDispatchCountFallback(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-pinned-dispatch-count-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	bound.SetDispatchCountLimit(1)
	// Consume the only dispatch slot directly. This leaves the account's
	// Status untouched, reproducing the race where the limit is discovered
	// inside takeByIDModeWithCapacity after the failure pin was inspected.
	if reservation := bound.reserveDispatchCount(time.Now()); !reservation.Allowed {
		t.Fatal("failed to seed dispatch-count window")
	}

	selected, _, _ := store.nextForSessionWithFilter(rootKey, 0, nil, nil, false, DispatchPolicyStandard)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("dispatch-count pinned owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFailurePinDoesNotBlockCredentialRefreshFallback(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-pinned-credential-refresh-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.PinSessionAffinityAfterTransientFailure(rootKey, bound.DBID)
	bound.Status = StatusCooldown
	bound.CooldownReason = "credential_refresh"
	bound.CooldownUtil = time.Now().Add(time.Hour)

	selected, _, _ := store.nextForSessionWithFilter(rootKey, 0, nil, nil, false, DispatchPolicyStandard)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("credential-refresh pinned owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestHardWindowFreshRootFallsBackWhenModelCooldownFiltersOwner(t *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	const rootKey = "hard-window-model-cooldown-root"
	bindHardWindowFallbackTestRoot(t, store, bound, rootKey)
	store.MarkModelCooldown(bound, "gpt-5.4", time.Hour, "model_capacity")
	filter := store.WithModelCooldownFilter("gpt-5.4", nil)

	selected, _, _ := store.nextForSessionWithFilter(rootKey, 0, nil, filter, false, DispatchPolicyStandard)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("model-cooldown owner selected = %#v, want fallback %d", selected, fallback.DBID)
	}
	store.Release(selected)
}

func TestFailurePinDoesNotBlockRequestFilterFallback(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "bound-token", Status: StatusReady, HealthTier: HealthTierHealthy},
			{DBID: 2, AccessToken: "fallback-token", Status: StatusReady, HealthTier: HealthTierHealthy},
		},
		maxConcurrency:          2,
		accountSessions:         make(map[int64]map[string]*accountSessionState),
		accountSessionsHydrated: make(map[int64]bool),
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.publishAccountSnapshot(store.accounts)
	const rootKey = "failure-pin-filter-root"
	store.BindSessionAffinity(rootKey, store.accounts[0], "")
	store.PinSessionAffinityAfterTransientFailure(rootKey, store.accounts[0].DBID)

	selected, _ := store.NextForSessionWithFilter(rootKey, 0, nil, func(account *Account) bool {
		return account.DBID == 2
	})
	if selected != store.accounts[1] {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("filtered failure-pinned owner selected = %#v, want fallback account 2", selected)
	}
	store.Release(selected)
	store.BindSessionAffinity(rootKey, selected, "")
	if got, ok := store.SessionAffinityAccountID(rootKey); !ok || got != 2 {
		t.Fatalf("binding after filter fallback = %d/%v, want account 2", got, ok)
	}
}

func TestBoundedEscapeReusesOnlyUsableOwnerWhenNoAlternativeExists(t *testing.T) {
	owner := &Account{DBID: 1, AccessToken: "owner-token", Status: StatusReady, HealthTier: HealthTierWarm}
	store := &Store{accounts: []*Account{owner}, maxConcurrency: 1}
	store.publishAccountSnapshot(store.accounts)
	const key = "bounded-single-owner"
	store.BindSessionAffinity(key, owner, "")
	store.sessionMu.Lock()
	binding := store.sessionBindings[key]
	binding.lastUsedAt = time.Now().Add(-sessionAffinityIdleEscape() - time.Minute)
	store.sessionBindings[key] = binding
	store.sessionMu.Unlock()

	selected, _ := store.NextForSession(key, 0, nil)
	if selected != owner {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("single usable owner selected = %#v, want owner", selected)
	}
	store.Release(selected)
}
