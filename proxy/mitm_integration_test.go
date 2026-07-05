package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/codex2api/internal/codexfp"
)

// TestMITMSideBySide drives our REAL transport functions (rustlsClientHelloSpec +
// codexfp.NewCodexH2ClientConn) through a locally-running MITM capture proxy so
// its log can be diffed against the real codex-rs baseline. Gated: set
// CODEX_FP_MITM=127.0.0.1:8899 (the MITM CONNECT proxy) to run.
func TestMITMSideBySide(t *testing.T) {
	proxyAddr := os.Getenv("CODEX_FP_MITM")
	if proxyAddr == "" {
		t.Skip("set CODEX_FP_MITM=host:port (running MITM) to emit our fingerprint")
	}
	raw, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial mitm: %v", err)
	}
	defer raw.Close()
	fmt.Fprintf(raw, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n\r\n")
	br := bufio.NewReader(raw)
	// consume the CONNECT 200 response head exactly (until blank line)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read connect resp: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	var conn net.Conn = raw
	if br.Buffered() > 0 {
		conn = &prefixedConn{Conn: raw, r: br}
	}

	// EXACT production code path: rustls spec + codex h2 SETTINGS.
	uconn := utls.UClient(conn, &utls.Config{ServerName: "chatgpt.com", InsecureSkipVerify: true}, utls.HelloCustom)
	if err := uconn.ApplyPreset(rustlsClientHelloSpec()); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if err := uconn.Handshake(); err != nil {
		t.Fatalf("handshake via mitm: %v", err)
	}
	cc, err := codexfp.NewCodexH2ClientConn(uconn)
	if err != nil {
		t.Fatalf("h2 client conn: %v", err)
	}
	defer cc.Close()
	req, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set("user-agent", "codex2api-selfcheck/0.142.3")
	resp, err := cc.RoundTrip(req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond) // let MITM flush its log
	t.Logf("emitted our fingerprint through MITM (roundtrip err=%v)", err)
}

type prefixedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixedConn) Read(b []byte) (int, error) { return c.r.Read(b) }
