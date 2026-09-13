package wsrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type compressionCountingConn struct {
	net.Conn
	written *atomic.Int64
	read    *atomic.Int64
}

func (connection *compressionCountingConn) Write(payload []byte) (int, error) {
	count, err := connection.Conn.Write(payload)
	connection.written.Add(int64(count))
	return count, err
}

func (connection *compressionCountingConn) Read(payload []byte) (int, error) {
	count, err := connection.Conn.Read(payload)
	connection.read.Add(int64(count))
	return count, err
}

func compressionTestPayload() []byte {
	var text strings.Builder
	text.WriteString(`{"type":"response.completed","data":"`)
	for index := 0; index < 256; index++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("compression-fixture-%d", index)))
		text.WriteString(hex.EncodeToString(digest[:]))
	}
	text.WriteString(`"}`)
	return []byte(text.String())
}

func TestContextTakeoverNegotiationAndDictionaryReuse(test *testing.T) {
	for _, scenario := range []struct {
		name string
		mode coderws.CompressionMode
	}{
		{"context_takeover", coderws.CompressionContextTakeover},
		{"server_requires_independent_messages", coderws.CompressionNoContextTakeover},
		{"server_declines_compression", coderws.CompressionDisabled},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			offers := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				offers <- request.Header.Clone()
				connection, err := coderws.Accept(writer, request, &coderws.AcceptOptions{CompressionMode: scenario.mode, CompressionThreshold: 1})
				if err != nil {
					test.Error(err)
					return
				}
				defer connection.CloseNow()
				connection.SetReadLimit(1 << 20)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for {
					messageType, payload, err := connection.Read(ctx)
					if err != nil {
						return
					}
					if connection.Write(ctx, messageType, payload) != nil {
						return
					}
				}
			}))
			test.Cleanup(server.Close)
			var sent, received atomic.Int64
			dialer := &websocket.Dialer{
				HandshakeTimeout: time.Second,
				NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &compressionCountingConn{Conn: connection, written: &sent, read: &received}, nil
				},
			}
			headers := http.Header{"Session-Id": {"session"}, "Thread-Id": {"related-thread"}, "Chatgpt-Account-Id": {"account"}}
			ctx, cancel := context.WithCancel(context.Background())
			connection, response, err := dialContextTakeover(ctx, dialer, server.URL, headers)
			cancel()
			require.NoError(test, err)
			test.Cleanup(func() { _ = connection.Close() })
			require.NotNil(test, connection.RemoteAddr())
			offer := <-offers
			require.Equal(test, "permessage-deflate; client_max_window_bits", offer.Get("Sec-WebSocket-Extensions"))
			require.Empty(test, offer.Get("Accept-Encoding"))
			for _, name := range []string{"Session-Id", "Thread-Id", "Chatgpt-Account-Id"} {
				require.Equal(test, headers.Get(name), offer.Get(name))
			}
			switch scenario.mode {
			case coderws.CompressionContextTakeover:
				require.Equal(test, "permessage-deflate", response.Header.Get("Sec-WebSocket-Extensions"))
			case coderws.CompressionNoContextTakeover:
				require.Contains(test, response.Header.Get("Sec-WebSocket-Extensions"), "server_no_context_takeover")
				require.Contains(test, response.Header.Get("Sec-WebSocket-Extensions"), "client_no_context_takeover")
			default:
				require.Empty(test, response.Header.Get("Sec-WebSocket-Extensions"))
			}
			payload := compressionTestPayload()
			var sentSizes, receivedSizes []int64
			for iteration := 0; iteration < 2; iteration++ {
				beforeSent, beforeReceived := sent.Load(), received.Load()
				require.NoError(test, connection.SetWriteDeadline(time.Now().Add(time.Second)))
				require.NoError(test, connection.WriteMessage(websocket.TextMessage, payload))
				messageType, reader, err := connection.NextReader()
				require.NoError(test, err)
				require.Equal(test, websocket.TextMessage, messageType)
				actual, err := io.ReadAll(reader)
				require.NoError(test, err)
				require.Equal(test, payload, actual)
				sentSizes = append(sentSizes, sent.Load()-beforeSent)
				receivedSizes = append(receivedSizes, received.Load()-beforeReceived)
			}
			if scenario.mode == coderws.CompressionContextTakeover {
				require.Less(test, sentSizes[1], sentSizes[0]*3/4, "outbound compressor must reuse the prior message dictionary")
				require.Less(test, receivedSizes[1], receivedSizes[0]*3/4, "inbound decoder must handle a dictionary-dependent message")
			} else {
				require.Greater(test, sentSizes[1], sentSizes[0]/2)
			}
		})
	}
}

