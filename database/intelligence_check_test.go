package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	ic "github.com/codex2api/internal/intelligencecheck"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func intelligenceDB(t *testing.T) *DB {
	t.Helper()
	var db *DB
	var e error
	if strings.Contains(t.Name(), "TestIntelligenceCheckPostgres/") {
		schema := fmt.Sprintf("ic_test_%d", time.Now().UnixNano())
		parsed, err := url.Parse(os.Getenv("CODEX2API_TEST_POSTGRES_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("options", "-c search_path="+schema+",public")
		parsed.RawQuery = q.Encode()
		db, e = New("postgres", parsed.String(), schema)
		if e == nil {
			t.Cleanup(func() {
				cleanup, ce := sql.Open("pgx", os.Getenv("CODEX2API_TEST_POSTGRES_DSN"))
				if ce == nil {
					defer cleanup.Close()
					cleanup.ExecContext(context.Background(), "DROP SCHEMA "+quotePostgresIdent(schema)+" CASCADE")
				}
			})
		}
	} else {
		db, e = New("sqlite", filepath.Join(t.TempDir(), "ic.db"))
	}

	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func intelligenceAccount(t *testing.T, db *DB) int64 {
	t.Helper()
	id, e := db.InsertAccount(context.Background(), "test", "local-fixture", "")
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func intelligenceRun(t *testing.T, db *DB, id int64, now time.Time, out string) *IntelligenceCheckRun {
	t.Helper()
	ctx := context.Background()
	_, e := db.EnqueueIntelligenceChecks(ctx, []int64{id}, now)
	if e != nil {
		t.Fatal(e)
	}
	run, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || run == nil {
		t.Fatalf("claim %v %v", run, e)
	}
	ok, e := db.FinishIntelligenceCheck(ctx, run.ID, "worker", ic.Observation{Complete: true, Output: out, DurationMS: 1}, now.Add(time.Second))
	if e != nil || !ok {
		t.Fatalf("finish %v %v", ok, e)
	}
	return run
}
func TestIntelligenceCheckSettingsAndMigration(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	s, e := db.GetIntelligenceCheckSettings(ctx)
	if e != nil || s.Enabled || s.Questions[0].ExpectedAnswer != "21" {
		t.Fatal(s, e)
	}
	if e = db.ensureIntelligenceCheckSchema(ctx); e != nil {
		t.Fatal(e)
	}
	s.Concurrency = 2
	saved, e := db.SaveIntelligenceCheckSettings(ctx, s)
	if e != nil || saved.Revision != s.Revision+1 {
		t.Fatal(saved, e)
	}
	if _, e = db.SaveIntelligenceCheckSettings(ctx, s); !errors.Is(e, ErrIntelligenceCheckRevisionConflict) {
		t.Fatal(e)
	}
	id := intelligenceAccount(t, db)
	a, _, e := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{})
	if e != nil || len(a) != 1 || a[0].AccountID != id || !a[0].Enabled {
		t.Fatal(a, e)
	}
	saved.NewAccountsEnabled = false
	if _, e = db.SaveIntelligenceCheckSettings(ctx, saved); e != nil {
		t.Fatal(e)
	}
	intelligenceAccount(t, db)
	a, _, e = db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{})
	if e != nil || len(a) != 2 || a[1].Enabled {
		t.Fatal(a, e)
	}
}
func TestIntelligenceCheckClaimDedupLeaseAndRetention(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, b := intelligenceAccount(t, db), intelligenceAccount(t, db)
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Concurrency = 1
	s.Retention = 2
	s.Questions = append(s.Questions, ic.Question{ID: "q2", Name: "second", Prompt: "x", ExpectedAnswer: "21", MatchMode: "exact", MaxDurationMS: 10, Enabled: true})
	db.SaveIntelligenceCheckSettings(ctx, s)
	ids, e := db.EnqueueIntelligenceChecks(ctx, []int64{a, b, a}, now)
	if e != nil || len(ids) != 2 {
		t.Fatal(ids, e)
	}
	again, e := db.EnqueueIntelligenceChecks(ctx, []int64{a}, now)
	if e != nil || again[0] != ids[0] {
		t.Fatal(again, e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claimed []*IntelligenceCheckRun
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Second)
			if e != nil {
				t.Error(e)
			}
			if r != nil {
				mu.Lock()
				claimed = append(claimed, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 1 {
		t.Fatal(len(claimed))
	}
	r := claimed[0]
	if ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "wrong", ic.Observation{Complete: true, Output: "21"}, now); e != nil || ok {
		t.Fatal(ok, e)
	}
	now = now.Add(220 * time.Second)
	n, e := db.ExpireIntelligenceCheckLeases(ctx, now.Add(2*time.Second))
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Complete: true, Output: "21"}, now.Add(3*time.Second)); e != nil || ok {
		t.Fatal(ok, e)
	}
	r, e = db.ClaimIntelligenceCheck(ctx, "worker", now.Add(3*time.Second), time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	if e = db.RequeueIntelligenceCheck(ctx, r.ID, "worker", now.Add(3*time.Second)); e != nil {
		t.Fatal(e)
	}
	if r, e = db.ClaimIntelligenceCheck(ctx, "worker", now.Add(3*time.Second), time.Minute); e != nil || r != nil {
		t.Fatal("busy requeue delay", r, e)
	}
	r, e = db.ClaimIntelligenceCheck(ctx, "worker", now.Add(10*time.Second), time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Complete: true, Output: "21"}, now.Add(11*time.Second))
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e = db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Complete: true, Output: "29"}, now.Add(12*time.Second)); e != nil || ok {
		t.Fatal(ok, e)
	}
	for i := 0; i < 4; i++ {
		intelligenceRun(t, db, a, now.Add(time.Duration(i+20)*time.Second), "21")
	}
	runs, total, e := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: a})
	if e != nil || total != 2 || len(runs) != 2 || runs[0].Question.ID == runs[1].Question.ID {
		t.Fatal(runs, total, e)
	}
}
func TestIntelligenceCheckOwnershipManualAndConfig(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := intelligenceAccount(t, db)
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Action = "pause"
	s.DegradeThreshold = 1
	s, _ = db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, id, now, "29")
	paused, e := db.IsIntelligenceCheckPaused(ctx, id)
	if e != nil || !paused {
		t.Fatal(paused, e)
	}
	if e = db.SetAccountEnabled(ctx, id, true); e != nil {
		t.Fatal(e)
	}
	paused, _ = db.IsIntelligenceCheckPaused(ctx, id)
	if paused {
		t.Fatal("same-value manual enable did not clear ownership")
	}
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now.Add(time.Minute))
	r, _ := db.ClaimIntelligenceCheck(ctx, "worker", now.Add(time.Minute), time.Minute)
	db.SetAccountEnabled(ctx, id, false)
	db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Complete: true, Output: "29"}, now.Add(61*time.Second))
	paused, _ = db.IsIntelligenceCheckPaused(ctx, id)
	if paused {
		t.Fatal("stale manual revision applied")
	}
	db.SetAccountEnabled(ctx, id, true)
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now.Add(2*time.Minute))
	r, _ = db.ClaimIntelligenceCheck(ctx, "worker", now.Add(2*time.Minute), time.Minute)
	s.IntervalSeconds++
	db.SaveIntelligenceCheckSettings(ctx, s)
	db.FinishIntelligenceCheck(ctx, r.ID, "worker", ic.Observation{Complete: true, Output: "29"}, now.Add(121*time.Second))
	paused, _ = db.IsIntelligenceCheckPaused(ctx, id)
	if paused {
		t.Fatal("stale config applied")
	}
	intelligenceRun(t, db, id, now.Add(3*time.Minute), "29")
	db.BatchUpdateAccountMetadata(ctx, []int64{id}, BatchAccountMetadataUpdate{Enabled: OptionalBool{Set: true, Value: true}})
	paused, _ = db.IsIntelligenceCheckPaused(ctx, id)
	if paused {
		t.Fatal("batch ownership")
	}
}
func TestIntelligenceCheckRestoreSurvivingGroups(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := intelligenceAccount(t, db)
	g1, _ := db.CreateAccountGroup(ctx, "first", "", "", 0, 0, sql.NullInt64{})
	g2, _ := db.CreateAccountGroup(ctx, "second", "", "", 0, 0, sql.NullInt64{})
	target, _ := db.CreateAccountGroup(ctx, "target", "", "", 0, 0, sql.NullInt64{})
	db.SetAccountGroups(ctx, id, []int64{g1, g2})
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Action = "switch_group"
	s.TargetGroupID = target
	s.DegradeThreshold = 1
	s, _ = db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, id, now, "29")
	groups, _ := db.GetAccountGroupIDs(ctx, id)
	if len(groups) != 1 || groups[0] != target {
		t.Fatal(groups)
	}
	db.DeleteAccountGroup(ctx, g2)
	s.Action = "none"
	db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, id, now.Add(time.Minute), "21")
	groups, _ = db.GetAccountGroupIDs(ctx, id)
	if len(groups) != 1 || groups[0] != g1 {
		t.Fatal(groups)
	}
	s, _ = db.GetIntelligenceCheckSettings(ctx)
	s.Action = "switch_group"
	db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, id, now.Add(2*time.Minute), "29")
	db.SetAccountGroups(ctx, id, []int64{target})
	intelligenceRun(t, db, id, now.Add(3*time.Minute), "21")
	groups, _ = db.GetAccountGroupIDs(ctx, id)
	if len(groups) != 1 || groups[0] != target {
		t.Fatal("manual same-value overwritten", groups)
	}
}

