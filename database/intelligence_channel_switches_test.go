package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ic "github.com/codex2api/internal/intelligencecheck"
)

// TestIntelligenceChannelSwitchesFilterBeforeLimitAndClaim checks source selection and default model snapshots.
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
		wantModel := map[string]string{"codex": s.GPTModel, "claude": s.ClaudeModel, "grok": "grok-4.5", "antigravity": "gemini-3.8-flash-medium"}[run.Channel]
		wantEffort := ""
		if run.Channel == "codex" {
			wantEffort = s.ReasoningEffort
		}
		if run.AccountID != ids[run.Channel] || run.Model != wantModel || run.ReasoningEffort != wantEffort {
			t.Fatalf("wrong source/model: %+v", run)
		}
		seen[run.Channel] = true
	}
	if len(seen) != 4 {
		t.Fatalf("source isolation lost: %+v", seen)
	}
}

// TestIntelligenceChannelSwitchesLegacySaveAndManualOverride preserves old clients and explicit manual checks.
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

// TestIntelligenceChannelSwitchesNewAccountDefaults verifies source-aware account enrollment.
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

// TestIntelligenceChannelSwitchesDisabledSourceDoesNotConflictWithTarget scopes automatic target validation.
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

// TestIntelligenceChannelSwitchesClaimBacklog filters disabled sources before the 100-candidate claim limit.
func TestIntelligenceChannelSwitchesClaimBacklog(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s.Channels = map[string]bool{"codex": true, "grok": true}
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		id, err := db.InsertAccountWithUpstream(ctx, "backlog", "openai", "api", map[string]interface{}{"upstream_type": "grok"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 100); err != nil || n != 100 {
		t.Fatalf("backlog=%d %v", n, err)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	s.Channels["grok"] = false
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	id := intelligenceAccount(t, db)
	if err := db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1); err != nil || n != 1 {
		t.Fatalf("eligible=%d %v", n, err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil || run.AccountID != id {
		t.Fatalf("eligible blocked by backlog: %+v %v", run, err)
	}
}

// TestIntelligenceChannelSwitchesModelsRefreshAndClear covers claim-time snapshots and explicit JSON clears.
func TestIntelligenceChannelSwitchesModelsRefreshAndClear(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s.Channels = map[string]bool{"grok": true, "antigravity": true}
	s.GrokModel, s.AntigravityModel = "grok-old", "gemini-old"
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"grok", "antigravity"} {
		id, err := db.InsertAccountWithUpstream(ctx, channel, "openai", "api", map[string]interface{}{"upstream_type": channel}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 10); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	s.GrokModel, s.AntigravityModel = "grok-custom", "gemini-custom"
	s, err := db.SaveIntelligenceCheckSettings(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
		if err != nil || run == nil {
			t.Fatal(run, err)
		}
		want := map[string]string{"grok": "grok-custom", "antigravity": "gemini-custom"}[run.Channel]
		if run.Model != want || run.ReasoningEffort != "" {
			t.Fatalf("stale/wrong model: %+v", run)
		}
	}
	// A JSON round trip exercises the same field-presence semantics as the API.
	raw, _ := json.Marshal(s)
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "grok_model")
	delete(fields, "antigravity_model")
	raw, _ = json.Marshal(fields)
	var request ic.Settings
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	s, err = db.SaveIntelligenceCheckSettings(ctx, request)
	if err != nil || s.GrokModel != "grok-custom" || s.AntigravityModel != "gemini-custom" {
		t.Fatal(s, err)
	}
	fields["revision"], _ = json.Marshal(s.Revision)
	fields["grok_model"], fields["antigravity_model"] = json.RawMessage(`""`), json.RawMessage(`""`)
	raw, _ = json.Marshal(fields)
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	s, err = db.SaveIntelligenceCheckSettings(ctx, request)
	if err != nil || s.GrokModel != "" || s.AntigravityModel != "" {
		t.Fatalf("clear failed: %+v %v", s, err)
	}
	for channel, want := range map[string]string{"grok": "grok-4.5", "antigravity": "gemini-3.8-flash-medium"} {
		if got, _ := s.ModelForChannel(channel); got != want {
			t.Fatalf("default %s=%s", channel, got)
		}
	}
}

// TestIntelligenceChannelSwitchesSourcePrecedence gives explicit upstream credentials priority over legacy platforms.
func TestIntelligenceChannelSwitchesSourcePrecedence(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	for _, tc := range []struct{ upstream, platform, want string }{
		{"grok", "google", "grok"}, {"antigravity", "xai", "antigravity"},
		{"claude", "google", "claude"}, {"openai_responses", "google", "codex"},
		{"", "google", "antigravity"}, {"", "xai", "grok"}, {"", "anthropic", "claude"},
	} {
		id, err := db.InsertAccountWithUpstream(ctx, tc.want, tc.platform, "api", map[string]interface{}{"upstream_type": tc.upstream}, "")
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
		if err != nil || len(rows) != 1 || rows[0].Channel != tc.want {
			t.Fatalf("%+v: %+v %v", tc, rows, err)
		}
	}
}

// TestIntelligenceChannelSwitchesModelValidation permits defaults but rejects blank and oversized overrides.
func TestIntelligenceChannelSwitchesModelValidation(t *testing.T) {
	for _, channel := range []string{"grok", "antigravity"} {
		for _, model := range []string{"", "custom-model", " \t\n", strings.Repeat("x", 256)} {
			s := ic.DefaultSettings()
			if channel == "grok" {
				s.GrokModel = model
			} else {
				s.AntigravityModel = model
			}
			err := ic.ValidateSettings(s)
			wantError := model != "" && (strings.TrimSpace(model) == "" || len(model) > 255)
			if (err != nil) != wantError {
				t.Fatalf("%s model=%q err=%v", channel, model, err)
			}
		}
	}
}

// TestIntelligenceChannelSwitchesAntigravityCredentialFence stops rejected OAuth checks even without adaptive scheduling.
func TestIntelligenceChannelSwitchesAntigravityCredentialFence(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled, s.AdaptiveEnabled = true, false
	s.Channels = map[string]bool{"antigravity": true}
	if _, err := db.SaveIntelligenceCheckSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccountWithUpstream(ctx, "rejected", "google", "oauth", map[string]interface{}{"upstream_type": "antigravity"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetIntelligenceCheckEnabled(ctx, []int64{id}, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	run, err := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if err != nil || run == nil {
		t.Fatal(run, err)
	}
	if ok, err := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Error: "final 401", CredentialRejected: true}, now.Add(time.Second)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(72*time.Hour), 1); err != nil || n != 0 {
		t.Fatalf("rejected credential rescheduled: %d %v", n, err)
	}
	if _, err = db.conn.ExecContext(ctx, `UPDATE accounts SET credential_generation=credential_generation+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := db.EnqueueDueIntelligenceChecks(ctx, now.Add(72*time.Hour), 1); err != nil || n != 1 {
		t.Fatalf("repaired credential blocked: %d %v", n, err)
	}
}
