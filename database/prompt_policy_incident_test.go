package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	promptPolicyDDLDriverOnce sync.Once
	promptPolicyDDLQueryMu    sync.Mutex
	promptPolicyDDLQueries    []string
)

type promptPolicyDDLDriver struct{}
type promptPolicyDDLConn struct{}

func (promptPolicyDDLDriver) Open(string) (driver.Conn, error) { return promptPolicyDDLConn{}, nil }
func (promptPolicyDDLConn) Prepare(string) (driver.Stmt, error) {
	return nil, nil
}
func (promptPolicyDDLConn) Close() error              { return nil }
func (promptPolicyDDLConn) Begin() (driver.Tx, error) { return nil, nil }
func (promptPolicyDDLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	promptPolicyDDLQueryMu.Lock()
	promptPolicyDDLQueries = append(promptPolicyDDLQueries, query)
	promptPolicyDDLQueryMu.Unlock()
	return driver.RowsAffected(0), nil
}

func promptPolicyTestFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newPromptPolicySQLiteTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "prompt-policy.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func promptPolicyTestInputs(incidentID string) (PromptPolicyIncidentInput, PromptRuleCandidateInput, PromptRuleCandidateEvidenceInput) {
	observedAt := time.Now().UTC().Truncate(time.Millisecond)
	zero := 0
	incident := PromptPolicyIncidentInput{
		IncidentID: incidentID, RequestCorrelationID: "request-1", AttemptIndex: 2, Transport: "sse",
		Endpoint: "/v1/responses", Protocol: "responses", Provider: "openai", Model: "gpt-5.4",
		StatusCode: 400, AccountID: 7, AccountName: "account@example.com", AccountPlatform: "openai", AccountGroupIDs: []int64{4, 7}, AccountGroupNames: []string{"打铁", "Team"},
		APIKeyID: 9, APIKeyName: "test", APIKeyAllowedGroupIDs: []int64{1, 4, 7}, APIKeyAllowedGroupNames: []string{"示例平台", "打铁", "Team"}, UpstreamErrorCode: "cyber_policy",
		UpstreamError: `{"error":{"code":"cyber_policy"}}`, LocalEvaluationState: PromptPolicyEvaluationCompleted,
		LocalOutcome: PromptPolicyOutcomeNoHit, LocalAction: "allow", LocalScore: &zero, LocalRawScore: &zero,
		LocalAuditScore: &zero, LocalAuditRawScore: &zero, LocalThreshold: 50, LocalMode: "block",
		LocalMatchedPatterns: "[]", PromptFingerprint: promptPolicyTestFingerprint("prompt"), PromptPreview: "prompt", PromptText: "prompt",
		ObservedAt: observedAt,
	}
	candidate := PromptRuleCandidateInput{
		Fingerprint: incident.PromptFingerprint, Kind: PromptRuleCandidateKindEvidence,
		Source: PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: incident.PromptPreview,
	}
	evidence := PromptRuleCandidateEvidenceInput{
		SourceKind: PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: "request-1",
		SourceRefHash: promptPolicyTestFingerprint(incidentID), MetadataJSON: `{}`,
		Protocol: "responses", Provider: "openai", Model: "gpt-5.4", APIKeyID: 9, APIKeyName: "test", ObservedAt: observedAt,
	}
	return incident, candidate, evidence
}

