package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUpstreamTraceAttemptIsolation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	account := &auth.Account{DBID: 1, UpstreamRequestIDHeader: "Vendor-Trace"}
	first := beginUpstreamTrace(c.Request.Context(), account, "http://user:password@proxy.invalid:1080", false)
	second := beginUpstreamTrace(c.Request.Context(), account, "", false)
	second(&http.Response{Header: http.Header{"Vendor-Trace": {"second"}, "X-Request-Id": {"not-selected"}}})
	first(&http.Response{Header: http.Header{"Vendor-Trace": {"late-first"}}})
	input := &database.UsageLogInput{AccountID: 1}
	populateUpstreamTrace(c, input)
	if input.RequestID == "" || input.UpstreamRequestID != "second" || input.UpstreamProxyName != "direct/no_proxy" {
		t.Fatalf("trace = %+v", input)
	}
	before := input.RequestID
	resetUpstreamRequestTrace(c)
	ws := beginUpstreamTrace(c.Request.Context(), account, "", true)
	ws(&http.Response{Header: http.Header{"Vendor-Trace": {"handshake-not-turn"}}})
	input = &database.UsageLogInput{AccountID: 1}
	populateUpstreamTrace(c, input)
	if input.RequestID == before || input.UpstreamRequestID != "" || input.UpstreamProxyName != "unknown" {
		t.Fatalf("WS trace = %+v", input)
	}
}

func TestUpstreamTraceHTTPFailureAndReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", strings.Repeat("界", 150))
		w.WriteHeader(429)
	}))
	defer server.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, server.URL, nil)
	resp, err := doTracedUpstreamRequest(server.Client(), req, &auth.Account{DBID: 2}, "http://secret:pass@proxy.invalid")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	input := &database.UsageLogInput{AccountID: 2}
	populateUpstreamTrace(c, input)
	if input.UpstreamRequestID == "" || len([]rune(input.UpstreamRequestID)) > 128 || input.UpstreamProxyName != "unmanaged" {
		t.Fatalf("failure trace = %+v", input)
	}
	resetUpstreamAttemptTrace(c.Request.Context())
	input = &database.UsageLogInput{AccountID: 2}
	populateUpstreamTrace(c, input)
	if input.UpstreamRequestID != "" || input.UpstreamProxyName != "" {
		t.Fatal("local failure inherited prior attempt")
	}
	beginUpstreamTrace(context.Background(), &auth.Account{}, "", false)(resp)
}
