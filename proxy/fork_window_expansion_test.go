package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestUserForkQuotesExpansionAgainstParentAccountAndChildKey(test *testing.T) {
	for _, consent := range []bool{false, true} {
		test.Run(map[bool]string{false: "without_consent", true: "with_consent"}[consent], func(test *testing.T) {
			previous := CurrentRuntimeSettings()
			test.Cleanup(func() { ApplyRuntimeSettings(previous) })
			settings := DefaultRuntimeSettings()
			settings.CodexSessionFailoverEnabled = true
			ApplyRuntimeSettings(settings)
			handler := newWindowAuthorizationHandler(test)
			owner := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, SessionCapacityEnabled: true, SessionCapacityMax: 2, SessionCapacityReserved: 1, SessionCapacityIdleTTLSeconds: 60}
			handler.store.AddAccount(owner)
			require.True(test, handler.store.AdmitAccountSession(owner, "occupied", time.Now()))
			parent := promptSessionTestFingerprint("fork-parent")
			child := promptSessionTestFingerprint("fork-child")
			sourceKey := sessionAffinityKey("newapi-root-session:"+parent, 101)
			_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(sourceKey), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true, Number: 55})
			require.NoError(test, err)
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: child, ForkedFromSessionFingerprint: parent, ThreadSource: "user", RequestKind: "turn"}
			body, err := json.Marshal(windowControlRequest{Operation: "quote", AllowExpansion: consent, ExtraLimit: 2, Multiplier: 1.5, ReservationID: "fork-request"})
			require.NoError(test, err)
			control, recorder := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
			handler.ControlNewAPIUserWindows(control)
			if !consent {
				require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				return
			}
			require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
			var result struct {
				Ticket string `json:"ticket"`
			}
			require.NoError(test, json.Unmarshal(recorder.Body.Bytes(), &result))
			grant, err := decodeWindowGrant("integration-secret", result.Ticket)
			require.NoError(test, err)
			require.True(test, grant.Grant.Expanded)
			require.Equal(test, 1.5, grant.Grant.Multiplier)
			require.Equal(test, owner.ID(), grant.Grant.OwnerAccountID)
			childKey := sessionAffinityKey("newapi-root-session:"+child, grant.APIKeyID)
			require.Equal(test, childKey, grant.Grant.OwnerKey)
			require.NotEqual(test, sourceKey, childKey)
			seed, requestBody := continuityTestRequest(55, "turn")
			meta.WindowGrant = result.Ticket
			request, _ := windowExpansionTestContext(test, "/v1/responses", requestBody, meta)
			request.Set(ingressRequestBodyContextKey, requestBody)
			handler.primeNewAPIPolicyContext(request, requestBody)
			usageRequestDiagnosticState(request).Resolved = usageRequestDiagnosticState(seed).Resolved
			beginDispatchSelection(request)
			require.Nil(test, handler.requestWindowGrantError(request))
			status, policy := handler.cachedNewAPIPolicyAuditState(request)
			require.Equal(test, "verified", status)
			require.True(test, policy.MetaVerified)
			require.NotNil(test, windowGrantForRequest(request))
			require.True(test, selectionTraceForRequest(request).ExpandedWindow())
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: "newapi-root-session:" + parent}, childKey, "gpt-5.6-sol", "gpt-5.6-sol", false, requestBody))
			require.Equal(test, owner.ID(), selectionTraceForRequest(request).PinnedAccount())
			require.Nil(test, usageRequestDiagnosticState(request).AccountFailover)
			require.Nil(test, handler.commitSessionContinuity(request, owner))
			record, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(childKey))
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, owner.ID(), record.AccountID)
		})
	}
}

func TestUserForkWindowNeverTreatsPassiveRequestsAsPaidRoots(test *testing.T) {
	base := newAPIPolicyMeta{ThreadSource: "user", RequestKind: "turn", RootSessionFingerprint: "child", ForkedFromSessionFingerprint: "parent"}
	require.True(test, userForkWindow(base))
	for _, change := range []func(*newAPIPolicyMeta){
		func(meta *newAPIPolicyMeta) { meta.RequestKind = "compaction" },
		func(meta *newAPIPolicyMeta) { meta.ThreadSource = "thread_title" },
		func(meta *newAPIPolicyMeta) { meta.SubagentKind = "guardian" },
		func(meta *newAPIPolicyMeta) { meta.PassiveFeature = newAPIPassiveFeatureRelatedInternal },
		func(meta *newAPIPolicyMeta) { meta.SessionAccounting = newAPISessionAccountingBypass },
		func(meta *newAPIPolicyMeta) { meta.ForkedFromSessionFingerprint = "" },
		func(meta *newAPIPolicyMeta) { meta.ForkedFromSessionFingerprint = meta.RootSessionFingerprint },
	} {
		meta := base
		change(&meta)
		require.False(test, userForkWindow(meta))
	}
}
