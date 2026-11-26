package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestInvalidInputImageResponsesReturnsWithoutReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, upstreamStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/upstreamStream=%t", stream, upstreamStream), func(t *testing.T) {
				previous := CurrentRuntimeSettings()
				t.Cleanup(func() { ApplyRuntimeSettings(previous) })
				settings := previous
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
				ApplyRuntimeSettings(settings)
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if upstreamStream && !stream {
						// Non-streaming relays return JSON, including a failed
						// protocol response carried by HTTP 200.
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_invalid","object":"response","status":"failed","error":`+invalidInputImageTestProblem+`}`)
					} else if upstreamStream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.failed\ndata: "+`{"type":"response.failed","response":{"status":"failed","error":`+invalidInputImageTestProblem+`}}`+"\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":`+invalidInputImageTestProblem+`}`)
					}
				}))
				t.Cleanup(upstream.Close)
				store := newOpenAIResponsesRelayStore(upstream.URL)
				t.Cleanup(store.Stop)
				handler := NewHandler(store, nil, nil, nil)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(fmt.Sprintf(`{"model":"gpt-4.1-direct","input":"synthetic image rejection","stream":%t}`, stream))).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				handler.Responses(c)
				if calls.Load() != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls.Load())
				}
				body := recorder.Body.String()
				if !strings.Contains(body, "invalid_value") || !strings.Contains(body, "does not represent a valid image") || strings.Contains(body, "response.completed") {
					t.Fatalf("terminal image error lost or masked: status=%d body=%s", recorder.Code, body)
				}
				if !upstreamStream && recorder.Code != http.StatusBadRequest {
					t.Fatalf("upstream HTTP 400 became %d", recorder.Code)
				}
				if ctx.Err() != nil {
					t.Fatal("terminal response waited for timeout")
				}
			})
		}
	}
}
