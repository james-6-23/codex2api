package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestShouldMarkBatchTestAccountError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "forbidden is account scoped",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"code":"unsupported_country_region_territory"}}`),
			want:       true,
		},
		{
			name:       "payment required deactivated workspace is account scoped",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"detail":{"code":"deactivated_workspace"}}`),
			want:       true,
		},
		{
			name:       "invalid grant bad request is account scoped",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":"invalid_grant"}`),
			want:       true,
		},
		{
			name:       "model version bad request is global",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"detail":"The 'gpt-5.5' model requires a newer version of Codex"}`),
			want:       false,
		},
		{
			name:       "server error is not marked as account error",
			statusCode: http.StatusBadGateway,
			body:       []byte(`bad gateway`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldMarkBatchTestAccountError(tt.statusCode, tt.body); got != tt.want {
				t.Fatalf("shouldMarkBatchTestAccountError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveBatchTestAccountsDefaultsToAllAccounts(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	accounts, missing := resolveBatchTestAccounts(store, nil)
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
}

func TestResolveBatchTestAccountsUsesSelectedIDs(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	ids := []int64{2, 99, 2, 1}
	accounts, missing := resolveBatchTestAccounts(store, &ids)
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
	if accounts[0].DBID != 2 || accounts[1].DBID != 1 {
		t.Fatalf("account order = [%d, %d], want [2, 1]", accounts[0].DBID, accounts[1].DBID)
	}
}

func TestRunSingleBatchTestTimesOutSlowStreamingBody(t *testing.T) {
	previousTimeout := batchTestAccountTimeout
	batchTestAccountTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		batchTestAccountTimeout = previousTimeout
	})

	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	start := time.Now()
	status, msg := handler.runSingleBatchTest(context.Background(), account)
	elapsed := time.Since(start)

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(msg, "测试超时") {
		t.Fatalf("message = %q, want timeout message", msg)
	}
	if elapsed > time.Second {
		t.Fatalf("batch test took %s, want bounded timeout", elapsed)
	}
	if account.Status == auth.StatusError {
		t.Fatal("timeout should not mark account as permanent error")
	}
	if account.LastTimeoutAt.IsZero() {
		t.Fatal("timeout should be recorded in scheduler health")
	}
}

func TestRunSingleBatchTestSuccessRecoversBannedAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:               1,
		UpstreamType:       auth.UpstreamOpenAIResponses,
		BaseURL:            server.URL,
		APIKey:             "test-key",
		Models:             []string{"gpt-4o-mini"},
		Status:             auth.StatusCooldown,
		CooldownUtil:       time.Now().Add(time.Hour),
		CooldownReason:     "unauthorized",
		HealthTier:         auth.HealthTierBanned,
		FailureStreak:      3,
		LastFailureAt:      time.Now().Add(-time.Minute),
		LastUnauthorizedAt: time.Now().Add(-time.Minute),
	}
	atomic.StoreInt32(&account.Disabled, 1)
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "success" {
		t.Fatalf("status = %q, message = %q, want success", status, msg)
	}

	account.Mu().RLock()
	accountStatus := account.Status
	healthTier := account.HealthTier
	cooldownUntil := account.CooldownUtil
	cooldownReason := account.CooldownReason
	failureStreak := account.FailureStreak
	successStreak := account.SuccessStreak
	lastSuccessAt := account.LastSuccessAt
	account.Mu().RUnlock()

	if atomic.LoadInt32(&account.Disabled) != 0 {
		t.Fatal("successful batch test should clear disabled flag")
	}
	if accountStatus != auth.StatusReady {
		t.Fatalf("Status = %v, want ready", accountStatus)
	}
	if healthTier == auth.HealthTierBanned {
		t.Fatal("successful batch test should recover banned health tier")
	}
	if !cooldownUntil.IsZero() || cooldownReason != "" {
		t.Fatalf("cooldown = (%s, %q), want cleared", cooldownUntil, cooldownReason)
	}
	if failureStreak != 0 {
		t.Fatalf("FailureStreak = %d, want 0", failureStreak)
	}
	if successStreak == 0 || lastSuccessAt.IsZero() {
		t.Fatal("successful batch test should record scheduler success")
	}
}

func TestRunSingleBatchTestResponseFailedDoesNotRecoverCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"message":"model unavailable","code":"model_not_available"}}}` + "\n\n"))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:           1,
		UpstreamType:   auth.UpstreamOpenAIResponses,
		BaseURL:        server.URL,
		APIKey:         "test-key",
		Models:         []string{"gpt-4o-mini"},
		Status:         auth.StatusCooldown,
		CooldownUtil:   time.Now().Add(time.Hour),
		CooldownReason: "rate_limited",
		HealthTier:     auth.HealthTierRisky,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status == "success" {
		t.Fatalf("status = success, want failure for response.failed; message=%q", msg)
	}
	if !strings.Contains(msg, "model unavailable") {
		t.Fatalf("message = %q, want upstream failure detail", msg)
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
}

