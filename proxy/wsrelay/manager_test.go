package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func TestManagerStopIdempotent(t *testing.T) {
	manager := NewManager()

	for i := 0; i < 3; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Stop call %d panicked: %v", i+1, r)
				}
			}()
			manager.Stop()
		}()
	}
}

func TestManagerStopConcurrent(t *testing.T) {
	manager := NewManager()

	const callers = 32
	start := make(chan struct{})
	panicCh := make(chan any, callers)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)

	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicCh <- r
				}
			}()
			<-start
			manager.Stop()
		}()
	}

	close(start)
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Stop calls timed out")
	}
	close(panicCh)

	for r := range panicCh {
		t.Fatalf("concurrent Stop panicked: %v", r)
	}
}

func TestRemoveConnectionUsesEffectiveProxyKey(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	account := &auth.Account{DBID: 42, ProxyURL: " http://proxy-a.example:8080 "}
	wsURL := "wss://example.test/responses"
	sessionKey := "session-1"
	proxyURL := effectiveProxyURL(account, "")
	key := manager.poolKey(account.ID(), wsURL, sessionKey, proxyURL)

	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{session: session, URL: wsURL, PoolKey: key}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)

	manager.RemoveConnection(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, ""))

	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected connection stored under effective proxy key to be removed")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected session stored under effective proxy key to be removed")
	}
	if conn.IsConnected() {
		t.Fatal("expected removed connection to be closed")
	}
}

func TestAcquireConnectionReusesIdleConnectedConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	// 测试环境无真实 WebSocket 连接，注入探活函数跳过 Ping
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	key := manager.poolKey(account.ID(), wsURL, "session-1", "")

	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{
		session:  session,
		URL:      wsURL,
		PoolKey:  key,
		httpResp: &http.Response{StatusCode: http.StatusSwitchingProtocols},
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)

	got, pr, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-1", http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireConnection() error = %v", err)
	}
	if got != conn {
		t.Fatal("expected existing connection to be reused")
	}
	if pr == nil {
		t.Fatal("expected pending request reservation")
	}
	if session.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want %d", session.PendingCount(), 1)
	}
	session.RemovePendingRequest(pr.RequestID)
}

func TestAcquireConnectionProbeDoesNotBlockDifferentPoolKey(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 4}
	wsURL := "wss://example.test/responses"
	slowConn, _ := newTestSlotConnection(manager, account, wsURL, "session-slow")
	fastConn, _ := newTestSlotConnection(manager, account, wsURL, "session-fast")

	slowProbeStarted := make(chan struct{})
	releaseSlowProbe := make(chan struct{})
	var releaseOnce sync.Once
	releaseSlow := func() { releaseOnce.Do(func() { close(releaseSlowProbe) }) }
	defer releaseSlow()
	var startedOnce sync.Once
	manager.probeFunc = func(wc *WsConnection) bool {
		if wc == slowConn {
			startedOnce.Do(func() { close(slowProbeStarted) })
			<-releaseSlowProbe
		}
		return true
	}

	type result struct {
		wc      *WsConnection
		pending *PendingRequest
		err     error
	}
	slowResult := make(chan result, 1)
	go func() {
		wc, pending, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-slow", http.Header{}, "")
		slowResult <- result{wc: wc, pending: pending, err: err}
	}()
	select {
	case <-slowProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("slow pool-key probe did not start")
	}

	fastResult := make(chan result, 1)
	go func() {
		wc, pending, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-fast", http.Header{}, "")
		fastResult <- result{wc: wc, pending: pending, err: err}
	}()
	select {
	case got := <-fastResult:
		if got.err != nil || got.wc != fastConn || got.pending == nil {
			t.Fatalf("fast acquire = (%p, %v, %v), want existing healthy connection", got.wc, got.pending, got.err)
		}
		got.wc.session.RemovePendingRequest(got.pending.RequestID)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("different pool-key acquire was blocked by another connection's network probe")
	}
	releaseSlow()
	slow := <-slowResult
	if slow.err != nil || slow.wc != slowConn || slow.pending == nil {
		t.Fatalf("slow acquire after probe release = (%p, %v, %v)", slow.wc, slow.pending, slow.err)
	}
	slow.wc.session.RemovePendingRequest(slow.pending.RequestID)
}

