package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Run through the real executor and reusable slot selection. Inspect each
// frame against its socket's frozen handshake, not just the pool-key helper.
func TestSessionIdentityFingerprintReusableWebsocket(t *testing.T) {
	testSessionIdentityFingerprintReusableWebsocket(t, false)
}

func TestSessionIdentityTimePreservingReusableWebsocket(t *testing.T) {
	// Register before Setenv so the environment is restored before reloading
	// process configuration during cleanup.
	t.Cleanup(func() {
		if err := proxy.ConfigureCodexIdentityTimeFromEnv(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CODEX_IDENTITY_V2_FROM_UNIX_MS", "1")
	if err := proxy.ConfigureCodexIdentityTimeFromEnv(); err != nil {
		t.Fatal(err)
	}
	testSessionIdentityFingerprintReusableWebsocket(t, true)
}

func testSessionIdentityFingerprintReusableWebsocket(t *testing.T, timePreserving bool) {
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "false")
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	previous := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(previous) })
	type observation struct {
		headers http.Header
		body    []byte
	}
	frames := make(chan observation, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- observation{r.Header.Clone(), frame}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 42, AccessToken: "fixture-token", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sockets []*WsConnection
	root, child := "root", "child"
	if timePreserving {
		root, child = "01a1e838-fc93-7ef0-b32b-c0c0c3466899", "01a1e838-fc94-7ef0-b32b-c0c0c3466899"
	}
	for i, thread := range []string{root, child, root} {
		original := []byte(fmt.Sprintf(`{"model":"gpt-5.4","input":"test","prompt_cache_key":"private-cache","client_metadata":{"session_id":%q,"thread_id":%q}}`, root, thread))
		headers := proxy.PrepareCodexFingerprintHeaders(account, nil, original)
		body := proxy.ApplyCodexFingerprintToBody(original, account, headers)
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, fmt.Sprintf("stateless-fixture-%d", i), "", "fixture-key", nil, headers, "fixture-key-pool")
		if err != nil {
			t.Fatal(err)
		}
		sockets = append(sockets, response.conn)
		httpResponse := websocketResponseToHTTP(ctx, response, http.StatusOK, nil)
		_, readErr := io.Copy(io.Discard, httpResponse.Body)
		httpResponse.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		select {
		case frame := <-frames:
			session, mappedThread := proxy.ConvergedCodexSessionIdentity(account, headers)
			if frame.headers.Get("Session-Id") != session || frame.headers.Get("Thread-Id") != mappedThread ||
				gjson.GetBytes(frame.body, "client_metadata.thread_id").String() != mappedThread {
				t.Fatalf("frame identity differs from its frozen handshake: session=%s thread=%s want=(%s,%s) body=%s",
					frame.headers.Get("Session-Id"), frame.headers.Get("Thread-Id"), session, mappedThread, frame.body)
			}
			if gjson.GetBytes(frame.body, "prompt_cache_key").String() != "private-cache" {
				t.Fatal("cache partition changed")
			}
			if timePreserving && uuid.MustParse(mappedThread).Time() != uuid.MustParse(thread).Time() {
				t.Fatal("actual WebSocket handshake/frame lost thread creation time")
			}
		case <-ctx.Done():
			t.Fatal("no local WebSocket frame")
		}
	}
	if sockets[0] == sockets[1] {
		t.Fatal("parent and child reused the same frozen handshake")
	}
	if sockets[0] != sockets[2] {
		t.Fatal("same conversation lost connection reuse")
	}
}
