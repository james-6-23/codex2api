package security

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// errDecompressedBodyTooLarge 表示解压后的请求体超过大小上限（解压炸弹防护）。
var errDecompressedBodyTooLarge = errors.New("decompressed body too large")

// A decoder window rejection does not establish the decoded body length.
// Preserve that distinction instead of reporting a guessed byte count.
type decompressedSizeError struct {
	stage    string
	observed int64
}

func (e *decompressedSizeError) Error() string { return errDecompressedBodyTooLarge.Error() }
func (e *decompressedSizeError) Unwrap() error { return errDecompressedBodyTooLarge }

// parseContentEncodings 解析 Content-Encoding 头，返回小写、去空白的编码列表。
// 按 RFC 9110，多个编码按施加顺序列出，解码时需要逆序处理；identity 视为无编码。
func parseContentEncodings(header string) []string {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	var encodings []string
	for _, token := range strings.Split(header, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" || token == "identity" {
			continue
		}
		encodings = append(encodings, token)
	}
	return encodings
}

func isSupportedContentEncoding(encoding string) bool {
	switch encoding {
	case "zstd", "gzip", "x-gzip", "br", "deflate":
		return true
	default:
		return false
	}
}

// decodeContentEncodingOnce 按单个编码解压 data，解压结果超过 maxSize 时报错。
func decodeContentEncodingOnce(data []byte, encoding string, maxSize int64) ([]byte, error) {
	return decodeContentEncodingWithMemory(data, encoding, maxSize, nil)
}

func decodeContentEncodingWithMemory(data []byte, encoding string, maxSize int64, reservation *RequestMemoryReservation) ([]byte, error) {
	var reader io.Reader
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(
			bytes.NewReader(data),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(maxSize)+1),
		)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		reader = dec
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = gr
	case "br":
		reader = brotli.NewReader(bytes.NewReader(data))
	case "deflate":
		// RFC 规定 deflate 是 zlib 封装，但部分客户端会发裸 flate 流，做一次回退。
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			fr := flate.NewReader(bytes.NewReader(data))
			defer fr.Close()
			reader = fr
		} else {
			defer zr.Close()
			reader = zr
		}
	default:
		return nil, fmt.Errorf("unsupported content encoding: %s", encoding)
	}

	decoded, err := readRequestMemoryBounded(reader, maxSize, reservation, 0)
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, &decompressedSizeError{stage: "decoder_window", observed: -1}
		}
		return nil, err
	}
	if int64(len(decoded)) > maxSize {
		return nil, &decompressedSizeError{stage: "decompressed_body", observed: int64(len(decoded))}
	}
	return decoded, nil
}

// RequestBodyDecompressor 按 Content-Encoding 解压入站请求体（zstd/gzip/br/deflate）。
// 新版 Codex 客户端会用 zstd 压缩 /v1/responses 请求体，不解压的话下游按明文 JSON
// 解析会读不到任何字段。解压成功后覆盖缓存的 raw_body 与 c.Request.Body，并移除
// Content-Encoding 头，保证后续流程（含转发上游）看到的都是明文。
// 未知编码保持原样透传，与历史行为一致。
func RequestBodyDecompressor(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := requestBodySizeLimit(c.Request.URL.Path, maxSize)
		encodings := parseContentEncodings(c.GetHeader("Content-Encoding"))
		if len(encodings) == 0 {
			c.Next()
			return
		}
		for _, encoding := range encodings {
			if !isSupportedContentEncoding(encoding) {
				c.Next()
				return
			}
		}

		var body []byte
		if cached, exists := c.Get("raw_body"); exists {
			body, _ = cached.([]byte)
		}
		mem := requestMemoryFromContext(c)
		if body == nil {
			if c.Request.Body == nil || c.Request.Body == http.NoBody {
				c.Next()
				return
			}
			var data []byte
			var err error
			if IsLargeImportUploadPath(c.Request.URL.Path) {
				// 导入端点在鉴权前不缓存 body；解压时也必须计入请求内存预算，
				// 不能让未认证请求用压缩体绕过 BodyCache 的修复。
				reservation, admitted := TryAcquireRequestMemory(0)
				if !admitted {
					rejectRequestMemory(c)
					return
				}
				defer reservation.Release()
				mem = reservation
				data, err = readRequestMemoryBounded(c.Request.Body, limit, reservation, 0)
			} else {
				data, err = io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
			}
			if err != nil {
				if errors.Is(err, ErrRequestMemoryBudget) {
					rejectRequestMemory(c)
					return
				}
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					rejectRequestSize(c, limit, maxBytesErr.Limit+1, "wire_body", true)
					return
				}
				c.JSON(http.StatusBadRequest, gin.H{
					"error": gin.H{
						"message": "读取请求体失败",
						"type":    "invalid_request_error",
					},
				})
				c.Abort()
				return
			}
			body = data
		}
		if int64(len(body)) > limit {
			rejectRequestSize(c, limit, int64(len(body)), "wire_body", true)
			return
		}
		if len(body) == 0 {
			c.Next()
			return
		}

		decoded := body
		// 多个编码按施加顺序列出，解码逆序进行。
		for i := len(encodings) - 1; i >= 0; i-- {
			var err error
			decoded, err = decodeContentEncodingWithMemory(decoded, encodings[i], limit, mem)
			if err != nil {
				if errors.Is(err, ErrRequestMemoryBudget) {
					rejectRequestMemory(c)
					return
				}
				if errors.Is(err, errDecompressedBodyTooLarge) {
					var sizeErr *decompressedSizeError
					if errors.As(err, &sizeErr) {
						rejectRequestSize(c, limit, sizeErr.observed, sizeErr.stage, sizeErr.observed >= 0)
					} else {
						rejectRequestSize(c, limit, -1, "decompressed_body", false)
					}
				} else {
					c.JSON(http.StatusBadRequest, gin.H{
						"error": gin.H{
							"message": fmt.Sprintf("请求体解压失败（Content-Encoding: %s）", encodings[i]),
							"type":    "invalid_request_error",
							"code":    "invalid_request_body_encoding",
						},
					})
				}
				c.Abort()
				return
			}
		}

		c.Set("raw_body", decoded)
		c.Request.Body = io.NopCloser(bytes.NewReader(decoded))
		c.Request.ContentLength = int64(len(decoded))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(decoded)))
		c.Request.Header.Del("Content-Encoding")
		c.Next()
	}
}
