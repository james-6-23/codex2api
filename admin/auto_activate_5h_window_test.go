package admin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestDefaultRuntimeSettingsAutoActivate5hDisabled(t *testing.T) {
	if proxy.DefaultRuntimeSettings().AutoActivate5hWindowEnabled {
		t.Fatal("AutoActivate5hWindowEnabled = true, want false")
	}
}

func TestRunAutoActivate5hScanDisabledDoesNotRequest(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	account := eligibleActivate5hAccount(11, now)
	store.AddAccount(account)

	var called atomic.Int32
	handler := &Handler{
		store: store,
		activate5hWindow: func(context.Context, *auth.Account) error {
			called.Add(1)
			return nil
		},
	}

	stats := handler.runAutoActivate5hScan(context.Background(), now)
	if stats.Enabled || stats.Activated != 0 || called.Load() != 0 {
		t.Fatalf("disabled scan stats=%+v called=%d", stats, called.Load())
	}
}

func TestRunAutoActivate5hScanActivatesOncePerWindow(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoActivate5hWindowEnabled = true
	proxy.ApplyRuntimeSettings(settings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	account := eligibleActivate5hAccount(12, now)
	store.AddAccount(account)

	var called atomic.Int32
	handler := &Handler{
		store: store,
		activate5hWindow: func(context.Context, *auth.Account) error {
			called.Add(1)
			return nil
		},
	}

	first := handler.runAutoActivate5hScan(context.Background(), now)
	if !first.Enabled || first.Scanned != 1 || first.Candidates != 1 || first.Activated != 1 || first.Failed != 0 {
		t.Fatalf("first scan = %+v", first)
	}
	if called.Load() != 1 {
		t.Fatalf("activate calls = %d, want 1", called.Load())
	}
	if got := account.GetActivated5hResetAt(); got.Unix() != now.Add(-time.Minute).Unix() {
		t.Fatalf("activated reset = %s, want previous Reset5hAt", got)
	}

	second := handler.runAutoActivate5hScan(context.Background(), now)
	if second.Activated != 0 || called.Load() != 1 {
		t.Fatalf("second scan = %+v called=%d, want no repeat", second, called.Load())
	}
}

func TestRunAutoActivate5hScanSkipsUnavailableAndMissingWindow(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoActivate5hWindowEnabled = true
	proxy.ApplyRuntimeSettings(settings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)

	missing := &auth.Account{DBID: 13, AccessToken: "at", Status: auth.StatusReady, PlanType: "team"}
	errored := eligibleActivate5hAccount(14, now)
	errored.Status = auth.StatusError
	store.AddAccount(missing)
	store.AddAccount(errored)

	var called atomic.Int32
	handler := &Handler{
		store: store,
		activate5hWindow: func(context.Context, *auth.Account) error {
			called.Add(1)
			return nil
		},
	}

	stats := handler.runAutoActivate5hScan(context.Background(), now)
	if stats.Candidates != 0 || stats.Activated != 0 || called.Load() != 0 {
		t.Fatalf("skip scan = %+v called=%d", stats, called.Load())
	}
}

func eligibleActivate5hAccount(id int64, now time.Time) *auth.Account {
	account := &auth.Account{DBID: id, AccessToken: "at", Status: auth.StatusReady, PlanType: "team"}
	account.SetUsageSnapshot5hAt(80, now.Add(-time.Minute), now.Add(-2*time.Hour))
	return account
}
