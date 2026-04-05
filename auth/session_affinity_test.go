package auth

import (
	"context"
	"testing"
	"time"
)

func TestNextForSessionPrefersBoundAccountAndProxy(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", nil)
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

func TestNextForSessionFallsBackWhenBoundAccountExcluded(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 2,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	acc, proxyURL := store.NextForSession("session-1", map[int64]bool{2: true})
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

func TestWaitForSessionAvailableReturnsBoundAccount(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, nil)
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

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, nil)
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
			{DBID: 2, AccessToken: "tok-2"},
		},
		maxConcurrency: 1,
	}
	store.bindSessionAffinity("session-1", store.accounts[1], "http://proxy-2")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	acc, proxyURL := store.WaitForSessionAvailable(ctx, "session-1", 50*time.Millisecond, map[int64]bool{2: true})
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

func TestUnbindSessionAffinityRemovesMatchingBinding(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			{DBID: 1, AccessToken: "tok-1"},
		},
		maxConcurrency: 1,
	}
	// 绑定一个不在 accounts 列表中的账号，unbind 后只能回退到 DBID=1
	store.bindSessionAffinity("session-1", &Account{DBID: 2, AccessToken: "tok-2"}, "http://proxy-2")

	store.UnbindSessionAffinity("session-1", 2)

	acc, proxyURL := store.NextForSession("session-1", nil)
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
