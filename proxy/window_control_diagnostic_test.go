package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestWindowControlAuditExplainsContinuationRejection(test *testing.T) {
	for _, scenario := range []struct {
		name, grantState, accountReason    string
		missingAccount, expiredReservation bool
	}{
		{name: "expired authorization and missing slot", grantState: "expired", accountReason: "session_capacity_full"},
		{name: "expired reservation", grantState: "reservation_expired", accountReason: "session_capacity_full", expiredReservation: true},
		{name: "binding points to missing account", grantState: "expired", accountReason: "account_unavailable", missingAccount: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			grant := quoteWindowAuthorization(test, handler, "private-root", "private-reservation", "user")
			now := time.Now().UTC()
			account := &auth.Account{DBID: 1695, AccessToken: "private-account-token", SessionCapacityEnabled: true, SessionCapacityMax: 2, SessionCapacityReserved: 1, SessionCapacityIdleTTLSeconds: 60}
			if !scenario.missingAccount {
				handler.store.AddAccount(account)
				require.True(test, handler.store.AdmitAccountSession(account, "another-private-session", now))
			}
			key := sessionAffinityKey("newapi-root-session:"+grant.Fingerprint, grant.APIKeyID)
			_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: account.ID(), LastSeen: now.Add(-2 * time.Minute), LastCompleted: now.Add(-90 * time.Second), LastStatus: 200})
			require.NoError(test, err)
			subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
			require.NoError(test, handler.db.UpdateUserWindowAdmissions(context.Background(), subject, func(state *database.UserWindowAdmissionState) error {
				stored := state.Windows[grant.Grant.Root]
				stored.ID, stored.OwnerKey, stored.OwnerAccountID = "private-grant-id", key, account.ID()
				stored.Confirmed = !scenario.expiredReservation
				stored.PendingUntil = now.Add(-time.Minute)
				if !scenario.expiredReservation {
					stored.ExpiresAt = now.Add(-time.Second)
				}
				return nil
			}))
			body := []byte(`{"operation":"quote","allow_expansion":false,"multiplier":1.5,"extra_limit":2,"reservation_id":"private-reservation"}`)
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: grant.Fingerprint, ThreadSource: "user", RequestKind: "turn"}
			request, response := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
			finish := handler.beginServiceErrorAudit(request)
			handler.ControlNewAPIUserWindows(request)
			finish()
			require.Equal(test, http.StatusBadRequest, response.Code, response.Body.String())
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			event := page.Items[0]
			require.Equal(test, "window_admission_failed", event.Code)
			diagnostic := event.WindowControl
			require.NotNil(test, diagnostic)
			require.Equal(test, grant.Grant.Root, diagnostic.RootHash)
			require.Equal(test, "continuity", diagnostic.OwnerSource)
			require.Equal(test, account.ID(), diagnostic.OwnerAccountID)
			require.Equal(test, 200, diagnostic.OwnerLastStatus)
			require.True(test, diagnostic.OwnerLastCompleted.Equal(now.Add(-90*time.Second)))
			require.Equal(test, scenario.grantState, diagnostic.Grant.State)
			require.Equal(test, scenario.accountReason, diagnostic.Account.Reason)
			require.Equal(test, "owner_admission_rejected", diagnostic.Decision)
			require.Equal(test, "not_authorized", diagnostic.ExpansionBlock)
			require.True(test, diagnostic.CountsEvaluated)
			require.Zero(test, diagnostic.OrdinaryUsed, "user quota is free even though the bound account rejects admission")
			if !scenario.missingAccount {
				require.Equal(test, "missing", diagnostic.Account.SlotState)
				require.EqualValues(test, 1, diagnostic.Account.TotalUsed)
				require.EqualValues(test, 1, diagnostic.Account.ReservedLimit)
			}
			encoded, err := json.Marshal(event)
			require.NoError(test, err)
			for _, secret := range []string{"private-root", "private-reservation", "private-grant-id", "private-account-token", "another-private-session", key, grant.Fingerprint} {
				require.NotContains(test, string(encoded), secret)
			}
		})
	}
}

func TestWindowControlContinuesExistingAccountSlotAtCapacity(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	grant := quoteWindowAuthorization(test, handler, "existing-root", "first", "user")
	account := &auth.Account{DBID: 1695, AccessToken: "test", SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60}
	handler.store.AddAccount(account)
	key := sessionAffinityKey("newapi-root-session:"+grant.Fingerprint, grant.APIKeyID)
	require.True(test, handler.store.AdmitAccountSession(account, key, time.Now()))
	subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
	require.NoError(test, handler.db.UpdateUserWindowAdmissions(context.Background(), subject, func(state *database.UserWindowAdmissionState) error {
		state.Windows[grant.Grant.Root].ExpiresAt = time.Now().Add(-time.Second)
		return nil
	}))
	body := []byte(`{"operation":"quote","allow_expansion":false,"multiplier":1,"extra_limit":0}`)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: grant.Fingerprint, ThreadSource: "user", RequestKind: "turn"}
	request, response := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
	handler.ControlNewAPIUserWindows(request)
	require.Equal(test, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(test, json.Unmarshal(response.Body.Bytes(), &result))
	renewed, err := decodeWindowGrant("integration-secret", result.Ticket)
	require.NoError(test, err)
	require.False(test, renewed.Grant.Expanded)
	require.Equal(test, 1.0, renewed.Grant.Multiplier)
	require.Equal(test, "existing_slot", windowControlDiagnostic(request).Account.Reason)
}

func TestWindowControlAuditDistinguishesUserQuotaAndExpansionPolicy(test *testing.T) {
	for _, scenario := range []struct {
		name, body, block string
	}{
		{"not authorized", `{"operation":"quote","allow_expansion":false,"multiplier":1.5,"extra_limit":2}`, "not_authorized"},
		{"no extra quota", `{"operation":"quote","allow_expansion":true,"multiplier":1.5,"extra_limit":0}`, "extra_limit_exhausted"},
		{"ordinary multiplier", `{"operation":"quote","allow_expansion":true,"multiplier":1,"extra_limit":2}`, "multiplier_not_expanded"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			quoteWindowAuthorization(test, handler, "first-root", "first", "user")
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: promptSessionTestFingerprint("second-root"), ThreadSource: "user", RequestKind: "turn"}
			request, response := windowExpansionTestContext(test, "/v1/session-windows", []byte(scenario.body), meta)
			finish := handler.beginServiceErrorAudit(request)
			handler.ControlNewAPIUserWindows(request)
			finish()
			require.Equal(test, http.StatusBadRequest, response.Code, response.Body.String())
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			diagnostic := page.Items[0].WindowControl
			require.NotNil(test, diagnostic)
			require.Equal(test, "user_window_limit", diagnostic.Decision)
			require.Equal(test, scenario.block, diagnostic.ExpansionBlock)
			require.True(test, diagnostic.CountsEvaluated)
			require.Equal(test, 1, diagnostic.OrdinaryUsed)
			require.Equal(test, "missing", diagnostic.Grant.State)
			require.Nil(test, diagnostic.Account, "user limit must not be attributed to an unobserved account")
		})
	}
}
