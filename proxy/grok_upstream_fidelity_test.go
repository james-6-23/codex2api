package proxy

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestGrokTurnIndex(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"string input", `{"input":"hello"}`, 1},
		{"one user turn", `{"input":[{"type":"message","role":"developer","content":[]},{"type":"message","role":"user","content":[]}]}`, 1},
		{"three user turns", `{"input":[{"type":"message","role":"user"},{"type":"reasoning"},{"type":"message","role":"assistant"},{"type":"message","role":"user"},{"type":"message","role":"user"}]}`, 3},
		{"no user messages", `{"input":[{"type":"reasoning"}]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokTurnIndex([]byte(tc.body)); got != tc.want {
				t.Fatalf("grokTurnIndex = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGrokBlobDetection(t *testing.T) {
	if !grokBodyHasBlobs([]byte(`{"input":[{"type":"reasoning","encrypted_content":"x"}]}`)) {
		t.Fatal("should detect encrypted_content")
	}
	if !grokBodyHasBlobs([]byte(`{"input":[{"type":"compaction"}]}`)) {
		t.Fatal("should detect compaction")
	}
	if grokBodyHasBlobs([]byte(`{"input":[{"type":"message"}]}`)) {
		t.Fatal("should not detect blobs in plain body")
	}

	if !grokIsBlobDecodeFailure([]byte(`{"code":"400","error":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`)) {
		t.Fatal("should recognize compaction blob decode failure")
	}
	if !grokIsBlobDecodeFailure([]byte(`{"error":"could not decrypt the provided encrypted_content"}`)) {
		t.Fatal("should recognize decrypt failure")
	}
	if grokIsBlobDecodeFailure([]byte(`{"error":"Argument not supported: external_web_access"}`)) {
		t.Fatal("unrelated 400 should not be treated as blob decode failure")
	}
}

func TestDecodeContentEncoding(t *testing.T) {
	original := []byte(`{"hello":"world","n":42}`)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(original)
	_ = gw.Close()
	if out, err := decodeContentEncoding(gz.Bytes(), "gzip"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("gzip decode failed: %v out=%s", err, out)
	}

	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write(original)
	_ = bw.Close()
	if out, err := decodeContentEncoding(br.Bytes(), "br"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("br decode failed: %v out=%s", err, out)
	}

	// identity / 空编码原样返回。
	if out, err := decodeContentEncoding(original, "identity"); err != nil || !bytes.Equal(out, original) {
		t.Fatalf("identity should pass through: %v", err)
	}
}