func TestPromptPolicyIncidentPersistsNullableScoresAndExactEvidenceLink(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("incident-zero")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	got, err := db.GetPromptPolicyIncident(ctx, incident.IncidentID)
	if err != nil {
		t.Fatalf("GetPromptPolicyIncident: %v", err)
	}
	if got.LocalScore == nil || *got.LocalScore != 0 || got.LocalAuditScore == nil || *got.LocalAuditScore != 0 {
		t.Fatalf("real zero scores were not preserved: %#v", got)
	}
	if got.LocalMiss || got.LocalComparison != PromptPolicyComparisonUpstreamOnly || got.CandidateID == 0 || got.CandidateEvidenceID == 0 {
		t.Fatalf("incident linkage/comparison = %#v", got)
	}
	if got.AccountName != incident.AccountName || len(got.AccountGroupIDs) != 2 || len(got.AccountGroupNames) != 2 || len(got.APIKeyAllowedGroupIDs) != 3 || len(got.APIKeyAllowedGroupNames) != 3 || !got.PromptAvailable {
		t.Fatalf("routing snapshot was not preserved: %#v", got)
	}
	items, err := db.ListPromptRuleCandidateEvidence(ctx, got.CandidateID, 10)
	if err != nil || len(items) != 1 || items[0].ID != got.CandidateEvidenceID || items[0].PromptPolicyIncidentID != incident.IncidentID {
		t.Fatalf("candidate evidence link items=%#v err=%v", items, err)
	}
	for _, auditReference := range []string{incident.IncidentID, incident.RequestCorrelationID} {
		incidents, total, err := db.ListPromptPolicyIncidentsPage(ctx, PromptPolicyIncidentQuery{Page: 1, PageSize: 10, Query: auditReference})
		if err != nil || total != 1 || len(incidents) != 1 || incidents[0].IncidentID != incident.IncidentID {
			t.Fatalf("audit reference %q did not locate incident: total=%d incidents=%#v err=%v", auditReference, total, incidents, err)
		}
	}

	notRun, notRunCandidate, notRunEvidence := promptPolicyTestInputs("incident-not-run")
	notRun.LocalEvaluationState = PromptPolicyEvaluationNotRun
	notRun.LocalOutcome = PromptPolicyOutcomeNoHit
	notRun.LocalScore, notRun.LocalRawScore, notRun.LocalAuditScore, notRun.LocalAuditRawScore = nil, nil, nil, nil
	notRun.PromptFingerprint = promptPolicyTestFingerprint("not-run")
	notRunCandidate.Fingerprint = notRun.PromptFingerprint
	notRunEvidence.SourceRefHash = promptPolicyTestFingerprint(notRun.IncidentID)
	if err := db.PersistPromptPolicyIncident(ctx, notRun, notRunCandidate, notRunEvidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident(not_run): %v", err)
	}
	got, err = db.GetPromptPolicyIncident(ctx, notRun.IncidentID)
	if err != nil || got.LocalScore != nil || got.LocalAuditScore != nil || got.LocalMiss {
		t.Fatalf("not_run nullable/local_miss got=%#v err=%v", got, err)
	}
}

func TestPromptPolicyIncidentCompositeTransactionRollsBack(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER fail_policy_evidence BEFORE INSERT ON prompt_rule_candidate_evidence BEGIN SELECT RAISE(ABORT, 'forced evidence failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	incident, candidate, evidence := promptPolicyTestInputs("incident-rollback")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err == nil {
		t.Fatal("PersistPromptPolicyIncident unexpectedly succeeded")
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents WHERE incident_id=$1`, incident.IncidentID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("incident transaction was not rolled back count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidates WHERE fingerprint=$1`, candidate.Fingerprint).Scan(&count); err != nil || count != 0 {
		t.Fatalf("candidate transaction was not rolled back count=%d err=%v", count, err)
	}
}

