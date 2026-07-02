package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestDispatchCountLimitMarksRateLimitedAtThreshold(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestModel: "gpt-5.4"})
	resetAt := time.Now().Add(2 * time.Hour)
	account := &Account{DBID: 1, AccessToken: "at", Status: StatusReady, PlanType: "free"}
	account.SetReset7dAt(resetAt)
	account.SetDispatchCountLimit(2)
	store.AddAccount(account)

	first := store.Next()
	if first == nil {
		t.Fatal("first Next() returned nil")
	}
	store.Release(first)
	if got := account.RuntimeStatus(); got == "rate_limited" {
		t.Fatal("account entered rate_limited before reaching dispatch count limit")
	}

	second := store.Next()
	if second == nil {
		t.Fatal("second Next() returned nil")
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() after threshold = %q, want rate_limited", got)
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != "rate_limited" {
		t.Fatalf("cooldown reason = %q, want rate_limited", reason)
	}
	if until.Before(resetAt.Add(-time.Second)) || until.After(resetAt.Add(time.Second)) {
		t.Fatalf("cooldown until = %s, want about resetAt %s", until, resetAt)
	}
	store.Release(second)

	if third := store.Next(); third != nil {
		store.Release(third)
		t.Fatal("third Next() returned an account after dispatch count limit")
	}
	snapshot := account.GetDispatchCountSnapshot()
	if snapshot.Limit != 2 || snapshot.Used != 2 || !snapshot.Limited {
		t.Fatalf("dispatch snapshot = %#v, want limit=2 used=2 limited=true", snapshot)
	}
}

func TestDispatchCountLimitResetsAfterWindow(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestModel: "gpt-5.4"})
	resetAt := time.Now().Add(40 * time.Millisecond)
	account := &Account{DBID: 2, AccessToken: "at", Status: StatusReady, PlanType: "free"}
	account.SetReset7dAt(resetAt)
	account.SetDispatchCountLimit(1)
	store.AddAccount(account)

	first := store.Next()
	if first == nil {
		t.Fatal("first Next() returned nil")
	}
	store.Release(first)
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() after first dispatch = %q, want rate_limited", got)
	}

	time.Sleep(80 * time.Millisecond)
	second := store.Next()
	if second == nil {
		t.Fatal("Next() returned nil after dispatch count window reset")
	}
	store.Release(second)
}

func TestFastSchedulerUsesDispatchCountLimit(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestModel: "gpt-5.4"})
	store.SetFastSchedulerEnabled(true)
	account := &Account{DBID: 3, AccessToken: "at", Status: StatusReady, PlanType: "free"}
	account.SetReset7dAt(time.Now().Add(time.Hour))
	account.SetDispatchCountLimit(1)
	store.AddAccount(account)

	first := store.Next()
	if first == nil {
		t.Fatal("first fast-scheduler Next() returned nil")
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() after fast-scheduler threshold = %q, want rate_limited", got)
	}
	store.Release(first)

	if second := store.Next(); second != nil {
		store.Release(second)
		t.Fatal("fast scheduler returned account after dispatch count limit")
	}
}
