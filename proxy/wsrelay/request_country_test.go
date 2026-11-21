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
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWebSocketRequestCountryAcrossTurns(t *testing.T) {
	// The mock assigns IDs per socket so a reconnect cannot pass as a continuation.
	countries := []string{`"US"`, "null", "", `"JP"`}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for turn, want := range countries {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if got := gjson.GetBytes(frame, "type").String(); got != "response.create" {
				t.Errorf("turn %d: upstream frame type = %q", turn, got)
			}
			country := gjson.GetBytes(frame, "request_country")
			if country.Exists() != (want != "") || country.Raw != want {
				t.Errorf("turn %d: upstream request_country = %q, want %q", turn, country.Raw, want)
			}
			if turn > 0 && gjson.GetBytes(frame, "previous_response_id").String() != fmt.Sprintf("resp_country_%d", turn-1) {
				t.Errorf("turn %d lost its upstream continuation", turn)
			}
			event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_country_%d","status":"completed","output":[]}}`, turn)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
		<-t.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	previousResin, previousExecute := proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
	t.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.WebsocketExecuteFunc = previousExecute
	})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, sessionID, proxyURL, key, cfg, headers, poolKey)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 904, AccessToken: "test-token", PlanType: "plus"})
	handler := proxy.NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handled := make(chan struct{})
	router.Use(func(c *gin.Context) {
		defer close(handled)
		c.Next()
	})
	handler.RegisterRoutes(router)
	gateway := httptest.NewServer(router)
	t.Cleanup(gateway.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"country-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		client.Close()
		select {
		case <-handled:
		case <-time.After(5 * time.Second):
			t.Error("WebSocket handler did not exit after client close")
		}
	}()
	var previousID string
	for turn, country := range countries {
		frame := map[string]any{"type": "response.create", "model": "gpt-5.5", "input": "hello"}
		if country != "" {
			frame["request_country"] = json.RawMessage(country)
		}
		if previousID != "" {
			frame["previous_response_id"] = previousID
		}
		if err := client.WriteJSON(frame); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, event, err := client.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			switch gjson.GetBytes(event, "type").String() {
			case "response.failed", "error":
				t.Fatalf("turn %d: %s", turn, event)
			case "response.completed":
				previousID = gjson.GetBytes(event, "response.id").String()
				if previousID != fmt.Sprintf("resp_country_%d", turn) {
					t.Fatalf("turn %d completed on a different upstream socket: %s", turn, event)
				}
			default:
				continue
			}
			break
		}
	}
}
