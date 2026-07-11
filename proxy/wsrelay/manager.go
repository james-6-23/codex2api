package wsrelay

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

// ==================== 连接池管理器 ====================

// ConnectionState 连接状态
type ConnectionState int32

const (
	StateDisconnected ConnectionState = 0
	StateConnecting   ConnectionState = 1
	StateConnected    ConnectionState = 2
	StateClosing      ConnectionState = 3
)

// WsConnection WebSocket 连接包装
type WsConnection struct {
	// WebSocket 连接
	conn *websocket.Conn

	// 会话
	session *Session

	// 连接 URL
	URL string

	// 连接池键
	PoolKey string

	// 连接状态
	state atomic.Int32

	// 最后使用时间
	lastUsed atomic.Int64

	// 创建时间（UnixNano），用于连接年龄判断（上游有 60 分钟连接寿命上限）。
	// 构造后不再修改；为 0 表示未知（测试用字面量构造），视为未到龄。
	createdAt int64

	// 写操作锁
	writeMu sync.Mutex

	// 永久 reader、业务帧 lease 与探活状态。读取状态按需初始化，兼容测试中
	// 通过字面量构造且没有底层 socket 的 WsConnection。
	readStateOnce       sync.Once
	readPumpOnce        sync.Once
	readFailureOnce     sync.Once
	controlHandlersOnce sync.Once
	readState           *wsReadState

	probeGateOnce sync.Once
	probeGate     chan struct{}
	probeStateMu  sync.Mutex
	probePayload  string
	probeResult   chan struct{}

	// 底层 socket 与断开回调只关闭/调用一次。
	closeOnce          sync.Once
	closeErr           error
	disconnectNotified atomic.Bool

	// HTTP 握手响应
	httpResp *http.Response

	// 连接关闭回调
	onDisconnected func(accountID int64)

	// 永久 reader 失败回调。Manager 使用指针级 CompareAndDelete 精确移除
	// 当前连接，避免误删同 PoolKey 下已经重建的连接。
	onReadFailure func(wc *WsConnection)
}

func effectiveProxyURL(account *auth.Account, proxyOverride string) string {
	proxyURL := ""
	if account != nil {
		account.Mu().RLock()
		proxyURL = account.ProxyURL
		account.Mu().RUnlock()
	}
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = proxyOverride
	}
	return strings.TrimSpace(proxyURL)
}

// NewWsConnection 创建 WebSocket 连接
func NewWsConnection(conn *websocket.Conn, session *Session, wsURL string) *WsConnection {
	wc := &WsConnection{
		conn:      conn,
		session:   session,
		URL:       wsURL,
		createdAt: time.Now().UnixNano(),
	}
	wc.lastUsed.Store(time.Now().UnixNano())
	wc.state.Store(int32(StateConnected))
	return wc
}

// Touch 更新最后使用时间
func (wc *WsConnection) Touch() {
	wc.lastUsed.Store(time.Now().UnixNano())
}

// IsExpired 检查连接是否过期
func (wc *WsConnection) IsExpired() bool {
	lastUsed := time.Unix(0, wc.lastUsed.Load())
	return time.Since(lastUsed) > IdleTimeout
}

// IsOverAge 检查连接是否超过最大寿命（MaxConnLifetime）。到龄连接不能再接新请求：
// 上游按连接建立时间计 60 分钟寿命，撞线后 response.create 一律报错，但 Ping
// 探活仍成功，必须按年龄主动识别。
func (wc *WsConnection) IsOverAge() bool {
	if wc.createdAt == 0 {
		return false
	}
	return time.Since(time.Unix(0, wc.createdAt)) > MaxConnLifetime
}

// IsConnected 检查是否已连接
func (wc *WsConnection) IsConnected() bool {
	return wc.state.Load() == int32(StateConnected)
}

// Close 安全关闭连接
func (wc *WsConnection) Close() error {
	if wc == nil {
		return nil
	}
	wc.closeOnce.Do(func() {
		wc.state.Store(int32(StateClosing))
		if wc.conn != nil {
			wc.closeErr = wc.conn.Close()
		}
		wc.state.Store(int32(StateDisconnected))
	})
	if wc.onDisconnected != nil && wc.session != nil && wc.disconnectNotified.CompareAndSwap(false, true) {
		wc.onDisconnected(wc.session.AccountID)
	}
	return wc.closeErr
}

// SetState 设置连接状态
func (wc *WsConnection) SetState(state ConnectionState) {
	wc.state.Store(int32(state))
}

