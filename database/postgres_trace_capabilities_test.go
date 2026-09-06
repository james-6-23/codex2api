package database

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Run against an empty disposable database, as for the other PostgreSQL tests.
func TestPostgresTraceAndCapabilities(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "compat-test", "refresh-test", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: id, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, RequestID: "pg-request", UpstreamRequestID: "pg-upstream", UpstreamProxyID: 9, UpstreamProxyName: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	f := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), RequestID: "pg-request", Page: 1, PageSize: 10}
	logs, err := db.ListUsageLogsByFilter(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].UpstreamRequestID != "pg-upstream" || logs[0].UpstreamProxyID != 9 || logs[0].UpstreamProxyName != "snapshot" {
		t.Fatalf("PostgreSQL trace did not round-trip: %+v", logs)
	}
	if _, err := db.ListUsageLogsByTimeRangePaged(ctx, f); err != nil {
		t.Fatal(err)
	}
	snapshot := ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: time.Now().UnixNano(), Models: map[string]map[string]json.RawMessage{"gpt-5.6-sol": {"context_window": json.RawMessage("1000"), "use_responses_lite": json.RawMessage("true")}}}
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListModelCapabilities(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if string(got[id].Models["gpt-5.6-sol"]["use_responses_lite"]) != "true" {
		t.Fatal("PostgreSQL capabilities missing")
	}
	// Reopening runs the idempotent schema path and restores persisted state.
	second, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got, err := second.ListModelCapabilities(ctx, []int64{id}); err != nil || len(got) != 1 {
		t.Fatalf("restart restore: %v, %+v", err, got)
	}
}
