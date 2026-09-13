package wsrelay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type takeoverWriteGate struct {
	net.Conn
	active    atomic.Bool
	once      sync.Once
	closeOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
	closed    chan struct{}
}

func (gate *takeoverWriteGate) Write(payload []byte) (int, error) {
	if gate.active.Load() {
		gate.once.Do(func() {
			close(gate.entered)
			select {
			case <-gate.release:
			case <-gate.closed:
			}
		})
	}
	return gate.Conn.Write(payload)
}

func (gate *takeoverWriteGate) Close() error {
	gate.closeOnce.Do(func() { close(gate.closed) })
	return gate.Conn.Close()
}

func TestContextTakeoverRescuedProbeSurvivesBlockedPingWrite(test *testing.T) {
	peers := make(chan *websocket.Conn, 1)
	pingReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		peer.SetPingHandler(func(payload string) error {
			pingReceived <- struct{}{}
			return peer.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second))
		})
		peers <- peer
		_, _, _ = peer.ReadMessage()
	}))
	defer server.Close()
	var gate *takeoverWriteGate
	dialer := &websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		gate = &takeoverWriteGate{Conn: connection, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
		return gate, nil
	}}
	socket, _, err := dialContextTakeover(context.Background(), dialer, server.URL, nil)
	require.NoError(test, err)
	manager := NewManager()
	defer manager.Stop()
	session := NewSession(1, manager)
	session.SetConnected(true)
	connection := newUpstreamWsConnection(socket, session, server.URL)
	defer connection.Close()
	commitReadPumpTestLease(test, connection, "probe-write")
	connection.StartReadPump()
	peer := <-peers
	gate.active.Store(true)
	probeFinished := make(chan bool, 1)
	const probeTimeout = 100 * time.Millisecond
	go func() { probeFinished <- probeConnectionWithTimeoutAfterQueueSequence(connection, probeTimeout, 0) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		test.Fatal("Ping did not start")
	}
	require.NoError(test, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)))
	select {
	case alive := <-probeFinished:
		require.True(test, alive)
	case <-time.After(time.Second):
		test.Fatal("inbound data did not rescue the probe")
	}
	// Cross the original probe deadline while the write is deliberately paused.
	timer := time.NewTimer(2 * probeTimeout)
	defer timer.Stop()
	select {
	case <-gate.closed:
		test.Fatal("rescued probe canceled the connection")
	case <-timer.C:
	}
	close(gate.release)
	select {
	case <-pingReceived:
	case <-time.After(time.Second):
		test.Fatal("Ping did not finish after the write resumed")
	}
	_, payload, err := readPumpMessage(test, connection)
	require.NoError(test, err)
	require.JSONEq(test, `{"type":"response.completed"}`, string(payload))
	require.True(test, connection.IsConnected())
}

func TestContextTakeoverHeartbeatDoesNotWaitForPong(test *testing.T) {
	pingReceived := make(chan struct{}, 1)
	manager, connection := newReadPumpTestConnectionWithUpgrader(test, websocket.Upgrader{}, func(peer *websocket.Conn) {
		peer.SetPingHandler(func(string) error { pingReceived <- struct{}{}; return nil })
		_, _, _ = peer.ReadMessage()
	}, true)
	completed := make(chan error, 1)
	go func() { completed <- manager.SendHeartbeat(connection) }()
	select {
	case err := <-completed:
		require.NoError(test, err)
	case <-time.After(time.Second):
		test.Fatal("send-only heartbeat waited for Pong")
	}
	select {
	case <-pingReceived:
	case <-time.After(time.Second):
		test.Fatal("heartbeat did not reach the peer")
	}
	require.True(test, connection.IsConnected())
	_, pooled := manager.connections.Load(connection.PoolKey)
	require.True(test, pooled, "missing Pong alone must not remove continuation affinity")
}