func TestPoolKeyIncludesSessionKey(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	keyA := manager.poolKey(42, "wss://example.test/responses", "session-a", "")
	keyB := manager.poolKey(42, "wss://example.test/responses", "session-b", "")
	if keyA == keyB {
		t.Fatal("expected different session keys to produce different pool keys")
	}
}

func TestPoolKeyKeepsSameSessionStable(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	keyA := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-a")
	keyB := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-a")
	if keyA != keyB {
		t.Fatal("expected identical session keys to produce the same pool key")
	}
}

func TestPoolKeyIncludesProxyScope(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	keyA := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-a")
	keyB := manager.poolKey(42, "wss://example.test/responses", "session-a", "http://proxy-b")
	if keyA == keyB {
		t.Fatal("expected different proxies to produce different pool keys")
	}
}

func TestCanReuseConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	t.Run("idle connected session can be reused", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.Touch()

		if !canReuseConnection(conn) {
			t.Fatal("expected connection to be reusable")
		}
	})

	t.Run("pending request blocks reuse", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		pending := session.AddPendingRequest("session-a")
		t.Cleanup(func() { session.RemovePendingRequest(pending.RequestID) })

		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.Touch()

		if canReuseConnection(conn) {
			t.Fatal("expected connection with pending request to be non-reusable")
		}
	})

	t.Run("expired connection cannot be reused", func(t *testing.T) {
		session := NewSession(42, manager)
		session.SetConnected(true)
		conn := &WsConnection{session: session}
		conn.SetState(StateConnected)
		conn.lastUsed.Store(time.Now().Add(-IdleTimeout - time.Second).UnixNano())

		if canReuseConnection(conn) {
			t.Fatal("expected expired connection to be non-reusable")
		}
	})
}

func TestAcquireConnectionWaitsWhileSessionHasPendingRequest(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	key := manager.poolKey(account.ID(), wsURL, "session-1", "")

	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	blocking := session.AddPendingRequest("session-1")
	t.Cleanup(func() { session.RemovePendingRequest(blocking.RequestID) })

	conn := &WsConnection{
		session:  session,
		URL:      wsURL,
		PoolKey:  key,
		httpResp: &http.Response{StatusCode: http.StatusSwitchingProtocols},
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, _, err := manager.AcquireConnection(ctx, account, wsURL, "session-1", http.Header{}, "")
	if err == nil {
		t.Fatal("expected acquire to stop when session stays busy until context timeout")
	}
}

func TestAcquireConnectionCapsIdleConnectionsAtAccountConcurrency(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 2}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	connections := make([]*WsConnection, 0, 3)

	for i := 0; i < 3; i++ {
		wc, pending, err := manager.AcquireConnection(
			context.Background(),
			account,
			wsURL,
			fmt.Sprintf("session-%d", i),
			http.Header{},
			"",
		)
		if err != nil {
			t.Fatalf("AcquireConnection(%d) error = %v", i, err)
		}
		wc.session.RemovePendingRequest(pending.RequestID)
		manager.ReleaseConnection(wc)
		connections = append(connections, wc)
		time.Sleep(2 * time.Millisecond)
	}

	if got := manager.ConnectionCount(); got != 2 {
		t.Fatalf("ConnectionCount = %d, want account concurrency cap 2", got)
	}
	if connections[0].IsConnected() {
		t.Fatal("oldest idle connection should be evicted when the account cap is reached")
	}
}

