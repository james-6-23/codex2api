package proxy

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestSendDownstreamGuardTerminalHTTPTryLockContention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req
	c.Writer.WriteHeaderNow()

	var downstreamMu sync.Mutex
	clientGone := false
	downstreamMu.Lock()

	lockHoldReleased := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		downstreamMu.Unlock()
		close(lockHoldReleased)
	}()

	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil {
		t.Fatalf("expected non-nil error when lock is contended")
	}
	if !clientGone {
		t.Fatalf("expected clientGone == true on lock contention")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("expected no body written on lock contention, got %s", w.Body.String())
	}
	<-lockHoldReleased
}

type fullMockGinResponseWriter struct {
	http.ResponseWriter
	writeCalled bool
}

func (m *fullMockGinResponseWriter) Status() int                       { return http.StatusOK }
func (m *fullMockGinResponseWriter) Size() int                         { return 0 }
func (m *fullMockGinResponseWriter) WriteString(s string) (int, error) { return m.Write([]byte(s)) }
func (m *fullMockGinResponseWriter) Written() bool                     { return true }
func (m *fullMockGinResponseWriter) WriteHeaderNow()                   {}
func (m *fullMockGinResponseWriter) Pusher() http.Pusher               { return nil }
func (m *fullMockGinResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
func (m *fullMockGinResponseWriter) Flush()                      {}
func (m *fullMockGinResponseWriter) CloseNotify() <-chan bool    { return nil }
func (m *fullMockGinResponseWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }
func (m *fullMockGinResponseWriter) Write(b []byte) (int, error) {
	m.writeCalled = true
	return m.ResponseWriter.Write(b)
}

func TestSendDownstreamGuardTerminalHTTPErrNotSupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	mockWriter := &fullMockGinResponseWriter{ResponseWriter: rec}
	c := &gin.Context{Writer: mockWriter}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = req

	var downstreamMu sync.Mutex
	clientGone := false
	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err == nil {
		t.Fatalf("expected error when deadline unsupported")
	}
	if !clientGone {
		t.Fatalf("expected clientGone == true")
	}
	if mockWriter.writeCalled {
		t.Fatalf("expected no write when deadline unsupported")
	}
}

type deadlineSpyResponseWriter struct {
	gin.ResponseWriter
	deadlines []time.Time
}

func (s *deadlineSpyResponseWriter) SetWriteDeadline(t time.Time) error {
	s.deadlines = append(s.deadlines, t)
	return nil
}

func TestSendDownstreamGuardTerminalHTTPDeadlineSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.WriteHeaderNow()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	spy := &deadlineSpyResponseWriter{ResponseWriter: c.Writer}
	c.Writer = spy

	var downstreamMu sync.Mutex
	clientGone := false
	err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clientGone {
		t.Fatalf("expected clientGone == true")
	}
	if len(spy.deadlines) != 2 || spy.deadlines[0].IsZero() || !spy.deadlines[1].IsZero() {
		t.Fatalf("expected 2 deadline calls (set+reset), got %+v", spy.deadlines)
	}
	if !strings.Contains(rec.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("body missing guard code: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "repetition") {
		t.Fatalf("body leaked internal reason: %s", rec.Body.String())
	}
}

func TestSendDownstreamGuardTerminalHTTPUncommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	var downstreamMu sync.Mutex
	clientGone := false
	if err := SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, GuardTriggeredPublicCode) || strings.Contains(body, "repetition") {
		t.Fatalf("invalid or leaked body: %s", body)
	}
}

func TestFormatGuardTerminalEnvelope(t *testing.T) {
	respData, status, err := FormatGuardTerminalEnvelope("responses", "")
	if err != nil || status != http.StatusOK || !strings.Contains(string(respData), `"type":"response.failed"`) || strings.Contains(string(respData), "response.completed") {
		t.Fatalf("responses envelope invalid: %s", string(respData))
	}
	chatData, _, err := FormatGuardTerminalEnvelope("chat", "")
	if err != nil || !strings.Contains(string(chatData), GuardTriggeredPublicCode) || strings.Contains(string(chatData), "[DONE]") {
		t.Fatalf("chat envelope invalid: %s", string(chatData))
	}
	anthropicData, _, err := FormatGuardTerminalEnvelope("anthropic", "")
	if err != nil || !strings.Contains(string(anthropicData), "event: error\n") || strings.Contains(string(anthropicData), "message_stop") {
		t.Fatalf("anthropic envelope invalid: %s", string(anthropicData))
	}
	geminiData, _, err := FormatGuardTerminalEnvelope("gemini", "")
	if err != nil || strings.HasPrefix(string(geminiData), "data:") {
		t.Fatalf("gemini envelope must be bare JSON")
	}
	var gJSON struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(geminiData, &gJSON); err != nil || gJSON.Error.Code != 502 || gJSON.Error.Status != "UNAVAILABLE" {
		t.Fatalf("gemini envelope invalid: %+v", gJSON)
	}
	wsData, _, err := FormatGuardTerminalEnvelope("websocket", "stream_xyz")
	if err != nil || !strings.Contains(string(wsData), `"stream_id":"stream_xyz"`) || !strings.Contains(string(wsData), GuardTriggeredPublicCode) {
		t.Fatalf("ws envelope invalid: %s", string(wsData))
	}
	if strings.Contains(string(respData), "repetition") || strings.Contains(string(chatData), "repetition") || strings.Contains(string(anthropicData), "repetition") {
		t.Fatalf("envelope leaked internal reason")
	}
}

func TestSendDownstreamGuardTerminalWSSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := SendDownstreamGuardTerminalWS(conn, "turn_001", "repetition"); err != nil {
			return
		}
	}))
	defer server.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer clientConn.Close()

	_, msg, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	parsed := gjson.ParseBytes(msg)
	if parsed.Get("type").String() != "error" || parsed.Get("stream_id").String() != "turn_001" || parsed.Get("error.code").String() != GuardTriggeredPublicCode {
		t.Fatalf("unexpected ws frame: %s", string(msg))
	}
	if strings.Contains(string(msg), "repetition") {
		t.Fatalf("ws frame leaked internal reason: %s", string(msg))
	}
}

func TestApplyDownstreamWriteDeadline(t *testing.T) {
	if err := ApplyDownstreamWriteDeadline(nil, time.Second); err == nil {
		t.Fatalf("expected error for nil writer")
	}
	rec := httptest.NewRecorder()
	if err := ApplyDownstreamWriteDeadline(rec, time.Second); err == nil {
		t.Fatalf("expected ErrNotSupported for plain recorder, got nil")
	}
	spy := &deadlineSpyResponseWriter{ResponseWriter: newGinTestWriter(rec)}
	if err := ApplyDownstreamWriteDeadline(spy, GuardWriteDeadlineTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ApplyDownstreamWriteDeadline(spy, 0); err != nil {
		t.Fatalf("unexpected reset error: %v", err)
	}
	if len(spy.deadlines) != 2 || spy.deadlines[0].IsZero() || !spy.deadlines[1].IsZero() {
		t.Fatalf("unexpected deadlines: %+v", spy.deadlines)
	}
}

// newGinTestWriter 返回一个实现了 gin.ResponseWriter 的测试 writer，仅用于
// deadline spy 的嵌入目标。
func newGinTestWriter(rec *httptest.ResponseRecorder) gin.ResponseWriter {
	c, _ := gin.CreateTestContext(rec)
	return c.Writer
}
