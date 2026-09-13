package wsrelay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
)

const readPumpTestTimeout = 2 * time.Second

type readPumpMessageResult struct {
	messageType int
	payload     []byte
	err         error
}

func newReadPumpTestConnection(t *testing.T, serve func(*websocket.Conn)) (*Manager, *WsConnection) {
	t.Helper()
	return newReadPumpTestConnectionWithUpgrader(t, websocket.Upgrader{}, serve)
}

func newReadPumpTestConnectionWithUpgrader(t *testing.T, upgrader websocket.Upgrader, serve func(*websocket.Conn), contextTakeover ...bool) (*Manager, *WsConnection) {
	t.Helper()

	ready := make(chan struct{})
	upgrader.CheckOrigin = func(*http.Request) bool { return true }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		<-ready
		serve(conn)
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	var conn upstreamWebsocket
	var err error
	if len(contextTakeover) > 0 && contextTakeover[0] {
		conn, _, err = dialContextTakeover(context.Background(), websocket.DefaultDialer, wsURL, nil)
	} else {
		conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	}
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}

	manager := NewManager()
	key := manager.poolKey(1, wsURL, "read-pump-test", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := newUpstreamWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	wc.onReadFailure = manager.DiscardConnection
	wc.installControlHandlers()
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)
	wc.StartReadPump()
	close(ready)

	t.Cleanup(func() {
		_ = wc.Close()
		manager.Stop()
		server.Close()
	})
	return manager, wc
}

func readPumpMessage(t *testing.T, wc *WsConnection) (int, []byte, error) {
	t.Helper()
	resultCh := make(chan readPumpMessageResult, 1)
	go func() {
		messageType, payload, err := wc.ReadMessage()
		resultCh <- readPumpMessageResult{messageType: messageType, payload: payload, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.messageType, result.payload, result.err
	case <-time.After(readPumpTestTimeout):
		t.Fatal("timed out waiting for read-pump message")
		return 0, nil, nil
	}
}

func waitForReadPumpCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(readPumpTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func commitReadPumpTestLease(t *testing.T, wc *WsConnection, requestID string) {
	t.Helper()
	if err := wc.BeginReadLease(requestID); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	activeLease := state.activeLease
	leasePhase := state.leasePhase
	if activeLease != requestID || leasePhase != readLeaseReserved {
		state.mu.Unlock()
		t.Fatalf("reserved lease = (%q, %d), want (%q, %d)", activeLease, leasePhase, requestID, readLeaseReserved)
	}
	state.leasePhase = readLeaseCommitted
	state.mu.Unlock()
}

func TestReadPumpProcessesPingWhileIdle(t *testing.T) {
	pongPayload := make(chan string, 1)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPongHandler(func(appData string) error {
			pongPayload <- appData
			return nil
		})
		if err := conn.WriteControl(websocket.PingMessage, []byte("idle-ping"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("write ping: %v", err)
			return
		}
		_, _, _ = conn.ReadMessage()
	})

	select {
	case got := <-pongPayload:
		if got != "idle-ping" {
			t.Fatalf("pong payload = %q, want %q", got, "idle-ping")
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("idle connection did not process peer ping")
	}
	if !wc.IsConnected() {
		t.Fatal("processing an idle ping must not disconnect the connection")
	}
}

func TestReadPumpRejectsQueuedPeerClose(t *testing.T) {
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		if err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "queued close"),
			time.Now().Add(time.Second),
		); err != nil {
			t.Errorf("write close: %v", err)
			return
		}
		<-holdOpen
	})

	waitForReadPumpCondition(t, func() bool {
		_, connectionExists := manager.connections.Load(wc.PoolKey)
		_, sessionExists := manager.sessions.Load(wc.PoolKey)
		return !wc.IsConnected() && !connectionExists && !sessionExists
	}, "peer close did not mark and remove the connection")
	if _, ok := manager.connections.Load(wc.PoolKey); ok {
		t.Fatal("peer close left the exact connection in Manager")
	}
	if _, ok := manager.sessions.Load(wc.PoolKey); ok {
		t.Fatal("peer close left the exact session in Manager")
	}
	if err := wc.BeginReadLease("next-request"); err == nil {
		t.Fatal("reader terminated by queued close must reject a new lease")
	}
}

func TestProbeRequiresMatchingPong(t *testing.T) {
	pingPayload := make(chan string, 1)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(appData string) error {
			pingPayload <- appData
			return conn.WriteControl(websocket.PongMessage, []byte("not-"+appData), time.Now().Add(time.Second))
		})
		_, _, _ = conn.ReadMessage()
	})

	if probeConnectionWithTimeout(wc, 100*time.Millisecond) {
		t.Fatal("probe succeeded without a matching pong")
	}
	select {
	case payload := <-pingPayload:
		if payload == "" {
			t.Fatal("probe ping payload must be unique and non-empty")
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("server never received the probe ping")
	}
}

func TestReadPumpProbeAcceptsMatchingPong(t *testing.T) {
	pingSeen := make(chan struct{}, 1)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		defaultPingHandler := conn.PingHandler()
		conn.SetPingHandler(func(appData string) error {
			pingSeen <- struct{}{}
			return defaultPingHandler(appData)
		})
		_, _, _ = conn.ReadMessage()
	})

	if !probeConnectionWithTimeout(wc, time.Second) {
		t.Fatal("probe rejected the matching pong")
	}
	select {
	case <-pingSeen:
	case <-time.After(readPumpTestTimeout):
		t.Fatal("server never received the successful probe ping")
	}
}

