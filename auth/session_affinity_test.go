package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func requireSessionBinding(t *testing.T, store *Store, key string, accountID int64) {
	t.Helper()
	store.sessionMu.RLock()
	binding, ok := store.sessionBindings[key]
	store.sessionMu.RUnlock()
	if !ok || binding.accountID != accountID {
		t.Fatalf("session binding %q = %#v, want account %d", key, binding, accountID)
	}
}

func TestNextForSessionPrefersBoundAccountAndProxy(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestNextForSessionUsesCachedAffinityWhenLocalBindingMissing(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	if err := tokenCache.SetSessionAffinity(context.Background(), "session-redis", cache.SessionAffinityBinding{
		AccountID: 2,
		ProxyURL:  "http://proxy-2",
	}, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}

	acc, proxyURL := store.NextForSession("session-redis", 0, nil)
	if acc == nil {
		t.Fatal("expected account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestNextForSessionRejectsRemovedPoolProxyAffinity(t *testing.T) {
	const removedProxy = "http://removed.example:8080"
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:   2,
		proxyPoolEnabled: true,
		proxyPool:        []string{removedProxy},
	}
	store.bindSessionAffinity("removed-local", store.accounts[0], removedProxy)

	store.mu.Lock()
	store.proxyPool = []string{"http://replacement.example:8080"}
	store.mu.Unlock()

	acc, proxyURL := store.NextForSession("removed-local", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	defer store.Release(acc)
	if proxyURL == removedProxy {
		t.Fatalf("proxyURL = %q, must not reuse removed pool proxy", proxyURL)
	}
	store.sessionMu.RLock()
	_, stillBound := store.sessionBindings["removed-local"]
	store.sessionMu.RUnlock()
	if stillBound {
		t.Fatal("removed proxy affinity remains in local bindings")
	}
}

func TestNextForSessionRejectsRemovedCachedProxyAffinity(t *testing.T) {
	const removedProxy = "http://removed.example:8080"
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	if err := tokenCache.SetSessionAffinity(context.Background(), "removed-cached", cache.SessionAffinityBinding{
		AccountID: 1,
		ProxyURL:  removedProxy,
	}, time.Hour); err != nil {
		t.Fatalf("SetSessionAffinity: %v", err)
	}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:   2,
		tokenCache:       tokenCache,
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://replacement.example:8080"},
	}

	acc, proxyURL := store.NextForSession("removed-cached", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	defer store.Release(acc)
	if proxyURL == removedProxy {
		t.Fatalf("proxyURL = %q, must not reuse removed cached proxy", proxyURL)
	}
	if _, ok, err := tokenCache.GetSessionAffinity(context.Background(), "removed-cached"); err != nil {
		t.Fatalf("GetSessionAffinity: %v", err)
	} else if ok {
		t.Fatal("removed proxy affinity remains in token cache")
	}
}

func TestBindSessionAffinityRejectsProxyRemovedBeforeLateBind(t *testing.T) {
	const removedProxy = "http://removed.example:8080"
	tokenCache := cache.NewMemory(1)
	defer tokenCache.Close()
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:   2,
		tokenCache:       tokenCache,
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://replacement.example:8080"},
	}

	store.BindSessionAffinity("late-bind", store.accounts[0], removedProxy)

	store.sessionMu.RLock()
	_, locallyBound := store.sessionBindings["late-bind"]
	store.sessionMu.RUnlock()
	if locallyBound {
		t.Fatal("late bind resurrected a proxy removed from the runtime pool")
	}
	if _, ok, err := tokenCache.GetSessionAffinity(context.Background(), "late-bind"); err != nil {
		t.Fatalf("GetSessionAffinity: %v", err)
	} else if ok {
		t.Fatal("late bind wrote a removed proxy to the token cache")
	}
}

func TestNextForSessionKeepsAffinityForEnabledPoolProxy(t *testing.T) {
	const enabledProxy = "http://enabled.example:8080"
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:   2,
		proxyPoolEnabled: true,
		proxyPool:        []string{enabledProxy},
	}
	store.BindSessionAffinity("enabled-proxy", store.accounts[0], enabledProxy)

	acc, proxyURL := store.NextForSession("enabled-proxy", 0, nil)
	if acc == nil {
		t.Fatal("expected bound account")
	}
	defer store.Release(acc)
	if proxyURL != enabledProxy {
		t.Fatalf("proxyURL = %q, want enabled proxy %q", proxyURL, enabledProxy)
	}
}

func TestBindSessionAffinityUsesConfigurableTTL(t *testing.T) {
	t.Setenv("CODEX_SESSION_AFFINITY_TTL", "2h")
	account := &Account{DBID: 1, AccessToken: "tok-1", ProxyURL: "http://proxy-1"}
	store := &Store{accounts: []*Account{account}}

	before := time.Now()
	store.bindSessionAffinity("session-ttl", account, "http://proxy-1")

	store.sessionMu.RLock()
	binding, ok := store.sessionBindings["session-ttl"]
	store.sessionMu.RUnlock()
	if !ok {
		t.Fatal("expected session binding")
	}
	if binding.expiresAt.Before(before.Add(2*time.Hour - time.Second)) {
		t.Fatalf("expiresAt too early: got %s want about 2h from now", binding.expiresAt)
	}
}

func TestNextForContinuationPreservesBoundedAffinity(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	fallback.SetSchedulerPriority(20)
	store := &Store{
		accounts:       []*Account{bound, fallback},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("conversation-1", bound, "")

	store.sessionMu.Lock()
	binding := store.sessionBindings["conversation-1"]
	binding.boundAt = time.Now().Add(-defaultMaxAffinityDuration - time.Minute)
	binding.requestCount = defaultMaxAffinityRequests
	store.sessionBindings["conversation-1"] = binding
	store.sessionMu.Unlock()

	acc, _ := store.NextForContinuationWithFilter("conversation-1", 0, nil, nil)
	if acc == nil || acc.DBID != bound.DBID {
		t.Fatalf("continuation account = %#v, want bound account %d", acc, bound.DBID)
	}
	store.Release(acc)

	acc, _ = store.NextForContinuationWithFilter("conversation-1", 0, map[int64]bool{bound.DBID: true}, nil)
	if acc != nil {
		store.Release(acc)
		t.Fatalf("continuation fell through to account %d when bound account was excluded", acc.DBID)
	}

	acc, _ = store.NextForSessionWithFilter("conversation-1", 0, nil, nil)
	if acc == nil || acc.DBID != fallback.DBID {
		t.Fatalf("ordinary bounded request account = %#v, want fallback account %d", acc, fallback.DBID)
	}
	store.Release(acc)
}

func TestNextForSessionFallsBackWhenBoundAccountExcluded(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 0, map[int64]bool{2: true})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionWithFilterFallsBackWhenBoundAccountRejected(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", PlanType: "pro"},
			{DBID: 2, AccessToken: "tok-2", PlanType: "plus", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	acc, proxyURL := store.NextForSessionWithFilter("session-1", 0, nil, func(acc *Account) bool {
		return acc.GetPlanType() == "pro"
	})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNoAffinitySplitGroupKeepsSessionStickyWithinTargetGroup(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "primary", GroupIDs: []int64{10}},
			{DBID: 2, AccessToken: "split-a", GroupIDs: []int64{20}},
			{DBID: 3, AccessToken: "split-b", GroupIDs: []int64{20}},
		},
		maxConcurrency: 1,
	}
	store.SetAPIKeyAllowedGroups(7, []int64{10})
	store.SetAPIKeyNoAffinityGroups(7, []int64{20})
	splitGroups := map[int64]struct{}{20: {}}
	splitFilter := func(account *Account) bool { return account.InAnyGroup(splitGroups) }

	first, _ := store.NextForSessionWithFilter("fallback-session::api-key:7", 7, nil, splitFilter)
	if first == nil {
		t.Fatal("expected an account from the no-affinity split group")
	}
	if first.DBID == 1 || !first.InAnyGroup(splitGroups) {
		t.Fatalf("first request selected account %d outside split group", first.DBID)
	}
	first.mu.Lock()
	first.ProxyURL = "http://split-proxy"
	first.mu.Unlock()
	store.BindSessionAffinity("fallback-session::api-key:7", first, "http://split-proxy")
	store.Release(first)

	second, proxyURL := store.NextForSessionWithFilter("fallback-session::api-key:7", 7, nil, splitFilter)
	if second == nil {
		t.Fatal("expected the bound split-group account on the next request")
	}
	defer store.Release(second)
	if second.DBID != first.DBID {
		t.Fatalf("split-group affinity moved from account %d to %d", first.DBID, second.DBID)
	}
	if proxyURL != "http://split-proxy" {
		t.Fatalf("sticky proxy = %q, want %q", proxyURL, "http://split-proxy")
	}
}

func TestNextForSessionFallsBackWhenBoundAccountIsError(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2", Status: StatusError, ErrorMsg: "deactivated_workspace"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
	if store.accounts[1].IsAvailable() {
		t.Fatal("error account should not be available for scheduling")
	}
}

func TestWaitForSessionAvailableReturnsBoundAccount(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected bound account")
	}
	if acc.DBID != 2 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 2)
	}
	if proxyURL != "http://proxy-2" {
		t.Fatalf("proxyURL = %q, want %q", proxyURL, "http://proxy-2")
	}
}

