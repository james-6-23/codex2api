package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func readUsageDiagnosticSnapshot(test *testing.T, input *database.UsageLogInput) usageRequestDiagnostics {
	test.Helper()
	var snapshot usageRequestDiagnostics
	if err := json.Unmarshal([]byte(input.RequestDiagnostics), &snapshot); err != nil {
		test.Fatal(err)
	}
	return snapshot
}

func TestUsageRequestDiagnosticsClassification(test *testing.T) {
	for _, item := range []struct {
		name       string
		resolution *usageRequestResolution
		compact    bool
		internal   string
		want       string
	}{
		{name: "no evidence", want: "unknown"},
		{name: "fingerprint alone", resolution: &usageRequestResolution{Fingerprint: true}, want: "unknown"},
		{name: "user", resolution: &usageRequestResolution{ThreadSource: "user", RootState: "resolved"}, want: "user"},
		{name: "related", resolution: &usageRequestResolution{Passive: true, Related: true}, want: "related_internal"},
		{name: "independent", resolution: &usageRequestResolution{Passive: true}, want: "independent_internal"},
		{name: "unauthorized related", resolution: &usageRequestResolution{Related: true, ThreadSource: "guardian_review"}, want: "related_unclassified"},
		{name: "compact before resolution", compact: true, want: "compaction"},
		{name: "compact metadata", resolution: &usageRequestResolution{RequestKind: "compaction", Passive: true}, want: "compaction"},
		{name: "gateway", internal: "continuation", want: "gateway_internal"},
	} {
		test.Run(item.name, func(test *testing.T) {
			requestContext := promptSessionLimitTestContext("")
			usageRequestDiagnosticState(requestContext).Resolved = item.resolution
			input := &database.UsageLogInput{Compact: item.compact, InternalReason: item.internal}
			populateUsageRequestDiagnostics(requestContext, input)
			if input.RequestType != item.want {
				test.Fatalf("type = %q, want %q", input.RequestType, item.want)
			}
			snapshot := readUsageDiagnosticSnapshot(test, input)
			if snapshot.CompletedAt.Before(snapshot.StartedAt) || snapshot.CorrelationID == "" {
				test.Fatalf("missing request timing or correlation: %+v", snapshot)
			}
		})
	}
}

func TestUsageRequestDiagnosticsIngressPrivacyAndBounds(test *testing.T) {
	requestContext := promptSessionLimitTestContext("private-session-name")
	requestContext.Request.Header.Set("Authorization", "Bearer sk-secret-authorization")
	requestContext.Request.Header.Set("X-API-Key", "sk-secret-api-key")
	requestContext.Request.Header.Add("Session-Id", testRootSessionA)
	requestContext.Request.Header.Set("X-Codex-Turn-Metadata", `{"thread_source":"guardian_review","session_id":"`+testRootSessionA+`","prompt":"private-header-prompt"}`)
	body := []byte(`{"input":"private-user-prompt","client_metadata":{"thread_id":"private-thread-name","parent_thread_id":false,"x-codex-parent-thread-id":"` + testRootSessionA + `","subagent_kind":"sk-secret-in-metadata","x-codex-turn-metadata":"{\"thread_source\":\"subagent\",\"request_kind\":\"turn\",\"thread_id\":\"` + testLeafSessionA + `\"}"}}`)
	captureUsageRequestIngress(requestContext, body)
	captureUsageRequestIngress(requestContext, []byte(`{"client_metadata":{"thread_source":"must-not-overwrite-ingress"}}`))
	input := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(requestContext, input)
	for _, secret := range []string{"private-session-name", "sk-secret-authorization", "sk-secret-api-key", "private-header-prompt", "private-user-prompt", "private-thread-name", "sk-secret-in-metadata", "must-not-overwrite-ingress"} {
		if strings.Contains(input.RequestDiagnostics, secret) {
			test.Fatalf("diagnostics leaked %q", secret)
		}
	}
	snapshot := readUsageDiagnosticSnapshot(test, input)
	if snapshot.Incoming["client_metadata.x-codex-turn-metadata"]["thread_id"] != testLeafSessionA || snapshot.Incoming["client_metadata"]["parent_thread_id"] != "invalid_type" || snapshot.Incoming["headers"]["Session-Id_multiple"] != "true" {
		test.Fatalf("missing source fields: %+v", snapshot.Incoming)
	}
	if diagnosticIdentifier(testLeafSessionA+":5") != testLeafSessionA+":5" {
		test.Fatal("valid window identifier was not retained")
	}
	state := usageRequestDiagnosticState(requestContext)
	state.Incoming["oversize"] = map[string]string{"value": strings.Repeat("x", 20000)}
	populateUsageRequestDiagnostics(requestContext, input)
	snapshot = readUsageDiagnosticSnapshot(test, input)
	if len(input.RequestDiagnostics) > database.MaxUsageRequestDiagnosticsBytes || !snapshot.Truncated || snapshot.Incoming != nil {
		test.Fatal("snapshot size was not bounded")
	}
}

