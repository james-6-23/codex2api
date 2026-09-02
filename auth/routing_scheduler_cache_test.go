package auth

import (
	"sync/atomic"
	"testing"
)

func newIndexedRoutingTestStore(accounts []*Account) *Store {
	store := &Store{
		accounts:          accounts,
		maxConcurrency:    1,
		routingSchedulers: make(map[int64]*routingSchedulerEntry),
		schedulerMetrics:  newSchedulerRuntimeMetrics(),
	}
	store.availability.Store(newAvailabilityHub())
	store.rebuildAccountIndex()
	store.publishAccountSnapshot(accounts)
	store.SetFastSchedulerEnabled(true)
	return store
}

func sparseRoutingAccounts(total int, allowedGroup int64) []*Account {
	accounts := make([]*Account, 0, total)
	for i := 1; i <= total; i++ {
		acc := newFastSchedulerTestAccount(int64(i), HealthTierHealthy, 100, 1)
		acc.GroupIDs = []int64{1}
		if i == total {
			acc.GroupIDs = []int64{allowedGroup}
		}
		accounts = append(accounts, acc)
	}
	return accounts
}

func TestRoutingSchedulerCachesSparseAPIKeyPool(t *testing.T) {
	const apiKeyID int64 = 42
	accounts := sparseRoutingAccounts(1000, 9)
	store := newIndexedRoutingTestStore(accounts)
	store.SetAPIKeyAllowedGroups(apiKeyID, []int64{9})

	first := store.NextExcluding(apiKeyID, nil)
	if first == nil || first.DBID != 1000 {
		t.Fatalf("first selection = %v, want account 1000", first)
	}
	store.Release(first)
	second := store.NextExcluding(apiKeyID, nil)
	if second == nil || second.DBID != 1000 {
		t.Fatalf("second selection = %v, want account 1000", second)
	}
	store.Release(second)

	metrics := store.GetSchedulerMetrics()
	if metrics.RoutingCacheBuilds != 1 {
		t.Fatalf("routing cache builds = %d, want 1", metrics.RoutingCacheBuilds)
	}
	if metrics.RoutingCacheHits < 1 {
		t.Fatalf("routing cache hits = %d, want at least 1", metrics.RoutingCacheHits)
	}
	if metrics.RoutingCacheEntries != 1 || metrics.RoutingCacheAccounts != 1 {
		t.Fatalf("routing cache gauges = entries:%d accounts:%d, want 1/1", metrics.RoutingCacheEntries, metrics.RoutingCacheAccounts)
	}
}

func TestRoutingSchedulerSkipsSubPoolForUnrestrictedKey(t *testing.T) {
	store := newIndexedRoutingTestStore(sparseRoutingAccounts(1000, 9))
	selected := store.NextExcluding(99, nil)
	if selected == nil {
		t.Fatal("unrestricted selection returned nil")
	}
	store.Release(selected)

	metrics := store.GetSchedulerMetrics()
	if metrics.RoutingCacheBuilds != 0 || metrics.RoutingCacheFallbacks != 1 {
		t.Fatalf("unrestricted cache metrics = builds:%d fallbacks:%d, want 0/1", metrics.RoutingCacheBuilds, metrics.RoutingCacheFallbacks)
	}
}

func TestRoutingSchedulerInvalidatesOnMembershipChange(t *testing.T) {
	const apiKeyID int64 = 7
	accounts := sparseRoutingAccounts(16, 9)
	store := newIndexedRoutingTestStore(accounts)
	store.SetAPIKeyAllowedGroups(apiKeyID, []int64{9})

	selected := store.NextExcluding(apiKeyID, nil)
	if selected == nil || selected.DBID != 16 {
		t.Fatalf("selection before membership change = %v, want account 16", selected)
	}
	store.Release(selected)

	store.ApplyAccountGroups(16, []int64{1})
	store.ApplyAccountGroups(3, []int64{9})
	selected = store.NextExcluding(apiKeyID, nil)
	if selected == nil || selected.DBID != 3 {
		t.Fatalf("selection after membership change = %v, want account 3", selected)
	}
	store.Release(selected)

	metrics := store.GetSchedulerMetrics()
	if metrics.RoutingCacheBuilds != 2 {
		t.Fatalf("routing cache builds = %d, want 2", metrics.RoutingCacheBuilds)
	}
	if metrics.RoutingCacheInvalidations == 0 {
		t.Fatal("membership change did not invalidate routing cache")
	}
}

