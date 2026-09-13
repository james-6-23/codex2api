package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionContextRestartRemovesOnlyOutgoingDependencies(test *testing.T) {
	body := []byte(`{"input":[
{"type":"reasoning","encrypted_content":"old-reasoning","id":"old-item"},
{"type":"compaction","encrypted_content":"old-compaction"},
{"type":"item_reference","id":"old-reference"},
{"role":"user","content":[{"type":"input_file","file_id":"old-file"},{"type":"input_file","file_id":"old-file-inline","file_data":"data:text/plain;base64,Zm9v"},{"type":"input_text","text":"Keep current task"},{"type":"input_image","image_url":"data:image/png;base64,Zm9v"}]},
{"type":"function_call","id":"old-call-item","call_id":"paired","name":"tool","arguments":"{}"},
{"type":"function_call_output","call_id":"paired","output":"literal {\"file_id\":\"keep-text\"}"},
{"type":"function_call_output","call_id":"orphan","output":"old output"},
{"type":"reasoning","id":"current-item","encrypted_content":"current-cipher"},
{"type":"item_reference","id":"current-item"}],
"previous_response_id":"old-response","conversation":"old-conversation","client_metadata":{"x-codex-turn-state":"old-state","turn_id":"original-turn","large_number":9007199254740993}}`)
	original := bytes.Clone(body)
	headers := http.Header{"X-Codex-Turn-State": {"old-state"}, "Session-Id": {"original-session"}}
	known := func(kind, value string) bool { return value == "current-cipher" || value == "current-item" }
	cleaned, outgoing, report, err := cleanSessionRestartContext(headers, body, known)
	require.NoError(test, err)
	require.Equal(test, original, body)
	require.Equal(test, "old-state", headers.Get("X-Codex-Turn-State"))
	require.Empty(test, outgoing.Get("X-Codex-Turn-State"))
	require.Equal(test, "original-session", outgoing.Get("Session-Id"))
	for _, old := range []string{"old-reasoning", "old-compaction", "old-reference", "old-file", "old-call-item", "old-response", "old-conversation", "old-state", "old output"} {
		require.NotContains(test, string(cleaned), old)
	}
	for _, kept := range []string{"current-cipher", "current-item", "Keep current task", "keep-text", "paired", "data:text/plain;base64,Zm9v", "data:image/png;base64,Zm9v", "original-turn", "9007199254740993"} {
		require.Contains(test, string(cleaned), kept)
	}
	require.Len(test, gjson.GetBytes(cleaned, "input").Array(), 5)
	require.Equal(test, 1, report.Removed["reasoning"])
	require.Equal(test, 1, report.Removed["compaction"])
	require.Equal(test, 2, report.Removed["file_reference"])
	repeated, _, secondReport, err := cleanSessionRestartContext(outgoing, cleaned, known)
	require.NoError(test, err)
	require.JSONEq(test, string(cleaned), string(repeated))
	require.Empty(test, secondReport.Removed)
	encoded, err := json.Marshal(report)
	require.NoError(test, err)
	require.NotContains(test, string(encoded), "old-")
	require.NotContains(test, string(encoded), "current-")
}

func TestSessionContextRestartAllItemsAndEmptyInput(test *testing.T) {
	body := []byte(`{"input":[` + strings.Repeat(`{"type":"reasoning","encrypted_content":"private"},`, 20) + `{"role":"user","content":"current"}]}`)
	cleaned, _, report, err := cleanSessionRestartContext(nil, body, nil)
	require.NoError(test, err)
	require.Equal(test, 20, report.Removed["reasoning"])
	require.Len(test, gjson.GetBytes(cleaned, "input").Array(), 1)
	_, _, _, err = cleanSessionRestartContext(nil, []byte(`{"input":[{"type":"compaction","encrypted_content":"only-history"}]}`), nil)
	require.ErrorContains(test, err, "没有可用输入")
	require.NotNil(test, codexIdentityRequestError(err))
	_, _, _, err = cleanSessionRestartContext(nil, []byte(`{"previous_response_id":"unknown"}`), nil)
	require.Error(test, err)
	_, _, _, err = cleanSessionRestartContext(nil, []byte(`{"previous_response_id":"known"}`), func(kind, value string) bool { return kind == "previous_response_id" && value == "known" })
	require.NoError(test, err)
}

func TestSessionContextRestartFailoverRestoresAndKeepsNewProvenance(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(test, handler)
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"old-reasoning"},{"type":"compaction","encrypted_content":"old-compaction"},{"role":"user","content":"current task"}]`))
	body, _ = sjson.SetBytes(body, "previous_response_id", "old-response")
	request.Request.Header.Set("X-Codex-Turn-State", "old-turn")
	original := bytes.Clone(body)
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	routing, routingHeaders := sessionRestartRoutingContext(request, body)
	require.NotContains(test, string(routing), "old-")
	require.Empty(test, routingHeaders.Get("X-Codex-Turn-State"))
	before, _, err := PrepareSessionRestartOutbound(request.Request.Context(), owner, body, request.Request.Header)
	require.NoError(test, err)
	require.Equal(test, original, before)
	selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.True(test, handled)
	require.Same(test, target, selected)
	handler.store.Release(selected)
	require.Nil(test, handler.commitSessionContinuity(request, target))
	cleaned, headers, err := PrepareSessionRestartOutbound(request.Request.Context(), target, body, request.Request.Header)
	require.NoError(test, err)
	require.NotContains(test, string(cleaned), "old-")
	require.Empty(test, headers.Get("X-Codex-Turn-State"))
	require.Equal(test, original, body)
	require.Equal(test, "outbound", usageRequestDiagnosticState(request).AccountFailover.ContextCleanup.Phase)
	require.Equal(test, 1, usageRequestDiagnosticState(request).AccountFailover.ContextCleanup.Removed["compaction"])
	handler.recordResponseContextProvenance(request, target, []byte(`{"output":[{"type":"reasoning","id":"new-item","encrypted_content":"new-reasoning"}]}`))
	settings := CurrentRuntimeSettings()
	settings.CodexSessionFailoverEnabled = false
	ApplyRuntimeSettings(settings)
	handler.continuityRecords = nil
	originalCache := handler.cache
	test.Cleanup(func() { handler.cache = originalCache })
	handler.cache = nil
	resumed, _ := failoverTestRequest(test, handler)
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"reasoning","id":"new-item","encrypted_content":"new-reasoning"}`))
	require.Nil(test, handler.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	cleaned, _, err = PrepareSessionRestartOutbound(resumed.Request.Context(), target, body, resumed.Request.Header)
	require.NoError(test, err)
	require.Contains(test, string(cleaned), "new-reasoning")
	require.Contains(test, string(cleaned), "new-item")
	require.NotContains(test, string(cleaned), "old-")
	_, _, err = PrepareSessionRestartOutbound(resumed.Request.Context(), owner, body, resumed.Request.Header)
	require.Error(test, err)
}
