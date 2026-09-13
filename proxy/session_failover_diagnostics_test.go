package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionFailoverBlockedDiagnosticSeparatesTriggerAndContext(test *testing.T) {
	for _, scenario := range []struct {
		path, value, reason string
	}{
		{"previous_response_id", `"old-response"`, "upstream_continuation"},
		{"client_metadata.x-codex-turn-state", `"old-state"`, "connection_turn_state"},
		{"input", `[{"type":"compaction","encrypted_content":"secret-context"}]`, "opaque_upstream_context"},
		{"input", `[{"type":"function_call_output","call_id":"missing","output":"result"}]`, "incomplete_tool_context"},
		{"input", `[]`, "missing_request_context"},
	} {
		test.Run(scenario.reason, func(test *testing.T) {
			handler, owner, _, key := failoverTestSetup(test, true)
			atomic.StoreInt32(&owner.Disabled, 1)
			request, body := failoverTestRequest(test, handler)
			body, err := sjson.SetRawBytes(body, scenario.path, []byte(scenario.value))
			require.NoError(test, err)
			finish := handler.beginServiceErrorAudit(request)
			failure := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			if scenario.path != "input" {
				require.Nil(test, failure)
				diagnostic := usageRequestDiagnosticState(request).AccountFailover
				require.Equal(test, "pending", diagnostic.Result)
				require.Equal(test, "account_disabled", diagnostic.TriggerReason)
				require.NotEmpty(test, diagnostic.ContextCleanup.Removed)
				finish()
				return
			}
			require.NotNil(test, failure)
			require.Equal(test, "codex_session_failover_context_required", string(failure.Code))
			require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(failure.Code))
			details, err := json.Marshal(failure.Details)
			require.NoError(test, err)
			require.Equal(test, "missing_request_context", gjson.GetBytes(details, "reason").String())
			require.Equal(test, "lossy_restart", gjson.GetBytes(details, "context_cleanup.mode").String())
			require.Equal(test, "account_disabled", gjson.GetBytes(details, "trigger_reason").String())
			require.Equal(test, "before_switch", gjson.GetBytes(details, "phase").String())
			require.Equal(test, "false", request.Writer.Header().Get("X-Should-Retry"))
			api.SendError(request, failure)
			finish()
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			event := page.Items[0]
			require.Equal(test, http.StatusBadRequest, event.StatusCode)
			require.NotNil(test, event.AccountFailover)
			require.Equal(test, "missing_request_context", event.AccountFailover.BlockReason)
			require.Equal(test, "account_disabled", event.AccountFailover.TriggerReason)
			require.Equal(test, owner.ID(), event.AccountFailover.PreviousAccountID)
			require.Equal(test, "not_started", gjson.GetBytes(event.UpstreamInfo, "transport").String())
			require.NotContains(test, string(details), "secret-context")
			record, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, owner.ID(), record.AccountID)
		})
	}
}

func TestSessionFailoverRestoredContextFailureIsLoggedBeforeContinuityExists(test *testing.T) {
	handler, old, current, key := failoverTestSetup(test, true)
	_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: old.ID(), AccountID: current.ID(), Reason: "account_usage_exhausted"})
	require.NoError(test, err)
	request, body := failoverTestRequest(test, handler)
	body, err = sjson.SetBytes(body, "previous_response_id", "old-response")
	require.NoError(test, err)
	finish := handler.beginServiceErrorAudit(request)
	failure := handler.restoreMigratedSessionOwner(request, key, body)
	require.NotNil(test, failure)
	require.Nil(test, usageRequestDiagnosticState(request).Continuity)
	api.SendError(request, failure)
	finish()
	page := serviceErrorTestPage(test, handler)
	require.Len(test, page.Items, 1)
	diagnostic := page.Items[0].AccountFailover
	require.NotNil(test, diagnostic)
	require.Equal(test, "after_switch", diagnostic.Phase)
	require.Equal(test, "upstream_continuation", diagnostic.BlockReason)
	require.Equal(test, "account_usage_exhausted", diagnostic.TriggerReason)
	require.Equal(test, old.ID(), diagnostic.PreviousAccountID)
	require.Equal(test, current.ID(), diagnostic.AccountID)
	require.Equal(test, uint64(1), diagnostic.Generation)
}

func TestSessionFailoverContextHTTPErrorIncludesActionableDetails(test *testing.T) {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	failure := sessionFailoverContextError(request, &sessionAccountFailoverDiagnostic{Phase: "before_switch", TriggerReason: "account_session_capacity_full"}, "persistent_owner_required")
	api.SendError(request, failure)
	require.Equal(test, http.StatusBadRequest, request.Writer.Status())
	require.Contains(test, failure.Message, "持久化账号归属")
}
