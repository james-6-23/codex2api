package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeRoundTripper struct {
	name    string
	err     error
	calls   int
	gotBody []byte
}

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	if r.Body != nil {
		f.gotBody, _ = io.ReadAll(r.Body)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("ok:" + f.name)),
		Header:     make(http.Header),
	}, nil
}

// On a connection-establishment error, the fallback must run, the POST body must
// be replayed intact, and the transport must stick to fallback afterwards.
func TestRustlsFallbackOnConnError(t *testing.T) {
	primary := &fakeRoundTripper{name: "primary", err: errors.New("TLS 握手失败: simulated")}
	fallback := &fakeRoundTripper{name: "fallback"}
	tr := &rustlsFallbackTransport{primary: primary, fallback: fallback}

	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/x", bytes.NewReader([]byte("codex-body")))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(fallback.gotBody) != "codex-body" {
		t.Fatalf("fallback body = %q, want codex-body (replay failed)", fallback.gotBody)
	}
	if !tr.tripped {
		t.Fatal("transport should be sticky-tripped after fallback")
	}

	// Second request must skip the (broken) primary entirely.
	req2, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/x", bytes.NewReader([]byte("second")))
	if _, err := tr.RoundTrip(req2); err != nil {
		t.Fatalf("second roundtrip err: %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("primary called %d times, want 1 (sticky fallback)", primary.calls)
	}
	if fallback.calls != 2 {
		t.Fatalf("fallback called %d times, want 2", fallback.calls)
	}
}

// A mid-stream / application error (not a connection-establishment failure) must
// NOT trigger fallback — the request may have already been partially sent.
func TestRustlsFallbackNotTriggeredOnAppError(t *testing.T) {
	appErr := errors.New("stream disconnected before completion")
	primary := &fakeRoundTripper{name: "primary", err: appErr}
	fallback := &fakeRoundTripper{name: "fallback"}
	tr := &rustlsFallbackTransport{primary: primary, fallback: fallback}

	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/x", bytes.NewReader([]byte("x")))
	_, err := tr.RoundTrip(req)
	if err != appErr {
		t.Fatalf("err = %v, want the original app error (no fallback)", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback must not run on app error, calls=%d", fallback.calls)
	}
	if tr.tripped {
		t.Fatal("must not trip on app error")
	}
}
