package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountSessionAdmissionDiagnosticPreservesDecisionAndExpiry(test *testing.T) {
	for _, scenario := range []struct {
		name, state, reason string
		allowed             bool
	}{
		{"existing at capacity", "active", "existing_slot", true},
		{"expired at capacity", "expired", "session_capacity_full", false},
		{"missing at capacity", "missing", "session_capacity_full", false},
		{"upgrade in progress", "active", "upgrade_pending", false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			store, account := newSessionCapacityTestStore(1)
			now := time.Now().UTC()
			require.True(test, store.AdmitAccountSession(account, "root", now))
			switch scenario.state {
			case "expired":
				store.accountSessions[account.ID()]["root"].lastSeen = now.Add(-time.Minute)
				store.accountSessions[account.ID()]["other"] = &accountSessionState{lastSeen: now}
			case "missing":
				delete(store.accountSessions[account.ID()], "root")
				store.accountSessions[account.ID()]["other"] = &accountSessionState{lastSeen: now}
			}
			if scenario.reason == "upgrade_pending" {
				store.accountSessions[account.ID()]["root"].pendingUpgrade = true
			}
			allowed, diagnostic := store.CanAdmitAccountSessionWithDiagnostic(account, "root", now)
			require.Equal(test, scenario.allowed, allowed)
			require.Equal(test, scenario.reason, diagnostic.Reason)
			require.Equal(test, scenario.state, diagnostic.SlotState)
			require.EqualValues(test, 1, diagnostic.TotalLimit)
			require.EqualValues(test, 1, diagnostic.TotalUsed)
			require.EqualValues(test, 60, diagnostic.IdleTTLSeconds)
			if scenario.state == "expired" {
				require.Equal(test, now, diagnostic.ExpiresAt)
				require.NotContains(test, store.accountSessions[account.ID()], "root")
			}
			require.Equal(test, allowed, store.CanAdmitAccountSession(account, "root", now))
		})
	}
}

func TestAccountSessionAdmissionDiagnosticDistinguishesMissingAccountAndReservedCapacity(test *testing.T) {
	store, account := newSessionCapacityTestStore(2)
	allowed, missing := store.CanAdmitAccountSessionWithDiagnostic(nil, "root", time.Now())
	require.False(test, allowed)
	require.Equal(test, "account_unavailable", missing.Reason)
	require.Equal(test, "not_checked", missing.SlotState)
	account.SessionCapacityReserved = 1
	now := time.Now()
	require.True(test, store.AdmitAccountSession(account, "ordinary", now))
	allowed, diagnostic := store.CanAdmitAccountSessionWithDiagnostic(account, "new-root", now)
	require.False(test, allowed)
	require.EqualValues(test, 2, diagnostic.TotalLimit)
	require.EqualValues(test, 1, diagnostic.ReservedLimit)
	require.EqualValues(test, 1, diagnostic.TotalUsed)
	require.Zero(test, diagnostic.ReservedUsed)
	require.Equal(test, "session_capacity_full", diagnostic.Reason)
}
