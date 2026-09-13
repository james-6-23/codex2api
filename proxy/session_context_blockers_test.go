package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/api"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionContextBlockersIdentifyFieldsWithoutValues(test *testing.T) {
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"private-compaction"},{"type":"reasoning","encrypted_content":"private-reasoning"},{"type":"message","content":[{"type":"input_file","file_id":"private-file"}]},{"type":"item_reference","id":"private-item"},{"private-key":{"type":"private-type","encrypted_content":"private-value"}}]}`)
	reason, blockers := inspectSessionFailoverContext(http.Header{}, body, nil)
	require.Equal(test, "opaque_upstream_context", reason)
	require.Len(test, blockers, 5)
	for index, path := range []string{"input[0].encrypted_content", "input[1].encrypted_content", "input[2].content[0].file_id", "input[3].id", "input[4].[field].encrypted_content"} {
		require.Equal(test, path, blockers[index].Path)
	}
	require.Equal(test, "compaction", blockers[0].ItemType)
	require.Equal(test, "reasoning", blockers[1].ItemType)
	encoded, err := json.Marshal(blockers)
	require.NoError(test, err)
	require.NotContains(test, string(encoded), "private")
	reason, blockers = inspectSessionFailoverContext(http.Header{}, body, func(string, string) bool { return true })
	require.Empty(test, reason)
	require.Empty(test, blockers)
	repeated := []byte(`{"input":[` + strings.TrimSuffix(strings.Repeat(`{"file_id":"secret"},`, 20), ",") + `]}`)
	_, blockers = inspectSessionFailoverContext(http.Header{}, repeated, nil)
	require.Len(test, blockers, 8)
}

func TestSessionContextBlockersReachClientUsageAndServiceErrors(test *testing.T) {
	handler, owner, _, key := failoverTestSetup(test, true)
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(test, handler)
	body, err := sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"private-value"}]`))
	require.NoError(test, err)
	finish := handler.beginServiceErrorAudit(request)
	failure := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
	require.NotNil(test, failure)
	require.Contains(test, failure.Message, "清理旧账号上下文后没有可用输入")
	require.NotContains(test, failure.Message, "private-value")
	details, err := json.Marshal(failure.Details)
	require.NoError(test, err)
	require.EqualValues(test, 1, gjson.GetBytes(details, "context_cleanup.removed.reasoning").Int())
	require.Equal(test, 1, usageRequestDiagnosticState(request).AccountFailover.ContextCleanup.Removed["reasoning"])
	api.SendError(request, failure)
	finish()
	page := serviceErrorTestPage(test, handler)
	require.Len(test, page.Items, 1)
	require.Equal(test, 1, page.Items[0].AccountFailover.ContextCleanup.Removed["reasoning"])
}

func TestUnboundNonzeroMessageUsesRequestedWording(test *testing.T) {
	require.Equal(test, "当前请求来自已有上下文窗口，请新开对话后重试。", sessionContinuityError("unbound_nonzero").Message)
}
