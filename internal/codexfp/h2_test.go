package codexfp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// capturedH2 holds what a peer observes from our client's opening frames.
type capturedH2 struct {
	order      []http2.SettingID
	settings   map[http2.SettingID]uint32
	connWindow uint32
}

// TestCodexH2SettingsMatchRealCodex asserts that a client connection built by
// NewCodexH2ClientConn emits the exact SETTINGS frame (values AND order) plus
// the connection WINDOW_UPDATE increment measured from real codex-rs 0.142.5.
// This is the regression guard for the HTTP/2 fingerprint.
func TestCodexH2SettingsMatchRealCodex(t *testing.T) {
	srvCfg := selfSignedServerTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan capturedH2, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		sc := tls.Server(conn, srvCfg)
		sc.SetDeadline(time.Now().Add(5 * time.Second))
		if err := sc.Handshake(); err != nil {
			errCh <- err
			return
		}
		pre := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(sc, pre); err != nil {
			errCh <- err
			return
		}
		fr := http2.NewFramer(sc, sc)
		cap := capturedH2{settings: map[http2.SettingID]uint32{}}
		for i := 0; i < 4; i++ {
			f, err := fr.ReadFrame()
			if err != nil {
				break
			}
			switch ff := f.(type) {
			case *http2.SettingsFrame:
				if ff.IsAck() {
					continue
				}
				ff.ForeachSetting(func(s http2.Setting) error {
					cap.order = append(cap.order, s.ID)
					cap.settings[s.ID] = s.Val
					return nil
				})
			case *http2.WindowUpdateFrame:
				if ff.StreamID == 0 {
					cap.connWindow = ff.Increment
				}
			}
			if len(cap.order) > 0 && cap.connWindow > 0 {
				break
			}
		}
		resultCh <- cap
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tc := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		ServerName:         "localhost",
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	cc, err := NewCodexH2ClientConn(tc)
	if err != nil {
		t.Fatalf("NewCodexH2ClientConn: %v", err)
	}
	defer cc.Close()

	select {
	case err := <-errCh:
		t.Fatalf("server side: %v", err)
	case got := <-resultCh:
		// Exact values measured from real codex-rs 0.142.5.
		want := map[http2.SettingID]uint32{
			http2.SettingEnablePush:         0,
			http2.SettingInitialWindowSize:  H2InitialWindowSize,
			http2.SettingMaxFrameSize:       H2MaxFrameSize,
			http2.SettingMaxHeaderListSize:  H2MaxHeaderListSize,
		}
		for id, wv := range want {
			if gv, ok := got.settings[id]; !ok || gv != wv {
				t.Errorf("SETTINGS[%v] = %d (present=%v), want %d", id, gv, ok, wv)
			}
		}
		// HEADER_TABLE_SIZE must NOT be sent (codex leaves it at 4096 default).
		if _, ok := got.settings[http2.SettingHeaderTableSize]; ok {
			t.Errorf("SETTINGS must not include HEADER_TABLE_SIZE, codex omits it")
		}
		// Order must be: ENABLE_PUSH, INITIAL_WINDOW_SIZE, MAX_FRAME_SIZE, MAX_HEADER_LIST_SIZE.
		wantOrder := []http2.SettingID{
			http2.SettingEnablePush,
			http2.SettingInitialWindowSize,
			http2.SettingMaxFrameSize,
			http2.SettingMaxHeaderListSize,
		}
		if len(got.order) != len(wantOrder) {
			t.Fatalf("SETTINGS order = %v, want %v", got.order, wantOrder)
		}
		for i := range wantOrder {
			if got.order[i] != wantOrder[i] {
				t.Fatalf("SETTINGS order = %v, want %v", got.order, wantOrder)
			}
		}
		// Connection WINDOW_UPDATE increment must match (guards the <4 MiB
		// net/http validation concern for 5177345).
		if got.connWindow != H2ConnWindowIncrement {
			t.Errorf("conn WINDOW_UPDATE increment = %d, want %d", got.connWindow, H2ConnWindowIncrement)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for server to read client SETTINGS")
	}
}

func selfSignedServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"h2"},
	}
}
