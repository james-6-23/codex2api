package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageTraceRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, input := range []*UsageLogInput{
		{Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 429, RequestID: "gateway-1", UpstreamRequestID: "vendor-1", UpstreamProxyID: 12, UpstreamProxyName: "old proxy label", IsRetryAttempt: true},
		{Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, RequestID: "gateway-1", UpstreamRequestID: "vendor-2", UpstreamProxyName: "direct/no_proxy"},
	} {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	db.flushLogs()
	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[1].RequestID != "gateway-1" || logs[1].UpstreamRequestID != "vendor-1" || logs[1].UpstreamProxyID != 12 || logs[1].UpstreamProxyName != "old proxy label" {
		t.Fatalf("trace fields missing: %+v", logs)
	}
	for _, filter := range []UsageLogFilter{
		{RequestID: "gateway-1"}, {UpstreamRequestID: "vendor-2"}, {Query: "vendor-2"},
	} {
		filter.Start = time.Now().Add(-time.Hour)
		filter.End = time.Now().Add(time.Hour)
		where, args := db.buildUsageLogWhere(filter)
		var n int
		if err := db.conn.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs u WHERE "+where, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := 1
		if filter.RequestID != "" {
			want = 2
		}
		if n != want {
			t.Fatalf("filter %+v matched %d, want %d", filter, n, want)
		}
	}
}
