package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionContextProvenanceIsScopedToOwnerAccountAndGeneration(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	record, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread})
	require.NoError(test, err)
	producer, _ := outboundEpochTestRequest(test, handler, 0)
	handler.attachSessionOutboundEpoch(producer, hashRiskIdentity(key), record)
	payload := []byte(`{"type":"response.completed","response":{"output":[{"type":"reasoning","id":"new-reasoning","encrypted_content":"current-reasoning"},{"type":"compaction","id":"new-compaction","encrypted_content":"current-compaction"}],"input":[{"type":"reasoning","encrypted_content":"echoed-input"}]}}`)
	handler.recordResponseContextProvenance(producer, target, payload)
	recordSessionTurnState(producer.Request.Context(), target, "current-turn-state")
	for _, scenario := range []string{"reasoning", "compaction", "item_reference", "header_state", "frame_state", "unknown", "echoed_input", "other_user", "other_root", "other_account", "return_to_account", "cache_unavailable", "legacy_provenance", "disabled_setting"} {
		test.Run(scenario, func(test *testing.T) {
			request, body := outboundEpochTestRequest(test, handler, 1)
			body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","id":"new-reasoning","encrypted_content":"current-reasoning"},{"role":"user","content":"continue"}]`))
			require.NoError(test, err)
			expected := true
			checked, rootKey := record, key
			switch scenario {
			case "compaction":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"current-compaction"}]`))
			case "item_reference":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"item_reference","id":"new-reasoning"}]`))
			case "header_state":
				request.Request.Header.Set("X-Codex-Turn-State", "current-turn-state")
			case "frame_state":
				body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "current-turn-state")
			case "unknown", "echoed_input", "legacy_provenance":
				expected = false
				cipher := map[string]string{"unknown": "unknown", "echoed_input": "echoed-input", "legacy_provenance": "legacy-account-only"}[scenario]
				require.NoError(test, handler.recordCompactionProvenance(test.Context(), target, cipher))
				body, _ = sjson.SetBytes(body, "input.0.encrypted_content", cipher)
			case "other_user":
				expected = false
				request.Request.Header.Set("Authorization", "Bearer another-user-key")
			case "other_root":
				expected, rootKey = false, key+"-different"
			case "other_account":
				expected, checked.AccountID = false, owner.ID()
			case "return_to_account":
				expected, checked.FailoverCount = false, record.FailoverCount+2
			case "cache_unavailable":
				previous := handler.cache
				test.Cleanup(func() { handler.cache = previous })
				handler.cache = nil
			case "disabled_setting":
				previous := CurrentRuntimeSettings()
				test.Cleanup(func() { ApplyRuntimeSettings(previous) })
				settings := previous
				settings.CodexSessionFailoverEnabled = false
				ApplyRuntimeSettings(settings)
			}
			original := bytes.Clone(body)
			failure := handler.validateMigratedSessionContext(request, body, checked, rootKey)
			require.Equal(test, expected, failure == nil, "%v", failure)
			require.Equal(test, original, body)
			require.NotEmpty(test, sessionFailoverContextBlock(request.Request.Header, body))
		})
	}
}

func TestSessionContextProvenanceCurrentCompactionHTTPFlow(test *testing.T) {
	handler, owner, target, _ := failoverTestSetup(test, true)
	previous := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previous) })
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received.Add(1)
		if strings.HasSuffix(request.URL.Path, "/responses/compact") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"current-compact-response","output":[{"type":"compaction","id":"current-compact","encrypted_content":"current-compact-value"}]}`))
			return
		}
		stickyFailureSuccess(writer)
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "context-test"})
	atomic.StoreInt32(&target.Disabled, 1)
	for step := 0; step < 3; step++ {
		if step == 1 {
			atomic.StoreInt32(&owner.Disabled, 1)
			atomic.StoreInt32(&target.Disabled, 0)
		}
		_, body := failoverTestRequest(test, handler)
		body, _ = sjson.SetBytes(body, "stream", true)
		path := "/v1/responses"
		if step == 1 {
			path = "/v1/responses/compact"
			body, _ = sjson.SetBytes(body, "stream", false)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "compaction")
		}
		if step == 2 {
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","id":"current-compact","encrypted_content":"current-compact-value"},{"role":"user","content":"continue"}]`))
		}
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Request.Header.Set("Authorization", "Bearer test-user-key")
		request.Request.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(request.Request.Context(), 5*time.Second)
		request.Request = request.Request.WithContext(ctx)
		if step == 1 {
			handler.ResponsesCompact(request)
		} else {
			handler.Responses(request)
		}
		cancel()
		require.Equal(test, http.StatusOK, recorder.Code, "step=%d body=%s", step, recorder.Body.String())
	}
	require.EqualValues(test, 3, received.Load())
}