func TestAcquireConnectionTrimsIdleConnectionsAfterDynamicLimitDecrease(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 3}
	wsURL := "wss://example.test/responses"
	connections := make([]*WsConnection, 0, 3)

	for i := 0; i < 3; i++ {
		wc, _ := newTestSlotConnection(manager, account, wsURL, fmt.Sprintf("session-%d", i))
		connections = append(connections, wc)
	}
	if got := manager.ConnectionCount(); got != 3 {
		t.Fatalf("ConnectionCount before limit decrease = %d, want 3", got)
	}

	account.Mu().Lock()
	account.DynamicConcurrencyLimit = 1
	account.Mu().Unlock()
	protected := connections[1]
	got, pending, err := manager.AcquireConnection(
		context.Background(), account, wsURL, "session-1", http.Header{}, "",
	)
	if err != nil {
		t.Fatalf("AcquireConnection after limit decrease error = %v", err)
	}
	if got != protected {
		t.Fatal("existing session connection should be reused after the limit decrease")
	}
	if count := manager.ConnectionCount(); count != 1 {
		t.Fatalf("ConnectionCount after limit decrease = %d, want 1", count)
	}
	if !protected.IsConnected() {
		t.Fatal("the connection selected for reuse must remain connected")
	}
	for i, wc := range connections {
		if wc != protected && wc.IsConnected() {
			t.Fatalf("idle connection %d remained connected after the limit decreased", i)
		}
	}
	got.session.RemovePendingRequest(pending.RequestID)
}

func TestAcquireConnectionCountsPendingDialTowardAccountCap(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			close(started)
		}
		<-release
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 1}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	type acquireResult struct {
		wc      *WsConnection
		pending *PendingRequest
		err     error
	}
	firstResult := make(chan acquireResult, 1)
	go func() {
		wc, pending, err := manager.AcquireConnection(
			context.Background(), account, wsURL, "session-first", http.Header{}, "",
		)
		firstResult <- acquireResult{wc: wc, pending: pending, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first websocket dial did not reach the server")
	}

	secondCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, _, secondErr := manager.AcquireConnection(
		secondCtx, account, wsURL, "session-second", http.Header{}, "",
	)
	if secondErr == nil {
		t.Fatal("second dial should wait for account connection capacity")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("websocket dial attempts = %d, want 1 while first dial is pending", got)
	}

	close(release)
	result := <-firstResult
	if result.err != nil {
		t.Fatalf("first AcquireConnection error = %v", result.err)
	}
	result.wc.session.RemovePendingRequest(result.pending.RequestID)
}

func newTestSlotConnection(manager *Manager, account *auth.Account, wsURL, slotSession string) (*WsConnection, *Session) {
	key := manager.poolKey(account.ID(), wsURL, slotSession, "")
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{
		session:  session,
		URL:      wsURL,
		PoolKey:  key,
		httpResp: &http.Response{StatusCode: http.StatusSwitchingProtocols},
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)
	return conn, session
}

func TestAcquireReusableConnectionReusesIdleSlot(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	conn, _ := newTestSlotConnection(manager, account, wsURL, "cache-key#0")

	got, pr, usedKey, err := manager.AcquireReusableConnection(context.Background(), account, wsURL, "cache-key", "stateless-xyz", 4, http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireReusableConnection() error = %v", err)
	}
	if got != conn {
		t.Fatal("expected idle slot connection to be reused")
	}
	if usedKey != "cache-key#0" {
		t.Fatalf("usedKey = %q, want cache-key#0", usedKey)
	}
	got.session.RemovePendingRequest(pr.RequestID)
}

