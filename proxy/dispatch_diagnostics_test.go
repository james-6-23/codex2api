package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestDispatchDiagnosticWireFixture(test *testing.T) {
	diagnostic := dispatchDiagnosticEnvelope{
		RequestID: "dispatch-fixture", ChannelID: 7, Status: 503, IssuedAt: 1788566400,
		SelectionDiagnostic: auth.SelectionDiagnostic{Stage: "account_selection", Reason: "root_owner_unavailable", Reasons: []string{"affinity_group_mismatch"}, RootAccount: 42, Retry: "stop"},
	}
	encoded, err := sealDispatchDiagnostic("integration-secret", "dispatch-fixture", "42", "test-platform", diagnostic, bytes.NewReader(bytes.Repeat([]byte{42}, 12)))
	if err != nil {
		test.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/dispatch_diagnostic_v1.txt")
	if err != nil {
		test.Fatal(err)
	}
	if encoded != strings.TrimSpace(string(fixture)) {
		test.Fatal("wire protocol differs from the shared receiver fixture")
	}
	diagnostic.Reasons = []string{"account_cooldown", "concurrency_exhausted"}
	second, err := sealDispatchDiagnostic("integration-secret", "dispatch-fixture", "42", "test-platform", diagnostic, bytes.NewReader(bytes.Repeat([]byte{43}, 12)))
	if err != nil || len(second) != len(encoded) || second == encoded {
		test.Fatal("diagnostics expose payload length or reuse ciphertext")
	}
	if _, err := sealDispatchDiagnostic("secret", "request", "42", "platform", diagnostic, bytes.NewReader(nil)); err == nil {
		test.Fatal("entropy failure was ignored")
	}
}

func TestDispatchUnavailablePublicAndTrustedTransports(test *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, protocol := range []string{"http", "responses_sse", "chat_sse", "ws"} {
			name := protocol
			if trusted {
				name += "_trusted"
			}
			test.Run(name, func(test *testing.T) {
				cfg := promptGuardTestConfig()
				cfg.Advanced.NewAPI.Enabled = true
				handler := newPromptGuardTestHandler(cfg)
				test.Cleanup(handler.store.Stop)
				body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
				ctx, recorder := signedNewAPIPolicyContext(test, "dispatch-test", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
				addSignedNewAPIPolicyMeta(test, ctx, newAPIPolicyMeta{ChannelID: 7, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce, Provider: "openai", Protocol: "responses"}, true)
				if trusted {
					handler.primeNewAPIPolicyContext(ctx, body)
				}
				beginDispatchSelection(ctx)
				selectionTraceForRequest(ctx).Reject("affinity_group_mismatch")
				selectionTraceForRequest(ctx).Bind(42)
				var output string
				if protocol == "ws" {
					payload, err := json.Marshal(handler.dispatchUnavailableAPIError(ctx))
					if err != nil {
						test.Fatal(err)
					}
					output = string(payload)
				} else {
					stream := protocol != "http"
					if stream {
						ctx.Writer.Header().Set("Content-Type", "text/event-stream")
						_, _ = ctx.Writer.WriteString(": ping\n\n")
					}
					handler.sendDispatchUnavailable(ctx, stream, protocol == "chat_sse")
					output = recorder.Body.String()
					if !stream && (recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("X-Request-ID") == "") {
						test.Fatal("missing status or correlation ID")
					}
				}
				for _, private := range []string{"affinity_group_mismatch", "root_owner_unavailable", "root_account", "integration-secret"} {
					if strings.Contains(output, private) || strings.Contains(recorder.Header().Get(dispatchDiagnosticHeader), private) {
						test.Fatalf("private diagnostic leaked: %s", private)
					}
				}
				if !strings.Contains(output, "service_unavailable") || !strings.Contains(output, "request_id") {
					test.Fatalf("missing safe public error: %s", output)
				}
				protected := strings.Contains(output, "v1.") || strings.HasPrefix(recorder.Header().Get(dispatchDiagnosticHeader), "v1.")
				if protected != trusted {
					test.Fatalf("trusted envelope boundary failed: trusted=%t protected=%t", trusted, protected)
				}
				if recorder.Header().Get("X-Codex2API-Policy-Violation") != "" {
					test.Fatal("dispatch failure became a policy violation")
				}
			})
		}
	}
}

func TestDispatchDiagnosticsModelAndGroupGateAttribution(test *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	beginDispatchSelection(ctx)
	var filter auth.AccountFilter = func(*auth.Account) bool { return false }
	trace := selectionTraceForRequest(ctx)
	filter = groupMembershipFilter(map[int64]struct{}{9: {}}, true, filter, trace)
	if filter(&auth.Account{DBID: 1}) {
		test.Fatal("group-mismatched account passed")
	}
	if diagnostic := trace.Snapshot(); diagnostic.Reason != "affinity_group_mismatch" {
		test.Fatalf("inner model gate blamed without evaluation: %+v", diagnostic)
	}
	var handler *Handler
	recorder := httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler.sendDispatchUnavailable(ctx, false, false)
	if gjson.GetBytes(recorder.Body.Bytes(), "error.code").String() != "service_unavailable" {
		test.Fatal("unbound diagnostic failed closed")
	}
}

func TestDispatchDiagnosticsDoNotBlameExemptPassiveModels(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	body := []byte(`{"model":"future-internal-model","input":"background"}`)
	ctx, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "future_internal_kind", RequestKind: "turn"})
	handler.primeNewAPIPolicyContext(ctx, body)
	identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
	if !identity.unlinkedFallbackOnly || !passiveInternalRequestAuthorized(ctx) {
		test.Fatal("test request did not acquire trusted rootless classification")
	}
	beginDispatchSelection(ctx)
	account := &auth.Account{DBID: 17, AccessToken: "token", Models: []string{"different-model"}}
	filter := handler.applyPassiveInternalModelRouting(ctx, "future-internal-model", identity, "", true, accountFilterForModel("future-internal-model"))
	if !filter(account) {
		test.Fatal("diagnostic instrumentation disabled the passive exemption")
	}
	if diagnostic := selectionTraceForRequest(ctx).Snapshot(); len(diagnostic.Reasons) != 0 {
		test.Fatalf("exempt model was blamed: %+v", diagnostic)
	}
}

func TestDispatchDiagnosticsReachHTTPFailureFromActualSelection(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	handler.store.SetSchedulerEngine("legacy")
	handler.store.AddAccount(&auth.Account{DBID: 17, AccessToken: "token", Status: auth.StatusReady, Disabled: 1})
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	ctx, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{ChannelID: 7, RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "future_internal_kind", RequestKind: "turn"})
	handler.Responses(ctx)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get(dispatchDiagnosticHeader) == "" {
		test.Fatalf("missing protected final dispatch failure: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if diagnostic := selectionTraceForRequest(ctx).Snapshot(); diagnostic.Reason != "account_disabled" {
		test.Fatalf("handler lost the actual selection reason: %+v", diagnostic)
	}
	if strings.Contains(recorder.Body.String(), "account_disabled") {
		test.Fatal("actual selection reason leaked to client")
	}
}
