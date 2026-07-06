package proxy

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// parsedHello holds the fields we assert against the real codex-rs baseline.
type parsedHello struct {
	ciphers  []uint16
	extOrder []uint16
	groups   []uint16
	keyShare []uint16
	sigAlgs  []uint16
}

// captureRustlsHello dials a throwaway listener with our rustls uTLS spec and
// returns the raw ClientHello the peer observed. The handshake never completes
// (the listener just captures bytes), but the ClientHello is sent first.
func captureRustlsHello(t *testing.T) parsedHello {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	recCh := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			recCh <- nil
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			recCh <- nil
			return
		}
		n := int(hdr[3])<<8 | int(hdr[4])
		body := make([]byte, n)
		io.ReadFull(c, body)
		recCh <- body
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:         "chatgpt.com",
		InsecureSkipVerify: true,
		OmitEmptyPsk:       true, // 避免 empty psk 错误
	}, utls.HelloCustom)
	if err := uconn.ApplyPreset(rustlsClientHelloSpec()); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	// Handshake will fail (no real server), but the ClientHello is written first.
	go uconn.Handshake()

	select {
	case body := <-recCh:
		if body == nil {
			t.Fatal("failed to capture ClientHello")
		}
		return parseHelloForTest(t, body)
	case <-time.After(4 * time.Second):
		t.Fatal("timeout capturing ClientHello")
		return parsedHello{}
	}
}

func TestRustlsHelloMatchesRealCodex(t *testing.T) {
	h := captureRustlsHello(t)

	// --- cipher suites (order fixed, AES256 first, SCSV last) ---
	wantCiphers := []uint16{
		0x1302, 0x1301, 0x1303, 0xc02c, 0xc02b, 0xcca9, 0xc030, 0xc02f, 0xcca8, 0x00ff,
	}
	if len(h.ciphers) != len(wantCiphers) {
		t.Fatalf("ciphers = %#x, want %#x", h.ciphers, wantCiphers)
	}
	for i := range wantCiphers {
		if h.ciphers[i] != wantCiphers[i] {
			t.Fatalf("ciphers = %#x, want %#x", h.ciphers, wantCiphers)
		}
	}

	// --- supported_groups: MLKEM768, X25519, P256, P384 ---
	wantGroups := []uint16{0x11ec, 0x001d, 0x0017, 0x0018}
	if !equalU16(h.groups, wantGroups) {
		t.Errorf("supported_groups = %#x, want %#x", h.groups, wantGroups)
	}
	// --- key_share: MLKEM768, X25519 ---
	wantKS := []uint16{0x11ec, 0x001d}
	if !equalU16(h.keyShare, wantKS) {
		t.Errorf("key_share = %#x, want %#x", h.keyShare, wantKS)
	}
	// --- sigalgs: must include ed25519 (0x0807) and ecdsa521 (0x0603) ---
	if !containsU16(h.sigAlgs, 0x0807) {
		t.Errorf("sigalgs missing ed25519 (0x0807): %#x", h.sigAlgs)
	}
	if !containsU16(h.sigAlgs, 0x0603) {
		t.Errorf("sigalgs missing ecdsa_secp521r1_sha512 (0x0603): %#x", h.sigAlgs)
	}
	// --- must NOT carry renegotiation_info extension (rustls uses 00ff SCSV) ---
	if containsU16(h.extOrder, 0xff01) {
		t.Errorf("hello must not include renegotiation_info ext (rustls uses SCSV): %#x", h.extOrder)
	}
	// --- extension set (11, order-independent) ---
	wantExts := []uint16{0x0000, 0x0005, 0x000a, 0x000b, 0x000d, 0x0010, 0x0017, 0x0023, 0x002b, 0x002d, 0x0033}
	for _, e := range wantExts {
		if !containsU16(h.extOrder, e) {
			t.Errorf("hello missing extension 0x%04x; got %#x", e, h.extOrder)
		}
	}
}

// TestRustlsHelloExtensionsShuffled 验证扩展每次随机打乱（对齐真实 codex 反 ossification），
// 但 pre_shared_key(41) 若存在必须最后（TLS 1.3 约束）。多次抓取应出现不同顺序。
func TestRustlsHelloExtensionsShuffled(t *testing.T) {
	seen := map[string]bool{}
	const tries = 8
	for i := 0; i < tries; i++ {
		h := captureRustlsHello(t)
		// 关键扩展必须存在
		for _, ext := range []uint16{0, 16, 43, 51} { // SNI, ALPN, SupportedVersions, KeyShare
			if !containsU16(h.extOrder, ext) {
				t.Fatalf("缺少扩展 0x%04x, 实际: %v", ext, h.extOrder)
			}
		}
		// PSK(41) 若存在必须最后
		if containsU16(h.extOrder, 41) && h.extOrder[len(h.extOrder)-1] != 41 {
			t.Fatalf("PreSharedKey(41) 必须最后, 实际顺序: %v", h.extOrder)
		}
		// 记录顺序指纹
		key := ""
		for _, e := range h.extOrder {
			key += fmt.Sprintf("%d,", e)
		}
		seen[key] = true
	}
	// 8 次里应出现多种顺序（随机打乱生效）；若全同则打乱失效
	if len(seen) < 2 {
		t.Errorf("扩展顺序 %d 次全相同, 随机打乱未生效", tries)
	}
}

func parseHelloForTest(t *testing.T, hs []byte) parsedHello {
	t.Helper()
	var h parsedHello
	if len(hs) < 38 || hs[0] != 0x01 {
		t.Fatalf("not a ClientHello")
	}
	p := hs[4:]
	p = p[2+32:] // version + random
	sidLen := int(p[0])
	p = p[1+sidLen:]
	csLen := int(p[0])<<8 | int(p[1])
	p = p[2:]
	for i := 0; i+1 < csLen; i += 2 {
		h.ciphers = append(h.ciphers, uint16(p[i])<<8|uint16(p[i+1]))
	}
	p = p[csLen:]
	cmLen := int(p[0])
	p = p[1+cmLen:]
	extTotal := int(p[0])<<8 | int(p[1])
	p = p[2:]
	if len(p) > extTotal {
		p = p[:extTotal]
	}
	for len(p) >= 4 {
		et := uint16(p[0])<<8 | uint16(p[1])
		el := int(p[2])<<8 | int(p[3])
		p = p[4:]
		if len(p) < el {
			break
		}
		data := p[:el]
		p = p[el:]
		h.extOrder = append(h.extOrder, et)
		switch et {
		case 0x000a:
			h.groups = parseU16ListTest(data)
		case 0x000d:
			h.sigAlgs = parseU16ListTest(data)
		case 0x0033:
			if len(data) >= 2 {
				n := int(data[0])<<8 | int(data[1])
				d := data[2:]
				if len(d) > n {
					d = d[:n]
				}
				for len(d) >= 4 {
					g := uint16(d[0])<<8 | uint16(d[1])
					kl := int(d[2])<<8 | int(d[3])
					d = d[4:]
					if len(d) < kl {
						break
					}
					d = d[kl:]
					h.keyShare = append(h.keyShare, g)
				}
			}
		}
	}
	return h
}

func parseU16ListTest(d []byte) []uint16 {
	if len(d) < 2 {
		return nil
	}
	n := int(d[0])<<8 | int(d[1])
	d = d[2:]
	if len(d) > n {
		d = d[:n]
	}
	var out []uint16
	for i := 0; i+1 < len(d); i += 2 {
		out = append(out, uint16(d[i])<<8|uint16(d[i+1]))
	}
	return out
}

func equalU16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsU16(s []uint16, v uint16) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