func TestAcquireReusableConnectionProbeDoesNotBlockDifferentPoolKey(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 4}
	wsURL := "wss://example.test/responses"
	slowConn, _ := newTestSlotConnection(manager, account, wsURL, "slow-cache#0")
	fastConn, _ := newTestSlotConnection(manager, account, wsURL, "fast-cache#0")

	slowProbeStarted := make(chan struct{})
	releaseSlowProbe := make(chan struct{})
	var releaseOnce sync.Once
	releaseSlow := func() { releaseOnce.Do(func() { close(releaseSlowProbe) }) }
	defer releaseSlow()
	var startedOnce sync.Once
	manager.probeFunc = func(wc *WsConnection) bool {
		if wc == slowConn {
			startedOnce.Do(func() { close(slowProbeStarted) })
			<-releaseSlowProbe
		}
		return true
	}

	type result struct {
		wc      *WsConnection
		pending *PendingRequest
		key     string
		err     error
	}
	slowResult := make(chan result, 1)
	go func() {
		wc, pending, key, err := manager.AcquireReusableConnection(context.Background(), account, wsURL, "slow-cache", "slow-fallback", 4, http.Header{}, "")
		slowResult <- result{wc: wc, pending: pending, key: key, err: err}
	}()
	select {
	case <-slowProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("slow reusable-slot probe did not start")
	}

	fastResult := make(chan result, 1)
	go func() {
		wc, pending, key, err := manager.AcquireReusableConnection(context.Background(), account, wsURL, "fast-cache", "fast-fallback", 4, http.Header{}, "")
		fastResult <- result{wc: wc, pending: pending, key: key, err: err}
	}()
	select {
	case got := <-fastResult:
		if got.err != nil || got.wc != fastConn || got.pending == nil || got.key != "fast-cache#0" {
			t.Fatalf("fast reusable acquire = (%p, %v, %q, %v), want healthy fast-cache#0", got.wc, got.pending, got.key, got.err)
		}
		got.wc.session.RemovePendingRequest(got.pending.RequestID)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("different reusable pool key was blocked by another connection's network probe")
	}
	releaseSlow()
	slow := <-slowResult
	if slow.err != nil || slow.wc != slowConn || slow.pending == nil || slow.key != "slow-cache#0" {
		t.Fatalf("slow reusable acquire after probe release = (%p, %v, %q, %v)", slow.wc, slow.pending, slow.key, slow.err)
	}
	slow.wc.session.RemovePendingRequest(slow.pending.RequestID)
}

func TestAcquireReusableConnectionSkipsBusySlot(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	_, busySession := newTestSlotConnection(manager, account, wsURL, "cache-key#0")
	busySession.AddPendingRequest("cache-key#0") // 占用 slot 0
	idleConn, _ := newTestSlotConnection(manager, account, wsURL, "cache-key#1")

	got, pr, usedKey, err := manager.AcquireReusableConnection(context.Background(), account, wsURL, "cache-key", "stateless-xyz", 4, http.Header{}, "")
	if err != nil {
		t.Fatalf("AcquireReusableConnection() error = %v", err)
	}
	if got != idleConn {
		t.Fatal("expected busy slot 0 to be skipped and idle slot 1 reused")
	}
	if usedKey != "cache-key#1" {
		t.Fatalf("usedKey = %q, want cache-key#1", usedKey)
	}
	got.session.RemovePendingRequest(pr.RequestID)
}

// ==================== 续链亲和(response_id → 连接绑定) ====================

func newBoundTestConn(t *testing.T, manager *Manager, accountID int64, sessionKey string) *WsConnection {
	t.Helper()
	key := manager.poolKey(accountID, "wss://example.test/responses", sessionKey, "")
	session := NewSession(accountID, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)
	return wc
}

func TestBindAndLookupResponseConn(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	wc := newBoundTestConn(t, manager, 7, "base#0")
	manager.BindResponseConn("resp_abc", wc, "base#0", 7, "key-A")

	got, slotKey := manager.lookupResponseConn("resp_abc", 7, "key-A")
	if got != wc {
		t.Fatal("lookup should return the bound connection")
	}
	if slotKey != "base#0" {
		t.Fatalf("slotKey = %q, want base#0", slotKey)
	}

	// 账号不匹配 → miss(续链换号后不得复用别人账号的连接)
	if got, _ := manager.lookupResponseConn("resp_abc", 8, "key-A"); got != nil {
		t.Fatal("lookup with wrong account must miss")
	}

	// 连接被移出池(销毁/重建) → miss
	manager.connections.Delete(wc.PoolKey)
	if got, _ := manager.lookupResponseConn("resp_abc", 7, "key-A"); got != nil {
		t.Fatal("lookup after conn removed from pool must miss")
	}
}

