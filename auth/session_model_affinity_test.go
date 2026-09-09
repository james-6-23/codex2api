package auth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/cache"
)

func TestSessionModelMismatchKeepsBoundAccount(test *testing.T) {
	for _, scenario := range []struct {
		name, mode   string
		hard, shared bool
	}{
		{"hard window", "bounded", true, false},
		{"ordinary affinity", "bounded", false, false},
		{"strict affinity", "strict", false, false},
		{"affinity disabled", "off", false, false},
		{"shared affinity", "bounded", false, true},
		{"shared affinity disabled", "off", false, true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			store, bound, fallback := newHardWindowFallbackTestStore()
			bound.Models, fallback.Models = []string{"model-b"}, []string{"model-a"}
			bound.SessionCapacityEnabled = scenario.hard
			store.SetAffinityMode(scenario.mode)
			if scenario.shared {
				store.tokenCache = cache.NewMemory(1)
				test.Cleanup(func() { _ = store.tokenCache.Close() })
			}
			const rootKey = "original-root::api-key:101"
			bindHardWindowFallbackTestRoot(test, store, bound, rootKey)
			if scenario.shared {
				store.sessionBindings = make(map[string]sessionAffinity)
			}
			modelFilter := func(account *Account) bool { return account.SupportsCodexModel("model-a") }
			trace := &SelectionTrace{}
			trace.SetSessionModelFilter(modelFilter)
			selected, _ := store.NextForSessionWithFilter(rootKey, 101, nil, modelFilter, trace)
			if selected != nil || !trace.SessionModelDenied() {
				test.Fatalf("model switch migrated to %v instead of rejecting", selected)
			}
			if accountID, found := store.SessionAffinityAccountID(rootKey); !found || accountID != bound.ID() {
				test.Fatalf("original binding was lost: account=%d found=%v", accountID, found)
			}
			trace.Reset()
			selected, _ = store.NextForSessionWithFilter("new-root::api-key:101", 101, nil, modelFilter, trace)
			if selected != fallback || trace.SessionModelDenied() {
				test.Fatal("a new conversation cannot select the requested model")
			}
			store.Release(selected)
			originalModelFilter := func(account *Account) bool { return account.SupportsCodexModel("model-b") }
			trace.Reset()
			trace.SetSessionModelFilter(originalModelFilter)
			selected, _ = store.NextForSessionWithFilter(rootKey, 101, nil, originalModelFilter, trace)
			if selected != bound {
				test.Fatal("switching back cannot reuse the original account")
			}
			store.Release(selected)
		})
	}
}

func TestSessionModelMismatchDoesNotWaitForCapacity(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		store, bound, fallback := newHardWindowFallbackTestStore()
		bound.Models, fallback.Models = []string{"model-b"}, []string{"model-a"}
		bindHardWindowFallbackTestRoot(test, store, bound, "root")
		filter := func(account *Account) bool { return account.SupportsCodexModel("model-a") }
		trace := &SelectionTrace{}
		trace.SetSessionModelFilter(filter)
		started := time.Now()
		selected, _, _ := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "root", 30*time.Second, 0, nil, filter, DispatchPolicyStandard, trace)
		if selected != nil || !trace.SessionModelDenied() || !time.Now().Equal(started) {
			test.Fatal("a deterministic model mismatch waited or selected another account")
		}
	})
}

func TestSessionModelConstraintPreservesQuotaFailover(test *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	bound.Models, fallback.Models = []string{"model-b"}, []string{"model-b"}
	bindHardWindowFallbackTestRoot(test, store, bound, "root")
	bound.AutoPause5hThreshold = 0.95
	bound.UsagePercent5h, bound.UsagePercent5hValid = 99, true
	bound.Reset5hAt = time.Now().Add(time.Hour)
	bound.recomputeEffectiveAutoPause(store)
	filter := func(account *Account) bool { return account.SupportsCodexModel("model-b") }
	trace := &SelectionTrace{}
	trace.SetSessionModelFilter(filter)
	selected, _ := store.NextForSessionWithFilter("root", 0, nil, filter, trace)
	if selected != fallback || trace.SessionModelDenied() {
		test.Fatal("a supported model changed the existing quota failover policy")
	}
	store.Release(selected)
}

func TestSessionModelConstraintIgnoresExpiredAffinity(test *testing.T) {
	store, bound, fallback := newHardWindowFallbackTestStore()
	bound.SessionCapacityEnabled = false
	bound.Models, fallback.Models = []string{"model-b"}, []string{"model-a"}
	store.sessionBindings["expired"] = sessionAffinity{accountID: bound.ID(), expiresAt: time.Now().Add(-time.Second)}
	if _, found := store.LiveSessionAccountID("expired", time.Now()); found {
		test.Fatal("expired affinity is still considered an active conversation")
	}
	filter := func(account *Account) bool { return account.SupportsCodexModel("model-a") }
	trace := &SelectionTrace{}
	trace.SetSessionModelFilter(filter)
	selected, _ := store.NextForSessionWithFilter("expired", 0, nil, filter, trace)
	if selected != fallback || trace.SessionModelDenied() {
		test.Fatal("expired affinity blocked a new model selection")
	}
	store.Release(selected)
}
