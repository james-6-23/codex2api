package wsrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestWebsocketReusedConnectionUsesOnlyCurrentFrameMetadata(test *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		for _, pooled := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/pooled=%t", mode, pooled), func(test *testing.T) {
				test.Setenv("CODEX_WS_STATELESS_ONESHOT", "false")
				test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
				previousResin := proxy.GetResinConfig()
				previousRuntime := proxy.CurrentRuntimeSettings()
				previousExecutor := proxy.WebsocketExecuteFunc
				test.Cleanup(func() {
					proxy.SetResinConfig(previousResin)
					proxy.ApplyRuntimeSettings(previousRuntime)
					proxy.WebsocketExecuteFunc = previousExecutor
				})
				proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
				type capture struct {
					connection int64
					headers    http.Header
					body       []byte
				}
				received := make(chan capture, 4)
				var connections atomic.Int64
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					connection, err := upgrader.Upgrade(writer, request, nil)
					if err != nil {
						return
					}
					defer connection.Close()
					connectionID := connections.Add(1)
					for {
						_, payload, err := connection.ReadMessage()
						if err != nil {
							return
						}
						received <- capture{connectionID, request.Header.Clone(), payload}
						turn := gjson.GetBytes(payload, "client_metadata.x-codex-turn-metadata").String()
						responseID := "resp_" + gjson.Get(turn, "turn_id").String()
						if err := connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{}}}); err != nil {
							return
						}
					}
				}))
				test.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "metadata-test"})
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
				account := &auth.Account{DBID: 902, AccountID: "fixture", AccessToken: "dummy-token", CodexFingerprintMode: mode, DynamicConcurrencyLimit: 1}
				stale := make(http.Header)
				stale.Set("Originator", "codex_cli_rs")
				stale.Set("Thread-Id", "stale-thread")
				stale.Set("X-Codex-Window-Id", "stale-thread:0")
				stale.Set("X-Codex-Parent-Thread-Id", "stale-parent")
				stale.Set("X-OpenAI-Subagent", "stale-agent")
				stale.Set("X-OpenAI-Memgen-Request", "true")
				stale.Set("X-Codex-Turn-State", "stale-state")
				stale.Set("X-Codex-Turn-Metadata", `{"thread_id":"stale-thread","parent_thread_id":"stale-parent"}`)
				unchanged := stale.Clone()
				for turn := 0; turn < 3; turn++ {
					thread := "current-thread"
					if pooled {
						thread += strconv.Itoa(turn)
					}
					canonical := map[string]any{
						"installation_id": "device-" + strconv.Itoa(turn), "session_id": "root", "thread_id": thread,
						"window_id": fmt.Sprintf("%s:%d", thread, turn), "window_number": turn,
						"context_window_id": "context-" + strconv.Itoa(turn), "turn_id": strconv.Itoa(turn),
						"request_kind": "turn", "thread_source": "user",
					}
					if turn == 0 {
						canonical["parent_thread_id"] = "root"
						canonical["forked_from_thread_id"] = "fork-source"
						canonical["subagent_kind"] = "review"
						canonical["thread_source"] = "subagent"
						canonical["request_kind"] = "memory"
					}
					raw, _ := json.Marshal(canonical)
					body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": map[string]any{"x-codex-turn-metadata": string(raw)}})
					expected := proxy.NewCodexFingerprint(account, stale, body).ApplyBody(body)
					sessionID := "gateway-cache"
					if pooled {
						sessionID = ""
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					response, err := proxy.ExecuteRequest(ctx, account, body, sessionID, "", "same-key", nil, stale, true)
					if err != nil {
						test.Fatal(err)
					}
					_, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil {
						test.Fatal(readErr)
					}
					var sent capture
					select {
					case sent = <-received:
					case <-ctx.Done():
						test.Fatal("mock did not receive frame")
					}
					if sent.connection != 1 {
						test.Fatalf("connection was not reused: %d", sent.connection)
					}
					for _, name := range []string{"X-Codex-Window-Id", "Thread-Id", "X-Client-Request-Id", "X-Codex-Turn-Metadata", "X-Codex-Parent-Thread-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", "X-Codex-Turn-State"} {
						if sent.headers.Get(name) != "" {
							test.Fatalf("frozen handshake contains per-request field %s", name)
						}
					}
					metadata := gjson.GetBytes(sent.body, codexTurnMetadataClientPath).String()
					if metadata != gjson.GetBytes(expected, codexTurnMetadataClientPath).String() {
						test.Fatalf("current canonical metadata changed or rehashed: %s, want %s", metadata, expected)
					}
					if window := gjson.GetBytes(sent.body, "client_metadata.x-codex-window-id").String(); window != gjson.Get(metadata, "window_id").String() {
						test.Fatalf("window projection differs: %s", sent.body)
					}
					if parent := gjson.GetBytes(sent.body, "client_metadata.x-codex-parent-thread-id").String(); parent != gjson.Get(metadata, "parent_thread_id").String() {
						test.Fatalf("parent projection differs: %s", sent.body)
					}
					if turn > 0 && (gjson.GetBytes(sent.body, "client_metadata.x-openai-subagent").Exists() || gjson.GetBytes(sent.body, "client_metadata.x-openai-memgen-request").Exists() || gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-state").Exists()) {
						test.Fatalf("absent optional fields inherited from handshake: %s", sent.body)
					}
				}
				if !reflect.DeepEqual(stale, unchanged) {
					test.Fatal("downstream handshake headers mutated")
				}
			})
		}
	}
}

func TestResponseConnectionBindingChecksRequestScope(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	session := NewSession(42, manager)
	session.SetConnected(true)
	connection := &WsConnection{session: session, PoolKey: "fixture-pool"}
	connection.SetState(StateConnected)
	connection.Touch()
	manager.connections.Store(connection.PoolKey, connection)
	manager.BindResponseConn("response", connection, "thread#ovf-1", 42, "key", "thread")
	if found, _ := manager.lookupResponseConn("response", 42, "key", "thread"); found != connection {
		test.Fatal("same-scope overflow connection lost")
	}
	for _, request := range []struct {
		account    int64
		key, scope string
	}{{43, "key", "thread"}, {42, "other-key", "thread"}, {42, "key", "other-thread"}, {42, "key", "stateless:pool"}} {
		if found, _ := manager.lookupResponseConn("response", request.account, request.key, request.scope); found != nil {
			test.Fatalf("connection crossed request scope: %+v", request)
		}
	}
}