func TestReadPumpProbeUsesSingleAbsoluteDeadline(t *testing.T) {
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(appData string) error {
			return conn.WriteControl(websocket.PongMessage, []byte("not-"+appData), time.Now().Add(time.Second))
		})
		_, _, _ = conn.ReadMessage()
	})

	const (
		probeTimeout = 150 * time.Millisecond
		writeDelay   = 100 * time.Millisecond
		maxElapsed   = 200 * time.Millisecond
	)
	wc.writeMu.Lock()
	started := time.Now()
	resultCh := make(chan bool, 1)
	go func() { resultCh <- probeConnectionWithTimeout(wc, probeTimeout) }()

	waitForReadPumpCondition(t, func() bool {
		wc.probeStateMu.Lock()
		registered := wc.probeResult != nil
		wc.probeStateMu.Unlock()
		return registered
	}, "probe did not register its pong waiter")
	time.Sleep(writeDelay)
	wc.writeMu.Unlock()

	select {
	case alive := <-resultCh:
		if alive {
			t.Fatal("probe succeeded without a matching pong")
		}
		if elapsed := time.Since(started); elapsed > maxElapsed {
			t.Fatalf("probe elapsed %s, want a single %s deadline", elapsed, probeTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not honor its timeout")
	}
}

func TestReadPumpProbeSerializationHonorsDeadline(t *testing.T) {
	pingPayloads := make(chan string, 2)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(appData string) error {
			pingPayloads <- appData
			return nil
		})
		_, _, _ = conn.ReadMessage()
	})

	longResult := make(chan bool, 1)
	go func() { longResult <- probeConnectionWithTimeout(wc, 300*time.Millisecond) }()
	select {
	case payload := <-pingPayloads:
		if payload == "" {
			t.Fatal("long probe sent an empty payload")
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("long probe never acquired the serialization gate")
	}

	started := time.Now()
	if probeConnectionWithTimeout(wc, 50*time.Millisecond) {
		t.Fatal("short probe succeeded without a pong")
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("serialized short probe elapsed %s, want its 50ms total budget", elapsed)
	}
	select {
	case alive := <-longResult:
		if alive {
			t.Fatal("long probe succeeded without a pong")
		}
	case <-time.After(time.Second):
		t.Fatal("long probe did not finish")
	}
}

func TestReadPumpProbeDoesNotWaitForDataWriteLock(t *testing.T) {
	pingSeen := make(chan struct{}, 1)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		defaultPingHandler := conn.PingHandler()
		conn.SetPingHandler(func(appData string) error {
			pingSeen <- struct{}{}
			return defaultPingHandler(appData)
		})
		_, _, _ = conn.ReadMessage()
	})

	wc.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			wc.writeMu.Unlock()
		}
	}()
	resultCh := make(chan bool, 1)
	go func() { resultCh <- probeConnectionWithTimeout(wc, 100*time.Millisecond) }()

	select {
	case <-pingSeen:
	case <-time.After(200 * time.Millisecond):
		wc.writeMu.Unlock()
		locked = false
		<-resultCh
		t.Fatal("probe Ping was blocked by the data-message write lock")
	}
	select {
	case alive := <-resultCh:
		if !alive {
			t.Fatal("matching Pong was not observed while the data write lock was held")
		}
	case <-time.After(200 * time.Millisecond):
		wc.writeMu.Unlock()
		locked = false
		t.Fatal("probe did not finish while the data write lock was held")
	}
	wc.writeMu.Unlock()
	locked = false
}

func TestReadMessageLivenessUsesRecentInboundWithoutExtraProbe(t *testing.T) {
	serverReady := make(chan struct{})
	clientProbe := make(chan string, 1)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		defaultPingHandler := conn.PingHandler()
		conn.SetPingHandler(func(appData string) error {
			select {
			case clientProbe <- appData:
			default:
			}
			return defaultPingHandler(appData)
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 8; i++ {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte("server-heartbeat"), time.Now().Add(time.Second)); err != nil {
					return
				}
			case <-stop:
				return
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"recent-inbound"}}`)); err != nil {
			t.Errorf("write terminal response: %v", err)
			return
		}
		<-stop
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "recent-inbound")
	messageType, payload, err := wc.readMessageWithLiveness(30*time.Millisecond, 25*time.Millisecond, 100*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("read long-running response with inbound heartbeats: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(payload), `"id":"recent-inbound"`) {
		t.Fatalf("response = (%d, %s), want recent-inbound terminal frame", messageType, payload)
	}
	select {
	case probePayload := <-clientProbe:
		t.Fatalf("recent inbound activity still triggered an active probe %q", probePayload)
	default:
	}
}

func TestReadMessageLivenessProbeKeepsSilentStreamAlive(t *testing.T) {
	serverReady := make(chan struct{})
	twoProbes := make(chan struct{})
	stop := make(chan struct{})
	var probeCount atomic.Int32
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		defaultPingHandler := conn.PingHandler()
		conn.SetPingHandler(func(appData string) error {
			if probeCount.Add(1) == 2 {
				close(twoProbes)
			}
			return defaultPingHandler(appData)
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)

		select {
		case <-twoProbes:
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"probe-alive"}}`)); err != nil {
				t.Errorf("write delayed terminal response: %v", err)
				return
			}
		case <-stop:
			return
		}
		<-stop
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "probe-alive")
	messageType, payload, err := wc.readMessageWithLiveness(25*time.Millisecond, 5*time.Millisecond, 100*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("read response after successful liveness probes: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(payload), `"id":"probe-alive"`) {
		t.Fatalf("response = (%d, %s), want probe-alive terminal frame", messageType, payload)
	}
	if got := probeCount.Load(); got < 2 {
		t.Fatalf("probe count = %d, want at least 2 checkpoints", got)
	}
}

func TestReadMessageLivenessFailsWithoutInboundOrMatchingPong(t *testing.T) {
	serverReady := make(chan struct{})
	probeSeen := make(chan string, 1)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(appData string) error {
			select {
			case probeSeen <- appData:
			default:
			}
			return nil
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)
		<-stop
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "probe-timeout")
	_, _, err := wc.readMessageWithLiveness(20*time.Millisecond, 5*time.Millisecond, 40*time.Millisecond, time.Minute)
	if err == nil {
		t.Fatal("missing inbound activity and matching Pong did not fail liveness check")
	}
	if !strings.Contains(err.Error(), "websocket liveness check timed out") ||
		!strings.Contains(err.Error(), "no matching pong") {
		t.Fatalf("liveness error = %v, want explicit probe failure", err)
	}
	select {
	case payload := <-probeSeen:
		if payload == "" {
			t.Fatal("liveness probe payload must be unique and non-empty")
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("server never received the liveness probe")
	}
}

