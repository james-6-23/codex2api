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
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Capture the final handshake and frames from the real executor across two
// logins, three workspaces and two downstream callers per workspace.
func TestOfficialWireWebsocketLoginIsolation(t *testing.T) {
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", `{"1":"11111111-1111-4111-8111-111111111111","2":"11111111-1111-4111-8111-111111111111","3":"22222222-2222-4222-8222-222222222222"}`)
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	old := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(old) })
	settings := old
	settings.ClientCompatMode = proxy.ClientCompatModeForce
	const syncedVersion = "0.171.0"
	settings.CodexSyncedCLIVersion = syncedVersion
	proxy.ApplyRuntimeSettings(settings)
	previous := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(previous) })
	type observation struct {
		headers http.Header
		body    []byte
	}
	frames := make(chan observation, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- observation{r.Header.Clone(), body}
			if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	manager := NewManager()
	defer manager.Stop()
	manager.probeFunc = func(*WsConnection) bool { return true }
	executor := NewExecutorWithManager(manager)
	for _, id := range []int64{1, 2, 3} {
		account := &auth.Account{DBID: id, AccessToken: "fixture", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow, CustomHeaders: map[string]string{"User-Agent": "codex-tui/{{codex_cli_version}} (Windows 10.0.19045; x86_64) unknown (codex-tui; {{codex_cli_version}})", "Version": "{{codex_cli_version}}", "Originator": "codex-tui"}}
		want := "11111111-1111-4111-8111-111111111111"
		if id == 3 {
			want = "22222222-2222-4222-8222-222222222222"
		}
		for _, caller := range []string{"caller-one", "caller-two"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			original := []byte(fmt.Sprintf(`{"model":"gpt-6-luna","input":"test","client_metadata":{"x-codex-installation-id":%q,"session_id":%q,"thread_id":%q}}`, caller, caller, caller))
			headers := proxy.PrepareCodexFingerprintHeaders(account, nil, original)
			body := proxy.ApplyCodexFingerprintToBody(original, account, headers)
			resp, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, fmt.Sprintf("login-isolation-%d-%s", id, caller), "", caller, nil, headers, caller)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			hr := websocketResponseToHTTP(ctx, resp, http.StatusOK, nil)
			_, err = io.Copy(io.Discard, hr.Body)
			hr.Body.Close()
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			select {
			case frame := <-frames:
				if gjson.GetBytes(frame.body, "client_metadata.x-codex-installation-id").String() != want {
					t.Fatal("login installation ID leaked across callers/accounts")
				}
				if frame.headers.Get("Version") != syncedVersion || frame.headers.Get("Originator") != "codex-tui" || frame.headers.Get("X-Codex-Installation-Id") != "" || frame.headers.Get("X-Oai-Attestation") != "" {
					t.Fatal("unexpected handshake identity")
				}
				if gjson.GetBytes(frame.body, "client_metadata.thread_id").String() != frame.headers.Get("Thread-Id") {
					t.Fatal("frame/handshake thread mismatch")
				}
			case <-ctx.Done():
				cancel()
				t.Fatal("missing wire capture")
			}
			cancel()
		}
	}
}
