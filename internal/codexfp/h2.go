// Package codexfp aligns codex2api's outbound network fingerprint with the real
// codex-rs client (reqwest + h2 + rustls). All constants are MEASURED from a live
// MITM capture of the official codex CLI 0.142.5 talking to chatgpt.com
// (see codex-fpcap/BASELINE.md), not guessed.
//
// Why this matters: Go's stock *http2.Transport emits a SETTINGS frame that is a
// dead giveaway of a Go net/http client — a 4 MiB stream window, no
// MAX_HEADER_LIST_SIZE, and an immediate ~1 GiB connection WINDOW_UPDATE. That
// contradicts a codex_cli_rs User-Agent the instant the connection opens, no
// matter how well the TLS/headers are spoofed. Real codex-rs (reqwest/h2) sends
// a very different, small, fixed SETTINGS frame. This package makes Go emit a
// byte-identical one.
package codexfp

import (
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

// codex-rs (reqwest/h2) HTTP/2 SETTINGS profile. Verified identical across
// codex 0.130.0 and 0.142.5 (library-level, version-independent).
//
// Observed SETTINGS frame, in send order:
//
//	ENABLE_PUSH           = 0
//	INITIAL_WINDOW_SIZE   = 2097152   (2 MiB)
//	MAX_FRAME_SIZE        = 16384
//	MAX_HEADER_LIST_SIZE  = 16384
//
// followed by a connection-level WINDOW_UPDATE(stream 0) increment of 5177345.
// HEADER_TABLE_SIZE is left at the 4096 default (not emitted), matching codex.
const (
	H2InitialWindowSize   = 2097152 // SETTINGS_INITIAL_WINDOW_SIZE (stream), 2 MiB
	H2MaxFrameSize        = 16384   // SETTINGS_MAX_FRAME_SIZE
	H2MaxHeaderListSize   = 16384   // SETTINGS_MAX_HEADER_LIST_SIZE
	H2ConnWindowIncrement = 5177345 // connection-level WINDOW_UPDATE increment
)

// NewCodexH2Transport returns an *http2.Transport whose initial SETTINGS frame
// (values AND order) plus the following connection WINDOW_UPDATE are identical
// to real codex-rs. The stream/connection flow-control windows are only
// reachable through net/http.HTTP2Config, so we link a throwaway *http.Transport
// via ConfigureTransports; MAX_FRAME_SIZE / MAX_HEADER_LIST_SIZE are set directly
// on the returned transport.
func NewCodexH2Transport() (*http2.Transport, error) {
	t1 := &http.Transport{
		HTTP2: &http.HTTP2Config{
			// -> SETTINGS_INITIAL_WINDOW_SIZE (conf.MaxUploadBufferPerStream)
			MaxReceiveBufferPerStream: H2InitialWindowSize,
			// -> WINDOW_UPDATE(0, increment) (conf.MaxUploadBufferPerConnection)
			MaxReceiveBufferPerConnection: H2ConnWindowIncrement,
		},
	}
	t2, err := http2.ConfigureTransports(t1)
	if err != nil {
		return nil, err
	}
	t2.MaxReadFrameSize = H2MaxFrameSize    // -> SETTINGS_MAX_FRAME_SIZE
	t2.MaxHeaderListSize = H2MaxHeaderListSize // -> SETTINGS_MAX_HEADER_LIST_SIZE
	return t2, nil
}

// NewCodexH2ClientConn wraps an already-established (TLS) conn in an HTTP/2
// client connection using the codex-rs SETTINGS profile. Drop-in replacement
// for `(&http2.Transport{}).NewClientConn(conn)`.
func NewCodexH2ClientConn(conn net.Conn) (*http2.ClientConn, error) {
	t, err := NewCodexH2Transport()
	if err != nil {
		return nil, err
	}
	return t.NewClientConn(conn)
}
