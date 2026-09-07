package auth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestAccountUsagePeriodSplitsAfterIdleExpiryButSharesRelatedRequests(test *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	now := time.Now().UTC()
	if !store.AdmitAccountSession(account, "root", now) {
		test.Fatal("initial admission failed")
	}
	first := store.AccountSessionUsagePeriod(account.ID(), "root", now)
	related := store.AccountSessionUsagePeriod(account.ID(), RelatedSessionAffinityKey("root"), now)
	if first.ID == "" || first != related || !first.StartedAt.Equal(now) {
		test.Fatalf("first=%+v related=%+v", first, related)
	}
	if !store.AdmitAccountSession(account, "root", now.Add(10*time.Second)) {
		test.Fatal("reuse failed")
	}
	if reused := store.AccountSessionUsagePeriod(account.ID(), "root", now); reused != first {
		test.Fatalf("reused=%+v first=%+v", reused, first)
	}
	reopenedAt := now.Add(2 * time.Minute)
	if !store.AdmitAccountSession(account, "root", reopenedAt) {
		test.Fatal("reopening failed")
	}
	reopened := store.AccountSessionUsagePeriod(account.ID(), "root", reopenedAt)
	if reopened.ID == first.ID || reopened.ID == "" || !reopened.StartedAt.Equal(reopenedAt) {
		test.Fatalf("reopened=%+v first=%+v", reopened, first)
	}
	if bypass := store.AccountSessionUsagePeriod(account.ID(), SessionAccountingBypassAffinityKey("root"), reopenedAt); bypass.ID != "" {
		test.Fatalf("bypass=%+v", bypass)
	}
}

func TestLegacyActiveWindowPersistsItsNewUsagePeriodImmediately(test *testing.T) {
	runtimeCache := cache.NewMemory(1)
	test.Cleanup(func() { _ = runtimeCache.Close() })
	payload, err := json.Marshal(persistedAccountSessionCollection{Version: 1, Sessions: []persistedAccountSessionState{{SessionID: "legacy-root", LastSeen: time.Now().UTC()}}})
	if err != nil {
		test.Fatal(err)
	}
	if err := runtimeCache.SetRuntime(test.Context(), accountSessionRuntimeNamespace, "11", payload, time.Hour); err != nil {
		test.Fatal(err)
	}
	var original AccountSessionUsagePeriod
	for attempt := 0; attempt < 2; attempt++ {
		store := NewStore(nil, runtimeCache, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1})
		test.Cleanup(store.Stop)
		account := &Account{DBID: 11, AccessToken: "test", Status: StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600}
		store.AddAccount(account)
		if !store.AdmitAccountSession(account, "legacy-root", time.Now()) {
			test.Fatal("legacy window was not restored")
		}
		period := store.AccountSessionUsagePeriod(account.ID(), "legacy-root", time.Now())
		if period.ID == "" || period.StartedAt.IsZero() {
			test.Fatalf("period=%+v", period)
		}
		if attempt == 0 {
			original = period
		} else if period.ID != original.ID || !period.StartedAt.Equal(original.StartedAt) {
			test.Fatalf("period changed before the normal persistence interval: original=%+v restored=%+v", original, period)
		}
	}
}