func TestLookupResponseConnRejectsRebuiltSlot(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	wc := newBoundTestConn(t, manager, 7, "base#0")
	manager.BindResponseConn("resp_old", wc, "base#0", 7, "key-A")

	// 同 PoolKey 槽位被重建为新连接:旧绑定必须失效(指针校验)
	replacement := &WsConnection{session: NewSession(7, manager), URL: wc.URL, PoolKey: wc.PoolKey}
	replacement.SetState(StateConnected)
	replacement.Touch()
	manager.connections.Store(wc.PoolKey, replacement)

	if got, _ := manager.lookupResponseConn("resp_old", 7, "key-A"); got != nil {
		t.Fatal("binding to a replaced connection must miss")
	}
}

func TestAcquirePreferredConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	wc := newBoundTestConn(t, manager, 7, "base#3")
	manager.BindResponseConn("resp_chain", wc, "base#3", 7, "key-A")

	got, pr, slotKey := manager.AcquirePreferredConnection("resp_chain", 7, "key-A")
	if got != wc {
		t.Fatal("AcquirePreferredConnection should return the bound connection")
	}
	if pr == nil {
		t.Fatal("pending request must be registered")
	}
	if slotKey != "base#3" {
		t.Fatalf("slotKey = %q, want base#3", slotKey)
	}
	if wc.session.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", wc.session.PendingCount())
	}

	// 连接忙(已有在途请求)时不等待,直接回退常规路径
	got2, pr2, _ := manager.AcquirePreferredConnection("resp_chain", 7, "key-A")
	if got2 != nil || pr2 != nil {
		t.Fatal("busy preferred connection must not be acquired")
	}
}

func TestAcquirePreferredConnectionProbeDoesNotBlockDifferentPoolKey(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	slowConn := newBoundTestConn(t, manager, 7, "slow#0")
	fastConn := newBoundTestConn(t, manager, 7, "fast#0")
	manager.BindResponseConn("resp_slow", slowConn, "slow#0", 7, "key-A")
	manager.BindResponseConn("resp_fast", fastConn, "fast#0", 7, "key-A")

	slowProbeStarted := make(chan struct{})
	releaseSlowProbe := make(chan struct{})
	var releaseOnce sync.Once
	releaseSlow := func() { releaseOnce.Do(func() { close(releaseSlowProbe) }) }
	defer releaseSlow()
	var startedOnce sync.Once
	manager.probeFunc = func(wc *WsConnection) bool {
		if wc == slowConn {
			startedOnce.Do(func() { close(slowProbeStarted) })
			<-releaseSlowProbe
		}
		return true
	}

	type result struct {
		wc      *WsConnection
		pending *PendingRequest
		key     string
	}
	slowResult := make(chan result, 1)
	go func() {
		wc, pending, key := manager.AcquirePreferredConnection("resp_slow", 7, "key-A")
		slowResult <- result{wc: wc, pending: pending, key: key}
	}()
	select {
	case <-slowProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("slow preferred-connection probe did not start")
	}

	fastResult := make(chan result, 1)
	go func() {
		wc, pending, key := manager.AcquirePreferredConnection("resp_fast", 7, "key-A")
		fastResult <- result{wc: wc, pending: pending, key: key}
	}()
	select {
	case got := <-fastResult:
		if got.wc != fastConn || got.pending == nil || got.key != "fast#0" {
			t.Fatalf("fast preferred acquire = (%p, %v, %q), want healthy fast#0", got.wc, got.pending, got.key)
		}
		got.wc.session.RemovePendingRequest(got.pending.RequestID)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("different preferred pool key was blocked by another connection's network probe")
	}
	releaseSlow()
	slow := <-slowResult
	if slow.wc != slowConn || slow.pending == nil || slow.key != "slow#0" {
		t.Fatalf("slow preferred acquire after probe release = (%p, %v, %q)", slow.wc, slow.pending, slow.key)
	}
	slow.wc.session.RemovePendingRequest(slow.pending.RequestID)
}

