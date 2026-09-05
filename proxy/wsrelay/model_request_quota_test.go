package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// The quota gate runs after AcquireConnection, which reserves both a pending
// request and a read lease. Rejection must roll back both reservations without
// writing response.create or destroying the otherwise healthy pooled socket.
func TestExecuteWebsocketModelQuotaRejectionReturnsUnsentLeaseToIdle(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const key = "sk-ws-model-quota-cleanup-test"
	keyID, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{Key: key, Limits: database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAPIKeyByValue(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if hit, err := db.ConsumeAPIKeyModelRequest(context.Background(), keyID, "already-used", "gpt-6", row.Limits.ModelRequestLimits, time.Now()); err != nil || hit != nil {
		t.Fatalf("exhaust test budget: hit=%v err=%v", hit, err)
	}

	// Exercise the production authentication attachment instead of duplicating
	// proxy's private context-key/charge-identity implementation in the test.
	handler := proxy.NewHandler(nil, db, nil, nil)
	var requestCtx context.Context
	router := gin.New()
	router.GET("/quota-context", handler.APIKeyAuthMiddleware(), func(c *gin.Context) {
		requestCtx = c.Request.Context()
		c.Status(http.StatusNoContent)
	})
	freshContext := func() context.Context {
		t.Helper()
		requestCtx = nil
		req := httptest.NewRequest(http.MethodGet, "/quota-context", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNoContent || requestCtx == nil {
			t.Fatalf("authenticate quota context: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return requestCtx
	}

	received := make(chan []byte, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received <- payload
		}
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	account := &auth.Account{DBID: 42, AccountID: "42", AccessToken: "test-upstream-token", DynamicConcurrencyLimit: 1}
	const sessionID = "quota-cleanup-session"
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	poolKey := manager.poolKey(account.ID(), wsURL, sessionID, "")
	session := NewSession(account.ID(), manager)
	session.ID = sessionID
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = poolKey
	wc.onReadFailure = manager.DiscardConnection
	wc.installControlHandlers()
	manager.connections.Store(poolKey, wc)
	manager.sessions.Store(poolKey, session)
	wc.StartReadPump()
	executor := NewExecutorWithManager(manager)

	response, err := executor.ExecuteRequestViaWebsocket(freshContext(), account, []byte(`{"model":"gpt-6","input":"rejected"}`), sessionID, "", key, nil, http.Header{}, "")
	if err == nil || response != nil || !strings.Contains(err.Error(), "weekly model request limit") {
		t.Fatalf("quota rejection: response=%v error=%v", response, err)
	}
	if got := session.PendingCount(); got != 0 {
		t.Fatalf("quota rejection leaked %d pending requests", got)
	}
	if !canReuseConnection(wc) {
		t.Fatal("quota rejection left the unsent read lease reserved or poisoned the healthy connection")
	}
	if pooled, ok := manager.connections.Load(poolKey); !ok || pooled != wc {
		t.Fatal("quota rejection discarded or replaced the existing connection")
	}
	select {
	case payload := <-received:
		t.Fatalf("quota rejection wrote an upstream frame: %s", payload)
	default:
	}

	// A different, unlimited model can immediately use the very same socket.
	response, err = executor.ExecuteRequestViaWebsocket(freshContext(), account, []byte(`{"model":"gpt-5.4","input":"allowed"}`), sessionID, "", key, nil, http.Header{}, "")
	if err != nil {
		t.Fatalf("reuse after quota rejection: %v", err)
	}
	defer response.Close()
	if response.conn != wc {
		t.Fatal("allowed request did not reuse the original connection")
	}
	select {
	case payload := <-received:
		if gjson.GetBytes(payload, "type").String() != "response.create" || gjson.GetBytes(payload, "model").String() != "gpt-5.4" {
			t.Fatalf("first upstream frame should be the allowed model: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allowed response.create did not reach upstream")
	}
}