func TestReadMessageLivenessTerminalDuringProbeWinsImmediately(t *testing.T) {
	serverReady := make(chan struct{})
	probeSeen := make(chan struct{}, 1)
	terminalSent := make(chan time.Time, 1)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error {
			select {
			case probeSeen <- struct{}{}:
			default:
			}
			return nil
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)

		select {
		case <-probeSeen:
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"probe-race"}}`)); err != nil {
				t.Errorf("write terminal response during probe: %v", err)
				return
			}
			terminalSent <- time.Now()
		case <-stop:
			return
		}
		<-stop
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "probe-race")
	resultCh := make(chan readPumpMessageResult, 1)
	go func() {
		messageType, payload, err := wc.readMessageWithLiveness(20*time.Millisecond, 5*time.Millisecond, 500*time.Millisecond, time.Minute)
		resultCh <- readPumpMessageResult{messageType: messageType, payload: payload, err: err}
	}()

	var sentAt time.Time
	select {
	case sentAt = <-terminalSent:
	case <-time.After(readPumpTestTimeout):
		t.Fatal("server did not send a terminal frame during the probe")
	}
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("terminal frame lost to probe failure: %v", result.err)
		}
		if result.messageType != websocket.TextMessage || !strings.Contains(string(result.payload), `"id":"probe-race"`) {
			t.Fatalf("response = (%d, %s), want probe-race terminal frame", result.messageType, result.payload)
		}
		if elapsed := time.Since(sentAt); elapsed >= 300*time.Millisecond {
			t.Fatalf("terminal frame delivery waited %s for the 500ms probe timeout", elapsed)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("terminal frame waited for the full liveness probe timeout")
	}
}

func TestReadMessageLivenessFailedProbeRescuedByFreshInbound(t *testing.T) {
	serverReady := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		var peerPingOnce sync.Once
		conn.SetPingHandler(func(string) error {
			// 吞掉探针 Ping(不回 Pong),改发一个对端 Ping:客户端 Ping handler
			// 会 touchInbound,但探针等的匹配 Pong 永远不来。只有 read_pump 的
			// 探针失败后复检 recentInbound 这条救援分支能救活本测试的流。
			peerPingOnce.Do(func() {
				_ = conn.WriteControl(websocket.PingMessage, []byte("peer-ping"), time.Now().Add(time.Second))
			})
			return nil
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)

		select {
		case <-time.After(150 * time.Millisecond):
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"probe-rescued"}}`)); err != nil {
				t.Errorf("write delayed terminal response: %v", err)
				return
			}
		case <-stop:
			return
		}
		<-stop
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "probe-rescued")
	// 窗口(300ms)远大于探针超时(50ms):探针失败时,探针期间收到的对端 Ping
	// 在复检处仍然新鲜,必须触发救援而不是杀掉活流。
	_, payload, err := wc.readMessageWithLiveness(20*time.Millisecond, 300*time.Millisecond, 50*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("failed probe with fresh inbound at re-check killed a live stream: %v", err)
	}
	if !strings.Contains(string(payload), `"id":"probe-rescued"`) {
		t.Fatalf("payload = %s, want probe-rescued terminal frame", payload)
	}
}

func TestReadMessageLivenessPeerCloseDuringProbeReturnsRealError(t *testing.T) {
	serverReady := make(chan struct{})
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error {
			// 探针进行中对端死亡:必须把真实 close code 归因给消费者
			// (下游 1009 HTTP 降级等判断依赖它),而不是泛化的 liveness 错误。
			return conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "died during probe"),
				time.Now().Add(time.Second),
			)
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)
		<-holdOpen
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "close-during-probe")
	_, _, err := wc.readMessageWithLiveness(20*time.Millisecond, 5*time.Millisecond, 500*time.Millisecond, time.Minute)
	if err == nil {
		t.Fatal("peer close during probe returned no error")
	}
	if strings.Contains(err.Error(), "liveness check timed out") || !strings.Contains(err.Error(), "1011") {
		t.Fatalf("error = %v, want the real close 1011 cause, not the generic liveness error", err)
	}
}

func TestReadMessageLivenessSilenceCapFailsDespiteLiveTransport(t *testing.T) {
	serverReady := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		close(serverReady)

		// 持续心跳让传输层始终"活着",但永不发业务帧:静默上限必须兜底收尾。
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte("server-heartbeat"), time.Now().Add(time.Second)); err != nil {
					return
				}
			case <-stop:
				return
			}
		}
	})

	<-serverReady
	commitReadPumpTestLease(t, wc, "silence-cap")
	started := time.Now()
	_, _, err := wc.readMessageWithLiveness(15*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, 60*time.Millisecond)
	if err == nil {
		t.Fatal("business-frame silence beyond the cap did not fail despite live transport")
	}
	if !strings.Contains(err.Error(), "websocket read timed out") || !strings.Contains(err.Error(), "no business frame within") {
		t.Fatalf("error = %v, want the silence-cap timeout error", err)
	}
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond {
		t.Fatalf("silence cap fired after %s, before the 60ms cap elapsed", elapsed)
	}
}

func TestReadMessageLivenessRejectsNonPositiveTimings(t *testing.T) {
	wc := &WsConnection{}
	for _, tt := range []struct {
		name                    string
		interval, window, probe time.Duration
		wantSubstr              string
	}{
		{"zero interval", 0, time.Second, time.Second, "check interval must be positive"},
		{"zero window", time.Second, 0, time.Second, "inbound window must be positive"},
		{"negative probe", time.Second, time.Second, -1, "probe timeout must be positive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := wc.readMessageWithLiveness(tt.interval, tt.window, tt.probe, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("err = %v, want %q", err, tt.wantSubstr)
			}
		})
	}
}

func TestReadLivenessConstantsInvariants(t *testing.T) {
	if ActiveReadRecentInboundWindow < 2*HeartbeatPingInterval {
		t.Fatalf("recent inbound window %s must tolerate one missed heartbeat (>= %s)", ActiveReadRecentInboundWindow, 2*HeartbeatPingInterval)
	}
	if ActiveReadRecentInboundWindow >= ReadLivenessCheckInterval {
		t.Fatalf("recent inbound window %s must be shorter than the check interval %s", ActiveReadRecentInboundWindow, ReadLivenessCheckInterval)
	}
	if ActiveReadProbeTimeout <= 0 || ActiveReadProbeTimeout >= ReadLivenessCheckInterval {
		t.Fatalf("probe timeout %s out of sane range (0, %s)", ActiveReadProbeTimeout, ReadLivenessCheckInterval)
	}
	if ActiveReadMaxTurnSilence <= ReadLivenessCheckInterval {
		t.Fatalf("max turn silence %s must exceed the check interval %s or every long turn dies at the first checkpoint", ActiveReadMaxTurnSilence, ReadLivenessCheckInterval)
	}
}