func TestUsageRequestDiagnosticsStageRetryAndWSIsolation(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	requestContext := promptSessionLimitTestContext(testRootSessionA)
	state := usageRequestDiagnosticState(requestContext)
	state.Resolved = &usageRequestResolution{Passive: false, RootState: "resolved", ThreadSource: "user"}
	handler.recordUsageAuthorization(requestContext, "audit")
	setPassiveInternalAuthorization(requestContext, true)
	handler.recordUsageAuthorization(requestContext, "dispatch")
	state.UserWindow = "created"
	state.AccountWindow = "admitted"
	state.Recent = usageRecentAccountDiagnostic{Result: "selected", AccountID: 41, ObservedAt: time.Now()}
	beginDispatchSelection(requestContext)
	selectionTraceForRequest(requestContext).Reject("concurrency")
	input := &database.UsageLogInput{AccountID: 41, AttemptIndex: 1}
	populateUsageRequestDiagnostics(requestContext, input)
	firstJSON := input.RequestDiagnostics
	first := readUsageDiagnosticSnapshot(test, input)
	if !first.ClassificationChanged || first.Selection != "recent_account" || len(first.CandidateRejections) != 1 || input.RequestType != "independent_internal" {
		test.Fatalf("missing actual dispatch snapshot: %+v", first)
	}
	beginUsageSelectionAttempt(requestContext, 2)
	selectionTraceForRequest(requestContext).Reset()
	if state.UserWindow != "not_reached" || state.AccountWindow != "not_reached" || state.Recent.AccountID != 0 || !state.Recent.ObservedAt.IsZero() {
		test.Fatalf("retry retained stale decisions: %+v", state)
	}
	if input.RequestDiagnostics != firstJSON {
		test.Fatal("saved snapshot changed with mutable context")
	}
	second := &database.UsageLogInput{AccountID: 42}
	populateUsageRequestDiagnostics(requestContext, second)
	if snapshot := readUsageDiagnosticSnapshot(test, second); snapshot.Attempt != 2 || snapshot.Selection != "scheduled" || len(snapshot.CandidateRejections) != 0 {
		test.Fatalf("invalid retry: %+v", snapshot)
	}
	resetPromptPolicyRequestCorrelationID(requestContext)
	resetCodexInternalRequestClassificationFrame(requestContext)
	captureUsageRequestIngress(requestContext, []byte(`{"client_metadata":{"thread_source":"user"}}`))
	fresh := usageRequestDiagnosticState(requestContext)
	if fresh == state || fresh.Resolved != nil || fresh.Dispatch != nil || fresh.Recent.AccountID != 0 || fresh.CorrelationID == first.CorrelationID || passiveInternalRequestAuthorized(requestContext) {
		test.Fatalf("WS frame retained previous state: %+v", fresh)
	}
}