// WriteMessage 安全写入消息
func (wc *WsConnection) WriteMessage(messageType int, data []byte) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() || wc.conn == nil {
		return fmt.Errorf("websocket connection is not connected")
	}
	leaseID, tracksLease, err := wc.beginReadLeaseWrite(messageType)
	if err != nil {
		return err
	}

	wc.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	defer wc.conn.SetWriteDeadline(time.Time{})

	writeErr := wc.conn.WriteMessage(messageType, data)
	if tracksLease {
		return wc.completeReadLeaseWrite(leaseID, writeErr)
	}
	return writeErr
}

// HTTPResponse 返回 HTTP 握手响应
func (wc *WsConnection) HTTPResponse() *http.Response {
	return wc.httpResp
}

// ==================== 连接池管理器 ====================

// Manager WebSocket 连接池管理器
type Manager struct {
	// 连接池（accountID -> *WsConnection）
	connections sync.Map

	// 会话池（accountID -> *Session）
	sessions sync.Map

	// 拨号器配置
	dialer *websocket.Dialer

	// 清理定时器
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
	stopOnce      sync.Once

	// 连接回调
	onConnected    func(accountID int64, session *Session)
	onDisconnected func(accountID int64)

	// 读写锁保护回调设置
	mu sync.RWMutex

	// pool key 级别串行化，避免同一逻辑 session 在 acquire 阶段竞争同一条连接
	keyLocks sync.Map

	// response_id -> 连接 绑定（续链亲和）。上游 chatgpt backend 无服务端存储时，
	// previous_response_id 的上下文只存活在产生该响应的那条 WS 连接里；带续链 ID
	// 的请求必须回到原连接，落到别的槽位会得到 "previous response not found"。
	// 参考 sub2api openai_ws_state_store 的 BindResponseConn/GetResponseConn。
	respConnMu       sync.Mutex
	respConnBindings map[string]responseConnBinding

	// 可选的探活函数（用于测试替换），nil 时使用默认 probeConnection
	probeFunc func(wc *WsConnection) bool

	// 可选的保活 Ping 函数（用于测试替换），nil 时使用默认 SendHeartbeat
	keepalivePingFunc func(wc *WsConnection) error
}

// responseConnBinding 记录某个 response_id 由哪条连接产出。
// conn 指针同时用作身份校验：同 poolKey 下连接被重建后旧绑定自动失效。
// apiKey 为产出该响应的下游 API Key（明文，仅存内存），lookup 时要求匹配，
// 防止跨 Key 用他人 response_id 定向挤上他人连接（与 response cache 的
// owner 隔离同一原则）。
type responseConnBinding struct {
	conn       *WsConnection
	sessionKey string
	accountID  int64
	apiKey     string
	expiresAt  time.Time
}

const (
	// responseConnBindingTTL 续链绑定的存活时间。上游空闲连接本身在 IdleTimeout
	// (5min) 后被清理，绑定活得再久也无意义，与其对齐。
	responseConnBindingTTL = IdleTimeout
	// responseConnBindingMaxEntries 绑定表上限，防止内存膨胀。
	responseConnBindingMaxEntries = 4096
)

// wsWriteBufferPool 在所有上游 WS 连接间共享写缓冲，降低高并发下的内存占用。
var wsWriteBufferPool = &sync.Pool{}

// NewManager 创建连接池管理器
func NewManager() *Manager {
	m := &Manager{
		dialer: &websocket.Dialer{
			HandshakeTimeout:  HandshakeTimeout,
			EnableCompression: true,
			// 上游 Codex WS 帧可达 48-91KB，默认 4KB 缓冲会导致单帧多轮 syscall；
			// 调大到 64KB 减少读写循环次数，写缓冲走共享池复用。
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			WriteBufferPool: wsWriteBufferPool,
			NetDialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		stopCleanup: make(chan struct{}),
	}

	// 启动后台清理
	m.cleanupTicker = time.NewTicker(30 * time.Second)
	go m.cleanupLoop()

	return m
}

// cleanupLoop 定期清理过期连接
func (m *Manager) cleanupLoop() {
	for {
		select {
		case <-m.cleanupTicker.C:
			m.evictExpired()
		case <-m.stopCleanup:
			m.cleanupTicker.Stop()
			return
		}
	}
}

// evictExpired 清理过期连接和会话（含到龄且空闲的连接，主动轮转避免撞上游寿命上限）
func (m *Manager) evictExpired() {
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		if wc.IsExpired() || !wc.IsConnected() || isRotatableOverAge(wc) {
			m.connections.Delete(key)
			wc.Close()
		}
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		if s.IsExpired() || !s.IsConnected() {
			m.sessions.Delete(key)
			s.Close()
		}
		return true
	})
}