func TestRunSingleBatchTestResponseFailedMarksReadyAccountError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"message":"model unavailable","code":"model_not_available"}}}` + "\n\n"))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status == "success" {
		t.Fatalf("status = success, want failure for response.failed; message=%q", msg)
	}
	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "model unavailable") {
		t.Fatalf("ErrorMsg = %q, want model unavailable", errorMsg)
	}
}

func TestRunSingleBatchTestUsageLimitResponseFailedMarksRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":3600}}}` + "\n\n"))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "rate_limited" {
		t.Fatalf("status = %q, message = %q, want rate_limited", status, msg)
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
}

func TestApplyUsageLimitedTestStateMarks7dRateLimited(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "team",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	account.SetUsagePercent7d(100)
	account.SetReset7dAt(time.Now().Add(time.Hour))
	store.AddAccount(account)

	applyUsageLimitedTestState(store, account, proxy.CodexUsageSyncResult{
		HasUsage7d: true,
		UsagePct7d: 100,
	})

	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != "rate_limited" || until.IsZero() {
		t.Fatalf("cooldown = (%q, %s), want active rate_limited cooldown", reason, until)
	}
}

func TestApplyUsageLimitedTestStatePreservesBannedAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:       1,
		PlanType:   "team",
		Status:     auth.StatusReady,
		HealthTier: auth.HealthTierBanned,
	}
	account.SetUsagePercent7d(100)
	account.SetReset7dAt(time.Now().Add(time.Hour))
	store.AddAccount(account)

	applyUsageLimitedTestState(store, account, proxy.CodexUsageSyncResult{
		HasUsage7d: true,
		UsagePct7d: 100,
	})

	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
}

func TestRunSingleBatchTestUnauthorizedRecordsErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_invalidated","message":"token invalidated"},"status":401}`))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "banned" {
		t.Fatalf("status = %q, message = %q, want banned", status, msg)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "token_invalidated") {
		t.Fatalf("ErrorMsg = %q, want token_invalidated", errorMsg)
	}
}

// TestRunSingleBatchTestDeletedAgentRuntimeMarksBanned 验证批量测试会将 runtime 已删除的账号标记为封禁。
func TestRunSingleBatchTestDeletedAgentRuntimeMarksBanned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Agent runtime has been deleted.","type":null,"code":"biscuit_baker_service_agent_error_status","param":null},"status":403}`))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "banned" {
		t.Fatalf("status = %q, message = %q, want banned", status, msg)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	_, cooldownUntil := account.GetCooldownSnapshot()
	if remaining := time.Until(cooldownUntil); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("cooldown remaining = %s, want approximately 24h", remaining)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "Agent runtime has been deleted") {
		t.Fatalf("ErrorMsg = %q, want deleted runtime message", errorMsg)
	}
}