func TestContextTakeoverSlowUploadAllowsPeerPingWithinWriteBudget(test *testing.T) {
	peers := make(chan *websocket.Conn, 1)
	pongReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peer, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		peer.SetReadLimit(1 << 20)
		peer.SetPongHandler(func(string) error { pongReceived <- struct{}{}; return nil })
		peers <- peer
		for {
			if _, _, err := peer.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	var gate *takeoverWriteGate
	dialer := &websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		gate = &takeoverWriteGate{Conn: connection, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
		return gate, nil
	}}
	socket, _, err := dialContextTakeover(context.Background(), dialer, server.URL, nil)
	require.NoError(test, err)
	manager := NewManager()
	defer manager.Stop()
	session := NewSession(1, manager)
	session.SetConnected(true)
	connection := newUpstreamWsConnection(socket, session, server.URL)
	defer connection.Close()
	connection.installControlHandlers()
	pingSeen := make(chan struct{}, 1)
	socket.(*contextTakeoverSocket).onPing = func() { connection.touchInbound(); pingSeen <- struct{}{} }
	require.NoError(test, connection.BeginReadLease("slow-upload"))
	connection.StartReadPump()
	gate.active.Store(true)
	writeDone := make(chan error, 1)
	go func() { writeDone <- connection.WriteMessage(websocket.BinaryMessage, compressionTestPayload()) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		test.Fatal("upload did not start")
	}
	peer := <-peers
	require.NoError(test, peer.WriteControl(websocket.PingMessage, []byte("upload-progress"), time.Now().Add(time.Second)))
	select {
	case <-pingSeen:
	case <-time.After(time.Second):
		test.Fatal("peer Ping was not read")
	}
	// A valid slow upload can exceed the old five-second automatic-Pong budget.
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-gate.closed:
		test.Fatal("automatic Pong interrupted an upload within its write budget")
	case <-timer.C:
	}
	close(gate.release)
	select {
	case err := <-writeDone:
		require.NoError(test, err)
	case <-time.After(time.Second):
		test.Fatal("upload did not resume")
	}
	select {
	case <-pongReceived:
	case <-time.After(time.Second):
		test.Fatal("Pong was not delivered after the data frame")
	}
	require.True(test, connection.IsConnected())
}

// Parse frames from the local test client to verify bounded data framing.
func readCompressionFixtureFrame(reader io.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(reader, head[:]); err != nil {
		return 0, nil, err
	}
	length := uint64(head[1] & 127)
	if length == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	} else if length == 127 {
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > 1<<20 {
		return 0, nil, fmt.Errorf("unexpected fixture frame length %d", length)
	}
	var mask [4]byte
	if head[1]&128 != 0 {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	_, err := io.ReadFull(reader, payload)
	if head[1]&128 != 0 {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	return head[0], payload, err
}

func TestContextTakeoverLargeMessagesUseBoundedFrames(test *testing.T) {
	for _, compressed := range []bool{false, true} {
		test.Run(fmt.Sprint(compressed), func(test *testing.T) {
			frames := make(chan []int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				peer, err := (&websocket.Upgrader{EnableCompression: compressed}).Upgrade(writer, request, nil)
				if err != nil {
					return
				}
				defer peer.Close()
				_ = peer.UnderlyingConn().SetReadDeadline(time.Now().Add(3 * time.Second))
				var lengths []int
				for {
					first, payload, err := readCompressionFixtureFrame(peer.UnderlyingConn())
					if err != nil {
						test.Error(err)
						return
					}
					lengths = append(lengths, len(payload))
					if len(lengths) == 1 {
						require.Equal(test, byte(websocket.BinaryMessage), first&15)
						require.Equal(test, compressed, first&64 != 0)
					} else {
						require.Zero(test, first&79, "continuations must not repeat opcode or RSV1")
					}
					if first&128 != 0 {
						break
					}
				}
				frames <- lengths
			}))
			defer server.Close()
			connection, _, err := dialContextTakeover(context.Background(), &websocket.Dialer{HandshakeTimeout: time.Second}, server.URL, nil)
			require.NoError(test, err)
			defer connection.Close()
			var payload []byte
			for index := 0; index < 4096; index++ {
				digest := sha256.Sum256([]byte(fmt.Sprint(index)))
				payload = append(payload, digest[:]...)
			}
			require.NoError(test, connection.WriteMessage(websocket.BinaryMessage, payload))
			select {
			case lengths := <-frames:
				require.Greater(test, len(lengths), 1)
				for _, length := range lengths {
					require.LessOrEqual(test, length, 16<<10)
				}
			case <-time.After(3 * time.Second):
				test.Fatal("large message did not complete")
			}
		})
	}
}

func TestContextTakeoverPlaintextProxyUsesConnect(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		kind, payload, err := peer.ReadMessage()
		if err == nil {
			_ = peer.WriteMessage(kind, payload)
		}
	}))
	defer upstream.Close()
	methods := make(chan string, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods <- request.Method
		if request.Method != http.MethodConnect {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		remote, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			return
		}
		defer remote.Close()
		client, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = io.WriteString(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffered); close(done) }()
		_, _ = io.Copy(client, remote)
		_ = client.Close()
		<-done
	}))
	defer proxyServer.Close()
	dialer := &websocket.Dialer{HandshakeTimeout: time.Second}
	require.NoError(test, configureWebsocketDialerProxy(dialer, proxyServer.URL))
	connection, _, err := dialContextTakeover(context.Background(), dialer, upstream.URL, nil)
	require.NoError(test, err)
	defer connection.Close()
	require.Equal(test, http.MethodConnect, <-methods)
	require.NoError(test, connection.WriteMessage(websocket.TextMessage, []byte("proxy echo")))
	_, reader, err := connection.NextReader()
	require.NoError(test, err)
	actual, err := io.ReadAll(bufio.NewReader(reader))
	require.NoError(test, err)
	require.True(test, bytes.Equal([]byte("proxy echo"), actual))
}