// Stop 停止管理器
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCleanup)
		m.closeAll()
	})
}

// closeAll 关闭所有连接
func (m *Manager) closeAll() {
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		m.connections.Delete(key)
		wc.Close()
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		m.sessions.Delete(key)
		s.Close()
		return true
	})
}

// SetOnConnected 设置连接回调
func (m *Manager) SetOnConnected(fn func(accountID int64, session *Session)) {
	m.mu.Lock()
	m.onConnected = fn
	m.mu.Unlock()
}

// SetOnDisconnected 设置断开回调
func (m *Manager) SetOnDisconnected(fn func(accountID int64)) {
	m.mu.Lock()
	m.onDisconnected = fn
	m.mu.Unlock()
}

// getOnDisconnected 获取断开回调
func (m *Manager) getOnDisconnected() func(accountID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onDisconnected
}

// getOnConnected 获取连接回调
func (m *Manager) getOnConnected() func(accountID int64, session *Session) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onConnected
}

func (m *Manager) keyLock(key string) *sync.Mutex {
	if v, ok := m.keyLocks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	if actual, loaded := m.keyLocks.LoadOrStore(key, mu); loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

// AcquireConnection 获取或创建连接
// 仅在同一逻辑 session 且连接空闲时复用，避免不同会话共用一条已握手连接。
func (m *Manager) AcquireConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	key := m.poolKey(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))
	lock := m.keyLock(key)
	wait := AcquireInitialBackoff
	var waited time.Duration
	var createLeaseFailures int

	for {
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				// 发送 Ping 探活，确认连接真正存活
				if m.probe(wc) {
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey)
					if leaseErr == nil {
						wc.Touch()
						lock.Unlock()
						return wc, pr, nil
					}
					m.DiscardConnection(wc)
					lock.Unlock()
					continue
				}
				// 探活失败，清理死连接
				m.DiscardConnection(wc)
				lock.Unlock()
				continue
			}
			if wc.IsConnected() && !wc.IsExpired() && wc.session != nil && wc.session.PendingCount() > 0 && !isRotatableOverAge(wc) {
				lock.Unlock()
				// 连接被同 session 的前一个请求占用：指数退避轮询等待其空闲，
				// 累计等待超过上限则返回错误，避免无界阻塞与固定间隔空转抢锁。
				// 到龄连接也会走到这里等在途请求结束，结束后下一轮循环轮转重建。
				if waited >= AcquireMaxWait {
					return nil, nil, fmt.Errorf("acquire websocket connection timed out after %s waiting for busy session", AcquireMaxWait)
				}
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-time.After(wait):
				}
				waited += wait
				if wait < AcquireMaxBackoff {
					wait *= 2
					if wait > AcquireMaxBackoff {
						wait = AcquireMaxBackoff
					}
				}
				continue
			}
			m.DiscardConnection(wc)
		}

		wc, err := m.createConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
		if err != nil {
			lock.Unlock()
			return nil, nil, err
		}

		// 存储新连接并立即占位 pending request，避免返回后才记账产生竞态
		m.connections.Store(key, wc)
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			createLeaseFailures++
			if createLeaseFailures >= maxCreateLeaseAttempts {
				return nil, nil, fmt.Errorf("reserve new websocket connection after %d attempts: %w", createLeaseFailures, leaseErr)
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}
			continue
		}
		lock.Unlock()

		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}

		return wc, pr, nil
	}
}

// StatelessConnectionSlots 无显式会话的请求在每个 (account, cacheKey) 维度下
// 复用的持久连接槽位数。槽位内空闲连接直接复用,避免每个请求都重新握手——
// 持续高 RPM 下逐请求握手会触发上游 WS 握手限流（bad handshake → 503）。
const StatelessConnectionSlots = 8

// maxCreateLeaseAttempts bounds retries when a freshly completed handshake is
// already rejected by its permanent reader before the first request lease can
// be reserved (for example, an immediately queued peer Close frame).
const maxCreateLeaseAttempts = 3

// Give the permanent reader a small, bounded window to surface a Close/error
// already queued with the handshake before returning a newly reserved lease.
const newConnectionReadFailureGrace = 5 * time.Millisecond

