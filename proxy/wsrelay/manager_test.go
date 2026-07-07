package wsrelay

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
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