func TestActiveReadMaxTurnSilenceEnvOverride(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        time.Duration
	}{
		{"default when unset", "", ActiveReadMaxTurnSilence},
		{"custom duration", "30m", 30 * time.Minute},
		{"zero disables the cap", "0", 0},
		{"garbage falls back", "not-a-duration", ActiveReadMaxTurnSilence},
		{"negative falls back", "-5m", ActiveReadMaxTurnSilence},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CODEX_WS_MAX_TURN_SILENCE", tt.value)
			if got := activeReadMaxTurnSilence(); got != tt.want {
				t.Fatalf("activeReadMaxTurnSilence() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReadPumpRejectsFragmentedMessageStartedWhileIdle(t *testing.T) {
	firstFragmentFlushed := make(chan struct{})
	finishMessage := make(chan struct{})
	pongSeen := make(chan struct{}, 1)

	manager, wc := newReadPumpTestConnectionWithUpgrader(t, websocket.Upgrader{WriteBufferSize: 64}, func(conn *websocket.Conn) {
		conn.SetPongHandler(func(string) error {
			select {
			case pongSeen <- struct{}{}:
			default:
			}
			return nil
		})
		go func() { _, _, _ = conn.ReadMessage() }()

		writer, err := conn.NextWriter(websocket.TextMessage)
		if err != nil {
			t.Errorf("NextWriter: %v", err)
			return
		}
		payload := `{"type":"response.output_text.delta","delta":"` + strings.Repeat("stale", 2048) + `"}`
		if _, err := writer.Write([]byte(payload)); err != nil {
			t.Errorf("write first fragmented payload: %v", err)
			return
		}
		if err := conn.WriteControl(websocket.PingMessage, []byte("between-fragments"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("write interleaved ping: %v", err)
			return
		}
		close(firstFragmentFlushed)
		<-finishMessage
		_ = writer.Close()
	})

	<-firstFragmentFlushed
	waitForReadPumpCondition(t, func() bool {
		select {
		case <-pongSeen:
			return true
		default:
			return !wc.IsConnected()
		}
	}, "fragmented message was not observed by the client reader")

	beginErr := wc.BeginReadLease("new-request")
	close(finishMessage)
	if beginErr == nil {
		_, payload, readErr := readPumpMessage(t, wc)
		if readErr == nil && strings.Contains(string(payload), "stale") {
			t.Fatal("message that started while idle was attributed to a later lease")
		}
		t.Fatal("connection accepted a lease after an idle fragmented message had started")
	}
	waitForReadPumpCondition(t, func() bool {
		_, exists := manager.connections.Load(wc.PoolKey)
		return !wc.IsConnected() && !exists
	}, "idle fragmented message did not discard the connection")
}

func TestReadPumpRejectsFrameBeforeRequestCommit(t *testing.T) {
	sendStale := make(chan struct{})
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		<-sendStale
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"stale-before-write"}`)); err != nil {
			t.Errorf("write stale frame: %v", err)
			return
		}
		<-holdOpen
	})

	if err := wc.BeginReadLease("reserved-request"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	close(sendStale)
	if _, payload, err := readPumpMessage(t, wc); err == nil {
		t.Fatalf("frame received before request commit was accepted: %s", payload)
	}
	waitForReadPumpCondition(t, func() bool {
		_, exists := manager.connections.Load(wc.PoolKey)
		return !wc.IsConnected() && !exists
	}, "pre-commit frame did not discard the connection")
}

func TestCaptureLeaseWaitsForSuccessfulInFlightWrite(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	if err := wc.BeginReadLease("fast-response"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, tracksLease, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}
	if !tracksLease {
		t.Fatal("text request did not start a tracked lease write")
	}

	captured, err := wc.captureReadLease()
	if err != nil {
		t.Fatalf("captureReadLease: %v", err)
	}
	if captured.leaseID != leaseID || captured.write == nil {
		t.Fatalf("captured lease = %#v, want writing lease %q with barrier", captured, leaseID)
	}

	resultCh := make(chan error, 1)
	go func() { resultCh <- wc.awaitCapturedReadLease(captured) }()
	select {
	case awaitErr := <-resultCh:
		t.Fatalf("await returned before the in-flight write completed: %v", awaitErr)
	case <-time.After(25 * time.Millisecond):
	}

	if err := wc.completeReadLeaseWrite(leaseID, nil); err != nil {
		t.Fatalf("completeReadLeaseWrite: %v", err)
	}
	select {
	case awaitErr := <-resultCh:
		if awaitErr != nil {
			t.Fatalf("await after successful write: %v", awaitErr)
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("await did not resume after the write committed")
	}
}

func TestCaptureLeaseRejectsFailedInFlightWrite(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	if err := wc.BeginReadLease("failed-write"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}

	captured, err := wc.captureReadLease()
	if err != nil {
		t.Fatalf("captureReadLease: %v", err)
	}
	resultCh := make(chan error, 1)
	go func() { resultCh <- wc.awaitCapturedReadLease(captured) }()
	select {
	case awaitErr := <-resultCh:
		t.Fatalf("await returned before the in-flight write failed: %v", awaitErr)
	case <-time.After(25 * time.Millisecond):
	}

	writeErr := errors.New("write: broken pipe")
	if err := wc.completeReadLeaseWrite(leaseID, writeErr); !errors.Is(err, writeErr) {
		t.Fatalf("completeReadLeaseWrite error = %v, want %v", err, writeErr)
	}
	select {
	case awaitErr := <-resultCh:
		if awaitErr == nil {
			t.Fatal("failed request write was accepted as a committed response lease")
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("await did not resume after the write failed")
	}
}

func TestReadPumpProcessesPingWhileResponseWaitsForWriteCommit(t *testing.T) {
	startResponse := make(chan struct{})
	finishResponse := make(chan struct{})
	pongSeen := make(chan struct{}, 1)
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishResponse) }) }

	_, wc := newReadPumpTestConnectionWithUpgrader(t, websocket.Upgrader{WriteBufferSize: 64}, func(conn *websocket.Conn) {
		conn.SetPongHandler(func(string) error {
			select {
			case pongSeen <- struct{}{}:
			default:
			}
			return nil
		})
		go func() { _, _, _ = conn.ReadMessage() }()
		<-startResponse

		writer, err := conn.NextWriter(websocket.TextMessage)
		if err != nil {
			t.Errorf("NextWriter: %v", err)
			return
		}
		payload := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("fast", 2048) + `"}`)
		if _, err := writer.Write(payload); err != nil {
			t.Errorf("write first response fragments: %v", err)
			return
		}
		if err := conn.WriteControl(websocket.PingMessage, []byte("during-write"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("write interleaved ping: %v", err)
			return
		}
		<-finishResponse
		if err := writer.Close(); err != nil {
			t.Errorf("close response writer: %v", err)
		}
	})
	t.Cleanup(finish)

	if err := wc.BeginReadLease("fast-response"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}
	close(startResponse)

	select {
	case <-pongSeen:
	case <-time.After(readPumpTestTimeout):
		t.Fatal("read pump did not process an interleaved Ping before the request write committed")
	}
	if err := wc.completeReadLeaseWrite(leaseID, nil); err != nil {
		t.Fatalf("completeReadLeaseWrite: %v", err)
	}
	finish()

	messageType, payload, err := readPumpMessage(t, wc)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(payload), strings.Repeat("fast", 2048)) {
		t.Fatalf("response = (%d, %d bytes), want the complete text message", messageType, len(payload))
	}
}

