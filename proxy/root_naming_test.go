package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestRootNamingUsesFinalRootAndDoesNotAffectOtherBackgroundTypes(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	body := []byte(`{"model":"gpt-5.6-sol","input":"title"}`)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: promptSessionTestFingerprint("main"), ThreadSource: "thread_title", RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal}
	first, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(first, body)
	account := &auth.Account{DBID: 1535}
	status, blocked := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(first, body, account, "", 1535)
	require.False(test, blocked, "%+v", status)
	require.Equal(test, "claimed", usageRequestDiagnosticState(first).Naming)
	require.Nil(test, handler.claimRequestRootNaming(first, body), "one logical request may retry its transport")
	for _, source := range []string{"thread_title", "thread_title_reconsideration", "thread_description"} {
		meta.ThreadSource = source
		meta.OriginalRootFingerprint = promptSessionTestFingerprint("another-title-thread-" + source)
		duplicate, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
		handler.primeNewAPIPolicyContext(duplicate, body)
		status, blocked = handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(duplicate, body, account, "", 1535)
		require.True(test, blocked)
		require.Equal(test, api.ErrorCode("codex_root_already_named"), status.AdmissionCode)
		require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(status.AdmissionCode))
	}
	for _, source := range []string{"user", "ambient_suggestions", "guardian_review"} {
		meta.ThreadSource = source
		request, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
		handler.primeNewAPIPolicyContext(request, body)
		require.Nil(test, handler.claimRequestRootNaming(request, body), source)
	}
	meta.ThreadSource, meta.RootSessionFingerprint = "thread_title", promptSessionTestFingerprint("other-main")
	other, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(other, body)
	require.Nil(test, handler.claimRequestRootNaming(other, body))
}

func TestRootNamingWaitDoesNotConsumeClaimAndDuplicateNeverReachesUpstream(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_title","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"title"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	account := &auth.Account{DBID: 1535, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test", Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(account)
	body := []byte(`{"model":"gpt-5.6-sol","input":"title","stream":false}`)
	remaining := int64(0)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: promptSessionTestFingerprint("main"), ThreadSource: "thread_title", RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining}
	waiting, recorder := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.Responses(waiting)
	require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(test, recorder.Body.String(), "codex_root_account_wait_timeout")
	require.Zero(test, calls.Load())
	meta.RootAccountWaitMillis = nil
	first, recorder := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(first, body)
	identity := handler.resolveRequestSessionIdentityForContext(first, body)
	rootKey, related := auth.RelatedSessionRootKey(capacityAwareSessionAffinityKey(identity, 101))
	require.True(test, related)
	handler.store.BindSessionAffinity(rootKey, account, "")
	handler.Responses(first)
	require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
	require.EqualValues(test, 1, calls.Load())
	meta.OriginalRootFingerprint = promptSessionTestFingerprint("another-naming-thread")
	duplicate, recorder := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.Responses(duplicate)
	require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	require.Contains(test, recorder.Body.String(), "codex_root_already_named")
	require.EqualValues(test, 1, calls.Load())
}
