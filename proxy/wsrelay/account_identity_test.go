package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWebsocketAccountIdentityReuseFailureHTTPAndAccountBoundary(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	previousResin, previousRuntime, previousExecutor := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings(), proxy.WebsocketExecuteFunc
	test.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 8)
	var connections, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !websocket.IsWebSocketUpgrade(request) {
			body, _ := io.ReadAll(request.Body)
			received <- capture{request.Header.Clone(), body}
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"response-http"}`))
			return
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		defer connection.Close()
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			sequence := requests.Add(1)
			received <- capture{request.Header.Clone(), body}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"alias_response_%d","status":"completed","output":[]}}`, sequence)
			if sequence == 1 {
				terminal = `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"overloaded"}}}`
			}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "account-identity"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	var usedConnections []*WsConnection
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		usedConnections = append(usedConnections, response.conn)
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "ws.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 15*time.Second)
	defer cancel()
	first := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice, DynamicConcurrencyLimit: 1,
		CustomHeaders: map[string]string{"Session-Id": "bad-session", "Thread-Id": "bad-thread"}}
	second := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "other-test-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice, DynamicConcurrencyLimit: 1}
	const root = "01a09302-49f4-7b53-b545-91ef29610317"
	headers := http.Header{"Session-Id": {root}, "Thread-Id": {root}, "Originator": {"codex-tui"}}
	var firstSession, firstCache, firstTurn string
	for index := 0; index < 4; index++ {
		account := first
		if index == 3 {
			account = second
		}
		originalTurn := fmt.Sprintf("01a095b5-86a3-7ec2-af42-%012x", index+1)
		body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[],"client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","context_window_id":"01a09302-49f4-7b53-b545-91fb553a57b4","window_id":"%s:%d","window_number":%d,"turn_id":"%s","request_kind":"turn","thread_source":"user"}}}`, root, root, root, root, root, index, index, originalTurn))
		response, err := proxy.ExecuteRequest(ctx, account, body, "cache", "", "test-key", nil, headers, index != 2)
		require.NoError(test, err)
		output, err := io.ReadAll(response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		if index == 0 {
			require.Contains(test, string(output), "server_is_overloaded")
		}
		var sent capture
		select {
		case sent = <-received:
		case <-ctx.Done():
			test.Fatal("upstream request not observed")
		}
		session := sent.headers.Get("Session-Id")
		cache := gjson.GetBytes(sent.body, "prompt_cache_key").String()
		if index == 0 {
			firstSession, firstCache = session, cache
		}
		if index < 3 {
			require.Equal(test, firstSession, session)
			require.Equal(test, firstCache, cache)
		} else {
			require.NotEqual(test, firstSession, session)
			require.NotEqual(test, firstCache, cache)
		}
		require.NotEqual(test, root, session)
		require.NotEqual(test, root[:13], session[:13])
		require.Equal(test, session, sent.headers.Get("Thread-Id"))
		require.Equal(test, session, sent.headers.Get("X-Client-Request-Id"))
		require.Equal(test, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
		require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
		metadata := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String()
		require.Equal(test, session, gjson.Get(metadata, "thread_id").String())
		require.Equal(test, fmt.Sprintf("%s:%d", session, index), gjson.Get(metadata, "window_id").String())
		mappedTurn := gjson.Get(metadata, "turn_id").String()
		require.NotEqual(test, originalTurn, mappedTurn)
		require.NotEqual(test, originalTurn[:13], mappedTurn[:13])
		if index == 0 {
			firstTurn = mappedTurn
		}
		if index == 1 {
			require.Equal(test, firstSession+":0", sent.headers.Get("X-Codex-Window-Id"))
			require.Equal(test, firstTurn, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "turn_id").String())
			require.NotEqual(test, firstTurn, mappedTurn)
		} else {
			require.Equal(test, mappedTurn, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "turn_id").String())
		}
	}
	require.EqualValues(test, 2, connections.Load())
	require.Same(test, usedConnections[0], usedConnections[1])
	require.NotSame(test, usedConnections[0], usedConnections[2])
	body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[],"previous_response_id":"alias_response_2","client_metadata":{"session_id":"%s","thread_id":"%s"}}`, root, root))
	response, err := executor.ExecuteRequestViaWebsocket(ctx, second, body, "cache", "", "test-key", nil, headers, "")
	require.Error(test, err)
	require.Nil(test, response)
	require.EqualValues(test, 4, requests.Load())
}

func TestWebsocketAccountIdentityPassiveFrames(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	previousResin, previousRuntime := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings()
	test.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousRuntime)
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		defer connection.Close()
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			received <- capture{request.Header.Clone(), body}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"passive-test","status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "passive-identity"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "passive.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 15*time.Second)
	defer cancel()
	accounts := []*auth.Account{
		{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "first-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice},
		{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "second-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice},
	}
	const root = "01a09302-49f4-7b53-b545-91ef29610317"
	const child = "01a09302-59f4-7b53-b545-91ef29610317"
	for _, source := range []string{"thread_description", "guardian_review", "memory_consolidation", "agent_created_thread", "compaction"} {
		test.Run(source, func(test *testing.T) {
			thread, relation := root, ""
			if source == "guardian_review" {
				thread = child
				relation = fmt.Sprintf(`,"parent_thread_id":"%s","subagent_kind":"guardian"`, root)
			}
			kind := "turn"
			if source == "compaction" {
				kind = "compaction"
			}
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[],"client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","window_id":"%s:0","thread_source":"%s","request_kind":"%s"%s}}}`, root, thread, root, thread, thread, source, kind, relation))
			var lastSession string
			for _, account := range accounts {
				var firstConnection *WsConnection
				var firstSession string
				for attempt := 0; attempt < 2; attempt++ {
					response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, "cache", "", "test-user-key", nil, nil, "")
					require.NoError(test, err)
					converted := websocketResponseToHTTP(ctx, response, http.StatusOK, nil)
					_, err = io.Copy(io.Discard, converted.Body)
					require.NoError(test, err)
					require.NoError(test, converted.Body.Close())
					var sent capture
					select {
					case sent = <-received:
					case <-ctx.Done():
						test.Fatal("upstream frame not observed")
					}
					session, mappedThread := sent.headers.Get("Session-Id"), sent.headers.Get("Thread-Id")
					require.NotEqual(test, root, session)
					require.NotEqual(test, root[:13], session[:13])
					require.Equal(test, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
					require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
					require.Equal(test, mappedThread, gjson.GetBytes(sent.body, "client_metadata.thread_id").String())
					require.Equal(test, mappedThread, sent.headers.Get("X-Client-Request-Id"))
					metadata := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String()
					require.Equal(test, source, gjson.Get(metadata, "thread_source").String())
					if relation != "" {
						require.Equal(test, session, gjson.Get(metadata, "parent_thread_id").String())
						require.NotEqual(test, session, mappedThread)
					} else {
						require.False(test, gjson.Get(metadata, "parent_thread_id").Exists())
					}
					if attempt == 0 {
						firstConnection, firstSession = response.conn, session
						require.NotEqual(test, lastSession, session)
					} else {
						require.Same(test, firstConnection, response.conn)
						require.Equal(test, firstSession, session)
					}
				}
				lastSession = firstSession
			}
		})
	}
	require.GreaterOrEqual(test, connections.Load(), int32(2))
}
