package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

func TestProbeUsageSnapshotRejectsAntigravity(t *testing.T) {
	handler := &Handler{}
	account := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token"}
	err := handler.ProbeUsageSnapshot(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "Antigravity") {
		t.Fatalf("ProbeUsageSnapshot() error = %v, want Antigravity rejection", err)
	}
}

func TestProbeUsageSnapshotClaudeUsesAnthropicMessagesOnly(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 77, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	account.Models = []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"}
	store.AddAccount(account)
	called := false
	h := &Handler{store: store, executeClaudeUsageProbe: func(_ context.Context, acc *auth.Account, body []byte) (*http.Response, error) {
		called = true
		if acc != account || !strings.Contains(string(body), `"model":"claude-haiku-4-5"`) || !strings.Contains(string(body), `"max_tokens":1`) {
			t.Fatalf("unexpected Claude probe request: account=%p body=%s", acc, body)
		}
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message","id":"msg_probe","content":[{"type":"text","text":"ok"}]}`))}
		resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
		resp.Header.Set("anthropic-ratelimit-unified-5h-reset", "4102444800")
		resp.Header.Set("anthropic-ratelimit-unified-7d-utilization", "0.4")
		resp.Header.Set("anthropic-ratelimit-unified-7d-reset", "4103049600")
		return resp, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if !called {
		t.Fatal("Claude probe callback was not called")
	}
	if got := account.UsagePercent5h; got != 25 {
		t.Fatalf("5h usage = %v, want 25", got)
	}
	if got := account.UsagePercent7d; got != 40 {
		t.Fatalf("7d usage = %v, want 40", got)
	}
}

func TestSelectClaudeUsageProbeModelSkipsFableWhenCheaperModelExists(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamClaude,
		Models:       []string{"claude-fable-5", "claude-sonnet-5", "claude-opus-4-7"},
	}
	model, err := selectClaudeUsageProbeModel(account)
	if err != nil || model != "claude-sonnet-5" {
		t.Fatalf("probe model=(%q,%v), want sonnet instead of credits-gated Fable", model, err)
	}
}

func TestSelectClaudeUsageProbeModelSkipsActiveModelCooldown(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamClaude,
		Models:       []string{"claude-haiku-4-5", "claude-sonnet-5"},
	}
	account.SetModelCooldownUntil("claude-haiku-4-5", "credits_required", time.Now().Add(time.Hour))
	model, err := selectClaudeUsageProbeModel(account)
	if err != nil || model != "claude-sonnet-5" {
		t.Fatalf("probe model=(%q,%v), want cooldown-free sonnet", model, err)
	}
}

func TestProbeUsageSnapshotClaudeCreditsRequiredDoesNotCooldownAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{
		DBID:         82,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-fable-5", "claude-sonnet-5"},
	}
	store.AddAccount(account)
	calledModel := ""
	h := &Handler{store: store, executeClaudeUsageProbe: func(_ context.Context, _ *auth.Account, body []byte) (*http.Response, error) {
		calledModel = gjson.GetBytes(body, "model").String()
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-sonnet-5"}}}`)),
		}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil || !strings.Contains(err.Error(), "usage credits") {
		t.Fatalf("credits_required probe error=%v, want explicit usage credits error", err)
	}
	if calledModel != "claude-sonnet-5" {
		t.Fatalf("probe selected model %q, want to skip Fable", calledModel)
	}
	if account.HasActiveCooldown() || account.RuntimeStatus() == "rate_limited" {
		t.Fatalf("credits_required probe must not cool down account: status=%q", account.RuntimeStatus())
	}
	if !account.IsModelRateLimited("claude-sonnet-5") {
		t.Fatal("credits_required probe should set a model-level cooldown")
	}
}

