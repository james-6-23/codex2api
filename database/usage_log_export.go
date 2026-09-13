package database

import (
	"context"
	"encoding/json"
	"time"
)

type UsageLogExportCursor struct {
	SnapshotID int64     `json:"snapshot_id"`
	BeforeID   int64     `json:"before_id"`
	BeforeTime time.Time `json:"before_time"`
}

func (db *DB) UsageLogExportSnapshot(ctx context.Context) (int64, error) {
	var maximum int64
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs`).Scan(&maximum)
	return maximum, err
}

func (db *DB) WalkUsageLogExportPage(ctx context.Context, filter *UsageLogFilter, cursor *UsageLogExportCursor, visit func(*UsageLogExportEntry) error) error {
	return db.walkUsageLogExport(ctx, filter, true, visit, cursor)
}

type UsageLogExportEntry struct {
	*UsageLog
	Diagnostics json.RawMessage `json:"diagnostics"`
}

func usageExportDiagnosticJSON(payload string) json.RawMessage {
	if payload == "" {
		return nil
	}
	if !json.Valid([]byte(payload)) {
		return json.RawMessage(`{"capture_status":"invalid_stored_json"}`)
	}
	return json.RawMessage(payload)
}
