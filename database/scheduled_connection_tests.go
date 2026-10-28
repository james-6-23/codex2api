package database

import "context"

// ListScheduledConnectionTestConfigs projects only non-secret plan data; token
// blobs and the runtime scheduler are not scanned into the maintenance worker.
type ScheduledConnectionTestConfig struct {
	AccountID int64
	Config    string
}

func (db *DB) ListScheduledConnectionTestConfigs(ctx context.Context) ([]ScheduledConnectionTestConfig, error) {
	expr := "credentials->>'scheduled_connection_tests'"
	upstream := "LOWER(COALESCE(credentials->>'upstream_type',''))"
	if db.isSQLite() {
		expr = "json_extract(credentials,'$.scheduled_connection_tests')"
		upstream = "LOWER(COALESCE(json_extract(credentials,'$.upstream_type'),''))"
	}
	rows, err := db.conn.QueryContext(ctx, "SELECT id,"+expr+" FROM accounts WHERE status<>'deleted' AND COALESCE(error_message,'')<>'deleted' AND COALESCE(enabled,true) AND "+upstream+" IN ('','codex','openai_responses') AND COALESCE("+expr+",'')<>'' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ScheduledConnectionTestConfig{}
	for rows.Next() {
		var row ScheduledConnectionTestConfig
		if err := rows.Scan(&row.AccountID, &row.Config); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (db *DB) ListOneShotMaintenanceJobs(ctx context.Context, id int64, prefix string, limit int) ([]MaintenanceJob, error) {
	if limit < 1 || limit > 100 {
		limit = 10
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT entity_id,job_kind,due_at,lease_owner,lease_until,attempts,last_error FROM maintenance_jobs WHERE entity_id=$1 AND substr(job_kind,1,length($2))=$2 ORDER BY due_at DESC LIMIT $3`, id, prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MaintenanceJob{}
	for rows.Next() {
		job, err := scanMaintenanceJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}
