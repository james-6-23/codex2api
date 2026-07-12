package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestUpstreamResetErrorMessage_CreditsOnlyMapsToChineseAndKeepsRaw(t *testing.T) {
	body := []byte(`{"detail":{"code":"rate_limit_not_resettable","reason":"credits_only"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)

	if !strings.Contains(msg, "额度（credits）计费") {
		t.Errorf("message missing Chinese explanation: %q", msg)
	}
	// 必须保留上游原文，便于排查。
	if !strings.Contains(msg, "rate_limit_not_resettable") || !strings.Contains(msg, "credits_only") {
		t.Errorf("message must retain raw upstream body: %q", msg)
	}
}

func TestUpstreamResetErrorMessage_KnownCodeWithoutReason(t *testing.T) {
	body := []byte(`{"detail":{"code":"rate_limit_not_resettable"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)
	if !strings.Contains(msg, "不支持主动重置") {
		t.Errorf("expected generic not-resettable Chinese message, got %q", msg)
	}
	if !strings.Contains(msg, "rate_limit_not_resettable") {
		t.Errorf("expected raw body retained, got %q", msg)
	}
}

func TestUpstreamResetErrorMessage_UnknownCodeFallsBackToRaw(t *testing.T) {
	body := []byte(`{"detail":{"code":"something_new"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)
	if !strings.Contains(msg, "something_new") {
		t.Errorf("unknown code should fall back to raw body, got %q", msg)
	}
	// 未识别 code 时不应硬塞中文说明。
	if strings.Contains(msg, "（上游：") {
		t.Errorf("unknown code should not be wrapped with Chinese prefix, got %q", msg)
	}
}

func TestUpstreamResetErrorMessage_EmptyBodyUsesStatus(t *testing.T) {
	msg := upstreamResetErrorMessage(http.StatusBadGateway, nil)
	if !strings.Contains(msg, "502") {
		t.Errorf("empty body should report status code, got %q", msg)
	}
}

func TestEarliestAutoResetCreditUsesConsumableUntilAndFutureWindow(t *testing.T) {
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	credits := []proxy.WhamResetCreditItem{
		{ID: "past", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)},
		{ID: "outside", ConsumableUntil: now.Add(61 * time.Minute).Format(time.RFC3339)},
		{ID: "fallback", ExpiresAt: now.Add(40 * time.Minute).Format(time.RFC3339)},
		{
			ID:              "canonical",
			ExpiresAt:       now.Add(5 * time.Minute).Format(time.RFC3339),
			ConsumableUntil: now.Add(20 * time.Minute).Format(time.RFC3339Nano),
		},
	}

	credit, expiresAt, ok := earliestAutoResetCredit(credits, now, time.Hour)
	if !ok {
		t.Fatal("earliestAutoResetCredit() = no candidate")
	}
	if credit.ID != "canonical" {
		t.Fatalf("credit.ID = %q, want canonical", credit.ID)
	}
	if want := now.Add(20 * time.Minute); !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, want)
	}
}

func TestAutoResetCreditsPlanOnlyPlusAndPro(t *testing.T) {
	for _, plan := range []string{"plus", "pro", "prolite", "PRO-LITE"} {
		if !isAutoResetCreditsPlan(plan) {
			t.Errorf("isAutoResetCreditsPlan(%q) = false, want true", plan)
		}
	}
	for _, plan := range []string{"", "free", "team", "k12", "business", "api"} {
		if isAutoResetCreditsPlan(plan) {
			t.Errorf("isAutoResetCreditsPlan(%q) = true, want false", plan)
		}
	}
}

func TestStableAutoResetCreditRequestID(t *testing.T) {
	account := &auth.Account{DBID: 7, AccountID: "workspace-1"}
	credit := proxy.WhamResetCreditItem{ID: "credit-1", ConsumableUntil: "2026-07-12T05:00:00Z"}
	first := stableAutoResetCreditRequestID(account, credit)
	second := stableAutoResetCreditRequestID(account, credit)
	if first == "" || first != second {
		t.Fatalf("stable request IDs = %q / %q", first, second)
	}
	other := stableAutoResetCreditRequestID(account, proxy.WhamResetCreditItem{ID: "credit-2"})
	if first == other {
		t.Fatalf("different credits share request ID %q", first)
	}
}

func TestRunAutoResetCreditsScanConsumesOneAndAuditsAuto(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	runtimeSettings.AutoResetCreditsBeforeExpiryMin = 60
	proxy.ApplyRuntimeSettings(runtimeSettings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 11, AccountID: "workspace-11", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	credit := proxy.WhamResetCreditItem{
		ID:              "credit-expiring",
		ResetType:       "codex_rate_limits",
		Status:          "available",
		ConsumableUntil: now.Add(30 * time.Minute).Format(time.RFC3339),
	}
	var gotRedeemID string
	var gotEventType, gotSource string
	probeDone := make(chan struct{}, 1)
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{credit}}, nil, nil
		},
		consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, redeemID string) (*proxy.WhamResetResult, *http.Response, error) {
			gotRedeemID = redeemID
			return &proxy.WhamResetResult{WindowsReset: 2}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(_ int64, eventType, source string) {
			gotEventType, gotSource = eventType, source
		},
		probeUsage: func(context.Context, *auth.Account) error {
			probeDone <- struct{}{}
			return nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), now)
	if !stats.Enabled || stats.Scanned != 1 || stats.Queried != 1 || stats.Candidates != 1 || stats.Consumed != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if gotRedeemID != stableAutoResetCreditRequestID(account, credit) {
		t.Fatalf("redeem ID = %q, want stable ID", gotRedeemID)
	}
	if gotEventType != "reset_credit" || gotSource != "auto" {
		t.Fatalf("event = %q/%q, want reset_credit/auto", gotEventType, gotSource)
	}
	if count, ok := account.GetRateLimitResetCredits(); !ok || count != 1 {
		t.Fatalf("remaining credits = (%d,%v), want (1,true)", count, ok)
	}
	select {
	case <-probeDone:
	case <-time.After(time.Second):
		t.Fatal("post-reset usage probe was not triggered")
	}
}

func TestRunAutoResetCreditsScanDisabledDoesNotQuery(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 12, AccessToken: "token", PlanType: "plus"})
	queried := false
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Enabled || queried {
		t.Fatalf("disabled scan stats=%+v queried=%v", stats, queried)
	}
}

func TestRunAutoResetCreditsScanUsesDatabaseSettingAsAuthority(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	db := newTestAdminDB(t)
	persisted := defaultBootstrapSettings()
	persisted.AutoResetCreditsEnabled = false
	persisted.AutoResetCreditsBeforeExpiryMin = 60
	if err := db.UpdateSystemSettings(context.Background(), persisted); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 14, AccessToken: "token", PlanType: "plus"})
	queried := false
	handler := &Handler{
		db:    db,
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Enabled || queried {
		t.Fatalf("database-disabled scan stats=%+v queried=%v", stats, queried)
	}
}

func TestAutoResetCreditsSkipsImmediatelyAfterManualReset(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 15, AccountID: "workspace-15", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	queries := 0
	consumes := 0
	manualHandler := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	autoHandler := &Handler{
		store: store,
		cache: tc,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queries++
			return &proxy.WhamResetCreditsList{}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	lock := manualHandler.resetCreditLock(account)
	lock.Lock()
	_, failure := manualHandler.consumeResetCreditLocked(context.Background(), account, "manual-request", "manual")
	lock.Unlock()
	if failure != nil {
		t.Fatalf("manual consume failure: %+v", failure)
	}

	stats := autoHandler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Queried != 0 || stats.Consumed != 0 || queries != 0 || consumes != 1 {
		t.Fatalf("stats=%+v queries=%d consumes=%d, want no immediate auto reset", stats, queries, consumes)
	}
}

func TestConsumeResetCreditLockedDoesNotReplaySuccessfulAutoRequestID(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 16, AccountID: "workspace-16", AccessToken: "token", PlanType: "pro"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	consumes := 0
	events := 0
	handler := &Handler{
		store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) { events++ },
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	first, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-auto-id", "auto")
	if failure != nil || first.AlreadyHandled {
		t.Fatalf("first outcome=%+v failure=%+v", first, failure)
	}
	second, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-auto-id", "auto")
	if failure != nil || !second.AlreadyHandled {
		t.Fatalf("second outcome=%+v failure=%+v", second, failure)
	}
	if consumes != 1 || events != 1 {
		t.Fatalf("consumes=%d events=%d, want 1/1", consumes, events)
	}
	if count, ok := account.GetRateLimitResetCredits(); !ok || count != 1 {
		t.Fatalf("remaining credits=(%d,%v), want (1,true)", count, ok)
	}
}

func TestConsumeResetCreditLockedHonorsSharedWorkspaceLease(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 17, AccountID: "workspace-shared", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	first := &Handler{store: store, cache: tc}
	acquired, release, err := first.acquireResetCreditLease(context.Background(), account)
	if err != nil || !acquired {
		t.Fatalf("first lease = (%v,%v)", acquired, err)
	}
	defer release()

	called := false
	second := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			called = true
			return nil, nil, nil
		},
	}
	outcome, failure := second.consumeResetCreditLocked(context.Background(), account, "request-2", "auto")
	if failure != nil || !outcome.InProgress || called {
		t.Fatalf("outcome=%+v failure=%+v called=%v, want in-progress without upstream call", outcome, failure, called)
	}
}

func TestAutoConsumeRechecksSharedCooldownAfterLease(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 19, AccountID: "workspace-cooldown", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	manual := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if _, failure := manual.consumeResetCreditLocked(context.Background(), account, "manual-before-auto", "manual"); failure != nil {
		t.Fatalf("manual consume failure: %+v", failure)
	}

	called := false
	automatic := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			called = true
			return nil, nil, nil
		},
	}
	outcome, failure := automatic.consumeResetCreditLocked(context.Background(), account, "auto-after-query", "auto")
	if failure != nil || !outcome.AlreadyHandled || called {
		t.Fatalf("outcome=%+v failure=%+v called=%v, want cooldown skip after lease", outcome, failure, called)
	}
}

func TestWaitAutoResetCreditsCancelsPostResetProbe(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 18, AccessToken: "token", PlanType: "plus"}
	store.AddAccount(account)
	probeStarted := make(chan struct{})
	probeStopped := make(chan struct{})
	handler := &Handler{
		store: store,
		probeUsage: func(ctx context.Context, _ *auth.Account) error {
			close(probeStarted)
			<-ctx.Done()
			close(probeStopped)
			return ctx.Err()
		},
	}

	backgroundCtx, cancel := context.WithCancel(context.Background())
	handler.StartAutoResetCredits(backgroundCtx)
	handler.refreshUsageAfterReset(account)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("post-reset probe did not start")
	}

	cancel()
	done := make(chan struct{})
	go func() {
		handler.WaitAutoResetCredits()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WaitAutoResetCredits did not return after cancellation")
	}
	select {
	case <-probeStopped:
	default:
		t.Fatal("post-reset probe did not observe cancellation")
	}
}

func TestConsumeResetCreditLockedRefreshes401WithSameRequestID(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 13, AccessToken: "old-token", PlanType: "pro"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	var redeemIDs []string
	refreshes := 0
	handler := &Handler{
		store: store,
		consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, redeemID string) (*proxy.WhamResetResult, *http.Response, error) {
			redeemIDs = append(redeemIDs, redeemID)
			if len(redeemIDs) == 1 {
				return nil, &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
			}
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		refreshAccount: func(context.Context, int64) error {
			refreshes++
			account.Mu().Lock()
			account.AccessToken = "new-token"
			account.Mu().Unlock()
			return nil
		},
	}

	outcome, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-request-id", "auto")
	if failure != nil {
		t.Fatalf("consumeResetCreditLocked failure: %+v", failure)
	}
	if refreshes != 1 || len(redeemIDs) != 2 || redeemIDs[0] != redeemIDs[1] {
		t.Fatalf("refreshes=%d redeemIDs=%v", refreshes, redeemIDs)
	}
	if outcome.WindowsReset != 1 || outcome.Remaining != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
}