// AcquireReusableConnection 在固定槽位内复用或创建连接，返回实际使用的 session key。
// 第一遍只复用已存在且空闲的连接；第二遍在空槽位新建持久连接；槽位全忙时回退到
// fallbackKey 的一次性连接，保持与原 stateless 行为一致的并发上限（无上限）。
func (m *Manager) AcquireReusableConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseKey string,
	fallbackKey string,
	slots int,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, string, error) {
	proxyURL := effectiveProxyURL(account, proxyOverride)
	// 第一遍：复用空闲连接（探活失败或已断开的顺手清理，让第二遍可以补位）
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock := m.keyLock(key)
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.probe(wc) {
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession)
					if leaseErr == nil {
						wc.Touch()
						lock.Unlock()
						return wc, pr, slotSession, nil
					}
					m.DiscardConnection(wc)
					lock.Unlock()
					continue
				}
				m.DiscardConnection(wc)
			} else if !wc.IsConnected() || wc.IsExpired() || isRotatableOverAge(wc) || wc.session == nil || wc.session.PendingCount() == 0 {
				m.DiscardConnection(wc)
			}
		}
		lock.Unlock()
	}
	// 第二遍：在空槽位新建持久连接
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock := m.keyLock(key)
		lock.Lock()
		if _, ok := m.connections.Load(key); ok {
			lock.Unlock()
			continue
		}
		wc, err := m.createConnection(ctx, account, wsURL, slotSession, headers, proxyOverride)
		if err != nil {
			lock.Unlock()
			return nil, nil, "", err
		}
		m.connections.Store(key, wc)
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, "", ctx.Err()
			}
			continue
		}
		lock.Unlock()
		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, slotSession, nil
	}
	// 槽位全忙：回退一次性连接
	wc, pr, err := m.AcquireConnection(ctx, account, wsURL, fallbackKey, headers, proxyOverride)
	return wc, pr, fallbackKey, err
}

// addPendingAndBeginReadLease keeps the Session reservation and the pump lease
// atomic from an acquire caller's perspective. On failure it rolls the pending
// request back; the caller discards the unusable connection while holding its
// pool-key acquisition lock.
func (m *Manager) addPendingAndBeginReadLease(wc *WsConnection, sessionKey string) (*PendingRequest, error) {
	if wc == nil || wc.session == nil {
		return nil, fmt.Errorf("begin websocket read lease: connection has no session")
	}
	pr := wc.session.AddPendingRequest(sessionKey)
	if err := wc.BeginReadLease(pr.RequestID); err != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		return nil, fmt.Errorf("reserve websocket connection: %w", err)
	}
	return pr, nil
}

func canReuseConnection(wc *WsConnection) bool {
	if wc == nil {
		return false
	}
	if !wc.IsConnected() || wc.IsExpired() || wc.IsOverAge() {
		return false
	}
	if wc.session == nil {
		return false
	}
	return wc.session.PendingCount() == 0 && wc.readPumpReusable()
}

// isRotatableOverAge 连接已到龄且当前无在途请求，可安全轮转（销毁重建）。
// 到龄但仍有在途请求的连接不动：50 分钟阈值留了 10 分钟余量，在途流仍能正常
// 收完，等其结束后再轮转，避免掐断在途响应。
func isRotatableOverAge(wc *WsConnection) bool {
	if wc == nil || !wc.IsOverAge() {
		return false
	}
	return wc.session == nil || wc.session.PendingCount() == 0
}

// probeConnection 发送 Ping 检测连接是否真正存活
func probeConnection(wc *WsConnection) bool {
	return probeConnectionWithTimeout(wc, defaultProbeTimeout)
}

// probe 调用探活函数���支持测试替换）
func (m *Manager) probe(wc *WsConnection) bool {
	m.mu.RLock()
	fn := m.probeFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(wc)
	}
	return probeConnection(wc)
}