func TestSessionFailoverUnsentMainAccountGuardCoversCompactAndABA(test *testing.T) {
	for _, compact := range []bool{false, true} {
		test.Run(map[bool]string{false: "http", true: "compact"}[compact], func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			request, body := outboundEpochTestRequest(test, handler, 0)
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", compact, body))
			require.Nil(test, handler.commitSessionContinuity(request, owner))
			var captured atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { captured.Add(1) }))
			test.Cleanup(server.Close)
			previous := GetResinConfig()
			test.Cleanup(func() { SetResinConfig(previous) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "unsent-test"})
			store := &backgroundMatchRaceStore{CodexIdentityStore: handler.db, switchOwner: func() {
				for index, selected := range []*auth.Account{target, owner} {
					previous := owner
					if index == 1 {
						previous = target
					}
					_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: previous.ID(), AccountID: selected.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: continuityTestThread})
					require.NoError(test, err)
				}
			}}
			ctx := WithCodexIdentityStore(request.Request.Context(), store)
			var response *http.Response
			var err error
			if compact {
				response, err = ExecuteCompactRequest(ctx, owner, body, "cache", "", "test-user-key", nil, request.Request.Header)
			} else {
				response, err = ExecuteRequest(ctx, owner, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
			}
			require.Error(test, err)
			require.Nil(test, response)
			require.Zero(test, captured.Load())
		})
	}
}

func TestSessionContextProvenanceNativeWebsocketFlow(test *testing.T) {
	handler, owner, target, _ := failoverTestSetup(test, true)
	previous := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previous) })
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	var received atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		step := received.Add(1)
		body := readUpstreamRequestBody(request)
		if step > 1 {
			require.NotContains(test, string(body), "old-restart-")
		}
		if step == 3 {
			require.Contains(test, string(body), "gAAAAnative-ws-state")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-ws-response\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"native-ws-reasoning\",\"encrypted_content\":\"gAAAAnative-ws-state\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	test.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "native-context"})
	engine := gin.New()
	engine.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(engine)
	test.Cleanup(server.Close)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-user-key")
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	require.NoError(test, err)
	test.Cleanup(func() { _ = connection.Close() })
	atomic.StoreInt32(&target.Disabled, 1)
	for step := 0; step < 3; step++ {
		if step == 1 {
			atomic.StoreInt32(&owner.Disabled, 1)
			atomic.StoreInt32(&target.Disabled, 0)
		}
		_, body := failoverTestRequest(test, handler)
		body, _ = sjson.SetBytes(body, "type", "response.create")
		if step == 1 {
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"gAAAAold-restart-reasoning"},{"type":"compaction","encrypted_content":"gAAAAold-restart-compaction"},{"role":"user","content":"continue current task"}]`))
			body, _ = sjson.SetBytes(body, "previous_response_id", "old-restart-response")
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-restart-state")
		}
		if step == 2 {
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","id":"native-ws-reasoning","encrypted_content":"gAAAAnative-ws-state"},{"role":"user","content":"continue"}]`))
		}
		require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
		require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
		for {
			_, payload, err := connection.ReadMessage()
			require.NoError(test, err)
			kind := gjson.GetBytes(payload, "type").String()
			require.NotEqual(test, "error", kind, "step=%d payload=%s", step, payload)
			require.NotEqual(test, "response.failed", kind, "step=%d payload=%s", step, payload)
			if kind == "response.completed" {
				break
			}
		}
	}
	require.EqualValues(test, 3, received.Load())
}
