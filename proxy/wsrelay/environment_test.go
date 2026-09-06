package wsrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/timezone"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestWebsocketEnvironmentUsesCurrentFrameAndActualConnectionProxy(test *testing.T) {
	previousResin := proxy.GetResinConfig()
	proxy.SetResinConfig(nil)
	test.Cleanup(func() { proxy.SetResinConfig(previousResin) })
	received := make(chan []byte, 4)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for turn := 0; ; turn++ {
			_, payload, err := connection.ReadMessage()
			if err != nil {
				return
			}
			received <- payload
			if err := connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_env_%d", turn), "status": "completed", "output": []any{}}}); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		test.Fatal(err)
	}
	manager := NewManager()
	test.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	account := &auth.Account{DBID: 1973, AccessToken: "dummy", ProxyURL: "http://original:8080", CodexFingerprintMode: auth.CodexFingerprintModeDevice, DynamicConcurrencyLimit: 1}
	const sessionID = "environment-session"
	wsURL, _ := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	poolKey := manager.poolKey(account.ID(), wsURL, sessionID, account.ProxyURL)
	session := NewSession(account.ID(), manager)
	session.ID = sessionID
	session.SetConnected(true)
	wrapped := NewWsConnection(connection, session, wsURL)
	wrapped.PoolKey, wrapped.proxyURL = poolKey, account.ProxyURL
	wrapped.onReadFailure = manager.DiscardConnection
	wrapped.installControlHandlers()
	manager.connections.Store(poolKey, wrapped)
	manager.sessions.Store(poolKey, session)
	wrapped.StartReadPump()
	executor := NewExecutorWithManager(manager)
	for turn, zone := range []string{"America/Los_Angeles", "Asia/Tokyo", "Europe/London"} {
		location, _ := timezone.Load(zone)
		reference := time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = proxy.WithCodexEnvironment(ctx, func(actualProxy string) *time.Location {
			if actualProxy != account.ProxyURL {
				test.Errorf("preferred connection used requested rather than actual egress: %s", actualProxy)
			}
			return location
		}, reference)
		text := "<environment_context><current_date>2026-09-06</current_date><timezone>Asia/Shanghai</timezone></environment_context>"
		if turn == 2 {
			text = "follow-up without environment"
		}
		payload := map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}}
		selectedProxy := account.ProxyURL
		if turn > 0 {
			payload["previous_response_id"] = fmt.Sprintf("resp_env_%d", turn-1)
			selectedProxy = "http://different-requested-proxy:8080"
		}
		body, _ := json.Marshal(payload)
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, sessionID, selectedProxy, "key", nil, http.Header{}, "")
		if err != nil {
			test.Fatal(err)
		}
		if response.conn != wrapped {
			test.Fatal("existing connection was not reused")
		}
		if err := response.ReadStream(func([]byte) bool { return true }); err != nil {
			test.Fatal(err)
		}
		response.Close()
		select {
		case actual := <-received:
			expected := text
			if turn < 2 {
				expected = strings.ReplaceAll(strings.ReplaceAll(text, "Asia/Shanghai", zone), "2026-09-06", reference.In(location).Format(time.DateOnly))
			}
			if got := gjson.GetBytes(actual, "input.0.content.0.text").String(); got != expected {
				test.Fatalf("turn=%d got=%s expected=%s", turn, got, expected)
			}
		case <-ctx.Done():
			test.Fatal("no upstream frame received")
		}
	}
}
