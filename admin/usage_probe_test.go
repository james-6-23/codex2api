package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestShouldMarkUsageProbeAccountError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "payment required deactivated workspace",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"detail":{"code":"deactivated_workspace"}}`),
			want:       true,
		},
		{
			name:       "forbidden deactivated workspace",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"code":"deactivated_workspace"}}`),
			want:       true,
		},
		{
			name:       "generic payment required is not account error",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"error":{"code":"billing_hard_limit_reached"}}`),
			want:       false,
		},
		{
			name:       "rate limit handled separately",
			statusCode: http.StatusTooManyRequests,
			body:       []byte(`{"detail":{"code":"deactivated_workspace"}}`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldMarkUsageProbeAccountError(tt.statusCode, tt.body); got != tt.want {
				t.Fatalf("shouldMarkUsageProbeAccountError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// issue #328：codex_at 账号可能 wham 恒 401 但真实流量可用。
// wham 单方面 401 不得把账号打入 unauthorized 冷却（误封后手动重置也会被再次封禁）。
func TestProbeUsageSnapshotWhamUnauthorizedDoesNotBanWhenFallbackUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, nil)
	// 关闭回退 → whamOnly：缺少 /responses 佐证时不允许封号
	store.SetUsageProbeResponsesFallbackEnabled(false)
	account := &auth.Account{DBID: 1, AccessToken: "at-only-token", Status: auth.StatusReady}
	store.AddAccount(account)

	h := &Handler{store: store}
	err := h.ProbeUsageSnapshot(context.Background(), account)
	if err == nil {
		t.Fatal("ProbeUsageSnapshot() expected error for wham 401")
	}
	if !errors.Is(err, errWhamUnauthorized) {
		t.Fatalf("error = %v, want errWhamUnauthorized", err)
	}
	if account.Status != auth.StatusReady {
		t.Fatalf("account status = %v, want %v (wham-only 401 must not ban)", account.Status, auth.StatusReady)
	}
	if account.CooldownReason == "unauthorized" {
		t.Fatal("account marked unauthorized cooldown by wham-only 401")
	}
}

// wham 429 的既有行为不受影响：只上报失败，不封号、不归类为 unauthorized。
func TestProbeUsageSnapshotWhamRateLimitedKeepsAccountUsable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, nil)
	store.SetUsageProbeResponsesFallbackEnabled(false)
	account := &auth.Account{DBID: 2, AccessToken: "at-only-token", Status: auth.StatusReady}
	store.AddAccount(account)

	h := &Handler{store: store}
	err := h.ProbeUsageSnapshot(context.Background(), account)
	if err == nil {
		t.Fatal("ProbeUsageSnapshot() expected error for wham 429")
	}
	if errors.Is(err, errWhamUnauthorized) {
		t.Fatalf("429 must not be classified as unauthorized: %v", err)
	}
	if account.CooldownReason == "unauthorized" {
		t.Fatal("account marked unauthorized cooldown by wham 429")
	}
}
