package admin

import (
	"runtime"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestAccountResponsePauseSnapshotStaysConsistentDuringUsageUpdates(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 7, AccessToken: "token", PlanType: "plus", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
	store.AddAccount(account)
	store.SetGlobalAutoPauseThresholds(0.9, 0.9)
	handler := &Handler{store: store}
	// Every atomic update has a distinct pair of reset timestamps and pause window.
	// A response must describe exactly one update, including when a pause clears.
	base := time.Now().UTC().Truncate(time.Second)
	windows := []string{"5h", "7d", ""}
	resets5h := []time.Time{base.Add(time.Hour), base.Add(2 * time.Hour), base.Add(3 * time.Hour)}
	resets7d := []time.Time{base.Add(24 * time.Hour), base.Add(48 * time.Hour), base.Add(72 * time.Hour)}
	update := func(i int) {
		account.Mu().Lock()
		account.UsagePercent5h, account.UsagePercent7d = 20, 20
		if i == 0 {
			account.UsagePercent5h = 95
		}
		if i == 1 {
			account.UsagePercent7d = 95
		}
		account.UsagePercent5hValid, account.UsagePercent7dValid = true, true
		account.Reset5hAt, account.Reset7dAt = resets5h[i], resets7d[i]
		account.Mu().Unlock()
	}
	update(0)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i = (i + 1) % len(windows) {
			select {
			case <-stop:
				return
			default:
			}
			update(i)
			runtime.Gosched()
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 3000; i++ {
		detailed := i%2 == 0
		response := handler.buildAccountResponse(&database.AccountRow{ID: 7}, account, nil, nil, nil, detailed)
		state := -1
		for j, window := range windows {
			if response.QuotaPauseWindow == window {
				state = j
				break
			}
		}
		if state < 0 {
			t.Fatalf("unknown pause window: %q", response.QuotaPauseWindow)
		}
		wantStatus := "quota_paused"
		if windows[state] == "" {
			wantStatus = "active"
		}
		if response.Status != wantStatus || response.Reset5hAt != resets5h[state].Format(time.RFC3339) || response.Reset7dAt != resets7d[state].Format(time.RFC3339) {
			t.Fatalf("detail=%v: mixed usage snapshots: status=%s window=%q reset5h=%s reset7d=%s; want status=%s reset5h=%s reset7d=%s", detailed, response.Status, response.QuotaPauseWindow, response.Reset5hAt, response.Reset7dAt, wantStatus, resets5h[state].Format(time.RFC3339), resets7d[state].Format(time.RFC3339))
		}
	}
}