func TestIntelligenceCheckSchedulerMetadataManualOverride(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := intelligenceAccount(t, db)
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Action = "pause"
	s.DegradeThreshold = 1
	db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, id, now, "29")
	if err := db.UpdateAccountSchedulerMetadata(ctx, id, OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{}, OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{Set: true}, OptionalString{}, nil); err != nil {
		t.Fatal(err)
	}
	paused, _ := db.IsIntelligenceCheckPaused(ctx, id)
	if !paused {
		t.Fatal("manual scheduler groups edit released owned pause")
	}
}
func TestIntelligenceCheckDueFiltersBarsAndDisabledPause(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := intelligenceAccount(t, db)
	b := intelligenceAccount(t, db)
	grok, e := db.InsertAccountWithUpstream(ctx, "grok", "xai", "api", map[string]interface{}{"upstream_type": "grok"}, "")
	if e != nil {
		t.Fatal(e)
	}
	var grokEnabled bool
	if e = db.conn.QueryRowContext(ctx, "SELECT enabled FROM intelligence_check_accounts WHERE account_id=$1", grok).Scan(&grokEnabled); e != nil || grokEnabled {
		t.Fatalf("Grok should remain opted out by default: %v %v", grokEnabled, e)
	}
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now, 5); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s.Action = "pause"
	s.DegradeThreshold = 1
	s, _ = db.SaveIntelligenceCheckSettings(ctx, s)
	db.SetIntelligenceCheckEnabled(ctx, []int64{b}, false)
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now, 5); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	r, e := db.ClaimIntelligenceCheck(ctx, "owner", now, time.Second)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	if r.LeaseUntil < now.Add(time.Duration(r.Settings.TimeoutSeconds+30)*time.Second).UnixMilli() {
		t.Fatal("lease too short for frozen timeout")
	}
	if ok, e := db.FinishIntelligenceCheck(ctx, r.ID, "owner", ic.Observation{Complete: true, Output: "29"}, now.Add(time.Second)); e != nil || !ok {
		t.Fatal(ok, e)
	}
	s.Action = "none"
	db.SaveIntelligenceCheckSettings(ctx, s)
	intelligenceRun(t, db, a, now.Add(time.Minute), "21")
	paused, _ := db.IsIntelligenceCheckPaused(ctx, a)
	if !paused {
		t.Fatal("disabled pause automatically recovered")
	}
	bars, e := db.IntelligenceCheckBars(ctx, []int64{a, b})
	if e != nil || len(bars[a].Records) != 2 || bars[a].Records[0].Status != "degraded" || !bars[a].QualityPaused || bars[b].Enabled {
		t.Fatal(bars, e)
	}
	rows, total, e := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: a})
	if e != nil || total != 1 || len(rows) != 1 {
		t.Fatal(rows, total, e)
	}
	history, total, e := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: a, Status: "normal", Channel: "codex", Search: "test"})
	if e != nil || total != 1 || len(history) != 1 {
		t.Fatal(history, total, e)
	}
}