func TestRoutingSchedulerRetainsUnavailableEligibleAccount(t *testing.T) {
	const apiKeyID int64 = 11
	accounts := sparseRoutingAccounts(16, 9)
	eligible := accounts[len(accounts)-1]
	atomic.StoreInt32(&eligible.DispatchPaused, 1)
	store := newIndexedRoutingTestStore(accounts)
	store.SetAPIKeyAllowedGroups(apiKeyID, []int64{9})

	if selected := store.NextExcluding(apiKeyID, nil); selected != nil {
		store.Release(selected)
		t.Fatalf("paused account was selected: %d", selected.DBID)
	}
	atomic.StoreInt32(&eligible.DispatchPaused, 0)
	selected := store.NextExcluding(apiKeyID, nil)
	if selected == nil || selected.DBID != eligible.DBID {
		t.Fatalf("recovered selection = %v, want account %d", selected, eligible.DBID)
	}
	store.Release(selected)

	if builds := store.GetSchedulerMetrics().RoutingCacheBuilds; builds != 1 {
		t.Fatalf("recovery rebuilt routing cache %d times, want 1", builds)
	}
}

func TestSchedulerShadowSamplesIndexedAvailability(t *testing.T) {
	accounts := sparseRoutingAccounts(16, 9)
	store := newIndexedRoutingTestStore(accounts)
	store.SetSchedulerEngine("shadow")

	selected := store.NextExcluding(0, nil)
	if selected == nil {
		t.Fatal("shadow legacy selection returned nil")
	}
	store.Release(selected)

	metrics := store.GetSchedulerMetrics()
	if metrics.ShadowChecks != 1 || metrics.ShadowAgreements != 1 || metrics.ShadowMismatches != 0 {
		t.Fatalf("shadow metrics = checks:%d agreements:%d mismatches:%d, want 1/1/0", metrics.ShadowChecks, metrics.ShadowAgreements, metrics.ShadowMismatches)
	}
	if metrics.SelectionSlowHit != 1 || metrics.SelectionFastHit != 0 {
		t.Fatalf("shadow authoritative path = slow:%d fast:%d, want 1/0", metrics.SelectionSlowHit, metrics.SelectionFastHit)
	}
}

func TestRoutingSchedulerEvictsSingleLRUEntry(t *testing.T) {
	accounts := sparseRoutingAccounts(16, 9)
	store := newIndexedRoutingTestStore(accounts)
	// 先配置全部 key 再选号:SetAPIKeyAllowedGroups 本身会失效缓存。
	for key := int64(1); key <= int64(maxRoutingSchedulerKeys)+1; key++ {
		store.SetAPIKeyAllowedGroups(key, []int64{9})
	}
	for key := int64(1); key <= int64(maxRoutingSchedulerKeys)+1; key++ {
		if acc := store.NextExcluding(key, nil); acc != nil {
			store.Release(acc)
		}
	}

	metrics := store.GetSchedulerMetrics()
	if metrics.RoutingCacheEvictions != 1 {
		t.Fatalf("evictions = %d, want exactly 1 (LRU, not wipe-all)", metrics.RoutingCacheEvictions)
	}
	if metrics.RoutingCacheEntries != int64(maxRoutingSchedulerKeys) {
		t.Fatalf("entries = %d, want %d", metrics.RoutingCacheEntries, maxRoutingSchedulerKeys)
	}

	store.routingSchedulersMu.RLock()
	_, oldest := store.routingSchedulers[1]
	_, newest := store.routingSchedulers[int64(maxRoutingSchedulerKeys)+1]
	store.routingSchedulersMu.RUnlock()
	if oldest {
		t.Fatal("LRU eviction should have removed the oldest key")
	}
	if !newest {
		t.Fatal("newest key missing from cache after eviction")
	}
}

func BenchmarkRoutingSchedulerSparse50000(b *testing.B) {
	const apiKeyID int64 = 42
	store := newIndexedRoutingTestStore(sparseRoutingAccounts(50_000, 9))
	store.SetAPIKeyAllowedGroups(apiKeyID, []int64{9})
	acc := store.NextExcluding(apiKeyID, nil)
	if acc == nil {
		b.Fatal("routing scheduler warmup returned nil")
	}
	store.Release(acc)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		acc := store.NextExcluding(apiKeyID, nil)
		if acc == nil {
			b.Fatal("routing scheduler returned nil")
		}
		store.Release(acc)
	}
}