func TestProbeUsageSnapshotClaudePersistsRejectedFiveHourLimit(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 78, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`))}
		resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "1")
		resp.Header.Set("anthropic-ratelimit-unified-5h-reset", "4102444800")
		resp.Header.Set("anthropic-ratelimit-unified-status", "rejected")
		return resp, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil {
		t.Fatal("Claude 429 probe should return an error to the queue")
	}
	if got := account.RuntimeStatus(); got != "rate_limited" && got != "cooldown" && got != auth.ResponsesRateLimitedCooldownReason {
		t.Fatalf("Claude rejected status = %q, want a rate-limited cooldown", got)
	}
	if !account.UsagePercent5hValid || account.UsagePercent5h != 100 {
		t.Fatalf("Claude 5h snapshot = (%v, %t), want 100%% valid", account.UsagePercent5h, account.UsagePercent5hValid)
	}
}

func TestProbeUsageSnapshotClaudeDoesNotClearRejectedStatusOnHTTP200(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 79, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))}
		resp.Header.Set("anthropic-ratelimit-unified-5h-utilization", "1")
		resp.Header.Set("anthropic-ratelimit-unified-5h-reset", "4102444800")
		resp.Header.Set("anthropic-ratelimit-unified-status", "rejected")
		return resp, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if got := account.RuntimeStatus(); got != auth.ResponsesRateLimitedCooldownReason {
		t.Fatalf("Claude rejected status after HTTP 200 = %q, want %q", got, auth.ResponsesRateLimitedCooldownReason)
	}
}

func TestProbeUsageSnapshotClaudePersistsSamplingMetadata(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "claude-sampling", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "claude-token",
		"refresh_token": "claude-refresh",
	}, "")
	if err != nil {
		t.Fatalf("insert Claude account: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store, db: db, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))}, nil
	}}
	if err := h.ProbeUsageSnapshot(ctx, account); err != nil {
		t.Fatalf("successful Claude probe: %v", err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("read successful probe metadata: %v", err)
	}
	if row.GetCredential("claude_usage_probe_at") == "" || row.GetCredential("claude_usage_probe_error") != "" {
		t.Fatalf("successful probe metadata = at=%q error=%q", row.GetCredential("claude_usage_probe_at"), row.GetCredential("claude_usage_probe_error"))
	}

	h.executeClaudeUsageProbe = func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`upstream failed`))}, nil
	}
	if err := h.ProbeUsageSnapshot(ctx, account); err == nil {
		t.Fatal("failed Claude probe should return an error")
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("read failed probe metadata: %v", err)
	}
	if row.GetCredential("claude_usage_probe_at") == "" || row.GetCredential("claude_usage_probe_error") == "" {
		t.Fatalf("failed probe metadata = at=%q error=%q", row.GetCredential("claude_usage_probe_at"), row.GetCredential("claude_usage_probe_error"))
	}
}

func TestProbeUsageSnapshotClaudeRejectsHTTP200ErrorPayload(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{DBID: 80, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"message":"wrapped failure"}}`)),
		}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil {
		t.Fatal("HTTP 200 native error payload must fail the Claude sample")
	}
}

func TestProbeUsageSnapshotClaudeCreditsRequiredWrappedInHTTP200(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{
		DBID:         83,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-5"},
	}
	store.AddAccount(account)
	h := &Handler{store: store, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-sonnet-5"}}}`)),
		}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil || !strings.Contains(err.Error(), "usage credits") {
		t.Fatalf("wrapped credits_required probe error=%v", err)
	}
	if account.HasActiveCooldown() || account.RuntimeStatus() == "rate_limited" {
		t.Fatalf("wrapped credits_required must not cool down account: status=%q", account.RuntimeStatus())
	}
	if !account.IsModelRateLimited("claude-sonnet-5") {
		t.Fatal("wrapped credits_required should cool down only the model")
	}
}

