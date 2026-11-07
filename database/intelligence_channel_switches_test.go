package database

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestIntelligenceChannelSwitchesFilterBeforeLimitAndClaim(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = true
	s.Channels = map[string]bool{"codex": false, "claude": false, "grok": false, "antigravity": false}
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, channel := range []string{"claude", "grok", "antigravity", "codex"} {
		id, err := db.InsertAccountWithUpstream(ctx, channel, "openai", "api", map[string]interface{}{"upstream_type": channel}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
			t.Fatal(err)
		}
		ids[channel] = id
	}
	now := time.Now()
	count, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1)
	if err != nil || count != 0 {
		t.Fatalf("all off: count=%d err=%v", count, err)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	s.Channels["codex"] = true
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	count, err = db.EnqueueDueIntelligenceChecks(ctx, now, 1)
	if err != nil || count != 1 {
		t.Fatalf("GPT only: count=%d err=%v", count, err)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	s.Channels["codex"] = false
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run != nil {
		t.Fatalf("queued GPT ran after disabling: %+v %v", run, err)
	}
	bars, err := db.IntelligenceCheckBars(ctx, []int64{ids["codex"]})
	if err != nil || bars[ids["codex"]].ActiveRunID != 0 || bars[ids["codex"]].NextDueAt != nil {
		t.Fatalf("disabled source must not show an active spinner/next due time: %+v %v", bars, err)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	for channel := range ids {
		s.Channels[channel] = true
	}
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	count, err = db.EnqueueDueIntelligenceChecks(ctx, now, 100)
	if err != nil || count != 3 {
		t.Fatalf("other three did not enqueue: %d %v", count, err)
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		run, err = db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
		if err != nil || run == nil {
			t.Fatalf("claim %d: %+v %v", i, run, err)
		}
		if run.AccountID != ids[run.Channel] || run.Model == "" {
			t.Fatalf("wrong source/model: %+v", run)
		}
		seen[run.Channel] = true
	}
	if len(seen) != 4 {
		t.Fatalf("source isolation lost: %+v", seen)
	}
}

func TestIntelligenceChannelSwitchesLegacySaveAndManualOverride(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	if !s.ChannelEnabled("codex") || !s.ChannelEnabled("claude") || s.ChannelEnabled("grok") || s.ChannelEnabled("antigravity") {
		t.Fatal("legacy defaults changed")
	}
	s.Channels = map[string]bool{"codex": false, "claude": false, "grok": false, "antigravity": false}
	saved, err := db.SaveIntelligenceCheckSettings(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	saved.Channels = nil // Old UI saving unrelated fields must preserve switches.
	saved, err = db.SaveIntelligenceCheckSettings(ctx, saved)
	if err != nil || saved.ChannelEnabled("codex") {
		t.Fatalf("legacy client reset switches: %+v %v", saved, err)
	}
	id := intelligenceAccount(t, db)
	if _, err = db.EnqueueIntelligenceChecks(ctx, []int64{id}, time.Now()); err != nil {
		t.Fatal(err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "manual", time.Now(), time.Minute)
	if err != nil || run == nil || run.Source != "manual" {
		t.Fatalf("manual override: %+v %v", run, err)
	}
}

func TestIntelligenceChannelSwitchesNewAccountDefaults(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	for _, channel := range []string{"codex", "claude", "grok", "antigravity"} {
		s, err := db.GetIntelligenceCheckSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s.Channels = map[string]bool{"codex": false, "claude": false, "grok": false, "antigravity": false}
		s.Channels[channel] = true
		s, err = db.SaveIntelligenceCheckSettings(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{true, false} {
			if !enabled {
				s.Channels[channel] = false
				s, err = db.SaveIntelligenceCheckSettings(ctx, s)
				if err != nil {
					t.Fatal(err)
				}
			}
			id, err := db.InsertAccountWithUpstream(ctx, channel, "openai", "api", map[string]interface{}{"upstream_type": channel}, "")
			if err != nil {
				t.Fatal(err)
			}
			rows, total, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
			if err != nil || total != 1 || rows[0].Enabled != enabled || rows[0].Channel != channel {
				t.Fatalf("new %s account opt-in=%v: %+v %v", channel, enabled, rows, err)
			}
		}
	}
}

func TestIntelligenceChannelSwitchesDisabledSourceDoesNotConflictWithTarget(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	intelligenceAccount(t, db)
	claude, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{"upstream_type": "claude"}, "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := db.CreateAccountGroup(ctx, "Codex target", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Action, s.TargetGroupID = "switch_group", target
	s.Channels = map[string]bool{"codex": true, "claude": false}
	if _, err = db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	rows, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: claude})
	if err != nil || len(rows) != 1 || rows[0].ConfigError != "" {
		t.Fatalf("disabled source conflict: %+v %v", rows, err)
	}
	if _, err = db.EnqueueIntelligenceChecks(ctx, []int64{claude}, time.Now()); err == nil {
		t.Fatal("manual submission must still enforce the target channel")
	}
}
