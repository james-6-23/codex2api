package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// issue #473：首包后断流必须给下游一个可编程识别的失败终态，
// 而不是静默 EOF 的"假 200"。

func TestWriteResponsesStreamBreakEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writer := newStreamFlushWriter(c.Writer, nil)
	if err := writeResponsesStreamBreakEvent(writer); err != nil {
		t.Fatalf("writeResponsesStreamBreakEvent: %v", err)
	}

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "data: ") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("unexpected SSE frame: %q", body)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	if !gjson.Valid(data) {
		t.Fatalf("stream break event is not valid JSON: %q", data)
	}
	if got := gjson.Get(data, "type").String(); got != "response.failed" {
		t.Fatalf("type = %q, want response.failed", got)
	}
	if got := gjson.Get(data, "response.status").String(); got != "failed" {
		t.Fatalf("response.status = %q, want failed", got)
	}
	if got := gjson.Get(data, "response.created_at").Int(); got <= 0 {
		t.Fatalf("response.created_at = %d, want a positive Unix timestamp", got)
	}
	if got := gjson.Get(data, "response.error.code").String(); got != ErrorCodeUpstreamStreamBreak {
		t.Fatalf("response.error.code = %q, want %q", got, ErrorCodeUpstreamStreamBreak)
	}
}

func TestWriteGrokNativeResponsesStreamBreakCarriesCreatedAt(t *testing.T) {
	var out bytes.Buffer
	before := time.Now().Unix()
	if err := writeGrokNativeStreamBreakTo(&out, GrokProtocolResponses, 0); err != nil {
		t.Fatalf("writeGrokNativeStreamBreakTo: %v", err)
	}
	after := time.Now().Unix()
	payload := strings.TrimSpace(strings.TrimPrefix(out.String(), "data: "))
	if got := gjson.Get(payload, "type").String(); got != "response.failed" {
		t.Fatalf("type = %q, want response.failed; payload=%s", got, payload)
	}
	if got := gjson.Get(payload, "response.created_at").Int(); got < before || got > after {
		t.Fatalf("response.created_at = %d, want helper fallback in [%d, %d]", got, before, after)
	}
}

func TestForwardGrokNativeResponsesStreamBreakReusesCreatedAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	const createdAt int64 = 1712345678
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_fixed","object":"response","created_at":1712345678,"status":"in_progress","model":"grok"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"visible"}`,
		``,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstream)),
	}

	_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolResponses, true, time.Now(), nil)

	if !wrote || outcome.logStatusCode != logStatusUpstreamStreamBreak {
		t.Fatalf("outcome/wrote = %#v %v, want visible stream break", outcome, wrote)
	}
	if !strings.Contains(recorder.Body.String(), `"delta":"visible"`) {
		t.Fatalf("visible frame was not forwarded: %s", recorder.Body.String())
	}
	var createdEventAt, failedEventAt int64
	if err := ReadSSEStream(strings.NewReader(recorder.Body.String()), func(data []byte) bool {
		switch gjson.GetBytes(data, "type").String() {
		case "response.created":
			createdEventAt = gjson.GetBytes(data, "response.created_at").Int()
		case "response.failed":
			failedEventAt = gjson.GetBytes(data, "response.created_at").Int()
		}
		return true
	}); err != nil {
		t.Fatalf("ReadSSEStream: %v", err)
	}
	if createdEventAt != createdAt || failedEventAt != createdAt {
		t.Fatalf("created_at = created:%d failed:%d, want both %d; stream=%s", createdEventAt, failedEventAt, createdAt, recorder.Body.String())
	}
}

func TestWriteChatCompletionsStreamBreakEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writer := newStreamFlushWriter(c.Writer, nil)
	if err := writeChatCompletionsStreamBreakEvent(writer); err != nil {
		t.Fatalf("writeChatCompletionsStreamBreakEvent: %v", err)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("stream break must not append [DONE]: %q", body)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	if !gjson.Valid(data) {
		t.Fatalf("stream break chunk is not valid JSON: %q", data)
	}
	if got := gjson.Get(data, "error.code").String(); got != ErrorCodeUpstreamStreamBreak {
		t.Fatalf("error.code = %q, want %q", got, ErrorCodeUpstreamStreamBreak)
	}
	if got := gjson.Get(data, "error.type").String(); got != ErrorTypeUpstreamError {
		t.Fatalf("error.type = %q, want %q", got, ErrorTypeUpstreamError)
	}
}

func TestShouldWriteStreamBreakEvent(t *testing.T) {
	errBoom := errors.New("boom")
	cases := []struct {
		name         string
		gotTerminal  bool
		wroteAnyBody bool
		ctxErr       error
		writeErr     error
		want         bool
	}{
		{"首包后断流", false, true, nil, nil, true},
		{"正常终态不触发", true, true, nil, nil, false},
		{"零写入交给透明重试/502 出口", false, false, nil, nil, false},
		{"客户端已断开不写", false, true, errBoom, nil, false},
		{"下游写失败不写", false, true, nil, errBoom, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldWriteStreamBreakEvent(tc.gotTerminal, tc.wroteAnyBody, tc.ctxErr, tc.writeErr); got != tc.want {
				t.Fatalf("shouldWriteStreamBreakEvent(%v,%v,%v,%v) = %v, want %v",
					tc.gotTerminal, tc.wroteAnyBody, tc.ctxErr, tc.writeErr, got, tc.want)
			}
		})
	}
}