func TestReadPumpProcessesPingAfterCompleteEarlyResponseBeforeWriteCommit(t *testing.T) {
	startResponse := make(chan struct{})
	responseSent := make(chan struct{})
	finishServer := make(chan struct{})
	pongSeen := make(chan struct{}, 1)
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishServer) }) }

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPongHandler(func(string) error {
			select {
			case pongSeen <- struct{}{}:
			default:
			}
			return nil
		})
		go func() { _, _, _ = conn.ReadMessage() }()
		<-startResponse
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"early"}`)); err != nil {
			t.Errorf("write complete early response: %v", err)
			return
		}
		close(responseSent)
		if err := conn.WriteControl(websocket.PingMessage, []byte("after-complete-message"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("write ping after complete response: %v", err)
			return
		}
		<-finishServer
	})
	t.Cleanup(finish)

	if err := wc.BeginReadLease("complete-early-response"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}
	close(startResponse)
	<-responseSent

	select {
	case <-pongSeen:
	case <-time.After(250 * time.Millisecond):
		_ = wc.completeReadLeaseWrite(leaseID, nil)
		t.Fatal("read pump stopped processing control frames after a complete early response")
	}
	if err := wc.completeReadLeaseWrite(leaseID, nil); err != nil {
		t.Fatalf("completeReadLeaseWrite: %v", err)
	}
	finish()

	messageType, payload, err := readPumpMessage(t, wc)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(payload), `"delta":"early"`) {
		t.Fatalf("response = (%d, %s), want the complete early response", messageType, payload)
	}
}

func TestFailedWriteNeverBecomesReusable(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	if err := wc.BeginReadLease("failed-write"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}

	writeErr := errors.New("write: broken pipe")
	if err := wc.completeReadLeaseWrite(leaseID, writeErr); !errors.Is(err, writeErr) {
		t.Fatalf("completeReadLeaseWrite error = %v, want %v", err, writeErr)
	}
	if wc.readPumpReusable() {
		t.Fatal("failed write exposed the connection as reusable")
	}
	if err := wc.BeginReadLease("next-request"); err == nil {
		t.Fatal("failed write allowed a later request to reserve the connection")
	}
}

func TestProvisionalTerminalFrameReleasesLeaseOnlyAfterWriteCommit(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	if err := wc.BeginReadLease("early-terminal"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}
	captured, err := wc.captureReadLease()
	if err != nil {
		t.Fatalf("captureReadLease: %v", err)
	}
	if err := wc.enqueueBusinessFrameForCapturedLease(
		websocket.TextMessage,
		[]byte(`{"type":"response.completed","response":{"id":"early"}}`),
		captured,
	); err != nil {
		t.Fatalf("enqueue provisional terminal: %v", err)
	}

	state := wc.ensureReadState()
	state.mu.Lock()
	activeBeforeCommit := state.activeLease
	phaseBeforeCommit := state.leasePhase
	terminalQueued := state.leaseTerminalQueued
	state.mu.Unlock()
	if activeBeforeCommit != leaseID || phaseBeforeCommit != readLeaseWriting || !terminalQueued {
		t.Fatalf("before commit = (lease %q, phase %d, terminal %v), want writing lease retained", activeBeforeCommit, phaseBeforeCommit, terminalQueued)
	}

	if err := wc.completeReadLeaseWrite(leaseID, nil); err != nil {
		t.Fatalf("completeReadLeaseWrite: %v", err)
	}
	if err := wc.awaitCapturedReadLease(captured); err != nil {
		t.Fatalf("awaitCapturedReadLease: %v", err)
	}
	state.mu.Lock()
	activeAfterCommit := state.activeLease
	phaseAfterCommit := state.leasePhase
	terminalQueued = state.leaseTerminalQueued
	state.mu.Unlock()
	if activeAfterCommit != "" || phaseAfterCommit != readLeaseIdle || terminalQueued {
		t.Fatalf("after commit = (lease %q, phase %d, terminal %v), want idle lease with queued response", activeAfterCommit, phaseAfterCommit, terminalQueued)
	}
	if wc.readPumpReusable() {
		t.Fatal("connection became reusable before the queued terminal frame was consumed")
	}
}

func TestProvisionalTerminalSurvivesNormalCloseBeforeSuccessfulCommit(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
	}{
		{name: "normal closure", code: websocket.CloseNormalClosure},
		{name: "going away", code: websocket.CloseGoingAway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wc := &WsConnection{}
			wc.state.Store(int32(StateConnected))
			state := wc.ensureReadState()
			state.mu.Lock()
			state.pumpStarted = true
			state.mu.Unlock()

			if err := wc.BeginReadLease("terminal-before-close"); err != nil {
				t.Fatalf("BeginReadLease: %v", err)
			}
			leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
			if err != nil {
				t.Fatalf("beginReadLeaseWrite: %v", err)
			}
			captured, err := wc.captureReadLease()
			if err != nil {
				t.Fatalf("captureReadLease: %v", err)
			}
			terminal := []byte(`{"type":"response.completed","response":{"id":"complete"}}`)
			if err := wc.enqueueBusinessFrameForCapturedLease(websocket.TextMessage, terminal, captured); err != nil {
				t.Fatalf("enqueue provisional terminal: %v", err)
			}

			wc.finishReadPump(&websocket.CloseError{Code: tt.code, Text: "response complete"}, true)
			if err := wc.completeReadLeaseWrite(leaseID, nil); err != nil {
				t.Fatalf("successful write after complete response and normal close: %v", err)
			}
			if err := wc.awaitCapturedReadLease(captured); err != nil {
				t.Fatalf("completed response was rejected: %v", err)
			}
			if wc.IsConnected() || wc.readPumpReusable() {
				t.Fatal("normally closed physical connection must remain retired")
			}
		})
	}
}

func TestReadPumpEnforcesQueuedItemLimit(t *testing.T) {
	const maxQueuedItems = 4096

	wc := &WsConnection{}
	wc.SetState(StateConnected)
	if err := wc.BeginReadLease("tiny-frame-flood"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	state.leasePhase = readLeaseCommitted
	state.mu.Unlock()
	for i := 0; i < maxQueuedItems; i++ {
		if err := wc.enqueueBusinessFrameForLease(websocket.TextMessage, nil, "tiny-frame-flood"); err != nil {
			t.Fatalf("enqueue item %d: %v", i+1, err)
		}
	}
	if err := wc.enqueueBusinessFrameForLease(websocket.TextMessage, nil, "tiny-frame-flood"); !errors.Is(err, errReadPumpQueueOverflow) {
		t.Fatalf("item %d error = %v, want %v", maxQueuedItems+1, err, errReadPumpQueueOverflow)
	}
}

func TestReadPumpPropagatesPrematureNormalClose(t *testing.T) {
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "premature normal close"),
			time.Now().Add(time.Second),
		); err != nil {
			t.Errorf("write normal close: %v", err)
			return
		}
		<-holdOpen
	})

	pr := wc.session.AddPendingRequest("read-pump-test")
	if err := wc.BeginReadLease(pr.RequestID); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	if err := wc.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	response := &WsResponse{conn: wc, pendingReq: pr, sessionID: "read-pump-test", manager: manager}
	err := response.ReadStream(func([]byte) bool { return true })
	if err == nil {
		t.Fatal("premature close 1000 was treated as a successful stream completion")
	}
	if !strings.Contains(err.Error(), "websocket read error") ||
		!strings.Contains(err.Error(), "1000") ||
		!strings.Contains(err.Error(), "premature normal close") {
		t.Fatalf("normal close error = %v, want wrapped code and reason", err)
	}
	response.mu.Lock()
	broken := response.connBroken
	response.mu.Unlock()
	if !broken {
		t.Fatal("premature normal close did not mark the connection broken")
	}
	if closeErr := response.Close(); closeErr != nil {
		t.Fatalf("response Close: %v", closeErr)
	}
}

func TestReadPumpAcquireRetriesEarlyReaderFailure(t *testing.T) {
	var handshakes atomic.Int32
	holdSecond := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		if handshakes.Add(1) == 1 {
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "close first connection"),
				time.Now().Add(time.Second),
			)
			time.Sleep(25 * time.Millisecond)
			return
		}
		<-holdSecond
	}))

	manager := NewManager()
	t.Cleanup(func() {
		manager.Stop()
		close(holdSecond)
		server.Close()
	})
	account := &auth.Account{DBID: 1}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wc, pr, err := manager.AcquireConnection(ctx, account, wsURL, "retry-early-close", http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireConnection: %v", err)
	}
	waitForReadPumpCondition(t, func() bool {
		return handshakes.Load() >= 2 || !wc.IsConnected()
	}, "first connection neither failed nor triggered a retry")
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes = %d, want 2 after one bounded retry", got)
	}
	if wc == nil || pr == nil || !wc.IsConnected() {
		t.Fatal("AcquireConnection did not return the healthy second connection")
	}
	if manager.ConnectionCount() != 1 || manager.SessionCount() != 1 {
		t.Fatalf("pool leak after retry: connections=%d sessions=%d", manager.ConnectionCount(), manager.SessionCount())
	}
	if pending := wc.session.PendingCount(); pending != 1 {
		t.Fatalf("healthy session pending count = %d, want 1", pending)
	}
}

func TestReadPumpRejectsIdleBusinessFrame(t *testing.T) {
	tests := []struct {
		name        string
		messageType int
		payload     []byte
	}{
		{name: "text", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.output_text.delta","delta":"stale"}`)},
		{name: "terminal text", messageType: websocket.TextMessage, payload: []byte(`{"type":"response.completed","response":{"id":"stale"}}`)},
		{name: "binary", messageType: websocket.BinaryMessage, payload: []byte("stale")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			holdOpen := make(chan struct{})
			t.Cleanup(func() { close(holdOpen) })
			manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
				if err := conn.WriteMessage(tt.messageType, tt.payload); err != nil {
					t.Errorf("write idle business frame: %v", err)
					return
				}
				<-holdOpen
			})

			waitForReadPumpCondition(t, func() bool {
				_, exists := manager.connections.Load(wc.PoolKey)
				return !wc.IsConnected() && !exists
			}, "idle business frame did not poison the connection")
			if _, ok := manager.connections.Load(wc.PoolKey); ok {
				t.Fatal("polluted connection remained in Manager")
			}
			if err := wc.BeginReadLease("next-request"); err == nil {
				t.Fatal("polluted connection accepted a subsequent lease")
			}
		})
	}
}