func TestWSCompressionSwitchPreservesExistingConnectionsAndContinuation(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, payload, err := connection.ReadMessage(); err != nil {
				return
			} else if connection.WriteMessage(websocket.TextMessage, payload) != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	manager := NewManager()
	test.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 1, DynamicConcurrencyLimit: 2}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	headers := http.Header{"Session-Id": {"root"}, "Thread-Id": {"root"}}
	for _, enabled := range []bool{false, true, false} {
		proxy.UpdateRuntimeSettings(func(settings proxy.RuntimeSettings) proxy.RuntimeSettings {
			settings.CodexWSContextTakeover = enabled
			return settings
		})
		connection, pending, err := manager.AcquireConnection(context.Background(), account, wsURL, "root", headers, "")
		require.NoError(test, err)
		_, usesTakeover := connection.conn.(*contextTakeoverSocket)
		require.Equal(test, enabled, usesTakeover)
		proxy.UpdateRuntimeSettings(func(settings proxy.RuntimeSettings) proxy.RuntimeSettings {
			settings.CodexWSContextTakeover = !enabled
			return settings
		})
		require.True(test, connection.IsConnected())
		require.NoError(test, connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)))
		_, payload, err := readPumpMessage(test, connection)
		require.NoError(test, err)
		require.JSONEq(test, `{"type":"response.completed"}`, string(payload))
		connection.session.RemovePendingRequest(pending.RequestID)
		manager.BindResponseConn("response", connection, "root", account.ID(), "key", "scope")
		reused, next, _, _, err := acquireContinuation(manager, nil, "response", account, "key", "scope", wsURL, headers, "")
		require.NoError(test, err)
		require.Same(test, connection, reused)
		releaseUnsentConnection(manager, reused, next)
		require.True(test, probeConnectionWithTimeout(connection, time.Second))
		require.NoError(test, manager.SendHeartbeat(connection))
		manager.DiscardConnection(connection)
	}
}

func TestContextTakeoverReadLimitsAndCloseCodes(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		closeCode int
		oversize  bool
	}{
		{name: "upstream_message_too_big", closeCode: websocket.CloseMessageTooBig},
		{name: "normal_close", closeCode: websocket.CloseNormalClosure},
		{name: "local_read_limit", oversize: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				connection, err := coderws.Accept(writer, request, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover, CompressionThreshold: 1})
				if err != nil {
					return
				}
				defer connection.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, _, err := connection.Read(ctx); err != nil {
					return
				}
				if scenario.oversize {
					_ = connection.Write(ctx, coderws.MessageText, []byte(strings.Repeat("x", 4096)))
					_, _, _ = connection.Read(ctx)
				} else {
					_ = connection.Close(coderws.StatusCode(scenario.closeCode), "fixture")
				}
			}))
			test.Cleanup(server.Close)
			connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
			require.NoError(test, err)
			test.Cleanup(func() { _ = connection.Close() })
			connection.SetReadLimit(1024)
			require.NoError(test, connection.WriteMessage(websocket.TextMessage, []byte("request")))
			_, reader, err := connection.NextReader()
			if err == nil {
				_, err = io.ReadAll(reader)
			}
			if scenario.oversize {
				require.ErrorIs(test, err, websocket.ErrReadLimit)
			} else {
				require.True(test, websocket.IsCloseError(err, scenario.closeCode), "%v", err)
				require.False(test, errors.Is(err, websocket.ErrReadLimit))
			}
		})
	}
}

