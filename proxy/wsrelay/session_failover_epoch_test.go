package wsrelay

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestWebsocketSessionFailoverResetsWindowAndConnection(test *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	test.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexSessionFailoverEnabled, settings.CodexForceWebsocket = true, true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "epoch.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	memory := cache.NewMemory(100)
	test.Cleanup(func() { require.NoError(test, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	test.Cleanup(store.Stop)
	first := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "first-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	second := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "second-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice, Disabled: 1}
	store.AddAccount(first)
	store.AddAccount(second)
	handler := proxy.NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(memory)
	type capture struct {
		headers    http.Header
		body       []byte
		connection int32
	}
	seen := make(chan capture, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		index := connections.Add(1)
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			seen <- capture{request.Header.Clone(), body, index}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"epoch-response","status":"completed","output":[{"type":"reasoning","id":"epoch-reasoning","encrypted_content":"gAAAAepoch-state-%d"}],"usage":{"input_tokens":1,"output_tokens":1}}}`, index))); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "epoch-ws"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	const root = "01a09351-7b81-7ae0-afd0-225e178ea131"
	var captures []capture
	const originalTurn = "01a095b5-86a3-7ec2-af42-0bb1111ef330"
	var mappedTurns []string
	for number := 0; number < 5; number++ {
		if number == 2 {
			atomic.StoreInt32(&first.Disabled, 1)
			atomic.StoreInt32(&second.Disabled, 0)
		}
		if number == 4 {
			atomic.StoreInt32(&first.Disabled, 0)
			atomic.StoreInt32(&second.Disabled, 1)
		}
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":"full plaintext context","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:%d","window_number":%d}}}`, root, root, root, root, root, number, number))
		for _, field := range []string{"turn_id", "root_turn_id"} {
			body, err = sjson.SetBytes(body, "client_metadata."+field, originalTurn)
			require.NoError(test, err)
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata."+field, originalTurn)
			require.NoError(test, err)
		}
		if number == 1 || number == 3 {
			body, err = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"type":"reasoning","id":"epoch-reasoning","encrypted_content":"gAAAAepoch-state-%d"},{"role":"user","content":"continue"}]`, number/2+1)))
			require.NoError(test, err)
		}
		if number == 2 || number == 4 {
			body, err = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"type":"reasoning","encrypted_content":"gAAAAepoch-state-%d"},{"type":"compaction","encrypted_content":"gAAAAold-compaction"},{"role":"user","content":[{"type":"input_file","file_id":"old-file"},{"type":"input_text","text":"current task"}]}]`, number/2)))
			require.NoError(test, err)
		}
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Request.Header.Set("Authorization", "Bearer test-user-key")
		ctx, cancel := context.WithTimeout(request.Request.Context(), 5*time.Second)
		request.Request = request.Request.WithContext(ctx)
		handler.Responses(request)
		cancel()
		require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Contains(test, recorder.Body.String(), "response.completed")
		select {
		case sent := <-seen:
			captures = append(captures, sent)
			if number == 2 || number == 4 {
				require.NotContains(test, string(sent.body), "gAAAAepoch-state-")
				require.NotContains(test, string(sent.body), "gAAAAold-compaction")
				require.NotContains(test, string(sent.body), "old-file")
				require.Contains(test, string(sent.body), "current task")
			}
			if number == 3 {
				require.Contains(test, string(sent.body), "gAAAAepoch-state-2")
			}
			meta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
			require.EqualValues(test, number%2, meta.Get("window_number").Uint())
			require.Equal(test, sent.headers.Get("Thread-Id")+fmt.Sprintf(":%d", number%2), meta.Get("window_id").String())
			require.Equal(test, sent.headers.Get("Session-Id"), meta.Get("session_id").String())
			mappedTurn := meta.Get("turn_id").String()
			require.NotEqual(test, originalTurn, mappedTurn)
			require.Equal(test, mappedTurn, meta.Get("root_turn_id").String())
			require.Equal(test, mappedTurn, gjson.GetBytes(sent.body, "client_metadata.turn_id").String())
			require.Equal(test, mappedTurn, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "turn_id").String())
			mappedTurns = append(mappedTurns, mappedTurn)
			require.False(test, gjson.GetBytes(sent.body, "account_mapping").Exists())
			require.Equal(test, originalTurn, gjson.GetBytes(body, "client_metadata.turn_id").String())
		case <-time.After(time.Second):
			test.Fatal("upstream frame missing")
		}
	}
	require.Equal(test, captures[0].connection, captures[1].connection)
	require.Equal(test, captures[2].connection, captures[3].connection)
	require.NotEqual(test, captures[0].connection, captures[2].connection)
	require.NotEqual(test, captures[0].connection, captures[4].connection)
	require.NotEqual(test, captures[0].headers.Get("Session-Id"), captures[4].headers.Get("Session-Id"))
	require.EqualValues(test, 3, connections.Load())
	require.Equal(test, mappedTurns[0], mappedTurns[1])
	require.Equal(test, mappedTurns[2], mappedTurns[3])
	require.NotEqual(test, mappedTurns[0], mappedTurns[2])
	require.NotEqual(test, mappedTurns[0], mappedTurns[4])
}