// createConnection 创建新 WebSocket 连接
func (m *Manager) createConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, error) {
	// 浅拷贝共享 dialer，继承全部调优字段（NetDialContext/KeepAlive、读写缓冲、压缩等），
	// 仅按需覆盖 Proxy；避免逐字段重建时漏抄字段（曾导致 NetDialContext/KeepAlive 失效）。
	dialerCopy := *m.dialer
	dialer := &dialerCopy

	// 配置代理（Resin 反代模式下跳过，URL 已包含 Resin 地址）
	proxyURL := effectiveProxyURL(account, proxyOverride)

	if !proxy.IsResinEnabled() && proxyURL != "" {
		proxyURLParsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL failed: %w", err)
		}
		dialer.Proxy = func(req *http.Request) (*url.URL, error) {
			return proxyURLParsed, nil
		}
	}

	// 创建会话（先关闭旧 session 避免泄漏）
	poolKey := m.poolKey(account.ID(), wsURL, sessionKey, proxyURL)
	if oldSessionVal, ok := m.sessions.Load(poolKey); ok {
		oldSession := oldSessionVal.(*Session)
		oldSession.Close()
	}
	session := NewSession(account.ID(), m)
	if trimmed := strings.TrimSpace(sessionKey); trimmed != "" {
		session.ID = trimmed
	}
	m.sessions.Store(poolKey, session)

	// 拨号连接
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		m.sessions.Delete(poolKey)
		session.Close()
		return nil, fmt.Errorf("websocket handshake failed: %w", err)
	}

	// 创建连接包装
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = poolKey
	wc.httpResp = resp
	wc.onDisconnected = m.getOnDisconnected()
	wc.onReadFailure = m.DiscardConnection
	session.SetConnected(true)

	// 控制帧处理器必须在唯一永久 reader 启动前安装。
	wc.installControlHandlers()
	wc.StartReadPump()

	return wc, nil
}

// ReleaseConnection 释放连接（归还池）
func (m *Manager) ReleaseConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	wc.Touch()
}

// RemoveConnection 移除连接
func (m *Manager) RemoveConnection(accountID int64, wsURL string, sessionKey string, proxyURL string) {
	key := m.poolKey(accountID, wsURL, sessionKey, proxyURL)
	if v, ok := m.connections.LoadAndDelete(key); ok {
		wc := v.(*WsConnection)
		wc.Close()
	}
	m.sessions.Delete(key)
}

// DiscardConnection 关闭并从连接池移除一条坏连接。
// 用于上游 WS 异常路径(read error / close 1006/1009/1011 / broken pipe / unexpected EOF)：
// 关闭底层 socket 解决 CLOSE_WAIT 滞留，并把连接从 connections/sessions 移除，
// 避免坏连接被 ReleaseConnection 归还后又被 canReuseConnection 误判为可复用。
// 使用 CompareAndDelete 按本连接精确删除，防止误删同 PoolKey 下已重建的新连接。
func (m *Manager) DiscardConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	if wc.PoolKey != "" {
		m.connections.CompareAndDelete(wc.PoolKey, wc)
		if wc.session != nil {
			m.sessions.CompareAndDelete(wc.PoolKey, wc.session)
		}
	}
	if wc.session != nil {
		wc.session.StopHeartbeat()
		wc.session.SetConnected(false)
	}
	_ = wc.Close()
}

// BindResponseConn 记录 response_id 由哪条连接产出（续链亲和）。
func (m *Manager) BindResponseConn(responseID string, wc *WsConnection, sessionKey string, accountID int64, apiKey string) {
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" || wc == nil {
		return
	}
	now := time.Now()
	m.respConnMu.Lock()
	if m.respConnBindings == nil {
		m.respConnBindings = make(map[string]responseConnBinding, 64)
	}
	// 有界保护：先清一轮过期项，仍超限则拒绝新增（旧绑定比新绑定更可能被续链）。
	if len(m.respConnBindings) >= responseConnBindingMaxEntries {
		for k, b := range m.respConnBindings {
			if now.After(b.expiresAt) {
				delete(m.respConnBindings, k)
			}
		}
	}
	if len(m.respConnBindings) < responseConnBindingMaxEntries {
		m.respConnBindings[responseID] = responseConnBinding{
			conn:       wc,
			sessionKey: sessionKey,
			accountID:  accountID,
			apiKey:     apiKey,
			expiresAt:  now.Add(responseConnBindingTTL),
		}
	}
	m.respConnMu.Unlock()
}

