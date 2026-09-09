package database

import (
	"context"
	"encoding/json"
)

const MaxUsageRequestDiagnosticsBytes = 12 * 1024

type UsageRequestDiagnosticDetail struct {
	RequestType string          `json:"request_type"`
	Diagnostics json.RawMessage `json:"diagnostics"`
}

func (db *DB) GetUsageRequestDiagnostics(ctx context.Context, id int64) (*UsageRequestDiagnosticDetail, error) {
	result := &UsageRequestDiagnosticDetail{}
	var payload string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(request_type, ''), COALESCE(request_diagnostics, '') FROM usage_logs WHERE id = $1`, id).Scan(&result.RequestType, &payload)
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 && len(payload) <= MaxUsageRequestDiagnosticsBytes && json.Valid([]byte(payload)) {
		result.Diagnostics = json.RawMessage(payload)
	}
	return result, nil
}

func boundedUsageRequestDiagnostics(payload string) string {
	if payload == "" {
		return `{"version":1,"capture_status":"not_available"}`
	}
	if len(payload) > MaxUsageRequestDiagnosticsBytes {
		return ""
	}
	return payload
}

func usageRequestType(value, internalReason string) string {
	if internalReason != "" {
		return "gateway_internal"
	}
	switch value {
	case "user", "related_internal", "independent_internal", "related_unclassified", "compaction", "gateway_internal":
		return value
	default:
		return "unknown"
	}
}