func TestUsageRequestDiagnosticsRecentSelectionDecisions(test *testing.T) {
	for _, outcome := range []string{"missing", "expired", "after_request_start", "account_unavailable", "selected"} {
		test.Run(outcome, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			account := &auth.Account{DBID: 17, AccessToken: "test-token", Status: auth.StatusReady}
			handler.store.AddAccount(account)
			body := []byte(`{"model":"gpt-5.6-sol","input":"background"}`)
			requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "user", RequestKind: "turn"})
			handler.primeNewAPIPolicyContext(requestContext, body)
			identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
			if !identity.unlinkedFallbackOnly || identity.unlinkedFallbackScope == "" {
				test.Fatalf("not a rootless request: %+v", identity)
			}
			observed := time.Now().Add(-time.Second)
			if outcome == "expired" {
				observed = time.Now().Add(-time.Hour)
			} else if outcome == "after_request_start" {
				observed = time.Now().Add(time.Hour)
			}
			if outcome != "missing" {
				payload, err := json.Marshal(unlinkedFallbackRuntimeRecord{AccountID: account.ID(), ObservedAt: observed})
				if err != nil {
					test.Fatal(err)
				}
				if err := handler.cache.SetRuntime(test.Context(), unlinkedFallbackRuntimeNamespace, identity.unlinkedFallbackScope, payload, time.Minute); err != nil {
					test.Fatal(err)
				}
			}
			excluded := map[int64]bool{17: outcome == "account_unavailable"}
			selected, _ := handler.takeUnlinkedRecentAccount(requestContext, identity, 101, excluded, nil, auth.DispatchPolicyStandard)
			if selected != nil {
				handler.store.Release(selected)
			}
			if got := usageRequestDiagnosticState(requestContext).Recent.Result; got != outcome {
				test.Fatalf("recent result = %q, want %q", got, outcome)
			}
		})
	}
}

func TestUsageRequestDiagnosticsWindowDecisions(test *testing.T) {
	handler := &Handler{}
	config := promptfilter.Config{}
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationLimit = 1
	config.Advanced.Risk.SessionCreationLimitWindowSeconds = 300
	for index, sessionID := range []string{testRootSessionA, testRootSessionA, testLeafSessionA} {
		requestContext := promptSessionLimitTestContext(sessionID)
		_, rejected := handler.checkPromptSessionCreationLimit(requestContext, config, nil)
		want := []string{"created", "reused", "limit_rejected"}[index]
		state := usageRequestDiagnosticState(requestContext)
		if state.UserWindow != want || rejected != (index == 2) || state.UserWindowKeyHash == "" {
			test.Fatalf("window %d = %+v, rejected=%v", index, state, rejected)
		}
	}
}

func BenchmarkUsageRequestDiagnostics(bench *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	for _, size := range []int{32768, 1048576} {
		bench.Run(fmt.Sprintf("body_%d", size), func(bench *testing.B) {
			body := []byte(`{"input":"` + strings.Repeat("x", size) + `","client_metadata":{"session_id":"` + testRootSessionA + `","thread_source":"user","request_kind":"turn"}}`)
			requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			requestContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			handler := &Handler{}
			bench.ReportAllocs()
			bench.ResetTimer()
			for iteration := 0; iteration < bench.N; iteration++ {
				resetPromptPolicyRequestCorrelationID(requestContext)
				resetCodexInternalRequestClassificationFrame(requestContext)
				handler.captureUsageRequestResolution(requestContext, body, requestSessionIdentity{stableIdentity: true}, requestRootSessionIdentity{sessionID: testRootSessionA, stable: true, threadSource: "user"}, verifiedNewAPIPolicyContext{}, "unsigned")
				handler.recordUsageAuthorization(requestContext, "audit")
				handler.recordUsageAuthorization(requestContext, "dispatch")
				input := &database.UsageLogInput{AccountID: 17}
				populateUsageRequestDiagnostics(requestContext, input)
				if input.RequestDiagnostics == "" {
					bench.Fatal("no snapshot")
				}
			}
		})
	}
}
