package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteProxyTimezoneMigration(test *testing.T) {
	path := filepath.Join(test.TempDir(), "timezone.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	proxyID, err := db.InsertProxy(ctx, "http://existing:8080", "existing")
	if err != nil {
		test.Fatal(err)
	}
	for _, column := range []string{"test_timezone", "timezone_override"} {
		if _, err := db.conn.ExecContext(ctx, "ALTER TABLE proxies DROP COLUMN "+column); err != nil {
			test.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	row, err := db.GetProxy(ctx, proxyID)
	if err != nil || row.Label != "existing" || row.TestTimezone != "" || row.TimezoneOverride != "" {
		test.Fatalf("migration lost proxy/defaults: %+v, %v", row, err)
	}
}

func TestProxyTimezonePersistence(test *testing.T) {
	runProxyTimezonePersistence(test, newProxyTestDB(test))
}

func TestProxyTimezonePersistencePostgres(test *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		test.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	connection, err := sql.Open("pgx", dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = connection.Close() })
	connection.SetMaxOpenConns(1)
	if _, err := connection.ExecContext(context.Background(), `CREATE TEMP TABLE proxies (
		id SERIAL PRIMARY KEY, url VARCHAR(500) NOT NULL UNIQUE,
		label VARCHAR(255) DEFAULT '', enabled BOOLEAN DEFAULT TRUE, created_at TIMESTAMPTZ DEFAULT NOW(),
		test_ip VARCHAR(100) DEFAULT '', test_location VARCHAR(255) DEFAULT '', test_latency_ms INT DEFAULT 0,
		test_status VARCHAR(20) NOT NULL DEFAULT 'untested', test_timezone VARCHAR(100) DEFAULT '',
		timezone_override VARCHAR(100) DEFAULT ''
	)`); err != nil {
		test.Fatal(err)
	}
	runProxyTimezonePersistence(test, &DB{conn: connection, driver: "postgres"})
}

func runProxyTimezonePersistence(test *testing.T, db *DB) {
	test.Helper()
	ctx := context.Background()
	proxyURL := "http://timezone.example:8080"
	proxyID, err := db.InsertProxy(ctx, proxyURL, "")
	if err != nil {
		test.Fatal(err)
	}
	manual := "Asia/Tokyo"
	if err := db.UpdateProxy(ctx, proxyID, nil, nil, nil, &manual); err != nil {
		test.Fatal(err)
	}
	for _, sample := range []struct {
		status, ip, inferred, expected string
	}{
		{ProxyTestStatusSuccess, "1.2.3.4", "America/Los_Angeles", "America/Los_Angeles"},
		{ProxyTestStatusSuccess, "1.2.3.4", "", "America/Los_Angeles"},
		{ProxyTestStatusError, "", "", "America/Los_Angeles"},
		{ProxyTestStatusSuccess, "5.6.7.8", "invalid/location", ""},
		{ProxyTestStatusSuccess, "5.6.7.8", "Europe/London", "Europe/London"},
	} {
		if err := db.UpdateProxyTestResult(ctx, proxyID, proxyURL, sample.status, sample.ip, "", 1, sample.inferred); err != nil {
			test.Fatal(err)
		}
		row, err := db.GetProxy(ctx, proxyID)
		if err != nil || row.TestTimezone != sample.expected || row.TimezoneOverride != manual {
			test.Fatalf("probe %+v: row=%+v err=%v", sample, row, err)
		}
	}
	for _, list := range []func(context.Context) ([]*ProxyRow, error){db.ListProxies, db.ListEnabledProxies, func(ctx context.Context) ([]*ProxyRow, error) { return db.ListProxiesByIDs(ctx, []int64{proxyID}) }} {
		rows, err := list(ctx)
		if err != nil || len(rows) != 1 || rows[0].TestTimezone != "Europe/London" || rows[0].TimezoneOverride != manual {
			test.Fatalf("list timezone rows=%+v err=%v", rows, err)
		}
	}
	for _, invalid := range []string{"Local", "../Asia/Tokyo", "Invalid/Zone"} {
		if err := db.UpdateProxy(ctx, proxyID, nil, nil, nil, &invalid); err == nil {
			test.Fatalf("accepted %q", invalid)
		}
	}
	newURL := "http://new-timezone.example:8080"
	if err := db.UpdateProxy(ctx, proxyID, &newURL, nil, nil); err != nil {
		test.Fatal(err)
	}
	if err := db.UpdateProxyTestResult(ctx, proxyID, proxyURL, ProxyTestStatusSuccess, "1.2.3.4", "", 1, "UTC"); !errors.Is(err, ErrProxyTestTargetChanged) {
		test.Fatalf("stale probe overwrote new proxy: %v", err)
	}
	row, _ := db.GetProxy(ctx, proxyID)
	if row.TestTimezone != "" || row.TimezoneOverride != manual {
		test.Fatalf("URL reset: %+v", row)
	}
	manual = ""
	if err := db.UpdateProxy(ctx, proxyID, nil, nil, nil, &manual); err != nil {
		test.Fatal(err)
	}
	row, _ = db.GetProxy(ctx, proxyID)
	if row.TimezoneOverride != "" {
		test.Fatal("manual override was not cleared")
	}
}
