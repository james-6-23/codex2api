package auth

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigureTransportProxyHTTPProxy(t *testing.T) {
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if err := ConfigureTransportProxy(transport, "http://127.0.0.1:8080", baseDialer); err != nil {
		t.Fatalf("ConfigureTransportProxy() error = %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected HTTP proxy handler to be configured")
	}
	if transport.DialContext == nil {
		t.Fatal("expected HTTP proxy to preserve the base dialer")
	}
}

func TestBuildHTTPClientCheckedRejectsInvalidProxy(t *testing.T) {
	if _, err := BuildHTTPClientChecked("not-a-proxy"); err == nil {
		t.Fatal("BuildHTTPClientChecked accepted malformed proxy and may bypass it")
	}
}

func TestConfigureTransportProxySOCKS5Proxy(t *testing.T) {
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if err := ConfigureTransportProxy(transport, "socks5://127.0.0.1:1080", baseDialer); err != nil {
		t.Fatalf("ConfigureTransportProxy() error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("expected SOCKS5 proxy to bypass transport.Proxy")
	}
	if transport.DialContext == nil {
		t.Fatal("expected SOCKS5 proxy dialer to be installed")
	}
}

func TestConfigureTransportProxyConnectObserverRunsAfterProxyTCPConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	var connected atomic.Bool
	transport := &http.Transport{}
	baseDialer := &net.Dialer{Timeout: time.Second}
	if err := ConfigureTransportProxyWithObserver(
		transport,
		"http://"+listener.Addr().String(),
		baseDialer,
		ProxyTransportObserver{
			OnProxyConnect: func() { connected.Store(true) },
		},
	); err != nil {
		t.Fatalf("ConfigureTransportProxyWithObserver() error = %v", err)
	}

	conn, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = conn.Close()
	if !connected.Load() {
		t.Fatal("proxy TCP connect observer was not called")
	}
}
