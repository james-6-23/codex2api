package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newManualFlushTestDB 返回停掉后台 flusher 的库,刷盘时机完全由用例控制。PostgreSQL 需要
// CODEX2API_TEST_POSTGRES_DSN 指向可写的测试库,用例前后清空 usage_logs。
func newManualFlushTestDB(t *testing.T, driver string) *DB {
	t.Helper()
	ctx := context.Background()
	var (
		db  *DB
		err error
	)
	if driver == "postgres" {
		dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
		}
		db, err = New("postgres", dsn)
	} else {
		db, err = New("sqlite", filepath.Join(t.TempDir(), "manual-flush.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	close(db.logStop)
	db.logWg.Wait()
	if driver == "postgres" {
		if err := db.ClearUsageLogs(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if driver == "postgres" {
			_ = db.ClearUsageLogs(ctx)
		}
		_ = db.conn.Close()
	})
	return db
}

// sameStoredTime 按驱动的存储精度比较时间:PostgreSQL 到微秒,SQLite 沿用 CURRENT_TIMESTAMP 的秒级文本。
func sameStoredTime(db *DB, got, want time.Time) bool {
	if db.isSQLite() {
		return got.UTC().Equal(want.UTC().Truncate(time.Second))
	}
	return got.Sub(want).Abs() < time.Microsecond
}

func TestUsageLogCreatedAtIsCompletionTimeSQLite(t *testing.T) {
	runUsageLogCreatedAtIsCompletionTime(t, "sqlite")
}

func TestUsageLogCreatedAtIsCompletionTimePostgres(t *testing.T) {
	runUsageLogCreatedAtIsCompletionTime(t, "postgres")
}

// issue #857:日志在缓冲里等一个刷盘间隔才落库,created_at 不能取刷盘时刻。
func runUsageLogCreatedAtIsCompletionTime(t *testing.T, driver string) {
	db := newManualFlushTestDB(t, driver)
	ctx := context.Background()

	// 整点前完成、过了整点才刷盘的请求必须记在上一个小时(含小时汇总)。
	hour := time.Now().UTC().Truncate(time.Hour)
	completed := hour.Add(-2*time.Second - 250*time.Millisecond)
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		Endpoint: "/created-at/explicit", Model: "gpt-5.5", StatusCode: 200, TotalTokens: 10, CreatedAt: completed,
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		Endpoint: "/created-at/implicit", Model: "gpt-5.5", StatusCode: 200, TotalTokens: 10,
	}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	// 没传 CreatedAt 时在进缓冲那一刻定时间,而不是等刷盘。
	db.logMu.Lock()
	buffered := db.logBuf[len(db.logBuf)-1].CreatedAt
	db.logMu.Unlock()
	if buffered.Before(before) || buffered.After(after) {
		t.Fatalf("buffered CreatedAt = %v, want within [%v, %v]", buffered, before, after)
	}

	db.FlushUsageLogs()

	readCreatedAt := func(endpoint string) time.Time {
		t.Helper()
		var raw interface{}
		if err := db.conn.QueryRowContext(ctx, `SELECT created_at FROM usage_logs WHERE endpoint = $1`, endpoint).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		value, err := parseDBTimeValue(raw)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := readCreatedAt("/created-at/explicit"); !sameStoredTime(db, got, completed) {
		t.Fatalf("explicit created_at = %v, want %v", got.UTC(), completed)
	}
	if got := readCreatedAt("/created-at/implicit"); !sameStoredTime(db, got, buffered) {
		t.Fatalf("implicit created_at = %v, want %v (enqueue time)", got.UTC(), buffered.UTC())
	}

	var rawBucket interface{}
	if err := db.conn.QueryRowContext(ctx, `SELECT bucket FROM usage_log_hourly WHERE endpoint = $1`, "/created-at/explicit").Scan(&rawBucket); err != nil {
		t.Fatal(err)
	}
	bucket, err := parseDBTimeValue(rawBucket)
	if err != nil {
		t.Fatal(err)
	}
	if want := hour.Add(-time.Hour); !bucket.UTC().Equal(want) {
		t.Fatalf("hourly bucket = %v, want %v (completion hour, not flush hour)", bucket.UTC(), want)
	}
}

func TestTrafficSnapshotUsesCompletionSecondsSQLite(t *testing.T) {
	runTrafficSnapshotUsesCompletionSeconds(t, "sqlite")
}

func TestTrafficSnapshotUsesCompletionSecondsPostgres(t *testing.T) {
	runTrafficSnapshotUsesCompletionSeconds(t, "postgres")
}

// 一次刷盘落库的请求不能被压进同一秒(否则峰值虚高);还在缓冲里的请求也要算进近 10 秒,
// 且刷盘前后不重复计数。
func runTrafficSnapshotUsesCompletionSeconds(t *testing.T, driver string) {
	db := newManualFlushTestDB(t, driver)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	add := func(ago time.Duration, tokens int, internalReason string) {
		t.Helper()
		if err := db.InsertUsageLog(ctx, &UsageLogInput{
			Endpoint: "/v1/responses", Model: "gpt-5.5", StatusCode: 200, TotalTokens: tokens,
			InternalReason: internalReason, CreatedAt: base.Add(-ago),
		}); err != nil {
			t.Fatal(err)
		}
	}

	add(5*time.Second, 100, "")
	add(4*time.Second, 100, "")
	add(3*time.Second, 100, "")
	add(4*time.Second, 999, "connection_test") // 内部请求不计
	add(6*time.Minute, 999, "")                // 超出 5 分钟回看窗口
	db.FlushUsageLogs()
	add(2*time.Second, 100, "")
	add(1*time.Second, 300, "")
	add(4*time.Second, 999, "connection_test")
	add(7*time.Minute, 999, "")

	check := func(stage string) {
		t.Helper()
		snapshot, err := db.GetTrafficSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := TrafficSnapshot{QPS: 0.5, QPSPeak: 1, TPS: 70, TPSPeak: 300}
		if *snapshot != want {
			t.Fatalf("%s: snapshot = %+v, want %+v", stage, *snapshot, want)
		}
	}
	check("two rows still buffered")
	db.FlushUsageLogs()
	check("all rows flushed")
}
