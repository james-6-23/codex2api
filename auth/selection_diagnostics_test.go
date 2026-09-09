package auth

import (
	"reflect"
	"testing"
	"time"
)

func TestSelectionDiagnosticsCaptureObservedRejections(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		configure func(*Account)
		reason    string
		retry     string
	}{
		{"disabled", func(account *Account) { account.Disabled = 1 }, "account_disabled", "stop"},
		{"paused", func(account *Account) { account.DispatchPaused = 1 }, "account_paused", "stop"},
		{"cooldown", func(account *Account) {
			account.Status = StatusCooldown
			account.CooldownUtil = time.Now().Add(time.Hour)
		}, "account_cooldown", "backoff_same_route"},
		{"credentials", func(account *Account) { account.AccessToken = "" }, "credential_unavailable", "stop"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			store, account := newSessionCapacityTestStore(1)
			scenario.configure(account)
			trace := &SelectionTrace{}
			selected := store.TakePreferredAccountWithDispatch(account.DBID, 0, nil, nil, DispatchPolicyStandard, trace)
			if selected != nil {
				store.Release(selected)
				test.Fatal("rejected account was selected")
			}
			diagnostic := trace.Snapshot()
			if diagnostic.Reason != scenario.reason || diagnostic.Retry != scenario.retry || diagnostic.Incomplete {
				test.Fatalf("unexpected diagnostic: %+v", diagnostic)
			}
			trace.Freeze()
			account.Disabled = 1
			account.IsAvailable(trace)
			if !reflect.DeepEqual(diagnostic, trace.Snapshot()) {
				test.Fatal("post-failure probe changed the diagnosis")
			}
		})
	}
}

func TestSelectionDiagnosticsPreserveKnownRoot(test *testing.T) {
	store, root := newSessionCapacityTestStore(1)
	root.SessionCapacityEnabled = false
	store.accounts = append(store.accounts, &Account{DBID: 2, AccessToken: "backup"})
	store.BindSessionAffinity("root", root, "")
	root.Disabled = 1
	trace := &SelectionTrace{}
	selected, _, _ := store.NextForSessionWithDispatchGuard(RelatedSessionAffinityKey("root"), 0, nil, nil, DispatchPolicyStandard, trace)
	if selected != nil {
		store.Release(selected)
		test.Fatal("diagnostic collection migrated a related request")
	}
	diagnostic := trace.Snapshot()
	if diagnostic.RootAccount != root.DBID || diagnostic.Reason != "root_owner_unavailable" || !reflect.DeepEqual(diagnostic.Reasons, []string{"account_disabled"}) {
		test.Fatalf("root rejection not captured: %+v", diagnostic)
	}
	trace.Reset()
	selected, _, _ = store.NextForSessionWithDispatchGuard("root", 0, nil, nil, DispatchPolicyStandard, trace)
	if selected == nil || selected.DBID != 2 {
		test.Fatal("ordinary root lost its existing fallback behavior")
	}
	store.Release(selected)
}

func TestSelectionDiagnosticsPartialMixedAndReset(test *testing.T) {
	trace := &SelectionTrace{}
	resume := trace.Pause()
	trace.Filter("shadow_only", func(*Account) bool { return false })(nil)
	resume()
	trace.Reject("affinity_group_mismatch")
	trace.Reject("account_cooldown")
	if diagnostic := trace.Snapshot(); diagnostic.Reason != "mixed_constraints" || len(diagnostic.Reasons) != 2 {
		test.Fatalf("shadow probe polluted actual constraints: %+v", diagnostic)
	}
	trace.Reject("indexed_candidates_unavailable")
	if diagnostic := trace.Snapshot(); !diagnostic.Incomplete || diagnostic.Retry != "default" {
		test.Fatalf("indexed miss claimed a complete diagnosis: %+v", diagnostic)
	}
	trace.Bind(42)
	trace.Freeze()
	trace.Reset()
	if diagnostic := trace.Snapshot(); !diagnostic.Incomplete || len(diagnostic.Reasons) != 0 || diagnostic.RootAccount != 0 {
		test.Fatalf("previous attempt survived reset: %+v", diagnostic)
	}
}