func TestPromptPolicyIncidentReconcilesAsyncShadowEvidenceInEitherWriteOrder(t *testing.T) {
	patterns := `[{"name":"malware_family","category":"malware","weight":20}]`
	shadowInput := func(correlationID string) *PromptFilterLogInput {
		return &PromptFilterLogInput{
			Source: "local_filter", Endpoint: "/v1/responses", Model: "gpt-5.4", Action: "allow", Mode: "block",
			AuditScore: 20, Threshold: 50, ReasonCode: "prompt_policy_shadow_async", PrimaryOrigin: "tool_output",
			MatchedPatterns: patterns, RequestCorrelationID: correlationID,
		}
	}
	insertShadow := func(t *testing.T, db *DB, correlationID string) {
		t.Helper()
		if err := db.InsertPromptFilterLog(context.Background(), shadowInput(correlationID)); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}
	assertReconciled := func(t *testing.T, db *DB, incidentID string) {
		t.Helper()
		got, err := db.GetPromptPolicyIncident(context.Background(), incidentID)
		if err != nil {
			t.Fatalf("GetPromptPolicyIncident: %v", err)
		}
		if got.LocalComparison != PromptPolicyComparisonLocalDetected || got.LocalOutcome != PromptPolicyOutcomeAuditHit || got.LocalMiss {
			t.Fatalf("async evidence comparison was not reconciled: %#v", got)
		}
		if got.LocalAuditScore == nil || *got.LocalAuditScore != 20 || got.LocalReasonCode != "prompt_policy_shadow_async" || got.LocalPrimaryOrigin != "tool_output" || got.LocalMatchedPatterns != patterns {
			t.Fatalf("async evidence fields were not reconciled: %#v", got)
		}
		var eventKind, comparison string
		if err := db.conn.QueryRowContext(context.Background(), `SELECT event_kind, local_comparison FROM prompt_risk_events WHERE source_type=$1 AND source_id=$2 LIMIT 1`, promptRiskSourceIncident, incidentID).Scan(&eventKind, &comparison); err != nil {
			t.Fatalf("query risk event: %v", err)
		}
		if eventKind != "upstream_cy_local_detected" || comparison != PromptPolicyComparisonLocalDetected {
			t.Fatalf("risk event was not reconciled: kind=%q comparison=%q", eventKind, comparison)
		}
		evidenceRows, err := db.ListPromptRuleCandidateEvidence(context.Background(), got.CandidateID, 100)
		if err != nil {
			t.Fatalf("query reconciled candidate evidence len=%d err=%v", len(evidenceRows), err)
		}
		var evidenceMetadata string
		for _, row := range evidenceRows {
			if row.PromptPolicyIncidentID == incidentID {
				evidenceMetadata = row.MetadataJSON
				break
			}
		}
		if evidenceMetadata == "" {
			t.Fatalf("candidate evidence link for incident %q not found", incidentID)
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(evidenceMetadata), &metadata); err != nil {
			t.Fatalf("decode reconciled evidence metadata: %v", err)
		}
		learning, _ := metadata["learning_evidence"].(map[string]any)
		shadow, _ := learning["shadow_audit"].(map[string]any)
		if metadata["local_comparison"] != PromptPolicyComparisonLocalDetected || metadata["local_outcome"] != PromptPolicyOutcomeAuditHit || metadata["local_reason_code"] != "prompt_policy_shadow_async" || shadow["audit_score"] != float64(20) {
			t.Fatalf("candidate learning evidence was not reconciled: %#v", metadata)
		}
	}

	t.Run("shadow_before_incident", func(t *testing.T) {
		db := newPromptPolicySQLiteTestDB(t)
		incident, candidate, evidence := promptPolicyTestInputs("incident-shadow-first")
		incident.RequestCorrelationID = "request-shadow-first"
		evidence.SourceRef = incident.RequestCorrelationID
		insertShadow(t, db, incident.RequestCorrelationID)
		if err := db.PersistPromptPolicyIncident(context.Background(), incident, candidate, evidence); err != nil {
			t.Fatalf("PersistPromptPolicyIncident: %v", err)
		}
		assertReconciled(t, db, incident.IncidentID)
	})

	t.Run("incident_before_shadow", func(t *testing.T) {
		db := newPromptPolicySQLiteTestDB(t)
		incident, candidate, evidence := promptPolicyTestInputs("incident-cy-first")
		incident.RequestCorrelationID = "request-cy-first"
		evidence.SourceRef = incident.RequestCorrelationID
		if err := db.PersistPromptPolicyIncident(context.Background(), incident, candidate, evidence); err != nil {
			t.Fatalf("PersistPromptPolicyIncident: %v", err)
		}
		insertShadow(t, db, incident.RequestCorrelationID)
		assertReconciled(t, db, incident.IncidentID)
	})

	t.Run("concurrent", func(t *testing.T) {
		db := newPromptPolicySQLiteTestDB(t)
		for index := 0; index < 25; index++ {
			incidentID := fmt.Sprintf("incident-concurrent-%d", index)
			correlationID := fmt.Sprintf("request-concurrent-%d", index)
			incident, candidate, evidence := promptPolicyTestInputs(incidentID)
			incident.RequestCorrelationID = correlationID
			evidence.SourceRef = correlationID
			start := make(chan struct{})
			errs := make(chan error, 2)
			var workers sync.WaitGroup
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				errs <- db.PersistPromptPolicyIncident(context.Background(), incident, candidate, evidence)
			}()
			go func() {
				defer workers.Done()
				<-start
				errs <- db.InsertPromptFilterLog(context.Background(), shadowInput(correlationID))
			}()
			close(start)
			workers.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent persistence %d: %v", index, err)
				}
			}
			assertReconciled(t, db, incidentID)
		}
	})
}