func TestProbeUsageSnapshotClaudeRejectsHTTP200NonMessagePayload(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{DBID: 81, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{store: store, executeClaudeUsageProbe: func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	}}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil {
		t.Fatal("HTTP 200 non-message payload must not count as a successful Claude sample")
	}
}

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
			name:       "forbidden deleted agent runtime",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"message":"Agent runtime has been deleted.","code":"biscuit_baker_service_agent_error_status"},"status":403}`),
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

// TestDeletedAgentRuntimeCooldownPersistsFor24Hours 验证用量探针会持久化 24 小时封禁冷却。
func TestDeletedAgentRuntimeCooldownPersistsFor24Hours(t *testing.T) {
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	account := &auth.Account{
		DBID:        accountID,
		AccessToken: "at-test",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	store.AddAccount(account)

	store.MarkCooldownWithErrorExactDuration(
		account,
		24*time.Hour,
		"unauthorized",
		"用量探针上游返回 403: Agent runtime has been deleted.",
	)

	_, cooldownUntil := account.GetCooldownSnapshot()
	if remaining := time.Until(cooldownUntil); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("runtime cooldown remaining = %s, want approximately 24h", remaining)
	}

	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.CooldownReason != "unauthorized" || !row.CooldownUntil.Valid {
		t.Fatalf("persisted cooldown = (%q, %v), want active unauthorized cooldown", row.CooldownReason, row.CooldownUntil)
	}
	if remaining := time.Until(row.CooldownUntil.Time); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("persisted cooldown remaining = %s, want approximately 24h", remaining)
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

func TestProbeUsageSnapshotRevokedTokenBansWithoutResponsesFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token for user, failing request"},"status":401}`))
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, nil)
	store.SetUsageProbeResponsesFallbackEnabled(false)
	account := &auth.Account{DBID: 2, AccessToken: "revoked-token", Status: auth.StatusReady}
	store.AddAccount(account)

	h := &Handler{store: store}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v, want revoked token handled as terminal account state", err)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	account.Mu().RLock()
	errorMessage := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMessage, "token_revoked") {
		t.Fatalf("ErrorMsg = %q, want token_revoked detail", errorMessage)
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

func TestProbeUsageSnapshotWhamCannotClearResponsesCooldownWhenUsageStatusIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":1800}}
		}`))
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	store.SetUsageProbeResponsesFallbackEnabled(false)
	account := &auth.Account{DBID: 3, AccessToken: "token", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(account)
	store.MarkPremium5hRateLimited(account, time.Now().Add(time.Hour))

	h := &Handler{store: store}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if !account.HasActiveCooldown() {
		t.Fatal("WHAM metadata cleared a cooldown that requires Responses success")
	}
}

func TestProbeUsageSnapshotResponsesSuccessRecoversIgnoredUsageCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":1800}}
		}`))
	}))
	defer server.Close()
	restoreWham := proxy.SetWhamUsageURLForTest(server.URL)
	defer restoreWham()

	executeCalls := 0
	executeRequest := func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		executeCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
		LazyMode:               true,
	})
	store.SetUsageProbeResponsesFallbackEnabled(true)
	account := &auth.Account{DBID: 4, AccessToken: "token", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(account)
	store.BindSessionAffinity("working-turn", account, "")
	store.MarkPremium5hRateLimited(account, time.Now().Add(time.Hour))

	h := &Handler{store: store, executeUsageProbe: executeRequest}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if executeCalls != 1 {
		t.Fatalf("Responses probe calls = %d, want 1 after successful WHAM metadata refresh", executeCalls)
	}
	if account.HasActiveCooldown() {
		t.Fatal("authoritative Responses 200 did not clear the prior cooldown")
	}
	if account.IsAvailable() {
		t.Fatal("WHAM 100% account became available to fresh sessions")
	}
	continued, _ := store.NextForContinuationWithFilter("working-turn", 0, nil, nil)
	if continued != account {
		t.Fatal("authoritative Responses 200 did not preserve active-turn continuation")
	}
	store.Release(continued)
}

