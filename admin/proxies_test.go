package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newAdminProxyTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newAdminProxyTestStore(t *testing.T, db *database.DB) *auth.Store {
	t.Helper()
	store := auth.NewStore(db, nil, nil)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool returned error: %v", err)
	}
	t.Cleanup(store.Stop)
	return store
}

func startSOCKS5Listener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func acceptSOCKS5Forever(listener net.Listener, handle func(net.Conn) error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = handle(c)
		}(conn)
	}
}

func completeSOCKS5Greeting(conn net.Conn) error {
	greetingHeader := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetingHeader); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greetingHeader[1])); err != nil {
		return err
	}
	_, err := conn.Write([]byte{0x05, 0x00})
	return err
}

func readSOCKS5RequestAddress(conn net.Conn) (string, error) {
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, requestHeader); err != nil {
		return "", err
	}
	var host string
	switch requestHeader[3] {
	case 0x01:
		ip := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", err
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", err
		}
		host = string(name)
	case 0x04:
		ip := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	default:
		return "", fmt.Errorf("unexpected SOCKS address type %d", requestHeader[3])
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", int(port[0])<<8|int(port[1]))), nil
}

func replySOCKS5HostUnreachable(conn net.Conn) error {
	if err := completeSOCKS5Greeting(conn); err != nil {
		return err
	}
	if _, err := readSOCKS5RequestAddress(conn); err != nil {
		return err
	}
	_, err := conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

func replySOCKS5ConnectOK(conn net.Conn) error {
	_, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

func tunnelSOCKS5To(conn net.Conn, address string) error {
	target, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer target.Close()
	if err := replySOCKS5ConnectOK(conn); err != nil {
		return err
	}
	errCh := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(target, conn)
		errCh <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(conn, target)
		errCh <- copyErr
	}()
	return <-errCh
}

func TestPersistProxyTestResultRefreshesRuntimePool(t *testing.T) {
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://proxy.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{db: db, store: store}

	if err := handler.persistProxyTestResult(ctx, id, "http://proxy.example:8080", database.ProxyTestStatusError, "", "", 0); err != nil {
		t.Fatalf("persistProxyTestResult(error) returned error: %v", err)
	}
	if got := store.NextProxy(); got != "" {
		t.Fatalf("NextProxy after error = %q, want empty", got)
	}

	if err := handler.persistProxyTestResult(ctx, id, "http://proxy.example:8080", database.ProxyTestStatusSuccess, "1.2.3.4", "US", 100); err != nil {
		t.Fatalf("persistProxyTestResult(success) returned error: %v", err)
	}
	if got := store.NextProxy(); got != "http://proxy.example:8080" {
		t.Fatalf("NextProxy after successful retest = %q, want proxy URL", got)
	}
}

func TestPersistProxyTestResultFailsClosedBeforeReloadFailure(t *testing.T) {
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const proxyURL = "http://proxy.example:8080"
	id, err := db.InsertProxy(ctx, proxyURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{
		db:    db,
		store: store,
		reloadProxyPoolFn: func() error {
			return errors.New("reload unavailable")
		},
	}

	err = handler.persistProxyTestResult(
		ctx,
		id,
		proxyURL,
		database.ProxyTestStatusError,
		"",
		"",
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "reload unavailable") {
		t.Fatalf("persistProxyTestResult() error = %v, want reload failure", err)
	}
	if got := store.NextProxy(); got != "" {
		t.Fatalf("NextProxy after reload failure = %q, want failed proxy removed", got)
	}
}

func TestTestProxyRejectsMismatchedURLWithoutChangingStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://proxy.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	if err := db.UpdateProxyTestResult(ctx, id, "http://proxy.example:8080", database.ProxyTestStatusSuccess, "1.2.3.4", "US", 100); err != nil {
		t.Fatalf("seed successful test result: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test",
		strings.NewReader(fmt.Sprintf(`{"id":%d,"url":"://bad"}`, id)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.TestProxy(ginCtx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error == "" {
		t.Fatalf("response = %#v, want conflict error", payload)
	}

	rows, err := db.ListProxies(ctx)
	if err != nil {
		t.Fatalf("ListProxies returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListProxies returned %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.TestStatus != database.ProxyTestStatusSuccess || row.TestIP != "1.2.3.4" || row.TestLocation != "US" || row.TestLatencyMs != 100 {
		t.Fatalf("persisted proxy result = %#v, want original successful state", row)
	}
	if got := store.NextProxy(); got != "http://proxy.example:8080" {
		t.Fatalf("NextProxy after rejected test = %q, want original proxy URL", got)
	}
}

func TestTestProxyIgnoresStaleResultAfterURLChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","query":"1.2.3.4","country":"US","regionName":"CA","city":"SF","isp":"test"}`))
	}))
	defer proxyServer.Close()

	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, proxyServer.URL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test",
		strings.NewReader(fmt.Sprintf(`{"id":%d,"url":%q}`, id, proxyServer.URL)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		handler.TestProxy(ginCtx)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy test did not reach probe server")
	}

	updatedURL := "http://updated.example:8080"
	if err := db.UpdateProxy(ctx, id, &updatedURL, nil, nil); err != nil {
		t.Fatalf("UpdateProxy returned error: %v", err)
	}
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy test did not complete")
	}

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	row := findAdminProxyRow(t, db, id)
	if row.URL != updatedURL || row.TestStatus != database.ProxyTestStatusUntested {
		t.Fatalf("proxy after stale result = %#v, want updated URL with untested status", row)
	}
}