func TestMergePromptPolicyCandidateEvidenceMetadataCompactsNearLimit(t *testing.T) {
	rawBytes, err := json.Marshal(map[string]any{
		"padding":          strings.Repeat("x", 65200),
		"evidence_quality": "complete",
		"learning_evidence": map[string]any{
			"version": 1, "quality": "complete", "prompt_text": "durable prompt",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergePromptPolicyCandidateEvidenceMetadata(string(rawBytes), PromptPolicyOutcomeAuditHit,
		PromptPolicyComparisonLocalDetected, 80, "prompt_policy_shadow_async", "current_user",
		`[{"name":"rule","weight":80}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) > 64*1024 || !json.Valid([]byte(merged)) {
		t.Fatalf("compacted metadata bytes=%d valid=%t", len(merged), json.Valid([]byte(merged)))
	}
	if strings.Contains(merged, `"padding"`) || !strings.Contains(merged, `"prompt_text":"durable prompt"`) || !strings.Contains(merged, `"local_comparison":"local_detected"`) {
		t.Fatalf("unexpected compacted metadata bytes=%d padding=%t prompt=%t comparison=%t", len(merged),
			strings.Contains(merged, `"padding"`), strings.Contains(merged, `"prompt_text":"durable prompt"`),
			strings.Contains(merged, `"local_comparison":"local_detected"`))
	}
}

func TestClearPromptFilterLogsKeepsIncidentsAndCandidateEvidence(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("incident-clear")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	if err := db.ClearPromptFilterLogs(ctx); err != nil {
		t.Fatalf("ClearPromptFilterLogs: %v", err)
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("incident unexpectedly cleared count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidates`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("candidate unexpectedly cleared count=%d err=%v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidate_evidence`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("evidence unexpectedly cleared count=%d err=%v", count, err)
	}
}

func TestDeletePromptPolicyIncidentRemovesHistoryButRetainsLearningEvidence(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("incident-delete")
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	stored, err := db.GetPromptPolicyIncident(ctx, incident.IncidentID)
	if err != nil {
		t.Fatalf("GetPromptPolicyIncident: %v", err)
	}
	if err := db.DeletePromptPolicyIncident(ctx, incident.IncidentID); err != nil {
		t.Fatalf("DeletePromptPolicyIncident: %v", err)
	}
	if _, err := db.GetPromptPolicyIncident(ctx, incident.IncidentID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted incident still readable: %v", err)
	}
	items, err := db.ListPromptRuleCandidateEvidence(ctx, stored.CandidateID, 10)
	if err != nil || len(items) != 1 || items[0].PromptPolicyIncidentID != "" {
		t.Fatalf("learning evidence was removed or retained a stale incident link: items=%#v err=%v", items, err)
	}
	var riskEvents int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_risk_events WHERE source_id=$1`, incident.IncidentID).Scan(&riskEvents); err != nil || riskEvents == 0 {
		t.Fatalf("risk profile history was not retained after incident deletion: count=%d err=%v", riskEvents, err)
	}
}

func TestClearPromptFilterLogsByReviewStatusKeepsOtherLogSection(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for _, input := range []*PromptFilterLogInput{
		{Source: "local_filter", Action: "block", Reviewed: false},
		{Source: "local_filter", Action: "allow", Reviewed: true, ReviewModel: "review-model"},
	} {
		if err := db.InsertPromptFilterLog(ctx, input); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}
	if err := db.ClearPromptFilterLogsByReviewStatus(ctx, true); err != nil {
		t.Fatalf("clear reviewed logs: %v", err)
	}
	var localCount, reviewCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_filter_logs WHERE reviewed = false`).Scan(&localCount); err != nil {
		t.Fatalf("count local logs: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_filter_logs WHERE reviewed = true`).Scan(&reviewCount); err != nil {
		t.Fatalf("count review logs: %v", err)
	}
	if localCount != 1 || reviewCount != 0 {
		t.Fatalf("review clear crossed sections: local=%d review=%d", localCount, reviewCount)
	}
	if err := db.ClearPromptFilterLogsByReviewStatus(ctx, false); err != nil {
		t.Fatalf("clear local logs: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_filter_logs`).Scan(&localCount); err != nil || localCount != 0 {
		t.Fatalf("local logs not cleared count=%d err=%v", localCount, err)
	}
}

func TestClearPromptFilterLogsBySourceKeepsOtherSources(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for _, input := range []*PromptFilterLogInput{
		{Source: "local_filter", Action: "block"},
		{Source: "local_filter", Action: "allow", Reviewed: true, ReviewModel: "review-model"},
		{Source: "upstream_cyber_policy", Action: "block"},
	} {
		if err := db.InsertPromptFilterLog(ctx, input); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}
	if err := db.ClearPromptFilterLogsBySource(ctx, "local_filter"); err != nil {
		t.Fatalf("clear local source logs: %v", err)
	}
	var localCount, upstreamCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_filter_logs WHERE source = 'local_filter'`).Scan(&localCount); err != nil {
		t.Fatalf("count local source logs: %v", err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_filter_logs WHERE source = 'upstream_cyber_policy'`).Scan(&upstreamCount); err != nil {
		t.Fatalf("count upstream source logs: %v", err)
	}
	if localCount != 0 || upstreamCount != 1 {
		t.Fatalf("source clear crossed boundaries: local=%d upstream=%d", localCount, upstreamCount)
	}
}

func TestLegacyPromptFilterLogMigratesWithoutInventingScores(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "upstream_cyber_policy", Endpoint: "/v1/responses", Model: "gpt-5.4", ErrorCode: "cyber_policy", FullText: "legacy redacted error",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	if err := db.migrateLegacyPromptPolicyIncidents(ctx); err != nil {
		t.Fatalf("migrateLegacyPromptPolicyIncidents: %v", err)
	}
	items, total, err := db.ListPromptPolicyIncidentsPage(ctx, PromptPolicyIncidentQuery{Page: 1, PageSize: 10})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("legacy incidents total=%d items=%#v err=%v", total, items, err)
	}
	if items[0].LocalEvaluationState != PromptPolicyEvaluationLegacyUnknown || items[0].LocalScore != nil || items[0].PromptText != "" || items[0].RequestCorrelationID != "" {
		t.Fatalf("legacy incident invented local data: %#v", items[0])
	}
}

func TestPromptPolicyIncidentSQLiteSchemaAndIndexes(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for table, expected := range map[string][]string{
		"usage_logs":                     {"prompt_policy_incident_id"},
		"prompt_rule_candidate_evidence": {"prompt_policy_incident_id"},
		"prompt_filter_logs":             {"request_correlation_id", "newapi_policy_status", "newapi_platform", "newapi_user_id", "newapi_request_id", "newapi_decision_id"},
		"prompt_policy_incidents":        {"incident_id", "request_correlation_id", "account_name", "account_group_ids", "api_key_allowed_group_ids", "prompt_available", "local_comparison", "local_score", "local_audit_score", "candidate_id", "candidate_evidence_id"},
	} {
		columns, err := db.sqliteTableColumns(ctx, table)
		if err != nil {
			t.Fatalf("sqliteTableColumns(%s): %v", table, err)
		}
		for _, name := range expected {
			if _, ok := columns[name]; !ok {
				t.Fatalf("%s missing column %q", table, name)
			}
		}
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='prompt_policy_incidents'`)
	if err != nil {
		t.Fatalf("list incident indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan incident index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{
		"idx_prompt_policy_incidents_request", "idx_prompt_policy_incidents_created", "idx_prompt_policy_incidents_api_key",
		"idx_prompt_policy_incidents_account", "idx_prompt_policy_incidents_endpoint", "idx_prompt_policy_incidents_outcome", "idx_prompt_policy_incidents_comparison",
	} {
		if !indexes[name] {
			t.Fatalf("prompt_policy_incidents missing index %q", name)
		}
	}
}

func TestPromptPolicyIncidentPostgresMigrationDDL(t *testing.T) {
	promptPolicyDDLDriverOnce.Do(func() { sql.Register("prompt-policy-ddl-capture", promptPolicyDDLDriver{}) })
	promptPolicyDDLQueryMu.Lock()
	promptPolicyDDLQueries = nil
	promptPolicyDDLQueryMu.Unlock()
	conn, err := sql.Open("prompt-policy-ddl-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	if err := db.ensurePromptPolicyIncidentsTable(context.Background()); err != nil {
		t.Fatalf("ensurePromptPolicyIncidentsTable: %v", err)
	}
	promptPolicyDDLQueryMu.Lock()
	joined := strings.Join(promptPolicyDDLQueries, "\n")
	promptPolicyDDLQueryMu.Unlock()
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS prompt_policy_incidents",
		"incident_id VARCHAR(64) NOT NULL UNIQUE",
		"local_score INT NULL",
		"local_audit_score INT NULL",
		"ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS prompt_policy_incident_id",
		"ALTER TABLE prompt_rule_candidate_evidence ADD COLUMN IF NOT EXISTS prompt_policy_incident_id",
		"ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_correlation_id",
		"ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_policy_status",
		"account_group_ids TEXT",
		"api_key_allowed_group_ids TEXT",
		"local_comparison VARCHAR(32)",
		"idx_prompt_policy_incidents_request",
		"idx_prompt_policy_incidents_outcome",
		"idx_prompt_policy_incidents_account",
		"idx_prompt_policy_incidents_comparison",
		"legacy-",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("postgres incident migration missing %q: %s", fragment, joined)
		}
	}
}

func TestUsageLogIncidentIDSurvivesEveryDetailQueryPath(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incidentID := "incident-usage-query-paths"
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID: 1, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 400,
		AttemptIndex: 2, UpstreamErrorKind: "cyber_policy", PromptPolicyIncidentID: incidentID,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()
	assertID := func(name string, logs []*UsageLog, err error) {
		t.Helper()
		if err != nil || len(logs) != 1 || logs[0].PromptPolicyIncidentID != incidentID {
			t.Fatalf("%s logs=%#v err=%v", name, logs, err)
		}
	}
	recent, err := db.ListRecentUsageLogs(ctx, 10)
	assertID("recent", recent, err)
	ranged, err := db.ListUsageLogsByTimeRange(ctx, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	assertID("time_range", ranged, err)
	paged, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), Page: 1, PageSize: 10, IncludeCanceled: true,
	})
	if err != nil || paged == nil {
		t.Fatalf("paged query: %#v err=%v", paged, err)
	}
	assertID("paged", paged.Logs, nil)
}

// TestMergePromptPolicyCandidateEvidenceMetadataEscapeInflation 验证 JSON
// HTML 转义膨胀(<>& → \u00XX)不会让对账报错回滚整个日志事务:学习包正文
// 按编码后体积收缩,对账结论字段始终保留。
func TestMergePromptPolicyCandidateEvidenceMetadataEscapeInflation(t *testing.T) {
	bundle := fmt.Sprintf(`{"version":1,"quality":"complete","prompt_text":%q,"context":[{"origin":"history","text":%q}],"upstream_error":%q}`,
		strings.Repeat("<", 20000), strings.Repeat("&", 11000), strings.Repeat(">", 4000))
	raw := `{"evidence_quality":"complete","incident_id":"escape","learning_evidence":` + bundle + `}`
	merged, err := mergePromptPolicyCandidateEvidenceMetadata(raw, PromptPolicyOutcomeAuditHit, PromptPolicyComparisonLocalDetected, 42, "rc", "current_user", `[]`)
	if err != nil {
		t.Fatalf("escape-heavy merge must not fail: %v", err)
	}
	if len(merged) > 64*1024 || !json.Valid([]byte(merged)) {
		t.Fatalf("merged bytes=%d valid=%t", len(merged), json.Valid([]byte(merged)))
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(merged), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["local_outcome"] != PromptPolicyOutcomeAuditHit || parsed["local_comparison"] != PromptPolicyComparisonLocalDetected {
		t.Fatalf("reconciled decision fields lost: %v", parsed)
	}
}