func TestProbeUsageSnapshotResponsesFailureInsideHTTP200PreservesCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":1800}}
		}`))
	}))
	defer server.Close()
	restoreWham := proxy.SetWhamUsageURLForTest(server.URL)
	defer restoreWham()

	executeRequest := func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"status_details\":{\"error\":{\"type\":\"usage_limit_reached\",\"plan_type\":\"plus\",\"resets_in_seconds\":1800}}}}\n\n",
			)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	store.SetUsageProbeResponsesFallbackEnabled(true)
	account := &auth.Account{DBID: 6, AccessToken: "token", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(account)
	store.MarkPremium5hRateLimited(account, time.Now().Add(time.Hour))

	h := &Handler{store: store, executeUsageProbe: executeRequest}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if !account.HasActiveCooldown() || account.IsAvailable() {
		t.Fatal("HTTP 200 response.failed usage_limit_reached cleared the account cooldown")
	}
	if reason := account.GetCooldownReason(); reason != auth.ResponsesRateLimitedCooldownReason {
		t.Fatalf("CooldownReason = %q, want %q", reason, auth.ResponsesRateLimitedCooldownReason)
	}
}

func TestProbeUsageSnapshotResponses429PreservesIgnoredUsageCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":1800}}
		}`))
	}))
	defer server.Close()
	restoreWham := proxy.SetWhamUsageURLForTest(server.URL)
	defer restoreWham()

	executeCalls := 0
	executeRequest := func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		executeCalls++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","plan_type":"plus","resets_in_seconds":1800}}`)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	store.SetUsageProbeResponsesFallbackEnabled(true)
	account := &auth.Account{DBID: 5, AccessToken: "token", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(account)
	store.MarkPremium5hRateLimited(account, time.Now().Add(time.Hour))

	h := &Handler{store: store, executeUsageProbe: executeRequest}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() error = %v", err)
	}
	if executeCalls != 1 {
		t.Fatalf("Responses probe calls = %d, want 1", executeCalls)
	}
	if !account.HasActiveCooldown() || account.IsAvailable() {
		t.Fatal("authoritative Responses 429 did not preserve account cooldown")
	}
	if reason := account.GetCooldownReason(); reason == "" {
		t.Fatal("Responses 429 left the account cooling without a reason")
	}
}

// team 空间被封时 wham 返回 402 {"detail":{"code":"deactivated_workspace"}}。
// 即使处于 wham-only 模式(无 /responses 佐证)也必须标错隔离——这是上游对
// 工作区状态的明确裁决,不存在 401 那种鉴权口径误报;否则账号永远"可用"。
func TestProbeUsageSnapshotWhamDeactivatedWorkspaceMarksError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":{"code":"deactivated_workspace"}}`, http.StatusPaymentRequired)
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, nil)
	store.SetUsageProbeResponsesFallbackEnabled(false) // wham-only
	account := &auth.Account{DBID: 1, AccessToken: "team-token", AccountID: "team-A", Status: auth.StatusReady}
	sibling := &auth.Account{DBID: 2, AccessToken: "team-token-2", AccountID: "team-A", Status: auth.StatusReady}
	store.AddAccount(account)
	store.AddAccount(sibling)

	h := &Handler{store: store}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err != nil {
		t.Fatalf("ProbeUsageSnapshot() = %v, want nil (classified)", err)
	}
	if account.Status != auth.StatusError {
		t.Fatalf("account status = %v, want error", account.Status)
	}
	if !strings.Contains(account.ErrorMsg, "deactivated_workspace") {
		t.Fatalf("error message = %q, want deactivated_workspace detail", account.ErrorMsg)
	}
	if sibling.RuntimeStatus() != "error" {
		t.Fatalf("same-workspace sibling status = %q, want error", sibling.RuntimeStatus())
	}
}

// 裸 402(无 deactivated_workspace 证据)不定罪,保持通用失败路径。
func TestProbeUsageSnapshotWhamBarePaymentRequiredDoesNotBan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"Payment Required"}`, http.StatusPaymentRequired)
	}))
	defer server.Close()
	restore := proxy.SetWhamUsageURLForTest(server.URL)
	defer restore()

	store := auth.NewStore(nil, nil, nil)
	store.SetUsageProbeResponsesFallbackEnabled(false)
	account := &auth.Account{DBID: 1, AccessToken: "team-token", Status: auth.StatusReady}
	store.AddAccount(account)

	h := &Handler{store: store}
	if err := h.ProbeUsageSnapshot(context.Background(), account); err == nil {
		t.Fatal("ProbeUsageSnapshot() expected generic error for bare 402")
	}
	if account.Status != auth.StatusReady {
		t.Fatalf("account status = %v, want ready (bare 402 must not ban)", account.Status)
	}
}
