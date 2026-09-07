package database

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualWindowLockLifecycleAndScope(test *testing.T) {
	path := filepath.Join(test.TempDir(), "window-lock.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	root := strings.Repeat("a", 24)
	first, err := db.LockPromptUserWindow(test.Context(), "NEWAPI", "42", root, now, time.Hour)
	if err != nil || !first.ExpiresAt.Equal(now.Add(time.Hour)) {
		test.Fatalf("lock=%+v err=%v", first, err)
	}
	repeated, err := db.LockPromptUserWindow(test.Context(), "newapi", "42", root, now.Add(time.Minute), time.Hour)
	if err != nil || !repeated.ExpiresAt.Equal(first.ExpiresAt) {
		test.Fatalf("repeat extended lock: %+v err=%v", repeated, err)
	}
	for _, scope := range [][3]string{{"other", "42", root}, {"newapi", "43", root}, {"newapi", "42", strings.Repeat("b", 24)}} {
		if _, err := db.GetActivePromptUserWindowLock(test.Context(), scope[0], scope[1], scope[2], now); !errors.Is(err, sql.ErrNoRows) {
			test.Fatalf("scope=%v err=%v", scope, err)
		}
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	locks, err := db.ListPromptUserWindowLocks(test.Context(), "newapi", "42", now)
	if err != nil || len(locks) != 1 {
		test.Fatalf("restored=%+v err=%v", locks, err)
	}
	if _, err := db.GetActivePromptUserWindowLock(test.Context(), "newapi", "42", root, first.ExpiresAt); !errors.Is(err, sql.ErrNoRows) {
		test.Fatalf("expired lock returned: %v", err)
	}
	if err := db.UnlockPromptUserWindow(test.Context(), "newapi", "42", root, now.Add(time.Minute)); err != nil {
		test.Fatal(err)
	}
	if _, err := db.GetActivePromptUserWindowLock(test.Context(), "newapi", "42", root, now.Add(time.Minute)); !errors.Is(err, sql.ErrNoRows) {
		test.Fatalf("unlocked lock returned: %v", err)
	}
	if _, err := db.LockPromptUserWindow(test.Context(), "newapi", "42", root, now.Add(2*time.Minute), time.Hour); err != nil {
		test.Fatal(err)
	}
	var automaticLocks int
	if err := db.conn.QueryRowContext(test.Context(), `SELECT COUNT(*) FROM prompt_conversation_locks`).Scan(&automaticLocks); err != nil || automaticLocks != 0 {
		test.Fatalf("automatic locks=%d err=%v", automaticLocks, err)
	}
}

func TestWindow500TelemetryKeepsLatestAttemptAndExactGeneration(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "window-errors.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Truncate(time.Second).Add(345678901 * time.Nanosecond)
	root := strings.Repeat("a", 24)
	expires := now.Add(time.Hour)
	base := UsageLogInput{AccountID: 9, SessionHash: root, NewAPIPlatform: "newapi", NewAPIUserID: "42", StatusCode: 500, ObservedAt: now, SessionWindowExpiresAt: expires}
	write := func(input UsageLogInput) {
		test.Helper()
		if err := db.InsertUsageLog(test.Context(), &input); err != nil {
			test.Fatal(err)
		}
	}
	write(base)
	for _, status := range []int{200, 400, 502, 503} {
		input := base
		input.StatusCode, input.ObservedAt = status, now.Add(time.Minute)
		write(input)
	}
	for _, change := range []func(*UsageLogInput){
		func(input *UsageLogInput) { input.NewAPIUserID = "43" },
		func(input *UsageLogInput) { input.NewAPIPlatform = "other" },
		func(input *UsageLogInput) { input.AccountID = 10 },
		func(input *UsageLogInput) { input.SessionHash = strings.Repeat("b", 24) },
		func(input *UsageLogInput) { input.SessionWindowExpiresAt = expires.Add(time.Hour) },
		func(input *UsageLogInput) { input.SessionWindowExpiresAt = now.Add(-time.Second) },
		func(input *UsageLogInput) { input.SessionWindowExpiresAt = time.Time{} },
	} {
		input := base
		change(&input)
		write(input)
	}
	db.FlushUsageLogs()
	var expiredCount int
	if err := db.conn.QueryRowContext(test.Context(), `SELECT COUNT(*) FROM prompt_session_account_errors WHERE window_expires_at<=$1`, now.UnixNano()).Scan(&expiredCount); err != nil || expiredCount != 0 {
		test.Fatalf("expired markers were retained: count=%d err=%v", expiredCount, err)
	}
	older := base
	older.ObservedAt = now.Add(-time.Second)
	write(older)
	db.FlushUsageLogs()
	items, err := db.ListPromptSessionAccountErrors(test.Context(), "newapi", "42", now)
	if err != nil || len(items) != 4 {
		test.Fatalf("items=%+v err=%v", items, err)
	}
	for _, item := range items {
		if !item.Last500At.Equal(now) {
			test.Fatalf("latest 500 changed after success/older attempt: %+v", item)
		}
	}
	items, err = db.ListPromptSessionAccountErrors(test.Context(), "newapi", "42", expires)
	if err != nil || len(items) != 1 || items[0].WindowExpiresAt != expires.Add(time.Hour).UnixNano() {
		test.Fatalf("expired generation leaked: %+v err=%v", items, err)
	}
}