func TestAcquirePreferredConnectionProbeFailureEvicts(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return false }

	wc := newBoundTestConn(t, manager, 7, "base#0")
	manager.BindResponseConn("resp_dead", wc, "base#0", 7, "key-A")

	got, pr, _ := manager.AcquirePreferredConnection("resp_dead", 7, "key-A")
	if got != nil || pr != nil {
		t.Fatal("dead preferred connection must not be acquired")
	}
	if _, ok := manager.connections.Load(wc.PoolKey); ok {
		t.Fatal("dead connection must be evicted from pool")
	}
}

func TestBindResponseConnBounded(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	wc := newBoundTestConn(t, manager, 7, "base#0")
	for i := 0; i < responseConnBindingMaxEntries+10; i++ {
		manager.BindResponseConn(fmt.Sprintf("resp_%d", i), wc, "base#0", 7, "key-A")
	}
	manager.respConnMu.Lock()
	size := len(manager.respConnBindings)
	manager.respConnMu.Unlock()
	if size > responseConnBindingMaxEntries {
		t.Fatalf("binding map size = %d, exceeds cap %d", size, responseConnBindingMaxEntries)
	}
}

func TestLookupResponseConnIsolatesAPIKeys(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	wc := newBoundTestConn(t, manager, 7, "base#0")
	manager.BindResponseConn("resp_owned", wc, "base#0", 7, "key-A")

	// 别的 API Key 拿着同一个 response_id 不得命中(防跨 Key 定向挤连接)
	if got, _ := manager.lookupResponseConn("resp_owned", 7, "key-B"); got != nil {
		t.Fatal("lookup with different api key must miss")
	}
	if got, _ := manager.lookupResponseConn("resp_owned", 7, ""); got != nil {
		t.Fatal("lookup with empty api key must miss when binding has one")
	}
	if got, _ := manager.lookupResponseConn("resp_owned", 7, "key-A"); got != wc {
		t.Fatal("owner api key must hit")
	}
}

// 上游对 WS upgrade 回 401 时，结构化握手错误必须原样穿透 AcquireConnection
// 的传播链（不被二次包装），执行器才能把它还原成真实状态码的 HTTP 响应；
// 否则 401 在使用日志里只会以 transport/598 出现且账号不触发 unauthorized 冷却。
func TestAcquireConnectionDial401ReturnsTypedHandshakeError(t *testing.T) {
	upstreamBody := `{"error":{"message":"Provided authentication token is expired.","type":"invalid_request_error","code":"token_expired"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	manager := NewManager()
	t.Cleanup(manager.Stop)

	account := &auth.Account{DBID: 42}
	wsURL := strings.Replace(upstream.URL, "http://", "ws://", 1)

	_, _, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-1", http.Header{}, "")
	if err == nil {
		t.Fatal("expected handshake error")
	}

	var hs *HandshakeHTTPError
	if !errors.As(err, &hs) {
		t.Fatalf("expected *HandshakeHTTPError to survive propagation, got %T: %v", err, err)
	}
	if hs.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", hs.StatusCode)
	}

	resp, ok := handshakeUnauthorizedHTTPResponse(err)
	if !ok {
		t.Fatal("expected 401 handshake error to convert to HTTP response")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("converted StatusCode = %d, want 401", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	// readHTTPErrorBody 会重排 JSON 键序，按字段断言。
	if !strings.Contains(string(got), `"code":"token_expired"`) || strings.Contains(string(got), "websocket handshake failed") {
		t.Fatalf("converted body should be raw upstream json, got %q", got)
	}
}