func TestReadPumpDropsIdleMetadataFrame(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "codex rate limits", payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{}}}`},
		{name: "codex response metadata", payload: `{"type":"codex.response.metadata","metadata":{}}`},
		{name: "bare response metadata", payload: `{"type":"response.metadata","metadata":{}}`},
		{name: "responsesapi websocket timing", payload: `{"type":"responsesapi.websocket_timing","timing":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			holdOpen := make(chan struct{})
			t.Cleanup(func() { close(holdOpen) })
			manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(tt.payload)); err != nil {
					t.Errorf("write idle metadata frame: %v", err)
					return
				}
				<-holdOpen
			})

			// 丢弃路径以 touchInbound 收尾：入站时间戳置位即代表帧已消费完毕
			waitForReadPumpCondition(t, func() bool { return wc.lastInbound.Load() != 0 }, "idle metadata frame was not consumed")
			if !wc.IsConnected() {
				t.Fatal("idle metadata frame must not poison the connection")
			}
			if _, ok := manager.connections.Load(wc.PoolKey); !ok {
				t.Fatal("connection dropped from manager after idle metadata frame")
			}
			if err := wc.BeginReadLease("after-metadata"); err != nil {
				t.Fatalf("BeginReadLease after idle metadata frame: %v", err)
			}
		})
	}
}

