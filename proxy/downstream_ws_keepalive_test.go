package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDownstreamWSKeepaliveIntervalFromEnv(t *testing.T) {
	const fallback = defaultDownstreamWSKeepaliveInterval
	for _, test := range []struct {
		raw  string
		want time.Duration
	}{
		{raw: "", want: fallback},
		{raw: "250ms", want: 250 * time.Millisecond},
		{raw: "0", want: 0},
		{raw: "-1s", want: fallback},
		{raw: "invalid", want: fallback},
	} {
		t.Setenv("DOWNSTREAM_WS_KEEPALIVE_INTERVAL", test.raw)
		if got := downstreamWSKeepaliveIntervalFromEnv(); got != test.want {
			t.Errorf("interval(%q) = %s, want %s", test.raw, got, test.want)
		}
	}
}

func TestStartDownstreamWSKeepaliveSendsPing(t *testing.T) {
	serverConn, clientConn, cleanup := newDownstreamWSPair(t)
	defer cleanup()
	previous := downstreamWSKeepaliveInterval
	downstreamWSKeepaliveInterval = time.Millisecond
	defer func() { downstreamWSKeepaliveInterval = previous }()

	pings := make(chan string, 1)
	clientConn.SetPingHandler(func(payload string) error {
		select {
		case pings <- payload:
		default:
		}
		return nil
	})
	go func() {
		for {
			if _, _, err := clientConn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	stop := startDownstreamWSKeepalive(ctx, serverConn, cancel)
	defer func() {
		cancel()
		stop()
	}()
	select {
	case payload := <-pings:
		if payload != "" {
			t.Fatalf("ping payload = %q, want empty", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("downstream websocket ping was not received")
	}
}

func TestStartDownstreamWSKeepaliveCancelsOnPingFailure(t *testing.T) {
	serverConn, _, cleanup := newDownstreamWSPair(t)
	defer cleanup()
	previous := downstreamWSKeepaliveInterval
	downstreamWSKeepaliveInterval = time.Millisecond
	defer func() { downstreamWSKeepaliveInterval = previous }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startDownstreamWSKeepalive(ctx, serverConn, cancel)
	defer stop()
	_ = serverConn.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("ping failure did not cancel downstream websocket context")
	}
}

func newDownstreamWSPair(t *testing.T) (*websocket.Conn, *websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	client, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):], nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket pair: %v", err)
	}
	var serverConn *websocket.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(time.Second):
		_ = client.Close()
		server.Close()
		t.Fatal("websocket server did not accept connection")
	}
	cleanup := func() {
		_ = serverConn.Close()
		_ = client.Close()
		server.Close()
	}
	return serverConn, client, cleanup
}