func TestWaitForSessionAvailableFallsBackWhenBindingExpired(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency:  1,
		sessionBindings: map[string]sessionAffinity{"session-1": {accountID: 99, proxyURL: "http://stale", expiresAt: time.Now().Add(-time.Minute)}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestWaitForSessionAvailableRespectsExcludeSet(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, 0, map[int64]bool{2: true})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestWaitForSessionAvailableReturnsImmediatelyWhenNoDispatchCandidate(t *testing.T) {
	store := &Store{
		accounts:       []*Account{},
		maxConcurrency: 1,
	}

	start := time.Now()
	acc, proxyURL := store.WaitForSessionAvailable(context.Background(), "", 2*time.Second, 0, nil)
	elapsed := time.Since(start)

	if acc != nil {
		t.Fatalf("account = %+v, want nil", acc)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty", proxyURL)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("WaitForSessionAvailable took %s with no dispatch candidates; want fast failure", elapsed)
	}
}

func TestWaitForSessionAvailableKeepsWaitingWhenCandidateIsBusy(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{
		accounts:       []*Account{account},
		maxConcurrency: 1,
	}
	atomic.StoreInt64(&account.ActiveRequests, 1)

	go func() {
		time.Sleep(75 * time.Millisecond)
		store.Release(account)
	}()

	acc, proxyURL := store.WaitForSessionAvailable(context.Background(), "", 500*time.Millisecond, 0, nil)
	if acc == nil {
		t.Fatal("expected busy candidate to become available")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty", proxyURL)
	}
}

func TestWaitForSessionAvailableVariantsRespectContextCancellation(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{accounts: []*Account{account}, maxConcurrency: 1}
	atomic.StoreInt64(&account.ActiveRequests, 1)
	tests := []struct {
		name string
		wait func(context.Context) (*Account, string)
	}{
		{name: "base", wait: func(ctx context.Context) (*Account, string) {
			return store.WaitForSessionAvailable(ctx, "", time.Hour, 0, nil)
		}},
		{name: "filter", wait: func(ctx context.Context) (*Account, string) {
			return store.WaitForSessionAvailableWithFilter(ctx, "", time.Hour, 0, nil, nil)
		}},
		{name: "dispatch", wait: func(ctx context.Context) (*Account, string) {
			return store.WaitForSessionAvailableWithDispatch(ctx, "", time.Hour, 0, nil, nil, DispatchPolicy(0))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			selected, _ := tc.wait(ctx)
			if selected != nil {
				t.Fatalf("selected busy account %+v", selected)
			}
			if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
				t.Fatalf("wait ignored context cancellation for %s", elapsed)
			}
		})
	}
}

func TestUnbindSessionAffinityRemovesMatchingBinding(t *testing.T) {
	boundAccount := &Account{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2", Disabled: 1}
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			boundAccount,
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", boundAccount, "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	store.UnbindSessionAffinity("session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 0, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionFallsBackWhenAPIKeyNotAllowed(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2", AllowedAPIKeyIDs: []int64{2}, allowedAPIKeySet: map[int64]struct{}{2: {}}},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 1, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}

func TestNextForSessionFallsBackWhenAPIKeyGroupNotAllowed(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1", GroupIDs: []int64{20}},
			{DBID: 2, AccessToken: "tok-2", ProxyURL: "http://proxy-2", GroupIDs: []int64{10}},
		},
		maxConcurrency: 2,
	}
	store.SetAPIKeyAllowedGroups(1, []int64{20})
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")
	requireSessionBinding(t, store, "session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", 1, nil)
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("account DBID = %d, want %d", acc.DBID, 1)
	}
	if proxyURL != "" {
		t.Fatalf("proxyURL = %q, want empty fallback proxy", proxyURL)
	}
}
