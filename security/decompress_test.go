package security

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const testPlainBody = `{"model":"gpt-5.6-sol","stream":true,"input":"hello"}`

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func brotliCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	if _, err := bw.Write(data); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func zlibCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

// runDecompressor 以「RequestSizeLimiter 在前」的真实链路跑中间件，返回下游
// 处理器看到的 raw_body、请求头与响应。
func runDecompressor(t *testing.T, body []byte, contentEncoding string, maxSize int64) (downstreamBody []byte, downstreamEncoding string, w *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestSizeLimiter(maxSize))
	r.Use(RequestBodyDecompressor(maxSize))
	r.POST("/test", func(c *gin.Context) {
		if cached, exists := c.Get("raw_body"); exists {
			downstreamBody, _ = cached.([]byte)
		}
		downstreamEncoding = c.GetHeader("Content-Encoding")
		got, _ := io.ReadAll(c.Request.Body)
		if !bytes.Equal(got, downstreamBody) {
			t.Errorf("c.Request.Body(%d bytes) 与 raw_body(%d bytes) 不一致", len(got), len(downstreamBody))
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return downstreamBody, downstreamEncoding, w
}

func TestRequestBodyDecompressorEncodings(t *testing.T) {
	plain := []byte(testPlainBody)
	cases := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{"zstd", "zstd", zstdCompress(t, plain)},
		{"gzip", "gzip", gzipCompress(t, plain)},
		{"x-gzip", "x-gzip", gzipCompress(t, plain)},
		{"brotli", "br", brotliCompress(t, plain)},
		{"deflate-zlib", "deflate", zlibCompress(t, plain)},
		{"identity", "identity", plain},
		{"none", "", plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, encoding, w := runDecompressor(t, tc.body, tc.encoding, 1<<20)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("downstream body = %q, want %q", got, plain)
			}
			if tc.encoding != "" && tc.encoding != "identity" && encoding != "" {
				t.Fatalf("Content-Encoding 头未移除: %q", encoding)
			}
		})
	}
}

func TestRequestBodyDecompressorChainedEncodings(t *testing.T) {
	plain := []byte(testPlainBody)
	// 施加顺序 gzip → zstd，头按施加顺序列出，解码应逆序进行。
	body := zstdCompress(t, gzipCompress(t, plain))
	got, encoding, w := runDecompressor(t, body, "gzip, zstd", 1<<20)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("downstream body = %q, want %q", got, plain)
	}
	if encoding != "" {
		t.Fatalf("Content-Encoding 头未移除: %q", encoding)
	}
}

func TestRequestBodyDecompressorUnknownEncodingPassthrough(t *testing.T) {
	plain := []byte(testPlainBody)
	got, encoding, w := runDecompressor(t, plain, "amz-1.0", 1<<20)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("未知编码应原样透传, got %q", got)
	}
	if encoding != "amz-1.0" {
		t.Fatalf("未知编码的 Content-Encoding 头应保留, got %q", encoding)
	}
}

func TestRequestBodyDecompressorCorruptBody(t *testing.T) {
	_, _, w := runDecompressor(t, []byte("not-zstd-data"), "zstd", 1<<20)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestRequestBodyDecompressorBombGuard(t *testing.T) {
	// 压缩后很小、解压后超限的高压缩比载荷。
	huge := bytes.Repeat([]byte("a"), 1<<20)
	body := zstdCompress(t, huge)
	_, _, w := runDecompressor(t, body, "zstd", 1<<10)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body = %s", w.Code, w.Body.String())
	}
}

func TestRequestBodyDecompressorRawDeflateFallback(t *testing.T) {
	plain := []byte(testPlainBody)
	var buf bytes.Buffer
	fw, err := newRawFlateWriter(&buf)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := fw.Write(plain); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	got, _, w := runDecompressor(t, buf.Bytes(), "deflate", 1<<20)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("downstream body = %q, want %q", got, plain)
	}
}

func newRawFlateWriter(w io.Writer) (io.WriteCloser, error) {
	return flate.NewWriter(w, flate.DefaultCompression)
}