func TestIntelligenceCheckPostgres(t *testing.T) {
	if os.Getenv("CODEX2API_TEST_POSTGRES_DSN") == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	for name, run := range map[string]func(*testing.T){"source_switches": TestIntelligenceChannelSwitchesFilterBeforeLimitAndClaim, "source_legacy": TestIntelligenceChannelSwitchesLegacySaveAndManualOverride, "source_defaults": TestIntelligenceChannelSwitchesNewAccountDefaults, "source_target": TestIntelligenceChannelSwitchesDisabledSourceDoesNotConflictWithTarget, "adaptive_lifecycle": TestIntelligenceCheckAdaptiveLifecycle, "adaptive_credentials": TestIntelligenceCheckAdaptiveCredentialFence, "adaptive_legacy": TestIntelligenceCheckAdaptiveLegacySettings, "new_defaults": TestIntelligenceCheckNewAccountDefaults, "concurrent_defaults": TestIntelligenceCheckNewAccountConcurrentSettings, "default_upgrade": TestIntelligenceCheckDefaultTriggerUpgrade, "conflict_batches": TestIntelligenceCheckTargetConflictBatches, "missing_default_target": TestIntelligenceCheckNewAccountWithMissingTarget, "settings": TestIntelligenceCheckSettingsAndMigration, "claims": TestIntelligenceCheckClaimDedupLeaseAndRetention, "ownership": TestIntelligenceCheckOwnershipManualAndConfig, "groups": TestIntelligenceCheckRestoreSurvivingGroups, "metadata": TestIntelligenceCheckSchedulerMetadataManualOverride, "due": TestIntelligenceCheckDueFiltersBarsAndDisabledPause, "queue": TestIntelligenceCheckDisabledAutomaticQueueDoesNotBlockManual, "stale_expiry": TestIntelligenceCheckExpiredOldConfigDoesNotOverwriteNewState, "promote": TestIntelligenceCheckManualPromotesExistingAutomaticQueue, "manual_groups": TestIntelligenceCheckManualGroupOwnershipIntents, "targets": TestIntelligenceCheckTargetCompatibilityPreflight, "claude_effort": TestIntelligenceCheckClaudeSnapshotEffort, "rotation": TestIntelligenceCheckQuestionRotationAfterDisablingEarlierQuestion, "restore_channel": TestIntelligenceCheckRestoreSkipsChangedGroupChannel, "pending_recovery": TestIntelligenceCheckPendingRecoveryIgnoresNewTargetRemoval, "invalid_claim": TestIntelligenceCheckClaimRejectsRemovedTargetBeforeSending, "legacy_groups": TestIntelligenceCheckRestorePreservesUnchangedLegacyMixedGroup, "snapshot_size": TestIntelligenceCheckRunSnapshotOmitsUnrelatedQuestionBank, "claim_refresh": TestIntelligenceCheckClaimRefreshesQueuedAndBusySnapshots, "bars_alloc": TestIntelligenceCheckBarsAvoidHistoryBodyAllocations, "bars_active": TestIntelligenceCheckBarsTrackActiveRun, "bars_disabled_automatic": TestIntelligenceCheckBarsAllowManualAfterDisablingAutomatic, "disabled_account": TestIntelligenceCheckAutomaticSkipsManuallyDisabledAccounts} {
		t.Run(name, run)
	}
}

