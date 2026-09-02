package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGrokMaintenanceJobTriggerLeaseAndReschedule(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "maintenance-jobs.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	accountID, err := db.InsertAccountWithUpstream(ctx, "grok-job", "xai", "grok", map[string]interface{}{
		"upstream_type": "grok",
		"base_url":      "https://api.x.ai/v1",
		"api_key":       "xai-test",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream: %v", err)
	}

	jobs, err := db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "worker-a", time.Now().Add(time.Second), time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimMaintenanceJobs(worker-a): %v", err)
	}
	if len(jobs) != 1 || jobs[0].EntityID != accountID {
		t.Fatalf("claimed jobs = %+v, want account %d", jobs, accountID)
	}
	jobs, err = db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "worker-b", time.Now().Add(time.Second), time.Minute, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("second lease claim = %+v, err=%v, want empty", jobs, err)
	}

	nextDue := time.Now().Add(time.Hour)
	if err := db.CompleteMaintenanceJob(ctx, accountID, MaintenanceJobGrokFreshness, "worker-a", nextDue); err != nil {
		t.Fatalf("CompleteMaintenanceJob: %v", err)
	}
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{"grok_rate_limit": `{"remaining":1}`}); err != nil {
		t.Fatalf("UpdateCredentials(benign): %v", err)
	}
	jobs, err = db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "worker-b", time.Now().Add(time.Minute), time.Minute, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("benign credential update rescheduled job: %+v, err=%v", jobs, err)
	}

	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{"base_url": "https://api-alt.x.ai/v1"}); err != nil {
		t.Fatalf("UpdateCredentials(routing): %v", err)
	}
	jobs, err = db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "worker-b", time.Now().Add(time.Second), time.Minute, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("routing credential update claim = %+v, err=%v, want one", jobs, err)
	}

	if err := db.SoftDeleteAccount(ctx, accountID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_jobs WHERE entity_id=$1 AND job_kind=$2`, accountID, MaintenanceJobGrokFreshness).Scan(&count); err != nil {
		t.Fatalf("count maintenance job: %v", err)
	}
	if count != 0 {
		t.Fatalf("maintenance job count after delete = %d, want 0", count)
	}
}

func TestFailMaintenanceJobTruncatesMultibyteError(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "maintenance-jobs-utf8.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	accountID, err := db.InsertAccountWithUpstream(ctx, "grok-utf8", "xai", "grok", map[string]interface{}{
		"upstream_type": "grok",
		"base_url":      "https://api.x.ai/v1",
		"api_key":       "xai-test",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream: %v", err)
	}
	jobs, err := db.ClaimMaintenanceJobs(ctx, MaintenanceJobGrokFreshness, "worker-utf8", time.Now().Add(time.Second), time.Minute, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ClaimMaintenanceJobs = %+v, err=%v", jobs, err)
	}

	// 500 字节边界恰好落在多字节字符中间,截断结果必须仍是合法 UTF-8。
	long := strings.Repeat("上游探测失败", 60)
	if err := db.FailMaintenanceJob(ctx, accountID, MaintenanceJobGrokFreshness, "worker-utf8", time.Now().Add(time.Minute), errors.New(long)); err != nil {
		t.Fatalf("FailMaintenanceJob: %v", err)
	}
	var lastError string
	if err := db.conn.QueryRowContext(ctx, `SELECT last_error FROM maintenance_jobs WHERE entity_id=$1`, accountID).Scan(&lastError); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if lastError == "" || !utf8.ValidString(lastError) || len(lastError) > 500 {
		t.Fatalf("last_error invalid: len=%d valid=%t", len(lastError), utf8.ValidString(lastError))
	}
}