// lookupResponseConn 返回 response_id 绑定的连接及其池内 sessionKey。
// 绑定过期、账号/API Key 不匹配、连接已断开/被重建（池内同 key 已非同一指针）
// 时返回 nil。
func (m *Manager) lookupResponseConn(responseID string, accountID int64, apiKey string) (*WsConnection, string) {
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" {
		return nil, ""
	}
	now := time.Now()
	m.respConnMu.Lock()
	binding, ok := m.respConnBindings[responseID]
	if ok && (now.After(binding.expiresAt) || binding.accountID != accountID || binding.apiKey != apiKey) {
		if now.After(binding.expiresAt) {
			delete(m.respConnBindings, responseID)
		}
		ok = false
	}
	m.respConnMu.Unlock()
	if !ok || binding.conn == nil {
		return nil, ""
	}
	// 指针级校验：连接必须仍在池中且是同一条（防止复用已重建槽位的陈旧绑定）。
	if v, exists := m.connections.Load(binding.conn.PoolKey); !exists || v != binding.conn {
		return nil, ""
	}
	if !binding.conn.IsConnected() || binding.conn.IsExpired() || binding.conn.IsOverAge() {
		return nil, ""
	}
	return binding.conn, binding.sessionKey
}

// AcquirePreferredConnection 尝试独占 response_id 绑定的原连接（续链亲和）。
// 成功返回 (连接, pendingRequest, 池内 sessionKey)；绑定失效或连接忙时返回 nil，
// 调用方回退到常规 acquire 路径。忙时不等待：续链上下文虽在原连接，但排队会
// 阻塞在前一个长响应后面，且该场景（同会话并发续链）极少，退化为缓存 miss 更稳。
func (m *Manager) AcquirePreferredConnection(responseID string, accountID int64, apiKey string) (*WsConnection, *PendingRequest, string) {
	wc, sessionKey := m.lookupResponseConn(responseID, accountID, apiKey)
	if wc == nil {
		return nil, nil, ""
	}
	lock := m.keyLock(wc.PoolKey)
	lock.Lock()
	defer lock.Unlock()
	// 加锁后复验：期间可能被其他请求占用或销毁。
	if v, exists := m.connections.Load(wc.PoolKey); !exists || v != wc {
		return nil, nil, ""
	}
	if !canReuseConnection(wc) {
		if wc.session == nil || wc.session.PendingCount() == 0 {
			m.DiscardConnection(wc)
		}
		return nil, nil, ""
	}
	if !m.probe(wc) {
		m.DiscardConnection(wc)
		return nil, nil, ""
	}
	pr, err := m.addPendingAndBeginReadLease(wc, sessionKey)
	if err != nil {
		m.DiscardConnection(wc)
		return nil, nil, ""
	}
	wc.Touch()
	return wc, pr, sessionKey
}

// poolKey 生成连接池键
func (m *Manager) poolKey(accountID int64, wsURL string, sessionKey string, proxyURL string) string {
	return fmt.Sprintf("%d|%s|%s|%s", accountID, wsURL, strings.TrimSpace(sessionKey), strings.TrimSpace(proxyURL))
}

// GetSession 获取会话
func (m *Manager) GetSession(accountID int64, wsURL string, sessionKey string, proxyURL string) (*Session, bool) {
	if v, ok := m.sessions.Load(m.poolKey(accountID, wsURL, sessionKey, proxyURL)); ok {
		return v.(*Session), true
	}
	return nil, false
}

// ConnectionCount 获取连接数量
func (m *Manager) ConnectionCount() int {
	count := 0
	m.connections.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// SessionCount 获取会话数量
func (m *Manager) SessionCount() int {
	count := 0
	m.sessions.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// ReplaceConnection 替换连接（用于重连）
func (m *Manager) ReplaceConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	// 先移除旧连接
	m.RemoveConnection(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))

	// 创建新连接
	return m.AcquireConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
}

// SendHeartbeat 发送心跳 Ping
func (m *Manager) SendHeartbeat(wc *WsConnection) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() {
		return fmt.Errorf("connection is not connected")
	}

	deadline := time.Now().Add(10 * time.Second)
	err := wc.conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
	if err != nil {
		log.Printf("WebSocket Ping 失败 (account %d): %v", wc.session.AccountID, err)
		m.DiscardConnection(wc)
		return err
	}
	return nil
}

// StartHeartbeat 启动连接心跳
func (m *Manager) StartHeartbeat(wc *WsConnection) {
	if wc == nil || wc.session == nil || !wc.IsConnected() || !wc.session.IsConnected() {
		return
	}
	wc.session.StartHeartbeat(func() error {
		return m.SendHeartbeat(wc)
	})
}

// 全局管理器实例
var globalManager *Manager
var managerOnce sync.Once

// GetManager 获取全局管理器实例
func GetManager() *Manager {
	managerOnce.Do(func() {
		globalManager = NewManager()
	})
	return globalManager
}

// ShutdownManager 关闭全局管理器
func ShutdownManager() {
	if globalManager != nil {
		globalManager.Stop()
	}
}
