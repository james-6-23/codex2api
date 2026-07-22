package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/tidwall/gjson"
)

// grokTurnIndex 估算会话内轮次序号（对齐官方 CLI 的 x-grok-turn-idx）。
// 无状态：按 input[] 里的 user 消息数计（每个 user 回合 +1），至少为 1。
// 与账号无关、对重试稳定（剥离密文不影响 user 消息计数）。
func grokTurnIndex(body []byte) int {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return 1
	}
	count := 0
	for _, item := range input.Array() {
		if item.Get("type").String() == "message" && strings.EqualFold(item.Get("role").String(), "user") {
			count++
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// grokBodyHasBlobs 判断请求体是否带有可能触发上游解码失败的外来密文
// （reasoning encrypted_content 或 compaction 项）。用于决定是否值得在 400 后重试。
func grokBodyHasBlobs(body []byte) bool {
	return bytes.Contains(body, []byte("encrypted_content")) || bytes.Contains(body, []byte(`"compaction"`))
}

// grokIsBlobDecodeFailure 判断上游 400 是否为密文解码失败（可通过剥离密文重试恢复）。
func grokIsBlobDecodeFailure(errBody []byte) bool {
	lower := bytes.ToLower(errBody)
	for _, marker := range [][]byte{
		[]byte("compaction blob"),
		[]byte("could not decrypt"),
		[]byte("could not decode the compaction"),
		[]byte("encrypted_content"),
	} {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// decodeGrokResponseEncoding 在手动声明 Accept-Encoding 后接管响应解压。
// SSE 流上游不压缩（无 Content-Encoding），此处只处理非流式的压缩响应（错误/billing/非流式补全）：
// 整体缓冲后按 Content-Encoding 逆序解码。event-stream 一律跳过，避免缓冲流式响应。
func decodeGrokResponseEncoding(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding == "" {
		return
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream") {
		return
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return
	}
	decoded, derr := decodeContentEncoding(data, encoding)
	if derr != nil {
		// 解码失败时原样返回，避免把问题放大成空响应。
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(decoded))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = int64(len(decoded))
	resp.Uncompressed = true
}

// decodeContentEncoding 按 Content-Encoding（可能是逗号分隔的多重编码）逆序解压 gzip/br/deflate。
func decodeContentEncoding(data []byte, encoding string) ([]byte, error) {
	encs := strings.Split(encoding, ",")
	for i := len(encs) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(encs[i]))
		switch enc {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			gr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			out, err := io.ReadAll(gr)
			_ = gr.Close()
			if err != nil {
				return nil, err
			}
			data = out
		case "br":
			out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
			if err != nil {
				return nil, err
			}
			data = out
		case "deflate":
			// HTTP "deflate" 规范上是 zlib 包装，但不少服务端发裸 deflate；先 zlib 后回退。
			var reader io.ReadCloser
			if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
				reader = zr
			} else {
				reader = flate.NewReader(bytes.NewReader(data))
			}
			out, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				return nil, err
			}
			data = out
		default:
			return nil, fmt.Errorf("unsupported content-encoding %q", enc)
		}
	}
	return data, nil
}
