package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newPromptRetentionTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "prompt-retention.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.ensurePromptPolicyIncidentsTable(ctx); err != nil {
		t.Fatalf("ensure incidents: %v", err)
	}
	if err := db.ensurePromptRiskEventsTable(ctx); err != nil {
		t.Fatalf("ensure risk events: %v", err)
	}
	return db
}

func (db *DB) mustExec(t *testing.T, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func (db *DB) mustCount(t *testing.T, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.conn.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

// seedPromptRetentionFixture 写入：
//   - 日志 1..4：1、2 过期，3 过期但关联 CY（corr=cy-1），4 未过期
//   - 风险事件：e1 挂日志 1（过期），e2 挂 CY（过期），e3 挂日志 4（未过期）
//   - 来源：s1(e1)、s2(e2)、s3(e3)、s4 孤儿过期、s5 孤儿未过期
//   - CY：incident-1，request_correlation_id=cy-1
func seedPromptRetentionFixture(t *testing.T, db *DB) {
	t.Helper()
	old := sqliteTimeParam(time.Now().UTC().Add(-10 * 24 * time.Hour))
	fresh := sqliteTimeParam(time.Now().UTC().Add(-time.Hour))
	insertLog := func(id int, createdAt, corr string, reviewed int, source string) {
		db.mustExec(t, `INSERT INTO prompt_filter_logs (id, created_at, request_correlation_id, reviewed, source, action) VALUES ($1, $2, $3, $4, $5, 'block')`, id, createdAt, corr, reviewed, source)
	}
	insertLog(1, old, "", 0, "local_filter")
	insertLog(2, old, "", 1, "review")
	insertLog(3, old, "cy-1", 0, "local_filter")
	insertLog(4, fresh, "", 0, "local_filter")
	db.mustExec(t, `INSERT INTO prompt_policy_incidents (incident_id, request_correlation_id, created_at) VALUES ('incident-1', 'cy-1', $1)`, old)
	insertEvent := func(id int, createdAt, sourceID, incidentID string, logID int) {
		db.mustExec(t, `INSERT INTO prompt_risk_events (id, created_at, source_type, source_id, incident_id, prompt_filter_log_id, subject_type, subject_key, event_kind)
			VALUES ($1, $2, 'src', $3, $4, $5, 'user', 'u-'||$1, 'block')`, id, createdAt, sourceID, incidentID, logID)
		db.mustExec(t, `INSERT INTO prompt_risk_event_sources (source_type, source_id, processed_at) VALUES ('src', $1, $2)`, sourceID, createdAt)
	}
	insertEvent(1, old, "s1", "", 1)
	insertEvent(2, old, "s2", "incident-1", 3)
	insertEvent(3, fresh, "s3", "", 4)
	db.mustExec(t, `INSERT INTO prompt_risk_event_sources (source_type, source_id, processed_at) VALUES ('src', 's4', $1), ('src', 's5', $2)`, old, fresh)
}

func TestPurgeExpiredPromptLogs_KeepsIncidentEvidenceAndFreshRows(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	seedPromptRetentionFixture(t, db)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	result, err := db.PurgeExpiredPromptLogs(context.Background(), cutoff, 1, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.Logs != 2 || result.Events != 1 || result.Sources != 2 || result.Interrupted {
		t.Fatalf("result = %+v, want logs=2 events=1 sources=2", result)
	}
	// batch=1 时每张表都要多跑一轮"空批"才能确认清完：3 + 2 + 3。
	if result.Batches < 6 {
		t.Fatalf("batches = %d, expected batched deletes", result.Batches)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs WHERE id IN (3, 4)`); got != 2 {
		t.Fatalf("protected/fresh logs missing: %d", got)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs`); got != 2 {
		t.Fatalf("logs remaining = %d", got)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_events WHERE id IN (2, 3)`); got != 2 {
		t.Fatalf("incident/fresh events missing: %d", got)
	}
	// 写入日志 / CY 时后台画像会自动登记 prompt_filter_log / prompt_policy_incident 来源
	// （processed_at 为当前时间，未过期），这里只断言测试自己写的 src 来源。
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_event_sources WHERE source_type = 'src'`); got != 3 {
		t.Fatalf("src sources remaining = %d, want s2 s3 s5", got)
	}

	// 再跑一次应当无事可做。
	again, err := db.PurgeExpiredPromptLogs(context.Background(), cutoff, 100, 0)
	if err != nil || again.Logs+again.Events+again.Sources != 0 {
		t.Fatalf("second purge = %+v err=%v", again, err)
	}
}

func TestPurgePromptFilterLogs_ManualClearRespectsFilterAndIncident(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	seedPromptRetentionFixture(t, db)
	reviewed := true
	result, err := db.PurgePromptFilterLogs(context.Background(), time.Now().UTC().Add(time.Minute), PromptLogPurgeFilter{Reviewed: &reviewed}, 100, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.Logs != 1 {
		t.Fatalf("reviewed purge should delete only log 2: %+v", result)
	}
	result, err = db.PurgePromptFilterLogs(context.Background(), time.Now().UTC().Add(time.Minute), PromptLogPurgeFilter{}, 100, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// 1 与 4 被清，3 因 CY 保留；风险事件与来源记录一律不动。
	if result.Logs != 2 || result.Events != 0 || result.Sources != 0 {
		t.Fatalf("full manual purge = %+v", result)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_events`); got != 3 {
		t.Fatalf("manual clear must keep risk events, got %d", got)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs`); got != 1 {
		t.Fatalf("only the CY-linked log should remain, got %d", got)
	}
}

func TestDeletePromptPolicyIncident_CascadesLinkedLogsButKeepsRiskProfile(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	seedPromptRetentionFixture(t, db)
	if err := db.DeletePromptPolicyIncident(context.Background(), "incident-1"); err != nil {
		t.Fatalf("delete incident: %v", err)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_policy_incidents`); got != 0 {
		t.Fatalf("incident still present")
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs WHERE id = 3`); got != 0 {
		t.Fatalf("CY-linked log should be cascaded")
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs`); got != 3 {
		t.Fatalf("unrelated logs = %d, want 3", got)
	}
	// 风险画像保留：事件 e2 与来源 s2 仍在，之后由保留策略按天龄清理。
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_events`); got != 3 {
		t.Fatalf("risk events must survive incident deletion, got %d", got)
	}
	// CY 与日志都没了 → e2 不再受保护，下一轮保留清理会带走它和 s2。
	result, err := db.PurgeExpiredPromptLogs(context.Background(), time.Now().UTC().Add(-7*24*time.Hour), 100, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_events WHERE id = 2`); got != 0 || result.Events != 2 {
		t.Fatalf("orphaned CY event should expire on the next purge: events=%d result=%+v", got, result)
	}
}

func TestClearPromptPolicyIncidents_CascadesLinkedLogs(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	seedPromptRetentionFixture(t, db)
	if err := db.ClearPromptPolicyIncidents(context.Background()); err != nil {
		t.Fatalf("clear incidents: %v", err)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_filter_logs`); got != 3 {
		t.Fatalf("logs = %d, want 3 (log 3 cascaded)", got)
	}
	if got := db.mustCount(t, `SELECT COUNT(*) FROM prompt_risk_events`); got != 3 {
		t.Fatalf("risk events must survive incident clear, got %d", got)
	}
}

func TestPromptLogRetentionConfigDefaults(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	cfg, err := db.GetPromptLogRetentionConfig(context.Background())
	if err != nil || cfg.RetentionDays != DefaultPromptLogRetentionDays {
		t.Fatalf("default config = %+v err=%v", cfg, err)
	}
	cfg, err = db.UpdatePromptLogRetentionDays(context.Background(), 1000)
	if err != nil || cfg.RetentionDays != MaxPromptLogRetentionDays {
		t.Fatalf("clamped config = %+v err=%v", cfg, err)
	}
	if err := db.RecordPromptLogRetentionRun(context.Background(), time.Now(), PromptLogPurgeResult{Logs: 5}, 1500*time.Millisecond, nil); err != nil {
		t.Fatalf("record run: %v", err)
	}
	cfg, _ = db.GetPromptLogRetentionConfig(context.Background())
	if !cfg.LastRunAt.Valid || cfg.LastDeletedLogs != 5 || cfg.LastDurationMs != 1500 {
		t.Fatalf("recorded run = %+v", cfg)
	}
}
