package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestParseOpenAIResponsesBalancePayload(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      float64
		wantUnit  string
		wantLimit bool
	}{
		{name: "sub2api remaining", body: `{"remaining":12.34,"unit":"USD"}`, want: 12.34, wantUnit: "USD"},
		{name: "new api token data", body: `{"code":true,"data":{"total_available":900000,"unlimited_quota":true}}`, want: 900000, wantUnit: "quota", wantLimit: true},
		{name: "nested balance", body: `{"data":{"quota":{"remaining":"3.5"}}}`, want: 3.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOpenAIResponsesBalancePayload([]byte(tt.body))
			if err != nil {
				t.Fatalf("parse balance: %v", err)
			}
			if got.Balance != tt.want || got.Unit != tt.wantUnit || got.Unlimited != tt.wantLimit {
				t.Fatalf("got %#v, want balance=%v unit=%q unlimited=%v", got, tt.want, tt.wantUnit, tt.wantLimit)
			}
		})
	}
}

func TestQueryOpenAIResponsesBalanceUsesSub2API(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Errorf("path = %s, want /v1/usage", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"unrestricted","balance":8.75,"unit":"USD"}`))
	}))
	defer server.Close()

	got, err := queryOpenAIResponsesBalance(context.Background(), server.URL, "sk-test", "", nil, "")
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	if got.Balance != 8.75 || got.Unit != "USD" || got.Source != "sub2api" {
		t.Fatalf("got %#v", got)
	}
}

func TestQueryOpenAIResponsesBalanceFallsBackToNewAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			http.NotFound(w, r)
		case "/v1/dashboard/billing/subscription":
			_, _ = w.Write([]byte(`{"hard_limit_usd":100}`))
		case "/v1/dashboard/billing/usage":
			_, _ = w.Write([]byte(`{"total_usage":1250}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := queryOpenAIResponsesBalance(context.Background(), server.URL, "sk-test", "", nil, "")
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	if got.Balance != 87.5 || got.Unit != "USD" || got.Source != "new-api" {
		t.Fatalf("got %#v", got)
	}
}

func TestQueryOpenAIResponsesBalanceUsesNewAPITokenEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/usage" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/api/usage/token/" {
			t.Errorf("path = %s, want /api/usage/token/", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":true,"data":{"total_available":4500,"unlimited_quota":false}}`))
	}))
	defer server.Close()

	got, err := queryOpenAIResponsesBalance(context.Background(), server.URL, "sk-test", "", nil, "")
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	if got.Balance != 4500 || got.Unit != "quota" || got.Source != "new-api" || got.Unlimited {
		t.Fatalf("got %#v", got)
	}
}

func TestQueryOpenAIResponsesBalanceUsesConfiguredEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/balance" {
			t.Errorf("path = %s, want /billing/balance", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":4.25,"unit":"USD"}`))
	}))
	defer server.Close()

	got, err := queryOpenAIResponsesBalance(
		context.Background(),
		server.URL+"/v1",
		"sk-test",
		"",
		nil,
		"/billing/balance",
	)
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	if got.Balance != 4.25 || got.Unit != "USD" || got.Source != "custom" {
		t.Fatalf("got %#v", got)
	}
}

func TestQueryOpenAIResponsesBalanceUsesAbsoluteConfiguredEndpoint(t *testing.T) {
	var hit atomic.Bool
	absoluteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		if r.URL.Path != "/absolute/balance" {
			t.Errorf("path = %s, want /absolute/balance", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":6.5,"unit":"USD"}`))
	}))
	defer absoluteServer.Close()

	// 同主机(不同路径/端口形态)的绝对地址被采纳。
	got, err := queryOpenAIResponsesBalance(
		context.Background(),
		absoluteServer.URL+"/v1",
		"sk-test",
		"",
		nil,
		absoluteServer.URL+"/absolute/balance",
	)
	if err != nil {
		t.Fatalf("query balance: %v", err)
	}
	if !hit.Load() || got.Balance != 6.5 || got.Unit != "USD" || got.Source != "custom" {
		t.Fatalf("hit=%v got %#v", hit.Load(), got)
	}

	// 跨主机的绝对地址会带着账号密钥出站,必须整体拒绝且不发起请求。
	hit.Store(false)
	if _, err := queryOpenAIResponsesBalance(
		context.Background(),
		"http://base.example/v1",
		"sk-test",
		"",
		nil,
		absoluteServer.URL+"/absolute/balance",
	); err == nil {
		t.Fatal("expected cross-host balance URL to be rejected")
	}
	if hit.Load() {
		t.Fatal("cross-host balance URL must not be requested at all")
	}
}

func TestResolveOpenAIResponsesBalanceQueryURLRejectsUserinfo(t *testing.T) {
	if _, err := resolveOpenAIResponsesBalanceQueryURL("https://relay.example/v1", "https://user:pass@relay.example/balance"); err == nil {
		t.Fatal("expected absolute URL with userinfo to be rejected")
	}
}

func TestNormalizeOpenAIResponsesBalanceQueryURL(t *testing.T) {
	if got, err := normalizeOpenAIResponsesBalanceQueryURL("/api/usage/token/"); err != nil || got != "/api/usage/token/" {
		t.Fatalf("relative path = %q, err=%v", got, err)
	}
	if _, err := normalizeOpenAIResponsesBalanceQueryURL("javascript:alert(1)"); err == nil {
		t.Fatal("expected non-http URL to be rejected")
	}
	if _, err := normalizeOpenAIResponsesBalanceQueryURL("balance"); err == nil {
		t.Fatal("expected relative URL without leading slash to be rejected")
	}
	if _, err := normalizeOpenAIResponsesBalanceQueryURL("//unexpected.example/balance"); err == nil {
		t.Fatal("expected network-path URL to be rejected")
	}
}
