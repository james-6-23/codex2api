package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaintenanceJobGrokFreshness = "grok_freshness"

type MaintenanceJob struct {
	EntityID   int64
	JobKind    string
	DueAt      time.Time
	LeaseOwner string
	LeaseUntil sql.NullTime
	Attempts   int64
	LastError  string
}

// SeedGrokMaintenanceJobs is a one-time startup reconciliation. Steady-state
// scheduling uses the due_at index and account-change triggers, so the worker
// never scans the full account pool every 30 seconds.
func (db *DB) SeedGrokMaintenanceJobs(ctx context.Context, dueAt time.Time) error {
	if dueAt.IsZero() {
		dueAt = time.Now()
	}
	upstreamExpr := `LOWER(COALESCE(credentials->>'upstream_type',''))`
	if db.isSQLite() {
		upstreamExpr = `LOWER(COALESCE(json_extract(credentials,'$.upstream_type'),''))`
	}
	query := fmt.Sprintf(`
		INSERT INTO maintenance_jobs(entity_id,job_kind,due_at,updated_at)
		SELECT id,$1,$2,CURRENT_TIMESTAMP FROM accounts
		WHERE status<>'deleted' AND COALESCE(error_message,'')<>'deleted'
		  AND COALESCE(enabled,true) AND %s='grok'
		ON CONFLICT(entity_id,job_kind) DO NOTHING`, upstreamExpr)
	_, err := db.conn.ExecContext(ctx, query, MaintenanceJobGrokFreshness, db.timeArg(dueAt))
	return err
}

func scanMaintenanceJob(scanner interface{ Scan(...interface{}) error }) (MaintenanceJob, error) {
	var job MaintenanceJob
	var dueRaw, leaseRaw interface{}
	if err := scanner.Scan(&job.EntityID, &job.JobKind, &dueRaw, &job.LeaseOwner, &leaseRaw, &job.Attempts, &job.LastError); err != nil {
		return job, err
	}
	var err error
	job.DueAt, err = parseDBTimeValue(dueRaw)
	if err != nil {
		return job, err
	}
	job.LeaseUntil, err = parseDBNullTimeValue(leaseRaw)
	return job, err
}

// ClaimMaintenanceJobs leases due rows. PostgreSQL uses SKIP LOCKED; SQLite
// serializes the short select/update transaction through its existing write
// gate. Both variants allow several application replicas to share one queue.
func (db *DB) ClaimMaintenanceJobs(ctx context.Context, jobKind, owner string, now time.Time, lease time.Duration, limit int) ([]MaintenanceJob, error) {
	jobKind = strings.TrimSpace(jobKind)
	owner = strings.TrimSpace(owner)
	if jobKind == "" || owner == "" {
		return nil, errors.New("maintenance job kind and owner are required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	leaseUntil := now.Add(lease)
	if !db.isSQLite() {
		rows, err := db.conn.QueryContext(ctx, `
			WITH due AS (
				SELECT entity_id,job_kind FROM maintenance_jobs
				WHERE job_kind=$1 AND due_at<=$2 AND (lease_until IS NULL OR lease_until<=$2)
				ORDER BY due_at,entity_id LIMIT $3 FOR UPDATE SKIP LOCKED
			)
			UPDATE maintenance_jobs j SET lease_owner=$4,lease_until=$5,attempts=j.attempts+1,updated_at=NOW()
			FROM due WHERE j.entity_id=due.entity_id AND j.job_kind=due.job_kind
			RETURNING j.entity_id,j.job_kind,j.due_at,j.lease_owner,j.lease_until,j.attempts,j.last_error`,
			jobKind, db.timeArg(now), limit, owner, db.timeArg(leaseUntil))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		jobs := make([]MaintenanceJob, 0, limit)
		for rows.Next() {
			job, err := scanMaintenanceJob(rows)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, job)
		}
		return jobs, rows.Err()
	}

	var jobs []MaintenanceJob
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `SELECT entity_id,job_kind,due_at,lease_owner,lease_until,attempts,last_error
			FROM maintenance_jobs WHERE job_kind=$1 AND due_at<=$2 AND (lease_until IS NULL OR lease_until<=$2)
			ORDER BY due_at,entity_id LIMIT $3`, jobKind, db.timeArg(now), limit)
		if err != nil {
			return err
		}
		var candidates []MaintenanceJob
		for rows.Next() {
			job, err := scanMaintenanceJob(rows)
			if err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, job)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, candidate := range candidates {
			result, err := tx.ExecContext(ctx, `UPDATE maintenance_jobs
				SET lease_owner=$1,lease_until=$2,attempts=attempts+1,updated_at=CURRENT_TIMESTAMP
				WHERE entity_id=$3 AND job_kind=$4 AND (lease_until IS NULL OR lease_until<=$5)`,
				owner, db.timeArg(leaseUntil), candidate.EntityID, candidate.JobKind, db.timeArg(now))
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed == 1 {
				candidate.LeaseOwner = owner
				candidate.LeaseUntil = sql.NullTime{Time: leaseUntil, Valid: true}
				candidate.Attempts++
				jobs = append(jobs, candidate)
			}
		}
		return tx.Commit()
	})
	return jobs, err
}

func (db *DB) CompleteMaintenanceJob(ctx context.Context, entityID int64, jobKind, owner string, nextDue time.Time) error {
	if nextDue.IsZero() {
		nextDue = time.Now().Add(time.Minute)
	}
	result, err := db.conn.ExecContext(ctx, `UPDATE maintenance_jobs SET due_at=$1,lease_owner='',lease_until=NULL,attempts=0,last_error='',updated_at=CURRENT_TIMESTAMP
		WHERE entity_id=$2 AND job_kind=$3 AND lease_owner=$4`, db.timeArg(nextDue), entityID, jobKind, owner)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) FailMaintenanceJob(ctx context.Context, entityID int64, jobKind, owner string, retryAt time.Time, failure error) error {
	if retryAt.IsZero() {
		retryAt = time.Now().Add(time.Minute)
	}
	message := ""
	if failure != nil {
		message = failure.Error()
		if len(message) > 500 {
			// 中文错误信息按字节截断会产生非法 UTF-8,PostgreSQL 会拒绝该参数。
			message = strings.ToValidUTF8(message[:500], "")
		}
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE maintenance_jobs SET due_at=$1,lease_owner='',lease_until=NULL,last_error=$2,updated_at=CURRENT_TIMESTAMP
		WHERE entity_id=$3 AND job_kind=$4 AND lease_owner=$5`, db.timeArg(retryAt), message, entityID, jobKind, owner)
	return err
}

func (db *DB) DeleteMaintenanceJob(ctx context.Context, entityID int64, jobKind string) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM maintenance_jobs WHERE entity_id=$1 AND job_kind=$2`, entityID, jobKind)
	return err
}