func TestTestProxyUsesStoredURLForCompareAndSwap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const (
		storedURL = "  http://proxy.example:8080  "
		dialURL   = "http://proxy.example:8080"
	)
	id, err := db.InsertProxy(ctx, storedURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}

	handler := &Handler{
		db: db,
		proxyProbe: func(_ context.Context, proxyURL, _ string) proxyProbeResult {
			if proxyURL != dialURL {
				t.Fatalf("probe URL = %q, want trimmed %q", proxyURL, dialURL)
			}
			return proxyProbeResult{
				Success:    true,
				Conclusive: true,
				IP:         "1.2.3.4",
				Location:   "US",
				LatencyMs:  100,
			}
		},
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test",
		strings.NewReader(fmt.Sprintf(`{"id":%d,"url":%q}`, id, dialURL)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.TestProxy(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row := findAdminProxyRow(t, db, id)
	if row.URL != storedURL || row.TestStatus != database.ProxyTestStatusSuccess || row.TestIP != "1.2.3.4" {
		t.Fatalf("persisted proxy = %#v, want raw URL preserved with successful result", row)
	}
}

func TestTestProxyProbeServiceFailuresAreInconclusive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "rate limited", statusCode: http.StatusTooManyRequests, body: `{"status":"fail","message":"rate limited"}`},
		{name: "server error", statusCode: http.StatusServiceUnavailable, body: `{"status":"fail","message":"unavailable"}`},
		{name: "malformed response", statusCode: http.StatusOK, body: `not-json`},
		{name: "missing exit IP", statusCode: http.StatusOK, body: `{"status":"success","country":"US"}`},
		{name: "invalid exit IP", statusCode: http.StatusOK, body: `{"status":"success","query":"not-an-ip","country":"US"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer proxyServer.Close()

			db := newAdminProxyTestDB(t)
			ctx := context.Background()
			id, err := db.InsertProxy(ctx, proxyServer.URL, "")
			if err != nil {
				t.Fatalf("InsertProxy returned error: %v", err)
			}
			if err := db.UpdateProxyTestResult(ctx, id, proxyServer.URL, database.ProxyTestStatusSuccess, "1.2.3.4", "US", 100); err != nil {
				t.Fatalf("seed successful test result: %v", err)
			}
			store := newAdminProxyTestStore(t, db)
			handler := &Handler{db: db, store: store}

			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(
				http.MethodPost,
				"/api/admin/proxies/test",
				strings.NewReader(fmt.Sprintf(`{"id":%d,"url":%q}`, id, proxyServer.URL)),
			)
			ginCtx.Request.Header.Set("Content-Type", "application/json")

			handler.TestProxy(ginCtx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			var payload struct {
				Success    bool  `json:"success"`
				Conclusive *bool `json:"conclusive"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload.Success || payload.Conclusive == nil || *payload.Conclusive {
				t.Fatalf("response = %#v, want inconclusive failure", payload)
			}

			row := findAdminProxyRow(t, db, id)
			if row.TestStatus != database.ProxyTestStatusSuccess || row.TestIP != "1.2.3.4" {
				t.Fatalf("proxy after inconclusive probe = %#v, want original successful state", row)
			}
			if got := store.NextProxy(); got != proxyServer.URL {
				t.Fatalf("NextProxy after inconclusive probe = %q, want %q", got, proxyServer.URL)
			}
		})
	}
}

func TestProbeProxyAuthenticationFailureIsConclusive(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxyServer.Close()

	result := probeProxy(context.Background(), proxyServer.URL, "en")

	if result.Success || !result.Conclusive || !strings.Contains(result.Error, "407") {
		t.Fatalf("probe result = %#v, want conclusive HTTP 407 failure", result)
	}
}

func TestProbeProxySOCKSTargetUnreachableIsInconclusive(t *testing.T) {
	listener := startSOCKS5Listener(t)
	go acceptSOCKS5Forever(listener, replySOCKS5HostUnreachable)

	result := probeProxy(context.Background(), "socks5://"+listener.Addr().String(), "en")
	if result.Success || result.Conclusive || !strings.Contains(strings.ToLower(result.Error), "host unreachable") {
		t.Fatalf("probe result = %#v, want inconclusive SOCKS target failure", result)
	}
	if !strings.Contains(result.Error, "IPv4/IPv6 检测目标都不可达") {
		t.Fatalf("probe result = %#v, want both-family fallback note", result)
	}
}

