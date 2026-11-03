package database

import (
	"context"
	"encoding/json"
	ic "github.com/codex2api/internal/intelligencecheck"
	"testing"
	"time"
)

func TestIntelligenceCheckAdaptiveLifecycle(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	id := intelligenceAccount(t, db)
	cfg, _ := db.GetIntelligenceCheckSettings(ctx)
	cfg.Enabled, cfg.AdaptiveEnabled, cfg.Action = true, true, "pause"
	cfg.IntervalSeconds, cfg.RetrySeconds, cfg.DegradeThreshold, cfg.RecoverThreshold = 1800, 300, 1, 3
	cfg, e := db.SaveIntelligenceCheckSettings(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	run := func(o ic.Observation) IntelligenceCheckAccount {
		t.Helper()
		if _, e := db.EnqueueIntelligenceChecks(ctx, []int64{id}, now); e != nil {
			t.Fatal(e)
		}
		r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
		if e != nil || r == nil {
			t.Fatal(r, e)
		}
		if ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "worker", o, now.Add(time.Second)); e != nil || !ok {
			t.Fatal(ok, e)
		}
		// Re-read persisted JSON; the policy must not rely on worker memory.
		accounts, _, e := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
		if e != nil || len(accounts) != 1 {
			t.Fatal(accounts, e)
		}
		now = now.Add(time.Minute)
		return accounts[0]
	}
	bad := ic.Observation{Complete: true, Output: "22", DurationMS: 1}
	good := ic.Observation{Complete: true, Output: "21", DurationMS: 1}
	a := run(bad)
	if !a.QualityPaused || a.NextIntervalSeconds != 1800 {
		t.Fatal(a)
	}
	a = run(ic.Observation{Error: "upstream unavailable"})
	if a.NextIntervalSeconds != 3600 {
		t.Fatal(a)
	}
	a = run(good)
	if a.ProbePhase != "recovering" || a.NextIntervalSeconds != 300 {
		t.Fatal(a)
	}
	a = run(ic.Observation{Error: "timeout"})
	if a.RecoverStreak != 0 || a.NextIntervalSeconds != 3600 || !a.QualityPaused {
		t.Fatal(a)
	}
	for i, want := range []int{300, 120, 1800} {
		a = run(good)
		if a.NextIntervalSeconds != want || a.QualityPaused != (i < 2) {
			t.Fatal(i, a)
		}
	}
	if a.PauseBackoffSeconds != 3600 {
		t.Fatal(a)
	}
	a = run(good)
	if a.PauseBackoffSeconds != 0 {
		t.Fatal(a)
	}
	a = run(bad)
	// Claim then expire a real lease: it must use backoff, not fixed retry_seconds.
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now)
	r, e := db.ClaimIntelligenceCheck(ctx, "expired", now, time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	expires := time.UnixMilli(r.LeaseUntil + 1)
	if n, e := db.ExpireIntelligenceCheckLeases(ctx, expires); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	list, _, _ := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
	a = list[0]
	if a.NextIntervalSeconds != 3600 || a.NextDueAt.Before(expires.Add(3240*time.Second)) || a.NextDueAt.After(expires.Add(time.Hour)) {
		t.Fatal(a)
	}
	bars, e := db.IntelligenceCheckBars(ctx, []int64{id})
	if e != nil {
		t.Fatal(e)
	}
	if bars[id].ProbePhase != "backoff" || bars[id].NextDueAt == nil {
		t.Fatal(bars)
	}
	// Reducing the cap reschedules a previously distant deadline.
	cfg.PauseMaxSeconds = 1800
	if _, e = db.SaveIntelligenceCheckSettings(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	list, _, _ = db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
	if list[0].PauseBackoffSeconds != 1800 || list[0].NextDueAt.After(time.Now().Add(1801*time.Second)) {
		t.Fatal(list)
	}
}

func TestIntelligenceCheckAdaptiveCredentialFence(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	id := intelligenceAccount(t, db)
	cfg, _ := db.GetIntelligenceCheckSettings(ctx)
	cfg.Enabled, cfg.AdaptiveEnabled = true, true
	db.SaveIntelligenceCheckSettings(ctx, cfg)
	now := time.Now().UTC()
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now)
	r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Error: "token revoked", CredentialRejected: true}, now.Add(time.Second))
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now.Add(72*time.Hour), 100); e != nil || n != 0 {
		t.Fatal("revoked credential was scheduled", n, e)
	}
	bars, _ := db.IntelligenceCheckBars(ctx, []int64{id})
	if bars[id].ProbePhase != "credential_blocked" || bars[id].NextDueAt != nil {
		t.Fatal(bars)
	}
	// An explicit manual check is still allowed, but a repair during execution
	// fences its old result from blocking the replacement credentials.
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now.Add(time.Minute))
	r, e = db.ClaimIntelligenceCheck(ctx, "worker", now.Add(time.Minute), time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	_, e = db.conn.ExecContext(ctx, `UPDATE accounts SET credential_generation=credential_generation+1 WHERE id=$1`, id)
	if e != nil {
		t.Fatal(e)
	}
	db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Error: "old token revoked", CredentialRejected: true}, now.Add(61*time.Second))
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now.Add(72*time.Hour), 100); e != nil || n != 1 {
		t.Fatal("replacement credential not scheduled", n, e)
	}
	r, e = db.ClaimIntelligenceCheck(ctx, "new", now.Add(72*time.Hour), time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
}

func TestIntelligenceCheckAdaptiveLegacySettings(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	cfg, _ := db.GetIntelligenceCheckSettings(ctx)
	raw, _ := json.Marshal(cfg)
	var legacy map[string]any
	json.Unmarshal(raw, &legacy)
	for _, key := range []string{"adaptive_enabled", "pause_initial_seconds", "pause_max_seconds", "recovery_initial_seconds", "recovery_min_seconds"} {
		delete(legacy, key)
	}
	raw, _ = json.Marshal(legacy)
	if _, e := db.conn.ExecContext(ctx, `UPDATE intelligence_check_settings SET config_json=$1 WHERE id=1`, string(raw)); e != nil {
		t.Fatal(e)
	}
	cfg, e := db.GetIntelligenceCheckSettings(ctx)
	if e != nil || cfg.AdaptiveEnabled || cfg.PauseMaxSeconds != 172800 || cfg.RecoverThreshold != 1 {
		t.Fatal(cfg, e)
	}
}
