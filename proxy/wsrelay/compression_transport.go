package wsrelay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

type upstreamWebsocket interface {
	Close() error
	SetReadLimit(int64)
	SetWriteDeadline(time.Time) error
	WriteMessage(int, []byte) error
	NextReader() (int, io.Reader, error)
	RemoteAddr() net.Addr
}

type contextTakeoverSocket struct {
	conn          *coderws.Conn
	peer          net.Addr
	deadlineMu    sync.Mutex
	writeDeadline time.Time
	onPing        func()
	onPong        func(string)
}

func dialContextTakeover(ctx context.Context, dialer *websocket.Dialer, wsURL string, headers http.Header) (upstreamWebsocket, *http.Response, error) {
	if dialer.HandshakeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialer.HandshakeTimeout)
		defer cancel()
	}
	transport := &http.Transport{
		Proxy:                 dialer.Proxy,
		DialContext:           dialer.NetDialContext,
		DialTLSContext:        dialer.NetDialTLSContext,
		TLSHandshakeTimeout:   dialer.HandshakeTimeout,
		ResponseHeaderTimeout: dialer.HandshakeTimeout,
		DisableCompression:    true,
		ReadBufferSize:        dialer.ReadBufferSize,
		WriteBufferSize:       dialer.WriteBufferSize,
		Protocols:             new(http.Protocols),
	}
	transport.Protocols.SetHTTP1(true)
	if transport.DialContext == nil && dialer.NetDial != nil {
		transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
			return dialer.NetDial(network, address)
		}
	}
	if err := configureTakeoverPlaintextProxy(transport, wsURL, headers); err != nil {
		return nil, nil, err
	}
	transport.TLSClientConfig = &tls.Config{}
	if dialer.TLSClientConfig != nil {
		transport.TLSClientConfig = dialer.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Jar:       dialer.Jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	socket := &contextTakeoverSocket{}
	settings := proxy.CurrentRuntimeSettings()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			socket.peer = info.Conn.RemoteAddr()
		},
	})
	options := &coderws.DialOptions{
		HTTPClient:           client,
		HTTPHeader:           headers.Clone(),
		Subprotocols:         append([]string(nil), dialer.Subprotocols...),
		CompressionMode:      coderws.CompressionContextTakeover,
		CompressionThreshold: 1,
		CompressionLevel:     settings.CodexWSCompressionLevel,
		DisableFragmentation: settings.CodexWSDisableFragmentation,
		OnPingReceived: func(_ context.Context, _ []byte) bool {
			if socket.onPing != nil {
				socket.onPing()
			}
			return true
		},
		OnPongReceived: func(_ context.Context, payload []byte) {
			if socket.onPong != nil {
				socket.onPong(string(payload))
			}
		},
	}
	if host := options.HTTPHeader.Get("Host"); host != "" {
		options.Host = host
		options.HTTPHeader.Del("Host")
	}
	if len(options.Subprotocols) == 0 {
		for _, value := range options.HTTPHeader.Values("Sec-WebSocket-Protocol") {
			for _, protocol := range strings.Split(value, ",") {
				if protocol = strings.TrimSpace(protocol); protocol != "" {
					options.Subprotocols = append(options.Subprotocols, protocol)
				}
			}
		}
	}
	connection, response, err := coderws.Dial(ctx, wsURL, options)
	if err != nil {
		return nil, response, err
	}
	socket.conn = connection
	return socket, response, nil
}

func (socket *contextTakeoverSocket) Close() error {
	return socket.conn.CloseNow()
}

func (socket *contextTakeoverSocket) SetReadLimit(limit int64) {
	socket.conn.SetReadLimit(limit)
}

func (socket *contextTakeoverSocket) SetWriteDeadline(deadline time.Time) error {
	socket.deadlineMu.Lock()
	socket.writeDeadline = deadline
	socket.deadlineMu.Unlock()
	return nil
}

func (socket *contextTakeoverSocket) WriteMessage(messageType int, payload []byte) error {
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return fmt.Errorf("unsupported upstream websocket message type: %d", messageType)
	}
	socket.deadlineMu.Lock()
	deadline := socket.writeDeadline
	socket.deadlineMu.Unlock()
	ctx := context.Background()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return normalizeTakeoverError(socket.conn.Write(ctx, coderws.MessageType(messageType), payload))
}

func (socket *contextTakeoverSocket) NextReader() (int, io.Reader, error) {
	messageType, reader, err := socket.conn.Reader(context.Background())
	if err != nil {
		return 0, nil, normalizeTakeoverError(err)
	}
	return int(messageType), takeoverMessageReader{Reader: reader}, nil
}

func (socket *contextTakeoverSocket) RemoteAddr() net.Addr {
	return socket.peer
}

func (socket *contextTakeoverSocket) writePing(ctx context.Context, payload string) error {
	return normalizeTakeoverError(socket.conn.WritePing(ctx, []byte(payload)))
}

type takeoverMessageReader struct {
	io.Reader
}

func (reader takeoverMessageReader) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	return count, normalizeTakeoverError(err)
}

func normalizeTakeoverError(err error) error {
	if errors.Is(err, coderws.ErrMessageTooBig) {
		return fmt.Errorf("%w: %v", websocket.ErrReadLimit, err)
	}
	var closeError coderws.CloseError
	if errors.As(err, &closeError) {
		return &websocket.CloseError{Code: int(closeError.Code), Text: closeError.Reason}
	}
	return err
}
