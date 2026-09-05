package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/tidwall/gjson"
)

const claudeTestBodyLimit = 64 << 10

type claudeTestHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type claudeTestCacheCreation struct {
	FiveMinute *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	OneHour    *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
}

type claudeTestUsage struct {
	InputTokens         *int64                   `json:"input_tokens,omitempty"`
	OutputTokens        *int64                   `json:"output_tokens,omitempty"`
	CacheReadTokens     *int64                   `json:"cache_read_input_tokens,omitempty"`
	CacheCreationTokens *int64                   `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation       *claudeTestCacheCreation `json:"cache_creation,omitempty"`
}

// Diagnostics describe this request only; missing observations stay absent.
type claudeTestDiagnostics struct {
	HTTPStatus      int                `json:"http_status,omitempty"`
	DurationMS      *int64             `json:"duration_ms,omitempty"`
	HeadersMS       *int64             `json:"headers_ms,omitempty"`
	FirstContentMS  *int64             `json:"first_content_ms,omitempty"`
	Model           string             `json:"model"`
	ResponseModel   string             `json:"response_model,omitempty"`
	FingerprintMode string             `json:"fingerprint_mode,omitempty"`
	RequestID       string             `json:"request_id,omitempty"`
	OrganizationID  string             `json:"organization_id,omitempty"`
	MessageID       string             `json:"message_id,omitempty"`
	StopReason      string             `json:"stop_reason,omitempty"`
	ErrorType       string             `json:"error_type,omitempty"`
	Usage           *claudeTestUsage   `json:"usage,omitempty"`
	ResponseHeaders []claudeTestHeader `json:"response_headers,omitempty"`
	ResponseBody    string             `json:"response_body,omitempty"`
	BodyTruncated   bool               `json:"body_truncated,omitempty"`
}

type claudeTestCapture struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *claudeTestCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(0, b.limit-b.Len())
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p) // bytes.Buffer.Write cannot fail.
	return n, nil            // Never truncate the stream consumed by the existing parser.
}

var claudeTestProxyCredentials = regexp.MustCompile(`(?i)((?:https?|socks5h?)://)[^\s/]+@`)

func sanitizeClaudeTestText(text, accessToken string) string {
	if accessToken != "" {
		text = strings.ReplaceAll(text, accessToken, "[REDACTED]")
	}
	text = claudeTestProxyCredentials.ReplaceAllString(text, "${1}[REDACTED]@")
	return promptfilter.RedactSensitive(text)
}

type claudeTestRecorder struct {
	details     *claudeTestDiagnostics
	start       time.Time
	accessToken string
	capture     claudeTestCapture
}

func newClaudeTestRecorder(resp *http.Response, model, mode, accessToken string, start time.Time) *claudeTestRecorder {
	r := &claudeTestRecorder{
		details: &claudeTestDiagnostics{Model: model, FingerprintMode: mode},
		start:   start, accessToken: accessToken,
		// Keep enough lookahead to redact a token crossing the preview boundary.
		capture: claudeTestCapture{limit: claudeTestBodyLimit + len(accessToken)},
	}
	if resp == nil {
		return r
	}
	r.details.HTTPStatus = resp.StatusCode
	ms := max(int64(0), time.Since(start).Milliseconds())
	r.details.HeadersMS = &ms
	r.details.RequestID = resp.Header.Get("request-id")
	if r.details.RequestID == "" {
		r.details.RequestID = resp.Header.Get("x-request-id")
	}
	r.details.OrganizationID = resp.Header.Get("anthropic-organization-id")
	keys := make([]string, 0, len(resp.Header))
	for key := range resp.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := strings.ToLower(key)
		keep := strings.HasPrefix(name, "anthropic-ratelimit-")
		switch name {
		case "request-id", "x-request-id", "anthropic-organization-id", "retry-after", "content-type", "date", "server-timing", "cf-ray", "x-should-retry":
			keep = true
		}
		if !keep {
			continue
		}
		for _, value := range resp.Header[key] {
			if len(r.details.ResponseHeaders) >= 64 {
				break
			}
			r.details.ResponseHeaders = append(r.details.ResponseHeaders, claudeTestHeader{Name: name, Value: r.safeValue(value)})
		}
	}
	r.details.RequestID = r.safeValue(r.details.RequestID)
	r.details.OrganizationID = r.safeValue(r.details.OrganizationID)
	if resp.Body != nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &r.capture), resp.Body}
	}
	return r
}

func (r *claudeTestRecorder) safeValue(value string) string {
	return truncate(sanitizeClaudeTestText(value, r.accessToken), 2048)
}

func (r *claudeTestRecorder) contentReceived() {
	if r.details.FirstContentMS == nil {
		ms := max(int64(0), time.Since(r.start).Milliseconds())
		r.details.FirstContentMS = &ms
	}
}

func (r *claudeTestRecorder) observe(data []byte) {
	event := gjson.ParseBytes(data)
	message := event
	if event.Get("type").String() == "message_start" {
		message = event.Get("message")
	}
	if model := message.Get("model").String(); model != "" {
		r.details.ResponseModel = r.safeValue(model)
	}
	if id := message.Get("id").String(); id != "" {
		r.details.MessageID = r.safeValue(id)
	}
	if reason := message.Get("stop_reason").String(); reason != "" {
		r.details.StopReason = r.safeValue(reason)
	}
	if reason := event.Get("delta.stop_reason").String(); reason != "" {
		r.details.StopReason = r.safeValue(reason)
	}
	if typ := event.Get("error.type").String(); typ != "" {
		r.details.ErrorType = r.safeValue(typ)
	}
	if event.Get("delta.text").String() != "" || event.Get("delta.thinking").String() != "" {
		r.contentReceived()
	}
	usage := message.Get("usage")
	if !usage.IsObject() {
		return
	}
	if r.details.Usage == nil {
		r.details.Usage = &claudeTestUsage{}
	}
	u := r.details.Usage
	updateClaudeTestCount(&u.InputTokens, usage.Get("input_tokens"))
	// Anthropic message_delta usage is cumulative, not an incremental delta.
	updateClaudeTestCount(&u.OutputTokens, usage.Get("output_tokens"))
	updateClaudeTestCount(&u.CacheReadTokens, usage.Get("cache_read_input_tokens"))
	updateClaudeTestCount(&u.CacheCreationTokens, usage.Get("cache_creation_input_tokens"))
	if creation := usage.Get("cache_creation"); creation.IsObject() {
		if u.CacheCreation == nil {
			u.CacheCreation = &claudeTestCacheCreation{}
		}
		updateClaudeTestCount(&u.CacheCreation.FiveMinute, creation.Get("ephemeral_5m_input_tokens"))
		updateClaudeTestCount(&u.CacheCreation.OneHour, creation.Get("ephemeral_1h_input_tokens"))
	}
}

func updateClaudeTestCount(target **int64, value gjson.Result) {
	if value.Type != gjson.Number {
		return
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	if err == nil && n >= 0 {
		*target = &n
	}
}

func (r *claudeTestRecorder) finish() *claudeTestDiagnostics {
	ms := max(int64(0), time.Since(r.start).Milliseconds())
	r.details.DurationMS = &ms
	body := sanitizeClaudeTestText(r.capture.String(), r.accessToken)
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	r.details.BodyTruncated = r.capture.truncated || len(body) > claudeTestBodyLimit
	if len(body) > claudeTestBodyLimit {
		body = strings.ToValidUTF8(body[:claudeTestBodyLimit], "")
	}
	r.details.ResponseBody = body
	return r.details
}