func TestReadPumpReusesConnectionAcrossLeases(t *testing.T) {
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		for i := 1; i <= 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Errorf("read request %d: %v", i, err)
				return
			}
			frames := [][]byte{
				[]byte(`{"type":"response.output_text.delta","delta":"lease-` + string(rune('0'+i)) + `-1"}`),
				[]byte(`{"type":"response.completed","response":{"id":"resp_` + string(rune('0'+i)) + `"}}`),
			}
			for _, frame := range frames {
				if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
					t.Errorf("write response %d: %v", i, err)
					return
				}
			}
		}
		<-holdOpen
	})

	for i := 1; i <= 2; i++ {
		requestID := "request-" + string(rune('0'+i))
		if err := wc.BeginReadLease(requestID); err != nil {
			t.Fatalf("BeginReadLease(%q): %v", requestID, err)
		}
		if err := wc.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}

		wantFrames := []string{
			`{"type":"response.output_text.delta","delta":"lease-` + string(rune('0'+i)) + `-1"}`,
			`{"type":"response.completed","response":{"id":"resp_` + string(rune('0'+i)) + `"}}`,
		}
		for frameIndex, want := range wantFrames {
			messageType, payload, err := readPumpMessage(t, wc)
			if err != nil {
				t.Fatalf("lease %d frame %d: %v", i, frameIndex, err)
			}
			if messageType != websocket.TextMessage || string(payload) != want {
				t.Fatalf("lease %d frame %d = (%d, %s), want (%d, %s)", i, frameIndex, messageType, payload, websocket.TextMessage, want)
			}
		}
	}
}

func TestReadPumpPropagatesActiveClose(t *testing.T) {
	triggerClose := make(chan struct{})
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		<-triggerClose
		if err := conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "active close"),
			time.Now().Add(time.Second),
		); err != nil {
			t.Errorf("write close: %v", err)
			return
		}
		<-holdOpen
	})

	pr := wc.session.AddPendingRequest("read-pump-test")
	if err := wc.BeginReadLease(pr.RequestID); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	if err := wc.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	close(triggerClose)

	response := &WsResponse{conn: wc, pendingReq: pr, sessionID: "read-pump-test", manager: manager}
	errCh := make(chan error, 1)
	go func() {
		errCh <- response.ReadStream(func([]byte) bool { return true })
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("active close 1011 did not reach the response consumer")
		}
		if !strings.Contains(err.Error(), "websocket read error") || !strings.Contains(err.Error(), "1011") {
			t.Fatalf("active close error = %v, want existing wrapped websocket close semantics", err)
		}
	case <-time.After(readPumpTestTimeout):
		t.Fatal("active close did not unblock WsResponse.ReadStream")
	}

	waitForReadPumpCondition(t, func() bool { return !wc.IsConnected() }, "active close did not mark connection unavailable")
	if closeErr := response.Close(); closeErr != nil {
		t.Fatalf("response Close: %v", closeErr)
	}
}

func TestReadPumpEnforcesQueuedPayloadLimit(t *testing.T) {
	triggerFrames := make(chan struct{})
	holdOpen := make(chan struct{})
	t.Cleanup(func() { close(holdOpen) })

	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		<-triggerFrames
		payload := strings.Repeat("x", readPumpMaxQueuedPayload/2+1)
		for range 2 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write oversized queue payload: %v", err)
				return
			}
		}
		<-holdOpen
	})

	if err := wc.BeginReadLease("large-response"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	if err := wc.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	close(triggerFrames)
	waitForReadPumpCondition(t, func() bool {
		_, exists := manager.connections.Load(wc.PoolKey)
		return !wc.IsConnected() && !exists
	}, "queue overflow did not poison the connection")
	if _, ok := manager.connections.Load(wc.PoolKey); ok {
		t.Fatal("queue-overflow connection remained in Manager")
	}

	if _, payload, err := readPumpMessage(t, wc); err != nil || len(payload) != readPumpMaxQueuedPayload/2+1 {
		t.Fatalf("first queued frame = (%d bytes, %v), want %d bytes", len(payload), err, readPumpMaxQueuedPayload/2+1)
	}
	if _, _, err := readPumpMessage(t, wc); !errors.Is(err, errReadPumpQueueOverflow) {
		t.Fatalf("overflow read error = %v, want %v", err, errReadPumpQueueOverflow)
	}
}

func TestReadPumpMapsReadLimitToClose1009(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	if err := wc.BeginReadLease("oversized-response"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	state.leasePhase = readLeaseCommitted
	state.mu.Unlock()

	wc.finishReadPump(websocket.ErrReadLimit, true)
	_, _, err := wc.ReadMessage()
	if !errors.Is(err, websocket.ErrReadLimit) {
		t.Fatalf("read error = %v, want preserved ErrReadLimit", err)
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read error = %v, want CloseError 1009", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
	}
	if shouldRetryWebsocketSendError(err) {
		t.Fatal("mapped read-limit error must reach HTTP fallback without WebSocket reconnects")
	}
}

func TestCompleteReadLeaseWritePreservesPeerCloseCause(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	state := wc.ensureReadState()
	state.mu.Lock()
	state.pumpStarted = true
	state.activeLease = "large-request"
	state.leasePhase = readLeaseWriting
	state.mu.Unlock()

	peerClose := &websocket.CloseError{
		Code: websocket.CloseMessageTooBig,
		Text: "message too big",
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		wc.finishReadPump(peerClose, true)
	}()

	err := wc.completeReadLeaseWrite("large-request", websocket.ErrCloseSent)
	select {
	case <-state.readerDone:
	case <-time.After(readPumpTestTimeout):
		t.Fatal("reader failure did not finish")
	}

	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("write error = %v, want preserved peer CloseError", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
	}
}

func TestCompleteReadLeaseWritePreservesPeerCloseAfterTransportWriteError(t *testing.T) {
	for _, writeErr := range []error{
		errors.New("write tcp: broken pipe"),
		errors.New("write tcp: connection reset by peer"),
	} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			wc := &WsConnection{}
			wc.state.Store(int32(StateConnected))
			state := wc.ensureReadState()
			state.mu.Lock()
			state.pumpStarted = true
			state.activeLease = "large-request"
			state.leasePhase = readLeaseWriting
			state.mu.Unlock()

			go func() {
				time.Sleep(10 * time.Millisecond)
				wc.finishReadPump(&websocket.CloseError{
					Code: websocket.CloseMessageTooBig,
					Text: "message too big",
				}, true)
			}()

			err := wc.completeReadLeaseWrite("large-request", writeErr)
			select {
			case <-state.readerDone:
			case <-time.After(readPumpTestTimeout):
				t.Fatal("reader failure did not finish")
			}

			var closeErr *websocket.CloseError
			if !errors.As(err, &closeErr) {
				t.Fatalf("write error = %v, want preserved peer CloseError", err)
			}
			if closeErr.Code != websocket.CloseMessageTooBig {
				t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
			}
		})
	}
}

