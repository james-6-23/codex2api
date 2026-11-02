package auth

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

// utlsAuthRoundTripper 使用 Chrome TLS 指纹的 http.RoundTripper，
// 专门用于 chatgpt.com 等受 Cloudflare 保护的 auth 端点。
type utlsAuthRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      xproxy.Dialer
}

const (
	utlsAuthReadIdleTimeout = 15 * time.Second
	utlsAuthPingTimeout     = 15 * time.Second
	utlsAuthIdleConnTimeout = 90 * time.Second
)

func newUTLSAuthTransport(proxyURL string) http.RoundTripper {
	var dialer xproxy.Dialer = xproxy.Direct
	if proxyURL != "" {
		d, err := buildAuthProxyDialer(proxyURL)
		if err != nil {
			dialer = xproxy.Direct
		} else {
			dialer = d
		}
	}
	return &utlsAuthRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

func (t *utlsAuthRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	if cond, ok := t.pending[host]; ok {
		for {
			cond.Wait()
			if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
				t.mu.Unlock()
				return h2Conn, nil
			}
			if _, still := t.pending[host]; !still {
				break
			}
		}
	}

	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	if oldConn, ok := t.connections[host]; ok {
		go oldConn.Close()
	}

	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *utlsAuthRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP 连接失败: %w", err)
	}

	tlsConfig := &utls.Config{ServerName: host}
	tlsConn := utls.UClient(conn, tlsConfig, utls.HelloChrome_Auto)

	handshakeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}

	// 认证 transport 自管 HTTP/2 连接，不经过 net/http 的连接池。三项超时
	// 必须直接配置在 http2.Transport 上：否则健康空闲连接不会安装回收计时器，
	// readLoop、socket 与缓冲区会一直驻留。
	tr := &http2.Transport{
		ReadIdleTimeout: utlsAuthReadIdleTimeout,
		PingTimeout:     utlsAuthPingTimeout,
		IdleConnTimeout: utlsAuthIdleConnTimeout,
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("HTTP/2 连接创建失败: %w", err)
	}

	return h2Conn, nil
}

func (t *utlsAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	hostname := req.URL.Hostname()

	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		h2Conn.Close()
		return nil, err
	}

	return resp, nil
}

func (t *utlsAuthRoundTripper) CloseIdleConnections() {
	var idle []*http2.ClientConn
	t.mu.Lock()
	for host, conn := range t.connections {
		state := conn.State()
		if state.StreamsActive == 0 && state.StreamsReserved == 0 && state.StreamsPending == 0 {
			delete(t.connections, host)
			idle = append(idle, conn)
		}
	}
	t.mu.Unlock()

	for _, conn := range idle {
		_ = conn.Close()
	}
}

// ---- proxy dialer helpers (auth 包内独立实现，避免循环依赖) ----

func buildAuthProxyDialer(proxyURL string) (xproxy.Dialer, error) {
	u, err := security.ParseProxyURL(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("解析代理 URL 失败: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return buildAuthHTTPProxyDialer(u)
	case "socks5", "socks5h":
		return buildAuthSOCKS5Dialer(u)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
}

type authHTTPConnectDialer struct {
	proxyAddr       string
	proxyServerName string
	useTLS          bool
	authHeader      string
}

func (d *authHTTPConnectDialer) Dial(network, addr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", d.proxyAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接代理服务器失败: %w", err)
	}

	// https:// 代理先与代理完成 TLS，避免 Basic 凭据明文传输。
	if d.useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: d.proxyServerName,
			MinVersion: tls.VersionTLS12,
		})
		_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理 TLS 握手失败: %w", err)
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.authHeader != "" {
		connectReq += fmt.Sprintf("Proxy-Authorization: %s\r\n", d.authHeader)
	}
	connectReq += "\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 请求失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取代理响应失败: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败 (status %d)", resp.StatusCode)
	}

	if br.Buffered() > 0 {
		return &authBufferedConn{Conn: conn, reader: br}, nil
	}
	return conn, nil
}

type authBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *authBufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

func buildAuthHTTPProxyDialer(u *url.URL) (xproxy.Dialer, error) {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}

	d := &authHTTPConnectDialer{
		proxyAddr:       addr,
		proxyServerName: u.Hostname(),
		useTLS:          strings.EqualFold(u.Scheme, "https"),
	}

	if u.User != nil {
		username := u.User.Username()
		password, _ := u.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		d.authHeader = "Basic " + credentials
	}

	return d, nil
}

func buildAuthSOCKS5Dialer(u *url.URL) (xproxy.Dialer, error) {
	var auth *xproxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &xproxy.Auth{User: u.User.Username(), Password: password}
	}
	return xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
}

// ---- uTLS client pool (带 TTL 清理) ----

var utlsAuthClientPool sync.Map

type utlsAuthPoolEntry struct {
	client   *http.Client
	lastUsed atomic.Int64
}

func (e *utlsAuthPoolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

const (
	utlsAuthClientPoolTTL             = 5 * time.Minute
	utlsAuthClientPoolCleanupInterval = 60 * time.Second
)

var utlsAuthClientPoolStop = make(chan struct{})

func init() {
	go func() {
		ticker := time.NewTicker(utlsAuthClientPoolCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				evictExpiredUTLSAuthClients()
			case <-utlsAuthClientPoolStop:
				return
			}
		}
	}()
}

func evictExpiredUTLSAuthClients() {
	cutoff := time.Now().Add(-utlsAuthClientPoolTTL).UnixNano()
	utlsAuthClientPool.Range(func(key, value any) bool {
		entry := value.(*utlsAuthPoolEntry)
		if entry.lastUsed.Load() < cutoff && utlsAuthClientPool.CompareAndDelete(key, value) {
			if ut, ok := entry.client.Transport.(*utlsAuthRoundTripper); ok {
				// 只关闭无在途 stream 的连接，避免清理与刚开始的认证请求
				// 竞态时误杀请求；漏过的连接由 IdleConnTimeout 最终回收。
				ut.CloseIdleConnections()
			}
		}
		return true
	})
}

// buildUTLSHTTPClient 构建使用 Chrome TLS 指纹的 HTTP 客户端（连接池复用）。
// 用于请求受 Cloudflare 保护的 chatgpt.com 端点。
func buildUTLSHTTPClient(proxyURL string) *http.Client {
	if v, ok := utlsAuthClientPool.Load(proxyURL); ok {
		entry := v.(*utlsAuthPoolEntry)
		entry.touch()
		return entry.client
	}

	transport := newUTLSAuthTransport(proxyURL)
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	entry := &utlsAuthPoolEntry{client: client}
	entry.touch()

	if v, loaded := utlsAuthClientPool.LoadOrStore(proxyURL, entry); loaded {
		e := v.(*utlsAuthPoolEntry)
		e.touch()
		return e.client
	}
	return client
}

// BuildUTLSHTTPClient builds a Chrome TLS fingerprint HTTP client (exported for admin OAuth flow).
func BuildUTLSHTTPClient(proxyURL string) *http.Client {
	return buildUTLSHTTPClient(proxyURL)
}