func TestIntelligenceCheckDisabledAutomaticQueueDoesNotBlockManual(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s, _ = db.SaveIntelligenceCheckSettings(ctx, s)
	for i := 0; i < 101; i++ {
		intelligenceAccount(t, db)
	}
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now, 200); e != nil || n != 101 {
		t.Fatal(n, e)
	}
	s.Enabled = false
	db.SaveIntelligenceCheckSettings(ctx, s)
	manual := intelligenceAccount(t, db)
	db.EnqueueIntelligenceChecks(ctx, []int64{manual}, now)
	r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil || r.AccountID != manual {
		t.Fatalf("manual task blocked by disabled automatic queue: %+v %v", r, e)
	}
}
func TestIntelligenceCheckAutomaticSkipsManuallyDisabledAccounts(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	if _, e := db.SaveIntelligenceCheckSettings(ctx, s); e != nil {
		t.Fatal(e)
	}
	active := intelligenceAccount(t, db)
	disabled := intelligenceAccount(t, db)
	if e := db.SetAccountEnabled(ctx, disabled, false); e != nil {
		t.Fatal(e)
	}
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now, 10); e != nil || n != 1 {
		t.Fatal("disabled account was scheduled", n, e)
	}
	r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil || r.AccountID != active {
		t.Fatalf("expected the enabled account, got %+v %v", r, e)
	}
	// A run queued before the account was disabled must not be sent either.
	if e = db.SetAccountEnabled(ctx, disabled, true); e != nil {
		t.Fatal(e)
	}
	if n, e := db.EnqueueDueIntelligenceChecks(ctx, now, 10); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if e = db.SetAccountEnabled(ctx, disabled, false); e != nil {
		t.Fatal(e)
	}
	if r, e = db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute); e != nil || r != nil {
		t.Fatalf("automatic check claimed for a disabled account: %+v %v", r, e)
	}
	// An explicit manual check still runs against a disabled account.
	if _, e = db.EnqueueIntelligenceChecks(ctx, []int64{disabled}, now); e != nil {
		t.Fatal(e)
	}
	r, e = db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil || r.AccountID != disabled || r.Source != "manual" {
		t.Fatalf("manual check blocked by account disable: %+v %v", r, e)
	}
}
func TestIntelligenceCheckExpiredOldConfigDoesNotOverwriteNewState(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := intelligenceAccount(t, db)
	db.EnqueueIntelligenceChecks(ctx, []int64{id}, now)
	r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil {
		t.Fatal(r, e)
	}
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.IntervalSeconds++
	db.SaveIntelligenceCheckSettings(ctx, s)
	if n, e := db.ExpireIntelligenceCheckLeases(ctx, now.Add(10*time.Minute)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	states, _, e := db.ListIntelligenceCheckAccounts(ctx, IntelligenceCheckAccountFilter{AccountID: id})
	if e != nil || states[0].Status != "unknown" || states[0].NextDueAt != nil {
		t.Fatal("expired stale execution changed current state", states, e)
	}
}
func TestIntelligenceCheckManualPromotesExistingAutomaticQueue(t *testing.T) {
	db := intelligenceDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := intelligenceAccount(t, db)
	s, _ := db.GetIntelligenceCheckSettings(ctx)
	s.Enabled = true
	s, _ = db.SaveIntelligenceCheckSettings(ctx, s)
	db.EnqueueDueIntelligenceChecks(ctx, now, 1)
	before, _, _ := db.ListIntelligenceCheckRuns(ctx, IntelligenceCheckRunFilter{AccountID: id})
	s.Enabled = false
	db.SaveIntelligenceCheckSettings(ctx, s)
	ids, e := db.EnqueueIntelligenceChecks(ctx, []int64{id}, now)
	if e != nil || len(ids) != 1 || ids[0] != before[0].ID {
		t.Fatal(ids, e)
	}
	r, e := db.ClaimIntelligenceCheck(ctx, "worker", now, time.Minute)
	if e != nil || r == nil || r.Source != "manual" {
		t.Fatal("manual request remains blocked by global switch", r, e)
	}
}