func TestProbeProxyIPv4SuccessDoesNotFallback(t *testing.T) {
	var otherHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		target := req.URL.String()
		if !strings.Contains(target, "ip-api.com") {
			otherHits.Add(1)
			http.Error(w, "unexpected fallback", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","query":"1.2.3.4","country":"US","regionName":"CA","city":"LA","isp":"Example"}`))
	}))
	t.Cleanup(proxyServer.Close)

	result := probeProxy(context.Background(), proxyServer.URL, "en")
	if !result.Success || result.IP != "1.2.3.4" || result.Location != "US·CA·LA" {
		t.Fatalf("probe result = %#v, want IPv4 success", result)
	}
	if otherHits.Load() != 0 {
		t.Fatalf("fallback hits = %d, want 0", otherHits.Load())
	}
}

func TestProbeProxyFallsBackToIPv6Echo(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"2605:1234:5678::1"}`))
	}))
	t.Cleanup(echo.Close)
	echoURL := echo.URL

	oldEcho := proxyProbeIPv6EchoURLsFn
	oldGeo := proxyProbeLookupGeoFn
	proxyProbeIPv6EchoURLsFn = func() []string { return []string{echoURL} }
	proxyProbeLookupGeoFn = func(_ context.Context, ip, lang string) (string, string, string, string) {
		if ip != "2605:1234:5678::1" || lang != "zh-CN" {
			t.Fatalf("geo lookup ip=%q lang=%q", ip, lang)
		}
		return "美国", "新泽西", "纽瓦克", "Example ISP"
	}
	t.Cleanup(func() {
		proxyProbeIPv6EchoURLsFn = oldEcho
		proxyProbeLookupGeoFn = oldGeo
	})

	listener := startSOCKS5Listener(t)
	go acceptSOCKS5Forever(listener, func(conn net.Conn) error {
		if err := completeSOCKS5Greeting(conn); err != nil {
			return err
		}
		address, err := readSOCKS5RequestAddress(conn)
		if err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(address)
		if strings.Contains(host, "ip-api.com") {
			_, err := conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return err
		}
		return tunnelSOCKS5To(conn, address)
	})

	result := probeProxy(context.Background(), "socks5://"+listener.Addr().String(), "zh-CN")
	if !result.Success || !result.Conclusive || result.IP != "2605:1234:5678::1" {
		t.Fatalf("probe result = %#v, want IPv6 fallback success", result)
	}
	if result.Location != "美国·新泽西·纽瓦克" || result.ISP != "Example ISP" {
		t.Fatalf("probe result = %#v, want gateway geo fields", result)
	}
}

func TestShouldFallbackToIPv6Probe(t *testing.T) {
	if shouldFallbackToIPv6Probe(proxyProbeResult{Success: true, Conclusive: true}) {
		t.Fatal("successful probe must not fallback")
	}
	if shouldFallbackToIPv6Probe(proxyProbeResult{Conclusive: true, Error: "代理认证失败 (HTTP 407)"}) {
		t.Fatal("conclusive proxy failure must not fallback")
	}
	if shouldFallbackToIPv6Probe(proxyProbeResult{Error: "代理检测服务暂时不可用 (HTTP 429)"}) {
		t.Fatal("probe service failure must not fallback")
	}
	if !shouldFallbackToIPv6Probe(proxyProbeResult{
		Error: "连接失败: socks connect tcp proxy->ip-api.com:80: unknown error host unreachable",
	}) {
		t.Fatal("SOCKS IPv4 target unreachable must fallback")
	}
}

func TestParseProxyProbeExitIP(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "ipify json", body: `{"ip":"2605:1234::1"}`, want: "2605:1234::1"},
		{name: "plain ip", body: "2001:db8::1\n", want: "2001:db8::1"},
		{name: "cloudflare trace", body: "fl=123\nip=2001:db8:1::2\nloc=US\n", want: "2001:db8:1::2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxyProbeExitIP([]byte(tt.body))
			if err != nil || got != tt.want {
				t.Fatalf("parseProxyProbeExitIP(%q) = %q, %v; want %q", tt.body, got, err, tt.want)
			}
		})
	}
}

func TestParseProxyProbeGeoFields(t *testing.T) {
	country, region, city, isp := parseIPAPIGeoFields(gjson.Parse(`{"country":"美国","regionName":"加州","city":"洛杉矶","isp":"Example"}`))
	if country != "美国" || region != "加州" || city != "洛杉矶" || isp != "Example" {
		t.Fatalf("ip-api geo = %q %q %q %q", country, region, city, isp)
	}
	country, region, city, isp = parseIPWhoisGeoFields(gjson.Parse(`{"country":"United States","region":"New Jersey","city":"Newark","connection":{"isp":"Example ISP"}}`))
	if country != "United States" || region != "New Jersey" || city != "Newark" || isp != "Example ISP" {
		t.Fatalf("ipwhois geo = %q %q %q %q", country, region, city, isp)
	}
}

func TestProbeProxySOCKSTargetTimeoutIsInconclusive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		greetingHeader := make([]byte, 2)
		if _, err := io.ReadFull(conn, greetingHeader); err != nil {
			serverErr <- err
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(greetingHeader[1])); err != nil {
			serverErr <- err
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			serverErr <- err
			return
		}

		requestHeader := make([]byte, 4)
		if _, err := io.ReadFull(conn, requestHeader); err != nil {
			serverErr <- err
			return
		}
		var addressBytes int64
		switch requestHeader[3] {
		case 0x01:
			addressBytes = net.IPv4len
		case 0x03:
			length := make([]byte, 1)
			if _, err := io.ReadFull(conn, length); err != nil {
				serverErr <- err
				return
			}
			addressBytes = int64(length[0])
		case 0x04:
			addressBytes = net.IPv6len
		default:
			serverErr <- fmt.Errorf("unexpected SOCKS address type %d", requestHeader[3])
			return
		}
		if _, err := io.CopyN(io.Discard, conn, addressBytes+2); err != nil {
			serverErr <- err
			return
		}

		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			serverErr <- err
			return
		}
		var oneByte [1]byte
		if _, err := conn.Read(oneByte[:]); err == nil {
			serverErr <- fmt.Errorf("expected probe client to close the stalled SOCKS connection")
			return
		} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			serverErr <- fmt.Errorf("probe client did not close the stalled SOCKS connection")
			return
		}
		serverErr <- nil
	}()

	result := probeProxyWithTimeout(
		context.Background(),
		"socks5://"+listener.Addr().String(),
		"en",
		50*time.Millisecond,
	)
	if result.Success || result.Conclusive || result.Error == "" {
		t.Fatalf("probe result = %#v, want inconclusive SOCKS target timeout", result)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("SOCKS test server: %v", err)
	}
}

func TestProbeProxySOCKSHandshakeTimeoutIsConclusive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		greetingHeader := make([]byte, 2)
		if _, err := io.ReadFull(conn, greetingHeader); err != nil {
			serverErr <- err
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(greetingHeader[1])); err != nil {
			serverErr <- err
			return
		}

		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			serverErr <- err
			return
		}
		var oneByte [1]byte
		if _, err := conn.Read(oneByte[:]); err == nil {
			serverErr <- fmt.Errorf("expected probe client to close the stalled SOCKS handshake")
			return
		} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			serverErr <- fmt.Errorf("probe client did not close the stalled SOCKS handshake")
			return
		}
		serverErr <- nil
	}()

	result := probeProxyWithTimeout(
		context.Background(),
		"socks5://"+listener.Addr().String(),
		"en",
		50*time.Millisecond,
	)
	if result.Success || !result.Conclusive || result.Error == "" {
		t.Fatalf("probe result = %#v, want conclusive SOCKS handshake timeout", result)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("SOCKS test server: %v", err)
	}
}

type proxyProbeTimeoutError struct{}

func (proxyProbeTimeoutError) Error() string   { return "timeout" }
func (proxyProbeTimeoutError) Timeout() bool   { return true }
func (proxyProbeTimeoutError) Temporary() bool { return true }

func TestProxyProbeErrorClassificationUsesCallerContext(t *testing.T) {
	if !proxyProbeErrorIsConclusive(context.Background(), proxyProbeTimeoutError{}, proxyProbeConnectionState{}, "http") {
		t.Fatal("proxy dial timeout with an active caller context must be conclusive")
	}
	if proxyProbeErrorIsConclusive(
		context.Background(),
		proxyProbeTimeoutError{},
		proxyProbeConnectionState{
			ConnectedToProxyEndpoint: true,
			GotTransportConnection:   true,
		},
		"http",
	) {
		t.Fatal("timeout after connecting through the proxy must stay inconclusive")
	}
	if proxyProbeErrorIsConclusive(
		context.Background(),
		fmt.Errorf("connection reset"),
		proxyProbeConnectionState{
			ConnectedToProxyEndpoint: true,
			GotTransportConnection:   true,
		},
		"http",
	) {
		t.Fatal("transport error after connecting through the proxy must stay inconclusive")
	}
	if !proxyProbeErrorIsConclusive(context.Background(), fmt.Errorf("connection refused"), proxyProbeConnectionState{}, "http") {
		t.Fatal("transport error before connecting through the proxy must be conclusive")
	}
	if !proxyProbeErrorIsConclusive(
		context.Background(),
		proxyProbeTimeoutError{},
		proxyProbeConnectionState{ConnectedToProxyEndpoint: true},
		"socks5",
	) {
		t.Fatal("SOCKS handshake timeout before starting target connect must be conclusive")
	}
	if proxyProbeErrorIsConclusive(
		context.Background(),
		proxyProbeTimeoutError{},
		proxyProbeConnectionState{
			ConnectedToProxyEndpoint:  true,
			StartedSOCKSTargetConnect: true,
		},
		"socks5",
	) {
		t.Fatal("SOCKS target timeout after connecting to the proxy must stay inconclusive")
	}
	if proxyProbeErrorIsConclusive(
		context.Background(),
		fmt.Errorf("socks connect tcp proxy->target: unknown error host unreachable"),
		proxyProbeConnectionState{
			ConnectedToProxyEndpoint:  true,
			StartedSOCKSTargetConnect: true,
		},
		"socks5",
	) {
		t.Fatal("SOCKS target unreachable reply must stay inconclusive")
	}
	if !proxyProbeErrorIsConclusive(
		context.Background(),
		fmt.Errorf("socks connect tcp proxy->target: dial tcp proxy: connect: connection refused"),
		proxyProbeConnectionState{},
		"socks5",
	) {
		t.Fatal("failure to connect to the SOCKS proxy itself must be conclusive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if proxyProbeErrorIsConclusive(ctx, proxyProbeTimeoutError{}, proxyProbeConnectionState{}, "http") {
		t.Fatal("timeout after caller cancellation must be inconclusive")
	}
}

func TestProbeProxyTimeoutIsInconclusive(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := probeProxy(ctx, proxyServer.URL, "en")

	if result.Success || result.Conclusive || result.Error == "" {
		t.Fatalf("probe result = %#v, want inconclusive timeout", result)
	}
}

func TestTestAllProxiesPersistsResultsAndReloadsPoolOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const (
		successURL      = "http://success.example:8080"
		errorURL        = "http://error.example:8080"
		inconclusiveURL = "http://inconclusive.example:8080"
	)
	successID, err := db.InsertProxy(ctx, successURL, "")
	if err != nil {
		t.Fatalf("InsertProxy(success) returned error: %v", err)
	}
	errorID, err := db.InsertProxy(ctx, errorURL, "")
	if err != nil {
		t.Fatalf("InsertProxy(error) returned error: %v", err)
	}
	inconclusiveID, err := db.InsertProxy(ctx, inconclusiveURL, "")
	if err != nil {
		t.Fatalf("InsertProxy(inconclusive) returned error: %v", err)
	}
	if err := db.UpdateProxyTestResult(ctx, inconclusiveID, inconclusiveURL, database.ProxyTestStatusSuccess, "9.8.7.6", "old", 321); err != nil {
		t.Fatalf("seed inconclusive proxy state: %v", err)
	}

	var reloads int32
	handler := &Handler{
		db: db,
		proxyProbe: func(_ context.Context, proxyURL, _ string) proxyProbeResult {
			switch proxyURL {
			case successURL:
				return proxyProbeResult{
					Success:    true,
					Conclusive: true,
					IP:         "1.2.3.4",
					Location:   "US",
					LatencyMs:  100,
				}
			case errorURL:
				return proxyProbeResult{
					Conclusive: true,
					Error:      "connection refused",
					LatencyMs:  50,
				}
			case inconclusiveURL:
				return proxyProbeResult{
					Error: "probe service unavailable",
				}
			default:
				t.Fatalf("unexpected proxy URL %q", proxyURL)
				return proxyProbeResult{}
			}
		},
		reloadProxyPoolFn: func() error {
			atomic.AddInt32(&reloads, 1)
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d,%d],"lang":"en"}`, successID, errorID, inconclusiveID)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.TestAllProxies(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	if got := atomic.LoadInt32(&reloads); got != 1 {
		t.Fatalf("proxy pool reloads = %d, want 1", got)
	}

	var progressEvents int
	var completeSeen bool
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			ProxyID int64  `json:"proxy_id"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE event: %v", err)
		}
		switch event.Type {
		case "progress":
			progressEvents++
		case "complete":
			completeSeen = true
		}
	}
	if progressEvents != 3 || !completeSeen {
		t.Fatalf("SSE events = %q, want 3 progress events and complete", recorder.Body.String())
	}

	successRow := findAdminProxyRow(t, db, successID)
	if successRow.TestStatus != database.ProxyTestStatusSuccess || successRow.TestIP != "1.2.3.4" {
		t.Fatalf("successful proxy result = %#v", successRow)
	}
	errorRow := findAdminProxyRow(t, db, errorID)
	if errorRow.TestStatus != database.ProxyTestStatusError {
		t.Fatalf("failed proxy result = %#v, want error", errorRow)
	}
	inconclusiveRow := findAdminProxyRow(t, db, inconclusiveID)
	if inconclusiveRow.TestStatus != database.ProxyTestStatusSuccess || inconclusiveRow.TestIP != "9.8.7.6" {
		t.Fatalf("inconclusive proxy result = %#v, want original state", inconclusiveRow)
	}
}

func TestTestAllProxiesFailsClosedBeforeReloadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const proxyURL = "http://error.example:8080"
	id, err := db.InsertProxy(ctx, proxyURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{
		db:    db,
		store: store,
		proxyProbe: func(context.Context, string, string) proxyProbeResult {
			return proxyProbeResult{Conclusive: true, Error: "connection refused"}
		},
		reloadProxyPoolFn: func() error {
			return errors.New("reload unavailable")
		},
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	handler.TestAllProxies(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "代理池刷新失败") {
		t.Fatalf("SSE body = %q, want reload failure in complete event", recorder.Body.String())
	}
	if got := store.NextProxy(); got != "" {
		t.Fatalf("NextProxy after batch reload failure = %q, want failed proxy removed", got)
	}
	if row := findAdminProxyRow(t, db, id); row.TestStatus != database.ProxyTestStatusError {
		t.Fatalf("persisted proxy = %#v, want error status", row)
	}
}

func TestTestAllProxiesRequiresExplicitBoundedIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://proxy.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	handler := &Handler{
		db: db,
		proxyProbe: func(context.Context, string, string) proxyProbeResult {
			return proxyProbeResult{Error: "not reached"}
		},
	}

	tooManyIDs := make([]int64, 101)
	for i := range tooManyIDs {
		tooManyIDs[i] = int64(i + 1)
	}
	tooManyBody, err := json.Marshal(gin.H{"ids": tooManyIDs})
	if err != nil {
		t.Fatalf("marshal too many IDs: %v", err)
	}
	oversizedBody, err := json.Marshal(gin.H{
		"ids":  []int64{id},
		"lang": strings.Repeat("x", proxyBatchTestMaxBody),
	})
	if err != nil {
		t.Fatalf("marshal oversized body: %v", err)
	}
	tests := []struct {
		name string
		body string
	}{
		{name: "missing IDs", body: `{}`},
		{name: "empty IDs", body: `{"ids":[]}`},
		{name: "unknown field", body: fmt.Sprintf(`{"id":%d}`, id)},
		{name: "too many IDs", body: string(tooManyBody)},
		{name: "oversized body", body: string(oversizedBody)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(
				http.MethodPost,
				"/api/admin/proxies/test-all",
				strings.NewReader(tt.body),
			)
			ginCtx.Request.Header.Set("Content-Type", "application/json")

			handler.TestAllProxies(ginCtx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
		})
	}
}

func TestTestAllProxiesFinishesWorkBeforeBlockedSSEWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://proxy.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}

	progressBlocked := make(chan struct{})
	releaseProgress := make(chan struct{})
	reloaded := make(chan struct{}, 1)
	var (
		progressBlockedOnce atomic.Bool
		releaseOnce         sync.Once
	)
	unblockProgress := func() {
		releaseOnce.Do(func() { close(releaseProgress) })
	}
	defer unblockProgress()

	handler := &Handler{
		db: db,
		proxyProbe: func(context.Context, string, string) proxyProbeResult {
			return proxyProbeResult{Conclusive: true, Error: "bad proxy"}
		},
		reloadProxyPoolFn: func() error {
			select {
			case reloaded <- struct{}{}:
			default:
			}
			return nil
		},
		proxyBatchEventSender: func(_ *gin.Context, event proxyBatchTestEvent) bool {
			if event.Type == "progress" && progressBlockedOnce.CompareAndSwap(false, true) {
				close(progressBlocked)
				<-releaseProgress
			}
			return true
		},
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		handler.TestAllProxies(ginCtx)
		close(done)
	}()

	select {
	case <-progressBlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("batch test never reached the blocked progress write")
	}

	select {
	case <-reloaded:
	case <-time.After(500 * time.Millisecond):
		t.Error("runtime proxy pool reload waited behind the blocked SSE write")
	}

	secondRecorder := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRecorder)
	secondCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)),
	)
	secondCtx.Request.Header.Set("Content-Type", "application/json")
	handler.TestAllProxies(secondCtx)
	if secondRecorder.Code != http.StatusOK {
		t.Errorf("batch after work completion status = %d, want %d", secondRecorder.Code, http.StatusOK)
	}

	unblockProgress()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch test did not finish after unblocking SSE")
	}
}

func TestTestAllProxiesRejectsConcurrentWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertProxy(ctx, "http://proxy.example:8080", "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var (
		startOnce   sync.Once
		releaseOnce sync.Once
	)
	unblockProbe := func() {
		releaseOnce.Do(func() { close(releaseProbe) })
	}
	defer unblockProbe()

	handler := &Handler{
		db: db,
		proxyProbe: func(context.Context, string, string) proxyProbeResult {
			startOnce.Do(func() { close(probeStarted) })
			<-releaseProbe
			return proxyProbeResult{Conclusive: true, Error: "bad proxy"}
		},
	}
	firstRecorder := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRecorder)
	firstCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)),
	)
	firstCtx.Request.Header.Set("Content-Type", "application/json")
	firstDone := make(chan struct{})
	go func() {
		handler.TestAllProxies(firstCtx)
		close(firstDone)
	}()

	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first batch did not start probing")
	}

	secondRecorder := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRecorder)
	secondCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/test-all",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, id)),
	)
	secondCtx.Request.Header.Set("Content-Type", "application/json")
	handler.TestAllProxies(secondCtx)
	if secondRecorder.Code != http.StatusConflict {
		t.Errorf("concurrent batch status = %d, want %d", secondRecorder.Code, http.StatusConflict)
	}

	unblockProbe()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first batch did not finish after releasing the probe")
	}
}

func TestUpdateProxyRejectsBlankURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const originalURL = "http://proxy.example:8080"
	id, err := db.InsertProxy(ctx, originalURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ginCtx.Request = httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/admin/proxies/%d", id),
		strings.NewReader(`{"url":"   "}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateProxy(ginCtx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := findAdminProxyRow(t, db, id).URL; got != originalURL {
		t.Fatalf("stored URL = %q, want unchanged %q", got, originalURL)
	}
}

func TestProbeProxyRejectsBlankURLWithoutDirectConnection(t *testing.T) {
	result := probeProxy(context.Background(), "   ", "en")
	if result.Success || !result.Conclusive || result.Error == "" {
		t.Fatalf("probe result = %#v, want conclusive blank URL failure", result)
	}
}

func TestAddProxiesRejectsBlankAndInvalidURLs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	handler := &Handler{db: db}
	tests := []struct {
		name string
		body string
	}{
		{name: "blank", body: `{"urls":["   "]}`},
		{name: "unsupported scheme", body: `{"urls":["ftp://proxy.example:21"]}`},
		{name: "too long", body: fmt.Sprintf(`{"urls":[%q]}`, "http://"+strings.Repeat("a", 501))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(
				http.MethodPost,
				"/api/admin/proxies",
				strings.NewReader(tt.body),
			)
			ginCtx.Request.Header.Set("Content-Type", "application/json")

			handler.AddProxies(ginCtx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
		})
	}
	if rows, err := db.ListProxies(context.Background()); err != nil {
		t.Fatalf("ListProxies returned error: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("stored proxies = %#v, want none", rows)
	}
}

func findAdminProxyRow(t *testing.T, db *database.DB, id int64) *database.ProxyRow {
	t.Helper()
	rows, err := db.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies returned error: %v", err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("proxy %d not found", id)
	return nil
}

func TestCleanErrorProxiesHandlerSynchronizesRuntimeAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	errorURL := "http://error.example:8080"
	healthyURL := "http://healthy.example:8080"
	errorID, err := db.InsertProxy(ctx, errorURL, "")
	if err != nil {
		t.Fatalf("InsertProxy(error) returned error: %v", err)
	}
	if _, err := db.InsertProxy(ctx, healthyURL, ""); err != nil {
		t.Fatalf("InsertProxy(healthy) returned error: %v", err)
	}
	if err := db.UpdateProxyTestResult(ctx, errorID, errorURL, database.ProxyTestStatusError, "", "", 0); err != nil {
		t.Fatalf("mark proxy error: %v", err)
	}
	accountID, err := db.InsertAccount(ctx, "bound", "rt-bound", errorURL)
	if err != nil {
		t.Fatalf("InsertAccount returned error: %v", err)
	}

	store := newAdminProxyTestStore(t, db)
	store.AddAccount(&auth.Account{
		DBID:        accountID,
		AccessToken: "tok-bound",
		ProxyURL:    errorURL,
		Status:      auth.StatusReady,
	})
	store.BindSessionAffinity("cleaned-proxy-session", store.FindByID(accountID), errorURL)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/proxies/clean-error", nil)
	handler.CleanErrorProxies(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload struct {
		Cleaned int `json:"cleaned"`
		Unbound int `json:"unbound"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Cleaned != 1 || payload.Unbound != 1 {
		t.Fatalf("response = %#v, want cleaned=1 unbound=1", payload)
	}

	runtimeAccount := store.FindByID(accountID)
	if runtimeAccount == nil {
		t.Fatal("runtime account not found")
	}
	if got := runtimeAccount.GetProxyURL(); got != "" {
		t.Fatalf("runtime account proxy = %q, want empty", got)
	}
	selected, proxyURL := store.NextForSession("cleaned-proxy-session", 0, nil)
	if selected == nil {
		t.Fatal("expected account to remain schedulable after proxy cleanup")
	}
	defer store.Release(selected)
	if proxyURL == errorURL {
		t.Fatalf("session affinity reused cleaned proxy %q", proxyURL)
	}
	if got := store.ResolveProxyForAccount(selected); got != healthyURL {
		t.Fatalf("unbound account proxy = %q, want remaining pool member %q", got, healthyURL)
	}
}

func TestDeleteProxyUnbindsRuntimeAccountsImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const retiredURL = "http://retired.example:8080"
	id, err := db.InsertProxy(ctx, retiredURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	accountID, err := db.InsertAccount(ctx, "bound", "rt-bound", retiredURL)
	if err != nil {
		t.Fatalf("InsertAccount returned error: %v", err)
	}

	store := newAdminProxyTestStore(t, db)
	store.AddAccount(&auth.Account{
		DBID:        accountID,
		AccessToken: "tok-bound",
		ProxyURL:    retiredURL,
		Status:      auth.StatusReady,
	})
	store.BindSessionAffinity("retired-proxy-session", store.FindByID(accountID), retiredURL)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/admin/proxies/%d", id), nil)
	handler.DeleteProxy(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	runtimeAccount := store.FindByID(accountID)
	if runtimeAccount == nil {
		t.Fatal("runtime account not found")
	}
	if got := runtimeAccount.GetProxyURL(); got != "" {
		t.Fatalf("runtime account proxy = %q, want empty", got)
	}
	if got := store.NextProxy(); got == retiredURL {
		t.Fatalf("NextProxy still returned retired URL %q", got)
	}
	selected, proxyURL := store.NextForSession("retired-proxy-session", 0, nil)
	if selected != nil {
		defer store.Release(selected)
	}
	if proxyURL == retiredURL {
		t.Fatalf("session affinity reused deleted proxy %q", proxyURL)
	}
}

func TestUpdateProxyDisableKeepsPinButStopsUsingIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const (
		disabledURL = "http://disabled.example:8080"
		enabledURL  = "http://enabled.example:8080"
	)
	disabledID, err := db.InsertProxy(ctx, disabledURL, "")
	if err != nil {
		t.Fatalf("InsertProxy(disabled) returned error: %v", err)
	}
	if _, err := db.InsertProxy(ctx, enabledURL, ""); err != nil {
		t.Fatalf("InsertProxy(enabled) returned error: %v", err)
	}
	pinnedID, err := db.InsertAccount(ctx, "pinned", "rt-pinned", disabledURL)
	if err != nil {
		t.Fatalf("InsertAccount(pinned) returned error: %v", err)
	}
	unboundID, err := db.InsertAccount(ctx, "unbound", "rt-unbound", "")
	if err != nil {
		t.Fatalf("InsertAccount(unbound) returned error: %v", err)
	}

	store := newAdminProxyTestStore(t, db)
	pinned := &auth.Account{DBID: pinnedID, AccessToken: "tok-pinned", ProxyURL: disabledURL, Status: auth.StatusReady}
	unbound := &auth.Account{DBID: unboundID, AccessToken: "tok-unbound", Status: auth.StatusReady}
	store.AddAccount(pinned)
	store.AddAccount(unbound)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", disabledID)}}
	ginCtx.Request = httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/admin/proxies/%d", disabledID),
		strings.NewReader(`{"enabled":false}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateProxy(ginCtx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	if got := store.FindByID(pinnedID).GetProxyURL(); got != disabledURL {
		t.Fatalf("disabled pin was cleared = %q, want kept %q", got, disabledURL)
	}
	if got := store.ResolveProxyForAccount(store.FindByID(pinnedID)); got != "" {
		t.Fatalf("disabled pin still resolved = %q", got)
	}
	selected := store.NextExcludingWithFilter(0, nil, nil)
	if selected == nil {
		t.Fatal("expected unbound account to remain schedulable")
	}
	defer store.Release(selected)
	if selected.DBID != unboundID {
		t.Fatalf("selected account %d, want unbound %d", selected.DBID, unboundID)
	}
	if got := store.ResolveProxyForAccount(selected); got != enabledURL {
		t.Fatalf("unbound proxy = %q, want remaining pool member %q", got, enabledURL)
	}

	enabled := true
	if err := db.UpdateProxy(ctx, disabledID, nil, nil, &enabled); err != nil {
		t.Fatalf("re-enable proxy: %v", err)
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool after re-enable: %v", err)
	}
	if got := store.ResolveProxyForAccount(store.FindByID(pinnedID)); got != disabledURL {
		t.Fatalf("re-enabled pin = %q, want original %q", got, disabledURL)
	}
}

func TestCleanErrorProxiesPreservesConcurrentRuntimeRebind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const (
		errorURL = "http://error.example:8080"
		newURL   = "http://new.example:8080"
	)
	errorID, err := db.InsertProxy(ctx, errorURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	if err := db.UpdateProxyTestResult(ctx, errorID, errorURL, database.ProxyTestStatusError, "", "", 0); err != nil {
		t.Fatalf("mark proxy error: %v", err)
	}
	accountID, err := db.InsertAccount(ctx, "bound", "rt-bound", errorURL)
	if err != nil {
		t.Fatalf("InsertAccount returned error: %v", err)
	}

	store := newAdminProxyTestStore(t, db)
	store.AddAccount(&auth.Account{
		DBID:        accountID,
		AccessToken: "tok-bound",
		ProxyURL:    newURL,
		Status:      auth.StatusReady,
	})
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/proxies/clean-error", nil)
	handler.CleanErrorProxies(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := store.FindByID(accountID).GetProxyURL(); got != newURL {
		t.Fatalf("runtime account proxy = %q, want concurrent rebind %q preserved", got, newURL)
	}
}

func TestCleanErrorProxiesReportsReloadFailureAfterFailClosedRemoval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const errorURL = "http://error.example:8080"
	errorID, err := db.InsertProxy(ctx, errorURL, "")
	if err != nil {
		t.Fatalf("InsertProxy returned error: %v", err)
	}
	store := newAdminProxyTestStore(t, db)
	if err := db.UpdateProxyTestResult(ctx, errorID, errorURL, database.ProxyTestStatusError, "", "", 0); err != nil {
		t.Fatalf("mark proxy error: %v", err)
	}

	handler := &Handler{
		db:    db,
		store: store,
		reloadProxyPoolFn: func() error {
			return errors.New("reload unavailable")
		},
	}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/proxies/clean-error", nil)
	handler.CleanErrorProxies(ginCtx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if got := store.NextProxy(); got != "" {
		t.Fatalf("NextProxy after cleanup reload failure = %q, want deleted proxies removed", got)
	}
}
