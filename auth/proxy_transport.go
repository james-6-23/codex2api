package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// ConfigureTransportProxy applies HTTP(S) or SOCKS5 proxy settings to a transport.
func ConfigureTransportProxy(transport *http.Transport, rawProxyURL string, baseDialer *net.Dialer) error {
	if transport == nil || strings.TrimSpace(rawProxyURL) == "" {
		return nil
	}
	if baseDialer == nil {
		baseDialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}

	u, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
		transport.DialContext = baseDialer.DialContext
		return nil
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: password}
		}

		dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, baseDialer)
		if err != nil {
			return fmt.Errorf("build socks5 dialer: %w", err)
		}
		if cd, ok := dialer.(contextDialer); ok {
			transport.DialContext = cd.DialContext
			transport.Proxy = nil
			return nil
		}

		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			type result struct {
				conn net.Conn
				err  error
			}
			done := make(chan result, 1)
			go func() {
				conn, err := dialer.Dial(network, address)
				done <- result{conn: conn, err: err}
			}()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case out := <-done:
				return out.conn, out.err
			}
		}
		transport.Proxy = nil
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
}
