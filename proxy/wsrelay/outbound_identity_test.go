package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestOutboundIdentityUsesOriginalHandshakeOnReuse(test *testing.T) {
	received := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		received <- request.Header.Clone()
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	manager := NewManager()
	defer manager.Stop()
	manager.probeFunc = func(*WsConnection) bool { return true }
	account := &auth.Account{DBID: 17, Status: auth.StatusReady}
	headers := http.Header{"User-Agent": {"codex-tui/0.154.0"}, "Originator": {"codex-tui"}, "Version": {"0.154.0"}, "Authorization": {"Bearer private-key"}}
	headers.Set("X-Codex-Installation-Id", "31811466-ec40-4690-b07b-b4828d3095ff")
	headers.Set("X-Codex-Turn-Metadata", `{"window_number":1}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	first, pending, err := manager.AcquireConnection(ctx, account, wsURL, "identity-session", headers, "")
	require.NoError(test, err)
	var actual http.Header
	select {
	case actual = <-received:
	case <-ctx.Done():
		test.Fatal("handshake was not received")
	}
	require.NotNil(test, first.upstreamIdentity)
	for _, name := range []string{"User-Agent", "Originator", "Version", "X-Codex-Installation-Id"} {
		require.Equal(test, actual.Get(name), first.upstreamIdentity.Headers[name])
	}
	require.Equal(test, "[present]", first.upstreamIdentity.Headers["Authorization"])
	require.Equal(test, "[present]", first.upstreamIdentity.Headers["Sec-WebSocket-Key"])
	require.Equal(test, "written", first.upstreamIdentity.CaptureStage)
	require.Equal(test, actual.Get("Sec-WebSocket-Extensions"), first.upstreamIdentity.Headers["Sec-WebSocket-Extensions"])
	releaseUnsentConnection(manager, first, pending)
	headers.Set("X-Codex-Turn-Metadata", `{"window_number":2}`)
	reused, pending, err := manager.AcquireConnection(ctx, account, wsURL, "identity-session", headers, "")
	require.NoError(test, err)
	require.Same(test, first, reused)
	require.Equal(test, actual.Get("X-Codex-Installation-Id"), reused.upstreamIdentity.Headers["X-Codex-Installation-Id"])
	require.Equal(test, "1", reused.upstreamIdentity.TurnMetadata["window_number"])
	releaseUnsentConnection(manager, reused, pending)
	headers.Set("X-Codex-Installation-Id", "41811466-ec40-4690-b07b-b4828d3095ff")
	rotated, pending, err := manager.AcquireConnection(ctx, account, wsURL, "identity-session", headers, "")
	require.NoError(test, err)
	require.NotSame(test, first, rotated)
	require.Equal(test, headers.Get("X-Codex-Installation-Id"), rotated.upstreamIdentity.Headers["X-Codex-Installation-Id"])
	releaseUnsentConnection(manager, rotated, pending)
}
