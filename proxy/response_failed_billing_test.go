package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 流式请求在首 token 之前收到 response.failed 时,应中止 SSE 转发、由循环外按真实
// HTTP 错误码返回,而不是把失败包成 200 + [DONE](后者会被上游中转/计费方误判为
// 成功流并按预估 input token 计费)。见 shouldReturnHTTPErrorForResponseFailed。
func TestShouldReturnHTTPErrorForResponseFailed(t *testing.T) {
	cases := []struct {
		name         string
		eventType    string
		ttftRecorded bool
		wroteAnyBody bool
		clientGone   bool
		want         bool
	}{
		{"首 token 前的 response.failed 应返回错误码", "response.failed", false, false, false, true},
		{"response.completed 不拦截", "response.completed", false, false, false, false},
		{"已产出首 token 不拦截(维持流式收尾)", "response.failed", true, false, false, false},
		{"已向下游写过 body 不拦截(200 已发出)", "response.failed", false, true, false, false},
		{"客户端已断开不拦截(继续读上游取 usage)", "response.failed", false, false, true, false},
		{"普通内容事件不拦截", "response.output_text.delta", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReturnHTTPErrorForResponseFailed(tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone)
			if got != tc.want {
				t.Errorf("shouldReturnHTTPErrorForResponseFailed(%q, ttft=%v, wrote=%v, gone=%v) = %v, want %v",
					tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone, got, tc.want)
			}
		})
	}
}

// streamEpilogue 复刻 Responses/ChatCompletions 流式路径的收尾时序:
// 逐事件走 defer/拦截逻辑 → 仅在写过 body 时收尾 flush → 拦截命中时循环外 c.JSON。
// 用真实 HTTP 连接验证,因为致命点在 gin 层:flusher.Flush 会先 WriteHeaderNow
// 提交 200,零写入时提前 flush 会让后续 c.JSON(4xx) 的状态码永远无法送达下游。
func streamEpilogue(t *testing.T, c *gin.Context, events [][]byte) {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer 未实现 http.Flusher")
		return
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	abortedForHTTPError := false
	gotTerminal := false

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.output_text.delta" {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
		wrote, err := writeDeferredSSEData(streamWriter, &pending, data, shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "context_length_exceeded", "type": "upstream_error"},
		})
	}
}

func TestStreamFirstTokenFailureWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	failedEvent := []byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input too long"}}}`)
	cases := []struct {
		name         string
		events       [][]byte
		wantStatus   int
		wantJSONType bool
		wantBodyHas  string
	}{
		{
			name: "首 token 前 response.failed → 下游收到真实 4xx JSON",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.in_progress"}`),
				failedEvent,
			},
			wantStatus:   http.StatusBadRequest,
			wantJSONType: true,
			wantBodyHas:  "context_length_exceeded",
		},
		{
			name: "已产出首 token → 维持 200 SSE 流式收尾",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
				failedEvent,
			},
			wantStatus:   http.StatusOK,
			wantJSONType: false,
			wantBodyHas:  "response.output_text.delta",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/stream", func(c *gin.Context) {
				streamEpilogue(t, c, tc.events)
			})
			srv := httptest.NewServer(router)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/stream")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", resp.StatusCode, tc.wantStatus, body)
			}
			ct := resp.Header.Get("Content-Type")
			if tc.wantJSONType && !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !tc.wantJSONType && !strings.Contains(ct, "text/event-stream") {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}
			if !strings.Contains(string(body), tc.wantBodyHas) {
				t.Errorf("body = %q, want contains %q", body, tc.wantBodyHas)
			}
		})
	}
}