func TestProvisionalFrameDoesNotMaskPeerCloseAfterTransportWriteError(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	state := wc.ensureReadState()
	state.mu.Lock()
	state.pumpStarted = true
	state.mu.Unlock()

	if err := wc.BeginReadLease("provisional-close"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	leaseID, _, err := wc.beginReadLeaseWrite(websocket.TextMessage)
	if err != nil {
		t.Fatalf("beginReadLeaseWrite: %v", err)
	}
	captured, err := wc.captureReadLease()
	if err != nil {
		t.Fatalf("captureReadLease: %v", err)
	}
	if err := wc.enqueueBusinessFrameForCapturedLease(
		websocket.TextMessage,
		[]byte(`{"type":"response.output_text.delta","delta":"early"}`),
		captured,
	); err != nil {
		t.Fatalf("enqueue provisional response: %v", err)
	}

	writeResult := make(chan error, 1)
	writeErr := errors.New("write tcp: broken pipe")
	go func() { writeResult <- wc.completeReadLeaseWrite(leaseID, writeErr) }()
	waitForReadPumpCondition(t, func() bool { return !wc.IsConnected() }, "failed writer did not seal the connection")
	wc.finishReadPump(&websocket.CloseError{
		Code: websocket.CloseMessageTooBig,
		Text: "message too big",
	}, true)

	resolvedWriteErr := <-writeResult
	var closeErr *websocket.CloseError
	if !errors.As(resolvedWriteErr, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("resolved write error = %v, want peer CloseError 1009", resolvedWriteErr)
	}
	if err := wc.awaitCapturedReadLease(captured); !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("provisional frame result = %v, want peer CloseError 1009", err)
	}
}

func TestCompleteSuccessfulWritePreservesConcurrentPeerCloseCause(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	state := wc.ensureReadState()
	state.mu.Lock()
	state.pumpStarted = true
	state.activeLease = "successful-large-request"
	state.leasePhase = readLeaseWriting
	state.mu.Unlock()

	wc.finishReadPump(&websocket.CloseError{
		Code: websocket.CloseMessageTooBig,
		Text: "message too big",
	}, true)

	err := wc.completeReadLeaseWrite("successful-large-request", nil)
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("commit error = %v, want preserved peer CloseError", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
	}
}

func TestReadPumpPreservesPeerCloseCauseDuringSocketWrite(t *testing.T) {
	closeSent := make(chan error, 1)
	_, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		if _, _, err := conn.NextReader(); err != nil {
			closeSent <- err
			return
		}
		closeSent <- conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"),
			time.Now().Add(time.Second),
		)
		time.Sleep(100 * time.Millisecond)
	})

	if err := wc.BeginReadLease("large-socket-write"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	payload := []byte(strings.Repeat("x", 8*1024*1024))
	err := wc.WriteMessage(websocket.TextMessage, payload)
	if sendErr := <-closeSent; sendErr != nil {
		t.Fatalf("server close 1009: %v", sendErr)
	}

	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("socket write error = %v, want preserved peer CloseError", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
	}
}

func TestPreservePeerCloseCauseHasBoundedWait(t *testing.T) {
	wc := &WsConnection{}
	wc.state.Store(int32(StateConnected))
	state := wc.ensureReadState()
	state.mu.Lock()
	state.pumpStarted = true
	state.activeLease = "stalled-reader"
	state.leasePhase = readLeaseWriting
	state.mu.Unlock()

	started := time.Now()
	err := wc.completeReadLeaseWrite("stalled-reader", websocket.ErrCloseSent)
	elapsed := time.Since(started)
	if !errors.Is(err, websocket.ErrCloseSent) {
		t.Fatalf("write error = %v, want original ErrCloseSent after bounded wait", err)
	}
	if elapsed < readPumpCloseCauseWait/2 || elapsed > 5*readPumpCloseCauseWait {
		t.Fatalf("close-cause wait = %s, want bounded near %s", elapsed, readPumpCloseCauseWait)
	}
}

// newProbeDeafTestConnection 建一条"探活失聪"连接：服务端吞掉一切 Ping 不回
// Pong（覆盖 gorilla 默认自动回复），Ping-Pong 往返 probe 必然超时失败。
// 用于区分 probe 的两条路径：免往返（近期入站）与完整往返。
func newProbeDeafTestConnection(t *testing.T) (*Manager, *WsConnection) {
	t.Helper()
	return newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error { return nil }) // 吞 Ping 不回 Pong
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
}

// TestProbeSkipsRoundtripAfterRecentInbound 近期有入站活动的干净连接免
// Ping-Pong 往返：请求刚完成后的热复用不为探活付一个上游 RTT（锁内串行）。
func TestProbeSkipsRoundtripAfterRecentInbound(t *testing.T) {
	manager, wc := newProbeDeafTestConnection(t)

	wc.touchInbound()
	if !manager.probe(wc) {
		t.Fatal("probe must trust a clean connection with recent inbound activity without a ping roundtrip")
	}
}

// TestWeakNetworkProbeRequiresRoundtripAfterRecentInbound 验证弱网模式不会因最近
// 收到过业务帧而跳过探活；每次复用都必须拿到本次 Ping 对应的 Pong（issue #442）。
func TestWeakNetworkProbeRequiresRoundtripAfterRecentInbound(t *testing.T) {
	setWeakNetworkModeForTest(t, true)
	manager, wc := newProbeDeafTestConnection(t)

	wc.touchInbound()
	if manager.probe(wc) {
		t.Fatal("weak-network mode must require a fresh ping/pong even after recent inbound activity")
	}
}

// TestProbeRoundtripsWhenInboundStale 入站活动过期后 probe 回退完整往返；
// 对端不回 Pong 时按超时判死，不能凭旧活跃放行。
func TestProbeRoundtripsWhenInboundStale(t *testing.T) {
	manager, wc := newProbeDeafTestConnection(t)

	wc.lastInbound.Store(time.Now().Add(-2 * probeRecencyWindow).UnixNano())
	if manager.probe(wc) {
		t.Fatal("probe must fall back to a full ping roundtrip once inbound activity is stale")
	}
}

// TestProbeRecentInboundDoesNotMaskDirtyLease 近期活跃不豁免 lease/队列检查：
// 有在途 lease 的连接即便刚有入站也不能被判定为可复用。
func TestProbeRecentInboundDoesNotMaskDirtyLease(t *testing.T) {
	manager, wc := newProbeDeafTestConnection(t)

	wc.touchInbound()
	if err := wc.BeginReadLease("in-flight-request"); err != nil {
		t.Fatalf("BeginReadLease: %v", err)
	}
	if manager.probe(wc) {
		t.Fatal("recent inbound must not bypass the lease/queue cleanliness check")
	}
}
