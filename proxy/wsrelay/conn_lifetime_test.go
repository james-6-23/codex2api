package wsrelay

import (
	"io"
	"testing"
	"time"
)

// overAge 把连接的创建时间拨回到超过 MaxConnLifetime 之前（测试辅助）。
func overAge(wc *WsConnection) {
	wc.createdAt = time.Now().Add(-MaxConnLifetime - time.Minute).UnixNano()
}

// TestIsOverAge 验证连接年龄判断：到龄返回 true；createdAt 为 0（字面量构造）
// 视为未知年龄，不判到龄。
func TestIsOverAge(t *testing.T) {
	wc := &WsConnection{}
	if wc.IsOverAge() {
		t.Fatal("zero createdAt must not be treated as over-age")
	}
	wc.createdAt = time.Now().UnixNano()
	if wc.IsOverAge() {
		t.Fatal("fresh connection must not be over-age")
	}
	overAge(wc)
	if !wc.IsOverAge() {
		t.Fatal("connection older than MaxConnLifetime must be over-age")
	}
}

// TestCanReuseConnectionRejectsOverAge 验证到龄连接即使空闲、已连接也不可复用：
// 上游按连接建立时间计 60 分钟寿命，撞线后 response.create 一律失败但 Ping 探活
// 仍成功，复用判断必须看年龄 (issue #346)。
func TestCanReuseConnectionRejectsOverAge(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	if !canReuseConnection(wc) {
		t.Fatal("setup failed: fresh idle connection should be reusable")
	}
	overAge(wc)
	if canReuseConnection(wc) {
		t.Fatal("over-age connection must not be reusable")
	}
}

// TestEvictExpiredRotatesIdleOverAgeConnection 验证后台清理主动轮转到龄且空闲的
// 连接；到龄但仍有在途请求的连接保留，等请求结束后再轮转，不掐断在途流。
func TestEvictExpiredRotatesIdleOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	idle := addConnectedConn(t, m, 1, "s1")
	overAge(idle)

	busy := addConnectedConn(t, m, 2, "s2")
	overAge(busy)
	pr := busy.session.AddPendingRequest("s2")
	t.Cleanup(func() { busy.session.RemovePendingRequest(pr.RequestID) })

	fresh := addConnectedConn(t, m, 3, "s3")

	m.evictExpired()

	if _, ok := m.connections.Load(idle.PoolKey); ok {
		t.Fatal("idle over-age connection must be evicted for rotation")
	}
	if idle.IsConnected() {
		t.Fatal("evicted over-age connection must be closed")
	}
	if _, ok := m.connections.Load(busy.PoolKey); !ok {
		t.Fatal("over-age connection with in-flight request must be kept until it drains")
	}
	if _, ok := m.connections.Load(fresh.PoolKey); !ok {
		t.Fatal("fresh connection must survive eviction")
	}
}

// TestPingIdleConnectionsSkipsOverAgeConnection 验证保活不给到龄连接续命：
// Pong 会刷新 lastUsed 让连接永不空闲过期，若继续 Ping，开保活的实例上所有
// 连接最终都会撞上游 60 分钟寿命上限。
func TestPingIdleConnectionsSkipsOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	var pingedKeys []string
	m.keepalivePingFunc = func(wc *WsConnection) error {
		pingedKeys = append(pingedKeys, wc.PoolKey)
		return nil
	}

	aged := addConnectedConn(t, m, 1, "s1")
	overAge(aged)
	fresh := addConnectedConn(t, m, 2, "s2")

	pinged, failed := m.PingIdleConnections()
	if pinged != 1 || failed != 0 {
		t.Fatalf("pinged=%d failed=%d, want pinged=1 failed=0", pinged, failed)
	}
	if len(pingedKeys) != 1 || pingedKeys[0] != fresh.PoolKey {
		t.Fatalf("pingedKeys=%v, want only fresh connection %q", pingedKeys, fresh.PoolKey)
	}
}

// TestLookupResponseConnRejectsOverAgeConnection 验证续链亲和不会把请求定向回
// 到龄连接（那上面必然收到 websocket_connection_limit_reached）。
func TestLookupResponseConnRejectsOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	m.BindResponseConn("resp_1", wc, "s1", 1, "key-1")

	if got, _ := m.lookupResponseConn("resp_1", 1, "key-1"); got != wc {
		t.Fatal("setup failed: fresh binding should resolve to the connection")
	}
	overAge(wc)
	if got, _ := m.lookupResponseConn("resp_1", 1, "key-1"); got != nil {
		t.Fatal("binding to an over-age connection must not resolve")
	}
}

// TestConnLimitErrorFrameMarksConnBroken 验证上游 websocket_connection_limit_reached
// 错误帧（连接级限制）标记坏连接；普通错误帧（请求级）不标记，连接仍可归还复用。
func TestConnLimitErrorFrameMarksConnBroken(t *testing.T) {
	limitFrame := []byte(`{"type":"error","error":{"code":"websocket_connection_limit_reached","type":"invalid_request_error","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`)
	plainFrame := []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`)

	r := &WsResponse{}
	if err := r.handleMessage(limitFrame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r.mu.Lock()
	broken := r.connBroken
	r.mu.Unlock()
	if !broken {
		t.Fatal("connection-limit error frame must mark connection broken (issue #346)")
	}

	r2 := &WsResponse{}
	if err := r2.handleMessage(plainFrame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r2.mu.Lock()
	broken2 := r2.connBroken
	r2.mu.Unlock()
	if broken2 {
		t.Fatal("request-level error frame must not mark connection broken")
	}
}

// TestConnLimitErrorDiscardsConnectionOnClose 端到端验证 issue #346 的止血链路：
// 收到连接级限制错误帧后，Close() 把连接销毁并移出连接池，而不是归还复用
// 继续毒害后续请求。
func TestConnLimitErrorDiscardsConnectionOnClose(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	pr := wc.session.AddPendingRequest("s1")

	r := &WsResponse{conn: wc, pendingReq: pr, sessionID: "s1", manager: m}
	frame := []byte(`{"type":"error","error":{"code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes)."}}`)
	if err := r.handleMessage(frame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r.markStreamCompleted()

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := m.connections.Load(wc.PoolKey); ok {
		t.Fatal("connection that hit the upstream lifetime limit must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("connection that hit the upstream lifetime limit must be closed")
	}
}
