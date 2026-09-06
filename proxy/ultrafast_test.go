package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestUltrafastRequestAndUsageTiers(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":"hello","service_tier":"ultrafast"}`)
	prepared, _ := PrepareResponsesBody(raw)
	ws, _ := PrepareResponsesWebSocketBody(raw)
	chat, err := TranslateRequest([]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}],"serviceTier":"ultrafast"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{prepared, ws, chat, PrepareOpenAIResponsesBody(raw)} {
		if got := gjson.GetBytes(body, "service_tier").String(); got != "ultrafast" {
			t.Fatalf("Ultrafast lost during request preparation: %s", body)
		}
	}
	for _, tc := range []struct{ actual, requested, billing string }{
		{"ultrafast", "ultrafast", "ultrafast"},
		{"default", "ultrafast", "default"},
		{"ultrafast", "default", "default"},
		{"", "ultrafast", "ultrafast"},
	} {
		got := resolveUsageServiceTiers(tc.actual, tc.requested)
		if got.RequestedServiceTier != tc.requested || got.ActualServiceTier != tc.actual || got.BillingServiceTier != tc.billing {
			t.Errorf("tiers = %+v for %+v", got, tc)
		}
	}
	fast := database.CalculateCost(1000, 500, 200, "gpt-5.6-sol", "priority")
	if got := database.CalculateCost(1000, 500, 200, "gpt-5.6-sol", "ultrafast"); got != fast || got <= 0 {
		t.Fatalf("Ultrafast billing = %v, want Fast policy %v", got, fast)
	}
}

func TestUltrafastExecutorHTTPAndWebSocket(t *testing.T) {
	previousResin, previousWS := resinCfg.Load(), WebsocketExecuteFunc
	t.Cleanup(func() { resinCfg.Store(previousResin); WebsocketExecuteFunc = previousWS })
	var httpTier, httpHint, wsTier string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readUpstreamRequestBody(r)
		httpTier = gjson.GetBytes(body, "service_tier").String()
		httpHint = r.Header.Get("X-Codex-Routing-Hint")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, device *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
		wsTier = gjson.GetBytes(body, "service_tier").String()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	for _, ws := range []bool{false, true} {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 621, AccessToken: "test"}, []byte(`{"model":"gpt-5.6-sol","input":"hello","service_tier":"ultrafast"}`), "session", "", "local", nil, http.Header{}, ws)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if httpTier != "ultrafast" || wsTier != "ultrafast" || httpHint != "model=gpt-5.6-sol;tier=ultrafast" {
		t.Fatalf("upstream tiers: HTTP=%q WS=%q hint=%q", httpTier, wsTier, httpHint)
	}
}
