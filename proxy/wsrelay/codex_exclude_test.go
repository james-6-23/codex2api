package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func TestPrepareWebsocketHeadersForwardsCodexExclude(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{DBID: 42, AccountID: "42"}

	cases := []struct {
		name       string
		ginHeaders http.Header
		account    *auth.Account
		want       string
		wantExists bool
	}{
		{
			name:       "forwards tools and usage attribution",
			ginHeaders: http.Header{proxy.CodexExcludeHeader: {"tools,usage.attribution"}},
			account:    account,
			want:       "tools,usage.attribution",
			wantExists: true,
		},
		{
			name:       "forwards tools only",
			ginHeaders: http.Header{proxy.CodexExcludeHeader: {"tools"}},
			account:    account,
			want:       "tools",
			wantExists: true,
		},
		{
			name:       "joins repeated header values",
			ginHeaders: http.Header{proxy.CodexExcludeHeader: {"tools", "usage.attribution"}},
			account:    account,
			want:       "tools,usage.attribution",
			wantExists: true,
		},
		{
			name:       "omitted stays omitted",
			ginHeaders: http.Header{},
			account:    account,
			want:       "",
			wantExists: false,
		},
		{
			name:       "whitespace only stays omitted",
			ginHeaders: http.Header{proxy.CodexExcludeHeader: {"  \t "}},
			account:    account,
			want:       "",
			wantExists: false,
		},
		{
			name:       "account custom header overrides downstream",
			ginHeaders: http.Header{proxy.CodexExcludeHeader: {"tools,usage.attribution"}},
			account:    &auth.Account{DBID: 42, AccountID: "42", CustomHeaders: map[string]string{"x-codex-exclude": "tools"}},
			want:       "tools",
			wantExists: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", tc.account, "42", "session-123", "api-key-1", nil, tc.ginHeaders, nil, "")
			_, exists := headers[proxy.CodexExcludeHeader]
			if exists != tc.wantExists {
				t.Fatalf("header presence = %v, want %v (header=%v)", exists, tc.wantExists, headers[proxy.CodexExcludeHeader])
			}
			if got := headers.Get(proxy.CodexExcludeHeader); got != tc.want {
				t.Fatalf("%s = %q, want %q", proxy.CodexExcludeHeader, got, tc.want)
			}
		})
	}
}

func TestWebsocketClientPoolScopesCodexExclude(t *testing.T) {
	base := http.Header{
		"User-Agent": {"codex-tui/0.171.0 (Mac OS 15.5.0; arm64) xterm"},
		"Version":    {"0.171.0"},
		"Originator": {"codex-tui"},
	}
	withDefault := base.Clone()
	withDefault.Set(proxy.CodexExcludeHeader, "tools,usage.attribution")
	withTools := base.Clone()
	withTools.Set(proxy.CodexExcludeHeader, "tools")

	kOmitted := websocketClientPoolKey("pool", base)
	kDefault := websocketClientPoolKey("pool", withDefault)
	kTools := websocketClientPoolKey("pool", withTools)

	if kOmitted == kDefault || kOmitted == kTools || kDefault == kTools {
		t.Fatalf("expected distinct pool keys for omitted/default/tools, got %q / %q / %q", kOmitted, kDefault, kTools)
	}
	if kDefault != websocketClientPoolKey("pool", withDefault.Clone()) {
		t.Fatal("identical x-codex-exclude did not retain pool key")
	}
}

