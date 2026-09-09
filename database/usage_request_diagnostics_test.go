package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUsageRequestDiagnosticsPersistenceAndLightweightLists(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "diagnostics.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	const payload = `{"version":1,"selected_account_id":17,"incoming":{"client_metadata":{"thread_source":"guardian_review"}}}`
	if err := db.InsertUsageLog(test.Context(), &UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, RequestType: "related_internal", RequestDiagnostics: payload}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 10}
	lists := []struct {
		name string
		read func() ([]*UsageLog, error)
	}{
		{"recent", func() ([]*UsageLog, error) { return db.ListRecentUsageLogs(test.Context(), 10) }},
		{"time_range", func() ([]*UsageLog, error) {
			return db.ListUsageLogsByTimeRange(test.Context(), filter.Start, filter.End)
		}},
		{"filter", func() ([]*UsageLog, error) { return db.ListUsageLogsByFilter(test.Context(), filter) }},
		{"paged", func() ([]*UsageLog, error) {
			page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
			if err != nil {
				return nil, err
			}
			return page.Logs, nil
		}},
	}
	for _, item := range lists {
		test.Run(item.name, func(test *testing.T) {
			logs, err := item.read()
			if err != nil || len(logs) != 1 {
				test.Fatalf("logs=%+v, err=%v", logs, err)
			}
			if logs[0].RequestType != "related_internal" {
				test.Fatalf("missing type: %+v", logs[0])
			}
			encoded, err := json.Marshal(logs)
			if err != nil {
				test.Fatal(err)
			}
			if strings.Contains(string(encoded), "guardian_review") || strings.Contains(string(encoded), "request_diagnostics") {
				test.Fatal("normal list includes full diagnostics")
			}
			detail, err := db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
			if err != nil || detail.RequestType != "related_internal" || string(detail.Diagnostics) != payload {
				test.Fatalf("detail=%+v, err=%v", detail, err)
			}
		})
	}
	if _, err := db.GetUsageRequestDiagnostics(test.Context(), 999999); !errors.Is(err, sql.ErrNoRows) {
		test.Fatalf("missing row error: %v", err)
	}
}

func TestUsageRequestDiagnosticsSQLiteMigrationAndHistoricalRows(test *testing.T) {
	path := filepath.Join(test.TempDir(), "migration.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	_, err = db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs (endpoint, model, status_code) VALUES ('/v1/responses', 'old', 200)`)
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN request_type`)
	}
	if err == nil {
		_, err = db.conn.ExecContext(test.Context(), `ALTER TABLE usage_logs DROP COLUMN request_diagnostics`)
	}
	_ = db.Close()
	if err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 1 || logs[0].RequestType != "" {
		test.Fatalf("historical logs=%+v, err=%v", logs, err)
	}
	detail, err := db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
	if err != nil || detail.Diagnostics != nil || detail.RequestType != "" {
		test.Fatalf("historical diagnostics must not be inferred: %+v, %v", detail, err)
	}
	if err := db.InsertUsageLog(test.Context(), &UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses"}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err = db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 2 || logs[0].RequestType != "unknown" {
		test.Fatalf("new unclassified row: %+v, %v", logs, err)
	}
	detail, err = db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
	if err != nil || !strings.Contains(string(detail.Diagnostics), "not_available") {
		test.Fatalf("missing capture status: %+v, %v", detail, err)
	}
}

type usageDiagnosticSQLCapture struct {
	query string
	args  []interface{}
}

func (capture *usageDiagnosticSQLCapture) ExecContext(_ context.Context, query string, args ...interface{}) (sql.Result, error) {
	capture.query, capture.args = query, args
	return nil, nil
}

func TestUsageRequestDiagnosticsPostgresBatchShape(test *testing.T) {
	capture := &usageDiagnosticSQLCapture{}
	db := &DB{}
	batch := []usageLogEntry{
		{RequestType: "user", RequestDiagnostics: `{"version":1}`, NewAPIUserName: "window-user", RequestID: "request-1", UpstreamRequestID: "upstream-1", UpstreamProxyID: 12, UpstreamProxyName: "proxy-1", ImageInputTokens: 7, ImageOutputTokens: 11, CachedImageInputTokens: 3},
		{RequestType: "compaction", RequestDiagnostics: `{"version":1,"attempt":2}`, RequestID: "request-2", UpstreamRequestID: "upstream-2"},
	}
	if err := db.batchInsertLogsChunk(test.Context(), capture, batch); err != nil {
		test.Fatal(err)
	}
	if len(capture.args) != len(batch)*usageLogInsertColumnCount || !strings.Contains(capture.query, "request_type, request_diagnostics,") || !strings.Contains(capture.query, fmt.Sprintf("$%d)", len(capture.args))) {
		test.Fatalf("invalid batch shape: args=%d, query=%s", len(capture.args), capture.query)
	}
	columns := strings.Split(capture.query[strings.Index(capture.query, "(")+1:strings.Index(capture.query, ")")], ",")
	for index := range columns {
		columns[index] = strings.TrimSpace(columns[index])
	}
	if len(columns) != usageLogInsertColumnCount {
		test.Fatalf("columns=%d, expected %d", len(columns), usageLogInsertColumnCount)
	}
	for index, entry := range batch {
		for name, expected := range map[string]interface{}{
			"request_type": entry.RequestType, "request_diagnostics": entry.RequestDiagnostics, "newapi_user_name": entry.NewAPIUserName,
			"request_id": entry.RequestID, "upstream_request_id": entry.UpstreamRequestID, "upstream_proxy_id": entry.UpstreamProxyID, "upstream_proxy_name": entry.UpstreamProxyName,
			"image_input_tokens": entry.ImageInputTokens, "image_output_tokens": entry.ImageOutputTokens, "cached_image_input_tokens": entry.CachedImageInputTokens,
		} {
			columnIndex := slices.Index(columns, name)
			if columnIndex < 0 || capture.args[index*usageLogInsertColumnCount+columnIndex] != expected {
				test.Fatalf("row %d: column %s was not preserved", index, name)
			}
		}
	}
}

func TestUsageRequestDiagnosticsBoundsAndLoggingModes(test *testing.T) {
	if boundedUsageRequestDiagnostics(strings.Repeat("x", MaxUsageRequestDiagnosticsBytes+1)) != "" || !json.Valid([]byte(boundedUsageRequestDiagnostics(""))) {
		test.Fatal("invalid size or missing-capture handling")
	}
	db, err := New("sqlite", filepath.Join(test.TempDir(), "mode.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	for _, mode := range []string{UsageLogModeOff, UsageLogModeErrors} {
		db.SetUsageLogConfig(mode, 100, 10)
		if err := db.InsertUsageLog(test.Context(), &UsageLogInput{StatusCode: 200, RequestType: "user", RequestDiagnostics: `{"version":1}`}); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 0 {
		test.Fatalf("diagnostics changed log mode: %+v, %v", logs, err)
	}
}
