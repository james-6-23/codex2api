package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestSessionFailurePinPreventsCooldownFallthroughAndClearsOnSuccess(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	primary := &Account{DBID: 1, AccessToken: "a", PlanType: "plus"}
	secondary := &Account{DBID: 2, AccessToken: "b", PlanType: "plus"}
	store.AddAccount(primary)
	store.AddAccount(secondary)
	const key = "failure-pinned-session"
	store.BindSessionAffinity(key, primary, "")
	store.PinSessionAffinityAfterTransientFailure(key, primary.ID())

	primary.Mu().Lock()
	primary.Status = StatusCooldown
	primary.CooldownUtil = time.Now().Add(time.Hour)
	primary.Mu().Unlock()
	if got, _ := store.NextForSession(key, 0, nil); got != nil {
		store.Release(got)
		t.Fatalf("failure-pinned cooldown fell through to account %d", got.ID())
	}
	if boundID, ok := store.SessionAffinityAccountID(key); !ok || boundID != primary.ID() {
		t.Fatalf("binding after cooldown=%v/%d, want primary=%d", ok, boundID, primary.ID())
	}

	primary.Mu().Lock()
	primary.Status = StatusReady
	primary.CooldownUtil = time.Time{}
	primary.Mu().Unlock()
	got, _ := store.NextForSession(key, 0, nil)
	if got != primary {
		if got != nil {
			store.Release(got)
		}
		t.Fatalf("recovered account=%v, want primary", got)
	}
	store.ReleaseForSession(got, key)
	store.sessionMu.RLock()
	pinned := store.sessionBindings[key].failurePinned
	store.sessionMu.RUnlock()
	if pinned {
		t.Fatal("successful release did not clear the transient failure pin")
	}
}

func TestNewStoreRespectsZeroRetrySetting(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if got := store.GetMaxRetries(); got != 0 {
		t.Fatalf("GetMaxRetries() = %d, want 0", got)
	}
}