func TestWebsocketReuseIsolatesCodexExcludeHandshake(t *testing.T) {
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "false")
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	previous := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(previous) })

	type observation struct {
		headers http.Header
	}
	observations := make(chan observation, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		handshakeHeaders := r.Header.Clone()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			observations <- observation{headers: handshakeHeaders}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})

	runSequence := func(t *testing.T, stateless bool) {
		t.Helper()
		manager := NewManager()
		t.Cleanup(manager.Stop)
		manager.probeFunc = func(*WsConnection) bool { return true }
		executor := NewExecutorWithManager(manager)
		account := &auth.Account{DBID: 42, AccessToken: "fixture-token"}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		steps := []struct {
			exclude    string
			wantExists bool
		}{
			{exclude: "tools,usage.attribution", wantExists: true},
			{exclude: "tools", wantExists: true},
			{exclude: "", wantExists: false},
			{exclude: "tools,usage.attribution", wantExists: true},
		}

		conns := make([]*WsConnection, 0, len(steps))
		for i, step := range steps {
			headers := http.Header{}
			if step.exclude != "" {
				headers.Set(proxy.CodexExcludeHeader, step.exclude)
			}
			sessionID := "explicit-session-exclude"
			poolRouteKey := ""
			if stateless {
				sessionID = fmt.Sprintf("stateless-exclude-%d", i)
				poolRouteKey = "reusable-pool-exclude"
			}
			resp, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4","input":"hello"}`), sessionID, "", "api-key-exclude", nil, headers, poolRouteKey)
			if err != nil {
				t.Fatalf("step %d ExecuteRequestViaWebsocket() error = %v", i, err)
			}
			conns = append(conns, resp.conn)
			httpResp := websocketResponseToHTTP(ctx, resp, http.StatusOK, nil)
			if _, err := io.Copy(io.Discard, httpResp.Body); err != nil {
				httpResp.Body.Close()
				t.Fatalf("step %d read stream error = %v", i, err)
			}
			httpResp.Body.Close()

			select {
			case obs := <-observations:
				_, exists := obs.headers[proxy.CodexExcludeHeader]
				if exists != step.wantExists {
					t.Fatalf("step %d handshake %s presence = %v, want %v", i, proxy.CodexExcludeHeader, exists, step.wantExists)
				}
				if got := obs.headers.Get(proxy.CodexExcludeHeader); got != step.exclude {
					t.Fatalf("step %d handshake %s = %q, want %q", i, proxy.CodexExcludeHeader, got, step.exclude)
				}
			case <-ctx.Done():
				t.Fatalf("step %d timed out waiting for server observation", i)
			}
		}

		if conns[0] == conns[1] {
			t.Fatal("switching x-codex-exclude from tools,usage.attribution to tools reused old connection")
		}
		if conns[0] == conns[2] || conns[1] == conns[2] {
			t.Fatal("omitting x-codex-exclude reused connection established with header")
		}
		if conns[0] != conns[3] {
			t.Fatal("switching back to matching x-codex-exclude did not reuse matching connection")
		}
	}

	t.Run("explicit session", func(t *testing.T) {
		runSequence(t, false)
	})
	t.Run("stateless reusable pool", func(t *testing.T) {
		runSequence(t, true)
	})
}

func TestWebsocketContinuationRejectsChangedCodexExclude(t *testing.T) {
	executor, original := continuationFixture(t)
	account := &auth.Account{DBID: 7, AccessToken: "fixture-token"}
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	executor.manager.connections.Delete(original.PoolKey)
	executor.manager.sessions.Delete(original.PoolKey)
	original.PoolKey = executor.manager.poolKey(account.ID(), wsURL, original.session.ID, "")
	executor.manager.connections.Store(original.PoolKey, original)
	executor.manager.sessions.Store(original.PoolKey, original.session)

	defaultHeaders := executor.prepareWebsocketHeaders(context.Background(), account.AccessToken, account, "", "session", "key", nil, http.Header{
		proxy.CodexExcludeHeader: {"tools,usage.attribution"},
	}, nil, "")
	original.upstreamClientIdentity = websocketClientIdentity(defaultHeaders)

	changedHeaders := executor.prepareWebsocketHeaders(context.Background(), account.AccessToken, account, "", "session", "key", nil, http.Header{
		proxy.CodexExcludeHeader: {"tools"},
	}, nil, "")
	omittedHeaders := executor.prepareWebsocketHeaders(context.Background(), account.AccessToken, account, "", "session", "key", nil, http.Header{}, nil, "")

	if got, _ := executor.manager.lookupContinuationConn(websocketContinuation{
		responseID: "previous", accountID: 7, apiKey: "key",
		identity: websocketClientIdentity(defaultHeaders), url: wsURL,
	}); got != original {
		t.Fatal("matching x-codex-exclude lost continuation connection")
	}
	for _, headers := range []http.Header{changedHeaders, omittedHeaders} {
		if got, _ := executor.manager.lookupContinuationConn(websocketContinuation{
			responseID: "previous", accountID: 7, apiKey: "key",
			identity: websocketClientIdentity(headers), url: wsURL,
		}); got != nil {
			t.Fatal("changed x-codex-exclude reused continuation connection")
		}
	}

	for name, ginHeaders := range map[string]http.Header{
		"switched to tools": {proxy.CodexExcludeHeader: {"tools"}},
		"omitted":           {},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4","previous_response_id":"previous","input":[]}`), "session", "", "key", nil, ginHeaders, "")
			var lost *proxy.ResponsesContinuationLostError
			if !errors.As(err, &lost) || lost.Reason != "original_connection_unavailable" {
				t.Fatalf("changed x-codex-exclude returned %v, want ResponsesContinuationLostError(original_connection_unavailable)", err)
			}
		})
	}
}
