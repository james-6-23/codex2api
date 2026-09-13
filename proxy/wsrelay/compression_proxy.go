package wsrelay

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Gorilla uses CONNECT for plaintext WS as well as WSS. Preserve that behavior
// when the optional HTTP transport would otherwise send a forward-proxy GET.
func configureTakeoverPlaintextProxy(transport *http.Transport, wsURL string, headers http.Header) error {
	target, err := url.Parse(wsURL)
	if err != nil {
		return err
	}
	if transport.Proxy == nil || (target.Scheme != "ws" && target.Scheme != "http") {
		return nil
	}
	target.Scheme = "http"
	proxyURL, err := transport.Proxy(&http.Request{URL: target, Header: headers})
	if err != nil {
		return err
	}
	if proxyURL == nil || proxyURL.Scheme != "http" {
		return nil
	}
	baseDial := transport.DialContext
	if baseDial == nil {
		baseDial = (&net.Dialer{}).DialContext
	}
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		proxyAddress := proxyURL.Host
		if proxyURL.Port() == "" {
			proxyAddress = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
		connection, err := baseDial(ctx, network, proxyAddress)
		if err != nil {
			return nil, err
		}
		keep := false
		defer func() {
			if !keep {
				_ = connection.Close()
			}
		}()
		stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
		defer stop()
		if deadline, exists := ctx.Deadline(); exists {
			_ = connection.SetDeadline(deadline)
		}
		request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			request.SetBasicAuth(proxyURL.User.Username(), password)
			request.Header.Set("Proxy-Authorization", request.Header.Get("Authorization"))
			request.Header.Del("Authorization")
		}
		if err := request.Write(connection); err != nil {
			return nil, err
		}
		reader := bufio.NewReader(connection)
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("WebSocket proxy CONNECT returned HTTP %d", response.StatusCode)
		}
		if !stop() {
			return nil, ctx.Err()
		}
		if err := connection.SetDeadline(time.Time{}); err != nil {
			return nil, err
		}
		keep = true
		return &takeoverTunnelConn{Conn: connection, reader: reader}, nil
	}
	return nil
}

type takeoverTunnelConn struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *takeoverTunnelConn) Read(payload []byte) (int, error) {
	return connection.reader.Read(payload)
}
