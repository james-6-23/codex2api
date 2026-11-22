package wsrelay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestCodexClientVersionUnavailableStopsWebsocketBeforeHandshake(t *testing.T) {
	old := proxy.CurrentRuntimeSettings()
	proxy.ApplyRuntimeSettings(proxy.RuntimeSettings{ClientCompatMode: proxy.ClientCompatModeAuto, CodexMinCLIVersion: "9.999.0", CodexUserAgentConfig: `{"client_kind":"codex-vscode"}`})
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(old) })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	for _, uaEnabled := range []string{"true", "false"} {
		t.Setenv("CODEX_WS_SEND_USER_AGENT", uaEnabled)
		_, err := NewExecutor().ExecuteRequestViaWebsocket(context.Background(), &auth.Account{DBID: 1, AccountID: "test-account", AccessToken: "test-token"}, []byte(`{"model":"gpt-5.4","input":[]}`), "session", server.URL, "", nil, nil, "")
		var apiErr *proxy.Error
		if !errors.As(err, &apiErr) || apiErr.Code != proxy.ErrorCodeCodexClientVersionUnavailable || apiErr.Retryable {
			t.Fatalf("version error: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times", calls.Load())
	}
}

func TestWebsocketClientPoolScopesFullVersionPair(t *testing.T) {
	headers := http.Header{"User-Agent": {"codex/0.158.0-alpha.1 (Windows 10; x86_64) (Codex Desktop; 26.928.31416)"}, "Version": {"0.158.0-alpha.1"}, "Originator": {"Codex Desktop"}}
	key := websocketClientPoolKey("pool", headers)
	if key != websocketClientPoolKey("pool", headers.Clone()) {
		t.Fatal("same client did not retain pool")
	}
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		changed := headers.Clone()
		changed.Set(name, "different")
		if key == websocketClientPoolKey("pool", changed) {
			t.Fatalf("changed %s reused client pool", name)
		}
	}
	if websocketClientPoolKey("", headers) != "" {
		t.Fatal("empty pool key changed")
	}
}

func TestWebsocketContinuationRejectsChangedClientAndReleasesLease(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	connection := newBoundTestConn(t, manager, 7, "base#0")
	connection.upstreamClientIdentity = "old-client"
	manager.BindResponseConn("response", connection, "base#0", 7, "api-key")
	executor := NewExecutorWithManager(manager)
	got, pending, key := executor.acquireClientContinuation(websocketContinuation{responseID: "response", accountID: 7, apiKey: "api-key", identity: "new-client"})
	if got != nil || pending != nil || key != "" || connection.session.PendingCount() != 0 || !canReuseConnection(connection) {
		t.Fatal("changed client retained continuation or leaked unsent lease")
	}
	got, pending, _ = executor.acquireClientContinuation(websocketContinuation{responseID: "response", accountID: 7, apiKey: "api-key", identity: "old-client"})
	if got != connection || pending == nil {
		t.Fatal("same client lost continuation")
	}
	connection.cancelUnsentReadLease(pending.RequestID)
	connection.session.RemovePendingRequest(pending.RequestID)
}
