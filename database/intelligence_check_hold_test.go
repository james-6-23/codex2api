package database

import (
	"context"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

func intelligenceHoldState(t *testing.T, db *DB, id int64) IntelligenceCheckAccount {
	t.Helper()
	accounts, _, err := db.ListIntelligenceCheckAccounts(context.Background(), IntelligenceCheckAccountFilter{AccountID: id})
	if err != nil || len(accounts) != 1 {
		t.Fatal(accounts, err)
	}
	return accounts[0]
}

// A rate-limited account must not be probed until its quota recovers, and the
// wait is neither a pass nor a failure.
func TestIntelligenceCheckQuotaHoldPostponesAutomaticChecks(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s.IntervalSeconds = 300
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	ready, limited := intelligenceAccount(t, db), intelligenceAccount(t, db)
	limitedUntil := now.Add(2 * time.Hour)
	var asked []string
	hold := func(id int64, model string) (time.Time, bool) {
		asked = append(asked, model)
		// The runtime persists cooldowns while it answers; the callback must not
		// be invoked under the queue's own write transaction.
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := db.SetCooldown(writeCtx, id, "", time.Time{}); err != nil {
			t.Errorf("hold evaluated under database locks: %v", err)
		}
		return limitedUntil, id == limited
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10, hold); err != nil || n != 1 {
		t.Fatal("limited account was queued", n, err)
	}
	if len(asked) != 2 || asked[0] != s.GPTModel {
		t.Fatalf("hold asked about %v", asked)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil || run.AccountID != ready {
		t.Fatalf("expected the ready account, got %+v %v", run, err)
	}
	if run, err = db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute); err != nil || run != nil {
		t.Fatalf("limited account has a claimable check: %+v %v", run, err)
	}
	_, total, err := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: limited})
	if err != nil || total != 0 {
		t.Fatal("a postponed check left a record", total, err)
	}
	// The reset is further away than one interval: look again after the interval.
	state := intelligenceHoldState(t, db, limited)
	if state.ProbePhase != ic.PhaseQuotaWait || state.NextDueAt == nil || !state.NextDueAt.Equal(now.Add(300*time.Second)) {
		t.Fatalf("unexpected wait: phase=%q due=%v", state.ProbePhase, state.NextDueAt)
	}
	if state.Status != "unknown" || state.DegradeStreak != 0 || state.RecoverStreak != 0 || state.ErrorRetrySeconds != 0 {
		t.Fatalf("a postponed check was graded: %+v", state)
	}
	bars, err := db.IntelligenceCheckBars(ctx, []int64{limited})
	if err != nil {
		t.Fatal(err)
	}
	if bar := bars[limited]; bar.ProbePhase != ic.PhaseQuotaWait || bar.NextDueAt != nil || bar.ActiveRunID != 0 || len(bar.Records) != 0 {
		t.Fatalf("unexpected bar: %+v", bar)
	}

	// Not due again before the interval elapses.
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(time.Minute), 10, hold); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	// A reset closer than the interval is the next deadline.
	later := now.Add(300 * time.Second)
	limitedUntil = later.Add(30 * time.Second)
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, later, 10, hold); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if state = intelligenceHoldState(t, db, limited); !state.NextDueAt.Equal(limitedUntil) {
		t.Fatalf("due=%v want %v", state.NextDueAt, limitedUntil)
	}
	// An unknown recovery time falls back to the interval.
	later, limitedUntil = limitedUntil, time.Time{}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, later, 10, hold); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if state = intelligenceHoldState(t, db, limited); !state.NextDueAt.Equal(later.Add(300 * time.Second)) {
		t.Fatalf("due=%v want %v", state.NextDueAt, later.Add(300*time.Second))
	}

	// An explicit manual check is not subject to the wait.
	if _, err = db.EnqueueIntelligenceChecks(ctx, []int64{limited}, later); err != nil {
		t.Fatal(err)
	}
	run, err = db.ClaimIntelligenceCheck(ctx, "manual", later, time.Minute)
	if err != nil || run == nil || run.AccountID != limited || run.Source != "manual" {
		t.Fatalf("manual check blocked by the wait: %+v %v", run, err)
	}
	if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "manual", ic.Observation{Interrupted: true, Error: "rate limited"}, later); err != nil || !ok {
		t.Fatal(ok, err)
	}

	// Once the quota recovers the check resumes and its result replaces the wait.
	recovered := later.Add(time.Hour)
	released := func(int64, string) (time.Time, bool) { return time.Time{}, false }
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, recovered, 10, released); err != nil || n != 1 {
		t.Fatal("recovered account was not queued", n, err)
	}
	run, err = db.ClaimIntelligenceCheck(ctx, "worker", recovered, time.Minute)
	if err != nil || run == nil || run.AccountID != limited || run.Source != "automatic" {
		t.Fatalf("expected the recovered account, got %+v %v", run, err)
	}
	if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: "21", DurationMS: 1}, recovered); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if state = intelligenceHoldState(t, db, limited); state.Status != "normal" || state.ProbePhase == ic.PhaseQuotaWait || state.RecoverStreak != 1 {
		t.Fatalf("wait outlived a graded result: %+v", state)
	}
}

// Only accounts that automatic checks still cover are shown as waiting.
func TestIntelligenceCheckQuotaHoldBarFollowsAutomaticChecks(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	id := intelligenceAccount(t, db)
	held := func(int64, string) (time.Time, bool) { return time.Time{}, true }
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10, held); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	phase := func() string {
		t.Helper()
		bars, err := db.IntelligenceCheckBars(ctx, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		return bars[id].ProbePhase
	}
	if got := phase(); got != ic.PhaseQuotaWait {
		t.Fatalf("phase=%q", got)
	}
	if err := db.SetIntelligenceCheckEnabled(ctx, []int64{id}, false); err != nil {
		t.Fatal(err)
	}
	if got := phase(); got != "" {
		t.Fatalf("an account without automatic checks still waits: %q", got)
	}
	// Without a hold the queue behaves exactly as before.
	if err := db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
