package wsrelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const handshakeErrorBodyLimit = 4 << 10 // 4 KiB

// HandshakeHTTPError 是握手阶段收到上游 HTTP 错误响应时的结构化错误：
// 除人类可读的错误文本外，保留原始状态码、响应头与规范化后的 JSON body，
// 供上层把鉴权失败（401）还原成真实状态码处理，而不是笼统的 transport 错误。
type HandshakeHTTPError struct {
	StatusCode int
	Header     http.Header
	// Body 是上游错误 body 的规范化文本（JSON 紧凑化、非 JSON 空白折叠），
	// 可直接用 gjson 解析 error.code / error.message。
	Body string
	msg  string
}

func (e *HandshakeHTTPError) Error() string { return e.msg }

// handshakeUnauthorizedHTTPResponse 把握手阶段的 401 转换成携带真实状态码与
// 上游原始错误体的 HTTP 响应，让调用方复用既有非 2xx 分支：usage log 记 401、
// applyCooldownForModel 的 unauthorized 冷却与 missing_scope 特判、换号重试。
// 其他状态码保持原有 transport 错误语义（如 503 握手限流仍走粘滞重试），
// 不在此转换。
func handshakeUnauthorizedHTTPResponse(err error) (*http.Response, bool) {
	var hs *HandshakeHTTPError
	if !errors.As(err, &hs) || hs.StatusCode != http.StatusUnauthorized {
		return nil, false
	}
	header := http.Header{}
	if hs.Header != nil {
		header = hs.Header.Clone()
	}
	return &http.Response{
		StatusCode: hs.StatusCode,
		Status:     fmt.Sprintf("%d %s", hs.StatusCode, http.StatusText(hs.StatusCode)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(hs.Body)),
	}, true
}

// formatDialHandshakeError 把 Dial 失败包装成可诊断的错误。
// gorilla/websocket 在 bad handshake 时仍返回 *http.Response，并已预读最多 1KB body；
// 这里补充 HTTP 状态码、关键响应头与上游 body（JSON 原样附带，便于前端 pretty-print）。
func formatDialHandshakeError(err error, resp *http.Response) error {
	if err == nil {
		return nil
	}
	if resp == nil {
		return fmt.Errorf("websocket handshake failed: %w", err)
	}

	status := resp.StatusCode
	statusText := strings.TrimSpace(http.StatusText(status))
	body := readHTTPErrorBody(resp)
	headers := handshakeErrorHeaderSnippet(resp.Header)

	var b strings.Builder
	b.WriteString("websocket handshake failed: ")
	b.WriteString(err.Error())
	if status > 0 {
		b.WriteString(fmt.Sprintf(" (HTTP %d", status))
		if statusText != "" {
			b.WriteString(" ")
			b.WriteString(statusText)
		}
		b.WriteString(")")
	}
	if headers != "" {
		b.WriteString("; ")
		b.WriteString(headers)
	}
	if body != "" {
		// 前缀以冒号结尾，前端 formatTestErrorMessage 会把后续 JSON 拆出并 pretty-print。
		b.WriteString(":\n")
		b.WriteString(body)
	}
	return &HandshakeHTTPError{
		StatusCode: status,
		Header:     resp.Header.Clone(),
		Body:       body,
		msg:        b.String(),
	}
}

// formatFailedHandshakeHTTPBody 用于握手 HTTP 响应状态非 2xx/101 时构造 body 文本。
func formatFailedHandshakeHTTPBody(statusCode int, resp *http.Response) string {
	body := readHTTPErrorBody(resp)
	headers := ""
	if resp != nil {
		headers = handshakeErrorHeaderSnippet(resp.Header)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("websocket handshake failed: HTTP %d", statusCode))
	if text := strings.TrimSpace(http.StatusText(statusCode)); text != "" {
		b.WriteString(" ")
		b.WriteString(text)
	}
	if headers != "" {
		b.WriteString("; ")
		b.WriteString(headers)
	}
	if body != "" {
		b.WriteString(":\n")
		b.WriteString(body)
	}
	return b.String()
}

func handshakeErrorHeaderSnippet(h http.Header) string {
	if h == nil {
		return ""
	}
	var parts []string
	for _, key := range []string{
		"Cf-Ray",
		"X-Request-Id",
		"X-Openai-Request-Id",
		"Openai-Organization",
		"Www-Authenticate",
		"X-Error-Code",
		"X-Error-Message",
	} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			parts = append(parts, key+"="+truncateRunes(v, 120))
		}
	}
	return strings.Join(parts, ", ")
}

// readHTTPErrorBody 读取上游错误 body；JSON 则规范为紧凑合法 JSON（前端再 pretty-print），
// 非 JSON 做空白折叠。会回填 resp.Body 供后续读取。
func readHTTPErrorBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, handshakeErrorBodyLimit))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}

	// 尽量保留上游 JSON 结构：解码再紧凑编码，去掉多余空白但不丢字段。
	if json.Valid([]byte(text)) {
		var v any
		if err := json.Unmarshal([]byte(text), &v); err == nil {
			if compact, err := json.Marshal(v); err == nil {
				return truncateRunes(string(compact), 2000)
			}
		}
		return truncateRunes(text, 2000)
	}

	// 纯文本 / HTML 粗截断。
	text = strings.Join(strings.Fields(text), " ")
	return truncateRunes(text, 500)
}

func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	b.Grow(max*3 + 1)
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	b.WriteString("…")
	return b.String()
}
