package wsrelay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestDiscardConnectionClosesAndRemovesFromPool 验证 DiscardConnection 关闭底层连接
// 并把连接/会话从池中移除。
func TestDiscardConnectionClosesAndRemovesFromPool(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	manager.DiscardConnection(wc)

	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected discarded connection to be removed from pool")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected discarded session to be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("expected discarded connection to be closed (state != connected)")
	}
}

// TestDiscardConnectionKeepsReplacedConnection 验证 DiscardConnection 使用
// CompareAndDelete 精确删除：当池中同 PoolKey 已被替换为新连接时，丢弃旧连接不得误删新连接。
func TestDiscardConnectionKeepsReplacedConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")

	oldSession := NewSession(1, manager)
	oldSession.SetConnected(true)
	oldConn := &WsConnection{session: oldSession, PoolKey: key}
	oldConn.SetState(StateConnected)

	// 池中已被替换为同 PoolKey 的新连接
	newSession := NewSession(1, manager)
	newSession.SetConnected(true)
	newConn := &WsConnection{session: newSession, PoolKey: key}
	newConn.SetState(StateConnected)
	newConn.Touch()
	manager.connections.Store(key, newConn)
	manager.sessions.Store(key, newSession)

	manager.DiscardConnection(oldConn)

	if v, ok := manager.connections.Load(key); !ok || v.(*WsConnection) != newConn {
		t.Fatal("DiscardConnection must not remove a replaced connection under the same key")
	}
	if v, ok := manager.sessions.Load(key); !ok || v.(*Session) != newSession {
		t.Fatal("DiscardConnection must not remove the replaced session under the same key")
	}
}

// TestWsResponseCloseDiscardsBrokenConnection 验证读流标记 connBroken 后，Close()
// 销毁坏连接并移出连接池，而不是归还复用。
func TestWsResponseCloseDiscardsBrokenConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}
	wsResp.markConnBroken()

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("broken connection must be removed from pool")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("broken session must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("broken connection must be closed")
	}
	if session.PendingCount() != 0 {
		t.Fatalf("pending must be cleared, got %d", session.PendingCount())
	}
}

// TestWsResponseCloseReleasesHealthyConnection 验证正常完成(读到终止边界且未标记
// connBroken)的响应关闭后，连接仍留在池中、保持 connected，可被后续请求复用。
func TestWsResponseCloseReleasesHealthyConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}
	wsResp.markStreamCompleted()

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); !ok {
		t.Fatal("healthy connection should remain in pool for reuse")
	}
	if !wc.IsConnected() {
		t.Fatal("healthy connection should stay connected")
	}
	if session.PendingCount() != 0 {
		t.Fatalf("pending must be cleared, got %d", session.PendingCount())
	}
}

// TestWsResponseCloseDiscardsIncompleteStream 验证读流未消费到终止边界(下游断开、
// ctx 取消等提前退出)时，Close() 销毁连接而不是归还池——上游可能仍在该连接上
// 推送残留帧，复用会把上一个请求的响应串给下一个用户 (issue #308)。
func TestWsResponseCloseDiscardsIncompleteStream(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	// 既没有 markStreamCompleted 也没有 markConnBroken：模拟流中途被放弃
	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("incomplete-stream connection must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("incomplete-stream connection must be closed")
	}
}

// TestHandleMessageDownstreamWriteFailureMarksConnBroken 复现 issue #308 的触发链路：
// 下游写入失败(callback 返回 false，如 broken pipe / 客户端断开)时，handleMessage
// 返回 io.EOF 让 ReadStream 结束，但必须标记 connBroken，使 Close() 销毁连接。
func TestHandleMessageDownstreamWriteFailureMarksConnBroken(t *testing.T) {
	r := &WsResponse{}

	err := r.handleMessage([]byte(`{"type":"response.output_text.delta","delta":"stale"}`), func(data []byte) bool {
		return false // 模拟 pw.Write 失败
	})
	if err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}

	r.mu.Lock()
	broken := r.connBroken
	r.mu.Unlock()
	if !broken {
		t.Fatal("downstream write failure must mark connection broken to prevent reuse (issue #308)")
	}
}

// TestReadStreamDownstreamWriteStopDiscardsConnection 端到端复现 issue #308：
// 上游持续推送帧，下游中途写入失败，ReadStream 正常返回(nil)，但 Close() 必须把
// 连接从池中移除并关闭底层 socket，绝不能留在池中被下一个请求复用。
func TestReadStreamDownstreamWriteStopDiscardsConnection(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		// 模拟上一个响应的多帧输出：下游在第一帧后就断开
		for range 3 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"stale"}`)); err != nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	// 下游第一帧就写失败（模拟 broken pipe / 客户端断开）
	err = wsResp.ReadStream(func(data []byte) bool { return false })
	if err != nil {
		t.Fatalf("ReadStream: %v (downstream stop is treated as end of stream)", err)
	}

	if cerr := wsResp.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("downstream write stop left websocket connection in pool; it should be discarded (issue #308)")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("downstream write stop left websocket session in pool; it should be discarded (issue #308)")
	}
	if wc.IsConnected() {
		t.Fatal("connection with unconsumed upstream frames must be closed")
	}
}

// TestReadStreamDiscardsConnectionOnAbnormalClose 端到端验证 issue #267 场景：
// 上游发送 close 1009 (message too big) 后，ReadStream 返回 read error 并标记坏连接，
// Close() 把连接从池中移除并关闭底层 socket（避免复用与 CLOSE_WAIT 滞留）。
func TestReadStreamDiscardsConnectionOnAbnormalClose(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		// 模拟上游异常关闭：发送 close 1009 (message too big)
		deadline := time.Now().Add(time.Second)
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"),
			deadline,
		)
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	err = wsResp.ReadStream(func(data []byte) bool { return true })
	if err == nil {
		t.Fatal("expected read error from abnormal close 1009")
	}
	if !strings.Contains(err.Error(), "websocket read error") {
		t.Fatalf("err = %v, want wrapped websocket read error", err)
	}

	if cerr := wsResp.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected broken connection removed from pool after abnormal close")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected broken session removed from pool after abnormal close")
	}
	if wc.IsConnected() {
		t.Fatal("expected broken connection to be closed")
	}
}