func TestContextTakeoverTLSUsesHTTP1AndRetainsVerification(test *testing.T) {
	protocols := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		protocols <- request.Proto
		connection, err := coderws.Accept(writer, request, nil)
		if err == nil {
			defer connection.CloseNow()
			_, _, _ = connection.Read(context.Background())
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	test.Cleanup(server.Close)
	dialer := &websocket.Dialer{HandshakeTimeout: time.Second}
	_, _, err := dialContextTakeover(context.Background(), dialer, server.URL, nil)
	require.ErrorContains(test, err, "certificate")
	dialer.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	dialer.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	connection, _, err := dialContextTakeover(context.Background(), dialer, server.URL, nil)
	require.NoError(test, err)
	test.Cleanup(func() { _ = connection.Close() })
	require.Equal(test, "HTTP/1.1", <-protocols)
	require.Equal(test, []string{"h2", "http/1.1"}, dialer.TLSClientConfig.NextProtos)
}

func TestContextTakeoverPumpControlFrames(test *testing.T) {
	test.Run("idle_peer_ping", func(test *testing.T) {
		pongReceived := make(chan string, 1)
		_, connection := newReadPumpTestConnectionWithUpgrader(test, websocket.Upgrader{EnableCompression: true}, func(peer *websocket.Conn) {
			peer.SetPongHandler(func(payload string) error {
				pongReceived <- payload
				return nil
			})
			_ = peer.WriteControl(websocket.PingMessage, []byte("idle-ping"), time.Now().Add(time.Second))
			_, _, _ = peer.ReadMessage()
		}, true)
		select {
		case payload := <-pongReceived:
			require.Equal(test, "idle-ping", payload)
		case <-time.After(time.Second):
			test.Fatal("no pong for idle upstream ping")
		}
		require.True(test, connection.IsConnected())
		require.Positive(test, connection.lastInbound.Load())
	})
	test.Run("mismatched_pong", func(test *testing.T) {
		_, connection := newReadPumpTestConnectionWithUpgrader(test, websocket.Upgrader{}, func(peer *websocket.Conn) {
			peer.SetPingHandler(func(payload string) error {
				return peer.WriteControl(websocket.PongMessage, []byte("not-"+payload), time.Now().Add(time.Second))
			})
			_, _, _ = peer.ReadMessage()
		}, true)
		require.False(test, probeConnectionWithTimeout(connection, 100*time.Millisecond))
		require.True(test, connection.IsConnected(), "a timed-out pong wait must not itself close the reader")
	})
	test.Run("business_frame_during_probe", func(test *testing.T) {
		_, connection := newReadPumpTestConnectionWithUpgrader(test, websocket.Upgrader{}, func(peer *websocket.Conn) {
			peer.SetPingHandler(func(string) error {
				return peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
			})
			_, _, _ = peer.ReadMessage()
		}, true)
		commitReadPumpTestLease(test, connection, "during-probe")
		started := time.Now()
		require.True(test, probeConnectionWithTimeoutAfterQueueSequence(connection, time.Second, 0))
		require.Less(test, time.Since(started), 500*time.Millisecond)
		_, payload, err := readPumpMessage(test, connection)
		require.NoError(test, err)
		require.JSONEq(test, `{"type":"response.completed"}`, string(payload))
		require.True(test, connection.IsConnected())
	})
}

func TestContextTakeoverPumpRejectsIdleFragmentBeforeNewLease(test *testing.T) {
	finishMessage := make(chan struct{})
	pongSeen := make(chan struct{}, 1)
	manager, connection := newReadPumpTestConnectionWithUpgrader(test, websocket.Upgrader{WriteBufferSize: 64}, func(peer *websocket.Conn) {
		peer.SetPongHandler(func(string) error {
			pongSeen <- struct{}{}
			return nil
		})
		go func() { _, _, _ = peer.ReadMessage() }()
		writer, err := peer.NextWriter(websocket.TextMessage)
		if err != nil {
			return
		}
		_, _ = writer.Write([]byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("stale", 2048) + `"}`))
		_ = peer.WriteControl(websocket.PingMessage, []byte("between-fragments"), time.Now().Add(time.Second))
		<-finishMessage
		_ = writer.Close()
	}, true)
	test.Cleanup(func() { close(finishMessage) })
	select {
	case <-pongSeen:
	case <-time.After(time.Second):
		test.Fatal("fragmented message or its interleaved ping was not consumed")
	}
	require.Error(test, connection.BeginReadLease("next-request"))
	manager.DiscardConnection(connection)
}

func TestContextTakeoverPreservesSOCKSRemoteDNS(test *testing.T) {
	addresses := make(chan string, 1)
	targets := make(chan string, 1)
	dialer := &websocket.Dialer{
		HandshakeTimeout: time.Second,
		NetDialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			addresses <- address
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				target, err := rejectSOCKS5Target(server)
				if err != nil {
					test.Error(err)
				}
				targets <- target
			}()
			return client, nil
		},
	}
	require.NoError(test, configureWebsocketDialerProxy(dialer, "socks5h://user:password@proxy.invalid:1080"))
	_, _, err := dialContextTakeover(context.Background(), dialer, "wss://remote-dns.invalid/responses", nil)
	require.Error(test, err)
	require.Equal(test, "proxy.invalid:1080", <-addresses)
	require.Equal(test, "remote-dns.invalid:443", <-targets)
}

func TestContextTakeoverHTTPProxyAndRedirectSafety(test *testing.T) {
	test.Run("connect_proxy", func(test *testing.T) {
		requests := make(chan *http.Request, 1)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests <- request.Clone(context.Background())
			writer.WriteHeader(http.StatusBadGateway)
		}))
		test.Cleanup(server.Close)
		dialer := &websocket.Dialer{HandshakeTimeout: time.Second}
		require.NoError(test, configureWebsocketDialerProxy(dialer, strings.Replace(server.URL, "://", "://user:password@", 1)))
		_, _, err := dialContextTakeover(context.Background(), dialer, "wss://upstream.invalid/responses", nil)
		require.Error(test, err)
		request := <-requests
		require.Equal(test, http.MethodConnect, request.Method)
		require.Equal(test, "upstream.invalid:443", request.Host)
		require.NotEmpty(test, request.Header.Get("Proxy-Authorization"))
	})
	test.Run("redirect_does_not_forward_credentials", func(test *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			if request.URL.Path == "/start" {
				http.Redirect(writer, request, "/target", http.StatusTemporaryRedirect)
				return
			}
			writer.WriteHeader(http.StatusUnauthorized)
		}))
		test.Cleanup(server.Close)
		_, response, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL+"/start", http.Header{"Authorization": {"Bearer fixture"}})
		require.Error(test, err)
		require.Equal(test, http.StatusTemporaryRedirect, response.StatusCode)
		require.Equal(test, int32(1), requests.Load())
	})
}
